---
output: 09_IMPLEMENTATION_PLAN/04_IDP_Brant_S1_Execution_Report_V0.1.html
title: IDP Brant S1 主数据与询价切片执行报告 V0.1
short: S1 执行报告 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / S1 EXECUTION REPORT
doc_title: S1 主数据与询价切片执行报告 V0.1
doc_desc: 远程 CI 跑通 · 命令账本 · EVD、PTY、CAT、INQ、POL · HTTP 入口、OIDC 与 OpenAPI 候选 · 岗位模板 · 主体筛查与名单 · 死信、Inbox 与提交未知恢复 · BRC-001 前段重放 · Python 测试移植
meta: 版本=V0.1; 日期=2026-11-10; 证据=E2 本机执行＋GitHub Actions; 结论=S1 部分完成 · G1 NO-GO
stage: 09 · 工程实施总体计划
eyebrow: S1 EXECUTION REPORT
search: 搜索工作项、测试、Python 用例或证据
foot: S1 Execution Report V0.1 · 2026-11-10
---
<span class="scope-badge">S1 PARTIAL · REMOTE CI GREEN · G1 NO-GO</span>

<h2>远程 CI 已跑通；询价主链在 PostgreSQL 16 上从来源走到 ready，并能经 HTTP 与 OIDC 令牌重放</h2>

<p class="lede">2026-11-10，修复了使 GitHub Actions 无法解析的工作流文件，远程 CI 在私有仓库 idpxyz/idp-brantgo 上首次通过。S1 建立了命令账本（幂等、重放、审计与 Outbox 同一事务）和 EVD、PTY、CAT、INQ、POL 五个上下文，HTTP 入口以 OIDC 访问令牌认证，Outbox 有了死信、告警与人工重放。BRC-001 前段（四行合计 120 PCS 的询价确认为 ready）在 PostgreSQL 16 上直接调用和经 HTTP 都已重放。同日第二批：POL 全部 18 种配置与 ConfigGuard v1，主体筛查与 PartyScreeningGuard v1，筛查名单作为参考数据受控导入，岗位模板与职责分离，OpenAPI 3.1 候选，Inbox 去重与提交结果未知的对账。真实身份提供方按负责人决定延后；G1 仍为 NO-GO（审批为模拟）。</p>

::: cover
- 结论 | S1 部分完成：S1-01、04、05、06、08、09、10、11 完成并有远程 CI 记录；S1-02、03、07 部分完成（交易档案、地址与服务方角色，货号映射，7 个 Python 测试未移植或只移植一半）。
- 证据 | 05_S1_EVIDENCE：环境、源文件 SHA-256 快照、make ci、逐项测试日志、GitHub Actions 运行记录、严格 JSON 模糊测试。
- 未执行 | 真实身份提供方（负责人决定延后至 B0 试点门之前）、Inbox 订阅登记与 WF-03 风险传播（S4-06）、定时复筛任务、真实名单文件与真实客户样例。
:::

## 01 结论摘要 {#s1}

