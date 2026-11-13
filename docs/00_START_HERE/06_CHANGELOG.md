# IDP Brant 主档案变更记录

## V1.8 · 2026-11-13 · S5 完成：B0 受限试点扩展（先于 S4）

S5 按负责人 2026-11-12 的决定先于 S4 执行并归档（S4 以后为 V1.9）。合并：新上下文 QLT（检验、处置、质量放行）、STL（客户信用与 CreditGuard、资金记录与核销、订单账户与 PaymentConditionGuard、结算单与业务应收应付、贷项与结算冲销、跨币种核销、费用分配与订单经营结果、期初余额）、CNX（贸易合规与 TradeComplianceGuard、出口许可证与用量台账、报关案、采购发票、退税案、官方证书、单证包与外部对账）、FUL（出运、装箱与履约根、节点、出口批次、商业条件放行、订舱与出运单证、保险、出运复核）；PUR 的采购承诺、直发交付与 SupplierDeliveryGuard、承诺变更与取消、分配复核；TRD 的订单快照、订单生效与变更应用接入信用与合规守卫、订单结束与变更读取出运与承诺事实；CAT 申报档案；POL 的信用与结算汇率、单证控制付款阶段与两种新配置；PTY 地址国别与国别项目参考数据；S4-06a 的 Inbox 订阅登记与进程内分发；黑盒负载工具与备份恢复脚本。BRC-001 经 HTTP 从订单走到结算、核销与中国资料；执行证据回写到 09 阶段。

### 新增
- 09：S5 执行报告 V0.1（12_IDP_Brant_S5_Execution_Report_V0.1.html；决定 S5-D01—S5-D40）与 13_S5_EVIDENCE（门禁摘要、GitHub Actions 运行、BRC-001 S5 链两次运行的记录与比对、容量与恢复演练的原始输出、CN-AT 与伴随场景矩阵）。

### 更新
- 09：WBS 中 S5-01—S5-05、S5-07—S5-10、S5-12—S5-20 为 DONE_CI（S5-05 的 CN-AT01—18 由 D2a 在一个套件中经 HTTP 运行，全部通过），S5-11 为 DONE_LOCAL（负载与演练只在本机）；S5-06 依赖 S4-08 与真实客户证据，不在 S5。S4-06 仍为 PARTIAL（订阅登记与分发已在 S5 完成，WF-03 的消费方属 S4）。
- 08：集成门 IG-06（Inbox 去重与重放）为 DONE_CI，IG-13（中国专业系统或人工路径对账）以人工路径与合成数据为 DONE_CI（连接器与专业角色签核仍缺），IG-14（备份恢复与升级演练）为 DONE_LOCAL。
- 10、03：产品门禁 G3、G4 记为 NO-GO（模拟评审，负责人决定 E-7）。
- 构建器：版本 1.8；新增 S5 证据检查，“最新证据快照”依次取 S5、S3、S2、S1、M0。
- 设计源（Context Map、战术模型、岗位模板、错误码目录）的 S5 内容已在 S5 A0（87a55fa）与岗位调整（4afa604）中修订，本版首次生成对应的页面与目录；单元报告的设计缺口汇总在 S5 报告第 09 节，未改设计源。

### 负责人决定（2026-11-12 至 11-13）
- S5 先于 S4；S4-06a Inbox 分发并入 S5；计划的 D-1—D-22 与 E-1—E-9 全部按建议采纳，E-5 为全深度（订单经营结果、费用分配、贷项、结算冲销、跨币种核销、承诺变更与全部 CN-AT），库存路线留给 STK。
- 外部专业系统只走人工路径；信用政策、国别项目与许可证为合成数据；演练在本机 WSL2；G3、G4 以模拟评审记为 NO-GO。
- 计划提出的 ADR-BR-018（双向关系的反向无锁读取）记为阶段决定，不进入 ADR 登记册（没有模拟评审记录）。
- 采购获得 number.issue；销售经办可释放自己未被消耗的信用占用；质量放行不设独立性规则。
- 出口批次的出口日期取报关单上的出口日期；FUL 不记录最终用户时出运许可证的最终用户取收货人。
- 已确认出运的单证放行按订单当前修订判定付款条件，已关闭订单仍可放行、已取消的不可以；确认后新出现的筛查或合规阻断仍阻止单证放行。
- 2026-11-13：S5 合并到 main 之前把 CI 的集成测试分片（并行作业，汇总作业仍名为 brant-ci），单元 CI（合并 ed1550c）：远程一次运行从 26.9 分钟降到 4.3 分钟。

