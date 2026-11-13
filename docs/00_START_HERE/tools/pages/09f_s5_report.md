---
output: 09_IMPLEMENTATION_PLAN/12_IDP_Brant_S5_Execution_Report_V0.1.html
title: IDP Brant S5 B0 受限试点扩展执行报告 V0.1
short: S5 执行报告 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / S5 EXECUTION REPORT
doc_title: S5 B0 受限试点扩展执行报告 V0.1
doc_desc: 采购承诺与直发交付 · 质量放行 · 出运、节点与出口批次 · 商业条件放行 · 订舱、单证与保险 · 客户信用与守卫接入 · 资金、核销与结算单 · 贷项、费用与订单经营结果 · 期初余额 · 贸易合规、许可证与中国最小协同 · 订单变更传播 · Inbox 分发 · 容量与恢复演练 · BRC-001 至结算
meta: 版本=V0.1; 日期=2026-11-13; 证据=E2 本机执行＋GitHub Actions; 结论=S5 完成 · G3、G4 NO-GO（模拟评审）
stage: 09 · 工程实施总体计划
eyebrow: S5 EXECUTION REPORT
search: 搜索工作项、单元、决定、缺口或证据
foot: S5 Execution Report V0.1 · 2026-11-13
---
<span class="scope-badge">S5 DONE @@S5-CI-BADGE@@ · G3 NO-GO · G4 NO-GO</span>

<h2>BRC-001 经 HTTP 从 960.00 USD 的生效订单走到出运、单证、结算与核销、报关与退税资料，订单经营结果 300.00 USD（31.25 %）；信用与贸易合规守卫接入订单生效，订单变更传到采购、出运、结算与合规</h2>

<p class="lede">2026-11-12 至 11-13，S5 按负责人决定先于 S4 执行。它新建 QLT（检验与质量放行）、STL（客户信用、资金记录与核销、结算单、贷项与冲销、费用与订单经营结果、期初余额）、CNX（贸易合规、出口许可证、报关、采购发票与退税资料、官方证书、单证包与外部对账）与 FUL（出运与装箱、节点、出口批次、商业条件放行、订舱与出运单证、保险、订单变更复核）四个上下文，扩展 PUR（采购承诺、直发交付、承诺变更与取消、分配复核）、TRD（下游快照、守卫接入、订单结束与变更读取出运与采购事实）、CAT（申报档案）、POL、PTY 与参考数据；Inbox 订阅登记与进程内分发（S4-06a）把订单事件送到各消费方。海关、税务、银行、保险与承运人全部是人工路径，信用、合规、许可证与税务数据为合成数据。容量与恢复演练只在本机 WSL2 上实测。G3、G4 按负责人决定记为模拟评审 NO-GO；没有真人签核。</p>

::: cover
- 结论 | S5 完成（S5-06 除外，它依赖 S4-08 与真实客户证据）：S5-01 至 S5-20 与 S4-06a 由 A0 设计源修订之后的 38 个功能单元交付，D1、D2a、D2b 三个验收单元汇总端到端证据，全部合并入 s5/business。每次合并本机 make check，按批运行 PostgreSQL 16 集成与竞态测试；GitHub Actions 结果见 11 节。
- 证据 | 13_S5_EVIDENCE：环境、源文件 SHA-256 快照、make check、各合并提交的门禁结果、GitHub Actions 运行记录、BRC-001 S5 链两次运行的记录、表格与比对、容量与恢复演练的原始测量，以及 CN-AT 矩阵与伴随场景矩阵。
- 未执行 | 海关单一窗口、税务、银行、保险人与承运人连接器（S6-05，S5 为人工路径）；真实信用额度、国别项目、许可证与 HS 编码（合成）；生产或准生产环境上的容量与恢复演练；真实客户任务与真人签核；S4 的任务与 AI 层；STK 库存路线（S6-01）；CLM 索赔（记为 not_enabled）；界面（CN-AT17 只证明了接口一半）。
:::

## 01 结论摘要 {#s1}

S5 的范围按负责人 2026-11-12 的决定为全深度（E-5）：订单经营结果、费用分配、贷项、结算冲销、跨币种核销、承诺变更与全部 CN-AT 用例都在 S5，只有库存路线留给 STK（S6-01，D-10）。每张卡片注明交付单元与合并提交（s5/business 上的合并提交）。

::: cards
- S4-06a · A1 14a6b57 | Inbox 订阅登记与进程内分发 | 上下文以 Subscriptions() 声明订阅（消费方、来源、事件类型、权限、事务前读取），brant-worker -dispatch 把事件按事件 ID 去重、在消费方自己的事务中投递，每个消费方一个系统操作者 system:inbox.<bc>.<purpose>，由 brant-admin inbox 授权；拒绝立即进入死信并告警，重放按事件 ID 去重。单元测试按白名单核对每个订阅的来源是 Context Map 上游，并要求 PL-09、PL-12、PL-13、PL-14 都有消费方。PG16 上测试了重复投递、两个工作进程、租约过期、乱序重放与失败重投。
- S5-01 · A4 2210943 | 采购承诺（A47） | 承诺申请绑定草稿哈希与订单修订；独立裁决（不是草稿编制人或申请人，AI 被拒）在事务内按当前数据重算草稿并逐字段比对，不等为 PURCHASE_DRAFT_STALE；有效期不晚于供应条件；成本偏差、不可比与超采须附理由确认（D-14，超采在承诺裁决中批准）；发出前可撤销，发出取 POL 采购单号并留发送证据；供应商确认为独立事实；已承诺草稿作废为 PURCHASE_DRAFT_COMMITTED（TD-AT06）。裁决、撤销与发出的并发各只有一个结果。
- S5-02 · A8 e874f59 | 检验、处置与质量放行 | 外部检验机构的报告是证据，放行由我方授权人员决定（F-06，AI 与系统被拒）；复检替代旧检验、不增加可放行量（BT08）；放行量不超过有效检验的合格量减已放行量；撤销冻结未出运部分，重复撤销无变化（BT21）；QualityReleaseGuard v1 供 FUL 在事务内锁定。
- S5-03 · B1a 9b5be90、B1a2 f1c6f48、B1b 1a1ec07 | 出运、节点与出口批次 | shipment.prepare 绑定生效订单修订、直发交付行与质量放行；出运确认（人工）先锁出运根、再锁订单履约根，按代码顺序调用全部守卫，FUL-INV-001 按订单行累计确认量不超过当前修订（零容差），两个出运抢最后的数量只有一个成功，重放不重复出运（BT11）；直发交付只算核实量（TD-AT14）。节点区分供应商声称、内部核实与已接受，报关放行、实际离港、交付与客户验收是不同节点（CN-R07）；出口批次映射分批出运的多行（CN-AT02）。ShipmentFacts v1、ShipmentLineRef v1、ExportLotRef v1 供 TRD、STL、CNX 读取。
- S5-04 · B2a 7e59d81、B2b b760a7c | 资金记录、核销与结算单 | 资金记录（人工、附银行证据）分开汇款额、到账额与银行费用，未解释差额在独立批准人解释前不可用（CN-R10）；结汇是转账记录，不是第二笔收款（CN-AT09）；订单账户承载当前修订的付款阶段，预付款核销到阶段，结算单确认后移到应收；同一出运行或交付行只结算一次，并发只有一个成功（STL-INV-001）；核销不超过来源余额与目标未结额（STL-INV-002）。PaymentConditionGuard v1 按出运或单证范围判定付款条件。CN-AT07、CN-AT08、CN-AT09 与基线 4.6 S-A 的现金一侧在 PG16 上通过。
- S5-05 · C3a 8f85e62、C3b 79484ed | 中国最小协同（人工路径） | 报关案从出口批次、已核实申报档案与外销合同准备，人工提交与登记回执（原文、映射版本与外部序号），旧回执不覆盖新回执，unknown 先核实再重报；采购发票导入为候选、人工核实并匹配承诺行，红字冲回把匹配送回复核；退税案按用途分配发票余额（并发只有一个成功），预计、已批与已收分开；单证包列出缺失项、负责人与期限，存在不等于有效。CNX 读取 FUL 的出口批次，出口日期取报关回执上的出口日期（负责人确认）。D2a 在一个套件中经 HTTP 端到端运行全部 18 个 CN-AT 用例，全部通过（04 节）。
- S5-07 · C1 a422f9e、C1b 6a79b3e、C1c 4d1aa66 | 商业条件放行 | commercial_release.evaluate 按出运或单证范围一次调用全部守卫并记录 granted 或 blocked 与首个未满足的码；例外只由信用经理批准、只覆盖一个未付阶段的付款条件，从不覆盖质量、筛查、合规或许可证（FUL-INV-002、BT13）；出运确认与单证放行在自身事务内重查并消耗放行。SC-08／TD-AT08（2.00 未解释差额被阻断，解释并核销后放行）、TD-AT12、质量撤销与出运确认、核销撤销与单证放行的两种提交顺序在 PG16 上通过。负责人 2026-11-12 的两项决定由 C1b、C1c 实现：已出运货物的单证放行按订单当前修订判定付款条件，出运确认后新出现的阻断仍然阻止单证放行。
- S5-08 · C2 6378737 | 订舱与出运单证套 | 订舱按版本记录；商业发票与装箱单由 shipment-text/0.1.0 按 POL 版式以中英文渲染并由 POL 编号，提单与原产地证为外部单证记录；签发前逐项核对发票、装箱单、提单、出运行、实际装箱与订单行的差异，未解决或未确认不能签发（BT12、FUL-INV-004）；签发件不可变，编号不复用；单证放行绑定已签发的提单。CN-AT17 的双语单证与签署件登记在 PG16 上通过（界面一半未做）。
- S5-09 · C4a f88bee4、C4b-CNX 9d668fc、C4b-STL 6244a7e、C4b-PUR 638d02a、C4b-FUL 43122ce | 订单变更传播 | TRD 在锁订单后无锁读取 FUL 的出运事实与 PUR 的下游承诺（D-1）：已出运的不能取消、不能减到已出运以下，关闭要求逐行出运等于订购，短交关闭要求已有出运（TD-AT01、TD-AT02、ES-09），取消与出运确认的两种提交顺序都正确（TRD-INV-019）；变更准备从下游事实预填影响项。OrderAmended 与订单结束事件触发 PUR 分配复核、FUL 出运复核、STL 付款计划复核与信用迁移、CNX 新修订的待定核查（PL-09、PL-14），每个修订只处理一次（BT10）。
- S5-10 · A5 47832cb、A7b 2076e18、C5 fe0b160 | 申报档案与官方证书 | 申报档案按（SKU、规格修订、编码表版本）建档，AI 可起草候选，核实由独立于最后编辑人的人工完成，已核实修订不可变（CAT-INV-002）；CNX 的评估与报关只用已核实档案。证书登记官方结果（原产地证、许可证副本、检验证书），对象、市场或有效期不符为 CERTIFICATE_NOT_APPLICABLE，FUL 的原产地证引用在准备时校验（CN-AT14 证书一半）。
- S5-11 · C7 ad9e7fd（3a7cb7f） | 容量基准与恢复演练 | 黑盒负载工具在 tests/load 下经 HTTP 与本地 OIDC 签发者驱动 BRC-001 订单循环；两轮阶梯负载 0 错误；读取 P95 在全部阶段 ≤ 368 ms，命令 P95 在 32 用户 363 ms、64 用户 800—834 ms；一个分发进程约 90 事件／秒是第一个瓶颈。逻辑恢复与 PITR 各两轮逐表一致，RPO 7.6—17.1 秒，RTO 约 30 秒；S5 起点的库升级到当前迁移 1.86 秒。四个候选目标冻结（读取与命令附容量条件），见 06 节。
- S5-12 · A6 b331e28、B4 ceb8026、B6 f2bda8e、B6b 9637ec1 | 客户信用与 CreditGuard 接入 | 额度按层级发布（顶层以上两人），占用计算出运后与开票后阶段中未受单证控制的部分，敞口为未结应收加未被应收覆盖的占用，超额或逾期超容差开启信用决定；决定按金额层级由信用专员或信用经理放行（TD-AT11、TD-AT16）。order.activate 依次调用 CNX 与 STL 守卫，无覆盖占用为 CREDIT_RESERVATION_REQUIRED；变更应用在信用需求增长时为新修订要求占用（D-9）；OrderActivated 消耗占用、订单结束释放（PL-12、PL-13），订单账户在生效时打开。SC-09 的两张订单在 PG16 上通过。
- S5-13 · A7 adcc88e、A7b、B4、C4b-CNX | 贸易合规核查与守卫 | 评估读取订单修订、PTY 地址国别、已核实申报档案与受控导入的合成国别项目（CN、UN），给出候选；结论由合规人工给出并绑定法规修订、名单版本与期限；TradeComplianceGuard v1 在订单生效与 FUL 放行中锁定结论。禁运目的国为 TRADE_COMPLIANCE_NOT_CLEAR，订单审批不能覆盖（TD-AT12）；新订单修订没有结论，旧结论不适用；法规新修订与名单新版本重开结论。
- S5-14 · B5 e50492e | 出口许可证与用量台账 | 许可证由人工登记，分配到出运或出口批次时核对目的国、最终用户、货物与期限，累计不超过许可数量与金额（TD-AT13：1,000 中已分配 600 再分配 500 被拒，并发只有一笔成功）；只有外部结果（修改或撤回的报关回执）才释放用量；ExportLicenseGuard v1 供 FUL 放行。
- S5-15 · B3 5662d7e | 直发供应商交付 | 交付按已发出承诺行由人工或 AI 登记为声称，由另一人工核实，核实量不超过承诺量（PUR-INV-008，并发只有一个成功），更正产生新版本；SupplierDeliveryGuard v1 供 FUL 在事务内锁定；未核实的声称从不计入（TD-AT14）。
- S5-16 · C6 799b195 | CIF／CIP 投保 | 保险单作为单证套中的记录：保险金额不低于合同比例（默认且最低 110 %）乘 CIF／CIP 发票金额，按币种 HALF_UP；CIF 或 CIP 缺保险单不能签发单证；保险费与核价快照的保险组件比较、只显示差异（R78）。SIM-02 第 8 步（9,200.00 → 10,120.00）在 PG16 上通过。
- S5-17 · C9 fc6fc58 | 费用、费用分配与订单经营结果 | 费用按金额、数量或重量分配，权重非负且和为正，最大余数法以稳定行 ID 破同（STL-INV-010）；订单经营结果给出预测、执行中、已确认、业务结算汇差与净现金；关闭要求履约完成或短交关闭、应收应付结清、费用确认，快照不可变，补录费用须重新打开并保留旧快照（STL-INV-011、BT18、BT24）。基线 4.6 S-A（预测 2,500，确认 2,350 = 1,410 + 940，汇差 −100）与 BRC-001（300.00，31.25 %）在 PG16 上通过。
- S5-18 · C8 2a69b7d | 贷项、结算冲销与跨币种核销 | 贷项由销售申请、独立的信用经理确认，确认后的贷项作为核销来源，不改出运数量、不自动调整退税（S-B：1,000 的贷项 100 → 净销售 900）；有有效核销的结算单不能冲销（SETTLEMENT_HAS_APPLICATIONS），冲销后来源行可再结算一次；跨币种核销要求 POL 结算用途、方向一致的汇率，汇差单列为业务结算汇差，不是收款或收入（STL-INV-007—009）。
- S5-19 · C11 32dc238 | 采购承诺变更与取消 | 变更产生下一承诺修订：编制、独立裁决（重算、确认偏差）、在绑定仍有效时应用，增量须有分配余量，减量不低于已核实交付；短交即减到核实量后视为 fulfilled；取消释放未交付部分的分配并保留已核实交付（SIM-02 工厂交付 850 / 900 在 PG16 上通过）。
- S5-20 · C10 fd6635b | 期初余额与期初退税资料 | 期初应收、应付与预付款按截止时点与来源单据登记，同一来源只登记一次（OPENING_BALANCE_DUPLICATE），期初应收计入信用敞口（SC-09 的“未结 7,000”）；期初发票余额与外部状态的退税案不重新生成已被外部结果消耗的用途（CN-R09）。CN-AT16、BT17 与 CN-AT11 的 CNX 一半（贷项在外部对账中反映，退税资料不变）在 PG16 上通过。
- 前置 · A2 26f8cce、A3a e896f55、A3b 55f7d85 | TRD 快照、POL 与国别 | TRD 发布订单商业快照（草稿与生效订单的付款阶段、条款、地址与单据）；POL 增加信用与结算汇率用途、单证控制的付款阶段（release_control）、信用政策的期限与两人规则、出运单证模板与出口退税政策两种配置（共 22 种）与装箱单编号；PTY 地址增加 ISO 3166-1 国别；参考数据增加受控导入的国别项目名单。既有修订的内容哈希全部不变。
- D1 · 2c7cd8a | BRC-001 订单至结算全链 | TestBRC001OrderToSettlementOverHTTP 经 301 个 HTTP 请求、19 位人工走完计划 §5 的全部步骤，事件经进程内分发三轮投递并重投一次无变化；两次运行在规范化服务端 ID、哈希与时间后完全一致。见 03 节。
:::

