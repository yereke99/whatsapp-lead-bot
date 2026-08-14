# API Reference

Base url: `https://your-domain.example`

All request and response bodies are JSON encoded as UTF-8, except media
uploads (multipart) and CSV exports.

---

## Conventions

### Response envelope

Success:

```json
{
  "data": { },
  "meta": { "total": 120, "limit": 25, "offset": 0, "has_more": true }
}
```

`meta` is present only on paginated collections.

Failure:

```json
{
  "error": {
    "code": "validation_failed",
    "message": "Шаблон деректері дұрыс емес",
    "details": [{ "field": "body", "message": "Мәтін бос болмауы керек" }]
  }
}
```

### Error codes

| Code | Status | Meaning |
|---|---|---|
| `bad_request` | 400 | Malformed request |
| `validation_failed` | 400 | Field-level validation failed; see `details` |
| `unauthorized` | 401 | Missing or expired session |
| `forbidden` | 403 | Insufficient role, or the action is not permitted for this contact |
| `csrf_failed` | 403 | Missing or wrong `X-CSRF-Token` |
| `not_found` | 404 | No such resource |
| `conflict` | 409 | Unique constraint, e.g. a duplicate name or trigger |
| `payload_too_large` | 413 | Upload exceeds the configured limit |
| `rate_limited` | 429 | Too many login attempts |
| `internal_error` | 500 | Unexpected server fault |
| `not_configured` | 503 | Green API credentials are absent |
| `service_unavailable` | 502/503 | The provider rejected or did not answer the request |

### Authentication

Sign in with `POST /api/auth/login`. The server sets two cookies:

- `wa_session` — HTTP-only, the session itself
- `wa_csrf` — readable by scripts, must be echoed back

Every `POST`, `PUT` and `DELETE` must include:

```
X-CSRF-Token: <value of the wa_csrf cookie>
```

Sessions last `SESSION_TTL` (12 h by default) and slide forward with activity.

### Roles

| Role | Permissions |
|---|---|
| `OWNER` | Everything, including managing operators and deleting records |
| `ADMIN` | All campaign, contact and messaging operations |
| `VIEWER` | Read-only |

### Pagination

`limit` (default 25, max 200) and `offset`.

---

## Authentication

### POST /api/auth/login

Rate limited per source address.

```json
{ "email": "admin@example.com", "password": "…" }
```

```json
{
  "data": {
    "admin": { "id": "…", "email": "admin@example.com", "role": "OWNER" },
    "csrf_token": "…",
    "expires_at": "2026-08-16T09:00:00Z"
  }
}
```

`401` on bad credentials, `429` when the attempt limit is reached.

### POST /api/auth/logout

Revokes the session and clears the cookies.

### GET /api/me

```json
{
  "data": {
    "admin": { "id": "…", "email": "…", "name": "…", "role": "OWNER" },
    "csrf_token": "…",
    "expires_at": "2026-08-16T09:00:00Z",
    "timezone": "Asia/Almaty"
  }
}
```

### POST /api/me/password

```json
{ "current_password": "…", "new_password": "…" }
```

Minimum 10 characters with at least two character classes. **All sessions,
including the caller's, are revoked on success.**

---

## Dashboard and analytics

### GET /api/dashboard

Returns `summary` counters, `queue` statistics, per-campaign `campaigns` rows,
the active `timezone` and `provider.configured`.

### GET /api/analytics/contacts?days=30

```json
{ "data": [{ "date": "2026-08-01", "value": 12 }] }
```

### GET /api/analytics/messages?days=30

```json
{ "data": { "incoming": [...], "outgoing": [...] } }
```

### GET /api/analytics/delivery?days=30

```json
{ "data": { "sent": 40, "delivered": 900, "read": 620, "failed": 12, "pending": 5 } }
```

### GET /api/analytics/campaigns

Per-campaign contacts, completions, unsubscribes, messages sent, pending,
failed and `completion_rate`.

### GET /api/analytics/triggers?limit=20

How many contacts each keyword brought into the funnel.

---

## Chats and messaging

### GET /api/chats

| Parameter | Description |
|---|---|
| `search` | Name or phone |
| `unread` | `true` to show only unread conversations |
| `limit`, `offset` | Pagination |

Returns contacts ordered by most recent activity, with `avatar_url`,
`unread_count`, `last_message_preview`, `last_message_direction` and
`active_campaign`.

### GET /api/contacts/{id}/messages

