package application

import (
	"context"
	"errors"
	"strings"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GetConfig returns a configuration and all its revisions, audited (Python foundation_get). Bank
// account revisions are filtered and masked (accounts).
func (s Service) GetConfig(ctx context.Context, kind, id string) (api.ConfigView, error) {
	var v api.ConfigView
	err := s.Ledger.Read(ctx, CommandGetConfig, api.ObjectID(kind, id), func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
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
		if err != nil {
			return err
		}
		reader := accounts{s: s, d: d}
		for _, r := range revisions {
			view := revisionView(r.Rev)
			visible, err := reader.show(ctx, exec, &view.Data)
			if err != nil {
				return err
			}
			if visible {
				v.Revisions = append(v.Revisions, view)
			}
		}
		if len(v.Revisions) == 0 {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		return nil
	})
	return v, err
}

// accounts applies the read rules of bank accounts (Python foundation_get, historical_access): a
// party's account is visible only to a reader whose data scope is the whole company, because POL
// cannot ask PTY which parties the reader sees (PTY is not upstream of POL); the account number is
// masked to its last four characters unless the reader holds pol.read_sensitive, which never
// replaces the data scope.
type accounts struct {
	s         Service
	d         authorization.Decision
	checked   bool
	sensitive bool
}

// show reports whether data is visible to the reader and masks its account number if the reader
// may not see it whole. Data of other kinds is always visible.
func (a *accounts) show(ctx context.Context, exec bentopg.Executor, data *api.ConfigData) (bool, error) {
	b := data.BankAccount
	if b == nil {
		return true, nil
	}
	if b.OwnerKind == api.OwnerParty && a.d.DataScope != authorization.DataScopeCompany {
		return false, nil
	}
	if !a.checked {
		_, err := a.s.Ledger.Authorizer.Authorize(ctx, exec, CommandReadSensitive)
		switch {
		case err == nil:
			a.sensitive = true
		case errors.Is(err, authorization.ErrForbidden), errors.Is(err, authorization.ErrActorKindDenied):
		default:
			return false, err
		}
		a.checked = true
	}
	if !a.sensitive {
		masked := *b
		masked.AccountNumber = mask(b.AccountNumber)
		data.BankAccount = &masked
	}
	return true, nil
}

// mask keeps the last four characters of an account number and stars the others (Python).
func mask(number string) string {
	r := []rune(number)
	keep := min(4, len(r))
	return strings.Repeat("*", len(r)-keep) + string(r[len(r)-keep:])
}

// ResolveConfigs answers ConfigRef v1 lookups of downstream contexts at an instant. Code covers the
// revision itself; a downstream that adopts dependencies too guards each of them (the entries of a
// FoundationSnapshot). Bank accounts follow the read rules of GetConfig.
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
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		keys := make([]postgres.Key, len(refs))
		for i, r := range refs {
			keys[i] = postgres.Key(r)
		}
		found, err := postgres.RevisionsByKey(ctx, exec, scope(ctx), keys)
		if err != nil {
			return err
		}
		reader := accounts{s: s, d: d}
		for i, ref := range refs {
			rev := found[keys[i]]
			if rev == nil {
				continue
			}
			r := api.ResolvedConfig{Ref: ref, Status: string(rev.Status), ContentHash: rev.Hash, ValidFrom: rev.Content.ValidFrom,
				ValidUntil: rev.Content.ValidUntil, Data: data(rev.Content)}
			visible, err := reader.show(ctx, exec, &r.Data)
			if err != nil {
				return err
			}
			if !visible {
				continue
			}
			if rej, ok := kernel.AsRejection(config.InForce(rev, at)); ok {
				r.Code = rej.Code
			}
			out[ref] = r
		}
		return nil
	})
	return out, err
}

