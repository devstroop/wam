# htmx v4 notes

- Version hardcoded to `4.0.0` in `internal/server/server.go:htmxVersion` and injected into
  `web/templates/layouts/base.html`:
  `https://cdn.jsdelivr.net/npm/htmx.org@4.0.0/dist/htmx.min.js`
- Fallback in `web/static/js/app.js` loads `https://unpkg.com/htmx.org@4/dist/htmx.min.js`
  if the primary CDN fails.
- Convention:
  - Normal navigation → full page (`Views.RenderPage` with `layouts/base.html` + `pages/*.html`).
  - `HX-Request: true` → fragment only (`Views.RenderPartial` with `partials/*.html`, no layout).
  - Detect via `middleware.IsHTMX(r)`.
- `hx-boost="true"` on `<body>` upgrades links/forms progressively.
- If htmx v4 CDN path differs at release, update `server.go:htmxVersion` + `base.html` + `app.js`.
