# WhatsApp Campaign Automation

A reusable WhatsApp campaign automation platform built on Go and Green API.

An administrator creates campaigns, defines the exact phrase that opens the
funnel, and builds a timeline of messages relative to an event. The Turkish
Ayran / Kaymak webinar is the first campaign loaded into it, not something the
code knows about.

It is an automation platform, not a WhatsApp client: there is no inbox and no
live chat. The panel manages campaigns, and the bot only ever replies to a
configured trigger.

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
Green API notification queue
        │
GET receiveNotification (long poll)  ──▶  stored raw, deduplicated by event id
        │
        ▼
Background worker: create contact ─▶ record consent ─▶ match trigger
        │
        ▼
Enrol into campaign, write one job per step into SQLite
        │
        ▼
Scheduler claims due jobs (one atomic UPDATE, serialised per contact)
        │
        ▼
Outbound queue: bounded concurrency, spaced-out sending
        │
        ▼
Render template ─▶ Green API ─▶ WhatsApp
        │
        ▼
Delivery receipts arrive on the same queue and update the message
```

Nothing is ever sent inline. A trigger produces database rows; a worker turns
those rows into messages later. That is what makes the whole funnel survive a
restart, and why there is no `time.Sleep` anywhere in the scheduling path.

### The message queue

A campaign is an ordered list of messages. Each one pairs a reusable template
with the moment it should go out:

```
Template          =  WHAT to send   (text, image, video, voice, document)
Campaign step     =  WHEN + WHAT    (a template plus its schedule)
Campaign          =  WHO + trigger  (who enters, and on what phrase)
Scheduler         =  executes it
Outbound queue    =  sends it safely
```

A template never carries a time. The same "one hour to go" template can sit in
three different campaigns at three different times.

### Two ways to schedule a message

**An exact date and time** — every contact receives it at the same moment. The
admin picks a wall-clock time and the panel stores the offset from the
campaign's event start, so moving the webinar moves the whole queue with it.

A webinar at 21:00 produces exactly:

| Offset | Send time | Type |
|---|---|---|
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

**A delay after the trigger** — the offset counts forward from the customer's
own message, so each contact gets a personal timetable:

```
Contact A writes at 17:30      Contact B writes at 18:10
  17:30:02  greeting             18:10:02  greeting
  17:40     message 2            18:20     message 2
  18:00     message 3            18:40     message 3
  18:30     message 4            19:10     message 4
```

The greeting is a queued job like any other, scheduled a couple of seconds out
(`TRIGGER_GREETING_DELAY_MS`) so the first reply does not land in the same
instant as the message that asked for it.

A contact who joins late does not receive messages whose moment has passed:
someone triggering at 20:40 gets the 20:45 message onwards, not a burst of
"5 hours to go" arriving after the fact.

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
scheduler workers and the Green API poller. The database file is created on
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
| `GREEN_API_RECEIVE_TIMEOUT` | How long Green API holds a poll open. Must stay below `GREEN_API_TIMEOUT`. Default 20 s. |
| `GREEN_API_WORKERS` | Workers applying received notifications. Default 5. |
| `TIMEZONE` | Default zone for the panel and new campaigns. Default `Asia/Almaty`. |
| `SECURE_COOKIES` | Must be `true` in production; the app refuses to start otherwise. |
| `TRUSTED_PROXIES` | Proxy addresses whose `X-Forwarded-For` is honoured. Leave empty when exposed directly. |
| `SCHEDULER_STALE_JOB_TTL` | Pending jobs older than this are cancelled instead of sent late. Default 2 h. |
| `TRIGGER_GREETING_DELAY_MS` | Pause between a trigger and the first reply, as a queued job rather than a sleep. Range 1000–5000, default 2000. |
| `WHATSAPP_SEND_WORKERS` | Messages in flight at once. Default 1 — for a single account, keep it there. Max 4. |
| `WHATSAPP_MIN_SEND_DELAY_MS` / `WHATSAPP_MAX_SEND_DELAY_MS` | Pause between two sends, drawn at random from this range. Defaults 2000 / 5000; the minimum may not go below 1000. |

**The server's own clock zone is irrelevant.** Every timestamp is stored in
UTC, each campaign carries an explicit IANA timezone, and conversions happen at
the edges. A server running on UTC and an operator in Almaty see the same
21:00.

---

## Green API setup

1. Create an instance at [green-api.com](https://green-api.com) and scan the QR
   code with the WhatsApp account that will send the messages.
2. Copy the instance id and API token into `GREEN_API_INSTANCE_ID` and
   `GREEN_API_TOKEN`, and set `GREEN_API_URL` to **your instance's host**. Green
   API shards instances by the first four digits of the id, so `7105xxxxxx` is
   served by `https://7105.api.greenapi.com`. The generic host does not reach a
   sharded instance, and it fails silently: polls succeed and return an empty
   queue, which is indistinguishable from nobody having messaged the bot.
