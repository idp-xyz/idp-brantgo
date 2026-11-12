---
output: 09_IMPLEMENTATION_PLAN/08_IDP_Brant_S3_Execution_Report_V0.1.html
title: IDP Brant S3 报价、确认、订单与采购草稿切片执行报告 V0.1
short: S3 执行报告 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / S3 EXECUTION REPORT
doc_title: S3 报价、确认、订单与采购草稿切片执行报告 V0.1
doc_desc: 报价根与业务哈希 · 服务端计算 · 审核与毛利例外 · 正式化与编号 · 客户回复与合同成立 · 销售订单与风险确认 · 查询与对账 · 订单变更 · 适用法律 · PUR 采购草稿 · BRC-001 · T03／T04 测试移植
meta: 版本=V0.1; 日期=2026-11-11; 证据=E2 本机执行＋GitHub Actions; 结论=S3 完成 · G1 NO-GO
stage: 09 · 工程实施总体计划
eyebrow: S3 EXECUTION REPORT
search: 搜索工作项、测试、Python 用例或证据
foot: S3 Execution Report V0.1 · 2026-11-11
---
<span class="scope-badge">S3 DONE · REMOTE CI GREEN · G1 NO-GO</span>

<h2>BRC-001 经 HTTP 从 960.00 USD 报价走到 960.00 USD 订单与 600.00 USD 采购草稿；合同成立以承诺到达时间判定，上游变化在订单审批中逐项确认</h2>

<p class="lede">2026-11-11，S3 建立了 TRD（报价根、服务端计算、审核与毛利例外、正式化、客户回复与 Acceptance、销售订单及其变更）与 PUR（采购草稿与需求分配）两个上下文。商业文档以与 Python V0.50 逐字节一致的 business_hash 封存；正式文档由 POL 在事务前取号，字节、绑定与提交证据同一事务写入。合同按承诺到达时间成立，报价到期后按时成立的合同仍可生效订单；成立后的上游变化不拒绝订单，由订单审批逐项确认。PTY 与 CAT 发布守卫函数，POL 新增 legal_terms 与 order_document_template。STL 信用与 CNX 贸易合规守卫按负责人决定未接入，订单激活以具名扩展点记为未检查。G1 仍为 NO-GO（审批为模拟）。</p>

::: cover
- 结论 | S3 完成：S3-01 至 S3-17 合并入 s3/quotes-orders，本机 make ci（含竞态测试）与 GitHub Actions 通过。T03、T04 的 417 个 Python 测试映射行的结论见 03 节。
- 证据 | 09_S3_EVIDENCE：环境、源文件 SHA-256 快照、make ci、逐项测试日志、GitHub Actions 运行记录、严格 JSON 模糊测试、BRC-001 全链记录。
- 未执行 | STL CreditGuard 与 CNX TradeComplianceGuard（S5-12、S5-13，订单激活记为未检查）；TSK 对象控制与 GoalReadiness（S4）；上游事件触发的风险暂停与 EVD 登记正式文件（S4-06）；FUL、STK 出运事实与 PUR、STL 承诺（订单结束与变更记为 not_wired）；外部发送渠道；CISG 与中国法以外的合同成立规则；真实客户样例与真人签核。
:::

## 01 结论摘要 {#s1}

::: cards
- S3-01 · S3-08 | QuoteRoot 与对象控制 | 草稿整稿保存、修订与丢弃（人工或 AI），冻结限人工。冻结封存规范化的商业文档，business_hash 与 Python V0.50 protocol.py 逐字节一致：hash-vectors.json 的 4 个向量（含中文、换行、é 的两种形式与辅助平面排序）原样复现，TRD 自有向量经 Python protocol.py 核对。数据库校验哈希、拒绝修改封存修订，每根至多一个 active 修订。冻结先锁本根、再锁权限守卫，按上下文代码顺序调用 CAT、CST、INQ、POL、PTY 的守卫。对象控制在根行上，bind、stop、release 推进 control_epoch，每次写入与重放都经围栏；标签与内部备注不入哈希（BC-AT09）。
- S3-02 | 服务端计算 | 纯函数包 calc：成本范围绑定（完整询价行、换算后数量、贸易与付款条款、地点与交付）、销售条款（税额适用、销售费用、重复义务拒绝）、行金额按币种 HALF_UP 舍入后求和、净销售经报价用汇率一次换算到比较币种、毛利精确比较 R − C ≥ R × minimum_margin 并给出精确差额、付款计划最大余数法。冻结只经服务端计算封存结果；INQ RfqRef v1 增加客户与联系人。T03 §5.3 合成案例、BC-AT04、06、07 与独立大整数算术对照（舍入与税额拆分 3000 例、汇率 2000 例、毛利与差额 5000 例、付款分配 3000 例）。
- R13 · R14 | PTY 与 CAT 守卫 | TradingProfileGuard v1、AddressRoleGuard v1 与 SkuGuard v1 为 SECURITY DEFINER 函数，对引用的确切行及其可变的主体、联系人、SKU 行 FOR SHARE，结论与对应查询逐字段一致。三种提交顺序在 PostgreSQL 16 上实测：上游先提交则下游拒绝，下游先持锁则上游等待，上游先持锁则守卫等待后看到变化；去掉 FOR SHARE 后，依赖锁的顺序测试只在受影响的行失败。
- S3-03 · S3-14 | 审核、批准与毛利例外 | 提交审核（frozen → pending_approval）、裁决（批准、退回、驳回）与撤销批准均限人工。审核人独立于编制人、冻结人与提交人，其当前授权在裁决事务内重读；批准绑定修订、哈希、目标、规则版本与期限。毛利低于最低值时普通批准不放行，须由持毛利例外权限、独立于编制人的人工在裁决中复述定价政策、最低毛利与精确差额（SC-02、BC-AT06）。放行时重查批准、期限、审核人资格与例外。
- S3-04 | 正式文档与激活 | 渲染器 quote-text/0.1.0 复现 Python 的 candidate-document.txt 与 Python 渲染器生成的向量。取号前先试运行全部准入；编号由 POL number.issue 在事务前分配，事务失败只留空号，重试取回同一编号。激活在一个事务内锁根、重查放行与全部上游守卫、渲染，写入文档字节与绑定、提交证据、修订状态、动作结果、审计与 QuoteActivated；文档每次读取都按绑定校验字节（BC-AT22）。两个首次激活只有一个成功（TRD-INV-001）；编号、渲染与存储失败无残留（TRD-INV-016）。
- S3-05 | 替代、撤回、取消、风险暂停与转换名额 | 替代在同一事务内使旧 active 修订 superseded；撤回结束 frozen 或待批修订并取消未决审核；取消结束 active 修订；历史均保留。撤销正式化所用的批准即登记风险暂停，修订不再服务新业务，不提供解除。转换名额 full_v1 在根上，被占后替代与取消为 ROOT_CONVERSION_LOCKED，替代与转换并发只有一个成功（SC-03、TRD-INV-010）。QuoteRevisionGuard v1 与修订查询供下游在事务内外使用。
- S3-06 · S3-13 | 客户回复、Acceptance 与合同成立 | 回复绑定 active、未暂停正式修订的哈希、编号与期限，只增不改。只有 full_unconditional 的回复经人工核实、复述修订、哈希、客户、联系人与完整行集合后形成 Acceptance（BC-AT10、11）。合同成立按到达时间：到达早于 expires_at 即成立，核实晚于到期也成立（SC-07，PostgreSQL 上以激活后 8 秒到期的报价实测）；迟到为 ACCEPTANCE_LATE，不创建任何内容（TD-AT03、BC-AT12）。效力账本 valid → invalidated 或 disputed，disputed 可再作废，不回到 valid。AcceptanceGuard v1 供订单在事务内判定。
- S3-07 · S3-13 | 销售订单、订单审批与风险确认 | order.prepare 把已成立合同所确认的 active 正式修订转为草稿订单，同一事务占用转换名额；订单快照逐字复制封存的商业文档与 Acceptance，其 SHA-256 即订单哈希。订单审批由独立于准备人与请求人的人工给出，期限与报价无关，并逐项确认合同成立后的上游风险。order.activate 在一个事务内重查 Acceptance、修订、名额、正式文档、正式化批准与订单批准，未确认的上游变化为 ORDER_RISK_UNACKNOWLEDGED，主体筛查不为 clear 即拒绝；报价到期后按时成立的合同仍可生效（BC-AT13、SC-07）。已成立合同的修订不能被替代或取消（ACCEPTANCE_ALREADY_EXISTS）。ActiveOrderRef v1 与 OrderGuard v1 供 PUR 使用（R19）。
- S3-09 | quote_ready、历史、发送前准入与动作对账 | quote_ready 判定修订现在能否服务下一步：对象控制、提交证据、文档字节、风险暂停、期限、批准与批准人当前资格、上游守卫、在当前输入上重新绑定的成本范围与销售条款、销售汇率时效。修订、审批、正式文档、提交证据与 Outbox 历史按报价根分页且不含载荷，Outbox 显示真实投递状态。发送前准入与发送目标只报告本地链条，没有发送渠道。同键重放与对账在事务前经 EVD 重查所引来源，读取、重放与对账都重查当前权限与数据范围（X-INV-003）。
- S3-10 · S3-16 | PUR 采购草稿、需求分配与作废 | 采购草稿从生效订单行与当前有效的供应条件准备，准备一次即不可变；一个订单行可按数量拆分给多个草稿与供应商，并发分配只有一个成功（TD-AT05、TD-AT06）；作废释放分配并保留记录。准备在事务内依次调用 CAT、CST、POL、PTY（新的 PartyGuard v1）、SUP 与 TRD OrderGuard 守卫；核价对照只记录不阻断（R21）。BRC-001 的 600.00 USD 草稿来自 960.00 USD 的生效订单。
- S3-15 | 生效订单变更与结束；S/C 与 PI 渲染 | 变更经准备、提交（七个领域的影响评估）、独立裁决与应用，形成带哈希链的新订单修订；已出运的数量、SKU 与价格不被覆盖（TD-AT01）；OrderGuard v1 对旧修订的绑定答 REFERENCE_STALE。生效订单可取消、短交关闭或关闭，FUL、PUR、STK、STL 事实记为 not_wired，不视为通过。order-text/0.1.0 渲染外销合同与形式发票，以黄金文件固定。
- S3-15 | 外销合同与形式发票 | order.document.generate 以 POL 版式与 order-text/0.1.0 生成订单当前修订的 S/C 或 PI，POL 取号，绑定、字节与提交证据同一事务写入；订单修订推进后旧单据为 ORDER_DOCUMENT_STALE（TRD-INV-020）。law 区块打印来源报价封存的法律条款，变更后不变。order.document.record_signed 以 EVD 文件登记签署件与会签件，未签署的生成件不能冒充签署件。SIM-02 第 6 步（生效前生成 PI 并登记会签件）在 PostgreSQL 16 上运行。
- S3-17 | 适用法律与合同成立规则（F-02） | POL 新增 legal_terms，公司档案可指定租户默认；冻结封存修订的法律条款与成立规则，quote-text/0.2.0 打印法律条款。CISG 与中国法按到达主义成立，其他法律在冻结时拒绝。SIM-02 第 3 步（德国法、汉堡仲裁、修订替代后按到达成立）在 PostgreSQL 16 上端到端运行；订单快照携带条款，条款退役在订单上成为待确认的风险。无法律条款的报价哈希与渲染逐字节不变。
- HTTP | 命令、读取与动作对账 | 35 个 TRD 命令与 2 个 PUR 命令经通用命令路由；读取有报价、active 修订、批准检查、正式文档（Base64，读取时校验）、quote_ready、五种历史、发送前准入与目标、订单与订单检查、客户回复、Acceptance、采购草稿与订单行分配，查询参数封闭。TestQuoteToAcceptanceOverHTTP 在 PostgreSQL 16 上从线上草稿经冻结、独立审核、激活、正式文档走到已核实的完整承诺，并核对 HTTP 草稿与 trd/api 草稿冻结出相同的商业内容。动作对账路由按 06 阶段契约答复 committed、payload_conflict 或 not_committed，不执行命令。
- S3-12 | BRC-001 全链经 HTTP 重放（BC-AT01） | TestBRC001FullChainOverHTTP 按事件风暴 ES-01 第 1—19 步在 PostgreSQL 16 上经 130 个 HTTP 请求、十位人工各持访问令牌走完 BRC-001：ST01 四行（24、36、12、48 PCS），两家供应商（5.00 与 5.30，按款式组合计 MOQ 100），核价场景 A 660.00 USD 冻结、B 696.00 USD；报价应付 96000（960.00 USD，毛利 300.00，付款 288.00＋672.00）经独立审核正式化为 QT-2026-00001；客户承诺按到达成立；订单 96000 经独立审核生效；采购草稿 60000（600.00 USD）。编制人自审被拒（INDEPENDENT_REVIEW_REQUIRED、REVIEWER_NOT_INDEPENDENT），第二次准备订单为 CONVERSION_SLOT_CONFLICT，超量分配为 PURCHASE_ALLOCATION_EXCEEDED。只有租户、十位人工的授权与令牌签发为夹具，其余全部经 HTTP；每一步记录在证据 07。
:::

