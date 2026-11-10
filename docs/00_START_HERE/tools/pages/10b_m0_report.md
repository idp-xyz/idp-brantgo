---
output: 10_M0_ENGINEERING_BASELINE/05_IDP_Brant_M0_Execution_Report_V1.0.html
title: IDP Brant M0 工程基线执行报告 V1.0
short: M0 执行报告 V1.0
kicker: IDP BRANT / 10 M0 ENGINEERING BASELINE / EXECUTION REPORT
doc_title: M0 工程基线执行报告 V1.0
doc_desc: Go 模块与依赖锁定 · kernel 精确算术 · 事务 owner、迁移、角色与 RLS · 授权守卫 · 依赖检查 · 一致性实测 · Outbox 中继 · CI
meta: 版本=V1.0; 日期=2026-11-07; 证据=E2 本机执行; 结论=G1 NO-GO · M0 {{=m0DoneLocal}}／{{=m0Tasks}} 项本机完成
stage: 10 · M0 工程基线与 G1 门禁
eyebrow: M0 EXECUTION REPORT
search: 搜索工作项、场景、测试或证据
foot: M0 Execution Report V1.0 · 2026-11-07
---
<span class="scope-badge">M0 EXECUTED LOCALLY · G1 NO-GO</span>

<h2>工程基线已在本机 PostgreSQL 16 上跑通；三项阻断决定有了实测依据，等待签核</h2>

<p class="lede">2026-11-07，idp-brantgo 根目录建立了 Go Module go.idp.xyz/idp-brant，按 WBS 完成 M0 的 {{=m0Tasks}} 项工作：{{=m0DoneLocal}} 项在本机全部通过验收，2 项部分完成（M0-03 等待 ADR-BR-REPO-10 裁决，M0-09 缺远程仓库与主干保护）。证据为本机执行（E2），尚无远程 CI 记录；G1 仍为 NO-GO。</p>

::: cover
- 结论 | 能运行不等于已获批。ADR-BR-004、ADR-BR-011、ADR-BR-REPO-10 已有实测与建议决定，签核前仍阻断 G1。
- 证据 | 06_M0_EVIDENCE：两次 make ci（Go 1.26.5 与 1.26.8）、逐项测试日志、一致性实测 JSON、迁移与崩溃演练日志、源文件 SHA-256 快照。
- 未执行 | 远程 CI、受保护主干、OIDC、Inbox、任何业务上下文与业务验收测试。
:::

## 01 结论摘要 {#s1}

::: cards
- M0-03 | kernel 精确算术 | 标准库 math/big；BRC-001 等 Python 向量逐分一致；两个模糊测试未发现反例。
- M0-04 | 每上下文独立事务 owner | AC4：在一个上下文的事务中调用另一个上下文的 DB，读、写与事务三种入口都返回 ErrTransactionConflict。
- M0-05 · 06 | 迁移、角色与 RLS | 空库迁移、重复执行、篡改拒绝、失败不记账；brant_app 执行 DDL 被拒；跨租户、跨公司负向测试通过。
- M0-07 | 授权守卫 | AI 与 system 身份执行人工专属命令被拒；撤权与生效两种提交顺序由同一守卫行裁决。
- M0-08 | 依赖检查 | 白名单由 Context Map 生成；预埋的 D1、D2、AC6、AC7、AC10 违规都使检查失败。
- M0-10 | 一致性实测 | SC-04：方案 A 有提交窗口，方案 B 三种顺序都正确。SC-03：无根锁 200／200 轮出现两个赢家，根锁 0／200。
- M0-11 | Outbox 中继 | 失败退避、放弃不阻塞分区、SIGKILL 后按租约重投、SIGTERM 正常退出。
- M0-09 | CI | 工作流已定义，本机等价的 make ci 通过；尚无远程仓库与主干保护。
:::

{{wbs_tasks phase=M0}}

状态说明：DONE_LOCAL 表示验收项在本机 PostgreSQL 16.12 与 Go 1.26.5 下全部通过、尚无远程 CI 记录；PARTIAL 表示部分完成或等待裁决。

## 02 交付物、测试与证据对照 {#s2}

