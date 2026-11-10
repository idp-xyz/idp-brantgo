#!/usr/bin/env python3
"""Build the IDP Brant master archive under docs/.

Run from anywhere:  python3 docs/00_START_HERE/tools/build_archive.py
Inputs: stage JSON deliverables, tools/data/*.json and tools/pages/*.md; everything the build reads lives in
this repository.
"""

from __future__ import annotations

import base64
import csv
import hashlib
import html
import json
import os
import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

TOOLS = Path(__file__).resolve().parent
sys.path.insert(0, str(TOOLS))

import mdlite  # noqa: E402
import svg  # noqa: E402

DOCS = TOOLS.parents[1]
GO_REPO = DOCS.parent  # the idp-brantgo Go module that holds this archive in docs/
PY_PACKAGE_NAME = "IDP_Brant_T04_A46采购准备_V0.50"  # Python V0.50 reference package, archived in 90_ORIGINAL_SOURCES
ARCHIVE_VERSION = "1.3"
ARCHIVE_DATE = "2026-11-10"
M0_REPORT = "10_M0_ENGINEERING_BASELINE/05_IDP_Brant_M0_Execution_Report_V1.0.html"
M0_EVIDENCE = "10_M0_ENGINEERING_BASELINE/06_M0_EVIDENCE"
S1_EVIDENCE = "09_IMPLEMENTATION_PLAN/05_S1_EVIDENCE"
M0_EVIDENCE_FILES = [
    "README.md", "01_environment.json", "02_go_source_manifest.txt", "03_make_ci_go1.26.5.txt", "04_make_ci_go1.26.8.txt",
    "05_integration_verbose_go1.26.5.txt", "06_kernel_fuzz.txt", "07_migrate_first_run.txt", "08_migrate_second_run.txt",
    "09_dependency_check_negative.txt", "10_spike_consistency.json", "11_relay_drill_test.txt", "12_golangci_lint.txt",
    "13_clock_observation.txt", "14_make_ci_go1.26.5_rerun.txt",
]
S1_EVIDENCE_FILES = [
    "README.md", "01_environment.json", "02_go_source_manifest.txt", "03_make_ci_go1.26.5.txt",
    "04_integration_verbose_go1.26.5.txt", "05_github_actions_runs.json.txt", "06_strictjson_fuzz.txt",
]
# Spike outcomes the M0 report states; the build fails if the evidence says otherwise.
SPIKE_EXPECTED = {
    "SC-04-A": "violation", "SC-04-B1": "consistent", "SC-04-B2": "consistent", "SC-04-B3": "consistent",
    "SC-03-NOLOCK": "violation", "SC-03-ROOTLOCK-replace": "consistent", "SC-03-ROOTLOCK-convert": "consistent",
    "SC-03-STRESS-NOLOCK": "violation", "SC-03-STRESS-ROOTLOCK": "consistent",
}


def load(rel: str):
    return json.loads((DOCS / rel).read_text(encoding="utf-8"))


def tool_data(name: str):
    return json.loads((TOOLS / "data" / name).read_text(encoding="utf-8"))


UL = load("02_UBIQUITOUS_LANGUAGE/01_IDP_Brant_Ubiquitous_Language_V0.1_Draft.json")
CM = load("03_STRATEGIC_DDD/01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json")
ES = load("04_DOMAIN_EVENT_STORMING/02_IDP_Brant_Event_Storming_Model_V0.1.json")
SAPREF = load("04_DOMAIN_EVENT_STORMING/04_IDP_Brant_SAP_Process_Reference_V0.1.json")
SIMG0 = load("04_DOMAIN_EVENT_STORMING/06_IDP_Brant_Simulated_G0_Walkthrough_V0.1.json")
TD = load("05_TACTICAL_DDD/02_IDP_Brant_Tactical_DDD_Candidate_V0.1.json")
WBS = load("09_IMPLEMENTATION_PLAN/02_IDP_Brant_WBS_V0.1.json")
G1 = load("10_M0_ENGINEERING_BASELINE/02_IDP_Brant_G1_Gate_Checklist_V0.1.json")
ADR = load("10_M0_ENGINEERING_BASELINE/03_IDP_Brant_ADR_Register_V0.1.json")
SIMREV = load("10_M0_ENGINEERING_BASELINE/07_IDP_Brant_Simulated_Review_V0.1.json")
REVIEW = tool_data("review_data.json")
ERRORS = tool_data("error_catalog.json")
TABLES = tool_data("table_alignment.json")
PYSUM = tool_data("python_inventory_summary.json")
ARCHIVE = tool_data("archive.json")
RERUN_PATH = DOCS / "07_ENGINEERING_BASELINE_REVIEW/05_EVIDENCE/python_v050_unittest_rerun.json"
RERUN = json.loads(RERUN_PATH.read_text(encoding="utf-8")) if RERUN_PATH.exists() else {}
SPIKE_PATH = DOCS / M0_EVIDENCE / "10_spike_consistency.json"
SPIKE = json.loads(SPIKE_PATH.read_text(encoding="utf-8")) if SPIKE_PATH.exists() else {"results": []}


def go_inventory() -> dict:
    """Go source files, lines and test functions per package directory of the module next to docs/."""
    pkgs: dict[str, dict] = {}
    for p in sorted(GO_REPO.rglob("*.go")):
        rel = p.relative_to(GO_REPO)
        if rel.parts[0] in ("docs", "brand", "bin") or any(part.startswith(".") for part in rel.parts):
            continue
        text = p.read_text(encoding="utf-8")
        e = pkgs.setdefault(rel.parent.as_posix(), {"files": 0, "lines": 0, "unit": 0, "integration": 0, "fuzz": 0})
        e["files"] += 1
        e["lines"] += text.count("\n")
        if p.name.endswith("_test.go"):
            tests = len(re.findall(r"^func Test\w+\(t \*testing\.T\)", text, re.M))
            e["integration" if text.startswith("//go:build integration") else "unit"] += tests
            e["fuzz"] += len(re.findall(r"^func Fuzz\w+\(f \*testing\.F\)", text, re.M))
    return pkgs


GO = go_inventory()

CTX = {c["id"]: c for c in CM["contexts"]}
TIER_CN = {"Core": "核心域", "SupportingPlus": "增强支撑", "Supporting": "支撑域", "Generic": "通用能力", "Optional": "可选包"}
COPIED: list[dict] = []
MISSING_SOURCES: list[str] = []


# ---------------------------------------------------------------- helpers
class Raw(str):
    """Pre-rendered HTML cell."""


def e(s: object) -> str:
    if isinstance(s, Raw):
        return str(s)
    return html.escape("" if s is None else str(s), quote=False)


def attr(s: object) -> str:
    return html.escape(str(s), quote=True)


def table(headers, rows, cls: str = "", tid: str | None = None) -> str:
    cls_attr = f' class="{cls}"' if cls else ""
    id_attr = f' id="{tid}"' if tid else ""
    out = [f'<div class="table-scroll" tabindex="0"><table{cls_attr}{id_attr}><thead><tr>']
    out += [f"<th>{e(h)}</th>" for h in headers]
    out.append("</tr></thead><tbody>")
    for r in rows:
        out.append("<tr>" + "".join(f"<td>{e(c)}</td>" for c in r) + "</tr>")
    out.append("</tbody></table></div>")
    return "".join(out)


def status_kind(s: str) -> str:
    u = str(s).upper()
    if any(k in u for k in ("NO-GO", "NOT_MET", "OPEN", "BLOCK", "FAIL")):
        return "no"
    if any(k in u for k in ("PARTIAL", "PENDING", "NOT_STARTED")):
        return "partial"
    if any(k in u for k in ("PASS", "MET", "OK", "DONE")) and "NOT" not in u:
        return "ok"
    return "cand"


def tag(text: object, kind: str | None = None) -> Raw:
    return Raw(f'<span class="tag {kind if kind is not None else status_kind(str(text))}">{html.escape(str(text))}</span>')


def chip(code: str) -> str:
    tier = CTX.get(code, {}).get("tier", "Supporting")
    return f'<span class="ctx {tier}">{html.escape(code)}</span>'


_CODE_RE = re.compile(r"\b(" + "|".join(sorted(CTX, key=len, reverse=True)) + r")\b")


def chips(text: str) -> Raw:
    return Raw(_CODE_RE.sub(lambda m: chip(m.group(1)), html.escape(str(text), quote=False)))


def code(s: str) -> Raw:
    return Raw(f"<code>{html.escape(str(s))}</code>")


def codes(items) -> Raw:
    return Raw(" ".join(f"<code>{html.escape(str(i))}</code>" for i in items) or "—")


def kpis(items) -> str:
    return '<div class="kpi-grid">' + "".join(
        f'<div class="kpi"><strong>{e(v)}</strong><span>{e(label)}</span></div>' for v, label in items
    ) + "</div>"


def filter_bar(tid: str, placeholder: str, total: int) -> str:
    return (
        f'<div class="filter-bar"><input data-filter="{tid}" placeholder="{attr(placeholder)}">'
        f'<span>显示 <b data-filter-count="{tid}">{total}</b> ／ {total}</span></div>'
    )


def figure(idx: str, title: str, desc: str, markup: str) -> str:
    return (
        f'<figure class="diagram"><figcaption><span class="figure-index">{e(idx)}</span><div><strong>{e(title)}</strong>'
        f"<p>{e(desc)}</p></div></figcaption><div class=\"svg-scroll\">{markup}</div></figure>"
    )


def ol(items) -> str:
    return "<ol>" + "".join(f"<li>{e(i)}</li>" for i in items) + "</ol>"


def ul(items) -> str:
    return "<ul>" + "".join(f"<li>{e(i)}</li>" for i in items) + "</ul>"


def write_csv(rel: str, header, rows) -> None:
    p = DOCS / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    with p.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(header)
        w.writerows(rows)


def sha256(p: Path) -> str:
    h = hashlib.sha256()
    with p.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def snake(name: str) -> str:
    return re.sub(r"(?<!^)(?=[A-Z])", "_", name).lower()


# ----------------------------------------------------------------- brand
# Brand kit V1.2 in the repository root: the visual is locked, the production master is not yet
# authorized. Pages embed these PNGs byte for byte as data URIs, never recolored or redrawn; the
# square icons are previews, not a signed favicon. Without the kit the pages keep the text logo.
BRAND_DIR = GO_REPO / "brand"
BRAND_FILES = {
    "lockup": ("01_TRANSPARENT_LOCKUPS/full_lockup_white.png", 1050, 301),
    "mark": ("01_TRANSPARENT_LOCKUPS/lockup_no_tagline_white.png", 1050, 233),
    "icon32": ("02_SYMBOL_AND_FAVICON_PREVIEWS/Brant_Symbol_midnight_32.png", 32, 32),
    "icon64": ("02_SYMBOL_AND_FAVICON_PREVIEWS/Brant_Symbol_midnight_64.png", 64, 64),
}


def load_brand() -> dict[str, str]:
    paths = {k: BRAND_DIR / rel for k, (rel, _, _) in BRAND_FILES.items()}
    if not all(p.exists() for p in paths.values()):
        return {}
    return {k: "data:image/png;base64," + base64.b64encode(p.read_bytes()).decode("ascii") for k, p in paths.items()}


BRAND = load_brand()


def brand_img(key: str, cls: str, alt: str) -> str:
    _, w, h = BRAND_FILES[key]
    return f'<img class="{cls}" src="{BRAND[key]}" alt="{attr(alt)}" width="{w}" height="{h}">'


def favicon_links() -> str:
    if not BRAND:
        return ""
    return "".join(f'<link rel="icon" type="image/png" sizes="{n}x{n}" href="{BRAND[f"icon{n}"]}">' for n in (32, 64))


# ------------------------------------------------------------- derived data
PY_COMMANDS = {
    "quote.save_draft", "quote.revise", "quote.discard_draft", "quote.freeze", "quote.submit_review", "quote.review.decide",
    "quote.approval.revoke", "quote.activate", "quote.replace", "quote.withdraw", "quote.cancel", "quote.response.record",
    "quote.response.verify", "order.prepare", "order.review.request", "order.review.decide", "order.activate",
    "order.approval.revoke", "purchase.draft.prepare", "rfq.save_draft", "rfq.confirm_scope", "rfq.cancel", "rfq.close",
    "foundation.snapshot", "cost_scenario.save_draft", "cost_scenario.evaluate", "cost_snapshot.freeze",
    "costing_context.save_draft", "costing_context.revise", "supplier_offer.save_draft", "purchase_terms.save_draft", "source.register",
}
PY_RENAMES = {
    "party.save_draft": "master.upsert_draft（kind=customer／supplier）", "party.confirm": "master.confirm", "party.revise": "master.revise",
    "party.deactivate": "master.deactivate", "contact.save_draft": "master.upsert_draft（kind=contact）", "contact.confirm": "master.confirm",
    "sku.save_draft": "master.upsert_draft（kind=sku）", "sku.confirm": "master.confirm", "sku.deactivate": "master.deactivate",
    "config.save_draft": "foundation.save", "config.publish": "foundation.publish", "config.retire": "foundation.retire",
    "number.issue": "foundation.number.issue", "file.register": "foundation.file.register",
    "object_control.bind": "t03.object_control.bind", "object_control.stop": "t03.object_control.stop",
    "object_control.release": "t03.object_control.unbind", "costing.attest_source": "attest_costing_source（本地接口）",
    "supplier_offer.verify": "T02 规格 §13.2 候选（引擎内实现）", "purchase_terms.publish": "T02 规格 §13.2 候选（引擎内实现）",
    "purchase_terms.retire": "T02 规格 §13.2 候选（引擎内实现）", "costing_context.verify": "T02 规格 §13.2 候选（引擎内实现）",
}


