#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""读取和写入 Trellis 任务进度。"""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


DIR_WORKFLOW = ".trellis"
DIR_TASKS = "tasks"
FILE_TASK_JSON = "task.json"
REQUIRED_FIELDS = {
    "updatedAt",
    "completedSteps",
    "partialStep",
    "nextStep",
    "notes",
}


def _find_repo_root(start: Path) -> Path | None:
    """从当前位置向上查找 Trellis 项目根目录。"""
    current = start.resolve()
    if current.is_file():
        current = current.parent
    while True:
        if (current / DIR_WORKFLOW).is_dir():
            return current
        if current == current.parent:
            return None
        current = current.parent


def _load_common_modules(repo_root: Path) -> None:
    """把 Trellis scripts 目录加入 import path。"""
    scripts_dir = repo_root / DIR_WORKFLOW / "scripts"
    if str(scripts_dir) not in sys.path:
        sys.path.insert(0, str(scripts_dir))


def _read_json(path: Path) -> dict[str, Any] | None:
    """读取 JSON 对象，失败或非对象时返回 None。"""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError, OSError):
        return None
    return data if isinstance(data, dict) else None


def _write_json(path: Path, data: dict[str, Any]) -> None:
    """按 Trellis 任务格式原子写回 JSON，失败时保留旧文件。"""
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


def _rel_path(repo_root: Path, path: Path) -> str:
    """尽量返回项目根目录相对路径。"""
    try:
        return path.relative_to(repo_root).as_posix()
    except ValueError:
        return str(path)


def _resolve_task_dir(repo_root: Path, task_ref: str) -> Path:
    """解析任务目录引用。"""
    _load_common_modules(repo_root)
    from common.task_utils import resolve_task_dir  # type: ignore[import-not-found]

    return resolve_task_dir(task_ref, repo_root)


def _current_task_dir(repo_root: Path) -> Path | None:
    """读取当前 session 的活动任务目录。"""
    _load_common_modules(repo_root)
    from common.active_task import resolve_active_task  # type: ignore[import-not-found]

    active = resolve_active_task(repo_root)
    if not active.task_path:
        return None
    task_dir = Path(active.task_path)
    if task_dir.is_absolute():
        return task_dir
    return repo_root / task_dir


def _task_json_path(task_dir: Path) -> Path:
    """返回任务 task.json 路径。"""
    return task_dir / FILE_TASK_JSON


def _progress_summary(progress: dict[str, Any]) -> dict[str, Any]:
    """提取恢复提示需要的最小进度字段。"""
    return {
        "completedSteps": progress.get("completedSteps", []),
        "partialStep": progress.get("partialStep"),
        "nextStep": progress.get("nextStep"),
        "notes": progress.get("notes", ""),
    }


def _utc_date() -> str:
    """返回 UTC 日期，保持 task.json 既有 completedAt 格式。"""
    return datetime.now(timezone.utc).date().isoformat()


def _legacy_progress(snapshot: dict[str, Any]) -> dict[str, Any]:
    """把旧 last_push_snapshot 映射为新进度结构。"""
    completed = snapshot.get("completed_steps")
    return {
        "updatedAt": snapshot.get("snapshot_at") or "legacy",
        "completedSteps": completed if isinstance(completed, list) else [],
        "partialStep": snapshot.get("partial_step"),
        "nextStep": snapshot.get("next_step") or "未记录",
        "notes": snapshot.get("notes") if isinstance(snapshot.get("notes"), str) else "",
    }


def _extract_progress(data: dict[str, Any]) -> tuple[dict[str, Any] | None, str | None]:
    """读取新进度，必要时兼容旧 snapshot。"""
    progress = data.get("progress")
    if isinstance(progress, dict):
        return progress, "progress"
    snapshot = data.get("last_push_snapshot")
    if isinstance(snapshot, dict):
        return _legacy_progress(snapshot), "legacy-last-push-snapshot"
    return None, None


