package httpapi

import (
	"context"
	"net/http"

	"go.idp.xyz/idp-brant/internal/foundation/plugin"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// EXT is the plugin platform the transport calls. It is not a bounded context.
type EXT interface {
	Enable(context.Context, plugin.EnablePlugin) (plugin.Receipt, error)
	EnterReadOnly(context.Context, plugin.EnterReadOnlyPlugin) (plugin.Receipt, error)
	Disable(context.Context, plugin.DisablePlugin) (plugin.Receipt, error)
	ListPlugins(context.Context) (plugin.PluginList, error)
	Audit(context.Context) ([]kernel.AuditEntry, error)
	Reconcile(context.Context, plugin.Command) (plugin.Reconciliation, error)
	AuditRejected(context.Context, string, string, string) error
}

func extCommands(c EXT) *commandSet {
	return &commandSet{bc: "ext", contract: plugin.Contract, auditor: c, audit: c.Audit, reconcile: reconciler(c.Reconcile),
		commands: map[string]commandFunc{
			plugin.CommandEnable:        reflected("", "", receiptOf[plugin.Receipt](c.Enable)),
			plugin.CommandEnterReadOnly: reflected("", "", receiptOf[plugin.Receipt](c.EnterReadOnly)),
			plugin.CommandDisable:       reflected("", "", receiptOf[plugin.Receipt](c.Disable)),
		}}
}

func (s *server) plugins(w http.ResponseWriter, r *http.Request) {
	v, err := s.cfg.EXT.ListPlugins(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, okView(v))
}
