---
output: 09_IMPLEMENTATION_PLAN/04_IDP_Brant_S1_Execution_Report_V0.1.html
title: IDP Brant S1 主数据与询价切片执行报告 V0.1
short: S1 执行报告 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / S1 EXECUTION REPORT
doc_title: S1 主数据与询价切片执行报告 V0.1
doc_desc: 远程 CI 跑通 · 命令账本 · EVD、PTY、CAT、INQ · HTTP 入口与 OIDC · 死信与重放 · BRC-001 前段重放 · Python 测试移植
meta: 版本=V0.1; 日期=2026-11-10; 证据=E2 本机执行＋GitHub Actions; 结论=S1 部分完成 · G1 NO-GO
stage: 09 · 工程实施总体计划
eyebrow: S1 EXECUTION REPORT
search: 搜索工作项、测试、Python 用例或证据
foot: S1 Execution Report V0.1 · 2026-11-10
---
<span class="scope-badge">S1 PARTIAL · REMOTE CI GREEN · G1 NO-GO</span>

<h2>远程 CI 已跑通；询价主链在 PostgreSQL 16 上从来源走到 ready，并能经 HTTP 与 OIDC 令牌重放</h2>

<p class="lede">2026-11-10，修复了使 GitHub Actions 无法解析的工作流文件，远程 CI 在私有仓库 idpxyz/idp-brantgo 上首次通过。S1 建立了命令账本（幂等、重放、审计与 Outbox 同一事务）和 EVD、PTY、CAT、INQ、POL 五个上下文，HTTP 入口以 OIDC 访问令牌认证，Outbox 有了死信、告警与人工重放。BRC-001 前段（四行合计 120 PCS 的询价确认为 ready）在 PostgreSQL 16 上直接调用和经 HTTP 都已重放。POL 完成首批 5 种配置类型；主体筛查与名单导入尚未开始；G1 仍为 NO-GO。</p>

::: cover
- 结论 | S1 部分完成：S1-01、S1-05、S1-08、S1-10 完成并有远程 CI 记录；S1-02、03、04、06、07 部分完成；S1-09、11 未开始。
- 证据 | 05_S1_EVIDENCE：环境、源文件 SHA-256 快照、make ci、逐项测试日志、GitHub Actions 运行记录、严格 JSON 模糊测试。
- 未执行 | 真实身份提供方、岗位模板（BC-D02）、Inbox 与 ErrCommitUncertain 恢复、POL 的 ConfigGuard 与其余 13 种配置、OpenAPI 3.1 候选、真实客户样例。
:::

## 01 结论摘要 {#s1}

::: cards
- G1 · C04、C05、C06 | 远程 CI 跑通 | 工作流文件第 67 行的 YAML 解析错误使首次推送 0 秒失败；修复后 GitHub Actions 全部 17 步通过，S1 分支每次推送都在全新 PostgreSQL 16.12 上迁移并运行集成与竞态测试。
- G1 · C09 | 受保护主干：负责人决定不配置 | GitHub 免费版的私有仓库不提供分支保护与规则集（API 返回 403）。负责人 2026-11-10 决定不升级、不配置；C09 本为非阻断项，记为接受的缺口，合并前以 PR 上的 CI 检查为准。
- S1 公共骨架 | 命令账本 | 契约、权限、已存幂等结果、CAS 与状态、引用的准入顺序沿用 Python；业务变化、动作结果、审计与 Outbox 同一事务提交；重放重查当前权限与可见性；拒绝一律审计。
- S1-01 | EVD 来源四层身份与业务文件 | 内容按 SHA-256 在公司内去重；邮件按渠道、账户与消息 ID 识别，同消息不同正文拒绝；来源不可变；猜正文探测不到他人私有来源。业务文件最多 10 MiB，校验类型与文件头，版本链不分叉。
- S1-02 · 03 | PTY 主体与联系人、CAT 的 SKU | 整稿草稿、人工核实、修订回到草稿并保留已核实版本、停用；主体用角色集合，同一主体可同时是客户与供应商；SKU 每次核实固定一个规格修订号。
- S1-05 · 08 | INQ 客户询价 | draft／ready／cancelled／closed；确认只限人工，须覆盖全部来源；记录确认时客户、联系人行版本与每行 SKU 规格修订号。BRC-001 前段四行 120 PCS 确认为 ready，直接调用与经 HTTP 各重放一次。
- S1-06 · C10 | HTTP 入口与 OIDC | 1 MiB、64 层、10 万个值；拒绝重复键、null、浮点数、未知字段、大小写不同的键名与空字符串；访问令牌须有签发者、受众、有效期、租户、公司与显式的操作者类型。
- S1-04 | POL 配置根与编号 | 配置的修订、发布须由编制人以外的人工执行（数据库约束同样强制）、原子替代、退役；首批 currency、unit、fx_rate、payment_terms、number_sequence；汇率按方向与用途、不推导反向、过期即阻断；单据编号按公司、类型、年度原子分配，同业务引用返回原号；基础快照只追加。
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

## 03 Python 测试移植 {#s3}

08 阶段映射表中 T01 与共享基础 V0.2 的 73 个测试方法：53 个已移植（PORTED），3 个为 SQLite 专属（NOT_APPLICABLE），17 个未移植；其中 4 个已移植一半（备注写明另一半所需的配置类型），其余依赖尚未实现的配置类型（company_profile、unit_conversion、tax、pricing_policy、document_template、bank_account、party_profile）。每个 Go 测试在注释中引用其 Python 来源。

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
> 真实身份提供方；岗位模板与职责分离（BC-D02）；Inbox 去重与 ErrCommitUncertain 恢复（S4-06）；OpenAPI 3.1 候选；主体筛查与名单导入（S1-09、S1-11）；POL 的 ConfigGuard（ADR-BR-004 守卫函数）与其余 13 种配置类型；真实客户样例。G0、G1 仍为 NO-GO。

> [!decision] 已决定（负责人，2026-11-10）
> 配置发布的核实证据须可读：Context Map 新增 R63 EVD→POL，POL 发布前经 EVD 读取证据，不可读返回 REFERENCE_INVALID（S1-D16）。受保护主干不配置：GitHub 免费版私有仓库不提供，C09 非阻断，记为接受的缺口。
