-- CNX: the trade compliance check of a sales order revision (tactical model
-- CNX-AGG-TradeComplianceCheck, SM-CNX-COMPLIANCE, CNX-INV-001; TRD-INV-023; WBS S5-13; S5 decisions
-- D-8, D-9). Python V0.50 has no CNX.
--
-- A check is one root per order revision (tenant, company, order_id, order_revision): a later
-- revision of the order never inherits an earlier revision's conclusion. Its state moves along
-- SM-CNX-COMPLIANCE only (trigger): pending → clear | blocked (a human decision), clear → pending
-- (a reopening), blocked → pending and pending → pending (a new evaluation); its identity, binding,
-- owner and opening never change; its row version moves by one per command; nothing is deleted.
-- Evaluations, decisions and reopenings are only inserted (triggers): an evaluation stores its
-- document as the exact canonical JSON bytes its facts hash covers, which a CHECK recomputes, and
-- binds the check's order revision and hash, with the country programme list versions it used; a
-- decision is human, binds its evaluation's
-- regulation revision and, when clear, a validity; a reopening names its cause. A deferred trigger
-- checks at commit that a decided check's state is its current decision's conclusion on its current
-- evaluation.
--
-- brant_cnx.guard_trade_compliance_v1 publishes TradeComplianceGuard v1 (Context Map R58 CNX->TRD
-- and R56 CNX->FUL, guardLock): whether a valid clear conclusion covers an order revision at its
-- order hash, holding the check row FOR SHARE until the caller's transaction ends, and whether the
-- programme lists the conclusion used are still at those versions, holding each list row FOR SHARE
-- through reference data's list guard (brant_ref.country_programme_list_current_v1; ref precedes cnx
-- in the manifest). CNX's reads derive the same state with brant_cnx.trade_compliance_state without
-- locks.
--
-- References to TRD, PTY, CAT, POL, EVD and reference data are plain IDs and hashes inside the rows
-- and the documents: no foreign key crosses a schema (DB2). action_result and audit_event are the
-- command ledger of CNX (internal/foundation/action).

CREATE SCHEMA brant_cnx;

CREATE TABLE brant_cnx.trade_compliance_check (
    tenant_id             text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id            text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    check_id              text        NOT NULL CHECK (check_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id              text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision        bigint      NOT NULL CHECK (order_revision > 0),
    order_hash            text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    status                text        NOT NULL CHECK (status IN ('pending', 'clear', 'blocked')),
    row_version           bigint      NOT NULL CHECK (row_version > 0),
    current_evaluation_id text,
    current_decision_id   text,
    previous_check_id     text,
    owner_actor_id        text        NOT NULL,
    owner_team_id         text,
    opened_by_kind        text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai', 'system')),
    opened_by_id          text        NOT NULL,
    open_epoch            bigint      NOT NULL CHECK (open_epoch > 0),
    opened_at             timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, check_id),
    UNIQUE (tenant_id, company_id, order_id, order_revision),
    CHECK (owner_actor_id = opened_by_id),
    CHECK ((status = 'pending') = (current_decision_id IS NULL)),
    CHECK (current_decision_id IS NULL OR current_evaluation_id IS NOT NULL),
    CHECK (updated_at >= opened_at)
);

-- A check is opened pending at version 1. Its identity, binding, owner and opening never change; its
-- version moves by one; its state moves along SM-CNX-COMPLIANCE: a new evaluation keeps or makes it
-- pending (not from clear), a decision concludes the current evaluation, a reopening returns a clear
-- one to pending without an evaluation. It is never deleted.
CREATE FUNCTION brant_cnx.guard_trade_compliance_check() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'row_version', 'current_evaluation_id', 'current_decision_id', 'updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_check: checks are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'pending' OR NEW.row_version <> 1 THEN
            RAISE EXCEPTION 'brant_cnx.trade_compliance_check: a check is opened pending at version 1' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(NEW) - v_moving) IS DISTINCT FROM (to_jsonb(OLD) - v_moving) OR NEW.row_version <> OLD.row_version + 1
            OR NEW.updated_at < OLD.updated_at
            OR NOT (
                -- evaluate: pending or blocked to pending with a new current evaluation
                (OLD.status IN ('pending', 'blocked') AND NEW.status = 'pending' AND NEW.current_evaluation_id IS NOT NULL
                    AND NEW.current_evaluation_id IS DISTINCT FROM OLD.current_evaluation_id)
                -- decide: the current evaluation concluded
                OR (OLD.status = 'pending' AND NEW.status IN ('clear', 'blocked') AND OLD.current_evaluation_id IS NOT NULL
                    AND NEW.current_evaluation_id = OLD.current_evaluation_id)
                -- reopen: clear to pending, no current evaluation
                OR (OLD.status = 'clear' AND NEW.status = 'pending' AND NEW.current_evaluation_id IS NULL)) THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_check: check % moves only along SM-CNX-COMPLIANCE, by one version, with its binding fixed',
            OLD.check_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER trade_compliance_check_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.trade_compliance_check
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_trade_compliance_check();

