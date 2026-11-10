"""Deterministic inline SVG diagrams for the IDP Brant master archive."""

from __future__ import annotations

import html
import math
import re

TIER_FILL = {
    "Core": ("#0b7b7c", "#ffffff"),
    "SupportingPlus": ("#6b4fa0", "#ffffff"),
    "Supporting": ("#e8eff3", "#123248"),
    "Generic": ("#f3eadb", "#5b4520"),
    "Optional": ("#eef1f3", "#5e6d78"),
}
NAVY = "#123248"
TEAL = "#087f80"
MUTED = "#647b8a"
LINE = "#c9d8dc"


def esc(s: object) -> str:
    return html.escape(str(s), quote=True)


def text_width(s: str, size: float) -> float:
    w = 0.0
    for ch in s:
        w += size * (1.0 if ord(ch) > 0x2E80 else 0.58)
    return w


_TOKEN = re.compile(r"[A-Za-z0-9_.\-/×@]+\s*|\s+|.")


def wrap(s: str, size: float, max_width: float) -> list[str]:
    """Wrap text at CJK characters or between Latin words; hard-break only over-long words."""
    lines, cur = [], ""
    for tok in _TOKEN.findall(s):
        if text_width(cur + tok, size) <= max_width:
            cur += tok
            continue
        if cur.strip():
            lines.append(cur.rstrip())
            cur = ""
        tok = tok.lstrip()
        while text_width(tok, size) > max_width:
            cut = 1
            while cut < len(tok) and text_width(tok[: cut + 1], size) <= max_width:
                cut += 1
            lines.append(tok[:cut])
            tok = tok[cut:]
        cur = tok
    if cur.strip():
        lines.append(cur.rstrip())
    return lines


def _svg(width: float, height: float, body: str, title: str) -> str:
    return (
        f'<svg viewBox="0 0 {width:.0f} {height:.0f}" xmlns="http://www.w3.org/2000/svg" role="img" '
        f'aria-label="{esc(title)}"><title>{esc(title)}</title>'
        '<defs><marker id="arr" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
        f'<path d="M0,0 L10,5 L0,10 z" fill="{MUTED}"/></marker>'
        '<marker id="arrT" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
        f'<path d="M0,0 L10,5 L0,10 z" fill="{TEAL}"/></marker></defs>'
        f"{body}</svg>"
    )


def _box(x, y, w, h, fill, stroke, rx=8, extra=""):
    return f'<rect x="{x:.1f}" y="{y:.1f}" width="{w:.1f}" height="{h:.1f}" rx="{rx}" fill="{fill}" stroke="{stroke}" {extra}/>'


def _text(x, y, s, size=12, fill=NAVY, weight="400", anchor="middle"):
    return (
        f'<text x="{x:.1f}" y="{y:.1f}" font-size="{size}" fill="{fill}" font-weight="{weight}" '
        f'text-anchor="{anchor}">{esc(s)}</text>'
    )


def _clip(cx, cy, w, h, tx, ty):
    """Point where the line from box centre to (tx, ty) leaves the box."""
    dx, dy = tx - cx, ty - cy
    if dx == 0 and dy == 0:
        return cx, cy
    sx = (w / 2) / abs(dx) if dx else math.inf
    sy = (h / 2) / abs(dy) if dy else math.inf
    s = min(sx, sy)
    return cx + dx * s, cy + dy * s


