#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""从 `.trellis/spec/` 发现相关项目 SOP/spec 文件。

这个 helper 刻意保持轻量：只返回候选路径、置信度、命中原因和章节加载计划，
让 AI 在项目局部知识可能影响做法的决策边界前按需读取；它不把正文注入输出。
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any


MAX_BODY_CHARS = 8000
MAX_SECTION_BODY_CHARS = 4000
FULL_FILE_MAX_BYTES = 12 * 1024
MAX_SECTION_LOAD_BYTES = 12 * 1024
MAX_SELECTED_SECTIONS = 2
DEFAULT_LIMIT = 3
MIN_SCORE = 3
MIN_BODY_ONLY_HITS = 5
MIN_ANCHORED_BODY_HITS = 2
MIN_SECTION_BODY_HITS = 2
NON_ROUTING_BODY_HEADINGS = {
    "tests required",
    "validation & error matrix",
    "validation and error matrix",
    "validation matrix",
    "good/base/bad cases",
    "wrong vs correct",
}
WEAK_TOKENS = {
    "action",
    "actions",
    "after",
    "and",
    "before",
    "change",
    "changes",
    "cli",
    "command",
    "commands",
    "commit",
    "context",
    "current",
    "data",
    "documentation",
    "edit",
    "file",
    "files",
    "flow",
    "flower",
    "for",
    "from",
    "guide",
    "guides",
    "in",
    "index",
    "match",
    "matched",
    "matches",
    "matching",
    "md",
    "normal",
    "of",
    "or",
    "path",
    "paths",
    "project",
    "py",
    "read",
    "readme",
    "reason",
    "reasons",
    "relevant",
    "run",
    "simple",
    "small",
    "sop",
    "spec",
    "status",
    "task",
    "tasks",
    "the",
    "to",
    "trellis",
    "typo",
    "update",
    "with",
    "workflow",
}
TOKEN_RE = re.compile(r"[A-Za-z0-9@]+|[\u4e00-\u9fff]+")
CJK_RE = re.compile(r"^[\u4e00-\u9fff]+$")
HEADER_RE = re.compile(r"^\s{0,3}(#{1,3})\s+(.+?)(?:\s+#+)?\s*$")
FENCE_RE = re.compile(r"^\s{0,3}(`{3,}|~{3,})")
NUMBERED_HEADING_RE = re.compile(r"^\d+(?:\.\d+)*\.\s*")
FRONTMATTER_BOUNDARY_RE = re.compile(r"^---\s*$")
MARKDOWN_LINK_RE = re.compile(r"\[([^\]]+)\]\(([^)]+\.md(?:#[^)]+)?)\)")


@dataclass
class Section:
    """Markdown 章节及其原文件范围。

    Attributes:
        heading: 当前标题文本。
        heading_path: 从父标题到当前标题的展示路径。
        routing_heading: 排除文档 H1 后用于局部路由的标题路径。
        level: ATX 标题层级。
        start_line: 原文件 1-based 起始行。
        end_line: 原文件 1-based 结束行。
        text: 完整章节范围文本。
        sample_text: 不包含子章节的直接正文样本。
    """

    heading: str
    heading_path: str
    routing_heading: str
    level: int
    start_line: int
    end_line: int
    text: str
    sample_text: str


@dataclass
class SectionMatch:
    """带分数和加载预算的章节候选。

    Attributes:
        heading: 章节展示路径。
        start_line: 原文件 1-based 起始行。
        end_line: 原文件 1-based 结束行。
        score: 章节相关性分数。
        confidence: 章节相关性置信度。
        estimated_bytes: 章节完整范围的 UTF-8 字节数。
    """

    heading: str
    start_line: int
    end_line: int
    score: int
    confidence: str
    estimated_bytes: int


