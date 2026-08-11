#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""SessionStart 启动更新检查 hook。

脚本只做只读检查和上下文注入:调用 `flower-trellis self-check --json`,
发现可执行更新时输出 `<flower-update>` 块。失败、离线、无更新或关闭检查时静默退出,
避免影响 Codex / Claude Code 正常启动。
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path


ACTIONABLE_STATUSES = {"update_available", "project_out_of_sync"}
SELF_CHECK_TIMEOUT_SECONDS = 30


def _debug(message: str) -> None:
    """在显式调试时输出简短错误。"""
    if os.environ.get("FLOWER_UPDATE_HOOK_DEBUG"):
        print(f"flower_update_hook: {message}", file=sys.stderr)


def _project_dir(hook_input: dict) -> Path:
    """从平台环境变量或 hook stdin 解析项目目录。"""
    for name in (
        "CLAUDE_PROJECT_DIR",
        "CODEX_PROJECT_DIR",
        "CURSOR_PROJECT_DIR",
        "GEMINI_PROJECT_DIR",
        "QODER_PROJECT_DIR",
        "CODEBUDDY_PROJECT_DIR",
        "TRAE_PROJECT_DIR",
    ):
        value = os.environ.get(name)
        if value:
            return Path(value).resolve()
    return Path(str(hook_input.get("cwd") or ".")).resolve()


def _run_self_check(project_dir: Path) -> dict | None:
    """执行 flower-trellis self-check 并解析 JSON。"""
    try:
        result = subprocess.run(
            [
                "flower-trellis",
                "self-check",
                "--json",
                "--target",
                str(project_dir),
            ],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=SELF_CHECK_TIMEOUT_SECONDS,
            cwd=str(project_dir),
        )
    except (subprocess.TimeoutExpired, FileNotFoundError, PermissionError) as exc:
        _debug(str(exc))
        return None
    if result.returncode != 0:
        _debug(result.stderr.strip() or f"退出码 {result.returncode}")
        return None
    try:
        data = json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        _debug(f"JSON 解析失败:{exc}")
        return None
    return data if isinstance(data, dict) else None


def _json_bool(value: object) -> str:
    """把布尔值格式化成 JSON 风格小写文本。"""
    return json.dumps(bool(value), ensure_ascii=False)


def _has_release_notes(data: dict) -> bool:
    """判断 self-check 是否提供了可展示的 release notes。"""
    notes = data.get("releaseNotes")
    if not isinstance(notes, dict):
        return False
    versions = notes.get("versions")
    return isinstance(versions, list) and bool(versions)


def _release_notes_lines(data: dict) -> list[str]:
    """把 self-check 的 releaseNotes 摘要格式化为短字段。"""
    notes = data.get("releaseNotes")
    if not isinstance(notes, dict):
        return []
    versions = notes.get("versions") or []
    if not isinstance(versions, list):
        versions = []
    safe_versions = []
    for entry in versions:
        if not isinstance(entry, dict):
            continue
        version = entry.get("version")
        body = entry.get("body")
        if isinstance(version, str) and isinstance(body, str) and body.strip():
            safe_versions.append(
                {
                    "version": version,
                    "body": body,
                    "truncated": bool(entry.get("truncated")),
                }
            )

    lines: list[str] = []
    note_range = notes.get("range") or {}
    if isinstance(note_range, dict) and (note_range.get("from") or note_range.get("to")):
        range_text = f"{note_range.get('from')} -> {note_range.get('to')}"
        if note_range.get("channel"):
            range_text += f" ({note_range.get('channel')})"
        lines.append(f"release_notes_range: {range_text}")
    if safe_versions:
        lines.append(
            "release_notes: "
            + json.dumps(safe_versions, ensure_ascii=False, separators=(",", ":"))
        )
    elif notes.get("unavailable"):
        lines.append("release_notes_unavailable: true")
    if notes.get("truncated"):
        lines.append(f"release_notes_truncated: {_json_bool(notes.get('truncated'))}")
    if notes.get("moreVersions"):
        lines.append(f"release_notes_more_versions: {_json_bool(notes.get('moreVersions'))}")
    return lines


