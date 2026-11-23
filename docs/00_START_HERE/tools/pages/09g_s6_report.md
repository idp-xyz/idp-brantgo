---
output: 09_IMPLEMENTATION_PLAN/14_IDP_Brant_S6_Execution_Report_V0.1.html
title: IDP Brant S6 R1 完整首发执行报告 V0.1
short: S6 执行报告 V0.1
kicker: IDP BRANT / 09 IMPLEMENTATION PLAN / S6 EXECUTION REPORT
doc_title: S6 R1 完整首发执行报告 V0.1
doc_desc: 部分与多次下单 · 库存台账与库存路线 · 生产协同与委外 · 索赔与退货 · 可选 CRM · 进程内渠道 · 本机回归 · 模拟 G5 NO-GO
meta: 版本=V0.1; 日期=2026-11-23; 证据=E2 本机执行与 GitHub Actions; 结论=S6 六项 DONE_CI · S6-06 PARTIAL · G5 NO-GO（模拟）
stage: 09 · 工程实施总体计划
eyebrow: S6 EXECUTION REPORT
search: 搜索工作项、合并、缺口或门禁
foot: S6 Execution Report V0.1 · 2026-11-23
---
<span class="scope-badge">S6 DONE_CI · S6-06 PARTIAL · G5 NO-GO</span>

<h2>六项工作在本机与 07601c8 的远程运行上通过；发布门禁仍是模拟 NO-GO</h2>

<p class="lede">2026-11-23，S6 记在集成分支 s6/business。本机回归的尖端是 9e2502a，e2e 合并的父提交是 b662ea5。档案提交 07601c8 已推送。同一提交上，推送运行 45129948206 与拉取请求运行 45131658394 都成功。拉取请求 #10 已合并，main 上的合并提交是 1264d61；1264d61 自己的运行不作为本页的证据。本版不改产品行为。S6-07、S6-01、S6-02、S6-03、S6-04、S6-05 为 DONE_CI。S6-06 为 PARTIAL。G5 仍是模拟 NO-GO。</p>

::: cover
- 结论 | 六项 DONE_CI，S6-06 PARTIAL。本机尖端门禁：迁移 0（106 个迁移已记录），集成 0（795 = 208+193+184+210，SHARDS=4，墙钟 8 分 04 秒）。远程：07601c8 的两次运行成功。
- 证据 | 15_S6_EVIDENCE：日期、环境、源文件 SHA-256 快照、本机尖端门禁、两次 GitHub Actions 运行的编号与地址。不复制 trace/、进程日志或密钥。
- 未执行 | 本机的 make race、make ai-eval-live、make eer-live、真实海关单一窗口、真实金税、真实客户、真人签核。远程工作流里的竞态分片已成功，那不是本机的 make race。
:::

## 01 七项工作与合并 {#s1}

状态说明：DONE_CI 表示本机 PostgreSQL 16 与 07601c8 上一次已结束的 GitHub Actions 运行都通过。PARTIAL 表示发布条件没有齐。下表的合并提交都在 s6/business 上。设计源修订 R0（单元提交 4a0eb07，合并 ebafb48）在这七项之前，基线是档案 V1.10 的 776431b；R0 本身不是这七项里的一项。

| 项 | 状态 | 单元提交 | 合并 | 这一项交付了什么 |
|---|---|---|---|---|
| S6-07 | DONE_CI | 3dbb052 | 9821c96 | 参照报价的部分与多次下单。报价可在冻结时显式选择 multi_order；人工专属命令 order.prepare_partial 按行占用剩余数量；并发只有一笔能占用超出剩余的数量 |
| S6-01 | DONE_CI | c626f0f；0065726 | f934a71；b828290 | 库存台账（FIFO、预留、公司开关、StockAvailability v1）以及 FUL 的库存路线。直发不写库存移动。STL 按库存成本层给库存路线计价 |
| S6-02 | DONE_CI | aeaeced | e1c2600 | 协作计划、委外单与材料交接。制造默认关闭。进度是来源声称，不写出库存、不给出质量放行、不做出运 |
| S6-03 | DONE_CI | 8a402c4 | 6c3a633 | 索赔、退货、补救与结案。退货不改原出运。一笔已批准的补救只记一次贷项。未结索赔使经营结果的索赔维不满足，关闭被拒。接线在 b828290 |
| S6-04 | DONE_CI | 54f2451 | 175c3c9 | 可选 CRM 与 BC-AT24。关闭时省略商机的询价仍能走完；带上商机则拒绝。零值 APIConfig 把 CRM 组装为开启；brant-api 传入关闭标志 |
| S6-05 | DONE_CI | 7a28f27 | b662ea5 | 进程内渠道端口与假适配器。超时记为 unknown，不是成功。过期回执记下来，不推进案件。没有真实海关单一窗口，也没有真实金税 |
| S6-06 | PARTIAL | 95e011a | 9e2502a | 经 HTTP 的回归锁住上面六段行为。795 个 bootstrap 测试，SHARDS=4，墙钟 8 分 04 秒（2026-11-23）。发布条件没有齐，见第 02 章 |

{{wbs_tasks phase=S6}}

合并顺序（新的在后）：ebafb48（R0）→ f934a71（STK）→ 9821c96（TRD）→ 6c3a633（CLM）→ 175c3c9（CRM，第一波结束）→ e1c2600（MFG）→ b828290（库存路线与贷项接线）→ b662ea5（渠道，第二波结束）→ 9e2502a（回归）。9e2502a 的父提交是 b662ea5。

