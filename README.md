# WhatsApp Campaign Automation

A reusable WhatsApp campaign automation platform built on Go and Green API.

An administrator creates campaigns, defines the exact phrase that opens the
funnel, builds a timeline of messages relative to an event, and watches the
conversations happen live. The Turkish Ayran / Kaymak webinar is the first
campaign loaded into it, not something the code knows about.

**The platform never messages anyone first.** A contact enters the funnel only
by sending a configured trigger phrase, and that inbound message is the consent
record every outbound path checks.

---

## Contents

- [How it works](#how-it-works)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Green API setup](#green-api-setup)
- [Using the admin panel](#using-the-admin-panel)
- [Architecture](#architecture)
- [Design decisions](#design-decisions)
- [Testing](#testing)
- [Operations](#operations)
- [API reference](#api-reference)

---

## How it works

```
Customer sends the trigger phrase
        │
        ▼
Green API webhook  ──▶  stored raw, deduplicated by event id
        │
        ▼
Background worker: create contact ─▶ record consent ─▶ match trigger
        │
        ▼
Enrol into campaign, write one job per step into SQLite
        │
        ▼
Scheduler claims due jobs (one atomic UPDATE) ─▶ renders ─▶ sends
        │
        ▼
Delivery webhooks update the message; the dashboard sees it over SSE
```

A webinar at 21:00 with the default steps produces exactly:

| Offset | Send time | Type |
|---|---|---|
| on trigger | immediately | voice |
| −5 h | 16:00 | image + text |
| −3 h | 18:00 | voice |
| −2 h | 19:00 | image + text |
| −1 h | 20:00 | image + text |
| −30 m | 20:30 | voice |
| −15 m | 20:45 | image + text + link |
| −7 m 30 s | 20:52:30 | image + text + link |
| start | 21:00 | image + text + link |

Offsets are stored in **seconds**, which is why the 7.5-minute step lands on
20:52:30 rather than being rounded to a whole minute.

---

## Quick start

### With Docker

```bash
cp .env.example .env

# Fill in at minimum:
#   SESSION_SECRET, ADMIN_EMAIL, ADMIN_PASSWORD
# Generate a session secret with:
openssl rand -base64 48

docker compose up -d --build
```

The panel is at <http://localhost:8086>. Migrations run automatically on start.

Load the example campaign:

```bash
docker compose exec app seed -date 2026-09-01 -time 21:00 -link "https://your-webinar-link"
```

### Without Docker

Requires Go 1.25+ and ffmpeg (only for voice messages). There is no database
server to install: the storage engine is SQLite, and the driver is pure Go, so
nothing needs CGO or a system libsqlite3.

```bash
cp .env.example .env      # set SESSION_SECRET, ADMIN_EMAIL, ADMIN_PASSWORD

make run
```

`make run` starts the whole system **in the background**: it applies
migrations, creates the first admin account, and brings up the HTTP server, the
scheduler workers and the webhook processor. The database file is created on
first start at `DATABASE_PATH` (default `./data/whatsapp.db`).

| Target | What it does |
|---|---|
| `make run` | Build, then start detached. Logs to `run/server.log`, pid in `run/server.pid`. Refuses to start twice. |
| `make run-logs` | Follow those logs. |
| `make run-status` | Report whether it is running, and its pid. |
| `make run-stop` | Stop it: SIGINT, then SIGKILL after 10s if it has not exited. |
| `make run-restart` | Stop and start. |
| `make dev` | Run in the foreground instead, for development. Ctrl-C stops it. |

`make run` compiles to `bin/server` and backgrounds that binary rather than
backgrounding `go run`. `go run` stays alive as a parent wrapper around the
process it compiled, so its pid is not the server's — stopping it can leave the
server orphaned and still holding the port.

This survives logging out, but it does not restart on crash and does not come
back after a reboot. Use the systemd service below for anything long-lived.

### As a systemd service

```bash
make start      # writes the unit if absent, then starts the service
```

`make start` runs `deploy/install-service.sh`, which writes
`/etc/systemd/system/whatsapp.service` **only when that file does not already
exist**. An existing unit is never modified, since it may carry hand-tuned
limits or a different user; pass `--force` to replace it (the previous version
is backed up alongside it first).

Once installed, the service is managed the usual way:

```bash
sudo systemctl start whatsapp.service
systemctl status whatsapp.service
journalctl -u whatsapp.service -f
```

`make stop`, `make restart`, `make status` and `make logs` wrap the same
commands.

---

## Configuration

Everything is read from the environment; `.env.example` documents every
variable. The settings that matter most:

| Variable | Purpose |
|---|---|
| `DATABASE_PATH` | SQLite file holding all state. Default `./data/whatsapp.db`. |
| `SESSION_SECRET` | ≥ 32 characters. Required. |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | Creates the first owner account when none exists. |
| `GREEN_API_INSTANCE_ID` / `GREEN_API_TOKEN` | Provider credentials. Without them the panel runs but nothing is sent or received. |
| `GREEN_API_WEBHOOK_TOKEN` | Shared secret for the webhook endpoint. Set it. |
| `TIMEZONE` | Default zone for the panel and new campaigns. Default `Asia/Almaty`. |
| `SECURE_COOKIES` | Must be `true` in production; the app refuses to start otherwise. |
| `TRUSTED_PROXIES` | Proxy addresses whose `X-Forwarded-For` is honoured. Leave empty when exposed directly. |
| `SCHEDULER_STALE_JOB_TTL` | Pending jobs older than this are cancelled instead of sent late. Default 2 h. |

**The server's own clock zone is irrelevant.** Every timestamp is stored in
UTC, each campaign carries an explicit IANA timezone, and conversions happen at
the edges. A server running on UTC and an operator in Almaty see the same
21:00.

---

## Green API setup

1. Create an instance at [green-api.com](https://green-api.com) and scan the QR
   code with the WhatsApp account that will send the messages.
2. Copy the instance id and API token into `GREEN_API_INSTANCE_ID` and
   `GREEN_API_TOKEN`.
3. In the instance settings, set the webhook url to:

   ```
   https://your-domain.example/api/webhooks/greenapi
   ```

4. Set a webhook token in the Green API console and put the same value in
   `GREEN_API_WEBHOOK_TOKEN`. The provider sends it as
   `Authorization: Bearer <token>`; requests without it are rejected.
5. Enable these notifications: incoming messages, incoming file messages,
   outgoing message status, and outgoing API message.

Check the connection under **Баптаулар → Жүйе**; it reports the live instance
state.

---

## Using the admin panel

The interface is in Kazakh.

| Page | What it does |
|---|---|
| **Басты бет** | Counters, contact and message trends, delivery breakdown, campaign funnel. |
| **Чаттар** | Live inbox. All conversations, avatars, full history, and replies with text, image, video, audio, voice or documents. New messages appear without refreshing. |
| **Клиенттер** | Filterable contact list with search, bulk actions, CSV export and a per-contact card. |
| **Кампаниялар** | Create, edit, duplicate, activate, pause, archive. |
| **Автоматтандыру** | Visual timeline builder inside a campaign: add, reorder, retime, enable and preview steps. |
| **Шаблондар** | Message templates with media upload, variable insertion, preview and version history. |
| **Триггерлер** | Every keyword across campaigns and how many contacts each brought in. |
| **Жоспарланған** | The job queue: what is pending, sent, failed, and why. Retry or cancel individually. |
| **Баптаулар** | Provider status, operators, audit log, webhook diagnostics, password change. |

### Setting up a campaign

1. **Шаблондар** → create the messages. For a voice note, choose type
   `Дауыстық хабарлама` and upload MP3, WAV, M4A or OGG; the server converts it
   to OGG/Opus so WhatsApp renders a real voice message rather than a music
   file. Preview the audio before saving.
2. **Кампаниялар** → create a campaign with the date, time, timezone and
   webinar link.
3. Open the campaign → add the trigger phrase (`Дәл сәйкестік` is the safe
   default) and build the steps.
4. Press **Алдын ала қарау** to see every send time in the campaign's timezone.
5. Activate.

### Template variables

| Variable | Renders as |
|---|---|
| `{{contact_name}}` | Contact's full name |
| `{{first_name}}` | First name |
| `{{phone}}` | `+7 700 123 45 67` |
| `{{campaign_name}}` | Campaign name |
| `{{webinar_date}}` | `15.08.2026` |
| `{{webinar_time}}` | `21:00` |
| `{{webinar_datetime}}` | `15.08.2026 21:00` |
| `{{webinar_link}}` | Join link |
| `{{remaining_time}}` | `2 сағат 30 минут` |
| `{{timezone}}` | `Asia/Almaty` |

A variable with no value falls back to a neutral phrase; a placeholder never
reaches a customer as literal `{{...}}` text.

---

## Architecture

```
cmd/
  server/        HTTP API, dashboard, webhook receiver, background workers
  migrate/       apply schema and exit
  seed/          load the example campaign

internal/
  api/           routing, handlers, middleware
  auth/          Argon2id hashing, sessions, CSRF, login throttling
  campaigns/     campaigns, triggers, steps, enrollment, schedule planning
  contacts/      contact records, consent state, chat list
  conversations/ message history
  templates/     templates and revision history
  scheduler/     persistent job queue and workers
  messaging/     outbound sending and the consent guard
  webhooks/      inbound pipeline, idempotency, media capture, avatars
  media/         upload validation, storage, ffmpeg transcoding
  realtime/      Server-Sent Events hub
  analytics/     dashboard aggregates
  exports/       CSV streaming
  audit/         administrative action log
  domain/        entity types shared across packages
  storage/       SQLite handle, query helpers and migration runner

pkg/
  textnorm/      trigger normalization and matching
  timex/         timezone handling
  render/        template variable substitution
  phone/         WhatsApp identifier normalization
  backoff/       retry timing

web/             admin dashboard (vanilla ES modules, no build step)
migrations/      numbered SQL files, embedded into the binary
```

Business logic depends on the `whatsapp.Provider` interface, never on Green API
directly, so a second provider can be added without touching the scheduler,
campaigns or webhook pipeline.

---

## Design decisions

### Template versioning: late binding

Campaign steps reference a template by id and the content is read **at send
time**. Fixing a typo an hour before a webinar corrects every message that has
not gone out yet, which is what an operator expects from an admin panel.

History is still exact: every edit writes an immutable row to
`message_template_versions`, and each sent message records the template id and
version number it rendered from. Future sends move forward; past ones never
change.

The alternative — snapshotting content into each job at enrollment — was
rejected because it makes a correction impossible to apply to a queue that may
already hold thousands of jobs.

### The queue lives in SQLite

Jobs are rows, not timers. A restart, crash or deploy loses nothing. Workers
claim work with a single statement:

```sql
UPDATE scheduled_messages AS sm
SET status = 'PROCESSING', locked_by = $1, locked_at = $2
FROM (SELECT id FROM scheduled_messages
      WHERE status = 'PENDING' AND scheduled_at <= $2
      ORDER BY scheduled_at LIMIT $3) due
WHERE sm.id = due.id RETURNING ...
```

Selection and state change happen in one statement, so a crash between "found"
and "claimed" is impossible. SQLite admits one writer at a time, so a second
worker polling at the same moment either waits for the write lock or sees the
rows already in `PROCESSING` and takes the next batch — the same guarantee
Postgres gets from `SKIP LOCKED`, arrived at by serialising rather than by
skipping. A `PROCESSING` row whose lock has aged past
`SCHEDULER_LOCK_TIMEOUT` is assumed orphaned and re-queued.

**No Redis.** SQLite provides the locking, durability and ordering this queue
needs; a second datastore would add an availability dependency and a
consistency boundary for no benefit at this scale.

### Duplicates cannot happen

- `scheduled_messages` has a unique constraint on
  `(enrollment_id, campaign_step_id, run_number)`. One delivery per step, per
  contact, per campaign run — enforced by the database, not by application
  logic.
- `webhook_events` is unique on `(provider, dedupe_key)`. Provider retries are
  dropped before they reach the domain.
- `messages.external_id` is unique, deduplicating both inbound replays and
  outbound echoes.
- Repeat triggers follow the campaign's configured behaviour: ignore (default),
  restart, continue, or send one special reply.

The one case that cannot be made perfectly exactly-once is a crash *between*
the provider accepting a message and the acknowledgement being recorded, since
Green API offers no idempotency key. The message row is written **before** the
send, so such a case is visible as a `PENDING` row rather than a silent gap, and
a retry that finds an existing provider id will not send again.

### Late jobs are cancelled, not sent

If the service is down for six hours, releasing "starts in 5 hours" messages
afterwards would be worse than sending nothing. Pending jobs older than
`SCHEDULER_STALE_JOB_TTL` are cancelled with a reason visible in the panel.

At enrollment the opposite rule applies: a contact who triggers two hours
before the webinar immediately receives the **most recent** missed step, and
the earlier ones are skipped so they do not arrive out of order.

### Never messaging first is structural

`contacts.first_contact_at` is set exactly once, on the first inbound message.
`Sender.Send` — the single function every outbound path goes through — refuses
to send when it is null. There is no code path, manual or automated, that can
open a conversation with someone who has not written to us. The chat console
shows the reason instead of a composer.

Bulk messaging is deliberately absent from the bulk actions menu.

---

## Testing

```bash
make test              # unit tests
make test-race         # unit tests under the race detector
make test-integration  # no setup: uses a scratch SQLite file
make test-all
```

Unit tests cover trigger normalization and matching (including Unicode,
case folding and word boundaries), schedule computation against the reference
webinar, timezone conversion, template rendering, webhook parsing and dedupe
keys, password hashing, and upload validation.

Integration tests run against a real SQLite database and cover the
guarantees that only show up under concurrency:

- eight simultaneous triggers produce one enrollment
- six workers claiming in parallel never receive the same job twice
- an orphaned lock is recovered and the job runs
- moving the event time reschedules only pending jobs
- unsubscribing cancels the queue
- the unique constraint rejects a duplicate step

```bash
make test-integration
```

Nothing needs provisioning: the suite creates a scratch database in a temp
directory and clears every table between cases. Set `TEST_DATABASE_PATH` to run
against a specific file instead — point it at a throwaway one, since the suite
empties it.

---

## Operations

### Health

`GET /api/health` reports database reachability and provider configuration. The
container healthcheck uses it.

### Logs

Structured JSON by default. Webhook receipt, contact creation, trigger
detection, campaign start, job scheduling, sends, failures, retries, admin
actions and authentication events are all logged. Credentials never are.

### Diagnosing a message that did not arrive

1. **Жоспарланған** → filter by `Қате`. The failure reason is on the row.
2. **Баптаулар → Webhook оқиғалары** → confirm the provider is reaching you.
3. **Баптаулар → Жүйе** → confirm the instance is authorized.
4. Open the contact and check whether they unsubscribed or were blocked.

Failed jobs can be retried individually from the queue page.

### Backups

Two things hold state: the SQLite database file and the media directory
(`media_data` volume). Back up both; media files are referenced by path from
the database.

The database is written in WAL mode, so `DATABASE_PATH` is accompanied by
`-wal` and `-shm` sidecar files. Copying the main file alone while the service
runs can capture a torn state. Either stop the service first and copy all
three, or take a consistent snapshot in place:

```bash
sqlite3 ./data/whatsapp.db ".backup '/backup/whatsapp-$(date +%F).db'"
```

### Scaling

The service runs as a single process against a local database file. Vertical
headroom is large — SQLite handles this workload comfortably, and the send rate
is capped by the provider's rate limit long before the database is the
constraint — but **it does not scale horizontally**: a second replica would
need the same file, and SQLite over a network filesystem is not safe.

If you ever outgrow one machine, the replaceable piece is
`internal/storage/sqlite`, which is the only package that knows what the
database is. Everything above it talks to `Querier` and Postgres-style `$1`
placeholders.

---

## API reference

Full documentation: [`docs/API.md`](docs/API.md).

All endpoints return a consistent envelope:

```json
{ "data": ..., "meta": { "total": 120, "limit": 25, "offset": 0, "has_more": true } }
```

```json
{ "error": { "code": "validation_failed", "message": "…", "details": [...] } }
```

Authentication is an HTTP-only session cookie. Every state-changing request
must echo the CSRF token in an `X-CSRF-Token` header.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/auth/login` | Sign in |
| `POST` | `/api/auth/logout` | Sign out |
| `GET` | `/api/me` | Current operator and CSRF token |
| `GET` | `/api/dashboard` | Dashboard aggregates |
| `GET` | `/api/chats` | Conversation list |
| `GET` | `/api/stream` | Server-Sent Events feed |
| `GET` | `/api/contacts` | Contacts, filterable |
| `GET` | `/api/contacts/{id}/messages` | Conversation history |
| `POST` | `/api/contacts/{id}/send` | Manual reply |
| `GET/POST/PUT/DELETE` | `/api/campaigns[/{id}]` | Campaign management |
| `GET/POST/PUT/DELETE` | `/api/campaigns/{id}/steps[/{stepId}]` | Automation steps |
| `GET/POST/PUT/DELETE` | `/api/templates[/{id}]` | Templates |
| `POST` | `/api/media/upload` | Upload media |
| `GET` | `/api/scheduled-messages` | Job queue |
| `GET` | `/api/exports/contacts` | CSV export |
| `POST` | `/api/webhooks/greenapi` | Provider webhook |

---

## License

Proprietary. All rights reserved.
