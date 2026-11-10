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
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/contexts/evd"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/database"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// Config is what every Brant process needs to reach PostgreSQL.
type Config struct {
	PostgresDSN string
	BentoSchema string // empty means bentopg.DefaultSchema
}

// Platform holds the connection pool that all transaction owners share.
type Platform struct {
	pool   *pgxpool.Pool
	schema string
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
}

// Relay assembles the Outbox relay on a transaction owner of its own.
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
		Outbox: store, Publisher: messaging.Webhook{URL: cfg.SinkURL}, Clock: bento.SystemClock{},
		Batch: cfg.Batch, LeaseFor: cfg.LeaseFor, PublishTimeout: cfg.PublishTimeout,
		MaxAttempts: cfg.MaxAttempts, MaxFailures: cfg.MaxFailures,
		RetryBase: cfg.RetryBase, RetryMax: cfg.RetryMax, IdleWait: cfg.IdleWait, Logger: cfg.Logger,
	}, nil
}

// newDB creates a new transaction owner on the shared pool.
func (p *Platform) newDB() (*bentopg.DB, error) {
	return bentopg.NewDB(p.pool, bentopg.WithSchema(p.schema))
}

// Contexts are the bounded contexts of a process, each on a transaction owner of its own.
type Contexts struct {
	EVD *evd.Module
}

// Contexts wires every bounded context. Call it once per process.
func (p *Platform) Contexts() (*Contexts, error) {
	evdDB, err := p.newDB()
	if err != nil {
		return nil, err
	}
	evdModule, err := evd.New(evdDB)
	if err != nil {
		return nil, err
	}
	return &Contexts{EVD: evdModule}, nil
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
