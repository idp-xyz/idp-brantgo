package plugin

import (
	"context"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// GuardFunction is brant_ext.guard_plugin_active_v1.
const GuardFunction = "brant_ext.guard_plugin_active_v1"

// GuardSQL locks the activation FOR SHARE and returns whether it is active for the profile.
const GuardSQL = `SELECT active, status, version, manifest_sha256, code FROM brant_ext.guard_plugin_active_v1($1, $2)`

// GuardArgs are the arguments of GuardSQL.
func GuardArgs(pluginID, profile string) []any { return []any{pluginID, profile} }

// GuardVerdict is one row of the guard.
type GuardVerdict struct {
	Active         bool
	Status         string
	Version        string
	ManifestSHA256 string
	Code           string
}

// ScanGuard reads a GuardSQL row.
func ScanGuard(scan func(dest ...any) error) (GuardVerdict, error) {
	var v GuardVerdict
	var status, version, sum, code *string
	if err := scan(&v.Active, &status, &version, &sum, &code); err != nil {
		return GuardVerdict{}, err
	}
	if status != nil {
		v.Status = *status
	}
	if version != nil {
		v.Version = *version
	}
	if sum != nil {
		v.ManifestSHA256 = *sum
	}
	if code != nil {
		v.Code = *code
	}
	return v, nil
}

// Require reports PLUGIN_NOT_ENABLED unless the row is active and names no code.
func (v GuardVerdict) Require(path string) error {
	if v.Active && v.Code == "" {
		return nil
	}
	return NotEnabled(path)
}

// RequireActive runs GuardSQL on exec inside the caller's transaction and refuses a plugin that
// is not active. The caller has already locked its own root FOR UPDATE and passed authorization,
// which holds the authority rows FOR SHARE; this call is the next FOR SHARE.
func RequireActive(ctx context.Context, exec bentopg.Executor, pluginID, profile, path string) error {
	v, err := ScanGuard(exec.QueryRow(ctx, GuardSQL, GuardArgs(pluginID, profile)...).Scan)
	if err != nil {
		return err
	}
	return v.Require(path)
}

// Active reports whether profile is the apparel profile the apparel plugin guards.
func Active(profile DocumentProfile) bool { return profile == ProfileApparel }

// Rejection is a small alias so callers can test codes without importing kernel twice.
func CodeOf(err error) string {
	rej, ok := kernel.AsRejection(err)
	if !ok {
		return ""
	}
	return rej.Code
}