状态说明：DONE_CI 表示验收项在本机与 GitHub Actions 上都通过。

{{wbs_tasks phase=S3}}

## 02 设计决定与有意背离 {#s2}

| 编号 | 决定 | 依据 | 对 Python V0.50 的差异 |
|---|---|---|---|
| S3-D01 | S3 不接入 STL CreditGuard（R57）、CNX TradeComplianceGuard（R58）与 TSK 对象控制命令（R17），登记为 S4—S5 接入。order.activate 保留具名扩展点（CNX 在 CAT 之后，STL 在 PTY 之后），未接入时提交证据记 not_wired、OrderActivated 列入 unchecked_guards，变更应用同样记录，从不视为通过 | 负责人决定（2026-11-10）；TRD-INV-022、023；S5-12、S5-13、S4-01 | — |
| S3-D02 | business_hash 为 canonical(normalize(commercial)) 的 UTF-8 字节的 SHA-256 小写十六进制：DECIMAL_PATHS 去尾零，SORT_PATHS 稳定排序，规范 JSON 同 Python json.dumps(ensure_ascii=False, sort_keys=True) 的转义集，U+2028、U+2029 与 DEL 原样输出，不做 Unicode 规范化。数据库 CHECK sha256(commercial) = business_hash | TRD-INV-002；S3-01 验收（含中文与换行的互操作向量） | — |
| S3-D03 | 商业文档为 quote_id、business_revision、issued_at、data、calculation、party_snapshots、dependency_manifest 与 normalization_version；依赖清单列出 POL 修订（含嵌套的税率档案与日历）、询价行集合哈希、成本快照、交易档案内容哈希与 EVD 来源 SHA-256 | TRD-INV-002 | — |
| S3-D04 | 草稿去掉 trade_scope_ref，贸易术语由 POL 贸易条款表达；为成本范围绑定，named_place_ref（location）与 delivery（CST 的形式，calendar_ref 为冻结采用的 POL 修订）为冻结必填，行可声明 base_quantity | S2-D12；R15；BC-AT04 | Python 有 trade_scope_ref |
| S3-D05 | 客户绑定 PTY 已确认的主体修订，联系人绑定行版本，地址绑定（主体、主体修订、地址）；查询不给这些记录与规格的内容哈希，故按修订固定、不列入依赖清单，商业文档也不快照客户名称 | R13、R14 | Python 客户绑定 row_version |
| S3-D06 | 丢弃草稿保留该修订行，修订号单调、不复用 | S1-D11 先例 | Python 删除该行并复用修订号 |
| S3-D07 | 冻结在事务前经 EVD、PTY、INQ、CAT、POL（两轮，取嵌套修订）与 CST 查询；事务内先锁本根 FOR UPDATE（同时承载对象控制）、再锁权限守卫 FOR SHARE，判定可见性、对象控制、根版本 CAS 与事务前读到的根版本未变，SealDraft 后按上下文代码顺序调用 CAT SkuGuard、CST AdoptabilityGuard、INQ RfqGuard、POL ConfigGuard（重核内容哈希）、PTY TradingProfileGuard 与 AddressRoleGuard | ADR-BR-004 方案 B；ADR-BR-011；R11—R15 | Python 在同一事务内直接读取 |
| S3-D08 | 每根至多一个 active 修订由部分唯一索引保证；数据库拒绝修改封存修订的内容，修订状态只按 SM-TRD-REVISION 迁移 | TRD-INV-001、002 | — |
| S3-D09 | quote.metadata.update 只改根上的标签与内部备注，推进根版本、不改 business_hash；该命令已登记进 Context Map 与销售经办岗位模板。草稿保存、修订、丢弃与对象控制变化不发布事件，草稿与商业文档以 JSON 文本保存，字节不变 | BC-AT09；TRD-INV-002；事件目录 | — |
| S3-D10 | 对象控制在根行上，绑定须有已存在的根；bind、stop、release 推进 control_epoch，不推进根版本；携带旧纪元的任务声明为 CONTROL_EPOCH_STALE（目录记为设计码，此处为实际返回的码） | TRD-INV-015；R17 | Python 允许绑定不存在的根；停止中返回 OBJECT_CONTROL_CONFLICT |
| S3-D11 | object_control.bind 限人工或 system，不许 AI；stop 与 release 限人工，release 可从 stopping 执行，manual 状态与释放前提归 TSK。任务声明只由受信适配器设置，命令结构没有 task 成员 | TRD-INV-015；R17；命令目录（system）与岗位模板（销售经办） | — |
| S3-D12 | 公司内任何授权变化都使对象控制绑定失效；TSK 接入时复核是否放宽 | Python delegate_epoch；S4-01 | 同 Python |
| S3-D13 | 请求哈希不含幂等键与各命令的 CAS 成员（expected_version、expected_control_epoch、expected_effectivity_version、expected_approval_effectivity_version）；扩展回执的重放从已存行重建 | S1 命令账本 | — |
| S3-D14 | 计算规则是 TRD 领域内只依赖 kernel 的纯函数包 calc；12 个计算码在 trd/api 声明（均 422），领域另有一份分类表，测试两表一致且与目录行一致，api.Reject 遇未声明的码即 panic | 分层规则；S2-D01 先例 | — |
| S3-D15 | 同一费用义务键重复、或行价已含的费用另行收取，均为 CHARGE_DOUBLE_COUNT | 错误码目录（重复费用义务）；BC-AT07；CST 同码 | Python 为 CHARGE_SCOPE_UNRESOLVED（PYT-0304）；行价含费无 Python 向量 |
| S3-D16 | what_if 快照为 COST_SNAPSHOT_NOT_ELIGIBLE；COST_SNAPSHOT_INELIGIBLE 仍是 CST 对“当前不可采纳”的结论 | TRD-INV-006；S2-D18 | Python 为 COST_SNAPSHOT_INELIGIBLE（PYT-0290） |
| S3-D17 | 同币种报价指定销售汇率、或异币种报价未指定汇率为 FX_SCOPE_MISMATCH，未知汇率引用为 REFERENCE_INVALID；汇率年龄窗口为闭区间 [0, 上限]，精度到微秒，超出为 FX_STALE | POL DecideFX；S1-D13 | Python T03 为 REFERENCE_INVALID |
| S3-D18 | 声明的基数量不等于换算后数量为 BASE_QUANTITY_MISMATCH，换算后数量不等于询价或快照为 COMPARISON_BASIS_MISMATCH；缺少单位换算为 DEPENDENCY_NOT_SUPPORTED，不接受反向换算 | BC-AT04；TRD-INV-006 | 比较基准与单向换算同 Python；BASE_QUANTITY_MISMATCH 为新增 |
| S3-D19 | 超过 200 行或 100 项费用为 CALCULATION_LIMIT_EXCEEDED，超过 20 个付款阶段为 PAYMENT_TOTAL_MISMATCH，空行 ID 为 DRAFT_NOT_READY；金额上限 10^24 最小单位，价格基数量允许 8 位小数 | T03 §10；PYT-0326 | Python 超限为 DRAFT_NOT_READY、CHARGE_SCOPE_UNRESOLVED，空行 ID 为 DUPLICATE_LINE_ID |
| S3-D20 | 不设 trade_scope，贸易条款按 CST 比较基准直接比较，交易档案条款一致性（Python A29）并入 BindScope。报价客户与联系人须等于询价的（REFERENCE_INVALID；RfqRef v1 新增 CustomerID、ContactID，LinesHash 与 RfqGuard 不变），联系人行版本不比对 | S2-D12；R12 | Python 有 trade_scope；PYT-0273 比对联系人行版本 |
| S3-D21 | 税额适用按 POL 的模型（方向、标的种类 sku 或 fee_category、标的 ID、主体，空为全部主体），适用主体不是客户为 TAX_POLICY_UNCONFIRMED；开票方与业务模式检查改为公司范围 | S1-D21；R15 | Python 检查 issuer 与业务模式 |
| S3-D22 | 毛利精确比较 R − C ≥ R × minimum_margin，结论为 pass 或 requires_approval，差额按主币单位精确保存，只按快照总额判定、不计逐行毛利；行金额按币种 HALF_UP 舍入后求和，付款计划用最大余数法 | TRD-INV-007、008；T03 §5.2 | — |
| S3-D23 | 配置是否在效、证据是否当前不在 calc 判定，由 POL ConfigGuard 与 EVD 判定 | R15、R16 | — |
| S3-D24 | 冻结只经服务端计算器封存结果；成本上下文经 cstapi.Lookups 读取，其计算币种与成本单位作为附加采用的 POL 修订列入依赖清单并经 ConfigGuard 重查 | R11、R15 | — |
| S3-D25 | 计算块在 Python 字段之外增加 sales_fx_ref、comparison_fx_rate、minimum_margin、margin_shortfall、cost_scope_binding、sales_terms_binding、calculation_version 与每项费用的 obligation_key，均入哈希 | TRD-INV-002、007 | Python 计算块无绑定与版本成员 |
| S3-D26 | 冻结的检查顺序为完整性、草稿规则、引用、TRD 专属内容、有效期、计算；calc 已判定的商业检查从冻结中移除，冻结只保留开票方公司档案、渲染器、编号序列单据类型与快照询价行集合哈希 | TRD-INV-006 | — |
| S3-D27 | excluded_cost_categories 由 TRD 按成本上下文的成本基础推导（nominal_direct_cost 对应 CST 的三个常量），因 CST 的已解析快照不携带 | R11 | — |
| S3-D28 | PTY 发布 brant_pty.guard_trading_profile_current_v1 与 guard_address_role_current_v1 两个守卫：二者事实在不同行，AddressRoleRef v1 自带主体（收货人可不是客户），每个引用在自己的路径得到一个结论 | R13；ADR-BR-004 方案 B | Python 在使用时重查 |
| S3-D29 | PTY、CAT 守卫均为 SECURITY DEFINER、search_path 固定、只授 brant_app 执行、恒返回一行，未知或他公司引用为 REFERENCE_INVALID；守卫不判数据范围，由事务前的查询判定 | ADR-BR-004 方案 B；S1-D17 先例 | — |
| S3-D30 | 交易档案守卫锁已发布修订行、主体行、所绑定联系人（按 ID）；地址守卫锁地址行、主体行；SKU 守卫锁 SKU 行、规格修订行。引用指名的不可变行同样加锁，判定过期即返回 | R13、R14 | — |
| S3-D31 | 过期原因与查询一致：交易档案 superseded、party_unavailable、party_changed、contact_changed，地址 party_unavailable、party_revised；SKU 重开或已确认更新修订为 REFERENCE_STALE，停用、未知或从未确认为 REFERENCE_INVALID | ResolveTradingProfiles、ResolveAddressRoles、ResolveSpecs | — |
| S3-D32 | PTY、CAT 命令不在持有守卫所锁之行时等待另一行，守卫不锁工作修订，故任意调用顺序都不死锁；冻结仍按（sku、修订）与（主体、修订、地址）排序调用，守卫返回的内容哈希或主体修订与事务前不同为 REFERENCE_STALE | ADR-BR-011 | — |
| S3-D33 | 不另建 PartyRef 与 ContactRef 守卫：主体币种与档案联系人经交易档案与地址守卫覆盖；结算条款用 ConfigGuard，主体筛查用 PartyScreeningGuard（订单激活） | R13；TRD-INV-021 | — |
| S3-D34 | 退回与驳回使修订回到 frozen；一个修订只审核一次，再提交为 INVALID_STATE，须修订后重审；每根至多一个未决审核 | SM-TRD-REVISION、SM-TRD-REVIEW | 同 Python |
| S3-D35 | quote.submit_review 显式给出 business_revision 与 target_command（quote.activate 或 quote.replace，后者须给 expected_active_revision）；目标在提交、裁决与放行时都检查：已有 active 为 QUOTE_ACTIVE，无 active 为 NO_ACTIVE_REVISION，指向其他修订为 QUOTE_NOT_CURRENT | TRD-INV-003；错误码目录（根已有 active） | Python 推断修订；已有 active 时为 QUOTE_NOT_CURRENT |
| S3-D36 | 审核与批准 ID 在报价内唯一（BUSINESS_KEY_CONFLICT） | 聚合内实体，先锁根 | Python 在公司内唯一 |
| S3-D37 | 裁决时陈述的哈希须等于修订的（BUSINESS_HASH_MISMATCH）；审核人不得是编制人、冻结人或提交人（INDEPENDENT_REVIEW_REQUIRED）；修订的规则版本须为当前版本 | TRD-INV-004；错误码目录（商业哈希不一致） | Python 哈希不符为 REFERENCE_INVALID |
| S3-D38 | 审核人当前资格在裁决事务内检查：账本授权 quote.review.decide，新的 Authorizer.Qualify 以相同的 FOR SHARE 锁序重读其 trd.read 授权（PERMISSION_DENIED），数据范围外为 NOT_FOUND | TRD-INV-004；A33；brant_auth 属基础层 | Python 只在正式化时检查读取权限 |
| S3-D39 | 时效：now < 报价 expires_at（QUOTE_EXPIRED），now < 批准 expires_at < 报价 expires_at（APPROVAL_EXPIRED），相等即拒绝；时间只接受 Z，缺期限或带偏移为 SCHEMA_INVALID | TRD-INV-005；kernel.Instant | Python 缺期限为 APPROVAL_EXPIRED、接受偏移；PYT-0398 不适用（列为 timestamptz） |
| S3-D40 | 退回与驳回须有理由，撤销须有理由与证据（1—200 条）；裁决与撤销都推进根版本，撤销以效力版本 1 → 2 CAS | SM-TRD-REVIEW、SM-TRD-APPROVAL 的守卫条件 | Python 理由与证据可选，撤销不推进根版本 |
| S3-D41 | 批准绑定修订、business_hash、目标与预期 active 修订、规则版本（规范化、计算、定价政策修订）、expires_at 与审核人及其权限纪元 | TRD-INV-003 | — |
| S3-D42 | 放行依次判定：修订待批，批准属本修订（APPROVAL_REQUIRED、APPROVAL_REFERENCE_MISMATCH），效力版本 CAS 后 APPROVAL_REVOKED，目标与根状态，哈希与规则版本（APPROVAL_STALE），QUOTE_EXPIRED 后 APPROVAL_EXPIRED，审核人仍独立且仍合格（授权行 FOR SHARE），毛利例外。审计读取 quote.approval.check 以码给出同一结论 | TRD-INV-003、004、005、007 | — |
| S3-D43 | requires_approval 的修订可以提交审核，由裁决附带毛利例外 | TRD-INV-007、ES-02 第 2 步优先于状态机守卫的“非低毛利例外” | Python 在提交时拒绝 margin_rule 不为 pass 的修订 |
| S3-D44 | 毛利例外在 approve 裁决内给出：裁决人须在裁决时持有 quote.margin_exception.approve 且独立于编制人（MARGIN_EXCEPTION_NOT_AUTHORIZED，先查权限），须复述定价政策、最低毛利与精确差额（APPROVAL_REFERENCE_MISMATCH）；毛利通过时给出例外为 INVALID_STATE | TRD-INV-007；BC-AT06 | test_t03_server_calculation.py 的有意背离（S3-14） |
| S3-D45 | 例外可随内容批准给出，或以同一 approval_id 与 expires_at 再次 approve；例外绑定哈希、规则版本、最低毛利、差额与币种，只插入，批准撤销即失效，普通批准不放行（MARGIN_POLICY_REQUIRES_APPROVAL） | ES-02 第 5 步；SC-02 | — |
| S3-D46 | margin_approver 岗位模板增加 quote.review.decide，毛利审批人能作出带例外的裁决；与销售、核价的禁止组合不变 | 负责人决定（2026-11-11）；ES-02 | — |
| S3-D47 | 审核、批准与例外三表 FORCE RLS，触发器保证审核以待批修订及其哈希开启且只裁决一次、批准只撤销一次、例外只插入，均不可删除 | TRD-INV-002、003 | — |
| S3-D48 | PostgreSQL 上的 SC-02 用 6.00 USD、最低毛利 0.1（差额 12），因测试世界的快照绑定 0.1 的定价政策；精确数字（7.00 USD、0.25、差额 30）在领域测试与黄金向量中 | SC-02；BC-AT06 | — |
| S3-D49 | 正式文档由纯函数渲染器 quote-text/0.1.0 生成；Prepare 在事务与取号之前检查已存字节、服务端计算、模板修订与内容哈希、模板受支持（十个区块、语言 en）、事实与引用一致、连同 64 字符编号不超过 1 MiB，此后 Formalize 只会因编号格式失败 | TRD-INV-009 | — |
| S3-D50 | candidate-document.txt 与 Python document.render 生成的向量逐字节复现；小数按规范形式打印（同一哈希得同一字节），Scope 行打印贸易条款修订，客户地址先取买方地址，时间按存储的 UTC 打印 | TRD-INV-009 | Python 按输入形式打印小数，Scope 行取 trade scope |
| S3-D51 | Python 未打印过的形式由 Brant 定义：无日期基准或日历的付款阶段、交期类交付、无里程碑或时区的交付；交付取自冻结文档的 data.delivery，其他形式为 REFERENCE_INVALID | TRD-INV-009 | 超出 Python |
| S3-D52 | 不复现 Python Core 的 A19 内部渲染（INTERNAL RENDER ONLY 与模板哈希）；模板哈希写入绑定而不是文件内容 | TRD-INV-009 | PYT-0161 断言文件含 template_data_sha256=，此处背离 |
| S3-D53 | zh-CN 报价模板没有可移植的 Python 版式，以 DEPENDENCY_NOT_SUPPORTED 拒绝，支持须新的渲染器版本 | R15 | Python 无中文版式 |
| S3-D54 | 新增 FORMAL_DOCUMENT_MISSING、FORMAL_DOCUMENT_INTEGRITY_FAILURE、DOCUMENT_BINDING_MISSING（均 422）；已存模板格式错误为 REFERENCE_INVALID（TEMPLATE_INVALID 属 POL），纯文本模板超限为 CALCULATION_LIMIT_EXCEEDED | 错误码目录 | Python 超限为 FILE_INVALID（目录限 EVD、POL 使用） |
| S3-D55 | 编号的业务引用为 quote_id:revision，超过 64 字符时为 quote- 加其 SHA-256 的前 58 位十六进制 | R15 NumberIssue v1 | 格式同 Python；超长规则为新增 |
| S3-D56 | 每次读取都按本修订的绑定校验字节：DOCUMENT_BINDING_MISSING（含 r2 引用 r1 的文档）、BUSINESS_HASH_MISMATCH、FORMAL_DOCUMENT_MISSING、FORMAL_DOCUMENT_INTEGRITY_FAILURE | TRD-INV-009；BC-AT22 | — |
| S3-D57 | 文档打印的名称在事务前从所有者按绑定读取：PTY 绑定修订的客户名称、绑定行版本的联系人名称（已变或不属于该客户为 REFERENCE_STALE）、CAT 绑定规格修订的品名，规格标签为 sku_id r<修订>。名称不入 business_hash，由文档 SHA-256 固定 | R13、R14；TRD-INV-009 | — |
| S3-D58 | 指定地点打印 POL 贸易条款的指定地点；named_place_ref 是没有 Brant 上下文发布的 location，只进入成本范围绑定 | R15 | — |
| S3-D59 | 编号由 POL 的 number.issue 在 TRD 事务前以操作者身份分配（幂等键为 trd. 加 sha256(命令, 键) 前 56 位）；事务失败只在 POL 序列留下空号，重试取回同一编号。取号前在只读事务中试运行相同的准入，可预见的拒绝不消耗编号 | 战术模型（编号由 POL 在事务前分配）；R15；S1-D14 | — |
| S3-D60 | 激活与替代的事务顺序：本根 FOR UPDATE、权限守卫 FOR SHARE、可见性、对象控制、根版本 CAS、（替代）转换名额、放行、销售汇率时效、与冻结相同次序的上游守卫、渲染，最后一并写入根、文档、提交证据、修订状态、动作结果、审计与事件 | ADR-BR-011；TRD-INV-004、016 | 次序同 Python activate（先批准与审核人，后成本与配置） |
| S3-D61 | 正式文档字节与绑定存于 TRD 自己的 schema（bytea，CHECK 大小与 SHA-256）；事务内不调用 EVD，QuoteActivated 与 QuoteReplaced 携带 EVD 登记所需的文件引用 | 战术模型注记；R16 FileRef v1 | — |
| S3-D62 | 提交证据每修订一行，以命令与幂等键指称动作，条目为绑定、命令与 CST 可采纳结论，存为规范 JSON 并由数据库核对 SHA-256；读取只显示摘要与该 SHA-256 | TRD-INV-016 | 读取形式同 Python commit_history |
| S3-D63 | 数据库强制：修订有提交证据才能 active，只能被指名它的替代证据 superseded 且名额未占用，withdrawn 与 cancelled 须有关闭行；文档、提交证据、风险暂停与名额只能插入 | TRD-INV-009、010、016 | — |
| S3-D64 | 激活指定批准的旧效力版本为 VERSION_CONFLICT，先于 APPROVAL_REVOKED | TRD-INV-003 | 同 Python 公共适配器 |
| S3-D65 | 销售经办岗位模板增加 number.issue：激活与替代在事务前经 POL 取号，无该权限为 PERMISSION_DENIED 且不写入任何内容 | 负责人决定（2026-11-11），源于实现时发现的模板缺口；R15 | — |
| S3-D66 | quote.withdraw 结束 frozen 或 pending_approval 的修订，未决审核改为 cancelled，不撤销该修订的批准；quote.cancel 只取消 active 修订，其他修订为 QUOTE_NOT_CURRENT | SM-TRD-REVIEW、SM-TRD-REVISION | Python 撤回时审核改为 rejected 并撤销批准 |
| S3-D67 | 撤销 active 修订提交证据所载的批准时，同一事务登记风险暂停（APPROVAL_REVOKED）：修订仍为 active、历史不变，但查询、守卫与名额都拒绝它；不提供解除，替代或取消结束该修订 | UL TRD-RiskHold（首批不提供解除捷径）；SC-04 | Python 候选实现中风险暂停阻断 activate 与 replace（INVALID_STATE） |
| S3-D68 | 转换名额 full_v1 在根上，按租户、公司、报价与 full_v1 唯一；名额被占后 replace 与 cancel 为 ROOT_CONVERSION_LOCKED，守卫为 CONVERSION_SLOT_CONFLICT（均 409） | TRD-INV-010；SC-03 | — |
| S3-D69 | QuoteRevisionGuard v1（brant_trd.guard_quote_revision_active_v1，只授 brant_app）锁报价根 FOR SHARE，码为 REFERENCE_INVALID、BUSINESS_HASH_MISMATCH、QUOTE_NOT_CURRENT、暂停的码与 CONVERSION_SLOT_CONFLICT；修订查询只需有效授权，一次至多 200 个 | ADR-BR-004 方案 B；ADR-BR-011 | — |
| S3-D70 | quote.response.record 限人工或 AI；回复须答复 active、未暂停的正式修订及其哈希，是观察记录，不创建 Acceptance 或订单 | 命令目录（人工或 AI）；TRD-INV-011 | Python 只许人工 |
| S3-D71 | 回复只增不改：同 ID 同内容重放，内容不同为 RESPONSE_IMMUTABLE；操作者须能读取消息来源与每条证据，迟到回复照常登记并标记 late | TD-AT03；R16 | — |
| S3-D72 | 到达时间在登记时陈述并注明依据：channel_server 与 import_record 计入，source_claimed 保存但不计入；计入的到达须在正式化之后、登记之前，否则 ACCEPTANCE_TIME_CONFLICT | TRD-INV-012；EVD SourceRef v1 不带接收时间 | — |
| S3-D73 | 消息身份为回复的 source_ref（来源与定位符） | R16 SourceRef v1 | Python 为自由文本 source_message_id |
| S3-D74 | 核实依次拒绝：来源不可读、非 full_unconditional、回复已有在效 Acceptance、ID 已用、复述的修订、哈希、客户或联系人不同、行集合不完整、修订不再可用、到达在核实之后或不早于 expires_at、消息已支持在效 Acceptance、修订已有 valid Acceptance；码依次为目录对应行 | TRD-INV-011；BC-AT10、11 | 核实时重查修订同 Python |
| S3-D75 | 合同成立按到达时间：到达早于 expires_at 即成立，核实晚于到期也成立；没有可计入的到达时以服务端核实时间判定；到达不早于 expires_at 为 ACCEPTANCE_LATE，不创建任何内容 | ADR-BR-013；TRD-INV-012；TD-AT03；BC-AT12 | Python 在核实时刻不早于到期时返回 QUOTE_EXPIRED（WBS 登记的有意背离） |
| S3-D76 | 迟到承诺没有确认路径，视为客户的新要约，以新报价修订与批准答复；每个已存 Acceptance 都是已成立的合同，数据库强制 arrival_at < expires_at | TRD-INV-012 | — |
| S3-D77 | 每修订至多一个 valid Acceptance，每个回复、每条消息至多一个在效（valid 或 disputed）Acceptance；作废释放回复与消息，重新核实建立新 Acceptance | TRD-INV-011 | Python 允许一个修订有多个 Acceptance |
| S3-D78 | 效力账本：valid（版本 1）→ invalidated 或 disputed（版本 2），disputed → invalidated（版本 3），按 expected_effectivity_version CAS，须理由与可读的新证据，不回到 valid；每次变化记入账本，延迟约束触发器要求 Acceptance 与账本条目同时提交 | TRD-INV-017；负责人决定（2026-11-11）；战术模型已补 disputed → invalidated | — |
| S3-D79 | 有 valid 或 disputed Acceptance 的修订不能被替代或取消（ACCEPTANCE_ALREADY_EXISTS，409），由领域与状态触发器 quote_revision_contract_guard 强制，须先由人工作废该 Acceptance。决定也列出撤回，但撤回只结束 frozen 或待批修订，而回复只答复 active 修订，故对撤回不产生新的约束 | 负责人决定（2026-11-11）；ADR-BR-013 | — |
| S3-D80 | 核实不要求独立于登记人，核实限人工 | 岗位模板独立性清单无此项；Python T04 A37 | 同 Python |
| S3-D81 | 回复与 Acceptance 命令先锁报价根 FOR UPDATE 并通过对象控制，invalidate 与 mark_disputed 再锁 Acceptance；根版本不推进 | ADR-BR-011；TRD-INV-015 | — |
| S3-D82 | AcceptanceVerified 发布在 Acceptance 聚合上，与同一 Acceptance 的作废、争议同序；新增 AcceptanceDisputed，已补入 Context Map 事实 | 战术模型 | — |
| S3-D83 | AcceptanceGuard v1（brant_trd.guard_acceptance_valid_v1）先锁报价根、再锁 Acceptance FOR SHARE，与 QuoteRevisionGuard 同以根为先；判定效力与合同成立，不比较时钟与 expires_at，码为 REFERENCE_INVALID、ACCEPTANCE_SCOPE_MISMATCH、ACCEPTANCE_NOT_VALID | ADR-BR-004 方案 B；TRD-INV-017 | — |
| S3-D84 | 不实现：客户 PO 行的自动比对（由登记人分类）、以行为承诺（定金，需 STL）、FileRef v1 形式的证据（EVD 未发布文件查询）、Python 的 source_author 与 forward_chain_ref | TD-AT04；R16 | Python 有 source_author、forward_chain_ref |
| S3-D85 | order.prepare 把 active、未暂停的正式修订在其哈希上、依据形成合同的有效 Acceptance 转为草稿订单，并在同一事务占用转换名额（不在激活时占用）；第二次准备为 CONVERSION_SLOT_CONFLICT，名额永不释放 | TRD-INV-010；BC-AT15；SC-03、SC-07；ES-08 | — |
| S3-D86 | 订单快照逐字复制该修订的封存商业文档（数据、服务端计算、主体快照、依赖清单）与所绑定的 Acceptance，其 SHA-256 为订单哈希；不重新计算、不补全条款 | T03 A25；TRD-INV-014 | 同 Python t03_order_snapshot；无 trade_scope_ref |
| S3-D87 | 订单命令先锁所转换的报价根 FOR UPDATE（报价、Acceptance 与订单的冲突键，承载对象控制与名额），再锁订单 FOR UPDATE，再经账本锁权限守卫；TRD 在已锁聚合上判定 QuoteRevisionGuard 与 AcceptanceGuard 的结论 | 战术模型 lockOrder；ADR-BR-002、ADR-BR-011 | — |
| S3-D88 | order.prepare 与 order.review.request 允许人工或 AI（名额可由 AI 占用）；裁决、撤销、激活、取消与草稿态未履约关闭限人工 | 命令目录；人工专属清单 | — |
| S3-D89 | 订单审核每订单一个未决审核；裁决人须独立于准备人与请求人（REVIEWER_NOT_INDEPENDENT）并在裁决时持有 trd.read，陈述的订单哈希须一致（ORDER_REVIEW_MISMATCH），驳回须有理由；新增事件 OrderReviewRequested | TRD-INV-013；T04 A41；战术模型 | — |
| S3-D90 | 订单批准期限由审核人设定，须晚于裁决时刻（ORDER_REVIEW_EXPIRY_INVALID），不受报价到期限制 | ADR-BR-013；TRD-INV-013 | Python 以报价到期为上限（PYT-0481、0518） |
| S3-D91 | prepare、decide 与 activate 只检查 Acceptance 有效且已成立（ACCEPTANCE_NOT_VALID、ACCEPTANCE_LATE），从不以自身时刻对比报价 expires_at；报价到期后，按时成立的合同仍可准备、批准与生效订单 | TRD-INV-017；ADR-BR-013；BC-AT13；SC-07 | Python T03 A26 检查报价未到期（WBS 登记的有意背离） |
| S3-D92 | 合同成立后的上游变化（CST 来源否认或输入撤回、POL 配置退役或过期、销售汇率超龄、CAT 规格、INQ 询价、PTY 主体变化）不拒绝订单：每一项是带路径、码与细节的风险，批准须逐项确认，未确认为 ORDER_RISK_UNACKNOWLEDGED，确认未发现的风险为 REFERENCE_INVALID | ADR-BR-004；TRD-INV-014；SC-04 | — |
| S3-D93 | 主体筛查不是风险：订单激活经 PTY PartyScreeningGuard 判定所绑定主体为 clear，否则 PARTY_SCREENING_NOT_CLEAR | TRD-INV-021；TD-AT09 | — |
| S3-D94 | order.activate 的事务顺序：报价根与订单 FOR UPDATE、权限守卫 FOR SHARE；对象控制、草稿状态与快照完整；Acceptance、修订、名额与正式文档字节；正式化批准与审核人；订单批准与审核人；按上下文代码顺序的上游守卫（CAT、CNX 扩展点、CST、INQ、POL、PTY 含筛查、STL 扩展点）；风险确认；提交证据、订单 active、动作结果、审计与 OrderActivated | TRD-INV-014、016；ADR-BR-011 | — |
| S3-D95 | 订单步骤的正式化批准只看撤销与审核人当前资格，不看批准期限（quote.FormalApproval）；quote_ready 用另一检查 quote.ReadyApproval | ADR-BR-013；TRD-INV-014 | — |
| S3-D96 | 撤销订单批准只阻止新的激活，不倒写已生效订单；ActiveOrderRef v1 与 OrderGuard v1 向 PUR 报告撤销（ORDER_APPROVAL_REVOKED） | SM-TRD-ORDER-REVIEW；R19 | — |
| S3-D97 | 订单激活的提交证据记录承诺到达时间、风险确认、所用订单批准、正式化批准、CST 结论与各纪元，存为规范 JSON 并核对 SHA-256 | TRD-INV-016；SC-07 | — |
| S3-D98 | ActiveOrderRef v1 返回生效订单的行、金额、修订与哈希，并带订单的询价、成本快照与交付要求；OrderGuard v1（brant_trd.guard_order_active_v1）只锁订单行 FOR SHARE、不锁报价行，码为 REFERENCE_INVALID、REFERENCE_STALE、ORDER_NOT_ACTIVE、ORDER_APPROVAL_REVOKED | R19；PUR-INV-004、006；R21；ADR-BR-004 方案 B | — |
| S3-D99 | 草稿订单的 order.cancel 与 order.close_unfulfilled 须人工、理由与证据，名额保持占用；审计读取 order.check 在读事务内运行激活的全部检查，拒绝以结论表达 | SM-TRD-ORDER；TRD-INV-010；06 阶段契约 §07 | — |
| S3-D100 | quote_ready 依次判定：调用方写入会遇到的对象控制、修订、提交证据、文档字节、风险暂停、期限、批准与批准人和例外批准人的当前资格、上游守卫、当前输入上重新绑定的成本范围与销售条款、销售汇率时效；修订其间变化最多重试 3 次 | T03 A8、A34；R18 | Python 对任何已绑定的根报告 OBJECT_CONTROL_CONFLICT |
| S3-D101 | active 修订的批准已用尽：quote_ready 判定其撤销与批准人资格，不再报告 APPROVAL_EXPIRED；非 active 修订仍随批准过期 | 负责人决定（2026-11-11）；ADR-BR-013；TRD-INV-017 | Python 报告 APPROVAL_EXPIRED（PYT-0397） |
| S3-D102 | quote_ready 的理由保留目录码并注明检查项与路径；不询问 EVD（草稿证据已由冻结判定），重新绑定比较封存的文档形式 | 错误码目录；TRD-INV-002 | Python 映射到更窄的枚举，另需 costing.read 等权限 |
| S3-D103 | 修订、审批、文档、提交证据与 Outbox 历史按报价根分页（默认 50、至多 200），有序且不含载荷 | X-INV-003；T03 A9—A13 | Python 按 approval_id、document_id、event_id、commit_id 各自索引 |
| S3-D104 | Outbox 历史读取 bento.outbox 的真实投递状态（pending、leased、published、abandoned），涵盖报价根及其回复与 Acceptance 的事件；外部送达恒为 not_started | T03 A12—A13 | Python 恒为 queued |
| S3-D105 | 收件快照即封存的商业文档，不另建快照表；发送前准入按（报价、修订）判定并当下经 PTY 重查收件人；没有发送渠道，发送目标恒不就绪（EXTERNAL_CHANNEL_NOT_CONFIGURED） | T03 A14—A16 | Python 另存发送目标快照，只在激活时检查收件人 |
| S3-D106 | 同键重放与对账在事务前经 EVD 重查命令所引来源，账本的可见性钩子在事务内据此拒绝（越出范围为 REFERENCE_INVALID，无 evd.read 为 PERMISSION_DENIED），哈希相同与不同同样适用；覆盖引用来源的报价、回复、Acceptance 与订单命令，与首次提交竞争的重放按 REFERENCE_INVALID 拒绝 | 06 阶段契约 §07；X-INV-003；R16 | Python A45 同样在重放时重查 |
| S3-D107 | 新增查询码 COMMIT_EVIDENCE_MISSING、APPROVAL_MISSING、RECIPIENT_SNAPSHOT_INTEGRITY_FAILURE、EXTERNAL_CHANNEL_NOT_CONFIGURED（目录行，422）；八个读取都需 trd.read 并审计 | 错误码目录；X-INV-003 | — |
| S3-D108 | PUR 契约为 brant.purchasing/0.1.0，草稿文档契约为 brant.purchase-draft/0.2.0；purchase.draft.prepare 与 purchase.draft.void 限人工并记录权限纪元 | PUR-INV-001；12 阶段仓库结构 | Python 草稿契约为 0.1.0 |
| S3-D109 | 一个订单行可拆分给多个草稿与供应商，至其数量为止；DemandAllocation 按（order_id、order_line_id）为根，超额为 PURCHASE_ALLOCATION_EXCEEDED，并发只有一个成功 | PUR-INV-002；TD-AT05、TD-AT06 | Python 每行一个草稿（PURCHASE_LINE_ALREADY_DRAFTED） |
| S3-D110 | 采购量为分配量加超采；超采须标理由并由准备人在事务内持有 purchase.overbuy.approve，其成本不计入 sold_cost_minor；MOQ 与订购倍数只在同一草稿、同一来源内计算，不自动上调 | PUR-INV-005 | Python 要求采购量等于订单量 |
| S3-D111 | purchase.overbuy.approve 在 S3 不授任何岗位，超采实际关闭；独立批准与 ES-07 的阈值在 S5 与采购承诺一并决定 | 负责人决定（2026-11-11）；ES-07 | — |
| S3-D112 | purchase.draft.void 只作用于 prepared 或 supply_expired、未承诺的草稿，须人工与理由；释放分配、保留记录 | PUR-INV-007；SM-PUR-DRAFT | Python 无作废 |
| S3-D113 | 草稿准备一次即不可变，数据库 CHECK 重算请求与文档的 SHA-256；状态为 prepared、supply_expired（读取时推导）与 voided，篡改的记录在读取、重放与作废时为 REFERENCE_INVALID | PUR-INV-001；SM-PUR-DRAFT | Python 状态为 draft |
| S3-D114 | prepare 先锁草稿行，再按（order_id、order_line_id）锁分配根，再锁权限守卫，守卫按上下文代码顺序 CAT、CST、POL、PTY、SUP、TRD 调用并各按键排序；void 先锁草稿再锁分配根，两者不成环 | ADR-BR-011；ADR-BR-004 方案 B | — |
| S3-D115 | PTY 新增 PartyGuard v1（brant_pty.guard_party_current_v1，锁主体行 FOR SHARE）；供应商须在供应条件绑定的行版本上为已确认，R22 登记为 PartyRef v1＋PartyGuard v1 | R22 | Python 检查当前主数据版本 |
| S3-D116 | R52 CAT→PUR 改为守卫锁，PUR 在事务内调用 SkuGuard v1；规格撤回为 REFERENCE_STALE，停用 SKU 为 REFERENCE_INVALID | 负责人决定（2026-11-11）；R52 | — |
| S3-D117 | 核价对照只记录不阻断：采购成本为快照行成本按分配量占订单量的比例（HALF_UP），跨币种差异为 null，可采纳结论写入文档 | R21；PUR-INV-004 | 全量分配时同 Python |
| S3-D118 | 未核实的供应条件工作修订为 REFERENCE_INVALID（SUP 查询不含草稿） | R20 | Python 为 SUPPLY_CONDITION_NOT_CURRENT |
| S3-D119 | 准备只需 purchase.draft.prepare，上游查询只需有效授权并遵守数据范围；去掉 purchase_trade_scope_ref，交付地点按地点 ID 比较 | S2-D06、S2-D12 | Python 另需 quote.read、read、costing.read、foundation.read |
| S3-D120 | PUR 使用目录未列 PUR 的 PRICE_TIER_AMBIGUOUS、UNSUPPORTED_PRICING_MODE、DEPENDENCY_NOT_SUPPORTED、TAX_TREATMENT_UNKNOWN（重述的 CST 计价规则），不使用 PURCHASE_LINE_ALREADY_DRAFTED 与 OBJECT_CONTROL_CONFLICT | 错误码目录；S2-D01 | — |
| S3-D121 | PUR 根没有对象控制（PYT-0571 留 S4）；没有 EVD→PUR 关系，作废只带理由 | 战术模型；Context Map | — |
| S3-D122 | 生效订单的结束为 order.cancel（无出运、下游承诺已处置）、order.close_remainder（已有出运、放弃余量）与 order.close（各行在批准容差内足量出运）；order.close_unfulfilled 只用于草稿，名额与全部修订保留 | SM-TRD-ORDER；TRD-INV-019 | Python 未实现 |
| S3-D123 | 修订 1 即订单快照，之后每个修订为 brant.order-revision/0.1.0 规范文档并指名前一修订的哈希；当前修订与哈希仍在订单行上，OrderGuard v1 与 ActiveOrderRef v1 随变更前进，旧修订的绑定为 REFERENCE_STALE | TRD-INV-018；R19 | Python 未实现 |
| S3-D124 | 变更提议是完整的下一版内容，金额按报价计算的规则重算；变更哈希（brant.order-amendment/0.1.0）覆盖基础、来源、规范化提议与影响评估，不覆盖派生金额 | TRD-INV-018 | — |
| S3-D125 | 已出运行的数量不低于已出运量（QUANTITY_BELOW_SHIPPED）并保留 SKU、价格与税，余量重新定价须拆成新行；新增费用为 DEPENDENCY_NOT_SUPPORTED；客户、联系人、币种、来源与行单位不可改 | TRD-INV-018；TD-AT01 | — |
| S3-D126 | 变更 prepare 限人工或 AI；submit 须评估七个影响领域（DRAFT_NOT_READY）；decide 须独立于每个准备人与提交人并在裁决时持有 trd.read；沿用订单审批的码 | SM-TRD-ORDER-AMENDMENT；ES-09 | — |
| S3-D127 | 变更命令只推进变更自身版本，apply 推进订单行版本；两个变更同时应用以 ORDER_AMENDMENT_STALE 裁决 | TRD-INV-018 | — |
| S3-D128 | apply 先锁报价根与订单、再锁权限守卫，重读裁决人授权，再对提议采用的规格与 POL 修订调用 SkuGuard 与 ConfigGuard（内容哈希须一致）；变更不重查毛利 | ADR-BR-011；TRD-INV-016 | — |
| S3-D129 | FUL、PUR、STK、STL 尚未建立：结束与应用把出运与下游承诺记为 not_wired，S3 内 close 恒为 ORDER_NOT_FULFILLED、close_remainder 为 INVALID_STATE；变更后 STL、CNX 结论同样记 not_wired | TRD-INV-019、022、023；负责人决定（2026-11-10） | — |
| S3-D130 | 原订单批准被撤销后，OrderGuard v1 在变更后仍答 ORDER_APPROVAL_REVOKED | TRD-INV-018（原审批不授权新修订，未言其撤销） | — |
| S3-D131 | 事件 OrderAmendmentRequested（每次准备）、OrderAmendmentDecided、OrderAmended、OrderAmendmentWithdrawn、OrderRemainderClosed、OrderClosed 在 SalesOrder 分区；提交不发事件，新事件已补入 Context Map 事实 | ES-09 第 2 步；战术模型 | — |
| S3-D132 | 迁移 trd/000006 的变更、订单修订、变更提交证据与结束各表 FORCE RLS 并由触发器守护：生效订单带修订行与提交证据才能前进修订，带结束行才能结束；snapshot_hash 回填语句暂时解除 FORCE RLS | TRD-INV-016、018、019 | — |
| S3-D133 | S/C 与 PI 渲染器 order-text/0.1.0：PI 声明非商业发票、非税务发票，S/C 签署区注明生成件未签署；POL 新增 order_document_template 与编号单据类型 proforma_invoice（S/C 用 order），每订单每种单据一个编号，修订共用 | TRD-INV-020；S1-D14 | Python 未实现 |
| S3-D134 | PI 不打印银行账户：完整账号需 pol.read_sensitive，单据岗位都不持有 | S1-D20 | — |
| S3-D135 | order.document.generate（人工或 AI）以 POL order_document_template 与 order-text/0.1.0 渲染订单当前修订的 S/C 或 PI；事务前试运行与取号（S/C 用 order，PI 用 proforma_invoice，每单每种一个号，文件名 <编号>-r<修订>.txt），事务内锁报价根、订单与权限守卫，ConfigGuard 重查模板与号段，绑定、字节与提交证据一行写入 | TRD-INV-020；正式化的顺序与取号（S3-D49、S3-D59） | —（Python 未实现） |
| S3-D136 | 订单修订推进后，按旧修订生成为 ORDER_DOCUMENT_STALE（/args/expected_order_revision），旧修订文档的签署件亦然；与应用变更竞争时生成等待并写入为零，两次并发生成只有一个成功 | TRD-INV-020；TD-AT01 | — |
| S3-D137 | law 区块打印来源报价封存的法律条款，随订单修订与变更保留，从不读取 POL 的当前条款；模板有无 law 区块须与订单有无法律条款一致，否则 REFERENCE_INVALID | S3-D141、S3-D145；F-02 | — |
| S3-D138 | 草稿订单可生成单据（SIM-02 第 6 步在生效前生成 PI 并登记会签件）；已取消或关闭的订单不再生成（INVALID_STATE），其当前单据仍可登记签署件 | SIM-02；TRD-INV-020 | — |
| S3-D139 | order.document.record_signed（仅人工）以 EVD 业务文件登记签署件或会签件：事务前读取文件的 SHA-256 与 MIME，生成的未签署文件不能作为签署件（INVALID_KNOWN_VALUE），同一种签署只登记一个文件（INVALID_STATE），重复登记同一文件不写入；重放与对账重查所引来源。成员名为 signature_kind（传输层保留 kind） | UL TRD-SalesContractDocument；S3-D106 | — |
| S3-D140 | 报价后修改的联系人阻止该订单生成单据（REFERENCE_STALE，/order/contact_ref），与正式化相同；PTY 没有联系人历史读取。迁移 trd/000007 的单据与签署件表 FORCE RLS、只插入，触发器绑定订单当前修订；事件 OrderDocumentGenerated、OrderDocumentSigned | S3-D57；R13 | — |
| S3-D141 | POL 新增 legal_terms（适用法律、CISG 适用或排除、成立规则、争议解决），与 order_document_template 一起使 POL 共 20 种配置；适用 CISG 或中国法时成立规则须为 arrival（INVALID_KNOWN_VALUE） | F-02；CISG 第 18(2)、23 条；民法典第 137、483、484 条 | Python 未实现 |
| S3-D142 | 租户默认为公司档案的 default_legal_terms（确切修订），不设保留的默认配置；冻结采用草稿所指条款，否则采用开票方默认，封存 legal_terms（来源、适用法律、CISG、争议解决、成立规则与依据）并列入依赖清单 | F-02；R15 | — |
| S3-D143 | TRD 自行推导成立规则：CISG 适用为 arrival、依据 cisg，中国法为 arrival、依据 prc_civil_code，其他法律或规则在冻结时为 DEPENDENCY_NOT_SUPPORTED，不含 CISG 的德国法待法务确认；CN 只指中国大陆法律 | ADR-BR-013；F-02 | — |
| S3-D144 | 无法律条款的修订按默认（CISG 与中国法，到达主义）成立，此前冻结的修订均如此，哈希与渲染逐字节不变 | ADR-BR-013 | — |
| S3-D145 | 法律条款随 ConfigGuard 在冻结、激活与替代、quote_ready、订单检查、审核与激活时重查；订单快照携带封存的条款，条款退役在订单上成为待确认的风险 | ADR-BR-004；R15 | — |
| S3-D146 | quote-text/0.2.0 增加 legal_terms 区块，0.1.0 保持 Python 版式；0.1.0 模板配法律条款为 DEPENDENCY_NOT_SUPPORTED，0.2.0 模板无条款为 DRAFT_NOT_READY | TRD-INV-009 | 0.1.0 同 Python |
| S3-D147 | 核实按修订封存的成立规则判定，acceptance_verified.v1 增加 formation_rule 与 formation_basis；成立由封存文档推导，不新增 TRD 迁移，AcceptanceGuard v1 不变 | ADR-BR-013；TRD-INV-012 | — |
| S3-D148 | legal_terms 与 default_legal_terms 由管理员起草，第二人发布，与其他 POL 配置相同；岗位模板不变 | 负责人决定（2026-11-11）；POL-INV-001 | — |
| S3-D149 | TRD 的 35 个命令经通用反射命令处理器路由，对象成员保留 T03、T04 的名称（quote_id、response_id、acceptance_id），Line.Sku 在线上名为 sku_ref | T03、T04 契约；S2-D21 的例外 | 成员名同 Python |
| S3-D150 | 命令路由返回通用回执（action_id、object_id、row_version、status、replayed）；扩展回执的成员经报价、正式文档、回复、Acceptance、订单与采购草稿的读取路由取得 | S1-06 | — |
| S3-D151 | 读取路由在 /api/brant/v1/trd/ 下有 quotes 及其 active、approvals 检查、revisions 文档、ready、history、approvals、documents、commits、outbox、dispatch_preflight、dispatch_target，orders 及其 check，customer_responses、acceptances，在 /api/brant/v1/pur/ 下有 purchase_drafts 与订单 allocations；查询参数封闭，未知、重复或格式错误为 SCHEMA_INVALID | 06 阶段契约 §07；S1-06 | — |
| S3-D152 | 动作对账 POST /api/brant/v1/{bc}/commands/{command}/reconcile 收取原样请求并按命令路由校验，答复 committed（附已存回执）、payload_conflict 或 not_committed，不执行命令；INQ、CST、TRD、PUR 的全部命令可对账 | 06 阶段契约 §07（A22、A23、A43）；X-INV-003 | — |
| S3-D153 | PTY、CST、CAT、INQ 发布给下游的引用查询（ResolveParties、ResolveTradingProfiles、ResolveAddressRoles；ResolveSnapshots、ResolveContexts、ResolveMeasurements；ResolveSkus、ResolveSpecs；ResolveRfqs）与 POL 的 ResolveConfigs 一样只需有效授权，调用方数据范围外的记录视为不存在；Brant 没有传递的依赖读取权限，冻结、正式化、quote_ready 与订单激活只要求命令自身的权限与报价可见性。审计读取仍须读取权限：所引证据须 evd.read，正式文档打印的客户、联系人与品名须 pty.read 与 cat.read | S1-D15 的扩展；各上下文 api 的查询约定 | Python 冻结、绑定交易档案与联系人、订单激活缺 costing.read、foundation.read、quotation_foundation.read 任一即 PERMISSION_DENIED（PYT-0253、0354、0371、0402） |


