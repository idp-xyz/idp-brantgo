-- FUL: the documents release of a confirmed shipment after an order change (owner decision
-- 2026-11-12, S5 unit C1b, a follow-up of WBS S5-07; tactical model FUL-AGG-CommercialRelease,
-- SM-FUL-RELEASE, FUL-INV-002; FUL-AGG-OrderFulfilment entity ShipmentReview,
-- SM-FUL-SHIPMENT-REVIEW; Context Map R27 TRD->FUL, R45 STL->FUL; WF-07, BT10, D-5, D-11).
--
-- The documents release of a confirmed shipment judges the payment condition on the order's current
-- revision (STL's PaymentConditionGuard v1 for the documents scope on the revision and hash TRD
-- shows now) and reuses the confirmation's verdicts on the goods; an order ended by order.close or
-- order.close_remainder still releases them. The release keeps binding the order revision its
-- shipment binds; what a judgement saw is in its bindings and guard evidence.
--
-- An exception of such a release is granted on the revision the payment condition was judged on,
-- which may be later than the one the release binds: the exception now records that revision's
-- order hash (exceptions before this migration bind the release's order and keep none), and its
-- guard admits a later revision only for the documents release of a confirmed shipment.
--
-- A shipment review's release item (a confirmed shipment's granted documents release judged on an
-- earlier revision) is settled by evaluating the release again on the current revision, or by
-- document_release.record, which judges the same way, after the review opened (reevaluated);
-- no_action, which left such a release as it was granted, is no longer accepted. Items disposed of
-- before this migration keep their dispositions.

ALTER TABLE brant_ful.commercial_release_exception ADD COLUMN order_hash text CHECK (order_hash ~ '^[0-9a-f]{64}$');

-- Evaluations and exceptions are only inserted, each at the row version it gave its release: an
-- evaluation with the state it left, an exception granting the release on the order revision the
-- payment condition was judged on — the release's own, or, for the documents release of a confirmed
-- shipment, a later one — with that revision's order hash.
CREATE OR REPLACE FUNCTION brant_ful.guard_commercial_release_part() RETURNS trigger LANGUAGE plpgsql AS $$
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
    ELSIF v_release.status <> 'granted' OR v_release.exceptions <> NEW.seq OR v_release.order_id <> NEW.order_id OR NEW.order_hash IS NULL
            OR NOT ((NEW.order_revision = v_release.order_revision AND NEW.order_hash = v_release.order_hash)
                OR (NEW.order_revision > v_release.order_revision AND v_release.scope = 'documents' AND EXISTS (
                    SELECT 1 FROM brant_ful.shipment s WHERE s.tenant_id = v_release.tenant_id AND s.company_id = v_release.company_id
                    AND s.shipment_id = v_release.shipment_id AND s.status = 'confirmed'))) THEN
        RAISE EXCEPTION 'brant_ful.commercial_release_exception: exception % does not grant release % on its order', NEW.seq, NEW.release_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

-- An item is inserted with an open review, without a disposition; it then changes only by its
-- disposition, once, while its review is open, and only to what happened in FUL's rows. A release
-- item is reevaluated when its release was evaluated again, or consumed, since the review opened.
CREATE OR REPLACE FUNCTION brant_ful.guard_shipment_review_item() RETURNS trigger LANGUAGE plpgsql AS $$
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
    IF NEW.disposition = 'reevaluated' AND NEW.kind = 'release' AND EXISTS (SELECT 1 FROM brant_ful.commercial_release c
            WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id AND c.release_id = NEW.release_id
            AND (c.evaluations > NEW.release_evaluations OR (c.status = 'consumed' AND c.consumed_at >= v_review.opened_at))) THEN
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
