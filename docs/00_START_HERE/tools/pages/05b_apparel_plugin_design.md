---
output: 05_TACTICAL_DDD/07_IDP_Brant_Apparel_Industry_Plugin_Detailed_Design_V0.1.html
title: IDP Brant 服装行业插件（industry.apparel）详细设计说明书 V0.1
short: 服装行业插件详细设计 V0.1
kicker: IDP BRANT / 05 TACTICAL DDD / APPAREL INDUSTRY PLUGIN DETAILED DESIGN V0.1
doc_title: 服装行业插件（industry.apparel）详细设计 V0.1
doc_desc: trade-extension.v1 扩展点底座 · CAT 款色码与样衣批准 · CST 分码单耗 BOM、MCQ 起染费与替代审批 · 大货短溢装容差 · QLT/FUL 质检配比与装柜测算 · AI 行业契约
meta: 版本=V0.1 Design Candidate; 日期=2026-11-24; 代码基线=main 80e94b5 / 8066e1c; 依据=ADR-BR-008 · 基线第 2.3/5/8.8/15.5/18 章; 证据层级=E0 编码前设计文档
stage: 05 · 战术 DDD
eyebrow: APPAREL PLUGIN DESIGN
search: 搜索 ADR-BR-EXT、SampleApproval、CostBOM、分码单耗、MCQ、短溢装、装柜、错误码或用例
foot: Apparel Industry Plugin Detailed Design V0.1 · 2026-11-24 · E0 Pre-Implementation
---
<span class="scope-badge">DETAILED DESIGN V0.1 (E0 PRE-IMPLEMENTATION) · ADR-BR-008 · BASELINE 8066e1c · NO GO CODE CHANGED</span>

<h2>以扩展点而非第 19 个上下文的方式，在确定性核价、守卫锁序与 AI 契约下交付服装行业包</h2>

<p class="lede">本文件是 <b>IDP Brant 服装行业插件（<code>industry.apparel</code>，版本 <code>0.1.0</code>，契约 <code>trade-extension.v1</code>）</b>及其前置能力的编码前详细设计说明书。设计严格依据《产品与研发范围基线 V1.0》第 2.3、5.1—5.8、8.8、15.5、18.1—18.4 章、附录 B Schema 与 <code>ADR-BR-008</code>，并对照 SAP Fashion 与中国服装外贸实战要求逐项对齐 <code>main</code> 分支当前代码（<code>8066e1c</code> / <code>80e94b5</code>）：保留 18 个限界上下文的单写者权威与无环守卫锁拓扑，通过平台扩展底座（<code>brant_ext</code>）与四个上下文的显式扩展点，完整补齐产品能力目录 <code>F13</code>（款色码矩阵、样衣批准、分码单耗 CostBOM、面料起染费 MCQ 与打样版费、替代物料审批、大货短溢装容差与增量信用守卫、服装质检参数、尺码装箱配比及箱规装柜测算）及对应 AI 行业契约。</p>

::: cover
- 设计依据 | `ADR-BR-008`（行业插件是扩展点而非限界上下文）、`ADR-BR-004` 方案 B（事务内守卫 `FOR SHARE`）、`ADR-BR-011`（确定性锁序）及产品基线第 5/8.8/15.5/18 章。
- 文档状态 | **E0 编码前详细设计候选**。本版不修改任何 Go、SQL 或既有规范 JSON 文件；待评审通过后按第 10 章四切片路线依次进入 R0 设计源更新与代码实现。
- 边界声明 | 本设计覆盖软件插件与前置聚合的完整闭环；**不包含**排料／裁床优化、完整 PLM 打样工单流、车间排产 MRP/MES 或公网第三方插件沙箱市场；BIP 已永久退出路线，不在任何桥接范围内。
:::

{{app_kpis}}

> [!warn] 编码前纪律声明（Design-First Discipline）
> 1. **本文件尚未对应已实现代码**：文中的 `brant_ext`、`Item`、`SampleApproval`、`CostBOM`、10 个 SQL 迁移、13 条新命令、13 个错误码与 `internal/plugins/apparel` 均为**待实施设计规格**，不代表当前 `8066e1c` 已支持这些路由或行为。
> 2. **既有规范 JSON 在编码前保持不变**：为保持当前主干 `make check`、`build_archive.py` 与 `02_go_source_manifest.txt`（1,369 个 Go 文件）完全一致，设计源 JSON 变更清单统一汇总在第 08 章与第 10 章的「切片 A · P1-R0」中，随首波代码一并落地。
> 3. **业务默认值待服装角色签核**：色批是否强制同缸发货、缩水与配比容差默认值、哪类样衣阶段阻断正式化或出运等属于客户／行业政策参数，本设计全部做成公司级可配置策略，商业上线前须经真实服装业务角色在 G0/G3 签核。

## 01 设计目标、核心架构裁决与范围边界 {#s1}

### 1.1 为什么必须先建底座与两项前置，再落 `industry.apparel`

对照产品基线 2.3 与 5.1 的依赖图，`industry.apparel` 显式声明了三项强依赖：`requires: ["trade.core", "samples", "cost_bom"]` 以及一项可选桥接：`optional_bridges: ["apparel.crm.bridge"]`。核查当前 `main`（`8066e1c`）代码现状：

- **`trade.core` 已完备**：`TRD`、`INQ`、`SUP`、`CST`、`CAT`（SKU 与规格修订）、`PUR`、`QLT`、`FUL`、`STL`、`CNX`、`STK`、`MFG`、`CLM`、`CRM`、`TSK` 与 `internal/ai` 全部已在 PostgreSQL 16 上通过 CI。
- **三块前置尚未实现**：
  1. 平台级 `trade-extension.v1` 插件注册、公司激活开关与单据级 `document_profile` 防混单底座尚未建立；
  2. 战术模型中 `CAT-AGG-Item`（商品款式）与 `CAT-AGG-SampleApproval`（`sample.approve`）仍为「轮廓聚合，待详细设计」，`brant_cat` 尚无对应表与 `SampleGuard v1`；
  3. `CST` 目前仅有通用 `CostScenario` 与 `CostSnapshot`，尚无版本化 `CostBOM`（面辅料单耗、幅宽、克重、缩水、损耗）与替代物料审批模型。

### 1.2 七项核心设计裁决（ADR-BR-EXT-001—007）

{{app_adrs}}

### 1.3 明确支持与明确不做边界

