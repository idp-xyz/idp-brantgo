# IDP Brant 核价输入与成本快照契约规格 V0.3.1

日期：2026-10-22。状态：审查后修订候选规格，尚未发布为运行时契约。文件沿用原入口名称，正文版本V0.3.1；V0.1严格审查及V0.2复审报告保留各自历史结论。产品范围保持中国外贸版V1.0。

## 1. 目标与适用边界

将基础数据补齐矩阵V0.2中的第一批要求落实为T02输入与计算规则，衔接既有业务核心详细设计V0.2。覆盖询价核价上下文、供应报价、可复用采购条件、物流报价、同口径比较、费用归集及不可变成本快照。供应报价核实不等于采购承诺，成本冻结不等于对客报价生效。

本文件不修改现有brant.trade-core/0.1.0与brant.foundation/0.1.0。拟新增候选契约brant.costing/0.1.0；正式发布须完成封闭JSON Schema、DDL迁移、命令实现及验证。本文件定义逻辑字段和计算语义，不冒充已生成的机器契约。既有73项测试仅证明原实现，本轮未运行新业务测试。

本阶段以原有PCS询价及已核实的SKU计数换算为首个实现路径；质量／长度／体积作为商品基准单位须通过专门契约和迁移后启用。物流重量和体积可独立作为计费测量值，不因此改变商品库存基准。

## 2. 数据对象与关系

| 对象 | 责任与关键关系 | 版本方式 |
|---|---|---|
| RFQCostingContext | 引用已核实CustomerRFQ及父对象版本与行ID，补充本轮需求与核价条件；原客户要求、经核实条件和测算假设分别记录 | 草稿CAS；核实形成不可变修订，RFQ改变需重新核实 |
| SupplierOffer | 一次供应报价及来源；响应询价时关联SupplierRFQ，主动报价／导入价目走显式来源分支；全部绑定需求行、规格和条款 | 草稿CAS；人工核实后内容冻结；更正生成新修订 |
| PurchaseTermsVersion | 从核实报价或合同批准形成的可复用采购条件；保留供应商、商品、适用区间、来源和批准记录 | 独立版本和效力；不自动从单次回复永久生效 |
| LogisticsOffer | 服务商具体角色、线路／区域、运输方式、费用及计费条件 | 草稿、核实修订、替代／撤销记录；有效期独立 |
| CostScenario | 一套明确需求、候选选择、计算货币、条件与费用范围；采购来源互斥选择offer_revision或purchase_terms_revision；支持比较，不自动形成采购订单 | 草稿CAS；计算产生不可变结果 |
| CostSnapshot | 记录所有输入修订、规则、核实证据、计算明细、差异和选定依据 | 仅追加；替代关系不覆盖旧快照 |

一个RFQ行可以有多家SupplierOffer；一个场景对每个需求行必须明确选择供给分配。跨行MOQ由明确分组聚合，不能按报价文件或供应商名隐式合并。相同需求比较两个候选通常生成两个完整场景。

## 3. 通用类型、数值与时间规则

| 类型 | 输入与约束 |
|---|---|
| ID | 沿用原契约1—64字符标识规则；租户、公司、操作者由服务端注入，禁止请求自报 |
| MasterRef | kind＋id＋row_version；核查当前可访问、正确角色、已核实及版本匹配 |
| ConfigRef | kind＋id＋revision；核查公司、访问权、当前效力、依赖、来源及有效期 |
| BusinessRef | kind＋id＋revision，或明确row_version的可变对象；两种版本语义不可混用 |
| Quantity | 十进制字符串，最多6位小数且不超过引用单位precision，正值，≤1,000,000；0仅允许用于明确表示费用免费或无需数量的其他类型，不能用作商品需求 |
| UnitPrice／RateAmount | 非负十进制字符串，最多8位小数；0需zero_price_reason并经核实；按明确计价基数使用 |
| Ratio | 十进制字符串，最多8位小数；比例用0—1的小数，不用30表示30%；毛利例外与适用区间分别约束 |
| ExchangeRate | 正十进制字符串，最多12位小数，≤1000，沿用基础汇率方向、用途和来源年龄约束；不与付款比例类型混用 |
| Money | 金额按引用币种的minor_units与HALF_UP输出；计算用Decimal和50位上下文。单项及场景绝对金额不得超过1,000,000,000,000 |
| Measurement | 重量／尺寸／体积最多6位小数及明确单位，严格正值；标准化至KG／M／M3，转换因子必须有版本依据 |
| DateTime | 带时区ISO时间；服务端计算时点，区间[valid_from,valid_until)，不可用客户端时间续期 |
| 未知值 | 不传null，不用0、空串或默认币种伪装未知。草稿缺字段允许；已提供字段仍须合法；假设须独立记录依据、范围及核实状态 |

不接受JSON浮点数、科学计数法、NaN或Infinity作为十进制输入。数量6、单价8、比例8分别校验，不能因为通用容器支持12位而放宽。中间计算不在第13.3节规定节点之外舍入；快照保留未舍入明细的确定性十进制表示以及最终金额。

## 4. 询价核价上下文字段

| 字段 | 结构与规则 | 必填节点 |
|---|---|---|
| rfq_ref | 当前ready询价及row_version，引用可读来源和已核实主数据 | 创建正式核价上下文 |
| lines | 唯一rfq_line_id、sku_profile_ref、quantity、unit_ref、conversion_ref（需要时）；与原需求逐行关联 | G1 |
| requested_currency_ref | 客户明确要求时记录；未知时省略并进入待确认清单 | 客户提供时；不妨碍明确计算币种的内部草案 |
| calculation_currency_ref | 核价政策对应的显式计算货币 | G1 |
| requested_delivery | 客户原文及来源定位；核实后为固定日期或锚点＋天数＋自然日／工作日及日历版本 | 影响比较时G1；承诺前G2 |
| requested_trade_terms_ref／requested_payment_terms_ref | 客户已核实要求；不得由公司默认条款覆盖原始要求 | 相应要求已知时 |
| comparison_basis | 统一商品规格、需求数量、责任范围、交付地点、交期约束、税价／可回收口径及所需费用项 | G1可比结果 |
| assumptions | assumption_id、涉及字段、拟用值、理由、证据、提出人及核实结果；保留与客户要求的差异 | 使用任何假设时 |

