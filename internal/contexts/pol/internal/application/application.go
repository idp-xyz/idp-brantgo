// Package application runs POL's commands and queries on the command ledger. Configuration rules
// live in domain/config, numbering in domain/numbering and snapshots in domain/snapshot; SQL lives
// in adapters/postgres. POL has no upstream context (every POL relationship of the Context Map is
// downstream), so no command reads another context; the queries serve POL's downstream contexts.
package application

import (
	"context"
	"errors"
	"maps"
	"strconv"
	"time"

	"go.idp.xyz/idp-bento-go/application"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/numbering"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/snapshot"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Read commands and permissions.
const (
	CommandGetConfig   = "config.get"
	CommandGetSnapshot = "snapshot.get"
	CommandAudit       = "pol.audit"
	PermissionRead     = "pol.read"
	PermissionAudit    = "pol.audit"
)

var (
	anyActor     = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI, kernel.ActorSystem}
	humanOrAgent = []kernel.ActorKind{kernel.ActorHuman, kernel.ActorAI}
)

// Rules is POL's authorization policy (05 stage humanOnlyCommands: config.publish, config.retire,
// config.discard_draft). Drafts, numbers and snapshots are open to AI agents with the grant.
func Rules() []authorization.Rule {
	return []authorization.Rule{
		{Command: api.CommandSaveDraft, Permission: api.CommandSaveDraft, ActorKinds: humanOrAgent},
		authorization.HumanOnly(api.CommandDiscardDraft, api.CommandDiscardDraft),
		authorization.HumanOnly(api.CommandPublish, api.CommandPublish),
		authorization.HumanOnly(api.CommandRetire, api.CommandRetire),
		{Command: api.CommandIssueNumber, Permission: api.CommandIssueNumber, ActorKinds: humanOrAgent},
		{Command: api.CommandCaptureSnapshot, Permission: api.CommandCaptureSnapshot, ActorKinds: humanOrAgent},
		{Command: CommandGetConfig, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandGetSnapshot, Permission: PermissionRead, ActorKinds: anyActor},
		{Command: CommandAudit, Permission: PermissionAudit, ActorKinds: anyActor},
	}
}

// Visible reports whether d's actor sees the object a stored result names. Configurations, issued
// numbers and snapshots belong to the company, not to an owner (Python foundation_root has none),
// so the read permission the ledger checks before a replay is the only restriction.
func Visible(context.Context, bentopg.Executor, authorization.Decision, string, string) (bool, error) {
	return true, nil
}

// Service runs POL's use cases.
type Service struct {
	Ledger *action.Ledger
	Clock  application.Clock
}

func (s Service) run(ctx context.Context, cmd action.Command, args any, validation error, h action.Handler) (api.Receipt, error) {
	if validation != nil {
		if rej, ok := kernel.AsRejection(validation); ok {
			if err := s.Ledger.AuditRejected(ctx, cmd.Name, cmd.ObjectID, rej.Code); err != nil {
				return api.Receipt{}, errors.Join(validation, err)
			}
		}
		return api.Receipt{}, validation
	}
	hash, err := action.RequestHash(args)
	if err != nil {
		return api.Receipt{}, err
	}
	cmd.RequestHash = hash
	res, err := s.Ledger.Execute(ctx, cmd, h)
	return api.Receipt(res), err
}

func scope(ctx context.Context) kernel.Scope {
	s, _ := bento.ScopeFromContext(ctx)
	return s
}

