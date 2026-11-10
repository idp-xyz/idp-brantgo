// Package snapshot is the FoundationSnapshot aggregate: the exact configuration revisions a
// calculation adopted at one instant, each with its content hash, and the hash of the whole
// snapshot (Python foundation.capture). It is append-only: a later publication, supersession or
// retirement never rewrites it, and its ID is never reused.
//
// A snapshot is a pricing foundation: it adopts exactly one company profile and one pricing policy,
// every revision they and the other references depend on, no tax profile whose recovery treatment
// is unknown, no bank account, and no exchange rate observed longer ago than the policy allows.
package snapshot

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeBusinessKeyConflict refuses a snapshot ID that is taken.
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

// CheckAdoptable refuses a reference a snapshot never adopts: a bank account, whose number is
// sensitive (Python SENSITIVE: PERMISSION_DENIED whatever the caller's grants).
func CheckAdoptable(kind, path string) error {
	if config.Kind(kind) == config.KindBankAccount {
		return kernel.Reject(kernel.CodePermissionDenied, path, "A bank account is not a pricing foundation")
	}
	return nil
}

// Adopted is one revision a snapshot adopts and the path of the request reference that brought it,
// directly or as a dependency.
type Adopted struct {
	Entry Entry
	Rev   *config.Revision
	Path  string
}

// CheckFoundation applies the rules of a pricing foundation to everything a snapshot adopts at
// instant at, in the Python order: exactly one company profile and one pricing policy
// (DRAFT_NOT_READY), no tax profile whose recovery treatment is unknown (TAX_POLICY_UNCONFIRMED), no
// exchange rate observed more than the policy's max_fx_age_seconds before at (FX_STALE).
func CheckFoundation(adopted []Adopted, at time.Time) error {
	var companies int
	var policy *config.PricingPolicy
	policies := 0
	for _, a := range adopted {
		switch a.Rev.Content.Kind {
		case config.KindCompanyProfile:
			companies++
		case config.KindPricingPolicy:
			policies++
			policy = a.Rev.Content.PricingPolicy
		}
	}
	if companies != 1 || policies != 1 {
		return kernel.Reject(kernel.CodeDraftNotReady, "/args/refs", "One company profile and one pricing policy are required")
	}
	for _, a := range adopted {
		if t := a.Rev.Content.TaxProfile; t != nil && t.RecoveryTreatment == config.RecoveryUnknown {
			return config.Reject(config.CodeTaxPolicyUnconfirmed, a.Path)
		}
	}
	maxAge := time.Duration(policy.MaxFXAgeSeconds) * time.Second
	for _, a := range adopted {
		x := a.Rev.Content.FXRate
		if x == nil {
			continue
		}
		observed, err := kernel.ParseTimestamp(x.ObservedAt)
		if err != nil {
			return err
		}
		if at.Sub(observed) > maxAge {
			return config.Reject(config.CodeFXStale, a.Path)
		}
	}
	return nil
}

// Entry is one adopted revision.
type Entry struct {
	Kind        string `json:"kind"`
	ConfigID    string `json:"id"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"content_hash"`
}

// Snapshot is the aggregate root. Entries are sorted by kind, ID and revision.
type Snapshot struct {
	ID         string
	Purpose    string
	CapturedAt time.Time
	CapturedBy kernel.ActorRef
	Epoch      int64
	Entries    []Entry
	Hash       string
}

// CheckNew refuses an ID that is taken (Python BUSINESS_KEY_CONFLICT).
func CheckNew(taken bool) error {
	if taken {
		return kernel.RejectAs(CodeBusinessKeyConflict, kernel.CategoryConflict, "/args/id", "")
	}
	return nil
}

// Capture returns the snapshot of entries, each revision once, with its hash: the SHA-256 of the
// canonical JSON of purpose, instant and sorted entries.
func Capture(id, purpose string, at time.Time, by kernel.ActorRef, epoch int64, entries []Entry) (*Snapshot, error) {
	sorted := slices.Clone(entries)
	slices.SortFunc(sorted, func(a, b Entry) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.ConfigID, b.ConfigID), cmp.Compare(a.Revision, b.Revision))
	})
	sorted = slices.CompactFunc(sorted, func(a, b Entry) bool {
		return a.Kind == b.Kind && a.ConfigID == b.ConfigID && a.Revision == b.Revision
	})
	doc, err := json.Marshal(struct {
		Purpose string  `json:"purpose"`
		AsOf    string  `json:"as_of"`
		Entries []Entry `json:"entries"`
	}{purpose, kernel.FormatTimestamp(at), sorted})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(doc)
	return &Snapshot{ID: id, Purpose: purpose, CapturedAt: at, CapturedBy: by, Epoch: epoch, Entries: sorted,
		Hash: hex.EncodeToString(sum[:])}, nil
}
