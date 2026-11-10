-- CAT: item code mappings (tactical model CAT-AGG-ItemCodeMapping; Python foundation kind
-- code_mapping). A mapping maps one external item code, a customer's or a supplier's own code, to a
-- SKU at one confirmed specification revision. Codes are matched exactly; a code without a published
-- mapping is unmapped and goes to manual verification, never to another SKU.
--
-- A mapping (tenant, company, mapping_id) has a key (party_id, party_role, external_code) fixed by
-- its first save and a root version every command compares and increments. Its content lives in
-- revisions numbered monotonically: one draft revision is saved whole; a human publishes it, which
-- supersedes the mapping's active revision in the same transaction. At most one revision is active
-- per key in a company, across all mappings: the unique index item_code_mapping_published_key holds
-- this under concurrency, and the second publication is refused BUSINESS_KEY_CONFLICT. A published
-- revision never changes: the trigger allows only its supersession, nothing is ever deleted, and
-- brant_app may update only the columns a transition writes.
--
-- The party is PTY's and stored as given (PTY is not upstream of CAT; consumers with both upstreams
-- check it). The SKU specification revision must exist as a confirmed revision (foreign key to
-- sku_revision, CAT-INV-001); that it is current when saved and published is CAT's command rule.

CREATE TABLE brant_cat.item_code_mapping (
    tenant_id     text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id    text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    mapping_id    text        NOT NULL CHECK (mapping_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_id      text        NOT NULL CHECK (party_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    party_role    text        NOT NULL CHECK (party_role IN ('customer', 'supplier')),
    external_code text        NOT NULL CHECK (char_length(external_code) BETWEEN 1 AND 500 AND external_code ~ '\S'
                                  AND external_code = btrim(external_code)),
    root_version  bigint      NOT NULL CHECK (root_version > 0),
    last_revision bigint      NOT NULL CHECK (last_revision > 0),
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, mapping_id),
    -- The target of the revisions' foreign key: a revision carries its mapping's key.
    UNIQUE (tenant_id, company_id, mapping_id, party_id, party_role, external_code)
);

CREATE TABLE brant_cat.item_code_mapping_revision (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    mapping_id        text        NOT NULL,
    revision          bigint      NOT NULL CHECK (revision > 0),
    party_id          text        NOT NULL,
    party_role        text        NOT NULL,
    external_code     text        NOT NULL,
    status            text        NOT NULL CHECK (status IN ('draft', 'active', 'superseded')),
    sku_id            text        NOT NULL,
    spec_revision     bigint      NOT NULL CHECK (spec_revision > 0),
    saved_by_kind     text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id       text        NOT NULL,
    saved_epoch       bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at          timestamptz NOT NULL,
    published_by_kind text,
    published_by_id   text,
    publish_epoch     bigint,
    published_at      timestamptz,
    evidence_ids      text[],
    superseded_at     timestamptz,
    PRIMARY KEY (tenant_id, company_id, mapping_id, revision),
    FOREIGN KEY (tenant_id, company_id, mapping_id, party_id, party_role, external_code)
        REFERENCES brant_cat.item_code_mapping (tenant_id, company_id, mapping_id, party_id, party_role, external_code),
    FOREIGN KEY (tenant_id, company_id, sku_id, spec_revision)
        REFERENCES brant_cat.sku_revision (tenant_id, company_id, sku_id, spec_revision),
    -- A published revision was published by a human on evidence.
    CHECK (CASE WHEN status IN ('active', 'superseded')
        THEN published_by_kind = 'human' AND published_by_id IS NOT NULL AND publish_epoch > 0 AND published_at IS NOT NULL
             AND cardinality(evidence_ids) BETWEEN 1 AND 200
        ELSE published_by_kind IS NULL AND published_by_id IS NULL AND publish_epoch IS NULL AND published_at IS NULL
             AND evidence_ids IS NULL END),
    CHECK ((status = 'superseded') = (superseded_at IS NOT NULL))
);

-- At most one draft revision per mapping, and one active revision per key in a company.
CREATE UNIQUE INDEX item_code_mapping_revision_draft ON brant_cat.item_code_mapping_revision (tenant_id, company_id, mapping_id)
    WHERE status = 'draft';
CREATE UNIQUE INDEX item_code_mapping_published_key
    ON brant_cat.item_code_mapping_revision (tenant_id, company_id, party_id, party_role, external_code)
    WHERE status = 'active';

-- The key is fixed, the root version only grows and revision numbers are never reused.
CREATE FUNCTION brant_cat.guard_item_code_mapping() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.item_code_mapping: mappings are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.mapping_id, NEW.party_id, NEW.party_role, NEW.external_code, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.mapping_id, OLD.party_id, OLD.party_role, OLD.external_code, OLD.created_at)
            OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision THEN
        RAISE EXCEPTION 'brant_cat.item_code_mapping: identity and key are fixed, the root version grows, revision numbers are not reused'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER item_code_mapping_guard BEFORE UPDATE OR DELETE ON brant_cat.item_code_mapping
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_item_code_mapping();

-- Only a draft revision is edited; publishing keeps its content; an active revision is only
-- superseded, keeping its content and publication; a superseded revision never changes; nothing is
-- deleted.
CREATE FUNCTION brant_cat.guard_item_code_mapping_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cat.item_code_mapping_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.mapping_id, NEW.revision, NEW.party_id, NEW.party_role, NEW.external_code)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.mapping_id, OLD.revision, OLD.party_id, OLD.party_role, OLD.external_code) THEN
        RAISE EXCEPTION 'brant_cat.item_code_mapping_revision: identity and key are fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' THEN
        RETURN NEW;
    END IF;
    IF (NEW.sku_id, NEW.spec_revision, NEW.saved_by_kind, NEW.saved_by_id, NEW.saved_epoch, NEW.saved_at)
            IS DISTINCT FROM (OLD.sku_id, OLD.spec_revision, OLD.saved_by_kind, OLD.saved_by_id, OLD.saved_epoch, OLD.saved_at) THEN
        RAISE EXCEPTION 'brant_cat.item_code_mapping_revision: only a draft revision is edited (revision % is %)', OLD.revision, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'active' THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'superseded'
            AND (NEW.published_by_kind, NEW.published_by_id, NEW.publish_epoch, NEW.published_at, NEW.evidence_ids)
                IS NOT DISTINCT FROM (OLD.published_by_kind, OLD.published_by_id, OLD.publish_epoch, OLD.published_at, OLD.evidence_ids) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_cat.item_code_mapping_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER item_code_mapping_revision_guard BEFORE UPDATE OR DELETE ON brant_cat.item_code_mapping_revision
    FOR EACH ROW EXECUTE FUNCTION brant_cat.guard_item_code_mapping_revision();

ALTER TABLE brant_cat.item_code_mapping ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.item_code_mapping FORCE ROW LEVEL SECURITY;
CREATE POLICY item_code_mapping_scope ON brant_cat.item_code_mapping
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cat.item_code_mapping_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cat.item_code_mapping_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY item_code_mapping_revision_scope ON brant_cat.item_code_mapping_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
