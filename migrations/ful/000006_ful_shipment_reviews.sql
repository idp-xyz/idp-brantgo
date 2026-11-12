-- FUL: shipment reviews after an order change (tactical model FUL-AGG-OrderFulfilment entity
-- ShipmentReview, SM-FUL-SHIPMENT-REVIEW; WF-06 step 5 with its idempotency and failure mode; event
-- storming PL-09; S5 decisions D-18, D-21; baseline BT10; WBS S5-09b, FUL part).
--
-- TRD's OrderAmended, OrderCancelled and OrderRemainderClosed reach FUL's Inbox consumer
-- ful.shipment_review, which opens, as its system actor, one review per (order, order revision,
-- cause) under the order's fulfilment root: its items are what the change leaves stale of FUL's facts
-- of the order — prepared shipments that can no longer be confirmed, their current packing records,
-- bookings and draft documents, and granted releases nothing can consume any more. Confirmed
-- shipments are never items. A review is inserted open with its items and changes once: settled by a
-- person (every item disposed, each with a reason) or superseded by the review of a later version of
-- the same order. An item changes only by its disposition, once, while its review is open, and only to
-- what happened in FUL's rows: its shipment cancelled; the shipment cancelled and replaced by a shipment
-- of the same order bound to the review's revision or a later one, not cancelled, holding the item's
-- counterpart (a packing record of the same kind, a booking, a version of the same document kind, a
-- release of the same scope); its release evaluated again; or, for a release of a confirmed shipment,
-- no action. At most one review of an order is open. Nothing is deleted.

