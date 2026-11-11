---
output: 09_IMPLEMENTATION_PLAN/06_IDP_Brant_S2_Execution_Report_V0.1.html
title: IDP Brant S2 寻源与核价切片执行报告 V0.1
short: S2 执行报告 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / S2 EXECUTION REPORT
doc_title: S2 寻源与核价切片执行报告 V0.1
doc_desc: SUP 供应条件与 SupplyConditionGuard · CST 计算规则 · 核价上下文与测量 · 场景评估 · 冻结快照 · 来源否认 · AdoptabilityGuard · 660 USD 基线 · T02 测试移植
meta: 版本=V0.1; 日期=2026-11-10; 证据=E2 本机执行＋GitHub Actions; 结论=S2 完成 · G1 NO-GO
stage: 09 · 工程实施总体计划
eyebrow: S2 EXECUTION REPORT
search: 搜索工作项、测试、Python 用例或证据
foot: S2 Execution Report V0.1 · 2026-11-10
---
<span class="scope-badge">S2 DONE · REMOTE CI GREEN · G1 NO-GO</span>

<h2>T02 的 660.00 USD 基线在 PostgreSQL 16 上从供应报价走到冻结快照；资格守卫按 ADR-BR-004 方案 B 实现</h2>

<p class="lede">2026-11-10，S2 建立了 SUP（供应商询价、供应报价、采购条件、物流报价）与 CST（核价上下文、测量快照、成本场景、成本快照）两个上下文。核价计算规则是不依赖数据库的纯函数；场景评估把规则按 Python 的次序组合，结果与规范化输入以 SHA-256 固定，同一输入得到逐字节相同的结果。冻结在一个事务内重新评估并重新守卫全部依赖，同一场景只冻结一次。来源被否认或依赖不再有效时，已冻结的快照不再可采纳，历史仍可读。Context Map 按负责人决定新增 R64 EVD→CST。G1 仍为 NO-GO（审批为模拟）。</p>

::: cover
- 结论 | S2 完成：S2-01 至 S2-08 都有本机与远程 CI 记录。T02 的 87 个 Python 测试中 74 个已移植、9 个为 SQLite 或 Python 专属（NOT_APPLICABLE）、4 个未移植并写明原因。
- 证据 | 07_S2_EVIDENCE：环境、源文件 SHA-256 快照、make ci、逐项测试日志、GitHub Actions 运行记录、严格 JSON 模糊测试。
- 未执行 | 真实名单与真实客户样例；PTY 主体与 CAT 规格修订进入可采纳判定（PTY 不是 CST 的上游，CAT 不发布守卫）；读取核价上下文历史时重查证据；TRD 与 PUR 对 AdoptabilityGuard 的真实调用（S3）。
:::

## 01 结论摘要 {#s1}

::: cards
- S2-04 · S2-08 | 核价计算规则 | 阶梯逐段计价、断档与重叠拒绝；MOQ 与订购倍数在按分组汇总后校验，不自动超采；同一费用义务只计一次，包含关系缺失或成环即拒绝；税额四种回收处理，部分回收按可回收率计算且不高于税率；汇率只用明确方向与 cost_estimate 用途；分摊以最小币值的最大余数法，合计严格等于总额。T02 §11 第 1—7 行与独立整数算术对照。
- S2-01 · S2-02 | SUP 供应条件 | 供应商询价引用 ready 询价行；供应报价、采购条件与物流报价各有草稿、修订、丢弃、核实或发布、撤回或退役；核实与发布须由草稿编辑人以外的人工执行；依附型采购条件随来源报价失效。核实检查与 CST 规则同码同序。
- S2-06 | SupplyConditionGuard v1 与 RfqGuard v1 | 下游事务内以 FOR SHARE 锁定确切修订行并判定是否仍有效；三种提交顺序实测符合设计。INQ 发布询价守卫，判定询价仍为 ready 且行集合未变。
- S2-03 | 核价上下文与测量 | 实际核价须覆盖询价的完整行集合；假设使结果保持 provisional，不可冻结；测量记录与评估使用同一套测量规则。引用的来源须对操作者可读（R64）。
- S2-05 | 场景评估与冻结 | 评估结果、规范化输入与全部绑定输入的内容哈希不可变地保存；冻结在同一事务内锁根、重新守卫依赖、重新评估并要求哈希逐字节一致，同一场景只冻结一次，并发冻结只有一个成功，Outbox 写入失败整体回滚。
- S2-05 · S2-06 | 来源否认与 AdoptabilityGuard v1 | 来源否认是 CST 的事实：否认后依赖它的快照（含经锚点与来源报价的传递依赖）不再可采纳，新冻结被拒，历史可读；冻结与否认的竞争由数据库行锁裁决。可采纳守卫供 TRD、PUR 调用。
:::

