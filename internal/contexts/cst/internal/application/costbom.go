package application

import (
	"context"
	"errors"
	"strconv"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/api"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cst/internal/domain/costbom"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// costBOMRules are the cost BOM commands. verify, withdraw and substitution.decide are human only.
// save_draft and substitution.request admit an AI actor at the gateway; HTTP still refuses one.
func costBOMRules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandSaveCostBOM, Permission: api.CommandSaveCostBOM, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandVerifyCostBOM, api.CommandVerifyCostBOM),
		authorization.HumanOnly(api.CommandWithdrawCostBOM, api.CommandWithdrawCostBOM),
		{Command: api.CommandRequestSubstitution, Permission: api.CommandRequestSubstitution, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandDecideSubstitution, api.CommandDecideSubstitution),
	}
}

func costBOMCommand(command string) bool {
	switch command {
	case api.CommandSaveCostBOM, api.CommandVerifyCostBOM, api.CommandWithdrawCostBOM, api.CommandRequestSubstitution, api.CommandDecideSubstitution:
		return true
	default:
		return false
	}
}

func costBOMRequest(cmd api.Command) (name, key string, args any) {
	switch c := cmd.(type) {
	case api.SaveCostBOM:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandSaveCostBOM, key, c
	case api.VerifyCostBOM:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandVerifyCostBOM, key, c
	case api.WithdrawCostBOM:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandWithdrawCostBOM, key, c
	case api.RequestSubstitution:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandRequestSubstitution, key, c
	case api.DecideSubstitution:
		key, c.IdempotencyKey, c.ExpectedVersion = c.IdempotencyKey, "", 0
		return api.CommandDecideSubstitution, key, c
	}
	return "", "", nil
}

type bomCommand struct {
	s    Service
	id   string
	bom  *costbom.BOM
	from int64
	run  func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *bomCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.bom, h.from = nil, 0
	h.bom, err = postgres.LoadBOM(ctx, exec, scope(ctx), h.id, postgres.ForUpdate)
	if h.bom != nil {
		h.from = h.bom.RootVersion
	}
	return err
}

func (h *bomCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.run(ctx, exec, d)
}

func (h *bomCommand) visible(d authorization.Decision) bool {
	return h.bom != nil && d.Sees(h.bom.OwnerActor, h.bom.OwnerTeam)
}

func (h *bomCommand) save(ctx context.Context, exec bentopg.Executor, created bool, status string, events ...action.Event) (action.Outcome, error) {
	if err := postgres.SaveBOM(ctx, exec, scope(ctx), h.bom, created, h.from, h.s.Clock.Now()); err != nil {
		return action.Outcome{}, bomConflict(err)
	}
	return action.Outcome{ObjectID: h.bom.ID, RowVersion: h.bom.RootVersion, Status: status, Events: events}, nil
}

func bomConflict(err error) error {
	if errors.Is(err, postgres.ErrConflict) {
		return kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
	}
	return err
}

// SaveCostBOM saves the draft. The apparel plugin does not have to be active.
func (s Service) SaveCostBOM(ctx context.Context, cmd api.SaveCostBOM) (api.Receipt, error) {
	content, err := bomContent(cmd.Data)
	if err != nil {
		return api.Receipt{}, err
	}
	h := &bomCommand{s: s, id: cmd.BomID}
	h.run = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		created := h.bom == nil
		if created {
			if cmd.ExpectedVersion != 0 {
				return action.Outcome{}, kernel.Reject(kernel.CodeNotFound, "/args/id", "")
			}
			h.bom = costbom.New(cmd.BomID, d.Actor.ID, d.Team)
		} else if err := costbom.Check(h.bom, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.bom.Save(content, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		return h.save(ctx, exec, created, string(r.Status))
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveCostBOM, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BomID},
		cmd, cmd.Validate(), h)
}

// VerifyCostBOM freezes the draft hash. The drafter is refused.
func (s Service) VerifyCostBOM(ctx context.Context, cmd api.VerifyCostBOM) (api.Receipt, error) {
	h := &bomCommand{s: s, id: cmd.BomID}
	h.run = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costbom.Check(h.bom, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.bom.Verify(cmd.Revision, d.Actor, d.Epoch, s.Clock.Now())
		if err != nil {
			return action.Outcome{}, err
		}
		event := action.Event{Type: api.EventCostBOMVerified, AggregateType: "CostBOM", AggregateID: h.bom.ID, AggregateVersion: h.bom.RootVersion,
			Fields: map[string]any{"bom_id": h.bom.ID, "revision": r.Number, "content_sha256": r.Hash, "document_profile": h.bom.Profile}}
		return h.save(ctx, exec, false, string(r.Status), event)
	}
	return s.run(ctx, action.Command{Name: api.CommandVerifyCostBOM, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BomID},
		cmd, cmd.Validate(), h)
}

