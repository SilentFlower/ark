#!/usr/bin/env python3
"""管理当前 session 的无任务 direct-edit 流程游标。"""

from __future__ import annotations

import argparse
import json
import os
import sys
import tempfile
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from common.active_task import resolve_context_key


STATE_KEY = "untracked_flow"
STATE_VERSION = 2
LEGACY_STATE_VERSION = 1
VALID_SOURCES = {"inferred", "user-explicit"}
VALID_BEGIN_MODES = {"tracked-direct-edit"}
VALID_STAGES = {"implement", "check", "spec", "push"}
LEGACY_STAGES = VALID_STAGES | {"inspect"}
VALID_CLEAR_REASONS = {"completed", "abandoned", "adopted"}
STAGE_CONTRACTS = {
    "implement": {
        "owner": "trellis-route(target=implement)",
        "remainingOwners": [
            "trellis-route(target=implement)",
            "trellis-check-all",
            "trellis-update-spec",
            "trellis-push",
        ],
        "advanceAfterOwner": "python3 ./.trellis/scripts/untracked_flow.py advance --stage check",
    },
    "check": {
        "owner": "trellis-check-all",
        "remainingOwners": [
            "trellis-check-all",
            "trellis-update-spec",
            "trellis-push",
        ],
        "advanceAfterOwner": "python3 ./.trellis/scripts/untracked_flow.py advance --stage spec",
        "advanceOnFindings": "python3 ./.trellis/scripts/untracked_flow.py advance --stage implement",
    },
    "spec": {
        "owner": "trellis-update-spec",
        "remainingOwners": [
            "trellis-update-spec",
            "trellis-push",
        ],
        "advanceAfterOwner": "python3 ./.trellis/scripts/untracked_flow.py advance --stage push",
    },
    "push": {
        "owner": "trellis-push",
        "remainingOwners": ["trellis-push"],
        "clearAfterOwner": "python3 ./.trellis/scripts/untracked_flow.py clear --reason completed",
    },
}


class UntrackedFlowError(Exception):
    """携带稳定 reason code 的 untracked 状态错误。"""

    def __init__(self, reason: str, message: str, **details: Any) -> None:
        """初始化结构化状态错误。

        Args:
            reason: 稳定机器错误码。
            message: 中文诊断说明。
            **details: 与错误相关的状态详情。
        """
        super().__init__(message)
        self.reason = reason
        self.details = details


def _utc_now() -> str:
    """返回秒级 UTC ISO-8601 时间。"""
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _find_root_upwards(start: Path) -> Path | None:
    """从指定路径向上查找带 .trellis 的目录。"""
    current = start.resolve()
    if current.is_file():
        current = current.parent
    while True:
        if (current / ".trellis").is_dir():
            return current
        # `.git` 文件或目录标记当前 worktree 边界；本地缺 Trellis 时不能继续读父 worktree。
        if (current / ".git").exists() or (current / ".git").is_symlink():
            return None
        if current == current.parent:
            return None
        current = current.parent


def _find_repo_root(start: Path | None = None) -> Path | None:
    """只在当前 worktree 路径向上查找 Trellis 项目根目录。"""
    current = (start or Path.cwd()).resolve()
    return _find_root_upwards(current)


def _session_path(repo_root: Path, context_key: str) -> Path:
    """返回指定 session runtime 文件路径。"""
    return repo_root / ".trellis/.runtime/sessions" / f"{context_key}.json"


def _read_json_result(path: Path) -> dict[str, Any]:
    """读取 JSON，并保留缺失、损坏和 I/O 错误差异。"""
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
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
    *,
    allow_active_task: bool = False,
) -> dict[str, Any]:
    """解析严格绑定当前 session 的 runtime 上下文。"""
    context_key = resolve_context_key(platform_input, platform)
    if not context_key:
        raise UntrackedFlowError("no-session-context", "无法解析当前 AI session")
    path = _session_path(repo_root, context_key)
    result = _read_json_result(path)
    if result["status"] in {"corrupt", "io_error"}:
        raise UntrackedFlowError(
            f"session-runtime-{result['status']}",
            "当前 session runtime 无法安全读取",
            error=result.get("error"),
        )
    context = result["data"] if isinstance(result.get("data"), dict) else {}
    current_task = context.get("current_task")
    if not allow_active_task and isinstance(current_task, str) and current_task.strip():
        raise UntrackedFlowError(
            "active-task-present",
            "当前 session 已绑定活动 task，不能创建无任务事项",
            task=current_task,
        )
    return {
        "context_key": context_key,
        "path": path,
        "context": context,
        "current_task": current_task,
    }


