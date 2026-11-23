-- STL settles an approved CLM remedy once (S6-D03, Context Map R35 CLM→STL, guardLock false;
-- STL-INV-011). credit_note.request and fund.record kind refund read CreditRequest v1 before the
-- transaction and insert one row here. The primary key is the remedy, so a second settlement is
-- CLAIM_REMEDY_ALREADY_SETTLED. A later rejection of the credit note does not delete the row.
-- There is no inbox consumer. Refunds are fund records, so the row is not a column of credit_note.

CREATE TABLE brant_stl.claim_remedy_settlement (
    tenant_id     text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    claim_id      text        NOT NULL CHECK (claim_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    remedy_id     text        NOT NULL CHECK (remedy_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind          text        NOT NULL CHECK (kind IN ('credit_note', 'refund')),
    settlement_id text        NOT NULL CHECK (settlement_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id      text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    amount_minor  bigint      NOT NULL CHECK (amount_minor > 0),
    currency      text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    recorded_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, claim_id, remedy_id)
);

ALTER TABLE brant_stl.claim_remedy_settlement ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.claim_remedy_settlement FORCE ROW LEVEL SECURITY;
CREATE POLICY claim_remedy_settlement_scope ON brant_stl.claim_remedy_settlement
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
