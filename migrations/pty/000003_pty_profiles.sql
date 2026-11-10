-- PTY: address roles and service roles of parties, and company trading profiles (WBS S1-02;
-- PTY-AGG-Party entities AddressRole and PartyRole, PTY-AGG-TradingProfile; Context Map R05, R13, R43).
--
-- party_address holds a party's addresses with their trade role (Python party_profile.addresses,
-- T03 A31). They are part of the party's draft content: rows change only while the party is a
-- draft. Every confirmation keeps them with its revision in party_revision_address, which never
-- changes (PTY-INV-001); AddressRoleRef v1 names (party, confirmed revision, address).
--
-- service_role holds the verified revisions of a confirmed supplier's service roles (T02 spec 13.1:
-- role, service, exact origin and destination, validity, evidence). A human verifies each revision
-- against the party's confirmed revision; revisions are appended, never changed, and the latest
-- revision of a role is the current one. ServiceRoleRef v1 names (party, role, revision).
--
-- trading_profile is the root of a party's trading profile in one company: the root version every
-- command compares and increments and the last revision number. trading_profile_revision holds its
-- revisions like POL's configuration revisions: one working revision is saved whole; a human other
-- than its last editor publishes it, which supersedes the active revision in the same transaction;
-- a published revision never changes except for its supersession, and nothing is deleted. A
-- revision binds the party's confirmed revision (foreign key), the settlement terms as exact POL
-- revisions (payment_terms and trade_terms; POL is another context, checked by PTY before its
-- transaction, so there is no foreign key), the business owner, and in trading_profile_contact the
-- contacts at their row versions, which change only while the revision is working.

