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
    initNavGroups(document);
    document.body.addEventListener("htmx:load", function (e) {
      initNavGroups(e.target);
    });
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

  // Expandable nav groups (adapted from notalk-web's grpX pattern, vanilla).
  //
  // Markup contract — works at any depth, so an item can be a parent, a
  // child, or a child carrying its own nested .nav-group:
  //
  //   <div class="nav-group" data-nav-group="access" data-open="true">
  //     <button class="nav-parent …" aria-expanded="true" aria-controls="nav-children-access">…chevron…</button>
  //     <div class="nav-children" id="nav-children-access">…links or nested .nav-group…</div>
  //   </div>
  //
  // Rules: open state = stored pref, else server-rendered data-open; a URL
  // matching any descendant link forces open (deep links/bookmarks). Parent
  // mirrors aria-current when a descendant is current (same highlight as
  // links, since utility classes are shared).
  function initNavGroups(root) {
    var scope = root && root.querySelectorAll ? root : document;
    var groups = scope.querySelectorAll('.nav-group[data-nav-group]:not([data-nav-init])');
    Array.prototype.forEach.call(groups, function (g) {
      g.setAttribute("data-nav-init", "1");
      var name = g.getAttribute("data-nav-group");
      var key = "wam:nav:" + name;
      var btn = g.querySelector(":scope > .nav-parent");
      var box = g.querySelector(":scope > .nav-children");
      if (!btn || !box) return;
      function descendantLinks() {
        return Array.prototype.slice.call(box.querySelectorAll('a[href]'));
      }
      function onGroupPath() {
        var p = window.location.pathname;
        return descendantLinks().some(function (a) {
          var href = a.getAttribute("href") || "";
          var path = href.split("#")[0].split("?")[0];
          return !!path && (p === path || p.indexOf(path + "/") === 0);
        });
      }
      function apply(open) {
        g.setAttribute("data-open", open ? "true" : "false");
        btn.setAttribute("aria-expanded", open ? "true" : "false");
        if (box.querySelector('[aria-current="page"]')) {
          btn.setAttribute("aria-current", "page");
        } else {
          btn.removeAttribute("aria-current");
        }
      }
      var stored = null;
      try {
        stored = window.localStorage.getItem(key);
      } catch (e) { /* private mode: fall through to server state */ }
      var open = stored !== null ? stored === "1" : g.getAttribute("data-open") === "true";
      if (onGroupPath()) open = true;
      apply(open);
      btn.addEventListener("click", function (e) {
        e.stopPropagation();
        open = g.getAttribute("data-open") !== "true";
        apply(open);
        try {
          window.localStorage.setItem(key, open ? "1" : "0");
        } catch (e2) { /* ignore */ }
      });
    });
  }
  initNavGroups(document);
})();
