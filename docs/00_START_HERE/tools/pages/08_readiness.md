---
output: 08_ENGINEERING_READINESS/01_IDP_Brant_Go_Migration_Readiness_V0.1.html
title: IDP Brant Go 迁移就绪度 V0.1
short: 工程就绪度 / Go 迁移 V0.1
kicker: IDP BRANT / 08 ENGINEERING READINESS
doc_title: Go 迁移就绪度验证 V0.1
doc_desc: Python 测试到 Go 用例的逐项映射 · 移植方法 · Go 侧技术就绪 · 集成门禁
meta: 版本=V0.1; 日期=2026-11-06; 状态=映射已建立 · 0 项移植; 门禁=G0 / G1 NO-GO
stage: 08 · 工程就绪度验证
eyebrow: GO MIGRATION READINESS
search: 搜索测试文件、上下文或门禁
foot: Go Migration Readiness V0.1 · 2026-11-06
---
<span class="scope-badge">READINESS ASSESSMENT / MAPPING IS NOT PASSING</span>

<h2>映射、移植、通过是三件不同的事，必须分别登记</h2>

<p class="lede">Cargo 的 ADR-IMP-002 要求为已知的 89 项 Python 参考用例逐项建立 Go 等价用例映射，并强调映射不能自动算通过。Brant 沿用同一纪律：本阶段为 Python V0.50 的 {{=pythonUniqueTests}} 个唯一测试方法逐项建立了到目标上下文、聚合与不变量的映射，Go 侧移植数为 {{=goTestsPorted}}。</p>

::: cover
- 映射表 | 02_Python_Test_to_Go_Mapping_V0.1.csv：每行一个 Python 测试方法，含目标上下文、聚合、Go 包、建议测试名、不变量与两种状态。
- 生成方式 | gen_python_inventory.py 用 Python ast 解析测试文件，可重复生成；原包只读。
- 集成门禁 | 03_Integration_Gate_Matrix_V0.1.csv：14 项真实集成门禁；M0 本机完成迁移、RLS 与 Outbox 中继三项，同根并发、资格重查与身份三项部分完成，其余未开始。
:::

## 01 就绪度的定义 {#s1}

| 状态 | 含义 | 由谁变更 |
|---|---|---|
| MAPPED_TO_CONTEXT | 已确定目标上下文、聚合、Go 包与对应不变量 | 本阶段（全部 {{=pythonUniqueTests}} 行） |
| PORT_PLANNED | 已排入具体 WBS 工作项，确定 Go 测试名与夹具 | 实施负责人 |
| PORTED | Go 等价用例已编写并进入仓库 | 开发者 |
| PASSING_GO | Go 等价用例在受保护 CI 上通过（单元或 PG16 集成） | CI 证据 |
| NOT_APPLICABLE | Python 特有行为（如 SQLite 迁移细节、打包脚本），以 Go 对应能力的测试替代并注明理由 | QA＋首席架构师 |
| SUPERSEDED | 规则因 ADR 改变（例如 ADR-BR-004 选择方案 A 时的部分时序断言），注明 ADR 与新断言 | 首席架构师 |
| DIVERGES_BY_DESIGN | Go 有意不复现的 Python 行为（缺陷或已被 05 阶段改正的领域规则），须引用 05 阶段不变量并以新断言替代 | 领域 Owner＋首席架构师 |

就绪度只在“PASSING_GO＋NOT_APPLICABLE＋SUPERSEDED＋DIVERGES_BY_DESIGN”覆盖某切片的全部映射行时，才可宣称该切片完成 Python 行为对照。

已登记的有意背离（移植时对应测试按新断言改写）：报价有效期与合同成立（TRD-INV-012、TRD-INV-014、TRD-INV-017，涉及 T04 A37、T03 A26、T03 A35）；毛利例外（TRD-INV-007，test_t03_server_calculation.py）；采购多来源分配、超采与草稿作废（PUR-INV-002、PUR-INV-005、PUR-INV-007，test_t04_a46_purchase_draft.py）；税额部分回收（CST-INV-007，扩展 test_costing_domains.py）；07 阶段问题登记中的 Python 缺陷。

## 02 测试映射总览 {#s2}

{{rd_kpis}}

{{rd_by_context}}

## 03 按测试文件的映射 {#s3}

{{rd_by_file}}

## 04 移植方法 {#s4}

