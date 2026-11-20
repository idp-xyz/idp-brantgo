package upstream

import (
	"context"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	ptyapi "go.idp.xyz/idp-brant/internal/contexts/pty/api"
)

// ServiceRoles is PTY's ServiceRoleRef v1 lookup (R77; S5R-D36).
type ServiceRoles interface {
	ResolveServiceRoles(ctx context.Context, refs []ptyapi.ServiceRoleRef, at time.Time) (map[ptyapi.ServiceRoleRef]ptyapi.ResolvedServiceRole, error)
}

// Brokers answers application.Brokers from PTY's ResolveServiceRoles (an active grant, PTY's data scope): the kind of a
// party's service role revision at an instant and whether it serves then. A customs broker is a role of kind
// customs_broker (S5R-D36), or of the S1 name broker PTY keeps verifiable until every caller moved.
type Brokers struct {
	Roles ServiceRoles
}

var _ application.Brokers = Brokers{}

// CustomsBroker reports whether kind is the service role kind of a customs broker.
func CustomsBroker(kind string) bool {
	return kind == ptyapi.ServiceCustomsBroker || kind == ptyapi.ServiceBroker
}

// ResolveBrokers asks PTY before CNX's transaction.
func (b Brokers) ResolveBrokers(ctx context.Context, refs []application.BrokerRef, at time.Time) (map[application.BrokerRef]application.Broker, error) {
	out := map[application.BrokerRef]application.Broker{}
	if len(refs) == 0 {
		return out, nil
	}
	asked := make([]ptyapi.ServiceRoleRef, len(refs))
	for i, r := range refs {
		asked[i] = ptyapi.ServiceRoleRef{PartyID: r.PartyID, ServiceRoleID: r.ServiceRoleID, Revision: r.Revision}
	}
	got, err := b.Roles.ResolveServiceRoles(ctx, asked, at)
	if err != nil {
		return nil, err
	}
	for ref, r := range got {
		out[application.BrokerRef{PartyID: ref.PartyID, ServiceRoleID: ref.ServiceRoleID, Revision: ref.Revision}] =
			application.Broker{Kind: r.Role, CustomsBroker: CustomsBroker(r.Role), Code: r.Code, Stale: r.Stale}
	}
	return out, nil
}
