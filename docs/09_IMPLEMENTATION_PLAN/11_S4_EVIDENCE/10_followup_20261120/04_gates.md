# 后续轮的门禁行（2026-11-20）

这些行从单元报告和主导者的门禁日志抄出。日志本身不进档案。E3 没有重跑 F2 与 R14 的整门禁。

## R14

报告 `s4-r14-report.md`。单元提交 597074f，合并 0cfdf59。容器 brant-pg16-s4r14，端口 56420。

- `make allowlist` 退出 0。
- `CI=true make check` 退出 0。依赖检查 189 个包，73 条已允许的上下文关系，没有违规。
- `CI=true make integration PG_PORT=56420 SHARDS=4` 退出 0。bootstrap 771 个测试各一次（204 + 187 + 178 + 202），0 FAIL，0 SKIP。其余 188 个包退出 0。墙钟 17 分 16 秒。没有跑 `make race`。

## F2

报告 `s4-f2-report.md`。单元提交在更正轮之后是 f049cc0，合并 9d2d045。容器 brant-pg16-s4f2，端口 56410。

- `CI=true make check` 退出 0（更正轮之前的树，以及更正轮各自一次）。
- `CI=true make integration PG_PORT=56410 SHARDS=4` 退出 0。bootstrap 768 个测试各一次（202 + 186 + 179 + 201），0 SKIP。墙钟 17 分 11 秒。这次集成在更正轮之前；更正轮只改了测试与 AGENTS.md 的反引号，报告写明没有重跑集成。
- `make ai-eval PG_PORT=56410` 退出 0。运行 `20261120T071425Z`，70 次里 70 次通过。an10 的五次把普通路由上的 `order.prepare`、`goal.verify` 与 `rfq.save_draft` 记为 `PERMISSION_DENIED`。
- 没有跑 `make race`、`make ai-eval-live`、`make eer-live`。

## 主导者在 9d2d045 上的门禁 f1

日志 `scratchpad/lead/logs/f1-gate.out`。开始于 2026-11-20T09:07:54Z，树是干净的 9d2d045。结束于 2026-11-20T09:41:32Z，`overall=0`，`gate_rc=0`。结束后树仍是干净的 9d2d045（09:46:47Z）。

- 迁移：先应用 1 个，随后 0 个新应用、96 个已记录。`db=0`。
- 集成：bootstrap 772 个测试，SHARDS=6，各一次（127＋140＋124＋116＋123＋142），墙钟 14 分 11 秒。`integration=0`。
- 竞态：同样 772 个测试，SHARDS=6，0 个数据竞争，墙钟 19 分 23 秒。`race=0`。
- 脚本化套件：容器 brant-pg16-ci100（`max_connections` 100），70 次里 70 次通过，315 秒，客户端连接峰值 12，运行 `bin/ai-eval/20261120T094145Z`。`ai_eval_rc=0`。

9d2d045 已推到 `origin/s4/followups`。`gh run list --branch s4/followups` 只列出运行 44718089797（2026-11-20T10:18:22Z 开始，工作流 Brant M0 / PG16 / Bento RC2，事件 push，标题是 F2 的合并说明）。E3 读到的状态先是 in_progress，随后是 queued。两次都没有结论。没有 ad704e3 的远程运行。

## L1

报告 `s4-l1-report.md`。单元提交 e9966e9，合并 ad704e3。容器 brant-pg16-s4l1，端口 56430。模型 Grok 4.7。

- `go vet -tags aievallive ./tests/ai-evaluation/` 退出 0。
- `CI=true make check` 退出 0。lint 0 个问题，137 个包 ok，依赖检查 189 个包。
- `CI=true make integration PG_PORT=56430 SHARDS=4` 退出 0。bootstrap 773 个测试各一次（204＋187＋179＋203），188 个其他包，0 SKIP，墙钟 25 分 44 秒。
- `make ai-eval PG_PORT=56430` 退出 0。70 次里 70 次通过。脚本化 bundle 哈希没有变。
- 冒烟运行退出 0，对着假的模型服务，不是证据，不在本目录。
- 主导者合并时的 `f-l1-check.log` 是一次 `make check`：golangci-lint 0 issues，日志跑到依赖检查，没有 FAIL 行。文件没有退出状态行。

没有跑 `make race`、`make ai-eval-live`、`make eer-live`。没有 9d2d045 或 ad704e3 上的真实运行。没执行的族的重跑还没有做。