HUMAN_ONLY = set(TD["humanOnlyCommands"])


def sm_actors():
    actors = defaultdict(set)
    for sm in TD["stateMachines"]:
        for t in sm["transitions"]:
            if not t["command"].startswith("（"):
                actors[t["command"]].update(t["actor"].split("|"))
    return actors


def command_rows():
    actors = sm_actors()
    rows = []
    for c in CM["contexts"]:
        for cmd in c["commands"]:
            if "（查询）" in cmd:
                continue
            legacy = cmd if cmd in PY_COMMANDS else PY_RENAMES.get(cmd, "—（Python 未实现）")
            act = actors.get(cmd)
            if cmd in HUMAN_ONLY:
                actor = "仅人工"
            elif act and "ai" in act:
                actor = "人工或 AI（同一守卫）"
            elif act == {"system"}:
                actor = "系统"
            else:
                actor = "人工或 AI（按动作权限）"
            rows.append([c["id"], cmd, actor, legacy, f"POST /api/brant/v1/{c['id'].lower()}/commands/{cmd}", c["stage"]])
    return rows


def guard_cycle() -> list[str]:
    """Return one cycle among guard-lock edges (downstream locks upstream guard rows FOR SHARE), or []."""
    edges = defaultdict(set)
    for r in CM["relationships"]:
        if r.get("guardLock"):
            edges[r["frm"]].add(r["to"])
    state: dict[str, int] = {}
    stack: list[str] = []

    def visit(n):
        state[n] = 1
        stack.append(n)
        for m in sorted(edges[n]):
            if state.get(m) == 1:
                return stack[stack.index(m):] + [m]
            if m not in state:
                found = visit(m)
                if found:
                    return found
        stack.pop()
        state[n] = 2
        return []

    for n in sorted(edges):
        if n not in state:
            found = visit(n)
            if found:
                return found
    return []


def event_rows():
    agg_of = {}
    for c in TD["contexts"]:
        for a in c["aggregates"]:
            for ev in a.get("events", []):
                agg_of[(c["id"], ev)] = a["name"]
    rows = []
    for c in CM["contexts"]:
        for ev in c["facts"]:
            rows.append([c["id"], ev, f"idp-brant.{c['id'].lower()}.{snake(ev)}.v1", f"idp-brant.{c['id'].lower()}", agg_of.get((c["id"], ev), "—"), c["stage"]])
    return rows


def whitelist_lines():
    ups = defaultdict(set)
    for r in CM["relationships"]:
        ups[r["to"]].add(r["frm"])
    lines = [
        "# tools/dependency-check 规则由 Context Map V0.1 的 relationships 生成，不手工编辑",
        f"# 来源：03_STRATEGIC_DDD/01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json（{len(CM['relationships'])} 条关系）",
    ]
    for c in CM["contexts"]:
        u = sorted(ups.get(c["id"], []))
        target = "{" + ",".join(x.lower() for x in u) + "}" if len(u) > 1 else (u[0].lower() if u else "")
        if u:
            lines.append(f"allow internal/contexts/{c['id'].lower()}/... -> internal/contexts/{target}/api")
        else:
            lines.append(f"# {c['id'].lower()}：无已登记上游（只依赖 kernel、reference/api、foundation）")
    lines += [
        "deny  internal/contexts/*/api -> internal/contexts/*/api          # api 是叶子包",
        "deny  internal/contexts/... -> internal/{transport,ai,processes,bootstrap}/...",
        "deny  internal/foundation/... -> internal/contexts/...",
        "deny  internal/{transport,ai,processes}/... -> internal/contexts/*/internal/...",
    ]
    return lines


# --------------------------------------------------------------- includes
def inc_stage_table(a):
    rows = []
    for s in fill_tree(ARCHIVE)["stages"]:
        rows.append([s["seq"], s["title"], s["kicker"], Raw(f'<a href="{attr(rel_from(a.get("_page_dir", ""), s["primary"]))}">{html.escape(s["primary"].split("/")[-1])}</a>')])
    return table(["阶段", "主题", "当前候选", "主文件"], rows, "compact")


def inc_value_chain(a):
    return figure("FIG 01", "业务全景与上下文分工", "AI 任务层只组织与核验，交易事实由各上下文维护；共享基础向多数上下文发布已发布语言。", svg.value_chain())


def inc_context_layers(a):
    return figure(a.get("fig", "FIG"), "候选限界上下文分层图", "青色为核心域、紫色为增强支撑、浅色为支撑域；虚线框为条件启用。连线只显示主要业务流。", svg.context_layers(CM["contexts"]))


def inc_dependency_matrix(a):
    return figure(a.get("fig", "FIG"), "可执行上下文映射：import 白名单矩阵", f"行是下游、列是上游；{len(CM['relationships'])} 条已登记关系是 dependency-check 的唯一来源。", svg.dependency_matrix(CM["contexts"], CM["relationships"]))


def inc_ul_kpis(a):
    t = UL["terms"]
    biz = sum(1 for x in t if x["scopeType"] == "business")
    return kpis([(len(t), "候选词条总数"), (biz, "业务术语（18 个上下文）"), (len(UL["invariants"]), "语言不变量"), (len(UL["collisions"]), "同词异义冲突记录")])


def inc_ul_context_table(a):
    cnt = Counter(t["context"] for t in UL["terms"])
    rows = [[Raw(chip(c["id"])), c["name"], c["english"], TIER_CN[c["tier"]] + ("（条件）" if c["conditional"] else ""), cnt.get(c["id"], 0), c["stage"]] for c in CM["contexts"]]
    for k, label in (("PLATFORM", "平台与应用控制（非上下文）"), ("AI", "AI 能力层（非上下文）"), ("KERNEL", "共享内核值对象")):
        rows.append([Raw(tag(k, "")), label, "—", "—", cnt.get(k, 0), "—"])
    return table(["代码", "名称", "English", "分级", "词条数", "Go 阶段"], rows, "compact")


def inc_ul_terms(a):
    scope = a.get("scope", "business")
    terms = [t for t in UL["terms"] if (t["scopeType"] == "business") == (scope == "business")]
    tid = f"ul-terms-{scope}"
    rows = [[code(t["id"]), Raw(chip(t["context"]) if t["context"] in CTX else tag(t["context"], "")), t["cn"], t["en"], t["definition"], t["exclusion"], t["rule"] or "—", t["source"]] for t in terms]
    return filter_bar(tid, "按词条、上下文、定义或规则过滤", len(rows)) + table(["词条 ID", "上下文", "中文", "English", "定义", "禁止混同", "规则", "来源"], rows, "compact", tid)


def inc_ul_commands(a):
    return table(["命令", "领域事件", "上下文", "规则"], [[code(c["command"]), c["event"], Raw(chip(c["context"])), c["rule"]] for c in UL["commands"]], "compact")


def inc_ul_invariants(a):
    return table(["编号", "名称", "规则"], [[x["id"], x["name"], x["rule"]] for x in UL["invariants"]], "compact")


def inc_ul_collisions(a):
    return table(["词", "Brant 业务含义", "其他含义", "处置规则"], [[x["word"], chips(x["brantMeaning"]), chips(x["otherMeaning"]), x["rule"]] for x in UL["collisions"]])


def inc_ul_decisions(a):
    return table(["编号", "术语", "裁决人", "结论", "状态"], [[x["id"], x["term"], x["owner"], x["outcome"], tag(x["status"])] for x in UL["semanticDecisions"]])


def inc_ul_deltas(a):
    return table(["编号", "对象", "原表述", "新表述", "理由", "状态"], [[x["id"], x["object"], x["before"], x["after"], x["rationale"], tag(x["status"])] for x in UL["languageDeltas"]], "compact")


def inc_cm_kpis(a):
    core = sum(1 for c in CM["contexts"] if c["tier"] == "Core")
    return kpis([(len(CM["contexts"]), f"候选限界上下文（核心 {core}）"), (len(CM["relationships"]), "上下游关系"), (len(CM["factAuthority"]), "事实权威条目"), (len(CM["adrs"]), "战略 ADR（1 项阻断）")])


def inc_cm_subdomains(a):
    return table(["子域", "上下文", "为什么", "可检验标准"], [[s["name"], Raw(" ".join(chip(c) for c in s["contexts"]) or "—"), s["why"], s["test"]] for s in CM["subdomains"]])


def inc_cm_context_table(a):
    rows = [[Raw(chip(c["id"])), c["name"], TIER_CN[c["tier"]] + ("（条件）" if c["conditional"] else ""), c["truth"], c["boundary"], c["stage"]] for c in CM["contexts"]]
    return table(["代码", "名称", "分级", "拥有的事实权威", "不负责", "阶段"], rows, "compact")


def inc_cm_context_cards(a):
    tiers = [t for t in a.get("tier", "").split(",") if t]
    out = []
    for c in CM["contexts"]:
        if tiers and c["tier"] not in tiers:
            continue

        def dd(k, v):
            return f"<dt>{e(k)}</dt><dd>{e(v)}</dd>"

        out.append(
            f'<div class="ctx-card" id="ctx-{c["id"].lower()}"><header>{chip(c["id"])}<h4>{e(c["name"])}</h4>'
            f'<small>{e(c["english"])} · {TIER_CN[c["tier"]]}{"（条件启用）" if c["conditional"] else ""} · Go 阶段 {e(c["stage"])}</small></header><dl>'
            + dd("职责", c["role"]) + dd("范围", c["scope"]) + dd("事实权威", c["truth"])
            + dd("候选聚合", "、".join(c["aggregates"])) + dd("代表命令", Raw(codes(c["commands"])))
            + dd("发布事实", "、".join(c["facts"])) + dd("语言不变量", "、".join(c["invariants"]) or "—")
            + dd("不负责", c["boundary"]) + dd("设计决定", c["decision"]) + dd("Python 来源", c["legacy"]) + "</dl></div>"
        )
    return "".join(out)


def inc_cm_relationships(a):
    tid = "cm-rel"
    rows = [[r["id"], Raw(chip(r["frm"])), Raw(chip(r["to"])), r["pattern"], r["fact"], r["contract"], "是" if r.get("guardLock") else "—", r["risk"], tag(r["contractStatus"], "partial" if r["contractStatus"].startswith("NOT") else "cand")] for r in CM["relationships"]]
    return filter_bar(tid, "按上下文代码、模式或契约过滤", len(rows)) + table(["编号", "上游", "下游", "模式", "流动的事实", "契约", "守卫锁", "风险", "契约状态"], rows, "compact", tid)


def inc_human_only(a):
    by_ctx = defaultdict(list)
    for r in command_rows():
        if r[1] in HUMAN_ONLY:
            by_ctx[r[0]].append(r[1])
    rows = [[Raw(chip(cid)), Raw(codes(cmds))] for cid, cmds in by_ctx.items()]
    return table(["上下文", f"人工专属命令（共 {sum(len(v) for v in by_ctx.values())} 条）"], rows, "compact")


def inc_cm_external(a):
    return table(["外部来源", "Brant 接入点", "模式", "规则", "边界"], [[x["src"], x["target"], x["pattern"], x["rule"], x["boundary"]] for x in CM["externalBoundaries"]])


def inc_cm_fact_matrix(a):
    tid = "cm-facts"
    rows = [[f["id"], f["fact"], f["classification"], chips(f["fact_authority"]), chips(f["write_owner"]), chips(f["consumers"]), f["invariant"], f["forbidden"], f["priority"]] for f in CM["factAuthority"]]
    return filter_bar(tid, "按事实、上下文或禁止事项过滤", len(rows)) + table(["编号", "事实", "分类", "事实权威", "写入方", "消费方", "不变量", "禁止", "优先级"], rows, "compact", tid)


def inc_cm_scenarios(a):
    out = []
    for s in CM["scenarios"]:
        out.append(f'<h4>{e(s["id"])} · {e(s["title"])} <span class="tag cand">{e(s["mode"])}</span></h4>')
        out.append(table(["项目", "内容"], [["目的", s["purpose"]], ["前提", s["given"]], ["步骤", Raw(ol(s["steps"]))], ["失败与反例", s["failure"]], ["验收", s["accept"]]], "compact"))
    return "".join(out)


def inc_cm_adrs(a):
    return table(["编号", "主题", "建议", "所需证据", "状态"], [[x["id"], x["title"], x["recommendation"], x["requiredEvidence"], tag(x["status"])] for x in CM["adrs"]])


def inc_cm_gates(a):
    return table(["门禁", "名称", "通过条件", "责任角色", "当前状态"], [[g["id"], g["name"], g["passCriteria"], g["approverRole"], tag(g["status"])] for g in CM["gates"]])


def inc_es_kpis(a):
    steps = sum(len(s["steps"]) for s in ES["scenarios"])
    return kpis([(len(ES["scenarios"]), "场景（1 条主链＋7 个反例）"), (steps, "事件风暴步骤"), (len(ES["hotspots"]), "热点与未决"), (len(ES["policies"]), "策略")])


def _es(sid):
    return next(s for s in ES["scenarios"] if s["id"] == sid)


def inc_es_timeline(a):
    s = _es(a.get("scenario", "ES-01"))
    return figure(a.get("fig", "FIG"), f"{s['id']} · {s['title']}", "每列一步：黄色执行者、蓝色命令、淡黄聚合、橙色事件、红色热点。", svg.es_timeline(s))


def inc_es_scenario_table(a):
    s = _es(a.get("scenario", "ES-01"))
    rows = [[st["n"], st["actor"], code(st["command"]), st["aggregate"], st["event"], st["policy"], st["hotspot"] or "—", st["evidence"] or "—"] for st in s["steps"]]
    return table(["步", "执行者", "命令", "聚合", "事件", "策略／规则", "热点", "证据"], rows, "compact")


