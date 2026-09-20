package handlers

import (
	"crypto/rand"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/spignelon/netra/internal/auth"
	"github.com/spignelon/netra/internal/db"
	"github.com/spignelon/netra/internal/models"
)

const slugAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// randomSlug returns a URL-safe random slug of length n.
func randomSlug(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	out := make([]byte, n)
	for i, c := range b {
		out[i] = slugAlphabet[int(c)%len(slugAlphabet)]
	}
	return string(out)
}

// ---- First-run setup ----

// Setup handles GET/POST /setup: claims the single admin account once.
func (h *Handler) Setup(w http.ResponseWriter, r *http.Request) {
	exists, err := h.DB.AdminExists()
	if err != nil {
		internalError(w, "setup: check admin exists", err)
		return
	}
	if exists {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		h.render(w, "setup.html", map[string]any{"CSRF": h.Auth.IssueCSRF(w)})
		return
	}
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")
	confirm := r.FormValue("confirm")
	if username == "" || len(pw) < 8 || pw != confirm {
		h.render(w, "setup.html", map[string]any{
			"Error": "Username required and passwords must match (min 8 characters).",
			"CSRF":  h.Auth.IssueCSRF(w),
		})
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		internalError(w, "setup: hash password", err)
		return
	}
	if err := h.DB.CreateAdmin(username, hash); err != nil {
		internalError(w, "setup: create admin", err)
		return
	}
	if err := h.Auth.Login(w); err != nil {
		internalError(w, "setup: create session", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// ---- Login / logout ----

// Login handles GET/POST /login.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	exists, err := h.DB.AdminExists()
	if err != nil {
		internalError(w, "login: check admin exists", err)
		return
	}
	if !exists {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if h.Auth.IsAuthed(r) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	// Basic Auth conceal style has no login-page markup at all — /login
	// itself must not become a way to reach real (or Nextcloud-disguised)
	// branding pre-auth in that mode, so route it through the exact same
	// challenge RequireAuth uses instead of rendering login.html.
	if h.Concealed() && h.ConcealStyle() == "basic_auth" {
		h.Auth.RequireAuth(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
		})(w, r)
		return
	}
	if r.Method == http.MethodGet {
		h.render(w, "login.html", map[string]any{})
		return
	}

	ip := h.clientIP(r)
	if ok, retryAfter := h.Auth.AllowLogin(ip); !ok {
		h.render(w, "login.html", map[string]any{
			"Error": fmt.Sprintf("Too many failed attempts. Try again in %d seconds.", int(retryAfter.Seconds())+1),
		})
		return
	}

	admin, err := h.DB.GetAdmin()
	if err != nil {
		internalError(w, "login: get admin", err)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	pw := r.FormValue("password")
	if username != admin.Username || !auth.CheckPassword(admin.PasswordHash, pw) {
		h.Auth.RecordLoginFailure(ip)
		h.render(w, "login.html", map[string]any{"Error": "Invalid credentials."})
		return
	}
	h.Auth.RecordLoginSuccess(ip)
	if err := h.Auth.Login(w); err != nil {
		internalError(w, "login: create session", err)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// Logout handles POST /logout.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	h.Auth.Logout(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ---- Settings ----

// SettingsPage handles GET /admin/settings.
func (h *Handler) SettingsPage(w http.ResponseWriter, r *http.Request) {
	webhook, err := h.DB.GetWebhookSettings()
	if err != nil {
		internalError(w, "settings: load webhook settings", err)
		return
	}
	h.render(w, "settings.html", map[string]any{
		"Nav":          "settings",
		"CSRF":         auth.CSRFToken(r),
		"Webhook":      webhook,
		"GeoIPEnabled": h.GeoIPEnabled(),
		"ConcealStyle": h.ConcealStyle(),
		"Notice":       r.URL.Query().Get("notice"),
		"Error":        r.URL.Query().Get("err"),
	})
}

// SaveWebhook handles POST /admin/settings/webhook: persists the webhook
// backend (ntfy/gotify) configuration and the GPS-capture alert settings.
func (h *Handler) SaveWebhook(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	ws := db.WebhookSettings{
		Type:             r.FormValue("webhook_type"),
		URL:              strings.TrimRight(strings.TrimSpace(r.FormValue("webhook_url")), "/"),
		Topic:            strings.TrimSpace(r.FormValue("webhook_topic")),
		Token:            strings.TrimSpace(r.FormValue("webhook_token")),
		Priority:         r.FormValue("webhook_priority"),
		OnHit:            r.FormValue("webhook_on_hit") == "1",
		AuthToken:        strings.TrimSpace(r.FormValue("webhook_auth_token")),
		AuthUser:         strings.TrimSpace(r.FormValue("webhook_auth_user")),
		AuthPass:         r.FormValue("webhook_auth_pass"),
		GPSAlertEnabled:  r.FormValue("gps_alert_enabled") == "1",
		GPSAlertPriority: r.FormValue("gps_alert_priority"),
	}
	switch ws.Type {
	case "none", "ntfy", "gotify":
	default:
		ws.Type = "none"
	}
	if err := h.DB.SetWebhookSettings(ws); err != nil {
		internalError(w, "settings: save webhook", err)
		return
	}
	if err := h.DB.SetGPSAlertSettings(ws.GPSAlertEnabled, ws.GPSAlertPriority); err != nil {
		internalError(w, "settings: save gps alert", err)
		return
	}
	h.reloadNotifyConfig()
	http.Redirect(w, r, "/admin/settings?notice=Webhook+settings+saved", http.StatusSeeOther)
}

// TestWebhook handles POST /admin/settings/webhook/test: sends one test
// notification using the currently *saved* settings (save first, then test).
func (h *Handler) TestWebhook(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	if err := h.NotifyConfig().Test(); err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEsc(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/settings?notice=Test+notification+sent", http.StatusSeeOther)
}

// ToggleGeoIP handles POST /admin/settings/geoip: flips the IP-geolocation
// (ip-api.com) enrichment toggle. Disabling it stops all outbound lookups —
// events are still logged, just without country/city/ISP/ASN fields.
func (h *Handler) ToggleGeoIP(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	newState := !h.GeoIPEnabled()
	if err := h.DB.SetGeoIPEnabled(newState); err != nil {
		internalError(w, "settings: set geoip enabled", err)
		return
	}
	h.SetGeoIPEnabled(newState)
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

// ToggleConceal handles POST /admin/settings/conceal: flips conceal mode,
// which disguises the login page, dashboard title, and favicon as a generic
// self-hosted Nextcloud instance so a casual visitor or port scanner can't
// tell Netra is running here. It only changes cosmetics on the admin-facing
// surface — the login form still only ever authenticates the one real admin
// account, and nothing extra is captured or stored about what anyone else
// types into it.
func (h *Handler) ToggleConceal(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	newState := !h.Concealed()
	if err := h.DB.SetConcealEnabled(newState); err != nil {
		internalError(w, "settings: set conceal mode", err)
		return
	}
	h.SetConcealed(newState)
	h.Auth.SetConcealed(newState)
	http.Redirect(w, r, "/admin/settings", http.StatusSeeOther)
}

// SaveConcealStyle handles POST /admin/settings/conceal-style: which
// disguise conceal mode uses at the login gate — the Nextcloud replica
// login page, or a native HTTP Basic Auth prompt with no page markup at
// all. Only meaningful while conceal mode itself is on, but saved
// independently so the choice is remembered across toggling conceal mode
// off and back on.
func (h *Handler) SaveConcealStyle(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	if err := h.DB.SetConcealStyle(r.FormValue("conceal_style")); err != nil {
		internalError(w, "settings: save conceal style", err)
		return
	}
	saved, err := h.DB.ConcealStyle()
	if err != nil {
		internalError(w, "settings: reload conceal style", err)
		return
	}
	h.SetConcealStyle(saved)
	h.Auth.SetBasicAuthStyle(saved == "basic_auth")
	http.Redirect(w, r, "/admin/settings?notice=Disguise+style+saved", http.StatusSeeOther)
}

// SaveAutoRefresh handles POST /admin/settings/refresh: persists how often
// (in seconds) the dashboard/events/links pages should re-poll their JSON
// APIs for new data. Clamped server-side to a sane range regardless of what
// the form sends (see db.MinAutoRefreshSeconds/MaxAutoRefreshSeconds).
func (h *Handler) SaveAutoRefresh(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	secs, err := strconv.Atoi(strings.TrimSpace(r.FormValue("auto_refresh_seconds")))
	if err != nil {
		secs = db.DefaultAutoRefreshSeconds
	}
	if err := h.DB.SetAutoRefreshSeconds(secs); err != nil {
		internalError(w, "settings: save auto-refresh interval", err)
		return
	}
	// Re-read so the in-memory cache reflects the actual clamped value, not
	// whatever out-of-range number the form happened to submit.
	saved, err := h.DB.AutoRefreshSeconds()
	if err != nil {
		internalError(w, "settings: reload auto-refresh interval", err)
		return
	}
	h.SetAutoRefreshSeconds(saved)
	http.Redirect(w, r, "/admin/settings?notice=Auto-refresh+interval+saved", http.StatusSeeOther)
}

// SaveTheme handles POST /admin/settings/theme.
func (h *Handler) SaveTheme(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	if err := h.DB.SetThemePreference(r.FormValue("theme")); err != nil {
		internalError(w, "settings: save theme preference", err)
		return
	}
	// Re-read so the in-memory cache reflects the validated/defaulted value,
	// not whatever arbitrary string the form happened to submit.
	saved, err := h.DB.ThemePreference()
	if err != nil {
		internalError(w, "settings: reload theme preference", err)
		return
	}
	h.SetTheme(saved)
	http.Redirect(w, r, "/admin/settings?notice=Appearance+saved", http.StatusSeeOther)
}

// ---- Dashboard ----

// Dashboard handles GET /admin.
func (h *Handler) Dashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := h.DB.GetStats(0)
	if err != nil {
		internalError(w, "dashboard: get stats", err)
		return
	}
	recent, err := h.DB.ListEvents(0, 15)
	if err != nil {
		internalError(w, "dashboard: list events", err)
		return
	}
	h.render(w, "dashboard.html", map[string]any{
		"Nav":    "dashboard",
		"CSRF":   auth.CSRFToken(r),
		"Stats":  stats,
		"Recent": recent,
	})
}

// ---- Links CRUD ----

// LinksList handles GET /admin/links.
func (h *Handler) LinksList(w http.ResponseWriter, r *http.Request) {
	links, err := h.DB.ListLinks()
	if err != nil {
		internalError(w, "links list: query", err)
		return
	}
	for _, l := range links {
		l.Expired = h.linkExpired(l)
	}
	h.render(w, "links.html", map[string]any{
		"Nav":     "links",
		"CSRF":    auth.CSRFToken(r),
		"Links":   links,
		"BaseURL": h.Cfg.BaseURL,
		"Error":   r.URL.Query().Get("err"),
	})
}

// linkLiveRow is the per-link payload LinksAPI returns: just the fields that
// can go stale between page loads (event count, last-activity time, active/
// expired state), not a full link — the Links page updates these in place
// rather than re-rendering whole rows, which would lose in-progress
// checkbox selections.
type linkLiveRow struct {
	ID         int64  `json:"id"`
	EventCount int    `json:"event_count"`
	LastEvent  string `json:"last_event,omitempty"`
	Active     bool   `json:"active"`
	Expired    bool   `json:"expired"`
}

// LinksAPI handles GET /admin/api/links, used by the Links page's
// auto-refresh to keep event counts/last-activity/status current without a
// full page reload.
func (h *Handler) LinksAPI(w http.ResponseWriter, r *http.Request) {
	links, err := h.DB.ListLinks()
	if err != nil {
		internalError(w, "links api: query", err)
		return
	}
	out := make([]linkLiveRow, 0, len(links))
	for _, l := range links {
		row := linkLiveRow{ID: l.ID, EventCount: l.EventCount, Active: l.Active, Expired: h.linkExpired(l)}
		if l.LastEvent != nil {
			row.LastEvent = l.LastEvent.Format(time.RFC3339)
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// CreateLink handles POST /admin/links.
func (h *Handler) CreateLink(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		// Fall back to plain form parse for non-multipart submissions.
		_ = r.ParseForm()
	}

	typ := r.FormValue("type")
	switch typ {
	case models.TypeRedirect, models.TypePixel, models.TypeGPS, models.TypeClone:
	default:
		http.Redirect(w, r, "/admin/links?err=Invalid+link+type", http.StatusSeeOther)
		return
	}

	slug := strings.TrimSpace(r.FormValue("slug"))
	if slug == "" {
		slug = randomSlug(7)
	} else if !validSlug(slug) {
		http.Redirect(w, r, "/admin/links?err=Slug+may+only+contain+letters,+numbers,+-+and+_", http.StatusSeeOther)
		return
	}
	if taken, _ := h.DB.SlugExists(slug); taken {
		http.Redirect(w, r, "/admin/links?err=That+slug+is+already+taken", http.StatusSeeOther)
		return
	}

	cfg := models.LinkConfig{
		Destination: strings.TrimSpace(r.FormValue("destination")),
		Theme:       r.FormValue("theme"),
		Title:       r.FormValue("title"),
		Description: r.FormValue("description"),
		Image:       strings.TrimSpace(r.FormValue("og_image")),
		CloneURL:    strings.TrimSpace(r.FormValue("clone_url")),
		Channel:     strings.TrimSpace(r.FormValue("channel")),
	}

	// Expiry: an optional datetime-local value (browser-local, no timezone)
	// and/or an optional max-click count.
	if raw := strings.TrimSpace(r.FormValue("expires_at")); raw != "" {
		if t, err := time.Parse("2006-01-02T15:04", raw); err == nil {
			cfg.ExpiresAt = t.Format(time.RFC3339)
		} else {
			http.Redirect(w, r, "/admin/links?err=Invalid+expiry+date", http.StatusSeeOther)
			return
		}
	}
	if raw := strings.TrimSpace(r.FormValue("max_clicks")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			cfg.MaxClicks = n
		} else {
			http.Redirect(w, r, "/admin/links?err=Invalid+max+clicks", http.StatusSeeOther)
			return
		}
	}

	// Handle a custom pixel image upload.
	if typ == models.TypePixel {
		if fname, err := h.saveUpload(r); err != nil {
			http.Redirect(w, r, "/admin/links?err="+urlEsc(err.Error()), http.StatusSeeOther)
			return
		} else if fname != "" {
			cfg.ImagePath = fname
		}
	}

	link := &models.Link{
		Slug:   slug,
		Type:   typ,
		Label:  strings.TrimSpace(r.FormValue("label")),
		Config: cfg,
		Active: true,
	}
	id, err := h.DB.CreateLink(link)
	if err != nil {
		http.Redirect(w, r, "/admin/links?err=Could+not+create+link", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/links/%d?created=1", id), http.StatusSeeOther)
}

// saveUpload stores an uploaded "image" form file and returns its stored
// filename, or "" if none was provided.
func (h *Handler) saveUpload(r *http.Request) (string, error) {
	file, header, err := r.FormFile("image")
	if errors.Is(err, http.ErrMissingFile) || header == nil {
		return "", nil
	}
	if err != nil {
		return "", nil
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
	default:
		return "", fmt.Errorf("unsupported image type %q", ext)
	}
	name := randomSlug(12) + ext
	dst := filepath.Join(h.Cfg.UploadsDir(), name)
	out, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("could not save image")
	}
	defer out.Close()
	if _, err := io.Copy(out, io.LimitReader(file, 5<<20)); err != nil { // 5 MB cap
		return "", fmt.Errorf("could not save image")
	}
	return name, nil
}

// LinkDetail handles GET /admin/links/{id}.
func (h *Handler) LinkDetail(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	link, err := h.DB.GetLink(id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		internalError(w, "link detail: get link", err)
		return
	}
	events, err := h.DB.ListEvents(id, 500)
	if err != nil {
		internalError(w, "link detail: list events", err)
		return
	}
	stats, _ := h.DB.GetStats(id)
	link.Expired = h.linkExpired(link)

	// Time-to-first-click: how long after creation the first event landed.
	var timeToFirst string
	if first, ok, err := h.DB.FirstEventAt(id); err == nil && ok {
		timeToFirst = first.Sub(link.CreatedAt).Round(time.Second).String()
	}

	// Build map points for the Leaflet map.
	type pt struct {
		Lat   float64 `json:"lat"`
		Lon   float64 `json:"lon"`
		Label string  `json:"label"`
		GPS   bool    `json:"gps"`
	}
	pts := []pt{}
	for _, e := range events {
		if e.BestLat() == 0 && e.BestLon() == 0 {
			continue
		}
		pts = append(pts, pt{
			Lat:   e.BestLat(),
			Lon:   e.BestLon(),
			Label: fmt.Sprintf("%s — %s (%s)", e.IP, e.City, e.Timestamp.Format("2006-01-02 15:04")),
			GPS:   e.HasGPS(),
		})
	}

	h.render(w, "link_detail.html", map[string]any{
		"Nav":         "links",
		"CSRF":        auth.CSRFToken(r),
		"Link":        link,
		"Events":      events,
		"Stats":       stats,
		"BaseURL":     h.Cfg.BaseURL,
		"ShareURL":    h.shareURL(link),
		"Points":      pts,
		"TimeToFirst": timeToFirst,
		"JustCreated": r.URL.Query().Get("created") == "1",
	})
}

// LinkQR handles GET /admin/links/{id}/qr.png: renders a QR code encoding
// the link's public share URL.
func (h *Handler) LinkQR(w http.ResponseWriter, r *http.Request) {
	id := atoi64(r.PathValue("id"))
	link, err := h.DB.GetLink(id)
	if errors.Is(err, db.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		internalError(w, "link qr: get link", err)
		return
	}
	png, err := qrcode.Encode(h.shareURL(link), qrcode.Medium, 256)
	if err != nil {
		internalError(w, "link qr: encode", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

// ToggleLink handles POST /admin/links/{id}/toggle.
func (h *Handler) ToggleLink(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	id := atoi64(r.PathValue("id"))
	link, err := h.DB.GetLink(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = h.DB.SetLinkActive(id, !link.Active)
	http.Redirect(w, r, "/admin/links", http.StatusSeeOther)
}

// DeleteLink handles POST /admin/links/{id}/delete.
func (h *Handler) DeleteLink(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	id := atoi64(r.PathValue("id"))
	// Best-effort cleanup of a custom pixel image.
	if link, err := h.DB.GetLink(id); err == nil && link.Config.ImagePath != "" {
		_ = os.Remove(filepath.Join(h.Cfg.UploadsDir(), filepath.Base(link.Config.ImagePath)))
	}
	_ = h.DB.DeleteLink(id)
	http.Redirect(w, r, "/admin/links", http.StatusSeeOther)
}

// DeleteLinksBulk handles POST /admin/links/delete: deletes multiple links
// (and their events, via cascade) selected via checkboxes on the links page.
func (h *Handler) DeleteLinksBulk(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	var ids []int64
	for _, s := range r.Form["ids"] {
		if id := atoi64(s); id > 0 {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		// Best-effort cleanup of a custom pixel image, same as single delete.
		if link, err := h.DB.GetLink(id); err == nil && link.Config.ImagePath != "" {
			_ = os.Remove(filepath.Join(h.Cfg.UploadsDir(), filepath.Base(link.Config.ImagePath)))
		}
	}
	_ = h.DB.DeleteLinks(ids)
	http.Redirect(w, r, "/admin/links", http.StatusSeeOther)
}

// ---- Events (global log page + search/pagination API) ----

// EventsPage handles GET /admin/events: a global event log across every
// link, with client-side search/filter and infinite scroll (see events.js).
func (h *Handler) EventsPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, "events.html", map[string]any{
		"Nav":  "events",
		"CSRF": auth.CSRFToken(r),
	})
}

// eventsPageSize is how many rows the infinite-scroll API returns per page.
const eventsPageSize = 50

// EventsAPI handles GET /admin/api/events: paginated, search/type-filtered
// event rows as JSON, used by both the global Events page and the per-link
// detail page's event log (scoped via ?link=id).
func (h *Handler) EventsAPI(w http.ResponseWriter, r *http.Request) {
	linkID := atoi64(r.URL.Query().Get("link"))
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	typ := strings.TrimSpace(r.URL.Query().Get("type"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}

	// Fetch one extra row to know whether another page remains, without a
	// separate COUNT query.
	events, err := h.DB.ListEventsFiltered(linkID, q, typ, offset, eventsPageSize+1)
	if err != nil {
		internalError(w, "events api: query", err)
		return
	}
	hasMore := len(events) > eventsPageSize
	if hasMore {
		events = events[:eventsPageSize]
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"events":   events,
		"has_more": hasMore,
	})
}

// EventsDelete handles POST /admin/api/events/delete: deletes one or more
// event rows by id, used by both the per-row delete button and the
// select-multiple bulk delete on the event log (see events.js).
func (h *Handler) EventsDelete(w http.ResponseWriter, r *http.Request) {
	if !auth.VerifyCSRF(r) {
		http.Error(w, "invalid csrf token", http.StatusForbidden)
		return
	}
	var ids []int64
	for _, s := range r.Form["ids"] {
		if id := atoi64(s); id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		http.Error(w, "no ids given", http.StatusBadRequest)
		return
	}
	if err := h.DB.DeleteEvents(ids); err != nil {
		internalError(w, "events: delete", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"deleted": len(ids)})
}

// ---- Stats API + CSV ----

// StatsAPI handles GET /admin/api/stats(.json), optionally scoped by ?link=id.
func (h *Handler) StatsAPI(w http.ResponseWriter, r *http.Request) {
	linkID := atoi64(r.URL.Query().Get("link"))
	stats, err := h.DB.GetStats(linkID)
	if err != nil {
		internalError(w, "stats api: get stats", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(stats)
}

// EventsCSV handles GET /admin/events.csv, optionally scoped by ?link=id.
func (h *Handler) EventsCSV(w http.ResponseWriter, r *http.Request) {
	linkID := atoi64(r.URL.Query().Get("link"))
	events, err := h.DB.ListEvents(linkID, 0)
	if err != nil {
		internalError(w, "csv export: list events", err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="netra-events-%s.csv"`, time.Now().Format("20060102-150405")))

	cw := csv.NewWriter(w)
	defer cw.Flush()
	_ = cw.Write([]string{
		"timestamp", "link_slug", "link_label", "type", "ip", "country", "region", "city",
		"isp", "org", "asn", "device", "os", "browser", "gps_lat", "gps_lon", "gps_accuracy",
		"accept_language", "user_agent",
	})
	for _, e := range events {
		_ = cw.Write([]string{
			e.Timestamp.Format(time.RFC3339), csvSafe(e.LinkSlug), csvSafe(e.LinkLabel), e.Type,
			csvSafe(e.IP), csvSafe(e.Country), csvSafe(e.Region), csvSafe(e.City), csvSafe(e.ISP),
			csvSafe(e.Org), csvSafe(e.ASN), csvSafe(e.Device), csvSafe(e.OS), csvSafe(e.Browser),
			floatOrEmpty(e.GPSLat), floatOrEmpty(e.GPSLon), floatOrEmpty(e.GPSAccuracy),
			csvSafe(e.AcceptLanguage), csvSafe(e.UserAgent),
		})
	}
}

// csvSafe defuses CSV/formula injection: several of these fields (most
// directly the User-Agent, and — if TRUST_PROXY is on — the client IP) are
// attacker-controlled free text. A value starting with =, +, -, or @ is
// interpreted as a formula by Excel/LibreOffice/Sheets when the export is
// opened, which can leak data or run commands on whoever opens it (this
// admin). Prefixing a leading tab defuses it while leaving the value's text
// unchanged for anyone just reading the cell.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "\t" + s
	}
	return s
}

// shareURL returns the public capture URL for a link.
func (h *Handler) shareURL(l *models.Link) string {
	switch l.Type {
	case models.TypeRedirect:
		return h.Cfg.BaseURL + "/s/" + l.Slug
	case models.TypePixel:
		return h.Cfg.BaseURL + "/i/" + l.Slug + ".png"
	case models.TypeGPS:
		return h.Cfg.BaseURL + "/g/" + l.Slug
	case models.TypeClone:
		return h.Cfg.BaseURL + "/p/" + l.Slug
	}
	return h.Cfg.BaseURL
}

func validSlug(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func floatOrEmpty(f *float64) string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("%.6f", *f)
}

func urlEsc(s string) string {
	return strings.ReplaceAll(s, " ", "+")
}
