-- What brant_app may do in the schemas brant_migrator owns. Run as brant_migrator after every
-- brant-migrate run. Each bounded context adds the least privilege its adapters need when its
-- migrations land; the migration ledger brant_migrations stays out of reach.

-- IDP-Bento-Go Outbox and Inbox.
GRANT USAGE ON SCHEMA bento TO brant_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA bento TO brant_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA bento TO brant_app;

-- foundation/authorization: grants are deactivated, never deleted.
GRANT USAGE ON SCHEMA brant_auth TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_auth.authority_scope, brant_auth.actor_grant TO brant_app;

-- EVD: sources are immutable; the ledger tables only grow.
GRANT USAGE ON SCHEMA brant_evd TO brant_app;
GRANT SELECT, INSERT ON brant_evd.source_content, brant_evd.source_origin, brant_evd.source_import,
    brant_evd.action_result, brant_evd.audit_event TO brant_app;
