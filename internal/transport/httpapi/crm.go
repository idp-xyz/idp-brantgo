package httpapi

import crmapi "go.idp.xyz/idp-brant/internal/contexts/crm/api"

// CRM is what the transport needs from the optional CRM context. Nil on Config leaves the
// commands unregistered (BC-AT24): a human POST is UNSUPPORTED_CONTRACT, an AI POST is
// PERMISSION_DENIED before the body is read, and a GET under /crm/ is NOT_FOUND. There is no
// CRM GET route; GetLead and GetOpportunity stay in process.
type CRM interface {
	crmapi.Commands
	crmapi.Queries
	crmapi.Auditor
	crmapi.Reconciler
}

func crmCommands(c CRM) *commandSet {
	// CRM names its objects lead_id, opportunity_id and follow_up_id, so no member is renamed to id.
	return &commandSet{bc: "crm", contract: crmapi.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile), commands: map[string]commandFunc{
		crmapi.CommandCaptureLead:        reflected("", "", receiptOf[crmapi.Receipt](c.CaptureLead)),
		crmapi.CommandAdvanceOpportunity: reflected("", "", receiptOf[crmapi.Receipt](c.AdvanceOpportunity)),
		crmapi.CommandRecordFollowUp:     reflected("", "", receiptOf[crmapi.Receipt](c.RecordFollowUp)),
	}}
}
