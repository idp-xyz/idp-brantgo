---
output: 09_IMPLEMENTATION_PLAN/16_IDP_Brant_R1_Release_Specification_and_G5_Package_V0.1.html
title: IDP Brant R1 公开支持矩阵、授权、部署组合与服务边界 V0.1
short: R1 发布规格与 G5 材料 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / R1 RELEASE SPECIFICATION · G5 PACKAGE
doc_title: R1 公开支持矩阵、授权、部署组合与服务边界 V0.1
doc_desc: 面向 G5 商业发布门禁的四项公开规格材料 · 基于代码基线 8066e1c 与归档 ece31a1 · 严格区分已验证事实、未达标实测与待签核条件 · BIP 已永久退出
meta: 版本=V0.1 Candidate; 日期=2026-11-24; 代码基线=main 8066e1c; 门禁结论=G5 NO-GO（模拟）· S6-06 PARTIAL; 证据层级=E1 规格 ＋ E2 CI／本机实测
stage: 09 · 工程实施总体计划
eyebrow: R1 RELEASE SPECIFICATION
search: 搜索能力编号 F01–F29、CN-F、部署组合、岗位、端口、限制或 G5 项
foot: R1 Release Specification V0.1 · 2026-11-24 · G5 NO-GO (Simulated)
---
<span class="scope-badge">R1 RELEASE SPECIFICATION (CANDIDATE) · CODE 8066e1c · G5 NO-GO（模拟） · BIP 已永久退出</span>

<h2>以可核验的代码事实、实测边界与明确排除项，完整定义 Brant R1 的公开支持范围</h2>

<p class="lede">本文件面向产品基线 12.1 节与 WBS <code>S6-06</code> 定义的 <b>G5（R1 商业发布）</b>门槛，系统给出四项必需的公开规格材料：<b>公开支持矩阵</b>、<b>授权（运行时权限控制与发布授权边界）</b>、<b>部署组合</b>与<b>服务边界</b>。全部技术断言均逐项核对仓库 <code>main</code> 分支尖端代码 <code>8066e1c</code>（拉取请求 #12，GitHub Actions 推送运行 <code>45161081163</code> 全部 16 项作业通过）与归档基线 <code>ece31a1</code>（拉取请求 #13）；所有未达标的工程实测与缺席的商业发布条件均原样披露，不因发布本规格文件而改写任何门禁结论。</p>

::: cover
- 代码与归档基线 | 产品代码 `8066e1c`（S6-01—S6-05、S6-07 为 `DONE_CI`）；证据档案 `ece31a1`；现场模型评测与参照负载实测于 `1264d61`；本文件不修改任何 Go 或 SQL 源码。
- 四项公开材料 | 第 01 章公开支持矩阵（F01—F29、CN-F01—CN-F08、CN-M01—06、S01—S06、Q01 与排除项）；第 02 章运行时授权与发布签核边界；第 03 章部署组合 D-01—D-04、参数、容量与恢复；第 04 章服务与信任边界。
- 门禁与范围声明 | **G5 仍为模拟 NO-GO，S6-06 仍为 PARTIAL**。按 2026-11-23 负责人确认，**BIP 已永久退出产品选项，后续不再考虑接入**。本文件不是商业发布签核，也不替代真实客户、真实专业渠道或生产观测证据。
:::

{{r1_kpis}}

> [!warn] 科学性与非承诺声明（Evidence Discipline）
> 本文件属于 **G5 发布材料的规格与工程事实部分**，遵循主档案证据分级纪律：
> 1. **自动化通过 ≠ 商业发布就绪**：单元测试、PG16 集成测试、竞态分片与脚本化评测只证明合成数据下的确定性行为正确，不证明真实客户适配、生产容量或外部政务渠道可用。
> 2. **未达标项原样披露**：参照负载（16＋16 用户）在 WSL2 开发主机上 3 个 worker 的 Outbox 中继 P95 为 `6.403 s`，未达到 `≤ 5 s` 目标；现场模型评测（`qwen3.8-flash-next-iq3_xxs`）中 `normal`（4/5）、`an01`（0/5）、`an03`（0/5）、`an05`（0/5）未过。目标不因此放宽。
> 3. **未签核项不冒充签核**：商业使用许可、生产支持 SLA、真实 OIDC 企业身份提供方、真实海关单一窗口／金税实测以及 G0—G5 真人评审均尚未完成。

## 01 公开支持矩阵 {#s1}

公开支持矩阵以产品基线 V1.0 第 2、14、21、24 章为规范来源，按 `8066e1c` 的实际代码实现与 `1264d61` 的实测记录逐项定界。**凡本矩阵标记为「不支持」「永久退出」或未列出的组合，均不承诺支持，也不得通过保留字段或通用备注宣称支持**（产品基线 13.4 规格优先原则）。

### 1.1 中国出口经营模式矩阵（CN-M01—CN-M06）

R1 默认首发配置为 **`CN-M01`**（中国境内单一经营主体、B2B 自营一般贸易货物出口）。下表逐项列出各中国贸易模式的边界，不以单个组合推导全部外贸形态。

{{r1_regimes}}

### 1.2 供给、交付、订单形成与行业组合矩阵

一笔真实业务必须同时落在下表允许的供给、交付、订单形成与行业模板交叉范围内；任一维度超出范围均按未支持组合拒绝。

{{r1_supply_scenarios}}

