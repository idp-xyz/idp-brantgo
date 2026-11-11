-- CST: cost scenarios and their evaluations (WBS S2-05, first half; tactical model
-- CST-AGG-CostScenario and SM-CST-SCENARIO; Python costing_engine integrated_model._cost and
-- command_model cost_scenario.*, T02 §7, §13.3).
--
-- A cost scenario is (tenant, company, scenario_id) with a root version every save compares and
-- increments. Its draft (data) is replaced whole while it is a draft; cost_scenario.evaluate
-- appends an immutable evaluation and leaves the root version as it is, counting evaluations in
-- last_evaluation; cost_snapshot.freeze (the next step) moves a draft to frozen once, with the next
-- root version, and nothing changes after that. Nothing is ever deleted.
--
-- An evaluation stores two canonical JSON documents with their SHA-256: the normalized input (the
-- scenario's normalized selection and every input revision it read with its content hash) and the
-- result (amounts in integer minor units, outcome, differences, checks). The result binds the input
-- hash and the engine version; neither document carries the evaluation's identity or instant, so
-- the same input always yields the same bytes. An evaluation is appended only to a draft at its
-- current root version, under the number the root just counted, and never changes.
--
-- References to INQ, CAT, POL and SUP are plain identifiers inside the documents: no foreign key
-- crosses a schema (DB2).

CREATE TABLE brant_cst.cost_scenario (
    tenant_id       text        NOT NULL CHECK (tenant_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    company_id      text        NOT NULL CHECK (company_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    scenario_id     text        NOT NULL CHECK (scenario_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    root_version    bigint      NOT NULL CHECK (root_version > 0),
    status          text        NOT NULL CHECK (status IN ('draft', 'frozen')),
    data            jsonb       NOT NULL CHECK (jsonb_typeof(data) = 'object'),
    content_hash    text        NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    contributors    text[]      NOT NULL CHECK (cardinality(contributors) >= 1),
    saved_by_kind   text        NOT NULL CHECK (saved_by_kind IN ('human', 'ai')),
    saved_by_id     text        NOT NULL,
    saved_epoch     bigint      NOT NULL CHECK (saved_epoch > 0),
    saved_at        timestamptz NOT NULL,
    last_evaluation bigint      NOT NULL CHECK (last_evaluation >= 0),
    owner_actor_id  text        NOT NULL,
    owner_team_id   text,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, scenario_id),
    -- A frozen scenario was evaluated (CST-INV-001: a freeze binds an evaluation).
    CHECK (status = 'draft' OR last_evaluation >= 1)
);

CREATE TABLE brant_cst.cost_evaluation (
    tenant_id             text        NOT NULL,
    company_id            text        NOT NULL,
    evaluation_id         text        NOT NULL CHECK (evaluation_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    scenario_id           text        NOT NULL,
    evaluation_no         bigint      NOT NULL CHECK (evaluation_no > 0),
    scenario_version      bigint      NOT NULL CHECK (scenario_version > 0),
    evaluated_by_kind     text        NOT NULL CHECK (evaluated_by_kind IN ('human', 'ai')),
    evaluated_by_id       text        NOT NULL,
    authority_epoch       bigint      NOT NULL CHECK (authority_epoch > 0),
    evaluated_at          timestamptz NOT NULL,
    engine_version        text        NOT NULL CHECK (char_length(engine_version) BETWEEN 1 AND 100),
    normalization_version text        NOT NULL CHECK (char_length(normalization_version) BETWEEN 1 AND 100),
    normalized_input      text        NOT NULL,
    input_hash            text        NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
    result                text        NOT NULL,
    result_hash           text        NOT NULL CHECK (result_hash ~ '^[0-9a-f]{64}$'),
    verification_status   text        NOT NULL CHECK (verification_status IN ('verified', 'provisional')),
    comparison_status     text        NOT NULL CHECK (comparison_status IN ('comparable', 'not_comparable')),
    currency              text        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    total_minor           numeric(32, 0) NOT NULL CHECK (total_minor >= 0),
    PRIMARY KEY (tenant_id, company_id, evaluation_id),
    UNIQUE (tenant_id, company_id, scenario_id, evaluation_no),
    FOREIGN KEY (tenant_id, company_id, scenario_id) REFERENCES brant_cst.cost_scenario (tenant_id, company_id, scenario_id),
    -- The documents are the bytes their hashes cover, valid JSON objects; the result binds the
    -- input hash and engine version, and the columns repeat what the result says.
    CHECK (encode(sha256(convert_to(normalized_input, 'UTF8')), 'hex') = input_hash),
    CHECK (encode(sha256(convert_to(result, 'UTF8')), 'hex') = result_hash),
    CHECK (jsonb_typeof(normalized_input::jsonb) = 'object' AND normalized_input::jsonb ->> 'normalization_version' = normalization_version
           AND normalized_input::jsonb ->> 'scenario_id' = scenario_id),
    CHECK (jsonb_typeof(result::jsonb) = 'object' AND result::jsonb ->> 'input_hash' = input_hash
           AND result::jsonb ->> 'engine_version' = engine_version AND result::jsonb ->> 'scenario_id' = scenario_id
           AND result::jsonb ->> 'verification_status' = verification_status AND result::jsonb ->> 'comparison_status' = comparison_status
           AND result::jsonb ->> 'currency' = currency AND (result::jsonb ->> 'total_minor')::numeric = total_minor)
);

CREATE INDEX cost_evaluation_by_input ON brant_cst.cost_evaluation (tenant_id, company_id, scenario_id, input_hash);

-- A scenario keeps its identity and owner. While it is a draft, a save replaces its content with
-- the next root version, and an evaluation counts itself without changing anything else; a freeze
-- moves it to frozen with the next root version and its content and evaluations unchanged. A
-- frozen scenario never changes; nothing is deleted.
CREATE FUNCTION brant_cst.guard_cost_scenario() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    content constant text[] := ARRAY['root_version', 'data', 'content_hash', 'contributors', 'saved_by_kind', 'saved_by_id',
        'saved_epoch', 'saved_at', 'updated_at'];
    o jsonb := to_jsonb(OLD);
    n jsonb := to_jsonb(NEW);
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'brant_cst.cost_scenario: scenarios are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.root_version > OLD.root_version
            AND NEW.last_evaluation = OLD.last_evaluation AND (o - content) = (n - content) THEN
        RETURN NEW; -- save_draft
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'draft' AND NEW.last_evaluation = OLD.last_evaluation + 1
            AND (o - ARRAY['last_evaluation', 'updated_at']) = (n - ARRAY['last_evaluation', 'updated_at']) THEN
        RETURN NEW; -- evaluate
    END IF;
    IF OLD.status = 'draft' AND NEW.status = 'frozen' AND NEW.root_version = OLD.root_version + 1
            AND (o - ARRAY['status', 'root_version', 'updated_at']) = (n - ARRAY['status', 'root_version', 'updated_at']) THEN
        RETURN NEW; -- freeze
    END IF;
    RAISE EXCEPTION 'brant_cst.cost_scenario: % to % is not a scenario transition', OLD.status, NEW.status
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER cost_scenario_guard BEFORE UPDATE OR DELETE ON brant_cst.cost_scenario
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_scenario();

-- An evaluation is appended to a draft at its current root version under the number the root has
-- just counted (the root is updated first in the same transaction), and never changes.
CREATE FUNCTION brant_cst.guard_cost_evaluation() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    s brant_cst.cost_scenario%ROWTYPE;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'brant_cst.cost_evaluation: evaluations never change and are never deleted'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    SELECT * INTO s FROM brant_cst.cost_scenario
        WHERE tenant_id = NEW.tenant_id AND company_id = NEW.company_id AND scenario_id = NEW.scenario_id;
    IF NOT FOUND OR s.status <> 'draft' OR s.root_version <> NEW.scenario_version OR s.last_evaluation <> NEW.evaluation_no THEN
        RAISE EXCEPTION 'brant_cst.cost_evaluation: evaluation % does not follow the draft it evaluates', NEW.evaluation_id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER cost_evaluation_guard BEFORE INSERT OR UPDATE OR DELETE ON brant_cst.cost_evaluation
    FOR EACH ROW EXECUTE FUNCTION brant_cst.guard_cost_evaluation();

ALTER TABLE brant_cst.cost_scenario ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_scenario FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_scenario_scope ON brant_cst.cost_scenario
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));

ALTER TABLE brant_cst.cost_evaluation ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_cst.cost_evaluation FORCE ROW LEVEL SECURITY;
CREATE POLICY cost_evaluation_scope ON brant_cst.cost_evaluation
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
