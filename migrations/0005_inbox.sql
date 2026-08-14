-- Chat console support: avatars, unread tracking, denormalized chat previews
-- and inbound media download bookkeeping.

ALTER TABLE contacts
    ADD COLUMN avatar_url             text        NOT NULL DEFAULT '',
    ADD COLUMN avatar_source_url      text        NOT NULL DEFAULT '',
    ADD COLUMN avatar_checked_at      timestamptz,
    ADD COLUMN unread_count           integer     NOT NULL DEFAULT 0,
    -- Denormalized so the chat list renders from a single indexed scan
    -- instead of a lateral join over the whole message history.
    ADD COLUMN last_message_preview   text        NOT NULL DEFAULT '',
    ADD COLUMN last_message_type      text        NOT NULL DEFAULT '',
    ADD COLUMN last_message_direction text        NOT NULL DEFAULT '',
    ADD CONSTRAINT contacts_unread_check CHECK (unread_count >= 0);

-- The chat list ordering: most recently active conversation first.
CREATE INDEX contacts_chat_list_idx
    ON contacts (last_activity_at DESC NULLS LAST, created_at DESC);

ALTER TABLE messages
    ADD COLUMN media_download_status text NOT NULL DEFAULT 'NONE',
    ADD COLUMN media_download_error  text NOT NULL DEFAULT '',
    ADD CONSTRAINT messages_media_download_check
        CHECK (media_download_status IN ('NONE', 'PENDING', 'DONE', 'FAILED', 'SKIPPED'));

-- Worker queue for pulling inbound attachments into local storage before the
-- provider's temporary download url expires.
CREATE INDEX messages_media_pending_idx
    ON messages (created_at)
    WHERE media_download_status = 'PENDING';
