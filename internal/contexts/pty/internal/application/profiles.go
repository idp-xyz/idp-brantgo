package application

import (
	"context"
	"slices"
	"strconv"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/api"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/lifecycle"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/party"
	"go.idp.xyz/idp-brant/internal/contexts/pty/internal/domain/profile"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// VerifyServiceRole records a verified revision of a confirmed supplier's service role (human only;
// T02 spec 13.1). It locks the party root FOR UPDATE and increments its row version; the evidence
// is read from EVD before PTY's transaction (Context Map R61).
func (s Service) VerifyServiceRole(ctx context.Context, cmd api.VerifyServiceRole) (api.Receipt, error) {
	var role party.ServiceRole
	validation := cmd.Validate()
	if validation == nil {
		from, _ := kernel.ParseTimestamp(cmd.ValidFrom)
		until, _ := kernel.ParseTimestamp(cmd.ValidUntil)
		role = party.ServiceRole{ID: cmd.ServiceRoleID, Kind: party.ServiceKind(cmd.Role), ServiceCode: cmd.ServiceCode, Origin: cmd.Origin,
			Destination: cmd.Destination, Validity: kernel.ValidityInterval{From: kernel.Instant(from), Until: kernel.Instant(until)},
			EvidenceIDs: cmd.EvidenceIDs}
	}
	args := cmd
	args.IdempotencyKey, args.ExpectedVersion = "", 0
	h := &partyCommand{s: s, id: cmd.PartyID, evidence: cmd.EvidenceIDs}
	h.apply = func(h *partyCommand, ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		if err := lifecycle.Check(h.record(), h.visible(d), cmd.ExpectedVersion, lifecycle.Confirmed); err != nil {
			return action.Outcome{}, err
		}
		sc, now := h.scope(ctx), s.Clock.Now()
		latest, err := postgres.LatestServiceRole(ctx, exec, sc, cmd.PartyID, cmd.ServiceRoleID)
		if err != nil {
			return action.Outcome{}, err
		}
		verified, err := h.p.VerifyServiceRole(role, latest, now)
		if err != nil {
			return action.Outcome{}, err
		}
		// The verifier must be able to read every source it cites (Python _sources).
		if err = h.checkEvidence(); err != nil {
			return action.Outcome{}, err
		}
		verified.VerifiedBy, verified.Epoch, verified.At = d.Actor, d.Epoch, now
		if err = postgres.InsertServiceRole(ctx, exec, sc, cmd.PartyID, verified); err != nil {
			return action.Outcome{}, err
		}
		h.p.Touch()
		out, err := h.save(ctx, exec, &action.Event{Type: api.EventServiceRoleVerified, Fields: map[string]any{
			"service_role_id": verified.ID, "service_role_revision": verified.Revision, "role": string(verified.Kind),
			"service_code": verified.ServiceCode, "origin": verified.Origin, "destination": verified.Destination,
			"valid_from": kernel.FormatTimestamp(verified.Validity.From), "valid_until": kernel.FormatTimestamp(verified.Validity.Until)}})
		out.Status = api.StatusVerified
		return out, err
	}
	return s.run(ctx, action.Command{Name: api.CommandVerifyServiceRole, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID},
		args, validation, &preparedParty{h})
}

// profileCommand locks a trading profile root FOR UPDATE and applies one command. The party and
// the bound contacts are then held FOR SHARE, after the authority guard (ADR-BR-011); the
// settlement terms are read from POL before PTY's transaction (R60 carries no guard lock).
type profileCommand struct {
	s       Service
	partyID string
	root    *profile.Root
	from    int64
	prepare func(ctx context.Context) error
	apply   func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error)
}

func (h *profileCommand) Lock(ctx context.Context, exec bentopg.Executor) error {
	var err error
	h.from = 0
	h.root, err = postgres.LoadProfile(ctx, exec, scope(ctx), h.partyID, postgres.ForUpdate)
	if h.root != nil {
		h.from = h.root.Version
	}
	return err
}

func (h *profileCommand) Apply(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
	return h.apply(ctx, exec, d)
}

func (h *profileCommand) Prepare(ctx context.Context) error { return h.prepare(ctx) }

