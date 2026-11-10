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
-- Roles are redefined in place; role assignments are ended, never deleted.
GRANT SELECT, INSERT, UPDATE ON brant_auth.role TO brant_app;
GRANT SELECT, INSERT ON brant_auth.actor_role TO brant_app;
GRANT UPDATE (removed_epoch, removed_at) ON brant_auth.actor_role TO brant_app;

-- foundation/messaging: dead letters are replayed, never deleted, and their envelope never changes.
GRANT USAGE ON SCHEMA brant_msg TO brant_app;
GRANT SELECT, INSERT ON brant_msg.dead_letter TO brant_app;
GRANT UPDATE (state, replay_attempts, last_replay_at, last_replay_error, replayed_at, replayed_by_kind,
              replayed_by_id, replay_epoch) ON brant_msg.dead_letter TO brant_app;

-- EVD: sources are immutable; the ledger tables only grow.
GRANT USAGE ON SCHEMA brant_evd TO brant_app;
GRANT SELECT, INSERT ON brant_evd.source_content, brant_evd.source_origin, brant_evd.source_import,
    brant_evd.business_file, brant_evd.action_result, brant_evd.audit_event TO brant_app;

-- PTY: parties and contacts change through their lifecycle; confirmed revisions and the ledger only grow.
GRANT USAGE ON SCHEMA brant_pty TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_pty.party, brant_pty.contact TO brant_app;
GRANT SELECT, INSERT ON brant_pty.party_revision, brant_pty.contact_revision, brant_pty.action_result, brant_pty.audit_event TO brant_app;

-- CAT: SKUs change through their lifecycle; specification revisions and the ledger only grow.
GRANT USAGE ON SCHEMA brant_cat TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_cat.sku TO brant_app;
GRANT SELECT, INSERT ON brant_cat.sku_revision, brant_cat.action_result, brant_cat.audit_event TO brant_app;

-- POL: a root's version and a working revision change; a published revision changes only its status and
-- end (trigger); counters count up; issued numbers, snapshots and the ledger only grow.
GRANT USAGE ON SCHEMA brant_pol TO brant_app;
GRANT SELECT, INSERT ON brant_pol.config_root, brant_pol.config_revision, brant_pol.number_counter TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_pol.config_root TO brant_app;
GRANT UPDATE (status, data, content_hash, valid_from, valid_until, saved_by_kind, saved_by_id, saved_epoch, saved_at,
              published_by_kind, published_by_id, publish_epoch, published_at, evidence_ids, ended_at)
    ON brant_pol.config_revision TO brant_app;
GRANT UPDATE (last_value, updated_at) ON brant_pol.number_counter TO brant_app;
GRANT SELECT, INSERT ON brant_pol.issued_number, brant_pol.foundation_snapshot, brant_pol.snapshot_entry,
    brant_pol.action_result, brant_pol.audit_event TO brant_app;

-- INQ: a draft's lines and sources are replaced whole; the ledger only grows.
GRANT USAGE ON SCHEMA brant_inq TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_inq.customer_rfq TO brant_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON brant_inq.rfq_line, brant_inq.rfq_source TO brant_app;
GRANT SELECT, INSERT ON brant_inq.action_result, brant_inq.audit_event TO brant_app;