3. Turn the notification types on. **This is the step that is easy to miss** —
   a new instance has them all off, and Green API only queues the types that
   are enabled, so with them off the bot receives nothing and reports no error:

   ```bash
   set -a; . ./.env; set +a
   curl -s -X POST "$GREEN_API_URL/waInstance$GREEN_API_INSTANCE_ID/setSettings/$GREEN_API_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"webhookUrl": "",
          "incomingWebhook": "yes",
          "outgoingMessageWebhook": "yes",
          "outgoingAPIMessageWebhook": "yes",
          "stateWebhook": "yes"}'
   ```

   Green API restarts the instance after this; give it a minute. `incomingWebhook`
   is the one triggers depend on.

**There is no webhook to configure.** Despite their names, those settings
control whether a notification is *produced* at all, not where it is delivered.
Messages are pulled from the queue with `receiveNotification`, so the server
needs no public url, no TLS certificate and no inbound firewall rule — keep
`webhookUrl` empty.

Verify the instance is connected:

```bash
curl -s "$GREEN_API_URL/waInstance$GREEN_API_INSTANCE_ID/getStateInstance/$GREEN_API_TOKEN"
# {"stateInstance":"authorized"}

curl -s "$GREEN_API_URL/waInstance$GREEN_API_INSTANCE_ID/getSettings/$GREEN_API_TOKEN" \
  | python3 -m json.tool | grep -iE "webhookUrl|incomingWebhook"
```

The panel reports the same state under **Баптаулар → Жүйе**.

---

## Using the admin panel

The interface is in Kazakh.

| Page | What it does |
|---|---|
| **Басты бет** | Counters, contact and message trends, delivery breakdown, campaign funnel. |
| **Клиенттер** | Filterable contact list with search, bulk actions, CSV export and a per-contact card. |
| **Кампаниялар** | Create, edit, duplicate, activate, pause, archive. |
| **Хабарламалар кезегі** | The message queue inside a campaign: add, drag to reorder, retime, enable, duplicate, preview and delete each message. |
| **Шаблондар** | Message templates with media upload, variable insertion, preview and version history. |
| **Триггерлер** | Every keyword across campaigns and how many contacts each brought in. |
| **Жоспарланған** | The job queue: what is pending, sent, failed, and why. Retry or cancel individually. |
| **Баптаулар** | Provider status, operators, audit log, inbound event log, password change. |

### Setting up a campaign

1. **Шаблондар** → create the messages. For a voice note, choose type
   `Дауыстық хабарлама` and upload MP3, WAV, M4A or OGG; the server converts it
   to OGG/Opus so WhatsApp renders a real voice message rather than a music
   file. Preview the audio before saving.
2. **Кампаниялар** → create a campaign with the date, time, timezone and
   webinar link.
3. Open the campaign → add the trigger phrase (`Дәл сәйкестік` is the safe
   default), then build the message queue. For each message pick a template and
   either an exact date and time, or a delay counted from the customer's own
   trigger.
4. Press **Хронология** to see every send time in the campaign's timezone
   before going live. The page also shows a checklist of anything that would
   block activation.
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
  server/        HTTP API, dashboard, notification poller, background workers
  migrate/       apply schema and exit
  seed/          load the example campaign

internal/
  api/           routing, handlers, middleware
  auth/          Argon2id hashing, sessions, CSRF, login throttling
  campaigns/     campaigns, triggers, steps, enrollment, schedule planning
  contacts/      contact records, consent state
  conversations/ message history
  templates/     templates and revision history
  scheduler/     persistent job queue and workers
  outbound/      the single controlled path to WhatsApp: concurrency and pacing
  messaging/     outbound sending and the consent guard
  inbound/       Green API queue poller, ingest pipeline, idempotency
  media/         upload validation, storage, ffmpeg transcoding
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
campaigns or the inbound pipeline.

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

### Sending is deliberately slow

Every automated message leaves through one gate (`internal/outbound`), which
bounds how many sends run at once and how far apart they go out. Without it,
each scheduler worker would call the provider the moment it claimed a job, and
a hundred contacts due at 21:00 would become a hundred near-simultaneous API
calls from a single WhatsApp account.

Ordering within a conversation is handled one level down, in the claim query,
which never hands out a second job for a contact while one is in flight and
never hands out a later message before an earlier one. A contact with three
overdue messages therefore receives them one at a time, in order — the gate
only has to care about the global pace.

"In flight" means a worker lease that is still alive. This matters more than it
sounds: serialising per contact would otherwise let one message that never
completes silence that customer's entire funnel. A `PROCESSING` row whose lease
has expired belongs to a worker that is gone, so the queue steps over it and
keeps moving while the recovery sweep requeues it. `SCHEDULER_LOCK_TIMEOUT` is
therefore both the recovery deadline and the worst case for how long one stuck
message can delay the next one — which is why its default is minutes, not
hours.

