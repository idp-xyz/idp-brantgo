package capability

import (
	"context"
	"errors"

	"go.idp.xyz/idp-brant/internal/ai/ingest"
	"go.idp.xyz/idp-brant/internal/ai/locate"
	cstapi "go.idp.xyz/idp-brant/internal/contexts/cst/api"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	inqapi "go.idp.xyz/idp-brant/internal/contexts/inq/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
)

// Q01Targets are the api command entry points the Q01 capabilities call. bootstrap passes the
// composed context modules.
type Q01Targets struct {
	EVD evdapi.Commands
	// Extracts reads sources and file extracts for the source locating of rfq.save_draft (unit
	// B4); without it, a line that places fields in a source is REFERENCE_INVALID.
	Extracts      locate.Extracts
	INQ           inqapi.Commands
	CST           cstapi.Commands
	CSTScenarios  cstapi.ScenarioCommands
	TRD           trdapi.Commands
	TRDAcceptance trdapi.AcceptanceCommands
	TRDOrders     trdapi.OrderCommands
}

// errNoTarget is the error of a capability whose context was not given.
var errNoTarget = errors.New("capability: the context of this capability is not wired")

// Q01 returns the capabilities of the AI execution identity in a Q01 task (decision D-7, role
// template ai_q01_agent): source and file registration, RFQ, costing context and cost scenario
// drafts and evaluation, quote drafts, revisions and internal metadata, customer response
// recording — autonomous under a delegation; order preparation and order review requests — each
// with a human-issued approval grant. Every other command is refused (default deny); the
// human-only commands (rfq.confirm_scope, quote.freeze, quote.submit_review, quote.activate,
// quote.response.verify, order.activate, ...) can never be added. Reads are not capabilities yet:
// the agent runner (unit C2) adds the ones it needs.
func Q01(t Q01Targets) ([]Capability, error) {
	var out []Capability
	var errs []error
	add := func(c Capability, err error) {
		out = append(out, c)
		errs = append(errs, err)
	}
	// Version 2 (unit B4): SourceKind file_extract with FileID has EVD read a registered business file
	// with the AI layer's extractor; the model never supplies an extract's text.
	add(Define(Spec{Command: evdapi.CommandRegisterSource, Context: "evd", Contract: evdapi.Contract, Version: 2,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Register a text source (manual_text or an email body as email_text) as evidence, or the text of a registered business file (SourceKind file_extract with FileID, no ContentText): EVD reads the file with the platform's extractor; returns the source ID."},
		func(c evdapi.RegisterSource) ObjectRef { return ObjectRef{Type: "evd.source", ID: c.SourceID} },
		func(ctx context.Context, c evdapi.RegisterSource) (evdapi.Receipt, error) {
			if t.EVD == nil {
				return evdapi.Receipt{}, errNoTarget
			}
			return t.EVD.RegisterSource(ctx, c)
		}))
	add(Define(Spec{Command: evdapi.CommandRegisterFile, Context: "evd", Contract: evdapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Register an immutable business file (text, PDF or XLSX, Base64) as evidence."},
		func(c evdapi.RegisterFile) ObjectRef { return ObjectRef{Type: "evd.file", ID: c.FileID} },
		func(ctx context.Context, c evdapi.RegisterFile) (evdapi.Receipt, error) {
			if t.EVD == nil {
				return evdapi.Receipt{}, errNoTarget
			}
			return t.EVD.RegisterFile(ctx, c)
		}))
	// Version 2 (unit B4): lines place their fields in file extracts (Locators: field, locator,
	// quote) and name pending fields; the preparation verifies every locator against the extract
	// and completes the pending fields before the call is hashed (internal/ai/locate).
	saveRfq, err := Define(Spec{Command: inqapi.CommandSaveRfq, Context: "inq", Contract: inqapi.Contract, Version: 2,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Save a customer RFQ draft with its lines, cited sources and unresolved fields; a human verifies every line and confirms its scope. For a line read from a file_extract source, give Locators: for each field (requested_item, sku_id, quantity, uom) the locator in the extract (text line:col, PDF page:line, XLSX Sheet!B3) and the source's own words there (Quote); name Pending fields (ambiguous, conflict, missing) with Candidates. Locators are verified against the extract."},
		func(c inqapi.SaveRfq) ObjectRef {
			return ObjectRef{Type: "inq.rfq", ID: c.RfqID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c inqapi.SaveRfq) (inqapi.Receipt, error) {
			if t.INQ == nil {
				return inqapi.Receipt{}, errNoTarget
			}
			return t.INQ.SaveRfq(ctx, c)
		})
	locator := locate.Locator{Extracts: t.Extracts, Profile: ingest.Q01Upload}
	add(Prepared(saveRfq, err, func(ctx context.Context, c inqapi.SaveRfq) (inqapi.SaveRfq, error) {
		lines, err := locator.Lines(ctx, c.Lines)
		if err != nil {
			return inqapi.SaveRfq{}, err
		}
		if c.Lines != nil {
			c.Lines = lines
		}
		return c, nil
	}))
	add(Define(Spec{Command: cstapi.CommandSaveContext, Context: "cst", Contract: cstapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Save a costing context draft for an RFQ; a human verifies it."},
		func(c cstapi.SaveContext) ObjectRef {
			return ObjectRef{Type: "cst.costing_context", ID: c.ContextID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c cstapi.SaveContext) (cstapi.Receipt, error) {
			if t.CST == nil {
				return cstapi.Receipt{}, errNoTarget
			}
			return t.CST.SaveContext(ctx, c)
		}))
	add(Define(Spec{Command: cstapi.CommandSaveScenario, Context: "cst", Contract: cstapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Save a cost scenario draft (inputs, prices, selections) for a verified costing context."},
		func(c cstapi.SaveScenario) ObjectRef {
			return ObjectRef{Type: "cst.cost_scenario", ID: c.ScenarioID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c cstapi.SaveScenario) (cstapi.Receipt, error) {
			if t.CSTScenarios == nil {
				return cstapi.Receipt{}, errNoTarget
			}
			return t.CSTScenarios.SaveScenario(ctx, c)
		}))
	add(Define(Spec{Command: cstapi.CommandEvaluateScenario, Context: "cst", Contract: cstapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Evaluate the current cost scenario draft; the evaluation is appended and the draft stays as it is."},
		func(c cstapi.EvaluateScenario) ObjectRef {
			return ObjectRef{Type: "cst.cost_scenario", ID: c.ScenarioID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c cstapi.EvaluateScenario) (cstapi.EvaluationReceipt, error) {
			if t.CSTScenarios == nil {
				return cstapi.EvaluationReceipt{}, errNoTarget
			}
			return t.CSTScenarios.EvaluateScenario(ctx, c)
		}))
	add(Define(Spec{Command: trdapi.CommandSaveDraft, Context: "trd", Contract: trdapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskMedium, Approval: ApprovalNone,
		Summary: "Save the working revision of a quote draft; a human freezes, reviews and activates it."},
		func(c trdapi.SaveDraft) ObjectRef {
			return ObjectRef{Type: "trd.quote", ID: c.QuoteID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c trdapi.SaveDraft) (trdapi.Receipt, error) {
			if t.TRD == nil {
				return trdapi.Receipt{}, errNoTarget
			}
			return t.TRD.SaveDraft(ctx, c)
		}))
	add(Define(Spec{Command: trdapi.CommandRevise, Context: "trd", Contract: trdapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskMedium, Approval: ApprovalNone,
		Summary: "Copy a frozen quote revision into a new working revision."},
		func(c trdapi.Revise) ObjectRef {
			return ObjectRef{Type: "trd.quote", ID: c.QuoteID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c trdapi.Revise) (trdapi.Receipt, error) {
			if t.TRD == nil {
				return trdapi.Receipt{}, errNoTarget
			}
			return t.TRD.Revise(ctx, c)
		}))
	add(Define(Spec{Command: trdapi.CommandUpdateMetadata, Context: "trd", Contract: trdapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskLow, Approval: ApprovalNone,
		Summary: "Update a quote's internal labels and note; never its business content."},
		func(c trdapi.UpdateMetadata) ObjectRef {
			return ObjectRef{Type: "trd.quote", ID: c.QuoteID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c trdapi.UpdateMetadata) (trdapi.Receipt, error) {
			if t.TRD == nil {
				return trdapi.Receipt{}, errNoTarget
			}
			return t.TRD.UpdateMetadata(ctx, c)
		}))
	add(Define(Spec{Command: trdapi.CommandRecordResponse, Context: "trd", Contract: trdapi.Contract, Version: 1,
		SideEffect: Draft, Risk: RiskMedium, Approval: ApprovalNone,
		Summary: "Record a customer's response to an active quote revision as received; a human verifies an acceptance."},
		func(c trdapi.RecordResponse) ObjectRef {
			return ObjectRef{Type: "trd.quote", ID: c.QuoteID, Version: c.BusinessRevision}
		},
		func(ctx context.Context, c trdapi.RecordResponse) (trdapi.ResponseReceipt, error) {
			if t.TRDAcceptance == nil {
				return trdapi.ResponseReceipt{}, errNoTarget
			}
			return t.TRDAcceptance.RecordResponse(ctx, c)
		}))
	add(Define(Spec{Command: trdapi.CommandPrepareOrder, Context: "trd", Contract: trdapi.Contract, Version: 1,
		SideEffect: Submit, Risk: RiskHigh, Approval: ApprovalGrant,
		Summary: "Prepare a draft sales order from a verified acceptance of an active quote revision; needs a human's approval grant for exactly this request."},
		func(c trdapi.PrepareOrder) ObjectRef {
			return ObjectRef{Type: "trd.quote", ID: c.QuoteID, Version: c.BusinessRevision}
		},
		func(ctx context.Context, c trdapi.PrepareOrder) (trdapi.OrderReceipt, error) {
			if t.TRDOrders == nil {
				return trdapi.OrderReceipt{}, errNoTarget
			}
			return t.TRDOrders.PrepareOrder(ctx, c)
		}))
	add(Define(Spec{Command: trdapi.CommandRequestOrderReview, Context: "trd", Contract: trdapi.Contract, Version: 1,
		SideEffect: Submit, Risk: RiskHigh, Approval: ApprovalGrant,
		Summary: "Open the review of a draft sales order; needs a human's approval grant for exactly this request."},
		func(c trdapi.RequestOrderReview) ObjectRef {
			return ObjectRef{Type: "trd.order", ID: c.OrderID, Version: c.ExpectedVersion}
		},
		func(ctx context.Context, c trdapi.RequestOrderReview) (trdapi.OrderReceipt, error) {
			if t.TRDOrders == nil {
				return trdapi.OrderReceipt{}, errNoTarget
			}
			return t.TRDOrders.RequestOrderReview(ctx, c)
		}))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return out, nil
}

// Q01Registry returns the registry of the Q01 capabilities.
func Q01Registry(t Q01Targets) (*Registry, error) {
	caps, err := Q01(t)
	if err != nil {
		return nil, err
	}
	return NewRegistry(caps...)
}
