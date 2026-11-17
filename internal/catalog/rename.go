package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Rename refusal reasons (ADR-0093): stable codes carried by a *RenameRefused cause.
const (
	// RenameRoleBusy: the role has live work (runs, waits, switches, queued launches), the space has an unfinished
	// dream or relocation, or an earlier rename of these names has not been moved on the node yet (or, closed, has not
	// reached the node yet).
	RenameRoleBusy = "role_busy"
	// RenameNameTaken: the target name is a role, has a memory collection, or leftovers of a deleted role of that name
	// would collide with the role's grants or pool.
	RenameNameTaken = "name_taken"
	// RenameNodeTooOld: the space's node last negotiated a protocol minor below proto.MinorRoleRename, so it cannot move
	// the role's memory folder. The node may hold memory central has not seen yet, so the rename is refused.
	RenameNodeTooOld = "node_too_old"
)

// Node states of a rename (ADR-0093). A rename on a space with a node starts pending and queues one memory.rename for
// the node. The node's memory.renamed report moves it: ok to done (final), error to failed, deferred back to pending.
// A failed rename becomes done on a later ok. Pending and failed renames are open: they hold launches of both names and
// dream launches in the space, block another rename of either name and a clean relocation of the space (a source_unavailable one closes the rename as
// not_needed), and make central
// refuse memory under both names. A space without a node starts and stays not_needed.
const (
	RenameNodePending   = "pending"
	RenameNodeFailed    = "failed"
	RenameNodeDone      = "done"
	RenameNodeNotNeeded = "not_needed"
	// RenameNodeAbandoned: an admin closed the rename (CloseRoleRename); the node's old folder is orphaned.
	RenameNodeAbandoned = "abandoned"
)

// RenameCommandKey is the idempotency key of a rename's first memory.rename command; a command queued again after the
// node refused one is keyed RenameCommandKey(id) + ":<n>".
func RenameCommandKey(renameID string) string { return "role-rename:" + renameID }

// crossSpaceRequester is the requester role knowledge-base requests from other spaces carry (kb.AskCrossSpace); it
// names no role, so a role is never renamed to it.
const crossSpaceRequester = "cross-space"

// maxRenameHops bounds following a chain of renames (a→b→c…); a longer chain resolves to the name unchanged.
const maxRenameHops = 16

// RenameRefused is the cause of a refused rename: a stable reason and, for a busy role, what to stop or finish first.
type RenameRefused struct {
	Reason   string
	Blockers []string
}

func (e *RenameRefused) Error() string {
	if len(e.Blockers) == 0 {
		return e.Reason
	}
	return e.Reason + ": " + strings.Join(e.Blockers, "; ")
}

// RoleRename is one rename of a space role (a space_role_renames row).
type RoleRename struct {
	ID             string     `json:"id"`
	SpaceID        string     `json:"space_id"`
	From           string     `json:"from"`
	To             string     `json:"to"`
	RenamedAt      time.Time  `json:"renamed_at"`
	RenamedBy      string     `json:"renamed_by"`
	NodeID         string     `json:"node_id,omitempty"`
	NodeState      string     `json:"node_state" enum:"pending,failed,done,not_needed,abandoned"`
	NodeMessage    string     `json:"node_message,omitempty"`
	NodeDoneAt     *time.Time `json:"node_done_at,omitempty"`
	CentralMovedAt *time.Time `json:"central_moved_at,omitempty"`
}

func microsPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func renameOut(r db.SpaceRoleRename) *RoleRename {
	return &RoleRename{ID: r.ID, SpaceID: r.SpaceID, From: r.FromName, To: r.ToName, RenamedAt: time.UnixMicro(r.RenamedAt).UTC(), RenamedBy: r.RenamedBy,
		NodeID: r.NodeID, NodeState: r.NodeState, NodeMessage: r.NodeMessage, NodeDoneAt: microsPtr(r.NodeDoneAt), CentralMovedAt: microsPtr(r.CentralMovedAt)}
}

