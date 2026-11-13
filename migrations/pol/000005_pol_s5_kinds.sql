-- POL: the S5 configuration kinds and the packing list's numbers (owner decision 2026-11-12; WBS
-- S5-08, S5 D-12, CN-AT12; Context Map R49, R50).
--
-- shipment_document_template is the layout of a commercial invoice (CI) or a packing list (PL) for
-- FUL's fixed shipment-text renderer, as order_document_template is an S/C's or a PI's: kind,
-- language, title, sections, clauses and footer note (validated by package api). It has no
-- references of its own; the command that renders names its number sequence.
--
-- export_tax_policy is the export tax refund method that applies to the company's exports
-- (exempt_credit_refund, exempt_refund or not_applicable), the policy documents it rests on and the
-- EVD source of the professional review, which its publication must cite. CNX binds the revision in
-- force at an export's date (CN-AT12), so a later revision never rewrites open business.
--
-- packing_list numbers packing lists from a counter of their own; a CI is numbered as an invoice.
--
-- The other S5 additions live in the revisions' data and need no DDL: fx_rate purposes credit and
-- settlement, a payment stage's release_control, and a credit policy's documents_secured,
-- reservation_validity_days, decision_validity_days and two_approvers_above_top. Each is left out of
-- the stored data when empty, false or 0, so every revision stored before keeps its data and its
-- content hash. A FoundationSnapshot keeps its purposes (cost_estimate, quotation): it is captured
-- for a calculation, never for credit or settlement.
--
-- The kind list keeps every kind 000001, 000003 and 000004 allowed, and the document types every
-- type 000001 and 000004 allowed.

ALTER TABLE brant_pol.config_root DROP CONSTRAINT config_root_kind_check;
ALTER TABLE brant_pol.config_root ADD CONSTRAINT config_root_kind_check CHECK (kind IN ('company_profile', 'currency', 'unit',
    'unit_conversion', 'fx_rate', 'calendar', 'trade_terms', 'payment_terms', 'tax_profile', 'tax_application', 'fee_category',
    'pricing_policy', 'bank_account', 'number_sequence', 'document_template', 'quote_document_template', 'screening_regulation',
    'credit_policy', 'legal_terms', 'order_document_template', 'shipment_document_template', 'export_tax_policy'));

ALTER TABLE brant_pol.number_counter DROP CONSTRAINT number_counter_document_type_check;
ALTER TABLE brant_pol.number_counter ADD CONSTRAINT number_counter_document_type_check CHECK (document_type IN ('rfq', 'quote', 'order',
    'purchase_order', 'invoice', 'proforma_invoice', 'packing_list'));