同一字段同时有客户要求和测算假设时以两条不同语义保留，不能覆盖客户值。未核实假设可用于verification_status=provisional的计算，可比性另行判断；不能冻结为verified成本。已有ready询价不自动代表核价上下文齐备。

## 5. 供应报价与采购条件字段

| 字段组 | 字段和规则 |
|---|---|
| 身份及来源 | supplier_ref、supplier_contact_ref（适用时）、origin_kind及分支引用、rfq_line_refs、source_refs及逐项locator；核实人及时间由服务器写入。in_response_to_rfq要求supplier_rfq_ref；unsolicited／imported_price_list不得补造SupplierRFQ，要求真实来源及本次适用核实 |
| 商品 | offer_line_id、sku_profile_ref、supplier_item_code、规格依据；映射不能只靠自由文本相似匹配 |
| 数量与计价 | purchase_unit_ref、price_unit_ref、price_basis_quantity、conversion_refs；price_basis_quantity正值且最多6位小数；明确每件、每打、每100件等基数 |
| MOQ | moq_quantity、moq_unit_ref、moq_scope（sku或explicit_group）、group_id及成员需求行；无共同可比单位时不可合计 |
| 订购倍数 | order_multiple及单位，适用时正值；未满足时不能自动增加采购量；另建明确超采场景并标明剩余量 |
| 价格区间 | pricing_mode（flat/all_units/graduated）、tiers的lower_inclusive／upper_exclusive、unit_price；区间连续、无重叠；无上限用省略表示，不能null |
| 商业口径 | currency_ref、tax_profile_ref、trade_terms_ref、payment_terms_ref、有效区间；price_includes列明包含的费用义务 |
| 交付 | 固定日期或带锚点、日历、天数的交期，供应能力声明及依据；有交期声明不等于库存已锁定 |
| 可复用条件 | 适用供应商／SKU／规格／数量范围，效力与引用原报价修订，独立发布与停用依据 |

阶梯语义：all_units按总采购数量所在区间的单价计算全部数量；graduated按各区间覆盖的数量分别计算后相加。使用连续数量区间，例如[0,100)、[100,200)；graduated将100件分为前100件，边界只决定增量区间，不额外加一件。flat仅一个价格。原供应报价无法判断阶梯含义时保持待确认。

MOQ校验使用实际采购数量；比较成本同时报告需求数量和采购数量。本轮基准场景不得自动超采或拆成多个供应商来规避MOQ。组合供给须明确每行分配，合计等于需求；包含超采时须独立标记并且不把全部采购成本默认为已售数量的成本。

## 6. 物流与直接费用字段

| 字段组 | 字段和规则 |
|---|---|
| 报价范围 | logistics_offer_ref、service_party_ref及role、service_code、mode、origin_ref、destination_ref、route／zone适用条件、有效区间及来源 |
| 测量口径 | measurement_ref及修订、对象层级（shipment/container/package/line）、实重、尺寸和包装数量；体积重须计量单位匹配的显式除数与舍入规则 |
| 计费规则 | fixed/per_basis/tiered；计费数量、单位及基数、费率、最低收费、币种、税价、包含关系和适用附加费；组合费用拆成独立component |
| 体积重规则 | chargeable_rule可为actual_weight、volume、max_actual_volumetric、per_container或明确固定费；体积重除数须为带单位、经核实规则，不默认6000或5000 |
| 舍入规则 | billing_increment、rounding_mode（ceil/half_up）、rounding_scope（per_package/shipment），均显式；最低收费在该规则对应计费对象层级使用 |
| 费用身份 | obligation_id标识同一商业费用义务，fee_type_ref、服务范围、责任方、来源；来源行ID和本地component_id不能替代义务身份 |
| 包含关系 | included_in_component_ref；含在货价内的费用可展示为included，但不再次求和；合计费用和其明细不能同时计入 |
| 分摊 | allocation_basis（quantity/weight/volume/amount/explicit）、target_line_ids、分母和单位；零分母拒绝，explicit使用非负整数相对权数，至少一个为正；服务器按总权数归一 |

计算顺序：按规则确定测量层级及计费量→执行计费量舍入→费率计算→同层级最低收费→税价口径处理→按明确汇率转计算币种→费用总额舍入→分摊。重量／体积分摊必须具备各目标行的可靠测量数据。

费用去重由服务器综合obligation_id、来源、服务范围与包含关系校验。不能仅按费用名称去重，也不能靠改component_id重复计入。无法判断两项是否重叠时进入CHARGE_SCOPE_UNRESOLVED，不擅自删除或双计。折扣、返利等不能以负物流费偷渡，首批不支持时明确拒绝。

分摊以币种最小单位执行最大余数法：总额先按币种舍入；各行精确份额向下取最小单位，剩余最小单位按小数余数从大到小分配，相同余数按ASCII line_id升序排序。合计必须严格等于费用总额。该规则仅适用于首批非负成本。

## 7. 同口径比较与计算边界

