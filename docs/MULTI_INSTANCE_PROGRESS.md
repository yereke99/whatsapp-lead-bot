# Multi-account WhatsApp support

One Go process, one SQLite database, several independent Green API WhatsApp
numbers. The number already in production is account 1 ("primary") and behaves
exactly as it did before.

**Status: implemented, compiled, vetted, and rehearsed against a copy of the
real database. Not yet committed, and the full test suite has not been run on a
machine with the real dependencies.** See "Before deploying" below.

---

## What it does

```
whatsapp_instances
        │
        └── campaigns ──┬── triggers
                        ├── steps and templates
                        ├── enrolments
                        └── scheduled messages ── retries
```

A campaign belongs to exactly one number, chosen at creation and fixed
afterwards. Everything downstream derives the number from it, so there is one
answer to "which phone does this leave through" and no second copy to drift.

| Situation | Behaviour |
|---|---|
| Same trigger word on both numbers | Allowed; the receiving number decides. |
| A word from number 2 sent to number 1 | Matches nothing. |
| One person writes to both numbers | One contact, two independent enrolments. |
| `STOP` to number 2 | Ends number 2's sequence only. |
| Administrator unsubscribes / blocks | Applies to every number. |
| Both numbers due at 20:45 | Own client, own pace, no cross-send, no waiting. |
| Number 2 offline | Its poller backs off alone; number 1 is unaffected. |

## Decisions

| Question | Decision |
|---|---|
| Ownership | Only `campaigns` carries it. |
| Campaign account | Immutable after creation — API returns 409, and the repository's `Update` does not include the column. |
| Contacts | One row per phone, unchanged. Isolation comes from the pre-existing `campaign_contacts UNIQUE (campaign_id, contact_id)`. |
| Unsubscribe | Per account (`contact_instance_optouts`). `contacts.opted_out` / `status` keep their old meaning for the panel. |
| Blocking | Global — a decision about the person. |
| Templates | Shared library; a template is inert content. |
| Credentials | Environment only. Never in the database, the API, or a log. |
| Config naming | `GREEN_API_INSTANCE_ID_2` / `_TOKEN_2`; `_3` for a third, no code change. |
| Deployment | `git pull && make run-restart`. `docker-compose.yml` untouched. |

---

## Migration `0007_whatsapp_instances.sql`

Additive and transactional. No `DROP TABLE`, no `TRUNCATE`, no row deleted.

1. creates `whatsapp_instances`, bootstraps `primary` (1) and `secondary` (2)
   with `WHERE NOT EXISTS`, so a re-run cannot duplicate them;
2. adds nullable `whatsapp_instance_id` to `campaigns`, `campaign_triggers`,
   `messages`, `webhook_events` and backfills every row to Primary;
3. creates `contact_instance_optouts` and carries every existing unsubscribe
   onto Primary, so the new guard is identical to the old one on one number;
4. the only structural change: `DROP INDEX campaign_triggers_unique_active`,
   recreated as `(whatsapp_instance_id, normalized_keyword, match_mode)`.

Templates, steps, schedules, contacts, enrolments and delivery history are not
touched by a single statement.

### Rehearsal result — copy of the real `data/whatsapp.db`

Applied 0003→0007 on a copy, at the production schema level:

```
    campaigns                        1       (unchanged)
    campaign_steps                   5       (unchanged)
    campaign_contacts                1       (unchanged)
    contacts                        25       (unchanged)
    messages                       307       (unchanged)
    scheduled_messages               2       (unchanged)
    message_templates                6       (unchanged)
    webhook_events                 893       (unchanged)
  + whatsapp_instances               2       (new)
  + contact_instance_optouts         0       (new)

  [ok] whatsapp_instances bootstrapped      [ok] campaign configuration unchanged
  [ok] every campaign has an owner          [ok] steps unchanged
  [ok] every campaign is on Primary         [ok] templates unchanged
  [ok] triggers / messages / events backfilled
  [ok] trigger uniqueness is per account    [ok] enrolments unchanged
  [ok] sqlite integrity_check: ok           [ok] scheduled messages unchanged
  [ok] foreign keys intact                  [ok] contacts unchanged
                                            [ok] message history unchanged
```

