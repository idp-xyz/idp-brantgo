package api

import "go.idp.xyz/idp-brant/internal/kernel"

// TradingProfileGuard v1 and AddressRoleGuard v1 (ADR-BR-004 option B, Context Map R13 PTY->TRD
// "PartyRef v1、TradingProfileRef v1、AddressRoleRef v1" with a qualification guard, guardLock true)
// let a downstream context re-check, inside its own transaction, that a trading profile revision
// and the address roles it adopts still serve new business, and keep them so until that
// transaction ends. PTY publishes them as SECURITY DEFINER functions in its schema with a fixed
// search_path (migrations/pty/000004_pty_profile_guards.sql):
//
//	brant_pty.guard_trading_profile_current_v1(p_party_id text, p_revision bigint)
//	    RETURNS TABLE (known boolean, status text, party_revision bigint, content_hash text, code text, stale text)
//	brant_pty.guard_address_role_current_v1(p_party_id text, p_party_revision bigint, p_address_id text)
//	    RETURNS TABLE (known boolean, role text, party_status text, current_revision bigint, code text, stale text)
//
// Each always returns one row, in the caller's scope (app.tenant_id and app.company_id, which
// bento.ScopeTransactor sets), and decides what ResolveTradingProfiles and ResolveAddressRoles
// decide before the transaction, with the same stale reasons. The trading profile guard locks, FOR
// SHARE in the caller's transaction, the revision row, then the party row, then the bound contacts'
// rows in contact ID order; the address role guard locks the address row of the party's confirmed
// revision, then the party row. trading_profile.publish of the next revision, party.revise,
// party.confirm, party.deactivate and contact.revise update one of those rows, so each either
// committed before the guard read it, and the verdict is stale, or waits until the caller's
// transaction ends.
//
// A downstream command locks its own aggregate root FOR UPDATE first and calls the guards after
// (ADR-BR-011), with its own executor; PTY comes after CAT, CST, INQ and POL in context code order.
// The profile is guarded once, its addresses one by one:
//
//	row := exec.QueryRow(ctx, ptyapi.TradingProfileGuardSQL, ptyapi.TradingProfileGuardArgs(profile)...)
//	verdict, err := ptyapi.ScanTradingProfileGuard(profile, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/trading_profile")
//	}
//	for _, a := range addresses { // in (party, revision, address) order
//		row := exec.QueryRow(ctx, ptyapi.AddressRoleGuardSQL, ptyapi.AddressRoleGuardArgs(a.Ref)...)
//		v, err := ptyapi.ScanAddressRoleGuard(a.Ref, row.Scan)
//		...v.Check(a.Path)
//	}
//
// The guards do not apply the caller's data scope (own, team): the downstream checks visibility
// when it resolves the references before its transaction. They do not check the settlement terms
// (POL revisions; ConfigGuard v1 under the downstream's own relationship with POL) or the party's
// screening (PartyScreeningGuard v1).
const (
	TradingProfileGuardFunction = "brant_pty.guard_trading_profile_current_v1"
	TradingProfileGuardSQL      = `SELECT known, status, party_revision, content_hash, code, stale FROM brant_pty.guard_trading_profile_current_v1($1, $2)`
	AddressRoleGuardFunction    = "brant_pty.guard_address_role_current_v1"
	AddressRoleGuardSQL         = `SELECT known, role, party_status, current_revision, code, stale FROM brant_pty.guard_address_role_current_v1($1, $2, $3)`
)

// TradingProfileGuardArgs returns the arguments of TradingProfileGuardSQL for ref, which names the
// exact published revision (Revision 0 is not one and the guard reports it unknown).
func TradingProfileGuardArgs(ref TradingProfileRef) []any { return []any{ref.PartyID, ref.Revision} }

// AddressRoleGuardArgs returns the arguments of AddressRoleGuardSQL for ref.
func AddressRoleGuardArgs(ref AddressRoleRef) []any {
	return []any{ref.PartyID, ref.PartyRevision, ref.AddressID}
}

