-- TRD: formalization of quote revisions and what ends them (WBS S3-04, S3-05; tactical model
-- TRD-AGG-QuoteRoot QuoteDocumentBinding, CommitEvidence, RiskHold, ConversionSlot; SM-TRD-REVISION;
-- TRD-INV-001, 009, 010, 016; Python t03_storage activate, withdraw, cancel and the t03_document_binding,
-- t03_commit_evidence and t03_risk tables of T03_reference.sql).
--
-- quote.activate and quote.replace formalize an approved revision in one transaction of the quote
-- root: its formal document (the bytes quote-text/0.1.0 rendered and the binding of number, file and
-- template to the revision's business hash), the commit evidence of the command (approval, epochs,
-- root version, what the upstream guards answered), and the revision pending_approval → active; a
-- replacement moves the active revision to superseded in the same transaction. The number comes from
-- POL (number.issue) before the transaction; a rollback leaves a gap in POL's sequence and nothing
-- here. One revision has at most one formal document and one commit evidence (primary keys), the
-- number belongs to one document of the company, and the revision becomes active only with both
-- (trigger). quote.withdraw ends a frozen or pending revision, quote.cancel the active one, each with
-- a closure row recording who, why and on which evidence.
--
-- A risk hold pauses the forward use of an active revision (UL TRD-RiskHold: no shortcut lifts it in
-- this batch): quote.approval.revoke of the approval the revision was formalized with registers one in
-- its transaction (SM-TRD-APPROVAL 生效后撤销登记风险暂停). The revision stays active; a replacement or a
-- cancellation ends it. The conversion slot (full_v1, TRD-INV-010) is taken once per root by the
-- order step for the active revision and kept for good; while it is taken the active revision is
-- neither superseded nor cancelled (ROOT_CONVERSION_LOCKED, checked by the command under the root
-- lock and again here).
--
-- Every row lives in the root's transaction: commands lock the quote root FOR UPDATE first. Nothing
-- here is ever updated or deleted. brant_trd.guard_quote_revision_active_v1 publishes, for callers
-- inside their own transaction, whether a formal revision is still active, unheld and convertible,
-- holding the root FOR SHARE until that transaction ends.

