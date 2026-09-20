package e2e

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/spignelon/netra/internal/netguard"
)

// newTargetServer spins up a local HTTP server standing in for a real
// third-party site, so the live-proxy clone engine tests don't depend on
// the real internet. It serves html at "/" with Content-Type text/html.
func newTargetServer(t *testing.T, html string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(html))
	}))
	return srv
}

// TestCloneViewPreservesOriginalURLs is the regression test for the bug
// this whole rework fixed: the ?direct=1 legacy path is ALLOWED to rewrite
// resource URLs, but the default (service-worker-backed) /view response
// must not — rewriting src/href is exactly what broke hydration on
// React/Next.js sites (see the mirror package doc comment).
func TestCloneViewPreservesOriginalURLs(t *testing.T) {
	target := newTargetServer(t, `<!doctype html><html><head>
		<script src="https://cdn.example.com/app.js"></script>
		<link rel="stylesheet" href="/static/style.css">
	</head><body><h1>Target</h1></body></html>`)
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"raw1"}, "destination": {target.URL}}).Body.Close()

	resp := a.get(t, client, "/p/raw1/view?eid=1")
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if !strings.Contains(body, `src="https://cdn.example.com/app.js"`) {
		t.Errorf("cross-origin script src was rewritten, want it untouched: %s", body)
	}
	if !strings.Contains(body, `href="/static/style.css"`) {
		t.Errorf("root-relative stylesheet href was rewritten, want it untouched: %s", body)
	}
	if strings.Contains(body, "/raw1/r?u=") {
		t.Errorf("found a legacy-style proxy-wrapped URL in the non-direct view response: %s", body)
	}
}

// TestCloneViewInjectsBeacon confirms the fingerprint beacon script is
// still appended before </body> even though nothing else is rewritten.
func TestCloneViewInjectsBeacon(t *testing.T) {
	target := newTargetServer(t, `<!doctype html><html><body><p>hi</p></body></html>`)
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"beacon1"}, "destination": {target.URL}}).Body.Close()

	resp := a.get(t, client, "/p/beacon1/view?eid=42")
	defer resp.Body.Close()
	body := bodyString(t, resp)

	// The beacon builds its target as "/p/" + SLUG + "/fp" at runtime, so
	// check for its pieces rather than the concatenated literal.
	if !strings.Contains(body, `SLUG = "beacon1"`) || !strings.Contains(body, `/fp"`) {
		t.Errorf("fingerprint beacon (posting to /p/<slug>/fp) missing or malformed in view response: %s", body)
	}
	if !strings.Contains(body, "EVENT_ID = 42") {
		t.Errorf("beacon script missing the correct event id: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("injected beacon broke the document structure: %s", body)
	}
}

// TestCloneLegacyViewRewritesURLs is the mirror image of
// TestCloneViewPreservesOriginalURLs: the ?direct=1 fallback path SHOULD
// still rewrite resource URLs through /r-legacy, exactly like the original
// server-side rewriter always did — it's the no-JS/no-ServiceWorker escape
// hatch, so it can't rely on the service worker doing that job instead.
func TestCloneLegacyViewRewritesURLs(t *testing.T) {
	target := newTargetServer(t, `<!doctype html><html><head>
		<link rel="stylesheet" href="/static/style.css">
	</head><body><h1>Target</h1></body></html>`)
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"legacy1"}, "destination": {target.URL}}).Body.Close()

	resp := a.get(t, client, "/p/legacy1/view?eid=1&direct=1")
	defer resp.Body.Close()
	body := bodyString(t, resp)

	if !strings.Contains(body, "/p/legacy1/r-legacy?u=") {
		t.Errorf("?direct=1 view did not rewrite the stylesheet through r-legacy: %s", body)
	}
}

