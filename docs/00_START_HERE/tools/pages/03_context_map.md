---
output: 03_STRATEGIC_DDD/02_IDP_Brant_Strategic_DDD_Context_Map_V0.1.html
title: IDP Brant 战略 DDD 与 Context Map V0.1
short: 战略 DDD / Context Map V0.1
kicker: IDP BRANT / 03 STRATEGIC DDD / CONTEXT MAP
doc_title: 战略 DDD 与 Context Map V0.1
doc_desc: 子域分层 · 18 个候选限界上下文 · 上下游关系 · 事实权威 · 战略 ADR
meta: 版本=V0.1 Draft; 日期=2026-11-06; 状态=候选 · ADR-BR-004 阻断; 门禁=G0 / G1 NO-GO
stage: 03 · 战略 DDD 与 Context Map
eyebrow: STRATEGIC DDD
search: 搜索上下文、关系或事实
foot: Context Map V0.1 Draft · 2026-11-06 · 主档案 V1.0
---
<span class="scope-badge">STRATEGIC DESIGN CANDIDATE / NOT A DEPLOYMENT TOPOLOGY</span>

<h2>每项关键事实恰有一个写入方，跨上下文只按引用、版本与已发布语言协作</h2>

<p class="lede">本稿把产品基线的 22 个逻辑模块、业务核心设计的对象归属与 Python V0.50 的实际实现，裁定为 {{=contexts}} 个候选限界上下文、{{=relationships}} 条上下游关系与 {{=factAuthority}} 条事实权威。它回答三个问题：谁拥有哪项事实，谁能依赖谁，哪些边界还需要裁决。</p>

::: cover
- 机器可读源 | 01_IDP_Brant_Strategic_Context_Map_V0.1_Draft.json：上下文、子域、关系、外部边界、场景、ADR、门禁与事实权威。
- 可执行映射 | {{=relationships}} 条关系将生成 Go 的 import 白名单（12 阶段），上下文映射从图变成 CI 检查；标为守卫锁的关系必须构成有向无环图。
- 阻断项 | ADR-BR-004：跨上下文的事务内资格重查与 Cargo 基线冲突，须在 M0 实测后裁决。
:::

::: notice 状态声明
上下文名称、分级与关系都是设计假设，必须由领域负责人与首席架构师裁决；它们不是组织架构、微服务拓扑或已运行的接口。
:::

## 01 战略结论与阅读方式 {#s1}

:::cards
- 结论一 | 18 个候选上下文，4 个核心域 | 核价（CST）、报价与订单（TRD）、采购与供需分配（PUR）、业务结算（STL）规则密集，决定利润与履约承诺，是 Brant 的差异化所在。
- 结论二 | 报价、客户确认与订单合为 TRD | 三者共享报价根冲突键、统一锁序与同事务资格重查；拆开会让 full_v1 转换名额与“占用后禁止替代”无法在一个事务内成立。
- 结论三 | AI 能力层不是上下文，业务任务是 | 与 Cargo 一致，AI 网关、策略与回执放 internal/ai；BusinessTask 对人工与 AI 都成立，作为 TSK 上下文。
- 结论四 | 一致性策略先于代码 | Python 依赖单库单事务串行化重查上游资格；Go 基线禁止跨上下文事务内读取，ADR-BR-004 必须先裁决。
:::

阅读建议：产品与架构负责人先读第 01、03、07、12 章；领域工程师重点读第 04—06、08、10 章；实施负责人读第 11—13 章。

## 02 子域分层与投入策略 {#s2}

分类决定投入方式：核心域使用完整战术模式与强测试；支撑域在存在生命周期或法律约束处建聚合，其余用轻量应用服务；通用能力优先复用平台组件。分类不是永久标签，例如 SUP 是否升级为核心域取决于寻源比选能否形成竞争优势。

{{cm_subdomains}}

## 03 18 个候选限界上下文 {#s3}

{{cm_kpis}}

{{context_layers fig="FIG 01"}}

{{cm_context_table}}

