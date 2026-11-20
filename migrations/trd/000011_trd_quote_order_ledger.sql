-- TRD 000011: partial and multiple orders from one quote (S6-07, TRD-INV-024, F-01, BC-D06, SIM-03).
--
-- A quote that does not opt in still converts once, as full_v1, through order.prepare. A quote
-- sealed with conversion_mode multi_order converts through order.prepare_partial: each order is
-- slot multi_order, and one append-only row per line records the quantity it took. full_v1 stays
-- unique. Cancelling an order does not delete the row. A consumption row counts as the conversion
-- being taken for quote.replace, quote.cancel and a cost-source hold. GoalReadiness v1 still reads
-- the full_v1 slot only; this file does not change that read.

-- Relax the 1:1 slot checks. The names are the ones PostgreSQL gave the unnamed constraints.
DO $$
DECLARE
    dropped int := 0;
    r record;
BEGIN
    FOR r IN
        SELECT con.conname, pg_get_constraintdef(con.oid) AS def
        FROM pg_constraint con
        JOIN pg_class rel ON rel.oid = con.conrelid
        JOIN pg_namespace nsp ON nsp.oid = rel.relnamespace
        WHERE nsp.nspname = 'brant_trd' AND rel.relname = 'sales_order'
    LOOP
        IF r.def LIKE '%slot = ''full_v1''%'
            OR r.def LIKE 'UNIQUE (tenant_id, company_id, quote_id, slot)%'
            OR r.def LIKE 'FOREIGN KEY (tenant_id, company_id, quote_id, slot) REFERENCES brant_trd.quote_conversion_slot%' THEN
            EXECUTE format('ALTER TABLE brant_trd.sales_order DROP CONSTRAINT %I', r.conname);
            dropped := dropped + 1;
        END IF;
    END LOOP;
    IF dropped <> 3 THEN
        RAISE EXCEPTION 'brant_trd.sales_order: expected to relax 3 full_v1 constraints, dropped %', dropped;
    END IF;
END $$;

ALTER TABLE brant_trd.sales_order
    ADD CONSTRAINT sales_order_slot_check CHECK (slot IN ('full_v1', 'multi_order'));

-- One full_v1 order per quote. Many multi_order orders may share the quote.
CREATE UNIQUE INDEX sales_order_one_full_v1
    ON brant_trd.sales_order (tenant_id, company_id, quote_id) WHERE slot = 'full_v1';

-- The quantity a partial order took of a quote line. Append-only: cancel does not return it.
CREATE TABLE brant_trd.quote_order_consumption (
    tenant_id         text          NOT NULL,
    company_id        text          NOT NULL,
    quote_id          text          NOT NULL,
    order_id          text          NOT NULL,
    business_revision bigint        NOT NULL CHECK (business_revision > 0),
    line_id           text          NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    quantity          numeric(18,6) NOT NULL CHECK (quantity > 0),
    recorded_by_kind  text          NOT NULL CHECK (recorded_by_kind = 'human'),
    recorded_by_id    text          NOT NULL,
    record_epoch      bigint        NOT NULL CHECK (record_epoch > 0),
    recorded_at       timestamptz   NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, line_id)
);

CREATE INDEX quote_order_consumption_line
    ON brant_trd.quote_order_consumption (tenant_id, company_id, quote_id, line_id);

-- No update and no delete. A full_v1 slot and a consumption row do not share a quote.
CREATE FUNCTION brant_trd.guard_quote_order_consumption() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_order_consumption: the consumption ledger is append-only'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id) THEN
        RAISE EXCEPTION 'brant_trd.quote_order_consumption: CONVERSION_SLOT_CONFLICT: the full_v1 slot is taken'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER quote_order_consumption_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_order_consumption
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_order_consumption();

-- At commit, every consumption row belongs to a multi_order sales order of the same revision.
-- The rows are inserted before the order, in the same transaction.
CREATE FUNCTION brant_trd.guard_quote_order_consumption_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM brant_trd.sales_order o WHERE o.tenant_id = NEW.tenant_id AND o.company_id = NEW.company_id
            AND o.order_id = NEW.order_id AND o.quote_id = NEW.quote_id AND o.slot = 'multi_order'
            AND o.revision = NEW.business_revision) THEN
        RAISE EXCEPTION 'brant_trd.quote_order_consumption: a consumption row belongs to a multi_order order of the same revision'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END $$;
CREATE CONSTRAINT TRIGGER quote_order_consumption_order
    AFTER INSERT ON brant_trd.quote_order_consumption
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_order_consumption_order();