| 类别 | 本设计涵盖范围（`industry.apparel 0.1.0`） | 明确不做（禁止通过字段预留宣称支持） |
|---|---|---|
| 商品与规格（`CAT`） | 款式 `Item` → 颜色 × 尺码矩阵 → 绑定已确认交易 `SKU`；面料幅宽（cm）、克重（GSM）、缩水率（%）、色批／缸号策略；严格区分 `measured=true` 实测值与预估值 | 不替换现有 `SkuRef` 主键语义；不做完整 PLM 版单管理或自动生成未知 SKU |
| 样衣批准（`CAT`） | 样衣送审草稿、`sample.approve`（人工专属）、`sample.revoke`（人工专属）；强绑定 `(sku_id, spec_revision)` 与样衣阶段（`proto`/`fit`/`pp_sample`/`shipment`）；`SampleGuard v1` | 不做外部打样工厂协同门户或自动免除技术版本升版后的重新确认 |
| 成本与用料（`CST`） | 版本化 `CostBOM`（支持**统一基准码**与**按尺码分档单耗 `size_graded`**）、基于幅宽／克重／缩水／损耗的确定性估算（`cost.estimate.v1`）、**按色汇总面料起染量附加费（MCQ / Dye Lot Surcharge）与打样／版费摊销**、替代面辅料独立审批 | **不做自动排料、裁床算料优化或自有车间工单在制核算**；替代绝不倒写已发料／已加工／已交付事实 |
| 报价、订单与容差（`TRD`/`INQ`） | `document_profile`（`generic` \| `apparel`）防混单；`quote.validate.v1` 合并校验；**大货短溢装容差（总容差 ±3% ＋ 单色单码 ±5%，溢装增量须过 `STL` 信用守卫）**；`Q01.industry` 接入 `SampleGuard v1` 与 `CostBOMGuard v1` | 不支持单据内跨行业模板逐行混单，不支持不同模板订单合并出运（基线 2.3 后置） |
| 质检、装箱与装柜（`QLT`/`FUL`） | 服装检验扩展参数（色差、水洗缩水、尺寸公差、色批／缸号）；装箱尺码配比（Assortment / Ratio Pack）、同色批约束、**箱规长宽高 CBM／毛净重推算与 20GP/40GP/40HQ 装柜容积载重校核** | 不做 3D 拼柜排箱算法，也不以插件检验参数替代核心 `QualityReleaseGuard v1` 或五重商业放行闸门 |
| 轻 CRM 桥接与 AI | `CRM` 开启时条件加载 `apparel.crm.bridge`，`CRM` 关闭时不加载且不反向强制安装 CRM；对接附录 B 五项 AI 行业契约与 `ExecutionBundle` 指纹 | 不允许第三方公网任意上传动态加载代码；不宣称同进程硬沙箱；**不涉及 BIP** |

## 02 运行时架构、分层依赖与 trade-extension.v1 契约 {#s2}

{{app_arch_diagram fig="FIG 01"}}

### 2.1 Go 仓库目录规划与 `dependency-check` 规则修订

为严格遵守仓库六边形分层规则，新增两个代码位置并在 `tools/dependency-check/main.go` 中引入 `layerPlugin` 分类：

```text
internal/
├── foundation/
│   └── plugin/                    # layerFoundation：trade-extension.v1 契约、注册表、brant_ext 激活仓储与 PluginGuard v1
│       ├── manifest.go            # PluginManifest（对齐附录 B）、精确版本组合矩阵
│       ├── ports.go               # CostProvider, QuoteValidator, PackingValidator, InspectionValidator, AI 行业端口
│       ├── registry.go            # 进程内注册表（冲突检测 COST_PROVIDER_CONFLICT、同名检查冲突拒绝）
│       ├── activation.go          # plugin.enable / plugin.enter_readonly / plugin.disable 应用服务
│       └── postgres/              # brant_ext 仓储与 PluginGuard v1 调用封装
└── plugins/
    └── apparel/                   # 新增 layerPlugin（须在 .github/CODEOWNERS 登记一行）
        ├── manifest.go            # industry.apparel 0.1.0 清单与能力声明
        ├── cost_provider.go       # cost.estimate.v1（纯函数：幅宽/克重/缩水/损耗，仅用 kernel.Decimal/Money）
        ├── quote_validator.go     # quote.validate.v1（款色码矩阵、样衣版本、混单、S-D 校验）
        ├── qlt_ful_hooks.go       # 服装质检参数校验 ＋ FUL 尺码配比（Assortment）校验
        ├── ai_contracts.go        # industry.semantics.v1 / context.provider.v1 / task.profile.v1 / outcome.check.v1
        └── crm_bridge.go          # 可选 apparel.crm.bridge（仅当 bootstrap CRM != nil 时装配）
```

**`tools/dependency-check` 新增硬性静态规则（编译期守住边界）**：
1. `layerFoundation`（含 `internal/foundation/plugin`）：只允许导入外部模块、`internal/kernel` 与其他 `internal/foundation/*`，**禁止导入任何 `internal/contexts/*` 或 `internal/plugins/*`**。
2. `layerPlugin`（`internal/plugins/<name>`）：只允许导入 Go 标准库、`internal/kernel`、`internal/foundation/plugin` 及各上下文的公共 `api/` 包（`catapi`、`cstapi`、`trdapi`、`inqapi`、`qltapi`、`fulapi`、`polapi`、`evdapi`、`crmapi`），**禁止导入任何上下文的 `internal/`、禁止导入 `bentopg` 或 `pgx`、禁止导入 `net/http` 客户端发外网请求**。
3. `layerContext`（18 个上下文）：**禁止任何上下文导入 `internal/plugins/*`**；上下文只能依赖 `internal/foundation/plugin` 中定义的中立扩展点接口（由 `internal/bootstrap` 在启动期注入实现），彻底杜绝核心与服装包的循环依赖。
4. `layerBootstrap`（`internal/bootstrap`）：唯一允许同时导入 `internal/contexts/*`、`internal/foundation/plugin` 与 `internal/plugins/apparel` 的组合根。

### 2.2 `PluginManifest` 与平台核心扩展点接口（Go 契约定义）

`industry.apparel` 的清单严格遵守产品基线 5.1 与附录 B 的 `PluginManifest` 闭环结构（禁止未声明字段）：

```json
{
  "id": "industry.apparel",
  "version": "0.1.0",
  "contract_version": "trade-extension.v1",
  "requires": [
    { "id": "trade.core", "contract_version": "brant.trd/0.1.0" },
    { "id": "samples",    "contract_version": "brant.cat/0.1.0" },
    { "id": "cost_bom",   "contract_version": "brant.cst/0.1.0" }
  ],
  "optional_bridges": ["apparel.crm.bridge"],
  "capabilities": [
    "cost.estimate.v1",
    "quote.validate.v1",
    "packing.validate.v1",
    "inspection.validate.v1",
    "industry.semantics.v1",
    "context.provider.v1",
    "task.profile.v1",
    "outcome.check.v1"
  ],
  "activation": { "scope": "company", "document_profile": "apparel" },
  "task_profile_ref": "task.profile.apparel.q01@0.1.0",
  "publisher_id": "vendor.official"
}
```

