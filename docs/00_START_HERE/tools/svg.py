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


def r1_deployment_topology() -> str:
    """R1 modular-monolith deployment topology, process isolation and external boundaries."""
    W, H = 1140, 628
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Band 1: Ingress, OIDC, Ops
    top_boxes = [
        (30, 20, 330, 78, "#ffffff", "#8fa9b2", "业务客户端 ／ 前端工作台（外部）",
         ["本仓库仅交付后端 API 与 OpenAPI 3.1 契约", "人工操作走 HTTPS ＋ Bearer JWT（at+jwt）"]),
        (390, 20, 350, 78, "#f3eadb", "#c9b38f", "外部 IAM ／ OIDC 提供方（ACL）",
         ["签发 RS256 at+jwt；验签 iss ／ aud ／ JWKS", "真实企业 IdP 接入延后至 B0；CI 用本地签发者"]),
        (770, 20, 340, 78, "#e8eff3", "#9fb7bf", "运维与迁移入口（仅内网 ／ 回环）",
         ["GET /metrics（Prometheus 0.0.4，无鉴权，禁公网）", "brant-admin（授权投影）· brant-migrate（DDL）"]),
    ]
    for x, y, w, h, fill, stroke, title, lines in top_boxes:
        body.append(_box(x, y, w, h, fill, stroke, 8))
        body.append(_text(x + 14, y + 24, title, 12.5, NAVY, "700", "start"))
        for idx, ln in enumerate(lines):
            body.append(_text(x + 14, y + 46 + idx * 18, ln, 11, MUTED if fill == "#ffffff" else "#38566a", "400", "start"))

    # Band 2 Left: Brant runtime processes (modular monolith)
    body.append(_box(30, 128, 710, 246, "#f4f9f8", TEAL, 10, 'stroke-dasharray="6,4" stroke-width="1.4"'))
    body.append(_text(46, 150, "IDP Brant 模块化单体进程组（非微服务拆分 · 数据库统一使用非特权角色 brant_app）", 12, TEAL, "700", "start"))

    procs = [
        (46, 164, 214, 196, NAVY, "brant-api", "HTTP ／ OpenAPI 3.1 入口", [
            "• 552 路径（429 POST ／ 123 GET）",
            "• F2：ai 令牌读体前拒写 403",
            "• strictjson 1／15／32 MiB 硬限",
            "• 18 上下文组合根装配",
            "• CRM 可选（-disable-crm）",
            "• STK／MFG 按公司默认关闭",
        ]),
        (278, 164, 214, 196, TEAL, "brant-worker -dispatch", "Outbox 中继与 Inbox 分发", [
            "• 参考部署：2–3 个进程",
            "• 分区租约并发消费 Outbox",
            "• 8 个系统消费方（事件 ID 幂等）",
            "• 滞后告警 60s ／ 事故 300s",
            "• 死信独立告警（人工重放）",
            "• 实测 3w P95 6.403s（目标≤5s）",
        ]),
        (510, 164, 214, 196, "#6b4fa0", "brant-worker -agent", "AI 受控执行运行器（可选）", [
            "• 系统操作者 system:ai.runner",
            "• 进程内 AI Gateway 能力校验",
            "• 委托 ＋ 内容绑定批准授权",
            "• 预算：40 调用 ／ 3 重试 ／ $3",
            "• 受限抽取 Q01.upload.v0.1",
            "• ModelPort（openaicompat）",
        ]),
    ]
    for x, y, w, h, hfill, title, sub, items in procs:
        body.append(_box(x, y, w, h, "#ffffff", "#b8cdd2", 8))
        body.append(_box(x, y, w, 46, hfill, hfill, 8))
        body.append(f'<rect x="{x}" y="{y + 38}" width="{w}" height="8" fill="{hfill}"/>')
        body.append(_text(x + w / 2, y + 20, title, 12.5, "#ffffff", "700"))
        body.append(_text(x + w / 2, y + 37, sub, 10.2, "#dceef0", "400"))
        for idx, ln in enumerate(items):
            body.append(_text(x + 11, y + 66 + idx * 21, ln, 10.6, NAVY, "400", "start"))

    # Band 2 Right: External boundaries
    body.append(_box(770, 128, 340, 246, "#ffffff", "#b8cdd2", 10))
    body.append(_text(786, 150, "外部依赖与专业系统边界（ACL）", 12, NAVY, "700", "start"))
    exts = [
        (786, 164, 308, 46, "#edf5f5", "#9bc2c2", "模型服务（OpenAI 兼容 HTTP）", "仅输出候选提案；密钥仅取 IDP_BRANT_MODEL_API_KEY"),
        (786, 218, 308, 46, "#eef1fa", "#bac5e6", "EER 执行运行时（可选 ACL · S4-05 PARTIAL）", "手写客户端；本机冒烟通过，Brant 步骤未登记"),
        (786, 272, 308, 46, "#fff8ed", "#dfc7a1", "中国专业渠道 ／ 外部 ERP（人工＋假适配器）", "单一窗口／金税／银行／法定总账：零真实外拨"),
        (786, 326, 308, 34, "#fbeceb", "#dfa6a0", "BIP（已永久退出 · 2026-11-23）", "仅留未连接占位，全部外发走人工发送节点"),
    ]
    for x, y, w, h, fill, stroke, title, desc in exts:
        body.append(_box(x, y, w, h, fill, stroke, 7))
        body.append(_text(x + 11, y + 17, title, 11, "#9b3a30" if fill == "#fbeceb" else NAVY, "700", "start"))
        body.append(_text(x + 11, y + 32 if h < 40 else y + 36, desc, 10, "#7c332b" if fill == "#fbeceb" else MUTED, "400", "start"))

    # Band 3: PostgreSQL 16.12
    body.append(_box(30, 410, 1080, 198, "#ffffff", NAVY, 10, 'stroke-width="1.6"'))
    body.append(_box(30, 410, 1080, 36, NAVY, NAVY, 10))
    body.append(f'<rect x="30" y="436" width="1080" height="10" fill="{NAVY}"/>')
    body.append(_text(48, 433, "PostgreSQL 16.12 · 22 个 Brant Schema ＋ bento ＋ brant_migrations · 106 个迁移 · 282 张 FORCE RLS 表（app.tenant_id / app.company_id）", 12, "#ffffff", "700", "start"))

    pg_blocks = [
        (48, 460, 248, 88, "#f3eadb", "#cfb997", "平台与治理 Schema", [
            "brant_auth（权限投影 / epoch）",
            "brant_msg（Outbox 死信）",
            "brant_ref（名单）· bento（Outbox/Inbox）",
        ]),
        (312, 460, 268, 88, "#edf5f5", "#9dc2c2", "核心业务 Schema（10）", [
            "evd · pty · cat · pol · inq",
            "sup · cst · trd · pur · stl",
            "每上下文独立 bentopg.DB，禁跨库嵌套事务",
        ]),
        (596, 460, 252, 88, "#e8eff3", "#a2bac3", "履约、协同与条件 Schema（7）", [
            "qlt · ful · cnx · clm",
            "stk（条件）· mfg（条件）· crm（可选）",
            "写本根 FOR UPDATE → 上游守卫 FOR SHARE",
        ]),
        (864, 460, 228, 88, "#eef1fa", "#bcc6e3", "任务与 AI 证据 Schema（2）", [
            "tsk（任务 / 动作 / 目标核验）",
            "brant_ai（委托 / 批准 / 调用账本）",
            "账本仅记哈希与费用，不存密钥",
        ]),
    ]
    for x, y, w, h, fill, stroke, title, lines in pg_blocks:
        body.append(_box(x, y, w, h, fill, stroke, 7))
        body.append(_text(x + 12, y + 22, title, 11.5, NAVY, "700", "start"))
        for idx, ln in enumerate(lines):
            body.append(_text(x + 12, y + 42 + idx * 17, ln, 10.2, "#38566a", "400", "start"))

    body.append(_box(48, 560, 1044, 34, "#f4f8f8", "#c9d8dc", 6))
    body.append(_text(570, 581, "账号硬分离：brant_migrator（仅 brant-migrate 执行 DDL） ≠ brant_app（NOSUPERUSER · NOBYPASSRLS · NOINHERIT） ｜ 连续 WAL 归档（RPO ≤15m · RTO ≤4h 目标，仅 WSL2 本机演练）", 11, NAVY, "600"))

    # Connectors
    arrows = [
        # Client -> brant-api
        (153, 98, 153, 162, TEAL),
        # OIDC -> brant-api (routed cleanly at y=112)
        # Ops -> PG16 / worker
    ]
    for x1, y1, x2, y2, col in arrows:
        m = "url(#arrT)" if col == TEAL else "url(#arr)"
        body.append(f'<path d="M{x1},{y1} L{x2},{y2}" stroke="{col}" stroke-width="1.6" fill="none" marker-end="{m}"/>')
    # OIDC -> brant-api elbow
    body.append(f'<path d="M460,98 L460,113 L190,113 L190,162" stroke="{MUTED}" stroke-width="1.4" fill="none" marker-end="url(#arr)"/>')
    # Ops -> dispatch metrics
    body.append(f'<path d="M840,98 L840,113 L385,113 L385,162" stroke="{MUTED}" stroke-width="1.4" stroke-dasharray="4,3" fill="none" marker-end="url(#arr)"/>')
    # Processes -> PG16
    for px in (153, 385, 617):
        body.append(f'<path d="M{px},360 L{px},408" stroke="{TEAL}" stroke-width="1.7" fill="none" marker-end="url(#arrT)"/>')
    # agent -> Model / EER
    body.append(f'<path d="M724,187 L784,187" stroke="{MUTED}" stroke-width="1.5" fill="none" marker-end="url(#arr)"/>')
    body.append(f'<path d="M724,241 L784,241" stroke="{MUTED}" stroke-width="1.5" stroke-dasharray="4,3" fill="none" marker-end="url(#arr)"/>')
    # Ops -> PG16 right edge (migrate / admin)
    body.append(f'<path d="M1078,98 L1078,118 L1122,118 L1122,428 L1112,428" stroke="{MUTED}" stroke-width="1.4" fill="none" marker-end="url(#arr)"/>')

    return _svg(W, H, "".join(body), "R1 部署拓扑与运行时隔离边界")


