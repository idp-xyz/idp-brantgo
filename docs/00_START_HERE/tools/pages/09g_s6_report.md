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
- 结论 | 六项 DONE_CI，S6-06 PARTIAL。本机尖端门禁：迁移 0（106 个迁移已记录），集成 0（795 = 208+193+184+210，SHARDS=4，墙钟 8 分 04 秒）。远程：07601c8 的两次运行成功。同日 main 的合并提交 8066e1c（拉取请求 #12）推送运行 45161081163 成功。三项后续行为在该提交。现场评测与参照负载跑在 1264d61，见第 05 章。
- 证据 | 15_S6_EVIDENCE：日期、环境、源文件 SHA-256 快照、本机尖端门禁、GitHub Actions 运行的编号与地址、现场评测第三次运行的 summary.md 与 report.json、两次参照负载的 summary.md 与 report.json。不复制 trace/、进程日志或密钥。
- 未执行 | 9e2502a 的本机门禁没有跑 make race、make ai-eval-live、make eer-live。8066e1c 没有另跑三项后续的本机合并集成。真实海关单一窗口、真实金税、真实客户、真人签核仍没有。远程工作流里的竞态分片已成功，那不是本机的 make race。
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

这些说明不改上表的状态。订单变更超过报价上限、委外没有累计上限、渠道命令没有 HTTP，这三句在 8066e1c 上已经不再成立，见第 05 章。其余仍是实现时写下的非阻断说明。

| 项 | 说明 |
|---|---|
| S6-07 | 不是 multi_order 的草稿如果带上行上限，保存时拒绝（INVALID_KNOWN_VALUE）。封存时对非 multi_order 清空 max_order_quantity 与 uncapped，使省略这些成员的文档与从前逐字节相同。8066e1c 起，multi_order 报价上的订单变更增加量不得超过该行剩余上限。TRD-INV-018 仍是订单相对已出运 |
| S6-01 | 并发出库与直发不虚构收发在 e2e 测试 TestS6E2EStockAndDirectShip 里。库存关闭时，库存路线的出运准备是 DEPENDENCY_NOT_SUPPORTED。直发确认不增加库存移动，也不调用库存守卫 |
| S6-02 | 8066e1c 起，发出时先锁协作计划。未取消的发出数量不得超过工序数量；引用了承诺时也不得超过 Prepare 里读到的需求数量。交接货号不是计划上的材料时，在库存移动之前拒绝。制造默认关闭。进度仍是来源声称，不写出库存、不给出质量放行、不做出运 |
| S6-03 | 退货是 CLM 的事实，不改原出运，也不在 R1 写库存。同一补救的第二次贷项是 CLAIM_REMEDY_ALREADY_SETTLED。未结索赔使 order_result.close 因索赔维被拒；结案后该维恢复满足 |
| S6-04 | 零值 APIConfig 会把 CRM 组装为开启。更早一次把 CRM 关掉的 ContextsWithOptions 组装的是另一套上下文，不改变 platform.API 造出的处理器。关闭 HTTP 须设置 APIConfig.DisableCRM。brant-api 传入关闭标志 |
| S6-05 | 超时是 unknown，命令成功，结果不是成功。过期回执写入 channel_receipt，authoritative 为假，案件不回退。没有拨号，没有真实单一窗口，没有真实金税。8066e1c 起，申报与退税的五条渠道命令有 HTTP，结算不是路由 |

S6 没有 Python V0.50 的对等测试。Q01 的 order.prepare、提示词与工具列表不因 order.prepare_partial 而改变。

## 04 本机门禁与未执行 {#s4}

本页记录的尖端门禁是 2026-11-23 在作者的 WSL Ubuntu 24.04 上、对尖端 9e2502a 的本机运行。数据库是 PostgreSQL 16 容器 brant-pg16，端口 55471。编写本页的档案单元没有重跑这次集成，也没有把进程日志复制进 15_S6_EVIDENCE。

