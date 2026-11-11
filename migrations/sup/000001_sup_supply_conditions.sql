-- SUP: supplier RFQs and supply conditions (Python T02 supplier_rfq and t02_root / t02_revision of
-- kind supplier_offer, purchase_terms and logistics_offer; tactical model SUP-AGG-SupplierRfq,
-- -SupplierOffer, -PurchaseTerms, -LogisticsOffer; T02 §13.2).
--
-- A supplier RFQ is a draft saved whole until a human records it as issued, which seals its content
-- and fixes its issued version; closing it changes nothing else (SM-SUP-SRFQ).
--
-- A supply condition is (tenant, company, kind, condition_id) with a root version every command
-- compares and increments, and revisions numbered monotonically (SM-SUP-OFFER): one draft revision
-- is saved whole; a human who never edited it verifies it (purchase terms: publishes it), which
-- seals it and supersedes the active revision in the same transaction (SUP-INV-001, SUP-INV-003);
-- withdrawing (purchase terms: retiring) ends an exact sealed revision; discarding ends the draft.
-- A sealed revision never changes its content, its saving or its verification: the trigger allows
-- only its supersession and its end, nothing is ever deleted, and brant_app may update only the
-- columns a transition writes. At most one draft and one active revision per root.
--
-- The content is the canonical JSON SUP's api encodes (data) with its SHA-256; the columns beside it
-- are what SupplyConditionGuard v1 reads without decoding it: the validity, and for purchase terms
-- the validity mode and the source offer revision. References to PTY, INQ, CAT, POL and EVD are
-- plain IDs inside the content: no foreign key crosses a schema (DB2).
--
-- action_result and audit_event are the command ledger of SUP (internal/foundation/action).

CREATE SCHEMA brant_sup;

CREATE TABLE brant_sup.supplier_rfq (
    tenant_id       text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    supplier_rfq_id text        NOT NULL CHECK (supplier_rfq_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    status          text        NOT NULL CHECK (status IN ('draft', 'issued', 'closed')),
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    data            jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    content_hash    text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    owner_actor_id  text        NOT NULL,
    owner_team_id   text,
    saved_by_kind   text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id     text        NOT NULL,
    saved_epoch     bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at        timestamptz NOT NULL,
    issued_version  bigint,
    issued_by_kind  text,
    issued_by_id    text,
    issue_epoch     bigint,
    issued_at       timestamptz,
    issue_reason    text,
    issue_evidence  jsonb,
    closed_by_kind  text,
    closed_by_id    text,
    close_epoch     bigint,
    closed_at       timestamptz,
    close_reason    text,
    close_evidence  jsonb,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, supplier_rfq_id),
    -- Issued and closed requests were issued by a human on evidence; only closed ones were closed.
    CHECK (CASE WHEN status = 'draft'
        THEN issued_version IS NULL AND issued_by_kind IS NULL AND issued_by_id IS NULL AND issue_epoch IS NULL
             AND issued_at IS NULL AND issue_reason IS NULL AND issue_evidence IS NULL
        ELSE issued_version > 1 AND issued_version <= row_version AND issued_by_kind = 'human' AND issued_by_id IS NOT NULL
             AND issue_epoch > 0 AND issued_at IS NOT NULL AND issue_reason IS NOT NULL
             AND jsonb_typeof(issue_evidence) = 'array' AND issue_evidence <> '[]'::jsonb END),
    CHECK (CASE WHEN status = 'closed'
        THEN closed_by_kind = 'human' AND closed_by_id IS NOT NULL AND close_epoch > 0 AND closed_at IS NOT NULL
             AND close_reason IS NOT NULL AND jsonb_typeof(close_evidence) = 'array' AND close_evidence <> '[]'::jsonb
        ELSE closed_by_kind IS NULL AND closed_by_id IS NULL AND close_epoch IS NULL AND closed_at IS NULL
             AND close_reason IS NULL AND close_evidence IS NULL END)
);