1. **按聚合放测试**：领域与应用测试放在 internal/contexts/<bc>/internal/{domain,application} 的 _test.go；需要 PostgreSQL 16 的测试放在上下文内部的 internal/integration（build tag integration）；根目录 tests/ 只做黑盒。
2. **合成夹具**：以 BRC-001 为基础夹具（company.A、customer.1、ST01 四个 SKU、supplier.A／B、660 USD 成本、960 USD 报价），并移植 Python 的 order_review_fixture、t03_sales_fixture、t03_scope_fixture 等夹具语义。
3. **表驱动与纯函数**：计算规则（阶梯、MOQ、分摊、税价、汇率、毛利、付款分配）先写成纯函数，用表驱动测试覆盖 T02 §11 与 T03 §5.3 的全部合成真值。
4. **并发重证**：凡 Python 依赖 BEGIN IMMEDIATE 串行化的用例（撤销与激活竞争、并发冻结、订单行争用），在 Go 中改写为两个连接的 PG16 竞争测试，并使用 go test -race。
5. **一用例一行**：每移植一个用例，更新映射表对应行的 go_status 与 Go 测试名；一个 Go 用例覆盖多个 Python 方法时逐行登记。
6. **不可移植项**：SQLite 迁移测试（test_foundation_migration、test_costing_migration）以 brant-migrate 的 PG16 迁移测试替代其语义（数据保全、未知历史拒绝、失败回滚）；test_contracts 以 contract-check 替代；test_cli 以 brant-cli 或 transport 测试替代。

## 05 Go 侧技术就绪 {#s5}

| 项目 | 现状 | 证据 |
|---|---|---|
| Go 工具链 | go.mod toolchain go1.26.5（与 CI 相同）；本机默认 go1.26.8，两者均运行过 make ci | 10 阶段 06_M0_EVIDENCE 03、04 |
| idp-bento-go | v0.1.0-rc.2 由 go.mod 精确引用，h1 与 Cargo 记录一致；Outbox Store 与 testkit 已在 M0 使用 | go.sum；06_M0_EVIDENCE 01 |
| idp-platform-go | v0.1.0-rc.1 在本机模块缓存中，尚未引用 | 待 IG-09 |
| 事务语义 | 另一 owner 的事务中返回 transactionConflict（AC4 已在 PG16 实测）；隔离级别 READ COMMITTED | internal/foundation/bento 集成测试 |
| 产品仓库 | idp-brantgo 根目录为 Go Module（M0 完成；S1 部分完成：EVD、PTY、CAT、INQ、POL、HTTP 入口与 OIDC）；docs/ 为本主档案；私有远程仓库 idpxyz/idp-brantgo，GitHub Actions 通过 | [S1 执行报告](../09_IMPLEMENTATION_PLAN/04_IDP_Brant_S1_Execution_Report_V0.1.html) |
| PostgreSQL 16 | 专用开发容器 brant-pg16（16.12，与 CI 同一 digest）；迁移、角色与 RLS 已实测 | 06_M0_EVIDENCE 05、07、08 |
| CI | .github/workflows/ci.yml 已定义，本机等价 make ci 通过；远程运行待仓库创建 | 06_M0_EVIDENCE 03 |

## 06 集成门禁矩阵 {#s6}

{{rd_gates}}

## 07 风险与对策 {#s7}

| 风险 | 后果 | 对策 |
|---|---|---|
| 只按规格重写、不移植测试 | 丢失 50 多个版本中发现的边界情况 | 以映射表驱动移植，S1—S3 退出条件绑定映射行结论 |
| 依赖 SQLite 串行化的守卫直接移植 | PG16 下出现竞态 | ADR-BR-011；每个此类用例改写为竞争测试 |
| 十进制实现差异 | 分位误差导致金额、税或付款计划不一致 | ADR-BR-REPO-10；独立算术对照 |
| ADR-BR-004 久拖不决 | S2 资格守卫与 S3 订单激活无法实现 | M0-10 先做竞争实测，G1 阻断 |
| 679 与 577 的计数混用 | 验收数字虚高 | 以唯一测试方法为单位登记 |

## 08 就绪结论 {#s8}

> [!warn] 结论：尚未就绪
> 设计与映射已覆盖 Python V0.50 的全部唯一测试方法。M0 建立了产品仓库、PostgreSQL 16 迁移与 FORCE RLS、授权守卫与 Outbox 中继；S1 第一批（2026-11-10）加入命令账本、EVD、PTY、CAT、INQ、POL（首批 5 种配置）、HTTP 入口、OIDC 令牌校验与 Outbox 死信，远程 CI 通过，Go 移植数为 {{=goTestsPorted}}。真实身份提供方、受保护主干与 Inbox 尚未就绪，G1 保持 NO-GO。