### 限制
- 海关、税务、银行、保险人与承运人没有连接器（S6-05）；退税用途、申报要素、信用额度与国别项目未经专业角色确认。
- 容量与恢复的结论只基于一台 WSL2 主机；中继延迟没有目标（待负责人决定）。
- 单元报告的设计缺口（目录补录、Context Map 缺行、岗位模板、状态机与不变量文字、负责人问题）留待下一次设计修订。G0、G1 结论不变。

## V1.7 · 2026-11-11 · S3 完成：报价、确认、订单与采购草稿

S3 合并：TRD 报价根（工作稿、冻结与服务端计算、商业哈希、对象控制）、审核与毛利例外、正式化（报价模板与确定性渲染、POL 编号、文件字节、提交证据、风险暂停）、替代、撤回与取消、客户回复与承诺效力账本、合同成立、外销订单（准备、审核与风险确认、生效）、订单变更与生效订单的终止、外销合同与形式发票、查询与动作对账、适用法律与争议解决；PUR 采购草稿、需求分配与草稿作废；PTY 与 CAT 为 TRD、PUR 发布的守卫。BRC-001 全链在 PostgreSQL 16 上经 HTTP 重放；执行证据回写到 09 阶段。

### 新增
- 09：S3 执行报告 V0.1 与 09_S3_EVIDENCE（含每次合并的 make race 记录与 BRC-001 链路记录）。
- POL：配置类型 legal_terms（WBS S3-17，F-02）与 order_document_template（WBS S3-15），共 20 种；公司档案可指定租户默认适用法律；编号单据类型 proforma_invoice。

### 更新
- 03：Context Map 的 TRD 事实补上 AcceptanceDisputed、OrderReviewRequested、OrderAmendmentRequested、OrderAmendmentDecided、OrderAmendmentWithdrawn；R22 为 PartyRef v1＋PartyGuard v1；R52 改为守卫锁定（SkuRef v1＋SkuGuard v1），负责人决定。
- 05：战术模型的 POL 封闭枚举加入 legal_terms 与 order_document_template；SM-TRD-ACCEPTANCE 增加 disputed → invalidated（acceptance.invalidate），争议承诺须先作废，其报价修订方可替代、撤回或取消（负责人决定）；SalesOrder 事件加入 OrderAmendmentWithdrawn。
- 08：Python 测试映射 T03、T04 的 417 行全部有结论：313 行 PORTED（16 行缺次要部分，注明于行内）、28 行 DIVERGES_BY_DESIGN、67 行 NOT_APPLICABLE、9 行 NOT_PORTED 并写明业务理由；共享基础 PYT-0135 的渲染一半随 S3-04 移植。全表 577 行中已移植 455 行。 集成门 IG-03（BC-AT15、BC-AT17）与 IG-04（跨上下文资格重查）为 DONE_CI，IG-08（正式文档字节）与 IG-09（编号）为 PARTIAL。
- 09：WBS 中 S3-01 至 S3-17 改为 DONE_CI；S3 完成。
- 构建器：版本 1.7；新增 S3 已移植测试表与 S3 证据检查，“最新证据快照”依次取 S3、S2、S1、M0。

### 负责人决定（2026-11-10 至 11-11）
- S3 不接入 STL 信用守卫（R57）、CNX 贸易合规守卫（R58）与 TSK 对象控制命令（R17），登记为 S4—S5 接入；订单生效的提交证据与 OrderActivated 记录 not_wired，不视为通过。
- 毛利审批岗获得 quote.review.decide；销售经办获得 number.issue 与 quote.metadata.update。
- 已有有效或争议承诺的报价修订不得替代、撤回或取消，须先由人工作废承诺（ACCEPTANCE_ALREADY_EXISTS）。
- 报价修订生效后，其批准的有效期不再影响 quote_ready（与订单生效一致，ADR-BR-013）。
- purchase.overbuy.approve 在 S3 不授予任何岗位，超采待 S5 的独立承诺审批。
- 适用法律配置与公司默认适用法律由管理员起草与发布，与其他 POL 配置相同。

