-- CAT: declaration profiles (tactical model CAT-AGG-DeclarationProfile, CAT-INV-002,
-- SM-CAT-DECLARATION; baseline 22.1 CNProductDeclarationProfile; fact MD-41; S5 D-22). A profile
-- says what one confirmed SKU specification revision is for customs under one version of the
-- commodity code table: the commodity code, the declaration name and elements, the legal units and
-- their conversion from the SKU's base unit, the origin basis and the export control classification,
-- each part with its own basis and sources (classification parts kept separately, D-22).
--
-- A profile (tenant, company, profile_id) has a key (sku_id, spec_revision, code_table_version)
-- fixed by its first save and unique in the company (declaration_profile_key; a second profile of
-- the key is refused BUSINESS_KEY_CONFLICT), and a root version every command compares and
-- increments. Its content lives in revisions numbered monotonically: one draft revision is saved
-- whole, by a human or an AI agent (a candidate, never used); a human other than its last editor
-- verifies it, which supersedes the profile's verified revision in the same transaction. At most one
-- draft and one verified revision per profile. A verified revision never changes: the trigger allows
-- only its supersession, nothing is ever deleted, and brant_app may update only the columns a
-- transition writes, so a declaration that adopted a revision keeps what it adopted.
--
-- content is the canonical JSON the domain hashes into content_hash (SHA-256); the CHECKs below hold
-- the parts every revision needs, whatever wrote it. The SKU specification revision must exist as a
-- confirmed revision (foreign key to sku_revision, CAT-INV-001); a profile may be kept for a revision
-- the SKU has moved on from, as goods made to it are still declared.

