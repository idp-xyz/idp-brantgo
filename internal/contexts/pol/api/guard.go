package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ConfigGuard v1 (ADR-BR-004 option B) lets a downstream context re-check, inside its own
// transaction, that a configuration revision it adopts is in force, and keeps it so until that
// transaction ends. POL publishes it as a SECURITY DEFINER function in its schema with a fixed
// search_path (migrations/pol/000002_pol_config_guard.sql):
//
//	brant_pol.guard_config_in_force_v1(p_kind text, p_config_id text, p_revision bigint, p_at timestamptz)
//	    RETURNS TABLE (known boolean, status text, code text, content_hash text)
//
// It always returns one row. It locks the revision row FOR SHARE in the caller's transaction, in
// the caller's scope (app.tenant_id and app.company_id, which bento.ScopeTransactor sets), so every
// POL command that would end the revision (publishing the next one supersedes it, retiring it
// retires it) either committed before the guard read the row, and the guard reports it not in
// force, or waits until the caller's transaction ends. The verdict is the revision's current state,
// not its history: it is in force when it is active now, was published at or before at, and its
// validity contains at.
//
// A downstream command locks its own aggregate root FOR UPDATE first and calls the guard after
// (ADR-BR-011), with its own executor:
//
//	row := exec.QueryRow(ctx, polapi.ConfigGuardSQL, polapi.ConfigGuardArgs(ref, now)...)
//	verdict, err := polapi.ScanConfigGuard(ref, row.Scan)
//	if err == nil {
//		err = verdict.Check("/args/fx_rate")
//	}
//
// The guard decides one exact revision. A configuration adopted with its dependencies, such as a
// pricing policy and the tax profile revision it names, is guarded entry by entry: the entries of
// the FoundationSnapshot that adopted them.
const (
	ConfigGuardFunction = "brant_pol.guard_config_in_force_v1"
	ConfigGuardSQL      = `SELECT known, status, code, content_hash FROM brant_pol.guard_config_in_force_v1($1, $2, $3, $4)`
)

// ConfigGuardArgs returns the arguments of ConfigGuardSQL for ref at instant at.
func ConfigGuardArgs(ref ConfigRef, at time.Time) []any {
	return []any{ref.Kind, ref.ConfigID, ref.Revision, kernel.Instant(at)}
}

// ConfigGuardVerdict is the guard's answer for Ref. Known is false when no such revision exists in
// the caller's company (Code REFERENCE_INVALID); otherwise Status is the revision's status and Code
// is empty when it is in force, CONFIG_NOT_ACTIVE when it is not active or was published after the
// instant, and CONFIG_EXPIRED when its validity does not contain the instant.
type ConfigGuardVerdict struct {
	Ref         ConfigRef
	Known       bool
	Status      string
	Code        string
	ContentHash string
}

// ScanConfigGuard reads the row of ConfigGuardSQL for ref through scan, such as a pgx row's Scan.
func ScanConfigGuard(ref ConfigRef, scan func(dest ...any) error) (ConfigGuardVerdict, error) {
	v := ConfigGuardVerdict{Ref: ref}
	var status, code, hash *string
	if err := scan(&v.Known, &status, &code, &hash); err != nil {
		return ConfigGuardVerdict{}, err
	}
	for _, f := range []struct {
		from *string
		to   *string
	}{{status, &v.Status}, {code, &v.Code}, {hash, &v.ContentHash}} {
		if f.from != nil {
			*f.to = *f.from
		}
	}
	return v, nil
}

// InForce reports whether the revision is in force.
func (v ConfigGuardVerdict) InForce() bool { return v.Known && v.Code == "" }

// Check returns nil when the revision is in force and POL's rejection otherwise, at path of the
// caller's request.
func (v ConfigGuardVerdict) Check(path string) error {
	switch {
	case !v.Known:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case v.Code != "":
		return Reject(v.Code, path)
	}
	return nil
}