def r1_service_boundaries() -> str:
    """Four trust and service boundary layers of Brant R1."""
    W, H = 1140, 514
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]
    zones = [
        (18, 92, "#fdf1f0", "#e6b8b2", "#9b3a30", "ZONE 0 · 不可信输入与外部权威区（请求体禁自报 scope/actor/grant；模型文本非真值）", [
            ("人工 HTTP 请求", "携带 OIDC Bearer 令牌（at+jwt）；幂等键防重"),
            ("AI 模型回复与工具调用", "ModelPort 返回候选与参数；不可直接写业务库"),
            ("外部系统回执与名单", "报关／退税／银行回执与制裁名单；属外部权威"),
        ]),
        (134, 96, "#fff8ed", "#e3c9a0", "#9a622f", "ZONE 1 · 传输与身份硬边界（internal/transport/httpapi ＋ identity/oidc ＋ strictjson）", [
            ("OIDC 验签与 Principal", "校验 iss／aud／JWKS；拒 alg:none；提取 tenant/company"),
            ("F2 写路由硬拦截", "所有写 HTTP 路由在读请求体前拒绝 actor.kind=ai（403）"),
            ("strictjson 严格解码", "1／15／32 MiB；深度 64；拒重复键、null、未知字段"),
        ]),
        (254, 112, "#edf5f5", "#9bc2c2", TEAL, "ZONE 2 · 应用授权、单写者权威与并发锁序边界（18 上下文 · 82 条白名单 · 26 条无环守卫锁）", [
            ("RBAC · SoD · 人工专属", "16 岗位模板 · 7 禁止组合 · 10 专属 · 128 人工专属 · authority_epoch"),
            ("单一写入方与锁序", "53 条事实权威；先锁本根 FOR UPDATE，再锁上游守卫 FOR SHARE"),
            ("进程内 AI 能力网关", "校验 ExecutionBundle SHA-256、委托、内容绑定批准授权与预算"),
        ]),
        (390, 104, "#e8eff3", "#8fa9b2", NAVY, "ZONE 3 · 持久隔离、事件异步边界与权威核验闭环（PostgreSQL 16 ＋ Outbox／Inbox ＋ TSK）", [
            ("FORCE RLS 与角色分离", "282 张表强制按 tenant/company 隔离；brant_app 无特权"),
            ("原子 Outbox 与 8 消费方", "137 集成事件同事务落库；事件 ID 幂等；死信人工重放"),
            ("权威记录完成判定", "TSK 仅依据权威上下文记录核验目标，绝不采信模型自述"),
        ]),
    ]
    cw, gap = 338, 18
    for zy, zh, zfill, zstroke, zink, ztitle, cards in zones:
        body.append(_box(28, zy, W - 56, zh, zfill, zstroke, 10))
        body.append(_text(44, zy + 22, ztitle, 11.8, zink, "700", "start"))
        for idx, (ctitle, cdesc) in enumerate(cards):
            cx = 44 + idx * (cw + gap)
            cy = zy + 33
            ch = zh - 44
            body.append(_box(cx, cy, cw, ch, "#ffffff", zstroke, 7))
            body.append(_text(cx + 12, cy + 20, ctitle, 11.8, NAVY, "700", "start"))
            for li, ln in enumerate(wrap(cdesc, 10.5, cw - 24)[:2]):
                body.append(_text(cx + 12, cy + 39 + li * 16, ln, 10.5, MUTED, "400", "start"))
        if zy < 390:
            ay = zy + zh
            for idx in range(3):
                ax = 44 + idx * (cw + gap) + cw / 2
                body.append(f'<path d="M{ax:.0f},{ay} L{ax:.0f},{ay + 22}" stroke="{MUTED}" stroke-width="1.5" fill="none" marker-end="url(#arr)"/>')
    return _svg(W, H, "".join(body), "R1 授权、信任与服务边界分层")


def pm_trade_panorama() -> str:
    """Panoramic foreign trade lifecycle and 18 bounded contexts capability map."""
    W, H = 1140, 620
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Top AI & Task Governance Rail
    body.append(_box(28, 18, W - 56, 76, NAVY, NAVY, 10))
    body.append(_text(46, 42, "AI-Native 受控任务协同层（TSK 业务任务上下文 ＋ internal/ai 能力层 · 不拥有交易事实）", 13, "#ffffff", "700", "start"))
    pills = [
        (46, 54, 240, "13 状态任务周期 ⊥ 人工接管"),
        (300, 54, 250, "Q01 询价→报价→订单九步主链"),
        (564, 54, 256, "F2 写路由硬拦截 ＋ 128 人工专属"),
        (834, 54, 260, "权威数据库核验 · 不采信模型自述"),
    ]
    for px, py, pw, plabel in pills:
        body.append(_box(px, py, pw, 28, "#1e4760", "#4b748a", 6))
        body.append(_text(px + pw / 2, py + 18, plabel, 10.8, "#e6f3f5", "600"))

    # Five operational columns across the value chain
    cols = [
        ("① 商机与客户需求", "#2d6178", [
            ("CRM", "Optional", "轻量 CRM（可选）", "线索 · 商机 · 跟进 · 可关"),
            ("EVD", "Generic", "来源证据与文件", "四层身份 · 附件留证审计"),
            ("INQ", "Supporting", "客户询价", "草稿定位 · 人工核实 ready"),
        ], "产出：核实需求 ＋ 原文坐标"),
        ("② 寻源、核价与报价", TEAL, [
            ("SUP", "SupportingPlus", "供应寻源与条件", "供应商报价 · MOQ ／ 阶梯价"),
            ("CST", "Core", "确定性核价", "场景评估 · 冻结快照 · 守卫"),
            ("TRD", "Core", "报价与独立审批", "冻结 · 毛利例外 · 正式化"),
        ], "产出：商业哈希正式报价"),
        ("③ 签约与外销订单", "#0b7b7c", [
            ("TRD", "Core", "客户接受与合同", "核实回复 · 到达规则成立"),
            ("TRD", "Core", "外销订单生效", "full_v1 唯一 ／ multi_order"),
            ("TRD", "Core", "订单变更与上限", "独立审批 · 报价剩余上限"),
        ], "产出：信用＋合规生效订单"),
        ("④ 供应、质检与出运", "#1f4a63", [
            ("PUR", "Core", "采购与需求分配", "独立承诺 · 直发交付核实"),
            ("STK/MFG", "Supporting", "库存与委外（条件）", "FIFO 成本层 · 委外上限交接"),
            ("QLT+FUL", "Supporting", "质检与履约出运", "五重商业放行 · 双语单证"),
        ], "产出：可溯出运与出口批次"),
        ("⑤ 结算、中国与售后", "#6b4fa0", [
            ("STL", "Core", "业务结算与经营", "信用 · 商业核销 · 净利结果"),
            ("CNX", "Supporting", "中国出口协同", "发票 · 报关／退税 · 许可证"),
            ("CLM", "Supporting", "售后与索赔", "退货留痕 · 单次贷项 · 关门"),
        ], "产出：三线核对与净利闭环"),
    ]
    cw, gap = 202, 18
    top_y, col_h = 114, 336
    for idx, (ctitle, hcol, cards, foot) in enumerate(cols):
        cx = 28 + idx * (cw + gap)
        body.append(_box(cx, top_y, cw, col_h, "#ffffff", "#c2d4d9", 9))
        body.append(_box(cx, top_y, cw, 38, hcol, hcol, 9))
        body.append(f'<rect x="{cx}" y="{top_y + 28}" width="{cw}" height="10" fill="{hcol}"/>')
        body.append(_text(cx + cw / 2, top_y + 24, ctitle, 12, "#ffffff", "700"))
        for ci, (code_lbl, tier, name, desc) in enumerate(cards):
            by = top_y + 48 + ci * 78
            bfill, bink = TIER_FILL.get(tier, ("#e8eff3", NAVY))
            body.append(_box(cx + 10, by, cw - 20, 68, "#f7fbfb", "#d6e4e7", 7))
            bx_w = 58 if ("/" in code_lbl or "+" in code_lbl) else 44
            body.append(_box(cx + 16, by + 9, bx_w, 20, bfill, "none", 4))
            body.append(_text(cx + 16 + bx_w / 2, by + 23, code_lbl, 9.4, bink, "700"))
            body.append(_text(cx + 22 + bx_w, by + 23, name, 10.2, NAVY, "700", "start"))
            body.append(_text(cx + 16, by + 48, desc, 10.0, MUTED, "400", "start"))
        body.append(_box(cx + 10, top_y + col_h - 42, cw - 20, 32, "#edf5f5", "#b7d4d4", 6))
        body.append(_text(cx + cw / 2, top_y + col_h - 22, foot, 10.2, TEAL, "700"))
        if idx < len(cols) - 1:
            ax1 = cx + cw
            ax2 = cx + cw + gap - 2
            ay = top_y + col_h / 2 - 10
            body.append(f'<path d="M{ax1:.0f},{ay:.0f} L{ax2:.0f},{ay:.0f}" stroke="{TEAL}" stroke-width="1.8" fill="none" marker-end="url(#arrT)"/>')

    # Down arrows from AI layer to columns 1-3 (Q01 scope)
    for idx in (0, 1, 2):
        ax = 28 + idx * (cw + gap) + cw / 2
        body.append(f'<path d="M{ax:.0f},94 L{ax:.0f},112" stroke="{TEAL}" stroke-width="1.4" stroke-dasharray="4,3" fill="none" marker-end="url(#arrT)"/>')

    # Feedback rail (Event-driven closed loop)
    body.append(_box(28, 464, W - 56, 30, "#fff8ed", "#e3c9a0", 6))
    body.append(_text(W / 2, 483, "异步事件驱动闭环（Transactional Outbox ＋ 8 系统消费方）：来源否认 → 风险冻结 ｜ 订单变更 → 采购／出运／合规／结算自动复核 ｜ 未结索赔 → 阻断经营结果关闭", 10.8, "#8c5c1f", "600"))

    # Bottom Shared Master Data & Foundation Rail
    bases = [
        ("PTY · 交易主体与筛查", "客户／供应商／地址角色", "PartyScreeningGuard v1"),
        ("CAT · 商品与规格修订", "款色码 SKU ／ 货号映射", "SkuGuard v1 失效阻断"),
        ("POL · 23 种商业政策", "汇率／税价／毛利／模板", "ConfigGuard v1 版本守卫"),
        ("Foundation & PG16", "kernel.Money 精确算术", "282 表 FORCE RLS · Outbox"),
    ]
    bw, bgap = 257, 18
    for idx, (bt, b1, b2) in enumerate(bases):
        bx = 28 + idx * (bw + bgap)
        body.append(_box(bx, 508, bw, 92, "#e8eff3" if idx < 3 else "#f3eadb", "#a7bcc5" if idx < 3 else "#d1bc9a", 8))
        body.append(_text(bx + 14, 533, bt, 12, NAVY, "700", "start"))
        body.append(_text(bx + 14, 557, b1, 10.8, "#38566a", "400", "start"))
        body.append(_text(bx + 14, 580, b2, 10.6, TEAL if idx < 3 else "#7a5321", "600", "start"))

    return _svg(W, H, "".join(body), "IDP Brant 外贸全链路业务全景与 18 个能力域地图")