1. 确认RFQ及上下文版本、所有引用权限、主体隔离、当前状态与效力。
2. 将需求与报价数量转到SKU可比基准，保留采购单位、计价单位、换算链及输入精度；不执行未经核实的逆向换算或跨SKU商品替代换算；已核实MOQ组允许成员各自标准化后汇总共同计量数量。
3. 按实际采购量检查MOQ、倍数、价格区间及交期约束，算出采购价原币金额。
4. 按显式税价及回收依据算出可用于核价的成本：全额回收、部分回收（按经核实的可回收率）或不可回收；未知回收处理阻断。可回收率不由税率直接推导，预计可回收额不计作利润。
5. 按统一责任与费用范围补齐差异；供应商价已含费用保持可追溯包含关系。不能简单认为FOB或CIF字符串相同就必然同口径。
6. 用明确方向、cost_estimate用途的汇率转计算币种；同币种明确1，跨币种缺引用拒绝。币种、费用及税务差异无法标准化时保留候选但标记not_comparable。
7. 保留采购成本、直接费用、分摊行、总成本、每行成本和单位分析成本；逐行金额汇总为场景总额，不能用展示单位成本反乘覆盖总成本。
8. comparable表示本次条件可比，不自动表示最低价、最佳供应商或已可采购。人工选择须有依据；高价候选不删除。

比较维度为comparison_status=comparable／not_comparable，资料维度为verification_status=verified／provisional，另有计算成功／失败状态；not_comparable带差异路径。valid_for_quote为服务端当前资格判定，仅对计算成功、可比、已核实冻结结果开放，后续T03使用时再次检查相关配置效力及适用条件。销售收入和毛利由T03结合报价计算；毛利率为(净销售额－适用成本)/净销售额，不能误用成本加成率。

## 8. 成本快照字段与冻结

| 字段 | 内容 |
|---|---|
| snapshot_id／scenario_ref／rfq_ref | 服务器标识、场景版本、需求版本；snapshot自身内容不可变 |
| calculation_time／engine_version | 服务端时间、算法版本及货币规则 |
| foundation_snapshot_ref | 引用已有基础快照，并校验新增报价、测量、来源及业务引用不因其存在而被遗漏 |
| input_manifest | 配置与主数据版本、供应报价／物流报价修订、原始来源及核实证据、测量和条件引用；按固定顺序保存 |
| components | 原币金额、税价和成本处理依据、汇率版本、标准化金额、费用义务、包含／排除原因、分摊依据与行结果 |
| comparison_result | 比较口径、候选差异、计算状态、假设清单及已核实状态 |
| selection | 被选方案、未选候选引用、选定人及原因；无选定信息不伪造“已选成本” |
| totals | 每行及整场景成本、币种与舍入结果；原始数量与采购数量分别保存 |
| evidence_hash | 对封闭、确定性序列化的业务载荷做SHA-256；版本及语义顺序明确，金额为十进制字符串；不含读者权限状态或运行耗时 |
| supersedes_ref | 更正产生新快照并指向旧快照；拒绝循环及跨公司替代；不自动修改已关联的报价 |

哈希不证明来源真实或供应商身份。冻结须授权人工核实结果齐备、无未解决关键差异、规则当前有效。既有共享配置的独立人工发布约束保留；本文件不凭空增加所有成本冻结必须双人这一规则，具体独立复核按当前核价政策及动作授权执行。

历史读重新检查当前权限，包括底层来源和敏感数据；不因自然到期删除历史结果。新报价和新计算不能把历史快照可读等同于当前适用。未知外部行为不得触发自动重复采购或发运，本切片不执行这些外部行为。

## 9. 候选命令与错误契约

所有命令采用contract＋command＋idempotency_key＋args；新增对象首次expected_version=0，更新严格CAS；草稿整份替换，不是PATCH。封闭结构禁止额外字段。候选命令名不表示当前已有接口。

| 命令 | 行为与守卫 |
|---|---|
| costing_context.save_draft／verify | 保存条件；人工核实版本和证据，形成可引用修订 |
| supplier_offer.save_draft／verify／withdraw | 保存、核实、撤销；核实后内容不原地修改，已核实报价更正另起修订 |
| purchase_terms.save_draft／publish／retire | 保存可复用条件，按独立人工发布规则生效；停用不抹去来源 |
| logistics_offer.save_draft／verify／withdraw | 保存、核实及撤销物流报价；检查服务角色、费用范围与计量规则 |
| cost_scenario.save_draft／evaluate | 保存方案、显式指定输入版本；evaluate无商业承诺，产生不可变计算结果，客户端合计不作真值 |
| cost_snapshot.freeze | 以结果引用及hash、场景expected_version和选择依据冻结；复验权限、规则、来源效力和假设核实状态 |

各对象采用第13.2节独立状态机；不可变业务修订与可变根版本分别管理。评估结果与冻结快照分开，防止把未核实预览冒充可报价成本。

校验顺序沿用T01：契约结构→当前身份及可访问性→幂等成功结果及载荷冲突→CAS与状态→字段、引用及跨字段业务校验→事务提交。所有错误返回code；仅获权后的结构／业务字段错误返回field_errors的path／reason，权限拒绝不返回隐藏依赖详情；查询缺失与无权的外部可见行为遵循既有防泄露规则。

| 错误码 | 确定含义 |
|---|---|
| SCHEMA_INVALID／UNSUPPORTED_CONTRACT | 字段类型、结构、精度或契约版本不合法 |
| PERMISSION_DENIED／NOT_FOUND | 沿用当前身份、公司及记录范围语义；不泄露其他公司存在性 |
| VERSION_CONFLICT／INVALID_STATE | 并发版本或状态不允许当前动作 |
| IDEMPOTENCY_PAYLOAD_CONFLICT | 同命令同键但业务参数不同 |
| REFERENCE_INVALID／REFERENCE_STALE | 引用类型或关系错误；引用版本／状态已不适用 |
| COST_INPUT_NOT_READY | 必需输入或证据缺失，返回逐字段路径 |
| MOQ_NOT_MET／ORDER_MULTIPLE_INVALID | 采购量不满足已核实数量条件 |
| PRICE_TIER_AMBIGUOUS | 区间重叠、不连续或阶梯含义未确定 |
| CHARGE_DOUBLE_COUNT／CHARGE_SCOPE_UNRESOLVED | 已识别重复费用义务；或范围尚不能确定 |
| COMPARISON_BASIS_MISMATCH | 无法统一规格、责任、交期或税价条件 |
| FX_NOT_APPLICABLE／TAX_TREATMENT_UNKNOWN | 汇率方向／用途／效力不符；或税额成本处理未核实 |
| ALLOCATION_INVALID／CALCULATION_LIMIT_EXCEEDED | 分摊依据不成立；或输入／运算超过范围 |

