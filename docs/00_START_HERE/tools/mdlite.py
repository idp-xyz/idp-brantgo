"""Markdown-lite renderer for IDP Brant master archive page sources.

Supported blocks: ### / #### headings, paragraphs, - / 1. lists (one nesting
level), pipe tables, > [!kind] callouts, ``` code fences, raw HTML lines,
::: cards blocks and {{include key=value}} directives.
"""

from __future__ import annotations

import html
import re
from typing import Callable

IncludeFn = Callable[[dict[str, str]], str]

_INLINE_CODE = re.compile(r"`([^`]+)`")
_BOLD = re.compile(r"\*\*(.+?)\*\*")
_LINK = re.compile(r"\[([^\]]+)\]\(([^)\s]+)\)")
_INCLUDE = re.compile(r"^\{\{\s*([\w.-]+)((?:\s+[\w-]+=(?:\"[^\"]*\"|\S+))*)\s*\}\}$")
_ARG = re.compile(r"([\w-]+)=(\"[^\"]*\"|\S+)")
_OL = re.compile(r"^(\s*)(\d+)\.\s+(.*)$")
_UL = re.compile(r"^(\s*)[-*]\s+(.*)$")


def inline(text: str) -> str:
    """Escape text and apply inline code, bold and links."""
    placeholders: list[str] = []

    def stash(fragment: str) -> str:
        placeholders.append(fragment)
        return f"\x00{len(placeholders) - 1}\x00"

    text = _INLINE_CODE.sub(lambda m: stash(f"<code>{html.escape(m.group(1))}</code>"), text)
    text = _LINK.sub(
        lambda m: stash(f'<a href="{html.escape(m.group(2), quote=True)}">{html.escape(m.group(1))}</a>'),
        text,
    )
    text = html.escape(text, quote=False)
    text = _BOLD.sub(lambda m: f"<b>{m.group(1)}</b>", text)
    return re.sub(r"\x00(\d+)\x00", lambda m: placeholders[int(m.group(1))], text)


def _table(lines: list[str]) -> str:
    rows = [[c.strip() for c in ln.strip().strip("|").split("|")] for ln in lines]
    head = rows[0]
    body = [r for r in rows[2:]] if len(rows) > 1 and set("".join(rows[1])) <= set("-: ") else rows[1:]
    out = ['<div class="table-scroll" tabindex="0"><table><thead><tr>']
    out += [f"<th>{inline(h)}</th>" for h in head]
    out.append("</tr></thead><tbody>")
    for r in body:
        cells = r + [""] * (len(head) - len(r))
        out.append("<tr>" + "".join(f"<td>{inline(c)}</td>" for c in cells[: len(head)]) + "</tr>")
    out.append("</tbody></table></div>")
    return "".join(out)


def _list(lines: list[str]) -> str:
    """Render a list block with at most one nesting level."""
    out: list[str] = []
    stack: list[tuple[str, int]] = []

    def open_list(kind: str, indent: int) -> None:
        out.append(f"<{kind}>")
        stack.append((kind, indent))

    for ln in lines:
        m_ol, m_ul = _OL.match(ln), _UL.match(ln)
        m = m_ol or m_ul
        if not m:
            if out and out[-1].endswith("</li>"):
                out[-1] = out[-1][: -len("</li>")] + " " + inline(ln.strip()) + "</li>"
            continue
        indent = len(m.group(1))
        kind = "ol" if m_ol else "ul"
        content = m.group(3) if m_ol else m.group(2)
        if not stack:
            open_list(kind, indent)
        elif indent > stack[-1][1]:
            if out and out[-1].endswith("</li>"):
                out[-1] = out[-1][: -len("</li>")]
                stack.append(("li-open", stack[-1][1]))
            open_list(kind, indent)
        else:
            while stack and indent < stack[-1][1]:
                k, _ = stack.pop()
                out.append(f"</{k}>")
                if stack and stack[-1][0] == "li-open":
                    stack.pop()
                    out.append("</li>")
        out.append(f"<li>{inline(content)}</li>")
    while stack:
        k, _ = stack.pop()
        out.append("</li>" if k == "li-open" else f"</{k}>")
    return "".join(out)


def _callout(lines: list[str]) -> str:
    first = lines[0].lstrip("> ").strip()
    kind, title = "note", ""
    m = re.match(r"\[!(\w+)\]\s*(.*)", first)
    body_lines = lines[1:]
    if m:
        kind, title = m.group(1).lower(), m.group(2)
    else:
        body_lines = lines
    paras = []
    buf: list[str] = []
    for ln in body_lines:
        t = ln[1:].strip() if ln.startswith(">") else ln.strip()
        if not t:
            if buf:
                paras.append(" ".join(buf))
                buf = []
        else:
            buf.append(t)
    if buf:
        paras.append(" ".join(buf))
    cls = {"note": "callout", "evidence": "callout evidence", "decision": "callout decision", "warn": "callout warn"}.get(
        kind, "callout"
    )
    head = f"<strong>{inline(title)}</strong>" if title else ""
    return f'<div class="{cls}">{head}' + "".join(f"<p>{inline(p)}</p>" for p in paras) + "</div>"