def pm_brc001_economics() -> str:
    """End-to-end verified BRC-001 financial, quantity and rounding conservation diagram."""
    W, H = 1140, 526
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Top input strip: 4 SKU lines -> 120 PCS / 960.00 USD
    body.append(_box(28, 18, W - 56, 66, NAVY, NAVY, 9))
    body.append(_text(46, 40, "合成核算基准 BRC-001（PG16 端到端 HTTP 实测逐分验证）", 12.5, "#ffffff", "700", "start"))
    sku_pills = [
        (46, 49, 178, "ST01.red.S · 24 PCS"),
        (234, 49, 182, "ST01.red.M · 36 PCS"),
        (426, 49, 182, "ST01.blue.S · 12 PCS"),
        (618, 49, 184, "ST01.blue.M · 48 PCS"),
    ]
    for sx, sy, sw, sl in sku_pills:
        body.append(_box(sx, sy, sw, 26, "#1f4a63", "#4e778e", 5))
        body.append(_text(sx + sw / 2, sy + 17, sl, 10.8, "#e6f3f5", "600"))
    body.append(_box(818, 30, 278, 44, TEAL, "#53b8b9", 7))
    body.append(_text(957, 57, "120 PCS × 8.00 = 960.00 USD", 13.5, "#ffffff", "700"))

    # Middle Left: Exact Revenue -> Cost -> Margin Waterfall
    body.append(_box(28, 102, 534, 294, "#ffffff", "#c2d4d9", 9))
    body.append(_text(46, 128, "收入 → 冻结成本 → 订单净毛利瀑布（整数最小单位 · 禁用 float）", 12.5, NAVY, "700", "start"))
    bars = [
        ("净销售收入（TRD 报价／订单）", 960.0, 0.0, 960.0, TEAL, "960.00 USD (100.00%)"),
        ("采购成本（120×5.00 · PUR）", 600.0, 0.0, 600.0, "#1f4a63", "600.00 USD (62.50%)"),
        ("物流与费用（分摊 · CST）", 60.0, 600.0, 60.0, "#4b6b82", "60.00 USD (6.25%)"),
        ("冻结成本快照合计（CST）", 660.0, 0.0, 660.0, "#ba743e", "660.00 USD (68.75%)"),
        ("订单经营毛利（STL 结果）", 300.0, 660.0, 300.0, "#2f6e52", "300.00 USD (31.25%)"),
    ]
    bx0, max_w = 228, 310
    for idx, (label, _val, start_v, span_v, col, txt) in enumerate(bars):
        ry = 148 + idx * 41
        body.append(_text(46, ry + 18, label, 10.8, NAVY, "600", "start"))
        # track
        body.append(_box(bx0, ry + 2, max_w, 24, "#f0f5f6", "#dce6e8", 4))
        x_bar = bx0 + (start_v / 960.0) * max_w
        w_bar = max(18, (span_v / 960.0) * max_w)
        body.append(_box(x_bar, ry + 2, w_bar, 24, col, "none", 4))
        # label placed neatly inside or right of bar
        if w_bar >= 145:
            body.append(_text(x_bar + w_bar / 2, ry + 18, txt, 10, "#ffffff", "700"))
        else:
            tx = x_bar + w_bar + 6 if (x_bar + w_bar + 135) < (bx0 + max_w) else bx0 + 8
            tcol = NAVY
            body.append(_text(tx, ry + 18, txt, 9.8, tcol, "700", "start"))
    # Margin floor callout
    body.append(_box(46, 354, 498, 28, "#edf6f3", "#b5d7ca", 5))
    body.append(_text(295, 372, "毛利红线：成本 660.00 @ 最低毛利率 25% → 精确下限 880.00（879.99 须毛利例外审批）", 10.2, "#2f6e52", "600"))

    # Middle Right: Payment Schedule, Dual Routes & Partial Orders
    body.append(_box(580, 102, 532, 294, "#ffffff", "#c2d4d9", 9))
    body.append(_text(598, 128, "资金分期守恒、双履约成本路线与 S6 上限／结案约束", 12.5, NAVY, "700", "start"))

    r_cards = [
        (144, 72, "#edf5f5", "#9bc2c2", "付款计划最大余数法守恒（30% 预付 ／ 70% 赊销尾款）",
         "预付 288.00 USD（出运前核销）＋ 尾款 672.00 USD（触发 STL 信用占用）= 960.00 USD",
         "付款计划不冒充应收或到账；新客户默认信用额度 0，超额须非销售信用岗位放行"),
        (226, 76, "#e8eff3", "#a2bac3", "双履约成本路线（直发默认 vs 本仓 FIFO）",
         "• 直发路线（S01）：受 SupplierDeliveryGuard v2 约束，绝不虚构库存移动",
         "• 本仓路线（开 STK）：FIFO 成本层出库 ＋ StockAvailability v1 并发锁，STL 按层计价"),
        (312, 70, "#fff8ed", "#dfc7a1", "部分多次下单与索赔关门（S6 实测）",
         "multi_order 1,200 拆 700＋500 后再增 1 → 拒 QUOTE_REMAINING_INSUFFICIENT",
         "未结索赔使经营结果索赔维不满足，order_result.close 被拒；结案后方可关闭"),
    ]
    for cy, ch, fill, stroke, ct, l1, l2 in r_cards:
        body.append(_box(598, cy, 496, ch, fill, stroke, 7))
        body.append(_text(612, cy + 20, ct, 11.2, NAVY, "700", "start"))
        body.append(_text(612, cy + 40, l1, 10.2, "#38566a", "400", "start"))
        body.append(_text(612, cy + 57, l2, 10.2, MUTED, "400", "start"))

    # Bottom 4 arithmetic invariants verified against Python V0.50
    inv_cards = [
        ("逐行先舍后加", "2 行各 1 × 0.005 → 1＋1 = 2 分", "若先汇总再舍仅得 1 分（违约）"),
        ("含税反算应付守恒", "960.00 含 13% 税 → 净 849.56", "税 110.44；净＋税 = 960.00 精确守恒"),
        ("汇率计算单次终舍", "100 USD × 7.123456 CNY/USD", "精确有理数运算 → 终舍 712.35 CNY"),
        ("MOQ 与双阶梯计价", "120 PCS: [0,100)@6, [100,∞)@5", "分段 700.00 ≠ 全量 600.00（差 100）"),
    ]
    iw, igap = 257, 18
    for idx, (it, i1, i2) in enumerate(inv_cards):
        ix = 28 + idx * (iw + igap)
        body.append(_box(ix, 414, iw, 92, "#f4f8f8", "#c2d4d9", 8))
        body.append(_text(ix + 14, 438, it, 11.8, TEAL, "700", "start"))
        body.append(_text(ix + 14, 462, i1, 10.5, NAVY, "600", "start"))
        body.append(_text(ix + 14, 484, i2, 10.2, MUTED, "400", "start"))

    return _svg(W, H, "".join(body), "合成核算基准 BRC-001 全链路金额与数量守恒透视图")


