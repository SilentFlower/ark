#!/usr/bin/env python3
"""为 Trellis 工作流捕获稳定的多仓 Git 证据。"""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
from pathlib import Path
from typing import Any

from common.config import get_git_packages


class GitEvidenceError(Exception):
    """携带稳定 reason code 的 Git 证据错误。"""

    def __init__(self, reason: str, message: str, **details: Any) -> None:
        """初始化结构化 Git 证据错误。

        Args:
            reason: 稳定机器错误码。
            message: 中文诊断说明。
            **details: 与错误相关的仓库或 Git 状态详情。
        """
        super().__init__(message)
        self.reason = reason
        self.details = details


def _run_git(repo: Path, args: list[str]) -> subprocess.CompletedProcess[bytes]:
    """在指定仓库执行 Git 并返回二进制输出。"""
    return subprocess.run(
        ["git", *args],
        cwd=repo,
        capture_output=True,
        check=False,
    )


def _decode(value: bytes) -> str:
    """按文件系统编码无损解码 Git 路径。"""
    return os.fsdecode(value)


def parse_porcelain_z(payload: bytes) -> list[dict[str, str]]:
    """解析 ``git status --porcelain=v1 -z`` 输出。

    Args:
        payload: Git porcelain 二进制输出。

    Returns:
        按路径稳定排序的状态对象列表。

    Raises:
        GitEvidenceError: 状态输出损坏或 rename/copy 缺少第二路径。
    """
    parts = payload.split(b"\0")
    entries: list[dict[str, str]] = []
    index = 0
    while index < len(parts):
        item = parts[index]
        index += 1
        if not item:
            continue
        if len(item) < 4 or item[2:3] != b" ":
            raise GitEvidenceError("git-status-invalid", "无法解析 Git porcelain 状态")
        status = item[:2].decode("ascii", errors="replace")
        entry = {"status": status, "path": _decode(item[3:])}
        if "R" in status or "C" in status:
            if index >= len(parts) or not parts[index]:
                raise GitEvidenceError(
                    "git-status-invalid",
                    "Git rename/copy 状态缺少第二路径",
                )
            entry["originalPath"] = _decode(parts[index])
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


def _repository_root(path: Path) -> Path | None:
    """解析路径所属 Git 工作树根目录，失败时返回 None。"""
    result = _run_git(path, ["rev-parse", "--show-toplevel"])
    if result.returncode != 0:
        return None
    value = result.stdout.decode("utf-8", errors="replace").strip()
    return Path(value).resolve() if value else None


def _relative_root(repo_root: Path, repository: Path) -> str:
    """返回仓库相对项目根路径。"""
    try:
        value = repository.relative_to(repo_root.resolve()).as_posix()
    except ValueError:
        value = str(repository)
    return value or "."


def discover_git_repositories(repo_root: Path) -> list[Path]:
    """发现项目根仓、递归 submodule 和配置独立 Git package。

    Args:
        repo_root: Trellis 项目根目录。

    Returns:
        去重并按项目相对路径稳定排序的 Git 仓库根目录。

    Raises:
        GitEvidenceError: 项目根不是 Git 仓库或配置 package 逃逸项目根。
    """
    root = _repository_root(repo_root)
    if root is None:
        raise GitEvidenceError("git-root-unreadable", "无法解析项目 Git 根目录")

    repositories: dict[str, Path] = {_relative_root(root, root): root}
    submodules = _run_git(
        root,
        ["submodule", "foreach", "--recursive", "--quiet", "pwd"],
    )
    if submodules.returncode != 0:
        raise GitEvidenceError(
            "git-submodule-unreadable",
            submodules.stderr.decode("utf-8", errors="replace").strip()
            or "无法读取递归 Git submodule",
            repository=str(root),
        )
    for raw in submodules.stdout.decode("utf-8", errors="replace").splitlines():
        candidate = Path(raw.strip()).resolve()
        if candidate.is_dir():
            repositories[_relative_root(root, candidate)] = candidate

    for _, configured_path in sorted(get_git_packages(root).items()):
        raw_path = Path(configured_path)
        candidate = (root / raw_path).resolve() if not raw_path.is_absolute() else raw_path.resolve()
        try:
            candidate.relative_to(root)
        except ValueError as exc:
            raise GitEvidenceError(
                "git-package-path-invalid",
                "配置的独立 Git package 逃逸项目根目录",
                path=str(configured_path),
            ) from exc
        repository = _repository_root(candidate)
        if repository is None or repository != candidate:
            raise GitEvidenceError(
                "git-package-unreadable",
                "配置的独立 Git package 不是可独立读取的仓库根目录",
                path=str(configured_path),
            )
        repositories[_relative_root(root, repository)] = repository

    return [repositories[key] for key in sorted(repositories)]


