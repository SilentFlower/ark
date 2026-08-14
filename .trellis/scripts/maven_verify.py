#!/usr/bin/env python3
"""为 Trellis 生成、执行并复用 Maven 分层验证证据。"""

from __future__ import annotations

import argparse
import hashlib
import json
import ntpath
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
import xml.etree.ElementTree as ET
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable


SCHEMA_VERSION = 1
KIND = "trellis-maven-verification"
PLAN_KIND = "trellis-maven-verification-plan"
DEFAULT_EVIDENCE_DIR = Path(".trellis/.runtime/maven-verification")
LIFECYCLE_ORDER = [
    "validate",
    "initialize",
    "generate-sources",
    "process-sources",
    "generate-resources",
    "process-resources",
    "compile",
    "process-classes",
    "generate-test-sources",
    "process-test-sources",
    "generate-test-resources",
    "process-test-resources",
    "test-compile",
    "process-test-classes",
    "test",
    "prepare-package",
    "package",
    "pre-integration-test",
    "integration-test",
    "post-integration-test",
    "verify",
    "install",
    "deploy",
]
LIFECYCLE_RANK = {phase: index for index, phase in enumerate(LIFECYCLE_ORDER)}
PLAN_GOALS = ("validate", "compile", "test", "package", "verify", "install")
ARTIFACT_NAMES = {
    "sources",
    "javadoc",
    "assembly",
    "shade",
    "repackage",
    "copy-dependencies",
}
PROPERTY_PATTERN = re.compile(r"\$\{([^}]+)\}")
TEST_SUMMARY_PATTERN = re.compile(
    r"Tests run:\s*(\d+)\s*,\s*Failures:\s*(\d+)\s*,\s*Errors:\s*(\d+)\s*,\s*Skipped:\s*(\d+)",
)
EVIDENCE_FILE_PATTERN = re.compile(r"^\d{17}-[0-9a-f]{12}\.json$")
SOURCE_SKIP_MIN_VERSION = (3, 0, 1)
COMPILER_SOURCE_STALE_MIN_VERSION = (3, 1, 0)
THREADS_PATTERN = re.compile(
    r"^(?:[1-9]\d*|(?:[1-9]\d*(?:\.\d+)?|0\.\d*[1-9]\d*)C)$"
)
WINDOWS_PATH_PATTERN = re.compile(r"^[A-Za-z]:[\\/]")
WINDOWS_MAVEN_SUFFIXES = (".cmd", ".bat", ".exe")
DEFAULT_GOAL_PHASES = {
    ("org.apache.maven.plugins:maven-source-plugin", "jar"): "package",
    ("org.apache.maven.plugins:maven-source-plugin", "jar-no-fork"): "package",
    ("org.apache.maven.plugins:maven-source-plugin", "test-jar"): "package",
    ("org.apache.maven.plugins:maven-source-plugin", "test-jar-no-fork"): "package",
    ("org.springframework.boot:spring-boot-maven-plugin", "repackage"): "package",
}


class MavenVerifyError(Exception):
    """携带稳定 reason code 的 Maven 验证错误。"""

    def __init__(self, code: str, message: str, **details: Any) -> None:
        """初始化结构化错误。

        Args:
            code: 稳定机器错误码。
            message: 中文诊断说明。
            **details: 额外错误上下文。
        """
        super().__init__(message)
        self.code = code
        self.details = details


@dataclass
class MavenModule:
    """表示 reactor 中一个可定位的 Maven module。"""

    path: str
    pom: Path
    group_id: str | None
    artifact_id: str
    version: str | None
    packaging: str
    properties: dict[str, str] = field(default_factory=dict)
    dependencies: list[tuple[str | None, str]] = field(default_factory=list)
    bindings: list[dict[str, Any]] = field(default_factory=list)
    java_target: str | None = None

    @property
    def coordinate(self) -> str:
        """返回稳定的 groupId:artifactId 坐标。"""
        return f"{self.group_id or ''}:{self.artifact_id}"


@dataclass(frozen=True)
class MavenCommand:
    """描述 Maven 的构建侧、逻辑可执行文件与宿主启动方式。"""

    build_side: str
    executable: str
    source: str
    runner: str
    project_filesystem: dict[str, Any] | None = None

    def to_json(self) -> dict[str, Any]:
        """返回可冻结进 plan/evidence 的 Maven 命令描述。"""
        return {
            "buildSide": self.build_side,
            "executable": self.executable,
            "source": self.source,
            "runner": self.runner,
            "projectFilesystem": self.project_filesystem,
        }


def _utc_now() -> str:
    """返回毫秒精度 UTC RFC3339 时间。"""
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _sha256_bytes(payload: bytes) -> str:
    """返回二进制内容的 SHA-256。"""
    return hashlib.sha256(payload).hexdigest()


def _stable_json(data: Any, *, pretty: bool = True) -> str:
    """序列化稳定 UTF-8 JSON 文本。"""
    return json.dumps(
        data,
        ensure_ascii=False,
        sort_keys=True,
        indent=2 if pretty else None,
        separators=None if pretty else (",", ":"),
    ) + "\n"


def _semantic_fingerprint(data: Any) -> str:
    """计算不受 JSON key 顺序影响的语义指纹。"""
    payload = json.dumps(data, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    return _sha256_bytes(payload.encode("utf-8"))


def _write_text_atomic(path: Path, content: str) -> None:
    """原子写入文本，替换失败时保留旧文件。"""
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary_path = Path(temporary)
    try:
        with os.fdopen(fd, "w", encoding="utf-8", newline="\n") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_path, path)
    except Exception:
        temporary_path.unlink(missing_ok=True)
        raise


def _write_json_atomic(path: Path, data: dict[str, Any]) -> None:
    """原子写入稳定 JSON。"""
    _write_text_atomic(path, _stable_json(data))


def _validate_json_object(
    data: Any,
    *,
    source: str,
    expected_kind: str | None = None,
) -> dict[str, Any]:
    """校验来自文件或标准输入的版本化 JSON 对象。"""
    if not isinstance(data, dict) or data.get("schemaVersion") != SCHEMA_VERSION:
        raise MavenVerifyError("schema-unsupported", f"JSON schema 不受支持：{source}", source=source)
    if expected_kind is not None and data.get("kind") != expected_kind:
        raise MavenVerifyError("kind-mismatch", f"JSON kind 不匹配：{source}", source=source)
    return data


def _read_json(path: Path, expected_kind: str | None = None) -> dict[str, Any]:
    """读取并校验文件中的版本化 JSON 对象。"""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise MavenVerifyError("json-unreadable", f"无法读取 JSON：{path}", path=str(path)) from error
    return _validate_json_object(data, source=str(path), expected_kind=expected_kind)


def _read_json_stdin(expected_kind: str | None = None) -> dict[str, Any]:
    """读取并校验标准输入中的版本化 JSON 对象。"""
    try:
        data = json.load(sys.stdin)
    except (OSError, json.JSONDecodeError) as error:
        raise MavenVerifyError("json-unreadable", "无法从标准输入读取 JSON", source="<stdin>") from error
    return _validate_json_object(data, source="<stdin>", expected_kind=expected_kind)


