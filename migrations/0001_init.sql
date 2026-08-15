-- Complete schema: administrators, media, campaigns, contacts, conversations,
-- the persistent job queue and raw provider events.
--
-- Conventions used throughout:
--   * ids are TEXT holding a uuid, defaulted with gen_random_uuid();
--   * timestamps are TEXT in fixed-width UTC ISO-8601, which sorts
--     chronologically as plain text, defaulted with now();
--   * booleans are INTEGER 0/1;
--   * json is TEXT, guarded with json_valid() so a malformed write fails at
--     the point it happens rather than at the point it is read.
--
-- now() and gen_random_uuid() are registered by internal/storage/sqlite.

-- ----------------------------------------------------------------- core --

CREATE TABLE admins (
    id            TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    email         TEXT    NOT NULL,
    name          TEXT    NOT NULL DEFAULT '',
    password_hash TEXT    NOT NULL,
    role          TEXT    NOT NULL DEFAULT 'ADMIN',
    is_active     INTEGER NOT NULL DEFAULT 1,
    last_login_at TEXT,
    created_at    TEXT    NOT NULL DEFAULT (now()),
    updated_at    TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT admins_role_check CHECK (role IN ('OWNER', 'ADMIN', 'VIEWER')),
    CONSTRAINT admins_email_not_blank CHECK (length(trim(email)) > 0)
);

CREATE UNIQUE INDEX admins_email_key ON admins (lower(email));

CREATE TRIGGER admins_set_updated_at
AFTER UPDATE ON admins FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE admins SET updated_at = now() WHERE id = NEW.id;
END;

CREATE TABLE admin_sessions (
    id           TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    admin_id     TEXT NOT NULL REFERENCES admins (id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    csrf_token   TEXT NOT NULL,
    ip_address   TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    expires_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL DEFAULT (now()),
    revoked_at   TEXT,
    created_at   TEXT NOT NULL DEFAULT (now())
);

CREATE INDEX admin_sessions_admin_id_idx ON admin_sessions (admin_id);
CREATE INDEX admin_sessions_expires_at_idx ON admin_sessions (expires_at);

-- Brute-force protection ledger. Rows are pruned by a background job.
CREATE TABLE login_attempts (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    email      TEXT    NOT NULL DEFAULT '',
    ip_address TEXT    NOT NULL DEFAULT '',
    success    INTEGER NOT NULL,
    created_at TEXT    NOT NULL DEFAULT (now())
);

CREATE INDEX login_attempts_email_idx ON login_attempts (lower(email), created_at DESC);
CREATE INDEX login_attempts_ip_idx ON login_attempts (ip_address, created_at DESC);

CREATE TABLE audit_logs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    admin_id    TEXT REFERENCES admins (id) ON DELETE SET NULL,
    admin_email TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    entity_type TEXT NOT NULL DEFAULT '',
    entity_id   TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL DEFAULT '',
    old_values  TEXT,
    new_values  TEXT,
    ip_address  TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (now()),
    CONSTRAINT audit_logs_old_values_json CHECK (old_values IS NULL OR json_valid(old_values)),
    CONSTRAINT audit_logs_new_values_json CHECK (new_values IS NULL OR json_valid(new_values))
);

CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_admin_idx ON audit_logs (admin_id, created_at DESC);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity_type, entity_id, created_at DESC);

CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (now()),
    updated_by TEXT REFERENCES admins (id) ON DELETE SET NULL,
    CONSTRAINT app_settings_value_json CHECK (json_valid(value))
);

