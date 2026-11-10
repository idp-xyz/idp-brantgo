// Package snapshot is the FoundationSnapshot aggregate: the exact configuration revisions a
// calculation adopted at one instant, each with its content hash, and the hash of the whole
// snapshot (Python foundation.capture). It is append-only: a later publication, supersession or
// retirement never rewrites it, and its ID is never reused.
package snapshot

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// CodeBusinessKeyConflict refuses a snapshot ID that is taken.
const CodeBusinessKeyConflict = "BUSINESS_KEY_CONFLICT"

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
