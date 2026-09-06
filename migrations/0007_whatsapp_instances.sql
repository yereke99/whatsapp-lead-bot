-- Multiple WhatsApp accounts in one installation.
--
-- The platform has always talked to exactly one Green API instance, and every
-- campaign, trigger, message and queued job in this database belongs to it.
-- This migration introduces the idea of a *logical WhatsApp account* and makes
-- every existing row the property of the account that has always owned it, so
-- that a second number can be added beside it without the first one noticing.
--
-- The shape of the change, in one line:
--
--   whatsapp_instances --< campaigns --< triggers, steps, enrolments, jobs
--
-- A campaign is the only thing that carries ownership. Everything downstream
-- of it -- its triggers, its steps, its enrolments, its scheduled messages,
-- its retries -- derives the account from the campaign it belongs to, so there
-- is exactly one place where the answer lives and no second copy that can
-- drift. Campaign ownership is immutable after creation, which is what makes
-- that derivation safe.
--
-- Everything here is additive. No table is dropped, no table is rebuilt, no
-- row is deleted, no existing column changes meaning, and every new column is
-- nullable with existing rows backfilled to the Primary account. An
-- installation that never configures a second account behaves exactly as it
-- does today, down to which messages go out at which second.
--
-- Credentials are deliberately absent. This table describes *ownership*; the
-- Green API instance id and token that back an account are supplied by the
-- environment at runtime and are never written to the database, never returned
-- by the API and never logged.
--
-- Rollback (this project applies migrations forward only; run by hand if ever
-- needed, after stopping the service):
--
--   DROP INDEX campaign_triggers_unique_active;
--   CREATE UNIQUE INDEX campaign_triggers_unique_active
--       ON campaign_triggers (normalized_keyword, match_mode) WHERE is_active;
--   DROP INDEX campaigns_instance_idx;
--   DROP INDEX messages_instance_idx;
--   DROP INDEX webhook_events_instance_idx;
--   ALTER TABLE campaigns         DROP COLUMN whatsapp_instance_id;
--   ALTER TABLE campaign_triggers DROP COLUMN whatsapp_instance_id;
--   ALTER TABLE messages          DROP COLUMN whatsapp_instance_id;
--   ALTER TABLE webhook_events    DROP COLUMN whatsapp_instance_id;
--   DROP TABLE contact_instance_optouts;
--   DROP TABLE whatsapp_instances;


-- ------------------------------------------------------ the account entity --

-- One row per WhatsApp number the installation talks through.
--
-- The id is a small integer rather than a uuid, unlike every other entity
-- here, because these rows are configuration rather than data: there are two
-- of them, they are referenced from the environment by key, and an operator
-- reading a log line benefits from "instance=2" over a uuid. instance_key is
-- the stable business identifier -- 'primary', 'secondary' -- and is what the
-- process matches its configured credentials against, so renaming the display
-- name in the panel can never repoint an account at the wrong number.
CREATE TABLE whatsapp_instances (
    id            INTEGER PRIMARY KEY,
    instance_key  TEXT    NOT NULL,
    name          TEXT    NOT NULL,
    -- A human label for the number, e.g. "+7 700 000 00 00 (Airan)". Safe to
    -- show in the panel. Never a credential.
    phone_label   TEXT    NOT NULL DEFAULT '',
    description   TEXT    NOT NULL DEFAULT '',
    -- A disabled account starts no poller and sends nothing new. Its
    -- campaigns, contacts, history and queued jobs stay exactly where they
    -- are: disabling is a pause, never a delete.
    enabled       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT    NOT NULL DEFAULT (now()),
    updated_at    TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT whatsapp_instances_key_not_blank CHECK (length(trim(instance_key)) > 0),
    CONSTRAINT whatsapp_instances_name_not_blank CHECK (length(trim(name)) > 0)
);

CREATE UNIQUE INDEX whatsapp_instances_key_idx ON whatsapp_instances (lower(trim(instance_key)));