CREATE TABLE brant_trd.quote_document (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    quote_id              text        NOT NULL,
    revision              bigint      NOT NULL,
    business_hash         text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    number                text        NOT NULL CHECK (number ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$'),
    sequence_kind         text        NOT NULL CHECK (sequence_kind = 'number_sequence'),
    sequence_id           text        NOT NULL,
    sequence_revision     bigint      NOT NULL CHECK (sequence_revision > 0),
    business_ref          text        NOT NULL CHECK (business_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    template_kind         text        NOT NULL CHECK (template_kind = 'quote_document_template'),
    template_id           text        NOT NULL,
    template_revision     bigint      NOT NULL CHECK (template_revision > 0),
    template_content_hash text        NOT NULL CHECK (template_content_hash ~ '^[0-9a-f]{64}$'),
    renderer_version      text        NOT NULL CHECK (char_length(renderer_version) BETWEEN 1 AND 64),
    file_name             text        NOT NULL,
    mime_type             text        NOT NULL CHECK (mime_type = 'text/plain'),
    content_sha256        text        NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    size_bytes            integer     NOT NULL CHECK (size_bytes BETWEEN 1 AND 1048576),
    content               bytea       NOT NULL,
    created_at            timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, quote_id, revision),
    UNIQUE (tenant_id, company_id, number),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_revision (tenant_id, company_id, quote_id, revision),
    -- The file is named by its number, and the bytes are the bound ones (read verifies them again).
    CHECK (file_name = number || '.txt'),
    CHECK (octet_length(content) = size_bytes AND encode(sha256(content), 'hex') = content_sha256)
);

CREATE TABLE brant_trd.quote_commit_evidence (
    tenant_id                    text        NOT NULL,
    company_id                   text        NOT NULL,
    quote_id                     text        NOT NULL,
    revision                     bigint      NOT NULL,
    command                      text        NOT NULL CHECK (command IN ('quote.activate', 'quote.replace')),
    idempotency_key              text        NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    business_hash                text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    approval_id                  text        NOT NULL,
    approval_effectivity_version bigint      NOT NULL CHECK (approval_effectivity_version = 1),
    replaced_revision            bigint      CHECK (replaced_revision > 0),
    number                       text        NOT NULL,
    content_sha256               text        NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    cost_snapshot_id             text        NOT NULL,
    authority_epoch              bigint      NOT NULL CHECK (authority_epoch > 0),
    control_epoch                bigint      NOT NULL CHECK (control_epoch >= 0),
    root_version                 bigint      NOT NULL CHECK (root_version > 0),
    committed_by_kind            text        NOT NULL CHECK (committed_by_kind = 'human'),
    committed_by_id              text        NOT NULL,
    committed_at                 timestamptz NOT NULL,
    -- The evidence entries as canonical JSON ([{"name": ..., "value": ...}, ...]) and their SHA-256.
    entries                      text        NOT NULL CHECK (left(entries, 1) = '['),
    entries_sha256               text        NOT NULL CHECK (entries_sha256 = encode(sha256(convert_to(entries, 'UTF8')), 'hex')),
    PRIMARY KEY (tenant_id, company_id, quote_id, revision),
    UNIQUE (tenant_id, company_id, quote_id, root_version),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_document (tenant_id, company_id, quote_id, revision),
    FOREIGN KEY (tenant_id, company_id, quote_id, approval_id) REFERENCES brant_trd.quote_approval (tenant_id, company_id, quote_id, approval_id),
    CHECK ((command = 'quote.replace') = (replaced_revision IS NOT NULL))
);

CREATE TABLE brant_trd.quote_revision_closure (
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    quote_id       text        NOT NULL,
    revision       bigint      NOT NULL,
    status         text        NOT NULL CHECK (status IN ('withdrawn', 'cancelled')),
    closed_by_kind text        NOT NULL CHECK (closed_by_kind = 'human'),
    closed_by_id   text        NOT NULL,
    close_epoch    bigint      NOT NULL CHECK (close_epoch > 0),
    closed_at      timestamptz NOT NULL,
    reason         text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 2000),
    evidence       text        NOT NULL CHECK (left(evidence, 1) = '['),
    root_version   bigint      NOT NULL CHECK (root_version > 0),
    PRIMARY KEY (tenant_id, company_id, quote_id, revision),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_revision (tenant_id, company_id, quote_id, revision)
);

CREATE TABLE brant_trd.quote_risk_hold (
    tenant_id           text        NOT NULL,
    company_id          text        NOT NULL,
    quote_id            text        NOT NULL,
    hold_id             text        NOT NULL,
    revision            bigint      NOT NULL,
    cause               text        NOT NULL CHECK (cause = 'approval_revoked'),
    approval_id         text        NOT NULL,
    code                text        NOT NULL CHECK (code = 'APPROVAL_REVOKED'),
    registered_by_kind  text        NOT NULL CHECK (registered_by_kind = 'human'),
    registered_by_id    text        NOT NULL,
    register_epoch      bigint      NOT NULL CHECK (register_epoch > 0),
    registered_at       timestamptz NOT NULL,
    root_version        bigint      NOT NULL CHECK (root_version > 0),
    PRIMARY KEY (tenant_id, company_id, quote_id, hold_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_revision (tenant_id, company_id, quote_id, revision),
    FOREIGN KEY (tenant_id, company_id, quote_id, approval_id) REFERENCES brant_trd.quote_approval (tenant_id, company_id, quote_id, approval_id),
    CHECK (hold_id = cause || ':' || approval_id)
);

CREATE TABLE brant_trd.quote_conversion_slot (
    tenant_id     text        NOT NULL,
    company_id    text        NOT NULL,
    quote_id      text        NOT NULL,
    slot          text        NOT NULL CHECK (slot = 'full_v1'),
    revision      bigint      NOT NULL,
    business_hash text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    order_id      text        NOT NULL CHECK (order_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    taken_by_kind text        NOT NULL CHECK (taken_by_kind = 'human'),
    taken_by_id   text        NOT NULL,
    take_epoch    bigint      NOT NULL CHECK (take_epoch > 0),
    taken_at      timestamptz NOT NULL,
    root_version  bigint      NOT NULL CHECK (root_version > 0),
    PRIMARY KEY (tenant_id, company_id, quote_id, slot),
    UNIQUE (tenant_id, company_id, order_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_revision (tenant_id, company_id, quote_id, revision)
);

-- A formal document is written for a revision pending approval at its business hash; its commit
-- evidence binds that document, number and file hash, and a valid approval of the revision for the
-- command's target (a replacement: of the revision it supersedes, while no conversion slot is taken).
-- Neither is ever updated or deleted.
CREATE FUNCTION brant_trd.guard_quote_document() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_document: a formal document never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision AND r.status = 'pending_approval'
            AND r.business_hash = NEW.business_hash) THEN
        RAISE EXCEPTION 'brant_trd.quote_document: a formal document is written for a revision pending approval at its business hash'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_document_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_document
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_document();

CREATE FUNCTION brant_trd.guard_quote_commit_evidence() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_commit_evidence: commit evidence never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision AND r.status = 'pending_approval'
                AND r.business_hash = NEW.business_hash)
            OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_document d WHERE d.tenant_id = NEW.tenant_id AND d.company_id = NEW.company_id
                AND d.quote_id = NEW.quote_id AND d.revision = NEW.revision AND d.business_hash = NEW.business_hash
                AND d.number = NEW.number AND d.content_sha256 = NEW.content_sha256)
            OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_approval a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                AND a.quote_id = NEW.quote_id AND a.approval_id = NEW.approval_id AND a.revision = NEW.revision
                AND a.business_hash = NEW.business_hash AND a.effectivity = 'valid' AND a.target_command = NEW.command
                AND a.expected_active_revision IS NOT DISTINCT FROM NEW.replaced_revision) THEN
        RAISE EXCEPTION 'brant_trd.quote_commit_evidence: commit evidence binds the revision''s formal document and a valid approval for the command'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.command = 'quote.replace' AND EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id) THEN
        RAISE EXCEPTION 'brant_trd.quote_commit_evidence: ROOT_CONVERSION_LOCKED: the conversion slot is taken'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_commit_evidence_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_commit_evidence
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_commit_evidence();