状态说明：DONE_CI 表示验收项在本机与 GitHub Actions 上都通过；DONE_LOCAL 表示只有本机证据（S5-11 的测量与演练不在 CI 上运行）。

{{wbs_tasks phase=S5}}

## 02 新建与扩展的上下文 {#s2}

四个新上下文各有自己的 schema、事务 owner、命令账本、FORCE RLS 与不可变触发器，迁移目录按固定顺序 qlt、stl、cnx、ful 追加在 pur 之后（D-2：FUL 的事务调用其他上下文的守卫，故排在最后）。S5 共新增 30 个迁移文件，数据库从 S5 起点的 35 个迁移到 65 个。

| 上下文 | schema 与迁移 | 命令（人工专属以“人”标注） | 发布的守卫与查询 | 订阅的事件 |
|---|---|---|---|---|
| QLT 质量 | brant_qlt；qlt 000001 | inspection.record；disposition.decide（人）、quality_release.grant（人）、quality_release.revoke（人） | QualityReleaseGuard v1（R26）；QualityReleaseRef v1 | — |
| STL 结算 | brant_stl；stl 000001—000007 | credit_limit.request、credit_limit.publish（人）、credit.reserve、credit.release、credit.consume（系统）、credit_exception.decide（人）；fund.record（人）、fund.resolve_difference（人）、order_account.open、application.apply、application.unapply（人）；settlement.confirm（人）、settlement.reverse（人）；payment_plan_review.open、payment_plan_review.settle（人）；credit_note.request、credit.confirm（人）；charge.record、charge.confirm（人）、charge.allocate；order_result.review、order_result.close（人）、order_result.reopen（人）；opening_balance.record（人） | CreditGuard v1（R57）、PaymentConditionGuard v1（R45）；FundRecordRef v1（R38）、CreditNoteRef v1 | stl.order_credit：order_activated、order_amended、order_cancelled、order_closed_unfulfilled、order_remainder_closed、order_closed；stl.order_account：order_activated、order_amended |
| CNX 中国协同 | brant_cnx；cnx 000001—000006 | trade_compliance.evaluate、trade_compliance.decide（人）、trade_compliance.reopen；export_license.register（人）、export_license.allocate、export_license.release（人）；purchase_invoice.import、purchase_invoice.verify（人）；tax_refund_case.allocate、tax_refund_case.record_result（人）、tax_refund_case.record_opening（人）；declaration_case.prepare、declaration_case.confirm（人）、declaration_case.record_submission（人）、declaration_case.record_receipt（人）；document_bundle.check；external_reconciliation.resolve（人）；certificate.register（人） | TradeComplianceGuard v1（R58、R56）、ExportLicenseGuard v1（R56）；CertificateResultRef v1 | cnx.trade_compliance：order_amended、config_published（screening_regulation）、country_programmes_imported |
| FUL 履约 | brant_ful；ful 000001—000007 | shipment.prepare、packing.plan、packing.record_actual、shipment.cancel、shipment.confirm（人）；milestone.plan、milestone.record_actual；export_lot.register；commercial_release.evaluate、commercial_release.grant_exception（人）、document_release.record（人）；booking.record、shipment_document.prepare、shipment_document.issue（人）、shipment_document.record_signed（人）；insurance.record；shipment_review.open（系统）、shipment_review.settle（人） | ShipmentFacts v1（R70，无锁读取）；ShipmentLineRef v1（R30）、ExportLotRef v1（R36） | ful.shipment_review：order_amended、order_cancelled、order_remainder_closed |

| 扩展的上下文 | S5 增加的内容 |
|---|---|
| PUR（pur 000002—000005） | 采购承诺（申请、独立裁决、撤销、发出、供应商确认）；直发交付（登记、独立核实、更正）；承诺变更（编制、裁决、应用）与取消；分配复核（消费方 pur.allocation_review，处置须有理由）。SupplierDeliveryGuard v1（R59）、DownstreamCommitments v1（R71，无锁读取）、InspectionSubject v1（R24）、PurchaseSettlementSource v1（R31）、PurchaseLineRef v1（R37） |
| TRD（trd 000008） | 订单商业快照（含提议中的变更修订）供 STL、CNX、FUL；order.activate 接入 CNX 与 STL 守卫，变更应用按 D-9 为新修订要求信用；订单状态无锁读取 order_state_v1；订单取消、短交关闭、关闭与变更读取 FUL、PUR 事实，STK 记为 not_enabled；变更准备预填影响项 |
| CAT（cat 000004） | 申报档案（草稿、独立核实、替代）与 DeclarationProfileRef v1（R39） |
| POL（pol 000005） | 汇率用途 credit 与 settlement；付款阶段 release_control；信用政策的单证担保、占用与决定期限、顶层以上两人；配置类型 shipment_document_template 与 export_tax_policy（共 22 种）；编号单据类型 packing_list |
| PTY（pty 000006）与参考数据（ref 000002） | 地址的 ISO 3166-1 alpha-2 国别（只随新确认修订，旧地址为未知）；国别项目名单（禁运、武器禁运、制裁、出口管制）由合规岗位受控导入，CountryProgrammeListGuard |
| CST | ResolveSnapshotComponents：核价快照的保险组件供 FUL 比较（R78） |
| messaging、bootstrap、cmd | 订阅登记、Dispatcher、brant-worker -dispatch、brant-admin inbox、brant-api -replay-dispatch |
| tests/load、scripts | 黑盒负载工具（不导入 internal/），scripts/backup-restore.sh（逻辑、PITR、升级演练），make load、make drill |

## 03 BRC-001 从订单到结算、核销与中国资料（D1） {#s3}

TestBRC001OrderToSettlementOverHTTP（构建标签 integration，PostgreSQL 16.12，HTTP 经测试内本地 OIDC 签发者的访问令牌）先运行 S3-12 的链（ES-01 第 1—19 步；自 B4 起其中已含计划 §5 的第 1—6 步），再走 §5 的第 7—27 步。每个命令是闭合信封的 POST，每个读取是 GET，由该步指定的人工以自己的令牌发出：301 个请求（218 POST、83 GET；S5 部分 148 个），19 位人工，约 7.5 秒。事件经 Platform.Relay 发布到 Contexts.Subscriptions() 的 Dispatcher（即 brant-worker -dispatch 的装配），三轮投递共 144 个事件，每轮之后由第二个分发器全部重投，Outbox 计数与 Inbox 记录不变。

| §5 步骤 | 人工 | 结果 |
|---|---|---|
| 1—3（在 S3 链内） | admin、config.reviewer、sales、两位信用经理 | 合成信用政策（E-2）生效；额度 10,000.00 USD 由两位信用经理发布（顶层 5,000 以上两人）；订单修订 1 占用 672.00（P2 70 % 出运后 30 天，非单证控制） |
| 4—6（在 S3 链内） | doc.1 → doc.2、compliance、order.reviewer、sales | 四个申报档案核实（6109100021，不受控）；CN、UN 国别项目导入，合规结论 clear、无需许可证；订单审核后生效：提交证据 stl.credit_guard.v1 covered（672.00）、cnx.trade_compliance_guard.v1 clear，未检查守卫为空；分发后占用 consumed、订单账户打开（P1、P2 均未付） |
| 7 | treasury | 资金记录 288.00（银行证据）核销到 P1：出运与单证范围的付款条件满足 |
| 8 | purchaser → order.reviewer → purchaser | 承诺申请、独立裁决（重算等于 600.00）、发出 PO-2026-00001、供应商确认；作废草稿为 409 PURCHASE_DRAFT_COMMITTED |
| 9 | buyer、qc | 外部检验机构（已核实 inspection 服务角色）的报告；四行检验 24、36、12、48 全部合格，四个放行共 120 PCS |
| 10—12 | merchandiser、purchaser | 节点计划与内部核实的装箱完成节点；直发交付 120 PCS 在 CNNGB 交接并由采购核实；出运准备（四行、交付行与放行）、装箱计划与实际装箱（10 箱）、订舱（CNNGB → DEHAM） |
| 13—14 | doc.1、merchandiser | 出运范围放行 granted（QLT 120、PUR 120、CNX clear、PTY clear、STL 已付 288.00、TRD 生效）；出运确认 120 ≤ 120，未检查守卫为空 |
| 15—17 | doc.1、merchandiser | 商业发票 CI-2026-00001（960.00 USD，“Net: 960.00 ／ Tax: 0.00 ／ Total: 960.00”）、装箱单 PL-2026-00001、提单记录；核对 0 差异后签发；单证范围放行 granted（P2 出运后、非单证控制，由信用覆盖），原提单放行；出口批次 120 PCS、960.00 USD，与出运一致 |
| 18 | sales、doc.1 | 外销合同生成；报关案准备、确认、人工提交与登记回执（released，原文与映射版本 v1 保留，出口日期取自回执）；FUL 的离港节点仍未登记（CN-R07） |
| 19—21 | treasury、credit.manager.1 | 销售结算单 960.00：P1 的 288.00 移到应收，未结 672.00，信用敞口 672.00（不重复计算）；收款 670.00（汇款 672.00）先核销 672.00 为 409 FUND_DIFFERENCE_UNEXPLAINED，信用经理把 2.00 解释为我方银行费用后核销 670.00 与 2.00，应收未结 0；采购结算单 600.00 付款后应付未结 0 |
| 22—24 | purchaser、tax.officer、doc.1 | 合成增值税专用发票导入并核实、匹配 120 PCS；退税案分配（用途 export_refund），绑定出口日期时有效的出口退税政策修订，Brant 不计算退税额，只记录预计阶段；单证包完整；外部对账申报金额 960.00 等于发票金额，差额 0 |
| 25 | merchandiser | 订单关闭：出运事实 24／36／12／48 等于订购，承诺 fulfilled；分发后占用 released，敞口 0 |
| 26—27 | credit.officer、treasury | 运费 60.00 USD 预计 → 凭承运人账单确认 → 分配到订单；订单经营结果复核并关闭：净销售 960.00、销售成本 600.00、直接费用 60.00，结果 300.00 USD、毛利率 31.25 %，与报价冻结时的利润一致，索赔记为 not_enabled |