## 03 Python 测试移植与 V0.50 等价性 {#s3}

08 阶段映射表中 T03 的 300 个与 T04 的 117 个测试方法，连同共享基础 PYT-0135 的渲染一半，共 418 行，全部有结论：314 个已移植（PORTED，其中 17 个缺次要部分，注明于行内），28 个有意背离（DIVERGES_BY_DESIGN），67 个为 Python 实现专属（NOT_APPLICABLE），9 个未移植（NOT_PORTED）。计为已移植的行，均有树内 Go 测试在注释中以映射编号或 Python 测试名引用它，且测试确实判定该行为。有意背离逐条引用 05 阶段不变量、ADR 或负责人决定，按 Brant 的规则判定，不按 Python 行为移植；未移植的行均写明业务理由。

### 总计

| 范围 | 行数 | PORTED | 其中缺次要部分 | DIVERGES_BY_DESIGN | NOT_APPLICABLE | NOT_PORTED |
|---|---|---|---|---|---|---|
| T03 | 300 | 219 | 10 | 22 | 51 | 8 |
| T04 | 117 | 94 | 6 | 6 | 16 | 1 |
| PYT-0135（共享基础，渲染一半） | 1 | 1 | 1 | 0 | 0 | 0 |
| S3 合计 | 418 | 314 | 17 | 28 | 67 | 9 |
| 08 阶段映射表全表（577 行） | 577 | 455 | 17 | 30 | 79 | 13 |

