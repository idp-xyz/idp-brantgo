package api

import (
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// ConfigAt v1 (S5R-D23, POL-INV-007; Context Map R50 POL->CNX) answers which revision of a
// configuration applies at an instant by its validity, whenever it was published: the revision whose
// validity [valid_from, valid_until) contains the instant, among every revision ever published
// (active, superseded or retired), and of several such the one published last. It is how CNX binds
// the export_tax_policy of an export to the revision valid at the export date (财税〔2012〕39号: the
// rate follows the 出口日期 on the declaration), so a revision published after the export, whose
// validity covers it, applies; ConfigGuard v1 and ResolveConfigs decide whether a revision is in
// force at an instant, which a revision published later never was.
//
// A retirement ends the configuration: from the instant a revision is retired on, no revision
// published before that retirement applies (it would otherwise come back through an older,
// superseded revision); a revision published after it applies again by its validity. A working or
// discarded revision was never published and never applies.
//
// Published revisions never change, so the answer for an instant depends only on publications
// later than the question, and neither form takes a lock: Queries.ConfigAt answers before the
// caller's transaction, and ConfigAtSQL runs the SQL function POL publishes in its schema
// (migrations/pol/000006) with the caller's executor inside its own transaction, in the caller's
// scope:
//
//	brant_pol.config_at_v1(p_kind text, p_config_id text, p_at timestamptz)
//	    RETURNS TABLE (known boolean, revision bigint, status text, code text, content_hash text)
//
// It always returns one row: known is false (code REFERENCE_INVALID) when the configuration does not
// exist in the caller's company; code is CONFIG_NOT_ACTIVE when no published revision applies at the
// instant; otherwise revision, status (now) and content_hash name the revision that applies.
//
// A caller that bound a revision earlier asks again to learn whether a revision published since
// covers the same instant (CNX's policy_superseded before a refund case is planned, S5R-D23): the
// answer is then another revision.
const (
	ConfigAtFunction = "brant_pol.config_at_v1"
	ConfigAtSQL      = `SELECT known, revision, status, code, content_hash FROM brant_pol.config_at_v1($1, $2, $3)`
)

// ConfigAtQuery asks for the revision of configuration (Kind, ConfigID) that applies at instant At.
type ConfigAtQuery struct {
	Kind     string
	ConfigID string
	At       time.Time
}

// Validate checks the form of the query.
func (q ConfigAtQuery) Validate() error {
	switch {
	case !KindKnown(q.Kind):
		return schema("/kind")
	case !ValidConfigID(q.ConfigID):
		return schema("/id")
	case q.At.IsZero():
		return schema("/at")
	}
	return nil
}

// ConfigAtArgs returns the arguments of ConfigAtSQL for q.
func ConfigAtArgs(q ConfigAtQuery) []any { return []any{q.Kind, q.ConfigID, kernel.Instant(q.At)} }

// RevisionAt is what POL says for a ConfigAtQuery. Code is empty when a revision applies at the
// instant, and then the other fields describe it: Ref the exact revision (a ConfigRef v1 to bind),
// Status its status now, its content hash, validity, publication and end, and its data (a bank
// account's number masked by the read rules of GetConfig). Code is REFERENCE_INVALID when the
// configuration does not exist in the caller's company or its read rules hide it, and
// CONFIG_NOT_ACTIVE when no published revision applies at the instant.
type RevisionAt struct {
	Ref         ConfigRef
	Status      string
	Code        string
	ContentHash string
	ValidFrom   time.Time
	ValidUntil  time.Time // zero: open-ended
	PublishedAt time.Time
	EndedAt     time.Time // when it was superseded or retired; zero while active
	Data        ConfigData
}

// Check returns nil when a revision applies and POL's rejection otherwise, at path of the caller's
// request.
func (r RevisionAt) Check(path string) error { return configAtCheck(r.Code, path) }

// ConfigAtVerdict is the SQL function's answer for Query. Ref.Revision is 0 unless a revision
// applies; Code is as RevisionAt's.
type ConfigAtVerdict struct {
	Query       ConfigAtQuery
	Known       bool
	Ref         ConfigRef
	Status      string
	Code        string
	ContentHash string
}

// ScanConfigAt reads the row of ConfigAtSQL for q through scan, such as a pgx row's Scan.
func ScanConfigAt(q ConfigAtQuery, scan func(dest ...any) error) (ConfigAtVerdict, error) {
	v := ConfigAtVerdict{Query: q, Ref: ConfigRef{Kind: q.Kind, ConfigID: q.ConfigID}}
	var revision *int64
	var status, code, hash *string
	if err := scan(&v.Known, &revision, &status, &code, &hash); err != nil {
		return ConfigAtVerdict{}, err
	}
	if revision != nil {
		v.Ref.Revision = *revision
	}
	v.Status, v.Code, v.ContentHash = orEmpty(status), orEmpty(code), orEmpty(hash)
	return v, nil
}

// Applies reports whether a revision applies at the instant.
func (v ConfigAtVerdict) Applies() bool { return v.Known && v.Code == "" }

// Check returns nil when a revision applies and POL's rejection otherwise, at path of the caller's
// request.
func (v ConfigAtVerdict) Check(path string) error { return configAtCheck(v.Code, path) }

func configAtCheck(code, path string) error {
	switch code {
	case "":
		return nil
	case kernel.CodeReferenceInvalid:
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	return Reject(code, path)
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
