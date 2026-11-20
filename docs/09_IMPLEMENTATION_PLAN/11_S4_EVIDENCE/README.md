# 11 · S4 执行证据（2026-11-13 至 11-19；档案 V1.9，2026-11-20）

本目录保存 S4（任务、AI 层、文件抽取、EER 与 BIP 适配、Q01 评测）与同期 S5R 修订轮的执行证据，对应报告 `../10_IDP_Brant_S4_Execution_Report_V0.1.html`。S5 按负责人决定先于 S4 归档（E-9）：S5 为档案 V1.8，使用 09 阶段的 12、13 号；S4 为档案 V1.9，使用 10、11 号。

证据层级：**E2 本机执行＋GitHub Actions 运行记录**。本机证据在作者的 WSL2 工作站（Ubuntu-24.04）上产生；远程证据是公开仓库 idp-xyz/idp-brantgo 上 s4/tasks-ai 分支的 GitHub Actions 运行记录。S4 的树止于提交 1aa0290。没有独立复核，也没有真人签核：S5R 的 47 项决定是模拟评审（E0），G2 按负责人决定 E-9 记为模拟 NO-GO；“通过”不改变 G0、G1 的 NO-GO 结论。

编写本目录的档案单元 E2 不运行数据库，也不运行评测：结果全部来自产生它们的单元与主导者，原样复制，各自注明日期与环境（01_environment.json 的 runs_in_this_folder）。E2 自己只运行了 `make check`（03）并生成源文件快照（02）；Go 源码与 1aa0290 一致，E2 的改动在 docs/ 之下，外加由 Context Map 生成的 `tools/dependency-check/allowlist.txt` 头部注释行里的哈希。**本档案提交自己的门禁与远程运行记录在拉取请求中**，不在本目录：档案文件不能记录包含它们自己的提交的运行结果。

| 文件 | 内容 | 来源与环境 | 对应工作项 |
|---|---|---|---|
| 01_environment.json | 生成本目录时的主机、Go、模块与 git 提交；各项原始运行的环境 | E2 | 全部 |
| 02_go_source_manifest.txt | 源文件的 SHA-256（不含 docs/、brand/ 与由 Context Map 生成的 allowlist.txt）；档案构建器以此校验证据对应当前源码 | E2 | 全部 |
| 03_make_check.txt | 在本目录的树上以 CI=true 运行 `make check` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查，及末行退出状态 | E2，WSL2，无数据库 | 全部 |
| 04_gate_results.md | 主导者在每个合并提交上的门禁：迁移、`make integration`、`make race`（各分片，每个 bootstrap 测试恰在一个分片运行一次）、100 连接上的脚本化评测、每次合并后的 `make check`，失败与作废的门禁；末尾是 s4/tasks-ai 每次推送的远程运行 | 主导者，WSL2，PostgreSQL 16.12 容器 brant-pg16（127.0.0.1:55471）与 brant-pg16-ci100（127.0.0.1:56310） | 全部 |
| 05_github_actions_runs.json.txt | s4/tasks-ai 的 GitHub Actions 运行（28 次推送）：编号、提交、结论与每一步结果 | GitHub Actions（brant-ci），每次在全新 PostgreSQL 16 上迁移 | 全部 |
| 06_evaluation_runs.md | tests/ai-evaluation 全部 26 个运行文件夹的一张表：脚本化与真实、提交、试次、通过、目标完成、模型与工具调用、令牌、费用、人工修正、复制了哪些文件、说明 | 主导者，由各运行的 report.json 生成 | S4-07、S4-08 |
| 07_evaluation_runs/ | 评测运行的原件：每个运行的 summary.md；全部真实运行与最终提交上的脚本化运行另有 report.json；最终提交 1aa0290 上的两个运行（脚本化 `20261119T101737Z` 70 个试次，真实 `live-20261119T103137Z` 4 个试次）另有每个试次的 trial.json | 主导者，lead 检出的 bin/ai-eval，Go 1.26.8，PostgreSQL 16；真实运行用本机模型服务 | S4-07、S4-08；BC-AT27、BC-AT28 |
| 08_s5r_regression/ | R12 的回归证据：S5R 决定的端到端场景（s5r_scenarios.json/.md）、D1 链路的两次运行比对与链路记录、CN-AT 矩阵、伴随场景表、八个 S5R 场景记录、与 S5 证据及基线的图形差异 | 单元 R12，2026-11-17，PostgreSQL 16.12 容器 brant-pg16-r12（127.0.0.1:56220） | S5R-12 |
| 09_d2_eer_live/ | D2 的 EER 真实运行的摘要（`make eer-live` 及两次适配器真实测试：命令与时间、Brant 自己的测试名与结果、条件、没有执行的部分）；原始输出不在档案里：它带有 EER 平台试点计划的标识符，不在已跟踪的代码里，仓库是公开的（决定 E-2），主导者审查后撤下，是否公开由负责人决定 | 单元 D2，2026-11-13，本机 EER（原件在作者工作站的交接目录，不公开） | S4-05、IG-10 |

S4 期间主导者在每次合并后运行 `make check`，按批运行迁移、`make integration` 与 `make race`（自 2026-11-17 起还在 100 连接的 PostgreSQL 上运行脚本化评测，即 CI 的条件）；每个单元在自己的容器上运行 `make check` 与 `make integration`（CI=true，不允许跳过数据库测试）。本目录只保存主导者门禁的摘要（04）与远程运行（05），不保存逐项测试日志。

没有复制：评测的 `trace/` 目录（真实运行的本地诊断，不是证据）、各运行的 `brant-*.log` 进程日志与每个试次的 `worker-*.log`；早于最终提交的脚本化运行的 report.json（每个约 900 KB；06 的表保留其合计，summary.md 保留逐家族表）；R12 的 18 个 CN-AT 用例记录与 14 个伴随场景记录（与 S5 证据 `../13_S5_EVIDENCE/` 的差异在 08 的差异文件里）；`live-20261118T100123Z`（没有 report.json：它不是一次运行，`make ai-eval-live` 在开始 13 秒后因 SIGHUP 结束，没有任何模型调用，见 06）。

未执行：除正常家族外的 13 个家族在真实模型上的试次（真实运行只有正常家族，在 1aa0290 上 4 个试次 4 个通过；全量真实评测在 2026-11-19 19:07 负责人问「不跑，直接做E2可以吗」之后由主导者停止）；负责人亲自操作的试次（E-7 选项 c）；真实脱敏询价与人工基线（AN12，BC-D01 之前不可执行，E-6 只允许合成数据）；真实客户、真实 BIP 接口（IG-11 未开）、EER 运行 Brant 的步骤、EER worker 崩溃与重放；生产或准生产环境；真人签核（G2 为模拟 NO-GO）。E2 没有运行 `make integration`、`make race`、`make ai-eval` 或 `make ai-eval-live`。