最终断言（测试在结束时重新读取）：订单 960.00（已关闭）；占用 672.00 由 reserved（S3 链）到 consumed（第 6 步）到 released（第 25 步）；出运 120；商业发票 960.00；出口批次 960.00；应收 960.00 → 0（288.00 ＋ 670.00 ＋ 2.00 已批准的银行费用）；应付 600.00 → 0；任何提交证据与事件都没有未检查的守卫，没有 not_wired；每个事件对每个订阅的消费方恰有一条 PROCESSED 的 Inbox 记录；经营结果 300.00（31.25 %）；§5 列出的结果事件各按链路引起的次数发布（如 Applied 4、QualityReleased 4、SettlementConfirmed 2、CommercialReleaseGranted 2）。

两次独立运行（2026-11-13，Go 1.26.8，WSL2 上的 PostgreSQL 16.12 容器）都通过，各 301 步、3 轮分发、3,203 个 JSON 值；原始差异 65 个（43 个服务端生成的 ID、16 个基于它们的 SHA-256、4 个时刻、2 个租户 ID），规范化后 0 差异（证据 06—09）。

D1 按代码而非字面执行了计划 §5 的 15 处细节，每处保留步骤意图：第 1—6 步按 B4 的守卫顺序在 S3 链内完成；额度由两位信用经理发布；目的国与买方地址国别由合规凭证据录入（客户地址没有国别）；订单由销售生效（order.activate 在销售经办模板）；S5 步骤所需的 POL 配置在 S3 链之后先行发布（出口退税政策须在出口日期之前发布）；四行分别检验与放行；“货物备妥”记在 packing_complete 节点；订舱港口不与货代的服务航线比对；报关前加 declaration_case.confirm 并先生成外销合同；增加一次未解释差额的拒绝；采购发票由采购登记扫描件、匹配在核实中完成、合成发票为 USD；用途为 export_refund，预计阶段须给出金额（记录发票进项税 78.00 作为上限，不是退税计算）；单证包的要求引用 EVD 来源；资金、信用与税务岗位的证据由单证员与跟单登记（这些模板没有 source.register）；doc.1 另持 number.issue 以编号商业发票与装箱单。这些都列入 09 节的缺口。

## 04 CN-AT 矩阵（CN-AT01—CN-AT18） {#s4}

负责人决定 E-5 把全部 18 个 CN-AT 用例纳入 S5，E-1 规定外部系统只走人工路径、数据为合成。每个用例先由负责的单元在自己的测试中证明（下表“单元测试”列，除注明外均为 PostgreSQL 16 集成测试），再由 D2a 在一个套件中经 HTTP 端到端运行（“D2a”列）。“中文操作”指界面，不在本 API 仓库；S5 只证明 zh-CN 数据、单证与错误文本。

| CN-AT | 用例 | 单元 | 单元测试 | D2a |
|---|---|---|---|---|
| 01 | 单一主体正常出口链 | D1；D2a 两次出运变体 | TestBRC001OrderToSettlementOverHTTP（一次出运） | TestCNATSuite01SingleEntityExportInTwoShipments 通过：两次出运各 480.00，两个批次报关放行（回执原文、映射 v1），发票 120 PCS 分配 60 ＋ 60 到两个退税案（预计、无金额），单证包完整，资金标为商业核销（CN-R11），订单关闭、占用释放 |
| 02 | 分批出运、多行出口 | B1b、C3a | TestExportLotsMapSplitShipmentsCNAT02、TestCNAT02DeclaresMultiLineLotsFromFUL；多个销售行进入一个批次只在领域测试中证明（共享订单只有一行） | TestCNATSuite02SplitShipmentsAndMultiLineExports 通过：L2 分 20 ＋ 16 出运，再多 1 件为 SHIPMENT_QUANTITY_EXCEEDED；两个批次 352.00、608.00，第二批两个报关项；漏行、重复批次与第二个未结报关均被拒；各销售行报关量 = 出运量 = 订购量 |
| 03 | 发票品名与单位对报关 | C3a | TestCNAT03MappingWithoutAProfessionalBasisIsNotConfirmed（DECLARATION_MAPPING_UNVERIFIED） | TestCNATSuite03InvoiceUnitDiffersFromDeclaration 通过（偏差：L4 以 4 DOZEN 贯穿各上下文）：单位未换算的报关项不能确认（DECLARATION_MAPPING_UNVERIFIED），受控换算后 48 PCS = 48 件，旧版本确认为 REFERENCE_STALE |
| 04 | 一张采购发票跨批次使用 | C3b | TestCNAT04PurposeBalancesAcrossBatches、TestCNAT04ConcurrentAllocationsOneWins（TAX_EVIDENCE_EXCEEDED，并发只有一个成功） | TestCNATSuite04OneInvoiceAcrossBatches 通过：两笔并发分配一胜一 TAX_EVIDENCE_EXCEEDED，胜者同键重试为同一动作，败者改用余量与另一张发票；免税用途余额分开。并发经 HTTP 同时发送，未在数据库中强制重叠（C3b 的测试强制） |
| 05 | 先出运后开票、红字冲回 | C3b | TestCNAT05RedReversalPutsMatchesBackToReview、TestRedReversalAndAllocationCommitOrders；应付不随红冲调整（B2b 决定 7） | TestCNATSuite05ShipFirstInvoiceLaterRedReversal 通过：先出运、报关与付款后到票；红冲后 4 个匹配与 4 个分配转复核，规划为 INVALID_STATE；交付、出运、批次与已付应付不变；更正发票替换分配。应付侧的暂估调整没有命令 |
| 06 | 修改与撤回报关、乱序回执 | C3a | TestCNAT06AmendedAndWithdrawnDeclarationsWithOutOfOrderReceipts（DECLARATION_RECEIPT_STALE） | TestCNATSuite06AmendedWithdrawnDeclarationOutOfOrderReceipts 通过（偏差：退税案在修改后保留已绑定的出口日期）：迟到与重复回执为 DECLARATION_RECEIPT_STALE 且无变化；修改产生版本 2（120 → 110），撤回后再收到放行为 INVALID_STATE；用途不因撤回恢复 |
| 07 | 合并付款含银行费用 | B2a | TestCombinedPaymentForTwoOrders | TestCNATSuite07CombinedPaymentWithBankCharges 通过（偏差：第二张订单为期初应收）：汇款 1,000.00 = 到账 980.00 ＋ 已解释银行费用 15.00 ＋ 待定 5.00；资金岗位自行解释为 PERMISSION_DENIED；敞口 1,000.00 → 5.00 |
| 08 | 付款人不同、退汇与退款 | B2a | TestRefundsReturnsAndPayers；第三方付款人保持 PARTY_ENTITY_MISMATCH 待核 | TestCNATSuite08DifferentPayerReturnedRemittanceAndRefund 通过：第三方付款核销为 PARTY_ENTITY_MISMATCH 待定，退汇从中扣减；超出余额的退款为 APPLICATION_EXCEEDS_AVAILABLE，退款另成资金记录；第三方付款依据的核实没有命令 |
| 09 | 先结汇后人民币收款 | B2a | TestBaseline46CashSideAndConversion（结汇记录不是第二笔收款） | TestCNATSuite09FXConversionThenCNYReceipt 通过：670.00 USD 按 7.1 结汇 CNY 4,757.00 为一条结汇记录；人民币一侧作为客户收款为 BUSINESS_KEY_CONFLICT，二次结汇超额，结汇记录不能核销；应收与净销售不变 |
| 10 | 预计、已批与已收退税 | C3b | TestCNAT10ExpectedApprovedAndReceivedStayApart | TestCNATSuite10ExpectedApprovedAndReceivedRefundDiffer 通过（偏差：税务局为 PTY 供应商主体，文书由 admin 登记）：预计 507.00、核准 480.00 ＋ 抵 20.00、到账 480.00 分开记录，金额与币种不符的到账被拒；不按冻结政策解释（CN-D02、CN-D07 未冻结） |
| 11 | 出口后折让或退货 | C8（STL 一半）、C10（CNX 一半） | TestBaseline47SBCreditNote、TestCNAT11CreditNoteAfterExportIsReflectedInCNX；退货入库属 STK（S6-01） | TestCNATSuite11AllowanceAfterExport 通过：贷项 50.00 确认前在 CNX 反映为 INVALID_STATE，确认并核销后（未结 672.00 → 622.00、净销售 910.00）在单证包对账中反映，差额 −50；报关、退税案、批次与出运不变。退运属 STK |
| 12 | 政策变化与未结历史业务 | A3a、C3b | TestExportTaxPolicyChangeKeepsTheRevisionOfOldExports、TestCNAT12CaseBindsThePolicyInForceAtTheExportDate | TestCNATSuite12PolicyChangeWithOpenHistoricalBusiness 通过：先出口的批次保留政策修订 r1，新修订发布后出口的批次绑定 r2（export_refund 被拒、免税可用），旧案在变化后仍按 r1 增加分配 |
| 13 | 提交超时、结果后到 | C3a | TestCNAT13SubmissionTimeoutIsVerifiedBeforeAnythingIsSubmittedAgain（EXTERNAL_RESULT_UNKNOWN） | TestCNATSuite13SubmissionTimeoutThenVerifiedResult 通过：结果 unknown 时重报为 EXTERNAL_RESULT_UNKNOWN，原提交同键重试为同一动作；无出口日期时分配为 INVALID_STATE；登记回执后才可分配 |
| 14 | 缺失单证、证书不适用 | C3a、C5 | TestCNAT14BundleListsWhatIsMissingAndPresenceIsNotValidity、TestCNAT14CertificateResultsApplyOnlyToTheirObjectMarketAndValidity | TestCNATSuite14MissingDocumentAndCertificateNotApplicable 通过：原产地证列为缺件（负责岗位、期限），上传件存在不等于已核实（DOCUMENT_REQUIREMENT_UNVERIFIED），他国或未覆盖全部行的证书为 CERTIFICATE_NOT_APPLICABLE，适用证书使单证包完整。FUL 原产地证记录的 certificate_ref 由 C5 的测试覆盖 |
| 15 | 出口方、发票买方与收款方主体不一致 | C3b、B2a | TestCNAT15BuyerEntityMismatchStaysInReview；收款账户须为本公司账户（B2a，领域测试）；“经获批方式复核”没有命令 | TestCNATSuite15EntityMismatchExporterBuyerPayee 通过（偏差：拒收后由供应商重开）：买方主体不符为 PARTY_ENTITY_MISMATCH 并保持导入状态，另一公司的读取为 404，他人账户的收款被拒，报关不接受出口方成员；获批方式复核没有命令，出口方非本公司（CN-M03）不在 CN-M01 |
| 16 | 期初余额与退税资料 | C10 | TestCNAT16OpeningBalancesReconcileAtTheCutoff、TestCNAT16OpeningRefundMaterialReconciles | TestCNATSuite16OpeningBalancesAndRefundMaterial 通过：截止时点的期初应收 7,000.00、应付 1,800.00、预收 2,000.00 与期初发票余额、已核准退税案对账，重复导入为 OPENING_BALANCE_DUPLICATE，敞口加 7,000.00，只有预收是现金 |
| 17 | 中文操作、双语单证、盖章修订 | C2；TRD 的 S/C 与 PI 签署件（S3） | TestBilingualDocumentsCorrespondAndSignedCopiesCNAT17（接口一半；界面未做） | TestCNATSuite17BilingualDocumentsAndSignedCopies 通过：中英文商业发票与装箱单出自同一内容并逐项对应，草稿不能登记签署件，AI 被拒，盖章扫描件登记一次，外销合同会签（TRD）；拒绝均为目录码，中文文本取自错误码目录。界面不在本仓库 |
| 18 | AI 读取发票与报关差异 | C3b | TestCNAT18CandidatesAreVerifiedByAHuman（AI 只导入候选，核实限人工，差异显示） | TestCNATSuite18AIReadsInvoicesAndDeclarations 通过：AI 导入误读的候选（12 PCS／60.00），其核实、确认与回执登记为 PERMISSION_DENIED；税务专员按 120／600.00 核实，6 处差异各注明来源 |

