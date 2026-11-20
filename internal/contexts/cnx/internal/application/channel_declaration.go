package application

import (
	"context"
	"errors"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/interop"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// SubmitDeclarationChannel files a confirmed declaration, or finishes a filing whose answer was
// lost (S6-D05, CN-AT13). A timeout is recorded as unknown. A second filing while the result is
// still unknown is EXTERNAL_RESULT_UNKNOWN. After a terminal result the same call returns that
// result and does not file again.
func (s Service) SubmitDeclarationChannel(ctx context.Context, cmd api.SubmitDeclarationChannel) (api.ChannelAnswer, error) {
	if err := cmd.Validate(); err != nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitDeclarationChannel, cmd.DeclarationID, err)
	}
	c, err := s.loadDeclaration(ctx, cmd.DeclarationID)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	if c == nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitDeclarationChannel, cmd.DeclarationID,
			kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", ""))
	}
	attempt, err := s.loadAttempt(ctx, interop.ChannelCustomsDeclaration, c.ID, api.CommandGetDeclaration)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	switch c.Status {
	case dcl.Confirmed:
		if err = dcl.CheckVersion(c, cmd.ExpectedVersion); err != nil {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitDeclarationChannel, c.ID, err)
		}
		attempt, err = s.openAttempt(ctx, cmd, interop.ChannelCustomsDeclaration, c.ID, dcl.Confirmed, c.RowVersion)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
		return s.fileDeclaration(ctx, cmd, c, attempt)
	case dcl.Submitted:
		if attempt == nil || attempt.State != string(interop.Planned) && attempt.State != string(interop.Submitted) {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitDeclarationChannel, c.ID,
				kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "Record this declaration's receipt by hand"))
		}
		return s.recoverDeclaration(ctx, cmd, c, attempt)
	case dcl.Unknown:
		return s.declarationUnknown(ctx, api.CommandSubmitDeclarationChannel, cmd.ExpectedVersion, cmd.IdempotencyKey, c, attempt, true)
	case dcl.Accepted, dcl.Released, dcl.Rejected, dcl.NotReceived:
		return s.finishDeclaration(ctx, cmd.IdempotencyKey, c, attempt)
	default:
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandSubmitDeclarationChannel, c.ID,
			kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "The declaration is "+c.Status))
	}
}

// ReconcileDeclarationChannel looks an unknown declaration up. NotFound, with the command's
// evidence and reason, is not_received. An authoritative accepted, released or rejected result is
// recorded. The attempt row is updated and not replaced.
func (s Service) ReconcileDeclarationChannel(ctx context.Context, cmd api.ReconcileDeclarationChannel) (api.ChannelAnswer, error) {
	if err := cmd.Validate(); err != nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, cmd.DeclarationID, err)
	}
	c, err := s.loadDeclaration(ctx, cmd.DeclarationID)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	if c == nil {
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, cmd.DeclarationID,
			kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", ""))
	}
	attempt, err := s.loadAttempt(ctx, interop.ChannelCustomsDeclaration, c.ID, api.CommandGetDeclaration)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	switch c.Status {
	case dcl.Unknown:
		if err = dcl.CheckVersion(c, cmd.ExpectedVersion); err != nil {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, c.ID, err)
		}
		if attempt == nil {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, c.ID,
				kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "The declaration has no channel attempt"))
		}
		return s.lookupDeclaration(ctx, cmd, c, attempt)
	case dcl.Accepted, dcl.Released, dcl.Rejected, dcl.NotReceived:
		return s.finishDeclaration(ctx, cmd.IdempotencyKey, c, attempt)
	default:
		return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, c.ID,
			kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "The declaration is "+c.Status+", not unknown"))
	}
}

func (s Service) fileDeclaration(ctx context.Context, cmd api.SubmitDeclarationChannel, c *dcl.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	res, err := s.port().Submit(ctx, interop.Submission{ID: interop.OperationID(attempt.OperationID), Channel: interop.ChannelCustomsDeclaration, Subject: c.ID})
	return s.afterDeclarationSend(ctx, api.CommandSubmitDeclarationChannel, cmd.IdempotencyKey, cmd.SubmitterName, cmd.EvidenceIDs, c, attempt, res, err, false)
}

