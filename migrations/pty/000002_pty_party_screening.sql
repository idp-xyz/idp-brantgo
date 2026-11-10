-- PTY: party screening (PTY-AGG-PartyScreening, PTY-INV-002; WBS S1-09) and PartyScreeningGuard v1
-- (Context Map R44 PTY->FUL, R13 PTY->TRD; ADR-BR-004 option B, ADR-BR-011 lock order).
--
-- party_screening is the aggregate root of one party's screening: the current conclusion and its
-- status, the negative-list flag and the row version every command compares and increments.
-- Conclusions (screening_result with the list versions used and the hits), decisions, positive-list
-- entries and negative-list entries are appended and never change: triggers refuse UPDATE and
-- DELETE and brant_app holds neither. A positive-list entry is bound to the party revision and to
-- the content hash of the list entry; it stops applying when either changes. The negative list is
-- never lifted: the root's flag cannot be cleared.
--
-- Whether the current conclusion still holds is derived when it is read, never written by another
-- command: brant_pty.party_screening_state compares it with the party's confirmed revision, its
-- validity, and the current version of every list it used (through reference data's guard
-- function brant_ref.screening_list_current_v1). brant_pty.party_screening_guard_v1 is that
-- derivation with FOR SHARE locks, for downstream transactions.

CREATE TABLE brant_pty.screening_result (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    screening_id        text        NOT NULL CHECK (screening_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id            text        NOT NULL,
    party_revision      bigint      NOT NULL CHECK (party_revision > 0),
    screened_name       text        NOT NULL,
    regulation_id       text        NOT NULL,
    regulation_revision bigint      NOT NULL CHECK (regulation_revision > 0),
    regulation_hash     text        NOT NULL CHECK (regulation_hash ~ '^[0-9a-f]{64}$'),
    outcome             text        NOT NULL CHECK (outcome IN ('clear', 'match', 'pending')),
    status              text        NOT NULL CHECK (status IN ('clear', 'match', 'pending', 'blocked')),
    missing_lists       text[]      NOT NULL,
    normalization       text        NOT NULL,
    screened_by_kind    text        NOT NULL CHECK (screened_by_kind IN ('human', 'ai')),
    screened_by_id      text        NOT NULL,
    authority_epoch     bigint      NOT NULL CHECK (authority_epoch > 0),
    screened_at         timestamptz NOT NULL,
    valid_until         timestamptz NOT NULL CHECK (valid_until > screened_at),
    PRIMARY KEY (tenant_id, company_id, screening_id),
    UNIQUE (tenant_id, company_id, party_id, screening_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, party_id, party_revision)
        REFERENCES brant_pty.party_revision (tenant_id, company_id, party_id, revision)
);

CREATE TABLE brant_pty.screening_list_used (
    tenant_id    text NOT NULL,
    company_id   text NOT NULL,
    screening_id text NOT NULL,
    list_code    text NOT NULL,
    jurisdiction text NOT NULL CHECK (jurisdiction IN ('CN', 'UN', 'foreign')),
    version      text NOT NULL,
    content_hash text NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (tenant_id, company_id, screening_id, list_code),
    FOREIGN KEY (tenant_id, company_id, screening_id) REFERENCES brant_pty.screening_result (tenant_id, company_id, screening_id)
);

CREATE TABLE brant_pty.screening_hit (
    tenant_id          text NOT NULL,
    company_id         text NOT NULL,
    screening_id       text NOT NULL,
    list_code          text NOT NULL,
    entry_id           text NOT NULL,
    version            text NOT NULL,
    entry_content_hash text NOT NULL CHECK (entry_content_hash ~ '^[0-9a-f]{64}$'),
    primary_name       text NOT NULL,
    matched_name       text NOT NULL,
    name_key           text NOT NULL,
    disposition        text NOT NULL CHECK (disposition IN ('suspected', 'positive_list', 'confirmed_match')),
    positive_id        text,
    PRIMARY KEY (tenant_id, company_id, screening_id, list_code, entry_id),
    FOREIGN KEY (tenant_id, company_id, screening_id, list_code)
        REFERENCES brant_pty.screening_list_used (tenant_id, company_id, screening_id, list_code),
    CHECK ((disposition = 'positive_list') = (positive_id IS NOT NULL))
);

CREATE TABLE brant_pty.screening_decision (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    screening_id    text        NOT NULL,
    party_id        text        NOT NULL,
    decision        text        NOT NULL CHECK (decision IN ('false_positive', 'true_match')),
    reason          text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    decided_by_kind text        NOT NULL CHECK (decided_by_kind = 'human'),
    decided_by_id   text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    decided_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, screening_id),
    FOREIGN KEY (tenant_id, company_id, party_id, screening_id)
        REFERENCES brant_pty.screening_result (tenant_id, company_id, party_id, screening_id)
);