| 工作项 | 代码位置 | 关键测试或检查 | 证据 |
|---|---|---|---|
| M0-01 | go.mod、Makefile、README.md、AGENTS.md、.golangci.yml、.gitignore | go build、go vet、golangci-lint 0 个问题 | 03、12 |
| M0-02 | go.mod（toolchain go1.26.5）、go.sum | bento h1 校验、go mod verify、go mod tidy -diff | 01、03 |
| M0-03 | internal/kernel | TestBRC001Arithmetic 等对照测试；FuzzParseDecimal、FuzzRoundHalfUpAgainstDigitOracle | 05、06 |
| M0-04 | internal/foundation/bento | TestContextsNeverJoinEachOthersTransactions、TestScopeBindsTheTransaction | 05 |
| M0-05 | cmd/brant-migrate、migrations/、internal/foundation/database | TestApplyIsIdempotentAndRefusesTampering；两次迁移实跑 | 05、07、08 |
| M0-06 | deploy/postgres、scripts/dev-postgres.sh、migrations/auth | TestApplicationRoleIsUnprivileged、TestApplicationRoleCannotRunDDL、TestScopesAreIsolatedByRowLevelSecurity | 05、07 |
| M0-07 | internal/foundation/identity、internal/foundation/authorization | TestAIIsRefusedHumanOnlyCommandsEvenWithTheGrant、两个撤权竞争测试、TestEpochChangesOnlyWhenAccessChanges | 05 |
| M0-08 | tools/dependency-check、.github/CODEOWNERS | 单元测试与负向实测 | 09 |
| M0-09 | .github/workflows/ci.yml、Makefile | make ci | 03、04 |
| M0-10 | tools/spikes/consistency | 5 个测试、9 个场景 | 10 |
| M0-11 | internal/foundation/messaging、internal/bootstrap、cmd/brant-worker | TestWorkerCrashRecoveryDrill 等 13 个测试 | 11、13 |

证据列的编号对应 06_M0_EVIDENCE 中的文件序号。M0 结束时仓库有 69 个测试函数与 2 个模糊测试（证据 05）；它们不是 Python 测试的移植，当时 08 阶段映射表的移植数为 0。S1 之后的数字见 09 阶段 S1 执行报告。

{{m0_tests}}

## 03 环境与依赖锁定 {#s3}

| 项目 | 值 | 说明 |
|---|---|---|
| Go 模块 | go.idp.xyz/idp-brant；go 1.26.0；toolchain go1.26.5 | 根目录唯一 go.mod；docs/ 与 brand/ 不含 Go 代码 |
| Go 版本 | CI 使用的 1.26.5 与本机默认的 1.26.8 各运行一次 make ci | 证据 03、04 |
| 框架 | go.idp.xyz/idp-bento-go v0.1.0-rc.2，h1:W9T1KsbXwuVs3lHTNwSUcEBY0Ids00hUH7I9KuBtCEo= | 与 Cargo 记录一致；GOPRIVATE 使其从源仓库直接解析 |
| go.sum | 44 行，与 Cargo go_cargo/go.sum 逐字节一致 | go mod verify、go mod tidy -diff 通过 |
| PostgreSQL | 16.12，digest sha256:97ff59a4…52a3，与 Cargo CI 相同 | 本机 Docker Hub 不可达，开发脚本从 ECR Public 镜像站按同一 digest 拉取 |
| 专用容器 | brant-pg16，127.0.0.1:55471，数据库 brant | 避开已被占用的 55432、55433；不触碰其他项目的容器 |
| 数据库角色 | brant_migrator 拥有 schema 并执行迁移；brant_app 无超级用户、BYPASSRLS 与 DDL | deploy/postgres/00_roles.sql、10_app_grants.sql |
| 静态检查 | golangci-lint v2.15.0，经 go run 临时运行 | 0 个问题；配置见 .golangci.yml |

## 04 kernel 精确算术与 ADR-BR-REPO-10 {#s4}

internal/kernel 只依赖标准库：Decimal（big.Rat）、以最小单位存放的 Money（big.Int）、最大余数分配、含税与未税拆分、ExchangeRate、Quantity、Ratio、ValidityInterval、UTC 微秒时间戳、Scope 与 ActorRef。下表是从 Python V0.50 与设计文档搬来的独立算术对照，全部逐分一致。

