package httpapi

import (
	"errors"
	"net/http"

	"go.idp.xyz/idp-brant/internal/foundation/identity"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// aiWritingRouteReason is the one refusal every writing route gives an actor of kind ai. A plain
// route cannot name a delegation, so the agent's commands go through the AI gateway in process.
// The second clause is the task-step route's own words.
const aiWritingRouteReason = "plain routes take no command from an AI actor; an AI agent's go through the AI gateway"

// rejectedCommand is the command name a route audits: the command, or "invalid" when the route has
// no such command (the name the command route already uses for that refusal).
func rejectedCommand(set *commandSet, name string) string {
	if _, known := set.commands[name]; !known {
		return "invalid"
	}
	return name
}

// refuseAI refuses an actor of kind ai before the body is read. audit records the refusal the way
// that route records its other transport refusals; nil means the route does not audit them. A
// principal that is not ai is left to the route. It reports whether it answered the request.
func (s *server) refuseAI(w http.ResponseWriter, r *http.Request, audit func(code string) error) bool {
	p, err := identity.FromContext(r.Context())
	if err != nil || p.Actor.Kind != kernel.ActorAI {
		return false
	}
	rej := kernel.Reject(kernel.CodePermissionDenied, "/", aiWritingRouteReason)
	if audit != nil {
		if auditErr := audit(rej.Code); auditErr != nil {
			s.fail(w, r, errors.Join(rej, auditErr))
			return true
		}
	}
	s.reject(w, r, rej)
	return true
}
