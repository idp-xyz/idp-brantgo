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
-- REF country programmes (S5 E-3): as the screening lists. The list guard function is executed by
-- CNX's trade compliance transactions.
GRANT SELECT, INSERT ON brant_ref.country_programme_list, brant_ref.country_programme_list_version, brant_ref.country_programme TO brant_app;
GRANT UPDATE (current_version, row_version, updated_at) ON brant_ref.country_programme_list TO brant_app;
GRANT EXECUTE ON FUNCTION brant_ref.country_programme_list_current_v1(text, boolean) TO brant_app;

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
-- TradingProfileGuard v1 and AddressRoleGuard v1: downstream contexts call them in their own
-- transactions with their own executors.
GRANT EXECUTE ON FUNCTION brant_pty.guard_trading_profile_current_v1(text, bigint),
    brant_pty.guard_address_role_current_v1(text, bigint, text) TO brant_app;
-- PartyGuard v1 (R22): PUR calls it in its own transactions with its own executor.
GRANT EXECUTE ON FUNCTION brant_pty.guard_party_current_v1(text, bigint) TO brant_app;

-- CAT: SKUs change through their lifecycle; specification revisions and the ledger only grow.
GRANT USAGE ON SCHEMA brant_cat TO brant_app;
GRANT SELECT, INSERT, UPDATE ON brant_cat.sku TO brant_app;
GRANT SELECT, INSERT ON brant_cat.sku_revision, brant_cat.action_result, brant_cat.audit_event TO brant_app;
-- SkuGuard v1: downstream contexts call it in their own transactions with their own executors.
GRANT EXECUTE ON FUNCTION brant_cat.guard_sku_spec_current_v1(text, bigint) TO brant_app;
-- CAT item code mappings: a root's version and a draft revision change; a published revision changes
-- only its status and supersession (trigger); the key never changes.
GRANT SELECT, INSERT ON brant_cat.item_code_mapping, brant_cat.item_code_mapping_revision TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_cat.item_code_mapping TO brant_app;
GRANT UPDATE (status, sku_id, spec_revision, saved_by_kind, saved_by_id, saved_epoch, saved_at, published_by_kind,
              published_by_id, publish_epoch, published_at, evidence_ids, superseded_at)
    ON brant_cat.item_code_mapping_revision TO brant_app;
-- CAT declaration profiles: a root's version and a draft revision change; a verified revision changes
-- only its status and supersession (trigger); the key never changes; nothing is deleted.
GRANT SELECT, INSERT ON brant_cat.declaration_profile, brant_cat.declaration_profile_revision TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_cat.declaration_profile TO brant_app;
GRANT UPDATE (status, content, content_hash, saved_by_kind, saved_by_id, saved_epoch, saved_at, verified_by_kind, verified_by_id,
              verify_epoch, verified_at, evidence_ids, superseded_at)
    ON brant_cat.declaration_profile_revision TO brant_app;

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
-- RfqGuard v1: downstream contexts call it in their own transactions with their own executors.
GRANT EXECUTE ON FUNCTION brant_inq.guard_rfq_ready_v1(text, bigint, text) TO brant_app;

-- CST: roots move their version; revisions are written as the triggers allow (a working revision
-- is edited, sealed and ended revisions only change state), never deleted; the ledger only grows.
GRANT USAGE ON SCHEMA brant_cst TO brant_app;
GRANT SELECT, INSERT ON brant_cst.costing_context, brant_cst.costing_context_revision, brant_cst.measurement,
    brant_cst.measurement_revision, brant_cst.action_result, brant_cst.audit_event TO brant_app;
GRANT UPDATE (root_version, last_revision, updated_at) ON brant_cst.costing_context, brant_cst.measurement TO brant_app;
GRANT UPDATE (status, base_revision, data, content_hash, contributors, provenance, saved_by_kind, saved_by_id, saved_epoch,
              saved_at, verified_by_kind, verified_by_id, verify_epoch, verified_at, verification_evidence, rfq_id,
              rfq_row_version, rfq_lines_hash, rfq_lines, superseded_at, ended_by_kind, ended_by_id, end_epoch, ended_at, end_reason,
              end_evidence)
    ON brant_cst.costing_context_revision TO brant_app;
GRANT UPDATE (status, superseded_at, withdrawn_by_id, withdraw_epoch, withdrawn_at, withdraw_reason, withdraw_evidence)
    ON brant_cst.measurement_revision TO brant_app;
-- Cost scenarios: a draft is replaced whole, an evaluation counts itself on the root, a freeze moves
-- the root to frozen (trigger); evaluations are only appended (trigger).
GRANT SELECT, INSERT ON brant_cst.cost_scenario, brant_cst.cost_evaluation TO brant_app;
GRANT UPDATE (root_version, status, data, content_hash, contributors, saved_by_kind, saved_by_id, saved_epoch, saved_at,
              last_evaluation, updated_at)
    ON brant_cst.cost_scenario TO brant_app;
