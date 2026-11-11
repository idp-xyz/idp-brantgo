---
output: 12_GO_FIRST_ALIGNMENT/02_IDP_Brant_Repository_Structure_Baseline_V1.0.html
title: IDP Brant 工程仓库结构基线 V1.0
short: Go 技术路线 / 仓库结构基线 V1.0
kicker: IDP BRANT / 12 GO-FIRST ALIGNMENT / REPOSITORY STRUCTURE
doc_title: 工程仓库结构基线 V1.0
doc_desc: 一个仓库 · 一个 Go Module · 18 个由编译器隔离的上下文 · 可执行上下文映射 · 事务与数据所有权
meta: 版本=V1.0 Baseline Candidate; 日期=2026-11-07（回写 M0）; 状态=待签核 · M0 工程基线已建立; 门禁=G0 / G1 NO-GO
stage: 12 · Go 技术路线裁决与追踪
eyebrow: REPOSITORY STRUCTURE BASELINE
search: 搜索目录、规则编号或上下文
foot: Repository Structure Baseline V1.0 · 2026-11-06
---
<span class="scope-badge">ENGINEERING BASELINE / PENDING SIGN-OFF</span>

<h2>代码放在哪里、谁能依赖谁、事务与数据归谁，都写成可检查的规则</h2>

<p class="lede">本文以 IDP-Cargo 工程仓库结构基线 V1.1 为模板，规定 Brant 的源代码如何承载 {{=contexts}} 个候选限界上下文：idp-brantgo 一个仓库、根目录一个 Go Module、按上下文组织、由编译器守住边界；依赖方向由 Context Map 生成白名单；事务与数据所有权按上下文划分。与 Cargo 不同的两点：主档案位于仓库 docs/（负责人 2026-11-06 选择），以及跨上下文资格重查的一致性策略尚按 ADR-BR-004（模拟评审接受）。</p>

::: cover
- 阅读约定 | “必须／禁止”是强制规则，CI 或评审据此拒绝变更；“应／不应”是默认做法，偏离须在 PR 中说明；“可”为可选。
- 状态标签 | M0、S1—S6 表示计划创建阶段；“待决定”需负责人裁决；M0 已落地的目录见第 18 章，其余均为计划。
- 依据 | Cargo 仓库结构基线 V1.1、Brant Context Map V0.1、战术模型 V0.1、idp-bento-go v0.1.0-rc.2 源码。
:::

## 01 决策摘要 {#s1}

> [!decision] 一个仓库 · 一个 Module · 18 个由编译器隔离的上下文
> Brant 采用单一产品仓库＋单一 Go Module 的模块化单体。业务代码按上下文组织，每个上下文由 module.go、api/ 与第二层 internal/ 构成编译器级边界；上下文只能依赖 Context Map 登记的上游，写操作不跨上下文共享事务，业务决策留在拥有它的上下文。不为每个上下文建 Module 或微服务；目录随真实代码创建，不预建空脚手架。

| 相对 Cargo V1.1 的差异 | Brant 的处理 |
|---|---|
| 上下文数量与代码 | 18 个候选上下文（PTY、CAT、POL、EVD、INQ、SUP、CST、TRD、PUR、MFG、QLT、STK、FUL、STL、CLM、CNX、CRM、TSK） |
| 主档案位置 | docs/ 在仓库内，保存本主档案；Go 构建不引用；若仓库公开，发布时排除 docs/ 或保持私有（ADR-BR-REPO-08） |
| 业务任务 | TSK 是上下文；AI 能力层仍在 internal/ai，不是上下文（ADR-BR-003） |
| 资格守卫 | Brant 要求事务内重查上游资格，与 Cargo T3 冲突；GR 规则按 ADR-BR-004 方案 B（模拟评审接受） |
| 首个切片 | EVD、PTY、CAT、POL、INQ（对齐 Python T01＋共享基础），而非 Cargo 的 ORG→RATE→COM→FUL→BOOK |
| 金额 | 十进制实现须与 Python 精确有理数规则逐分一致（ADR-BR-REPO-10） |

## 02 范围、依据与文档关系 {#s2}

本文只把业务语义映射到 Go 包、数据库 schema 与 CI 规则，不重新裁决业务边界；业务语义以 UL 与 Context Map 为准。