The point is reliability, not evasion. There is no account rotation, no proxy
juggling and no attempt to disguise automated traffic; the queue simply refuses
to go faster than configured, and holds work rather than dropping it when a
campaign hits its own hourly or daily limit.

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
open a conversation with someone who has not written to us. The platform
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
webinar, timezone conversion, template rendering, notification parsing and dedupe
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

Structured JSON by default. Notification receipt, contact creation, trigger
detection, campaign start, job scheduling, sends, failures, retries, admin
actions and authentication events are all logged. Credentials never are.

### Diagnosing a message that did not arrive

Every queued message carries its own verdict, so the question is always
answerable from the row itself.

1. **Жоспарланған** → filter by the campaign. Read `status` first:

   | Status | Meaning |
   |---|---|
   | `PENDING`, `next_attempt_at` in the future | Waiting out a retry backoff, or held because the campaign is paused or at a sending limit. `last_error` says which. |
   | `PENDING`, due, not moving | An earlier message for the same contact has not finished. Check that contact's other rows. |
   | `PROCESSING` with an old `locked_at` | The worker holding it died. Recovery requeues it within `SCHEDULER_LOCK_TIMEOUT`. |
   | `FAILED` | Gave up after `max_send_attempts`; `last_error` has the provider's reason. |
   | `CANCELLED` | `cancel_reason` says why — unsubscribed, step disabled, campaign archived, or the send window expired. |
   | no row at all | It was never queued. The step's time had already passed when the contact enrolled, which is the late-joiner rule. |

2. **Баптаулар → Кіріс оқиғалар** → confirm notifications are being drained.
3. **Баптаулар → Жүйе** → confirm the instance is authorized.
4. Open the contact and check whether they unsubscribed or were blocked.

Straight from the database, for one campaign:

```sql
SELECT cs.order_index, COALESCE(NULLIF(cs.name,''), t.name) AS step,
       sm.scheduled_at, sm.status, sm.attempt_count,
       COALESCE(NULLIF(sm.last_error,''), sm.cancel_reason, '') AS problem
FROM scheduled_messages sm
JOIN campaign_steps cs   ON cs.id = sm.campaign_step_id
JOIN message_templates t ON t.id = cs.message_template_id
JOIN contacts c          ON c.id = sm.contact_id
WHERE c.phone = '77011234567'
ORDER BY sm.scheduled_at;
```

Failed jobs can be retried individually from the queue page.

**Sending is serialised per contact**, so one message that cannot complete
delays that contact's later messages until its worker lease expires. If a whole
funnel goes quiet after one particular step, look at that step's row first —
its `last_error` is usually the answer for everything behind it.

### The bot does not react to an inbound message

Work down this list; each step distinguishes the next.

```bash
grep GREENAPI run/server.log | tail -20
```

| What the log shows | Meaning |
|---|---|
| no `polling started` line | The poller never ran. Credentials are missing, or the build predates polling. |
| repeated `receive failed` | The provider is unreachable or the token is wrong. The error text says which. |
| **only** `polling started`, nothing else | Polls succeed and the queue is always empty — the provider is not producing notifications. Continue below. |
| `notification queued` but no `trigger handled` | The message arrived; the text did not match a trigger. |

An always-empty queue is almost always one of two setup mistakes, both silent:

- **Notification types are off.** A fresh instance has `incomingWebhook: "no"`,
  and Green API only queues what is enabled. Check with `getSettings` and fix
  with `setSettings` — see [Green API setup](#green-api-setup).
- **Wrong host.** A sharded instance (`7105xxxxxx`) must be polled on
  `7105.api.greenapi.com`. The generic host answers, but for a different shard.

If notifications arrive but no trigger fires, compare the message against the
configured keyword and match mode: `EXACT` requires the whole message to equal
the keyword, so a long trigger phrase will not fire on a single word from it.

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
| `GET` | `/api/contacts` | Contacts, filterable |
| `GET` | `/api/contacts/{id}/messages` | Message log for one contact (read-only) |
| `POST` | `/api/contacts/{id}/send` | Manual reply |
| `GET/POST/PUT/DELETE` | `/api/campaigns[/{id}]` | Campaign management |
| `GET/POST/PUT/DELETE` | `/api/campaigns/{id}/steps[/{stepId}]` | Automation steps |
| `GET/POST/PUT/DELETE` | `/api/templates[/{id}]` | Templates |
| `POST` | `/api/media/upload` | Upload media |
| `GET` | `/api/scheduled-messages` | Job queue |
| `GET` | `/api/exports/contacts` | CSV export |

---

## License

Proprietary. All rights reserved.