CREATE TABLE brant_pty.party_address (
    tenant_id  text NOT NULL,
    company_id text NOT NULL,
    party_id   text NOT NULL,
    address_id text NOT NULL CHECK (address_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    role       text NOT NULL CHECK (role IN ('buyer', 'seller', 'payer', 'payee', 'consignee', 'shipper', 'billing')),
    address    text NOT NULL CHECK (char_length(address) BETWEEN 1 AND 500),
    PRIMARY KEY (tenant_id, company_id, party_id, address_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id)
);

CREATE TABLE brant_pty.party_revision_address (
    tenant_id  text   NOT NULL,
    company_id text   NOT NULL,
    party_id   text   NOT NULL,
    revision   bigint NOT NULL,
    address_id text   NOT NULL CHECK (address_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    role       text   NOT NULL CHECK (role IN ('buyer', 'seller', 'payer', 'payee', 'consignee', 'shipper', 'billing')),
    address    text   NOT NULL CHECK (char_length(address) BETWEEN 1 AND 500),
    PRIMARY KEY (tenant_id, company_id, party_id, revision, address_id),
    FOREIGN KEY (tenant_id, company_id, party_id, revision)
        REFERENCES brant_pty.party_revision (tenant_id, company_id, party_id, revision)
);

-- A party's addresses change only while the party is a draft (PTY-INV-001).
CREATE FUNCTION brant_pty.guard_party_address() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r record := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'brant_pty.party_address: addresses are replaced, not edited' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pty.party p WHERE p.tenant_id = r.tenant_id AND p.company_id = r.company_id
            AND p.party_id = r.party_id AND p.status = 'draft') THEN
        RAISE EXCEPTION 'brant_pty.party_address: addresses change only while the party is a draft (PTY-INV-001)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN r;
END
$$;
CREATE TRIGGER party_address_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pty.party_address
    FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_party_address();
CREATE TRIGGER party_revision_address_immutable BEFORE UPDATE OR DELETE ON brant_pty.party_revision_address
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_change();

CREATE TABLE brant_pty.service_role (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    party_id         text        NOT NULL,
    service_role_id  text        NOT NULL CHECK (service_role_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    revision         bigint      NOT NULL CHECK (revision > 0),
    party_revision   bigint      NOT NULL CHECK (party_revision > 0),
    role             text        NOT NULL CHECK (role IN ('forwarder', 'carrier', 'broker', 'inspection', 'insurer')),
    service_code     text        NOT NULL CHECK (service_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    origin           text        NOT NULL CHECK (origin ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
    destination      text        NOT NULL CHECK (destination ~ '^[A-Z]{2}[A-Z2-9]{3}$'),
    valid_from       timestamptz NOT NULL,
    valid_until      timestamptz NOT NULL CHECK (valid_until > valid_from),
    evidence_ids     text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 200),
    verified_by_kind text        NOT NULL CHECK (verified_by_kind = 'human'),
    verified_by_id   text        NOT NULL,
    authority_epoch  bigint      NOT NULL CHECK (authority_epoch > 0),
    verified_at      timestamptz NOT NULL CHECK (valid_until > verified_at),
    PRIMARY KEY (tenant_id, company_id, party_id, service_role_id, revision),
    FOREIGN KEY (tenant_id, company_id, party_id, party_revision)
        REFERENCES brant_pty.party_revision (tenant_id, company_id, party_id, revision)
);

CREATE TRIGGER service_role_immutable BEFORE UPDATE OR DELETE ON brant_pty.service_role
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_change();

CREATE TABLE brant_pty.trading_profile (
    tenant_id     text        NOT NULL,
    company_id    text        NOT NULL,
    party_id      text        NOT NULL,
    root_version  bigint      NOT NULL CHECK (root_version > 0),
    last_revision bigint      NOT NULL CHECK (last_revision > 0),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id)
);

CREATE TABLE brant_pty.trading_profile_revision (
    tenant_id              text        NOT NULL,
    company_id             text        NOT NULL,
    party_id               text        NOT NULL,
    revision               bigint      NOT NULL CHECK (revision > 0),
    status                 text        NOT NULL CHECK (status IN ('working', 'active', 'superseded')),
    content_hash           text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    party_revision         bigint      NOT NULL CHECK (party_revision > 0),
    payment_terms_id       text        NOT NULL CHECK (payment_terms_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$'),
    payment_terms_revision bigint      NOT NULL CHECK (payment_terms_revision > 0),
    trade_terms_id         text        NOT NULL CHECK (trade_terms_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$'),
    trade_terms_revision   bigint      NOT NULL CHECK (trade_terms_revision > 0),
    owner_actor_id         text        NOT NULL CHECK (owner_actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    owner_team_id          text        CHECK (owner_team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    saved_by_kind          text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id            text        NOT NULL,
    saved_epoch            bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at               timestamptz NOT NULL,
    published_by_kind      text,
    published_by_id        text,
    publish_epoch          bigint,
    published_at           timestamptz,
    evidence_ids           text[],
    ended_at               timestamptz,
    PRIMARY KEY (tenant_id, company_id, party_id, revision),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.trading_profile (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, party_id, party_revision)
        REFERENCES brant_pty.party_revision (tenant_id, company_id, party_id, revision),
    -- A published revision was published by a human other than its last editor.
    CHECK (CASE WHEN status IN ('active', 'superseded')
        THEN published_by_kind = 'human' AND published_by_id IS NOT NULL AND publish_epoch > 0 AND published_at IS NOT NULL
             AND cardinality(evidence_ids) BETWEEN 1 AND 200 AND published_by_id <> saved_by_id
        ELSE published_by_kind IS NULL AND published_by_id IS NULL AND publish_epoch IS NULL AND published_at IS NULL
             AND evidence_ids IS NULL END),
    CHECK ((status = 'superseded') = (ended_at IS NOT NULL))
);

-- At most one working and one active revision per trading profile.
CREATE UNIQUE INDEX trading_profile_revision_working ON brant_pty.trading_profile_revision (tenant_id, company_id, party_id)
    WHERE status = 'working';
CREATE UNIQUE INDEX trading_profile_revision_active ON brant_pty.trading_profile_revision (tenant_id, company_id, party_id)
    WHERE status = 'active';

CREATE TABLE brant_pty.trading_profile_contact (
    tenant_id           text   NOT NULL,
    company_id          text   NOT NULL,
    party_id            text   NOT NULL,
    revision            bigint NOT NULL,
    contact_id          text   NOT NULL,
    contact_row_version bigint NOT NULL CHECK (contact_row_version > 0),
    PRIMARY KEY (tenant_id, company_id, party_id, revision, contact_id),
    FOREIGN KEY (tenant_id, company_id, party_id, revision)
        REFERENCES brant_pty.trading_profile_revision (tenant_id, company_id, party_id, revision),
    FOREIGN KEY (tenant_id, company_id, contact_id) REFERENCES brant_pty.contact (tenant_id, company_id, contact_id)
);

-- The root keeps its party, its version only grows and revision numbers are never reused.
CREATE FUNCTION brant_pty.guard_trading_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pty.trading_profile: trading profiles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.party_id, NEW.created_at) IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.party_id, OLD.created_at)
            OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision THEN
        RAISE EXCEPTION 'brant_pty.trading_profile: identity is fixed, the root version grows, revision numbers are not reused'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER trading_profile_guard BEFORE UPDATE OR DELETE ON brant_pty.trading_profile
    FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_trading_profile();

-- Only a working revision is edited; a published revision is only superseded, keeping its content
-- and publication; a superseded revision never changes; nothing is deleted (PTY-INV-001).
CREATE FUNCTION brant_pty.guard_trading_profile_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.party_id, NEW.revision) IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.party_id, OLD.revision) THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status = 'working' THEN
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - ARRAY['status', 'published_by_kind', 'published_by_id', 'publish_epoch', 'published_at', 'evidence_ids', 'ended_at'])
            IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['status', 'published_by_kind', 'published_by_id', 'publish_epoch', 'published_at', 'evidence_ids', 'ended_at']) THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_revision: only a working revision is edited (revision % is %)', OLD.revision, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status = 'active' THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'superseded'
            AND (NEW.published_by_kind, NEW.published_by_id, NEW.publish_epoch, NEW.published_at, NEW.evidence_ids)
                IS NOT DISTINCT FROM (OLD.published_by_kind, OLD.published_by_id, OLD.publish_epoch, OLD.published_at, OLD.evidence_ids) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pty.trading_profile_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER trading_profile_revision_guard BEFORE UPDATE OR DELETE ON brant_pty.trading_profile_revision
    FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_trading_profile_revision();

