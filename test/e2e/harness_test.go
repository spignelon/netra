// Package e2e drives the whole application through real HTTP requests
// against an httptest.Server — the same route table cmd/netra/main.go
// builds (via internal/app.NewMux), backed by a real (temp-file) sqlite
// database. Nothing here mocks internal packages: auth, CSRF, sessions,
// the DB, and the live-proxy clone engine (against local httptest "target"
// servers, so tests don't depend on the real internet) are all exercised
// as an actual client would hit them.
package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/spignelon/netra/internal/app"
	"github.com/spignelon/netra/internal/auth"
	"github.com/spignelon/netra/internal/config"
	"github.com/spignelon/netra/internal/db"
	"github.com/spignelon/netra/internal/geoip"
	"github.com/spignelon/netra/internal/handlers"
)

// testApp bundles a running instance of the whole app for one test.
type testApp struct {
	srv *httptest.Server
	db  *db.DB
}

// newTestApp builds a fresh Netra instance — temp-dir data directory,
// fresh sqlite database, the exact production route table — and returns it
// wrapped in an httptest.Server. Callers must t.Cleanup or defer Close().
func newTestApp(t *testing.T) *testApp {
	t.Helper()

	dir := t.TempDir()
	cfg := &config.Config{
		Port: "0",
		// Deliberately doesn't contain "netra" — a real deployment's BASE_URL
		// is the admin's own domain, which would never coincidentally spell
		// out the tool's name, and TestPublicPagesNeverMentionNetra checks
		// og:url (built from this value) along with everything else.
		BaseURL:      "http://admin.test",
		DataDir:      dir,
		SessionKey:   randomKey(t),
		TrustProxy:   false,
		CookieSecure: false,
	}
	if err := os.MkdirAll(cfg.UploadsDir(), 0o750); err != nil {
		t.Fatalf("uploads dir: %v", err)
	}

	database, err := db.Open(cfg.DBPath())
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	am := auth.NewManager(database, cfg.CookieSecure, cfg.TrustProxy)
	geo := geoip.New()

	h, err := handlers.New(database, cfg, am, geo)
	if err != nil {
		t.Fatalf("handlers.New: %v", err)
	}

	srv := httptest.NewServer(app.NewMux(h, am))
	t.Cleanup(srv.Close)

	return &testApp{srv: srv, db: database}
}

func randomKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return []byte(hex.EncodeToString(b))
}

// authedClient creates the first (and only) admin account via the real
// /setup flow and returns an http.Client whose cookie jar carries the
// resulting session — exactly what a logged-in browser would have.
func (a *testApp) authedClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}

	// GET /setup first to obtain its CSRF cookie — the endpoint requires it
	// on the POST, just like every other state-changing action.
	getResp, err := client.Get(a.srv.URL + "/setup")
	if err != nil {
		t.Fatalf("setup get: %v", err)
	}
	getResp.Body.Close()

	form := url.Values{
		"username":   {"admin"},
		"password":   {"password12345"},
		"confirm":    {"password12345"},
		"csrf_token": {a.csrfFromJar(client)},
	}
	resp, err := client.PostForm(a.srv.URL+"/setup", form)
	if err != nil {
		t.Fatalf("setup post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("setup: unexpected status %d: %s", resp.StatusCode, body)
	}

	if a.csrfFromJar(client) == "" {
		t.Fatal("setup: no CSRF cookie after login")
	}
	return client
}

// csrfFromJar reads the netra_csrf cookie value the way an admin template
// would (the cookie is deliberately not HttpOnly for exactly this reason).
func (a *testApp) csrfFromJar(client *http.Client) string {
	u, _ := url.Parse(a.srv.URL)
	for _, c := range client.Jar.Cookies(u) {
		if c.Name == "netra_csrf" {
			return c.Value
		}
	}
	return ""
}

// postForm submits an admin form POST with the CSRF token automatically
// included, the way every real admin template does via its hidden field.
func (a *testApp) postForm(t *testing.T, client *http.Client, path string, form url.Values) *http.Response {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", a.csrfFromJar(client))
	resp, err := client.PostForm(a.srv.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func (a *testApp) get(t *testing.T, client *http.Client, path string) *http.Response {
	t.Helper()
	resp, err := client.Get(a.srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// bodyString drains and returns a response body as a string, closing it.
func bodyString(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// noRedirectClient returns an http.Client sharing the same cookie jar as
// client, but that stops at the first redirect instead of following it —
// useful for asserting on a 302/303's Location header directly.
func noRedirectClient(client *http.Client) *http.Client {
	return &http.Client{
		Jar: client.Jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// extractSlug pulls the slug segment back out of a share URL / Location
// header of the form ".../s/{slug}", ".../p/{slug}", etc.
func extractSlug(path string) string {
	parts := strings.Split(strings.TrimRight(path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func parseInt(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
