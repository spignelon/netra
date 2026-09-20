// Shared "click a row to see everything captured for that event" popup,
// used by both events.js (global Events page + a link's own event log) and
// dashboard.js (Recent events). Factored out so the dashboard doesn't have
// to duplicate this ~100-line modal-building logic to get the same popup.
window.Netra = window.Netra || {};

(function () {
  function esc(s) {
    const d = document.createElement("div");
    d.textContent = s == null ? "" : String(s);
    return d.innerHTML;
  }

  function buildEventModal() {
    const modal = document.createElement("div");
    modal.className = "event-modal";
    modal.id = "eventModal";
    modal.hidden = true;
    modal.innerHTML =
      '<div class="event-modal-card">' +
      '<button type="button" class="event-modal-close" aria-label="Close">&times;</button>' +
      '<h3 id="eventModalTitle"></h3>' +
      '<div id="eventModalBody"></div>' +
      "</div>";
    document.body.appendChild(modal);

    const close = () => { modal.hidden = true; };
    modal.querySelector(".event-modal-close").addEventListener("click", close);
    modal.addEventListener("click", (e) => { if (e.target === modal) close(); });
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && !modal.hidden) close();
    });
    return modal;
  }

  function fieldRow(label, value) {
    if (value === null || value === undefined || value === "") return "";
    return "<dt>" + esc(label) + "</dt><dd>" + esc(value) + "</dd>";
  }

  // Header keys captured server-side at event time (see internal/handlers/
  // public.go's capture()) — shown separately from the JS-side fingerprint,
  // which is merged into the same blob under "js_fingerprint" once a GPS or
  // Clone link's beacon reports back (see internal/db/queries.go).
  const RAW_HEADER_LABELS = {
    "X-Forwarded-For": "X-Forwarded-For",
    "X-Real-IP": "X-Real-IP",
    "DNT": "Do Not Track",
    "Sec-CH-UA": "Client Hints (UA)",
    "Sec-CH-UA-Mobile": "Client Hints (Mobile)",
    "Sec-CH-UA-Platform": "Client Hints (Platform)",
  };

  // showEventModal renders the full detail popup for event e. Pass
  // {showLink: true} on pages that list events from more than one link
  // (Dashboard, the global Events page) so the Overview section identifies
  // which link it came from; omit/false on a single link's own detail page,
  // where that's already implied by the page itself.
  function showEventModal(e, opts) {
    const showLink = !!(opts && opts.showLink);
    const modal = window._eventModal || (window._eventModal = buildEventModal());
    modal.querySelector("#eventModalTitle").textContent = "Event #" + e.ID + " — " + e.Type;

    let headers = {};
    try { headers = JSON.parse(e.HeadersJSON || "{}"); } catch (err) { /* malformed/empty */ }
    const fp = headers.js_fingerprint || null;

    const loc = [e.City, e.Region, e.Country].filter(Boolean).join(", ");
    const coords = e.Lat || e.Lon ? e.Lat.toFixed(4) + ", " + e.Lon.toFixed(4) : "";

    let html = "";

    html += '<div class="event-modal-section"><h4>Overview</h4><dl class="event-modal-grid">' +
      fieldRow("Time", new Date(e.Timestamp).toLocaleString()) +
      (showLink ? fieldRow("Link", (e.LinkLabel || e.LinkSlug || ("#" + e.LinkID))) : "") +
      fieldRow("Type", e.Type) +
      "</dl></div>";

    html += '<div class="event-modal-section"><h4>Network &amp; location</h4><dl class="event-modal-grid">' +
      fieldRow("IP address", e.IP) +
      fieldRow("Location", loc) +
      fieldRow("Approx. coordinates", coords) +
      fieldRow("ISP", e.ISP) +
      fieldRow("Org", e.Org) +
      fieldRow("ASN", e.ASN) +
      fieldRow("Referer", e.Referer) +
      "</dl></div>";

    html += '<div class="event-modal-section"><h4>Device</h4><dl class="event-modal-grid">' +
      fieldRow("Device", e.Device) +
      fieldRow("OS", e.OS) +
      fieldRow("Browser", e.Browser) +
      fieldRow("User-Agent", e.UserAgent) +
      fieldRow("Accept-Language", e.AcceptLanguage) +
      "</dl></div>";

    if (e.GPSLat != null && e.GPSLon != null) {
      html += '<div class="event-modal-section"><h4>GPS capture (precise)</h4><dl class="event-modal-grid">' +
        fieldRow("Latitude", e.GPSLat) +
        fieldRow("Longitude", e.GPSLon) +
        fieldRow("Accuracy", e.GPSAccuracy != null ? "±" + Math.round(e.GPSAccuracy) + "m" : "") +
        "</dl></div>";
    }

    if (fp) {
      html += '<div class="event-modal-section"><h4>Browser fingerprint (JS-side)</h4><dl class="event-modal-grid">' +
        fieldRow("Timezone", fp.tz) +
        fieldRow("Screen size", fp.screen) +
        fieldRow("Language", fp.lang) +
        fieldRow("Platform", fp.platform) +
        "</dl></div>";
    }

    const rawRows = Object.keys(RAW_HEADER_LABELS)
      .map((k) => fieldRow(RAW_HEADER_LABELS[k], headers[k]))
      .join("");
    if (rawRows) {
      html += '<div class="event-modal-section"><h4>Raw request headers</h4><dl class="event-modal-grid">' +
        rawRows + "</dl></div>";
    }

    modal.querySelector("#eventModalBody").innerHTML = html;
    modal.hidden = false;
  }

  window.Netra.showEventModal = showEventModal;
})();