D2a（单元提交 d21d52d，合并 7aedd52）在 internal/bootstrap 中新增 cnat_integration_test.go、cnat_harness_integration_test.go 与 cnat_dozen_integration_test.go：TestCNATSuite01—18 每个用例使用新租户，从 D1 的 BRC-001 链分叉，经 HTTP 由用例指定的人工或 AI 代理以本地签发者的令牌发出全部命令与读取，事件经进程内分发投递并重投一次无变化；18 个用例共 4,770 个请求（3,671 POST、1,099 GET），套件约 105 s。证据运行于 2026-11-13（UTC），在最终树 df25a38 上 18 个全部通过（Go 1.26.8，WSL2，PostgreSQL 16.12 容器 brant-pg16-d2a）；单元门禁 make check 0，make integration 107 包 ok（03:25—03:40 UTC）。各用例的完整记录与矩阵在证据 11—13。全部用例都在 D1 的链上运行，D1 的 15 处细节偏差同样适用；另加的夹具：sales 另持 credit_note.request（销售经办模板的权限），AI 代理 agent.reader 持显式权限，另一公司的读者持 cnx.read。任何用例都没有执行：海关单一窗口、税务机关、银行、承运人、货代与检验机构的系统、真实身份提供方与界面；全部数字、主体与外部结果为合成，证明的是组合进程上的规则，不是合规处理（基线 §24.3）。没有发现产品缺陷。

## 05 伴随场景 {#s5}

计划 §5 在主链之外列出伴随场景，并把基线 4.6 S-A、S-B、跨币种核销、结算冲销、BT18／BT24、承诺变更与期初余额列为全深度的伴随测试。各场景先由负责的单元证明，D2b 在一个套件中把它们按场景重新串起来。

| 场景 | 内容 | 单元与测试 | D2b |
|---|---|---|---|
| SC-08 ／ TD-AT08 | 核心设计条款 30 %／70 % 出运前：670.00 到账、2.00 未解释 → COMMERCIAL_RELEASE_BLOCKED，解释并核销后放行 | C1 TestSC08ReleaseWaitsForTheUnexplainedDifference | TestS5SC08ReleaseWaitsForTheUnexplainedDifferenceOverHTTP 通过（偏差：订单从一开始按 30 %／70 % 出运前成立，不用变更）：670.00 核销后放行 blocked（P2 短 2.00），出运确认 409；信用经理解释 2.00 后仍 blocked，核销 2.00 后同一放行 granted；单证放行原提单；结算单 960.00 未结 0 |
| SC-09 | 两张订单：额度 10,000、未结 7,000、占用 4,000 → 信用决定 → 信用经理放行 → 生效；禁运目的国 → TRADE_COMPLIANCE_NOT_CLEAR，保持草稿 | B4 TestOrderActivationAsksTradeComplianceThenCredit；C10 TestSC09AnOpeningReceivableCountsInTheCreditExposure（未结 7,000 来自期初应收） | TestS5SCSC09CreditDecisionAndEmbargoedOrderOverHTTP 通过：期初应收 7,000.00 使敞口 7,000.00；订单 A 出运后部分 4,000.00 → 信用决定 CREDIT_LIMIT_EXCEEDED（超额 1,000.00），生效 409，信用经理放行后占用、生效；订单 B 收货人在 AQ（合成禁运地区）→ 合规 blocked，TRADE_COMPLIANCE_NOT_CLEAR，保持草稿 |
| SIM-02 变体 | CIF 汉堡 1,000 × 9.20、O/A 60 天、变更减 10 % 后 PUR 复核与草稿作废、保险 110 %、工厂交付 850 / 900、出运 850、短交关闭 50 → STL 释放 | C6 TestInsuranceOnTheCIFInvoiceSIM02Step8；C11 TestCommitmentShortReceiptSIM02；C4b-PUR、C4b-FUL 的复核测试 | TestS5SCSIM02VariantOverHTTP 通过：CIF 9,200.00，信用决定后生效；减 10 % 后 PUR 复核作废草稿并另起 900，STL 复核信用部分 9,200.00 → 8,280.00，CNX 新修订 clear；交付 450 ＋ 400，短交变更后承诺 fulfilled；无保险单 409、8,601.99 不足额 422，8,602.00（7,820.00 × 110 %）足额；短交关闭余 50，占用释放 |
| TD-AT13 | 许可证 1,000、已分配 600、再分配 500 → EXPORT_LICENSE_EXCEEDED；过期 → EXPORT_LICENSE_EXPIRED；并发只有一笔成功 | B5 TestTDAT13ExportLicenseRegisterAllocateAndGuard、TestTDAT13ConcurrentAllocationsOneWins | TestS5SCTDAT13ExportLicenceOverHTTP 通过：放行在分配许可证前为 EXPORT_LICENSE_REQUIRED；许可证 A（1,000）分配 600 后放行；500 再分配到 A 为 EXPORT_LICENSE_EXCEEDED，过期的 B 为 EXPORT_LICENSE_EXPIRED，另一许可证 C 使订单整单出运 |
| TD-AT14 ／ ES-12 | 承诺 120、核实 100、未核实声称 20：出运 120 → SUPPLIER_DELIVERY_INSUFFICIENT | B1a2 TestShipmentOnVerifiedDeliveriesTDAT14；B3 的 PUR 一半 | TestS5SCTDAT14ShipmentOnlyOnVerifiedDeliveryOverHTTP 通过：声称 120、核实 100（L4 28）、待核 20、拒绝 5；出运 120 为 SUPPLIER_DELIVERY_INSUFFICIENT，待核与已拒为 SUPPLIER_DELIVERY_UNVERIFIED；出运 100 |
| BT10 | 部分出运后减量或改色：承诺、装箱与付款计划的影响可追溯，已出运事实保留 | C4b-PUR TestAllocationReviewFollowsAReducedOrderBT10；C4b-FUL TestShipmentReviewFollowsAReducedOrderBT10；C1b TestDocumentsReleaseOfAConfirmedShipmentAfterAReductionBT10 | TestS5SCBT10OrderChangeAfterPartialShipmentOverHTTP 通过：已出运 60 后减到低于已出运为 QUANTITY_BELOW_SHIPPED；L4 48 → 36 的修订 2 由五个消费方各处理一次；PUR、FUL、STL 复核分别以承诺变更、替换出运、接受处置，CNX 修订 2 重新 clear；已出运事实保留 |
| BT12 | 装箱单箱数与发票数量不符：逐项显示差异，未解决不能签发 | C2 TestShipmentDocumentsReconcileBeforeIssueBT12 | TestS5SCBT12PackingListMismatchBlocksTheIssueOverHTTP 通过：工厂装箱单与出运行、实际装箱、发票有 6 处差异，签发 409 并逐项给出路径；按实际装箱的装箱单版本 2 对齐后签发，同一编号 PL-2026-00001，版本 1 被替代 |
| BT21 | 放行 200、出运 100、撤销 → 剩余 100 冻结，二次撤销无变化 | A8 TestRevocationFreezesTheUnshippedRestBT21 | TestS5SCBT21RevocationAfterPartialShipmentOverHTTP 通过：L4 放行 48、已出运 24 后撤销，二次撤销重放无新事件；第二个出运确认为 QUALITY_RELEASE_REQUIRED；冻结的 24 不能再放行（QUALITY_RELEASE_EXCEEDS_CONFORMING） |
| 基线 4.6 S-A | 预付 200、结算 600 ＋ 400、以 6.875 结汇 800 = CNY 5,500、运费 650 → 390 ＋ 260、确认结果 CNY 2,350 = 1,410 ＋ 940、汇差 −100 | B2a TestBaseline46CashSideAndConversion；B2b TestBaseline46SAStatements；C8 TestCrossCurrencyApplicationAtTheSettlementRateS_A；C9 TestBaseline46SAOrderResult | TestS5SCSABaseline46OverHTTP 通过（偏差：人民币到账按结算汇率直接核销，不经结汇记录）：CNY 1,400.00 → USD 200.00，CNY 5,500.00 → 400.00 ＋ 400.00（各 −50.00），运费 650 → 390.00 ＋ 260.00，确认结果 CNY 2,350.00 = 1,410.00 ＋ 940.00，预测 2,500.00，汇差 −100.00，净现金 2,900 |
| 基线 4.7 S-B | 1,000 的贷项 100 → 净销售 900，应收减少，数量不变 | C8 TestBaseline47SBCreditNote；C9 TestSBCreditNoteInTheConfirmedResult（领域） | TestS5SCSBCreditNoteOverHTTP 通过：销售经办申请退货贷项 100.00、自行确认为 PERMISSION_DENIED，信用经理确认并核销；数量仍为 100 PCS，净销售 900，经营结果 900 − 400 − 50 = 450.00（退回 10 PCS 入库的 40.00 属 STK，故非基线的 490.00） |
| 跨币种核销 | 人民币收款按结算汇率核销美元应收，汇差单列；缺结算汇率 → FX_SCOPE_MISMATCH | C8 TestCrossCurrencyApplicationAtTheSettlementRateS_A、TestCrossCurrencyPrepaymentMovesInProportion | TestS5SCCrossCurrencyWriteOffAtTheSettlementRateOverHTTP 通过：CNY 4,620.00 核销 P2 672.00，无汇率或信用用途汇率为 FX_SCOPE_MISMATCH，结算汇率 0.145454545455 得 USD 672.00，应收未结 0 |
| 结算冲销 | 有有效核销时冲销被拒，撤销核销后冲销，出运行只再结算一次 | C8 TestReversalRestoresTheSourceOnce、TestReversalAndApplicationOfOneStatementOneWins | TestS5SCSettlementReversalAfterUnapplyOverHTTP 通过：有移入的预付核销时冲销为 SETTLEMENT_HAS_APPLICATIONS，撤销后由信用经理冲销，再结算一次，第三张结算单为 SETTLEMENT_SOURCE_DUPLICATE |
| BT18 ／ BT24 | 费用待定时关闭被拒并列出维度；关闭后补录下月运费 → 重新打开、新快照、旧快照保留 | C9 TestBaseline46SAOrderResult（BT18 部分）、TestBT24LateFreightReopensTheResult | TestS5SCBT18BT24OrderResultNotSettledAndReopenedOverHTTP 通过：费用待定时关闭 409 列出履约、应收、采购成本与直接费用；全部结清后快照 1 为 300.00（31.25 %）；下月运费 20.00 须重新打开，快照 2 为 280.00，快照 1 保留并标为重新打开 |
| 承诺变更与取消 | SIM-02 短交 850；取消释放分配、保留已核实交付 | C11 TestCommitmentShortReceiptSIM02、TestCommitmentCancelReleasesTheUndeliveredAllocationAndKeepsTheVerified | TestS5SCSIM02VariantOverHTTP（短交 850，承诺 fulfilled，余 50 短交关闭）与 TestS5SCCommitmentCancellationOverHTTP 通过：取消释放未交付的 L3、L4 分配，保留已核实的 60，草稿保持已承诺，替代草稿另向 supplier.B 采购 |

D2b（单元提交 a78d064，合并 f76861c）新增 s5_scenarios_integration_test.go 与 s5sc_deal_integration_test.go：14 个测试 TestS5SC…OverHTTP，每个使用一个租户，九个在 BRC-001 上运行，五个（SC-09、TD-AT13、S-A、S-B、SIM-02）需要 BRC-001 给不出的数字，在按场景构造的成交（deal builder）上从询价走到订单；16 个场景行全部执行，共 3,054 个请求，证据运行 73 s（2026-11-13，最终提交 a78d064，Go 1.26.8，WSL2，PostgreSQL 16.12 容器 brant-pg16-d2b）；单元门禁 make check 0，make integration 107 包 ok。期初应收 7,000.00 的种子在 SC-09 与 SIM-02 中运行。省略的部分各在记录中写明理由：SIM-02 第 1—3、5、10、11 步（不属计划的变体）；BT10 的改色（C4b 的单元测试）；TD-AT14 两个出运确认的竞争（B1a、B1b 的 PG16 测试）；S-B 的退货入库（STK，S6-01）。没有发现产品缺陷。各场景的完整记录与矩阵在证据 14—16。

## 06 容量基准与恢复演练（S5-11，G4 的准备） {#s6}

单元 C7 于 2026-11-12（约 13:10—15:20 UTC）在一台 WSL2 开发主机上测量（内核 6.18.58.1-microsoft-standard-WSL2，20 个逻辑处理器，64 GiB 内存，Go 1.26.8；PostgreSQL 16.12 为 CI 所用 digest 的 Docker 容器，未调优：shared_buffers 160 MB、max_connections 100、synchronous_commit on、WAL 归档 archive_timeout 60 s；API 连接池 20）。负载工具、brant-api、brant-worker 与数据库在同一主机，主机上同时有其他单元的测试套件；两轮阶梯负载都在 load1 低于 2.0 时开始，两轮每阶段相差 1—8 %。这是本机的工程基线，不是生产证据，也不是商业承诺（基线 11.1）。

负载：一个租户、一个客户；命令用户循环执行 BRC-001 的订单链（17 个命令、4 个读取，六种人工身份，含合规与信用步骤），读取用户在已完成的循环上均匀读取 14 个 GET 路由；闭环、无思考时间；第 n 阶为 n 个命令用户加 n 个读取用户；每阶 10 s 预热后测量 50 s。两轮 14 个窗口共 88,866 个命令、346,669 个读取、5,196 个订单循环，0 错误、0 超时、0 死锁、0 回滚。

