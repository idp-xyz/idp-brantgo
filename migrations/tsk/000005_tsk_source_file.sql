-- TSK: a task may start from an uploaded file (owner decision 2026-11-18; WBS S4-01, S4-04; S4 unit G2).
--
-- Until now a task's source was an EVD source or an INQ RFQ: something that exists when the task is
-- created. A Q01 task that starts from a customer's inquiry file has no such source yet — the source
-- that holds the file's text is registered by the task's own agent (EVD source.register, source_kind
-- file_extract, naming the file) as the task's first step. Such a task names the file itself:
-- source_kind evd_file, source_id the EVD business file (file.register). The committed action of the
-- task then records which source the agent registered for it.
--
-- 来源防重 is unchanged and covers the new kind as it covers the others: the partial unique index
-- business_task_open_source is over (tenant, company, goal, source_kind, source_id) of every open
-- task, so one open task serves a goal for a file. source_kind and source_id stay creation fields
-- the guard trigger never lets change.
--
-- Migration 000001 declared the kinds as an unnamed column CHECK, which PostgreSQL named
-- business_task_source_kind_check; it is replaced by the wider one under the same name.

ALTER TABLE brant_tsk.business_task DROP CONSTRAINT business_task_source_kind_check;
ALTER TABLE brant_tsk.business_task ADD CONSTRAINT business_task_source_kind_check
    CHECK (source_kind IN ('evd_source', 'evd_file', 'rfq'));
