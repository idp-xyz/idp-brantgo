# 05 · S1 执行证据（2026-11-10）

本目录保存 S1 主数据与询价切片的原始执行证据，对应报告 `../04_IDP_Brant_S1_Execution_Report_V0.1.html`。2026-11-10 第二批合并后重新生成（档案 V1.4），覆盖第一批的同名文件；第一批的运行记录保留在 05 中。

证据层级：**E2 本机执行＋GitHub Actions 运行记录**。本机证据在作者的 WSL2 工作站上产生，使用专用 PostgreSQL 16.12 容器 brant-pg16 与 Go 1.26.5（CI 所用版本）；远程证据是私有仓库 idpxyz/idp-brantgo 上 GitHub Actions 的运行记录。没有独立复核，也没有真人签核；“通过”不改变 G0、G1 的 NO-GO 结论。

| 文件 | 内容 | 对应工作项 |
|---|---|---|
| 01_environment.json | 主机、Go、PostgreSQL、模块清单与 git 提交 | 全部 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256（不含 docs/、brand/ 与由 Context Map 生成的 allowlist.txt）；档案构建器以此校验证据对应当前源码 | 全部 |
| 03_make_ci_go1.26.5.txt | 以 Go 1.26.5、CI=true 运行 `make ci` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查、迁移、集成测试、竞态检测、一致性实测 | S1-01–11、G1 C07、C10 |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果 | S1-01–11、G1 C07、C10 |
| 05_github_actions_runs.json.txt | GitHub Actions 运行记录：编号、分支、提交、每一步结果；含一次失败（42815414128，竞态检测下 TestConcurrentCommands 暴露账本同键并发窗口，已修复，见报告 S1-D32） | M0-09、G1 C04–C07、C09、C10、C13 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码模糊测试 30 秒的结果 | S1-06 |

未执行：受保护主干、必需评审与签名溯源（GitHub 免费版私有仓库不提供，负责人决定不配置）；真实身份提供方（测试使用本地签发者，负责人决定延后至 B0 试点门之前）；Inbox 订阅登记与 WF-03 风险传播（S4-06）；定时复筛任务；主体交易档案、地址与服务方角色（S1-02）与货号映射（S1-03）；真实名单文件与真实客户样例（名单为合成数据）。本机运行使用已有的开发数据库（迁移按账本增量应用）；从空库迁移由 GitHub Actions 每次运行。
