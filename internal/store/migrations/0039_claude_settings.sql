-- 0039_claude_settings: Claude Code settings on harness profiles (ADR-0059, SM-PROMPT-006). A claude-code profile can carry
-- auto mode rules and permission rules; central validates them and a run's node writes them as the run's
-- $CLAUDE_CONFIG_DIR/settings.json. '{}' means none.

-- +goose Up
ALTER TABLE harness_profiles ADD COLUMN claude_settings_json TEXT NOT NULL DEFAULT '{}';

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
