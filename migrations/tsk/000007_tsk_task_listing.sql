-- TSK (S7-06 read models): the order of task.list. A list returns the tasks of one company the
-- caller sees, most recently changed first, a page at a time; the page boundary is the pair
-- (updated_at, task_id). No column changes: updated_at is set by every write since 000001.

CREATE INDEX business_task_listing
    ON brant_tsk.business_task (tenant_id, company_id, updated_at DESC, task_id DESC);