> 说明：基线 5.1 中的 `ui.extension.v1` 属于前端工作台扩展声明；由于本仓库仅交付 Go 后端与 OpenAPI 契约，首期服务端清单不虚报 `ui.extension.v1`，待前端工作台立项时由前端按同一 `document_profile` 协议对接。

`internal/foundation/plugin/ports.go` 核心 Go 接口设计（全部同步方法为纯计算、无外部 I/O、无数据库事务）：

```go
package plugin

import (
    "context"
    "go.idp.xyz/idp-brant/internal/kernel"
)

// DocumentProfile bounds a single document's industry template (generic or apparel).
type DocumentProfile string

const (
    ProfileGeneric DocumentProfile = "generic"
    ProfileApparel DocumentProfile = "apparel"
)

// Verdict follows baseline 5.3 merge precedence: reject > requires_approval > pass.
type Verdict string

const (
    VerdictPass             Verdict = "pass"
    VerdictRequiresApproval Verdict = "requires_approval"
    VerdictReject           Verdict = "reject"
)

type Finding struct {
    RuleID          string // e.g. "APP-RULE-SAMPLE-CURRENT"
    Verdict         Verdict
    Code            string // registered error code, e.g. SAMPLE_SPEC_MISMATCH
    Path            string // JSON pointer, e.g. "/args/data/lines/0/sku_ref"
    Message         string
    Waivable        bool   // NEVER true for core permissions, money, stock, or party errors
    ApprovalScope   string // explicit scope if VerdictRequiresApproval && Waivable
    EvidenceIDs     []string
}

// MergeFindings computes the aggregate verdict while keeping EVERY finding (order-independent).
func MergeFindings(findings []Finding) Verdict { ... }

// CostProvider implements cost.estimate.v1. Pure deterministic math (kernel.Decimal / kernel.Money).
// Never sets final selling price; never performs DB or network I/O.
type CostProvider interface {
    PluginID() string
    DocumentProfile() DocumentProfile
    Estimate(in CostEstimateInput) (CostEstimateOutput, error)
}

// QuoteValidator implements quote.validate.v1. Called inside TRD freeze/activate after core checks.
type QuoteValidator interface {
    PluginID() string
    DocumentProfile() DocumentProfile
    ValidateQuote(in QuoteValidationInput) ([]Finding, error)
}

// PackingValidator & InspectionValidator implement FUL ratio-pack and QLT apparel param rules.
type PackingValidator interface {
    PluginID() string
    DocumentProfile() DocumentProfile
    ValidatePacking(in PackingValidationInput) ([]Finding, error)
}

type InspectionValidator interface {
    PluginID() string
    DocumentProfile() DocumentProfile
    ValidateInspection(in InspectionValidationInput) ([]Finding, error)
}
```

## 03 前置能力一：CAT 款色码矩阵与样衣批准（Item & SampleApproval） {#s3}

{{app_er_diagram fig="FIG 02"}}

### 3.1 `CAT-AGG-Item`：商品款式与款-色-码-SKU 矩阵

现有 `brant_cat.sku` 已经作为全部 18 个上下文的最小可交易商品单元（被 `INQ`、`SUP`、`CST`、`TRD`、`PUR`、`STK`、`MFG`、`FUL`、`CNX` 通过 `SkuRef(sku_id, spec_revision)` 广泛引用）。因此，**服装款色码设计绝不破坏或替换 `brant_cat.sku`**，而是在 `CAT` 内向上补齐 `Item`（款式）聚合，将「款式 × 颜色 × 尺码」确定性映射到已确认的 `brant_cat.sku`：

- **聚合身份**：`(tenant_id, company_id, item_id)`，携带不可变 `document_profile`（`generic` | `apparel`）、唯一业务款号 `style_code`（公司内唯一，冲突拒 `BUSINESS_KEY_CONFLICT`）与版本号 `root_version` / `revision`。
- **款色码矩阵表 `brant_cat.item_sku_variant`**：
  - 复合唯一键 `(tenant_id, company_id, item_id, revision, color_code, size_code)` 与 `(tenant_id, company_id, item_id, revision, sku_id)`，保证同一款式修订内「一个色码组合唯一对应一个 `sku_id`」，且同一 `sku_id` 不可被映射到两个不同款色码上。
  - 外键直指 `brant_cat.sku_revision(tenant_id, company_id, sku_id, spec_revision)`：建立或确认款式修订时，每个格子的 SKU 必须已存在且为当前确认版（`CAT-INV-001`）。
- **服装技术参数（基线 5.8 · 严格区分实测与预估）**：
  - 结构化列保存：面料主成分与规格、有效幅宽 `fabric_width_cm`（`kernel.Decimal` 精度 6 位）、克重 `fabric_gsm`（`kernel.Decimal`）、经向／纬向缩水率 `warp_shrinkage_pct` / `weft_shrinkage_pct`、色批／缸号约束 `dye_lot_policy`（`none` | `same_lot_per_color` | `same_lot_per_order`）、尺码表引用 `size_chart_evidence_id`。
  - **防伪造实测铁律**：每项工艺参数显式携带 `measured boolean NOT NULL` 与 `source_evidence_id`；`measured = false` 表示估算值，在 `cost.estimate.v1` 和 `quote.validate.v1` 中绝不能冒充实测值（若公司策略要求实测才能正式化，返回 `BOM_UNVERIFIED` / `requires_approval`）。

### 3.2 `CAT-AGG-SampleApproval`：样衣批准状态机与 `SampleGuard v1`

`SampleApproval` 补齐 `BC-AT23` 与基线 15.5 `S-D` 规定的核心行业风控：**“样衣确认绑定旧技术文件版本（如 `TECH-01-r1`），当 SKU 规格修订升至 `r2` 时，旧样衣批准自动失效，绝不能用来证明新版本条件已满足。”**

- **聚合身份与不可变版本**：`(tenant_id, company_id, sample_approval_id)`，主表保存 `sku_id`、当前状态 `status`（`draft` | `approved` | `superseded` | `revoked`）、`row_version` 与 `current_revision`；子表 `sample_approval_revision` 不可变记录每次批准的 `spec_revision`、`sample_stage`（`proto` 产前初样 | `fit` 尺寸样 | `pp_sample` 产前封样 | `shipment_sample` 船头大货样）、`tech_pack_sha256`、`evidence_ids[]`、`approved_by_id`（必须 `human`）与 `authority_epoch`。
- **权限与职责控制**：
  - `sample.save_draft`：人工或 AI 均可发起送审登记；
  - `sample.approve`：**人工专属（`authorization.HumanOnly`）**，已预登记在 `quality_inspector` 岗位模板（`roles_gen.go`），且须独立于草稿提交人（或满足公司独立确认规则），批准时强制校验所引 `sku_id` 的 `spec_revision` 为当前确认版且 `evidence_ids` 均在 `EVD` 中对审批人可见；
  - `sample.revoke`：**人工专属**（`quality_inspector`），记录撤销原因，立即使 `SampleGuard v1` 返回不通过。
