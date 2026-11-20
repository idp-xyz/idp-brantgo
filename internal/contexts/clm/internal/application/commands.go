package application

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/api"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/clm/internal/domain/claim"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

func lineRefusal(line ShipmentLine) error {
	switch line.Code {
	case "":
		return nil
	case kernel.CodeInvalidState:
		return kernel.Reject(kernel.CodeInvalidState, "/args/shipment_id", "The shipment is "+line.Status+", not confirmed")
	default:
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_line_id", "The shipment line has no price")
	}
}

func done(c *claim.Case, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: c.ID, RowVersion: c.Version, Status: c.Status, Events: events}
}

type openCommand struct {
	s        Service
	cmd      api.OpenClaim
	line     ShipmentLine
	units    int
	existing *claim.Case
	replay   bool
}

func (h *openCommand) Prepare(ctx context.Context) error {
	var taken bool
	err := h.s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		c, err := postgres.LoadCase(ctx, exec, scopeOf(ctx), h.cmd.ClaimID, postgres.NoLock)
		taken = c != nil
		return err
	})
	if err != nil || taken {
		return err
	}
	line, ok, err := h.s.Upstream.Lines.ResolveLine(ctx, h.cmd.ShipmentID, h.cmd.ShipmentLineID)
	if err != nil {
		return err
	}
	if !ok {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_line_id", "The shipment line is not known")
	}
	if err = lineRefusal(line); err != nil {
		return err
	}
	if line.OrderID != h.cmd.OrderID {
		return kernel.Reject(kernel.CodeReferenceInvalid, "/args/order_id", "The shipment line belongs to order "+line.OrderID)
	}
	h.units, err = h.s.Upstream.Currency.MinorUnits(ctx, line.CurrencyCode, line.CurrencyRevision, h.s.Clock.Now())
	if err != nil {
		return err
	}
	if _, err = h.s.evidence(ctx, h.cmd.EvidenceIDs); err != nil {
		return err
	}
	h.line = line
	return nil
}

func (h *openCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.replay = nil, false
	var err error
	h.existing, err = postgres.LoadCase(ctx, exec, scopeOf(ctx), h.cmd.ClaimID, postgres.ForUpdate)
	return err
}

func (h *openCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if c := h.existing; c != nil {
		if err := seen(d, c.OwnerActor, c.OwnerTeam); err != nil {
			return action.Outcome{}, err
		}
		if err := c.Answer(requestHash(h.cmd), d.Actor); err != nil {
			return action.Outcome{}, err
		}
		h.replay = true
		return done(c), nil
	}
	q, err := kernel.ParseDecimal(h.line.Quantity, kernel.QuantityFormat)
	if err != nil || q.Sign() <= 0 {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_line_id", "The shipment line has no quantity")
	}
	cur, err := kernel.NewCurrency(h.line.CurrencyCode, h.units)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/shipment_line_id", "The shipment currency is not usable")
	}
	now := kernel.Instant(h.s.Clock.Now())
	c := &claim.Case{ID: h.cmd.ClaimID, Status: claim.StatusOpen, Version: 1, ShipmentID: h.cmd.ShipmentID, LineID: h.cmd.ShipmentLineID,
		ShipmentVersion: h.line.Version, OrderID: h.line.OrderID, OrderRevision: h.line.OrderRevision, OrderHash: h.line.OrderHash,
		CustomerID: h.line.CustomerID, OrderLineID: h.line.OrderLineID, SkuID: h.line.SkuID, SpecRevision: h.line.SpecRevision,
		UnitID: h.line.UnitID, UnitRevision: h.line.UnitRevision, Shipped: q, Currency: cur, CurrencyRevision: h.line.CurrencyRevision,
		Reason: h.cmd.Reason, Evidence: append([]string(nil), h.cmd.EvidenceIDs...), RequestHash: requestHash(h.cmd),
		OwnerActor: d.Actor.ID, OwnerTeam: d.Team, Opened: claim.Act{By: d.Actor, Epoch: d.Epoch, At: now, Team: d.Team}}
	if err = postgres.InsertCase(ctx, exec, scopeOf(ctx), c); err != nil {
		return action.Outcome{}, err
	}
	ev := action.Event{Type: api.EventClaimOpened, AggregateType: "ClaimCase", AggregateID: c.ID, AggregateVersion: c.Version,
		Fields: map[string]any{"claim_id": c.ID, "shipment_id": c.ShipmentID, "shipment_line_id": c.LineID, "order_id": c.OrderID,
			"customer_party_id": c.CustomerID}}
	return done(c, ev), nil
}

