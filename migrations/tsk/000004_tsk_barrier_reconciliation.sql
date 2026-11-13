-- TSK: the stop barrier reconciles a task's open actions itself (SM-TSK-TASK task.finish_stop "屏障完成：
-- submitted／unknown 已对账"; SM-TRD-CONTROL object_control.enter_manual "屏障完成，submitted／unknown 已对账";
-- TSK-INV-002 "外部未知结果先对账"; TSK-INV-003; baseline 8.5 "对已提交或结果未知的动作，先查询及对账，再允许有
-- 冲突的人工或后台动作" and 16.1; WBS S4-01e).
--
-- task.finish_stop is a system command. Before it changes the root's control at TRD, it asks the
-- target context's ActionReconciliation v1 (R18, R66-R69) for every action of the task whose attempt
-- is submitted or unknown, and records each answer in the action's log as action.reconcile does: so a
-- reconciliation row may now name the system actor that completed the barrier. Every other row of
-- the log stays a human's or the task's AI agent's: only people and the agent record attempts and
-- what they saw of them (B1's rule).

ALTER TABLE brant_tsk.action_attempt DROP CONSTRAINT action_attempt_actor_kind_check;
ALTER TABLE brant_tsk.action_attempt ADD CONSTRAINT action_attempt_actor_kind_check
    CHECK (actor_kind IN ('human', 'ai') OR (actor_kind = 'system' AND command = 'action.reconcile'));
