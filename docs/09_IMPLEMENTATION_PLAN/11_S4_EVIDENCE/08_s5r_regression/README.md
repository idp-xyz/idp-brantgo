# S5R-12 回归证据（单元 R12，2026-11-17）

R12 在 82c49f6（R13 合并后）之上的单个提交 6ab1aa2 的树上，用 HTTP 重放 S5 的链路与场景，并为 S5R 的决定新增场景。本目录原样复制 R12 的 `scratchpad/r12/final/` 中的主要文件，未编辑。数据库：PostgreSQL 16.12 容器 brant-pg16-r12（127.0.0.1:56220），Go 1.26.8，WSL2 Ubuntu-24.04；R12 的 `make integration PG_PORT=56220 SHARDS=4` 通过（736 个 bootstrap 测试各一次，另 186 个包），没有运行 `make race`（R12 报告）。

| 文件 | 内容 |
|---|---|
| `s5r_scenarios.json`、`s5r_scenarios.md` | 八个 S5R 场景的汇总：S5R-D19、D03、D04、D20、D23、D10、D05、D22 各自的测试、断言的数字与拒绝码 |
| `s5r_records/` | 这八个场景的完整记录（每个请求的方法、路由、状态、回执或拒绝码与读到的事实）：S5R-D03、D04、D05、D10、D19（单元 D2b 的记录器）与 s5r-d20、d22、d23（单元 D2a 的记录器） |
| `d1_chain/` | D1 的 BRC-001 链路（`TestBRC001OrderToSettlementOverHTTP`）在这棵树上的记录与表格，及两次运行的比对 `compare.txt`：两次均通过，301 个步骤，3 次分发，3209 个值；规范化服务端 ID、SHA-256、时刻、日期与租户后 0 差异 |
| `d2a_cnat/` | CN-AT01—18 的矩阵（`cnat_matrix.json/.md`）：这棵树上 18 个用例 18 个通过 |
| `d2b_scenarios/` | 伴随场景表（`s5_scenarios.json`、`s5_scenarios_table.md`）：计划内的 16 个用例 16 个通过 |
| `diff-base-final.txt` | 与基线 82c49f6 上重录的结果相比的图形差异 |
| `diff-s5-final.txt` | 与 S5 的归档证据（`../../13_S5_EVIDENCE/`）相比的图形差异：每个 CN-AT 用例的结果与请求数、观察到的文字、偏差说明；其中的文字变化来自 S5R 的决定（例如 S5R-D26 的税务人员自行登记来源、R7 的第三方付款人先核实） |
| `diff-base-final-d1-steps.txt` | D1 链路步骤数的比对：301 对 301 |

没有复制：18 个 CN-AT 用例的逐个记录与 14 个伴随场景的逐个记录（共约 3 MB；与 S5 的差异已在两个差异文件里）。

CN-AT04 的并发赢家在基线重录与这次运行之间调换过：请求数不变，这是竞争顺序，不是决定的结果（R12 报告）。S5R-D05 的“先确认后批准”缺口由这些记录显示：`shipment.gap` 的 60 先提交，之后审核人仍批准了 50 的核销；其后再出运 1 被拒（`SUPPLIER_DELIVERY_INSUFFICIENT`），见 S5R-D05 记录里的 `approval_after_confirmation` 与 `later_shipment`。
