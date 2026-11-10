---
output: 10_M0_ENGINEERING_BASELINE/01_IDP_Brant_M0_Go_Baseline_and_G1_Gate_V0.1.html
title: IDP Brant M0 工程基线与 G1 技术准入门禁 V0.1
short: M0 工程基线 / G1 门禁 V0.1
kicker: IDP BRANT / 10 M0 ENGINEERING BASELINE
doc_title: M0 工程基线与 G1 技术准入门禁 V0.1
doc_desc: 门禁映射 · 12 项技术准入控制 · 技术基线 · 阻断 ADR · AI 风险治理 · 进入 S1 的条件
meta: 版本=V0.1; 日期=2026-11-07（回写 M0 证据）; 结论=G0 NO-GO · G1 NO-GO; 状态=模拟评审完成（E0）· 未经真人签核
stage: 10 · M0 工程基线与 G1 门禁
eyebrow: M0 BASELINE / G1 GATE
search: 搜索控制项、ADR 或门禁
foot: M0 Go Baseline · G1 Gate V0.1 · 2026-11-07
---
<span class="scope-badge">GATE STATUS: NO-GO</span>

<h2>证据与批准是两个维度：能运行不等于已获批，已获批也不等于已验证</h2>

<p class="lede">本阶段定义 Brant 进入 Go 实现之前必须满足的工程基线与技术准入条件，并登记全部 ADR 与继承的待决项。M0 已于 2026-11-07 在本机执行（{{=m0DoneLocal}}／{{=m0Tasks}} 项完成），证据与建议决定见 <a href="05_IDP_Brant_M0_Execution_Report_V1.0.html">M0 执行报告</a>。当前结论不变：G0（客户场景确认）NO-GO，G1（详细设计与技术准入）NO-GO。</p>

::: cover
- 检查清单 | 02_IDP_Brant_G1_Gate_Checklist_V0.1.json：{{=g1Controls}} 项控制，含状态与是否阻断。
- ADR 登记册 | 03_IDP_Brant_ADR_Register_V0.1.json 与 04 CSV：{{=adrs}} 条 ADR 与继承待决项。
- M0 执行报告 | 05_IDP_Brant_M0_Execution_Report_V1.0.html 与 06_M0_EVIDENCE：本机实测、建议决定与原始证据。
- 形式化规则 | GO = 全部签核 ∧ 全部证据 ∧ 全部强制领域测试 ∧ 全部集成门禁 ∧ 无未决阻断项。
:::

## 01 门禁映射与当前结论 {#s1}

Brant 的产品基线自己定义了 G0—G5。Cargo 主档案的 G0 是“M0 工程启动门禁”，在 Brant 的语义里对应 G1；Brant 的 G0 是客户场景确认。为了避免同名不同义，本档案一律使用 Brant 的定义。

{{cm_gates}}

> [!decision] 为什么 G0 NO-GO 不阻止 M0
> 产品基线 12.3 节规定：缺少 G0 客户证据不妨碍受控 P0 研发，但不能据此进入真实业务试点。因此 M0 与 S1—S3 可以在合成数据上推进，B0 试点必须先通过 G0。

## 02 G1 技术准入检查清单 {#s2}

{{g1_controls}}