-- Cost snapshots and their components and allocations are only appended (triggers). The single
-- column grant on cost_snapshot exists so a freeze can lock the snapshot it supersedes FOR UPDATE;
-- the trigger refuses every update. A source's standing is created undisputed and denied once; an
-- attestation is only appended (triggers).
GRANT SELECT, INSERT ON brant_cst.cost_snapshot, brant_cst.cost_snapshot_component, brant_cst.cost_snapshot_allocation,
    brant_cst.source_standing, brant_cst.source_attestation TO brant_app;
GRANT UPDATE (snapshot_id) ON brant_cst.cost_snapshot TO brant_app;
GRANT UPDATE (root_version, status, denial_id, denied_at, updated_at) ON brant_cst.source_standing TO brant_app;
-- AdoptabilityGuard v1: downstream contexts call it in their own transactions with their own executors.
GRANT EXECUTE ON FUNCTION brant_cst.guard_cost_snapshot_adoptable_v1(text, text, timestamptz) TO brant_app;

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

-- TRD: a quote root moves its version and its object control (trigger: identity and owner fixed,
-- versions and control epochs only grow); a working revision is edited, frozen or discarded, a sealed
-- one only changes its status along SM-TRD-REVISION, a discarded one never changes (trigger); a review
-- is decided once, an approval revoked once, a margin exception never changes (triggers); formal
-- documents, commit evidence, closures, risk holds and conversion slots are only inserted (triggers);
-- nothing is deleted; the ledger only grows. The freeze, activation and replacement call CST's, INQ's,
-- CAT's, PTY's and POL's guard functions, granted above; TRD publishes its quote revision guard.
GRANT USAGE ON SCHEMA brant_trd TO brant_app;
GRANT SELECT, INSERT ON brant_trd.quote_root, brant_trd.quote_revision, brant_trd.action_result, brant_trd.audit_event TO brant_app;
GRANT UPDATE (root_version, last_revision, labels, internal_note, control_state, control_epoch, control_task_id,
              control_authority_epoch, control_by_kind, control_by_id, control_changed_at, updated_at)
    ON brant_trd.quote_root TO brant_app;
-- TRD 000009 (S4 A2): a bind or handoff records the binding's delegation (decision D-5).
GRANT UPDATE (control_initiator_id, control_initiator_epoch, control_agent_id, control_agent_epoch) ON brant_trd.quote_root TO brant_app;
GRANT UPDATE (status, data, content_hash, contributors, saved_by_kind, saved_by_id, saved_epoch, saved_at, business_hash, commercial,
              issued_at, expires_at, frozen_by_kind, frozen_by_id, freeze_epoch, frozen_at, frozen_root_version, ended_by_kind,
              ended_by_id, end_epoch, ended_at, end_reason, end_evidence)
    ON brant_trd.quote_revision TO brant_app;
GRANT SELECT, INSERT ON brant_trd.quote_review, brant_trd.quote_approval, brant_trd.quote_margin_exception TO brant_app;
GRANT UPDATE (status, decided_by_kind, decided_by_id, decided_epoch, decided_at, decision_reason, decision_evidence)
    ON brant_trd.quote_review TO brant_app;
GRANT UPDATE (effectivity, effectivity_version, revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, revoke_evidence)
    ON brant_trd.quote_approval TO brant_app;
GRANT SELECT, INSERT ON brant_trd.quote_document, brant_trd.quote_commit_evidence, brant_trd.quote_revision_closure,
    brant_trd.quote_risk_hold, brant_trd.quote_conversion_slot TO brant_app;
GRANT EXECUTE ON FUNCTION brant_trd.guard_quote_revision_active_v1(text, bigint, text) TO brant_app;
-- Customer responses and the effectivity ledger are only inserted (triggers); an acceptance changes
-- its effectivity once (trigger), and acceptance.invalidate and acceptance.mark_disputed lock it FOR
-- UPDATE, which needs the column grant. TRD publishes its acceptance guard.
GRANT SELECT, INSERT ON brant_trd.customer_response, brant_trd.acceptance, brant_trd.acceptance_effectivity TO brant_app;
GRANT UPDATE (effectivity, effectivity_version) ON brant_trd.acceptance TO brant_app;
GRANT EXECUTE ON FUNCTION brant_trd.guard_acceptance_valid_v1(text, text, bigint, text) TO brant_app;
-- Sales orders: an order is inserted with its snapshot and then changes only its status, row version
-- and activating review (trigger); a review is decided once and revoked once (trigger); commit
-- evidence and closures are only inserted. Every order command locks the order FOR UPDATE, which
-- needs the column grants. order.activate calls PTY's PartyScreeningGuard, granted above; TRD
-- publishes its order guard (ActiveOrderRef v1, R19).
GRANT SELECT, INSERT ON brant_trd.sales_order, brant_trd.sales_order_review, brant_trd.sales_order_commit_evidence,
    brant_trd.sales_order_closure TO brant_app;