- **事务内守卫函数 `SampleGuard v1`**（严格遵循 `ADR-BR-004` 方案 B，与既有 `brant_cat.guard_sku_spec_current_v1` 并列）：

```sql
CREATE FUNCTION brant_cat.guard_sample_approved_v1(
    p_sku_id        text,
    p_spec_revision bigint,
    p_stage         text
) RETURNS TABLE (
    known                   boolean,
    approved                boolean,
    sample_approval_id      text,
    approved_spec_revision  bigint,
    code                    text
) LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, brant_cat AS $$
-- 1. 在调用方作用域 (app.tenant_id, app.company_id) 下先对 brant_cat.sku 行加 FOR SHARE 锁；
-- 2. 对匹配 (sku_id, stage) 的最新 sample_approval 主行加 FOR SHARE 锁（阻止并发 sample.revoke 或新审批提交窗口）；
-- 3. 判定逻辑：
--    - SKU 不存在或停用           -> code = 'REFERENCE_INVALID'
--    - 无有效 approved 样衣       -> code = 'SAMPLE_APPROVAL_REQUIRED'
--    - 有 approved 样衣，但 approved_spec_revision <> p_spec_revision
--      （例：样衣绑 r1，订单要求 r2） -> code = 'SAMPLE_SPEC_MISMATCH'
--    - 完全匹配且处于 approved     -> approved = true, code = ''
$$;
```

## 04 前置能力二：CST 成本 BOM、用料核算与替代物料审批 {#s4}

### 4.1 `CST-AGG-CostBOM`：版本化面辅料成本结构

`CostBOM` 归属 `CST`（`brant_cst`），为服装与定制采购提供可复现、可追溯的物料用量与成本计算来源，并与现有 `CST` 场景评估（`CostScenario`）和冻结快照（`CostSnapshot`）无缝衔接：

- **聚合结构**：
  - `brant_cst.cost_bom`（根表）：`(tenant_id, company_id, bom_id)`，绑定目标 `item_id` 或 `(sku_id, spec_revision)`、`document_profile`、`base_size_code`（基准尺码，如 `M`）、`status`（`draft` | `verified` | `superseded` | `withdrawn`）、`root_version`、`active_revision`。
  - `brant_cst.cost_bom_revision`（不可变修订）：固化 `content_sha256`、`verified_by_id`（必须独立于编制人的 `costing` 人工角色）、`authority_epoch`、`evidence_ids[]`。
  - `brant_cst.cost_bom_line`（面辅料明细行，全数值列为高精度十进制字符串／numeric，**禁用 float**）：
    - `line_id`、`material_kind`（`shell_fabric` 主面料 | `lining` 里料 | `interlining` 衬布 | `trim` 辅料 | `packaging` 包装 | `labor_cm` 加工费）
    - `material_code`、`unit`（如 `M`、`KG`、`PCS`）、`grading_mode`（`uniform` 统一基准码 | `size_graded` 按尺码分档）、`consumption_per_unit`（基准码单件净耗，6 位小数）
    - `wastage_ratio`（工艺损耗率，8 位小数，如 `0.03` 表示 3%）、`shrinkage_factor`（缩水补偿系数，如经纬缩水折算后的 `1.04`）
    - `fabric_width_cm`、`fabric_gsm`、`measured`（实测标记）与 `formula_basis`（计算依据标识：`direct_qty` | `width_length_m` | `gsm_weight_kg`）
    - `supply_ref`（可选引用 `SUP` 已核实供应报价版本，保证单价有据可查）。
  - `brant_cst.cost_bom_size_grade`（**按尺码分档单耗子表**，同属 `cst/000004`，`FORCE RLS`）：
    - 主键 `(tenant_id, company_id, bom_id, revision, line_id, size_code)`；登记各尺码或尺码档（如 `S`/`M` `1.45 M`、`L`/`XL` `1.60 M`、`2XL`/`3XL` `1.78 M`）的精确净耗 `consumption_per_unit`（6 位小数）与可选尺码加工加价 `size_cm_delta_minor`（解决欧美大码订单按单码均值算亏的行业痛点）。
  - `brant_cst.cost_bom_surcharge`（**面料起染费 MCQ 与打样／版费子表**，同属 `cst/000004`，`FORCE RLS`）：
    - 登记按色汇总触发的染厂起染量阈值（`mcq_threshold` ＋ `mcq_unit`）与起染附加费（`surcharge_kind`: `dye_lot_mcq` | `trim_moq_surcharge` | `sample_dev_fee` | `pattern_fee`）、归属范围（`per_color` | `per_style` | `per_order`）、固定金额或分级单价（`kernel.Money`）、摊销口径（`order_lump_sum` 整单列支 | `allocate_by_qty` 按件最大余数法分摊到对应色行）以及关联 `POL fee_category` 费目。

### 4.2 确定性服装用料、分码单耗与 MCQ／版费核算公式（`cost.estimate.v1`）

`internal/plugins/apparel/cost_provider.go` 全部使用 `internal/kernel` 的精确有理数（`big.Rat`，中间精度 ≥ 34 位有效数字）计算每条 BOM 行的有效单耗与标准成本组件，只在行金额出口按币种最小单位做一次 `HALF_UP` 舍入：

1. **尺码单耗查找（严禁静默回退）**：
   - 当 `grading_mode = "uniform"` 时取 `cost_bom_line.consumption_per_unit`；
   - 当 `grading_mode = "size_graded"` 时，按订单行 SKU 对应的 `size_code` 精确查找 `cost_bom_size_grade`；**若某尺码未命中分档，立即拒绝 `422 BOM_SIZE_GRADE_MISSING`，绝不静默回退基准码**。
2. **按米计价主面料（`width_length_m`）**：
   `有效单耗 (M/件) = 对应尺码净耗 × (基准幅宽 / 实际有效幅宽) × 缩水补偿系数 × (1 + 损耗率)`
3. **按公斤计价针织面料（`gsm_weight_kg`）**：
   `有效单耗 (KG/件) = (对应尺码用布面积 m²/件 × 克重 g/m² / 1000) × 缩水补偿系数 × (1 + 损耗率)`
4. **辅料／包装／加工费（`direct_qty`）**：
   `有效单耗 = 对应尺码净耗 × (1 + 损耗率)`（若含 `size_cm_delta_minor`，单独累加至加工费组件）。