// roleNameClass says what a column that names a space role means for a rename (ADR-0093 §1.2).
type roleNameClass string

const (
	// roleNameLive: the value is read later to route, launch, authorise or deliver. A rename rewrites it.
	roleNameLive roleNameClass = "live"
	// roleNameHistory: the value records what happened. A rename leaves it as written.
	roleNameHistory roleNameClass = "history"
	// roleNameNotASpaceRole: the column is named role but holds something other than a space role name.
	roleNameNotASpaceRole roleNameClass = "not-a-space-role"
)

// roleNameEntry is the reviewed class of one role-name column and why.
type roleNameEntry struct {
	class  roleNameClass
	reason string
}

// roleNameColumns classifies every column that names a space role (the names the schema guard matches, see
// isRoleNameColumn), keyed "table.column". A column added by a migration fails TestRoleNameColumnsAreClassified until it
// is listed here with its class and a reason. Keep this next to RenameSpaceRole, which must honour the classes. This map
// only checks the classification is complete; what actually enforces the live and history classes is the table-driven
// TestRenameSpaceRoleRewritesLiveRows, which seeds a row of each kind and asserts the live ones are rewritten and the
// history ones are left as written.
var roleNameColumns = map[string]roleNameEntry{
	"account_switches.role":                         {roleNameLive, "open switches block the rename (role_busy); closed ones are history"},
	"approvals.requester_role":                      {roleNameHistory, "who asked, as recorded"},
	"dream_configs.memory_roles_json":               {roleNameLive, "JSON array that may hold *; rewritten in place by rewriteLists"},
	"dream_configs.space_role":                      {roleNameLive, "the dream's role, must name an existing role"},
	"dream_configs.transcript_roles_json":           {roleNameLive, "JSON array that may hold *; rewritten in place by rewriteLists"},
	"human_actions.requester_role":                  {roleNameHistory, "who asked, as recorded"},
	"human_notifications.target_role":               {roleNameLive, "label of a pending notification; rewritten while pending"},
	"human_policies.successor_role":                 {roleNameLive, "LiveRunOfRole fallback routing"},
	"kb_repos.allowed_roles_json":                   {roleNameLive, "JSON array that may hold *; rewritten in place by rewriteLists"},
	"kb_repos.space_role":                           {roleNameLive, "must name an existing role"},
	"kb_requests.requester_role":                    {roleNameLive, "an answer to an open request goes to role:<requester_role>"},
	"kb_sessions.requester_role":                    {roleNameHistory, "who asked, as recorded"},
	"limit_waits.role":                              {roleNameLive, "a resume relaunches with this role; open waits block the rename (role_busy)"},
	"mcp_grants.role":                               {roleNameLive, "read on every gateway call"},
	"memory_collections.role":                       {roleNameLive, "names the central repository on disk"},
	"oidc_group_mappings.role":                      {roleNameNotASpaceRole, "an SSO group's role grant, not a space role"},
	"role_grants.role":                              {roleNameNotASpaceRole, "a Casbin role, not a space role"},
	"run_usage.role":                                {roleNameHistory, "usage as recorded; the ledger keeps the name it ran under"},
	"runs.role":                                     {roleNameHistory, "read live while the run runs (AuthenticateRun); a run is refused while its role is busy"},
	"space_human_type_permissions.agent_roles_json": {roleNameLive, "JSON array; rewritten in place by rewriteLists"},
	"space_repositories.role":                       {roleNameNotASpaceRole, "a repository's role, not a space role name"},
	"space_human_type_permissions.view_role":        {roleNameNotASpaceRole, "the view level a type needs, one of viewer, operator, admin"},
	"space_human_type_permissions.create_role":      {roleNameNotASpaceRole, "the create level a type needs, one of operator, admin"},
	"space_human_type_permissions.respond_role":     {roleNameNotASpaceRole, "the respond level a type needs, one of operator, admin"},
	"telegram_links.max_role":                       {roleNameNotASpaceRole, "the highest Casbin role a Telegram link may use"},
	"space_role_accounts.role":                      {roleNameLive, "the role's account pool"},
	"space_schedules.assignee_role":                 {roleNameLive, "copied into each task the schedule creates"},
	"tasks.assignee_role":                           {roleNameLive, "open tasks follow the role; closed ones are history"},
	"usage_buckets.role":                            {roleNameHistory, "part of the primary key; rewriting could collide"},
	"xspace_requests.requester_role":                {roleNameHistory, "who asked, as recorded"},
}