ALTER TABLE brant_trd.quote_order_consumption ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_order_consumption FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_order_consumption_scope ON brant_trd.quote_order_consumption
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- full_v1 still requires its slot row. multi_order requires the revision, a valid acceptance and
-- the consumption rows, and no full_v1 slot. The rest of the order's life is unchanged.
CREATE OR REPLACE FUNCTION brant_trd.guard_sales_order() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    mutable constant text[] := ARRAY['status', 'row_version', 'activated_review_id', 'updated_at', 'order_revision', 'order_hash'];
    v_same  boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.sales_order: orders are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'draft' OR NEW.row_version <> 1 OR NEW.order_revision <> 1 OR NEW.order_hash <> NEW.snapshot_hash
                OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_document d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
                    AND d.quote_id = NEW.quote_id AND d.revision = NEW.revision AND d.business_hash = NEW.business_hash
                    AND d.number = NEW.number AND d.content_sha256 = NEW.document_sha256)
                OR NOT EXISTS (SELECT 1 FROM brant_trd.acceptance a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                    AND a.acceptance_id = NEW.acceptance_id AND a.quote_id = NEW.quote_id AND a.revision = NEW.revision
                    AND a.business_hash = NEW.business_hash AND a.effectivity = 'valid' AND a.effectivity_version = NEW.acceptance_version
                    AND a.arrival_at = NEW.arrival_at AND a.arrival_basis = NEW.arrival_basis AND a.expires_at = NEW.expires_at) THEN
            RAISE EXCEPTION 'brant_trd.sales_order: an order is prepared a draft, binding the formal document and a valid acceptance of the revision'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.slot = 'full_v1' THEN
            IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
                    AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id AND s.slot = NEW.slot AND s.order_id = NEW.order_id
                    AND s.revision = NEW.revision AND s.business_hash = NEW.business_hash)
                    OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id
                        AND c.company_id = NEW.company_id AND c.quote_id = NEW.quote_id) THEN
                RAISE EXCEPTION 'brant_trd.sales_order: a full_v1 order is prepared for its conversion slot, and not after a partial order'
                    USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        ELSIF NEW.slot = 'multi_order' THEN
            IF EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
                    AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id)
                    OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id
                        AND c.company_id = NEW.company_id AND c.order_id = NEW.order_id AND c.quote_id = NEW.quote_id
                        AND c.business_revision = NEW.revision)
                    OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id
                        AND c.company_id = NEW.company_id AND c.order_id = NEW.order_id
                        AND (c.quote_id <> NEW.quote_id OR c.business_revision <> NEW.revision)) THEN
                RAISE EXCEPTION 'brant_trd.sales_order: a multi_order order binds its consumption rows and no full_v1 slot'
                    USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        ELSE
            RAISE EXCEPTION 'brant_trd.sales_order: slot % is not a conversion', NEW.slot
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (to_jsonb(OLD) - mutable) IS DISTINCT FROM (to_jsonb(NEW) - mutable) OR NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'brant_trd.sales_order: order % keeps its identity, binding and snapshot; its row version moves by one',
            OLD.order_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    v_same := NEW.order_revision = OLD.order_revision AND NEW.order_hash = OLD.order_hash;
    IF v_same AND OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.activated_review_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF v_same AND OLD.status = 'draft' AND NEW.status = 'active' AND EXISTS (SELECT 1 FROM brant_trd.sales_order_commit_evidence e
            JOIN brant_trd.sales_order_review r ON r.tenant_id = e.tenant_id AND r.company_id = e.company_id AND r.order_id = e.order_id
                AND r.review_id = e.review_id
            WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.order_id = NEW.order_id
                AND e.order_revision = NEW.order_revision AND e.order_hash = NEW.order_hash AND e.review_id = NEW.activated_review_id
                AND e.row_version = NEW.row_version AND r.status = 'approved') THEN
        RETURN NEW;
    END IF;
    IF v_same AND OLD.status = 'draft' AND NEW.status IN ('cancelled', 'closed_unfulfilled') AND NEW.activated_review_id IS NULL
            AND EXISTS (SELECT 1 FROM brant_trd.sales_order_closure c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.order_id = NEW.order_id AND c.status = NEW.status AND c.row_version = NEW.row_version) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'active' AND NEW.activated_review_id = OLD.activated_review_id
            AND (v_same OR (NEW.order_revision = OLD.order_revision + 1
                AND EXISTS (SELECT 1 FROM brant_trd.sales_order_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                    AND r.order_id = NEW.order_id AND r.order_revision = NEW.order_revision AND r.order_hash = NEW.order_hash
                    AND r.previous_hash = OLD.order_hash AND r.row_version = NEW.row_version)
                AND EXISTS (SELECT 1 FROM brant_trd.sales_order_amendment_commit e WHERE e.tenant_id = NEW.tenant_id
                    AND e.company_id = NEW.company_id AND e.order_id = NEW.order_id AND e.order_revision = NEW.order_revision
                    AND e.row_version = NEW.row_version))) THEN
        RETURN NEW;
    END IF;
    IF v_same AND OLD.status = 'active' AND NEW.status IN ('cancelled', 'closed') AND NEW.activated_review_id = OLD.activated_review_id
            AND EXISTS (SELECT 1 FROM brant_trd.sales_order_end e WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id
                AND e.order_id = NEW.order_id AND e.status = NEW.status AND e.row_version = NEW.row_version
                AND e.order_revision = NEW.order_revision AND e.order_hash = NEW.order_hash) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.sales_order: order % is %; it does not become % here', OLD.order_id, OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END $$;