5. **行标准材料成本**：
   `行总用量 = 订单行数量（已换算至基础单位 PCS）× 有效单耗`；`行标准成本（最小单位）= RoundHalfUp(行总用量 × 供应单价)`。
6. **分色汇总起染量（MCQ）与打样／版费核算**：
   - 按 `(material_code, color_code)` 聚合同一颜色下全部尺码的真实总用布量 `color_total_usage = Σ 行总用量`；
   - 若 `color_total_usage < mcq_threshold`，自动生成显式成本组件 `Kind = "surcharge"`（保留触发颜色、实际用量与阈值证据），并按最大余数法分摊到该颜色的订单行（保证分摊之和与附加费逐分守恒）；同一 `obligation_id` 绝不重复计费（遵守 T02 §6）；
   - 若供应商／BOM 声明了 `mcq_threshold` 但未明确起染费金额，拒绝 **`CHARGE_SCOPE_UNRESOLVED`**，绝不按 `0` 静默放行；
   - 按款／按单登记的 `sample_dev_fee` 与 `pattern_fee` 作为显式一次性成本组件进入 `CostScenario`，并在冻结快照中固定分摊方式。
   - **重要约束**：若任意必需工艺参数（幅宽／克重／缩水）为 `measured=false` 且公司政策开启「正式报价必须实测」，或引用了 `draft`/`withdrawn` 的 BOM，`cost.estimate.v1` 必须返回 `adoptable = false` 与 `BOM_UNVERIFIED`，**绝不静默产出可报价成本**；且 `cost.estimate.v1` 永不直接写出最终售价。

### 4.3 替代面辅料变更审批与不可倒写原则（基线 5.8）

生产或寻源阶段常因面料缺缸、辅料停产需要替代物料。`brant_cst.cost_bom_substitution` 强制落实基线 5.8：

- **显式变更记录**：`cost_bom.substitution.request` 必须登记 `original_line_id` / `original_material_code`、`substitute_material_code`、`applicable_quantity`、`target_spec_revision`、`cost_delta_minor`、`lead_days_delta`、`sample_impact`（是否要求重新确认样衣）与 `reason`。
- **独立审批闭环**：`cost_bom.substitution.decide` 为**人工专属**，审批人独立于申请人；若 `sample_impact = true`，批准后自动要求目标 `spec_revision` 具备新的 `SampleApproval`；批准通过后原子生成新的 `cost_bom_revision`（原修订进入 `superseded` 永久保留）。
- **绝不倒写已发生事实**：替代审批只作用于新修订和尚未执行的后续单据，**严禁原地修改已发料（`STK`/`MFG` `material.handover`）、已加工（`MFG` 委外单）及已交付（`PUR`/`FUL`）的历史事实**。
- **`CostBOMGuard v1`**（`brant_cst.guard_cost_bom_current_v1`）：在 `CST` 冻结快照与 `TRD` 冻结／激活报价的事务中以 `FOR SHARE` 锁住 `cost_bom` 根行，确保引用的 BOM 修订仍为 `verified` 当前版且不存在未决阻断替代。

## 05 固定计价链、防混单控制与 QLT／FUL 服装扩展 {#s5}

{{app_pricing_diagram fig="FIG 03"}}

### 5.1 单据级 `document_profile` 与防混单硬规则

- **单据归属字段**：`INQ`（`rfq.save_draft`）、`CST`（`costing_context.save_draft` / `cost_bom.save_draft`）、`TRD`（`quote.save_draft` / `order.prepare`）与 `FUL`（`shipment.prepare`）新增可选输入字段 `document_profile`（默认省略即 `"generic"`，保持对既有全部 Q01 与 S1—S6 测试 100% 向后兼容；传入 `"apparel"` 时，应用层前置校验 `brant_ext.guard_plugin_active_v1('industry.apparel', 'apparel')` 处于 `active`）。
- **三处强制防混单关卡（全部返回 `422 DOCUMENT_PROFILE_MISMATCH` 并给出拆单提示）**：
  1. **询价／报价行内混单**：`document_profile = apparel` 的询价或报价中若引用了属于 `generic` 的 `Item`（或反之），直接拒绝；
  2. **上下游单据错配**：`TRD` 报价的 `document_profile` 必须与其所引 `INQ` 询价及 `CST` 成本快照完全一致；
  3. **跨模板合并出运**：`FUL` `shipment.prepare` 若试图将 `generic` 订单与 `apparel` 订单合并进同一票出运，直接拒绝（基线 2.3）。

### 5.2 校验合并规则与 `quote.validate.v1` 服装规则集

在 `TRD` 的 `quote.freeze` 与 `quote.activate` 中，核心不变量（权限、合法金额、币种汇率、主体筛查、信用／合规、库存）永远最先执行且**不可被业务审批豁免**；随后调用绑定到 `document_profile` 的 `QuoteValidator.ValidateQuote`，结果按基线 5.3 合并：

- **严格优先级**：`reject` > `requires_approval` > `pass`；**全部 findings 均完整保留并随冻结快照落库**，展示排序不影响业务结论。
- **`industry.apparel` 首期内置规则集**：
  - `APP-VAL-01`（`reject`）：所有行 SKU 必须属于已确认的 `apparel` 款色码矩阵且单位换算精确合法（锁住 `2 DOZEN = 24 PCS` 反例）。
  - `APP-VAL-02`（`reject`）：报价行数量必须与所引 `INQ` 询价的核实矩阵完全一致，源文件总数冲突未决时不可冻结。
  - `APP-VAL-03`（`reject` 或 `requires_approval`，按公司政策）：涉及 `SampleApproval` 强制阶段时，校验当前 `spec_revision` 的有效样衣批准；旧版本样衣返回 `reject`（`SAMPLE_SPEC_MISMATCH`）。
  - `APP-VAL-04`（`reject`）：`CostBOM` 含未审批替代物料（`BOM_SUBSTITUTION_UNAPPROVED`）或强制实测参数为 `measured=false`（`BOM_UNVERIFIED`）。
  - `APP-VAL-05`（`requires_approval`，`Waivable=true`）：色批容差放宽或特殊尺码加价豁免（须显式内容绑定审批，注明规则 ID、快照哈希与插件版本）。

### 5.3 大货短溢装容差与溢装增量信用守卫（Delivery Tolerance · `ADR-BR-EXT-007`）

纺织服装大货裁片与整烫天然存在小幅短装或溢装（行业合同通常约定“整单 ±3%，单色单码 ±5%”）。对标 SAP SD/MM 的 Delivery Tolerance，设计在不破坏既有不变量的前提下引入显式合同容差：