GRANT UPDATE (status, row_version, activated_review_id, updated_at) ON brant_trd.sales_order TO brant_app;
GRANT UPDATE (status, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason, decision_evidence, expires_at,
              acknowledged_risks, revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, revoke_evidence)
    ON brant_trd.sales_order_review TO brant_app;
GRANT EXECUTE ON FUNCTION brant_trd.guard_order_active_v1(text, bigint, text) TO brant_app;
-- TRD publishes the lock-free order-state read STL's credit.release calls inside its transaction (S5 B4).
GRANT EXECUTE ON FUNCTION brant_trd.order_state_v1(text) TO brant_app;
-- Active orders' amendments, revisions and ends (S3-15): an order moves to its next revision only
-- with the revision row and the amendment's commit evidence (trigger), which, like the end of an
-- active order, are only inserted; an amendment changes along SM-TRD-ORDER-AMENDMENT (trigger).
GRANT SELECT, INSERT ON brant_trd.sales_order_amendment, brant_trd.sales_order_revision, brant_trd.sales_order_amendment_commit,
    brant_trd.sales_order_end TO brant_app;
GRANT UPDATE (order_revision, order_hash) ON brant_trd.sales_order TO brant_app;
GRANT UPDATE (base_revision, base_hash, content, amendment_hash, status, version, preparations, submitted_by_kind, submitted_by_id,
              submit_epoch, submitted_at, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, decision_reason,
              decision_evidence, applied_by_kind, applied_by_id, apply_epoch, applied_at, applied_revision, withdrawn_by_kind,
              withdrawn_by_id, withdraw_epoch, withdrawn_at, withdraw_reason, withdraw_evidence, updated_at)
    ON brant_trd.sales_order_amendment TO brant_app;
-- Order documents and their signed copies (S3-15, TRD-INV-020): only inserted, for the order's
-- current revision (trigger).
GRANT SELECT, INSERT ON brant_trd.sales_order_document, brant_trd.sales_order_document_signature TO brant_app;
-- TRD 000010 (S4 B3): the risk records a denied cost source leaves on active orders are only inserted
-- (trigger); its holds go to quote_risk_hold, granted above (WF-03, decision D-9).
GRANT SELECT, INSERT ON brant_trd.sales_order_risk TO brant_app;

-- PUR: a purchase draft is inserted prepared with its lines and only voided, once (trigger); its
-- lines never change; a DemandAllocation root moves its balance, order revision and version by one
-- per command (trigger), and its entries are taken once and released once (trigger); nothing is
-- deleted; the ledger only grows. FOR UPDATE on drafts and roots needs the column grants. The
-- purchase draft commands call SUP's, CST's, POL's, PTY's, CAT's and TRD's guard functions, granted
-- above.
GRANT USAGE ON SCHEMA brant_pur TO brant_app;
GRANT SELECT, INSERT ON brant_pur.purchase_draft, brant_pur.purchase_draft_line, brant_pur.demand_allocation,
    brant_pur.allocation_entry, brant_pur.action_result, brant_pur.audit_event TO brant_app;
GRANT UPDATE (status, row_version, voided_by_kind, voided_by_id, void_epoch, voided_at, void_reason)
    ON brant_pur.purchase_draft TO brant_app;
GRANT UPDATE (order_revision, demand, allocated, row_version, updated_at) ON brant_pur.demand_allocation TO brant_app;
GRANT UPDATE (released_version, released_at) ON brant_pur.allocation_entry TO brant_app;
-- PUR purchase commitments (S5-01): a commitment is inserted requested and then changes only along
-- SM-PUR-COMMITMENT, each step writing its own columns (trigger); supplier confirmations are only
-- inserted; FOR UPDATE on a commitment needs the column grants. Decisions and issues call the guard
-- functions granted above, and the issue POL's number.issue before the transaction. TRD executes
-- DownstreamCommitments v1 (R71) inside its own transactions.
GRANT SELECT, INSERT ON brant_pur.purchase_commitment, brant_pur.supplier_confirmation TO brant_app;
GRANT UPDATE (status, row_version, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, valid_until, decision_reason,
              acknowledgements, revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, issued_by_kind, issued_by_id,
              issue_epoch, issued_at, po_number, sequence_id, sequence_revision, sent_at, issue_evidence, issue_file_id, issue_file_sha256,
              updated_at)
    ON brant_pur.purchase_commitment TO brant_app;