func (s Service) recoverDeclaration(ctx context.Context, cmd api.SubmitDeclarationChannel, c *dcl.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	res, err := s.port().Lookup(ctx, interop.OperationID(attempt.OperationID))
	return s.afterDeclarationSend(ctx, api.CommandSubmitDeclarationChannel, cmd.IdempotencyKey, cmd.SubmitterName, cmd.EvidenceIDs, c, attempt, res, err, true)
}

func (s Service) lookupDeclaration(ctx context.Context, cmd api.ReconcileDeclarationChannel, c *dcl.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	res, err := s.port().Lookup(ctx, interop.OperationID(attempt.OperationID))
	if errors.Is(err, interop.ErrNotConfigured) {
		return api.ChannelAnswer{}, s.notConfigured(ctx, api.CommandReconcileDeclarationChannel, c.ID)
	}
	if errors.Is(err, interop.ErrUnavailable) {
		return api.ChannelAnswer{}, s.unavailable(ctx, api.CommandReconcileDeclarationChannel, c.ID,
			"The professional system could not be reached; the declaration stays unknown")
	}
	if errors.Is(err, interop.ErrNotFound) {
		if cmd.Reason == "" {
			return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, c.ID, kernel.Reject(kernel.CodeSchemaInvalid, "/args/reason", ""))
		}
		return s.writeDeclaration(ctx, cmd.IdempotencyKey, "", c, attempt, cmd.EvidenceIDs, api.DeclarationNotReceived, cmd.Reason, interop.Result{},
			string(interop.NotReceived), "")
	}
	if err == nil && customsAuthoritative(res, s.Clock.Now()) {
		state := string(interop.Succeeded)
		if res.Outcome == api.DeclarationRejected {
			state = string(interop.Failed)
		}
		return s.writeDeclaration(ctx, cmd.IdempotencyKey, "", c, attempt, cmd.EvidenceIDs, res.Outcome, "", res, state, res.Outcome)
	}
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	return api.ChannelAnswer{}, s.refuse(ctx, api.CommandReconcileDeclarationChannel, c.ID,
		kernel.Reject(kernel.CodeInvalidKnownValue, "/channel", "The professional answer cannot be recorded"))
}

// lostNotReceived is true when a lookup that finds nothing, during recovery of a submission that
// already committed, must be recorded as unknown rather than not_received.
func (s Service) afterDeclarationSend(ctx context.Context, command, key, submitter string, evidence []string, c *dcl.Case, attempt *postgres.ChannelAttempt, res interop.Result, err error, recovery bool) (api.ChannelAnswer, error) {
	if errors.Is(err, interop.ErrNotConfigured) {
		return api.ChannelAnswer{}, s.notConfigured(ctx, command, c.ID)
	}
	if errors.Is(err, interop.ErrUnavailable) {
		return api.ChannelAnswer{}, s.unavailable(ctx, command, c.ID, "The professional system could not be reached; nothing was recorded as a success")
	}
	if err == nil && customsAuthoritative(res, s.Clock.Now()) {
		state := string(interop.Succeeded)
		if res.Outcome == api.DeclarationRejected {
			state = string(interop.Failed)
		}
		return s.writeDeclaration(ctx, key, submitter, c, attempt, evidence, res.Outcome, "", res, state, res.Outcome)
	}
	if errors.Is(err, interop.ErrUnknown) || errors.Is(err, interop.ErrNotFound) || err == nil && !recovery {
		return s.writeDeclaration(ctx, key, submitter, c, attempt, evidence, api.DeclarationUnknown, channelTimeoutReason, interop.Result{},
			string(interop.Unknown), "")
	}
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID,
		kernel.Reject(kernel.CodeInvalidKnownValue, "/channel", "The professional answer cannot be recorded"))
}

func customsAuthoritative(res interop.Result, now time.Time) bool {
	now = kernel.Instant(now)
	at := kernel.Instant(res.At)
	exported := time.Time{}
	if !res.ExportedAt.IsZero() {
		exported = kernel.Instant(res.ExportedAt)
	}
	switch res.Outcome {
	case api.DeclarationAccepted:
		if res.State != interop.Succeeded || !exported.IsZero() {
			return false
		}
	case api.DeclarationRejected:
		if res.State != interop.Failed || !exported.IsZero() {
			return false
		}
	case api.DeclarationReleased:
		if res.State != interop.Succeeded || exported.IsZero() || exported.After(at) || exported.After(now) {
			return false
		}
	default:
		return false
	}
	if res.Sequence < 1 || at.IsZero() || at.After(now) {
		return false
	}
	return api.Text(res.StatusCode, api.MaxExternalStatus) && api.Text(res.OriginalText, api.MaxOriginalText) &&
		kernel.ValidIdentifier(res.MappingVersion) && api.Text(res.DeclarationNumber, api.MaxDeclarationRef)
}