> [!decision] 命名调整
> Brant 基线中的 shared.foundation 在 Go 中以 POL（商业政策与基础配置）为代码，避免与技术层 internal/foundation 混名；inventory.basic 以 STK 为代码，避免与外贸常用的 INV（Invoice）混淆。上下文代码同时用于 Go 包名、schema（brant_<bc> 中的 bc）、事件来源与迁移目录。

## 04 上下文详解：核心域 {#s4}

{{cm_context_cards tier=Core}}

## 05 上下文详解：增强支撑与支撑域 {#s5}

{{cm_context_cards tier=SupportingPlus,Supporting}}

## 06 上下文详解：通用能力与可选包 {#s6}

{{cm_context_cards tier=Generic,Optional}}

## 07 边界决策评分 {#s7}

评分只用于促进讨论，不自动替代业务裁决。每项 0—2 分，总分 0—10；高分建议独立语义边界，低分建议并入既有上下文。下表记录本稿最关键的八个边界判断。

| 候选边界 | 语义冲突 | 规则自主 | 事务独立 | 生命周期差异 | 演进节奏 | 总分 | 反例 | 本稿处理 |
|---|---|---|---|---|---|---|---|---|
| 报价与订单拆为两个上下文 | 1 | 1 | 0 | 2 | 1 | 5 | 订单准备与报价替代并发，需同根锁序 | 不拆，合为 TRD（ADR-BR-002） |
| 客户确认独立为上下文 | 1 | 1 | 0 | 1 | 1 | 4 | 确认效力纠错与订单激活须按根→确认→订单锁序裁决 | 并入 TRD |
| 供应条件独立于核价 | 1 | 2 | 2 | 2 | 1 | 8 | 采购草稿直接绑定供应报价，不经核价 | 独立为 SUP（ADR-BR-006） |
| 业务任务独立为上下文 | 2 | 2 | 2 | 2 | 2 | 10 | 任务 completed 不代表订单生效 | 独立为 TSK（ADR-BR-003） |
| 商业政策独立为上下文 | 1 | 2 | 2 | 2 | 1 | 8 | 汇率与税价由财务独立发布，被多个上下文引用 | 独立为 POL（ADR-BR-007） |
| 主体与商品拆分 | 1 | 2 | 2 | 1 | 1 | 7 | SKU 技术版本由产品负责，客户档案由销售负责 | 拆为 PTY 与 CAT |
| 中国协同拆为发票、报关、退税 | 1 | 1 | 1 | 2 | 1 | 6 | 同一出口批次被三类资料同时引用 | 暂不拆（ADR-BR-009），CN-D 冻结后复评 |
| 采购与供需分配拆分 | 1 | 1 | 1 | 1 | 1 | 5 | 混合供给启用前分配几乎都落到采购 | 暂不拆，混合供给启用时复评 |

## 08 上下游关系与契约 {#s8}

关系使用 Cargo 的模式词汇：OHS／Published Language、Customer／Supplier、ACL、Partnership、Conformist。带“Qualification Guard”的关系表示下游在提交前必须重查上游事实的当前资格，其实现方式取决于 ADR-BR-004。

{{dependency_matrix fig="FIG 02"}}

{{cm_relationships}}

> [!decision] 白名单上线前的前置条件
> 关系表是 tools/dependency-check 的唯一来源。如果实现中出现未登记的依赖，先在本表登记并签核，再修改代码；不能为了让检查通过而绕开白名单。

### 平台提供方与系统消费方

参考数据（internal/reference，schema brant_ref）不是限界上下文，而是平台提供方（S5R-D34，2026-11-13 模拟签核 E0）。读取筛查名单或国别项目的上下文，必须有一条以 REF 为上游的关系（R80 REF→PTY、R81 REF→CNX），Inbox 订阅检查也只对这些下游放行 idp-brant.ref。Go 的允许清单生成器只为 internal/contexts 下的上下文生成行，REF 关系不进 allowlist.txt；R1 把订阅检查与 reference/api 的导入规则收紧到这两条关系。

系统消费方由 Inbox 分发器以系统操作者 system:inbox.<bc>.<purpose> 运行，权限由 brant-admin inbox 按下表授予，不用岗位模板。只由系统执行的命令（订单变更复核的开启）只授予消费方（S5R-D29）。构建器校验每个消费方只订阅其上游的事件，且不持人工专属命令。

