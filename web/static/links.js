// Select the full value of a readonly "capture URL" field on click, so
// copying it is one click instead of a manual select-drag. Wired via
// addEventListener (not an inline onclick="" attribute) for the same
// reason as the navbar hamburger — see nav.js.
(function () {
  document.querySelectorAll("[data-select-on-click]").forEach((el) => {
    el.addEventListener("click", () => el.select());
  });
})();

// Select-all + count-aware "Delete selected" button for the links table
// (mirrors the same pattern on the event log — see events.js).
(function () {
  const selectAll = document.getElementById("linksSelectAll");
  const deleteBtn = document.getElementById("linksDeleteSelected");
  if (!selectAll || !deleteBtn) return;

  const checkboxes = () => Array.from(document.querySelectorAll(".link-select"));

  function update() {
    const all = checkboxes();
    const checked = all.filter((cb) => cb.checked);
    deleteBtn.disabled = checked.length === 0;
    deleteBtn.textContent = checked.length > 0 ? "Delete selected (" + checked.length + ")" : "Delete selected";
    selectAll.checked = all.length > 0 && checked.length === all.length;
    selectAll.indeterminate = checked.length > 0 && checked.length < all.length;
  }

  selectAll.addEventListener("change", () => {
    checkboxes().forEach((cb) => (cb.checked = selectAll.checked));
    update();
  });
  checkboxes().forEach((cb) => cb.addEventListener("change", update));
  update();
})();

// Shows/hides the type-specific fields on the "create link" form based on the
// selected link type.
(function () {
  const select = document.getElementById("typeSelect");
  const groups = document.querySelectorAll("[data-show-for]");
  if (!select) return;

  function update() {
    const type = select.value;
    groups.forEach((g) => {
      const shown = g.dataset.showFor.split(" ").includes(type);
      g.style.display = shown ? "" : "none";
      g.querySelectorAll("input, select").forEach((el) => (el.disabled = !shown));
    });
  }

  select.addEventListener("change", update);
  update();
})();

// QR code popup: click the QR icon beside a link to show a scannable code
// for its share URL.
(function () {
  const modal = document.getElementById("qrModal");
  if (!modal) return;
  const img = document.getElementById("qrModalImg");
  const title = document.getElementById("qrModalTitle");
  const closeBtn = modal.querySelector(".qr-modal-close");

  document.querySelectorAll(".qr-btn").forEach((btn) => {
    btn.addEventListener("click", () => {
      img.src = btn.dataset.qrUrl;
      title.textContent = "QR code — " + btn.dataset.qrName;
      modal.hidden = false;
    });
  });

  function close() {
    modal.hidden = true;
    img.src = "";
  }
  closeBtn.addEventListener("click", close);
  modal.addEventListener("click", (e) => {
    if (e.target === modal) close();
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape" && !modal.hidden) close();
  });
})();

// Row click navigates to that link's detail page — same destination as
// clicking its label, just extended to the whole row (excluding the
// checkbox, QR button, URL field, and the toggle/delete forms, which each
// already do their own thing) so the user doesn't have to hit the small
// label text specifically. Mirrors events.js's row-click-to-detail pattern.
(function () {
  const tbody = document.getElementById("linksBody");
  if (!tbody) return;

  tbody.addEventListener("click", (e) => {
    if (e.target.closest(".select-cell") || e.target.closest(".qr-btn") ||
        e.target.closest(".row-actions") || e.target.closest(".url-cell") ||
        e.target.closest("a")) {
      return;
    }
    const row = e.target.closest("tr[data-id]");
    if (!row) return;
    window.location.href = "/admin/links/" + row.dataset.id;
  });
})();

// Auto-refresh: keeps each row's event count, last-activity time, and
// active/expired status current without a page reload. Updates cells in
// place rather than re-rendering rows, so in-progress checkbox selections
// (and the toggle/delete forms' own state) survive a refresh tick.
// Only runs on the Links list page (link_detail.html reuses this file for
// its QR modal but has no links table of its own).
(function () {
  if (!document.getElementById("linksSelectAll")) return;
  if (!window.Netra || !window.Netra.onAutoRefresh) return;

  function timeAgo(iso) {
    const mins = Math.floor((Date.now() - new Date(iso).getTime()) / 60000);
    if (mins < 1) return "just now";
    if (mins < 60) return mins + "m ago";
    const hours = Math.floor(mins / 60);
    if (hours < 24) return hours + "h ago";
    return Math.floor(hours / 24) + "d ago";
  }

  window.Netra.onAutoRefresh(function () {
    fetch("/admin/api/links")
      .then((r) => r.json())
      .then((rows) => {
        rows.forEach((row) => {
          const tr = document.querySelector('tr[data-id="' + row.id + '"]');
          if (!tr) return;
          const eventsCell = tr.querySelector(".link-events-cell");
          if (eventsCell) eventsCell.textContent = row.event_count;
          const lastCell = tr.querySelector(".link-lastactivity-cell");
          if (lastCell) {
            lastCell.innerHTML = row.last_event
              ? timeAgo(row.last_event)
              : '<span class="muted">never</span>';
          }
          const statusCell = tr.querySelector(".link-status-cell");
          if (statusCell) {
            statusCell.innerHTML = row.expired
              ? '<span class="pill pill-off">Expired</span>'
              : row.active
              ? '<span class="pill pill-on">Active</span>'
              : '<span class="pill pill-off">Disabled</span>';
          }
        });
      })
      .catch(() => {});
  });
})();
