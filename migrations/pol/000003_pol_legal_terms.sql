-- POL: legal terms, the clause template of quotations and export contracts (WBS S3-17; the simulated
-- G0 walkthrough's decision F-02, the condition of ADR-BR-013): the governing law, whether the CISG
-- applies, the contract-formation rule that law implies and the dispute resolution. A legal_terms
-- configuration is a configuration like any other: revisions saved whole, published by a second
-- human, superseded or retired, and adopted downstream by exact revision through ConfigRef v1 and
-- ConfigGuard v1 (brant_pol.guard_config_in_force_v1 decides any kind). Its data, and a company
-- profile's default_legal_terms reference, live in the revision's data like every kind's, so only
-- the closed enumeration of kinds changes.

ALTER TABLE brant_pol.config_root DROP CONSTRAINT config_root_kind_check;
ALTER TABLE brant_pol.config_root ADD CONSTRAINT config_root_kind_check
    CHECK (kind IN ('company_profile', 'currency', 'unit', 'unit_conversion', 'fx_rate', 'calendar', 'trade_terms', 'payment_terms',
        'tax_profile', 'tax_application', 'fee_category', 'pricing_policy', 'bank_account', 'number_sequence', 'document_template',
        'quote_document_template', 'screening_regulation', 'credit_policy', 'legal_terms'));
