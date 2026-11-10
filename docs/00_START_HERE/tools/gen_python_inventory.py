"""Generate Python V0.50 inventories for stages 07, 08 and 90.

Outputs (paths relative to docs/):
  07_ENGINEERING_BASELINE_REVIEW/02_Python_Module_Inventory_V0.50.csv
  08_ENGINEERING_READINESS/02_Python_Test_to_Go_Mapping_V0.1.csv
  00_START_HERE/tools/data/python_inventory_summary.json

Input: the Python V0.50 package archived in this repository
(90_ORIGINAL_SOURCES/01_IDP_Brant_T04_A46采购准备包_V0.50.zip), extracted into a temporary directory.
90_ORIGINAL_SOURCES/03_Historical_Package_Register.csv is frozen registry data; it is read for the summary
counts and never regenerated.
"""

from __future__ import annotations

import ast
import csv
import json
import re
import tempfile
import zipfile
from collections import Counter, defaultdict
from pathlib import Path

DOCS = Path(__file__).resolve().parents[2]
PACKAGE_ZIP = DOCS / "90_ORIGINAL_SOURCES/01_IDP_Brant_T04_A46采购准备包_V0.50.zip"
PACKAGE_NAME = "IDP_Brant_T04_A46采购准备_V0.50"
REGISTER = DOCS / "90_ORIGINAL_SOURCES/03_Historical_Package_Register.csv"

MODULE_MAP: list[tuple[str, str, str, str]] = [
    # (path prefix, slice, target context, target go package)
    ("brant_core/costing_engine/", "T02", "CST／SUP", "internal/contexts/cst/internal/domain（规则）＋sup/internal/domain（供应条件）"),
    ("brant_core/costing", "T02", "CST", "internal/contexts/cst"),
    ("brant_core/foundation", "共享基础 V0.2", "POL", "internal/contexts/pol"),
    ("brant_core/t03_config.py", "T03 A18–A19", "POL", "internal/contexts/pol/internal/domain/config"),
    ("brant_core/t03_cost_scope.py", "T03 A27", "TRD", "internal/contexts/trd/internal/domain/quote"),
    ("brant_core/t03_sales_terms.py", "T03 A28", "TRD", "internal/contexts/trd/internal/domain/quote"),
    ("brant_core/t03_trade_parties.py", "T03 A29–A31", "TRD", "internal/contexts/trd/internal/domain/quote"),
    ("brant_core/t03_order_", "T03 A25–A26", "TRD", "internal/contexts/trd/internal/domain/order"),
    ("brant_core/t03_reconciliation.py", "T03 A22–A23", "TSK／TRD", "internal/contexts/tsk/internal/domain/action"),
    ("brant_core/t03_query.py", "T03 A8–A16", "TRD", "internal/contexts/trd/internal/application（查询）"),
    ("brant_core/t03_write.py", "T03 A17", "TRD", "internal/contexts/trd/internal/application（命令）"),
    ("brant_core/t03_storage.py", "T03／T04", "TRD／PUR", "internal/contexts/trd/internal/adapters/postgres（按聚合拆分）"),
    ("brant_core/t04_response.py", "T04 A36–A37／A44–A45", "TRD", "internal/contexts/trd/internal/domain/response"),
    ("brant_core/t04_order_review.py", "T04 A38–A43", "TRD", "internal/contexts/trd/internal/domain/order"),
    ("brant_core/t04_purchase.py", "T04 A46", "PUR", "internal/contexts/pur/internal/domain/draft"),
    ("brant_core/service.py", "T01–T04", "多上下文（组合根）", "internal/bootstrap＋各上下文 module.go"),
    ("brant_core/storage.py", "T01", "INQ／EVD／PTY", "internal/contexts/{inq,evd,pty}/internal/adapters/postgres"),
    ("brant_core/common.py", "T01", "kernel／foundation", "internal/kernel＋internal/foundation/identity"),
    ("brant_core/__main__.py", "T01", "transport", "cmd/brant-cli"),
    ("brant_core/__init__.py", "T01", "—", "—"),
    ("t03_candidate/", "T03 候选契约", "TRD（契约）", "contracts/＋tools/contract-check"),
]

