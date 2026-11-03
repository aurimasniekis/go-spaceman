package fleet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Token formats. NodeEnrollment tokens are single use; node credentials authenticate the node's WebSocket. Neither is ever
// accepted by the administrative API (different prefixes, different verifiers).
const (
	EnrollTokenPrefix = "spaceman_en_"
	NodeCredPrefix    = "spaceman_nd_"
	RunTokenPrefix    = "spaceman_rn_"
	// CredentialOverlap is how long the previous credential keeps working after a rotation, so an agent that
	// restarts mid-rotation can still connect and pick up the new one.
	CredentialOverlap = 10 * time.Minute
)

var reNodeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Node is a registered execution node (no secrets).
type Node struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	Description      string             `json:"description"`
	State            string             `json:"state" enum:"pending,active,disabled,revoked"`
	Labels           map[string]string  `json:"labels"`
	AgentVersion     string             `json:"agent_version,omitempty"`
	ProtocolMinor    int64              `json:"protocol_minor"`
	Capabilities     proto.Capabilities `json:"capabilities"`
	Capacity         int64              `json:"capacity"`
	Connected        bool               `json:"connected"`
	LastSeenAt       *time.Time         `json:"last_seen_at,omitempty"`
	LastConnectAt    *time.Time         `json:"last_connect_at,omitempty"`
	LastDisconnectAt *time.Time         `json:"last_disconnect_at,omitempty"`
	Version          int64              `json:"version"`
	CreatedAt        time.Time          `json:"created_at"`
}

func microsPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMicro(v.Int64).UTC()
	return &t
}

func (svc *Service) nodeFromRow(r db.Node) *Node {
	n := &Node{ID: r.ID, Name: r.Name, Description: r.Description, State: r.State, Labels: map[string]string{}, AgentVersion: r.AgentVersion,
		ProtocolMinor: r.ProtocolMinor, Capacity: r.Capacity, LastSeenAt: microsPtr(r.LastSeenAt), LastConnectAt: microsPtr(r.LastConnectAt),
		LastDisconnectAt: microsPtr(r.LastDisconnectAt), Version: r.Version, CreatedAt: time.UnixMicro(r.CreatedAt).UTC()}
	_ = json.Unmarshal([]byte(r.LabelsJson), &n.Labels)
	_ = json.Unmarshal([]byte(r.CapabilitiesJson), &n.Capabilities)
	if n.Capabilities.Harnesses == nil {
		n.Capabilities.Harnesses = []proto.HarnessInfo{}
	}
	if n.Capabilities.Features == nil {
		n.Capabilities.Features = []string{}
	}
	n.Connected = svc.hub.Connected(r.ID)
	return n
}

