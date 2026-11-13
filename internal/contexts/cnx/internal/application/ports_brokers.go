package application

import (
	"context"
	"time"
)

// Brokers is CNX's port to PTY for customs brokers (Context Map R77 PTY→CNX, "ServiceRoleRef v1（customs_broker；报关行
// 须已核实，S5R-D36）"; no guard lock): what PTY says at an instant of a revision of a party's service role — its kind and
// whether it serves new business then (Code "" or PTY's REFERENCE_STALE with Stale saying why). A role PTY does not show
// the caller (no such party, role or revision; not the caller's) is absent. Asked before CNX's transaction.
type Brokers interface {
	ResolveBrokers(ctx context.Context, refs []BrokerRef, at time.Time) (map[BrokerRef]Broker, error)
}

// BrokerRef names a revision of a service role of a party (PTY ServiceRoleRef v1).
type BrokerRef struct {
	PartyID       string
	ServiceRoleID string
	Revision      int64
}

// Broker is what PTY says of a BrokerRef: the service role's kind, whether that kind is a customs broker's
// (customs_broker, or the S1 name broker PTY keeps verifiable until CNX moves; S5R-D36), and its code now.
type Broker struct {
	Kind          string
	CustomsBroker bool
	Code          string
	Stale         string
}