-- A withdrawal closes a frozen or pending revision; a cancellation the active revision of a root
-- whose conversion slot is free. Never changed or deleted.
CREATE FUNCTION brant_trd.guard_quote_revision_closure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_revision_closure: a closure never changes' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
            AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
            AND r.status = ANY (CASE NEW.status WHEN 'withdrawn' THEN ARRAY['frozen', 'pending_approval'] ELSE ARRAY['active'] END)) THEN
        RAISE EXCEPTION 'brant_trd.quote_revision_closure: a withdrawal closes a frozen or pending revision, a cancellation the active one'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'cancelled' AND EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id
            AND s.company_id = NEW.company_id AND s.quote_id = NEW.quote_id) THEN
        RAISE EXCEPTION 'brant_trd.quote_revision_closure: ROOT_CONVERSION_LOCKED: the conversion slot is taken'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_revision_closure_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_revision_closure
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_revision_closure();

-- A risk hold is registered on the active revision when the approval its commit evidence names is
-- revoked. Never changed or deleted.
CREATE FUNCTION brant_trd.guard_quote_risk_hold() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a risk hold never changes and is never lifted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r
            JOIN brant_trd.quote_commit_evidence e ON e.tenant_id = r.tenant_id AND e.company_id = r.company_id AND e.quote_id = r.quote_id
                AND e.revision = r.revision
            JOIN brant_trd.quote_approval a ON a.tenant_id = e.tenant_id AND a.company_id = e.company_id AND a.quote_id = e.quote_id
                AND a.approval_id = e.approval_id
            WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'active' AND e.approval_id = NEW.approval_id AND a.effectivity = 'revoked') THEN
        RAISE EXCEPTION 'brant_trd.quote_risk_hold: a hold is registered on the active revision whose formalization approval was revoked'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_risk_hold_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_risk_hold
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_risk_hold();

-- The conversion slot is taken for the active revision at its business hash while it is not held,
-- and kept for good.
CREATE FUNCTION brant_trd.guard_quote_conversion_slot() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_trd.quote_conversion_slot: a conversion slot is kept for good' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id AND r.company_id = NEW.company_id
                AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision AND r.status = 'active' AND r.business_hash = NEW.business_hash)
            OR EXISTS (SELECT 1 FROM brant_trd.quote_risk_hold h WHERE h.tenant_id = NEW.tenant_id AND h.company_id = NEW.company_id
                AND h.quote_id = NEW.quote_id AND h.revision = NEW.revision) THEN
        RAISE EXCEPTION 'brant_trd.quote_conversion_slot: the slot is taken for the active, unheld revision at its business hash'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_conversion_slot_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_conversion_slot
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_conversion_slot();

