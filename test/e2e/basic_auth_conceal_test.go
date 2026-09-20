package e2e

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
)

// enableBasicAuthConceal turns conceal mode on and sets its disguise style
// to basic_auth, via the real admin Settings forms.
func enableBasicAuthConceal(t *testing.T, a *testApp, client *http.Client) {
	t.Helper()
	a.postForm(t, client, "/admin/settings/conceal", nil).Body.Close()
	a.postForm(t, client, "/admin/settings/conceal-style", map[string][]string{"conceal_style": {"basic_auth"}}).Body.Close()
}

// TestBasicAuthConcealChallenge covers the alternative conceal-mode
// disguise: instead of a Nextcloud-replica login page, an unauthenticated
// admin request gets the browser's own native HTTP Basic Auth prompt, with
// no page markup at all. This is a real credential path (not a cosmetic
// gate), so it's tested against the actual admin account end to end:
// missing/wrong credentials are rejected, the WWW-Authenticate realm never
// hints at what software this is, and correct credentials transparently
// create a real session — the same one a normal /login POST would.
func TestBasicAuthConcealChallenge(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)
	enableBasicAuthConceal(t, a, client)

	anonJar, _ := cookiejar.New(nil)
	anon := noRedirectClient(&http.Client{Jar: anonJar})

	// No credentials at all -> 401 with a generic challenge, no branding.
	resp := a.get(t, anon, "/admin")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no creds: got %d, want 401", resp.StatusCode)
	}
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(wwwAuth, "Basic realm=") {
		t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", wwwAuth)
	}
	if strings.Contains(strings.ToLower(wwwAuth), "netra") || strings.Contains(strings.ToLower(wwwAuth), "nextcloud") {
		t.Errorf("WWW-Authenticate realm must not hint at the software, got: %q", wwwAuth)
	}
	body := bodyString(t, a.get(t, anon, "/admin"))
	if strings.TrimSpace(body) != "" {
		t.Errorf("the 401 response body should be empty (no login-page markup at all), got: %q", body)
	}

	// Wrong credentials -> still 401.
	req, _ := http.NewRequest(http.MethodGet, a.srv.URL+"/admin", nil)
	req.SetBasicAuth("admin", "wrong-password")
	wrongResp, err := anon.Do(req)
	if err != nil {
		t.Fatalf("wrong creds request: %v", err)
	}
	wrongResp.Body.Close()
	if wrongResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong creds: got %d, want 401", wrongResp.StatusCode)
	}

	// Correct credentials -> through to the real page, with a real session
	// now set (same cookie a normal form login would create).
	okReq, _ := http.NewRequest(http.MethodGet, a.srv.URL+"/admin", nil)
	okReq.SetBasicAuth("admin", "password12345")
	okResp, err := anon.Do(okReq)
	if err != nil {
		t.Fatalf("correct creds request: %v", err)
	}
	defer okResp.Body.Close()
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("correct creds: got %d, want 200", okResp.StatusCode)
	}
	u, _ := okReq.URL.Parse("/")
	hasSession := false
	for _, c := range anon.Jar.Cookies(u) {
		if c.Name == "netra_session" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Error("correct Basic Auth credentials should create a real session cookie")
	}

	// A follow-up request using just that session (no Basic Auth header
	// this time) should now work without re-challenging.
	resp2 := a.get(t, anon, "/admin")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("session from Basic Auth login should carry over: got %d, want 200", resp2.StatusCode)
	}
}

// TestBasicAuthConcealLoginRedirectsToChallenge ensures /login itself
// doesn't become a loophole back to page markup while Basic Auth conceal
// is active — visiting it directly must also just trigger the native
// challenge, not render any login page (Nextcloud-styled or real).
func TestBasicAuthConcealLoginRedirectsToChallenge(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)
	enableBasicAuthConceal(t, a, client)

	anonJar, _ := cookiejar.New(nil)
	anon := &http.Client{Jar: anonJar}
	resp := a.get(t, anon, "/login")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /login under basic_auth conceal: got %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("GET /login under basic_auth conceal should still carry the Basic Auth challenge header")
	}
}

// TestNextcloudConcealStillUsesLoginPage is the control case: with conceal
// mode on but the style left at its default (nextcloud), behavior must be
// completely unchanged from before this feature existed — no surprise
// regression for the existing disguise from adding the new one.
func TestNextcloudConcealStillUsesLoginPage(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)
	a.postForm(t, client, "/admin/settings/conceal", nil).Body.Close()

	anonJar, _ := cookiejar.New(nil)
	anon := noRedirectClient(&http.Client{Jar: anonJar})
	resp := a.get(t, anon, "/admin")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("nextcloud-style conceal, unauthenticated /admin: got %d, want 303 redirect to /login", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("redirect target = %q, want /login", loc)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Error("nextcloud-style conceal must never send a WWW-Authenticate challenge")
	}
}
