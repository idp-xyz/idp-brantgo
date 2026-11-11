# 07 · S2 执行证据（2026-11-10）

本目录保存 S2 寻源与核价切片的原始执行证据，对应报告 `../06_IDP_Brant_S2_Execution_Report_V0.1.html`。

证据层级：**E2 本机执行＋GitHub Actions 运行记录**。本机证据在作者的 WSL2 工作站上产生，使用专用 PostgreSQL 16.12 容器 brant-pg16 与 Go 1.26.5（CI 所用版本）；远程证据是私有仓库 idpxyz/idp-brantgo 上 S2 分支的 GitHub Actions 运行记录。没有独立复核，也没有真人签核；“通过”不改变 G0、G1 的 NO-GO 结论。

| 文件 | 内容 | 对应工作项 |
|---|---|---|
| 01_environment.json | 主机、Go、PostgreSQL、模块清单与 git 提交 | 全部 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256（不含 docs/、brand/ 与由 Context Map 生成的 allowlist.txt）；档案构建器以此校验证据对应当前源码 | 全部 |
| 03_make_ci_go1.26.5.txt | 以 Go 1.26.5、CI=true 运行 `make ci` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查、迁移、集成测试、竞态检测、一致性实测 | S2-01–08 |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果，含 660.00 USD 基线冻结与守卫的提交顺序测试 | S2-01–08 |
| 05_github_actions_runs.json.txt | S2 分支的 GitHub Actions 运行记录：编号、分支、提交、每一步结果 | S2 全部 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码模糊测试 30 秒的结果 | 入口 |

未执行：真实名单文件与真实客户样例（名单与报价均为合成数据）；主体与 SKU 主数据修订进入可采纳判定；读取核价上下文历史时重查证据；TRD 与 PUR 对 AdoptabilityGuard 的真实调用（S3）。本机运行使用已有的开发数据库（迁移按账本增量应用）；从空库迁移由 GitHub Actions 每次运行。