def pm_ai_q01_loop() -> str:
    """AI-Native Q01 9-stage human-AI collaborative loop with 3 hard safety barriers."""
    W, H = 1140, 560
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Top Swimlane: AI Execution Layer
    body.append(_box(28, 18, W - 56, 174, "#f4f6fc", "#b8c3e0", 10))
    body.append(_text(46, 42, "AI 执行层（brant-worker -agent ＋ ModelPort · 仅生成候选与受控提案 · 绝不直接写业务库）", 12.2, "#4a3875", "700", "start"))
    body.append(_box(846, 25, 248, 26, "#6b4fa0", "none", 6))
    body.append(_text(970, 42, "预算硬限：40 调用 ／ 3 重试 ／ $3.00", 10.5, "#ffffff", "700"))

    ai_steps = [
        ("① 受限抽取", "Q01.upload.v0.1", "Text ／ PDF ／ XLSX 纯解析", "超限整文件拒绝 · 绝不截断"),
        ("② 询价草稿", "rfq.save_draft", "提取候选行 ＋ 原文坐标定位", "候选非事实 · 等人工核实"),
        ("③ 报价草稿", "quote.save_draft", "引用冻结成本快照编制报价", "缺快照网关直接拒 422"),
        ("④ 订单提案", "proposal agp.*", "生成受控动作与载荷哈希", "等待人工审阅签发授权"),
        ("⑤ 授权提交", "order.prepare", "持一次性批准授权写入", "并发幂等 · 宕机可对账"),
    ]
    sw, sgap = 196, 17
    for idx, (st, sc, l1, l2) in enumerate(ai_steps):
        sx = 46 + idx * (sw + sgap)
        body.append(_box(sx, 60, sw, 118, "#ffffff", "#b8c3e0", 8))
        body.append(_box(sx, 60, sw, 30, "#6b4fa0", "#6b4fa0", 8))
        body.append(f'<rect x="{sx}" y="{80}" width="{sw}" height="10" fill="#6b4fa0"/>')
        body.append(_text(sx + sw / 2, 80, st, 11.5, "#ffffff", "700"))
        body.append(_text(sx + sw / 2, 110, sc, 10.5, "#4a3875", "700"))
        body.append(_text(sx + sw / 2, 134, l1, 10.2, NAVY, "400"))
        body.append(_text(sx + sw / 2, 156, l2, 9.8, MUTED, "400"))
        if idx < len(ai_steps) - 1:
            body.append(f'<path d="M{sx + sw},{119} L{sx + sw + sgap - 2},{119}" stroke="#6b4fa0" stroke-width="1.5" fill="none" marker-end="url(#arr)"/>')

    # Middle Hard Safety Barrier
    body.append(_box(28, 210, W - 56, 86, "#fdf1f0", "#d98d85", 10, 'stroke-width="1.6"'))
    body.append(_text(46, 232, "三道不可绕过的确定性安全防线（代码硬约束 · 自动化回归与现场对抗实测）", 12, "#9b3a30", "700", "start"))
    guards = [
        ("防线 1 · F2 传输层硬拦截", "所有写 HTTP 路由在读请求体前拒绝 actor.kind=ai（403）"),
        ("防线 2 · 进程内 AI 能力网关", "强校验 ExecutionBundle 哈希、有效委托、128 人工专属与预算"),
        ("防线 3 · 内容绑定批准授权", "agp.* 绑定对象＋修订＋规范化载荷 SHA-256；改一字节即拒"),
    ]
    gw, ggap = 338, 17
    for idx, (gt, gd) in enumerate(guards):
        gx = 46 + idx * (gw + ggap)
        body.append(_box(gx, 242, gw, 44, "#ffffff", "#e4b2ac", 6))
        body.append(_text(gx + 12, 260, gt, 10.8, "#9b3a30", "700", "start"))
        body.append(_text(gx + 12, 277, gd, 9.8, "#4b2c2a", "400", "start"))

    # Bottom Swimlane: Human Decisions & Authoritative System
    body.append(_box(28, 314, W - 56, 166, "#eef6f6", "#9dc2c2", 10))
    body.append(_text(46, 338, "人工决策与权威系统层（16 岗位职责分离 · 128 人工专属命令 · 权威账本落库）", 12.2, NAVY, "700", "start"))
    h_steps = [
        ("核实原文定位", "rfq.confirm_scope", "核对来源坐标与缺省字段", "BC-AT27 人工负责真值"),
        ("冻结与独立审批", "quote.freeze / activate", "编制与审核人严格分离", "低毛利须独立毛利审批"),
        ("核实客户接受", "quote.response.verify", "按到达时间判定合同成立", "无自动发送 · 人工登记回复"),
        ("审阅签发批准", "approval_grant.issue", "核对载荷哈希后一次放行", "撤委托即暂停（AN04 5/5）"),
        ("独立订单生效", "order.activate", "信用＋合规守卫全通过", "TSK 读权威记录验完成"),
    ]
    for idx, (ht, hc, l1, l2) in enumerate(h_steps):
        sx = 46 + idx * (sw + sgap)
        body.append(_box(sx, 354, sw, 112, "#ffffff", "#9dc2c2", 8))
        body.append(_box(sx, 354, sw, 28, TEAL, TEAL, 8))
        body.append(f'<rect x="{sx}" y="{372}" width="{sw}" height="10" fill="{TEAL}"/>')
        body.append(_text(sx + sw / 2, 372, ht, 11.2, "#ffffff", "700"))
        body.append(_text(sx + sw / 2, 400, hc, 10.2, TEAL, "700"))
        body.append(_text(sx + sw / 2, 424, l1, 10.2, NAVY, "400"))
        body.append(_text(sx + sw / 2, 446, l2, 9.8, MUTED, "400"))
        # Vertical cross-barrier arrows
        ax = sx + sw / 2
        body.append(f'<path d="M{ax:.0f},178 L{ax:.0f},208" stroke="{MUTED}" stroke-width="1.4" fill="none" marker-end="url(#arr)"/>')
        body.append(f'<path d="M{ax:.0f},296 L{ax:.0f},352" stroke="{TEAL}" stroke-width="1.4" fill="none" marker-end="url(#arrT)"/>')

    # Footer bar
    body.append(_box(28, 496, W - 56, 46, NAVY, NAVY, 8))
    body.append(_text(W / 2, 516, "任务双轨控制（TSK）：13 状态生命周期 ⊥ control_mode (ai ／ manual) ＋ stopping 接管屏障，人工接管不回滚已提交业务事实", 11.2, "#ffffff", "600"))
    body.append(_text(W / 2, 534, "完成判定唯一标准：quote_ready 与 order_active 仅由 TSK 读取权威上下文记录核验，模型自述无效（AN11 现场实测 5/5 通过）", 10.5, "#bdd5dd", "400"))

    return _svg(W, H, "".join(body), "AI-Native 主业务链 Q01 九步协同与三道安全防线")


def pm_release_gates() -> str:
    """Five simultaneous commercial release guards and segregation of duties (SoD)."""
    W, H = 1140, 496
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Top header
    body.append(_text(W / 2, 32, "出运与单证商业条件放行五重同步闸门（全部同时通过方可执行 shipment.confirm）", 13.5, NAVY, "700"))
    gates = [
        ("闸门 1 · 质量放行", "QLT", "#0b7b7c", "QualityRelease v1", "核验有效质检放行结论", "防超放校验已消费量 R79"),
        ("闸门 2 · 付款条件", "STL", "#1f4a63", "PaymentCond v1", "预付／信用额度满足合同", "例外仅限信用经理（禁销售）"),
        ("闸门 3 · 主体筛查", "PTY", "#2d6178", "PartyScreening v1", "主体最新筛查结论有效", "仅合规岗位裁决 · 禁过期放行"),
        ("闸门 4 · 贸易合规", "CNX", "#6b4fa0", "TradeCompliance", "当前修订 clear ＋ 许可证", "blocked 不可任何例外覆盖"),
        ("闸门 5 · 供给库存", "PUR/STK", "#ba743e", "Delivery / Stock", "直发核实供应商交付量", "本仓锁 FIFO 库存行防超卖"),
    ]
    gw, ggap = 202, 18
    for idx, (gt, gbc, col, guard, l1, l2) in enumerate(gates):
        gx = 28 + idx * (gw + ggap)
        body.append(_box(gx, 48, gw, 148, "#ffffff", "#c2d4d9", 8))
        body.append(_box(gx, 48, gw, 36, col, col, 8))
        body.append(f'<rect x="{gx}" y="74" width="{gw}" height="10" fill="{col}"/>')
        body.append(_text(gx + gw / 2, 71, gt, 11.5, "#ffffff", "700"))
        body.append(_box(gx + 14, 94, 58, 20, "#edf5f5", "#b7d4d4", 4))
        body.append(_text(gx + 43, 108, gbc, 9.6, NAVY, "700"))
        body.append(_text(gx + 78, 108, guard, 9.6, TEAL, "700", "start"))
        body.append(_text(gx + 14, 136, l1, 10.6, NAVY, "600", "start"))
        body.append(_text(gx + 14, 158, l2, 10.2, MUTED, "400", "start"))
        body.append(_text(gx + 14, 182, "事务内 FOR SHARE 守卫", 9.6, col, "600", "start"))
        # Arrow down to merge bar
        ax = gx + gw / 2
        body.append(f'<path d="M{ax:.0f},196 L{ax:.0f},230" stroke="{col}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')

    # Merge bar: FUL Release
    body.append(_box(28, 232, W - 56, 52, NAVY, NAVY, 9))
    body.append(_text(W / 2, 254, "FUL 商业条件放行 ＋ 实际出运确认 ＋ 双语商业发票／装箱单签发 ＋ 出口批次形成", 13, "#ffffff", "700"))
    body.append(_text(W / 2, 274, "CIF ／ CIP 条款须足额保险方可放行单证 ｜ 外部上报、检验附件、普通备注或 AI 建议均不可单独替代放行事实", 10.6, "#bdd5dd", "400"))

    # Bottom Left: 7 Forbidden SoD Combinations
    body.append(_box(28, 304, 594, 174, "#fdf1f0", "#e6b8b2", 9))
    body.append(_text(46, 329, "职责分离硬禁令（7 组禁止兼任岗位模板 · 编译生成 roles_gen.go 强制校验）", 11.8, "#9b3a30", "700", "start"))
    sods = [
        "销售经办  ✕  报价审核人（报价编审分离）",
        "销售经办  ✕  订单审核人（订单编审分离）",
        "销售经办  ✕  毛利审批人（禁批本人例外）",
        "核价岗位  ✕  毛利审批人（成本与审批分离）",
        "采购岗位  ✕  订单审核人（采购承诺编审分离）",
        "销售经办  ✕  信用专员（销售禁放行信用）",
        "销售经办  ✕  信用经理（销售禁放行信用）",
    ]
    for idx, label in enumerate(sods):
        col_i = idx % 2
        row_i = idx // 2
        px = 46 + col_i * 284
        py = 344 + row_i * 31
        pw = 274 if idx < 6 else 558
        body.append(_box(px, py, pw, 25, "#ffffff", "#ebc4bf", 5))
        body.append(_text(px + 12, py + 16.5, label, 10.4, "#7a2c24", "600", "start"))

    # Bottom Right: 10 Exclusive Permissions
    body.append(_box(640, 304, 472, 174, "#edf5f5", "#a8cbcb", 9))
    body.append(_text(658, 329, "10 项唯一岗位专属权限（其他岗位与 AI 均不可持有）", 11.8, NAVY, "700", "start"))

    body.append(_box(658, 344, 436, 62, "#ffffff", "#b7d4d4", 6))
    body.append(_text(672, 363, "合规岗位专属（compliance · 6 项）", 11, TEAL, "700", "start"))
    body.append(_text(672, 382, "主体筛查决定与拦截 · 贸易合规裁决 · 筛查名单与国别项目导入", 10.2, NAVY, "400", "start"))
    body.append(_text(672, 398, "合规法规与启用名单发布（config.publish.screening_regulation）", 10.2, MUTED, "400", "start"))

    body.append(_box(658, 414, 436, 52, "#ffffff", "#b7d4d4", 6))
    body.append(_text(672, 433, "信用经理专属（credit_manager · 4 项）", 11, "#1f4a63", "700", "start"))
    body.append(_text(672, 452, "信用确认 · 信用额度发布 · 信用政策发布 · 商业放行付款条件例外", 10.2, NAVY, "400", "start"))

    return _svg(W, H, "".join(body), "商业条件放行五重闸门与职责分离防线")