def _cards(lines: list[str]) -> str:
    items = []
    for ln in lines:
        ln = ln.strip()
        if not ln.startswith("- "):
            continue
        parts = [p.strip() for p in ln[2:].split("|")]
        while len(parts) < 3:
            parts.append("")
        label, title, desc = parts[0], parts[1], "|".join(parts[2:]).strip()
        items.append(
            f"<article><span>{inline(label)}</span><h3>{inline(title)}</h3><p>{inline(desc)}</p></article>"
        )
    return '<div class="principle-grid">' + "".join(items) + "</div>"


def render(text: str, includes: dict[str, IncludeFn] | None = None) -> str:
    """Render a markdown-lite fragment to HTML."""
    includes = includes or {}
    lines = text.replace("\r\n", "\n").split("\n")
    out: list[str] = []
    i = 0
    para: list[str] = []

    def flush_para() -> None:
        if para:
            out.append(f"<p>{inline(' '.join(s.strip() for s in para))}</p>")
            para.clear()

    while i < len(lines):
        ln = lines[i]
        s = ln.strip()
        if not s:
            flush_para()
            i += 1
            continue
        if s.startswith("```"):
            flush_para()
            lang = s[3:].strip()
            i += 1
            code = []
            while i < len(lines) and not lines[i].strip().startswith("```"):
                code.append(lines[i])
                i += 1
            i += 1
            label = f' data-lang="{html.escape(lang)}"' if lang else ""
            out.append(
                f'<div class="code-wrap"{label}><button class="copy-code" type="button">复制</button>'
                f"<pre><code>{html.escape(chr(10).join(code))}</code></pre></div>"
            )
            continue
        m_inc = _INCLUDE.match(s)
        if m_inc:
            flush_para()
            name = m_inc.group(1)
            args = {k: v.strip('"') for k, v in _ARG.findall(m_inc.group(2) or "")}
            if name not in includes:
                raise KeyError(f"unknown include: {name}")
            out.append(includes[name](args))
            i += 1
            continue
        if s.startswith(":::"):
            flush_para()
            kind = s[3:].strip()
            i += 1
            block = []
            while i < len(lines) and lines[i].strip() != ":::":
                block.append(lines[i])
                i += 1
            i += 1
            if kind == "cards":
                out.append(_cards(block))
            elif kind == "cover":
                cells = []
                for ln in block:
                    ln = ln.strip()
                    if ln.startswith("- "):
                        head, _, body = ln[2:].partition("|")
                        cells.append(f"<div><strong>{inline(head.strip())}</strong>{inline(body.strip())}</div>")
                out.append('<div class="cover-grid">' + "".join(cells) + "</div>")
            elif kind.startswith("notice"):
                title = kind[len("notice"):].strip()
                body = " ".join(b.strip() for b in block if b.strip())
                head = f"<strong>{inline(title)}</strong>" if title else ""
                out.append(f'<div class="notice">{head}{inline(body)}</div>')
            elif kind.startswith("details"):
                summary = kind[len("details"):].strip() or "展开详情"
                out.append(f"<details><summary>{inline(summary)}</summary><div class=\"prose\">{render(chr(10).join(block), includes)}</div></details>")
            else:
                out.append(f'<div class="{html.escape(kind)}">{render(chr(10).join(block), includes)}</div>')
            continue
        if s.startswith("#### "):
            flush_para()
            out.append(f"<h4>{inline(s[5:])}</h4>")
            i += 1
            continue
        if s.startswith("### "):
            flush_para()
            out.append(f"<h3>{inline(s[4:])}</h3>")
            i += 1
            continue
        if s.startswith("|"):
            flush_para()
            block = []
            while i < len(lines) and lines[i].strip().startswith("|"):
                block.append(lines[i])
                i += 1
            out.append(_table(block))
            continue
        if s.startswith(">"):
            flush_para()
            block = []
            while i < len(lines) and lines[i].strip().startswith(">"):
                block.append(lines[i].strip())
                i += 1
            out.append(_callout(block))
            continue
        if _UL.match(ln) or _OL.match(ln):
            flush_para()
            block = []
            while i < len(lines) and lines[i].strip() and (
                _UL.match(lines[i]) or _OL.match(lines[i]) or lines[i].startswith("  ")
            ):
                block.append(lines[i])
                i += 1
            out.append(_list(block))
            continue
        if s.startswith("<"):
            flush_para()
            block = []
            while i < len(lines) and lines[i].strip():
                block.append(lines[i])
                i += 1
            out.append("\n".join(block))
            continue
        para.append(ln)
        i += 1
    flush_para()
    return "\n".join(out)
