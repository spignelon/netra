package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/spignelon/netra/internal/models"
)

// sqliteTimeLayouts are the timestamp formats sqlite's CURRENT_TIMESTAMP and
// this driver may hand back as raw text.
var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05",
	time.RFC3339,
	time.RFC3339Nano,
}

// parseSQLiteTime parses a raw sqlite timestamp string, trying the layouts
// sqlite is known to produce.
func parseSQLiteTime(s string) (time.Time, error) {
	var lastErr error
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		} else {
			lastErr = err
		}
	}
	return time.Time{}, lastErr
}

// ErrNotFound is returned when a lookup matches no row.
var ErrNotFound = errors.New("not found")

// ---- Admin ----

// AdminExists reports whether the instance has been claimed.
func (db *DB) AdminExists() (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM admin`).Scan(&n)
	return n > 0, err
}

// CreateAdmin inserts the single owner account.
func (db *DB) CreateAdmin(username, passwordHash string) error {
	_, err := db.Exec(`INSERT INTO admin (username, password_hash) VALUES (?, ?)`, username, passwordHash)
	return err
}

// GetAdmin returns the owner account, or ErrNotFound if unclaimed.
func (db *DB) GetAdmin() (*models.Admin, error) {
	a := &models.Admin{}
	err := db.QueryRow(`SELECT id, username, password_hash, created_at FROM admin ORDER BY id LIMIT 1`).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &a.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ---- Settings ----

// settingConcealEnabled is the key under which conceal-mode's on/off state
// is stored in the settings table.
const settingConcealEnabled = "conceal_enabled"

// Settings keys for GeoIP toggle, webhook notifications, and the separate
// GPS-capture high-priority alert. All stored as plain strings in the
// generic settings table, mirroring settingConcealEnabled above.
const (
	settingGeoIPEnabled     = "geoip_enabled"
	settingWebhookType      = "webhook_type"
	settingWebhookURL       = "webhook_url"
	settingWebhookTopic     = "webhook_topic"
	settingWebhookToken     = "webhook_token"
	settingWebhookPriority  = "webhook_priority"
	settingWebhookOnHit     = "webhook_on_hit"
	settingWebhookAuthToken = "webhook_auth_token"
	settingWebhookAuthUser  = "webhook_auth_user"
	settingWebhookAuthPass  = "webhook_auth_pass"
	settingGPSAlertEnabled  = "gps_alert_enabled"
	settingGPSAlertPriority = "gps_alert_priority"
	settingAutoRefreshSecs  = "auto_refresh_seconds"
	settingThemePreference  = "theme_preference"
	settingConcealStyle     = "conceal_style"
)

// DefaultConcealStyle is used until changed from Settings. "nextcloud" is
// the original disguise (a replica Nextcloud login page); "basic_auth"
// instead challenges with the browser's own native HTTP Basic Auth prompt
// — no login page markup at all, so there's nothing to fingerprint as any
// particular software.
const DefaultConcealStyle = "nextcloud"

// DefaultThemePreference is used until changed from Settings; "auto" follows
// the browser's prefers-color-scheme.
const DefaultThemePreference = "auto"

// Admin-UI auto-refresh interval bounds and default (seconds). Below the
// minimum risks hammering the DB from an idle browser tab; above the
// maximum defeats the point of "auto" refresh.
const (
	DefaultAutoRefreshSeconds = 30
	MinAutoRefreshSeconds     = 5
	MaxAutoRefreshSeconds     = 3600
)

// AutoRefreshSeconds returns how often the admin dashboard/events/links
// pages should re-poll their JSON APIs for new data, defaulting to
// DefaultAutoRefreshSeconds until changed from Settings.
func (db *DB) AutoRefreshSeconds() (int, error) {
	v, ok, err := db.GetSetting(settingAutoRefreshSecs)
	if err != nil || !ok {
		return DefaultAutoRefreshSeconds, err
	}
	n, convErr := strconv.Atoi(v)
	if convErr != nil || n < MinAutoRefreshSeconds || n > MaxAutoRefreshSeconds {
		return DefaultAutoRefreshSeconds, nil
	}
	return n, nil
}

// SetAutoRefreshSeconds persists the auto-refresh interval, clamped to
// [MinAutoRefreshSeconds, MaxAutoRefreshSeconds].
func (db *DB) SetAutoRefreshSeconds(n int) error {
	if n < MinAutoRefreshSeconds {
		n = MinAutoRefreshSeconds
	}
	if n > MaxAutoRefreshSeconds {
		n = MaxAutoRefreshSeconds
	}
	return db.SetSetting(settingAutoRefreshSecs, strconv.Itoa(n))
}

// ThemePreference returns "auto", "light", or "dark" — the admin UI's
// color-scheme preference, defaulting to DefaultThemePreference until
// changed from Settings.
func (db *DB) ThemePreference() (string, error) {
	v, ok, err := db.GetSetting(settingThemePreference)
	if err != nil || !ok {
		return DefaultThemePreference, err
	}
	switch v {
	case "auto", "light", "dark":
		return v, nil
	default:
		return DefaultThemePreference, nil
	}
}

// SetThemePreference persists the theme preference, falling back to
// DefaultThemePreference for anything other than "auto"/"light"/"dark".
func (db *DB) SetThemePreference(v string) error {
	switch v {
	case "auto", "light", "dark":
	default:
		v = DefaultThemePreference
	}
	return db.SetSetting(settingThemePreference, v)
}

// ConcealStyle returns "nextcloud" or "basic_auth" — which disguise conceal
// mode uses at the login gate, defaulting to DefaultConcealStyle until
// changed from Settings. Only meaningful while conceal mode itself is on.
func (db *DB) ConcealStyle() (string, error) {
	v, ok, err := db.GetSetting(settingConcealStyle)
	if err != nil || !ok {
		return DefaultConcealStyle, err
	}
	switch v {
	case "nextcloud", "basic_auth":
		return v, nil
	default:
		return DefaultConcealStyle, nil
	}
}

// SetConcealStyle persists the conceal-mode disguise style, falling back to
// DefaultConcealStyle for anything other than "nextcloud"/"basic_auth".
func (db *DB) SetConcealStyle(v string) error {
	switch v {
	case "nextcloud", "basic_auth":
	default:
		v = DefaultConcealStyle
	}
	return db.SetSetting(settingConcealStyle, v)
}

// GeoIPEnabled reports whether IP geolocation lookups are enabled. Defaults
// to true (enabled) until explicitly turned off from Settings.
func (db *DB) GeoIPEnabled() (bool, error) {
	v, ok, err := db.GetSetting(settingGeoIPEnabled)
	if err != nil {
		return true, err
	}
	if !ok {
		return true, nil
	}
	return v == "1", nil
}

// SetGeoIPEnabled persists the GeoIP toggle.
func (db *DB) SetGeoIPEnabled(enabled bool) error {
	return db.SetSetting(settingGeoIPEnabled, boolStr(enabled))
}

// WebhookSettings is the persisted webhook + GPS-alert configuration.
type WebhookSettings struct {
	Type             string
	URL              string
	Topic            string
	Token            string // gotify application token
	Priority         string
	OnHit            bool
	AuthToken        string // ntfy access token (Authorization: Bearer), takes precedence over AuthUser/AuthPass
	AuthUser         string // ntfy username, for servers requiring Basic auth
	AuthPass         string // ntfy password
	GPSAlertEnabled  bool
	GPSAlertPriority string
}

// GetWebhookSettings loads the current webhook/GPS-alert configuration,
// applying sane defaults for anything never explicitly set.
func (db *DB) GetWebhookSettings() (WebhookSettings, error) {
	var ws WebhookSettings
	var err error
	get := func(key, def string) string {
		if err != nil {
			return def
		}
		var v string
		var ok bool
		v, ok, err = db.GetSetting(key)
		if !ok {
			return def
		}
		return v
	}
	ws.Type = get(settingWebhookType, "none")
	ws.URL = get(settingWebhookURL, "")
	ws.Topic = get(settingWebhookTopic, "")
	ws.Token = get(settingWebhookToken, "")
	ws.Priority = get(settingWebhookPriority, "default")
	ws.OnHit = get(settingWebhookOnHit, "1") == "1"
	ws.AuthToken = get(settingWebhookAuthToken, "")
	ws.AuthUser = get(settingWebhookAuthUser, "")
	ws.AuthPass = get(settingWebhookAuthPass, "")
	ws.GPSAlertEnabled = get(settingGPSAlertEnabled, "1") == "1"
	ws.GPSAlertPriority = get(settingGPSAlertPriority, "urgent")
	return ws, err
}

// SetWebhookSettings persists the webhook configuration (type/url/topic/token/priority/on-hit/auth).
func (db *DB) SetWebhookSettings(ws WebhookSettings) error {
	pairs := map[string]string{
		settingWebhookType:      ws.Type,
		settingWebhookURL:       ws.URL,
		settingWebhookTopic:     ws.Topic,
		settingWebhookToken:     ws.Token,
		settingWebhookPriority:  ws.Priority,
		settingWebhookOnHit:     boolStr(ws.OnHit),
		settingWebhookAuthToken: ws.AuthToken,
		settingWebhookAuthUser:  ws.AuthUser,
		settingWebhookAuthPass:  ws.AuthPass,
	}
	for k, v := range pairs {
		if err := db.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// SetGPSAlertSettings persists the separate GPS-capture high-priority alert configuration.
func (db *DB) SetGPSAlertSettings(enabled bool, priority string) error {
	if err := db.SetSetting(settingGPSAlertEnabled, boolStr(enabled)); err != nil {
		return err
	}
	return db.SetSetting(settingGPSAlertPriority, priority)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// GetSetting returns a raw setting value, or ok=false if it has never been set.
func (db *DB) GetSetting(key string) (value string, ok bool, err error) {
	err = db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetSetting upserts a raw setting value.
func (db *DB) SetSetting(key, value string) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// ConcealEnabled reports whether conceal mode is currently on. Defaults to
// false (disabled) until explicitly toggled on from the admin settings page.
func (db *DB) ConcealEnabled() (bool, error) {
	v, ok, err := db.GetSetting(settingConcealEnabled)
	if err != nil || !ok {
		return false, err
	}
	return v == "1", nil
}

// SetConcealEnabled persists conceal mode's on/off state.
func (db *DB) SetConcealEnabled(enabled bool) error {
	v := "0"
	if enabled {
		v = "1"
	}
	return db.SetSetting(settingConcealEnabled, v)
}

// ---- Sessions ----

// CreateSession stores a login session token.
func (db *DB) CreateSession(token string, ttl time.Duration) error {
	_, err := db.Exec(`INSERT INTO sessions (token, expires_at) VALUES (?, ?)`,
		token, time.Now().Add(ttl))
	return err
}

// SessionValid reports whether token exists and has not expired.
func (db *DB) SessionValid(token string) (bool, error) {
	var expires time.Time
	err := db.QueryRow(`SELECT expires_at FROM sessions WHERE token = ?`, token).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return time.Now().Before(expires), nil
}

// DeleteSession removes a session (logout).
func (db *DB) DeleteSession(token string) error {
	_, err := db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PurgeExpiredSessions removes stale sessions.
func (db *DB) PurgeExpiredSessions() error {
	_, err := db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now())
	return err
}

// ---- Links ----

// CreateLink inserts a new capture surface and returns its id.
func (db *DB) CreateLink(l *models.Link) (int64, error) {
	cfg, err := json.Marshal(l.Config)
	if err != nil {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO links (slug, type, label, config, active) VALUES (?, ?, ?, ?, ?)`,
		l.Slug, l.Type, l.Label, string(cfg), boolToInt(l.Active))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SlugExists reports whether a slug is already taken.