def _load_task_progress(repo_root: Path, task_dir: Path) -> dict[str, Any]:
    """读取单个任务的进度状态。"""
    task_json = _task_json_path(task_dir)
    data = _read_json(task_json)
    task_rel = _rel_path(repo_root, task_dir)
    if data is None:
        return {
            "status": "error",
            "reason": "invalid-task-json",
            "task": task_rel,
            "path": _rel_path(repo_root, task_json),
        }
    progress, source = _extract_progress(data)
    if progress is None:
        return {"status": "no-progress", "task": task_rel}
    validated, errors = _validate_progress(progress)
    if validated is None:
        return {
            "status": "error",
            "reason": "invalid-progress-schema",
            "task": task_rel,
            "source": source,
            "errors": errors,
        }
    return {
        "status": "ok",
        "task": task_rel,
        "taskStatus": data.get("status"),
        "source": source,
        "progress": validated,
        "summary": _progress_summary(validated),
    }


def _iter_active_task_dirs(repo_root: Path) -> list[Path]:
    """列出活动任务树的一层任务目录。"""
    tasks_dir = repo_root / DIR_WORKFLOW / DIR_TASKS
    try:
        return [
            path
            for path in sorted(tasks_dir.iterdir())
            if path.is_dir() and path.name != "archive"
        ]
    except OSError:
        return []


def _progress_candidates(
    repo_root: Path,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[dict[str, Any]]]:
    """扫描健康候选，并返回损坏进度与无法判定的任务诊断。"""
    candidates: list[dict[str, Any]] = []
    invalid_candidates: list[dict[str, Any]] = []
    scan_warnings: list[dict[str, Any]] = []
    for task_dir in _iter_active_task_dirs(repo_root):
        task_json = _task_json_path(task_dir)
        data = _read_json(task_json)
        if data is None:
            scan_warnings.append({
                "task": _rel_path(repo_root, task_dir),
                "path": _rel_path(repo_root, task_json),
                "reason": "invalid-task-json",
            })
            continue
        if data.get("status") not in ("in_progress", "completed"):
            continue
        progress, source = _extract_progress(data)
        if progress is None:
            continue
        validated, errors = _validate_progress(progress)
        if validated is None:
            invalid_candidates.append({
                "task": _rel_path(repo_root, task_dir),
                "source": source,
                "reason": "invalid-progress-schema",
                "errors": errors,
            })
            continue
        candidates.append({
            "task": _rel_path(repo_root, task_dir),
            "taskStatus": data.get("status"),
            "source": source,
            **_progress_summary(validated),
        })
    return candidates, invalid_candidates, scan_warnings


def _validate_progress(value: Any) -> tuple[dict[str, Any] | None, list[str]]:
    """校验 progress schema。"""
    if not isinstance(value, dict):
        return None, ["progress 必须是 JSON 对象"]

    errors: list[str] = []
    extra_fields = sorted(set(value) - REQUIRED_FIELDS)
    if extra_fields:
        errors.append(f"包含不支持的字段：{', '.join(extra_fields)}")

    for field in sorted(REQUIRED_FIELDS):
        if field not in value:
            errors.append(f"缺少必填字段 {field}")

    if not isinstance(value.get("updatedAt"), str) or not value.get("updatedAt", "").strip():
        errors.append("updatedAt 必须是非空字符串")

    completed = value.get("completedSteps")
    if not isinstance(completed, list) or any(not isinstance(item, str) for item in completed):
        errors.append("completedSteps 必须是字符串数组")

    partial = value.get("partialStep")
    if partial is not None and not isinstance(partial, str):
        errors.append("partialStep 必须是字符串或 null")

    if not isinstance(value.get("nextStep"), str) or not value.get("nextStep", "").strip():
        errors.append("nextStep 必须是非空字符串")

    if not isinstance(value.get("notes"), str):
        errors.append("notes 必须是字符串")

    if errors:
        return None, errors
    return value, []


def _print_json(data: dict[str, Any], exit_code: int = 0) -> int:
    """输出紧凑 JSON。"""
    print(json.dumps(data, ensure_ascii=False, sort_keys=True))
    return exit_code