-- A sealed revision changes its status along SM-TRD-REVISION only, and nothing else of it changes:
-- frozen ↔ pending_approval (review); pending_approval → active with its commit evidence (activation,
-- replacement); active → superseded by the replacement whose commit evidence names it; frozen or
-- pending_approval → withdrawn and active → cancelled with their closure; neither superseded nor
-- cancelled while the conversion slot is taken. A working revision is edited, frozen or discarded as
-- before; revisions are never deleted.
CREATE OR REPLACE FUNCTION brant_trd.guard_quote_revision() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    same    boolean;
    slotted boolean;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.quote_revision: revisions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'working' THEN
            RAISE EXCEPTION 'brant_trd.quote_revision: a revision is created working' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.tenant_id, NEW.company_id, NEW.quote_id, NEW.revision, NEW.base_revision)
            IS DISTINCT FROM (OLD.tenant_id, OLD.company_id, OLD.quote_id, OLD.revision, OLD.base_revision) THEN
        RAISE EXCEPTION 'brant_trd.quote_revision: identity is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'working' AND NEW.status IN ('working', 'frozen', 'discarded') THEN
        RETURN NEW;
    END IF;
    same := (to_jsonb(OLD) - 'status') = (to_jsonb(NEW) - 'status');
    slotted := EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s WHERE s.tenant_id = NEW.tenant_id AND s.company_id = NEW.company_id
        AND s.quote_id = NEW.quote_id);
    IF same AND (OLD.status, NEW.status) IN (('frozen', 'pending_approval'), ('pending_approval', 'frozen')) THEN
        RETURN NEW;
    END IF;
    IF same AND OLD.status = 'pending_approval' AND NEW.status = 'active' AND EXISTS (SELECT 1 FROM brant_trd.quote_commit_evidence e
            WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id AND e.quote_id = NEW.quote_id AND e.revision = NEW.revision
            AND e.business_hash = NEW.business_hash) THEN
        RETURN NEW;
    END IF;
    IF same AND OLD.status = 'active' AND NEW.status = 'superseded' AND NOT slotted AND EXISTS (SELECT 1
            FROM brant_trd.quote_commit_evidence e WHERE e.tenant_id = NEW.tenant_id AND e.company_id = NEW.company_id
            AND e.quote_id = NEW.quote_id AND e.replaced_revision = NEW.revision) THEN
        RETURN NEW;
    END IF;
    IF same AND ((OLD.status IN ('frozen', 'pending_approval') AND NEW.status = 'withdrawn')
                 OR (OLD.status = 'active' AND NEW.status = 'cancelled' AND NOT slotted))
            AND EXISTS (SELECT 1 FROM brant_trd.quote_revision_closure c WHERE c.tenant_id = NEW.tenant_id AND c.company_id = NEW.company_id
                AND c.quote_id = NEW.quote_id AND c.revision = NEW.revision AND c.status = NEW.status) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.quote_revision: revision % is %; its content and business hash do not change and it does not become %',
        OLD.revision, OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
END
$$;

ALTER TABLE brant_trd.quote_document ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_document FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_document_scope ON brant_trd.quote_document
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_commit_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_commit_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_commit_evidence_scope ON brant_trd.quote_commit_evidence
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_revision_closure ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_revision_closure FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_revision_closure_scope ON brant_trd.quote_revision_closure
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_risk_hold ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_risk_hold FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_risk_hold_scope ON brant_trd.quote_risk_hold
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_conversion_slot ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_conversion_slot FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_conversion_slot_scope ON brant_trd.quote_conversion_slot
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

