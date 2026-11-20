package httpapi

import mfgapi "go.idp.xyz/idp-brant/internal/contexts/mfg/api"

// MFG is what the transport needs from the manufacturing context. Nil leaves its commands out.
// material.handover is in process only and is not registered.
type MFG interface {
	mfgapi.Commands
	mfgapi.Queries
	mfgapi.Reconciler
	mfgapi.Auditor
}

func mfgCommands(c MFG) *commandSet {
	return &commandSet{bc: "mfg", contract: mfgapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			mfgapi.CommandConfigure: reflected("", "", receiptOf[mfgapi.Receipt](c.ConfigureModule)),
			mfgapi.CommandPublish:   reflected("", "", receiptOf[mfgapi.Receipt](c.PublishPlan)),
			mfgapi.CommandIssue:     reflected("", "", receiptOf[mfgapi.Receipt](c.IssueSubcontract)),
			mfgapi.CommandProgress:  reflected("", "", receiptOf[mfgapi.Receipt](c.RecordProgress)),
		}}
}