CREATE TRIGGER whatsapp_instances_set_updated_at
AFTER UPDATE ON whatsapp_instances FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE whatsapp_instances SET updated_at = now() WHERE id = NEW.id;
END;

-- Bootstrap the two accounts.
--
-- INSERT ... SELECT ... WHERE NOT EXISTS rather than a bare INSERT so that
-- re-running this statement by hand, or restoring a database where an operator
-- created the rows already, cannot produce duplicates. The ids are fixed
-- because the environment refers to accounts positionally: the first block of
-- Green API credentials is Primary, the second is Secondary.
--
-- Secondary is created even though no credentials may be configured for it
-- yet. An account row is a place to hang campaigns, not a promise that a
-- number is connected; the process refuses to poll or send for an account
-- whose credentials are absent, and says so at startup.
INSERT INTO whatsapp_instances (id, instance_key, name, description)
SELECT 1, 'primary', 'Primary WhatsApp',
       'The number this installation has always used. Every campaign that existed before multi-account support belongs to it.'
WHERE NOT EXISTS (SELECT 1 FROM whatsapp_instances WHERE lower(trim(instance_key)) = 'primary');

INSERT INTO whatsapp_instances (id, instance_key, name, description)
SELECT 2, 'secondary', 'Secondary WhatsApp',
       'The second number. Its campaigns, triggers and schedules are created in the admin panel and are independent of Primary.'
WHERE NOT EXISTS (SELECT 1 FROM whatsapp_instances WHERE lower(trim(instance_key)) = 'secondary');


-- ----------------------------------------------------- campaign ownership --

-- The one column that decides which number a message leaves through.
--
-- Nullable, and read as Primary when null. That is not laziness: SQLite cannot
-- add a NOT NULL column to a populated table without a default, and a default
-- of 1 baked into the schema would silently reassign a row somebody creates by
-- hand later. Treating null as Primary in one documented place in the code is
-- honest about the same thing and keeps a hand-written row routable rather
-- than stranded.
ALTER TABLE campaigns ADD COLUMN whatsapp_instance_id INTEGER REFERENCES whatsapp_instances (id);

-- Every campaign that exists right now belongs to the number that has been
-- sending its messages all along. Ids, names, statuses, event times, links,
-- steps, templates, enrolments, queued jobs and delivery history are all
-- untouched: this writes one previously-empty column and nothing else.
UPDATE campaigns SET whatsapp_instance_id = 1 WHERE whatsapp_instance_id IS NULL;

CREATE INDEX campaigns_instance_idx ON campaigns (whatsapp_instance_id, status);


-- ------------------------------------------------------ trigger ownership --

-- A trigger's account is a copy of its campaign's, which is the one piece of
-- denormalisation in this design. It exists because the uniqueness rule below
-- has to be enforced by an index, and a partial index cannot reach through a
-- foreign key to read the campaign's column.
--
-- It cannot drift: a campaign's account is immutable once the campaign exists,
-- and the trigger's copy is written from the campaign at insert time.
ALTER TABLE campaign_triggers ADD COLUMN whatsapp_instance_id INTEGER REFERENCES whatsapp_instances (id);

UPDATE campaign_triggers
SET whatsapp_instance_id = COALESCE(
        (SELECT c.whatsapp_instance_id FROM campaigns c WHERE c.id = campaign_triggers.campaign_id),
        1)
WHERE whatsapp_instance_id IS NULL;

-- The routing rule, rescoped.
--
-- It used to read: one normalized keyword resolves to a single campaign,
-- anywhere in the installation. With two numbers that is too strong -- the
-- word a customer sends only ever reaches the number they sent it to, so
-- "START" on Primary and "START" on Secondary are two different, unambiguous
-- instructions and both must be allowed.
--
-- Dropping and recreating an index rewrites no table data and touches no row:
-- the rows are read to rebuild the b-tree and written back nowhere. The old
-- index is recreated verbatim in the rollback note at the top of this file.
DROP INDEX campaign_triggers_unique_active;

