---
output: 06_ENGINEERING_CONTRACTS/01_IDP_Brant_Engineering_Contracts_Go_V0.1.html
title: IDP Brant 工程契约 · Go V0.1
short: 工程契约 / Go V0.1
kicker: IDP BRANT / 06 ENGINEERING CONTRACTS
doc_title: 工程契约 · Go V0.1
doc_desc: 契约事实源 · 命令与响应约定 · 命令、事件与错误码目录 · PG16 数据所有权 · Go 进程内契约
meta: 版本=V0.1 Candidate; 日期=2026-11-06; 状态=候选 · 无可调用 Go 接口; 门禁=G0 / G1 NO-GO
stage: 06 · 工程契约
eyebrow: ENGINEERING CONTRACTS
search: 搜索命令、事件、错误码或 schema
foot: Engineering Contracts V0.1 · 2026-11-06 · 主档案 V1.0
---
<span class="scope-badge">CONTRACT CANDIDATE / NOT A PUBLISHED API</span>

<h2>每份契约只有一个事实源，并由 CI 检查兼容性</h2>

<p class="lede">本稿把战术模型转为工程契约候选：{{=commands}} 个写命令、{{=events}} 种集成事件、{{=errorCodes}} 个错误码、{{=contexts}} 个 PostgreSQL schema 的归属，以及 Go 进程内契约的组织方式。Python V0.50 的候选 JSON Schema 与参考 SQL 原件逐字节收录，作为设计输入而非已发布契约。</p>

::: cover
- 进程内契约 | 上下文之间以各自 api/ 包的 Go 类型为准；api 是只依赖标准库与 kernel 的叶子包。
- 进程外契约 | HTTP、事件与跨产品接口以 contracts/ 中的 OpenAPI 3.1、AsyncAPI、JSON Schema 为准。
- 目录 | 命令、事件、错误码与 schema 归属的 CSV 在 02_CONTRACT_CATALOG，由构建器从战术模型生成。
:::

::: notice 状态声明
本页的路由、类型与字段均为候选，不是可调用接口。Go 代码目前只有 M0 工程基线（kernel、foundation、迁移与中继），没有业务上下文与业务接口；发布任何写接口之前必须具备封闭 schema、角色映射、状态转换与确定错误测试。
:::

## 01 契约原则与事实源 {#s1}

| 编号 | 规则 | 级别 |
|---|---|---|
| C1 | 上下文之间的进程内契约以 api/ 的 Go 类型为准：命令、回执、查询、引用类型、事件名与载荷、错误类别 | 必须 |
| C2 | 进程外契约以 contracts/ 中的 OpenAPI、AsyncAPI、JSON Schema 为准；对应 Go 类型或由 tools/codegen 生成，或由 tools/contract-check 校验，二者择一并写入 ADR | 必须 |
| C3 | 事件类型名带主版本后缀，如 idp-brant.trd.quote_activated.v1；破坏性变更发布新主版本并并行一段时间 | 必须 |
| C4 | 事件来源统一为 idp-brant.<bc>；载荷含标识、版本与战术模型 eventPayloads 登记的业务值（数量、金额、期限、结论），不含法定标识、个人数据与文件内容（S5R-D43） | 必须 |
| C5 | 入站协议只依据 api 错误类别映射状态码，不依据内部错误原因 | 必须 |
| C6 | 行业标准与中国专业系统报文先经 foundation/interop 编解码，再由上下文 adapters/external 转为本上下文模型 | 必须 |
| C7 | 封闭结构：拒绝额外字段、重复 JSON 键、浮点数、科学计数法、NaN／Infinity 与无效 UTF-8；业务小数一律用字符串 | 必须 |
| C8 | 旧 q01-contracts 0.1.0 的 23 个定义与 Python 候选契约保持原义，新契约通过版本投影兼容，不放宽旧 schema | 必须 |

## 02 命令请求与响应约定 {#s2}

所有写命令采用统一请求形状；租户、公司与操作者由服务端从已验证令牌注入，请求体不得自报。新建对象 expected_version 为 0，更新严格 CAS；草稿保存是整稿替换，不是 PATCH。

```json
{
  "contract": "brant.quotation/0.1.0",
  "command": "quote.activate",
  "idempotency_key": "req-2026-11-06-0001",
  "args": {
    "quote_id": "quote.1",
    "business_revision": 1,
    "business_hash": "sha256:…",
    "expected_root_version": 6
  }
}
```

成功与错误是两个封闭分支，不以任意 result 字段容纳未知输出：

