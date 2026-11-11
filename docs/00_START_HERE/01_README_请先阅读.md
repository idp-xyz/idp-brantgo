# IDP Brant 唯一主档案 V1.6 · 请先阅读

**首先用浏览器打开 `00_START_HERE/00_INDEX.html`。** 这是本档案唯一的导航首页。

## 这是什么

本目录按 IDP-Cargo 主档案（V2.9）的标准，为 IDP Brant（AI-Native Trade Operations Platform，中国 B2B 出口业务版）建立的 DDD 与工程主档案。内容来自三份输入：

1. 《IDP Brant 产品与研发范围基线 V1.0（中国 B2B 出口业务版）》——收录于 01 阶段，在本仓库维护。
2. 《IDP Brant 业务核心详细设计 V0.2》——收录于 01 阶段，在本仓库维护。
3. Python 参考实现 IDP_Brant_T04_A46采购准备_V0.50——作为行为基准审查（07）与测试映射（08），关键资料包 ZIP 作为冻结的参考副本收录于 90。

全部档案内容与工具的输入都在本仓库（idp-brantgo）内。

V1.1（2026-11-07）回写了 M0 工程基线的本机执行证据：报告在 10 阶段 `05_IDP_Brant_M0_Execution_Report_V1.0.html`，原始证据在 `10_M0_ENGINEERING_BASELINE/06_M0_EVIDENCE/`。V1.2（2026-11-09）在全部页面使用品牌标志与 favicon。V1.3 至 V1.5（2026-11-10）回写 S1 三批的执行证据，S1 完成；V1.6 回写 S2（报告 `06_IDP_Brant_S2_Execution_Report_V0.1.html`，证据 `07_S2_EVIDENCE/`），S2 完成：报告在 09 阶段 `04_IDP_Brant_S1_Execution_Report_V0.1.html`，原始证据在 `09_IMPLEMENTATION_PLAN/05_S1_EVIDENCE/`。

## 阅读顺序

01 产品蓝图 → 02 统一语言 → 03 战略 DDD → 04 事件风暴 → 05 战术 DDD → 06 工程契约 → 07 Python 基线审查 → 08 Go 迁移就绪度 → 09 实施计划 → 10 M0 与 G1 门禁（含 M0 执行报告）→ 11 方法论 → 12 Go 仓库结构基线。

## 目录约定

- 01—12：连续编号的阶段，每个阶段一个主文件（HTML），机器可读源为同目录的 JSON；CSV 由构建器生成。
- 80：页面截图；90：原始资料与历史版本登记。
- 同阶段内按 `01_`、`02_` 编号排序；新版本放在原阶段目录，旧版本保留。
- 本仓库根目录（idp-brantgo）是 Go Module `go.idp.xyz/idp-brant`；docs/ 不进入 Go 构建（`tools/dependency-check` 的 AC10 检查），公开发布时排除。代码说明见仓库根目录的 `README.md` 与 `AGENTS.md`。
- 页面标志与 favicon 取自仓库根目录的 `brand/`（IDP Brant Logo engineering V1.2）：白色透明底组合与深色方形图标，原样内联进每个页面，不改色、不缩裁、不重绘。该品牌包视觉已锁定，但生产母版尚未授权，方形图标只是预览；构建器核对内联文件与品牌包 QA 清单逐字节一致，找不到 `brand/` 时页面退回文字标志。

## 当前状态

