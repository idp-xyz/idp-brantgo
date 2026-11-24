// Package plugin is the trade-extension platform (brant_ext). It is not a bounded context:
// contexts call the guard it publishes and never import a plugin implementation.
package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"go.idp.xyz/idp-brant/internal/kernel"
)

// Contract is the command envelope's contract member.
const Contract = "brant.ext/0.1.0"

// Source is the platform's event source. Enable, enter-readonly and disable publish no events.
const Source = "idp-brant.ext"

// CoreVersion is the platform core the compatibility matrix is keyed by. A plugin is compatible
// only with an exact (core, contract, plugin) triple, never a semantic-version range.
const CoreVersion = "brant/0.1.0"

// ContractTradeExtension is trade-extension.v1.
const ContractTradeExtension = "trade-extension.v1"

// ApparelPluginID is the published industry.apparel plugin the document guards name.
const ApparelPluginID = "industry.apparel"

// CapabilityCost is the capability that makes a plugin a cost provider for its document profile.
const CapabilityCost = "cost.estimate.v1"

// Commands.
const (
	CommandEnable        = "plugin.enable"
	CommandEnterReadOnly = "plugin.enter_readonly"
	CommandDisable       = "plugin.disable"
	CommandList          = "ext.read"
	CommandAudit         = "ext.audit"
)

// Rejection codes registered in the error catalog. HTTP follows the category.
const (
	CodeCostProviderConflict      = "COST_PROVIDER_CONFLICT"
	CodeDocumentProfileMismatch   = "DOCUMENT_PROFILE_MISMATCH"
	CodePluginNotEnabled          = "PLUGIN_NOT_ENABLED"
	CodePluginInUse               = "PLUGIN_IN_USE"
	CodePluginVersionIncompatible = "PLUGIN_VERSION_INCOMPATIBLE"
)

// Activation is how a company turns a plugin on: one document profile per activation.
type Activation struct {
	Scope           string `json:"scope"`
	DocumentProfile string `json:"document_profile"`
}

// Requirement is one contract a plugin declares it needs.
type Requirement struct {
	ID              string `json:"id"`
	ContractVersion string `json:"contract_version"`
}

// PluginManifest is the closed manifest of Baseline Appendix B. Unknown fields are not stored.
type PluginManifest struct {
	ID              string
	Version         string
	ContractVersion string
	Requires        []Requirement
	OptionalBridges []string
	Capabilities    []string
	Activation      Activation
	TaskProfileRef  string
	PublisherID     string
	// Checks are the rule identifiers the plugin registers. A second plugin may not reuse one.
	Checks []string
}

// ProvidesCost reports whether the manifest supplies cost.estimate.v1 for its document profile.
func (m PluginManifest) ProvidesCost() bool {
	return slices.Contains(m.Capabilities, CapabilityCost)
}

// Profile is the manifest's document profile, or generic when it names none.
func (m PluginManifest) Profile() DocumentProfile {
	p, err := Canonical(m.Activation.DocumentProfile, "/")
	if err != nil {
		return ProfileGeneric
	}
	return p
}

type manifestDoc struct {
	ID              string        `json:"id"`
	Version         string        `json:"version"`
	ContractVersion string        `json:"contract_version"`
	Requires        []Requirement `json:"requires"`
	OptionalBridges []string      `json:"optional_bridges"`
	Capabilities    []string      `json:"capabilities"`
	Activation      Activation    `json:"activation"`
	TaskProfileRef  string        `json:"task_profile_ref"`
	PublisherID     string        `json:"publisher_id"`
}