// party holds the party FOR SHARE and returns what PTY says about it to the actor.
func (h *profileCommand) party(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (profile.PartyFact, error) {
	p, err := postgres.LoadParty(ctx, exec, scope(ctx), h.partyID, postgres.ForShare)
	if err != nil || p == nil || !d.Sees(p.OwnerActor, p.OwnerTeam) {
		return profile.PartyFact{}, err
	}
	return profile.PartyFact{Visible: true, Confirmed: p.Status == lifecycle.Confirmed, Revision: p.Revision}, nil
}

// references holds the content's contacts FOR SHARE, in ID order, and checks the party and the
// contacts (profile.CheckReferences).
func (h *profileCommand) references(ctx context.Context, exec bentopg.Executor, d authorization.Decision, c profile.Content, party profile.PartyFact) error {
	ids := make([]string, len(c.Contacts))
	for i, ref := range c.Contacts {
		ids[i] = ref.ContactID
	}
	contacts, err := postgres.ContactsLocked(ctx, exec, scope(ctx), ids, postgres.ForShare)
	if err != nil {
		return err
	}
	f := profile.Facts{Party: party, Contacts: map[string]profile.ContactFact{}}
	for _, ct := range contacts {
		if d.Sees(ct.OwnerActor, ct.OwnerTeam) {
			f.Contacts[ct.ID] = profile.ContactFact{PartyID: ct.PartyID, Confirmed: ct.Status == lifecycle.Confirmed, RowVersion: ct.RowVersion}
		}
	}
	return profile.CheckReferences(h.partyID, c, f)
}

// termsRefs returns the POL references of the settlement terms of c.
func termsRefs(c profile.Content) []polapi.ConfigRef {
	return []polapi.ConfigRef{polRef(c.PaymentTerms), polRef(c.TradeTerms)}
}

func polRef(r profile.TermsRef) polapi.ConfigRef {
	return polapi.ConfigRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

// checkTerms requires both settlement terms to be the exact POL revisions of their kinds and in
// force at instant at, as POL decided before PTY's transaction: REFERENCE_INVALID when POL does
// not know the revision in the company, POL's CONFIG_NOT_ACTIVE or CONFIG_EXPIRED when it is not in
// force (Python active()).
func checkTerms(c profile.Content, resolved map[polapi.ConfigRef]polapi.ResolvedConfig, at time.Time) error {
	for _, t := range []struct {
		ref  profile.TermsRef
		path string
	}{{c.PaymentTerms, "/args/payment_terms"}, {c.TradeTerms, "/args/trade_terms"}} {
		r, ok := resolved[polRef(t.ref)]
		if !ok {
			return kernel.Reject(kernel.CodeReferenceInvalid, t.path, "")
		}
		if err := r.Check(t.path); err != nil {
			return err
		}
		if !(kernel.ValidityInterval{From: r.ValidFrom, Until: r.ValidUntil}).Contains(at) {
			return polapi.Reject(polapi.CodeConfigExpired, t.path)
		}
	}
	return nil
}

func (h *profileCommand) outcome(rev *profile.Revision, events ...action.Event) action.Outcome {
	return action.Outcome{ObjectID: h.partyID, RowVersion: h.root.Version, Status: string(rev.Status), Events: events}
}

// SaveTradingProfile saves the whole working revision of a party's trading profile (Python
// foundation.save kind=party_profile).
func (s Service) SaveTradingProfile(ctx context.Context, cmd api.SaveTradingProfile) (api.Receipt, error) {
	contacts := make([]profile.ContactRef, len(cmd.ContactRefs))
	for i, c := range cmd.ContactRefs {
		contacts[i] = profile.ContactRef{ContactID: c.ContactID, RowVersion: c.RowVersion}
	}
	content := profile.Content{PartyRevision: cmd.PartyRevision, Contacts: contacts,
		PaymentTerms: profile.TermsRef{Kind: cmd.PaymentTerms.Kind, ConfigID: cmd.PaymentTerms.ConfigID, Revision: cmd.PaymentTerms.Revision},
		TradeTerms:   profile.TermsRef{Kind: cmd.TradeTerms.Kind, ConfigID: cmd.TradeTerms.ConfigID, Revision: cmd.TradeTerms.Revision},
		Owner:        profile.Owner{ActorID: cmd.Owner.ActorID, TeamID: cmd.Owner.TeamID}}
	h := &profileCommand{s: s, partyID: cmd.PartyID}
	var terms map[polapi.ConfigRef]polapi.ResolvedConfig
	h.prepare = func(ctx context.Context) (err error) {
		terms, err = s.Configs.ResolveConfigs(ctx, termsRefs(content), s.Clock.Now())
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		party, err := h.party(ctx, exec, d)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = profile.CheckSave(h.root, party.Visible, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		if err = h.references(ctx, exec, d, content, party); err != nil {
			return action.Outcome{}, err
		}
		now := s.Clock.Now()
		if err = checkTerms(content, terms, now); err != nil {
			return action.Outcome{}, err
		}
		created := h.root == nil
		if created {
			h.root = profile.New(cmd.PartyID)
		}
		rev, fresh := h.root.Save(content, d.Actor, d.Epoch, now)
		sc := scope(ctx)
		if created {
			err = postgres.InsertProfile(ctx, exec, sc, h.root, now)
		} else {
			err = postgres.UpdateProfile(ctx, exec, sc, h.root, h.from, now)
		}
		if err == nil && fresh {
			err = postgres.InsertProfileRevision(ctx, exec, sc, cmd.PartyID, rev)
		} else if err == nil {
			err = postgres.UpdateProfileRevision(ctx, exec, sc, cmd.PartyID, rev)
		}
		return h.outcome(rev), err
	}
	return s.run(ctx, action.Command{Name: api.CommandSaveTradingProfile, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID},
		struct {
			ID      string
			Content profile.Content
		}{cmd.PartyID, profile.NewContent(content.PartyRevision, content.PaymentTerms, content.TradeTerms, content.Owner, content.Contacts)},
		cmd.Validate(), h)
}

// PublishTradingProfile makes the working revision of a party's trading profile active and
// supersedes the active one (Python foundation.publish kind=party_profile): a human other than the
// revision's last editor, with the party's confirmed revision, the contacts' row versions and the
// settlement terms still current, on evidence the publisher may read.
func (s Service) PublishTradingProfile(ctx context.Context, cmd api.PublishTradingProfile) (api.Receipt, error) {
	h := &profileCommand{s: s, partyID: cmd.PartyID}
	var (
		terms    map[polapi.ConfigRef]polapi.ResolvedConfig
		evidence map[string]evdapi.SourceRef
	)
	h.prepare = func(ctx context.Context) error {
		// The working revision's settlement terms are read here to ask POL; Apply requires the
		// revision it publishes to name the same ones.
		var content *profile.Content
		err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, _ authorization.Decision) error {
			root, err := postgres.LoadProfile(ctx, exec, scope(ctx), cmd.PartyID, postgres.NoLock)
			if err == nil && root != nil && root.Working != nil {
				content = &root.Working.Content
			}
			return err
		})
		if err != nil {
			return err
		}
		if content != nil {
			if terms, err = s.Configs.ResolveConfigs(ctx, termsRefs(*content), s.Clock.Now()); err != nil {
				return err
			}
		}
		evidence, err = s.Sources.ResolveSources(ctx, cmd.EvidenceIDs)
		return err
	}
	h.apply = func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) (action.Outcome, error) {
		party, err := h.party(ctx, exec, d)
		if err != nil {
			return action.Outcome{}, err
		}
		if err = profile.Check(h.root, party.Visible, cmd.ExpectedVersion); err != nil {
			return action.Outcome{}, err
		}
		w, err := h.root.Publishable(cmd.Revision, d.Actor)
		if err != nil {
			return action.Outcome{}, err
		}
		// The references of the revision must still be current: the party in the bound revision,
		// each contact at its bound row version, the settlement terms in force (Python publish
		// re-validates the content). Rejections point at the revision published.
		if err = h.references(ctx, exec, d, w.Content, party); err != nil {
			return action.Outcome{}, profile.Repath(err, "/args/revision")
		}
		for _, ref := range termsRefs(w.Content) {
			if _, ok := terms[ref]; !ok {
				return action.Outcome{}, profile.Reject(profile.CodeReferenceStale, "/args/revision", "The revision changed while it was published")
			}
		}
		now := s.Clock.Now()
		if err = checkTerms(w.Content, terms, now); err != nil {
			return action.Outcome{}, profile.Repath(err, "/args/revision")
		}
		for i, id := range cmd.EvidenceIDs {
			if _, ok := evidence[id]; !ok {
				return action.Outcome{}, kernel.Reject(kernel.CodeReferenceInvalid, "/args/evidence_ids/"+strconv.Itoa(i), "Source is not accessible")
			}
		}
		superseded := h.root.Publish(d.Actor, d.Epoch, cmd.EvidenceIDs, now)
		sc, active := scope(ctx), h.root.Active
		fields := map[string]any{"party_id": cmd.PartyID, "revision": active.Number, "root_version": h.root.Version,
			"content_hash": active.Hash, "party_revision": active.Content.PartyRevision}
		if superseded != nil {
			// The superseded revision is written first: there is one active revision at a time.
			fields["superseded_revision"] = superseded.Number
			err = postgres.UpdateProfileRevision(ctx, exec, sc, cmd.PartyID, superseded)
		}
		if err == nil {
			err = postgres.UpdateProfileRevision(ctx, exec, sc, cmd.PartyID, active)
		}
		if err == nil {
			err = postgres.UpdateProfile(ctx, exec, sc, h.root, h.from, now)
		}
		return h.outcome(active, action.Event{Type: api.EventTradingProfilePublished, AggregateType: "TradingProfile",
			AggregateID: cmd.PartyID, AggregateVersion: h.root.Version, Fields: fields}), err
	}
	return s.run(ctx, action.Command{Name: api.CommandPublishTradingProfile, IdempotencyKey: cmd.IdempotencyKey, ObjectID: cmd.PartyID,
		Reason: cmd.Reason}, struct {
		ID       string
		Revision int64
		Evidence []string
		Reason   string
	}{cmd.PartyID, cmd.Revision, cmd.EvidenceIDs, cmd.Reason}, cmd.Validate(), h)
}