// RenameSpaceRole renames a space role in place (ADR-0093, SM-PROMPT-009). In one transaction it renames the role row
// and every live reference in the space: MCP grants, KB and dream configuration, schedules, the human policy successor,
// human type permissions, the role's account pool, its memory collection, open tasks, unacknowledged messages to the
// role, open KB requests and pending human notifications. Records of what happened keep the name they were written
// with. A role with live work is refused (role_busy, listing the blockers), as is a target name in use (name_taken).
// ifRevision > 0 must match the role's revision. The rename is recorded in space_role_renames; while its node_state is
// pending, launches of the new name and dream launches in the space are held (fleet.LaunchRun).
//
// The transaction does no file or network I/O, and its body can run again (a BUSY retry) with the same result.
func (svc *Service) RenameSpaceRole(ctx context.Context, actor audit.Actor, spaceID, from, to string, ifRevision int64) (*SpaceRole, *RoleRename, error) {
	v := &apperr.Validation{}
	if !reRoleName.MatchString(to) {
		v.Add("new_name", "must be a lowercase role name, e.g. implementor-backend")
	} else if to == crossSpaceRequester {
		v.Add("new_name", "is reserved: knowledge-base requests from other spaces carry it")
	} else if from == to {
		v.Add("new_name", "is the role's current name")
	}
	if err := v.Err(); err != nil {
		return nil, nil, err
	}
	id := ids.New(ids.RoleRename)
	err := svc.s.W().TxNamed(ctx, "catalog.space_role_rename", func(tx *store.Tx) error {
		return svc.renameTx(ctx, tx, actor, id, spaceID, from, to, ifRevision)
	})
	if err != nil {
		return nil, nil, err
	}
	if svc.AfterRename != nil {
		if rn, err := svc.s.R().GetSpaceRoleRename(ctx, id); err == nil {
			svc.AfterRename(ctx, *renameOut(rn))
		}
	}
	r, err := svc.s.R().GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: to})
	if err != nil {
		return nil, nil, err
	}
	rn, err := svc.s.R().GetSpaceRoleRename(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return spaceRoleOut(r), renameOut(rn), nil
}