// SaveDraft saves the whole working revision of a configuration (Python foundation.save).
func (s Service) SaveDraft(ctx context.Context, cmd api.SaveDraft) (api.Receipt, error) {
	validation := cmd.Validate()
	var c config.Content
	if validation == nil {
		c = content(cmd)
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	h := &configCommand{kind: cmd.Kind, id: cmd.ConfigID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := config.CheckSave(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err := config.Validate(cmd.ConfigID, c); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		f, err := facts(ctx, exec, c, now)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = config.CheckReferences(c, f); err != nil {
			return action.Outcome{}, err
		}
		created := h.root == nil
		if created {
			h.root = config.New(config.Kind(cmd.Kind), cmd.ConfigID)
		}
		rev, fresh, err := h.root.Save(c, d.Actor, d.Epoch, now)
		if err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		if created {
			err = postgres.InsertRoot(ctx, exec, sc, h.root, now)
		} else {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		if err == nil && fresh {
			err = postgres.InsertRevision(ctx, exec, sc, h.root, rev)
		} else if err == nil {
			err = postgres.UpdateRevision(ctx, exec, sc, h.root, rev)
		}
		return h.outcome(rev), err
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveDraft, IdempotencyKey: cmd.IdempotencyKey, ObjectID: api.ObjectID(cmd.Kind, cmd.ConfigID)},
		args, validation, h)
}

// Publish makes the working revision active and supersedes the active one (Python
// foundation.publish).
func (s Service) Publish(ctx context.Context, cmd api.Publish) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	h := &configCommand{kind: cmd.Kind, id: cmd.ConfigID}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := config.Check(h.root, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		var f config.Facts
		if w := h.root.Working; w != nil {
			var err error
			if f, err = facts(ctx, exec, w.Content, now); err != nil {
				return action.Outcome{}, err
			}
		}
		superseded, err := h.root.Publish(cmd.Revision, f, d.Actor, d.Epoch, cmd.EvidenceIDs, now)
		if err != nil {
			return action.Outcome{}, err
		}
		sc, active := scope(ctx), h.root.Active
		events := []action.Event{h.event(api.EventConfigPublished, active, nil)}
		if superseded != nil {
			// The superseded revision is written first: there is one active revision at a time.
			err = postgres.UpdateRevision(ctx, exec, sc, h.root, superseded)
			events = append(events, h.event(api.EventConfigSuperseded, superseded, map[string]any{"superseded_by_revision": active.Number}))
		}
		if err == nil {
			err = postgres.UpdateRevision(ctx, exec, sc, h.root, active)
		}
		if err == nil {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		return h.outcome(active, events...), err
	}
	return s.run(ctx, action.Command{Name: api.CommandPublish, IdempotencyKey: cmd.IdempotencyKey, ObjectID: api.ObjectID(cmd.Kind, cmd.ConfigID),
		Reason: cmd.Reason}, args, cmd.Validate(), h)
}

// Retire ends the active revision (Python foundation.retire).
func (s Service) Retire(ctx context.Context, cmd api.Retire) (api.Receipt, error) {
	return s.end(ctx, api.CommandRetire, cmd.IdempotencyKey, cmd.Kind, cmd.ConfigID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		api.EventConfigRetired, (*config.Root).Retire)
}

// DiscardDraft ends the working revision.
func (s Service) DiscardDraft(ctx context.Context, cmd api.DiscardDraft) (api.Receipt, error) {
	return s.end(ctx, api.CommandDiscardDraft, cmd.IdempotencyKey, cmd.Kind, cmd.ConfigID, cmd.ExpectedVersion, cmd.Reason, cmd.Validate(),
		"", (*config.Root).Discard)
}

func (s Service) end(ctx context.Context, command, key, kind, id string, expected int64, reason string, validation error, event string,
	move func(*config.Root, time.Time) (*config.Revision, error)) (api.Receipt, error) {
	h := &configCommand{kind: kind, id: id}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := config.Check(h.root, expected); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		rev, err := move(h.root, now)
		if err != nil {
			return action.Outcome{}, err
		}
		sc := scope(ctx)
		if err = postgres.UpdateRevision(ctx, exec, sc, h.root, rev); err == nil {
			err = postgres.UpdateRoot(ctx, exec, sc, h.root, h.from, now)
		}
		var events []action.Event
		if event != "" {
			events = append(events, h.event(event, rev, nil))
		}
		return h.outcome(rev, events...), err
	}
	return s.run(ctx, action.Command{Name: command, IdempotencyKey: key, ObjectID: api.ObjectID(kind, id), Reason: reason},
		struct{ Kind, ID, Reason string }{kind, id, reason}, validation, h)
}

// configCommand locks a configuration root and applies one command.
type configCommand struct {
	kind  string
	id    string
	root  *config.Root
	from  int64
	apply func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *configCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.root, err = postgres.LoadRoot(ctx, exec, scope(ctx), h.kind, h.id, true)
	h.from = 0
	if h.root != nil {
		h.from = h.root.Version
	}
	return err
}

func (h *configCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *configCommand) outcome(rev *config.Revision, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: api.ObjectID(h.kind, h.id), RowVersion: h.root.Version, Status: string(rev.Status), Events: events}
}