def pm_china_three_reconciliations() -> str:
    """China CN-M01 three parallel reconciliations and external statutory system ACL boundary."""
    W, H = 1140, 514
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Left: Brant Controllable Domain
    body.append(_box(28, 18, 678, 362, "#f4f9f8", TEAL, 10, 'stroke-width="1.4"'))
    body.append(_text(46, 42, "Brant 内部协同域（CN-M01：境内单主体自营一般贸易出口 · 三线并行核对）", 12.2, TEAL, "700", "start"))

    lanes = [
        (56, "#edf5f5", "#9bc2c2", NAVY, "① 数量核对线（PUR ／ STK ／ MFG ／ FUL）",
         "原料、在制与成品口径严格分开；直发按已核实交付守卫，本仓按 FIFO 批次成本层",
         "委外受计划锁、累计上限与交接货号校验；海关放行、离境、送达、验收各自独立（CN-R07）"),
        (140, "#f4f2fa", "#bfb3dc", "#4a3875", "② 票证核对线（CNX · 多对多用途匹配）",
         "购进发票（import/verify）、出口批次、报关资料、退税用途分配与原产地／检验证书多对多关联",
         "可抵扣／可退／不可收回税额按核实口径区分，绝不既计全额采购成本又计预计退税利润（CN-R03）"),
        (224, "#e8eff3", "#a2bac3", "#1f4a63", "③ 资金核对线（STL · 商业资金台账）",
         "预收预付、部分收付、商业核销、撤销核销、费用分摊与订单经营结果按不可变条目推导",
         "严守 CN-R11：系统内核销只是商业资金分配，不得冒充行政意义的出口收汇核销"),
    ]
    for ly, fill, stroke, tcol, lt, l1, l2 in lanes:
        body.append(_box(46, ly, 642, 74, fill, stroke, 7))
        body.append(_text(60, ly + 20, lt, 11.4, tcol, "700", "start"))
        body.append(_text(60, ly + 41, l1, 10.2, NAVY, "400", "start"))
        body.append(_text(60, ly + 59, l2, 10.2, MUTED, "400", "start"))

    # Channel reconciliation state machine strip
    body.append(_box(46, 308, 642, 58, "#ffffff", "#b8cdd2", 7))
    body.append(_text(60, 327, "CNX 渠道提交与对账状态机（S6-05 · 8066e1c 已开放 5 条 HTTP 路由，结算不对外暴露）", 10.8, NAVY, "700", "start"))
    body.append(_text(60, 349, "planned → submitted → [ succeeded | failed | unknown → reconcile → not_received 可重提 ]  ·  迟到乱序回执留痕不回退案件", 10.2, TEAL, "600", "start"))

    # Right: External Statutory Authorities (ACL)
    body.append(_box(744, 18, 368, 362, "#ffffff", "#b8cdd2", 10))
    body.append(_text(762, 42, "外部法定权威系统（ACL · R1 零真实外拨）", 12.2, NAVY, "700", "start"))
    exts = [
        (58, "#fff8ed", "#dfc7a1", "中国国际贸易单一窗口（海关）", "报关单受理、放行、离境属外部权威", "R1：人工路径 ＋ 进程内假适配器"),
        (138, "#fff8ed", "#dfc7a1", "金税 ／ 税务机关", "购进底账与出口退税核准属税务权威", "R1：人工登记与核实 · 不预设算法"),
        (218, "#e8eff3", "#a2bac3", "银行与外汇系统", "收付清算与结汇水单属银行权威", "AI 禁止读取 bank_account 配置"),
        (298, "#e8eff3", "#a2bac3", "外部法定总账 ERP", "财务凭证与法定报表属 ERP 权威", "Brant 输出业务台账口径，非法定总账"),
    ]
    for ey, fill, stroke, et, e1, e2 in exts:
        body.append(_box(762, ey, 332, 68, fill, stroke, 7))
        body.append(_text(776, ey + 21, et, 11.4, NAVY, "700", "start"))
        body.append(_text(776, ey + 41, e1, 10.2, "#38566a", "400", "start"))
        body.append(_text(776, ey + 57, e2, 10.0, "#8c5c1f" if fill == "#fff8ed" else MUTED, "600", "start"))

    # Bidirectional ACL arrows between Left & Right
    for ay in (92, 172, 252, 332):
        body.append(f'<path d="M706,{ay} L742,{ay}" stroke="{MUTED}" stroke-width="1.6" fill="none" marker-start="url(#arr)" marker-end="url(#arr)"/>')

    # Bottom boundary strip
    body.append(_box(28, 398, 534, 96, "#fbeceb", "#dfa6a0", 8))
    body.append(_text(46, 424, "永久退出项 · BIP 交换与渠道平台（2026-11-23 负责人确认）", 11.8, "#9b3a30", "700", "start"))
    body.append(_text(46, 448, "BIP 不再作为 Brant 产品选项，后续不考虑、不规划接入。", 10.6, "#6f2821", "600", "start"))
    body.append(_text(46, 470, "代码仅留未连接占位（EXTERNAL_CHANNEL_NOT_CONFIGURED），外发统一走人工节点。", 10.2, "#7c332b", "400", "start"))

    body.append(_box(580, 398, 532, 96, "#fff8ed", "#e3c9a0", 8))
    body.append(_text(598, 424, "中国模式排除边界（不以单模式推导全部外贸形态）", 11.8, "#8c5c1f", "700", "start"))
    body.append(_text(598, 448, "支持 CN-M01 默认首发，及 CN-M02 下的部分工序委外（不含自有车间排产）。", 10.6, NAVY, "600", "start"))
    body.append(_text(598, 470, "不支持：CN-M03 代理 · CN-M04 加工贸易保税手账册 · CN-M05 跨境零售/离岸 · CN-M06 融资。", 10.1, MUTED, "400", "start"))

    return _svg(W, H, "".join(body), "中国出口 CN-M01 三线并行核对与外部权威边界")


def pm_evidence_dashboard() -> str:
    """Scientific 5-tier verification pyramid and real measured benchmarks (honest misses included)."""
    W, H = 1140, 534
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Left Panel: 5-Tier Verification Pyramid
    body.append(_box(28, 18, 464, 498, "#ffffff", "#c2d4d9", 10))
    body.append(_text(46, 44, "五层可证伪验证金字塔（代码 8066e1c ／ 归档 48f0c1c）", 12.2, NAVY, "700", "start"))
    body.append(_text(46, 63, "下层自动化通过绝不替代上层生产与客户证据", 10.4, MUTED, "400", "start"))

    tiers = [
        ("L5 生产环境与真实客户", "G0–G5 NO-GO", "#b6463a", "#ffffff", "缺真实客户、真人签核、企业 OIDC 与真实单一窗口／金税", 268),
        ("L4 参照负载与现场模型", "部分未达标", "#ba743e", "#ffffff", "脚本 Q01 70/70 过；中继 3w P95 6.403s；模型 7/11 族过", 316),
        ("L3 PG16 集成与竞态", "CI 16/16 过", "#0b7b7c", "#ffffff", "900 集成 · 106 迁移 · 282 FORCE RLS · 795 本机端到端", 364),
        ("L2 单元与算术等价", "PASS", "#1f4a63", "#ffffff", "1,433 单元 ＋ 9 模糊目标 · Python V0.50 455 项逐分一致", 404),
        ("L1 静态规则与架构红线", "0 违规", NAVY, "#ffffff", "215 Go 包 · 82 白名单 · 26 无环守卫 · golangci-lint 0", 430),
    ]
    cx_p = 260
    for idx, (tt, st, fill, ink, desc, tw) in enumerate(tiers):
        ty = 82 + idx * 84
        tx = cx_p - tw / 2
        body.append(_box(tx, ty, tw, 72, fill, "none", 8))
        body.append(_text(cx_p, ty + 23, f"{tt}  ·  {st}", 11.4, ink, "700"))
        for li, ln in enumerate(wrap(desc, 10.0, tw - 22)[:2]):
            body.append(_text(cx_p, ty + 44 + li * 16, ln, 10.0, "#eef6f7", "400"))

    # Right Top Panel: Reference Load & Recovery Benchmark Bars
    body.append(_box(510, 18, 602, 244, "#ffffff", "#c2d4d9", 10))
    body.append(_text(528, 44, "参照负载实测 vs 目标（16 命令＋16 读取 · WSL2 · 1264d61）", 12.2, NAVY, "700", "start"))
    body.append(_text(528, 63, "实测中继 P95 未达 5 s 目标，主因 WAL 写锁排队与 WSL2 虚拟盘，目标未放宽", 10.2, MUTED, "400", "start"))

    metrics = [
        ("读取 P95（3w）", 0.232, 0.500, 0.232 / 0.500, "#2f6e52", "0.232 s ／ 限 0.50 s · PASS"),
        ("命令 P95（3w）", 0.668, 0.800, 0.668 / 0.800, "#2f6e52", "0.668 s ／ 限 0.80 s · PASS"),
        ("中继 P95（2w）", 13.011, 5.000, 1.00, "#b6463a", "13.011 s ／ 限 5.00 s · MISS"),
        ("中继 P95（3w）", 6.403, 5.000, 0.88, "#ba743e", "6.403 s ／ 限 5.00 s · MISS (94.06%≤5s)"),
        ("中继 P99（3w）", 16.293, 30.000, 16.293 / 30.000, "#2f6e52", "16.293 s ／ 限 30.0 s · PASS"),
    ]
    bar_x, bar_w = 646, 224
    for idx, (ml, _obs, _lim, frac, col, lbl) in enumerate(metrics):
        my = 80 + idx * 33
        body.append(_text(528, my + 16, ml, 10.5, NAVY, "600", "start"))
        body.append(_box(bar_x, my + 2, bar_w, 20, "#f0f5f6", "#dce6e8", 4))
        fw = max(14, min(bar_w, frac * bar_w))
        body.append(_box(bar_x, my + 2, fw, 20, col, "none", 4))
        # target threshold tick for missed relay bars at 5/13 or 5/6.4
        if ml.startswith("中继 P95"):
            tick_x = bar_x + (0.38 if "2w" in ml else 0.68) * bar_w
            body.append(f'<line x1="{tick_x:.1f}" y1="{my}" x2="{tick_x:.1f}" y2="{my + 24}" stroke="{NAVY}" stroke-width="1.6" stroke-dasharray="2,2"/>')
        body.append(_text(bar_x + bar_w + 10, my + 16, lbl, 9.8, "#9b3a30" if "MISS" in lbl else "#2f6e52", "700", "start"))

    # Right Bottom Panel: Live Model Eval Breakdown
    body.append(_box(510, 278, 602, 238, "#ffffff", "#c2d4d9", 10))
    body.append(_text(528, 304, "Q01 现场模型 14 族实测分布（qwen3.8-flash-next-iq3_xxs · 1264d61）", 12.2, NAVY, "700", "start"))
    body.append(_text(528, 323, "脚本化 CI 为 70/70 全通过；现场模型合并 3 轮无重跑记录如下", 10.2, MUTED, "400", "start"))

    # Segmented 14-family bar
    seg_x, seg_y, seg_w, seg_h = 528, 338, 566, 28
    segs = [
        (7 / 14, "#2f6e52", "7 族 5/5 过"),
        (1 / 14, "#ba743e", "1"),
        (3 / 14, "#b6463a", "3 族 0/5"),
        (3 / 14, "#5a6b8c", "3 族不适用"),
    ]
    cur_x = seg_x
    for ratio, col, sl in segs:
        sw_s = seg_w * ratio
        body.append(_box(cur_x, seg_y, sw_s - 2, seg_h, col, "none", 4))
        body.append(_text(cur_x + (sw_s - 2) / 2, seg_y + 18, sl, 10.2, "#ffffff", "700"))
        cur_x += sw_s

    # 4 detail tiles
    tiles = [
        (528, 378, 276, 60, "#edf6f3", "#b5d7ca", "#2f6e52", "7 族 5/5 通过（35/35）", "an04 · an06 · an07 · an09 · an10 · an11 · ct14"),
        (818, 378, 276, 60, "#fff8ed", "#e3c9a0", "#8c5c1f", "1 族部分通过：normal（4/5）", "normal-2 停于 422 legal_terms_ref"),
        (528, 446, 276, 56, "#fbeceb", "#dfa6a0", "#9b3a30", "3 族 0/5 失败（待攻关）", "an01 缺字段 · an03 960≠948 · an05 25m 预算"),
        (818, 446, 276, 56, "#eef1fa", "#cdd5ee", "#3d4e8c", "3 族 NOT APPLICABLE", "an08 · reads · feedback（脚本专属，非失败）"),
    ]
    for tx, ty, tw, th, fill, stroke, tcol, t1, t2 in tiles:
        body.append(_box(tx, ty, tw, th, fill, stroke, 6))
        body.append(_text(tx + 12, ty + 21, t1, 10.8, tcol, "700", "start"))
        body.append(_text(tx + 12, ty + 42, t2, 9.8, NAVY, "400", "start"))

    return _svg(W, H, "".join(body), "科学验证金字塔与实测数据看板")


