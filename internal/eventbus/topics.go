package eventbus

// Topics published by SpaceMan services. Payloads carry IDs only; consumers re-read state from the database.
const (
	TopicUserChanged   = "user.changed"
	TopicGrantsChanged = "grants.changed"
	TopicTokenRevoked  = "token.revoked"
	TopicChange        = "change.committed" // a row was appended to the durable change feed (Key = seq)
	TopicNodeChanged   = "node.changed"
	TopicSpaceChanged  = "space.changed"
	TopicRunChanged    = "run.changed"
	TopicAudit         = "audit.appended"
	TopicSystemStatus  = "system.status"
	// TopicTelegramSettings: the Telegram settings or a space's Telegram settings changed (Key = space ID, "" for the
	// global settings). The settings cache reloads; it never reaches the change feed (ADR-0083).
	TopicTelegramSettings = "telegram.settings"
)
