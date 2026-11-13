-- TRD 000009: object control under TSK — the manual state, the hand-back and the binding's
-- delegation (WBS S4-01b; SM-TRD-CONTROL as A0 committed it, TRD-INV-015, TSK-INV-004; S4 decisions
-- D-3, D-4 and D-5).
--
-- SM-TRD-CONTROL gains manual: a takeover stops the root (bound → stopping) and, once TSK's barrier
-- completed, puts it under manual control (stopping → manual, object_control.enter_manual), where only
-- humans write with the bound task's claim; a hand-back stops a manual root (manual → stopping) and
-- binds it to the same task's AI again (stopping → bound, object_control.handoff). release ends the
-- binding from bound, manual or stopping. Every change moves the control epoch by exactly one.
--
-- A binding records the delegation it holds under (decision D-5, replacing S3-D12's company authority
-- epoch as the fence): the task's initiator and AI agent with the authority epoch of each one's last
-- grant change (brant_auth.actor_grant.changed_epoch) when the bind or the handoff was made. A write
-- on a bound root passes only while both grants are active at those epochs. control_authority_epoch
-- stays, recorded at each bind and handoff and no longer compared. Roots bound before this migration
-- name no delegation; TRD admits no write on them until they are stopped and handed back or released.
--
-- The trigger guard_quote_root keeps its S3 rules (identity and owner fixed, versions and epochs never
-- decrease, control only with a new epoch, metadata only with a new root version) and adds the state
-- machine: the transitions above, one epoch each; the task fixed from the bind to the release; a bind
-- or handoff naming the delegation; the delegation unchanged by a stop or a switch to manual.

ALTER TABLE brant_trd.quote_root DROP CONSTRAINT quote_root_control_state_check;
ALTER TABLE brant_trd.quote_root ADD CONSTRAINT quote_root_control_state_check
    CHECK (control_state IN ('unbound', 'bound', 'stopping', 'manual'));

ALTER TABLE brant_trd.quote_root
    ADD COLUMN control_initiator_id    text   CHECK (control_initiator_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN control_initiator_epoch bigint CHECK (control_initiator_epoch > 0),
    ADD COLUMN control_agent_id        text   CHECK (control_agent_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN control_agent_epoch     bigint CHECK (control_agent_epoch > 0),
    -- The delegation is named whole or not at all, never on an unbound root, and its two actors differ.
    ADD CONSTRAINT quote_root_control_delegation_whole CHECK (
        (control_initiator_id IS NULL) = (control_initiator_epoch IS NULL)
        AND (control_initiator_id IS NULL) = (control_agent_id IS NULL)
        AND (control_agent_id IS NULL) = (control_agent_epoch IS NULL)),
    ADD CONSTRAINT quote_root_control_delegation_bound CHECK (control_state <> 'unbound' OR control_initiator_id IS NULL),
    ADD CONSTRAINT quote_root_control_delegation_actors CHECK (control_initiator_id <> control_agent_id);

CREATE OR REPLACE FUNCTION brant_trd.guard_quote_root() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    control constant text[] := ARRAY['control_state', 'control_epoch', 'control_task_id', 'control_authority_epoch',
        'control_by_kind', 'control_by_id', 'control_changed_at', 'control_initiator_id', 'control_initiator_epoch',
        'control_agent_id', 'control_agent_epoch'];
    delegation constant text[] := ARRAY['control_initiator_id', 'control_initiator_epoch', 'control_agent_id',
        'control_agent_epoch'];
    versioned constant text[] := ARRAY['root_version', 'last_revision', 'labels', 'internal_note'];
    o jsonb := to_jsonb(OLD);
    n jsonb := to_jsonb(NEW);
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_trd.quote_root: roots are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (o - control - versioned - 'updated_at') IS DISTINCT FROM (n - control - versioned - 'updated_at')
            OR NEW.root_version < OLD.root_version OR NEW.last_revision < OLD.last_revision
            OR NEW.control_epoch < OLD.control_epoch THEN
        RAISE EXCEPTION 'brant_trd.quote_root: identity and owner are fixed; versions and epochs never decrease'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.control_epoch = OLD.control_epoch AND (SELECT bool_or(o -> k IS DISTINCT FROM n -> k) FROM unnest(control) AS k) THEN
        RAISE EXCEPTION 'brant_trd.quote_root: the object control changes only with a new control epoch'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.control_epoch <> OLD.control_epoch THEN
        IF NEW.control_epoch <> OLD.control_epoch + 1 OR (OLD.control_state, NEW.control_state) NOT IN (
                ('unbound', 'bound'), ('bound', 'stopping'), ('manual', 'stopping'), ('stopping', 'manual'), ('stopping', 'bound'),
                ('bound', 'unbound'), ('manual', 'unbound'), ('stopping', 'unbound')) THEN
            RAISE EXCEPTION 'brant_trd.quote_root: the object control moves from % to % only along SM-TRD-CONTROL, one epoch at a time',
                OLD.control_state, NEW.control_state USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF OLD.control_state <> 'unbound' AND NEW.control_state <> 'unbound'
                AND NEW.control_task_id IS DISTINCT FROM OLD.control_task_id THEN
            RAISE EXCEPTION 'brant_trd.quote_root: a binding keeps its task until it is released'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.control_state = 'bound' AND NEW.control_initiator_id IS NULL THEN
            RAISE EXCEPTION 'brant_trd.quote_root: a bind or handoff names the delegation'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.control_state IN ('stopping', 'manual')
                AND (SELECT bool_or(o -> k IS DISTINCT FROM n -> k) FROM unnest(delegation) AS k) THEN
            RAISE EXCEPTION 'brant_trd.quote_root: a stop or a switch to manual keeps the delegation'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF (SELECT bool_or(o -> k IS DISTINCT FROM n -> k) FROM unnest(ARRAY['last_revision', 'labels', 'internal_note']) AS k)
            AND NEW.root_version = OLD.root_version THEN
        RAISE EXCEPTION 'brant_trd.quote_root: revisions and metadata change only with a new root version'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