-- A draft is replaced whole; an issued request changes only by being closed; a closed one never
-- changes; nothing is deleted.
CREATE FUNCTION brant_sup.guard_supplier_rfq() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_sup.supplier_rfq: supplier RFQs are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.supplier_rfq_id, NEW.owner_actor_id, NEW.owner_team_id, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.supplier_rfq_id, OLD.owner_actor_id, OLD.owner_team_id, OLD.created_at)
            OR NEW.row_version <= OLD.row_version THEN
        RAISE EXCEPTION 'brant_sup.supplier_rfq: identity and owner are fixed and the row version grows'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'issued' AND NEW.status = 'closed'
            AND (to_jsonb(NEW) - ARRAY['status', 'row_version', 'updated_at', 'closed_by_kind', 'closed_by_id', 'close_epoch',
                'closed_at', 'close_reason', 'close_evidence'])
                IS NOT DISTINCT FROM
                (to_jsonb(OLD) - ARRAY['status', 'row_version', 'updated_at', 'closed_by_kind', 'closed_by_id', 'close_epoch',
                'closed_at', 'close_reason', 'close_evidence']) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_sup.supplier_rfq: a % supplier RFQ is not edited; a correction is a new supplier RFQ', OLD.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER supplier_rfq_guard BEFORE UPDATE OR DELETE ON brant_sup.supplier_rfq
    FOR EACH ROW EXECUTE FUNCTION brant_sup.guard_supplier_rfq();

CREATE TABLE brant_sup.condition_root (
    tenant_id      text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id     text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    kind           text        NOT NULL CHECK (kind IN ('supplier_offer', 'purchase_terms', 'logistics_offer')),
    condition_id   text        NOT NULL CHECK (condition_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version   bigint      NOT NULL CHECK (root_version > 0),
    last_revision  bigint      NOT NULL CHECK (last_revision > 0),
    owner_actor_id text        NOT NULL,
    owner_team_id  text,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, kind, condition_id)
);

