package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// TextExtractor is the text reader and its version.
const TextExtractor = "brant.ingest.text/1"

// ExtractText reads a plain text file (zh/en inquiry text, baseline 14.2): UTF-8, a leading byte
// order mark dropped, CR LF and CR read as line ends. Each line is a segment at "line:1", empty
// lines included, so that line numbers are those of the file; a final line end does not open an
// empty last line. Control characters other than tab (C0, DEL, C1) are refused with their locator,
// and so are a file of more than MaxCharacters characters and a file with nothing but white space.
func (p Profile) ExtractText(content []byte) (*Extract, error) {
	if err := p.checkSize(content); err != nil {
		return nil, err
	}
	if !utf8.Valid(content) {
		return nil, p.refuse(LimitEncoding, "")
	}
	text := strings.TrimPrefix(string(content), "\ufeff")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.TrimSuffix(text, "\n")
	if strings.TrimSpace(text) == "" {
		return nil, p.refuse(LimitEmpty, "")
	}
	if n := utf8.RuneCountInString(text); n > p.MaxCharacters {
		return nil, p.over(LimitCharacters, "", int64(n), int64(p.MaxCharacters), false)
	}
	lines := strings.Split(text, "\n")
	e := &Extract{Kind: KindText, Extractor: TextExtractor, Profile: p.ID, FileSHA256: sha(content)}
	e.Segments = make([]Segment, len(lines))
	for i, line := range lines {
		col := 1
		for _, r := range line {
			if control(r) {
				return nil, p.refuse(LimitEncoding, TextLocator(i+1, col))
			}
			col++
		}
		e.Segments[i] = Segment{Locator: TextLocator(i+1, 1), Text: line}
	}
	return e.seal(), nil
}

// control reports a control character that is not text: C0 other than tab and line feed, DEL and
// C1. NUL in particular cannot be stored in PostgreSQL text.
func control(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n') || (r >= 0x7f && r <= 0x9f)
}

func (p Profile) checkSize(content []byte) error {
	if len(content) == 0 || len(content) > p.MaxFileBytes {
		return p.over(LimitFileBytes, "", int64(len(content)), int64(p.MaxFileBytes), false)
	}
	return nil
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