@dataclass
class Candidate:
    """带文件分数和加载计划的项目知识候选项。

    Attributes:
        path: 相对项目根目录的 spec 路径。
        score: 文件相关性分数。
        kind: frontmatter 声明或推断出的知识类型。
        load: 原有 frontmatter 加载声明。
        priority: 原有 frontmatter 优先级。
        confidence: 文件相关性置信度。
        reasons: 文件命中原因。
        load_strategy: `full`、`sections` 或 `outline`。
        sections: 建议读取的章节范围。
        action: 面向 AI 的读取动作。
    """

    path: str
    score: int
    kind: str
    load: str
    priority: str
    confidence: str
    reasons: list[str]
    load_strategy: str
    sections: list[SectionMatch]
    action: str


def find_trellis_root(start: Path) -> Path | None:
    """查找最近的 `.trellis/` 所在项目根目录。

    Args:
        start: 开始查找的目录或文件路径。

    Returns:
        项目根目录；找不到 `.trellis/` 时返回 None。
    """
    current = start.resolve()
    if current.is_file():
        current = current.parent
    while True:
        if (current / ".trellis").is_dir():
            return current
        if current == current.parent:
            return None
        current = current.parent


def parse_scalar(value: str) -> str:
    """解析简单 YAML 风格标量值。

    Args:
        value: `key:` 后面的原始值。

    Returns:
        去掉引号后的标量文本。
    """
    value = value.strip()
    if (
        (value.startswith('"') and value.endswith('"'))
        or (value.startswith("'") and value.endswith("'"))
    ):
        return value[1:-1]
    return value


def find_frontmatter_end(lines: list[str]) -> int | None:
    """查找简单 Markdown frontmatter 的结束行索引。

    Args:
        lines: 不带或带换行符的 Markdown 行。

    Returns:
        结束分隔线的 0-based 索引；没有完整 frontmatter 时返回 None。
    """
    if not lines or not FRONTMATTER_BOUNDARY_RE.match(lines[0].rstrip("\r\n")):
        return None
    for index in range(1, len(lines)):
        if FRONTMATTER_BOUNDARY_RE.match(lines[index].rstrip("\r\n")):
            return index
    return None


def parse_frontmatter(text: str) -> tuple[dict[str, Any], str]:
    """解析 Markdown 文件中的简单 frontmatter。

    只支持 `key: value` 和 `key:` 后接 `- item` 列表。为了避免新增依赖，
    刻意不支持复杂 YAML。

    Args:
        text: Markdown 文件内容。

    Returns:
        `(元数据, 去除 frontmatter 后的正文)`。
    """
    lines = text.splitlines()
    end_index = find_frontmatter_end(lines)
    if end_index is None:
        return {}, text

    metadata: dict[str, Any] = {}
    current_key: str | None = None
    for raw_line in lines[1:end_index]:
        line = raw_line.rstrip()
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue

        if current_key and stripped.startswith("- "):
            value = parse_scalar(stripped[2:])
            if value:
                metadata.setdefault(current_key, []).append(value)
            continue

        if ":" not in stripped:
            current_key = None
            continue

        key, value = stripped.split(":", 1)
        key = key.strip()
        if not key:
            current_key = None
            continue

        if value.strip():
            metadata[key] = parse_scalar(value)
            current_key = None
        else:
            metadata[key] = []
            current_key = key

    body = "\n".join(lines[end_index + 1 :])
    return metadata, body


