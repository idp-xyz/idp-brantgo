-- INQ (S7-01): one document profile per customer RFQ. Omitted means generic, which is every RFQ
-- saved before this column. A draft may change it. Once the RFQ leaves draft, the profile stays
-- (the ready-row trigger in 000001 already refuses other content edits).

ALTER TABLE brant_inq.customer_rfq
    ADD COLUMN document_profile text NOT NULL DEFAULT 'generic'
        CHECK (document_profile IN ('generic', 'apparel'));

CREATE FUNCTION brant_inq.guard_document_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.document_profile IS DISTINCT FROM OLD.document_profile
        AND (OLD.status <> 'draft' OR NEW.status <> 'draft') THEN
        RAISE EXCEPTION 'brant_inq.customer_rfq: document_profile does not change after the draft'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER customer_rfq_document_profile BEFORE UPDATE OF document_profile ON brant_inq.customer_rfq
    FOR EACH ROW EXECUTE FUNCTION brant_inq.guard_document_profile();
