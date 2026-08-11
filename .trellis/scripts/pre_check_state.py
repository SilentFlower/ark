#!/usr/bin/env python3
"""读写当前 session 的 Check-All 前暂缓偏好。"""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from common.active_task import resolve_active_task, resolve_context_key


PREFERENCE_KEY = "pre_check_preference"
PREFERENCE_VERSION = 2
LEGACY_PREFERENCE_VERSION = 1
VALID_SOURCES = {"user-explicit", "follow-up-edit"}
UNTRACKED_STATE_VERSIONS = {1, 2}
UNTRACKED_STAGES = {"inspect", "implement", "check", "spec", "push"}
SESSION_START_HINT = "Pre-check: deferred for current work; latest user intent may override."


def _find_repo_root(start: Path | None = None) -> Path | None:
    """从当前目录向上查找 Trellis 项目根目录。"""
    current = (start or Path.cwd()).resolve()
    if current.is_file():
        current = current.parent
    while True:
        if (current / ".trellis").is_dir():
            return current
        if current == current.parent:
            break
        current = current.parent

    script_root = Path(__file__).resolve().parents[2]
    return script_root if (script_root / ".trellis").is_dir() else None


def _utc_now() -> str:
    """返回秒级 UTC ISO-8601 时间。"""
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _session_path(repo_root: Path, context_key: str) -> Path:
    """返回指定 session 的 runtime 文件。"""
    return repo_root / ".trellis/.runtime/sessions" / f"{context_key}.json"


def _read_json_result(path: Path) -> dict[str, Any]:
    """读取 JSON，并保留缺失、损坏和 I/O 错误的差异。"""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {"status": "missing", "data": None, "error": None}
    except json.JSONDecodeError as error:
        return {"status": "corrupt", "data": None, "error": str(error)}
    except OSError as error:
        return {"status": "io_error", "data": None, "error": str(error)}
    if not isinstance(data, dict):
        return {"status": "corrupt", "data": None, "error": "JSON 根节点不是对象"}
    return {"status": "ok", "data": data, "error": None}


def _write_json(path: Path, data: dict[str, Any]) -> None:
    """使用同目录临时文件原子替换 session runtime。"""
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


def _runtime_scope(
    repo_root: Path,
    platform_input: dict[str, Any] | None,
    platform: str | None,
    active: Any | None,
) -> dict[str, Any]:
    """解析严格绑定当前 session 的 task 或 untracked subject。"""
    context_key = resolve_context_key(platform_input, platform)
    if not context_key:
        return {"status": "miss", "reason": "no-session-context"}

    path = _session_path(repo_root, context_key)
    result = _read_json_result(path)
    if result["status"] in {"corrupt", "io_error"}:
        return {
            "status": "error",
            "reason": f"session-runtime-{result['status']}",
            "context_key": context_key,
            "path": path,
            "error": result.get("error"),
        }
    context = result["data"] if isinstance(result.get("data"), dict) else {}
    active_task = active or resolve_active_task(repo_root, platform_input, platform)
    source_type = getattr(active_task, "source_type", None)
    if source_type in {"session-corrupt", "session-io_error"}:
        return {
            "status": "error",
            "reason": f"session-runtime-{source_type.removeprefix('session-')}",
            "context_key": context_key,
            "path": path,
        }
    task = getattr(active_task, "task_path", None)
    if isinstance(task, str) and task:
        # 不接受 unique-session fallback。新 AI session 即使看到旧任务指针，也不能继承旧 hold。
        if getattr(active_task, "context_key", None) != context_key:
            return {"status": "miss", "reason": "session-task-mismatch"}
        return {
            "status": "ok",
            "subject": {"kind": "task", "id": task},
            "task": task,
            "context_key": context_key,
            "path": path,
            "context": context,
        }

    untracked = context.get("untracked_flow")
    if isinstance(untracked, dict):
        work_id = untracked.get("id")
        if not (
            untracked.get("version") in UNTRACKED_STATE_VERSIONS
            and isinstance(work_id, str)
            and work_id.strip()
            and untracked.get("stage") in UNTRACKED_STAGES
        ):
            return {
                "status": "error",
                "reason": "invalid-untracked-state",
                "context_key": context_key,
                "path": path,
            }
        return {
            "status": "ok",
            "subject": {"kind": "untracked", "id": work_id},
            "workId": work_id,
            "context_key": context_key,
            "path": path,
            "context": context,
        }

    return {"status": "miss", "reason": "no-current-work"}


