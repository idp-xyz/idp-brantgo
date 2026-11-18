-- POL: the sales price list and the target margin (owner decision 2026-11-18: a quotation's unit
-- price comes from a price list where one is in force, otherwise from the formula cost ÷ (1 − target
-- margin) of the pricing policy; S4 unit G4a).
--
-- sales_price is one price of the price list: the price (unit_price, a decimal string) of one SKU
-- (sku_id) in one currency, per price_basis_quantity of one unit, for every customer or, with
-- party_id, for one. Its validity is the revision's; its life cycle, ConfigGuard v1 (000002) and
-- ConfigAt v1 (000006) are those of every kind and need nothing new: both take the kind as an
-- argument. Its data is validated by package api and the domain.
--
-- A subject (SKU, customer or every customer, currency) has at most one price in force:
-- config_revision_sales_price_subject admits one active revision per subject across the
-- configurations of the kind, so a lookup of the list never chooses between two. POL's
-- config.publish refuses a second one with BUSINESS_KEY_CONFLICT; the index decides two publications
-- that race. An absent party_id is the general price ('' in the index).
--
-- The target margin of a pricing policy lives in the revision's data (target_margin) and needs no
-- DDL: it is left out of the stored data when the policy states none, so every revision stored
-- before keeps its data and its content hash.
--
-- No table is added: config_root and config_revision keep the row level security of 000001 and the
-- grants of deploy/postgres/10_app_grants.sql. The kind list keeps every kind 000005 allowed.

ALTER TABLE brant_pol.config_root DROP CONSTRAINT config_root_kind_check;
ALTER TABLE brant_pol.config_root ADD CONSTRAINT config_root_kind_check CHECK (kind IN ('company_profile', 'currency', 'unit',
    'unit_conversion', 'fx_rate', 'calendar', 'trade_terms', 'payment_terms', 'tax_profile', 'tax_application', 'fee_category',
    'pricing_policy', 'bank_account', 'number_sequence', 'document_template', 'quote_document_template', 'screening_regulation',
    'credit_policy', 'legal_terms', 'order_document_template', 'shipment_document_template', 'export_tax_policy', 'sales_price'));

CREATE UNIQUE INDEX config_revision_sales_price_subject ON brant_pol.config_revision
    (tenant_id, company_id, (data->>'sku_id'), (COALESCE(data->>'party_id', '')), (data->>'currency'))
    WHERE kind = 'sales_price' AND status = 'active';
