package httpapi

import stkapi "go.idp.xyz/idp-brant/internal/contexts/stk/api"

// STK is what the transport needs from the stock context. Nil leaves its commands out.
type STK interface {
	stkapi.Commands
	stkapi.Queries
	stkapi.Reconciler
	stkapi.Auditor
}

func stkCommands(c STK) *commandSet {
	return &commandSet{bc: "stk", contract: stkapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			stkapi.CommandConfigure: reflected("", "", receiptOf[stkapi.Receipt](c.ConfigureModule)),
			stkapi.CommandReceive:   reflected("", "", receiptOf[stkapi.Receipt](c.ReceiveStock)),
			stkapi.CommandIssue:     reflected("", "", receiptOf[stkapi.Receipt](c.IssueStock)),
			stkapi.CommandAdjust:    reflected("", "", receiptOf[stkapi.Receipt](c.AdjustStock)),
			stkapi.CommandPlace:     reflected("", "", receiptOf[stkapi.Receipt](c.PlaceReservation)),
			stkapi.CommandRelease:   reflected("", "", receiptOf[stkapi.Receipt](c.ReleaseReservation)),
		}}
}
