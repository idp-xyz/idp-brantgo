package file

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// Python test_foundation.py test_file_signature_size_and_branching_are_explicit (signature half)
// and test_attachment_limit_and_large_valid_content.
func TestDecodeChecksNameTypeSignatureAndSize(t *testing.T) {
	for _, c := range []struct {
		name, mime, content string
		ok                  bool
	}{
		{"evidence.txt", "text/plain", b64("原文"), true},
		{"x.pdf", "application/pdf", b64("not a pdf"), false},
		{"x.pdf", "application/pdf", b64("%PDF-1.7 ..."), true},
		{"x.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", b64("PK\x03\x04..."), true},
		{"x.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", b64("plain"), false},
		{"x.TXT", "text/plain", b64("ok"), true},
		{"x.pdf", "text/plain", b64("ok"), false},
		{"../x.txt", "text/plain", b64("ok"), false},
		{"x.txt", "text/plain", "%%%bad", false},
		{"x.txt", "text/plain", "", false},
		{"x.txt", "text/plain", base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}), false},
		{"large.txt", "text/plain", b64(strings.Repeat("A", 1100*1024)), true},
		{"too.large.txt", "text/plain", b64(strings.Repeat("A", 10<<20+1)), false},
	} {
		_, hash, err := Decode(c.name, c.mime, c.content, 10<<20)
		if (err == nil) != c.ok || (err != nil && !errors.As(err, new(Invalid))) || (c.ok && len(hash) != 64) {
			t.Errorf("%s %s: %v", c.name, c.mime, err)
		}
	}
}
