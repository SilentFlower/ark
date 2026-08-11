#!/usr/bin/env python3
"""Trellis auto loop 的可恢复流程控制器。"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import socket
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from decision_log import DecisionLogError, append_decision
from git_evidence import GitEvidenceError, discover_git_repositories, parse_porcelain_z

SCHEMA_VERSION = 2
LEGACY_SCHEMA_VERSION = 1
DEFAULT_PROFILE = "commit-only"
MAX_FIX_RECHECK = 3
MAX_COMMIT_REPAIR = MAX_FIX_RECHECK
MAX_PLANNING_REPAIR = 3
MAX_ARTIFACT_RECONCILE = 3
DECISION_LOG_LIMIT = 20
MANIFEST_AUDIT_TAIL_LIMIT = 3
TASK_PROGRESS_NOTE_LIMIT = 500
ACTIVE_RUN_STATUSES = {"preparing", "awaiting_input", "running"}
TERMINAL_RUN_STATUSES = {
    "completed",
    "completed_with_blocked",
    "globally_blocked",
    "stopped",
}
ALLOWED_TASK_STATUSES = {"planning", "in_progress"}
VALID_IMPLEMENT_ROUTES = {"inline", "subagent"}
VALID_CHECK_ROUTES = {"check-all-inline", "check-all-subagent"}
VALID_CHECK_DEPTHS = {"auto", "light", "full"}
CHECK_ACTIONS = {"run_check_all", "run_recheck"}
RECOVERABLE_BLOCK_REASONS = {
    "missing-prd",
    "open-questions",
    "open-questions-ambiguous",
    "planning-readiness",
    "planning-readiness-ambiguous",
    "incomplete-complex-artifacts",
    "missing-implement-context",
    "missing-check-context",
    "planning-repair-budget-exhausted",
    "retry-budget-exhausted",
    "commit-repair-budget-exhausted",
    "artifact-drift",
    "protected-path-conflict",
    "protected-baseline-drift",
    "blocked-dependency",
    "unknown-step",
}
STEP_ACTIONS = {
    "classify_dirty_baseline": "classify_dirty_baseline",
    "resolve_open_questions": "resolve_open_questions",
    "review_open_questions": "review_open_questions",
    "review_planning_readiness": "review_planning_readiness",
    "planning_repair": "run_planning_repair",
    "refresh_brief": "refresh_brief",
    "confirm_brief": "confirm_brief",
    "start_task": "start_task",
    "implement": "run_implement",
    "check": "run_check_all",
    "fix": "run_fix",
    "recheck": "run_recheck",
    "spec_update": "run_spec_update",
    "commit_only": "commit_only",
}
ROUTE_ACTION_TARGETS = {
    "run_implement": "implement",
    "run_fix": "implement",
    "run_check_all": "check",
    "run_recheck": "check",
}


def _utc_now() -> str:
    """返回秒级 UTC 时间字符串。"""
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _utc_date() -> str:
    """返回 UTC 日期，保持 task.json 既有 completedAt 格式。"""
    return datetime.now(timezone.utc).date().isoformat()


def _repo_root() -> Path | None:
    """从当前目录向上寻找 Trellis 项目根。"""
    current = Path.cwd().resolve()
    while True:
        if (current / ".trellis").is_dir():
            return current
        if current == current.parent:
            return None
        current = current.parent


def _print(data: dict[str, Any]) -> int:
    """输出给 agent 消费的紧凑 JSON。"""
    print(json.dumps(data, ensure_ascii=False, sort_keys=True))
    return 0


def _read_json_result(path: Path) -> dict[str, Any]:
    """读取 runtime JSON，并保留缺失、损坏和 I/O 错误的区别。"""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {"status": "missing", "data": None, "error": None}
    except json.JSONDecodeError as exc:
        return {"status": "corrupt", "data": None, "error": str(exc)}
    except OSError as exc:
        return {"status": "io_error", "data": None, "error": str(exc)}
    if not isinstance(data, dict):
        return {"status": "corrupt", "data": None, "error": "JSON 根节点不是对象"}
    return {"status": "ok", "data": data, "error": None}


def _read_json(path: Path) -> dict[str, Any]:
    """兼容非关键读取；仅返回成功解析的 JSON 对象。"""
    result = _read_json_result(path)
    data = result.get("data")
    return data if isinstance(data, dict) else {}


def _write_json(path: Path, data: dict[str, Any]) -> None:
    """使用同目录临时文件原子写入 runtime JSON。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
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
    """尽量返回相对项目根的 POSIX 路径。"""
    try:
        return path.relative_to(repo_root).as_posix()
    except ValueError:
        return str(path)


def _repo_root_from_run_path(path: Path) -> Path | None:
    """从 auto-loop runtime 文件路径反推项目根目录。"""
    try:
        if path.parents[2].name == ".trellis":
            return path.parents[3]
    except IndexError:
        return None
    return None


def _manifest_audit_path(path: Path) -> Path:
    """返回当前 run 的 manifest audit JSONL 路径。"""
    return path.with_suffix(".manifest.jsonl")


def _audit_rel_path(path: Path, audit_path: Path) -> str:
    """返回可写入 runtime 的相对 audit 路径。"""
    repo_root = _repo_root_from_run_path(path)
    if repo_root is not None:
        return _rel_path(repo_root, audit_path)
    return audit_path.name


def _manifest_event(payload: dict[str, Any]) -> dict[str, Any]:
    """把 manifest payload 包装为可去重的 audit 事件。"""
    revision = int(payload.get("revision") or 0)
    sha256 = str(payload.get("sha256") or "")
    return {
        "event_id": f"manifest-{revision:06d}-{sha256[:12]}",
        "type": "manifest_revision",
        "revision": revision,
        "sha256": sha256,
        "created_at": payload.get("created_at"),
        "payload": payload,
    }


def _read_manifest_event_keys(path: Path) -> set[tuple[int, str]]:
    """读取已写入 manifest audit 的 revision/sha256 键。"""
    keys: set[tuple[int, str]] = set()
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        return keys
    except OSError:
        return keys
    for line in lines:
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict) or event.get("type") != "manifest_revision":
            continue
        try:
            revision = int(event.get("revision") or 0)
        except (TypeError, ValueError):
            continue
        sha256 = event.get("sha256")
        if isinstance(sha256, str) and sha256:
            keys.add((revision, sha256))
    return keys


