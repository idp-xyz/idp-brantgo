package plugin

import "go.idp.xyz/idp-brant/internal/kernel"

// Command is a plugin command the transport reconciles.
type Command interface{ extCommand() }

// Receipt is the closed success answer of a plugin command.
type Receipt struct {
	ActionID   string
	ObjectID   string
	RowVersion int64
	Status     string
	Replayed   bool
}

// Reconciliation is what became of a command, without running it again.
type Reconciliation struct {
	State    kernel.ReconcileState
	ActionID string
	ObjectID string
	Receipt  *Receipt
}

// EnablePlugin turns a registered plugin on for the caller's company.
type EnablePlugin struct {
	IdempotencyKey string
	PluginID       string
}

func (EnablePlugin) extCommand() {}

// EnterReadOnlyPlugin moves an active plugin to draining_readonly.
type EnterReadOnlyPlugin struct {
	IdempotencyKey string
	PluginID       string
}

func (EnterReadOnlyPlugin) extCommand() {}

// DisablePlugin moves an active or draining plugin to disabled when nothing of its profile is open.
type DisablePlugin struct {
	IdempotencyKey string
	PluginID       string
}

func (DisablePlugin) extCommand() {}

// PluginState is one registered plugin and the company's activation, if any.
type PluginState struct {
	PluginID        string
	Version         string
	ContractVersion string
	DocumentProfile string
	Status          string
	ManifestSHA256  string
	CostProvider    bool
	RowVersion      int64
}

// PluginList is the company's plugin catalogue.
type PluginList struct {
	Plugins []PluginState
}

func (c EnablePlugin) Validate() error        { return head(c.IdempotencyKey, c.PluginID) }
func (c EnterReadOnlyPlugin) Validate() error { return head(c.IdempotencyKey, c.PluginID) }
func (c DisablePlugin) Validate() error       { return head(c.IdempotencyKey, c.PluginID) }

func head(key, id string) error {
	switch {
	case !kernel.ValidIdentifier(key):
		return kernel.Reject(kernel.CodeSchemaInvalid, "/idempotency_key", "")
	case !kernel.ValidIdentifier(id):
		return kernel.Reject(kernel.CodeSchemaInvalid, "/args/plugin_id", "")
	}
	return nil
}
