---
output: 05_TACTICAL_DDD/01_IDP_Brant_DDD_Domain_Design_Master_V0.1.html
title: IDP Brant 战术 DDD 主文件 V0.1
short: 战术 DDD / Domain Design Master V0.1
kicker: IDP BRANT / 05 TACTICAL DDD / DOMAIN DESIGN MASTER
doc_title: 战术 DDD 主文件 V0.1
doc_desc: 候选聚合 · 状态机 · 锁序与一致性 · 不变量登记 · 编排 · 事件信封 · 参考验收
meta: 版本=V0.1; 日期=2026-11-06; 状态=候选 · 源自 Python V0.50 已验证规则; 门禁=G0 / G1 NO-GO
stage: 05 · 战术 DDD
eyebrow: TACTICAL DDD
search: 搜索聚合、命令、状态或不变量
foot: Tactical DDD V0.1 · 2026-11-06 · 主档案 V1.0
---
<span class="scope-badge">TACTICAL DESIGN CANDIDATE / GO NOT IMPLEMENTED</span>

<h2>先确定必须在一个事务内成立的不变量，再命名聚合根</h2>

<p class="lede">本稿给出 {{=contexts}} 个上下文的 {{=aggregates}} 个候选聚合、{{=stateMachines}} 个状态机与 {{=invariantRegister}} 条不变量。其中 PTY、CAT、POL、EVD、INQ、SUP、CST、TRD、PUR、TSK 十个上下文完整深化，因为 Python V0.50 已经实现并测试了它们的大部分规则；其余八个上下文给出轮廓，待 B0、R1 前详细设计。</p>

::: cover
- 机器可读源 | 02_IDP_Brant_Tactical_DDD_Candidate_V0.1.json；状态转换矩阵、不变量登记与参考验收 CSV 由构建器生成。
- 行为基准 | 每个完整深化的聚合都标出 Python 来源模块与测试；Go 实现以这些测试的等价用例为验收。
- 未决 | 跨上下文资格重查（ADR-BR-004）与 PostgreSQL 锁序（ADR-BR-011）会改变部分守卫的实现方式，不改变规则本身。
:::

## 01 战术建模原则 {#s1}

{{td_kpis}}

{{td_principles}}

聚合审查五问（沿用 Cargo 方法论）：是否有明确的根 ID、生命周期与业务所有者；所有不变量能否在一个本地事务中保证；能否通过命令拒绝非法状态与版本冲突；对跨上下文对象是否只保存不可变引用或快照；拆分后能否用事件与幂等消费者维持业务闭环。

## 02 上下文与聚合总览 {#s2}

{{td_context_overview}}

## 03 TRD：报价根、客户确认与外销订单 {#s3}

TRD 是 Brant 最复杂的上下文。它把四个聚合放在同一上下文里，是因为它们共享报价根冲突键 root_quote_id、统一锁序与同事务资格重查。QuoteRoot 聚合较大，但它的规模来自真实不变量：单一 active、批准绑定、转换名额锁定与对象控制都以根为单位成立。

{{td_aggregates context=TRD}}

> [!decision] 同上下文两聚合事务例外（ADR-BR-002）
> order.prepare 在同一事务内占用报价根的 full_v1 名额并创建 SalesOrder 草稿，违反“一个事务只修改一个聚合实例”的默认规则。业务核心设计明确要求名额与草稿同事务保留；备选方案是订单表上的唯一约束 (company, root_quote_id, slot) 加报价根行锁，quote.replace 与 quote.cancel 在同一锁下查询名额是否被占用。两种实现须在 PG16 上通过 BC-AT15、BC-AT17 与 SC-03 后择一。

## 04 TRD 状态机 {#s4}

### 4.1 报价修订效力

修订效力与商业内容分开保存：商业内容冻结后不可变，效力只能由注册命令改变。pending_approval 只表示修订处于生效前状态，不能推断存在可用批准；审核请求与批准效力另有各自的状态机。

{{td_state_machine id=SM-TRD-REVISION fig="FIG 01"}}

### 4.2 外销订单

{{td_state_machine id=SM-TRD-ORDER fig="FIG 02"}}

### 4.3 对象控制绑定

{{td_state_machine id=SM-TRD-CONTROL fig="FIG 03"}}

## 05 SUP 与 CST：供应条件与核价 {#s5}

SUP 拥有供应条件的已核实修订；CST 拥有场景、评估与冻结快照。两者分开，是因为采购草稿（PUR）同样直接绑定供应报价与采购条件，而不经过核价（ADR-BR-006）。

{{td_aggregates context=SUP}}

