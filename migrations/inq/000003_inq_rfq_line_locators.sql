-- INQ: structured locators, pending fields and provenance on RFQ lines, and the corrections a human
-- makes to the lines an AI actor proposed (WBS S4-04b; BC-AT27 字段定位、逐行修正; core design 5.2
-- RFQLine: the source's own words and the verified structured value are kept apart; INQ entities
-- UnresolvedField and Clarification; SD-07: AI output is a candidate).
--
-- locators: [{field, locator, quote}] places a line's fields in its source (a file extract), in the
-- notation of the extract's reader. pending: [{field, reason, candidates}] names the fields the
-- source does not settle (ambiguous, conflict, missing, unlocated, noted); rfq.confirm_scope refuses
-- while any is left. proposed_by is the actor whose save last changed the line (NULL for lines
-- saved before this migration). Lines still change only while their RFQ is a draft (000001).
--
-- rfq_correction records, in order, every human change, settlement or removal of a line an AI
-- actor proposed (rfq.save_draft) and every field the human verified on rfq.confirm_scope, with the
-- value before and after. Corrections are never updated or deleted: brant_app holds SELECT and
-- INSERT, and a trigger refuses the rest.

ALTER TABLE brant_inq.rfq_line
    ADD COLUMN locators jsonb NOT NULL DEFAULT '[]'
        CHECK (CASE WHEN jsonb_typeof(locators) = 'array' THEN jsonb_array_length(locators) <= 16 ELSE false END),
    ADD COLUMN pending jsonb NOT NULL DEFAULT '[]'
        CHECK (CASE WHEN jsonb_typeof(pending) = 'array' THEN jsonb_array_length(pending) <= 4 ELSE false END),
    ADD COLUMN proposed_by_kind text CHECK (proposed_by_kind IN ('human', 'ai', 'system')),
    ADD COLUMN proposed_by_id text,
    ADD CONSTRAINT rfq_line_proposed_by_check CHECK ((proposed_by_kind IS NULL) = (proposed_by_id IS NULL));

CREATE TABLE brant_inq.rfq_correction (
    tenant_id         text        NOT NULL,
    company_id        text        NOT NULL,
    rfq_id            text        NOT NULL,
    seq               int         NOT NULL CHECK (seq > 0),
    row_version       bigint      NOT NULL CHECK (row_version > 0),
    command           text        NOT NULL CHECK (command IN ('rfq.save_draft', 'rfq.confirm_scope')),
    line_id           text        NOT NULL CHECK (line_id ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$'),
    field             text        NOT NULL CHECK (field IN ('', 'requested_item', 'sku_id', 'quantity', 'uom', 'source_id', 'locator')),
    kind              text        NOT NULL CHECK (kind IN ('changed', 'verified', 'relocated', 'removed')),
    from_value        text        NOT NULL CHECK (char_length(from_value) <= 200),
    to_value          text        NOT NULL CHECK (char_length(to_value) <= 200),
    locator           text        NOT NULL CHECK (char_length(locator) <= 200),
    reason            text        NOT NULL CHECK (reason IN ('', 'ambiguous', 'conflict', 'missing', 'unlocated', 'noted')),
    candidates        text[]      NOT NULL CHECK (cardinality(candidates) <= 10),
    proposed_by_kind  text        CHECK (proposed_by_kind IN ('human', 'ai', 'system')),
    proposed_by_id    text,
    corrected_by_kind text        NOT NULL CHECK (corrected_by_kind = 'human'),
    corrected_by_id   text        NOT NULL,
    recorded_at       timestamptz NOT NULL,
    PRIMARY KEY (tenant_id, company_id, rfq_id, seq),
    FOREIGN KEY (tenant_id, company_id, rfq_id) REFERENCES brant_inq.customer_rfq (tenant_id, company_id, rfq_id),
    CHECK ((kind = 'removed') = (field = '')),
    CHECK ((proposed_by_kind IS NULL) = (proposed_by_id IS NULL))
);

CREATE FUNCTION brant_inq.refuse_correction_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'brant_inq.rfq_correction: corrections are never changed or removed' USING ERRCODE = 'integrity_constraint_violation';
END
$$;
CREATE TRIGGER rfq_correction_immutable BEFORE UPDATE OR DELETE ON brant_inq.rfq_correction
    FOR EACH ROW EXECUTE FUNCTION brant_inq.refuse_correction_change();

ALTER TABLE brant_inq.rfq_correction ENABLE ROW LEVEL SECURITY;
ALTER TABLE brant_inq.rfq_correction FORCE ROW LEVEL SECURITY;
CREATE POLICY rfq_correction_scope ON brant_inq.rfq_correction
    USING (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true) AND company_id = current_setting('app.company_id', true));
