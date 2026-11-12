-- PUR: purchase drafts and the demand allocation of active order lines (tactical model
-- PUR-AGG-PurchaseDraft, PUR-AGG-DemandAllocation, SM-PUR-DRAFT, PUR-INV-001 to 007; WBS S3-10,
-- S3-16; Python T04 A46 migration 17, t04_purchase_draft and t04_purchase_draft_line).
--
-- A purchase draft is prepared once and never edited (PUR-INV-007): its request and its document
-- are stored as the exact canonical JSON bytes their SHA-256 cover, which the CHECKs recompute, and
-- the only change is prepared → voided by a human with a reason; nothing is deleted. Its lines are
-- its claims on order lines, written with it and never after (a late line is refused). Every draft
-- is not_committed: commitments are S5.
--
-- DemandAllocation is one root per order line (tenant, company, order_id, order_line_id): the
-- line's quantity at the order revision it is bound to (demand), in the line's unit, and the sum of
-- the effective allocation entries (allocated), never above the demand (PUR-INV-002). A draft line
-- takes an entry; voiding the draft releases it (released_version), and the entry stays. The root's
-- row version moves by one per command; a command locks the roots FOR UPDATE in (order_id,
-- order_line_id) order and writes the balance with a compare-and-set, so concurrent drafts on one
-- line commit one at a time and the second sees the first's balance. Deferred constraint triggers
-- check at commit that every root's balance is the sum of its effective entries and that a draft's
-- entries are exactly its lines, all effective while it is prepared and all released once it is
-- voided.
--
-- References to TRD, SUP, PTY, CAT, POL and CST are plain IDs and hashes inside the rows and the
-- document: no foreign key crosses a schema (DB2). action_result and audit_event are the command
-- ledger of PUR (internal/foundation/action).

CREATE SCHEMA brant_pur;