func (s Service) writeDeclaration(ctx context.Context, key, submitter string, c *dcl.Case, attempt *postgres.ChannelAttempt, evidence []string, outcome, reason string, res interop.Result, settleState, settleOutcome string) (api.ChannelAnswer, error) {
	version := c.RowVersion
	var sub api.RecordSubmission
	if c.Status == dcl.Confirmed {
		sub = api.RecordSubmission{IdempotencyKey: phaseKey(key, "submission"), DeclarationID: c.ID, ExpectedVersion: version,
			OperationReference: attempt.OperationID, Submitter: api.Submitter{Kind: api.SubmitterOwnClerk, Name: submitter},
			SubmittedAt: kernel.FormatTimestamp(kernel.Instant(s.Clock.Now())), EvidenceIDs: evidence}
		if err := sub.Validate(); err != nil {
			return api.ChannelAnswer{}, err
		}
	}
	rec := customsReceipt(key, version, c.ID, attempt.OperationID, evidence, outcome, reason, res)
	if err := rec.Validate(); err != nil {
		return api.ChannelAnswer{}, err
	}
	if c.Status == dcl.Confirmed {
		got, err := s.RecordSubmission(ctx, sub)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
		version = got.RowVersion
		rec.ExpectedVersion = version
	}
	got, err := s.RecordReceipt(ctx, rec)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	if err = s.settle(ctx, key, interop.ChannelCustomsDeclaration, c.ID, settleState, settleOutcome, reason, true); err != nil {
		return api.ChannelAnswer{}, err
	}
	a, err := s.loadAttempt(ctx, interop.ChannelCustomsDeclaration, c.ID, api.CommandGetDeclaration)
	if err != nil {
		return api.ChannelAnswer{}, err
	}
	return answered(got.Receipt, a), nil
}

func customsReceipt(key string, version int64, id, operation string, evidence []string, outcome, reason string, res interop.Result) api.RecordReceipt {
	seq := res.Sequence
	if outcome == api.DeclarationUnknown || outcome == api.DeclarationNotReceived {
		seq = 0
	}
	rec := api.RecordReceipt{IdempotencyKey: phaseKey(key, "receipt-"+outcome), DeclarationID: id, ExpectedVersion: version,
		ReceiptID: receiptKey(operation, outcome, seq), Outcome: outcome, EvidenceIDs: evidence, Reason: reason}
	if outcome == api.DeclarationUnknown || outcome == api.DeclarationNotReceived {
		return rec
	}
	rec.External = &api.ExternalReceipt{Sequence: res.Sequence, StatusCode: res.StatusCode, OriginalText: res.OriginalText,
		ReceivedAt: kernel.FormatTimestamp(res.At)}
	rec.MappingVersion, rec.DeclarationNumber = res.MappingVersion, res.DeclarationNumber
	if outcome == api.DeclarationReleased {
		rec.ExportedAt = kernel.FormatTimestamp(res.ExportedAt)
	}
	return rec
}

func (s Service) declarationUnknown(ctx context.Context, command string, version int64, key string, c *dcl.Case, attempt *postgres.ChannelAttempt, refuseAgain bool) (api.ChannelAnswer, error) {
	if err := dcl.CheckVersion(c, version); err != nil {
		return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID, err)
	}
	if attempt != nil && attempt.State == string(interop.Unknown) && refuseAgain {
		return api.ChannelAnswer{}, s.refuse(ctx, command, c.ID, dcl.CheckSubmission(c))
	}
	if attempt != nil && !attemptTerminal(attempt.State) {
		if err := s.settle(ctx, key, interop.ChannelCustomsDeclaration, c.ID, string(interop.Unknown), "", channelTimeoutReason, true); err != nil {
			return api.ChannelAnswer{}, err
		}
		var err error
		attempt, err = s.loadAttempt(ctx, interop.ChannelCustomsDeclaration, c.ID, api.CommandGetDeclaration)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
	}
	return answered(api.Receipt{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, attempt), nil
}

