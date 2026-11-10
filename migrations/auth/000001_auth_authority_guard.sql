-- foundation/authorization: the local authorization projection and the authority guard row of
-- each tenant and company scope (Python t03_authority_scope / authority_epoch).
--
-- A command locks its scope row and then its grant row FOR SHARE inside its own transaction; a
-- grant change locks the scope row FOR NO KEY UPDATE first and increments authority_epoch. Grants
-- are deactivated, never deleted. Both tables are FORCE RLS on app.tenant_id and app.company_id.

CREATE SCHEMA brant_auth;

CREATE TABLE brant_auth.authority_scope (
    tenant_id       text   NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text   NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    authority_epoch bigint NOT NULL DEFAULT 1 CHECK (authority_epoch > 0),
    PRIMARY KEY (tenant_id, company_id)
);

CREATE TABLE brant_auth.actor_grant (
    tenant_id     text        NOT NULL,
    company_id    text        NOT NULL,
    actor_id      text        NOT NULL CHECK (actor_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    actor_kind    text        NOT NULL CHECK (actor_kind IN ('human', 'ai', 'system')),
    permissions   text[]      NOT NULL CHECK (array_position(permissions, NULL) IS NULL),
    active        boolean     NOT NULL,
    changed_epoch bigint      NOT NULL CHECK (changed_epoch > 0),
    changed_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, actor_id),
    FOREIGN KEY (tenant_id, company_id) REFERENCES brant_auth.authority_scope (tenant_id, company_id)
);

ALTER TABLE brant_auth.authority_scope ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_auth.authority_scope FORCE ROW LEVEL SECURITY;
ALTER TABLE brant_auth.actor_grant ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_auth.actor_grant FORCE ROW LEVEL SECURITY;

CREATE POLICY authority_scope_scope ON brant_auth.authority_scope
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

CREATE POLICY actor_grant_scope ON brant_auth.actor_grant
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
