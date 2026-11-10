package pgtest

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// FaultMode is how a CommitFault loses a COMMIT.
type FaultMode int32

const (
	// LoseAnswer sends the COMMIT, waits for the server's answer and loses it: the transaction
	// committed, the client cannot know.
	LoseAnswer FaultMode = iota + 1
	// LoseCommit closes the connection instead of sending the COMMIT: the server rolls the
	// transaction back, the client cannot know.
	LoseCommit
)

// CommitFault makes the next COMMIT on the connections of a FaultyAppPool end without an answer,
// as a network failure between the commit and its acknowledgement would.
type CommitFault struct {
	mode      atomic.Int32
	fired     atomic.Bool
	committed atomic.Bool
}

// Arm sets the fault for the next COMMIT.
func (f *CommitFault) Arm(m FaultMode) {
	f.fired.Store(false)
	f.committed.Store(false)
	f.mode.Store(int32(m))
}

// Fired reports whether a COMMIT was lost since Arm.
func (f *CommitFault) Fired() bool { return f.fired.Load() }

// Committed reports whether the server answered the lost COMMIT with CommandComplete.
func (f *CommitFault) Committed() bool { return f.committed.Load() }

// commitMessage is the simple-protocol Query message pgx sends for Tx.Commit.
var commitMessage = []byte("Q\x00\x00\x00\x0bcommit\x00")

type faultConn struct {
	net.Conn
	f    *CommitFault
	lost atomic.Bool // the next answer is lost
}

func (c *faultConn) Write(b []byte) (int, error) {
	if bytes.Equal(b, commitMessage) {
		switch FaultMode(c.f.mode.Swap(0)) {
		case LoseAnswer:
			c.f.fired.Store(true)
			c.lost.Store(true) // before the write: pgx may already be reading
			return c.Conn.Write(b)
		case LoseCommit:
			c.f.fired.Store(true)
			_ = c.Close()
			return len(b), nil
		}
	}
	return c.Conn.Write(b)
}

func (c *faultConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 && c.lost.Load() {
		c.f.committed.Store(b[0] == 'C')
		_ = c.Close()
		return 0, io.ErrUnexpectedEOF
	}
	return n, err
}

// FaultyAppPool returns a pool for the application role whose connections lose the COMMIT f is
// armed for. It skips database.Open's role checks; AppPool has run them on the same DSN.
func FaultyAppPool(t testing.TB, f *CommitFault) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn(t, AppDSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	dial := cfg.ConnConfig.DialFunc
	cfg.ConnConfig.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &faultConn{Conn: conn, f: f}, nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
