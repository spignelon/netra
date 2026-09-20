(function () {
  var params = new URL(self.location.href).searchParams;
  var TARGET_ORIGIN = params.get("origin");
  var TARGET_BASE = params.get("base") || "";
  var RELAY_PATH = params.get("relay");
  var SELF_PATH = new URL(self.location.href).pathname;
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
      return;
    }
    if (url.pathname === SELF_PATH || OWN_PATHS.indexOf(url.pathname) !== -1) return;
    if (!TARGET_ORIGIN || !RELAY_PATH) return;

    var real;
    if (url.origin === self.location.origin) {
      var p = url.pathname;
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
            return new Response("", { status: 502 });
          });
      })()
    );
  });
})();