| 门禁 | 结果 |
|---|---|
| 迁移 | 0。106 个迁移已经记录，没有新应用 |
| 集成 | 0。795 个 bootstrap 测试各一次，SHARDS=4，分片 208+193+184+210。墙钟 8 分 04 秒 |
| 回归提交 | 95e011a，合并为 9e2502a。它只增加集成测试，不改生产代码 |

回归覆盖六段：部分下单（1,200 中先 700 再 500，再 1 被拒；AI 令牌在读请求体之前是 PERMISSION_DENIED）、库存路线的 FIFO 成本与直发不虚构收发、制造委外与交接、索赔贷项与关闭门、CRM 开与关、渠道的 unknown 与过期回执。

远程工作流在 07601c8 上跑了竞态分片并且成功。本机没有另跑 `make race`。下面这些是 `9e2502a` 那次门禁没有执行的项目，也不把它们记成那次门禁的通过。现场评测与参照负载是同日更后的另两次测量，见第 05 章。

- 本机的 make race
- 那次门禁中的 make ai-eval-live
- make eer-live
- 真实海关单一窗口
- 真实金税
- 真实客户任务
- 真人签核

档案单元自己的门禁是两次档案构建与一次 CI=true make check。那次 check 的退出状态记在单元报告里，日志不进本目录。

## 05 同日后续：三项行为、远程运行、现场评测与负载 {#s5}