CREATE INDEX trade_compliance_check_order ON brant_cnx.trade_compliance_check (tenant_id, company_id, order_id);

CREATE TABLE brant_cnx.trade_compliance_evaluation (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    evaluation_id       text        NOT NULL CHECK (evaluation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    check_id            text        NOT NULL,
    seq                 integer     NOT NULL CHECK (seq > 0),
    order_id            text        NOT NULL,
    order_revision      bigint      NOT NULL,
    order_hash          text        NOT NULL,
    regulation_id       text        NOT NULL CHECK (regulation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,39}$'),
    regulation_revision bigint      NOT NULL CHECK (regulation_revision > 0),
    regulation_hash     text        NOT NULL CHECK (regulation_hash ~ '^[0-9a-f]{64}$'),
    outcome             text        NOT NULL CHECK (outcome IN ('no_match', 'match', 'incomplete')),
    document            text        NOT NULL CHECK (left(document, 1) = '{'),
    facts_hash          text        NOT NULL,
    valid_until         timestamptz NOT NULL,
    evaluated_by_kind   text        NOT NULL CHECK (evaluated_by_kind IN ('human', 'ai', 'system')),
    evaluated_by_id     text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    evaluated_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, evaluation_id),
    UNIQUE (tenant_id, company_id, check_id, evaluation_id),
    UNIQUE (tenant_id, company_id, check_id, seq),
    FOREIGN KEY (tenant_id, company_id, check_id) REFERENCES brant_cnx.trade_compliance_check (tenant_id, company_id, check_id),
    CHECK (facts_hash = encode(sha256(convert_to(document, 'UTF8')), 'hex')),
    CHECK (valid_until > evaluated_at)
);

-- An evaluation is only inserted, for its check's order revision at its order hash, next in its
-- check's sequence.
CREATE FUNCTION brant_cnx.guard_trade_compliance_evaluation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_evaluation: evaluations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_cnx.trade_compliance_check c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.check_id = NEW.check_id AND c.order_id = NEW.order_id AND c.order_revision = NEW.order_revision
            AND c.order_hash = NEW.order_hash)
            OR NEW.seq <> 1 + (SELECT count(*) FROM brant_cnx.trade_compliance_evaluation e WHERE e.tenant_id = NEW.tenant_id
                AND e.company_id = NEW.company_id AND e.check_id = NEW.check_id) THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_evaluation: an evaluation binds its check''s order revision and comes next in its sequence'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER trade_compliance_evaluation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.trade_compliance_evaluation
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_trade_compliance_evaluation();

-- The country programme list versions an evaluation used (reference data, S5 A3b): inserted with
-- the evaluation, never changed.
CREATE TABLE brant_cnx.trade_compliance_list_used (
    tenant_id     text NOT NULL,
    company_id    text NOT NULL,
    evaluation_id text NOT NULL,
    check_id      text NOT NULL,
    list_code     text NOT NULL CHECK (list_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,31}$'),
    jurisdiction  text NOT NULL CHECK (jurisdiction IN ('CN', 'UN', 'foreign')),
    version       text NOT NULL,
    content_hash  text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, company_id, evaluation_id, list_code),
    FOREIGN KEY (tenant_id, company_id, check_id, evaluation_id)
        REFERENCES brant_cnx.trade_compliance_evaluation (tenant_id, company_id, check_id, evaluation_id)
);

