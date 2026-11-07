-- 0041_spaceman_mcp: the SpaceMan admin MCP server kind (ADR-0063, SM-MCP-005…009). A gateway server of kind 'spaceman'
-- runs in central and acts through a secretless internal API token: api_tokens.purpose 'spaceman_mcp' marks a token
-- that can never authenticate as a bearer (its MAC is over a discarded random value and AuthenticateAPIToken refuses
-- any purpose); central builds the principal from the token ID per call. spaceman_json holds the server's preset,
-- tool list and approval mode. url is '' for the spaceman kind (enforced by the service).

-- +goose Up
ALTER TABLE mcp_servers ADD COLUMN kind TEXT NOT NULL DEFAULT 'http' CHECK (kind IN ('http', 'spaceman'));
ALTER TABLE mcp_servers ADD COLUMN api_token_id TEXT REFERENCES api_tokens (id);
ALTER TABLE mcp_servers ADD COLUMN spaceman_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE api_tokens ADD COLUMN purpose TEXT NOT NULL DEFAULT '' CHECK (purpose IN ('', 'spaceman_mcp'));

-- +goose Down
SELECT 'forward-only migrations: restore the pre-migration snapshot instead';
