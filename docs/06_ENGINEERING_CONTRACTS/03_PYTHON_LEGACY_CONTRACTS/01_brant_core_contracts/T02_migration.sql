CREATE TABLE t02_root(
 tenant TEXT NOT NULL, company TEXT NOT NULL, namespace TEXT NOT NULL,
 kind TEXT NOT NULL,id TEXT NOT NULL,root_version INTEGER NOT NULL CHECK(root_version>0),
 last_revision INTEGER NOT NULL CHECK(last_revision>0),working_revision INTEGER,active_revision INTEGER,
 PRIMARY KEY(tenant,company,namespace,kind,id),FOREIGN KEY(tenant,company) REFERENCES scope,
 CHECK(working_revision IS NULL OR active_revision IS NULL OR working_revision<>active_revision),
 FOREIGN KEY(tenant,company,namespace,kind,id,working_revision) REFERENCES t02_revision(tenant,company,namespace,kind,id,revision) DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(tenant,company,namespace,kind,id,active_revision) REFERENCES t02_revision(tenant,company,namespace,kind,id,revision) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE t02_revision(
 tenant TEXT NOT NULL,company TEXT NOT NULL,namespace TEXT NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,
 revision INTEGER NOT NULL CHECK(revision>0),sealed INTEGER NOT NULL CHECK(sealed IN(0,1)),
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)),
 PRIMARY KEY(tenant,company,namespace,kind,id,revision),
 FOREIGN KEY(tenant,company,namespace,kind,id) REFERENCES t02_root
);
CREATE TRIGGER t02_sealed_update BEFORE UPDATE ON t02_revision WHEN OLD.sealed=1
 BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_REVISION'); END;
CREATE TRIGGER t02_sealed_delete BEFORE DELETE ON t02_revision WHEN OLD.sealed=1
 BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_REVISION'); END;
CREATE TABLE t02_record(
 tenant TEXT NOT NULL,company TEXT NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,
 row_version INTEGER NOT NULL CHECK(row_version>0),status TEXT NOT NULL,
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)),
 PRIMARY KEY(tenant,company,kind,id),FOREIGN KEY(tenant,company) REFERENCES scope
);
CREATE TRIGGER t02_frozen_record_update BEFORE UPDATE ON t02_record WHEN OLD.kind='cost_scenario' AND OLD.status='frozen'
 BEGIN SELECT RAISE(ABORT,'FROZEN_T02_SCENARIO'); END;
CREATE TRIGGER t02_frozen_record_delete BEFORE DELETE ON t02_record WHEN OLD.kind='cost_scenario' AND OLD.status='frozen'
 BEGIN SELECT RAISE(ABORT,'FROZEN_T02_SCENARIO'); END;
CREATE TABLE t02_result(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN('evaluation','snapshot')),
 scenario_id TEXT NOT NULL,input_version INTEGER NOT NULL,result_hash TEXT NOT NULL CHECK(length(result_hash)=64),
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),binding_json TEXT NOT NULL CHECK(json_valid(binding_json)),
 sealed INTEGER NOT NULL CHECK(sealed IN(0,1)),PRIMARY KEY(tenant,company,id),
 UNIQUE(tenant,company,id,scenario_id,input_version,result_hash),FOREIGN KEY(tenant,company) REFERENCES scope
);
CREATE UNIQUE INDEX t02_one_snapshot ON t02_result(tenant,company,scenario_id) WHERE kind='snapshot';
CREATE TABLE t02_component(
 tenant TEXT NOT NULL,company TEXT NOT NULL,snapshot_id TEXT NOT NULL,id TEXT NOT NULL,
 amount_minor INTEGER NOT NULL CHECK(amount_minor>=0),PRIMARY KEY(tenant,company,snapshot_id,id),
 FOREIGN KEY(tenant,company,snapshot_id) REFERENCES t02_result(tenant,company,id)
);
CREATE TABLE t02_allocation(
 tenant TEXT NOT NULL,company TEXT NOT NULL,snapshot_id TEXT NOT NULL,component_id TEXT NOT NULL,line_id TEXT NOT NULL,
 amount_minor INTEGER NOT NULL CHECK(amount_minor>=0),base_integer TEXT NOT NULL,
 PRIMARY KEY(tenant,company,snapshot_id,component_id,line_id),
 FOREIGN KEY(tenant,company,snapshot_id,component_id) REFERENCES t02_component
);
CREATE TRIGGER t02_component_insert BEFORE INSERT ON t02_component
 WHEN NOT EXISTS(SELECT 1 FROM t02_result WHERE tenant=NEW.tenant AND company=NEW.company AND id=NEW.snapshot_id AND kind='snapshot' AND sealed=0)
 BEGIN SELECT RAISE(ABORT,'SEALED_T02_CHILDREN'); END;
