-- CST: cost snapshots, source attestations and AdoptabilityGuard v1 (WBS S2-05 second half, S2-06;
-- tactical model CST-AGG-CostSnapshot, SM-CST-SCENARIO draft→frozen, SM-CST-ADOPT, CST-INV-001,
-- CST-INV-002; Context Map R11 CST→TRD "CostSnapshotRef v1＋AdoptabilityGuard" and R21 CST→PUR;
-- Python costing.py snapshot_in_transaction and source_denial.record, t02_result, t02_component,
-- t02_allocation, t02_denial and t02_source_attestation).
--
-- cost_snapshot.freeze moves a draft scenario to frozen with the next root version and, in the same
-- transaction, inserts its one snapshot (CST-INV-002: (tenant, company, scenario_id) is unique): the
-- evaluation it froze, both scenario versions, the hashes of that evaluation, the selection reason,
-- the amounts, and a canonical snapshot document whose SHA-256 is the evidence hash. Its components
-- and their allocations to lines are child rows counted on the snapshot, so a later transaction
-- cannot add one; at commit their amounts add up to the snapshot's total and each component's
-- allocations to the component (CST-INV-004). A snapshot may name the snapshot of the same costing
-- context it supersedes; a snapshot has at most one successor. Nothing here ever changes or is
-- deleted.
--
-- Source standing: CST's own fact about an EVD source (Context Map R64: EVD publishes the source
-- and its read permission; whether a costing may still rest on it is CST's). A row exists for every
-- source CST has bound into a freeze or recorded an attestation about; costing.attest_source with
-- verdict denied moves it from undisputed to denied once and for ever (T02 §13: 重新恢复效力须新的
-- 核实／发布，不能删除否认事件复活旧结论). An attestation is an immutable record of a human's
-- verdict on a source, attested or denied, with reason and evidence; a source has at most one
-- denial. A freeze locks the standing rows of every source its evaluation rests on FOR SHARE and a
-- denial updates its row, so a denial either committed first and the freeze is refused, or waits
-- until the freeze committed and then makes the new snapshot not adoptable.
--
-- AdoptabilityGuard v1 (ADR-BR-004 option B, ADR-BR-011): brant_cst.guard_cost_snapshot_adoptable_v1
-- is what TRD calls in its own transaction, with its own executor, after locking its own root FOR
-- UPDATE. It locks the snapshot row FOR SHARE, then the standing rows of its sources, then the rows
-- of its inputs CST owns, then calls the guards of INQ, POL and SUP for the upstream inputs (each
-- locking its row FOR SHARE), and answers in one row: adoptable, denied (a source denied before any
-- successor was frozen), superseded, or not_current (an input no longer in force at p_at). Every
-- command that changes one of those facts updates or locks a row the guard locked, so it either
-- committed first and the guard sees it, or waits until the downstream transaction ends.

CREATE TABLE brant_cst.source_standing (
    tenant_id    text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id   text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_id    text        NOT NULL CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version bigint      NOT NULL CHECK (root_version > 0),
    status       text        NOT NULL CHECK (status IN ('undisputed', 'denied')),
    denial_id    text,
    denied_at    timestamptz,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, source_id),
    CHECK ((status = 'denied') = (denial_id IS NOT NULL) AND (denial_id IS NULL) = (denied_at IS NULL))
);