// GetTradingProfile returns a party's trading profile and all its revisions, audited.
func (s Service) GetTradingProfile(ctx context.Context, partyID string) (api.TradingProfileView, error) {
	var v api.TradingProfileView
	err := s.Ledger.Read(ctx, CommandGetTradingProfile, partyID, func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		p, err := postgres.LoadParty(ctx, exec, sc, partyID, postgres.NoLock)
		if err != nil {
			return err
		}
		var root *profile.Root
		if p != nil && d.Sees(p.OwnerActor, p.OwnerTeam) {
			if root, err = postgres.LoadProfile(ctx, exec, sc, partyID, postgres.NoLock); err != nil {
				return err
			}
		}
		if root == nil {
			return kernel.Reject(kernel.CodeNotFound, "/id", "")
		}
		v = api.TradingProfileView{PartyID: partyID, RootVersion: root.Version, Revisions: []api.TradingProfileRevisionView{}}
		if root.Working != nil {
			v.WorkingRevision = root.Working.Number
		}
		if root.Active != nil {
			v.ActiveRevision = root.Active.Number
		}
		revisions, err := postgres.ProfileRevisions(ctx, exec, sc, partyID)
		for _, r := range revisions {
			v.Revisions = append(v.Revisions, profileRevisionView(r))
		}
		return err
	})
	return v, err
}