同键成功重放须复核当前读权，包括新对象依赖和底层来源；不得重跑计算、创建新快照或因旧资料自然到期而改变原成功结果。重新评估使用新动作键并生成新result_id；未修改场景时仍绑定原root_version，修改场景才递增root_version。一次成功写入必须原子提交业务对象、action_result、审计与Outbox，失败不留下半份快照。

## 10. 兼容与存储迁移要求

采用加法迁移保存新上下文、报价、场景、快照及组件；公司复合外键、引用版本和行ID唯一在存储层落实，角色、适用性及Decimal运算在服务层落实。新迁移号由实施时读取实际schema_migration后分配，不能在文档中预称已升为3。

旧RFQ不补造币种、付款或交期；旧PCS记录保持原含义。新上下文绑定原rfq行与版本；RFQ修订使后续使用需重新核实上下文，历史快照不改写。非PCS商品基准的扩展另行版本化，不能在旧契约中悄悄放开uom。

先备份、事务迁移、核对记录数／关联／历史审计，再验证旧命令回归；无真实依据的数据不回填。回退用完整备份，不能删除迁移记录让旧程序读取新库。

## 11. 首批验收样例及判定

全部为合成数据，不使用真实税率、汇率或市场价。以下是待实施的预期结果，不是本轮已通过测试。

| 场景 | 输入 | 预期 |
|---|---|---|
| 既有基准回归 | 120PCS×5USD，另有60USD独立直接费用 | 商品600、费用60、总成本660USD |
| 两家同口径比较 | 同120PCS；A按PCS每1件5USD且已含相同服务义务运费；B按PCS每100件480USD、另付运费60USD；均为合成零税率、nonrecoverable配置，同地点同规格已核实且报价当前有效 | A600、B576＋60＝636USD；不为A重复加60，价格不同仍保留两个候选 |
| 全量／累进阶梯 | 150PCS；[0,100)单价5，[100,∞)单价4 | all_units为600；graduated为100×5＋50×4＝700；未知模式拒绝 |
| 包装及MOQ | 一打12PCS，需求120PCS；最低10打、每打报价60USD | 采购10打，成本600USD；最低11打则阻断，不静默超采 |
| 分组MOQ | 四SKU同规格族明确分组，总需求120件、MOQ100件；另有无关SKU | 只聚合明确成员；无关SKU不参与凑量；不能对每个SKU误套组MOQ |
| 费用尾差 | 0.05USD按三行相等权重分摊 | 按稳定ID顺序为0.02、0.02、0.01；总和0.05 |
| 跨币种 | 原币费用100USD，合成cost_estimate汇率7.123456 CNY/USD | 712.35CNY；缺方向、过期或报价用途汇率不放行 |
| 并发及重放 | 两个同版本冻结请求；相同动作键重试 | 不产生重复冻结；过期CAS拒绝；合法重放返回原成功及replayed=true，当前无权则不返回原敏感内容 |
| 历史及新使用 | 供应报价撤销或规格修订后读取旧成本，再发起新计算 | 历史按当前权限可读；新计算拒绝旧失效输入；不改旧成本数值 |
| 假设与草稿 | 缺核实交期、使用未核实运费假设 | 可保存并明确provisional；不可冻结成verified或作为正式报价依据 |

还须验证每字段精度边界、伪造公司、越权来源／历史重放、跨公司引用、配置替代、税费处理未知、零分摊分母、费用重叠、迁移失败回滚和旧T01回归。测试编号及数量以实际测试实现与证据为准，不预填通过。

## 12. 实施出口与下一项工作

完成标准：封闭schema与规范样例一致；DDL及迁移可执行；服务端逐字段守卫、Decimal计算、并发／幂等、快照证据和权限完整；上述行为预期通过并输出真实验证记录；原38项适用T01回归和现有基础能力不得退化。总测试数不与历史66或73简单相加。

下一项工作是按本规格生成候选JSON Schema、参考DDL及有效／无效样例，进行交叉校验后实现A批基础补齐与T02。不把本规格发布登记为代码完成，也不提前登记正式报价、订单或真实AI整链验收。

对应补齐矩阵：FND-05/07/08/09/11/12/15、INT-01及INT-04为核心；FND-01/03/04/06/10按实际输入依赖引用。INT-03中T02依赖的最小服务角色前置；其余INT-02/03/05/06/07保留在对应报价／履约切片，不因本文件提前宣布完成。


## 13. 严格审查修订与实施细则

本节将CR-01—12逐项转为规范要求，与前文共同构成本版；未定义或未支持的路径应明确拒绝，不由实现者猜测补齐。

### 13.1 数量、来源与最小依赖（CR-01/04/07/08）