// TestResourceRelayForwardsMethodAndBody confirms the SW-facing /r endpoint
// (mirror.Relay) forwards method and body — needed for the intercepted
// fetch/XHR calls a target site's own JS makes, not just GET asset loads.
func TestResourceRelayForwardsMethodAndBody(t *testing.T) {
	var gotMethod, gotBody string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/echo" {
			gotMethod = r.Method
			buf := make([]byte, 1024)
			n, _ := r.Body.Read(buf)
			gotBody = string(buf[:n])
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>ok</body></html>`))
	}))
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"relay1"}, "destination": {target.URL}}).Body.Close()

	relayURL := "/p/relay1/r?u=" + url.QueryEscape(target.URL+"/api/echo")
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+relayURL, strings.NewReader(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("relay post: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("relay post: got %d, want 200", resp.StatusCode)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("target saw method %q, want POST", gotMethod)
	}
	if !strings.Contains(gotBody, "hello") {
		t.Errorf("target saw body %q, want it to contain the posted JSON", gotBody)
	}
}

// TestResourceRelayAbsolutizesCSSURLs is the regression test for the font-
// 404 bug found while fixing hydration: a service-worker-relayed response's
// reported URL is this server's own relay endpoint, not the real
// stylesheet's URL, so the browser resolves any *relative* url(...) inside
// it against the wrong base. Relay must rewrite those references to
// fully-qualified absolute URLs (not proxy-wrapped — just qualified) so the
// resulting fetch is a normal request the service worker can intercept.
func TestResourceRelayAbsolutizesCSSURLs(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assets/style.css":
			w.Header().Set("Content-Type", "text/css")
			_, _ = w.Write([]byte(`@font-face { src: url(../fonts/x.woff2); }`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"css1"}, "destination": {target.URL + "/"}}).Body.Close()

	relayURL := "/p/css1/r?u=" + url.QueryEscape(target.URL+"/assets/style.css")
	resp := a.get(t, client, relayURL)
	defer resp.Body.Close()
	body := bodyString(t, resp)

	want := target.URL + "/fonts/x.woff2"
	if !strings.Contains(body, want) {
		t.Errorf("relayed CSS = %q, want an absolutized reference to %q", body, want)
	}
	if strings.Contains(body, "../fonts") {
		t.Errorf("relayed CSS still contains the unresolved relative reference: %s", body)
	}
}

// TestMirrorServiceWorkerScript checks the SW script is served with a
// script-executable content type from under the link's own scope (so its
// default registration scope covers /p/{slug}/view without needing a
// Service-Worker-Allowed header).
func TestMirrorServiceWorkerScript(t *testing.T) {
	target := newTargetServer(t, `<html><body>hi</body></html>`)
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"sw1"}, "destination": {target.URL}}).Body.Close()

	resp := a.get(t, client, "/p/sw1/sw.js?origin="+url.QueryEscape(target.URL)+"&relay="+url.QueryEscape("/p/sw1/r"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sw.js: got %d, want 200", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "javascript") {
		t.Fatalf("sw.js content-type = %q, want a javascript type", ct)
	}
	body := bodyString(t, resp)
	if !strings.Contains(body, "addEventListener(\"fetch\"") {
		t.Errorf("sw.js body doesn't look like the mirror service worker: %s", body[:min(200, len(body))])
	}
}

// TestMirrorSSRFGuardBlocksPrivateTargets is the security-critical test:
// an operator-supplied clone destination pointing at loopback/private
// addresses must never actually be fetched. Uses a REAL local server (so a
// non-existent listener like "http://127.0.0.1:1" can't make this pass
// vacuously — there'd be nothing to leak either way) and asserts its
// distinctive body content never comes back through the proxy.
func TestMirrorSSRFGuardBlocksPrivateTargets(t *testing.T) {
	// This test verifies the real guard, so undo TestMain's blanket
	// allowance for the duration of this test only.
	netguard.AllowPrivateForTesting = false
	t.Cleanup(func() { netguard.AllowPrivateForTesting = true })

	privateTarget := newTargetServer(t, `<html><body>TOP-SECRET-INTERNAL-CONTENT</body></html>`)
	defer privateTarget.Close() // listens on 127.0.0.1 — exactly what guardURL must refuse

	a := newTestApp(t)
	client := a.authedClient(t)

	for _, dest := range []string{
		privateTarget.URL,                          // http://127.0.0.1:PORT
		"http://169.254.169.254/latest/meta-data/", // cloud metadata endpoint, unreachable but must still be refused pre-fetch
	} {
		t.Run(dest, func(t *testing.T) {
			slug := "ssrf-" + sanitizeSlug(dest)
			a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {slug}, "destination": {dest}}).Body.Close()

			resp := a.get(t, client, "/p/"+slug+"/view?eid=1&direct=1")
			defer resp.Body.Close()
			body := bodyString(t, resp)

			if strings.Contains(body, "TOP-SECRET-INTERNAL-CONTENT") {
				t.Fatalf("SSRF guard did not block %q — the private target's real content was relayed back", dest)
			}
			// The fallback card (clone.html) deliberately shows no error text
			// to the visitor — see ClonePageView's comment on why a blocked/
			// failed fetch must never look different from an intentionally
			// bare link. "Continue to site" is the one thing it always
			// renders when a destination is set, so it's what confirms the
			// fallback card rendered instead of, say, the app crashing.
			if !strings.Contains(body, "Continue to site") {
				t.Fatalf("expected the blocked-fetch fallback card for %q, got: %s", dest, body)
			}
		})
	}
}

func sanitizeSlug(s string) string {
	r := strings.NewReplacer("http://", "", "https://", "", "/", "-", ":", "-", ".", "-")
	return r.Replace(s)
}

// TestPublicPagesNeverMentionNetra is the regression test for a real leak:
// every page a link's actual visitor (not the admin) can reach — the GPS
// decoy theme, the Clone/Preview live-proxy loader and its own service
// worker script, and the Clone/Preview fallback card — must never contain
// the word "Netra" anywhere in the response body, including in HTML/JS
// comments. A comment that isn't visible on the rendered page is still
// fully visible via view-source, and one slipped in once already (added
// alongside an otherwise-unrelated favicon fix, caught only by manual
// review) — this test exists so that class of mistake fails CI instead of
// waiting to be noticed by whoever the tool is used against.
func TestPublicPagesNeverMentionNetra(t *testing.T) {
	target := newTargetServer(t, `<html><body>real target content</body></html>`)
	defer target.Close()

	a := newTestApp(t)
	client := a.authedClient(t)

	a.createLink(t, client, url.Values{"type": {"gps"}, "slug": {"leak-gps"}, "theme": {"cats"}}).Body.Close()
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"leak-clone"}, "destination": {target.URL}}).Body.Close()
	// A destination that can never be reached forces the clone.html
	// fallback card instead of the live-proxied real page.
	a.createLink(t, client, url.Values{"type": {"clone"}, "slug": {"leak-clone-fail"}, "destination": {"http://127.0.0.1:1/unreachable"}}).Body.Close()

	anon := &http.Client{Jar: nil}
	checks := []struct {
		name string
		url  string
	}{
		{"GPS decoy page", "/g/leak-gps"},
		{"Clone/Preview loader (mirror_loader.html)", "/p/leak-clone"},
		{"Clone/Preview service worker script", "/p/leak-clone/sw.js?origin=" + url.QueryEscape(target.URL) + "&relay=" + url.QueryEscape("/p/leak-clone/r")},
		{"Clone/Preview fallback card (live fetch failed)", "/p/leak-clone-fail/view?eid=1&direct=1"},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			body := bodyString(t, a.get(t, anon, c.url))
			if strings.Contains(strings.ToLower(body), "netra") {
				t.Errorf("%s must never mention Netra (visible via view-source to the actual link visitor), got: %s", c.name, body)
			}
		})
	}
}