CREATE TABLE brant_cat.declaration_profile (
    tenant_id          text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id         text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    profile_id         text        NOT NULL CHECK (profile_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    sku_id             text        NOT NULL,
    spec_revision      bigint      NOT NULL CHECK (spec_revision > 0),
    code_table_version text        NOT NULL CHECK (code_table_version ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version       bigint      NOT NULL CHECK (root_version > 0),
    last_revision      bigint      NOT NULL CHECK (last_revision > 0),
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, profile_id),
    CONSTRAINT declaration_profile_key UNIQUE (tenant_id, company_id, sku_id, spec_revision, code_table_version),
    FOREIGN KEY (tenant_id, company_id, sku_id, spec_revision)
        REFERENCES brant_cat.sku_revision (tenant_id, company_id, sku_id, spec_revision)
);

CREATE TABLE brant_cat.declaration_profile_revision (
    tenant_id        text        NOT NULL,
    company_id       text        NOT NULL,
    profile_id       text        NOT NULL,
    revision         bigint      NOT NULL CHECK (revision > 0),
    status           text        NOT NULL CHECK (status IN ('draft', 'verified', 'superseded')),
    content          jsonb       NOT NULL CHECK (jsonb_typeof(content) = 'object'),
    content_hash     text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    saved_by_kind    text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id      text        NOT NULL,
    saved_epoch      bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at         timestamptz NOT NULL,
    verified_by_kind text,
    verified_by_id   text,
    verify_epoch     bigint,
    verified_at      timestamptz,
    evidence_ids     text[],
    superseded_at    timestamptz,
    PRIMARY KEY (tenant_id, company_id, profile_id, revision),
    FOREIGN KEY (tenant_id, company_id, profile_id) REFERENCES brant_cat.declaration_profile (tenant_id, company_id, profile_id),
    -- The parts every revision has: a commodity code of digits, a declaration name, elements, one or
    -- two legal units, an origin country, an export control classification with a list reference
    -- exactly when controlled. A missing member fails the check (IS TRUE), it does not pass as NULL.
    CHECK ((content #>> '{commodity,code}' ~ '^[0-9]{6,12}$'
        AND content ->> 'declaration_name' <> ''
        AND jsonb_array_length(content -> 'elements') BETWEEN 1 AND 50
        AND jsonb_array_length(content -> 'legal_units') BETWEEN 1 AND 2
        AND content #>> '{origin,country}' ~ '^[A-Z]{2}$'
        AND content #>> '{origin,criterion}' IN ('wholly_obtained', 'substantial_transformation')
        AND content #>> '{export_control,status}' IN ('not_controlled', 'controlled')
        AND ((content #>> '{export_control,status}' = 'controlled') = (content #> '{export_control,list_reference}' IS NOT NULL))) IS TRUE),
    -- A verified revision was verified by a human other than its last editor, on evidence
    -- (CAT-INV-002; role templates: declaration_profile.verify 独立于该修订的最后编辑人).
    CHECK (CASE WHEN status IN ('verified', 'superseded')
        THEN verified_by_kind = 'human' AND verified_by_id IS NOT NULL AND verify_epoch > 0 AND verified_at IS NOT NULL
             AND cardinality(evidence_ids) BETWEEN 1 AND 200 AND verified_by_id <> saved_by_id
        ELSE verified_by_kind IS NULL AND verified_by_id IS NULL AND verify_epoch IS NULL AND verified_at IS NULL
             AND evidence_ids IS NULL END),
    CHECK ((status = 'superseded') = (superseded_at IS NOT NULL))
);

-- At most one draft and one verified revision per profile.
CREATE UNIQUE INDEX declaration_profile_revision_draft ON brant_cat.declaration_profile_revision (tenant_id, company_id, profile_id)
    WHERE status = 'draft';
CREATE UNIQUE INDEX declaration_profile_revision_verified ON brant_cat.declaration_profile_revision (tenant_id, company_id, profile_id)
    WHERE status = 'verified';

-- The key is fixed, the root version only grows and revision numbers are never reused.
CREATE FUNCTION brant_cat.guard_declaration_profile() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.declaration_profile: profiles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.profile_id, NEW.sku_id, NEW.spec_revision, NEW.code_table_version, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.profile_id, OLD.sku_id, OLD.spec_revision, OLD.code_table_version, OLD.created_at)
            OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision THEN
        RAISE EXCEPTION 'brant_cat.declaration_profile: identity and key are fixed, the root version grows, revision numbers are not reused'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER declaration_profile_guard BEFORE UPDATE OR DELETE ON brant_cat.declaration_profile
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_declaration_profile();

-- Only a draft revision is edited; verifying keeps its content and last editor; a verified revision
-- is only superseded, keeping its content and verification; a superseded revision never changes;
-- nothing is deleted.
CREATE FUNCTION brant_cat.guard_declaration_profile_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.declaration_profile_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.profile_id, NEW.revision) IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.profile_id, OLD.revision) THEN
        RAISE EXCEPTION 'brant_cat.declaration_profile_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' THEN
        RETURN NEW;
    END IF;
    IF (NEW.content, NEW.content_hash, NEW.saved_by_kind, NEW.saved_by_id, NEW.saved_epoch, NEW.saved_at)
            IS DISTINCT FROM (OLD.content, OLD.content_hash, OLD.saved_by_kind, OLD.saved_by_id, OLD.saved_epoch, OLD.saved_at) THEN
        RAISE EXCEPTION 'brant_cat.declaration_profile_revision: only a draft revision is edited (revision % is %)', OLD.revision, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'verified' THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'verified' AND NEW.status = 'superseded'
            AND (NEW.verified_by_kind, NEW.verified_by_id, NEW.verify_epoch, NEW.verified_at, NEW.evidence_ids)
                IS NOT DISTINCT FROM (OLD.verified_by_kind, OLD.verified_by_id, OLD.verify_epoch, OLD.verified_at, OLD.evidence_ids) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cat.declaration_profile_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER declaration_profile_revision_guard BEFORE UPDATE OR DELETE ON brant_cat.declaration_profile_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_declaration_profile_revision();

ALTER TABLE brant_cat.declaration_profile ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.declaration_profile FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_profile_scope ON brant_cat.declaration_profile
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cat.declaration_profile_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.declaration_profile_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY declaration_profile_revision_scope ON brant_cat.declaration_profile_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
