-- TSK: the history of a task keeps what it was resumed with (WBS S4-01, S4-03; S4 unit G3).
--
-- task.resume names what arrived (arrival {kind, id, version}): the customer's reply, the verified
-- acceptance, the approval grant a person issued. Until now TSK used it to admit the resume and
-- kept it only inside the free-text reason, and only when the caller gave none. The agent that
-- continues the task has to find what arrived — the source of the customer's reply is what its
-- quote.response.record cites — so the history row of a resume now records the arrival as given.
-- The agent runner reads it from the task's history into the facts of the next turn.
--
-- The three columns are set together and only by task.resume; a resume of a paused task, which
-- needs a reason and no arrival, leaves them NULL, as do the rows written before this migration.
-- History rows are still never changed or deleted (guard_task_transition).

ALTER TABLE brant_tsk.task_transition
    ADD COLUMN arrival_kind    text   CHECK (arrival_kind ~ '^[a-z][a-z0-9_]{0,39}$'),
    ADD COLUMN arrival_id      text   CHECK (arrival_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    ADD COLUMN arrival_version bigint CHECK (arrival_version >= 0);

ALTER TABLE brant_tsk.task_transition ADD CONSTRAINT task_transition_arrival_whole_check
    CHECK (num_nulls(arrival_kind, arrival_id, arrival_version) IN (0, 3));
ALTER TABLE brant_tsk.task_transition ADD CONSTRAINT task_transition_arrival_command_check
    CHECK (arrival_kind IS NULL OR command = 'task.resume');
