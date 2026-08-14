-- Conversation history, the persistent job queue and raw provider events.

CREATE TABLE messages (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contact_id          uuid        NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    campaign_id         uuid REFERENCES campaigns (id) ON DELETE SET NULL,
    enrollment_id       uuid REFERENCES campaign_contacts (id) ON DELETE SET NULL,
    campaign_step_id    uuid REFERENCES campaign_steps (id) ON DELETE SET NULL,
    scheduled_message_id uuid,
    direction           text        NOT NULL,
    type                text        NOT NULL,
    text                text        NOT NULL DEFAULT '',
    media_file_id       uuid REFERENCES media_files (id) ON DELETE SET NULL,
    media_url           text        NOT NULL DEFAULT '',
    file_name           text        NOT NULL DEFAULT '',
    mime_type           text        NOT NULL DEFAULT '',
    external_id         text,
    status              text        NOT NULL DEFAULT 'PENDING',
    error               text        NOT NULL DEFAULT '',
    is_manual           boolean     NOT NULL DEFAULT false,
    sent_by_admin_id    uuid REFERENCES admins (id) ON DELETE SET NULL,
    template_id         uuid REFERENCES message_templates (id) ON DELETE SET NULL,
    template_version    integer,
    metadata            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    sent_at             timestamptz,
    delivered_at        timestamptz,
    read_at             timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT messages_direction_check CHECK (direction IN ('INCOMING', 'OUTGOING')),
    CONSTRAINT messages_type_check CHECK (type IN (
        'TEXT', 'IMAGE', 'AUDIO', 'VOICE', 'VIDEO', 'DOCUMENT', 'STICKER',
        'LOCATION', 'CONTACT', 'POLL', 'REACTION', 'UNKNOWN'
    )),
    CONSTRAINT messages_status_check CHECK (status IN (
        'PENDING', 'SENT', 'DELIVERED', 'READ', 'FAILED', 'RECEIVED'
    ))
);

-- Provider message ids are globally unique; this index is the deduplication
-- point for both outbound echoes and inbound webhook replays.
CREATE UNIQUE INDEX messages_external_id_key ON messages (external_id) WHERE external_id IS NOT NULL;
CREATE INDEX messages_contact_created_idx ON messages (contact_id, created_at DESC);
CREATE INDEX messages_created_at_idx ON messages (created_at DESC);
CREATE INDEX messages_campaign_idx ON messages (campaign_id, created_at DESC);
CREATE INDEX messages_direction_created_idx ON messages (direction, created_at DESC);
CREATE INDEX messages_status_idx ON messages (status) WHERE direction = 'OUTGOING';

CREATE TRIGGER messages_set_updated_at
    BEFORE UPDATE ON messages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE scheduled_messages (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id      uuid        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id       uuid        NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    enrollment_id    uuid        NOT NULL REFERENCES campaign_contacts (id) ON DELETE CASCADE,
    campaign_step_id uuid        NOT NULL REFERENCES campaign_steps (id) ON DELETE CASCADE,
    run_number       integer     NOT NULL DEFAULT 1,
    scheduled_at     timestamptz NOT NULL,
    status           text        NOT NULL DEFAULT 'PENDING',
    attempt_count    integer     NOT NULL DEFAULT 0,
    next_attempt_at  timestamptz,
    locked_by        text,
    locked_at        timestamptz,
    sent_at          timestamptz,
    cancelled_at     timestamptz,
    cancel_reason    text        NOT NULL DEFAULT '',
    last_error       text        NOT NULL DEFAULT '',
    message_id       uuid REFERENCES messages (id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT scheduled_messages_status_check CHECK (status IN (
        'PENDING', 'PROCESSING', 'SENT', 'FAILED', 'CANCELLED'
    )),
    -- The hard anti-duplicate guarantee: one delivery per step, per contact,
    -- per campaign run.
    CONSTRAINT scheduled_messages_unique_step UNIQUE (enrollment_id, campaign_step_id, run_number)
);

-- The scheduler's hot path: pending jobs that are due, oldest first.
CREATE INDEX scheduled_messages_due_idx
    ON scheduled_messages (scheduled_at)
    WHERE status = 'PENDING';
CREATE INDEX scheduled_messages_status_idx ON scheduled_messages (status, scheduled_at);
CREATE INDEX scheduled_messages_campaign_idx ON scheduled_messages (campaign_id, status);
CREATE INDEX scheduled_messages_contact_idx ON scheduled_messages (contact_id, scheduled_at);
CREATE INDEX scheduled_messages_stuck_idx ON scheduled_messages (locked_at) WHERE status = 'PROCESSING';

CREATE TRIGGER scheduled_messages_set_updated_at
    BEFORE UPDATE ON scheduled_messages
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE messages
    ADD CONSTRAINT messages_scheduled_message_fk
    FOREIGN KEY (scheduled_message_id) REFERENCES scheduled_messages (id) ON DELETE SET NULL;

CREATE TABLE webhook_events (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider     text        NOT NULL DEFAULT 'greenapi',
    event_type   text        NOT NULL DEFAULT '',
    -- Stable identity of the delivered event, used to reject replays.
    dedupe_key   text        NOT NULL,
    external_id  text        NOT NULL DEFAULT '',
    payload      jsonb       NOT NULL,
    status       text        NOT NULL DEFAULT 'RECEIVED',
    attempts     integer     NOT NULL DEFAULT 0,
    error        text        NOT NULL DEFAULT '',
    received_at  timestamptz NOT NULL DEFAULT now(),
    processed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT webhook_events_status_check CHECK (status IN (
        'RECEIVED', 'PROCESSING', 'PROCESSED', 'FAILED', 'IGNORED'
    )),
    UNIQUE (provider, dedupe_key)
);

CREATE INDEX webhook_events_pending_idx
    ON webhook_events (received_at)
    WHERE status IN ('RECEIVED', 'PROCESSING');
CREATE INDEX webhook_events_type_idx ON webhook_events (event_type, received_at DESC);
CREATE INDEX webhook_events_status_idx ON webhook_events (status, received_at DESC);

CREATE TRIGGER webhook_events_set_updated_at
    BEFORE UPDATE ON webhook_events
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