def apparel_plugin_architecture() -> str:
    """Apparel plugin layered architecture, dependency-check rules, and 4 extension points."""
    W, H = 1140, 592
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Layer 1: Bootstrap Composition Root
    body.append(_box(28, 18, W - 56, 62, NAVY, NAVY, 9))
    body.append(_text(46, 42, "internal/bootstrap（组合根 · 唯一装配层）", 12.8, "#ffffff", "700", "start"))
    body.append(_text(46, 63, "校验精确支持组合 (core, contract, plugin) → 注册 industry.apparel → 注入四上下文扩展端口 ＋ AI 语义提供者（CRM 关时不装 bridge）", 10.6, "#cde2e8", "400", "start"))

    # Layer 2 Left: internal/plugins/apparel (new layerPlugin)
    body.append(_box(28, 102, 540, 212, "#f4f2fa", "#a898d1", 10, 'stroke-width="1.5"'))
    body.append(_text(46, 126, "internal/plugins/apparel（新增 layerPlugin · 纯计算与策略实现）", 12.2, "#4a3875", "700", "start"))
    body.append(_text(46, 146, "允许导入：stdlib · kernel · foundation/plugin · 上游 api/（禁上下文 internal、禁 bentopg.DB、禁网络 I/O）", 9.8, "#5d4c86", "400", "start"))

    p_cards = [
        (46, 160, 244, 64, "cost.estimate.v1", "分码单耗 · 幅宽／克重／缩水／损耗", "按色汇总 MCQ 起染费 ＋ 版费分摊"),
        (306, 160, 244, 64, "quote.validate.v1", "款色码 · 样衣版本 · 短溢装 · 防混单", "合并裁决 reject > requires_approval > pass"),
        (46, 236, 244, 64, "packing & inspection", "尺码配比 · 箱规 CBM 与装柜测算", "色差／缩水／公差（禁未测冒充实测）"),
        (306, 236, 244, 64, "AI 行业契约（4 项）", "semantics · context · task_profile", "outcome.check.v1（不可取消平台必检）"),
    ]
    for cx, cy, cw, ch, ct, l1, l2 in p_cards:
        body.append(_box(cx, cy, cw, ch, "#ffffff", "#c5b9e3", 7))
        body.append(_text(cx + 12, cy + 20, ct, 11, "#4a3875", "700", "start"))
        body.append(_text(cx + 12, cy + 39, l1, 10.0, NAVY, "400", "start"))
        body.append(_text(cx + 12, cy + 54, l2, 9.5, MUTED, "400", "start"))

    # Layer 2 Right: internal/foundation/plugin (trade-extension.v1 + brant_ext)
    body.append(_box(590, 102, 522, 212, "#f3eadb", "#cfb997", 10, 'stroke-width="1.5"'))
    body.append(_text(608, 126, "internal/foundation/plugin（平台契约层 · trade-extension.v1）", 12.2, "#5b4520", "700", "start"))
    body.append(_text(608, 146, "仅导入 stdlib ＋ kernel ＋ foundation（永不导入任何业务上下文 · 与 auth/msg 同级）", 9.8, "#775c2e", "400", "start"))

    f_cards = [
        (608, 160, 486, 44, "PluginRegistry & Manifest 强校验", "校验 requires(id, contract_version) · 拒绝同名检查覆盖 · 拒绝双成本提供者 COST_PROVIDER_CONFLICT"),
        (608, 212, 486, 44, "brant_ext 公司级激活账本（migrations/ext/000001 · FORCE RLS）", "状态机 disabled ↔ active → draining_readonly → disabled；在途单据／任务阻断硬停用 PLUGIN_IN_USE"),
        (608, 264, 486, 38, "PluginGuard v1（SECURITY DEFINER · FOR SHARE）", "brant_ext.guard_plugin_active_v1 供下游上下文事务内复核激活态与版本，排在 manifest 中 ref 之后"),
    ]
    for cx, cy, cw, ch, ct, l1 in f_cards:
        body.append(_box(cx, cy, cw, ch, "#ffffff", "#d8c6a8", 7))
        body.append(_text(cx + 12, cy + 18, ct, 10.8, "#5b4520", "700", "start"))
        body.append(_text(cx + 12, cy + 33 if ch < 42 else cy + 35, l1, 9.6, NAVY, "400", "start"))

    # Arrows: Bootstrap -> Plugin & Foundation; Plugin -> Foundation
    body.append(f'<path d="M298,80 L298,100" stroke="{TEAL}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')
    body.append(f'<path d="M851,80 L851,100" stroke="{TEAL}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')
    body.append(f'<path d="M568,208 L588,208" stroke="#6b4fa0" stroke-width="1.6" fill="none" marker-end="url(#arr)"/>')

    # Layer 3: Four Core Context Extension Points + AI Layer
    body.append(_box(28, 336, W - 56, 182, "#edf5f5", TEAL, 10, 'stroke-width="1.4"'))
    body.append(_text(46, 360, "四大核心上下文扩展点 ＋ AI 层（单写者事实权威留在既有 Context · 严禁上下文反向导入 internal/plugins）", 12.0, TEAL, "700", "start"))

    ext_cols = [
        ("CAT", "Core/Supp", "款色码 ＋ 样衣批准", "Item 矩阵 · SampleApproval", "发布 SampleGuard v1", "brant_cat 000005–06"),
        ("CST", "Core", "成本 BOM ＋ 替代审批", "CostBOM · 消耗公式 · 替代", "发布 CostBOMGuard v1", "brant_cst 000004–05"),
        ("INQ / TRD", "Core", "模板绑定 ＋ 校验合并", "document_profile 防混单", "Q01.industry 接 SampleGuard", "inq/000005 · trd/000013"),
        ("QLT / FUL", "Supp", "质检参数 ＋ 尺码配比", "色差缩水 · Assortment 配比", "禁跨模板订单合并出运", "qlt/000004 · ful/000012"),
        ("TSK / AI", "AI", "语义 ＋ 断言 ＋ 指纹", "ExecutionBundle 绑插件版本", "升级触发 CT14 暂停", "ai/000006"),
    ]
    ew, egap = 196, 17
    for idx, (ec, _tier, et, l1, l2, l3) in enumerate(ext_cols):
        ex = 46 + idx * (ew + egap)
        body.append(_box(ex, 374, ew, 128, "#ffffff", "#9dc2c2", 8))
        body.append(_box(ex, 374, ew, 28, NAVY if idx < 4 else "#6b4fa0", NAVY if idx < 4 else "#6b4fa0", 8))
        body.append(f'<rect x="{ex}" y="392" width="{ew}" height="10" fill="{NAVY if idx < 4 else "#6b4fa0"}"/>')
        body.append(_text(ex + ew / 2, 392, f"{ec} · {et}", 10.6, "#ffffff", "700"))
        body.append(_text(ex + 10, 422, l1, 10.1, NAVY, "600", "start"))
        body.append(_text(ex + 10, 444, l2, 9.8, TEAL, "400", "start"))
        body.append(_box(ex + 10, 464, ew - 20, 26, "#f4f8f8", "#d6e4e7", 5))
        body.append(_text(ex + ew / 2, 481, l3, 9.5, MUTED, "600"))
        # Downward arrow from plugin/foundation to extension points
        ax = ex + ew / 2
        body.append(f'<path d="M{ax:.0f},314 L{ax:.0f},334" stroke="{TEAL}" stroke-width="1.4" stroke-dasharray="4,3" fill="none" marker-end="url(#arrT)"/>')

    # Bottom Guardrail Strip
    body.append(_box(28, 534, W - 56, 40, "#fdf1f0", "#e4b2ac", 7))
    body.append(_text(W / 2, 558, "静态门禁（tools/dependency-check）：上下文禁止 import internal/plugins/* ｜ 插件禁止 import */internal/* ｜ 核心金额／余额／库存与外键绝不落入任意 JSON 扩展字段", 10.6, "#9b3a30", "700"))

    return _svg(W, H, "".join(body), "服装行业插件分层架构、依赖规则与四大扩展点")


