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
	"go.idp.xyz/idp-brant/internal/contexts/ful"
	fulapi "go.idp.xyz/idp-brant/internal/contexts/ful/api"
	"go.idp.xyz/idp-brant/internal/contexts/inq"
	"go.idp.xyz/idp-brant/internal/contexts/pol"
	"go.idp.xyz/idp-brant/internal/contexts/pty"
	"go.idp.xyz/idp-brant/internal/contexts/pur"
	"go.idp.xyz/idp-brant/internal/contexts/qlt"
	"go.idp.xyz/idp-brant/internal/contexts/stl"
	"go.idp.xyz/idp-brant/internal/contexts/sup"
	"go.idp.xyz/idp-brant/internal/contexts/trd"
	"go.idp.xyz/idp-brant/internal/contexts/tsk"
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

// RelayConfig configures the Outbox relay that publishes to an HTTP sink or to Publisher.
type RelayConfig struct {
	SinkURL string
	// Publisher receives each event instead of an HTTP sink, such as the Dispatcher of the composed
	// contexts (brant-worker -dispatch); SinkURL is empty then.
	Publisher      eventing.Publisher
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
	// Metrics counts the relay's deliveries, latency, failures and dead letters (S5R-D07); nil
	// counts nothing. See NewRelayMetrics.
	Metrics *messaging.RelayMetrics
}

// Relay assembles the Outbox relay on a transaction owner of its own. The dead letters share it,
// so an abandonment and its dead letter commit together.
func (p *Platform) Relay(cfg RelayConfig) (messaging.Relay, error) {
	publisher, err := publisherFor(cfg.SinkURL, cfg.Publisher)
	if err != nil {
		return messaging.Relay{}, err
	}
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
		Publisher: publisher, Clock: bento.SystemClock{},
		Batch: cfg.Batch, LeaseFor: cfg.LeaseFor, PublishTimeout: cfg.PublishTimeout,
		MaxAttempts: cfg.MaxAttempts, MaxFailures: cfg.MaxFailures,
		RetryBase: cfg.RetryBase, RetryMax: cfg.RetryMax, IdleWait: cfg.IdleWait, Logger: cfg.Logger, Metrics: cfg.Metrics,
	}, nil
}

// ReplayConfig configures the replay of dead letters to the relay's HTTP sink or to Publisher.
type ReplayConfig struct {
	SinkURL string
	// Publisher receives replayed events instead of an HTTP sink, such as the Dispatcher of the
	// composed contexts; SinkURL is empty then.
	Publisher      eventing.Publisher
	PublishTimeout time.Duration
}

