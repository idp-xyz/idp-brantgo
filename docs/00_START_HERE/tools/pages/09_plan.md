---
output: 09_IMPLEMENTATION_PLAN/01_IDP_Brant_Implementation_Master_Plan_Go_V0.1.html
title: IDP Brant 工程实施总体计划 · Go V0.1
short: 工程实施总体计划 / Go V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN
doc_title: 工程实施总体计划 · Go V0.1
doc_desc: M0 与 S1—S6 · {{=wbsTasks}} 项工作 · Python 行为等价退出条件 · 角色、节奏与证据
meta: 版本=V0.1 Candidate; 日期=2026-11-06; 状态=候选 · 无已批准排期; 门禁=G0 / G1 NO-GO
stage: 09 · 工程实施总体计划
eyebrow: IMPLEMENTATION PLAN
search: 搜索工作编号、上下文或验收
foot: Implementation Plan V0.1 · 2026-11-06
---
<span class="scope-badge">PLAN CANDIDATE / NO APPROVED SCHEDULE</span>

<h2>一次做一个可验收的纵向切片，并以 Python 行为等价作为前三个切片的退出条件</h2>

<p class="lede">计划把 Go 实现分为 M0 工程基线与 S1—S6 六个切片，共 {{=wbsTasks}} 项工作。前三个切片对齐 Python V0.50 的交付顺序（T01＋共享基础、T02、T03＋T04），以已验证行为为基准降低风险；S4 补齐 AI-Native 与运行能力以满足 P0 退出前提；S5、S6 分别对应 B0 受限试点与 R1 完整首发。</p>

::: cover
- 机器可读源 | 02_IDP_Brant_WBS_V0.1.json 与 03_IDP_Brant_WBS_V0.1.csv。
- 顺序依据 | 产品基线 14.1 的 P0 依赖顺序；仓库结构基线的“随真实代码创建目录”原则。
- 排期 | 不给出日期与人力；须在 G0 客户证据与 G1 技术准入之后评估。
:::

## 01 计划原则 {#s1}

:::cards
- 原则一 | 纵向切片，不先建空脚手架 | 每个切片都从命令、聚合、迁移、测试走到可重放的业务结果；目录随第一段真实代码创建。
- 原则二 | 以等价性降低风险 | S1—S3 的退出条件是对应 Python 测试映射行全部有结论（通过、不适用或因 ADR 替代）。
- 原则三 | 阻断项前置 | ADR-BR-004、ADR-BR-011 与十进制实现在 M0 用 PG16 实测后裁决，避免 S2、S3 返工。
- 原则四 | 证据回写档案 | 每个切片的测试、CI 与 PG16 证据像 Cargo V2.4—V2.9 那样回写到 06、10、12 阶段与版本登记。
:::

## 02 阶段路线 {#s2}

{{wbs_roadmap fig="FIG 01"}}

{{wbs_phases}}

## 03 M0 工程基线 {#s3}

M0 建立可构建、可迁移、可测试的空产品仓库，并完成阻断 ADR 的实测原型。M0 不包含业务功能。

{{wbs_tasks phase=M0}}

## 04 S1 主数据与询价切片 {#s4}

S1 交付 EVD、PTY、CAT、POL、INQ 五个上下文，与 Python 的 BC-T01 和共享基础 V0.2 行为等价。它是后续所有上下文的上游，也是第一次验证编译器边界、import 白名单与每上下文独立事务 owner 的切片。

{{wbs_tasks phase=S1}}

## 05 S2 寻源与核价切片 {#s5}

S2 交付 SUP 与 CST，与 Python T02 行为等价，并第一次实现跨上下文资格守卫（S2-06），其方式取决于 ADR-BR-004 的裁决。

{{wbs_tasks phase=S2}}

## 06 S3 报价、确认、订单与采购草稿 {#s6}

S3 交付 TRD 与 PUR 的采购草稿，与 Python T03、T04（A1—A46）行为等价。这是最大的切片，按聚合拆为十二项工作；结束时出具 V0.50 等价性报告，并在 PG16 上重放 BC-AT01 人工链。

{{wbs_tasks phase=S3}}

## 07 S4 业务任务与 AI-Native {#s7}

S4 交付 TSK、internal/ai、Inbox 与真实 AI 评测，是 P0 退出的前提。产品基线明确：仅人工链可用不满足 AI-Native 要求，P0 最终退出需要 BC-AT28 真实 AI 目标链。

{{wbs_tasks phase=S4}}

## 08 S5 B0 受限试点扩展 {#s8}

{{wbs_tasks phase=S5}}

S5 完成后，2026-11-13 的 S5R 模拟签核（证据层级 E0，不是真人签核；10 阶段第 10 章）以 SAP 与富通天下为参照裁决了 S5 的 47 项待决问题与设计缺口。设计源已由 R0 修订；S5 各项的完成状态只对其原范围成立，改变的范围由下列 S5R 实现单元承担（R3 与 S4 的 TRD 对象控制冲突，排在 S4 A2 之后）。

{{wbs_tasks phase=S5R}}

## 09 S6 R1 完整首发 {#s9}

{{wbs_tasks phase=S6}}

## 10 角色与节奏 {#s10}

产品基线要求至少覆盖产品与行业、领域与后端、交互与前端、AI 与集成、测试与交付五类角色，可以由同一人兼任，但工作量、可用时间与缺口必须登记后再估算工期。

| 角色 | 主要责任 | 关键签核 |
|---|---|---|
| 产品与行业 | 范围、客户证据、行业规则 | G0、G3 |
| 首席架构师 | 上下文边界、ADR、白名单、一致性策略 | ADR-BR-001—012、ADR-BR-REPO |
| 领域与后端 | 聚合、命令、迁移、测试移植 | 各上下文 Domain Owner |
| 交互与前端 | 工作台与人工节点（产品基线 UI-01—UI-09） | 页面验收 |
| AI 与集成 | TSK、internal/ai、EER／BIP 适配、评测 | BC-AT26—28 |
| 测试与交付 | 映射表维护、PG16 与 CI 证据、等价性报告 | G1、G2 |

每个切片的节奏：更新 UL 与战术模型 → 实现命令与聚合 → 移植映射用例 → PG16 集成与并发测试 → 回写档案证据 → 切片评审。

## 11 质量门槛与证据 {#s11}

| 层 | 门槛 | 证据 |
|---|---|---|
| 静态 | gofmt、go vet（带与不带 integration 标签）、golangci-lint、dependency-check | CI 日志 |
| 领域 | go test ./...；每个不变量至少一条拒绝用例 | Go 测试报告 |
| 并发 | go test -race；CAS、幂等、名额与撤销竞争 | Race 与 PG16 竞争测试 |
| 数据库 | 真实 PostgreSQL 16 迁移、RLS、角色、约束与 Outbox 原子性 | PG16 集成报告 |
| 契约 | contract-check；负向载荷；事件重放 | 契约测试报告 |
| 等价性 | 映射表对应行全部有结论 | 等价性报告 |

## 12 排期与资源声明 {#s12}

> [!warn] 本计划不含排期
> 本计划只规定顺序、依赖与退出条件，不给出日期、工时或人数。Cargo 主档案同样没有自动认可原排期。排期须在取得 G0 客户证据、完成 M0 实测与 G1 技术准入、登记实际人员与可用时间之后评估，并作为本计划的新版本发布。
