-- POL: the order document template and the proforma invoice's numbers (WBS S3-15, TRD-INV-020; UL
-- TRD-SalesContractDocument).
--
-- order_document_template is the layout of an export sales contract (S/C) or a proforma invoice
-- (PI) for TRD's fixed order-text renderer, as quote_document_template is a quotation's: kind,
-- language, title, sections, clauses and footer note (validated by package api). It has no
-- references of its own; the command that renders names its number sequence.
--
-- proforma_invoice numbers PIs from a counter of their own: a PI is not a commercial invoice, and the
-- two never share a sequence. An S/C is numbered as an order.

ALTER TABLE brant_pol.config_root DROP CONSTRAINT config_root_kind_check;
ALTER TABLE brant_pol.config_root ADD CONSTRAINT config_root_kind_check CHECK (kind IN ('company_profile', 'currency', 'unit',
    'unit_conversion', 'fx_rate', 'calendar', 'trade_terms', 'payment_terms', 'tax_profile', 'tax_application', 'fee_category',
    'pricing_policy', 'bank_account', 'number_sequence', 'document_template', 'quote_document_template', 'screening_regulation',
    'credit_policy', 'order_document_template'));

ALTER TABLE brant_pol.number_counter DROP CONSTRAINT number_counter_document_type_check;
ALTER TABLE brant_pol.number_counter ADD CONSTRAINT number_counter_document_type_check CHECK (document_type IN ('rfq', 'quote', 'order',
    'purchase_order', 'invoice', 'proforma_invoice'));