| 输入依据 | 版本 | 本文如何使用 |
|---|---|---|
| Context Map | V0.1 Draft：{{=contexts}} 个上下文、{{=relationships}} 条关系、{{=strategicAdrs}} 项 ADR | 上下文清单、分级与 import 白名单的唯一来源 |
| 统一语言 | V0.1 Draft：{{=ulTerms}} 个词条 | 包名、类型名与聚合名 |
| 战术模型 | V0.1：{{=aggregates}} 个候选聚合、{{=stateMachines}} 个状态机、锁序 | 聚合子包、事务规则 |
| Cargo 仓库结构基线 | V1.1（待签核，main 0900ec8） | 原则、规则编号与反模式 |
| idp-bento-go | v0.1.0-rc.2：postgres/transaction.go | 事务 owner 行为：同 owner 嵌套加入，不同 owner 返回 transactionConflict；READ COMMITTED |
| Python V0.50 | {{=pythonSourceModules}} 个模块、{{=pythonUniqueTests}} 个唯一测试方法 | 行为基准与测试映射 |

## 03 八条设计原则 {#s3}

| 编号 | 原则 | 检查方式 |
|---|---|---|
| P1 | 一个产品、一个仓库、一个 Module：idp-brantgo 根目录唯一 go.mod；idp-bento-go 与 idp-platform-go 以精确版本引用，不复制源码、不长期 replace | go.mod／go.sum 评审 |
| P2 | 按上下文组织、随代码创建：业务代码只在 internal/contexts/<bc>/，目录在第一段真实代码或测试出现时创建 | 评审 |
| P3 | 边界由编译器保证：上下文私有代码在第二层 internal/，对外只暴露 module.go 与 api/ | go build；边界探针 |
| P4 | 依赖方向来自 Context Map：上下文只能导入已登记上游的 api/；api 是叶子包 | tools/dependency-check |
| P5 | 事务不跨上下文：每个上下文持有自己的 Bento 事务 owner；跨上下文写入经 Outbox／Inbox（资格守卫读取按 ADR-BR-004 方案 B） | 跨上下文嵌套事务测试 |
| P6 | 决策留在拥有它的上下文：流程管理器默认放在拥有关键决策的上下文；processes/ 不含业务不变量 | 评审＋第 10 章归属表 |
| P7 | 数据按上下文拥有：每个上下文一个 brant_<bc> schema，只经自己的仓储写入 | dependency-check（Go 导入）；migration-check 与按上下文拆分的数据库角色待建（S1–S2），在此之前由评审检查迁移文件 |
| P8 | 人与 AI 走同一条命令路径：AI 能力层只调用各上下文 api 中的命令，不访问仓储 | dependency-check |

## 04 仓库与模块依赖 {#s4}

| 仓库 | Module | Brant 的使用方式 | 版本 |
|---|---|---|---|
| idp-bento-go | go.idp.xyz/idp-bento-go | 事务机制、版本化仓储接口、Outbox／Inbox、testkit 合同；Brant 不修改框架 | v0.1.0-rc.2（RC，本机缓存可用） |
| idp-platform-go | go.idp.xyz/idp-platform-go | 编号等跨产品能力（是否采用待 IG-09） | v0.1.0-rc.1（本机缓存） |
| idp-brantgo | go.idp.xyz/idp-brant（候选） | 全部外贸业务模型、应用、接口与 AI 业务能力；docs/ 为主档案 | M0 已建立 go.mod 与工程基线（2026-11-07） |

必须：go.mod 引用精确版本；go.sum 是 Go 1.26.x 执行 go mod tidy 生成的完整依赖图（Cargo V2.5 曾因 go.sum 只含两行而全部构建失败）。禁止：生产构建使用 replace；复制或 fork 框架源码；以 GitHub 源码路径代替 vanity import。待决定：go.idp.xyz/idp-brant 的 vanity 路由在验证前只作仓库内部导入路径（ADR-BR-REPO-09）。

## 05 运行时分层 {#s5}

{{rs_layers fig="FIG 01"}}

