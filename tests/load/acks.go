package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// ack is one acknowledged command of a writes run.
type ack struct {
	At             time.Time `json:"at"`
	BC             string    `json:"bc"`
	Command        string    `json:"command"`
	IdempotencyKey string    `json:"idempotency_key"`
}

// AckCheck compares a writes run's acknowledged commands with a (restored) database: which of them
// its command ledgers hold. After a restore to the end of the WAL archive, the acknowledged
// commands it lacks are the data the restore lost; the time between the oldest lost one and the
// newest kept one bounds the loss window (RPO as the client saw it).
type AckCheck struct {
	Tenant         string  `json:"tenant"`
	Acknowledged   int     `json:"acknowledged"`
	Present        int     `json:"present"`
	Lost           int     `json:"lost"`
	FirstAckAt     string  `json:"first_ack_at,omitempty"`
	LastAckAt      string  `json:"last_ack_at,omitempty"`
	LastPresentAt  string  `json:"last_present_ack_at,omitempty"`
	FirstLostAt    string  `json:"first_lost_ack_at,omitempty"`
	LostWindowS    float64 `json:"lost_window_s"`    // last acknowledged − first lost, 0 when nothing was lost
	LostOutOfOrder int     `json:"lost_before_kept"` // lost commands acknowledged before a kept one (must be 0)
}

func readAcks(path string) ([]ack, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []ack
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var a ack
		if err = json.Unmarshal(sc.Bytes(), &a); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, len(out)+1, err)
		}
		out = append(out, a)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, sc.Err()
}

// checkAcks looks each acknowledged command up in brant_<bc>.action_result of tenant, as the
// application role under the tenant's row-level security scope.
func checkAcks(ctx context.Context, dsn, tenant, path string) (AckCheck, error) {
	res := AckCheck{Tenant: tenant}
	acks, err := readAcks(path)
	if err != nil {
		return res, err
	}
	res.Acknowledged = len(acks)
	if len(acks) == 0 {
		return res, nil
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return res, err
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true), set_config('app.company_id', $2, true)`, tenant, company); err != nil {
		return res, err
	}
	byBC := map[string][]string{}
	for _, a := range acks {
		byBC[a.BC] = append(byBC[a.BC], a.IdempotencyKey)
	}
	present := map[string]bool{}
	for bc, keys := range byBC {
		rows, err := tx.Query(ctx, "SELECT command, idempotency_key FROM "+pgx.Identifier{"brant_" + bc, "action_result"}.Sanitize()+
			" WHERE idempotency_key = ANY($1)", keys)
		if err != nil {
			return res, err
		}
		for rows.Next() {
			var command, key string
			if err = rows.Scan(&command, &key); err != nil {
				rows.Close()
				return res, err
			}
			present[bc+" "+command+" "+key] = true
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return res, err
		}
	}
	var lastPresent, firstLost time.Time
	for _, a := range acks {
		if present[a.BC+" "+a.Command+" "+a.IdempotencyKey] {
			res.Present++
			lastPresent = a.At
			continue
		}
		res.Lost++
		if firstLost.IsZero() {
			firstLost = a.At
		}
	}
	for _, a := range acks {
		if !firstLost.IsZero() && a.At.Before(lastPresent) && !present[a.BC+" "+a.Command+" "+a.IdempotencyKey] {
			res.LostOutOfOrder++
		}
	}
	format := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.UTC().Format(time.RFC3339Nano)
	}
	res.FirstAckAt, res.LastAckAt = format(acks[0].At), format(acks[len(acks)-1].At)
	res.LastPresentAt, res.FirstLostAt = format(lastPresent), format(firstLost)
	if !firstLost.IsZero() {
		res.LostWindowS = round(acks[len(acks)-1].At.Sub(firstLost).Seconds(), 3)
	}
	return res, nil
}
