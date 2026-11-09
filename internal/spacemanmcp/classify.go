package spacemanmcp

import "github.com/aurimasniekis/go-spaceman/internal/api"

// Class is what an API operation is as a SpaceMan MCP tool (ADR-0063 §Decision 5).
type Class string

// The classes, from least to most consequential. Excluded operations are never tools, whatever a server's settings.
const (
	// Read operations only read what the token's grants reach.
	Read Class = "read"
	// Write operations change SpaceMan's state within the token's grants.
	Write Class = "write"
	// Dangerous operations destroy data, handle secrets, change what runs execute or widen reach. They are offered
	// only when a server names them, and a call always needs a person's approval (AM-5).
	Dangerous Class = "dangerous"
	// SensitiveRead operations read recordings and transcripts; offered only when named.
	SensitiveRead Class = "sensitive-read"
	// Excluded operations are never offered.
	Excluded Class = "excluded"
)

// Classes lists the classes in order.
var Classes = []Class{Read, Write, Dangerous, SensitiveRead, Excluded}

// Entry is the reviewed classification of one operation.
type Entry struct {
	Class  Class
	Reason string
}

// AutoExcluded reports whether an operation's rule keeps it from the SpaceMan MCP whatever the table says, and why:
// it needs a person at the keyboard (sign-in, own account, step-up) or no principal at all. The API refuses an
// internal principal on these rules too (authorizeOps).
func AutoExcluded(r api.Rule) (string, bool) {
	switch {
	case r.Public:
		return "public operation: needs no principal, so it is not an administrator's action", true
	case r.Reauth || r.MFA:
		return "needs a recent re-authentication or a second factor: a person at the keyboard (step-up)", true
	case r.UserOnly:
		return "acts on the caller's own user account", true
	case r.Session && !r.Filtered:
		return "a signed-in principal's own view (session rule); internal principals are refused on it", true
	}
	return "", false
}

// Reasons shared by many entries.
const (
	rRead       = "reads what the token's grants reach"
	rSpaceWrite = "changes a space's work or configuration within the token's grants"
	rDraft      = "edits a draft or an unpublished catalog entry; publishing is separate"
	rHuman      = "acts as a person on a human action (answers, assigns, moves, comments or routes it)"
	rApproval   = "approvals are a person's decision; an agent never reads or decides them through these tools"
	rTakeover   = "terminal takeover and input belong to a person at the keyboard"
	rUsers      = "user accounts and identities are managed by people only"
	rTokens     = "token management: an agent never mints, lists or revokes credentials"
	rSignIn     = "sign-in providers and settings are managed by people only"
	rMCP        = "HTTP MCP server and grant writes: outbound calls (SSRF) and tool reach of other runs; AM-5 also refuses spaceman-kind targets"
	rDelete     = "deletes configuration or data"
	rSecret     = "handles credentials or keys (write-only secrets pass through the model's context)"
	rPublish    = "publishes or rolls back what every space's runs execute or render"
	rPolicy     = "changes who is asked and how for human actions"
	rRecording  = "reads terminal recordings, which may contain anything a run printed"
	rSelf       = "SpaceMan MCP servers hand an administrator's authority to agents: managed by people only"
	// rSpaceRole: a space role decides what a launch by role runs (its template's kind, profile, permission mode and
	// settings), so saving one can make a role diagnostic or widen its runs' access without any later launch saying so.
	// Every save is dangerous rather than only the widening ones: whether a template is diagnostic or a profile or
	// setting widens access is resolved from other catalog entries, which can change after the check.
	rSpaceRole = "points a space role at any role template and profile and sets its permission mode and settings: it can " +
		"make a role diagnostic or widen what its runs may do, and every later launch by that role inherits it"
	// rLaunch: a diagnostic launch is dangerous. The gateway classes it by its kind argument; the API checks the kind as
	// resolved (a space role may set it) and refuses a diagnostic launch the gateway did not approve.
	rLaunch = "launches a run; inherent to operating a space (a diagnostic launch, by argument or by its role's kind, " +
		"needs a person's approval)"
	// rFiltered marks Session-rule reads that return only what the principal's grants reach (filtered in the handler,
	// api.Rule.Filtered). Internal principals may call them; personal Session operations stay excluded.
	rFiltered = "reads only what the principal's grants reach (filtered in the handler)"
)