::: cards
- G1 · C04、C05、C06 | 远程 CI 跑通 | 工作流文件第 67 行的 YAML 解析错误使首次推送 0 秒失败；修复后 GitHub Actions 全部 17 步通过，S1 分支每次推送都在全新 PostgreSQL 16.12 上迁移并运行集成与竞态测试。
- G1 · C09 | 受保护主干：负责人决定不配置 | GitHub 免费版的私有仓库不提供分支保护与规则集（API 返回 403）。负责人 2026-11-10 决定不升级、不配置；C09 本为非阻断项，记为接受的缺口，合并前以 PR 上的 CI 检查为准。
- S1 公共骨架 | 命令账本 | 契约、权限、已存幂等结果、CAS 与状态、引用的准入顺序沿用 Python；业务变化、动作结果、审计与 Outbox 同一事务提交；重放重查当前权限与可见性；拒绝一律审计。
- S1-01 | EVD 来源四层身份与业务文件 | 内容按 SHA-256 在公司内去重；邮件按渠道、账户与消息 ID 识别，同消息不同正文拒绝；来源不可变；猜正文探测不到他人私有来源。业务文件最多 10 MiB，校验类型与文件头，版本链不分叉。
- S1-02 · 03 | PTY 主体与联系人、CAT 的 SKU | 整稿草稿、人工核实、修订回到草稿并保留已核实版本、停用；主体用角色集合，同一主体可同时是客户与供应商；SKU 每次核实固定一个规格修订号。
- S1-05 · 08 | INQ 客户询价 | draft／ready／cancelled／closed；确认只限人工，须覆盖全部来源；记录确认时客户、联系人行版本与每行 SKU 规格修订号。BRC-001 前段四行 120 PCS 确认为 ready，直接调用与经 HTTP 各重放一次。
- S1-06 · C10 | HTTP 入口、OIDC 与 OpenAPI 候选 | 1 MiB、64 层、10 万个值（名单导入另为 32 MiB、100 万个值）；拒绝重复键、null、浮点数、未知字段、大小写不同的键名与空字符串；访问令牌须有签发者、受众、有效期、租户、公司与显式的操作者类型。OpenAPI 3.1 候选由传输层的命令表与请求结构生成，测试保证与代码一致。
- S1-04 | POL 全部 18 种配置与 ConfigGuard v1 | 配置的修订、发布须由编制人以外的人工执行（数据库约束同样强制）、原子替代、退役；汇率按方向与用途、不推导反向、过期即阻断；税额拆分精确、未知可抵扣性阻断；单位换算按 SKU 规格修订、需舍入即拒绝；银行账号无敏感权限只见末四位；单据编号按公司、类型、年度原子分配；基础快照须有公司档案与定价政策。守卫函数对确切修订行 FOR SHARE，三种提交顺序实测符合设计。
- S1-09 · S1-11 | 主体筛查与筛查名单 | 名单版本由合规岗位受控导入（来源、发布日期、文件 SHA-256、条目数），条目不可改；按 screening_regulation 启用的名单精确匹配；疑似命中由合规人工裁决，AI 与 system 不能裁决；正向名单绑定主体修订与条目内容，负向名单永久阻断；新名单版本或主体修订使结论不再通过。PartyScreeningGuard v1 在四种提交顺序下实测符合设计（TD-AT15）。
- BC-D02 · C10 | 岗位模板与职责分离 | 15 个默认岗位由 05 阶段源文件生成，覆盖全部命令；禁止组合在定义与分配岗位时拒绝；专属权限（合规结论、名单导入、信用决定）只归其岗位。
- C07 | Inbox 去重与提交结果未知 | 消费方在一个事务内按事件 ID 去重并处理，重复投递只处理一次；丢失 COMMIT 应答报告为 COMMIT_UNCERTAIN，Reconcile 只读判定已提交、载荷冲突或未提交；在 PostgreSQL 16 上以注入故障的连接实测丢失 COMMIT 与丢失应答两种情形。
- S1-10 · C13 | Outbox 死信、告警与重放 | 四条放弃路径（含 Bento 在领取次数超限时的静默放弃）都写入死信并告警；撤销、否认与风险类事件为关键告警；人工按事件 ID 以原事件 ID 重放。
:::

状态说明：DONE_CI 表示验收项在本机与 GitHub Actions 上都通过；PARTIAL 表示部分完成；NOT_STARTED 表示未开始。

{{wbs_tasks phase=S1}}

## 02 设计决定与有意背离 {#s2}