// CreateNodeInput registers a node.
type CreateNodeInput struct {
	Name        string            `json:"name" doc:"Lowercase DNS-like name, unique"`
	Description string            `json:"description,omitempty" maxLength:"500"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// NodeEnrollment is a node with its one-time enrollment token (shown once).
type NodeEnrollment struct {
	Node      *Node     `json:"node"`
	Token     string    `json:"token" doc:"Single-use; run spaceman-agent enroll with it on the node"`
	ExpiresAt time.Time `json:"expires_at"`
}

func randToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func (svc *Service) mac(token string) ([]byte, error) {
	return svc.kr.MAC(keyring.PurposeToken, []byte("fleet|"+token))
}

func (svc *Service) macs(token string) ([][]byte, error) {
	return svc.kr.MACAll(keyring.PurposeToken, []byte("fleet|"+token))
}

func validLabels(v *apperr.Validation, labels map[string]string) {
	if len(labels) > 32 {
		v.Add("labels", "at most 32 labels")
	}
	for k, val := range labels {
		if !reNodeName.MatchString(k) || len(val) > 128 {
			v.Add("labels", "label %q: keys are lowercase names, values at most 128 bytes", k)
		}
	}
}

// CreateNode registers a node and issues its enrollment token (SM-NODE-001).
func (svc *Service) CreateNode(ctx context.Context, actor audit.Actor, in CreateNodeInput) (*NodeEnrollment, error) {
	v := &apperr.Validation{}
	if !reNodeName.MatchString(in.Name) {
		v.Add("name", "must be a lowercase name (letters, digits, dashes; at most 63)")
	}
	validLabels(v, in.Labels)
	if err := v.Err(); err != nil {
		return nil, err
	}
	token, err := randToken(EnrollTokenPrefix)
	if err != nil {
		return nil, err
	}
	mac, err := svc.mac(token)
	if err != nil {
		return nil, err
	}
	labels, _ := json.Marshal(nonNil(in.Labels))
	id := ids.New(ids.Node)
	var exp time.Time
	err = svc.s.W().TxNamed(ctx, "fleet.create_node", func(tx *store.Tx) error {
		if _, err := tx.Q.GetNodeByName(ctx, in.Name); err == nil {
			return apperr.New(apperr.CodeAlreadyExists, "a node named %q already exists", in.Name)
		}
		if err := tx.Q.InsertNode(ctx, db.InsertNodeParams{ID: id, Name: in.Name, Description: strings.TrimSpace(in.Description), State: "pending",
			LabelsJson: string(labels), CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor), UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		exp = tx.Now.Add(svc.opts.EnrollmentTTL)
		if err := tx.Q.InsertNodeEnrollment(ctx, db.InsertNodeEnrollmentParams{Mac: mac, NodeID: id, Hint: token[len(token)-4:], ExpiresAt: exp.UnixMicro(),
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor)}); err != nil {
			return err
		}
		if err := svc.record(ctx, tx, "", "node", id, "created", 1); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "node.created", Target: audit.Target{Type: "node", ID: id},
			Details: map[string]any{"name": in.Name}})
		return err
	})
	if err != nil {
		return nil, err
	}
	n, err := svc.GetNode(ctx, id)
	if err != nil {
		return nil, err
	}
	return &NodeEnrollment{Node: n, Token: token, ExpiresAt: exp}, nil
}

// ReissueEnrollment issues a new enrollment token for a node that has not enrolled, or to re-enroll a node (the old
// credentials are revoked when the new enrollment is used).
func (svc *Service) ReissueEnrollment(ctx context.Context, actor audit.Actor, nodeID string) (*NodeEnrollment, error) {
	token, err := randToken(EnrollTokenPrefix)
	if err != nil {
		return nil, err
	}
	mac, err := svc.mac(token)
	if err != nil {
		return nil, err
	}
	var exp time.Time
	err = svc.s.W().TxNamed(ctx, "fleet.reissue_enrollment", func(tx *store.Tx) error {
		n, err := tx.Q.GetNode(ctx, nodeID)
		if errors.Is(err, sql.ErrNoRows) {
			return errNodeNotFound
		} else if err != nil {
			return err
		}
		if n.State == "revoked" {
			return apperr.New(apperr.CodePrecondition, "the node is revoked; create a new node")
		}
		if _, err := tx.Q.RevokeNodeEnrollments(ctx, db.RevokeNodeEnrollmentsParams{UsedAt: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, NodeID: nodeID}); err != nil {
			return err
		}
		exp = tx.Now.Add(svc.opts.EnrollmentTTL)
		if err := tx.Q.InsertNodeEnrollment(ctx, db.InsertNodeEnrollmentParams{Mac: mac, NodeID: nodeID, Hint: token[len(token)-4:], ExpiresAt: exp.UnixMicro(),
			CreatedAt: tx.NowMicros(), CreatedBy: actorRef(actor)}); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "node.enrollment.issued", Target: audit.Target{Type: "node", ID: nodeID}})
		return err
	})
	if err != nil {
		return nil, err
	}
	n, err := svc.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return &NodeEnrollment{Node: n, Token: token, ExpiresAt: exp}, nil
}

var (
	errNodeNotFound  = apperr.New(apperr.CodeNotFound, "node not found")
	errBadEnrollment = apperr.New(apperr.CodeUnauthorized, "invalid or expired enrollment token")
	errBadCredential = apperr.New(apperr.CodeUnauthorized, "invalid node credential")
)

// EnrollResult is returned to the agent: its node identity and credential (shown once).
type EnrollResult struct {
	NodeID     string `json:"node_id"`
	NodeName   string `json:"node_name"`
	Credential string `json:"credential"`
}

// Enroll exchanges a single-use enrollment token for a node credential. Previous credentials of the node are revoked
// (re-enrollment replaces the identity material).
func (svc *Service) Enroll(ctx context.Context, token, ip string) (*EnrollResult, error) {
	if !strings.HasPrefix(token, EnrollTokenPrefix) || len(token) != len(EnrollTokenPrefix)+43 {
		return nil, errBadEnrollment
	}
	macs, err := svc.macs(token)
	if err != nil {
		return nil, err
	}
	cred, err := randToken(NodeCredPrefix)
	if err != nil {
		return nil, err
	}
	credMAC, err := svc.mac(cred)
	if err != nil {
		return nil, err
	}
	var res EnrollResult
	err = svc.s.W().TxNamed(ctx, "fleet.enroll", func(tx *store.Tx) error {
		var row db.NodeEnrollment
		found := false
		for _, m := range macs {
			r, err := tx.Q.GetNodeEnrollment(ctx, m)
			if err == nil {
				row, found = r, true
				break
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		if !found {
			return errBadEnrollment
		}
		n, err := tx.Q.UseNodeEnrollment(ctx, db.UseNodeEnrollmentParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, Mac: row.Mac})
		if err != nil {
			return err
		}
		if n != 1 {
			return errBadEnrollment
		}
		node, err := tx.Q.GetNode(ctx, row.NodeID)
		if err != nil {
			return err
		}
		if node.State == "revoked" || node.State == "disabled" {
			return errBadEnrollment
		}
		id := ids.New(ids.NodeCredential)
		if _, err := tx.Q.RevokeNodeCredentials(ctx, db.RevokeNodeCredentialsParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, NodeID: node.ID, ExceptID: id}); err != nil {
			return err
		}
		if err := tx.Q.InsertNodeCredential(ctx, db.InsertNodeCredentialParams{ID: id, NodeID: node.ID, Mac: credMAC, Hint: cred[len(cred)-4:],
			CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		res = EnrollResult{NodeID: node.ID, NodeName: node.Name, Credential: cred}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: node.ID}, Action: "node.enrolled", SourceIP: ip,
			Target: audit.Target{Type: "node", ID: node.ID}, Details: map[string]any{"credential_id": id}})
		return err
	})
	if err != nil {
		return nil, err
	}
	svc.hub.Disconnect(res.NodeID, "re-enrolled")
	return &res, nil
}

// AuthenticateNode resolves a node credential. Revoked and disabled nodes, and revoked or expired credentials, fail.
func (svc *Service) AuthenticateNode(ctx context.Context, cred string) (*db.Node, string, error) {
	if !strings.HasPrefix(cred, NodeCredPrefix) || len(cred) != len(NodeCredPrefix)+43 {
		return nil, "", errBadCredential
	}
	macs, err := svc.macs(cred)
	if err != nil {
		return nil, "", err
	}
	now := svc.now()
	for _, m := range macs {
		c, err := svc.s.R().GetNodeCredentialByMAC(ctx, m)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return nil, "", err
		}
		if c.RevokedAt.Valid || (c.ExpiresAt.Valid && !now.Before(time.UnixMicro(c.ExpiresAt.Int64))) {
			return nil, "", errBadCredential
		}
		n, err := svc.s.R().GetNode(ctx, c.NodeID)
		if err != nil || n.State == "revoked" || n.State == "disabled" {
			return nil, "", errBadCredential
		}
		return &n, c.ID, nil
	}
	return nil, "", errBadCredential
}

// nodeCredentialValid reports whether a connected node's credential is still unrevoked and unexpired and the node
// is neither disabled nor revoked: the periodic re-check of a live connection (SM-AUTH-007). Errors count as valid,
// so a transient database error does not drop healthy nodes; the next check decides.
func (svc *Service) nodeCredentialValid(ctx context.Context, nodeID, credID string) bool {
	n, err := svc.s.R().GetNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	} else if err != nil {
		return true
	}
	if n.State == "revoked" || n.State == "disabled" {
		return false
	}
	creds, err := svc.s.R().ListNodeCredentials(ctx, nodeID)
	if err != nil {
		return true
	}
	now := svc.now()
	for _, c := range creds {
		if c.ID == credID {
			return !c.RevokedAt.Valid && (!c.ExpiresAt.Valid || now.Before(time.UnixMicro(c.ExpiresAt.Int64)))
		}
	}
	return false
}

// RotateCredential issues a new credential for an authenticated node; the old one keeps working for CredentialOverlap.
func (svc *Service) RotateCredential(ctx context.Context, nodeID, currentCredID string) (string, error) {
	cred, err := randToken(NodeCredPrefix)
	if err != nil {
		return "", err
	}
	m, err := svc.mac(cred)
	if err != nil {
		return "", err
	}
	err = svc.s.W().TxNamed(ctx, "fleet.rotate_credential", func(tx *store.Tx) error {
		if err := tx.Q.ExpireNodeCredential(ctx, db.ExpireNodeCredentialParams{ExpiresAt: sql.NullInt64{Int64: tx.Now.Add(CredentialOverlap).UnixMicro(), Valid: true},
			ID: currentCredID}); err != nil {
			return err
		}
		id := ids.New(ids.NodeCredential)
		if err := tx.Q.InsertNodeCredential(ctx, db.InsertNodeCredentialParams{ID: id, NodeID: nodeID, Mac: m, Hint: cred[len(cred)-4:], CreatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		_, err := svc.audit.Append(ctx, tx, audit.Event{Actor: audit.Actor{Type: audit.ActorNode, ID: nodeID}, Action: "node.credential.rotated",
			Target: audit.Target{Type: "node", ID: nodeID}, Details: map[string]any{"credential_id": id}})
		return err
	})
	return cred, err
}

// GetNode returns a node.
func (svc *Service) GetNode(ctx context.Context, id string) (*Node, error) {
	r, err := svc.s.R().GetNode(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNodeNotFound
	} else if err != nil {
		return nil, err
	}
	return svc.nodeFromRow(r), nil
}

// ListNodes returns every node.
func (svc *Service) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := svc.s.R().ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Node, 0, len(rows))
	for _, r := range rows {
		out = append(out, svc.nodeFromRow(r))
	}
	return out, nil
}

// SetNodeState disables, re-enables or permanently revokes a node. Disabling and revoking close its connection at
// once and revoke its credentials (revoked) — the node then cannot renew leases, so its runs stop within the
// lease and disconnect grace (SM-NODE-001, SM-NODE-007).
func (svc *Service) SetNodeState(ctx context.Context, actor audit.Actor, id, state string) (*Node, error) {
	switch state {
	case "active", "disabled", "revoked":
	default:
		return nil, apperr.New(apperr.CodeInvalid, "state must be active, disabled or revoked")
	}
	err := svc.s.W().TxNamed(ctx, "fleet.set_node_state", func(tx *store.Tx) error {
		n, err := tx.Q.GetNode(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return errNodeNotFound
		} else if err != nil {
			return err
		}
		if n.State == "revoked" {
			return apperr.New(apperr.CodePrecondition, "a revoked node cannot change state")
		}
		if _, err := tx.Q.SetNodeState(ctx, db.SetNodeStateParams{State: state, UpdatedAt: tx.NowMicros(), ID: id}); err != nil {
			return err
		}
		if state == "revoked" {
			if _, err := tx.Q.RevokeNodeCredentials(ctx, db.RevokeNodeCredentialsParams{Now: sql.NullInt64{Int64: tx.NowMicros(), Valid: true}, NodeID: id, ExceptID: ""}); err != nil {
				return err
			}
		}
		if err := svc.record(ctx, tx, "", "node", id, "updated", n.Version+1); err != nil {
			return err
		}
		_, err = svc.audit.Append(ctx, tx, audit.Event{Actor: actor, Action: "node." + state, Target: audit.Target{Type: "node", ID: id}})
		return err
	})
	if err != nil {
		return nil, err
	}
	if state != "active" {
		svc.hub.Disconnect(id, "node "+state)
		svc.kickNodeRelocations(ctx, id)
	}
	return svc.GetNode(ctx, id)
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func actorRef(a audit.Actor) string {
	if a.ID == "" {
		return a.Type
	}
	return a.Type + ":" + a.ID
}
