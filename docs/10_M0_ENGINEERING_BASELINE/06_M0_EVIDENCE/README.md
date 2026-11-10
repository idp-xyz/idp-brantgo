# 06 · M0 工程证据（2026-11-07；2026-11-10 复跑 make ci）

本目录保存 M0 工程基线的原始执行证据，对应报告 `../05_IDP_Brant_M0_Execution_Report_V1.0.html`。

证据层级：**E2 本机执行**。证据在作者的 WSL2 工作站上产生，使用专用 PostgreSQL 16.12 容器，Go 版本为 1.26.5（与 CI 相同）和 1.26.8。没有远程 CI 运行记录，也没有独立复核。这里的“通过”只表示本机通过，不改变 G1 NO-GO 结论。

| 文件 | 内容 | 对应工作项 |
|---|---|---|
| 01_environment.json | 主机、Go、依赖、PostgreSQL、角色、观察到的问题与未执行项 | 全部 |
| 02_go_source_manifest.txt | Go 模块 53 个源文件的 SHA-256，对应 2026-11-10 复跑时的源码。仓库还没有 git 提交，以此追溯；不含 docs/、brand/ 与由 Context Map 生成的 tools/dependency-check/allowlist.txt（改动 Context Map 后以 make allowlist 重新生成） | 全部 |
| 03_make_ci_go1.26.5.txt | 以 CI 的 Go 1.26.5 运行 `make ci` 的完整日志：模块校验、格式、vet、golangci-lint、单元测试、依赖检查、迁移、集成测试、竞态检测、一致性实测 | M0-01、02、09 |
| 04_make_ci_go1.26.8.txt | 以本机默认 Go 1.26.8 运行 `make ci` 的日志 | M0-09 |
| 05_integration_verbose_go1.26.5.txt | 单元与 PG16 集成测试逐项结果：69 个测试函数与 2 个模糊测试（只跑种子用例），连同子测试共 88 项通过，0 失败、0 跳过 | 全部 |
| 06_kernel_fuzz.txt | 两个模糊测试各 30 秒：十进制解析 34,475,392 次执行；HALF_UP 舍入与逐位字符串运算的独立结果比对 21,278,625 次执行；均未发现反例 | M0-03 |
| 07_migrate_first_run.txt | 空库迁移：以非超级用户 brant_migrator 应用 2 条迁移，Bento CheckSchema 通过 | M0-05、06 |
| 08_migrate_second_run.txt | 重复执行：0 条应用，2 条已登记 | M0-05 |
| 09_dependency_check_negative.txt | 在临时副本中预埋违规后，检查以退出码 1 失败：D1／AC2、D2／AC3、AC6、AC7×3、AC10×2；另有 D7 警告 | M0-08 |
| 10_spike_consistency.json | SC-04（方案 A、B1–B3）与 SC-03（无锁与根锁的交错执行、各 200 轮压力测试）结构化结果 | M0-10 |
| 11_relay_drill_test.txt | Outbox 中继与 SIGKILL 崩溃演练：首次运行和 3 次复跑 | M0-11 |
| 12_golangci_lint.txt | golangci-lint v2.15.0：0 个问题 | M0-01 |
| 13_clock_observation.txt | 开发机墙上时钟阶跃对租约的影响及原因 | M0-11 |
| 14_make_ci_go1.26.5_rerun.txt | 2026-11-10T05:32:45Z 以 Go 1.26.5、CI=true 复跑 `make ci` 的完整日志，exit 0，无失败、无跳过 | M0-08、09 |

未执行：GitHub Actions 运行（无远程仓库）、受保护主干、必需评审、签名溯源、OIDC、Inbox、ErrCommitUncertain 恢复、任何业务上下文与业务验收测试，以及三项阻断 ADR 的签核。

复跑（2026-11-10）：Context Map 关系由 43 条增加后，`tools/dependency-check/main_test.go` 去掉了写死的关系数（改为要求能读出关系并生成白名单），`make allowlist` 重新生成了 `tools/dependency-check/allowlist.txt`。与 2026-11-07 的源码相比，只有这两个文件和 `AGENTS.md` 不同。复跑只执行了 Go 1.26.5 的 `make ci`（14）；03、05–13 仍是 2026-11-07 的记录，04（Go 1.26.8）与模糊测试未复跑。