### 按 Python 测试文件

| Python 测试文件 | 切片 | 行数 | PORTED | DIVERGES | N/A | NOT_PORTED |
|---|---|---|---|---|---|---|
| `test_foundation.py` | 共享基础 V0.2 | 1 | 1 | 0 | 0 | 0 |
| `test_t03_a19_configuration_consumption.py` | T03 A19 | 5 | 2 | 1 | 1 | 1 |
| `test_t03_a20_acceptance_order.py` | T03 A20 | 8 | 7 | 1 | 0 | 0 |
| `test_t03_a23_reconciliation_access.py` | T03 A23 | 15 | 12 | 0 | 3 | 0 |
| `test_t03_a24_write_control.py` | T03 A24 | 14 | 13 | 0 | 1 | 0 |
| `test_t03_a25_order_snapshot.py` | T03 A25 | 25 | 14 | 0 | 7 | 4 |
| `test_t03_a26_order_qualification.py` | T03 A26 | 34 | 17 | 10 | 6 | 1 |
| `test_t03_a27_cost_scope.py` | T03 A27 | 36 | 30 | 0 | 6 | 0 |
| `test_t03_a28_sales_terms.py` | T03 A28 | 50 | 40 | 3 | 6 | 1 |
| `test_t03_a29_trade_parties.py` | T03 A29 | 16 | 10 | 2 | 4 | 0 |
| `test_t03_a30_contact_profiles.py` | T03 A30 | 15 | 6 | 2 | 6 | 1 |
| `test_t03_a31_address_roles.py` | T03 A31 | 4 | 2 | 0 | 2 | 0 |
| `test_t03_a32_sales_fx.py` | T03 A32 | 2 | 1 | 0 | 1 | 0 |
| `test_t03_a33_reviewer_qualification.py` | T03 A33 | 7 | 7 | 0 | 0 | 0 |
| `test_t03_a34_ready_reviewer.py` | T03 A34 | 3 | 3 | 0 | 0 | 0 |
| `test_t03_a35_time_qualification.py` | T03 A35 | 6 | 4 | 1 | 1 | 0 |
| `test_t03_a6_authority.py` | T03 A6 | 3 | 2 | 1 | 0 | 0 |
| `test_t03_configuration_runtime.py` | T03 A18 | 6 | 5 | 1 | 0 | 0 |
| `test_t03_cost_binding.py` | T03 | 3 | 2 | 0 | 1 | 0 |
| `test_t03_formalization.py` | T03 A4 | 5 | 5 | 0 | 0 | 0 |
| `test_t03_persistence.py` | T03 A1–A5 | 7 | 3 | 0 | 4 | 0 |
| `test_t03_query_contract.py` | T03 A8 | 12 | 12 | 0 | 0 | 0 |
| `test_t03_quote_review.py` | T03 A3 | 5 | 5 | 0 | 0 | 0 |
| `test_t03_server_calculation.py` | T03 A5 | 11 | 11 | 0 | 0 | 0 |
| `test_t03_snapshot.py` | T03 | 2 | 1 | 0 | 1 | 0 |
| `test_t03_write_contract.py` | T03 A17 | 6 | 5 | 0 | 1 | 0 |
| `test_t04_a36_customer_response.py` | T04 A36 | 7 | 4 | 1 | 2 | 0 |
| `test_t04_a37_response_verification.py` | T04 A37 | 11 | 8 | 1 | 2 | 0 |
| `test_t04_a38_order_review.py` | T04 A38 | 8 | 5 | 2 | 1 | 0 |
| `test_t04_a39_order_gate.py` | T04 A39 | 21 | 20 | 0 | 1 | 0 |
| `test_t04_a40_public_revoke.py` | T04 A40 | 6 | 5 | 0 | 1 | 0 |
| `test_t04_a41_public_review.py` | T04 A41 | 5 | 4 | 1 | 0 | 0 |
| `test_t04_a42_public_review_query.py` | T04 A42 | 4 | 4 | 0 | 0 | 0 |
| `test_t04_a43_order_reconciliation.py` | T04 A43 | 3 | 3 | 0 | 0 | 0 |
| `test_t04_a44_public_customer_response.py` | T04 A44 | 4 | 4 | 0 | 0 | 0 |
| `test_t04_a45_response_query_recovery.py` | T04 A45 | 26 | 18 | 0 | 8 | 0 |
| `test_t04_a46_purchase_draft.py` | T04 A46 | 22 | 19 | 1 | 1 | 1 |

