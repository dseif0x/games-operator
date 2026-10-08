-- +goose Up
-- A second, hub-managed token per user: the one the hub hands moonlight-web
-- for each "Play in browser", separate from the token on the Account page.
ALTER TABLE users ADD COLUMN browser_token_hash text NOT NULL DEFAULT '';
-- How a pairing came to be: '' for a PIN typed on the Pair page, 'browser'
-- for moonlight-web pairing through the Wolf-compatible API with a browser
-- token. Browser pairings follow whoever presses Play.
ALTER TABLE pairings ADD COLUMN via text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pairings DROP COLUMN via;
ALTER TABLE users DROP COLUMN browser_token_hash;