def value_chain() -> str:
    W, H = 1120, 360
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    body.append(_box(30, 20, W - 60, 56, "#123248", "none", 10))
    body.append(_text(W / 2, 44, "业务任务与 AI-Native 执行（TSK ＋ internal/ai，不拥有交易事实）", 14, "#ffffff", "600"))
    body.append(_text(W / 2, 64, "意图接收 → 上下文组装 → 方案与候选 → 人工审核 → 受控命令落地 → 权威记录核验", 12, "#bdd5dd"))
    stages = [
        ("询价", "INQ", "Supporting"), ("寻源", "SUP", "SupportingPlus"), ("核价", "CST", "Core"),
        ("报价与审批", "TRD", "Core"), ("客户确认", "TRD", "Core"), ("外销订单", "TRD", "Core"),
        ("采购", "PUR", "Core"), ("生产协同", "MFG", "Supporting"), ("质检放行", "QLT", "Supporting"),
        ("出运", "FUL", "Supporting"), ("结算", "STL", "Core"), ("售后", "CLM", "Supporting"),
    ]
    bw, bh, gap = 150, 62, 30
    for i, (cn, code, tier) in enumerate(stages):
        row, col = divmod(i, 6)
        x = 45 + col * (bw + gap) if row == 0 else 45 + (5 - col) * (bw + gap)
        y = 112 + row * 108
        fill, ink = TIER_FILL[tier]
        body.append(_box(x, y, bw, bh, fill, "#9fb7bf" if tier in ("Supporting", "Optional", "Generic") else fill, 10))
        body.append(_text(x + bw / 2, y + 27, cn, 14, ink, "600"))
        body.append(_text(x + bw / 2, y + 47, code, 11, ink if tier in ("Core", "SupportingPlus") else MUTED, "700"))
        if i < len(stages) - 1:
            if i == 5:
                body.append(f'<path d="M{x + bw / 2:.0f},{y + bh} L{x + bw / 2:.0f},{y + 108 - 2}" stroke="{MUTED}" stroke-width="1.6" fill="none" marker-end="url(#arr)"/>')
            elif row == 0:
                body.append(f'<path d="M{x + bw:.0f},{y + bh / 2} L{x + bw + gap - 3:.0f},{y + bh / 2}" stroke="{MUTED}" stroke-width="1.6" marker-end="url(#arr)"/>')
            else:
                body.append(f'<path d="M{x:.0f},{y + bh / 2} L{x - gap + 3:.0f},{y + bh / 2}" stroke="{MUTED}" stroke-width="1.6" marker-end="url(#arr)"/>')
    body.append(_box(30, 300, W - 60, 46, "#f3eadb", "#e0cfb0", 10))
    body.append(_text(W / 2, 328, "共享基础：PTY 交易主体 · CAT 商品与规格 · POL 商业政策与基础配置 · EVD 来源证据 · CNX 中国出口专业协同", 13, "#5b4520", "600"))
    return _svg(W, H, "".join(body), "IDP Brant 业务全景与上下文分工")


LAYER_ROWS = [
    ("任务与 AI", ["TSK"]),
    ("售前与商业", ["CRM", "INQ", "SUP", "CST"]),
    ("交易承诺", ["TRD"]),
    ("供给与履约", ["PUR", "MFG", "QLT", "STK", "FUL"]),
    ("资金与合规", ["STL", "CLM", "CNX"]),
    ("共享基础", ["PTY", "CAT", "POL", "EVD"]),
]
MAIN_FLOWS = [
    ("INQ", "SUP"), ("INQ", "CST"), ("SUP", "CST"), ("CST", "TRD"), ("TRD", "PUR"), ("SUP", "PUR"),
    ("PUR", "QLT"), ("QLT", "FUL"), ("TRD", "FUL"), ("FUL", "STL"), ("PUR", "STL"), ("FUL", "CNX"),
    ("FUL", "CLM"), ("CLM", "STL"), ("TSK", "TRD"), ("TRD", "TSK"), ("PUR", "MFG"), ("MFG", "QLT"),
    ("STK", "FUL"), ("CRM", "INQ"),
]


def context_layers(contexts: list[dict]) -> str:
    by_id = {c["id"]: c for c in contexts}
    W, bw, bh = 1180, 168, 62
    row_h = 112
    H = 40 + row_h * len(LAYER_ROWS) + 20
    pos: dict[str, tuple[float, float]] = {}
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    for r, (label, ids) in enumerate(LAYER_ROWS):
        y = 30 + r * row_h
        body.append(_box(14, y - 10, W - 28, row_h - 16, "#ffffff" if r % 2 else "#f5f9f9", "#e3ecee", 10))
        body.append(_text(30, y + 8, label, 11, MUTED, "700", "start"))
        n = len(ids)
        span = n * bw + (n - 1) * 40
        x0 = 150 + (W - 170 - span) / 2
        for i, cid in enumerate(ids):
            c = by_id[cid]
            x = x0 + i * (bw + 40)
            fill, ink = TIER_FILL[c["tier"]]
            dash = ' stroke-dasharray="5 4"' if c.get("conditional") else ""
            body.append(_box(x, y + 6, bw, bh, fill, "#90aab3" if fill.startswith("#e") or fill.startswith("#f") else fill, 9, dash))
            body.append(_text(x + bw / 2, y + 30, c["id"], 15, ink, "800"))
            body.append(_text(x + bw / 2, y + 51, c["name"][:12], 11.5, ink))
            pos[cid] = (x + bw / 2, y + 6 + bh / 2)
    for a, b in MAIN_FLOWS:
        if a not in pos or b not in pos:
            continue
        (ax, ay), (bx, by) = pos[a], pos[b]
        if (b, a) in MAIN_FLOWS and a > b:
            ax += 10
            bx += 10
        sx, sy = _clip(ax, ay, bw + 6, bh + 6, bx, by)
        ex, ey = _clip(bx, by, bw + 10, bh + 10, ax, ay)
        body.append(f'<path d="M{sx:.1f},{sy:.1f} L{ex:.1f},{ey:.1f}" stroke="{TEAL}" stroke-opacity=".55" stroke-width="1.5" fill="none" marker-end="url(#arrT)"/>')
    y = 30 + (len(LAYER_ROWS) - 1) * row_h
    body.append(_text(W - 30, y + 90, "共享基础向多数上下文发布已发布语言（PartyRef、SkuRef、ConfigRef、SourceRef），图中省略连线", 10.5, MUTED, "400", "end"))
    return _svg(W, H, "".join(body), "IDP Brant 候选限界上下文分层图")


