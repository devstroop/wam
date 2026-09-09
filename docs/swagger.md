# API docs (Swagger UI + OpenAPI)

> Served only in development (`WAM_ENV=development`/`dev`/empty).
> In production the routes below are unregistered (404) and the UI links
> are hidden.

- Canonical spec: `api/openapi.yaml` (OpenAPI 3.1.0).
- Served live:
  - `GET /api-docs/openapi.yaml` (canonical)
  - `GET /openapi.yaml` (alias)
- UI: `GET /api-docs/` renders Swagger UI from CDN (`swagger-ui-dist@5.17.14`)
  loading `/api-docs/openapi.yaml`.
- Legacy: `GET /swagger`, `/swagger/`, `/swagger/openapi.yaml` → `301` to `/api-docs/`.
  (Same convention as notalk's `/api-docs`.)
- Validation (optional): `make openapi-lint` uses `vacuum` or `redocly` if installed.