def _normalized_state(value: Any) -> dict[str, Any] | None:
    """校验并投影 v1/v2 untracked 状态，非法时返回 None。"""
    if not isinstance(value, dict):
        return None
    version = value.get("version")
    if version not in {LEGACY_STATE_VERSION, STATE_VERSION}:
        return None
    if version == LEGACY_STATE_VERSION and value.get("mode") != "direct_edit":
        return None
    source = value.get("source")
    stage = value.get("stage")
    work_id = value.get("id")
    summary = value.get("summary")
    if (
        source not in VALID_SOURCES
        or stage not in LEGACY_STAGES
        or not isinstance(work_id, str)
        or not work_id.strip()
        or not isinstance(summary, str)
        or not summary.strip()
    ):
        return None
    created_at = value.get("createdAt")
    updated_at = value.get("updatedAt")
    return {
        "version": STATE_VERSION,
        "id": work_id,
        "source": source,
        "summary": summary,
        "stage": "implement" if stage == "inspect" else stage,
        "createdAt": created_at if isinstance(created_at, str) else "",
        "updatedAt": updated_at if isinstance(updated_at, str) else "",
    }


def _state_summary(state: dict[str, Any]) -> dict[str, Any]:
    """返回默认输出需要的最小状态字段。"""
    stage = state.get("stage")
    summary = {
        "workId": state.get("id"),
        "summary": state.get("summary"),
        "source": state.get("source"),
        "stage": stage,
    }
    if isinstance(stage, str) and stage in STAGE_CONTRACTS:
        summary.update(_stage_contract(stage))
    return summary


def _stage_contract(stage: str) -> dict[str, Any]:
    """返回当前阶段的 Trellis owner 和剩余完成链提示。"""
    contract = STAGE_CONTRACTS[stage]
    return {
        key: list(value) if isinstance(value, list) else value
        for key, value in contract.items()
    }


def _persist(scope: dict[str, Any], state: dict[str, Any] | None) -> None:
    """保存或清理当前 session 的 untracked 字段。"""
    context = dict(scope["context"])
    context.setdefault(
        "platform",
        scope["context_key"].split("_", 1)[0]
        if "_" in scope["context_key"]
        else "session",
    )
    context["last_seen_at"] = _utc_now()
    if state is None:
        context.pop(STATE_KEY, None)
    else:
        context[STATE_KEY] = state
    _write_json(scope["path"], context)


def begin_untracked_work(
    repo_root: Path,
    summary: str,
    source: str,
    mode: str | None,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
) -> dict[str, Any]:
    """为当前 session 创建单一 untracked work item。

    Args:
        repo_root: Trellis 项目根目录。
        summary: 当前事项摘要。
        source: ``inferred`` 或 ``user-explicit``。
        mode: 入口模式，必须是 ``tracked-direct-edit``。
        platform_input: 可选平台 hook 输入。
        platform: 可选平台名称。

    Returns:
        创建或命中同一事项的结构化结果。
    """
    if mode not in VALID_BEGIN_MODES:
        raise UntrackedFlowError(
            "invalid-entry-mode",
            "只有需要跨轮恢复的无任务直接修改才能创建无任务事项",
            mode=mode,
            allowed=sorted(VALID_BEGIN_MODES),
        )
    clean_summary = summary.strip()
    if not clean_summary:
        raise UntrackedFlowError("summary-empty", "无任务事项摘要不能为空")
    if source not in VALID_SOURCES:
        raise UntrackedFlowError("invalid-source", "无任务事项来源不合法", source=source)
    scope = _runtime_scope(repo_root, platform_input, platform)
    existing = _normalized_state(scope["context"].get(STATE_KEY))
    if existing:
        if existing["summary"] == clean_summary:
            return {"status": "hit", **_state_summary(existing)}
        raise UntrackedFlowError(
            "active-work-conflict",
            "当前 session 已有另一个活跃无任务事项",
            activeWorkId=existing["id"],
            activeSummary=existing["summary"],
        )
    if STATE_KEY in scope["context"]:
        raise UntrackedFlowError("invalid-state", "当前 session 的无任务状态已损坏")

    now = _utc_now()
    state = {
        "version": STATE_VERSION,
        "id": f"uw-{uuid.uuid4().hex[:12]}",
        "source": source,
        "summary": clean_summary,
        "stage": "implement",
        "createdAt": now,
        "updatedAt": now,
    }
    _persist(scope, state)
    return {"status": "created", **_state_summary(state)}