-- A consumption row locks replace and cancel the way the full_v1 slot does.
CREATE OR REPLACE FUNCTION brant_trd.guard_quote_commit_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_commit_evidence: commit evidence never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision AND r.status = 'pending_approval'
                AND r.business_hash = NEW.business_hash)
            OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_document d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
                AND d.quote_id = NEW.quote_id AND d.revision = NEW.revision AND d.business_hash = NEW.business_hash
                AND d.number = NEW.number AND d.content_sha256 = NEW.content_sha256)
            OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_approval a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                AND a.quote_id = NEW.quote_id AND a.approval_id = NEW.approval_id AND a.revision = NEW.revision
                AND a.business_hash = NEW.business_hash AND a.effectivity = 'valid' AND a.target_command = NEW.command
                AND a.expected_active_revision IS NOT DISTINCT FROM NEW.replaced_revision) THEN
        RAISE EXCEPTION 'brant_trd.quote_commit_evidence: commit evidence binds the revision''s formal document and a valid approval for the command'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.command = 'quote.replace' AND (EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id
                AND c.company_id = NEW.company_id AND c.quote_id = NEW.quote_id)) THEN
        RAISE EXCEPTION 'brant_trd.quote_commit_evidence: ROOT_CONVERSION_LOCKED: the conversion slot is taken'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION brant_trd.guard_quote_revision_closure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_revision_closure: a closure never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
            AND r.status = ANY (CASE NEW.status WHEN 'withdrawn' THEN ARRAY['frozen', 'pending_approval'] ELSE ARRAY['active'] END)) THEN
        RAISE EXCEPTION 'brant_trd.quote_revision_closure: a withdrawal closes a frozen or pending revision, a cancellation the active one'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'cancelled' AND (EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id
                AND c.company_id = NEW.company_id AND c.quote_id = NEW.quote_id)) THEN
        RAISE EXCEPTION 'brant_trd.quote_revision_closure: ROOT_CONVERSION_LOCKED: the conversion slot is taken'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION brant_trd.guard_quote_conversion_slot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_conversion_slot: a conversion slot is kept for good' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision AND r.status = 'active' AND r.business_hash = NEW.business_hash)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_risk_hold h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
                AND h.quote_id = NEW.quote_id AND h.revision = NEW.revision)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id
                AND c.company_id = NEW.company_id AND c.quote_id = NEW.quote_id)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND (r.commercial::jsonb #>> '{data,conversion_mode}') = 'multi_order') THEN
        RAISE EXCEPTION 'brant_trd.quote_conversion_slot: the slot is taken for the active, unheld single_full revision at its business hash'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

CREATE OR REPLACE FUNCTION brant_trd.guard_quote_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    same    boolean;
    slotted boolean;
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
    same := (to_jsonb(OLD) - 'status') = (to_jsonb(NEW) - 'status');
    slotted := EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
        AND s.quote_id = NEW.quote_id)
        OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
            AND c.quote_id = NEW.quote_id);
    IF same AND (OLD.status, NEW.status) IN (('frozen', 'pending_approval'), ('pending_approval', 'frozen')) THEN
        RETURN NEW;
    END IF;
    IF same AND OLD.status = 'pending_approval' AND NEW.status = 'active' AND EXISTS (SELECT 1 FROM brant_trd.quote_commit_evidence e
            WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.quote_id = NEW.quote_id AND e.revision = NEW.revision
            AND e.business_hash = NEW.business_hash) THEN
        RETURN NEW;
    END IF;
    IF same AND OLD.status = 'active' AND NEW.status = 'superseded' AND NOT slotted AND EXISTS (SELECT 1
            FROM brant_trd.quote_commit_evidence e WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id
            AND e.quote_id = NEW.quote_id AND e.replaced_revision = NEW.revision) THEN
        RETURN NEW;
    END IF;
    IF same AND ((OLD.status IN ('frozen', 'pending_approval') AND NEW.status = 'withdrawn')
                 OR (OLD.status = 'active' AND NEW.status = 'cancelled' AND NOT slotted))
            AND EXISTS (SELECT 1 FROM brant_trd.quote_revision_closure c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.quote_id = NEW.quote_id AND c.revision = NEW.revision AND c.status = NEW.status) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.quote_revision: revision % is %; its content and business hash do not change and it does not become %',
        OLD.revision, OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
