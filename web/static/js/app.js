// WAM frontend bootstrap (htmx v4).
//
// - Verifies htmx loaded; falls back to unpkg if jsDelivr failed.
// - Sets sane htmx defaults (history cache, timeout).
// - Logs htmx errors to console in development.
(function () {
  function ensureHtmx(src, done) {
    if (window.htmx) return done();
    var s = document.createElement("script");
    s.src = src;
    s.onload = done;
    s.onerror = function () {
      console.error("[wam] failed to load htmx fallback:", src);
    };
    document.head.appendChild(s);
  }

  function toast(msg, kind) {
    var box = document.getElementById("toasts");
    if (!box) return;
    var el = document.createElement("div");
    el.className = "toast";
    el.textContent = msg;
    if (kind === "error") el.style.borderColor = "hsl(var(--destructive))";
    box.appendChild(el);
    setTimeout(function () { el.remove(); }, 4000);
  }

  function boot() {
    if (!window.htmx) return;
    // htmx v4 namespaced events; detail carries {ctx} with the fetch Response.
    document.body.addEventListener("htmx:response:error", function (e) {
      var st = e.detail && e.detail.ctx && e.detail.ctx.response && e.detail.ctx.response.status;
      console.error("[wam] htmx response error", st);
      toast("Request failed (" + st + ")", "error");
    });
    document.body.addEventListener("htmx:error", function (e) {
      console.error("[wam] htmx error", e.detail && e.detail.error);
      toast("Network error", "error");
    });
    document.body.addEventListener("htmx:after:request", function (e) {
      var t = e.detail && e.detail.ctx && e.detail.ctx.response &&
        e.detail.ctx.response.headers.get("hx-trigger");
      if (t) {
        try {
          var evt = JSON.parse(t);
          if (evt.toast) toast(evt.toast);
        } catch (_) { /* plain string trigger */ }
      }
    });
  }

  if (window.htmx) {
    boot();
  } else {
    // Fallback CDN if primary (jsDelivr, injected in base.html) failed.
    document.addEventListener("DOMContentLoaded", function () {
      ensureHtmx("https://unpkg.com/htmx.org@4/dist/htmx.min.js", boot);
    });
  }
})();