CREATE TABLE brant_cst.source_attestation (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    attestation_id  text        NOT NULL CHECK (attestation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_id       text        NOT NULL,
    verdict         text        NOT NULL CHECK (verdict IN ('attested', 'denied')),
    reason          text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence        jsonb       NOT NULL CHECK (jsonb_typeof(evidence) = 'array' AND jsonb_array_length(evidence) BETWEEN 1 AND 100),
    attested_by_kind text       NOT NULL CHECK (attested_by_kind = 'human'),
    attested_by_id  text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    attested_at     timestamptz NOT NULL,
    owner_team_id   text,
    PRIMARY KEY (tenant_id, company_id, attestation_id),
    FOREIGN KEY (tenant_id, company_id, source_id) REFERENCES brant_cst.source_standing (tenant_id, company_id, source_id)
);

-- A source is denied once.
CREATE UNIQUE INDEX source_attestation_one_denial ON brant_cst.source_attestation (tenant_id, company_id, source_id)
    WHERE verdict = 'denied';

ALTER TABLE brant_cst.source_standing ADD FOREIGN KEY (tenant_id, company_id, denial_id)
    REFERENCES brant_cst.source_attestation (tenant_id, company_id, attestation_id);

CREATE TABLE brant_cst.cost_snapshot (
    tenant_id               text           NOT NULL,
    company_id              text           NOT NULL,
    snapshot_id             text           NOT NULL CHECK (snapshot_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    scenario_id             text           NOT NULL,
    evaluation_id           text           NOT NULL,
    input_scenario_version  bigint         NOT NULL CHECK (input_scenario_version > 0),
    frozen_scenario_version bigint         NOT NULL CHECK (frozen_scenario_version = input_scenario_version + 1),
    context_id              text           NOT NULL,
    context_revision        bigint         NOT NULL CHECK (context_revision > 0),
    engine_version          text           NOT NULL,
    normalization_version   text           NOT NULL,
    input_hash              text           NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
    result_hash             text           NOT NULL CHECK (result_hash ~ '^[0-9a-f]{64}$'),
    selection_reason        text           NOT NULL CHECK (char_length(selection_reason) BETWEEN 1 AND 1000),
    currency                text           NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    total_minor             numeric(32, 0) NOT NULL CHECK (total_minor >= 0),
    supersedes_snapshot_id  text,
    document                text           NOT NULL,
    evidence_hash           text           NOT NULL CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    component_count         integer        NOT NULL CHECK (component_count BETWEEN 1 AND 100000),
    allocation_count        integer        NOT NULL CHECK (allocation_count BETWEEN 0 AND 10000000),
    frozen_by_kind          text           NOT NULL CHECK (frozen_by_kind = 'human'),
    frozen_by_id            text           NOT NULL,
    authority_epoch         bigint         NOT NULL CHECK (authority_epoch > 0),
    frozen_at               timestamptz    NOT NULL,
    owner_actor_id          text           NOT NULL,
    owner_team_id           text,
    PRIMARY KEY (tenant_id, company_id, snapshot_id),
    UNIQUE (tenant_id, company_id, scenario_id),
    UNIQUE (tenant_id, company_id, evaluation_id),
    UNIQUE (tenant_id, company_id, supersedes_snapshot_id),
    FOREIGN KEY (tenant_id, company_id, scenario_id) REFERENCES brant_cst.cost_scenario (tenant_id, company_id, scenario_id),
    FOREIGN KEY (tenant_id, company_id, evaluation_id) REFERENCES brant_cst.cost_evaluation (tenant_id, company_id, evaluation_id),
    FOREIGN KEY (tenant_id, company_id, supersedes_snapshot_id) REFERENCES brant_cst.cost_snapshot (tenant_id, company_id, snapshot_id),
    CHECK (supersedes_snapshot_id IS NULL OR supersedes_snapshot_id <> snapshot_id),
    -- The document is the bytes the evidence hash covers and repeats the columns.
    CHECK (encode(sha256(convert_to(document, 'UTF8')), 'hex') = evidence_hash),
    CHECK (jsonb_typeof(document::jsonb) = 'object' AND document::jsonb ->> 'kind' = 'cost_snapshot'
           AND document::jsonb ->> 'snapshot_id' = snapshot_id AND document::jsonb ->> 'scenario_id' = scenario_id
           AND document::jsonb ->> 'evaluation_id' = evaluation_id AND document::jsonb ->> 'result_hash' = result_hash
           AND document::jsonb ->> 'input_hash' = input_hash AND document::jsonb ->> 'engine_version' = engine_version
           AND document::jsonb ->> 'selection_reason' = selection_reason AND document::jsonb ->> 'currency' = currency
           AND (document::jsonb ->> 'total_minor')::numeric = total_minor
           AND (document::jsonb ->> 'frozen_scenario_version')::bigint = frozen_scenario_version
           AND document::jsonb ->> 'supersedes_snapshot_id' IS NOT DISTINCT FROM supersedes_snapshot_id)
);

CREATE TABLE brant_cst.cost_snapshot_component (
    tenant_id     text           NOT NULL,
    company_id    text           NOT NULL,
    snapshot_id   text           NOT NULL,
    ordinal       integer        NOT NULL CHECK (ordinal >= 1),
    component_id  text           NOT NULL CHECK (char_length(component_id) BETWEEN 1 AND 100),
    kind          text           NOT NULL CHECK (kind IN ('purchase', 'charge')),
    obligation_id text           NOT NULL,
    currency      text           NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    amount_minor  numeric(32, 0) NOT NULL CHECK (amount_minor >= 0),
    PRIMARY KEY (tenant_id, company_id, snapshot_id, ordinal),
    UNIQUE (tenant_id, company_id, snapshot_id, component_id),
    FOREIGN KEY (tenant_id, company_id, snapshot_id) REFERENCES brant_cst.cost_snapshot (tenant_id, company_id, snapshot_id)
);

CREATE TABLE brant_cst.cost_snapshot_allocation (
    tenant_id    text           NOT NULL,
    company_id   text           NOT NULL,
    snapshot_id  text           NOT NULL,
    ordinal      integer        NOT NULL CHECK (ordinal >= 1),
    component_id text           NOT NULL,
    line_id      text           NOT NULL,
    basis        text           NOT NULL,
    base_integer text           NOT NULL CHECK (base_integer ~ '^(0|[1-9][0-9]*)$' AND char_length(base_integer) <= 300),
    amount_minor numeric(32, 0) NOT NULL CHECK (amount_minor >= 0),
    PRIMARY KEY (tenant_id, company_id, snapshot_id, ordinal),
    UNIQUE (tenant_id, company_id, snapshot_id, component_id, line_id),
    FOREIGN KEY (tenant_id, company_id, snapshot_id, component_id)
        REFERENCES brant_cst.cost_snapshot_component (tenant_id, company_id, snapshot_id, component_id)
);

CREATE INDEX cost_snapshot_by_context ON brant_cst.cost_snapshot (tenant_id, company_id, context_id);

-- A snapshot freezes the latest state of its scenario: the scenario was just moved to frozen at the
-- snapshot's frozen version (the root is updated first in the same transaction), the evaluation is
-- the scenario's, computed on the version before, verified and comparable, with the hashes and
-- amounts the snapshot repeats; a superseded snapshot costs the same costing context. Snapshots
-- never change and are never deleted.
CREATE FUNCTION brant_cst.guard_cost_snapshot() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    s brant_cst.cost_scenario%ROWTYPE;
    e brant_cst.cost_evaluation%ROWTYPE;
    p brant_cst.cost_snapshot%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cst.cost_snapshot: snapshots never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO s FROM brant_cst.cost_scenario
        WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND scenario_id = NEW.scenario_id;
    SELECT * INTO e FROM brant_cst.cost_evaluation
        WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND evaluation_id = NEW.evaluation_id;
    IF s.status IS DISTINCT FROM 'frozen' OR s.root_version <> NEW.frozen_scenario_version OR e.scenario_id IS DISTINCT FROM NEW.scenario_id
            OR e.scenario_version <> NEW.input_scenario_version OR e.verification_status <> 'verified' OR e.comparison_status <> 'comparable'
            OR e.input_hash <> NEW.input_hash OR e.result_hash <> NEW.result_hash OR e.engine_version <> NEW.engine_version
            OR e.normalization_version <> NEW.normalization_version OR e.currency <> NEW.currency OR e.total_minor <> NEW.total_minor
            OR (e.result::jsonb -> 'context_ref' ->> 'context_id') <> NEW.context_id
            OR (e.result::jsonb -> 'context_ref' ->> 'revision')::bigint <> NEW.context_revision THEN
        RAISE EXCEPTION 'brant_cst.cost_snapshot: snapshot % does not freeze the evaluation of its scenario', NEW.snapshot_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.supersedes_snapshot_id IS NOT NULL THEN
        SELECT * INTO p FROM brant_cst.cost_snapshot
            WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND snapshot_id = NEW.supersedes_snapshot_id;
        IF p.context_id IS DISTINCT FROM NEW.context_id THEN
            RAISE EXCEPTION 'brant_cst.cost_snapshot: snapshot % supersedes a snapshot of another costing context', NEW.snapshot_id
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER cost_snapshot_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_snapshot
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_snapshot();

-- At commit a snapshot holds exactly the children it counts, the components add up to its total and
-- each component's allocations to the component.
CREATE FUNCTION brant_cst.check_cost_snapshot_complete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    components integer;
    allocations integer;
    total numeric;
    unbalanced integer;
BEGIN
    SELECT count(*), coalesce(sum(amount_minor), 0) INTO components, total FROM brant_cst.cost_snapshot_component
        WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND snapshot_id = NEW.snapshot_id;
    SELECT count(*) INTO allocations FROM brant_cst.cost_snapshot_allocation
        WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND snapshot_id = NEW.snapshot_id;
    SELECT count(*) INTO unbalanced FROM brant_cst.cost_snapshot_component c
        WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.snapshot_id = NEW.snapshot_id
          AND c.amount_minor <> (SELECT coalesce(sum(a.amount_minor), 0) FROM brant_cst.cost_snapshot_allocation a
              WHERE a.tenant_id = c.tenant_id AND a.company_id = c.company_id AND a.snapshot_id = c.snapshot_id
                AND a.component_id = c.component_id);
    IF components <> NEW.component_count OR allocations <> NEW.allocation_count OR total <> NEW.total_minor OR unbalanced <> 0 THEN
        RAISE EXCEPTION 'brant_cst.cost_snapshot: snapshot % is not complete or does not add up', NEW.snapshot_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER cost_snapshot_complete AFTER INSERT ON brant_cst.cost_snapshot
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_cst.check_cost_snapshot_complete();

-- A frozen scenario has its snapshot at commit.
CREATE FUNCTION brant_cst.check_cost_scenario_frozen() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_cst.cost_snapshot
            WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND scenario_id = NEW.scenario_id) THEN
        RAISE EXCEPTION 'brant_cst.cost_scenario: scenario % was frozen without a snapshot', NEW.scenario_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER cost_scenario_frozen_has_snapshot AFTER UPDATE ON brant_cst.cost_scenario
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.status = 'draft' AND NEW.status = 'frozen')
    EXECUTE FUNCTION brant_cst.check_cost_scenario_frozen();

