-- foundation/authorization: the data scope of each grant (BC-D02; Python actor_access.data_scope
-- and team_id). An actor sees the records it owns; with data scope team also the records owned by
-- its team, and with data scope company every record of the company. Changing the data scope alters
-- access, so it increments the scope's authority epoch like any other grant change.

ALTER TABLE brant_auth.actor_grant
    ADD COLUMN data_scope text NOT NULL DEFAULT 'own' CHECK (data_scope IN ('own', 'team', 'company')),
    ADD COLUMN team_id    text CHECK (team_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD CONSTRAINT actor_grant_team_scope CHECK (data_scope <> 'team' OR team_id IS NOT NULL);