### 限制
- STL、CNX 守卫与 TSK 对象控制未接入；FUL、STK、STL 的事实在订单终止中记录为 not_wired，S3 中生效订单的关闭与剩余关闭因此不能完成。
- 适用法律只实现 CISG 与中国法的到达规则；其他法律在冻结时拒绝。
- 订单审批没有 POL 政策（有效期只要求晚于决定时刻）；EVD 文件登记的消费、上游事件触发的风险暂停未做。G0、G1 结论不变。

## V1.6 · 2026-11-10 · S2 完成：寻源与核价

S2 合并：SUP 供应条件与 SupplyConditionGuard v1，CST 计算规则、核价上下文与测量、场景评估、冻结快照、来源否认与 AdoptabilityGuard v1，INQ 的 RfqGuard v1。T02 的 660.00 USD 基线在 PostgreSQL 16 上冻结；执行证据回写到 09 阶段。

### 新增
- 09：S2 执行报告 V0.1（决定 S2-D01—S2-D22）与 07_S2_EVIDENCE。
- 03：Context Map 新增 R64 EVD→CST（SourceRef v1），负责人决定。

### 更新
- 03、05：SUP 与 CST 实现的修订、丢弃与撤回命令和 LogisticsOfferWithdrawn、CostingContextVerified、MeasurementRecorded 事件登记进 Context Map 与战术模型；costing_context.withdraw、measurement.supersede、measurement.withdraw 加入人工专属清单。
- 05：岗位模板把新命令分给采购与核价岗位，costing.attest_source 另归合规；独立性清单加入 supplier_offer.verify、purchase_terms.publish、logistics_offer.verify 与 costing_context.verify。
- 06：错误码的适用上下文补上 SUP、CST（COST_INPUT_NOT_READY、CALCULATION_LIMIT_EXCEEDED、CHARGE_DOUBLE_COUNT、ALLOCATION_INVALID、SUPPLY_CONDITION_*、COST_SNAPSHOT_INELIGIBLE）。
- 08：Python 测试映射 T02 的 87 行：74 行 PORTED、9 行 NOT_APPLICABLE、4 行 NOT_PORTED 并写明原因。全表 577 行中已移植 141 行。
- 09：WBS 中 S2-01 至 S2-08 改为 DONE_CI；S2 完成。
- 构建器：版本 1.6；新增 S2 已移植测试表与 S2 证据检查，“最新证据快照”依次取 S2、S1、M0。

### 仓库
- 2026-11-11 按负责人决定，仓库由个人账号 idpxyz 转入组织 idp-xyz，并改为公开（github.com/idp-xyz/idp-brantgo）；提交、分支、PR #1—#5 与 Actions 运行记录随仓库转移，旧链接自动跳转。公开前扫描了全部历史：没有令牌、密钥或个人邮箱，唯一的口令是 Makefile 中仅供本机开发库使用的口令。
- 2026-11-11 按负责人决定，main 设为受保护分支：必须通过 brant-ci（GitHub Actions），对管理员同样生效，禁止强推与删除；不要求评审与签名溯源。G1 C09 为 PARTIAL（必需检查已配置）。

### 限制
- 主体与 SKU 主数据修订不进入成本快照的可采纳判定（PTY 不是 CST 的上游，CAT 不发布守卫）。
- 读取核价上下文历史时不经 EVD 重查证据（与 S1-D16 相同）。
- TRD、PUR 尚未调用 AdoptabilityGuard（S3）。G0、G1 结论不变。

## V1.5 · 2026-11-10 · S1 完成

S1 第三批合并：PTY 交易档案、地址角色与服务方角色，CAT 货号映射，brant-cli 基础演示；S1 的 11 项工作全部有本机与远程 CI 记录。