func termsView(r profile.TermsRef) api.TermsRef {
	return api.TermsRef{Kind: r.Kind, ConfigID: r.ConfigID, Revision: r.Revision}
}

func contactViews(refs []profile.ContactRef) []api.PinnedContact {
	out := make([]api.PinnedContact, len(refs))
	for i, c := range refs {
		out[i] = api.PinnedContact{ContactID: c.ContactID, RowVersion: c.RowVersion}
	}
	return out
}

func profileRevisionView(r *profile.Revision) api.TradingProfileRevisionView {
	c := r.Content
	v := api.TradingProfileRevisionView{Revision: r.Number, Status: string(r.Status), ContentHash: r.Hash, PartyRevision: c.PartyRevision,
		PaymentTerms: termsView(c.PaymentTerms), TradeTerms: termsView(c.TradeTerms), Owner: api.OwnerRef{ActorID: c.Owner.ActorID, TeamID: c.Owner.TeamID},
		ContactRefs: contactViews(c.Contacts), SavedBy: r.SavedBy, SavedAt: r.SavedAt, EndedAt: r.EndedAt}
	if p := r.Publication; p != nil {
		v.PublishedBy, v.PublishedAt, v.EvidenceIDs = p.By, p.At, p.EvidenceIDs
	}
	return v
}