def inc_es_other_scenarios(a):
    out = []
    for s in ES["scenarios"][1:]:
        out.append(f'<h4>{e(s["id"])} · {e(s["title"])} <span class="tag cand">{e(s["type"])}</span></h4>')
        out.append(f'<p>涉及上下文：{" ".join(chip(c) for c in s["contextFlow"])}</p>')
        rows = [[st["n"], st["actor"], code(st["command"]), st["aggregate"], st["event"], st["policy"], st["hotspot"] or "—"] for st in s["steps"]]
        out.append(table(["步", "执行者", "命令", "聚合", "事件／结果", "策略", "热点"], rows, "compact"))
        out.append(f"<p><b>补充反例：</b>{e('；'.join(s['counterexamples']))}。<b>未决问题：</b>{e('；'.join(s['openQuestions']))}。</p>")
    return "".join(out)


def inc_es_hotspots(a):
    return table(["编号", "热点", "涉及", "问题", "处置建议", "负责人", "状态"], [[h["id"], h["title"], chips(h["context"]), h["issue"], h["proposal"], h["owner"], tag(h["status"])] for h in ES["hotspots"]])


SAP_STATUS_KIND = {"COVERED": "ok", "PARTIAL": "partial", "DEVIATION": "cand", "CONDITIONAL": "cand", "OUT_OF_SCOPE": "", "GAP": "no", "LOCAL": "cand"}


def inc_sap_reference(a):
    cnt = Counter(r["status"] for r in SAPREF["rows"])
    out = [table(["状态", "含义", "行数"], [[tag(k, SAP_STATUS_KIND[k]), v, cnt.get(k, 0)] for k, v in SAPREF["statuses"].items()], "compact")]
    for ln in SAPREF["lines"]:
        rows = [[r["id"], r["sapProcess"], r["sapArea"], Raw(" ".join(chip(c) for c in r["contexts"]) or "—"), Raw(codes(r["commands"]) if r["commands"] else "—"),
                 "、".join(r["events"]) or "—", tag(r["status"], SAP_STATUS_KIND[r["status"]]), r["note"], r["followUp"] or "—", r["refs"]]
                for r in SAPREF["rows"] if r["line"] == ln["id"]]
        tid = f"sap-{ln['id'].lower()}"
        out.append(f"<h3>{e(ln['name'])}</h3>")
        out.append(filter_bar(tid, "按流程、上下文、命令或状态过滤", len(rows))
                   + table(["编号", "SAP 流程", "SAP 领域", "Brant 上下文", "命令", "事件", "状态", "说明", "后续处理", "依据"], rows, "compact", tid))
    return "".join(out)


FIT_KIND = {"FIT": "ok", "PARTIAL": "partial", "GAP": "no"}
DEC_KIND = {"ACCEPTED": "ok", "ACCEPTED_WITH_CONDITIONS": "partial", "DEFERRED": "no", "REJECTED": "no"}


def inc_sim_g0(a):
    c = SIMG0["company"]
    out = [table(["项目", "内容"], [["模拟企业", c["name"]], ["画像", c["profile"]], ["构造依据", c["basis"]]], "compact")]
    for case in SIMG0["cases"]:
        out.append(f"<h3>{e(case['id'])} · {e(case['title'])}</h3><p>{e(case['given'])}</p>")
        rows = [[st["n"], st["actor"], st["action"], Raw(codes(st["commands"]) if st["commands"] else "—"), "、".join(st["events"]) or "—",
                 tag(SIMG0["fits"][st["fit"]], FIT_KIND[st["fit"]]), st["finding"] or "—"] for st in case["steps"]]
        out.append(table(["步", "岗位", "业务动作", "Brant 命令", "事件", "吻合度", "发现"], rows, "compact"))
    out.append("<h3>发现与处置</h3>")
    out.append(table(["编号", "类型", "发现", "处置", "负责角色", "WBS", "依据"],
                     [[f["id"], tag(f["kind"], {"GAP": "no", "PARTIAL": "partial"}.get(f["kind"], "cand")), f["title"], f["decision"], f["owner"], f["wbs"], f["refs"]] for f in SIMG0["findings"]], "compact"))
    return "".join(out)


def inc_sim_review(a):
    out = [table(["角色", "名称", "评审范围"], [[r["id"], r["name"], r["scope"]] for r in SIMREV["roles"]], "compact")]
    cnt = Counter(d["decision"] for d in SIMREV["decisions"])
    out.append(f"<h3>ADR 与待决项裁决（{len(SIMREV['decisions'])} 项：" + "、".join(f"{SIMREV['decisionKinds'][k]} {v}" for k, v in cnt.items()) + "）</h3>")
    tid = "sim-dec"
    rows = [[d["id"], d["subject"], d["role"], tag(SIMREV["decisionKinds"][d["decision"]], DEC_KIND[d["decision"]]), d["outcome"], "；".join(d["conditions"]) or "—"] for d in SIMREV["decisions"]]
    out.append(filter_bar(tid, "按编号、角色或结论过滤", len(rows)) + table(["记录", "对象", "角色", "结论", "裁决", "条件"], rows, "compact", tid))
    out.append("<h3>上下文签核</h3>")
    out.append(table(["上下文", "角色", "结论", "范围", "条件"], [[Raw(chip(x["context"])), x["role"], tag(SIMREV["decisionKinds"][x["decision"]], DEC_KIND[x["decision"]]), x["scope"], "；".join(x["conditions"]) or "—"] for x in SIMREV["contextSignoffs"]], "compact"))
    out.append("<h3>门禁结论</h3>")
    out.append(table(["门禁", "结论"], [[k, v] for k, v in SIMREV["gateOutcome"].items()], "compact"))
    return "".join(out)


def inc_es_policies(a):
    return table(["编号", "当……发生", "则……", "归属"], [[p["id"], p["when"], p["then"], chips(p["owner"])] for p in ES["policies"]], "compact")


def inc_td_kpis(a):
    aggs = sum(len(c["aggregates"]) for c in TD["contexts"])
    full = sum(1 for c in TD["contexts"] if c["depth"] == "full")
    return kpis([(aggs, f"候选聚合（{full} 个上下文完整深化）"), (len(TD["stateMachines"]), "状态机"), (len(TD["invariantRegister"]), "登记不变量"), (len(TD["testCatalog"]), "参考验收用例")])


def inc_td_principles(a):
    return ol(TD["principles"])


def inc_td_lock_order(a):
    return ol(TD["lockOrder"])


def inc_td_context_overview(a):
    rows = [[Raw(chip(c["id"])), "完整" if c["depth"] == "full" else "轮廓", "、".join(x["name"] for x in c["aggregates"]), c["coreQuestion"], c["consistency"]] for c in TD["contexts"]]
    return table(["上下文", "深度", "候选聚合", "核心问题", "一致性"], rows, "compact")


def inc_td_aggregates(a):
    cid = a["context"]
    c = next(x for x in TD["contexts"] if x["id"] == cid)
    out = []
    for g in c["aggregates"]:

        def dd(k, v):
            return f"<dt>{e(k)}</dt><dd>{e(v)}</dd>"

        out.append(
            f'<div class="ctx-card" id="{attr(g["id"].lower())}"><header>{chip(cid)}<h4>{e(g["name"])} · {e(g["cn"])}</h4><small>聚合根 {e(g["root"])}</small></header><dl>'
            + dd("内部实体", "、".join(g["entities"]) or "—") + dd("值对象", "、".join(g["valueObjects"]) or "—")
            + dd("身份", g["identity"]) + dd("并发", g["concurrency"]) + dd("不变量", "、".join(g["invariants"]) or "—")
            + dd("命令", Raw(codes(g["commands"]))) + dd("领域事件", "、".join(g["events"]) or "—")
            + dd("Go 包（目标）", Raw(code(g["goPackage"]))) + dd("Python 来源", g["pythonSource"]) + dd("说明", g["notes"] or "—") + "</dl></div>"
        )
    pol = c.get("policies") or []
    risks = c.get("risks") or []
    if pol or risks:
        out.append(f"<p><b>策略：</b>{e('；'.join(pol) or '—')}　<b>风险：</b>{e('；'.join(risks) or '—')}</p>")
    return "".join(out)


def _sm(sid):
    return next(s for s in TD["stateMachines"] if s["id"] == sid)


def _sm_table(sm):
    rows = [[code(t["from"]), code(t["command"]), code(t["to"]), t["guard"], t["actor"]] for t in sm["transitions"]]
    return table(["从", "命令", "到", "守卫", "执行者"], rows, "compact")


def inc_td_state_machine(a):
    sm = _sm(a["id"])
    fig = figure(a.get("fig", "FIG"), f"{sm['id']} · {sm['aggregate']} · {sm['object']}", "深色为初始状态，双框为终态；箭头只标注命令动词（省略对象前缀），完整命令、守卫与执行者见下表。", svg.state_machine(sm))
    return fig + _sm_table(sm)


def inc_td_state_tables(a):
    skip = set(a.get("skip", "").split(","))
    out = []
    for sm in TD["stateMachines"]:
        if sm["id"] in skip:
            continue
        out.append(f'<h4>{e(sm["id"])} · {chip(sm["context"])} {e(sm["aggregate"])} · {e(sm["object"])}</h4>')
        out.append(_sm_table(sm))
    return "".join(out)


def inc_td_invariants(a):
    cid = a.get("context", "all")
    regs = [r for r in TD["invariantRegister"] if cid == "all" or r["context"] == cid]
    tid = f"td-inv-{cid.lower()}"
    rows = [[r["id"], Raw(chip(r["context"]) if r["context"] in CTX else tag(r["context"], "")), r["aggregate"], r["rule"], r["enforcement"], r["negative"], r["errors"], r["pythonEvidence"], r["ulRef"] or "—"] for r in regs]
    return filter_bar(tid, "按编号、聚合、规则或错误码过滤", len(rows)) + table(["编号", "上下文", "聚合", "业务规则", "实施方式", "负向用例", "错误码", "Python 证据", "UL"], rows, "compact", tid)


def inc_td_orchestrations(a):
    rows = [[w["id"], w["name"], chips(w["owner"]), Raw(ol(w["sequence"])), w["failureMode"], w["idempotency"]] for w in TD["crossContextOrchestrations"]]
    return table(["编号", "编排", "归属", "顺序", "失败模式", "幂等"], rows, "compact")


def inc_td_envelope(a):
    return f'<div class="code-wrap" data-lang="json"><button class="copy-code" type="button">复制</button><pre><code>{html.escape(json.dumps(TD["sharedEventEnvelope"], ensure_ascii=False, indent=2))}</code></pre></div>'


def inc_td_approval_matrix(a):
    return table(["组成部分", "签核人", "所需证据", "状态"], [[x["component"], x["approver"], x["evidence"], tag(x["status"])] for x in TD["approvalMatrix"]])


def inc_td_tests(a):
    tid = "td-tests"
    rows = [[t["id"], chips(t["context"]), t["aggregate"], code(t["command"]), t["given"], t["assertion"], t["class_"], tag(t["status"])] for t in TD["testCatalog"]]
    return filter_bar(tid, "按编号、上下文或断言过滤", len(rows)) + table(["编号", "上下文", "聚合", "命令", "前提", "断言", "类别", "状态"], rows, "compact", tid)


def inc_ec_kpis(a):
    return kpis([(len(command_rows()), "候选写命令"), (len(event_rows()), "集成事件类型"), (len(ERRORS["codes"]), "错误码"), (len(CM["contexts"]), "PostgreSQL schema（brant_<bc>）")])


def inc_ec_commands(a):
    tid = "ec-cmd"
    rows = [[Raw(chip(r[0])), code(r[1]), r[2], r[3], code(r[4]), r[5]] for r in command_rows()]
    return filter_bar(tid, "按上下文、命令或 Python 名称过滤", len(rows)) + table(["上下文", "命令", "执行身份", "Python 对应", "候选 HTTP 路由", "阶段"], rows, "compact", tid)


def inc_ec_events(a):
    tid = "ec-evt"
    rows = [[Raw(chip(r[0])), r[1], code(r[2]), code(r[3]), r[4], r[5]] for r in event_rows()]
    return filter_bar(tid, "按上下文或事件过滤", len(rows)) + table(["上下文", "事件", "eventType", "source", "聚合", "阶段"], rows, "compact", tid)


def inc_ec_errors(a):
    tid = "ec-err"
    rows = [[code(c[0]), c[1], c[2], c[3], chips(c[4]), c[5]] for c in ERRORS["codes"]]
    return filter_bar(tid, "按错误码、类别或上下文过滤", len(rows)) + table(["错误码", "类别", "HTTP", "可重试", "上下文", "含义"], rows, "compact", tid)


def inc_ec_schemas(a):
    rows = [[Raw(chip(c["id"])), code(f"brant_{c['id'].lower()}"), code(f"migrations/{c['id'].lower()}"), code(f"idp-brant.{c['id'].lower()}"), "、".join(c["aggregates"]), c["stage"]] for c in CM["contexts"]]
    rows += [[Raw(tag("bento", "")), code("bento"), "由 idp-bento-go 发布", "—", "Outbox、Inbox、框架技术表", "M0"],
             [Raw(tag("proc", "")), code("brant_proc"), code("migrations/proc"), "—", "无单一 Owner 的流程状态（按需）", "按需"],
             [Raw(tag("ref", "")), code("brant_ref"), code("migrations/ref"), "—", "外部机构代码表（只读）", "待决定"]]
    return table(["上下文", "schema", "迁移目录", "事件来源", "拥有的聚合表", "阶段"], rows, "compact")