def _preference_subject(preference: dict[str, Any]) -> dict[str, str] | None:
    """读取 v2 subject，并兼容旧版 task 绑定。"""
    if preference.get("version") == PREFERENCE_VERSION:
        subject = preference.get("subject")
        if not isinstance(subject, dict):
            return None
        kind = subject.get("kind")
        identifier = subject.get("id")
        if kind in {"task", "untracked"} and isinstance(identifier, str) and identifier:
            return {"kind": kind, "id": identifier}
        return None
    if preference.get("version") == LEGACY_PREFERENCE_VERSION:
        task = preference.get("task")
        if isinstance(task, str) and task:
            return {"kind": "task", "id": task}
    return None


def _context_matches_subject(context: dict[str, Any], subject: dict[str, str]) -> bool:
    """判断 runtime 当前工作是否仍与 hold subject 一致。"""
    if subject["kind"] == "task":
        return context.get("current_task") == subject["id"]
    state = context.get("untracked_flow")
    return isinstance(state, dict) and state.get("id") == subject["id"]


def _subject_result_fields(subject: dict[str, str]) -> dict[str, Any]:
    """构造兼容 task 调用方并支持 untracked 的结果字段。"""
    fields: dict[str, Any] = {"subject": subject}
    if subject["kind"] == "task":
        fields["task"] = subject["id"]
    else:
        fields["workId"] = subject["id"]
    return fields


def read_pre_check_preference(
    repo_root: Path,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
    active: Any | None = None,
) -> dict[str, Any]:
    """读取当前工作的 Check-All 前暂缓偏好。

    Args:
        repo_root: Trellis 项目根目录。
        platform_input: 可选的平台 hook 输入。
        platform: 可选的平台名称。
        active: 可复用的活动任务解析结果。

    Returns:
        包含 ``hit``、``miss`` 或 ``error`` 状态的结构化结果。
    """
    scope = _runtime_scope(repo_root, platform_input, platform, active)
    if scope["status"] != "ok":
        return scope

    result = _read_json_result(scope["path"])
    if result["status"] in {"corrupt", "io_error"}:
        return {
            **scope,
            "status": "error",
            "reason": f"session-runtime-{result['status']}",
            "error": result.get("error"),
        }
    context = result["data"] if isinstance(result.get("data"), dict) else {}
    preference = context.get(PREFERENCE_KEY)
    if not isinstance(preference, dict):
        return {**scope, "status": "miss", "reason": "no-hold"}
    if (
        preference.get("mode") != "hold"
        or preference.get("source") not in VALID_SOURCES
    ):
        return {**scope, "status": "miss", "reason": "invalid-hold"}
    subject = _preference_subject(preference)
    if subject is None:
        return {**scope, "status": "miss", "reason": "invalid-hold"}
    if subject != scope["subject"]:
        return {**scope, "status": "miss", "reason": "subject-mismatch"}
    return {
        "status": "hit",
        "mode": "hold",
        **_subject_result_fields(subject),
        "source": preference["source"],
        "updated_at": preference.get("updated_at"),
        "context_key": scope["context_key"],
        "path": scope["path"],
    }


def set_pre_check_hold(
    repo_root: Path,
    source: str,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
) -> dict[str, Any]:
    """为当前 session 的活动工作写入软暂缓偏好。

    Args:
        repo_root: Trellis 项目根目录。
        source: ``user-explicit`` 或 ``follow-up-edit``。
        platform_input: 可选的平台 hook 输入。
        platform: 可选的平台名称。

    Returns:
        写入结果；损坏 runtime 会返回 ``error`` 且保持原文件不变。
    """
    if source not in VALID_SOURCES:
        return {"status": "error", "reason": "invalid-source"}
    scope = _runtime_scope(repo_root, platform_input, platform, None)
    if scope["status"] != "ok":
        return scope

    result = _read_json_result(scope["path"])
    if result["status"] in {"corrupt", "io_error"}:
        return {
            **scope,
            "status": "error",
            "reason": f"session-runtime-{result['status']}",
            "error": result.get("error"),
        }
    context = result["data"] if isinstance(result.get("data"), dict) else {}
    if not _context_matches_subject(context, scope["subject"]):
        return {**scope, "status": "error", "reason": "runtime-subject-mismatch"}
    preference = {
        "version": PREFERENCE_VERSION,
        "subject": scope["subject"],
        "mode": "hold",
        "source": source,
        "updated_at": _utc_now(),
    }
    context[PREFERENCE_KEY] = preference
    try:
        _write_json(scope["path"], context)
    except OSError as error:
        return {**scope, "status": "error", "reason": "runtime-write-failed", "error": str(error)}
    return {
        "status": "held",
        "mode": "hold",
        **_subject_result_fields(scope["subject"]),
        "source": source,
        "updated_at": preference["updated_at"],
        "context_key": scope["context_key"],
        "path": scope["path"],
    }


