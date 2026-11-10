-- Candidate DDL against a disposable existing v3 core database.
-- Not registered in the application's migration dispatcher.
CREATE TABLE t03_authority_guard(
 tenant TEXT NOT NULL,company TEXT NOT NULL,authority_epoch INTEGER NOT NULL CHECK(authority_epoch>0),
 PRIMARY KEY(tenant,company),FOREIGN KEY(tenant,company) REFERENCES scope
);
CREATE TABLE t03_quote_root(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,root_version INTEGER NOT NULL CHECK(root_version>0),
 last_revision INTEGER NOT NULL CHECK(last_revision>=0),current_active_revision INTEGER,working_generation INTEGER,
 slot_order_id TEXT,owner_actor TEXT NOT NULL,owner_team TEXT,
 PRIMARY KEY(tenant,company,id),FOREIGN KEY(tenant,company) REFERENCES scope,
 FOREIGN KEY(tenant,company,id,current_active_revision) REFERENCES t03_quote_revision(tenant,company,quote_id,revision) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE t03_object_control(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,control_epoch INTEGER NOT NULL CHECK(control_epoch>0),
 mode TEXT NOT NULL CHECK(mode IN('unbound','manual','ai','stopping','unknown')),owner_task_id TEXT,
 PRIMARY KEY(tenant,company,quote_id),FOREIGN KEY(tenant,company,quote_id) REFERENCES t03_quote_root
);
CREATE TABLE t03_quote_draft(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,generation INTEGER NOT NULL CHECK(generation>0),
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),contributors_json TEXT NOT NULL CHECK(json_valid(contributors_json)),
 PRIMARY KEY(tenant,company,quote_id),FOREIGN KEY(tenant,company,quote_id) REFERENCES t03_quote_root
);
CREATE TABLE t03_quote_revision(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>0),
 sealed INTEGER NOT NULL CHECK(sealed IN(0,1)),business_hash TEXT NOT NULL CHECK(length(business_hash)=64),
 commercial_json TEXT NOT NULL CHECK(json_valid(commercial_json)),snapshot_id TEXT NOT NULL,
 issued_at TEXT NOT NULL,expires_at TEXT NOT NULL,
 PRIMARY KEY(tenant,company,quote_id,revision),FOREIGN KEY(tenant,company,quote_id) REFERENCES t03_quote_root,
 FOREIGN KEY(tenant,company,snapshot_id) REFERENCES t02_result(tenant,company,id)
);
CREATE TABLE t03_quote_line(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,line_id TEXT NOT NULL,
 net_minor TEXT NOT NULL CHECK(length(net_minor) BETWEEN 1 AND 24 AND net_minor NOT GLOB '*[^0-9]*' AND (net_minor='0' OR substr(net_minor,1,1)<>'0')),
 tax_minor TEXT NOT NULL CHECK(length(tax_minor) BETWEEN 1 AND 24 AND tax_minor NOT GLOB '*[^0-9]*' AND (tax_minor='0' OR substr(tax_minor,1,1)<>'0')),
 payable_minor TEXT NOT NULL CHECK(length(payable_minor) BETWEEN 1 AND 24 AND payable_minor NOT GLOB '*[^0-9]*' AND (payable_minor='0' OR substr(payable_minor,1,1)<>'0')),
 PRIMARY KEY(tenant,company,quote_id,revision,line_id),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE TABLE t03_quote_charge(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,charge_id TEXT NOT NULL,
 obligation_key TEXT NOT NULL,data_json TEXT NOT NULL CHECK(json_valid(data_json)),
 PRIMARY KEY(tenant,company,quote_id,revision,charge_id),UNIQUE(tenant,company,quote_id,revision,obligation_key),
 FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE TABLE t03_quote_payment(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,stage_id TEXT NOT NULL,
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),
 PRIMARY KEY(tenant,company,quote_id,revision,stage_id),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE TABLE t03_revision_effectivity(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,
 effectivity_version INTEGER NOT NULL CHECK(effectivity_version>0),
 status TEXT NOT NULL CHECK(status IN('frozen','pending_approval','active','superseded','withdrawn','cancelled')),
 PRIMARY KEY(tenant,company,quote_id,revision),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE UNIQUE INDEX t03_one_active ON t03_revision_effectivity(tenant,company,quote_id) WHERE status='active';
CREATE TABLE t03_review_request(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,
 business_hash TEXT NOT NULL,target_command TEXT NOT NULL CHECK(target_command IN('quote.activate','quote.replace')),
 expected_old_active_revision INTEGER,policy_id TEXT NOT NULL,policy_revision INTEGER NOT NULL,
 status TEXT NOT NULL CHECK(status IN('open','approved','returned','rejected','cancelled')),
 PRIMARY KEY(tenant,company,id),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision,
 CHECK((target_command='quote.activate' AND expected_old_active_revision IS NULL) OR (target_command='quote.replace' AND expected_old_active_revision>0))
);
CREATE UNIQUE INDEX t03_one_open_review ON t03_review_request(tenant,company,quote_id,revision) WHERE status='open';
CREATE TABLE t03_approval(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,review_request_id TEXT NOT NULL,
 quote_id TEXT NOT NULL,revision INTEGER NOT NULL,business_hash TEXT NOT NULL,record_json TEXT NOT NULL CHECK(json_valid(record_json)),
 PRIMARY KEY(tenant,company,id),UNIQUE(tenant,company,review_request_id),
 FOREIGN KEY(tenant,company,review_request_id) REFERENCES t03_review_request,
 FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE TABLE t03_approval_effectivity(
 tenant TEXT NOT NULL,company TEXT NOT NULL,approval_id TEXT NOT NULL,effectivity_version INTEGER NOT NULL CHECK(effectivity_version>0),
 status TEXT NOT NULL CHECK(status IN('valid','revoked')),revocation_json TEXT CHECK(revocation_json IS NULL OR json_valid(revocation_json)),
 PRIMARY KEY(tenant,company,approval_id),FOREIGN KEY(tenant,company,approval_id) REFERENCES t03_approval
);
CREATE TABLE t03_document_binding(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,
 role TEXT NOT NULL CHECK(role='primary'),number_id TEXT NOT NULL,file_id TEXT NOT NULL,business_hash TEXT NOT NULL,
 content_hash TEXT NOT NULL CHECK(length(content_hash)=64),template_id TEXT NOT NULL,template_revision INTEGER NOT NULL,
 renderer_version TEXT NOT NULL CHECK(renderer_version='quote-text/0.1.0'),
 PRIMARY KEY(tenant,company,quote_id,revision,role),UNIQUE(tenant,company,number_id),UNIQUE(tenant,company,file_id),
 FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision,
 FOREIGN KEY(tenant,company,number_id) REFERENCES document_number,FOREIGN KEY(tenant,company,file_id) REFERENCES business_file
);
CREATE TABLE t03_dispatch_target_snapshot(
 tenant TEXT NOT NULL,company TEXT NOT NULL,snapshot_id TEXT NOT NULL,event_id TEXT NOT NULL,
 quote_id TEXT NOT NULL,business_revision INTEGER NOT NULL,business_hash TEXT NOT NULL CHECK(length(business_hash)=64),
 customer_ref_json TEXT NOT NULL CHECK(json_valid(customer_ref_json)),contact_ref_json TEXT NOT NULL CHECK(json_valid(contact_ref_json)),
 customer_snapshot_json TEXT NOT NULL CHECK(json_valid(customer_snapshot_json)),contact_snapshot_json TEXT NOT NULL CHECK(json_valid(contact_snapshot_json)),
 snapshot_hash TEXT NOT NULL CHECK(length(snapshot_hash)=64),created_at TEXT NOT NULL,
 PRIMARY KEY(tenant,company,snapshot_id),UNIQUE(tenant,company,event_id),UNIQUE(tenant,company,quote_id,business_revision),
 FOREIGN KEY(tenant,company,quote_id) REFERENCES t03_quote_root
);
CREATE TABLE t03_dependency(
 tenant TEXT NOT NULL,company TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,
 kind TEXT NOT NULL,id TEXT NOT NULL,version INTEGER NOT NULL CHECK(version>0),content_hash TEXT NOT NULL CHECK(length(content_hash)=64),
 PRIMARY KEY(tenant,company,quote_id,revision,kind,id,version),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE TABLE t03_event(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER,
 event_type TEXT NOT NULL,actor TEXT NOT NULL,occurred_at TEXT NOT NULL,data_json TEXT NOT NULL CHECK(json_valid(data_json)),
 PRIMARY KEY(tenant,company,id),FOREIGN KEY(tenant,company,quote_id) REFERENCES t03_quote_root
);
CREATE TABLE t03_commit_evidence(
 tenant TEXT NOT NULL,company TEXT NOT NULL,action_id TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,
 authority_epoch INTEGER NOT NULL CHECK(authority_epoch>0),control_epoch INTEGER NOT NULL CHECK(control_epoch>0),
 evidence_json TEXT NOT NULL CHECK(json_valid(evidence_json)),
 PRIMARY KEY(tenant,company,action_id),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision,
 FOREIGN KEY(tenant,company,action_id) REFERENCES action_result(tenant,company,action_id)
);
CREATE TABLE t03_risk(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,quote_id TEXT NOT NULL,revision INTEGER NOT NULL,
 risk_json TEXT NOT NULL CHECK(json_valid(risk_json)),hold INTEGER NOT NULL CHECK(hold=1),
 PRIMARY KEY(tenant,company,id),FOREIGN KEY(tenant,company,quote_id,revision) REFERENCES t03_quote_revision
);
CREATE TABLE t03_config_root(
 tenant TEXT NOT NULL,company TEXT NOT NULL,kind TEXT NOT NULL CHECK(kind IN('quote_document_template','sales_tax_application')),
 id TEXT NOT NULL,root_version INTEGER NOT NULL CHECK(root_version>0),last_revision INTEGER NOT NULL CHECK(last_revision>0),
 active_revision INTEGER,working_revision INTEGER,
 PRIMARY KEY(tenant,company,kind,id),FOREIGN KEY(tenant,company) REFERENCES scope,
 FOREIGN KEY(tenant,company,kind,id,active_revision) REFERENCES t03_config_revision DEFERRABLE INITIALLY DEFERRED,
 FOREIGN KEY(tenant,company,kind,id,working_revision) REFERENCES t03_config_revision DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE t03_config_revision(
 tenant TEXT NOT NULL,company TEXT NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL CHECK(revision>0),
 sealed INTEGER NOT NULL CHECK(sealed IN(0,1)),data_json TEXT NOT NULL CHECK(json_valid(data_json)),metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)),
 PRIMARY KEY(tenant,company,kind,id,revision),FOREIGN KEY(tenant,company,kind,id) REFERENCES t03_config_root
);
CREATE TABLE t03_config_effectivity(
 tenant TEXT NOT NULL,company TEXT NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,revision INTEGER NOT NULL,
 effectivity_version INTEGER NOT NULL CHECK(effectivity_version>0),
 effectivity TEXT NOT NULL CHECK(effectivity IN('draft','active','superseded','retired','discarded')),
 PRIMARY KEY(tenant,company,kind,id,revision),
 FOREIGN KEY(tenant,company,kind,id,revision) REFERENCES t03_config_revision
);
CREATE UNIQUE INDEX t03_config_one_active ON t03_config_effectivity(tenant,company,kind,id) WHERE effectivity='active';
CREATE TABLE t03_config_event(
 tenant TEXT NOT NULL,company TEXT NOT NULL,id TEXT NOT NULL,kind TEXT NOT NULL,config_id TEXT NOT NULL,
 revision INTEGER NOT NULL,root_version INTEGER NOT NULL CHECK(root_version>0),actor TEXT NOT NULL,occurred_at TEXT NOT NULL,
 event_type TEXT NOT NULL CHECK(event_type IN('save_draft','revise','discard_draft','publish','retire')),
 data_json TEXT NOT NULL CHECK(json_valid(data_json)),PRIMARY KEY(tenant,company,id),
 FOREIGN KEY(tenant,company,kind,config_id,revision) REFERENCES t03_config_revision
);
CREATE TRIGGER t03_root_conversion_guard BEFORE UPDATE OF current_active_revision ON t03_quote_root
 WHEN OLD.slot_order_id IS NOT NULL AND NEW.current_active_revision IS NOT OLD.current_active_revision
 BEGIN SELECT RAISE(ABORT,'ROOT_CONVERSION_LOCKED'); END;
CREATE TRIGGER t03_revision_update BEFORE UPDATE ON t03_quote_revision
 WHEN OLD.sealed=1 OR NEW.sealed<>1 OR NEW.tenant<>OLD.tenant OR NEW.company<>OLD.company OR NEW.quote_id<>OLD.quote_id
 OR NEW.revision<>OLD.revision OR NEW.business_hash<>OLD.business_hash OR NEW.commercial_json<>OLD.commercial_json
 OR NEW.snapshot_id<>OLD.snapshot_id OR NEW.issued_at<>OLD.issued_at OR NEW.expires_at<>OLD.expires_at
 BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_REVISION'); END;
CREATE TRIGGER t03_revision_delete BEFORE DELETE ON t03_quote_revision BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_REVISION'); END;
CREATE TRIGGER t03_line_insert BEFORE INSERT ON t03_quote_line
 WHEN NOT EXISTS(SELECT 1 FROM t03_quote_revision WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision AND sealed=0)
 BEGIN SELECT RAISE(ABORT,'SEALED_T03_CHILD'); END;
CREATE TRIGGER t03_charge_insert BEFORE INSERT ON t03_quote_charge
 WHEN NOT EXISTS(SELECT 1 FROM t03_quote_revision WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision AND sealed=0)
 BEGIN SELECT RAISE(ABORT,'SEALED_T03_CHILD'); END;
CREATE TRIGGER t03_payment_insert BEFORE INSERT ON t03_quote_payment
 WHEN NOT EXISTS(SELECT 1 FROM t03_quote_revision WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision AND sealed=0)
 BEGIN SELECT RAISE(ABORT,'SEALED_T03_CHILD'); END;
CREATE TRIGGER t03_dependency_insert BEFORE INSERT ON t03_dependency
 WHEN NOT EXISTS(SELECT 1 FROM t03_quote_revision WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision AND sealed=0)
 BEGIN SELECT RAISE(ABORT,'SEALED_T03_CHILD'); END;
CREATE TRIGGER t03_seal_complete BEFORE UPDATE OF sealed ON t03_quote_revision WHEN OLD.sealed=0 AND NEW.sealed=1
 BEGIN
 SELECT CASE WHEN (SELECT count(*) FROM t03_quote_line WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision)<>json_array_length(NEW.commercial_json,'$.calculation.lines')
 THEN RAISE(ABORT,'INCOMPLETE_T03_LINES') END;
 SELECT CASE WHEN (SELECT count(*) FROM t03_quote_charge WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision)<>json_array_length(NEW.commercial_json,'$.calculation.charges')
 THEN RAISE(ABORT,'INCOMPLETE_T03_CHARGES') END;
 SELECT CASE WHEN (SELECT count(*) FROM t03_quote_payment WHERE tenant=NEW.tenant AND company=NEW.company AND quote_id=NEW.quote_id AND revision=NEW.revision)<>json_array_length(NEW.commercial_json,'$.calculation.payments')
 THEN RAISE(ABORT,'INCOMPLETE_T03_PAYMENTS') END;
 END;
-- Amounts are decimal TEXT because the contract allows 24 minor-unit digits.
-- Exact conservation and all hash/identity equality remain application guards;
-- SQLite CAST/SUM must not silently narrow them to signed 64-bit integers.
CREATE TRIGGER t03_line_update BEFORE UPDATE ON t03_quote_line BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_line_delete BEFORE DELETE ON t03_quote_line BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_charge_update BEFORE UPDATE ON t03_quote_charge BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_charge_delete BEFORE DELETE ON t03_quote_charge BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_payment_update BEFORE UPDATE ON t03_quote_payment BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_payment_delete BEFORE DELETE ON t03_quote_payment BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_dependency_update BEFORE UPDATE ON t03_dependency BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_dependency_delete BEFORE DELETE ON t03_dependency BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CHILD'); END;
CREATE TRIGGER t03_approval_update BEFORE UPDATE ON t03_approval BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_APPROVAL'); END;
CREATE TRIGGER t03_approval_delete BEFORE DELETE ON t03_approval BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_APPROVAL'); END;
CREATE TRIGGER t03_document_update BEFORE UPDATE ON t03_document_binding BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_DOCUMENT'); END;
CREATE TRIGGER t03_document_delete BEFORE DELETE ON t03_document_binding BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_DOCUMENT'); END;
CREATE TRIGGER t03_dispatch_target_snapshot_update BEFORE UPDATE ON t03_dispatch_target_snapshot BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_DISPATCH_TARGET_SNAPSHOT'); END;
CREATE TRIGGER t03_dispatch_target_snapshot_delete BEFORE DELETE ON t03_dispatch_target_snapshot BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_DISPATCH_TARGET_SNAPSHOT'); END;
CREATE TRIGGER t03_commit_update BEFORE UPDATE ON t03_commit_evidence BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_COMMIT'); END;
CREATE TRIGGER t03_commit_delete BEFORE DELETE ON t03_commit_evidence BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_COMMIT'); END;
CREATE TRIGGER t03_event_update BEFORE UPDATE ON t03_event BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_EVENT'); END;
CREATE TRIGGER t03_event_delete BEFORE DELETE ON t03_event BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_EVENT'); END;
CREATE TRIGGER t03_risk_update BEFORE UPDATE ON t03_risk BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_RISK'); END;
CREATE TRIGGER t03_risk_delete BEFORE DELETE ON t03_risk BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_RISK'); END;
CREATE TRIGGER t03_config_update BEFORE UPDATE ON t03_config_revision WHEN OLD.sealed=1 BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CONFIG'); END;
CREATE TRIGGER t03_config_delete BEFORE DELETE ON t03_config_revision BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CONFIG'); END;
CREATE TRIGGER t03_config_effectivity_cas BEFORE UPDATE ON t03_config_effectivity
 WHEN NEW.tenant<>OLD.tenant OR NEW.company<>OLD.company OR NEW.kind<>OLD.kind OR NEW.id<>OLD.id
 OR NEW.revision<>OLD.revision OR NEW.effectivity_version<>OLD.effectivity_version+1
 BEGIN SELECT RAISE(ABORT,'INVALID_T03_CONFIG_EFFECTIVITY_VERSION'); END;
CREATE TRIGGER t03_config_effectivity_delete BEFORE DELETE ON t03_config_effectivity BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CONFIG_EFFECTIVITY'); END;
CREATE TRIGGER t03_config_event_update BEFORE UPDATE ON t03_config_event BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CONFIG_EVENT'); END;
CREATE TRIGGER t03_config_event_delete BEFORE DELETE ON t03_config_event BEGIN SELECT RAISE(ABORT,'IMMUTABLE_T03_CONFIG_EVENT'); END;