### 新增
- 09：S1 执行报告新增决定 S1-D33—S1-D40（地址角色、服务方角色、交易档案、contact.revise、货号映射、REFERENCE_STALE 的用法、基础演示、HTTP 映射的复数缩写）。

### 更新
- 03：PTY 命令新增 contact.revise，事件新增 ContactRevised、TradingProfilePublished、PartyScreeningRecorded；CAT 命令新增 item_code_mapping.save_draft；R60 的契约扩展到交易档案引用的付款与贸易条款。
- 05：Contact 聚合加入 contact.revise 与 ContactRevised；岗位模板：contact.revise 与 item_code_mapping.save_draft 归销售经办与采购，trading_profile.save_draft 另归采购；独立性清单加入 trading_profile.publish。
- 06：错误码 INDEPENDENT_REVIEW_REQUIRED 适用 PTY，BUSINESS_KEY_CONFLICT 适用 CAT 与参考数据。
- 08：Python 测试映射 T01 与共享基础 73 行：67 行 PORTED、2 行 DIVERGES_BY_DESIGN（引用 X-INV-003、POL-INV-003）、3 行 NOT_APPLICABLE；PYT-0135 的渲染一半改归 TRD，随 S3-04 移植。
- 09：WBS 中 S1-02、S1-03、S1-07 改为 DONE_CI；S1 完成。
- 构建器：版本 1.5。

### 限制
- TradingProfileRef 与 AddressRoleRef 只有事务前查询，没有守卫函数（R13 有守卫锁，TRD 在 S3 需要）。
- 货号映射没有退役命令，已发布的键不能释放；服务方角色没有撤回，交易档案没有草稿丢弃。设计中都没有对应命令，作为设计缺口登记。
- 真实身份提供方仍按负责人决定延后；G0、G1 结论不变。

## V1.4 · 2026-11-10 · S1 第二批：筛查、岗位模板与 POL 全部配置

S1 第二批合并：POL 其余 13 种配置与 ConfigGuard v1、主体筛查与筛查名单、岗位模板与职责分离、OpenAPI 3.1 候选、Inbox 去重与提交结果未知的对账。执行证据回写到 09 阶段。

### 新增
- 05：岗位模板与职责分离源文件 `06_IDP_Brant_Role_Templates_V0.1.json`（BC-D02）：15 个默认岗位、禁止组合、专属权限与独立性规则；Go 的岗位表由它生成，构建器校验岗位覆盖全部命令、权限均已登记、专属权限只归其岗位。
- 09：S1 执行报告新增决定 S1-D17—S1-D32（ConfigGuard、引用方式、银行账号遮蔽、信用政策、HTTP 反射映射、筛查结论推导、匹配规则、ADR-BR-017 默认、参考数据、岗位新增权限、Inbox、提交结果未知、账本同键并发）。

### 更新
- 08：Python 测试映射表又有 10 行改为 PORTED（共 63 行），3 行注明已移植一半，4 行写明未移植原因。
- 09：WBS 中 S1-04、S1-06、S1-09、S1-11 改为 DONE_CI，S4-06 改为 PARTIAL（Inbox 去重与对账已做，订阅登记与 WF-03 风险传播未做）。
- 10：G1 的 C07 改为 MET（本机＋远程 CI），C06 列入全部 brant_ schema；G1 页的当前满足情况改按 S1 证据重写；ADR-BR-015 的证据不再写“死信与重放未实现”，ADR-BR-004、ADR-BR-017 登记 S1 的实现。
- 12：依赖规则 D6 允许 transport、ai、processes 导入 reference/api；internal/reference 在 S1 建立（归属仍待决定）。
- 构建器：版本 1.4。

### 发现
- Bento v0.1.0-rc.2 与 pgx v5.10.0 把丢失的 COMMIT 应答报告为普通的连接已关闭，而不是 ErrCommitUncertain；ScopeTransactor 已补正，修复应进入 Bento 上游。
- 命令账本先查已存结果、再锁根行：同一幂等键的并发请求可能在两者之间提交，使后到者得到 VERSION_CONFLICT（GitHub Actions 运行 42815414128 在竞态检测下出现）。已修复，并以强制该交错的测试固定。

