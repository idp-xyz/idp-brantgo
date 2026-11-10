# 05 · S1 执行证据（2026-11-10）

本目录保存 S1 主数据与询价切片第一批的原始执行证据，对应报告 `../04_IDP_Brant_S1_Execution_Report_V0.1.html`。

证据层级：**E2 本机执行＋GitHub Actions 运行记录**。本机证据在作者的 WSL2 工作站上产生，使用专用 PostgreSQL 16.12 容器 brant-pg16 与 Go 1.26.5（CI 所用版本）；远程证据是私有仓库 idpxyz/idp-brantgo 上 GitHub Actions 的运行记录。没有独立复核，也没有真人签核；“通过”不改变 G0、G1 的 NO-GO 结论。

| 文件 | 内容 | 对应工作项 |
|---|---|---|
| 01_environment.json | 主机、Go、PostgreSQL、模块清单与 git 提交 | 全部 |
| 02_go_source_manifest.txt | 证据运行时全部源文件的 SHA-256（不含 docs/、brand/ 与由 Context Map 生成的 allowlist.txt）；档案构建器以此校验证据对应当前源码 | 全部 |
| 03_make_ci_go1.26.5.txt | 以 Go 1.26.5、CI=true 运行 `make ci` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查、迁移、集成测试、竞态检测、一致性实测 | S1-01–08、S1-10 |
| 04_integration_verbose_go1.26.5.txt | 单元与 PostgreSQL 16 集成测试逐项结果 | S1-01–08、S1-10 |
| 05_github_actions_runs.json.txt | GitHub Actions 运行记录：编号、分支、提交、每一步结果 | M0-09、G1 C04–C06、C09 |
| 06_strictjson_fuzz.txt | 严格 JSON 解码模糊测试 30 秒的结果 | S1-06 |

未执行：受保护主干、必需评审与签名溯源（GitHub 免费版私有仓库不提供，负责人决定不配置）；真实身份提供方（测试使用本地签发者）；岗位模板与职责分离（BC-D02）；Inbox 去重与 ErrCommitUncertain 恢复（S4-06）；OpenAPI 3.1 候选；POL 的 ConfigGuard 与其余 13 种配置类型；主体筛查（S1-09）与名单导入（S1-11）；真实客户样例。
