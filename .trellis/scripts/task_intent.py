#!/usr/bin/env python3
"""Trellis 自动意图路由的 task 创建与安全丢弃 helper。"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

from common.active_task import (
    normalize_task_ref,
    resolve_active_task,
    resolve_context_key,
)
from common.io import read_json, write_json
from common.paths import get_repo_root, get_tasks_dir
from common.task_utils import is_safe_task_path, resolve_task_dir
from git_evidence import (
    GitEvidenceError,
    capture_workspace_evidence,
    task_compat_baseline,
)
from untracked_flow import STATE_KEY, read_untracked_state


class IntentTaskError(Exception):
    """携带稳定 reason code 的 task intent 操作错误。"""

    def __init__(self, reason: str, message: str) -> None:
        """初始化结构化错误。

        Args:
            reason: 稳定机器错误码。
            message: 中文诊断说明。
        """
        super().__init__(message)
        self.reason = reason


def _utc_now() -> str:
    """返回无微秒 UTC ISO-8601 时间。"""
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _emit(payload: dict) -> None:
    """向 stdout 输出稳定 JSON。"""
    print(json.dumps(payload, ensure_ascii=False, sort_keys=True))


def _run_git(repo_root: Path, args: list[str], *, binary: bool = False) -> subprocess.CompletedProcess:
    """执行只面向当前仓库的 Git 命令。"""
    return subprocess.run(
        ["git", *args],
        cwd=repo_root,
        capture_output=True,
        text=not binary,
        check=False,
    )


def capture_git_baseline(repo_root: Path) -> dict:
    """捕获 task 创建前的 Git HEAD 与 dirty 状态。

    Args:
        repo_root: Trellis 项目根目录。

    Returns:
        包含 head 与结构化 porcelain entries 的基线。
    """
    try:
        evidence = capture_workspace_evidence(repo_root, block_unsafe=False)
    except GitEvidenceError as error:
        raise IntentTaskError(error.reason, str(error)) from error
    return task_compat_baseline(evidence)


def _write_json_atomic(path: Path, data: dict) -> None:
    """使用同目录临时文件原子替换 JSON。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=path.parent,
    )
    temp_path = Path(temp_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        try:
            temp_path.unlink()
        except FileNotFoundError:
            pass
        raise


def _task_script() -> Path:
    """返回与当前 helper 同目录的 task.py。"""
    return Path(__file__).resolve().with_name("task.py")


def _find_created_task(stdout: str) -> str:
    """从 task.py create stdout 提取仓库相对 task 路径。"""
    for line in reversed(stdout.splitlines()):
        value = line.strip()
        if value.startswith(".trellis/tasks/"):
            return value
    raise IntentTaskError("create-output-invalid", "task.py create 未返回可识别的 task 路径")


def _create_planning_task(
    repo_root: Path,
    args: argparse.Namespace,
) -> tuple[str, Path, dict]:
    """调用 task.py 创建 planning task 并读取元数据。"""
    command = [
        sys.executable,
        str(_task_script()),
        "create",
        args.title,
        "--slug",
        args.slug,
    ]
    if args.parent:
        command.extend(["--parent", args.parent])
    if args.package:
        command.extend(["--package", args.package])
    if args.priority:
        command.extend(["--priority", args.priority])
    if args.description:
        command.extend(["--description", args.description])

    result = subprocess.run(
        command,
        cwd=repo_root,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.stderr:
        sys.stderr.write(result.stderr)
    if result.returncode != 0:
        raise IntentTaskError("task-create-failed", "task.py create 执行失败")

    task_ref = _find_created_task(result.stdout)
    task_dir = resolve_task_dir(task_ref, repo_root)
    data = read_json(task_dir / "task.json")
    if not data:
        _rollback_created_task(
            task_dir,
            {"parent": args.parent} if args.parent else {},
            task_ref,
            repo_root,
        )
        raise IntentTaskError("task-json-invalid", "新 task 的 task.json 无法读取")
    return task_ref, task_dir, data


def create_auto_task(args: argparse.Namespace) -> dict:
    """创建并标记由当前请求自动路由产生的 planning task。

    Args:
        args: create 子命令参数。

    Returns:
        创建结果、task 路径与基线摘要。
    """
    repo_root = get_repo_root()
    baseline = capture_git_baseline(repo_root)
    task_ref, task_dir, data = _create_planning_task(repo_root, args)
    task_json = task_dir / "task.json"
    meta = data.get("meta") if isinstance(data.get("meta"), dict) else {}
    context_key = resolve_context_key()
    meta["intentRouting"] = {
        "autoCreated": True,
        "createdAt": _utc_now(),
        "contextKey": context_key,
        "implementationStarted": False,
        "baseline": baseline,
    }
    data["meta"] = meta
    if not write_json(task_json, data):
        _rollback_created_task(task_dir, data, task_ref, repo_root)
        raise IntentTaskError("task-json-write-failed", "无法写入自动路由 task 元数据")
    active = resolve_active_task(repo_root)
    auto_discard_eligible = bool(
        context_key
        and active.context_key == context_key
        and active.task_path
        and normalize_task_ref(active.task_path) == normalize_task_ref(task_ref)
    )
    return {
        "status": "created",
        "task": task_ref,
        "autoDiscardEligible": auto_discard_eligible,
        "baselineEntries": len(baseline["status"]),
        "baselineRepositories": len(baseline.get("repositories", [])),
    }


def _session_snapshot(repo_root: Path, context_key: str) -> tuple[Path, dict]:
    """读取 adoption 前的当前 session 原文对象。"""
    path = repo_root / ".trellis/.runtime/sessions" / f"{context_key}.json"
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise IntentTaskError("session-runtime-missing", "当前 untracked session runtime 不存在") from exc
    except json.JSONDecodeError as exc:
        raise IntentTaskError("session-runtime-corrupt", "当前 session runtime 已损坏") from exc
    except OSError as exc:
        raise IntentTaskError("session-runtime-io_error", "当前 session runtime 无法读取") from exc
    if not isinstance(data, dict):
        raise IntentTaskError("session-runtime-corrupt", "当前 session runtime 根节点不是对象")
    return path, data


def _rollback_adoption(
    task_dir: Path,
    task_data: dict,
    task_ref: str,
    repo_root: Path,
    session_path: Path,
    session_before: dict,
) -> None:
    """补偿删除 adoption 新 task 并恢复原 session。"""
    try:
        _rollback_created_task(task_dir, task_data, task_ref, repo_root)
        _write_json_atomic(session_path, session_before)
    except (IntentTaskError, OSError) as exc:
        raise IntentTaskError(
            "adoption-rollback-failed",
            f"纳管失败且无法完整恢复原 untracked 状态:{exc}",
        ) from exc


def adopt_untracked_work(args: argparse.Namespace) -> dict:
    """把当前 untracked diff 和流程游标原地接管到 planning task。

    Args:
        args: adopt 子命令参数。

    Returns:
        新 planning task、来源 work id 和接管阶段。
    """
    repo_root = get_repo_root()
    context_key = resolve_context_key()
    if not context_key:
        raise IntentTaskError("no-session-context", "无法解析当前 AI session")
    try:
        untracked = read_untracked_state(repo_root)
    except Exception as exc:
        reason = getattr(exc, "reason", "untracked-state-invalid")
        raise IntentTaskError(reason, str(exc)) from exc
    if untracked.get("status") != "hit":
        raise IntentTaskError("no-active-untracked-work", "当前 session 没有可纳管的无任务事项")
    state = untracked["state"]
    session_path, session_before = _session_snapshot(repo_root, context_key)
    raw_state = session_before.get(STATE_KEY)
    if not isinstance(raw_state, dict) or raw_state.get("id") != state["id"]:
        raise IntentTaskError("untracked-state-mismatch", "纳管前 untracked 状态已变化")
    baseline = capture_git_baseline(repo_root)

    task_ref = ""
    task_dir: Path | None = None
    task_data: dict = {}
    try:
        task_ref, task_dir, task_data = _create_planning_task(repo_root, args)
        task_json = task_dir / "task.json"
        meta = task_data.get("meta") if isinstance(task_data.get("meta"), dict) else {}
        meta["intentRouting"] = {
            "autoCreated": False,
            "adoptedUntracked": True,
            "createdAt": _utc_now(),
            "contextKey": context_key,
            "untrackedWorkId": state["id"],
            "implementationStarted": True,
            "baseline": baseline,
            "adoptedStage": state["stage"],
        }
        task_data["meta"] = meta
        _write_json_atomic(task_json, task_data)

        active = resolve_active_task(repo_root)
        if (
            active.context_key != context_key
            or normalize_task_ref(active.task_path or "") != normalize_task_ref(task_ref)
        ):
            raise IntentTaskError("task-session-bind-failed", "新 task 未绑定到当前 session")

        current_session_path, current_session = _session_snapshot(repo_root, context_key)
        current_state = current_session.get(STATE_KEY)
        if not isinstance(current_state, dict) or current_state.get("id") != state["id"]:
            raise IntentTaskError("untracked-state-mismatch", "清理前 untracked 状态已变化")
        current_session.pop(STATE_KEY, None)
        _write_json_atomic(current_session_path, current_session)
    except (IntentTaskError, OSError) as exc:
        if task_dir is not None and task_ref:
            _rollback_adoption(
                task_dir,
                task_data,
                task_ref,
                repo_root,
                session_path,
                session_before,
            )
        if isinstance(exc, IntentTaskError):
            raise
        raise IntentTaskError("adoption-write-failed", f"无法完成 untracked 纳管:{exc}") from exc

    return {
        "status": "adopted",
        "task": task_ref,
        "workId": state["id"],
        "adoptedStage": state["stage"],
        "implementationStarted": True,
    }


def _resolve_safe_task(task_ref: str, repo_root: Path) -> tuple[Path, str]:
    """解析并验证活动 tasks 根目录下的直接 task 路径。"""
    if not is_safe_task_path(task_ref, repo_root):
        raise IntentTaskError("unsafe-task-path", "task 路径未通过安全校验")
    raw_task_dir = resolve_task_dir(task_ref, repo_root)
    if raw_task_dir.is_symlink():
        raise IntentTaskError("unsafe-task-path", "不允许丢弃软链 task 目录")
    tasks_dir = get_tasks_dir(repo_root).resolve()
    try:
        task_dir = raw_task_dir.resolve(strict=True)
    except (FileNotFoundError, OSError):
        raise IntentTaskError("task-not-found", "目标 task 不存在")
    if task_dir.parent != tasks_dir:
        raise IntentTaskError("unsafe-task-path", "只允许丢弃活动 tasks 根目录下的直接子目录")
    return task_dir, task_dir.relative_to(repo_root).as_posix()


def _has_git_history(repo_root: Path, task_ref: str) -> bool:
    """判断 task 路径是否已进入 index 或任一可达提交。"""
    tracked = _run_git(repo_root, ["ls-files", "--cached", "--", task_ref])
    if tracked.returncode != 0:
        raise IntentTaskError("git-check-failed", "无法检查 task 是否已被 Git 跟踪")
    if tracked.stdout.strip():
        return True
    history = _run_git(repo_root, ["log", "--all", "--format=%H", "--", task_ref])
    if history.returncode != 0:
        raise IntentTaskError("git-check-failed", "无法检查 task Git 历史")
    return bool(history.stdout.strip())


def _prepare_parent_update(task_dir: Path, task_data: dict, repo_root: Path) -> tuple[Path, dict, dict] | None:
    """校验父引用并计算删除后的 parent task.json。"""
    parent_name = task_data.get("parent")
    if not parent_name:
        return None
    parent_dir = resolve_task_dir(str(parent_name), repo_root)
    if parent_dir.is_symlink() or parent_dir.resolve().parent != get_tasks_dir(repo_root).resolve():
        raise IntentTaskError("parent-link-invalid", "parent task 路径不安全")
    parent_json = parent_dir / "task.json"
    original = read_json(parent_json)
    if not original:
        raise IntentTaskError("parent-link-invalid", "parent task.json 无法读取")
    child_name = task_dir.name
    referenced = False
    next_data = dict(original)
    for key in ("children", "subtasks"):
        values = original.get(key)
        if not isinstance(values, list):
            continue
        if child_name in values:
            referenced = True
        next_data[key] = [value for value in values if value != child_name]
    if not referenced:
        raise IntentTaskError("parent-link-invalid", "parent 未包含当前 child 引用")
    return parent_json, original, next_data


def _matching_sessions(canonical: str, repo_root: Path) -> list[tuple[Path, dict]]:
    """收集所有仍指向目标 task 的 session 文件及其原文数据。"""
    sessions_dir = repo_root / ".trellis/.runtime/sessions"
    if not sessions_dir.is_dir():
        return []
    matches: list[tuple[Path, dict]] = []
    for session_path in sorted(sessions_dir.glob("*.json")):
        data = read_json(session_path)
        current = data.get("current_task") if isinstance(data, dict) else None
        if isinstance(current, str) and normalize_task_ref(current) == canonical:
            matches.append((session_path, data))
    return matches


def _restore_json_files(entries: list[tuple[Path, dict]]) -> bool:
    """恢复 JSON 文件快照，并返回是否全部成功。"""
    restored = True
    for path, data in entries:
        restored = write_json(path, data) and restored
    return restored


def _remove_session_files(entries: list[tuple[Path, dict]]) -> int:
    """删除匹配 session；中途失败时恢复此前已删文件。"""
    removed: list[tuple[Path, dict]] = []
    try:
        for session_path, data in entries:
            if session_path.exists():
                session_path.unlink()
                removed.append((session_path, data))
    except OSError as exc:
        if not _restore_json_files(removed):
            raise IntentTaskError(
                "session-rollback-failed",
                f"清理 session 失败且无法完整回滚:{exc}",
            ) from exc
        raise IntentTaskError("session-clear-failed", f"无法清理 task session:{exc}") from exc
    return len(removed)


def _restore_parent(parent_update: tuple[Path, dict, dict] | None) -> bool:
    """恢复 parent task.json；没有 parent 时视为成功。"""
    if not parent_update:
        return True
    parent_json, original_parent, _ = parent_update
    return write_json(parent_json, original_parent)


def _delete_task_transaction(
    task_dir: Path,
    task_data: dict,
    canonical: str,
    repo_root: Path,
) -> tuple[int, bool]:
    """按 parent、session、task 顺序删除，并在失败时补偿恢复。

    Args:
        task_dir: 待删除的 task 目录。
        task_data: 当前 task.json 数据。
        canonical: 仓库相对规范 task 路径。
        repo_root: Trellis 项目根目录。

    Returns:
        清理的 session 数和 parent 是否更新。

    Raises:
        IntentTaskError: 任一步失败或补偿恢复不完整。
    """
    parent_update = _prepare_parent_update(task_dir, task_data, repo_root)
    sessions = _matching_sessions(canonical, repo_root)
    if parent_update:
        parent_json, _, next_parent = parent_update
        if not write_json(parent_json, next_parent):
            raise IntentTaskError("parent-write-failed", "无法更新 parent task 引用")

    try:
        cleared = _remove_session_files(sessions)
    except IntentTaskError:
        if not _restore_parent(parent_update):
            raise IntentTaskError("parent-rollback-failed", "session 清理失败且 parent 回滚失败")
        raise

    try:
        shutil.rmtree(task_dir)
    except OSError as exc:
        sessions_restored = _restore_json_files(sessions)
        parent_restored = _restore_parent(parent_update)
        if not sessions_restored or not parent_restored:
            raise IntentTaskError(
                "task-delete-rollback-failed",
                f"删除 task 失败且补偿恢复不完整:{exc}",
            ) from exc
        raise IntentTaskError("task-delete-failed", f"删除 task 目录失败:{exc}") from exc
    return cleared, parent_update is not None


def _rollback_created_task(
    task_dir: Path,
    task_data: dict,
    canonical: str,
    repo_root: Path,
) -> None:
    """回滚已创建但未能写入 intent 元数据的 task。"""
    try:
        _delete_task_transaction(task_dir, task_data, canonical, repo_root)
    except IntentTaskError as exc:
        raise IntentTaskError(
            "task-create-rollback-failed",
            f"自动 task 初始化失败且无法完整回滚:{exc}",
        ) from exc


def discard_auto_task(args: argparse.Namespace) -> dict:
    """在全部安全条件成立时丢弃当前请求自动创建的 planning task。

    Args:
        args: discard 子命令参数。

    Returns:
        删除路径、session 清理数和 parent 更新结果。
    """
    repo_root = get_repo_root()
    task_dir, canonical = _resolve_safe_task(args.task, repo_root)
    task_json = task_dir / "task.json"
    data = read_json(task_json)
    if not data:
        raise IntentTaskError("task-json-invalid", "目标 task.json 无法读取")
    meta = data.get("meta") if isinstance(data.get("meta"), dict) else {}
    intent = meta.get("intentRouting") if isinstance(meta.get("intentRouting"), dict) else {}
    if intent.get("autoCreated") is not True:
        raise IntentTaskError("not-auto-created", "只允许自动丢弃由 intent helper 创建的 task")

    context_key = resolve_context_key()
    active = resolve_active_task(repo_root)
    if (
        not context_key
        or intent.get("contextKey") != context_key
        or normalize_task_ref(active.task_path or "") != canonical
    ):
        raise IntentTaskError("request-scope-mismatch", "task 不属于当前请求的活动 session")
    if data.get("status") != "planning" or intent.get("implementationStarted") is True:
        raise IntentTaskError("implementation-started", "task 已进入或曾标记进入实施阶段")
    if data.get("children") or data.get("subtasks"):
        raise IntentTaskError("has-children", "task 存在子任务")
    for field, reason in (
        ("commit", "has-commit"),
        ("pr_url", "has-pr"),
        ("worktree_path", "has-worktree"),
        ("progress", "has-progress"),
        ("last_push_snapshot", "has-progress"),
    ):
        if data.get(field):
            raise IntentTaskError(reason, f"task 字段 {field} 表明已有实施或进度记录")
    if _has_git_history(repo_root, canonical):
        raise IntentTaskError("task-already-versioned", "task 已进入 Git index 或历史提交")

    cleared, parent_updated = _delete_task_transaction(
        task_dir,
        data,
        canonical,
        repo_root,
    )
    return {
        "status": "discarded",
        "task": canonical,
        "sessionsCleared": cleared,
        "parentUpdated": parent_updated,
    }


def build_parser() -> argparse.ArgumentParser:
    """构造 task intent helper CLI parser。

    Returns:
        已配置 create/discard/adopt 子命令的 parser。
    """
    parser = argparse.ArgumentParser(description="Trellis task intent helper")
    subparsers = parser.add_subparsers(dest="command", required=True)

    create_parser = subparsers.add_parser("create", help="创建自动路由 planning task")
    create_parser.add_argument("--title", required=True)
    create_parser.add_argument("--slug", required=True)
    create_parser.add_argument("--parent")
    create_parser.add_argument("--package")
    create_parser.add_argument("--priority", default="P2")
    create_parser.add_argument("--description")

    discard_parser = subparsers.add_parser("discard", help="安全丢弃自动路由 planning task")
    discard_parser.add_argument("--task", required=True)
    adopt_parser = subparsers.add_parser("adopt", help="把当前 untracked 事项纳管为 planning task")
    adopt_parser.add_argument("title")
    adopt_parser.add_argument("--slug", required=True)
    adopt_parser.add_argument("--parent")
    adopt_parser.add_argument("--package")
    adopt_parser.add_argument("--priority", default="P2")
    adopt_parser.add_argument("--description")
    return parser


def main() -> int:
    """执行 task intent helper CLI。

    Returns:
        成功为 0，拒绝或失败为 1。
    """
    args = build_parser().parse_args()
    try:
        if args.command == "create":
            payload = create_auto_task(args)
        elif args.command == "adopt":
            payload = adopt_untracked_work(args)
        else:
            payload = discard_auto_task(args)
        _emit(payload)
        return 0
    except IntentTaskError as exc:
        _emit({"status": "error", "reason": exc.reason, "message": str(exc)})
        return 1


if __name__ == "__main__":
    sys.exit(main())