各单元在自己的 PostgreSQL 16 容器上跑过本机 make check 与 make integration（CI=true，SHARDS=4）。那些容器不是本页的尖端门禁。尖端门禁用的是 brant-pg16、端口 55471，数字见第 04 章。

## 02 G5 是模拟 NO-GO {#s2}

G5 的通过条件是公开支持矩阵、授权、部署组合、服务边界、完整回归、实际发布指标与支持机制。2026-11-23 把 G5 记为 NO-GO（模拟）。评审没有召开。缺的是：

| 发布条件 | 本档案里的情况 |
|---|---|
| 真实客户 | 没有。测试用合成数据与本地签发者 |
| 真人签核 | 没有 |
| 实际发布指标与支持机制 | 没有 |
| 真实海关单一窗口或金税 | 没有。S6-05 只有进程内假适配器 |
| 该尖端的远程运行 | 07601c8 的推送运行 45129948206 与拉取请求运行 45131658394 都成功。这不构成发布指标，G5 仍是模拟 NO-GO |

S6-06 因此是 PARTIAL：回归在本机通过，发布门禁没有通过。G0、G1、G2、G3、G4 的结论不因这一章改变。G2、G3、G4 仍是此前的模拟 NO-GO。

> [!warn] 模拟
> 这一条 NO-GO 是档案按缺席的发布条件写下的结论。没有人召开过 G5 评审，也没有人在本页上签字。

## 03 已接受的缺口 {#s3}

这些缺口不改上表的状态。它们是实现时已经写明、本版照录的非阻断说明。

| 项 | 说明 |
|---|---|
| S6-07 | 不是 multi_order 的草稿如果带上行上限，保存时拒绝（INVALID_KNOWN_VALUE）。封存时对非 multi_order 清空 max_order_quantity 与 uncapped，使省略这些成员的文档与从前逐字节相同。订单变更提高数量时不写入消耗台账，因此可以超过报价上限。TRD-INV-018 仍是订单相对已出运 |
| S6-01 | 并发出库与直发不虚构收发在 e2e 测试 TestS6E2EStockAndDirectShip 里。库存关闭时，库存路线的出运准备是 DEPENDENCY_NOT_SUPPORTED。直发确认不增加库存移动，也不调用库存守卫 |
| S6-02 | 没有委外数量相对工序或需求的累计上限，同一上限可以被第二次发出再引用。发出时对计划的读取不加锁（R23 的 guardLock 为假）。交接的货号不与计划的材料核对 |
| S6-03 | 退货是 CLM 的事实，不改原出运，也不在 R1 写库存。同一补救的第二次贷项是 CLAIM_REMEDY_ALREADY_SETTLED。未结索赔使 order_result.close 因索赔维被拒；结案后该维恢复满足 |
| S6-04 | 零值 APIConfig 会把 CRM 组装为开启。更早一次把 CRM 关掉的 ContextsWithOptions 组装的是另一套上下文，不改变 platform.API 造出的处理器。关闭 HTTP 须设置 APIConfig.DisableCRM。brant-api 传入关闭标志 |
| S6-05 | 超时是 unknown，命令成功，结果不是成功。过期回执写入 channel_receipt，authoritative 为假，案件不回退。没有拨号，没有真实单一窗口，没有真实金税，渠道命令没有新的 HTTP 路由 |

S6 没有 Python V0.50 的对等测试。Q01 的 order.prepare、提示词与工具列表不因 order.prepare_partial 而改变。

## 04 本机门禁与未执行 {#s4}

本页记录的尖端门禁是 2026-11-23 在作者的 WSL Ubuntu 24.04 上、对尖端 9e2502a 的本机运行。数据库是 PostgreSQL 16 容器 brant-pg16，端口 55471。编写本页的档案单元没有重跑这次集成，也没有把进程日志复制进 15_S6_EVIDENCE。

| 门禁 | 结果 |
|---|---|
| 迁移 | 0。106 个迁移已经记录，没有新应用 |
| 集成 | 0。795 个 bootstrap 测试各一次，SHARDS=4，分片 208+193+184+210。墙钟 8 分 04 秒 |
| 回归提交 | 95e011a，合并为 9e2502a。它只增加集成测试，不改生产代码 |

回归覆盖六段：部分下单（1,200 中先 700 再 500，再 1 被拒；AI 令牌在读请求体之前是 PERMISSION_DENIED）、库存路线的 FIFO 成本与直发不虚构收发、制造委外与交接、索赔贷项与关闭门、CRM 开与关、渠道的 unknown 与过期回执。

远程工作流在 07601c8 上跑了竞态分片并且成功。本机没有另跑 `make race`。下面这些仍然没有执行，也不记为通过：

- 本机的 make race
- make ai-eval-live
- make eer-live
- 真实海关单一窗口
- 真实金税
- 真实客户任务
- 真人签核

档案单元自己的门禁是两次档案构建与一次 CI=true make check。那次 check 的退出状态记在单元报告里，日志不进本目录。

{{s6_evidence}}

> [!decision] 本版不改行为
> 之前：六项为 DONE_LOCAL，因为 9e2502a 的远程运行还没有结论。之后：同一天 07601c8 的两次 GitHub Actions 运行成功，六项改为 DONE_CI。S6-06 仍为 PARTIAL，G5 仍为模拟 NO-GO。命令、收据、迁移、岗位权限与既有测试的期望都不变。S1 至 S5R 的状态不变。BIP 不在这次记录里。
