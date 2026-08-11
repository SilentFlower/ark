#!/usr/bin/env python3
"""诊断并管理分支本地化的 Trellis Git worktree。"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from contextlib import contextmanager
from datetime import datetime, timezone
from pathlib import Path, PurePosixPath
from typing import Any, Iterator

REGISTRY_SCHEMA_VERSION = 1
LEGACY_MANIFEST_SCHEMA_VERSION = 1
MANIFEST_NAME = ".trellis-worktree.json"
LEGACY_ENTRY_PATHS = (".trellis", ".agents", ".codex", ".claude")
ENTRY_PATHS = (*LEGACY_ENTRY_PATHS, ".flower")
REGISTRY_DIRECTORY = "trellis"
REGISTRY_NAME = "registry-v1.json"
ACTIVE_TASK_STATUSES = {"planning", "in_progress"}
LOCAL_STATE_PATHS = (".trellis", ".flower", ".agents", ".codex", ".claude")
ROUTE_PREFERENCES_PATH = ".trellis/.route-prefs.tmp"
ROUTE_PREFERENCE_MODES = {
    "implement": {"inline", "subagent"},
    "check": {"check-all-inline", "check-all-subagent"},
}
NOT_INHERITED_LOCAL_STATE = (
    "session-state",
    "auto-loop",
    "flower-local-state",
    "platform-local-settings",
    "cache-and-transaction-state",
)


class WorktreeSetupError(Exception):
    """携带稳定 reason code 的 worktree 操作错误。"""

    def __init__(self, reason: str, message: str, **details: Any) -> None:
        """初始化结构化错误。

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


def _path_text(path: Path) -> str:
    """返回不跟随最终 symlink 的稳定绝对路径。"""
    return os.path.abspath(os.fspath(path.expanduser()))