func (db *DB) SlugExists(slug string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM links WHERE slug = ?`, slug).Scan(&n)
	return n > 0, err
}

// GetLinkBySlug returns an active-or-not link by its slug.
func (db *DB) GetLinkBySlug(slug string) (*models.Link, error) {
	return db.scanLink(db.QueryRow(
		`SELECT id, slug, type, label, config, active, created_at FROM links WHERE slug = ?`, slug))
}

// GetLink returns a link by id.
func (db *DB) GetLink(id int64) (*models.Link, error) {
	return db.scanLink(db.QueryRow(
		`SELECT id, slug, type, label, config, active, created_at FROM links WHERE id = ?`, id))
}

func (db *DB) scanLink(row *sql.Row) (*models.Link, error) {
	l := &models.Link{}
	var cfg string
	var active int
	err := row.Scan(&l.ID, &l.Slug, &l.Type, &l.Label, &cfg, &active, &l.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.Active = active == 1
	if err := json.Unmarshal([]byte(cfg), &l.Config); err != nil {
		return nil, err
	}
	return l, nil
}

// ListLinks returns all links, newest first, with event counts.
func (db *DB) ListLinks() ([]*models.Link, error) {
	rows, err := db.Query(`
		SELECT l.id, l.slug, l.type, l.label, l.config, l.active, l.created_at,
		       COUNT(e.id) AS cnt, MAX(e.ts) AS last
		FROM links l
		LEFT JOIN events e ON e.link_id = l.id
		GROUP BY l.id
		ORDER BY l.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.Link
	for rows.Next() {
		l := &models.Link{}
		var cfg string
		var active int
		// MAX(e.ts) is an aggregate expression, and the sqlite driver does not
		// reliably report its declared column type the way it does for a
		// plain column reference — so it can come back as a raw string
		// instead of being auto-converted to time.Time. Scan it as a
		// nullable string and parse it ourselves to avoid a driver-dependent
		// "unsupported Scan" error.
		var last sql.NullString
		if err := rows.Scan(&l.ID, &l.Slug, &l.Type, &l.Label, &cfg, &active,
			&l.CreatedAt, &l.EventCount, &last); err != nil {
			return nil, err
		}
		l.Active = active == 1
		if last.Valid {
			if t, err := parseSQLiteTime(last.String); err == nil {
				l.LastEvent = &t
			}
		}
		if err := json.Unmarshal([]byte(cfg), &l.Config); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetLinkActive toggles a link's active flag.
func (db *DB) SetLinkActive(id int64, active bool) error {
	_, err := db.Exec(`UPDATE links SET active = ? WHERE id = ?`, boolToInt(active), id)
	return err
}

// UpdateLinkConfig overwrites a link's config JSON blob (used to persist
// ExpiredNotified once a one-time expiry webhook has fired).
func (db *DB) UpdateLinkConfig(id int64, cfg models.LinkConfig) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE links SET config = ? WHERE id = ?`, string(b), id)
	return err
}

// CountEvents returns the total number of events logged against a link —
// used to enforce a max-clicks expiry.
func (db *DB) CountEvents(linkID int64) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE link_id = ?`, linkID).Scan(&n)
	return n, err
}

