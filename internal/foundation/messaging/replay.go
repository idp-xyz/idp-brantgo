package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"go.idp.xyz/idp-bento-go/application"
	"go.idp.xyz/idp-bento-go/eventing"
	bentopg "go.idp.xyz/idp-bento-go/postgres"
	"go.idp.xyz/idp-brant/internal/foundation/authorization"
	"go.idp.xyz/idp-brant/internal/foundation/bento"
	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// The command and the permission an operator needs to replay a dead letter. Only humans may.
const (
	ReplayCommand    = "dead_letter.replay"
	ReplayPermission = "dead_letter.replay"
)

var (
	ErrDeadLetterNotFound = errors.New("messaging: dead letter not found")
	ErrAlreadyReplayed    = errors.New("messaging: dead letter already replayed")
	ErrReplayFailed       = errors.New("messaging: dead letter replay failed")
)

// ReplayRule is the rule of ReplayCommand.
func ReplayRule() authorization.Rule { return authorization.HumanOnly(ReplayCommand, ReplayPermission) }

// ReplayResult is a completed replay.
type ReplayResult struct {
	Source         string
	EventID        eventing.EventID
	ReplayedAt     time.Time
	ReplayedBy     kernel.ActorRef
	AuthorityEpoch int64  // the epoch the authorization saw
	Attempts       uint32 // replay attempts of the letter, this one included
}

// Replayer republishes a dead letter by event ID for an authorized human (ADR-BR-015). The envelope
// goes out unchanged, with its original event ID, so consumers deduplicate it like any redelivery.
//
// The scope is the Principal's, so RLS limits a replay to the operator's own tenant and company.
// No transaction stays open while publishing: one transaction authorizes and reads the letter,
// the publish follows, and a second transaction records the outcome only if the letter is still
// dead. Two concurrent replays may both publish; consumers drop the duplicate, the first to
// record wins and the other gets ErrAlreadyReplayed. A failed publish leaves the letter dead and
// records the error and the attempt.
type Replayer struct {
	DB             *bentopg.DB // a transaction owner of its own
	Publisher      eventing.Publisher
	Clock          application.Clock
	PublishTimeout time.Duration
}

// Replay republishes the dead letter of source and eventID in the scope of the Principal in ctx.
func (r Replayer) Replay(ctx context.Context, source, eventID string) (ReplayResult, error) {
	if r.DB == nil || r.Publisher == nil || r.Clock == nil || r.PublishTimeout <= 0 {
		return ReplayResult{}, errors.New("messaging: replayer misconfigured")
	}
	if source == "" || eventID == "" {
		return ReplayResult{}, fmt.Errorf("%w: source and event ID are required", ErrDeadLetterNotFound)
	}
	principal, err := identity.FromContext(ctx)
	if err != nil {
		return ReplayResult{}, err
	}
	policy, err := authorization.NewPolicy(ReplayRule())
	if err != nil {
		return ReplayResult{}, err
	}
	auth := authorization.Authorizer{Policy: policy}
	tx := bento.ScopeTransactor{DB: r.DB, Scope: principal.Scope}
	key := []any{string(principal.Scope.Tenant), string(principal.Scope.Company), source, eventID}

	var decision authorization.Decision
	var stored []byte
	err = tx.WithinTransaction(ctx, func(txCtx context.Context) error {
		exec, err := r.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		if decision, err = auth.Authorize(txCtx, exec, ReplayCommand); err != nil {
			return err
		}
		var state string
		err = exec.QueryRow(txCtx, readDeadLetter, key...).Scan(&state, &stored)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("%w: %s/%s in %s", ErrDeadLetterNotFound, source, eventID, principal.Scope)
		case err != nil:
			return err
		case state != stateDead:
			return fmt.Errorf("%w: %s/%s", ErrAlreadyReplayed, source, eventID)
		}
		return nil
	})
	if err != nil {
		return ReplayResult{}, err
	}

	var envelope eventing.Envelope
	publishErr := json.Unmarshal(stored, &envelope) // Bento's decoder validates the envelope
	if publishErr == nil {
		publishCtx, cancel := context.WithTimeout(ctx, r.PublishTimeout)
		publishErr = r.Publisher.Publish(publishCtx, envelope)
		cancel()
	}
	at := kernel.Instant(r.Clock.Now())

	// The publish happened or was tried; record it even if the caller has gone.
	result := ReplayResult{Source: source, EventID: eventing.EventID(eventID), ReplayedAt: at, ReplayedBy: principal.Actor, AuthorityEpoch: decision.Epoch}
	err = tx.WithinTransaction(context.WithoutCancel(ctx), func(txCtx context.Context) error {
		exec, err := r.DB.RequireExecutor(txCtx)
		if err != nil {
			return err
		}
		if publishErr != nil {
			_, err = exec.Exec(txCtx, recordReplayFailure, append(key, at, cutText(publishErr.Error(), maxErrorBytes))...)
			return err
		}
		var attempts int64
		err = exec.QueryRow(txCtx, markReplayed, append(key, at, string(principal.Actor.Kind), principal.Actor.ID, decision.Epoch)...).Scan(&attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s/%s by a concurrent replay", ErrAlreadyReplayed, source, eventID)
		}
		result.Attempts = uint32(attempts)
		return err
	})
	if publishErr != nil {
		return ReplayResult{}, errors.Join(fmt.Errorf("%w: %s/%s: %w", ErrReplayFailed, source, eventID, publishErr), err)
	}
	if err != nil {
		return ReplayResult{}, err
	}
	return result, nil
}