def parse_sections(text: str) -> list[Section]:
    """解析 H1-H3 Markdown 章节并保留原文件行号。

    fenced code block 内的伪标题不会参与章节结构。章节加载范围延伸到下一个
    同级或更高级标题前，匹配样本只延伸到下一个任意标题前，避免把子章节
    的零散正文证据聚合到父章节。

    Args:
        text: 完整 Markdown 文本。

    Returns:
        按原文件顺序排列的章节列表。
    """
    lines = text.splitlines(keepends=True)
    if not lines:
        return []

    frontmatter_end = find_frontmatter_end(lines)
    heading_stack: dict[int, str] = {}
    records: list[tuple[str, str, str, int, int]] = []
    fence_char: str | None = None
    fence_length = 0

    for line_number, raw_line in enumerate(lines, start=1):
        if frontmatter_end is not None and line_number <= frontmatter_end + 1:
            continue

        line = raw_line.rstrip("\r\n")
        fence_match = FENCE_RE.match(line)
        if fence_match:
            marker = fence_match.group(1)
            if fence_char is None:
                fence_char = marker[0]
                fence_length = len(marker)
            elif marker[0] == fence_char and len(marker) >= fence_length:
                fence_char = None
                fence_length = 0
            continue
        if fence_char is not None:
            continue

        header_match = HEADER_RE.match(line)
        if not header_match:
            continue
        marker, heading = header_match.groups()
        level = len(marker)
        heading = heading.strip()
        for existing_level in [item for item in heading_stack if item >= level]:
            heading_stack.pop(existing_level)
        heading_stack[level] = heading
        heading_path = " > ".join(
            heading_stack[item]
            for item in sorted(heading_stack)
            if item <= level
        )
        routing_parts = [
            heading_stack[item]
            for item in sorted(heading_stack)
            if 2 <= item <= level
        ]
        routing_heading = " > ".join(routing_parts) if routing_parts else heading
        records.append(
            (heading, heading_path, routing_heading, level, line_number)
        )

    sections: list[Section] = []
    for index, (
        heading,
        heading_path,
        routing_heading,
        level,
        start_line,
    ) in enumerate(records):
        end_line = len(lines)
        sample_end_line = len(lines)
        if index + 1 < len(records):
            sample_end_line = records[index + 1][4] - 1
        for _, _, _, next_level, next_start_line in records[index + 1 :]:
            if next_level <= level:
                end_line = next_start_line - 1
                break
        sections.append(
            Section(
                heading=heading,
                heading_path=heading_path,
                routing_heading=routing_heading,
                level=level,
                start_line=start_line,
                end_line=end_line,
                text="".join(lines[start_line - 1 : end_line]),
                sample_text="".join(lines[start_line:sample_end_line]),
            )
        )
    return sections


def as_list(value: Any) -> list[str]:
    """把 frontmatter 值转换成字符串列表。

    Args:
        value: 解析后的元数据值。

    Returns:
        非空字符串列表。
    """
    if isinstance(value, list):
        return [str(item).strip() for item in value if str(item).strip()]
    if value is None:
        return []
    text = str(value).strip()
    return [text] if text else []


def add_token(tokens: list[str], seen: set[str], token: str) -> None:
    """按原始顺序追加去重 token。

    Args:
        tokens: 正在构造的 token 列表。
        seen: 已追加 token 集合。
        token: 待追加 token。

    Returns:
        None。
    """
    if len(token) < 2 or token in seen:
        return
    seen.add(token)
    tokens.append(token)


def add_cjk_tokens(tokens: list[str], seen: set[str], text: str) -> None:
    """为连续中文文本追加有限 n-gram token。

    中文没有空格分词。这里生成 2 到 6 字的 n-gram，用 token 集合匹配替代旧
    版任意子串匹配，同时保留 `发版` 命中 `发版流程` 这类常见能力。

    Args:
        tokens: 正在构造的 token 列表。
        seen: 已追加 token 集合。
        text: 连续中文文本。

    Returns:
        None。
    """
    max_size = min(6, len(text))
    for size in range(2, max_size + 1):
        for start in range(0, len(text) - size + 1):
            add_token(tokens, seen, text[start : start + size])


def normalize_tokens(text: str) -> list[str]:
    """提取查询或文档 token，用于确定性的轻量匹配。

    Args:
        text: 查询或可搜索文本。

    Returns:
        去重后的小写 token，保留原始顺序。
    """
    seen: set[str] = set()
    tokens: list[str] = []
    for match in TOKEN_RE.finditer(text.lower()):
        token = match.group(0)
        if CJK_RE.match(token):
            add_cjk_tokens(tokens, seen, token)
        else:
            add_token(tokens, seen, token)
    return tokens