- 询价行唯一引用为rfq_id＋rfq_row_version＋line_id，首批不创建虚构独立行版本。上下文核实必须覆盖原询价完整行集合，SKU、规格和标准化数量逐行相等。试算另一需求数量须另建what_if上下文并标记不可用于该RFQ正式报价；正式变更先修订RFQ。
- 商品数量同时满足字段6位上限及单位precision；单位换算须精确可表示，不能截断或自动凑整。首批实物采购DOZEN数量精度由单位定义确定；按PCS计价时可直接采用供应商核实的PCS条件，不能从整打采购规则擅自推导拆打能力。
- MOQ组按supplier_id＋offer_revision或有效采购条件版本＋joint_order_group_id＋明确成员组成。只合计本场景分配给该组供应商且条款允许同批订购的采购量，不合计其他供应商、未选候选或历史采购。不同SKU各用自身有效换算进入共同组单位，不表示商品可互相替代。
- price_tier_scope与moq_scope分别声明。首批moq_scope允许sku或explicit_group，price_tier_scope仅允许sku；组MOQ可跨SKU，不支持组阶梯；explicit_group阶梯返回UNSUPPORTED_PRICING_MODE，不回退到逐SKU计价。后续启用组阶梯须独立定义组计费对象及税价／分摊算法。阈值数量先转换到tier_unit，片段数量转到price_unit后按数量÷price_basis_quantity×unit_price求金额；累进模式各片段求和。
- SupplierRFQ最小对象随T02交付：id、root_version、supplier_ref、rfq_ref、唯一需求行集合、来源、draft／issued／closed；issued仅表示已登记的人工发出事实及证据，不自动发送。只有origin_kind=in_response_to_rfq的SupplierOffer必须关联该对象；主动报价或导入价目走受限来源分支，仍须关联本次需求行并核实适用性，不得伪造issued。
- MeasurementSnapshot随T02交付：id、revision、来源及定位、测量时间、核实人、包装行ID、包装数量、每包装尺寸／净毛重及单位、所覆盖RFQ行、汇总重量／体积。先按包装数量汇总，父子层级不能重复计入。不可变测量修订供LogisticsOffer及场景引用。
- 最小ServicePartyRole以伙伴引用、角色、适用服务、起讫地点、有效期及核实证据随T02交付；不修改旧customer／supplier角色含义。首批仅exact origin＋destination匹配和明确运输方式，zone及复杂路线匹配暂拒绝。临时自由文本不替代适用性检查。
- 首批支持明确fixed、per_basis和已定义阶梯计费；未实现的模式返回UNSUPPORTED_PRICING_MODE，未交付依赖返回DEPENDENCY_NOT_SUPPORTED，均不静默套用相近公式。

### 13.2 对象状态与版本（CR-06）

每个修订根保存root_version、working_revision及active_revision；业务内容和核实证据在核实后不可修改，效力由独立状态事实表达。root_version为所有根写动作的CAS，不等于revision。所有动作使用当前root_version，新建expected_version=0；每次成功根写递增1。

| 命令／对象 | 前置与目标 | 成功结果 |
|---|---|---|
| save_draft（上下文／供应报价／物流报价／采购条件／服务角色） | 新根，或已有working_revision；已有active但无工作稿时拒绝，必须revise | 初建分配服务端revision=1；编辑现有工作稿保留revision并递增root_version；不触碰active |
| revise | 明确base_revision属于本根且内容已核实；working_revision必须空，CAS匹配 | 复制为新revision草稿，保留来源和base_ref，清除新稿核实／发布证据；active不变 |
| discard_draft | CAS及working_revision匹配 | 记录稿件丢弃事实并清空working，不减少last_revision，不影响active |
| verify（上下文／供应报价／物流报价／服务角色） | 人工权限、CAS、working_revision及证据齐备；若已有active，supersedes_ref必须恰等于它 | 封存工作稿，原子替代active，清空working；旧稿记superseded；前向使用只接受明确当前active版本 |
| publish（采购条件） | 同verify，另需独立发布者与编制者不同、适用时段及证据 | 新版本active，旧版本superseded，清空working；同根最多一个active |
| withdraw／retire | CAS及精确target_revision，目标为已核实版本；不能撤销未核实稿 | 记撤销事件；仅当target等于active时清空active。撤销旧superseded版本不影响当前active；不回退启用更旧版本 |
| CostScenario.save_draft／evaluate | save须draft及CAS；evaluate须显式场景版本 | save递增root_version；evaluate仅追加不可变结果，不递增场景版本、不改变状态；同版本可以新键新时点评估 |
| cost_snapshot.freeze | draft场景CAS、结果及依赖全部满足13.5 | 场景frozen并递增root_version；唯一快照保存freeze前input_scenario_version与冻结后scenario_version，结果绑定前者 |
| MeasurementSnapshot | measurement.record_verified人工核实、来源及测量字段齐备 | 不可变版本与效力头按13.8管理；不是普通draft对象 |

新增候选入口包括各修订对象的revise／discard_draft、上下文及服务角色withdraw、采购条件retire、supplier_rfq.save_draft／record_issued／close、measurement.record_verified／supersede／withdraw。draft与verified是修订状态而非根唯一状态；新稿不影响现行版本。SupplierRFQ仅draft可编辑，record_issued要求人工发送证据并封存内容；已issued更正用新SupplierRFQ并引用旧对象，不覆写发出事实。

核实／发布／撤销／freeze各有显式动作权限。核实人、操作者及时间均由服务器获取。采购条件独立发布和既有共享配置独立发布为强制要求；其他核实是否独立按当前已启用核价政策执行，政策要求independent_review时编制人不得自核实。measurement同样适用该政策。

### 13.3 成本及舍入公式（CR-03）

首批以同一选定商业条件中的单SKU报价行作为采购计费粒度；同一行的阶梯片段精确求和后再拆税。该行可分配到多个同SKU需求行，但须先聚合采购量并只计费一次，禁止通过拆分需求行改变舍入。组MOQ不改变计费对象；组阶梯及其他计费粒度明确UNSUPPORTED_PRICING_MODE。