CREATE TABLE brant_sup.condition_revision (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    kind                  text        NOT NULL,
    condition_id          text        NOT NULL,
    revision              bigint      NOT NULL CHECK (revision > 0),
    status                text        NOT NULL CHECK (status IN ('draft', 'active', 'superseded', 'withdrawn', 'retired', 'discarded')),
    base_revision         bigint      CHECK (base_revision > 0),
    data                  jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    content_hash          text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    valid_from            timestamptz,
    valid_until           timestamptz CHECK (valid_until > valid_from),
    validity_mode         text        CHECK (validity_mode IN ('independent_validity', 'dependent_validity')),
    source_offer_id       text        CHECK (source_offer_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source_offer_revision bigint      CHECK (source_offer_revision > 0),
    saved_by_kind         text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id           text        NOT NULL,
    saved_epoch           bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at              timestamptz NOT NULL,
    contributors          text[]      NOT NULL CHECK (cardinality(contributors) BETWEEN 1 AND 1000),
    verified_by_kind      text,
    verified_by_id        text,
    verify_epoch          bigint,
    verified_at           timestamptz,
    verification_evidence jsonb,
    superseded_at         timestamptz,
    ended_by_kind         text        CHECK (ended_by_kind IN ('human', 'ai')),
    ended_by_id           text,
    end_epoch             bigint,
    ended_at              timestamptz,
    end_reason            text,
    end_evidence          jsonb,
    PRIMARY KEY (tenant_id, company_id, kind, condition_id, revision),
    FOREIGN KEY (tenant_id, company_id, kind, condition_id)
        REFERENCES brant_sup.condition_root (tenant_id, company_id, kind, condition_id),
    CHECK (base_revision < revision),
    CHECK (kind = 'purchase_terms' OR (validity_mode IS NULL AND source_offer_id IS NULL AND source_offer_revision IS NULL)),
    CHECK ((source_offer_id IS NULL) = (source_offer_revision IS NULL)),
    -- Purchase terms are retired, offers are withdrawn.
    CHECK (status <> 'retired' OR kind = 'purchase_terms'),
    CHECK (status <> 'withdrawn' OR kind <> 'purchase_terms'),
    -- A sealed revision was verified on evidence by a human who never edited it (SUP-INV-003), and
    -- its content is complete: a validity and, for purchase terms, the mode and the source offer.
    CHECK (CASE WHEN status IN ('active', 'superseded', 'withdrawn', 'retired')
        THEN verified_by_kind = 'human' AND verified_by_id IS NOT NULL AND verify_epoch > 0 AND verified_at IS NOT NULL
             AND jsonb_typeof(verification_evidence) = 'array' AND verification_evidence <> '[]'::jsonb
             AND NOT (verified_by_id = ANY (contributors)) AND saved_by_id <> verified_by_id
             AND valid_from IS NOT NULL AND valid_until IS NOT NULL
             AND (kind <> 'purchase_terms' OR (validity_mode IS NOT NULL AND source_offer_id IS NOT NULL))
        ELSE verified_by_kind IS NULL AND verified_by_id IS NULL AND verify_epoch IS NULL AND verified_at IS NULL
             AND verification_evidence IS NULL END),
    CHECK (status <> 'superseded' OR superseded_at IS NOT NULL),
    CHECK (status NOT IN ('draft', 'active', 'discarded') OR superseded_at IS NULL),
    -- Withdrawn, retired and discarded revisions record who ended them, when and why; a withdrawal or
    -- retirement is a human act on evidence.
    CHECK ((status IN ('withdrawn', 'retired', 'discarded')) = (ended_at IS NOT NULL)),
    CHECK (CASE WHEN ended_at IS NULL
        THEN ended_by_kind IS NULL AND ended_by_id IS NULL AND end_epoch IS NULL AND end_reason IS NULL AND end_evidence IS NULL
        ELSE ended_by_kind IS NOT NULL AND ended_by_id IS NOT NULL AND end_epoch > 0 AND end_reason IS NOT NULL END),
    CHECK (status NOT IN ('withdrawn', 'retired')
        OR (ended_by_kind = 'human' AND jsonb_typeof(end_evidence) = 'array' AND end_evidence <> '[]'::jsonb)),
    CHECK (status <> 'discarded' OR end_evidence IS NULL)
);

-- At most one draft and one active revision per supply condition (SUP-INV-003: 同根最多一个 active).
CREATE UNIQUE INDEX condition_revision_draft ON brant_sup.condition_revision (tenant_id, company_id, kind, condition_id)
    WHERE status = 'draft';
CREATE UNIQUE INDEX condition_revision_active ON brant_sup.condition_revision (tenant_id, company_id, kind, condition_id)
    WHERE status = 'active';

-- The root version only grows, revision numbers are never reused, the owner is fixed.
CREATE FUNCTION brant_sup.guard_root() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_sup.condition_root: supply conditions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.kind, NEW.condition_id, NEW.owner_actor_id, NEW.owner_team_id, NEW.created_at)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.kind, OLD.condition_id, OLD.owner_actor_id, OLD.owner_team_id, OLD.created_at)
            OR NEW.root_version <= OLD.root_version OR NEW.last_revision < OLD.last_revision THEN
        RAISE EXCEPTION 'brant_sup.condition_root: identity and owner are fixed, the root version grows, revision numbers are not reused'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER condition_root_guard BEFORE UPDATE OR DELETE ON brant_sup.condition_root
    FOR EACH ROW EXECUTE FUNCTION brant_sup.guard_root();