| 向量 | 来源 | Go 结果 |
|---|---|---|
| BRC-001：24＋36＋12＋48＝120 PCS，单价 8.00 USD | 白皮书合成核算样例 | 净销售 960.00；成本 660.00；毛利 300.00；毛利率 31.25%；付款 288.00＋672.00；采购草稿 600.00 |
| 两行各 1 × 0.005 | T03 test_multiple_lines_round_before_aggregation | 先逐行舍入，1＋1＝2 分；先汇总再舍入只有 1 分 |
| 含税 960.00，税率 13% | T03 test_inclusive_tax_is_split_from_line_payable_before_margin | 净额 849.56（84955.75… 舍入）、税额 110.44 |
| 未税 100.00，税率 13% | foundation.tax.preview | 税额 13.00、含税 113.00；从 113.00 反向拆分得到相同结果 |
| 100 USD，汇率 7.123456 CNY/USD | T02 §11 | 712.35 CNY，只在最后舍入一次 |
| 0.05 按 33.33／33.33／33.34 分期 | T03 test_payment_tail_uses_remainder_order_and_zero_stage_is_rejected | 2／1／2 分；总额 0.01 时得 0／0／1，由调用方拒绝零额阶段 |
| 阶梯价 [0, 100) 单价 6、[100, ∞) 单价 5，数量 120 | T04 A46 阶梯测试 | 分段计价 700.00、全量计价 600.00、差异 100.00 |
| 成本 660.00，最低毛利率 25% | T03 毛利规则 | 收入 880.00 恰好通过，879.99 需要审批；精确比较，不先舍入毛利率 |
| 10 DOZEN，换算系数 12 | 共享基础单位换算 | 120 PCS |

两个模糊测试各运行 30 秒：十进制解析与按文本逐位计算的规范形式比对 34,475,392 次，HALF_UP 舍入与逐位进位的独立实现比对 21,278,625 次，均未发现反例。分配算法另有 2,000 轮随机性质测试：结果之和等于总额，每份都是精确份额的下取整或上取整，输入顺序不影响结果。

> [!decision] 建议：标准库 math/big，不引入第三方十进制库
> 精确运算用 big.Rat，金额用 big.Int 最小单位；只在显式舍入点按 HALF_UP 舍入，对非负值与 Python 的 (2n+d)//(2d) 完全相同；金额的绝对值必须小于 10^24（对应 CALCULATION_LIMIT_EXCEEDED）；解析时拒绝符号、科学计数法、NaN、前导零与超出位数的输入，JSON 浮点数因此无法混入。待首席架构师与财务裁决。

> [!note] 负数的舍入
> kernel 对负数采用“远离零”的 HALF_UP；Python 的公式对负数是“向正无穷”。现行契约只对非负金额舍入，两者在契约范围内没有差别；若将来出现负数舍入，须在对应上下文的规则中明确选择。

## 05 ADR-BR-004：跨上下文资格重查（SC-04） {#s5}

场景：TRD 激活订单时依赖 CST 的成本快照，CST 同时否认这份快照。业务核心设计的要求是：否认先提交，激活必须被拒绝；激活先提交，激活有效，否认随后生效并触发风险登记。实测在 PostgreSQL 16 的 READ COMMITTED 下进行，TRD 与 CST 各有自己的 Bento 事务 owner。CST 发布守卫函数 guard_snapshot_adoptable（SECURITY DEFINER，函数内 SELECT … FOR SHARE），TRD 在自己的事务中用自己的 executor 调用它。

{{m0_spike prefix=SC-04}}

- 方案 A 的提交窗口已确定性复现：TRD 读到“可采纳”，CST 的否认随后提交，TRD 的激活仍然成功，订单落在已被否认的快照上，只能依靠事后的风险事件补救。
- 方案 B 在三种顺序下都符合要求：谁先锁住快照行，谁的结果算数。TRD 用自己的 owner 调用守卫，没有触发 ErrTransactionConflict；B2 中否认确实进入了锁等待（pg_stat_activity 显示 Lock），直到激活提交。

> [!decision] 建议方案 B，并保留方案 A 的事件传播
> 上游在自己的 schema 中发布版本化的守卫函数，作为其 api 契约的一部分；下游只在自身事务中、用自身 executor 调用，只用来放行或拒绝本上下文的写入，不 JOIN、不写他域。激活先持有守卫时，上游的撤销等待其提交，然后发布事件，由下游登记风险。待首席架构师、研发负责人与 TRD、CST Owner 裁决。

