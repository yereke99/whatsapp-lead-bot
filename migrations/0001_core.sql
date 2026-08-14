-- Core platform tables: administrators, sessions, auditing, settings, media.

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE admins (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text        NOT NULL,
    name          text        NOT NULL DEFAULT '',
    password_hash text        NOT NULL,
    role          text        NOT NULL DEFAULT 'ADMIN',
    is_active     boolean     NOT NULL DEFAULT true,
    last_login_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT admins_role_check CHECK (role IN ('OWNER', 'ADMIN', 'VIEWER')),
    CONSTRAINT admins_email_not_blank CHECK (length(btrim(email)) > 0)
);

CREATE UNIQUE INDEX admins_email_key ON admins (lower(email));

CREATE TRIGGER admins_set_updated_at
    BEFORE UPDATE ON admins
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE admin_sessions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_id     uuid        NOT NULL REFERENCES admins (id) ON DELETE CASCADE,
    token_hash   text        NOT NULL UNIQUE,
    csrf_token   text        NOT NULL,
    ip_address   text        NOT NULL DEFAULT '',
    user_agent   text        NOT NULL DEFAULT '',
    expires_at   timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX admin_sessions_admin_id_idx ON admin_sessions (admin_id);
CREATE INDEX admin_sessions_expires_at_idx ON admin_sessions (expires_at);

-- Brute-force protection ledger. Rows are pruned by a background job.
CREATE TABLE login_attempts (
    id         bigserial PRIMARY KEY,
    email      text        NOT NULL DEFAULT '',
    ip_address text        NOT NULL DEFAULT '',
    success    boolean     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX login_attempts_email_idx ON login_attempts (lower(email), created_at DESC);
CREATE INDEX login_attempts_ip_idx ON login_attempts (ip_address, created_at DESC);

CREATE TABLE audit_logs (
    id          bigserial PRIMARY KEY,
    admin_id    uuid REFERENCES admins (id) ON DELETE SET NULL,
    admin_email text        NOT NULL DEFAULT '',
    action      text        NOT NULL,
    entity_type text        NOT NULL DEFAULT '',
    entity_id   text        NOT NULL DEFAULT '',
    summary     text        NOT NULL DEFAULT '',
    old_values  jsonb,
    new_values  jsonb,
    ip_address  text        NOT NULL DEFAULT '',
    user_agent  text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_created_at_idx ON audit_logs (created_at DESC);
CREATE INDEX audit_logs_admin_idx ON audit_logs (admin_id, created_at DESC);
CREATE INDEX audit_logs_entity_idx ON audit_logs (entity_type, entity_id, created_at DESC);

CREATE TABLE app_settings (
    key        text PRIMARY KEY,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    updated_by uuid REFERENCES admins (id) ON DELETE SET NULL
);

CREATE TABLE media_files (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    original_name       text        NOT NULL,
    stored_name         text        NOT NULL,
    relative_path       text        NOT NULL UNIQUE,
    mime_type           text        NOT NULL,
    size_bytes          bigint      NOT NULL,
    kind                text        NOT NULL,
    checksum_sha256     text        NOT NULL DEFAULT '',
    duration_ms         integer,
    width               integer,
    height              integer,
    -- Set when this row is a derived artifact, e.g. an MP3 transcoded to
    -- OGG/Opus so WhatsApp renders it as a voice note.
    source_media_file_id uuid REFERENCES media_files (id) ON DELETE SET NULL,
    uploaded_by         uuid REFERENCES admins (id) ON DELETE SET NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT media_files_kind_check CHECK (kind IN ('IMAGE', 'VIDEO', 'AUDIO', 'VOICE', 'DOCUMENT')),
    CONSTRAINT media_files_size_check CHECK (size_bytes > 0)
);

CREATE INDEX media_files_kind_idx ON media_files (kind, created_at DESC);
CREATE INDEX media_files_checksum_idx ON media_files (checksum_sha256);
