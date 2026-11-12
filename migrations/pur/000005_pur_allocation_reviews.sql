-- PUR: the allocation review after an order change (WBS S5-09b, PUR part; tactical model
-- PUR-AGG-DemandAllocation entity AllocationReview, SM-PUR-ALLOCATION-REVIEW; WF-06 step 4; event
-- storming PL-09, ES-09 step 5; S5 decisions D-18, D-21; BT10). Python V0.50 has none of this.
--
-- TRD's OrderAmended, OrderCancelled and OrderRemainderClosed reach PUR's Inbox consumer
-- pur.allocation_review, which opens, as its system actor, one review per (order, order revision,
-- cause) with the TRD event and the order's row version it follows, and one item per thing the change
-- touches of PUR's facts: an order line allocated above what the order now needs (allocation), a
-- prepared draft never committed that holds such a line (draft), an issued or confirmed commitment
-- buying on it (commitment), each with what PUR knew of it then as JSON. A review opened for a later
-- order version supersedes the open reviews of the order's earlier versions. A person (the purchaser,
-- the review's responsible role) settles a review once, every item with its disposition and a reason:
-- a draft voided, a commitment changed or cancelled — each of which must have happened — or no_action.
-- The review disposes of nothing itself; reviews and items are never deleted.

CREATE TABLE brant_pur.allocation_review (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    review_id        text        NOT NULL CHECK (review_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id         text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_revision   bigint      NOT NULL CHECK (order_revision > 0),
    order_hash       text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    order_version    bigint      NOT NULL CHECK (order_version > 0),
    cause            text        NOT NULL CHECK (cause IN ('order_amended', 'order_cancelled', 'order_remainder_closed')),
    event_id         text        NOT NULL CHECK (event_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status           text        NOT NULL CHECK (status IN ('open', 'settled', 'superseded')),
    row_version      bigint      NOT NULL CHECK (row_version IN (1, 2)),
    responsible_role text        NOT NULL CHECK (responsible_role = 'purchaser'),
    item_count       integer     NOT NULL CHECK (item_count BETWEEN 0 AND 1000),
    opened_by_kind   text        NOT NULL CHECK (opened_by_kind = 'system'),
    opened_by_id     text        NOT NULL,
    open_epoch       bigint      NOT NULL CHECK (open_epoch > 0),
    opened_at        timestamptz NOT NULL,
    settled_by_kind  text        CHECK (settled_by_kind = 'human'),
    settled_by_id    text,
    settle_epoch     bigint      CHECK (settle_epoch > 0),
    settled_at       timestamptz,
    settle_reason    text        CHECK (char_length(settle_reason) BETWEEN 1 AND 1000),
    superseded_by    text,
    superseded_at    timestamptz,
    PRIMARY KEY (tenant_id, company_id, review_id),
    -- WF-06: 各下游按（订单、订单修订）只开启一次复核 — once per order revision and change, once per event.
    UNIQUE (tenant_id, company_id, order_id, order_revision, cause),
    UNIQUE (tenant_id, company_id, event_id),
    CHECK ((settled_at IS NULL) = (settled_by_id IS NULL) AND (settled_by_id IS NULL) = (settled_by_kind IS NULL)
        AND (settled_by_id IS NULL) = (settle_epoch IS NULL) AND (settled_by_id IS NOT NULL OR settle_reason IS NULL)),
    CHECK ((superseded_by IS NULL) = (superseded_at IS NULL)),
    CHECK (CASE status
        WHEN 'open' THEN row_version = 1 AND settled_at IS NULL AND superseded_at IS NULL
        WHEN 'settled' THEN row_version = 2 AND settled_at >= opened_at AND superseded_at IS NULL
            AND (item_count > 0 OR settle_reason IS NOT NULL)
        ELSE row_version = 2 AND superseded_at >= opened_at AND settled_at IS NULL END)
);

CREATE INDEX allocation_review_order ON brant_pur.allocation_review (tenant_id, company_id, order_id, order_version);

-- A review is opened open; it changes once: settled by a human, or superseded by a review of the same
-- order opened for a later order version. Nothing else about it changes; it is never deleted.
CREATE FUNCTION brant_pur.guard_allocation_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_settle    text[] := ARRAY['status', 'row_version', 'settled_by_kind', 'settled_by_id', 'settle_epoch', 'settled_at', 'settle_reason'];
    v_supersede text[] := ARRAY['status', 'row_version', 'superseded_by', 'superseded_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.allocation_review: reviews are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' THEN
            RAISE EXCEPTION 'brant_pur.allocation_review: a review is opened first' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.status = 'settled' AND (to_jsonb(NEW) - v_settle) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_settle) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.status = 'superseded' AND (to_jsonb(NEW) - v_supersede) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_supersede)
            AND EXISTS (SELECT 1 FROM brant_pur.allocation_review r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.review_id = NEW.superseded_by AND r.order_id = NEW.order_id AND r.order_version > NEW.order_version) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_pur.allocation_review: review % moves only from open, once: settled, or superseded by a later order version''s review',
        OLD.review_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER allocation_review_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.allocation_review
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_allocation_review();

CREATE TABLE brant_pur.allocation_review_item (
    tenant_id                 text        NOT NULL,
    company_id                text        NOT NULL,
    review_id                 text        NOT NULL,
    item_no                   integer     NOT NULL CHECK (item_no BETWEEN 1 AND 1000),
    kind                      text        NOT NULL CHECK (kind IN ('allocation', 'draft', 'commitment')),
    order_line_id             text,
    draft_id                  text,
    commitment_id             text,
    commitment_revision       bigint      CHECK (commitment_revision > 0),
    -- What PUR knew of the item's object when the review opened (api.ReviewItemView).
    facts                     text        NOT NULL CHECK (left(facts, 1) = '{'),
    disposition               text        CHECK (disposition IN ('draft_voided', 'commitment_changed', 'commitment_cancelled', 'no_action')),
    disposition_draft_id      text,
    disposition_change_id     text,
    disposition_commitment_id text,
    disposition_revision      bigint      CHECK (disposition_revision > 1),
    disposition_at            timestamptz,
    disposition_reason        text        CHECK (char_length(disposition_reason) BETWEEN 1 AND 1000),
    PRIMARY KEY (tenant_id, company_id, review_id, item_no),
    FOREIGN KEY (tenant_id, company_id, review_id) REFERENCES brant_pur.allocation_review (tenant_id, company_id, review_id),
    CHECK (CASE kind
        WHEN 'allocation' THEN order_line_id IS NOT NULL AND draft_id IS NULL AND commitment_id IS NULL AND commitment_revision IS NULL
        WHEN 'draft' THEN order_line_id IS NULL AND draft_id IS NOT NULL AND commitment_id IS NULL AND commitment_revision IS NULL
        ELSE order_line_id IS NULL AND draft_id IS NOT NULL AND commitment_id IS NOT NULL AND commitment_revision IS NOT NULL END),
    -- A disposition names exactly the object it needs, with a reason; a change names its commitment
    -- and the revision its application made.
    CHECK (CASE coalesce(disposition, '')
        WHEN '' THEN num_nonnulls(disposition_draft_id, disposition_change_id, disposition_commitment_id, disposition_revision,
            disposition_at, disposition_reason) = 0
        WHEN 'no_action' THEN num_nonnulls(disposition_draft_id, disposition_change_id, disposition_commitment_id, disposition_revision,
            disposition_at) = 0 AND disposition_reason IS NOT NULL
        WHEN 'draft_voided' THEN disposition_draft_id IS NOT NULL AND disposition_change_id IS NULL AND disposition_commitment_id IS NULL
            AND disposition_revision IS NULL AND disposition_at IS NOT NULL AND disposition_reason IS NOT NULL
        WHEN 'commitment_changed' THEN disposition_draft_id IS NULL AND disposition_change_id IS NOT NULL
            AND disposition_commitment_id IS NOT NULL AND disposition_revision IS NOT NULL AND disposition_at IS NOT NULL
            AND disposition_reason IS NOT NULL
        ELSE disposition_draft_id IS NULL AND disposition_change_id IS NULL AND disposition_commitment_id IS NOT NULL
            AND disposition_revision IS NOT NULL AND disposition_at IS NOT NULL AND disposition_reason IS NOT NULL END)
);

-- Items are written with their review, while it is open, and never after it opened; an item changes
-- only by its disposition, once, while its review is still open (it is settled in the same
-- transaction). The disposition must be one that happened: the draft voided, the change of the
-- commitment applied, the commitment cancelled — as PUR's own rows show it.
CREATE FUNCTION brant_pur.guard_allocation_review_item() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_disposed text[] := ARRAY['disposition', 'disposition_draft_id', 'disposition_change_id', 'disposition_commitment_id',
                               'disposition_revision', 'disposition_at', 'disposition_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_pur.allocation_review_item: review items are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_pur.allocation_review r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.review_id = NEW.review_id AND r.status = 'open' AND NEW.item_no <= r.item_count) THEN
        RAISE EXCEPTION 'brant_pur.allocation_review_item: review % is not open or has no item %', NEW.review_id, NEW.item_no
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.disposition IS NOT NULL THEN
            RAISE EXCEPTION 'brant_pur.allocation_review_item: an item is listed before it is disposed of'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.disposition IS NOT NULL OR NEW.disposition IS NULL OR (to_jsonb(NEW) - v_disposed) IS DISTINCT FROM (to_jsonb(OLD) - v_disposed) THEN
        RAISE EXCEPTION 'brant_pur.allocation_review_item: item % of review % is only disposed of, once', OLD.item_no, OLD.review_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.disposition = 'draft_voided' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_draft d WHERE d.tenant_id = NEW.tenant_id
                AND d.company_id = NEW.company_id AND d.draft_id = NEW.disposition_draft_id AND d.status = 'voided'))
        OR (NEW.disposition = 'commitment_changed' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment_change x
                WHERE x.tenant_id = NEW.tenant_id AND x.company_id = NEW.company_id AND x.change_id = NEW.disposition_change_id
                AND x.commitment_id = NEW.disposition_commitment_id AND x.status = 'applied' AND x.applied_revision = NEW.disposition_revision))
        OR (NEW.disposition = 'commitment_cancelled' AND NOT EXISTS (SELECT 1 FROM brant_pur.purchase_commitment c
                WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.commitment_id = NEW.disposition_commitment_id
                AND c.status = 'cancelled' AND c.revision = NEW.disposition_revision)) THEN
        RAISE EXCEPTION 'brant_pur.allocation_review_item: the disposition of item % of review % has not happened', NEW.item_no, NEW.review_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER allocation_review_item_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_pur.allocation_review_item
    FOR EACH ROW EXECUTE FUNCTION brant_pur.guard_allocation_review_item();