```json
{"ok": true, "action_id": "act-…", "object_id": "quote.1", "root_version": 7,
 "business_revision": 1, "state": "active", "replayed": false, "result_hash": "sha256:…"}
{"ok": false, "code": "APPROVAL_REVOKED", "category": "业务拒绝", "message": "…",
 "field_errors": [], "retryable": false, "correlation_id": "corr-…"}
```

准入顺序沿用 Python 实现：契约与结构 → 当前身份与范围 → 已成功幂等结果及当前读取授权 → 根 CAS 与状态 → 引用与业务完整性 → 金额与规则 → 动作审批与提交。授权前不读取隐藏依赖正文；无权或不存在按防泄露规则返回通用错误。

| 资源限制 | 值（沿用 Python T02／T03） |
|---|---|
| 原始请求 | 1 MiB |
| JSON 嵌套 | 64 层；100,000 个值 |
| 直接引用 | 1,000；展开节点 5,000；依赖深度 8 |
| 询价／报价行 | 200；销售收费 100；付款阶段 20 |
| 主正式文档 | 1 MiB UTF-8 |
| 金额 | 每项及总额最多 24 位最小单位整数；整数部分最多 12 位 |

## 03 命令目录 {#s3}

{{ec_kpis}}

“执行身份”：05 阶段 humanOnlyCommands 清单中的命令为“仅人工”（唯一来源，构建器校验状态机与之一致）；其余由状态机转换的执行者推导（没有状态机的命令可在战术模型 commandActors 中写明，如 order_account.open 为人工或系统，S5R-D02），未出现在状态机中的命令为“人工或 AI（按动作权限）”；结汇是 fund.record 的 kind=fx_conversion，没有单独命令；“Python 对应”列给出 V0.50 中的命令或本地接口名，未实现的标为 Python 未实现。候选路由形如 POST /api/brant/v1/<bc>/commands/<command>，最终路由在 OpenAPI 发布时确定。

{{ec_commands}}

## 04 集成事件目录 {#s4}

事件表达已经提交的事实，不是对其他上下文的远程更新。“登记的载荷字段”取自战术模型各聚合的 eventPayloads（S5R-D43）：S5 的上下文按代码登记，新增事件的载荷为设计，其余上下文由 S4 E2 补登。投递语义为至少一次；消费方按事件 ID 去重，不宣称 exactly-once。信封的 partitionKey 为 aggregateType:aggregateId，同一聚合的事件按 aggregateVersion 顺序投递。预算耗尽的消息进入死信并告警，由授权运维按事件 ID 重放；撤销、否认与风险类事件不得静默放弃（ADR-BR-015）。

{{ec_events}}

## 05 错误码目录 {#s5}

错误码从 Python V0.50 源码中的常量提取并按类别整理。HTTP 状态只是传输分类：结构或业务校验失败 400／422，未认证 401，授权拒绝 403，作用域内不存在 404，并发、幂等与名额冲突 409，暂时不可用 503。业务处理以错误码为准。

{{ec_errors}}

## 06 PostgreSQL 16 数据所有权 {#s6}

| 编号 | 规则 | 级别 |
|---|---|---|
| DB1 | schema 命名为 brant_<bc>；流程状态用 brant_proc，外部代码表用 brant_ref，框架表在 bento | 必须（ADR-BR-REPO-07） |
| DB2 | 上下文只经自己的仓储写自己的 schema；禁止跨 schema 外键与跨上下文写 SQL | 必须 |
| DB3 | 跨上下文引用使用稳定 ID、版本化快照与本地投影；投影表属于消费方 schema | 必须 |
| DB4 | 应用角色 brant_app 没有 owner、superuser、BYPASSRLS 与 DDL 权限；迁移由 brant_migrator 执行 | 必须 |
| DB5 | 每个事务经租户事务器设置 app.tenant_id 与 app.company_id；业务表 ENABLE 并 FORCE ROW LEVEL SECURITY | 必须 |
| DB6 | 资格守卫读取若获批准（ADR-BR-004 方案 B），只能调用上游发布的版本化守卫函数，并在 03 阶段关系表标记守卫锁、在 dependency-check 与 migration-check 中登记；守卫锁边必须无环 | 模拟评审接受（ADR-BR-004） |
| DB7 | Bento 技术迁移由 idp-bento-go 发布；brant-migrate 先建 bento schema，再按批准顺序执行框架与 Brant 迁移并记录校验值 | 必须 |
| DB8 | 迁移文件一经执行不得修改；迁移 ID 取自文件名，校验值取自内容 | 必须 |
| DB9 | 封存行（商业修订、批准、正式文档、提交证据、采购草稿）由数据库触发器或权限禁止更新与删除，服务层同时守卫 | 必须 |

{{ec_schemas}}

