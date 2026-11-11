-- TRD: customer responses, verified acceptances and their effectivity ledger (WBS S3-06; the
-- contract-formation half of S3-13; tactical model TRD-AGG-CustomerResponse, TRD-AGG-Acceptance,
-- SM-TRD-ACCEPTANCE; TRD-INV-011, 012, 017; ADR-BR-013; Python t04_customer_response,
-- t04_response_verification and t03_acceptance).
--
-- A customer response is the customer's reply to an active formal revision as an operator or an AI
-- agent recorded it: bound to the revision, its business hash, its formal document's number and its
-- validity end, classified (full_unconditional, conditional, partial, reject, ambiguous), located in
-- the customer's message (an EVD source and the place in it), with the time the message arrived and
-- the basis of that time. Responses are only appended. A human verifies a full_unconditional
-- response into an acceptance in one transaction: the acceptance binds the revision, its hash and
-- number, the customer, the contact and the complete line set, and records the contract's
-- formation — the arrival time (the response's verifiable arrival, else the verification time), its
-- basis and the expires_at it was judged against; it is stored only when the acceptance arrived
-- before expires_at (ADR-BR-013: a late acceptance forms no contract and is refused). Its
-- effectivity is valid at version 1 and moves once, to invalidated or disputed at version 2, with
-- an entry in the effectivity ledger; it never returns to valid. One valid acceptance per revision;
-- one live (valid or disputed) acceptance per response and per customer message.
--
-- Every write locks the quote root FOR UPDATE first (the response, its acceptances and their
-- effectivity live under it), and acceptance.invalidate and acceptance.mark_disputed then lock the
-- acceptance row FOR UPDATE. brant_trd.guard_acceptance_valid_v1 publishes, for callers inside
-- their own transaction, whether an acceptance is valid and formed for a revision, holding the root
-- and the acceptance FOR SHARE until that transaction ends.

CREATE TABLE brant_trd.customer_response (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    response_id        text        NOT NULL CHECK (response_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quote_id           text        NOT NULL,
    revision           bigint      NOT NULL,
    business_hash      text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    number             text        NOT NULL,
    expires_at         timestamptz NOT NULL,
    classification     text        NOT NULL CHECK (classification IN ('full_unconditional', 'conditional', 'partial', 'reject',
                                                                      'ambiguous')),
    summary            text        NOT NULL CHECK (char_length(summary) BETWEEN 1 AND 4000),
    source_id          text        NOT NULL CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    locator            text        NOT NULL CHECK (char_length(locator) BETWEEN 1 AND 500),
    evidence           text        NOT NULL CHECK (left(evidence, 1) = '['),
    received_at        timestamptz,
    received_at_basis  text        CHECK (received_at_basis IN ('channel_server', 'import_record', 'source_claimed')),
    customer_po_number text        CHECK (char_length(customer_po_number) BETWEEN 1 AND 64),
    recorded_by_kind   text        NOT NULL CHECK (recorded_by_kind IN ('human', 'ai')),
    recorded_by_id     text        NOT NULL,
    record_epoch       bigint      NOT NULL CHECK (record_epoch > 0),
    recorded_at        timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, response_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_document (tenant_id, company_id, quote_id, revision),
    CHECK ((received_at IS NULL) = (received_at_basis IS NULL)),
    -- A verifiable arrival precedes the recording (ACCEPTANCE_TIME_CONFLICT).
    CHECK (coalesce(received_at_basis, '') NOT IN ('channel_server', 'import_record') OR received_at <= recorded_at)
);

CREATE INDEX customer_response_quote ON brant_trd.customer_response (tenant_id, company_id, quote_id, revision);

CREATE TABLE brant_trd.acceptance (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    acceptance_id       text        NOT NULL CHECK (acceptance_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    response_id         text        NOT NULL,
    quote_id            text        NOT NULL,
    revision            bigint      NOT NULL,
    business_hash       text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    number              text        NOT NULL,
    customer_party_id   text        NOT NULL,
    customer_revision   bigint      NOT NULL CHECK (customer_revision > 0),
    contact_id          text        NOT NULL,
    contact_row_version bigint      NOT NULL CHECK (contact_row_version > 0),
    line_ids            text[]      NOT NULL CHECK (cardinality(line_ids) BETWEEN 1 AND 200),
    message_source_id   text        NOT NULL,
    message_locator     text        NOT NULL,
    arrival_at          timestamptz NOT NULL,
    arrival_basis       text        NOT NULL CHECK (arrival_basis IN ('channel_server', 'import_record', 'verification')),
    expires_at          timestamptz NOT NULL,
    verified_by_kind    text        NOT NULL CHECK (verified_by_kind = 'human'),
    verified_by_id      text        NOT NULL,
    verify_epoch        bigint      NOT NULL CHECK (verify_epoch > 0),
    verified_at         timestamptz NOT NULL,
    effectivity         text        NOT NULL CHECK (effectivity IN ('valid', 'invalidated', 'disputed')),
    effectivity_version bigint      NOT NULL CHECK (effectivity_version > 0),
    PRIMARY KEY (tenant_id, company_id, acceptance_id),
    FOREIGN KEY (tenant_id, company_id, response_id) REFERENCES brant_trd.customer_response (tenant_id, company_id, response_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_document (tenant_id, company_id, quote_id, revision),
    -- The contract formed: the acceptance arrived before the revision's validity ended, no later than
    -- its verification (ADR-BR-013, TRD-INV-012).
    CHECK (arrival_at < expires_at),
    CHECK (arrival_at <= verified_at),
    CHECK (arrival_basis <> 'verification' OR arrival_at = verified_at),
    -- Valid at version 1; invalidated or disputed at version 2, for good.
    CHECK ((effectivity = 'valid') = (effectivity_version = 1)),
    CHECK (effectivity_version <= 2)
);

-- One valid acceptance per revision; one live (valid or disputed) acceptance per response and per
-- customer message (TRD-INV-011: 同消息不重复).
CREATE UNIQUE INDEX acceptance_valid_revision ON brant_trd.acceptance (tenant_id, company_id, quote_id, revision)
    WHERE effectivity = 'valid';
CREATE UNIQUE INDEX acceptance_live_response ON brant_trd.acceptance (tenant_id, company_id, response_id)
    WHERE effectivity IN ('valid', 'disputed');
CREATE UNIQUE INDEX acceptance_live_message ON brant_trd.acceptance (tenant_id, company_id, message_source_id, md5(message_locator))
    WHERE effectivity IN ('valid', 'disputed');
CREATE INDEX acceptance_quote ON brant_trd.acceptance (tenant_id, company_id, quote_id, revision);

CREATE TABLE brant_trd.acceptance_effectivity (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    acceptance_id       text        NOT NULL,
    effectivity_version bigint      NOT NULL CHECK (effectivity_version BETWEEN 1 AND 2),
    effectivity         text        NOT NULL CHECK (effectivity IN ('valid', 'invalidated', 'disputed')),
    changed_by_kind     text        NOT NULL CHECK (changed_by_kind = 'human'),
    changed_by_id       text        NOT NULL,
    change_epoch        bigint      NOT NULL CHECK (change_epoch > 0),
    changed_at          timestamptz NOT NULL,
    reason              text        CHECK (char_length(reason) BETWEEN 1 AND 2000),
    evidence            text        CHECK (left(evidence, 1) = '['),
    PRIMARY KEY (tenant_id, company_id, acceptance_id, effectivity_version),
    FOREIGN KEY (tenant_id, company_id, acceptance_id) REFERENCES brant_trd.acceptance (tenant_id, company_id, acceptance_id),
    -- Version 1 is the verification (valid, no reason or evidence); a later entry invalidates or
    -- disputes with a reason and new evidence.
    CHECK ((effectivity_version = 1) = (effectivity = 'valid')),
    CHECK ((effectivity_version = 1) = (reason IS NULL) AND (effectivity_version = 1) = (evidence IS NULL))
);

-- A response is recorded for the active formal revision at its business hash, document number and
-- validity end while no risk hold pauses it; a verifiable arrival is not before the revision's
-- formalization. It never changes and is never deleted.
CREATE FUNCTION brant_trd.guard_customer_response() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.customer_response: a customer response never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r
            JOIN brant_trd.quote_document d ON d.tenant_id = r.tenant_id AND d.company_id = r.company_id AND d.quote_id = r.quote_id
                AND d.revision = r.revision
            JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.quote_id = r.quote_id
                AND e.revision = r.revision
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'active' AND r.business_hash = NEW.business_hash AND r.expires_at = NEW.expires_at AND d.number = NEW.number
                AND (coalesce(NEW.received_at_basis, '') NOT IN ('channel_server', 'import_record') OR NEW.received_at >= e.committed_at))
            OR EXISTS (SELECT 1 FROM brant_trd.quote_risk_hold h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
                AND h.quote_id = NEW.quote_id AND h.revision = NEW.revision) THEN
        RAISE EXCEPTION 'brant_trd.customer_response: a response answers the active, unheld formal revision at its hash and number'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER customer_response_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.customer_response
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_customer_response();

-- An acceptance is created valid at version 1 from a full_unconditional response, binding the
-- response's revision, hash, number, validity end and message, while the revision is active and
-- unheld; its arrival is the response's verifiable arrival, or else its verification. It changes
-- once: valid → invalidated or disputed at the next version, with that ledger entry written first;
-- nothing else of it changes and it is never deleted.
CREATE FUNCTION brant_trd.guard_acceptance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.acceptance: acceptances are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.effectivity <> 'valid' OR NOT EXISTS (SELECT 1 FROM brant_trd.customer_response p
                JOIN brant_trd.quote_revision r ON r.tenant_id = p.tenant_id AND r.company_id = p.company_id AND r.quote_id = p.quote_id
                    AND r.revision = p.revision
                WHERE p.tenant_id = NEW.tenant_id AND p.company_id = NEW.company_id AND p.response_id = NEW.response_id
                    AND p.classification = 'full_unconditional' AND p.quote_id = NEW.quote_id AND p.revision = NEW.revision
                    AND p.business_hash = NEW.business_hash AND p.number = NEW.number AND p.expires_at = NEW.expires_at
                    AND p.source_id = NEW.message_source_id AND p.locator = NEW.message_locator
                    AND r.status = 'active' AND r.business_hash = NEW.business_hash
                    AND CASE WHEN coalesce(p.received_at_basis, '') IN ('channel_server', 'import_record')
                        THEN NEW.arrival_at = p.received_at AND NEW.arrival_basis = p.received_at_basis
                        ELSE NEW.arrival_basis = 'verification' END)
                OR EXISTS (SELECT 1 FROM brant_trd.quote_risk_hold h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
                    AND h.quote_id = NEW.quote_id AND h.revision = NEW.revision) THEN
            RAISE EXCEPTION 'brant_trd.acceptance: an acceptance is created valid from a full_unconditional response of the active, unheld revision'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.effectivity <> 'valid' OR NEW.effectivity NOT IN ('invalidated', 'disputed')
            OR NEW.effectivity_version <> OLD.effectivity_version + 1
            OR (to_jsonb(OLD) - 'effectivity' - 'effectivity_version') IS DISTINCT FROM (to_jsonb(NEW) - 'effectivity' - 'effectivity_version')
            OR NOT EXISTS (SELECT 1 FROM brant_trd.acceptance_effectivity f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
                AND f.acceptance_id = NEW.acceptance_id AND f.effectivity_version = NEW.effectivity_version
                AND f.effectivity = NEW.effectivity) THEN
        RAISE EXCEPTION 'brant_trd.acceptance: acceptance % is %; it is invalidated or disputed once, with its ledger entry, and nothing else changes',
            OLD.acceptance_id, OLD.effectivity USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER acceptance_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.acceptance
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_acceptance();

-- The effectivity ledger only grows: version 1 records the verification of a valid acceptance;
-- version 2 records its invalidation or dispute while it is still valid at version 1.
CREATE FUNCTION brant_trd.guard_acceptance_effectivity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.acceptance_effectivity: the effectivity ledger never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.acceptance a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
            AND a.acceptance_id = NEW.acceptance_id AND a.effectivity = 'valid' AND a.effectivity_version = 1
            AND (NEW.effectivity_version = 2 OR (a.verified_by_id = NEW.changed_by_id AND a.verify_epoch = NEW.change_epoch
                AND a.verified_at = NEW.changed_at))) THEN
        RAISE EXCEPTION 'brant_trd.acceptance_effectivity: an entry records the verification or the one change of a valid acceptance'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER acceptance_effectivity_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.acceptance_effectivity
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_acceptance_effectivity();

-- An acceptance commits with its ledger: the verification at version 1 and the entry of its
-- current effectivity (checked when the transaction commits).
CREATE FUNCTION brant_trd.check_acceptance_ledger() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_trd.acceptance_effectivity f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
                AND f.acceptance_id = NEW.acceptance_id AND f.effectivity_version = 1)
            OR NOT EXISTS (SELECT 1 FROM brant_trd.acceptance_effectivity f WHERE f.tenant_id = NEW.tenant_id AND f.company_id = NEW.company_id
                AND f.acceptance_id = NEW.acceptance_id AND f.effectivity_version = NEW.effectivity_version AND f.effectivity = NEW.effectivity) THEN
        RAISE EXCEPTION 'brant_trd.acceptance: acceptance % commits with its effectivity ledger', NEW.acceptance_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER acceptance_ledger AFTER INSERT OR UPDATE ON brant_trd.acceptance DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION brant_trd.check_acceptance_ledger();

ALTER TABLE brant_trd.customer_response ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.customer_response FORCE ROW LEVEL SECURITY;
CREATE POLICY customer_response_scope ON brant_trd.customer_response
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.acceptance ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.acceptance FORCE ROW LEVEL SECURITY;
CREATE POLICY acceptance_scope ON brant_trd.acceptance
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.acceptance_effectivity ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.acceptance_effectivity FORCE ROW LEVEL SECURITY;
CREATE POLICY acceptance_effectivity_scope ON brant_trd.acceptance_effectivity
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- AcceptanceGuard v1: whether an acceptance is valid and formed for a revision, for a caller inside
-- its own transaction (ADR-BR-004 option B; ADR-BR-011: the caller locks its own root first). It
-- locks the quote root p_quote_id FOR SHARE, as QuoteRevisionGuard v1 does, then the acceptance
-- row FOR SHARE, so acceptance.invalidate and acceptance.mark_disputed (which lock the root and then
-- the acceptance FOR UPDATE) either committed before and the verdict says so, or wait until the
-- caller's transaction ends; in READ COMMITTED a guard that waited reads the row as they left it.
--
-- It always returns one row in the caller's scope (app.tenant_id, app.company_id). known is false,
-- with code REFERENCE_INVALID, for an acceptance that does not exist there. Otherwise the columns are
-- the acceptance's, formed says whether it arrived before expires_at, and code is NULL when it is
-- valid and formed for p_quote_id, p_revision and p_business_hash, ACCEPTANCE_SCOPE_MISMATCH when it
-- accepts another quote, revision or hash, ACCEPTANCE_NOT_VALID while invalidated or disputed, and
-- ACCEPTANCE_LATE for one that did not form a contract (the table stores none). No clock is compared
-- with expires_at: a formed contract stays formed after the quote expires (TRD-INV-017). The data
-- scope (own, team) is the caller's to check before its transaction (TRD's lookups). SECURITY
-- DEFINER, fixed search_path, qualified names; brant_app alone may execute it.
CREATE FUNCTION brant_trd.guard_acceptance_valid_v1(p_acceptance_id text, p_quote_id text, p_revision bigint, p_business_hash text)
    RETURNS TABLE (known boolean, quote_id text, revision bigint, business_hash text, response_id text, effectivity text,
                   effectivity_version bigint, arrival_at timestamptz, arrival_basis text, expires_at timestamptz, formed boolean,
                   code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_acc     record;
BEGIN
    IF p_acceptance_id IS NULL OR p_quote_id IS NULL OR p_revision IS NULL OR p_business_hash IS NULL THEN
        RAISE EXCEPTION 'brant_trd.guard_acceptance_valid_v1: acceptance_id, quote_id, revision and business_hash are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    PERFORM 1 FROM brant_trd.quote_root q
    WHERE q.tenant_id = v_tenant AND q.company_id = v_company AND q.quote_id = p_quote_id
    FOR SHARE;
    SELECT a.quote_id, a.revision, a.business_hash, a.response_id, a.effectivity, a.effectivity_version, a.arrival_at, a.arrival_basis,
        a.expires_at INTO v_acc
    FROM brant_trd.acceptance a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.acceptance_id = p_acceptance_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::text, NULL::bigint, NULL::timestamptz,
            NULL::text, NULL::timestamptz, false, 'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, v_acc.quote_id, v_acc.revision, v_acc.business_hash, v_acc.response_id, v_acc.effectivity,
        v_acc.effectivity_version, v_acc.arrival_at, v_acc.arrival_basis, v_acc.expires_at, v_acc.arrival_at < v_acc.expires_at,
        CASE
            WHEN v_acc.quote_id <> p_quote_id OR v_acc.revision <> p_revision OR v_acc.business_hash <> p_business_hash
                THEN 'ACCEPTANCE_SCOPE_MISMATCH'
            WHEN v_acc.effectivity <> 'valid' THEN 'ACCEPTANCE_NOT_VALID'
            WHEN v_acc.arrival_at >= v_acc.expires_at THEN 'ACCEPTANCE_LATE'
        END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_trd.guard_acceptance_valid_v1(text, text, bigint, text) FROM PUBLIC;