### 有意背离（DIVERGES_BY_DESIGN）

| 编号 | Python 测试 | 固定 Go 行为的测试 | 依据与差异 |
|---|---|---|---|
| PYT-0164 | `test_t03_a19_configuration_consumption.py::test_configuration_reference_requires_conditional_read_permission` | TestQuoteReadyReflectsTheActiveRevisionNow | S1-D15；POL 配置属于公司，TRD 经 POL 查询只需有效授权，冻结与激活不要求配置读取权限，Python 返回 PERMISSION_DENIED（只持 trd.read 的读者经上游查询得出 ready：TestQuoteReadyReflectsTheActiveRevisionNow） |
| PYT-0167 | `test_t03_a20_acceptance_order.py::test_late_and_duplicate_confirmation_are_rejected_without_residue` | TestAcceptanceArrivingBeforeExpiryFormsTheContractAfterExpiry、TestVerificationScopeImmutabilityAndUniqueness、TestOneLiveAcceptancePerResponseMessageAndRevision | TRD-INV-012、ADR-BR-013；承诺按到达时间判定，到达早于报价到期即成立合同，核实晚于到期亦然，到达不早于到期为 ACCEPTANCE_LATE 且无残留；Python 按核实时刻返回 QUOTE_EXPIRED（TestAcceptanceArrivingBeforeExpiryFormsTheContractAfterExpiry）；重复消息一半已移植（SOURCE_MESSAGE_DUPLICATE） |
| PYT-0228 | `test_t03_a26_order_qualification.py::test_current_cost_denial_blocks_new_activation_without_residue` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；合同成立后成本来源被否认是须由订单审批确认的风险，未确认为 ORDER_RISK_UNACKNOWLEDGED 且无残留，确认后激活；Python 以 COST_SNAPSHOT_NOT_ELIGIBLE 拒绝（TestUpstreamChangesAfterApprovalAreRisksToAcknowledge） |
| PYT-0229 | `test_t03_a26_order_qualification.py::test_retired_template_blocks_new_activation_without_residue` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；合同成立后配置停用是须确认的风险（CONFIG_NOT_ACTIVE），确认后激活；Python 以 REFERENCE_STALE 拒绝；Go 以停用销售税适用演示（TestUpstreamChangesAfterApprovalAreRisksToAcknowledge） |
| PYT-0235 | `test_t03_a26_order_qualification.py::test_quote_expiration_blocks_uncommitted_order_even_if_acceptance_was_timely` | TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract | ADR-BR-013、TRD-INV-014、TRD-INV-017；合同成立后报价到期不阻断订单激活；Python 返回 QUOTE_EXPIRED（TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract） |
| PYT-0236 | `test_t03_a26_order_qualification.py::test_approval_expiry_is_exclusive_and_does_not_change_order` | TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract | ADR-BR-013、TRD-INV-014；正式化批准的期限在正式化时已判定，订单激活只查其未撤销、审核人仍合格；Python 在批准期限届满时为 APPROVAL_STALE（TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract） |
| PYT-0246 | `test_t03_a26_order_qualification.py::test_end_of_check_approval_expiry_rolls_back_before_state_change` | TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract | ADR-BR-013、TRD-INV-014；订单激活不判定正式化批准的期限，检查中途跨过期限不阻断，全部检查以同一时刻判定；Python 为 APPROVAL_STALE（TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract） |
| PYT-0253 | `test_t03_a26_order_qualification.py::test_dependency_read_grants_are_required_for_new_activation` | TestAccessNeedsAnActiveGrantButNoPermission | S3-D153（S1-D15 的扩展）；上游查询只需有效授权，订单激活不要求 costing.read、foundation.read、quotation_foundation.read 这类依赖读取权限，Python 缺任一即 PERMISSION_DENIED（TestAccessNeedsAnActiveGrantButNoPermission）；激活仍需 order.activate 与报价可见性 |
| PYT-0254 | `test_t03_a26_order_qualification.py::test_cost_denial_committing_first_blocks_waiting_activation` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；先提交的成本否认使等待的激活拒绝为 ORDER_RISK_UNACKNOWLEDGED 且无残留，经新审批确认后可激活；Python 为 COST_SNAPSHOT_NOT_ELIGIBLE；两种提交次序已移植（TestUpstreamChangesAfterApprovalAreRisksToAcknowledge） |
| PYT-0256 | `test_t03_a26_order_qualification.py::test_retired_cost_pricing_policy_blocks_new_activation` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；合同成立后成本所用定价政策停用是须订单审批确认的风险：未确认为 ORDER_RISK_UNACKNOWLEDGED，确认后激活；Python 拒绝（COST_SNAPSHOT_NOT_ELIGIBLE） |
| PYT-0258 | `test_t03_a26_order_qualification.py::test_end_of_check_quote_expiry_rolls_back_before_state_change` | TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract | ADR-BR-013、TRD-INV-017；检查中途报价到期不阻断已成立合同的订单激活，全部检查以同一时刻判定；Python 为 QUOTE_EXPIRED（TestOrderActivatesAfterTheQuoteExpiredOnATimelyContract） |
| PYT-0260 | `test_t03_a26_order_qualification.py::test_sales_fx_is_current_for_new_activation_and_staleness_is_not_ignored` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；销售汇率超龄是须订单审批确认的风险，不拒绝；Python 为 FX_STALE；PostgreSQL 测试世界没有销售汇率，规则由成本与配置用例固定（TestUpstreamChangesAfterApprovalAreRisksToAcknowledge） |
| PYT-0336 | `test_t03_a28_sales_terms.py::test_freeze_to_formalization_rechecks_read_authority` | TestNumberRenderAndFileFailuresLeaveNothing | S1-D15；POL 配置属于公司，激活读取配置只需有效授权，不要求配置读取权限（Python 缺 quotation_foundation.read 为 PERMISSION_DENIED）；激活前对 PTY 读取权限的重查见 TestNumberRenderAndFileFailuresLeaveNothing |
| PYT-0343 | `test_t03_a28_sales_terms.py::test_new_order_activation_rechecks_sales_fee_after_prepare` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；订单准备后停用的配置是订单审批须确认的风险（ORDER_RISK_UNACKNOWLEDGED），确认后可激活，Python 拒绝（CONFIG_NOT_ACTIVE）；测试以销售税适用演示，费用类别经同一 ConfigGuard 路径 |
| PYT-0344 | `test_t03_a28_sales_terms.py::test_new_order_activation_rechecks_sales_application_after_prepare` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；订单准备后停用的配置是订单审批须确认的风险（ORDER_RISK_UNACKNOWLEDGED），确认后可激活，Python 拒绝（REFERENCE_STALE）；测试以销售税适用演示 |
| PYT-0354 | `test_t03_a29_trade_parties.py::test_profile_read_authority_is_required_at_use` | TestQuoteReadyReflectsTheActiveRevisionNow、TestNumberRenderAndFileFailuresLeaveNothing | S3-D153（S1-D15 的扩展）；冻结经 PTY 查询只需有效授权，不要求 pty.read，Python 绑定时返回 PERMISSION_DENIED（仅持 trd.read 的读者经上游查询：TestQuoteReadyReflectsTheActiveRevisionNow）；激活读取客户与联系人名称须 pty.read（TestNumberRenderAndFileFailuresLeaveNothing） |
| PYT-0359 | `test_t03_a29_trade_parties.py::test_order_activation_rechecks_profile` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge、TestTradingProfileGuardDecidesEveryCommitOrder | ADR-BR-004、TRD-INV-014；订单激活时交易档案失效是须订单审批确认的风险（ORDER_RISK_UNACKNOWLEDGED），确认后可激活，Python 直接拒绝（CONFIG_NOT_ACTIVE）（TestUpstreamChangesAfterApprovalAreRisksToAcknowledge）；PTY 守卫一半见 TestTradingProfileGuardDecidesEveryCommitOrder |
| PYT-0371 | `test_t03_a30_contact_profiles.py::test_contact_profile_foundation_read_authority_is_required` | TestQuoteReadyReflectsTheActiveRevisionNow、TestNumberRenderAndFileFailuresLeaveNothing | S3-D153（S1-D15 的扩展）；Brant 无 contact_profile，冻结经 PTY 查询只需有效授权，不要求读权限，Python 返回 PERMISSION_DENIED（TestQuoteReadyReflectsTheActiveRevisionNow）；激活打印联系人姓名须 pty.read（TestNumberRenderAndFileFailuresLeaveNothing） |
| PYT-0373 | `test_t03_a30_contact_profiles.py::test_new_order_rechecks_contact_profile` | TestUpstreamChangesAfterApprovalAreRisksToAcknowledge | ADR-BR-004、TRD-INV-014；Brant 无 contact_profile，订单激活时联系人修订经 TradingProfileGuard 成为须确认的风险（ORDER_RISK_UNACKNOWLEDGED），Python 直接拒绝（TestUpstreamChangesAfterApprovalAreRisksToAcknowledge 以主体修订测试同一路径） |
| PYT-0397 | `test_t03_a35_time_qualification.py::test_ready_compares_quote_and_approval_offsets_as_instants` | TestQuoteReadyComparesInstants、TestFormalApprovalStandsOnCurrentQualification | 负责人决定 2026-11-11、ADR-BR-013、TRD-INV-017；active 修订的批准已用尽，报价到期后 quote_ready 只报 QUOTE_EXPIRED，Python 同时报 APPROVAL_EXPIRED；按 UTC 时刻比较已移植 |
| PYT-0402 | `test_t03_a6_authority.py::test_structured_freeze_requires_transitive_cost_and_foundation_reads` | TestQuoteReadyReflectsTheActiveRevisionNow、TestAccessNeedsAnActiveGrantButNoPermission | S3-D153（S1-D15 的扩展）；冻结经 CST、PTY、POL 等上游查询只需有效授权，不要求 costing.read 等读权限，Python 返回 PERMISSION_DENIED（TestQuoteReadyReflectsTheActiveRevisionNow、TestAccessNeedsAnActiveGrantButNoPermission）；所引证据仍须 evd.read |
| PYT-0406 | `test_t03_configuration_runtime.py::test_scope_permissions_ai_and_object_control_are_enforced` | TestIndependentPublicationAndAIGuard | S1-D15；POL 配置属于公司、不设数据范围归属，own 范围读者也能读取草稿，Python 返回 NOT_FOUND；AI 发布被拒见 TestIndependentPublicationAndAIGuard（未引用本测试）；配置没有对象控制（只在报价根） |
| PYT-0467 | `test_t04_a36_customer_response.py::test_task_binding_and_human_required` | TestResponseAndAcceptanceWritesPassTheObjectControl、TestCustomerResponseRecordAndVerifyLifecycle | X-INV-002；quote.response.record 不在人工专属清单，命令目录为人工或 AI，AI 可登记客户回复（TestCustomerResponseRecordAndVerifyLifecycle），Python 拒绝 AI；绑定根须任务声明的一半已移植 |
| PYT-0473 | `test_t04_a37_response_verification.py::test_expiry_boundary_and_historical_replay` | TestAcceptanceArrivingBeforeExpiryFormsTheContractAfterExpiry、TestTimelyAcceptanceFormsTheContractWhenVerifiedAfterExpiry | ADR-BR-013、TRD-INV-012、TRD-INV-017；合同按到达时间成立，到达早于 expires_at 的承诺在报价到期后核实仍形成 Acceptance，到达不早于 expires_at 为 ACCEPTANCE_LATE；Python 在核实时刻不早于到期即为 QUOTE_EXPIRED。到期后重放与对象控制一半已移植 |
| PYT-0481 | `test_t04_a38_order_review.py::test_expiry_and_reviewer_current_authority` | TestOrderReviewRulesAndIndependence、TestOrderReviewDecisions | TRD-INV-013、ADR-BR-013；订单审批有效期由订单审批自身设定，晚于报价 expires_at 的批准被接受，Python 拒绝；无时区时间（SCHEMA_INVALID）、审核人失去授权与审批到期（ORDER_APPROVAL_EXPIRED，TestOrderActivationGate 以 PYT-0491、0492 引用）一半已移植 |
| PYT-0482 | `test_t04_a38_order_review.py::test_access_task_and_human_controls` | TestOrderReviewRulesAndIndependence | X-INV-002；order.review.request 不在人工专属清单，命令目录为人工或 AI，AI 可请求订单审核，Python 拒绝；裁决限人工已移植；他范围主体与绑定根须任务声明在激活上见 TestOrderActivationGate（未引用本测试） |
| PYT-0518 | `test_t04_a41_public_review.py::test_public_decision_expiry_and_quote_bound_are_checked` | TestOrderReviewRulesAndIndependence、TestOrderCommandContracts | TRD-INV-013、ADR-BR-013；订单审批有效期不受报价 expires_at 限制，Python 为 ORDER_REVIEW_EXPIRY_INVALID；无时区时间为 SCHEMA_INVALID 已移植（TestOrderCommandContracts） |
| PYT-0570 | `test_t04_a46_purchase_draft.py::test_identity_permissions_and_data_scope` | TestIdentityPermissionsAndDataScope | S3-D119、S3-D153；编制只需 purchase.draft.prepare，上游查询只需有效授权并按数据范围，持 purchase.draft.prepare 而缺 costing.read 的编制人可以编制，Python 返回 PERMISSION_DENIED（TestIdentityPermissionsAndDataScope）；AI 被拒、他公司与数据范围外为 NOT_FOUND 已移植 |

