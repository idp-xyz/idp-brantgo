// Package file is the BusinessFile aggregate: immutable bytes of a declared type. A new version
// names the file it supersedes and a file has at most one successor, so versions form a chain,
// never a tree (EVD-INV-001; Python foundation.file_register).
package file

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Types a business file may declare, with the extension its name must end in.
var extensions = map[string]string{
	"text/plain":      ".txt",
	"application/pdf": ".pdf",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx",
}

// Invalid is returned for bytes that are not valid Base64, empty, too large or not of the declared
// type, and for a file name the type does not allow.
type Invalid struct{ Reason string }

func (e Invalid) Error() string { return "file: " + e.Reason }

// Decode checks name, type and content and returns the bytes and their SHA-256. maxBytes bounds
// the decoded size.
func Decode(name, mimeType, contentBase64 string, maxBytes int) ([]byte, string, error) {
	ext, ok := extensions[mimeType]
	switch {
	case !ok:
		return nil, "", Invalid{"type"}
	case strings.ContainsAny(name, "/\\\x00"):
		return nil, "", Invalid{"name holds a path separator"}
	case !strings.HasSuffix(strings.ToLower(name), ext):
		return nil, "", Invalid{"name does not end in " + ext}
	case base64.StdEncoding.DecodedLen(len(contentBase64)) > maxBytes+2:
		return nil, "", Invalid{"too large"}
	}
	content, err := base64.StdEncoding.Strict().DecodeString(contentBase64)
	switch {
	case err != nil:
		return nil, "", Invalid{"not Base64"}
	case len(content) == 0 || len(content) > maxBytes:
		return nil, "", Invalid{"empty or too large"}
	case mimeType == "text/plain" && !utf8.Valid(content):
		return nil, "", Invalid{"text is not UTF-8"}
	case mimeType == "application/pdf" && !bytes.HasPrefix(content, []byte("%PDF-")):
		return nil, "", Invalid{"not a PDF"}
	case strings.HasSuffix(mimeType, "sheet") && !bytes.HasPrefix(content, []byte("PK\x03\x04")):
		return nil, "", Invalid{"not an XLSX package"}
	}
	sum := sha256.Sum256(content)
	return content, hex.EncodeToString(sum[:]), nil
}