CREATE TABLE media_files (
    id                   TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    original_name        TEXT    NOT NULL,
    stored_name          TEXT    NOT NULL,
    relative_path        TEXT    NOT NULL UNIQUE,
    mime_type            TEXT    NOT NULL,
    size_bytes           INTEGER NOT NULL,
    kind                 TEXT    NOT NULL,
    checksum_sha256      TEXT    NOT NULL DEFAULT '',
    duration_ms          INTEGER,
    width                INTEGER,
    height               INTEGER,
    -- Set when this row is a derived artifact, e.g. an MP3 transcoded to
    -- OGG/Opus so WhatsApp renders it as a voice note.
    source_media_file_id TEXT REFERENCES media_files (id) ON DELETE SET NULL,
    uploaded_by          TEXT REFERENCES admins (id) ON DELETE SET NULL,
    created_at           TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT media_files_kind_check CHECK (kind IN ('IMAGE', 'VIDEO', 'AUDIO', 'VOICE', 'DOCUMENT')),
    CONSTRAINT media_files_size_check CHECK (size_bytes > 0)
);

CREATE INDEX media_files_kind_idx ON media_files (kind, created_at DESC);
CREATE INDEX media_files_checksum_idx ON media_files (checksum_sha256);

-- ------------------------------------------------------------ campaigns --

CREATE TABLE message_templates (
    id            TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    name          TEXT    NOT NULL,
    description   TEXT    NOT NULL DEFAULT '',
    type          TEXT    NOT NULL,
    body          TEXT    NOT NULL DEFAULT '',
    media_file_id TEXT REFERENCES media_files (id) ON DELETE RESTRICT,
    file_name     TEXT    NOT NULL DEFAULT '',
    link_preview  INTEGER NOT NULL DEFAULT 1,
    version       INTEGER NOT NULL DEFAULT 1,
    archived_at   TEXT,
    created_by    TEXT REFERENCES admins (id) ON DELETE SET NULL,
    created_at    TEXT    NOT NULL DEFAULT (now()),
    updated_at    TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT message_templates_type_check CHECK (type IN (
        'TEXT', 'IMAGE', 'IMAGE_WITH_CAPTION', 'VIDEO', 'VIDEO_WITH_CAPTION',
        'AUDIO', 'VOICE', 'DOCUMENT'
    )),
    CONSTRAINT message_templates_name_not_blank CHECK (length(trim(name)) > 0),
    -- Text-only messages need a body; every media type needs a file.
    CONSTRAINT message_templates_payload_check CHECK (
        (type = 'TEXT' AND length(trim(body)) > 0 AND media_file_id IS NULL)
        OR (type <> 'TEXT' AND media_file_id IS NOT NULL)
    ),
    -- Captions belong to the *_WITH_CAPTION variants only.
    CONSTRAINT message_templates_caption_check CHECK (
        type NOT IN ('VOICE', 'AUDIO', 'IMAGE', 'VIDEO') OR length(trim(body)) = 0
    )
);

CREATE UNIQUE INDEX message_templates_name_key
    ON message_templates (lower(trim(name))) WHERE archived_at IS NULL;
CREATE INDEX message_templates_type_idx ON message_templates (type);
CREATE INDEX message_templates_media_idx ON message_templates (media_file_id);

CREATE TRIGGER message_templates_set_updated_at
AFTER UPDATE ON message_templates FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE message_templates SET updated_at = now() WHERE id = NEW.id;
END;

-- Immutable history of every template revision. Sent messages record the
-- version they rendered from, so an edit never rewrites the past.
CREATE TABLE message_template_versions (
    id            TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    template_id   TEXT    NOT NULL REFERENCES message_templates (id) ON DELETE CASCADE,
    version       INTEGER NOT NULL,
    name          TEXT    NOT NULL,
    type          TEXT    NOT NULL,
    body          TEXT    NOT NULL DEFAULT '',
    media_file_id TEXT REFERENCES media_files (id) ON DELETE SET NULL,
    file_name     TEXT    NOT NULL DEFAULT '',
    created_by    TEXT REFERENCES admins (id) ON DELETE SET NULL,
    created_at    TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT message_template_versions_unique UNIQUE (template_id, version)
);