| 层 | 目录 | 职责 | 可以依赖 |
|---|---|---|---|
| 入口 | cmd/ | brant-api、brant-worker、brant-migrate、brant-cli | internal/bootstrap |
| 组合根 | internal/bootstrap | 读取配置、建立连接池、为每个上下文创建独立 bentopg.DB、装配模块、注册入站协议与订阅 | 全部内部包（唯一例外） |
| 入站与编排 | internal/transport · internal/ai · internal/processes | 协议适配；AI 能力网关；无单一 Owner 的流程 | 各上下文 api/、reference/api、kernel、foundation |
| 业务 | internal/contexts/<bc> | 18 个候选上下文 | 已登记上游的 api/、kernel、reference/api、foundation |
| 共享 | internal/kernel · internal/reference | 共享值对象；外部机构代码表 | kernel 只依赖标准库（或已批准的十进制库） |
| 技术基础 | internal/foundation | bento、identity、authorization、database、messaging、configuration、telemetry、interop | 外部 Module、标准库、kernel；禁止依赖任何上下文 |

## 06 目标目录结构 {#s6}

这是目标结构，不是待建清单。每个目录标注了创建阶段；没有到阶段的目录不得提前创建。

```text
idp-brantgo/
├── go.mod · go.sum                module go.idp.xyz/idp-brant（候选）          M0
├── README.md · AGENTS.md · Makefile · .golangci.yml                          M0
├── .github/workflows/             ci.yml · pg16-integration.yml               M0
├── .github/CODEOWNERS             上下文目录 → Domain Owner                    M0
├── cmd/
│   ├── brant-migrate/             bento → brant_<bc> 迁移                      M0
│   ├── brant-worker/              Outbox 中继 · Inbox 分发 · 定时器            M0／S4
│   ├── brant-api/                 HTTP 入口                                    S1
│   └── brant-cli/                 运维与本地演示                               S1
├── internal/
│   ├── contexts/
│   │   ├── evd/ pty/ cat/ pol/ inq/                                          S1
│   │   ├── sup/ cst/                                                         S2
│   │   ├── trd/ pur/                                                         S3
│   │   ├── tsk/                                                              S4
│   │   ├── qlt/ ful/ stl/ cnx/                                               S5
│   │   └── stk/ mfg/ clm/ crm/                                               S6
│   ├── kernel/                    Money · Quantity · Ratio · ValidityInterval · Scope   M0
│   ├── reference/                 外部机构代码表与筛查名单（受控导入，上下文只读）  S1（归属待决定）
│   ├── processes/                 仅无单一 Owner 的端到端流程                   按需
│   ├── ai/                        capability · gateway · policy · approvalgrant · execution · executionevidence   S4
│   ├── transport/http/            服务器 · 路由 · 中间件 · 入口资源限制          S1
│   ├── foundation/
│   │   ├── bento/                 每上下文 DB · 租户与公司事务                  M0
│   │   ├── database/ identity/ authorization/ configuration/                 M0
│   │   ├── messaging/             Outbox 中继 · Inbox · 订阅登记               M0／S4
│   │   ├── telemetry/             审计与指标                                   S1
│   │   └── interop/               中国专业系统与行业标准编解码                  S5
│   └── bootstrap/                 组合根                                       S1
├── migrations/
│   ├── manifest/                  跨上下文迁移顺序                              M0
│   └── <bc>/                      随上下文创建                                 S1—S6
├── contracts/                     OpenAPI · AsyncAPI · JSON Schema             随首个外部消费者
├── tools/                         dependency-check · contract-check · migration-check · codegen   M0／S2
├── tests/                         黑盒 E2E · 安全 · 性能 · ai-evaluation        S4
└── docs/                          本主档案（不进入 Go 构建）                   已存在
```

| 要创建的目录 | 前提条件（全部满足才可创建） |
|---|---|
| internal/contexts/<bc>/ | UL 中已有其术语与聚合候选；有第一条用例与测试；CODEOWNERS 已指定 Domain Owner；它依赖的上游关系已在 Context Map 登记 |
| internal/processes/<name>/ | 满足第 10 章判定；具名业务 Owner；brant_proc 迁移；与参与上下文的关系已登记 |
| internal/foundation/<x>/ | 至少有一个上下文或入站协议实际使用 |
| contracts/ | 首个外部消费者出现，且负责人批准公开范围 |
| tests/ | cmd/brant-api 出现之后；需要访问上下文内部的测试不放在这里 |

## 07 限界上下文内部结构 {#s7}