TEST_MAP: list[tuple[str, str, str, str, list[str]]] = [
    # (file prefix, slice, context, aggregate, invariant refs)
    ("test_t04_a46", "T04 A46", "PUR", "PurchaseDraft", ["PUR-INV-001", "PUR-INV-002", "PUR-INV-003", "PUR-INV-004", "PUR-INV-005", "PUR-INV-006", "PUR-INV-007"]),
    ("test_t04_a45", "T04 A45", "TRD", "CustomerResponse", ["TRD-INV-011", "X-INV-003"]),
    ("test_t04_a44", "T04 A44", "TRD", "CustomerResponse", ["TRD-INV-011", "X-INV-001"]),
    ("test_t04_a43", "T04 A43", "TRD／TSK", "SalesOrder（对账）", ["TRD-INV-013", "X-INV-003"]),
    ("test_t04_a42", "T04 A42", "TRD", "SalesOrder（查询）", ["TRD-INV-013", "X-INV-003"]),
    ("test_t04_a41", "T04 A41", "TRD", "SalesOrder（公共生命周期）", ["TRD-INV-013", "X-INV-001"]),
    ("test_t04_a40", "T04 A40", "TRD", "SalesOrder（审批撤销）", ["TRD-INV-013"]),
    ("test_t04_a39", "T04 A39", "TRD", "SalesOrder（统一门禁）", ["TRD-INV-013", "TRD-INV-014"]),
    ("test_t04_a38", "T04 A38", "TRD", "SalesOrder（订单审批）", ["TRD-INV-013"]),
    ("test_t04_a37", "T04 A37", "TRD", "CustomerResponse／Acceptance", ["TRD-INV-011", "TRD-INV-012"]),
    ("test_t04_a36", "T04 A36", "TRD", "CustomerResponse", ["TRD-INV-011"]),
    ("test_t03_a35", "T03 A35", "TRD", "QuoteRoot（审批时效）", ["TRD-INV-005"]),
    ("test_t03_a34", "T03 A34", "TRD", "QuoteRoot（就绪审核资格）", ["TRD-INV-004"]),
    ("test_t03_a33", "T03 A33", "TRD", "QuoteRoot（审核人资格）", ["TRD-INV-004"]),
    ("test_t03_a32", "T03 A32", "TRD", "QuoteRoot（销售汇率）", ["POL-INV-003", "TRD-INV-014"]),
    ("test_t03_a31", "T03 A31", "TRD", "QuoteRoot（地址角色）", ["TRD-INV-006"]),
    ("test_t03_a30", "T03 A30", "TRD", "QuoteRoot（联系人档案）", ["TRD-INV-006"]),
    ("test_t03_a29", "T03 A29", "TRD", "QuoteRoot（商业主体）", ["TRD-INV-006"]),
    ("test_t03_a28", "T03 A28", "TRD", "QuoteRoot（销售收费与税）", ["TRD-INV-007", "TRD-INV-008"]),
    ("test_t03_a27", "T03 A27", "TRD", "QuoteRoot（成本范围）", ["TRD-INV-006"]),
    ("test_t03_a26", "T03 A26", "TRD", "SalesOrder（激活资格）", ["TRD-INV-014"]),
    ("test_t03_a25", "T03 A25", "TRD", "SalesOrder（商业快照）", ["TRD-INV-014"]),
    ("test_t03_a24", "T03 A24", "TRD", "SalesOrder（写控制）", ["TRD-INV-010", "TRD-INV-015"]),
    ("test_t03_a23", "T03 A23", "TSK／TRD", "ActionExecution（对账授权）", ["X-INV-003"]),
    ("test_t03_a20", "T03 A20", "TRD", "Acceptance／SalesOrder", ["TRD-INV-010", "TRD-INV-011"]),
    ("test_t03_a19", "T03 A19", "TRD", "QuoteRoot（配置消费与文件）", ["TRD-INV-009"]),
    ("test_t03_a6", "T03 A6", "TRD", "QuoteRoot（组合授权）", ["TRD-INV-003"]),
    ("test_t03_configuration_runtime", "T03 A18", "POL", "ConfigurationRoot（报价配置）", ["POL-INV-001"]),
    ("test_t03_cost_binding", "T03", "TRD", "QuoteRoot（成本绑定）", ["TRD-INV-006"]),
    ("test_t03_formalization", "T03 A4", "TRD", "QuoteRoot（正式化）", ["TRD-INV-001", "TRD-INV-009"]),
    ("test_t03_persistence", "T03 A1–A5", "TRD", "QuoteRoot（持久化）", ["TRD-INV-001", "TRD-INV-016"]),
    ("test_t03_query_contract", "T03 A8", "TRD", "QuoteRoot（查询契约）", ["X-INV-003"]),
    ("test_t03_quote_review", "T03 A3", "TRD", "QuoteRoot（审核）", ["TRD-INV-003", "TRD-INV-004"]),
    ("test_t03_server_calculation", "T03 A5", "TRD", "QuoteRoot（服务端计算）", ["TRD-INV-007", "TRD-INV-008"]),
    ("test_t03_snapshot", "T03", "TRD", "QuoteRoot（商业快照）", ["TRD-INV-002"]),
    ("test_t03_write_contract", "T03 A17", "TRD", "QuoteRoot（写契约）", ["X-INV-001", "X-INV-002"]),
    ("test_costing_lifecycle", "T02", "SUP／CST", "SupplierOffer／CostScenario", ["SUP-INV-001", "SUP-INV-003", "CST-INV-002"]),
    ("test_costing_domains", "T02", "CST", "CostScenario（规则域）", ["CST-INV-003", "CST-INV-005", "CST-INV-006", "CST-INV-007", "SUP-INV-002"]),
    ("test_costing_failures", "T02", "CST", "CostScenario（失败路径）", ["CST-INV-001"]),
    ("test_costing_migration", "T02", "CST", "迁移", []),
    ("test_costing_query", "T02", "CST", "CostSnapshot（查询）", ["X-INV-003"]),
    ("test_costing", "T02", "CST", "CostScenario／CostSnapshot", ["CST-INV-001", "CST-INV-004"]),
    ("test_foundation_migration", "共享基础 V0.2", "POL", "迁移", []),
    ("test_foundation", "共享基础 V0.2", "POL", "ConfigurationRoot", ["POL-INV-001", "POL-INV-002", "POL-INV-003", "POL-INV-004", "POL-INV-005"]),
    ("test_storage", "T01", "EVD／INQ", "Source／CustomerRfq（存储）", ["EVD-INV-001", "INQ-INV-002"]),
    ("test_core", "T01", "INQ／PTY／EVD", "CustomerRfq／Party／Source", ["INQ-INV-001", "INQ-INV-002", "EVD-INV-001", "PTY-INV-001", "X-INV-001", "X-INV-002"]),
    ("test_contracts", "T01", "契约", "JSON Schema", []),
    ("test_cli", "T01", "transport", "CLI", ["INQ-INV-001"]),
]