def clear_pre_check_preference(
    repo_root: Path,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
) -> dict[str, Any]:
    """清除当前工作匹配的软暂缓偏好。

    Args:
        repo_root: Trellis 项目根目录。
        platform_input: 可选的平台 hook 输入。
        platform: 可选的平台名称。

    Returns:
        结构化清理结果；任务不匹配时只忽略，不删除其它任务状态。
    """
    scope = _runtime_scope(repo_root, platform_input, platform, None)
    if scope["status"] != "ok":
        return scope

    result = _read_json_result(scope["path"])
    if result["status"] in {"corrupt", "io_error"}:
        return {
            **scope,
            "status": "error",
            "reason": f"session-runtime-{result['status']}",
            "error": result.get("error"),
        }
    context = result["data"] if isinstance(result.get("data"), dict) else {}
    preference = context.get(PREFERENCE_KEY)
    if not isinstance(preference, dict):
        return {**scope, "status": "cleared", "existed": False}
    subject = _preference_subject(preference)
    if subject != scope["subject"]:
        return {**scope, "status": "miss", "reason": "subject-mismatch"}

    context.pop(PREFERENCE_KEY, None)
    try:
        _write_json(scope["path"], context)
    except OSError as error:
        return {**scope, "status": "error", "reason": "runtime-write-failed", "error": str(error)}
    return {**scope, "status": "cleared", "existed": True}


def session_start_hint(
    repo_root: Path,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
    active: Any | None = None,
) -> str | None:
    """返回 SessionStart 所需的一行暂缓提示。

    Args:
        repo_root: Trellis 项目根目录。
        platform_input: 平台 SessionStart hook 输入。
        platform: 平台名称。
        active: SessionStart 已解析的活动任务结果。

    Returns:
        当前任务命中 hold 时返回提示，否则返回 ``None``。
    """
    result = read_pre_check_preference(repo_root, platform_input, platform, active)
    return SESSION_START_HINT if result.get("status") == "hit" else None


def _compact_result(result: dict[str, Any], verbose: bool) -> dict[str, Any]:
    """压缩默认 CLI 输出，诊断模式保留完整字段。"""
    if verbose:
        payload = dict(result)
        path = payload.get("path")
        if isinstance(path, Path):
            payload["path"] = str(path)
        return payload
    keys = ("status", "reason", "mode", "subject", "task", "workId", "source", "existed")
    return {key: result[key] for key in keys if key in result}


def build_parser() -> argparse.ArgumentParser:
    """构造 pre-check 状态 CLI 参数解析器。

    Returns:
        配置完成的 ``ArgumentParser``。
    """
    parser = argparse.ArgumentParser(description="Read/write Trellis pre-check preference.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    status_parser = subparsers.add_parser("status", help="read current pre-check preference")
    status_parser.add_argument("--verbose", action="store_true")

    hold_parser = subparsers.add_parser("hold", help="defer the next interactive Check-All")
    hold_parser.add_argument("--source", choices=sorted(VALID_SOURCES), required=True)
    hold_parser.add_argument("--verbose", action="store_true")

    clear_parser = subparsers.add_parser("clear", help="clear current pre-check preference")
    clear_parser.add_argument("--verbose", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    """执行 pre-check 状态命令。

    Args:
        argv: 可选命令行参数，默认读取 ``sys.argv``。

    Returns:
        进程退出码；业务 miss/error 通过 JSON 表达，不依赖非零退出码。
    """
    args = build_parser().parse_args(argv)
    repo_root = _find_repo_root()
    if repo_root is None:
        result = {"status": "miss", "reason": "not-trellis-project"}
    elif args.command == "status":
        result = read_pre_check_preference(repo_root)
    elif args.command == "hold":
        result = set_pre_check_hold(repo_root, args.source)
    else:
        result = clear_pre_check_preference(repo_root)
    print(json.dumps(_compact_result(result, args.verbose), ensure_ascii=False, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