CREATE TABLE campaigns (
    id                           TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    name                         TEXT    NOT NULL,
    description                  TEXT    NOT NULL DEFAULT '',
    event_type                   TEXT    NOT NULL DEFAULT 'WEBINAR',
    event_start_at               TEXT,
    timezone                     TEXT    NOT NULL DEFAULT 'Asia/Almaty',
    webinar_link                 TEXT    NOT NULL DEFAULT '',
    status                       TEXT    NOT NULL DEFAULT 'DRAFT',
    -- What happens when an already-enrolled contact sends the trigger again.
    existing_contact_behavior    TEXT    NOT NULL DEFAULT 'IGNORE',
    existing_contact_template_id TEXT REFERENCES message_templates (id) ON DELETE SET NULL,
    unsubscribe_keywords         TEXT    NOT NULL DEFAULT '["STOP","ТОҚТАТУ","СТОП"]',
    -- Steps whose scheduled time has already passed at enrollment: send the
    -- most recent one immediately, or skip everything in the past.
    catch_up_missed_steps        INTEGER NOT NULL DEFAULT 1,
    max_send_attempts            INTEGER NOT NULL DEFAULT 5,
    archived_at                  TEXT,
    created_by                   TEXT REFERENCES admins (id) ON DELETE SET NULL,
    created_at                   TEXT    NOT NULL DEFAULT (now()),
    updated_at                   TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT campaigns_status_check CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'COMPLETED', 'ARCHIVED')),
    CONSTRAINT campaigns_event_type_check CHECK (event_type IN ('WEBINAR', 'EVENT', 'PRODUCT_LAUNCH', 'CUSTOM')),
    CONSTRAINT campaigns_behavior_check CHECK (existing_contact_behavior IN ('IGNORE', 'RESTART', 'CONTINUE', 'SPECIAL_MESSAGE')),
    CONSTRAINT campaigns_name_not_blank CHECK (length(trim(name)) > 0),
    CONSTRAINT campaigns_max_attempts_check CHECK (max_send_attempts BETWEEN 1 AND 20),
    CONSTRAINT campaigns_keywords_json CHECK (json_valid(unsubscribe_keywords)),
    -- A campaign cannot go live without an event anchor to schedule against.
    CONSTRAINT campaigns_active_needs_event CHECK (status <> 'ACTIVE' OR event_start_at IS NOT NULL)
);

CREATE UNIQUE INDEX campaigns_name_key ON campaigns (lower(trim(name))) WHERE archived_at IS NULL;
CREATE INDEX campaigns_status_idx ON campaigns (status);
CREATE INDEX campaigns_event_start_idx ON campaigns (event_start_at);

CREATE TRIGGER campaigns_set_updated_at
AFTER UPDATE ON campaigns FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE campaigns SET updated_at = now() WHERE id = NEW.id;
END;

CREATE TABLE campaign_triggers (
    id                 TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    campaign_id        TEXT    NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    keyword            TEXT    NOT NULL,
    normalized_keyword TEXT    NOT NULL,
    match_mode         TEXT    NOT NULL DEFAULT 'EXACT',
    is_active          INTEGER NOT NULL DEFAULT 1,
    created_at         TEXT    NOT NULL DEFAULT (now()),
    updated_at         TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT campaign_triggers_mode_check CHECK (match_mode IN ('EXACT', 'CONTAINS', 'STARTS_WITH')),
    CONSTRAINT campaign_triggers_keyword_not_blank CHECK (length(trim(normalized_keyword)) > 0)
);

-- One normalized keyword may only resolve to a single campaign at a time,
-- otherwise trigger routing would be ambiguous.
CREATE UNIQUE INDEX campaign_triggers_unique_active
    ON campaign_triggers (normalized_keyword, match_mode) WHERE is_active;
CREATE INDEX campaign_triggers_campaign_idx ON campaign_triggers (campaign_id);

CREATE TRIGGER campaign_triggers_set_updated_at
AFTER UPDATE ON campaign_triggers FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE campaign_triggers SET updated_at = now() WHERE id = NEW.id;
END;