状态说明：DONE_CI 表示验收项在本机与 GitHub Actions 上都通过。

{{wbs_tasks phase=S2}}

## 02 设计决定与有意背离 {#s2}

| 编号 | 决定 | 依据 | 对 Python V0.50 的差异 |
|---|---|---|---|
| S2-D01 | 计算规则是 CST 领域内的纯函数包（rules、delivery、budget），只依赖 kernel；SUP 不能导入 CST（CST 是下游），在自身领域按相同的次序、错误码与路径重述核实检查，并重放 CST 的测试向量 | 分层规则；SUP-INV-002 | Python 两者共用一个引擎 |
| S2-D02 | 部分回收：成本为含税额减（未税额×可回收率，按币种 HALF_UP），可回收率不高于税率、允许为 0，回收额不超过实付税额；其他处理附带可回收率即拒绝 | CST-INV-007；S2-08 | Python 只有 recoverable、nonrecoverable |
| S2-D03 | 超出有效期的汇率为 FX_NOT_APPLICABLE；单项数量超过 10^6 为 CALCULATION_LIMIT_EXCEEDED；小数位过多为 SCHEMA_INVALID；256 位有理数上限改为逐个输入按格式与范围校验 | T02 §9、§13.6 | Python 为 REFERENCE_STALE、INVALID_KNOWN_VALUE，并逐个约束 Fraction |
| S2-D04 | 计价模式为空表示含义未定（PRICE_TIER_AMBIGUOUS，报价保持待定），未知模式为 UNSUPPORTED_PRICING_MODE；按基数计价的零费率也须写明原因 | SUP-INV-002；T02 §3 | Python 只检查零固定金额 |
| S2-D05 | 核实时 supersedes 与当前生效修订不符为 VERSION_CONFLICT；编辑人核实或发布自己的草稿为 INDEPENDENT_REVIEW_REQUIRED，三类供应条件都要求独立；已被替代的修订仍可撤回或退役 | 战术模型 SM-SUP-OFFER；SUP-INV-003；T02 §13.2 | Python 为 REFERENCE_STALE、PERMISSION_DENIED |
| S2-D06 | SUP 的引用：SKU 为 CAT 的规格修订；交期为 UN/LOCODE 地点加 POL 锚点事件与日历；物流起讫地须等于 PTY 服务方角色的起讫地；去掉 trade_scope（POL 没有该类型）；测量引用按原样保存，由 CST 解析 | Context Map R05—R07 | Python 用 sku_profile、location 与 trade_scope 配置 |
| S2-D07 | SupplyConditionGuard v1 对确切修订行 FOR SHARE，依附型采购条件同时锁定来源报价修订；不在有效期为 SUPPLY_CONDITION_EXPIRED（422），被替代、撤回、退役或未核实为 SUPPLY_CONDITION_NOT_CURRENT（409） | ADR-BR-004 方案 B；R09、R20 | Python 在使用时重查 |
| S2-D08 | INQ 只保留一个 RfqRef v1 查询（含行集合哈希，一次最多 200 个，格式错误的 ID 拒绝），并发布 RfqGuard v1；核价上下文核实时先锁本根，再调用 RfqGuard 与 ConfigGuard | R08；ADR-BR-011 | Python 在同一事务内直接读取 |
| S2-D09 | 核价上下文的核实总是要求独立（任何参与编辑者都不能核实）；上下文绑定确切的配置修订而不是基础快照；实际核价须等于询价的完整行集合，what_if 永不可冻结；存在假设即为 provisional | CST-INV-001 | Python 的独立性取决于定价政策的 independent_review |
| S2-D10 | R64 EVD→CST：核价上下文的保存与核实（含假设证据）、撤回，测量的记录、替代与撤回，都要求所引来源对操作者可读：不可读为 REFERENCE_INVALID，无 EVD 读权限为 PERMISSION_DENIED | 负责人决定（2026-11-10）；R63 先例 | Python 不可读为 PERMISSION_DENIED |
| S2-D11 | 评估结果确定：规范化输入与结果均为规范 JSON 并以 SHA-256 固定，含引擎与规范化版本和每个绑定输入的内容哈希；场景版本是列，不进入输入哈希；同一输入重复评估逐字节相同 | CST-INV-001 | — |
| S2-D12 | POL 没有销售交易范围，销售侧费用义务为空；覆盖关系只依据比较基准中的必需费用类型，不从贸易术语推断 | UL 不从贸易术语推导责任 | Python 有 trade_scope |
| S2-D13 | 供应商交期锚定在 POL 订单事件上，核价时尚未发生，结果为 provisional 且不可比较；锚定在 service_party_role 上为 DEPENDENCY_NOT_SUPPORTED（PTY 不是 CST 的上游） | R10；Context Map | Python 可锚定服务方角色 |
| S2-D14 | POL 日历在其有效区间内按其时区覆盖，开放截止视为覆盖至 9999-12-31；POL 日历没有工作日覆盖（待决） | POL calendar 类型 | Python 有覆盖范围与复核过的例外 |
| S2-D15 | 冻结在其时刻重新评估，要求输入哈希、引擎与规范化版本、结果哈希逐字节相等；同一场景只冻结一次（不同请求键同样） | CST-INV-001、002 | Python 逐字段比较 |
| S2-D16 | 来源否认是 CST 的事实（每来源一行状态，否认一次即终态，核验记录不可变）；attested 只记录不强制；对已否认来源再核验为 INVALID_STATE，恢复须以其他证据建立新修订 | R64 只发布来源与读取权限；SM-CST-ADOPT | Python 拒绝未核验来源（COST_INPUT_NOT_READY），允许以新 ID 重复否认 |
| S2-D17 | 评估与冻结经 EVD 重查输入自身证据（上下文核实与假设、测量、供应条件内容与核实、锚点与来源报价）的可读性：不可读为 PERMISSION_DENIED，已否认为 REFERENCE_STALE；POL、INQ、CAT 不发布其引用背后的证据，不在否认链内 | Python _source | — |
| S2-D18 | AdoptabilityGuard v1 依次 FOR SHARE 锁定快照、其来源状态行、所绑定的 CST 根，再调用上游守卫；结论为 adoptable、denied、superseded 或 not_current，后三者的错误码为 COST_SNAPSHOT_INELIGIBLE；汇率时效只在冻结时检查，CAT 规格无守卫（R51）不重查 | ADR-BR-004 方案 B；R11、R21 | Python 在查询时重新评估输入 |
| S2-D19 | CostSnapshotRef v1 为（snapshot_id、result_hash）；替代由 supersedes_snapshot_id 显式声明，限同一核价上下文且最多一个后继；否认与替代并存时以先发生者为准 | Python T03 snapshot_ref | — |
| S2-D20 | SUP 与 CST 新增设计中未列出的修订、丢弃与撤回命令（来自 Python 与 T02 §13.2），已登记进 Context Map、战术模型、人工专属清单与岗位模板 | T02 §13.2 | — |
| S2-D21 | HTTP 用通用的反射命令处理器：命令的 ID 成员为 id、expected_version 必填、SUP 的种类取自路由；api 包显式给出的 JSON 名优先（CST 的三个 id 与 context_kind）；冻结保留 Python 的 scenario_id，评估引用名为 evaluation_id | S1-D23 | Python 为 result_id |
| S2-D22 | Python 的基础扩展分支在 Brant 是 POL 配置类型与 PTY 角色；主体与 SKU 主数据修订不进入可采纳判定 | Context Map 没有 PTY→CST、CAT 不发布守卫 | Python 有 revised_supplier_master 与扩展分支测试 |