// TradingProfileGuardVerdict is the guard's answer for Ref. Known is false when the party has no
// such published revision in the caller's company (Code REFERENCE_INVALID); a working revision is
// not published. Otherwise Status (active or superseded), PartyRevision and ContentHash are the
// revision's, which never change, and Code is empty when the revision serves new business, or
// REFERENCE_STALE with Stale saying why: StaleSuperseded, StalePartyUnavailable, StalePartyChanged
// or StaleContactChanged, decided in that order.
type TradingProfileGuardVerdict struct {
	Ref           TradingProfileRef
	Known         bool
	Status        string
	PartyRevision int64
	ContentHash   string
	Code          string
	Stale         string
}

// ScanTradingProfileGuard reads the row of TradingProfileGuardSQL for ref through scan, such as a
// pgx row's Scan.
func ScanTradingProfileGuard(ref TradingProfileRef, scan func(dest ...any) error) (TradingProfileGuardVerdict, error) {
	v := TradingProfileGuardVerdict{Ref: ref}
	var status, hash, code, stale *string
	var revision *int64
	if err := scan(&v.Known, &status, &revision, &hash, &code, &stale); err != nil {
		return TradingProfileGuardVerdict{}, err
	}
	v.Status, v.ContentHash, v.Code, v.Stale = orEmpty(status), orEmpty(hash), orEmpty(code), orEmpty(stale)
	if revision != nil {
		v.PartyRevision = *revision
	}
	return v, nil
}

// Current reports whether the revision serves new business.
func (v TradingProfileGuardVerdict) Current() bool { return v.Known && v.Code == "" }

// Check returns nil when the revision serves new business and, at path of the caller's request,
// REFERENCE_INVALID for an unknown revision and REFERENCE_STALE (409, refresh and resubmit)
// otherwise.
func (v TradingProfileGuardVerdict) Check(path string) error {
	return guardCheck(v.Known, v.Code, v.Stale, path)
}

// AddressRoleGuardVerdict is the guard's answer for Ref. Known is false when the party has no such
// confirmed revision with that address in the caller's company (Code REFERENCE_INVALID).
// Otherwise Role is the address's trade role in that revision, PartyStatus and CurrentRevision (the
// party's latest confirmed revision) describe the party now, and Code is empty when the party is
// confirmed in Ref.PartyRevision, or REFERENCE_STALE with Stale StalePartyUnavailable (not
// confirmed now) or StalePartyRevised (confirmed in a later revision).
type AddressRoleGuardVerdict struct {
	Ref             AddressRoleRef
	Known           bool
	Role            string
	PartyStatus     string
	CurrentRevision int64
	Code            string
	Stale           string
}

// ScanAddressRoleGuard reads the row of AddressRoleGuardSQL for ref through scan, such as a pgx
// row's Scan.
func ScanAddressRoleGuard(ref AddressRoleRef, scan func(dest ...any) error) (AddressRoleGuardVerdict, error) {
	v := AddressRoleGuardVerdict{Ref: ref}
	var role, status, code, stale *string
	var revision *int64
	if err := scan(&v.Known, &role, &status, &revision, &code, &stale); err != nil {
		return AddressRoleGuardVerdict{}, err
	}
	v.Role, v.PartyStatus, v.Code, v.Stale = orEmpty(role), orEmpty(status), orEmpty(code), orEmpty(stale)
	if revision != nil {
		v.CurrentRevision = *revision
	}
	return v, nil
}

// Current reports whether the address serves new business.
func (v AddressRoleGuardVerdict) Current() bool { return v.Known && v.Code == "" }

// Check returns nil when the address serves new business and, at path of the caller's request,
// REFERENCE_INVALID for an unknown address and REFERENCE_STALE (409, refresh and resubmit)
// otherwise.
func (v AddressRoleGuardVerdict) Check(path string) error {
	return guardCheck(v.Known, v.Code, v.Stale, path)
}