-- A child row is inserted only under the ordinal its snapshot counts; it never changes.
CREATE FUNCTION brant_cst.guard_cost_snapshot_child() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    counted integer;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION '%: snapshot rows never change and are never deleted', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_TABLE_NAME = 'cost_snapshot_component' THEN
        SELECT component_count INTO counted FROM brant_cst.cost_snapshot
            WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND snapshot_id = NEW.snapshot_id;
    ELSE
        SELECT allocation_count INTO counted FROM brant_cst.cost_snapshot
            WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND snapshot_id = NEW.snapshot_id;
    END IF;
    IF counted IS NULL OR NEW.ordinal > counted THEN
        RAISE EXCEPTION '%: snapshot % does not count row %', TG_TABLE_NAME, NEW.snapshot_id, NEW.ordinal
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER cost_snapshot_component_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_snapshot_component
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_snapshot_child();
CREATE TRIGGER cost_snapshot_allocation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_snapshot_allocation
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_snapshot_child();

-- A standing row starts undisputed and is denied once, by the denial attestation of its source.
CREATE FUNCTION brant_cst.guard_source_standing() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    a brant_cst.source_attestation%ROWTYPE;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'undisputed' OR NEW.root_version <> 1 THEN
            RAISE EXCEPTION 'brant_cst.source_standing: a source starts undisputed' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status = 'undisputed' AND NEW.status = 'denied' AND NEW.root_version = OLD.root_version + 1
            AND NEW.source_id = OLD.source_id AND NEW.created_at = OLD.created_at THEN
        SELECT * INTO a FROM brant_cst.source_attestation
            WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND attestation_id = NEW.denial_id;
        IF a.verdict = 'denied' AND a.source_id = NEW.source_id AND a.attested_at = NEW.denied_at THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_cst.source_standing: % is not a standing transition', TG_OP USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER source_standing_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.source_standing
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_source_standing();

