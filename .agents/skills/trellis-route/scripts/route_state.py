#!/usr/bin/env python3
"""读写当前 session runtime 文件里的 Trellis route 决策。"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


VALID_SOURCES = {"trellis-route", "numbered-fallback", "route-prefs", "auto-loop"}
VALID_MODES = {
    "implement": {"inline", "subagent"},
    "check": {"check-all-inline", "check-all-subagent"},
}
LEGACY_MODE_ALIASES = {
    "check": {
        "check-inline": "check-all-inline",
        "check-subagent": "check-all-subagent",
    },
}
PREF_MODES = {
    "implement": {"inline", "subagent"},
    "check": {"check-all-inline", "check-all-subagent"},
}


def _repo_root() -> Path | None:
    """从当前目录向上查找 Trellis 项目根目录。"""
    current = Path.cwd().resolve()
    while True:
        if (current / ".trellis").is_dir():
            return current
        if current == current.parent:
            return None
        current = current.parent


def _utc_now() -> str:
    """返回秒级 UTC 时间字符串。"""
    return (
        datetime.now(timezone.utc)
        .replace(microsecond=0)
        .isoformat()
        .replace("+00:00", "Z")
    )


def _read_json_result(path: Path) -> dict[str, Any]:
    """读取 runtime JSON，并区分缺失、损坏和 I/O 错误。"""
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
    """兼容非关键扫描；仅返回成功解析的 JSON 对象。"""
    result = _read_json_result(path)
    data = result.get("data")
    return data if isinstance(data, dict) else {}


def _write_json(path: Path, data: dict[str, Any]) -> None:
    """使用同目录临时文件原子写入 runtime 状态。"""
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


def _print(data: dict[str, Any]) -> int:
    """输出给 AI 读取的紧凑 JSON。"""
    print(json.dumps(data, ensure_ascii=False, sort_keys=True))
    return 0


def _output(args: argparse.Namespace, data: dict[str, Any], verbose: dict[str, Any] | None = None) -> int:
    """按默认精简模式或详细模式输出 JSON。"""
    if getattr(args, "verbose", False) and verbose:
        data = {**data, **verbose}
    return _print(data)


def _decision_summary(decision: dict[str, Any]) -> dict[str, Any]:
    """提取默认输出需要的最小 route 决策字段。"""
    return {
        "task": decision.get("task"),
        "mode": decision.get("mode"),
        "source": decision.get("source"),
    }


def _normalize_mode(target: str, mode: Any) -> str | None:
    """把历史 route mode 归一为当前统一入口，非法值返回 None。"""
    aliases = LEGACY_MODE_ALIASES.get(target, {})
    normalized = aliases.get(mode, mode)
    return str(normalized) if normalized in VALID_MODES[target] else None


def _current_task(repo_root: Path) -> tuple[str | None, str | None, str | None]:
    """通过 task.py current --source 获取当前任务和 session key。"""
    result = subprocess.run(
        ["python3", str(repo_root / ".trellis/scripts/task.py"), "current", "--source"],
        cwd=repo_root,
        check=False,
        text=True,
        capture_output=True,
    )
    current = None
    source = None
    for line in result.stdout.splitlines():
        if line.startswith("Current task: "):
            value = line.split(": ", 1)[1].strip()
            current = None if value == "(none)" else value
        elif line.startswith("Source: "):
            source = line.split(": ", 1)[1].strip()
    context_key = None
    if source and source.startswith(("session:", "session-fallback:")):
        context_key = source.split(":", 1)[1].strip()
    return current, source, context_key


def _session_path(repo_root: Path, context_key: str) -> Path:
    """返回当前 session runtime 文件路径。"""
    return repo_root / ".trellis/.runtime/sessions" / f"{context_key}.json"


def _rel_path(repo_root: Path, path: Path) -> str:
    """尽量输出相对项目根的路径，便于用户阅读。"""
    try:
        return path.relative_to(repo_root).as_posix()
    except ValueError:
        return str(path)


def _pref_path(repo_root: Path) -> Path:
    """返回个人 route 默认配置文件路径。"""
    return repo_root / ".trellis/.route-prefs.tmp"


def _auto_loop_dir(repo_root: Path) -> Path:
    """返回 auto-loop runtime 目录。"""
    return repo_root / ".trellis/.runtime/auto-loop"


def _auto_loop_pointer(repo_root: Path) -> Path:
    """返回当前 auto-loop run 指针文件。"""
    return _auto_loop_dir(repo_root) / "current.json"


def _read_prefs(repo_root: Path) -> dict[str, str]:
    """读取个人 route 默认配置，只保留合法 key-value。"""
    path = _pref_path(repo_root)
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
        if key in PREF_MODES:
            normalized = _normalize_mode(key, value)
            if normalized in PREF_MODES[key]:
                prefs[key] = normalized
    return prefs


def _running_auto_run_paths(repo_root: Path) -> list[Path]:
    """扫描当前项目内所有 running auto-loop run。"""
    running: list[Path] = []
    for path in sorted(_auto_loop_dir(repo_root).glob("auto-*.json")):
        state = _read_json(path)
        if state.get("status") == "running":
            running.append(path)
    return running


def _auto_state_path(repo_root: Path, run_id: Any) -> Path | None:
    """把 run id 转成状态文件路径，非法值返回 None。"""
    if not isinstance(run_id, str) or not run_id.strip():
        return None
    return _auto_loop_dir(repo_root) / f"{run_id.strip()}.json"


def _run_contains_task(state: dict[str, Any], current_task: str) -> bool:
    """判断当前任务是否属于 run 的未完成队列。"""
    queue = state.get("queue")
    if not isinstance(queue, list):
        return False
    normalized = current_task.replace("\\", "/")
    for item in queue:
        if not isinstance(item, dict) or item.get("status") not in {"pending", "running"}:
            continue
        task = item.get("task")
        if isinstance(task, str) and task.replace("\\", "/") == normalized:
            return True
    return False


def _auto_route_mode(
    repo_root: Path,
    context_key: str,
    current_task: str,
    target: str,
) -> tuple[str | None, Path | None, str | None]:
    """读取当前 running auto-loop run 的临时 route 授权。

    auto 授权低于个人 `.route-prefs.tmp`，只在当前 session runtime 绑定了
    running `current_auto_run`，或全局 current 指针能指向 running run 时生效。
    如果这些指针 stale，则忽略 stale pointer，并 fallback 到唯一 running run。
    """
    session_path = _session_path(repo_root, context_key)
    session_result = _read_json_result(session_path)
    if session_result["status"] in {"corrupt", "io_error"}:
        return None, session_path, f"session-runtime-{session_result['status']}"
    session = session_result["data"] if isinstance(session_result.get("data"), dict) else {}
    candidate_paths: list[Path] = []
    stale_paths: list[Path] = []

    pointer_result = _read_json_result(_auto_loop_pointer(repo_root))
    pointer = pointer_result["data"] if isinstance(pointer_result.get("data"), dict) else {}
    for source, run_id in (
        ("session", session.get("current_auto_run")),
        ("pointer", pointer.get("run_id")),
    ):
        path = _auto_state_path(repo_root, run_id)
        if path is None:
            continue
        result = _read_json_result(path)
        if source == "session" and result["status"] in {"corrupt", "io_error"}:
            return None, path, f"session-auto-run-{result['status']}"
        state = result["data"] if isinstance(result.get("data"), dict) else {}
        if state.get("status") == "running":
            candidate_paths.append(path)
        else:
            stale_paths.append(path)

    if not candidate_paths:
        candidate_paths = _running_auto_run_paths(repo_root)
    unique_paths = []
    seen: set[str] = set()
    for path in candidate_paths:
        key = str(path)
        if key in seen:
            continue
        seen.add(key)
        unique_paths.append(path)
    if len(unique_paths) != 1:
        reason = "no-unique-auto-run"
        if stale_paths and not unique_paths:
            reason = "stale-auto-run-pointer"
        return None, stale_paths[0] if stale_paths else None, reason

    path = unique_paths[0]
    state = _read_json(path)
    if not _run_contains_task(state, current_task):
        return None, path, "auto-run-task-mismatch"

    auth = state.get("route_authorization")
    if not isinstance(auth, dict):
        return None, path, "no-route-authorization"

    mode = _normalize_mode(target, auth.get(target))
    if mode in PREF_MODES[target]:
        return mode, path, None
    return None, path, "invalid-auto-route-mode"


def _write_prefs(repo_root: Path, prefs: dict[str, str]) -> None:
    """写入个人 route 默认配置；为空则删除文件。"""
    path = _pref_path(repo_root)
    valid = {
        target: value
        for target, value in prefs.items()
        if target in PREF_MODES and value in PREF_MODES[target]
    }
    if not valid:
        try:
            path.unlink()
        except FileNotFoundError:
            pass
        return

    lines = []
    for target in ("implement", "check"):
        value = valid.get(target)
        if value:
            lines.append(f"{target}={value}")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            handle.write("\n".join(lines) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        try:
            temp_path.unlink()
        except FileNotFoundError:
            pass
        raise


def _normalized_decision(decision: Any, target: str, current_task: str) -> dict[str, Any] | None:
    """校验并归一化 runtime route 决策，非法时返回 None。"""
    if not isinstance(decision, dict):
        return None
    mode = _normalize_mode(target, decision.get("mode"))
    if not (
        decision.get("target") == target
        and mode is not None
        and decision.get("source") in VALID_SOURCES
        and decision.get("scope") == "task"
        and decision.get("task") == current_task
    ):
        return None
    normalized = dict(decision)
    normalized["mode"] = mode
    return normalized


def _decision(target: str, mode: str, source: str, current_task: str) -> dict[str, str]:
    """构造标准 route 决策对象。"""
    return {
        "target": target,
        "mode": mode,
        "source": source,
        "scope": "task",
        "task": current_task,
        "decided_at": _utc_now(),
    }


def _write_runtime_decision(
    repo_root: Path,
    context_key: str,
    current_task: str,
    target: str,
    mode: str,
    source: str,
) -> tuple[Path, dict[str, str]]:
    """写入 session runtime 文件中的 route_decisions 字段。"""
    path = _session_path(repo_root, context_key)
    context_result = _read_json_result(path)
    if context_result["status"] in {"corrupt", "io_error"}:
        raise ValueError(f"session-runtime-{context_result['status']}")
    context = context_result["data"] if isinstance(context_result.get("data"), dict) else {}
    now = _utc_now()
    context.setdefault("platform", context_key.split("_", 1)[0] if "_" in context_key else "session")
    context["last_seen_at"] = now
    context["current_task"] = current_task
    context.setdefault("current_run", None)
    context["route_state_version"] = 1
    decisions = context.get("route_decisions")
    if not isinstance(decisions, dict):
        decisions = {}
    # 同一 AI session 可能连续处理多个 Trellis 任务。写入新任务的任一路由时，
    # 先丢弃旧任务的 runtime 决策，避免后续 check 阶段绕过 helper 时误复用上个任务的选择。
    decisions = {
        key: value
        for key, value in decisions.items()
        if isinstance(value, dict) and value.get("task") == current_task
    }
    decision = _decision(target, mode, source, current_task)
    decision["decided_at"] = now
    decisions[target] = decision
    context["route_decisions"] = decisions
    _write_json(path, context)
    return path, decision


def _current_context_or_miss(repo_root: Path) -> tuple[str | None, str | None, str | None, dict[str, Any] | None]:
    """解析当前任务和 session key；失败时返回可直接输出的 miss 对象。"""
    current_task, source, context_key = _current_task(repo_root)
    if not current_task:
        return None, source, None, {"status": "miss", "reason": "no-current-task", "source": source}
    if not context_key:
        return current_task, source, None, {"status": "miss", "reason": "no-session-context", "source": source}
    return current_task, source, context_key, None


def read_runtime(args: argparse.Namespace) -> int:
    """只读取并校验当前 target 的 runtime route 决策。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "miss", "reason": "not-trellis-project"})

    current_task, _, context_key, miss = _current_context_or_miss(repo_root)
    if miss:
        return _print(miss)
    assert current_task is not None and context_key is not None

    path = _session_path(repo_root, context_key)
    context_result = _read_json_result(path)
    if context_result["status"] in {"corrupt", "io_error"}:
        return _output(
            args,
            {"status": "miss", "reason": f"session-runtime-{context_result['status']}"},
            {"path": _rel_path(repo_root, path), "error": context_result.get("error")},
        )
    context = context_result["data"] if isinstance(context_result.get("data"), dict) else {}
    decision = context.get("route_decisions", {}).get(args.target)
    normalized = _normalized_decision(decision, args.target, current_task)
    if normalized is not None:
        return _output(
            args,
            {
                "status": "hit",
                **_decision_summary(normalized),
            },
            {
                "decision": normalized,
                "path": _rel_path(repo_root, path),
                "context_key": context_key,
                "task": current_task,
            }
        )

    return _output(
        args,
        {
            "status": "miss",
            "reason": "no-valid-decision",
        },
        {
            "path": _rel_path(repo_root, path),
            "context_key": context_key,
            "task": current_task,
        }
    )