def significant_hits(query_tokens: list[str], target_tokens: list[str]) -> list[str]:
    """计算非弱词 token 命中，保留查询 token 顺序。

    Args:
        query_tokens: 查询 token。
        target_tokens: 待匹配文本 token。

    Returns:
        非弱词命中列表。
    """
    target_set = set(target_tokens)
    return [
        token
        for token in query_tokens
        if token not in WEAK_TOKENS and token in target_set
    ]


def allows_body_routing(section: Section) -> bool:
    """判断章节正文是否适合作为知识路由证据。

    测试矩阵和 Good/Bad 示例章节经常包含故意写入的负例关键词，只允许它们
    通过标题参与明确查询，不让示例正文反向召回整份规范。

    Args:
        section: 待判断章节。

    Returns:
        章节正文可参与路由时返回 True。
    """
    heading = NUMBERED_HEADING_RE.sub("", section.heading.strip().lower())
    return heading not in NON_ROUTING_BODY_HEADINGS


def best_body_hits(
    query_tokens: list[str],
    text: str,
    sections: list[Section],
) -> list[str]:
    """从单个正文区域中选择最强 token 命中。

    有章节时分别采样每个章节的直接正文，避免把多个章节的零散弱证据合并；
    没有章节时保留原有的文档前缀退化行为。

    Args:
        query_tokens: 查询 token。
        text: 完整 Markdown 文本。
        sections: 已解析章节。

    Returns:
        单个最强正文区域的非弱词命中。
    """
    if sections:
        samples = [
            section.sample_text[:MAX_SECTION_BODY_CHARS]
            for section in sections
            if allows_body_routing(section)
        ]
    else:
        _, body = parse_frontmatter(text)
        samples = [body[:MAX_BODY_CHARS]]

    best_hits: list[str] = []
    for sample in samples:
        hits = significant_hits(query_tokens, normalize_tokens(sample))
        if len(hits) > len(best_hits):
            best_hits = hits
    return best_hits


def mask_non_routing_section_bodies(
    text: str,
    sections: list[Section],
) -> str:
    """遮蔽不应参与路由的章节正文并保持原字符位置。

    标题行保留给显式标题匹配；正文字符替换为空格而不是删除，确保后续仍按
    原文件前 `MAX_BODY_CHARS` 字符取样，不会因过滤而扩大前缀窗口。

    Args:
        text: 完整 Markdown 文本。
        sections: 已解析章节。

    Returns:
        保持行号和字符位置、但已遮蔽非路由正文的 Markdown 文本。
    """
    lines = text.splitlines(keepends=True)
    for section in sections:
        if allows_body_routing(section):
            continue
        for line_index in range(section.start_line, section.end_line):
            lines[line_index] = re.sub(r"[^\r\n]", " ", lines[line_index])
    return "".join(lines)


def prefix_body_hits(
    query_tokens: list[str],
    text: str,
    sections: list[Section],
) -> list[str]:
    """计算过滤测试、验证和示例章节后的文件前缀正文证据。

    Args:
        query_tokens: 查询 token。
        text: 完整 Markdown 文本。
        sections: 已解析章节。

    Returns:
        文件正文前缀中的非弱词命中。
    """
    masked_text = mask_non_routing_section_bodies(text, sections)
    _, body = parse_frontmatter(masked_text)
    return significant_hits(
        query_tokens,
        normalize_tokens(body[:MAX_BODY_CHARS]),
    )