CREATE TABLE brant_pty.positive_list_entry (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    positive_id        text        NOT NULL CHECK (positive_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id           text        NOT NULL,
    party_revision     bigint      NOT NULL CHECK (party_revision > 0),
    list_code          text        NOT NULL,
    entry_id           text        NOT NULL,
    entry_content_hash text        NOT NULL CHECK (entry_content_hash ~ '^[0-9a-f]{64}$'),
    screening_id       text        NOT NULL,
    reason             text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    registered_by_kind text        NOT NULL CHECK (registered_by_kind = 'human'),
    registered_by_id   text        NOT NULL,
    authority_epoch    bigint      NOT NULL CHECK (authority_epoch > 0),
    registered_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, positive_id),
    FOREIGN KEY (tenant_id, company_id, screening_id, list_code, entry_id)
        REFERENCES brant_pty.screening_hit (tenant_id, company_id, screening_id, list_code, entry_id),
    FOREIGN KEY (tenant_id, company_id, party_id, screening_id)
        REFERENCES brant_pty.screening_result (tenant_id, company_id, party_id, screening_id)
);
CREATE INDEX positive_list_entry_party ON brant_pty.positive_list_entry (tenant_id, company_id, party_id);

CREATE TABLE brant_pty.negative_list_entry (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    party_id        text        NOT NULL,
    reason          text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    blocked_by_kind text        NOT NULL CHECK (blocked_by_kind = 'human'),
    blocked_by_id   text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    blocked_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id)
);

CREATE TABLE brant_pty.party_screening (
    tenant_id            text        NOT NULL,
    company_id           text        NOT NULL,
    party_id             text        NOT NULL,
    row_version          bigint      NOT NULL CHECK (row_version > 0),
    current_screening_id text,
    status               text        CHECK (status IN ('clear', 'match', 'pending', 'blocked')),
    negative             boolean     NOT NULL,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, party_id) REFERENCES brant_pty.party (tenant_id, company_id, party_id),
    FOREIGN KEY (tenant_id, company_id, party_id, current_screening_id)
        REFERENCES brant_pty.screening_result (tenant_id, company_id, party_id, screening_id),
    CHECK ((current_screening_id IS NULL) = (status IS NULL))
);

-- The root keeps its party, counts every command, and never leaves the negative list.
CREATE FUNCTION brant_pty.guard_screening_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pty.party_screening: a screening is never deleted (PTY-INV-002)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.party_id, NEW.created_at) IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.party_id, OLD.created_at)
       OR NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_pty.party_screening: a screening keeps its party and counts its commands (PTY-INV-002)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.negative AND NOT NEW.negative THEN
        RAISE EXCEPTION 'brant_pty.party_screening: the negative list is never lifted (PTY-INV-002)'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER party_screening_guard BEFORE UPDATE OR DELETE ON brant_pty.party_screening
    FOR EACH ROW EXECUTE FUNCTION brant_pty.guard_screening_root();

CREATE FUNCTION brant_pty.refuse_screening_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_pty.%: screening records are appended, never changed (PTY-INV-002)', TG_TABLE_NAME
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER screening_result_immutable BEFORE UPDATE OR DELETE ON brant_pty.screening_result
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_screening_change();
CREATE TRIGGER screening_list_used_immutable BEFORE UPDATE OR DELETE ON brant_pty.screening_list_used
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_screening_change();
CREATE TRIGGER screening_hit_immutable BEFORE UPDATE OR DELETE ON brant_pty.screening_hit
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_screening_change();
CREATE TRIGGER screening_decision_immutable BEFORE UPDATE OR DELETE ON brant_pty.screening_decision
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_screening_change();
CREATE TRIGGER positive_list_entry_immutable BEFORE UPDATE OR DELETE ON brant_pty.positive_list_entry
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_screening_change();
CREATE TRIGGER negative_list_entry_immutable BEFORE UPDATE OR DELETE ON brant_pty.negative_list_entry
    FOR EACH ROW EXECUTE FUNCTION brant_pty.refuse_screening_change();

ALTER TABLE brant_pty.party_screening ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.party_screening FORCE ROW LEVEL SECURITY;
CREATE POLICY party_screening_scope ON brant_pty.party_screening
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.screening_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.screening_result FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_result_scope ON brant_pty.screening_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.screening_list_used ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.screening_list_used FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_list_used_scope ON brant_pty.screening_list_used
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.screening_hit ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.screening_hit FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_hit_scope ON brant_pty.screening_hit
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.screening_decision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.screening_decision FORCE ROW LEVEL SECURITY;
CREATE POLICY screening_decision_scope ON brant_pty.screening_decision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.positive_list_entry ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.positive_list_entry FORCE ROW LEVEL SECURITY;
CREATE POLICY positive_list_entry_scope ON brant_pty.positive_list_entry
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pty.negative_list_entry ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pty.negative_list_entry FORCE ROW LEVEL SECURITY;
CREATE POLICY negative_list_entry_scope ON brant_pty.negative_list_entry
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- The screening status of a party at p_at, in the caller's scope; with p_lock the rows it read stay
-- locked FOR SHARE until the caller's transaction ends. Lock order: the screening root, the party,
-- then the lists through reference data's guard, in list-code order. The checks run from the most
-- to the least lasting: the negative list, no conclusion, the party revision, validity, the list
-- versions, then the conclusion's own status.
CREATE FUNCTION brant_pty.party_screening_state(p_party_id text, p_at timestamptz, p_lock boolean)
    RETURNS TABLE (clear boolean, status text, reason text, screening_id text, party_revision bigint)
    LANGUAGE plpgsql VOLATILE SECURITY INVOKER SET search_path = pg_catalog, pg_temp