| 包 | 职责 | 允许导入 | 规则 |
|---|---|---|---|
| <bc>（module.go） | 构造 Module；实现 api 中的接口；注册 HTTP 处理器与事件订阅；把内部错误归类为 api 错误类别 | 本上下文全部包、foundation、kernel | 只导出 New、Module 及其方法；不导出领域类型 |
| api/ | 已发布语言：命令、回执、查询、引用类型、事件名与载荷、错误类别、本上下文需要的端口 | 标准库、kernel | 叶子包；不得别名、嵌入或返回内部类型；不导入其他上下文的 api |
| internal/domain | 聚合、实体、值对象、不变量、领域事件；根包放本上下文共享的 ID、值对象与错误 | 标准库、kernel | 不依赖 Bento、pgx、HTTP、AI；聚合子包只按 ID 互相引用 |
| internal/application | 用例（每个用例一个文件）；事务、授权、幂等与资格守卫编排；本上下文拥有的流程管理器 | domain、kernel、Bento application 接口 | 单一包；端口接口由消费方定义 |
| internal/adapters/postgres | 租户作用域仓储、命令回执、Outbox 写入、Bento 仓储端口 | application、domain、foundation、Bento | 只读写本上下文 schema |
| internal/adapters/http · events | 入站处理器、事件处理器 | api、application、foundation | 由 module.go 注册；不含业务规则 |
| internal/adapters/external | 外部系统防腐层 | application、domain、foundation/interop | 外部模型不得进入 domain |
| internal/integration | 需要真实 PG16 的集成测试（build tag integration）、Bento testkit 合同 | 本上下文全部包、Bento testkit | 不放根目录 tests/ |

```go
// internal/contexts/trd/module.go —— S3 创建；候选示意
package trd

type Module struct{ /* db *bentopg.DB · outbox · authz api.Authorizer · guards · clock */ }

var _ api.Commands = (*Module)(nil)

// cst 与 pol 只以资格守卫端口传入（实现方式取决于 ADR-BR-004）。
func New(db *bentopg.DB, authz api.Authorizer, cost cstapi.AdoptabilityGuard, pol polapi.ConfigGuard) (*Module, error)
func (m *Module) RegisterHTTP(mux *http.ServeMux)
func (m *Module) Subscriptions() []messaging.Subscription
```

TRD 的领域包按聚合拆分：internal/domain/{quote, response, acceptance, order}，根包放共享的 QuoteID、Scope 引用、错误与事件定义；聚合子包互不导入。Python 的 t03_storage.py 不按原样移植。

## 08 依赖规则与可执行上下文映射 {#s8}

Go 的 internal 规则只阻止越界导入，不限制方向；方向由 Context Map 生成的白名单在 CI 中检查。

| 编号 | 规则 | 级别 | 检查 |
|---|---|---|---|
| D1 | 上下文只能导入已登记上游的 api/，也只订阅这些上游的事件 | 必须 | dependency-check |
| D2 | api 包只导入标准库与 kernel，不导入其他上下文的 api | 必须 | dependency-check |
| D3 | api 包不得以类型别名、嵌入或返回值暴露内部类型 | 必须 | dependency-check＋评审 |
| D4 | 上下文不得导入 transport、ai、processes、bootstrap | 必须 | dependency-check |
| D5 | foundation 只导入外部 Module、标准库、kernel 与其他 foundation 包，不导入任何上下文、bootstrap 与入站层（M0 澄清，ADR-BR-REPO-11） | 必须 | dependency-check |
| D6 | transport、ai、processes 只导入各上下文与参考数据的 api（S1 起含 reference/api）；Module 实例由 bootstrap 注入 | 必须 | 编译器＋dependency-check |
| D7 | 导入其他上下文的 api 时使用 <bc>api 别名（如 cstapi、polapi） | 应 | dependency-check 警告 |
| D8 | 引用其他上下文的标识时使用本包定义的类型或 kernel 类型 | 必须 | 编译器 |
| D9 | internal/foundation/database/pgtest 等测试支撑包只允许测试文件导入（M0 新增） | 必须 | dependency-check |

{{dependency_matrix fig="FIG 02"}}

{{rs_whitelist}}

## 09 事务边界与跨上下文协作 {#s9}

idp-bento-go v0.1.0-rc.2 的 WithinTransaction 发现 context 中已有同一 owner 的事务时加入外层事务，owner 不同则返回 transactionConflict；每次 NewDB 生成新的 owner；默认隔离级别 READ COMMITTED。因此是否共享 owner 完全取决于组合根的装配方式，必须写成规则并用测试固定。

