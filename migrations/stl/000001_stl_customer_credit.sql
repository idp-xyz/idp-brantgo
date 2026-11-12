-- STL: customer credit (tactical model STL-AGG-CustomerCredit; SM-STL-CREDIT-LIMIT,
-- SM-STL-CREDIT-RESERVATION, SM-STL-CREDIT-DECISION; STL-INV-004, STL-INV-005; Context Map R57
-- CreditGuard v1; WBS S5-12a; S5 plan D-4 to D-7, owner decision E-2). Python V0.50 has no STL.
--
-- CustomerCredit is one root per customer of a company (tenant, company, party_id). Every command
-- that changes a customer's limit requests, limit versions, reservations or decisions locks the root
-- FOR UPDATE first and moves its row version by one, so a customer's credit is judged one command
-- at a time: two reservations that together exceed the limit commit one after the other, and the
-- second sees the first (STL-INV-004). Balances are derived from the immutable facts under that
-- lock, never stored (S5 D-3): the exposure is the live reservations (and, from B2b, the
-- receivables) of the customer.
--
-- A limit request is requested once and decided once (published or rejected), its version moving by
-- one per approval; a published one has exactly one immutable limit version. A decision is opened
-- once and decided once (released or rejected); its expiry is derived (principle 3), never written.
-- A reservation is reserved once and then consumed and released, or released; its expiry is
-- derived too, and a reservation of an order revision that is reserved or consumed is unique
-- (WF-08). Approvals are only appended. Nothing is deleted.
--
-- brant_stl.guard_credit_reservation_v1 publishes CreditGuard v1 (R57, guardLock): TRD calls it in
-- its own transaction; it locks the order revision's live reservation row FOR SHARE.
--
-- References to TRD (orders), PTY (customers), EVD (evidence) and POL (credit_policy, currency,
-- fx_rate) are plain IDs, revisions and hashes: no foreign key crosses a schema (DB2).
-- action_result and audit_event are the command ledger of STL (internal/foundation/action).

CREATE SCHEMA brant_stl;

CREATE TABLE brant_stl.customer_credit (
    tenant_id     text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id      text        NOT NULL CHECK (party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    row_version   bigint      NOT NULL CHECK (row_version >= 0),
    limit_version bigint      NOT NULL CHECK (limit_version >= 0),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id),
    CHECK (updated_at >= created_at)
);

-- A root is created at version 0 by the command that first locks it, which moves it to 1 before it
-- commits; its identity and creation never change, its version moves by one and its count of limit
-- versions by at most one per command; it is never deleted.
CREATE FUNCTION brant_stl.guard_customer_credit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.customer_credit: credit roots are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.row_version <> 0 OR NEW.limit_version <> 0 THEN
            RAISE EXCEPTION 'brant_stl.customer_credit: a root starts at version 0' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.party_id, NEW.created_at) IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.party_id, OLD.created_at)
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.limit_version NOT IN (OLD.limit_version, OLD.limit_version + 1) THEN
        RAISE EXCEPTION 'brant_stl.customer_credit: identity and creation are fixed, the version moves by one and limit versions by at most one'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER customer_credit_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.customer_credit
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_customer_credit();

-- At commit: a root a command created has been moved to version 1 at least.
CREATE FUNCTION brant_stl.check_customer_credit_committed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM brant_stl.customer_credit c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.party_id = NEW.party_id AND c.row_version = 0) THEN
        RAISE EXCEPTION 'brant_stl.customer_credit: root % is committed at version 0', NEW.party_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER customer_credit_committed AFTER INSERT ON brant_stl.customer_credit
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_customer_credit_committed();