def inc_ec_legacy(a):
    rows = [[code(x["target"]), x["size"], code(x["sha256"][:16] + "…")] for x in COPIED if "03_PYTHON_LEGACY_CONTRACTS" in x["target"]]
    return table(["归档文件", "字节", "SHA-256（前 16 位）"], rows, "compact")


def inc_py_kpis(a):
    summary = RERUN.get("summary", "未执行")
    return kpis([(PYSUM["sourceModules"], f"Python 源模块（{PYSUM['sourceLines']:,} 行）"), (PYSUM["testMethodsStatic"], f"唯一测试方法（{PYSUM['testFiles']} 个文件）"), ("679 OK" if "679" in summary and "OK" in summary else summary, "本次重跑 unittest 结果"), (PYSUM["historicalEntries"], "历史版本目录与 ZIP")])


def inc_py_rerun(a):
    if not RERUN:
        return '<div class="callout warn"><strong>未找到重跑证据</strong><p>05_EVIDENCE/python_v050_unittest_rerun.json 不存在。</p></div>'
    return (
        '<div class="callout evidence"><strong>本次实测（E2 · Python 参考实现）</strong>'
        f'<p>{e(RERUN["started_at"])} 至 {e(RERUN["finished_at"])}，把 <code>{e(RERUN["source"])}</code>（SHA-256 {e(RERUN["source_sha256"][:16])}…）解压到 <code>{e(RERUN["copy"])}</code>，按包内锁文件建立环境（{e(RERUN["environment"])}，{e(RERUN["python"])}），执行 <code>{e(RERUN["command"])}</code>：<b>{e(RERUN["summary"])}</b>，退出码 {e(RERUN["exit_code"])}。ZIP 本身未被修改。</p>'
        "<p>这证明 Python V0.50 参考行为在本机可复现；它不是 Go 实现、PostgreSQL 16、真实身份或生产集成的证据。</p></div>"
    )


def inc_py_lines(a):
    rows = [[chips(k), v] for k, v in PYSUM["linesByTargetContext"].items()]
    return table(["目标上下文", "Python 源码行数"], rows, "compact")


def inc_py_modules(a):
    p = DOCS / "07_ENGINEERING_BASELINE_REVIEW/02_Python_Module_Inventory_V0.50.csv"
    with p.open(encoding="utf-8-sig") as f:
        rows = list(csv.reader(f))[1:]
    tid = "py-mod"
    body = [[code(r[0]), r[1], r[4], chips(r[5]), r[6]] for r in rows]
    return filter_bar(tid, "按模块、切片或目标上下文过滤", len(body)) + table(["Python 模块", "行数", "切片", "目标上下文", "Go 目标位置"], body, "compact", tid)


def inc_py_issues(a):
    tid = "py-iss"
    rows = [[x["id"], tag(x["severity"], "no" if x["severity"] == "High" else ("partial" if x["severity"] == "Medium" else "cand")), x["area"], x["finding"], x["impact"], x["goAction"], x["ref"]] for x in REVIEW["pythonIssues"]]
    return filter_bar(tid, "按编号、等级或领域过滤", len(rows)) + table(["编号", "等级", "领域", "发现", "影响", "Go 处置", "依据"], rows, "compact", tid)


def inc_py_tables(a):
    rows = [[code(r[0]), r[1], chips(r[2]), r[3], r[4]] for r in TABLES["rows"]]
    return table(["Python 表", "定义位置", "Go 目标", "聚合", "说明"], rows, "compact")


def _mapping_rows():
    p = DOCS / "08_ENGINEERING_READINESS/02_Python_Test_to_Go_Mapping_V0.1.csv"
    with p.open(encoding="utf-8-sig") as f:
        return list(csv.DictReader(f))


def inc_rd_kpis(a):
    rows = _mapping_rows()
    ported = sum(1 for r in rows if r["go_status"] != "NOT_PORTED")
    ctxs = {r["target_context"] for r in rows}
    return kpis([(len(rows), "唯一 Python 测试方法"), (len({r["python_test_file"] for r in rows}), "测试文件"), (len(ctxs), "映射到的上下文组"), (ported, "已移植到 Go")])


def inc_s1_ported(a):
    rows = [r for r in _mapping_rows() if r["slice"] in ("T01", "共享基础 V0.2")]
    tid = "s1-ported"
    body = [[r["mapping_id"], code(r["python_test_file"] + "::" + r["python_test_method"]), tag(r["go_status"]),
             code(r["target_go_package"]) if r["go_status"] == "PORTED" else "—",
             codes(r["proposed_go_test"].split(";")) if r["go_status"] == "PORTED" else "—"] for r in rows]
    return filter_bar(tid, "按编号、Python 测试、状态或 Go 测试过滤", len(body)) + table(["编号", "Python 测试", "移植状态", "Go 包", "Go 测试"], body, "compact", tid)


def inc_rd_by_context(a):
    rows = [[chips(k), v] for k, v in PYSUM["testMethodsByContext"].items()]
    return table(["目标上下文", "测试方法数"], rows, "compact")


def inc_rd_by_file(a):
    agg = {}
    for r in _mapping_rows():
        k = r["python_test_file"]
        if k not in agg:
            agg[k] = [k, 0, r["slice"], r["target_context"], r["target_aggregate"], r["invariant_refs"]]
        agg[k][1] += 1
    tid = "rd-file"
    rows = [[code(v[0]), v[1], v[2], chips(v[3]), v[4], v[5] or "—"] for v in agg.values()]
    return filter_bar(tid, "按文件、切片、上下文或不变量过滤", len(rows)) + table(["Python 测试文件", "方法数", "切片", "目标上下文", "目标聚合", "对应不变量"], rows, "compact", tid)


def inc_rd_gates(a):
    return table(["编号", "门禁", "集成项", "所需证据", "负责人", "状态"], [[g["id"], g["gate"], g["name"], g["evidence"], g["owner"], tag(g["state"])] for g in REVIEW["integrationGates"]])


def inc_wbs_roadmap(a):
    return figure(a.get("fig", "FIG"), "Go 实施阶段路线（M0、S1–S6）", "每块显示工作项数量与 Python 对等范围；顺序表示依赖，不是排期。", svg.wbs_roadmap(WBS["phases"], WBS["tasks"]))


def inc_wbs_phases(a):
    cnt = Counter(t["phase"] for t in WBS["tasks"])
    return table(["阶段", "名称", "目标", "退出条件", "Python 对等", "工作项"], [[p["id"], p["name"], p["goal"], p["exit"], p["pythonParity"], cnt[p["id"]]] for p in WBS["phases"]])


def inc_wbs_tasks(a):
    ph = a.get("phase")
    rows = [[t["id"], t["domain"], t["title"], t["priority"], "、".join(t["dependencies"]) or "—", t["deliverable"], t["acceptance"], t["owner"], t["pythonParity"] or "—", tag(t["status"])] for t in WBS["tasks"] if not ph or t["phase"] == ph]
    return table(["编号", "领域", "工作", "优先级", "依赖", "交付物", "验收", "负责角色", "Python 对等", "状态"], rows, "compact")


def inc_g1_controls(a):
    return table(["编号", "控制项", "要求", "证据", "负责人", "状态", "阻断"], [[c["id"], c["name"], c["requirement"], c["evidence"], c["owner"], tag(c["state"]), "是" if c["blocking"] else "否"] for c in G1["controls"]])


def inc_adr_register(a):
    grp = [g for g in a.get("group", "").split(",") if g]
    ids = [i for i in a.get("ids", "").split(",") if i]
    rows = [[x["id"], x["group"], x["title"], x["decision"], x["owner"], tag(x["status"]), x["stage"]] for x in ADR["adrs"]
            if (not grp or x["group"] in grp) and (not ids or x["id"] in ids)]
    return table(["编号", "分组", "主题", "决策", "负责人", "状态", "阶段"], rows, "compact")


# Chinese labels for the spike evidence, which the Go tests write in English: option, sequence, requirement.
SPIKE_CN = {
    "SC-04-A": ("A：事务外预取", "TRD 读到“可采纳”；CST 否认提交；TRD 激活", "激活被拒绝（否认先提交）"),
    "SC-04-B1": ("B：下游事务内调用守卫", "CST 否认提交；TRD 经守卫激活", "激活被拒绝"),
    "SC-04-B2": ("B：下游事务内调用守卫", "TRD 守卫通过并持有 FOR SHARE；CST 否认开始并等锁；TRD 提交；否认提交", "激活有效（先提交）；否认随后生效并触发风险事件"),
    "SC-04-B3": ("B：下游事务内调用守卫", "CST 否认更新并持有行锁；TRD 守卫等锁；否认提交；守卫重读", "激活被拒绝"),
    "SC-03-NOLOCK": ("先检查后写入，不锁根行", "替代与转单都读到 active；两者都写入并提交", "只有一个成功"),
    "SC-03-ROOTLOCK-replace": ("先以 FOR UPDATE 锁根行", "替代先锁根行；转单等锁；替代提交；转单重读", "只有一个成功"),
    "SC-03-ROOTLOCK-convert": ("先以 FOR UPDATE 锁根行", "转单先锁根行；替代等锁；转单提交；替代重读", "只有一个成功"),
    "SC-03-STRESS-NOLOCK": ("不锁根行", "200 轮无同步的替代与转单竞争，检查与写入之间有 2 ms 处理", "每轮恰好一个成功"),
    "SC-03-STRESS-ROOTLOCK": ("先以 FOR UPDATE 锁根行", "200 轮无同步的替代与转单竞争，检查与写入之间有 2 ms 处理", "每轮恰好一个成功"),
}
SPIKE_WORDS = {"activation": "激活", "denial": "否认", "replace": "替代", "convert": "转单", "other": "另一请求",
               "rounds_with_two_winners": "两个赢家的轮数", "rounds_with_no_winner": "无赢家的轮数",
               "committed": "已提交", "rejected": "被拒绝", "converted": "已转单", "replaced": "已替代", "active": "有效"}


def spike_detail_cn(r) -> str:
    d = r.get("detail", "")
    if not d:
        return "—"
    if m := re.match(r"(\d+) order active on a snapshot denied before activation", d):
        return f"{m.group(1)} 张订单落在激活前已被否认的快照上，只能靠事后的风险事件补救"
    if m := re.search(r"UPDATE returned (\S+) after the activation", d):
        return f"否认在守卫行上等锁，激活方继续执行并提交后，否认的 UPDATE 才返回（{m.group(1)}）"
    if m := re.match(r"final status (\w+) with (\d+) order", d):
        tail = "：已被替代的报价同时被转成订单" if "also converted" in d else ""
        return f"最终状态为{SPIKE_WORDS.get(m.group(1), m.group(1))}，订单 {m.group(2)} 张{tail}"
    return d


def inc_m0_spike(a):
    prefix = a.get("prefix", "")
    verdict = {"consistent": tag("一致", "ok"), "violation": tag("违反", "no")}
    rows = []
    for r in SPIKE["results"]:
        if not r["id"].startswith(prefix):
            continue
        option, sequence, expected = SPIKE_CN.get(r["id"], (r["option"], r["sequence"], r["expected"]))
        outcomes = "；".join(f"{SPIKE_WORDS.get(k, k)}：{SPIKE_WORDS.get(v, v)}" for k, v in r["outcomes"].items())
        rows.append([r["id"], option, sequence, outcomes, expected, verdict.get(r["verdict"], tag(r["verdict"])), spike_detail_cn(r)])
    return table(["场景", "实现方式", "执行顺序", "结果", "业务设计要求", "结论", "说明"], rows, "compact")


def inc_m0_tests(a):
    rows, tot = [], Counter()
    for d, x in sorted(GO.items()):
        rows.append([code(d), x["files"], f'{x["lines"]:,}', x["unit"] or "—", x["integration"] or "—", x["fuzz"] or "—"])
        tot.update(x)
    rows.append([Raw("<b>合计</b>"), tot["files"], f'{tot["lines"]:,}', tot["unit"], tot["integration"], tot["fuzz"]])
    return table(["Go 包目录", "源文件", "行数", "单元测试函数", "PG16 集成测试函数", "模糊测试"], rows, "compact")


def inc_s1_evidence(a):
    return inc_m0_evidence(a, S1_EVIDENCE, S1_EVIDENCE_FILES)


def inc_m0_evidence(a, folder=M0_EVIDENCE, files=M0_EVIDENCE_FILES):
    base = DOCS / folder
    rows = []
    for name in files:
        p = base / name
        if p.exists():
            href = os.path.relpath(p, DOCS / a["_page_dir"])
            rows.append([Raw(f'<a href="{attr(href)}">{html.escape(name)}</a>'), f"{p.stat().st_size:,}", code(sha256(p)[:16])])
    return table(["证据文件", "字节", "SHA-256（前 16 位）"], rows, "compact")


def inc_method_templates(a):
    d = DOCS / "11_DDD_METHODOLOGY/02_METHOD_TEMPLATES"
    rows = []
    for p in sorted(d.glob("*")):
        rows.append([Raw(f'<a href="02_METHOD_TEMPLATES/{attr(p.name)}">{html.escape(p.name)}</a>'), p.stat().st_size])
    return table(["模板文件", "字节"], rows, "compact")


def inc_quality_gates(a):
    q = load("11_DDD_METHODOLOGY/02_METHOD_TEMPLATES/06_DDD_Quality_Gates.json")
    return table(["门禁", "名称", "必需材料", "签核人", "状态"], [[g["id"], g["name"], "、".join(g["must_have"]), g["approver"], tag(g["state"])] for g in q["gates"]])