def _git_run(
    start: Path,
    *args: str,
    timeout: int = 10,
) -> subprocess.CompletedProcess[str]:
    """运行 Git 命令并返回完整结果。"""
    try:
        return subprocess.run(
            ["git", "-C", str(start), *args],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise WorktreeSetupError(
            "git-command-failed",
            "Git 命令无法执行",
            command=["git", "-C", str(start), *args],
            error=str(error),
        ) from error


def _git_run_bytes(
    start: Path,
    *args: str,
    timeout: int = 10,
) -> subprocess.CompletedProcess[bytes]:
    """运行需要保留 Git NUL 分隔输出的命令。"""
    try:
        return subprocess.run(
            ["git", "-C", str(start), *args],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise WorktreeSetupError(
            "git-command-failed",
            "Git 命令无法执行",
            command=["git", "-C", str(start), *args],
            error=str(error),
        ) from error


def _parse_porcelain_z(payload: bytes) -> list[dict[str, str]]:
    """解析 Git porcelain NUL 输出，避免路径转义和换行歧义。"""
    parts = payload.split(b"\0")
    entries: list[dict[str, str]] = []
    index = 0
    while index < len(parts):
        item = parts[index]
        index += 1
        if not item:
            continue
        if len(item) < 4 or item[2:3] != b" ":
            raise WorktreeSetupError("git-status-invalid", "无法解析 Git porcelain 状态")
        status = item[:2].decode("ascii", errors="replace")
        entry = {"status": status, "path": os.fsdecode(item[3:])}
        if "R" in status or "C" in status:
            if index >= len(parts) or not parts[index]:
                raise WorktreeSetupError("git-status-invalid", "Git rename/copy 状态缺少第二路径")
            entry["originalPath"] = os.fsdecode(parts[index])
            index += 1
        entries.append(entry)
    return sorted(
        entries,
        key=lambda entry: (
            entry.get("path", ""),
            entry.get("originalPath", ""),
            entry.get("status", ""),
        ),
    )


def _git_output(start: Path, *args: str) -> str | None:
    """执行只读 Git 命令并返回 stdout，非零退出时返回 None。"""
    result = _git_run(start, *args)
    if result.returncode != 0:
        return None
    output = result.stdout.strip()
    return output or None


def _git_require(start: Path, *args: str, reason: str, message: str) -> str:
    """执行必须成功的 Git 命令并返回去空白 stdout。"""
    result = _git_run(start, *args, timeout=30)
    if result.returncode != 0:
        raise WorktreeSetupError(
            reason,
            message,
            command=["git", "-C", str(start), *args],
            stderr=result.stderr.strip(),
        )
    return result.stdout.strip()


def _resolve_start(path_text: str | None) -> Path:
    """解析已存在的 target，允许传入 worktree 子目录或文件。"""
    path = Path(path_text or os.getcwd()).expanduser()
    if not path.exists():
        raise WorktreeSetupError("target-missing", "目标路径不存在", target=str(path))
    resolved = path.resolve()
    return resolved.parent if resolved.is_file() else resolved


def _git_toplevel(start: Path) -> Path | None:
    """返回当前 Git worktree 根目录。"""
    output = _git_output(start, "rev-parse", "--path-format=absolute", "--show-toplevel")
    if output is None:
        output = _git_output(start, "rev-parse", "--show-toplevel")
    return Path(output).expanduser().resolve() if output else None


def _git_common_dir(start: Path) -> Path | None:
    """返回当前仓库 common dir，兼容旧 Git 的相对路径输出。"""
    output = _git_output(start, "rev-parse", "--path-format=absolute", "--git-common-dir")
    if output is None:
        output = _git_output(start, "rev-parse", "--git-common-dir")
    if output is None:
        return None
    common_dir = Path(output)
    if common_dir.is_absolute():
        return common_dir.resolve()
    top_level = _git_toplevel(start)
    if top_level is None:
        return None
    return (top_level / common_dir).resolve()


def _git_dir(start: Path) -> Path | None:
    """返回当前 worktree 的独立 git-dir。"""
    output = _git_output(start, "rev-parse", "--path-format=absolute", "--git-dir")
    if output is None:
        output = _git_output(start, "rev-parse", "--git-dir")
    if output is None:
        return None
    git_dir = Path(output)
    return git_dir.resolve() if git_dir.is_absolute() else (start / git_dir).resolve()


def _worktree_context(start: Path) -> dict[str, Any]:
    """解析目标 worktree 的 Git 身份和分支事实。"""
    target_root = _git_toplevel(start)
    if target_root is None:
        raise WorktreeSetupError("not-git-worktree", "目标路径不在 Git worktree 中", target=str(start))
    common_dir = _git_common_dir(target_root)
    git_dir = _git_dir(target_root)
    if common_dir is None or git_dir is None:
        raise WorktreeSetupError("git-metadata-unavailable", "无法解析 worktree 的 Git 元数据")
    branch = _git_output(target_root, "branch", "--show-current")
    head = _git_output(target_root, "rev-parse", "HEAD")
    return {
        "targetRoot": target_root,
        "gitCommonDir": common_dir,
        "gitDir": git_dir,
        "worktreeId": hashlib.sha256(_path_text(git_dir).encode("utf-8")).hexdigest()[:16],
        "branch": branch,
        "head": head,
    }


def _read_json(path: Path) -> tuple[str, dict[str, Any] | None]:
    """读取 JSON 对象，并区分缺失、损坏和有效状态。"""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return "missing", None
    except (json.JSONDecodeError, OSError):
        return "invalid", None
    return ("ok", data) if isinstance(data, dict) else ("invalid", None)


def _write_json_atomic(path: Path, data: dict[str, Any]) -> None:
    """使用同目录临时文件原子写入 JSON。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(json.dumps(data, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        temp_path.unlink(missing_ok=True)
        raise


def _write_text_atomic(path: Path, content: str) -> None:
    """使用同目录临时文件原子写入 UTF-8 文本。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    temp_path = Path(temp_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
    except Exception:
        temp_path.unlink(missing_ok=True)
        raise


def _registry_path(common_dir: Path) -> Path:
    """返回当前仓库的机器本地 worktree registry 路径。"""
    return common_dir / REGISTRY_DIRECTORY / REGISTRY_NAME


def _load_registry(common_dir: Path) -> dict[str, Any]:
    """读取并校验机器本地 registry。"""
    path = _registry_path(common_dir)
    status, data = _read_json(path)
    if status == "missing":
        return {"schemaVersion": REGISTRY_SCHEMA_VERSION, "worktrees": {}}
    if status != "ok" or data is None:
        raise WorktreeSetupError(
            "registry-invalid",
            "worktree registry 已损坏，拒绝覆盖",
            registryPath=_path_text(path),
        )
    if data.get("schemaVersion") != REGISTRY_SCHEMA_VERSION or not isinstance(data.get("worktrees"), dict):
        raise WorktreeSetupError(
            "registry-schema-unsupported",
            "worktree registry schema 不受支持",
            registryPath=_path_text(path),
        )
    return data


@contextmanager
def _registry_lock(common_dir: Path) -> Iterator[None]:
    """通过原子 mkdir 获取 registry 写锁，并在退出时释放。"""
    lock_path = common_dir / REGISTRY_DIRECTORY / "locks" / "registry.lock"
    try:
        lock_path.mkdir(parents=True, exist_ok=False)
    except FileExistsError as error:
        raise WorktreeSetupError(
            "registry-lock-held",
            "另一个 worktree 操作正在更新 registry",
            lockPath=_path_text(lock_path),
        ) from error
    try:
        _write_json_atomic(
            lock_path / "owner.json",
            {"pid": os.getpid(), "createdAt": _utc_now(), "cwd": _path_text(Path.cwd())},
        )
        yield
    finally:
        shutil.rmtree(lock_path, ignore_errors=True)


def _registry_entry(context: dict[str, Any], task: str | None = None) -> dict[str, Any]:
    """构造当前 worktree 的 registry 条目。"""
    target_root = context["targetRoot"]
    version_path = target_root / ".trellis/.version"
    try:
        version = version_path.read_text(encoding="utf-8").strip() or None
    except OSError:
        version = None
    return {
        "path": _path_text(target_root),
        "gitDir": _path_text(context["gitDir"]),
        "branch": context["branch"],
        "head": context["head"],
        "task": task,
        "trellisVersion": version,
        "updatedAt": _utc_now(),
    }


def _register_worktree(
    context: dict[str, Any],
    *,
    task: str | None = None,
    developer: str | None = None,
) -> bool:
    """在 common-dir registry 中新增或刷新当前 worktree。"""
    common_dir = context["gitCommonDir"]
    registry = _load_registry(common_dir)
    worktrees = registry["worktrees"]
    registration_conflicts = _registry_registration_conflicts(context, registry, task=task)
    if registration_conflicts:
        conflict = registration_conflicts[0]
        raise WorktreeSetupError(
            conflict["reason"],
            "worktree registry 约束冲突",
            conflicts=registration_conflicts,
        )
    existing = worktrees.get(context["worktreeId"])
    if isinstance(existing, dict):
        if existing.get("path") != _path_text(context["targetRoot"]) or existing.get("gitDir") != _path_text(context["gitDir"]):
            raise WorktreeSetupError("registry-drift", "worktree ID 与现有 registry 路径不一致")
        if task is None:
            task = existing.get("task") if isinstance(existing.get("task"), str) else None
    desired = _registry_entry(context, task)
    comparable_keys = ("path", "gitDir", "branch", "head", "task", "trellisVersion")
    changed = not isinstance(existing, dict) or any(existing.get(key) != desired.get(key) for key in comparable_keys)
    if not changed and isinstance(existing, dict):
        desired = existing
    worktrees[context["worktreeId"]] = desired
    if developer and registry.get("developer") != developer:
        registry["developer"] = developer
        changed = True
    if changed:
        _write_json_atomic(_registry_path(common_dir), registry)
    return changed


def _configured_entry_paths(target_root: Path) -> set[str]:
    """从目标分支元数据识别已启用的本地入口。"""
    configured = {".trellis"}
    trellis_root = target_root / ".trellis"
    if trellis_root.is_dir() and not trellis_root.is_symlink():
        hashes_path = trellis_root / ".template-hashes.json"
        status, data = _read_json(hashes_path)
        if status == "ok" and data is not None:
            hashes = data.get("hashes")
            if isinstance(hashes, dict):
                for relative in hashes:
                    if not isinstance(relative, str):
                        continue
                    first = PurePosixPath(relative.replace("\\", "/")).parts
                    if first and first[0] in ENTRY_PATHS:
                        configured.add(first[0])
    for relative in ENTRY_PATHS:
        path = target_root / relative
        if path.exists() and not path.is_symlink():
            configured.add(relative)
    return configured


def _legacy_manifest(target_root: Path) -> dict[str, Any]:
    """读取 schema v1 manifest 并返回稳定诊断。"""
    path = target_root / MANIFEST_NAME
    status, data = _read_json(path)
    result: dict[str, Any] = {"path": _path_text(path), "status": status, "managedPaths": []}
    if status != "ok" or data is None:
        return result
    result["schemaVersion"] = data.get("schemaVersion")
    result["sourceRoot"] = data.get("sourceRoot")
    result["targetRoot"] = data.get("targetRoot")
    links = data.get("links")
    if data.get("schemaVersion") != LEGACY_MANIFEST_SCHEMA_VERSION or not isinstance(links, list):
        result["status"] = "unsupported"
        return result
    source_root = Path(data["sourceRoot"]).expanduser() if isinstance(data.get("sourceRoot"), str) else None
    declared_target = Path(data["targetRoot"]).expanduser() if isinstance(data.get("targetRoot"), str) else None
    if source_root is None or declared_target is None or not source_root.is_absolute() or not declared_target.is_absolute():
        result["status"] = "invalid"
        return result
    managed_paths: list[str] = []
    for item in links:
        relative = item.get("path") if isinstance(item, dict) else None
        if not isinstance(relative, str) or relative not in LEGACY_ENTRY_PATHS or relative in managed_paths:
            result["status"] = "invalid"
            return result
        expected_source = _path_text(source_root / relative)
        expected_target = _path_text(target_root / relative)
        if item.get("source") != expected_source or item.get("target") != expected_target:
            result["status"] = "invalid"
            return result
        managed_paths.append(relative)
    result["managedPaths"] = managed_paths
    return result


def _same_symlink_target(link: Path, expected: Path) -> bool:
    """判断 symlink 是否仍指向 manifest 声明的绝对来源。"""
    try:
        raw_target = Path(os.readlink(link))
    except OSError:
        return False
    actual = raw_target if raw_target.is_absolute() else link.parent / raw_target
    return actual.resolve(strict=False) == expected.resolve(strict=False)


def _registry_registration_conflicts(
    context: dict[str, Any],
    registry: dict[str, Any],
    *,
    task: str | None,
) -> list[dict[str, Any]]:
    """返回当前 worktree 注册会违反的机器状态约束。"""
    target_path = _path_text(context["targetRoot"])
    git_dir_path = _path_text(context["gitDir"])
    worktree_id = context["worktreeId"]
    conflicts: list[dict[str, Any]] = []
    for existing_id, entry in registry["worktrees"].items():
        if not isinstance(entry, dict):
            conflicts.append({"reason": "registry-entry-invalid", "worktreeId": existing_id})
            continue
        entry_path = entry.get("path")
        entry_git_dir = entry.get("gitDir")
        if existing_id == worktree_id:
            if entry_path != target_path or entry_git_dir != git_dir_path:
                conflicts.append(
                    {
                        "reason": "registry-drift",
                        "worktreeId": existing_id,
                        "path": entry_path,
                        "gitDir": entry_git_dir,
                    }
                )
            continue
        if entry_path == target_path or entry_git_dir == git_dir_path:
            conflicts.append(
                {
                    "reason": "registry-worktree-collision",
                    "worktreeId": existing_id,
                    "path": entry_path,
                    "gitDir": entry_git_dir,
                }
            )
        if task is not None and entry.get("task") == task:
            conflicts.append(
                {
                    "reason": "task-already-registered",
                    "worktreeId": existing_id,
                    "task": task,
                    "path": entry_path,
                }
            )
    return conflicts


def _analyze(target_arg: str | None) -> dict[str, Any]:
    """生成 target-local readiness 诊断，且不写盘。"""
    context = _worktree_context(_resolve_start(target_arg))
    target_root = context["targetRoot"]
    legacy = _legacy_manifest(target_root)
    configured = _configured_entry_paths(target_root)
    managed = set(legacy.get("managedPaths", []))
    source_text = legacy.get("sourceRoot")
    source_root = Path(source_text).expanduser() if isinstance(source_text, str) else None
    conflicts: list[dict[str, Any]] = []
    entries: list[dict[str, Any]] = []
    legacy_paths: list[str] = []
    missing_configured: list[str] = []

    if legacy["status"] in {"invalid", "unsupported"}:
        conflicts.append({"path": MANIFEST_NAME, "reason": "legacy-manifest-invalid"})
    elif legacy["status"] == "ok":
        declared_target = legacy.get("targetRoot")
        if not isinstance(declared_target, str) or Path(declared_target).expanduser().resolve() != target_root:
            conflicts.append({"path": MANIFEST_NAME, "reason": "legacy-target-mismatch"})

    for relative in ENTRY_PATHS:
        path = target_root / relative
        item: dict[str, Any] = {"path": relative, "configured": relative in configured}
        if path.is_symlink():
            expected = source_root / relative if source_root is not None else None
            if relative in managed and expected is not None and _same_symlink_target(path, expected):
                item["state"] = "legacy-link"
                legacy_paths.append(relative)
            else:
                item["state"] = "conflict"
                item["reason"] = "unmanaged-or-drifted-symlink"
                conflicts.append({"path": relative, "reason": item["reason"]})
        elif path.is_dir():
            item["state"] = "local-directory"
        elif path.exists():
            item["state"] = "conflict"
            item["reason"] = "entry-not-directory"
            conflicts.append({"path": relative, "reason": item["reason"]})
        else:
            item["state"] = "missing"
            if relative in configured:
                missing_configured.append(relative)
        entries.append(item)

    developer_path = target_root / ".trellis/.developer"
    runtime_path = target_root / ".trellis/.runtime"
    local_state = {
        "developer": "ready" if developer_path.is_file() and not developer_path.is_symlink() else "missing",
        "runtime": "ready" if runtime_path.is_dir() and not runtime_path.is_symlink() else "missing",
    }
    if developer_path.is_symlink() or (developer_path.exists() and not developer_path.is_file()):
        conflicts.append({"path": ".trellis/.developer", "reason": "local-state-conflict"})
    if runtime_path.is_symlink() or (runtime_path.exists() and not runtime_path.is_dir()):
        conflicts.append({"path": ".trellis/.runtime", "reason": "local-state-conflict"})

    registry_path = _registry_path(context["gitCommonDir"])
    registry_status, registry_data = _read_json(registry_path)
    registry_entry = None
    if registry_status == "ok" and registry_data is not None:
        worktrees = registry_data.get("worktrees")
        if registry_data.get("schemaVersion") != REGISTRY_SCHEMA_VERSION or not isinstance(worktrees, dict):
            registry_status = "invalid"
        else:
            registry_entry = worktrees.get(context["worktreeId"])
            current_task = registry_entry.get("task") if isinstance(registry_entry, dict) else None
            conflicts.extend(
                _registry_registration_conflicts(
                    context,
                    registry_data,
                    task=current_task if isinstance(current_task, str) else None,
                )
            )
    if registry_status == "invalid":
        conflicts.append({"path": _path_text(registry_path), "reason": "registry-invalid"})

    status = "ready-local"
    reason: str | None = None
    actions: list[dict[str, str]] = []
    if conflicts:
        status = "blocked"
        reason = "worktree-local-conflict"
    elif legacy["status"] == "ok" or legacy_paths:
        status = "needs-migration"
        reason = "legacy-projection-detected"
        actions.append({"command": "migrate", "reason": reason})
    elif not (target_root / ".trellis").is_dir() or missing_configured:
        status = "needs-init"
        reason = "local-trellis-missing"
        actions.append({"command": "flower-trellis init --target <worktree>", "reason": reason})
    elif local_state["developer"] != "ready" or local_state["runtime"] != "ready":
        status = "needs-prepare"
        reason = "local-runtime-missing"
        actions.append({"command": "prepare", "reason": reason})

    return {
        "status": status,
        "reason": reason,
        "targetRoot": _path_text(target_root),
        "gitDir": _path_text(context["gitDir"]),
        "gitCommonDir": _path_text(context["gitCommonDir"]),
        "worktreeId": context["worktreeId"],
        "branch": context["branch"],
        "head": context["head"],
        "entries": entries,
        "localState": local_state,
        "legacy": legacy,
        "registry": {
            "path": _path_text(registry_path),
            "status": registry_status,
            "entry": registry_entry,
        },
        "actions": actions,
        "conflicts": conflicts,
    }


def _developer_from_file(target_root: Path) -> str | None:
    """从目标本地 `.developer` 读取开发者名。"""
    try:
        lines = (target_root / ".trellis/.developer").read_text(encoding="utf-8").splitlines()
    except OSError:
        return None
    for line in lines:
        if line.startswith("name=") and line.split("=", 1)[1].strip():
            return line.split("=", 1)[1].strip()
    return None


def _write_developer(target_root: Path, developer: str) -> None:
    """在当前 worktree 写入本地开发者身份。"""
    developer_path = target_root / ".trellis/.developer"
    if developer_path.exists() or developer_path.is_symlink():
        raise WorktreeSetupError(
            "developer-state-conflict",
            "目标 `.developer` 不是可创建的本地文件",
            path=_path_text(developer_path),
        )
    developer_path.write_text(f"name={developer}\ninitialized_at={_utc_now()}\n", encoding="utf-8")


def _read_route_preferences(target_root: Path) -> dict[str, Any]:
    """安全读取并规范化个人 route 偏好。"""
    path = target_root / ROUTE_PREFERENCES_PATH
    try:
        mode = path.lstat().st_mode
    except FileNotFoundError:
        return {"path": _path_text(path), "status": "missing", "values": {}}
    except OSError as error:
        return {
            "path": _path_text(path),
            "status": "unreadable",
            "values": {},
            "error": str(error),
        }
    if not stat.S_ISREG(mode):
        return {"path": _path_text(path), "status": "type-invalid", "values": {}}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as error:
        return {
            "path": _path_text(path),
            "status": "unreadable",
            "values": {},
            "error": str(error),
        }

    values: dict[str, str] = {}
    for raw in lines:
        if "=" not in raw:
            continue
        key, value = raw.split("=", 1)
        normalized_key = key.strip()
        normalized_value = value.strip()
        if normalized_value in ROUTE_PREFERENCE_MODES.get(normalized_key, set()):
            values[normalized_key] = normalized_value
    ordered = {key: values[key] for key in ROUTE_PREFERENCE_MODES if key in values}
    return {
        "path": _path_text(path),
        "status": "ok" if ordered else "invalid",
        "values": ordered,
    }


def _route_preference_transfer(
    source_root: Path,
    target_root: Path,
    source_developer: str | None,
    target_developer: str,
) -> dict[str, Any]:
    """计算 route 偏好继承动作，不写入目标。"""
    target_path = target_root / ROUTE_PREFERENCES_PATH
    if target_path.exists() or target_path.is_symlink():
        return {"action": "preserved", "values": {}}
    if source_developer != target_developer:
        return {"action": "notInherited", "reason": "developer-mismatch", "values": {}}
    source = _read_route_preferences(source_root)
    if source["status"] != "ok":
        return {
            "action": "notInherited",
            "reason": f"source-{source['status']}",
            "values": {},
        }
    return {"action": "inherited", "values": source["values"]}


def _write_route_preferences(target_root: Path, values: dict[str, str]) -> None:
    """以固定字段顺序写入已规范化的 route 偏好。"""
    path = target_root / ROUTE_PREFERENCES_PATH
    if path.exists() or path.is_symlink():
        raise WorktreeSetupError(
            "route-preferences-target-conflict",
            "目标 route 偏好路径已经存在，拒绝覆盖",
            path=_path_text(path),
        )
    content = "".join(f"{key}={values[key]}\n" for key in ROUTE_PREFERENCE_MODES if key in values)
    _write_text_atomic(path, content)


def _prepare_local(
    plan: dict[str, Any],
    developer: str | None,
    *,
    source: str | None = None,
    inherit_route_prefs: bool = False,
) -> dict[str, Any]:
    """初始化当前 worktree 自己的 gitignored 运行态并注册。"""
    if plan["status"] == "needs-migration":
        raise WorktreeSetupError("migration-required", "检测到旧投影，请先运行 migrate")
    if plan["status"] == "needs-init":
        raise WorktreeSetupError("local-trellis-missing", "当前分支缺少本地 Trellis，请先在目标分支安装")
    if plan["status"] == "blocked":
        raise WorktreeSetupError("worktree-local-conflict", "目标 worktree 存在本地路径冲突", conflicts=plan["conflicts"])

    target_root = Path(plan["targetRoot"])
    context = _worktree_context(target_root)
    source_context = None
    source_developer = None
    if inherit_route_prefs:
        if not source:
            raise WorktreeSetupError(
                "route-preferences-source-required",
                "显式继承 route 偏好时需要 --source",
            )
        source_context = _worktree_context(_resolve_start(source))
        if source_context["gitCommonDir"] != context["gitCommonDir"]:
            raise WorktreeSetupError(
                "route-preferences-repository-mismatch",
                "route 偏好来源与目标不属于同一 Git 仓库",
            )
        source_developer = _developer_from_file(source_context["targetRoot"])
    changed_paths: list[str] = []
    route_transfer = {"action": "notRequested", "values": {}}
    with _registry_lock(context["gitCommonDir"]):
        current_developer = _developer_from_file(target_root)
        registry = _load_registry(context["gitCommonDir"])
        registration_conflicts = _registry_registration_conflicts(context, registry, task=None)
        if registration_conflicts:
            conflict = registration_conflicts[0]
            raise WorktreeSetupError(
                conflict["reason"],
                "worktree registry 约束冲突",
                conflicts=registration_conflicts,
            )
        resolved_developer = developer or current_developer or registry.get("developer")
        if current_developer is None and (not isinstance(resolved_developer, str) or not resolved_developer.strip()):
            raise WorktreeSetupError(
                "developer-required",
                "目标 worktree 缺少本地开发者身份，请传入 --developer",
            )
        normalized_developer = resolved_developer.strip()
        if inherit_route_prefs:
            if source_developer != normalized_developer:
                raise WorktreeSetupError(
                    "route-preferences-developer-mismatch",
                    "route 偏好来源与目标开发者身份不一致",
                    sourceDeveloper=source_developer,
                    targetDeveloper=normalized_developer,
                )
            route_transfer = _route_preference_transfer(
                source_context["targetRoot"],
                target_root,
                source_developer,
                normalized_developer,
            )
        runtime_sessions = target_root / ".trellis/.runtime/sessions"
        if not runtime_sessions.is_dir():
            runtime_sessions.mkdir(parents=True, exist_ok=True)
            changed_paths.append(".trellis/.runtime/sessions")
        if current_developer is None:
            _write_developer(target_root, normalized_developer)
            changed_paths.append(".trellis/.developer")
        if route_transfer["action"] == "inherited":
            _write_route_preferences(target_root, route_transfer["values"])
            changed_paths.append(ROUTE_PREFERENCES_PATH)
        registry_changed = _register_worktree(context, developer=normalized_developer)
    result = {
        **_analyze(str(target_root)),
        "status": "prepared" if changed_paths or registry_changed else "ready-local",
        "changed": bool(changed_paths or registry_changed),
        "changedPaths": changed_paths,
        "registryChanged": registry_changed,
    }
    result["localStateTransfer"] = {
        "routePreferences": route_transfer,
        "notInherited": list(NOT_INHERITED_LOCAL_STATE),
    }
    return result


def _archive_head_entries(target_root: Path, entries: list[str], destination: Path) -> None:
    """只从目标分支 HEAD 提取可验证的入口内容。"""
    result = subprocess.run(
        ["git", "-C", str(target_root), "archive", "--format=tar", "HEAD", "--", *entries],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=30,
    )
    if result.returncode != 0:
        raise WorktreeSetupError(
            "migration-source-unavailable",
            "无法从目标分支 HEAD 重建 legacy 入口",
            stderr=result.stderr.decode("utf-8", errors="replace").strip(),
        )
    archive_path = destination / "head.tar"
    archive_path.write_bytes(result.stdout)
    candidate_root = destination / "candidate"
    candidate_root.mkdir()
    with tarfile.open(archive_path, "r:") as archive:
        for member in archive.getmembers():
            pure = PurePosixPath(member.name)
            if pure.is_absolute() or ".." in pure.parts or not pure.parts or pure.parts[0] not in entries:
                raise WorktreeSetupError("migration-archive-invalid", "目标分支 archive 包含越界路径")
            if not (member.isdir() or member.isfile()):
                raise WorktreeSetupError(
                    "migration-archive-invalid",
                    "目标分支 archive 包含不受支持的链接或设备文件",
                    path=member.name,
                )
        archive.extractall(candidate_root)


def _migration_entries(plan: dict[str, Any]) -> list[str]:
    """校验 legacy manifest，并返回可事务迁移的受管路径。"""
    legacy = plan["legacy"]
    if legacy.get("status") != "ok":
        raise WorktreeSetupError("legacy-manifest-invalid", "schema v1 manifest 无效或缺失")
    managed = list(legacy.get("managedPaths", []))
    if not managed or ".trellis" not in managed:
        raise WorktreeSetupError("legacy-manifest-invalid", "legacy manifest 未管理 `.trellis`")
    entry_states = {item["path"]: item["state"] for item in plan["entries"]}
    invalid = [relative for relative in managed if entry_states.get(relative) != "legacy-link"]
    if invalid:
        raise WorktreeSetupError(
            "legacy-link-drift",
            "legacy symlink 已漂移，拒绝自动迁移",
            paths=invalid,
        )
    tracked = _git_output(Path(plan["targetRoot"]), "ls-tree", "-r", "--name-only", "HEAD", "--", *managed)
    tracked_paths = tracked.splitlines() if tracked else []
    missing = [relative for relative in managed if not any(path == relative or path.startswith(relative + "/") for path in tracked_paths)]
    if missing:
        raise WorktreeSetupError(
            "migration-source-unavailable",
            "目标分支 HEAD 无法重建全部 legacy 入口",
            paths=missing,
        )
    return managed


def _migrate(plan: dict[str, Any], *, dry_run: bool) -> dict[str, Any]:
    """事务化迁移 schema v1 整目录投影。"""
    if plan["status"] in {"ready-local", "needs-prepare"}:
        return {**plan, "changed": False, "dryRun": dry_run}
    if plan["status"] != "needs-migration":
        raise WorktreeSetupError(
            "migration-not-available",
            "当前状态不能执行 legacy 迁移",
            status=plan["status"],
            conflicts=plan["conflicts"],
        )
    managed = _migration_entries(plan)
    if dry_run:
        return {**plan, "status": "migration-ready", "changed": False, "dryRun": True, "migrationPaths": managed}

    target_root = Path(plan["targetRoot"])
    context = _worktree_context(target_root)
    with _registry_lock(context["gitCommonDir"]):
        with tempfile.TemporaryDirectory(prefix="flower-trellis-migrate-", dir=target_root.parent) as temp_name:
            temp_root = Path(temp_name)
            _archive_head_entries(target_root, managed, temp_root)
            candidate_root = temp_root / "candidate"
            backup_root = temp_root / "backup"
            backup_root.mkdir()
            moved_new: list[str] = []
            moved_old: list[str] = []
            manifest_path = target_root / MANIFEST_NAME
            try:
                for relative in managed:
                    candidate = candidate_root / relative
                    if not candidate.is_dir() or candidate.is_symlink():
                        raise WorktreeSetupError(
                            "migration-source-unavailable",
                            "目标分支 HEAD 没有可用的真实目录",
                            path=relative,
                        )
                for relative in managed:
                    os.replace(target_root / relative, backup_root / relative)
                    moved_old.append(relative)
                    shutil.move(str(candidate_root / relative), str(target_root / relative))
                    moved_new.append(relative)
                if manifest_path.exists() or manifest_path.is_symlink():
                    os.replace(manifest_path, backup_root / MANIFEST_NAME)
                (target_root / ".trellis/.runtime/sessions").mkdir(parents=True, exist_ok=True)
                _register_worktree(context)
            except Exception:
                for relative in reversed(moved_new):
                    current = target_root / relative
                    if current.is_dir() and not current.is_symlink():
                        shutil.rmtree(current)
                    elif current.exists() or current.is_symlink():
                        current.unlink()
                for relative in reversed(moved_old):
                    backup = backup_root / relative
                    if backup.exists() or backup.is_symlink():
                        shutil.move(str(backup), str(target_root / relative))
                manifest_backup = backup_root / MANIFEST_NAME
                if manifest_backup.exists() or manifest_backup.is_symlink():
                    shutil.move(str(manifest_backup), str(manifest_path))
                raise

    return {
        **_analyze(str(target_root)),
        "status": "migrated",
        "changed": True,
        "dryRun": False,
        "migrationPaths": managed,
    }


def _task_directories(target_root: Path) -> set[str]:
    """返回目标 worktree 当前任务目录集合。"""
    tasks_root = target_root / ".trellis/tasks"
    if not tasks_root.is_dir():
        return set()
    return {
        path.name
        for path in tasks_root.iterdir()
        if path.is_dir() and (path / "task.json").is_file()
    }


def _run_target_python(target_root: Path, script: Path, *args: str) -> None:
    """使用当前 Python 解释器运行目标分支自己的 Trellis 脚本。"""
    result = subprocess.run(
        [sys.executable, str(script), *args],
        cwd=target_root,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=30,
    )
    if result.returncode != 0:
        raise WorktreeSetupError(
            "task-command-failed",
            "目标分支 task 命令执行失败",
            command=[sys.executable, str(script), *args],
            stdout=result.stdout.strip(),
            stderr=result.stderr.strip(),
        )


def _working_tree_summary(source_root: Path) -> dict[str, Any]:
    """返回根仓当前未提交状态，并明确这些内容不属于基线。"""
    result = _git_run_bytes(
        source_root,
        "status",
        "--porcelain=v1",
        "-z",
        "--untracked-files=all",
        timeout=30,
    )
    if result.returncode != 0:
        raise WorktreeSetupError(
            "git-status-unavailable",
            "无法读取来源 worktree 状态",
            stderr=result.stderr.decode("utf-8", errors="replace").strip(),
        )
    entries = _parse_porcelain_z(result.stdout)

    conflict_codes = {"DD", "AU", "UD", "UA", "DU", "AA", "UU"}
    return {
        "clean": not entries,
        "includedInBase": False,
        "entries": entries,
        "counts": {
            "tracked": sum(entry["status"] != "??" for entry in entries),
            "staged": sum(entry["status"] != "??" and entry["status"][0] != " " for entry in entries),
            "unstaged": sum(entry["status"] != "??" and entry["status"][1] != " " for entry in entries),
            "untracked": sum(entry["status"] == "??" for entry in entries),
            "conflicts": sum(entry["status"] in conflict_codes for entry in entries),
        },
    }


def _submodule_names(source_root: Path, commit: str) -> dict[str, str]:
    """通过选定提交的 `.gitmodules` 解析 submodule 名称。"""
    output = _git_output(
        source_root,
        "config",
        "--blob",
        f"{commit}:.gitmodules",
        "--get-regexp",
        r"^submodule\..*\.path$",
    )
    if not output:
        return {}
    names: dict[str, str] = {}
    for line in output.splitlines():
        parts = line.split(None, 1)
        if len(parts) != 2:
            continue
        key, path = parts
        if key.startswith("submodule.") and key.endswith(".path"):
            names[path] = key[len("submodule.") : -len(".path")]
    return names


def _base_repositories(
    source_root: Path,
    source_context: dict[str, Any],
    commit: str,
    target_branch: str,
) -> list[dict[str, Any]]:
    """盘点基线提交中的根仓与全部递归 gitlink。"""
    result = _git_run_bytes(source_root, "ls-tree", "-r", "-z", commit, timeout=30)
    if result.returncode != 0:
        raise WorktreeSetupError(
            "create-base-inventory-failed",
            "无法盘点 base 提交中的 submodule",
            stderr=result.stderr.decode("utf-8", errors="replace").strip(),
        )
    names = _submodule_names(source_root, commit)
    repositories: list[dict[str, Any]] = [
        {
            "kind": "root",
            "name": source_root.name,
            "path": ".",
            "selected": True,
            "createsBranch": True,
            "targetBranch": target_branch,
            "sourcePath": _path_text(source_root),
            "baseCommit": commit,
            "initialized": True,
            "sourceBranch": source_context["branch"],
            "sourceHead": source_context["head"],
        }
    ]
    submodules: list[dict[str, Any]] = []
    for raw in result.stdout.split(b"\0"):
        if not raw or b"\t" not in raw:
            continue
        metadata, raw_path = raw.split(b"\t", 1)
        fields = metadata.split(b" ", 2)
        if len(fields) != 3 or fields[0] != b"160000":
            continue
        relative = os.fsdecode(raw_path)
        candidate = source_root / relative
        initialized = (candidate / ".git").exists()
        source_branch = None
        source_head = None
        if initialized and _git_toplevel(candidate) == candidate.resolve():
            source_branch = _git_output(candidate, "branch", "--show-current")
            source_head = _git_output(candidate, "rev-parse", "HEAD")
        else:
            initialized = False
        submodules.append(
            {
                "kind": "submodule",
                "name": names.get(relative, PurePosixPath(relative).name),
                "path": relative,
                "selected": False,
                "createsBranch": False,
                "targetBranch": None,
                "sourcePath": _path_text(candidate),
                "baseCommit": fields[2].decode("ascii"),
                "initialized": initialized,
                "sourceBranch": source_branch,
                "sourceHead": source_head,
            }
        )
    return repositories + sorted(submodules, key=lambda item: item["path"])


def _create_plan(args: argparse.Namespace) -> dict[str, Any]:
    """构造无写入的 create 计划及确认指纹。"""
    source_context = _worktree_context(_resolve_start(args.source))
    source_root = source_context["targetRoot"]
    target = Path(args.target).expanduser().resolve(strict=False)
    if target.exists() or target.is_symlink():
        raise WorktreeSetupError("create-target-exists", "create 目标路径已经存在", target=_path_text(target))
    if _git_output(source_root, "show-ref", "--verify", f"refs/heads/{args.branch}") is not None:
        raise WorktreeSetupError("create-branch-exists", "create 目标分支已经存在", branch=args.branch)

    requested_base = args.base
    effective_base = requested_base or source_context["branch"] or "HEAD"
    resolved_commit = _git_output(source_root, "rev-parse", "--verify", f"{effective_base}^{{commit}}")
    if resolved_commit is None:
        raise WorktreeSetupError("create-base-invalid", "base ref 无法解析", base=effective_base)
    task_entry = _git_run(source_root, "cat-file", "-e", f"{resolved_commit}:.trellis/scripts/task.py")
    if task_entry.returncode != 0:
        raise WorktreeSetupError(
            "local-trellis-missing",
            "base 提交不包含 `.trellis/scripts/task.py`",
            base=effective_base,
            resolvedCommit=resolved_commit,
        )

    source_developer = _developer_from_file(source_root)
    developer = args.developer or source_developer
    if not developer:
        raise WorktreeSetupError("developer-required", "create 需要 --developer 或来源 worktree 的本地身份")
    route_transfer = _route_preference_transfer(source_root, target, source_developer, developer)
    plan: dict[str, Any] = {
        "status": "confirmation-required",
        "changed": False,
        "requiresConfirmation": True,
        "source": {
            "repository": source_root.name,
            "root": _path_text(source_root),
            "gitCommonDir": _path_text(source_context["gitCommonDir"]),
            "gitDir": _path_text(source_context["gitDir"]),
            "worktreeId": source_context["worktreeId"],
            "branch": source_context["branch"],
            "head": source_context["head"],
            "workingTree": _working_tree_summary(source_root),
        },
        "base": {
            "requested": requested_base,
            "ref": effective_base,
            "resolvedCommit": resolved_commit,
            "defaultedFromCurrentBranch": requested_base is None and source_context["branch"] is not None,
        },
        "baseRef": effective_base,
        "target": {
            "root": _path_text(target),
            "branch": args.branch,
        },
        "targetRoot": _path_text(target),
        "branch": args.branch,
        "repositories": _base_repositories(source_root, source_context, resolved_commit, args.branch),
        "taskRequest": {
            "title": args.task_title,
            "slug": args.task_slug,
            "description": args.task_description,
        },
        "localStateTransfer": {
            "developer": {
                "action": "initialized",
                "name": developer,
                "sourceName": source_developer,
            },
            "routePreferences": route_transfer,
            "initialized": ["session-runtime"],
            "notInherited": list(NOT_INHERITED_LOCAL_STATE),
        },
    }
    fingerprint_payload = json.dumps(plan, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    fingerprint = hashlib.sha256(fingerprint_payload.encode("utf-8")).hexdigest()
    plan["confirmation"] = {"flag": "--yes", "fingerprint": fingerprint}
    return plan


def _create(args: argparse.Namespace) -> dict[str, Any]:
    """创建新 branch/worktree、planning task 和 registry 记录。"""
    plan = _create_plan(args)
    if not args.yes:
        return plan
    if not args.plan_fingerprint:
        raise WorktreeSetupError(
            "create-plan-fingerprint-required",
            "确认创建时必须提供预检计划指纹",
            plan=plan,
        )
    if args.plan_fingerprint != plan["confirmation"]["fingerprint"]:
        raise WorktreeSetupError(
            "create-plan-changed",
            "create 计划已变化，请重新确认最新计划",
            plan=plan,
        )

    source_context = _worktree_context(_resolve_start(args.source))
    source_root = source_context["targetRoot"]
    target = Path(plan["targetRoot"])
    developer = plan["localStateTransfer"]["developer"]["name"]
    base_ref = plan["base"]["ref"]
    base_commit = plan["base"]["resolvedCommit"]

    created_worktree = False
    registry_written = False
    task_relative: str | None = None
    rollback_errors: list[str] = []
    with _registry_lock(source_context["gitCommonDir"]):
        try:
            if target.exists() or target.is_symlink():
                raise WorktreeSetupError("create-target-exists", "create 目标路径已经存在", target=_path_text(target))
            if _git_output(source_root, "show-ref", "--verify", f"refs/heads/{args.branch}") is not None:
                raise WorktreeSetupError("create-branch-exists", "create 目标分支已经存在", branch=args.branch)
            _git_require(
                source_root,
                "worktree",
                "add",
                "-b",
                args.branch,
                str(target),
                base_commit,
                reason="worktree-create-failed",
                message="Git worktree 创建失败",
            )
            created_worktree = True
            target_plan = _analyze(str(target))
            if target_plan["status"] == "needs-init":
                raise WorktreeSetupError(
                    "local-trellis-missing",
                    "base 分支不包含本地 Trellis，create 已停止；请先在该分支安装",
                )
            if target_plan["status"] not in {"ready-local", "needs-prepare"}:
                raise WorktreeSetupError(
                    "worktree-create-not-ready",
                    "新 worktree 无法进入本地 ready 状态",
                    status=target_plan["status"],
                    conflicts=target_plan["conflicts"],
                )
            runtime_sessions = target / ".trellis/.runtime/sessions"
            runtime_sessions.mkdir(parents=True, exist_ok=True)
            if _developer_from_file(target) is None:
                _write_developer(target, developer)
            route_transfer = plan["localStateTransfer"]["routePreferences"]
            if route_transfer["action"] == "inherited":
                _write_route_preferences(target, route_transfer["values"])

            task_script = target / ".trellis/scripts/task.py"
            if not task_script.is_file():
                raise WorktreeSetupError("task-script-missing", "目标分支缺少 `.trellis/scripts/task.py`")
            before_tasks = _task_directories(target)
            create_args = [
                "create",
                args.task_title,
                "--slug",
                args.task_slug,
                "--assignee",
                developer,
                "--base-branch",
                base_ref,
                "--no-start",
            ]
            if args.task_description:
                create_args.extend(["--description", args.task_description])
            _run_target_python(target, task_script, *create_args)
            created_tasks = sorted(_task_directories(target) - before_tasks)
            if len(created_tasks) != 1:
                raise WorktreeSetupError(
                    "task-create-ambiguous",
                    "无法唯一识别本轮创建的 task 目录",
                    createdTasks=created_tasks,
                )
            task_relative = f".trellis/tasks/{created_tasks[0]}"
            _run_target_python(target, task_script, "set-branch", task_relative, args.branch)
            target_context = _worktree_context(target)
            _register_worktree(target_context, task=task_relative, developer=developer)
            registry_written = True
        except Exception as error:
            if registry_written:
                try:
                    registry = _load_registry(source_context["gitCommonDir"])
                    target_git_dir = _git_dir(target) if target.exists() else None
                    if target_git_dir is not None:
                        worktree_id = hashlib.sha256(_path_text(target_git_dir).encode("utf-8")).hexdigest()[:16]
                        registry["worktrees"].pop(worktree_id, None)
                        _write_json_atomic(_registry_path(source_context["gitCommonDir"]), registry)
                except Exception as rollback_error:
                    rollback_errors.append(f"registry: {rollback_error}")
            if created_worktree:
                result = _git_run(source_root, "worktree", "remove", "--force", str(target), timeout=30)
                if result.returncode != 0:
                    rollback_errors.append(f"worktree: {result.stderr.strip()}")
            if created_worktree:
                result = _git_run(source_root, "branch", "-D", args.branch, timeout=30)
                if result.returncode != 0:
                    rollback_errors.append(f"branch: {result.stderr.strip()}")
            if isinstance(error, WorktreeSetupError):
                error.details["rollbackErrors"] = rollback_errors
                raise
            raise WorktreeSetupError(
                "worktree-create-failed",
                "create 编排失败",
                error=str(error),
                rollbackErrors=rollback_errors,
            ) from error

    target_context = _worktree_context(target)
    return {
        **plan,
        "status": "created",
        "changed": True,
        "requiresConfirmation": False,
        "head": target_context["head"],
        "task": task_relative,
        "registry": _path_text(_registry_path(target_context["gitCommonDir"])),
        "handoff": {
            "cwd": _path_text(target),
            "workspaceRoot": _path_text(target),
            "task": task_relative,
            "command": f"cd {target}",
            "requiresNewSession": True,
            "reason": "新 worktree 需要独立会话，避免继承来源会话运行态",
        },
    }


def _task_status(target_root: Path, task_relative: str) -> str | None:
    """读取 registry 绑定 task 的版本化状态。"""
    task_path = target_root / task_relative / "task.json"
    status, data = _read_json(task_path)
    if status != "ok" or data is None:
        return None
    value = data.get("status")
    return value if isinstance(value, str) else None


def _active_sessions(target_root: Path) -> list[str]:
    """返回仍绑定任务或 untracked work 的本地 session 文件。"""
    sessions_root = target_root / ".trellis/.runtime/sessions"
    if not sessions_root.is_dir():
        return []
    active: list[str] = []
    for path in sorted(sessions_root.glob("*.json")):
        status, data = _read_json(path)
        if status != "ok" or data is None:
            active.append(path.name)
            continue
        if data.get("current_task") or data.get("untracked_flow") or data.get("auto_loop"):
            active.append(path.name)
    return active


def _listed_worktrees(start: Path) -> list[Path]:
    """按 Git 声明顺序返回同仓 worktree 根目录。"""
    output = _git_output(start, "worktree", "list", "--porcelain")
    if output is None:
        return []
    return [
        Path(line.split(" ", 1)[1]).expanduser().resolve()
        for line in output.splitlines()
        if line.startswith("worktree ")
    ]


def _control_worktree(target_root: Path) -> Path | None:
    """选择同仓另一个 worktree 作为 remove 控制端。"""
    for candidate in _listed_worktrees(target_root):
        if candidate != target_root and candidate.is_dir():
            return candidate
    return None


def _snapshot_local_state(target_root: Path, backup_root: Path) -> dict[str, list[str]]:
    """备份 remove 失败补偿所需的 Trellis 本地忽略文件。"""
    result = _git_run(
        target_root,
        "ls-files",
        "-z",
        "--others",
        "--ignored",
        "--exclude-standard",
        "--",
        *LOCAL_STATE_PATHS,
    )
    if result.returncode != 0:
        raise WorktreeSetupError(
            "local-state-snapshot-failed",
            "无法枚举 worktree 本地忽略文件",
            stderr=result.stderr.strip(),
        )
    files: list[str] = []
    for relative in result.stdout.split("\0"):
        if not relative:
            continue
        pure = PurePosixPath(relative.replace("\\", "/"))
        if pure.is_absolute() or ".." in pure.parts:
            raise WorktreeSetupError("local-state-path-invalid", "本地状态路径越界", path=relative)
        source = target_root.joinpath(*pure.parts)
        destination = backup_root.joinpath(*pure.parts)
        destination.parent.mkdir(parents=True, exist_ok=True)
        if source.is_symlink():
            os.symlink(os.readlink(source), destination, target_is_directory=source.is_dir())
        elif source.is_file():
            shutil.copy2(source, destination)
        else:
            continue
        files.append(pure.as_posix())

    directories = [
        relative
        for relative in (".trellis/.runtime", ".trellis/.runtime/sessions")
        if (target_root / relative).is_dir() and not (target_root / relative).is_symlink()
    ]
    return {"files": files, "directories": directories}


def _restore_local_state(target_root: Path, backup_root: Path, snapshot: dict[str, list[str]]) -> None:
    """恢复 remove 补偿前保存的 Trellis 本地忽略文件。"""
    for relative in snapshot["directories"]:
        (target_root / relative).mkdir(parents=True, exist_ok=True)
    for relative in snapshot["files"]:
        pure = PurePosixPath(relative)
        source = backup_root.joinpath(*pure.parts)
        destination = target_root.joinpath(*pure.parts)
        destination.parent.mkdir(parents=True, exist_ok=True)
        if source.is_symlink():
            os.symlink(os.readlink(source), destination, target_is_directory=source.is_dir())
        else:
            shutil.copy2(source, destination)


def _restore_removed_worktree(
    control: Path,
    context: dict[str, Any],
    backup_root: Path,
    snapshot: dict[str, list[str]],
) -> list[str]:
    """在 registry 提交失败后重建 worktree 并恢复本地状态。"""
    target_root = context["targetRoot"]
    branch = context["branch"]
    args = ["worktree", "add"]
    if branch:
        args.extend([str(target_root), branch])
    else:
        args.extend(["--detach", str(target_root), context["head"]])
    result = _git_run(control, *args, timeout=30)
    errors: list[str] = []
    if result.returncode != 0:
        errors.append(f"git-worktree: {result.stderr.strip()}")
        return errors
    try:
        _restore_local_state(target_root, backup_root, snapshot)
    except OSError as error:
        errors.append(f"local-state: {error}")
    return errors


def _remove(target_arg: str | None) -> dict[str, Any]:
    """在保留 branch 的前提下安全移除已注册 worktree。"""
    context = _worktree_context(_resolve_start(target_arg))
    target_root = context["targetRoot"]
    common_dir = context["gitCommonDir"]
    listed_worktrees = _listed_worktrees(target_root)
    if listed_worktrees and listed_worktrees[0] == target_root:
        raise WorktreeSetupError("remove-main-worktree-forbidden", "不能通过 worktree remove 删除主 worktree")
    control = _control_worktree(target_root)
    if control is None:
        raise WorktreeSetupError("remove-main-worktree-forbidden", "不能移除仓库唯一或主 worktree")
    worktree_lock = common_dir / REGISTRY_DIRECTORY / "locks" / f"{context['worktreeId']}.lock"
    if worktree_lock.exists():
        raise WorktreeSetupError("worktree-lock-held", "目标 worktree 仍有活动锁", lockPath=_path_text(worktree_lock))
    dirty_result = _git_run(target_root, "status", "--porcelain", "--untracked-files=all")
    if dirty_result.returncode != 0:
        raise WorktreeSetupError(
            "worktree-status-failed",
            "无法读取目标 worktree 状态",
            stderr=dirty_result.stderr.strip(),
        )
    dirty = dirty_result.stdout.strip()
    if dirty:
        raise WorktreeSetupError("worktree-dirty", "目标 worktree 含未提交修改，拒绝移除", dirty=dirty.splitlines())
    active_sessions = _active_sessions(target_root)
    if active_sessions:
        raise WorktreeSetupError("active-session", "目标 worktree 仍有活动 session", sessions=active_sessions)

    with _registry_lock(common_dir):
        registry = _load_registry(common_dir)
        entry = registry["worktrees"].get(context["worktreeId"])
        if not isinstance(entry, dict) or entry.get("path") != _path_text(target_root):
            raise WorktreeSetupError("registry-drift", "目标 worktree 与 registry 记录不一致")
        task_relative = entry.get("task")
        if isinstance(task_relative, str):
            task_status = _task_status(target_root, task_relative)
            if task_status is None:
                raise WorktreeSetupError("registry-task-drift", "registry 绑定的 task 不可读取", task=task_relative)
            if task_status in ACTIVE_TASK_STATUSES:
                raise WorktreeSetupError(
                    "active-task",
                    "目标 worktree 仍绑定未完成任务",
                    task=task_relative,
                    taskStatus=task_status,
                )
        transaction_root = common_dir / REGISTRY_DIRECTORY / "transactions"
        transaction_root.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix="remove-", dir=transaction_root) as temp_name:
            backup_root = Path(temp_name) / "local-state"
            backup_root.mkdir()
            snapshot = _snapshot_local_state(target_root, backup_root)
            _git_require(
                control,
                "worktree",
                "remove",
                str(target_root),
                reason="worktree-remove-failed",
                message="Git worktree 移除失败",
            )
            registry["worktrees"].pop(context["worktreeId"], None)
            try:
                _write_json_atomic(_registry_path(common_dir), registry)
            except Exception as error:
                rollback_errors = _restore_removed_worktree(control, context, backup_root, snapshot)
                if rollback_errors:
                    raise WorktreeSetupError(
                        "worktree-remove-rollback-failed",
                        "registry 更新失败且 worktree 补偿不完整",
                        error=str(error),
                        rollbackErrors=rollback_errors,
                    ) from error
                raise WorktreeSetupError(
                    "registry-write-failed",
                    "registry 更新失败，目标 worktree 和本地状态已恢复",
                    error=str(error),
                    worktreeRestored=True,
                ) from error
    return {
        "status": "removed",
        "changed": True,
        "targetRoot": _path_text(target_root),
        "branch": context["branch"],
        "branchPreserved": True,
    }


def _emit(payload: dict[str, Any], *, compact: bool) -> None:
    """向 stdout 输出 JSON。"""
    print(json.dumps(payload, ensure_ascii=False, sort_keys=compact, indent=None if compact else 2))


def build_parser() -> argparse.ArgumentParser:
    """构造 worktree CLI parser。

    Returns:
        已配置的 argparse parser。
    """
    parser = argparse.ArgumentParser(description="Trellis branch-local worktree helper")
    subparsers = parser.add_subparsers(dest="command", required=True)

    for command in ("status", "prepare", "migrate", "remove"):
        command_parser = subparsers.add_parser(command)
        command_parser.add_argument("--target", help="worktree path; defaults to cwd")
        command_parser.add_argument("--json", action="store_true", help="emit compact stable JSON")
        if command == "prepare":
            command_parser.add_argument("--developer", help="target-local developer identity")
            command_parser.add_argument("--source", help="explicit controlling worktree for preference inheritance")
            command_parser.add_argument(
                "--inherit-route-prefs",
                action="store_true",
                help="inherit normalized route preferences from --source",
            )
        if command == "migrate":
            command_parser.add_argument("--dry-run", action="store_true", help="validate migration without writing")

    create_parser = subparsers.add_parser("create")
    create_parser.add_argument("--source", help="existing repository worktree; defaults to cwd")
    create_parser.add_argument("--target", required=True, help="new worktree path")
    create_parser.add_argument("--branch", required=True, help="new branch name")
    create_parser.add_argument("--base", help="base commit or branch; defaults to current source branch")
    create_parser.add_argument("--task-title", required=True, help="planning task title")
    create_parser.add_argument("--task-slug", required=True, help="planning task slug")
    create_parser.add_argument("--task-description", help="planning task description")
    create_parser.add_argument("--developer", help="target-local developer identity")
    create_parser.add_argument("--yes", action="store_true", help="execute a previously confirmed create plan")
    create_parser.add_argument("--plan-fingerprint", help="fingerprint returned by create preflight")
    create_parser.add_argument("--json", action="store_true", help="emit compact stable JSON")
    return parser


def main(argv: list[str] | None = None) -> int:
    """执行 worktree CLI。

    Args:
        argv: 可选命令参数；缺省读取进程 argv。

    Returns:
        成功返回 0，结构化失败返回 1。
    """
    args = build_parser().parse_args(argv)
    compact = bool(getattr(args, "json", False))
    try:
        if args.command == "create":
            payload = _create(args)
        elif args.command == "remove":
            payload = _remove(args.target)
        else:
            plan = _analyze(args.target)
            if args.command == "prepare":
                payload = _prepare_local(
                    plan,
                    args.developer,
                    source=args.source,
                    inherit_route_prefs=args.inherit_route_prefs,
                )
            elif args.command == "migrate":
                payload = _migrate(plan, dry_run=args.dry_run)
            else:
                payload = plan
        _emit(payload, compact=compact)
        return 0
    except (WorktreeSetupError, OSError, tarfile.TarError) as error:
        if isinstance(error, WorktreeSetupError):
            payload = {"status": "error", "reason": error.reason, "message": str(error), **error.details}
        else:
            payload = {"status": "error", "reason": "worktree-operation-failed", "message": str(error)}
        _emit(payload, compact=compact)
        return 1


if __name__ == "__main__":
    sys.exit(main())
