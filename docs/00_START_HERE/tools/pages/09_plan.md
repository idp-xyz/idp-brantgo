---
output: 09_IMPLEMENTATION_PLAN/01_IDP_Brant_Implementation_Master_Plan_Go_V0.1.html
title: IDP Brant 工程实施总体计划 · Go V0.1
short: 工程实施总体计划 / Go V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN
doc_title: 工程实施总体计划 · Go V0.1
doc_desc: M0 与 S1—S8 · {{=wbsTasks}} 项工作 · Python 行为等价 · 服装插件与商业化交付闭环 · 角色、节奏与证据
meta: 版本=V0.2 Candidate; 日期=2026-11-24; 状态=候选 · 无已批准排期; 门禁=G0–G5 NO-GO
stage: 09 · 工程实施总体计划
eyebrow: IMPLEMENTATION PLAN
search: 搜索工作编号、上下文、S7、S8 或验收
foot: Implementation Plan V0.2 · 2026-11-24
---
<span class="scope-badge">PLAN CANDIDATE / M0–S8 · {{=wbsTasks}} TASKS / NO APPROVED SCHEDULE</span>

<h2>一次做一个可验收的纵向切片：从无头交易内核（M0–S6）走向完整可交付外贸产品（S7–S8）</h2>

<p class="lede">计划把 IDP Brant 实施分为 <b>M0 工程基线</b>与 <b>S1—S8</b> 共 10 个阶段、<b>{{=wbsTasks}} 项工作</b>。S1—S3 对齐 Python V0.50（T01＋共享基础、T02、T03＋T04）以行为等价守住交易内核；S4 补齐 AI-Native 任务层与评测；S5／S5R 与 S6 完成 B0 扩展与 R1 后端条件模块；2026-11-24 依据对照 SAP 与富通天下的六大差距评估，正式纳入 <b>S7（服装行业插件与可交付产品闭环，9 项）</b>与 <b>S8（复杂外贸专项与规模化商业发布，4 项）</b>，实现软件闭环与商业发布路径全覆盖。</p>

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

## 09 S6 R1 完整首发（无头后端切片） {#s9}

{{wbs_tasks phase=S6}}

2026-11-23 的执行结果在 [S6 执行报告](14_IDP_Brant_S6_Execution_Report_V0.1.html) 与 [15_S6_EVIDENCE](15_S6_EVIDENCE/README.md)，公开支持矩阵与发布规格在 [R1 发布规格与 G5 材料 V0.1](16_IDP_Brant_R1_Release_Specification_and_G5_Package_V0.1.html)。`S6-01` 至 `S6-05` 与 `S6-07` 已在 `07601c8` 与 `8066e1c`（含报价变更上限、委外累计上限与交接货号、五条渠道命令 HTTP）通过远程 CI，为 `DONE_CI`。`S6-06` 为 `PARTIAL`，G5 仍为模拟 `NO-GO`。BIP 已于 2026-11-23 经负责人确认永久退出路线。

## 09B S7 服装行业插件与可交付产品闭环 {#s9b}

2026-11-24 依据对照 SAP 外贸套件与富通天下外贸系统的客观差距评估，正式将以下 9 项工作纳入 S7，把无头后端核心升级为可直接交付服装与一般贸易出口企业上线使用的完整产品。服装行业插件（`S7-01`—`S7-04`）的编码前详细设计见 [服装行业插件详细设计说明书 V0.1](../05_TACTICAL_DDD/07_IDP_Brant_Apparel_Industry_Plugin_Detailed_Design_V0.1.html)。

{{wbs_tasks phase=S7}}

## 09C S8 复杂外贸专项与规模化商业发布 {#s9c}

S8 覆盖面向中大型及集团型出口企业的专项能力（信用证／中信保／锁汇、香港离岸＋境内双主体关联交易）、真实企业 OIDC 与外部政务／银行渠道联调，以及首批真实服装外贸客户试点与 G0—G5 真人评审签核。

{{wbs_tasks phase=S8}}

### 对照 SAP 与富通天下六大差距的 WBS 全覆盖矩阵

| 识别出的核心差距 | 对应 WBS 工作项 | 所在阶段 | 当前状态 |
|---|---|---|---|
| 1. 前端工作台（UI-01—UI-09）、双语单证套打与 Excel 批量交互 | `S7-06` | S7 | `NOT_STARTED` |
| 2. 邮件协同（替代永久退出的 BIP）＋ 外部财务总账凭证导出（金蝶／用友） | `S7-07`、`S7-08` | S7 | `NOT_STARTED` |
| 3. 四项高频外贸实战场景（短溢装容差、多单拼柜分摊、L/C 与中信保、香港离岸双主体） | `S7-03`、`S7-05`、`S8-01`、`S8-02` | S7 / S8 | `NOT_STARTED` |
| 4. 服装行业插件完整实现（底座、样衣、分码 CostBOM、MCQ 版费、质检配比、装柜、AI 契约） | `S7-01`、`S7-02`、`S7-03`、`S7-04` | S7 | `NOT_STARTED`（设计已出） |
| 5. 已暴露工程硬伤（独立主机中继 P95 ≤ 5 s ＋ 生产级大模型 Q01 全量适用族达标） | `S5R-11`、`S4-07` → `S7-09` 收口 | S4 / S5R / S7 | `PARTIAL` / `NOT_STARTED` |
| 6. 企业 OIDC、真实单一窗口／金税／银行联调、真实客户样本撞击与 G0—G5 签核 | `S5-06`、`S4-08`、`S6-06` → `S8-03`、`S8-04` 收口 | S4 / S5 / S6 / S8 | `NOT_STARTED` / `PARTIAL` |

## 10 角色与节奏 {#s10}

产品基线要求至少覆盖产品与行业、领域与后端、交互与前端、AI 与集成、测试与交付五类角色，可以由同一人兼任，但工作量、可用时间与缺口必须登记后再估算工期。

| 角色 | 主要责任 | 关键签核 |
|---|---|---|
| 产品与行业 | 范围、客户证据、服装与外贸行业规则 | G0、G3 |
| 首席架构师 | 上下文边界、插件扩展点、ADR、白名单、一致性策略 | ADR-BR-001—017、ADR-BR-EXT |
| 领域与后端 | 聚合、命令、迁移、插件实现、测试移植 | 各上下文 Domain Owner |
| 交互与前端 | Web 工作台、套打与 Excel 交互（UI-01—UI-09，`S7-06`） | 页面与端到端验收 |
| AI 与集成 | TSK、internal/ai、EER／标准邮件 ACL、总账导出、政务渠道、评测（BIP 已永久退出） | BC-AT26—28、S7-07/08/09、S8-03 |
| 测试与交付 | 映射表维护、PG16 与 CI 证据、独立主机压测、真实客户试点 | G1、G2、G4、G5 |

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