| 编号 | 规则 | 级别 |
|---|---|---|
| T1 | bootstrap 为每个上下文调用一次 bentopg.NewDB（可共享一个 pgxpool），不得把同一个 *bentopg.DB 注入两个上下文 | 必须 |
| T2 | 一个事务只修改一个既有聚合实例；聚合写入、命令回执、提交证据与 Outbox 在同一事务提交。同上下文内允许同时创建由本命令派生的新聚合，限于登记的例外：TRD order.prepare 占用名额并创建订单草稿（ADR-BR-002）；TRD quote.response.verify 创建 Acceptance；CST cost_snapshot.freeze 冻结场景并创建快照；PUR purchase.draft.prepare 更新需求分配余额并创建草稿 | 必须 |
| T3 | 对其他上下文的同步调用只允许查询，且在本上下文事务之外发起——资格守卫读取的例外见 GR 规则（ADR-BR-004，模拟评审接受） | 必须 |
| T4 | 对其他上下文的写入使用上游 api 的命令消息，经 Outbox 与对方 Inbox 投递，携带 CommandID | 必须 |
| T5 | 投递语义为至少一次；消费方按 CommandID 或事件 ID 去重 | 必须 |
| T6 | Inbox 分发就绪（S4）之前，跨上下文步骤由 API 客户端、操作员或 TSK 任务逐步发起，每一步只在一个上下文内提交 | 应 |
| T7 | 按 ADR-BR-011 的锁序显式加锁：本上下文聚合根 FOR UPDATE（多根按 ID 排序）→ 同根下级行 → 上游守卫 FOR SHARE（按上下文代码排序，含公司授权守卫）→ 动作结果与 Outbox；以 PG16 竞争测试固定 | 必须 |

### 9.1 资格守卫读取规则（GR，ADR-BR-004 模拟评审接受方案 B）

M0-10 在 PostgreSQL 16 上实测了这组规则（M0 执行报告第 05 章）：方案 B 在三种提交顺序下都符合业务设计，方案 A 存在已复现的提交窗口。实测把“守卫视图”改成了“守卫函数”，因为对视图执行 SELECT … FOR SHARE 要求调用方对视图本身有 UPDATE 权限，等于把写权限授予下游角色。

| 编号 | 规则（ADR-BR-004 方案 B） |
|---|---|
| GR1 | 守卫契约由上游在 api 中声明（函数名、参数与返回字段），并在自己的 schema 中发布版本化的 SECURITY DEFINER 守卫函数，函数内以 SELECT … FOR SHARE 锁定守卫行，SET search_path = pg_catalog，只向应用角色授予 EXECUTE |
| GR2 | 下游在自身事务中、以自身 executor 调用守卫函数；不经上游的 DB owner（否则返回 transactionConflict），行锁归下游事务所有，直到其提交 |
| GR3 | 守卫读取只用于拒绝或放行本上下文的写入，不得 JOIN 下游表，不得写入他域 |
| GR4 | 上游撤销以 UPDATE 修改守卫行并递增 eligibility_epoch；下游把观察到的 epoch 写入提交证据；撤销等待持有守卫的下游事务提交后，以事件触发下游的风险登记 |
| GR5 | 每个守卫函数对应 03 阶段关系表中 guardLock=true 的一条关系，并在 dependency-check 与 migration-check（S2 交付）中登记；未登记的跨 schema 读取视为违规 |
| GR6 | 若裁决为方案 A，GR 规则作废：改为事务外预取、提交证据记录观察到的 epoch、上游事件触发风险暂停 |
| GR7 | 守卫锁边（下游以 FOR SHARE 锁上游守卫行）必须构成有向无环图，由主档案构建器校验；导入白名单中的双向关系（如 TRD↔TSK、FUL↔STL）至多一个方向使用守卫锁，另一方向只经事件或事务外查询 |