// ConfigAt answers ConfigAt v1 (S5R-D23, POL-INV-007): the revision of a configuration that applies
// at an instant by its validity, published before or after it (config.At). It reads every revision
// of the configuration without locks: published revisions never change. Bank accounts follow the
// read rules of GetConfig; one the reader may not see is not known to it.
func (s Service) ConfigAt(ctx context.Context, q api.ConfigAtQuery) (api.RevisionAt, error) {
	if err := q.Validate(); err != nil {
		return api.RevisionAt{}, err
	}
	at := kernel.Instant(q.At)
	unknown := api.RevisionAt{Ref: api.ConfigRef{Kind: q.Kind, ConfigID: q.ConfigID}, Code: kernel.CodeReferenceInvalid}
	out := unknown
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		stored, err := postgres.Revisions(ctx, exec, scope(ctx), q.Kind, q.ConfigID)
		if err != nil || len(stored) == 0 {
			return err
		}
		revisions := make([]*config.Revision, len(stored))
		for i, r := range stored {
			revisions[i] = r.Rev
		}
		rev := config.At(revisions, at)
		if rev == nil {
			out = api.RevisionAt{Ref: unknown.Ref, Code: config.CodeConfigNotActive}
			return nil
		}
		out = api.RevisionAt{Ref: api.ConfigRef{Kind: q.Kind, ConfigID: q.ConfigID, Revision: rev.Number}, Status: string(rev.Status),
			ContentHash: rev.Hash, ValidFrom: rev.Content.ValidFrom, ValidUntil: rev.Content.ValidUntil, PublishedAt: rev.Publication.At,
			EndedAt: rev.EndedAt, Data: data(rev.Content)}
		reader := accounts{s: s, d: d}
		visible, err := reader.show(ctx, exec, &out.Data)
		if err == nil && !visible {
			out = unknown
		}
		return err
	})
	if err != nil {
		return api.RevisionAt{}, err
	}
	return out, nil
}