def inc_rs_layers(a):
    return figure(a.get("fig", "FIG"), "运行时分层与允许的依赖方向", "包之间只能自上而下依赖；上下文之间的横向依赖由白名单单独约束。", svg.runtime_layers())


def inc_rs_packages(a):
    rows = [[Raw(chip(c["id"])), code(f"internal/contexts/{c['id'].lower()}"), code(f"brant_{c['id'].lower()}"), code(f"{c['id'].lower()}api"), TIER_CN[c["tier"]] + ("（条件）" if c["conditional"] else ""), c["stage"]] for c in CM["contexts"]]
    return table(["上下文", "包路径", "schema", "导入别名", "分级", "创建阶段"], rows, "compact")


def inc_rs_whitelist(a):
    return f'<div class="code-wrap"><button class="copy-code" type="button">复制</button><pre><code>{html.escape(chr(10).join(whitelist_lines()))}</code></pre></div>'


def inc_go_alignment(a):
    return table(["主题", "IDP-Cargo 现行", "IDP Brant 候选", "状态"], [[g["topic"], g["cargo"], g["brant"], tag(g["status"])] for g in REVIEW["goAlignment"]])


INCLUDES = {k[4:]: v for k, v in globals().items() if k.startswith("inc_")}


# ------------------------------------------------------------ page render
DOC_CSS = (TOOLS / "assets/brant-doc.css").read_text(encoding="utf-8")
DOC_JS = (TOOLS / "assets/brant-doc.js").read_text(encoding="utf-8")
INDEX_CSS = (TOOLS / "assets/brant-index.css").read_text(encoding="utf-8")
HERO_ART = (
    '<svg class="hero-art" viewBox="0 0 320 320" xmlns="http://www.w3.org/2000/svg" aria-hidden="true">'
    '<g fill="none" stroke="#ffffff" stroke-opacity=".28" stroke-width="1.2"><circle cx="170" cy="160" r="128"/><circle cx="170" cy="160" r="86"/>'
    '<path d="M36 214 C104 128 190 118 292 64"/><path d="M58 250 C128 170 206 158 296 118"/><path d="M110 92 L170 160 L238 104 M170 160 L150 252 M170 160 L262 214"/></g>'
    '<g fill="#9fdcdc" fill-opacity=".85"><circle cx="110" cy="92" r="5"/><circle cx="238" cy="104" r="5"/><circle cx="150" cy="252" r="5"/><circle cx="262" cy="214" r="5"/><circle cx="170" cy="160" r="8"/></g></svg>'
)

DOC_TEMPLATE = """<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>%%TITLE%%</title>%%FAVICON%%<meta name="generator" content="idp-brant-archive-builder"><style>%%CSS%%</style></head>
<body id="top"><div class="progress"></div>
<aside class="sidebar"><div class="brand"><a href="#top" aria-label="IDP Brant">%%SIDEBRAND%%</a><p>AI-NATIVE TRADE OPERATIONS PLATFORM</p></div>
<div class="search-box"><label for="doc-search">全文检索</label><input id="doc-search" placeholder="%%SEARCH%%"><div class="search-results" hidden></div></div>
<nav class="nav-scroll">%%NAV%%</nav><div class="sidebar-foot">%%FOOT%%</div></aside>
<main><div class="topbar"><button class="mobile-menu" aria-expanded="false">目录 ☰</button><span class="crumb"><a href="%%INDEX%%">主档案目录</a> ／ %%SHORT%%</span><div class="actions"><button id="expand-all">展开／收起</button><button id="print-doc">打印／保存PDF</button></div></div>
<article class="document"><header class="hero"><p class="kicker">%%KICKER%%</p>%%HEROBRAND%%<p class="doc-title">%%DOCTITLE%%</p><p class="doc-desc">%%DOCDESC%%</p><div class="hero-meta">%%META%%</div>%%ART%%</header>
<div class="document-inner">%%BODY%%<footer class="document-footer"><p class="footer-brand">IDP Brant</p><p>%%FOOTER%%</p><p>本文件属于阶段 %%STAGE%%；全项目导航为 00_START_HERE/00_INDEX.html。G0 客户场景 NO-GO · G1 技术准入 NO-GO。</p></footer></div></article></main>
<script>%%JS%%</script></body></html>
"""


def rel_from(page_dir: str, target: str) -> str:
    return os.path.relpath(DOCS / target, DOCS / page_dir) if page_dir else target


def parse_page(raw: str):
    if not raw.startswith("---\n"):
        raise ValueError("page source needs front matter")
    head, body = raw[4:].split("\n---\n", 1)
    fm = {}
    for ln in head.splitlines():
        if ":" in ln:
            k, v = ln.split(":", 1)
            fm[k.strip()] = v.strip()
    return fm, body


_CH = re.compile(r"^##\s+(?:(\d{2}[A-Z]?)\s+)?(.+?)\s*(?:\{#([\w-]+)\})?\s*$")


def split_chapters(body: str):
    intro, chapters, cur = [], [], None
    for ln in body.split("\n"):
        m = _CH.match(ln) if ln.startswith("## ") else None
        if m:
            cur = [m.group(1) or "", m.group(2), m.group(3) or f"s{len(chapters) + 1}", []]
            chapters.append(cur)
        elif cur is None:
            intro.append(ln)
        else:
            cur[3].append(ln)
    return "\n".join(intro), [(n, t, i, "\n".join(c)) for n, t, i, c in chapters]


_TOKEN = re.compile(r"\{\{=(\w+)\}\}")


def fill_counts(text: str) -> str:
    c = counts()
    return _TOKEN.sub(lambda m: f"{c[m.group(1)]:,}" if isinstance(c[m.group(1)], int) else str(c[m.group(1)]), text)


def fill_tree(obj):
    if isinstance(obj, str):
        return fill_counts(obj)
    if isinstance(obj, list):
        return [fill_tree(x) for x in obj]
    if isinstance(obj, dict):
        return {k: fill_tree(v) for k, v in obj.items()}
    return obj


def render_page(src: Path) -> str:
    fm, body = parse_page(fill_counts(src.read_text(encoding="utf-8")))
    out_rel = fm["output"]
    page_dir = str(Path(out_rel).parent)
    includes = {k: (lambda f: (lambda a: f({**a, "_page_dir": page_dir})))(v) for k, v in INCLUDES.items()}
    intro, chapters = split_chapters(body)
    nav, sections = [], []
    if intro.strip():
        nav.append('<a href="#guide">导读与状态</a>')
        sections.append(f'<section class="baseline-intro" id="guide" data-search-title="导读与状态">{mdlite.render(intro, includes)}</section>')
    for num, title, cid, content in chapters:
        nav.append(f'<a href="#{cid}">{e((num + " ") if num else "")}{e(title)}</a>')
        sections.append(
            f'<section class="chapter" id="{cid}" data-search-title="{attr((num + " " if num else "") + title)}"><header class="chapter-header">'
            f'<span class="chapter-num">{e(num)}</span><div><p class="eyebrow">{e(fm.get("eyebrow", ""))}</p><h2>{e(title)}</h2></div></header>'
            f'<div class="prose">{mdlite.render(content, includes)}</div><a class="chapter-back" href="#top">返回顶部 ↑</a></section>'
        )
    meta = "".join(
        f"<span>{e(k.strip())} <b>{e(v.strip())}</b></span>" for k, v in (p.split("=", 1) for p in fm.get("meta", "").split(";") if "=" in p)
    )
    index_rel = os.path.relpath(DOCS / "00_START_HERE/00_INDEX.html", (DOCS / out_rel).parent)
    english = fm.get("english", "AI-Native Trade Operations Platform")
    if BRAND:
        hero_brand = f'<h1 class="hero-lockup">{brand_img("lockup", "brand-lockup", "IDP Brant — AI-Native Trade Operations Platform")}</h1>'
        if english != "AI-Native Trade Operations Platform":
            hero_brand += f'<p class="english-title">{e(english)}</p>'
        side_brand = brand_img("mark", "brand-lockup", "IDP Brant")
    else:
        hero_brand = f'<h1><small>IDP</small>Brant</h1><p class="english-title">{e(english)}</p>'
        side_brand = '<span class="idp">IDP</span><span class="brand-name">Brant</span>'
    repl = {
        "TITLE": e(fm["title"]), "CSS": DOC_CSS, "SEARCH": attr(fm.get("search", "搜索术语、命令或编号")), "NAV": "".join(nav),
        "FOOT": e(fm.get("foot", "")), "INDEX": attr(index_rel), "SHORT": e(fm.get("short", "")), "KICKER": e(fm.get("kicker", "")),
        "FAVICON": favicon_links(), "SIDEBRAND": side_brand, "HEROBRAND": hero_brand, "DOCTITLE": e(fm.get("doc_title", "")),
        "DOCDESC": e(fm.get("doc_desc", "")), "META": meta, "ART": HERO_ART, "BODY": "".join(sections), "FOOTER": e(fm.get("footer", fm["title"])),
        "STAGE": e(fm.get("stage", "")), "JS": DOC_JS,
    }
    html_out = DOC_TEMPLATE
    for k in ("CSS", "JS"):
        html_out = html_out.replace(f"%%{k}%%", "\x01" + k + "\x01")
    for k, v in repl.items():
        if k not in ("CSS", "JS"):
            html_out = html_out.replace(f"%%{k}%%", v)
    html_out = html_out.replace("\x01CSS\x01", repl["CSS"]).replace("\x01JS\x01", repl["JS"])
    target = DOCS / out_rel
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(html_out, encoding="utf-8")
    return out_rel


# ----------------------------------------------------------------- index
def render_index() -> str:
    A = fill_tree(ARCHIVE)

    def href(t):
        return t if t.startswith(("http", "#")) else os.path.relpath(DOCS / t, DOCS / "00_START_HERE")

    parts = [
        "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">",
        f"<title>{e(A['title'])}</title>{favicon_links()}<style>{INDEX_CSS}</style></head><body>",
        '<header class="hero"><div class="brandline">'
        + (brand_img("mark", "brand-lockup", "IDP Brant") if BRAND else '<span class="idp">IDP</span><span class="name">Brant</span>')
        + f'</div><div class="eyebrow">{e(A["eyebrow"])}</div>',
        f'<h1>{A["h1"]}</h1><p>{e(A["lede"])}</p><div class="pills">' + "".join(f"<span>{e(p)}</span>" for p in A["pills"]) + "</div></header>",
        '<div class="shell"><div class="grid4">' + "".join(f'<div class="stat"><strong>{e(v)}</strong><span>{e(k)}</span></div>' for v, k in A["stats"]) + "</div>",
        f'<div class="alert"><b>当前技术方向：</b>{e(A["alert"])}</div>',
    ]
    for fbox in A["focus"]:
        parts.append(f'<div class="sectionbox" style="border-left-color:{fbox.get("color", "#087f80")}"><h3>{e(fbox["title"])}</h3><p>{e(fbox["body"])}</p>')
        parts.append("".join(f'<a href="{attr(href(h))}">{e(t)}</a>' for t, h in fbox["links"]) + "</div>")
    parts.append('<div class="layout"><main><h2>01–12 · 逐阶段交付目录</h2><div class="stage-grid">')
    for s in A["stages"]:
        more = "".join(f'<a class="more" href="{attr(href(h))}">{e(t)}</a>' for t, h in s.get("more", []))
        parts.append(
            f'<article class="stage"><div class="stage-no">{e(s["seq"])}</div><div><div class="stage-kicker">{e(s["kicker"])}</div><h3>{e(s["title"])}</h3>'
            f'<p>{e(s["desc"])}</p><a href="{attr(href(s["primary"]))}">打开本阶段主文件 →</a>{more}</div></article>'
        )
    parts.append("</div>")
    parts.append(f'<p class="label">辅助资产 · 证据与原始资料</p><div class="sectionbox"><h3>工程证据与来源原件</h3><p>{e(A["auxNote"])}</p>' + "".join(f'<a href="{attr(href(h))}">{e(t)}</a>' for t, h in A["aux"]) + "</div>")
    parts.append(f'<div class="sectionbox"><h3>可核验目录</h3><p>{e(A["verifyNote"])}</p>' + "".join(f'<a href="{attr(href(h))}">{e(t)}</a>' for t, h in A["verify"]) + "</div></main>")
    parts.append('<aside class="side"><h3>建议阅读顺序</h3><ol>' + "".join(f"<li>{e(x)}</li>" for x in A["readingOrder"]) + "</ol>")
    parts.append("<h3>版本选择规则</h3>" + "".join(f"<p>{e(x)}</p>" for x in A["rules"]))
    parts.append(f'<h3>重要提醒</h3><p>{e(A["warning"])}</p></aside></div>')
    parts.append(f'<p class="footer">{e(A["footer"])}</p></div></body></html>')
    out = DOCS / "00_START_HERE/00_INDEX.html"
    out.write_text("".join(parts), encoding="utf-8")
    return "00_START_HERE/00_INDEX.html"