def _run(
    argv: list[str],
    *,
    cwd: Path,
    text: bool = False,
    timeout: float | None = None,
) -> subprocess.CompletedProcess[Any]:
    """不经过 shell 执行命令。"""
    try:
        return subprocess.run(
            argv,
            cwd=cwd,
            capture_output=True,
            text=text,
            check=False,
            timeout=timeout,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise MavenVerifyError(
            "command-unavailable",
            f"命令不可用：{argv[0]}",
            argv=argv,
            cwd=str(cwd),
        ) from error


def _is_wsl() -> bool:
    """判断当前 Python 是否运行在 WSL。"""
    if os.name == "nt":
        return False
    release = Path("/proc/sys/kernel/osrelease")
    try:
        return "microsoft" in release.read_text(encoding="utf-8").lower()
    except OSError:
        return False


def _is_windows_path(value: str) -> bool:
    """判断字符串是否为 Windows drive 或 UNC 路径。"""
    return bool(WINDOWS_PATH_PATTERN.match(value)) or value.startswith(("\\\\", "//"))


def _is_wsl_windows_filesystem(filesystem: dict[str, Any] | None) -> bool:
    """根据 mount source 识别 WSL 中的 Windows 文件系统。"""
    if not _is_wsl() or not filesystem:
        return False
    filesystem_type = str(filesystem.get("type") or "").lower()
    source = str(filesystem.get("source") or "")
    return filesystem_type in {"9p", "drvfs"} and (
        _is_windows_path(source) or bool(re.fullmatch(r"[A-Za-z]:", source))
    )


def _decode_command_output(payload: bytes, build_side: str) -> str:
    """按构建侧解码命令输出，并对 Windows 本地代码页降级。"""
    encodings = ("utf-8", "gb18030") if build_side == "windows" else ("utf-8",)
    for encoding in encodings:
        try:
            return payload.decode(encoding)
        except UnicodeDecodeError:
            continue
    return payload.decode(encodings[-1], errors="replace")


def _host_to_windows_path(path: Path) -> str:
    """把 WSL 可见路径转换为 Windows 路径。"""
    resolved = path.resolve()
    if os.name == "nt":
        return str(resolved)
    if not _is_wsl():
        raise MavenVerifyError(
            "path-side-mismatch",
            "当前宿主无法把 POSIX 路径映射到 Windows 构建侧",
            path=str(resolved),
        )
    result = _run(["wslpath", "-w", str(resolved)], cwd=resolved if resolved.is_dir() else resolved.parent)
    value = _decode_command_output(result.stdout, "windows").strip()
    if result.returncode != 0 or not value:
        raise MavenVerifyError("path-conversion-failed", "无法转换 Windows 构建路径", path=str(resolved))
    return value


def _windows_to_host_path(value: str, cwd: Path) -> Path:
    """把 Windows 构建路径转换为当前 Python 可访问路径。"""
    if os.name == "nt":
        return Path(value).resolve()
    if not _is_wsl():
        raise MavenVerifyError(
            "path-side-mismatch",
            "当前宿主无法访问 Windows 构建路径",
            path=value,
        )
    result = _run(["wslpath", "-u", value], cwd=cwd)
    converted = _decode_command_output(result.stdout, "windows").strip()
    if result.returncode != 0 or not converted:
        raise MavenVerifyError("path-conversion-failed", "无法转换 WSL 宿主路径", path=value)
    return Path(converted).resolve()


def _project_build_side(cwd: Path) -> tuple[str, dict[str, Any] | None]:
    """根据 Maven 根目录所在的原生文件系统决定构建侧。"""
    filesystem = _filesystem_info(str(cwd))
    if os.name == "nt":
        return "windows", filesystem
    if _is_wsl_windows_filesystem(filesystem):
        return "windows", filesystem
    return "posix", filesystem


def _windows_environment(cwd: Path) -> dict[str, str]:
    """读取 Windows 构建侧环境，避免混用 WSL 的 Maven/JDK 配置。"""
    if os.name == "nt":
        return {key.upper(): value for key, value in os.environ.items()}
    result = _run(["cmd.exe", "/u", "/d", "/c", "set"], cwd=cwd)
    output = result.stdout.decode("utf-16le", errors="replace")
    if result.returncode != 0:
        raise MavenVerifyError("windows-environment-unreadable", "无法读取 Windows 构建环境")
    environment: dict[str, str] = {}
    for line in output.splitlines():
        if "=" not in line or line.startswith("="):
            continue
        key, value = line.split("=", 1)
        environment[key.upper()] = value
    return environment


def _build_environment(build_side: str, cwd: Path) -> dict[str, str]:
    """返回与 Maven 构建侧一致的环境变量视图。"""
    if build_side == "windows":
        return _windows_environment(cwd)
    return dict(os.environ)


def _maven_process_argv(command: MavenCommand, arguments: Iterable[str]) -> list[str]:
    """把逻辑 Maven argv 转换为当前 Python 宿主可启动的 argv。"""
    logical = [command.executable, *[str(value) for value in arguments]]
    if command.runner == "windows-cmd":
        unsafe = [value for value in logical if any(character in value for character in "&|<>^%\r\n\x00")]
        if unsafe:
            raise MavenVerifyError(
                "command-argument-invalid",
                "Windows Maven argv 含 cmd.exe 会二次解释的字符",
                arguments=unsafe,
            )
        return ["cmd.exe", "/d", "/c", "call", *logical]
    return logical


def _maven_command_from_toolchain(toolchain: dict[str, Any]) -> MavenCommand:
    """从冻结工具链恢复 Maven 命令描述。"""
    maven = toolchain.get("maven", {})
    executable = maven.get("executable")
    build_side = maven.get("buildSide")
    runner = maven.get("runner")
    if not all(isinstance(value, str) and value for value in (executable, build_side, runner)):
        raise MavenVerifyError("plan-toolchain-invalid", "计划缺少 Maven 构建侧或执行包装")
    return MavenCommand(
        build_side=build_side,
        executable=executable,
        source=str(maven.get("source") or "frozen"),
        runner=runner,
        project_filesystem=maven.get("projectFilesystem"),
    )


def _is_project_wrapper_command(command: MavenCommand, cwd: Path) -> bool:
    """判断冻结命令是否实际指向当前 Maven 根的项目 wrapper。"""
    if command.source == "project-wrapper":
        return True
    wrapper = cwd / ("mvnw.cmd" if command.build_side == "windows" else "mvnw")
    if not wrapper.is_file():
        return False
    executable = (
        _windows_to_host_path(command.executable, cwd)
        if command.build_side == "windows"
        else Path(command.executable)
    )
    return executable.resolve() == wrapper.resolve()


def _run_maven(
    command: MavenCommand,
    arguments: Iterable[str],
    *,
    cwd: Path,
    timeout: float | None = None,
) -> tuple[subprocess.CompletedProcess[bytes], list[str]]:
    """使用冻结构建侧执行 Maven，并返回真实宿主 argv。"""
    host_argv = _maven_process_argv(command, arguments)
    return _run(host_argv, cwd=cwd, timeout=timeout), host_argv


def _git_root(path: Path) -> Path:
    """解析 Git 工作树根目录。"""
    result = _run(["git", "rev-parse", "--show-toplevel"], cwd=path, text=True)
    if result.returncode != 0 or not result.stdout.strip():
        raise MavenVerifyError("git-root-unreadable", "无法解析 Git 工作树根目录")
    return Path(result.stdout.strip()).resolve()


def _path_from_root(root: Path, value: Path) -> str:
    """尽量返回相对根目录的 POSIX 路径。"""
    try:
        relative = value.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return str(value.resolve())
    return relative or "."


def _git_pathspec(repo_root: Path, scope: Path) -> str:
    """返回 Git pathspec 使用的仓库相对路径。"""
    try:
        value = scope.resolve().relative_to(repo_root.resolve()).as_posix()
    except ValueError as error:
        raise MavenVerifyError("maven-root-outside-git", "Maven 根目录不在 Git 工作树中") from error
    return value or "."


def _scope_pathspecs(pathspec: str) -> list[str]:
    """返回 Maven 范围及运行时/构建产物排除规则。"""
    prefix = "" if pathspec == "." else f"{pathspec}/"
    return [
        pathspec,
        f":(glob,exclude){prefix}.trellis/.runtime/**",
        f":(glob,exclude){prefix}**/target/**",
    ]


def _changed_paths(repo_root: Path, maven_root: Path) -> list[str]:
    """列出 Maven 根下 staged、unstaged 与 untracked 变更路径。"""
    pathspec = _git_pathspec(repo_root, maven_root)
    pathspecs = _scope_pathspecs(pathspec)
    result = _run(
        ["git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", *pathspecs],
        cwd=repo_root,
    )
    if result.returncode != 0:
        raise MavenVerifyError("git-status-unreadable", "无法读取 Maven 范围 Git 状态")
    values: set[str] = set()
    parts = result.stdout.split(b"\0")
    index = 0
    while index < len(parts):
        item = parts[index]
        index += 1
        if not item:
            continue
        if len(item) < 4 or item[2:3] != b" ":
            raise MavenVerifyError("git-status-invalid", "无法解析 Git porcelain 状态")
        status = item[:2].decode("ascii", errors="replace")
        values.add(os.fsdecode(item[3:]))
        if "R" in status or "C" in status:
            if index >= len(parts) or not parts[index]:
                raise MavenVerifyError("git-status-invalid", "Git rename/copy 状态缺少原路径")
            values.add(os.fsdecode(parts[index]))
            index += 1
    return sorted(values)


def _file_digest(path: Path) -> str:
    """计算普通文件或软链的稳定摘要。"""
    try:
        if path.is_symlink():
            return _sha256_bytes(os.readlink(path).encode("utf-8", errors="surrogateescape"))
        if path.is_file():
            return _sha256_bytes(path.read_bytes())
    except OSError:
        return "unreadable"
    return "missing"


def _side_executable_digest(value: str | None, cwd: Path, build_side: str) -> str | None:
    """计算构建侧可执行文件摘要，不运行目标程序。"""
    if not value:
        return None
    if build_side == "windows":
        return _file_digest(_windows_to_host_path(value, cwd))
    candidate = Path(value)
    if not candidate.is_absolute() and candidate.parent == Path("."):
        resolved = shutil.which(value)
        if not resolved:
            return "missing"
        candidate = Path(resolved)
    elif not candidate.is_absolute():
        candidate = cwd / candidate
    return _file_digest(candidate.resolve())


def _workspace_evidence(repo_root: Path, maven_root: Path) -> dict[str, Any]:
    """捕获 Maven 根范围的 Git 内容证据。"""
    pathspec = _git_pathspec(repo_root, maven_root)
    pathspecs = _scope_pathspecs(pathspec)
    head = _run(["git", "rev-parse", "HEAD"], cwd=repo_root, text=True)
    if head.returncode != 0:
        raise MavenVerifyError("git-head-unreadable", "无法读取 Git HEAD")
    worktree = _run(
        ["git", "diff", "--binary", "--no-ext-diff", "--", *pathspecs],
        cwd=repo_root,
    )
    index = _run(
        ["git", "diff", "--cached", "--binary", "--no-ext-diff", "--", *pathspecs],
        cwd=repo_root,
    )
    if worktree.returncode != 0 or index.returncode != 0:
        raise MavenVerifyError("git-diff-unreadable", "无法读取 Maven 范围 Git diff")
    changed = _changed_paths(repo_root, maven_root)
    untracked: list[dict[str, str]] = []
    status = _run(
        ["git", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", *pathspecs],
        cwd=repo_root,
    )
    if status.returncode != 0:
        raise MavenVerifyError("git-status-unreadable", "无法读取 Maven 范围 untracked 状态")
    for item in status.stdout.split(b"\0"):
        if item.startswith(b"?? "):
            relative = os.fsdecode(item[3:])
            untracked.append({"path": relative, "sha256": _file_digest(repo_root / relative)})
    evidence = {
        "root": ".",
        "head": head.stdout.strip(),
        "scope": pathspec,
        "worktreeDiffSha256": _sha256_bytes(worktree.stdout),
        "indexDiffSha256": _sha256_bytes(index.stdout),
        "untracked": sorted(untracked, key=lambda item: item["path"]),
        "changedPaths": changed,
    }
    evidence["fingerprint"] = _semantic_fingerprint(evidence)
    return evidence


def _local_name(tag: str) -> str:
    """去掉 XML namespace。"""
    return tag.rsplit("}", 1)[-1]


def _child(element: ET.Element | None, name: str) -> ET.Element | None:
    """返回第一个指定 local-name 的直接子元素。"""
    if element is None:
        return None
    return next((item for item in element if _local_name(item.tag) == name), None)


def _children(element: ET.Element | None, name: str) -> list[ET.Element]:
    """返回全部指定 local-name 的直接子元素。"""
    if element is None:
        return []
    return [item for item in element if _local_name(item.tag) == name]


def _text(element: ET.Element | None, default: str | None = None) -> str | None:
    """读取 XML 元素去空白文本。"""
    if element is None or element.text is None:
        return default
    value = element.text.strip()
    return value or default


def _parse_xml(path: Path) -> ET.Element:
    """读取 Maven XML 根元素。"""
    try:
        return ET.parse(path).getroot()
    except (OSError, ET.ParseError) as error:
        raise MavenVerifyError("pom-unreadable", f"无法解析 POM：{path}", path=str(path)) from error


def _properties(project: ET.Element) -> dict[str, str]:
    """解析 project properties。"""
    values: dict[str, str] = {}
    properties = _child(project, "properties")
    if properties is not None:
        for item in list(properties):
            values[_local_name(item.tag)] = _text(item, "") or ""
    return values


def _resolve_property(value: str | None, properties: dict[str, str]) -> str | None:
    """有限次展开 Maven 属性，保留无法解析的占位符。"""
    if value is None:
        return None
    result = value
    for _ in range(8):
        replaced = PROPERTY_PATTERN.sub(lambda match: properties.get(match.group(1), match.group(0)), result)
        if replaced == result:
            break
        result = replaced
    return result


def _configuration_text(plugin: ET.Element, name: str) -> str | None:
    """读取 plugin configuration 的简单标量。"""
    return _text(_child(_child(plugin, "configuration"), name))


def _plugin_bindings(
    project: ET.Element,
    source: str,
    *,
    module_path: str | None = None,
    global_inherited: bool = False,
) -> list[dict[str, Any]]:
    """提取 build/plugins execution 绑定，并补全已确认的 goal 默认阶段。"""
    build = _child(project, "build")
    plugins = _child(build, "plugins")
    bindings: list[dict[str, Any]] = []
    for plugin in _children(plugins, "plugin"):
        group_id = _text(_child(plugin, "groupId"), "org.apache.maven.plugins")
        artifact_id = _text(_child(plugin, "artifactId"))
        version = _text(_child(plugin, "version"))
        inherited = (_text(_child(plugin, "inherited"), "true") or "true").lower() != "false"
        if not artifact_id:
            continue
        executions = _child(plugin, "executions")
        for execution in _children(executions, "execution"):
            execution_phase = _text(_child(execution, "phase"))
            execution_id = _text(_child(execution, "id"), "default")
            for goal_node in _children(_child(execution, "goals"), "goal"):
                goal = _text(goal_node)
                if not goal:
                    continue
                phase = execution_phase
                phase_source = "execution"
                if phase is None:
                    phase = DEFAULT_GOAL_PHASES.get((f"{group_id}:{artifact_id}", goal))
                    phase_source = "default-goal-mapping" if phase else "unknown"
                bindings.append(
                    {
                        "plugin": f"{group_id}:{artifact_id}",
                        "version": version,
                        "goal": goal,
                        "phase": phase,
                        "phaseSource": phase_source,
                        "executionId": execution_id,
                        "source": source,
                        "module": None if global_inherited and inherited else module_path,
                        "inherited": inherited,
                    }
                )
    return bindings


def _java_target(project: ET.Element, properties: dict[str, str]) -> str | None:
    """从 properties 或 compiler plugin configuration 识别 Java 目标。"""
    for key in ("maven.compiler.release", "maven.compiler.target", "java.version"):
        value = _resolve_property(properties.get(key), properties)
        if value:
            return value
    plugins = _child(_child(project, "build"), "plugins")
    for plugin in _children(plugins, "plugin"):
        if _text(_child(plugin, "artifactId")) != "maven-compiler-plugin":
            continue
        for name in ("release", "target", "source"):
            value = _resolve_property(_configuration_text(plugin, name), properties)
            if value:
                return value
    return None


def _read_module(pom: Path, root_dir: Path, inherited: dict[str, str] | None = None) -> MavenModule:
    """读取一个 module POM 的坐标、依赖和插件绑定。"""
    project = _parse_xml(pom)
    parent = _child(project, "parent")
    raw_group = _text(_child(project, "groupId")) or _text(_child(parent, "groupId"))
    raw_version = _text(_child(project, "version")) or _text(_child(parent, "version"))
    raw_artifact = _text(_child(project, "artifactId"))
    if not raw_artifact:
        raise MavenVerifyError("pom-coordinate-missing", f"POM 缺少 artifactId：{pom}")
    props = dict(inherited or {})
    props.update(_properties(project))
    props.update(
        {
            "project.groupId": raw_group or "",
            "pom.groupId": raw_group or "",
            "project.artifactId": raw_artifact,
            "pom.artifactId": raw_artifact,
            "project.version": raw_version or "",
            "pom.version": raw_version or "",
        }
    )
    group_id = _resolve_property(raw_group, props)
    artifact_id = _resolve_property(raw_artifact, props) or raw_artifact
    version = _resolve_property(raw_version, props)
    dependencies: list[tuple[str | None, str]] = []
    for dependency in _children(_child(project, "dependencies"), "dependency"):
        dep_artifact = _resolve_property(_text(_child(dependency, "artifactId")), props)
        if dep_artifact:
            dependencies.append(
                (
                    _resolve_property(_text(_child(dependency, "groupId")), props),
                    dep_artifact,
                )
            )
    relative = pom.parent.resolve().relative_to(root_dir.resolve()).as_posix() or "."
    packaging = _text(_child(project, "packaging"), "jar") or "jar"
    return MavenModule(
        path=relative,
        pom=pom.resolve(),
        group_id=group_id,
        artifact_id=artifact_id,
        version=version,
        packaging=packaging,
        properties=props,
        dependencies=dependencies,
        bindings=_plugin_bindings(
            project,
            f"pom:{relative}",
            module_path=relative,
            global_inherited=relative == "." and packaging == "pom",
        ),
        java_target=_java_target(project, props),
    )


def _reactor_modules(root_pom: Path) -> list[MavenModule]:
    """递归解析 reactor module，并拒绝路径逃逸和重复 module。"""
    root_dir = root_pom.parent.resolve()
    modules: dict[str, MavenModule] = {}

    def visit(pom: Path, inherited: dict[str, str] | None = None) -> None:
        module = _read_module(pom, root_dir, inherited)
        if module.path in modules:
            return
        modules[module.path] = module
        project = _parse_xml(pom)
        for node in _children(_child(project, "modules"), "module"):
            value = _resolve_property(_text(node), module.properties)
            if not value:
                continue
            candidate = (pom.parent / value / "pom.xml").resolve()
            try:
                candidate.relative_to(root_dir)
            except ValueError as error:
                raise MavenVerifyError("module-path-invalid", f"module 路径逃逸 reactor：{value}") from error
            if not candidate.is_file():
                raise MavenVerifyError("module-pom-missing", f"module POM 不存在：{candidate}")
            visit(candidate, module.properties)

    visit(root_pom.resolve())
    return [modules[key] for key in sorted(modules)]


def _external_parent_fingerprints(
    modules: list[MavenModule],
    local_repository: str | None,
) -> list[dict[str, str]]:
    """解析并递归指纹化本地仓库中的外部父 POM。"""
    if not local_repository:
        return []
    repository = Path(local_repository).expanduser().resolve()
    reactor_root = modules[0].pom.parent if modules else Path.cwd()
    reactor_poms = {module.pom.resolve() for module in modules}
    seen: set[Path] = set()
    result: list[dict[str, str]] = []

    def visit(project_pom: Path, project_id: str) -> None:
        project = _parse_xml(project_pom)
        parent = _child(project, "parent")
        if parent is None:
            return
        relative_node = _child(parent, "relativePath")
        relative_path = "../pom.xml" if relative_node is None else _text(relative_node)
        local_parent = (project_pom.parent / relative_path).resolve() if relative_path else None
        if local_parent is not None and local_parent.is_file():
            if local_parent not in reactor_poms and local_parent not in seen:
                seen.add(local_parent)
                parent_id = f"{project_id}/relative-parent:{relative_path or '<empty>'}"
                result.append(
                    {
                        "id": parent_id,
                        "path": str(local_parent),
                        "sha256": _file_digest(local_parent),
                    }
                )
                visit(local_parent, parent_id)
            return
        group_id = _text(_child(parent, "groupId"))
        artifact_id = _text(_child(parent, "artifactId"))
        version = _text(_child(parent, "version"))
        if not group_id or not artifact_id or not version or "${" in "".join((group_id, artifact_id, version)):
            return
        candidate = (
            repository
            / Path(*group_id.split("."))
            / artifact_id
            / version
            / f"{artifact_id}-{version}.pom"
        ).resolve()
        if candidate in seen or not candidate.is_file():
            return
        seen.add(candidate)
        parent_id = f"repository:{group_id}:{artifact_id}:{version}"
        result.append(
            {
                "id": parent_id,
                "path": str(candidate),
                "sha256": _file_digest(candidate),
            }
        )
        visit(candidate, parent_id)

    for module in modules:
        visit(module.pom, f"reactor:{_path_from_root(reactor_root, module.pom)}")
    return sorted(result, key=lambda item: item["id"])


def _maven_model_inputs(
    maven_root: Path,
    toolchain: dict[str, Any] | None = None,
) -> list[dict[str, str]]:
    """指纹化会影响 effective model 的用户和项目 Maven 配置。"""
    config_home_value = (toolchain or {}).get("maven", {}).get("configHome")
    config_home = (
        Path(config_home_value)
        if isinstance(config_home_value, str) and config_home_value
        else Path(os.environ.get("MAVEN_CONFIG", str(Path.home() / ".m2"))).expanduser()
    )
    candidates = [
        ("user-settings", config_home / "settings.xml"),
        ("project-maven-config", maven_root / ".mvn/maven.config"),
        ("project-jvm-config", maven_root / ".mvn/jvm.config"),
        ("project-extensions", maven_root / ".mvn/extensions.xml"),
        ("project-wrapper-properties", maven_root / ".mvn/wrapper/maven-wrapper.properties"),
    ]
    return [
        {"id": input_id, "path": str(path.resolve()), "sha256": _file_digest(path)}
        for input_id, path in candidates
        if path.exists() or path.is_symlink()
    ]


def _stable_model_entries(entries: Iterable[dict[str, str]]) -> list[dict[str, str]]:
    """从模型输入中移除仅用于本机诊断的绝对路径。"""
    return [
        {"id": entry["id"], "sha256": entry["sha256"]}
        for entry in sorted(entries, key=lambda item: item["id"])
    ]


def _pom_fingerprint(
    modules: list[MavenModule],
    effective_pom: Path,
    local_repository: str | None,
    toolchain: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """计算 reactor POM 与 effective model 的内容指纹。"""
    root_dir = modules[0].pom.parent if modules else effective_pom.parent
    files = [
        {"path": _path_from_root(root_dir, module.pom), "sha256": _file_digest(module.pom)}
        for module in modules
    ]
    evidence = {
        "files": files,
        "externalParents": _external_parent_fingerprints(modules, local_repository),
        "modelInputs": _maven_model_inputs(root_dir, toolchain),
        "effectivePomSha256": _file_digest(effective_pom),
    }
    evidence["fingerprint"] = _semantic_fingerprint(
        {
            "files": evidence["files"],
            "externalParents": _stable_model_entries(evidence["externalParents"]),
            "modelInputs": _stable_model_entries(evidence["modelInputs"]),
            "effectivePomSha256": evidence["effectivePomSha256"],
        }
    )
    return evidence


def _raw_pom_fingerprint(
    modules: list[MavenModule],
    local_repository: str | None = None,
    toolchain: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """计算无需执行 Maven 即可复核的 reactor POM 指纹。"""
    root_dir = modules[0].pom.parent if modules else Path.cwd()
    files = [
        {"path": _path_from_root(root_dir, module.pom), "sha256": _file_digest(module.pom)}
        for module in modules
    ]
    external_parents = _external_parent_fingerprints(modules, local_repository)
    payload = {
        "files": files,
        "externalParents": external_parents,
        "modelInputs": _maven_model_inputs(root_dir, toolchain),
    }
    payload["fingerprint"] = _semantic_fingerprint(
        {
            "files": payload["files"],
            "externalParents": _stable_model_entries(payload["externalParents"]),
            "modelInputs": _stable_model_entries(payload["modelInputs"]),
        }
    )
    return payload


def _effective_pom(
    maven_root: Path,
    command: MavenCommand,
    supplied: Path | None,
    local_repository_build_path: str | None = None,
    offline: str = "auto",
) -> tuple[Path, tempfile.TemporaryDirectory[str] | None, list[str]]:
    """读取冻结 effective POM，或通过 Maven 生成临时文件。"""
    if supplied is not None:
        resolved = supplied.resolve()
        if not resolved.is_file():
            raise MavenVerifyError("effective-pom-missing", f"effective POM 不存在：{resolved}")
        return resolved, None, []
    temporary = tempfile.TemporaryDirectory(prefix=".trellis-maven-effective-", dir=maven_root)
    output = Path(temporary.name) / "effective-pom.xml"
    output_build_path = (
        _host_to_windows_path(output) if command.build_side == "windows" else str(output)
    )
    argv = [
        command.executable,
        *_offline_args(offline),
        *([f"-Dmaven.repo.local={local_repository_build_path}"] if local_repository_build_path else []),
        "-q",
        "help:effective-pom",
        f"-Doutput={output_build_path}",
    ]
    result, host_argv = _run_maven(command, argv[1:], cwd=maven_root)
    if result.returncode != 0 or not output.is_file():
        temporary.cleanup()
        raise MavenVerifyError(
            "effective-pom-failed",
            "无法生成 effective POM，不能确认外部父 POM生命周期绑定",
            argv=argv,
            hostArgv=host_argv,
            stderr=_decode_command_output(result.stderr, command.build_side).strip(),
        )
    return output, temporary, argv


def _effective_bindings(path: Path, modules: list[MavenModule]) -> list[dict[str, Any]]:
    """提取 effective model 中的插件 execution 绑定及其 reactor 作用域。"""
    root = _parse_xml(path)
    projects = [root] if _local_name(root.tag) == "project" else _children(root, "project")
    by_artifact: dict[str, list[MavenModule]] = {}
    for module in modules:
        by_artifact.setdefault(module.artifact_id, []).append(module)
    bindings: list[dict[str, Any]] = []
    for index, project in enumerate(projects):
        artifact_id = _text(_child(project, "artifactId"))
        matches = by_artifact.get(artifact_id or "", [])
        module = matches[0] if len(matches) == 1 else None
        packaging = module.packaging if module is not None else (_text(_child(project, "packaging"), "jar") or "jar")
        source = f"effective-pom:{module.path if module is not None else index}"
        bindings.extend(
            _plugin_bindings(
                project,
                source,
                module_path=module.path if module is not None else None,
                global_inherited=module is None or (module.path == "." and packaging == "pom"),
            )
        )
    return bindings


def _binding_artifact(binding: dict[str, Any]) -> str | None:
    """把已知昂贵 plugin goal 映射为附属制品分类。"""
    plugin = binding.get("plugin", "")
    goal = binding.get("goal", "")
    if plugin.endswith(":maven-source-plugin") and goal in {
        "jar",
        "jar-no-fork",
        "test-jar",
        "test-jar-no-fork",
    }:
        return "sources"
    if plugin.endswith(":maven-dependency-plugin") and goal == "copy-dependencies":
        return "copy-dependencies"
    if plugin.endswith(":spring-boot-maven-plugin") and goal == "repackage":
        return "repackage"
    if plugin.endswith(":maven-shade-plugin") and goal == "shade":
        return "shade"
    if plugin.endswith(":maven-assembly-plugin"):
        return "assembly"
    if plugin.endswith(":maven-javadoc-plugin"):
        return "javadoc"
    if "frontend-maven-plugin" in plugin:
        return "frontend"
    return None


def _binding_key(
    binding: dict[str, Any],
) -> tuple[str, str, str, str | None]:
    """返回不受 raw/effective 来源重复影响的绑定执行语义 key。"""
    return (
        str(binding.get("plugin", "")),
        str(binding.get("goal", "")),
        str(binding.get("executionId", "")),
        binding.get("module"),
    )


def _all_bindings(
    modules: list[MavenModule],
    effective_pom: Path,
    execution_modules: set[str],
) -> list[dict[str, Any]]:
    """合并并按实际 reactor 执行范围过滤插件绑定。"""
    values: dict[tuple[str, str, str, str | None], dict[str, Any]] = {}
    candidates = [
        *_effective_bindings(effective_pom, modules),
        *(item for module in modules for item in module.bindings),
    ]
    for binding in candidates:
        if binding.get("module") is not None and binding.get("module") not in execution_modules:
            continue
        enriched = dict(binding)
        artifact = _binding_artifact(enriched)
        if artifact:
            enriched["artifact"] = artifact
            enriched["expensive"] = True
        # effective model 排在前面，能提供继承展开后的版本和阶段，重复时应保留它。
        values.setdefault(_binding_key(enriched), enriched)
    return sorted(
        values.values(),
        key=lambda item: (
            LIFECYCLE_RANK.get(item.get("phase") or "", 10_000),
            item.get("plugin") or "",
            item.get("version") or "",
            item.get("goal") or "",
            item.get("source") or "",
        ),
    )


def _phase_reached(binding_phase: str | None, goal: str) -> bool:
    """判断目标 lifecycle 是否会经过绑定阶段。"""
    if not binding_phase:
        return False
    return LIFECYCLE_RANK.get(binding_phase, 10_000) <= LIFECYCLE_RANK[goal]


def _source_skip_supported(bindings: Iterable[dict[str, Any]]) -> bool:
    """只有全部 sources 绑定版本都已确认支持 skipSource 时才启用参数。"""
    versions = [
        _maven_version_tuple(binding.get("version"))
        for binding in bindings
        if binding.get("artifact") == "sources"
    ]
    return bool(versions) and all(
        version is not None and version >= SOURCE_SKIP_MIN_VERSION for version in versions
    )


def _compiler_source_stale_supported(bindings: Iterable[dict[str, Any]]) -> bool:
    """确认全部主源码 compiler execution 支持 source-stale 参数。"""
    versions = [
        _maven_version_tuple(binding.get("version"))
        for binding in bindings
        if binding.get("plugin") == "org.apache.maven.plugins:maven-compiler-plugin"
        and binding.get("goal") == "compile"
    ]
    return bool(versions) and all(
        version is not None and version >= COMPILER_SOURCE_STALE_MIN_VERSION
        for version in versions
    )


def _normalize_threads(value: str | None) -> str | None:
    """校验并规范化 Maven `-T` 的固定线程数或 CPU 倍数。"""
    if value is None:
        return None
    normalized = value.strip().upper()
    if not THREADS_PATTERN.fullmatch(normalized):
        raise MavenVerifyError(
            "threads-invalid",
            "--threads 只接受正整数或正数 CPU 倍数，例如 4、1C、1.5C",
            value=value,
        )
    return normalized


def _resolve_modules(selectors: Iterable[str], modules: list[MavenModule]) -> list[str]:
    """把相对路径、artifactId 或 groupId:artifactId selector 解析为 module 路径。"""
    resolved: list[str] = []
    for selector in selectors:
        normalized = selector.strip().rstrip("/") or "."
        matches = [
            module.path
            for module in modules
            if normalized in {module.path, module.artifact_id, module.coordinate, f":{module.artifact_id}"}
        ]
        if not matches:
            raise MavenVerifyError("module-not-found", f"找不到 Maven module：{selector}")
        if len(matches) > 1:
            raise MavenVerifyError("module-ambiguous", f"Maven module selector 不唯一：{selector}", matches=matches)
        if matches[0] not in resolved:
            resolved.append(matches[0])
    return sorted(resolved)


def _changed_modules(
    changed_paths: list[str],
    repo_root: Path,
    maven_root: Path,
    modules: list[MavenModule],
) -> tuple[list[str], list[str]]:
    """把 Git 变更映射到 module，并返回会影响整个 reactor 的输入。"""
    root_relative = _git_pathspec(repo_root, maven_root)
    root_prefix = "" if root_relative == "." else f"{root_relative}/"
    root_pom = f"{root_prefix}pom.xml"
    reactor_wide_paths = {
        root_pom,
        f"{root_prefix}.mvn/maven.config",
        f"{root_prefix}.mvn/jvm.config",
        f"{root_prefix}.mvn/extensions.xml",
        f"{root_prefix}.mvn/wrapper/maven-wrapper.properties",
    }
    reactor_wide_changes = sorted(reactor_wide_paths.intersection(changed_paths))
    if reactor_wide_changes:
        return [module.path for module in modules], reactor_wide_changes
    selected: set[str] = set()
    module_paths = sorted(modules, key=lambda item: len(item.path), reverse=True)
    for changed in changed_paths:
        if root_prefix and not changed.startswith(root_prefix):
            continue
        relative = changed[len(root_prefix):] if root_prefix else changed
        if relative.startswith((".trellis/", ".agents/", ".claude/")):
            continue
        relative_path = Path(relative)
        match = next(
            (
                module.path
                for module in module_paths
                if (
                    module.path == "."
                    and module.packaging != "pom"
                )
                or (
                    module.path != "."
                    and (
                        relative_path == Path(module.path)
                        or Path(module.path) in relative_path.parents
                    )
                )
            ),
            None,
        )
        if match is not None:
            selected.add(match)
    return sorted(selected), []


def _reverse_dependencies(dependencies: dict[str, list[str]]) -> dict[str, list[str]]:
    """构造解析成功的本地 module 反向依赖图。"""
    reverse: dict[str, set[str]] = {module: set() for module in dependencies}
    for consumer, upstreams in dependencies.items():
        for upstream in upstreams:
            reverse[upstream].add(consumer)
    return {key: sorted(values) for key, values in sorted(reverse.items())}


def _coordinate_resolved(group_id: str | None, artifact_id: str | None) -> bool:
    """判断 Maven 坐标是否完整且不含未展开属性。"""
    return bool(group_id and artifact_id and "${" not in group_id and "${" not in artifact_id)


def _local_dependencies(
    modules: list[MavenModule],
) -> tuple[dict[str, list[str]], list[dict[str, Any]]]:
    """只用完整坐标解析本地依赖，并记录无法可靠判断的坐标。"""
    by_coordinate: dict[tuple[str, str], str] = {
        (module.group_id, module.artifact_id): module.path
        for module in modules
        if _coordinate_resolved(module.group_id, module.artifact_id)
    }
    dependencies: dict[str, set[str]] = {module.path: set() for module in modules}
    unresolved: list[dict[str, Any]] = [
        {
            "module": module.path,
            "groupId": module.group_id,
            "artifactId": module.artifact_id,
        }
        for module in modules
        if not _coordinate_resolved(module.group_id, module.artifact_id)
    ]
    for consumer in modules:
        for group_id, artifact_id in consumer.dependencies:
            if not _coordinate_resolved(group_id, artifact_id):
                unresolved.append(
                    {
                        "consumer": consumer.path,
                        "groupId": group_id,
                        "artifactId": artifact_id,
                    }
                )
                continue
            dependency = by_coordinate.get((group_id, artifact_id))
            if dependency is not None:
                dependencies[consumer.path].add(dependency)
    return (
        {key: sorted(values) for key, values in sorted(dependencies.items())},
        sorted(
            unresolved,
            key=lambda item: (
                item.get("consumer") or item.get("module") or "",
                item.get("groupId") or "",
                item.get("artifactId") or "",
            ),
        ),
    )


def _transitive_upstreams(selected: list[str], dependencies: dict[str, list[str]]) -> list[str]:
    """返回 final `-am` 会带入的 reactor 内传递上游。"""
    seen: set[str] = set()
    pending = list(selected)
    while pending:
        current = pending.pop()
        for upstream in dependencies.get(current, []):
            if upstream in seen or upstream in selected:
                continue
            seen.add(upstream)
            pending.append(upstream)
    return sorted(seen)


def _transitive_consumers(changed: list[str], reverse: dict[str, list[str]]) -> list[str]:
    """返回本地依赖图中变更 module 的传递消费者建议。"""
    seen: set[str] = set()
    pending = list(changed)
    while pending:
        current = pending.pop()
        for consumer in reverse.get(current, []):
            if consumer in seen or consumer in changed:
                continue
            seen.add(consumer)
            pending.append(consumer)
    return sorted(seen)


def _tool_version(argv: list[str], cwd: Path) -> dict[str, Any]:
    """探测工具版本并返回首行与完整输出摘要。"""
    result = _run(argv, cwd=cwd, text=True, timeout=20)
    combined = "\n".join(value for value in (result.stdout.strip(), result.stderr.strip()) if value)
    if result.returncode != 0 or not combined:
        raise MavenVerifyError("toolchain-unreadable", f"无法读取工具版本：{argv[0]}")
    return {"version": combined.splitlines()[0], "details": combined}


def _windows_where(executable: str, cwd: Path) -> str | None:
    """从 Windows PATH 查询可执行文件，不回退到 WSL PATH。"""
    result = _run(["where.exe", executable], cwd=cwd)
    output = _decode_command_output(result.stdout, "windows")
    if result.returncode != 0:
        return None
    return next((line.strip() for line in output.splitlines() if line.strip()), None)


def _posix_maven_executable(value: str, cwd: Path) -> str:
    """解析 POSIX 构建侧 Maven，并拒绝 Windows 路径。"""
    if _is_windows_path(value) or value.lower().endswith(WINDOWS_MAVEN_SUFFIXES):
        raise MavenVerifyError(
            "maven-toolchain-side-mismatch",
            "POSIX 项目不能使用 Windows Maven",
            executable=value,
            buildSide="posix",
        )
    candidate = Path(value).expanduser()
    if candidate.is_absolute() or candidate.parent != Path("."):
        resolved = candidate if candidate.is_absolute() else cwd / candidate
        resolved = resolved.resolve()
        if not resolved.is_file() or not os.access(resolved, os.X_OK):
            raise MavenVerifyError("command-unavailable", f"Maven 不可执行：{resolved}")
        return str(resolved)
    found = shutil.which(value)
    if not found:
        raise MavenVerifyError("command-unavailable", f"当前 POSIX PATH 中没有 Maven：{value}")
    return str(Path(found).resolve())


def _windows_maven_executable(value: str, cwd: Path) -> str:
    """解析 Windows 构建侧 Maven，并拒绝 WSL ext4 可执行文件。"""
    if _is_windows_path(value):
        executable = ntpath.normpath(value)
    else:
        candidate = Path(value).expanduser()
        if candidate.is_absolute() or candidate.parent != Path("."):
            resolved = candidate if candidate.is_absolute() else cwd / candidate
            resolved = resolved.resolve()
            filesystem = _filesystem_info(str(resolved))
            if os.name != "nt" and not _is_wsl_windows_filesystem(filesystem):
                raise MavenVerifyError(
                    "maven-toolchain-side-mismatch",
                    "Windows 项目不能使用 WSL/Linux Maven",
                    executable=str(resolved),
                    buildSide="windows",
                )
            executable = _host_to_windows_path(resolved)
        else:
            executable = _windows_where(value, cwd) or ""
            if not executable and value.lower() == "mvn":
                executable = _windows_where("mvn.cmd", cwd) or ""
            if not executable:
                raise MavenVerifyError("command-unavailable", f"当前 Windows PATH 中没有 Maven：{value}")
    if not executable.lower().endswith(WINDOWS_MAVEN_SUFFIXES):
        raise MavenVerifyError(
            "maven-toolchain-side-mismatch",
            "Windows 构建侧 Maven 必须是 .cmd、.bat 或 .exe",
            executable=executable,
        )
    if os.name != "nt":
        host_executable = _windows_to_host_path(executable, cwd)
        if not host_executable.is_file():
            raise MavenVerifyError("command-unavailable", f"Windows Maven 不存在：{executable}")
    return executable


def _resolve_maven_command(requested: str | None, cwd: Path) -> MavenCommand:
    """按项目构建侧选择现有 Maven，不跨操作系统回退。"""
    build_side, filesystem = _project_build_side(cwd)
    if build_side == "windows":
        wrapper = cwd / "mvnw.cmd"
        if requested is None and wrapper.is_file():
            executable = _host_to_windows_path(wrapper)
            source = "project-wrapper"
        else:
            executable = _windows_maven_executable(requested or "mvn.cmd", cwd)
            source = "explicit" if requested is not None else "path"
        return MavenCommand(build_side, executable, source, "windows-cmd", filesystem)
    wrapper = cwd / "mvnw"
    if requested is None and wrapper.is_file() and os.access(wrapper, os.X_OK):
        executable = str(wrapper.resolve())
        source = "project-wrapper"
    else:
        executable = _posix_maven_executable(requested or "mvn", cwd)
        source = "explicit" if requested is not None else "path"
    return MavenCommand(build_side, executable, source, "direct", filesystem)


def _maven_tool_version(command: MavenCommand, cwd: Path) -> dict[str, Any]:
    """使用冻结构建侧探测 Maven 版本与 Maven 实际使用的 Java。"""
    result, host_argv = _run_maven(command, ["-version"], cwd=cwd, timeout=20)
    combined = "\n".join(
        value
        for value in (
            _decode_command_output(result.stdout, command.build_side).strip(),
            _decode_command_output(result.stderr, command.build_side).strip(),
        )
        if value
    )
    if result.returncode != 0 or not combined:
        raise MavenVerifyError(
            "toolchain-unreadable",
            f"无法读取 Maven 版本：{command.executable}",
            argv=host_argv,
        )
    return {"version": combined.splitlines()[0], "details": combined, "hostArgv": host_argv}


def _java_executable(cwd: Path) -> tuple[str, str | None]:
    """返回 Maven 实际优先使用的 Java 可执行文件与 JAVA_HOME。"""
    java_home = os.environ.get("JAVA_HOME")
    if not java_home:
        return "java", None
    home = Path(java_home).expanduser()
    if not home.is_absolute():
        home = (cwd / home).resolve()
    else:
        home = home.resolve()
    executable = home / "bin" / ("java.exe" if os.name == "nt" else "java")
    return str(executable), str(home)


def _maven_java_version(details: str) -> str | None:
    """从 `mvn -version` 输出读取 Maven 实际使用的 Java 版本。"""
    match = re.search(r"^Java version:\s*([^,\r\n]+)", details, flags=re.MULTILINE | re.IGNORECASE)
    return match.group(1).strip() if match else None


def _maven_java_runtime(details: str) -> str | None:
    """从 `mvn -version` 输出读取 Maven 实际 Java runtime。"""
    match = re.search(r"runtime:\s*([^\r\n]+)", details, flags=re.IGNORECASE)
    return match.group(1).strip() if match else None


def _java_major_from_value(value: str | None) -> int | None:
    """从 Java 版本值解析主版本。"""
    if not value:
        return None
    normalized = value.strip().strip('"')
    match = re.match(r"(\d+)(?:\.(\d+))?", normalized)
    if not match:
        return None
    first, second = match.groups()
    return int(second) if first == "1" and second else int(first)


def _java_major(details: str) -> int | None:
    """从 java -version 输出解析主版本。"""
    match = re.search(r'version\s+"([^"]+)"', details)
    if not match:
        return None
    value = match.group(1)
    if value.startswith("1."):
        value = value.split(".", 2)[1]
    else:
        value = value.split(".", 1)[0]
    return int(value) if value.isdigit() else None


def _maven_version_tuple(version: str | None) -> tuple[int, int, int] | None:
    """从 Maven 或插件版本字符串读取二至三段数字版本。"""
    if not version:
        return None
    match = re.search(r"(?:Apache Maven\s+)?(\d+)\.(\d+)(?:\.(\d+))?", version)
    if not match:
        return None
    major, minor, patch = match.groups()
    return int(major), int(minor), int(patch or 0)


def _maven_args_supported(version: str | None) -> bool:
    """判断当前 Maven 是否支持 3.9 引入的 MAVEN_ARGS。"""
    parsed = _maven_version_tuple(version)
    return parsed is not None and parsed >= (3, 9, 0)


def _maven_property_value(tokens: Iterable[str], name: str) -> str | None:
    """读取 Maven `-Dname=value` 属性，并让后出现的设置覆盖前值。"""
    value: str | None = None
    prefix = f"-D{name}="
    for token in tokens:
        if token.startswith(prefix):
            value = token.split("=", 1)[1]
    return value


def _expand_windows_environment(value: str, environment: dict[str, str]) -> str:
    """展开 Windows `%VAR%`、`${user.home}` 与前导 `~`。"""
    expanded = re.sub(
        r"%([^%]+)%",
        lambda match: environment.get(match.group(1).upper(), match.group(0)),
        value,
    )
    user_profile = environment.get("USERPROFILE")
    if user_profile:
        expanded = expanded.replace("${user.home}", user_profile)
        if expanded == "~" or expanded.startswith(("~/", "~\\")):
            expanded = ntpath.join(user_profile, expanded[2:]) if len(expanded) > 1 else user_profile
    return expanded


def _resolve_side_path(
    value: str,
    cwd: Path,
    build_side: str,
    environment: dict[str, str],
) -> tuple[str, str]:
    """把路径解析为构建侧绝对路径与当前宿主可访问路径。"""
    if build_side == "windows":
        expanded = _expand_windows_environment(value, environment)
        if _is_windows_path(expanded):
            build_path = ntpath.normpath(expanded)
            if build_path.lower().startswith(("\\\\wsl$\\", "\\\\wsl.localhost\\")):
                raise MavenVerifyError(
                    "path-side-mismatch",
                    "Windows 构建侧不能使用 WSL Linux 文件系统路径",
                    path=value,
                )
        else:
            host_candidate = Path(expanded).expanduser()
            if host_candidate.is_absolute():
                filesystem = _filesystem_info(str(host_candidate))
                if not _is_wsl_windows_filesystem(filesystem):
                    raise MavenVerifyError(
                        "path-side-mismatch",
                        "Windows 构建侧不能使用 WSL/Linux 本地路径",
                        path=value,
                    )
                build_path = _host_to_windows_path(host_candidate)
            else:
                build_path = ntpath.normpath(ntpath.join(_host_to_windows_path(cwd), expanded))
        host_path = _windows_to_host_path(build_path, cwd)
        return build_path, str(host_path)
    if _is_windows_path(value):
        raise MavenVerifyError(
            "path-side-mismatch",
            "POSIX 构建侧不能使用 Windows 路径",
            path=value,
        )
    repository = Path(value).expanduser()
    host_path = repository.resolve() if repository.is_absolute() else (cwd / repository).resolve()
    return str(host_path), str(host_path)


def _local_repository(
    *,
    maven_version: str | None,
    jvm_config: Iterable[str],
    maven_config: Iterable[str],
    cwd: Path,
    build_side: str,
    environment: dict[str, str],
) -> tuple[str, str]:
    """从同侧环境或用户 settings.xml 读取 Maven 本地仓库。"""
    maven_args = (
        _split_maven_arguments(
            environment.get("MAVEN_ARGS") or "",
            "MAVEN_ARGS",
            build_side,
        )
        if _maven_args_supported(maven_version)
        else []
    )
    tokens = [
        *jvm_config,
        *_split_maven_arguments(
            environment.get("MAVEN_OPTS") or "",
            "MAVEN_OPTS",
            build_side,
        ),
        *maven_config,
        *maven_args,
    ]
    configured = _maven_property_value(tokens, "maven.repo.local")
    if configured:
        return _resolve_side_path(configured, cwd, build_side, environment)
    if build_side == "windows":
        user_profile = environment.get("USERPROFILE")
        if not user_profile:
            raise MavenVerifyError("windows-home-unreadable", "Windows 构建环境缺少 USERPROFILE")
        config_home_value = environment.get("MAVEN_CONFIG") or ntpath.join(user_profile, ".m2")
    else:
        config_home_value = environment.get("MAVEN_CONFIG") or str(Path.home() / ".m2")
    config_build_path, config_host_path = _resolve_side_path(
        config_home_value,
        cwd,
        build_side,
        environment,
    )
    settings = Path(config_host_path) / "settings.xml"
    if settings.is_file():
        try:
            value = _text(_child(_parse_xml(settings), "localRepository"))
        except MavenVerifyError:
            value = None
        if value:
            return _resolve_side_path(value, cwd, build_side, environment)
    build_repository = (
        ntpath.join(config_build_path, "repository")
        if build_side == "windows"
        else str(Path(config_build_path) / "repository")
    )
    host_repository = str(Path(config_host_path) / "repository")
    return build_repository, host_repository


def _resolve_local_repository(
    value: str | None,
    cwd: Path,
    build_side: str,
    environment: dict[str, str],
) -> tuple[str, str] | None:
    """把显式本地仓库解析为同侧构建路径与宿主路径。"""
    if value is None:
        return None
    return _resolve_side_path(value, cwd, build_side, environment)


def _decode_mount_field(value: str) -> str:
    """解码 Linux mountinfo 中的八进制转义字段。"""
    return re.sub(r"\\([0-7]{3})", lambda match: chr(int(match.group(1), 8)), value)


def _filesystem_info(path: str | None) -> dict[str, Any] | None:
    """返回本地仓库所在文件系统，并标记已知高延迟小文件挂载。"""
    if not path:
        return None
    candidate = Path(path).expanduser().resolve()
    probe = candidate
    while not probe.exists() and probe.parent != probe:
        probe = probe.parent
    mountinfo = Path("/proc/self/mountinfo")
    if not mountinfo.is_file():
        return None
    matches: list[tuple[int, dict[str, Any]]] = []
    try:
        lines = mountinfo.read_text(encoding="utf-8").splitlines()
    except OSError:
        return None
    risky_types = {"9p", "cifs", "drvfs", "fuseblk", "nfs", "nfs4", "smbfs"}
    for line in lines:
        fields = line.split()
        if "-" not in fields:
            continue
        separator = fields.index("-")
        if separator < 5 or len(fields) <= separator + 2:
            continue
        mount_point = Path(_decode_mount_field(fields[4])).resolve()
        try:
            probe.relative_to(mount_point)
        except ValueError:
            continue
        filesystem_type = fields[separator + 1].lower()
        matches.append(
            (
                len(mount_point.parts),
                {
                    "type": filesystem_type,
                    "mountPoint": str(mount_point),
                    "source": _decode_mount_field(fields[separator + 2]),
                    "ioRisk": filesystem_type in risky_types,
                },
            )
        )
    return max(matches, key=lambda item: item[0])[1] if matches else None


def _repository_filesystem_info(path: str | None, build_side: str) -> dict[str, Any] | None:
    """按 Maven 实际访问侧解释本地仓库文件系统风险。"""
    filesystem = _filesystem_info(path)
    if not filesystem or build_side != "windows":
        return filesystem
    if filesystem.get("type") not in {"9p", "drvfs"}:
        return filesystem
    return {
        **filesystem,
        "hostViewType": filesystem.get("type"),
        "type": "windows-native",
        "accessSide": "windows",
        "ioRisk": False,
    }


def _toolchain(
    command: MavenCommand,
    cwd: Path,
    local_repository_override: str | None = None,
    *,
    frozen_toolchain: dict[str, Any] | None = None,
    probe_maven: bool = True,
) -> dict[str, Any]:
    """探测 Java、Maven 和可确认本地仓库。"""
    current_side, current_filesystem = _project_build_side(cwd)
    if current_side != command.build_side:
        raise MavenVerifyError(
            "maven-toolchain-side-mismatch",
            "冻结 Maven 构建侧与当前项目文件系统不一致",
            expected=command.build_side,
            actual=current_side,
        )
    if command.project_filesystem is None and current_filesystem is not None:
        command = MavenCommand(
            command.build_side,
            command.executable,
            command.source,
            command.runner,
            current_filesystem,
        )
    environment = _build_environment(command.build_side, cwd)
    if probe_maven:
        maven = _maven_tool_version(command, cwd)
    else:
        frozen_maven = (frozen_toolchain or {}).get("maven", {})
        if not isinstance(frozen_maven.get("version"), str):
            raise MavenVerifyError(
                "toolchain-unreadable",
                "只读检查缺少可复用的冻结 Maven 版本证据",
            )
        maven = {
            "version": frozen_maven["version"],
            "details": "",
            "hostArgv": frozen_maven.get("hostArgv", []),
        }
    maven_config = cwd / ".mvn/maven.config"
    jvm_config = cwd / ".mvn/jvm.config"
    maven_config_arguments = _split_maven_arguments(
        maven_config.read_text(encoding="utf-8") if maven_config.is_file() else "",
        str(maven_config),
        command.build_side,
    )
    jvm_config_arguments = _split_maven_arguments(
        jvm_config.read_text(encoding="utf-8") if jvm_config.is_file() else "",
        str(jvm_config),
        command.build_side,
    )
    maven_args = environment.get("MAVEN_ARGS")
    supports_maven_args = _maven_args_supported(maven["version"])
    if command.build_side == "windows":
        user_profile = environment.get("USERPROFILE")
        if not user_profile:
            raise MavenVerifyError("windows-home-unreadable", "Windows 构建环境缺少 USERPROFILE")
        config_home_value = environment.get("MAVEN_CONFIG") or ntpath.join(user_profile, ".m2")
    else:
        config_home_value = environment.get("MAVEN_CONFIG") or str(Path.home() / ".m2")
    config_home_build, config_home_host = _resolve_side_path(
        config_home_value,
        cwd,
        command.build_side,
        environment,
    )
    explicit_repository = _resolve_local_repository(
        local_repository_override,
        cwd,
        command.build_side,
        environment,
    )
    local_repository_build, local_repository_host = explicit_repository or _local_repository(
        maven_version=maven["version"],
        jvm_config=jvm_config_arguments,
        maven_config=maven_config_arguments,
        cwd=cwd,
        build_side=command.build_side,
        environment=environment,
    )
    if command.build_side == "windows" and probe_maven:
        java_version = _maven_java_version(maven["details"])
        java_runtime = _maven_java_runtime(maven["details"])
        java_home = environment.get("JAVA_HOME")
        java_executable = ntpath.join(java_home, "bin", "java.exe") if java_home else None
        java = {
            "version": f'java version "{java_version}"' if java_version else None,
            "major": _java_major_from_value(java_version),
            "home": java_home,
            "runtime": java_runtime,
            "executable": java_executable,
        }
    elif command.build_side == "windows":
        frozen_java = (frozen_toolchain or {}).get("java", {})
        java_home = environment.get("JAVA_HOME")
        java = {
            **frozen_java,
            "home": java_home,
            "executable": ntpath.join(java_home, "bin", "java.exe") if java_home else None,
        }
    elif probe_maven:
        java_executable, java_home = _java_executable(cwd)
        java_probe = _tool_version([java_executable, "-version"], cwd)
        java = {
            "version": java_probe["version"],
            "major": _java_major(java_probe["details"]),
            "home": java_home,
            "runtime": None,
            "executable": java_executable,
        }
    else:
        frozen_java = (frozen_toolchain or {}).get("java", {})
        java_executable, java_home = _java_executable(cwd)
        java = {
            **frozen_java,
            "home": java_home,
            "executable": java_executable,
        }
    java["executableSha256"] = _side_executable_digest(
        java.get("executable"),
        cwd,
        command.build_side,
    )
    return {
        "java": java,
        "maven": {
            "version": maven["version"],
            **command.to_json(),
            "hostArgv": maven["hostArgv"],
            "executableSha256": _side_executable_digest(
                command.executable,
                cwd,
                command.build_side,
            ),
            "localRepository": local_repository_host,
            "localRepositoryBuildPath": local_repository_build,
            "configHome": config_home_host,
            "configHomeBuildPath": config_home_build,
            "settingsPath": str(Path(config_home_host) / "settings.xml"),
            "localRepositoryOverride": explicit_repository[1] if explicit_repository else None,
            "localRepositoryOverrideBuildPath": explicit_repository[0] if explicit_repository else None,
            "localRepositoryFilesystem": _repository_filesystem_info(
                local_repository_host,
                command.build_side,
            ),
            "arguments": {
                "MAVEN_ARGS": maven_args if supports_maven_args else None,
                "MAVEN_OPTS": environment.get("MAVEN_OPTS"),
                "jvmConfig": jvm_config_arguments,
                "mavenConfig": maven_config_arguments,
            },
            "ignoredArguments": (
                {"MAVEN_ARGS": maven_args, "reason": "requires-maven-3.9"}
                if maven_args and not supports_maven_args
                else {}
            ),
        },
    }


def _split_windows_arguments(value: str, source: str) -> list[str]:
    """按 Windows 命令行引用规则拆分参数，并保留路径反斜杠。"""
    arguments: list[str] = []
    index = 0
    length = len(value)
    while index < length:
        while index < length and value[index].isspace():
            index += 1
        if index >= length:
            break
        argument: list[str] = []
        in_quotes = False
        started = False
        while index < length and (in_quotes or not value[index].isspace()):
            started = True
            if value[index] == "\\":
                start = index
                while index < length and value[index] == "\\":
                    index += 1
                backslashes = index - start
                if index < length and value[index] == '"':
                    argument.extend("\\" * (backslashes // 2))
                    if backslashes % 2:
                        argument.append('"')
                        index += 1
                    else:
                        in_quotes = not in_quotes
                        index += 1
                else:
                    argument.extend("\\" * backslashes)
                continue
            if value[index] == '"':
                in_quotes = not in_quotes
                index += 1
                continue
            argument.append(value[index])
            index += 1
        if in_quotes:
            raise MavenVerifyError(
                "maven-arguments-invalid",
                f"无法解析 Maven 参数：{source}",
                source=source,
            )
        if started:
            arguments.append("".join(argument))
    return arguments


def _split_maven_arguments(value: str, source: str, build_side: str = "posix") -> list[str]:
    """按 Maven 构建侧的引用规则解析参数，但不执行任何字符串。"""
    if build_side == "windows":
        return _split_windows_arguments(value, source)
    try:
        return shlex.split(value, comments=True, posix=True)
    except ValueError as error:
        raise MavenVerifyError(
            "maven-arguments-invalid",
            f"无法解析 Maven 参数：{source}",
            source=source,
        ) from error


def _effective_maven_argument_tokens(plan_argv: list[str], toolchain: dict[str, Any]) -> list[str]:
    """按低到高优先级汇总会影响 Maven 行为的参数 token。"""
    maven = toolchain.get("maven", {})
    arguments = maven.get("arguments", {})
    build_side = str(maven.get("buildSide") or "posix")
    tokens = [
        *arguments.get("jvmConfig", []),
        *_split_maven_arguments(arguments.get("MAVEN_OPTS") or "", "MAVEN_OPTS", build_side),
        *arguments.get("mavenConfig", []),
        *_split_maven_arguments(arguments.get("MAVEN_ARGS") or "", "MAVEN_ARGS", build_side),
        *plan_argv[1:],
    ]
    return [str(token) for token in tokens]


def _maven_boolean_property(tokens: Iterable[str], name: str) -> bool:
    """读取 Maven `-D` 布尔属性，并让后出现的设置覆盖前值。"""
    value = False
    prefix = f"-D{name}"
    for token in tokens:
        if token == prefix:
            value = True
        elif token.startswith(f"{prefix}="):
            value = token.split("=", 1)[1].strip().lower() in {"1", "true", "yes", "on"}
    return value


def _test_skip_state(plan_argv: list[str], toolchain: dict[str, Any]) -> dict[str, bool]:
    """识别 Maven 配置、环境和计划 argv 中生效的测试跳过属性。"""
    tokens = _effective_maven_argument_tokens(plan_argv, toolchain)
    test_execution_skipped = _maven_boolean_property(tokens, "skipTests")
    test_compilation_skipped = _maven_boolean_property(tokens, "maven.test.skip")
    return {
        "testsSkipped": test_execution_skipped or test_compilation_skipped,
        "testCompilationSkipped": test_compilation_skipped,
    }


def _find_maven_root(repo_root: Path, requested: str | None) -> Path | None:
    """定位 Maven 根；多个候选时要求显式选择。"""
    if requested:
        candidate = Path(requested).expanduser()
        candidate = candidate if candidate.is_absolute() else repo_root / candidate
        candidate = candidate.resolve()
        if not (candidate / "pom.xml").is_file():
            raise MavenVerifyError("maven-root-invalid", f"目录没有 pom.xml：{candidate}")
        _git_pathspec(repo_root, candidate)
        return candidate
    current = Path.cwd().resolve()
    while True:
        if (current / "pom.xml").is_file():
            _git_pathspec(repo_root, current)
            return current
        if current == repo_root or current.parent == current:
            break
        current = current.parent
    if (repo_root / "pom.xml").is_file():
        return repo_root
    candidates = sorted(
        path.parent for path in repo_root.glob("*/pom.xml") if ".trellis" not in path.parts
    )
    if not candidates:
        return None
    if len(candidates) > 1:
        raise MavenVerifyError(
            "maven-root-ambiguous",
            "发现多个 Maven 根，请使用 --maven-root 明确选择",
            candidates=[_path_from_root(repo_root, item) for item in candidates],
        )
    return candidates[0].resolve()


def _offline_args(value: str) -> list[str]:
    """把 offline 选择转换为 Maven argv。"""
    return ["-o"] if value == "yes" else []


def _dedupe(values: Iterable[str]) -> list[str]:
    """保持顺序去重非空字符串。"""
    result: list[str] = []
    for value in values:
        if value and value not in result:
            result.append(value)
    return result


def _blocked_payload(error: MavenVerifyError, command: str) -> dict[str, Any]:
    """把结构化异常转换为稳定 CLI 结果。"""
    return {
        "schemaVersion": SCHEMA_VERSION,
        "kind": PLAN_KIND if command == "plan" else KIND,
        "status": "blocked",
        "reasons": [
            {
                "code": error.code,
                "message": str(error),
                **({"details": error.details} if error.details else {}),
            }
        ],
    }


def _stable_plan_argv(
    argv: Any,
    local_repository_override: str | None,
    local_repository_override_build_path: str | None = None,
) -> Any:
    """移除 Maven 可执行文件，并规范化仅用于本机定位的仓库绝对路径。"""
    if not isinstance(argv, list):
        return argv
    repository_argument = (
        f"-Dmaven.repo.local={local_repository_override_build_path or local_repository_override}"
        if local_repository_override or local_repository_override_build_path
        else None
    )
    return [
        "-Dmaven.repo.local=<explicit>" if item == repository_argument else item
        for item in argv[1:]
    ]


def _stable_plan_warnings(warnings: Any) -> Any:
    """移除 warning 中仅用于本机诊断的仓库与挂载路径。"""
    if not isinstance(warnings, list):
        return warnings
    result: list[Any] = []
    for warning in warnings:
        if not isinstance(warning, dict):
            result.append(warning)
            continue
        stable = dict(warning)
        stable.pop("localRepository", None)
        filesystem = stable.get("filesystem")
        if isinstance(filesystem, dict):
            stable["filesystem"] = {
                key: filesystem.get(key)
                for key in ("type", "ioRisk")
                if key in filesystem
            }
        result.append(stable)
    return result


def _stable_maven_argument_tokens(value: Any, build_side: str) -> Any:
    """规范化 Maven 参数中的本机仓库路径，同时保留其它执行语义。"""
    if value is None:
        return None
    tokens = (
        value
        if isinstance(value, list)
        else _split_maven_arguments(value, "plan-fingerprint", build_side)
    )
    return [
        "-Dmaven.repo.local=<configured>"
        if token.startswith("-Dmaven.repo.local=")
        else token
        for token in tokens
    ]


def _stable_maven_arguments(arguments: Any, build_side: str) -> Any:
    """返回不含本机仓库绝对路径的 Maven 配置参数。"""
    if not isinstance(arguments, dict):
        return arguments
    return {
        key: _stable_maven_argument_tokens(value, build_side)
        for key, value in arguments.items()
    }


def _plan_fingerprint(plan: dict[str, Any]) -> str:
    """根据计划的执行语义和输入证据重新计算指纹。"""
    local_repository_override = plan.get("localRepositoryOverride")
    local_repository_override_build_path = plan.get("localRepositoryOverrideBuildPath")
    build_side = str(plan.get("toolchain", {}).get("maven", {}).get("buildSide") or "posix")
    return _semantic_fingerprint(
        {
            "schemaVersion": plan.get("schemaVersion"),
            "kind": plan.get("kind"),
            "status": plan.get("status"),
            "mode": plan.get("mode"),
            "mavenRoot": plan.get("mavenRoot"),
            "changedModules": plan.get("changedModules"),
            "selectedModules": plan.get("selectedModules"),
            "executionModules": plan.get("executionModules"),
            "consumerModules": plan.get("consumerModules"),
            "inferredConsumers": plan.get("inferredConsumers"),
            "rootPomChanged": plan.get("rootPomChanged"),
            "reactorWideChanges": plan.get("reactorWideChanges"),
            "goal": plan.get("goal"),
            "compileStrategy": plan.get("compileStrategy"),
            "threads": plan.get("threads"),
            "argv": _stable_plan_argv(
                plan.get("argv"),
                local_repository_override,
                local_repository_override_build_path,
            ),
            "fallbackArgv": _stable_plan_argv(
                plan.get("fallbackArgv"),
                local_repository_override,
                local_repository_override_build_path,
            ),
            "offline": plan.get("offline"),
            "localRepositoryOverride": bool(local_repository_override),
            "tests": plan.get("tests"),
            "artifacts": plan.get("artifacts"),
            "lifecycle": plan.get("lifecycle"),
            "coverage": plan.get("coverage"),
            "repository": plan.get("repository", {}).get("fingerprint"),
            "rawPom": plan.get("rawPom", {}).get("fingerprint"),
            "pom": plan.get("pom", {}).get("fingerprint"),
            "effectivePom": {
                "sha256": plan.get("effectivePom", {}).get("sha256"),
            },
            "toolchain": {
                "javaMajor": plan.get("toolchain", {}).get("java", {}).get("major"),
                "javaExecutableSha256": plan.get("toolchain", {}).get("java", {}).get(
                    "executableSha256"
                ),
                "mavenVersion": plan.get("toolchain", {}).get("maven", {}).get("version"),
                "mavenExecutableSha256": plan.get("toolchain", {}).get("maven", {}).get(
                    "executableSha256"
                ),
                "buildSide": plan.get("toolchain", {}).get("maven", {}).get("buildSide"),
                "runner": plan.get("toolchain", {}).get("maven", {}).get("runner"),
                "mavenArguments": _stable_maven_arguments(
                    plan.get("toolchain", {}).get("maven", {}).get("arguments"),
                    build_side,
                ),
            },
            "javaTargets": plan.get("javaTargets"),
            "confidence": plan.get("confidence"),
            "warnings": _stable_plan_warnings(plan.get("warnings")),
        }
    )


def _plan_integrity_fingerprint(plan: dict[str, Any]) -> str:
    """绑定计划全部字段，包括仅用于本机执行的绝对路径。"""
    return _semantic_fingerprint(
        {
            key: value
            for key, value in plan.items()
            if key not in {"planFingerprint", "planIntegrityFingerprint"}
        }
    )


def _validate_plan_fingerprint(plan: dict[str, Any], source: str) -> None:
    """拒绝语义指纹或本机完整性指纹不一致的计划。"""
    expected_semantics = plan.get("planFingerprint")
    actual_semantics = _plan_fingerprint(plan)
    expected_integrity = plan.get("planIntegrityFingerprint")
    actual_integrity = _plan_integrity_fingerprint(plan)
    if (
        not isinstance(expected_semantics, str)
        or expected_semantics != actual_semantics
        or not isinstance(expected_integrity, str)
        or expected_integrity != actual_integrity
    ):
        raise MavenVerifyError(
            "plan-fingerprint-mismatch",
            f"计划语义指纹不匹配：{source}",
            source=source,
            expected={"semantics": expected_semantics, "integrity": expected_integrity},
            actual={"semantics": actual_semantics, "integrity": actual_integrity},
        )


def _validate_plan_paths(plan: dict[str, Any]) -> tuple[Path, Path]:
    """校验计划中的项目根、Maven 根与 cwd 指向同一工作树位置。"""
    project_root = Path(plan.get("projectRoot", "")).resolve()
    cwd = Path(plan.get("cwd", "")).resolve()
    maven_root = plan.get("mavenRoot")
    if not project_root.is_dir() or not isinstance(maven_root, str) or not maven_root:
        raise MavenVerifyError("plan-invalid", "计划缺少有效 projectRoot 或 mavenRoot")
    expected_cwd = project_root if maven_root == "." else (project_root / maven_root).resolve()
    if cwd != expected_cwd or not cwd.is_dir():
        raise MavenVerifyError(
            "plan-path-mismatch",
            "计划 cwd 与 projectRoot/mavenRoot 不一致",
            projectRoot=str(project_root),
            mavenRoot=maven_root,
            cwd=str(cwd),
        )
    if _git_root(cwd) != project_root:
        raise MavenVerifyError("plan-git-root-mismatch", "计划 projectRoot 不是 cwd 所属 Git 根")
    return project_root, cwd


def _evidence_fingerprint(evidence: dict[str, Any]) -> str:
    """计算 evidence 除自身指纹字段外的完整语义摘要。"""
    return _semantic_fingerprint(
        {key: value for key, value in evidence.items() if key != "evidenceFingerprint"}
    )


def _validate_evidence_fingerprint(evidence: dict[str, Any], source: str) -> None:
    """拒绝内容与冻结指纹不一致的 evidence。"""
    expected = evidence.get("evidenceFingerprint")
    actual = _evidence_fingerprint(evidence)
    if not isinstance(expected, str) or expected != actual:
        raise MavenVerifyError(
            "evidence-fingerprint-mismatch",
            f"evidence 语义指纹不匹配：{source}",
            source=source,
            expected=expected,
            actual=actual,
        )


def create_plan(args: argparse.Namespace) -> dict[str, Any]:
    """生成 Maven quick/final 验证计划。"""
    if args.test and LIFECYCLE_RANK[args.goal] < LIFECYCLE_RANK["test"]:
        raise MavenVerifyError(
            "test-goal-insufficient",
            "声明 --test 时 goal 至少必须是 test",
            goal=args.goal,
        )
    repo_root = _git_root(Path.cwd())
    maven_root = _find_maven_root(repo_root, args.maven_root)
    if maven_root is None:
        return {
            "schemaVersion": SCHEMA_VERSION,
            "kind": PLAN_KIND,
            "status": "not-applicable",
            "reason": "no-pom",
        }
    modules = _reactor_modules(maven_root / "pom.xml")
    workspace = _workspace_evidence(repo_root, maven_root)
    changed, reactor_wide_changes = _changed_modules(
        workspace["changedPaths"], repo_root, maven_root, modules
    )
    root_changed = _git_pathspec(repo_root, maven_root)
    root_changed = f"{'' if root_changed == '.' else f'{root_changed}/'}pom.xml" in reactor_wide_changes
    explicit = _resolve_modules(args.module, modules)
    consumers = _resolve_modules(args.consumer, modules)
    selected = sorted(set(changed) | set(explicit) | set(consumers))
    if not selected:
        return {
            "schemaVersion": SCHEMA_VERSION,
            "kind": PLAN_KIND,
            "status": "not-applicable",
            "reason": "no-maven-changes",
            "mavenRoot": _path_from_root(repo_root, maven_root),
        }

    all_paths = [module.path for module in modules]
    select_all = bool(reactor_wide_changes) or set(selected) == set(all_paths) or "." in selected
    dependencies, unresolved_dependencies = _local_dependencies(modules)
    execution_modules = set(all_paths if select_all else selected)
    if not select_all:
        execution_modules.update(_transitive_upstreams(selected, dependencies))
    threads = _normalize_threads(args.threads)

    command = _resolve_maven_command(args.maven_executable, maven_root)
    build_environment = _build_environment(command.build_side, maven_root)
    explicit_repository_paths = _resolve_local_repository(
        args.local_repository,
        maven_root,
        command.build_side,
        build_environment,
    )
    explicit_repository_build = explicit_repository_paths[0] if explicit_repository_paths else None
    explicit_repository = explicit_repository_paths[1] if explicit_repository_paths else None
    if explicit_repository:
        repository_path = Path(explicit_repository)
        if repository_path.exists() and not repository_path.is_dir():
            raise MavenVerifyError(
                "local-repository-invalid",
                "显式 Maven 本地仓库不是目录",
                path=explicit_repository,
            )
        if args.offline == "yes" and not repository_path.is_dir():
            raise MavenVerifyError(
                "local-repository-missing-offline",
                "离线计划要求显式 Maven 本地仓库已完整准备",
                path=explicit_repository,
            )
    toolchain = _toolchain(command, maven_root, explicit_repository_build)
    local_repository = toolchain["maven"].get("localRepository")
    effective_path: Path
    temporary: tempfile.TemporaryDirectory[str] | None
    supplied_effective = Path(args.effective_pom).resolve() if args.effective_pom else None
    effective_path, temporary, effective_argv = _effective_pom(
        maven_root,
        command,
        supplied_effective,
        explicit_repository_build,
        args.offline,
    )
    try:
        bindings = _all_bindings(modules, effective_path, execution_modules)
        pom = _pom_fingerprint(modules, effective_path, local_repository, toolchain)
        raw_pom = _raw_pom_fingerprint(modules, local_repository, toolchain)
        effective_model = {
            "origin": "supplied" if supplied_effective is not None else "generated",
            "path": str(effective_path) if supplied_effective is not None else None,
            "sha256": _file_digest(effective_path),
        }
    finally:
        if temporary is not None:
            temporary.cleanup()

    unknown_phase_bindings = [
        item for item in bindings if item.get("expensive") and not item.get("phase")
    ]
    active_bindings = [item for item in bindings if _phase_reached(item.get("phase"), args.goal)]
    expensive = [item for item in active_bindings if item.get("expensive")]
    requested_artifacts = set(args.artifact)
    active_artifacts = {item.get("artifact") for item in active_bindings if item.get("artifact")}
    unknown_phase_artifacts = {
        item.get("artifact") for item in unknown_phase_bindings if item.get("artifact")
    }
    uncertain_artifacts = sorted(requested_artifacts.intersection(unknown_phase_artifacts))
    if uncertain_artifacts:
        raise MavenVerifyError(
            "artifact-binding-phase-unknown",
            "附属制品绑定缺少可确认的 lifecycle 阶段",
            artifacts=uncertain_artifacts,
            bindings=unknown_phase_bindings,
        )
    missing_artifacts = sorted(requested_artifacts - active_artifacts)
    if missing_artifacts:
        raise MavenVerifyError(
            "artifact-binding-missing",
            "目标 lifecycle 没有可确认的附属制品绑定",
            goal=args.goal,
            artifacts=missing_artifacts,
        )
    skipped_bindings: list[dict[str, Any]] = []
    skip_args: list[str] = []
    source_skip_supported = _source_skip_supported(expensive)
    for binding in expensive:
        if (
            source_skip_supported
            and binding.get("artifact") == "sources"
            and "sources" not in requested_artifacts
        ):
            # maven-source-plugin 官方参数是唯一首版内置的安全 skip 契约。
            skip_args.append("-Dmaven.source.skip=true")
            skipped_bindings.append(binding)
    skip_args = _dedupe(skip_args)

    compiler_source_stale_supported = _compiler_source_stale_supported(active_bindings)
    requested_compile_strategy = args.compile_strategy
    if requested_compile_strategy == "source-stale" and args.goal != "compile":
        raise MavenVerifyError(
            "source-stale-goal-unsupported",
            "source-stale 只允许用于 compile 验证",
            goal=args.goal,
        )
    if requested_compile_strategy == "source-stale" and not compiler_source_stale_supported:
        raise MavenVerifyError(
            "compiler-source-stale-unsupported",
            "无法确认当前 maven-compiler-plugin 支持 source-stale 参数",
            minimumVersion="3.1",
        )
    if requested_compile_strategy == "auto":
        effective_compile_strategy = (
            "source-stale"
            if args.mode == "quick"
            and args.goal == "compile"
            and compiler_source_stale_supported
            else "conservative"
        )
    else:
        effective_compile_strategy = requested_compile_strategy
    compile_args = (
        ["-Dmaven.compiler.useIncrementalCompilation=false"]
        if effective_compile_strategy == "source-stale"
        else []
    )

    selectors = [value for value in selected if value != "."]
    argv = [
        command.executable,
        *(["-T", threads] if threads else []),
        *_offline_args(args.offline),
        *([f"-Dmaven.repo.local={explicit_repository_build}"] if explicit_repository_build else []),
    ]
    if not select_all and selectors:
        argv.extend(["-pl", ",".join(selectors)])
        argv.append("-am")
    argv.extend(skip_args)
    argv.extend(compile_args)
    if args.test:
        argv.append(f"-Dtest={','.join(args.test)}")
    argv.append(args.goal)
    test_skip_state = _test_skip_state(argv, toolchain)

    fallback_argv = None
    if args.mode == "quick" and effective_compile_strategy == "source-stale":
        fallback_argv = [
            value
            for value in argv
            if value != "-Dmaven.compiler.useIncrementalCompilation=false"
        ]

    reverse = _reverse_dependencies(dependencies)
    inferred_consumers = (
        [] if unresolved_dependencies else _transitive_consumers(changed, reverse)
    )
    java_targets = sorted(
        {
            module.java_target
            for module in modules
            if module.path in execution_modules and module.java_target
        }
    )
    warnings: list[dict[str, Any]] = []
    repository_filesystem = toolchain["maven"].get("localRepositoryFilesystem")
    if repository_filesystem and repository_filesystem.get("ioRisk"):
        warnings.append(
            {
                "code": "local-repository-high-latency-filesystem",
                "message": "Maven 本地仓库位于高延迟小文件文件系统；compile 的依赖 classpath 解析可能显著变慢。",
                "localRepository": local_repository,
                "filesystem": repository_filesystem,
                "suggestion": "在 Linux 原生文件系统准备完整仓库后，通过 --local-repository 显式使用；本工具不会自动复制或修改 settings.xml。",
            }
        )
    if (
        requested_compile_strategy == "auto"
        and args.mode == "quick"
        and args.goal == "compile"
        and not compiler_source_stale_supported
    ):
        warnings.append(
            {
                "code": "compiler-source-stale-unsupported",
                "message": "无法确认 maven-compiler-plugin >= 3.1，quick 已降级为 conservative。",
                "minimumVersion": "3.1",
            }
        )
    if effective_compile_strategy == "source-stale":
        warnings.append(
            {
                "code": "source-stale-local-feedback",
                "message": "source-stale 按源文件与 class 时间戳编译，不能替代公共 API/ABI、常量内联、注解处理器或 POM变化后的 conservative final。",
            }
        )
    if threads:
        warnings.append(
            {
                "code": "parallel-build-explicit",
                "message": "已按显式请求启用 Maven 并行构建；调用方必须确认项目插件线程安全。",
                "threads": threads,
            }
        )
    if inferred_consumers and not consumers:
        warnings.append(
            {
                "code": "consumer-suggestions-not-selected",
                "message": "检测到本地反向依赖消费者；final 计划需由任务材料或调用方确认后显式传入。",
                "modules": inferred_consumers,
            }
        )
    if unresolved_dependencies:
        warnings.append(
            {
                "code": "dependency-coordinate-unresolved",
                "message": "存在未解析的 Maven 依赖坐标；不会按同名 artifactId 猜测上游或消费者。",
                "dependencies": unresolved_dependencies,
            }
        )
    if unknown_phase_bindings:
        warnings.append(
            {
                "code": "binding-phase-unknown",
                "message": "存在缺少显式阶段且不在默认阶段兼容表中的昂贵 goal。",
                "bindings": unknown_phase_bindings,
            }
        )
    if test_skip_state["testsSkipped"]:
        warnings.append(
            {
                "code": "tests-skipped-by-configuration",
                "message": "Maven 配置或环境会跳过测试，当前结果不能作为测试通过证据。",
                **test_skip_state,
            }
        )
    if (
        any(item.get("artifact") == "sources" for item in expensive)
        and "sources" not in requested_artifacts
        and not source_skip_supported
    ):
        warnings.append(
            {
                "code": "sources-skip-unsupported",
                "message": "检测到 sources 绑定，但插件版本未全部落在已确认支持 skipSource 的范围内。",
                "bindings": [
                    item for item in expensive if item.get("artifact") == "sources"
                ],
            }
        )
    if args.goal in {"package", "verify", "install"} and expensive:
        warnings.append(
            {
                "code": "expensive-lifecycle-bindings",
                "message": "目标生命周期会执行附属制品或资源型 goal。",
                "bindings": expensive,
            }
        )
    plan = {
        "schemaVersion": SCHEMA_VERSION,
        "kind": PLAN_KIND,
        "status": "planned",
        "mode": args.mode,
        "projectRoot": str(repo_root),
        "mavenRoot": _path_from_root(repo_root, maven_root),
        "cwd": str(maven_root),
        "changedModules": changed,
        "selectedModules": selected,
        "executionModules": sorted(execution_modules),
        "consumerModules": consumers,
        "inferredConsumers": inferred_consumers,
        "rootPomChanged": root_changed,
        "reactorWideChanges": reactor_wide_changes,
        "goal": args.goal,
        "compileStrategy": {
            "requested": requested_compile_strategy,
            "effective": effective_compile_strategy,
            "supported": compiler_source_stale_supported,
        },
        "threads": threads,
        "argv": argv,
        "fallbackArgv": fallback_argv,
        "offline": args.offline,
        "localRepositoryOverride": explicit_repository,
        "localRepositoryOverrideBuildPath": explicit_repository_build,
        "tests": args.test,
        "artifacts": sorted(requested_artifacts),
        "lifecycle": {
            "bindings": active_bindings,
            "expensiveBindings": expensive,
            "skippedBindings": skipped_bindings,
            "unknownPhaseBindings": unknown_phase_bindings,
        },
        "coverage": {
            "level": args.goal,
            "modules": sorted(execution_modules),
            "consumers": consumers,
            "tests": args.test,
            "artifacts": sorted(requested_artifacts),
            **test_skip_state,
        },
        "repository": workspace,
        "pom": pom,
        "rawPom": raw_pom,
        "effectivePom": effective_model,
        "toolchain": toolchain,
        "javaTargets": java_targets,
        "effectivePomCommand": effective_argv,
        "confidence": "low" if unresolved_dependencies or unknown_phase_bindings else "high",
        "warnings": warnings,
    }
    plan["planFingerprint"] = _plan_fingerprint(plan)
    plan["planIntegrityFingerprint"] = _plan_integrity_fingerprint(plan)
    return plan


def _resolve_runtime_dir(project_root: Path, value: str | None) -> Path:
    """解析 evidence 目录。"""
    path = Path(value) if value else DEFAULT_EVIDENCE_DIR
    return path.resolve() if path.is_absolute() else (project_root / path).resolve()


def _current_preconditions(
    plan: dict[str, Any],
    *,
    audit_only: bool = False,
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any]]:
    """读取计划执行前的 Git、POM 与工具链状态。"""
    repo_root, maven_root = _validate_plan_paths(plan)
    modules = _reactor_modules(maven_root / "pom.xml")
    local_repository = plan["toolchain"]["maven"].get("localRepository")
    local_repository_override = plan.get("localRepositoryOverrideBuildPath")
    command = _maven_command_from_toolchain(plan["toolchain"])
    probe_maven = not (audit_only and _is_project_wrapper_command(command, maven_root))
    return (
        _workspace_evidence(repo_root, maven_root),
        _raw_pom_fingerprint(modules, local_repository, plan["toolchain"]),
        _toolchain(
            command,
            maven_root,
            local_repository_override,
            frozen_toolchain=plan["toolchain"],
            probe_maven=probe_maven,
        ),
    )


def _precondition_reasons(
    plan: dict[str, Any],
    current: tuple[dict[str, Any], dict[str, Any], dict[str, Any]] | None = None,
) -> list[dict[str, Any]]:
    """检查计划生成后、执行前是否发生漂移。"""
    workspace, raw_pom, toolchain = current or _current_preconditions(plan)
    reasons: list[dict[str, Any]] = []
    for code, expected, actual in (
        ("workspace-changed", plan["repository"]["fingerprint"], workspace["fingerprint"]),
        ("pom-changed", plan["rawPom"]["fingerprint"], raw_pom["fingerprint"]),
        ("java-major-changed", plan["toolchain"]["java"].get("major"), toolchain["java"].get("major")),
        (
            "java-executable-changed",
            plan["toolchain"]["java"].get("executableSha256"),
            toolchain["java"].get("executableSha256"),
        ),
        ("maven-version-changed", plan["toolchain"]["maven"].get("version"), toolchain["maven"].get("version")),
        (
            "maven-executable-changed",
            plan["toolchain"]["maven"].get("executableSha256"),
            toolchain["maven"].get("executableSha256"),
        ),
        ("maven-build-side-changed", plan["toolchain"]["maven"].get("buildSide"), toolchain["maven"].get("buildSide")),
        ("maven-runner-changed", plan["toolchain"]["maven"].get("runner"), toolchain["maven"].get("runner")),
        (
            "local-repository-changed",
            plan["toolchain"]["maven"].get("localRepository"),
            toolchain["maven"].get("localRepository"),
        ),
        ("maven-arguments-changed", plan["toolchain"]["maven"].get("arguments"), toolchain["maven"].get("arguments")),
    ):
        if expected != actual:
            reasons.append({"code": code, "expected": expected, "actual": actual})
    reasons.extend(_effective_pom_reasons(plan))
    return reasons


def _execution_drift_reasons(
    before: tuple[dict[str, Any], dict[str, Any], dict[str, Any]],
    after: tuple[dict[str, Any], dict[str, Any], dict[str, Any]],
) -> list[dict[str, Any]]:
    """判断 Maven 执行窗口内输入代码、POM 或工具链是否变化。"""
    before_workspace, before_raw_pom, before_toolchain = before
    after_workspace, after_raw_pom, after_toolchain = after
    reasons: list[dict[str, Any]] = []
    for code, expected, actual in (
        ("workspace-changed-during-run", before_workspace["fingerprint"], after_workspace["fingerprint"]),
        ("pom-changed-during-run", before_raw_pom["fingerprint"], after_raw_pom["fingerprint"]),
        (
            "java-major-changed-during-run",
            before_toolchain["java"].get("major"),
            after_toolchain["java"].get("major"),
        ),
        (
            "java-executable-changed-during-run",
            before_toolchain["java"].get("executableSha256"),
            after_toolchain["java"].get("executableSha256"),
        ),
        (
            "maven-version-changed-during-run",
            before_toolchain["maven"].get("version"),
            after_toolchain["maven"].get("version"),
        ),
        (
            "maven-executable-changed-during-run",
            before_toolchain["maven"].get("executableSha256"),
            after_toolchain["maven"].get("executableSha256"),
        ),
        (
            "maven-build-side-changed-during-run",
            before_toolchain["maven"].get("buildSide"),
            after_toolchain["maven"].get("buildSide"),
        ),
        (
            "maven-runner-changed-during-run",
            before_toolchain["maven"].get("runner"),
            after_toolchain["maven"].get("runner"),
        ),
        (
            "local-repository-changed-during-run",
            before_toolchain["maven"].get("localRepository"),
            after_toolchain["maven"].get("localRepository"),
        ),
        (
            "maven-arguments-changed-during-run",
            before_toolchain["maven"].get("arguments"),
            after_toolchain["maven"].get("arguments"),
        ),
    ):
        if expected != actual:
            reasons.append({"code": code, "expected": expected, "actual": actual})
    return reasons


def _effective_pom_reasons(plan: dict[str, Any]) -> list[dict[str, Any]]:
    """复核调用方提供的 frozen effective POM 内容。"""
    model = plan.get("effectivePom", {})
    if model.get("origin") != "supplied":
        return []
    path_value = model.get("path")
    expected = model.get("sha256")
    actual = _file_digest(Path(path_value)) if isinstance(path_value, str) and path_value else "missing"
    if expected == actual:
        return []
    return [{"code": "effective-pom-changed", "expected": expected, "actual": actual}]


def _parse_test_statistics(log_text: str) -> dict[str, int]:
    """汇总 Surefire/Failsafe 风格测试统计。"""
    summaries_after_results: list[tuple[int, int, int, int]] = []
    pending_results = False
    all_summaries: list[tuple[int, int, int, int]] = []
    for line in log_text.splitlines():
        if line.strip().endswith("Results:"):
            pending_results = True
            continue
        match = TEST_SUMMARY_PATTERN.search(line)
        if match is None:
            continue
        summary = tuple(int(value) for value in match.groups())
        all_summaries.append(summary)
        if pending_results:
            summaries_after_results.append(summary)
            pending_results = False
    selected = summaries_after_results or all_summaries[-1:]
    totals = {"run": 0, "failures": 0, "errors": 0, "skipped": 0}
    for summary in selected:
        for key, value in zip(totals, summary):
            totals[key] += value
    return totals


def run_plan(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    """执行冻结计划并写入成功或失败 evidence。"""
    plan_source = "<stdin>" if args.plan_stdin else str(Path(args.plan_json).resolve())
    plan = (
        _read_json_stdin(PLAN_KIND)
        if args.plan_stdin
        else _read_json(Path(args.plan_json).resolve(), PLAN_KIND)
    )
    if plan.get("status") != "planned":
        raise MavenVerifyError("plan-not-runnable", "计划状态不是 planned")
    _validate_plan_fingerprint(plan, plan_source)
    project_root, cwd = _validate_plan_paths(plan)
    argv = plan.get("argv")
    if not isinstance(argv, list) or not all(
        isinstance(item, str) and item for item in argv
    ):
        raise MavenVerifyError("plan-invalid", "计划缺少可执行 cwd 或 argv")
    command = _maven_command_from_toolchain(plan["toolchain"])
    if argv[0] != command.executable:
        raise MavenVerifyError("plan-toolchain-invalid", "计划 argv 与冻结 Maven executable 不一致")
    host_argv = _maven_process_argv(command, argv[1:])
    execution_inputs = _current_preconditions(plan)
    drift = _precondition_reasons(plan, execution_inputs)
    if drift:
        result = {
            "schemaVersion": SCHEMA_VERSION,
            "kind": KIND,
            "status": "stale",
            "reasons": drift,
            "planFingerprint": plan.get("planFingerprint"),
        }
        return result, 3

    evidence_dir = _resolve_runtime_dir(project_root, args.evidence_dir)
    started_at = _utc_now()
    time_token = re.sub(r"[^0-9]", "", started_at)
    evidence_id = f"{time_token}-{plan['planFingerprint'][:12]}"
    log_path = evidence_dir / f"{evidence_id}.log"
    evidence_path = evidence_dir / f"{evidence_id}.json"
    evidence_dir.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    exit_code = 1
    interrupted = False
    log_parts: list[str] = []
    process: subprocess.Popen[str] | None = None
    with log_path.open("w", encoding="utf-8", newline="\n") as log:
        try:
            process = subprocess.Popen(
                host_argv,
                cwd=cwd,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                bufsize=0,
            )
            assert process.stdout is not None
            for raw_line in iter(process.stdout.readline, b""):
                line = _decode_command_output(raw_line, command.build_side)
                sys.stderr.write(line)
                sys.stderr.flush()
                log.write(line)
                log.flush()
                log_parts.append(line)
            exit_code = process.wait()
        except KeyboardInterrupt:
            interrupted = True
            if process is not None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
            exit_code = 130
        except OSError as error:
            message = f"无法启动 Maven 命令：{error}\n"
            sys.stderr.write(message)
            log.write(message)
            log_parts.append(message)
            exit_code = 127
    finished_at = _utc_now()
    duration_ms = int((time.monotonic() - started) * 1000)
    postconditions = _current_preconditions(plan)
    execution_drift = _execution_drift_reasons(execution_inputs, postconditions)
    workspace, raw_pom, toolchain = execution_inputs
    status = "success" if exit_code == 0 else "failed"
    if exit_code == 0 and execution_drift:
        status = "stale"
    evidence = {
        "schemaVersion": SCHEMA_VERSION,
        "kind": KIND,
        "status": status,
        "evidenceId": evidence_id,
        "plan": plan,
        "planFingerprint": plan["planFingerprint"],
        "repository": workspace,
        "pom": plan["pom"],
        "rawPom": raw_pom,
        "toolchain": toolchain,
        "postconditions": {
            "repository": postconditions[0],
            "rawPom": postconditions[1],
            "toolchain": postconditions[2],
            "reasons": execution_drift,
        },
        "execution": {
            "startedAt": started_at,
            "finishedAt": finished_at,
            "durationMs": duration_ms,
            "exitCode": exit_code,
            "interrupted": interrupted,
            "argv": argv,
            "hostArgv": host_argv,
            "cwd": str(cwd),
            "log": _path_from_root(project_root, log_path),
            "logSha256": _file_digest(log_path),
            "logSizeBytes": log_path.stat().st_size,
            "tests": _parse_test_statistics("".join(log_parts)),
        },
        "coverage": plan["coverage"],
        "risks": plan.get("warnings", []),
    }
    evidence["evidenceFingerprint"] = _evidence_fingerprint(evidence)
    _write_json_atomic(evidence_path, evidence)
    result = {
        "schemaVersion": SCHEMA_VERSION,
        "kind": KIND,
        "status": evidence["status"],
        "evidence": _path_from_root(project_root, evidence_path),
        "log": _path_from_root(project_root, log_path),
        "exitCode": exit_code,
        "durationMs": duration_ms,
        "coverage": evidence["coverage"],
        "reasons": execution_drift,
    }
    if status == "stale":
        return result, 3
    return result, 0 if exit_code == 0 else max(1, min(exit_code, 125))


def _latest_evidence(project_root: Path, evidence_dir: str | None) -> Path:
    """定位最新的 Maven evidence JSON。"""
    root = _resolve_runtime_dir(project_root, evidence_dir)
    candidates = sorted(
        path for path in root.glob("*.json") if EVIDENCE_FILE_PATTERN.fullmatch(path.name)
    )
    if not candidates:
        raise MavenVerifyError("evidence-missing", f"没有可用 Maven evidence：{root}")
    return candidates[-1]


def _requirements(args: argparse.Namespace, evidence: dict[str, Any]) -> dict[str, Any]:
    """从 require plan 或 CLI 约束构造覆盖要求。"""
    if args.require_plan:
        plan_path = Path(args.require_plan).resolve()
        plan = _read_json(plan_path, PLAN_KIND)
        if plan.get("status") != "planned":
            raise MavenVerifyError("require-plan-invalid", "require plan 状态不是 planned")
        _validate_plan_fingerprint(plan, str(plan_path))
        coverage = plan.get("coverage", {})
        return {
            "goal": coverage.get("level", "compile"),
            "modules": coverage.get("modules", []),
            "consumers": coverage.get("consumers", []),
            "tests": coverage.get("tests", []),
            "artifacts": coverage.get("artifacts", []),
            "testsRequired": bool(coverage.get("tests")) or LIFECYCLE_RANK.get(coverage.get("level", "compile"), 0) >= LIFECYCLE_RANK["test"],
            "planFingerprint": plan.get("planFingerprint"),
        }
    actual = evidence.get("coverage", {})
    return {
        "goal": args.require_goal or actual.get("level", "compile"),
        "modules": _dedupe(args.require_module),
        "consumers": _dedupe(args.require_consumer),
        "tests": _dedupe(args.require_test),
        "artifacts": _dedupe(args.require_artifact),
        "testsRequired": bool(args.require_test) or (
            args.require_goal is not None
            and LIFECYCLE_RANK.get(args.require_goal, 0) >= LIFECYCLE_RANK["test"]
        ),
        "planFingerprint": None,
    }


def _freshness_reasons(evidence: dict[str, Any]) -> list[dict[str, Any]]:
    """只读检查 evidence 与当前 Git/POM/工具链是否匹配。"""
    plan = evidence.get("plan")
    if not isinstance(plan, dict):
        raise MavenVerifyError("evidence-plan-missing", "evidence 缺少冻结计划")
    _validate_plan_fingerprint(plan, "evidence.plan")
    if evidence.get("planFingerprint") != plan.get("planFingerprint"):
        raise MavenVerifyError(
            "evidence-plan-fingerprint-mismatch",
            "evidence 与内嵌计划的指纹不一致",
            evidence=evidence.get("planFingerprint"),
            plan=plan.get("planFingerprint"),
        )
    workspace, raw_pom, toolchain = _current_preconditions(plan, audit_only=True)
    reasons: list[dict[str, Any]] = []
    checks = (
        ("workspace-changed", evidence.get("repository", {}).get("fingerprint"), workspace["fingerprint"]),
        ("pom-changed", evidence.get("rawPom", {}).get("fingerprint"), raw_pom["fingerprint"]),
        ("java-major-changed", evidence.get("toolchain", {}).get("java", {}).get("major"), toolchain["java"].get("major")),
        (
            "java-executable-changed",
            evidence.get("toolchain", {}).get("java", {}).get("executableSha256"),
            toolchain["java"].get("executableSha256"),
        ),
        ("maven-version-changed", evidence.get("toolchain", {}).get("maven", {}).get("version"), toolchain["maven"].get("version")),
        (
            "maven-executable-changed",
            evidence.get("toolchain", {}).get("maven", {}).get("executableSha256"),
            toolchain["maven"].get("executableSha256"),
        ),
        ("maven-build-side-changed", evidence.get("toolchain", {}).get("maven", {}).get("buildSide"), toolchain["maven"].get("buildSide")),
        ("maven-runner-changed", evidence.get("toolchain", {}).get("maven", {}).get("runner"), toolchain["maven"].get("runner")),
        (
            "local-repository-changed",
            evidence.get("toolchain", {}).get("maven", {}).get("localRepository"),
            toolchain["maven"].get("localRepository"),
        ),
        ("maven-arguments-changed", evidence.get("toolchain", {}).get("maven", {}).get("arguments"), toolchain["maven"].get("arguments")),
    )
    for code, expected, actual in checks:
        if expected != actual:
            reasons.append({"code": code, "expected": expected, "actual": actual})
    reasons.extend(_effective_pom_reasons(plan))
    log_value = evidence.get("execution", {}).get("log")
    project_root = Path(plan.get("projectRoot", "")).resolve()
    log_path = Path(log_value) if isinstance(log_value, str) else Path("")
    if log_value:
        log_path = log_path if log_path.is_absolute() else project_root / log_path
    if not log_value or not log_path.is_file():
        reasons.append({"code": "log-missing", "actual": str(log_path) if log_value else None})
    else:
        expected_log = evidence.get("execution", {}).get("logSha256")
        actual_log = _file_digest(log_path)
        if not isinstance(expected_log, str) or expected_log != actual_log:
            reasons.append(
                {
                    "code": "log-changed",
                    "expected": expected_log,
                    "actual": actual_log,
                }
            )
    return reasons


def _coverage_reasons(actual: dict[str, Any], required: dict[str, Any]) -> list[dict[str, Any]]:
    """判断 lifecycle、module、test 和附属制品覆盖缺口。"""
    reasons: list[dict[str, Any]] = []
    if (
        required.get("planFingerprint")
        and required.get("planFingerprint") != actual.get("planFingerprint")
    ):
        reasons.append(
            {
                "code": "plan-semantics-mismatch",
                "required": required.get("planFingerprint"),
                "actual": actual.get("planFingerprint"),
            }
        )
    actual_goal = actual.get("level", "validate")
    required_goal = required.get("goal", "compile")
    if LIFECYCLE_RANK.get(actual_goal, -1) < LIFECYCLE_RANK.get(required_goal, -1):
        reasons.append({"code": "lifecycle-insufficient", "required": required_goal, "actual": actual_goal})
    for key, code in (
        ("modules", "modules-missing"),
        ("consumers", "consumers-missing"),
        ("tests", "tests-missing"),
        ("artifacts", "artifacts-missing"),
    ):
        missing = sorted(set(required.get(key, [])) - set(actual.get(key, [])))
        if missing:
            reasons.append({"code": code, "required": required.get(key, []), "actual": actual.get(key, []), "missing": missing})
    if required.get("testsRequired") and actual.get("testsSkipped"):
        reasons.append({"code": "tests-skipped", "required": True, "actual": True})
    return reasons


def check_evidence(args: argparse.Namespace) -> tuple[dict[str, Any], int]:
    """只读校验 Maven evidence 的新鲜度与覆盖。"""
    repo_root = _git_root(Path.cwd())
    if args.latest:
        evidence_path = _latest_evidence(repo_root, args.evidence_dir)
    elif args.evidence:
        evidence_path = Path(args.evidence).resolve()
    else:
        raise MavenVerifyError("evidence-required", "check 需要 --evidence 或 --latest")
    evidence = _read_json(evidence_path, KIND)
    _validate_evidence_fingerprint(evidence, str(evidence_path))
    if evidence.get("status") == "stale":
        result = {
            "schemaVersion": SCHEMA_VERSION,
            "kind": KIND,
            "status": "stale",
            "coverage": "none",
            "evidence": _path_from_root(repo_root, evidence_path),
            "reasons": evidence.get("postconditions", {}).get("reasons", []),
        }
        return result, 3
    if evidence.get("status") != "success" or evidence.get("execution", {}).get("exitCode") != 0:
        result = {
            "schemaVersion": SCHEMA_VERSION,
            "kind": KIND,
            "status": "failed",
            "coverage": "none",
            "evidence": _path_from_root(repo_root, evidence_path),
            "reasons": [
                {
                    "code": "verification-command-failed",
                    "exitCode": evidence.get("execution", {}).get("exitCode"),
                }
            ],
        }
        return result, 4
    freshness = _freshness_reasons(evidence)
    if freshness:
        result = {
            "schemaVersion": SCHEMA_VERSION,
            "kind": KIND,
            "status": "stale",
            "coverage": "none",
            "evidence": _path_from_root(repo_root, evidence_path),
            "reasons": freshness,
        }
        return result, 3
    required = _requirements(args, evidence)
    actual_coverage = {
        **evidence.get("coverage", {}),
        "planFingerprint": evidence.get("planFingerprint"),
    }
    coverage_reasons = _coverage_reasons(actual_coverage, required)
    status = "partial" if coverage_reasons else "reusable"
    result = {
        "schemaVersion": SCHEMA_VERSION,
        "kind": KIND,
        "status": status,
        "coverage": "partial" if coverage_reasons else "full",
        "evidence": _path_from_root(repo_root, evidence_path),
        "required": required,
        "actual": actual_coverage,
        "reasons": coverage_reasons,
    }
    return result, 2 if coverage_reasons else 0


def _add_common_output(parser: argparse.ArgumentParser) -> None:
    """为子命令添加稳定 JSON 输出参数。"""
    parser.add_argument("--json", action="store_true", help="显式要求 stdout 输出稳定 JSON（默认行为）")
    parser.add_argument("--output", help="同时把最终 JSON 原子写入指定文件")


def build_parser() -> argparse.ArgumentParser:
    """创建 Maven 验证 CLI parser。"""
    parser = argparse.ArgumentParser(description="Trellis Maven 分层验证与 evidence 复用助手")
    subparsers = parser.add_subparsers(dest="command", required=True)

    plan = subparsers.add_parser("plan", help="分析 diff/POM/lifecycle 并生成冻结计划")
    plan.add_argument("--mode", choices=("quick", "final"), required=True)
    plan.add_argument(
        "--compile-strategy",
        choices=("auto", "conservative", "source-stale"),
        default="auto",
    )
    plan.add_argument("--threads", help="显式 Maven 并行度，例如 4、1C、1.5C")
    plan.add_argument("--maven-root")
    plan.add_argument("--module", action="append", default=[])
    plan.add_argument("--consumer", action="append", default=[])
    plan.add_argument("--goal", choices=PLAN_GOALS, default="compile")
    plan.add_argument("--test", action="append", default=[])
    plan.add_argument("--artifact", action="append", choices=sorted(ARTIFACT_NAMES), default=[])
    plan.add_argument("--offline", choices=("auto", "yes", "no"), default="auto")
    plan.add_argument("--local-repository")
    plan.add_argument("--effective-pom")
    plan.add_argument(
        "--maven-executable",
        help="显式同侧 Maven；默认优先项目 wrapper，再复用项目构建侧 PATH 中的 Maven",
    )
    _add_common_output(plan)

    run = subparsers.add_parser("run", help="执行冻结计划并写 evidence")
    plan_input = run.add_mutually_exclusive_group(required=True)
    plan_input.add_argument("--plan-json")
    plan_input.add_argument("--plan-stdin", action="store_true")
    run.add_argument("--evidence-dir")
    _add_common_output(run)

    check = subparsers.add_parser("check", help="只读校验 evidence 新鲜度与覆盖")
    evidence_group = check.add_mutually_exclusive_group(required=True)
    evidence_group.add_argument("--evidence")
    evidence_group.add_argument("--latest", action="store_true")
    check.add_argument("--evidence-dir")
    check.add_argument("--require-plan")
    check.add_argument("--require-goal", choices=PLAN_GOALS)
    check.add_argument("--require-module", action="append", default=[])
    check.add_argument("--require-consumer", action="append", default=[])
    check.add_argument("--require-test", action="append", default=[])
    check.add_argument("--require-artifact", action="append", choices=sorted(ARTIFACT_NAMES), default=[])
    _add_common_output(check)
    return parser


def main(argv: list[str] | None = None) -> int:
    """运行 Maven 验证 CLI。"""
    args = build_parser().parse_args(argv)
    exit_code = 0
    try:
        if args.command == "plan":
            payload = create_plan(args)
            exit_code = 0 if payload.get("status") in {"planned", "not-applicable"} else 5
        elif args.command == "run":
            payload, exit_code = run_plan(args)
        else:
            payload, exit_code = check_evidence(args)
    except MavenVerifyError as error:
        payload = _blocked_payload(error, args.command)
        exit_code = 5
    if args.output:
        _write_json_atomic(Path(args.output).resolve(), payload)
    sys.stdout.write(_stable_json(payload))
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