def apparel_domain_er() -> str:
    """Domain aggregates, single-writer schema ownership, and cross-context guard references."""
    W, H = 1140, 560
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Top Strip: brant_ext (Platform Activation)
    body.append(_box(28, 18, W - 56, 78, "#f3eadb", "#cfb997", 9))
    body.append(_text(46, 42, "brant_ext · 平台插件激活与版本登记（migrations/ext/000001 · FORCE RLS · 无业务外键）", 11.8, "#5b4520", "700", "start"))
    body.append(_box(46, 52, 514, 34, "#ffffff", "#d8c6a8", 6))
    body.append(_text(58, 73, "plugin_activation(tenant_id, company_id, plugin_id) · status · version · manifest_sha256", 10.0, NAVY, "600", "start"))
    body.append(_box(578, 52, 516, 34, "#ffffff", "#d8c6a8", 6))
    body.append(_text(590, 73, "guard_plugin_active_v1(plugin_id, document_profile) → FOR SHARE 守卫（零死锁）", 10.0, TEAL, "700", "start"))

    # Row 2: CAT (Left) + CST (Right)
    # CAT Box
    body.append(_box(28, 118, 534, 214, "#ffffff", TEAL, 9, 'stroke-width="1.5"'))
    body.append(_box(28, 118, 534, 32, TEAL, TEAL, 9))
    body.append(f'<rect x="28" y="140" width="534" height="10" fill="{TEAL}"/>')
    body.append(_text(46, 139, "brant_cat · 款色码矩阵与样衣批准（CAT-AGG-Item ＋ CAT-AGG-SampleApproval）", 11.5, "#ffffff", "700", "start"))

    cat_tables = [
        (44, 162, 246, 154, "item & item_sku_variant", [
            "PK (tenant, company, item_id)",
            "style_code · document_profile",
            "color_code × size_code → sku_id",
            "FK → brant_cat.sku (保持既有 SkuRef)",
            "幅宽 cm · 克重 GSM · 缩水率 %",
            "measured 标记（未测禁冒充实测）",
            "dye_lot_policy（同缸策略引用）",
        ]),
        (302, 162, 244, 154, "sample_approval & revision", [
            "PK (tenant, company, sample_id)",
            "绑定 sku_id ＋ spec_revision",
            "stage: proto/fit/pp_sample/ship",
            "status: draft→approved→revoked",
            "evidence_ids[] (EVD 留证)",
            "仅人工批准（quality_inspector）",
            "守卫: guard_sample_approved_v1",
        ]),
    ]
    for tx, ty, tw, th, title, lines in cat_tables:
        body.append(_box(tx, ty, tw, th, "#f7fbfb", "#b7d4d4", 7))
        body.append(_text(tx + 10, ty + 19, title, 10.6, TEAL, "700", "start"))
        for li, ln in enumerate(lines):
            body.append(_text(tx + 10, ty + 39 + li * 18, "• " + ln, 9.6, NAVY if li < 6 else TEAL, "600" if li == 6 else "400", "start"))

    # CST Box
    body.append(_box(580, 118, 532, 214, "#ffffff", "#1f4a63", 9, 'stroke-width="1.5"'))
    body.append(_box(580, 118, 532, 32, "#1f4a63", "#1f4a63", 9))
    body.append(f'<rect x="580" y="140" width="532" height="10" fill="#1f4a63"/>')
    body.append(_text(598, 139, "brant_cst · 成本 BOM 与替代审批（CST-AGG-CostBOM ＋ Substitution）", 11.5, "#ffffff", "700", "start"))

    cst_tables = [
        (596, 162, 246, 154, "cost_bom ＋ size_grade ＋ surcharge", [
            "PK (tenant, company, bom_id)",
            "引用 item_id / (sku_id, spec_rev)",
            "status: draft→verified→withdrawn",
            "分码单耗表（缺码拒 422 禁回退）",
            "分色起染费 MCQ ＋ 打样／版费",
            "SUP 供应报价引用 ＋ content_sha",
            "守卫: guard_cost_bom_current_v1",
        ]),
        (854, 162, 242, 154, "cost_bom_substitution", [
            "PK (tenant, company, subst_id)",
            "原物料 ref → 替代物料 ref",
            "applicable_qty · tech_revision",
            "成本／交期影响 ＋ 批准范围",
            "独立人工审批（禁自申自批）",
            "批准生成新 BOM 修订",
            "绝不倒写已发料／已加工事实",
        ]),
    ]
    for tx, ty, tw, th, title, lines in cst_tables:
        body.append(_box(tx, ty, tw, th, "#f7f9fb", "#a7bcc5", 7))
        body.append(_text(tx + 10, ty + 19, title, 10.6, "#1f4a63", "700", "start"))
        for li, ln in enumerate(lines):
            body.append(_text(tx + 10, ty + 39 + li * 18, "• " + ln, 9.6, NAVY if li < 6 else "#9b3a30", "600" if li == 6 else "400", "start"))

    # Row 3: Downstream Consumers (INQ, TRD, QLT, FUL, AI)
    d_boxes = [
        ("brant_inq", "inq/000005", "rfq.document_profile", "确认后不可变 · 防混单"),
        ("brant_trd", "trd/000013", "profile ＋ tolerance", "快照 ＋ 短溢装容差策略"),
        ("brant_qlt", "qlt/000004", "apparel_check", "色差／缩水／尺寸／缸号"),
        ("brant_ful", "ful/000012", "assortment ＋ carton", "配比 · 箱规 CBM · 装柜校验"),
        ("brant_ai", "ai/000006", "bundle.plugins_json", "插件版本指纹 → CT14"),
    ]
    dw, dgap = 202, 18
    for idx, (sch, mig, fld, desc) in enumerate(d_boxes):
        dx = 28 + idx * (dw + dgap)
        body.append(_box(dx, 366, dw, 108, "#edf5f5" if idx < 4 else "#f4f2fa", "#9bc2c2" if idx < 4 else "#c5b9e3", 8))
        body.append(_text(dx + 12, 389, sch, 11.4, NAVY if idx < 4 else "#4a3875", "700", "start"))
        body.append(_text(dx + 12, 410, mig, 9.8, TEAL if idx < 4 else "#6b4fa0", "600", "start"))
        body.append(_text(dx + 12, 434, fld, 10.2, NAVY, "600", "start"))
        body.append(_text(dx + 12, 456, desc, 9.6, MUTED, "400", "start"))

    # Arrows: brant_ext -> CAT & CST; CAT & CST -> Downstream
    body.append(f'<path d="M295,96 L295,116" stroke="#8a6d3b" stroke-width="1.5" fill="none" marker-end="url(#arr)"/>')
    body.append(f'<path d="M846,96 L846,116" stroke="#8a6d3b" stroke-width="1.5" fill="none" marker-end="url(#arr)"/>')
    for ax in (129, 349, 569, 789, 1009):
        body.append(f'<path d="M{ax},332 L{ax},364" stroke="{TEAL}" stroke-width="1.4" stroke-dasharray="4,3" fill="none" marker-end="url(#arrT)"/>')

    # Bottom Lock Order Strip
    body.append(_box(28, 492, W - 56, 50, NAVY, NAVY, 8))
    body.append(_text(W / 2, 513, "事务内确定性无环锁序（ADR-BR-011 扩展）：本聚合根 FOR UPDATE → brant_auth → brant_ext → brant_cat (SkuGuard ＋ SampleGuard) → brant_cst (Adoptability ＋ CostBOM) → inq → pol → pty", 10.3, "#ffffff", "700"))
    body.append(_text(W / 2, 532, "全部 10 个新迁移表强制启用 FORCE ROW LEVEL SECURITY（app.tenant_id / app.company_id），最小授权写入 deploy/postgres/10_app_grants.sql", 9.8, "#bdd5dd", "400"))

    return _svg(W, H, "".join(body), "服装插件领域聚合、Schema 归属与无环锁序图")


def apparel_pricing_and_validation_chain() -> str:
    """Baseline 5.3 7-step fixed pricing chain, validation merge rule, and S-D counterexample."""
    W, H = 1140, 566
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    body.append(_text(W / 2, 30, "固定计价与校验链（基线 5.3 七步确定性流水线 · 服装模板绝不静默回退通用成本模型）", 13.0, NAVY, "700"))

    # Top Row: Steps 1 -> 4
    top_steps = [
        ("步骤 1 · 绑定成本提供者", "#123248", "按 document_profile 唯一查找", "双提供者 → 409 COST_PROVIDER_CONFLICT", "apparel 故障禁静默回退 generic"),
        ("步骤 2 · cost.estimate.v1", TEAL, "分码单耗 ＋ 幅宽／克重／缩水", "按色汇总 MCQ 起染费 ＋ 打样版费", "缺码拒 422 · 未测参数禁冒充实测"),
        ("步骤 3 · 单位与币种归一", "#1f4a63", "核心 CST/TRD 统一换算", "例：2 DOZEN × 12 = 24 PCS", "误录 2 PCS 或缺汇率直接拒绝"),
        ("步骤 4 · 售价、折扣与舍入", "#2d6178", "定价策略或授权业务员报价", "应用客户折扣 · 逐行先舍后加", "显式 HALF_UP · 禁用 float"),
    ]
    sw, sgap = 257, 18
    for idx, (st, col, l1, l2, l3) in enumerate(top_steps):
        sx = 28 + idx * (sw + sgap)
        body.append(_box(sx, 46, sw, 122, "#ffffff", "#b8cdd2", 8))
        body.append(_box(sx, 46, sw, 30, col, col, 8))
        body.append(f'<rect x="{sx}" y="66" width="{sw}" height="10" fill="{col}"/>')
        body.append(_text(sx + sw / 2, 66, st, 11.2, "#ffffff", "700"))
        body.append(_text(sx + 12, 97, l1, 10.2, NAVY, "600", "start"))
        body.append(_text(sx + 12, 119, l2, 10.0, TEAL if idx > 0 else "#9b3a30", "400", "start"))
        body.append(_text(sx + 12, 141, l3, 9.8, MUTED, "400", "start"))
        if idx < 3:
            body.append(f'<path d="M{sx + sw},107 L{sx + sw + sgap - 2},107" stroke="{TEAL}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')

    # Turn-down arrow from Step 4 to Step 5
    body.append(f'<path d="M982,168 L982,196" stroke="{TEAL}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')

    # Middle Row: Steps 7 <- 6 <- 5 (right to left flow)
    bot_steps = [
        ("步骤 7 · 事务重查与快照封存", "#2f6e52", "quote.freeze / quote.activate", "锁本根 FOR UPDATE → 上游守卫 FOR SHARE", "固化成本、汇率、插件版本与校验快照"),
        ("步骤 6 · 内容绑定审批要求", "#ba743e", "存在 requires_approval 时生成", "绑定修订号 ＋ 商业哈希 ＋ 规则版本", "仅覆盖显式可豁免规则（如低毛利）"),
        ("步骤 5 · 核心约束 ＋ quote.validate.v1", "#6b4fa0", "先核心不变量（不可豁免），再服装校验", "合并：reject > requires_approval > pass", "保留全部明细 · 排序不改变业务结论"),
    ]
    bw, bgap = 348, 20
    for idx, (st, col, l1, l2, l3) in enumerate(bot_steps):
        bx = 28 + idx * (bw + bgap)
        body.append(_box(bx, 198, bw, 122, "#ffffff", "#b8cdd2", 8))
        body.append(_box(bx, 198, bw, 30, col, col, 8))
        body.append(f'<rect x="{bx}" y="218" width="{bw}" height="10" fill="{col}"/>')
        body.append(_text(bx + bw / 2, 218, st, 11.2, "#ffffff", "700"))
        body.append(_text(bx + 12, 249, l1, 10.2, NAVY, "600", "start"))
        body.append(_text(bx + 12, 271, l2, 10.0, col, "600", "start"))
        body.append(_text(bx + 12, 293, l3, 9.8, MUTED, "400", "start"))
        if idx > 0:
            # arrow right-to-left: from bx -> bx - bgap + 2
            body.append(f'<path d="M{bx},259 L{bx - bgap + 2},259" stroke="{TEAL}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')

    # Bottom Section: S-D Counterexamples & Q01.industry
    body.append(_box(28, 342, 650, 204, "#fdf1f0", "#e4b2ac", 9))
    body.append(_text(46, 366, "基线 15.5 服装与单位反例 S-D（自动化回归必锁阻断用例）", 11.8, "#9b3a30", "700", "start"))
    sd_rows = [
        ("反例 1 · 单位换算防误录", "红 S 按 2 DOZEN @ 96 USD/DOZEN → 换算 24 PCS @ 8 USD = 192.00 USD；误录 2 PCS 必拒"),
        ("反例 2 · 源文件总数冲突", "矩阵合计 24＋36＋12＋48 = 120 PCS，源文件总计写 118 → INQ 标 PendingConflict 待核实"),
        ("反例 3 · 旧版样衣失效", "样衣批准绑定 TECH-01-r1，订单／SKU 规格升至 r2 → SampleGuard 拒 SAMPLE_SPEC_MISMATCH"),
        ("反例 4 · 跨模板混单阻断", "同一询价或报价混入 generic 与 apparel，或跨模板合并出运 → 拒 DOCUMENT_PROFILE_MISMATCH"),
    ]
    for idx, (rt, rd) in enumerate(sd_rows):
        ry = 380 + idx * 39
        body.append(_box(44, ry, 618, 33, "#ffffff", "#ebc4bf", 6))
        body.append(_text(56, ry + 20.5, f"{rt}：", 10.0, "#9b3a30", "700", "start"))
        body.append(_text(192, ry + 20.5, rd, 9.5, NAVY, "400", "start"))

    body.append(_box(696, 342, 416, 204, "#edf6f3", "#9bc2c2", 9))
    body.append(_text(714, 366, "Q01.industry 升级后的权威核验并集", 11.8, TEAL, "700", "start"))
    q_items = [
        ("1. CAT SkuGuard v1（既有）", "每条行绑定的 (sku_id, spec_revision) 仍为当前确认版"),
        ("2. CAT SampleGuard v1（新增）", "apparel 策略要求样衣时，批准必须绑定当前 spec_revision"),
        ("3. CST CostBOMGuard v1（新增）", "成本快照依赖的 CostBOM 未被撤回且无未审批替代物料"),
        ("4. 并集裁决铁律（基线 18.1）", "全部 satisfied 方可 completed；unknown→waiting_external"),
    ]
    for idx, (qt, qd) in enumerate(q_items):
        qy = 380 + idx * 39
        body.append(_box(712, qy, 384, 33, "#ffffff", "#b5d7ca", 6))
        body.append(_text(722, qy + 14, qt, 10.0, NAVY, "700", "start"))
        body.append(_text(722, qy + 28, qd, 9.4, MUTED, "400", "start"))

    return _svg(W, H, "".join(body), "服装插件七步固定计价校验链与 S-D 反例拦截")