## 07 查询与读取契约 {#s7}

查询同时区分持久状态、当前资格、检查时点与原因。历史可读与当前可用分开表达：历史报价可读不代表当前 quote_ready；订单 active 不代表后续资格仍满足。

| 查询 | 返回边界 | Python 来源 |
|---|---|---|
| quote_ready | 当前根、报价与批准时效、风险、正式文件完整性、成本与配置资格，带 as_of | T03 A8、A34 |
| 报价／审批／文档历史 | 封存内容与效力事件；读取时重查当前权限与传递依赖权限 | A9—A11 |
| Outbox 状态与提交证据 | 内部正式化事实与证据；不表示外部送达 | A12—A13 |
| 发送前准入与投递目标 | 正式文档、投递目标快照与渠道配置的检查结论 | A14—A16 |
| 客户回复与确认 | 回复分类、来源定位、核实摘要或 null | A45 |
| 订单审批 | 请求、裁决、撤销与经审核激活记录 | A42 |
| 动作对账 | committed、payload_conflict 或 not_committed；不重新执行 | A22、A23、A43 |

读取、重放与对账都必须重查当前调用方权限与对象范围；来源权限撤回后，查询、同键写重放与对账均不能返回成功事实。

读取目录（S5R-D43）：每个上下文与平台提供方的读取登记在 Context Map 的 queries 中，名称取 OpenAPI 候选的 operationId，权限为 <bc>.read，全部审计；POL 的 fx.convert、unit.convert、tax.preview 是进程内查询。构建器校验目录与 contracts/openapi 的 GET 路由一一对应（通用审计路由 /api/brant/v1/{bc}/audit 与 healthz 除外），并生成 02_CONTRACT_CATALOG/06_Query_Catalog_V0.1.csv。

{{ec_queries}}

## 08 Go 进程内契约示意 {#s8}

以下是 TRD 上下文 api 包的候选形态，用于说明 C1 规则：api 只暴露命令、回执、引用与错误类别，不以别名、嵌入或返回值暴露内部领域类型。

```go
// internal/contexts/trd/api/commands.go —— 候选示意，尚未实现
package api

import "context"

// Source 是 TRD 发布事件的来源标识。
const Source = "idp-brant.trd"

type ActivateQuote struct {
	CommandID           string
	QuoteID             string
	BusinessRevision    int
	BusinessHash        string
	ExpectedRootVersion int64
}

type PrepareOrder struct {
	CommandID    string
	QuoteID      string
	AcceptanceID string
	Slot         string // 首批仅 "full_v1"
}

type Receipt struct {
	ActionID    string
	ObjectID    string
	RootVersion int64
	Replayed    bool
}

type Commands interface {
	ActivateQuote(ctx context.Context, cmd ActivateQuote) (Receipt, error)
	PrepareOrder(ctx context.Context, cmd PrepareOrder) (Receipt, error)
}
```

```go
// internal/contexts/trd/api/errors.go —— 候选示意
package api

type Category string

const (
	CategoryInvalid   Category = "invalid"   // 结构或业务校验
	CategoryForbidden Category = "forbidden" // 授权拒绝或不存在（防泄露）
	CategoryConflict  Category = "conflict"  // CAS、幂等、名额、对象控制
	CategoryRejected  Category = "rejected"  // 状态、资格或规则拒绝
)

type Rejection struct {
	Code     string // 例如 ROOT_CONVERSION_LOCKED
	Category Category
	Fields   []FieldError
}
```

## 09 Python 候选契约原件 {#s9}

下列文件是收录自 Python V0.50 的冻结参考副本，来自 brant_core/contracts、brant_core/costing_engine/contracts 与 t03_candidate/contracts。它们包含 brant.trade-core、brant.foundation、brant.costing、brant.quotation 的候选 Schema、映射文件与参考 SQL，是 Go 契约设计的输入；其中多版本映射文件（如 authority-control-mapping v0.12—v0.27）在 Go 中须收敛为单一事实源（C2）。

{{ec_legacy}}

## 10 兼容与版本策略 {#s10}

- 契约版本与产品范围版本、软件包版本、证据版本分别管理；契约主版本变更须列出受影响的上下文与消费者。
- 新增可选字段为次版本；删除字段、改变语义、收紧枚举为主版本，并保留旧主版本的并行期。
- 旧记录的哈希算法版本保留用于回放；新算法不重算历史请求。
- 商业哈希的规范化须有黄金向量（字段排序、数值字面差异、数组语义、中文与换行、负例），并通过 Go 与 Python 的互操作验证后才发布。
- 契约检查进入 CI：provider 与 consumer 双向兼容测试、负向载荷拒绝、事件重放。
