package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/adapters/postgres"
	dcl "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/declaration"
	tr "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/taxrefund"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/interop"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// channelTimeoutReason is recorded when the connector lost the answer. It is not a professional result.
const channelTimeoutReason = "The channel timed out before the professional system answered"

func (s Service) port() interop.Port {
	if s.Interop == nil {
		return interop.Unconnected{Name: "professional"}
	}
	return s.Interop
}

func phaseKey(key, phase string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x1f%s", phase, key))
	return "ch-" + hex.EncodeToString(sum[:20])
}

func receiptKey(operation, outcome string, sequence int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "receipt\x1f%s\x1f%s\x1f%d", operation, outcome, sequence))
	return "rc-" + hex.EncodeToString(sum[:20])
}

func chainID(subject, outcome, status, text string, sequence int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x1f%d\x1f%s\x1f%s\x1f%s", subject, sequence, outcome, status, text))
	return "cr-" + hex.EncodeToString(sum[:20])
}

func (s Service) refuse(ctx context.Context, command, objectID string, err error) error {
	if rej, ok := kernel.AsRejection(err); ok {
		if auditErr := s.Ledger.AuditRejected(ctx, command, objectID, rej.Code); auditErr != nil {
			return errors.Join(err, auditErr)
		}
	}
	return err
}

func (s Service) notConfigured(ctx context.Context, command, objectID string) error {
	return s.refuse(ctx, command, objectID, errors.Join(interop.ErrNotConfigured,
		kernel.RejectAs("EXTERNAL_CHANNEL_NOT_CONFIGURED", kernel.CategoryInvalid, "/channel",
			"No professional system is configured; nothing was submitted")))
}

func (s Service) unavailable(ctx context.Context, command, objectID, reason string) error {
	return s.refuse(ctx, command, objectID, errors.Join(interop.ErrUnavailable,
		kernel.Reject(kernel.CodeInvalidState, "/channel", reason)))
}

func (s Service) execute(ctx context.Context, objectID string, req api.Command, h action.Handler) (action.Result, error) {
	name, key, args := request(req)
	if name == "" {
		return action.Result{}, kernel.Reject(kernel.CodeSchemaInvalid, "/command", "")
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return action.Result{}, err
	}
	return s.Ledger.Execute(ctx, action.Command{Name: name, IdempotencyKey: key, RequestHash: hash, ObjectID: objectID}, h)
}

// channelRequest is request's part for the connector commands.
func channelRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SubmitDeclarationChannel:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSubmitDeclarationChannel, key, c
	case api.ReconcileDeclarationChannel:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandReconcileDeclarationChannel, key, c
	case api.IngestDeclarationReceipts:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandIngestDeclarationReceipts, key, c
	case api.SubmitTaxChannel:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSubmitTaxChannel, key, c
	case api.ReconcileTaxChannel:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandReconcileTaxChannel, key, c
	case api.SettleDeclarationChannel:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandSettleDeclarationChannel, key, c
	case api.SettleTaxChannel:
		key, c.IdempotencyKey = c.IdempotencyKey, ""
		return api.CommandSettleTaxChannel, key, c
	}
	return "", "", nil
}

func (s Service) loadDeclaration(ctx context.Context, id string) (*dcl.Case, error) {
	var c *dcl.Case
	err := s.Ledger.Lookup(ctx, api.CommandGetDeclaration, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		c, err = postgres.LoadDeclaration(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err
	})
	return c, err
}

func (s Service) loadTaxCase(ctx context.Context, id string) (*tr.Case, error) {
	var c *tr.Case
	err := s.Ledger.Lookup(ctx, api.CommandGetTaxCase, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		c, err = postgres.LoadCase(ctx, exec, scope(ctx), id, postgres.NoLock)
		return err
	})
	return c, err
}

func (s Service) loadAttempt(ctx context.Context, channel, subject, read string) (*postgres.ChannelAttempt, error) {
	var a *postgres.ChannelAttempt
	err := s.Ledger.Lookup(ctx, read, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		a, err = postgres.LoadChannelAttempt(ctx, exec, scope(ctx), channel, subject, postgres.NoLock)
		return err
	})
	return a, err
}

func (s Service) loadChain(ctx context.Context, channel, subject, read string) ([]*postgres.ChannelReceipt, error) {
	var rows []*postgres.ChannelReceipt
	err := s.Ledger.Lookup(ctx, read, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var err error
		rows, err = postgres.ChannelReceipts(ctx, exec, scope(ctx), channel, subject)
		return err
	})
	return rows, err
}

