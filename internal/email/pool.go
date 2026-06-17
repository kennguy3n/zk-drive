package email

import (
	"net"
	"net/smtp"
	"sync"
	"time"
)

// Pooling defaults, applied when the matching SMTPConfig field is
// left at its zero value. They are tuned for the transactional
// relay case: a small warm set (so a burst of invites reuses a
// handshake+AUTH instead of paying TLS per message), an idle
// window comfortably below the 30-60s most relays use to reap idle
// sockets, and a hard lifetime cap so a long-lived connection is
// periodically refreshed (relay restarts, credential rotation, NAT
// rebinding).
const (
	defaultPoolMaxIdle         = 2
	defaultPoolMaxConnIdleTime = 15 * time.Second
	defaultPoolMaxConnLifetime = 5 * time.Minute
)

// quitTimeout bounds a graceful QUIT during teardown so a wedged
// relay cannot block process shutdown.
const quitTimeout = 2 * time.Second

// pooledConn is a warmed SMTP connection: an *smtp.Client already
// past EHLO / STARTTLS / AUTH (so it is one MAIL FROM away from
// sending), the underlying net.Conn (so the per-send wall-clock
// deadline can be re-stamped before each reuse), and the timestamps
// the pool uses to evict stale or over-age connections.
type pooledConn struct {
	cli       *smtp.Client
	conn      net.Conn
	createdAt time.Time
	lastUsed  time.Time
}

// close tears the connection down. It bounds the graceful QUIT with
// a short deadline so a relay that has stopped responding cannot
// wedge the caller (notably pool teardown at shutdown), then closes
// the raw socket if QUIT did not.
func (pc *pooledConn) close() {
	_ = pc.conn.SetDeadline(time.Now().Add(quitTimeout))
	if err := pc.cli.Quit(); err != nil {
		_ = pc.conn.Close()
	}
}

// connPool is a bounded free-list of warmed SMTP connections.
//
// It bounds the number of IDLE connections kept open between sends
// (maxIdle), not the number of concurrent in-flight sends: the
// transactional-email volume is low enough that capping concurrency
// would add head-of-line blocking for no measurable benefit, and a
// send that cannot find a warm connection simply dials a fresh one.
// Connections are returned to the pool only after a fully successful
// send; a connection that errored mid-transaction is in an unknown
// protocol state and is closed rather than reused.
type connPool struct {
	maxIdle     int
	maxIdleTime time.Duration
	maxLifetime time.Duration

	mu     sync.Mutex
	idle   []*pooledConn // LIFO — the most-recently-used conn is reused first so it stays warm
	closed bool
}

// newConnPool builds a pool from the (already defaulted) sizing
// knobs. A non-positive maxIdle disables pooling: get always misses
// and put always closes, so every send dials a fresh connection and
// tears it down afterwards (the pre-pooling behaviour, available as
// an explicit opt-out).
func newConnPool(maxIdle int, maxIdleTime, maxLifetime time.Duration) *connPool {
	return &connPool{
		maxIdle:     maxIdle,
		maxIdleTime: maxIdleTime,
		maxLifetime: maxLifetime,
	}
}

// get pops the most-recently-used warm connection, discarding (and
// closing) any that have been idle past maxIdleTime or alive past
// maxLifetime. It returns nil when the pool holds no reusable
// connection. The returned connection has NOT been liveness-probed
// — the caller re-stamps the socket deadline and issues RSET to both
// reset the protocol state and detect a relay that dropped the
// socket while it was idle.
func (p *connPool) get() *pooledConn {
	now := time.Now()
	var (
		live    *pooledConn
		expired []*pooledConn
	)
	p.mu.Lock()
	for len(p.idle) > 0 {
		last := len(p.idle) - 1
		pc := p.idle[last]
		p.idle[last] = nil
		p.idle = p.idle[:last]
		if p.closed || now.Sub(pc.lastUsed) > p.maxIdleTime || now.Sub(pc.createdAt) > p.maxLifetime {
			expired = append(expired, pc)
			continue
		}
		live = pc
		break
	}
	p.mu.Unlock()
	// Tear evicted connections down after releasing the lock: close
	// issues a blocking QUIT (bounded by quitTimeout), and holding the
	// mutex across it would stall concurrent get/put for up to
	// maxIdle*quitTimeout — the same reason put and close close after
	// unlocking.
	for _, pc := range expired {
		pc.close()
	}
	return live
}

// put returns a connection to the pool after a successful send. The
// connection is closed instead of pooled when the pool is full, the
// pool is closed, or the connection has exceeded its lifetime — in
// which case a fresh handshake on the next send is cheaper than
// reusing a connection a relay is about to reap anyway.
func (p *connPool) put(pc *pooledConn) {
	now := time.Now()
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.maxIdle || now.Sub(pc.createdAt) > p.maxLifetime {
		p.mu.Unlock()
		pc.close()
		return
	}
	pc.lastUsed = now
	p.idle = append(p.idle, pc)
	p.mu.Unlock()
}

// close drains and tears down every idle connection and marks the
// pool closed so any later put closes rather than pools. In-flight
// sends are unaffected: their connection was already removed from
// the idle set by get, so they complete and then close on put.
func (p *connPool) close() {
	p.mu.Lock()
	conns := p.idle
	p.idle = nil
	p.closed = true
	p.mu.Unlock()
	for _, pc := range conns {
		pc.close()
	}
}