GRANT EXECUTE ON FUNCTION brant_pur.order_commitments_v1(text) TO brant_app;
-- PUR supplier deliveries (S5-15): a delivery root is inserted with its first version and then only
-- moves its version counters (trigger); a version is inserted recorded and changes only by its
-- decision and its supersession, once each (trigger); a line is written with its version and only
-- takes its verified quantity, once (trigger); nothing is deleted. FOR UPDATE on a delivery root
-- needs the column grants. FUL's transactions execute SupplierDeliveryGuard v1 (R59).
GRANT SELECT, INSERT ON brant_pur.supplier_delivery, brant_pur.supplier_delivery_version, brant_pur.supplier_delivery_line TO brant_app;
GRANT UPDATE (row_version, latest_version, verified_version, updated_at) ON brant_pur.supplier_delivery TO brant_app;
GRANT UPDATE (status, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, decision_reason, decision_evidence,
              superseded_by_version, superseded_at)
    ON brant_pur.supplier_delivery_version TO brant_app;
GRANT UPDATE (verified_quantity) ON brant_pur.supplier_delivery_line TO brant_app;
GRANT EXECUTE ON FUNCTION brant_pur.guard_supplier_delivery_v1(text, text, bigint) TO brant_app;
-- PUR commitment changes and cancellation (S5-19): a change is inserted as a draft with its lines and
-- then changes only by its decision and its application, once each (trigger); a revision is inserted
-- with its lines and never changes; a commitment moves to its next revision or is cancelled (trigger);
-- nothing is deleted. The triggers' deferred checks call PUR's helper functions as the application role.
GRANT SELECT, INSERT ON brant_pur.purchase_commitment_change, brant_pur.purchase_commitment_change_line,
    brant_pur.purchase_commitment_revision, brant_pur.purchase_commitment_revision_line TO brant_app;
GRANT UPDATE (status, row_version, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision, decision_reason, acknowledgements,
              applied_by_kind, applied_by_id, apply_epoch, applied_at, applied_revision, updated_at)
    ON brant_pur.purchase_commitment_change TO brant_app;
GRANT UPDATE (revision, cancelled_by_kind, cancelled_by_id, cancel_epoch, cancelled_at, cancel_reason, cancel_evidence)
    ON brant_pur.purchase_commitment TO brant_app;
GRANT EXECUTE ON FUNCTION brant_pur.commitment_lines(text, text, text), brant_pur.commitment_line_verified(text, text, text, text),
    brant_pur.draft_claims_whole(text, text, text) TO brant_app;
-- PUR allocation reviews (S5-09b): a review is inserted open with its items and changes once, settled
-- or superseded (trigger); an item changes only by its disposition, once (trigger); nothing is
-- deleted. FOR UPDATE on reviews needs the column grants.
GRANT SELECT, INSERT ON brant_pur.allocation_review, brant_pur.allocation_review_item TO brant_app;
GRANT UPDATE (status, row_version, settled_by_kind, settled_by_id, settle_epoch, settled_at, settle_reason, superseded_by, superseded_at)
    ON brant_pur.allocation_review TO brant_app;
GRANT UPDATE (disposition, disposition_draft_id, disposition_change_id, disposition_commitment_id, disposition_revision, disposition_at,
              disposition_reason)
    ON brant_pur.allocation_review_item TO brant_app;

-- QLT: an inspection is inserted recorded and changes only by its disposition and its supersession,
-- once each (trigger); disposition items are written with the disposition and never change; a
-- release is inserted granted on the quantities its subject's rows give (trigger) and only revoked,
-- once, with what FUL shipped on it (S5R-D18); nothing is deleted; the ledger only grows. FOR UPDATE
-- on inspections and releases needs the column grants. FUL's transactions execute
-- QualityReleaseGuard v1; QLT's revocation executes FUL's ReleaseConsumption v1, granted below.
GRANT USAGE ON SCHEMA brant_qlt TO brant_app;
GRANT SELECT, INSERT ON brant_qlt.inspection, brant_qlt.disposition_item, brant_qlt.quality_release, brant_qlt.action_result,
    brant_qlt.audit_event TO brant_app;
GRANT UPDATE (status, row_version, disposed_by_kind, disposed_by_id, dispose_epoch, disposed_at, disposition_reason,
    disposition_evidence_ids, superseded_by_inspection_id, superseded_at) ON brant_qlt.inspection TO brant_app;
GRANT UPDATE (status, row_version, revoked_by_kind, revoked_by_id, revoke_epoch, revoked_at, revoke_reason, revoke_evidence_ids,
    shipped_quantity, frozen_quantity) ON brant_qlt.quality_release TO brant_app;
GRANT EXECUTE ON FUNCTION brant_qlt.guard_quality_release_v1(text, timestamptz) TO brant_app;

