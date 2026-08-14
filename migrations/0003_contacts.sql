-- Contacts and their campaign enrollments.

CREATE TABLE contacts (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    phone                 text        NOT NULL,
    chat_id               text        NOT NULL,
    name                  text        NOT NULL DEFAULT '',
    push_name             text        NOT NULL DEFAULT '',
    source                text        NOT NULL DEFAULT 'WHATSAPP_TRIGGER',
    first_trigger_keyword text        NOT NULL DEFAULT '',
    first_campaign_id     uuid REFERENCES campaigns (id) ON DELETE SET NULL,
    status                text        NOT NULL DEFAULT 'NEW',
    opted_out             boolean     NOT NULL DEFAULT false,
    opted_out_at          timestamptz,
    blocked_at            timestamptz,
    -- Consent anchor: set on the first inbound message. Nothing may be sent
    -- to a contact whose value is NULL.
    first_contact_at      timestamptz,
    last_incoming_at      timestamptz,
    last_outgoing_at      timestamptz,
    last_activity_at      timestamptz,
    incoming_count        integer     NOT NULL DEFAULT 0,
    outgoing_count        integer     NOT NULL DEFAULT 0,
    notes                 text        NOT NULL DEFAULT '',
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT contacts_status_check CHECK (status IN ('NEW', 'ACTIVE', 'COMPLETED', 'UNSUBSCRIBED', 'BLOCKED', 'ERROR')),
    CONSTRAINT contacts_phone_format CHECK (phone ~ '^[0-9]{6,20}$')
);

CREATE UNIQUE INDEX contacts_phone_key ON contacts (phone);
CREATE UNIQUE INDEX contacts_chat_id_key ON contacts (chat_id);
CREATE INDEX contacts_created_at_idx ON contacts (created_at DESC);
CREATE INDEX contacts_status_idx ON contacts (status);
CREATE INDEX contacts_last_activity_idx ON contacts (last_activity_at DESC NULLS LAST);
CREATE INDEX contacts_first_campaign_idx ON contacts (first_campaign_id);
CREATE INDEX contacts_name_search_idx ON contacts (lower(name));

CREATE TRIGGER contacts_set_updated_at
    BEFORE UPDATE ON contacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE tags (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL,
    color      text        NOT NULL DEFAULT '#4b5563',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tags_name_not_blank CHECK (length(btrim(name)) > 0)
);

CREATE UNIQUE INDEX tags_name_key ON tags (lower(btrim(name)));

CREATE TABLE contact_tags (
    contact_id uuid        NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    tag_id     uuid        NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (contact_id, tag_id)
);

CREATE INDEX contact_tags_tag_idx ON contact_tags (tag_id);

-- A contact's participation in one campaign. run_number increments when the
-- admin has configured RESTART behaviour and the contact triggers again.
CREATE TABLE campaign_contacts (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id     uuid        NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    contact_id      uuid        NOT NULL REFERENCES contacts (id) ON DELETE CASCADE,
    trigger_id      uuid REFERENCES campaign_triggers (id) ON DELETE SET NULL,
    trigger_keyword text        NOT NULL DEFAULT '',
    status          text        NOT NULL DEFAULT 'ACTIVE',
    run_number      integer     NOT NULL DEFAULT 1,
    restart_count   integer     NOT NULL DEFAULT 0,
    enrolled_at     timestamptz NOT NULL DEFAULT now(),
    completed_at    timestamptz,
    cancelled_at    timestamptz,
    cancel_reason   text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT campaign_contacts_status_check CHECK (status IN ('ACTIVE', 'COMPLETED', 'CANCELLED', 'UNSUBSCRIBED')),
    CONSTRAINT campaign_contacts_run_check CHECK (run_number >= 1),
    UNIQUE (campaign_id, contact_id)
);

CREATE INDEX campaign_contacts_campaign_idx ON campaign_contacts (campaign_id, status);
CREATE INDEX campaign_contacts_contact_idx ON campaign_contacts (contact_id);
CREATE INDEX campaign_contacts_enrolled_idx ON campaign_contacts (enrolled_at DESC);

CREATE TRIGGER campaign_contacts_set_updated_at
    BEFORE UPDATE ON campaign_contacts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
