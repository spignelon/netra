// Service Worker for the live-proxy clone engine (Clone/Preview links and
// GPS-decoy clone pages — see internal/mirror and public.go's *View/*
// handlers). One static script, reused for every cloned link: its own
// registration URL carries the per-link config as query params, read here
// at install time via self.location.search.
//
// Why this exists: the server-side rewriter (mirror.Serve/ServeResource,
// still used as the no-JS/no-SW fallback under ?direct=1) rewrites every
// src/href so resources route back through this server. That's necessary to
// keep the visitor's browser talking only to this origin, but it makes the
// served DOM attributes differ from what a hydrated framework's client
// bundle (React, Next.js, etc.) expects to see, which silently breaks
// hydration. Intercepting at the network layer instead lets the page keep
// the target's original absolute URLs untouched — hydration sees exactly
// what it expects — while every actual request still gets rerouted through
// this server's SSRF-guarded relay endpoint.
(function () {
  var params = new URL(self.location.href).searchParams;
  var TARGET_ORIGIN = params.get("origin"); // e.g. "https://example.com"
  // The target's own directory on its origin (e.g. "/project" for a GitHub
  // Pages project site at https://user.github.io/project/, "" for root) —
  // needed to correctly reconstruct page-relative references (src="./x.js"),
  // which the browser resolves against *our* proxied page's directory, not
  // the target's. Without this, a subpath-hosted target's relative assets
  // resolve to the wrong (root) URL on the real origin and 404.
  var TARGET_BASE = params.get("base") || "";
  var RELAY_PATH = params.get("relay"); // e.g. "/p/abc123/r"
  var SELF_PATH = new URL(self.location.href).pathname; // this sw.js's own path
  // This link's own route prefix, e.g. "/p/abc123" or "/g/abc123" — derived
  // by stripping RELAY_PATH's trailing "/r". Every other route this app
  // itself serves under that prefix (the view page, the fingerprint/GPS
  // beacon endpoints, this script) must never be intercepted-and-relayed —
  // those are real requests meant for THIS server, not the cloned target,
  // and relaying them would ask the target for a path it knows nothing
  // about (breaking the very first navigation, and the tracking beacons).
  var OWN_PREFIX = RELAY_PATH ? RELAY_PATH.replace(/\/r$/, "") : null;
  var OWN_PATHS = OWN_PREFIX
    ? [OWN_PREFIX + "/view", OWN_PREFIX + "/fp", OWN_PREFIX + "/loc", OWN_PREFIX + "/sw.js", RELAY_PATH]
    : [];

  self.addEventListener("install", function () {
    self.skipWaiting();
  });

  self.addEventListener("activate", function (event) {
    event.waitUntil(self.clients.claim());
  });

  self.addEventListener("fetch", function (event) {
    var req = event.request;
    var url;
    try {
      url = new URL(req.url);
    } catch (e) {
      return; // let the browser handle it normally
    }
    // Never intercept requests to this script itself or the relay endpoint
    // — those must reach this server directly, not loop back through us.
    if (url.pathname === SELF_PATH || OWN_PATHS.indexOf(url.pathname) !== -1) return;
    if (!TARGET_ORIGIN || !RELAY_PATH) return;

    // A same-origin *relative* request (e.g. fetch("/api/x")) resolves
    // against this server's origin, but it was written by the target
    // site's own JS to mean *its* backend — reconstruct the real target.
    var real;
    if (url.origin === self.location.origin) {
      var p = url.pathname;
      // A reference with no leading slash (e.g. href="news.css") resolves
      // against the *current directory* of the page it's on — which, for
      // /p/{slug}/view, means the browser bakes our own OWN_PREFIX into the
      // resolved path (producing "/p/slug/news.css"). Strip it back off and
      // put the target's own directory (TARGET_BASE) in its place — a
      // root-relative reference (e.g. href="/about") never had OWN_PREFIX
      // added in the first place, so this branch is a no-op for it.
      if (OWN_PREFIX && p.indexOf(OWN_PREFIX + "/") === 0) {
        p = TARGET_BASE + p.slice(OWN_PREFIX.length);
      }
      real = TARGET_ORIGIN + p + url.search;
    } else {
      real = url.href;
    }

    event.respondWith(
      (function () {
        var init = { method: req.method };
        var headers = {};
        req.headers.forEach(function (v, k) {
          var lk = k.toLowerCase();
          if (lk === "host" || lk === "origin" || lk === "referer" || lk === "cookie") return;
          headers[k] = v;
        });
        init.headers = headers;

        var bodyPromise = Promise.resolve(undefined);
        if (req.method !== "GET" && req.method !== "HEAD") {
          bodyPromise = req.clone().arrayBuffer();
        }
        return bodyPromise
          .then(function (body) {
            init.body = body;
            return fetch(RELAY_PATH + "?u=" + encodeURIComponent(real), init);
          })
          .catch(function () {
            return new Response("mirror relay failed", { status: 502 });
          });
      })()
    );
  });
})();
