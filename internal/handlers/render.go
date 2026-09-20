// Package handlers implements the public capture routes and the admin dashboard.
package handlers

import (
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spignelon/netra/internal/auth"
	"github.com/spignelon/netra/internal/config"
	"github.com/spignelon/netra/internal/db"
	"github.com/spignelon/netra/internal/geoip"
	"github.com/spignelon/netra/internal/models"
	"github.com/spignelon/netra/internal/notify"
	"github.com/spignelon/netra/web"
)

// Handler bundles the dependencies shared across all routes.
type Handler struct {
	DB   *db.DB
	Cfg  *config.Config
	Auth *auth.Manager
	Geo  *geoip.Client
	tmpl *template.Template

	// concealed caches the "conceal mode" setting in memory so every request
	// (including unauthenticated ones like /login and /favicon.ico) can check
	// it without a DB round trip. Kept in sync via SetConcealed on toggle.
	concealed atomic.Bool

	// geoipEnabled caches the GeoIP on/off toggle, so capture() can skip the
	// ip-api.com lookup on every hit without a DB round trip.
	geoipEnabled atomic.Bool

	// autoRefreshSeconds caches the admin-UI polling interval (dashboard,
	// events log, links list), so every render() call can inject it into
	// templates without a DB round trip. Kept in sync via SetAutoRefreshSeconds.
	autoRefreshSeconds atomic.Int32

	// theme caches the "auto"/"light"/"dark" appearance preference so every
	// render() call can set <html data-theme="..."> without a DB round trip.
	// Kept in sync via SetTheme. Holds a string, hence atomic.Value not Bool.
	theme atomic.Value

	// notifyMu guards notifyCfg, refreshed whenever the webhook/GPS-alert
	// settings are saved from the admin Settings page.
	notifyMu  sync.RWMutex
	notifyCfg notify.Config

	// loginLimiter rate-limits failed /login attempts per client IP.
	loginLimiter *loginThrottle
}

// New constructs a Handler and parses all templates.
func New(database *db.DB, cfg *config.Config, am *auth.Manager, geo *geoip.Client) (*Handler, error) {
	funcs := template.FuncMap{
		"fmtTime": func(t time.Time) string { return t.Format("2006-01-02 15:04:05 MST") },
		"fmtDate": func(t time.Time) string { return t.Format("Jan 2, 2006") },
		"since":   func(t time.Time) string { return humanize(time.Since(t)) },
		"typeBadge": func(s string) string {
			switch s {
			case "redirect":
				return "Redirect"
			case "pixel":
				return "Pixel"
			case "gps":
				return "GPS Decoy"
			case "clone":
				return "Clone/Preview"
			}
			return s
		},
		"shareURL": func(base string, l *models.Link) string {
			switch l.Type {
			case models.TypeRedirect:
				return base + "/s/" + l.Slug
			case models.TypePixel:
				return base + "/i/" + l.Slug + ".png"
			case models.TypeGPS:
				return base + "/g/" + l.Slug
			case models.TypeClone:
				return base + "/p/" + l.Slug
			}
			return base
		},
		"toJSON": func(v any) template.JS {
			b, err := json.Marshal(v)
			if err != nil {
				return "null"
			}
			return template.JS(b)
		},
		"linkName": func(l *models.Link) string { return l.DisplayName() },
	}
	t, err := template.New("").Funcs(funcs).ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, err
	}
	h := &Handler{DB: database, Cfg: cfg, Auth: am, Geo: geo, tmpl: t, loginLimiter: newLoginThrottle()}
	if enabled, err := database.ConcealEnabled(); err != nil {
		log.Printf("WARNING: could not load conceal-mode setting, defaulting to off: %v", err)
	} else {
		h.concealed.Store(enabled)
	}
	if enabled, err := database.GeoIPEnabled(); err != nil {
		log.Printf("WARNING: could not load geoip-enabled setting, defaulting to on: %v", err)
		h.geoipEnabled.Store(true)
	} else {
		h.geoipEnabled.Store(enabled)
	}
	if secs, err := database.AutoRefreshSeconds(); err != nil {
		log.Printf("WARNING: could not load auto-refresh setting, defaulting to %ds: %v", db.DefaultAutoRefreshSeconds, err)
		h.autoRefreshSeconds.Store(db.DefaultAutoRefreshSeconds)
	} else {
		h.autoRefreshSeconds.Store(int32(secs))
	}
	if theme, err := database.ThemePreference(); err != nil {
		log.Printf("WARNING: could not load theme preference, defaulting to %q: %v", db.DefaultThemePreference, err)
		h.theme.Store(db.DefaultThemePreference)
	} else {
		h.theme.Store(theme)
	}
	h.reloadNotifyConfig()
	return h, nil
}

// GeoIPEnabled reports whether IP geolocation lookups are currently enabled.
func (h *Handler) GeoIPEnabled() bool { return h.geoipEnabled.Load() }