def score_section(
    section: Section,
    query_tokens: list[str],
) -> tuple[SectionMatch, int] | None:
    """为单个 Markdown 章节计算局部相关性。

    Args:
        section: 待评分章节。
        query_tokens: 查询 token。

    Returns:
        `(章节候选, 标题层级)`；证据不足时返回 None。
    """
    heading_hits = significant_hits(
        query_tokens,
        normalize_tokens(section.routing_heading),
    )
    body_sample = (
        section.sample_text[:MAX_SECTION_BODY_CHARS]
        if allows_body_routing(section)
        else ""
    )
    body_hits = significant_hits(
        query_tokens,
        normalize_tokens(body_sample),
    )
    if len(heading_hits) >= 2 or (
        heading_hits and len(body_hits) >= MIN_ANCHORED_BODY_HITS
    ):
        confidence = "high"
    elif heading_hits or len(body_hits) >= MIN_SECTION_BODY_HITS:
        confidence = "medium"
    else:
        return None

    score = 4 * len(heading_hits) + len(body_hits)
    return (
        SectionMatch(
            heading=section.heading_path,
            start_line=section.start_line,
            end_line=section.end_line,
            score=score,
            confidence=confidence,
            estimated_bytes=len(section.text.encode("utf-8")),
        ),
        section.level,
    )


def sections_overlap(left: SectionMatch, right: SectionMatch) -> bool:
    """判断两个章节加载范围是否重叠。

    Args:
        left: 左侧章节候选。
        right: 右侧章节候选。

    Returns:
        两个闭区间有交集时返回 True。
    """
    return not (
        left.end_line < right.start_line
        or right.end_line < left.start_line
    )


def select_section_matches(
    sections: list[Section],
    query_tokens: list[str],
) -> list[SectionMatch]:
    """选择预算内、互不重叠的相关章节。

    Args:
        sections: 已解析章节。
        query_tokens: 查询 token。

    Returns:
        至多 `MAX_SELECTED_SECTIONS` 个章节加载范围。
    """
    has_detailed_sections = any(section.level >= 2 for section in sections)
    eligible_sections = [
        section
        for section in sections
        if not has_detailed_sections or section.level >= 2
    ]
    scored = [
        result
        for section in eligible_sections
        if (result := score_section(section, query_tokens)) is not None
    ]
    scored.sort(
        key=lambda item: (
            0 if item[0].confidence == "high" else 1,
            -item[0].score,
            -item[1],
            item[0].estimated_bytes,
            item[0].start_line,
        )
    )

    selected: list[SectionMatch] = []
    total_bytes = 0
    for match, _ in scored:
        if match.estimated_bytes > MAX_SECTION_LOAD_BYTES:
            continue
        if total_bytes + match.estimated_bytes > MAX_SECTION_LOAD_BYTES:
            continue
        if any(sections_overlap(match, existing) for existing in selected):
            continue
        selected.append(match)
        total_bytes += match.estimated_bytes
        if len(selected) >= MAX_SELECTED_SECTIONS:
            break
    return selected


def build_load_plan(
    text: str,
    sections: list[Section],
    query_tokens: list[str],
) -> tuple[str, list[SectionMatch]]:
    """根据文件大小和章节证据生成加载计划。

    Args:
        text: 完整 Markdown 文本。
        sections: 已解析章节。
        query_tokens: 查询 token。

    Returns:
        `(load_strategy, 章节候选)`。
    """
    if len(text.encode("utf-8")) <= FULL_FILE_MAX_BYTES:
        return "full", []
    selected = select_section_matches(sections, query_tokens)
    if selected:
        return "sections", selected
    return "outline", []


