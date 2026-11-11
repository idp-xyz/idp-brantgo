# 09 · S3 执行证据（2026-11-11）

本目录保存 S3 报价、确认、订单与采购草稿切片的原始执行证据，对应报告 `../08_IDP_Brant_S3_Execution_Report_V0.1.html`。

证据层级：**E2 本机执行＋GitHub Actions 运行记录**。本机证据在作者的 WSL2 工作站上产生，使用专用 PostgreSQL 16.12 容器 brant-pg16 与 Go 1.26.5（CI 所用版本）；远程证据是公开仓库 idp-xyz/idp-brantgo 上 S3 分支与 PR 的 GitHub Actions 运行记录。没有独立复核，也没有真人签核；“通过”不改变 G0、G1 的 NO-GO 结论。

| 文件 | 内容 | 对应工作项 |
|---|---|---|
| 01_environment.json | 主机、Go、PostgreSQL、模块清单与 git 提交 | 全部 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256（不含 docs/、brand/ 与由 Context Map 生成的 allowlist.txt）；档案构建器以此校验证据对应当前源码 | 全部 |
| 03_make_ci_go1.26.5.txt | 以 Go 1.26.5、CI=true 运行 `make ci` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查、迁移、集成测试、竞态检测、一致性实测 | S3-01–17 |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果，含守卫的提交顺序测试、BC-AT15、BC-AT17 与 BRC-001 全链 | S3-01–17 |
| 05_github_actions_runs.json.txt | S3 分支与 PR 的 GitHub Actions 运行记录：编号、分支、提交、每一步结果 | S3 全部 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码模糊测试 30 秒的结果 | 入口 |
| 07_brc001_chain.json | TestBRC001FullChainOverHTTP 在 04 的运行中写下的链路记录：环境、夹具、十位操作者、每个 HTTP 请求的方法、路由、状态、对象、行版本与金额（成本快照 660.00 USD、报价 960.00 USD、订单 960.00 USD、采购草稿 600.00 USD） | S3-12 |

S3 期间每次合并都在本机运行 `make check`、`make integration` 与 `make race`；本目录只保存最终提交上的一次完整运行。

未执行：STL 信用守卫与 CNX 贸易合规守卫（按负责人决定 S4—S5 接入，订单生效记为 not_wired）；TSK 对象控制命令；FUL、STK 出运事实与 PUR 承诺；外部发送渠道；真实客户样例与真实名单（全部为合成数据）。本机运行使用已有的开发数据库（迁移按账本增量应用）；从空库迁移由 GitHub Actions 每次运行。
