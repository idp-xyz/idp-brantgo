package plugin

import (
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	bentooutbox "go.idp.xyz/idp-bento-go/postgres/outbox"
	"go.idp.xyz/idp-brant/internal/foundation/action"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/plugin/postgres"
)

// Module is the plugin platform on a transaction owner of its own.
type Module struct {
	Service
}

// New wires the platform to db. reg, when nil, is a registry that already holds ApparelManifest.
// inFlight may be nil in a process that does not compose the downstream contexts.
func New(db *bentopg.DB, reg *Registry, inFlight Checker) (*Module, error) {
	if reg == nil {
		reg = NewRegistry()
		if err := reg.Register(Registration{Manifest: ApparelManifest()}); err != nil {
			return nil, err
		}
	}
	policy, err := authorization.NewPolicy(Rules()...)
	if err != nil {
		return nil, err
	}
	outbox, err := bentooutbox.NewStore(db)
	if err != nil {
		return nil, err
	}
	clock := bento.SystemClock{}
	return &Module{Service{
		Ledger: &action.Ledger{
			DB: db, Schema: postgres.Schema, Source: Source, Authorizer: authorization.Authorizer{Policy: policy},
			Outbox: outbox, Clock: clock, ReplayRead: CommandList, Visible: Visible,
		},
		Clock: clock, Registry: reg, InFlight: inFlight,
	}}, nil
}