- **合同级声明（`migrations/trd/000013`）**：`quote_revision` 与 `sales_order` 增加可选结构 `delivery_tolerance { short_pct, over_pct, per_line_short_pct, per_line_over_pct }`（`kernel.Decimal` 8 位精度比例，上限硬限 ≤ `0.20`）。`generic` 单据默认全 `0`（保持既有严格不超量行为 100% 不变）；`apparel` 单据按客户合同约定写入并在商业哈希（`business_hash`）与审批中绑定。
- **出运上下限守卫（`FUL` 调用 `OrderGuard v1`）**：
  - 单行累计已确认出运上限：`max_line_ship = floor(ordered_line_qty × (1 + per_line_over_pct))`；
  - 整单累计已确认出运上限：`max_order_ship = floor(total_ordered_qty × (1 + over_pct))`；
  - 任意一笔 `shipment.prepare` 或 `shipment.confirm` 若使累计量超过上述任一上限，立即拒绝 **`409 DELIVERY_TOLERANCE_EXCEEDED`**。
- **溢装增量信用守卫（严防借溢装绕过信用控制）**：对于含出运后收款条款的赊销订单，若本次出运触发了超出原订购量的溢装金额增量 `over_amount_minor > 0`，`FUL shipment.confirm` 在调用 `STL PaymentConditionGuard` 时必须校验客户信用额度覆盖该溢装增量（否则拒绝放行或要求非销售的信用经理显式批准，绝不允许借 +5% 溢装悄悄突破信用额）。
- **短装容差结案与 `TRD-INV-018` 兼容**：当每行与整单累计出运均达到短装下限 `ceil(ordered_qty × (1 - short_pct))` 时，允许按「容差内完成」正常结案并按实出数量在 `STL` 结算；低于短装下限仍走既有短交关闭流程；生效订单变更仍严格遵守 `TRD-INV-018`（变更后数量 ≥ 已确认出运量）。

### 5.4 `QLT` 服装质检参数、`FUL` 尺码装箱配比与箱规装柜测算

- **`QLT` 扩展（`migrations/qlt/000004_qlt_apparel_inspection_params.sql`）**：
  - 在 `inspection.record` 请求中增加可选的 `apparel_checks` 结构化数组（仅当所检采购承诺／委外单归属 `apparel` 订单时接受，否则拒 `DOCUMENT_PROFILE_MISMATCH`）：记录**色差灰卡等级（`color_fastness_grade`）**、**水洗经／纬缩水实测（`measured_warp_shrinkage_pct` / `measured_weft_shrinkage_pct`）**、**关键部位尺寸公差判定（`measurement_tolerance_pass`）**与**色批／缸号（`dye_lot_no`）**，每项绑定检验报告证据文件。
  - `InspectionValidator` 校验实测值是否在规格公差内；若 `dye_lot_policy = same_lot_per_color` 且同色出现混缸且无让步处置（`disposition.decide` `concession`），拒绝放行。
- **`FUL` 尺码配比与箱规装柜测算（`migrations/ful/000012_ful_document_profile_and_size_ratio.sql`）**：
  - 在 `packing.plan` 与 `packing.record_actual` 中支持服装**尺码装箱配比规则（Assortment / Ratio Pack）**与**单箱物理规格（Carton Spec）**：
    - 配比定义：单箱混码比例（如 `S:M:L = 1:2:1`，单箱 `24` 件）或单色单码独箱、尾箱规则（`allow_remainder_carton`）、以及可选同箱同缸（`dye_lot_no`）校验。
    - 箱规与装柜参数：单箱外径 `length_cm` × `width_cm` × `height_cm`、`net_weight_kg`、`gross_weight_kg`（强制 `gross >= net`），以及可选目标集装箱型 `container_type`（`20GP` | `40GP` | `40HQ` | `LCL`，柜型有效净容积与限重基准取自公司 `POL` 配置，不写死魔法数）。
  - `PackingValidator` 使用 `kernel.Decimal` 精确执行四项校验：
    1. **件数与配比守恒**：每箱内各尺码件数严格符合配比比例，箱数 × 单箱件数（含显式声明尾箱）与出运总件数严格相等，否则拒 **`422 PACKING_RATIO_MISMATCH`**；
    2. **箱规 CBM 与毛净重推算核对**：`单箱体积 (CBM) = (L × W × H) / 1,000,000`，`总 CBM = Σ(箱数 × 单箱体积)`，`总毛重 = Σ(箱数 × 单箱毛重)`；若请求手填的 `Package.VolumeCbm` / `GrossWeightKg` / `NetWeightKg` 与箱规推算值超出允许舍入差，拒绝 `422 PACKING_RATIO_MISMATCH`（防手填错小数点导致订舱爆仓）；
    3. **集装箱容积与载重校核**：指定整柜类型时，计算精确体积利用率（%）与载重利用率（%）；若总 CBM 超出目标柜型配置净容积（如 `40HQ` 常用净容积 `68.000 CBM`）或总毛重超限重上限，拒绝 **`422 CONTAINER_CAPACITY_EXCEEDED`** 并返回需拆柜／升级柜型提示；
    4. **核价前置复用**：经核实的箱规规格可同步供 `CST MeasurementSnapshot` 引用，使海运按方／按柜费用分摊与实际出运单证保持同一口径。

## 06 数据库 Schema、PostgreSQL 16 迁移与无环锁序设计 {#s6}

所有新增表严格遵循仓库迁移规范：**永不编辑已应用的 106 个迁移文件**；新文件连续编号无跳号；每张新表均启用 `FORCE ROW LEVEL SECURITY`（按 `app.tenant_id` 与 `app.company_id` 过滤）；应用账号 `brant_app` 的最小授权统一追加至 `deploy/postgres/10_app_grants.sql`。

### 6.1 新增 SQL 迁移清单（共 10 个迁移文件）

{{app_migrations}}

### 6.2 `migrations/manifest/contexts.txt` 拓扑顺序与锁序证明