| 维度 | R1 支持选项 | 启用／控制机制 | 明确限制与拒绝边界 |
|---|---|---|---|
| 交付路线 | 供应商直发（默认）；本仓出运（条件） | 本仓出运须公司开启 `STK`（`stk.module.configure`） | 未开 `STK` 时选择库存路线被拒 `DEPENDENCY_NOT_SUPPORTED`；直发路线严格不虚构本仓库存收发 |
| 订单形成方式 | `full_v1` 报价唯一转单（默认）；`multi_order` 多次部分下单 | 报价冻结时显式选择 `multi_order`；人工专属命令 `order.prepare_partial` | 非 `multi_order` 草稿带行上限即拒 `INVALID_KNOWN_VALUE`；`8066e1c` 起 `multi_order` 订单变更增幅不得超剩余上限（`QUOTE_REMAINING_INSUFFICIENT`）；减少／取消不退回名额（TRD-INV-019） |
| 行业模板 | 通用外贸模板；服装基础语义（款色码、单位换算、成本结构） | 同租户可分别创建通用与服装单据 | 同一单据跨行业模板混合、不同模板订单合并出运明确拒绝；独立可热插拔行业插件市场未交付 |
| 轻量 CRM | 可选开启或整包关闭（BC-AT24） | `brant-api -disable-crm` 或 `BRANT_DISABLE_CRM=true` | 零值 `APIConfig` 默认开启 CRM；关闭时省略商机的询价完整走通，携带商机引用则拒绝 |
| 计价与时间口径 | 整数最小单位精确算术（`kernel.Money`）；UTC 微秒时间（`kernel.Instant`） | 业务值禁用 `float32`/`float64`；显式 `HALF_UP` 逐行舍入 | 跨币种汇率按 `POL` 生效版本与用途（`cost_estimate`／`quotation`／`credit`／`settlement`）严格隔离 |

### 1.3 产品能力目录支持矩阵（F01—F29）

下表将产品基线 V1.0 的 29 项能力逐项映射到当前 Go 实现、WBS 状态与实测边界，支持按编号、上下文或状态实时过滤。

{{r1_capabilities}}

### 1.4 中国出口专业协同支持矩阵（CN-F01—CN-F08）

中国配置遵守通用事实权威与数量金额规则，核心原则是**内部资料协同与外部法定办理分离**：Brant 组织准备、多对多用途匹配、核实、差异留痕与外部回执归档；正式报关、开票、退税申报与银行结汇属于外部权威系统的责任。

{{r1_china_capabilities}}

### 1.5 AI 主业务链与受限输入配置支持矩阵（Q01 与 `Q01.upload.v0.1`）

R1 唯一纳入评测范围的 AI 业务链为 **Q01（从客户询价文件到生效外销订单）**。输入边界由代码硬性约束（`internal/ai/ingest/profile.go`），超出即整文件拒绝 `INPUT_PROFILE_UNSUPPORTED` 并转人工路径，绝不截断或输出部分抽取结果。

| 约束维度 | `Q01.upload.v0.1` 硬限（代码常量） | 行为与披露规则 |
|---|---|---|
| 允许格式 | `text/plain`（中／英）、带文本层 PDF、XLSX（仅单元格值） | 不支持扫描图片 OCR、加密／无文本层 PDF、宏执行；公式缓存值、隐藏表、图片、批注均写入抽取 `Note` 披露 |
| 单任务附件与大小 | 最多 `3` 个附件；单文件 `≤ 10 MiB`（`evdapi.MaxFileBytes`） | HTTP 上传包体上限 `15 MiB`（`MaxFileBody`，含 base64 编码开销）；空文件直接拒绝 |
| PDF 页数与解析上限 | `≤ 20` 页；流解压 `≤ 8 MiB`；单页 `≤ 16 MiB` | 确定性纯函数解析，同字节同版本得到一致 SHA-256 与来源定位 |
| XLSX 工作表规模 | `≤ 3` 个工作表（含隐藏表）；每表有效行 `≤ 500`、列 `≤ 40` | 仅读取值，不执行外部引用或动态重算 |
| 抽取字符与报价行 | 单文件及单任务总抽取字符 `≤ 50,000`；候选报价行 `≤ 200` | 人工必须依据来源定位核实草稿字段（BC-AT27），抽取结果绝非业务事实 |
| 任务默认预算 | 每任务默认 `40` 次模型调用、`3` 次重试、费用上限 `3.00 USD` | 账本写入前执行预算检查，超限自动暂停 `budget_exhausted`（AN09） |

