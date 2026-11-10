# IDP Brant · idp-bento-go 消费者基线 V0.1（候选）

日期：2026-11-06。状态：候选，尚无 Brant Go 代码消费该框架。

## 版本与来源

| 项目 | 值 |
|---|---|
| Module | go.idp.xyz/idp-bento-go |
| 版本 | v0.1.0-rc.2（RC，不是稳定版） |
| 本机位置 | ~/go/pkg/mod/go.idp.xyz/idp-bento-go@v0.1.0-rc.2 |
| 包 | api、application、compat、domain、eventing、infra、internal、postgres（含 inbox、outbox、migrations）、repository、testkit |
| 参考消费者 | IDP-Cargo go_cargo（main 0900ec8）：Repository、Outbox、Inbox 三套 testkit 合同共 13 个子场景在 GitHub Actions 上通过 |

## 已核对的框架行为

| 行为 | 依据 | 对 Brant 的影响 |
|---|---|---|
| WithinTransaction 发现同一 owner 的事务时加入外层事务 | postgres/transaction.go | 同一上下文内的嵌套调用自然合并 |
| owner 不同则返回 transactionConflict | postgres/transaction.go | 跨上下文事务内读取被框架禁止，直接影响 ADR-BR-004 |
| 每次 NewDB 生成新的 owner | Cargo 仓库结构基线 V1.1 第 9 章 | bootstrap 为每个上下文调用一次 NewDB（规则 T1） |
| 默认隔离级别 READ COMMITTED | postgres/transaction.go（pgx.TxOptions） | 不能依赖 SQLite 式全库串行，需显式行锁（ADR-BR-011） |
| Outbox 租约按 worker 墙上时钟计算 | Cargo V2.9 报告 | brant-worker 主机须时钟同步；批量×单次发布超时须小于租约 |

## Brant 的消费规则

1. go.mod 精确引用 v0.1.0-rc.2，go.sum 为完整依赖图；生产构建禁止 replace。
2. 每个上下文一个 bentopg.DB；共享一个 pgxpool。
3. 聚合写入、命令回执、提交证据与 Outbox 写入在同一事务提交。
4. 在每个上下文的 internal/integration 中绑定 Repository、Outbox、Inbox testkit 合同。
5. 资格守卫读取（若 ADR-BR-004 选择方案 B）由下游在自身事务中以自身 executor 调用上游发布的守卫函数（12 阶段 GR1–GR7），不经上游上下文的 DB owner；M0-10 已用框架的公开接口验证可行，不修改框架源码。

## 待核实

- Inbox 消费者与 ErrCommitUncertain 恢复在 Brant 场景中的用法（Cargo 也尚未完成）。
- vanity 路由 go.idp.xyz/idp-brant 是否可解析（Cargo 曾发现 vanity 主机对任意路径返回 idp-bento-go 的 go-import）。