def dependency_matrix(contexts: list[dict], relationships: list[dict]) -> str:
    ids = [c["id"] for c in contexts]
    tier = {c["id"]: c["tier"] for c in contexts}
    cell, left, top = 30, 84, 74
    n = len(ids)
    W, H = left + n * cell + 190, top + n * cell + 30
    rel = {(r["to"], r["frm"]): r for r in relationships}
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    body.append(_text(left + n * cell / 2, 18, "列：上游（被依赖）", 11, MUTED, "600"))
    for j, up in enumerate(ids):
        x = left + j * cell + cell / 2
        fill, ink = TIER_FILL[tier[up]]
        body.append(_box(left + j * cell + 2, top - 34, cell - 4, 28, fill, "#b4c7cd", 4))
        body.append(_text(x, top - 15, up, 9.5, ink, "700"))
    for i, down in enumerate(ids):
        y = top + i * cell
        fill, ink = TIER_FILL[tier[down]]
        body.append(_box(left - 64, y + 2, 58, cell - 4, fill, "#b4c7cd", 4))
        body.append(_text(left - 35, y + cell / 2 + 4, down, 10, ink, "700"))
        for j, up in enumerate(ids):
            x = left + j * cell
            r = rel.get((down, up))
            if down == up:
                body.append(_box(x + 1, y + 1, cell - 2, cell - 2, "#eef2f3", "none", 3))
            elif r:
                guard = "Guard" in r["pattern"]
                started = not r["contractStatus"].startswith("NOT_STARTED")
                fillc = TEAL if started else "#9cc9c9"
                body.append(_box(x + 3, y + 3, cell - 6, cell - 6, fillc, "#ba743e" if guard else fillc, 4, 'stroke-width="2.4"' if guard else ""))
            else:
                body.append(_box(x + 1, y + 1, cell - 2, cell - 2, "#ffffff", "#e6eef0", 2))
    body.append(f'<text x="{left - 74}" y="{top + n * cell / 2}" font-size="11" fill="{MUTED}" font-weight="600" text-anchor="middle" transform="rotate(-90 {left - 74} {top + n * cell / 2})">行：下游（依赖方）</text>')
    lx = left + n * cell + 18
    legend = [(TEAL, TEAL, ["已登记关系", "（契约设计中）"]), ("#9cc9c9", "#9cc9c9", ["已登记关系", "（尚未开始）"]), ("#ffffff", "#ba743e", ["金色边：需资格守卫", "（ADR-BR-004）"])]
    for k, (f, s, lines) in enumerate(legend):
        yy = top + k * 40
        body.append(_box(lx, yy, 22, 22, f, s, 4, 'stroke-width="2.4"'))
        for li, part in enumerate(lines):
            body.append(_text(lx + 30, yy + 10 + li * 14, part, 10.5, NAVY, "400", "start"))
    return _svg(W, H, "".join(body), "可执行上下文映射：import 白名单矩阵")


def _verb(command: str) -> str:
    """Drop the object prefix (quote., supplier_offer., task. …); the full command is in the table."""
    if command.startswith("（"):
        return command
    parts = command.split(".")
    return ".".join(parts[1:]) if len(parts) > 1 else command