// Replayer assembles dead-letter replay on a transaction owner of its own. Its caller puts the
// verified Principal of the operator in the context of each Replay.
func (p *Platform) Replayer(cfg ReplayConfig) (messaging.Replayer, error) {
	if cfg.PublishTimeout <= 0 {
		return messaging.Replayer{}, errors.New("bootstrap: replay needs a publish timeout")
	}
	publisher, err := publisherFor(cfg.SinkURL, cfg.Publisher)
	if err != nil {
		return messaging.Replayer{}, err
	}
	db, err := p.newDB()
	if err != nil {
		return messaging.Replayer{}, err
	}
	return messaging.Replayer{
		DB: db, Publisher: publisher, Clock: bento.SystemClock{}, PublishTimeout: cfg.PublishTimeout,
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
	QLT *qlt.Module
	STL *stl.Module
	CNX *cnx.Module
	FUL *ful.Module
	TSK *tsk.Module
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
		// STL's CreditGuard v1 (R57) and CNX's TradeComplianceGuard v1 (R58) are guard functions TRD
		// runs with its own executor: TRD is composed before STL and CNX, which read its orders.
		c.TRD, err = trd.New(db, trd.Upstream{Sources: c.EVD, Parties: c.PTY, Profiles: c.PTY, Skus: c.CAT, Configs: c.POL, Numbers: c.POL,
			Rfqs: c.INQ, Snapshots: c.CST, Contexts: c.CST, Credit: trd.STLCreditGuard(), TradeCompliance: trd.CNXTradeComplianceGuard(),
			Shipments: trd.FULShipmentFacts(), Commitments: trd.PURDownstreamCommitments()})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		c.PUR, err = pur.New(db, pur.Upstream{Orders: pur.TRDOrders(c.TRD), Supply: c.SUP, Snapshots: c.CST, Parties: c.PTY, Configs: c.POL,
			Numbers: c.POL, Skus: c.CAT, Sources: c.EVD})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		// PUR publishes no InspectionSubject v1 yet (S5 unit A4): QLT's port to it answers
		// DEPENDENCY_NOT_SUPPORTED until it is bound to PUR's lookup. FUL's ReleaseConsumption v1
		// (R79) is a SQL function QLT's revocation reads; FUL is composed later (S5R-D18).
		c.QLT, err = qlt.New(db, qlt.Upstream{Subjects: qlt.PURSubjects(c.PUR), Sources: c.EVD, Agencies: qlt.PTYAgencies(c.PTY),
			Consumption: qlt.FULReleaseConsumption()})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		// STL's settlement statements read FUL's shipment lines and PUR's verified deliveries (R30,
		// R31); FUL is composed after STL, so its port is bound to c.FUL by a function (D-2).
		c.STL, err = stl.New(db, stl.Upstream{Orders: stl.TRDOrders(c.TRD), Configs: c.POL, Parties: c.PTY, Sources: c.EVD,
			Shipments: stl.FULShipmentLines(func() fulapi.ShipmentLineLookups {
				if c.FUL == nil {
					return nil
				}
				return c.FUL
			}), Deliveries: stl.PURSettlementSources(c.PUR)})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		// CNX after PUR (S5: QLT, STL, CNX, FUL).
		c.CNX, err = cnx.New(db, cnx.Upstream{Orders: cnx.TRDOrders(c.TRD), Countries: cnx.PTYCountries(c.PTY), Profiles: cnx.CATProfiles(c.CAT),
			Programmes: cnx.REFProgrammes(c.REF), Configs: c.POL, Sources: c.EVD,
			// FUL's shipments and export lots (R36): FUL is composed after CNX, so its lookups are
			// bound when CNX asks (S5 D-2).
			Exports: cnx.FULExports(func() cnx.FULLookups { return c.FUL }, c.POL, cnx.PTYCountries(c.PTY)), Purchases: cnx.PURLines(c.PUR),
			// STL's fund records and credit notes (R38 STL→CNX; CreditNoteRef v1 for CN-AT11, S5 C10).
			Funds: cnx.STLFunds(c.STL), CreditNotes: cnx.STLCreditNotes(c.STL),
			// PTY's customs brokers (R77, ServiceRoleRef v1; S5R-D36).
			Brokers: cnx.PTYBrokers(c.PTY)})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		// FUL last of S5: its commercial release and confirmation call CNX's and STL's guards (C1).
		c.FUL, err = ful.New(db, ful.Upstream{Orders: ful.TRDOrders(c.TRD), Releases: ful.QLTReleases(c.QLT), Deliveries: ful.PURDeliveries(c.PUR),
			Screening: ful.PTYScreening(), Sources: c.EVD, Compliance: ful.CNXCompliance(), Payments: ful.STLPayments(),
			// S5-08: bookings and shipment documents; S5 C5: CNX's certificate results (R56)
			Documents: ful.DocumentPorts(c.POL, c.PTY, c.TRD).WithCertificates(c.CNX),
			Costs:     c.CST, // S5-16: the costing's insurance component (R78)
		})
	}
	if err == nil {
		db, err = p.newDB()
	}
	if err == nil {
		// TSK (S4) after TRD: it calls TRD's object control (R17) and its GoalReadiness v1 (R18; S4
		// unit B2: goal.verify) before its own transactions, and the ActionReconciliation v1 of the
		// contexts a task step writes (R18, R66–R69; S4 unit B1).
		c.TSK, err = tsk.New(db, tsk.Upstream{Control: tsk.TRDControl(c.TRD), Readiness: tsk.TRDReadiness(c.TRD),
			Reconcilers: tsk.ActionReconciliation(tsk.ActionReconcilers{EVD: c.EVD, INQ: c.INQ, SUP: c.SUP, CST: c.CST, TRD: c.TRD})})
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
	ReplaySinkURL string
	// ReplayDispatch enables the replay route with in-process dispatch to the subscriptions of the
	// composed contexts instead of a sink, for deployments whose worker runs -dispatch.
	ReplayDispatch       bool
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
	tc := httpapi.Config{EVD: c.EVD, PTY: c.PTY, CAT: c.CAT, INQ: c.INQ, POL: c.POL, REF: c.REF, SUP: c.SUP, CST: c.CST, TRD: c.TRD, PUR: c.PUR, QLT: c.QLT, Verifier: oidc.HeaderVerifier{Tokens: verifier},
		Clock: bento.SystemClock{}, Logger: cfg.Logger}
	tc.STL = c.STL
	tc.CNX = c.CNX
	tc.FUL = c.FUL
	if cfg.ReplaySinkURL != "" || cfg.ReplayDispatch {
		rc := ReplayConfig{SinkURL: cfg.ReplaySinkURL, PublishTimeout: cfg.ReplayPublishTimeout}
		if cfg.ReplayDispatch {
			if rc.Publisher, err = c.Dispatcher(); err != nil {
				return nil, err
			}
		}
		replayer, err := p.Replayer(rc)
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
