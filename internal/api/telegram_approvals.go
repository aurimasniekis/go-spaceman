package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// Approvals decided from Telegram (ADR-0083 decision 14, plan §2.4, TG-7-2). The rule is the server's, read at request
// time; the chat only renders it:
//
//   - An advisory approval (no enforced approval behind the action) may be decided from Telegram like on the web:
//     approval:decide, and a denial needs a note (the human service).
//   - An enforced approval (ADR-0024) is refused from Telegram unless the action's space has enforced_approvals =
//     confirm, read from the store for every request (TelegramPolicy.ApprovalsInChat). With it, the decision must come
//     from the bot in process (never the Mini App over HTTP) and carry a confirmation the chat armed at an earlier step
//     for the same link, space, action, outcome and version (ApprovalConfirmations): single use, expiring after
//     TelegramConfirmTTL.
//   - Cancelling an approval stays refused from Telegram (it finalizes the action: an enforced approval is revoked).
//
// The approver re-check before an approved operation runs (SM-APPR-003, approverStillAuthorized) applies either way.

// TelegramConfirmTTL is how long an armed confirmation stays valid: the chat's pending-state TTL (plan §4.5).
const TelegramConfirmTTL = 15 * time.Minute

// Bounds of the armed confirmations, so a link can't grow them without limit.
const (
	maxConfirmationsPerLink = 10
	maxConfirmations        = 10000
)

// ApprovalConfirmations holds the confirmations the chat armed for enforced approvals: the first step of the two-step
// decision. Only code inside central reaches it (the chat's Arm, the respond rule's consume); nothing a client sends
// can arm one. In memory only (single central, SM-INV-001): a restart drops them, which just means "tap again".
type ApprovalConfirmations struct {
	now func() time.Time

	mu sync.Mutex
	m  map[string]armedConfirmation
}

type armedConfirmation struct {
	linkID, spaceID, actionID, outcome string
	version                            int64
	at                                 time.Time
}

// NewApprovalConfirmations returns an empty set; now is its clock (nil: the system clock).
func NewApprovalConfirmations(now func() time.Time) *ApprovalConfirmations {
	if now == nil {
		now = time.Now
	}
	return &ApprovalConfirmations{now: now, m: map[string]armedConfirmation{}}
}

// Arm records the first step of deciding an enforced approval from the chat (the link tapped Approve, or Deny) and
// returns the token the second step passes with WithApprovalConfirmation. A link's oldest confirmation makes room for
// a new one past maxConfirmationsPerLink; past maxConfirmations nothing is armed ("" never confirms).
func (c *ApprovalConfirmations) Arm(linkID, spaceID, actionID, outcome string, version int64) string {
	if c == nil || linkID == "" || spaceID == "" || actionID == "" || outcome == "" || version <= 0 {
		return ""
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	tok := hex.EncodeToString(b[:])
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	var oldest string
	var oldestAt time.Time
	n := 0
	for k, a := range c.m {
		switch {
		case now.Sub(a.at) > TelegramConfirmTTL:
			delete(c.m, k)
		case a.linkID == linkID:
			n++
			if oldest == "" || a.at.Before(oldestAt) {
				oldest, oldestAt = k, a.at
			}
		}
	}
	if n >= maxConfirmationsPerLink {
		delete(c.m, oldest)
	}
	if len(c.m) >= maxConfirmations {
		return ""
	}
	c.m[tok] = armedConfirmation{linkID: linkID, spaceID: spaceID, actionID: actionID, outcome: outcome, version: version, at: now}
	return tok
}

// consume reports whether tok was armed for exactly this decision and is still fresh. A token is used up by any attempt,
// matching or not.
func (c *ApprovalConfirmations) consume(tok, linkID, spaceID, actionID, outcome string, version int64) bool {
	if c == nil || tok == "" {
		return false
	}
	c.mu.Lock()
	a, ok := c.m[tok]
	delete(c.m, tok)
	c.mu.Unlock()
	return ok && c.now().Sub(a.at) <= TelegramConfirmTTL && a.linkID == linkID && a.spaceID == spaceID && a.actionID == actionID &&
		a.outcome == outcome && a.version == version
}

type approvalConfirmKey struct{}

// WithApprovalConfirmation returns ctx carrying the token of an armed confirmation, for the chat's second step. It is
// honoured only on the chat channel (InProcessHandler); a context never crosses HTTP, so the Mini App can't send one.
func WithApprovalConfirmation(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, approvalConfirmKey{}, tok)
}

// ApprovalConfirmationFrom returns the token WithApprovalConfirmation put on ctx ("" when none).
func ApprovalConfirmationFrom(ctx context.Context) string {
	t, _ := ctx.Value(approvalConfirmKey{}).(string)
	return t
}

// Refusal texts of approvals from Telegram.
const (
	textApprovalWebOnly = "enforced approvals in this space are decided on the web, not from Telegram"
	textApprovalConfirm = "an enforced approval is decided from Telegram only in the chat, with a second confirmation"
	textApprovalUnwired = "approvals are decided on the web, not from Telegram"
)

// telegramApproval is the rule for a Telegram principal deciding approval action a with outcome at version (the
// If-Match sent). Nil: the decision may go on to the approval:decide check and the human service.
func (d *Deps) telegramApproval(ctx context.Context, p *auth.Principal, a *human.Action, outcome string, version int64) error {
	tp := d.Telegram
	if tp == nil || tp.EnforcedApproval == nil {
		return apperr.New(apperr.CodeForbidden, textApprovalUnwired)
	}
	enforced, err := tp.EnforcedApproval(ctx, a)
	if err != nil {
		return err
	}
	if !enforced {
		return nil
	}
	// The opt-in of the action's own space (the path's space, which the action was read from), read now.
	if tp.ApprovalsInChat == nil || !tp.ApprovalsInChat(ctx, a.SpaceID) {
		return apperr.New(apperr.CodeForbidden, textApprovalWebOnly)
	}
	if telegramChannelFrom(ctx) != channelChat ||
		!tp.Confirmations.consume(ApprovalConfirmationFrom(ctx), p.TelegramLinkID, a.SpaceID, a.ID, outcome, version) {
		return apperr.New(apperr.CodeForbidden, textApprovalConfirm)
	}
	return nil
}
