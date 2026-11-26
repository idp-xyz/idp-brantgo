-- CAT: the style matrix (CAT-AGG-Item). A style does not replace brant_cat.sku. It maps one
-- style revision's colour and size cells onto confirmed SKU specification revisions, and keeps the
-- apparel technical parameters on that revision (unit, source, measured). A draft is saved whole,
-- by a human or an AI agent. item.confirm publishes it as an immutable revision. Master data does
-- not require the apparel plugin to be active.
--
-- Decimals are the contract's plain decimal strings (kernel.ParseDecimal, QuantityFormat), never
-- floats. measured = false is an estimate and stays an estimate. A cell's SKU must already be a
-- confirmed specification revision (foreign key, CAT-INV-001). SkuGuard v1 is what a command calls
-- to require that revision to be the SKU's current one.

CREATE TABLE brant_cat.item (
    tenant_id        text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id       text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    item_id          text        NOT NULL CHECK (item_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    document_profile text        NOT NULL CHECK (document_profile IN ('generic', 'apparel')),
    style_code       text        NOT NULL CHECK (style_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version     bigint      NOT NULL CHECK (root_version > 0),
    last_revision    bigint      NOT NULL CHECK (last_revision > 0),
    current_revision bigint      NOT NULL CHECK (current_revision >= 0),
    owner_actor_id   text        NOT NULL,
    owner_team_id    text,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, item_id),
    CONSTRAINT item_style_code UNIQUE (tenant_id, company_id, style_code),
    CHECK (current_revision <= last_revision)
);

CREATE TABLE brant_cat.item_revision (
    tenant_id               text        NOT NULL,
    company_id              text        NOT NULL,
    item_id                 text        NOT NULL,
    revision                bigint      NOT NULL CHECK (revision > 0),
    status                  text        NOT NULL CHECK (status IN ('draft', 'confirmed', 'superseded')),
    content_hash            text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    fabric_composition      text        NOT NULL CHECK (char_length(fabric_composition) BETWEEN 1 AND 200),
    fabric_spec             text        CHECK (fabric_spec IS NULL OR char_length(fabric_spec) BETWEEN 1 AND 200),
    fabric_width_cm         text        NOT NULL CHECK (fabric_width_cm ~ '^[0-9]{1,12}(\.[0-9]{1,6})?$'),
    fabric_width_unit       text        NOT NULL CHECK (fabric_width_unit = 'cm'),
    fabric_width_measured   boolean     NOT NULL,
    fabric_width_source     text        NOT NULL CHECK (fabric_width_source ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    fabric_gsm              text        NOT NULL CHECK (fabric_gsm ~ '^[0-9]{1,12}(\.[0-9]{1,6})?$'),
    fabric_gsm_unit         text        NOT NULL CHECK (fabric_gsm_unit = 'g/m2'),
    fabric_gsm_measured     boolean     NOT NULL,
    fabric_gsm_source       text        NOT NULL CHECK (fabric_gsm_source ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    warp_shrinkage_pct      text        NOT NULL CHECK (warp_shrinkage_pct ~ '^[0-9]{1,12}(\.[0-9]{1,6})?$'),
    warp_shrinkage_unit     text        NOT NULL CHECK (warp_shrinkage_unit = 'pct'),
    warp_shrinkage_measured boolean     NOT NULL,
    warp_shrinkage_source   text        NOT NULL CHECK (warp_shrinkage_source ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    weft_shrinkage_pct      text        NOT NULL CHECK (weft_shrinkage_pct ~ '^[0-9]{1,12}(\.[0-9]{1,6})?$'),
    weft_shrinkage_unit     text        NOT NULL CHECK (weft_shrinkage_unit = 'pct'),
    weft_shrinkage_measured boolean     NOT NULL,
    weft_shrinkage_source   text        NOT NULL CHECK (weft_shrinkage_source ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    dye_lot_policy          text        NOT NULL CHECK (dye_lot_policy IN ('none', 'same_lot_per_color', 'same_lot_per_order')),
    size_chart_evidence_id  text        CHECK (size_chart_evidence_id IS NULL OR size_chart_evidence_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    saved_by_kind           text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id             text        NOT NULL,
    saved_epoch             bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at                timestamptz NOT NULL,
    confirmed_by_kind       text,
    confirmed_by_id         text,
    confirm_epoch           bigint,
    confirmed_at            timestamptz,
    evidence_ids            text[],
    superseded_at           timestamptz,
    PRIMARY KEY (tenant_id, company_id, item_id, revision),
    FOREIGN KEY (tenant_id, company_id, item_id) REFERENCES brant_cat.item (tenant_id, company_id, item_id),
    CHECK ((status = 'draft') = (confirmed_by_kind IS NULL AND confirmed_by_id IS NULL AND confirm_epoch IS NULL
        AND confirmed_at IS NULL AND evidence_ids IS NULL AND superseded_at IS NULL)),
    CHECK (status = 'draft' OR (confirmed_by_kind = 'human' AND confirmed_by_id IS NOT NULL AND confirm_epoch > 0
        AND confirmed_at IS NOT NULL AND cardinality(evidence_ids) BETWEEN 1 AND 200)),
    CHECK ((status = 'superseded') = (superseded_at IS NOT NULL))
);

CREATE UNIQUE INDEX item_revision_draft ON brant_cat.item_revision (tenant_id, company_id, item_id)
    WHERE status = 'draft';
CREATE UNIQUE INDEX item_revision_confirmed ON brant_cat.item_revision (tenant_id, company_id, item_id)
    WHERE status = 'confirmed';

CREATE TABLE brant_cat.item_sku_variant (
    tenant_id     text   NOT NULL,
    company_id    text   NOT NULL,
    item_id       text   NOT NULL,
    revision      bigint NOT NULL,
    color_code    text   NOT NULL CHECK (color_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    size_code     text   NOT NULL CHECK (size_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id        text   NOT NULL,
    spec_revision bigint NOT NULL CHECK (spec_revision > 0),
    PRIMARY KEY (tenant_id, company_id, item_id, revision, color_code, size_code),
    CONSTRAINT item_sku_variant_sku UNIQUE (tenant_id, company_id, item_id, revision, sku_id),
    FOREIGN KEY (tenant_id, company_id, item_id, revision)
        REFERENCES brant_cat.item_revision (tenant_id, company_id, item_id, revision),
    FOREIGN KEY (tenant_id, company_id, sku_id, spec_revision)
        REFERENCES brant_cat.sku_revision (tenant_id, company_id, sku_id, spec_revision)
);

CREATE FUNCTION brant_cat.guard_item() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.item: styles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.item_id, NEW.document_profile, NEW.style_code, NEW.owner_actor_id, NEW.owner_team_id, NEW.created_at)
            IS DISTINCT FROM
       (OLD.tenant_id, OLD.company_id, OLD.item_id, OLD.document_profile, OLD.style_code, OLD.owner_actor_id, OLD.owner_team_id, OLD.created_at)
        OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision OR NEW.current_revision < OLD.current_revision THEN
        RAISE EXCEPTION 'brant_cat.item: identity, profile and style code are fixed; versions only grow'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER item_guard BEFORE UPDATE OR DELETE ON brant_cat.item
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_item();

-- A draft revision is edited whole. Confirming keeps its technical content and last editor. A
-- confirmed revision is only superseded. A superseded revision never changes. Nothing is deleted.
CREATE FUNCTION brant_cat.guard_item_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.item_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.item_id, NEW.revision) IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.item_id, OLD.revision) THEN
        RAISE EXCEPTION 'brant_cat.item_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.confirmed_by_kind IS NULL AND NEW.evidence_ids IS NULL AND NEW.superseded_at IS NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'confirmed' AND NEW.superseded_at IS NULL
        AND (NEW.content_hash, NEW.fabric_composition, NEW.fabric_spec, NEW.fabric_width_cm, NEW.fabric_width_measured, NEW.fabric_width_source,
             NEW.fabric_gsm, NEW.fabric_gsm_measured, NEW.fabric_gsm_source, NEW.warp_shrinkage_pct, NEW.warp_shrinkage_measured, NEW.warp_shrinkage_source,
             NEW.weft_shrinkage_pct, NEW.weft_shrinkage_measured, NEW.weft_shrinkage_source, NEW.dye_lot_policy, NEW.size_chart_evidence_id,
             NEW.saved_by_kind, NEW.saved_by_id, NEW.saved_epoch, NEW.saved_at)
            IS NOT DISTINCT FROM
           (OLD.content_hash, OLD.fabric_composition, OLD.fabric_spec, OLD.fabric_width_cm, OLD.fabric_width_measured, OLD.fabric_width_source,
            OLD.fabric_gsm, OLD.fabric_gsm_measured, OLD.fabric_gsm_source, OLD.warp_shrinkage_pct, OLD.warp_shrinkage_measured, OLD.warp_shrinkage_source,
            OLD.weft_shrinkage_pct, OLD.weft_shrinkage_measured, OLD.weft_shrinkage_source, OLD.dye_lot_policy, OLD.size_chart_evidence_id,
            OLD.saved_by_kind, OLD.saved_by_id, OLD.saved_epoch, OLD.saved_at) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'confirmed' AND NEW.status = 'superseded' AND NEW.superseded_at IS NOT NULL
        AND (NEW.content_hash, NEW.confirmed_by_kind, NEW.confirmed_by_id, NEW.confirm_epoch, NEW.confirmed_at, NEW.evidence_ids)
            IS NOT DISTINCT FROM
           (OLD.content_hash, OLD.confirmed_by_kind, OLD.confirmed_by_id, OLD.confirm_epoch, OLD.confirmed_at, OLD.evidence_ids) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cat.item_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER item_revision_guard BEFORE UPDATE OR DELETE ON brant_cat.item_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_item_revision();

-- Cells belong to a draft until that revision is confirmed. Confirm does not rewrite them.
CREATE FUNCTION brant_cat.guard_item_sku_variant() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    v_status text;
BEGIN
    SELECT r.status INTO v_status
    FROM brant_cat.item_revision r
    WHERE r.tenant_id = COALESCE(NEW.tenant_id, OLD.tenant_id) AND r.company_id = COALESCE(NEW.company_id, OLD.company_id)
      AND r.item_id = COALESCE(NEW.item_id, OLD.item_id) AND r.revision = COALESCE(NEW.revision, OLD.revision);
    IF v_status = 'draft' AND TG_OP IN ('INSERT', 'DELETE') THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cat.item_sku_variant: only a draft revision''s matrix changes'
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER item_sku_variant_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cat.item_sku_variant
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_item_sku_variant();

ALTER TABLE brant_cat.item ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.item FORCE ROW LEVEL SECURITY;
CREATE POLICY item_scope ON brant_cat.item
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cat.item_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.item_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY item_revision_scope ON brant_cat.item_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cat.item_sku_variant ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.item_sku_variant FORCE ROW LEVEL SECURITY;
CREATE POLICY item_sku_variant_scope ON brant_cat.item_sku_variant
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