def collect_index_descriptions(spec_dir: Path) -> dict[str, list[str]]:
    """从 `index.md` 链接行收集目标文档的路由描述。

    Args:
        spec_dir: `.trellis/spec` 目录。

    Returns:
        以 spec 相对路径为 key 的描述文本列表。
    """
    descriptions: dict[str, list[str]] = {}
    spec_root = spec_dir.resolve()
    for index_path in iter_spec_files(spec_dir):
        if index_path.name != "index.md":
            continue

        text = read_markdown(index_path)
        if text is None:
            continue
        _, body = parse_frontmatter(text)

        for line in body.splitlines():
            matches = list(MARKDOWN_LINK_RE.finditer(line))
            if not matches:
                continue

            clean_line = MARKDOWN_LINK_RE.sub(lambda item: item.group(1), line).strip()
            for match in matches:
                link_target = match.group(2).split("#", 1)[0].strip()
                if "://" in link_target or link_target.startswith("#"):
                    continue

                target_path = (index_path.parent / link_target).resolve()
                try:
                    target_rel_path = target_path.relative_to(spec_root).as_posix()
                except ValueError:
                    continue

                if not target_path.is_file() or target_path == index_path.resolve():
                    continue

                description = f"{match.group(1)} {clean_line}".strip()
                if description:
                    descriptions.setdefault(target_rel_path, []).append(description)
    return descriptions


def classify_confidence(
    matched_triggers: list[str],
    path_hits: list[str],
    header_hits: list[str],
    index_hits: list[str],
    body_hits: list[str],
) -> str | None:
    """根据强锚点和弱证据判断候选置信度。

    Args:
        matched_triggers: frontmatter trigger 命中。
        path_hits: 路径命中。
        header_hits: 标题命中。
        index_hits: index 描述命中。
        body_hits: 正文样本命中。

    Returns:
        `high` / `medium`；证据不足时返回 None。
    """
    if matched_triggers:
        return "high"

    anchor_groups = [path_hits, header_hits, index_hits]
    anchor_count = sum(1 for group in anchor_groups if group)
    if len(path_hits) >= 2 or len(header_hits) >= 2 or len(index_hits) >= 2:
        return "high"
    if anchor_count >= 2:
        return "high"
    if anchor_count == 1 and len(body_hits) >= MIN_ANCHORED_BODY_HITS:
        return "high"
    if anchor_count == 1:
        return "medium"
    if len(body_hits) >= MIN_BODY_ONLY_HITS:
        return "medium"
    return None


def action_for_load_strategy(confidence: str, load_strategy: str) -> str:
    """按置信度和加载策略返回读取建议。

    Args:
        confidence: 文件候选置信度。
        load_strategy: `full` / `sections` / `outline`。

    Returns:
        面向 AI 的精确行动建议。
    """
    if confidence == "high":
        return {
            "full": "read full file before acting",
            "sections": "read matched sections before acting; expand only if needed",
            "outline": "inspect headings and read relevant sections before acting",
        }[load_strategy]
    return {
        "full": "read full file if clearly relevant",
        "sections": "read matched sections if clearly relevant",
        "outline": "inspect headings if clearly relevant",
    }[load_strategy]


def read_markdown(path: Path) -> str | None:
    """以容错 UTF-8 方式读取 Markdown 文件。

    Args:
        path: Markdown 文件路径。

    Returns:
        文件内容；读取失败时返回 None。
    """
    try:
        return path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None


def iter_spec_files(spec_dir: Path) -> list[Path]:
    """列出 `.trellis/spec/` 下的 Markdown 文件，包括 guides。

    Args:
        spec_dir: `.trellis/spec` 目录。

    Returns:
        排序后的 Markdown 路径列表。
    """
    if not spec_dir.is_dir():
        return []

    result: list[Path] = []
    for path in spec_dir.rglob("*.md"):
        rel_parts = path.relative_to(spec_dir).parts
        if any(part.startswith(".") for part in rel_parts):
            continue
        result.append(path)
    return sorted(result)


