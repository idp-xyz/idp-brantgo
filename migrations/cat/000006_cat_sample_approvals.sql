-- CAT: sample approval (CAT-AGG-SampleApproval, BC-AT23) and SampleGuard v1.
--
-- One approval per (company, sku, stage). A draft is saved by a human or an AI agent. sample.approve
-- is human only and independent of the draft's last editor; it binds the draft's specification
-- revision, which must be the SKU's current confirmed one. sample.revoke records a reason and makes
-- the guard fail closed. An approval of an older specification revision does not satisfy a newer one.
--
-- SampleGuard v1 locks the approval FOR SHARE and then the SKU FOR SHARE (approval before SKU).
-- sample.approve locks its own root FOR UPDATE and then calls SkuGuard, which locks the SKU. The
-- same order keeps quote.freeze, which calls this guard before SkuGuard, from deadlocking with
-- approve. search_path is fixed, every name is qualified, and PUBLIC has no EXECUTE.

CREATE TABLE brant_cat.sample_approval (
    tenant_id          text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sample_approval_id text        NOT NULL CHECK (sample_approval_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id             text        NOT NULL CHECK (sku_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sample_stage       text        NOT NULL CHECK (sample_stage IN ('proto', 'fit', 'pp_sample', 'shipment')),
    status             text        NOT NULL CHECK (status IN ('draft', 'approved', 'revoked')),
    root_version       bigint      NOT NULL CHECK (root_version > 0),
    last_revision      bigint      NOT NULL CHECK (last_revision > 0),
    current_revision   bigint      NOT NULL CHECK (current_revision >= 0),
    owner_actor_id     text        NOT NULL,
    owner_team_id      text,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, sample_approval_id),
    CONSTRAINT sample_approval_sku_stage UNIQUE (tenant_id, company_id, sku_id, sample_stage),
    CONSTRAINT sample_approval_identity UNIQUE (tenant_id, company_id, sample_approval_id, sku_id, sample_stage),
    CHECK (current_revision <= last_revision)
);

CREATE TABLE brant_cat.sample_approval_revision (
    tenant_id          text        NOT NULL,
    company_id         text        NOT NULL,
    sample_approval_id text        NOT NULL,
    revision           bigint      NOT NULL CHECK (revision > 0),
    sku_id             text        NOT NULL,
    sample_stage       text        NOT NULL CHECK (sample_stage IN ('proto', 'fit', 'pp_sample', 'shipment')),
    spec_revision      bigint      NOT NULL CHECK (spec_revision > 0),
    tech_pack_sha256   text        NOT NULL CHECK (tech_pack_sha256 ~ '^[0-9a-f]{64}$'),
    evidence_ids       text[]      NOT NULL CHECK (cardinality(evidence_ids) BETWEEN 1 AND 200),
    status             text        NOT NULL CHECK (status IN ('draft', 'approved', 'superseded', 'revoked')),
    saved_by_kind      text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id        text        NOT NULL,
    saved_epoch        bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at           timestamptz NOT NULL,
    approved_by_kind   text,
    approved_by_id     text,
    approve_epoch      bigint,
    approved_at        timestamptz,
    revoke_reason      text,
    revoked_by_kind    text,
    revoked_by_id      text,
    revoke_epoch       bigint,
    revoked_at         timestamptz,
    superseded_at      timestamptz,
    PRIMARY KEY (tenant_id, company_id, sample_approval_id, revision),
    FOREIGN KEY (tenant_id, company_id, sample_approval_id, sku_id, sample_stage)
        REFERENCES brant_cat.sample_approval (tenant_id, company_id, sample_approval_id, sku_id, sample_stage),
    FOREIGN KEY (tenant_id, company_id, sku_id, spec_revision)
        REFERENCES brant_cat.sku_revision (tenant_id, company_id, sku_id, spec_revision),
    CHECK (CASE status
        WHEN 'draft' THEN approved_by_kind IS NULL AND approved_by_id IS NULL AND approve_epoch IS NULL AND approved_at IS NULL
            AND revoke_reason IS NULL AND revoked_by_kind IS NULL AND revoked_by_id IS NULL AND revoke_epoch IS NULL
            AND revoked_at IS NULL AND superseded_at IS NULL
        WHEN 'approved' THEN approved_by_kind = 'human' AND approved_by_id IS NOT NULL AND approved_by_id <> saved_by_id
            AND approve_epoch > 0 AND approved_at IS NOT NULL
            AND revoke_reason IS NULL AND revoked_by_kind IS NULL AND revoked_at IS NULL AND superseded_at IS NULL
        WHEN 'superseded' THEN approved_by_kind = 'human' AND approved_by_id IS NOT NULL AND approved_by_id <> saved_by_id
            AND approve_epoch > 0 AND approved_at IS NOT NULL
            AND revoke_reason IS NULL AND revoked_by_kind IS NULL AND revoked_at IS NULL AND superseded_at IS NOT NULL
        WHEN 'revoked' THEN approved_by_kind = 'human' AND approved_by_id IS NOT NULL AND approved_by_id <> saved_by_id
            AND approve_epoch > 0 AND approved_at IS NOT NULL
            AND revoke_reason IS NOT NULL AND char_length(revoke_reason) BETWEEN 1 AND 1000
            AND revoked_by_kind = 'human' AND revoked_by_id IS NOT NULL AND revoke_epoch > 0 AND revoked_at IS NOT NULL
            AND superseded_at IS NULL
        ELSE false END)
);

CREATE UNIQUE INDEX sample_approval_revision_draft ON brant_cat.sample_approval_revision (tenant_id, company_id, sample_approval_id)
    WHERE status = 'draft';
CREATE UNIQUE INDEX sample_approval_revision_approved ON brant_cat.sample_approval_revision (tenant_id, company_id, sample_approval_id)
    WHERE status = 'approved';

CREATE FUNCTION brant_cat.guard_sample_approval() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.sample_approval: approvals are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.sample_approval_id, NEW.sku_id, NEW.sample_stage, NEW.owner_actor_id, NEW.owner_team_id, NEW.created_at)
            IS DISTINCT FROM
       (OLD.tenant_id, OLD.company_id, OLD.sample_approval_id, OLD.sku_id, OLD.sample_stage, OLD.owner_actor_id, OLD.owner_team_id, OLD.created_at)
        OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision OR NEW.current_revision < OLD.current_revision THEN
        RAISE EXCEPTION 'brant_cat.sample_approval: identity, SKU and stage are fixed; versions only grow'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER sample_approval_guard BEFORE UPDATE OR DELETE ON brant_cat.sample_approval
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_sample_approval();