1. 计价数量除基数及阶梯片段金额采用13.9的有界精确有理数；进入规定币种舍入节点才HALF_UP，不能先将单位价格舍入为币种金额。累进片段精确求和得到原币计价金额。Decimal／50位上下文限于已证明等价的现有税价公式及显示路径。
2. 原币税价拆分与现有tax.preview同版本规则一致：含税价先舍入含税总额，再将gross/(1+rate)按币种舍入为net，tax=gross-net；未税价先舍入net，再舍入net×rate得tax，gross=net+tax。此处是明定舍入节点，并非“所有中间值不舍入”。
3. recoverable成本为net，nonrecoverable成本为gross；partial成本为gross减可回收税额，可回收税额＝net×可回收率（可回收率不高于税率，按币种HALF_UP舍入），用于外贸企业免退税下退税率低于征税率等情形；unknown阻断。回收处理与可回收率须经业务适用核实（CN-D02），不从商品名称或税率推断；预计可回收额不计作利润。
4. 每个采购报价行或单个费用计费对象的原币成本乘显式汇率，按计算币种舍入得到component金额；不同来源行不得为减少尾差擅自合并。整场景成本等于这些component金额之和。
5. 共同费用在component金额上执行13.4分摊。采购component若覆盖多个目标行，也用同一分摊规则。最终各目标行成本为所获component金额之和；场景总额严格等于目标行合计。
6. 解析单位分析成本仅用于展示，以8位HALF_UP输出，标记derived_display；不能反乘改写权威合计。负成本首批不支持，返回明确错误；partial缺可回收率或可回收率高于税率返回TAX_RECOVERY_RATE_INVALID。

固定反例：两个独立未税采购行各0.005USD、合成零税率、nonrecoverable、同币种时各0.01，总成本0.02；同一行两个阶梯片段各0.005时先合计为0.01。另例：合成含税113CNY、税率0.13、recoverable为net100、tax13；成本转USD，显式0.14USD/CNY，得到14.00USD；nonrecoverable则15.82USD；partial可回收率0.09时成本104CNY，转USD为14.56USD。以上仅合成算术，不代表现行税务政策。

### 13.4 分摊相对权数（CR-02）

explicit输入weight为0—1,000,000,000的整数字符串，总和必须大于0；不是付款Ratio，不要求总和1。quantity／weight／volume／amount模式先统一单位和最大小数位，再乘公共10次幂得到精确整数权数，禁止先截成8位比例。amount的唯一基数为本场景各目标行已确定的原始采购component成本（计算币种、已舍入、未加任何共同费用），快照逐行保存base_component_refs及金额；不引用销售额或费用分摊后的累计成本。共享采购component自身不得用amount自分摊，首批使用quantity或explicit；其分摊确定后才形成共同费用的固定amount基数。quantity采用标准化需求数量，weight采用已核实净重，volume采用已核实外包装体积，均须覆盖同一目标范围且单位可比，否则拒绝。

先将已舍入component金额转为整数最小币值N。对每行整数权数w及总权数W，使用整数divmod(N*w,W)得到商与余数；商为初始份额。按余数降序、ASCII line_id升序分配剩余最小币值。这样1:1:1精确表达等权；0.05USD分为0.02／0.02／0.01，不依赖浮点容差或Decimal近似除法。权数0可以有目标行但不分得费用；无合法分母则拒绝。

### 13.5 评估、冻结与当前资格（CR-05/09）

EvaluationResult固定保存scenario_id、scenario_root_version、context_revision、RFQ版本、input_manifest_hash、实际输入载荷、engine_version、calculation_time及两个状态维度。服务端生成input_manifest_hash与result_hash，不接受客户端合计或hash作为计算真值。

freeze须在单个一致事务中读取并核对：场景CAS／draft状态、结果属于同场景同版本、全部需求与上下文绑定、规范化输入及算法版本相等、核实状态verified、comparison_status=comparable、所有依赖当前权限及效力、汇率来源年龄与全部有效期满足冻结时点。任何值或适用性改变均拒绝并要求新evaluate，不能只重签旧结果。金额不因freeze自动重算。

事务中原子冻结场景、插入快照、记录动作／审计／Outbox，并以公司＋scenario_id唯一约束确保不同请求键也不能重复冻结。合法同键重放仅按当前读权返回原成功，独立于“新冻结”的适用性校验，不复用到别的场景。

valid_for_quote是查询或T03动作时的服务端派生资格，不是快照内永远有效的真值；对已失效报价或规则的新使用必须拒绝。历史快照仍可按当前权限读取。计算失败返回错误而非伪造零成本结果；可比性与核实程度分离后，provisional可以在形式上comparable，但不得冻结为verified。

### 13.6 边界、哈希与错误保密（CR-10/11/12）

首批资源上限：单请求1MiB；需求行200、每报价行阶梯50、候选报价200、费用component500、测量包装行1000、直接引用1000、依赖深度8、包含图深度8；各类限额分别计数，同一对象同版本去重后计直接引用；一次计算展开对象节点总数≤5000、包含边≤2000，递归展开总操作数≤10000，循环引用一律拒绝；超过任一限额在计算前拒绝。源文本入口沿用旧上限，不以本规格扩大。

十进制输入最长40字符且满足既定小数位／金额／数量上限；RateAmount／UnitPrice≤10^12、尺寸每轴≤1000M、汇总重≤10^9KG、体积≤10^9M3。最终业务数量、测量、component金额及场景总额检查各自业务上限；中间量不套用最终金额上限，受下述精确运算资源边界控制。超过明确限制返回CALCULATION_LIMIT_EXCEEDED。相对权数不进入Decimal乘除，使用有界整数并受目标行数限制。

JSON拒绝重复键、未知字段、null及非有限数；权限会话字段不能由请求注入。证据载荷字符串按原Unicode码点保留，不擅自NFC改写来源；Decimal业务值移除无意义尾零，零统一为“0”，禁止负零输入。对象键按现有canonical排序，结构数组按明确稳定ID排序，语义有序数组例如阶梯按区间下界排序并验证唯一；source原始正文hash规则不变。

请求幂等哈希沿用现有canonical请求规则，不强行把“1.0”和“1.00”当同请求；业务证据哈希对服务端规范化数值载荷计算。两者分离，保留算法版本。机器契约阶段必须交付规范输入／规范输出／SHA-256黄金向量，缺失不得发布运行时契约。