// event carries identifiers, versions and the content hash of rev, never its content.
func (h *configCommand) event(typ string, rev *config.Revision, fields map[string]any) action.Event {
	f := map[string]any{"kind": h.kind, "config_id": h.id, "revision": rev.Number, "root_version": h.root.Version, "content_hash": rev.Hash}
	maps.Copy(f, fields)
	return action.Event{Type: typ, AggregateType: "ConfigurationRoot", AggregateID: api.ObjectID(h.kind, h.id),
		AggregateVersion: h.root.Version, Fields: f}
}

// facts reads, at instant at, the currencies an exchange rate names.
func facts(ctx context.Context, exec bentopg.Executor, c config.Content, at time.Time) (config.Facts, error) {
	f := config.Facts{Currencies: map[string]error{}}
	if c.Kind != config.KindFXRate {
		return f, nil
	}
	for _, code := range []string{c.FXRate.FromCurrency, c.FXRate.ToCurrency} {
		cur, err := currencyAt(ctx, exec, code, at)
		if err != nil {
			return f, err
		}
		if cur.known {
			f.Currencies[code] = cur.refused
		}
	}
	return f, nil
}

// currency is a currency configuration at an instant: whether it is configured at all, and either
// its revision in force or what InForce says against it.
type currency struct {
	known   bool
	rev     *config.Revision
	refused error
}

// check returns nil for a currency in force and the rejection against it at path otherwise.
func (c currency) check(path string) error {
	switch {
	case !c.known:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case c.refused != nil:
		return config.Repath(c.refused, path)
	}
	return nil
}

func currencyAt(ctx context.Context, exec bentopg.Executor, code string, at time.Time) (currency, error) {
	rev, known, err := postgres.PublishedAt(ctx, exec, scope(ctx), string(config.KindCurrency), code, at)
	if err != nil || !known {
		return currency{}, err
	}
	c := currency{known: true, refused: config.InForce(rev, at)}
	if c.refused == nil {
		c.rev = rev
	}
	return c, nil
}

// IssueNumber issues the next document number of a sequence (Python foundation.number.issue).
func (s Service) IssueNumber(ctx context.Context, cmd api.IssueNumber) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey = ""
	return s.run(ctx, action.Command{Name: api.CommandIssueNumber, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.BusinessRef},
		args, cmd.Validate(), &numberCommand{s: s, cmd: cmd})
}

// numberCommand locks the counter of the sequence's document type and year, then issues.
type numberCommand struct {
	s       Service
	cmd     api.IssueNumber
	now     time.Time
	seq     *config.Revision
	counter *numbering.Counter
	from    int64
}

func (h *numberCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	h.now, h.seq, h.counter, h.from = h.s.Clock.Now(), nil, nil, 0
	sc := scope(ctx)
	seq, err := postgres.LoadRevision(ctx, exec, sc, postgres.Key(h.cmd.Sequence))
	if err != nil || seq == nil {
		return err
	}
	n := seq.Content.NumberSequence
	year, err := numbering.Year(h.now, n.TimeZone)
	if err != nil {
		return err
	}
	h.seq = seq
	h.counter, err = postgres.LockCounter(ctx, exec, sc, n.DocumentType, year)
	if h.counter != nil {
		h.from = h.counter.Last
	}
	return err
}

