-- TRD: review requests, approvals and margin exceptions of frozen quote revisions (WBS S3-03,
-- S3-14; tactical model SM-TRD-REVISION, SM-TRD-REVIEW, SM-TRD-APPROVAL; TRD-INV-003, 004, 005, 007;
-- Python t03_storage t03_review_request, t03_approval and t03_approval_event).
--
-- A review request asks an independent human to approve one frozen revision, at its business hash,
-- for quote.activate or quote.replace (with the active revision a replacement replaces). A revision
-- is reviewed once and a root has one open review. Submitting moves the revision frozen →
-- pending_approval; returning or rejecting it moves it back to frozen. The guard trigger of
-- 000001 is replaced so that a sealed revision changes its status along those two transitions
-- only, and nothing else of it ever changes; activation, replacement, withdrawal and
-- cancellation replace it again when they come.
--
-- An approval binds its review's revision, business hash and target, and the rule versions the
-- revision was calculated under (normalization version, calculation version, pricing policy
-- revision). It is valid until revoked (effectivity_version 1 → 2); it is never deleted or edited
-- otherwise. A margin exception approves a margin below the pricing policy's minimum explicitly
-- (TRD-INV-007): it binds the same hash and rule versions and the exact shortfall, and is never
-- edited or deleted; it lapses with its approval. Whether an approval releases its revision is
-- decided at the use, on the reviewers' grants then.
--
-- Rows live in the root's transaction: every write locks the quote root FOR UPDATE first. Evidence
-- is stored as JSON text, as in 000001.

