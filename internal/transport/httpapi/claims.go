package httpapi

import clmapi "go.idp.xyz/idp-brant/internal/contexts/clm/api"

// CLM is what the transport needs from the claims context. Claim reads stay in process: the
// Context Map lists no CLM query, so this transport registers no CLM GET route.
type CLM interface {
	clmapi.Commands
	clmapi.Queries
	clmapi.Reconciler
	clmapi.Auditor
}

func clmCommands(c CLM) *commandSet {
	return &commandSet{bc: "clm", contract: clmapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			clmapi.CommandOpenClaim:      reflected("", "", receiptOf[clmapi.Receipt](c.OpenClaim)),
			clmapi.CommandRegisterReturn: reflected("", "", receiptOf[clmapi.Receipt](c.RegisterReturn)),
			clmapi.CommandDecideRemedy:   reflected("", "", receiptOf[clmapi.Receipt](c.DecideRemedy)),
			clmapi.CommandCloseClaim:     reflected("", "", receiptOf[clmapi.Receipt](c.CloseClaim)),
		}}
}