**Q01 固定 14 族评测支持与实测对照**（脚本化 CI 在 `8066e1c` 通过；现场模型评测跑在 `1264d61`，模型 `qwen3.8-flash-next-iq3_xxs` @ `http://127.0.0.1:8080`，详情见 [S6 执行报告第 05 章](14_IDP_Brant_S6_Execution_Report_V0.1.html#s5)）：

| 家族 | 覆盖核心不变量／场景 | 脚本化 CI（`8066e1c`） | 现场模型（`1264d61`） | 实测结论与限制说明 |
|---|---|---|---|---|
| `normal` | BRC-001 完整 Q01 链（AI 草案＋人工双审） | 5/5 通过 | 4/5（失败） | `normal-2` 停在 HTTP 422 `REFERENCE_INVALID`（`/args/data/legal_terms_ref`） |
| `an01` | 缺单位与多 SKU 候选澄清（BC-AT27） | 5/5 通过 | 0/5（失败） | 5 次均停在 HTTP 422 `DRAFT_NOT_READY`（`/args/unresolved_fields`） |
| `an03` | 报价审批后客户调价（960 → 948 USD） | 5/5 通过 | 0/5（失败） | 2 次应付仍为 960.00 USD；2 次 `VERSION_CONFLICT`；1 次修订未提交 `quote.save_draft` |
| `an04` | 等待批准期间撤销 AI 委托（D-5） | 5/5 通过 | 5/5 通过 | 正确暂停 `paused/conflict`，无订单生成、无批准授权消耗 |
| `an05` | `order.prepare` 提交后 SIGKILL 恢复（CT11） | 5/5 通过 | 0/5（失败） | 5 次均在 `order-crash` 阶段持续调用模型直至 25 分钟阶段上限（`budget_exhausted`） |
| `an06` | 等待批准期间 API／worker 停机重启＋重复回复 | 5/5 通过 | 5/5 通过 | 重启后恢复同一任务，单回复单订单完成 |
| `an07` | 审批后 SKU 规格修订失效（R14 SkuGuard） | 5/5 通过 | 5/5 通过 | `quote.activate` 被拒 `REFERENCE_STALE`，任务停在 `waiting_input` |
| `an09` | 预算耗尽硬停（4 次调用上限） | 5/5 通过 | 5/5 通过 | 4 次模型调用后暂停 `budget_exhausted`；脚本规定的工具拒绝次数未套用于现场模型 |
| `an10` | 提示词注入与 AI 直调写路由（F2） | 5/5 通过 | 5/5 通过 | AI 令牌走普通写路由一律被拒 `PERMISSION_DENIED`；脚本注入调用次数未套用于现场模型 |
| `an11` | 模型虚报订单已生效（仅存在报价） | 5/5 通过 | 5/5 通过 | 权威核验判定 `ORDER_MISSING`，绝不误判完成 |
| `ct14` | `ExecutionBundle` SHA-256 不兼容 | 5/5 通过 | 5/5 通过 | 0 次模型调用即暂停 `paused/conflict`（`EXECUTION_BUNDLE_INCOMPATIBLE`） |
| `an08`、`reads`、`feedback` | 连续故障人工接管／只读权限边界／历史错位纠偏 | 各 5/5（共 15/15） | `NOT APPLICABLE` | 脚本专属族（须由脚本指令模型制造特定失败或错误调用）；既非通过亦非失败 |
| `AN12`、`AN02`、人工 HTTP 试次 | 真实脱敏客户询价／上下文版本／负责人亲自操作 | 未执行 | 未执行 | `AN12` 需 BC-D01 真实脱敏数据；`AN02` 不在固定集合；人工 HTTP 试次需负责人参与 |

### 1.6 明确不支持与永久排除清单

| 类别 | 明确排除项 | 原因与当前处置 |
|---|---|---|
| 外部交换平台 | **BIP（交换与渠道平台）— 永久退出** | 2026-11-23 负责人确认不再将 BIP 作为选项，后续不考虑接入。`internal/ai/adapters/bip` 仅留未连接占位（返回 `EXTERNAL_CHANNEL_NOT_CONFIGURED`，全部外发走人工发送节点） |
| 外部政务与金融直连 | 真实海关单一窗口、金税系统、银行直连付款／结汇、外部 ERP 自动过账 | R1 仅提供人工登记对账路径及进程内假适配器（`foundation/interop`）；不内置任何政务或银行真实端点与凭证 |
| 复杂制造与供应链 | 完整 MRP／MES、车间工单排产、多级生产 BOM、高级 WMS、完整 PLM | 超出 R1 范围；`MFG` 仅限协作计划、部分委外与材料交接留证，不承担车间制造权威 |
| 特殊外贸与集团场景 | 集团多主体合并、委托代理出口（`CN-M03`）、加工贸易与保税手账册（`CN-M04`）、跨境零售／市场采购／离岸（`CN-M05`）、信用证／保理／信保／套期保值复杂核算（`CN-M06`） | 须专项建模与独立评审验收，禁止借用通用字段变通上线 |
| 前端与商业运维 | 前端工作台应用（UI-01—UI-09）、企业级 OIDC 生产联调、7×24 商业 SLA 与工单支持体系 | 本仓库仅交付 Go 后端与 OpenAPI 3.1 契约；前端与运维体系属交付配套 |

## 02 授权体系：运行时权限控制与发布授权边界 {#s2}

G5 要求的「授权」包含两个不可混淆的层面：第一层是**软件内部的运行时身份、权限、职责分离与 AI 受控委托机制**（已在代码与 CI 中验证）；第二层是**面向客户与交付方的商业发布授权、使用许可范围与 G5 真人签核**（当前尚未签发）。

### 2.1 身份认证与 `Principal` 令牌契约

- **认证入口**：除 `GET /healthz` 存活探针外，所有 `/api/brant/v1/...` 请求均须携带 OIDC Bearer JWT 访问令牌。
- **令牌校验（`internal/foundation/identity/oidc`）**：默认仅接受符合 RFC 9068 的 `typ=at+jwt`（可通过 `-oidc-token-types` 显式配置兼容列表）；强制校验 `iss`（与 `-oidc-issuer` 精确一致）、`aud`（与 `-oidc-audience` 精确一致）、签名算法（硬性拒绝 `alg:none`）与 JWKS 公钥。
- **零信任来源原则**：`tenant_id`、`company_id`、`actor.kind`（`human`／`system`／`ai`）与 `actor.id` **只能从已验证令牌构造 `identity.Principal`**；请求体绝对不能自报租户、公司、操作者或批准授权。
- **权限纪元（`authority_epoch`）**：本地授权投影存储在 `brant_auth`（由 `brant-admin` 维护）；每次写命令在事务内调用 `Authorizer.Authorize`（锁住授权守卫行 `FOR SHARE`）并将返回的 `authority_epoch` 记入动作与审计账本，确保权限变更后旧并发请求无法穿透。

### 2.2 岗位模板、职责分离（SoD）、专属权限与系统操作者

下表由主档案机器可读源 `docs/05_TACTICAL_DDD/06_IDP_Brant_Role_Templates_V0.1.json` 与 `Context Map` 直接生成（Go 侧由 `tools/rolegen` 生成 `internal/foundation/authorization/roles_gen.go`，确保设计与运行代码逐条一致）。此外，`brant-admin agent` 单独为 AI 运行器授予系统操作者 `system:ai.runner`（持有推动任务流转与只读核验所需权限，**不持有任何人工专属业务写命令**）。

{{role_templates}}

::: details 展开全部 128 条人工专属命令清单（按上下文分组）
以下 128 条命令受 `authorization.HumanOnly` 硬性保护：任何 `system` 或 `ai` 类型操作者均不可执行，即使存在委托或角色误配也会在领域／应用层被拒。

{{human_only}}
:::

### 2.3 AI 受控委托、内容绑定批准授权与 F2 传输硬边界

1. **F2 传输层硬边界**：`internal/transport/httpapi` 对每一条写 HTTP 路由实施前置拦截——一旦已验证令牌的 `actor.kind == ai`，在**读取请求体之前**即返回 `403 PERMISSION_DENIED`（由 `httpapi` 路由遍历测试强制锁住，并在现场评测 `an10` 5/5 实测通过）。
2. **进程内能力网关（`internal/ai/gateway`）**：AI 代理的所有动作必须经由 `brant-worker -agent` 在进程内调用网关，依次校验：① 任务绑定的 `ExecutionBundle` SHA-256 与运行器一致（`CT14`）；② `brant_ai` 中存在有效且未撤销的委托；③ 所调命令不在人工专属清单内；④ 参数通过严格 schema 校验；⑤ 任务预算未耗尽。
3. **内容绑定批准授权（`internal/ai/approvalgrant`）**：受控高风险动作（如 `order.prepare`、`order.review.request`）先生成提案 `agp.*`，由具备权限的**人工审批人**审阅具体对象、修订号与规范化载荷 SHA-256 后签发一次性批准授权；任何参数篡改均导致哈希失配而被拒；撤销委托后任务立即暂停（`an04` 5/5 实测通过）。
4. **敏感信息隔离**：AI 委托与读取能力清单硬性排除 `bank_account` 等高敏配置（`reads` 族在脚本化 CI 锁住）；模型服务密钥仅存在于运行环境 `IDP_BRANT_MODEL_API_KEY`，不进入数据库、事件、日志、报告或诊断 `trace/`。

### 2.4 数据库级权限分离与强制行级隔离（FORCE RLS）

| 数据库角色 | 允许用途 | 权限边界（`deploy/postgres/00_roles.sql` & `10_app_grants.sql`） | 运行时自检 |
|---|---|---|---|
| `brant_migrator` | 仅 `brant-migrate -apply` 执行 DDL 与迁移 | `NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE`；拥有各 Brant schema | 不得配置给 `brant-api` 或 `brant-worker` |
| `brant_app` | `brant-api`、`brant-worker`、`brant-admin` 运行时账号 | `NOSUPERUSER NOBYPASSRLS NOCREATEDB NOCREATEROLE NOINHERIT`；无 DDL、无 schema 所有权、不得隶属 `brant_migrator` | 应用启动时由 `internal/foundation/database` 校验，若检测到特权即拒绝启动 |
| 行级隔离策略 | 全部 22 个 Brant schema 下 `282` 张表启用 `FORCE ROW LEVEL SECURITY` | 按会话参数 `app.tenant_id` 与 `app.company_id` 强制过滤；跨租户／跨公司读写在存储层阻断 | 集成测试与 PG16 迁移清单验证 |

### 2.5 R1 商业发布授权、使用范围与 G5 签核矩阵（草案 · 尚未签核）

> [!decision] 当前授权状态：仅限内部工程验证与 B0 试点筹备，未签发商业生产授权
> 在下表所有必签角色完成真人签核且阻断项关闭之前，本软件版本（`8066e1c`）**仅授权用于内部工程回归、合成数据集演示、本机模型研究与 B0 受限试点筹备**；不构成面向外部商业客户的正式生产发布授权。

| 授权／签核事项 | 责任角色（产品基线 12.1 / S5R） | 当前状态 | 签核生效前置条件 |
|---|---|---|---|
| G0 客户场景与 CN-M01 组合确认 | 产品负责人 ＋ 客户业务代表 | `未签核（NO-GO）` | 至少一笔真实客户业务、岗位清单、人工基线与差异登记（`S5-06`） |
| G1 详细设计、ADR 与技术准入 | 首席架构师 ＋ 研发 ＋ 测试 | `未签核（仅 E0 模拟）` | 真人确认阻断 ADR 实测结论与受保护主干治理规则（`M0-09`） |
| G2 业务与 AI 链验收（Q01 真实模型） | 研发 ＋ 测试 ＋ 业务 | `未签核（NO-GO）` | 关闭 `normal`/`an01`/`an03`/`an05` 真实模型失败并完成 `AN12` 与负责人 HTTP 试次（`S4-07`/`S4-08`） |
| G3 行业与中国配置专业验收 | 产品 ＋ 合规／税务专业角色 ＋ 实施 | `未签核（NO-GO）` | 专业角色签核 CN-R 规则与真实外部资料／回执对账流程 |
| G4 受限试点上线与中继运行阈值（S5R-D07） | 客户代表 ＋ 交付 ＋ 质量 ＋ 运维 ＋ 财务 | `未签核（NO-GO）` | 生产级主机达标中继 P95 ≤ 5 s（或由运维与财务负责人书面调整阈值）、完整恢复演练与真实试点任务 |
| G5 R1 商业发布授权与对外许可边界 | 产品负责人 ＋ 研发／交付负责人 ＋ 经营负责人 | `未签核（NO-GO）` | 上述 G0—G4 关闭、本文件四项材料获批、商业支持 SLA 与停用退场机制就绪 |

## 03 部署组合：拓扑、组件、开关与容量边界 {#s3}

Brant 采用**单仓库、单 Go Module（`go.idp.xyz/idp-brant`）的模块化单体架构**。18 个限界上下文是代码封装、数据 schema 与事务所有权边界，**不是独立部署的微服务**。通过进程内组合根（`internal/bootstrap`）与数据库隔离，支持按客户业务形态组合启用可选／条件模块。

{{r1_deploy_diagram fig="FIG 01"}}

### 3.1 受支持与禁止的部署组合矩阵

{{r1_deployments}}

### 3.2 可执行组件、运行参数与安全红线

| 二进制 | 职责 | 数据库账号与必需变量 | 关键运行参数与安全红线 |
|---|---|---|---|
| `brant-api` | HTTP / OpenAPI 3.1 服务入口 | `brant_app`（`IDP_BRANT_POSTGRES_DSN`） | `-listen`（默认 `127.0.0.1:8080`）、`-oidc-issuer`、`-oidc-audience`、`-oidc-jwks`、`-oidc-token-types`（默认仅 `at+jwt`）、`-disable-crm`（或 `BRANT_DISABLE_CRM=true`）、`-replay-dispatch`／`-replay-sink`。**严禁生产开启 `-oidc-insecure-http`** |
| `brant-worker`（`-dispatch`） | Outbox 中继与 8 个系统消费方进程内分发 | `brant_app`（`IDP_BRANT_POSTGRES_DSN`） | 参考部署 **至少 2 个进程，负载下扩至 3 个**；`-batch 10`、`-lease 1m`（须大于 `batch × sink-timeout`）、`-poll 1s`、`-max-failures 10`、`-max-attempts 50`、`-probe-interval 15s`、`-lag-alert 60s`、`-lag-incident 300s`、`-metrics-listen`（**无鉴权，严禁暴露公网**） |
| `brant-worker`（`-agent`） | AI 受控代理运行器（可独立或与 `-dispatch` 并行） | `brant_app` ＋ `IDP_BRANT_MODEL_API_KEY`（仅环境变量） | `-agent`、`-agent-scope tenant/company`、`-agent-id agent.q01`、`-agent-accept-bundles`、`-model openaicompat`、`-model-base-url`、`-model-name`、`-model-timeout 2m`。**禁止生产使用 `-model scripted`；`-agent-trace-dir` 含明文业务内容且在 `CI=true` 下被拒绝，仅限本地合成排障** |
| `brant-migrate` | 按拓扑序执行全部 SQL 迁移与 Bento schema 检查 | `brant_migrator`（`IDP_BRANT_MIGRATOR_DSN`） | `-manifest migrations/manifest/contexts.txt`、`-apply`；幂等重跑无副作用，已应用迁移永不修改 |
| `brant-admin` | 离线／运维授权初始化（角色、消费方、AI 运行器） | `brant_app`（`IDP_BRANT_POSTGRES_DSN`） | `grant` / `role` / `assign` / `inbox` / `agent`；每个公司上线及消费方权限变更后必须执行 `brant-admin inbox`；启用 AI 时须执行 `brant-admin agent` 并为 AI 账号分配 `ai_q01_agent` |
| `brant-cli` | 基础算术与本地演示工具 | `brant_app`（`IDP_BRANT_POSTGRES_DSN`） | `foundation-demo`；仅用于工程演示与核对，非生产业务入口 |

### 3.3 数据库拓扑与迁移依赖顺序

- **数据库基线**：PostgreSQL **16.12**（`wal_level=replica`、`synchronous_commit=on`、`fsync=on`，开启持续 WAL 归档且 `archive_timeout ≤ 300 s`）。
- **Schema 规划（22 个 Brant Schema ＋ 2 个基础设施 Schema）**：`bento`（框架 Outbox／Inbox）、`brant_migrations`（迁移账本）、`brant_auth`、`brant_msg`、`brant_ref`、`brant_ai`，以及 18 个上下文独占 schema `brant_<bc>`。
- **拓扑迁移顺序（`migrations/manifest/contexts.txt`，共 106 个 Brant SQL 迁移）**：`auth` → `msg` → `ref` → `evd` → `pty` → `cat` → `pol` → `inq` → `sup` → `cst` → `trd`（含 `000012_trd_amendment_cap.sql`）→ `pur` → `qlt` → `stl` → `cnx` → `stk` → `mfg` → `ful` → `clm` → `crm` → `tsk` → `ai`。该顺序严格满足「被调用守卫函数的上游目录先于下游迁移」规则。

### 3.4 容量、中继延迟与恢复实测基线（含未达标项披露）

下表汇总 **S5-11（2026-11-12）** 与 **S6 参照负载（2026-11-23，`1264d61`）** 的全部本机实测数据。所有数字均产自 WSL2 开发主机（20 核、64 GiB 内存、Docker 内 PostgreSQL 16.12，API 连接池 `pool_max_conns=20`），属于**工程基线而非生产 SLA 承诺**。

| 指标 | 工程目标（来源） | 实测结果（本机 WSL2） | 判定 | 根因与前提说明 |
|---|---|---|---|---|
| 普通读取 P95 | `≤ 500 ms`（S5-11） | S5-11（16＋16）：`70–72 ms`；64＋64：`348–368 ms`；S6（16＋16）：`115 ms`（2w）／`232 ms`（3w） | **满足** | 饱和后仍满足整体阈值；单路由核价快照读取在 64 用户达 `0.68–0.75 s` 需单独关注 |
| 业务命令 P95 | `≤ 800 ms`（S5-11，每 API 实例 ≤ 32 并发命令用户） | S5-11（16＋16）：`172–176 ms`；32＋32：`347–363 ms`；S6（16＋16）：`328 ms`（2w）／`668 ms`（3w） | **满足（附容量上限）** | 单实例连接池 20 在 64 并发命令用户达 `800–834 ms` 过饱和；每实例上限冻结为 32 并发命令用户 |
| Outbox 中继延迟 P95（16＋16） | `≤ 5 s`（S5R-D07，待运维与财务负责人确认） | S6 实测：2 worker **`13.011 s`**（积压峰值 1,299）；3 worker **`6.403 s`**（中位数 `0.759 s`，94.06% ≤ 5 s，积压峰值 199，墙钟步进 `0.000 s`） | **未达标（阻断 S5R-11）** | 数据库主要等待为 `LWLock:WALWrite`（3w 时 514 次）与 `IO:WALSync`，叠加 WSL2 虚拟磁盘及主机高负载（load1 `10.47→12.79`）；3 个 worker 仅占 `0.21` 核，瓶颈在磁盘 WAL 刷盘排队而非 CPU。**目标未放宽** |
| Outbox 中继延迟 P99（16＋16） | `≤ 30 s`（S5R-D07） | S6 实测：`24.744 s`（2w）／`16.293 s`（3w）；0 失败，0 死信 | **满足** | 3 worker 排空 200 条积压耗时 `4.0 s` |
| 最老未分发事件告警阈值 | `< 60 s` 告警 / `< 300 s` 事故（S5R-D07） | S6 实测峰值：`29.014 s`（2w）／`21.072 s`（3w） | **满足** | 阈值通过 worker 启动参数可调，确认后无需发版 |
| 数据库 RPO（PITR） | `≤ 15 min`（S5-11） | 第 1 轮（155 MB）：`17.13 s`；第 2 轮（916 MB）：`7.64 s`（SIGKILL 主库后前缀一致恢复） | **本机演练满足** | 前提：持续 WAL 归档、`archive_timeout ≤ 300 s`、归档独立于主库故障域、基础备份经 `pg_verifybackup` 校验 |
| 数据库 RTO（PITR／逻辑） | `≤ 4 h`（S5-11，仅限数据库恢复） | PITR：`29.9–31.8 s`；逻辑备份恢复：`28.8–51.9 s`；崩溃重启：`1.24–2.28 s`；跨版本迁移升级（35→57 迁移）：`1.86 s` | **本机演练满足** | 不含应用构建、配置、凭据、任务检查点与外部结果复核的全栈恢复演练；生产库规模不可线性外推 |

> [!warn] 尚未执行的部署与生产验证清单
> 生产或准生产独立硬件压测；多租户＋多客户真实思考时间负载模型；浏览器前端端到端延迟；负载期间进程重启、数据库断连与消息重投故障注入；异地／对象存储 WAL 归档（带 `fsync`）与主备自动故障转移；长时浸泡测试（Soak Test）；应用＋配置＋凭据＋外部回执全链路灾备演练；生产基础设施与模型调用成本核算。

## 04 服务边界：内部权威、接口契约与外部反腐层 {#s4}

服务边界从**静态编译边界**、**事务并发与单写者权威**、**HTTP／OpenAPI 3.1 入站契约**、**异步事件边界**与**外部系统反腐层（ACL）**五个正交平面封闭定义。

{{r1_boundary_diagram fig="FIG 02"}}

### 4.1 静态包依赖规则与事务并发契约

以下分层与依赖规则由 `tools/dependency-check` 在每次 `CI=true make check` 中静态强制执行（当前覆盖 **215** 个 Go 包、**82** 条允许上下文关系，**0 违规**）：

| 边界层 | 允许导入方向 | 硬性禁止 |
|---|---|---|
| `cmd/` | 仅导入 `internal/bootstrap` | 禁止直接导入上下文或基础实现 |
| `internal/kernel` | 仅标准库（`math/big` 等） | 禁止导入外部模块、foundation 或任何上下文；业务值禁止 `float32`/`float64` |
| `internal/foundation` | 外部模块、`internal/kernel` 及其他 foundation 包 | 禁止导入任何业务上下文 |
| `internal/contexts/<bc>/api` | 仅标准库与 `internal/kernel` | 禁止暴露领域内部类型或数据库连接 |
| `internal/contexts/<bc>/internal/domain` | 仅 `internal/kernel` 与本上下文 domain | 禁止跨上下文导入或 I/O |
| 跨上下文引用 | 仅当 Context Map 存在上游关系时导入上游 `api/` | 允许清单 `tools/dependency-check/allowlist.txt` 由 `make allowlist` 自动生成，禁止手改 |
| 归档与黑盒测试 | `docs/` 不得包含任何 Go 代码；根 `tests/` 禁止导入 `internal/` | `pgtest` 仅限 `*_test.go` 导入 |

**事务与锁序不变量**：
- **单写者独占事务**：每个上下文只写自己的 `brant_<bc>` schema，持有独立的 `bentopg.DB` 与 `bento.ScopeTransactor`。严禁在一个事务内调用另一个上下文的事务 owner。
- **确定性锁序（无死锁保证）**：事务内必须先以 `FOR UPDATE` 锁住本聚合根，再通过上游发布的守卫函数以 `FOR SHARE` 锁住上游守卫行（当前 26 条守卫锁边已由归档构建器校验**无环**）。
- **双向关系反向读契约**：`TRD` 读取 `FUL` 出运事实与 `PUR` 承诺、`PUR` 按承诺行读取 `FUL` 已确认出运量（`R82`），均在读方锁住本聚合根后于本事务内无锁读取；其正确性依赖写方严格对称守卫——所有写订单事实的 `FUL`/`PUR` 命令必调 `OrderGuard v1`，所有确认出运量的 `FUL` 命令必调 `SupplierDeliveryGuard v2`（`FOR SHARE` 锁承诺根）。

### 4.2 18 个限界上下文独占边界总览

下表由 Context Map、代码目录与迁移目录自动核算生成，标明每个上下文的独占 Schema、聚合／命令规模与启用开关。

{{r1_context_matrix}}

### 4.3 HTTP / OpenAPI 3.1 服务入口与输入硬限

对外唯一同步服务契约为 `contracts/openapi/brant-api.openapi.json`（OpenAPI **3.1.0**，版本 `0.1.0-candidate`，由 `tools/openapigen` 从传输层自动生成，禁止手工修改）。

- **路由模式**：所有业务写命令统一采用 `POST /api/brant/v1/<bc>/commands/<command>`，并配对 `POST .../reconcile` 用于提交结果未知（断连／超时）后的幂等对账；所有受控只读查询采用 `GET /api/brant/v1/<bc>/...`；通用审计查询采用 `GET /api/brant/v1/{bc}/audit`；健康探针为 `GET /healthz`。
- **请求体与超时硬限（`strictjson` ＋ `httpapi` ＋ `cmd/brant-api`）**：
  - 默认命令请求体 `≤ 1 MiB`（`DefaultMaxBytes`），最大 JSON 嵌套深度 `64`，最大 JSON 值数量 `100,000`；重复键、`null`、未知字段、尾随数据、非法 UTF-8 一律拒绝 `422 SCHEMA_INVALID`。
  - 文件登记 `file.register` 上限 `15 MiB`（`MaxFileBody`）；合规名单导入 `screening_list.import` 与 `country_programme.import` 上限 `32 MiB`（`MaxListImportBody`）。
  - 服务端超时：`ReadHeaderTimeout 5 s`、`ReadTimeout 30 s`、`WriteTimeout 30 s`、`IdleTimeout 60 s`、`MaxHeaderBytes 64 KiB`。
- **错误语义契约（C5）**：HTTP 状态码仅按领域拒绝类别映射（`401` 未认证、`403` 越权／F2 拦截、`404` 未找到、`409` 冲突／并发／上限、`413` 载荷超限、`422` 校验／引用无效），不泄露底层数据库或内部堆栈字符串。

下表由构建器直接统计当前 `brant-api.openapi.json` 生成：

{{r1_openapi_table}}

### 4.4 异步事件与死信边界（Outbox / Inbox）

- **Transactional Outbox**：`137` 种集成事件（事件源 `idp-brant.<bc>`）与聚合状态变更在同一 PostgreSQL 事务中原子写入 `bento.outbox`，消除双写不一致。
- **Inbox 去重与 8 个系统消费方**：每个事件消费方仅订阅 Context Map 中其上游上下文的事件（由 `Subscriptions()` 与白名单校验），使用独立系统操作者 `system:inbox.<bc>.<purpose>`，以事件 ID 作为命令幂等键，在事务前的 `Prepare` 阶段读取上游状态，绝不在消费处理中调用上游事务 owner。
- **死信隔离与人工重放（ADR-BR-015）**：单条投递连续失败达 `-max-failures`（默认 10 次）或认领达 `-max-attempts`（默认 50 次）后放弃并落入 `brant_msg` 死信表，触发独立指标 `brant_outbox_relay_dead_letters_total` 与 ERROR 日志；仅授权人工可通过 `POST /api/brant/v1/msg/commands/dead_letter.replay` 按事件 ID 重放。

### 4.5 外部系统服务边界（ACL 与责任切分）

| 外部系统／角色 | Brant 接入点 | 集成模式与当前实现（`8066e1c`） | 事实责任与硬性边界 |
|---|---|---|---|
| **IAM / OIDC 身份提供方** | `internal/foundation/identity/oidc` | ACL：标准 OIDC 发现、JWKS 与 RFC 9068 `at+jwt` 验签；本地维护 `brant_auth` 权限投影；**真实企业 IdP 尚未联调（延后至 B0）** | 外部 IdP 拥有用户认证权威；Brant 不宣称与外部 SSO 跨系统原子一致 |
| **大语言模型服务** | `internal/ai/adapters/model/openaicompat` | Adapter（`ModelPort`）：OpenAI 兼容 HTTP 协议，密钥仅来自 `IDP_BRANT_MODEL_API_KEY`；本机实测 `qwen3.8-flash-next-iq3_xxs` | 模型仅产出候选与工具调用请求，**不拥有任何业务事实或权限，不能宣告任务完成** |
| **EER（TELOS 执行运行时）** | `internal/ai/adapters/eer` | ACL：按公开 Runtime API v1 手写 HTTP 客户端，仓库不含任何 EER 代码／契约／夹具；2026-11-13 本机冒烟通过，**Brant 步骤未在 EER 登记（S4-05 PARTIAL）** | EER 可承担外部步骤编排，但绝不拥有报价、订单、资金或库存真值 |
| **海关单一窗口 / 金税 / 银行** | `internal/foundation/interop` ＋ `CNX` / `STL` | ACL：人工登记对账路径 ＋ 进程内渠道端口与假适配器（`8066e1c` 提供 5 条 CNX 渠道 HTTP 命令，超时记 `unknown`，乱序回执非权威留存）；**零真实外拨** | 报关放行、税款核定、退税批复与银行清算均属外部法定权威；Brant 绝不伪造真实申报凭证 |
| **外部 ERP / 法定总账** | `STL`（过账口径边界） | ACL：Brant 维护不可变业务应收应付、核销与订单经营结果；**无直连 ERP 适配器** | Brant 不是法定总账系统；ERP 过账回执为外部事实 |
| **BIP（原交换与渠道平台）** | `internal/ai/adapters/bip` | **已永久退出（2026-11-23 负责人确认）**；代码仅保留未连接占位（`EXTERNAL_CHANNEL_NOT_CONFIGURED`），所有外发转人工发送节点 | 后续不规划、不接入 BIP |
| **前端工作台（UI-01—UI-09）** | OpenAPI 3.1 HTTP 契约 | 本仓库仅交付后端 API 与契约定义，不含浏览器前端代码 | 前端不得绕过服务端权限过滤或替代服务端计算 |

::: details 展开全部 53 条事实权威总表（唯一写入方与允许消费方）
每条业务事实在 Brant 中严格只有一个写入方上下文，消费方必须与写入方在 Context Map 中具备上下游关系（已由归档构建器 100% 静态校验）。

{{cm_fact_matrix}}
:::

## 05 回归证据、运维支持机制与 G5 门禁清账 {#s5}

### 5.1 自动化回归与等价证据摘要

- **最新主干远程 CI（`main` `8066e1c`）**：推送运行 [45161081163](https://github.com/idp-xyz/idp-brantgo/actions/runs/45161081163) 全部 **16 项作业成功**（静态检查、bootstrap 1/6—6/6、bootstrap race 1/6—6/6、脚本化 Q01 70/70、负载冒烟与一致性尖刺、`brant-ci`）。三项后续合并提交未在本机再次合跑全量集成，以该次远程 CI 作为合并验证依据。
- **档案与契约一致性（`ece31a1`）**：拉取请求 #13（`b328dc3`）32 项远程检查全部通过；`build_archive.py` 全部结构与交叉引用校验通过，最新 Go 源码快照与当前 `1,369` 个 Go 文件逐字节一致。
- **S6 端到端本机门禁（`9e2502a`）**：PostgreSQL 16 容器 `brant-pg16`（端口 55471）全量 795 项 bootstrap 集成测试 `SHARDS=4` 墙钟 8 分 04 秒全部通过（详情见 [15_S6_EVIDENCE](15_S6_EVIDENCE/README.md)）。
- **S1—S3 Python V0.50 等价基准**：`{{=pythonUniqueTests}}` 个唯一 Python 测试方法逐项登记（已移植 `{{=goTestsPorted}}` 项，其余均写明 ADR 有意背离或 Python 实现专属理由）；S5、S6 无 Python 对等套件，S4 以 Q01 V0.1 16 个向量为对等参考。

### 5.2 运维与商业支持机制：已有资产与缺口对照

| 维度 | 仓库现有工程资产 | 商业发布（G5）尚缺的支持机制 |
|---|---|---|
| 中继监控与排障 | `ops/runbook-outbox-relay.md`、Prometheus `GET /metrics`、滞后告警／事故日志、死信人工重放命令 | 生产级监控告警接入（Alertmanager／值班通知）、确认后的 S5R-D07 阈值、分级响应 SLA |
| 备份、恢复与升级 | `scripts/backup-restore.sh`（逻辑恢复、PITR、跨版本升级演练）、`brant-migrate` 拓扑迁移 | 异地／对象存储 WAL 归档与保留周期、定期自动演练、包含配置／凭据／外部对账的全栈灾备手册 |
| 租户初始化与退场 | `brant-admin`（角色、消费方、AI 运行器初始化）、`deploy/postgres/00_roles.sql` & `10_app_grants.sql` | 客户上线切换 SOP、历史数据范围声明、租户／模块停用后的只读留存与受控导出机制（基线 11.2） |
| 技术支持访问 | 基线 6.1 规范要求限时、范围受控、经授权并审计，不常态使用超级权限 | 面向生产运维的临时授权审批工单与审计复核流程尚未落地 |

### 5.3 G5（R1 商业发布）门禁逐项清账

下表严格对照产品基线 12.1 节 G5 通过条件，说明本文件补齐了什么、还阻断在什么位置：

{{r1_g5_checklist}}

> [!decision] 本文件归档结论（2026-11-24）
> 1. **补齐规格缺口**：本文件以 `8066e1c` 与 `ece31a1` 为事实基准，完整交付了 G5 所需的四项公开规格材料（公开支持矩阵、授权、部署组合、服务边界），并纳入主档案自动构建与校验链。
> 2. **确认 BIP 永久退出**：依据 2026-11-23 负责人决议，BIP 永久移出 Brant 产品选项与后续路线，所有外部交换统一走人工发送节点与对应专业上下文受控路径。
> 3. **门禁结论不变**：`S6-06` 仍为 **`PARTIAL`**，`G5` 仍为 **`NO-GO（模拟）`**。要推动 G5 转为 `GO`，必须依次完成：① 修复或经负责人确认关闭 S5R-11 中继 P95 与 S4-07 真实模型四族失败；② 在生产级主机完成真实企业 OIDC、真实客户任务（`S5-06`／`AN12`）与外部专业渠道核对实证；③ 建立商业运维支持机制并召开产品、研发／交付与经营负责人参与的真人评审签核。
