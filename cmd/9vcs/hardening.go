package main

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// connCapListener rejects a new connection outright once maxConns are
// already live, so a burst of connections can't spawn unbounded
// goroutines/TLS handshakes on `9vcs serve` — decision #7's "connection
// cap ... ahead of the TLS handshake". Rejection is silent at this
// layer (just a closed TCP connection, no 9P reply — there's no
// connection yet for one): the caller sees a failed dial, same as if
// nothing were listening.
type connCapListener struct {
	net.Listener
	max int32
	n   int32
}

func newConnCapListener(l net.Listener, max int) *connCapListener {
	return &connCapListener{Listener: l, max: int32(max)}
}

func (l *connCapListener) Accept() (net.Conn, error) {
	for {
		nc, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if atomic.AddInt32(&l.n, 1) > l.max {
			atomic.AddInt32(&l.n, -1)
			nc.Close()
			continue
		}
		return &capTrackedConn{Conn: nc, l: l}, nil
	}
}

// capTrackedConn releases its connCapListener slot on Close, exactly
// once — Server.Serve's per-connection goroutine always closes nc when
// it's done (deferred), and 9P's own tClunk/shutdown paths may also
// reach Close directly.
type capTrackedConn struct {
	net.Conn
	l    *connCapListener
	once sync.Once
}

func (c *capTrackedConn) Close() error {
	c.once.Do(func() { atomic.AddInt32(&c.l.n, -1) })
	return c.Conn.Close()
}

// ipRateLimiter caps how many new connections one remote address may
// open per window — decision #7's "per-IP rate limit", checked ahead of
// the TLS handshake so a single address hammering the connection (TLS's
// handshake does real asymmetric-crypto work, not free) can't burn
// server CPU indefinitely. A fixed window counter, not a sliding one:
// simpler, and precise enough for what this guards against — it can
// admit up to 2x limit right at a window boundary, which is fine for
// blunting a flood, not for exact fairness accounting.
type ipRateLimiter struct {
	limit  int
	window time.Duration

	mu      sync.Mutex
	entries map[string]*rateEntry
	calls   uint64
}

type rateEntry struct {
	windowStart time.Time
	count       int
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{limit: limit, window: window, entries: map[string]*rateEntry{}}
}

// Allow is allowAt(addr, time.Now()) — the real entry point; allowAt
// exists so tests can drive the window deterministically instead of
// racing wall-clock time.
func (r *ipRateLimiter) Allow(addr string) bool {
	return r.allowAt(addr, time.Now())
}

func (r *ipRateLimiter) allowAt(addr string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Stale entries (addresses that haven't reconnected in a while)
	// would otherwise accumulate for as long as the process runs — swept
	// opportunistically rather than on a timer, so there's no background
	// goroutine to shut down.
	r.calls++
	if r.calls%256 == 0 {
		for addr, e := range r.entries {
			if now.Sub(e.windowStart) >= r.window {
				delete(r.entries, addr)
			}
		}
	}

	e, ok := r.entries[addr]
	if !ok || now.Sub(e.windowStart) >= r.window {
		e = &rateEntry{windowStart: now}
		r.entries[addr] = e
	}
	e.count++
	return e.count <= r.limit
}

// rateLimitListener applies an ipRateLimiter to a net.Listener's Accept,
// keyed by the connecting address's host (port excluded — the same
// remote host reconnecting from a different ephemeral port is still the
// same rate-limit subject).
type rateLimitListener struct {
	net.Listener
	limiter *ipRateLimiter
}

func newRateLimitListener(l net.Listener, limiter *ipRateLimiter) *rateLimitListener {
	return &rateLimitListener{Listener: l, limiter: limiter}
}

func (l *rateLimitListener) Accept() (net.Conn, error) {
	for {
		nc, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(nc.RemoteAddr().String())
		if err != nil {
			host = nc.RemoteAddr().String() // e.g. a non-TCP net.Addr in tests
		}
		if !l.limiter.Allow(host) {
			nc.Close()
			continue
		}
		return nc, nil
	}
}