func addressViews(addresses []party.Address) []api.Address {
	out := make([]api.Address, len(addresses))
	for i, a := range addresses {
		out[i] = api.Address{AddressID: a.ID, Role: string(a.Role), Address: a.Text, Country: a.Country}
	}
	return out
}

func serviceRoleView(r party.ServiceRole) api.ServiceRoleView {
	return api.ServiceRoleView{ServiceRoleID: r.ID, Revision: r.Revision, PartyRevision: r.PartyRevision, Role: string(r.Kind),
		ServiceCode: r.ServiceCode, Origin: r.Origin, Destination: r.Destination, ValidFrom: r.Validity.From, ValidUntil: r.Validity.Until,
		EvidenceIDs: r.EvidenceIDs, VerifiedBy: r.VerifiedBy, VerifiedAt: r.At, AuthorityEpoch: r.Epoch}
}

// visibleParties returns the parties among ids the actor of d sees, as they are now.
func visibleParties(ctx context.Context, exec bentopg.Executor, d authorization.Decision, ids []string) (map[string]*party.Party, error) {
	parties, err := postgres.Parties(ctx, exec, scope(ctx), slices.Compact(slices.Sorted(slices.Values(ids))))
	out := map[string]*party.Party{}
	for _, p := range parties {
		if d.Sees(p.OwnerActor, p.OwnerTeam) {
			out[p.ID] = p
		}
	}
	return out, err
}

func stale(reason string) (string, string) {
	if reason == "" {
		return "", ""
	}
	return api.CodeReferenceStale, reason
}

// ResolveAddressRoles answers AddressRoleRef v1 lookups of downstream contexts.
func (s Service) ResolveAddressRoles(ctx context.Context, refs []api.AddressRoleRef) (map[api.AddressRoleRef]api.ResolvedAddressRole, error) {
	out := map[api.AddressRoleRef]api.ResolvedAddressRole{}
	if err := api.ValidateLookup(len(refs)); err != nil || len(refs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		ids, keys := make([]string, len(refs)), make([]postgres.AddressKey, len(refs))
		for i, r := range refs {
			ids[i], keys[i] = r.PartyID, postgres.AddressKey{PartyID: r.PartyID, Revision: r.PartyRevision, AddressID: r.AddressID}
		}
		parties, err := visibleParties(ctx, exec, d, ids)
		if err != nil {
			return err
		}
		addresses, err := postgres.RevisionAddresses(ctx, exec, scope(ctx), keys)
		for i, ref := range refs {
			p, a := parties[ref.PartyID], addresses[keys[i]]
			if p == nil || a.ID == "" {
				continue
			}
			var reason string
			switch {
			case p.Status != lifecycle.Confirmed:
				reason = api.StalePartyUnavailable
			case p.Revision != ref.PartyRevision:
				reason = api.StalePartyRevised
			}
			r := api.ResolvedAddressRole{Ref: ref, Role: string(a.Role), Address: a.Text, Country: a.Country, PartyStatus: string(p.Status),
				CurrentRevision: p.Revision}
			r.Code, r.Stale = stale(reason)
			out[ref] = r
		}
		return err
	})
	return out, err
}