当前满足情况（2026-11-07 回写 M0 证据）：9 项 PARTIAL，C01、C11、C12 为 NOT_MET，没有 MET。C02（ADR-BR-004）与 C03（ADR-BR-011）已有 M0-10 的 PG16 实测与建议决定，仍待签核；C04、C05、C06、C09 的本机证据齐全，还缺远程 CI 运行与主干保护。逐项证据见 [M0 执行报告](05_IDP_Brant_M0_Execution_Report_V1.0.html#s10)。

## 03 M0 技术基线 {#s3}

| 能力 | 候选 | 理由 | 裁决状态 |
|---|---|---|---|
| 语言与工具链 | Go 1.26.5（go.mod toolchain 指令），与 idp-bento-go 一致 | 与 Cargo 同一工具链，便于复用框架与 CI | 候选；M0 本机以 1.26.5 与 1.26.8 验证 |
| 架构 | 单仓库、单 Module 的模块化单体＋六边形 | 18 个上下文是模型边界，不是部署单元 | 候选（ADR-BR-001、REPO-01） |
| 框架 | go.idp.xyz/idp-bento-go v0.1.0-rc.2 | Repository、Outbox、Inbox 与 testkit 合同 | 候选；RC 不等于稳定版；M0 已引用并验证 h1 |
| 平台 | go.idp.xyz/idp-platform-go v0.1.0-rc.1 | 编号等跨产品能力 | 待 IG-09 |
| 数据库 | PostgreSQL 16；brant_<bc> schema；FORCE RLS | 与 Cargo 一致；按上下文拥有数据 | 候选；M0 已建 bento、brant_auth |
| 数据访问 | pgx/v5＋sqlc | 显式 SQL 与强类型查询 | 候选；M0 使用 pgx/v5 |
| 迁移 | brant-migrate，沿用 bento 迁移资产与校验值不可变 | Cargo V2.5 已验证同一做法 | M0 已实现（本机） |
| 接口 | net/http；OpenAPI 3.1；JSON Schema 2020-12 | 薄接口层，只做校验、身份映射与调用应用服务 | 候选（S1） |
| 身份 | OIDC＋本地授权投影＋authority_epoch | 撤权与生效按同一守卫裁决 | 守卫已实测（ADR-BR-IMP-002）；OIDC 待 S1 |
| 金额 | 标准库 math/big：big.Rat 精确运算、big.Int 最小单位 | 必须与 Python 规则逐分一致 | 已附实测，模拟评审接受（ADR-BR-REPO-10） |
| 一致性 | 每上下文独立事务 owner；上游发布守卫函数，下游在自身事务中调用 | Cargo T1 与 Brant 同事务重查要求冲突 | 方案 B，模拟评审接受（ADR-BR-004） |
| 隔离与锁 | READ COMMITTED＋本聚合根行 FOR UPDATE 优先 | 替代 SQLite 全库写串行 | 已附实测，模拟评审接受（ADR-BR-011） |

## 04 阻断 ADR {#s4}

{{adr_register group=一致性}}

| 阻断 ADR | 裁决所需证据 | 负责人 | 关联工作 |
|---|---|---|---|
| ADR-BR-004 | SC-04 两种提交顺序在 PG16 上的结果；方案 A 的提交窗口度量；方案 B 守卫读取在 idp-bento-go 事务 owner 下的可行性 | 首席架构师、研发负责人、TRD／CST Owner | M0-10（已实测，报告第 05 章）、S2-06 |
| ADR-BR-011 | SC-03、BC-AT15、BC-AT17 在 PG16 上的竞争测试；锁序死锁检查 | 技术负责人、DBA | M0-10（SC-03 已实测，报告第 06 章）、S3-05、S3-07 |
| ADR-BR-REPO-10 | T02 §11 与 T03 §5.3 全部合成样例的独立算术对照，结果逐分一致 | 首席架构师、财务 | M0-03（已实测，报告第 04 章）、S2-04、S3-02 |

## 05 ADR 登记册 {#s5}

{{adr_register}}

## 06 AI 风险与自治治理 {#s6}

AI 预授权与领域写入都不能绕过确定性业务守卫。Brant 沿用 Cargo 的自治分级作为内部策略，并加上 Brant 的人工专属命令清单。

| 级别 | 含义 | Brant 首发策略 |
|---|---|---|
| L0 | 只读与建议 | 允许：读取授权上下文、生成候选与解释 |
| L1 | 草稿写入 | 允许：询价、报价、核价场景的草稿保存，走同一命令与对象控制 |
| L2 | 需强审批的动作 | 只在具体 ApprovalGrant 绑定命令摘要、资源版本与到期时间后执行 |
| L3 | 预授权低风险动作 | 首发不启用，需逐项风险评估 |
| L4 | 无人值守的高风险动作 | 默认拒绝 |

人工专属命令（AI 身份即使有通配授权也被拒绝）以 05 阶段战术模型的 humanOnlyCommands 为唯一清单，06 阶段命令目录与下表都由它生成；AC9 按此清单逐命令登记测试。

{{human_only}}

岗位模板与职责分离（BC-D02）以 05 阶段 06_IDP_Brant_Role_Templates_V0.1.json 为唯一来源：模拟评审 SR-32 接受的 15 个默认岗位，每条命令至少归属一个岗位；Go 端由 tools/rolegen 生成目录，过期时测试失败。租户可把多个模板合并为自己的岗位，但不得合并下列禁止组合，操作者所持岗位的并集也不得构成禁止组合；人工专属命令仍只接受人工身份。岗位到命令的映射是候选（E0），真实客户开通时由租户确认。

{{role_templates}}

其他必须验证的风险：邮件、附件与网页内容一律不可信，不能把文本变成授权；提示注入红队覆盖越权与跨租户；模型端点、允许字段与日志脱敏按 BC-D07 登记；预算与重试上限由执行入口强制。

## 07 进入 S1 的最低条件 {#s7}

1. C02、C03 已裁决并签核（含对 Cargo 基线 T3 的例外或放宽的书面决定）。
2. C04、C05、C06、C09 达标：仓库可构建，编译器边界与白名单检查在 CI 中可失败，PG16 迁移与 RLS 可执行，主干受保护。
3. ADR-BR-REPO-10 已裁决，internal/kernel 的金额与数量值对象通过独立算术对照。
4. C01 中 S1 涉及的上下文（EVD、PTY、CAT、POL、INQ）的词条与边界已签核。
5. 08 阶段映射表中 S1 对应的行已排入工作项（PORT_PLANNED）。

## 08 签核记录 {#s8}

| 控制项或 ADR | 签核人 | 角色 | 日期 | 结论 | 证据引用 |
|---|---|---|---|---|---|
| （待填写） | — | — | — | — | — |

签核只能由实际责任人登记；实施证明可行，不证明已被批准。第 09 章的模拟评审不填入本表；本档案中的任何“通过”都不改变本页的 NO-GO 结论，除非对应控制项获得签核并附有证据。

## 09 模拟评审记录 {#s9}

> [!warn] 这是 AI 模拟评审，不是真人签核
> 由 AI 依次扮演首席架构师与各领域负责人，依据本档案、SAP 标准流程公开资料与中国外贸通行做法逐项裁决，证据层级 E0。属于产品决定的事项直接裁决；取决于具体客户的法律与税务事实改为租户配置并由客户确认；需要真实证据的事项（真实客户样例、远程 CI、OIDC、AI 红队）不能由模拟评审满足。真人签核后，在第 08 章登记签核人、日期与证据。

模拟 G0 走查见 04 阶段第 08 章。机器可读源为 07_IDP_Brant_Simulated_Review_V0.1.json，裁决 CSV 由构建器生成；构建器校验模拟评审覆盖全部 ADR 与全部上下文，且 ADR 状态与裁决一致。

{{sim_review}}