func guardCheck(known bool, code, stale, path string) error {
	switch {
	case !known || code == kernel.CodeReferenceInvalid:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case code != "":
		return check(code, stale, path)
	}
	return nil
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// PartyGuard v1 (ADR-BR-004 option B, Context Map R22 PTY->PUR "PartyRef v1", guardLock true) lets
// a downstream context re-check, inside its own transaction, that a party it adopts at a bound row
// version is still confirmed at that row version, and keeps it so until that transaction ends. PUR
// binds the supplier of a supply condition as SUP verified it ({party_id, row_version}). PTY
// publishes it as a SECURITY DEFINER function in its schema with a fixed search_path
// (migrations/pty/000005_pty_party_guard.sql):
//
//	brant_pty.guard_party_current_v1(p_party_id text, p_row_version bigint)
//	    RETURNS TABLE (known boolean, status text, row_version bigint, revision bigint, roles text[], code text, stale text)
//
// It always returns one row in the caller's scope and locks the party row FOR SHARE in the caller's
// transaction: party.save_draft, party.confirm, party.revise and party.deactivate update that row,
// so each either committed before the guard read it, and the verdict is stale, or waits until the
// caller's transaction ends. It decides what ResolveParties shows before the transaction (status and
// row version). A downstream command locks its own aggregate root FOR UPDATE first and calls the
// guard after, in context code order, with its own executor:
//
//	b := ptyapi.PartyBinding{PartyID: id, RowVersion: version}
//	v, err := ptyapi.ScanPartyGuard(b, exec.QueryRow(ctx, ptyapi.PartyGuardSQL, ptyapi.PartyGuardArgs(b)...).Scan)
//	if err == nil {
//		err = v.Check(path)
//	}
//
// The guard does not apply the caller's data scope (own, team) and does not judge roles: the
// downstream checks visibility when it resolves the party before its transaction, and the role it
// needs with HasRole.
const (
	PartyGuardFunction = "brant_pty.guard_party_current_v1"
	PartyGuardSQL      = `SELECT known, status, row_version, revision, roles, code, stale FROM brant_pty.guard_party_current_v1($1, $2)`
)

// PartyBinding is a party as a downstream fact binds it: at the row version its author saw.
type PartyBinding struct {
	PartyID    string
	RowVersion int64
}

// PartyGuardArgs returns the arguments of PartyGuardSQL for b.
func PartyGuardArgs(b PartyBinding) []any { return []any{b.PartyID, b.RowVersion} }

// PartyGuardVerdict is the guard's answer for Ref. Known is false when the party does not exist in
// the caller's company (Code REFERENCE_INVALID). Otherwise Status, RowVersion, Revision (its latest
// confirmed revision, 0 when never confirmed) and Roles describe the party now, and Code is empty
// when it is confirmed at Ref.RowVersion, or REFERENCE_STALE with Stale StalePartyUnavailable (not
// confirmed now) or StalePartyChanged (confirmed at another row version).
type PartyGuardVerdict struct {
	Ref        PartyBinding
	Known      bool
	Status     string
	RowVersion int64
	Revision   int64
	Roles      []string
	Code       string
	Stale      string
}

// ScanPartyGuard reads the row of PartyGuardSQL for b through scan, such as a pgx row's Scan.
func ScanPartyGuard(b PartyBinding, scan func(dest ...any) error) (PartyGuardVerdict, error) {
	v := PartyGuardVerdict{Ref: b}
	var status, code, stale *string
	var version, revision *int64
	var roles []string
	if err := scan(&v.Known, &status, &version, &revision, &roles, &code, &stale); err != nil {
		return PartyGuardVerdict{}, err
	}
	v.Status, v.Code, v.Stale, v.Roles = orEmpty(status), orEmpty(code), orEmpty(stale), roles
	if version != nil {
		v.RowVersion = *version
	}
	if revision != nil {
		v.Revision = *revision
	}
	return v, nil
}

// Current reports whether the party is confirmed at the bound row version.
func (v PartyGuardVerdict) Current() bool { return v.Known && v.Code == "" }

// HasRole reports whether the party has role now.
func (v PartyGuardVerdict) HasRole(role string) bool {
	for _, r := range v.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Check returns nil when the party is confirmed at the bound row version and, at path of the
// caller's request, REFERENCE_INVALID for an unknown party and REFERENCE_STALE (409, refresh and
// resubmit) otherwise.
func (v PartyGuardVerdict) Check(path string) error {
	return guardCheck(v.Known, v.Code, v.Stale, path)
}
