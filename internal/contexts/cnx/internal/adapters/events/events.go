// Package events holds CNX's Inbox consumer: the inbound adapter that turns the integration events
// of CNX's upstreams into calls on CNX's application (repository structure §07). The Dispatcher
// runs each handler in a transaction on CNX's own transaction owner, as the consumer's system
// actor, once per event ID (foundation/messaging).
//
// cnx.trade_compliance keeps trade compliance conclusions bound to what they were decided on (S5
// unit C4b-CNX, WBS S5-09b; CNX-INV-001; SM-CNX-COMPLIANCE; S5 D-9, D-18):
//
//   - TRD's OrderAmended (Context Map R46; event storming PL-14; WF-06 last step) opens a pending
//     check for the new order revision;
//   - POL's ConfigPublished of a screening_regulation (R50) reopens the clear conclusions that
//     follow an earlier revision of it;
//   - reference data's CountryProgrammesImported (MD-47) reopens the clear conclusions that used
//     another version of the list.
//
// Reference data is no bounded context of the Context Map: every context uses it through
// internal/reference/api (dependency rule D1), and its events reach consumers like a context's.
package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/api"
	"go.idp.xyz/idp-brant/internal/contexts/cnx/internal/application"
	tcc "go.idp.xyz/idp-brant/internal/contexts/cnx/internal/domain/tradecompliancecheck"
	polapi "go.idp.xyz/idp-brant/internal/contexts/pol/api"
	trdapi "go.idp.xyz/idp-brant/internal/contexts/trd/api"
	"go.idp.xyz/idp-brant/internal/foundation/messaging"
	"go.idp.xyz/idp-brant/internal/kernel"
	refapi "go.idp.xyz/idp-brant/internal/reference/api"
)

// TradeCompliance is the consumer of the events that make a trade compliance conclusion
// inapplicable. Its handlers run as system:inbox.cnx.trade_compliance, which brant-admin inbox
// grants TradeCompliancePermissions in every company it serves.
const TradeCompliance eventing.InboxConsumer = "cnx.trade_compliance"

// FailureEventInvalid is the failure code of an event whose payload does not carry what its
// producer's events carry: the event is refused for good.
const FailureEventInvalid eventing.FailureCode = "cnx.event_invalid"

// TradeCompliancePermissions are what the consumer's commands need: trade_compliance.reopen, and
// cnx.read, which the command ledger asks for before it answers a command from its stored result.
func TradeCompliancePermissions() []string {
	return []string{api.CommandReopen, api.PermissionRead}
}

// Follow is what cnx.trade_compliance asks of CNX's application; application.Service implements it.
type Follow interface {
	PrepareAmendedOrder(ctx context.Context, e application.AmendedOrder) (application.OrderBasis, error)
	OpenOnAmendment(ctx context.Context, exec bentopg.Executor, e application.AmendedOrder, b application.OrderBasis) (string, error)
	PrepareRegulation(ctx context.Context, e application.PublishedRegulation) (application.RegulationBasis, error)
	ReopenOnRegulation(ctx context.Context, exec bentopg.Executor, e application.PublishedRegulation, b application.RegulationBasis) (string, []string, error)
	ReopenOnProgrammes(ctx context.Context, exec bentopg.Executor, e application.ImportedList) ([]string, error)
}

// Subscriptions returns CNX's subscriptions on db, CNX's own transaction owner.
func Subscriptions(db *bentopg.DB, app Follow) []messaging.Subscription {
	permissions := TradeCompliancePermissions()
	return []messaging.Subscription{{
		Consumer:    TradeCompliance,
		Source:      trdapi.Source,
		EventTypes:  []eventing.EventType{trdapi.EventOrderAmended},
		DB:          db,
		Permissions: permissions,
		Prepare:     AmendedOrderPrepare(app),
		Handler:     AmendedOrderHandler(app),
	}, {
		Consumer:    TradeCompliance,
		Source:      polapi.Source,
		EventTypes:  []eventing.EventType{polapi.EventConfigPublished},
		DB:          db,
		Permissions: permissions,
		Prepare:     RegulationPrepare(app),
		Handler:     RegulationHandler(app),
	}, {
		Consumer:    TradeCompliance,
		Source:      refapi.Source,
		EventTypes:  []eventing.EventType{refapi.EventCountryProgrammesImported},
		DB:          db,
		Permissions: permissions,
		Handler:     ProgrammesHandler(app),
	}}
}