| 阶段（命令＋读取） | 命令 ok／s | 命令 P95 ms（轮 1／轮 2） | 读取 P95 ms | 订单循环／s | Outbox 积压峰值／中继延迟 P95 s |
|---|---|---|---|---|---|
| 1＋1 | 30.5／29.5 | 68.4／67.9 | 24.2／24.2 | 1.78 | 30／0.99 |
| 2＋2 | 56.3／52.1 | 74.4／79.2 | 27.0／28.7 | 3.32 | 59／1.00 |
| 4＋4 | 99.9／95.4 | 83.5／87.3 | 30.4／32.1 | 5.90 | 112／1.02 |
| 8＋8 | 160.3／160.4 | 102.3／103.6 | 37.1／37.6 | 9.44 | 3,397／20.3 |
| 16＋16 | 192.3／188.0 | 172.1／176.0 | 70.0／72.2 | 11.24 | 8,294／45.5 |
| 32＋32 | 182.8／190.4 | 363.3／347.3 | 165.3／161.0 | 10.54 | 13,350／82.4 |
| 64＋64 | 167.2／172.4 | 833.8／799.7 | 368.2／348.4 | 9.38 | 17,531／117.0 |

读数：吞吐在 16＋16 达峰（约 11.2 循环／s），之后下降而延迟随用户数加倍；峰值时连接池 20 个连接全部占用，最多 18 个处于事务中空闲。数据库等待以 WAL 刷盘为主（WSL2 虚拟磁盘），64 用户时出现共享行的锁等待（最多 19 个等待者，最长 4.8 s），最慢的命令是同一客户信用根上的 credit.reserve（P95 1.31—1.37 s）。第一个容量限制是中继而不是 API：一个分发进程负载下约 85—94 事件／秒，循环每个产生约 16 个事件，从 8＋8 起积压无界增长；第三轮实验用两个分发进程，积压保持有界（4／8／16 用户峰值 99／359／937），延迟 P95 0.9／7.5／9.7 s。

| 演练 | 第 1 轮（155 MB） | 第 2 轮（916 MB） |
|---|---|---|
| 逻辑备份恢复（pg_dump -Fc、pg_restore -j 4）：逐表行数与摘要、28 个 SHA-256 约束重算、迁移账本、文件字节、服务检查 | 全部一致；RTO 28.8 s | 全部一致；RTO 51.9 s |
| PITR（基础备份 ＋ WAL 归档）：写入 90 s 后 SIGKILL 主库，恢复到归档末尾 | RPO 17.13 s（8,709 个已确认写入中丢失最后 17.2 s 的 1,514 个，为前缀）；RTO 29.9 s | RPO 7.64 s（丢失 832 个，窗口 7.7 s，前缀）；RTO 31.8 s |
| 恢复到命名时点 | 与源库在该点完全一致 | 完全一致 |
| 主库崩溃恢复（SIGKILL 后重启） | 2.28 s，全部已确认命令都在 | 1.24 s，全部在 |

升级演练（第 1 轮）：以 git archive 导出 S5 起点 3b76201，用它自己的 brant-migrate 迁移空库（35 个）并用它自己的集成套件写入数据（324 个测试，76,306 行），备份后恢复到新库，再以当前 brant-migrate 应用 22 个迁移（1.86 s），第二次运行应用 0 个；108 张旧表的行数保持，104 张逐字节相同、3 张按设计改变（PTY 地址国别列、PUR 分配条目）；28 个 SHA-256 约束成立；升级后的库上 BRC-001 链与负载冒烟测试通过。

| 候选目标（WBS S5-11） | 结论 | 依据 |
|---|---|---|
| 普通读取 P95 500 ms | 冻结为 500 ms（参考部署的工程目标） | 两轮全部阶段都满足，饱和后也满足：8＋8 以内 24—37 ms，峰值 16＋16 为 70—72 ms，64＋64 为 348—368 ms；个别路由（核价快照读取 64 用户 P95 0.68—0.75 s）需单独关注 |
| 业务命令 P95 800 ms | 冻结为 800 ms，附容量条件：每个 API 实例（连接池 20）不超过 32 个并发命令用户 | 8 用户以内 68—104 ms，峰值 172—176 ms，32 用户 347—363 ms；64 用户 800—834 ms 已过饱和点 |
| RPO 15 分钟 | 冻结为 15 分钟，附配置：持续 WAL 归档、archive_timeout ≤ 300 s、归档存放在主库失效后仍可用的存储、基础备份经校验 | 实测 7.6 s 与 17.1 s；只靠 pg_dump 的 RPO 等于备份间隔 |
| RTO 4 小时 | 冻结为 4 小时，限于以 PITR 恢复数据库（基础备份不超过 24 小时）；完整恢复手册（构建、配置、凭据、任务检查点、外部结果复核）未演练 | PITR 约 30 s，逻辑恢复 28.8—51.9 s；向生产规模的外推不是证据 |

另需一个目标而候选中没有：Outbox 中继延迟。C7 建议参考负载下 P95 ≤ 5 s、至少 2—3 个分发进程，待负责人决定（09 节）。工具发现：PostgreSQL 16.12（Alpine）的 pg_basebackup 以 tar 写到标准输出并生成清单时以 139 退出，演练改用普通格式备份并以 pg_verifybackup 校验。

未执行：生产或准生产主机；真实客户任务、数据与负载模型（租户组合、思考时间）；真实身份提供方；前端端到端延迟；多租户、多客户（本次是单租户最坏情形）；AI 任务、报表、大文件与外部服务；负载期间的进程重启与消息重投注入；异地或对象存储的 WAL 归档与保留；复制与故障转移；构建、配置与凭据的恢复；数小时的浸泡测试；调优实验（连接池、分发批量与轮询、数据库内存）；成本。归档卷与基础备份与“丢失的”主库在同一磁盘，归档命令为不带 fsync 的 cp；WSL2 时钟与租约的注意事项照旧。原始测量与演练日志在证据 10_capacity_and_recovery/。

## 07 门禁：G3、G4 模拟核对 {#s7}

负责人决定 E-7：G3、G4 与 G1、G2 一样以模拟评审（E0）处理，并因缺少真实客户、专业角色与生产证据记为 NO-GO。本节是 E2 单元按该决定所做的逐条对照，把门禁的通过标准与现有证据比较；它不是评审会议，没有任何真人或专业角色签核。

| 门禁 | 通过标准 | 现有证据 | 缺少 | 结论 |
|---|---|---|---|---|
| G3 行业与中国配置验收（产品＋行业／税务等专业角色＋实施） | CRM 关闭下核心交易成立 | BRC-001 S5 链在没有 CRM 的情况下从询价走到订单经营结果（03 节） | 真实客户业务 | 合成数据上满足 |
| G3 | 选定行业及中国业务 CN-R／CN-AT 落实 | CN-AT01—18 在 PG16 上通过（04 节，人工路径、合成数据）；CN-R04—R11 由 CNX 与 STL 的规则执行（用途余额、回执原文与映射版本、红冲复核、退税阶段分开、只有外部结果释放用途、未解释差额、核销不冒充出口收汇核销） | CN-AT17 的界面一半；行业配置（服装）未经行业角色确认；退税用途、申报要素与 HS 编码未经专业确认 | 部分 |
| G3 | 专业接口或人工协同可对账 | 人工路径：回执原文、映射版本、外部序号、unknown 先核实；外部对账记录差额、责任与调整（IG-13） | 海关、税务、银行、保险、承运人连接器（S6-05）；税务、报关、合规、财务专业角色的签核 | 部分 |
| G3 | 审批 | — | 产品、专业角色与实施的真人签核 | 未满足 |
| G4 受限试点上线（客户代表＋交付＋质量＋运维） | 真实客户任务、质量／人工／成本比较 | — | S5-06 依赖 S4-08 与真实客户证据（BC-D01），G0 仍为 NO-GO | 未满足 |
| G4 | 严重缺陷关闭 | S5 期间发现的缺陷均已修复（11 节）；未决的是设计缺口与负责人问题（09 节） | 外部评审 | 合成数据上满足 |
| G4 | 迁移、权限、容量与恢复演练通过 | 升级演练、逻辑恢复与 PITR、阶梯负载在本机 WSL2 通过；岗位模板与职责分离由构建器校验；每个合并提交在 GitHub Actions 上从空库迁移（06 节、11 节） | 生产环境与异地备份；真实身份提供方（IG-07 PARTIAL）；运维真人签核 | 部分 |
| G4 | 审批 | — | 客户代表、交付、质量与运维的真人签核 | 未满足 |

结论：G3 NO-GO，G4 NO-GO（模拟评审，2026-11-13）。G0 与 G1 的结论不变。

## 08 负责人决定与阶段决定 {#s8}

负责人 2026-11-12 采纳了计划 §3 的全部 D- 与 E- 建议，只有 E-5 选择了全深度（选项 c）。S5-D01—S5-D22 对应 D-1—D-22，S5-D23—S5-D31 对应 E-1—E-9；其后是 2026-11-12、11-13 的其他决定。计划提出的 ADR-BR-018 记为阶段决定（S5-D33），不作为已接受的 ADR：ADR 登记册要求每项 ADR 有模拟评审记录，S5 没有为它做模拟评审，本报告也不编造评审结论。

