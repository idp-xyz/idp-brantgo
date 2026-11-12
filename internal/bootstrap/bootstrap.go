// Package bootstrap is Brant's composition root. It owns the connection pool and gives every
// bounded context, and the Outbox relay, an IDP-Bento-Go transaction owner of its own (rule T1). A
// call from one context into another inside a transaction then fails with
// application.ErrTransactionConflict instead of silently joining that transaction.
//
// It is the only package that may import every other internal package; process entry points under
// cmd/ reach Brant code through it.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/cat"
	"go.idp.xyz/idp-brant/internal/contexts/cnx"
	"go.idp.xyz/idp-brant/internal/contexts/cst"
	"go.idp.xyz/idp-brant/internal/contexts/evd"
	"go.idp.xyz/idp-brant/internal/contexts/inq"
	"go.idp.xyz/idp-brant/internal/contexts/pol"
	"go.idp.xyz/idp-brant/internal/contexts/pty"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	"go.idp.xyz/idp-brant/internal/contexts/sup"
	"go.idp.xyz/idp-brant/internal/contexts/trd"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database"
	"go.idp.xyz/idp-brant/internal/foundation/identity/oidc"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
	"go.idp.xyz/idp-brant/internal/reference"
	"go.idp.xyz/idp-brant/internal/transport/httpapi"
)

// Config is what every Brant process needs to reach PostgreSQL.
type Config struct {
	PostgresDSN string
	BentoSchema string // empty means bentopg.DefaultSchema
}

// Platform holds the connection pool that all transaction owners share.
type Platform struct {
	pool     *pgxpool.Pool
	schema   string
	verifier *oidc.Verifier
}