CREATE TABLE brant_pur.purchase_draft (
    tenant_id          text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    draft_id           text        NOT NULL CHECK (draft_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status             text        NOT NULL CHECK (status IN ('prepared', 'voided')),
    row_version        bigint      NOT NULL CHECK (row_version IN (1, 2)),
    commitment_state   text        NOT NULL CHECK (commitment_state = 'not_committed'),
    order_id           text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision     bigint      NOT NULL CHECK (order_revision > 0),
    order_hash         text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    supplier_id        text        NOT NULL CHECK (supplier_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    currency           text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    line_count         integer     NOT NULL CHECK (line_count BETWEEN 1 AND 200),
    request            text        NOT NULL CHECK (left(request, 1) = '{'),
    request_hash       text        NOT NULL,
    document           text        NOT NULL CHECK (left(document, 1) = '{'),
    draft_hash         text        NOT NULL,
    supply_valid_until timestamptz NOT NULL,
    owner_actor_id     text        NOT NULL,
    owner_team_id      text,
    prepared_by_kind   text        NOT NULL CHECK (prepared_by_kind = 'human'),
    prepared_by_id     text        NOT NULL,
    prepare_epoch      bigint      NOT NULL CHECK (prepare_epoch > 0),
    prepared_at        timestamptz NOT NULL,
    voided_by_kind     text,
    voided_by_id       text,
    void_epoch         bigint,
    voided_at          timestamptz,
    void_reason        text,
    PRIMARY KEY (tenant_id, company_id, draft_id),
    CHECK (request_hash = encode(sha256(convert_to(request, 'UTF8')), 'hex')),
    CHECK (draft_hash = encode(sha256(convert_to(document, 'UTF8')), 'hex')),
    CHECK (owner_actor_id = prepared_by_id),
    CHECK (CASE WHEN status = 'prepared'
        THEN row_version = 1 AND voided_by_kind IS NULL AND voided_by_id IS NULL AND void_epoch IS NULL AND voided_at IS NULL
             AND void_reason IS NULL
        ELSE row_version = 2 AND voided_by_kind = 'human' AND voided_by_id IS NOT NULL AND void_epoch > 0 AND voided_at >= prepared_at
             AND char_length(void_reason) BETWEEN 1 AND 1000 END)
);

CREATE INDEX purchase_draft_order ON brant_pur.purchase_draft (tenant_id, company_id, order_id);

-- A draft is inserted prepared; it changes only by being voided, once, by a human, and nothing else
-- about it changes; it is never deleted.
CREATE FUNCTION brant_pur.guard_purchase_draft() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_void text[] := ARRAY['status', 'row_version', 'voided_by_kind', 'voided_by_id', 'void_epoch', 'voided_at', 'void_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.purchase_draft: purchase drafts are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'prepared' THEN
            RAISE EXCEPTION 'brant_pur.purchase_draft: a draft is prepared first' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'prepared' AND NEW.status = 'voided' AND (to_jsonb(NEW) - v_void) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_void) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pur.purchase_draft: draft % is never modified or replaced; it is only voided, once (PUR-INV-007)', OLD.draft_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER purchase_draft_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_draft
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_draft();

CREATE TABLE brant_pur.purchase_draft_line (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    draft_id          text          NOT NULL,
    order_line_id     text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    line_no           integer       NOT NULL CHECK (line_no BETWEEN 0 AND 199),
    order_id          text          NOT NULL,
    quantity          numeric(18,6) NOT NULL CHECK (quantity > 0),
    overbuy           numeric(18,6) NOT NULL CHECK (overbuy >= 0),
    source_kind       text          NOT NULL CHECK (source_kind IN ('supplier_offer', 'purchase_terms')),
    source_id         text          NOT NULL CHECK (source_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_revision   bigint        NOT NULL CHECK (source_revision > 0),
    condition_line_id text          NOT NULL CHECK (condition_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, draft_id, order_line_id),
    UNIQUE (tenant_id, company_id, draft_id, line_no),
    FOREIGN KEY (tenant_id, company_id, draft_id) REFERENCES brant_pur.purchase_draft (tenant_id, company_id, draft_id)
);

-- A line is written with its draft and never changes; one written later, or to a draft that is not
-- prepared, is refused.
CREATE FUNCTION brant_pur.guard_purchase_draft_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_pur.purchase_draft_line: draft lines never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
            AND d.draft_id = NEW.draft_id AND d.status = 'prepared' AND d.order_id = NEW.order_id AND NEW.line_no < d.line_count
            AND (SELECT count(*) FROM brant_pur.purchase_draft_line l WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id
                AND l.draft_id = NEW.draft_id) < d.line_count) THEN
        RAISE EXCEPTION 'brant_pur.purchase_draft_line: draft % does not take line %', NEW.draft_id, NEW.order_line_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER purchase_draft_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.purchase_draft_line
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_purchase_draft_line();

CREATE TABLE brant_pur.demand_allocation (
    tenant_id      text          NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text          NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id       text          NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_line_id  text          NOT NULL CHECK (order_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision bigint        NOT NULL CHECK (order_revision > 0),
    unit_id        text          NOT NULL,
    unit_revision  bigint        NOT NULL CHECK (unit_revision > 0),
    demand         numeric(18,6) NOT NULL CHECK (demand > 0),
    allocated      numeric(18,6) NOT NULL CHECK (allocated >= 0),
    row_version    bigint        NOT NULL CHECK (row_version > 0),
    created_at     timestamptz   NOT NULL,
    updated_at     timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, order_line_id),
    CHECK (allocated <= demand)
);

-- A root keeps its identity, unit and creation; its row version moves by one per command and its
-- order revision never goes back; it is never deleted.
CREATE FUNCTION brant_pur.guard_demand_allocation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.demand_allocation: allocations are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.order_id, NEW.order_line_id, NEW.unit_id, NEW.unit_revision, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.order_id, OLD.order_line_id, OLD.unit_id, OLD.unit_revision, OLD.created_at)
            OR NEW.row_version <> OLD.row_version + 1 OR NEW.order_revision < OLD.order_revision THEN
        RAISE EXCEPTION 'brant_pur.demand_allocation: identity, unit and creation are fixed, the version moves by one and the order revision never goes back'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER demand_allocation_guard BEFORE UPDATE OR DELETE ON brant_pur.demand_allocation
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_demand_allocation();

CREATE TABLE brant_pur.allocation_entry (
    tenant_id        text          NOT NULL,
    company_id       text          NOT NULL,
    order_id         text          NOT NULL,
    order_line_id    text          NOT NULL,
    draft_id         text          NOT NULL,
    quantity         numeric(18,6) NOT NULL CHECK (quantity > 0),
    taken_version    bigint        NOT NULL CHECK (taken_version > 0),
    released_version bigint,
    released_at      timestamptz,
    PRIMARY KEY (tenant_id, company_id, order_id, order_line_id, draft_id),
    FOREIGN KEY (tenant_id, company_id, order_id, order_line_id)
        REFERENCES brant_pur.demand_allocation (tenant_id, company_id, order_id, order_line_id),
    FOREIGN KEY (tenant_id, company_id, draft_id, order_line_id)
        REFERENCES brant_pur.purchase_draft_line (tenant_id, company_id, draft_id, order_line_id),
    CHECK ((released_version IS NULL) = (released_at IS NULL)),
    CHECK (released_version > taken_version)
);

CREATE INDEX allocation_entry_draft ON brant_pur.allocation_entry (tenant_id, company_id, draft_id);

-- An entry is taken once and released once; nothing else changes and nothing is deleted.
CREATE FUNCTION brant_pur.guard_allocation_entry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.allocation_entry: allocation entries are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.released_version IS NOT NULL OR NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft_line l
                WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.draft_id = NEW.draft_id
                    AND l.order_line_id = NEW.order_line_id AND l.order_id = NEW.order_id AND l.quantity = NEW.quantity) THEN
            RAISE EXCEPTION 'brant_pur.allocation_entry: an entry is the effective claim of its draft line' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.released_version IS NULL AND NEW.released_version IS NOT NULL
            AND (to_jsonb(NEW) - ARRAY['released_version', 'released_at']) IS NOT DISTINCT FROM
                (to_jsonb(OLD) - ARRAY['released_version', 'released_at']) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pur.allocation_entry: an entry is only released, once' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER allocation_entry_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.allocation_entry
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_allocation_entry();

