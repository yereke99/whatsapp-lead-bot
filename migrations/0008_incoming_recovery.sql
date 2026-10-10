-- Additive recovery checkpoints. Existing rows and migration checksums remain
-- unchanged; NULL means that the old application did not record a checkpoint.
ALTER TABLE webhook_events ADD COLUMN trigger_result TEXT
    CHECK (trigger_result IS NULL OR json_valid(trigger_result));
ALTER TABLE webhook_events ADD COLUMN next_attempt_at TEXT;
ALTER TABLE messages ADD COLUMN incoming_event_id TEXT REFERENCES webhook_events (id);

CREATE INDEX webhook_events_incoming_identity_idx
    ON webhook_events (provider, COALESCE(whatsapp_instance_id, 1), external_id, event_type);
CREATE INDEX webhook_events_reply_job_idx
    ON webhook_events (CAST(json_extract(trigger_result, '$.initial_job_id') AS TEXT))
    WHERE trigger_result IS NOT NULL;
CREATE INDEX messages_incoming_event_idx ON messages (incoming_event_id)
    WHERE incoming_event_id IS NOT NULL;
CREATE INDEX messages_scheduled_receipt_idx ON messages (scheduled_message_id, sent_at)
    WHERE scheduled_message_id IS NOT NULL;