func (h *numberCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	if h.seq == nil {
		return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/sequence", "")
	}
	sc, n := scope(ctx), h.seq.Content.NumberSequence
	existing, err := postgres.IssuedFor(ctx, exec, sc, n.DocumentType, h.cmd.BusinessRef)
	if err != nil {
		return action.Outcome{}, err
	}
	if existing != nil {
		if err = numbering.Reuse(*existing, h.cmd.Sequence.ConfigID); err != nil {
			return action.Outcome{}, err
		}
		return action.Outcome{ObjectID: existing.Number, RowVersion: 1, Status: api.StatusIssued}, nil
	}
	if err = config.InForce(h.seq, h.now); err != nil {
		return action.Outcome{}, config.Repath(err, "/args/sequence")
	}
	issued, err := h.counter.Next(n.Prefix, n.Width)
	if err != nil {
		return action.Outcome{}, err
	}
	issued.BusinessRef, issued.SequenceID, issued.SequenceRevision = h.cmd.BusinessRef, h.cmd.Sequence.ConfigID, h.cmd.Sequence.Revision
	if err = postgres.SaveCounter(ctx, exec, sc, h.counter, h.from, h.now); err == nil {
		err = postgres.InsertIssued(ctx, exec, sc, issued, d.Actor, d.Epoch, h.now)
	}
	year := strconv.Itoa(issued.Year)
	return action.Outcome{ObjectID: issued.Number, RowVersion: 1, Status: api.StatusIssued, Events: []action.Event{{
		Type: api.EventNumberIssued, AggregateType: "DocumentNumberSequence", AggregateID: issued.DocumentType + ":" + year,
		AggregateVersion: issued.Value, Fields: map[string]any{"document_number": issued.Number, "document_type": issued.DocumentType,
			"year": issued.Year, "sequence_value": issued.Value, "business_ref": issued.BusinessRef,
			"sequence_config_id": issued.SequenceID, "sequence_revision": issued.SequenceRevision}}}}, err
}

// CaptureSnapshot records the exact revisions a calculation adopts (Python foundation.snapshot).
func (s Service) CaptureSnapshot(ctx context.Context, cmd api.CaptureSnapshot) (api.Receipt, error) {
	args := cmd
	args.IdempotencyKey = ""
	return s.run(ctx, action.Command{Name: api.CommandCaptureSnapshot, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.SnapshotID},
		args, cmd.Validate(), &snapshotCommand{s: s, cmd: cmd})
}

// snapshotCommand creates a new FoundationSnapshot; there is no root to lock, and two captures of
// one ID collide on its key.
type snapshotCommand struct {
	s   Service
	cmd api.CaptureSnapshot
}

func (h *snapshotCommand) Lock(context.Context, bentopg.Executor) error { return nil }