{{td_aggregates context=CST}}

{{td_state_machine id=SM-SUP-OFFER fig="FIG 04"}}

## 06 PTY、CAT、POL、EVD、INQ：主数据、政策、证据与询价 {#s6}

{{td_aggregates context=PTY}}

{{td_aggregates context=CAT}}

{{td_aggregates context=POL}}

{{td_aggregates context=EVD}}

{{td_aggregates context=INQ}}

{{td_state_machine id=SM-INQ-RFQ fig="FIG 05"}}

## 07 PUR 与 TSK：采购草稿与业务任务 {#s7}

{{td_aggregates context=PUR}}

{{td_aggregates context=TSK}}

{{td_state_machine id=SM-TSK-TASK fig="FIG 06"}}

## 08 轮廓上下文 {#s8}

以下上下文在 Python V0.50 中没有实现，本稿只给出候选聚合轮廓，用于确认边界与依赖；详细状态机与不变量在对应阶段（S5、S6）前完成。

{{td_aggregates context=QLT}}

{{td_aggregates context=FUL}}

{{td_aggregates context=STL}}

{{td_aggregates context=CNX}}

{{td_aggregates context=MFG}}

{{td_aggregates context=STK}}

{{td_aggregates context=CLM}}

{{td_aggregates context=CRM}}

## 09 其余状态机 {#s9}

{{td_state_tables skip=SM-TRD-REVISION,SM-TRD-ORDER,SM-TRD-CONTROL,SM-SUP-OFFER,SM-INQ-RFQ,SM-TSK-TASK}}

## 10 事务、锁序与一致性 {#s10}

强一致只在聚合内成立：命令 → 校验预期版本 → 守卫 → 变更 → 持久化 → Outbox，全部在一个数据库事务里提交。聚合之间与上下文之间采用最终一致：事件 → Inbox 去重 → 防腐层 → 命令 → 状态。

统一锁序（ADR-BR-011）：先锁本上下文的聚合根，再锁同根下级行，最后以 FOR SHARE 调用上游守卫；撤权、纠错、替代、订单准备、生效、变更与接管使用相同锁序与条件更新；持锁期间禁止模型与网络等待。上游从不锁下游行，守卫锁边必须无环（03 阶段关系表“守卫锁”列，构建器校验）；导入白名单中的双向关系至多一个方向使用守卫锁。

{{td_lock_order}}

| 机制 | Python V0.50 | Go 候选 |
|---|---|---|
| 写串行化 | SQLite BEGIN IMMEDIATE：全库写锁，所有守卫天然串行 | PostgreSQL 16 READ COMMITTED：按锁序显式 SELECT … FOR UPDATE／FOR NO KEY UPDATE（ADR-BR-011） |
| 跨模块资格重查 | 同一连接、同一事务内调用 costing_snapshot_in_transaction | 取决于 ADR-BR-004：资格守卫读取或事务外预取＋风险暂停 |
| 幂等 | (scope, command, idempotency_key) 唯一＋request_hash | 同左；按上下文 schema 保存命令回执 |
| 提交证据 | 与业务写入同事务追加 | 同左；server_commit_at 为持锁事务末尾的裁决时点 |
| 事件 | 本地 outbox 表 | idp-bento-go Outbox；brant-worker 中继 |

## 11 不变量登记 {#s11}

每条不变量至少要有一个能失败的负向用例；“Python 证据”列指向已实现该规则的测试文件，Go 实现须建立等价用例（08 阶段映射）。

{{td_invariants context=all}}

## 12 跨上下文编排 {#s12}

编排归属拥有决策的上下文；Q01 由 TSK 的业务任务承载，但每一步的业务决策仍在 INQ、CST、TRD 中，TSK 只记录步骤、等待与核验。

{{td_orchestrations}}

## 13 集成事件信封 {#s13}

对外发布的集成事件使用统一信封；eventType 带主版本后缀。载荷含标识与版本，以及各聚合 eventPayloads 登记的业务值（数量、金额、期限、结论），不含法定标识、个人数据与文件内容（S5R-D43，取代“载荷只含标识与版本”）。机器可读 Schema 见 06 阶段 04_Integration_Event_Envelope_V0.1.schema.json。

{{td_envelope}}

## 14 参考验收用例 {#s14}

BC-AT01—BC-AT28 沿用业务核心设计的编号；SC-03、SC-04 来自 Context Map；T02、T03、A46 用例来自对应规格与报告。状态 SPEC 表示待在 Go 中执行，不是已通过。

{{td_tests}}

## 15 签核矩阵 {#s15}

{{td_approval_matrix}}