-- At commit: a review has exactly its item_count items, numbered from 1; every item has a disposition
-- exactly when the review is settled.
CREATE FUNCTION brant_pur.check_allocation_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_review   record;
    v_items    bigint;
    v_disposed bigint;
BEGIN
    SELECT r.status, r.item_count INTO v_review FROM brant_pur.allocation_review r
    WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.review_id = NEW.review_id;
    SELECT count(*), count(*) FILTER (WHERE i.disposition IS NOT NULL) INTO v_items, v_disposed FROM brant_pur.allocation_review_item i
    WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.review_id = NEW.review_id;
    IF v_items <> v_review.item_count OR v_disposed <> (CASE WHEN v_review.status = 'settled' THEN v_items ELSE 0 END) THEN
        RAISE EXCEPTION 'brant_pur.allocation_review: review % (%) has % of % items, % disposed of', NEW.review_id, v_review.status, v_items,
            v_review.item_count, v_disposed USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER allocation_review_whole AFTER INSERT OR UPDATE ON brant_pur.allocation_review
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_allocation_review();
CREATE CONSTRAINT TRIGGER allocation_review_item_whole AFTER INSERT OR UPDATE ON brant_pur.allocation_review_item
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_pur.check_allocation_review();

ALTER TABLE brant_pur.allocation_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.allocation_review FORCE ROW LEVEL SECURITY;
CREATE POLICY allocation_review_scope ON brant_pur.allocation_review
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_pur.allocation_review_item ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_pur.allocation_review_item FORCE ROW LEVEL SECURITY;
CREATE POLICY allocation_review_item_scope ON brant_pur.allocation_review_item
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
