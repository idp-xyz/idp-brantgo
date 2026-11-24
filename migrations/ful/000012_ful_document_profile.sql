-- FUL (S7-01): a shipment stores the one document profile of the orders it ships. Mixed profiles
-- are refused before the row is written. The profile does not change after prepare.
-- Assortment and carton columns are ful/000013 (S7-03), not this file.

ALTER TABLE brant_ful.shipment
    ADD COLUMN document_profile text NOT NULL DEFAULT 'generic'
        CHECK (document_profile IN ('generic', 'apparel'));

CREATE FUNCTION brant_ful.guard_document_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.document_profile IS DISTINCT FROM OLD.document_profile THEN
        RAISE EXCEPTION 'brant_ful.shipment: document_profile does not change after the shipment is prepared'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER shipment_document_profile BEFORE UPDATE OF document_profile ON brant_ful.shipment
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_document_profile();