结构校验可返回用户本人提交的字段错误；鉴权失败只给通用PERMISSION_DENIED或按既有规则NOT_FOUND，不返回隐藏依赖名称、字段路径或比较结果。源正文与敏感字段必须在获权之后读取或比较；原成功重放也执行当前依赖读权检查。

### 13.7 规范样例要求及复审状态

第11章样例为行为预期；机器契约生成时必须为每个样例补齐引用配置、已核实来源、主体角色、适用时钟、SKU／单位版本、税价和费用义务。MOQ样例的DOZEN若要整打采购，其单位precision必须0，price_basis_quantity=1，purchase_unit=price_unit=DOZEN；报价valid_from≤测试时钟<valid_until。两家报价比较使用同一comparison_basis及同一运输服务范围，已含费用为included而不再求和。

本版12项审查问题均已有文字处置：CR-01/04/07/08见13.1；CR-06见13.2；CR-03见13.3；CR-02见13.4；CR-05/09见13.5；CR-10/11/12见13.6—13.7。仍需机器契约、迁移与运行时验证证明实现符合，不能将“文字已处理”登记为业务测试通过。

本版先完成RV-01—08修订的交叉检查；只有无剩余文字阻断项才进入候选机器契约制作，不可直接宣称运行时发布准入通过。后续schema或可执行样例若揭示语义冲突，应返回本规格修订，不以实现细节偷偷覆盖契约。

审查编号独立索引：

| 问题 | 修订位置 |
|---|---|
| CR-01 | 13.1 |
| CR-02 | 13.4 |
| CR-03 | 13.3 |
| CR-04 | 13.1 |
| CR-05 | 13.5 |
| CR-06 | 13.2 |
| CR-07 | 13.1 |
| CR-08 | 13.1 |
| CR-09 | 13.5 |
| CR-10 | 13.6 |
| CR-11 | 13.6 |
| CR-12 | 13.6—13.7 |


### 13.8 来源、测量与效力的统一约束（RV-03/05）

场景每条采购分配使用封闭oneOf来源：{kind:offer,id,revision}或{kind:purchase_terms,id,revision}。服务端先归一为相同字段的ResolvedPurchaseCondition，保留原始来源类型及规则版本；不能同时相加两个来源。offer原始分支须in_response_to_rfq／unsolicited／imported_price_list互斥，响应分支核对询价供应商和需求；其余分支禁止supplier_rfq_ref，必须真实来源和本次适用核实证据。

purchase_terms默认independent_validity：发布的条件有独立有效期和适用范围，原报价仅作可读来源证明，原报价自然到期不使其自动失效；原来源撤回真实性或条款被否认时，登记可追溯的来源否认事实，并以purchase_terms.retire及明确target_revision处理相关条件；在完成逐根停用前，新计算、冻结与T03使用也必须沿来源链检查该否认事实并立即阻断。原报价普通自然到期或不涉及真实性的商业撤销不自动解释为来源否认。仅显式dependent_validity模式才要求原报价当前active且有效；发布时固定模式，不允许客户端在场景中切换。任何模式都重查当前来源读权和已登记撤销／否认事实，不因独立效力绕过访问控制。

MeasurementSnapshot根有last_revision、active_revision、root_version；record_verified仅新建根；supersede需当前CAS、精确active引用及新核实证据，追加新版本并原子替代；withdraw精确目标，只在撤销当前active时清空它；旧数据仍按当前权限历史可读，无自动回退。场景只接受当前有效版本，冻结与新使用重查效力。测量来源若实质被否认须显式撤销，不能只上传新附件。

测量字段kind=measured／estimated；估计值可由授权人工核实估算依据，但标签保持estimated，不能冒充实测。场景若用estimated必须保存估算理由与批准依据；能否冻结由显式pricing_policy.allow_verified_estimates决定，缺该扩展即按false阻断冻结，旧政策不得默认放行。原始长宽高和包装数量为体积推导依据，服务端换算后计算体积；提供的声明体积仅用于一致性核实，不覆盖推导。首批声明值须与按固定volume_precision=6、M3单位HALF_UP舍入的推导值相等，否则待核实。净重／毛重为有来源的测量事实，按同一包装口径校验毛重≥净重。

测量包装数量是整数，leaf包装逐个或按相同尺寸／重量分组，只有叶子计入；混装包未能确定目标行净重或体积时不得用weight／volume分摊，改用有依据的explicit。父包装尺寸用于计费时替代其子包装计费范围，不能父子双计。首批支持单一计费层级，无混合层级算法时明确拒绝。

max_scope与rounding_scope分离且首批要求一致，取值per_package／shipment。per_package为每包max(实毛重,体积重)再按billing_increment舍入，最后按包数求和；shipment先汇总毛重与体积重，再取max并舍入。体积重由明确单位的体积及除数计算。scope不同的组合首批拒绝，不猜测顺序。例如两包实重／体积重分别10／1和1／10KG，逐包取max为20KG，整票取max为11KG；计费资料必须明确选择哪种。

### 13.9 固定分摊基数、运算范围与规范样例（RV-01/02/06/07/08）

共同费用的amount基数在采购成本归集完成、任何共同费用分摊之前一次冻结。各费用都使用该同一基数，费用处理顺序不得改变结果。零总基数返回ALLOCATION_INVALID，不自动均分。选用explicit替代须新的明确场景输入和核实依据，不由算法自行选择。quantity／weight／volume不满足共同单位或测量范围也同样拒绝。

商品组阶梯首批明确不支持：同组150件、前100件5USD／其后4USD，不得自动算750、700或1400，而应UNSUPPORTED_PRICING_MODE。单SKU同一报价行150件的graduated仍为700；组MOQ四SKU总120件满足MOQ100的已有范围不变。后续支持组阶梯须另发候选契约并补成本分摊及税价规则。