func answered(rec api.Receipt, a *postgres.ChannelAttempt) api.ChannelAnswer {
	out := api.ChannelAnswer{Receipt: rec}
	if a != nil {
		out.OperationID, out.ConnectorState, out.AttemptNo, out.Outcome = a.OperationID, a.State, a.AttemptNo, a.Outcome
	}
	return out
}

func chainEntry(r *postgres.ChannelReceipt) api.ChannelChainEntry {
	return api.ChannelChainEntry{ReceiptID: r.ID, IngressNo: r.IngressNo, Sequence: r.Sequence, Outcome: r.Outcome,
		Authoritative: r.Authoritative, RefusalCode: r.RefusalCode, StatusCode: r.StatusCode, OriginalText: r.OriginalText,
		MappingVersion: r.MappingVersion}
}

// ChannelLog returns the connector attempt and the ingested chain of subject on channel.
func (s Service) ChannelLog(ctx context.Context, channel, subjectID string) (api.ChannelLog, error) {
	if channel != interop.ChannelCustomsDeclaration && channel != interop.ChannelTaxRefund {
		return api.ChannelLog{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/channel", "")
	}
	read := api.CommandGetDeclaration
	if channel == interop.ChannelTaxRefund {
		read = api.CommandGetTaxCase
	}
	a, err := s.loadAttempt(ctx, channel, subjectID, read)
	if err != nil {
		return api.ChannelLog{}, err
	}
	rows, err := s.loadChain(ctx, channel, subjectID, read)
	if err != nil {
		return api.ChannelLog{}, err
	}
	out := api.ChannelLog{Chain: []api.ChannelChainEntry{}}
	if a != nil {
		out.Attempt = &api.ChannelAttemptView{AttemptID: a.ID, Channel: a.Channel, SubjectID: a.SubjectID, AttemptNo: a.AttemptNo,
			OperationID: a.OperationID, State: a.State, Outcome: a.Outcome, Reason: a.Reason, RowVersion: a.RowVersion}
	}
	for _, r := range rows {
		out.Chain = append(out.Chain, chainEntry(r))
	}
	return out, nil
}

func (s Service) openAttempt(ctx context.Context, req api.Command, channel, subject, expect string, version int64) (*postgres.ChannelAttempt, error) {
	if _, err := s.execute(ctx, subject, req, &openAttempt{s: s, channel: channel, subject: subject, expectedVersion: version, expectStatus: expect}); err != nil {
		return nil, err
	}
	read := api.CommandGetDeclaration
	if channel == interop.ChannelTaxRefund {
		read = api.CommandGetTaxCase
	}
	return s.loadAttempt(ctx, channel, subject, read)
}

func (s Service) settle(ctx context.Context, key, channel, subject, state, outcome, reason string, declaration bool) error {
	id := phaseKey(key, "settle-"+state)
	var req api.Command
	if declaration {
		req = api.SettleDeclarationChannel{IdempotencyKey: id, Channel: channel, SubjectID: subject, State: state, Outcome: outcome, Reason: reason}
	} else {
		req = api.SettleTaxChannel{IdempotencyKey: id, Channel: channel, SubjectID: subject, State: state, Outcome: outcome, Reason: reason}
	}
	_, err := s.execute(ctx, subject, req, &moveAttempt{s: s, channel: channel, subject: subject, state: state, outcome: outcome, reason: reason})
	return err
}

func attemptTerminal(state string) bool {
	return state == string(interop.Succeeded) || state == string(interop.Failed) || state == string(interop.NotReceived)
}

// openAttempt records a planned attempt for a confirmed declaration or a planned tax case.
type openAttempt struct {
	s               Service
	channel         string
	subject         string
	expectedVersion int64
	expectStatus    string
	declaration     *dcl.Case
	taxCase         *tr.Case
	attempt         *postgres.ChannelAttempt
}

func (h *openAttempt) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.declaration, h.taxCase, h.attempt = nil, nil, nil
	sc := scope(ctx)
	var err error
	switch h.channel {
	case interop.ChannelCustomsDeclaration:
		h.declaration, err = postgres.LoadDeclaration(ctx, exec, sc, h.subject, postgres.ForUpdate)
	case interop.ChannelTaxRefund:
		h.taxCase, err = postgres.LoadCase(ctx, exec, sc, h.subject, postgres.ForUpdate)
	default:
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/channel", "")
	}
	if err != nil {
		return err
	}
	h.attempt, err = postgres.LoadChannelAttempt(ctx, exec, sc, h.channel, h.subject, postgres.ForUpdate)
	return err
}