// LookupFX applies POL-INV-003 to one use of an exchange rate (Python foundation.fx.convert without
// the conversion, which the caller does with kernel.ExchangeRate).
func (s Service) LookupFX(ctx context.Context, q api.FXQuery) (api.ExchangeRate, error) {
	if err := q.Validate(); err != nil {
		return api.ExchangeRate{}, err
	}
	at := kernel.Instant(q.At)
	var out api.ExchangeRate
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		var currencies [2]kernel.Currency
		for i, c := range []struct{ code, path string }{{q.FromCurrency, "/from_currency"}, {q.ToCurrency, "/to_currency"}} {
			cur, err := inForce(ctx, exec, config.Target{Kind: config.KindCurrency, ID: c.code}, at, c.path)
			if err != nil {
				return err
			}
			if currencies[i], err = kernel.NewCurrency(cur.Content.Currency.Code, cur.Content.Currency.MinorUnits); err != nil {
				return err
			}
		}
		maxAge := q.MaxObservationAge
		if q.Policy != nil {
			policy, err := s.policyInForce(ctx, exec, *q.Policy, at)
			if err != nil {
				return err
			}
			if policy.CalculationCurrency != q.ToCurrency {
				return config.Reject(config.CodeFXScopeMismatch, "/policy")
			}
			if age := time.Duration(policy.MaxFXAgeSeconds) * time.Second; maxAge == 0 || age < maxAge {
				maxAge = age
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
		value, err := config.DecideFX(config.FXUse{From: q.FromCurrency, To: q.ToCurrency, Purpose: q.Purpose, At: at, MaxAge: maxAge}, rate)
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

// policyInForce returns the pricing policy ref names, in force at instant at with every revision it
// depends on (its calculation currency and tax profile); rejections are at /policy.
func (s Service) policyInForce(ctx context.Context, exec bentopg.Executor, ref api.ConfigRef, at time.Time) (*config.PricingPolicy, error) {
	key := postgres.Key(ref)
	rev, err := postgres.LoadRevision(ctx, exec, scope(ctx), key)
	if err != nil {
		return nil, err
	}
	if err = newAdoption(at, nil).adoptRef(ctx, exec, key, rev, "/policy"); err != nil {
		return nil, err
	}
	return rev.Content.PricingPolicy, nil
}

// LookupTax returns a tax profile revision a calculation may apply (Python foundation.tax.preview
// without the split, which the caller does with api.TaxRate.Split): in force at the instant, with a
// recovery treatment that is not unknown (TAX_POLICY_UNCONFIRMED), and a currency in force.
func (s Service) LookupTax(ctx context.Context, q api.TaxQuery) (api.TaxRate, error) {
	if err := q.Validate(); err != nil {
		return api.TaxRate{}, err
	}
	at := kernel.Instant(q.At)
	var out api.TaxRate
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		rev, err := inForce(ctx, exec, config.Target{Kind: config.KindTaxProfile, ID: q.Profile.ConfigID, Revision: q.Profile.Revision}, at, "/profile")
		if err != nil {
			return err
		}
		cur, err := inForce(ctx, exec, config.Target{Kind: config.KindCurrency, ID: q.Currency}, at, "/currency")
		if err != nil {
			return err
		}
		t := rev.Content.TaxProfile
		if t.RecoveryTreatment == config.RecoveryUnknown {
			return config.Reject(config.CodeTaxPolicyUnconfirmed, "/profile")
		}
		currency, err := kernel.NewCurrency(cur.Content.Currency.Code, cur.Content.Currency.MinorUnits)
		if err != nil {
			return err
		}
		rate, err := kernel.ParseDecimal(t.Rate, kernel.FactorFormat)
		out = api.TaxRate{Profile: q.Profile, ContentHash: rev.Hash, Currency: currency, Rate: rate, PriceBasis: t.PriceBasis,
			Recovery: t.RecoveryTreatment}
		return err
	})
	if err != nil {
		return api.TaxRate{}, err
	}
	return out, nil
}

// LookupUnitConversion returns a unit conversion a calculation may apply (Python
// foundation.unit.convert without the conversion, which the caller does with
// api.QuantityConversion.Convert): in force at the instant for exactly the SKU and specification
// revision asked (UNIT_SCOPE_MISMATCH otherwise), with both its units in force.
func (s Service) LookupUnitConversion(ctx context.Context, q api.UnitQuery) (api.QuantityConversion, error) {
	if err := q.Validate(); err != nil {
		return api.QuantityConversion{}, err
	}
	at := kernel.Instant(q.At)
	var out api.QuantityConversion
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
		key := postgres.Key(q.Conversion)
		rev, err := postgres.LoadRevision(ctx, exec, scope(ctx), key)
		if err != nil {
			return err
		}
		adopted := newAdoption(at, nil)
		if err = adopted.adoptRef(ctx, exec, key, rev, "/conversion"); err != nil {
			return err
		}
		u := rev.Content.UnitConversion
		switch {
		case u.SkuID != q.SkuID:
			return config.Reject(config.CodeUnitScopeMismatch, "/sku_id")
		case u.SpecRevision != q.SpecRevision:
			return config.Reject(config.CodeUnitScopeMismatch, "/spec_revision")
		}
		factor, err := kernel.ParseDecimal(u.Factor, kernel.FactorFormat)
		to := adopted.find(config.Target{Kind: config.KindUnit, ID: u.ToUnit})
		out = api.QuantityConversion{Conversion: q.Conversion, ContentHash: rev.Hash, From: kernel.UnitCode(u.FromUnit), To: kernel.UnitCode(u.ToUnit),
			Factor: factor, ToPrecision: to.Content.Unit.Precision}
		return err
	})
	if err != nil {
		return api.QuantityConversion{}, err
	}
	return out, nil
}

// GetSnapshot returns a FoundationSnapshot, audited. A snapshot is read as captured: later
// supersession, retirement or expiry of what it adopted never changes it or blocks reading it.
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
