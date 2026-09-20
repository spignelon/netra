// Mobile hamburger menu toggle for the admin navbar (web/templates/layout.html's
// "topnav" define, included on every admin page). Wired via addEventListener
// rather than an inline onclick="" attribute — inline handlers are exactly the
// kind of thing strict browser privacy shields / ad-blockers / CSP policies can
// silently strip, which is a real, needless fragility for something this core
// to using the app on mobile at all.
(function () {
  var toggle = document.querySelector("[data-nav-toggle]");
  var menu = document.getElementById("mobileNav");
  if (!toggle || !menu) return;

  function setOpen(open) {
    menu.hidden = !open;
    toggle.setAttribute("aria-expanded", String(open));
  }

  toggle.addEventListener("click", function () {
    setOpen(menu.hidden);
  });

  // Closing on outside-click/Escape matches the QR/event modal pattern used
  // elsewhere in the admin UI, so the dropdown doesn't feel like an outlier.
  document.addEventListener("click", function (e) {
    if (menu.hidden) return;
    if (menu.contains(e.target) || toggle.contains(e.target)) return;
    setOpen(false);
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && !menu.hidden) setOpen(false);
  });
})();