def _append_manifest_events(path: Path, revisions: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """把完整 manifest revision 追加到 audit JSONL，并跳过已存在事件。"""
    events = [_manifest_event(payload) for payload in revisions if isinstance(payload, dict)]
    if not events:
        return []
    path.parent.mkdir(parents=True, exist_ok=True)
    existing = _read_manifest_event_keys(path)
    pending = [
        event
        for event in events
        if (int(event.get("revision") or 0), str(event.get("sha256") or "")) not in existing
    ]
    if not pending:
        return events
    with path.open("a", encoding="utf-8") as handle:
        for event in pending:
            handle.write(json.dumps(event, ensure_ascii=False, sort_keys=True) + "\n")
        handle.flush()
        os.fsync(handle.fileno())
    return events


def _compact_manifest_history(path: Path, state: dict[str, Any]) -> None:
    """把主 runtime 中的完整 manifest 历史迁移到旁路 JSONL。"""
    revisions = state.get("manifest_revisions")
    if not isinstance(revisions, list) or not revisions:
        state.pop("manifest_revisions", None)
        if state.get("manifest_revision"):
            state.setdefault("manifest_audit_path", _audit_rel_path(path, _manifest_audit_path(path)))
        return
    audit_path = _manifest_audit_path(path)
    events = _append_manifest_events(audit_path, [item for item in revisions if isinstance(item, dict)])
    state["manifest_audit_path"] = _audit_rel_path(path, audit_path)
    state["manifest_tail"] = [
        {
            "event_id": event.get("event_id"),
            "revision": event.get("revision"),
            "sha256": event.get("sha256"),
            "created_at": event.get("created_at"),
        }
        for event in events[-MANIFEST_AUDIT_TAIL_LIMIT:]
    ]
    state.pop("manifest_revisions", None)


def _auto_dir(repo_root: Path) -> Path:
    """返回 auto-loop runtime 目录。"""
    return repo_root / ".trellis/.runtime/auto-loop"


def _current_pointer(repo_root: Path) -> Path:
    """返回当前 auto run 指针文件。"""
    return _auto_dir(repo_root) / "current.json"


def _read_route_prefs(repo_root: Path) -> dict[str, str]:
    """读取个人 route 默认配置，用于 start gate 判断是否需要 JSONL context。"""
    path = repo_root / ".trellis/.route-prefs.tmp"
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return {}

    prefs: dict[str, str] = {}
    for raw in lines:
        if "=" not in raw:
            continue
        key, value = raw.split("=", 1)
        key = key.strip()
        value = value.strip()
        if key == "implement" and value in VALID_IMPLEMENT_ROUTES:
            prefs[key] = value
        elif key == "check" and value in VALID_CHECK_ROUTES:
            prefs[key] = value
    return prefs


def _route_state_helper(repo_root: Path) -> Path | None:
    """返回本地 trellis-route helper 路径，缺失时返回 None。"""
    candidates = [
        repo_root / ".agents/skills/trellis-route/scripts/route_state.py",
        repo_root / ".claude/skills/trellis-route/scripts/route_state.py",
    ]
    for path in candidates:
        if path.is_file():
            return path
    return None


def _current_task_ref(repo_root: Path) -> str | None:
    """读取当前活动任务路径，失败时返回 None。"""
    result = subprocess.run(
        ["python3", str(repo_root / ".trellis/scripts/task.py"), "current"],
        cwd=repo_root,
        check=False,
        text=True,
        capture_output=True,
    )
    for raw in result.stdout.splitlines():
        line = raw.strip()
        if line.startswith(".trellis/tasks/"):
            return line
        if "Current task:" in line:
            value = line.split("Current task:", 1)[1].strip()
            if value.startswith(".trellis/tasks/"):
                return value
    return None


def _resolve_existing_route(repo_root: Path, task_ref: str, target: str) -> str | None:
    """通过 trellis-route helper 解析已有 runtime/prefs route，失败时不阻断。"""
    helper = _route_state_helper(repo_root)
    if helper is None:
        return None
    if _current_task_ref(repo_root) != task_ref:
        return None
    result = subprocess.run(
        ["python3", str(helper), "resolve", "--target", target],
        cwd=repo_root,
        check=False,
        text=True,
        capture_output=True,
    )
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError:
        return None
    if not isinstance(data, dict) or data.get("status") != "hit":
        return None
    if data.get("task") != task_ref:
        return None
    mode = data.get("mode")
    if target == "implement" and mode in VALID_IMPLEMENT_ROUTES:
        return str(mode)
    if target == "check" and mode in VALID_CHECK_ROUTES:
        return str(mode)
    return None


def _effective_route_authorization(repo_root: Path, task_ref: str, route_authorization: Any) -> dict[str, str]:
    """按 route 优先级估算 start gate 需要的 context 类型。"""
    effective: dict[str, str] = {}
    if isinstance(route_authorization, dict):
        implement = route_authorization.get("implement")
        check = route_authorization.get("check")
        if implement in VALID_IMPLEMENT_ROUTES:
            effective["implement"] = str(implement)
        if check in VALID_CHECK_ROUTES:
            effective["check"] = str(check)

    # 个人默认优先于 auto 临时授权；start gate 的 JSONL 判断也要遵守同一优先级，
    # 否则可能在个人 subagent 默认下误放行，或在个人 inline 默认下误阻塞。
    effective.update(_read_route_prefs(repo_root))
    for target in ("implement", "check"):
        if target not in effective:
            mode = _resolve_existing_route(repo_root, task_ref, target)
            if mode:
                effective[target] = mode
    return effective


def _requested_check_depth(state: dict[str, Any]) -> str:
    """读取 run 请求的检查深度，旧状态或非法值按 full 兼容。"""
    value = state.get("check_depth")
    return str(value) if value in VALID_CHECK_DEPTHS else "full"


def _append_item_decision(
    item: dict[str, Any],
    event_type: str,
    summary: str,
    data: dict[str, Any] | None = None,
) -> None:
    """向当前队列项追加精简决策日志。"""
    log = item.get("decision_log")
    if not isinstance(log, list):
        log = []
    log.append({
        "at": _utc_now(),
        "type": event_type,
        "task": item.get("task"),
        "summary": summary,
        "data": data or {},
    })
    item["decision_log"] = log[-DECISION_LOG_LIMIT:]


def _decision_tail(state: dict[str, Any], limit: int = 8, include_data: bool = True) -> list[dict[str, Any]]:
    """返回 run 内最近的关键决策摘要。"""
    queue = state.get("queue") if isinstance(state.get("queue"), list) else []
    events: list[dict[str, Any]] = []
    for item in queue:
        if not isinstance(item, dict):
            continue
        log = item.get("decision_log")
        if isinstance(log, list):
            events.extend(event for event in log if isinstance(event, dict))
    tail = events[-limit:]
    if include_data:
        return tail
    compact: list[dict[str, Any]] = []
    for event in tail:
        compact.append({
            key: event.get(key)
            for key in ("at", "type", "task", "summary")
            if event.get(key) is not None
        })
    return compact


def _run_path(repo_root: Path, run_id: str) -> Path:
    """返回指定 auto run 状态文件。"""
    return _auto_dir(repo_root) / f"{run_id}.json"


def _run_paths(repo_root: Path) -> list[Path]:
    """返回按 run id 从新到旧排序的 auto run 文件。"""
    return sorted(_auto_dir(repo_root).glob("auto-*.json"), reverse=True)


def _new_run_id() -> str:
    """生成短小稳定的 run id。"""
    return "auto-" + datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")


def _normalize_task_ref(repo_root: Path, task_ref: str) -> str:
    """把任务引用规范化为 `.trellis/tasks/<dir>`。"""
    raw = task_ref.strip()
    path_obj = Path(raw)
    candidates: list[Path]
    if path_obj.is_absolute():
        candidates = [path_obj]
    elif raw.startswith(".trellis/"):
        candidates = [repo_root / raw]
    elif raw.startswith("tasks/"):
        candidates = [repo_root / ".trellis" / raw]
    else:
        candidates = [repo_root / ".trellis/tasks" / raw, repo_root / raw]

    for candidate in candidates:
        if candidate.is_dir():
            return _rel_path(repo_root, candidate)
    raise ValueError(f"任务不存在:{task_ref}")


def _task_dir(repo_root: Path, task_ref: str) -> Path:
    """把规范化任务引用解析为绝对路径。"""
    if task_ref.startswith(".trellis/"):
        return repo_root / task_ref
    return repo_root / ".trellis/tasks" / task_ref


def _load_task_json(repo_root: Path, task_ref: str) -> dict[str, Any]:
    """读取任务 task.json。"""
    return _read_json(_task_dir(repo_root, task_ref) / "task.json")


def _task_status(repo_root: Path, task_ref: str) -> str:
    """读取任务状态。"""
    return str(_load_task_json(repo_root, task_ref).get("status") or "unknown")


def _parents_outside_queue(repo_root: Path, queue: list[dict[str, Any]]) -> list[str]:
    """返回队列任务的父任务中未纳入本次队列的引用。

    父任务通常只负责范围、依赖顺序和集成复核，不进入 auto-loop 实现流水线，
    但它的归档必须排在全部子任务之后。runner 不会替它跑实现，也无法自动归档，
    所以要在 run 摘要里显式列出，避免无人值守跑完后父任务被静默漏掉。

    Args:
        repo_root: Trellis 项目根。
        queue: 本次 run 的队列项列表。

    Returns:
        规范化后的父任务引用列表，保持首次出现顺序且不含队列内任务。
    """
    queued = {str(item.get("task") or "") for item in queue}
    parents: list[str] = []
    for item in queue:
        parent = _load_task_json(repo_root, str(item.get("task") or "")).get("parent")
        if not isinstance(parent, str) or not parent.strip():
            continue
        try:
            ref = _normalize_task_ref(repo_root, parent)
        except ValueError:
            # 父任务可能已归档或被移除；缺失不阻断 run，也不进入待归档提示。
            continue
        if ref in queued or ref in parents:
            continue
        parents.append(ref)
    return parents


def _trim_text(value: Any, limit: int = TASK_PROGRESS_NOTE_LIMIT) -> str:
    """把 progress notes 中的长文本压到固定上限。"""
    text = str(value or "").replace("\n", " ").strip()
    if len(text) <= limit:
        return text
    return text[: limit - 1].rstrip() + "…"


def _progress_stable_fields(progress: dict[str, Any]) -> dict[str, Any]:
    """提取 progress 中除 updatedAt 外的稳定字段。"""
    return {
        "completedSteps": progress.get("completedSteps", []),
        "partialStep": progress.get("partialStep"),
        "nextStep": progress.get("nextStep"),
        "notes": progress.get("notes", ""),
    }


def _repo_commit_summaries(item: dict[str, Any], full: bool = False) -> list[str]:
    """返回队列项已记录仓库提交的紧凑摘要。

    Args:
        item: Auto-Loop 队列项。
        full: 是否保留完整提交哈希。

    Returns:
        `<repository>:<hash>` 摘要列表。
    """
    commits = item.get("commits") if isinstance(item.get("commits"), list) else []
    summaries: list[str] = []
    for entry in commits:
        if not isinstance(entry, dict):
            continue
        repository = str(entry.get("repository") or ".")
        commit = str(entry.get("commit") or "")
        if not commit:
            continue
        summaries.append(f"{repository}:{commit if full else commit[:7]}")
    return summaries


def _progress_for_completed(state: dict[str, Any], item: dict[str, Any]) -> dict[str, Any]:
    """构造 auto-loop 本地提交完成后的任务进度。"""
    commit = str(item.get("commit") or "")
    short_commit = commit[:7] if commit else "unknown"
    repo_commits = _repo_commit_summaries(item)
    completed_label = ", ".join(repo_commits) if repo_commits else short_commit
    full_repo_commits = _repo_commit_summaries(item, full=True)
    notes = "; ".join(
        part
        for part in (
            f"run_id={state.get('run_id')}",
            f"task={item.get('task')}",
            f"commit={commit or 'unknown'}",
            f"commits={','.join(full_repo_commits)}" if full_repo_commits else "",
            f"status={state.get('status')}",
        )
        if part
    )
    return {
        "updatedAt": _utc_now(),
        "completedSteps": [f"auto-loop: 本地提交完成 {completed_label}"],
        "partialStep": None,
        "nextStep": "auto-loop 已本地提交并置为本地完成态；需要用户显式运行 finish-work/archive 完成归档",
        "notes": _trim_text(notes),
    }


def _progress_for_blocked(state: dict[str, Any], item: dict[str, Any]) -> dict[str, Any]:
    """构造 auto-loop blocked 后的任务进度。"""
    blocked = item.get("blocked") if isinstance(item.get("blocked"), dict) else {}
    reason = str(blocked.get("reason") or "unknown")
    summary = str(blocked.get("summary") or "")
    task = str(item.get("task") or "")
    run_id = str(state.get("run_id") or "")
    repo_commits = _repo_commit_summaries(item)
    full_repo_commits = _repo_commit_summaries(item, full=True)
    command = f"python3 ./.trellis/scripts/auto_loop.py retry-blocked --run-id {run_id} --task {task}"
    notes = "; ".join(
        part
        for part in (
            f"run_id={run_id}",
            f"task={task}",
            f"status={state.get('status')}",
            f"reason={reason}",
            f"summary={summary}" if summary else "",
            f"commits={','.join(full_repo_commits)}" if full_repo_commits else "",
        )
        if part
    )
    return {
        "updatedAt": _utc_now(),
        "completedSteps": [f"auto-loop: 已保留本地提交 {', '.join(repo_commits)}"] if repo_commits else [],
        "partialStep": f"auto-loop blocked: {reason}",
        "nextStep": f"auto-loop 已阻断；确认后运行 {command}",
        "notes": _trim_text(notes),
    }


def _auto_progress_for_item(state: dict[str, Any], item: dict[str, Any]) -> dict[str, Any] | None:
    """按队列项终态生成可写入 task.json 的 progress。"""
    if item.get("status") == "completed":
        return _progress_for_completed(state, item)
    if item.get("status") == "blocked":
        return _progress_for_blocked(state, item)
    return None


def _apply_local_completion(item: dict[str, Any], task_data: dict[str, Any]) -> bool:
    """把已本地提交的队列项写入任务本地完成态，返回 task.json 是否发生变化。

    auto-loop 的终点是本地提交，但归档守卫要求 `status=completed` 且有 `completedAt`；
    若不在这里落地本地完成态，任务会卡在既无法 finish-work 也无法 archive 的状态。
    只允许 `in_progress -> completed` 这一个跃迁，并保留既有 `completedAt`，
    避免覆盖人工已确认的完成日期或把 planning/已完成任务重复改写。
    """
    if item.get("status") != "completed":
        return False
    if not item.get("commit") and not _repo_commit_summaries(item):
        return False
    changed = False
    if task_data.get("status") == "in_progress":
        task_data["status"] = "completed"
        changed = True
    if task_data.get("status") == "completed" and not task_data.get("completedAt"):
        task_data["completedAt"] = _utc_date()
        changed = True
    return changed


def _write_auto_task_progress(repo_root: Path, state: dict[str, Any], item: dict[str, Any]) -> dict[str, Any] | None:
    """把 auto-loop 下一步写入对应 task.json.progress 与本地完成态。"""
    progress = _auto_progress_for_item(state, item)
    if progress is None:
        return None
    task_ref = str(item.get("task") or "")
    task_json_path = _task_dir(repo_root, task_ref) / "task.json"
    task_data = _read_json(task_json_path)
    if not task_data:
        return {
            "task": task_ref,
            "reason": "invalid-task-json",
            "path": _rel_path(repo_root, task_json_path),
        }
    existing = task_data.get("progress")
    progress_changed = not (
        isinstance(existing, dict)
        and _progress_stable_fields(existing) == _progress_stable_fields(progress)
    )
    # 生命周期写入独立判定：progress 稳定字段未变时也可能仍缺本地完成态。
    lifecycle_changed = _apply_local_completion(item, task_data)
    if not progress_changed and not lifecycle_changed:
        return None
    if progress_changed:
        task_data["progress"] = progress
    task_data.pop("last_push_snapshot", None)
    _write_json(task_json_path, task_data)
    result = {
        "task": task_ref,
        "status": "written",
        "path": _rel_path(repo_root, task_json_path),
    }
    if lifecycle_changed:
        result["lifecycle"] = "completed"
    return result


def _sync_auto_task_progress(repo_root: Path, state: dict[str, Any]) -> None:
    """同步所有终态队列项的任务恢复提示。"""
    warnings: list[dict[str, Any]] = []
    for item in _queue_items(state):
        try:
            result = _write_auto_task_progress(repo_root, state, item)
        except OSError as exc:
            result = {
                "task": item.get("task"),
                "reason": "task-progress-write-failed",
                "message": str(exc),
            }
        if isinstance(result, dict) and result.get("reason"):
            warnings.append(result)
    if warnings:
        state["task_progress_warnings"] = warnings[-DECISION_LOG_LIMIT:]
    else:
        state.pop("task_progress_warnings", None)


def _has_real_jsonl_entries(path: Path, repo_root: Path) -> bool:
    """判断 JSONL 上下文清单是否至少有一条真实且存在的 file entry。"""
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return False

    for line in lines:
        if not line.strip():
            continue
        try:
            data = json.loads(line)
        except json.JSONDecodeError:
            continue
        file_path = data.get("file")
        if not isinstance(file_path, str) or not file_path.strip():
            continue
        if (repo_root / file_path).exists():
            return True
    return False


def _open_questions(text: str) -> dict[str, list[str]]:
    """按 checkbox 契约分类 PRD 中的 Open Questions 条目。"""
    lines = text.splitlines()
    collecting = False
    questions: dict[str, list[str]] = {"unchecked": [], "checked": [], "bare": []}
    for line in lines:
        if line.startswith("## "):
            collecting = line.strip().lower() == "## open questions"
            continue
        stripped = line.strip()
        if not collecting or not stripped.startswith("-"):
            continue
        lowered = stripped.lower()
        if lowered.startswith("- [ ]"):
            item = stripped[5:].strip()
            if item:
                questions["unchecked"].append(item)
        elif lowered.startswith("- [x]"):
            item = stripped[5:].strip()
            if item:
                questions["checked"].append(item)
        else:
            item = stripped[1:].strip()
            if item:
                questions["bare"].append(item)
    return questions


def _prd_sha256(content: bytes) -> str:
    """返回用于绑定语义复核结果的 PRD 内容摘要。"""
    return hashlib.sha256(content).hexdigest()


def _artifact_digest(paths: list[Path]) -> tuple[str, list[str]]:
    """返回按路径和内容绑定的稳定摘要，以及参与摘要的文件名。"""
    digest = hashlib.sha256()
    names: list[str] = []
    for path in paths:
        if not path.is_file():
            continue
        content = path.read_bytes()
        names.append(path.name)
        digest.update(path.name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(content)
        digest.update(b"\0")
    return digest.hexdigest(), names


def _task_artifact_hashes(repo_root: Path, item: dict[str, Any]) -> dict[str, str]:
    """返回当前任务 planning/handoff 文件的逐文件摘要。"""
    task_dir = _task_dir(repo_root, str(item.get("task") or ""))
    hashes: dict[str, str] = {}
    for name in ("prd.md", "design.md", "implement.md", "brief.md"):
        path = task_dir / name
        hashes[_baseline_key(".", _rel_path(repo_root, path))] = _file_sha256(path)
    return hashes


def _normalize_record_file(raw: str) -> str:
    """把 action 文件参数规范化为跨仓库唯一键。"""
    value = raw.strip()
    if "::" in value:
        repository, path = value.split("::", 1)
        repository = repository.strip() or "."
        return _baseline_key(repository, path.strip().removeprefix("./"))
    return _baseline_key(".", value.removeprefix("./"))


def _normalize_repository_root(raw: str) -> str:
    """把 record 中的仓库根规范化为 run baseline 使用的相对路径。"""
    value = raw.strip()
    if value == ".":
        return "."
    return value.removeprefix("./").rstrip("/")


def _planning_digest(task_dir: Path) -> tuple[str, list[str]]:
    """返回当前 planning authoritative artifacts 的内容摘要。"""
    return _artifact_digest([
        task_dir / "prd.md",
        task_dir / "design.md",
        task_dir / "implement.md",
    ])


def _brief_is_stale(task_dir: Path) -> tuple[bool, list[str]]:
    """判断 brief 是否早于任一 authoritative planning artifact。"""
    brief = task_dir / "brief.md"
    if not brief.is_file():
        return True, ["brief.md"]
    brief_mtime = brief.stat().st_mtime_ns
    newer = [
        path.name
        for path in (task_dir / "prd.md", task_dir / "design.md", task_dir / "implement.md")
        if path.is_file() and path.stat().st_mtime_ns > brief_mtime
    ]
    return bool(newer), newer


def _schema_version(state: dict[str, Any]) -> int:
    """读取 runtime schema；历史缺失值按 schema 1 兼容。"""
    value = state.get("schema_version", LEGACY_SCHEMA_VERSION)
    if value not in {LEGACY_SCHEMA_VERSION, SCHEMA_VERSION}:
        raise ValueError(f"不支持的 auto-loop schema_version:{value}")
    return int(value)


def _file_sha256(path: Path) -> str:
    """返回路径当前内容摘要，缺失路径使用稳定哨兵值。"""
    if not path.exists():
        return "missing"
    if path.is_symlink():
        return hashlib.sha256(os.readlink(path).encode("utf-8")).hexdigest()
    if not path.is_file():
        return "non-file"
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _git_output(repo: Path, *args: str) -> subprocess.CompletedProcess[bytes]:
    """执行返回原始字节的 Git 命令。"""
    return subprocess.run(
        ["git", *args],
        cwd=repo,
        check=False,
        capture_output=True,
    )


def _validated_repo_commits(
    repo_root: Path,
    state: dict[str, Any],
    raw_values: list[str],
) -> tuple[list[dict[str, str]], dict[str, Any] | None]:
    """校验并解析 `--repo-commit` 参数。

    Args:
        repo_root: 当前 Trellis 项目根目录。
        state: 当前 Auto-Loop runtime 状态。
        raw_values: 重复传入的 `<repository>::<commit>` 参数。

    Returns:
        规范化提交列表和可选错误对象。
    """
    repositories = state.get("repositories") if isinstance(state.get("repositories"), list) else []
    known = {
        str(entry.get("root") or ".")
        for entry in repositories
        if isinstance(entry, dict)
    }
    parsed: list[dict[str, str]] = []
    by_repository: dict[str, str] = {}
    for raw in raw_values:
        if "::" not in raw:
            return [], {
                "status": "error",
                "reason": "invalid-repo-commit",
                "message": f"repo-commit 必须是 <repository>::<commit>:{raw}",
            }
        repository_raw, commit_raw = raw.split("::", 1)
        repository = _normalize_repository_root(repository_raw)
        commit = commit_raw.strip()
        if not repository or not commit:
            return [], {
                "status": "error",
                "reason": "invalid-repo-commit",
                "message": f"repo-commit 的仓库和提交不能为空:{raw}",
            }
        if repository not in known:
            return [], {
                "status": "error",
                "reason": "repo-commit-repository-not-in-run",
                "repository": repository,
            }
        if not 7 <= len(commit) <= 64 or any(char not in "0123456789abcdefABCDEF" for char in commit):
            return [], {
                "status": "error",
                "reason": "invalid-repo-commit-hash",
                "repository": repository,
                "commit": commit,
            }
        repository_path = repo_root if repository == "." else repo_root / repository
        try:
            resolved = _git_output(repository_path, "rev-parse", "--verify", f"{commit}^{{commit}}")
        except OSError as exc:
            return [], {
                "status": "error",
                "reason": "repo-commit-repository-unreadable",
                "repository": repository,
                "commit": commit,
                "message": str(exc),
            }
        if resolved.returncode != 0:
            return [], {
                "status": "error",
                "reason": "repo-commit-not-a-commit",
                "repository": repository,
                "commit": commit,
                "message": resolved.stderr.decode("utf-8", errors="replace").strip(),
            }
        full_commit = resolved.stdout.decode("utf-8", errors="replace").strip()
        previous = by_repository.get(repository)
        if previous and previous != full_commit:
            return [], {
                "status": "error",
                "reason": "repo-commit-conflict",
                "repository": repository,
                "commits": [previous, full_commit],
            }
        if previous == full_commit:
            continue
        by_repository[repository] = full_commit
        parsed.append({"repository": repository, "commit": full_commit})
    return parsed, None


def _merge_repo_commits(
    item: dict[str, Any],
    additions: list[dict[str, str]],
) -> dict[str, Any] | None:
    """把新提交幂等合并进队列项，并拒绝同仓冲突哈希。"""
    existing = item.get("commits") if isinstance(item.get("commits"), list) else []
    merged: list[dict[str, str]] = []
    by_repository: dict[str, str] = {}
    for entry in [*existing, *additions]:
        if not isinstance(entry, dict):
            continue
        repository = str(entry.get("repository") or ".")
        commit = str(entry.get("commit") or "")
        if not commit:
            continue
        previous = by_repository.get(repository)
        if previous and previous != commit:
            return {
                "status": "error",
                "reason": "repo-commit-conflict",
                "repository": repository,
                "commits": [previous, commit],
            }
        if previous == commit:
            continue
        by_repository[repository] = commit
        merged.append({"repository": repository, "commit": commit})
    if merged:
        item["commits"] = merged
    return None


def _resolved_primary_repo_commit(
    item: dict[str, Any],
    raw_commit: str | None,
) -> tuple[str | None, dict[str, Any] | None]:
    """把多仓兼容主提交解析为 `commits[]` 中的完整哈希。

    Args:
        item: Auto-Loop 队列项。
        raw_commit: `record --commit` 传入的兼容主提交。

    Returns:
        完整主提交哈希和可选错误对象；单仓旧协议不参与该校验。
    """
    commits = item.get("commits") if isinstance(item.get("commits"), list) else []
    if not commits or not raw_commit:
        return raw_commit, None
    candidate = raw_commit.strip()
    if not 7 <= len(candidate) <= 64 or any(char not in "0123456789abcdefABCDEF" for char in candidate):
        return None, {
            "status": "error",
            "reason": "repo-commit-primary-mismatch",
            "commit": candidate,
            "message": "多仓兼容主 commit 必须是 commits[] 中提交的哈希或唯一前缀",
        }
    matches = {
        str(entry.get("commit") or "")
        for entry in commits
        if isinstance(entry, dict)
        and str(entry.get("commit") or "").lower().startswith(candidate.lower())
    }
    matches.discard("")
    if len(matches) != 1:
        return None, {
            "status": "error",
            "reason": "repo-commit-primary-mismatch",
            "commit": candidate,
            "commits": sorted(matches),
            "message": "多仓兼容主 commit 必须唯一匹配 commits[] 中的已验证提交",
        }
    return next(iter(matches)), None


def _integration_in_progress(repo: Path) -> list[str]:
    """返回当前仓库未完成的 Git 集成状态名称。"""
    result = _git_output(repo, "rev-parse", "--git-path", ".")
    if result.returncode != 0:
        return []
    git_dir = Path(result.stdout.decode("utf-8", errors="replace").strip())
    if not git_dir.is_absolute():
        git_dir = repo / git_dir
    markers = {
        "MERGE_HEAD": "merge",
        "rebase-merge": "rebase",
        "rebase-apply": "rebase",
        "CHERRY_PICK_HEAD": "cherry-pick",
        "REVERT_HEAD": "revert",
    }
    return sorted({name for marker, name in markers.items() if (git_dir / marker).exists()})


def _git_repositories(repo_root: Path) -> list[Path]:
    """返回主仓、递归子模块和配置独立 Git package。"""
    try:
        return discover_git_repositories(repo_root)
    except GitEvidenceError as error:
        # auto-loop 的纯状态机测试和只读恢复场景允许临时目录不是 Git 仓库；
        # 真正开始任务时，后续 task/Git 门禁仍会给出明确阻断。
        if error.reason == "git-root-unreadable":
            return []
        raise


def _parse_porcelain_z(repo: Path, payload: bytes) -> list[dict[str, str]]:
    """解析 `git status --porcelain=v1 -z` 输出。"""
    _ = repo
    return [
        {
            "xy": entry["status"],
            "path": entry["path"],
            "original_path": entry.get("originalPath", ""),
        }
        for entry in parse_porcelain_z(payload)
    ]


def _capture_git_baseline(repo_root: Path) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    """捕获 Git dirty baseline，并返回全局阻断信息。"""
    repositories: list[dict[str, Any]] = []
    try:
        git_repositories = _git_repositories(repo_root)
    except GitEvidenceError as error:
        return [], {
            "reason": error.reason,
            "message": str(error),
            **error.details,
        }
    for repo in git_repositories:
        status = _git_output(repo, "status", "--porcelain=v1", "-z", "--untracked-files=all")
        if status.returncode != 0:
            return [], {
                "reason": "git-status-unreadable",
                "message": status.stderr.decode("utf-8", errors="replace").strip(),
                "repository": _rel_path(repo_root, repo),
            }
        entries = _parse_porcelain_z(repo, status.stdout)
        conflicts = [entry for entry in entries if "U" in entry["xy"] or entry["xy"] in {"AA", "DD"}]
        staged = [entry for entry in entries if entry["xy"][0] not in {" ", "?"}]
        integration = _integration_in_progress(repo)
        if conflicts or staged or integration:
            return [], {
                "reason": "git-global-safety-block",
                "message": "存在 staged、冲突或未完成 Git 集成",
                "repository": _rel_path(repo_root, repo),
                "staged": staged,
                "conflicts": conflicts,
                "integration": integration,
            }
        dirty: list[dict[str, str]] = []
        for entry in entries:
            path = entry["path"]
            dirty.append({
                "path": path,
                "xy": entry["xy"],
                "sha256": _file_sha256(repo / path),
            })
        repositories.append({
            "root": _rel_path(repo_root, repo),
            "dirty": dirty,
            "owned_dirty": [],
            "protected_retained": [],
        })
    return repositories, None


def _dirty_entries(state: dict[str, Any]) -> list[dict[str, str]]:
    """展平 manifest 中尚未分类的 dirty baseline。"""
    entries: list[dict[str, str]] = []
    repositories = state.get("repositories") if isinstance(state.get("repositories"), list) else []
    for repository in repositories:
        if not isinstance(repository, dict):
            continue
        root = str(repository.get("root") or ".")
        dirty = repository.get("dirty") if isinstance(repository.get("dirty"), list) else []
        for entry in dirty:
            if isinstance(entry, dict):
                entries.append({
                    "repository": root,
                    "path": str(entry.get("path") or ""),
                    "xy": str(entry.get("xy") or ""),
                    "sha256": str(entry.get("sha256") or ""),
                })
    return entries


def _batch_open_questions(repo_root: Path, state: dict[str, Any]) -> list[dict[str, Any]]:
    """收集整个队列中由人工拥有的 Open Questions。"""
    questions: list[dict[str, Any]] = []
    for item in _queue_items(state):
        if item.get("status") == "blocked":
            continue
        task_ref = str(item.get("task") or "")
        if _task_status(repo_root, task_ref) != "planning":
            continue
        prd = _task_dir(repo_root, task_ref) / "prd.md"
        if not prd.is_file():
            continue
        text = prd.read_text(encoding="utf-8")
        parsed = _open_questions(text)
        for kind in ("unchecked", "bare"):
            for question in parsed[kind]:
                line = next(
                    (number for number, raw in enumerate(text.splitlines(), 1) if question in raw),
                    None,
                )
                questions.append({
                    "task": task_ref,
                    "question": question,
                    "kind": kind,
                    "source": f"{_rel_path(repo_root, prd)}:{line or 1}",
                    "prd_sha256": _prd_sha256(text.encode("utf-8")),
                })
    return questions


def _manifest_payload(state: dict[str, Any], revision: int) -> dict[str, Any]:
    """构造不含自身摘要的确定性 manifest revision。"""
    queue = _queue_items(state)
    return {
        "revision": revision,
        "created_at": _utc_now(),
        "authorization": state.get("authorization"),
        "original_order": state.get("original_order"),
        "execution_order": [item.get("task") for item in queue],
        "dependencies": {
            str(item.get("task")): item.get("depends_on", [])
            for item in queue
            if item.get("depends_on")
        },
        "route_authorization": state.get("route_authorization"),
        "check_depth": _requested_check_depth(state),
        "profile": state.get("profile"),
        "repositories": state.get("repositories", []),
        "tasks": [
            {
                "task": item.get("task"),
                "task_status": item.get("task_status"),
                "prepare_status": item.get("prepare_status"),
                "planning_sha256": item.get("planning_sha256", ""),
                "handoff_sha256": item.get("handoff_sha256", ""),
                "owned_dirty": item.get("owned_dirty", []),
                "decision_count": item.get("decision_count", 0),
                "decision_ids": item.get("decision_ids", []),
            }
            for item in queue
        ],
    }


def _append_manifest_revision(
    state: dict[str, Any],
    decision_id: str | None = None,
    change_source: str | None = None,
    files: list[str] | None = None,
) -> dict[str, Any]:
    """追加并激活一个 run manifest revision。"""
    revisions = state.get("manifest_revisions")
    if not isinstance(revisions, list):
        revisions = []
    try:
        next_revision = int(state.get("manifest_revision") or 0) + 1
    except (TypeError, ValueError):
        next_revision = len(revisions) + 1
    payload = _manifest_payload(state, next_revision)
    if decision_id:
        payload["decision_id"] = decision_id
    if change_source:
        payload["change_source"] = change_source
    if files:
        payload["files"] = sorted(files)
    encoded = json.dumps(payload, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    payload["sha256"] = hashlib.sha256(encoded.encode("utf-8")).hexdigest()
    revisions.append(payload)
    state["manifest_revisions"] = revisions
    state["manifest_revision"] = payload["revision"]
    state["manifest_sha256"] = payload["sha256"]
    for item in _queue_items(state):
        item["manifest_revision"] = payload["revision"]
    return payload


def _stable_topological_order(queue: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """根据显式依赖执行稳定拓扑排序。"""
    by_task = {str(item.get("task")): item for item in queue}
    original_index = {task: index for index, task in enumerate(by_task)}
    indegree = {task: 0 for task in by_task}
    followers: dict[str, list[str]] = {task: [] for task in by_task}
    for task, item in by_task.items():
        dependencies = item.get("depends_on") if isinstance(item.get("depends_on"), list) else []
        for dependency in dependencies:
            if dependency == task:
                raise ValueError(f"任务不能依赖自身:{task}")
            if dependency not in by_task:
                raise ValueError(f"依赖不在当前队列:{task} -> {dependency}")
            indegree[task] += 1
            followers[dependency].append(task)

    ready = sorted((task for task, value in indegree.items() if value == 0), key=original_index.get)
    ordered: list[dict[str, Any]] = []
    while ready:
        task = ready.pop(0)
        ordered.append(by_task[task])
        for follower in sorted(followers[task], key=original_index.get):
            indegree[follower] -= 1
            if indegree[follower] == 0:
                ready.append(follower)
                ready.sort(key=original_index.get)
    if len(ordered) != len(queue):
        cycle = sorted(task for task, value in indegree.items() if value > 0)
        raise ValueError(f"任务依赖存在循环:{','.join(cycle)}")
    return ordered


def _start_gate(
    repo_root: Path,
    task_ref: str,
    item: dict[str, Any],
    route_authorization: dict[str, Any] | None = None,
) -> tuple[str, dict[str, Any]]:
    """检查 planning -> start 前置条件。"""
    task_dir = _task_dir(repo_root, task_ref)
    prd = task_dir / "prd.md"
    if not prd.is_file():
        return "blocked", {"reason": "missing-prd", "message": "缺少 prd.md"}

    try:
        prd_content = prd.read_bytes()
    except OSError as exc:
        return "blocked", {
            "reason": "missing-prd",
            "message": f"无法读取 prd.md:{exc}",
        }
    questions = _open_questions(prd_content.decode("utf-8"))
    if questions["unchecked"]:
        return "blocked", {
            "reason": "open-questions",
            "message": "PRD 仍有阻塞性 Open Questions",
            "questions": questions["unchecked"],
        }
    if questions["bare"]:
        prd_hash = _prd_sha256(prd_content)
        review = item.get("open_questions_review")
        if isinstance(review, dict) and review.get("prd_sha256") == prd_hash:
            verdict = review.get("verdict")
            if verdict == "resolved":
                pass
            elif verdict == "blocking":
                return "blocked", {
                    "reason": "open-questions",
                    "message": "历史 Open Questions 经语义复核仍有未决事项",
                    "questions": questions["bare"],
                    "review": review,
                }
            else:
                return "blocked", {
                    "reason": "open-questions-ambiguous",
                    "message": "历史 Open Questions 语义复核无法确定，需人工收敛",
                    "questions": questions["bare"],
                    "review": review,
                }
        else:
            return "action", {
                "action": "review_open_questions",
                "message": "历史 PRD 使用无状态列表，需由 AI 复核是否仍有开放问题",
                "questions": questions["bare"],
                "prd_sha256": prd_hash,
            }

    design = task_dir / "design.md"
    implement = task_dir / "implement.md"
    if design.exists() != implement.exists():
        missing = "implement.md" if design.exists() else "design.md"
        return "blocked", {"reason": "incomplete-complex-artifacts", "message": f"复杂任务缺少 {missing}"}

    auth = route_authorization if isinstance(route_authorization, dict) else {}
    needs_implement_context = auth.get("implement") != "inline"
    needs_check_context = auth.get("check") != "check-all-inline"
    if needs_implement_context and not _has_real_jsonl_entries(task_dir / "implement.jsonl", repo_root):
        return "blocked", {
            "reason": "missing-implement-context",
            "message": "implement.jsonl 未 curated（当前 route 可能需要 sub-agent context）",
        }
    if needs_check_context and not _has_real_jsonl_entries(task_dir / "check.jsonl", repo_root):
        return "blocked", {
            "reason": "missing-check-context",
            "message": "check.jsonl 未 curated（当前 route 可能需要 sub-agent context）",
        }

    try:
        planning_hash, planning_files = _planning_digest(task_dir)
    except OSError as exc:
        return "blocked", {
            "reason": "planning-readiness",
            "message": f"无法读取 planning artifacts:{exc}",
        }
    readiness = item.get("planning_readiness_review")
    if not isinstance(readiness, dict) or readiness.get("planning_sha256") != planning_hash:
        return "action", {
            "action": "review_planning_readiness",
            "message": "需要语义复核 planning artifacts 是否达到可实现状态",
            "planning_sha256": planning_hash,
            "planning_files": planning_files,
        }
    verdict = readiness.get("verdict")
    if verdict == "blocking":
        return "blocked", {
            "reason": "planning-readiness",
            "message": "planning artifacts 经语义复核仍不具备实现条件",
            "review": readiness,
        }
    if verdict != "ready":
        return "blocked", {
            "reason": "planning-readiness-ambiguous",
            "message": "planning artifacts 的实现就绪性无法确定，需继续收敛",
            "review": readiness,
        }

    try:
        brief_stale, newer_sources = _brief_is_stale(task_dir)
    except OSError as exc:
        return "blocked", {
            "reason": "planning-readiness",
            "message": f"无法检查 brief freshness:{exc}",
        }
    if brief_stale:
        return "action", {
            "action": "refresh_brief",
            "message": "brief.md 缺失或过期，需先用 trellis-task-brief 刷新并展示任务摘要",
            "planning_sha256": planning_hash,
            "newer_sources": newer_sources,
        }

    try:
        handoff_hash, handoff_files = _artifact_digest([
            task_dir / "prd.md",
            task_dir / "design.md",
            task_dir / "implement.md",
            task_dir / "brief.md",
        ])
    except OSError as exc:
        return "blocked", {
            "reason": "planning-readiness",
            "message": f"无法读取 brief handoff artifacts:{exc}",
        }
    confirmation = item.get("brief_confirmation")
    if not isinstance(confirmation, dict) or confirmation.get("handoff_sha256") != handoff_hash:
        return "action", {
            "action": "confirm_brief",
            "message": "展示当前 brief.md，并等待用户显式确认 planning artifacts 与 brief",
            "handoff_sha256": handoff_hash,
            "handoff_files": handoff_files,
        }

    return "ok", {"message": "start gate satisfied"}


def _current_session_key(repo_root: Path) -> str | None:
    """尽力解析当前 session key，用于把 auto run 绑定给 route helper。"""
    override = os.environ.get("TRELLIS_CONTEXT_ID")
    if override:
        return override.strip() or None

    result = subprocess.run(
        ["python3", str(repo_root / ".trellis/scripts/task.py"), "current", "--source"],
        cwd=repo_root,
        check=False,
        text=True,
        capture_output=True,
    )
    for line in result.stdout.splitlines():
        if line.startswith("Source: "):
            source = line.split(": ", 1)[1].strip()
            if source.startswith(("session:", "session-fallback:")):
                return source.split(":", 1)[1].strip()
    return None


def _link_session_run(repo_root: Path, run_id: str) -> None:
    """把当前 auto run 写入 session runtime，方便 route_state.py 精确恢复。"""
    context_key = _current_session_key(repo_root)
    if not context_key:
        return
    path = repo_root / ".trellis/.runtime/sessions" / f"{context_key}.json"
    result = _read_json_result(path)
    if result["status"] in {"corrupt", "io_error"}:
        return
    context = result["data"] if isinstance(result.get("data"), dict) else {}
    context.setdefault("platform", context_key.split("_", 1)[0] if "_" in context_key else "session")
    context["last_seen_at"] = _utc_now()
    context["current_auto_run"] = run_id
    _write_json(path, context)


def _load_run_state(path: Path) -> dict[str, Any]:
    """读取单个 run；损坏或 I/O 错误时保留证据并报错。"""
    result = _read_json_result(path)
    if result["status"] == "missing":
        raise ValueError(f"auto run 不存在:{path.stem}")
    if result["status"] != "ok":
        raise ValueError(f"auto run 状态 {result['status']}:{path}:{result.get('error') or ''}")
    state = result["data"]
    _schema_version(state)
    return state


def _healthy_running_runs(repo_root: Path) -> list[tuple[Path, dict[str, Any]]]:
    """返回所有可解析且仍处于活动阶段的 run。"""
    running: list[tuple[Path, dict[str, Any]]] = []
    for path in _run_paths(repo_root):
        result = _read_json_result(path)
        state = result.get("data")
        if result["status"] == "ok" and isinstance(state, dict) and state.get("status") in ACTIVE_RUN_STATUSES:
            try:
                _schema_version(state)
            except ValueError:
                continue
            running.append((path, state))
    return running


def _load_current_state(repo_root: Path, run_id: str | None = None) -> tuple[Path, dict[str, Any]]:
    """加载指定或当前 auto run 状态。"""
    if run_id:
        path = _run_path(repo_root, run_id)
        return path, _load_run_state(path)

    pointer_path = _current_pointer(repo_root)
    pointer_result = _read_json_result(pointer_path)
    if pointer_result["status"] in {"corrupt", "io_error"}:
        running = _healthy_running_runs(repo_root)
        if len(running) == 1:
            path, state = running[0]
            _write_pointer(repo_root, str(state.get("run_id") or path.stem))
            return path, state
        raise ValueError(
            f"auto current pointer {pointer_result['status']}:{pointer_path}:"
            f"{pointer_result.get('error') or ''}"
        )
    pointer = pointer_result["data"] if isinstance(pointer_result.get("data"), dict) else {}
    current = pointer.get("run_id")
    if isinstance(current, str) and current:
        path = _run_path(repo_root, current)
        run_result = _read_json_result(path)
        if run_result["status"] in {"corrupt", "io_error"}:
            raise ValueError(
                f"current auto run {run_result['status']}:{path}:{run_result.get('error') or ''}"
            )
        state = run_result["data"] if isinstance(run_result.get("data"), dict) else {}
        if state and state.get("status") in ACTIVE_RUN_STATUSES:
            return path, state
        if state and state.get("status") in TERMINAL_RUN_STATUSES:
            _clear_pointer_if_current(repo_root, current)
        elif state:
            return path, state

    running = _healthy_running_runs(repo_root)
    if len(running) == 1:
        return running[0]
    if len(running) > 1:
        run_ids = ", ".join(str(state.get("run_id") or path.stem) for path, state in running[:8])
        raise ValueError(f"存在多个 running auto run，请指定 --run-id。候选:{run_ids}")
    raise ValueError("没有可恢复的唯一 auto run")


def _load_blocked_state(repo_root: Path, run_id: str | None = None) -> tuple[Path, dict[str, Any]]:
    """加载可重试的 blocked auto run。"""
    if run_id:
        path, state = _load_current_state(repo_root, run_id)
        if _blocked_items(state):
            return path, state
        raise ValueError(f"auto run 没有 blocked 队列项:{run_id}")

    try:
        path, state = _load_current_state(repo_root)
        if _blocked_items(state):
            return path, state
    except ValueError:
        pass

    blocked_runs: list[tuple[Path, dict[str, Any]]] = []
    for path in _run_paths(repo_root):
        state = _read_json(path)
        if state and _blocked_items(state):
            blocked_runs.append((path, state))
    if len(blocked_runs) == 1:
        return blocked_runs[0]
    if not blocked_runs:
        raise ValueError("没有可重试的 blocked auto run")
    run_ids = ", ".join(str(state.get("run_id") or path.stem) for path, state in blocked_runs[:8])
    raise ValueError(f"存在多个 blocked auto run，请指定 --run-id。候选:{run_ids}")


def _recent_run_summaries(repo_root: Path, limit: int = 8) -> list[dict[str, Any]]:
    """返回最近 auto run 的轻量状态列表。"""
    runs: list[dict[str, Any]] = []
    for path in _run_paths(repo_root)[:limit]:
        result = _read_json_result(path)
        if result["status"] != "ok":
            runs.append({
                "run_id": path.stem,
                "path": _rel_path(repo_root, path),
                "run_status": result["status"],
                "error": result.get("error"),
            })
            continue
        state = result["data"]
        counts = _queue_counts(state)
        current = _current_queue_item(state)
        # run 终态会清除 pointer，之后 status 只走本列表；归档待办必须在这里也可见。
        handoff = _pending_archive_handoff(state)
        entry = {
            "run_id": state.get("run_id") or path.stem,
            "path": _rel_path(repo_root, path),
            "run_status": state.get("status"),
            "profile": state.get("profile"),
            "check_depth": _requested_check_depth(state),
            "updated_at": state.get("updated_at"),
            "completed": counts["completed"],
            "blocked": counts["blocked"],
            "remaining": counts["remaining"],
            "current_task": current.get("task") if current else None,
            "next_step": current.get("current_step") if current else "done",
        }
        if handoff:
            entry["pending_archive"] = handoff
        runs.append(entry)
    return runs


def _write_state(path: Path, state: dict[str, Any]) -> None:
    """刷新 auto run 状态和更新时间。"""
    state["updated_at"] = _utc_now()
    state.pop("resume_capsule", None)
    _compact_manifest_history(path, state)
    repo_root = _repo_root_from_run_path(path)
    if repo_root is not None:
        _sync_auto_task_progress(repo_root, state)
    _write_json(path, state)


def _write_pointer(repo_root: Path, run_id: str) -> None:
    """写入当前 auto run 指针。"""
    _write_json(_current_pointer(repo_root), {"run_id": run_id, "updated_at": _utc_now()})


def _clear_pointer_if_current(repo_root: Path, run_id: str | None) -> None:
    """当 current 指针仍指向本 run 时删除，避免 stale pointer 影响后续恢复。"""
    if not run_id:
        return
    pointer_path = _current_pointer(repo_root)
    result = _read_json_result(pointer_path)
    if result["status"] != "ok":
        return
    pointer = result["data"]
    if pointer.get("run_id") != run_id:
        return
    try:
        pointer_path.unlink()
    except FileNotFoundError:
        pass


def _pending_archive_handoff(state: dict[str, Any]) -> dict[str, Any] | None:
    """返回本次 run 遗留的归档待办。

    runner 只推进到本地提交与本地完成态，归档必须由用户显式执行；
    父任务不进入实现流水线，但必须排在全部子任务归档之后单独收尾。
    没有任何待办时返回 None，避免在摘要里留下空字段。

    Args:
        state: Auto run 状态。

    Returns:
        含待归档任务与队列外父任务的交接信息，或 None。
    """
    awaiting = [
        str(item.get("task") or "")
        for item in _queue_items(state)
        if item.get("status") == "completed"
    ]
    parents = [str(ref) for ref in (state.get("parent_tasks_outside_queue") or []) if str(ref)]
    if not awaiting and not parents:
        return None
    handoff: dict[str, Any] = {
        "note": "auto-loop 已写入本地 completed+completedAt；归档仍需用户显式运行 finish-work",
        "tasks_awaiting_archive": awaiting,
    }
    if parents:
        handoff["parent_tasks_outside_queue"] = parents
        handoff["parent_note"] = "父任务未纳入队列；需在全部子任务归档后单独 finish-work"
    return handoff


def _resume_capsule(state: dict[str, Any]) -> dict[str, Any]:
    """生成短小的人类可读恢复摘要。"""
    queue = _queue_items(state)
    current = _current_queue_item(state)
    auto_completed = [item.get("task") for item in queue if item.get("status") == "completed"]
    blocked = [item.get("task") for item in queue if item.get("status") == "blocked"]
    recorded_commits = [
        {"task": item.get("task"), "commits": _repo_commit_summaries(item)}
        for item in queue
        if _repo_commit_summaries(item)
    ]
    counts = _queue_counts(state)
    handoff = _pending_archive_handoff(state)
    return {
        "run_id": state.get("run_id"),
        "status": state.get("status"),
        "check_depth": _requested_check_depth(state),
        "current_task": current.get("task") if current else None,
        "next_step": current.get("current_step") if current else "done",
        "completed": counts["completed"],
        "blocked": counts["blocked"],
        "remaining": counts["remaining"],
        "auto_completed_tasks": auto_completed,
        "recorded_commits": recorded_commits,
        "task_lifecycle_note": handoff["note"] if handoff else None,
        "pending_archive": handoff,
        "blocked_tasks": blocked,
    }


def _terminal_status(queue: list[Any], schema_version: int = SCHEMA_VERSION) -> str:
    """根据队列终态区分全完成和带阻塞结束。"""
    has_blocked = any(isinstance(item, dict) and item.get("status") == "blocked" for item in queue)
    if schema_version == LEGACY_SCHEMA_VERSION:
        return "blocked" if has_blocked else "completed"
    return "completed_with_blocked" if has_blocked else "completed"


def _queue_items(state: dict[str, Any]) -> list[dict[str, Any]]:
    """返回合法队列项。"""
    queue = state.get("queue") if isinstance(state.get("queue"), list) else []
    return [item for item in queue if isinstance(item, dict)]


def _queue_counts(state: dict[str, Any]) -> dict[str, int]:
    """返回队列状态计数。"""
    queue = _queue_items(state)
    return {
        "total": len(queue),
        "completed": sum(1 for item in queue if item.get("status") == "completed"),
        "blocked": sum(1 for item in queue if item.get("status") == "blocked"),
        "remaining": sum(1 for item in queue if item.get("status") in {"pending", "running"}),
    }


def _current_queue_item(state: dict[str, Any]) -> dict[str, Any] | None:
    """返回当前待处理队列项。"""
    for item in _queue_items(state):
        if item.get("status") in {"pending", "running"}:
            return item
    return None


def _blocked_items(state: dict[str, Any]) -> list[dict[str, Any]]:
    """返回 blocked 队列项。"""
    return [item for item in _queue_items(state) if item.get("status") == "blocked"]


def _outstanding_action(state: dict[str, Any]) -> dict[str, Any] | None:
    """返回当前等待 record 回写的 action。"""
    for item in _queue_items(state):
        if item.get("status") != "running":
            continue
        last_action = item.get("last_action")
        if isinstance(last_action, dict):
            return {
                "task": item.get("task"),
                "action": last_action.get("action"),
                "current_step": last_action.get("current_step"),
                "issued_at": last_action.get("issued_at"),
            }
        return None
    return None


def _completed_task_summaries(
    state: dict[str, Any],
    include_detail: bool = False,
) -> list[dict[str, Any]]:
    """返回已完成任务的紧凑摘要。"""
    completed: list[dict[str, Any]] = []
    for item in _queue_items(state):
        if item.get("status") != "completed":
            continue
        task = {"task": item.get("task")}
        if item.get("commit"):
            task["commit"] = item.get("commit")
        repo_commits = item.get("commits") if isinstance(item.get("commits"), list) else []
        if repo_commits:
            task["commits"] = repo_commits if include_detail else _repo_commit_summaries(item)
        completed.append(task)
    return completed


def _blocked_task_summaries(state: dict[str, Any], include_detail: bool = False) -> list[dict[str, Any]]:
    """返回 blocked 任务摘要，默认不带 detail。"""
    blocked_tasks: list[dict[str, Any]] = []
    for item in _blocked_items(state):
        blocked = item.get("blocked")
        blocked_data = blocked if isinstance(blocked, dict) else {}
        task = {
            "task": item.get("task"),
            "reason": blocked_data.get("reason"),
            "summary": blocked_data.get("summary"),
            "blocked_at": blocked_data.get("blocked_at"),
        }
        if include_detail:
            task["detail"] = blocked_data.get("detail") or {}
            if isinstance(item.get("commits"), list) and item.get("commits"):
                task["commits"] = item.get("commits")
        elif _repo_commit_summaries(item):
            task["commits"] = _repo_commit_summaries(item)
        blocked_tasks.append(task)
    return blocked_tasks


def _pending_task_summaries(state: dict[str, Any], include_status: bool = False) -> list[dict[str, Any]]:
    """返回未完成任务摘要。"""
    pending: list[dict[str, Any]] = []
    for item in _queue_items(state):
        if item.get("status") not in {"pending", "running"}:
            continue
        task = {
            "task": item.get("task"),
            "status": item.get("status"),
            "current_step": item.get("current_step"),
        }
        if include_status:
            task["last_failure"] = item.get("last_failure")
            task["attempts"] = item.get("attempts")
            if isinstance(item.get("commits"), list) and item.get("commits"):
                task["commits"] = item.get("commits")
        elif _repo_commit_summaries(item):
            task["commits"] = _repo_commit_summaries(item)
        pending.append(task)
    return pending


def _compact_summary(state: dict[str, Any]) -> dict[str, Any]:
    """返回默认给 agent 消费的紧凑状态摘要。"""
    current = _current_queue_item(state)
    completed_tasks = _completed_task_summaries(state)
    summary = {
        "schema_version": state.get("schema_version", LEGACY_SCHEMA_VERSION),
        "run_id": state.get("run_id"),
        "run_status": state.get("status"),
        "profile": state.get("profile"),
        "check_depth": _requested_check_depth(state),
        "current_index": state.get("current_index"),
        "current_task": current.get("task") if current else None,
        "next_step": current.get("current_step") if current else "done",
        "outstanding_action": _outstanding_action(state),
        "queue_counts": _queue_counts(state),
        "completed_tasks": completed_tasks,
        "blocked_tasks": _blocked_task_summaries(state),
        "pending_tasks": _pending_task_summaries(state),
        "recent_decisions": _decision_tail(state, 3, include_data=False),
    }
    if _schema_version(state) == SCHEMA_VERSION:
        summary.update({
            "prepare_action": (
                state.get("prepare_action", {}).get("action")
                if isinstance(state.get("prepare_action"), dict)
                else None
            ),
            "manifest_revision": state.get("manifest_revision"),
            "manifest_sha256": state.get("manifest_sha256"),
            "decision_count": sum(int(item.get("decision_count") or 0) for item in _queue_items(state)),
            "queue_reordered": bool(state.get("queue_reordered")),
        })
    handoff = _pending_archive_handoff(state)
    if handoff:
        summary["task_lifecycle_note"] = handoff["note"]
        summary["pending_archive"] = handoff
    return summary


def _summary(state: dict[str, Any]) -> dict[str, Any]:
    """返回 verbose 诊断状态摘要。"""
    summary = _compact_summary(state)
    summary.update({
        "completed_tasks": _completed_task_summaries(state, include_detail=True),
        "blocked_tasks": _blocked_task_summaries(state, include_detail=True),
        "pending_tasks": _pending_task_summaries(state, include_status=True),
        "recent_decisions": _decision_tail(state, DECISION_LOG_LIMIT, include_data=True),
        "resume_capsule": _resume_capsule(state),
    })
    if _schema_version(state) == SCHEMA_VERSION:
        summary.update({
            "authorization": state.get("authorization"),
            "original_order": state.get("original_order"),
            "queue_reordered_detail": state.get("queue_reordered"),
            "repositories": state.get("repositories"),
            "protected_drifts": state.get("protected_drifts", []),
            "manifest_audit_path": state.get("manifest_audit_path"),
            "manifest_tail": state.get("manifest_tail", []),
            "global_block": state.get("global_block"),
            "task_progress_warnings": state.get("task_progress_warnings", []),
        })
    return summary


def _format_summary(state: dict[str, Any], args: argparse.Namespace | None = None) -> dict[str, Any]:
    """根据 --verbose 返回紧凑或详细摘要。"""
    if args is not None and getattr(args, "verbose", False):
        return _summary(state)
    return _compact_summary(state)


def _make_item(repo_root: Path, task_ref: str) -> dict[str, Any]:
    """构造队列任务项。"""
    normalized = _normalize_task_ref(repo_root, task_ref)
    status = _task_status(repo_root, normalized)
    step = "start_task" if status == "planning" else "implement"
    return {
        "task": normalized,
        "status": "pending",
        "task_status": status,
        "current_step": step,
        "prepare_status": "pending",
        "planning_attempts": 0,
        "planning_sha256": "",
        "handoff_sha256": "",
        "manifest_revision": 0,
        "depends_on": [],
        "blocked_by": [],
        "owned_dirty": [],
        "decision_count": 0,
        "attempts": {"fix_recheck": 0, "artifact_reconcile": 0, "commit_repair": 0},
        "last_failure": None,
        "last_check": None,
        "last_action": None,
        "commit": None,
        "blocked": None,
        "decision_log": [],
        "updated_at": _utc_now(),
    }


def _action(action: str, item: dict[str, Any], extra: dict[str, Any] | None = None) -> dict[str, Any]:
    """构造下一步动作对象。"""
    task = item.get("task")
    base: dict[str, Any] = {
        "status": "action",
        "action": action,
        "task": task,
        "current_step": item.get("current_step"),
    }
    if action == "classify_dirty_baseline":
        base["instruction"] = (
            "把 baseline 中每个 dirty path 精确分类为某个队列任务的 owned_dirty，或全局 "
            "protected_retained；然后 record 同名 action。"
        )
    elif action == "resolve_open_questions":
        base["instruction"] = (
            "按 trellis-brainstorm 一次引导人工处理一个 Open Question；AI 不得代答、改写或勾选。"
            "全部问题由人工更新到 planning artifacts 后，record --action resolve_open_questions --result ok。"
        )
    elif action == "review_open_questions":
        base["instruction"] = (
            "语义复核历史 Open Questions 裸列表，并调用 record --action review_open_questions "
            "--result <ok|blocked> --review-verdict <resolved|blocking|ambiguous> --summary <摘要>。"
        )
    elif action == "review_planning_readiness":
        base["instruction"] = (
            "按 trellis-brainstorm Quality Bar 复核验收标准是否可测试、范围/非目标是否明确、"
            "关键决策是否收敛、仓库可回答的问题是否已研究；然后调用 record "
            "--action review_planning_readiness --result <ok|blocked> "
            "--readiness-verdict <ready|repairable|blocking> --summary <摘要>。"
        )
    elif action == "run_planning_repair":
        base["instruction"] = (
            "仅依据现有需求、代码、spec 和仓库证据修复 planning artifacts；不得处理 Open Questions "
            "或高风险黑名单事项。完成后 record --action run_planning_repair --result ok。"
        )
    elif action == "start_task":
        task_name = Path(str(task)).name
        base["command"] = f"python3 ./.trellis/scripts/task.py start {task_name}"
    elif action == "refresh_brief":
        base["instruction"] = "运行 trellis-task-brief 刷新 brief.md，然后 record --result ok；schema 2 不再逐任务等待确认。"
    elif action == "confirm_brief":
        base["instruction"] = (
            "在对话中展示当前完整 brief.md，并等待用户显式确认；收到确认后才调用 "
            "record --action confirm_brief --result ok --summary <确认摘要>。"
        )
    elif action == "run_implement":
        base["instruction"] = "进入 Phase 2.1 implement route，并执行实现。"
    elif action == "run_check_all":
        base["instruction"] = (
            "进入 Phase 2.2 check route，按 requested_check_depth 执行统一 Check-All；DOC 修复需精确声明 "
            "--doc-remediation-file。record 成功后立即 next；返回 retryable 时先在同一 action 自纠。"
        )
    elif action == "run_fix":
        base["instruction"] = "根据最近失败摘要修复问题。"
    elif action == "run_recheck":
        base["instruction"] = (
            "修复后重新执行统一 Check-All，不得低于 minimum_check_depth；DOC 修复需精确声明 "
            "--doc-remediation-file。record 成功后立即 next；返回 retryable 时先在同一 action 自纠。"
        )
    elif action == "run_spec_update":
        base["instruction"] = (
            "执行 trellis-update-spec 并读取 spec_update_result：no-op/written 时 "
            "record --action run_spec_update --result ok 后立即 next；needs-review 时 "
            "record --action run_spec_update --result blocked --failure-type spec-needs-review。"
        )
    elif action == "commit_only":
        base["instruction"] = "进入 trellis-push commit-only 语义：AI 先生成当前任务提交计划并复核 Git 状态，只提交可归属文件，不 push。"
    if extra:
        base.update(extra)
    return base


def _remember_action(
    item: dict[str, Any],
    action_data: dict[str, Any],
    artifact_sha256: dict[str, str] | None = None,
) -> dict[str, Any]:
    """记录 runner 已发出的待回写 action。"""
    item["last_action"] = {
        "action": action_data.get("action"),
        "current_step": action_data.get("current_step"),
        "issued_at": _utc_now(),
    }
    for key in ("prd_sha256", "planning_sha256", "handoff_sha256"):
        if action_data.get(key):
            item["last_action"][key] = action_data[key]
    if artifact_sha256 is not None:
        item["last_action"]["artifact_sha256"] = artifact_sha256
    item["updated_at"] = _utc_now()
    return action_data


def _outstanding_action_name(item: dict[str, Any]) -> str | None:
    """返回当前任务等待 record 回写的 action 名。"""
    last_action = item.get("last_action")
    if isinstance(last_action, dict) and isinstance(last_action.get("action"), str):
        return last_action["action"]
    return None


def _minimum_check_depth(item: dict[str, Any], action: str) -> str | None:
    """返回检查续跑的最小有效深度；旧 recheck 状态按 full 兼容。"""
    if action not in {"run_check_all", "run_recheck"}:
        return None
    last_check = item.get("last_check")
    if not isinstance(last_check, dict):
        return "full" if action == "run_recheck" else None
    effective = last_check.get("effective_depth")
    return str(effective) if effective in {"light", "full"} else "full"


def _block_item(item: dict[str, Any], reason: str, summary: str, detail: dict[str, Any] | None = None) -> None:
    """把当前任务标记为 blocked。"""
    item["status"] = "blocked"
    item["blocked"] = {
        "reason": reason,
        "summary": summary,
        "detail": detail or {},
        "blocked_at": _utc_now(),
    }
    item["updated_at"] = _utc_now()
    _append_item_decision(
        item,
        "blocked",
        summary,
        {"reason": reason, "detail": detail or {}, "queue_continues": True},
    )


def _next_item_legacy(repo_root: Path, state: dict[str, Any]) -> tuple[dict[str, Any] | None, dict[str, Any]]:
    """按 schema 1 协议计算下一步动作。"""
    queue = state.get("queue") if isinstance(state.get("queue"), list) else []
    for index, item in enumerate(queue):
        if not isinstance(item, dict) or item.get("status") not in {"pending", "running"}:
            continue

        state["current_index"] = index
        item["status"] = "running"
        task = str(item.get("task"))
        item["task_status"] = _task_status(repo_root, task)

        if item["task_status"] == "planning":
            gate_status, gate = _start_gate(
                repo_root,
                task,
                item,
                _effective_route_authorization(repo_root, task, state.get("route_authorization")),
            )
            if gate_status == "blocked":
                _block_item(item, gate["reason"], gate["message"], gate)
                continue
            if gate_status == "action":
                action_name = str(gate.get("action") or "refresh_brief")
                item["current_step"] = action_name
                return item, _remember_action(item, _action(action_name, item, gate))
            item["current_step"] = "start_task"
            return item, _remember_action(item, _action("start_task", item))

        step = item.get("current_step") or "implement"
        if step in {"start_task", "implement"}:
            item["current_step"] = "implement"
            return item, _remember_action(item, _action("run_implement", item))
        if step == "check":
            return item, _remember_action(
                item,
                _action(
                    "run_check_all",
                    item,
                    {
                        "requested_check_depth": _requested_check_depth(state),
                        "minimum_check_depth": _minimum_check_depth(item, "run_check_all"),
                    },
                ),
            )
        if step == "fix":
            attempts = int(item.setdefault("attempts", {}).get("fix_recheck", 0))
            if attempts > MAX_FIX_RECHECK:
                _block_item(item, "retry-budget-exhausted", "fix/recheck 已达到默认 3 轮预算")
                continue
            return item, _remember_action(
                item,
                _action("run_fix", item, {"attempt": attempts, "max_attempts": MAX_FIX_RECHECK}),
            )
        if step == "recheck":
            return item, _remember_action(
                item,
                _action(
                    "run_recheck",
                    item,
                    {
                        "requested_check_depth": _requested_check_depth(state),
                        "minimum_check_depth": _minimum_check_depth(item, "run_recheck"),
                    },
                ),
            )
        if step == "spec_update":
            return item, _remember_action(item, _action("run_spec_update", item))
        if step == "commit_only":
            return item, _remember_action(item, _action("commit_only", item))

        _block_item(item, "unknown-step", f"未知 current_step:{step}")

    state["status"] = _terminal_status(queue, LEGACY_SCHEMA_VERSION)
    return None, {
        "status": "blocked" if state["status"] == "blocked" else "done",
        "finish_work_required_for_archive": True,
        "instruction": (
            "auto-loop 队列存在 blocked 项；补齐条件后运行 retry-blocked 继续同一个 run。"
            if state["status"] == "blocked"
            else "auto-loop 队列已结束；如需归档任务，请用户显式运行 trellis-finish-work。"
        ),
        "summary": _compact_summary(state),
    }


def _prepare_action(state: dict[str, Any], action: str, extra: dict[str, Any]) -> dict[str, Any]:
    """保存并返回 run 级 prepare action。"""
    action_data = {"status": "action", "action": action, **extra}
    state["prepare_action"] = {
        **action_data,
        "issued_at": _utc_now(),
    }
    return action_data


def _planning_gate_v2(
    repo_root: Path,
    state: dict[str, Any],
    item: dict[str, Any],
) -> tuple[str, dict[str, Any]]:
    """执行 schema 2 单任务 planning 确定性门禁。"""
    task_ref = str(item.get("task") or "")
    task_dir = _task_dir(repo_root, task_ref)
    prd = task_dir / "prd.md"
    if not prd.is_file():
        return "blocked", {"reason": "missing-prd", "message": "缺少 prd.md"}
    design = task_dir / "design.md"
    implement = task_dir / "implement.md"
    if design.exists() != implement.exists():
        missing = "implement.md" if design.exists() else "design.md"
        return "blocked", {
            "reason": "incomplete-complex-artifacts",
            "message": f"复杂任务缺少 {missing}",
        }

    authorization = _effective_route_authorization(
        repo_root,
        task_ref,
        state.get("route_authorization"),
    )
    if authorization.get("implement") != "inline" and not _has_real_jsonl_entries(
        task_dir / "implement.jsonl",
        repo_root,
    ):
        return "blocked", {
            "reason": "missing-implement-context",
            "message": "implement.jsonl 未 curated",
        }
    if authorization.get("check") != "check-all-inline" and not _has_real_jsonl_entries(
        task_dir / "check.jsonl",
        repo_root,
    ):
        return "blocked", {
            "reason": "missing-check-context",
            "message": "check.jsonl 未 curated",
        }

    planning_hash, planning_files = _planning_digest(task_dir)
    readiness = item.get("planning_readiness_review")
    if not isinstance(readiness, dict) or readiness.get("planning_sha256") != planning_hash:
        return "action", {
            "action": "review_planning_readiness",
            "message": "复核 planning artifacts；可确定的问题允许返回 repairable",
            "planning_sha256": planning_hash,
            "planning_files": planning_files,
            "attempt": int(item.get("planning_attempts") or 0),
            "max_attempts": MAX_PLANNING_REPAIR,
        }
    verdict = readiness.get("verdict")
    if verdict == "repairable":
        attempts = int(item.get("planning_attempts") or 0)
        if attempts >= MAX_PLANNING_REPAIR:
            return "blocked", {
                "reason": "planning-repair-budget-exhausted",
                "message": "planning repair 已达到 3 轮预算",
                "review": readiness,
            }
        return "action", {
            "action": "run_planning_repair",
            "message": readiness.get("summary") or "修复 planning artifacts",
            "planning_sha256": planning_hash,
            "attempt": attempts + 1,
            "max_attempts": MAX_PLANNING_REPAIR,
        }
    if verdict != "ready":
        return "blocked", {
            "reason": "planning-readiness",
            "message": readiness.get("summary") or "planning artifacts 不具备实现条件",
            "review": readiness,
        }

    brief_stale, newer_sources = _brief_is_stale(task_dir)
    if brief_stale:
        return "action", {
            "action": "refresh_brief",
            "message": "brief.md 缺失或过期",
            "planning_sha256": planning_hash,
            "newer_sources": newer_sources,
        }
    handoff_hash, handoff_files = _artifact_digest([
        task_dir / "prd.md",
        task_dir / "design.md",
        task_dir / "implement.md",
        task_dir / "brief.md",
    ])
    return "ready", {
        "planning_sha256": planning_hash,
        "planning_files": planning_files,
        "handoff_sha256": handoff_hash,
        "handoff_files": handoff_files,
    }


def _dependency_blockers(state: dict[str, Any], item: dict[str, Any]) -> list[dict[str, Any]]:
    """返回当前任务已失败的直接依赖及其链摘要。"""
    by_task = {str(entry.get("task")): entry for entry in _queue_items(state)}
    blockers: list[dict[str, Any]] = []
    for dependency in item.get("depends_on", []):
        target = by_task.get(str(dependency))
        if not isinstance(target, dict) or target.get("status") != "blocked":
            continue
        blocked = target.get("blocked") if isinstance(target.get("blocked"), dict) else {}
        chain = [str(dependency)] + [
            str(value)
            for value in target.get("blocked_by", [])
            if isinstance(value, str)
        ]
        blockers.append({
            "task": dependency,
            "reason": blocked.get("reason"),
            "chain": chain,
        })
    return blockers


def _current_artifact_hashes(repo_root: Path, item: dict[str, Any]) -> tuple[str, str]:
    """读取任务当前 planning 与 handoff 摘要。"""
    task_dir = _task_dir(repo_root, str(item.get("task") or ""))
    planning_hash, _ = _planning_digest(task_dir)
    handoff_hash, _ = _artifact_digest([
        task_dir / "prd.md",
        task_dir / "design.md",
        task_dir / "implement.md",
        task_dir / "brief.md",
    ])
    return planning_hash, handoff_hash


def _next_running_v2(repo_root: Path, state: dict[str, Any]) -> tuple[dict[str, Any] | None, dict[str, Any]]:
    """按冻结 manifest 调度 schema 2 running 队列。"""
    queue = _queue_items(state)
    for index, item in enumerate(queue):
        if item.get("status") not in {"pending", "running"}:
            continue
        state["current_index"] = index
        blockers = _dependency_blockers(state, item)
        if blockers:
            item["blocked_by"] = [str(entry["task"]) for entry in blockers]
            _block_item(
                item,
                "blocked-dependency",
                "显式前置任务未完成",
                {"dependencies": blockers},
            )
            continue
        item["status"] = "running"
        task_ref = str(item.get("task") or "")
        task_status = _task_status(repo_root, task_ref)
        item["task_status"] = task_status
        if task_status not in ALLOWED_TASK_STATUSES:
            _block_item(item, "task-status-drift", f"任务状态已变化:{task_status}")
            continue
        if item.get("planning_sha256"):
            try:
                planning_hash, handoff_hash = _current_artifact_hashes(repo_root, item)
            except OSError as exc:
                _block_item(item, "artifact-drift", f"无法读取 planning artifacts:{exc}")
                continue
            if (
                planning_hash != item.get("planning_sha256")
                or handoff_hash != item.get("handoff_sha256")
            ):
                _block_item(
                    item,
                    "artifact-drift",
                    "planning/handoff artifacts 与已授权 manifest 不一致",
                    {
                        "expected_planning_sha256": item.get("planning_sha256"),
                        "actual_planning_sha256": planning_hash,
                        "expected_handoff_sha256": item.get("handoff_sha256"),
                        "actual_handoff_sha256": handoff_hash,
                    },
                )
                continue

        step = item.get("current_step") or ("start_task" if task_status == "planning" else "implement")
        if task_status == "planning" and step == "start_task":
            return item, _remember_action(item, _action("start_task", item))
        if step in {"start_task", "implement"}:
            item["current_step"] = "implement"
            return item, _remember_action(item, _action("run_implement", item))
        if step == "check":
            return item, _remember_action(item, _action("run_check_all", item, {
                "requested_check_depth": _requested_check_depth(state),
                "minimum_check_depth": _minimum_check_depth(item, "run_check_all"),
            }), _task_artifact_hashes(repo_root, item))
        if step == "fix":
            attempts = int(item.setdefault("attempts", {}).get("fix_recheck", 0))
            if attempts > MAX_FIX_RECHECK:
                _block_item(item, "retry-budget-exhausted", "fix/recheck 已达到默认 3 轮预算")
                continue
            return item, _remember_action(item, _action("run_fix", item, {
                "attempt": attempts,
                "max_attempts": MAX_FIX_RECHECK,
            }))
        if step == "recheck":
            return item, _remember_action(item, _action("run_recheck", item, {
                "requested_check_depth": _requested_check_depth(state),
                "minimum_check_depth": _minimum_check_depth(item, "run_recheck"),
            }), _task_artifact_hashes(repo_root, item))
        if step == "spec_update":
            return item, _remember_action(item, _action("run_spec_update", item))
        if step == "commit_only":
            return item, _remember_action(item, _action("commit_only", item))
        _block_item(item, "unknown-step", f"未知 current_step:{step}")

    state["status"] = _terminal_status(queue)
    return None, {
        "status": "done" if state["status"] == "completed" else "completed_with_blocked",
        "finish_work_required_for_archive": True,
        "instruction": (
            "auto-loop 已处理完整队列；blocked 项可由用户显式运行 retry-blocked。"
            if state["status"] == "completed_with_blocked"
            else "auto-loop 队列已结束；归档仍需用户显式运行 trellis-finish-work。"
        ),
        "summary": _compact_summary(state),
    }


def _next_prepare(repo_root: Path, state: dict[str, Any]) -> tuple[dict[str, Any] | None, dict[str, Any]]:
    """推进 schema 2 全队列 prepare，并在就绪后直接进入 running。"""
    existing = state.get("prepare_action")
    if isinstance(existing, dict):
        return None, {key: value for key, value in existing.items() if key != "issued_at"}

    dirty = _dirty_entries(state)
    if dirty and not state.get("dirty_classified"):
        state["status"] = "preparing"
        return None, _prepare_action(state, "classify_dirty_baseline", {
            "dirty": dirty,
            "message": "分类启动前已存在的 dirty paths",
        })

    questions = _batch_open_questions(repo_root, state)
    if questions:
        state["status"] = "awaiting_input"
        return None, _prepare_action(state, "resolve_open_questions", {
            "questions": questions,
            "message": "全队列 Open Questions 必须由人工全部收敛",
        })
    state["status"] = "preparing"

    for item in _queue_items(state):
        if item.get("status") == "blocked" or item.get("prepare_status") == "ready":
            continue
        task_ref = str(item.get("task") or "")
        status = _task_status(repo_root, task_ref)
        item["task_status"] = status
        if status not in ALLOWED_TASK_STATUSES:
            item["prepare_status"] = "blocked"
            _block_item(item, "task-status-not-runnable", f"任务状态不允许 auto-loop:{status}")
            continue
        if status == "in_progress":
            item["prepare_status"] = "ready"
            if item.get("current_step") not in {"implement", "check", "fix", "recheck", "spec_update", "commit_only"}:
                item["current_step"] = "implement"
            continue

        gate_status, gate = _planning_gate_v2(repo_root, state, item)
        if gate_status == "blocked":
            item["prepare_status"] = "blocked"
            _block_item(item, str(gate["reason"]), str(gate["message"]), gate)
            continue
        if gate_status == "action":
            action_name = str(gate["action"])
            item["current_step"] = "planning_repair" if action_name == "run_planning_repair" else action_name
            item["prepare_status"] = "repairing" if action_name == "run_planning_repair" else "pending"
            return item, _remember_action(item, _action(action_name, item, gate))
        item["prepare_status"] = "ready"
        item["planning_sha256"] = gate["planning_sha256"]
        item["handoff_sha256"] = gate["handoff_sha256"]
        item["current_step"] = "start_task"

    original = [str(value) for value in state.get("original_order", [])]
    try:
        ordered = _stable_topological_order(_queue_items(state))
    except ValueError as exc:
        state["status"] = "globally_blocked"
        state["global_block"] = {
            "reason": "invalid-task-dependencies",
            "message": str(exc),
            "blocked_at": _utc_now(),
        }
        return None, {
            "status": "globally_blocked",
            "reason": "invalid-task-dependencies",
            "message": str(exc),
        }
    execution = [str(item.get("task")) for item in ordered]
    if execution != original:
        state["queue"] = ordered
        state["queue_reordered"] = {
            "original_order": original,
            "execution_order": execution,
            "reason": "explicit-dependencies",
            "recorded_at": _utc_now(),
        }
    _append_manifest_revision(state)
    state["status"] = "running"
    state["current_index"] = 0
    return _next_running_v2(repo_root, state)


def _next_item(repo_root: Path, state: dict[str, Any]) -> tuple[dict[str, Any] | None, dict[str, Any]]:
    """按 runtime schema 分派 prepare 或 running 状态机。"""
    if _schema_version(state) == LEGACY_SCHEMA_VERSION:
        return _next_item_legacy(repo_root, state)
    if state.get("status") in {"preparing", "awaiting_input"}:
        return _next_prepare(repo_root, state)
    return _next_running_v2(repo_root, state)


def cmd_start(args: argparse.Namespace) -> int:
    """创建 auto run。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})

    try:
        current_path, current = _load_current_state(repo_root)
        if current.get("status") in ACTIVE_RUN_STATUSES and not args.force:
            return _print({
                "status": "error",
                "reason": "auto-run-already-running",
                "run_id": current.get("run_id"),
                "path": _rel_path(repo_root, current_path),
            })
        if current.get("status") in {"blocked", "completed_with_blocked"} and not args.force:
            return _print({
                "status": "error",
                "reason": "auto-run-blocked-retry-available",
                "run_id": current.get("run_id"),
                "path": _rel_path(repo_root, current_path),
                "suggested_command": f"python3 ./.trellis/scripts/auto_loop.py retry-blocked --run-id {current.get('run_id')}",
                "summary": _format_summary(current, args),
            })
    except ValueError as exc:
        if str(exc) != "没有可恢复的唯一 auto run":
            return _print({
                "status": "error",
                "reason": "current-auto-state-invalid",
                "message": str(exc),
                "runs": _recent_run_summaries(repo_root),
            })

    route_authorization: dict[str, str] = {}
    if args.route_implement:
        route_authorization["implement"] = args.route_implement
    if args.route_check:
        route_authorization["check"] = args.route_check

    run_id = args.run_id or _new_run_id()
    queue = [_make_item(repo_root, task) for task in args.tasks]
    tasks_by_ref = {str(item["task"]): item for item in queue}
    invalid_statuses = [
        {"task": task, "status": item.get("task_status")}
        for task, item in tasks_by_ref.items()
        if item.get("task_status") not in ALLOWED_TASK_STATUSES
    ]
    if invalid_statuses:
        return _print({
            "status": "error",
            "reason": "task-status-not-runnable",
            "tasks": invalid_statuses,
        })
    for raw in args.depends_on or []:
        if "=" not in raw:
            raise ValueError(f"依赖参数必须是 <dependent>=<dependency>:{raw}")
        dependent_raw, dependency_raw = raw.split("=", 1)
        dependent = _normalize_task_ref(repo_root, dependent_raw)
        dependency = _normalize_task_ref(repo_root, dependency_raw)
        if dependent not in tasks_by_ref or dependency not in tasks_by_ref:
            raise ValueError(f"依赖双方必须都在当前队列:{raw}")
        dependencies = tasks_by_ref[dependent].setdefault("depends_on", [])
        if dependency not in dependencies:
            dependencies.append(dependency)

    repositories, git_block = _capture_git_baseline(repo_root)
    if git_block:
        return _print({"status": "error", **git_block})
    now = _utc_now()
    state = {
        "schema_version": SCHEMA_VERSION,
        "run_id": run_id,
        "status": "preparing",
        "profile": args.profile,
        "check_depth": args.check_depth,
        "created_at": now,
        "updated_at": now,
        "owner": {"host": socket.gethostname(), "pid": os.getpid()},
        "current_index": 0,
        "route_authorization": route_authorization,
        "authorization": {
            "source": "user-auto-loop-start",
            "authorized_at": now,
            "profile": args.profile,
        },
        "original_order": [str(item["task"]) for item in queue],
        "parent_tasks_outside_queue": _parents_outside_queue(repo_root, queue),
        "repositories": repositories,
        "dirty_classified": not bool(_dirty_entries({"repositories": repositories})),
        "prepare_action": None,
        "manifest_revisions": [],
        "queue": queue,
    }
    path = _run_path(repo_root, run_id)
    _write_state(path, state)
    _write_pointer(repo_root, run_id)
    _link_session_run(repo_root, run_id)
    return _print({"status": "started", "path": _rel_path(repo_root, path), **_format_summary(state, args)})


def cmd_resume(args: argparse.Namespace) -> int:
    """恢复 auto run 摘要。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_current_state(repo_root, args.run_id)
    except ValueError as exc:
        return _print({"status": "error", "reason": "resume-failed", "message": str(exc)})
    _link_session_run(repo_root, str(state.get("run_id")))
    output = {"status": "resumed", "path": _rel_path(repo_root, path), **_format_summary(state, args)}
    if getattr(args, "verbose", False):
        output["resume_capsule"] = _resume_capsule(state)
    return _print(output)


def cmd_next(args: argparse.Namespace) -> int:
    """计算下一步动作。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_current_state(repo_root, args.run_id)
    except ValueError as exc:
        return _print({"status": "error", "reason": "next-failed", "message": str(exc)})
    _link_session_run(repo_root, str(state.get("run_id")))
    if state.get("status") not in ACTIVE_RUN_STATUSES:
        output_status = "done" if state.get("status") == "completed" else state.get("status") or "unknown"
        return _print({"run_id": state.get("run_id"), "status": output_status, "summary": _format_summary(state, args)})
    _, action = _next_item(repo_root, state)
    _write_state(path, state)
    if state.get("status") in TERMINAL_RUN_STATUSES:
        _clear_pointer_if_current(repo_root, str(state.get("run_id") or ""))
    if action.get("status") in {"done", "blocked", "completed_with_blocked", "globally_blocked"}:
        action["summary"] = _format_summary(state, args)
    return _print({"run_id": state.get("run_id"), **action})


def _apply_route_authorization_args(state: dict[str, Any], args: argparse.Namespace) -> dict[str, str]:
    """把命令行 route 参数合并进 run 的临时授权。"""
    route_authorization = state.get("route_authorization")
    if not isinstance(route_authorization, dict):
        route_authorization = {}
    if getattr(args, "route_implement", None):
        route_authorization["implement"] = args.route_implement
    if getattr(args, "route_check", None):
        route_authorization["check"] = args.route_check
    state["route_authorization"] = route_authorization
    return {str(key): str(value) for key, value in route_authorization.items()}


def _apply_check_depth_arg(state: dict[str, Any], args: argparse.Namespace) -> tuple[str, str, bool]:
    """把 retry 参数中的检查深度合并到 run 状态。"""
    previous = _requested_check_depth(state)
    requested = getattr(args, "check_depth", None)
    if requested in VALID_CHECK_DEPTHS:
        state["check_depth"] = requested
    current = _requested_check_depth(state)
    return previous, current, previous != current


def _blocked_reason(item: dict[str, Any]) -> str:
    """读取队列项 blocked reason。"""
    blocked = item.get("blocked")
    if isinstance(blocked, dict):
        reason = blocked.get("reason")
        if isinstance(reason, str):
            return reason
    return ""


def cmd_retry_blocked(args: argparse.Namespace) -> int:
    """把可恢复的 blocked 队列项重置为 pending，复用同一个 auto run。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_blocked_state(repo_root, args.run_id)
    except ValueError as exc:
        return _print({
            "status": "error",
            "reason": "retry-blocked-failed",
            "message": str(exc),
            "runs": _recent_run_summaries(repo_root),
        })

    task = _normalize_task_ref(repo_root, args.task) if args.task else None
    route_authorization = _apply_route_authorization_args(state, args)
    previous_check_depth, check_depth, check_depth_changed = _apply_check_depth_arg(state, args)
    queue = state.get("queue") if isinstance(state.get("queue"), list) else []
    reset: list[str] = []
    skipped: list[dict[str, str]] = []

    for index, item in enumerate(queue):
        if not isinstance(item, dict) or item.get("status") != "blocked":
            continue
        if task and item.get("task") != task:
            continue
        reason = _blocked_reason(item)
        if not args.all and not task and reason not in RECOVERABLE_BLOCK_REASONS:
            skipped.append({"task": str(item.get("task")), "reason": reason or "unknown"})
            continue
        item["status"] = "pending"
        item["blocked"] = None
        item["last_action"] = None
        if reason in {"retry-budget-exhausted", "artifact-drift", "commit-repair-budget-exhausted"}:
            attempts = item.get("attempts")
            if not isinstance(attempts, dict):
                attempts = {}
            if reason == "retry-budget-exhausted":
                attempts["fix_recheck"] = 0
            elif reason == "artifact-drift":
                attempts["artifact_reconcile"] = 0
            else:
                attempts["commit_repair"] = 0
            item["attempts"] = attempts
        item["task_status"] = _task_status(repo_root, str(item.get("task")))
        item["updated_at"] = _utc_now()
        state["current_index"] = min(int(state.get("current_index") or index), index)
        reset.append(str(item.get("task")))
        _append_item_decision(
            item,
            "retry_unblocked",
            "blocked 队列项已重置，将在同一个 auto run 内重试",
            {
                "previous_reason": reason,
                "route_authorization": route_authorization,
                "check_depth": check_depth,
                "previous_check_depth": previous_check_depth if check_depth_changed else None,
            },
        )

    if not reset:
        return _print({
            "status": "error",
            "reason": "no-retryable-blocked-items",
            "message": "没有可自动重试的 blocked 队列项；如确认要重试，可加 --all 或指定 --task。",
            "skipped": skipped,
            "summary": _format_summary(state, args),
        })

    if _schema_version(state) == SCHEMA_VERSION:
        state["status"] = "preparing"
        state["prepare_action"] = None
        state["manifest_sha256"] = None
        for queue_item in _queue_items(state):
            if queue_item.get("status") == "pending":
                queue_item["prepare_status"] = "pending"
    else:
        state["status"] = "running"
    _write_state(path, state)
    _write_pointer(repo_root, str(state.get("run_id")))
    _link_session_run(repo_root, str(state.get("run_id")))
    return _print({
        "status": "retry-ready",
        "run_id": state.get("run_id"),
        "path": _rel_path(repo_root, path),
        "reset": reset,
        "skipped": skipped,
        "summary": _format_summary(state, args),
    })


def _find_record_item(state: dict[str, Any], task_ref: str | None) -> dict[str, Any] | None:
    """找到 record 要更新的队列项。"""
    queue = state.get("queue") if isinstance(state.get("queue"), list) else []
    if task_ref:
        for item in queue:
            if (
                isinstance(item, dict)
                and item.get("task") == task_ref
                and isinstance(item.get("last_action"), dict)
            ):
                return item
    for item in queue:
        if isinstance(item, dict) and isinstance(item.get("last_action"), dict):
            return item
    return None


def _record_open_questions_review(
    repo_root: Path,
    item: dict[str, Any],
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """校验并保存历史 Open Questions 的 AI 语义复核结果。"""
    verdict = getattr(args, "review_verdict", None)
    if verdict not in {"resolved", "blocking", "ambiguous"}:
        return {
            "status": "error",
            "reason": "missing-review-verdict",
            "message": "review_open_questions 必须提供 --review-verdict。",
        }
    expected_result = "ok" if verdict == "resolved" else "blocked"
    if args.result != expected_result:
        return {
            "status": "error",
            "reason": "review-result-mismatch",
            "message": f"{verdict} 必须配合 --result {expected_result}。",
        }

    task_ref = str(item.get("task") or "")
    prd = _task_dir(repo_root, task_ref) / "prd.md"
    try:
        content = prd.read_bytes()
    except OSError as exc:
        return {
            "status": "error",
            "reason": "review-prd-unreadable",
            "message": str(exc),
        }
    actual_hash = _prd_sha256(content)
    last_action = item.get("last_action")
    expected_hash = last_action.get("prd_sha256") if isinstance(last_action, dict) else None
    if expected_hash != actual_hash:
        return {
            "status": "error",
            "reason": "stale-open-questions-review",
            "expected_prd_sha256": expected_hash,
            "actual_prd_sha256": actual_hash,
            "message": "prd.md 已变化，请重新运行 next 获取新的复核 action。",
        }

    questions = _open_questions(content.decode("utf-8"))["bare"]
    review = {
        "prd_sha256": actual_hash,
        "verdict": verdict,
        "items": questions,
        "summary": args.summary or "",
        "reviewed_at": _utc_now(),
    }
    item["open_questions_review"] = review
    item["last_action"] = None
    if verdict == "resolved":
        item["current_step"] = "start_task"
        item["updated_at"] = _utc_now()
        _append_item_decision(
            item,
            "open_questions_reviewed",
            args.summary or "历史 Open Questions 已确认无阻塞事项",
            review,
        )
    else:
        reason = "open-questions" if verdict == "blocking" else "open-questions-ambiguous"
        summary = args.summary or (
            "历史 Open Questions 仍有未决事项"
            if verdict == "blocking"
            else "历史 Open Questions 无法确定是否已解决"
        )
        _block_item(item, reason, summary, {"questions": questions, "review": review})
    return None


def _record_planning_readiness_review(
    repo_root: Path,
    item: dict[str, Any],
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """校验并保存绑定当前 artifacts 的 planning 语义就绪结论。"""
    verdict = getattr(args, "readiness_verdict", None)
    allowed = {"ready", "repairable", "blocking"} if getattr(args, "schema_v2", False) else {
        "ready",
        "blocking",
        "ambiguous",
    }
    if verdict not in allowed:
        return {
            "status": "error",
            "reason": "missing-readiness-verdict",
            "message": "review_planning_readiness 必须提供 --readiness-verdict。",
        }
    expected_result = "ok" if verdict in {"ready", "repairable"} else "blocked"
    if args.result != expected_result:
        return {
            "status": "error",
            "reason": "readiness-result-mismatch",
            "message": f"{verdict} 必须配合 --result {expected_result}。",
        }

    task_dir = _task_dir(repo_root, str(item.get("task") or ""))
    try:
        actual_hash, files = _planning_digest(task_dir)
    except OSError as exc:
        return {"status": "error", "reason": "planning-artifacts-unreadable", "message": str(exc)}
    last_action = item.get("last_action")
    expected_hash = last_action.get("planning_sha256") if isinstance(last_action, dict) else None
    if expected_hash != actual_hash:
        return {
            "status": "error",
            "reason": "stale-planning-readiness-review",
            "expected_planning_sha256": expected_hash,
            "actual_planning_sha256": actual_hash,
            "message": "planning artifacts 已变化，请重新运行 next 获取新的复核 action。",
        }

    review = {
        "planning_sha256": actual_hash,
        "verdict": verdict,
        "files": files,
        "summary": args.summary or "",
        "reviewed_at": _utc_now(),
    }
    item["planning_readiness_review"] = review
    item["last_action"] = None
    if verdict == "ready":
        item["current_step"] = "start_task"
        item["updated_at"] = _utc_now()
        _append_item_decision(item, "planning_readiness_reviewed", args.summary or "planning artifacts 已具备实现条件", review)
    elif verdict == "repairable":
        item["current_step"] = "planning_repair"
        item["prepare_status"] = "repairing"
        item["updated_at"] = _utc_now()
        _append_item_decision(
            item,
            "planning_repair_requested",
            args.summary or "planning artifacts 存在可由 AI 修复的问题",
            review,
        )
    else:
        reason = "planning-readiness" if verdict == "blocking" else "planning-readiness-ambiguous"
        summary = args.summary or ("planning artifacts 仍有阻塞项" if verdict == "blocking" else "planning readiness 无法确定")
        _block_item(item, reason, summary, {"review": review})
    return None


def _record_brief_confirmation(
    repo_root: Path,
    item: dict[str, Any],
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """校验并保存绑定当前 handoff artifacts 的显式 brief 确认。"""
    if args.result != "ok":
        return {
            "status": "error",
            "reason": "brief-confirmation-result-mismatch",
            "message": "confirm_brief 只有收到用户显式确认后才能以 --result ok 回写。",
        }
    task_dir = _task_dir(repo_root, str(item.get("task") or ""))
    try:
        actual_hash, files = _artifact_digest([
            task_dir / "prd.md",
            task_dir / "design.md",
            task_dir / "implement.md",
            task_dir / "brief.md",
        ])
    except OSError as exc:
        return {"status": "error", "reason": "brief-handoff-unreadable", "message": str(exc)}
    last_action = item.get("last_action")
    expected_hash = last_action.get("handoff_sha256") if isinstance(last_action, dict) else None
    if expected_hash != actual_hash:
        return {
            "status": "error",
            "reason": "stale-brief-confirmation",
            "expected_handoff_sha256": expected_hash,
            "actual_handoff_sha256": actual_hash,
            "message": "planning artifacts 或 brief.md 已变化，请重新运行 next 并展示最新 brief。",
        }
    confirmation = {
        "handoff_sha256": actual_hash,
        "files": files,
        "summary": args.summary or "",
        "confirmed_at": _utc_now(),
    }
    item["brief_confirmation"] = confirmation
    item["last_action"] = None
    item["current_step"] = "start_task"
    item["updated_at"] = _utc_now()
    _append_item_decision(item, "brief_confirmed", args.summary or "用户已确认 planning artifacts 与 brief", confirmation)
    return None


def _advance_after_ok(item: dict[str, Any], action: str, args: argparse.Namespace) -> None:
    """根据成功动作推进 current_step。"""
    if action in ROUTE_ACTION_TARGETS:
        target = ROUTE_ACTION_TARGETS[action]
        mode = getattr(args, "route_mode", None)
        source = getattr(args, "route_source", None)
        data = {"target": target}
        if mode:
            data["mode"] = mode
        if source:
            data["source"] = source
        summary = f"{target} route resolved"
        if mode:
            summary += f" to {mode}"
        if source:
            summary += f" from {source}"
        _append_item_decision(item, "route_resolved", summary, data)

    if action == "refresh_brief":
        item["current_step"] = "start_task"
    elif action == "start_task":
        item["current_step"] = "implement"
    elif action in {"run_implement", "run_fix"}:
        item["current_step"] = "check" if action == "run_implement" else "recheck"
    elif action in {"run_check_all", "run_recheck"}:
        item["current_step"] = "spec_update"
    elif action == "run_spec_update":
        item["current_step"] = "commit_only"
    elif action == "commit_only":
        item["status"] = "completed"
        item["current_step"] = "done"
        repo_commits = item.get("commits") if isinstance(item.get("commits"), list) else []
        item["commit"] = args.commit or (repo_commits[-1].get("commit") if repo_commits else None)
        if args.summary:
            item["commit_summary"] = args.summary
        _append_item_decision(
            item,
            "commit_plan",
            args.summary or "trellis-push commit-only 计划已执行",
            {
                "planned_files": args.files or [],
                "retained_files": args.retained_files or [],
                "commit_message": args.commit_message,
                "snapshot_commit": args.snapshot_commit,
                "commits": repo_commits,
            },
        )
        _append_item_decision(
            item,
            "commit_completed",
            "trellis-push commit-only 本地提交完成",
            {
                "commit": item.get("commit"),
                "commit_message": args.commit_message,
                "snapshot_commit": args.snapshot_commit,
                "commits": repo_commits,
            },
        )
        _append_item_decision(
            item,
            "task_auto_completed",
            "auto-loop item 已完成本地提交；任务生命周期仍等待 finish-work/archive",
            {
                "commit": item.get("commit"),
                "summary": args.summary,
                "task_status": item.get("task_status"),
                "commits": repo_commits,
            },
        )
    item["updated_at"] = _utc_now()


def _record_failure(item: dict[str, Any], action: str, args: argparse.Namespace) -> None:
    """记录失败并决定是否进入 fix 或 blocked。"""
    item["last_failure"] = {
        "action": action,
        "failure_type": args.failure_type,
        "summary": args.summary,
        "files": args.files or [],
        "commits": item.get("commits", []),
        "failed_at": _utc_now(),
    }
    if action in {"run_implement", "run_check_all", "run_fix", "run_recheck"}:
        attempts = item.setdefault("attempts", {})
        attempts["fix_recheck"] = int(attempts.get("fix_recheck", 0)) + 1
        if attempts["fix_recheck"] > MAX_FIX_RECHECK:
            _block_item(item, "retry-budget-exhausted", args.summary or "fix/recheck 达到默认 3 轮预算")
        else:
            item["current_step"] = "fix"
            item["updated_at"] = _utc_now()
        _append_item_decision(
            item,
            "warning",
            args.summary or f"{action} 执行失败，进入修复流程",
            {
                "action": action,
                "failure_type": args.failure_type,
                "files": args.files or [],
                "attempts": item.setdefault("attempts", {}).get("fix_recheck", 0),
            },
        )
        return
    if action == "commit_only" and args.failure_type == "commit-repairable":
        attempts = item.setdefault("attempts", {})
        attempts["commit_repair"] = int(attempts.get("commit_repair", 0)) + 1
        if attempts["commit_repair"] > MAX_COMMIT_REPAIR:
            _block_item(
                item,
                "commit-repair-budget-exhausted",
                args.summary or "commit-only 达到默认 3 轮可恢复重试预算",
                {"commits": item.get("commits", [])},
            )
        else:
            item["current_step"] = "commit_only"
            item["updated_at"] = _utc_now()
        _append_item_decision(
            item,
            "warning",
            args.summary or "commit-only 执行失败，将从真实 Git 状态重新规划",
            {
                "action": action,
                "failure_type": args.failure_type,
                "files": args.files or [],
                "commits": item.get("commits", []),
                "attempts": attempts["commit_repair"],
            },
        )
        return
    _block_item(item, args.failure_type or "action-failed", args.summary or f"{action} 执行失败")


def _check_record_error(
    state: dict[str, Any],
    item: dict[str, Any],
    action: str,
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """校验检查结果没有低于请求或重检要求的最小深度。"""
    if action not in {"run_check_all", "run_recheck"}:
        return None
    effective = getattr(args, "effective_check_depth", None) or "full"
    requested = _requested_check_depth(state)
    minimum = _minimum_check_depth(item, action)
    if effective == "light" and (requested == "full" or minimum == "full"):
        return {
            "status": "error",
            "reason": "check-depth-below-minimum",
            "requested_check_depth": requested,
            "minimum_check_depth": minimum,
            "effective_check_depth": effective,
            "message": "检查结果低于 run 请求或重检要求的最小深度，请按 full 完成 Check-All 后重新 record。",
        }
    return None


def _record_check_result(
    state: dict[str, Any],
    item: dict[str, Any],
    action: str,
    args: argparse.Namespace,
) -> None:
    """保存 Check-All 实际深度与结果，供审计和后续 recheck 使用。"""
    if action not in {"run_check_all", "run_recheck"}:
        return
    provided_effective = getattr(args, "effective_check_depth", None)
    effective = provided_effective or "full"
    if provided_effective is None:
        reason = "legacy-default-full"
    else:
        reason = getattr(args, "check_depth_reason", None) or "未提供深度原因"
    last_check = {
        "action": action,
        "requested_depth": _requested_check_depth(state),
        "minimum_depth": _minimum_check_depth(item, action),
        "effective_depth": effective,
        "reason": reason,
        "result": args.result,
        "recorded_at": _utc_now(),
    }
    item["last_check"] = last_check
    _append_item_decision(
        item,
        "check_recorded",
        f"Check-All {args.result}: requested={last_check['requested_depth']} effective={effective}",
        last_check,
    )


def _baseline_key(repository: str, path: str) -> str:
    """构造跨仓库唯一的 dirty baseline key。"""
    return f"{repository}::{path}"


def _record_dirty_classification(
    repo_root: Path,
    state: dict[str, Any],
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """校验并保存启动前 dirty path 分类。"""
    if args.result != "ok":
        return {
            "status": "error",
            "reason": "dirty-classification-result-mismatch",
            "message": "dirty baseline 分类完成后必须以 --result ok 回写",
        }
    entries = {
        _baseline_key(str(entry["repository"]), str(entry["path"])): entry
        for entry in _dirty_entries(state)
    }
    assignments: dict[str, tuple[str, str]] = {}
    for raw in args.owned_dirty or []:
        if "=" not in raw:
            return {
                "status": "error",
                "reason": "invalid-owned-dirty",
                "message": f"owned-dirty 必须是 <task>=<repository>::<path>:{raw}",
            }
        task_raw, key = raw.split("=", 1)
        task = _normalize_task_ref(repo_root, task_raw)
        if task not in {str(item.get("task")) for item in _queue_items(state)}:
            return {"status": "error", "reason": "owned-dirty-task-not-in-queue", "task": task}
        if key in assignments:
            return {"status": "error", "reason": "dirty-path-classified-twice", "path": key}
        assignments[key] = ("owned", task)
    for key in args.protected_retained or []:
        if key in assignments:
            return {"status": "error", "reason": "dirty-path-classified-twice", "path": key}
        assignments[key] = ("protected", "")
    if set(assignments) != set(entries):
        return {
            "status": "error",
            "reason": "dirty-classification-incomplete",
            "missing": sorted(set(entries) - set(assignments)),
            "unknown": sorted(set(assignments) - set(entries)),
        }

    repositories = state.get("repositories") if isinstance(state.get("repositories"), list) else []
    by_task = {str(item.get("task")): item for item in _queue_items(state)}
    by_repo = {str(repository.get("root")): repository for repository in repositories if isinstance(repository, dict)}
    for key, entry in entries.items():
        repository = str(entry["repository"])
        path = str(entry["path"])
        root = repo_root if repository == "." else repo_root / repository
        if _file_sha256(root / path) != entry["sha256"]:
            return {
                "status": "error",
                "reason": "dirty-baseline-drift",
                "path": key,
            }
        kind, task = assignments[key]
        record = {"repository": repository, "path": path, "sha256": entry["sha256"]}
        if kind == "owned":
            by_task[task].setdefault("owned_dirty", []).append(record)
            by_repo[repository].setdefault("owned_dirty", []).append({"task": task, **record})
        else:
            by_repo[repository].setdefault("protected_retained", []).append(record)
    state["dirty_classified"] = True
    state["prepare_action"] = None
    return None


def _record_resolved_open_questions(
    repo_root: Path,
    state: dict[str, Any],
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """确认人工已在 artifacts 中收敛全部 Open Questions。"""
    if args.result != "ok":
        return {
            "status": "error",
            "reason": "open-questions-result-mismatch",
            "message": "Open Questions 未全部解决时不得完成 prepare action",
        }
    questions = _batch_open_questions(repo_root, state)
    if questions:
        return {
            "status": "error",
            "reason": "open-questions-still-unresolved",
            "questions": questions,
        }
    state["prepare_action"] = None
    state["status"] = "preparing"
    return None


def _record_prepare_item(
    repo_root: Path,
    state: dict[str, Any],
    item: dict[str, Any],
    action: str,
    args: argparse.Namespace,
) -> dict[str, Any] | None:
    """记录 schema 2 单任务 prepare action。"""
    if action == "review_planning_readiness":
        setattr(args, "schema_v2", True)
        return _record_planning_readiness_review(repo_root, item, args)
    if action == "run_planning_repair":
        if args.result != "ok":
            _block_item(
                item,
                args.failure_type or "planning-repair-failed",
                args.summary or "planning repair 失败",
            )
            item["prepare_status"] = "blocked"
            item["last_action"] = None
            return None
        task_dir = _task_dir(repo_root, str(item.get("task") or ""))
        actual_hash, files = _planning_digest(task_dir)
        last_action = item.get("last_action")
        previous_hash = last_action.get("planning_sha256") if isinstance(last_action, dict) else None
        if actual_hash == previous_hash:
            return {
                "status": "error",
                "reason": "planning-repair-no-change",
                "message": "planning repair 未改变 authoritative artifacts",
            }
        item["planning_attempts"] = int(item.get("planning_attempts") or 0) + 1
        item["planning_readiness_review"] = None
        item["last_action"] = None
        item["prepare_status"] = "pending"
        item["current_step"] = "start_task"
        _append_item_decision(item, "planning_repaired", args.summary or "planning artifacts 已修复", {
            "attempt": item["planning_attempts"],
            "files": files,
            "planning_sha256": actual_hash,
        })
        return None
    if action == "refresh_brief":
        if args.result != "ok":
            _block_item(item, args.failure_type or "brief-refresh-failed", args.summary or "brief 刷新失败")
            item["prepare_status"] = "blocked"
            item["last_action"] = None
            return None
        task_dir = _task_dir(repo_root, str(item.get("task") or ""))
        stale, newer = _brief_is_stale(task_dir)
        if stale:
            return {
                "status": "error",
                "reason": "brief-still-stale",
                "newer_sources": newer,
            }
        item["last_action"] = None
        item["prepare_status"] = "pending"
        item["current_step"] = "start_task"
        return None
    return {"status": "error", "reason": "invalid-prepare-action", "action": action}


def _record_schema2_prepare(
    repo_root: Path,
    path: Path,
    state: dict[str, Any],
    args: argparse.Namespace,
) -> int:
    """处理 schema 2 prepare/awaiting_input 阶段的 record。"""
    action = args.action
    prepare_action = state.get("prepare_action")
    if isinstance(prepare_action, dict):
        expected = prepare_action.get("action")
        if action != expected:
            return _print({"status": "error", "reason": "action-mismatch", "expected_action": expected, "actual_action": action})
        if action == "classify_dirty_baseline":
            error = _record_dirty_classification(repo_root, state, args)
        elif action == "resolve_open_questions":
            error = _record_resolved_open_questions(repo_root, state, args)
        else:
            error = {"status": "error", "reason": "invalid-prepare-action", "action": action}
        if error:
            return _print(error)
        _write_state(path, state)
        return _print({"status": "recorded", "run_id": state.get("run_id"), "summary": _format_summary(state, args)})

    task = _normalize_task_ref(repo_root, args.task) if args.task else None
    item = _find_record_item(state, task)
    if item is None:
        return _print({"status": "error", "reason": "no-outstanding-prepare-action"})
    expected = _outstanding_action_name(item)
    if action != expected:
        return _print({"status": "error", "reason": "action-mismatch", "expected_action": expected, "actual_action": action})
    error = _record_prepare_item(repo_root, state, item, action, args)
    if error:
        return _print(error)
    _write_state(path, state)
    return _print({
        "status": "recorded",
        "run_id": state.get("run_id"),
        "task": item.get("task"),
        "item_status": item.get("status"),
        "current_step": item.get("current_step"),
        "summary": _format_summary(state, args),
    })


def _protected_path_conflicts(state: dict[str, Any], files: list[str] | None) -> list[str]:
    """返回 action 文件列表与 protected-retained 的交集。"""
    protected = {
        _baseline_key(str(repository.get("root") or "."), str(entry.get("path") or ""))
        for repository in state.get("repositories", [])
        if isinstance(repository, dict)
        for entry in repository.get("protected_retained", [])
        if isinstance(entry, dict)
    }
    requested = {_normalize_record_file(value) for value in files or []}
    return sorted(protected & requested)


def _consume_protected_baseline_drifts(
    repo_root: Path,
    state: dict[str, Any],
    item: dict[str, Any],
    action: str,
) -> list[dict[str, str]]:
    """检测并记录当前 action 期间发生的 protected 文件漂移。"""
    drifts: list[dict[str, str]] = []
    for repository in state.get("repositories", []):
        if not isinstance(repository, dict):
            continue
        repository_root = str(repository.get("root") or ".")
        root = repo_root if repository_root == "." else repo_root / repository_root
        for entry in repository.get("protected_retained", []):
            if not isinstance(entry, dict):
                continue
            expected = str(entry.get("current_sha256") or entry.get("sha256") or "")
            actual = _file_sha256(root / str(entry.get("path") or ""))
            if actual == expected:
                continue
            drift = {
                "repository": repository_root,
                "path": str(entry.get("path") or ""),
                "expected_sha256": expected,
                "actual_sha256": actual,
            }
            drifts.append(drift)
            # 当前任务被阻塞后，以观察到的内容继续作为 retained 基线，避免误伤独立后续任务。
            entry["current_sha256"] = actual
    if drifts:
        history = state.get("protected_drifts")
        if not isinstance(history, list):
            history = []
        history.append({
            "detected_at": _utc_now(),
            "task": item.get("task"),
            "action": action,
            "files": drifts,
        })
        state["protected_drifts"] = history[-DECISION_LOG_LIMIT:]
    return drifts


def _consume_pending_artifact_decision(
    repo_root: Path,
    state: dict[str, Any],
    item: dict[str, Any],
) -> dict[str, Any] | None:
    """使用待处理 AI 决策授权一次 planning/handoff artifact 重绑定。"""
    pending = item.get("pending_artifact_decision")
    if not isinstance(pending, dict):
        return None
    previous = pending.get("artifact_sha256")
    if not isinstance(previous, dict):
        item["pending_artifact_decision"] = None
        return None
    current = _task_artifact_hashes(repo_root, item)
    changed = sorted(
        key
        for key in set(previous) | set(current)
        if previous.get(key) != current.get(key)
    )
    if not changed:
        item["pending_artifact_decision"] = None
        return None
    authorized = {
        _normalize_record_file(value)
        for value in pending.get("files", [])
        if isinstance(value, str)
    }
    unauthorized = sorted(set(changed) - authorized)
    if unauthorized:
        return {
            "decision_id": pending.get("decision_id"),
            "changed": changed,
            "unauthorized": unauthorized,
        }

    planning_hash, handoff_hash = _current_artifact_hashes(repo_root, item)
    item["planning_sha256"] = planning_hash
    item["handoff_sha256"] = handoff_hash
    item["pending_artifact_decision"] = None
    manifest = _append_manifest_revision(state, str(pending.get("decision_id") or ""))
    _append_item_decision(
        item,
        "decision_artifacts_rebound",
        f"{pending.get('decision_id')}: planning/handoff artifacts 已重新绑定",
        {
            "decision_id": pending.get("decision_id"),
            "files": changed,
            "manifest_revision": manifest["revision"],
            "manifest_sha256": manifest["sha256"],
        },
    )
    return {
        "decision_id": pending.get("decision_id"),
        "changed": changed,
        "manifest_revision": manifest["revision"],
    }


def _consume_check_doc_remediation(
    repo_root: Path,
    state: dict[str, Any],
    item: dict[str, Any],
    action: str,
    raw_files: list[str],
) -> dict[str, Any] | None:
    """校验并消费 Check-All 声明的低风险任务文档修复。"""
    if not raw_files:
        return None
    if action not in CHECK_ACTIONS:
        return {
            "status": "error",
            "reason": "doc-remediation-action-not-allowed",
            "message": "--doc-remediation-file 只允许用于 run_check_all 或 run_recheck。",
        }
    if isinstance(item.get("pending_artifact_decision"), dict):
        return {
            "status": "error",
            "reason": "doc-remediation-decision-conflict",
            "message": "当前 action 同时存在待消费的 decide artifact 授权，不能混用 DOC remediation。",
        }

    last_action = item.get("last_action")
    previous = last_action.get("artifact_sha256") if isinstance(last_action, dict) else None
    if not isinstance(previous, dict):
        return {
            "status": "error",
            "reason": "doc-remediation-baseline-missing",
            "message": "当前 outstanding check action 没有逐文件 artifact baseline，请重新启动安全流程。",
        }

    task_dir = _task_dir(repo_root, str(item.get("task") or ""))
    allowed = {
        _baseline_key(".", _rel_path(repo_root, task_dir / "implement.md")),
        _baseline_key(".", _rel_path(repo_root, task_dir / "brief.md")),
    }
    declared = {_normalize_record_file(value) for value in raw_files}
    forbidden = sorted(declared - allowed)
    if forbidden:
        return {
            "status": "error",
            "reason": "doc-remediation-file-not-allowed",
            "files": forbidden,
            "allowed_files": sorted(allowed),
        }

    current = _task_artifact_hashes(repo_root, item)
    changed = {
        key
        for key in set(previous) | set(current)
        if previous.get(key) != current.get(key)
    }
    if changed != declared:
        return {
            "status": "error",
            "reason": "doc-remediation-files-mismatch",
            "declared_files": sorted(declared),
            "changed_files": sorted(changed),
        }

    planning_hash, handoff_hash = _current_artifact_hashes(repo_root, item)
    item["planning_sha256"] = planning_hash
    item["handoff_sha256"] = handoff_hash
    manifest = _append_manifest_revision(
        state,
        change_source="check-doc-remediation",
        files=sorted(changed),
    )
    detail = {
        "files": sorted(changed),
        "manifest_revision": manifest["revision"],
        "manifest_sha256": manifest["sha256"],
    }
    _append_item_decision(
        item,
        "check_doc_artifacts_rebound",
        "Check-All 低风险任务文档修复已重新绑定到 manifest",
        detail,
    )
    return detail


def _record_artifact_drift(
    path: Path,
    state: dict[str, Any],
    item: dict[str, Any],
    action: str,
    args: argparse.Namespace,
    summary: str,
    detail: dict[str, Any],
) -> int:
    """按 action 风险和预算记录 artifact drift。"""
    if action not in CHECK_ACTIONS or (
        args.result == "blocked" and args.failure_type == "artifact-drift"
    ):
        item["last_action"] = None
        _block_item(item, "artifact-drift", summary, detail)
        _write_state(path, state)
        return _print({
            "status": "recorded",
            "run_id": state.get("run_id"),
            "task": item.get("task"),
            "item_status": item.get("status"),
            "current_step": item.get("current_step"),
            "summary": _format_summary(state, args),
        })

    attempts = item.setdefault("attempts", {})
    attempt = int(attempts.get("artifact_reconcile", 0)) + 1
    attempts["artifact_reconcile"] = attempt
    item["last_failure"] = {
        "action": action,
        "failure_type": "artifact-drift",
        "summary": summary,
        "detail": detail,
        "failed_at": _utc_now(),
    }
    if attempt > MAX_ARTIFACT_RECONCILE:
        item["last_action"] = None
        _block_item(
            item,
            "artifact-drift",
            "Check action artifact drift 连续自纠超过 3 次",
            {**detail, "attempt": attempt, "max_attempts": MAX_ARTIFACT_RECONCILE},
        )
        _write_state(path, state)
        return _print({
            "status": "recorded",
            "run_id": state.get("run_id"),
            "task": item.get("task"),
            "item_status": item.get("status"),
            "current_step": item.get("current_step"),
            "summary": _format_summary(state, args),
        })

    item["updated_at"] = _utc_now()
    _append_item_decision(
        item,
        "warning",
        summary,
        {
            "action": action,
            "failure_type": "artifact-drift",
            "attempt": attempt,
            "max_attempts": MAX_ARTIFACT_RECONCILE,
            "detail": detail,
        },
    )
    _write_state(path, state)
    return _print({
        "status": "retryable",
        "reason": "artifact-drift",
        "run_id": state.get("run_id"),
        "task": item.get("task"),
        "item_status": item.get("status"),
        "current_step": item.get("current_step"),
        "attempt": attempt,
        "max_attempts": MAX_ARTIFACT_RECONCILE,
        "outstanding_action": action,
        "detail": detail,
        "instruction": (
            "不要运行 next；撤回本 action 的误改后重录，或为合法 implement.md/brief.md 修复补充 "
            "--doc-remediation-file。无法安全归因时用 blocked + artifact-drift 重录。"
        ),
        "summary": _format_summary(state, args),
    })


def cmd_record(args: argparse.Namespace) -> int:
    """记录 agent 执行结果。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_current_state(repo_root, args.run_id)
    except ValueError as exc:
        return _print({"status": "error", "reason": "record-failed", "message": str(exc)})
    schema_version = _schema_version(state)
    if schema_version == SCHEMA_VERSION and state.get("status") in {"preparing", "awaiting_input"}:
        if not args.action:
            return _print({"status": "error", "reason": "missing-action"})
        return _record_schema2_prepare(repo_root, path, state, args)
    if state.get("status") != "running":
        return _print({
            "status": "error",
            "reason": "auto-run-not-running",
            "run_status": state.get("status"),
            "summary": _format_summary(state, args),
        })

    task = _normalize_task_ref(repo_root, args.task) if args.task else None
    item = _find_record_item(state, task)
    if item is None:
        return _print({"status": "error", "reason": "no-running-task"})

    if not args.action:
        return _print({"status": "error", "reason": "missing-action", "message": "record 必须显式传入 --action"})
    expected_action = _outstanding_action_name(item)
    if expected_action is None:
        return _print({
            "status": "error",
            "reason": "no-outstanding-action",
            "message": "没有等待回写的 action；请先运行 next 获取下一步。",
            "task": item.get("task"),
            "current_step": item.get("current_step"),
        })
    if args.action != expected_action:
        return _print({
            "status": "error",
            "reason": "action-mismatch",
            "expected_action": expected_action,
            "actual_action": args.action,
            "task": item.get("task"),
            "current_step": item.get("current_step"),
        })

    action = args.action
    if args.repo_commit and action != "commit_only":
        return _print({
            "status": "error",
            "reason": "repo-commit-action-mismatch",
            "message": "--repo-commit 只允许用于 commit_only action",
            "task": item.get("task"),
            "action": action,
        })
    if action == "commit_only":
        repo_commits, repo_commit_error = _validated_repo_commits(repo_root, state, args.repo_commit or [])
        if repo_commit_error is not None:
            repo_commit_error.update({"task": item.get("task"), "action": action})
            return _print(repo_commit_error)
        merge_error = _merge_repo_commits(item, repo_commits)
        if merge_error is not None:
            merge_error.update({"task": item.get("task"), "action": action})
            return _print(merge_error)
        if args.result == "ok":
            primary_commit, primary_error = _resolved_primary_repo_commit(item, args.commit)
            if primary_error is not None:
                primary_error.update({"task": item.get("task"), "action": action})
                return _print(primary_error)
            args.commit = primary_commit
    if action == "review_open_questions":
        review_error = _record_open_questions_review(repo_root, item, args)
        if review_error is not None:
            review_error.update({"task": item.get("task"), "action": action})
            return _print(review_error)
        _write_state(path, state)
        return _print({
            "status": "recorded",
            "run_id": state.get("run_id"),
            "task": item.get("task"),
            "item_status": item.get("status"),
            "current_step": item.get("current_step"),
            "summary": _format_summary(state, args),
        })

    if action == "review_planning_readiness":
        review_error = _record_planning_readiness_review(repo_root, item, args)
        if review_error is not None:
            review_error.update({"task": item.get("task"), "action": action})
            return _print(review_error)
        _write_state(path, state)
        return _print({
            "status": "recorded",
            "run_id": state.get("run_id"),
            "task": item.get("task"),
            "item_status": item.get("status"),
            "current_step": item.get("current_step"),
            "summary": _format_summary(state, args),
        })

    if action == "confirm_brief":
        confirmation_error = _record_brief_confirmation(repo_root, item, args)
        if confirmation_error is not None:
            confirmation_error.update({"task": item.get("task"), "action": action})
            return _print(confirmation_error)
        _write_state(path, state)
        return _print({
            "status": "recorded",
            "run_id": state.get("run_id"),
            "task": item.get("task"),
            "item_status": item.get("status"),
            "current_step": item.get("current_step"),
            "summary": _format_summary(state, args),
        })

    if schema_version == SCHEMA_VERSION:
        protected_drifts = _consume_protected_baseline_drifts(repo_root, state, item, action)
        if protected_drifts:
            item["last_action"] = None
            _block_item(
                item,
                "protected-baseline-drift",
                "当前 action 期间 protected-retained 文件内容发生变化",
                {"files": protected_drifts},
            )
            _write_state(path, state)
            return _print({
                "status": "recorded",
                "run_id": state.get("run_id"),
                "task": item.get("task"),
                "item_status": item.get("status"),
                "current_step": item.get("current_step"),
                "summary": _format_summary(state, args),
            })
        doc_rebind = _consume_check_doc_remediation(
            repo_root,
            state,
            item,
            action,
            args.doc_remediation_file,
        )
        if isinstance(doc_rebind, dict) and doc_rebind.get("status") == "error":
            doc_rebind.update({"task": item.get("task"), "action": action})
            return _print(doc_rebind)
        artifact_rebind = _consume_pending_artifact_decision(repo_root, state, item)
        if isinstance(artifact_rebind, dict) and artifact_rebind.get("unauthorized"):
            return _record_artifact_drift(
                path,
                state,
                item,
                action,
                args,
                "planning/handoff artifacts 的变化未被当前 AI 决策文件范围授权",
                artifact_rebind,
            )
        if action in CHECK_ACTIONS and doc_rebind is None and artifact_rebind is None:
            last_action = item.get("last_action")
            previous = last_action.get("artifact_sha256") if isinstance(last_action, dict) else None
            if isinstance(previous, dict):
                current = _task_artifact_hashes(repo_root, item)
                changed = sorted(
                    key
                    for key in set(previous) | set(current)
                    if previous.get(key) != current.get(key)
                )
                if changed:
                    return _record_artifact_drift(
                        path,
                        state,
                        item,
                        action,
                        args,
                        "Check action 发出后 planning/handoff 文件发生未声明变化",
                        {"changed_files": changed},
                    )
        if item.get("planning_sha256"):
            try:
                planning_hash, handoff_hash = _current_artifact_hashes(repo_root, item)
            except OSError as exc:
                planning_hash, handoff_hash = "", ""
                drift_detail = {"error": str(exc)}
            else:
                drift_detail = {
                    "expected_planning_sha256": item.get("planning_sha256"),
                    "actual_planning_sha256": planning_hash,
                    "expected_handoff_sha256": item.get("handoff_sha256"),
                    "actual_handoff_sha256": handoff_hash,
                }
            if (
                planning_hash != item.get("planning_sha256")
                or handoff_hash != item.get("handoff_sha256")
            ):
                return _record_artifact_drift(
                    path,
                    state,
                    item,
                    action,
                    args,
                    "action 发出后 planning/handoff artifacts 发生未授权变化",
                    drift_detail,
                )
        conflicts = _protected_path_conflicts(state, args.files)
        if conflicts:
            item["last_action"] = None
            _block_item(
                item,
                "protected-path-conflict",
                "当前 action 涉及 protected-retained 文件",
                {"files": conflicts},
            )
            _write_state(path, state)
            return _print({
                "status": "recorded",
                "run_id": state.get("run_id"),
                "task": item.get("task"),
                "item_status": item.get("status"),
                "current_step": item.get("current_step"),
                "summary": _format_summary(state, args),
            })

    check_error = _check_record_error(state, item, action, args)
    if check_error is not None:
        check_error.update({"task": item.get("task"), "action": action})
        return _print(check_error)

    _record_check_result(state, item, action, args)
    if action in CHECK_ACTIONS:
        item.setdefault("attempts", {})["artifact_reconcile"] = 0
    item["last_action"] = None
    if args.result == "ok":
        _advance_after_ok(item, action, args)
    elif args.result == "failed":
        _record_failure(item, action, args)
    else:
        _block_item(item, args.failure_type or "blocked", args.summary or "agent 标记 blocked")
        if action == "commit_only":
            _append_item_decision(
                item,
                "task_skipped",
                args.summary or "commit-only 预检不安全，当前任务已跳过，队列继续",
                {"failure_type": args.failure_type, "files": args.files or []},
            )

    _write_state(path, state)
    output = {
        "status": "recorded",
        "run_id": state.get("run_id"),
        "task": item.get("task"),
        "item_status": item.get("status"),
        "current_step": item.get("current_step"),
        "commit": item.get("commit"),
        "commits": item.get("commits", []),
        "summary": _format_summary(state, args),
    }
    if getattr(args, "verbose", False):
        output["item"] = item
    return _print(output)


def cmd_status(args: argparse.Namespace) -> int:
    """输出 auto run 状态。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_current_state(repo_root, args.run_id)
    except ValueError as exc:
        message = str(exc)
        invalid_runtime = any(
            marker in message
            for marker in (" corrupt:", " io_error:", "pointer corrupt:", "pointer io_error:")
        )
        return _print({
            "status": "ok",
            "run_status": "invalid-current-run" if invalid_runtime else "no-current-run",
            "reason": "runtime-state-invalid" if invalid_runtime else "status-list",
            "message": message,
            "runs": _recent_run_summaries(repo_root),
        })
    return _print({"status": "ok", "path": _rel_path(repo_root, path), **_format_summary(state, args)})


def cmd_stop(args: argparse.Namespace) -> int:
    """停止 auto run。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_current_state(repo_root, args.run_id)
    except ValueError as exc:
        return _print({"status": "error", "reason": "stop-failed", "message": str(exc)})
    state["status"] = "stopped"
    state["stop_reason"] = args.reason
    _write_state(path, state)
    _clear_pointer_if_current(repo_root, str(state.get("run_id") or ""))
    return _print({"status": "stopped", "run_id": state.get("run_id"), "path": _rel_path(repo_root, path), "reason": args.reason})


def cmd_decide(args: argparse.Namespace) -> int:
    """记录 AI 在 run 授权边界内作出的低/中风险决策。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "error", "reason": "not-trellis-project"})
    try:
        path, state = _load_current_state(repo_root, args.run_id)
    except ValueError as exc:
        return _print({"status": "error", "reason": "decide-failed", "message": str(exc)})
    if _schema_version(state) != SCHEMA_VERSION:
        return _print({"status": "error", "reason": "decision-log-requires-schema-2"})
    if state.get("status") not in ACTIVE_RUN_STATUSES:
        return _print({"status": "error", "reason": "auto-run-not-active", "run_status": state.get("status")})
    task_ref = _normalize_task_ref(repo_root, args.task)
    item = next((entry for entry in _queue_items(state) if entry.get("task") == task_ref), None)
    if item is None:
        return _print({"status": "error", "reason": "decision-task-not-in-queue", "task": task_ref})
    if isinstance(item.get("pending_artifact_decision"), dict):
        return _print({
            "status": "error",
            "reason": "decision-artifact-rebind-pending",
            "message": "上一条决策仍等待当前 action record 消费",
            "decision_id": item["pending_artifact_decision"].get("decision_id"),
        })

    decision_files = [_normalize_record_file(value) for value in args.file or []]
    conflicts = _protected_path_conflicts(state, decision_files)
    if conflicts:
        return _print({
            "status": "error",
            "reason": "protected-path-conflict",
            "files": conflicts,
        })

    task_dir = _task_dir(repo_root, task_ref)
    planning_hash = ""
    handoff_hash = ""
    if (task_dir / "prd.md").is_file():
        planning_hash, handoff_hash = _current_artifact_hashes(repo_root, item)
    try:
        event = append_decision(
            task_dir,
            run_id=str(state.get("run_id") or ""),
            topic=args.topic,
            options=args.option,
            choice=args.choice,
            summary=args.summary,
            evidence=args.evidence or [],
            risk=args.risk,
            confidence=args.confidence,
            requirements=args.requirement or [],
            files=decision_files,
            planning_sha256=planning_hash,
            handoff_sha256=handoff_hash,
            verification=args.verification or "",
        )
    except DecisionLogError as exc:
        return _print({"status": "error", "reason": "decision-log-error", "message": str(exc)})
    item["decision_count"] = int(item.get("decision_count") or 0) + 1
    decision_ids = item.get("decision_ids")
    if not isinstance(decision_ids, list):
        decision_ids = []
    decision_ids.append(event["decision_id"])
    item["decision_ids"] = decision_ids
    _append_item_decision(item, "ai_decision", f"{event['decision_id']}: {event['topic']} -> {event['choice']}", {
        "decision_id": event["decision_id"],
        "risk": event["risk"],
        "confidence": event["confidence"],
        "files": event["files"],
    })
    if state.get("status") == "running" and planning_hash:
        item["pending_artifact_decision"] = {
            "decision_id": event["decision_id"],
            "files": event["files"],
            "artifact_sha256": _task_artifact_hashes(repo_root, item),
            "planning_sha256": planning_hash,
            "handoff_sha256": handoff_hash,
            "recorded_at": event["recorded_at"],
        }
    _write_state(path, state)
    return _print({
        "status": "decided",
        "run_id": state.get("run_id"),
        "task": task_ref,
        "decision": event,
        "artifact_rebind_pending": bool(item.get("pending_artifact_decision")),
        "manifest_revision": state.get("manifest_revision"),
        "manifest_sha256": state.get("manifest_sha256"),
    })


def build_parser() -> argparse.ArgumentParser:
    """构造命令行解析器。"""
    parser = argparse.ArgumentParser(description="Trellis auto loop runner.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    start = subparsers.add_parser("start", help="create an auto loop run")
    start.add_argument("--tasks", nargs="+", required=True)
    start.add_argument("--run-id")
    start.add_argument("--profile", choices=(DEFAULT_PROFILE,), default=DEFAULT_PROFILE)
    start.add_argument("--check-depth", choices=sorted(VALID_CHECK_DEPTHS), default="auto")
    start.add_argument("--route-implement", choices=sorted(VALID_IMPLEMENT_ROUTES))
    start.add_argument("--route-check", choices=sorted(VALID_CHECK_ROUTES))
    start.add_argument("--depends-on", action="append", default=[])
    start.add_argument("--force", action="store_true")
    start.add_argument("--verbose", action="store_true")
    start.set_defaults(func=cmd_start)

    for name, func in (("resume", cmd_resume), ("next", cmd_next), ("status", cmd_status)):
        sub = subparsers.add_parser(name)
        sub.add_argument("--run-id")
        sub.add_argument("--verbose", action="store_true")
        sub.set_defaults(func=func)

    record = subparsers.add_parser("record")
    record.add_argument("--run-id")
    record.add_argument("--task")
    record.add_argument("--action")
    record.add_argument("--result", choices=("ok", "failed", "blocked"), required=True)
    record.add_argument("--failure-type")
    record.add_argument("--summary")
    record.add_argument("--files", nargs="*")
    record.add_argument("--doc-remediation-file", action="append", default=[])
    record.add_argument("--retained-files", nargs="*")
    record.add_argument("--commit")
    record.add_argument("--repo-commit", action="append", default=[])
    record.add_argument("--commit-message")
    record.add_argument("--snapshot-commit")
    record.add_argument("--route-mode")
    record.add_argument("--route-source")
    record.add_argument("--effective-check-depth", choices=("light", "full"))
    record.add_argument("--check-depth-reason")
    record.add_argument("--review-verdict", choices=("resolved", "blocking", "ambiguous"))
    record.add_argument("--readiness-verdict", choices=("ready", "repairable", "blocking", "ambiguous"))
    record.add_argument("--owned-dirty", action="append", default=[])
    record.add_argument("--protected-retained", action="append", default=[])
    record.add_argument("--verbose", action="store_true")
    record.set_defaults(func=cmd_record)

    retry = subparsers.add_parser("retry-blocked")
    retry.add_argument("--run-id")
    retry.add_argument("--task")
    retry.add_argument("--route-implement", choices=sorted(VALID_IMPLEMENT_ROUTES))
    retry.add_argument("--route-check", choices=sorted(VALID_CHECK_ROUTES))
    retry.add_argument("--check-depth", choices=sorted(VALID_CHECK_DEPTHS))
    retry.add_argument("--all", action="store_true")
    retry.add_argument("--verbose", action="store_true")
    retry.set_defaults(func=cmd_retry_blocked)

    stop = subparsers.add_parser("stop")
    stop.add_argument("--run-id")
    stop.add_argument("--reason")
    stop.set_defaults(func=cmd_stop)

    decide = subparsers.add_parser("decide")
    decide.add_argument("--run-id")
    decide.add_argument("--task", required=True)
    decide.add_argument("--topic", required=True)
    decide.add_argument("--option", action="append", required=True)
    decide.add_argument("--choice", required=True)
    decide.add_argument("--summary", required=True)
    decide.add_argument("--evidence", action="append", default=[])
    decide.add_argument("--risk", choices=("low", "medium"), required=True)
    decide.add_argument("--confidence", choices=("low", "medium", "high"), required=True)
    decide.add_argument("--requirement", action="append", default=[])
    decide.add_argument("--file", action="append", default=[])
    decide.add_argument("--verification")
    decide.set_defaults(func=cmd_decide)

    return parser


def main() -> int:
    """脚本入口。"""
    parser = build_parser()
    args = parser.parse_args()
    try:
        return args.func(args)
    except ValueError as exc:
        return _print({"status": "error", "reason": "invalid-input", "message": str(exc)})
    except OSError as exc:
        return _print({"status": "error", "reason": "runtime-io-error", "message": str(exc)})


if __name__ == "__main__":
    sys.exit(main())