| 编号 | 决定 | 日期与来源 | 落实 |
|---|---|---|---|
| S5-D01 | TRD 在锁订单后于自身事务内无锁读取 FUL 的出运事实与 PUR 的下游承诺（R70、R71）；每个 FUL、PUR 写入方都经 OrderGuard 共享订单行锁，故订单锁已串行化 | D-1，负责人 2026-11-12 | C4a；B1a、A4 发布读取函数；两种提交顺序测试 |
| S5-D02 | 守卫是各 api 包中的 SQL 常量；双向关系的事务前查询以构造后绑定的闭包接线，不在构造参数中形成环 | D-2 | bootstrap |
| S5-D03 | 每个事务只修改一个聚合：锁根（资金记录、订单账户、履约、许可证、信用）FOR UPDATE，余额从不可变条目推导 | D-3 | 全部新上下文；登记的例外见 09 节 |
| S5-D04 | STL 订单账户按订单承载当前修订的付款阶段；核销的目标为订单阶段（预付）或应收，结算单确认时阶段核销移到应收；敞口 = 未结应收 ＋ 未被应收覆盖的占用 | D-4 | A6、B2a、B2b |
| S5-D05 | 单证控制的付款（F-04）：PaymentStage.release_control = documents，信用政策的 documents_secured；此类阶段无需占用、由单证范围的付款条件要求 | D-5 | A2、A3a、A6、B2a |
| S5-D06 | 信用审批层级按操作者的岗位模板对应信用政策层级；信用专员获得 credit_exception.decide；顶层以上须两位不同的顶层审批人 | D-6 | A3a、A6 |
| S5-D07 | 销售经办可在额度内占用信用（credit.reserve），超额开启只由信用岗位放行的决定 | D-7 | A6、角色模板 |
| S5-D08 | 国别：PTY 地址的 ISO alpha-2 国别；CNX 评估取收货人地址国为目的国（合规可凭证据另录）与交易方国别；缺国别不能得出结论 | D-8 | A3b、A7 |
| S5-D09 | 变更应用在出运后部分增长时为下一修订要求信用占用；应用时不查合规，新修订没有结论，CNX 开启待定核查，FUL 放行阻断直至 clear | D-9 | B4、C4b-CNX、C1 |
| S5-D10 | FUL 只支持供应商直发路线；库存路线为 DEPENDENCY_NOT_SUPPORTED，TRD 记 STK 为 not_enabled | D-10 | B1a、C4a |
| S5-D11 | commercial_release.evaluate 按（出运、范围）记录决定与例外；出运确认与单证放行要求该范围已放行并在自身事务内重查全部守卫；例外只用于付款条件 | D-11 | C1 |
| S5-D12 | 商业发票与装箱单由 shipment-text/0.1.0 按新的 POL 出运单证模板渲染并编号；提单、原产地证与保险单为外部单证记录；签发前逐项核对 | D-12 | A3a、C2 |
| S5-D13 | 保险只作单证套中的记录；保险费与核价快照的保险组件经新关系 R78 比较 | D-13（选项 a） | C6 |
| S5-D14 | 采购承诺：申请、独立裁决（重算相等、有效期、偏差含超采）、发出前撤销、带 POL 编号与证据发出、供应商确认分开；超采在承诺裁决中批准，取代 S3 无人持有的超采权限（关闭 S3-D111） | D-14 | A4 |
| S5-D15 | QLT：检验对象为采购承诺行；标准以 EVD 引用；只由我方质检放行；FUL 记录消耗；撤销冻结剩余 | D-15 | A8 |
| S5-D16 | 资金记录限人工并附银行证据；未解释差额在独立批准人（信用经理）解释前待定；结汇是转账记录 | D-16 | B2a |
| S5-D17 | 计划 §2 列出的人工专属命令全部登记（人工专属清单 111 条） | D-17 | A0、各单元 |
| S5-D18 | 消费方：STL（生效消耗、结束释放、变更迁移订单账户）、CNX（变更开启待定核查、法规发布重开）、PUR 与 FUL（变更开启复核） | D-18 | B6、B6b、C4b 四个单元 |
| S5-D19 | 订单关闭容差在 S5 为零（没有 POL 容差配置） | D-19 | C4a |
| S5-D20 | S3 测试在接入守卫后由共享夹具在生效前记录 clear 结论（付款含出运后部分时另加占用）；S3 的 BRC-001 链保持出运后 30 天并占用 672.00；SC-08 以核心设计的 30 %／70 % 出运前作伴随测试 | D-20 | B4、D2b |
| S5-D21 | PUR、FUL、STL 的订单变更复核：每个（订单、修订）一条复核记录，含条目、状态与负责岗位 | D-21 | C4b 单元（键加入原因，见 09 节） |
| S5-D22 | 申报档案按（SKU、规格修订、编码表版本）识别，分类字段分开；由单证岗位核实，独立于最后编辑人 | D-22 | A5 |
| S5-D23 | 外部专业系统（海关单一窗口、税务、银行、保险人、承运人）在 S5 只走人工路径，连接器在 S6-05 | E-1 | C3a、C3b、B2a、C2、C6 |
| S5-D24 | 合成信用政策：USD，信用专员 1,000、信用经理 5,000，顶层以上两人，逾期容差 30 天；真实数值在 B0 前由财务提供 | E-2 | A3a、A6、D1 |
| S5-D25 | 贸易合规数据：受控导入的合成 CN 与 UN 国别项目，结论由人工给出；外国名单问题仍归法务 | E-3 | A3b、A7 |
| S5-D26 | 出口管制与许可证数据：合成许可证与分类，只用于 TD-AT13 | E-4 | B5 |
| S5-D27 | P1 项目全深度：订单经营结果、费用分配、贷项、结算冲销、跨币种核销、承诺变更与全部 CN-AT 进入 S5（新增 WBS S5-17—S5-20）；库存路线留给 STK（S6-01） | E-5（选项 c） | 各单元 |
| S5-D28 | S5-11 在本机 WSL2 的 PostgreSQL 16 容器上演练，tests/load 黑盒工具；G4 保持 NO-GO | E-6 | C7 |
| S5-D29 | G3、G4 以模拟评审（E0）处理，因缺真实证据记为 NO-GO | E-7 | 07 节 |
| S5-D30 | S5 基于 s5/business = 3b76201（保留 S4 A0 的设计修订）；S4 之后重编其迁移与关系编号 | E-8 | 全部单元 |
| S5-D31 | 档案槽位：S5 为 V1.8（09f、12、13），S4 为 V1.9（09e、10、11） | E-9 | 本报告 |
| S5-D32 | 阶段顺序：业务（S5）先于任务与 AI（S4）；S4-06a Inbox 分发作为单元 A1 并入 S5；S5-06 等待 S4-08 | 负责人 2026-11-12 | A1；WBS |
| S5-D33 | （计划中的 ADR-BR-018）双向关系中的反向读取，是读取方在锁定自身根之后于自身事务内的无锁读取，写入方的守卫调用共享该锁。记为阶段决定，未进入 ADR 登记册（无模拟评审记录）；评审时一并复核 AGENTS 的审查规则 | 负责人 2026-11-12（同 D-1） | C4a；AGENTS.md |
| S5-D34 | 岗位：采购获得 number.issue（发出时由 POL 取号）；销售经办可释放自己未被消耗的信用占用（credit.release）；quality_release.grant 保持无独立性规则（我方质检决定，检验员可放行）；quality_inspector 不加 qlt.audit | 负责人 2026-11-12 | 4afa604；roles_gen.go |
| S5-D35 | 出口批次的出口日期取海关在报关单上记载的出口日期，由 declaration_case.record_receipt 记录（决定退税率与适用政策）；FUL 的实际离港节点仍是物流事实（CN-R07）。FUL 不记录最终用户时，出运许可证的最终用户取收货人 | 主导决定 2026-11-12 19:15，负责人约 19:40 确认 | C3a |
| S5-D36 | 已确认出运的单证放行按订单当前修订判定付款条件；贸易合规、许可证与货物事实保持出运确认时的判定；已关闭（含短交关闭）的订单仍可放行单证，已取消的不可以 | 负责人 2026-11-12 约 21:50（选项询问） | C1b |
| S5-D37 | 出运确认后新出现的阻断（交易方筛查变为 blocked，或订单修订的贸易合规重开后判为 blocked）仍阻止单证放行；付款仍按当前修订，其他货物事实保持确认时的判定 | 负责人 2026-11-12 约 23:05（选项询问） | C1c |
| S5-D38 | 单元代理并行运行（14:59）；15:19 负责人决定其余单元代理改用 Opus 5.5 并行，主导者据此调整为全部单元代理使用 Opus 5.5（主导者的工作会占用同一模型的额度）并告知负责人；被中断的单元从分支、wip 提交与 NOTES.md 继续 | 负责人 2026-11-12 14:59、15:19 | 过程 |
| S5-D39 | 两个主导会话同时活动时，负责人选定 WSL 中的 Claude Code 会话为唯一主导，Cursor 主导停止 | 负责人 2026-11-12 约 19:55 | 过程；LEADS.md |

主导者（lead）在执行中作出、并告知负责人的决定：A6 的两项缺口（人工释放与生效的竞争、变更提议修订的信用）交给 B4 在 TRD 与 STL 两侧解决，B4 增加 trd 000008 的订单状态无锁读取；C4b 按上下文拆为 C4b-STL、C4b-PUR、C4b-CNX、C4b-FUL 四个单元并行；CNX 到 FUL 的适配并入 C3a；迁移编号冲突时后合并的文件改号并重生成固定迁移清单；D2 拆为 D2a（CN-AT 套件）与 D2b（伴随场景）；QLT 以交付行为检验对象的做法未建（承诺行对象覆盖了全部验收用例，B3 报告说明了不重复计数的做法）。

### 单元实现中的设计取舍

下表摘录各单元在设计文本未规定处所作、影响行为的选择；完整列表在各单元报告中（主导者的会话资料，不在本档案）。

| 单元 | 取舍 |
|---|---|
| A1 | 每个消费方一个系统操作者；拒绝立即入死信、重放被拒；-sink 与 -dispatch 互斥；事务前 Prepare 读取上游；订阅在基础层校验（不订阅自身事件、来源须为上游）；消费方消失时不收回授权 |
| A2 | release_control 以 omitempty 加入封存事实，既有哈希不变；Incoterm 不封存，从订单绑定的 POL 条款修订读取；快照不含本公司主体 |
| A3a | release_control 只允许用于 after_shipment；新信用政策必须给出占用与决定期限；退税方式三种（snake_case），出口退税政策须引用专业复核证据；信用与结算汇率不进入基础快照 |
| A3b | 国别只接受正式分配的 ISO 代码（UK、XK、ZZ 被拒）；国别按地址、随确认修订；国别项目与筛查名单分开存放，外国辖区可导入但不启用 |
| A4 | 草稿的已承诺状态由承诺表推导（发出只改一个聚合）；只在承诺已发出时禁止作废草稿；过期的已批准承诺仍需撤销后再申请；PO 编号在记录发出时取号 |
| A5 | 档案以 profile_id 为句柄，D-22 键唯一；独立性只对最后编辑人；核实人须能读取全部引用来源；只建模两种非优惠原产地标准 |
| A6 | 额度只用信用政策币种；“不是客户的销售经办”近似为“不是申请人且不持销售经办模板”；只占用当前修订（提议修订由 B4 补上）；无出运后到期部分的订单不能占用 |
| A7、A7b | 国别项目名单由 CNX 评估输入（screening_regulation 尚无该字段），外国名单被拒；禁运与制裁覆盖全部货物，武器禁运与出口管制只覆盖受控或未分类货物；结论期限受法规有效期封顶，blocked 不失效；评估必须给出编码表版本 |
| A8 | 守卫带时刻参数；撤销的放行全额计入（冻结余量在 S5 不能再放行）；让步不使货物可放行；外部机构须是已确认、带 UN/LOCODE 服务角色的主体 |
| B1a、B1a2 | 交付引用带版本，消耗按（交付、行）跨版本累计；确认不要求实际装箱；R44 筛查客户与订单绑定的全部地址角色主体；PUR 守卫的结论原样传递 |
| B1b | 出运准备时保存绑定修订的行价（TRD 只给当前修订价格）；部分数量按金额 × q／Q 一次 HALF_UP，净额与税额分开；“标记批次待复核”在读取时推导；一个出运行至多进入一个批次 |
| B2a | 新增 order_account.open（设计中没有创建订单账户的命令）；已解释差额只经核销计入付款条件；结汇是 fund.record 的一种，只减少来源现金；第三方付款人保持待核 |
| B2b | 应收的到期计划按付款阶段依次填充；“出运”锚点取最早的出运确认，“开票”锚点取销售结算单，只按日历日；应付以核实交付为准；阶段核销移到应收时改写核销聚合（D-4 隐含的例外） |
| B3 | 一张交付对应一张承诺；“承诺量加获批容差”即承诺裁决中批准的超采；首次核实须大于零，更正可全部为零（撤回）；交付命令不调用 TRD 守卫 |
| B4 | 守卫按调用顺序取第一个拒绝（CNX、PTY、STL），全部守卫照常运行；提议修订只取待批与已批的变更，按当前 POL 事实推导；同一（订单、修订）只有一个有效占用；订单在占用修订或之后生效时拒绝人工释放 |
| B5 | 分配不带 expected_version（根锁与推导用量即条件）；货物不符答 EXPORT_LICENSE_EXPIRED（目录无专码）；一个分配覆盖出运的全部行；释放不发布事件；许可证在公司范围 |
| B6、B6b | 订阅 OrderClosedUnfulfilled（否则草稿的占用要等到过期）；乱序生效以“由本消费方释放”识别；人工已释放的占用拒绝消耗并入死信；订单账户另设消费方（D-3、故障隔离），只打开生效修订 |
| C1、C1b、C1c | evaluate 记录结果而不拒绝，自转换照记；例外只覆盖一个未付阶段、属于一个放行，有效期上限 90 天；单证放行在确认后按当前修订判定付款，并按 S5-D37 重查合规（只有 blocked 阻止）与筛查（非 clear 即阻止） |
| C2 | 单证状态机按 A0 登记（无 checked）：差异在新版本中消失或在签发时附理由确认；同一（单证套、种类）跨版本一个编号；装箱单按实际装箱打印，除非人工另报件数 |
| C3a | 最终用户取收货人，否则取客户；目的国取收货人地址国，否则取合规评估；状态机扩展（乱序前跳、amended → amended 等）；订单已有新修订时报关准备为 REFERENCE_STALE |
| C3b | 每个出口批次一个退税案，由首次分配开启；用途封闭为 export_refund 与 export_exemption（须专业确认）；匹配在核实中完成；红字发票核实同时写入红票与被冲蓝票；部分红冲按全额冲回 |
| C4a | “未处置”指没有处置的下游行，fulfilled 承诺与不超过已出运量的分配除外；关闭不读下游承诺；变更准备只预填“待评估”条目，物料与作业不预填 |
| C4b-CNX | 一个消费方三个订阅；每个 OrderAmended 都为新修订开启核查；法规新修订只重开同一法规的较低修订；参考数据的来源对全部上下文放行（Context Map 没有参考数据关系） |
| C4b-STL | 两个消费方各在自己的事务；核销按阶段 ID 跨修订沿用；只把不再适配的阶段列为条目；被替代修订的占用在 OrderAmended 时释放；信用需求变小时保留较大占用直至订单结束 |
| C4b-PUR、C4b-FUL | 复核键为（订单、修订、原因）；变更目标取自 TRD 的生效订单引用；一次处置整个复核、每个条目须有理由；处置词汇为单元自定 |
| C5 | 不适用的原产地证引用在准备时拒绝；证书只登记、无撤回，登记人即核实人；覆盖按（出运、SKU、规格修订）比较，不比数量 |
| C6 | 缺保险单按核对差异处理（SHIPMENT_DOCUMENTS_UNRECONCILED），不足额为 INSURANCE_COVERAGE_INSUFFICIENT；保险比例由单证员给出（默认且最低 110 %）；保险费与核价组件整单比较、同币种、不分摊 |
| C7 | 负载工具为 tests/load 下的 package main，不导入 internal/；分位数取最近秩、只计成功应答；PITR 基础备份为普通格式；升级演练以起点提交自己的集成套件写入数据 |
| C8 | credit.confirm 不核销贷项，确认后的贷项是核销来源；冲销要求先撤销核销，不回移阶段；跨币种预付按比例移到应收；汇差只在分析币种为资金或订单币种时记录 |
| C9 | 结果币种为 TRD 的分析币种；预测只计起初为预计的费用；预计采购成本取 PUR 已发出或确认的承诺行；净现金只计已核销金额；已取消的订单从不结算经营结果 |
| C10 | 来源键严于 STL-INV-012（同一来源不论截止时点只登记一次）；期初结算单不绑定 Brant 订单（不进经营结果、不能开贷项）；期初资金记录不能冲销；期初退税案的政策取出口日期时有效的修订 |
| C11 | 承诺修订为行记录；修订单价取自行上指定的已核实供应条件修订；裁决与应用要求供应条件仍有效；变更绑定订单当前修订；取消不调用上游守卫，已取消承诺的草稿保持已承诺 |
| D1 | 只抽取、不复制 S3 链的步骤；事件经测试内的独立 Outbox 复制后分发；15 处按代码执行的细节见 03 节 |

