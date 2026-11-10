# ADR-BR-IMP-001 · IDP Brant Go 后端基线（候选）

- **状态**：PROPOSED。负责人已明确开发方向为 Go，并选择 idp-brantgo 根目录作为 Go Module（2026-11-06）；正式架构签核未完成。
- **版本**：V1.0，2026-11-06。
- **背景**：Brant 现有业务核心是 Python／SQLite 参考实现（IDP_Brant_T04_A46采购准备_V0.50，54 个模块、21,136 行、577 个唯一测试方法，679 项 unittest 于 2026-11-06 在源码副本上重跑通过）。产品要求按 IDP-Cargo 主档案的标准，以 Go 完整实现。
- **决策建议**：业务后端 Go-first；首发为模块化单体＋六边形架构＋战略／战术 DDD；18 个候选上下文是模型边界，不是部署单元；不强制微服务或 Event Sourcing。
- **候选组合**：Go 1.26.x 精确版本（与 idp-bento-go 一致；本机 go1.26.8）／net/http／PostgreSQL 16／pgx/v5／sqlc／brant-migrate（沿用 bento 迁移资产）／OpenAPI 3.1／JSON Schema 2020-12／OIDC／Transactional Outbox-Inbox／go.idp.xyz/idp-bento-go v0.1.0-rc.2。
- **平台复用**：idp-bento-go 作为 Go 框架依赖（不复制、不修改）；idp-platform-go v0.1.0-rc.1 的编号能力是否采用待 IG-09；EER、BIP、CMI、MCP 通过 internal/ai 的适配器接入，不复制其运行时。
- **金额**：最小单位整数＋精确有理数，或经验证的十进制库；禁止 float64；必须与 Python 规则逐分一致（ADR-BR-REPO-10）。
- **一致性**：每个上下文独立事务 owner；跨上下文资格重查的实现方式待 ADR-BR-004 裁决；PostgreSQL 隔离与锁序见 ADR-BR-011。
- **验收**：gofmt＋go vet＋go test＋go test -race；真实 PG16 迁移、RLS 与事务测试；契约与负向载荷测试；Python 577 个唯一测试方法逐项映射、移植并在 Go CI 上通过，映射不能自动算通过。
- **未批准项**：精确 Go 版本、第三方库锁定、十进制实现、授权组织与岗位、外部 Port Owner、税务与外汇规则、G1 授权。
- **回退策略**：如某项既有平台能力无法从 Go 直接复用，优先稳定 API 适配器或 Go 原生受控实现；不得让 Brant 领域层反向依赖 Python 进程内模型。
- **事实依据**：负责人在 2026-11-06 的会话中选择“idp-brantgo 根目录为 Go Module、docs/ 为主档案”和“一次性完成 01—12 初稿”；idp-bento-go v0.1.0-rc.2 源码 postgres/transaction.go；Go 官方模块布局 https://go.dev/doc/modules/layout 。
