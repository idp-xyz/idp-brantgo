package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"go.idp.xyz/idp-brant/internal/contexts/sup/api"
	"go.idp.xyz/idp-brant/internal/contexts/sup/internal/domain/revision"
	"go.idp.xyz/idp-brant/internal/kernel"
)

// document is the content of a supply condition revision: exactly the member of its kind is set.
type document struct {
	kind      string
	offer     *api.SupplierOffer
	terms     *api.PurchaseTerms
	logistics *api.LogisticsOffer
}

// member returns the content of d's kind.
func (d document) member() any {
	switch d.kind {
	case api.KindSupplierOffer:
		return d.offer
	case api.KindPurchaseTerms:
		return d.terms
	}
	return d.logistics
}

// validity returns the validity bounds of the content as given.
func (d document) validity() (from, until string) {
	switch d.kind {
	case api.KindSupplierOffer:
		return d.offer.ValidFrom, d.offer.ValidUntil
	case api.KindPurchaseTerms:
		return d.terms.ValidFrom, d.terms.ValidUntil
	}
	return d.logistics.ValidFrom, d.logistics.ValidUntil
}

// canonical returns d with its timestamps in canonical form, so equal content always hashes
// equally; the contract admitted them already.
func (d document) canonical() document {
	switch d.kind {
	case api.KindSupplierOffer:
		v := *d.offer
		v.ValidFrom, v.ValidUntil = canonicalTime(v.ValidFrom), canonicalTime(v.ValidUntil)
		d.offer = &v
	case api.KindPurchaseTerms:
		v := *d.terms
		v.ValidFrom, v.ValidUntil = canonicalTime(v.ValidFrom), canonicalTime(v.ValidUntil)
		d.terms = &v
	default:
		v := *d.logistics
		v.ValidFrom, v.ValidUntil = canonicalTime(v.ValidFrom), canonicalTime(v.ValidUntil)
		d.logistics = &v
	}
	return d
}

func canonicalTime(s string) string {
	if t, err := kernel.ParseTimestamp(s); err == nil {
		return kernel.FormatTimestamp(t)
	}
	return s
}

// encode returns the canonical content of d: its JSON (the snake_case members of the api), the
// SHA-256 of the JSON of its kind and data, and the header the guard reads.
func (d document) encode() (revision.Content, error) {
	d = d.canonical()
	data, err := json.Marshal(d.member())
	if err != nil {
		return revision.Content{}, err
	}
	doc, err := json.Marshal(struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}{d.kind, data})
	if err != nil {
		return revision.Content{}, err
	}
	sum := sha256.Sum256(doc)
	c := revision.Content{Data: data, Hash: hex.EncodeToString(sum[:])}
	from, until := d.validity()
	c.Header.ValidFrom, c.Header.ValidUntil = instant(from), instant(until)
	if t := d.terms; t != nil {
		c.Header.ValidityMode = t.ValidityMode
		if r := t.SourceOfferRef; r != nil {
			c.Header.SourceID, c.Header.SourceRevision = r.ID, r.Revision
		}
	}
	return c, nil
}

// decode returns the stored content of a revision of kind.
func decode(kind string, raw []byte) (document, error) {
	d := document{kind: kind}
	var err error
	switch kind {
	case api.KindSupplierOffer:
		d.offer = &api.SupplierOffer{}
		err = json.Unmarshal(raw, d.offer)
	case api.KindPurchaseTerms:
		d.terms = &api.PurchaseTerms{}
		err = json.Unmarshal(raw, d.terms)
	case api.KindLogisticsOffer:
		d.logistics = &api.LogisticsOffer{}
		err = json.Unmarshal(raw, d.logistics)
	default:
		err = fmt.Errorf("sup: no content of kind %q", kind)
	}
	return d, err
}

// hashJSON returns the canonical JSON of v and its SHA-256.
func hashJSON(v any) ([]byte, string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// instant parses a timestamp the contract admitted; empty is the zero time.
func instant(s string) time.Time {
	t, err := kernel.ParseTimestamp(s)
	if err != nil {
		return time.Time{}
	}
	return kernel.Instant(t)
}

func toEvidence(ev []api.Evidence) []revision.Evidence {
	out := make([]revision.Evidence, len(ev))
	for i, e := range ev {
		out[i] = revision.Evidence(e)
	}
	return out
}

func fromEvidence(ev []revision.Evidence) []api.Evidence {
	out := make([]api.Evidence, len(ev))
	for i, e := range ev {
		out[i] = api.Evidence(e)
	}
	return out
}

func fromAct(a *revision.Act) *api.Act {
	if a == nil {
		return nil
	}
	return &api.Act{By: a.By, AuthorityEpoch: a.Epoch, At: a.At, Reason: a.Reason, Evidence: fromEvidence(a.Evidence)}
}