CREATE TABLE brant_trd.quote_review (
    tenant_id                 text        NOT NULL,
    company_id                text        NOT NULL,
    quote_id                  text        NOT NULL,
    review_id                 text        NOT NULL CHECK (review_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    revision                  bigint      NOT NULL,
    business_hash             text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    target_command            text        NOT NULL CHECK (target_command IN ('quote.activate', 'quote.replace')),
    expected_active_revision  bigint      CHECK (expected_active_revision > 0),
    margin_exception_required boolean     NOT NULL,
    status                    text        NOT NULL CHECK (status IN ('open', 'approved', 'returned', 'rejected', 'cancelled')),
    requested_by_kind         text        NOT NULL CHECK (requested_by_kind = 'human'),
    requested_by_id           text        NOT NULL,
    requested_epoch           bigint      NOT NULL CHECK (requested_epoch > 0),
    requested_at              timestamptz NOT NULL,
    decided_by_kind           text        CHECK (decided_by_kind = 'human'),
    decided_by_id             text,
    decided_epoch             bigint      CHECK (decided_epoch > 0),
    decided_at                timestamptz,
    decision_reason           text        CHECK (char_length(decision_reason) BETWEEN 1 AND 2000),
    decision_evidence         text        CHECK (left(decision_evidence, 1) = '['),
    PRIMARY KEY (tenant_id, company_id, quote_id, review_id),
    UNIQUE (tenant_id, company_id, quote_id, revision),
    FOREIGN KEY (tenant_id, company_id, quote_id, revision) REFERENCES brant_trd.quote_revision (tenant_id, company_id, quote_id, revision),
    CHECK ((target_command = 'quote.replace') = (expected_active_revision IS NOT NULL)),
    -- An open review is undecided; a decided one records who decided, under which epoch and when,
    -- and a return or rejection why.
    CHECK ((status = 'open') = (decided_at IS NULL)),
    CHECK ((decided_at IS NULL) = (decided_by_kind IS NULL) AND (decided_at IS NULL) = (decided_by_id IS NULL)
        AND (decided_at IS NULL) = (decided_epoch IS NULL) AND (decided_at IS NULL) = (decision_evidence IS NULL)),
    CHECK (status NOT IN ('returned', 'rejected') OR decision_reason IS NOT NULL)
);

-- One open review per root.
CREATE UNIQUE INDEX quote_review_open ON brant_trd.quote_review (tenant_id, company_id, quote_id) WHERE status = 'open';

CREATE TABLE brant_trd.quote_approval (
    tenant_id                text        NOT NULL,
    company_id               text        NOT NULL,
    quote_id                 text        NOT NULL,
    approval_id              text        NOT NULL CHECK (approval_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    review_id                text        NOT NULL,
    revision                 bigint      NOT NULL,
    business_hash            text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    target_command           text        NOT NULL CHECK (target_command IN ('quote.activate', 'quote.replace')),
    expected_active_revision bigint      CHECK (expected_active_revision > 0),
    normalization_version    text        NOT NULL CHECK (char_length(normalization_version) BETWEEN 1 AND 64),
    calculation_version      text        NOT NULL CHECK (char_length(calculation_version) BETWEEN 1 AND 64),
    pricing_policy_kind      text        NOT NULL CHECK (pricing_policy_kind = 'pricing_policy'),
    pricing_policy_id        text        NOT NULL,
    pricing_policy_revision  bigint      NOT NULL CHECK (pricing_policy_revision > 0),
    approved_by_kind         text        NOT NULL CHECK (approved_by_kind = 'human'),
    approved_by_id           text        NOT NULL,
    approved_epoch           bigint      NOT NULL CHECK (approved_epoch > 0),
    approved_at              timestamptz NOT NULL,
    approval_reason          text        CHECK (char_length(approval_reason) BETWEEN 1 AND 2000),
    approval_evidence        text        NOT NULL CHECK (left(approval_evidence, 1) = '['),
    expires_at               timestamptz NOT NULL,
    effectivity              text        NOT NULL CHECK (effectivity IN ('valid', 'revoked')),
    effectivity_version      bigint      NOT NULL CHECK (effectivity_version > 0),
    revoked_by_kind          text        CHECK (revoked_by_kind = 'human'),
    revoked_by_id            text,
    revoke_epoch             bigint      CHECK (revoke_epoch > 0),
    revoked_at               timestamptz,
    revoke_reason            text        CHECK (char_length(revoke_reason) BETWEEN 1 AND 2000),
    revoke_evidence          text        CHECK (left(revoke_evidence, 1) = '['),
    PRIMARY KEY (tenant_id, company_id, quote_id, approval_id),
    UNIQUE (tenant_id, company_id, quote_id, review_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, review_id) REFERENCES brant_trd.quote_review (tenant_id, company_id, quote_id, review_id),
    CHECK ((target_command = 'quote.replace') = (expected_active_revision IS NOT NULL)),
    -- An approval lapses after it is given (and, the domain checks, before the quote expires).
    CHECK (expires_at > approved_at),
    -- A valid approval is at effectivity version 1; a revoked one at 2, with who revoked it, when, why
    -- and on which evidence.
    CHECK (CASE WHEN effectivity = 'valid'
        THEN effectivity_version = 1 AND revoked_by_kind IS NULL AND revoked_by_id IS NULL AND revoke_epoch IS NULL AND revoked_at IS NULL
             AND revoke_reason IS NULL AND revoke_evidence IS NULL
        ELSE effectivity_version = 2 AND revoked_by_kind IS NOT NULL AND revoked_by_id IS NOT NULL AND revoke_epoch IS NOT NULL
             AND revoked_at IS NOT NULL AND revoke_reason IS NOT NULL AND revoke_evidence IS NOT NULL END)
);

CREATE TABLE brant_trd.quote_margin_exception (
    tenant_id               text        NOT NULL,
    company_id              text        NOT NULL,
    quote_id                text        NOT NULL,
    approval_id             text        NOT NULL,
    business_hash           text        NOT NULL CHECK (business_hash ~ '^[0-9a-f]{64}$'),
    normalization_version   text        NOT NULL,
    calculation_version     text        NOT NULL,
    pricing_policy_kind     text        NOT NULL CHECK (pricing_policy_kind = 'pricing_policy'),
    pricing_policy_id       text        NOT NULL,
    pricing_policy_revision bigint      NOT NULL CHECK (pricing_policy_revision > 0),
    -- Exact decimals as the frozen calculation states them: the minimum margin (0–1) and the positive
    -- shortfall in major units of the comparison currency.
    minimum_margin          text        NOT NULL CHECK (minimum_margin ~ '^(0(\.[0-9]{1,12})?|1)$'),
    shortfall               text        NOT NULL CHECK (shortfall ~ '^(0|[1-9][0-9]{0,23})(\.[0-9]{1,16})?$' AND shortfall ~ '[1-9]'),
    currency                text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    approved_by_kind        text        NOT NULL CHECK (approved_by_kind = 'human'),
    approved_by_id          text        NOT NULL,
    approved_epoch          bigint      NOT NULL CHECK (approved_epoch > 0),
    approved_at             timestamptz NOT NULL,
    reason                  text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 2000),
    evidence                text        NOT NULL CHECK (left(evidence, 1) = '['),
    PRIMARY KEY (tenant_id, company_id, quote_id, approval_id),
    FOREIGN KEY (tenant_id, company_id, quote_id, approval_id) REFERENCES brant_trd.quote_approval (tenant_id, company_id, quote_id, approval_id)
);

-- A sealed revision's status moves frozen → pending_approval (quote.submit_review) and back
-- (quote.review.decide return or reject), and nothing else of it changes; a working revision is
-- edited, frozen or discarded as before; a discarded revision never changes; revisions are never
-- deleted. Activation and the later transitions come with their own migration.
CREATE OR REPLACE FUNCTION brant_trd.guard_quote_revision() RETURNS trigger LANGUAGE plpgsql AS $$
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
    IF (OLD.status, NEW.status) IN (('frozen', 'pending_approval'), ('pending_approval', 'frozen'))
            AND (to_jsonb(OLD) - 'status') = (to_jsonb(NEW) - 'status') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.quote_revision: revision % is %; its content and business hash do not change and it does not become %',
        OLD.revision, OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
END
$$;

-- A review is created open for a revision pending approval at that revision's business hash; it is
-- decided once (open → approved, returned, rejected or cancelled), recording the decision and
-- nothing else; it is never deleted.
CREATE FUNCTION brant_trd.guard_quote_review() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    decision constant text[] := ARRAY['status', 'decided_by_kind', 'decided_by_id', 'decided_epoch', 'decided_at', 'decision_reason',
        'decision_evidence'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.quote_review: reviews are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'open' OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_revision r WHERE r.tenant_id = NEW.tenant_id
                AND r.company_id = NEW.company_id AND r.quote_id = NEW.quote_id AND r.revision = NEW.revision
                AND r.status = 'pending_approval' AND r.business_hash = NEW.business_hash) THEN
            RAISE EXCEPTION 'brant_trd.quote_review: a review is opened for a revision pending approval at its business hash'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> 'open' OR NEW.status = 'open'
            OR (to_jsonb(OLD) - decision) IS DISTINCT FROM (to_jsonb(NEW) - decision) THEN
        RAISE EXCEPTION 'brant_trd.quote_review: review % is %; it is decided once and nothing else changes', OLD.review_id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_review_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_review
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_review();

-- An approval is created valid for an approved review, binding the review's revision, hash and
-- target; it is revoked once (valid → revoked, effectivity version + 1), recording the revocation
-- and nothing else; it is never deleted.
CREATE FUNCTION brant_trd.guard_quote_approval() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    revocation constant text[] := ARRAY['effectivity', 'effectivity_version', 'revoked_by_kind', 'revoked_by_id', 'revoke_epoch',
        'revoked_at', 'revoke_reason', 'revoke_evidence'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.quote_approval: approvals are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.effectivity <> 'valid' OR NOT EXISTS (SELECT 1 FROM brant_trd.quote_review v WHERE v.tenant_id = NEW.tenant_id
                AND v.company_id = NEW.company_id AND v.quote_id = NEW.quote_id AND v.review_id = NEW.review_id AND v.status = 'approved'
                AND v.revision = NEW.revision AND v.business_hash = NEW.business_hash AND v.target_command = NEW.target_command
                AND v.expected_active_revision IS NOT DISTINCT FROM NEW.expected_active_revision) THEN
            RAISE EXCEPTION 'brant_trd.quote_approval: an approval is created valid and binds its approved review'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.effectivity <> 'valid' OR NEW.effectivity <> 'revoked' OR NEW.effectivity_version <> OLD.effectivity_version + 1
            OR (to_jsonb(OLD) - revocation) IS DISTINCT FROM (to_jsonb(NEW) - revocation) THEN
        RAISE EXCEPTION 'brant_trd.quote_approval: approval % is %; it is revoked once and nothing else changes', OLD.approval_id,
            OLD.effectivity USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER quote_approval_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_approval
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_approval();

-- A margin exception is added to a valid approval, binding its hash and rule versions; it never
-- changes and is never deleted.
CREATE FUNCTION brant_trd.guard_quote_margin_exception() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NOT EXISTS (SELECT 1 FROM brant_trd.quote_approval a WHERE a.tenant_id = NEW.tenant_id AND a.company_id = NEW.company_id
                AND a.quote_id = NEW.quote_id AND a.approval_id = NEW.approval_id AND a.effectivity = 'valid'
                AND a.business_hash = NEW.business_hash AND a.normalization_version = NEW.normalization_version
                AND a.calculation_version = NEW.calculation_version AND a.pricing_policy_kind = NEW.pricing_policy_kind
                AND a.pricing_policy_id = NEW.pricing_policy_id AND a.pricing_policy_revision = NEW.pricing_policy_revision) THEN
            RAISE EXCEPTION 'brant_trd.quote_margin_exception: an exception binds a valid approval''s hash and rules'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'brant_trd.quote_margin_exception: margin exceptions never change' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER quote_margin_exception_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_trd.quote_margin_exception
    FOR EACH ROW EXECUTE FUNCTION brant_trd.guard_quote_margin_exception();

ALTER TABLE brant_trd.quote_review ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_review FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_review_scope ON brant_trd.quote_review
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_approval ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_approval FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_approval_scope ON brant_trd.quote_approval
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_trd.quote_margin_exception ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_trd.quote_margin_exception FORCE ROW LEVEL SECURITY;
CREATE POLICY quote_margin_exception_scope ON brant_trd.quote_margin_exception
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