# ------------------------------------------------------------- exports
def export_csvs() -> None:
    write_csv("02_UBIQUITOUS_LANGUAGE/03_UL_Term_Registry_V0.1.csv",
              ["term_id", "context_id", "en", "cn", "definition", "exclusions", "invariant_refs", "source_evidence", "owner", "scope_type", "status", "version"],
              [[t["id"], t["context"], t["en"], t["cn"], t["definition"], t["exclusion"], t["rule"], t["source"], f"{t['context']} Domain Owner", t["scopeType"], t["status"], UL["version"]] for t in UL["terms"]])
    write_csv("03_STRATEGIC_DDD/03_FACT_AUTHORITY/01_IDP_Brant_Fact_Authority_Matrix_V0.1.csv",
              ["id", "fact", "classification", "fact_authority", "write_owner", "consumers", "invariant", "forbidden", "priority"],
              [[f[k] for k in ("id", "fact", "classification", "fact_authority", "write_owner", "consumers", "invariant", "forbidden", "priority")] for f in CM["factAuthority"]])
    write_csv("03_STRATEGIC_DDD/03_FACT_AUTHORITY/02_Context_Relationships_V0.1.csv",
              ["id", "upstream", "downstream", "pattern", "fact", "contract", "authority", "risk", "contract_status", "guard_lock"],
              [[r[k] for k in ("id", "frm", "to", "pattern", "fact", "contract", "authority", "risk", "contractStatus")] + ["yes" if r.get("guardLock") else "no"] for r in CM["relationships"]])
    write_csv("04_DOMAIN_EVENT_STORMING/03_IDP_Brant_Scenario_Event_Catalog_V0.1.csv",
              ["scenario_id", "scenario", "type", "step", "actor", "command", "aggregate", "event", "policy", "hotspot", "evidence"],
              [[s["id"], s["title"], s["type"], st["n"], st["actor"], st["command"], st["aggregate"], st["event"], st["policy"], st["hotspot"], st["evidence"]] for s in ES["scenarios"] for st in s["steps"]])
    write_csv("04_DOMAIN_EVENT_STORMING/05_IDP_Brant_SAP_Process_Reference_V0.1.csv",
              ["id", "line", "sap_process", "sap_area", "brant_contexts", "commands", "events", "status", "note", "follow_up", "refs"],
              [[r["id"], r["line"], r["sapProcess"], r["sapArea"], "；".join(r["contexts"]), "；".join(r["commands"]), "；".join(r["events"]), r["status"], r["note"], r["followUp"], r["refs"]] for r in SAPREF["rows"]])
    write_csv("04_DOMAIN_EVENT_STORMING/07_IDP_Brant_Simulated_G0_Steps_V0.1.csv", ["case_id", "step", "actor", "action", "commands", "events", "fit", "finding"],
              [[c["id"], st["n"], st["actor"], st["action"], "；".join(st["commands"]), "；".join(st["events"]), st["fit"], st["finding"]] for c in SIMG0["cases"] for st in c["steps"]])
    write_csv("10_M0_ENGINEERING_BASELINE/08_IDP_Brant_Simulated_Review_Decisions_V0.1.csv", ["record", "subject", "role", "decision", "outcome", "conditions"],
              [[d["id"], d["subject"], d["role"], d["decision"], d["outcome"], "；".join(d["conditions"])] for d in SIMREV["decisions"]]
              + [["SIGNOFF", x["context"], x["role"], x["decision"], x["scope"], "；".join(x["conditions"])] for x in SIMREV["contextSignoffs"]])
    write_csv("05_TACTICAL_DDD/03_IDP_Brant_State_Transition_Matrix_V0.1.csv",
              ["state_machine", "context", "aggregate", "object", "from", "command", "to", "guard", "actor"],
              [[sm["id"], sm["context"], sm["aggregate"], sm["object"], t["from"], t["command"], t["to"], t["guard"], t["actor"]] for sm in TD["stateMachines"] for t in sm["transitions"]])
    write_csv("05_TACTICAL_DDD/04_IDP_Brant_Aggregate_Invariant_Register_V0.1.csv",
              ["invariant_id", "context", "aggregate", "business_rule", "enforcement", "positive_test", "negative_test", "concurrency_test", "errors", "python_evidence", "ul_ref", "status"],
              [[r["id"], r["context"], r["aggregate"], r["rule"], r["enforcement"], r["positive"], r["negative"], r["concurrency"], r["errors"], r["pythonEvidence"], r["ulRef"], r["status"]] for r in TD["invariantRegister"]])
    write_csv("05_TACTICAL_DDD/05_IDP_Brant_DDD_Reference_Acceptance_Tests_V0.1.csv",
              ["test_id", "context", "aggregate", "command", "given", "assertion", "class", "source", "status"],
              [[t["id"], t["context"], t["aggregate"], t["command"], t["given"], t["assertion"], t["class_"], t["source"], t["status"]] for t in TD["testCatalog"]])
    write_csv("06_ENGINEERING_CONTRACTS/02_CONTRACT_CATALOG/01_Command_Catalog_V0.1.csv", ["context", "command", "actor", "python_equivalent", "candidate_http_route", "stage"], command_rows())
    write_csv("06_ENGINEERING_CONTRACTS/02_CONTRACT_CATALOG/02_Event_Catalog_V0.1.csv", ["context", "event", "event_type", "source", "aggregate", "stage"], event_rows())
    write_csv("06_ENGINEERING_CONTRACTS/02_CONTRACT_CATALOG/03_Error_Catalog_V0.1.csv", ERRORS["columns"], ERRORS["codes"])
    platform_schemas = [
        ["IDP-Bento-Go", "bento", "Bento 框架迁移（brant-migrate 渲染）", "—", "Outbox；Inbox", "M0（已建）"],
        ["foundation/authorization", "brant_auth", "migrations/auth", "—", "AuthorityScope；ActorGrant", "M0（已建）"],
        ["cmd/brant-migrate", "brant_migrations", "迁移台账（brant-migrate 自建）", "—", "—", "M0（已建）"],
    ]
    write_csv("06_ENGINEERING_CONTRACTS/02_CONTRACT_CATALOG/05_PG16_Schema_Ownership_V0.1.csv", ["context", "schema", "migrations", "event_source", "aggregates", "stage"],
              [[c["id"], f"brant_{c['id'].lower()}", f"migrations/{c['id'].lower()}", f"idp-brant.{c['id'].lower()}", "；".join(c["aggregates"]), c["stage"]] for c in CM["contexts"]] + platform_schemas)
    envelope_schema = {
        "$schema": "https://json-schema.org/draft/2020-12/schema",
        "$id": "https://contracts.idp.xyz/brant/integration-event-envelope/1.0.0",
        "title": "IDP Brant integration event envelope (candidate)",
        "type": "object", "additionalProperties": False,
        "required": ["eventId", "eventType", "schemaVersion", "source", "aggregateType", "aggregateId", "aggregateVersion", "partitionKey", "occurredAt", "recordedAt", "actor", "tenantId", "companyId", "correlationId", "causationId", "payloadHash", "payload"],
        "properties": {
            "eventId": {"type": "string", "minLength": 1, "maxLength": 64},
            "eventType": {"type": "string", "pattern": "^idp-brant\\.[a-z]{3}\\.[a-z0-9_]+\\.v[0-9]+$"},
            "schemaVersion": {"type": "string", "pattern": "^[0-9]+\\.[0-9]+\\.[0-9]+$"},
            "source": {"type": "string", "enum": [f"idp-brant.{c['id'].lower()}" for c in CM["contexts"]]},
            "aggregateType": {"type": "string"}, "aggregateId": {"type": "string"}, "aggregateVersion": {"type": "integer", "minimum": 1},
            "partitionKey": {"type": "string", "description": "aggregateType:aggregateId；同一聚合的事件按 aggregateVersion 顺序投递"},
            "businessRevision": {"type": "integer", "minimum": 1}, "businessHash": {"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"},
            "occurredAt": {"type": "string", "format": "date-time"}, "recordedAt": {"type": "string", "format": "date-time"},
            "actor": {"type": "object", "additionalProperties": False, "required": ["kind", "id"], "properties": {"kind": {"enum": ["human", "ai", "system"]}, "id": {"type": "string"}}},
            "tenantId": {"type": "string"}, "companyId": {"type": "string"}, "correlationId": {"type": "string"}, "causationId": {"type": "string"},
            "idempotencyKey": {"type": "string"},
            "task": {"type": "object", "additionalProperties": False, "required": ["taskId", "controlEpoch"], "properties": {"taskId": {"type": "string"}, "controlEpoch": {"type": "integer", "minimum": 0}}},
            "evidenceRefs": {"type": "array", "items": {"type": "string"}, "maxItems": 50},
            "payloadHash": {"type": "string", "pattern": "^sha256:[0-9a-f]{64}$"},
            "payload": {"type": "object", "description": "只含标识与版本，不含法定标识或个人数据；按 eventType 另有封闭 schema"},
        },
    }
    p = DOCS / "06_ENGINEERING_CONTRACTS/02_CONTRACT_CATALOG/04_Integration_Event_Envelope_V0.1.schema.json"
    p.write_text(json.dumps(envelope_schema, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    write_csv("07_ENGINEERING_BASELINE_REVIEW/03_Python_Review_Issue_Register_V0.1.csv", ["id", "severity", "area", "finding", "impact", "go_action", "ref"],
              [[x[k] for k in ("id", "severity", "area", "finding", "impact", "goAction", "ref")] for x in REVIEW["pythonIssues"]])
    write_csv("07_ENGINEERING_BASELINE_REVIEW/04_Python_Table_Alignment_V0.1.csv", ["python_tables", "defined_in", "go_target", "aggregate", "note"], TABLES["rows"])
    write_csv("08_ENGINEERING_READINESS/03_Integration_Gate_Matrix_V0.1.csv", ["id", "gate", "name", "evidence", "owner", "state"],
              [[g[k] for k in ("id", "gate", "name", "evidence", "owner", "state")] for g in REVIEW["integrationGates"]])
    write_csv("09_IMPLEMENTATION_PLAN/03_IDP_Brant_WBS_V0.1.csv", ["id", "phase", "domain", "title", "priority", "dependencies", "deliverable", "acceptance", "owner", "python_parity", "status"],
              [[t["id"], t["phase"], t["domain"], t["title"], t["priority"], ";".join(t["dependencies"]), t["deliverable"], t["acceptance"], t["owner"], t["pythonParity"], t["status"]] for t in WBS["tasks"]])
    write_csv("10_M0_ENGINEERING_BASELINE/04_IDP_Brant_ADR_Register_V0.1.csv", ["id", "group", "title", "decision", "status", "owner", "stage", "evidence"],
              [[x[k] for k in ("id", "group", "title", "decision", "status", "owner", "stage", "evidence")] for x in ADR["adrs"]])
    write_csv("12_GO_FIRST_ALIGNMENT/04_Go_Alignment_Matrix_V0.1.csv", ["topic", "idp_cargo", "idp_brant", "status"], [[g["topic"], g["cargo"], g["brant"], g["status"]] for g in REVIEW["goAlignment"]])
    (DOCS / "12_GO_FIRST_ALIGNMENT/05_Context_Import_Whitelist_V0.1.txt").write_text("\n".join(whitelist_lines()) + "\n", encoding="utf-8")


def copy_sources() -> None:
    """Register the reference copies kept in this archive.

    Nothing is copied from outside the repository. The stage-01 documents are maintained here; the 90 ZIPs and the
    Python legacy contracts are frozen reference copies of the Python V0.50 era.
    """
    plan = [
        ("IDP_Brant_T04_A46采购准备包_V0.50.zip", "90_ORIGINAL_SOURCES/01_IDP_Brant_T04_A46采购准备包_V0.50.zip"),
        ("IDP_Brant_Python_Original_Sources_Collection.zip", "90_ORIGINAL_SOURCES/02_IDP_Brant_Python_Original_Sources_Collection.zip"),
        ("Q01_P0_契约与执行验证包_V0.1.zip", "90_ORIGINAL_SOURCES/04_Q01_P0_契约与执行验证包_V0.1.zip"),
        ("IDP_Brant_共享基础能力实现与验证包_V0.2.zip", "90_ORIGINAL_SOURCES/05_IDP_Brant_共享基础能力实现与验证包_V0.2.zip"),
        ("IDP_Brant_T02_候选契约与交叉验证包_V0.4.zip", "90_ORIGINAL_SOURCES/06_IDP_Brant_T02_候选契约与交叉验证包_V0.4.zip"),
        ("IDP_Brant_T03_正式报价契约与验收设计包_V0.1.zip", "90_ORIGINAL_SOURCES/07_IDP_Brant_T03_正式报价契约与验收设计包_V0.1.zip"),
    ]
    legacy_dirs = (("brant_core/contracts", "01_brant_core_contracts"), ("brant_core/costing_engine/contracts", "02_costing_engine_contracts"), ("t03_candidate/contracts", "03_t03_candidate_contracts"))
    for d, sub in legacy_dirs:
        for p in sorted((DOCS / "06_ENGINEERING_CONTRACTS/03_PYTHON_LEGACY_CONTRACTS" / sub).glob("*")):
            if p.suffix in (".json", ".sql"):
                plan.append((f"{PY_PACKAGE_NAME}/{d}/{p.name}", f"06_ENGINEERING_CONTRACTS/03_PYTHON_LEGACY_CONTRACTS/{sub}/{p.name}"))
    for origin, rel in plan:
        dst = DOCS / rel
        if not dst.exists():
            MISSING_SOURCES.append(rel)
            continue
        COPIED.append({"source": origin, "target": rel, "size": dst.stat().st_size, "sha256": sha256(dst)})
    legacy = [x for x in COPIED if "03_PYTHON_LEGACY_CONTRACTS" in x["target"]]
    lines = ["# Python V0.50 候选契约（遗留参考）", "",
             f"收录自 Python V0.50 参考包 `{PY_PACKAGE_NAME}` 的 brant_core/contracts、brant_core/costing_engine/contracts、t03_candidate/contracts，作为冻结的参考副本。",
             "这些是 Python 实现生成或使用的候选 JSON Schema 与参考 SQL，只作为 Go 契约设计的输入，不是 Brant Go 的已发布契约。", "",
             "| 文件 | 字节 | SHA-256 |", "|---|---|---|"]
    lines += [f"| {x['target'].split('03_PYTHON_LEGACY_CONTRACTS/')[1]} | {x['size']} | {x['sha256']} |" for x in legacy]
    (DOCS / "06_ENGINEERING_CONTRACTS/03_PYTHON_LEGACY_CONTRACTS/README.md").write_text("\n".join(lines) + "\n", encoding="utf-8")
    origin = [x for x in COPIED if x["target"].startswith("90_")]
    lines = ["# 90 · 原始资料", "",
             "本目录保存 IDP Brant 进入本主档案之前的关键 Python 时期资料包，作为冻结的参考副本；Python V0.50 参考包即 01 号 ZIP，07／08 阶段的盘点由它生成。",
             "01 阶段的产品范围基线、业务核心详细设计与补充规格在本仓库维护，不在此登记。",
             "完整的 50 多个历史版本目录与 ZIP 不收录，只在 `03_Historical_Package_Register.csv` 登记名称、大小与 SHA-256。", "",
             "| 归档位置 | 字节 | SHA-256 | 原名 |", "|---|---|---|---|"]
    lines += [f"| {x['target']} | {x['size']} | {x['sha256']} | {x['source']} |" for x in origin]
    lines += ["", "提醒：这些资料中的测试通过数只适用于对应 Python 包与本地 SQLite，不代表 Go 实现、PostgreSQL 16 或生产集成。"]
    (DOCS / "90_ORIGINAL_SOURCES/README.md").write_text("\n".join(lines) + "\n", encoding="utf-8")


# ------------------------------------------------------------ registry
def counts() -> dict:
    return {
        "ulTerms": len(UL["terms"]), "ulBusinessTerms": sum(1 for t in UL["terms"] if t["scopeType"] == "business"),
        "ulInvariants": len(UL["invariants"]), "contexts": len(CM["contexts"]), "coreContexts": sum(1 for c in CM["contexts"] if c["tier"] == "Core"),
        "relationships": len(CM["relationships"]), "factAuthority": len(CM["factAuthority"]), "strategicAdrs": len(CM["adrs"]),
        "eventStormingScenarios": len(ES["scenarios"]), "aggregates": sum(len(c["aggregates"]) for c in TD["contexts"]),
        "stateMachines": len(TD["stateMachines"]), "invariantRegister": len(TD["invariantRegister"]), "referenceTests": len(TD["testCatalog"]),
        "commands": len(command_rows()), "events": len(event_rows()), "errorCodes": len(ERRORS["codes"]),
        "pythonSourceModules": PYSUM["sourceModules"], "pythonSourceLines": PYSUM["sourceLines"], "pythonUniqueTests": PYSUM["testMethodsStatic"],
        "pythonUnittestRerun": RERUN.get("summary", ""), "goTestsPorted": sum(1 for r in _mapping_rows() if r["go_status"] == "PORTED"), "wbsTasks": len(WBS["tasks"]), "adrs": len(ADR["adrs"]),
        "g1Controls": len(G1["controls"]),
        "sapReferenceRows": len(SAPREF["rows"]), "sapReferenceGaps": sum(1 for r in SAPREF["rows"] if r["status"] == "GAP"),
        "sapReferencePartial": sum(1 for r in SAPREF["rows"] if r["status"] == "PARTIAL"),
        "goPackages": len(GO), "goSourceLines": sum(x["lines"] for x in GO.values()),
        "goTestFunctions": sum(x["unit"] + x["integration"] for x in GO.values()),
        "goIntegrationTests": sum(x["integration"] for x in GO.values()), "goFuzzTargets": sum(x["fuzz"] for x in GO.values()),
        "m0DoneLocal": sum(1 for t in WBS["tasks"] if t["phase"] == "M0" and t["status"] == "DONE_LOCAL"),
        "m0Tasks": sum(1 for t in WBS["tasks"] if t["phase"] == "M0"),
    }


def write_registry(pages: list[str]) -> None:
    reg = {
        "archive_version": ARCHIVE_VERSION, "as_of": ARCHIVE_DATE, "product": "IDP Brant · AI-Native Trade Operations Platform（中国 B2B 出口业务版）",
        "standard": "IDP-Cargo 主档案 V2.9（01–12 阶段、证据分级、门禁与 Go-first 工程基线）",
        "implementation_language": "Go (Golang)",
        "go_repository": "idp-brantgo（根目录 Go Module go.idp.xyz/idp-brant；私有远程仓库 idpxyz/idp-brantgo，GitHub Actions 通过；M0 完成，S1 部分完成；未配置受保护主干）",
        "status": "DESIGN_CANDIDATE_ARCHIVE_WITH_M0_BASELINE_AND_S1_PARTIAL", "production_ready": False,
        "gates": G1["productGates"], "gate_mapping": G1["gateMapping"],
        "versioning_policy": "Latest means the most recent design candidate, not approved or implemented",
        "stages": [{"seq": s["seq"], "folder": s["folder"], "stage": s["title"], "latest_candidate": s["kicker"], "summary": s["desc"], "status": s["status"], "primary_file": s["primary"]} for s in fill_tree(ARCHIVE)["stages"]],
        "technical_baseline": {
            "domain_backend": "Go 1.26.5（go.mod toolchain 指令，与 idp-bento-go 和 CI 一致；本机另以 go1.26.8 复跑）",
            "runtime_approach": "Modular Monolith + Hexagonal Architecture; 18 candidate bounded contexts are not deployment units",
            "framework": "go.idp.xyz/idp-bento-go@v0.1.0-rc.2（go.mod 精确引用，h1 与记录一致）", "platform": "go.idp.xyz/idp-platform-go@v0.1.0-rc.1（编号能力是否采用待 IG-09）",
            "database": "PostgreSQL 16.12（CI 钉住的 digest）；已建 bento、brant_auth、brant_migrations；brant_<bc> 随 S1；brant_proc、brant_ref 候选",
            "data_access": "pgx/v5（M0 使用）；sqlc 候选",
            "auth": "本地授权投影与 authority_epoch 守卫（brant_auth，M0 实测）；OIDC 随 S1", "events": "Transactional Outbox（brant-worker 中继，M0 实测）；Inbox 随 S4；idp-brant.<bc> sources",
            "decimal": "标准库 math/big（ADR-BR-REPO-10 建议，待裁决）",
            "python_reference": "IDP_Brant_T04_A46采购准备_V0.50 — behaviour reference only; 679 unittest rerun OK on a copy extracted from the 90 ZIP (E2, Python)",
            "blocking_decisions": [{"id": x["id"], "status": x["status"], "evidence": x["evidence"]} for x in ADR["adrs"] if x["id"] in ("ADR-BR-004", "ADR-BR-011", "ADR-BR-REPO-10")],
        },
        "m0": {
            "report": M0_REPORT, "evidence": M0_EVIDENCE, "evidence_level": "E2 本机执行；无远程 CI 记录",
            "result": f"M0 {counts()['m0DoneLocal']}／{counts()['m0Tasks']} 项本机完成，其余 PARTIAL；本机 make ci（Go 1.26.5 与 1.26.8）通过",
            "spike": {r["id"]: r["verdict"] for r in SPIKE["results"]},
        },
        "brand": {
            "kit": "brand/（仓库根目录）IDP Brant Logo engineering V1.2：视觉已锁定，生产母版尚未授权",
            "embedded": [{"file": f"brand/{rel}", "sha256": sha256(BRAND_DIR / rel)} for rel, _, _ in BRAND_FILES.values()] if BRAND else [],
            "use": "页面主视觉用白色完整组合，侧栏与首页用白色无副标题组合，favicon 用深色方形图标 32／64 px；原样内联为 data URI，不改色、不缩裁、不重绘",
            "not_authorized": "方形图标只是预览，不是正式签发的 favicon；SVG 描摹不是矢量母版，未使用",
        },
        "counts": counts(), "generated_pages": pages,
        "notes": [
            "本主档案按 IDP-Cargo 主档案的结构与方法组织，内容来自 Brant 产品基线 V1.0、业务核心详细设计 V0.2 与 Python V0.50。",
            "全部 DDD 资产为候选设计，未经业务或技术签核。",
            "V1.1 回写 M0 工程基线的本机证据：Go 模块、kernel、foundation、迁移、依赖检查、Outbox 中继与一致性实测；尚无业务上下文。",
            "V1.3 远程 CI 跑通并回写 S1 第一批：命令账本、EVD、PTY、CAT、INQ、HTTP 入口与 OIDC、Outbox 死信；POL、主体筛查与受保护主干未完成。",
            "Python 测试通过数与 M0 的 Go 测试都不计入业务验收；G0、G1 仍为 NO-GO。",
        ],
    }
    (DOCS / "00_START_HERE/02_VERSION_REGISTRY.json").write_text(json.dumps(reg, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")


# ----------------------------------------------------------- validation
def validate(pages: list[str]) -> list[dict]:
    res: list[dict] = []

    def check(name, bad, detail_ok=""):
        res.append({"check": name, "result": "PASS" if not bad else "FAIL", "detail": detail_ok if not bad else "；".join(map(str, bad[:20]))})

    bad = []
    for p in DOCS.rglob("*.json"):
        if "__pycache__" in p.parts:
            continue
        try:
            json.loads(p.read_text(encoding="utf-8"))
        except Exception as ex:  # noqa: BLE001
            bad.append(f"{p.relative_to(DOCS)}: {ex}")
    check("全部 JSON 可解析", bad, f"{sum(1 for _ in DOCS.rglob('*.json'))} 个 JSON")
    ids = set(CTX)
    ul_inv = {i["id"] for i in UL["invariants"]}
    term_ids = [t["id"] for t in UL["terms"]]
    check("UL 词条 ID 唯一", [i for i, n in Counter(term_ids).items() if n > 1], f"{len(term_ids)} 个词条")
    check("UL 词条上下文均已登记", [t["id"] for t in UL["terms"] if t["context"] not in ids | {"PLATFORM", "AI", "KERNEL"}])
    check("UL 词条规则引用存在", [t["id"] for t in UL["terms"] if t["rule"] and t["rule"] not in ul_inv])
    check("UL 命令上下文均已登记", [c["command"] for c in UL["commands"] if c["context"] not in ids])
    check("关系两端均为已登记上下文", [r["id"] for r in CM["relationships"] if r["frm"] not in ids or r["to"] not in ids], f"{len(CM['relationships'])} 条关系")
    check("关系无重复（上游、下游）", [k for k, n in Counter((r["frm"], r["to"]) for r in CM["relationships"]).items() if n > 1])
    check("上下文引用的 UL 不变量存在", [f"{c['id']}:{i}" for c in CM["contexts"] for i in c["invariants"] if i not in ul_inv])
    check("子域覆盖全部上下文", sorted(ids - {c for s in CM["subdomains"] for c in s["contexts"]}))
    td_ids = {c["id"] for c in TD["contexts"]}
    check("战术模型覆盖全部上下文", sorted(ids ^ td_ids))
    reg = {r["id"] for r in TD["invariantRegister"]}
    check("聚合引用的不变量已登记", [f"{a['id']}:{i}" for c in TD["contexts"] for a in c["aggregates"] for i in a["invariants"] if i not in reg])
    check("不变量登记的 UL 引用存在", [r["id"] for r in TD["invariantRegister"] if r["ulRef"] and r["ulRef"] not in ul_inv])
    check("状态机转换的状态均已声明", [f"{sm['id']}:{t['from']}->{t['to']}" for sm in TD["stateMachines"] for t in sm["transitions"] if t["from"] not in sm["states"] or t["to"] not in sm["states"]])
    check("状态机布局覆盖全部状态", [f"{sm['id']}:{s}" for sm in TD["stateMachines"] for s in sm["states"] if s not in sm["layout"]])
    check("事件风暴上下文均已登记", [f"{s['id']}:{c}" for s in ES["scenarios"] for c in s["contextFlow"] if c not in ids])
    tids = {t["id"] for t in WBS["tasks"]}
    check("WBS 依赖均存在", [f"{t['id']}->{d}" for t in WBS["tasks"] for d in t["dependencies"] if d not in tids], f"{len(tids)} 项工作")
    check("WBS 阶段均已定义", [t["id"] for t in WBS["tasks"] if t["phase"] not in {p["id"] for p in WBS["phases"]}])
    # cross-document semantics
    cm_cmds = {cmd for c in CM["contexts"] for cmd in c["commands"]}
    td_cmds = {cmd for c in TD["contexts"] for g in c["aggregates"] for cmd in g["commands"]}
    check("人工专属命令均已登记为上下文命令", sorted(HUMAN_ONLY - cm_cmds - td_cmds), f"{len(HUMAN_ONLY)} 条人工专属命令")
    acts = sm_actors()
    check("状态机中只由人工执行的命令均列入人工专属清单", sorted(cmd for cmd, a in acts.items() if a == {"human"} and cmd not in HUMAN_ONLY))
    check("人工专属命令在状态机中不由 AI 或系统执行", sorted(cmd for cmd in HUMAN_ONLY if acts.get(cmd, set()) - {"human"}))
    rel_pairs = {(r["frm"], r["to"]) for r in CM["relationships"]}
    bad = []
    for f in CM["factAuthority"]:
        writers = set(_CODE_RE.findall(f["write_owner"]))
        if len(writers) > 1:
            bad.append(f"{f['id']}: 写入方 {f['write_owner']}")
            continue
        if not writers:  # foundation or other non-context owner
            continue
        w = writers.pop()
        bad += [f"{f['id']}: {w}→{c}" for c in sorted(set(_CODE_RE.findall(f["consumers"])) - {w}) if (w, c) not in rel_pairs]
    check("事实权威恰有一个写入方且消费方均有上下游关系", bad, f"{len(CM['factAuthority'])} 条事实")
    td_aggs = {c["id"]: {g["name"] for g in c["aggregates"]} for c in TD["contexts"]}
    check("Context Map 聚合均在战术模型登记", [f"{c['id']}:{g}" for c in CM["contexts"] for g in c["aggregates"] if g not in td_aggs.get(c["id"], set())])
    check("集成事件均有生产聚合", [f"{r[0]}:{r[1]}" for r in event_rows() if r[4] == "—"], f"{len(event_rows())} 个集成事件")
    err_codes = {c[0] for c in ERRORS["codes"]}
    code_re = re.compile(r"\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b")
    texts = [(r["id"], " ".join(r[k] for k in ("errors", "positive", "negative", "concurrency"))) for r in TD["invariantRegister"]]
    texts += [(t["id"], t["given"] + " " + t["assertion"]) for t in TD["testCatalog"]]
    texts += [(s["id"], s["failure"] + " " + s["accept"]) for s in CM["scenarios"]]
    texts += [(s["id"], " ".join([st["event"] + " " + st["policy"] for st in s["steps"]] + s["counterexamples"])) for s in ES["scenarios"]]
    check("不变量、验收与场景引用的错误码均已登记", sorted({f"{i}:{m}" for i, t in texts for m in code_re.findall(t) if m not in err_codes}), f"{len(err_codes)} 个错误码")
    cyc = guard_cycle()
    check("守卫锁边无环（下游以 FOR SHARE 锁上游守卫）", [" → ".join(cyc)] if cyc else [], f"{sum(1 for r in CM['relationships'] if r.get('guardLock'))} 条守卫锁边")
    all_events = {ev for c in CM["contexts"] for ev in c["facts"]} | {ev for c in TD["contexts"] for g in c["aggregates"] for ev in g.get("events", [])}
    lines_ok = {ln["id"] for ln in SAPREF["lines"]}
    bad = [f"{i}: 重复" for i, n in Counter(r["id"] for r in SAPREF["rows"]).items() if n > 1]
    for r in SAPREF["rows"]:
        if r["line"] not in lines_ok or r["status"] not in SAPREF["statuses"]:
            bad.append(f"{r['id']}: 主线或状态未定义")
        bad += [f"{r['id']}: 上下文 {c}" for c in r["contexts"] if c not in ids]
        bad += [f"{r['id']}: 命令 {c}" for c in r["commands"] if c not in cm_cmds | td_cmds]
        bad += [f"{r['id']}: 事件 {ev}" for ev in r["events"] if ev not in all_events]
        if r["status"] in ("COVERED", "PARTIAL", "CONDITIONAL", "LOCAL") and not r["commands"]:
            bad.append(f"{r['id']}: {r['status']} 须引用命令")
        if r["status"] in ("PARTIAL", "GAP") and not r["followUp"]:
            bad.append(f"{r['id']}: {r['status']} 须写后续处理")
        if r["status"] in ("DEVIATION", "OUT_OF_SCOPE") and not (r["note"] and r["refs"]):
            bad.append(f"{r['id']}: {r['status']} 须写理由与依据")
    check("SAP 流程参照引用的上下文、命令与事件均存在且状态完整", bad, f"{len(SAPREF['rows'])} 行参照")
    finding_ids = {f["id"] for f in SIMG0["findings"]}
    wbs_ids = {t["id"] for t in WBS["tasks"]}
    role_ids = {r["id"] for r in SIMREV["roles"]}
    bad = []
    for c in SIMG0["cases"]:
        for st in c["steps"]:
            tagid = f"{c['id']}#{st['n']}"
            bad += [f"{tagid}: 命令 {x}" for x in st["commands"] if x not in cm_cmds | td_cmds]
            bad += [f"{tagid}: 事件 {x}" for x in st["events"] if x not in all_events]
            if st["fit"] not in SIMG0["fits"] or (st["fit"] != "FIT" and st["finding"] not in finding_ids) or (st["fit"] != "GAP" and not st["commands"]):
                bad.append(f"{tagid}: 吻合度、发现或命令不完整")
    bad += [f"{f['id']}: WBS {f['wbs']}" for f in SIMG0["findings"] if f["wbs"] not in wbs_ids]
    bad += [f"{f['id']}: 角色 {f['owner']}" for f in SIMG0["findings"] if f["owner"] not in role_ids]
    check("模拟 G0 走查引用的命令、事件、发现与 WBS 均存在", bad, f"{sum(len(c['steps']) for c in SIMG0['cases'])} 步，{len(SIMG0['findings'])} 项发现")
    adr_ids = {x["id"] for x in ADR["adrs"]}
    dec_subjects = [d["subject"] for d in SIMREV["decisions"]]
    bad = sorted(adr_ids ^ set(dec_subjects)) + [f"{k}: 重复" for k, n in Counter(dec_subjects).items() if n > 1]
    bad += [f"{d['id']}: 角色或结论" for d in SIMREV["decisions"] if d["role"] not in role_ids or d["decision"] not in SIMREV["decisionKinds"]]
    bad += sorted(ids ^ {x["context"] for x in SIMREV["contextSignoffs"]})
    sim_status = {d["subject"]: d["decision"] for d in SIMREV["decisions"]}
    bad += [f"{x['id']}: 模拟评审 {sim_status[x['id']]} 与状态 {x['status']} 不一致" for x in ADR["adrs"]
            if (sim_status[x["id"]] in ("ACCEPTED", "ACCEPTED_WITH_CONDITIONS")) != x["status"].startswith("ACCEPTED")]
    check("模拟评审覆盖全部 ADR 与上下文且状态一致", bad, f"{len(SIMREV['decisions'])} 项裁决，{len(SIMREV['contextSignoffs'])} 个上下文")
    reg_status = {x["id"]: x["status"] for x in ADR["adrs"]}
    check("战略 ADR 状态与 ADR 登记一致", [f"{x['id']}: {x['status']}≠{reg_status[x['id']]}" for x in CM["adrs"] if x["id"] in reg_status and not x["status"].startswith(reg_status[x["id"]])])
    mapping = _mapping_rows()
    check("Python 测试映射覆盖全部唯一测试方法", [] if len(mapping) == PYSUM["testMethodsStatic"] else [f"{len(mapping)}≠{PYSUM['testMethodsStatic']}"], f"{len(mapping)} 行")
    check("Python 测试映射均有目标上下文", [r["mapping_id"] for r in mapping if r["target_context"] in ("", "—")])
    check("映射引用的不变量已登记", sorted({i for r in mapping for i in r["invariant_refs"].split("；") if i and i not in reg}))
    # links and anchors in generated pages
    bad_links, bad_anchor = [], []
    for rel in pages + ["00_START_HERE/00_INDEX.html"]:
        p = DOCS / rel
        text = re.sub(r"<script>.*?</script>", "", p.read_text(encoding="utf-8"), flags=re.S)
        anchors = set(re.findall(r'id="([^"]+)"', text))
        for href in re.findall(r'href="([^"]+)"', text):
            if href.startswith(("http://", "https://", "mailto:", "data:")):
                continue
            if href.startswith("#"):
                if href[1:] and href[1:] not in anchors:
                    bad_anchor.append(f"{rel}{href}")
                continue
            target = (p.parent / href.split("#")[0]).resolve()
            if not target.exists():
                bad_links.append(f"{rel} → {href}")
    check("生成页面的本地链接均可解析", bad_links, f"{len(pages) + 1} 个页面")
    check("生成页面的锚点均存在", bad_anchor)
    check("参考副本均已收录", MISSING_SOURCES, f"{len(COPIED)} 个参考副本（90 阶段资料包与 Python 候选契约）")
    check("M0 证据文件齐全", [f for f in M0_EVIDENCE_FILES if not (DOCS / M0_EVIDENCE / f).exists()], f"{len(M0_EVIDENCE_FILES)} 个文件")
    check("S1 证据文件齐全", [f for f in S1_EVIDENCE_FILES if not (DOCS / S1_EVIDENCE / f).exists()], f"{len(S1_EVIDENCE_FILES)} 个文件")
    got = {r["id"]: r["verdict"] for r in SPIKE["results"]}
    check("M0 一致性实测结论与报告一致", [f"{k}: {got.get(k)}≠{v}" for k, v in SPIKE_EXPECTED.items() if got.get(k) != v] + sorted(set(got) - set(SPIKE_EXPECTED)),
          f"{len(got)} 个场景")
    check("M0 实测场景均有中文说明", sorted(set(got) - set(SPIKE_CN)) + [f"{r['id']}: {k}" for r in SPIKE["results"] for k in list(r["outcomes"]) + list(r["outcomes"].values()) if k not in SPIKE_WORDS and not k.isdigit()])
    # Evidence was produced from the sources listed in the manifest; code that changed since then needs new evidence.
    # The latest evidence snapshot is the one the current code must match: S1 once it exists, else M0.
    drift = []
    manifest = DOCS / S1_EVIDENCE / "02_go_source_manifest.txt"
    if not manifest.exists():
        manifest = DOCS / M0_EVIDENCE / "02_go_source_manifest.txt"
    if manifest.exists():
        for ln in manifest.read_text(encoding="utf-8").splitlines():
            digest, rel = ln.split(None, 1)
            if not re.search(r"(\.go|\.sql|go\.mod|go\.sum|Makefile|\.yml|\.sh)$", rel):
                continue
            p = GO_REPO / rel
            if not p.exists() or sha256(p) != digest:
                drift.append(rel)
        listed = {ln.split(None, 1)[1] for ln in manifest.read_text(encoding="utf-8").splitlines()}
        drift += [f"新增 {d}/{n}" for d in GO for n in [p.name for p in (GO_REPO / d).glob("*.go")] if f"{d}/{n}" not in listed]
    else:
        drift.append("缺少 02_go_source_manifest.txt")
    check("最新证据快照对应当前 Go 源码", drift, f"{sum(x['files'] for x in GO.values())} 个 Go 文件与 {manifest.parent.name} 快照一致")
    if BRAND:
        qa = json.loads((BRAND_DIR / "05_QA/sha256_manifest.json").read_text(encoding="utf-8"))
        bad = [rel for rel, _, _ in BRAND_FILES.values() if qa.get(rel) != sha256(BRAND_DIR / rel)]
        bad += [rel for rel in pages + ["00_START_HERE/00_INDEX.html"] if 'class="brand-lockup"' not in (DOCS / rel).read_text(encoding="utf-8")]
        check("品牌标志与品牌包 QA 清单逐字节一致且各页均已使用", bad, f"{len(BRAND_FILES)} 个品牌文件，{len(pages) + 1} 个页面")
    else:
        check("品牌标志与品牌包 QA 清单逐字节一致且各页均已使用", [], "未找到 brand/，页面使用文字标志")
    return res


def write_manifest() -> int:
    skip = {"03_FILE_MANIFEST.csv", "04_SHA256SUMS.txt"}
    files = sorted(
        p for p in DOCS.rglob("*")
        if p.is_file() and "__pycache__" not in p.parts and "_review_tmp" not in p.parts and p.name not in skip
    )
    rows, sums = [], []
    for p in files:
        rel = p.relative_to(DOCS).as_posix()
        h = sha256(p)
        rows.append([rel, p.stat().st_size, h, rel.split("/")[0], p.suffix.lstrip(".") or "file"])
        sums.append(f"{h}  {rel}")
    write_csv("00_START_HERE/03_FILE_MANIFEST.csv", ["path", "size_bytes", "sha256", "stage_folder", "type"], rows)
    (DOCS / "00_START_HERE/04_SHA256SUMS.txt").write_text("\n".join(sums) + "\n", encoding="utf-8")
    return len(files)


def ensure_csv_bom() -> None:
    """Hand-written CSV files get the same UTF-8 BOM as generated ones (Excel compatibility)."""
    for p in DOCS.rglob("*.csv"):
        raw = p.read_bytes()
        if not raw.startswith(b"\xef\xbb\xbf"):
            p.write_bytes(b"\xef\xbb\xbf" + raw)


def main() -> int:
    copy_sources()
    export_csvs()
    ensure_csv_bom()
    pages = [render_page(p) for p in sorted((TOOLS / "pages").glob("*.md"))]
    render_index()
    write_registry(pages)
    results = validate(pages)
    failed = [r for r in results if r["result"] != "PASS"]
    report = {
        "archive_version": ARCHIVE_VERSION, "as_of": ARCHIVE_DATE, "builder": "00_START_HERE/tools/build_archive.py",
        "scope": "结构与交叉引用校验：JSON 可解析、术语／上下文／关系／聚合／不变量／状态机／WBS／测试映射的引用完整性、生成页面的本地链接与锚点；"
                 "M0 与 S1 证据文件齐全、一致性实测结论与报告一致、最新证据快照对应当前 Go 源码；页面内联的品牌标志与品牌包 QA 清单逐字节一致。",
        "not_in_scope": "构建器不执行 Go 测试或 PostgreSQL 16（证据见 10 阶段 06_M0_EVIDENCE 与 09 阶段 05_S1_EVIDENCE）；不验证业务正确性、真实身份提供方或任何外部集成，这些仍为 G1 待办。",
        "result": "PASS" if not failed else "FAIL", "checks": results, "counts": counts(),
    }
    (DOCS / "00_START_HERE/05_VALIDATION_REPORT.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    n = write_manifest()
    for r in results:
        print(f"[{r['result']}] {r['check']} {r['detail']}")
    print(f"pages={len(pages)} files={n} references={len(COPIED)} result={report['result']}")
    return 0 if not failed else 1


if __name__ == "__main__":
    raise SystemExit(main())
