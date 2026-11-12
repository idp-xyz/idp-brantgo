-- STL: the payment plan review — an order account follows TRD's order revision (tactical model
-- STL-AGG-OrderAccount, entity PaymentPlanReview; SM-STL-PAYMENT-PLAN-REVIEW; WF-06 step 6; event
-- storming PL-09; STL-INV-003; S5 plan S5-09, D-4, D-18, D-21; WBS S5-09b, unit C4b-STL).
--
-- When TRD amends an order (OrderAmended), payment_plan_review.open moves the order account to the
-- new revision: the account adopts that revision's payment stages (inserted beside the earlier
-- revisions' stages, which stay as they were), and a review record for (order, revision) lists what
-- no longer fits — a stage that is gone, a stage with more applied than it now asks, a changed credit
-- portion — as items a person of the responsible role settles item by item with a reason
-- (payment_plan_review.settle, human only). A later revision's review supersedes an open review of
-- an earlier one; at most one review of an order is open.
--
-- Applications made at an earlier revision remain facts bound to the stage row they were made on.
-- They stand on the stage of the same stage ID at every later revision: what is applied on a stage
-- of the account's revision is the sum of the order's standing applications on that stage ID made at
-- that revision or before. PaymentConditionGuard v1 is replaced here with that rule (same signature
-- and result), and a deferred check holds it for every new application.

-- An order account is inserted open at version 1; afterwards it changes only by one version at a
-- time: to a later order revision (the payment plan review's move, with that revision's hash and
-- currency revision) or at the same revision (a review settled). It is never deleted.
CREATE OR REPLACE FUNCTION brant_stl.guard_order_account() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['order_revision', 'order_hash', 'currency_revision', 'version', 'updated_at'];
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status = 'open' AND NEW.version = 1 THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.status = 'open' AND NEW.version = OLD.version + 1 AND NEW.updated_at >= OLD.updated_at
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving)
            AND (NEW.order_revision > OLD.order_revision OR (NEW.order_revision = OLD.order_revision AND NEW.order_hash = OLD.order_hash
                AND NEW.currency_revision = OLD.currency_revision)) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.order_account: an order account is opened once and moves only by its payment plan review'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- At commit, an account moved to another revision has that revision's stages and its review.
CREATE FUNCTION brant_stl.check_order_account_moved() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.order_revision = OLD.order_revision THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_stl.order_account_stage s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
            AND s.order_id = NEW.order_id AND s.order_revision = NEW.order_revision)
        OR NOT EXISTS (SELECT 1 FROM brant_stl.payment_plan_review r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.order_id = NEW.order_id AND r.order_revision = NEW.order_revision AND r.from_revision = OLD.order_revision) THEN
        RAISE EXCEPTION 'brant_stl.order_account: % moved to revision % without its stages and payment plan review', NEW.order_id, NEW.order_revision
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER order_account_moved AFTER UPDATE ON brant_stl.order_account
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_order_account_moved();

CREATE TABLE brant_stl.payment_plan_review (
    tenant_id              text        NOT NULL,
    company_id             text        NOT NULL,
    order_id               text        NOT NULL,
    order_revision         bigint      NOT NULL CHECK (order_revision > 0),
    order_hash             text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    from_revision          bigint      NOT NULL CHECK (from_revision >= 0),
    from_hash              text        NOT NULL CHECK (from_hash = '' OR from_hash ~ '^[0-9a-f]{64}$'),
    responsible_role       text        NOT NULL CHECK (responsible_role ~ '^[a-z][a-z0-9_]{0,63}(,[a-z][a-z0-9_]{0,63})*$'),
    status                 text        NOT NULL CHECK (status IN ('open', 'settled', 'superseded')),
    version                bigint      NOT NULL CHECK (version BETWEEN 1 AND 2),
    opened_by_kind         text        NOT NULL CHECK (opened_by_kind IN ('human', 'system')),
    opened_by_id           text        NOT NULL,
    open_epoch             bigint      NOT NULL CHECK (open_epoch > 0),
    opened_at              timestamptz NOT NULL,
    settled_by_kind        text        CHECK (settled_by_kind = 'human'),
    settled_by_id          text,
    settle_epoch           bigint      CHECK (settle_epoch > 0),
    settled_at             timestamptz,
    settle_reason          text        CHECK (char_length(settle_reason) BETWEEN 1 AND 1000),
    superseded_by_revision bigint,
    superseded_at          timestamptz,
    updated_at             timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_stl.order_account (tenant_id, company_id, order_id),
    CHECK (from_revision < order_revision),
    CHECK ((from_revision = 0) = (from_hash = '')),
    CHECK ((status = 'open') = (version = 1)),
    CHECK ((status = 'settled') = (settled_by_id IS NOT NULL AND settled_by_kind IS NOT NULL AND settle_epoch IS NOT NULL
        AND settled_at IS NOT NULL AND settle_reason IS NOT NULL)),
    CHECK (status = 'settled' OR (settled_by_id IS NULL AND settled_by_kind IS NULL AND settle_epoch IS NULL AND settled_at IS NULL
        AND settle_reason IS NULL)),
    CHECK ((status = 'superseded') = (superseded_by_revision IS NOT NULL AND superseded_at IS NOT NULL)),
    CHECK (status = 'superseded' OR (superseded_by_revision IS NULL AND superseded_at IS NULL)),
    CHECK (superseded_by_revision IS NULL OR superseded_by_revision > order_revision),
    CHECK (updated_at >= opened_at)
);

