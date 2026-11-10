-- POL: configuration roots and their revisions, document number sequences and foundation snapshots
-- (Python foundation_root, foundation_revision, document_counter, document_number,
-- foundation_snapshot; tactical model POL-AGG-ConfigurationRoot, -DocumentNumberSequence,
-- -FoundationSnapshot).
--
-- A configuration is (tenant, company, kind, config_id) with a root version every command compares
-- and increments, and revisions numbered monotonically (SM-POL-REVISION): one working revision is
-- saved whole; a human other than its last editor publishes it (POL-INV-001), which supersedes the
-- active revision in the same transaction; retiring ends the active revision; discarding ends the
-- working one. A published revision never changes: the trigger allows only its supersession or
-- retirement, nothing is ever deleted, and brant_app may update only the columns a transition
-- writes. Superseded, retired and discarded revisions keep their content, so history stays
-- readable and a FoundationSnapshot always names what it adopted.
--
-- Document numbers are taken from a yearly counter per document type under its row lock
-- (POL-INV-005); a business reference has one number; issued numbers and snapshots never change.
--
-- action_result and audit_event are the command ledger of POL (internal/foundation/action).

CREATE SCHEMA brant_pol;

CREATE TABLE brant_pol.config_root (
    tenant_id     text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind          text        NOT NULL CHECK (kind IN ('company_profile', 'currency', 'unit', 'unit_conversion', 'fx_rate',
                                  'calendar', 'trade_terms', 'payment_terms', 'tax_profile', 'tax_application', 'fee_category',
                                  'pricing_policy', 'bank_account', 'number_sequence', 'document_template',
                                  'quote_document_template', 'screening_regulation', 'credit_policy')),
    config_id     text        NOT NULL CHECK (config_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$'),
    root_version  bigint      NOT NULL CHECK (root_version > 0),
    last_revision bigint      NOT NULL CHECK (last_revision > 0),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, kind, config_id)
);

CREATE TABLE brant_pol.config_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    kind              text        NOT NULL,
    config_id         text        NOT NULL,
    revision          bigint      NOT NULL CHECK (revision > 0),
    status            text        NOT NULL CHECK (status IN ('working', 'active', 'superseded', 'retired', 'discarded')),
    data              jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    content_hash      text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    valid_from        timestamptz NOT NULL,
    valid_until       timestamptz CHECK (valid_until > valid_from),
    saved_by_kind     text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id       text        NOT NULL,
    saved_epoch       bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at          timestamptz NOT NULL,
    published_by_kind text,
    published_by_id   text,
    publish_epoch     bigint,
    published_at      timestamptz,
    evidence_ids      text[],
    ended_at          timestamptz,
    PRIMARY KEY (tenant_id, company_id, kind, config_id, revision),
    FOREIGN KEY (tenant_id, company_id, kind, config_id) REFERENCES brant_pol.config_root (tenant_id, company_id, kind, config_id),
    -- A published revision was published by a human other than its last editor (POL-INV-001).
    CHECK (CASE WHEN status IN ('active', 'superseded', 'retired')
        THEN published_by_kind = 'human' AND published_by_id IS NOT NULL AND publish_epoch > 0 AND published_at IS NOT NULL
             AND cardinality(evidence_ids) BETWEEN 1 AND 200 AND published_by_id <> saved_by_id
        ELSE published_by_kind IS NULL AND published_by_id IS NULL AND publish_epoch IS NULL AND published_at IS NULL
             AND evidence_ids IS NULL END),
    CHECK ((status IN ('superseded', 'retired', 'discarded')) = (ended_at IS NOT NULL)),
    CHECK (kind <> 'fx_rate' OR valid_until IS NOT NULL)
);

-- At most one working and one active revision per configuration.
CREATE UNIQUE INDEX config_revision_working ON brant_pol.config_revision (tenant_id, company_id, kind, config_id)
    WHERE status = 'working';
CREATE UNIQUE INDEX config_revision_active ON brant_pol.config_revision (tenant_id, company_id, kind, config_id)
    WHERE status = 'active';

-- The root version only grows and revision numbers are never reused.
CREATE FUNCTION brant_pol.guard_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pol.config_root: configurations are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.kind, NEW.config_id, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.kind, OLD.config_id, OLD.created_at)
            OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision THEN
        RAISE EXCEPTION 'brant_pol.config_root: identity is fixed, the root version grows, revision numbers are not reused'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER config_root_guard BEFORE UPDATE OR DELETE ON brant_pol.config_root
    FOR EACH ROW EXECUTE FUNCTION brant_pol.guard_root();

-- Only a working revision is edited; a published revision is only superseded or retired, keeping
-- its content and publication; ended revisions never change; nothing is deleted (SM-POL-REVISION).
CREATE FUNCTION brant_pol.guard_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pol.config_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.kind, NEW.config_id, NEW.revision)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.kind, OLD.config_id, OLD.revision) THEN
        RAISE EXCEPTION 'brant_pol.config_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status = 'working' THEN
        RETURN NEW;
    END IF;
    IF (NEW.data, NEW.content_hash, NEW.valid_from, NEW.valid_until, NEW.saved_by_kind, NEW.saved_by_id, NEW.saved_epoch, NEW.saved_at)
            IS DISTINCT FROM
       (OLD.data, OLD.content_hash, OLD.valid_from, OLD.valid_until, OLD.saved_by_kind, OLD.saved_by_id, OLD.saved_epoch, OLD.saved_at) THEN
        RAISE EXCEPTION 'brant_pol.config_revision: only a working revision is edited (revision % is %)', OLD.revision, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status IN ('active', 'discarded') THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status IN ('superseded', 'retired')
            AND (NEW.published_by_kind, NEW.published_by_id, NEW.publish_epoch, NEW.published_at, NEW.evidence_ids)
                IS NOT DISTINCT FROM (OLD.published_by_kind, OLD.published_by_id, OLD.publish_epoch, OLD.published_at, OLD.evidence_ids) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pol.config_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER config_revision_guard BEFORE UPDATE OR DELETE ON brant_pol.config_revision
    FOR EACH ROW EXECUTE FUNCTION brant_pol.guard_revision();