### 限制
- 真实身份提供方接入按负责人 2026-11-10 的决定延后至 B0 试点门之前；C10 的其余要求已满足。
- ADR-BR-017 的默认名单未内置；没有定时复筛任务；名单只做精确匹配（name-key-v1）；名单按公司导入。
- 证据为本机执行（E2）加 GitHub Actions 运行记录；没有真实名单文件、真实客户样例与真人签核。G0、G1 结论不变。

## V1.3 · 2026-11-10 · S1 第一批与远程 CI

仓库推送到私有仓库 idpxyz/idp-brantgo，GitHub Actions 远程 CI 首次通过；S1 主数据与询价切片完成第一批，执行证据回写到 09 阶段。

### 新增
- 09：S1 执行报告 V0.1；05_S1_EVIDENCE 收录环境、源文件 SHA-256 快照、make ci、逐项测试日志、GitHub Actions 运行记录与严格 JSON 模糊测试。
- 03：Context Map 新增 R61 EVD→PTY、R62 EVD→CAT、R63 EVD→POL（SourceRef v1，无守卫锁）。PTY 政策要求主数据核实须具备来源读取权限，原图缺少让 PTY、CAT 询问 EVD 的关系；R63 按负责人决定让配置发布校验核实证据可读。关系数由 60 条变为 63 条，import 白名单随之重新生成。
- 构建器：版本 1.3；“证据对应当前源码”改为对最新一次证据快照（S1，没有时为 M0）比对；新增 S1 已移植测试表；已移植数由映射表实算。

### 更新
- 08：Python 测试映射表 T01 与共享基础 V0.2 的 73 行中 53 行改为 PORTED（登记 Go 包与测试名），4 行注明已移植一半，3 行 SQLite 专属改为 NOT_APPLICABLE。
- 09：WBS 回写 S1 状态。
- 10：G1 检查清单回写远程 CI、OIDC 与死信的证据；C01 的关系数改为 63；C09 记为负责人接受的缺口。M0 执行报告中“移植数仍为 0”改为固定的历史表述。

### 限制
- POL 在配置被使用时不重查核实证据可读（Python 会重查）。
- 受保护主干与必需评审未配置：GitHub 免费版的私有仓库不提供，负责人 2026-11-10 决定不配置（C09 非阻断）。
- 证据为本机执行（E2）加 GitHub Actions 运行记录；没有真实身份提供方、真实客户样例与真人签核。G0、G1 结论不变。

## V1.2 · 2026-11-09 · 品牌标志

按负责人选择，在全部页面使用仓库 `brand/`（IDP Brant Logo engineering V1.2）中的标志。

### 更新
- 各阶段页面主视觉改用白色完整组合（含 AI-Native Trade Operations Platform），侧栏与首页品牌行改用白色无副标题组合，替换原来的文字标志。
- 全部页面加入 favicon：深色方形图标 32 px 与 64 px。
- 图片原样以 data URI 内联，页面仍可单文件离线打开；不改色、不缩裁、不重绘，未使用 SVG 描摹。档案配色不变（品牌调色板尚未正式批准）。
- 构建器：版本 1.2；新增校验——内联的 4 个品牌文件与 `brand/05_QA/sha256_manifest.json` 逐字节一致，且每个页面都使用了标志；版本登记新增 brand 段；找不到 `brand/` 时退回文字标志。
- 80：全部预览截图重新生成。

### 限制
- 品牌包状态为“视觉已锁定，生产母版尚未授权”；方形图标在 16–24 px 下可能难以辨认，不是正式签发的 favicon。G0、G1 结论不变。

## V1.1 · 2026-11-07 · M0 工程基线（本机证据）

在 idp-brantgo 根目录建立 Go Module `go.idp.xyz/idp-brant`，完成 WBS M0 的 11 项工作，并把本机执行证据回写到本档案。

### 新增
- 10：M0 执行报告 V1.0；06_M0_EVIDENCE 收录环境说明、源文件 SHA-256 快照、两次 make ci 日志（Go 1.26.5 与 1.26.8）、逐项测试日志、模糊测试、两次迁移、依赖检查负向实测、一致性实测 JSON、崩溃演练、lint 结果与时钟观察。
- ADR-BR-IMP-002（本地授权投影与 authority_epoch 守卫，brant_auth）；ADR-BR-REPO-11（D5 澄清、D9 测试支撑包、AC6 的检查方式）。
- 06：PG16 schema 归属表增加 bento、brant_auth、brant_migrations。

