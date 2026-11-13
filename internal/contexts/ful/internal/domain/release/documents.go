package release

// BillOfLadingRef is the issued bill of lading record of the shipment's document set a documents
// release was recorded on (document_release.record; WBS S5-08 with S5-07): the set, the B/L
// record's version, its number and the SHA-256 of its content.
type BillOfLadingRef struct {
	DocumentSetID string
	Version       int64
	Number        string
	ContentSHA256 string
}
