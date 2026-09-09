# WAM — WhatsApp Marketing

Self-hosted WhatsApp marketing server: import contacts, segment them into groups,
compose reusable templates, broadcast campaigns with live delivery tracking —
through one paired WhatsApp account.

Built with Go + [whatsmeow](https://github.com/tulir/whatsmeow),
SQLite (app tables share the file with the whatsmeow session store),
server-rendered HTML + [htmx v4](https://htmx.org), and a versioned
[OpenAPI](./api/openapi.yaml) spec served as Swagger UI.

## Quickstart

```sh
go run ./cmd/wam
# open http://localhost:8080
```

Pairing: click **Link a device** in the sidebar, then **Scan QR**
(WhatsApp → Linked devices → Link a device) or **Pair with code**
(enter the 8-character code under Link with phone number).

With Docker:

```sh
cp .env.example .env   # set passwords/secrets
docker compose up --build
```

## Features

- **Contacts** — E.164 contacts, CSV import, live search, filter by group.
- **Groups** — static segments for campaign audiences.
- **Templates** — reusable message blueprints with `{{name}}` personalization.
- **Campaigns** — audience snapshot at creation (union of selected groups +
  individual contacts, empty = all), optional RFC3339 schedule, template picker,
  pause / resume / cancel, per-recipient states, CSV export.
- **Analytics** — delivery funnel (queued → sent → delivered → read → failed),
  14-day timeline, per-campaign rates.
- **Pairing dialog** — QR + phone-code tabs, live connection status in the sidebar.
- **Single-admin auth** — cookie session; disabled when no admin password is set
  (local-dev convenience).
- **API** — JSON under `/api/v1/*`, interactive docs at `/api-docs/`
  (spec: `/api-docs/openapi.yaml`; docs + spec are served only in
  development — 404 in production).

## Configuration

Environment variables (see [`.env.example`](./.env.example)):

| Variable              | Default                   | Description                              |
| --------------------- | ------------------------- | ---------------------------------------- |
| `WAM_ADDR`            | `:8080`                   | TCP address to listen on                 |
| `WAM_ENV`             | `development`             | Shown as badge in the UI                 |
| `WAM_LOG_LEVEL`       | `info`                    | `debug` also logs page/htmx hits         |
| `WAM_PUBLIC_BASE_URL` | `http://localhost:8080`   | Absolute URLs in OpenAPI/Swagger UI      |
| `WAM_DB_PATH`         | `./data/wam.db`           | SQLite file (app + whatsmeow store)      |
| `WAM_DATA_DIR`        | `./data`                  | Created at boot                          |
| `WAM_ADMIN_PASSWORD`  | *(empty = auth disabled)* | Single-admin password                    |
| `WAM_SESSION_SECRET`  | dev default               | Cookie signing secret (32+ bytes in prod)|

## Development

```sh
make run          # go run ./cmd/wam
make build        # -> bin/wam
make vet test fmt # checks (also run in CI)
```

CI (`.github/workflows/ci.yml`) runs `gofmt`, `go vet`, `go build` and
`go test` on pushes/PRs to `main` and `develop`.

## Layout

```
cmd/wam            entrypoint
internal/
  server/          routes + middleware wiring
  handlers/        JSON APIs + HTML/htmx partials
  store/           SQLite persistence (contacts, groups, campaigns, templates)
  campaigns/       background sender worker
  wa/              whatsmeow lifecycle (pair, send, receipts)
  views/           html/template layouts, pages, partials
  auth/ config/ middleware/
web/               templates, css, js (embedded via go:embed)
api/openapi.yaml   canonical API spec
```
