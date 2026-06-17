package email

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingDialer hands each dial its own in-process fakeSMTPServer
// (over a net.Pipe) and counts how many times it was invoked, so a
// test can assert whether the pool reused a warm connection (one
// dial across several sends) or opened a fresh one each time. The
// first dial can optionally be configured to hang up after its
// first DATA, exercising the dead-connection probe-and-replace path.
type recordingDialer struct {
	t                   *testing.T
	mu                  sync.Mutex
	dials               int
	servers             []*fakeSMTPServer
	closeFirstAfterData bool
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	clientSide, serverSide := net.Pipe()
	d.mu.Lock()
	d.dials++
	first := d.dials == 1
	srv := &fakeSMTPServer{
		t:                   d.t,
		conn:                serverSide,
		br:                  bufio.NewReader(serverSide),
		bw:                  bufio.NewWriter(serverSide),
		closeAfterFirstData: first && d.closeFirstAfterData,
	}
	d.servers = append(d.servers, srv)
	d.mu.Unlock()
	go srv.run()
	return clientSide, nil
}

func (d *recordingDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func newPooledTestClient(t *testing.T, cfg SMTPConfig, d *recordingDialer) *SMTPClient {
	t.Helper()
	if cfg.Host == "" {
		cfg.Host = "fake.local"
	}
	if cfg.Port == 0 {
		cfg.Port = 2525
	}
	if cfg.FromAddress == "" {
		cfg.FromAddress = "noreply@drive.example.com"
	}
	if cfg.TLSMode == "" {
		cfg.TLSMode = TLSModeNone
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	c, err := NewSMTPClient(cfg)
	if err != nil {
		t.Fatalf("NewSMTPClient: %v", err)
	}
	c.dialer = d.dial
	return c
}

func sendOne(t *testing.T, c *SMTPClient, to string) {
	t.Helper()
	if err := c.Send(context.Background(), Message{
		To:       to,
		Subject:  "hi",
		TextBody: "body",
	}); err != nil {
		t.Fatalf("Send(%s): %v", to, err)
	}
}

// TestSMTPClient_PoolReusesConnectionAcrossSends is the headline
// guarantee: with pooling enabled (the default), a burst of sends
// reuses one warm connection rather than dialing+handshaking per
// message.
func TestSMTPClient_PoolReusesConnectionAcrossSends(t *testing.T) {
	d := &recordingDialer{t: t}
	c := newPooledTestClient(t, SMTPConfig{}, d)
	defer func() { _ = c.Close() }()

	for i := 0; i < 4; i++ {
		sendOne(t, c, fmt.Sprintf("user%d@example.com", i))
	}

	if got := d.dialCount(); got != 1 {
		t.Fatalf("dial count = %d, want 1 (pool should reuse a single warm connection across sends)", got)
	}
	// The single long-lived server saw the most recent transaction.
	if last := d.servers[0]; !strings.Contains(last.rcptTo, "user3@example.com") {
		t.Errorf("reused server RCPT TO = %q, want the last recipient", last.rcptTo)
	}
}

// TestSMTPClient_PoolDisabledDialsEverySend verifies the explicit
// opt-out: a negative PoolMaxIdle restores the pre-pooling behavior
// where every send dials a fresh connection and closes it after.
func TestSMTPClient_PoolDisabledDialsEverySend(t *testing.T) {
	d := &recordingDialer{t: t}
	c := newPooledTestClient(t, SMTPConfig{PoolMaxIdle: -1}, d)
	defer func() { _ = c.Close() }()

	for i := 0; i < 3; i++ {
		sendOne(t, c, fmt.Sprintf("user%d@example.com", i))
	}

	if got := d.dialCount(); got != 3 {
		t.Fatalf("dial count = %d, want 3 (pooling disabled should dial per send)", got)
	}
}

// TestSMTPClient_PoolReplacesDeadConnection proves a pooled
// connection that the relay silently dropped is detected by the RSET
// probe and transparently replaced with a fresh dial — the send
// still succeeds.
func TestSMTPClient_PoolReplacesDeadConnection(t *testing.T) {
	d := &recordingDialer{t: t, closeFirstAfterData: true}
	c := newPooledTestClient(t, SMTPConfig{}, d)
	defer func() { _ = c.Close() }()

	// First send warms + pools a connection; the server then hangs
	// up, so the pooled connection is now half-open.
	sendOne(t, c, "first@example.com")
	// Second send pops the dead connection, fails the RSET probe,
	// discards it, and dials fresh — succeeding transparently.
	sendOne(t, c, "second@example.com")

	if got := d.dialCount(); got != 2 {
		t.Fatalf("dial count = %d, want 2 (dead pooled conn should be replaced by a fresh dial)", got)
	}
}

// TestSMTPClient_PoolEvictsIdleConnection verifies a connection idle
// past PoolMaxConnIdleTime is evicted rather than reused, so the
// client never hands a transaction to a socket the relay has likely
// already reaped.
func TestSMTPClient_PoolEvictsIdleConnection(t *testing.T) {
	d := &recordingDialer{t: t}
	c := newPooledTestClient(t, SMTPConfig{PoolMaxConnIdleTime: 30 * time.Millisecond}, d)
	defer func() { _ = c.Close() }()

	sendOne(t, c, "first@example.com")
	time.Sleep(80 * time.Millisecond) // exceed the idle window
	sendOne(t, c, "second@example.com")

	if got := d.dialCount(); got != 2 {
		t.Fatalf("dial count = %d, want 2 (idle-evicted conn should force a fresh dial)", got)
	}
}

// TestSMTPClient_CloseDrainsPool verifies Close tears down warm
// connections and that the client still sends afterwards (dialing
// fresh, never pooling on a closed pool).
func TestSMTPClient_CloseDrainsPool(t *testing.T) {
	d := &recordingDialer{t: t}
	c := newPooledTestClient(t, SMTPConfig{}, d)

	sendOne(t, c, "first@example.com")
	if n := idleLen(c.pool); n != 1 {
		t.Fatalf("pool idle len after send = %d, want 1", n)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := idleLen(c.pool); n != 0 {
		t.Fatalf("pool idle len after Close = %d, want 0 (pool should be drained)", n)
	}

	// A send after Close still works — it dials fresh and the
	// closed pool refuses to retain the connection.
	sendOne(t, c, "second@example.com")
	if n := idleLen(c.pool); n != 0 {
		t.Fatalf("pool idle len after post-Close send = %d, want 0 (closed pool must not retain)", n)
	}
	if got := d.dialCount(); got != 2 {
		t.Fatalf("dial count = %d, want 2 (post-Close send should dial fresh)", got)
	}
}

// idleLen returns the number of warm connections the pool currently
// holds. In-package test helper for asserting pool occupancy.
func idleLen(p *connPool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

// newBlockingQuitConn builds a warmed pooledConn (past EHLO, like a
// real pooled connection) whose close() — the SMTP QUIT — blocks
// until the returned release func is called. It lets a test pin a
// connection mid-teardown and observe whether get() keeps the pool
// mutex held while closing evicted connections. release is also
// registered as a t.Cleanup so the server goroutine never leaks.
func newBlockingQuitConn(t *testing.T) (*pooledConn, <-chan struct{}, func()) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	started := make(chan struct{})
	rel := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(rel) }) }
	t.Cleanup(release)

	srv := &fakeSMTPServer{
		t:           t,
		conn:        serverSide,
		br:          bufio.NewReader(serverSide),
		bw:          bufio.NewWriter(serverSide),
		quitStarted: started,
		quitRelease: rel,
	}
	go srv.run()

	cli, err := smtp.NewClient(clientSide, "fake.local")
	if err != nil {
		t.Fatalf("smtp.NewClient: %v", err)
	}
	if err := cli.Hello("fake.local"); err != nil {
		t.Fatalf("EHLO: %v", err)
	}
	now := time.Now()
	return &pooledConn{cli: cli, conn: clientSide, createdAt: now, lastUsed: now}, started, release
}

