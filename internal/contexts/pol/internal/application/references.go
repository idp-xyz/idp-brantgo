package application

import (
	"context"
	"time"

	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/adapters/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/config"
	"go.idp.xyz/idp-brant/internal/contexts/pol/internal/domain/snapshot"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// facts reads, at instant at, the configurations content c refers to, and the scope's company.
// References within POL are read in POL's own transaction without locks: a reference that a
// concurrent command ends is caught when the content is used, which checks every dependency again.
func facts(ctx context.Context, exec bentopg.Executor, c config.Content, at time.Time) (config.Facts, error) {
	f := config.Facts{Company: string(scope(ctx).Company), Configs: map[config.Target]config.Fact{}}
	for _, ref := range c.References() {
		fact, known, err := resolve(ctx, exec, ref.Target(), at)
		if err != nil {
			return f, err
		}
		if known {
			f.Configs[ref.Target()] = fact
		}
	}
	return f, nil
}

// resolve returns the state of t at instant at and whether it is configured: for a target by ID
// the revision published then, for an exact target that revision.
func resolve(ctx context.Context, exec bentopg.Executor, t config.Target, at time.Time) (config.Fact, bool, error) {
	sc := scope(ctx)
	var rev *config.Revision
	if t.Revision == 0 {
		var known bool
		var err error
		if rev, known, err = postgres.PublishedAt(ctx, exec, sc, string(t.Kind), t.ID, at); err != nil || !known {
			return config.Fact{}, false, err
		}
	} else {
		var err error
		if rev, err = postgres.LoadRevision(ctx, exec, sc, postgres.Key{Kind: string(t.Kind), ConfigID: t.ID, Revision: t.Revision}); err != nil || rev == nil {
			return config.Fact{}, false, err
		}
	}
	return config.Fact{Rev: rev, Refused: config.InForce(rev, at)}, true, nil
}

// inForce returns the revision of t in force at instant at, or the rejection at path: REFERENCE_INVALID
// when t is not configured, otherwise what InForce says.
func inForce(ctx context.Context, exec bentopg.Executor, t config.Target, at time.Time, path string) (*config.Revision, error) {
	fact, known, err := resolve(ctx, exec, t, at)
	switch {
	case err != nil:
		return nil, err
	case !known:
		return nil, kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	case fact.Refused != nil:
		return nil, config.Repath(fact.Refused, path)
	}
	return fact.Rev, nil
}

// adoption collects what a use of configurations adopts at one instant: every revision once, with
// the revisions it depends on, all in force (Python active, recursively). Visit runs on each revision
// before its dependencies are adopted.
type adoption struct {
	at    time.Time
	list  []snapshot.Adopted
	seen  map[postgres.Key]bool
	visit func(rev *config.Revision, path string) error
}

func newAdoption(at time.Time, visit func(*config.Revision, string) error) *adoption {
	return &adoption{at: at, seen: map[postgres.Key]bool{}, visit: visit}
}

// adoptRef adopts the revision ref names, which must exist and be in force; rejections are at path.
func (a *adoption) adoptRef(ctx context.Context, exec bentopg.Executor, key postgres.Key, rev *config.Revision, path string) error {
	if rev == nil {
		return kernel.Reject(kernel.CodeReferenceInvalid, path, "")
	}
	if err := config.InForce(rev, a.at); err != nil {
		return config.Repath(err, path)
	}
	return a.adopt(ctx, exec, key, rev, path)
}

func (a *adoption) adopt(ctx context.Context, exec bentopg.Executor, key postgres.Key, rev *config.Revision, path string) error {
	if a.seen[key] {
		return nil
	}
	a.seen[key] = true
	if a.visit != nil {
		if err := a.visit(rev, path); err != nil {
			return err
		}
	}
	a.list = append(a.list, snapshot.Adopted{Entry: snapshot.Entry{Kind: key.Kind, ConfigID: key.ConfigID, Revision: key.Revision,
		ContentHash: rev.Hash}, Rev: rev, Path: path})
	for _, ref := range rev.Content.References() {
		dep, err := inForce(ctx, exec, ref.Target(), a.at, path)
		if err != nil {
			return err
		}
		if err = a.adopt(ctx, exec, postgres.Key{Kind: string(ref.Kind), ConfigID: ref.ID, Revision: dep.Number}, dep, path); err != nil {
			return err
		}
	}
	return nil
}

// find returns the adopted revision of target by ID or exactly, or nil.
func (a *adoption) find(t config.Target) *config.Revision {
	for _, e := range a.list {
		if string(t.Kind) == e.Entry.Kind && t.ID == e.Entry.ConfigID && (t.Revision == 0 || t.Revision == e.Entry.Revision) {
			return e.Rev
		}
	}
	return nil
}