```go
// internal/bootstrap/modules.go —— S1 创建；示意
pool := must(pgxpool.New(ctx, cfg.PostgresDSN)) // 一个连接池

evdDB := must(bentopg.NewDB(pool)) // 每个上下文一个 DB：连接共享，事务 owner 隔离
ptyDB := must(bentopg.NewDB(pool))
catDB := must(bentopg.NewDB(pool))
polDB := must(bentopg.NewDB(pool))
inqDB := must(bentopg.NewDB(pool))

evdModule := must(evd.New(evdDB, authz.EVD()))
ptyModule := must(pty.New(ptyDB, authz.PTY(), evdModule))
catModule := must(cat.New(catDB, authz.CAT(), evdModule))
polModule := must(pol.New(polDB, authz.POL()))
inqModule := must(inq.New(inqDB, authz.INQ(), ptyModule, catModule, evdModule)) // 只作为上游查询端口传入
```

## 10 业务流程的归属 {#s10}

| 流程 | 本基线归属 | 说明 |
|---|---|---|
| Q01 询价到订单 | TSK（业务任务）；决策在 INQ、CST、TRD | 任务只记录步骤、等待与核验，不含业务不变量 |
| 订单到采购草稿 | PUR | 采购编制人显式选择供应条件 |
| 来源否认的风险传播 | CST 发布事实；TRD、PUR 各自守卫 | 方式取决于 ADR-BR-004 |
| 正式化与发送 | TRD 正式化；发送在 internal/ai＋BIP（未交付） | 发送需独立授权与 operation_id |
| 中国出口资料链 | CNX | B0 前详细设计 |

processes/<name> 的准入条件：具名业务 Owner 并写入 CODEOWNERS；状态存放在 brant_proc；只包含顺序、超时、重试与补偿协调，不含业务不变量；只通过各上下文 api 交互；先在 Context Map 登记关系再建目录。

## 11 共享内核与参考数据 {#s11}

| 要共享的东西 | 放在哪里 |
|---|---|
| 只在一个上下文内共享（如 TRD 四个聚合共用的 QuoteID 与错误） | 该上下文的 domain 根包 |
| 跨上下文、语义完全一致的值对象：Money、Quantity、Ratio、ExchangeRate、ValidityInterval、Scope、ActorRef | internal/kernel |
| 外部机构发布、需要版本与生效日期的代码表：ISO 币种与国家、UN/LOCODE、HS 编码、筛查名单（S1 已建） | internal/reference（brant_ref，归属待决定） |
| 某上下文拥有、其他上下文引用的业务事实：PartyRef、SkuRef、ConfigRef、CostSnapshotRef | 拥有方的 api/，不进 kernel |

kernel 只依赖标准库（或经 ADR-BR-REPO-10 批准的十进制库）；CODEOWNERS 要求首席架构师与受影响的上下文 Owner 共同评审。租户与公司身份的权威是 IAM 与 POL，kernel 中的 Scope 只是值类型，总是从已验证令牌中取得。

## 12 18 个限界上下文的包、schema 与阶段 {#s12}

包名使用上下文代码，与 schema brant_<bc>、事件来源 idp-brant.<bc>、迁移目录 migrations/<bc> 保持一致；每个上下文包的文档注释写出全称，导入时使用 <bc>api 别名。

{{rs_packages}}

## 13 AI-Native 能力层 {#s13}

| 包（internal/ai/…） | 职责 | 不负责 |
|---|---|---|
| capability | 把上下文命令登记为受控能力：版本、输入 Schema、风险等级 | 定义业务规则 |
| gateway | 能力调用入口、Schema 校验、任务关联 | 鉴别用户身份 |
| policy | 委托范围、自治级别与能力许可 | 业务授权与领域守卫 |
| approvalgrant | 校验审批凭证绑定的租户、公司、命令摘要、资源版本与到期时间 | 报价、订单与采购承诺审批 |
| execution | 调用 api 命令并跟踪 ACCEPTED、COMMITTED、VERIFIED | 直接访问仓储或数据库 |
| executionevidence | 与 EER 交换执行证据引用与哈希 | 业务证据 |
| adapters/{eer, bip, cmi, mcp} | 接入既有平台 | 授予任何业务权限 |

三层授权互不替代：L1 身份（foundation/identity，只取自已验证令牌）；L2 AI 委托（ai/policy、ai/approvalgrant）；L3 业务授权（各上下文 Authorizer 端口＋领域守卫，含人工专属命令）。

## 14 数据库与迁移 {#s14}

一个 PostgreSQL 16 实例起步，每个上下文一个 schema。同一实例不等于安全隔离：租户 RLS、数据库角色、业务授权与负向测试缺一不可。规则 DB1—DB9 见 06 阶段第 06 章；迁移顺序由 migrations/manifest 声明，第二个上下文出现时（S1）设计。

