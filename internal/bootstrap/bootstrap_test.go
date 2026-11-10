package bootstrap

import (
	"context"
	"testing"
	"time"
)

func TestOpenRequiresADSN(t *testing.T) {
	if _, err := Open(context.Background(), Config{}); err == nil {
		t.Fatal("Open without a DSN must fail")
	}
}

func TestReplayerRequiresSinkAndTimeout(t *testing.T) {
	for _, cfg := range []ReplayConfig{{}, {SinkURL: "http://sink"}, {PublishTimeout: time.Second}} {
		if _, err := (&Platform{}).Replayer(cfg); err == nil {
			t.Errorf("Replayer(%+v) must fail", cfg)
		}
	}
}
