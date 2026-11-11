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

-- REF: screening list reference data. An import moves a list's current version; versions, entries and
-- name keys are never updated or deleted; the ledger only grows. The list guard function is executed
-- by PTY's screening guard and by PTY's screening commands.
GRANT USAGE ON SCHEMA brant_ref TO brant_app;
GRANT SELECT, INSERT ON brant_ref.screening_list, brant_ref.screening_list_version, brant_ref.screening_list_entry,
    brant_ref.screening_entry_name, brant_ref.action_result, brant_ref.audit_event TO brant_app;
GRANT UPDATE (current_version, row_version, updated_at) ON brant_ref.screening_list TO brant_app;
GRANT EXECUTE ON FUNCTION brant_ref.screening_list_current_v1(text, boolean) TO brant_app;

-- PTY: parties and contacts change through their lifecycle; confirmed revisions and the ledger only grow.
GRANT USAGE ON SCHEMA brant_pty TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_pty.party, brant_pty.contact TO brant_app;
GRANT SELECT, INSERT ON brant_pty.party_revision, brant_pty.contact_revision, brant_pty.action_result, brant_pty.audit_event TO brant_app;
-- PTY screening: the root moves its current conclusion and status and never leaves the negative list
-- (trigger); conclusions, list versions used, hits, decisions and positive and negative list entries
-- only grow. PartyScreeningGuard v1 is executed by downstream contexts in their own transactions.
GRANT SELECT, INSERT ON brant_pty.party_screening, brant_pty.screening_result, brant_pty.screening_list_used,
    brant_pty.screening_hit, brant_pty.screening_decision, brant_pty.positive_list_entry, brant_pty.negative_list_entry TO brant_app;
GRANT UPDATE (row_version, current_screening_id, status, negative, updated_at) ON brant_pty.party_screening TO brant_app;
GRANT EXECUTE ON FUNCTION brant_pty.party_screening_state(text, timestamptz, boolean),
    brant_pty.party_screening_guard_v1(text, timestamptz) TO brant_app;
-- PTY profiles: a draft party's addresses are replaced whole (trigger: only while the party is a draft);
-- confirmed revisions' addresses and verified service roles only grow. A trading profile's root
-- moves its version; a working revision is edited and published, a published one only superseded
-- (trigger); a working revision's contacts are replaced whole (trigger).
GRANT SELECT, INSERT, DELETE ON brant_pty.party_address, brant_pty.trading_profile_contact TO brant_app;
GRANT SELECT, INSERT ON brant_pty.party_revision_address, brant_pty.service_role, brant_pty.trading_profile,
    brant_pty.trading_profile_revision TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_pty.trading_profile TO brant_app;
GRANT UPDATE (status, content_hash, party_revision, payment_terms_id, payment_terms_revision, trade_terms_id, trade_terms_revision,
              owner_actor_id, owner_team_id, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind, published_by_id,
              publish_epoch, published_at, evidence_ids, ended_at)
    ON brant_pty.trading_profile_revision TO brant_app;

-- CAT: SKUs change through their lifecycle; specification revisions and the ledger only grow.
GRANT USAGE ON SCHEMA brant_cat TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_cat.sku TO brant_app;
GRANT SELECT, INSERT ON brant_cat.sku_revision, brant_cat.action_result, brant_cat.audit_event TO brant_app;
-- CAT item code mappings: a root's version and a draft revision change; a published revision changes
-- only its status and supersession (trigger); the key never changes.
GRANT SELECT, INSERT ON brant_cat.item_code_mapping, brant_cat.item_code_mapping_revision TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_cat.item_code_mapping TO brant_app;
GRANT UPDATE (status, sku_id, spec_revision, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind,
              published_by_id, publish_epoch, published_at, evidence_ids, superseded_at)
    ON brant_cat.item_code_mapping_revision TO brant_app;

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
-- ConfigGuard v1: downstream contexts call it in their own transactions with their own executors.
GRANT EXECUTE ON FUNCTION brant_pol.guard_config_in_force_v1(text, text, bigint, timestamptz) TO brant_app;

-- INQ: a draft's lines and sources are replaced whole; the ledger only grows.
GRANT USAGE ON SCHEMA brant_inq TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_inq.customer_rfq TO brant_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON brant_inq.rfq_line, brant_inq.rfq_source TO brant_app;
GRANT SELECT, INSERT ON brant_inq.action_result, brant_inq.audit_event TO brant_app;

-- SUP: a supplier RFQ's draft is replaced whole, an issued one is only closed (trigger). A supply
-- condition's root moves its version; a draft revision is edited, sealed or discarded, a sealed one
-- only superseded, withdrawn or retired (trigger); the ledger only grows.
GRANT USAGE ON SCHEMA brant_sup TO brant_app;
GRANT SELECT, INSERT ON brant_sup.supplier_rfq, brant_sup.condition_root, brant_sup.condition_revision,
    brant_sup.action_result, brant_sup.audit_event TO brant_app;
GRANT UPDATE (status, row_version, data, content_hash, saved_by_kind, saved_by_id, saved_epoch, saved_at, issued_version,
              issued_by_kind, issued_by_id, issue_epoch, issued_at, issue_reason, issue_evidence, closed_by_kind, closed_by_id,
              close_epoch, closed_at, close_reason, close_evidence, updated_at)
    ON brant_sup.supplier_rfq TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_sup.condition_root TO brant_app;
GRANT UPDATE (status, data, content_hash, valid_from, valid_until, validity_mode, source_offer_id, source_offer_revision,
              saved_by_kind, saved_by_id, saved_epoch, saved_at, contributors, verified_by_kind, verified_by_id, verify_epoch,
              verified_at, verification_evidence, superseded_at, ended_by_kind, ended_by_id, end_epoch, ended_at, end_reason,
              end_evidence)
    ON brant_sup.condition_revision TO brant_app;
-- SupplyConditionGuard v1: downstream contexts call it in their own transactions with their own executors.
GRANT EXECUTE ON FUNCTION brant_sup.guard_supply_condition_v1(text, text, bigint, timestamptz) TO brant_app;
