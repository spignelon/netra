// Package auth handles the single-admin login: password hashing, session
// cookies, CSRF tokens, and the middleware that guards admin routes.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/spignelon/netra/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie = "netra_session"
	csrfCookie    = "netra_csrf"
	// SessionTTL is how long a login lasts.
	SessionTTL = 7 * 24 * time.Hour
)

// Manager wires auth helpers to the database and cookie settings.
type Manager struct {
	DB           *db.DB
	CookieSecure bool
	// TrustProxy mirrors config.Config.TrustProxy — RequireAuth's Basic Auth
	// path needs the real client IP for login throttling, same caveat as
	// handlers.Handler.clientIP: only honour X-Forwarded-For/X-Real-IP
	// behind an actual trusted reverse proxy, or visitors can spoof it.
	TrustProxy bool

	loginLimiter *loginThrottle

	// concealed and basicAuthStyle cache the conceal-mode settings RequireAuth
	// needs to decide whether an unauthenticated request gets redirected to
	// /login (the normal case, and the Nextcloud-disguise case) or
	// challenged with a native browser HTTP Basic Auth prompt instead. Kept
	// in sync by handlers.Handler's settings-save handlers via SetConcealed/
	// SetBasicAuthStyle — this package can't import handlers (handlers
	// already imports auth), so it keeps its own small cache rather than
	// asking the Handler at request time.
	concealed      atomic.Bool
	basicAuthStyle atomic.Bool
}

// NewManager constructs a Manager with its login throttle initialized.
// Prefer this over a bare struct literal — RequireAuth's Basic Auth path
// needs loginLimiter to be non-nil.
func NewManager(database *db.DB, cookieSecure, trustProxy bool) *Manager {
	return &Manager{DB: database, CookieSecure: cookieSecure, TrustProxy: trustProxy, loginLimiter: newLoginThrottle()}
}

// SetConcealed updates the in-memory conceal-mode cache RequireAuth reads.
// Call this right alongside handlers.Handler.SetConcealed, from the same
// settings-save handler, so the two caches never disagree.
func (m *Manager) SetConcealed(v bool) { m.concealed.Store(v) }

// SetBasicAuthStyle updates the in-memory conceal-style cache RequireAuth
// reads — true means "basic_auth", false means "nextcloud". Call this
// right alongside handlers.Handler.SetConcealStyle.
func (m *Manager) SetBasicAuthStyle(v bool) { m.basicAuthStyle.Store(v) }

// useBasicAuthChallenge reports whether an unauthenticated admin request
// should get a native HTTP Basic Auth prompt instead of a redirect to the
// (Nextcloud-disguised or real) /login page.
func (m *Manager) useBasicAuthChallenge() bool {
	return m.concealed.Load() && m.basicAuthStyle.Load()
}

// clientIP resolves the real client IP for login-throttling purposes, with
// the same TrustProxy caveat as handlers.Handler.clientIP — duplicated here
// (rather than shared) since that helper lives in a package this one can't
// import.
func (m *Manager) clientIP(r *http.Request) string {
	if m.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[0]); ip != "" {
				return ip
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// AllowLogin reports whether ip may attempt a login right now, and if not,
// how much longer it must wait. Shared by the form-based /login handler and
// RequireAuth's Basic Auth challenge path, so switching disguise styles
// can't be used to reset or dodge the lockout.
func (m *Manager) AllowLogin(ip string) (ok bool, retryAfter time.Duration) {
	return m.loginLimiter.allow(ip)
}

// RecordLoginFailure and RecordLoginSuccess update the shared login
// throttle — see AllowLogin.
func (m *Manager) RecordLoginFailure(ip string) { m.loginLimiter.recordFailure(ip) }
func (m *Manager) RecordLoginSuccess(ip string) { m.loginLimiter.recordSuccess(ip) }

// HashPassword returns a bcrypt hash of the given password.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(b), err
}

