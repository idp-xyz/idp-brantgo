-- QLT: the frozen quantity of a revoked release, its release again after a re-inspection, and
-- approved concessions (S5R unit R5, WBS S5R-05; decision S5R-D18; tactical model
-- QLT-AGG-QualityRelease with ReleaseRevocation, FrozenQuantity and ConcessionFlag, SM-QLT-RELEASE,
-- QLT-INV-001, QLT-INV-002; Context Map R79 FUL->QLT "ReleaseConsumption v1"; BT21).
--
-- A revocation records what FUL had shipped on the release (shipped_quantity, read through FUL's
-- ReleaseConsumption v1 after the release row was locked) and the frozen quantity, the rest. A
-- revoked release counts against its subject in full until every inspection it was judged on has
-- been inspected again after the revocation — superseded, and the inspection valid now at the end of
-- its chain of supersessions recorded at or after revoked_at — and from then on only its shipped
-- quantity. Revocations recorded before this migration know no shipped quantity and count in full.
--
-- A disposition's concession item (accepted on the customer's consent, which the disposition cites
-- as evidence) counts next to the conforming quantity while its inspection is valid: a release is at
-- most the conforming quantity of its subject's valid inspections at its specification plus their
-- approved concessions (conceded) less what the releases before count (released_before). The part
-- of a release beyond the conforming quantity left is conceded goods (concession_quantity; the
-- release carries the concession flag when it is positive). Releases before this migration conceded
-- nothing.

ALTER TABLE brant_qlt.quality_release
    ADD COLUMN conceded            numeric(18,6) NOT NULL DEFAULT 0 CHECK (conceded >= 0),
    ADD COLUMN concession_quantity numeric(18,6) NOT NULL DEFAULT 0 CHECK (concession_quantity >= 0),
    ADD COLUMN shipped_quantity    numeric(18,6) CHECK (shipped_quantity >= 0),
    ADD COLUMN frozen_quantity     numeric(18,6) CHECK (frozen_quantity >= 0);
ALTER TABLE brant_qlt.quality_release ALTER COLUMN conceded DROP DEFAULT, ALTER COLUMN concession_quantity DROP DEFAULT;

-- QLT-INV-001 with concessions (replaces quantity <= conforming - released_before).
ALTER TABLE brant_qlt.quality_release DROP CONSTRAINT quality_release_check;
ALTER TABLE brant_qlt.quality_release ADD CONSTRAINT quality_release_within_releasable
    CHECK (quantity <= conforming + conceded - released_before);
ALTER TABLE brant_qlt.quality_release ADD CONSTRAINT quality_release_concession
    CHECK (concession_quantity = GREATEST(0, quantity - GREATEST(0, conforming - released_before)));
-- QLT-INV-002: the frozen quantity is the release's quantity less what FUL shipped on it, recorded
-- with the revocation.
ALTER TABLE brant_qlt.quality_release ADD CONSTRAINT quality_release_frozen
    CHECK ((shipped_quantity IS NULL) = (frozen_quantity IS NULL) AND (shipped_quantity IS NULL
        OR (status = 'revoked' AND shipped_quantity <= quantity AND frozen_quantity = quantity - shipped_quantity)));

-- Whether every inspection among p_ids has been inspected again since p_at: it is superseded, and
-- the inspection valid now at the end of its chain of supersessions was recorded at or after p_at.
-- An empty list is not (fail closed).
CREATE FUNCTION brant_qlt.reinspected_since(p_tenant text, p_company text, p_ids text[], p_at timestamptz) RETURNS boolean
    LANGUAGE sql STABLE
AS $$
    WITH RECURSIVE chain (start_id, inspection_id, status, superseded_by, recorded_at) AS (
        SELECT i.inspection_id, i.inspection_id, i.status, i.superseded_by_inspection_id, i.recorded_at
        FROM brant_qlt.inspection i
        WHERE i.tenant_id = p_tenant AND i.company_id = p_company AND i.inspection_id = ANY (p_ids) AND i.status = 'superseded'
        UNION ALL
        SELECT c.start_id, n.inspection_id, n.status, n.superseded_by_inspection_id, n.recorded_at
        FROM chain c
        JOIN brant_qlt.inspection n ON n.tenant_id = p_tenant AND n.company_id = p_company AND n.inspection_id = c.superseded_by
    )
    SELECT pg_catalog.cardinality(p_ids) > 0 AND (SELECT pg_catalog.count(DISTINCT c.start_id) FROM chain c
        WHERE c.status <> 'superseded' AND c.recorded_at >= p_at) = pg_catalog.cardinality(p_ids)