// Open connects as the application role, refuses servers other than PostgreSQL 16 and privileged
// roles, and checks the Bento schema.
func Open(ctx context.Context, cfg Config) (*Platform, error) {
	if cfg.PostgresDSN == "" {
		return nil, errors.New("bootstrap: PostgreSQL DSN is required")
	}
	schema := cfg.BentoSchema
	if schema == "" {
		schema = bentopg.DefaultSchema
	}
	pool, err := database.Open(ctx, cfg.PostgresDSN)
	if err != nil {
		return nil, err
	}
	p := &Platform{pool: pool, schema: schema}
	db, err := p.newDB()
	if err == nil {
		err = db.CheckSchema(ctx)
	}
	if err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Close releases the connection pool.
func (p *Platform) Close() { p.pool.Close() }

// RelayConfig configures the Outbox relay that publishes to an HTTP sink.
type RelayConfig struct {
	SinkURL        string
	Batch          int
	LeaseFor       time.Duration
	PublishTimeout time.Duration
	MaxAttempts    uint32
	MaxFailures    uint32
	RetryBase      time.Duration
	RetryMax       time.Duration
	IdleWait       time.Duration
	Logger         *slog.Logger
	// Alerter is told about each dead letter; nil logs it at ERROR on Logger.
	Alerter messaging.Alerter
	// Critical marks revocation, repudiation and risk event types; nil means
	// messaging.CriticalEventType.
	Critical func(eventing.EventType) bool
}

// Relay assembles the Outbox relay on a transaction owner of its own. The dead letters share it,
// so an abandonment and its dead letter commit together.
func (p *Platform) Relay(cfg RelayConfig) (messaging.Relay, error) {
	db, err := p.newDB()
	if err != nil {
		return messaging.Relay{}, err
	}
	store, err := bentooutbox.NewStore(db)
	if err != nil {
		return messaging.Relay{}, err
	}
	return messaging.Relay{
		Outbox: store, DeadLetters: messaging.DeadLetterStore{DB: db}, Alerter: cfg.Alerter, Critical: cfg.Critical,
		Publisher: messaging.Webhook{URL: cfg.SinkURL}, Clock: bento.SystemClock{},
		Batch: cfg.Batch, LeaseFor: cfg.LeaseFor, PublishTimeout: cfg.PublishTimeout,
		MaxAttempts: cfg.MaxAttempts, MaxFailures: cfg.MaxFailures,
		RetryBase: cfg.RetryBase, RetryMax: cfg.RetryMax, IdleWait: cfg.IdleWait, Logger: cfg.Logger,
	}, nil
}

// ReplayConfig configures the replay of dead letters to the relay's HTTP sink.
type ReplayConfig struct {
	SinkURL        string
	PublishTimeout time.Duration
}

// Replayer assembles dead-letter replay on a transaction owner of its own. Its caller puts the
// verified Principal of the operator in the context of each Replay.
func (p *Platform) Replayer(cfg ReplayConfig) (messaging.Replayer, error) {
	if cfg.SinkURL == "" || cfg.PublishTimeout <= 0 {
		return messaging.Replayer{}, errors.New("bootstrap: replay needs a sink URL and a publish timeout")
	}
	db, err := p.newDB()
	if err != nil {
		return messaging.Replayer{}, err
	}
	return messaging.Replayer{
		DB: db, Publisher: messaging.Webhook{URL: cfg.SinkURL}, Clock: bento.SystemClock{}, PublishTimeout: cfg.PublishTimeout,
	}, nil
}

// newDB creates a new transaction owner on the shared pool.
func (p *Platform) newDB() (*bentopg.DB, error) {
	return bentopg.NewDB(p.pool, bentopg.WithSchema(p.schema))
}

// Contexts are the bounded contexts of a process and its reference data, each on a transaction
// owner of its own.
type Contexts struct {
	REF *reference.Module // screening lists; not a bounded context
	EVD *evd.Module
	PTY *pty.Module
	CAT *cat.Module
	POL *pol.Module
	INQ *inq.Module
	SUP *sup.Module
	CST *cst.Module
	TRD *trd.Module
	PUR *pur.Module
	CNX *cnx.Module
}

// Contexts wires reference data and every bounded context, upstream first. Call it once per
// process.
func (p *Platform) Contexts() (*Contexts, error) {
	var c Contexts
	db, err := p.newDB()
	if err == nil {
		c.REF, err = reference.New(db)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.EVD, err = evd.New(db)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.POL, err = pol.New(db, c.EVD)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.PTY, err = pty.New(db, c.EVD, c.POL, c.REF)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.CAT, err = cat.New(db, c.EVD)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.INQ, err = inq.New(db, c.EVD, c.PTY, c.CAT)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.SUP, err = sup.New(db, sup.Upstream{Sources: c.EVD, Parties: c.PTY, Skus: c.CAT, Rfqs: c.INQ, Configs: c.POL})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.CST, err = cst.New(db, c.EVD, c.INQ, c.CAT, c.POL, c.SUP)
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.TRD, err = trd.New(db, trd.Upstream{Sources: c.EVD, Parties: c.PTY, Profiles: c.PTY, Skus: c.CAT, Configs: c.POL, Numbers: c.POL,
			Rfqs: c.INQ, Snapshots: c.CST, Contexts: c.CST})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.PUR, err = pur.New(db, pur.Upstream{Orders: pur.TRDOrders(c.TRD), Supply: c.SUP, Snapshots: c.CST, Parties: c.PTY, Configs: c.POL,
			Skus: c.CAT})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		// CNX after PUR (S5: QLT, STL, CNX, FUL). CAT's declaration profiles (S5 A5) are wired when
		// A5 merges; until then that port answers DEPENDENCY_NOT_SUPPORTED.
		c.CNX, err = cnx.New(db, cnx.Upstream{Orders: cnx.TRDOrders(c.TRD), Countries: cnx.PTYCountries(c.PTY), Profiles: cnx.ProfilesNotWired(),
			Programmes: cnx.REFProgrammes(c.REF), Configs: c.POL, Sources: c.EVD})
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Grant is an operator's change to one actor's access in a tenant and company. It is not a user
// command: the identity provider and an administration process own access, and this is the
// out-of-band path they and the local demo use (Python Core.provision).
type Grant struct {
	Scope       kernel.Scope
	Actor       kernel.ActorRef
	Permissions []string
	DataScope   authorization.DataScope // empty keeps the current data scope (own for a new grant)
	Team        string
}

// Provision creates the scope's authority row when it is missing and gives the actor exactly
// g.Permissions and, when set, g.DataScope. It returns the scope's authority epoch after the change.
func (p *Platform) Provision(ctx context.Context, g Grant) (int64, error) {
	db, err := p.newDB()
	if err != nil {
		return 0, err
	}
	var epoch int64
	err = bento.ScopeTransactor{DB: db, Scope: g.Scope}.WithinTransaction(ctx, func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		grants, now := authorization.Grants{}, bento.SystemClock{}.Now()
		if err = grants.Provision(ctx, exec, g.Scope); err != nil {
			return err
		}
		if epoch, err = grants.Put(ctx, exec, g.Scope, g.Actor, g.Permissions, now); err != nil {
			return err
		}
		if g.DataScope != "" {
			epoch, err = grants.SetDataScope(ctx, exec, g.Scope, g.Actor.ID, g.DataScope, g.Team, now)
		}
		return err
	})
	return epoch, err
}

// ProvisionActor is Provision from plain operator input, for cmd/brant-admin.
func (p *Platform) ProvisionActor(ctx context.Context, tenant, company, actorID, kind string, permissions []string, dataScope, team string) (int64, error) {
	scope, err := kernel.NewScope(tenant, company)
	if err != nil {
		return 0, err
	}
	actor, err := kernel.NewActorRef(kernel.ActorKind(kind), actorID)
	if err != nil {
		return 0, err
	}
	if dataScope != "" && !authorization.DataScope(dataScope).Valid() {
		return 0, fmt.Errorf("bootstrap: data scope %q is not own, team or company", dataScope)
	}
	if permissions == nil {
		permissions = []string{}
	}
	return p.Provision(ctx, Grant{Scope: scope, Actor: actor, Permissions: permissions, DataScope: authorization.DataScope(dataScope), Team: team})
}

// APIConfig configures the HTTP API process: the OIDC issuer whose access tokens it accepts.
type APIConfig struct {
	Issuer            string
	Audience          string
	JWKSURL           string   // empty discovers it from the issuer
	TokenTypes        []string // accepted "typ" header values; empty means "at+jwt" (RFC 9068)
	AllowInsecureHTTP bool     // http:// issuer on a loopback host, for local tests only
	// ReplaySinkURL enables dead-letter replay over the API (ADR-BR-015): the HTTP sink that
	// receives replayed envelopes, normally the relay's sink. Empty leaves the route out.
	ReplaySinkURL        string
	ReplayPublishTimeout time.Duration
	Logger               *slog.Logger
}

// API wires every context behind the HTTP transport with an OIDC verifier. It makes no network
// request; keys load on first use, or earlier through VerifyIssuer.
func (p *Platform) API(cfg APIConfig) (http.Handler, error) {
	verifier, err := oidc.New(oidc.Config{Issuer: cfg.Issuer, Audience: cfg.Audience, JWKSURL: cfg.JWKSURL,
		TokenTypes: cfg.TokenTypes, AllowInsecureHTTP: cfg.AllowInsecureHTTP})
	if err != nil {
		return nil, err
	}
	p.verifier = verifier
	c, err := p.Contexts()
	if err != nil {
		return nil, err
	}
	tc := httpapi.Config{EVD: c.EVD, PTY: c.PTY, CAT: c.CAT, INQ: c.INQ, POL: c.POL, REF: c.REF, SUP: c.SUP, CST: c.CST, TRD: c.TRD, PUR: c.PUR, Verifier: oidc.HeaderVerifier{Tokens: verifier},
		Clock: bento.SystemClock{}, Logger: cfg.Logger}
	tc.CNX = c.CNX
	if cfg.ReplaySinkURL != "" {
		replayer, err := p.Replayer(ReplayConfig{SinkURL: cfg.ReplaySinkURL, PublishTimeout: cfg.ReplayPublishTimeout})
		if err != nil {
			return nil, err
		}
		tc.Replayer = replayer
	}
	return httpapi.New(tc)
}

// VerifyIssuer loads the issuer's keys now, so a wrong issuer or JWKS URL fails at start-up.
func (p *Platform) VerifyIssuer(ctx context.Context) error {
	if p.verifier == nil {
		return errors.New("bootstrap: API not built")
	}
	return p.verifier.Refresh(ctx)
}

// AssignRoles gives an actor exactly roles (role templates or tenant roles, BC-D02) and derives its
// permissions; a forbidden combination is refused with authorization.ErrSeparationOfDuties. It is
// an operator's change like Provision; dataScope and team are set as Provision sets them.
func (p *Platform) AssignRoles(ctx context.Context, scope kernel.Scope, actor kernel.ActorRef, roles []string, dataScope authorization.DataScope, team string) (int64, error) {
	return p.inAuthority(ctx, scope, func(ctx context.Context, exec bentopg.Executor, g authorization.Grants, now time.Time) (int64, error) {
		if err := g.Provision(ctx, exec, scope); err != nil {
			return 0, err
		}
		epoch, err := g.AssignRoles(ctx, exec, scope, actor, roles, now)
		if err == nil && dataScope != "" {
			epoch, err = g.SetDataScope(ctx, exec, scope, actor.ID, dataScope, team, now)
		}
		return epoch, err
	})
}

// DefineRole defines a tenant role that merges role templates; a forbidden combination is refused.
func (p *Platform) DefineRole(ctx context.Context, scope kernel.Scope, code, name string, templates []string) (int64, error) {
	return p.inAuthority(ctx, scope, func(ctx context.Context, exec bentopg.Executor, g authorization.Grants, now time.Time) (int64, error) {
		if err := g.Provision(ctx, exec, scope); err != nil {
			return 0, err
		}
		return g.DefineRole(ctx, exec, scope, code, name, templates, now)
	})
}

func (p *Platform) inAuthority(ctx context.Context, scope kernel.Scope, fn func(context.Context, bentopg.Executor, authorization.Grants, time.Time) (int64, error)) (int64, error) {
	db, err := p.newDB()
	if err != nil {
		return 0, err
	}
	var epoch int64
	err = bento.ScopeTransactor{DB: db, Scope: scope}.WithinTransaction(ctx, func(ctx context.Context) error {
		exec, err := db.RequireExecutor(ctx)
		if err != nil {
			return err
		}
		epoch, err = fn(ctx, exec, authorization.Grants{}, bento.SystemClock{}.Now())
		return err
	})
	return epoch, err
}

// AssignRolesFor is AssignRoles from plain operator input, for cmd/brant-admin.
func (p *Platform) AssignRolesFor(ctx context.Context, tenant, company, actorID, kind string, roles []string, dataScope, team string) (int64, error) {
	scope, err := kernel.NewScope(tenant, company)
	if err != nil {
		return 0, err
	}
	actor, err := kernel.NewActorRef(kernel.ActorKind(kind), actorID)
	if err != nil {
		return 0, err
	}
	if dataScope != "" && !authorization.DataScope(dataScope).Valid() {
		return 0, fmt.Errorf("bootstrap: data scope %q is not own, team or company", dataScope)
	}
	return p.AssignRoles(ctx, scope, actor, roles, authorization.DataScope(dataScope), team)
}

// DefineRoleFor is DefineRole from plain operator input, for cmd/brant-admin.
func (p *Platform) DefineRoleFor(ctx context.Context, tenant, company, code, name string, templates []string) (int64, error) {
	scope, err := kernel.NewScope(tenant, company)
	if err != nil {
		return 0, err
	}
	return p.DefineRole(ctx, scope, code, name, templates)
}
