# 13 · S5 执行证据（2026-11-12 至 11-13）

本目录保存 S5 B0 受限试点扩展的原始执行证据，对应报告 `../12_IDP_Brant_S5_Execution_Report_V0.1.html`。S5 按负责人决定先于 S4 归档（E-9）：S5 为档案 V1.8，使用 09 阶段的 12、13 号；S4 以后为 V1.9，使用 10、11 号。

证据层级：**E2 本机执行＋GitHub Actions 运行记录**。本机证据在作者的 WSL2 工作站（Ubuntu-24.04）上产生；远程证据是公开仓库 idp-xyz/idp-brantgo 上 s5/business 分支的 GitHub Actions 运行记录。没有独立复核，也没有真人签核；“通过”不改变 G0、G1 的 NO-GO 结论，G3、G4 按负责人决定记为模拟评审 NO-GO。

编写本目录的档案单元 E2 不运行数据库：数据库上的结果全部来自产生它们的单元与主导者，原样复制，各自注明日期与环境（01_environment.json 的 runs_in_this_folder）。E2 自己只运行了 make check（03）并生成源文件快照（02）。

| 文件 | 内容 | 来源与环境 | 对应工作项 |
|---|---|---|---|
| 01_environment.json | 生成本目录时的主机、Go、模块与 git 提交；各项原始运行的环境 | E2 | 全部 |
| 02_go_source_manifest.txt | 最终提交全部源文件的 SHA-256（不含 docs/、brand/ 与由 Context Map 生成的 allowlist.txt）；档案构建器以此校验证据对应当前源码 | E2 | 全部 |
| 03_make_check.txt | 在最终提交上以 CI=true 运行 `make check` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查（含 docs/ 不含 Go 代码） | E2，WSL2，无数据库 | 全部 |
| 04_gate_results.md | 主导者在每个合并批次上的门禁：迁移、`make integration`、`make race` 的通过包数、失败、数据竞争与结束时间，以及每次合并后的 `make check` | 主导者，WSL2，PostgreSQL 16.12 容器 brant-pg16（127.0.0.1:55471） | 全部 |
| 05_github_actions_runs.json.txt | s5/business 的 GitHub Actions 运行：编号、提交、结论与每一步结果 | GitHub Actions（brant-ci），每次在全新 PostgreSQL 16 上迁移 | 全部 |
| 06_brc001_s5_chain.json | TestBRC001OrderToSettlementOverHTTP 第一次运行的链路记录：环境、夹具、19 位人工、301 个 HTTP 请求（方法、路由、状态、对象、行版本、回执或拒绝码、事实）、三轮分发的事件与 Inbox 记录、未执行项 | 单元 D1，2026-11-13，Go 1.26.8，PostgreSQL 16.12 容器 brant-pg16-d1 | 计划 §5；S5-01—05、07、08、12、13、15、17 |
| 07_brc001_s5_chain_run2.json | 同一测试的第二次运行 | 同上 | 同上 |
| 08_brc001_s5_chain_table.md | 由记录生成的表格：逐步结果、分发、每个请求、金额、人工、夹具与未执行项 | 单元 D1 | 同上 |
| 09_brc001_s5_compare.txt | 两次运行的比对：规范化服务端 ID、SHA-256、时刻与租户后 0 差异 | 单元 D1 | 同上 |
| 10_capacity_and_recovery/ | 阶梯负载三轮的报告与汇总、汇总表、两轮备份恢复与升级演练的日志、事实表与校验结果 | 单元 C7，2026-11-12，WSL2（20 核、64 GiB），PostgreSQL 16.12 未调优 | S5-11、IG-14 |
@@EVIDENCE-D2-README@@

S5 期间主导者在每次合并后运行 `make check`，按批运行迁移、`make integration` 与 `make race`；每个单元在自己的容器上运行 `make check` 与 `make integration`（CI=true，不允许跳过数据库测试）。本目录只保存主导者门禁的摘要（04）与远程运行（05），不保存逐项测试日志。

未执行：海关单一窗口、税务、银行、保险人与承运人的真实系统（S5 为人工路径，回执与水单为合成）；真实信用、国别项目、许可证、HS 编码与退税数据（全部合成）；生产或准生产环境上的负载、恢复与复制演练；真实身份提供方（测试使用本地签发者）；真实客户样例与真人签核；S4 的任务与 AI 层；STK 与 CLM；界面。E2 没有运行 `make ci`、`make integration` 或 `make race`。