-- QuoteRevisionGuard v1: whether a formal revision of a quote is still active, unheld and
-- convertible, for a caller inside its own transaction (ADR-BR-004 option B; ADR-BR-011: the caller
-- locks its own root first). It locks the quote root FOR SHARE, so quote.replace, quote.cancel,
-- quote.approval.revoke and the order step taking the conversion slot (which lock it FOR UPDATE)
-- either committed before and the verdict says so, or wait until the caller's transaction ends. In
-- READ COMMITTED a guard that waited reads the rows as that command left them.
--
-- It always returns one row in the caller's scope (app.tenant_id, app.company_id). known is false,
-- with code REFERENCE_INVALID, for a quote or revision that does not exist there or a revision never
-- formalized. Otherwise status is the revision's, active_revision the root's (0: none), number and
-- content_sha256 those of its formal document, expires_at its validity end; code is NULL when the
-- revision is active at p_business_hash and unheld, BUSINESS_HASH_MISMATCH for another hash,
-- QUOTE_NOT_CURRENT when it is no longer (or not) the active revision, and the hold's code
-- (APPROVAL_REVOKED) when a risk hold pauses it. conversion_taken says whether the root's conversion
-- slot is taken (CONVERSION_SLOT_CONFLICT for a second conversion). The data scope (own, team) is
-- the caller's to check before its transaction (TRD's lookups). SECURITY DEFINER, fixed
-- search_path, qualified names; brant_app alone may execute it.
CREATE FUNCTION brant_trd.guard_quote_revision_active_v1(p_quote_id text, p_revision bigint, p_business_hash text)
    RETURNS TABLE (known boolean, status text, active_revision bigint, business_hash text, number text, content_sha256 text,
                   expires_at timestamptz, on_hold boolean, conversion_taken boolean, code text)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_tenant  text := pg_catalog.current_setting('app.tenant_id', true);
    v_company text := pg_catalog.current_setting('app.company_id', true);
    v_rev     record;
    v_doc     record;
    v_active  bigint;
    v_hold    text;
    v_slot    boolean;
BEGIN
    IF p_quote_id IS NULL OR p_revision IS NULL OR p_business_hash IS NULL THEN
        RAISE EXCEPTION 'brant_trd.guard_quote_revision_active_v1: quote_id, revision and business_hash are required'
            USING ERRCODE = 'null_value_not_allowed';
    END IF;
    PERFORM 1 FROM brant_trd.quote_root q
    WHERE q.tenant_id = v_tenant AND q.company_id = v_company AND q.quote_id = p_quote_id
    FOR SHARE;
    IF NOT FOUND THEN
        RETURN QUERY SELECT false, NULL::text, NULL::bigint, NULL::text, NULL::text, NULL::text, NULL::timestamptz, false, false,
            'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    SELECT r.status, r.business_hash, r.expires_at INTO v_rev
    FROM brant_trd.quote_revision r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.quote_id = p_quote_id AND r.revision = p_revision;
    SELECT d.number, d.content_sha256 INTO v_doc
    FROM brant_trd.quote_document d
    WHERE d.tenant_id = v_tenant AND d.company_id = v_company AND d.quote_id = p_quote_id AND d.revision = p_revision;
    SELECT coalesce(max(r.revision), 0) INTO v_active
    FROM brant_trd.quote_revision r
    WHERE r.tenant_id = v_tenant AND r.company_id = v_company AND r.quote_id = p_quote_id AND r.status = 'active';
    v_slot := EXISTS (SELECT 1 FROM brant_trd.quote_conversion_slot s
        WHERE s.tenant_id = v_tenant AND s.company_id = v_company AND s.quote_id = p_quote_id);
    IF v_rev.status IS NULL OR v_doc.number IS NULL THEN
        RETURN QUERY SELECT false, v_rev.status, v_active, v_rev.business_hash, NULL::text, NULL::text, v_rev.expires_at, false, v_slot,
            'REFERENCE_INVALID'::text;
        RETURN;
    END IF;
    SELECT min(h.code) INTO v_hold
    FROM brant_trd.quote_risk_hold h
    WHERE h.tenant_id = v_tenant AND h.company_id = v_company AND h.quote_id = p_quote_id AND h.revision = p_revision;
    RETURN QUERY SELECT true, v_rev.status, v_active, v_rev.business_hash, v_doc.number, v_doc.content_sha256, v_rev.expires_at,
        v_hold IS NOT NULL, v_slot,
        CASE
            WHEN v_rev.business_hash <> p_business_hash THEN 'BUSINESS_HASH_MISMATCH'
            WHEN v_rev.status <> 'active' THEN 'QUOTE_NOT_CURRENT'
            WHEN v_hold IS NOT NULL THEN v_hold
        END::text;
END
$$;

REVOKE ALL ON FUNCTION brant_trd.guard_quote_revision_active_v1(text, bigint, text) FROM PUBLIC;
