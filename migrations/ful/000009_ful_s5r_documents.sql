-- FUL: S5R unit R6 (WBS S5R-06) — the simulated decisions of 2026-11-13 (E0) on FUL's insurance,
-- exceptions, documents, service roles and parties: S5R-D09, D11, D28, D29, D40 and the QLT
-- concession flag of S5R-D18 shown by FUL (tactical model FUL-AGG-Shipment, FUL-AGG-MilestonePlan,
-- FUL-AGG-CommercialRelease, SM-FUL-RELEASE, FUL-INV-002; Context Map R26, R44, R49; role templates'
-- independence row of commercial_release.grant_exception). Rows written before this migration keep
-- what they say: the new rules hold for rows inserted from now on (NOT VALID constraints and insert
-- triggers); every table here only ever gets inserts.

-- S5R-D11: a requested booking is a node of the plan (booking_requested, its planned date the
-- deadline for the request before the cut-off), paired with booking_confirmed. A booking record still
-- records a confirmed booking only.
ALTER TABLE brant_ful.milestone_node DROP CONSTRAINT milestone_node_node_type_check;
ALTER TABLE brant_ful.milestone_node ADD CONSTRAINT milestone_node_node_type_check CHECK (node_type IN ('material_ready', 'production_start',
    'production_complete', 'inspection', 'packing_complete', 'supplier_handover', 'booking_requested', 'booking_confirmed', 'cargo_loaded',
    'customs_release', 'actual_departure', 'arrival', 'delivery', 'customer_acceptance'));

-- S5R-D40: the parties a shipment binds and its confirmation screens (R44) include the order's notify
-- party (role notify, at its address, as the order revision sealed it) and the end user the shipment
-- states (role end_user, a PTY party at a confirmed revision, no address); the customer and the end
-- user are the parties without an address. A shipment names at most one of each.
ALTER TABLE brant_ful.shipment_party DROP CONSTRAINT shipment_party_check;
ALTER TABLE brant_ful.shipment_party ADD CONSTRAINT shipment_party_address_by_role CHECK ((role IN ('customer', 'end_user')) = (address_id IS NULL));
CREATE UNIQUE INDEX shipment_party_one_notify ON brant_ful.shipment_party (tenant_id, company_id, shipment_id) WHERE role = 'notify';
CREATE UNIQUE INDEX shipment_party_one_end_user ON brant_ful.shipment_party (tenant_id, company_id, shipment_id) WHERE role = 'end_user';

-- S5R-D18 (shown by FUL, unit R6): a shipment line records whether its quality release was granted on
-- an approved concession (QLT's QualityReleaseRef v1 when the shipment was prepared; the flag of a
-- release never changes after the grant). Lines before this migration say false.
ALTER TABLE brant_ful.shipment_line ADD COLUMN quality_concession boolean NOT NULL DEFAULT false;

-- S5R-D29: commercial_release.evaluate is a human's or an AI's (B0 has no system re-evaluation).
-- Evaluations recorded before keep their actor.
ALTER TABLE brant_ful.commercial_release_evaluation ADD CONSTRAINT commercial_release_evaluation_actor_s5r
    CHECK (evaluated_by_kind IN ('human', 'ai')) NOT VALID;

-- S5R-D09: an exception names the POL credit_policy revision in force when it was granted, with its
-- content hash and decision_validity_days, and runs no longer than those days (FUL's 90 days stay the
-- cap). Exceptions granted before name none.
ALTER TABLE brant_ful.commercial_release_exception
    ADD COLUMN credit_policy_id       text    CHECK (credit_policy_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN credit_policy_revision bigint  CHECK (credit_policy_revision > 0),
    ADD COLUMN credit_policy_hash     text    CHECK (credit_policy_hash ~ '^[0-9a-f]{64}$'),
    ADD COLUMN decision_validity_days integer CHECK (decision_validity_days BETWEEN 1 AND 3650),
    ADD CONSTRAINT commercial_release_exception_credit_policy CHECK (
        num_nulls(credit_policy_id, credit_policy_revision, credit_policy_hash, decision_validity_days) IN (0, 4)
        AND (decision_validity_days IS NULL OR valid_until <= granted_at + make_interval(days => decision_validity_days)));
ALTER TABLE brant_ful.commercial_release_exception ADD CONSTRAINT commercial_release_exception_credit_policy_s5r
    CHECK (credit_policy_id IS NOT NULL) NOT VALID;

-- S5R-D28: an exception is granted by someone independent of the release's evaluators.
CREATE FUNCTION brant_ful.guard_commercial_release_exception_independent() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM brant_ful.commercial_release_evaluation e
               WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.release_id = NEW.release_id
                   AND e.evaluated_by_kind = NEW.granted_by_kind AND e.evaluated_by_id = NEW.granted_by_id) THEN
        RAISE EXCEPTION 'brant_ful.commercial_release_exception: % evaluated release %; its exception is granted by someone independent (S5R-D28)',
            NEW.granted_by_id, NEW.release_id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER commercial_release_exception_independent BEFORE INSERT ON brant_ful.commercial_release_exception
    FOR EACH ROW EXECUTE FUNCTION brant_ful.guard_commercial_release_exception_independent();