CREATE FUNCTION brant_cnx.guard_trade_compliance_list_used() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_list_used: the lists an evaluation used never change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER trade_compliance_list_used_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.trade_compliance_list_used
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_trade_compliance_list_used();

CREATE TABLE brant_cnx.trade_compliance_decision (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    decision_id         text        NOT NULL CHECK (decision_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    check_id            text        NOT NULL,
    evaluation_id       text        NOT NULL,
    conclusion          text        NOT NULL CHECK (conclusion IN ('clear', 'blocked')),
    reason              text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    license_required    boolean     NOT NULL,
    regulation_id       text        NOT NULL,
    regulation_revision bigint      NOT NULL,
    regulation_hash     text        NOT NULL,
    valid_until         timestamptz,
    evidence            text        NOT NULL CHECK (left(evidence, 1) = '['),
    decided_by_kind     text        NOT NULL CHECK (decided_by_kind = 'human'),
    decided_by_id       text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    decided_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, decision_id),
    UNIQUE (tenant_id, company_id, check_id, decision_id),
    FOREIGN KEY (tenant_id, company_id, check_id, evaluation_id)
        REFERENCES brant_cnx.trade_compliance_evaluation (tenant_id, company_id, check_id, evaluation_id),
    CHECK (CASE conclusion WHEN 'clear' THEN valid_until IS NOT NULL AND valid_until > decided_at
        ELSE valid_until IS NULL AND NOT license_required END)
);

-- A decision is only inserted, by a human, for the regulation revision its evaluation followed and
-- never past the evaluation's validity.
CREATE FUNCTION brant_cnx.guard_trade_compliance_decision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_decision: decisions never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_cnx.trade_compliance_evaluation e WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id
            AND e.check_id = NEW.check_id AND e.evaluation_id = NEW.evaluation_id AND e.regulation_id = NEW.regulation_id
            AND e.regulation_revision = NEW.regulation_revision AND e.regulation_hash = NEW.regulation_hash
            AND (NEW.valid_until IS NULL OR NEW.valid_until <= e.valid_until)) THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_decision: a decision follows its evaluation''s regulation revision within its validity'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER trade_compliance_decision_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.trade_compliance_decision
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_trade_compliance_decision();

CREATE TABLE brant_cnx.trade_compliance_reopening (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    reopening_id        text        NOT NULL CHECK (reopening_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    check_id            text        NOT NULL,
    cause               text        NOT NULL CHECK (cause IN ('regulation_revised', 'programme_updated', 'expired', 'order_amended')),
    decision_id         text,
    regulation_id       text,
    regulation_revision bigint,
    updated_lists       text[]      NOT NULL,
    reason              text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    reopened_by_kind    text        NOT NULL CHECK (reopened_by_kind IN ('human', 'system')),
    reopened_by_id      text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    reopened_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, reopening_id),
    FOREIGN KEY (tenant_id, company_id, check_id) REFERENCES brant_cnx.trade_compliance_check (tenant_id, company_id, check_id),
    FOREIGN KEY (tenant_id, company_id, check_id, decision_id)
        REFERENCES brant_cnx.trade_compliance_decision (tenant_id, company_id, check_id, decision_id),
    CHECK ((cause = 'order_amended') = (decision_id IS NULL)),
    CHECK ((cause = 'regulation_revised') = (regulation_id IS NOT NULL)),
    CHECK ((regulation_id IS NULL) = (regulation_revision IS NULL)),
    CHECK ((cause = 'programme_updated') = (cardinality(updated_lists) > 0))
);

CREATE INDEX trade_compliance_reopening_check ON brant_cnx.trade_compliance_reopening (tenant_id, company_id, check_id);

-- A reopening is only inserted.
CREATE FUNCTION brant_cnx.guard_trade_compliance_reopening() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_reopening: reopenings never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER trade_compliance_reopening_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cnx.trade_compliance_reopening
    FOR EACH ROW EXECUTE FUNCTION brant_cnx.guard_trade_compliance_reopening();

