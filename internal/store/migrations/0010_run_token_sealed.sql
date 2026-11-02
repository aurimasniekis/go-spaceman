-- 0010_run_token_sealed: a run keeps one credential for its lifetime. The token is also stored sealed (keyring purpose
-- credential) so that re-sending an unacknowledged launch carries the same token the running harness already holds.

-- +goose Up
ALTER TABLE run_credentials ADD COLUMN token_enc BLOB;

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