## 09 设计缺口 {#s9}

单元按规则不修改 docs/、岗位模板与 Context Map，发现的缺口在报告中提出，按最接近设计文本的读法实现。下表按类别汇总，并注明发现的单元。本档案版本（V1.8）只回写 S5 的执行结果，不改上述设计源；这些缺口留待负责人裁决或下一次设计修订。

### 目录补录（命令、事件、读取与错误码）

| 上下文 | 缺少或不符的目录条目 | 发现单元 |
|---|---|---|
| PUR | 读取 purchase.commitment.get／list、purchase.commitment.change.get／list、allocation_review.get／list；事件目录 CSV 缺 purchase_commitment_revoked.v1；偏差种类、确认结果、确认的 revision 参数、sequence_ref 参数；DownstreamCommitments v1 的列契约与 fulfilled 状态；承诺修订文档契约 brant.purchase-commitment-revision/0.1.0；分配复核的消费方、条目种类、影响与处置 | A4、C11、C4b-PUR |
| STL | 事件 credit_limit_requested、credit_exception_decided、credit_reservation_consumed、fund_difference_resolved、applied／unapplied（含 moved 状态）、settlement_reversed、charge_allocated、order_result_reopened、opening_balance_recorded；命令 credit.consume 与 reserve、release 的系统操作者、order_account.open、credit_note.request、settlement.reverse、charge.record、charge.confirm、order_result.review、order_result.reopen；读取 settlement_statement.get、receivable.get、payable.get、order_settlement.get、credit_note.get、charge.get、order_result.get、opening_balance.get／list、payment_plan_review.list；结汇没有命令；消费方失败码 | A6、B2a、B2b、B6b、C4b-STL、C8、C9、C10 |
| FUL | 读取 commercial_release.get／list、shipment_review.get／list；放行事件的载荷与 shipment_confirmed 的 release_id；bindings.confirmation、bindings.rechecked、证据名 ful.shipment_confirmation 与 cnx.trade_compliance_guard.v1/current；放行事件的 payment_order_revision、payment_order_hash；复核的消费方、失败码、条目种类与处置 | C1、C1b、C1c、C4b-FUL |
| CNX | TradeComplianceDecided、TradeComplianceReopened 的载荷；事件类型串 tax_refund_result_recorded 与 tax_opening_recorded；读取 tax_opening.get／list；外部对账主体 credit_note；证书事件载荷含业务值（超出“只含 ID 与版本”） | A7、C3b、C5、C10 |
| TRD | 读取 order.snapshot.get 及其路由 | A2 |
| 错误码 | 没有专码而用 INVALID_KNOWN_VALUE 或 INVALID_STATE 的情形：检验量超过对象数量（A8 建议 INSPECTION_QUANTITY_EXCEEDED）、装箱与出运行不符（B1a）、批次超过出运行（B1b）、评估不完整（A7）、超量匹配承诺行（C3b）、占用 ID 已被占用（A6）；许可证货物不符借用 EXPORT_LICENSE_EXPIRED（B5）；APPLICATION_CURRENCY_MISMATCH 未入目录，币种不符答 FX_SCOPE_MISMATCH（B2a） | A6、A7、A8、B1a、B1b、B2a、B5、C3b |
| 错误码的适用上下文 | BUSINESS_KEY_CONFLICT 缺 QLT、FUL、STL、CNX；DEPENDENCY_NOT_SUPPORTED 缺 QLT、FUL；QUALITY_RELEASE_REQUIRED 缺 QLT；ORDER_APPROVAL_REQUIRED／REVOKED 缺 FUL；DOCUMENT_INTEGRITY_FAILURE 只列 TRD；PARTY_ENTITY_MISMATCH 用于文字未列的供应商与红票主体差异；INSURANCE_COVERAGE_INSUFFICIENT 的说明与缺保险单的处理不一致（见负责人问题） | A8、B1a、B1b、B2a、B5、C2、C3b、C6 |

### Context Map 缺行或契约不符

| 缺口 | 发现单元 |
|---|---|
| 没有关系行写明 CreditNoteRef v1：R38 STL→CNX 只写 FundRecordRef v1（白名单已有 cnx stl）；Context Map 没有对账事件 | C8、C10 |
| STL 读取 PUR 承诺（预计采购成本）没有关系行；R31 的事实名为“采购承诺”而契约为 PurchaseSettlementSource v1；另一做法是 TRD 发布封存的成本 | C9 |
| CN-AT05 红冲后标记应付需要 CNX→STL 关系（消费 purchase_invoice_verified.v1），Context Map 没有 | B2b |
| 参考数据没有 Context Map 关系；消费国别项目导入事件时，订阅检查对全部上下文放行 idp-brant.ref | C4b-CNX |
| ActiveOrderRef v1 只给当前修订的价格，没有价格基数与折扣，FUL 自存绑定修订的行价；没有按修订读取 TRD 条款的接口，FUL 单证打印当前条款 | B1b、C2 |
| R43 写 TradingProfileRef v1，STL 以 PartyRef v1 核对客户 | A6 |
| R50 为守卫锁，但 ConfigGuard v1 只判定“当前”；CN-AT12 需要“某时点有效的修订”的查询或守卫版本 | A3a、C3b |
| R56 为守卫锁，CertificateResultRef v1 实为无守卫查询；R39 无守卫，CNX 可能停在旧的（不可变、已核实）档案修订 | A5、C5 |
| FUL 读取 PTY 服务方角色（货代、承运人、保险人）不在 R44（R44 只写 PartyRef）；报关代理与里程碑负责人没有 PTY 关系 | B1b、C2、C3a |
| MD-38 写 STL、CNX 为放行事实的消费方，实际没有消费方；MD-50／MD-51 的期初事实没有关系与契约 | C1、C10 |
| PUR、FUL 的复核没有事实，开启与处置不发布事件 | C4b-PUR、C4b-FUL |
| QLT 冻结余量的再放行需要 FUL→QLT 的出运量读取，或撤销时记录冻结量 | A8 |
| POL screening_regulation 没有国别项目名单字段（CNX 由评估输入名单），外国项目名单因此不能启用 | A3b、A7 |
| TradeComplianceGuard v1 在 pending 时不返回上一结论，重新评估中的 blocked 结论在窗口内被视为 pending；需要守卫 v2 或 FUL 记住见过的阻断 | C1c |
| TRD 没有给出已核销资金的无锁读取（变更预填的付款计划影响取自 CreditGuard） | C4a |

### 岗位模板缺口

| 缺口 | 发现单元 |
|---|---|
| documentation 模板没有 number.issue，商业发票与装箱单须由单证员取号（测试另授权限）；或改由 POL 代上下文取号 | C2、C6、D1 |
| treasury、credit_manager、credit_officer、tax_specialist 没有 source.register（tax_specialist 也没有 file.register），compliance 没有 source.register：依据银行水单、账单、发票与税务文书作决定的人不能登记这些证据（D2a 的 CN-AT10 由 admin 登记税务局文书） | A7b、D1、D2a、D2b |
| shipment.confirm：事件风暴 ES-10、ES-12 写单证员，岗位模板只给跟单（按模板实现） | B1a、C1 |
| order.activate 在销售经办模板，计划 §5 写订单审核人 | D1 |
| order_account.open 借用 payment_plan_review.open 权限；建议设自有权限，由资金岗位与系统消费方持有，销售与 AI 不持有 | B2a、B6b |
| commercial_release.grant_exception 与 certificate.register 没有独立性规则；quality_release.grant 无独立性已由负责人决定保持（S5-D34） | A8、C1、C5 |
| commercial_release.evaluate 的系统操作者需要授权，D-18 没有列出这样的系统消费方 | C1 |
| shipment_review.open、allocation_review.open 在跟单、采购模板中，但只有系统使用 | C4b-FUL、C4b-PUR |
| POL 发布权限按命令而不按配置种类：任何持 config.publish 的人都能发布信用政策与出口退税政策；POL 不核对 approval_tiers 的层级是否对应岗位模板 | A3a |
| 没有客户与销售经办的指派，“不是该客户的销售经办”只能近似 | A6 |
| AI 代理调用 FUL 的 shipment.get 需要 ful.read | C3a |

### 待负责人决定的问题

| 问题 | 发现单元 |
|---|---|
| order_account.open 的权限（见上表） | B2a、B6b |
| 出运由谁确认：跟单（模板）还是单证员（事件风暴） | B1a、C1 |
| 本地取消、未报关的出运，其许可证分配永远不能释放（只有外部结果释放）；一个出运需要两张许可证时不支持 | B5 |
| 生效订单持有已交付未出运的货物时不能取消：SM-TRD-ORDER 的“或已有获批处置”没有来源（可由 PUR 命令写入 DownstreamCommitments v1 的 disposition，或由订单结束前的复核结果提供） | C4b-PUR |
| CIF／CIP 缺保险单的码：FUL-INV-005 与错误码目录写 INSURANCE_COVERAGE_INSUFFICIENT，计划写 SHIPMENT_DOCUMENTS_UNRECONCILED（按计划实现）；TRD／POL 没有合同保险比例；核价保险组件是否按部分出运分摊 | C6 |
| 事件队列（Outbox 中继）延迟目标：C7 建议参考负载下 P95 ≤ 5 s、2—3 个分发进程 | C7 |
| 信用：新修订信用需求变小时占用不减，敞口在订单结束前偏高；consumed → released 是否增加“被后续修订替代”；游离占用（应用未要求而另行占用）至过期前计入敞口。D2b 的 SIM-02：订单减到 8,280.00 后，较大的占用保持 consumed，敞口仍按 16,200.00（含期初 7,000.00）计至订单结束 | C4b-STL、D2b |
| 付款条件例外的有效期上限（单元取 90 天，设计无值） | C1 |
| 单证放行的细则：已结束订单上撤销生效批准仍拒绝；PTY 筛查严格（任何非 clear 都阻止，含筛查过期与名单更新）还是只识别真实阻断；CNX 中间修订不查 | C1b、C1c |
| 订舱是否需要“已申请”状态；证书适用性的判定时点、每张原产地证是否必须是登记的 CNX 结果、证书种类是否包括目的市场供应链合规证据与优惠原产地 | C2、C5 |
| 报关 unknown 之后“海关从未收到”的路径；出口日期与最终用户的规则已由负责人确认（S5-D35） | C3a |
| 退税用途 export_refund／export_exemption 与退税方式（免税与征税未区分）须专业确认 | A3a、C3b |
| 国别：区域级项目（ISO 3166-2）不能表达；POL 公司档案、适用法律与筛查条目的国别只校验格式；国别项目来源的批准矩阵行仍为 PENDING | A3b |
| 商业发票与其他发票共用 POL 的 invoice 计数器 | A3a |
| 贸易合规：评估有禁运命中时人工仍可判为 clear（A7 决定 4：命中只是候选，由合规人工判断）；SC-09 的“订单审批不能覆盖”成立，因为 TRD 的订单审核从不覆盖 CNX 的结论 | A7、D2b |
| 质量：撤销的放行全额计入，冻结的未出运数量永远不能再放行（D2b 的 BT21：冻结的 24 件再放行为 QUALITY_RELEASE_EXCEEDS_CONFORMING） | A8、D2b |
| 结算：订单经营结果不能把结汇记录（fx_conversion）归到订单的业务结算汇差，净现金不含费用付款（D2b 的 S-A 改为按结算汇率直接核销人民币到账，得出基线的 −100.00 汇差，净现金 2,900） | C9、D2b |
| 红字发票对应付（暂估）的影响没有命令或消费方（应付只依据核实交付）；第三方付款人的付款依据、主体不一致的“获批方式复核”没有命令 | B2a、B2b、C3b、D2a |
| 期初：期初应收不能开贷项；期初资金记录不能冲销；期初应收币种与额度币种不同时为 FX_SCOPE_MISMATCH；POL 没有“追溯录入的历史政策修订” | C10 |
| 运营：出口退税政策须在海关放行之前发布，之后发布的修订不适用于已出口批次；FCA 下买方指定货代的运费仍记为我方直接费用（报价成本含运费）；出口退税政策的新修订只适用于发布后出口的批次（D2a 的 CN-AT12） | C3b、D1、D2a |

### 战术模型修订