func (h *openAttempt) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	status, version, ok := h.root()
	path := "/args/declaration_id"
	if h.channel == interop.ChannelTaxRefund {
		path = "/args/case_id"
	}
	if !ok {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, path, "")
	}
	if version != h.expectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	if status != h.expectStatus {
		if h.channel == interop.ChannelTaxRefund && status == tr.Expected {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, path, "Plan the tax refund before the channel files it")
		}
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, path, "The case is "+status+", not "+h.expectStatus)
	}
	if h.attempt != nil {
		if h.attempt.State != string(interop.Planned) {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, path, "The channel attempt is "+h.attempt.State)
		}
		return action.Outcome{ObjectID: h.subject, RowVersion: h.attempt.RowVersion, Status: h.attempt.State}, nil
	}
	now := kernel.Instant(h.s.Clock.Now())
	h.attempt = &postgres.ChannelAttempt{ID: action.NewID("cha", now), Channel: h.channel, SubjectID: h.subject, AttemptNo: 1,
		OperationID: string(interop.OperationIDFor(scope(ctx), h.channel, h.subject, 1)), State: string(interop.Planned),
		RowVersion: 1, Actor: d.Actor, Epoch: d.Epoch, CreatedAt: now, UpdatedAt: now}
	if err := postgres.InsertChannelAttempt(ctx, exec, scope(ctx), h.attempt); err != nil {
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.subject, RowVersion: h.attempt.RowVersion, Status: h.attempt.State}, nil
}

func (h *openAttempt) root() (status string, version int64, ok bool) {
	if h.channel == interop.ChannelCustomsDeclaration {
		if h.declaration == nil {
			return "", 0, false
		}
		return h.declaration.Status, h.declaration.RowVersion, true
	}
	if h.taxCase == nil {
		return "", 0, false
	}
	return h.taxCase.Status, h.taxCase.RowVersion, true
}

// moveAttempt moves the attempt along the connector. A repeat of the same state does nothing, so
// the row is not overwritten.
type moveAttempt struct {
	s           Service
	channel     string
	subject     string
	state       string
	outcome     string
	reason      string
	declaration *dcl.Case
	taxCase     *tr.Case
	attempt     *postgres.ChannelAttempt
}

func (h *moveAttempt) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.declaration, h.taxCase, h.attempt = nil, nil, nil
	sc := scope(ctx)
	var err error
	switch h.channel {
	case interop.ChannelCustomsDeclaration:
		h.declaration, err = postgres.LoadDeclaration(ctx, exec, sc, h.subject, postgres.ForUpdate)
	case interop.ChannelTaxRefund:
		h.taxCase, err = postgres.LoadCase(ctx, exec, sc, h.subject, postgres.ForUpdate)
	default:
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/channel", "")
	}
	if err != nil {
		return err
	}
	h.attempt, err = postgres.LoadChannelAttempt(ctx, exec, sc, h.channel, h.subject, postgres.ForUpdate)
	return err
}

func (h *moveAttempt) Apply(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) (action.Outcome, error) {
	path := "/args/declaration_id"
	if h.channel == interop.ChannelTaxRefund {
		path = "/args/case_id"
	}
	if h.channel == interop.ChannelCustomsDeclaration && h.declaration == nil || h.channel == interop.ChannelTaxRefund && h.taxCase == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, path, "")
	}
	a := h.attempt
	if a == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, path, "The channel attempt is not open")
	}
	if a.State == h.state && (h.outcome == "" || a.Outcome == h.outcome) {
		return action.Outcome{ObjectID: h.subject, RowVersion: a.RowVersion, Status: a.State}, nil
	}
	if attemptTerminal(a.State) {
		return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, path, "The channel attempt is "+a.State)
	}
	from := a.RowVersion
	a.State, a.Outcome, a.Reason = h.state, h.outcome, h.reason
	a.RowVersion++
	now := kernel.Instant(h.s.Clock.Now())
	if err := postgres.SaveChannelAttempt(ctx, exec, scope(ctx), a, from, now); err != nil {
		if errors.Is(err, postgres.ErrConflict) {
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		return action.Outcome{}, err
	}
	return action.Outcome{ObjectID: h.subject, RowVersion: a.RowVersion, Status: a.State}, nil
}