-- Only a draft revision is edited (SUP-INV-001). A draft is sealed or discarded; an active revision
-- is superseded, withdrawn or retired; a superseded one withdrawn or retired; the content, the
-- saving, the base and a recorded verification never change after the draft; superseded and ended
-- revisions keep their facts; nothing is deleted.
CREATE FUNCTION brant_sup.guard_revision() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_sup.condition_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.kind, NEW.condition_id, NEW.revision, NEW.base_revision)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.kind, OLD.condition_id, OLD.revision, OLD.base_revision) THEN
        RAISE EXCEPTION 'brant_sup.condition_revision: identity and base are fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' THEN
        RETURN NEW;
    END IF;
    IF (NEW.data, NEW.content_hash, NEW.valid_from, NEW.valid_until, NEW.validity_mode, NEW.source_offer_id, NEW.source_offer_revision,
            NEW.saved_by_kind, NEW.saved_by_id, NEW.saved_epoch, NEW.saved_at, NEW.contributors)
            IS DISTINCT FROM
       (OLD.data, OLD.content_hash, OLD.valid_from, OLD.valid_until, OLD.validity_mode, OLD.source_offer_id, OLD.source_offer_revision,
            OLD.saved_by_kind, OLD.saved_by_id, OLD.saved_epoch, OLD.saved_at, OLD.contributors) THEN
        RAISE EXCEPTION 'brant_sup.condition_revision: only a draft revision is edited (revision % is %; SUP-INV-001)', OLD.revision, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status IN ('active', 'discarded') THEN
        RETURN NEW;
    END IF;
    IF (NEW.verified_by_kind, NEW.verified_by_id, NEW.verify_epoch, NEW.verified_at, NEW.verification_evidence)
            IS DISTINCT FROM (OLD.verified_by_kind, OLD.verified_by_id, OLD.verify_epoch, OLD.verified_at, OLD.verification_evidence) THEN
        RAISE EXCEPTION 'brant_sup.condition_revision: a verification is never changed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'active' AND NEW.status = 'superseded' AND OLD.superseded_at IS NULL AND NEW.ended_at IS NULL THEN
        RETURN NEW;
    END IF;
    IF OLD.status IN ('active', 'superseded') AND NEW.status IN ('withdrawn', 'retired') AND OLD.ended_at IS NULL
            AND NEW.superseded_at IS NOT DISTINCT FROM OLD.superseded_at THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_sup.condition_revision: % to % is not a revision transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER condition_revision_guard BEFORE UPDATE OR DELETE ON brant_sup.condition_revision
    FOR EACH ROW EXECUTE FUNCTION brant_sup.guard_revision();

ALTER TABLE brant_sup.supplier_rfq ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_sup.supplier_rfq FORCE ROW LEVEL SECURITY;
CREATE POLICY supplier_rfq_scope ON brant_sup.supplier_rfq
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_sup.condition_root ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_sup.condition_root FORCE ROW LEVEL SECURITY;
CREATE POLICY condition_root_scope ON brant_sup.condition_root
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_sup.condition_revision ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_sup.condition_revision FORCE ROW LEVEL SECURITY;
CREATE POLICY condition_revision_scope ON brant_sup.condition_revision
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE TABLE brant_sup.action_result (
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    command         text        NOT NULL CHECK (command ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_.]*$'),
    idempotency_key text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    request_hash    text        NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    action_id       text        NOT NULL,
    object_id       text        NOT NULL,
    row_version     bigint      NOT NULL CHECK (row_version > 0),
    status          text        NOT NULL,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    authority_epoch bigint      NOT NULL CHECK (authority_epoch > 0),
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, command, idempotency_key),
    UNIQUE (tenant_id, company_id, action_id)
);

CREATE TABLE brant_sup.audit_event (
    seq             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id       text        NOT NULL,
    company_id      text        NOT NULL,
    event_id        text        NOT NULL UNIQUE,
    actor_kind      text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    actor_id        text        NOT NULL,
    command         text        NOT NULL,
    object_id       text,
    outcome         text        NOT NULL CHECK (outcome IN ('succeeded', 'replayed', 'rejected', 'read')),
    code            text,
    correlation_id  text        NOT NULL,
    recorded_at     timestamptz NOT NULL,
    reason          text,
    authority_epoch bigint,
    CHECK ((outcome = 'rejected') = (code IS NOT NULL))
);

ALTER TABLE brant_sup.action_result ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_sup.action_result FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_sup.audit_event ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_sup.audit_event FORCE ROW LEVEL SECURITY;
CREATE POLICY action_result_scope ON brant_sup.action_result
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY audit_event_scope ON brant_sup.audit_event
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