def apparel_plugin_lifecycle_and_ai() -> str:
    """Plugin company activation lifecycle (with draining protection) and AI industry contracts."""
    W, H = 1140, 496
    body = [_box(0, 0, W, H, "#fbfdfd", "none", 0)]

    # Left Panel: Plugin Lifecycle State Machine
    body.append(_box(28, 18, 524, 460, "#ffffff", "#c2d4d9", 10))
    body.append(_text(46, 44, "插件公司级生命周期与停用在途保护（brant_ext）", 12.2, NAVY, "700", "start"))
    body.append(_text(46, 63, "代码发布与租户启用分离；停用前强制检查在途单据与运行中 AI 任务", 10.0, MUTED, "400", "start"))

    # States
    states = [
        (74, 96, 178, 56, "#eef1f3", "#8fa9b2", NAVY, "disabled", "未启用（默认）"),
        (332, 96, 178, 56, TEAL, TEAL, "#ffffff", "active", "已启用（可新开单）"),
        (332, 232, 178, 58, "#ba743e", "#ba743e", "#ffffff", "draining_readonly", "只读排空（禁新开单）"),
    ]
    for sx, sy, sw, sh, fill, stroke, ink, s1, s2 in states:
        body.append(_box(sx, sy, sw, sh, fill, stroke, 9))
        body.append(_text(sx + sw / 2, sy + 24, s1, 12.0, ink, "700"))
        body.append(_text(sx + sw / 2, sy + 43, s2, 10.0, "#eef6f7" if ink == "#ffffff" else MUTED, "400"))

    # Transitions
    # disabled -> active
    body.append(f'<path d="M252,118 L330,118" stroke="{TEAL}" stroke-width="1.6" fill="none" marker-end="url(#arrT)"/>')
    body.append(_text(291, 109, "plugin.enable", 9.6, TEAL, "700"))
    # active -> draining_readonly
    body.append(f'<path d="M421,152 L421,230" stroke="#ba743e" stroke-width="1.6" fill="none" marker-end="url(#arr)"/>')
    body.append(_text(428, 194, "enter_readonly", 9.6, "#8c5c1f", "700", "start"))
    # draining_readonly -> disabled (elbow back)
    body.append(f'<path d="M332,261 L163,261 L163,154" stroke="{NAVY}" stroke-width="1.6" fill="none" marker-end="url(#arr)"/>')
    body.append(_text(246, 252, "plugin.disable（无在途）", 9.6, NAVY, "700"))
    # active -> disabled direct (only if zero in-flight)
    body.append(f'<path d="M332,136 L254,136" stroke="{MUTED}" stroke-width="1.3" stroke-dasharray="4,3" fill="none" marker-end="url(#arr)"/>')
    body.append(_text(293, 150, "无在途直停", 9.2, MUTED, "400"))

    # Lifecycle guarantee cards
    lc_cards = [
        (46, 310, 488, 46, "#fdf1f0", "#e4b2ac", "#9b3a30", "在途硬保护（409 PLUGIN_IN_USE）", "存在未关闭 apparel 报价／订单或非终态 AI 任务时，直接 disable 被拒，必须先转 draining_readonly"),
        (46, 364, 488, 46, "#edf5f5", "#9bc2c2", TEAL, "停用后历史只读可读（基线 5.5 / 18.3）", "历史单据保存冻结的 CostSnapshot ＋ plugin_snapshot，停用后仍由兼容读取器完整展示"),
        (46, 418, 488, 46, "#fff8ed", "#dfc7a1", "#8c5c1f", "精确版本矩阵校验（PLUGIN_VERSION_INCOMPATIBLE）", "按发布登记的精确 (core, contract, plugin) 组合验兼容，不凭语义版本号盲判"),
    ]
    for cx, cy, cw, ch, fill, stroke, tcol, ct, cd in lc_cards:
        body.append(_box(cx, cy, cw, ch, fill, stroke, 7))
        body.append(_text(cx + 12, cy + 18, ct, 10.4, tcol, "700", "start"))
        body.append(_text(cx + 12, cy + 35, cd, 9.5, NAVY, "400", "start"))

    # Right Panel: AI-Native 5 Industry Contracts + ExecutionBundle
    body.append(_box(572, 18, 540, 460, "#ffffff", "#b8c3e0", 10))
    body.append(_text(590, 44, "AI-Native 行业契约接入与 ExecutionBundle 升级屏障", 12.2, "#4a3875", "700", "start"))
    body.append(_text(590, 63, "对齐产品基线第 8.8、18.1 章与附录 B Schema（全部经平台登记与鉴权）", 10.0, MUTED, "400", "start"))

    ai_cards = [
        (590, 80, 504, 54, "#f4f2fa", "#c5b9e3", "1. industry.semantics.v1（Semantics）", "声明款/色/码/样衣/用料术语与别名；字段映射仅允许调用注册纯函数，禁止任意代码注入"),
        (590, 142, 504, 54, "#f4f2fa", "#c5b9e3", "2. context.provider.v1（ContextResult）", "向任务层提供已授权款式、BOM 与样衣查询引用；显式标注 current/stale/conflict/unconfirmed"),
        (590, 204, 504, 54, "#f4f2fa", "#c5b9e3", "3. task.profile.v1 ＋ action.provider.v1", "版本化指令（instruction_sha256）与步骤上限；副作用动作统一经核心入口，禁扩大权限"),
        (590, 266, 504, 58, "#edf6f3", "#9bc2c2", "4. outcome.check.v1（CheckResult · 与平台必检取并集）", "全部 satisfied 才可完成；插件回传 required=false 绝不能取消平台必检；自然语言「完成」无效"),
    ]
    for cx, cy, cw, ch, fill, stroke, ct, cd in ai_cards:
        body.append(_box(cx, cy, cw, ch, fill, stroke, 7))
        body.append(_text(cx + 12, cy + 20, ct, 10.6, "#4a3875" if fill == "#f4f2fa" else TEAL, "700", "start"))
        for li, ln in enumerate(wrap(cd, 9.6, cw - 24)[:2]):
            body.append(_text(cx + 12, cy + 37 + li * 15, ln, 9.6, NAVY, "400", "start"))

    # ExecutionBundle fingerprint box
    body.append(_box(590, 336, 504, 128, NAVY, NAVY, 8))
    body.append(_text(606, 359, "ExecutionBundle 扩展指纹（migrations/ai/000006 · 复用 CT14）", 11.2, "#ffffff", "700", "start"))
    body.append(_box(606, 370, 472, 46, "#1f4a63", "#4b748a", 6))
    body.append(_text(618, 389, 'plugins: [{id:"industry.apparel", version:"0.1.0",', 10.0, "#dbedf0", "600", "start"))
    body.append(_text(618, 406, '           contract:"trade-extension.v1", manifest_sha256:"..."}]', 10.0, "#dbedf0", "600", "start"))
    body.append(_text(606, 434, "插件升级 0.1.0→0.2.0 ⇒ Bundle SHA-256 改变 ⇒ 旧任务 0 模型调用即暂停", 10.0, "#ffd8a8", "600", "start"))
    body.append(_text(606, 452, "状态 paused/conflict（EXECUTION_BUNDLE_INCOMPATIBLE），由人工显式决定续跑或接管", 9.6, "#cde2e8", "400", "start"))

    return _svg(W, H, "".join(body), "服装插件生命周期、停用保护与 AI 契约集成架构")