- **Manifest 插入位置**：新增目录 `ext` 插入到 `ref` 之后、`evd` 之前（顺序变为 `auth` → `msg` → `ref` → **`ext`** → `evd` → `pty` → `cat` → `pol` → `inq` → `sup` → `cst` → `trd` → `pur` → `qlt` → `stl` → `cnx` → `stk` → `mfg` → `ful` → `clm` → `crm` → `tsk` → `ai`）。`brant_ext` 不调用任何业务上下文的守卫，因此处于全部业务上下文上游，零循环依赖。
- **扩展后的确定性锁序（`ADR-BR-011`）**：以最复杂的 `TRD quote.freeze`（`document_profile=apparel`）为例，同一 PostgreSQL 事务内严格按以下固定顺序获取行锁：
  1. 本聚合根 `brant_trd.quote_root` **`FOR UPDATE`**；
  2. 权限守卫 `brant_auth` **`FOR SHARE`**（`Authorizer.Authorize`）；
  3. 平台插件守卫 `brant_ext.guard_plugin_active_v1` **`FOR SHARE`**（确认 `industry.apparel` 处于 `active` 且未在并发事务中被切入 `draining_readonly`）；
  4. `brant_cat` **`FOR SHARE`**：先按 `(sku_id, spec_revision)` 字典序调 `SkuGuard v1`，再按 `(sku_id, stage)` 字典序调 `SampleGuard v1`；
  5. `brant_cst` **`FOR SHARE`**：先调 `AdoptabilityGuard v1`，再调 `CostBOMGuard v1`；
  6. 依次调 `brant_inq`（`RfqGuard v1`）→ `brant_pol`（`ConfigGuard v1`）→ `brant_pty`（`TradingProfileGuard v1` / `AddressRoleGuard v1`）**`FOR SHARE`**。
- **死锁免疫性**：所有上游守卫严格沿 Context Map DAG 拓扑单向向下调用，且同类对象按主键字典序排序加锁；`FUL shipment.confirm` 在检查 `apparel` 溢装容差增量信用时，仍严格遵守既有 `FUL` 锁序（`OrderGuard v1` 读取容差 → `PaymentConditionGuard` 校验增量信用），不引入任何反向锁边；既有 `build_archive.py` 守卫锁环检测（后续更新至 29 条守卫锁边）继续保持**严格无环**。

## 07 插件生命周期、停用在途保护与 AI 行业契约 {#s7}

{{app_lifecycle_diagram fig="FIG 04"}}

### 7.1 插件版本兼容矩阵、升级与停用在途保护（基线 5.5 / 18.3）

1. **精确发布组合验证**：首期插件随经过测试的 Go 二进制一同构建发布，平台内置已验证的精确兼容三元组登记表 `(core_version, contract_version, plugin_version)`。`plugin.enable` 与进程启动校验精确匹配，**绝不凭语义版本号（如 `^0.1`）猜测兼容性**；不匹配即拒 `409 PLUGIN_VERSION_INCOMPATIBLE`。
2. **三态生命周期（`disabled` → `active` → `draining_readonly` → `disabled`）**：
   - `plugin.enable`（`administrator`，人工专属）：仅从 `disabled` 启用。校验 `requires` 依赖上下文全部就绪，校验该公司在 `document_profile=apparel` 上无第二个成本提供者（否则 `409 COST_PROVIDER_CONFLICT`），写入 `brant_ext.plugin_activation(status='active')`。
   - `plugin.enter_readonly`（`administrator`，人工专属）：仅从 `active` 切入 `draining_readonly`——阻止新建任何 `apparel` 询价、草稿与 AI 任务，但允许已在途的报价审批、订单履约、出运与结算继续完结。
   - `plugin.disable`（`administrator`，人工专属）：**仅从 `draining_readonly` 执行**。该状态下守卫拒绝一切新建 apparel 单据，因此 `Prepare` 读到的在途集合只会缩小，不会在加锁前被新单据扩大。从 `active` 直接调用拒绝 `INVALID_STATE`，并要求管理员先执行 `plugin.enter_readonly`；缺失或已是 `disabled` 同样拒绝 `INVALID_STATE`（插件不在排空）。在 `draining_readonly` 下若 `INQ`/`TRD`/`FUL`/`TSK` 仍有未终态 apparel 单据或运行中任务，硬拒绝 `409 PLUGIN_IN_USE`。确认无在途后方可置为 `disabled`。
3. **停用后历史可读**：即使 `industry.apparel` 处于 `disabled`，历史已完成报价与订单仍完整保存其 `CostSnapshot` 与 `plugin_snapshot`（含规则版本与 findings），通过受支持的只读视图原样展示，不丢失任何财务或审计记录。

### 7.2 对齐附录 B 的五项 AI 行业契约与 `ExecutionBundle` 指纹

- **`industry.semantics.v1`（`Semantics`）**：声明服装领域术语与别名（款号 `style`、色号 `color`、尺码 `size`、打 `DOZEN`→`12 PCS` 换算、产前样 `pp_sample`、幅宽／克重／缩水等）及其 `EvidenceRef` 来源。映射字段仅允许引用平台白名单注册的纯转换函数（如 `uom.convert_dozen_to_pcs`），**禁止嵌入任意脚本或动态表达式**。
- **`context.provider.v1`（`ContextResult`）**：供 `brant-worker -agent` 在组装上下文时只读查询已授权的款式矩阵、最新 `CostBOM` 与 `SampleApproval` 状态；结果强制标注 `freshness`（`current` | `stale` | `conflict` | `unconfirmed`），**“无资料”必须显式标为 `unconfirmed`，绝不等价于条件已满足**。
- **`task.profile.v1`（`TaskProfile`）**：注册 `task.profile.apparel.q01@0.1.0`，声明支持目标（`quote_ready`、`order_active`）、允许动作子集（严禁超出平台委托与 128 人工专属红线）、行业必需检查清单、版本化指令摘要 `instruction_sha256` 与 `max_steps`。
- **`action.provider.v1`（`ActionRequest`）**：服装专属草稿动作（`item.save_draft`、`sample.save_draft`、`cost_bom.save_draft`）若开放给 AI，全部经由现有进程内 `internal/ai/gateway` 与核心命令账本执行，服从同一委托、预算、幂等与 F2 写路由拦截规则。
- **`outcome.check.v1`（`CheckResult`）与 `Q01.industry` 升级**：
  - 平台将**公共必需断言**与**行业必需断言**取并集：`TRD GoalReadiness` 的 `Q01.industry` 断言对 `document_profile=apparel` 的报价／订单同步执行 `SkuGuard v1` ∩ `SampleGuard v1` ∩ `CostBOMGuard v1` ∩ 服装规则校验。
  - **不可降级铁律**：检查项是否 `required` 由平台注册表决定，**插件返回 `required=false` 绝对不能取消平台必检**；若任一必检返回 `unknown` 或 `error`（如依赖不可达），`TSK` 将任务置入 `waiting_external` 而非 `completed`；自然语言声称“样衣已OK”彻底无效。
- **`ExecutionBundle` 插件指纹绑定（`ADR-BR-EXT-006`）**：在 `migrations/ai/000006_ai_bundle_plugins.sql` 中为 `ExecutionBundle` 增加 `plugins_json`（按 `id` 排序的 `{id, version, contract_version, manifest_sha256}`），参与 bundle 规范化 SHA-256。一旦服装插件从 `0.1.0` 升级至 `0.2.0`，在途 AI 任务在下一次轮询时自动触发 `EXECUTION_BUNDLE_INCOMPATIBLE`（`CT14`），0 模型调用安全暂停等待人工接管或显式迁移。

