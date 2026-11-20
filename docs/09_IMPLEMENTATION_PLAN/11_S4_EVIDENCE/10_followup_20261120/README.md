# S4 后续轮证据（2026-11-20）

本目录是档案 V1.10 追加的证据。V1.9 放在 `11_S4_EVIDENCE/` 根上的文件不改，包括 `02_go_source_manifest.txt` 与 `03_make_check.txt`。构建器先用本目录的 `02_go_source_manifest.txt` 对照当前 Go 源码，找不到才用 V1.9 的那一份。

日期：2026-11-20。编写的单元是 E3，工作树 `.claude/worktrees/s4-e3`，分支 `s4/e3-archive`。第二轮把源码快照和 `make check` 换成包含 L1（合并 ad704e3）之后的这棵树；V1.9 根上的那两份文件不改。

| 文件 | 是什么 |
|---|---|
| `02_go_source_manifest.txt` | 第二轮这棵树的源码 SHA-256（含 L1）。不含 `docs/`、`brand/` 与点开头的目录。第一轮的那份已换掉 |
| `03_make_check.txt` | 第二轮这棵树上 `CI=true make check` 的输出，末行是退出状态。第一轮的那份已换掉 |
| `04_gates.md` | F2、R14、L1 的门禁行，以及主导者的门禁 f1（`gate_rc=0`）、推送和远程运行 44718089797 |
| `05_live-20261120T054323Z/` | 主导者检出里同名运行的 `report.json` 与 `summary.md`，字节与原件相同。提交 74743b8。开始 2026-11-20T05:43:23Z，结束 2026-11-20T06:48:04Z。70 次试次，15 次通过。本追加不改这个目录 |
| `06_scenarios/` | F2 与 R14 改过的场景在本工作树上的新记录 |
| `07_live-20261120T122849Z/` | 主导者检出里同名运行的 `report.json` 与 `summary.md`，字节与原件相同。提交 224ffcf。开始 2026-11-20T12:28:49Z，结束 2026-11-20T13:08:18Z。45 次试次，35 次通过，10 次失败，0 次未执行 |

环境：`05_live-20261120T054323Z` 的环境写在它自己的 summary.md（linux/amd64，go1.26.8，20 个 CPU，PostgreSQL 16 在 127.0.0.1:56310，并行 4，模型服务返回 qwen3.8-flash-next-iq3_xxs）。`07_live-20261120T122849Z` 的 summary 是同一台主机、同一个模型和同一个数据库，`report.json` 的 `environment.parallel` 是 2，`git_head` 是 224ffcf。场景重跑是第一轮在容器 `brant-pg16-s4e3`、端口 56440 上做的，第二轮没有重跑。源码快照与 `make check` 是包含 L1 的这棵树，本追加没有重做这两份。

同日在 224ffcf 上还有一次被停止的运行 `live-20261120T120042Z`。本目录不放它的任何文件。部分报告在主导者检出里记开始 2026-11-20T12:00:42Z、`finished_at` 2026-11-20T12:28:33Z，合计 18 次试次、5 次通过；试次目录是 an01 的五个、an03 的五个、an04 的五个，以及 an05-1、an05-2、an05-3。那不是一次完成的运行。完成的是 `07_live-20261120T122849Z`：45 次里 35 次通过，10 次失败，0 次因模型服务没让跑而记成未执行；an08、reads、feedback 三个只适用于脚本的族不适用。家族表与 an01、an03 的逐次结果在 S4 报告第 13 章，数字只按复制进来的 summary.md 与 report.json。

没有执行，也没有放进本目录：

