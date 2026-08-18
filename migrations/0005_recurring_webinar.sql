-- Daily recurring webinars: one campaign whose event happens every day at the
-- same local time, rather than one campaign per date.
--
-- The whole feature is an *anchor* change. A campaign step already resolves to
-- "anchor + offset_seconds"; today the anchor is always campaigns.event_start_at.
-- With recurrence on, each enrolment pins the occurrence it belongs to and that
-- becomes its anchor instead. Nothing downstream of the anchor changes: the
-- planner, the reconciler, the audience filter, the queue's unique constraint,
-- the worker and the Green API send path are all untouched.
--
-- Pinning per enrolment rather than rolling campaigns.event_start_at forward is
-- deliberate, and it is what keeps the two dangerous outcomes impossible:
--
--   * no duplicates -- the occurrence is a property of the enrolment, so the
--     existing UNIQUE (enrollment_id, campaign_step_id, run_number) already is
--     the "one job per campaign + occurrence + step + recipient" guarantee the
--     queue needs. No second idempotency mechanism is introduced.
--   * no backlog -- a recurring campaign does not re-send itself to every
--     historical contact every day. A contact belongs to the one occurrence
--     that was current when they entered, exactly as the existing enrolment
--     rules say.
--
-- Everything here is additive with defaults that reproduce today's behaviour,
-- so every campaign, enrolment and queued message already in this database
-- keeps working exactly as it does now. The feature does nothing at all until
-- an operator ticks the box on a specific campaign.
--
-- Rollback (this project applies migrations forward only; run by hand if ever
-- needed, after stopping the service):
--
--   ALTER TABLE campaigns         DROP COLUMN is_daily_recurring;
--   ALTER TABLE campaigns         DROP COLUMN recurrence_time;
--   ALTER TABLE campaigns         DROP COLUMN recurrence_start_date;
--   ALTER TABLE campaign_contacts DROP COLUMN occurrence_at;

-- Off for every existing row. This is what makes the feature opt-in: a campaign
-- nobody has configured is scheduled from event_start_at precisely as before.
ALTER TABLE campaigns ADD COLUMN is_daily_recurring INTEGER NOT NULL DEFAULT 0;

-- The daily start, as 'HH:MM' wall-clock text read in campaigns.timezone. It is
-- stored as a local time rather than as an instant on purpose: "21:00 every day
-- in Asia/Almaty" is a calendar statement, and adding 24h to a UTC timestamp
-- would drift the moment a zone changes its offset. Occurrences are built with
-- Go's zone database, one calendar day at a time.
--
-- NULL means "not configured", which is the only valid state while
-- is_daily_recurring is 0.
ALTER TABLE campaigns ADD COLUMN recurrence_time TEXT;

-- The first calendar day of the series, 'YYYY-MM-DD' local to the same zone.
-- It is what lets an operator schedule a series that starts next Monday, and
-- it keeps the existing date picker meaningful instead of removing it: for a
-- recurring campaign the date the admin picks is the day the series begins.
-- NULL falls back to the day of event_start_at.
ALTER TABLE campaigns ADD COLUMN recurrence_start_date TEXT;

-- The webinar occurrence this enrolment belongs to, as a canonical UTC
-- timestamp in the same fixed-width form as every other timestamp here, so it
-- compares correctly as text.
--
-- NULL means "no occurrence pinned", which is every row that exists today and
-- every row a non-recurring campaign will ever create. Such an enrolment is
-- anchored to campaigns.event_start_at, unchanged.
--
-- It is written once, when the enrolment is created or restarted, and is not
-- recomputed by the scheduler. That is what makes reconciliation stable: the
-- desired send time for a step must not move underneath a queue that is
-- already draining, and a sweep that recomputed "today's occurrence" on every
-- tick would do exactly that at midnight.
ALTER TABLE campaign_contacts ADD COLUMN occurrence_at TEXT;

-- No new index. Occurrences are reached through campaign_contacts_campaign_idx
-- (campaign_id, status), which the reconciler and the admin rebase already use,
-- and an index that is never the chosen plan still costs every write.