-- STL customer credit (S5-12a): a credit root moves its version and its count of limit versions by
-- one per command (trigger); a limit request and a decision change only their state, version and
-- decision until decided, once (triggers); limit versions and approvals are only inserted; a
-- reservation moves reserved → consumed → released or reserved → released (trigger); nothing is
-- deleted; the ledger only grows. FOR UPDATE on the root needs the column grant. The credit commands
-- call POL's ConfigGuard, granted above; STL publishes CreditGuard v1 (R57) for TRD.
GRANT USAGE ON SCHEMA brant_stl TO brant_app;
GRANT SELECT, INSERT ON brant_stl.customer_credit, brant_stl.credit_limit_request, brant_stl.credit_limit_version, brant_stl.credit_decision,
    brant_stl.credit_approval, brant_stl.credit_reservation, brant_stl.action_result, brant_stl.audit_event TO brant_app;
GRANT UPDATE (row_version, limit_version, updated_at) ON brant_stl.customer_credit TO brant_app;
GRANT UPDATE (status, version, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason, limit_version)
    ON brant_stl.credit_limit_request TO brant_app;
GRANT UPDATE (status, version, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason) ON brant_stl.credit_decision TO brant_app;
GRANT UPDATE (status, version, consumed_by_kind, consumed_by_id, consume_epoch, consumed_at, activated_at, released_by_kind, released_by_id,
              release_epoch, released_at, release_reason) ON brant_stl.credit_reservation TO brant_app;
GRANT EXECUTE ON FUNCTION brant_stl.guard_credit_reservation_v1(text, bigint, numeric, text, timestamptz) TO brant_app;

-- STL funds and applications (S5-04a, S5-07): a fund record moves only a receipt's state and version
-- by one per resolution (trigger); resolutions, order accounts, their stages and unapplies are only
-- inserted; an application moves once, applied → unapplied (trigger); nothing is deleted. FOR UPDATE
-- on fund records and order accounts needs the column grants (an order account itself does not
-- change: its trigger refuses). fund.record and order_account.open call POL's ConfigGuard, granted
-- above; STL publishes PaymentConditionGuard v1 (R45) for FUL.
GRANT SELECT, INSERT ON brant_stl.fund_record, brant_stl.fund_difference_resolution, brant_stl.order_account, brant_stl.order_account_stage,
    brant_stl.application, brant_stl.application_unapply TO brant_app;
GRANT UPDATE (status, version, updated_at) ON brant_stl.fund_record TO brant_app;
GRANT UPDATE (updated_at) ON brant_stl.order_account TO brant_app;
GRANT UPDATE (status, version, updated_at) ON brant_stl.application TO brant_app;
GRANT EXECUTE ON FUNCTION brant_stl.guard_payment_condition_v1(text, bigint, text, timestamptz) TO brant_app;

-- STL payment plan review (S5-09b): the order account moves to a later order revision, or by one
-- version when a review is settled (trigger); a review moves once, open → settled or superseded
-- (trigger); items and dispositions are only inserted; nothing is deleted.
GRANT UPDATE (order_revision, order_hash, currency_revision, version) ON brant_stl.order_account TO brant_app;
GRANT SELECT, INSERT ON brant_stl.payment_plan_review, brant_stl.payment_plan_review_item, brant_stl.payment_plan_review_disposition TO brant_app;
GRANT UPDATE (status, version, settled_by_kind, settled_by_id, settle_epoch, settled_at, settle_reason, superseded_by_revision, superseded_at,
              updated_at) ON brant_stl.payment_plan_review TO brant_app;