def resolve_route(args: argparse.Namespace) -> int:
    """按 runtime → prefs → auto-loop 的优先级解析 route 决策。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "miss", "reason": "not-trellis-project"})

    current_task, _, context_key, miss = _current_context_or_miss(repo_root)
    if miss:
        return _print(miss)
    assert current_task is not None and context_key is not None

    path = _session_path(repo_root, context_key)
    context_result = _read_json_result(path)
    if context_result["status"] in {"corrupt", "io_error"}:
        return _output(
            args,
            {"status": "miss", "reason": f"session-runtime-{context_result['status']}"},
            {"path": _rel_path(repo_root, path), "error": context_result.get("error")},
        )
    context = context_result["data"] if isinstance(context_result.get("data"), dict) else {}
    decision = context.get("route_decisions", {}).get(args.target)
    normalized = _normalized_decision(decision, args.target, current_task)
    if normalized is not None:
        written_path = path
        if normalized.get("mode") != decision.get("mode"):
            written_path, normalized = _write_runtime_decision(
                repo_root,
                context_key,
                current_task,
                args.target,
                str(normalized["mode"]),
                str(normalized["source"]),
            )
        return _output(
            args,
            {
                "status": "hit",
                "origin": "runtime",
                **_decision_summary(normalized),
            },
            {
                "decision": normalized,
                "path": _rel_path(repo_root, written_path),
                "context_key": context_key,
                "task": current_task,
                "normalized_legacy_mode": normalized.get("mode") != decision.get("mode"),
            }
        )

    prefs = _read_prefs(repo_root)
    pref_mode = prefs.get(args.target)
    if pref_mode in PREF_MODES[args.target]:
        written_path, pref_decision = _write_runtime_decision(
            repo_root,
            context_key,
            current_task,
            args.target,
            pref_mode,
            "route-prefs",
        )
        return _output(
            args,
            {
                "status": "hit",
                "origin": "route-prefs",
                **_decision_summary(pref_decision),
            },
            {
                "decision": pref_decision,
                "path": _rel_path(repo_root, written_path),
                "pref_path": _rel_path(repo_root, _pref_path(repo_root)),
                "context_key": context_key,
                "task": current_task,
                "wrote_runtime": True,
            }
        )

    auto_mode, auto_path, auto_reason = _auto_route_mode(
        repo_root,
        context_key,
        current_task,
        args.target,
    )
    if auto_mode in PREF_MODES[args.target]:
        written_path, auto_decision = _write_runtime_decision(
            repo_root,
            context_key,
            current_task,
            args.target,
            auto_mode,
            "auto-loop",
        )
        return _output(
            args,
            {
                "status": "hit",
                "origin": "auto-loop",
                **_decision_summary(auto_decision),
            },
            {
                "decision": auto_decision,
                "path": _rel_path(repo_root, written_path),
                "auto_path": _rel_path(repo_root, auto_path) if auto_path else None,
                "pref_path": _rel_path(repo_root, _pref_path(repo_root)),
                "context_key": context_key,
                "task": current_task,
                "wrote_runtime": True,
            }
        )

    return _output(
        args,
        {
            "status": "miss",
            "reason": "no-valid-decision-pref-or-auto",
        },
        {
            "path": _rel_path(repo_root, path),
            "pref_path": _rel_path(repo_root, _pref_path(repo_root)),
            "auto_reason": auto_reason,
            "auto_path": _rel_path(repo_root, auto_path) if auto_path else None,
            "context_key": context_key,
            "task": current_task,
        }
    )


def write_route(args: argparse.Namespace) -> int:
    """写入当前 target 的 route 决策并保留 session 文件的其他字段。"""
    if args.source not in VALID_SOURCES:
        return _print({"status": "error", "reason": "invalid-source", "source": args.source})
    mode = _normalize_mode(args.target, args.mode)
    if mode is None:
        return _print(
            {
                "status": "error",
                "reason": "invalid-mode",
                "target": args.target,
                "mode": args.mode,
            }
        )

    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "skipped", "reason": "not-trellis-project"})

    if args.save_pref:
        if mode not in PREF_MODES[args.target]:
            return _print(
                {
                    "status": "error",
                    "reason": "mode-cannot-be-saved-as-pref",
                    "target": args.target,
                    "mode": args.mode,
                }
            )
        prefs = _read_prefs(repo_root)
        prefs[args.target] = mode
        _write_prefs(repo_root, prefs)

    current_task, source, context_key = _current_task(repo_root)
    if not current_task:
        return _print({"status": "skipped", "reason": "no-current-task", "source": source})
    if not context_key:
        return _print({"status": "skipped", "reason": "no-session-context", "source": source})

    try:
        path, decision = _write_runtime_decision(
            repo_root,
            context_key,
            current_task,
            args.target,
            mode,
            args.source,
        )
    except (OSError, ValueError) as exc:
        return _print({"status": "error", "reason": "runtime-write-failed", "message": str(exc)})
    return _output(
        args,
        {
            "status": "written",
            **_decision_summary(decision),
        },
        {
            "decision": decision,
            "path": _rel_path(repo_root, path),
            "pref_path": _rel_path(repo_root, _pref_path(repo_root)) if args.save_pref else None,
            "context_key": context_key,
            "task": current_task,
            "saved_pref": bool(args.save_pref),
        }
    )


def read_pref(args: argparse.Namespace) -> int:
    """读取不依赖任务或 session 的个人 route 偏好。

    Args:
        args: 包含 target 和 verbose 的命令行参数。

    Returns:
        命令退出码。
    """
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "miss", "reason": "not-trellis-project"})
    mode = _read_prefs(repo_root).get(args.target)
    if mode not in PREF_MODES[args.target]:
        return _output(
            args,
            {"status": "miss", "reason": "no-valid-pref", "target": args.target},
            {"pref_path": _rel_path(repo_root, _pref_path(repo_root))},
        )
    return _output(
        args,
        {
            "status": "hit",
            "target": args.target,
            "mode": mode,
            "source": "route-prefs",
        },
        {"pref_path": _rel_path(repo_root, _pref_path(repo_root))},
    )


def write_pref(args: argparse.Namespace) -> int:
    """写入不依赖任务或 session 的个人 route 偏好。

    Args:
        args: 包含 target、mode 和 verbose 的命令行参数。

    Returns:
        命令退出码。
    """
    mode = _normalize_mode(args.target, args.mode)
    if mode not in PREF_MODES[args.target]:
        return _print(
            {
                "status": "error",
                "reason": "invalid-mode",
                "target": args.target,
                "mode": args.mode,
            }
        )
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "skipped", "reason": "not-trellis-project"})
    prefs = _read_prefs(repo_root)
    prefs[args.target] = mode
    _write_prefs(repo_root, prefs)
    return _output(
        args,
        {
            "status": "written",
            "target": args.target,
            "mode": mode,
            "source": "route-prefs",
        },
        {"pref_path": _rel_path(repo_root, _pref_path(repo_root))},
    )


def clear_pref(args: argparse.Namespace) -> int:
    """清除当前 target 的个人默认 route 配置。"""
    repo_root = _repo_root()
    if repo_root is None:
        return _print({"status": "skipped", "reason": "not-trellis-project"})
    prefs = _read_prefs(repo_root)
    existed = args.target in prefs
    prefs.pop(args.target, None)
    _write_prefs(repo_root, prefs)
    return _output(
        args,
        {
            "status": "cleared",
            "target": args.target,
        },
        {
            "existed": existed,
            "pref_path": _rel_path(repo_root, _pref_path(repo_root)),
        }
    )


def build_parser() -> argparse.ArgumentParser:
    """构造命令行参数解析器。"""
    parser = argparse.ArgumentParser(description="Read/write Trellis route state.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    resolve_parser = subparsers.add_parser("resolve", help="resolve route from runtime then prefs")
    resolve_parser.add_argument("--target", choices=sorted(VALID_MODES), required=True)
    resolve_parser.add_argument("--verbose", action="store_true", help="include diagnostic paths and session metadata")
    resolve_parser.set_defaults(func=resolve_route)

    read_parser = subparsers.add_parser("read-runtime", help="read a runtime route decision")
    read_parser.add_argument("--target", choices=sorted(VALID_MODES), required=True)
    read_parser.add_argument("--verbose", action="store_true", help="include diagnostic paths and session metadata")
    read_parser.set_defaults(func=read_runtime)

    write_parser = subparsers.add_parser("write", help="write a route decision")
    write_parser.add_argument("--target", choices=sorted(VALID_MODES), required=True)
    write_parser.add_argument("--mode", required=True)
    write_parser.add_argument("--source", choices=sorted(VALID_SOURCES), required=True)
    write_parser.add_argument("--save-pref", action="store_true")
    write_parser.add_argument("--verbose", action="store_true", help="include diagnostic paths and session metadata")
    write_parser.set_defaults(func=write_route)

    read_pref_parser = subparsers.add_parser("read-pref", help="read a personal route preference")
    read_pref_parser.add_argument("--target", choices=sorted(PREF_MODES), required=True)
    read_pref_parser.add_argument("--verbose", action="store_true", help="include preference path metadata")
    read_pref_parser.set_defaults(func=read_pref)

    write_pref_parser = subparsers.add_parser("write-pref", help="write a personal route preference")
    write_pref_parser.add_argument("--target", choices=sorted(PREF_MODES), required=True)
    write_pref_parser.add_argument("--mode", required=True)
    write_pref_parser.add_argument("--verbose", action="store_true", help="include preference path metadata")
    write_pref_parser.set_defaults(func=write_pref)

    clear_parser = subparsers.add_parser("clear-pref", help="clear a personal route preference")
    clear_parser.add_argument("--target", choices=sorted(PREF_MODES), required=True)
    clear_parser.add_argument("--verbose", action="store_true", help="include diagnostic paths and preference metadata")
    clear_parser.set_defaults(func=clear_pref)

    return parser


def main() -> int:
    """脚本入口。"""
    parser = build_parser()
    args = parser.parse_args()
    try:
        return args.func(args)
    except (OSError, ValueError) as exc:
        return _print({"status": "error", "reason": "runtime-io-error", "message": str(exc)})


if __name__ == "__main__":
    sys.exit(main())