| 类别 | 缺口 | 发现单元 |
|---|---|---|
| 状态机 | SM-FUL-RELEASE 增加 blocked → blocked、granted → granted；SM-FUL-SHIPMENT 没有准备与装箱的转换；SM-CNX-COMPLIANCE 增加 pending → pending 与重开原因（项目更新、过期、订单变更）；SM-CNX-DECLARATION 增加乱序前跳、amended → amended、released／amended → withdrawn；SM-CNX-TAXCASE 没有记录预计金额的转换；SM-CAT-DECLARATION 没有撤回与草稿丢弃；SM-STL-FUND 没有终态；MilestonePlan、OrderAccount 没有状态机（OrderAccount 也没有创建命令与终态）；CertificateResult 没有撤回；SM-STL-CREDIT-RESERVATION 未列 OrderClosedUnfulfilled 与“被后续修订替代” | A5、A7、B1a、B1b、B2a、B6、B6b、C1、C3a、C3b、C4b-STL、C5 |
| 复核 | 复核键为（订单、修订、原因），与 D-21 的（订单、修订）不同；处置词汇（取消、替换、重新评估、接受、更正）没有设计；没有把已准备出运改绑新修订的命令 | C4b-PUR、C4b-FUL、C4b-STL |
| 原则 7 的例外待登记 | inspection.record 的替代同时修改旧检验；shipment.confirm 同时消耗放行；结算单确认时改写阶段核销；红字发票核实写两张发票；承诺变更应用与取消写需求分配根 | A8、B2b、C1、C3b、C11 |
| 锁序 | PUR 的交付根先于承诺根（核实锁承诺但不写），计划只写了“承诺根 → 需求分配根” | B3 |
| 不变量文字 | FUL-INV-002 应写明已确认出运的单证放行重查什么；TRD-INV-019 未说明为已交付货物保留的分配是否算未处置；STL-INV-012 的唯一键文字（截止时点、来源）宽于实现；PUR-INV-008 的“获批容差”按超采解读 | B3、C1c、C4a、C10 |
| 模型 | FUL 的出运没有最终用户字段，CNX 不核对最终用户；PTY 没有 notify 角色，订单不绑定 notify 方；TRD 封存的地址没有国别、Incoterm 未封存；结算单不随缩小的计划重拆，不支持工作日阶段；TRD 应用与 STL 迁移之间的窗口答 revision_mismatch；贷项不拆税；单位假定承诺行与订单行一致；已取消订单的经营结果；净现金不含费用付款与差额解释（S-A 为 2,900 而基线为 2,250）；供应条件过期后的短交须重新核实供应条件；取消时未核实的到货不能再核实；QLT 让步是否使货物可放行 | A2、A3b、A8、B2b、B5、C3a、C4b-STL、C8、C9、C11 |

### 文本对齐（事件风暴、统一语言、WBS 与计划）

| 缺口 | 发现单元 |
|---|---|
| PL-13 未列 OrderClosedUnfulfilled；PL-14 的“目的国、交易方或商品变化”与“每个 OrderAmended 都开启核查”不同；D-18 的法规发布与 MD-47 的名单导入两项策略没有编号；WF-06 与 PL-09 只写 OrderAmended，计划还写了订单结束事件 | B6、C4b-CNX、C4b-FUL |
| SM-STL-PAYMENT-PLAN-REVIEW 的守卫“付款阶段与信用部分的变化逐项处置”可读作每一处变化，实现只列不再适配的部分 | C4b-STL |
| SC-08 的“STL 解释 2.00 即放行”实际还须把 2.00 核销 | C1 |
| 计划的单证 checked 状态与单独的确认命令、报关的 exported 状态在状态机中不存在（分别为签发时确认与 confirmed） | C2、C3a |
| 基线 9.3 的 BT17 是外厂材料、在制品与委外加工（MFG），战术模型把它映射到期初命令 | C10 |
| 计划 §5 第 2 步写一位信用经理发布 10,000，与顶层以上两人的规则不符；计划 §1 写的 CNX 迁移顺序与实际（C3b 取 000003、C3a 取 000004）不同 | A6、主导者 |
| “变更的申请人”读作承诺的申请人（变更没有单独的申请人） | C11 |

## 10 Python 对等 {#s10}

Python V0.50 没有 S5 的内容：08 阶段的映射表（577 行）没有 QLT、FUL、STL、CNX 或 STK 的行，也没有 A47 的行；PUR 的 22 行都是 T04 A46 的采购草稿，已在 S3 得出结论。A47 只存在于 Python 包的设计文档（T04_A46_PURCHASE_DRAFT_REPORT_V0.1.md 中“独立采购承诺审批设计”）。因此 S5 没有等价性表（ADR-BR-012）：S5-01 的测试引用 A47 的各点，其余测试引用设计编号（不变量、状态机、TD-AT、BT、CN-AT、SC、基线章节）。映射表的移植计数不变（已移植 455 行）。

## 11 门禁与远程 CI {#s11}

每个单元在自己的工作树与 PostgreSQL 16 容器上运行 CI=true make check 与 make integration（不允许跳过数据库测试）后报告；主导者在每次合并后运行 make check，并按批在主导者的数据库（容器 brant-pg16，端口 55471）上运行迁移、make integration 与 make race，通过后推送 s5/business，由 GitHub Actions 在全新的 PostgreSQL 16 上迁移并运行全部测试与竞态检测。逐批结果与日志摘要在证据 04，GitHub Actions 运行在证据 05。

| 合并批次（s5/business） | 单元 | 主导者门禁（WSL2，brant-pg16，UTC） | GitHub Actions（brant-ci） |
|---|---|---|---|
| 87a55fa | A0 设计源 | — | 43153565559 成功 |
| 55f7d85—2076e18、4afa604、10d0bfe | A2、A3b、A3a、A1、A5、A8、A4、A6、A7、A7b；岗位调整；测试窗口 | 上一主导会话：本机 make integration 在 adcc88e 的树上通过；10d0bfe make check 通过（11-12 06:22） | 4afa604 43162711473 成功；10d0bfe 43164621807 成功 |
| 5662d7e | B3 | 迁移 1 个；集成 81 包 ok；竞态 81 包 ok，无数据竞争（11-12 08:00） | 43179614057 成功 |
| f2bda8e | B6 | 集成第一次被外部进程以 signal: terminated 终止（另一单元按名称结束进程，此后禁止），重跑 82 包 ok；竞态 82 包 ok（08:29） | 43183442227 成功 |
| 9b5be90、7e59d81 | B1a、B2a | 迁移 2 个；集成 88 包 ok；竞态 88 包 ok（08:51） | 43186410797 成功 |
| e50492e | B5 | 迁移 1 个；集成 89 包 ok；竞态 89 包 ok（09:10） | 43189168946 成功 |
| f1c6f48、32dc238 | B1a2、C11 | 迁移 1 个；集成 90 包 ok；竞态 90 包 ok（09:30） | 43192152220 成功 |
| 9637ec1 | B6b | 集成 90 包 ok；竞态 90 包 ok（09:51） | 43195264580 成功 |
| ceb8026 | B4 | 迁移 1 个；集成 90 包 ok；竞态 90 包 ok（10:18） | 43206662127 成功 |
| 1a1ec07、79484ed、f88bee4 | B1b、C3b、C4a | 迁移 2 个；集成与竞态各 93 包 ok、1 个包失败：B1b 的 TestShipmentLineAndExportLotLookups 改了已出运行的单价，C4a 接入真实出运事实后被拒（11:32） | 43207039854 失败（同一测试；f88bee4 在门禁结束前被另一主导会话推送） |
| 582b692、9d668fc、6244a7e | 测试修正、C4b-CNX、C4b-STL | 迁移 1 个；集成 95 包 ok；竞态 95 包 ok（12:01） | 43214260963 成功 |
| a422f9e | C1 | 迁移 1 个；集成 96 包 ok；竞态 96 包 ok（12:40） | 43220082347 成功 |
| b760a7c、8f85e62、6378737、638d02a | B2b、C3a、C2、C4b-PUR | 迁移 4 个；集成 101 包 ok；竞态 101 包 ok（13:08） | 43224594901 成功 |
| 799b195、fe0b160 | C6、C5 | 迁移 2 个；集成 102 包 ok；竞态 102 包 ok（14:01） | 43232957188 成功 |
| 43122ce、2a69b7d | C4b-FUL、C8 | 迁移 2 个；集成 104 包 ok；竞态 104 包 ok（14:30） | 43238013494 成功 |
| 6a79b3e | C1b | 迁移 1 个；竞态 104 包 ok（15:17）；合并后的树即单元已通过集成门禁的树 | 43344603484 成功 |
| fc6fc58、ad9e7fd、3a7cb7f | C9、C7、测试修正 | 迁移 1 个；竞态 107 包 ok；集成 106 包 ok、1 个包失败：TestLoadGeneratorSmoke 在与套件共用的数据库上有 3 次探测超过 1 s 采样周期（Outbox 积压约 60 万行）；3a7cb7f 放宽该测试，make check 通过（11-13 01:54） | 3a7cb7f 43348362221 成功 |
| 4d1aa66 | C1c | 集成 107 包 ok；竞态 107 包 ok（11-13 02:36） | 43353425674 成功 |
| 2c7cd8a、fd6635b | D1、C10 | 迁移 2 个；集成 107 包 ok（02:53）；竞态 107 包 ok，无数据竞争（03:09） | 43357384395 成功 |
| 7aedd52 | D2a | 迁移 0 个；竞态 107 包 ok，无数据竞争（04:04）；合并后的树即 D2a 已通过集成门禁的树（107 包 ok） | 随 f76861c 推送 |
| f76861c | D2b | @@GATE-f76861c@@ | @@CI-f76861c@@ |

S5 期间发现并修复的缺陷：C4b-STL 版本的 PaymentConditionGuard v1 把超额核销的阶段按全额计入（C1 发现，B2b 在 stl 000004 中按到期额封顶并加测试）；B1b 的测试与 C4a 的规则冲突（582b692）；TestExpiredOrRevokedApprovalLeavesNoFormalArtifacts 的 3 秒窗口在竞态检测下过短（10d0bfe）；负载冒烟测试在共用数据库上的采样容差（3a7cb7f）。未发现其他产品缺陷。

## 12 证据 {#s12}

| 文件 | 内容 |
|---|---|
| README.md | 证据目录说明：来源、日期、环境与未执行项 |
| 01_environment.json | 生成本目录时的主机、Go、模块与 git 提交，以及各项原始运行的环境 |
| 02_go_source_manifest.txt | 最终提交全部源文件的 SHA-256（不含 docs/、brand/ 与生成的白名单）；构建器以此校验证据对应当前源码 |
| 03_make_check.txt | E2 单元在最终提交上运行 CI=true make check 的完整日志 |
| 04_gate_results.md | 各合并批次的主导者门禁（迁移、集成、竞态）：日志中的通过包数、失败、数据竞争与结束时间 |
| 05_github_actions_runs.json.txt | s5/business 的 GitHub Actions 运行记录 |
| 06_brc001_s5_chain.json | D1 第一次运行的链路记录：环境、夹具、19 位人工、301 个请求与三轮分发 |
| 07_brc001_s5_chain_run2.json | D1 第二次运行的记录 |
| 08_brc001_s5_chain_table.md | 由记录生成的表格：逐步结果、分发、金额与未执行项 |
| 09_brc001_s5_compare.txt | 两次运行的比对：规范化后 0 差异 |
| 10_capacity_and_recovery/ | C7 的阶梯负载报告（三轮）、汇总表与两轮演练的日志、事实表与校验结果 |
| 11_cnat_matrix.json、12_cnat_matrix.md | D2a 的 CN-AT 矩阵：每个用例的要求、测试、负责单元与其测试、观察结果、拒绝码、未执行项与偏差 |
| 13_cnat_cases/ | D2a 每个用例的完整记录 cn-at01—18.json（每个请求与分发轮次） |
| 14_s5_scenarios.json、15_s5_scenarios_table.md | D2b 的伴随场景矩阵：每个场景的设计编号、依据的单元与测试、断言的数字、偏差、省略部分与未执行项 |
| 16_s5_scenarios/ | D2b 每个场景的完整记录（每个请求与分发轮次） |

{{s5_evidence}}

### 未完成与缺口

| 项目 | 归属 |
|---|---|
| B0 客户场景与 G0、G3、G4 的真实证据（S5-06） | S4-08 之后；BC-D01 |
| 海关单一窗口、税务、银行、保险人与承运人连接器 | S6-05 |
| 库存路线（STK）、退货入库 | S6-01 |
| CLM 索赔与 CreditRequest v1（订单经营结果记为 not_enabled） | S6-03 |
| 任务与 AI 层、对象控制、风险传播消费方（WF-03） | S4 |
| 真实信用额度与层级、国别项目、许可证、HS 编码与退税规则 | B0 之前由财务、合规、税务与报关角色提供 |
| 生产或准生产环境上的容量、恢复与复制演练；中继延迟目标 | G4；负责人 |
| 界面（CN-AT17 的中文操作一半） | 前端 |
| 09 节的目录补录、Context Map 行、岗位模板与状态机修订 | 下一次设计修订 |

> [!warn] 尚未执行
> 真实客户样例与真人签核；海关、税务、银行、保险与承运人的真实系统（S5 只有人工路径）；真实信用、合规、许可证与税务数据（全部合成）；生产环境上的容量与恢复演练；真实身份提供方（测试用本地签发者）；S4 的任务与 AI；STK、CLM；界面。G0、G1 仍为 NO-GO，G3、G4 记为 NO-GO（模拟评审）。

> [!decision] 已决定（负责人，2026-11-12 至 11-13）
> S5 先于 S4，S4-06a 并入 S5（S5-D32）；计划的 D-1—D-22 与 E-1—E-9 全部按建议采纳，E-5 为全深度（S5-D01—S5-D31）；ADR-BR-018 记为阶段决定（S5-D33）；岗位调整（S5-D34）；出口日期与最终用户（S5-D35）；已出运货物的单证放行按当前修订判定付款、确认后出现的阻断仍阻止单证放行（S5-D36、S5-D37）。
