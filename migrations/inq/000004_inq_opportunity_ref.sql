-- INQ (R40, S6-D04): an optional CRM opportunity on a customer RFQ draft. The id is a plain text
-- reference; no foreign key crosses into brant_crm (DB2). Empty means the draft cites no
-- opportunity, which is every RFQ saved before this column and every save that omits it.
-- CRM being off does not affect a row whose opportunity_id is null. A ready RFQ is still not
-- edited (000001 guard_rfq): confirming keeps the id the draft stored.

ALTER TABLE brant_inq.customer_rfq
    ADD COLUMN opportunity_id text
        CHECK (opportunity_id IS NULL OR opportunity_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$');