-- An attestation is appended once; a denied source is not attested again.
CREATE FUNCTION brant_cst.guard_source_attestation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cst.source_attestation: attestations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_cst.source_standing
            WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND source_id = NEW.source_id AND status = 'denied') THEN
        RAISE EXCEPTION 'brant_cst.source_attestation: source % is denied', NEW.source_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER source_attestation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.source_attestation
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_source_attestation();

ALTER TABLE brant_cst.source_standing ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.source_standing FORCE ROW LEVEL SECURITY;
CREATE POLICY source_standing_scope ON brant_cst.source_standing
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.source_attestation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.source_attestation FORCE ROW LEVEL SECURITY;
CREATE POLICY source_attestation_scope ON brant_cst.source_attestation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_snapshot ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_snapshot FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_snapshot_scope ON brant_cst.cost_snapshot
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_snapshot_component ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_snapshot_component FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_snapshot_component_scope ON brant_cst.cost_snapshot_component
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_snapshot_allocation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_snapshot_allocation FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_snapshot_allocation_scope ON brant_cst.cost_snapshot_allocation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- AdoptabilityGuard v1. It always returns one row: known is false (REFERENCE_INVALID) when no
-- snapshot of that ID exists in the caller's scope, or when p_result_hash is given and is not the
-- snapshot's result hash (CostSnapshotRef v1 binds both). Otherwise status is adoptable (code NULL),
-- or, with code COST_SNAPSHOT_INELIGIBLE and in input the first fact that decided it: denied (a
-- source of the snapshot's evaluation denied, /sources/<id>), superseded (/superseded_by/<id>) —
-- whichever happened first, as both are terminal (SM-CST-ADOPT) — or not_current
-- (/inputs/<context>/<kind>/<id>/<revision>): a costing context, measurement or anchor revision of
-- CST that is no longer active, the RFQ no longer ready at its row version and line set (RfqGuard
-- v1), a POL configuration not in force at p_at or changed (ConfigGuard v1), a SUP supply condition
-- not in force at p_at or changed, or a SUP anchor no longer active (SupplyConditionGuard v1). CAT
-- specification revisions publish no guard (Context Map R51 has no guard lock) and are not
-- re-checked here. SECURITY DEFINER runs it as the schema owner, which FORCE ROW LEVEL SECURITY
-- still confines to app.tenant_id and app.company_id; search_path is fixed and every name is
-- qualified. brant_app gets EXECUTE in deploy/postgres/10_app_grants.sql; nobody else does.
CREATE FUNCTION brant_cst.guard_cost_snapshot_adoptable_v1(p_snapshot_id text, p_result_hash text, p_at timestamptz)
    RETURNS TABLE (known boolean, status text, code text, input text, result_hash text, evidence_hash text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    t constant text := pg_catalog.current_setting('app.tenant_id', true);
    c constant text := pg_catalog.current_setting('app.company_id', true);
    ineligible constant text := 'COST_SNAPSHOT_INELIGIBLE';
    s record;
    inputs jsonb;
    i record;
    v record;
    denied_at timestamptz;
    denied_source text;
    successor_at timestamptz;
    successor text;
    path text;
BEGIN
    IF p_snapshot_id IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_cst.guard_cost_snapshot_adoptable_v1: snapshot and instant are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT x.evaluation_id, x.result_hash, x.evidence_hash INTO s FROM brant_cst.cost_snapshot x
        WHERE x.tenant_id = t AND x.company_id = c AND x.snapshot_id = p_snapshot_id
        FOR SHARE;
    IF NOT FOUND OR (p_result_hash IS NOT NULL AND s.result_hash <> p_result_hash) THEN
        RETURN QUERY SELECT false, NULL::text, 'REFERENCE_INVALID'::text, NULL::text, NULL::text, NULL::text;
        RETURN;
    END IF;
    SELECT e.normalized_input::jsonb -> 'inputs' INTO inputs FROM brant_cst.cost_evaluation e
        WHERE e.tenant_id = t AND e.company_id = c AND e.evaluation_id = s.evaluation_id;

    -- The sources, in ID order: a denial updates its row.
    PERFORM 1 FROM brant_cst.source_standing st
        WHERE st.tenant_id = t AND st.company_id = c
          AND st.source_id IN (SELECT x ->> 'id' FROM pg_catalog.jsonb_array_elements(inputs) x WHERE x ->> 'context' = 'evd')
        ORDER BY st.source_id
        FOR SHARE;
    SELECT st.denied_at, st.source_id INTO denied_at, denied_source FROM brant_cst.source_standing st
        WHERE st.tenant_id = t AND st.company_id = c AND st.status = 'denied'
          AND st.source_id IN (SELECT x ->> 'id' FROM pg_catalog.jsonb_array_elements(inputs) x WHERE x ->> 'context' = 'evd')
        ORDER BY st.denied_at, st.source_id
        LIMIT 1;
    -- A freeze that supersedes the snapshot locks its row FOR UPDATE before it inserts the successor.
    SELECT n.frozen_at, n.snapshot_id INTO successor_at, successor FROM brant_cst.cost_snapshot n
        WHERE n.tenant_id = t AND n.company_id = c AND n.supersedes_snapshot_id = p_snapshot_id;
    IF denied_at IS NOT NULL AND (successor_at IS NULL OR denied_at <= successor_at) THEN
        RETURN QUERY SELECT true, 'denied'::text, ineligible, '/sources/' || denied_source, s.result_hash, s.evidence_hash;
        RETURN;
    END IF;
    IF successor_at IS NOT NULL THEN
        RETURN QUERY SELECT true, 'superseded'::text, ineligible, '/superseded_by/' || successor, s.result_hash, s.evidence_hash;
        RETURN;
    END IF;

    -- CST's own inputs, then INQ, POL and SUP through their guards (context code order).
    FOR i IN SELECT x ->> 'context' AS ctx, x ->> 'kind' AS kind, x ->> 'id' AS id, (x ->> 'revision')::bigint AS rev,
                    x ->> 'content_hash' AS hash, x ->> 'use' AS use
             FROM pg_catalog.jsonb_array_elements(inputs) x
             WHERE x ->> 'context' IN ('cst', 'inq', 'pol', 'sup')
             ORDER BY 1, 2, 3, 4 LOOP
        path := '/inputs/' || i.ctx || '/' || i.kind || '/' || i.id || '/' || i.rev;
        IF i.ctx = 'cst' AND i.kind = 'costing_context' THEN
            PERFORM 1 FROM brant_cst.costing_context r WHERE r.tenant_id = t AND r.company_id = c AND r.context_id = i.id FOR SHARE;
            SELECT r.status, r.content_hash INTO v FROM brant_cst.costing_context_revision r
                WHERE r.tenant_id = t AND r.company_id = c AND r.context_id = i.id AND r.revision = i.rev;
            IF NOT FOUND OR v.status <> 'active' OR v.content_hash <> i.hash THEN
                RETURN QUERY SELECT true, 'not_current'::text, ineligible, path, s.result_hash, s.evidence_hash;
                RETURN;
            END IF;
        ELSIF i.ctx = 'cst' AND i.kind = 'measurement' THEN
            PERFORM 1 FROM brant_cst.measurement r WHERE r.tenant_id = t AND r.company_id = c AND r.measurement_id = i.id FOR SHARE;
            SELECT r.status, r.content_hash INTO v FROM brant_cst.measurement_revision r
                WHERE r.tenant_id = t AND r.company_id = c AND r.measurement_id = i.id AND r.revision = i.rev;
            IF NOT FOUND OR v.status <> 'active' OR v.content_hash <> i.hash THEN
                RETURN QUERY SELECT true, 'not_current'::text, ineligible, path, s.result_hash, s.evidence_hash;
                RETURN;
            END IF;
        ELSIF i.ctx = 'inq' THEN
            SELECT g.known, g.code INTO v FROM brant_inq.guard_rfq_ready_v1(i.id, i.rev, i.hash) g;
            IF NOT v.known OR v.code IS NOT NULL THEN
                RETURN QUERY SELECT true, 'not_current'::text, ineligible, path, s.result_hash, s.evidence_hash;
                RETURN;
            END IF;
        ELSIF i.ctx = 'pol' THEN
            SELECT g.known, g.code, g.content_hash INTO v FROM brant_pol.guard_config_in_force_v1(i.kind, i.id, i.rev, p_at) g;
            IF NOT v.known OR v.code IS NOT NULL OR v.content_hash IS DISTINCT FROM i.hash THEN
                RETURN QUERY SELECT true, 'not_current'::text, ineligible, path, s.result_hash, s.evidence_hash;
                RETURN;
            END IF;
        ELSIF i.ctx = 'sup' AND i.use = 'input' THEN
            SELECT g.known, g.code, g.content_hash INTO v FROM brant_sup.guard_supply_condition_v1(i.kind, i.id, i.rev, p_at) g;
            IF NOT v.known OR v.code IS NOT NULL OR v.content_hash IS DISTINCT FROM i.hash THEN
                RETURN QUERY SELECT true, 'not_current'::text, ineligible, path, s.result_hash, s.evidence_hash;
                RETURN;
            END IF;
        ELSIF i.ctx = 'sup' AND i.use = 'anchor' THEN
            SELECT g.known, g.status INTO v FROM brant_sup.guard_supply_condition_v1(i.kind, i.id, i.rev, p_at) g;
            IF NOT v.known OR v.status IS DISTINCT FROM 'active' THEN
                RETURN QUERY SELECT true, 'not_current'::text, ineligible, path, s.result_hash, s.evidence_hash;
                RETURN;
            END IF;
        END IF;
    END LOOP;
    RETURN QUERY SELECT true, 'adoptable'::text, NULL::text, NULL::text, s.result_hash, s.evidence_hash;
END
$$;

REVOKE ALL ON FUNCTION brant_cst.guard_cost_snapshot_adoptable_v1(text, text, timestamptz) FROM PUBLIC;