// renameTx is the rename transaction's body. It keeps no state between attempts: everything it writes is derived from
// what it reads in tx.
func (svc *Service) renameTx(ctx context.Context, tx *store.Tx, actor audit.Actor, id, spaceID, from, to string, ifRevision int64) error {
	q := tx.Q
	sp, err := q.GetSpace(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return apperr.New(apperr.CodeNotFound, "space not found")
	} else if err != nil {
		return err
	}
	role, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: from})
	if errors.Is(err, sql.ErrNoRows) {
		if cur, rerr := CurrentRoleName(ctx, q, spaceID, from); rerr == nil && cur != from {
			return apperr.New(apperr.CodeNotFound, "space role not found; it was renamed to %s", cur)
		}
		return apperr.New(apperr.CodeNotFound, "space role not found")
	} else if err != nil {
		return err
	}
	if ifRevision > 0 && role.Revision != ifRevision {
		return apperr.New(apperr.CodePrecondition, "the role changed since revision %d (now %d)", ifRevision, role.Revision)
	}
	blockers, err := renameBlockers(ctx, q, sp, from, to)
	if err != nil {
		return err
	}
	if len(blockers) > 0 {
		return apperr.Wrap(apperr.CodePrecondition, &RenameRefused{Reason: RenameRoleBusy, Blockers: blockers},
			"the role %s is busy (%s); stop or finish first: %s", from, RenameRoleBusy, strings.Join(blockers, "; "))
	}
	taken, err := renameTaken(ctx, q, spaceID, from, to)
	if err != nil {
		return err
	}
	if len(taken) > 0 {
		return apperr.Wrap(apperr.CodeAlreadyExists, &RenameRefused{Reason: RenameNameTaken, Blockers: taken},
			"the name %s is taken (%s): %s", to, RenameNameTaken, strings.Join(taken, "; "))
	}
	if err := renameNodeReady(ctx, q, sp); err != nil {
		return err
	}

	now := tx.NowMicros()
	rewritten := map[string]int64{}
	changed := []change{}
	if n, err := q.RenameSpaceRoleRow(ctx, db.RenameSpaceRoleRowParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from}); err != nil {
		return err
	} else if n != 1 {
		return apperr.New(apperr.CodeConflict, "the role changed concurrently")
	}
	if err := svc.rewriteColumns(ctx, tx, spaceID, from, to, actor, rewritten, &changed); err != nil {
		return err
	}
	if err := rewriteLists(ctx, tx, spaceID, from, to, actor, rewritten, &changed); err != nil {
		return err
	}

	state := RenameNodePending
	if sp.NodeID == "" {
		state = RenameNodeNotNeeded
	}
	if err := q.InsertSpaceRoleRename(ctx, db.InsertSpaceRoleRenameParams{ID: id, SpaceID: spaceID, FromName: from, ToName: to, RenamedAt: now,
		RenamedBy: actor.ID, NodeID: sp.NodeID, NodeState: state}); err != nil {
		return err
	}
	if state == RenameNodePending {
		if err := QueueMemoryRename(ctx, tx, sp, id, RenameCommandKey(id), from, to); err != nil {
			return err
		}
	}
	if err := svc.changes.Record(ctx, tx, spaceID, "space", spaceID, "updated", 0); err != nil {
		return err
	}
	for _, c := range changed {
		if err := svc.changes.Record(ctx, tx, spaceID, c.typ, c.id, "updated", c.version); err != nil {
			return err
		}
	}
	_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "space.role_renamed", Target: audit.Target{Type: "space", ID: spaceID},
		Details: map[string]any{"from": from, "to": to, "rename_id": id, "node_id": sp.NodeID, "node_state": state, "rewritten": rewritten}})
	return err
}

// change is one change-feed record the rename writes for a rewritten row.
type change struct {
	typ, id string
	version int64
}

