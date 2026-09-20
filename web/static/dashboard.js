// Renders the dashboard Chart.js graphs from the `stats` object embedded by
// the server-side template (see dashboard.html), then keeps the stat cards,
// charts, and recent-events table current via periodic polling
// (window.Netra.onAutoRefresh, see autorefresh.js) instead of requiring a
// manual page reload to see a new click/view/GPS capture show up.
(function () {
  if (typeof Chart === "undefined" || typeof stats === "undefined") return;

  const palette = ["#2563eb", "#7c3aed", "#0891b2", "#db2777", "#65a30d", "#ea580c", "#4338ca", "#0d9488"];

  function labels(buckets) { return (buckets || []).map((b) => b.label); }
  function counts(buckets) { return (buckets || []).map((b) => b.count); }

  function esc(s) {
    const d = document.createElement("div");
    d.textContent = s == null ? "" : String(s);
    return d.innerHTML;
  }

  let timeChart, countryChart, deviceChart, browserChart;

  const timeCtx = document.getElementById("chartTime");
  if (timeCtx) {
    timeChart = new Chart(timeCtx, {
      type: "line",
      data: {
        labels: labels(stats.events_by_day),
        datasets: [{
          label: "Events",
          data: counts(stats.events_by_day),
          borderColor: palette[0],
          backgroundColor: "rgba(37,99,235,0.1)",
          tension: 0.3,
          fill: true,
        }],
      },
      options: { plugins: { legend: { display: false } }, scales: { y: { beginAtZero: true } } },
    });
  }

  const countryCtx = document.getElementById("chartCountry");
  if (countryCtx) {
    countryChart = new Chart(countryCtx, {
      type: "bar",
      data: {
        labels: labels(stats.by_country),
        datasets: [{ label: "Events", data: counts(stats.by_country), backgroundColor: palette[1] }],
      },
      options: { plugins: { legend: { display: false } }, scales: { y: { beginAtZero: true } } },
    });
  }

  const deviceCtx = document.getElementById("chartDevice");
  if (deviceCtx) {
    deviceChart = new Chart(deviceCtx, {
      type: "doughnut",
      data: {
        labels: labels(stats.by_device),
        datasets: [{ data: counts(stats.by_device), backgroundColor: palette }],
      },
    });
  }

  const browserCtx = document.getElementById("chartBrowser");
  if (browserCtx) {
    browserChart = new Chart(browserCtx, {
      type: "doughnut",
      data: {
        labels: labels(stats.by_browser),
        datasets: [{ data: counts(stats.by_browser), backgroundColor: palette }],
      },
    });
  }

  // Toggles the "No data yet" overlay (see dashboard.html's chart-empty
  // divs) vs. the chart canvas itself, and only touches the Chart.js
  // instance's data once there's something to show — a chart initialized
  // against a hidden (display:none, zero-size) canvas needs an explicit
  // resize once it becomes visible, or it stays blank.
  function updateChart(chart, canvasId, buckets) {
    const canvas = document.getElementById(canvasId);
    const empty = document.getElementById(canvasId + "Empty");
    const hasData = buckets && buckets.length > 0;
    if (canvas) canvas.hidden = !hasData;
    if (empty) empty.hidden = hasData;
    if (!chart || !hasData) return;
    chart.data.labels = labels(buckets);
    chart.data.datasets[0].data = counts(buckets);
    chart.update();
    chart.resize();
  }

  function refreshStats() {
    fetch("/admin/api/stats")
      .then((r) => r.json())
      .then((s) => {
        const links = document.getElementById("statTotalLinks");
        const events = document.getElementById("statTotalEvents");
        const gps = document.getElementById("statGPSCaptures");
        const ips = document.getElementById("statUniqueIPs");
        if (links) links.textContent = s.total_links;
        if (events) events.textContent = s.total_events;
        if (gps) gps.textContent = s.gps_captures;
        if (ips) ips.textContent = s.unique_ips;
        updateChart(timeChart, "chartTime", s.events_by_day);
        updateChart(countryChart, "chartCountry", s.by_country);
        updateChart(deviceChart, "chartDevice", s.by_device);
        updateChart(browserChart, "chartBrowser", s.by_browser);
      })
      .catch(() => {});
  }

  // Recent events are clickable (same detail popup as the Events page — see
  // event-modal.js), so each rendered row needs the event's full data kept
  // around by ID, not just the summary fields shown in the table cells.
  const recentEventsById = new Map();
  (typeof recentEvents !== "undefined" ? recentEvents : []).forEach((e) => recentEventsById.set(e.ID, e));

  function recentRowHTML(e) {
    const loc = [e.City, e.Country].filter(Boolean).join(", ");
    return (
      "<tr data-id=\"" + e.ID + "\" class=\"events-row-clickable\" title=\"Click to view full event details\">" +
      "<td>" + esc(new Date(e.Timestamp).toLocaleString()) + "</td>" +
      "<td><a href=\"/admin/links/" + e.LinkID + "\">" + esc(e.LinkLabel || e.LinkSlug) + "</a></td>" +
      "<td><span class=\"badge\">" + esc(e.Type) + "</span></td>" +
      "<td>" + esc(e.IP) + "</td>" +
      "<td>" + esc(loc) + "</td>" +
      "<td>" + esc(e.Device) + " / " + esc(e.OS) + " / " + esc(e.Browser) + "</td>" +
      "</tr>"
    );
  }

  function refreshRecent() {
    fetch("/admin/api/events?offset=0")
      .then((r) => r.json())
      .then((data) => {
        const events = (data.events || []).slice(0, 15);
        const wrap = document.getElementById("recentEventsWrap");
        const empty = document.getElementById("recentEventsEmpty");
        const hint = document.getElementById("recentEventsHint");
        const body = document.getElementById("recentEventsBody");
        if (!wrap || !empty || !body) return;
        if (events.length === 0) {
          wrap.hidden = true;
          empty.hidden = false;
          if (hint) hint.hidden = true;
          return;
        }
        wrap.hidden = false;
        empty.hidden = true;
        if (hint) hint.hidden = false;
        recentEventsById.clear();
        events.forEach((e) => recentEventsById.set(e.ID, e));
        body.innerHTML = events.map(recentRowHTML).join("");
      })
      .catch(() => {});
  }

  const recentBody = document.getElementById("recentEventsBody");
  if (recentBody) {
    recentBody.addEventListener("click", (e) => {
      if (e.target.closest("a")) return;
      const row = e.target.closest("tr[data-id]");
      if (!row) return;
      const ev = recentEventsById.get(Number(row.dataset.id));
      if (ev && window.Netra && window.Netra.showEventModal) {
        window.Netra.showEventModal(ev, { showLink: true });
      }
    });
  }

  if (window.Netra && window.Netra.onAutoRefresh) {
    window.Netra.onAutoRefresh(function () {
      refreshStats();
      refreshRecent();
    });
  }
})();