CREATE TABLE campaign_steps (
    id                  TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    campaign_id         TEXT    NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    name                TEXT    NOT NULL DEFAULT '',
    -- Signed offset from campaign.event_start_at. Seconds keep fractional
    -- minute steps such as -7.5 minutes exact.
    offset_seconds      INTEGER NOT NULL,
    message_template_id TEXT    NOT NULL REFERENCES message_templates (id) ON DELETE RESTRICT,
    enabled             INTEGER NOT NULL DEFAULT 1,
    order_index         INTEGER NOT NULL,
    -- ON_TRIGGER steps fire the moment a contact enrolls, ignoring the event
    -- anchor. Everything else is scheduled from the event start.
    schedule_kind       TEXT    NOT NULL DEFAULT 'RELATIVE_TO_EVENT',
    created_at          TEXT    NOT NULL DEFAULT (now()),
    updated_at          TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT campaign_steps_kind_check CHECK (schedule_kind IN ('RELATIVE_TO_EVENT', 'ON_TRIGGER')),
    CONSTRAINT campaign_steps_offset_range CHECK (offset_seconds BETWEEN -31536000 AND 31536000),
    -- SQLite has no deferred constraints, so a reorder cannot pass through a
    -- colliding intermediate state. ReorderSteps renumbers in two phases via
    -- the negative range instead.
    CONSTRAINT campaign_steps_order_key UNIQUE (campaign_id, order_index)
);

CREATE INDEX campaign_steps_campaign_id_idx ON campaign_steps (campaign_id, order_index);
CREATE INDEX campaign_steps_template_idx ON campaign_steps (message_template_id);

CREATE TRIGGER campaign_steps_set_updated_at
AFTER UPDATE ON campaign_steps FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE campaign_steps SET updated_at = now() WHERE id = NEW.id;
END;

-- ------------------------------------------------------------- contacts --

CREATE TABLE contacts (
    id                     TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    phone                  TEXT    NOT NULL,
    chat_id                TEXT    NOT NULL,
    name                   TEXT    NOT NULL DEFAULT '',
    push_name              TEXT    NOT NULL DEFAULT '',
    source                 TEXT    NOT NULL DEFAULT 'WHATSAPP_TRIGGER',
    first_trigger_keyword  TEXT    NOT NULL DEFAULT '',
    first_campaign_id      TEXT REFERENCES campaigns (id) ON DELETE SET NULL,
    status                 TEXT    NOT NULL DEFAULT 'NEW',
    opted_out              INTEGER NOT NULL DEFAULT 0,
    opted_out_at           TEXT,
    blocked_at             TEXT,
    -- Consent anchor: set on the first inbound message. Nothing may be sent
    -- to a contact whose value is NULL.
    first_contact_at       TEXT,
    last_incoming_at       TEXT,
    last_outgoing_at       TEXT,
    last_activity_at       TEXT,
    incoming_count         INTEGER NOT NULL DEFAULT 0,
    outgoing_count         INTEGER NOT NULL DEFAULT 0,
    notes                  TEXT    NOT NULL DEFAULT '',
    avatar_url             TEXT    NOT NULL DEFAULT '',
    avatar_source_url      TEXT    NOT NULL DEFAULT '',
    avatar_checked_at      TEXT,
    unread_count           INTEGER NOT NULL DEFAULT 0,
    -- Denormalized so the chat list renders from a single indexed scan
    -- instead of a lateral join over the whole message history.
    last_message_preview   TEXT    NOT NULL DEFAULT '',
    last_message_type      TEXT    NOT NULL DEFAULT '',
    last_message_direction TEXT    NOT NULL DEFAULT '',
    created_at             TEXT    NOT NULL DEFAULT (now()),
    updated_at             TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT contacts_status_check CHECK (status IN ('NEW', 'ACTIVE', 'COMPLETED', 'UNSUBSCRIBED', 'BLOCKED', 'ERROR')),
    -- SQLite has no regex; this is the exact equivalent of ^[0-9]{6,20}$.
    CONSTRAINT contacts_phone_format CHECK (
        length(phone) BETWEEN 6 AND 20 AND phone NOT GLOB '*[^0-9]*'
    ),
    CONSTRAINT contacts_unread_check CHECK (unread_count >= 0)
);