// FirstEventAt returns the timestamp of a link's earliest event, for the
// time-to-first-click metric. ok is false if the link has no events yet.
func (db *DB) FirstEventAt(linkID int64) (t time.Time, ok bool, err error) {
	var first sql.NullString
	err = db.QueryRow(`SELECT MIN(ts) FROM events WHERE link_id = ?`, linkID).Scan(&first)
	if err != nil || !first.Valid {
		return time.Time{}, false, err
	}
	t, err = parseSQLiteTime(first.String)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// DeleteLink removes a link and (via cascade) its events.
func (db *DB) DeleteLink(id int64) error {
	_, err := db.Exec(`DELETE FROM links WHERE id = ?`, id)
	return err
}

// DeleteLinks removes the links with the given ids (and, via ON DELETE
// CASCADE, their events). No-op if ids is empty.
func (db *DB) DeleteLinks(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := db.Exec(`DELETE FROM links WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}

// ---- Events ----

// InsertEvent stores a capture record and returns its id.
func (db *DB) InsertEvent(e *models.Event) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO events (link_id, type, ip, country, region, city, lat, lon, isp, org, asn,
			user_agent, device, os, browser, referer, accept_language, headers_json,
			gps_lat, gps_lon, gps_accuracy)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.LinkID, e.Type, e.IP, e.Country, e.Region, e.City, e.Lat, e.Lon, e.ISP, e.Org, e.ASN,
		e.UserAgent, e.Device, e.OS, e.Browser, e.Referer, e.AcceptLanguage, e.HeadersJSON,
		nullFloat(e.GPSLat), nullFloat(e.GPSLon), nullFloat(e.GPSAccuracy))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// mergeFingerprint reads an event's current headers_json and returns it with
// the given JS-side fingerprint signals (timezone, screen size, language,
// platform) merged in under a "js_fingerprint" key, preserving whatever
// headers were already recorded there (the request headers captured at
// initial view time — X-Forwarded-For, Sec-CH-UA, etc.).
func (db *DB) mergeFingerprint(id, linkID int64, fp map[string]string) (string, error) {
	var hdr string
	if err := db.QueryRow(`SELECT headers_json FROM events WHERE id = ? AND link_id = ?`, id, linkID).Scan(&hdr); err != nil {
		return "", err
	}
	m := map[string]any{}
	_ = json.Unmarshal([]byte(hdr), &m) // best-effort; start fresh on malformed data
	m["js_fingerprint"] = fp
	b, err := json.Marshal(m)
	return string(b), err
}

// AttachGPS updates an existing event with browser geolocation data and
// merges in the JS-side fingerprint (see mergeFingerprint) rather than
// overwriting headers_json outright — an earlier version of this method
// replaced the whole blob with just the fingerprint, silently discarding
// the request headers captured when the event was first created.
// linkID scopes the update to the event actually belonging to that link —
// without it, any visitor could forge an arbitrary event_id in the POST
// body and overwrite GPS coordinates on an event belonging to a completely
// different link.
func (db *DB) AttachGPS(id, linkID int64, lat, lon, accuracy float64, fp map[string]string) error {
	headersJSON, err := db.mergeFingerprint(id, linkID, fp)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE events SET type = ?, gps_lat = ?, gps_lon = ?, gps_accuracy = ?, headers_json = ? WHERE id = ? AND link_id = ?`,
		models.EventGPS, lat, lon, accuracy, headersJSON, id, linkID)
	return err
}

// AttachFingerprint merges JS-side fingerprint signals (from the clone
// live-proxy's beacon) into an existing event's headers_json blob — see
// mergeFingerprint. linkID scopes both the read and the write to the event
// actually belonging to that link — see the identical note on AttachGPS.
func (db *DB) AttachFingerprint(id, linkID int64, fp map[string]string) error {
	headersJSON, err := db.mergeFingerprint(id, linkID, fp)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE events SET headers_json = ? WHERE id = ? AND link_id = ?`, headersJSON, id, linkID)
	return err
}

// DeleteEvents removes the events with the given ids. No-op if ids is empty.
func (db *DB) DeleteEvents(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err := db.Exec(`DELETE FROM events WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	return err
}

// ListEvents returns events, optionally filtered by link, newest first, capped by limit.
func (db *DB) ListEvents(linkID int64, limit int) ([]*models.Event, error) {
	q := `
		SELECT e.id, e.link_id, e.type, e.ts, e.ip, e.country, e.region, e.city, e.lat, e.lon,
		       e.isp, e.org, e.asn, e.user_agent, e.device, e.os, e.browser, e.referer,
		       e.accept_language, e.headers_json, e.gps_lat, e.gps_lon, e.gps_accuracy,
		       l.slug, l.label
		FROM events e JOIN links l ON l.id = e.link_id`
	args := []any{}
	if linkID > 0 {
		q += ` WHERE e.link_id = ?`
		args = append(args, linkID)
	}
	q += ` ORDER BY e.ts DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.Event
	for rows.Next() {
		e := &models.Event{}
		var gpsLat, gpsLon, gpsAcc sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.LinkID, &e.Type, &e.Timestamp, &e.IP, &e.Country, &e.Region,
			&e.City, &e.Lat, &e.Lon, &e.ISP, &e.Org, &e.ASN, &e.UserAgent, &e.Device, &e.OS,
			&e.Browser, &e.Referer, &e.AcceptLanguage, &e.HeadersJSON,
			&gpsLat, &gpsLon, &gpsAcc, &e.LinkSlug, &e.LinkLabel); err != nil {
			return nil, err
		}
		if gpsLat.Valid {
			e.GPSLat = &gpsLat.Float64
		}
		if gpsLon.Valid {
			e.GPSLon = &gpsLon.Float64
		}
		if gpsAcc.Valid {
			e.GPSAccuracy = &gpsAcc.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEventsFiltered returns a search-filtered, paginated slice of events,
// newest first. linkID scopes to one link when > 0; q performs a
// case-insensitive substring match across IP/geo/device fields when
// non-empty; typ filters by exact event type when non-empty. It returns one
// extra row beyond limit (trimmed by the caller) so callers can tell whether
// more pages remain without a separate COUNT query.
func (db *DB) ListEventsFiltered(linkID int64, q, typ string, offset, limit int) ([]*models.Event, error) {
	query := `
		SELECT e.id, e.link_id, e.type, e.ts, e.ip, e.country, e.region, e.city, e.lat, e.lon,
		       e.isp, e.org, e.asn, e.user_agent, e.device, e.os, e.browser, e.referer,
		       e.accept_language, e.headers_json, e.gps_lat, e.gps_lon, e.gps_accuracy,
		       l.slug, l.label
		FROM events e JOIN links l ON l.id = e.link_id
		WHERE 1=1`
	args := []any{}
	if linkID > 0 {
		query += ` AND e.link_id = ?`
		args = append(args, linkID)
	}
	if typ != "" {
		query += ` AND e.type = ?`
		args = append(args, typ)
	}
	if q != "" {
		like := "%" + q + "%"
		query += ` AND (e.ip LIKE ? OR e.country LIKE ? OR e.region LIKE ? OR e.city LIKE ?
			OR e.isp LIKE ? OR e.org LIKE ? OR e.device LIKE ? OR e.os LIKE ? OR e.browser LIKE ?
			OR l.label LIKE ? OR l.slug LIKE ?)`
		for i := 0; i < 11; i++ {
			args = append(args, like)
		}
	}
	query += ` ORDER BY e.ts DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*models.Event
	for rows.Next() {
		e := &models.Event{}
		var gpsLat, gpsLon, gpsAcc sql.NullFloat64
		if err := rows.Scan(&e.ID, &e.LinkID, &e.Type, &e.Timestamp, &e.IP, &e.Country, &e.Region,
			&e.City, &e.Lat, &e.Lon, &e.ISP, &e.Org, &e.ASN, &e.UserAgent, &e.Device, &e.OS,
			&e.Browser, &e.Referer, &e.AcceptLanguage, &e.HeadersJSON,
			&gpsLat, &gpsLon, &gpsAcc, &e.LinkSlug, &e.LinkLabel); err != nil {
			return nil, err
		}
		if gpsLat.Valid {
			e.GPSLat = &gpsLat.Float64
		}
		if gpsLon.Valid {
			e.GPSLon = &gpsLon.Float64
		}
		if gpsAcc.Valid {
			e.GPSAccuracy = &gpsAcc.Float64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- Stats ----

// Stats is an aggregate snapshot for the dashboard.
type Stats struct {
	TotalLinks  int      `json:"total_links"`
	TotalEvents int      `json:"total_events"`
	GPSCaptures int      `json:"gps_captures"`
	UniqueIPs   int      `json:"unique_ips"`
	EventsByDay []Bucket `json:"events_by_day"`
	ByCountry   []Bucket `json:"by_country"`
	ByDevice    []Bucket `json:"by_device"`
	ByBrowser   []Bucket `json:"by_browser"`
}

// Bucket is a label/count pair for charts.
type Bucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// GetStats computes dashboard aggregates. linkID > 0 scopes to one link.
func (db *DB) GetStats(linkID int64) (*Stats, error) {
	s := &Stats{}
	where := ""
	args := []any{}
	if linkID > 0 {
		where = ` WHERE link_id = ?`
		args = append(args, linkID)
	}

	_ = db.QueryRow(`SELECT COUNT(*) FROM links`).Scan(&s.TotalLinks)
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`+where, args...).Scan(&s.TotalEvents); err != nil {
		return nil, err
	}
	gpsWhere := ` WHERE type = 'gps'`
	if linkID > 0 {
		gpsWhere = ` WHERE link_id = ? AND type = 'gps'`
	}
	_ = db.QueryRow(`SELECT COUNT(*) FROM events`+gpsWhere, args...).Scan(&s.GPSCaptures)
	_ = db.QueryRow(`SELECT COUNT(DISTINCT ip) FROM events`+where, args...).Scan(&s.UniqueIPs)

	var err error
	if s.EventsByDay, err = db.buckets(
		`SELECT date(ts) AS l, COUNT(*) FROM events`+where+` GROUP BY l ORDER BY l`, args); err != nil {
		return nil, err
	}
	if s.ByCountry, err = db.buckets(
		`SELECT CASE WHEN country='' THEN 'Unknown' ELSE country END AS l, COUNT(*)
		 FROM events`+where+` GROUP BY l ORDER BY COUNT(*) DESC LIMIT 10`, args); err != nil {
		return nil, err
	}
	if s.ByDevice, err = db.buckets(
		`SELECT CASE WHEN device='' THEN 'Unknown' ELSE device END AS l, COUNT(*)
		 FROM events`+where+` GROUP BY l ORDER BY COUNT(*) DESC`, args); err != nil {
		return nil, err
	}
	if s.ByBrowser, err = db.buckets(
		`SELECT CASE WHEN browser='' THEN 'Unknown' ELSE browser END AS l, COUNT(*)
		 FROM events`+where+` GROUP BY l ORDER BY COUNT(*) DESC LIMIT 10`, args); err != nil {
		return nil, err
	}
	return s, nil
}

func (db *DB) buckets(query string, args []any) ([]Bucket, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Label, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullFloat(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
