package api

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// The list query (S7-06 read models): a page of the configurations of one kind, most recently
// changed first (kernel.ListCursor), audited as config.list under pol.read. It is how a person
// finds the ID and the revision a document must name. Like the lookup of configurations in
// force, it never returns a configuration body, and kind bank_account is PERMISSION_DENIED:
// which accounts exist follows the bank account read rules, which a list does not apply.

// ConfigQuery asks for a page of the configurations of Kind. Search keeps those whose ID
// contains the text, whatever its case. Active keeps those that have an active revision. After
// is the Next of the previous page, empty for the first. Limit is 0 (kernel.DefaultListPage) to
// kernel.MaxListPage.
type ConfigQuery struct {
	Kind   string
	Search string
	Active bool
	After  string
	Limit  int
}

// Validate checks the closed contract; the kind bank_account is refused by the read itself.
func (q ConfigQuery) Validate() error {
	switch {
	case !KindKnown(q.Kind):
		return schema("/kind")
	case q.Search != "" && !kernel.ValidSearchText(q.Search):
		return schema("/q")
	}
	if _, field := kernel.ListPosition(q.After, q.Limit); field != "" {
		return schema("/" + field)
	}
	return nil
}

// ConfigSummary is one configuration of a list. Status is the status of its latest revision
// (LastRevision). ActiveRevision is the active revision, 0 when there is none, and ValidFrom and
// ValidUntil are that revision's validity [ValidFrom, ValidUntil), ValidUntil zero when open: an
// active revision is in force only inside it.
type ConfigSummary struct {
	ConfigID       string
	RootVersion    int64
	LastRevision   int64
	Status         string
	ActiveRevision int64
	ValidFrom      time.Time
	ValidUntil     time.Time
	UpdatedAt      time.Time
}

// ConfigPage is a page of a ConfigQuery. Next is the After of the following page, empty on the last.
type ConfigPage struct {
	Kind    string
	Configs []ConfigSummary
	Next    string
}

// ListQueries is the audited list of the configurations of a kind (pol.read).
type ListQueries interface {
	ListConfigs(ctx context.Context, q ConfigQuery) (ConfigPage, error)
}