AS $$
#variable_conflict use_column
DECLARE
    v_tenant    text := current_setting('app.tenant_id', true);
    v_company   text := current_setting('app.company_id', true);
    v_root      record;
    v_has_root  boolean;
    v_party     record;
    v_has_party boolean;
    v_result    record;
    v_list      record;
    v_current   text;
BEGIN
    IF p_at IS NULL OR p_party_id IS NULL OR p_lock IS NULL THEN
        RAISE EXCEPTION 'brant_pty.party_screening_state: party, instant and lock mode are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_lock THEN
        SELECT s.current_screening_id, s.status, s.negative INTO v_root FROM brant_pty.party_screening s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.party_id = p_party_id FOR SHARE;
        v_has_root := FOUND;
        SELECT p.status, p.revision INTO v_party FROM brant_pty.party p
        WHERE p.tenant_id = v_tenant AND p.company_id = v_company AND p.party_id = p_party_id FOR SHARE;
        v_has_party := FOUND;
    ELSE
        SELECT s.current_screening_id, s.status, s.negative INTO v_root FROM brant_pty.party_screening s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.party_id = p_party_id;
        v_has_root := FOUND;
        SELECT p.status, p.revision INTO v_party FROM brant_pty.party p
        WHERE p.tenant_id = v_tenant AND p.company_id = v_company AND p.party_id = p_party_id;
        v_has_party := FOUND;
    END IF;
    IF NOT v_has_party THEN
        RETURN QUERY SELECT false, 'pending'::text, 'unknown_party'::text, ''::text, 0::bigint;
        RETURN;
    END IF;
    IF v_has_root AND v_root.negative THEN
        RETURN QUERY SELECT false, 'blocked'::text, 'negative_list'::text, coalesce(v_root.current_screening_id, ''), 0::bigint;
        RETURN;
    END IF;
    IF NOT v_has_root OR v_root.current_screening_id IS NULL THEN
        RETURN QUERY SELECT false, 'pending'::text, 'not_screened'::text, ''::text, 0::bigint;
        RETURN;
    END IF;
    SELECT r.party_revision, r.valid_until, r.missing_lists INTO v_result FROM brant_pty.screening_result r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.screening_id = v_root.current_screening_id;
    IF v_party.status <> 'confirmed' OR v_party.revision <> v_result.party_revision THEN
        RETURN QUERY SELECT false, 'pending'::text, 'party_changed'::text, v_root.current_screening_id, v_result.party_revision;
        RETURN;
    END IF;
    IF v_result.valid_until <= p_at THEN
        RETURN QUERY SELECT false, 'expired'::text, 'expired'::text, v_root.current_screening_id, v_result.party_revision;
        RETURN;
    END IF;
    FOR v_list IN
        SELECT u.list_code AS code, u.version AS version FROM brant_pty.screening_list_used u
        WHERE u.tenant_id = v_tenant AND u.company_id = v_company AND u.screening_id = v_root.current_screening_id
        UNION ALL
        SELECT m.code, NULL::text FROM unnest(v_result.missing_lists) AS m(code)
        ORDER BY 1
    LOOP
        v_current := brant_ref.screening_list_current_v1(v_list.code, p_lock);
        IF v_current IS DISTINCT FROM v_list.version THEN
            RETURN QUERY SELECT false, 'pending'::text, 'list_updated'::text, v_root.current_screening_id, v_result.party_revision;
            RETURN;
        END IF;
    END LOOP;
    RETURN QUERY SELECT v_root.status = 'clear',
        CASE v_root.status WHEN 'blocked' THEN 'blocked' WHEN 'match' THEN 'match' WHEN 'pending' THEN 'pending' ELSE 'clear' END,
        CASE v_root.status WHEN 'blocked' THEN 'true_match' WHEN 'match' THEN 'suspected_match' WHEN 'pending' THEN 'incomplete'
            ELSE 'screened' END,
        v_root.current_screening_id, v_result.party_revision;
END
$$;
REVOKE ALL ON FUNCTION brant_pty.party_screening_state(text, timestamptz, boolean) FROM PUBLIC;

-- PartyScreeningGuard v1: the state with FOR SHARE locks, for a downstream context's transaction.
CREATE FUNCTION brant_pty.party_screening_guard_v1(p_party_id text, p_at timestamptz)
    RETURNS TABLE (clear boolean, status text, reason text, screening_id text, party_revision bigint)
    LANGUAGE sql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT s.clear, s.status, s.reason, s.screening_id, s.party_revision
    FROM brant_pty.party_screening_state(p_party_id, p_at, true) s
$$;
REVOKE ALL ON FUNCTION brant_pty.party_screening_guard_v1(text, timestamptz) FROM PUBLIC;