def state_machine(sm: dict) -> str:
    layout = sm["layout"]
    nw, nh, gx, gy = 158, 46, 252, 112
    cols = max(v[0] for v in layout.values()) + 1
    rows = max(v[1] for v in layout.values()) + 1
    W = 40 + cols * gx
    H = 70 + rows * gy
    center = {s: (30 + c * gx + nw / 2, 50 + r * gy + nh / 2) for s, (c, r) in layout.items()}
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    pair_count: dict[tuple[str, str], int] = {}
    for t in sm["transitions"]:
        a, b = t["from"], t["to"]
        if a not in center or b not in center:
            continue
        key = (a, b)
        k = pair_count.get(key, 0)
        pair_count[key] = k + 1
        (ax, ay), (bx, by) = center[a], center[b]
        label = _verb(t["command"])
        if a == b:
            body.append(f'<path d="M{ax - 30:.0f},{ay - nh / 2:.0f} C{ax - 40:.0f},{ay - nh / 2 - 46:.0f} {ax + 40:.0f},{ay - nh / 2 - 46:.0f} {ax + 30:.0f},{ay - nh / 2:.0f}" stroke="{MUTED}" fill="none" stroke-width="1.3" marker-end="url(#arr)"/>')
            body.append(_text(ax, ay - nh / 2 - 36, label, 10, TEAL, "600"))
            continue
        sx, sy = _clip(ax, ay, nw + 4, nh + 4, bx, by)
        ex, ey = _clip(bx, by, nw + 8, nh + 8, ax, ay)
        back = (bx < ax) or (bx == ax and by < ay) or k > 0
        if back:
            mx, my = (sx + ex) / 2, (sy + ey) / 2
            dx, dy = ex - sx, ey - sy
            length = math.hypot(dx, dy) or 1
            nx, ny = -dy / length, dx / length
            # Curve downwards for horizontal-ish edges, rightwards for vertical ones,
            # so a return edge never shares the label lane of its forward edge.
            if (abs(dx) >= abs(dy) and ny < 0) or (abs(dx) < abs(dy) and nx < 0):
                nx, ny = -nx, -ny
            off = 30 + 18 * k
            cx, cy = mx + nx * off, my + ny * off
            body.append(f'<path d="M{sx:.1f},{sy:.1f} Q{cx:.1f},{cy:.1f} {ex:.1f},{ey:.1f}" stroke="{MUTED}" fill="none" stroke-width="1.3" marker-end="url(#arr)"/>')
            lx, ly = (sx + 2 * cx + ex) / 4 + nx * 8, (sy + 2 * cy + ey) / 4 + ny * 8 + 4
        else:
            body.append(f'<path d="M{sx:.1f},{sy:.1f} L{ex:.1f},{ey:.1f}" stroke="{MUTED}" fill="none" stroke-width="1.3" marker-end="url(#arr)"/>')
            lx, ly = (sx + ex) / 2, (sy + ey) / 2 - 6
        tw = text_width(label, 10) + 8
        body.append(_box(lx - tw / 2, ly - 11, tw, 15, "#fbfdfd", "none", 3))
        body.append(_text(lx, ly, label, 10, TEAL, "600"))
    terminal = set(sm.get("terminal", []))
    for s, (cx, cy) in center.items():
        x, y = cx - nw / 2, cy - nh / 2
        is_init = s == sm.get("initial")
        fill = "#123248" if is_init else ("#f3f6f7" if s in terminal else "#ffffff")
        ink = "#ffffff" if is_init else NAVY
        body.append(_box(x, y, nw, nh, fill, "#8fa9b2", 10))
        if s in terminal:
            body.append(_box(x + 3, y + 3, nw - 6, nh - 6, "none", "#8fa9b2", 8))
        body.append(_text(cx, cy + 4, s, 12.5, ink, "600"))
    return _svg(W, H, "".join(body), f"状态机 {sm['id']} · {sm['object']}")