CREATE TRIGGER t02_allocation_insert BEFORE INSERT ON t02_allocation
 WHEN NOT EXISTS(SELECT 1 FROM t02_result WHERE tenant=NEW.tenant AND company=NEW.company AND id=NEW.snapshot_id AND kind='snapshot' AND sealed=0)
 BEGIN SELECT RAISE(ABORT,'SEALED_T02_CHILDREN'); END;
CREATE TRIGGER t02_component_update BEFORE UPDATE ON t02_component BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_CHILDREN'); END;
CREATE TRIGGER t02_component_delete BEFORE DELETE ON t02_component BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_CHILDREN'); END;
CREATE TRIGGER t02_allocation_update BEFORE UPDATE ON t02_allocation BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_CHILDREN'); END;
CREATE TRIGGER t02_allocation_delete BEFORE DELETE ON t02_allocation BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_CHILDREN'); END;
CREATE TRIGGER t02_result_update BEFORE UPDATE ON t02_result
 WHEN OLD.sealed=1 OR NEW.sealed<>1 OR NEW.tenant<>OLD.tenant OR NEW.company<>OLD.company OR NEW.id<>OLD.id
 OR NEW.kind<>OLD.kind OR NEW.scenario_id<>OLD.scenario_id OR NEW.input_version<>OLD.input_version
 OR NEW.result_hash<>OLD.result_hash OR NEW.data_json<>OLD.data_json OR NEW.binding_json<>OLD.binding_json
 BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_RESULT'); END;
CREATE TRIGGER t02_result_delete BEFORE DELETE ON t02_result BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T02_RESULT'); END;
CREATE TRIGGER t02_snapshot_seal BEFORE UPDATE OF sealed ON t02_result WHEN OLD.kind='snapshot' AND OLD.sealed=0
 BEGIN
 SELECT CASE WHEN (SELECT count(*) FROM t02_component WHERE tenant=NEW.tenant AND company=NEW.company AND snapshot_id=NEW.id)<>json_array_length(NEW.data_json,'$.components')
 THEN RAISE(ABORT,'INCOMPLETE_T02_COMPONENTS') END;
 SELECT CASE WHEN EXISTS(
 SELECT 1 FROM json_each(NEW.data_json,'$.components') c
 LEFT JOIN t02_component p ON p.tenant=NEW.tenant AND p.company=NEW.company AND p.snapshot_id=NEW.id AND p.id=json_extract(c.value,'$.id')
 WHERE p.id IS NULL OR p.amount_minor<>CAST(json_extract(c.value,'$.amount_minor') AS INTEGER)
 OR (SELECT count(*) FROM t02_allocation a WHERE a.tenant=p.tenant AND a.company=p.company AND a.snapshot_id=p.snapshot_id AND a.component_id=p.id)<>json_array_length(c.value,'$.allocations')
 OR (SELECT coalesce(sum(a.amount_minor),0) FROM t02_allocation a WHERE a.tenant=p.tenant AND a.company=p.company AND a.snapshot_id=p.snapshot_id AND a.component_id=p.id)<>p.amount_minor
 ) THEN RAISE(ABORT,'INCOMPLETE_T02_ALLOCATIONS') END;
 SELECT CASE WHEN EXISTS(
 SELECT 1 FROM json_each(NEW.data_json,'$.components') c, json_each(c.value,'$.allocations') a
 LEFT JOIN t02_allocation p ON p.tenant=NEW.tenant AND p.company=NEW.company AND p.snapshot_id=NEW.id
 AND p.component_id=json_extract(c.value,'$.id') AND p.line_id=json_extract(a.value,'$.line_id')
 WHERE p.line_id IS NULL OR p.amount_minor<>CAST(json_extract(a.value,'$.amount_minor') AS INTEGER)
 OR p.base_integer<>json_extract(a.value,'$.base_integer')
 ) THEN RAISE(ABORT,'MISMATCHED_T02_ALLOCATIONS') END;
 SELECT CASE WHEN (SELECT coalesce(sum(amount_minor),0) FROM t02_component
 WHERE tenant=NEW.tenant AND company=NEW.company AND snapshot_id=NEW.id)<>CAST(json_extract(NEW.data_json,'$.total_minor') AS INTEGER)
 THEN RAISE(ABORT,'MISMATCHED_T02_TOTAL') END;
 END;
