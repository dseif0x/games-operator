-- +goose Up
CREATE TABLE users (
    id             uuid PRIMARY KEY,
    username       text NOT NULL UNIQUE,
    password_hash  text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    disabled       boolean NOT NULL DEFAULT false,
    api_token_hash text NOT NULL DEFAULT ''
);
CREATE INDEX users_api_token_idx ON users(api_token_hash) WHERE api_token_hash <> '';

CREATE TABLE apps (
    id              uuid PRIMARY KEY,
    owner_id        uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    moonlight_id    integer NOT NULL,
    name            text NOT NULL,
    preset          text NOT NULL,
    image           text NOT NULL,
    icon_url        text NOT NULL DEFAULT '',
    hdr             boolean NOT NULL DEFAULT false,
    command         text NOT NULL DEFAULT '',
    pvc_size        text NOT NULL,
    storage_class   text NOT NULL DEFAULT '',
    resources       jsonb NOT NULL DEFAULT '{}'::jsonb,
    env             jsonb NOT NULL DEFAULT '{}'::jsonb,
    host_ipc        boolean NOT NULL DEFAULT false,
    capabilities    jsonb NOT NULL DEFAULT '[]'::jsonb,
    state           text NOT NULL,
    state_reason    text NOT NULL DEFAULT '',
    generation      integer NOT NULL DEFAULT 1,
    stream          jsonb,
    slot            integer NOT NULL DEFAULT -1,
    wolf_session_id text NOT NULL DEFAULT '',
    stream_url      text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    last_active_at  timestamptz,
    UNIQUE (owner_id, moonlight_id)
);
CREATE INDEX apps_owner_idx ON apps(owner_id);
CREATE INDEX apps_state_idx ON apps(state);

-- A Moonlight client certificate (by fingerprint) bound to the user who
-- typed the PIN.
CREATE TABLE pairings (
    id           text PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    cert_pem     text NOT NULL,
    name         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz
);
CREATE INDEX pairings_user_idx ON pairings(user_id);

CREATE TABLE app_events (
    id      bigserial PRIMARY KEY,
    app_id  uuid NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    at      timestamptz NOT NULL DEFAULT now(),
    kind    text NOT NULL,
    message text NOT NULL DEFAULT ''
);
CREATE INDEX app_events_app_idx ON app_events(app_id, id DESC);

-- +goose Down
DROP TABLE app_events;
DROP TABLE pairings;
DROP TABLE apps;
DROP TABLE users;