func (s Service) finishDeclaration(ctx context.Context, key string, c *dcl.Case, attempt *postgres.ChannelAttempt) (api.ChannelAnswer, error) {
	if state, outcome, ok := connectorDeclaration(c.Status); ok && attempt != nil && attempt.State != state && !attemptTerminal(attempt.State) {
		if err := s.settle(ctx, key, interop.ChannelCustomsDeclaration, c.ID, state, outcome, "", true); err != nil {
			return api.ChannelAnswer{}, err
		}
		var err error
		attempt, err = s.loadAttempt(ctx, interop.ChannelCustomsDeclaration, c.ID, api.CommandGetDeclaration)
		if err != nil {
			return api.ChannelAnswer{}, err
		}
	}
	return answered(api.Receipt{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status}, attempt), nil
}

func connectorDeclaration(status string) (state, outcome string, ok bool) {
	switch status {
	case dcl.Accepted, dcl.Released:
		return string(interop.Succeeded), status, true
	case dcl.Rejected:
		return string(interop.Failed), status, true
	case dcl.NotReceived:
		return string(interop.NotReceived), "", true
	default:
		return "", "", false
	}
}

// IngestDeclarationReceipts keeps every external receipt of a submitted declaration. One that
// CheckReceipt refuses is stored on the chain with its refusal and does not advance the case.
// An exact repeat is not stored twice. The command succeeds when the chain is kept.
func (s Service) IngestDeclarationReceipts(ctx context.Context, cmd api.IngestDeclarationReceipts) (api.ChannelIngest, error) {
	if err := cmd.Validate(); err != nil {
		return api.ChannelIngest{}, s.refuse(ctx, api.CommandIngestDeclarationReceipts, cmd.DeclarationID, err)
	}
	res, err := s.execute(ctx, cmd.DeclarationID, cmd, &ingestReceipts{s: s, cmd: cmd})
	if err != nil {
		return api.ChannelIngest{}, err
	}
	c, err := s.loadDeclaration(ctx, cmd.DeclarationID)
	if err != nil {
		return api.ChannelIngest{}, err
	}
	if c == nil {
		return api.ChannelIngest{}, kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", "")
	}
	rows, err := s.loadChain(ctx, interop.ChannelCustomsDeclaration, cmd.DeclarationID, api.CommandGetDeclaration)
	if err != nil {
		return api.ChannelIngest{}, err
	}
	out := api.ChannelIngest{Receipt: api.Receipt(res), DeclarationID: c.ID, LastSequence: c.LastSequence, Chain: []api.ChannelChainEntry{}}
	out.Status, out.RowVersion = c.Status, c.RowVersion
	for _, r := range rows {
		out.Chain = append(out.Chain, chainEntry(r))
	}
	return out, nil
}

type ingestReceipts struct {
	s           Service
	cmd         api.IngestDeclarationReceipts
	declaration *dcl.Case
}

func (h *ingestReceipts) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.declaration, err = postgres.LoadDeclaration(ctx, exec, scope(ctx), h.cmd.DeclarationID, postgres.ForUpdate)
	return err
}

