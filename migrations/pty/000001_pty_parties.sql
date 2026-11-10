-- PTY: trading parties and their contacts (Python BC_T01_reference.sql master, kind customer,
-- supplier and contact). A party plays one or more roles for the company. Drafts are saved whole;
-- a human confirms a draft on evidence it may read; a confirmed record is revised back into a draft
-- or deactivated (PTY-INV-001). Every confirmation is kept as an immutable revision with its
-- evidence, the confirming actor and the authority epoch, so revising never loses what was
-- confirmed. A trigger refuses content changes outside the draft state and any change to an
-- inactive record. Records are owned by the actor who created them (BC-D02 data scope).
--
-- action_result and audit_event are the command ledger of PTY (internal/foundation/action).

CREATE SCHEMA brant_pty;

CREATE TABLE brant_pty.party (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id       text        NOT NULL CHECK (party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status         text        NOT NULL CHECK (status IN ('draft', 'confirmed', 'inactive')),
    row_version    bigint      NOT NULL CHECK (row_version > 0),
    revision       bigint      NOT NULL CHECK (revision >= 0),
    name           text        CHECK (char_length(name) BETWEEN 1 AND 200),
    roles          text[]      NOT NULL CHECK (cardinality(roles) BETWEEN 1 AND 2 AND roles <@ ARRAY['customer', 'supplier']),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id),
    CHECK (status = 'draft' OR (name IS NOT NULL AND revision > 0))
);

CREATE TABLE brant_pty.party_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    party_id          text        NOT NULL,
    revision          bigint      NOT NULL CHECK (revision > 0),
    row_version       bigint      NOT NULL CHECK (row_version > 0),
    name              text        NOT NULL,
    roles             text[]      NOT NULL,
    evidence_ids      text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 200),
    confirmed_by_kind text        NOT NULL CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id   text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    confirmed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id, revision),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id)
);

CREATE TABLE brant_pty.contact (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    contact_id     text        NOT NULL CHECK (contact_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id       text,
    status         text        NOT NULL CHECK (status IN ('draft', 'confirmed', 'inactive')),
    row_version    bigint      NOT NULL CHECK (row_version > 0),
    revision       bigint      NOT NULL CHECK (revision >= 0),
    name           text        CHECK (char_length(name) BETWEEN 1 AND 200),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, contact_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id),
    CHECK (status = 'draft' OR (name IS NOT NULL AND party_id IS NOT NULL AND revision > 0))
);

CREATE TABLE brant_pty.contact_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    contact_id        text        NOT NULL,
    revision          bigint      NOT NULL CHECK (revision > 0),
    row_version       bigint      NOT NULL CHECK (row_version > 0),
    party_id          text        NOT NULL,
    name              text        NOT NULL,
    evidence_ids      text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 200),
    confirmed_by_kind text        NOT NULL CHECK (confirmed_by_kind = 'human'),
    confirmed_by_id   text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    confirmed_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, contact_id, revision),
    FOREIGN KEY (tenant_id, company_id, contact_id) REFERENCES brant_pty.contact (tenant_id, company_id, contact_id)
);

-- PTY-INV-001 in the database: content changes only in the draft state; an inactive record never
-- changes; revisions never change.
CREATE FUNCTION brant_pty.guard_master() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'inactive' THEN
        RAISE EXCEPTION 'brant_pty.%: an inactive record does not change (PTY-INV-001)', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'draft' AND (to_jsonb(NEW) - ARRAY['status', 'row_version', 'revision', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status', 'row_version', 'revision', 'updated_at']) THEN
        RAISE EXCEPTION 'brant_pty.%: a confirmed record is revised, not edited (PTY-INV-001)', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER party_guard BEFORE UPDATE ON brant_pty.party FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_master();
CREATE TRIGGER contact_guard BEFORE UPDATE ON brant_pty.contact FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_master();

CREATE FUNCTION brant_pty.refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_pty.%: confirmed revisions are immutable (PTY-INV-001)', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER party_revision_immutable BEFORE UPDATE OR DELETE ON brant_pty.party_revision
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_change();
CREATE TRIGGER contact_revision_immutable BEFORE UPDATE OR DELETE ON brant_pty.contact_revision
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_change();

ALTER TABLE brant_pty.party ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.party FORCE ROW LEVEL SECURITY;
CREATE POLICY party_scope ON brant_pty.party
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.party_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.party_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY party_revision_scope ON brant_pty.party_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.contact ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.contact FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_scope ON brant_pty.contact
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.contact_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.contact_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY contact_revision_scope ON brant_pty.contact_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_pty.action_result (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    command         text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash    text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action_id       text        NOT NULL,
    object_id       text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    status          text        NOT NULL,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, command, idempotency_key),
    UNIQUE (tenant_id, company_id, action_id)
);

CREATE TABLE brant_pty.audit_event (
    seq             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    event_id        text        NOT NULL UNIQUE,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    command         text        NOT NULL,
    object_id       text,
    outcome         text        NOT NULL CHECK (outcome IN ('succeeded', 'replayed', 'rejected', 'read')),
    code            text,
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    reason          text,
    authority_epoch bigint,
    CHECK ((outcome = 'rejected') = (code IS NOT NULL))
);

ALTER TABLE brant_pty.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_pty.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_pty.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