def es_timeline(scenario: dict, per_row: int = 5) -> str:
    steps = scenario["steps"]
    cw, gap = 196, 14
    rows = math.ceil(len(steps) / per_row)
    row_h = 318
    W = 30 + per_row * (cw + gap)
    H = 20 + rows * row_h
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    colors = {"actor": ("#fff4c2", "#8a6d1b"), "command": ("#d6e8fb", "#1d4f86"), "aggregate": ("#fffbe6", "#6b5a1e"), "event": ("#ffe1c7", "#8a4513"), "hotspot": ("#fbdada", "#9b2c2c")}
    for idx, st in enumerate(steps):
        r, c = divmod(idx, per_row)
        x = 18 + c * (cw + gap)
        y = 14 + r * row_h
        body.append(_text(x + 4, y + 14, f"{st['n']:02d}", 12, TEAL, "800", "start"))
        yy = y + 22
        for key, h in (("actor", 30), ("command", 62), ("aggregate", 44), ("event", 62), ("hotspot", 50)):
            val = st.get(key) or ""
            if key == "hotspot" and not val:
                continue
            fill, ink = colors[key]
            body.append(_box(x, yy, cw, h, fill, "none", 4))
            lines = wrap(val, 11, cw - 14)[: (h - 8) // 14]
            for li, ln in enumerate(lines):
                body.append(_text(x + 7, yy + 16 + li * 14, ln, 11, ink, "600" if key in ("command", "event") else "400", "start"))
            yy += h + 6
    return _svg(W, H, "".join(body), f"事件风暴时间线 {scenario['id']}")


def runtime_layers() -> str:
    W, H = 1100, 520
    bands = [
        ("cmd/", "brant-api · brant-worker · brant-migrate · brant-cli", "#123248", "#ffffff"),
        ("internal/bootstrap", "组合根：配置、连接池、每个上下文独立 bentopg.DB、模块装配", "#1f4a63", "#ffffff"),
        ("internal/transport · internal/ai · internal/processes", "HTTP 入站 · AI 能力层（非上下文）· 无单一 Owner 的流程（按需）", "#2d6178", "#ffffff"),
        ("internal/contexts/<bc>", "18 个候选上下文：module.go ＋ api/ ＋ internal/{domain, application, adapters, integration}", "#0b7b7c", "#ffffff"),
        ("internal/kernel · internal/reference", "共享值对象（Money、Quantity、Ratio、Scope…）· 外部机构代码表（只读）", "#e8eff3", NAVY),
        ("internal/foundation", "bento · identity · authorization · database · messaging · configuration · telemetry · interop", "#f3eadb", "#5b4520"),
        ("外部 Module 与系统", "idp-bento-go RC2 · idp-platform-go · pgx/v5 · PostgreSQL 16 · OIDC · EER／BIP", "#eef1f3", MUTED),
    ]
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    bh, gap = 58, 12
    for i, (name, desc, fill, ink) in enumerate(bands):
        y = 16 + i * (bh + gap)
        body.append(_box(40, y, W - 80, bh, fill, "#b4c7cd" if fill.startswith(("#e", "#f")) else fill, 10))
        body.append(_text(64, y + 24, name, 14, ink, "700", "start"))
        body.append(_text(64, y + 44, desc, 12, ink, "400", "start"))
        if i < len(bands) - 1:
            body.append(f'<path d="M{W - 70},{y + bh} L{W - 70},{y + bh + gap - 1}" stroke="{MUTED}" stroke-width="1.6" marker-end="url(#arr)"/>')
    return _svg(W, H, "".join(body), "运行时分层与允许的依赖方向")


def wbs_roadmap(phases: list[dict], tasks: list[dict]) -> str:
    W, H = 1160, 250
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    n = len(phases)
    bw = (W - 60 - (n - 1) * 14) / n
    counts = {p["id"]: sum(1 for t in tasks if t["phase"] == p["id"]) for p in phases}
    for i, p in enumerate(phases):
        x = 30 + i * (bw + 14)
        fill = "#123248" if p["id"] == "M0" else ("#0b7b7c" if p["id"] in ("S1", "S2", "S3") else ("#2d6178" if p["id"] == "S4" else "#e8eff3"))
        ink = "#ffffff" if fill != "#e8eff3" else NAVY
        body.append(_box(x, 30, bw, 150, fill, "#b4c7cd" if fill == "#e8eff3" else fill, 10))
        body.append(_text(x + bw / 2, 58, p["id"], 20, ink, "800"))
        for li, ln in enumerate(wrap(p["name"], 12, bw - 16)[:2]):
            body.append(_text(x + bw / 2, 82 + li * 16, ln, 12, ink, "600"))
        body.append(_text(x + bw / 2, 132, f"{counts[p['id']]} 项工作", 12, ink))
        for li, ln in enumerate(wrap(p.get("pythonParity") or "—", 10, bw - 16)[:2]):
            body.append(_text(x + bw / 2, 152 + li * 13, ln, 10, ink))
        if i < n - 1:
            body.append(f'<path d="M{x + bw:.0f},105 L{x + bw + 12:.0f},105" stroke="{MUTED}" stroke-width="1.6" marker-end="url(#arr)"/>')
    body.append(_text(W / 2, 214, "顺序表示依赖与准入，不是已批准的排期；S1–S3 以 Python V0.50 行为等价为退出条件，S4 满足 P0 退出前提", 12, MUTED))
    return _svg(W, H, "".join(body), "Go 实施阶段路线")