def _print_status_text(data: dict[str, Any]) -> int:
    """输出便于人和 AI 阅读的状态文本。"""
    status = data.get("status")
    if status == "ok":
        summary = data.get("summary") if isinstance(data.get("summary"), dict) else {}
        print(f"任务：{data.get('task')}")
        print(f"任务状态：{data.get('taskStatus') or '(unknown)'}")
        print(f"来源：{data.get('source')}")
        print(f"completedSteps: {summary.get('completedSteps', [])}")
        print(f"partialStep: {summary.get('partialStep') or '(none)'}")
        print(f"nextStep: {summary.get('nextStep') or '(none)'}")
        print(f"notes: {summary.get('notes') or '(none)'}")
        return 0
    if status == "candidates":
        candidates = data.get("candidates")
        print("无活动任务。任务进度候选：")
        if isinstance(candidates, list) and candidates:
            for item in candidates:
                print(
                    f"- {item.get('task')} ({item.get('taskStatus') or 'unknown'}): "
                    f"nextStep={item.get('nextStep') or '(none)'}"
                )
        else:
            print("(none)")
        invalid = data.get("invalidCandidates")
        warnings = data.get("scanWarnings")
        if isinstance(invalid, list) and invalid:
            print("损坏的任务进度：")
            for item in invalid:
                print(f"- {item.get('task')}: {item.get('reason')}")
        if isinstance(warnings, list) and warnings:
            print("扫描警告：")
            for item in warnings:
                print(f"- {item.get('task')}: {item.get('reason')}")
        return 0
    if status == "no-progress":
        print(f"任务没有进度记录：{data.get('task')}")
        return 0
    if status == "no-current-task":
        print("没有活动任务，也没有可用的任务进度候选。")
        invalid = data.get("invalidCandidates")
        warnings = data.get("scanWarnings")
        if isinstance(invalid, list) and invalid:
            print(f"发现 {len(invalid)} 个损坏的任务进度候选。")
        if isinstance(warnings, list) and warnings:
            print(f"发现 {len(warnings)} 个任务扫描警告。")
        return 0
    print(f"错误：{data.get('reason') or status}", file=sys.stderr)
    return 1


def cmd_status(args: argparse.Namespace, repo_root: Path) -> int:
    """执行 status 子命令。"""
    if args.task:
        result = _load_task_progress(repo_root, _resolve_task_dir(repo_root, args.task))
    else:
        task_dir = _current_task_dir(repo_root)
        if task_dir is None:
            candidates, invalid_candidates, scan_warnings = _progress_candidates(repo_root)
            result = {
                "status": "candidates" if candidates else "no-current-task",
                "candidates": candidates,
                "invalidCandidates": invalid_candidates,
                "scanWarnings": scan_warnings,
            }
        else:
            result = _load_task_progress(repo_root, task_dir)

    exit_code = 1 if result.get("status") == "error" else 0
    if args.json:
        return _print_json(result, exit_code)
    return _print_status_text(result)


def cmd_write(args: argparse.Namespace, repo_root: Path) -> int:
    """执行 write 子命令。"""
    task_dir = _resolve_task_dir(repo_root, args.task)
    task_json = _task_json_path(task_dir)
    data = _read_json(task_json)
    if data is None:
        result = {
            "status": "error",
            "reason": "invalid-task-json",
            "task": _rel_path(repo_root, task_dir),
        }
        return _print_json(result, 1) if args.json else _print_status_text(result)

    try:
        raw_progress = json.loads(args.progress_json)
    except json.JSONDecodeError as exc:
        result = {
            "status": "error",
            "reason": "invalid-progress-json",
            "message": str(exc),
        }
        if args.json:
            return _print_json(result, 1)
        print(f"错误：invalid-progress-json：{exc}", file=sys.stderr)
        return 1

    progress, errors = _validate_progress(raw_progress)
    if progress is None:
        result = {
            "status": "error",
            "reason": "invalid-progress-schema",
            "errors": errors,
        }
        if args.json:
            return _print_json(result, 1)
        print("错误：invalid-progress-schema", file=sys.stderr)
        for error in errors:
            print(f"- {error}", file=sys.stderr)
        return 1

    if data.get("status") != "in_progress":
        result = {
            "status": "error",
            "reason": "invalid-status-transition",
            "task": _rel_path(repo_root, task_dir),
            "taskStatus": data.get("status"),
            "expectedStatus": "in_progress",
        }
        return _print_json(result, 1) if args.json else _print_status_text(result)

    migrated = isinstance(data.get("last_push_snapshot"), dict)
    data["progress"] = progress
    data.pop("last_push_snapshot", None)
    if getattr(args, "complete", False):
        data["status"] = "completed"
        data["completedAt"] = _utc_date()
    try:
        _write_json(task_json, data)
    except OSError as exc:
        result = {
            "status": "error",
            "reason": "write-failed",
            "message": str(exc),
            "task": _rel_path(repo_root, task_dir),
        }
        if args.json:
            return _print_json(result, 1)
        print(f"错误：write-failed：{exc}", file=sys.stderr)
        return 1

    result = {
        "status": "written",
        "task": _rel_path(repo_root, task_dir),
        "path": _rel_path(repo_root, task_json),
        "migratedLegacySnapshot": migrated,
        "taskStatus": data.get("status"),
        "summary": _progress_summary(progress),
    }
    if args.json:
        return _print_json(result)
    print(f"✓ 已更新任务进度：{result['path']}")
    print(f"nextStep: {result['summary'].get('nextStep') or '(none)'}")
    return 0