def advance_stage(
    repo_root: Path,
    stage: str,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
) -> dict[str, Any]:
    """更新当前 untracked 事项的流程游标。

    Args:
        repo_root: Trellis 项目根目录。
        stage: ``implement``、``check``、``spec`` 或 ``push``。
        platform_input: 可选平台 hook 输入。
        platform: 可选平台名称。

    Returns:
        更新后的事项状态摘要。
    """
    if stage not in VALID_STAGES:
        raise UntrackedFlowError("invalid-stage", "目标阶段不合法", stage=stage)
    scope = _runtime_scope(repo_root, platform_input, platform)
    state = _normalized_state(scope["context"].get(STATE_KEY))
    if state is None:
        raise UntrackedFlowError("no-active-work", "当前 session 没有活跃无任务事项")
    state["stage"] = stage
    state["updatedAt"] = _utc_now()
    if not state.get("createdAt"):
        state["createdAt"] = state["updatedAt"]
    _persist(scope, state)
    return {"status": "advanced", **_state_summary(state)}


def read_untracked_state(
    repo_root: Path,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
    *,
    allow_active_task: bool = False,
    validate_workspace: bool | None = None,
) -> dict[str, Any]:
    """读取当前 session 的 untracked 流程游标。

    Args:
        repo_root: Trellis 项目根目录。
        platform_input: 可选平台 hook 输入。
        platform: 可选平台名称。
        allow_active_task: 是否允许 session 同时已有 task，供 adoption 清理使用。
        validate_workspace: 兼容旧调用方的废弃参数；不再读取或校验 Git workspace。

    Returns:
        ``hit`` 或 ``miss`` 的结构化状态。
    """
    del validate_workspace
    scope = _runtime_scope(
        repo_root,
        platform_input,
        platform,
        allow_active_task=allow_active_task,
    )
    raw = scope["context"].get(STATE_KEY)
    if raw is None:
        return {"status": "miss", "reason": "no-active-work", "contextKey": scope["context_key"]}
    state = _normalized_state(raw)
    if state is None:
        raise UntrackedFlowError("invalid-state", "当前 session 的无任务状态已损坏")
    return {
        "status": "hit",
        **_state_summary(state),
        "contextKey": scope["context_key"],
        "path": scope["path"],
        "state": state,
    }


def clear_untracked_state(
    repo_root: Path,
    reason: str,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
    *,
    work_id: str | None = None,
    allow_active_task: bool = True,
) -> dict[str, Any]:
    """按明确原因清理当前 session 的 untracked 状态。

    Args:
        repo_root: Trellis 项目根目录。
        reason: 完成、放弃或纳管原因。
        platform_input: 可选平台 hook 输入。
        platform: 可选平台名称。
        work_id: 可选的精确事项 ID 防误清。
        allow_active_task: 是否允许 session 已绑定 task。

    Returns:
        清理结果和被清理事项 ID。
    """
    if reason not in VALID_CLEAR_REASONS:
        raise UntrackedFlowError("invalid-clear-reason", "无任务状态清理原因不合法")
    scope = _runtime_scope(
        repo_root,
        platform_input,
        platform,
        allow_active_task=allow_active_task,
    )
    state = _normalized_state(scope["context"].get(STATE_KEY))
    if state is None:
        return {"status": "cleared", "existed": False, "reason": reason}
    if work_id and state["id"] != work_id:
        raise UntrackedFlowError(
            "work-id-mismatch",
            "待清理事项与当前 session 状态不匹配",
            expected=work_id,
            actual=state["id"],
        )
    _persist(scope, None)
    return {
        "status": "cleared",
        "existed": True,
        "reason": reason,
        "workId": state["id"],
    }