CREATE FUNCTION brant_cat.guard_sample_approval_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.sample_approval_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.sample_approval_id, NEW.revision, NEW.sku_id, NEW.sample_stage)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.sample_approval_id, OLD.revision, OLD.sku_id, OLD.sample_stage) THEN
        RAISE EXCEPTION 'brant_cat.sample_approval_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.approved_by_kind IS NULL AND NEW.superseded_at IS NULL AND NEW.revoked_at IS NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'approved' AND NEW.superseded_at IS NULL AND NEW.revoked_at IS NULL
        AND (NEW.spec_revision, NEW.tech_pack_sha256, NEW.evidence_ids, NEW.saved_by_kind, NEW.saved_by_id, NEW.saved_epoch, NEW.saved_at)
            IS NOT DISTINCT FROM
           (OLD.spec_revision, OLD.tech_pack_sha256, OLD.evidence_ids, OLD.saved_by_kind, OLD.saved_by_id, OLD.saved_epoch, OLD.saved_at) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'approved' AND NEW.status = 'superseded' AND NEW.superseded_at IS NOT NULL AND NEW.revoked_at IS NULL
        AND (NEW.spec_revision, NEW.tech_pack_sha256, NEW.evidence_ids, NEW.approved_by_id, NEW.approved_at)
            IS NOT DISTINCT FROM (OLD.spec_revision, OLD.tech_pack_sha256, OLD.evidence_ids, OLD.approved_by_id, OLD.approved_at) THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'approved' AND NEW.status = 'revoked' AND NEW.superseded_at IS NULL
        AND (NEW.spec_revision, NEW.tech_pack_sha256, NEW.evidence_ids, NEW.approved_by_id, NEW.approved_at)
            IS NOT DISTINCT FROM (OLD.spec_revision, OLD.tech_pack_sha256, OLD.evidence_ids, OLD.approved_by_id, OLD.approved_at) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cat.sample_approval_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER sample_approval_revision_guard BEFORE UPDATE OR DELETE ON brant_cat.sample_approval_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_sample_approval_revision();

