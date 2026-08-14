-- Campaign configuration: campaigns, triggers, reusable templates and the
-- ordered automation steps that make up a campaign's schedule.

CREATE TABLE message_templates (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        NOT NULL,
    description   text        NOT NULL DEFAULT '',
    type          text        NOT NULL,
    body          text        NOT NULL DEFAULT '',
    media_file_id uuid REFERENCES media_files (id) ON DELETE RESTRICT,
    file_name     text        NOT NULL DEFAULT '',
    link_preview  boolean     NOT NULL DEFAULT true,
    version       integer     NOT NULL DEFAULT 1,
    archived_at   timestamptz,
    created_by    uuid REFERENCES admins (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT message_templates_type_check CHECK (type IN (
        'TEXT', 'IMAGE', 'IMAGE_WITH_CAPTION', 'VIDEO', 'VIDEO_WITH_CAPTION',
        'AUDIO', 'VOICE', 'DOCUMENT'
    )),
    CONSTRAINT message_templates_name_not_blank CHECK (length(btrim(name)) > 0),
    -- Text-only messages need a body; every media type needs a file.
    CONSTRAINT message_templates_payload_check CHECK (
        (type = 'TEXT' AND length(btrim(body)) > 0 AND media_file_id IS NULL)
        OR (type <> 'TEXT' AND media_file_id IS NOT NULL)
    ),
    -- Captions belong to the *_WITH_CAPTION variants only.
    CONSTRAINT message_templates_caption_check CHECK (
        type NOT IN ('VOICE', 'AUDIO', 'IMAGE', 'VIDEO') OR length(btrim(body)) = 0
    )
);

CREATE UNIQUE INDEX message_templates_name_key
    ON message_templates (lower(btrim(name))) WHERE archived_at IS NULL;
CREATE INDEX message_templates_type_idx ON message_templates (type);
CREATE INDEX message_templates_media_idx ON message_templates (media_file_id);

CREATE TRIGGER message_templates_set_updated_at
    BEFORE UPDATE ON message_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Immutable history of every template revision. Sent messages record the
-- version they rendered from, so an edit never rewrites the past.
CREATE TABLE message_template_versions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id   uuid        NOT NULL REFERENCES message_templates (id) ON DELETE CASCADE,
    version       integer     NOT NULL,
    name          text        NOT NULL,
    type          text        NOT NULL,
    body          text        NOT NULL DEFAULT '',
    media_file_id uuid REFERENCES media_files (id) ON DELETE SET NULL,
    file_name     text        NOT NULL DEFAULT '',
    created_by    uuid REFERENCES admins (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (template_id, version)
);

CREATE TABLE campaigns (
    id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name                      text        NOT NULL,
    description               text        NOT NULL DEFAULT '',
    event_type                text        NOT NULL DEFAULT 'WEBINAR',
    event_start_at            timestamptz,
    timezone                  text        NOT NULL DEFAULT 'Asia/Almaty',
    webinar_link              text        NOT NULL DEFAULT '',
    status                    text        NOT NULL DEFAULT 'DRAFT',
    -- What happens when an already-enrolled contact sends the trigger again.
    existing_contact_behavior text        NOT NULL DEFAULT 'IGNORE',
    existing_contact_template_id uuid REFERENCES message_templates (id) ON DELETE SET NULL,
    unsubscribe_keywords      text[]      NOT NULL DEFAULT ARRAY['STOP', 'ТОҚТАТУ', 'СТОП']::text[],
    -- Steps whose scheduled time has already passed at enrollment: send the
    -- most recent one immediately, or skip everything in the past.
    catch_up_missed_steps     boolean     NOT NULL DEFAULT true,
    max_send_attempts         integer     NOT NULL DEFAULT 5,
    archived_at               timestamptz,
    created_by                uuid REFERENCES admins (id) ON DELETE SET NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT campaigns_status_check CHECK (status IN ('DRAFT', 'ACTIVE', 'PAUSED', 'COMPLETED', 'ARCHIVED')),
    CONSTRAINT campaigns_event_type_check CHECK (event_type IN ('WEBINAR', 'EVENT', 'PRODUCT_LAUNCH', 'CUSTOM')),
    CONSTRAINT campaigns_behavior_check CHECK (existing_contact_behavior IN ('IGNORE', 'RESTART', 'CONTINUE', 'SPECIAL_MESSAGE')),
    CONSTRAINT campaigns_name_not_blank CHECK (length(btrim(name)) > 0),
    CONSTRAINT campaigns_max_attempts_check CHECK (max_send_attempts BETWEEN 1 AND 20),
    -- A campaign cannot go live without an event anchor to schedule against.
    CONSTRAINT campaigns_active_needs_event CHECK (status <> 'ACTIVE' OR event_start_at IS NOT NULL)
);

CREATE UNIQUE INDEX campaigns_name_key ON campaigns (lower(btrim(name))) WHERE archived_at IS NULL;
CREATE INDEX campaigns_status_idx ON campaigns (status);
CREATE INDEX campaigns_event_start_idx ON campaigns (event_start_at);

CREATE TRIGGER campaigns_set_updated_at
    BEFORE UPDATE ON campaigns
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE campaign_triggers (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id        uuid        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    keyword            text        NOT NULL,
    normalized_keyword text        NOT NULL,
    match_mode         text        NOT NULL DEFAULT 'EXACT',
    is_active          boolean     NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT campaign_triggers_mode_check CHECK (match_mode IN ('EXACT', 'CONTAINS', 'STARTS_WITH')),
    CONSTRAINT campaign_triggers_keyword_not_blank CHECK (length(btrim(normalized_keyword)) > 0)
);

-- One normalized keyword may only resolve to a single campaign at a time,
-- otherwise trigger routing would be ambiguous.
CREATE UNIQUE INDEX campaign_triggers_unique_active
    ON campaign_triggers (normalized_keyword, match_mode) WHERE is_active;
CREATE INDEX campaign_triggers_campaign_idx ON campaign_triggers (campaign_id);

CREATE TRIGGER campaign_triggers_set_updated_at
    BEFORE UPDATE ON campaign_triggers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE campaign_steps (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id         uuid        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    name                text        NOT NULL DEFAULT '',
    -- Signed offset from campaign.event_start_at. Seconds keep fractional
    -- minute steps such as -7.5 minutes exact.
    offset_seconds      integer     NOT NULL,
    message_template_id uuid        NOT NULL REFERENCES message_templates (id) ON DELETE RESTRICT,
    enabled             boolean     NOT NULL DEFAULT true,
    order_index         integer     NOT NULL,
    -- ON_TRIGGER steps fire the moment a contact enrolls, ignoring the event
    -- anchor. Everything else is scheduled from the event start.
    schedule_kind       text        NOT NULL DEFAULT 'RELATIVE_TO_EVENT',
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT campaign_steps_kind_check CHECK (schedule_kind IN ('RELATIVE_TO_EVENT', 'ON_TRIGGER')),
    CONSTRAINT campaign_steps_offset_range CHECK (offset_seconds BETWEEN -31536000 AND 31536000)
);

-- Deferred so a reorder can shuffle indices inside one transaction.
ALTER TABLE campaign_steps
    ADD CONSTRAINT campaign_steps_order_key UNIQUE (campaign_id, order_index)
    DEFERRABLE INITIALLY DEFERRED;

CREATE INDEX campaign_steps_campaign_id_idx ON campaign_steps (campaign_id, order_index);
CREATE INDEX campaign_steps_template_idx ON campaign_steps (message_template_id);

CREATE TRIGGER campaign_steps_set_updated_at
    BEFORE UPDATE ON campaign_steps
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