def integration_in_progress(repository: Path) -> list[str]:
    """返回仓库中未完成的 Git 集成状态。

    Args:
        repository: Git 仓库根目录。

    Returns:
        稳定排序的 merge/rebase/cherry-pick/revert 状态列表。

    Raises:
        GitEvidenceError: 无法读取仓库 Git 目录。
    """
    result = _run_git(repository, ["rev-parse", "--git-path", "."])
    if result.returncode != 0:
        raise GitEvidenceError(
            "git-integration-state-unreadable",
            result.stderr.decode("utf-8", errors="replace").strip()
            or "无法读取 Git 集成状态",
            repository=str(repository),
        )
    git_dir = Path(result.stdout.decode("utf-8", errors="replace").strip())
    if not git_dir.is_absolute():
        git_dir = repository / git_dir
    markers = {
        "MERGE_HEAD": "merge",
        "rebase-merge": "rebase",
        "rebase-apply": "rebase",
        "CHERRY_PICK_HEAD": "cherry-pick",
        "REVERT_HEAD": "revert",
    }
    return sorted({name for marker, name in markers.items() if (git_dir / marker).exists()})


def _sha256(payload: bytes) -> str:
    """返回二进制内容的 SHA-256。"""
    return hashlib.sha256(payload).hexdigest()


def _path_digest(path: Path) -> str:
    """计算普通文件或软链的稳定摘要。"""
    try:
        if path.is_symlink():
            return _sha256(os.readlink(path).encode("utf-8", errors="surrogateescape"))
        if path.is_file():
            return _sha256(path.read_bytes())
    except OSError:
        return "unreadable"
    return "missing"


def _git_payload(repository: Path, args: list[str], reason: str) -> bytes:
    """读取 Git 二进制输出，失败时抛出结构化错误。"""
    result = _run_git(repository, args)
    if result.returncode != 0:
        raise GitEvidenceError(
            reason,
            result.stderr.decode("utf-8", errors="replace").strip() or "Git 证据读取失败",
            repository=str(repository),
        )
    return result.stdout


def _repository_evidence(
    repo_root: Path,
    repository: Path,
    *,
    block_unsafe: bool,
) -> dict[str, Any]:
    """捕获单个仓库的 HEAD、状态和内容指纹。"""
    status_payload = _git_payload(
        repository,
        ["status", "--porcelain=v1", "-z", "--untracked-files=all"],
        "git-status-unreadable",
    )
    status = parse_porcelain_z(status_payload)
    conflicts = [
        entry
        for entry in status
        if "U" in entry["status"] or entry["status"] in {"AA", "DD"}
    ]
    integration = integration_in_progress(repository)
    if block_unsafe and (conflicts or integration):
        raise GitEvidenceError(
            "git-global-safety-block",
            "存在冲突或未完成 Git 集成",
            repository=_relative_root(repo_root, repository),
            conflicts=conflicts,
            integration=integration,
        )

    head_result = _run_git(repository, ["rev-parse", "HEAD"])
    head = (
        head_result.stdout.decode("utf-8", errors="replace").strip()
        if head_result.returncode == 0
        else None
    )
    worktree_diff = _git_payload(
        repository,
        ["diff", "--binary", "--no-ext-diff"],
        "git-worktree-diff-unreadable",
    )
    index_diff = _git_payload(
        repository,
        ["diff", "--cached", "--binary", "--no-ext-diff"],
        "git-index-diff-unreadable",
    )
    untracked = [
        {"path": entry["path"], "sha256": _path_digest(repository / entry["path"])}
        for entry in status
        if entry["status"] == "??"
    ]
    evidence = {
        "root": _relative_root(repo_root, repository),
        "head": head,
        "status": status,
        "worktreeDiffSha256": _sha256(worktree_diff),
        "indexDiffSha256": _sha256(index_diff),
        "untracked": untracked,
        "conflicts": conflicts,
        "integration": integration,
    }
    evidence["fingerprint"] = _sha256(
        json.dumps(evidence, ensure_ascii=False, sort_keys=True).encode("utf-8")
    )
    return evidence


def capture_workspace_evidence(
    repo_root: Path,
    *,
    block_unsafe: bool = True,
) -> dict[str, Any]:
    """捕获项目全部 Git 仓库的稳定 workspace 证据。

    Args:
        repo_root: Trellis 项目根目录。
        block_unsafe: 是否在冲突或未完成 Git 集成时阻止捕获。

    Returns:
        包含仓库证据列表和全局 fingerprint 的版本化对象。

    Raises:
        GitEvidenceError: 任一仓库证据不完整或存在全局安全阻断。
    """
    repositories = [
        _repository_evidence(
            repo_root.resolve(),
            repository,
            block_unsafe=block_unsafe,
        )
        for repository in discover_git_repositories(repo_root)
    ]
    payload = {"version": 1, "repositories": repositories}
    payload["fingerprint"] = _sha256(
        json.dumps(payload, ensure_ascii=False, sort_keys=True).encode("utf-8")
    )
    return payload


def task_compat_baseline(evidence: dict[str, Any]) -> dict[str, Any]:
    """把多仓证据转换为兼容旧 task intent 字段的 baseline。

    Args:
        evidence: ``capture_workspace_evidence`` 返回值。

    Returns:
        保留根仓 ``head/status`` 的多仓 baseline。
    """
    repositories = evidence.get("repositories")
    values = repositories if isinstance(repositories, list) else []
    root = next(
        (entry for entry in values if isinstance(entry, dict) and entry.get("root") == "."),
        {},
    )
    return {
        "head": root.get("head"),
        "status": root.get("status", []),
        "repositories": values,
        "fingerprint": evidence.get("fingerprint"),
    }