def score_file(
    root: Path,
    path: Path,
    query: str,
    query_tokens: list[str],
    index_descriptions: dict[str, list[str]],
) -> Candidate | None:
    """按查询为一个 Markdown spec 文件打分。

    Args:
        root: 项目根目录。
        path: Markdown 文件路径。
        query: 原始查询文本。
        query_tokens: 标准化后的查询 token。
        index_descriptions: 从 index.md 收集的目标文档描述。

    Returns:
        分数达到阈值时返回候选项，否则返回 None。
    """
    text = read_markdown(path)
    if text is None:
        return None

    metadata, _ = parse_frontmatter(text)
    sections = parse_sections(text)
    rel_path = path.relative_to(root).as_posix()
    spec_rel_path = path.relative_to(root / ".trellis" / "spec").as_posix()
    spec_rel_tokens = normalize_tokens(spec_rel_path)
    header_tokens = normalize_tokens(
        " ".join(section.heading for section in sections)
    )
    index_text = " ".join(index_descriptions.get(spec_rel_path, []))
    index_tokens = normalize_tokens(index_text)

    kind = str(metadata.get("kind") or "").strip()
    load = str(metadata.get("load") or "").strip()
    priority = str(metadata.get("priority") or "").strip()
    triggers = as_list(metadata.get("triggers"))

    score = 0
    reasons: list[str] = []
    query_lower = query.lower()

    matched_triggers: list[str] = []
    for trigger in triggers:
        trigger_lower = trigger.lower()
        if trigger_lower and trigger_lower in query_lower:
            matched_triggers.append(trigger)
    if matched_triggers:
        score += 8 * len(matched_triggers)
        reasons.append(f"matched triggers: {', '.join(matched_triggers[:5])}")

    path_hits = significant_hits(query_tokens, spec_rel_tokens)
    if path_hits:
        score += 5 * len(path_hits)
        reasons.append(f"matched path tokens: {', '.join(path_hits[:5])}")

    header_hits = significant_hits(query_tokens, header_tokens)
    if header_hits:
        score += 3 * len(header_hits)
        reasons.append(f"matched headings: {', '.join(header_hits[:5])}")

    index_hits = significant_hits(query_tokens, index_tokens)
    if index_hits:
        score += 4 * len(index_hits)
        reasons.append(f"matched index descriptions: {', '.join(index_hits[:5])}")

    section_body_hits = best_body_hits(query_tokens, text, sections)
    existing_body_hits = prefix_body_hits(query_tokens, text, sections)
    has_anchor = bool(matched_triggers or path_hits or header_hits or index_hits)
    body_hits = existing_body_hits if has_anchor else section_body_hits
    if body_hits:
        score += len(body_hits)
        reasons.append(f"matched body tokens: {', '.join(body_hits[:5])}")

    # 避免 `to` / `flow` / `commit` 这类泛词或少量正文词把无关文件拉进上下文。
    confidence = classify_confidence(
        matched_triggers,
        path_hits,
        header_hits,
        index_hits,
        body_hits,
    )
    if confidence is None:
        return None

    if kind.lower() in {"sop", "procedure", "guide", "thinking-guide"} and score > 0:
        score += 2
    if load.lower() in {"before_action", "before-acting", "before_acting"} and score > 0:
        score += 2
    if priority.lower() == "high" and score > 0:
        score += 2

    if score < MIN_SCORE:
        return None

    load_strategy, section_matches = build_load_plan(
        text,
        sections,
        query_tokens,
    )

    return Candidate(
        path=rel_path,
        score=score,
        kind=kind or ("thinking-guide" if "/guides/" in f"/{rel_path}" else "spec"),
        load=load,
        priority=priority,
        confidence=confidence,
        reasons=reasons,
        load_strategy=load_strategy,
        sections=section_matches,
        action=action_for_load_strategy(confidence, load_strategy),
    )


def find_candidates(root: Path, query: str, limit: int) -> list[Candidate]:
    """按查询查找项目知识候选项。

    Args:
        root: 项目根目录。
        query: 描述意图动作的短查询。
        limit: 最大候选数量。

    Returns:
        排序后的候选项列表。
    """
    query_tokens = normalize_tokens(query)
    if not query_tokens:
        return []

    candidates: list[Candidate] = []
    spec_dir = root / ".trellis" / "spec"
    index_descriptions = collect_index_descriptions(spec_dir)
    for path in iter_spec_files(spec_dir):
        candidate = score_file(root, path, query, query_tokens, index_descriptions)
        if candidate:
            candidates.append(candidate)

    candidates.sort(
        key=lambda item: (
            0 if item.confidence == "high" else 1,
            -item.score,
            item.path,
        )
    )
    return candidates[:limit]


