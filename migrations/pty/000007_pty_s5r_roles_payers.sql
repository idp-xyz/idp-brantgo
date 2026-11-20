-- PTY: the S5R additions (simulated sign-off E0 of 2026-11-13; tactical model PTY-AGG-Party and
-- PTY-AGG-TradingProfile; UL PTY-AddressRole, PTY-ServicePartyRole, PTY-TradingProfile; Context Map
-- R43, R44, R73, R77).
--
-- S5R-D40: an address may serve the notify party (role notify), which an order binds and FUL prints,
-- checks and screens. The role checks of party_address and party_revision_address admit it; no row
-- changes.
--
-- S5R-D36: service roles of kind insurer, inspection_agency (QLT, R73) and customs_broker (CNX, R77)
-- are verified parties without a route, so their origin and destination may be NULL; a forwarder or
-- a carrier serves a route, as do the S1 kinds broker and inspection, which stay admitted until QLT
-- and CNX resolve the S5R names. A stated location is still a UN/LOCODE (the column checks of 000003
-- hold for every non-NULL value). Rows verified before keep their route.
--
-- S5R-D21: trading_profile_payer holds the permitted payers of a trading profile revision, other
-- parties whose payments count as the party's own in STL (R43, TradingProfileRef v1). Like the
-- contacts of a revision they change only while the revision is working (trigger) and are kept with
-- it once it is published; a revision without payers has no rows, so its content hash (which omits
-- an empty list) is as before.

ALTER TABLE brant_pty.party_address DROP CONSTRAINT party_address_role_check;
ALTER TABLE brant_pty.party_address ADD CONSTRAINT party_address_role_check
    CHECK (role IN ('buyer', 'seller', 'payer', 'payee', 'consignee', 'shipper', 'billing', 'notify'));
ALTER TABLE brant_pty.party_revision_address DROP CONSTRAINT party_revision_address_role_check;
ALTER TABLE brant_pty.party_revision_address ADD CONSTRAINT party_revision_address_role_check
    CHECK (role IN ('buyer', 'seller', 'payer', 'payee', 'consignee', 'shipper', 'billing', 'notify'));

ALTER TABLE brant_pty.service_role DROP CONSTRAINT service_role_role_check;
ALTER TABLE brant_pty.service_role ADD CONSTRAINT service_role_role_check
    CHECK (role IN ('forwarder', 'carrier', 'insurer', 'inspection_agency', 'customs_broker', 'broker', 'inspection'));
ALTER TABLE brant_pty.service_role ALTER COLUMN origin DROP NOT NULL;
ALTER TABLE brant_pty.service_role ALTER COLUMN destination DROP NOT NULL;
ALTER TABLE brant_pty.service_role ADD CONSTRAINT service_role_route_check
    CHECK (role NOT IN ('forwarder', 'carrier', 'broker', 'inspection') OR (origin IS NOT NULL AND destination IS NOT NULL));

CREATE TABLE brant_pty.trading_profile_payer (
    tenant_id      text   NOT NULL,
    company_id     text   NOT NULL,
    party_id       text   NOT NULL,
    revision       bigint NOT NULL,
    payer_party_id text   NOT NULL CHECK (payer_party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, party_id, revision, payer_party_id),
    FOREIGN KEY (tenant_id, company_id, party_id, revision)
        REFERENCES brant_pty.trading_profile_revision (tenant_id, company_id, party_id, revision),
    FOREIGN KEY (tenant_id, company_id, payer_party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id),
    -- The party itself pays without being listed.
    CHECK (payer_party_id <> party_id)
);

-- The permitted payers of a revision change only while it is working (PTY-INV-001).
CREATE FUNCTION brant_pty.guard_trading_profile_payer() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r record := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_payer: payers are replaced, not edited' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pty.trading_profile_revision v WHERE v.tenant_id = r.tenant_id AND v.company_id = r.company_id
            AND v.party_id = r.party_id AND v.revision = r.revision AND v.status = 'working') THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_payer: payers change only while the revision is working (PTY-INV-001)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN r;
END
$$;
CREATE TRIGGER trading_profile_payer_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pty.trading_profile_payer
    FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_trading_profile_payer();

ALTER TABLE brant_pty.trading_profile_payer ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.trading_profile_payer FORCE ROW LEVEL SECURITY;
CREATE POLICY trading_profile_payer_scope ON brant_pty.trading_profile_payer
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
