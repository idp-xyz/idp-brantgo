# IDP Brant 主档案变更记录

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
