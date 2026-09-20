package e2e

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestAutoRefreshSetting exercises the auto-refresh interval end to end:
// defaults to 30s, persists a valid value, is embedded into the page for
// the frontend polling scripts to read, and clamps an out-of-range value
// server-side rather than trusting whatever the form sends.
func TestAutoRefreshSetting(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)

	settings := bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, `value="30"`) {
		t.Fatalf("expected default auto-refresh interval of 30s in settings page, got: %s", settings)
	}

	csrf := a.csrfFromJar(client)
	a.postForm(t, client, "/admin/settings/refresh", url.Values{"auto_refresh_seconds": {"45"}, "csrf_token": {csrf}}).Body.Close()

	settings = bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, `value="45"`) {
		t.Fatalf("expected auto-refresh interval to persist as 45s, got: %s", settings)
	}

	dashboard := bodyString(t, a.get(t, client, "/admin"))
	// html/template defensively pads JS-context interpolations with spaces
	// (e.g. "= 45 ;" rather than "=45;") to avoid ambiguous token merging —
	// match loosely rather than asserting exact spacing.
	if !regexp.MustCompile(`AUTO_REFRESH_SECONDS\s*=\s*45\s*;`).MatchString(dashboard) {
		t.Fatalf("expected dashboard to embed AUTO_REFRESH_SECONDS=45 for the frontend poller, got: %s", dashboard)
	}

	// Out-of-range values are clamped server-side, not trusted as-is.
	a.postForm(t, client, "/admin/settings/refresh", url.Values{"auto_refresh_seconds": {"99999"}, "csrf_token": {csrf}}).Body.Close()
	settings = bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, `value="3600"`) {
		t.Fatalf("expected an out-of-range interval to clamp to the 3600s max, got: %s", settings)
	}
}

// TestConcealModeToggle exercises the conceal-mode setting end to end: it
// starts off, flips on (nav labels + login page change), then off again.
func TestConcealModeToggle(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)

	settings := bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, "Conceal mode is OFF") {
		t.Fatalf("expected conceal mode OFF by default, settings page: %s", settings)
	}

	a.postForm(t, client, "/admin/settings/conceal", nil).Body.Close()

	settings = bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, "Conceal mode is ON") {
		t.Fatal("conceal mode did not turn ON after toggling")
	}

	// The nav should now read "Links", not "Files" (see the earlier session
	// note: conceal mode disguises Dashboard/Activity/Settings but "Files"
	// for Links was confusing, so that one label was fixed to stay literal).
	links := bodyString(t, a.get(t, client, "/admin/links"))
	if !strings.Contains(links, ">Links<") {
		t.Errorf("conceal mode nav should still say 'Links', got: %s", links)
	}

	// Login page should now show Nextcloud branding — checked with a
	// genuinely separate, cookie-less client. Reusing the authed client's
	// jar here would make this request already-authenticated, so /login
	// redirects straight to /admin instead of ever rendering the disguised
	// login page — a real bug that let this assertion pass vacuously
	// before conceal mode's scope was narrowed to the login page only,
	// since the dashboard it silently redirected to used to say
	// "Nextcloud" too.
	anonJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	anon := &http.Client{Jar: anonJar}
	login := bodyString(t, a.get(t, anon, "/login"))
	if !strings.Contains(login, "Nextcloud") {
		t.Errorf("conceal mode ON: /login should mention Nextcloud, got: %s", login)
	}

	// Conceal mode's whole point is protecting a signed-out visitor —
	// once actually logged in, the dashboard must always show real Netra
	// branding regardless of the setting (this was the actual scope bug
	// the setting was narrowed to fix: it used to disguise the admin's
	// own authenticated session too, which nobody who's already logged in
	// needs protecting from).
	dashboard := bodyString(t, a.get(t, client, "/admin"))
	if strings.Contains(dashboard, "Nextcloud") {
		t.Errorf("conceal mode ON: authenticated /admin must never show Nextcloud branding, got: %s", dashboard)
	}
	if !strings.Contains(dashboard, "Dashboard — Netra") {
		t.Errorf("conceal mode ON: authenticated /admin should still show the real Netra title, got: %s", dashboard)
	}

	// Toggle back off.
	a.postForm(t, client, "/admin/settings/conceal", nil).Body.Close()
	settings = bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, "Conceal mode is OFF") {
		t.Fatal("conceal mode did not turn back OFF")
	}
}

// TestGeoIPToggleSkipsLookupFields verifies that with GeoIP disabled,
// events still log (IP/device/timestamp), just without geo fields set —
// per the documented behavior, not a broken capture path.
func TestGeoIPToggleSkipsLookupFields(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)

	a.postForm(t, client, "/admin/settings/geoip", nil).Body.Close() // starts enabled by default per config; this disables it
	settings := bodyString(t, a.get(t, client, "/admin/settings"))
	if !strings.Contains(settings, "GeoIP is OFF") {
		t.Fatalf("expected GeoIP OFF after toggle, settings page: %s", settings)
	}

	a.createLink(t, client, url.Values{"type": {"redirect"}, "slug": {"geooff"}, "destination": {"https://example.com"}}).Body.Close()
	anon := noRedirectClient(&http.Client{Jar: client.Jar})
	a.get(t, anon, "/s/geooff").Body.Close()

	events := a.eventsJSON(t, client, "")
	for _, e := range events {
		if e["Type"] == "click" {
			if country, _ := e["Country"].(string); country != "" {
				t.Errorf("GeoIP disabled but event has Country=%q, want empty", country)
			}
			if ip, _ := e["IP"].(string); ip == "" {
				t.Error("GeoIP disabled: event IP should still be logged, got empty")
			}
			return
		}
	}
	t.Fatal("no click event found for /s/geooff")
}

