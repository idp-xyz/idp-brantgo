-- CST: versioned cost BOMs (WBS S7-02; design CST-AGG-CostBOM).
--
-- A cost BOM is (tenant, company, bom_id). One draft revision is saved whole. A human who did not
-- draft it verifies the revision and freezes content_sha256. Withdraw ends that revision. Nothing
-- is deleted. Money is integer minor units. Quantities and rates are exact decimal strings, never
-- floats. A size-graded line stores one consumption row per size it claims; the guard never
-- substitutes the base size.
--
-- brant_cst.guard_cost_bom_current_v1 locks the root and the named revision FOR SHARE. Draft,
-- withdrawn, superseded, or a revision with a required parameter still unmeasured is BOM_UNVERIFIED.
-- A hash that is not the frozen hash is not current. A size the revision does not store is
-- BOM_SIZE_GRADE_MISSING. The function also reads cost_bom_substitution (created in cst/000005,
-- which is applied before any call): a pending request, or a named substitution that does not
-- cover this revision, is BOM_SUBSTITUTION_UNAPPROVED.
--
-- SECURITY DEFINER, fixed search_path, REVOKE FROM PUBLIC. FORCE RLS still confines the function
-- to app.tenant_id and app.company_id.

CREATE TABLE brant_cst.cost_bom (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    bom_id           text        NOT NULL CHECK (bom_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version     bigint      NOT NULL CHECK (root_version > 0),
    last_revision    bigint      NOT NULL CHECK (last_revision > 0),
    active_revision  bigint      CHECK (active_revision > 0),
    status           text        NOT NULL CHECK (status IN ('draft', 'verified', 'withdrawn')),
    document_profile text        NOT NULL CHECK (document_profile IN ('generic', 'apparel')),
    base_size_code   text        NOT NULL CHECK (base_size_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    item_id          text        CHECK (item_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id           text        CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    spec_revision    bigint      CHECK (spec_revision > 0),
    owner_actor_id   text        NOT NULL,
    owner_team_id    text,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, bom_id),
    CHECK (item_id IS NOT NULL OR sku_id IS NOT NULL),
    CHECK ((sku_id IS NULL) = (spec_revision IS NULL)),
    CHECK ((status = 'draft') = (active_revision IS NULL))
);

CREATE TABLE brant_cst.cost_bom_revision (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    bom_id           text        NOT NULL,
    revision         bigint      NOT NULL CHECK (revision > 0),
    status           text        NOT NULL CHECK (status IN ('draft', 'verified', 'superseded', 'withdrawn')),
    content_sha256   text        CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    contributors     text[]      NOT NULL CHECK (cardinality(contributors) >= 1),
    saved_by_kind    text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id      text        NOT NULL,
    saved_epoch      bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at         timestamptz NOT NULL,
    verified_by_kind text        CHECK (verified_by_kind = 'human'),
    verified_by_id   text,
    verify_epoch     bigint      CHECK (verify_epoch > 0),
    verified_at      timestamptz,
    superseded_at    timestamptz,
    ended_by_kind    text        CHECK (ended_by_kind = 'human'),
    ended_by_id      text,
    end_epoch        bigint      CHECK (end_epoch > 0),
    ended_at         timestamptz,
    end_reason       text        CHECK (char_length(end_reason) BETWEEN 1 AND 1000),
    PRIMARY KEY (tenant_id, company_id, bom_id, revision),
    FOREIGN KEY (tenant_id, company_id, bom_id) REFERENCES brant_cst.cost_bom (tenant_id, company_id, bom_id),
    CHECK ((status = 'draft') = (content_sha256 IS NULL)),
    CHECK ((status = 'draft') = (verified_by_id IS NULL)),
    CHECK (status = 'draft' OR (verified_by_kind = 'human' AND verified_by_id IS NOT NULL AND verified_at IS NOT NULL
        AND NOT (verified_by_id = ANY (contributors)))),
    CHECK ((status = 'withdrawn') = (ended_at IS NOT NULL)),
    CHECK (ended_at IS NULL OR (ended_by_kind = 'human' AND ended_by_id IS NOT NULL AND end_reason IS NOT NULL)),
    CHECK (status <> 'superseded' OR superseded_at IS NOT NULL),
    CHECK (superseded_at IS NULL OR status IN ('superseded', 'withdrawn'))
);

CREATE UNIQUE INDEX cost_bom_revision_draft ON brant_cst.cost_bom_revision (tenant_id, company_id, bom_id)
    WHERE status = 'draft';

CREATE TABLE brant_cst.cost_bom_line (
    tenant_id             text    NOT NULL,
    company_id            text    NOT NULL,
    bom_id                text    NOT NULL,
    revision              bigint  NOT NULL,
    line_id               text    NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    material_kind         text    NOT NULL CHECK (material_kind IN ('shell_fabric', 'lining', 'interlining', 'trim', 'packaging', 'labor_cm')),
    material_code         text    NOT NULL CHECK (material_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    unit                  text    NOT NULL CHECK (unit ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    grading_mode          text    NOT NULL CHECK (grading_mode IN ('uniform', 'size_graded')),
    consumption_per_unit  text    NOT NULL CHECK (consumption_per_unit ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    wastage_ratio         text    NOT NULL CHECK (wastage_ratio ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    shrinkage_factor      text    NOT NULL CHECK (shrinkage_factor ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    fabric_width_cm       text    CHECK (fabric_width_cm ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    fabric_gsm            text    CHECK (fabric_gsm ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    measured              boolean NOT NULL,
    formula_basis         text    NOT NULL CHECK (formula_basis IN ('direct_qty', 'width_length_m', 'gsm_weight_kg')),
    supply_kind           text    CHECK (supply_kind ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    supply_id             text    CHECK (supply_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    supply_revision       bigint  CHECK (supply_revision > 0),
    PRIMARY KEY (tenant_id, company_id, bom_id, revision, line_id),
    FOREIGN KEY (tenant_id, company_id, bom_id, revision)
        REFERENCES brant_cst.cost_bom_revision (tenant_id, company_id, bom_id, revision),
    CHECK ((supply_id IS NULL) = (supply_kind IS NULL) AND (supply_id IS NULL) = (supply_revision IS NULL))
);

CREATE TABLE brant_cst.cost_bom_size_grade (
    tenant_id            text   NOT NULL,
    company_id           text   NOT NULL,
    bom_id               text   NOT NULL,
    revision             bigint NOT NULL,
    line_id              text   NOT NULL,
    size_code            text   NOT NULL CHECK (size_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    consumption_per_unit text   NOT NULL CHECK (consumption_per_unit ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    size_cm_delta_minor  bigint,
    currency             text   CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units          smallint CHECK (minor_units BETWEEN 0 AND 4),
    PRIMARY KEY (tenant_id, company_id, bom_id, revision, line_id, size_code),
    FOREIGN KEY (tenant_id, company_id, bom_id, revision, line_id)
        REFERENCES brant_cst.cost_bom_line (tenant_id, company_id, bom_id, revision, line_id),
    CHECK ((size_cm_delta_minor IS NULL) = (currency IS NULL) AND (currency IS NULL) = (minor_units IS NULL))
);

CREATE TABLE brant_cst.cost_bom_surcharge (
    tenant_id       text   NOT NULL,
    company_id      text   NOT NULL,
    bom_id          text   NOT NULL,
    revision        bigint NOT NULL,
    surcharge_id    text   NOT NULL CHECK (surcharge_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    surcharge_kind  text   NOT NULL CHECK (surcharge_kind IN ('dye_lot_mcq', 'trim_moq_surcharge', 'sample_dev_fee', 'pattern_fee')),
    scope           text   NOT NULL CHECK (scope IN ('per_color', 'per_style', 'per_order')),
    color_code      text   CHECK (color_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    material_code   text   CHECK (material_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    mcq_threshold   text   CHECK (mcq_threshold ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    mcq_unit        text   CHECK (mcq_unit ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    amount_minor    bigint,
    currency        text   CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units     smallint CHECK (minor_units BETWEEN 0 AND 4),
    allocation      text   NOT NULL CHECK (allocation IN ('order_lump_sum', 'allocate_by_qty')),
    fee_category    text   CHECK (fee_category ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    PRIMARY KEY (tenant_id, company_id, bom_id, revision, surcharge_id),
    FOREIGN KEY (tenant_id, company_id, bom_id, revision)
        REFERENCES brant_cst.cost_bom_revision (tenant_id, company_id, bom_id, revision),
    CHECK ((mcq_threshold IS NULL) = (mcq_unit IS NULL)),
    CHECK ((amount_minor IS NULL) = (currency IS NULL) AND (currency IS NULL) = (minor_units IS NULL))
);

-- A draft's lines may be replaced. A sealed revision's lines are inserted once and then never change.
CREATE FUNCTION brant_cst.guard_cost_bom_child() RETURNS trigger
    LANGUAGE plpgsql AS $$
DECLARE
    parent_status text;
    parent_tenant text;
    parent_company text;
    parent_bom text;
    parent_rev bigint;
BEGIN
    IF TG_OP = 'DELETE' THEN
        parent_tenant := OLD.tenant_id;
        parent_company := OLD.company_id;
        parent_bom := OLD.bom_id;
        parent_rev := OLD.revision;
    ELSE
        parent_tenant := NEW.tenant_id;
        parent_company := NEW.company_id;
        parent_bom := NEW.bom_id;
        parent_rev := NEW.revision;
    END IF;
    SELECT status INTO parent_status FROM brant_cst.cost_bom_revision
        WHERE tenant_id = parent_tenant AND company_id = parent_company AND bom_id = parent_bom AND revision = parent_rev;
    IF TG_OP = 'UPDATE' OR (TG_OP = 'DELETE' AND parent_status IS DISTINCT FROM 'draft') THEN
        RAISE EXCEPTION '%: a sealed cost BOM line does not change', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' AND parent_status IS DISTINCT FROM 'draft' THEN
        RAISE EXCEPTION '%: lines are written only while the revision is a draft', TG_TABLE_NAME
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER cost_bom_line_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_bom_line
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_bom_child();
CREATE TRIGGER cost_bom_size_grade_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_bom_size_grade
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_bom_child();
CREATE TRIGGER cost_bom_surcharge_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_bom_surcharge
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_bom_child();

-- Sealed revisions keep their hash and verifier. Status moves draft to verified, verified to
-- superseded or withdrawn, and superseded to withdrawn.
CREATE FUNCTION brant_cst.guard_cost_bom_revision() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_revision: revisions are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        RETURN NEW;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.company_id IS DISTINCT FROM OLD.company_id
            OR NEW.bom_id IS DISTINCT FROM OLD.bom_id OR NEW.revision IS DISTINCT FROM OLD.revision THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_revision: identity does not change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (OLD.status = 'draft' AND NEW.status = 'draft')
            AND (NEW.contributors IS DISTINCT FROM OLD.contributors OR NEW.saved_by_id IS DISTINCT FROM OLD.saved_by_id
                OR NEW.saved_by_kind IS DISTINCT FROM OLD.saved_by_kind OR NEW.saved_epoch IS DISTINCT FROM OLD.saved_epoch
                OR NEW.saved_at IS DISTINCT FROM OLD.saved_at) THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_revision: drafters change only on a draft'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'verified' AND NEW.content_sha256 IS NOT NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'verified' AND NEW.status = 'superseded' AND NEW.content_sha256 IS NOT DISTINCT FROM OLD.content_sha256
            AND NEW.verified_by_id IS NOT DISTINCT FROM OLD.verified_by_id THEN
        RETURN NEW;
    END IF;
    IF OLD.status IN ('verified', 'superseded') AND NEW.status = 'withdrawn'
            AND NEW.content_sha256 IS NOT DISTINCT FROM OLD.content_sha256
            AND NEW.verified_by_id IS NOT DISTINCT FROM OLD.verified_by_id THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.content_sha256 IS NULL AND NEW.verified_by_id IS NULL THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cst.cost_bom_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;

CREATE TRIGGER cost_bom_revision_guard BEFORE UPDATE OR DELETE ON brant_cst.cost_bom_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_bom_revision();

ALTER TABLE brant_cst.cost_bom ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_bom FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_bom_scope ON brant_cst.cost_bom
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_bom_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_bom_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_bom_revision_scope ON brant_cst.cost_bom_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_bom_line ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_bom_line FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_bom_line_scope ON brant_cst.cost_bom_line
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_bom_size_grade ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_bom_size_grade FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_bom_size_grade_scope ON brant_cst.cost_bom_size_grade
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_bom_surcharge ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_bom_surcharge FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_bom_surcharge_scope ON brant_cst.cost_bom_surcharge
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- CostBOMGuard v1. One row always. known is false when the BOM or the revision is outside the
-- caller's company. Otherwise code is NULL and status is verified when the revision may be
-- adopted; BOM_UNVERIFIED, BOM_SIZE_GRADE_MISSING or BOM_SUBSTITUTION_UNAPPROVED otherwise; a
-- verified revision whose frozen hash is not p_content_sha256 answers status not_current.
CREATE FUNCTION brant_cst.guard_cost_bom_current_v1(
    p_bom_id text, p_revision bigint, p_content_sha256 text, p_size_code text, p_substitution_id text)
    RETURNS TABLE (known boolean, status text, code text, content_sha256 text, revision bigint, document_profile text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog
AS $$
DECLARE
    t constant text := pg_catalog.current_setting('app.tenant_id', true);
    c constant text := pg_catalog.current_setting('app.company_id', true);
    root_profile text;
    rev_status text;
    rev_hash text;
    rev_no bigint;
    unverified constant text := 'BOM_UNVERIFIED';
BEGIN
    IF p_bom_id IS NULL OR p_revision IS NULL THEN
        RAISE EXCEPTION 'brant_cst.guard_cost_bom_current_v1: bom and revision are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    SELECT x.document_profile INTO root_profile FROM brant_cst.cost_bom x
        WHERE x.tenant_id = t AND x.company_id = c AND x.bom_id = p_bom_id
        FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::text, NULL::text, NULL::bigint, NULL::text;
        RETURN;
    END IF;
    SELECT r.status, r.content_sha256, r.revision INTO rev_status, rev_hash, rev_no
        FROM brant_cst.cost_bom_revision r
        WHERE r.tenant_id = t AND r.company_id = c AND r.bom_id = p_bom_id AND r.revision = p_revision
        FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::text, NULL::text, NULL::bigint, NULL::text;
        RETURN;
    END IF;
    PERFORM 1 FROM brant_cst.cost_bom_line l
        WHERE l.tenant_id = t AND l.company_id = c AND l.bom_id = p_bom_id AND l.revision = p_revision
        ORDER BY l.line_id
        FOR SHARE;
    IF rev_status IS DISTINCT FROM 'verified' THEN
        RETURN QUERY SELECT true, rev_status, unverified, rev_hash, rev_no, root_profile;
        RETURN;
    END IF;
    IF p_content_sha256 IS NOT NULL AND rev_hash IS DISTINCT FROM p_content_sha256 THEN
        RETURN QUERY SELECT true, 'not_current'::text, NULL::text, rev_hash, rev_no, root_profile;
        RETURN;
    END IF;
    IF EXISTS (SELECT 1 FROM brant_cst.cost_bom_line l
            WHERE l.tenant_id = t AND l.company_id = c AND l.bom_id = p_bom_id AND l.revision = p_revision
              AND l.measured = false) THEN
        RETURN QUERY SELECT true, rev_status, unverified, rev_hash, rev_no, root_profile;
        RETURN;
    END IF;
    IF p_size_code IS NOT NULL AND p_size_code <> '' THEN
        PERFORM 1 FROM brant_cst.cost_bom_size_grade g
            WHERE g.tenant_id = t AND g.company_id = c AND g.bom_id = p_bom_id AND g.revision = p_revision
              AND g.size_code = p_size_code
            ORDER BY g.line_id
            FOR SHARE;
        IF EXISTS (SELECT 1 FROM brant_cst.cost_bom_line l
                WHERE l.tenant_id = t AND l.company_id = c AND l.bom_id = p_bom_id AND l.revision = p_revision
                  AND l.grading_mode = 'size_graded'
                  AND NOT EXISTS (SELECT 1 FROM brant_cst.cost_bom_size_grade g
                      WHERE g.tenant_id = l.tenant_id AND g.company_id = l.company_id AND g.bom_id = l.bom_id
                        AND g.revision = l.revision AND g.line_id = l.line_id AND g.size_code = p_size_code)) THEN
            RETURN QUERY SELECT true, rev_status, 'BOM_SIZE_GRADE_MISSING'::text, rev_hash, rev_no, root_profile;
            RETURN;
        END IF;
    END IF;
    -- cost_bom_substitution is created in the next migration. PL/pgSQL resolves it at call time,
    -- after that migration has committed. A pending request blocks adoption. A named substitution
    -- must be an approval of this revision.
    PERFORM 1 FROM brant_cst.cost_bom_substitution s
        WHERE s.tenant_id = t AND s.company_id = c AND s.bom_id = p_bom_id
        ORDER BY s.substitution_id
        FOR SHARE;
    IF EXISTS (SELECT 1 FROM brant_cst.cost_bom_substitution s
            WHERE s.tenant_id = t AND s.company_id = c AND s.bom_id = p_bom_id AND s.status = 'requested')
        OR (p_substitution_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM brant_cst.cost_bom_substitution s
            WHERE s.tenant_id = t AND s.company_id = c AND s.substitution_id = p_substitution_id
              AND s.bom_id = p_bom_id AND s.status = 'approved' AND s.decided_revision = p_revision)) THEN
        RETURN QUERY SELECT true, rev_status, 'BOM_SUBSTITUTION_UNAPPROVED'::text, rev_hash, rev_no, root_profile;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, 'verified'::text, NULL::text, rev_hash, rev_no, root_profile;
END
$$;

REVOKE ALL ON FUNCTION brant_cst.guard_cost_bom_current_v1(text, bigint, text, text, text) FROM PUBLIC;
