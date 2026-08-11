#!/usr/bin/env python3
"""Trellis 任务 AI 决策日志与归档审查工具。"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

SCHEMA_VERSION = 1
VALID_RISKS = {"low", "medium"}
VALID_CONFIDENCES = {"low", "medium", "high"}
VALID_VERDICTS = {"accepted", "changes-requested"}


class DecisionLogError(ValueError):
    """表示决策日志损坏或调用参数违反审计契约。"""


def _utc_now() -> str:
    """返回秒级 UTC 时间。

    Returns:
        ISO 8601 UTC 时间字符串。
    """
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def _repo_root() -> Path | None:
    """从当前目录向上查找 Trellis 项目根。

    Returns:
        Trellis 项目根；未找到时返回 None。
    """
    current = Path.cwd().resolve()
    while True:
        if (current / ".trellis").is_dir():
            return current
        if current == current.parent:
            return None
        current = current.parent


def _resolve_task_dir(repo_root: Path, task: str) -> Path:
    """把任务引用解析为活动任务目录。

    Args:
        repo_root: Trellis 项目根。
        task: 任务名、相对路径或绝对路径。

    Returns:
        已存在的任务目录绝对路径。

    Raises:
        DecisionLogError: 任务不存在或位于项目外。
    """
    raw = task.strip()
    candidate = Path(raw)
    candidates = [candidate] if candidate.is_absolute() else [
        repo_root / raw,
        repo_root / ".trellis/tasks" / raw,
    ]
    for path in candidates:
        try:
            resolved = path.resolve()
            resolved.relative_to((repo_root / ".trellis/tasks").resolve())
        except (OSError, ValueError):
            continue
        if resolved.is_dir():
            return resolved
    raise DecisionLogError(f"任务不存在或不在活动任务目录:{task}")


def _validate_event(event: Any, line_number: int) -> dict[str, Any]:
    """校验单条 JSONL 事件。

    Args:
        event: 待校验的 JSON 值。
        line_number: 原文件行号。

    Returns:
        校验后的事件对象。

    Raises:
        DecisionLogError: 事件结构不合法。
    """
    if not isinstance(event, dict):
        raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行不是对象")
    if event.get("schema_version") != SCHEMA_VERSION:
        raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 schema_version 不受支持")
    event_type = event.get("event")
    if event_type == "decision":
        required_strings = (
            "decision_id",
            "run_id",
            "topic",
            "choice",
            "summary",
            "risk",
            "confidence",
            "recorded_at",
        )
        if any(not isinstance(event.get(key), str) or not event.get(key) for key in required_strings):
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 decision 字段不完整")
        if event["risk"] not in VALID_RISKS:
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 risk 不合法")
        if event["confidence"] not in VALID_CONFIDENCES:
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 confidence 不合法")
        for key in ("options", "evidence", "requirements", "files"):
            if not isinstance(event.get(key), list):
                raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 {key} 不是数组")
    elif event_type == "review":
        if event.get("verdict") not in VALID_VERDICTS:
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 verdict 不合法")
        if not isinstance(event.get("decision_digest"), str) or not event.get("decision_digest"):
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行缺少 decision_digest")
        if not isinstance(event.get("decision_ids"), list):
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 decision_ids 不是数组")
        if not isinstance(event.get("reviewed_at"), str) or not event.get("reviewed_at"):
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行缺少 reviewed_at")
    else:
        raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 event 不合法")
    return event


def load_events(task_dir: Path) -> list[dict[str, Any]]:
    """严格读取任务决策事件。

    Args:
        task_dir: 任务目录。

    Returns:
        按文件顺序排列的事件列表；文件不存在时返回空列表。

    Raises:
        DecisionLogError: JSONL 损坏或事件结构不合法。
    """
    path = task_dir / "decisions.jsonl"
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except FileNotFoundError:
        return []
    except OSError as exc:
        raise DecisionLogError(f"无法读取 decisions.jsonl:{exc}") from exc

    events: list[dict[str, Any]] = []
    for line_number, raw in enumerate(lines, 1):
        if not raw.strip():
            continue
        try:
            event = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise DecisionLogError(f"decisions.jsonl 第 {line_number} 行 JSON 损坏:{exc}") from exc
        events.append(_validate_event(event, line_number))
    return events


def _decision_events(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """筛选 decision 事件。

    Args:
        events: 全部日志事件。

    Returns:
        decision 事件列表。
    """
    return [event for event in events if event.get("event") == "decision"]


def decision_digest(events: list[dict[str, Any]]) -> str:
    """计算当前全部 decision 事件的稳定摘要。

    Args:
        events: 全部日志事件。

    Returns:
        SHA-256 十六进制摘要；无 decision 时返回空字符串。
    """
    decisions = _decision_events(events)
    if not decisions:
        return ""
    payload = "\n".join(
        json.dumps(event, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
        for event in decisions
    ) + "\n"
    return hashlib.sha256(payload.encode("utf-8")).hexdigest()


def _atomic_write_events(task_dir: Path, events: list[dict[str, Any]]) -> None:
    """原子写回完整决策日志。

    Args:
        task_dir: 任务目录。
        events: 完整事件列表。

    Raises:
        OSError: 临时文件或原子替换失败。
    """
    path = task_dir / "decisions.jsonl"
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            for event in events:
                handle.write(json.dumps(event, ensure_ascii=False, sort_keys=True) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        try:
            temp_path.unlink()
        except FileNotFoundError:
            pass
        raise


def decision_review_status(task_dir: Path) -> dict[str, Any]:
    """返回任务当前决策摘要与归档审查状态。

    Args:
        task_dir: 任务目录。

    Returns:
        包含 decision 数量、digest、最新有效 review 和归档门禁状态的对象。

    Raises:
        DecisionLogError: 日志损坏。
    """
    events = load_events(task_dir)
    decisions = _decision_events(events)
    digest = decision_digest(events)
    decision_ids = [str(event["decision_id"]) for event in decisions]
    valid_review: dict[str, Any] | None = None
    for event in reversed(events):
        if event.get("event") != "review":
            continue
        if event.get("decision_digest") != digest:
            continue
        if event.get("decision_ids") != decision_ids:
            continue
        valid_review = event
        break
    verdict = valid_review.get("verdict") if valid_review else None
    return {
        "task": str(task_dir),
        "log_path": str(task_dir / "decisions.jsonl"),
        "has_decisions": bool(decisions),
        "decision_count": len(decisions),
        "decision_ids": decision_ids,
        "decision_digest": digest,
        "review_verdict": verdict,
        "reviewed_at": valid_review.get("reviewed_at") if valid_review else None,
        "archive_allowed": not decisions or verdict == "accepted",
        "needs_review": bool(decisions) and verdict != "accepted",
        "decisions": decisions,
    }


def append_decision(
    task_dir: Path,
    *,
    run_id: str,
    topic: str,
    options: list[str],
    choice: str,
    summary: str,
    evidence: list[str],
    risk: str,
    confidence: str,
    requirements: list[str],
    files: list[str],
    planning_sha256: str = "",
    handoff_sha256: str = "",
    verification: str = "",
) -> dict[str, Any]:
    """追加一条 AI 自主决策。

    Args:
        task_dir: 任务目录。
        run_id: 产生决策的 auto-loop run id。
        topic: 决策主题。
        options: 已评估的候选方案。
        choice: 最终选择。
        summary: 可审计的简短依据。
        evidence: 仓库或需求证据摘要。
        risk: 风险等级，仅允许 low 或 medium。
        confidence: 置信度。
        requirements: 受影响的需求标识。
        files: 受影响的 artifact 或代码文件。
        planning_sha256: 决策时 planning 摘要。
        handoff_sha256: 决策时 handoff 摘要。
        verification: 后续验证结果摘要。

    Returns:
        新增的 decision 事件。

    Raises:
        DecisionLogError: 参数不合法或现有日志损坏。
    """
    if risk not in VALID_RISKS:
        raise DecisionLogError("risk 只允许 low 或 medium；高风险事项必须 blocked")
    if confidence not in VALID_CONFIDENCES:
        raise DecisionLogError("confidence 必须是 low、medium 或 high")
    if not all((run_id.strip(), topic.strip(), choice.strip(), summary.strip())):
        raise DecisionLogError("run_id、topic、choice 和 summary 不能为空")
    normalized_options = [value for value in options if value]
    if not normalized_options or choice not in normalized_options:
        raise DecisionLogError("choice 必须存在于非空 options 中")

    events = load_events(task_dir)
    existing_ids = [
        int(str(event["decision_id"]).removeprefix("DEC-"))
        for event in _decision_events(events)
        if str(event.get("decision_id", "")).removeprefix("DEC-").isdigit()
    ]
    event = {
        "schema_version": SCHEMA_VERSION,
        "event": "decision",
        "decision_id": f"DEC-{max(existing_ids, default=0) + 1:04d}",
        "run_id": run_id,
        "topic": topic,
        "options": normalized_options,
        "choice": choice,
        "summary": summary,
        "evidence": [value for value in evidence if value],
        "risk": risk,
        "confidence": confidence,
        "requirements": [value for value in requirements if value],
        "files": [value for value in files if value],
        "planning_sha256": planning_sha256,
        "handoff_sha256": handoff_sha256,
        "verification": verification,
        "recorded_at": _utc_now(),
    }
    events.append(event)
    _atomic_write_events(task_dir, events)
    return event


def review_decisions(
    task_dir: Path,
    *,
    verdict: str,
    decision_ids: list[str] | None = None,
    notes: str = "",
) -> dict[str, Any]:
    """记录当前 decision digest 的人工审查结果。

    Args:
        task_dir: 任务目录。
        verdict: accepted 或 changes-requested。
        decision_ids: changes-requested 时指定返工的决策 ID。
        notes: 人工审查备注。

    Returns:
        新增的 review 事件。

    Raises:
        DecisionLogError: 没有决策、verdict 或 decision ID 不合法。
    """
    if verdict not in VALID_VERDICTS:
        raise DecisionLogError("verdict 必须是 accepted 或 changes-requested")
    events = load_events(task_dir)
    decisions = _decision_events(events)
    if not decisions:
        raise DecisionLogError("任务没有可审查的 AI 决策")
    all_ids = [str(event["decision_id"]) for event in decisions]
    requested_ids = [value for value in (decision_ids or []) if value]
    unknown = sorted(set(requested_ids) - set(all_ids))
    if unknown:
        raise DecisionLogError(f"未知 decision ID:{','.join(unknown)}")
    if verdict == "accepted" and requested_ids:
        raise DecisionLogError("accepted 只能一次接受当前全部决策，不接收 decision-id")
    if verdict == "changes-requested" and not requested_ids:
        raise DecisionLogError("changes-requested 必须至少指定一个 decision-id")

    event = {
        "schema_version": SCHEMA_VERSION,
        "event": "review",
        "verdict": verdict,
        "decision_digest": decision_digest(events),
        # review 始终绑定当前全部 decision；requested_decision_ids 单独表达返工范围。
        "decision_ids": all_ids,
        "requested_decision_ids": requested_ids,
        "notes": notes,
        "reviewed_at": _utc_now(),
    }
    events.append(event)
    _atomic_write_events(task_dir, events)
    return event


def _print(data: dict[str, Any]) -> int:
    """输出紧凑 JSON。

    Args:
        data: 输出对象。

    Returns:
        进程成功退出码。
    """
    print(json.dumps(data, ensure_ascii=False, sort_keys=True))
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    """处理 status 命令。

    Args:
        args: 命令行参数。

    Returns:
        进程退出码。
    """
    repo_root = _repo_root()
    if repo_root is None:
        raise DecisionLogError("当前目录不是 Trellis 项目")
    task_dir = _resolve_task_dir(repo_root, args.task)
    status = decision_review_status(task_dir)
    if not args.json:
        status = {key: value for key, value in status.items() if key != "decisions"}
    return _print({"status": "ok", **status})


def cmd_review(args: argparse.Namespace) -> int:
    """处理 review 命令。

    Args:
        args: 命令行参数。

    Returns:
        进程退出码。
    """
    repo_root = _repo_root()
    if repo_root is None:
        raise DecisionLogError("当前目录不是 Trellis 项目")
    task_dir = _resolve_task_dir(repo_root, args.task)
    event = review_decisions(
        task_dir,
        verdict=args.verdict,
        decision_ids=args.decision_id,
        notes=args.notes or "",
    )
    return _print({"status": "reviewed", "task": str(task_dir), "review": event})


def build_parser() -> argparse.ArgumentParser:
    """构造命令行解析器。

    Returns:
        已配置的参数解析器。
    """
    parser = argparse.ArgumentParser(description="Trellis AI decision log.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    status = subparsers.add_parser("status")
    status.add_argument("--task", required=True)
    status.add_argument("--json", action="store_true")
    status.set_defaults(func=cmd_status)

    review = subparsers.add_parser("review")
    review.add_argument("--task", required=True)
    review.add_argument("--verdict", choices=sorted(VALID_VERDICTS), required=True)
    review.add_argument("--decision-id", action="append", default=[])
    review.add_argument("--notes")
    review.set_defaults(func=cmd_review)
    return parser


def main() -> int:
    """运行 CLI。

    Returns:
        进程退出码。
    """
    args = build_parser().parse_args()
    try:
        return args.func(args)
    except (DecisionLogError, OSError) as exc:
        print(json.dumps({"status": "error", "reason": "decision-log-error", "message": str(exc)}, ensure_ascii=False))
        return 1


if __name__ == "__main__":
    sys.exit(main())