def cmd_reopen(args: argparse.Namespace, repo_root: Path) -> int:
    """把已完成任务显式恢复为进行中并保留进度证据。"""
    task_dir = _resolve_task_dir(repo_root, args.task)
    task_json = _task_json_path(task_dir)
    data = _read_json(task_json)
    if data is None:
        result = {
            "status": "error",
            "reason": "invalid-task-json",
            "task": _rel_path(repo_root, task_dir),
        }
        return _print_json(result, 1) if args.json else _print_status_text(result)
    if data.get("status") != "completed":
        result = {
            "status": "error",
            "reason": "invalid-status-transition",
            "task": _rel_path(repo_root, task_dir),
            "taskStatus": data.get("status"),
            "expectedStatus": "completed",
        }
        return _print_json(result, 1) if args.json else _print_status_text(result)

    data["status"] = "in_progress"
    data["completedAt"] = None
    try:
        _write_json(task_json, data)
    except OSError as exc:
        result = {
            "status": "error",
            "reason": "write-failed",
            "message": str(exc),
            "task": _rel_path(repo_root, task_dir),
        }
        return _print_json(result, 1) if args.json else _print_status_text(result)

    result = {
        "status": "reopened",
        "task": _rel_path(repo_root, task_dir),
        "path": _rel_path(repo_root, task_json),
        "taskStatus": "in_progress",
    }
    if args.json:
        return _print_json(result)
    print(f"✓ 已重新打开任务：{result['task']}")
    return 0


def build_parser() -> argparse.ArgumentParser:
    """创建命令行解析器。"""
    parser = argparse.ArgumentParser(description="读取或写入 Trellis 任务进度。")
    subparsers = parser.add_subparsers(dest="command", required=True)

    status = subparsers.add_parser("status", help="读取任务进度")
    status.add_argument("--task", help="任务目录、路径或任务名")
    status.add_argument("--json", action="store_true", help="输出 JSON")

    write = subparsers.add_parser("write", help="写入任务进度")
    write.add_argument("--task", required=True, help="任务目录、路径或任务名")
    write.add_argument("--progress-json", required=True, help="progress JSON 对象")
    write.add_argument(
        "--complete",
        action="store_true",
        help="与最终进度原子写入 completed 状态和 completedAt",
    )
    write.add_argument("--json", action="store_true", help="输出 JSON")

    reopen = subparsers.add_parser("reopen", help="把 completed 任务恢复为 in_progress")
    reopen.add_argument("--task", required=True, help="任务目录、路径或任务名")
    reopen.add_argument("--json", action="store_true", help="输出 JSON")

    return parser


def main() -> int:
    """命令入口。"""
    repo_root = _find_repo_root(Path.cwd())
    if repo_root is None:
        print("错误：不是 Trellis 项目（缺少 .trellis/）", file=sys.stderr)
        return 1

    parser = build_parser()
    args = parser.parse_args()
    if args.command == "status":
        return cmd_status(args, repo_root)
    if args.command == "write":
        return cmd_write(args, repo_root)
    if args.command == "reopen":
        return cmd_reopen(args, repo_root)
    parser.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
