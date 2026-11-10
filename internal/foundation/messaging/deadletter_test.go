package messaging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"go.idp.xyz/idp-bento-go/eventing"
	"go.idp.xyz/idp-bento-go/testkit"
)

// ADR-BR-015: revocation, repudiation and risk events alert as critical.
func TestCriticalEventTypes(t *testing.T) {
	if got := strings.Join(CriticalMarkers(), " "); got != "revoked repudiat risk withdrawn cancelled deactivated" {
		t.Fatalf("CriticalMarkers = %s", got)
	}
	for _, typ := range []eventing.EventType{
		"brant.pty.qualification_revoked", "brant.evd.evidence_repudiated", "brant.evd.repudiation_recorded",
		"brant.pol.risk_flagged", "brant.inq.inquiry_withdrawn", "brant.inq.quote_cancelled",
		"brant.pty.party_deactivated", "Brant.PTY.Grant_REVOKED",
	} {
		if !CriticalEventType(typ) {
			t.Errorf("%s must be critical", typ)
		}
	}
	for _, typ := range []eventing.EventType{"brant.inq.quote_activated", "brant.pty.party_registered", "brant.relay_probe.recorded", ""} {
		if CriticalEventType(typ) {
			t.Errorf("%s must not be critical", typ)
		}
	}
	custom := TypeContains("Suspended", "")
	if !custom("brant.pty.party_suspended") || custom("brant.pty.party_revoked") || custom("") {
		t.Fatal("TypeContains matches its own markers only, ignoring case and empty markers")
	}
	markers := CriticalMarkers()
	markers[0] = "changed"
	if !CriticalEventType("x.revoked") {
		t.Fatal("CriticalMarkers must return a copy")
	}
}

func TestSlogAlerterLogsAtError(t *testing.T) {
	var buf bytes.Buffer
	SlogAlerter{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}.Alert(context.Background(), DeadLetterAlert{
		Source: "idp-brant.pty", EventID: "e-1", EventType: "brant.pty.qualification_revoked", Scope: "t/c",
		Code: FailurePublishRejected, Attempt: 2, Failures: 1, Critical: true, Stored: true,
	})
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line %q: %v", buf.String(), err)
	}
	if line["level"] != "ERROR" || line["msg"] != "outbox delivery dead-lettered" || line["critical"] != true ||
		line["event_id"] != "e-1" || line["failure_code"] != "publish.rejected" {
		t.Fatalf("log line = %v", line)
	}
	buf.Reset()
	SlogAlerter{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}.Alert(context.Background(), DeadLetterAlert{EventID: "e-2", Critical: true})
	if !strings.Contains(buf.String(), `"msg":"outbox delivery abandoned without a dead letter"`) || !strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Fatalf("unstored alert = %s", buf.String())
	}
}

func TestEncodeEnvelopeUsesBentoEncoding(t *testing.T) {
	e := testkit.ValidEnvelope()
	got, err := encodeEnvelope(e)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(e)
	if !bytes.Equal(got, want) {
		t.Fatalf("encoding = %s, want Bento's %s", got, want)
	}
	var back eventing.Envelope
	if err = json.Unmarshal(got, &back); err != nil || back.ID != e.ID || string(back.Payload) != string(e.Payload) {
		t.Fatalf("decode = %+v, %v", back, err)
	}
}

// Bento refuses to encode an invalid envelope; the dead letter keeps it under the same names.
func TestEncodeEnvelopeKeepsInvalidEnvelope(t *testing.T) {
	e := testkit.ValidEnvelope()
	e.Subject = "bad\tsubject"
	got, err := encodeEnvelope(e)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err = json.Unmarshal(got, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["id"] != string(e.ID) || fields["subject"] != "bad\tsubject" || fields["payload"].(map[string]any)["value"] != "test" {
		t.Fatalf("fields = %v", fields)
	}
	var decoded eventing.Envelope
	if err = json.Unmarshal(got, &decoded); err == nil {
		t.Fatal("an invalid envelope must not decode as valid")
	}

	e.Payload = []byte("not json \x00")
	if got, err = encodeEnvelope(e); err != nil || !strings.Contains(string(got), `"payload":"not json "`) {
		t.Fatalf("non-JSON payload = %s, %v", got, err)
	}
}

func TestCutTextKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("错", 400) // 3 bytes each
	got := cutText(long, maxErrorBytes)
	if len(got) > maxErrorBytes || !utf8.ValidString(got) || len(got) != 1023 {
		t.Fatalf("cut to %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if got := cutText("a\x00b\xffc", 10); got != "ab\uFFFDc" {
		t.Fatalf("cleaned = %q", got)
	}
}