| 编号 | 决定 | 依据 | 对 Python V0.50 的差异 |
|---|---|---|---|
| S1-D01 | 每个上下文一份命令账本表（action_result、audit_event），在自身 schema 内，FORCE RLS | AGENTS 规则：上下文只写自己的 schema | Python 为单库共享表 |
| S1-D02 | 锁序：先锁本聚合根 FOR UPDATE，再由账本以 FOR SHARE 锁授权守卫行 | ADR-BR-011 | Python 以 SQLite BEGIN IMMEDIATE 串行 |
| S1-D03 | INQ 在自身事务开始前读取 PTY、CAT、EVD（R01–R03 无守卫锁），领域按读到的事实裁决，次序与 Python 一致 | ADR-BR-004、Context Map | Python 在同一事务内直接 JOIN |
| S1-D04 | Context Map 新增 R61 EVD→PTY、R62 EVD→CAT（SourceRef v1，无守卫锁） | PTY 政策“主数据核实须具备来源读取权限”需要 PTY 询问 EVD，原图缺这两条边 | — |
| S1-D05 | Python master 拆为 PTY 的 party、contact 与 CAT 的 sku；主体用角色集合 | 05 阶段战术模型 | master.kind 单值；改 kind 在草稿中也被拒绝，Go 允许草稿中改角色 |
| S1-D06 | 数据范围 own／team／company 存在授权投影 actor_grant，变更递增权限纪元 | BC-D02、Python actor_access | Python 为独立表 |
| S1-D07 | 契约名按上下文：brant.evd／pty／cat／inq/0.1.0；路由 POST /api/brant/v1/&lt;bc&gt;/commands/&lt;command&gt;，请求体的 command 须与路由一致 | 06 阶段命令目录 | Python 为 brant.trade-core/0.1.0 单一入口 |
| S1-D08 | 草稿保存不发布集成事件；核实、修订、停用、确认、取消、关闭发布 | 06 阶段事件目录 | Python 每条命令写一条本地 outbox 行 |
| S1-D09 | 访问令牌默认只接受 typ=at+jwt，须带 kid；操作者类型声明必填，缺失从不当作人工 | RFC 9068；人工专属命令的安全前提 | Python 由本地适配器注入身份 |
| S1-D10 | 重复 JSON 键返回 SCHEMA_INVALID（HTTP 422） | 错误码目录 | Python CLI 返回 transport_error INVALID_INPUT |
| S1-D11 | POL 每个配置根只有一个编制中修订，保存为整稿替换；修订号单调且不复用 | SM-POL-REVISION | Python 每次保存产生新草稿修订 |
| S1-D12 | POL 配置类型按战术模型命名（fx_rate、number_sequence 等）；每个修订都有有效区间，fx_rate 必须有截止 | 05 阶段战术模型 | Python 为 fx、sequence，只有部分类型有有效期 |
| S1-D13 | 汇率以币种代码引用币种（币种语义按 POL-INV-002 不可变）；不设汇率上限；缺少交叉汇率返回 FX_SCOPE_MISMATCH，使用时过期返回 FX_STALE；换算由调用方用 kernel.ExchangeRate 一次舍入 | POL-INV-002、003；错误码目录 | Python 固定币种修订、汇率上限 1000、DRAFT_NOT_READY、CONFIG_EXPIRED，并提供 fx.convert 命令 |
| S1-D14 | 同一业务引用在同一编号序列下返回原编号；序列的年份按其时区计算 | POL-INV-005 | Python 对重复业务引用返回 BUSINESS_KEY_CONFLICT，年份取公司档案 |
| S1-D15 | 配置、编号与快照属于公司，不设数据范围归属；下游查询只需有效授权 | 事实权威：POL 为公司级配置 | Python 对编号与快照记录归属 |
| S1-D16 | Context Map 新增 R63 EVD→POL；发布时（在状态、独立性与有效期检查之后）发布人须能读取所引证据，否则 REFERENCE_INVALID；无 EVD 读权限为 PERMISSION_DENIED | 负责人决定（2026-11-10）；Python publish | Python 在配置被使用时还会重查证据可读，Go 未实现 |
| S1-D17 | ConfigGuard v1（brant_pol.guard_config_in_force_v1，SECURITY DEFINER，固定 search_path）对确切修订行加 FOR SHARE，一次只判一个修订；下游对所采用快照的每一项分别调用 | ADR-BR-004 方案 B；ADR-BR-011 | Python 每次使用时递归重查全部依赖 |
| S1-D18 | 币种与单位按代码引用（S1-D13 扩展到本币、计算币种、账户与信用币种、换算单位）；税率档案、日历等按确切修订引用；数据内的引用为 {"kind","id","revision"} | POL-INV-002 | Python 每个引用都固定 kind、id 与修订 |
| S1-D19 | 付款条件的日期基准与日历按整份条件声明；单位换算以 sku_id 与规格修订号限定，不设 sku_profile 类型，量纲在保存与发布时检查 | POL-INV-004；CAT 不是 POL 的上游 | Python 逐阶段声明，另有 sku_profile 配置 |
| S1-D20 | 银行账户不记 owner_row_version；主体的账户只对公司数据范围可见（POL 不询问 PTY 归属）；完整账号另需 pol.read_sensitive（只授人工与 system），否则只见末四位，ResolveConfigs 同样遮蔽 | BC-D07 数据策略 | Python 以 owner_row_version 与归属判定 |
| S1-D21 | 字段按 Go 契约命名（time_zone）；只有 fx_rate 必须有截止；pricing_policy 去掉恒定的 independent_review；tax_application 按术语表、party_id 可选；document_template 接受五种单据类型，模板解析另拒绝 {field:}；不提供 convert、preview 命令；快照不携带主数据；LookupFX 的定价政策可选，另给年龄上限时取较严者 | 05 阶段战术模型、术语表 | Python 为 timezone、sales_tax_application、实验上限与换算命令 |
| S1-D22 | credit_policy 按战术模型、ADR-BR-016 与 ES-11 设计：默认额度、额度来源、严格递增的审批层级、逾期容忍天数；字段集待信用 Owner 评审 | ADR-BR-016 | Python 未实现 |
| S1-D23 | HTTP 对 POL 配置数据与筛查视图做反射映射：成员名为 Go 字段名的下划线小写形式，配置引用为 {"kind","id","revision"}，时间为 UTC 微秒时间戳，未知值省略、数值 0 保留，视图可原样作为请求读回；OpenAPI 候选随之列出 18 种配置结构 | 契约 C7；S1-06 | Python 为手写 JSON Schema |
| S1-D24 | 筛查结论记录确切的 screening_regulation 修订；结论是否仍成立（主体修订、有效期、名单当前版本）在读取与守卫时由 brant_pty.party_screening_state 推导，名单导入与主体修订不写下游；party_screening.due 列出待复筛主体，含规则修订变化（守卫不感知，R60 无守卫锁） | ADR-BR-004 方案 B；不跨上下文写 | Python 未实现 |
| S1-D25 | 结论有效至筛查时间加复筛间隔与规则截止中较早者；裁决针对整条结论；真实命中按名单条目记住，复筛仍阻断，条目移出名单才解除；负向名单永不解除 | PTY-INV-002；TD-AT15 | Python 未实现 |
| S1-D26 | 匹配规则 name-key-v1：NFKD、去附加符号、小写、ß 作 ss、去撇号与句点、其余非字母数字为分隔，键相等才算命中；无模糊、音近、音译或 AI 匹配；名称无字母或数字的主体拒绝而不判为通过 | 合规可解释性 | Python 未实现 |
| S1-D27 | ADR-BR-017 的默认（中国名单与联合国名单默认启用）不内置：规则修订列出的名单就是启用的名单；外国名单须登记法务确认的 EVD 来源，发布时须列为可读证据 | ADR-BR-017（SR-19）；R63 | Python 未实现 |
| S1-D28 | 筛查名单是参考数据（internal/reference，schema brant_ref，契约 brant.ref/0.1.0），按公司导入并 FORCE RLS，版本与条目不可变；名单导入的入口限制为 32 MiB、100 万个 JSON 值；D6 允许入站层导入 reference/api；跨租户共享名单目录须另立 ADR | 12 阶段仓库结构 | Python 未实现 |
| S1-D29 | 岗位新增权限：screening_list.import 只归合规岗位（专属）；ref.read 归全部岗位；ref.audit 归合规与管理员；pol.read_sensitive 归资金与管理员；参考数据权限登记为能力权限 | BC-D02；S1-09 名单来源由合规负责人确认 | — |
| S1-D30 | Inbox：消费方在一个事务内按事件 ID 去重并处理，处理者以 system:inbox.&lt;consumer&gt; 身份运行；只有 RejectEvent 记为永久拒绝，处理者返回的业务拒绝按暂时失败重试直至死信 | ADR-BR-015；Bento Inbox | Python 无 Inbox |
| S1-D31 | Bento v0.1.0-rc.2 与 pgx v5.10.0 把丢失的 COMMIT 应答报告为普通的连接已关闭；ScopeTransactor 在回调成功后为其附加 ErrCommitUncertain。账本的 Reconcile 只读并审计，需业务读权限而非写权限 | Python test_reconciliation_requires_business_read_and_does_not_repeat_write_permission | 修复应进入 Bento 上游 |
| S1-D32 | 账本先查已存结果再锁根行，同键命令在两者之间提交会表现为版本冲突；命令以 VERSION_CONFLICT 结束时在新事务中查同键已存结果，有则按重放或载荷冲突回答 | Python 在同一锁内判定；GitHub Actions 运行 42815414128 暴露该窗口 | — |

