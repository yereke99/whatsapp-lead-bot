-- Campaign message queue: per-step trigger delays, a pause/resume policy,
-- pinned template versions and per-campaign send limits.
--
-- Everything here is additive. No existing column changes meaning, no table is
-- rebuilt, and every new column carries a default that reproduces the previous
-- behaviour, so an existing database keeps running exactly as before until an
-- operator changes a setting.
--
-- Rollback (this project applies migrations forward only; run by hand if ever
-- needed, after stopping the service):
--
--   ALTER TABLE campaigns          DROP COLUMN resume_policy;
--   ALTER TABLE campaigns          DROP COLUMN pin_template_version;
--   ALTER TABLE campaigns          DROP COLUMN max_messages_per_hour;
--   ALTER TABLE campaigns          DROP COLUMN max_messages_per_day;
--   ALTER TABLE campaigns          DROP COLUMN max_active_contacts;
--   ALTER TABLE scheduled_messages DROP COLUMN template_version;
--   DROP INDEX scheduled_messages_contact_status_idx;
--   DROP INDEX scheduled_messages_campaign_sent_idx;

-- What happens to jobs whose time passed while the campaign was paused.
--
--   SKIP_EXPIRED    cancel them; resuming at 20:00 does not dump the 18:00 and
--                   19:00 messages on the contact at once.
--   SEND_NEXT_VALID cancel all but the most recent expired step per contact,
--                   which is sent immediately so the contact still gets context.
--
-- The default matches the safer behaviour requested for production.
ALTER TABLE campaigns ADD COLUMN resume_policy TEXT NOT NULL DEFAULT 'SKIP_EXPIRED';

-- When 0 (the default, and the behaviour the platform has always had) a queued
-- job renders from the template as it stands at send time, so editing a
-- template updates every message not yet sent. When 1, each job renders the
-- template version that was current when the job was queued, and an edit only
-- affects contacts who enrol afterwards.
ALTER TABLE campaigns ADD COLUMN pin_template_version INTEGER NOT NULL DEFAULT 0;

-- Optional safety caps. NULL means no limit. When a cap is reached the queue
-- holds -- jobs are deferred, never dropped -- and the panel shows a warning.
ALTER TABLE campaigns ADD COLUMN max_messages_per_hour INTEGER;
ALTER TABLE campaigns ADD COLUMN max_messages_per_day  INTEGER;
ALTER TABLE campaigns ADD COLUMN max_active_contacts   INTEGER;

-- The template revision that was current when this job was queued. Recorded
-- for every job so the panel can show which version a contact will receive;
-- only used for rendering when the campaign pins versions.
ALTER TABLE scheduled_messages ADD COLUMN template_version INTEGER;

-- The claim query serialises delivery per contact: it skips a contact who
-- already has a job in flight, and skips a job that has an earlier sibling
-- still waiting. Both are lookups by (contact_id, status).
CREATE INDEX scheduled_messages_contact_status_idx
    ON scheduled_messages (contact_id, status, scheduled_at);

-- Hourly and daily send counters for the per-campaign limits.
CREATE INDEX scheduled_messages_campaign_sent_idx
    ON scheduled_messages (campaign_id, sent_at) WHERE status = 'SENT';