func (h *ingestReceipts) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc, c := scope(ctx), h.declaration
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/declaration_id", "")
	}
	if err := dcl.CheckVersion(c, h.cmd.ExpectedVersion); err != nil {
		return action.Outcome{}, err
	}
	if c.Status == dcl.Draft || c.Status == dcl.Confirmed {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/declaration_id", "Nothing was submitted")
	}
	vs, err := postgres.Versions(ctx, exec, sc, c.ID, c.Version)
	if err != nil {
		return action.Outcome{}, err
	}
	if len(vs) != 1 {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/declaration_id", "The declaration has no current version")
	}
	op := ""
	subs, err := postgres.Submissions(ctx, exec, sc, c.ID)
	if err != nil {
		return action.Outcome{}, err
	}
	if n := len(subs); n > 0 && kernel.ValidIdentifier(subs[n-1].OperationReference) {
		op = subs[n-1].OperationReference
	}
	now := kernel.Instant(h.s.Clock.Now())
	var events []action.Event
	for _, in := range h.cmd.Receipts {
		id := chainID(c.ID, in.Outcome, in.StatusCode, in.OriginalText, in.Sequence)
		existing, err := postgres.LoadChannelReceipt(ctx, exec, sc, id)
		if err != nil {
			return action.Outcome{}, err
		}
		if existing != nil {
			continue
		}
		n, err := postgres.CountChannelReceipts(ctx, exec, sc, interop.ChannelCustomsDeclaration, c.ID)
		if err != nil {
			return action.Outcome{}, err
		}
		received, _ := kernel.ParseTimestamp(in.ReceivedAt)
		var exported time.Time
		if in.ExportedAt != "" {
			exported, _ = kernel.ParseTimestamp(in.ExportedAt)
		}
		input := dcl.ReceiptInput{Outcome: in.Outcome, Sequence: in.Sequence, DeclarationNumber: in.DeclarationNumber,
			ExportedAt: kernel.Instant(exported), ReceivedAt: kernel.Instant(received), AmendedItems: map[int]bool{}, At: now}
		checkErr := dcl.CheckReceipt(c, dcl.Items(vs[0].Document), input)
		if checkErr == nil && !input.ReceivedAt.IsZero() && input.ReceivedAt.After(now) {
			checkErr = kernel.Reject(kernel.CodeInvalidKnownValue, "/args/receipts/received_at", "The receipt is in the future")
		}
		row := &postgres.ChannelReceipt{ID: id, Channel: interop.ChannelCustomsDeclaration, SubjectID: c.ID, OperationID: op,
			IngressNo: n + 1, Sequence: in.Sequence, Outcome: in.Outcome, StatusCode: in.StatusCode, OriginalText: in.OriginalText,
			MappingVersion: in.MappingVersion, RecordedAt: now, Actor: d.Actor, Epoch: d.Epoch}
		if checkErr != nil {
			rej, ok := kernel.AsRejection(checkErr)
			if !ok {
				return action.Outcome{}, checkErr
			}
			row.RefusalCode = rej.Code
			if err = postgres.InsertChannelReceipt(ctx, exec, sc, row); err != nil {
				return action.Outcome{}, err
			}
			continue
		}
		from := c.RowVersion
		c.RowVersion++
		r := &dcl.Receipt{ID: id, DeclarationID: c.ID, No: c.Receipts + 1, Outcome: in.Outcome, From: c.Status,
			MappingVersion: in.MappingVersion, DeclarationNumber: in.DeclarationNumber, Sequence: in.Sequence, StatusCode: in.StatusCode,
			OriginalText: in.OriginalText, ReceivedAt: input.ReceivedAt, ExportedAt: input.ExportedAt, By: d.Actor, Epoch: d.Epoch, At: now}
		c.Apply(input)
		if err = postgres.InsertReceipt(ctx, exec, sc, r); err != nil {
			return action.Outcome{}, err
		}
		if err = postgres.SaveDeclaration(ctx, exec, sc, c, from, now); err != nil {
			return action.Outcome{}, err
		}
		row.Authoritative = true
		if err = postgres.InsertChannelReceipt(ctx, exec, sc, row); err != nil {
			return action.Outcome{}, err
		}
		fields := map[string]any{"declaration_id": c.ID, "export_lot_id": c.ExportLotID, "attempt": c.Attempt, "shipment_id": c.ShipmentID,
			"order_id": c.OrderID, "order_revision": c.OrderRevision, "receipt_id": r.ID, "receipt_no": r.No, "outcome": r.Outcome, "from": r.From,
			"version": c.Version}
		if r.Sequence > 0 {
			fields["external_sequence"], fields["mapping_version"], fields["declaration_number"] = r.Sequence, r.MappingVersion, c.DeclarationNumber
		}
		if !c.ExportedAt.IsZero() {
			fields["exported_at"] = kernel.FormatTimestamp(c.ExportedAt)
		}
		events = append(events, action.Event{Type: api.EventDeclarationReceiptRecorded, AggregateType: "DeclarationCase", AggregateID: c.ID,
			AggregateVersion: c.RowVersion, Fields: fields})
	}
	return action.Outcome{ObjectID: c.ID, RowVersion: c.RowVersion, Status: c.Status, Events: events}, nil
}