$$;

-- A release is inserted granted, on exactly the conforming quantity of its subject's valid
-- inspections at its specification, their approved concessions and what the releases at it before
-- count — a revoked release in full until its frozen quantity was inspected again, then its shipped
-- quantity — as the rows stand in the granting transaction (QLT-INV-001); it changes only by its
-- revocation, once, which records the shipped and frozen quantities, and nothing else about it
-- changes; it is never deleted.
CREATE OR REPLACE FUNCTION brant_qlt.guard_quality_release() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_revoke     text[] := ARRAY['status', 'row_version', 'revoked_by_kind', 'revoked_by_id', 'revoke_epoch', 'revoked_at', 'revoke_reason',
                                 'revoke_evidence_ids', 'shipped_quantity', 'frozen_quantity'];
    v_conforming numeric;
    v_conceded   numeric;
    v_ids        text[];
    v_released   numeric;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_qlt.quality_release: releases are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        SELECT coalesce(sum(i.conforming), 0), coalesce(array_agg(i.inspection_id ORDER BY i.inspection_id COLLATE "C"), '{}')
            INTO v_conforming, v_ids FROM brant_qlt.inspection i
        WHERE i.tenant_id = NEW.tenant_id AND i.company_id = NEW.company_id AND i.subject_kind = NEW.subject_kind AND i.subject_id = NEW.subject_id
            AND i.subject_line_id = NEW.subject_line_id AND i.status <> 'superseded' AND i.sku_id = NEW.sku_id
            AND i.spec_revision = NEW.spec_revision AND i.unit_id = NEW.unit_id AND i.unit_revision = NEW.unit_revision;
        SELECT coalesce(sum(d.quantity), 0) INTO v_conceded FROM brant_qlt.disposition_item d
        JOIN brant_qlt.inspection i ON i.tenant_id = d.tenant_id AND i.company_id = d.company_id AND i.inspection_id = d.inspection_id
        WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id AND d.action = 'concession' AND i.subject_kind = NEW.subject_kind
            AND i.subject_id = NEW.subject_id AND i.subject_line_id = NEW.subject_line_id AND i.status <> 'superseded' AND i.sku_id = NEW.sku_id
            AND i.spec_revision = NEW.spec_revision AND i.unit_id = NEW.unit_id AND i.unit_revision = NEW.unit_revision;
        SELECT coalesce(sum(CASE WHEN r.status = 'revoked' AND r.shipped_quantity IS NOT NULL
                    AND brant_qlt.reinspected_since(r.tenant_id, r.company_id, r.inspection_ids, r.revoked_at)
                THEN r.shipped_quantity ELSE r.quantity END), 0) INTO v_released
        FROM brant_qlt.quality_release r
        WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.subject_kind = NEW.subject_kind AND r.subject_id = NEW.subject_id
            AND r.subject_line_id = NEW.subject_line_id AND r.sku_id = NEW.sku_id AND r.spec_revision = NEW.spec_revision
            AND r.unit_id = NEW.unit_id AND r.unit_revision = NEW.unit_revision;
        IF NEW.status <> 'granted' OR NEW.conforming <> v_conforming OR NEW.conceded <> v_conceded OR NEW.released_before <> v_released
                OR NEW.inspection_ids <> v_ids OR NEW.shipped_quantity IS NOT NULL THEN
            RAISE EXCEPTION 'brant_qlt.quality_release: release % is judged on conforming %, conceded % and released % of inspections %, the rows say %, %, % and % (QLT-INV-001)',
                NEW.release_id, NEW.conforming, NEW.conceded, NEW.released_before, NEW.inspection_ids, v_conforming, v_conceded, v_released, v_ids
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status = 'granted' AND NEW.status = 'revoked' AND NEW.shipped_quantity IS NOT NULL
            AND (to_jsonb(NEW) - v_revoke) IS NOT DISTINCT FROM (to_jsonb(OLD) - v_revoke) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_qlt.quality_release: release % is never modified; it is only revoked, once, with what FUL shipped on it (SM-QLT-RELEASE)',
        OLD.release_id USING ERRCODE = 'integrity_constraint_violation';
END
$$;
