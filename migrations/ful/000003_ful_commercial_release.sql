-- FUL: the commercial release of a shipment (tactical model FUL-AGG-CommercialRelease,
-- SM-FUL-RELEASE, FUL-INV-002; UL-INV-28, MD-38; Context Map R26 QLT->FUL, R27 TRD->FUL, R44 PTY->FUL,
-- R45 STL->FUL, R56 CNX->FUL, R59 PUR->FUL; WF-07, ES-10, SC-08, BT13; WBS S5-07; owner decision
-- D-11).
--
-- A release is the root of one shipment and scope: shipment (the actual shipment) or documents (the
-- original bill of lading or the telex release). commercial_release.evaluate creates it with its
-- first evaluation and moves it between blocked and granted by every evaluation;
-- commercial_release.grant_exception moves a blocked release to granted by a payment-condition
-- exception; shipment.confirm (scope shipment) or document_release.record (scope documents)
-- consumes a granted release, once. Every evaluation and exception is kept, with the guard evidence
-- of its transaction as canonical JSON and its SHA-256 and, for an evaluation, what each guard
-- answered (bindings). A confirmed shipment has a consumed shipment release, written in the
-- confirming transaction (checked at commit).
--
-- References to PTY, PUR, QLT, STL, CNX and TRD are plain IDs and hashes: no foreign key crosses a
-- schema (DB2).

