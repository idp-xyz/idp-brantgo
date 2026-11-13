package application

import (
	"context"
	"encoding/json"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/supplierrfq"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// rfqCommand locks a supplier RFQ and applies one command.
type rfqCommand struct {
	s     Service
	id    string
	rfq   *supplierrfq.SupplierRfq
	from  int64
	apply func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *rfqCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.rfq, err = postgres.LoadSupplierRfq(ctx, exec, scope(ctx), h.id, true)
	h.from = 0
	if h.rfq != nil {
		h.from = h.rfq.RowVersion
	}
	return err
}

func (h *rfqCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *rfqCommand) visible(d authorization.Decision) bool {
	return h.rfq != nil && d.Sees(h.rfq.OwnerActor, h.rfq.OwnerTeam)
}

func (h *rfqCommand) outcome(event string) action.Outcome {
	r := h.rfq
	out := action.Outcome{ObjectID: r.ID, RowVersion: r.RowVersion, Status: string(r.Status)}
	if event == "" {
		return out
	}
	var content api.SupplierRfq
	_ = json.Unmarshal(r.Data, &content) // stored by SUP
	fields := map[string]any{"supplier_rfq_id": r.ID, "row_version": r.RowVersion, "issued_version": r.IssuedVersion,
		"content_hash": r.Hash, "rfq_line_ids": content.RfqLineIDs}
	if p := content.Supplier; p != nil {
		fields["supplier_party_id"], fields["supplier_row_version"] = p.PartyID, p.RowVersion
	}
	if q := content.Rfq; q != nil {
		fields["rfq_id"], fields["rfq_row_version"] = q.RfqID, q.RowVersion
	}
	out.Events = []action.Event{{Type: event, AggregateType: "SupplierRfq", AggregateID: r.ID, AggregateVersion: r.RowVersion, Fields: fields}}
	return out
}

// supplierRfqHash returns the canonical content of a supplier RFQ and its SHA-256.
func supplierRfqHash(d api.SupplierRfq) ([]byte, string, error) {
	data, err := json.Marshal(d)
	if err != nil {
		return nil, "", err
	}
	_, hash, err := hashJSON(struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}{"supplier_rfq", data})
	return data, hash, err
}

// SaveSupplierRfq saves a whole draft supplier RFQ (T02 §13.2 supplier_rfq.save_draft).
func (s Service) SaveSupplierRfq(ctx context.Context, cmd api.SaveSupplierRfq) (api.Receipt, error) {
	var f facts
	h := &rfqCommand{s: s, id: cmd.SupplierRfqID}
	prepare := func(ctx context.Context) (err error) {
		var r refs
		r.supplierRfq(cmd.Data)
		f, err = s.gather(ctx, r, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := supplierrfq.CheckSave(h.rfq, h.visible(d), cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		if err := (judge{f: f, at: now}).supplierRfq(&cmd.Data); err != nil {
			return action.Outcome{}, err
		}
		data, hash, err := supplierRfqHash(cmd.Data)
		if err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		if h.rfq == nil {
			h.rfq = supplierrfq.New(cmd.SupplierRfqID, d.Actor.ID, d.Team)
			h.rfq.Replace(data, hash, d.Actor, d.Epoch, now)
			return h.outcome(""), postgres.InsertSupplierRfq(ctx, exec, sc, h.rfq, now)
		}
		h.rfq.Replace(data, hash, d.Actor, d.Epoch, now)
		return h.outcome(""), postgres.UpdateSupplierRfq(ctx, exec, sc, h.rfq, h.from, now)
	}
	_, _, args := request(cmd)
	return s.run(ctx, action.Command{Name: api.CommandSaveSupplierRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SupplierRfqID},
		args, cmd.Validate(), prepared{h, prepare})
}

// RecordSupplierRfqIssued records the human sending of a complete draft and seals it (T02 §13.1,
// §17.1: the supplier, the current ready RFQ and its lines are checked).
func (s Service) RecordSupplierRfqIssued(ctx context.Context, cmd api.RecordSupplierRfqIssued) (api.Receipt, error) {
	var f facts
	var seen int64
	h := &rfqCommand{s: s, id: cmd.SupplierRfqID}
	prepare := func(ctx context.Context) error {
		var r refs
		r.evidence(cmd.Evidence)
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
			stored, err := postgres.LoadSupplierRfq(ctx, exec, scope(ctx), cmd.SupplierRfqID, false)
			if err != nil || stored == nil || !d.Sees(stored.OwnerActor, stored.OwnerTeam) {
				return err
			}
			var content api.SupplierRfq
			if err = json.Unmarshal(stored.Data, &content); err != nil {
				return err
			}
			seen = stored.RowVersion
			r.supplierRfq(content)
			return nil
		})
		if err != nil {
			return err
		}
		f, err = s.gather(ctx, r, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := supplierrfq.Check(h.rfq, h.visible(d), cmd.ExpectedVersion, supplierrfq.Draft); err != nil {
			return action.Outcome{}, err
		}
		if h.rfq.RowVersion != seen {
			// The draft changed after its references were read; issuing would judge stale facts.
			return action.Outcome{}, kernel.Reject(kernel.CodeVersionConflict, "/args/expected_version", "")
		}
		var content api.SupplierRfq
		if err := json.Unmarshal(h.rfq.Data, &content); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		j := judge{f: f, at: now, seal: true}
		if err := j.supplierRfq(&content); err != nil {
			return action.Outcome{}, err
		}
		if err := j.sources(cmd.Evidence, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		h.rfq.RecordIssued(d.Actor, d.Epoch, cmd.Reason, toEvidence(cmd.Evidence), now)
		return h.outcome(api.EventSupplierRfqIssued), postgres.UpdateSupplierRfq(ctx, exec, scope(ctx), h.rfq, h.from, now)
	}
	_, _, args := request(cmd)
	return s.run(ctx, action.Command{Name: api.CommandIssueSupplierRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SupplierRfqID,
		Reason: cmd.Reason}, args, cmd.Validate(), prepared{h, prepare})
}

// CloseSupplierRfq closes an issued supplier RFQ; its issued content and version stay.
func (s Service) CloseSupplierRfq(ctx context.Context, cmd api.CloseSupplierRfq) (api.Receipt, error) {
	var f facts
	h := &rfqCommand{s: s, id: cmd.SupplierRfqID}
	prepare := func(ctx context.Context) (err error) {
		var r refs
		r.evidence(cmd.Evidence)
		f, err = s.gather(ctx, r, s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := supplierrfq.Check(h.rfq, h.visible(d), cmd.ExpectedVersion, supplierrfq.Issued); err != nil {
			return action.Outcome{}, err
		}
		if err := (judge{f: f}).sources(cmd.Evidence, "/args/evidence"); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		h.rfq.Close(d.Actor, d.Epoch, cmd.Reason, toEvidence(cmd.Evidence), now)
		return h.outcome(""), postgres.UpdateSupplierRfq(ctx, exec, scope(ctx), h.rfq, h.from, now)
	}
	_, _, args := request(cmd)
	return s.run(ctx, action.Command{Name: api.CommandCloseSupplierRfq, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SupplierRfqID,
		Reason: cmd.Reason}, args, cmd.Validate(), prepared{h, prepare})
}