// SetGeoIPEnabled updates the in-memory GeoIP toggle cache. Call this right
// after persisting the new value with DB.SetGeoIPEnabled.
func (h *Handler) SetGeoIPEnabled(v bool) { h.geoipEnabled.Store(v) }

// AutoRefreshSeconds returns the current admin-UI polling interval.
func (h *Handler) AutoRefreshSeconds() int { return int(h.autoRefreshSeconds.Load()) }

// SetAutoRefreshSeconds updates the in-memory auto-refresh cache. Call this
// right after persisting the new value with DB.SetAutoRefreshSeconds.
func (h *Handler) SetAutoRefreshSeconds(v int) { h.autoRefreshSeconds.Store(int32(v)) }

// Theme returns the current appearance preference ("auto"/"light"/"dark").
func (h *Handler) Theme() string { return h.theme.Load().(string) }

// SetTheme updates the in-memory theme cache. Call this right after
// persisting the new value with DB.SetThemePreference.
func (h *Handler) SetTheme(v string) { h.theme.Store(v) }

// NotifyConfig returns a snapshot of the current webhook/GPS-alert settings.
func (h *Handler) NotifyConfig() notify.Config {
	h.notifyMu.RLock()
	defer h.notifyMu.RUnlock()
	return h.notifyCfg
}

// reloadNotifyConfig re-reads the webhook + GPS-alert settings from the DB
// into the in-memory cache. Call after saving either from Settings.
func (h *Handler) reloadNotifyConfig() {
	ws, err := h.DB.GetWebhookSettings()
	if err != nil {
		log.Printf("WARNING: could not load webhook settings: %v", err)
		return
	}
	cfg := notify.Config{
		Type:             ws.Type,
		URL:              ws.URL,
		Topic:            ws.Topic,
		Token:            ws.Token,
		Priority:         ws.Priority,
		OnHit:            ws.OnHit,
		AuthToken:        ws.AuthToken,
		AuthUser:         ws.AuthUser,
		AuthPass:         ws.AuthPass,
		GPSAlertEnabled:  ws.GPSAlertEnabled,
		GPSAlertPriority: ws.GPSAlertPriority,
	}
	h.notifyMu.Lock()
	h.notifyCfg = cfg
	h.notifyMu.Unlock()
}

// internalError logs the real cause server-side (so it shows up in `docker
// logs`) and returns a generic 500 to the client.
func internalError(w http.ResponseWriter, context string, err error) {
	log.Printf("%s: %v", context, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// concealedTemplates is every page a signed-out visitor can reach — the
// only pages where the conceal-mode setting should ever change what's
// rendered. /setup deliberately isn't here even though it's pre-auth: it
// always shows real branding since an account doesn't exist to log into
// yet, so there's no login-page disguise to apply.
var concealedTemplates = map[string]bool{
	"login.html": true,
}

// Concealed reports whether conceal mode is currently on.
func (h *Handler) Concealed() bool { return h.concealed.Load() }

// SetConcealed updates the in-memory conceal-mode cache. Call this right
// after persisting the new value with DB.SetConcealEnabled.
func (h *Handler) SetConcealed(v bool) { h.concealed.Store(v) }

// render executes a named template with a base layout. Every page gets a
// "Concealed" data key automatically (the true, current setting value —
// used by settings.html's own status pill/toggle, where the admin needs
// to see and control the real state regardless of which page they're on)
// and a "Disguised" key (whether *this specific page's* chrome should
// actually show the Nextcloud disguise) so templates can switch their
// title/favicon/branding without every call site remembering to pass it.
//
// Conceal mode's whole purpose is to protect a non-admin visitor who only
// has the instance URL or a shared/shortened/cloned link from learning
// Netra is what's running here — it was never meant to also disguise the
// authenticated admin's own dashboard, since by definition nobody who's
// already logged in needs protecting from that fact. concealedTemplates
// is the (deliberately short) list of pages a signed-out visitor can
// actually reach — everything else gets Disguised=false unconditionally,
// so a new admin page added later is concealed-safe by default instead of
// needing to remember to opt out.
func (h *Handler) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["Concealed"]; !ok {
		data["Concealed"] = h.Concealed()
	}
	if _, ok := data["Disguised"]; !ok {
		data["Disguised"] = concealedTemplates[name] && h.Concealed()
	}
	if _, ok := data["AutoRefreshSeconds"]; !ok {
		data["AutoRefreshSeconds"] = h.AutoRefreshSeconds()
	}
	if _, ok := data["Theme"]; !ok {
		data["Theme"] = h.Theme()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// clientIP resolves the real client IP, honouring proxy headers only when
// TRUST_PROXY is enabled.
func (h *Handler) clientIP(r *http.Request) string {
	if h.Cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// First entry is the original client.
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

// humanize renders a duration as a compact relative string.
func humanize(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h ago"
	default:
		return itoa(int(d.Hours()/24)) + "d ago"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