CREATE UNIQUE INDEX contacts_phone_key ON contacts (phone);
CREATE UNIQUE INDEX contacts_chat_id_key ON contacts (chat_id);
CREATE INDEX contacts_created_at_idx ON contacts (created_at DESC);
CREATE INDEX contacts_status_idx ON contacts (status);
CREATE INDEX contacts_last_activity_idx ON contacts (last_activity_at DESC);
CREATE INDEX contacts_first_campaign_idx ON contacts (first_campaign_id);
CREATE INDEX contacts_name_search_idx ON contacts (lower(name));
-- The chat list ordering: most recently active conversation first.
CREATE INDEX contacts_chat_list_idx ON contacts (last_activity_at DESC, created_at DESC);

CREATE TRIGGER contacts_set_updated_at
AFTER UPDATE ON contacts FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE contacts SET updated_at = now() WHERE id = NEW.id;
END;

CREATE TABLE tags (
    id         TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    name       TEXT NOT NULL,
    color      TEXT NOT NULL DEFAULT '#4b5563',
    created_at TEXT NOT NULL DEFAULT (now()),
    CONSTRAINT tags_name_not_blank CHECK (length(trim(name)) > 0)
);

CREATE UNIQUE INDEX tags_name_key ON tags (lower(trim(name)));

CREATE TABLE contact_tags (
    contact_id TEXT NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    tag_id     TEXT NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (now()),
    PRIMARY KEY (contact_id, tag_id)
);

CREATE INDEX contact_tags_tag_idx ON contact_tags (tag_id);

-- A contact's participation in one campaign. run_number increments when the
-- admin has configured RESTART behaviour and the contact triggers again.
CREATE TABLE campaign_contacts (
    id              TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    campaign_id     TEXT    NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id      TEXT    NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    trigger_id      TEXT REFERENCES campaign_triggers (id) ON DELETE SET NULL,
    trigger_keyword TEXT    NOT NULL DEFAULT '',
    status          TEXT    NOT NULL DEFAULT 'ACTIVE',
    run_number      INTEGER NOT NULL DEFAULT 1,
    restart_count   INTEGER NOT NULL DEFAULT 0,
    enrolled_at     TEXT    NOT NULL DEFAULT (now()),
    completed_at    TEXT,
    cancelled_at    TEXT,
    cancel_reason   TEXT    NOT NULL DEFAULT '',
    created_at      TEXT    NOT NULL DEFAULT (now()),
    updated_at      TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT campaign_contacts_status_check CHECK (status IN ('ACTIVE', 'COMPLETED', 'CANCELLED', 'UNSUBSCRIBED')),
    CONSTRAINT campaign_contacts_run_check CHECK (run_number >= 1),
    CONSTRAINT campaign_contacts_unique UNIQUE (campaign_id, contact_id)
);

CREATE INDEX campaign_contacts_campaign_idx ON campaign_contacts (campaign_id, status);
CREATE INDEX campaign_contacts_contact_idx ON campaign_contacts (contact_id);
CREATE INDEX campaign_contacts_enrolled_idx ON campaign_contacts (enrolled_at DESC);

CREATE TRIGGER campaign_contacts_set_updated_at
AFTER UPDATE ON campaign_contacts FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE campaign_contacts SET updated_at = now() WHERE id = NEW.id;
END;

-- ------------------------------------------------------------ messaging --