CREATE TABLE t02_source_attestation(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,source_id TEXT NOT NULL,
 actor_id TEXT NOT NULL,recorded_at TEXT NOT NULL,reason TEXT NOT NULL,
 PRIMARY KEY(tenant,company,id),FOREIGN KEY(tenant,company,source_id) REFERENCES source_origin(tenant,company,id),
 FOREIGN KEY(tenant,company,actor_id) REFERENCES authorization(tenant,company,actor)
);
CREATE TRIGGER t02_attestation_update BEFORE UPDATE ON t02_source_attestation BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TRIGGER t02_attestation_delete BEFORE DELETE ON t02_source_attestation BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TABLE t02_denial(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,source_id TEXT NOT NULL,
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),PRIMARY KEY(tenant,company,id),
 FOREIGN KEY(tenant,company,source_id) REFERENCES source_origin(tenant,company,id)
);
CREATE TRIGGER t02_denial_update BEFORE UPDATE ON t02_denial BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TRIGGER t02_denial_delete BEFORE DELETE ON t02_denial BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TABLE t02_action_dependency(
 tenant TEXT NOT NULL,company TEXT NOT NULL,action_id TEXT NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,version INTEGER NOT NULL,content_hash TEXT NOT NULL,
 PRIMARY KEY(tenant,company,action_id,kind,id,version),FOREIGN KEY(tenant,company,action_id) REFERENCES action_result(tenant,company,action_id)
);
CREATE TABLE t02_policy(
 tenant TEXT NOT NULL,company TEXT NOT NULL,ref_json TEXT NOT NULL CHECK(json_valid(ref_json)),
 PRIMARY KEY(tenant,company),FOREIGN KEY(tenant,company) REFERENCES scope
);
CREATE TABLE t02_command_event(
 tenant TEXT NOT NULL,company TEXT NOT NULL,action_id TEXT NOT NULL,command TEXT NOT NULL,
 actor_id TEXT NOT NULL,recorded_at TEXT NOT NULL,args_json TEXT NOT NULL CHECK(json_valid(args_json)),
 events_json TEXT NOT NULL CHECK(json_valid(events_json)),PRIMARY KEY(tenant,company,action_id),
 FOREIGN KEY(tenant,company,action_id) REFERENCES action_result(tenant,company,action_id)
);
CREATE TRIGGER t02_command_event_update BEFORE UPDATE ON t02_command_event BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TRIGGER t02_command_event_delete BEFORE DELETE ON t02_command_event BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TABLE t02_rfq_binding(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,row_version INTEGER NOT NULL,
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),PRIMARY KEY(tenant,company,id,row_version),
 FOREIGN KEY(tenant,company,id) REFERENCES rfq(tenant,company,id)
);
CREATE TRIGGER t02_binding_update BEFORE UPDATE ON t02_rfq_binding BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
CREATE TRIGGER t02_binding_delete BEFORE DELETE ON t02_rfq_binding BEGIN SELECT RAISE(ABORT,'APPEND_ONLY'); END;