def _ai_instruction(data: dict, command: str | None) -> str | None:
    """生成面向 AI 的单条动作指令,避免重复字段互相打架。"""
    ai = data.get("ai") or {}
    instruction = ai.get("instruction")
    if ai.get("mode") == "ask" and command:
        if _has_release_notes(data):
            return "先展示 release_notes 摘要和 recommended_command,再询问用户确认;确认前禁止执行 recommended_command;用户拒绝本次升级时执行 snooze_command,明确跳过时执行 skip_command。"
        return "先展示 recommended_command,再询问用户确认;确认前禁止执行 recommended_command;用户拒绝本次升级时执行 snooze_command,明确跳过时执行 skip_command。"
    return instruction if isinstance(instruction, str) and instruction else None


def _format_context(data: dict) -> str:
    """把 self-check JSON 转成给 AI 读取的短上下文块。"""
    current = data.get("current") or {}
    project = data.get("project") or {}
    remote = data.get("remote") or {}
    ai = data.get("ai") or {}
    prompt = data.get("prompt") or {}
    safety = data.get("safety") or {}
    out_of_sync_reasons = project.get("outOfSyncReasons") or []
    current_trellis = current.get("bundledTrellisVersion")
    project_trellis = project.get("trellisVersion")
    lines = [
        "<flower-update>",
        "priority: blocking_confirmation_required",
        "instruction_scope: first_assistant_reply",
        f"status: {data.get('status')}",
        f"current_flower: {current.get('flowerVersion')}",
        f"project_flower: {project.get('flowerVersion')}",
    ]
    if current_trellis and project_trellis and current_trellis != project_trellis:
        lines.append(f"bundled_trellis: {current_trellis}")
        lines.append(f"project_trellis: {project_trellis}")
    if out_of_sync_reasons:
        lines.append(f"project_out_of_sync_reasons: {', '.join(out_of_sync_reasons)}")
    if data.get("status") == "update_available" and remote.get("tags"):
        lines.append(f"remote: {json.dumps(remote.get('tags'), ensure_ascii=False)}")
    if remote.get("errorCode"):
        lines.append(f"remote_error_code: {remote.get('errorCode')}")
    lines.extend(_release_notes_lines(data))
    command = (data.get("commands") or {}).get("recommended") or ai.get("command")
    if command:
        lines.append(f"recommended_command: {command}")
    prompt_commands = prompt.get("commands") if isinstance(prompt, dict) else {}
    if isinstance(prompt_commands, dict):
        snooze_command = prompt_commands.get("snooze")
        skip_command = prompt_commands.get("skip")
        if isinstance(snooze_command, str) and snooze_command:
            lines.append(f"snooze_command: {snooze_command}")
        if isinstance(skip_command, str) and skip_command:
            lines.append(f"skip_command: {skip_command}")
    if safety.get("reasons"):
        lines.append(f"safety_reasons: {', '.join(safety.get('reasons') or [])}")
    instruction = _ai_instruction(data, command)
    if instruction:
        lines.append(f"ai_instruction: {instruction}")
    lines.append("</flower-update>")
    return "\n".join(lines)


def _system_message(data: dict) -> str:
    """生成 Codex / Claude Code 更容易注意到的短系统提示。"""
    ai = data.get("ai") or {}
    command = (data.get("commands") or {}).get("recommended") or ai.get("command")
    if ai.get("mode") == "ask" and command:
        if _has_release_notes(data):
            return "flower-trellis 发现可执行更新;必须先展示更新摘要并询问用户是否执行 recommended_command,确认前禁止运行;用户拒绝时执行 snooze_command。"
        return "flower-trellis 发现可执行更新;必须先询问用户是否执行 recommended_command,确认前禁止运行;用户拒绝时执行 snooze_command。"
    if command:
        return "flower-trellis 发现可执行更新;已注入 recommended_command。"
    return "flower-trellis update context injected"


def _emit_context(context: str, system_message: str) -> None:
    """输出 Codex / Claude Code 都接受的 SessionStart hook JSON。"""
    result = {
        "suppressOutput": True,
        "systemMessage": system_message,
        "hookSpecificOutput": {
            "hookEventName": "SessionStart",
            "additionalContext": context,
        },
    }
    print(json.dumps(result, ensure_ascii=False), flush=True)


def main() -> None:
    try:
        hook_input = json.loads(sys.stdin.read() or "{}")
        if not isinstance(hook_input, dict):
            hook_input = {}
    except json.JSONDecodeError:
        hook_input = {}

    data = _run_self_check(_project_dir(hook_input))
    if not data or data.get("status") not in ACTIONABLE_STATUSES:
        return
    _emit_context(_format_context(data), _system_message(data))


if __name__ == "__main__":
    main()
