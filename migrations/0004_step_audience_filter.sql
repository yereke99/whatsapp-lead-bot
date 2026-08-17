-- Per-step audience cutoff: send a message only to contacts who entered the
-- campaign at or after a given moment.
--
-- The case this exists for is the last message before a webinar. "Болашақ
-- кәсіпкерлер жиналып жатыр" at 20:55 reads as a welcome to someone who signed
-- up two minutes ago and as a repetition to someone who has been receiving
-- reminders since 16:00. The operator wants that one message to reach only the
-- late arrivals, without splitting the campaign in two.
--
-- Both columns are additive and default to the existing behaviour, so every
-- campaign, step and queued message already in the database keeps working
-- exactly as before. The feature does nothing at all until an operator ticks
-- the box on a specific step.
--
-- Rollback (this project applies migrations forward only; run by hand if ever
-- needed, after stopping the service):
--
--   ALTER TABLE campaign_steps DROP COLUMN audience_filter_enabled;
--   ALTER TABLE campaign_steps DROP COLUMN audience_min_joined_at;

-- Off for every existing row, which is what makes this opt-in: a step that
-- nobody has configured behaves precisely as it did before the column existed.
ALTER TABLE campaign_steps ADD COLUMN audience_filter_enabled INTEGER NOT NULL DEFAULT 0;

-- The cutoff, stored as a canonical UTC timestamp in the same fixed-width form
-- as every other timestamp in the schema, so it compares correctly as text and
-- carries no timezone ambiguity. The admin panel collects a local date, time
-- and zone and converts before saving, exactly as it already does for a step's
-- own send time.
--
-- NULL means "no cutoff set". The column stays nullable rather than defaulting
-- to an epoch so that switching the filter off and on again does not silently
-- resurrect a stale boundary, and so a half-configured step is distinguishable
-- from one deliberately set to the beginning of time.
ALTER TABLE campaign_steps ADD COLUMN audience_min_joined_at TEXT;

-- Eligibility is decided against campaign_contacts.enrolled_at, the moment the
-- contact's enrolment was created. That column already exists and already
-- carries the right meaning: it is written once when the trigger matches and is
-- never touched by later messages, so a contact who keeps chatting does not
-- drift across the cutoff.
--
-- The existing campaign_contacts_enrolled_idx (enrolled_at DESC) and
-- campaign_contacts_campaign_idx (campaign_id, status) already cover how the
-- scheduler reaches enrolments, and the reconciler filters in memory from
-- enrolments it has already loaded for other reasons. No new index is added
-- here: one that is never used still costs every write.