-- A check's current evaluation and decision are its own (checked at commit: the check row is
-- written before the rows it names).
ALTER TABLE brant_cnx.trade_compliance_check ADD FOREIGN KEY (tenant_id, company_id, check_id, current_evaluation_id)
    REFERENCES brant_cnx.trade_compliance_evaluation (tenant_id, company_id, check_id, evaluation_id) DEFERRABLE INITIALLY DEFERRED;
ALTER TABLE brant_cnx.trade_compliance_check ADD FOREIGN KEY (tenant_id, company_id, check_id, current_decision_id)
    REFERENCES brant_cnx.trade_compliance_decision (tenant_id, company_id, check_id, decision_id) DEFERRABLE INITIALLY DEFERRED;

-- At commit: a decided check's state is its current decision's conclusion, on its current
-- evaluation.
CREATE FUNCTION brant_cnx.check_trade_compliance_current() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.current_decision_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_cnx.trade_compliance_decision d
            WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.check_id = NEW.check_id
                AND d.decision_id = NEW.current_decision_id AND d.evaluation_id = NEW.current_evaluation_id
                AND d.conclusion = NEW.status) THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_check: check % is % without a % decision on its current evaluation', NEW.check_id,
            NEW.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER trade_compliance_check_current AFTER INSERT OR UPDATE ON brant_cnx.trade_compliance_check
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_cnx.check_trade_compliance_current();