{{cm_platform}}

## 09 外部边界 {#s9}

外部系统通过防腐层接入，不得把外部模型带进领域层。HOST、EER、BIP、CMI、MCP 等既有平台按各自边界复用，是否接入以实际接口证据为准。

{{cm_external}}

## 10 事实权威矩阵 {#s10}

矩阵回答“这项事实谁说了算”：事实权威是业务上有权纠正原始事实的一方；写入方是系统中唯一能经仓储修改它的上下文；消费方只能持有引用、快照或投影。

{{cm_fact_matrix}}

## 11 验证场景 {#s11}

场景用于从反例检验边界：每个场景都给出前提、步骤、失败与反例、验收。SC-04 的验收取决于 ADR-BR-004 的裁决，因此当前标为阻断。

{{cm_scenarios}}

## 12 战略 ADR {#s12}

{{cm_adrs}}

### 12.1 ADR-BR-004 方案对比

Brant 的业务核心设计与 Python 实现要求：报价正式化、订单激活与成本冻结时，在同一事务内重查成本快照可采纳资格、已消费配置效力、审核人当前资格与供应条件效力；上游撤销先提交则新动作必须拒绝。Cargo 仓库结构基线规定每个上下文独立事务 owner（T1），对其他上下文的同步调用只允许在本上下文事务之外查询（T3）；idp-bento-go 在另一 owner 的事务中开事务返回 transactionConflict。

| 方案 | 做法 | 能否保持“反证先提交则拒绝” | 代价与风险 |
|---|---|---|---|
| A 事务外预取＋事件风险暂停 | 下游在事务前查询上游资格，提交时记录观察到的资格版本；上游撤销发布事件，下游消费后对已提交对象登记风险暂停 | 否，变为事后检测，存在提交窗口 | 完全符合 Cargo T1／T3；需要业务负责人接受规则放宽 |
| B 受治理的资格守卫读取 | 上游发布版本化的 SECURITY DEFINER 守卫函数（最小字段，函数内 FOR SHARE）；下游在自身事务内以自身 executor 调用，上游撤销以 FOR UPDATE 修改守卫行，由 PostgreSQL 行锁裁决先后 | 是 | 对 Cargo T3 与 DB2 的受控例外；守卫函数须登记、只读、版本化。运行时耦合：两个上下文永久同库；上游撤销须等待持有守卫的下游事务提交；上游守卫表的 DDL 会阻塞下游；每个命令对公司授权守卫行加 FOR SHARE，须压测热点行与撤权等待 |
| C 合并相关上下文 | 把 CST 的可采纳资格与 POL 已消费配置的效力并入 TRD 的事务 owner | 是 | 上下文边界失真，核价与政策 Owner 失去自主；后续拆分成本高 |

> [!decision] 决定（模拟评审接受，E0）
> 采用方案 B 实现资格守卫，同时采用方案 A 的事件传播处理已提交对象的风险登记；M0-10 已在 PostgreSQL 16 上完成 SC-04 竞争实测。适用范围按合同成立区分：quote.activate、quote.replace、cost_snapshot.freeze 发生在对外承诺之前，需要“反证先提交则拒绝”；order.activate 发生在合同成立之后，上游变化只需登记为由订单审批确认的风险（ADR-BR-013），可评估改用方案 A。事后风险登记依赖 ADR-BR-015 的死信与重放。由首席架构师、研发负责人与 TRD、CST 负责人签核对 Cargo 基线 T3 的例外。

## 13 门禁与签核 {#s13}

Brant 沿用产品基线自己定义的 G0—G5 门禁。Cargo 主档案的 G0（M0 启动门禁）在 Brant 对应 G1（详细设计与技术准入）。

{{cm_gates}}

签核要求：每个上下文由其 Domain Owner 签核职责、事实权威与边界；跨上下文关系由首席架构师与上下游负责人共同签核；ADR 的签核记录登记在 10 阶段 ADR 登记册。