// Table is the reviewed classification of every API operation that AutoExcluded does not exclude. An entry for an
// automatically excluded operation is the class it falls back to if its rule ever stops excluding it.
// TestEveryOperationIsClassified keeps the table complete: a new operation needs an entry (and a review).
var Table = map[string]Entry{
	// Session-rule reads filtered by the principal's grants (api.FilteredRead).
	"list-spaces":                {Read, rFiltered},
	"list-event-connections":     {Read, rFiltered},
	"get-event-connection":       {Read, rFiltered},
	"list-event-templates":       {Read, rFiltered},
	"get-event-template":         {Read, rFiltered},
	"preview-event-template":     {Read, rFiltered},
	"get-attention":              {Read, rFiltered},
	"list-account-kinds":         {Read, rFiltered},
	"list-limit-definitions":     {Read, rFiltered},
	"list-pending-human-actions": {Read, rFiltered},

	// Accounts (credentials for harnesses).
	"create-account":      {Dangerous, rSecret},
	"update-account":      {Dangerous, rSecret},
	"delete-account":      {Dangerous, rSecret},
	"get-account":         {Read, rRead},
	"list-accounts":       {Read, rRead},
	"list-space-accounts": {Read, rRead},
	"set-space-accounts":  {Dangerous, "chooses which credentials a space's runs receive"},

	// Admin.
	"get-break-glass":     {Read, rRead},
	"get-system":          {Read, rRead},
	"acknowledge-restore": {Excluded, "acknowledging a restore is an operator's confirmation after a recovery"},
	// Step-up (excluded by its rule); the fallback.
	"acknowledge-break-glass": {Excluded, "acknowledging a break-glass sign-in is an operator's confirmation"},

	// Approvals.
	"create-approval-policy": {Excluded, rApproval},
	"delete-approval-policy": {Excluded, rApproval},
	"get-action-approval":    {Excluded, rApproval},
	"get-approval":           {Excluded, rApproval},
	"get-approval-policy":    {Excluded, rApproval},
	"list-approval-policies": {Excluded, rApproval},
	"list-approvals":         {Excluded, rApproval},
	"revoke-approval":        {Excluded, rApproval},

	// Attachments.
	"get-attachment":    {Read, rRead},
	"upload-attachment": {Excluded, "multipart upload: tools carry JSON arguments only"},

	// Audit.
	"list-audit":         {Read, rRead},
	"verify-audit-chain": {Read, "verifies the audit hash chain; changes nothing"},

	// Backups.
	"create-backup":     {Write, "creates a backup"},
	"get-restore-state": {Read, rFiltered},
	"list-backups":      {Read, rRead},
	"verify-backup":     {Write, "verifies a backup; changes nothing but runs work"},

	// Cross-space.
	"cancel-xspace-request":    {Write, rSpaceWrite},
	"decline-xspace-request":   {Write, rSpaceWrite},
	"delete-xspace-peer":       {Dangerous, "removes a cross-space trust relation"},
	"get-xspace-peer":          {Read, rRead},
	"get-xspace-request":       {Read, rRead},
	"get-xspace-settings":      {Read, rRead},
	"list-all-xspace-peers":    {Read, rRead},
	"list-all-xspace-requests": {Read, rRead},
	"list-xspace-peers":        {Read, rRead},
	"list-xspace-requests":     {Read, rRead},
	"put-xspace-peer":          {Dangerous, "widens what another space may ask of this one"},
	"update-xspace-settings":   {Dangerous, "widens what other spaces may ask of this one"},

	// Dreams.
	"create-dream": {Write, rSpaceWrite},
	"dream-runs":   {Read, rRead},
	"list-dreams":  {Read, rRead},
	"start-dream":  {Write, rSpaceWrite},
	"update-dream": {Write, rSpaceWrite},

	// Events.
	"create-event-connection": {Dangerous, rSecret},
	"update-event-connection": {Dangerous, rSecret},
	"delete-event-connection": {Dangerous, rSecret},
	"create-event-template":   {Write, rDraft},
	"save-template-draft":     {Write, rDraft},
	"publish-template":        {Dangerous, rPublish},

	// Git SSH keys.
	"create-git-ssh-key":      {Dangerous, rSecret},
	"delete-git-ssh-key":      {Dangerous, rSecret},
	"get-git-ssh-key":         {Read, rRead},
	"list-git-ssh-keys":       {Read, rRead},
	"list-space-git-ssh-keys": {Read, rRead},
	"rotate-git-ssh-key":      {Dangerous, rSecret},
	"update-git-ssh-key":      {Dangerous, "changes which spaces a repository key reaches"},

	// Harbor Pilot (ADR-0060): the connection holds write-only tokens; a space's settings are configuration.
	"put-harbor-pilot-connection":    {Dangerous, rSecret},
	"delete-harbor-pilot-connection": {Dangerous, rSecret},
	"get-harbor-pilot-connection":    {Read, rRead},
	"get-space-harbor-pilot":         {Read, rRead},
	"get-space-harbor-pilot-status":  {Read, rRead},
	"put-space-harbor-pilot":         {Dangerous, "chooses which Harbor Pilot project (and spend) a space's runs bill against"},
	"delete-space-harbor-pilot":      {Write, rSpaceWrite},

	// Human-action policies.
	"delete-human-policy":       {Dangerous, rPolicy},
	"delete-space-human-policy": {Dangerous, rPolicy},
	"list-human-policies":       {Read, rRead},
	"list-space-human-policies": {Read, rRead},
	"set-human-policy":          {Dangerous, rPolicy},
	"set-space-human-policy":    {Dangerous, rPolicy},

	// Human-action types.
	"create-human-type":            {Write, rDraft},
	"save-human-type-draft":        {Write, rDraft},
	"publish-human-type":           {Dangerous, rPublish},
	"delete-human-type-permission": {Dangerous, "changes who may answer a human-action type"},
	"set-human-type-permission":    {Dangerous, "changes who may answer a human-action type"},
	"disable-space-human-type":     {Write, rSpaceWrite},
	"enable-space-human-type":      {Write, rSpaceWrite},
	"get-human-type":               {Read, rRead},
	"get-human-type-permission":    {Read, rRead},
	"list-human-type-options":      {Read, rRead},
	"list-human-type-permissions":  {Read, rRead},
	"list-human-types":             {Read, rRead},
	"list-space-human-types":       {Read, rRead},

	// Human actions: an agent may ask and read, never answer.
	"create-human-action":      {Write, "asks a person, as the spaceman_human_ask tool does"},
	"get-human-action":         {Read, rRead},
	"list-human-actions":       {Read, rRead},
	"assign-human-action":      {Excluded, rHuman},
	"cancel-human-action":      {Excluded, rHuman},
	"clarify-human-action":     {Excluded, rHuman},
	"comment-human-action":     {Excluded, rHuman},
	"move-human-action":        {Excluded, rHuman},
	"respond-human-action":     {Excluded, rHuman},
	"route-human-notification": {Excluded, rHuman},

	// Inbox.
	"create-inbox-item": {Write, rSpaceWrite},
	"get-inbox-item":    {Read, rRead},
	"list-inbox":        {Read, rRead},
	"set-inbox-status":  {Write, rSpaceWrite},

	// Knowledge bases.
	"accept-kb-proposal": {Write, rSpaceWrite},
	"cancel-kb-request":  {Write, rSpaceWrite},
	"create-kb":          {Write, rSpaceWrite},
	"get-kb":             {Read, rRead},
	"list-kbs":           {Read, rRead},
	"reject-kb-proposal": {Write, rSpaceWrite},
	"update-kb":          {Write, rSpaceWrite},

	// Limits.
	"get-space-limits": {Read, rRead},
	"set-space-limits": {Write, rSpaceWrite},

	// SpaceMan MCP servers: step-up (excluded by their rule); the fallback. An agent never manages its own tools.
	"create-spaceman-mcp":  {Excluded, rSelf},
	"update-spaceman-mcp":  {Excluded, rSelf},
	"reissue-spaceman-mcp": {Excluded, rSelf},
	// The tool picker for people creating a server.
	"list-spaceman-mcp-tools": {Excluded, rSelf},

	// MCP gateway.
	"add-mcp-grant":         {Dangerous, rMCP},
	"check-mcp-server":      {Dangerous, rMCP},
	"create-mcp-server":     {Dangerous, rMCP},
	"delete-mcp-server":     {Dangerous, rMCP},
	"revoke-mcp-grant":      {Dangerous, rMCP},
	"update-mcp-server":     {Dangerous, rMCP},
	"get-mcp-server":        {Read, rRead},
	"list-mcp-grants":       {Read, rRead},
	"list-mcp-servers":      {Read, rRead},
	"list-space-mcp-grants": {Read, rRead},

	// Memory.
	"get-memory":              {Read, rRead},
	"get-memory-file":         {Read, rRead},
	"list-memory":             {Read, rRead},
	"list-memory-conflicts":   {Read, rRead},
	"list-memory-files":       {Read, rRead},
	"memory-history":          {Read, rRead},
	"resolve-memory-conflict": {Write, rSpaceWrite},

	// Messages.
	"ack-message":     {Write, rSpaceWrite},
	"cancel-message":  {Write, rSpaceWrite},
	"get-message":     {Read, rRead},
	"list-messages":   {Read, rRead},
	"reply-message":   {Write, rSpaceWrite},
	"resolve-message": {Write, rSpaceWrite},
	"send-message":    {Write, rSpaceWrite},

	// Nodes.
	"get-node":       {Read, rRead},
	"list-nodes":     {Read, rRead},
	"update-node":    {Dangerous, "changes a node's labels, which decide where runs are placed"},
	"set-node-state": {Dangerous, "drains or disables a node, stopping its runs"},
	// Step-up (excluded by their rule); the fallback: they return a one-time enrollment secret.
	"create-node":        {Excluded, "returns a one-time node enrollment secret"},
	"reissue-enrollment": {Excluded, "returns a one-time node enrollment secret"},

	// Prompts and harness profiles.
	"create-prompt":          {Write, rDraft},
	"diff-prompt-versions":   {Read, rRead},
	"discard-prompt-draft":   {Write, rDraft},
	"fork-prompt":            {Write, rDraft},
	"get-prompt":             {Read, rRead},
	"list-prompts":           {Read, rRead},
	"publish-prompt":         {Dangerous, rPublish},
	"rollback-prompt":        {Dangerous, rPublish},
	"save-prompt-draft":      {Write, rDraft},
	"delete-harness-profile": {Dangerous, rDelete},
	"get-harness-profile":    {Read, rRead},
	"list-harness-profiles":  {Read, rRead},
	"save-harness-profile":   {Dangerous, "harness settings (hooks) run code on nodes (T-32)"},

	// Queue.
	"add-task-blocker":      {Write, rSpaceWrite},
	"create-task":           {Write, rSpaceWrite},
	"decide-incident":       {Write, rSpaceWrite},
	"escalate-task":         {Write, rSpaceWrite},
	"get-task":              {Read, rRead},
	"list-tasks":            {Read, rRead},
	"move-task":             {Write, rSpaceWrite},
	"queue-decisions":       {Read, rRead},
	"queue-next":            {Read, rRead},
	"resolve-task-blocker":  {Write, rSpaceWrite},
	"resume-task":           {Write, rSpaceWrite},
	"set-queue-policy":      {Write, rSpaceWrite},
	"set-task-dependencies": {Write, rSpaceWrite},
	"set-task-status":       {Write, rSpaceWrite},
	"update-task":           {Write, rSpaceWrite},

	// Recordings and transcripts.
	"get-recording":     {SensitiveRead, rRecording},
	"delete-recording":  {Dangerous, rDelete},
	"delete-transcript": {Dangerous, rDelete},

	// Relocation.
	"cancel-relocation":  {Dangerous, "moves a space between nodes"},
	"get-relocation":     {Read, rRead},
	"list-relocations":   {Read, rRead},
	"start-relocation":   {Dangerous, "moves a space between nodes"},
	"release-relocation": {Dangerous, "moves a space between nodes"},
	"retry-relocation":   {Dangerous, "moves a space between nodes"},

	// Repositories.
	"create-space-repository":     {Write, rSpaceWrite},
	"delete-space-repository":     {Dangerous, rDelete},
	"generate-repository-ssh-key": {Dangerous, rSecret},
	"get-space-repository":        {Read, rRead},
	"list-space-repositories":     {Read, rRead},
	"remove-repository-ssh-key":   {Dangerous, rSecret},
	"sync-space-repositories":     {Write, rSpaceWrite},
	"update-space-repository":     {Write, rSpaceWrite},

	// Retention.
	"get-retention": {Read, rRead},
	"run-retention": {Dangerous, rDelete},

	// Roles and role templates.
	"add-starter-roles":    {Write, rSpaceWrite},
	"delete-role-template": {Dangerous, rPublish},
	"delete-space-role":    {Dangerous, rDelete},
	"get-role-template":    {Read, rRead},
	"get-space-role":       {Read, rRead},
	"list-role-options":    {Read, rRead},
	"list-role-templates":  {Read, rRead},
	"list-space-roles":     {Read, rRead},
	"list-starter-roles":   {Read, rRead},
	"preview-space-role":   {Read, rRead},
	"save-role-template":   {Dangerous, rPublish},
	"save-space-role":      {Dangerous, rSpaceRole},

	// Runs.
	"get-run":        {Read, rRead},
	"launch-run":     {Write, rLaunch},
	"list-runs":      {Read, rRead},
	"stop-run":       {Write, rSpaceWrite},
	"start-takeover": {Excluded, rTakeover},
	"end-takeover":   {Excluded, rTakeover},
	"renew-takeover": {Excluded, rTakeover},
	"send-input":     {Excluded, rTakeover},
	"takeover-input": {Excluded, rTakeover},
	// Resizing a run's terminal needs terminal:takeover (ADR-0075): it is a person's at the keyboard too.
	"set-terminal-size":   {Excluded, rTakeover},
	"clear-terminal-size": {Excluded, rTakeover},

	// Schedules.
	"create-schedule":  {Write, rSpaceWrite},
	"delete-schedule":  {Dangerous, rDelete},
	"get-schedule":     {Read, rRead},
	"list-schedules":   {Read, rRead},
	"preview-schedule": {Read, "previews a schedule's next times; changes nothing"},
	"update-schedule":  {Write, rSpaceWrite},

	// Sign-in providers.
	"get-oidc-provider":    {Excluded, rSignIn},
	"get-oidc-test-login":  {Excluded, rSignIn},
	"get-sign-in-settings": {Excluded, rSignIn},
	"list-oidc-mappings":   {Excluded, rSignIn},
	"list-oidc-providers":  {Excluded, rSignIn},
	// The step-up writes are excluded by their rule; these entries are what they fall back to.
	"set-sign-in-settings": {Excluded, rSignIn},
	"create-oidc-provider": {Excluded, rSignIn},
	"update-oidc-provider": {Excluded, rSignIn},
	"delete-oidc-provider": {Excluded, rSignIn},
	"add-oidc-mapping":     {Excluded, rSignIn},
	"delete-oidc-mapping":  {Excluded, rSignIn},

	// Skills.
	"create-skill":     {Write, rDraft},
	"get-skill":        {Read, rRead},
	"list-skills":      {Read, rRead},
	"publish-skill":    {Dangerous, rPublish},
	"save-skill-draft": {Write, rDraft},

	// Spaces.
	"create-space":     {Write, "creates a space"},
	"get-space":        {Read, rRead},
	"get-space-export": {Read, "reads an export's summary (counts and sizes); the export itself is a raw route"},
	"purge-space":      {Dangerous, rDelete},
	"set-space-state":  {Write, rSpaceWrite},
	"update-space":     {Write, rSpaceWrite},

	// Subscriptions.
	"create-subscription":        {Write, rSpaceWrite},
	"delete-subscription":        {Dangerous, rDelete},
	"get-subscription":           {Read, rRead},
	"list-quarantine":            {Read, rRead},
	"list-subscription-delivery": {Read, rRead},
	"list-subscription-replays":  {Read, rRead},
	"list-subscriptions":         {Read, rRead},
	"replay-subscription":        {Write, rSpaceWrite},
	"resolve-quarantine":         {Write, rSpaceWrite},
	"update-subscription":        {Write, rSpaceWrite},

	// System.
	"get-outbound-events": {Read, rRead},

	// Tokens. The step-up writes are excluded by their rule; these entries are what they fall back to.
	"get-root-token":    {Excluded, rTokens},
	"list-all-tokens":   {Excluded, rTokens},
	"revoke-any-token":  {Excluded, rTokens},
	"rotate-root-token": {Excluded, rTokens},
	"revoke-root-token": {Excluded, rTokens},

	// Usage.
	"get-run-usage":   {Read, rRead},
	"get-space-usage": {Read, rRead},

	// Users. The step-up writes are excluded by their rule; these entries are what they fall back to.
	"get-user":             {Excluded, rUsers},
	"list-user-identities": {Excluded, rUsers},
	"list-users":           {Excluded, rUsers},
	"create-user":          {Excluded, rUsers},
	"update-user":          {Excluded, rUsers},
	"set-user-grants":      {Excluded, rUsers},
	"disable-user":         {Excluded, rUsers},
	"enable-user":          {Excluded, rUsers},
	"issue-reset":          {Excluded, rUsers},
	"issue-link-invite":    {Excluded, rUsers},
	"link-user-identity":   {Excluded, rUsers},
	"unlink-user-identity": {Excluded, rUsers},
}

// Classify returns an operation's effective class and reason, and false when the operation is not classified.
func Classify(op string, r api.Rule) (Entry, bool) {
	if why, ok := AutoExcluded(r); ok {
		return Entry{Class: Excluded, Reason: why}, true
	}
	e, ok := Table[op]
	return e, ok
}