CREATE TABLE brant_ful.commercial_release (
    tenant_id              text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id             text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    release_id             text        NOT NULL CHECK (release_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    shipment_id            text        NOT NULL,
    scope                  text        NOT NULL CHECK (scope IN ('shipment', 'documents')),
    status                 text        NOT NULL CHECK (status IN ('blocked', 'granted', 'consumed')),
    row_version            bigint      NOT NULL CHECK (row_version > 0),
    -- the order the shipment binds
    order_id               text        NOT NULL,
    order_revision         bigint      NOT NULL CHECK (order_revision > 0),
    order_hash             text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    evaluations            bigint      NOT NULL CHECK (evaluations > 0),
    exceptions             bigint      NOT NULL CHECK (exceptions >= 0),
    opened_by_kind         text        NOT NULL CHECK (opened_by_kind IN ('human', 'ai', 'system')),
    opened_by_id           text        NOT NULL,
    opened_at              timestamptz NOT NULL,
    consumed_command       text,
    consumed_by_kind       text,
    consumed_by_id         text,
    consume_epoch          bigint,
    consumed_at            timestamptz,
    consume_evidence_ids   text[],
    -- the guard evidence of the consuming transaction as canonical JSON and its SHA-256
    consume_entries        text,
    consume_entries_sha256 text,
    document_release_kind  text CHECK (document_release_kind IN ('original_bl', 'telex_release')),
    PRIMARY KEY (tenant_id, company_id, release_id),
    UNIQUE (tenant_id, company_id, shipment_id, scope),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    CHECK (row_version = evaluations + exceptions + (status = 'consumed')::int),
    CHECK (num_nulls(consumed_command, consumed_by_kind, consumed_by_id, consume_epoch, consumed_at, consume_evidence_ids, consume_entries,
                     consume_entries_sha256) IN (0, 8)),
    CHECK ((status = 'consumed') = (consumed_at IS NOT NULL)),
    CHECK (consumed_at IS NULL OR (consumed_by_kind = 'human' AND consume_epoch > 0 AND consumed_at >= opened_at
        AND cardinality(consume_evidence_ids) <= 20 AND left(consume_entries, 1) = '['
        AND consume_entries_sha256 = encode(sha256(convert_to(consume_entries, 'UTF8')), 'hex'))),
    CHECK ((scope = 'shipment' AND coalesce(consumed_command, 'shipment.confirm') = 'shipment.confirm' AND document_release_kind IS NULL)
        OR (scope = 'documents' AND coalesce(consumed_command, 'document_release.record') = 'document_release.record'
            AND (document_release_kind IS NULL) = (consumed_at IS NULL)
            AND (consumed_at IS NULL OR cardinality(consume_evidence_ids) >= 1)))
);

CREATE TABLE brant_ful.commercial_release_evaluation (
    tenant_id            text        NOT NULL,
    company_id           text        NOT NULL,
    release_id           text        NOT NULL,
    seq                  bigint      NOT NULL CHECK (seq > 0),
    release_version      bigint      NOT NULL CHECK (release_version > 0),
    outcome              text        NOT NULL CHECK (outcome IN ('granted', 'blocked')),
    -- blocked: the first unmet guard, its code and why
    guard                text,
    code                 text        CHECK (code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    reason               text        CHECK (char_length(reason) <= 2000),
    -- granted: the exception that covered the payment condition
    exception_seq        bigint      CHECK (exception_seq > 0),
    guard_entries        text        NOT NULL CHECK (left(guard_entries, 1) = '['),
    guard_entries_sha256 text        NOT NULL,
    bindings             jsonb       NOT NULL CHECK (jsonb_typeof(bindings) = 'object'),
    evaluated_by_kind    text        NOT NULL CHECK (evaluated_by_kind IN ('human', 'ai', 'system')),
    evaluated_by_id      text        NOT NULL,
    evaluate_epoch       bigint      NOT NULL CHECK (evaluate_epoch > 0),
    evaluated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, release_id, seq),
    UNIQUE (tenant_id, company_id, release_id, release_version),
    FOREIGN KEY (tenant_id, company_id, release_id) REFERENCES brant_ful.commercial_release (tenant_id, company_id, release_id),
    CHECK ((outcome = 'granted') = (code IS NULL)),
    CHECK ((outcome = 'granted') = (guard IS NULL)),
    CHECK (outcome = 'granted' OR exception_seq IS NULL),
    CHECK (guard_entries_sha256 = encode(sha256(convert_to(guard_entries, 'UTF8')), 'hex'))
);

CREATE TABLE brant_ful.commercial_release_exception (
    tenant_id            text          NOT NULL,
    company_id           text          NOT NULL,
    release_id           text          NOT NULL,
    seq                  bigint        NOT NULL CHECK (seq > 0),
    release_version      bigint        NOT NULL CHECK (release_version > 1),
    -- bound to the order revision, the release's shipment and scope, the stage and the shortfall
    order_id             text          NOT NULL,
    order_revision       bigint        NOT NULL CHECK (order_revision > 0),
    stage_id             text          NOT NULL CHECK (stage_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    currency             text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    shortfall_minor      numeric(30,0) NOT NULL CHECK (shortfall_minor > 0),
    valid_until          timestamptz   NOT NULL,
    reason               text          NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    evidence_ids         text[]        NOT NULL CHECK (cardinality(evidence_ids) <= 20),
    guard_entries        text          NOT NULL CHECK (left(guard_entries, 1) = '['),
    guard_entries_sha256 text          NOT NULL,
    granted_by_kind      text          NOT NULL CHECK (granted_by_kind = 'human'),
    granted_by_id        text          NOT NULL,
    grant_epoch          bigint        NOT NULL CHECK (grant_epoch > 0),
    granted_at           timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, release_id, seq),
    UNIQUE (tenant_id, company_id, release_id, release_version),
    FOREIGN KEY (tenant_id, company_id, release_id) REFERENCES brant_ful.commercial_release (tenant_id, company_id, release_id),
    CHECK (valid_until > granted_at AND valid_until <= granted_at + interval '90 days'),
    CHECK (guard_entries_sha256 = encode(sha256(convert_to(guard_entries, 'UTF8')), 'hex'))
);

-- A release is inserted by its first evaluation (row version 1, blocked or granted). It then
-- changes only by one more evaluation (blocked or granted), by an exception (blocked to granted) or
-- by its consumption (granted to consumed, once); its shipment, scope, order and opening never
-- change; a consumed release never changes; it is never deleted (SM-FUL-RELEASE).
CREATE FUNCTION brant_ful.guard_commercial_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_evaluate text[] := ARRAY['status', 'row_version', 'evaluations'];
    v_except   text[] := ARRAY['status', 'row_version', 'exceptions'];
    v_consume  text[] := ARRAY['status', 'row_version', 'consumed_command', 'consumed_by_kind', 'consumed_by_id', 'consume_epoch',
                               'consumed_at', 'consume_evidence_ids', 'consume_entries', 'consume_entries_sha256', 'document_release_kind'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.commercial_release: releases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('blocked', 'granted') OR NEW.row_version <> 1 OR NEW.evaluations <> 1 OR NEW.exceptions <> 0 THEN
            RAISE EXCEPTION 'brant_ful.commercial_release: a release is inserted by its first evaluation'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status IN ('blocked', 'granted') AND NEW.row_version = OLD.row_version + 1 THEN
        IF NEW.status IN ('blocked', 'granted') AND NEW.evaluations = OLD.evaluations + 1
                AND (to_jsonb(NEW) - v_evaluate) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_evaluate) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'blocked' AND NEW.status = 'granted' AND NEW.exceptions = OLD.exceptions + 1
                AND (to_jsonb(NEW) - v_except) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_except) THEN
            RETURN NEW;
        END IF;
        IF OLD.status = 'granted' AND NEW.status = 'consumed'
                AND (to_jsonb(NEW) - v_consume) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_consume) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_ful.commercial_release: release % changes only by an evaluation, an exception or its consumption (SM-FUL-RELEASE)',
        OLD.release_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER commercial_release_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.commercial_release
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_commercial_release();