// WithdrawCostBOM withdraws a sealed revision.
func (s Service) WithdrawCostBOM(ctx context.Context, cmd api.WithdrawCostBOM) (api.Receipt, error) {
	h := &bomCommand{s: s, id: cmd.BomID}
	h.run = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costbom.Check(h.bom, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		r, err := h.bom.Withdraw(cmd.Revision, d.Actor, d.Epoch, s.Clock.Now(), cmd.Reason)
		if err != nil {
			return action.Outcome{}, err
		}
		event := action.Event{Type: api.EventCostBOMWithdrawn, AggregateType: "CostBOM", AggregateID: h.bom.ID, AggregateVersion: h.bom.RootVersion,
			Fields: map[string]any{"bom_id": h.bom.ID, "revision": r.Number}}
		return h.save(ctx, exec, false, string(h.bom.Status), event)
	}
	return s.run(ctx, action.Command{Name: api.CommandWithdrawCostBOM, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BomID, Reason: cmd.Reason},
		cmd, cmd.Validate(), h)
}

// RequestSubstitution records a substitution against the verified revision. It does not change that revision.
func (s Service) RequestSubstitution(ctx context.Context, cmd api.RequestSubstitution) (api.Receipt, error) {
	qty, err := canonDecimal(cmd.ApplicableQuantity, kernel.QuantityFormat)
	if err != nil {
		return api.Receipt{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/applicable_quantity", "")
	}
	minor, err := canonMinor(cmd.CostDeltaMinor, cmd.Currency, cmd.MinorUnits)
	if err != nil {
		return api.Receipt{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/cost_delta_minor", "")
	}
	delta, err := parseMinor(minor)
	if err != nil {
		return api.Receipt{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/cost_delta_minor", "")
	}
	h := &bomCommand{s: s, id: cmd.BomID}
	h.run = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costbom.Check(h.bom, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		active := h.bom.Active()
		if active == nil || active.Status != costbom.StatusVerified || !lineMatches(active, cmd.OriginalLineID, cmd.OriginalMaterialCode) {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/bom_id", "")
		}
		h.bom.RootVersion++
		if err := postgres.SaveBOM(ctx, exec, scope(ctx), h.bom, false, h.from, s.Clock.Now()); err != nil {
			return action.Outcome{}, bomConflict(err)
		}
		row := postgres.SubstitutionRow{ID: cmd.SubstitutionID, BomID: cmd.BomID, LineID: cmd.OriginalLineID, FromCode: cmd.OriginalMaterialCode,
			ToCode: cmd.SubstituteMaterialCode, Quantity: qty, SpecRevision: cmd.TargetSpecRevision, CostDeltaMinor: delta,
			Currency: cmd.Currency, MinorUnits: cmd.MinorUnits, LeadDays: cmd.LeadDaysDelta, SampleImpact: cmd.SampleImpact, Reason: cmd.Reason}
		if err := postgres.InsertSubstitution(ctx, exec, scope(ctx), row, d.Actor, kernel.Instant(s.Clock.Now())); err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{ObjectID: h.bom.ID, RowVersion: h.bom.RootVersion, Status: "requested"}, nil
	}
	return s.run(ctx, action.Command{Name: api.CommandRequestSubstitution, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BomID},
		cmd, cmd.Validate(), h)
}

// DecideSubstitution approves or rejects. Approval appends a verified revision and does not rewrite the old one.
func (s Service) DecideSubstitution(ctx context.Context, cmd api.DecideSubstitution) (api.Receipt, error) {
	h := &bomCommand{s: s, id: cmd.BomID}
	h.run = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := costbom.Check(h.bom, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		row, err := postgres.LoadSubstitution(ctx, exec, scope(ctx), cmd.SubstitutionID)
		if err != nil {
			return action.Outcome{}, err
		}
		if row == nil || row.BomID != cmd.BomID || row.Status != "requested" {
			return action.Outcome{}, kernel.Reject(kernel.CodeInvalidState, "/args/substitution_id", "")
		}
		if d.Actor.Kind != kernel.ActorHuman || d.Actor.ID == row.RequestedByID {
			return action.Outcome{}, kernel.Reject(kernel.CodePermissionDenied, "/", "")
		}
		var decided int64
		status := "rejected"
		if cmd.Decision == api.DecisionApprove {
			rev, err := h.bom.ApproveSubstitution(row.LineID, row.FromCode, row.ToCode, row.RequestedByID, d.Actor, d.Epoch, s.Clock.Now())
			if err != nil {
				return action.Outcome{}, err
			}
			decided = rev.Number
			status = "approved"
		} else {
			h.bom.RootVersion++
		}
		if err := postgres.SaveBOM(ctx, exec, scope(ctx), h.bom, false, h.from, s.Clock.Now()); err != nil {
			return action.Outcome{}, bomConflict(err)
		}
		if err := postgres.DecideSubstitution(ctx, exec, scope(ctx), cmd.SubstitutionID, status, d.Actor, d.Epoch, decided, kernel.Instant(s.Clock.Now())); err != nil {
			return action.Outcome{}, bomConflict(err)
		}
		event := action.Event{Type: api.EventCostBOMSubstitution, AggregateType: "CostBOM", AggregateID: h.bom.ID, AggregateVersion: h.bom.RootVersion,
			Fields: map[string]any{"bom_id": h.bom.ID, "substitution_id": cmd.SubstitutionID, "decision": cmd.Decision, "revision": decided}}
		return action.Outcome{ObjectID: h.bom.ID, RowVersion: h.bom.RootVersion, Status: status, Events: []action.Event{event}}, nil
	}
	return s.run(ctx, action.Command{Name: api.CommandDecideSubstitution, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BomID},
		cmd, cmd.Validate(), h)
}

func lineMatches(r *costbom.Revision, lineID, material string) bool {
	for _, l := range r.Lines {
		if l.LineID == lineID && l.MaterialCode == material {
			return true
		}
	}
	return false
}

func bomContent(d api.CostBOMData) (costbom.Content, error) {
	profile := d.DocumentProfile
	if profile == "" {
		profile = "generic"
	}
	c := costbom.Content{Profile: profile, BaseSize: d.BaseSizeCode, ItemID: d.ItemID, SkuID: d.SkuID, SpecRevision: d.SpecRevision}
	for _, l := range d.Lines {
		line := costbom.Line{LineID: l.LineID, MaterialKind: l.MaterialKind, MaterialCode: l.MaterialCode, Unit: l.Unit,
			GradingMode: l.GradingMode, Measured: l.Measured, FormulaBasis: l.FormulaBasis}
		var err error
		if line.ConsumptionPerUnit, err = canonDecimal(l.ConsumptionPerUnit, kernel.QuantityFormat); err != nil {
			return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
		}
		if line.WastageRatio, err = canonDecimal(l.WastageRatio, kernel.PriceFormat); err != nil {
			return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
		}
		if line.ShrinkageFactor, err = canonDecimal(l.ShrinkageFactor, kernel.PriceFormat); err != nil {
			return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
		}
		if line.FabricWidthCM, err = canonDecimal(l.FabricWidthCM, kernel.QuantityFormat); err != nil {
			return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
		}
		if line.FabricGSM, err = canonDecimal(l.FabricGSM, kernel.QuantityFormat); err != nil {
			return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
		}
		if l.Supply != nil {
			line.Supply = &costbom.SupplyRef{Kind: l.Supply.Kind, ID: l.Supply.ID, Revision: l.Supply.Revision}
		}
		for _, g := range l.Grades {
			grade := costbom.Grade{SizeCode: g.SizeCode, Currency: g.Currency, MinorUnits: g.MinorUnits}
			if grade.ConsumptionPerUnit, err = canonDecimal(g.ConsumptionPerUnit, kernel.QuantityFormat); err != nil {
				return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
			}
			if g.SizeCMDeltaMinor != "" {
				if grade.SizeCMDeltaMinor, err = canonMinor(g.SizeCMDeltaMinor, g.Currency, g.MinorUnits); err != nil {
					return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/lines", "")
				}
			}
			line.Grades = append(line.Grades, grade)
		}
		c.Lines = append(c.Lines, line)
	}
	for _, s := range d.Surcharges {
		item := costbom.Surcharge{SurchargeID: s.SurchargeID, Kind: s.Kind, Scope: s.Scope, ColorCode: s.ColorCode,
			MaterialCode: s.MaterialCode, MCQUnit: s.MCQUnit, Currency: s.Currency, MinorUnits: s.MinorUnits,
			Allocation: s.Allocation, FeeCategory: s.FeeCategory}
		var err error
		if item.MCQThreshold, err = canonDecimal(s.MCQThreshold, kernel.QuantityFormat); err != nil {
			return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/surcharges", "")
		}
		if s.AmountMinor != "" {
			if item.AmountMinor, err = canonMinor(s.AmountMinor, s.Currency, s.MinorUnits); err != nil {
				return costbom.Content{}, kernel.Reject(kernel.CodeSchemaInvalid, "/args/data/surcharges", "")
			}
		}
		c.Surcharges = append(c.Surcharges, item)
	}
	return c, nil
}

func canonDecimal(s string, f kernel.DecimalFormat) (string, error) {
	if s == "" {
		return "", nil
	}
	d, err := kernel.ParseDecimal(s, f)
	if err != nil {
		return "", err
	}
	return d.String(), nil
}

func canonMinor(minor, currency string, units int) (string, error) {
	c, err := kernel.NewCurrency(currency, units)
	if err != nil {
		return "", err
	}
	m, err := kernel.ParseMinor(c, minor)
	if err != nil {
		return "", err
	}
	return m.MinorString(), nil
}

func parseMinor(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