> [!warn] 档案修正：守卫视图改为守卫函数
> 原设计写的是“上游发布只读守卫视图，下游以 FOR SHARE 读取”。PostgreSQL 对视图执行 SELECT … FOR SHARE 时，调用方须对视图本身有 UPDATE 权限，等于把写权限授予下游角色。SECURITY DEFINER 函数以上游 schema 所有者的权限执行，调用方只需 EXECUTE 权限，行锁仍归调用方事务所有。12 阶段的 GR 规则已按此修改。

## 06 ADR-BR-011：锁序与同根并发（SC-03） {#s6}

场景：同一张报价，一个请求要替代它，另一个请求要把它转为订单。业务要求两者只能有一个成功。两种实现的写入完全相同，唯一区别是读取报价根行时是否加 FOR UPDATE。

{{m0_spike prefix=SC-03}}

> [!decision] 建议：保留 READ COMMITTED，命令先锁本聚合根行
> 锁序为：本聚合根行 FOR UPDATE，再锁子行，最后以 FOR SHARE 调用上游守卫；上游从不锁下游行，守卫只沿 Context Map 的上游方向读取，因此不会形成死锁环。READ COMMITTED 下 FOR UPDATE 等待结束后读取最新已提交版本，效果相当于 SQLite 的 BEGIN IMMEDIATE 对同一个根的串行化，又不会串行化无关的根。不采用 SERIALIZABLE：它需要在每个命令外包重试，而 Bento 事务使用 READ COMMITTED。BC-AT15、BC-AT17 随 S3 在 TRD 上验证。待技术负责人与 DBA 裁决。

## 07 身份、授权与 RLS（M0-06、M0-07） {#s7}

foundation/authorization 拥有 brant_auth：每个租户与公司一行 authority_scope 守卫行，actor_grant 存放本地授权投影，两张表都启用 FORCE RLS。命令在自身事务中先后以 FOR SHARE 锁定守卫行和授权行；授权变更先以 FOR NO KEY UPDATE 锁定守卫行并递增 authority_epoch。规则以 ADR-BR-IMP-002 登记。

| 测试 | 断言 | 结果 |
|---|---|---|
| AI 执行人工专属命令 | quote.activate 对 AI 与 system 身份返回 ErrActorKindDenied，即使授权投影里有这项权限 | 通过 |
| 无授权、缺权限、未知命令、令牌声称 human 而投影为 ai | 全部拒绝 | 通过 |
| authority_epoch | 只有权限实际变化时才递增（Python test_authority_epoch_changes_only_when_access_changes 的语义） | 通过 |
| 命令先持有守卫 | 撤权进入锁等待，命令提交后撤权生效，下一次命令被拒 | 通过 |
| 撤权先持有守卫 | 命令进入锁等待，撤权提交后命令被拒 | 通过 |
| 跨公司、跨租户 | 看不到其他 scope 的授权行；写入其他 scope 的行返回 42501；主体 scope 与事务 scope 不一致返回 ErrScopeMismatch；事务外看不到任何行 | 通过 |
| brant_app 执行 DDL | 建 schema、建表、关闭或取消 FORCE RLS、删策略、删表、TRUNCATE、DELETE、读取迁移台账均返回 42501 | 通过 |
| 以迁移角色作为应用角色连接 | database.Open 拒绝（拥有 schema） | 通过 |

身份只来自已验证的令牌；OIDC 适配随 S1 的 HTTP 接口交付，此前 identity.Principal 的可信度取决于调用方。

## 08 迁移、依赖检查与 CI（M0-05、M0-08、M0-09） {#s8}

- 迁移：brant-migrate 先创建 bento schema 并应用 Bento 框架迁移，再按 migrations/manifest 应用 migrations/auth；每条迁移在事务内持有咨询锁，SHA-256 记入 brant_migrations.applied。空库应用 2 条，重复执行 0 条；已应用的迁移被修改时报“migration tampering”并停止；失败的迁移不记账。
- 依赖检查：在临时副本中预埋违规后，检查以退出码 1 失败，报出 D1／AC2（CST 反向导入 TRD 的 api）、D2／AC3（api 导入 Bento）、AC6（tests/ 导入 internal）、AC7（3 个上下文缺 CODEOWNERS）、AC10（docs/ 中的 Go 代码），以及 D7 别名警告。跨上下文导入 internal 包由编译器直接拒绝（AC1）。
- CI：.github/workflows/ci.yml 依次执行依赖校验、格式、vet、golangci-lint、依赖检查、角色、空库迁移、幂等迁移、应用授权、单元与集成测试（CI 中缺数据库即失败）、竞态检测、一致性实测报告。PostgreSQL 镜像与 Go 版本和 Cargo 相同。本机等价的 make ci 用时约 25 秒。