func (s Service) OpenClaim(ctx context.Context, cmd api.OpenClaim) (api.Receipt, error) {
	h := &openCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandOpenClaim, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ClaimID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type returnCommand struct {
	s        Service
	cmd      api.RegisterReturn
	existing *claim.Case
	replay   bool
}

func (h *returnCommand) Prepare(ctx context.Context) error {
	_, err := h.s.evidence(ctx, h.cmd.EvidenceIDs)
	return err
}

func (h *returnCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.replay = nil, false
	var err error
	h.existing, err = postgres.LoadCase(ctx, exec, scopeOf(ctx), h.cmd.ClaimID, postgres.ForUpdate)
	return err
}

func (h *returnCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.existing
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/claim_id", "")
	}
	if err := seen(d, c.OwnerActor, c.OwnerTeam); err != nil {
		return action.Outcome{}, err
	}
	sc := scopeOf(ctx)
	if err := postgres.LockLine(ctx, exec, sc, c.ShipmentID, c.LineID); err != nil {
		return action.Outcome{}, err
	}
	counted, err := postgres.CountedReturns(ctx, exec, sc, c.ShipmentID, c.LineID)
	if err != nil {
		return action.Outcome{}, err
	}
	qty, err := kernel.ParseDecimal(h.cmd.Quantity, kernel.QuantityFormat)
	if err != nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/quantity", "")
	}
	from := c.Version
	now := kernel.Instant(h.s.Clock.Now())
	replay, err := c.Register(h.cmd.ReturnID, qty, h.cmd.Condition, h.cmd.Note, requestHash(h.cmd), h.cmd.EvidenceIDs,
		claim.Act{By: d.Actor, Epoch: d.Epoch, At: now, Team: d.Team}, counted)
	if err != nil {
		return action.Outcome{}, err
	}
	if replay {
		h.replay = true
		return done(c), nil
	}
	ret := c.Returns[len(c.Returns)-1]
	if err = postgres.InsertReturn(ctx, exec, sc, c.ID, ret); err != nil {
		return action.Outcome{}, err
	}
	if err = postgres.Advance(ctx, exec, sc, c, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	ev := action.Event{Type: api.EventReturnRegistered, AggregateType: "ClaimCase", AggregateID: c.ID, AggregateVersion: c.Version,
		Fields: map[string]any{"claim_id": c.ID, "return_id": ret.ID, "shipment_id": c.ShipmentID, "shipment_line_id": c.LineID,
			"quantity": postgres.Text(ret.Quantity), "unit_id": c.UnitID}}
	return done(c, ev), nil
}

func (s Service) RegisterReturn(ctx context.Context, cmd api.RegisterReturn) (api.Receipt, error) {
	h := &returnCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandRegisterReturn, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ClaimID},
		cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type remedyCommand struct {
	s        Service
	cmd      api.DecideRemedy
	existing *claim.Case
	replay   bool
}

func (h *remedyCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.existing, h.replay = nil, false
	var err error
	h.existing, err = postgres.LoadCase(ctx, exec, scopeOf(ctx), h.cmd.ClaimID, postgres.ForUpdate)
	return err
}