`main` 的合并提交是 `8066e1c`，父提交是 `23ed630` 与 `5eee844`。`5eee844` 在 `s6/integrate` 上合入下表三项。本机没有把这三项再跑一遍合并后的集成。远程门禁是推送运行 [45161081163](https://github.com/idp-xyz/idp-brantgo/actions/runs/45161081163)，工作流 Brant M0 / PG16 / Bento RC2，16 项作业成功，含静态检查、bootstrap 六片、竞态六片、脚本化 Q01、其余包与 `brant-ci`。作业日志不进证据目录。这次运行里的脚本化 Q01 不是现场模型评测。

| 后续 | 单元提交 | 在 8066e1c 上的行为 |
|---|---|---|
| S6-07 报价上限 | 51b9d41 | multi_order 报价上，订单变更的增加量不得超过该行剩余上限，否则 `QUOTE_REMAINING_INSUFFICIENT`。增加量是新数量减去本单已经占用的数量。减少与取消不退回数量。不是 multi_order 的报价不走这条 |
| S6-02 委外上限 | 750d1de | 发出时先对协作计划 `FOR UPDATE`。未取消的发出数量不得超过工序数量；引用了承诺时也不得超过 Prepare 里读到的需求数量。交接的货号不是计划上的材料时，在库存移动之前以 `REFERENCE_INVALID` 拒绝，路径 `/args/sku_id` |
| S6-05 渠道 HTTP | 7dd2604 | 五条命令是 `declaration_case.channel_submit`、`declaration_case.channel_ingest`、`declaration_case.channel_reconcile`、`tax_refund_case.channel_submit`、`tax_refund_case.channel_reconcile`，路径在 `POST /api/brant/v1/cnx/commands/`。权限仍是 `record_submission`、`record_receipt`、`record_result`。结算不是路由 |

七项的状态不因这三项改变。S6-06 仍是 PARTIAL。G5 仍是模拟 NO-GO。BIP 不在这次记录里。

现场评测的产品提交是 `1264d61`，不是 `8066e1c`。模型服务是 `http://127.0.0.1:8080`，模型 `qwen3.8-flash-next-iq3_xxs`。密钥只在环境变量 `IDP_BRANT_MODEL_API_KEY`，不写入本档案。`CI` 没有设置。三次运行。前两次被中断，没有 `summary.md`。没有 `trial.json` 的试验不计。第三次 `live-20261123T110243Z` 从 2026-11-23T11:02:43Z 到 11:31:30Z，退出 0。它的 `summary.md` 与 `report.json` 在 `07_live-20261123T110243Z/`。那份摘要只含它自己跑的试验（`an05` 1 次、`an06` 4 次）：续跑没有重跑已经落盘的试验，用的是工作区里未提交的 `-only`。下表是三次运行里每个试验号一份 `trial.json` 的合计。数据库容器 `brant-pg16-s6live` 已删除。

| 家族 | 结果 |
|---|---|
| normal | 失败。4/5。`normal-2` 停在 `REFERENCE_INVALID`，路径 `/args/data/legal_terms_ref` |
| an01 | 失败。0/5。`DRAFT_NOT_READY`，路径 `/args/unresolved_fields` |
| an03 | 失败。0/5。`an03-1` 与 `an03-2` 的生效修订应付是 960.00 USD，家族要求 948.00 USD。`an03-3` 与 `an03-4` 是 `VERSION_CONFLICT`。`an03-5` 在修订阶段没有 `quote.save_draft` |
| an04 | 通过。5/5。`paused/conflict` |
| an05 | 失败。0/5。`order-crash` 都在 25 分钟阶段上限后 `paused/budget_exhausted` |
| an06 | 通过。5/5。`an06-1` 来自第二次运行，其余来自第三次。都是 `completed/ai_with_required_review` |
| an07 | 通过。5/5。`waiting_input`。`quote.activate` 被拒绝 `REFERENCE_STALE` |
| an08 | NOT APPLICABLE。脚本族：现场模型不会按脚本去失败那些调用。不是通过，也不是失败 |
| an09 | 通过。5/5。`paused/budget_exhausted`。脚本要求的 `VERSION_CONFLICT` 拒绝次数没有套到现场模型上 |
| an10 | 通过。5/5。`waiting_input`。AI 令牌走普通写路由被拒绝 `PERMISSION_DENIED`。脚本要求的注入调用次数没有套到现场模型上 |
| an11 | 通过。5/5。`waiting_input` |
| ct14 | 通过。5/5。`paused/conflict`，没有模型调用 |
| reads | NOT APPLICABLE。脚本族。不是通过，也不是失败 |
| feedback | NOT APPLICABLE。脚本族。不是通过，也不是失败 |

没有执行，也不是失败：AN12（真实脱敏询价，BC-D01 之前）、AN02（不在固定集合）、需要负责人在 HTTP 上操作的那一次。没有一次试验是因为模型服务不让跑而记成未执行。

参照负载的产品提交也是 `1264d61`。阶段 16 是 16 个命令用户和 16 个读用户。5 秒的中继 P95 目标没有改，也没有为此改 Go。

| worker | 证据 | 中继 P95 | 中继 P99 | 结论 |
|---|---|---|---|---|
| 2 | `08_load-20261123T075618Z` | 13.011 秒 | 24.744 秒 | 未达到 5 秒 |
| 3 | `08_load-20261123T075819Z` | 6.403 秒 | 16.293 秒 | 未达到 5 秒 |

两次都只复制 `summary.md` 与 `report.json`。进程日志没有复制。容器 `brant-pg16-s6load` 已删除。

{{s6_evidence}}

> [!decision] 本版不改行为
> 之前：六项为 DONE_LOCAL，因为 9e2502a 的远程运行还没有结论。之后：同一天 07601c8 的两次 GitHub Actions 运行成功，六项改为 DONE_CI。S6-06 仍为 PARTIAL，G5 仍为模拟 NO-GO。命令、收据、迁移、岗位权限与既有测试的期望都不变。S1 至 S5R 的状态不变。BIP 不在这次记录里。

> [!decision] 三项后续改了行为，七项状态不变
> 之前：第 03 章把订单变更超过报价上限、委外没有累计上限、渠道命令没有 HTTP 记成当时的缺口。之后：这三句在 8066e1c 上不再成立，见第 05 章。S6-06 仍为 PARTIAL，G5 仍为模拟 NO-GO。S1 至 S5R 的状态不变。BIP 不在这次记录里。