- `trace/`、进程日志、每个试次的 `trial.json`、`.env`。两次运行都如此；被停止的那次连 `report.json` 与 `summary.md` 也不复制。
- an05 没有列入完成的那次真实运行。AN12 与负责人亲自操作的试次仍未执行。
- `make race`、`make ai-eval`、`make ai-eval-live`、`make eer-live`。本追加没有模型密钥，没有向 127.0.0.1:8080 发请求，没有新开数据库。上面两份文件是主导者已经跑完的原件。
- 编写第 13 章初稿时，9d2d045 的远程运行 44718089797 还没有结论。其后 224ffcf 的远程运行 44729594979 已成功（16 个作业全部成功，2026-11-20T11:41:04Z 开始），记在 `04_gates.md`。本追加提交自己的远程 CI 仍记在拉取请求里。
- L1 的冒烟运行。它对着假的模型服务，不是证据。

早先的文件被盖过的范围只写在 `06_scenarios/README.md`。

## 凭据检查

第二轮换过源码快照和 `make check` 之后，对本目录 10 个文件再跑了一次 `grep -r -n -i -E 'authorization|bearer|api[_-]?key|sk-|7897'`。命中都不是密钥、令牌值或代理地址：

| 命中 | 在哪里 | 是什么 |
|---|---|---|
| `sk-` | summary.md 的家族表，report.json 的设计编号，以及本说明里的同一句 | `TSK-INV-001`、`TSK-INV-002`、`TSK-INV-003`、`SM-TSK-TASK` 里的字母 `SK-` |
| `api_key` | report.json 一处 `"key_from_env_var": "IDP_BRANT_MODEL_API_KEY"`，以及本说明里的同一句 | 环境变量的名字。值不在文件里 |
| `authorization` | `02_go_source_manifest.txt` 的八个包路径；`03_make_check.txt` 的一行 `ok go.idp.xyz/idp-brant/internal/foundation/authorization` | 包路径和测试结果 |
| `bearer` | 源码快照 | `internal/foundation/identity/oidc/bearer.go` 与 `bearer_test.go` |
| `7897` | 源码快照的三处十六进制摘要 | 哈希里的数字，不是端口 |

另检：`postgres://` 与 `postgresql://` 在证据文件里是 0 行（本说明提到这两个词）。`password`、`secret`、`passwd`、私钥头、`proxy` 只出现在本说明列出的检索词里，别处是 0 行。`eyJ` 只出现在本说明里，没有 JWT。`token` 这个词出现在：report.json 的 212 行与 summary.md 的 9 行（模型用量字段和说明文字）、cn-at18.json 的 2 行（“agent's token”的说明，以及“本地签发者铸造每个令牌”）、S5R-D05.json 的 3 行（访问令牌是测试的本地签发者，没有令牌值）、源码快照里 `internal/ai/adapters/eer/tokens.go` 的路径、本说明的 1 行。模型服务地址在 report.json 里是 `http://127.0.0.1:8080`，与 V1.9 已经写过的本机服务相同，不是代理。

## 凭据检查（只查新增的两个文件）

对 `07_live-20261120T122849Z/report.json` 与 `summary.md` 检索 `authorization`、`bearer`、`api[_-]?key`、`sk-`、`7897`、`postgres://`、`postgresql://`、`password`、`passwd`、`secret`、私钥头、`proxy`、`eyJ`。密钥、令牌值、代理地址与 DSN 是 0。命中如下：

| 命中 | 次数 | 是什么 |
|---|---|---|
| `api_key` | report.json 1 | `"key_from_env_var": "IDP_BRANT_MODEL_API_KEY"`。环境变量的名字。值不在文件里 |
| `sk-` | report.json 13，summary.md 3 | `TSK-INV-001`、`TSK-INV-002`、`SM-TSK-TASK` 里的字母 `SK-` |
| `7897` | report.json 1 | 落在一条时间戳的小数秒里，不是端口 |
| `token` 这个词 | report.json 139 | 模型用量字段和说明文字。`bearer` 是 0，`eyJ` 是 0，没有 JWT |
| `127.0.0.1:8080` | report.json 1 | 本机模型服务，与上一节相同，不是代理 |

`authorization`、`bearer`、`postgres://`、`postgresql://`、`password`、`passwd`、`secret`、`proxy`、私钥头在这两个文件里是 0。summary.md 没有 `api_key`，也没有 `7897`。