CREATE TABLE messages (
    id                   TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    contact_id           TEXT    NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    campaign_id          TEXT REFERENCES campaigns (id) ON DELETE SET NULL,
    enrollment_id        TEXT REFERENCES campaign_contacts (id) ON DELETE SET NULL,
    campaign_step_id     TEXT REFERENCES campaign_steps (id) ON DELETE SET NULL,
    -- Forward reference; SQLite resolves foreign key targets at write time.
    scheduled_message_id TEXT REFERENCES scheduled_messages (id) ON DELETE SET NULL,
    direction            TEXT    NOT NULL,
    type                 TEXT    NOT NULL,
    text                 TEXT    NOT NULL DEFAULT '',
    media_file_id        TEXT REFERENCES media_files (id) ON DELETE SET NULL,
    media_url            TEXT    NOT NULL DEFAULT '',
    file_name            TEXT    NOT NULL DEFAULT '',
    mime_type            TEXT    NOT NULL DEFAULT '',
    external_id          TEXT,
    status               TEXT    NOT NULL DEFAULT 'PENDING',
    error                TEXT    NOT NULL DEFAULT '',
    is_manual            INTEGER NOT NULL DEFAULT 0,
    sent_by_admin_id     TEXT REFERENCES admins (id) ON DELETE SET NULL,
    template_id          TEXT REFERENCES message_templates (id) ON DELETE SET NULL,
    template_version     INTEGER,
    metadata             TEXT    NOT NULL DEFAULT '{}',
    media_download_status TEXT   NOT NULL DEFAULT 'NONE',
    media_download_error  TEXT   NOT NULL DEFAULT '',
    sent_at              TEXT,
    delivered_at         TEXT,
    read_at              TEXT,
    created_at           TEXT    NOT NULL DEFAULT (now()),
    updated_at           TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT messages_direction_check CHECK (direction IN ('INCOMING', 'OUTGOING')),
    CONSTRAINT messages_type_check CHECK (type IN (
        'TEXT', 'IMAGE', 'AUDIO', 'VOICE', 'VIDEO', 'DOCUMENT', 'STICKER',
        'LOCATION', 'CONTACT', 'POLL', 'REACTION', 'UNKNOWN'
    )),
    CONSTRAINT messages_status_check CHECK (status IN (
        'PENDING', 'SENT', 'DELIVERED', 'READ', 'FAILED', 'RECEIVED'
    )),
    CONSTRAINT messages_metadata_json CHECK (json_valid(metadata)),
    CONSTRAINT messages_media_download_check
        CHECK (media_download_status IN ('NONE', 'PENDING', 'DONE', 'FAILED', 'SKIPPED'))
);

-- Provider message ids are globally unique; this index is the deduplication
-- point for both outbound echoes and inbound webhook replays.
CREATE UNIQUE INDEX messages_external_id_key ON messages (external_id) WHERE external_id IS NOT NULL;
CREATE INDEX messages_contact_created_idx ON messages (contact_id, created_at DESC);
CREATE INDEX messages_created_at_idx ON messages (created_at DESC);
CREATE INDEX messages_campaign_idx ON messages (campaign_id, created_at DESC);
CREATE INDEX messages_direction_created_idx ON messages (direction, created_at DESC);
CREATE INDEX messages_status_idx ON messages (status) WHERE direction = 'OUTGOING';
-- Worker queue for pulling inbound attachments into local storage before the
-- provider's temporary download url expires.
CREATE INDEX messages_media_pending_idx
    ON messages (created_at) WHERE media_download_status = 'PENDING';

CREATE TRIGGER messages_set_updated_at
AFTER UPDATE ON messages FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE messages SET updated_at = now() WHERE id = NEW.id;
END;