| 角色 | 权限 |
|---|---|
| brant_migrator | 执行 bento 与 brant_<bc> 迁移；拥有全部 Brant schema 与 DDL；不是超级用户（M0 已建） |
| brant_app | 无 owner、superuser、BYPASSRLS 与 DDL；每个事务设置 app.tenant_id 与 app.company_id（M0 已建，DDL 负向测试通过） |
| 按上下文角色（可选，S2 评估） | 在各自事务器中 SET LOCAL ROLE，使“只写本上下文 schema”由数据库强制；守卫函数为 SECURITY DEFINER，拆分角色后仍可用 |

M0 已建的非上下文 schema：bento（Outbox 与 Inbox）、brant_auth（foundation/authorization 的守卫行与授权投影，ADR-BR-IMP-002）、brant_migrations（迁移台账，应用角色不可访问）。角色与授权脚本在仓库的 deploy/postgres。

## 15 契约与发布范围 {#s15}

上下文之间的进程内契约以 api/ 的 Go 类型为准；进程外契约以 contracts/ 为准，Go 类型由 codegen 生成或由 contract-check 校验（二者择一，首个外部契约出现时写入 ADR）。事件类型名带主版本后缀，来源为 idp-brant.<bc>。

> [!decision] docs/ 与发布范围（ADR-BR-REPO-08）
> 负责人选择把主档案放在仓库 docs/ 中，方便设计与代码同步演进。Go 构建不引用 docs/；若仓库公开，发布时排除 docs/ 或保持私有，contracts/ 随首个外部消费者进入仓库并受 CI 约束。

## 16 测试与质量门禁 {#s16}

| 编号 | 可证伪的结构验收断言 | 证据 | 阶段 |
|---|---|---|---|
| AC1 | 上下文外导入 internal/contexts/<bc>/internal/… 编译失败 | 边界探针日志 | M0（负向实测已验证）／S1 |
| AC2 | 所有跨上下文导入都在白名单内 | dependency-check 输出 | 检查 M0 已可失败；S1 起生效 |
| AC3 | 每个 api 包只导入标准库与 kernel | dependency-check 输出 | 检查 M0 已可失败；S1 起生效 |
| AC4 | 两个上下文、两个 DB 时，跨上下文嵌套事务返回 transactionConflict | 集成测试 | M0（已实测） |
| AC5 | 迁移中不存在跨 schema 外键（守卫函数例外须登记） | migration-check | S2 |
| AC6 | 根目录 tests/ 不导入任何 internal 包 | dependency-check（同一 Module 内编译器不拦截） | 检查 M0 已实施；S4 起有 tests/ |
| AC7 | CODEOWNERS 覆盖每个上下文目录与 kernel | dependency-check | M0 已实施；S1 起覆盖上下文 |
| AC8 | 已发布事件的载荷与契约一致 | 契约测试 | S1 |
| AC9 | 人工专属命令拒绝 AI 身份 | 应用层测试 | 授权策略 M0 已实测；S1 起逐命令登记 |
| AC10 | docs/ 不含 Go 代码；go list ./... 不包含 docs | dependency-check | M0（已实施） |

## 17 首个纵向切片 {#s17}

S1 交付 EVD、PTY、CAT、POL、INQ，与 Python BC-T01 和共享基础 V0.2 行为等价。它只依赖已登记的关系 R01（PTY→INQ）、R02（CAT→INQ）、R03（EVD→INQ）；POL 没有上游。

| 步骤 | 上下文 | 命令与事实 | 契约 |
|---|---|---|---|
| ① | EVD | source.register → SourceRegistered | SourceRef v1 |
| ② | PTY | party.confirm、contact.confirm → PartyConfirmed、ContactConfirmed | PartyRef v1 |
| ③ | CAT | sku.confirm ×4 → SkuConfirmed | SkuRef v1 |
| ④ | POL | config.publish（币种、单位、换算、付款条件等）→ ConfigPublished | ConfigRef v1 |
| ⑤ | INQ | rfq.save_draft → rfq.confirm_scope → RfqScopeConfirmed | RfqRef v1 |