// Apply adopts every reference in force now and, for an exchange rate, requires the snapshot's
// purpose and a rate that is not stale (POL-INV-003) and adopts the revisions of its currencies.
func (h *snapshotCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	sc := scope(ctx)
	taken, err := postgres.SnapshotExists(ctx, exec, sc, h.cmd.SnapshotID)
	if err != nil {
		return action.Outcome{}, err
	}
	if err = snapshot.CheckNew(taken); err != nil {
		return action.Outcome{}, err
	}
	now := h.s.Clock.Now()
	keys := make([]postgres.Key, len(h.cmd.Refs))
	for i, r := range h.cmd.Refs {
		keys[i] = postgres.Key(r)
	}
	found, err := postgres.RevisionsByKey(ctx, exec, sc, keys)
	if err != nil {
		return action.Outcome{}, err
	}
	var entries []snapshot.Entry
	for i, ref := range h.cmd.Refs {
		path := "/args/refs/" + strconv.Itoa(i)
		rev := found[keys[i]]
		if rev == nil {
			return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
		}
		if err = config.InForce(rev, now); err != nil {
			return action.Outcome{}, config.Repath(err, path)
		}
		entries = append(entries, snapshot.Entry{Kind: ref.Kind, ConfigID: ref.ConfigID, Revision: ref.Revision, ContentHash: rev.Hash})
		if rev.Content.Kind != config.KindFXRate {
			continue
		}
		x := rev.Content.FXRate
		use := config.FXUse{From: x.FromCurrency, To: x.ToCurrency, Purpose: h.cmd.Purpose, At: now}
		if _, err = config.DecideFX(use, rev); err != nil {
			return action.Outcome{}, config.Repath(err, path)
		}
		for _, code := range []string{x.FromCurrency, x.ToCurrency} {
			cur, err := currencyAt(ctx, exec, code, now)
			if err == nil {
				err = cur.check(path)
			}
			if err != nil {
				return action.Outcome{}, err
			}
			entries = append(entries, snapshot.Entry{Kind: string(config.KindCurrency), ConfigID: code, Revision: cur.rev.Number, ContentHash: cur.rev.Hash})
		}
	}
	snap, err := snapshot.Capture(h.cmd.SnapshotID, h.cmd.Purpose, now, d.Actor, d.Epoch, entries)
	if err != nil {
		return action.Outcome{}, err
	}
	err = postgres.InsertSnapshot(ctx, exec, sc, snap)
	return action.Outcome{ObjectID: snap.ID, RowVersion: 1, Status: api.StatusCaptured, Events: []action.Event{{
		Type: api.EventSnapshotCaptured, AggregateType: "FoundationSnapshot", AggregateID: snap.ID, AggregateVersion: 1,
		Fields: map[string]any{"snapshot_id": snap.ID, "purpose": snap.Purpose, "content_hash": snap.Hash, "entries": len(snap.Entries)}}}}, err
}

// GetConfig returns a configuration and all its revisions, audited (Python foundation_get).
func (s Service) GetConfig(ctx context.Context, kind, id string) (api.ConfigView, error) {
	var v api.ConfigView
	err := s.Ledger.Read(ctx, CommandGetConfig, api.ObjectID(kind, id), func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		if !api.KindKnown(kind) || !api.ValidConfigID(id) {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		sc := scope(ctx)
		root, err := postgres.LoadRoot(ctx, exec, sc, kind, id, false)
		if err != nil {
			return err
		}
		if root == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.ConfigView{Kind: kind, ConfigID: id, RootVersion: root.Version}
		if root.Working != nil {
			v.WorkingRevision = root.Working.Number
		}
		if root.Active != nil {
			v.ActiveRevision = root.Active.Number
		}
		revisions, err := postgres.Revisions(ctx, exec, sc, kind, id)
		for _, r := range revisions {
			v.Revisions = append(v.Revisions, revisionView(r.Rev))
		}
		return err
	})
	return v, err
}

// ResolveConfigs answers ConfigRef v1 lookups of downstream contexts at an instant.
func (s Service) ResolveConfigs(ctx context.Context, refs []api.ConfigRef, at time.Time) (map[api.ConfigRef]api.ResolvedConfig, error) {
	out := map[api.ConfigRef]api.ResolvedConfig{}
	if err := api.ValidateRefs(refs); err != nil {
		return out, err
	}
	if at.IsZero() {
		return out, kernel.Reject(kernel.CodeSchemaInvalid, "/at", "")
	}
	if len(refs) == 0 {
		return out, nil
	}
	at = kernel.Instant(at)
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		keys := make([]postgres.Key, len(refs))
		for i, r := range refs {
			keys[i] = postgres.Key(r)
		}
		found, err := postgres.RevisionsByKey(ctx, exec, scope(ctx), keys)
		for i, ref := range refs {
			rev := found[keys[i]]
			if rev == nil {
				continue
			}
			r := api.ResolvedConfig{Ref: ref, Status: string(rev.Status), ContentHash: rev.Hash, ValidFrom: rev.Content.ValidFrom,
				ValidUntil: rev.Content.ValidUntil, Data: data(rev.Content)}
			if rej, ok := kernel.AsRejection(config.InForce(rev, at)); ok {
				r.Code = rej.Code
			}
			out[ref] = r
		}
		return err
	})
	return out, err
}