def session_start_hint(
    repo_root: Path,
    platform_input: dict[str, Any] | None = None,
    platform: str | None = None,
) -> str | None:
    """返回适合 SessionStart 的紧凑 untracked 恢复提示。

    Args:
        repo_root: Trellis 项目根目录。
        platform_input: 可选平台 hook 输入。
        platform: 可选平台名称。

    Returns:
        命中时返回一行提示，否则返回 None。
    """
    try:
        result = read_untracked_state(repo_root, platform_input, platform)
    except UntrackedFlowError:
        return None
    if result.get("status") != "hit":
        return None
    owner = result.get("owner")
    remaining_owners = result.get("remainingOwners")
    remaining = (
        [value for value in remaining_owners if isinstance(value, str) and value]
        if isinstance(remaining_owners, list)
        else []
    )
    remaining_text = " -> ".join(remaining) if remaining else str(owner)
    return (
        f"Untracked work: {result['workId']}; stage={result['stage']}; "
        f"owner={owner}; remaining={remaining_text}; "
        f"summary={result['summary']}."
    )


def _emit(payload: dict[str, Any], *, include_state: bool = False) -> None:
    """向 stdout 输出稳定 JSON。"""
    serializable = {
        key: str(value) if isinstance(value, Path) else value
        for key, value in payload.items()
        if include_state or key != "state"
    }
    print(json.dumps(serializable, ensure_ascii=False, sort_keys=True))


def build_parser() -> argparse.ArgumentParser:
    """构造 untracked flow CLI parser。

    Returns:
        已配置全部状态命令的 parser。
    """
    parser = argparse.ArgumentParser(description="Trellis untracked workflow cursor helper")
    subparsers = parser.add_subparsers(dest="command", required=True)

    begin_parser = subparsers.add_parser("begin", help="create one untracked work item")
    begin_parser.add_argument("--summary", required=True)
    begin_parser.add_argument("--source", choices=sorted(VALID_SOURCES), required=True)
    begin_parser.add_argument("--mode", required=True)

    advance_parser = subparsers.add_parser("advance", help="set workflow stage cursor")
    advance_parser.add_argument("--stage", choices=sorted(VALID_STAGES), required=True)

    status_parser = subparsers.add_parser("status", help="read current untracked work state")
    status_parser.add_argument("--verbose", action="store_true")
    subparsers.add_parser("session-start-hint", help="render compact recovery hint")

    clear_parser = subparsers.add_parser("clear", help="clear current untracked work state")
    clear_parser.add_argument("--reason", choices=sorted(VALID_CLEAR_REASONS), required=True)
    clear_parser.add_argument("--work-id")
    return parser


def main(argv: list[str] | None = None) -> int:
    """执行 untracked flow CLI。

    Args:
        argv: 可选命令参数，默认读取 ``sys.argv``。

    Returns:
        成功或 miss 为 0，状态错误为 1。
    """
    args = build_parser().parse_args(argv)
    repo_root = _find_repo_root()
    if repo_root is None:
        _emit({"status": "error", "reason": "not-trellis-project"})
        return 1
    try:
        if args.command == "begin":
            payload = begin_untracked_work(
                repo_root,
                args.summary,
                args.source,
                args.mode,
            )
        elif args.command == "advance":
            payload = advance_stage(repo_root, args.stage)
        elif args.command == "status":
            payload = read_untracked_state(repo_root)
        elif args.command == "session-start-hint":
            hint = session_start_hint(repo_root)
            payload = {"status": "hit", "hint": hint} if hint else {"status": "miss"}
        else:
            payload = clear_untracked_state(repo_root, args.reason, work_id=args.work_id)
        _emit(payload, include_state=args.command == "status" and args.verbose)
        return 0
    except (UntrackedFlowError, OSError) as error:
        if isinstance(error, UntrackedFlowError):
            payload = {
                "status": "error",
                "reason": error.reason,
                "message": str(error),
                **error.details,
            }
        else:
            payload = {"status": "error", "reason": "runtime-write-failed", "message": str(error)}
        _emit(payload)
        return 1


if __name__ == "__main__":
    sys.exit(main())