func (h *remedyCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.existing
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/claim_id", "")
	}
	if err := seen(d, c.OwnerActor, c.OwnerTeam); err != nil {
		return action.Outcome{}, err
	}
	hash := requestHash(h.cmd)
	if c.Version != h.cmd.ExpectedVersion {
		if c.Remedy != nil && c.Remedy.ID == h.cmd.RemedyID && c.Remedy.RequestHash == hash && c.Remedy.By.By == d.Actor {
			h.replay = true
			return done(c), nil
		}
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	sc := scopeOf(ctx)
	if err := postgres.LockLine(ctx, exec, sc, c.ShipmentID, c.LineID); err != nil {
		return action.Outcome{}, err
	}
	amount, err := amountOf(c, h.cmd.RemedyKind, h.cmd.AmountMinor)
	if err != nil {
		return action.Outcome{}, err
	}
	from := c.Version
	now := kernel.Instant(h.s.Clock.Now())
	replay, err := c.Decide(h.cmd.RemedyID, h.cmd.RemedyKind, amount, h.cmd.Reason, hash,
		claim.Act{By: d.Actor, Epoch: d.Epoch, At: now, Team: d.Team})
	if err != nil {
		return action.Outcome{}, err
	}
	if replay {
		h.replay = true
		return done(c), nil
	}
	if err = postgres.InsertRemedy(ctx, exec, sc, c.ID, c.Remedy); err != nil {
		if postgres.RemedyConflict(err) {
			return action.Outcome{}, api.Reject(api.CodeBusinessKeyConflict, "/args/remedy_id", "The remedy ID holds another remedy")
		}
		return action.Outcome{}, err
	}
	if err = postgres.Advance(ctx, exec, sc, c, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	ev := action.Event{Type: api.EventRemedyDecided, AggregateType: "ClaimCase", AggregateID: c.ID, AggregateVersion: c.Version,
		Fields: map[string]any{"claim_id": c.ID, "remedy_id": c.Remedy.ID, "kind": c.Remedy.Kind,
			"amount_minor": c.Remedy.Amount.MinorString(), "currency": c.Remedy.Amount.Currency().Code(), "order_id": c.OrderID}}
	return done(c, ev), nil
}

func amountOf(c *claim.Case, kind, minor string) (kernel.Money, error) {
	if minor == "" {
		minor = "0"
	}
	amount, err := kernel.ParseMinor(c.Currency, minor)
	if err != nil {
		return kernel.Money{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount_minor", "The amount is above zero")
	}
	if claim.MoneyKind(kind) && amount.Sign() <= 0 {
		return kernel.Money{}, kernel.Reject(kernel.CodeInvalidKnownValue, "/args/amount_minor", "The amount is above zero")
	}
	return amount, nil
}

func (s Service) DecideRemedy(ctx context.Context, cmd api.DecideRemedy) (api.Receipt, error) {
	h := &remedyCommand{s: s, cmd: cmd}
	res, err := s.run(ctx, action.Command{Name: api.CommandDecideRemedy, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ClaimID,
		Reason: cmd.Reason}, cmd, cmd.Validate(), h)
	if err != nil {
		return api.Receipt{}, err
	}
	res.Replayed = res.Replayed || h.replay
	return res, nil
}

type closeCommand struct {
	s        Service
	cmd      api.CloseClaim
	existing *claim.Case
}

func (h *closeCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.existing, err = postgres.LoadCase(ctx, exec, scopeOf(ctx), h.cmd.ClaimID, postgres.ForUpdate)
	return err
}

func (h *closeCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	c := h.existing
	if c == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/claim_id", "")
	}
	if err := seen(d, c.OwnerActor, c.OwnerTeam); err != nil {
		return action.Outcome{}, err
	}
	if c.Version != h.cmd.ExpectedVersion {
		return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	from := c.Version
	now := kernel.Instant(h.s.Clock.Now())
	if err := c.Close(h.cmd.Summary, requestHash(h.cmd), claim.Act{By: d.Actor, Epoch: d.Epoch, At: now, Team: d.Team}); err != nil {
		return action.Outcome{}, err
	}
	if err := postgres.Advance(ctx, exec, scopeOf(ctx), c, from); err != nil {
		return action.Outcome{}, conflict(err)
	}
	ev := action.Event{Type: api.EventClaimClosed, AggregateType: "ClaimCase", AggregateID: c.ID, AggregateVersion: c.Version,
		Fields: map[string]any{"claim_id": c.ID, "closed_at": kernel.FormatTimestamp(c.Closure.By.At)}}
	return done(c, ev), nil
}

func (s Service) CloseClaim(ctx context.Context, cmd api.CloseClaim) (api.Receipt, error) {
	h := &closeCommand{s: s, cmd: cmd}
	return s.run(ctx, action.Command{Name: api.CommandCloseClaim, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.ClaimID,
		Reason: cmd.Summary}, cmd, cmd.Validate(), h)
}
