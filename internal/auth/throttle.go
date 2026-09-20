package handlers

import (
	"sync"
	"time"
)

// loginThrottle rate-limits failed /login attempts per client IP with a
// growing lockout — there was previously no protection at all beyond
// bcrypt's inherent per-attempt cost, leaving the single admin account open
// to unlimited-rate password guessing.
type loginThrottle struct {
	mu    sync.Mutex
	byIP  map[string]*loginAttempts
	limit int // failures before the first lockout kicks in
}

type loginAttempts struct {
	failures  int
	lockUntil time.Time
	lastSeen  time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{byIP: make(map[string]*loginAttempts), limit: 5}
}

// allow reports whether ip may attempt a login right now, and if not, how
// much longer it must wait.
func (t *loginThrottle) allow(ip string) (ok bool, retryAfter time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()

	a, exists := t.byIP[ip]
	if !exists {
		return true, 0
	}
	if now := time.Now(); now.Before(a.lockUntil) {
		return false, a.lockUntil.Sub(now)
	}
	return true, 0
}

// recordFailure counts a bad login attempt from ip, extending its lockout
// with exponential backoff (capped) once past the initial free attempts.
func (t *loginThrottle) recordFailure(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, exists := t.byIP[ip]
	if !exists {
		a = &loginAttempts{}
		t.byIP[ip] = a
	}
	a.failures++
	a.lastSeen = time.Now()
	if a.failures >= t.limit {
		backoff := time.Duration(1<<uint(a.failures-t.limit)) * time.Second
		const maxBackoff = 5 * time.Minute
		if backoff > maxBackoff || backoff <= 0 {
			backoff = maxBackoff
		}
		a.lockUntil = time.Now().Add(backoff)
	}
}

// recordSuccess clears any accumulated failures for ip after a good login.
func (t *loginThrottle) recordSuccess(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.byIP, ip)
}

// pruneLocked drops entries untouched for an hour, so this map can't grow
// without bound from scanning traffic. Caller must hold t.mu.
func (t *loginThrottle) pruneLocked() {
	if len(t.byIP) < 1000 {
		return // not worth the scan yet
	}
	cutoff := time.Now().Add(-time.Hour)
	for ip, a := range t.byIP {
		if a.lastSeen.Before(cutoff) {
			delete(t.byIP, ip)
		}
	}
}