| Parameter | Description |
|---|---|
| `limit` | Default 50, max 200 |
| `before` | Message id; pages backwards through history |
| `after` | Message id; returns only newer messages |

Returns messages oldest-first. Locally stored attachments carry
`media_access_url`. Inbound attachments still being fetched have
`media_download_status: "PENDING"`.

### POST /api/contacts/{id}/send

Sends a one-to-one reply. The contact must already exist and must have written
to us first.

```json
{
  "type": "IMAGE_WITH_CAPTION",
  "text": "Сабақ 21:00-де басталады",
  "media_file_id": "…",
  "file_name": "webinar.jpg",
  "link_preview": true
}
```

| `type` | Requires media | Carries text |
|---|---|---|
| `TEXT` | no | yes |
| `IMAGE` | yes | no |
| `IMAGE_WITH_CAPTION` | yes | yes |
| `VIDEO` | yes | no |
| `VIDEO_WITH_CAPTION` | yes | yes |
| `AUDIO` | yes | no |
| `VOICE` | yes | no |
| `DOCUMENT` | yes | yes |

A caption travels inside the same WhatsApp message; it is never sent as a
separate follow-up.

Responses: `201` with the message, `403` when the contact has not opted in or
has unsubscribed, `503` when the provider is unconfigured, `502` when the
provider rejected the send.

### POST /api/contacts/{id}/read

Clears the unread counter.

### GET /api/stream

Server-Sent Events. Event types:

| Event | Payload |
|---|---|
| `message.created` | The new message |
| `message.status` | `{ external_id, status, at }` |
| `chat.updated` | Preview, direction, timestamp, `unread_delta` |
| `contact.updated` | Changed contact fields |
| `job.updated` | `{ job_id, status, detail }` |
| `provider.state` | `{ state }` |

Each frame carries `contact_id` where relevant. The browser reconnects
automatically; reconcile any gap with `GET /api/contacts/{id}/messages?after=…`.

---

## Contacts

### GET /api/contacts

| Parameter | Description |
|---|---|
| `search` | Name or phone |
| `status` | `NEW`, `ACTIVE`, `COMPLETED`, `UNSUBSCRIBED`, `BLOCKED`, `ERROR` |
| `campaign_id` | Only contacts enrolled in this campaign |
| `trigger` | Exact trigger keyword |
| `opted_out` | `true` / `false` |
| `tag_id` | Filter by tag |
| `created_from`, `created_to` | `YYYY-MM-DD`, in the app timezone |
| `sort` | `created_desc` (default), `created_asc`, `activity_desc`, `activity_asc`, `name_asc` |

### GET /api/contacts/{id}

Returns `contact`, `enrollments` and `scheduled` jobs.

### PUT /api/contacts/{id}

```json
{ "name": "Әлішер Сәрсенов", "notes": "…" }
```

### POST /api/contacts/{id}/block

```json
{ "blocked": true }
```

Blocking also cancels every pending job for the contact.

### POST /api/contacts/{id}/unsubscribe

```json
{ "opted_out": true }
```

### POST /api/contacts/{id}/refresh-profile

Re-fetches the display name and avatar from the provider.

### POST /api/contacts/bulk

```json
{ "contact_ids": ["…"], "action": "unsubscribe" }
```

Actions: `unsubscribe`, `resubscribe`, `block`, `unblock`,
`remove_from_automation`, `tag` (with `tag_name` and optional `tag_color`).

Maximum 5000 ids. **There is no bulk send action**: outbound messaging is
limited to campaign automation and one-to-one replies.

### DELETE /api/contacts/{id}

Owner only. Removes the contact and its history.

---

## Campaigns

### GET /api/campaigns

`search`, `status`, `include_archived`. Each campaign includes its triggers and
aggregate counts.

### POST /api/campaigns · PUT /api/campaigns/{id}

```json
{
  "name": "Түрік айран вебинары",
  "description": "…",
  "event_type": "WEBINAR",
  "event_date": "2026-08-16",
  "event_time": "21:00",
  "timezone": "Asia/Almaty",
  "webinar_link": "https://example.com/live",
  "existing_contact_behavior": "IGNORE",
  "existing_contact_template_id": "",
  "unsubscribe_keywords": ["STOP", "ТОҚТАТУ"],
  "catch_up_missed_steps": true,
  "max_send_attempts": 5
}
```

`event_date` and `event_time` are wall-clock values in `timezone`; the server
converts them to a UTC instant.