-- SampleGuard v1. One row. known is false when the SKU is not in the caller's company. code is
-- REFERENCE_INVALID when the SKU is missing or inactive, SAMPLE_APPROVAL_REQUIRED when nothing
-- approved for the stage is current, SAMPLE_SPEC_MISMATCH when the current approval binds another
-- specification revision, and NULL when that revision is approved for the stage. The approval row
-- is locked before the SKU row.
CREATE FUNCTION brant_cat.guard_sample_approved_v1(p_sku_id text, p_spec_revision bigint, p_stage text)
    RETURNS TABLE (known boolean, approved boolean, sample_approval_id text, approved_spec_revision bigint, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_id      text;
    v_status  text;
    v_current bigint;
    v_spec    bigint;
    v_revstat text;
    v_sku     text;
BEGIN
    IF p_sku_id IS NULL OR p_spec_revision IS NULL OR p_stage IS NULL THEN
        RAISE EXCEPTION 'brant_cat.guard_sample_approved_v1: sku_id, spec_revision and stage are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    IF p_stage NOT IN ('proto', 'fit', 'pp_sample', 'shipment') THEN
        RAISE EXCEPTION 'brant_cat.guard_sample_approved_v1: unknown sample stage'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    SELECT a.sample_approval_id, a.status, a.current_revision INTO v_id, v_status, v_current
    FROM brant_cat.sample_approval a
    WHERE a.tenant_id = v_tenant AND a.company_id = v_company AND a.sku_id = p_sku_id AND a.sample_stage = p_stage
    FOR SHARE;
    IF FOUND AND v_current > 0 THEN
        SELECT r.spec_revision, r.status INTO v_spec, v_revstat
        FROM brant_cat.sample_approval_revision r
        WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.sample_approval_id = v_id AND r.revision = v_current
        FOR SHARE;
    END IF;
    SELECT k.status INTO v_sku
    FROM brant_cat.sku k
    WHERE k.tenant_id = v_tenant AND k.company_id = v_company AND k.sku_id = p_sku_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, false, NULL::text, NULL::bigint, 'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    IF v_sku = 'inactive' THEN
        RETURN QUERY SELECT true, false, v_id, v_spec, 'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    IF v_id IS NULL OR v_status IS DISTINCT FROM 'approved' OR v_revstat IS DISTINCT FROM 'approved' THEN
        RETURN QUERY SELECT true, false, v_id, v_spec, 'SAMPLE_APPROVAL_REQUIRED'::text;
        RETURN;
    END IF;
    IF v_spec IS DISTINCT FROM p_spec_revision THEN
        RETURN QUERY SELECT true, false, v_id, v_spec, 'SAMPLE_SPEC_MISMATCH'::text;
        RETURN;
    END IF;
    RETURN QUERY SELECT true, true, v_id, v_spec, NULL::text;
END
$$;

REVOKE ALL ON FUNCTION brant_cat.guard_sample_approved_v1(text, bigint, text) FROM PUBLIC;

ALTER TABLE brant_cat.sample_approval ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.sample_approval FORCE ROW LEVEL SECURITY;
CREATE POLICY sample_approval_scope ON brant_cat.sample_approval
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cat.sample_approval_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.sample_approval_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY sample_approval_revision_scope ON brant_cat.sample_approval_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