// TestEventsCSVDefusesFormulaInjection is the regression test for a real
// CSV/formula-injection gap found during a security review: a visitor's
// User-Agent (attacker-controlled, always) was written into the CSV export
// completely unescaped. A value starting with =/+/-/@ is interpreted as a
// formula by Excel/LibreOffice/Sheets when the admin opens the export,
// which can exfiltrate data or run commands on the admin's own machine.
func TestEventsCSVDefusesFormulaInjection(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)
	a.createLink(t, client, url.Values{"type": {"redirect"}, "slug": {"csvtest"}, "destination": {"https://example.com"}}).Body.Close()

	req, err := http.NewRequest(http.MethodGet, a.srv.URL+"/s/csvtest", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("User-Agent", `=cmd|'/c calc'!A1`)
	nr := noRedirectClient(client)
	resp, err := nr.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	csvResp := a.get(t, client, "/admin/events.csv")
	defer csvResp.Body.Close()
	body := bodyString(t, csvResp)

	if strings.Contains(body, "\n=cmd") || strings.Contains(body, ",=cmd") {
		t.Fatalf("CSV export contains an unescaped formula-injection payload: %s", body)
	}
	if !strings.Contains(body, "cmd|'/c calc'!A1") {
		t.Fatalf("CSV export lost the user-agent value entirely (should be defused, not dropped): %s", body)
	}
}

// TestWebhookDeliversToPrivateNetworkTarget guards against a real
// regression caught after the fact: an early pass at the security review
// routed the webhook notifier through the same SSRF-blocking transport as
// the live-proxy clone engine, reasoning it was "just as admin-configured."
// That broke the normal case — self-hosting ntfy/Gotify on a private
// address or as a sibling container on the same docker-compose network,
// which this project's own docs recommend — verified directly against a
// real container before being reverted. This test locks that in: a webhook
// target on loopback (the same address class the clone engine's guard
// would refuse) must still receive the notification.
func TestWebhookDeliversToPrivateNetworkTarget(t *testing.T) {
	received := make(chan string, 1)
	fakeNtfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeNtfy.Close() // a loopback address — exactly what netguard would refuse for the clone engine

	a := newTestApp(t)
	client := a.authedClient(t)

	a.postForm(t, client, "/admin/settings/webhook", url.Values{
		"webhook_type":     {"ntfy"},
		"webhook_url":      {fakeNtfy.URL},
		"webhook_topic":    {"test"},
		"webhook_priority": {"default"},
		"webhook_on_hit":   {"1"},
	}).Body.Close()

	nr := noRedirectClient(client)
	resp := a.postForm(t, nr, "/admin/settings/webhook/test", nil)
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if strings.Contains(loc, "err=") {
		t.Fatalf("test notification to a private-network target failed: %s", loc)
	}

	select {
	case body := <-received:
		if !strings.Contains(body, "you can see this") {
			t.Fatalf("fake ntfy server got unexpected body: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake ntfy server never received the test notification")
	}
}

// TestEventsSearchAndDelete exercises the admin events log's search filter
// and per-event / bulk delete.
func TestEventsSearchAndDelete(t *testing.T) {
	a := newTestApp(t)
	client := a.authedClient(t)

	a.createLink(t, client, url.Values{"type": {"redirect"}, "slug": {"ev1"}, "destination": {"https://example.com"}}).Body.Close()
	a.createLink(t, client, url.Values{"type": {"redirect"}, "slug": {"ev2"}, "destination": {"https://example.com"}}).Body.Close()

	anon := noRedirectClient(&http.Client{Jar: client.Jar})
	a.get(t, anon, "/s/ev1").Body.Close()
	a.get(t, anon, "/s/ev2").Body.Close()

	all := a.eventsJSON(t, client, "")
	if len(all) != 2 {
		t.Fatalf("expected 2 events total, got %d", len(all))
	}

	filtered := a.eventsJSON(t, client, "ev1")
	if len(filtered) != 1 {
		t.Fatalf("search for 'ev1' returned %d events, want 1", len(filtered))
	}

	var id float64
	for _, e := range all {
		if id == 0 {
			id = e["ID"].(float64)
		}
	}
	nr := noRedirectClient(client)
	resp := a.postForm(t, nr, "/admin/api/events/delete", url.Values{"ids": {itoa(int64(id))}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete event: got %d, want 200", resp.StatusCode)
	}

	remaining := a.eventsJSON(t, client, "")
	if len(remaining) != 1 {
		t.Fatalf("after deleting 1 of 2 events, got %d remaining, want 1", len(remaining))
	}
}