`existing_contact_behavior` controls repeat triggers:

| Value | Effect |
|---|---|
| `IGNORE` | Nothing happens (default) |
| `RESTART` | Cancels pending jobs and re-enrols from scratch |
| `CONTINUE` | Keeps the existing schedule, fills any gaps |
| `SPECIAL_MESSAGE` | Sends `existing_contact_template_id` once |

**Update returns the rescheduling result:**

```json
{
  "data": {
    "campaign": { },
    "event_time_changed": true,
    "rescheduled_jobs": 248
  }
}
```

Only `PENDING` jobs move. Sent, failed and cancelled jobs are history and are
never rewritten.

### POST /api/campaigns/{id}/status

```json
{ "status": "ACTIVE" }
```

Activation is refused unless the campaign has an event start time, at least one
enabled step, and at least one trigger.

### POST /api/campaigns/{id}/duplicate

Copies the campaign and its steps as a `DRAFT`. Triggers are not copied,
because one keyword may route to only one active campaign.

### GET /api/campaigns/{id}/preview

Every step resolved to an absolute time in the campaign's timezone:

```json
{
  "data": [{
    "step_id": "…",
    "name": "5 сағат бұрын",
    "offset_seconds": -18000,
    "offset_label": "-5h",
    "local_time": "2026-08-16 16:00:00",
    "utc_time": "2026-08-16T11:00:00Z",
    "template_name": "…",
    "template_type": "IMAGE_WITH_CAPTION",
    "enabled": true
  }]
}
```

---

## Automation steps

### POST /api/campaigns/{id}/steps · PUT /api/campaigns/{id}/steps/{stepId}

```json
{
  "name": "1 сағат бұрын",
  "schedule_kind": "RELATIVE_TO_EVENT",
  "offset_seconds": -3600,
  "message_template_id": "…",
  "enabled": true
}
```

`schedule_kind` is `RELATIVE_TO_EVENT` or `ON_TRIGGER` (fires the moment a
contact enrols, ignoring the event anchor).

`offset_seconds` is signed: negative is before the event. Seconds allow
fractional-minute steps — 7.5 minutes is `-450`.

Side effects on update:

- disabling a step cancels its pending jobs
- enabling a step back-fills it for active enrollments whose time has not passed
- changing the offset reschedules its pending jobs

### POST /api/campaigns/{id}/steps/reorder

```json
{ "step_ids": ["…", "…"] }
```

### DELETE /api/campaigns/{id}/steps/{stepId}

Cancels pending jobs for the step, then removes it.

---

## Triggers

### GET /api/triggers

Every trigger across campaigns.

### POST /api/campaigns/{id}/triggers · PUT /api/triggers/{id}

```json
{
  "keyword": "Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді",
  "match_mode": "EXACT",
  "is_active": true
}
```

| Mode | Behaviour |
|---|---|
| `EXACT` | The whole message equals the keyword after normalization (default) |
| `STARTS_WITH` | Message begins with the keyword, ending on a word boundary |
| `CONTAINS` | Keyword appears as a whole word; minimum 4 characters |

Normalization applies to both sides: NFKC composition, case folding,
whitespace collapsing, zero-width character removal, and folding of
typographic dashes, quotes and slashes. `  АЙРАН  ` matches `Айран`.

`409` when the keyword is already used by another active campaign.

---

## Templates

### GET /api/templates

`search`, `type`, `include_archived`.

### POST /api/templates · PUT /api/templates/{id}

```json
{
  "name": "15 минут бұрын",
  "description": "…",
  "type": "IMAGE_WITH_CAPTION",
  "body": "Сабақ басталады: {{webinar_link}}",
  "media_file_id": "…",
  "file_name": "webinar.jpg",
  "link_preview": true
}
```

Validation mirrors the database constraints: media types require a file, text
requires a body, and types without captions reject one.

Each save increments `version` and writes an immutable revision row. Content is
resolved **at send time**, so an edit reaches every message not yet sent.

### DELETE /api/templates/{id}

Removes the template outright when nothing references it; otherwise archives it
so existing steps keep working. Returns `{ "archived": true|false }`.

### GET /api/templates/{id}/preview

Any query parameter overrides a sample variable value.

```json
{
  "data": {
    "type": "IMAGE_WITH_CAPTION",
    "rendered_text": "…",
    "media_url": "/api/media/…/content",
    "unknown_variables": ["typo_name"]
  }
}
```

### GET /api/templates/{id}/versions · GET /api/templates/variables