### 更新
- ADR-BR-004、ADR-BR-011、ADR-BR-REPO-10 由 OPEN 改为 PROPOSED，附实测与建议决定，仍待签核。
- G1 检查清单：9 项 PARTIAL，C01、C11、C12 为 NOT_MET，没有 MET；G1 仍为 NO-GO。
- WBS：M0 的 9 项为 DONE_LOCAL，M0-03（待 ADR-BR-REPO-10 裁决）与 M0-09（缺远程仓库与主干保护）为 PARTIAL。
- 集成门禁：IG-01、IG-02、IG-05 为 DONE_LOCAL；IG-03、IG-04、IG-07 为 PARTIAL。Go 对齐矩阵按 M0 更新，新增依赖检查、Outbox 中继、租户与授权三行。
- 12：GR 规则由“守卫视图”改为“SECURITY DEFINER 守卫函数”（对视图执行 FOR SHARE 需要底层表的 UPDATE 权限）；D5、D9、AC 表与现状表。
- 08 的 Go 侧技术就绪表；01、06 的现状描述；首页与“请先阅读”。
- 构建器：版本 1.1；Go 测试计数取自仓库源码；新增 M0 证据齐全、实测结论与报告一致、证据对应当前源码三项校验。

### 结论
- G0 NO-GO；G1 NO-GO。证据层级为本机执行（E2）；尚无远程 CI、主干保护、OIDC、Inbox 与任何业务上下文。

## V1.0 · 2026-11-06 · 首版（01—12 全阶段初稿）

按负责人选择（idp-brantgo 根目录为 Go Module、docs/ 为主档案；一次性完成 01—12 初稿再统一审），以 IDP-Cargo 主档案 V2.9 为标准建立 Brant 主档案。

### 新增
- 00：唯一首页、请先阅读、版本登记、文件清单、SHA-256、结构校验报告、本变更记录；构建工具（tools/）。
- 01：主白皮书 Go AI-Native V1.0；产品与研发范围基线 V1.0、业务核心详细设计 V0.2 及两份补充规格原件。
- 02：UL V0.1 Draft（159 个候选词条、25 条语言不变量、11 组冲突、10 项语义决定、8 项语言变更）及 HTML、CSV。
- 03：Context Map V0.1 Draft（18 个候选上下文、43 条关系、32 条事实权威、6 个验证场景、12 项战略 ADR、G0—G5）及 HTML、CSV。
- 04：事件风暴 V0.1（8 个场景、7 个热点、8 条策略）及 HTML、CSV。
- 05：战术 DDD V0.1（40 个候选聚合、16 个状态机、51 条不变量、34 条参考验收、5 个编排、事件信封）及 HTML、CSV。
- 06：工程契约 Go V0.1（命令、事件、错误码目录，事件信封 Schema，PG16 schema 归属）；Python 候选契约原件。
- 07：Python V0.50 基线审查（模块清单、表对照、17 项发现）；679 项 unittest 重跑证据。
- 08：Go 迁移就绪度（577 个唯一测试方法逐项映射；14 项集成门禁）。
- 09：实施计划 Go V0.1（M0、S1—S6 共 58 项工作）及 WBS JSON、CSV。
- 10：M0 基线与 G1 门禁（12 项控制）、ADR 登记册（31 条）。
- 11：DDD 方法论 Go AI-Native V1.0 与 7 份模板。
- 12：ADR-BR-IMP-001、仓库结构基线 V1.0、idp-bento-go 消费者基线、Go 对齐矩阵、import 白名单。
- 80：页面截图；90：关键原件 ZIP 与历史版本登记。

### 结论
- G0 NO-GO；G1 NO-GO。阻断项：ADR-BR-004、ADR-BR-011、ADR-BR-REPO-10。
- 没有 Go 代码、PostgreSQL 16、CI、真实身份或真实客户证据。
