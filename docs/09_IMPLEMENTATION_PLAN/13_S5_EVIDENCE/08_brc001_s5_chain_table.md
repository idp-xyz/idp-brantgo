## The chain by step

| step | phase | actor(s) | commands → outcome | reads |
|---|---|---|---|---|
| S3-12 (ES-01 1–19) | EVD and POL foundation | admin, config.reviewer, sales | evd source.register (admin) → registered; pol config.save_draft (admin) → working ×20; pol config.publish (config.reviewer) → active ×20 | 1 |
| S3-12 (ES-01 1–19) | S1 front | sales | evd source.register (sales) → registered; pty party.save_draft (sales) → draft; pty party.confirm (sales) → confirmed; pty contact.save_draft (sales) → draft; pty contact.confirm (sales) → confirmed; cat sku.save_draft (sales) → draft ×4; cat sku.confirm (sales) → confirmed ×4; inq rfq.save_draft (sales) → draft; inq rfq.confirm_scope (sales) → ready | 2 |
| S3-12 (ES-01 1–19) | PTY trading profile and screening | sales, config.reviewer, compliance | pty trading_profile.save_draft (sales) → working; pty trading_profile.publish (config.reviewer) → active; ref screening_list.import (compliance) → imported; pty party_screening.record (compliance) → clear | 1 |
| S3-12 (ES-01 1–19) | SUP supply conditions | buyer, cost.checker | evd source.register (buyer) → registered; pty party.save_draft (buyer) → draft ×3; pty party.confirm (buyer) → confirmed ×3; sup supplier_rfq.save_draft (buyer) → draft ×2; sup supplier_rfq.record_issued (buyer) → issued ×2; sup supplier_offer.save_draft (buyer) → draft ×2; sup supplier_offer.verify (cost.checker) → active ×2; pty service_role.verify (buyer) → verified; sup logistics_offer.save_draft (buyer) → draft; sup logistics_offer.verify (cost.checker) → active | 2 |
| S3-12 (ES-01 1–19) | CST costing | buyer, cost.checker, sales | cst costing_context.save_draft (buyer) → working; cst costing_context.verify (cost.checker) → active; cst cost_scenario.save_draft (buyer) → draft ×2; cst cost_scenario.evaluate (cost.checker) → evaluated ×2; cst cost_snapshot.freeze (cost.checker) → frozen | 4 |
| S3-12 (ES-01 1–19) | TRD quote | sales, quote.reviewer | trd quote.save_draft (sales) → working; trd quote.freeze (sales) → frozen; trd quote.submit_review (sales) → pending_approval; trd quote.review.decide (sales) → INDEPENDENT_REVIEW_REQUIRED; trd quote.review.decide (quote.reviewer) → approved; trd quote.activate (sales) → active | 3 |
| S3-12 (ES-01 1–19) | TRD customer acceptance | sales, acceptance.verifier | evd source.register (sales) → registered; trd quote.response.record (sales) → recorded; trd quote.response.verify (acceptance.verifier) → valid | 2 |
| §5 step 6 (S3 chain) | TRD sales order | sales, order.reviewer | trd order.prepare (sales) → draft; trd order.review.request (sales) → open; trd order.review.decide (sales) → REVIEWER_NOT_INDEPENDENT; trd order.review.decide (order.reviewer) → approved; trd order.activate (sales) → active | 4 |
| §5 steps 4–5 (S3 chain) | CNX trade compliance | compliance, doc.1, doc.2, sales | ref country_programme.import (compliance) → imported ×2; cat declaration_profile.save_draft (doc.1) → draft ×4; cat declaration_profile.verify (doc.2) → verified ×4; cnx trade_compliance.evaluate (compliance) → pending; cnx trade_compliance.decide (compliance) → clear | 2 |
| §5 steps 1–3 (S3 chain) | STL credit | admin, config.reviewer, sales, credit.manager.1, credit.manager.2 | pol config.save_draft (admin) → working; pol config.publish (config.reviewer) → active; stl credit_limit.request (sales) → requested; stl credit_limit.publish (credit.manager.1) → requested; stl credit_limit.publish (credit.manager.2) → published; stl credit.reserve (sales) → reserved | 1 |
| S3-12 (ES-01 19) | PUR purchase draft | purchaser | pur purchase.draft.prepare (purchaser) → prepared | 0 |
| S3-12 | read back | sales, purchaser |  | 7 |
| S3-12 (BC-AT15) | 有效订单唯一 | sales, purchaser | trd order.prepare (sales) → CONVERSION_SLOT_CONFLICT; pur purchase.draft.prepare (purchaser) → PURCHASE_ALLOCATION_EXCEEDED | 4 |
| §5 step 1 | POL configurations of the S5 steps | admin, config.reviewer | pol config.save_draft (admin) → working ×9; pol config.publish (config.reviewer) → active ×9; evd source.register (admin) → registered | 0 |
| §5 step 6 | STL consumers of OrderActivated | sales, treasury |  | 2 |
| §5 step 7 | STL receipt of P1 and its application | doc.1, treasury | evd source.register (doc.1) → registered; stl fund.record (treasury) → recorded; stl application.apply (treasury) → applied | 1 |
| §5 step 8 | PUR purchase commitment | purchaser, order.reviewer | pur purchase.commitment.request (purchaser) → requested; pur purchase.commitment.decide (order.reviewer) → approved; evd source.register (purchaser) → registered ×2; pur purchase.commitment.issue (purchaser) → issued; pur purchase.supplier_confirmation.record (purchaser) → confirmed; pur purchase.draft.void (purchaser) → PURCHASE_DRAFT_COMMITTED | 3 |
| §5 step 9 | QLT inspection and quality release | buyer, qc | pty party.save_draft (buyer) → draft; pty party.confirm (buyer) → confirmed; pty service_role.verify (buyer) → verified; evd source.register (qc) → registered; evd file.register (qc) → registered; qlt inspection.record (qc) → recorded ×4; qlt quality_release.grant (qc) → granted ×4 | 4 |
| §5 step 10 | FUL milestones | merchandiser | ful milestone.plan (merchandiser) → active; evd source.register (merchandiser) → registered; ful milestone.record_actual (merchandiser) → active | 1 |
| §5 step 11 | PUR supplier delivery | merchandiser, purchaser | evd source.register (merchandiser) → registered; pur supplier_delivery.record (merchandiser) → recorded; pur supplier_delivery.verify (purchaser) → verified | 1 |
| §5 step 12 | FUL shipment, packing and booking | merchandiser | ful shipment.prepare (merchandiser) → prepared; ful packing.plan (merchandiser) → prepared; ful packing.record_actual (merchandiser) → prepared; evd source.register (merchandiser) → registered; ful booking.record (merchandiser) → booked | 1 |
| §5 step 13 | FUL commercial release of the shipment | doc.1 | ful commercial_release.evaluate (doc.1) → granted | 1 |
| §5 step 14 | FUL shipment confirmation | merchandiser | ful shipment.confirm (merchandiser) → confirmed | 2 |
| §5 step 15 | FUL shipment documents | doc.1 | ful shipment_document.prepare (doc.1) → draft ×3; evd file.register (doc.1) → registered; ful shipment_document.issue (doc.1) → issued | 3 |
| §5 step 16 | FUL documents release | doc.1 | ful commercial_release.evaluate (doc.1) → granted; evd source.register (doc.1) → registered; ful document_release.record (doc.1) → consumed | 2 |
| §5 step 17 | FUL export lot | merchandiser | ful export_lot.register (merchandiser) → registered | 1 |
| §5 step 18 | CNX customs declaration | sales, doc.1, merchandiser | trd order.document.generate (sales) → generated; cnx declaration_case.prepare (doc.1) → draft; cnx declaration_case.confirm (doc.1) → confirmed; evd source.register (doc.1) → registered ×2; cnx declaration_case.record_submission (doc.1) → submitted; cnx declaration_case.record_receipt (doc.1) → released | 3 |
| §5 step 19 | STL sales statement and receivable | treasury, credit.officer | stl settlement.confirm (treasury) → confirmed | 3 |
| §5 step 20 | STL collection of P2 with a bank charge | doc.1, treasury, credit.manager.1, credit.officer | evd source.register (doc.1) → registered; stl fund.record (treasury) → recorded; stl application.apply (treasury) → FUND_DIFFERENCE_UNEXPLAINED; stl fund.resolve_difference (credit.manager.1) → difference_resolved; stl application.apply (treasury) → applied ×2 | 3 |
| §5 step 21 | STL purchase statement, payable and payment | treasury, doc.1 | stl settlement.confirm (treasury) → confirmed; evd source.register (doc.1) → registered; stl fund.record (treasury) → recorded; stl application.apply (treasury) → applied | 2 |
| §5 step 22 | CNX purchase invoice | purchaser, tax.officer | evd file.register (purchaser) → registered; evd source.register (purchaser) → registered; cnx purchase_invoice.import (tax.officer) → imported; cnx purchase_invoice.verify (tax.officer) → verified | 1 |
| §5 step 23 | CNX tax refund case | tax.officer | cnx tax_refund_case.allocate (tax.officer) → expected; cnx tax_refund_case.record_result (tax.officer) → expected | 2 |
| §5 step 24 | CNX document bundle and external reconciliation | doc.1, tax.officer | evd source.register (doc.1) → registered; cnx document_bundle.check (doc.1) → complete; cnx external_reconciliation.resolve (tax.officer) → complete | 1 |
| §5 step 25 | TRD order close | merchandiser | trd order.close (merchandiser) → closed | 3 |
| §5 step 25 | STL consumer of OrderClosed | sales, credit.officer |  | 2 |
| §5 step 26 | STL freight charge | credit.officer, merchandiser, treasury | stl charge.record (credit.officer) → estimated; evd source.register (merchandiser) → registered; stl charge.confirm (treasury) → confirmed; stl charge.allocate (credit.officer) → confirmed | 1 |
| §5 step 27 | STL order result | treasury | stl order_result.review (treasury) → reviewed; stl order_result.close (treasury) → settled | 1 |
| §5 final assertions | §5 final assertions | sales, merchandiser, treasury |  | 6 |

