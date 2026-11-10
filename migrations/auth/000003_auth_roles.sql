-- foundation/authorization: role templates and separation of duties (BC-D02, simulated review
-- SR-32). The default templates are product code generated from the master archive; a tenant may
-- define roles that merge templates (brant_auth.role) but never a forbidden combination, and an
-- actor may hold roles whose templates together hold none either. Assigning roles derives the
-- actor's permissions in actor_grant, which bumps the authority epoch when access changes.
-- Assignments are ended, never deleted, so who held which role and when stays readable.

CREATE TABLE brant_auth.role (
    tenant_id     text        NOT NULL,
    company_id    text        NOT NULL,
    role_code     text        NOT NULL CHECK (role_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    name          text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 200),
    templates     text[]      NOT NULL CHECK (cardinality(templates) BETWEEN 1 AND 15 AND array_position(templates, NULL) IS NULL),
    defined_epoch bigint      NOT NULL CHECK (defined_epoch > 0),
    defined_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, role_code),
    FOREIGN KEY (tenant_id, company_id) REFERENCES brant_auth.authority_scope (tenant_id, company_id)
);

CREATE TABLE brant_auth.actor_role (
    assignment_id  bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id      text        NOT NULL,
    company_id     text        NOT NULL,
    actor_id       text        NOT NULL,
    role_code      text        NOT NULL CHECK (role_code ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    assigned_epoch bigint      NOT NULL CHECK (assigned_epoch > 0),
    assigned_at    timestamptz NOT NULL,
    removed_epoch  bigint      CHECK (removed_epoch >= assigned_epoch),
    removed_at     timestamptz,
    CHECK ((removed_epoch IS NULL) = (removed_at IS NULL)),
    FOREIGN KEY (tenant_id, company_id, actor_id) REFERENCES brant_auth.actor_grant (tenant_id, company_id, actor_id)
);
CREATE UNIQUE INDEX actor_role_current ON brant_auth.actor_role (tenant_id, company_id, actor_id, role_code)
    WHERE removed_epoch IS NULL;

ALTER TABLE brant_auth.role ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_auth.role FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_auth.actor_role ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_auth.actor_role FORCE ROW LEVEL SECURITY;

CREATE POLICY role_scope ON brant_auth.role
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
CREATE POLICY actor_role_scope ON brant_auth.actor_role
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