- 全部 DDD 资产已经过 **AI 模拟评审**（2026-11-10，证据层级 E0，10 阶段第 09 章）：38 项 ADR 与待决项中 37 项接受或附条件接受、1 项（真实客户样例）暂缓，18 个上下文签核为接受或附条件接受。尚无真人签核。
- **G0 客户场景确认 NO-GO**：模拟 G0 走查已完成（04 阶段第 08 章，3 个虚构业务、6 项发现均已处置），但没有真实客户资料。**G1 详细设计与技术准入 NO-GO**：设计与 ADR 已模拟签核；远程 CI、OIDC 令牌校验、岗位模板、Outbox 死信与 Inbox 去重已实现（2026-11-10），13 项控制中 11 项 MET；C01—C03 只有模拟评审、没有真人签核；真实身份提供方接入按负责人决定延后至 B0 试点门之前；AI 红队（C11，非阻断）未做；受保护主干：仓库原为私有、GitHub 免费版不提供，负责人 2026-11-10 决定不配置；2026-11-11 仓库转入组织 idp-xyz 并公开后，负责人决定 main 只要求 CI（brant-ci）通过，不要求评审与签名溯源（C09 非阻断，PARTIAL）（Cargo 的 G0 在 Brant 对应 G1）。
- **阻断项**：原三项阻断 ADR（ADR-BR-004、ADR-BR-011、ADR-BR-REPO-10）已按 M0 实测在模拟评审中接受；G1 的工程阻断项均已满足：C04、C05、C06、C10、C13 有远程 CI 证据（C10 的真实身份提供方接入延后）；G1 的 NO-GO 来自审批只有模拟评审。
- **Go 侧**：M0 工程基线 2026-11-07 在本机完成 9／11 项；S1 于 2026-11-10 完成（11 项工作均有远程 CI 记录）：命令账本、EVD、PTY（含主体筛查、交易档案、地址与服务方角色）、CAT（含货号映射）、INQ、POL（全部 18 种配置与 ConfigGuard）、筛查名单参考数据、HTTP 入口、OIDC 与 OpenAPI 候选、岗位模板、Outbox 死信与 Inbox 去重，公开仓库 idp-xyz/idp-brantgo（2026-11-11 由个人账号 idpxyz 转入）的 GitHub Actions 通过（09 阶段 S1 执行报告）。S2 于同日完成：SUP 供应条件与 CST 核价，T02 的 660.00 USD 基线在 PostgreSQL 16 上冻结。下一阶段为 S3 报价、确认、订单与采购草稿。
- Python V0.50 的 679 项 unittest 已于 2026-11-10 在 90 阶段 V0.50 ZIP 解压的副本上重跑通过，这只说明参考行为可复现，不计入 Go 验收；08 阶段映射的 577 个测试方法中，已移植 141 个（S1 67 个、S2 74 个），另有 2 个登记为有意背离。

## 如何重新生成与校验

```bash
python3 docs/00_START_HERE/tools/gen_python_inventory.py   # 只在重新盘点 Python 参考包时运行：从 90 阶段的 V0.50 ZIP 读取，重新生成 07／08 的清单与映射
python3 docs/00_START_HERE/tools/build_archive.py          # 重新生成页面、CSV、首页、版本登记、校验报告、清单与 SHA-256
make ci                                                    # 在仓库根目录运行 Go 侧的全部检查（需要 Docker）
```

构建器只读取本仓库内的各阶段 JSON、`tools/data/*.json` 与 `tools/pages/*.md`；90 阶段资料包与 06 阶段 Python 候选契约是冻结的参考副本，只登记哈希。使用 `03_FILE_MANIFEST.csv` 与 `04_SHA256SUMS.txt` 逐文件校验（清单不包含其自身）；`05_VALIDATION_REPORT.json` 记录结构与交叉引用校验结果及其适用范围。构建器还核对最新一次证据快照（S2 起为 `09_IMPLEMENTATION_PLAN/07_S2_EVIDENCE`）与当前 Go 源码一致：Go 代码改动后，须重新运行 `make ci` 并更新该证据，否则校验失败。

## 如何避免版本混乱

1. 每次修改直接更新对应阶段目录、版本登记与变更记录，不另开第二个首页。
2. “通过”一词必须注明层级：Python 参考、结构校验、Go 单元、PG16 集成（本机或 CI）或业务签核互不替代。
3. 每轮测试与 CI 证据回写到 06、10、12 阶段，像 Cargo V2.4—V2.9 那样逐版登记；V1.1 是第一轮（M0，本机）。