依据归类：ADR-BR-004 与 TRD-INV-014（订单激活时的上游变化是须由订单审批确认的风险）PYT-0228、0229、0254、0256、0260、0343、0344、0359、0373；ADR-BR-013、TRD-INV-012、014、017（合同按到达成立，成立后报价与正式化批准到期不阻断订单）PYT-0167、0235、0236、0246、0258、0473；TRD-INV-013（订单审批有效期与报价有效期无关）PYT-0481、0518；负责人决定 2026-11-11（active 修订的批准已用尽）PYT-0397；X-INV-002（人工专属以清单为准，回复登记与订单审核请求为人工或 AI）PYT-0467、0482；S1-D15（POL 配置属于公司，查询只需有效授权）PYT-0164、0336、0406；S3-D153（PTY、CST、CAT、INQ 的引用查询只需有效授权，没有传递的依赖读取权限）PYT-0253、0354、0371、0402，与 S3-D119 一起 PYT-0570。

### 未移植（NOT_PORTED）

**能力或关系不存在（5）**

| 编号 | Python 测试 | 说明 |
|---|---|---|
| PYT-0225 | `test_t03_a25_order_snapshot.py::test_acceptance_line_scope_corruption_rejects_new_order_atomically` | 准备时不再比对 Acceptance 的行集合（核实时已比对，BC-AT11）；Acceptance 不可修改（TestDatabaseRefusesIllegalAcceptanceChanges），绕过触发器的篡改在激活与读取时由 Verify 按完整行集合重建核对发现，不在准备时拒绝 |
| PYT-0230 | `test_t03_a26_order_qualification.py::test_unverified_publication_source_blocks_new_activation` | POL 配置的发布证据只在发布时核对（S1-D16），ConfigGuard 只判配置在效，EVD 核实撤回不使已发布配置失效；按 ADR-BR-004 这类变化在订单激活时也只是须确认的风险 |
| PYT-0335 | `test_t03_a28_sales_terms.py::test_charge_evidence_verification_is_rechecked_independently` | EVD 来源没有核实状态，Context Map R16 只发布 SourceRef v1 查询而无来源守卫；销售费用证据只在冻结前经 EVD 判定可读，激活在 TRD 事务内不再查询 EVD |
| PYT-0370 | `test_t03_a30_contact_profiles.py::test_contact_profile_source_access_is_rechecked` | 联系人与交易档案的来源证据归 PTY，在发布时核验（S1-D35）；TRD 只经 EVD 重查报价自身所引证据，不重查上游主数据的来源可读性 |
| PYT-0571 | `test_t04_a46_purchase_draft.py::test_task_control_applies_to_new_drafts_and_write_replays` | PUR 的根没有对象控制（战术模型未给 PUR 聚合 ObjectControl），OrderGuard v1 不发布报价根的对象控制；任务控制随 TSK 在 S4-01 接入（负责人决定 2026-11-10） |