-- STL settlement statements (S5-04b): statements, their lines and installments are only inserted
-- (triggers; settlement.reverse is unit C8's); an application may also move applied → moved when a
-- sales statement takes over a prepayment (column grant above). FOR UPDATE on a statement needs the
-- column grant (the statement itself does not change: its trigger refuses).
GRANT SELECT, INSERT ON brant_stl.settlement_statement, brant_stl.settlement_line, brant_stl.settlement_installment TO brant_app;
GRANT UPDATE (updated_at) ON brant_stl.settlement_statement TO brant_app;

-- STL credit notes and settlement reversal (S5-18): a statement root moves by one version per credit note
-- requested or decided and once more when reversed (trigger: confirmed → reversed, terminal); its lines move
-- settled → reversed with it; a credit note is decided once (trigger); reversals are only inserted; nothing is
-- deleted. FOR UPDATE on statements needs the column grant above.
GRANT UPDATE (status, version) ON brant_stl.settlement_statement TO brant_app;
GRANT UPDATE (status) ON brant_stl.settlement_line TO brant_app;
GRANT SELECT, INSERT ON brant_stl.credit_note, brant_stl.settlement_reversal TO brant_app;
GRANT UPDATE (status, version, decided_by_kind, decided_by_id, decide_epoch, decided_at, decision_reason, updated_at)
    ON brant_stl.credit_note TO brant_app;

-- STL charges and order results (S5-17): a charge moves estimated → pending → confirmed and names its newest
-- allocation (trigger: what is fixed when recorded never changes); beneficiaries, allocations and their lines are
-- only inserted; an order result moves along SM-STL-ORDER-RESULT (trigger) and its reviews, closing snapshots and
-- reopenings are only inserted (a snapshot never changes, MD-49); nothing is deleted. FOR UPDATE on charges and
-- FOR UPDATE / FOR SHARE on order results need the column grants.
GRANT SELECT, INSERT ON brant_stl.charge, brant_stl.charge_beneficiary, brant_stl.charge_allocation, brant_stl.charge_allocation_line,
    brant_stl.order_result, brant_stl.order_result_review, brant_stl.order_result_snapshot, brant_stl.order_result_reopening TO brant_app;
GRANT UPDATE (status, version, source_kind, source_party_id, source_reference, estimate_reference, amount_minor, occurred_on, analysis_on,
    basis, evidence_ids, billed_by_kind, billed_by_id, bill_epoch, billed_at, confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at,
    confirm_evidence_ids, confirm_reason, allocation_seq, updated_at) ON brant_stl.charge TO brant_app;
GRANT UPDATE (status, version, updated_at) ON brant_stl.order_result TO brant_app;

-- STL opening balances (S5-20): an opening item is only inserted with the opening statement or fund record its command
-- writes, and moves recorded → reversed with its reversed statement (trigger); nothing is deleted.
GRANT SELECT, INSERT ON brant_stl.opening_balance TO brant_app;
GRANT UPDATE (status, updated_at) ON brant_stl.opening_balance TO brant_app;

-- CNX: a trade compliance check moves its state, current evaluation and decision and its version
-- along SM-CNX-COMPLIANCE (trigger: binding, owner and opening fixed); evaluations, decisions and
-- reopenings are only inserted (triggers); nothing is deleted; the ledger only grows. FOR UPDATE on
-- checks needs the column grant. The evaluation and decision call POL's guard function and
-- reference data's programme list guard, granted above; CNX's reads derive a check's state with its
-- state function, and CNX publishes TradeComplianceGuard v1 for TRD and FUL.
GRANT USAGE ON SCHEMA brant_cnx TO brant_app;
GRANT SELECT, INSERT ON brant_cnx.trade_compliance_check, brant_cnx.trade_compliance_evaluation, brant_cnx.trade_compliance_list_used,
    brant_cnx.trade_compliance_decision, brant_cnx.trade_compliance_reopening, brant_cnx.action_result, brant_cnx.audit_event TO brant_app;
GRANT UPDATE (status, row_version, current_evaluation_id, current_decision_id, updated_at) ON brant_cnx.trade_compliance_check TO brant_app;
GRANT EXECUTE ON FUNCTION brant_cnx.trade_compliance_state(text, bigint, text, timestamptz, boolean) TO brant_app;
GRANT EXECUTE ON FUNCTION brant_cnx.guard_trade_compliance_v1(text, bigint, text, timestamptz) TO brant_app;

-- CNX export licences: a licence's terms never change, only its version moves with each allocation
-- and release (trigger); allocations and releases are only inserted (triggers, which lock the
-- licence FOR UPDATE: the column grant); CNX's reads derive the usage and a shipment's verdict with
-- its functions, and CNX publishes ExportLicenseGuard v1 for FUL.
GRANT SELECT, INSERT ON brant_cnx.export_license, brant_cnx.export_license_allocation, brant_cnx.export_license_release TO brant_app;
GRANT UPDATE (row_version, updated_at) ON brant_cnx.export_license TO brant_app;
GRANT EXECUTE ON FUNCTION brant_cnx.export_license_outstanding(text, text, text) TO brant_app;
GRANT EXECUTE ON FUNCTION brant_cnx.export_license_state(text, timestamptz, boolean) TO brant_app;
GRANT EXECUTE ON FUNCTION brant_cnx.guard_export_license_v1(text, timestamptz) TO brant_app;

-- CNX purchase invoices and tax refund cases: an invoice keeps its import and is decided and
-- reversed once, a case keeps its binding and follows SM-CNX-TAXCASE (triggers; the column grants
-- are also what the FOR UPDATE locks of the commands and the allocation trigger need); lines,
-- matches, allocations and results are only inserted. The reads derive whether an allocation
-- counts with its function.
GRANT SELECT, INSERT ON brant_cnx.purchase_invoice, brant_cnx.purchase_invoice_line, brant_cnx.purchase_invoice_match,
    brant_cnx.tax_refund_case, brant_cnx.tax_evidence_allocation, brant_cnx.tax_refund_result TO brant_app;
GRANT UPDATE (status, row_version, invoice_kind, invoice_number, colour, verified, decision, decision_reason, decided_by_kind, decided_by_id,
    decide_epoch, decided_at, red_of_invoice_id, reversed_by_invoice_id, updated_at) ON brant_cnx.purchase_invoice TO brant_app;
GRANT UPDATE (status, row_version, updated_at) ON brant_cnx.tax_refund_case TO brant_app;
GRANT EXECUTE ON FUNCTION brant_cnx.tax_evidence_counts(text, text, text, text) TO brant_app;

-- CNX declarations, document bundles and external reconciliation (S5-05a): a declaration case keeps
-- its binding and follows SM-CNX-DECLARATION, a bundle keeps its binding and moves with each check
-- and reconciliation (triggers; the column grants are also what the commands' FOR UPDATE locks and
-- the entry triggers' locks need); versions, confirmations, submissions, receipts, checks and
-- reconciliations are only inserted; nothing is deleted.
GRANT SELECT, INSERT ON brant_cnx.declaration_case, brant_cnx.declaration_version, brant_cnx.declaration_confirmation,
    brant_cnx.declaration_submission, brant_cnx.declaration_receipt, brant_cnx.document_bundle, brant_cnx.document_bundle_check,
    brant_cnx.external_reconciliation TO brant_app;
GRANT UPDATE (status, current_version, mapping, declaration_number, exported_at, last_sequence, submissions, receipts, row_version,
    updated_at) ON brant_cnx.declaration_case TO brant_app;
GRANT UPDATE (status, checks, reconciliations, row_version, updated_at) ON brant_cnx.document_bundle TO brant_app;

-- CNX certificate results (S5-10 CNX half): a certificate keeps its identity and kind and moves by one
-- revision per registration (trigger; the column grant is also what a correction's FOR UPDATE lock and
-- the revision trigger's lock need); revisions are only inserted; nothing is deleted.
GRANT SELECT, INSERT ON brant_cnx.certificate_result, brant_cnx.certificate_revision TO brant_app;
GRANT UPDATE (issuer, number, current_revision, row_version, updated_at) ON brant_cnx.certificate_result TO brant_app;

-- CNX opening refund material (S5-20): opening items and the opening consumption of invoice lines are only inserted
-- (triggers; the consumption trigger locks the invoice FOR UPDATE through the invoice's column grant above); a
-- reconciliation of a credit note keeps what STL stated of it in its own columns.
GRANT SELECT, INSERT ON brant_cnx.tax_opening, brant_cnx.tax_evidence_opening TO brant_app;

-- FUL shipments (S5-03a): an order's fulfilment root is only inserted; the single column grant
-- exists so shipment.prepare and shipment.confirm can lock it FOR UPDATE, and the trigger refuses
-- every update. A shipment is inserted prepared with its lines and parties and then changes only by
-- a packing record, its confirmation or its cancellation, once (trigger); lines, parties, packing
-- records, packages and their contents are only inserted; nothing is deleted; the ledger only
-- grows. The shipment commands call PTY's, QLT's and TRD's guard functions, granted above; TRD
-- executes ShipmentFacts v1 (R70) inside its own transactions.
GRANT USAGE ON SCHEMA brant_ful TO brant_app;
GRANT SELECT, INSERT ON brant_ful.order_fulfilment, brant_ful.shipment, brant_ful.shipment_line, brant_ful.shipment_party,
    brant_ful.shipment_packing, brant_ful.shipment_package, brant_ful.shipment_package_content, brant_ful.action_result,
    brant_ful.audit_event TO brant_app;
GRANT UPDATE (order_id) ON brant_ful.order_fulfilment TO brant_app;
GRANT UPDATE (status, row_version, packing_records, confirmed_by_kind, confirmed_by_id, confirm_epoch, confirmed_at, confirm_evidence_ids,
              guard_entries, guard_entries_sha256, unchecked_guards, cancelled_by_kind, cancelled_by_id, cancel_epoch, cancelled_at,
              cancel_reason) ON brant_ful.shipment TO brant_app;
GRANT EXECUTE ON FUNCTION brant_ful.order_shipped_v1(text) TO brant_app;
-- QLT's quality_release.revoke executes ReleaseConsumption v1 (R79, S5R-D18) inside its own transactions.
GRANT EXECUTE ON FUNCTION brant_ful.release_consumption_v1(text) TO brant_app;

-- FUL milestones and export lots (S5-03b): a shipment line's price is only inserted with its
-- shipment; a milestone plan moves only its version, revision, counts and last revision (trigger),
-- the column grant also lets milestone commands lock it FOR UPDATE; nodes and actuals are only
-- inserted; export lots and their lines are only inserted and never change; nothing is deleted.
GRANT SELECT, INSERT ON brant_ful.shipment_line_price, brant_ful.milestone_plan, brant_ful.milestone_node, brant_ful.milestone_actual,
    brant_ful.export_lot, brant_ful.export_lot_line TO brant_app;
GRANT UPDATE (row_version, plan_revision, node_count, actual_count, revised_by_kind, revised_by_id, revise_epoch, revised_at,
              revision_reason) ON brant_ful.milestone_plan TO brant_app;

-- FUL commercial release (S5-07): a release moves only by an evaluation, an exception or its
-- consumption, once (trigger); evaluations and exceptions are only inserted; nothing is deleted.
-- FOR UPDATE on releases needs the column grants. The release commands and shipment.confirm call
-- CNX's, PTY's, PUR's, QLT's, STL's and TRD's guard functions, granted above.
GRANT SELECT, INSERT ON brant_ful.commercial_release, brant_ful.commercial_release_evaluation, brant_ful.commercial_release_exception
    TO brant_app;
GRANT UPDATE (status, row_version, evaluations, exceptions, consumed_command, consumed_by_kind, consumed_by_id, consume_epoch, consumed_at,
              consume_evidence_ids, consume_entries, consume_entries_sha256, document_release_kind) ON brant_ful.commercial_release TO brant_app;

-- FUL bookings and shipment documents (S5-08): booking versions are only inserted; a document set
-- moves only its version and counts (trigger; the column grant also lets the document commands lock
-- it FOR UPDATE); a document version moves only draft -> issued or superseded and issued ->
-- superseded (trigger); renditions, issues and signed copies are only inserted; nothing is deleted.
-- The document commands call POL's ConfigGuard v1, granted above. document_release.record writes the
-- issued B/L record it was recorded on with the release's consumption.
GRANT SELECT, INSERT ON brant_ful.shipment_booking, brant_ful.shipment_document_set, brant_ful.shipment_document,
    brant_ful.shipment_document_rendition, brant_ful.shipment_document_issue, brant_ful.shipment_document_signature TO brant_app;
GRANT UPDATE (row_version, document_count, issue_count) ON brant_ful.shipment_document_set TO brant_app;
GRANT UPDATE (status, issue_seq, issued_at, superseded_at) ON brant_ful.shipment_document TO brant_app;
GRANT UPDATE (consumed_document_set_id, consumed_bl_version, consumed_bl_number, consumed_bl_sha256) ON brant_ful.commercial_release TO brant_app;

-- FUL shipment reviews (S5-09b): a review is inserted open with its items and changes once, settled or
-- superseded (trigger); an item changes only by its disposition, once (trigger); nothing is deleted.
-- FOR UPDATE on reviews needs the column grants.
GRANT SELECT, INSERT ON brant_ful.shipment_review, brant_ful.shipment_review_item TO brant_app;
GRANT UPDATE (status, row_version, settled_by_kind, settled_by_id, settle_epoch, settled_at, settle_reason, superseded_by, superseded_at)
    ON brant_ful.shipment_review TO brant_app;
GRANT UPDATE (disposition, disposition_shipment_id, disposition_revision, disposition_ref, disposition_status, disposition_at,
              disposition_reason)
    ON brant_ful.shipment_review_item TO brant_app;

-- TSK business tasks (S4-01): a task is inserted created and moves only along SM-TSK-TASK, one version at
-- a time, never changing what it was created with (trigger); its history only grows (trigger); nothing
-- is deleted; the ledger only grows. FOR UPDATE on tasks needs the column grants.
GRANT USAGE ON SCHEMA brant_tsk TO brant_app;
GRANT SELECT, INSERT ON brant_tsk.business_task, brant_tsk.task_transition, brant_tsk.action_result, brant_tsk.audit_event TO brant_app;
GRANT UPDATE (lifecycle, control_mode, control_epoch, task_version, target_quote_id, binding_quote_id, binding_state, binding_epoch,
              bound_at, released_at, wait_kind, wait_reason, wait_responsible_id, wait_items, awaiting_kind, awaiting_id, awaiting_version,
              wait_due_at, wait_since, pause_cause, pause_reason, pause_responsible_id, resume_condition, paused_at, paused_from,
              stop_purpose, stop_from, stop_reason, stop_by_kind, stop_by_id, stop_at, scope_settled_at, completion_actor, end_cause,
              end_reason, end_responsible_id, ended_by_kind, ended_by_id, ended_at, updated_at)
    ON brant_tsk.business_task TO brant_app;

-- internal/ai (brant_ai; plan S4 D-6): a delegation is revoked and an approval grant used or revoked,
-- once each; their binding never changes and neither is deleted (triggers); the ledger only grows.
GRANT USAGE ON SCHEMA brant_ai TO brant_app;
GRANT SELECT, INSERT ON brant_ai.delegation, brant_ai.approval_grant, brant_ai.action_result, brant_ai.audit_event TO brant_app;
GRANT UPDATE (status, row_version, revoked_by_id, revoked_epoch, revoked_at, revoke_reason) ON brant_ai.delegation TO brant_app;
GRANT UPDATE (status, row_version, used_key, used_by_id, used_epoch, used_at, revoked_by_id, revoked_epoch, revoked_at, revoke_reason)
    ON brant_ai.approval_grant TO brant_app;