## Event relay runs (Platform.Relay → messaging.Dispatcher of Contexts.Subscriptions)

| when | after request | events | Inbox records (consumer ← event type × n, each PROCESSED) | redelivered without effect | facts |
|---|---|---|---|---|---|
| after the activation (S3 chain, §5 step 6) | 172 | 80 | cnx.trade_compliance ← idp-brant.pol.config_published.v1 ×30; cnx.trade_compliance ← idp-brant.ref.country_programmes_imported.v1 ×2; stl.order_account ← idp-brant.trd.order_activated.v1 ×1; stl.order_credit ← idp-brant.trd.order_activated.v1 ×1 | 80 | {"order_account": "open", "reservation": "consumed"} |
| after order.close (§5 step 25) | 285 | 60 | stl.order_credit ← idp-brant.trd.order_closed.v1 ×1 | 60 | {"reservation": "released"} |
| at the end of the chain | 295 | 4 | none subscribed | 4 | {} |

## Every request

| # | phase | actor | request | status | object / row version / result | facts |
|---|---|---|---|---|---|---|
| 1 | EVD and POL foundation | admin | POST evd/commands/source.register | 200 | evidence rv 1 registered |  |
| 2 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | currency:USD rv 1 working |  |
| 3 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | currency:USD rv 2 active |  |
| 4 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | unit:PCS rv 1 working |  |
| 5 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | unit:PCS rv 2 active |  |
| 6 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_profile:tax.zero rv 1 working |  |
| 7 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_profile:tax.zero rv 2 active |  |
| 8 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | fee_category:freight rv 1 working |  |
| 9 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | fee_category:freight rv 2 active |  |
| 10 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:purchase.ST01.red.S rv 1 working |  |
| 11 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:purchase.ST01.red.S rv 2 active |  |
| 12 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:sales.ST01.red.S rv 1 working |  |
| 13 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:sales.ST01.red.S rv 2 active |  |
| 14 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:purchase.ST01.red.M rv 1 working |  |
| 15 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:purchase.ST01.red.M rv 2 active |  |
| 16 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:sales.ST01.red.M rv 1 working |  |
| 17 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:sales.ST01.red.M rv 2 active |  |
| 18 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:purchase.ST01.blue.S rv 1 working |  |
| 19 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:purchase.ST01.blue.S rv 2 active |  |
| 20 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:sales.ST01.blue.S rv 1 working |  |
| 21 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:sales.ST01.blue.S rv 2 active |  |
| 22 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:purchase.ST01.blue.M rv 1 working |  |
| 23 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:purchase.ST01.blue.M rv 2 active |  |
| 24 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:sales.ST01.blue.M rv 1 working |  |
| 25 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:sales.ST01.blue.M rv 2 active |  |
| 26 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | tax_application:tax.service rv 1 working |  |
| 27 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | tax_application:tax.service rv 2 active |  |
| 28 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | payment_terms:payment.30-70 rv 1 working |  |
| 29 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | payment_terms:payment.30-70 rv 2 active |  |
| 30 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | trade_terms:FCA rv 1 working |  |
| 31 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | trade_terms:FCA rv 2 active |  |
| 32 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | pricing_policy:policy.1 rv 1 working |  |
| 33 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | pricing_policy:policy.1 rv 2 active |  |
| 34 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | company_profile:company.A rv 1 working |  |
| 35 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | company_profile:company.A rv 2 active |  |
| 36 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | quote_document_template:quote.template rv 1 working |  |
| 37 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | quote_document_template:quote.template rv 2 active |  |
| 38 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | number_sequence:quote.seq rv 1 working |  |
| 39 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | number_sequence:quote.seq rv 2 active |  |
| 40 | EVD and POL foundation | admin | POST pol/commands/config.save_draft | 200 | screening_regulation:screening rv 1 working |  |
| 41 | EVD and POL foundation | config.reviewer | POST pol/commands/config.publish | 200 | screening_regulation:screening rv 2 active |  |
| 42 | EVD and POL foundation | sales | GET pol/configs/payment_terms/payment.30-70 | 200 |  | {"active_revision": 1} |
| 43 | S1 front | sales | POST evd/commands/source.register | 200 | source.1 rv 1 registered |  |
| 44 | S1 front | sales | POST pty/commands/party.save_draft | 200 | customer.1 rv 1 draft |  |
| 45 | S1 front | sales | POST pty/commands/party.confirm | 200 | customer.1 rv 2 confirmed |  |
| 46 | S1 front | sales | POST pty/commands/contact.save_draft | 200 | contact.1 rv 1 draft |  |
| 47 | S1 front | sales | POST pty/commands/contact.confirm | 200 | contact.1 rv 2 confirmed |  |
| 48 | S1 front | sales | GET pty/parties/customer.1 | 200 |  | {"addresses": 2, "revision": 1} |
| 49 | S1 front | sales | POST cat/commands/sku.save_draft | 200 | ST01.red.S rv 1 draft |  |
| 50 | S1 front | sales | POST cat/commands/sku.confirm | 200 | ST01.red.S rv 2 confirmed |  |
| 51 | S1 front | sales | POST cat/commands/sku.save_draft | 200 | ST01.red.M rv 1 draft |  |
| 52 | S1 front | sales | POST cat/commands/sku.confirm | 200 | ST01.red.M rv 2 confirmed |  |
| 53 | S1 front | sales | POST cat/commands/sku.save_draft | 200 | ST01.blue.S rv 1 draft |  |
| 54 | S1 front | sales | POST cat/commands/sku.confirm | 200 | ST01.blue.S rv 2 confirmed |  |
| 55 | S1 front | sales | POST cat/commands/sku.save_draft | 200 | ST01.blue.M rv 1 draft |  |
| 56 | S1 front | sales | POST cat/commands/sku.confirm | 200 | ST01.blue.M rv 2 confirmed |  |
| 57 | S1 front | sales | POST inq/commands/rfq.save_draft | 200 | rfq.1 rv 1 draft |  |
| 58 | S1 front | sales | POST inq/commands/rfq.confirm_scope | 200 | rfq.1 rv 2 ready |  |
| 59 | S1 front | sales | GET inq/rfqs/rfq.1 | 200 |  | {"quantities": ["24", "36", "12", "48"], "row_version": 2, "status": "ready"} |
| 60 | PTY trading profile and screening | sales | POST pty/commands/trading_profile.save_draft | 200 | customer.1 rv 1 working |  |
| 61 | PTY trading profile and screening | config.reviewer | POST pty/commands/trading_profile.publish | 200 | customer.1 rv 2 active |  |
| 62 | PTY trading profile and screening | compliance | POST ref/commands/screening_list.import | 200 | cn.uel:v1 rv 1 imported |  |
| 63 | PTY trading profile and screening | compliance | POST pty/commands/party_screening.record | 200 | customer.1 rv 1 clear |  |
| 64 | PTY trading profile and screening | sales | GET pty/parties/customer.1/trading_profile | 200 |  | {"active_revision": 1} |
| 65 | SUP supply conditions | buyer | POST evd/commands/source.register | 200 | supplier.quotes rv 1 registered |  |
| 66 | SUP supply conditions | buyer | POST pty/commands/party.save_draft | 200 | supplier.A rv 1 draft |  |
| 67 | SUP supply conditions | buyer | POST pty/commands/party.confirm | 200 | supplier.A rv 2 confirmed |  |
| 68 | SUP supply conditions | buyer | POST sup/commands/supplier_rfq.save_draft | 200 | request.A rv 1 draft |  |
| 69 | SUP supply conditions | buyer | POST sup/commands/supplier_rfq.record_issued | 200 | request.A rv 2 issued |  |
| 70 | SUP supply conditions | buyer | POST sup/commands/supplier_offer.save_draft | 200 | offer.A rv 1 draft |  |
| 71 | SUP supply conditions | cost.checker | POST sup/commands/supplier_offer.verify | 200 | offer.A rv 2 active |  |
| 72 | SUP supply conditions | buyer | GET sup/supplier_offers/offer.A | 200 |  | {"active_revision": 1, "unit_price": "5.00"} |
| 73 | SUP supply conditions | buyer | POST pty/commands/party.save_draft | 200 | supplier.B rv 1 draft |  |
| 74 | SUP supply conditions | buyer | POST pty/commands/party.confirm | 200 | supplier.B rv 2 confirmed |  |
| 75 | SUP supply conditions | buyer | POST sup/commands/supplier_rfq.save_draft | 200 | request.B rv 1 draft |  |
| 76 | SUP supply conditions | buyer | POST sup/commands/supplier_rfq.record_issued | 200 | request.B rv 2 issued |  |
| 77 | SUP supply conditions | buyer | POST sup/commands/supplier_offer.save_draft | 200 | offer.B rv 1 draft |  |
| 78 | SUP supply conditions | cost.checker | POST sup/commands/supplier_offer.verify | 200 | offer.B rv 2 active |  |
| 79 | SUP supply conditions | buyer | GET sup/supplier_offers/offer.B | 200 |  | {"active_revision": 1, "unit_price": "5.30"} |
| 80 | SUP supply conditions | buyer | POST pty/commands/party.save_draft | 200 | provider.1 rv 1 draft |  |
| 81 | SUP supply conditions | buyer | POST pty/commands/party.confirm | 200 | provider.1 rv 2 confirmed |  |
| 82 | SUP supply conditions | buyer | POST pty/commands/service_role.verify | 200 | provider.1 rv 3 verified |  |
| 83 | SUP supply conditions | buyer | POST sup/commands/logistics_offer.save_draft | 200 | log.1 rv 1 draft |  |
| 84 | SUP supply conditions | cost.checker | POST sup/commands/logistics_offer.verify | 200 | log.1 rv 2 active |  |
| 85 | CST costing | buyer | POST cst/commands/costing_context.save_draft | 200 | context.1 rv 1 working |  |
| 86 | CST costing | cost.checker | POST cst/commands/costing_context.verify | 200 | context.1 rv 2 active |  |
| 87 | CST costing | buyer | POST cst/commands/cost_scenario.save_draft | 200 | scenario.A rv 1 draft |  |
| 88 | CST costing | cost.checker | POST cst/commands/cost_scenario.evaluate | 200 | eval-01a1d2a8-630b-78df-a092-b97fdfeed75c rv 1 evaluated |  |
| 89 | CST costing | cost.checker | GET cst/cost_evaluations/eval-01a1d2a8-630b-78df-a092-b97fdfeed75c | 200 |  | {"comparison_status": "comparable", "evaluation_id": "eval-01a1d2a8-630b-78df-a092-b97fdfeed75c", "result_hash": "d9a0377071a827bc4972f3fbc8228c2a2cd26d5a38949551abd72b31e3dc05ad", "total": "660.00 USD", "total_minor": "66000"} |
| 90 | CST costing | buyer | POST cst/commands/cost_scenario.save_draft | 200 | scenario.B rv 1 draft |  |
| 91 | CST costing | cost.checker | POST cst/commands/cost_scenario.evaluate | 200 | eval-01a1d2a8-6361-7734-b61d-365da4a1daad rv 1 evaluated |  |
| 92 | CST costing | cost.checker | GET cst/cost_evaluations/eval-01a1d2a8-6361-7734-b61d-365da4a1daad | 200 |  | {"comparison_status": "comparable", "evaluation_id": "eval-01a1d2a8-6361-7734-b61d-365da4a1daad", "result_hash": "67ad5350d4c8a60a76804ada93c6a432f8e495321957233ead21a3f315faa865", "total": "696.00 USD", "total_minor": "69600"} |
| 93 | CST costing | cost.checker | POST cst/commands/cost_snapshot.freeze | 200 | snap-01a1d2a8-639d-768f-90f5-ee5f667f827b rv 2 frozen |  |
| 94 | CST costing | cost.checker | GET cst/cost_snapshots/snap-01a1d2a8-639d-768f-90f5-ee5f667f827b | 200 |  | {"result_hash": "d9a0377071a827bc4972f3fbc8228c2a2cd26d5a38949551abd72b31e3dc05ad", "snapshot_id": "snap-01a1d2a8-639d-768f-90f5-ee5f667f827b", "total": "660.00 USD", "total_minor": "66000"} |
| 95 | CST costing | sales | GET cst/cost_snapshots/snap-01a1d2a8-639d-768f-90f5-ee5f667f827b/adoptability | 200 |  | {"currently_eligible_for_quote": true, "status": "adoptable"} |
| 96 | TRD quote | sales | POST trd/commands/quote.save_draft | 200 | quote.1 rv 1 working |  |
| 97 | TRD quote | sales | POST trd/commands/quote.freeze | 200 | quote.1 rv 2 frozen |  |
| 98 | TRD quote | sales | GET trd/quotes/quote.1 | 200 |  | {"business_hash": "b14048331942262eaa716288391c8c4a99e9693c8a1c68cdfe442326f2762a2b", "business_revision": 1, "cost_minor": "66000", "margin_rule": "pass", "minimum_margin": "0.25", "net_minor": "96000", "payable": "960.00 USD", "payable_minor": "96000", "payments_minor": ["28800", "67200"], "profit_minor": "30000"} |
| 99 | TRD quote | sales | POST trd/commands/quote.submit_review | 200 | quote.1 rv 3 pending_approval |  |
| 100 | TRD quote | sales | POST trd/commands/quote.review.decide | 422 | INDEPENDENT_REVIEW_REQUIRED | {"field_errors": [{"path": "/args/review_request_id", "reason": "The reviewer contributed to, froze or submitted the revision"}]} |
| 101 | TRD quote | quote.reviewer | POST trd/commands/quote.review.decide | 200 | quote.1 rv 4 approved |  |
| 102 | TRD quote | sales | GET trd/quotes/quote.1/approvals/approval.quote.1/check | 200 |  | {"releasable": true} |
| 103 | TRD quote | sales | POST trd/commands/quote.activate | 200 | quote.1 rv 5 active |  |
| 104 | TRD quote | sales | GET trd/quotes/quote.1/ready | 200 |  | {"business_revision": 1, "ready": true} |
| 105 | TRD customer acceptance | sales | POST evd/commands/source.register | 200 | mail rv 1 registered |  |
| 106 | TRD customer acceptance | sales | POST trd/commands/quote.response.record | 200 | response.1 rv 1 recorded |  |
| 107 | TRD customer acceptance | acceptance.verifier | POST trd/commands/quote.response.verify | 200 | acceptance.1 rv 1 valid |  |
| 108 | TRD customer acceptance | sales | GET trd/customer_responses/response.1 | 200 |  | {"business_hash": "b14048331942262eaa716288391c8c4a99e9693c8a1c68cdfe442326f2762a2b", "classification": "full_unconditional"} |
| 109 | TRD customer acceptance | sales | GET trd/acceptances/acceptance.1 | 200 |  | {"arrival_at": "2026-11-13T02:06:53.703549Z", "effectivity": "valid", "expires_at": "2026-11-16T02:06:53.416679Z", "formation_rule": "arrival", "formed": true, "number": "QT-2026-00001"} |
| 110 | TRD sales order | sales | POST trd/commands/order.prepare | 200 | order.1 rv 1 draft |  |
| 111 | TRD sales order | sales | GET trd/orders/order.1 | 200 |  | {"order_hash": "24ce1a1f111d25aeafe710396cc639f165222ef89ddb82dbe4335aa03066d3aa", "order_revision": 1, "slot": "full_v1"} |
| 112 | TRD sales order | sales | POST trd/commands/order.review.request | 200 | order.1 rv 2 open |  |
| 113 | TRD sales order | sales | POST trd/commands/order.review.decide | 422 | REVIEWER_NOT_INDEPENDENT | {"field_errors": [{"path": "/args/review_id", "reason": "The reviewer prepared the order or requested the review"}]} |
| 114 | TRD sales order | order.reviewer | POST trd/commands/order.review.decide | 200 | order.1 rv 3 approved |  |
| 115 | TRD sales order | sales | GET trd/orders/order.1/check?review_id=review.order.1 | 200 | TRADE_COMPLIANCE_NOT_CLEAR | {"activatable": false, "code": "TRADE_COMPLIANCE_NOT_CLEAR"} |
| 116 | CNX trade compliance | compliance | POST ref/commands/country_programme.import | 200 | cn.country_programmes:v1 rv 1 imported |  |
| 117 | CNX trade compliance | compliance | POST ref/commands/country_programme.import | 200 | un.country_programmes:v1 rv 1 imported |  |
| 118 | CNX trade compliance | doc.1 | POST cat/commands/declaration_profile.save_draft | 200 | decl.ST01.red.S rv 1 draft |  |
| 119 | CNX trade compliance | doc.2 | POST cat/commands/declaration_profile.verify | 200 | decl.ST01.red.S rv 2 verified |  |
| 120 | CNX trade compliance | doc.1 | POST cat/commands/declaration_profile.save_draft | 200 | decl.ST01.red.M rv 1 draft |  |
| 121 | CNX trade compliance | doc.2 | POST cat/commands/declaration_profile.verify | 200 | decl.ST01.red.M rv 2 verified |  |
| 122 | CNX trade compliance | doc.1 | POST cat/commands/declaration_profile.save_draft | 200 | decl.ST01.blue.S rv 1 draft |  |
| 123 | CNX trade compliance | doc.2 | POST cat/commands/declaration_profile.verify | 200 | decl.ST01.blue.S rv 2 verified |  |
| 124 | CNX trade compliance | doc.1 | POST cat/commands/declaration_profile.save_draft | 200 | decl.ST01.blue.M rv 1 draft |  |
| 125 | CNX trade compliance | doc.2 | POST cat/commands/declaration_profile.verify | 200 | decl.ST01.blue.M rv 2 verified |  |
| 126 | CNX trade compliance | compliance | POST cnx/commands/trade_compliance.evaluate | 200 | tcc-01a1d2a8-6674-7bf5-b251-a3c7d6cd31c0 rv 1 pending |  |
| 127 | CNX trade compliance | compliance | GET cnx/trade_compliance_checks/tcc-01a1d2a8-6674-7bf5-b251-a3c7d6cd31c0 | 200 |  |  |
| 128 | CNX trade compliance | compliance | POST cnx/commands/trade_compliance.decide | 200 | tcc-01a1d2a8-6674-7bf5-b251-a3c7d6cd31c0 rv 2 clear | {"check_id": "tcc-01a1d2a8-6674-7bf5-b251-a3c7d6cd31c0", "conclusion": "clear", "order_revision": 1} |
| 129 | CNX trade compliance | sales | GET trd/orders/order.1/check?review_id=review.order.1 | 200 | CREDIT_RESERVATION_REQUIRED | {"activatable": false, "code": "CREDIT_RESERVATION_REQUIRED"} |
| 130 | STL credit | admin | POST pol/commands/config.save_draft | 200 | credit_policy:credit.usd rv 1 working |  |
| 131 | STL credit | config.reviewer | POST pol/commands/config.publish | 200 | credit_policy:credit.usd rv 2 active |  |
| 132 | STL credit | sales | POST stl/commands/credit_limit.request | 200 | limit.1 rv 1 requested |  |
| 133 | STL credit | credit.manager.1 | POST stl/commands/credit_limit.publish | 200 | limit.1 rv 2 requested |  |
| 134 | STL credit | credit.manager.2 | POST stl/commands/credit_limit.publish | 200 | limit.1 rv 3 published |  |
| 135 | STL credit | sales | POST stl/commands/credit.reserve | 200 | res.order.1 rv 1 reserved |  |
| 136 | STL credit | sales | GET stl/credit_reservations/res.order.1 | 200 |  | {"basis": "within_limit", "exposure_minor": "67200", "portion_minor": "67200"} |
| 137 | TRD sales order | sales | GET trd/orders/order.1/check?review_id=review.order.1 | 200 |  | {"activatable": true, "unchecked": null} |
| 138 | TRD sales order | sales | POST trd/commands/order.activate | 200 | order.1 rv 4 active |  |
| 139 | TRD sales order | sales | GET trd/orders/order.1 | 200 |  | {"credit_guard": "covered", "credit_portion_minor": "67200", "credit_reservation_id": "res.order.1", "trade_compliance": "clear/cleared/tcc-01a1d2a8-6674-7bf5-b251-a3c7d6cd31c0/tcd-01a1d2a8-6692-7fe0-8f57-04c638722538/false"} |
| 140 | PUR purchase draft | purchaser | POST pur/commands/purchase.draft.prepare | 200 | purchase.1 rv 1 prepared |  |
| 141 | read back | sales | GET trd/quotes/quote.1 | 200 |  | {"active_revision": 1, "conversion_order_id": "order.1", "conversion_slot": "full_v1", "payable_minor": "96000"} |
| 142 | read back | sales | GET trd/quotes/quote.1/active | 200 |  | {"conversion_taken": true, "document_sha256": "57cb8f149ab167256bb111d570df37dead4b89d550eb63418d0ce80405fd16c9", "number": "QT-2026-00001"} |
| 143 | read back | sales | GET trd/quotes/quote.1/revisions/1/document | 200 |  | {"bytes": 1260, "document_sha256": "57cb8f149ab167256bb111d570df37dead4b89d550eb63418d0ce80405fd16c9", "number": "QT-2026-00001", "renderer": "quote-text/0.1.0", "states": ["Net: 96000 \| Tax: 0 \| Payable: 96000", "P1 \| 30% \| 28800", "P2 \| 70% \| 67200"]} |
| 144 | read back | sales | GET trd/quotes/quote.1/ready | 200 |  | {"ready": true, "reasons": null} |
| 145 | read back | sales | GET trd/orders/order.1 | 200 |  | {"cost_snapshot_id": "snap-01a1d2a8-639d-768f-90f5-ee5f667f827b", "line_payable_minor": ["19200", "28800", "9600", "38400"], "net_minor": "96000", "order_hash": "24ce1a1f111d25aeafe710396cc639f165222ef89ddb82dbe4335aa03066d3aa", "order_revision": 1, "payable": "960.00 USD", "payable_minor": "96000", "row_version": 4, "status": "active"} |
| 146 | read back | purchaser | GET pur/purchase_drafts/purchase.1 | 200 |  | {"commitment_state": "not_committed", "draft_hash": "307b7dc0027efe9be05024211bac2b88ad0a4d61b7d847bcf68919996752cff4", "gross_minor": "60000", "line_net_minor": ["12000", "18000", "6000", "24000"], "net": "600.00 USD", "net_minor": "60000", "order_hash": "24ce1a1f111d25aeafe710396cc639f165222ef89ddb82dbe4335aa03066d3aa", "order_id": "order.1", "order_revision": 1, "supplier_id": "supplier.A"} |
| 147 | read back | purchaser | GET pur/orders/order.1/allocations | 200 |  | {"allocated": ["24", "36", "12", "48"], "remaining": "0"} |
| 148 | 有效订单唯一 | sales | POST trd/commands/order.prepare | 409 | CONVERSION_SLOT_CONFLICT | {"field_errors": [{"path": "/args/quote_id", "reason": "The quote's full_v1 conversion slot is taken"}]} |
| 149 | 有效订单唯一 | sales | GET trd/orders/order.2 | 404 | NOT_FOUND |  |
| 150 | 有效订单唯一 | purchaser | POST pur/commands/purchase.draft.prepare | 409 | PURCHASE_ALLOCATION_EXCEEDED | {"field_errors": [{"path": "/args/lines/0/quantity", "reason": "Allocating 1 exceeds the remaining 0 of the order line's 24"}]} |
| 151 | 有效订单唯一 | purchaser | GET pur/purchase_drafts/purchase.2 | 404 | NOT_FOUND |  |
| 152 | 有效订单唯一 | purchaser | GET pur/orders/order.1/allocations | 200 |  | {"allocated": ["24", "36", "12", "48"], "remaining": "0"} |
| 153 | 有效订单唯一 | sales | GET trd/quotes/quote.1 | 200 |  | {"conversion_order_id": "order.1"} |
| 154 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | bank_account:bank.usd rv 1 working |  |
| 155 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | bank_account:bank.usd rv 2 active |  |
| 156 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | number_sequence:po.seq rv 1 working |  |
| 157 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | number_sequence:po.seq rv 2 active |  |
| 158 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | shipment_document_template:ship.ci.en rv 1 working |  |
| 159 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | shipment_document_template:ship.ci.en rv 2 active |  |
| 160 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | shipment_document_template:ship.pl.en rv 1 working |  |
| 161 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | shipment_document_template:ship.pl.en rv 2 active |  |
| 162 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | number_sequence:ci.seq rv 1 working |  |
| 163 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | number_sequence:ci.seq rv 2 active |  |
| 164 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | number_sequence:pl.seq rv 1 working |  |
| 165 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | number_sequence:pl.seq rv 2 active |  |
| 166 | §5 step 1: POL configurations of the S5 steps | admin | POST evd/commands/source.register | 200 | tax.review rv 1 registered |  |
| 167 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | export_tax_policy:export.tax rv 1 working |  |
| 168 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | export_tax_policy:export.tax rv 2 active |  |
| 169 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | order_document_template:order.sc rv 1 working |  |
| 170 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | order_document_template:order.sc rv 2 active |  |
| 171 | §5 step 1: POL configurations of the S5 steps | admin | POST pol/commands/config.save_draft | 200 | number_sequence:order.seq rv 1 working |  |
| 172 | §5 step 1: POL configurations of the S5 steps | config.reviewer | POST pol/commands/config.publish | 200 | number_sequence:order.seq rv 2 active |  |
| 173 | §5 step 6: STL consumers of OrderActivated | sales | GET stl/credit_reservations/res.order.1 | 200 |  | {"consumed_by": {"id": "inbox.stl.order_credit", "kind": "system"}, "portion_minor": "67200", "status": "consumed", "version": 2} |
| 174 | §5 step 6: STL consumers of OrderActivated | treasury | GET stl/order_accounts/order.1 | 200 |  | {"conditions": [{"applied_minor": "0", "reason": "stage_unpaid", "required_minor": "28800", "scope": "shipment", "shortfall_minor": "28800", "stage_id": "P1", "status": "blocked"}, {"applied_minor": "0", "reason": "stage_unpaid", "required_minor": "28800", "scope": "documents", "shortfall_minor": "28800", "stage_id": "P1", "status": "blocked"}], "order_revision": 1, "stages": 2} |
| 175 | §5 step 7: STL receipt of P1 and its application | doc.1 | POST evd/commands/source.register | 200 | bank.p1 rv 1 registered |  |
| 176 | §5 step 7: STL receipt of P1 and its application | treasury | POST stl/commands/fund.record | 200 | fund.p1 rv 1 recorded |  |
| 177 | §5 step 7: STL receipt of P1 and its application | treasury | POST stl/commands/application.apply | 200 | app.p1 rv 1 applied |  |
| 178 | §5 step 7: STL receipt of P1 and its application | treasury | GET stl/order_accounts/order.1 | 200 |  | {"conditions": [{"applied_minor": "28800", "required_minor": "28800", "scope": "shipment", "status": "satisfied"}, {"applied_minor": "28800", "required_minor": "28800", "scope": "documents", "status": "satisfied"}], "stage_P1_applied_minor": "28800"} |
| 179 | §5 step 8: PUR purchase commitment | purchaser | GET pur/purchase_drafts/purchase.1 | 200 |  |  |
| 180 | §5 step 8: PUR purchase commitment | purchaser | POST pur/commands/purchase.commitment.request | 200 | commitment.1 rv 1 requested |  |
| 181 | §5 step 8: PUR purchase commitment | order.reviewer | POST pur/commands/purchase.commitment.decide | 200 | commitment.1 rv 2 approved |  |
| 182 | §5 step 8: PUR purchase commitment | purchaser | POST evd/commands/source.register | 200 | po.sent rv 1 registered |  |
| 183 | §5 step 8: PUR purchase commitment | purchaser | POST pur/commands/purchase.commitment.issue | 200 | commitment.1 rv 3 issued |  |
| 184 | §5 step 8: PUR purchase commitment | purchaser | POST evd/commands/source.register | 200 | supplier.ack rv 1 registered |  |
| 185 | §5 step 8: PUR purchase commitment | purchaser | POST pur/commands/purchase.supplier_confirmation.record | 200 | commitment.1 rv 4 confirmed |  |
| 186 | §5 step 8: PUR purchase commitment | purchaser | GET pur/purchase_commitments/commitment.1 | 200 |  | {"draft_hash": "307b7dc0027efe9be05024211bac2b88ad0a4d61b7d847bcf68919996752cff4", "po_number": "PO-2026-00001", "status": "confirmed"} |
| 187 | §5 step 8: PUR purchase commitment | purchaser | POST pur/commands/purchase.draft.void | 409 | PURCHASE_DRAFT_COMMITTED | {"field_errors": [{"path": "/args/draft_id", "reason": "A draft that formed a purchase commitment is not voided"}]} |
| 188 | §5 step 8: PUR purchase commitment | purchaser | GET pur/purchase_drafts/purchase.1 | 200 |  | {"commitment_state": "committed", "net_minor": "60000"} |
| 189 | §5 step 9: QLT inspection and quality release | buyer | POST pty/commands/party.save_draft | 200 | agency.1 rv 1 draft |  |
| 190 | §5 step 9: QLT inspection and quality release | buyer | POST pty/commands/party.confirm | 200 | agency.1 rv 2 confirmed |  |
| 191 | §5 step 9: QLT inspection and quality release | buyer | POST pty/commands/service_role.verify | 200 | agency.1 rv 3 verified |  |
| 192 | §5 step 9: QLT inspection and quality release | qc | POST evd/commands/source.register | 200 | standard rv 1 registered |  |
| 193 | §5 step 9: QLT inspection and quality release | qc | POST evd/commands/file.register | 200 | report.final rv 1 registered |  |
| 194 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/inspection.record | 200 | insp.L1 rv 1 recorded |  |
| 195 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/quality_release.grant | 200 | rel.L1 rv 1 granted |  |
| 196 | §5 step 9: QLT inspection and quality release | qc | GET qlt/quality_releases/rel.L1 | 200 |  |  |
| 197 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/inspection.record | 200 | insp.L2 rv 1 recorded |  |
| 198 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/quality_release.grant | 200 | rel.L2 rv 1 granted |  |
| 199 | §5 step 9: QLT inspection and quality release | qc | GET qlt/quality_releases/rel.L2 | 200 |  |  |
| 200 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/inspection.record | 200 | insp.L3 rv 1 recorded |  |
| 201 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/quality_release.grant | 200 | rel.L3 rv 1 granted |  |
| 202 | §5 step 9: QLT inspection and quality release | qc | GET qlt/quality_releases/rel.L3 | 200 |  |  |
| 203 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/inspection.record | 200 | insp.L4 rv 1 recorded |  |
| 204 | §5 step 9: QLT inspection and quality release | qc | POST qlt/commands/quality_release.grant | 200 | rel.L4 rv 1 granted |  |
| 205 | §5 step 9: QLT inspection and quality release | qc | GET qlt/quality_releases/rel.L4 | 200 |  | {"released": 120} |
| 206 | §5 step 10: FUL milestones | merchandiser | POST ful/commands/milestone.plan | 200 | plan.order.1 rv 1 active |  |
| 207 | §5 step 10: FUL milestones | merchandiser | POST evd/commands/source.register | 200 | cargo.ready rv 1 registered |  |
| 208 | §5 step 10: FUL milestones | merchandiser | POST ful/commands/milestone.record_actual | 200 | plan.order.1 rv 2 active |  |
| 209 | §5 step 10: FUL milestones | merchandiser | GET ful/milestone_plans/plan.order.1 | 200 |  | {"actuals": 1, "evidence_level": "internal_verified", "node": "M3 packing_complete", "revision": null} |
| 210 | §5 step 11: PUR supplier delivery | merchandiser | POST evd/commands/source.register | 200 | forwarder.receipt rv 1 registered |  |
| 211 | §5 step 11: PUR supplier delivery | merchandiser | POST pur/commands/supplier_delivery.record | 200 | delivery.1 rv 1 recorded |  |
| 212 | §5 step 11: PUR supplier delivery | purchaser | POST pur/commands/supplier_delivery.verify | 200 | delivery.1 rv 2 verified |  |
| 213 | §5 step 11: PUR supplier delivery | purchaser | GET pur/purchase_commitments/commitment.1/deliveries | 200 |  | {"deliveries": [{"order_line_id": "L1", "purchase_quantity": "24", "remaining_quantity": "0", "sku_id": "ST01.red.S", "spec_revision": 1, "unit_id": "PCS", "unit_revision": 1, "verified_quantity": "24"}, {"order_line_id": "L2", "purchase_quantity": "36", "remaining_quantity": "0", "sku_id": "ST01.red.M", "spec_revision": 1, "unit_id": "PCS", "unit_revision": 1, "verified_quantity": "36"}, {"order_line_id": "L3", "purchase_quantity": "12", "remaining_quantity": "0", "sku_id": "ST01.blue.S", "spec_revision": 1, "unit_id": "PCS", "unit_revision": 1, "verified_quantity": "12"}, {"order_line_id": "L4", "purchase_quantity": "48", "remaining_quantity": "0", "sku_id": "ST01.blue.M", "spec_revision":… |
| 214 | §5 step 12: FUL shipment, packing and booking | merchandiser | POST ful/commands/shipment.prepare | 200 | shipment.1 rv 1 prepared |  |
| 215 | §5 step 12: FUL shipment, packing and booking | merchandiser | POST ful/commands/packing.plan | 200 | shipment.1 rv 2 prepared |  |
| 216 | §5 step 12: FUL shipment, packing and booking | merchandiser | POST ful/commands/packing.record_actual | 200 | shipment.1 rv 3 prepared |  |
| 217 | §5 step 12: FUL shipment, packing and booking | merchandiser | POST evd/commands/source.register | 200 | booking.confirmation rv 1 registered |  |
| 218 | §5 step 12: FUL shipment, packing and booking | merchandiser | POST ful/commands/booking.record | 200 | shipment.1 rv 1 booked |  |
| 219 | §5 step 12: FUL shipment, packing and booking | merchandiser | GET ful/shipments/shipment.1 | 200 |  | {"lines": 4, "row_version": 3, "status": "prepared"} |
| 220 | §5 step 13: FUL commercial release of the shipment | doc.1 | POST ful/commands/commercial_release.evaluate | 200 | rls-01a1d2a8-7114-71e0-8231-6928c5292415 rv 1 granted |  |
| 221 | §5 step 13: FUL commercial release of the shipment | doc.1 | GET ful/commercial_releases/rls-01a1d2a8-7114-71e0-8231-6928c5292415 | 200 |  | {"delivery_verified": 120, "guards_sha256": "7a7e89c7e71e4ca6c22cbe1d3c4c5254ace71a70bbcf86ba5dd32e406dfa10ed", "license_required": false, "order": "active", "outcome": "granted", "payment_applied_minor": "28800", "payment_condition": "satisfied", "quality_released": 120, "release_id": "rls-01a1d2a8-7114-71e0-8231-6928c5292415", "screening": "clear", "trade_compliance": "clear"} |
| 222 | §5 step 14: FUL shipment confirmation | merchandiser | POST ful/commands/shipment.confirm | 200 | shipment.1 rv 4 confirmed |  |
| 223 | §5 step 14: FUL shipment confirmation | merchandiser | GET ful/shipments/shipment.1 | 200 |  | {"guards": 14, "guards_sha256": "5332fa80b014277fcae1290dedf80583dbbcdbda585b599052ee8fe558a8ff7a", "status": "confirmed", "unchecked_guards": 0} |
| 224 | §5 step 14: FUL shipment confirmation | merchandiser | GET ful/orders/order.1/fulfilment | 200 |  | {"shipped": 120} |
| 225 | §5 step 15: FUL shipment documents | doc.1 | POST ful/commands/shipment_document.prepare | 200 | docs.shipment.1 rv 1 draft |  |
| 226 | §5 step 15: FUL shipment documents | doc.1 | POST ful/commands/shipment_document.prepare | 200 | docs.shipment.1 rv 2 draft |  |
| 227 | §5 step 15: FUL shipment documents | doc.1 | POST evd/commands/file.register | 200 | bl.scan rv 1 registered |  |
| 228 | §5 step 15: FUL shipment documents | doc.1 | POST ful/commands/shipment_document.prepare | 200 | docs.shipment.1 rv 3 draft |  |
| 229 | §5 step 15: FUL shipment documents | doc.1 | GET ful/document_sets/docs.shipment.1 | 200 |  | {"differences": 0, "reconciliation": "reconciled", "reconciliation_sha256": "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"} |
| 230 | §5 step 15: FUL shipment documents | doc.1 | POST ful/commands/shipment_document.issue | 200 | docs.shipment.1 rv 4 issued |  |
| 231 | §5 step 15: FUL shipment documents | doc.1 | GET ful/document_sets/docs.shipment.1 | 200 |  |  |
| 232 | §5 step 15: FUL shipment documents | doc.1 | GET ful/document_sets/docs.shipment.1/documents/commercial_invoice/versions/1/en | 200 |  | {"bl_number": "SYNSTAR026E0001", "ci_number": "CI-2026-00001", "ci_sha256": "8fdbb3273318ba0e1097a3654cec5802aa7755eb80fb035e98ca83a9abce3a7c", "ci_states": "Net: 960.00 \| Tax: 0.00 \| Total: 960.00", "pl_number": "PL-2026-00001"} |
| 233 | §5 step 16: FUL documents release | doc.1 | POST ful/commands/commercial_release.evaluate | 200 | rls-01a1d2a8-721c-780f-8d9a-8a8b4c732644 rv 1 granted |  |
| 234 | §5 step 16: FUL documents release | doc.1 | GET ful/commercial_releases/rls-01a1d2a8-721c-780f-8d9a-8a8b4c732644 | 200 |  | {"outcome": "granted", "payment_condition": "satisfied", "release_id": "rls-01a1d2a8-721c-780f-8d9a-8a8b4c732644", "required_minor": "28800"} |
| 235 | §5 step 16: FUL documents release | doc.1 | POST evd/commands/source.register | 200 | bl.release rv 1 registered |  |
| 236 | §5 step 16: FUL documents release | doc.1 | POST ful/commands/document_release.record | 200 | rls-01a1d2a8-721c-780f-8d9a-8a8b4c732644 rv 2 consumed |  |
| 237 | §5 step 16: FUL documents release | doc.1 | GET ful/commercial_releases/rls-01a1d2a8-721c-780f-8d9a-8a8b4c732644 | 200 |  | {"release_kind": "original_bl", "status": "consumed"} |
| 238 | §5 step 17: FUL export lot | merchandiser | POST ful/commands/export_lot.register | 200 | lot.1 rv 1 registered |  |
| 239 | §5 step 17: FUL export lot | merchandiser | GET ful/export_lots/lot.1 | 200 |  | {"lines": 4, "net_minor": "96000", "reconciliation": "matched"} |
| 240 | §5 step 18: CNX customs declaration | sales | GET trd/orders/order.1 | 200 |  |  |
| 241 | §5 step 18: CNX customs declaration | sales | POST trd/commands/order.document.generate | 200 | order.1 rv 4 generated |  |
| 242 | §5 step 18: CNX customs declaration | doc.1 | POST cnx/commands/declaration_case.prepare | 200 | dcl-01a1d2a8-72cc-7b40-b35a-1fecba584128 rv 1 draft |  |
| 243 | §5 step 18: CNX customs declaration | doc.1 | POST cnx/commands/declaration_case.confirm | 200 | dcl-01a1d2a8-72cc-7b40-b35a-1fecba584128 rv 2 confirmed |  |
| 244 | §5 step 18: CNX customs declaration | doc.1 | POST evd/commands/source.register | 200 | declaration.sent rv 1 registered |  |
| 245 | §5 step 18: CNX customs declaration | doc.1 | POST cnx/commands/declaration_case.record_submission | 200 | dcl-01a1d2a8-72cc-7b40-b35a-1fecba584128 rv 3 submitted |  |
| 246 | §5 step 18: CNX customs declaration | doc.1 | POST evd/commands/source.register | 200 | customs.receipt rv 1 registered |  |
| 247 | §5 step 18: CNX customs declaration | doc.1 | POST cnx/commands/declaration_case.record_receipt | 200 | dcl-01a1d2a8-72cc-7b40-b35a-1fecba584128 rv 4 released |  |
| 248 | §5 step 18: CNX customs declaration | doc.1 | GET cnx/declaration_cases/dcl-01a1d2a8-72cc-7b40-b35a-1fecba584128 | 200 |  | {"customs_released": true, "declaration_id": "dcl-01a1d2a8-72cc-7b40-b35a-1fecba584128", "declaration_number": "SYN-2026-000001", "lot_value_minor": "96000", "mapping_version": "v1", "original_text_kept": true, "sales_contract": "SC-2026-00001", "status": "released"} |
| 249 | §5 step 18: CNX customs declaration | merchandiser | GET ful/milestone_plans/plan.order.1 | 200 |  | {"ful_actuals": 1} |
| 250 | §5 step 19: STL sales statement and receivable | treasury | POST stl/commands/settlement.confirm | 200 | stmt.sales.1 rv 1 confirmed |  |
| 251 | §5 step 19: STL sales statement and receivable | treasury | GET stl/receivables/stmt.sales.1 | 200 |  | {"amount_minor": "96000", "applied_minor": "28800", "installments": ["P1 28800 applied 28800 open 0", "P2 67200 applied 0 open 67200"], "open_minor": "67200"} |
| 252 | §5 step 19: STL sales statement and receivable | treasury | GET stl/applications/app.p1 | 200 |  | {"p1_application": "moved"} |
| 253 | §5 step 19: STL sales statement and receivable | credit.officer | GET stl/customers/customer.1/credit | 200 |  | {"exposure_currency": "USD", "exposure_minor": "67200"} |
| 254 | §5 step 20: STL collection of P2 with a bank charge | doc.1 | POST evd/commands/source.register | 200 | bank.p2 rv 1 registered |  |
| 255 | §5 step 20: STL collection of P2 with a bank charge | treasury | POST stl/commands/fund.record | 200 | fund.p2 rv 1 recorded | {"difference_minor": "200", "status": "recorded"} |
| 256 | §5 step 20: STL collection of P2 with a bank charge | treasury | POST stl/commands/application.apply | 409 | FUND_DIFFERENCE_UNEXPLAINED | {"field_errors": [{"path": "/args/amount", "reason": "Only 670.00 USD is available: 2.00 USD of the difference is unexplained"}]} |
| 257 | §5 step 20: STL collection of P2 with a bank charge | credit.manager.1 | POST stl/commands/fund.resolve_difference | 200 | fund.p2 rv 2 difference_resolved |  |
| 258 | §5 step 20: STL collection of P2 with a bank charge | treasury | POST stl/commands/application.apply | 200 | app.p2 rv 1 applied |  |
| 259 | §5 step 20: STL collection of P2 with a bank charge | treasury | POST stl/commands/application.apply | 200 | app.p2.charge rv 1 applied |  |
| 260 | §5 step 20: STL collection of P2 with a bank charge | treasury | GET stl/receivables/stmt.sales.1 | 200 |  | {"applied_minor": "96000", "installments": ["P1 28800 applied 28800 open 0", "P2 67200 applied 67200 open 0"], "open_minor": "0"} |
| 261 | §5 step 20: STL collection of P2 with a bank charge | treasury | GET stl/fund_records/fund.p2 | 200 |  | {"fund_available_minor": "0", "fund_resolved_minor": "200"} |
| 262 | §5 step 20: STL collection of P2 with a bank charge | credit.officer | GET stl/customers/customer.1/credit | 200 |  | {"exposure_currency": "USD", "exposure_minor": "0"} |
| 263 | §5 step 21: STL purchase statement, payable and payment | treasury | POST stl/commands/settlement.confirm | 200 | stmt.purchase.1 rv 1 confirmed |  |
| 264 | §5 step 21: STL purchase statement, payable and payment | treasury | GET stl/payables/stmt.purchase.1 | 200 |  | {"amount_minor": "60000", "open_minor": "60000"} |
| 265 | §5 step 21: STL purchase statement, payable and payment | doc.1 | POST evd/commands/source.register | 200 | bank.pay rv 1 registered |  |
| 266 | §5 step 21: STL purchase statement, payable and payment | treasury | POST stl/commands/fund.record | 200 | fund.pay.1 rv 1 recorded |  |
| 267 | §5 step 21: STL purchase statement, payable and payment | treasury | POST stl/commands/application.apply | 200 | app.pay.1 rv 1 applied |  |
| 268 | §5 step 21: STL purchase statement, payable and payment | treasury | GET stl/payables/stmt.purchase.1 | 200 |  | {"applied_minor": "60000", "open_minor": "0"} |
| 269 | §5 step 22: CNX purchase invoice | purchaser | POST evd/commands/file.register | 200 | invoice.scan rv 1 registered |  |
| 270 | §5 step 22: CNX purchase invoice | purchaser | POST evd/commands/source.register | 200 | invoice.received rv 1 registered |  |
| 271 | §5 step 22: CNX purchase invoice | tax.officer | POST cnx/commands/purchase_invoice.import | 200 | pin-01a1d2a8-742f-718a-9db5-3d42c6975cb4 rv 1 imported |  |
| 272 | §5 step 22: CNX purchase invoice | tax.officer | POST cnx/commands/purchase_invoice.verify | 200 | pin-01a1d2a8-742f-718a-9db5-3d42c6975cb4 rv 2 verified |  |
| 273 | §5 step 22: CNX purchase invoice | tax.officer | GET cnx/purchase_invoices/pin-01a1d2a8-742f-718a-9db5-3d42c6975cb4 | 200 |  | {"differences": 0, "matched": 120, "purchase_invoice_id": "pin-01a1d2a8-742f-718a-9db5-3d42c6975cb4", "status": "verified"} |
| 274 | §5 step 23: CNX tax refund case | tax.officer | POST cnx/commands/tax_refund_case.allocate | 200 | trc-01a1d2a8-7479-74a8-bfab-9820d378bd66 rv 1 expected |  |
| 275 | §5 step 23: CNX tax refund case | tax.officer | GET cnx/tax_refund_cases/trc-01a1d2a8-7479-74a8-bfab-9820d378bd66 | 200 |  | {"allocations": 4, "amounts": {}, "case_id": "trc-01a1d2a8-7479-74a8-bfab-9820d378bd66", "policy_revision": 1, "status": "expected"} |
| 276 | §5 step 23: CNX tax refund case | tax.officer | POST cnx/commands/tax_refund_case.record_result | 200 | trc-01a1d2a8-7479-74a8-bfab-9820d378bd66 rv 2 expected |  |
| 277 | §5 step 23: CNX tax refund case | tax.officer | GET cnx/tax_refund_cases/trc-01a1d2a8-7479-74a8-bfab-9820d378bd66 | 200 |  | {"amounts": {"currency_code": "USD", "expected_minor": "7800"}, "status": "expected"} |
| 278 | §5 step 24: CNX document bundle and external reconciliation | doc.1 | POST evd/commands/source.register | 200 | docs.originals rv 1 registered |  |
| 279 | §5 step 24: CNX document bundle and external reconciliation | doc.1 | POST cnx/commands/document_bundle.check | 200 | dbn-01a1d2a8-74c2-7ede-ad6d-12ea2cb9c138 rv 1 complete |  |
| 280 | §5 step 24: CNX document bundle and external reconciliation | tax.officer | POST cnx/commands/external_reconciliation.resolve | 200 | dbn-01a1d2a8-74c2-7ede-ad6d-12ea2cb9c138 rv 2 complete |  |
| 281 | §5 step 24: CNX document bundle and external reconciliation | doc.1 | GET cnx/document_bundles/dbn-01a1d2a8-74c2-7ede-ad6d-12ea2cb9c138 | 200 |  | {"bundle_id": "dbn-01a1d2a8-74c2-7ede-ad6d-12ea2cb9c138", "reconciliation_difference": "0", "status": "complete"} |
| 282 | §5 step 25: TRD order close | merchandiser | GET trd/orders/order.1 | 200 |  |  |
| 283 | §5 step 25: TRD order close | merchandiser | POST trd/commands/order.close | 200 | order.1 rv 5 closed |  |
| 284 | §5 step 25: TRD order close | merchandiser | GET trd/orders/order.1 | 200 |  | {"end_command": "order.close", "fulfilment_source": "ful.shipment_facts.v1", "row_version": 5, "shipped": ["24", "36", "12", "48"], "status": "closed"} |
| 285 | §5 step 25: TRD order close | merchandiser | GET pur/purchase_commitments/commitment.1 | 200 |  | {"commitment_fulfilled": true} |
| 286 | §5 step 25: STL consumer of OrderClosed | sales | GET stl/credit_reservations/res.order.1 | 200 |  | {"released_by": {"id": "inbox.stl.order_credit", "kind": "system"}, "status": "released", "version": 3} |
| 287 | §5 step 25: STL consumer of OrderClosed | credit.officer | GET stl/customers/customer.1/credit | 200 |  | {"exposure_currency": "USD", "exposure_minor": "0"} |
| 288 | §5 step 26: STL freight charge | credit.officer | POST stl/commands/charge.record | 200 | freight.brc rv 1 estimated |  |
| 289 | §5 step 26: STL freight charge | merchandiser | POST evd/commands/source.register | 200 | freight.bill rv 1 registered |  |
| 290 | §5 step 26: STL freight charge | treasury | POST stl/commands/charge.confirm | 200 | freight.brc rv 2 confirmed |  |
| 291 | §5 step 26: STL freight charge | credit.officer | POST stl/commands/charge.allocate | 200 | freight.brc rv 3 confirmed |  |
| 292 | §5 step 26: STL freight charge | treasury | GET stl/charges/freight.brc | 200 |  | {"allocation_seq": 1, "amount_minor": "6000", "estimate_minor": "6000", "status": "confirmed"} |
| 293 | §5 step 27: STL order result | treasury | POST stl/commands/order_result.review | 200 | order.1 rv 1 reviewed |  |
| 294 | §5 step 27: STL order result | treasury | POST stl/commands/order_result.close | 200 | order.1 rv 2 settled |  |
| 295 | §5 step 27: STL order result | treasury | GET stl/orders/order.1/result | 200 |  | {"claims": "not_enabled", "cost_of_sales_minor": "60000", "direct_charges_minor": "6000", "forecast_result_minor": "30000", "margin": "31.25", "net_sales_minor": "96000", "result_minor": "30000", "snapshot_sha256": "a74d35904264a695406f82ac199fc405ab84ad063a5fcf77108dbda8522bebd2", "status": "settled"} |
| 296 | §5 final assertions | sales | GET trd/orders/order.1 | 200 |  | {"commits": 1, "order_payable_minor": "96000", "order_status": "closed"} |
| 297 | §5 final assertions | sales | GET stl/credit_reservations/res.order.1 | 200 |  | {"portion_minor": "67200", "reservation": "reserved → consumed → released"} |
| 298 | §5 final assertions | merchandiser | GET ful/shipments/shipment.1 | 200 |  |  |
| 299 | §5 final assertions | treasury | GET stl/receivables/stmt.sales.1 | 200 |  |  |
| 300 | §5 final assertions | treasury | GET stl/payables/stmt.purchase.1 | 200 |  | {"payable": "60000 → 0", "receivable": "96000 → 0"} |
| 301 | §5 final assertions | treasury | GET stl/orders/order.1/result | 200 |  | {"commit_entries_checked": 60, "events_delivered": 144, "events_with_unchecked_guards_member": 2, "not_wired": 0, "order_result": "settled 30000 (31.25 %)", "outcome_events": ["2: 1 × idp-brant.stl.credit_limit_published.v1", "3: 1 × idp-brant.stl.credit_reserved.v1", "4: 4 × idp-brant.cat.declaration_profile_verified.v1", "5: 1 × idp-brant.cnx.trade_compliance_decided.v1", "6: 1 × idp-brant.trd.order_activated.v1", "6: 1 × idp-brant.stl.credit_reservation_consumed.v1", "7: 4 × idp-brant.stl.applied.v1", "8: 1 × idp-brant.pur.purchase_commitment_approved.v1", "8: 1 × idp-brant.pur.purchase_commitment_issued.v1", "8: 1 × idp-brant.pur.supplier_confirmation_recorded.v1", "9: 4 × idp-brant.qlt.… |

## Amounts

- commercial_invoice: 960.00 USD
- cost_snapshot: 660.00 USD
- cost_snapshot_minor: 66000
- credit_reserved: 672.00 USD
- credit_reserved_minor: 67200
- export_lot: 960.00 USD
- freight_charge: 60.00 USD
- order_payable: 960.00 USD
- order_payable_minor: 96000
- order_result: 300.00 USD (31.25 %)
- payable: 600.00 USD
- payable_open: 0.00 USD
- purchase_commitment: 600.00 USD
- purchase_draft: 600.00 USD
- purchase_draft_net_minor: 60000
- quote_payable: 960.00 USD
- quote_payable_minor: 96000
- quote_payments: 288.00 + 672.00 USD
- quote_profit: 300.00 USD
- receipt_p1: 288.00 USD
- receivable: 960.00 USD
- receivable_applied: 288.00 + 670.00 + 2.00 USD
- receivable_open: 0.00 USD
- scenario_B_minor: 69600
- shipped: 120 PCS

## Run

- result: passed | started 2026-11-13T02:06:51.956323Z | finished 2026-11-13T02:06:59.033127Z | requests: 301 (POST 218 / GET 83 )
- environment: {"go": "go1.26.8", "identity": "local OIDC issuer of the test (RS256, typ at+jwt, JWKS by discovery); no real identity provider", "os_arch": "linux/amd64", "postgresql": "16.12", "transport": "bootstrap.Platform.API behind net/http/httptest on loopback"}
- actors: acceptance.verifier (客户确认核实人), admin (管理员（POL 草稿）), buyer (采购／核价经办（起草）), compliance (合规（名单与主体筛查）), config.reviewer (配置与交易档案发布人（独立复核）), cost.checker (核价复核（核实、评估、冻结）), credit.manager.1 (信用经理), credit.manager.2 (信用经理), credit.officer (信用专员), doc.1 (单证（申报要素归类）), doc.2 (单证（归类核实）), merchandiser (跟单), order.reviewer (订单审核人), purchaser (采购编制人), qc (质检), quote.reviewer (报价审核人), sales (销售经办), tax.officer (税务专员), treasury (资金)
- fixtures:
  - tenant tenant.36d78a318915 with company.A — newWorld (pgtest.UniqueID) — a tenant is the access token's brant_tenant claim; there is no business HTTP route for it
  - grants of the ten humans — bootstrap.Platform.Provision: every permission, company data scope, team trade — operator-only (cmd/brant-admin grant); no business HTTP route
  - role templates of doc.1, doc.2 (documentation) and credit.manager.1, credit.manager.2 (credit_manager) — bootstrap.Platform.AssignRoles with company data scope — operator-only; STL reads the approver's tier from the credit_manager template (S5 D-6)
  - access tokens — the test's local OIDC issuer, one token per request — no real identity provider is connected before the B0 pilot gate
  - role templates of merchandiser (merchandiser), qc (quality_inspector), treasury (treasury), tax.officer (tax_specialist) and credit.officer (credit_officer) — bootstrap.Platform.AssignRoles with company data scope — operator-only; no business HTTP route
  - number.issue for doc.1 besides the documentation template — bootstrap.Platform.Provision with the documentation template's permissions plus number.issue — POL NumberIssue v1 numbers the commercial invoice and the packing list as the acting human; the documentation template lacks number.issue (C2's reported gap)
  - the Inbox actors of every consumer the composed contexts subscribe — bootstrap.Platform.ProvisionInbox (as brant-admin inbox) — operator-only; a consumer's commands run as system:inbox.<consumer>
  - the event relay — bootstrap.Platform.Relay publishing to messaging.Dispatcher of Contexts.Subscriptions (brant-worker -dispatch in process) over a throwaway Bento outbox fed with the platform Outbox's events of the tenant, unchanged — a relay on the shared bento.outbox would also claim other tests' events
- not executed:
  - sending the purchase order and the supplier's confirmation through a channel: purchase.commitment.issue and purchase.supplier_confirmation.record record the human sending and receiving as facts on EVD evidence
  - the external inspection agency's system: qc records the agency's report (an EVD file) as a fact
  - the carrier's and the forwarder's systems: the booking, the B/L record and the forwarder's bill are entered from EVD evidence
  - China customs (single window): the declaration's submission and receipt are recorded manually (CN-M01), the receipt's original text is synthetic
  - the bank: fund records are entered by treasury from synthetic bank advices
  - the tax authority: the VAT special invoice is a synthetic scan imported and verified by tax.officer; no refund is submitted, approved or received, the case stays expected
  - CLM: the order result records claims not_enabled (S5 has no claims context)
  - the Outbox relay to an external sink: the relay publishes to the in-process dispatcher (brant-worker -dispatch)
  - a real identity provider
