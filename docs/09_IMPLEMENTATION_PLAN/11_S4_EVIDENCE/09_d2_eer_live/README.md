# D2 的 EER 真实运行：摘要（S4-05）

**原始输出不在本档案里，读者只靠本档案不能核对这些运行。** D2 单元在 2026-11-13 对本机运行的 EER（IDP TELOS 平台的执行运行时）做过真实运行，输出是三份 Go 测试日志。V1.9 的初稿把它们原样放进了本目录；主导者在 2026-11-20 审查后把它们撤下，留下本摘要。

- 为什么撤下：仓库是公开的，决定 E-2 把 EER 的代码、契约与夹具留在这个仓库之外；三份日志带有 EER 试点计划里的标识符（它的步骤、连接方、事件与决定的名称，以及它的拒绝文本），这些标识符不在仓库已跟踪的代码里。
- 谁决定：主导者 2026-11-20 的审查决定，是否公开原始输出等负责人决定（见 S4 报告第 09 章的待决问题）。
- 原件在哪里：作者工作站上的交接目录（不在仓库里，不公开）。
- 摘要只写 Brant 自己的测试名（都在公开的树里）和它们的结果，不写 EER 一侧的任何标识符、拒绝文本或地址。

## 运行了什么

| 运行 | 命令与时间 | 树 |
|---|---|---|
| 1 | 适配器对真实 EER 的 5 个测试（`TestLive…`），日志文件时间 2026-11-13 13:41Z | 日志没有记录运行的树 |
| 2 | 同一个测试 `TestLiveSubmissionIsIdempotentByOperation` 单独一次，日志文件时间 13:41Z | 同上 |
| 3 | `make eer-live PG_PORT=56290`，2026-11-13T13:49:02Z 至 13:49:26Z | wip 提交 4dc57c3 加上未提交的改动：Makefile、`internal/ai/channel/channel.go`、`internal/ai/execution.go`、`internal/ai/execution/runner.go`、`internal/ai/runtime/runtime.go` 与两个新文件 `internal/bootstrap/ai_eer_live_integration_test.go`、`internal/bootstrap/ai_runtime_integration_test.go`（日志头记录的 7 个路径） |

运行 3 先对两个构建标签做 `go vet`，再跑 `go test -tags eerlive -count=1 -v ./internal/ai/adapters/eer/`，最后跑 `go test -tags 'integration eerlive' -count=1 -v -run '^TestEERLive' ./internal/bootstrap/`；数据库连接参数由 make 目标设置，不写在这里。

## 测试与结果

运行 3（日志末行 `eer-live=0`）：

| 测试 | 对象 | 结果 | 秒 |
|---|---|---|---|
| `TestSubmitIsOneExecutionPerOperation` | 包 `internal/ai/adapters/eer`，合成的 EER 替身 | PASS | 0.04 |
| `TestLostAnswersAreReconciledByLookup` | 同上 | PASS | 0.03 |
| `TestBarrierCancelsTheExecutionAndWaitsForItsEnd` | 同上 | PASS | 0.02 |
| `TestDecideAnswersTheApprovalWait` | 同上 | PASS | 0.02 |
| `TestAnswersBrantCannotTrustAreRefused` | 同上 | PASS | 0.01 |
| `TestCredentialsNeverAppear` | 同上 | PASS | 0.03 |
| `TestHumanTasksOfAnExecution` | 同上 | PASS | 0.02 |
| `TestStatesMapOntoBrants` | 同上 | PASS | 0.00 |
| `TestLiveSubmissionIsIdempotentByOperation` | 同一个包，真实 EER | PASS | 0.74 |
| `TestLiveLostAnswersAreReconciled` | 同上 | PASS | 0.93 |
| `TestLiveStopBarrierCancelsAndWaits` | 同上 | PASS | 0.70 |
| `TestLiveRecoveryByAFreshProcess` | 同上 | PASS | 1.65 |
| `TestLiveIdentityIsTheTokens` | 同上 | PASS | 0.86 |
| `TestEERLiveStopBarrierOfATask` | 包 `internal/bootstrap`，PostgreSQL 16 上的一个 Brant 任务对真实 EER 的停止屏障 | PASS | 3.44 |

包 `internal/ai/adapters/eer` 的结果行是 `ok`（5.065 s），包 `internal/bootstrap` 的是 `ok`（3.448 s）；没有 FAIL。计数：适配器对合成 EER 的测试 8 个，对真实 EER 的测试 5 个，加上 PostgreSQL 16 上的停止屏障测试 1 个。

运行 1（5 个 `TestLive…` 测试，全部 PASS）：`TestLiveSubmissionIsIdempotentByOperation` 9.29 s、`TestLiveLostAnswersAreReconciled` 0.76 s、`TestLiveStopBarrierCancelsAndWaits` 0.66 s、`TestLiveRecoveryByAFreshProcess` 1.77 s、`TestLiveIdentityIsTheTokens` 0.84 s；包结果 `ok`（13.322 s）。运行 2：`TestLiveSubmissionIsIdempotentByOperation` PASS（0.80 s），包结果 `ok`（0.805 s）。运行 1 的幂等测试比运行 2 与 3 少检查一种情形（日志里的检查行数与行号不同），所以运行 1 是同一批测试的较早状态；运行 2 与运行 3 的检查行相同。运行 1 与 2 的日志没有开始、结束与退出状态的行。

## 运行的条件

- EER 是本机运行的平台，驱动的是它的试点计划 `address-error-actions`（计划名与平台名 TELOS 都已在已跟踪的代码里），输入是合成的。
- 用的是平台的开发身份；凭证在运行时从环境读取，不写进任何文件（`TestCredentialsNeverAppear` 是适配器测试之一）。
- 平台的 worker 用替身执行者运行，没有登记 Brant 的计划或执行者（D2 报告 D2-D04）。所以这些运行说明 Brant 一侧的适配器、停止屏障与恢复在真实 EER 上工作，**不**说明 EER 运行 Brant 的步骤。

## 没有执行

- EER 工作进程崩溃与重放（会重启共享平台）、人工任务的创建与完成、审批到期的路径（D2 报告）。
- D2 的最终提交 c0facac 之后没有再运行 `make eer-live`，合并后的树上也没有重跑。
- EER 运行 Brant 的步骤；任何对 BIP 的运行（BIP 没有接口）。

S4-05 与 IG-10 因此仍为 PARTIAL。