-- Evaluations and exceptions are only inserted, each at the row version it gave its release: an
-- evaluation with the state it left, an exception granting the release on the order the release
-- binds.
CREATE FUNCTION brant_ful.guard_commercial_release_part() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_release record;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_ful.%: rows never change and are never deleted', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT r.* INTO v_release FROM brant_ful.commercial_release r
    WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.release_id = NEW.release_id;
    IF NOT FOUND OR v_release.row_version <> NEW.release_version THEN
        RAISE EXCEPTION 'brant_ful.%: release % is not at version %', TG_TABLE_NAME, NEW.release_id, NEW.release_version
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_TABLE_NAME = 'commercial_release_evaluation' THEN
        IF v_release.status <> NEW.outcome OR v_release.evaluations <> NEW.seq THEN
            RAISE EXCEPTION 'brant_ful.commercial_release_evaluation: evaluation % does not leave release % %', NEW.seq, NEW.release_id, NEW.outcome
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    ELSIF v_release.status <> 'granted' OR v_release.exceptions <> NEW.seq OR v_release.order_id <> NEW.order_id
            OR v_release.order_revision <> NEW.order_revision THEN
        RAISE EXCEPTION 'brant_ful.commercial_release_exception: exception % does not grant release % on its order', NEW.seq, NEW.release_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER commercial_release_evaluation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.commercial_release_evaluation
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_commercial_release_part();
CREATE TRIGGER commercial_release_exception_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.commercial_release_exception
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_commercial_release_part();

-- At commit: a release has exactly the evaluations and exceptions it counts, and its order and
-- shipment agree with the shipment's; a consumed release's shipment is confirmed.
CREATE FUNCTION brant_ful.check_commercial_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_release     record;
    v_evaluations bigint;
    v_exceptions  bigint;
    v_ship        record;
BEGIN
    SELECT r.* INTO v_release FROM brant_ful.commercial_release r
    WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.release_id = NEW.release_id;
    SELECT count(*) INTO v_evaluations FROM brant_ful.commercial_release_evaluation e
    WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.release_id = NEW.release_id;
    SELECT count(*) INTO v_exceptions FROM brant_ful.commercial_release_exception x
    WHERE x.tenant_id = NEW.tenant_id AND x.company_id = NEW.company_id AND x.release_id = NEW.release_id;
    IF v_evaluations <> v_release.evaluations OR v_exceptions <> v_release.exceptions THEN
        RAISE EXCEPTION 'brant_ful.commercial_release: release % counts % evaluations and % exceptions, the rows are % and %',
            v_release.release_id, v_release.evaluations, v_release.exceptions, v_evaluations, v_exceptions
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT s.* INTO v_ship FROM brant_ful.shipment s
    WHERE s.tenant_id = v_release.tenant_id AND s.company_id = v_release.company_id AND s.shipment_id = v_release.shipment_id;
    IF v_ship.order_id <> v_release.order_id OR v_ship.order_revision <> v_release.order_revision OR v_ship.order_hash <> v_release.order_hash
            OR (v_release.status = 'consumed' AND v_ship.status <> 'confirmed') THEN
        RAISE EXCEPTION 'brant_ful.commercial_release: release % does not agree with shipment % (%)', v_release.release_id,
            v_release.shipment_id, v_ship.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER commercial_release_at_commit AFTER INSERT OR UPDATE ON brant_ful.commercial_release
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_commercial_release();
CREATE CONSTRAINT TRIGGER commercial_release_evaluation_at_commit AFTER INSERT ON brant_ful.commercial_release_evaluation
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_commercial_release();
CREATE CONSTRAINT TRIGGER commercial_release_exception_at_commit AFTER INSERT ON brant_ful.commercial_release_exception
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_commercial_release();

-- At commit: a shipment confirmed from now on consumed its shipment release in the confirming
-- transaction (FUL-INV-002, D-11: no confirmation without a granted release).
CREATE FUNCTION brant_ful.check_shipment_released() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_ful.commercial_release r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.shipment_id = NEW.shipment_id AND r.scope = 'shipment' AND r.status = 'consumed' AND r.consumed_at = NEW.confirmed_at) THEN
        RAISE EXCEPTION 'brant_ful.shipment: shipment % is confirmed without consuming its shipment release (FUL-INV-002)', NEW.shipment_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER shipment_released_at_commit AFTER UPDATE ON brant_ful.shipment
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.status = 'prepared' AND NEW.status = 'confirmed')
    EXECUTE FUNCTION brant_ful.check_shipment_released();

ALTER TABLE brant_ful.commercial_release ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.commercial_release FORCE ROW LEVEL SECURITY;
CREATE POLICY commercial_release_scope ON brant_ful.commercial_release
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.commercial_release_evaluation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.commercial_release_evaluation FORCE ROW LEVEL SECURITY;
CREATE POLICY commercial_release_evaluation_scope ON brant_ful.commercial_release_evaluation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.commercial_release_exception ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.commercial_release_exception FORCE ROW LEVEL SECURITY;
CREATE POLICY commercial_release_exception_scope ON brant_ful.commercial_release_exception
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