// CheckPassword verifies a password against a bcrypt hash.
func CheckPassword(hash, pw string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// token returns a URL-safe random token of n bytes.
func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Login creates a session, sets the session + CSRF cookies, and returns nil on success.
func (m *Manager) Login(w http.ResponseWriter) error {
	tok := token(32)
	if err := m.DB.CreateSession(tok, SessionTTL); err != nil {
		return err
	}
	m.setCookie(w, sessionCookie, tok, SessionTTL, true)
	// CSRF token is readable by templates (not HttpOnly) so forms can echo it.
	m.setCookie(w, csrfCookie, token(24), SessionTTL, false)
	return nil
}

// setupCSRFTTL is short since the first-run /setup form is meant to be
// filled in immediately after deployment, not left open indefinitely.
const setupCSRFTTL = 15 * time.Minute

// IssueCSRF sets a CSRF cookie without creating a session, and returns the
// token value to embed in the form directly (VerifyCSRF later reads the
// cookie from the request, but this same response can't see its own
// Set-Cookie yet). Used by the pre-login /setup form, which — unlike every
// other state-changing action — has no existing session to piggyback a
// CSRF token off of.
func (m *Manager) IssueCSRF(w http.ResponseWriter) string {
	tok := token(24)
	m.setCookie(w, csrfCookie, tok, setupCSRFTTL, false)
	return tok
}

// Logout clears the current session server-side and expires the cookies.
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = m.DB.DeleteSession(c.Value)
	}
	m.clearCookie(w, sessionCookie)
	m.clearCookie(w, csrfCookie)
}

// IsAuthed reports whether the request carries a valid session.
func (m *Manager) IsAuthed(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	ok, err := m.DB.SessionValid(c.Value)
	return err == nil && ok
}

// CSRFToken returns the CSRF token from the request cookie, or "".
func CSRFToken(r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil {
		return c.Value
	}
	return ""
}

// basicAuthRealm is deliberately generic — it's the one piece of text a
// visitor who reaches the Basic Auth challenge sees at all (rendered by the
// browser's own native credential prompt, not this app's markup), so it
// must never hint at what software is actually running here.
const basicAuthRealm = "Restricted"

// RequireAuth wraps a handler: authenticated requests pass straight
// through; unauthenticated ones either redirect to /login (the normal
// case, and the Nextcloud-disguise conceal-mode case) or — when conceal
// mode is on with the Basic Auth disguise style — get a native HTTP Basic
// Auth challenge instead, with no login-page markup involved at all.
func (m *Manager) RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.IsAuthed(r) {
			next(w, r)
			return
		}
		if m.useBasicAuthChallenge() {
			m.requireBasicAuth(w, r, next)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

// requireBasicAuth checks credentials against the one real admin account.
// On success it transparently creates a real session (via Login) and
// continues the request, exactly as if a normal /login POST had just
// succeeded — the browser already resent the original request once it had
// credentials, so there's no separate redirect step needed. On failure (or
// while rate-limited) it re-issues the WWW-Authenticate challenge, which
// makes the browser re-prompt the visitor.
func (m *Manager) requireBasicAuth(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	challenge := func() {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+basicAuthRealm+`"`)
		http.Error(w, "", http.StatusUnauthorized)
	}

	ip := m.clientIP(r)
	if ok, _ := m.AllowLogin(ip); !ok {
		http.Error(w, "", http.StatusTooManyRequests)
		return
	}

	username, password, ok := r.BasicAuth()
	if !ok {
		challenge()
		return
	}

	admin, err := m.DB.GetAdmin()
	if err != nil || username != admin.Username || !CheckPassword(admin.PasswordHash, password) {
		m.RecordLoginFailure(ip)
		challenge()
		return
	}
	m.RecordLoginSuccess(ip)
	if err := m.Login(w); err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	next(w, r)
}

// VerifyCSRF constant-time compares the form token against the cookie for unsafe methods.
func VerifyCSRF(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	cookie := CSRFToken(r)
	form := r.FormValue("csrf_token")
	if cookie == "" || form == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie), []byte(form)) == 1
}

func (m *Manager) setCookie(w http.ResponseWriter, name, value string, ttl time.Duration, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Expires:  time.Now().Add(ttl),
		HttpOnly: httpOnly,
		Secure:   m.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (m *Manager) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   m.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}