-- The contacts of a revision change only while it is working.
CREATE FUNCTION brant_pty.guard_trading_profile_contact() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    r record := CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_contact: contacts are replaced, not edited' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pty.trading_profile_revision v WHERE v.tenant_id = r.tenant_id AND v.company_id = r.company_id
            AND v.party_id = r.party_id AND v.revision = r.revision AND v.status = 'working') THEN
        RAISE EXCEPTION 'brant_pty.trading_profile_contact: contacts change only while the revision is working (PTY-INV-001)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN r;
END
$$;
CREATE TRIGGER trading_profile_contact_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pty.trading_profile_contact
    FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_trading_profile_contact();

ALTER TABLE brant_pty.party_address ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.party_address FORCE ROW LEVEL SECURITY;
CREATE POLICY party_address_scope ON brant_pty.party_address
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.party_revision_address ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.party_revision_address FORCE ROW LEVEL SECURITY;
CREATE POLICY party_revision_address_scope ON brant_pty.party_revision_address
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.service_role ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.service_role FORCE ROW LEVEL SECURITY;
CREATE POLICY service_role_scope ON brant_pty.service_role
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.trading_profile ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.trading_profile FORCE ROW LEVEL SECURITY;
CREATE POLICY trading_profile_scope ON brant_pty.trading_profile
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.trading_profile_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.trading_profile_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY trading_profile_revision_scope ON brant_pty.trading_profile_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.trading_profile_contact ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.trading_profile_contact FORCE ROW LEVEL SECURITY;
CREATE POLICY trading_profile_contact_scope ON brant_pty.trading_profile_contact
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