CREATE TABLE scheduled_messages (
    id               TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    campaign_id      TEXT    NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id       TEXT    NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    enrollment_id    TEXT    NOT NULL REFERENCES campaign_contacts (id) ON DELETE CASCADE,
    campaign_step_id TEXT    NOT NULL REFERENCES campaign_steps (id) ON DELETE CASCADE,
    run_number       INTEGER NOT NULL DEFAULT 1,
    scheduled_at     TEXT    NOT NULL,
    status           TEXT    NOT NULL DEFAULT 'PENDING',
    attempt_count    INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  TEXT,
    locked_by        TEXT,
    locked_at        TEXT,
    sent_at          TEXT,
    cancelled_at     TEXT,
    cancel_reason    TEXT    NOT NULL DEFAULT '',
    last_error       TEXT    NOT NULL DEFAULT '',
    message_id       TEXT REFERENCES messages (id) ON DELETE SET NULL,
    created_at       TEXT    NOT NULL DEFAULT (now()),
    updated_at       TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT scheduled_messages_status_check CHECK (status IN (
        'PENDING', 'PROCESSING', 'SENT', 'FAILED', 'CANCELLED'
    )),
    -- The hard anti-duplicate guarantee: one delivery per step, per contact,
    -- per campaign run.
    CONSTRAINT scheduled_messages_unique_step UNIQUE (enrollment_id, campaign_step_id, run_number)
);

-- The scheduler's hot path: pending jobs that are due, oldest first.
CREATE INDEX scheduled_messages_due_idx
    ON scheduled_messages (scheduled_at) WHERE status = 'PENDING';
CREATE INDEX scheduled_messages_status_idx ON scheduled_messages (status, scheduled_at);
CREATE INDEX scheduled_messages_campaign_idx ON scheduled_messages (campaign_id, status);
CREATE INDEX scheduled_messages_contact_idx ON scheduled_messages (contact_id, scheduled_at);
CREATE INDEX scheduled_messages_stuck_idx ON scheduled_messages (locked_at) WHERE status = 'PROCESSING';

CREATE TRIGGER scheduled_messages_set_updated_at
AFTER UPDATE ON scheduled_messages FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE scheduled_messages SET updated_at = now() WHERE id = NEW.id;
END;

CREATE TABLE webhook_events (
    id           TEXT PRIMARY KEY DEFAULT (gen_random_uuid()),
    provider     TEXT    NOT NULL DEFAULT 'greenapi',
    event_type   TEXT    NOT NULL DEFAULT '',
    -- Stable identity of the delivered event, used to reject replays.
    dedupe_key   TEXT    NOT NULL,
    external_id  TEXT    NOT NULL DEFAULT '',
    payload      TEXT    NOT NULL,
    status       TEXT    NOT NULL DEFAULT 'RECEIVED',
    attempts     INTEGER NOT NULL DEFAULT 0,
    error        TEXT    NOT NULL DEFAULT '',
    received_at  TEXT    NOT NULL DEFAULT (now()),
    processed_at TEXT,
    created_at   TEXT    NOT NULL DEFAULT (now()),
    updated_at   TEXT    NOT NULL DEFAULT (now()),
    CONSTRAINT webhook_events_status_check CHECK (status IN (
        'RECEIVED', 'PROCESSING', 'PROCESSED', 'FAILED', 'IGNORED'
    )),
    CONSTRAINT webhook_events_payload_json CHECK (json_valid(payload)),
    CONSTRAINT webhook_events_provider_dedupe_key UNIQUE (provider, dedupe_key)
);

CREATE INDEX webhook_events_pending_idx
    ON webhook_events (received_at) WHERE status IN ('RECEIVED', 'PROCESSING');
CREATE INDEX webhook_events_type_idx ON webhook_events (event_type, received_at DESC);
CREATE INDEX webhook_events_status_idx ON webhook_events (status, received_at DESC);

CREATE TRIGGER webhook_events_set_updated_at
AFTER UPDATE ON webhook_events FOR EACH ROW
WHEN NEW.updated_at IS OLD.updated_at
BEGIN
    UPDATE webhook_events SET updated_at = now() WHERE id = NEW.id;
END;