// refused turns an application's *FollowMismatch into the event's refusal; other errors retry.
func refused(err error) error {
	var mismatch *application.FollowMismatch
	if errors.As(err, &mismatch) {
		return messaging.RejectEvent(eventing.FailureCode(mismatch.Code), mismatch)
	}
	return err
}

// AmendedOrderPrepare reads TRD's order before the consumer's transaction
// (application.PrepareAmendedOrder). A payload that is not OrderAmended's is refused for good; a
// failed read is retried.
func AmendedOrderPrepare(app Follow) func(ctx context.Context, envelope eventing.Envelope) (any, error) {
	return func(ctx context.Context, envelope eventing.Envelope) (any, error) {
		e, err := amendedOrder(envelope)
		if err != nil {
			return nil, messaging.RejectEvent(FailureEventInvalid, err)
		}
		return app.PrepareAmendedOrder(ctx, e)
	}
}

// AmendedOrderHandler opens the pending check of the amended revision with what
// AmendedOrderPrepare read (application.OpenOnAmendment).
func AmendedOrderHandler(app Follow) messaging.EventFunc {
	return func(ctx context.Context, m messaging.Event) error {
		e, err := amendedOrder(m.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureEventInvalid, err)
		}
		basis, ok := m.Prepared.(application.OrderBasis)
		if !ok {
			return fmt.Errorf("cnx: %s delivered without its prepared order", TradeCompliance)
		}
		_, err = app.OpenOnAmendment(ctx, m.Exec, e, basis)
		return refused(err)
	}
}

// RegulationPrepare reads the published screening_regulation revision before the consumer's
// transaction (application.PrepareRegulation).
func RegulationPrepare(app Follow) func(ctx context.Context, envelope eventing.Envelope) (any, error) {
	return func(ctx context.Context, envelope eventing.Envelope) (any, error) {
		e, err := publishedConfig(envelope)
		if err != nil {
			return nil, messaging.RejectEvent(FailureEventInvalid, err)
		}
		return app.PrepareRegulation(ctx, e)
	}
}

// RegulationHandler reopens the clear conclusions that follow an earlier revision of the published
// screening_regulation (application.ReopenOnRegulation); other kinds are settled without effect.
func RegulationHandler(app Follow) messaging.EventFunc {
	return func(ctx context.Context, m messaging.Event) error {
		e, err := publishedConfig(m.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureEventInvalid, err)
		}
		basis, ok := m.Prepared.(application.RegulationBasis)
		if !ok {
			return fmt.Errorf("cnx: %s delivered without its prepared regulation", TradeCompliance)
		}
		_, _, err = app.ReopenOnRegulation(ctx, m.Exec, e, basis)
		return refused(err)
	}
}

