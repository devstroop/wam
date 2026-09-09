# WAM roadmap

## Data model (SQLite)

- `contacts(id, phone UNIQUE, name, wa_jid, wa_ok, created_at)`
- `groups(id, name UNIQUE, color)` + `contact_groups(contact_id, group_id)` (labels)
- `campaigns(id, name, status, body_template, audience_filter JSON, scheduled_at, created_at)`
- `campaign_recipients(campaign_id, contact_id, status, wa_msg_id, error, sent_at)`
  - status: `queued|sent|delivered|read|failed|replied`
- Analytics = `SELECT status, COUNT(*) ... GROUP BY` (no extra tables).

## WhatsApp (`internal/wa`)

- `wa.Service{client, container}` with shared sqlstore container (same SQLite file), `DeviceProps{Os: WAM}`.
- `GetQR` / `PairPhone` / `SendText(ParseJID + waE2E + token-bucket 30/min)` /
  `IsOnWhatsApp` / `LoggedOut` wipe / receipts → `campaign_recipients`.

## Sender

- One worker: claim `queued` rows, render `{{name}}`, send, update status, respect pause/cancel.
- Scheduler: `scheduled_at <= now` flips `scheduled → sending`.

## Out of scope (v1)

Media library, button/list templates, drip sequences, business-hours throttle,
multi-user — schema leaves room, don't build now.