// LookupFX applies POL-INV-003 to one use of an exchange rate.
func (s Service) LookupFX(ctx context.Context, q api.FXQuery) (api.ExchangeRate, error) {
	if err := q.Validate(); err != nil {
		return api.ExchangeRate{}, err
	}
	at := kernel.Instant(q.At)
	var out api.ExchangeRate
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var currencies [2]kernel.Currency
		for i, c := range []struct{ code, path string }{{q.FromCurrency, "/from_currency"}, {q.ToCurrency, "/to_currency"}} {
			cur, err := currencyAt(ctx, exec, c.code, at)
			if err == nil {
				err = cur.check(c.path)
			}
			if err != nil {
				return err
			}
			if currencies[i], err = kernel.NewCurrency(cur.rev.Content.Currency.Code, cur.rev.Content.Currency.MinorUnits); err != nil {
				return err
			}
		}
		var rate *config.Revision
		if q.Rate != nil {
			var err error
			if rate, err = postgres.LoadRevision(ctx, exec, scope(ctx), postgres.Key(*q.Rate)); err != nil {
				return err
			}
			if rate == nil {
				return kernel.Reject(kernel.CodeReferenceInvalid, "/rate", "")
			}
		}
		value, err := config.DecideFX(config.FXUse{From: q.FromCurrency, To: q.ToCurrency, Purpose: q.Purpose, At: at, MaxAge: q.MaxObservationAge}, rate)
		if err != nil {
			return err
		}
		if out.Value, err = kernel.NewExchangeRate(currencies[0], currencies[1], value); err != nil {
			return err
		}
		if rate != nil {
			out.Rate, out.ContentHash = *q.Rate, rate.Hash
			out.ObservedAt, err = kernel.ParseTimestamp(rate.Content.FXRate.ObservedAt)
		}
		return err
	})
	if err != nil {
		return api.ExchangeRate{}, err
	}
	return out, nil
}

// GetSnapshot returns a FoundationSnapshot, audited.
func (s Service) GetSnapshot(ctx context.Context, id string) (api.SnapshotView, error) {
	var v api.SnapshotView
	err := s.Ledger.Read(ctx, CommandGetSnapshot, id, func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		snap, err := postgres.LoadSnapshot(ctx, exec, scope(ctx), id)
		if err != nil {
			return err
		}
		if snap == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.SnapshotView{SnapshotID: snap.ID, Purpose: snap.Purpose, CapturedAt: snap.CapturedAt, CapturedBy: snap.CapturedBy,
			ContentHash: snap.Hash}
		for _, e := range snap.Entries {
			v.Entries = append(v.Entries, api.SnapshotEntry{Ref: api.ConfigRef{Kind: e.Kind, ConfigID: e.ConfigID, Revision: e.Revision},
				ContentHash: e.ContentHash})
		}
		return nil
	})
	return v, err
}

// Audit returns POL's audit trail.
func (s Service) Audit(ctx context.Context) ([]kernel.AuditEntry, error) {
	return s.Ledger.Audit(ctx, CommandAudit)
}

// AuditRejected records a request an inbound adapter refused before it became a command.
func (s Service) AuditRejected(ctx context.Context, command, objectID, code string) error {
	return s.Ledger.AuditRejected(ctx, command, objectID, code)
}
