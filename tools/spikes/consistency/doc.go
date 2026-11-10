// Package consistency holds the M0-10 spikes behind two blocking decisions:
//
//   - ADR-BR-004: how a downstream context re-checks an upstream context's current qualification
//     at the moment its own transaction commits (SC-04: TRD activates an order while CST denies the
//     cost snapshot it depends on);
//   - ADR-BR-011: the PostgreSQL 16 lock order that replaces SQLite's BEGIN IMMEDIATE (SC-03: TRD
//     replaces a quote while another request converts it into an order).
//
// The scenarios are integration tests against PostgreSQL 16 at READ COMMITTED, the isolation level
// of IDP-Bento-Go transactions. Each run provisions throwaway copies of the tables CST and TRD will
// own; nothing here is production code.
//
//	go test -tags integration -count=1 -v ./tools/spikes/consistency
//
// Set BRANT_SPIKE_REPORT to a file path to receive the outcomes as JSON.
package consistency