CREATE TABLE brant_stl.credit_limit_request (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    request_id        text          NOT NULL CHECK (request_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id          text          NOT NULL,
    status            text          NOT NULL CHECK (status IN ('requested', 'published', 'rejected')),
    version           bigint        NOT NULL CHECK (version BETWEEN 1 AND 3),
    currency          text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units       smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    currency_revision bigint        NOT NULL CHECK (currency_revision > 0),
    amount_minor      numeric(32,0) NOT NULL CHECK (amount_minor >= 0),
    internal_minor    numeric(32,0) CHECK (internal_minor >= 0),
    internal_evidence text[]        CHECK (cardinality(internal_evidence) BETWEEN 1 AND 20),
    sinosure_minor    numeric(32,0) CHECK (sinosure_minor >= 0),
    sinosure_evidence text[]        CHECK (cardinality(sinosure_evidence) BETWEEN 1 AND 20),
    valid_from        timestamptz   NOT NULL,
    valid_until       timestamptz   NOT NULL,
    policy_id         text          NOT NULL,
    policy_revision   bigint        NOT NULL CHECK (policy_revision > 0),
    policy_hash       text          NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    reason            text          NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    owner_actor_id    text          NOT NULL,
    owner_team_id     text,
    requested_by_kind text          NOT NULL CHECK (requested_by_kind IN ('human', 'ai')),
    requested_by_id   text          NOT NULL,
    request_epoch     bigint        NOT NULL CHECK (request_epoch > 0),
    requested_at      timestamptz   NOT NULL,
    decided_by_kind   text,
    decided_by_id     text,
    decide_epoch      bigint,
    decided_at        timestamptz,
    decision_reason   text,
    limit_version     bigint,
    PRIMARY KEY (tenant_id, company_id, request_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_stl.customer_credit (tenant_id, company_id, party_id),
    CHECK (internal_minor IS NOT NULL OR sinosure_minor IS NOT NULL),
    CHECK ((internal_minor IS NULL) = (internal_evidence IS NULL) AND (sinosure_minor IS NULL) = (sinosure_evidence IS NULL)),
    CHECK (amount_minor = least(coalesce(internal_minor, sinosure_minor), coalesce(sinosure_minor, internal_minor))),
    CHECK (valid_until > valid_from),
    CHECK (owner_actor_id = requested_by_id),
    CHECK (CASE WHEN status = 'requested'
        THEN decided_by_kind IS NULL AND decided_by_id IS NULL AND decide_epoch IS NULL AND decided_at IS NULL AND decision_reason IS NULL
             AND limit_version IS NULL AND version <= 2
        ELSE decided_by_kind = 'human' AND decided_by_id IS NOT NULL AND decide_epoch > 0 AND decided_at >= requested_at
             AND char_length(decision_reason) BETWEEN 1 AND 1000 AND version >= 2
             AND (status = 'published') = (limit_version IS NOT NULL) AND (limit_version IS NULL OR limit_version > 0) END)
);

CREATE INDEX credit_limit_request_party ON brant_stl.credit_limit_request (tenant_id, company_id, party_id);

-- A request is inserted requested at version 1; it changes only its state, version and decision,
-- the version by one, until it is published or rejected; then it never changes; never deleted.
CREATE FUNCTION brant_stl.guard_credit_limit_request() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision_reason',
        'limit_version'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.credit_limit_request: limit requests are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'requested' OR NEW.version <> 1 THEN
            RAISE EXCEPTION 'brant_stl.credit_limit_request: a request is inserted requested' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'requested' AND NEW.version = OLD.version + 1 AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.credit_limit_request: request % is %; it is decided once, along SM-STL-CREDIT-LIMIT', OLD.request_id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER credit_limit_request_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.credit_limit_request
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_credit_limit_request();

CREATE TABLE brant_stl.credit_limit_version (
    tenant_id       text          NOT NULL,
    company_id      text          NOT NULL,
    party_id        text          NOT NULL,
    version         bigint        NOT NULL CHECK (version > 0),
    request_id      text          NOT NULL,
    currency        text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units     smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor    numeric(32,0) NOT NULL CHECK (amount_minor >= 0),
    source          text          NOT NULL CHECK (source IN ('internal_assessment', 'sinosure_buyer_limit', 'lower_of_both')),
    valid_from      timestamptz   NOT NULL,
    valid_until     timestamptz   NOT NULL,
    policy_id       text          NOT NULL,
    policy_revision bigint        NOT NULL CHECK (policy_revision > 0),
    policy_hash     text          NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    published_by_id text          NOT NULL,
    publish_epoch   bigint        NOT NULL CHECK (publish_epoch > 0),
    published_at    timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id, version),
    UNIQUE (tenant_id, company_id, request_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_stl.customer_credit (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, request_id) REFERENCES brant_stl.credit_limit_request (tenant_id, company_id, request_id),
    CHECK (valid_until > valid_from)
);

-- A limit version is only inserted, as its request's publication: same customer, currency, amount,
-- source and interval; it never changes and is never deleted.
CREATE FUNCTION brant_stl.guard_credit_limit_version() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.credit_limit_version: limit versions never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_stl.credit_limit_request r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.request_id = NEW.request_id AND r.party_id = NEW.party_id AND r.currency = NEW.currency AND r.minor_units = NEW.minor_units
            AND r.amount_minor = NEW.amount_minor AND r.valid_from = NEW.valid_from AND r.valid_until = NEW.valid_until
            AND NEW.source = CASE WHEN r.internal_minor IS NOT NULL AND r.sinosure_minor IS NOT NULL THEN 'lower_of_both'
                WHEN r.internal_minor IS NOT NULL THEN 'internal_assessment' ELSE 'sinosure_buyer_limit' END) THEN
        RAISE EXCEPTION 'brant_stl.credit_limit_version: version % does not publish request %', NEW.version, NEW.request_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER credit_limit_version_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.credit_limit_version
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_credit_limit_version();

-- At commit: a published request has its limit version, a version its published request, and the
-- root counts its versions.
CREATE FUNCTION brant_stl.check_credit_limit_published() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM brant_stl.credit_limit_request r
            LEFT JOIN brant_stl.credit_limit_version v ON v.tenant_id = r.tenant_id AND v.company_id = r.company_id AND v.request_id = r.request_id
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.party_id = NEW.party_id
                AND (r.status = 'published') IS DISTINCT FROM (v.version IS NOT NULL AND v.version = r.limit_version))
        OR (SELECT count(*) FROM brant_stl.credit_limit_version v WHERE v.tenant_id = NEW.tenant_id AND v.company_id = NEW.company_id
                AND v.party_id = NEW.party_id)
            IS DISTINCT FROM (SELECT c.limit_version FROM brant_stl.customer_credit c WHERE c.tenant_id = NEW.tenant_id
                AND c.company_id = NEW.company_id AND c.party_id = NEW.party_id) THEN
        RAISE EXCEPTION 'brant_stl.credit_limit_version: the limit versions of % are not their published requests', NEW.party_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER credit_limit_request_published AFTER INSERT OR UPDATE ON brant_stl.credit_limit_request
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_credit_limit_published();
CREATE CONSTRAINT TRIGGER credit_limit_version_published AFTER INSERT ON brant_stl.credit_limit_version
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_credit_limit_published();