GO_PKG = {
    "PUR": "internal/contexts/pur", "TRD": "internal/contexts/trd", "TRD／TSK": "internal/contexts/trd",
    "TSK／TRD": "internal/contexts/tsk", "POL": "internal/contexts/pol", "SUP／CST": "internal/contexts/sup",
    "CST": "internal/contexts/cst", "EVD／INQ": "internal/contexts/evd", "INQ／PTY／EVD": "internal/contexts/inq",
    "契约": "tools/contract-check", "transport": "cmd/brant-cli",
}


def camel(name: str) -> str:
    name = re.sub(r"^test_?", "", name)
    return "Test" + "".join(p[:1].upper() + p[1:] for p in re.split(r"[_\W]+", name) if p)


def module_entry(rel: str) -> tuple[str, str, str]:
    for prefix, slc, ctx, pkg in MODULE_MAP:
        if rel.startswith(prefix):
            return slc, ctx, pkg
    return "—", "—", "—"


def test_entry(fname: str) -> tuple[str, str, str, list[str]]:
    for prefix, slc, ctx, agg, inv in TEST_MAP:
        if fname.startswith(prefix):
            return slc, ctx, agg, inv
    return "—", "—", "—", []



def write_csv(path: Path, header: list[str], rows: list[list[object]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f)
        w.writerow(header)
        w.writerows(rows)


def extract_package(dest: Path) -> Path:
    if not PACKAGE_ZIP.exists():
        raise SystemExit(f"Python reference package not found: {PACKAGE_ZIP}")
    with zipfile.ZipFile(PACKAGE_ZIP) as zf:
        for name in zf.namelist():
            if name.startswith("/") or ".." in name.split("/"):
                raise SystemExit(f"unsafe path in {PACKAGE_ZIP.name}: {name}")
            if name.startswith(("brant_core/", "t03_candidate/", "tests/")) and not name.endswith("/"):
                zf.extract(name, dest)
    return dest


def main() -> None:
    with tempfile.TemporaryDirectory(prefix="brant-py-v050-") as tmp:
        build(extract_package(Path(tmp)))


def build(package: Path) -> None:

    # Module inventory
    mod_rows = []
    loc_by_ctx: Counter[str] = Counter()
    src_files = sorted(
        p for d in ("brant_core", "t03_candidate") for p in (package / d).rglob("*.py") if "__pycache__" not in p.parts
    )
    for p in src_files:
        rel = p.relative_to(package).as_posix()
        text = p.read_text(encoding="utf-8")
        tree = ast.parse(text)
        classes = [n.name for n in tree.body if isinstance(n, ast.ClassDef)]
        funcs = [n.name for n in tree.body if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef))]
        lines = text.count("\n") + 1
        slc, ctx, pkg = module_entry(rel)
        loc_by_ctx[ctx] += lines
        mod_rows.append([rel, lines, len(classes), len(funcs), slc, ctx, pkg, "；".join(classes[:8])])
    write_csv(
        DOCS / "07_ENGINEERING_BASELINE_REVIEW/02_Python_Module_Inventory_V0.50.csv",
        ["python_module", "lines", "classes", "top_level_functions", "slice", "target_context", "target_go_location", "class_names"],
        mod_rows,
    )

    # Test mapping
    test_rows = []
    per_file: Counter[str] = Counter()
    per_ctx: Counter[str] = Counter()
    test_files = sorted((package / "tests").glob("test_*.py"))
    for p in test_files:
        tree = ast.parse(p.read_text(encoding="utf-8"))
        slc, ctx, agg, inv = test_entry(p.name)
        for node in ast.walk(tree):
            if isinstance(node, ast.ClassDef):
                for item in node.body:
                    if isinstance(item, (ast.FunctionDef, ast.AsyncFunctionDef)) and item.name.startswith("test"):
                        doc = ast.get_docstring(item) or ""
                        test_rows.append([
                            f"PYT-{len(test_rows) + 1:04d}", p.name, node.name, item.name, item.lineno, slc, ctx, agg,
                            GO_PKG.get(ctx, "—"), camel(item.name), "；".join(inv), "MAPPED_TO_CONTEXT", "NOT_PORTED",
                            doc.splitlines()[0][:120] if doc else "",
                        ])
                        per_file[p.name] += 1
                        per_ctx[ctx] += 1
    write_csv(
        DOCS / "08_ENGINEERING_READINESS/02_Python_Test_to_Go_Mapping_V0.1.csv",
        ["mapping_id", "python_test_file", "python_class", "python_test_method", "line", "slice", "target_context",
         "target_aggregate", "target_go_package", "proposed_go_test", "invariant_refs", "mapping_status", "go_status", "python_docstring"],
        test_rows,
    )

    # Historical package register: frozen data, read only
    with REGISTER.open(encoding="utf-8-sig", newline="") as f:
        reg_rows = [[r["name"], r["kind"]] for r in csv.DictReader(f)]

    summary = {
        "package": PACKAGE_NAME,
        "sourceModules": len(mod_rows),
        "sourceLines": sum(r[1] for r in mod_rows),
        "linesByTargetContext": dict(loc_by_ctx.most_common()),
        "testFiles": len(test_files),
        "testMethodsStatic": len(test_rows),
        "testMethodsByFile": dict(per_file.most_common()),
        "testMethodsByContext": dict(per_ctx.most_common()),
        "historicalEntries": len(reg_rows),
        "historicalZips": sum(1 for r in reg_rows if r[1] == "zip"),
        "historicalDirectories": sum(1 for r in reg_rows if r[1] == "directory"),
    }
    out = Path(__file__).resolve().parent / "data" / "python_inventory_summary.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({k: v for k, v in summary.items() if not isinstance(v, dict)}, ensure_ascii=False))


if __name__ == "__main__":
    main()
