-- +goose Up
-- Roles: 'admin' runs the place (users, the catalog, every app); 'user'
-- plays what the catalog offers. Quotas are per user, empty = the defaults.
ALTER TABLE users ADD COLUMN role text NOT NULL DEFAULT 'user';
ALTER TABLE users ADD COLUMN quota jsonb NOT NULL DEFAULT '{}'::jsonb;
-- An install so far had exactly one account, the operator's.
UPDATE users SET role = 'admin' WHERE (SELECT count(*) FROM users) = 1;

-- The catalog: app definitions an admin maintains; users create their own
-- instances (rows in apps, each with its own volume) from them.
CREATE TABLE catalog (
    id              uuid PRIMARY KEY,
    name            text NOT NULL UNIQUE,
    description     text NOT NULL DEFAULT '',
    preset          text NOT NULL,
    image           text NOT NULL,
    icon_url        text NOT NULL DEFAULT '',
    hdr             boolean NOT NULL DEFAULT false,
    command         text NOT NULL DEFAULT '',
    pvc_size        text NOT NULL DEFAULT '',
    storage_class   text NOT NULL DEFAULT '',
    resources       jsonb NOT NULL DEFAULT '{}'::jsonb,
    env             jsonb NOT NULL DEFAULT '{}'::jsonb,
    host_ipc        boolean NOT NULL DEFAULT false,
    capabilities    jsonb NOT NULL DEFAULT '[]'::jsonb,
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

-- An instance follows its catalog entry (settings are copied on every
-- start); a deleted entry leaves the instance as it last ran.
ALTER TABLE apps ADD COLUMN template_id uuid REFERENCES catalog(id) ON DELETE SET NULL;
CREATE INDEX apps_template_idx ON apps(template_id);

-- +goose Down
DROP INDEX apps_template_idx;
ALTER TABLE apps DROP COLUMN template_id;
DROP TABLE catalog;
ALTER TABLE users DROP COLUMN quota;
ALTER TABLE users DROP COLUMN role;