-- At commit: a root's balance is the sum of its effective entries.
CREATE FUNCTION brant_pur.check_allocation_balance() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_allocated numeric;
    v_sum       numeric;
BEGIN
    SELECT a.allocated INTO v_allocated FROM brant_pur.demand_allocation a
    WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id AND a.order_id = NEW.order_id AND a.order_line_id = NEW.order_line_id;
    SELECT coalesce(sum(e.quantity), 0) INTO v_sum FROM brant_pur.allocation_entry e
    WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.order_id = NEW.order_id AND e.order_line_id = NEW.order_line_id
        AND e.released_version IS NULL;
    IF v_allocated IS DISTINCT FROM v_sum THEN
        RAISE EXCEPTION 'brant_pur.demand_allocation: line % of order % allocates %, its effective entries %', NEW.order_line_id, NEW.order_id,
            v_allocated, v_sum USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER demand_allocation_balance AFTER INSERT OR UPDATE ON brant_pur.demand_allocation
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_allocation_balance();
CREATE CONSTRAINT TRIGGER allocation_entry_balance AFTER INSERT OR UPDATE ON brant_pur.allocation_entry
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_allocation_balance();

-- At commit: a draft has exactly its line_count lines, one entry per line, all effective while it
-- is prepared and all released once it is voided.
CREATE FUNCTION brant_pur.check_draft_claims() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_draft record;
    v_lines bigint;
    v_effective bigint;
    v_released bigint;
BEGIN
    SELECT d.status, d.line_count INTO v_draft FROM brant_pur.purchase_draft d
    WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.draft_id = NEW.draft_id;
    SELECT count(*) INTO v_lines FROM brant_pur.purchase_draft_line l
    WHERE l.tenant_id = NEW.tenant_id AND l.company_id = NEW.company_id AND l.draft_id = NEW.draft_id;
    SELECT count(*) FILTER (WHERE e.released_version IS NULL), count(*) FILTER (WHERE e.released_version IS NOT NULL)
        INTO v_effective, v_released FROM brant_pur.allocation_entry e
    WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.draft_id = NEW.draft_id;
    IF v_lines <> v_draft.line_count OR (v_draft.status = 'prepared' AND (v_effective, v_released) <> (v_lines, 0::bigint))
            OR (v_draft.status = 'voided' AND (v_effective, v_released) <> (0::bigint, v_lines)) THEN
        RAISE EXCEPTION 'brant_pur.purchase_draft: draft % (%) has % of % lines, % effective and % released entries', NEW.draft_id,
            v_draft.status, v_lines, v_draft.line_count, v_effective, v_released USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER purchase_draft_claims AFTER INSERT OR UPDATE ON brant_pur.purchase_draft
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_draft_claims();
CREATE CONSTRAINT TRIGGER allocation_entry_claims AFTER INSERT OR UPDATE ON brant_pur.allocation_entry
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_draft_claims();

CREATE TABLE brant_pur.action_result (
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

CREATE TABLE brant_pur.audit_event (
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

ALTER TABLE brant_pur.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_pur.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_pur.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.purchase_draft ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_draft FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_draft_scope ON brant_pur.purchase_draft
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.purchase_draft_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.purchase_draft_line FORCE ROW LEVEL SECURITY;
CREATE POLICY purchase_draft_line_scope ON brant_pur.purchase_draft_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.demand_allocation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.demand_allocation FORCE ROW LEVEL SECURITY;
CREATE POLICY demand_allocation_scope ON brant_pur.demand_allocation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.allocation_entry ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.allocation_entry FORCE ROW LEVEL SECURITY;
CREATE POLICY allocation_entry_scope ON brant_pur.allocation_entry
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
