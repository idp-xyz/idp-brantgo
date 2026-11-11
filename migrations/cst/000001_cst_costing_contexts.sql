-- CST: costing contexts and measurement snapshots (WBS S2-03; tactical model CST-AGG-CostingContext
-- and CST-AGG-MeasurementSnapshot; Python costing_engine RevisionModel and CommandModel
-- measurement, T02 §4, §13.1, §13.2, §13.8, §15.2).
--
-- A costing context is (tenant, company, context_id) with a root version every command compares
-- and increments, and revisions numbered monotonically (T02 §13.2): one working revision is saved
-- whole, possibly by several contributors; a human who did not contribute to it verifies it, which
-- seals it and supersedes the active revision in the same transaction; revise copies a sealed
-- revision into a new working one; discard ends the working revision; withdraw ends a sealed
-- revision and clears the active one when it was the target, without reviving an older one. A
-- sealed revision never changes its content, its assumption provenance or its verification; the
-- verification records the RFQ binding it was verified against (RfqRef v1: RFQ, row version, hash
-- of the complete line set, and the lines as INQ gave them). Nothing is ever deleted.
--
-- A measurement snapshot is (tenant, company, measurement_id) with immutable revisions recorded by
-- a human (T02 §13.8): record_verified creates the root with revision 1 active; supersede appends
-- the next revision and supersedes the active one atomically; withdraw ends an exact revision and
-- clears the active one when it was the target. Revision content never changes.
--
-- References to INQ, CAT and POL are plain identifiers: no foreign key crosses a schema (DB2).
-- Evidence is stored as given (source ID and locator).
--
-- action_result and audit_event are the command ledger of CST (internal/foundation/action).

CREATE SCHEMA brant_cst;

CREATE TABLE brant_cst.costing_context (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    context_id     text        NOT NULL CHECK (context_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version   bigint      NOT NULL CHECK (root_version > 0),
    last_revision  bigint      NOT NULL CHECK (last_revision > 0),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, context_id)
);

CREATE TABLE brant_cst.costing_context_revision (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    context_id            text        NOT NULL,
    revision              bigint      NOT NULL CHECK (revision > 0),
    status                text        NOT NULL CHECK (status IN ('working', 'active', 'superseded', 'withdrawn', 'discarded')),
    base_revision         bigint      CHECK (base_revision > 0 AND base_revision < revision),
    data                  jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    content_hash          text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    contributors          text[]      NOT NULL CHECK (cardinality(contributors) >= 1),
    provenance            jsonb       NOT NULL CHECK (jsonb_typeof(provenance) = 'object'),
    saved_by_kind         text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id           text        NOT NULL,
    saved_epoch           bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at              timestamptz NOT NULL,
    verified_by_kind      text,
    verified_by_id        text,
    verify_epoch          bigint,
    verified_at           timestamptz,
    verification_evidence jsonb,
    rfq_id                text,
    rfq_row_version       bigint,
    rfq_lines_hash        text,
    rfq_lines             jsonb,
    superseded_at         timestamptz,
    ended_by_kind         text,
    ended_by_id           text,
    end_epoch             bigint,
    ended_at              timestamptz,
    end_reason            text        CHECK (char_length(end_reason) BETWEEN 1 AND 1000),
    end_evidence          jsonb,
    PRIMARY KEY (tenant_id, company_id, context_id, revision),
    FOREIGN KEY (tenant_id, company_id, context_id) REFERENCES brant_cst.costing_context (tenant_id, company_id, context_id),
    -- A sealed revision was verified by a human who did not contribute to it, against a ready RFQ.
    CHECK (CASE WHEN status IN ('active', 'superseded', 'withdrawn')
        THEN verified_by_kind = 'human' AND verified_by_id IS NOT NULL AND verify_epoch > 0 AND verified_at IS NOT NULL
             AND jsonb_typeof(verification_evidence) = 'array' AND NOT (verified_by_id = ANY (contributors))
             AND rfq_id IS NOT NULL AND rfq_row_version > 0 AND rfq_lines_hash ~ '^[0-9a-f]{64}$'
             AND jsonb_typeof(rfq_lines) = 'array' AND jsonb_array_length(rfq_lines) >= 1
        ELSE verified_by_kind IS NULL AND verified_by_id IS NULL AND verify_epoch IS NULL AND verified_at IS NULL
             AND verification_evidence IS NULL AND rfq_id IS NULL AND rfq_row_version IS NULL AND rfq_lines_hash IS NULL
             AND rfq_lines IS NULL END),
    CHECK ((status IN ('withdrawn', 'discarded')) = (ended_at IS NOT NULL)),
    CHECK (ended_at IS NULL OR (ended_by_kind IN ('human', 'ai') AND ended_by_id IS NOT NULL AND end_epoch > 0 AND end_reason IS NOT NULL)),
    CHECK ((status = 'withdrawn') = (end_evidence IS NOT NULL) AND (status <> 'withdrawn' OR ended_by_kind = 'human')),
    CHECK (superseded_at IS NULL OR status IN ('superseded', 'withdrawn'))
);

-- At most one working and one active revision per costing context.
CREATE UNIQUE INDEX costing_context_revision_working ON brant_cst.costing_context_revision (tenant_id, company_id, context_id)
    WHERE status = 'working';
CREATE UNIQUE INDEX costing_context_revision_active ON brant_cst.costing_context_revision (tenant_id, company_id, context_id)
    WHERE status = 'active';

CREATE TABLE brant_cst.measurement (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    measurement_id text        NOT NULL CHECK (measurement_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version   bigint      NOT NULL CHECK (root_version > 0),
    last_revision  bigint      NOT NULL CHECK (last_revision > 0),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, measurement_id)
);