CREATE UNIQUE INDEX campaign_triggers_unique_active
    ON campaign_triggers (whatsapp_instance_id, normalized_keyword, match_mode) WHERE is_active;

CREATE INDEX campaign_triggers_instance_idx ON campaign_triggers (whatsapp_instance_id) WHERE is_active;


-- ------------------------------------------------- conversation ownership --

-- Which number a message went through. Not used for routing -- a queued job
-- resolves its account from its campaign -- but it is what lets the admin
-- conversation view say which of two numbers a customer was talking to, and
-- what makes a message traceable to an account after the fact.
ALTER TABLE messages ADD COLUMN whatsapp_instance_id INTEGER REFERENCES whatsapp_instances (id);

UPDATE messages SET whatsapp_instance_id = 1 WHERE whatsapp_instance_id IS NULL;

CREATE INDEX messages_instance_idx ON messages (whatsapp_instance_id, created_at DESC);

-- The account whose queue a provider notification came out of.
--
-- Deduplication of notifications is scoped by account through the dedupe_key
-- itself, which the process now namespaces with the account key before
-- storing. The existing UNIQUE (provider, dedupe_key) constraint therefore
-- becomes per-account without being touched, and a receipt handled on Primary
-- can no longer suppress an identical-looking one on Secondary. This column
-- carries the same fact in queryable form for the admin event log.
ALTER TABLE webhook_events ADD COLUMN whatsapp_instance_id INTEGER REFERENCES whatsapp_instances (id);

UPDATE webhook_events SET whatsapp_instance_id = 1 WHERE whatsapp_instance_id IS NULL;

CREATE INDEX webhook_events_instance_idx ON webhook_events (whatsapp_instance_id, received_at DESC);


-- ---------------------------------------------------- per-account opt-out --

-- Unsubscribing from one number does not unsubscribe from the other.
--
-- A contact is one person and stays one row in contacts: the same human with
-- the same phone number, who may write to both of the installation's numbers.
-- What is *not* shared is the instruction they gave. "STOP" sent to the
-- Secondary number is a statement about the Secondary number's campaigns; it
-- must not cancel the reminders they are waiting for from Primary.
--
-- Presence of a row means opted out. contacts.opted_out, contacts.opted_out_at
-- and contacts.status keep their existing meaning and keep being written
-- exactly as before, so the panel, its filters, the exports and the analytics
-- are unchanged -- they are the "this person asked us to stop somewhere" view.
-- This table is what the send path actually asks before it delivers anything.
--
-- An administrator unsubscribing someone from the panel is a statement about
-- the person rather than about one number, so it writes a row for every
-- account. That is what makes the two views agree for anybody who only ever
-- deals with one number.
CREATE TABLE contact_instance_optouts (
    whatsapp_instance_id INTEGER NOT NULL REFERENCES whatsapp_instances (id) ON DELETE CASCADE,
    contact_id           TEXT    NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    -- Free text, for the panel: which keyword they sent, or which administrator
    -- unsubscribed them.
    reason               TEXT    NOT NULL DEFAULT '',
    opted_out_at         TEXT    NOT NULL DEFAULT (now()),
    PRIMARY KEY (whatsapp_instance_id, contact_id)
);

CREATE INDEX contact_instance_optouts_contact_idx ON contact_instance_optouts (contact_id);

-- Everybody who has already unsubscribed stays unsubscribed, on the number
-- they unsubscribed from -- which, until this migration ran, was the only
-- number there was. This is what makes the new guard identical to the old one
-- on an installation with a single account.
INSERT INTO contact_instance_optouts (whatsapp_instance_id, contact_id, reason, opted_out_at)
SELECT 1, c.id, 'unsubscribed before multi-account support', COALESCE(c.opted_out_at, now())
FROM contacts c
WHERE c.opted_out = 1
  AND NOT EXISTS (
      SELECT 1 FROM contact_instance_optouts o
      WHERE o.whatsapp_instance_id = 1 AND o.contact_id = c.id
  );