S1 创建的目录：internal/contexts/{evd, pty, cat, pol, inq}、internal/kernel、internal/bootstrap、internal/transport/http、internal/foundation/{bento, database, identity, authorization, configuration, telemetry}、cmd/{brant-api, brant-cli, brant-migrate}、migrations/{manifest, evd, pty, cat, pol, inq}、tools/dependency-check、.github/CODEOWNERS。

## 18 现状与演进路线 {#s18}

| 事项 | 状态（2026-11-07） |
|---|---|
| idp-brantgo 仓库目录 | 根目录为 Go Module；docs/ 为本主档案；brand/ 为品牌资料（不属于 Go 模块）；公开远程仓库 idp-xyz/idp-brantgo（2026-11-11 由个人账号 idpxyz 转入组织 idp-xyz） |
| go.mod、go.sum | 已建立：go 1.26.0、toolchain go1.26.5；idp-bento-go v0.1.0-rc.2；go.sum 与 Cargo 逐字节一致 |
| cmd/ | brant-migrate、brant-worker 已实现；brant-api、brant-cli 随 S1 |
| internal/ | kernel、bootstrap、foundation/{bento, database, identity, authorization, messaging} 已实现；尚无 contexts/ |
| migrations/、deploy/、scripts/ | manifest＋auth；PG16 角色与授权脚本；开发库启动脚本 |
| tools/ | dependency-check（白名单由 Context Map V0.1 生成）；spikes/consistency |
| CI | .github/workflows/ci.yml 已定义；本机等价 make ci 通过；远程运行与主干保护待仓库创建 |
| PostgreSQL 16 | 专用开发容器 brant-pg16（16.12，与 CI 同一 digest） |
| ADR-BR-004、ADR-BR-011、ADR-BR-REPO-10 | M0-10、M0-03 已实测并给出建议决定，待签核 |

| 阶段 | 新增目录 | 退出条件 |
|---|---|---|
| M0 | go.mod、kernel、foundation/{bento,database,identity,authorization,messaging}、brant-migrate、brant-worker、tools/dependency-check、CI | AC1、AC4、AC10（本机已满足）；阻断 ADR 已裁决（待签核） |
| S1 | evd、pty、cat、pol、inq；transport/http；bootstrap；brant-api | AC2、AC3、AC7、AC8、AC9；BRC-001 前段在 PG16 可重放 |
| S2 | sup、cst；资格守卫 | T02 等价；SC-04 依裁决通过 |
| S3 | trd、pur | V0.50 等价报告；BC-AT01 人工链重放 |
| S4 | tsk、ai、tests、Inbox | P0 退出前提（G2） |
| S5 | qlt、ful、stl、cnx、foundation/interop | G0、G3、G4 |
| S6 | stk、mfg、clm、crm、reference | G5 |

## 19 不推荐的方案 {#s19}

| 方案 | 问题 | 替代做法 |
|---|---|---|
| models/ services/ repositories/ 按技术层全局分类 | 领域边界消失 | 按上下文组织（P2） |
| 按 Python 切片建包（t03/、t04/） | 交付节奏不是领域边界 | 按上下文与聚合建包 |
| 每个上下文一个 Module 或微服务 | 版本协调、分布式事务与运维成本过早增加 | 单一 Module＋internal（P1、P3） |
| shared/models/ 或 common/ | 跨领域共享模型，无 Owner 的业务逻辑集中地 | 小而受治理的 kernel；决策留在拥有它的上下文 |
| 所有上下文共用一个 bentopg.DB | 跨上下文调用静默并入同一事务 | 每上下文独立 owner（T1） |
| 未登记的跨 schema 读取或 JOIN | 数据所有权失效 | ID＋快照＋投影；守卫读取须登记（GR5） |
| 先为 18 个上下文建空脚手架 | 形式复杂、没有业务验证 | 随真实代码创建 |
| AI 直接调用仓储，或把 ApprovalGrant 当业务审批 | 绕过授权与领域守卫 | 只调用 api 命令（P8） |

## 20 决策记录与待签核事项 {#s20}

{{adr_register group=仓库,技术基线}}

签核角色：首席架构师（问责）、研发负责人（问责）、各上下文 Domain Owner（各自目录）、DBA（第 14 章）、安全负责人（第 13 章授权分层）、产品负责人（发布范围）。

## 21 Go 技术对齐矩阵 {#s21}

{{go_alignment}}