数值业务上限在各类型输入、标准化最终数量、component及场景金额检查；中间有理数不套最终金额上限。对输入十进制先转精确整数／10的幂；数量除基数、单位标准化、体积重及权数比较可使用有界有理数，单个分子／分母十进制位数≤256、分母非零，超限拒绝。第13.3确定的币种／数量舍入或精确可表示校验前不以Decimal近似值判定边界。现有tax.preview公式及HALF_UP保持，新增非整除计价路径采用精确有理数后舍入；50位Decimal作为展示或已证明不改变结果的实现选择，不是“精确”的替代保证。

例：单价10^12、数量10^6、计价基数10^6，最终金额10^12合法；中间数量乘单价10^18不因最终金额上限而提前拒绝。最终金额10^12＋0.01仍拒绝。

机器契约制作的必交付清单：逐命令请求／响应schema及封闭oneOf，root／revision／效力头DDL与唯一约束，命令权限及政策矩阵，来源分支和全部依赖的完整种子数据，固定时钟的有效／无效端到端样例，逐JSON路径的排序及规范化清单，原请求／规范业务载荷的哈希黄金向量，CR与RV到案例的映射。样例需实际通过schema与跨字段校验，不能只提供算术表。该清单未交付前不登记运行时契约发布或编码验收完成。

### 13.10 本次修订处置与验证边界

| 复审问题 | V0.3处置 |
|---|---|
| RV-01 | 首批组阶梯显式拒绝，保留组MOQ；单SKU报价行计费和聚合粒度固定，见5、13.1、13.3、13.9 |
| RV-02 | amount采用固定原始采购成本基数，禁止循环及动态累计基数，见6、13.4、13.9 |
| RV-03 | 真实报价来源分支及互斥采购条件来源，独立／依赖效力模式明确，见2、5、13.1、13.8 |
| RV-04 | working与active分别管理，逐命令CAS及目标修订、丢弃稿和旧版本撤销行为明确，见9、13.2 |
| RV-05 | 测量更正／撤销、当前效力、推导体积权威性与max_scope明确，见6、13.8 |
| RV-06 | 同场景版本可新键评估，追加结果；仅修改内容递增场景版本，见9、13.2 |
| RV-07 | 类型级最终业务上限、全局资源预算及有理数中间运算边界，见13.6、13.9 |
| RV-08 | 完整样例、哈希向量、权限矩阵列为机器契约交付门槛，未伪称已验证，见13.7、13.9 |

本轮验证仅覆盖规格一致性检查及可复现算术／拒绝预期；不代表已运行T02业务。若后续机器契约暴露未表达的业务分支，必须修订文档并重新交叉验证。组阶梯属于后续支持路径，不删除原产品对阶梯采购条件的总体要求，首批支持单SKU阶梯。


## 14. V0.3.1准入复核记录
+
+复核日期2026-10-22。结论：**可进入限定范围的候选机器契约制作与交叉验证；尚不具备运行时契约发布或业务完成验收条件。** 这是制作准入，不是无缺陷或生产可用证明。
+
+本轮同步改正阶梯范围枚举与首批支持冲突，统一精确有理数／Decimal路径，固定体积精度，并将来源实质否认的失效路径落实为来源链阻断和明确条件停用。复核记录不改变此前针对V0.1及V0.2的不通过结论。
+
+| 复核项 | 本版规范结果 | 必须由下一阶段提供的证据 |
+|---|---|---|
+| RV-01 组阶梯 | 首批显式不支持，单SKU阶梯及跨SKU组MOQ支持范围分开 | schema及业务校验不能静默降级计算 |
+| RV-02 费用基数 | 按采购成本固定基数，整数余数分摊，禁止循环 | 顺序交换及拆分需求行的结果一致性案例 |
+| RV-03 报价来源 | 响应／主动／价目分支与采购来源oneOf明确 | 完整来源链、效力模式、否认事实及当前读权案例 |
+| RV-04 修订根 | working／active分离，CAS和旧版撤销行为明确 | DDL唯一约束、状态矩阵、并发及回滚案例 |
+| RV-05 测量 | 当前效力、纠错撤销、计费max层级与体积精度明确 | 包装层级、混装拒绝、估算政策及测量失效案例 |
+| RV-06 重评估 | 同版本允许新动作键新结果，不改场景状态 | 重放不重复计算与新评估的区别案例 |
+| RV-07 数值及资源 | 最终业务上限与中间有理数预算分离 | 边界输入、资源拒绝及非整除舍入黄金样例 |
+| RV-08 规范表达 | 完整样例／权限矩阵／哈希向量作为强制交付物 | 实际schema、DDL及跨字段校验输出，不能以文字替代 |
+
+候选制作必须先补齐的基础能力适配：ServicePartyRole及MeasurementSnapshot的新存储与访问范围；来源否认事实的授权写入／查询／审计；pricing_policy.allow_verified_estimates的版本化扩展；KG／M／M3标准单位及转换依据。现有字典不能表达未交付依赖时，相应路径返回DEPENDENCY_NOT_SUPPORTED，不绕过校验。旧pricing_policy没有估算开关时按false处理；新开关必须通过新版封闭配置契约或明确扩展契约交付，不能向旧schema偷偷加字段。
+
+权限矩阵至少区分编制、核实、独立发布、撤销、评估、冻结及历史读取，覆盖人工／AI、own／team／company和来源依赖；来源否认只允许授权人工登记并提供证据，不能由AI相似度判断自行生效。来源否认记录ID、目标来源、理由、证据、服务器操作者与时点；不能把失去读取权限混同为来源被否认。重新恢复效力须新的核实／发布，不能删除否认事件复活旧结论。
+
+下阶段任一模型不能表达上述规则，或有效／无效样例出现相反结果，应返回规格修订；不凭本轮制作准入继续发布或上线。范围仍为T02局部，不含正式报价、订单、自动发运或完整AI业务链。
