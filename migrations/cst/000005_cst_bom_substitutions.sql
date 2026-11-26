-- CST: cost BOM material substitutions (WBS S7-02). A request records the original line, the
-- substitute, the quantity and technical revision it covers, and the cost and lead-time delta.
-- cost_bom.substitution.decide is a different human from the requester. Approval appends a new
-- cost BOM revision in the same transaction (the application does that); this table never rewrites
-- a decided row, so an issued, processed or delivered fact cannot be rewritten from here either.
-- This slice has no issued, processed or delivered facts. The trigger still forbids changing a
-- decided row.

CREATE TABLE brant_cst.cost_bom_substitution (
    tenant_id                 text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id                text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    substitution_id           text        NOT NULL CHECK (substitution_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    bom_id                    text        NOT NULL,
    original_line_id          text        NOT NULL CHECK (original_line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    original_material_code    text        NOT NULL CHECK (original_material_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    substitute_material_code  text        NOT NULL CHECK (substitute_material_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    applicable_quantity       text        NOT NULL CHECK (applicable_quantity ~ '^(0|[1-9][0-9]*)(\.[0-9]+)?$'),
    target_spec_revision      bigint      NOT NULL CHECK (target_spec_revision > 0),
    cost_delta_minor          bigint      NOT NULL,
    currency                  text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    minor_units               smallint    NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    lead_days_delta           integer     NOT NULL CHECK (lead_days_delta BETWEEN -3650 AND 3650),
    sample_impact             boolean     NOT NULL,
    reason                    text        NOT NULL CHECK (char_length(reason) BETWEEN 1 AND 1000),
    status                    text        NOT NULL CHECK (status IN ('requested', 'approved', 'rejected')),
    requested_by_kind         text        NOT NULL CHECK (requested_by_kind IN ('human', 'ai')),
    requested_by_id           text        NOT NULL,
    requested_at              timestamptz NOT NULL,
    decided_by_kind           text        CHECK (decided_by_kind = 'human'),
    decided_by_id             text,
    decided_at                timestamptz,
    authority_epoch           bigint      CHECK (authority_epoch > 0),
    decided_revision          bigint      CHECK (decided_revision > 0),
    PRIMARY KEY (tenant_id, company_id, substitution_id),
    FOREIGN KEY (tenant_id, company_id, bom_id) REFERENCES brant_cst.cost_bom (tenant_id, company_id, bom_id),
    CHECK (status <> 'requested' OR (decided_by_id IS NULL AND decided_at IS NULL AND decided_revision IS NULL)),
    CHECK (status = 'requested' OR (decided_by_kind = 'human' AND decided_by_id IS NOT NULL AND decided_by_id <> requested_by_id
        AND decided_at IS NOT NULL AND authority_epoch IS NOT NULL)),
    CHECK (status <> 'approved' OR decided_revision IS NOT NULL)
);

CREATE INDEX cost_bom_substitution_bom ON brant_cst.cost_bom_substitution (tenant_id, company_id, bom_id, substitution_id);

-- A decided row is immutable. The only update is the decision itself, by someone other than the
-- requester. Deletes never happen. There is no path here onto issued, processed or delivered rows.
CREATE FUNCTION brant_cst.guard_cost_bom_substitution() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_substitution: a substitution is never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'requested' THEN
            RAISE EXCEPTION 'brant_cst.cost_bom_substitution: a substitution starts requested'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.status <> 'requested' THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_substitution: a decided substitution does not change'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status NOT IN ('approved', 'rejected')
            OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.company_id IS DISTINCT FROM OLD.company_id
            OR NEW.substitution_id IS DISTINCT FROM OLD.substitution_id OR NEW.bom_id IS DISTINCT FROM OLD.bom_id
            OR NEW.original_line_id IS DISTINCT FROM OLD.original_line_id
            OR NEW.original_material_code IS DISTINCT FROM OLD.original_material_code
            OR NEW.substitute_material_code IS DISTINCT FROM OLD.substitute_material_code
            OR NEW.applicable_quantity IS DISTINCT FROM OLD.applicable_quantity
            OR NEW.target_spec_revision IS DISTINCT FROM OLD.target_spec_revision
            OR NEW.cost_delta_minor IS DISTINCT FROM OLD.cost_delta_minor
            OR NEW.currency IS DISTINCT FROM OLD.currency OR NEW.minor_units IS DISTINCT FROM OLD.minor_units
            OR NEW.lead_days_delta IS DISTINCT FROM OLD.lead_days_delta
            OR NEW.sample_impact IS DISTINCT FROM OLD.sample_impact
            OR NEW.reason IS DISTINCT FROM OLD.reason
            OR NEW.requested_by_id IS DISTINCT FROM OLD.requested_by_id
            OR NEW.requested_at IS DISTINCT FROM OLD.requested_at THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_substitution: a decision does not rewrite the request'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.decided_by_id IS NULL OR NEW.decided_by_id = OLD.requested_by_id THEN
        RAISE EXCEPTION 'brant_cst.cost_bom_substitution: the requester does not decide'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER cost_bom_substitution_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_bom_substitution
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_bom_substitution();

ALTER TABLE brant_cst.cost_bom_substitution ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_bom_substitution FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_bom_substitution_scope ON brant_cst.cost_bom_substitution
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