END $$;

-- A cost-source hold is not registered once any partial order has consumed quantity.
CREATE OR REPLACE FUNCTION brant_trd.guard_quote_risk_hold() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a risk hold never changes and is never lifted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.cause = 'approval_revoked' AND NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r
            JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.quote_id = r.quote_id
                AND e.revision = r.revision
            JOIN brant_trd.quote_approval a ON a.tenant_id = e.tenant_id AND a.company_id = e.company_id AND a.quote_id = e.quote_id
                AND a.approval_id = e.approval_id
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'active' AND e.approval_id = NEW.approval_id AND a.effectivity = 'revoked') THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a hold is registered on the active revision whose formalization approval was revoked'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.cause = 'cost_source_denied' AND (NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r
            JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.quote_id = r.quote_id
                AND e.revision = r.revision
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'active' AND e.cost_snapshot_id = NEW.cost_snapshot_id)
            OR EXISTS (SELECT 1 FROM brant_trd.acceptance a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                AND a.quote_id = NEW.quote_id AND a.revision = NEW.revision AND a.effectivity IN ('valid', 'disputed'))
            OR EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
                AND s.quote_id = NEW.quote_id)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.quote_id = NEW.quote_id)) THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a cost source hold is registered on the active revision whose commit evidence names the snapshot, before a contract formed on it'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END $$;

-- conversion_taken is the full_v1 slot or any consumption row. GoalReadiness v1 does not read this
-- flag for its full_v1 evidence; it reads the slot.
CREATE OR REPLACE FUNCTION brant_trd.guard_quote_revision_active_v1(p_quote_id text, p_revision bigint, p_business_hash text)
    RETURNS TABLE (known boolean, status text, active_revision bigint, business_hash text, number text, content_sha256 text,
                   expires_at timestamptz, on_hold boolean, conversion_taken boolean, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_rev     record;
    v_doc     record;
    v_active  bigint;
    v_hold    text;
    v_slot    boolean;
BEGIN
    IF p_quote_id IS NULL OR p_revision IS NULL OR p_business_hash IS NULL THEN
        RAISE EXCEPTION 'brant_trd.guard_quote_revision_active_v1: quote_id, revision and business_hash are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    PERFORM 1 FROM brant_trd.quote_root q
    WHERE q.tenant_id = v_tenant AND q.company_id = v_company AND q.quote_id = p_quote_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::text, NULL::timestamptz, false, false,
            'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    SELECT r.status, r.business_hash, r.expires_at INTO v_rev
    FROM brant_trd.quote_revision r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.quote_id = p_quote_id AND r.revision = p_revision;
    SELECT d.number, d.content_sha256 INTO v_doc
    FROM brant_trd.quote_document d
    WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.quote_id = p_quote_id AND d.revision = p_revision;
    SELECT coalesce(max(r.revision), 0) INTO v_active
    FROM brant_trd.quote_revision r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.quote_id = p_quote_id AND r.status = 'active';
    v_slot := EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.quote_id = p_quote_id)
        OR EXISTS (SELECT 1 FROM brant_trd.quote_order_consumption c
            WHERE c.tenant_id = v_tenant AND c.company_id = v_company AND c.quote_id = p_quote_id);
    IF v_rev.status IS NULL OR v_doc.number IS NULL THEN
        RETURN QUERY SELECT false, v_rev.status, v_active, v_rev.business_hash, NULL::text, NULL::text, v_rev.expires_at, false, v_slot,
            'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    SELECT min(h.code) INTO v_hold
    FROM brant_trd.quote_risk_hold h
    WHERE h.tenant_id = v_tenant AND h.company_id = v_company AND h.quote_id = p_quote_id AND h.revision = p_revision;
    RETURN QUERY SELECT true, v_rev.status, v_active, v_rev.business_hash, v_doc.number, v_doc.content_sha256, v_rev.expires_at,
        v_hold IS NOT NULL, v_slot,
        CASE
            WHEN v_rev.business_hash <> p_business_hash THEN 'BUSINESS_HASH_MISMATCH'
            WHEN v_rev.status <> 'active' THEN 'QUOTE_NOT_CURRENT'
            WHEN v_hold IS NOT NULL THEN v_hold
        END::text;
END
$$;