// renameBlockers lists what keeps a rename from going ahead (ADR-0093 decision 2): live runs of the role, its open
// limit waits and account switches, launches of it the node has not accepted, unfinished dreams and pending dream
// commits in the space, a relocation, an earlier rename of either name whose node move is still pending, and a closed
// rename of either name whose memory.rename the node has not journaled yet.
func renameBlockers(ctx context.Context, q *db.Queries, sp db.Space, from, to string) ([]string, error) {
	var out []string
	runs, err := q.RoleRenameLiveRuns(ctx, db.RoleRenameLiveRunsParams{SpaceID: sp.ID, Role: from})
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		out = append(out, fmt.Sprintf("run %s (%s)", r.ID, r.Observed))
	}
	waits, err := q.RoleRenameOpenLimitWaits(ctx, db.RoleRenameOpenLimitWaitsParams{SpaceID: sp.ID, Role: from})
	if err != nil {
		return nil, err
	}
	for _, w := range waits {
		out = append(out, fmt.Sprintf("limit wait %s (%s)", w.ID, w.State))
	}
	sws, err := q.RoleRenameOpenAccountSwitches(ctx, db.RoleRenameOpenAccountSwitchesParams{SpaceID: sp.ID, Role: from})
	if err != nil {
		return nil, err
	}
	for _, s := range sws {
		out = append(out, fmt.Sprintf("account switch %s (%s)", s.ID, s.State))
	}
	launches, err := q.RoleRenameQueuedLaunches(ctx, db.RoleRenameQueuedLaunchesParams{SpaceID: sp.ID, Role: from})
	if err != nil {
		return nil, err
	}
	for _, c := range launches {
		out = append(out, fmt.Sprintf("queued launch %s (%s)", c.ID, c.State))
	}
	dreams, err := q.RoleRenameOpenDreamRuns(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	for _, d := range dreams {
		out = append(out, fmt.Sprintf("dream run %s (%s)", d.ID, d.State))
	}
	commits, err := q.RoleRenamePendingDreamCommits(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	for _, c := range commits {
		out = append(out, fmt.Sprintf("dream commit %s (%s)", c.ID, c.State))
	}
	if sp.DesiredNodeID.Valid {
		out = append(out, "relocation (the space is moving to another node)")
	} else if rel, err := q.GetActiveRelocation(ctx, sp.ID); err == nil {
		out = append(out, fmt.Sprintf("relocation %s (%s)", rel.ID, rel.State))
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	pending, err := q.ListOpenSpaceRoleRenames(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range pending {
		if p.FromName == from || p.ToName == from || p.FromName == to || p.ToName == to {
			out = append(out, fmt.Sprintf("rename %s (%s to %s) waiting for node %s to move its memory (%s)", p.ID, p.FromName, p.ToName, p.NodeID, p.NodeState))
		}
	}
	// A closed rename whose command the node has not journaled yet still blocks both names: the node would get this
	// rename's move and the closed one's in any order (ADR-0093 addendum, ordering). Close with forget_node releases them.
	closed, err := q.ListUndeliveredClosedRoleRenames(ctx, sp.ID)
	if err != nil {
		return nil, err
	}
	for _, p := range closed {
		if p.FromName == from || p.ToName == from || p.FromName == to || p.ToName == to {
			out = append(out, fmt.Sprintf("closed rename %s (%s to %s) not yet delivered to node %s (close it with forget_node to release the names)",
				p.ID, p.FromName, p.ToName, p.NodeID))
		}
	}
	return out, nil
}

// renameNodeReady refuses a rename whose space's node cannot move the role's memory folder: one that last negotiated a
// protocol minor below proto.MinorRoleRename (or never connected). Central cannot skip the node's move, since the node may
// hold history of the role that central has not received yet.
func renameNodeReady(ctx context.Context, q *db.Queries, sp db.Space) error {
	if sp.NodeID == "" {
		return nil
	}
	n, err := q.GetNode(ctx, sp.NodeID)
	if err != nil {
		return err
	}
	if n.ProtocolMinor >= proto.MinorRoleRename {
		return nil
	}
	return apperr.Wrap(apperr.CodePrecondition, &RenameRefused{Reason: RenameNodeTooOld},
		"the space's node %s last spoke node protocol 1.%d; renaming a role needs 1.%d, which moves the role's memory on the node (%s): "+
			"upgrade its spaceman-agent and let it connect first", n.Name, n.ProtocolMinor, proto.MinorRoleRename, RenameNodeTooOld)
}

// QueueMemoryRename queues the node's memory.rename for a rename, inside the rename's (or a re-queue's) transaction. The
// command is execution in the space, so it carries a lease generation: the current lease's, or 1 before any lease. The
// hub sends it at the space's lease generation at the time it is delivered (a lease granted again after the node was
// away has a newer one), so the stored generation only orders it.
func QueueMemoryRename(ctx context.Context, tx *store.Tx, sp db.Space, renameID, key, from, to string) error {
	return queueMemoryRename(ctx, tx, sp, proto.MemoryRename{RenameID: renameID, SpaceID: sp.ID, From: from, To: to}, key)
}

func queueMemoryRename(ctx context.Context, tx *store.Tx, sp db.Space, p proto.MemoryRename, key string) error {
	gen := int64(1)
	if l, err := tx.Q.GetLease(ctx, sp.ID); err == nil {
		gen = l.Generation
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return tx.Q.InsertNodeCommand(ctx, db.InsertNodeCommandParams{ID: ids.New(ids.Command), NodeID: sp.NodeID, SpaceID: sp.ID, Generation: gen,
		Kind: proto.CmdMemoryRename, IdempotencyKey: key, PayloadJson: string(b), CreatedAt: tx.NowMicros()})
}

// renameTaken lists why the target name is not free (ADR-0093 decision 3): a role has it, a memory collection has it
// (existing memory is never merged), or leftovers of a deleted role of that name collide with the role's MCP grants
// (one per server) or account pool (one per account).
func renameTaken(ctx context.Context, q *db.Queries, spaceID, from, to string) ([]string, error) {
	var out []string
	if _, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: to}); err == nil {
		out = append(out, "a role "+to+" exists")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if c, err := q.GetMemoryCollectionByRole(ctx, db.GetMemoryCollectionByRoleParams{SpaceID: spaceID, Role: to}); err == nil {
		out = append(out, "memory collection "+c.ID+" is named "+to)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	servers, err := q.RoleRenameGrantCollisions(ctx, db.RoleRenameGrantCollisionsParams{ToName: to, SpaceID: spaceID, FromName: from})
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		out = append(out, "an MCP grant of server "+s+" to "+to+" exists")
	}
	accounts, err := q.RoleRenamePoolCollisions(ctx, db.RoleRenamePoolCollisionsParams{ToName: to, SpaceID: spaceID, FromName: from})
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		out = append(out, "account "+a+" is in the pool of "+to)
	}
	return out, nil
}

// rewriteColumns renames the plain-column references.
func (svc *Service) rewriteColumns(ctx context.Context, tx *store.Tx, spaceID, from, to string, actor audit.Actor, rewritten map[string]int64, changed *[]change) error {
	q, now := tx.Q, tx.NowMicros()
	grants, err := q.RenameRoleMCPGrants(ctx, db.RenameRoleMCPGrantsParams{ToName: to, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, g := range grants {
		if !seen[g.ServerID] {
			seen[g.ServerID] = true
			*changed = append(*changed, change{"mcp_server", g.ServerID, 0})
		}
	}
	count(rewritten, "mcp_grants", int64(len(grants)))

	kbs, err := q.RenameRoleKBRepos(ctx, db.RenameRoleKBReposParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	for _, r := range kbs {
		*changed = append(*changed, change{"kb", r.ID, r.Version})
	}
	count(rewritten, "kb_repos.space_role", int64(len(kbs)))

	dreams, err := q.RenameRoleDreamConfigs(ctx, db.RenameRoleDreamConfigsParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	for _, r := range dreams {
		*changed = append(*changed, change{"dream", r.ID, r.Version})
	}
	count(rewritten, "dream_configs.space_role", int64(len(dreams)))

	scheds, err := q.RenameRoleSchedules(ctx, db.RenameRoleSchedulesParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	for _, r := range scheds {
		*changed = append(*changed, change{"schedule", r.ID, r.Version})
	}
	count(rewritten, "space_schedules", int64(len(scheds)))

	pols, err := q.RenameRoleHumanPolicies(ctx, db.RenameRoleHumanPoliciesParams{ToName: to, Now: now, By: actor.ID,
		SpaceID: sql.NullString{String: spaceID, Valid: true}, FromName: from})
	if err != nil {
		return err
	}
	for _, r := range pols {
		*changed = append(*changed, change{"human_policy", r.ID, 0})
	}
	count(rewritten, "human_policies", int64(len(pols)))

	n, err := q.RenameRoleAccounts(ctx, db.RenameRoleAccountsParams{ToName: to, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	count(rewritten, "space_role_accounts", n)

	mems, err := q.RenameRoleMemoryCollection(ctx, db.RenameRoleMemoryCollectionParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	for _, id := range mems {
		*changed = append(*changed, change{"memory", id, 0})
	}
	count(rewritten, "memory_collections", int64(len(mems)))

	tasks, err := q.RenameRoleOpenTasks(ctx, db.RenameRoleOpenTasksParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	for _, r := range tasks {
		*changed = append(*changed, change{"task", r.ID, r.Version})
	}
	count(rewritten, "tasks", int64(len(tasks)))

	msgs, err := q.RenameRolePendingMessages(ctx, db.RenameRolePendingMessagesParams{ToName: to, SpaceID: spaceID, FromName: from})
	if err != nil {
		return err
	}
	for _, r := range msgs {
		*changed = append(*changed, change{"message", r.ID, r.Version})
	}
	count(rewritten, "messages", int64(len(msgs)))

	if n, err = q.RenameRoleOpenKBRequests(ctx, db.RenameRoleOpenKBRequestsParams{ToName: to, Now: now, SpaceID: spaceID, FromName: from}); err != nil {
		return err
	}
	count(rewritten, "kb_requests", n)

	if n, err = q.RenameRolePendingHumanNotifications(ctx, db.RenameRolePendingHumanNotificationsParams{ToName: to, SpaceID: spaceID, FromName: from}); err != nil {
		return err
	}
	count(rewritten, "human_notifications", n)
	return nil
}

// rewriteLists renames the role inside the JSON arrays of role names (KB allowed roles, dream memory and transcript
// roles, human type permission agent roles), in Go so the arrays stay what a save writes.
func rewriteLists(ctx context.Context, tx *store.Tx, spaceID, from, to string, actor audit.Actor, rewritten map[string]int64, changed *[]change) error {
	q, now := tx.Q, tx.NowMicros()
	kbs, err := q.RoleListsKBRepos(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, r := range kbs {
		next, ok := renameInList(r.AllowedRolesJson, from, to)
		if !ok {
			continue
		}
		v, err := q.SetKBRepoAllowedRoles(ctx, db.SetKBRepoAllowedRolesParams{AllowedRolesJson: next, UpdatedAt: now, ID: r.ID})
		if err != nil {
			return err
		}
		*changed = append(*changed, change{"kb", r.ID, v})
		count(rewritten, "kb_repos.allowed_roles_json", 1)
	}
	dreams, err := q.RoleListsDreamConfigs(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, r := range dreams {
		mem, ok1 := renameInList(r.MemoryRolesJson, from, to)
		trn, ok2 := renameInList(r.TranscriptRolesJson, from, to)
		if !ok1 && !ok2 {
			continue
		}
		v, err := q.SetDreamConfigRoles(ctx, db.SetDreamConfigRolesParams{MemoryRolesJson: mem, TranscriptRolesJson: trn, UpdatedAt: now, ID: r.ID})
		if err != nil {
			return err
		}
		*changed = append(*changed, change{"dream", r.ID, v})
		count(rewritten, "dream_configs.roles_json", 1)
	}
	perms, err := q.RoleListsHumanTypePermissions(ctx, spaceID)
	if err != nil {
		return err
	}
	for _, r := range perms {
		next, ok := renameInList(r.AgentRolesJson, from, to)
		if !ok {
			continue
		}
		if _, err := q.SetHumanTypePermissionAgentRoles(ctx, db.SetHumanTypePermissionAgentRolesParams{AgentRolesJson: next, UpdatedAt: now, UpdatedBy: actor.ID,
			SpaceID: spaceID, ActionType: r.ActionType}); err != nil {
			return err
		}
		count(rewritten, "space_human_type_permissions", 1)
	}
	return nil
}

// renameInList replaces from with to in a JSON array of role names: the order is kept, "*" and other names are left
// alone, and a name that then appears twice keeps its first place. It reports whether the list changed; a list that
// does not parse is left alone.
func renameInList(list, from, to string) (string, bool) {
	var in []string
	if err := json.Unmarshal([]byte(list), &in); err != nil {
		return list, false
	}
	hit := false
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, r := range in {
		if r == from {
			r, hit = to, true
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if !hit {
		return list, false
	}
	b, _ := json.Marshal(out)
	return string(b), true
}

func count(m map[string]int64, table string, n int64) {
	if n > 0 {
		m[table] += n
	}
}

// RoleNameReader is what resolving role names reads: the store's reader or a transaction's queries.
type RoleNameReader interface {
	GetSpaceRole(ctx context.Context, arg db.GetSpaceRoleParams) (db.SpaceRole, error)
	LatestSpaceRoleRenameFrom(ctx context.Context, arg db.LatestSpaceRoleRenameFromParams) (db.SpaceRoleRename, error)
	NextSpaceRoleRenameFrom(ctx context.Context, arg db.NextSpaceRoleRenameFromParams) (db.SpaceRoleRename, error)
	ListSpaceRoleRenamesTo(ctx context.Context, arg db.ListSpaceRoleRenamesToParams) ([]db.SpaceRoleRename, error)
}

// CurrentRoleName resolves a role name taken from a history row (a run, a past control) to the role's current name
// (ADR-0093 decision 5). A name a space role has is never redirected. Otherwise it follows the latest rename away from
// the name and then, in time order, each later rename of the name it reached: the chain the role itself went through,
// so a name reused on the way (a→b, b→c, then a new role b) does not capture it. The end of the chain is the answer
// when a role has that name; a chain that leads nowhere (the role was deleted, more than 16 hops) gives the name back
// unchanged.
func CurrentRoleName(ctx context.Context, q RoleNameReader, spaceID, name string) (string, error) {
	if name == "" {
		return name, nil
	}
	if _, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: name}); err == nil {
		return name, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return name, err
	}
	r, err := q.LatestSpaceRoleRenameFrom(ctx, db.LatestSpaceRoleRenameFromParams{SpaceID: spaceID, FromName: name})
	if errors.Is(err, sql.ErrNoRows) {
		return name, nil
	} else if err != nil {
		return name, err
	}
	for range maxRenameHops {
		next, err := q.NextSpaceRoleRenameFrom(ctx, db.NextSpaceRoleRenameFromParams{SpaceID: spaceID, FromName: r.ToName, AfterAt: r.RenamedAt, AfterID: r.ID})
		if err == nil {
			r = next // renames only move forward in time, so the walk has no cycle
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return name, err
		}
		if _, err := q.GetSpaceRole(ctx, db.GetSpaceRoleParams{SpaceID: spaceID, Name: r.ToName}); err == nil {
			return r.ToName, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return name, err
		}
		return name, nil
	}
	return name, nil
}

// FormerRoleNames lists the names that resolve to the role name through renames (CurrentRoleName gives name for each):
// the role's earlier names, never a name a role has now. It is empty when the role was never renamed.
func FormerRoleNames(ctx context.Context, q RoleNameReader, spaceID, name string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{name: true}
	next := []string{name}
	for hop := 0; hop < maxRenameHops && len(next) > 0; hop++ {
		var more []string
		for _, n := range next {
			rs, err := q.ListSpaceRoleRenamesTo(ctx, db.ListSpaceRoleRenamesToParams{SpaceID: spaceID, ToName: n})
			if err != nil {
				return nil, err
			}
			for _, r := range rs {
				if seen[r.FromName] {
					continue
				}
				seen[r.FromName] = true
				more = append(more, r.FromName) // walk on through a name reused since: an older name behind it may still lead here
				cur, err := CurrentRoleName(ctx, q, spaceID, r.FromName)
				if err != nil {
					return nil, err
				}
				if cur == name {
					out = append(out, r.FromName)
				}
			}
		}
		next = more
	}
	return out, nil
}

// CurrentRoleName resolves a former role name to the current one (see the package function), reading the store.
func (svc *Service) CurrentRoleName(ctx context.Context, spaceID, name string) (string, error) {
	return CurrentRoleName(ctx, svc.s.R(), spaceID, name)
}

// ListRoleRenames returns a space's role renames, newest first.
func (svc *Service) ListRoleRenames(ctx context.Context, spaceID string) ([]*RoleRename, error) {
	rows, err := svc.s.R().ListSpaceRoleRenames(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	out := make([]*RoleRename, 0, len(rows))
	for _, r := range rows {
		out = append(out, renameOut(r))
	}
	return out, nil
}