// ProgrammesHandler reopens the clear conclusions that used another version of the imported list
// than its current one (application.ReopenOnProgrammes).
func ProgrammesHandler(app Follow) messaging.EventFunc {
	return func(ctx context.Context, m messaging.Event) error {
		e, err := importedList(m.Envelope)
		if err != nil {
			return messaging.RejectEvent(FailureEventInvalid, err)
		}
		_, err = app.ReopenOnProgrammes(ctx, m.Exec, e)
		return refused(err)
	}
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

func payloadOf(envelope eventing.Envelope, into any) error {
	if err := json.Unmarshal(envelope.Payload, into); err != nil {
		return fmt.Errorf("cnx: payload of %s %s: %w", envelope.Type, envelope.ID, err)
	}
	return nil
}

func invalid(envelope eventing.Envelope, what string) error {
	return fmt.Errorf("cnx: event %s of type %s: %s", envelope.ID, envelope.Type, what)
}

// amendedOrder reads envelope as TRD's OrderAmended (contract C4: the aggregate the command ledger
// names and the order binding TRD adds to every order event — here the amended revision).
func amendedOrder(envelope eventing.Envelope) (application.AmendedOrder, error) {
	var p struct {
		AggregateType    string `json:"aggregate_type"`
		AggregateID      string `json:"aggregate_id"`
		AggregateVersion int64  `json:"aggregate_version"`
		OrderID          string `json:"order_id"`
		OrderRevision    int64  `json:"order_revision"`
		OrderHash        string `json:"order_hash"`
	}
	if err := payloadOf(envelope, &p); err != nil {
		return application.AmendedOrder{}, err
	}
	switch {
	case p.AggregateType != "SalesOrder":
		return application.AmendedOrder{}, invalid(envelope, "aggregate_type is not SalesOrder")
	case !kernel.ValidIdentifier(p.OrderID) || p.OrderID != p.AggregateID:
		return application.AmendedOrder{}, invalid(envelope, "order_id is not the aggregate's identifier")
	case p.AggregateVersion < 1:
		return application.AmendedOrder{}, invalid(envelope, "aggregate_version is not positive")
	case p.OrderRevision < 2:
		return application.AmendedOrder{}, invalid(envelope, "order_revision is not an amended revision")
	case !sha256Hex.MatchString(p.OrderHash):
		return application.AmendedOrder{}, invalid(envelope, "order_hash is not a SHA-256")
	}
	return application.AmendedOrder{EventID: string(envelope.ID), EventType: string(envelope.Type),
		Binding: tcc.Binding{OrderID: p.OrderID, OrderRevision: p.OrderRevision, OrderHash: p.OrderHash}}, nil
}

// publishedConfig reads envelope as POL's ConfigPublished: the kind, the configuration and the
// revision made active.
func publishedConfig(envelope eventing.Envelope) (application.PublishedRegulation, error) {
	var p struct {
		AggregateType string `json:"aggregate_type"`
		Kind          string `json:"kind"`
		ConfigID      string `json:"config_id"`
		Revision      int64  `json:"revision"`
	}
	if err := payloadOf(envelope, &p); err != nil {
		return application.PublishedRegulation{}, err
	}
	switch {
	case p.AggregateType != "ConfigurationRoot":
		return application.PublishedRegulation{}, invalid(envelope, "aggregate_type is not ConfigurationRoot")
	case !kernel.ValidIdentifier(p.Kind):
		return application.PublishedRegulation{}, invalid(envelope, "kind is not an identifier")
	case !kernel.ValidIdentifier(p.ConfigID):
		return application.PublishedRegulation{}, invalid(envelope, "config_id is not an identifier")
	case p.Revision < 1:
		return application.PublishedRegulation{}, invalid(envelope, "revision is not positive")
	}
	return application.PublishedRegulation{EventID: string(envelope.ID), EventType: string(envelope.Type), Kind: p.Kind,
		Ref: polapi.ConfigRef{Kind: p.Kind, ConfigID: p.ConfigID, Revision: p.Revision}}, nil
}

// importedList reads envelope as reference data's CountryProgrammesImported: the list and the
// version imported.
func importedList(envelope eventing.Envelope) (application.ImportedList, error) {
	var p struct {
		AggregateType string `json:"aggregate_type"`
		ListCode      string `json:"list_code"`
		Version       string `json:"version"`
	}
	if err := payloadOf(envelope, &p); err != nil {
		return application.ImportedList{}, err
	}
	switch {
	case p.AggregateType != "CountryProgrammeList":
		return application.ImportedList{}, invalid(envelope, "aggregate_type is not CountryProgrammeList")
	case len(p.ListCode) > api.MaxListCodeLength || !kernel.ValidIdentifier(p.ListCode):
		return application.ImportedList{}, invalid(envelope, "list_code is not a list code")
	case !refapi.ValidVersion(p.Version):
		return application.ImportedList{}, invalid(envelope, "version is not a list version")
	}
	return application.ImportedList{EventID: string(envelope.ID), EventType: string(envelope.Type), ListCode: p.ListCode, Version: p.Version}, nil
}