CREATE TABLE brant_ful.shipment_review (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    review_id        text        NOT NULL CHECK (review_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    order_id         text        NOT NULL,
    order_revision   bigint      NOT NULL CHECK (order_revision > 0),
    order_hash       text        NOT NULL CHECK (order_hash ~ '^[0-9a-f]{64}$'),
    -- TRD's row version of the order after the event: reviews of an order follow it
    order_version    bigint      NOT NULL CHECK (order_version > 0),
    cause            text        NOT NULL CHECK (cause IN ('order_amended', 'order_cancelled', 'order_remainder_closed')),
    event_id         text        NOT NULL CHECK (event_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status           text        NOT NULL CHECK (status IN ('open', 'settled', 'superseded')),
    row_version      bigint      NOT NULL CHECK (row_version IN (1, 2)),
    responsible_role text        NOT NULL CHECK (responsible_role = 'merchandiser'),
    item_count       integer     NOT NULL CHECK (item_count BETWEEN 0 AND 1000),
    opened_by_kind   text        NOT NULL CHECK (opened_by_kind = 'system'),
    opened_by_id     text        NOT NULL,
    open_epoch       bigint      NOT NULL CHECK (open_epoch > 0),
    opened_at        timestamptz NOT NULL,
    settled_by_kind  text,
    settled_by_id    text,
    settle_epoch     bigint,
    settled_at       timestamptz,
    settle_reason    text CHECK (char_length(settle_reason) BETWEEN 1 AND 1000),
    superseded_by    text,
    superseded_at    timestamptz,
    PRIMARY KEY (tenant_id, company_id, review_id),
    -- one review per order revision and cause, one per event (WF-06 idempotency)
    UNIQUE (tenant_id, company_id, order_id, order_revision, cause),
    UNIQUE (tenant_id, company_id, event_id),
    FOREIGN KEY (tenant_id, company_id, order_id) REFERENCES brant_ful.order_fulfilment (tenant_id, company_id, order_id),
    CHECK (num_nulls(settled_by_kind, settled_by_id, settle_epoch, settled_at) IN (0, 4)),
    CHECK (settled_at IS NULL OR (settled_by_kind = 'human' AND settle_epoch > 0 AND settled_at >= opened_at)),
    CHECK (settle_reason IS NULL OR settled_at IS NOT NULL),
    CHECK (num_nulls(superseded_by, superseded_at) IN (0, 2)),
    CHECK (superseded_at IS NULL OR (superseded_at >= opened_at AND superseded_by <> review_id)),
    CHECK (status = CASE WHEN settled_at IS NOT NULL THEN 'settled' WHEN superseded_at IS NOT NULL THEN 'superseded' ELSE 'open' END),
    CHECK (settled_at IS NULL OR superseded_at IS NULL),
    CHECK (row_version = CASE status WHEN 'open' THEN 1 ELSE 2 END),
    -- At most one review of an order is open at commit: a review supersedes the open ones as it opens
    -- (it is inserted first, since a supersession names it).
    CONSTRAINT shipment_review_one_open EXCLUDE USING btree (tenant_id WITH =, company_id WITH =, order_id WITH =) WHERE (status = 'open')
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE brant_ful.shipment_review_item (
    tenant_id               text        NOT NULL,
    company_id              text        NOT NULL,
    review_id               text        NOT NULL,
    item_no                 integer     NOT NULL CHECK (item_no BETWEEN 1 AND 1000),
    kind                    text        NOT NULL CHECK (kind IN ('shipment', 'packing', 'booking', 'document', 'release')),
    -- the shipment the item hangs on, as it was when the review opened
    shipment_id             text        NOT NULL,
    shipment_status         text        NOT NULL CHECK (shipment_status IN ('prepared', 'confirmed')),
    packing_kind            text        CHECK (packing_kind IN ('plan', 'actual')),
    document_kind           text        CHECK (document_kind ~ '^[a-z][a-z_]{0,63}$'),
    release_id              text,
    release_scope           text        CHECK (release_scope IN ('shipment', 'documents')),
    release_evaluations     bigint      CHECK (release_evaluations > 0),
    -- what FUL knew of the object when the review opened, as JSON, and its SHA-256
    facts                   text        NOT NULL CHECK (left(facts, 1) = '{' AND char_length(facts) <= 65536),
    facts_sha256            text        NOT NULL,
    disposition             text        CHECK (disposition IN ('shipment_cancelled', 'replaced', 'reevaluated', 'no_action')),
    disposition_shipment_id text,
    disposition_revision    bigint      CHECK (disposition_revision > 0),
    disposition_ref         text        CHECK (char_length(disposition_ref) BETWEEN 1 AND 200),
    disposition_status      text        CHECK (char_length(disposition_status) BETWEEN 1 AND 64),
    disposition_at          timestamptz,
    disposition_reason      text        CHECK (char_length(disposition_reason) BETWEEN 1 AND 1000),
    PRIMARY KEY (tenant_id, company_id, review_id, item_no),
    FOREIGN KEY (tenant_id, company_id, review_id) REFERENCES brant_ful.shipment_review (tenant_id, company_id, review_id),
    FOREIGN KEY (tenant_id, company_id, shipment_id) REFERENCES brant_ful.shipment (tenant_id, company_id, shipment_id),
    CHECK (facts_sha256 = encode(sha256(convert_to(facts, 'UTF8')), 'hex')),
    CHECK ((kind = 'packing') = (packing_kind IS NOT NULL)),
    CHECK ((kind = 'document') = (document_kind IS NOT NULL)),
    CHECK ((kind = 'release') = (num_nulls(release_id, release_scope, release_evaluations) = 0)),
    CHECK (num_nulls(release_id, release_scope, release_evaluations) IN (0, 3)),
    -- a confirmed shipment stays: only its releases are items
    CHECK (kind = 'release' OR shipment_status = 'prepared'),
    CHECK ((disposition IS NULL) = (disposition_reason IS NULL)),
    CHECK (disposition IS NOT NULL OR num_nulls(disposition_shipment_id, disposition_revision, disposition_ref, disposition_status,
        disposition_at) = 5),
    CHECK ((disposition IS NOT DISTINCT FROM 'replaced') = (disposition_shipment_id IS NOT NULL AND disposition_revision IS NOT NULL)),
    CHECK (disposition IS NULL OR disposition = 'no_action' OR disposition_at IS NOT NULL)
);

-- A review is inserted open and changes once: settled by a human or superseded by a review of a later
-- version of the same order (SM-FUL-SHIPMENT-REVIEW); nothing else of it ever changes, nothing is
-- deleted.
CREATE FUNCTION brant_ful.guard_shipment_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_settle    text[] := ARRAY['status', 'row_version', 'settled_by_kind', 'settled_by_id', 'settle_epoch', 'settled_at', 'settle_reason'];
    v_supersede text[] := ARRAY['status', 'row_version', 'superseded_by', 'superseded_at'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.shipment_review: reviews are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' THEN
            RAISE EXCEPTION 'brant_ful.shipment_review: a review is inserted open' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.status = 'settled'
            AND (to_jsonb(NEW) - v_settle) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_settle) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'open' AND NEW.status = 'superseded'
            AND (to_jsonb(NEW) - v_supersede) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_supersede)
            AND EXISTS (SELECT 1 FROM brant_ful.shipment_review r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.review_id = NEW.superseded_by AND r.order_id = NEW.order_id AND r.order_version > NEW.order_version) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_ful.shipment_review: review % changes only once, open to settled or to superseded by a later order version (SM-FUL-SHIPMENT-REVIEW)',
        OLD.review_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER shipment_review_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_review
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_review();

-- An item is inserted with an open review, without a disposition; it then changes only by its
-- disposition, once, while its review is open, and only to what happened in FUL's rows.
CREATE FUNCTION brant_ful.guard_shipment_review_item() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_dispose  text[] := ARRAY['disposition', 'disposition_shipment_id', 'disposition_revision', 'disposition_ref', 'disposition_status',
                               'disposition_at', 'disposition_reason'];
    v_review   record;
    v_shipment record;
    v_new      record;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_ful.shipment_review_item: review items are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT r.* INTO v_review FROM brant_ful.shipment_review r
    WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.review_id = NEW.review_id;
    IF v_review.status IS DISTINCT FROM 'open' THEN
        RAISE EXCEPTION 'brant_ful.shipment_review_item: review % is not open', NEW.review_id USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.disposition IS NOT NULL THEN
            RAISE EXCEPTION 'brant_ful.shipment_review_item: an item is inserted without a disposition'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.disposition IS NOT NULL OR NEW.disposition IS NULL
            OR (to_jsonb(NEW) - v_dispose) IS DISTINCT FROM (to_jsonb(OLD) - v_dispose) THEN
        RAISE EXCEPTION 'brant_ful.shipment_review_item: item % of review % changes only by its disposition, once', OLD.item_no, OLD.review_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT s.* INTO v_shipment FROM brant_ful.shipment s
    WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.shipment_id = NEW.shipment_id;
    IF NEW.disposition = 'no_action' AND NEW.kind = 'release' AND NEW.shipment_status = 'confirmed' THEN
        RETURN NEW;
    END IF;
    IF NEW.disposition = 'reevaluated' AND NEW.kind = 'release' AND EXISTS (SELECT 1 FROM brant_ful.commercial_release c
            WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.release_id = NEW.release_id
            AND c.evaluations > NEW.release_evaluations) THEN
        RETURN NEW;
    END IF;
    IF NEW.disposition = 'shipment_cancelled' AND NEW.shipment_status = 'prepared' AND v_shipment.status = 'cancelled' THEN
        RETURN NEW;
    END IF;
    IF NEW.disposition = 'replaced' AND NEW.shipment_status = 'prepared' AND v_shipment.status = 'cancelled'
            AND v_review.cause = 'order_amended' AND NEW.disposition_shipment_id <> NEW.shipment_id THEN
        SELECT s.* INTO v_new FROM brant_ful.shipment s
        WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id AND s.shipment_id = NEW.disposition_shipment_id;
        IF v_new.order_id = v_review.order_id AND v_new.order_revision >= v_review.order_revision AND v_new.status <> 'cancelled'
                AND v_new.order_revision = NEW.disposition_revision
                AND (CASE NEW.kind
                    WHEN 'shipment' THEN true
                    WHEN 'packing' THEN EXISTS (SELECT 1 FROM brant_ful.shipment_packing p WHERE p.tenant_id = NEW.tenant_id
                        AND p.company_id = NEW.company_id AND p.shipment_id = v_new.shipment_id AND p.kind = NEW.packing_kind)
                    WHEN 'booking' THEN EXISTS (SELECT 1 FROM brant_ful.shipment_booking b WHERE b.tenant_id = NEW.tenant_id
                        AND b.company_id = NEW.company_id AND b.shipment_id = v_new.shipment_id)
                    WHEN 'document' THEN EXISTS (SELECT 1 FROM brant_ful.shipment_document d JOIN brant_ful.shipment_document_set x
                        ON x.tenant_id = d.tenant_id AND x.company_id = d.company_id AND x.document_set_id = d.document_set_id
                        WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND x.shipment_id = v_new.shipment_id
                        AND d.document_kind = NEW.document_kind)
                    WHEN 'release' THEN EXISTS (SELECT 1 FROM brant_ful.commercial_release c WHERE c.tenant_id = NEW.tenant_id
                        AND c.company_id = NEW.company_id AND c.shipment_id = v_new.shipment_id AND c.scope = NEW.release_scope)
                    ELSE false END) THEN
            RETURN NEW;
        END IF;
    END IF;
    RAISE EXCEPTION 'brant_ful.shipment_review_item: item % of review % is disposed of as %, which did not happen', NEW.item_no, NEW.review_id,
        NEW.disposition USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER shipment_review_item_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_ful.shipment_review_item
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_shipment_review_item();

-- At commit: a review holds exactly item_count items; it is settled exactly when every item is
-- disposed of, and an open or superseded review has no disposed item.
CREATE FUNCTION brant_ful.check_shipment_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_review   record;
    v_items    bigint;
    v_disposed bigint;
BEGIN
    SELECT r.* INTO v_review FROM brant_ful.shipment_review r
    WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.review_id = NEW.review_id;
    SELECT count(*), count(i.disposition) INTO v_items, v_disposed FROM brant_ful.shipment_review_item i
    WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.review_id = NEW.review_id;
    IF v_items <> v_review.item_count OR (v_review.status = 'settled') <> (v_disposed = v_items) OR (v_review.status <> 'settled' AND v_disposed > 0) THEN
        RAISE EXCEPTION 'brant_ful.shipment_review: review % (%) counts % items, has % with % disposed', v_review.review_id, v_review.status,
            v_review.item_count, v_items, v_disposed USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER shipment_review_at_commit AFTER INSERT OR UPDATE ON brant_ful.shipment_review
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_review();
CREATE CONSTRAINT TRIGGER shipment_review_item_at_commit AFTER INSERT OR UPDATE ON brant_ful.shipment_review_item
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION brant_ful.check_shipment_review();

ALTER TABLE brant_ful.shipment_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_review FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_review_scope ON brant_ful.shipment_review
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_ful.shipment_review_item ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_ful.shipment_review_item FORCE ROW LEVEL SECURITY;
CREATE POLICY shipment_review_item_scope ON brant_ful.shipment_review_item
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