## 03 Python 测试移植 {#s3}

08 阶段映射表中 T02 的 87 个测试方法：74 个已移植（PORTED），9 个为 SQLite 迁移、备份或 Python 内存加载器专属（NOT_APPLICABLE），4 个未移植：PYT-0041 的历史一半（读取核价上下文时不经 EVD 重查证据，与 S1-D16 相同）、PYT-0094（服务方角色归 PTY）、PYT-0105（扩展分支已改为 POL 配置类型与 PTY 角色）、PYT-0122（主体与 SKU 修订不进入可采纳判定）。每个 Go 测试在注释中引用其 Python 来源。

{{s2_ported}}

## 04 证据 {#s4}

| 文件 | 内容 |
|---|---|
| 01_environment.json | 主机、Go、PostgreSQL、依赖与 git 提交 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256；构建器以此校验证据对应当前源码 |
| 03_make_ci_go1.26.5.txt | 以 CI 的 Go 1.26.5、CI=true 运行 make ci 的完整日志 |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果 |
| 05_github_actions_runs.json.txt | S2 分支的 GitHub Actions 运行记录 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码 30 秒模糊测试 |

{{s2_evidence}}

> [!warn] 尚未执行
> 真实名单文件与真实客户样例；PTY 主体与 CAT 规格修订进入可采纳判定；读取核价上下文历史时重查证据（与 S1-D16 一并处理）；POL、INQ、CAT 引用背后的来源进入否认链；TRD 与 PUR 对 AdoptabilityGuard 的真实调用（S3）；真人签核。G0、G1 仍为 NO-GO。

> [!decision] 已决定（负责人，2026-11-10）
> 核价证据须可读：Context Map 新增 R64 EVD→CST（SourceRef v1），与 R63 EVD→POL 相同（S2-D10）。S3 不接入 STL 信用守卫、CNX 贸易合规守卫与 TSK 对象控制，登记为 S4—S5 接入。