**同键重放不重验所存内容（TRD 的重放口径）（3）**

| 编号 | Python 测试 | 说明 |
|---|---|---|
| PYT-0213 | `test_t03_a25_order_snapshot.py::test_public_prepare_receipt_replay_rechecks_persisted_snapshot` | TRD 的同键重放按命令账本与订单行重建回执，只重查可见性、对象控制与所引来源，不重验所存内容，与报价、回复与 Acceptance 的重放相同（PUR 的草稿重放另行重验）；快照由不可变触发器与 CHECK（order_hash = sha256(snapshot)）保护（TestDatabaseRefusesIllegalOrderChanges），绕过触发器并重算哈希的篡改在下一次激活、读取或 ActiveOrderRef 时由 Verify 发现 |
| PYT-0214 | `test_t03_a25_order_snapshot.py::test_public_active_receipt_replay_rechecks_persisted_snapshot` | TRD 的同键重放按命令账本与订单行重建回执，只重查可见性、对象控制与所引来源，不重验所存内容，与报价、回复与 Acceptance 的重放相同（PUR 的草稿重放另行重验）；快照由不可变触发器与 CHECK（order_hash = sha256(snapshot)）保护（TestDatabaseRefusesIllegalOrderChanges），绕过触发器并重算哈希的篡改在下一次激活、读取或 ActiveOrderRef 时由 Verify 发现 |
| PYT-0215 | `test_t03_a25_order_snapshot.py::test_local_active_replay_rechecks_persisted_snapshot` | TRD 的同键重放按命令账本与订单行重建回执，只重查可见性、对象控制与所引来源，不重验所存内容，与报价、回复与 Acceptance 的重放相同（PUR 的草稿重放另行重验）；快照由不可变触发器与 CHECK（order_hash = sha256(snapshot)）保护（TestDatabaseRefusesIllegalOrderChanges），绕过触发器并重算哈希的篡改在下一次激活、读取或 ActiveOrderRef 时由 Verify 发现 |

**实质一半在 Brant 不存在（部分移植）（1）**

| 编号 | Python 测试 | 说明 |
|---|---|---|
| PYT-0165 | `test_t03_a19_configuration_consumption.py::test_revoked_publication_evidence_blocks_formalization_atomically` | 配置失效时激活整体拒绝、不留文档、提交证据与编号已移植（TestGuardedInputsLapsingBeforeActivationAreRefused，以模板停用，CONFIG_NOT_ACTIVE）；发布证据的核实撤销在 Brant 不存在：POL 只在发布时核对证据可读（S1-D16），已发布配置不因证据变化失效 |

### Python 实现专属（NOT_APPLICABLE），按理由分组

| 理由 | 编号 |
|---|---|
| SQLite 迁移与结构升级；Go 由 brant-migrate 按迁移账本在 PostgreSQL 16 上迁移 | PYT-0219、PYT-0220、PYT-0418、PYT-0422、PYT-0423、PYT-0424、PYT-0453、PYT-0465、PYT-0466、PYT-0477、PYT-0478、PYT-0485、PYT-0506、PYT-0575（14） |
| Python 运行时与候选 JSON Schema、映射文件的一致性；Go 契约为类型化 api，请求结构由传输层严格解码封闭 | PYT-0180、PYT-0185、PYT-0382、PYT-0459、PYT-0549（5） |
| 篡改已存回执 JSON 或 JSON 正文；Go 不存回执 JSON，回执由聚合重建，回复与 Acceptance 为类型化列，受 CHECK、外键与不可变触发器约束 | PYT-0188、PYT-0226、PYT-0542、PYT-0544、PYT-0545、PYT-0546、PYT-0547（7） |
| 绕过触发器改写封存数据、文件字节或时间文本；Go 以 CHECK（哈希、大小）与 timestamptz 使该状态无法存储，命令只收 Z | PYT-0206、PYT-0237、PYT-0238、PYT-0261、PYT-0398、PYT-0552（6） |
| Python 调用方自有的 SQLite 事务与连接；Go 的事务由各上下文的应用层拥有（ADR-BR-REPO-04），命令整体提交或回滚 | PYT-0199、PYT-0248、PYT-0288、PYT-0340、PYT-0341、PYT-0362、PYT-0376（7） |
| 对象控制只在 TRD 报价根（TRD-INV-015）；POL 配置、汇率与 PTY 交易档案没有对象控制 | PYT-0163、PYT-0231、PYT-0333、PYT-0334、PYT-0353、PYT-0368、PYT-0384（7） |
| 旧格式数据、伪造的旧版本修订或客户端证明；Go 冻结必经服务端计算并封存当前契约的绑定 | PYT-0217、PYT-0218、PYT-0286、PYT-0289、PYT-0291、PYT-0294、PYT-0297、PYT-0339、PYT-0356、PYT-0360、PYT-0377、PYT-0412（12） |
| Brant 没有 contact_profile 配置类型，联系人由 PTY 交易档案按行版本固定（S1-D35） | PYT-0364、PYT-0366、PYT-0375（3） |
| Python 的本地与公共两入口及旧标识；Go 只有一条命令路径 | PYT-0512、PYT-0555（2） |
| Python 实现专属的语义：布尔与整数相等、引用种类区分税适用、地址角色可改、依赖读取权限 | PYT-0222、PYT-0329、PYT-0381、PYT-0257（4） |

