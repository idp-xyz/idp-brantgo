package api

import (
	"strings"
	"testing"

	"go.idp.xyz/idp-brant/internal/kernel"
)

func code(err error) string {
	if r, ok := kernel.AsRejection(err); ok {
		return r.Code + " " + r.Fields[0].Path
	}
	return "OK"
}

func listImport() ImportScreeningList {
	return ImportScreeningList{IdempotencyKey: "k1", ListCode: "un.consolidated", Jurisdiction: JurisdictionUN, Version: "2026-11-01",
		SourceName: "UN Security Council Consolidated List", SourceURL: "https://scsanctions.un.org/consolidated/", PublishedOn: "2026-11-01",
		FileSHA256: strings.Repeat("ab", 32), EntryCount: 1,
		Entries: []ScreeningEntry{{EntryID: "QDe.001", PrimaryName: "Synthetic Entity", Aliases: []string{"SE"}, EntityType: EntityTypeEntity}}}
}

// S1-11: a list version is imported as a structured list with its source, publication date, file
// hash and entry count.
func TestImportContract(t *testing.T) {
	for _, c := range []struct {
		edit func(*ImportScreeningList)
		want string
	}{
		{func(*ImportScreeningList) {}, "OK"},
		{func(c *ImportScreeningList) { c.ListCode = strings.Repeat("x", MaxListCodeLength+1) }, "SCHEMA_INVALID /args/list_code"},
		{func(c *ImportScreeningList) { c.Jurisdiction = "US" }, "SCHEMA_INVALID /args/jurisdiction"},
		{func(c *ImportScreeningList) { c.Version = strings.Repeat("1", MaxVersionLength+1) }, "SCHEMA_INVALID /args/version"},
		{func(c *ImportScreeningList) { c.SourceName = " " }, "SCHEMA_INVALID /args/source_name"},
		{func(c *ImportScreeningList) { c.SourceURL = "ftp://example.org/list" }, "SCHEMA_INVALID /args/source_url"},
		{func(c *ImportScreeningList) { c.SourceURL = "https://user:pw@example.org/" }, "SCHEMA_INVALID /args/source_url"},
		{func(c *ImportScreeningList) { c.PublishedOn = "2026-11-1" }, "SCHEMA_INVALID /args/published_on"},
		{func(c *ImportScreeningList) { c.FileSHA256 = strings.Repeat("AB", 32) }, "SCHEMA_INVALID /args/file_sha256"},
		{func(c *ImportScreeningList) { c.EntryCount = 2 }, "INVALID_KNOWN_VALUE /args/entry_count"},
		{func(c *ImportScreeningList) { c.EntryCount, c.Entries = 0, nil }, "SCHEMA_INVALID /args/entry_count"},
		{func(c *ImportScreeningList) { c.Entries[0].PrimaryName = "--" }, "SCHEMA_INVALID /args/entries/0/primary_name"},
		{func(c *ImportScreeningList) { c.Entries[0].Aliases = []string{"A", "A"} }, "SCHEMA_INVALID /args/entries/0/aliases/1"},
		{func(c *ImportScreeningList) { c.Entries[0].EntityType = "company" }, "SCHEMA_INVALID /args/entries/0/entity_type"},
		{func(c *ImportScreeningList) { c.Entries[0].Country = "cn" }, "SCHEMA_INVALID /args/entries/0/country"},
		{func(c *ImportScreeningList) {
			c.EntryCount, c.Entries = 2, append(c.Entries, c.Entries[0])
		}, "SCHEMA_INVALID /args/entries/1/entry_id"},
	} {
		cmd := listImport()
		c.edit(&cmd)
		if got := code(cmd.Validate()); got != c.want {
			t.Errorf("%s: want %s", got, c.want)
		}
	}
	if !kernel.ValidIdentifier(ObjectID(strings.Repeat("l", MaxListCodeLength), strings.Repeat("v", MaxVersionLength))) {
		t.Fatal("the longest object ID must stay an identifier")
	}
}
