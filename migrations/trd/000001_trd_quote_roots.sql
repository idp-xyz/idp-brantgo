-- TRD: quote roots, their business revisions and the root's object control (WBS S3-01, S3-08;
-- tactical model TRD-AGG-QuoteRoot, SM-TRD-REVISION, SM-TRD-CONTROL; TRD-INV-001, 002, 015, 016;
-- Python t03_storage quote_root, quote_revision and object_control).
--
-- A quote root is (tenant, company, quote_id) with a root version every quote command compares and
-- increments, and business revisions numbered monotonically. One working revision is saved whole,
-- copied from a frozen one (revise) or discarded; nothing is deleted and no revision number is
-- reused. A human freezes the working revision: the row then holds its commercial document, the
-- normalized canonical JSON whose SHA-256 is its business hash (Python protocol.business_hash), and
-- the CHECK below makes the database verify that binding on every write. A frozen revision never
-- changes in this migration; the review, activation, replacement, withdrawal and cancellation
-- waves add their transitions, and at most one revision of a root is ever active (TRD-INV-001: the
-- partial unique index, under the root's row lock and version CAS).
--
-- Internal labels and notes belong to the root (quote.metadata.update) and change its version only;
-- they are never part of a revision or of its business hash (TRD-INV-002, BC-AT09).
--
-- The object control lives on the root row, so the root's FOR UPDATE lock serializes every write
-- with every bind, stop and release (TRD-INV-015). Its control epoch grows by one with each of them
-- and never otherwise; TRD is its only writer.
--
-- Drafts, commercial documents and evidence are stored as JSON text, not jsonb: their bytes are
-- kept exactly, a string may hold any character the contract admits, and the commercial document's
-- bytes are what its hash covers. References to other contexts are plain identifiers: no foreign
-- key crosses a schema (DB2).
--
-- action_result and audit_event are the command ledger of TRD (internal/foundation/action).

CREATE SCHEMA brant_trd;

CREATE TABLE brant_trd.quote_root (
    tenant_id               text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id              text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quote_id                text        NOT NULL CHECK (quote_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version            bigint      NOT NULL CHECK (root_version > 0),
    last_revision           bigint      NOT NULL CHECK (last_revision > 0),
    owner_actor_id          text        NOT NULL,
    owner_team_id           text,
    labels                  text[]      NOT NULL CHECK (cardinality(labels) <= 20),
    internal_note           text        NOT NULL CHECK (char_length(internal_note) <= 2000),
    control_state           text        NOT NULL CHECK (control_state IN ('unbound', 'bound', 'stopping')),
    control_epoch           bigint      NOT NULL CHECK (control_epoch >= 0),
    control_task_id         text        CHECK (control_task_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    control_authority_epoch bigint      CHECK (control_authority_epoch > 0),
    control_by_kind         text        CHECK (control_by_kind IN ('human', 'system')),
    control_by_id           text,
    control_changed_at      timestamptz,
    created_at              timestamptz NOT NULL,
    updated_at              timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, quote_id),
    -- A bound or stopping root names its task and the authority epoch it was bound under.
    CHECK ((control_state = 'unbound') = (control_task_id IS NULL)),
    CHECK ((control_state = 'unbound') = (control_authority_epoch IS NULL)),
    -- Every bind, stop and release records who made it and when; a root never bound has epoch 0.
    CHECK ((control_epoch = 0) = (control_changed_at IS NULL)),
    CHECK ((control_changed_at IS NULL) = (control_by_kind IS NULL) AND (control_by_kind IS NULL) = (control_by_id IS NULL)),
    CHECK (control_epoch > 0 OR control_state = 'unbound')
);

CREATE TABLE brant_trd.quote_revision (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    quote_id            text        NOT NULL,
    revision            bigint      NOT NULL CHECK (revision > 0),
    status              text        NOT NULL CHECK (status IN ('working', 'frozen', 'discarded', 'pending_approval', 'active',
                                                               'superseded', 'withdrawn', 'cancelled')),
    base_revision       bigint      CHECK (base_revision > 0 AND base_revision < revision),
    data                text        NOT NULL CHECK (left(data, 1) = '{'),
    content_hash        text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    contributors        text[]      NOT NULL CHECK (cardinality(contributors) >= 1),
    saved_by_kind       text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id         text        NOT NULL,
    saved_epoch         bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at            timestamptz NOT NULL,
    business_hash       text        CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    commercial          text,
    issued_at           timestamptz,
    expires_at          timestamptz,
    frozen_by_kind      text,
    frozen_by_id        text,
    freeze_epoch        bigint,
    frozen_at           timestamptz,
    frozen_root_version bigint,
    ended_by_kind       text,
    ended_by_id         text,
    end_epoch           bigint,
    ended_at            timestamptz,
    end_reason          text        CHECK (char_length(end_reason) BETWEEN 1 AND 2000),
    end_evidence        text,
    PRIMARY KEY (tenant_id, company_id, quote_id, revision),
    FOREIGN KEY (tenant_id, company_id, quote_id) REFERENCES brant_trd.quote_root (tenant_id, company_id, quote_id),
    -- A revision past working or discarded was frozen by a human: it holds its commercial document,
    -- which hashes to its business hash, and a validity interval issued_at < expires_at
    -- (TRD-INV-002, TRD-INV-005).
    CHECK (CASE WHEN status IN ('working', 'discarded')
        THEN business_hash IS NULL AND commercial IS NULL AND issued_at IS NULL AND expires_at IS NULL AND frozen_by_kind IS NULL
             AND frozen_by_id IS NULL AND freeze_epoch IS NULL AND frozen_at IS NULL AND frozen_root_version IS NULL
        ELSE business_hash IS NOT NULL AND commercial IS NOT NULL
             AND encode(sha256(convert_to(commercial, 'UTF8')), 'hex') = business_hash
             AND issued_at IS NOT NULL AND expires_at > issued_at AND frozen_by_kind = 'human' AND frozen_by_id IS NOT NULL
             AND freeze_epoch > 0 AND frozen_at IS NOT NULL AND frozen_root_version > 0 END),
    CHECK ((status = 'discarded') = (ended_at IS NOT NULL)),
    CHECK (CASE WHEN ended_at IS NULL
        THEN ended_by_kind IS NULL AND ended_by_id IS NULL AND end_epoch IS NULL AND end_reason IS NULL AND end_evidence IS NULL
        ELSE ended_by_kind IN ('human', 'ai') AND ended_by_id IS NOT NULL AND end_epoch > 0 AND end_reason IS NOT NULL
             AND left(end_evidence, 1) = '[' END)
);

-- At most one working revision per root; at most one active revision per root (TRD-INV-001: two
-- first activations racing have one winner, whichever commits first); one freeze per root version.
CREATE UNIQUE INDEX quote_revision_working ON brant_trd.quote_revision (tenant_id, company_id, quote_id)
    WHERE status = 'working';
CREATE UNIQUE INDEX quote_revision_active ON brant_trd.quote_revision (tenant_id, company_id, quote_id)
    WHERE status = 'active';
CREATE UNIQUE INDEX quote_revision_frozen_at ON brant_trd.quote_revision (tenant_id, company_id, quote_id, frozen_root_version)
    WHERE frozen_root_version IS NOT NULL;

-- A root keeps its identity and owner; its root version, last revision and control epoch never
-- decrease; the object control changes only with a new control epoch; the labels, the note and the
-- revision count change only with a new root version. Roots are never deleted.
CREATE FUNCTION brant_trd.guard_quote_root() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    control constant text[] := ARRAY['control_state', 'control_epoch', 'control_task_id', 'control_authority_epoch',
        'control_by_kind', 'control_by_id', 'control_changed_at'];
    versioned constant text[] := ARRAY['root_version', 'last_revision', 'labels', 'internal_note'];
    o jsonb := to_jsonb(OLD);
    n jsonb := to_jsonb(NEW);
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.quote_root: roots are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (o - control - versioned - 'updated_at') IS DISTINCT FROM (n - control - versioned - 'updated_at')
            OR NEW.root_version < OLD.root_version OR NEW.last_revision < OLD.last_revision
            OR NEW.control_epoch < OLD.control_epoch THEN
        RAISE EXCEPTION 'brant_trd.quote_root: identity and owner are fixed; versions and epochs never decrease'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.control_epoch = OLD.control_epoch AND (SELECT bool_or(o -> k IS DISTINCT FROM n -> k) FROM unnest(control) AS k) THEN
        RAISE EXCEPTION 'brant_trd.quote_root: the object control changes only with a new control epoch'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (SELECT bool_or(o -> k IS DISTINCT FROM n -> k) FROM unnest(ARRAY['last_revision', 'labels', 'internal_note']) AS k)
            AND NEW.root_version = OLD.root_version THEN
        RAISE EXCEPTION 'brant_trd.quote_root: revisions and metadata change only with a new root version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_root_guard BEFORE UPDATE OR DELETE ON brant_trd.quote_root
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_root();

-- A revision is created working. Only a working revision is edited (content, contributors, last
-- save), frozen or discarded; a frozen or discarded revision never changes (TRD-INV-002: the
-- database refuses to modify a sealed row). Revisions are never deleted.
CREATE FUNCTION brant_trd.guard_quote_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.quote_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'working' THEN
            RAISE EXCEPTION 'brant_trd.quote_revision: a revision is created working' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.quote_id, NEW.revision, NEW.base_revision)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.quote_id, OLD.revision, OLD.base_revision) THEN
        RAISE EXCEPTION 'brant_trd.quote_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status IN ('working', 'frozen', 'discarded') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.quote_revision: revision % is %; its content and business hash do not change', OLD.revision, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER quote_revision_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_revision
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_revision();

ALTER TABLE brant_trd.quote_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_root FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_root_scope ON brant_trd.quote_root
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_revision_scope ON brant_trd.quote_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_trd.action_result (
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

CREATE TABLE brant_trd.audit_event (
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

ALTER TABLE brant_trd.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_trd.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_trd.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