// ResolveServiceRoles answers ServiceRoleRef v1 lookups of downstream contexts at instant at.
func (s Service) ResolveServiceRoles(ctx context.Context, refs []api.ServiceRoleRef, at time.Time) (map[api.ServiceRoleRef]api.ResolvedServiceRole, error) {
	out := map[api.ServiceRoleRef]api.ResolvedServiceRole{}
	if err := api.ValidateLookup(len(refs)); err != nil || len(refs) == 0 {
		return out, err
	}
	if at.IsZero() {
		return out, kernel.Reject(kernel.CodeSchemaInvalid, "/at", "")
	}
	at = kernel.Instant(at)
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		ids, keys := make([]string, len(refs)), make([]postgres.ServiceRoleKey, len(refs))
		for i, r := range refs {
			ids[i], keys[i] = r.PartyID, postgres.ServiceRoleKey{PartyID: r.PartyID, ServiceRoleID: r.ServiceRoleID, Revision: r.Revision}
		}
		parties, err := visibleParties(ctx, exec, d, ids)
		if err != nil {
			return err
		}
		roles, err := postgres.ServiceRolesByKey(ctx, exec, scope(ctx), keys)
		for i, ref := range refs {
			p, stored := parties[ref.PartyID], roles[keys[i]]
			if p == nil || stored.PartyID == "" {
				continue
			}
			role := stored.Role
			var reason string
			switch {
			case stored.Latest != role.Revision:
				reason = api.StaleSuperseded
			case p.Status != lifecycle.Confirmed || p.Revision != role.PartyRevision:
				reason = api.StalePartyChanged
			case !role.Validity.Contains(at):
				reason = api.StaleOutsideValidity
			}
			r := api.ResolvedServiceRole{Ref: ref, Role: string(role.Kind), ServiceCode: role.ServiceCode, Origin: role.Origin,
				Destination: role.Destination, ValidFrom: role.Validity.From, ValidUntil: role.Validity.Until, PartyRevision: role.PartyRevision,
				LatestRevision: stored.Latest}
			r.Code, r.Stale = stale(reason)
			out[ref] = r
		}
		return err
	})
	return out, err
}

// ResolveTradingProfiles answers TradingProfileRef v1 lookups of downstream contexts.
func (s Service) ResolveTradingProfiles(ctx context.Context, refs []api.TradingProfileRef) (map[api.TradingProfileRef]api.ResolvedTradingProfile, error) {
	out := map[api.TradingProfileRef]api.ResolvedTradingProfile{}
	if err := api.ValidateLookup(len(refs)); err != nil || len(refs) == 0 {
		return out, err
	}
	err := s.Ledger.Lookup(ctx, "", func(ctx context.Context, exec bentopg.Executor, d authorization.Decision) error {
		sc := scope(ctx)
		ids := make([]string, len(refs))
		for i, r := range refs {
			ids[i] = r.PartyID
		}
		parties, err := visibleParties(ctx, exec, d, ids)
		if err != nil {
			return err
		}
		active, err := postgres.ActiveProfiles(ctx, exec, sc, ids)
		if err != nil {
			return err
		}
		keys := make([]postgres.ProfileKey, len(refs))
		for i, r := range refs {
			keys[i] = postgres.ProfileKey{PartyID: r.PartyID, Revision: r.Revision}
			if r.Revision == 0 {
				keys[i].Revision = active[r.PartyID]
			}
		}
		revisions, err := postgres.PublishedProfiles(ctx, exec, sc, keys)
		if err != nil {
			return err
		}
		var contactIDs []string
		for _, rev := range revisions {
			for _, c := range rev.Content.Contacts {
				contactIDs = append(contactIDs, c.ContactID)
			}
		}
		contacts, err := postgres.Contacts(ctx, exec, sc, slices.Compact(slices.Sorted(slices.Values(contactIDs))))
		facts := map[string]profile.ContactFact{}
		for _, c := range contacts {
			facts[c.ID] = profile.ContactFact{PartyID: c.PartyID, Confirmed: c.Status == lifecycle.Confirmed, RowVersion: c.RowVersion}
		}
		for i, ref := range refs {
			p, rev := parties[ref.PartyID], revisions[keys[i]]
			if p == nil || rev == nil {
				continue
			}
			use := profile.Usability(rev, profile.PartyFact{Visible: true, Confirmed: p.Status == lifecycle.Confirmed, Revision: p.Revision}, facts)
			c := rev.Content
			r := api.ResolvedTradingProfile{Ref: api.TradingProfileRef{PartyID: ref.PartyID, Revision: rev.Number}, Status: string(rev.Status),
				ActiveRevision: active[ref.PartyID], ContentHash: rev.Hash, PartyRevision: c.PartyRevision, PaymentTerms: termsView(c.PaymentTerms),
				TradeTerms: termsView(c.TradeTerms), Owner: api.OwnerRef{ActorID: c.Owner.ActorID, TeamID: c.Owner.TeamID},
				ContactRefs: contactViews(c.Contacts)}
			r.Code, r.Stale = stale(string(use))
			out[ref] = r
		}
		return err
	})
	return out, err
}