### 已移植但缺次要部分

| 编号 | Python 测试 | 缺少的部分 |
|---|---|---|
| PYT-0135 | `test_foundation.py::test_number_and_plaintext_template_bind_snapshot` | 编号与模板字段由 POL 移植（TestBusinessReferenceKeepsItsNumber、TestPlainTextTemplateNamesFixedFieldsOnly，同一业务引用返回原编号，S1-D14）；按模板渲染的字节由 TRD 的 RenderPlain 逐字节复现（TestPlainTextTemplateRendersFixedFields）；Go 没有 foundation.document.render 命令，渲染结果不登记为文件、不记模板修订 |
| PYT-0209 | `test_t03_a25_order_snapshot.py::test_local_public_replay_and_restart_keep_one_snapshot` | 同键重放返回同一动作与订单哈希（TestOrderPrepareReviewAndActivate）；Go 只有一个入口，订单的进程重开未单独测试（同类见 TestReconciliationObservesRecoversAndStaysAvailable） |
| PYT-0241 | `test_t03_a26_order_qualification.py::test_revocation_event_is_a_durable_revision_hold_even_if_effectivity_is_corrupted` | 撤销正式化批准即登记风险暂停，订单激活为 APPROVAL_REVOKED（TestActivationRechecksTheContractAndTheSourceRevision）；改回批准效力的篡改未测，批准撤销以效力版本 CAS 且只撤销一次，风险暂停只可插入 |
| PYT-0273 | `test_t03_a27_cost_scope.py::test_wrong_contact_revision_is_rejected` | 联系人须为询价的联系人（TestBindScopeRefusals）；行版本不与询价比对，联系人行版本不是 PTY 当前的在冻结时为 REFERENCE_STALE（TestSealDraftRefusesWhatCannotBeFrozen，未引用本测试） |
| PYT-0348 | `test_t03_a29_trade_parties.py::test_reference_and_address_validation` | 缺交易档案、缺地址（DRAFT_NOT_READY）与未知地址（REFERENCE_INVALID）已移植；空 ID 与布尔修订号在 Go 属契约解码（SCHEMA_INVALID），未在此测试 |
| PYT-0383 | `test_t03_a32_sales_fx.py::test_age_boundary_and_stale_formalization_leave_no_business_writes` | 时效边界在计算、正式化与 quote_ready 三处的单元测试；PostgreSQL 上未执行（测试世界没有销售汇率），拒绝不留业务写入未实测；quote_ready 原因保持 FX_STALE（Python 公共查询映射为 REFERENCE_STALE） |
| PYT-0396 | `test_t03_a35_time_qualification.py::test_future_negative_offset_and_fraction_are_not_prematurely_expired` | 负偏移与未到的小数时刻按 UTC 移植，放行不提前判过期；激活后的 quote_ready 未在此测试重复 |
| PYT-0436 | `test_t03_query_contract.py::test_query_contract_rejects_bad_shape_and_keeps_unimplemented_kinds_explicit` | 畸形查询为 SCHEMA_INVALID（TestQueryContracts），多余成员由传输层严格解码拒绝；Go 的读取按路由分开，没有 Python 的查询种类分派与 UNSUPPORTED_CONTRACT |
| PYT-0455 | `test_t03_write_contract.py::test_public_contract_executes_all_thirteen_actions` | 保存、冻结、修订与丢弃已移植（TestQuoteDraftFreezeReviseAndDiscard）；审核、批准、退回、驳回、撤销、激活、替代、取消与撤回各按其 Python 测试移植（PYT-0437、0441、0413、0416、0417），未串成一个测试 |
| PYT-0456 | `test_t03_write_contract.py::test_idempotency_replay_and_changed_payload_are_atomic_and_authorized` | 同键同参重放与同键异参 IDEMPOTENCY_PAYLOAD_CONFLICT 已移植，另加对账；撤权后重放为 PERMISSION_DENIED 由命令账本统一执行，此测试未重复 |
| PYT-0458 | `test_t03_write_contract.py::test_quote_root_own_team_and_company_scope_is_enforced` | 他公司、own 范围外与同团队 team 范围已移植；团队变更后不可见未在此测试重复 |
| PYT-0494 | `test_t04_a39_order_gate.py::test_stale_hash_version_and_mismatched_order_fail_closed` | 审核所绑订单哈希不符为 ORDER_REVIEW_STALE 已移植；Python 绕过触发器改写订单行版本与订单 ID 的情形未移植，Go 由数据库触发器拒绝此类改写 |
| PYT-0509 | `test_t04_a40_public_revoke.py::test_payload_conflict_and_closed_schema` | 封闭结构、空理由与超长理由已移植；同键异参的 IDEMPOTENCY_PAYLOAD_CONFLICT 由通用命令账本执行（X-INV-001），未对撤销单独测试 |
| PYT-0517 | `test_t04_a41_public_review.py::test_rejected_decision_blocks_activation_and_atomic_audit_failure_rolls_back` | 驳回阻断激活已移植；审核请求的存储故障回滚未单独测试（准备、激活与撤销的故障见 TestOrderWritesFailWithoutResidue） |
| PYT-0521 | `test_t04_a42_public_review_query.py::test_query_rejects_extra_fields_and_cross_scope_review` | 他范围读取为 NOT_FOUND 已移植；请求的额外成员由传输层严格解码拒绝，此处未测 |
| PYT-0536 | `test_t04_a45_response_query_recovery.py::test_own_team_and_company_scope_are_current` | own 与公司范围已移植；team 范围未在此测试（通用可见性见 TestTeamScopeAndReplayDoNotLeak） |
| PYT-0548 | `test_t04_a45_response_query_recovery.py::test_read_audit_is_atomic_and_failure_has_no_business_effect` | 读取与被拒读取（NOT_FOUND）均写入审计已移植；审计写入失败时读取一并失败、不留审计的情形未测 |

### 本次补齐的移植

第一轮核对后有 52 行未移植而缺业务理由：31 行已被 Go 测试判定但测试未引用，21 行为测试缺口。本次逐行补齐：已判定的测试补引用（核对后不足的补用例），缺口按所属层级补写测试，新增的 PostgreSQL 16 集成测试在 trd_order_eqv、trd_quote_eqv、trd_formal_eqv 与 pol_quote_config 四个文件；另修正引用了编号却未判定该行为的测试注释（PYT-0162、0164、0165、0221、0295、0534 及 PYT-0195 的命令清单）。

产品修正一处：order.approval.revoke、order.cancel、order.close_unfulfilled、order.close_remainder 与 order.close（给出理由时）的理由须为 1—2000 个字符的有效 UTF-8、不全为空白、不含控制字符，否则 SCHEMA_INVALID（api reasonText，与 PUR 的 api.Reason 相同；PYT-0497）。

{{s3_ported}}

## 04 证据 {#s4}

| 文件 | 内容 |
|---|---|
| README.md | 证据目录说明：运行方式、对应提交与未执行项 |
| 01_environment.json | 主机、Go、PostgreSQL、依赖与 git 提交 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256；构建器以此校验证据对应当前源码 |
| 03_make_ci_go1.26.5.txt | 以 CI 的 Go 1.26.5、CI=true 运行 make ci 的完整日志（含竞态测试） |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果 |
| 05_github_actions_runs.json.txt | S3 分支的 GitHub Actions 运行记录 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码模糊测试 |
| 07_brc001_chain.json | BRC-001 全链经 HTTP 的记录：报价 960.00、订单 960.00、采购草稿 600.00 |

{{s3_evidence}}

### 未完成与缺口

| 缺口 | 归属 |
|---|---|
| STL CreditGuard（R57）与 CNX TradeComplianceGuard（R58）：订单激活与变更应用记为 not_wired | S5-12、S5-13 |
| TSK 对象控制：manual 状态与屏障、释放前提、受信适配器设置任务声明、system 操作者的绑定授权，及绑定随授权变化失效是否放宽；GoalReadiness（R18）的非审计查询 | S4-01、S4-02 |
| 上游事件触发的风险暂停（来源否认、成本变化等，SC-04）；EVD 按 QuoteActivated、QuoteReplaced 登记正式文件（R16 FileRef v1） | S4-06（WF-03） |
| FUL、STK 出运事实与 PUR、STL 承诺：订单取消、短交关闭、关闭与变更记为 not_wired；PUR 对 OrderAmended 的复核 | S5-01、S5-03、S5-04、S5-09 |
| 订单变更不能修改法律条款；新增费用的变更（DEPENDENCY_NOT_SUPPORTED）；变更不重查毛利 | S3-15、S3-17 余项 |
| 已批准但过时的变更永久保持 approved（状态机无 approved → withdrawn）；订单取消时未决变更不关闭；POL 无订单审批政策 | 设计修订 |
| PUR 来源可信（PUR-INV-004）只覆盖核价快照的来源且不阻断（R21），须 CST 契约与负责人决定；超采的独立批准与阈值；PUR 根的对象控制（PYT-0571） | 待定；S5；S4 |
| 采购承诺、供应商确认与直发交付 | S5-01、S5-15 |
| 外部发送渠道：发送目标恒不就绪 | 待定 |
| CISG 与中国法以外的适用法律与成立规则（须法务确认）；zh-CN 报价版式 | 待定 |
| 正式文档不打印 named_place_ref；报价联系人行版本与询价的比对（PYT-0273） | S3-04、S3-02 余项 |
| 毛利例外批准人独立于核价编制人只由岗位禁止组合保证 | S3-14 余项 |
| 测试：销售汇率时效在激活与 quote_ready 时只有领域测试（PostgreSQL 夹具无销售汇率）；SC-02 精确数字只在领域测试与黄金向量中 | S3-04、S3-14 余项 |
| 客户 PO 行的自动比对；以行为承诺（定金，需 STL）；FileRef v1 形式的回复证据 | 待定；S5 |
| quote.response.verify 是否要求四眼 | 待负责人决定 |
| 动作对账路由：EVD、PTY、CAT、POL、REF、SUP 尚无 | 待定 |
| 报价侧全部理由与订单审核、订单变更裁决的驳回理由、order.amendment.withdraw 只查长度与 UTF-8，不拒绝全空白与控制字符（订单撤销、取消与关闭已收紧，PYT-0497） | 待定 |
| 订单占用转换名额后 quote_ready 仍答 ready（只重查激活所查），/active 显示 conversion_taken；读取 ActiveOrderRef v1 没有 HTTP 路由 | S4（TSK GoalReadiness） |
| OpenAPI 候选的读取响应没有模式（空列表省略）；同一引用在各上下文拼写不同（PTY contact_refs[].id 与 TRD contact_ref.contact_id，CST rfq_ref.id 与 SUP、TRD 的 rfq_id），契约冻结前统一 | 契约冻结前 |
| 正式报价文档以最小币值打印金额（与 Python 相同），API 中不出现 960.00 这类主币值 | 待定 |

> [!warn] 尚未执行
> 真实客户样例与真人签核；STL 信用与 CNX 贸易合规守卫（S5-12、S5-13，订单激活记为未检查）；TSK 对象控制与 GoalReadiness（S4-01）；Inbox 消费方（上游风险暂停、EVD 文件登记，S4-06）；FUL、STK、STL 事实（订单结束与变更记为 not_wired）；外部发送渠道；CISG 与中国法以外的成立规则。G0、G1 仍为 NO-GO。

> [!decision] 已决定（负责人，2026-11-10、2026-11-11）
> S3 不接入 STL、CNX 与 TSK，缺失记为未检查（S3-D01）。毛利审批人可作出审核裁决（S3-D46）；销售经办持有 number.issue（S3-D65）。已成立合同的修订不能被替代或取消，争议中的 Acceptance 可直接作废（S3-D78、S3-D79）。active 修订的批准已用尽，quote_ready 不再报告其过期（S3-D101）。超采批准在 S3 不授岗位（S3-D111）；R52 CAT→PUR 为守卫锁（S3-D116）；法律条款由管理员起草与发布（S3-D148）。