-- At most one review of an order is open (WF-06: a later revision's review supersedes the open one).
CREATE UNIQUE INDEX payment_plan_review_open ON brant_stl.payment_plan_review (tenant_id, company_id, order_id) WHERE status = 'open';

-- A review is inserted open at version 1 for the revision its account holds; it moves once, open →
-- settled (a person) or open → superseded (a later revision), along SM-STL-PAYMENT-PLAN-REVIEW, and
-- is never deleted.
CREATE FUNCTION brant_stl.guard_payment_plan_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_moving text[] := ARRAY['status', 'version', 'settled_by_kind', 'settled_by_id', 'settle_epoch', 'settled_at', 'settle_reason',
        'superseded_by_revision', 'superseded_at', 'updated_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_stl.payment_plan_review: reviews are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' OR NEW.version <> 1 OR NOT EXISTS (SELECT 1 FROM brant_stl.order_account a WHERE a.tenant_id = NEW.tenant_id
                AND a.company_id = NEW.company_id AND a.order_id = NEW.order_id AND a.order_revision = NEW.order_revision
                AND a.order_hash = NEW.order_hash) THEN
            RAISE EXCEPTION 'brant_stl.payment_plan_review: a review opens for the revision its order account holds'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.status IN ('settled', 'superseded') AND NEW.version = OLD.version + 1 AND NEW.updated_at >= OLD.updated_at
            AND (to_jsonb(NEW) - v_moving) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_moving) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.payment_plan_review: review % of % moves only along SM-STL-PAYMENT-PLAN-REVIEW', OLD.order_revision, OLD.order_id
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER payment_plan_review_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.payment_plan_review
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_payment_plan_review();

-- What no longer fits the new revision: a stage that is gone (from = its due at the earlier
-- revision, to = 0, applied = what stands on it), a stage with more applied than it now asks (from,
-- to = its due before and now, applied), or a changed credit portion (no stage; from, to = the
-- portion before and now, applied = 0). Amounts in minor units of the account's currency.
CREATE TABLE brant_stl.payment_plan_review_item (
    tenant_id      text          NOT NULL,
    company_id     text          NOT NULL,
    order_id       text          NOT NULL,
    order_revision bigint        NOT NULL,
    seq            integer       NOT NULL CHECK (seq >= 1),
    kind           text          NOT NULL CHECK (kind IN ('stage_removed', 'stage_over_applied', 'credit_portion_changed')),
    stage_id       text          CHECK (stage_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    from_minor     numeric(32,0) NOT NULL CHECK (from_minor >= 0),
    to_minor       numeric(32,0) NOT NULL CHECK (to_minor >= 0),
    applied_minor  numeric(32,0) NOT NULL CHECK (applied_minor >= 0),
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision, seq),
    FOREIGN KEY (tenant_id, company_id, order_id, order_revision)
        REFERENCES brant_stl.payment_plan_review (tenant_id, company_id, order_id, order_revision),
    CHECK ((kind = 'credit_portion_changed') = (stage_id IS NULL)),
    CHECK (kind <> 'stage_removed' OR to_minor = 0),
    CHECK (kind <> 'stage_over_applied' OR applied_minor > to_minor),
    CHECK (kind <> 'credit_portion_changed' OR (applied_minor = 0 AND from_minor <> to_minor))
);

-- Items are inserted with their open review and never change.
CREATE FUNCTION brant_stl.guard_payment_plan_review_item() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND EXISTS (SELECT 1 FROM brant_stl.payment_plan_review r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.order_id = NEW.order_id AND r.order_revision = NEW.order_revision AND r.status = 'open') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.payment_plan_review_item: items are inserted with their open review and never change'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER payment_plan_review_item_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.payment_plan_review_item
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_payment_plan_review_item();

-- How the settling person disposed of each item: accepted as it stands, or corrected by other
-- commands (an unapply and a new application, a credit command) before the review was settled.
CREATE TABLE brant_stl.payment_plan_review_disposition (
    tenant_id      text    NOT NULL,
    company_id     text    NOT NULL,
    order_id       text    NOT NULL,
    order_revision bigint  NOT NULL,
    seq            integer NOT NULL,
    disposition    text    NOT NULL CHECK (disposition IN ('accepted', 'corrected')),
    note           text    NOT NULL CHECK (char_length(note) BETWEEN 1 AND 500),
    PRIMARY KEY (tenant_id, company_id, order_id, order_revision, seq),
    FOREIGN KEY (tenant_id, company_id, order_id, order_revision, seq)
        REFERENCES brant_stl.payment_plan_review_item (tenant_id, company_id, order_id, order_revision, seq)
);

-- Dispositions are inserted with the settlement of their review and never change.
CREATE FUNCTION brant_stl.guard_payment_plan_review_disposition() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND EXISTS (SELECT 1 FROM brant_stl.payment_plan_review r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.order_id = NEW.order_id AND r.order_revision = NEW.order_revision AND r.status = 'settled') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_stl.payment_plan_review_disposition: dispositions are inserted when their review is settled and never change'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER payment_plan_review_disposition_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_stl.payment_plan_review_disposition
    FOR EACH ROW EXECUTE FUNCTION brant_stl.guard_payment_plan_review_disposition();

-- At commit, a settled review has a disposition for every item ("逐项处置").
CREATE FUNCTION brant_stl.check_payment_plan_review_settled() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'settled' AND EXISTS (SELECT 1 FROM brant_stl.payment_plan_review_item i
            LEFT JOIN brant_stl.payment_plan_review_disposition d ON d.tenant_id = i.tenant_id AND d.company_id = i.company_id
                AND d.order_id = i.order_id AND d.order_revision = i.order_revision AND d.seq = i.seq
            WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.order_id = NEW.order_id
                AND i.order_revision = NEW.order_revision AND d.seq IS NULL) THEN
        RAISE EXCEPTION 'brant_stl.payment_plan_review: review % of % is settled without disposing of every item', NEW.order_revision, NEW.order_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER payment_plan_review_settled AFTER UPDATE ON brant_stl.payment_plan_review
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_payment_plan_review_settled();

-- At commit, a new application keeps its stage within the amount at the revision it was made:
-- everything standing on that stage ID from that revision or an earlier one (STL-INV-002 with the
-- carry-over above).
CREATE FUNCTION brant_stl.check_application_stage_carry() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.target_kind = 'order_stage' AND (SELECT s.amount_minor FROM brant_stl.order_account_stage s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.order_id = NEW.order_id AND s.order_revision = NEW.order_revision AND s.stage_id = NEW.stage_id)
        < (SELECT coalesce(sum(b.amount_minor), 0) FROM brant_stl.application b WHERE b.tenant_id = NEW.tenant_id AND b.company_id = NEW.company_id
            AND b.order_id = NEW.order_id AND b.stage_id = NEW.stage_id AND b.order_revision <= NEW.order_revision AND b.status = 'applied') THEN
        RAISE EXCEPTION 'brant_stl.application: % applies stage % of % beyond its amount (STL-INV-002)', NEW.application_id, NEW.stage_id, NEW.order_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER application_stage_carry AFTER INSERT ON brant_stl.application
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_stl.check_application_stage_carry();

-- PaymentConditionGuard v1 (R45), same signature and result as in 000002; applications made at the
-- account's revision or an earlier one count for the stage of the same ID.
CREATE OR REPLACE FUNCTION brant_stl.guard_payment_condition_v1(p_order_id text, p_order_revision bigint, p_scope text, p_at timestamptz)
    RETURNS TABLE (status text, reason text, order_revision bigint, order_hash text, currency text, required_minor numeric,
                   applied_minor numeric, stage_id text, stage_trigger text, stage_due_minor numeric, stage_applied_minor numeric,
                   shortfall_minor numeric, checked_at timestamptz, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant   text := pg_catalog.current_setting('app.tenant_id', true);
    v_company  text := pg_catalog.current_setting('app.company_id', true);
    v_a        record;
    v_s        record;
    v_required numeric := 0;
    v_applied  numeric := 0;
    v_first    record;
    v_found    boolean := false;
BEGIN
    IF p_order_id IS NULL OR p_order_revision IS NULL OR p_scope IS NULL OR p_at IS NULL THEN
        RAISE EXCEPTION 'brant_stl.guard_payment_condition_v1: order_id, order_revision, scope and at are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_scope NOT IN ('shipment', 'documents') THEN
        RAISE EXCEPTION 'brant_stl.guard_payment_condition_v1: scope is shipment or documents' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT a.order_revision, a.order_hash, a.currency INTO v_a FROM brant_stl.order_account a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.order_id = p_order_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT 'blocked'::text, 'no_account'::text, NULL::bigint, NULL::text, NULL::text, NULL::numeric, NULL::numeric, NULL::text,
            NULL::text, NULL::numeric, NULL::numeric, NULL::numeric, p_at, 'COMMERCIAL_RELEASE_BLOCKED'::text;
        RETURN;
    END IF;
    IF v_a.order_revision <> p_order_revision THEN
        RETURN QUERY SELECT 'blocked'::text, 'revision_mismatch'::text, v_a.order_revision, v_a.order_hash, v_a.currency, NULL::numeric,
            NULL::numeric, NULL::text, NULL::text, NULL::numeric, NULL::numeric, NULL::numeric, p_at, 'COMMERCIAL_RELEASE_BLOCKED'::text;
        RETURN;
    END IF;
    FOR v_s IN
        SELECT s.stage_id, s.stage_trigger, s.amount_minor,
            (SELECT coalesce(sum(x.amount_minor), 0) FROM brant_stl.application x WHERE x.tenant_id = s.tenant_id AND x.company_id = s.company_id
                AND x.order_id = s.order_id AND x.order_revision <= s.order_revision AND x.stage_id = s.stage_id AND x.status = 'applied') AS applied
        FROM brant_stl.order_account_stage s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.order_id = p_order_id AND s.order_revision = v_a.order_revision
            AND (s.stage_trigger IN ('order_confirmed', 'before_shipment')
                OR (p_scope = 'documents' AND s.stage_trigger IN ('after_shipment', 'after_invoice') AND s.release_control = 'documents'))
        ORDER BY s.seq
    LOOP
        v_required := v_required + v_s.amount_minor;
        v_applied := v_applied + v_s.applied;
        IF NOT v_found AND v_s.applied < v_s.amount_minor THEN
            v_first := v_s;
            v_found := true;
        END IF;
    END LOOP;
    IF v_found THEN
        RETURN QUERY SELECT 'blocked'::text, 'stage_unpaid'::text, v_a.order_revision, v_a.order_hash, v_a.currency, v_required, v_applied,
            v_first.stage_id::text, v_first.stage_trigger::text, v_first.amount_minor::numeric, v_first.applied::numeric,
            (v_first.amount_minor - v_first.applied)::numeric, p_at, 'COMMERCIAL_RELEASE_BLOCKED'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT 'satisfied'::text, ''::text, v_a.order_revision, v_a.order_hash, v_a.currency, v_required, v_applied, NULL::text,
        NULL::text, NULL::numeric, NULL::numeric, NULL::numeric, p_at, NULL::text;
END
$$;

REVOKE ALL ON FUNCTION brant_stl.guard_payment_condition_v1(text, bigint, text, timestamptz) FROM PUBLIC;

ALTER TABLE brant_stl.payment_plan_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.payment_plan_review FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_plan_review_scope ON brant_stl.payment_plan_review
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.payment_plan_review_item ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.payment_plan_review_item FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_plan_review_item_scope ON brant_stl.payment_plan_review_item
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_stl.payment_plan_review_disposition ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_stl.payment_plan_review_disposition FORCE ROW LEVEL SECURITY;
CREATE POLICY payment_plan_review_disposition_scope ON brant_stl.payment_plan_review_disposition
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