CREATE TABLE brant_cnx.action_result (
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

CREATE TABLE brant_cnx.audit_event (
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

ALTER TABLE brant_cnx.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_cnx.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_cnx.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.trade_compliance_check ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.trade_compliance_check FORCE ROW LEVEL SECURITY;
CREATE POLICY trade_compliance_check_scope ON brant_cnx.trade_compliance_check
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.trade_compliance_evaluation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.trade_compliance_evaluation FORCE ROW LEVEL SECURITY;
CREATE POLICY trade_compliance_evaluation_scope ON brant_cnx.trade_compliance_evaluation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.trade_compliance_list_used ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.trade_compliance_list_used FORCE ROW LEVEL SECURITY;
CREATE POLICY trade_compliance_list_used_scope ON brant_cnx.trade_compliance_list_used
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.trade_compliance_decision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.trade_compliance_decision FORCE ROW LEVEL SECURITY;
CREATE POLICY trade_compliance_decision_scope ON brant_cnx.trade_compliance_decision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cnx.trade_compliance_reopening ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cnx.trade_compliance_reopening FORCE ROW LEVEL SECURITY;
CREATE POLICY trade_compliance_reopening_scope ON brant_cnx.trade_compliance_reopening
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The trade compliance state of an order revision at its order hash at p_at, in the caller's
-- scope; with p_lock the check row, then the programme lists the clear conclusion used (in
-- list-code order, through reference data's list guard), stay locked FOR SHARE until the caller's
-- transaction ends. A revision without a check locks nothing (a check is opened pending). The checks
-- run from the most to the least lasting: no check, another order hash, pending, blocked, the clear
-- conclusion's validity, the programme list versions it used (MD-47: a new version makes it due).
CREATE FUNCTION brant_cnx.trade_compliance_state(p_order_id text, p_order_revision bigint, p_order_hash text, p_at timestamptz, p_lock boolean)
    RETURNS TABLE (clear boolean, status text, reason text, check_id text, decision_id text, license_required boolean,
                   regulation_id text, regulation_revision bigint, valid_until timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_tenant   text := pg_catalog.current_setting('app.tenant_id', true);
    v_company  text := pg_catalog.current_setting('app.company_id', true);
    v_check    record;
    v_decision record;
    v_list     record;
    v_code     text := 'TRADE_COMPLIANCE_NOT_CLEAR';
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_order_hash IS NULL OR p_at IS NULL OR p_lock IS NULL THEN
        RAISE EXCEPTION 'brant_cnx.trade_compliance_state: order, order revision, order hash, instant and lock mode are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_lock THEN
        SELECT c.check_id, c.order_hash, c.status, c.current_evaluation_id, c.current_decision_id INTO v_check
        FROM brant_cnx.trade_compliance_check c
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id AND c.order_revision = p_order_revision
        FOR SHARE;
    ELSE
        SELECT c.check_id, c.order_hash, c.status, c.current_evaluation_id, c.current_decision_id INTO v_check
        FROM brant_cnx.trade_compliance_check c
        WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.order_id = p_order_id AND c.order_revision = p_order_revision;
    END IF;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, 'pending'::text,
            CASE WHEN EXISTS (SELECT 1 FROM brant_cnx.trade_compliance_check o WHERE o.tenant_id = v_tenant AND o.company_id = v_company
                AND o.order_id = p_order_id) THEN 'order_revised' ELSE 'not_checked' END::text,
            ''::text, ''::text, false, NULL::text, NULL::bigint, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_check.current_decision_id IS NOT NULL THEN
        SELECT d.license_required, d.regulation_id, d.regulation_revision, d.valid_until INTO v_decision
        FROM brant_cnx.trade_compliance_decision d
        WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.check_id = v_check.check_id
            AND d.decision_id = v_check.current_decision_id;
    END IF;
    IF v_check.order_hash <> p_order_hash THEN
        RETURN QUERY SELECT false, v_check.status, 'order_changed'::text, v_check.check_id, coalesce(v_check.current_decision_id, ''), false,
            NULL::text, NULL::bigint, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_check.status = 'pending' THEN
        RETURN QUERY SELECT false, 'pending'::text,
            CASE WHEN v_check.current_evaluation_id IS NULL THEN 'not_evaluated' ELSE 'awaiting_decision' END::text,
            v_check.check_id, ''::text, false, NULL::text, NULL::bigint, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_check.status = 'blocked' THEN
        RETURN QUERY SELECT false, 'blocked'::text, 'blocked'::text, v_check.check_id, v_check.current_decision_id, false,
            v_decision.regulation_id, v_decision.regulation_revision, NULL::timestamptz, v_code;
        RETURN;
    END IF;
    IF v_decision.valid_until <= p_at THEN
        RETURN QUERY SELECT false, 'expired'::text, 'expired'::text, v_check.check_id, v_check.current_decision_id, v_decision.license_required,
            v_decision.regulation_id, v_decision.regulation_revision, v_decision.valid_until, v_code;
        RETURN;
    END IF;
    FOR v_list IN
        SELECT u.list_code AS code, u.version AS version FROM brant_cnx.trade_compliance_list_used u
        WHERE u.tenant_id = v_tenant AND u.company_id = v_company AND u.check_id = v_check.check_id
            AND u.evaluation_id = v_check.current_evaluation_id
        ORDER BY u.list_code
    LOOP
        IF brant_ref.country_programme_list_current_v1(v_list.code, p_lock) IS DISTINCT FROM v_list.version THEN
            RETURN QUERY SELECT false, 'pending'::text, 'list_updated'::text, v_check.check_id, v_check.current_decision_id,
                v_decision.license_required, v_decision.regulation_id, v_decision.regulation_revision, v_decision.valid_until, v_code;
            RETURN;
        END IF;
    END LOOP;
    RETURN QUERY SELECT true, 'clear'::text, 'cleared'::text, v_check.check_id, v_check.current_decision_id, v_decision.license_required,
        v_decision.regulation_id, v_decision.regulation_revision, v_decision.valid_until, NULL::text;
END
$$;
REVOKE ALL ON FUNCTION brant_cnx.trade_compliance_state(text, bigint, text, timestamptz, boolean) FROM PUBLIC;

-- TradeComplianceGuard v1: the state with the FOR SHARE lock, for a downstream context's
-- transaction (TRD order.activate, R58; FUL release, R56).
CREATE FUNCTION brant_cnx.guard_trade_compliance_v1(p_order_id text, p_order_revision bigint, p_order_hash text, p_at timestamptz)
    RETURNS TABLE (clear boolean, status text, reason text, check_id text, decision_id text, license_required boolean,
                   regulation_id text, regulation_revision bigint, valid_until timestamptz, code text)
    LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT s.clear, s.status, s.reason, s.check_id, s.decision_id, s.license_required, s.regulation_id, s.regulation_revision,
        s.valid_until, s.code
    FROM brant_cnx.trade_compliance_state(p_order_id, p_order_revision, p_order_hash, p_at, true) s
$$;
REVOKE ALL ON FUNCTION brant_cnx.guard_trade_compliance_v1(text, bigint, text, timestamptz) FROM PUBLIC;