Revision history, and the catalog of supported variables.

---

## Media

### POST /api/media/upload

`multipart/form-data`:

| Field | Description |
|---|---|
| `file` | The file |
| `kind` | Optional: `IMAGE`, `VIDEO`, `AUDIO`, `VOICE`, `DOCUMENT` |

The content type is decided by sniffing the bytes, not the filename or the
client's header. Allowed: JPEG, PNG, WebP, GIF, MP4, MOV, 3GP, WebM, MP3, WAV,
M4A, AAC, OGG, PDF, TXT, CSV and Office documents.

**Voice:** with `kind=VOICE`, audio that is not already OGG/Opus is transcoded
to mono 48 kHz Opus so WhatsApp renders a real voice note. Both the original
and the converted file are kept; the response describes the converted one. If
ffmpeg is unavailable the request fails with `503` rather than silently sending
a music file.

Errors: `400` for a disallowed type or a content mismatch, `413` when too
large.

### GET /api/media/{id}/content

Streams the file with range support. `?download=true` forces a download.

### DELETE /api/media/{id}

`409` if a template still references it.

---

## Scheduled messages

### GET /api/scheduled-messages

`campaign_id`, `contact_id`, `status`, `from`, `to`, `limit`, `offset`.

Statuses: `PENDING`, `PROCESSING`, `SENT`, `FAILED`, `CANCELLED`.

Each row carries `attempt_count`, `last_error`, `cancel_reason`, `sent_at` and
the contact and step it belongs to.

### POST /api/scheduled-messages/{id}/retry

Returns a `FAILED` or `CANCELLED` job to the queue with its attempt counter
reset.

### POST /api/scheduled-messages/{id}/cancel

Cancels a pending job.

---

## Export

### GET /api/exports/contacts

Accepts the same filters as `GET /api/contacts`; the export covers the whole
filtered set, not the current page.

Returns `text/csv` with a UTF-8 BOM so Excel reads Kazakh and Cyrillic
correctly. Columns: phone, name, push_name, campaign, trigger, status,
opted_out, first_contact, last_activity, messages_received, messages_sent,
created_at.

---

## System and audit

### GET /api/system/settings

Timezone, environment, provider status, whether voice transcoding is available,
upload limit, scheduler state, the variable catalog and the number of connected
dashboards.

### GET /api/system/provider

Live Green API instance state.

### GET /api/audit-logs

`action`, `entity_type`, `entity_id`, `admin_id`, `search`, pagination.

Records logins, campaign and template changes, event-time changes with the old
and new values, contact actions, manual messages, bulk actions, exports and
media operations.

### GET /api/webhook-events

Raw provider deliveries with their processing status and attempt counts.
Useful for confirming the provider is reaching the server.

### GET /api/health

Unauthenticated.

```json
{
  "data": {
    "status": "ok",
    "database": "ok",
    "provider_ready": true,
    "realtime_clients": 2,
    "time": "2026-08-16T09:00:00Z"
  }
}
```

`503` when the database is unreachable.

---

## Operators

Owner only.

### GET /api/admins · POST /api/admins · PUT /api/admins/{id} · DELETE /api/admins/{id}

```json
{ "email": "operator@example.com", "name": "…", "password": "…", "role": "ADMIN" }
```

Deactivating an operator revokes their sessions immediately. The last active
owner cannot be removed.

---

## Webhook

### POST /api/webhooks/greenapi

Called by Green API. Authenticated with `GREEN_API_WEBHOOK_TOKEN`, sent as
`Authorization: Bearer <token>`.

Handled notification types: `incomingMessageReceived`, `outgoingMessageStatus`,
`outgoingMessageReceived`, `outgoingAPIMessageReceived`, `stateInstanceChanged`
and `incomingCall`.

The handler stores the raw payload and returns immediately; parsing, contact
creation and campaign enrollment happen on background workers, so a slow
domain operation can never cause the provider to time out and retry.

Responses are always `200` for an accepted or duplicate delivery, because a
non-2xx would make the provider retry work that is already queued:

```json
{ "data": { "status": "accepted" } }
{ "data": { "status": "duplicate" } }
{ "data": { "status": "ignored" } }
```

`401` when the token is wrong.

Idempotency is keyed on the provider message id, with the status value included
for delivery notifications so `sent`, `delivered` and `read` are each processed
once. Delivery states only ever move forward: a late `sent` webhook cannot
undo a recorded `read`.