CREATE TABLE brant_cst.measurement_revision (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    measurement_id   text        NOT NULL,
    revision         bigint      NOT NULL CHECK (revision > 0),
    status           text        NOT NULL CHECK (status IN ('active', 'superseded', 'withdrawn')),
    data             jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    content_hash     text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    kind             text        NOT NULL CHECK (kind IN ('measured', 'estimated')),
    observed_at      timestamptz NOT NULL,
    line_ids         text[]      NOT NULL CHECK (cardinality(line_ids) >= 1),
    recorded_by_kind text        NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id   text        NOT NULL,
    record_epoch     bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at      timestamptz NOT NULL,
    reason           text        CHECK (char_length(reason) BETWEEN 1 AND 1000),
    superseded_at    timestamptz,
    withdrawn_by_id  text,
    withdraw_epoch   bigint,
    withdrawn_at     timestamptz,
    withdraw_reason  text        CHECK (char_length(withdraw_reason) BETWEEN 1 AND 1000),
    withdraw_evidence jsonb,
    PRIMARY KEY (tenant_id, company_id, measurement_id, revision),
    FOREIGN KEY (tenant_id, company_id, measurement_id) REFERENCES brant_cst.measurement (tenant_id, company_id, measurement_id),
    -- A measurement observed after it was recorded is not a verified fact (T02 §13.8).
    CHECK (observed_at <= recorded_at),
    -- Revision 1 is recorded; every later revision supersedes with a reason.
    CHECK ((revision = 1) = (reason IS NULL)),
    CHECK ((status = 'withdrawn') = (withdrawn_at IS NOT NULL)),
    CHECK (withdrawn_at IS NULL OR (withdrawn_by_id IS NOT NULL AND withdraw_epoch > 0 AND withdraw_reason IS NOT NULL
        AND jsonb_typeof(withdraw_evidence) = 'array')),
    CHECK (superseded_at IS NULL OR status IN ('superseded', 'withdrawn'))
);

-- At most one active revision per measurement.
CREATE UNIQUE INDEX measurement_revision_active ON brant_cst.measurement_revision (tenant_id, company_id, measurement_id)
    WHERE status = 'active';

-- Roots keep their identity and owner, root versions only grow and revision numbers are never
-- reused; nothing is deleted.
CREATE FUNCTION brant_cst.guard_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cst.%: roots are never deleted', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['root_version', 'last_revision', 'updated_at'])
            IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['root_version', 'last_revision', 'updated_at'])
            OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision THEN
        RAISE EXCEPTION 'brant_cst.%: identity and owner are fixed, the root version grows, revision numbers are not reused', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER costing_context_guard BEFORE UPDATE OR DELETE ON brant_cst.costing_context
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_root();
CREATE TRIGGER measurement_guard BEFORE UPDATE OR DELETE ON brant_cst.measurement
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_root();

-- Only a working revision is edited (content, contributors, provenance, last save). Verification
-- seals it (working → active); discarding ends it; a sealed revision is only superseded or
-- withdrawn, keeping its content and verification; ended revisions never change.
CREATE FUNCTION brant_cst.guard_context_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    ending constant text[] := ARRAY['status', 'superseded_at', 'ended_by_kind', 'ended_by_id', 'end_epoch', 'ended_at',
        'end_reason', 'end_evidence'];
    o jsonb := to_jsonb(OLD);
    n jsonb := to_jsonb(NEW);
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cst.costing_context_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.context_id, NEW.revision)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.context_id, OLD.revision) THEN
        RAISE EXCEPTION 'brant_cst.costing_context_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status IN ('working', 'active', 'discarded') THEN
        RETURN NEW;
    END IF;
    IF (o - ending) IS DISTINCT FROM (n - ending) THEN
        RAISE EXCEPTION 'brant_cst.costing_context_revision: revision % is %; its content and verification do not change', OLD.revision, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'active' AND NEW.status IN ('superseded', 'withdrawn') THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'superseded' AND NEW.status = 'withdrawn' AND NEW.superseded_at IS NOT DISTINCT FROM OLD.superseded_at THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cst.costing_context_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER costing_context_revision_guard BEFORE UPDATE OR DELETE ON brant_cst.costing_context_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_context_revision();

-- A measurement revision keeps its content and recording; it is only superseded or withdrawn.
CREATE FUNCTION brant_cst.guard_measurement_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    ending constant text[] := ARRAY['status', 'superseded_at', 'withdrawn_by_id', 'withdraw_epoch', 'withdrawn_at',
        'withdraw_reason', 'withdraw_evidence'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cst.measurement_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (to_jsonb(OLD) - ending) IS DISTINCT FROM (to_jsonb(NEW) - ending) THEN
        RAISE EXCEPTION 'brant_cst.measurement_revision: a recorded measurement does not change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (OLD.status = 'active' AND NEW.status IN ('superseded', 'withdrawn'))
            OR (OLD.status = 'superseded' AND NEW.status = 'withdrawn' AND NEW.superseded_at IS NOT DISTINCT FROM OLD.superseded_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cst.measurement_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER measurement_revision_guard BEFORE UPDATE OR DELETE ON brant_cst.measurement_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_measurement_revision();

ALTER TABLE brant_cst.costing_context ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.costing_context FORCE ROW LEVEL SECURITY;
CREATE POLICY costing_context_scope ON brant_cst.costing_context
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.costing_context_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.costing_context_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY costing_context_revision_scope ON brant_cst.costing_context_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.measurement ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.measurement FORCE ROW LEVEL SECURITY;
CREATE POLICY measurement_scope ON brant_cst.measurement
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.measurement_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.measurement_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY measurement_revision_scope ON brant_cst.measurement_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_cst.action_result (
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

CREATE TABLE brant_cst.audit_event (
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

ALTER TABLE brant_cst.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_cst.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_cst.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