## 03 Python 测试移植 {#s3}

08 阶段映射表中 T01 与共享基础 V0.2 的 73 个测试方法：63 个已移植（PORTED），3 个为 SQLite 专属（NOT_APPLICABLE），7 个未移植。未移植的 7 个中 3 个已移植一半（模板渲染为文件不属于 POL；快照读取时重查证据未实现；SKU 档案一半属于 CAT），2 个属于 PTY、CAT 尚未实现的档案，1 个依赖 Go 不提供的 fx.convert 命令，1 个为 CLI 演示。主体筛查与名单在 Python V0.50 中没有实现，Go 测试引用 TD-AT15 与 PTY-INV-002。每个 Go 测试在注释中引用其来源。

{{s1_ported}}

## 04 证据 {#s4}

| 文件 | 内容 |
|---|---|
| 01_environment.json | 主机、Go、PostgreSQL、依赖与 git 提交 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256；构建器以此校验证据对应当前源码 |
| 03_make_ci_go1.26.5.txt | 以 CI 的 Go 1.26.5、CI=true 运行 make ci 的完整日志 |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果 |
| 05_github_actions_runs.json.txt | GitHub Actions 运行记录：编号、分支、提交、每一步结果 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码 30 秒模糊测试 |

{{s1_evidence}}

> [!warn] 尚未执行
> 真实身份提供方；Inbox 订阅登记与 WF-03 风险传播（S4-06，消费方尚未接入）；定时复筛任务（现只有待复筛查询）；主体交易档案、地址与服务方角色（S1-02）与货号映射（S1-03）；Bento 上游对丢失 COMMIT 应答的修复；真实名单文件、真实客户样例与真人签核。G0、G1 仍为 NO-GO。

> [!decision] 已决定（负责人，2026-11-10）
> 配置发布的核实证据须可读：Context Map 新增 R63 EVD→POL，POL 发布前经 EVD 读取证据，不可读返回 REFERENCE_INVALID（S1-D16）。受保护主干不配置：GitHub 免费版私有仓库不提供，C09 非阻断，记为接受的缺口。真实身份提供方接入延后到 B0 试点门之前；C10 的其余要求（令牌取得租户与公司、权限纪元、人工专属命令、岗位与数据范围）已满足。