Also verified: a fresh empty database takes all seven migrations; re-running
the bootstrap inserts leaves two accounts; the same keyword is accepted on two
accounts and still refused twice on one. The script is `~/scratch/rehearse.py`
(outside the repository).

---

## Code

| Area | Change |
|---|---|
| `internal/domain/whatsapp_instance.go` | new entity, `PrimaryInstanceID`, `InstanceIDOrPrimary` |
| `internal/domain/models.go` | ownership on campaign / trigger / message; `Contact.CanBeContacted()` |
| `internal/config` | `Config.Instances`, `_N` parsing, per-account pacing, both-or-neither validation, duplicate-instance-id refusal |
| `internal/instances` | **new** — account repository + registry; `ProviderFor(id)` is the single campaign→phone lookup |
| `internal/contacts/optout.go` | **new** — per-account opt-out |
| `internal/outbound/router.go` | **new** — one queue and rate limiter per account |
| `internal/messaging/sender.go` | client resolved per message; account-aware consent |
| `internal/inbound` | one poller per account with its own backoff; account carried through ingest, matching, unsubscribe, echoes; dedupe key namespaced per account |
| `internal/campaigns` | trigger matching, stop words, enrolment stopping and **reconciliation** all scoped by account |
| `internal/scheduler` | job context reads the campaign's account and the per-account opt-out; retries re-derive it |
| `internal/api` | `GET /api/whatsapp-instances[/states]`; account on create, refused on update; list filter |
| `cmd/server/main.go` | registry after migrations, poller per account, outbound router, shutdown waits for all |
| Admin panel | account selector (locked after creation), badge on list and detail, list filter, account column on triggers, connection state per number in settings |

## Tests written

- `internal/integration/multi_instance_test.go` — bootstrap; pre-0007 rows
  routing as Primary; cross-account trigger isolation; the same word on both
  numbers; uniqueness still holding within one number; one phone enrolled on
  both; unsubscribe scoped; jobs resolving their own account; the per-account
  opt-out reaching the send path; ownership surviving an update; triggers
  inheriting their campaign's account; reconciliation still repairing the other
  number.
- `internal/outbound/router_test.go` — a stuck send on one account does not
  block the other; unknown account refused with no fallback; unset account
  falls back to Primary; the two queues are distinct.
- `internal/instances/registry_test.go` — `ProviderFor` never falls back
  (disabled / unconfigured / unknown all error); unset id resolves to Primary;
  `Active()` only lists accounts that can work.

## Verification done here

- `gofmt` clean across `cmd`, `internal`, `pkg`
- `go build ./...` — clean
- `go vet ./...` and `go vet -tags=integration ./...` — clean
- `go test ./internal/outbound/... ./internal/instances/...` — pass, also under
  `-race`
- every `web/js` module parses

This environment has no access to the Go module proxy, so the build above ran
against local stubs of `github.com/google/uuid`, `golang.org/x/crypto`,
`golang.org/x/text` and `modernc.org/sqlite`. That is enough to type-check
every line, and it is why the remaining packages' tests could not be run here:
their behaviour depends on the real argon2, the real Unicode normaliser and the
real SQLite driver. `go.mod` in the repository is untouched.

---

## Before deploying

```bash
make fmt          # no-op, already formatted
make build
make test-all     # the existing suite is the backward-compatibility proof
```

Then, in `.env`:

```dotenv
GREEN_API_INSTANCE_ID_2=...
GREEN_API_TOKEN_2=...
```

and

```bash
git pull
make run-restart
```

Migration `0007` applies itself at startup on top of the existing database.
Without the two variables the service starts exactly as before, on one number,
with the second account shown in the panel as not connected.

## Housekeeping

`dist/sync.tgz` is a build-sync artefact from this work. It is inside the
gitignored `dist/` directory and can be deleted.
