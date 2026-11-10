-- foundation/messaging: dead letters of the Outbox relay (ADR-BR-015).
--
-- A delivery the relay gives up on (failure or attempt budget spent, permanent rejection by the
-- sink, invalid envelope) is stored here in the same transaction that abandons it in the Bento
-- outbox: the envelope as Bento encodes it, attempts, failures, the failure code and the last error.
-- An authorized human replays it by event ID with the same event ID, so consumers deduplicate. A
-- replay marks the row replayed and never deletes it. FORCE RLS on app.tenant_id and app.company_id.

CREATE SCHEMA brant_msg;

CREATE TABLE brant_msg.dead_letter (
    tenant_id         text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id        text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    source            text        NOT NULL CHECK (btrim(source) <> '' AND octet_length(source) <= 255),
    event_id          text        NOT NULL CHECK (btrim(event_id) <> '' AND octet_length(event_id) <= 128),
    event_type        text        NOT NULL CHECK (octet_length(event_type) <= 255),
    envelope          jsonb       NOT NULL CHECK (jsonb_typeof(envelope) = 'object'),
    attempt           bigint      NOT NULL CHECK (attempt BETWEEN 0 AND 4294967295),
    failures          bigint      NOT NULL CHECK (failures BETWEEN 0 AND 4294967295),
    failure_code      text        NOT NULL CHECK (failure_code ~ '^[a-z0-9][a-z0-9._-]{0,127}$'),
    last_error        text        CHECK (octet_length(last_error) <= 1024),
    critical          boolean     NOT NULL,
    dead_at           timestamptz NOT NULL,
    state             text        NOT NULL DEFAULT 'dead' CHECK (state IN ('dead', 'replayed')),
    replay_attempts   bigint      NOT NULL DEFAULT 0 CHECK (replay_attempts >= 0),
    last_replay_at    timestamptz,
    last_replay_error text        CHECK (octet_length(last_replay_error) <= 1024),
    replayed_at       timestamptz,
    replayed_by_kind  text        CHECK (replayed_by_kind = 'human'),
    replayed_by_id    text        CHECK (replayed_by_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    replay_epoch      bigint      CHECK (replay_epoch > 0),
    PRIMARY KEY (tenant_id, company_id, source, event_id),
    CONSTRAINT dead_letter_replay_shape_chk CHECK (
        (state = 'dead' AND replayed_at IS NULL AND replayed_by_kind IS NULL AND replayed_by_id IS NULL
            AND replay_epoch IS NULL)
        OR (state = 'replayed' AND replayed_at IS NOT NULL AND replayed_by_kind IS NOT NULL
            AND replayed_by_id IS NOT NULL AND replay_epoch IS NOT NULL AND replay_attempts > 0)
    )
);

ALTER TABLE brant_msg.dead_letter ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_msg.dead_letter FORCE ROW LEVEL SECURITY;

CREATE POLICY dead_letter_scope ON brant_msg.dead_letter
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