## 08 命令、HTTP 路由、权限与错误码变更清单（R0 设计源规格） {#s8}

进入编码前（切片 A · `P1-R0`），以下清单将同步写入主档案设计源 JSON（Context Map、Tactical Model、Role Templates、`error_catalog.json`）并通过 `make allowlist`、`go run ./tools/rolegen` 与 `go run ./tools/openapigen` 自动生成白名单、角色代码与 OpenAPI 3.1.0 文档。

### 8.1 新增命令、HTTP 路由与权限矩阵（支持实时过滤）

> 所有新增写路由自动继承 F2 传输硬拦截：`actor.kind == ai` 访问任一 `POST /api/brant/v1/.../commands/...` 在读请求体前直接返回 `403 PERMISSION_DENIED`；每个写命令自动配对 `POST .../reconcile` 幂等对账路由。

{{app_commands}}

### 8.2 新增只读路由清单

| 上下文 | 读取路由（`GET`） | 权限 | 返回内容 |
|---|---|---|---|
| `EXT` | `GET /api/brant/v1/ext/plugins` | `ext.read` | 当前公司所有已登记插件清单、精确版本与激活态（`disabled`/`active`/`draining_readonly`） |
| `CAT` | `GET /api/brant/v1/cat/items/{id}` | `cat.read` | 商品款式、色码-SKU 矩阵与服装技术参数（含 `measured` 标记与证据引用） |
| `CAT` | `GET /api/brant/v1/cat/sample_approvals/{id}` | `cat.read` | 样衣批准详情、所绑 `spec_revision`、当前效力与历史撤销记录 |
| `CST` | `GET /api/brant/v1/cst/cost_boms/{id}` | `cst.read` | 版本化成本 BOM 明细、单耗公式依据、实测标记与替代物料变更历史 |

### 8.3 新增错误码登记清单（`error_catalog.json` 待追加）

{{app_errors}}

## 09 可证伪验证矩阵（覆盖基线 15.5 S-D 与 18.3 全部强制用例） {#s9}

产品基线 18.3 明确列出了 9 项插件必测验证点，基线 15.5 给出了服装与单位反例 `S-D`。下表将每一项规范要求拆解为可执行的自动化测试用例组（支持实时过滤）：

{{app_tests}}

## 10 四切片实施路线、单元规划与待业务确认项 {#s10}

### 10.1 增量四切片开发计划（正式映射至 WBS `S7-01`—`S7-04`）

按 Brant 既有纵向切片纪律（每次合入主干须过 `CI=true make check`、PG16 `make db && make integration SHARDS=4` 与 GitHub Actions），本设计的四个实施切片已正式登记在 [工程实施总体计划 · S7 阶段](../09_IMPLEMENTATION_PLAN/01_IDP_Brant_Implementation_Master_Plan_Go_V0.1.html#s9b) 的 **`S7-01`—`S7-04`** 中（后续紧接 `S7-05` 拼柜分摊、`S7-06` 前端工作台、`S7-07` 标准邮件协同、`S7-08` 总账凭证导出与 `S7-09` 压测／大模型收口）：

{{app_slices}}

### 10.2 待真实服装业务角色确认的参数清单（不阻塞软件开发）

以下参数在代码中全部实现为**公司级版本化配置（`POL` / `Item` 策略）**，以合成样例默认值完成全量自动化验证；在进入真实服装客户 B0 试点（G0 / G3）前，由业务与质检负责人书面确认即可，无需修改代码：

| 编号 | 待确认业务口径 | 软件内默认处理（可配置） | 确认责任角色 |
|---|---|---|---|
| `APP-D01` | 报价正式化前强制哪一阶段样衣批准（`proto`／`fit`／`pp_sample`）？ | 按公司策略开关；合成测试默认 `pp_sample` 阻断订单生效与 `Q01.industry` | 服装业务负责人 ＋ 质检负责人 |
| `APP-D02` | 同色服装是否强制同色批／同缸发货（`same_lot_per_color`）？ | 默认允许配置 `none` / `same_lot_per_color` / `same_lot_per_order`，混缸须 `QLT` 让步处置 | 客户代表 ＋ 质检负责人 |
| `APP-D03` | 正式报价是否禁止含 `measured=false`（估算幅宽／克重／缩水）的 BOM？ | 默认开启硬阻断 `BOM_UNVERIFIED`，可按公司政策配置为 `requires_approval` 例外放行 | 核价负责人 ＋ 财务 |
| `APP-D04` | 服装尺码配比装箱（Ratio Pack）是否允许尾箱非整数倍散箱？ | 默认支持显式声明尾箱规则（`allow_remainder_carton`），未声明则按严格整比校验 | 单证 ＋ 跟单负责人 |
| `APP-D05` | 服装大货默认短溢装容差比例（总容差／单色单码容差）及溢装增量信用放行规则？ | 默认 `generic = 0%`；`apparel` 支持合同逐单配置（典型 `±3% / ±5%`，系统硬上限 `20%`），溢装增量强制过 `STL` 信用校验 | 销售 ＋ 信用经理 ＋ 财务 |
| `APP-D06` | `20GP`/`40GP`/`40HQ` 净容积与载重上限基准、以及起染费 MCQ 对应的 `POL fee_category`？ | 全部纳入公司级 `POL` 配置版本化管理（示例默认 `40HQ = 68 CBM / 26,000 KG`，可按船公司或客户调整） | 物流／单证 ＋ 核价负责人 |

### 10.3 关联主档案索引

- 战略架构依据：[战略 DDD 与 Context Map（ADR-BR-008）](../03_STRATEGIC_DDD/02_IDP_Brant_Strategic_DDD_Context_Map_V0.1.html)
- 既有战术模型（含轮廓聚合）：[战术 DDD 主文件](01_IDP_Brant_DDD_Domain_Design_Master_V0.1.html)
- 产品基线原文（第 5/8.8/15.5/18 章与附录 B）：[产品与研发范围基线 V1.0](../01_PRODUCT_BLUEPRINT/01_IDP_Brant_Product_Development_Scope_Baseline_V1.0.html)
- 完整产品说明与发布规格：[产品说明 V1.0](../01_PRODUCT_BLUEPRINT/05_IDP_Brant_Product_Manual_V1.0.html) · [R1 发布规格与 G5 材料 V0.1](../09_IMPLEMENTATION_PLAN/16_IDP_Brant_R1_Release_Specification_and_G5_Package_V0.1.html)
- 仓库分层与依赖检查规范：[Go 仓库结构基线 V1.0](../12_GO_FIRST_ALIGNMENT/02_IDP_Brant_Repository_Structure_Baseline_V1.0.html)