// TestConnPool_GetEvictsExpiredWithoutHoldingLock pins the fix for
// the get()-under-lock concurrency bug: evicting an expired idle
// connection must close it AFTER releasing the pool mutex, because
// close performs a blocking QUIT (bounded by quitTimeout). If get
// held the lock across close, a burst of expired connections would
// stall every concurrent get/put for up to maxIdle*quitTimeout.
func TestConnPool_GetEvictsExpiredWithoutHoldingLock(t *testing.T) {
	pc, quitStarted, release := newBlockingQuitConn(t)
	// Force eviction on the next get: idle far past any sane window.
	stale := time.Now().Add(-time.Hour)
	pc.createdAt = stale
	pc.lastUsed = stale

	p := newConnPool(2, 15*time.Second, 5*time.Minute)
	p.idle = append(p.idle, pc)

	got := make(chan *pooledConn, 1)
	go func() { got <- p.get() }()

	// get() pops the stale conn, releases the lock, then blocks in
	// close() (the QUIT awaits release). Wait until close is in
	// flight before probing the lock.
	select {
	case <-quitStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("evicted connection's QUIT never started")
	}

	// With the lock released, idleLen (which takes p.mu) must return
	// promptly even though close() is still blocked. Under the old
	// code, get held p.mu across close and this would block.
	lockFree := make(chan int, 1)
	go func() { lockFree <- idleLen(p) }()
	select {
	case n := <-lockFree:
		if n != 0 {
			t.Fatalf("idle len = %d, want 0 (the stale conn was popped)", n)
		}
	case <-time.After(time.Second):
		t.Fatal("idleLen blocked >1s: get() held the pool mutex while closing an evicted connection")
	}

	release() // let the blocking QUIT finish

	select {
	case c := <-got:
		if c != nil {
			t.Fatalf("get returned a connection, want nil (the only idle conn was stale)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("get did not return after the evicted connection closed")
	}
}