> [!decision] 规则澄清（ADR-BR-REPO-11）
> D5 改为：foundation 只导入外部 Module、标准库、kernel 与其他 foundation 包，不导入任何上下文、bootstrap 与入站层（authorization 需要 identity 与 bento）。新增 D9：internal/foundation/database/pgtest 等测试支撑包只允许测试文件导入。AC6 的检查方式改为 dependency-check：根目录 tests/ 与 internal/ 同属一个 Module，编译器不拦截这种导入。

## 09 Outbox 中继与崩溃演练（M0-11） {#s9}

brant-worker 经 internal/bootstrap 装配，Relay 拥有独立的事务 owner。PostgreSQL 16 上的结果：首次发布失败后按退避重试并成功；超过失败预算的毒消息被放弃，同一分区的后续事件继续投递；worker 在 sink 持有投递时被 SIGKILL，投递未标记为已发布，第二个 worker 在租约到期后重投，收到 SIGTERM 后正常退出，sink 共收到 2 次（至少一次投递）。

> [!warn] 开发机时钟阶跃使租约提前到期
> 首次演练中重投发生在首次投递后 1.454 秒，短于 3 秒租约。原因是 WSL2 的墙上时钟在演练期间前跳约 3 秒：WSL 时钟比 Windows 宿主快约 30 秒，systemd-timesyncd 与 Hyper-V 宿主时间同步交替校正。三次复跑的重投间隔为 3.075、3.092、3.016 秒。租约按墙上时钟比较，生产主机须平滑校时（启动后不再步进）并监控时钟偏差；详见证据 13。

## 10 G1 控制项与 ADR {#s10}

{{g1_controls}}

12 项控制中 9 项 PARTIAL，C01、C11、C12 为 NOT_MET，没有 MET；G1 仍为 NO-GO。以下是本次新增或因 M0 证据而更新的 ADR：

{{adr_register ids=ADR-BR-004,ADR-BR-011,ADR-BR-REPO-10,ADR-BR-IMP-002,ADR-BR-REPO-11,ADR-BR-REPO-04}}

## 11 未执行事项与风险 {#s11}

| 事项 | 影响 | 下一步 |
|---|---|---|
| 远程仓库与 GitHub Actions | 证据只有本机层级 | 负责人创建远程仓库后推送，运行 ci.yml |
| 受保护主干、必需评审、签名溯源 | C09 未满足 | 创建仓库时一并配置 |
| OIDC 令牌 | 主体的可信度取决于调用方 | S1 transport/http |
| Inbox 去重与 ErrCommitUncertain 恢复 | 跨上下文消费未验证（C07） | S4-06 |
| 业务上下文与业务验收 | 没有业务命令 | S1 起 |
| BC-AT15、BC-AT17 | 锁序尚未在真实聚合上验证 | S3-05、S3-07 |
| 开发机时钟阶跃 | 租约提前到期、提前重投 | 生产主机平滑校时并监控偏差 |
| idp-platform-go 编号 | 未引入 | IG-09 |
| ADR 签核 | ADR-BR-004、011、REPO-10 仍阻断 G1 | 首席架构师、财务、技术负责人与 DBA 签核 |

## 12 证据文件 {#s12}

{{m0_evidence}}

证据目录说明见 [06_M0_EVIDENCE/README.md](06_M0_EVIDENCE/README.md)。02 号文件是产生证据时 Go 模块源文件的 SHA-256 快照；构建器每次运行都核对证据与当前源码一致，代码改动后须重新产生证据。

## 13 进入 S1 之前 {#s13}

1. 签核 ADR-BR-004、ADR-BR-011、ADR-BR-REPO-10，以及 ADR-BR-IMP-002、ADR-BR-REPO-11。
2. 创建远程仓库，运行 CI 并开启主干保护，使 C04、C05、C06、C09 具备签核条件。
3. 完成 C01 中 S1 涉及的 EVD、PTY、CAT、POL、INQ 词条与边界签核。
4. 把 08 阶段映射表中属于 S1 的行排入工作项（PORT_PLANNED）。
5. S1 起，每新建一个上下文目录，同时补齐 CODEOWNERS、迁移目录、授权规则与 10_app_grants.sql 中的授权。