CREATE TABLE brant_stl.credit_decision (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    decision_id       text          NOT NULL CHECK (decision_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id          text          NOT NULL,
    order_id          text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision    bigint        NOT NULL CHECK (order_revision > 0),
    order_hash        text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    currency          text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units       smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    portion_minor     numeric(32,0) NOT NULL CHECK (portion_minor > 0),
    limit_currency    text          NOT NULL CHECK (limit_currency ~ '^[A-Z]{3}$'),
    limit_minor_units smallint      NOT NULL CHECK (limit_minor_units BETWEEN 0 AND 4),
    converted_minor   numeric(32,0) NOT NULL CHECK (converted_minor >= 0),
    fx_id             text,
    fx_revision       bigint        CHECK (fx_revision > 0),
    fx_hash           text          CHECK (fx_hash ~ '^[0-9a-f]{64}$'),
    limit_minor       numeric(32,0) NOT NULL CHECK (limit_minor >= 0),
    limit_version     bigint        NOT NULL CHECK (limit_version >= 0),
    exposure_minor    numeric(32,0) NOT NULL CHECK (exposure_minor >= 0),
    excess_minor      numeric(32,0) NOT NULL CHECK (excess_minor >= 0),
    cover_minor       numeric(32,0) NOT NULL CHECK (cover_minor > 0),
    overdue_days      integer       CHECK (overdue_days >= 0),
    reasons           text[]        NOT NULL CHECK (cardinality(reasons) BETWEEN 1 AND 2
                                        AND reasons <@ ARRAY['CREDIT_LIMIT_EXCEEDED', 'CREDIT_OVERDUE_EXCEEDED']),
    policy_id         text          NOT NULL,
    policy_revision   bigint        NOT NULL CHECK (policy_revision > 0),
    policy_hash       text          NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    status            text          NOT NULL CHECK (status IN ('open', 'released', 'rejected')),
    version           bigint        NOT NULL CHECK (version BETWEEN 1 AND 3),
    expires_at        timestamptz   NOT NULL,
    owner_actor_id    text          NOT NULL,
    owner_team_id     text,
    opened_by_kind    text          NOT NULL CHECK (opened_by_kind IN ('human', 'ai', 'system')),
    opened_by_id      text          NOT NULL,
    open_epoch        bigint        NOT NULL CHECK (open_epoch > 0),
    opened_at         timestamptz   NOT NULL,
    decided_by_kind   text,
    decided_by_id     text,
    decide_epoch      bigint,
    decided_at        timestamptz,
    decision_reason   text,
    PRIMARY KEY (tenant_id, company_id, decision_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_stl.customer_credit (tenant_id, company_id, party_id),
    CHECK ((fx_id IS NULL) = (fx_revision IS NULL) AND (fx_id IS NULL) = (fx_hash IS NULL)),
    CHECK ((fx_id IS NULL) = (currency = limit_currency)),
    CHECK (('CREDIT_LIMIT_EXCEEDED' = ANY (reasons)) = (excess_minor > 0)),
    CHECK (('CREDIT_OVERDUE_EXCEEDED' = ANY (reasons)) = (overdue_days IS NOT NULL)),
    CHECK (expires_at > opened_at),
    CHECK (owner_actor_id = opened_by_id),
    CHECK (CASE WHEN status = 'open'
        THEN decided_by_kind IS NULL AND decided_by_id IS NULL AND decide_epoch IS NULL AND decided_at IS NULL AND decision_reason IS NULL
             AND version <= 2
        ELSE decided_by_kind = 'human' AND decided_by_id IS NOT NULL AND decide_epoch > 0 AND decided_at >= opened_at
             AND char_length(decision_reason) BETWEEN 1 AND 1000 AND version >= 2 END)
);

CREATE INDEX credit_decision_order ON brant_stl.credit_decision (tenant_id, company_id, order_id, order_revision);
CREATE INDEX credit_decision_party ON brant_stl.credit_decision (tenant_id, company_id, party_id);

-- A decision is inserted open at version 1; it changes only its state, version and decision, the
-- version by one, until it is released or rejected; then it never changes; never deleted.
CREATE FUNCTION brant_stl.guard_credit_decision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'decided_by_kind', 'decided_by_id', 'decide_epoch', 'decided_at', 'decision_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.credit_decision: credit decisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' OR NEW.version <> 1 THEN
            RAISE EXCEPTION 'brant_stl.credit_decision: a decision is inserted open' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.version = OLD.version + 1 AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.credit_decision: decision % is %; it is decided once, along SM-STL-CREDIT-DECISION', OLD.decision_id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER credit_decision_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.credit_decision
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_credit_decision();

CREATE TABLE brant_stl.credit_approval (
    tenant_id      text          NOT NULL,
    company_id     text          NOT NULL,
    subject        text          NOT NULL CHECK (subject IN ('limit_request', 'decision')),
    subject_id     text          NOT NULL,
    approver_id    text          NOT NULL,
    approver_kind  text          NOT NULL CHECK (approver_kind = 'human'),
    tier_level     text          NOT NULL CHECK (tier_level ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    tier_max_minor numeric(32,0) NOT NULL CHECK (tier_max_minor > 0),
    approval_epoch bigint        NOT NULL CHECK (approval_epoch > 0),
    approved_at    timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, subject, subject_id, approver_id)
);

-- An approval is only appended, by a human, to a request or decision being decided by it (whose
-- version it moves in the same command); it never changes and is never deleted.
CREATE FUNCTION brant_stl.guard_credit_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_stl.credit_approval: approvals never change' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (CASE NEW.subject
            WHEN 'limit_request' THEN EXISTS (SELECT 1 FROM brant_stl.credit_limit_request r WHERE r.tenant_id = NEW.tenant_id
                AND r.company_id = NEW.company_id AND r.request_id = NEW.subject_id AND r.requested_by_id <> NEW.approver_id)
            ELSE EXISTS (SELECT 1 FROM brant_stl.credit_decision d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
                AND d.decision_id = NEW.subject_id AND d.opened_by_id <> NEW.approver_id) END) THEN
        RAISE EXCEPTION 'brant_stl.credit_approval: % % has no independent approval by %', NEW.subject, NEW.subject_id, NEW.approver_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER credit_approval_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.credit_approval
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_credit_approval();

CREATE TABLE brant_stl.credit_reservation (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    reservation_id    text          NOT NULL CHECK (reservation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id          text          NOT NULL,
    order_id          text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision    bigint        NOT NULL CHECK (order_revision > 0),
    order_hash        text          NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    currency          text          NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units       smallint      NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    amount_minor      numeric(32,0) NOT NULL CHECK (amount_minor >= 0),
    secured_minor     numeric(32,0) NOT NULL CHECK (secured_minor >= 0),
    limit_currency    text          NOT NULL CHECK (limit_currency ~ '^[A-Z]{3}$'),
    limit_minor_units smallint      NOT NULL CHECK (limit_minor_units BETWEEN 0 AND 4),
    exposure_minor    numeric(32,0) NOT NULL CHECK (exposure_minor >= 0),
    fx_id             text,
    fx_revision       bigint        CHECK (fx_revision > 0),
    fx_hash           text          CHECK (fx_hash ~ '^[0-9a-f]{64}$'),
    basis             text          NOT NULL CHECK (basis IN ('within_limit', 'credit_decision', 'documents_secured')),
    decision_id       text,
    policy_id         text          NOT NULL,
    policy_revision   bigint        NOT NULL CHECK (policy_revision > 0),
    policy_hash       text          NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    status            text          NOT NULL CHECK (status IN ('reserved', 'consumed', 'released')),
    version           bigint        NOT NULL CHECK (version BETWEEN 1 AND 3),
    expires_at        timestamptz   NOT NULL,
    owner_actor_id    text          NOT NULL,
    owner_team_id     text,
    reserved_by_kind  text          NOT NULL CHECK (reserved_by_kind IN ('human', 'ai', 'system')),
    reserved_by_id    text          NOT NULL,
    reserve_epoch     bigint        NOT NULL CHECK (reserve_epoch > 0),
    reserved_at       timestamptz   NOT NULL,
    consumed_by_kind  text,
    consumed_by_id    text,
    consume_epoch     bigint,
    consumed_at       timestamptz,
    activated_at      timestamptz,
    released_by_kind  text,
    released_by_id    text,
    release_epoch     bigint,
    released_at       timestamptz,
    release_reason    text,
    PRIMARY KEY (tenant_id, company_id, reservation_id),
    UNIQUE (tenant_id, company_id, decision_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_stl.customer_credit (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, decision_id) REFERENCES brant_stl.credit_decision (tenant_id, company_id, decision_id),
    CHECK ((fx_id IS NULL) = (fx_revision IS NULL) AND (fx_id IS NULL) = (fx_hash IS NULL)),
    CHECK (fx_id IS NULL OR currency <> limit_currency),
    CHECK ((basis = 'credit_decision') = (decision_id IS NOT NULL)),
    CHECK (CASE WHEN basis = 'documents_secured' THEN amount_minor = 0 AND secured_minor > 0 AND exposure_minor = 0 AND fx_id IS NULL
        ELSE amount_minor > 0 AND (fx_id IS NOT NULL OR (currency = limit_currency AND exposure_minor = amount_minor)) END),
    CHECK (expires_at > reserved_at),
    CHECK (owner_actor_id = reserved_by_id),
    CHECK ((consumed_at IS NULL) = (consumed_by_kind IS NULL) AND (consumed_at IS NULL) = (consumed_by_id IS NULL)
        AND (consumed_at IS NULL) = (consume_epoch IS NULL) AND (consumed_at IS NULL) = (activated_at IS NULL)),
    CHECK (consumed_by_kind IS NULL OR (consumed_by_kind = 'system' AND consume_epoch > 0 AND activated_at >= reserved_at
        AND activated_at < expires_at AND consumed_at >= activated_at)),
    CHECK ((released_at IS NULL) = (released_by_kind IS NULL) AND (released_at IS NULL) = (released_by_id IS NULL)
        AND (released_at IS NULL) = (release_epoch IS NULL) AND (released_at IS NULL) = (release_reason IS NULL)),
    CHECK (released_at IS NULL OR (release_epoch > 0 AND released_at >= reserved_at AND char_length(release_reason) BETWEEN 1 AND 1000)),
    CHECK (CASE status
        WHEN 'reserved' THEN version = 1 AND consumed_at IS NULL AND released_at IS NULL
        WHEN 'consumed' THEN version = 2 AND consumed_at IS NOT NULL AND released_at IS NULL
        ELSE released_at IS NOT NULL AND version = CASE WHEN consumed_at IS NULL THEN 2 ELSE 3 END
             AND (consumed_at IS NULL OR (released_by_kind = 'system' AND released_at >= consumed_at)) END)
);

-- One reservation of an order revision holds credit at a time (WF-08); an expired one is released
-- by the command that reserves the revision again.
CREATE UNIQUE INDEX credit_reservation_live ON brant_stl.credit_reservation (tenant_id, company_id, order_id, order_revision)
    WHERE status IN ('reserved', 'consumed');
CREATE INDEX credit_reservation_party ON brant_stl.credit_reservation (tenant_id, company_id, party_id);

-- A reservation is inserted reserved at version 1; reserved → consumed → released, or reserved →
-- released, the version by one, nothing else; never deleted.
CREATE FUNCTION brant_stl.guard_credit_reservation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'consumed_by_kind', 'consumed_by_id', 'consume_epoch', 'consumed_at', 'activated_at',
        'released_by_kind', 'released_by_id', 'release_epoch', 'released_at', 'release_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.credit_reservation: reservations are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'reserved' OR NEW.version <> 1 OR (NEW.decision_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_stl.credit_decision d
                WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.decision_id = NEW.decision_id AND d.status = 'released'
                    AND d.party_id = NEW.party_id AND d.order_id = NEW.order_id AND d.order_revision = NEW.order_revision
                    AND d.order_hash = NEW.order_hash AND d.portion_minor >= NEW.amount_minor AND d.expires_at > NEW.reserved_at)) THEN
            RAISE EXCEPTION 'brant_stl.credit_reservation: a reservation is inserted reserved, under a released decision of its binding if any'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.version = OLD.version + 1 AND ((OLD.status = 'reserved' AND NEW.status IN ('consumed', 'released'))
            OR (OLD.status = 'consumed' AND NEW.status = 'released'))
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving)
            AND (OLD.consumed_at IS NULL OR (NEW.consumed_by_kind, NEW.consumed_by_id, NEW.consume_epoch, NEW.consumed_at, NEW.activated_at)
                IS NOT DISTINCT FROM (OLD.consumed_by_kind, OLD.consumed_by_id, OLD.consume_epoch, OLD.consumed_at, OLD.activated_at)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.credit_reservation: reservation % is %; it moves along SM-STL-CREDIT-RESERVATION only', OLD.reservation_id, OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER credit_reservation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.credit_reservation
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_credit_reservation();

-- CreditGuard v1 (R57): the order revision's live reservation, locked FOR SHARE in the caller's
-- transaction; always one row in the caller's scope. See internal/contexts/stl/api/guard.go.
CREATE FUNCTION brant_stl.guard_credit_reservation_v1(p_order_id text, p_order_revision bigint, p_portion_minor numeric, p_currency text,
        p_at timestamptz)
    RETURNS TABLE (status text, reservation_id text, party_id text, order_hash text, reservation_status text, currency text,
                   reserved_minor numeric, secured_minor numeric, basis text, decision_id text, expires_at timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_r       record;
    v_status  text;
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_portion_minor IS NULL OR p_currency IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_stl.guard_credit_reservation_v1: order_id, order_revision, portion_minor, currency and at are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_portion_minor < 0 OR p_portion_minor <> pg_catalog.trunc(p_portion_minor) THEN
        RAISE EXCEPTION 'brant_stl.guard_credit_reservation_v1: the portion is a whole number of minor units, not negative'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT r.reservation_id, r.party_id, r.order_hash, r.status, r.currency, r.amount_minor, r.secured_minor, r.basis, r.decision_id,
        r.expires_at INTO v_r
    FROM brant_stl.credit_reservation r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.order_id = p_order_id AND r.order_revision = p_order_revision
        AND r.status IN ('reserved', 'consumed')
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'missing'::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::text, NULL::numeric, NULL::numeric,
            NULL::text, NULL::text, NULL::timestamptz, 'CREDIT_RESERVATION_REQUIRED'::text;
        RETURN;
    END IF;
    v_status := CASE
        WHEN v_r.status = 'reserved' AND v_r.expires_at <= p_at THEN 'expired'
        WHEN v_r.currency <> p_currency OR v_r.amount_minor < p_portion_minor THEN 'insufficient'
        WHEN v_r.basis = 'documents_secured' THEN 'documents_secured'
        ELSE 'covered' END;
    RETURN QUERY SELECT v_status, v_r.reservation_id, v_r.party_id, v_r.order_hash, v_r.status, v_r.currency, v_r.amount_minor,
        v_r.secured_minor, v_r.basis, v_r.decision_id, v_r.expires_at,
        CASE WHEN v_status IN ('covered', 'documents_secured') THEN NULL ELSE 'CREDIT_RESERVATION_REQUIRED' END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_stl.guard_credit_reservation_v1(text, bigint, numeric, text, timestamptz) FROM PUBLIC;

CREATE TABLE brant_stl.action_result (
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

CREATE TABLE brant_stl.audit_event (
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

ALTER TABLE brant_stl.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_stl.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_stl.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.customer_credit ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.customer_credit FORCE ROW LEVEL SECURITY;
CREATE POLICY customer_credit_scope ON brant_stl.customer_credit
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.credit_limit_request ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.credit_limit_request FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_limit_request_scope ON brant_stl.credit_limit_request
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.credit_limit_version ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.credit_limit_version FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_limit_version_scope ON brant_stl.credit_limit_version
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.credit_decision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.credit_decision FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_decision_scope ON brant_stl.credit_decision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.credit_approval ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.credit_approval FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_approval_scope ON brant_stl.credit_approval
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.credit_reservation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.credit_reservation FORCE ROW LEVEL SECURITY;
CREATE POLICY credit_reservation_scope ON brant_stl.credit_reservation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