// SHA256 is the SHA-256 of the canonical manifest document. Check identifiers are not part of it:
// they are a process registration, not the published manifest.
func (m PluginManifest) SHA256() string {
	doc := manifestDoc{
		ID: m.ID, Version: m.Version, ContractVersion: m.ContractVersion,
		Requires: append([]Requirement(nil), m.Requires...), OptionalBridges: append([]string(nil), m.OptionalBridges...),
		Capabilities: append([]string(nil), m.Capabilities...), Activation: m.Activation,
		TaskProfileRef: m.TaskProfileRef, PublisherID: m.PublisherID,
	}
	if doc.Requires == nil {
		doc.Requires = []Requirement{}
	}
	if doc.OptionalBridges == nil {
		doc.OptionalBridges = []string{}
	}
	if doc.Capabilities == nil {
		doc.Capabilities = []string{}
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic("plugin: manifest does not encode: " + err.Error())
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Validate checks the closed manifest. A failure is SCHEMA_INVALID.
func (m PluginManifest) Validate() error {
	switch {
	case !kernel.ValidIdentifier(m.ID):
		return kernel.Reject(kernel.CodeSchemaInvalid, "/id", "")
	case !versionText(m.Version) || !versionText(m.ContractVersion):
		return kernel.Reject(kernel.CodeSchemaInvalid, "/version", "")
	case m.Activation.Scope != "company":
		return kernel.Reject(kernel.CodeSchemaInvalid, "/activation/scope", "")
	case m.PublisherID == "" || len(m.PublisherID) > 64:
		return kernel.Reject(kernel.CodeSchemaInvalid, "/publisher_id", "")
	}
	if _, err := Canonical(m.Activation.DocumentProfile, "/activation/document_profile"); err != nil {
		return err
	}
	seen := map[string]bool{}
	for i, r := range m.Requires {
		if !versionText(r.ID) || !versionText(r.ContractVersion) || seen["req:"+r.ID] {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/requires", "")
		}
		seen["req:"+r.ID] = true
		_ = i
	}
	for _, id := range m.Capabilities {
		if !versionText(id) || seen["cap:"+id] {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/capabilities", "")
		}
		seen["cap:"+id] = true
	}
	for _, id := range m.OptionalBridges {
		if !versionText(id) || seen["bridge:"+id] {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/optional_bridges", "")
		}
		seen["bridge:"+id] = true
	}
	for _, id := range m.Checks {
		if !versionText(id) || seen["check:"+id] {
			return kernel.Reject(kernel.CodeSchemaInvalid, "/checks", "")
		}
		seen["check:"+id] = true
	}
	if m.TaskProfileRef != "" && !versionText(m.TaskProfileRef) {
		return kernel.Reject(kernel.CodeSchemaInvalid, "/task_profile_ref", "")
	}
	return nil
}

func versionText(s string) bool {
	return s != "" && len(s) <= 64 && !strings.ContainsAny(s, " \t\r\n")
}

// Triple is one tested (core, contract, plugin) combination.
type Triple struct {
	CoreVersion     string
	ContractVersion string
	PluginVersion   string
}

// Supported is the exact compatibility matrix. Nothing else enables.
func Supported() []Triple {
	return []Triple{{
		CoreVersion: CoreVersion, ContractVersion: ContractTradeExtension, PluginVersion: "0.1.0",
	}}
}

// Compatible reports whether m is one of the triples for core.
func Compatible(core string, m PluginManifest) bool {
	for _, t := range Supported() {
		if t.CoreVersion == core && t.ContractVersion == m.ContractVersion && t.PluginVersion == m.Version {
			return true
		}
	}
	return false
}

// ApparelManifest is the published industry.apparel 0.1.0 declaration. The implementation lives in
// internal/plugins/apparel (S7-03); the platform registers the declaration so a company can enable it.
func ApparelManifest() PluginManifest {
	return PluginManifest{
		ID: ApparelPluginID, Version: "0.1.0", ContractVersion: ContractTradeExtension,
		Requires: []Requirement{
			{ID: "trade.core", ContractVersion: "brant.trd/0.1.0"},
			{ID: "samples", ContractVersion: "brant.cat/0.1.0"},
			{ID: "cost_bom", ContractVersion: "brant.cst/0.1.0"},
		},
		OptionalBridges: []string{"apparel.crm.bridge"},
		Capabilities: []string{
			CapabilityCost, "quote.validate.v1", "packing.validate.v1", "inspection.validate.v1",
			"industry.semantics.v1", "context.provider.v1", "task.profile.v1", "outcome.check.v1",
		},
		Activation:     Activation{Scope: "company", DocumentProfile: string(ProfileApparel)},
		TaskProfileRef: "task.profile.apparel.q01@0.1.0",
		PublisherID:    "vendor.official",
	}
}

// VersionIncompatible refuses a manifest outside Supported.
func VersionIncompatible() *kernel.Rejection {
	return kernel.RejectAs(CodePluginVersionIncompatible, kernel.CategoryConflict, "/args/plugin_id",
		"The plugin version is not a tested combination with this core")
}

// CostConflict refuses a second active cost provider for one document profile.
func CostConflict() *kernel.Rejection {
	return kernel.RejectAs(CodeCostProviderConflict, kernel.CategoryConflict, "/args/plugin_id",
		"Another cost provider is already active for this document profile")
}

// NotEnabled refuses an apparel document while the plugin is not active.
func NotEnabled(path string) *kernel.Rejection {
	return kernel.RejectAs(CodePluginNotEnabled, kernel.CategoryInvalid, path, "The industry plugin is not active")
}

// InUse refuses plugin.disable while a document or task of the profile is still open.
func InUse() *kernel.Rejection {
	return kernel.RejectAs(CodePluginInUse, kernel.CategoryConflict, "/args/plugin_id",
		"The industry plugin still has work in flight")
}

// ProfileMismatch refuses a document that mixes or crosses document profiles.
func ProfileMismatch(path string) *kernel.Rejection {
	return kernel.RejectAs(CodeDocumentProfileMismatch, kernel.CategoryInvalid, path,
		"Split the documents by document profile")
}