CREATE TABLE brant_pol.number_counter (
    tenant_id     text        NOT NULL,
    company_id    text        NOT NULL,
    document_type text        NOT NULL CHECK (document_type IN ('rfq', 'quote', 'order', 'purchase_order', 'invoice')),
    year          integer     NOT NULL CHECK (year BETWEEN 2000 AND 9999),
    last_value    bigint      NOT NULL CHECK (last_value > 0),
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, document_type, year)
);

CREATE TABLE brant_pol.issued_number (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    document_number    text        NOT NULL CHECK (document_number ~ '^[A-Z0-9][A-Z0-9-]{0,63}$'),
    document_type      text        NOT NULL,
    year               integer     NOT NULL,
    sequence_value     bigint      NOT NULL CHECK (sequence_value > 0),
    business_ref       text        NOT NULL CHECK (business_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sequence_kind      text        NOT NULL CHECK (sequence_kind = 'number_sequence'),
    sequence_config_id text        NOT NULL,
    sequence_revision  bigint      NOT NULL,
    issued_by_kind     text        NOT NULL CHECK (issued_by_kind IN ('human', 'ai', 'system')),
    issued_by_id       text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    issued_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, document_number),
    UNIQUE (tenant_id, company_id, document_type, business_ref),
    UNIQUE (tenant_id, company_id, document_type, year, sequence_value),
    FOREIGN KEY (tenant_id, company_id, document_type, year)
        REFERENCES brant_pol.number_counter (tenant_id, company_id, document_type, year),
    FOREIGN KEY (tenant_id, company_id, sequence_kind, sequence_config_id, sequence_revision)
        REFERENCES brant_pol.config_revision (tenant_id, company_id, kind, config_id, revision)
);

-- A counter only counts up and is never deleted.
CREATE FUNCTION brant_pol.guard_counter() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR (NEW.tenant_id, NEW.company_id, NEW.document_type, NEW.year)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.document_type, OLD.year)
            OR NEW.last_value <= OLD.last_value THEN
        RAISE EXCEPTION 'brant_pol.number_counter: a counter only counts up' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER number_counter_guard BEFORE UPDATE OR DELETE ON brant_pol.number_counter
    FOR EACH ROW EXECUTE FUNCTION brant_pol.guard_counter();

CREATE TABLE brant_pol.foundation_snapshot (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    snapshot_id       text        NOT NULL CHECK (snapshot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    purpose           text        NOT NULL CHECK (purpose IN ('cost_estimate', 'quotation')),
    content_hash      text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    captured_by_kind  text        NOT NULL CHECK (captured_by_kind IN ('human', 'ai', 'system')),
    captured_by_id    text        NOT NULL,
    authority_epoch   bigint      NOT NULL CHECK (authority_epoch > 0),
    captured_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, snapshot_id)
);

CREATE TABLE brant_pol.snapshot_entry (
    tenant_id    text   NOT NULL,
    company_id   text   NOT NULL,
    snapshot_id  text   NOT NULL,
    kind         text   NOT NULL,
    config_id    text   NOT NULL,
    revision     bigint NOT NULL,
    content_hash text   NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, company_id, snapshot_id, kind, config_id, revision),
    FOREIGN KEY (tenant_id, company_id, snapshot_id) REFERENCES brant_pol.foundation_snapshot (tenant_id, company_id, snapshot_id),
    FOREIGN KEY (tenant_id, company_id, kind, config_id, revision)
        REFERENCES brant_pol.config_revision (tenant_id, company_id, kind, config_id, revision)
);

CREATE FUNCTION brant_pol.refuse_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_pol.%: issued numbers and snapshots are immutable', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER issued_number_immutable BEFORE UPDATE OR DELETE ON brant_pol.issued_number
    FOR EACH ROW EXECUTE FUNCTION brant_pol.refuse_change();
CREATE TRIGGER foundation_snapshot_immutable BEFORE UPDATE OR DELETE ON brant_pol.foundation_snapshot
    FOR EACH ROW EXECUTE FUNCTION brant_pol.refuse_change();
CREATE TRIGGER snapshot_entry_immutable BEFORE UPDATE OR DELETE ON brant_pol.snapshot_entry
    FOR EACH ROW EXECUTE FUNCTION brant_pol.refuse_change();

ALTER TABLE brant_pol.config_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.config_root FORCE ROW LEVEL SECURITY;
CREATE POLICY config_root_scope ON brant_pol.config_root
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pol.config_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.config_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY config_revision_scope ON brant_pol.config_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pol.number_counter ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.number_counter FORCE ROW LEVEL SECURITY;
CREATE POLICY number_counter_scope ON brant_pol.number_counter
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pol.issued_number ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.issued_number FORCE ROW LEVEL SECURITY;
CREATE POLICY issued_number_scope ON brant_pol.issued_number
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pol.foundation_snapshot ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.foundation_snapshot FORCE ROW LEVEL SECURITY;
CREATE POLICY foundation_snapshot_scope ON brant_pol.foundation_snapshot
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pol.snapshot_entry ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.snapshot_entry FORCE ROW LEVEL SECURITY;
CREATE POLICY snapshot_entry_scope ON brant_pol.snapshot_entry
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_pol.action_result (
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

CREATE TABLE brant_pol.audit_event (
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

ALTER TABLE brant_pol.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pol.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_pol.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_pol.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