def format_markdown(candidates: list[Candidate]) -> str:
    """把候选项格式化为给 AI 阅读的紧凑 Markdown。

    Args:
        candidates: 已打分候选项。

    Returns:
        Markdown 输出。
    """
    lines = ["## Relevant Project Knowledge", ""]
    if not candidates:
        lines.append("No relevant project SOP/spec matched. Continue with the normal workflow.")
        return "\n".join(lines)

    for candidate in candidates:
        lines.append(f"- {candidate.path}")
        lines.append(f"  kind: {candidate.kind}")
        lines.append(f"  score: {candidate.score}")
        lines.append(f"  confidence: {candidate.confidence}")
        lines.append(f"  load_strategy: {candidate.load_strategy}")
        for section in candidate.sections:
            lines.append(
                "  section: "
                f"lines {section.start_line}-{section.end_line} | "
                f"{section.heading} | score={section.score} | "
                f"confidence={section.confidence} | "
                f"bytes={section.estimated_bytes}"
            )
        if candidate.load:
            lines.append(f"  load: {candidate.load}")
        if candidate.priority:
            lines.append(f"  priority: {candidate.priority}")
        if candidate.reasons:
            lines.append(f"  reason: {'; '.join(candidate.reasons)}")
        lines.append(f"  action: {candidate.action}")
    return "\n".join(lines)


def format_json(candidates: list[Candidate]) -> str:
    """把候选项格式化为 JSON。

    Args:
        candidates: 已打分候选项。

    Returns:
        JSON 字符串。
    """
    payload = [
        {
            "file": candidate.path,
            "kind": candidate.kind,
            "score": candidate.score,
            "confidence": candidate.confidence,
            "load": candidate.load,
            "priority": candidate.priority,
            "reason": candidate.reasons,
            "load_strategy": candidate.load_strategy,
            "sections": [
                {
                    "heading": section.heading,
                    "start_line": section.start_line,
                    "end_line": section.end_line,
                    "score": section.score,
                    "confidence": section.confidence,
                    "estimated_bytes": section.estimated_bytes,
                }
                for section in candidate.sections
            ],
            "action": candidate.action,
        }
        for candidate in candidates
    ]
    return json.dumps(payload, ensure_ascii=False, indent=2)


def parse_args(argv: list[str]) -> argparse.Namespace:
    """解析 CLI 参数。

    Args:
        argv: 不包含可执行文件名的命令行参数。

    Returns:
        解析后的参数。
    """
    parser = argparse.ArgumentParser(
        description="Discover relevant project SOP/spec files from .trellis/spec/."
    )
    parser.add_argument(
        "query",
        nargs="*",
        help="Short query describing the intended action.",
    )
    parser.add_argument(
        "--limit",
        type=int,
        default=DEFAULT_LIMIT,
        help=f"Maximum number of matches to return (default: {DEFAULT_LIMIT}).",
    )
    parser.add_argument(
        "--json",
        action="store_true",
        help="Output JSON instead of Markdown.",
    )
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    """运行项目知识发现命令。

    Args:
        argv: 可选命令行参数，不包含可执行文件名。

    Returns:
        进程退出码。
    """
    args = parse_args(sys.argv[1:] if argv is None else argv)
    query = " ".join(args.query).strip()
    root = find_trellis_root(Path.cwd()) or find_trellis_root(Path(__file__).resolve())
    if root is None:
        print("## Relevant Project Knowledge\n\nNo .trellis/ directory found. Continue with the normal workflow.")
        return 0

    candidates = find_candidates(root, query, max(1, args.limit))
    print(format_json(candidates) if args.json else format_markdown(candidates))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
