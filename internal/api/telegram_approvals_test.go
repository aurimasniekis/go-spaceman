package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/human"
)

// Unit tests of the Telegram approval rule (ADR-0083 decision 14, TG-7-2): telegramApproval and the armed confirmations
// it consumes. The end-to-end tests through the real API are in internal/app.

const tgAction = "hum_0000000000000000000000000a"

// An armed confirmation is single use, expires after TelegramConfirmTTL and matches only its own link, space, action,
// outcome and version.
func TestApprovalConfirmations(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	c := NewApprovalConfirmations(func() time.Time { return now })
	arm := func() string { return c.Arm("tgl_1", tgSpace, tgAction, "approved", 4) }
	if tok := arm(); !c.consume(tok, "tgl_1", tgSpace, tgAction, "approved", 4) || c.consume(tok, "tgl_1", tgSpace, tgAction, "approved", 4) {
		t.Fatal("not single use")
	}
	for name, try := range map[string]func(string) bool{
		"other link":    func(tok string) bool { return c.consume(tok, "tgl_2", tgSpace, tgAction, "approved", 4) },
		"other space":   func(tok string) bool { return c.consume(tok, "tgl_1", "spc_other", tgAction, "approved", 4) },
		"other action":  func(tok string) bool { return c.consume(tok, "tgl_1", tgSpace, "hum_other", "approved", 4) },
		"other outcome": func(tok string) bool { return c.consume(tok, "tgl_1", tgSpace, tgAction, "denied", 4) },
		"other version": func(tok string) bool { return c.consume(tok, "tgl_1", tgSpace, tgAction, "approved", 5) },
		"unknown token": func(string) bool { return c.consume("feedface", "tgl_1", tgSpace, tgAction, "approved", 4) },
		"empty token":   func(string) bool { return c.consume("", "tgl_1", tgSpace, tgAction, "approved", 4) },
	} {
		tok := arm()
		if try(tok) {
			t.Errorf("%s: confirmed", name)
		}
		if c.consume(tok, "tgl_1", tgSpace, tgAction, "approved", 4) && name != "unknown token" && name != "empty token" {
			t.Errorf("%s: a failed attempt left the token usable", name)
		}
	}
	tok := arm()
	now = now.Add(TelegramConfirmTTL + time.Second)
	if c.consume(tok, "tgl_1", tgSpace, tgAction, "approved", 4) {
		t.Fatal("expired token confirmed")
	}
	if c.Arm("", tgSpace, tgAction, "approved", 4) != "" || c.Arm("tgl_1", tgSpace, tgAction, "approved", 0) != "" {
		t.Fatal("armed an incomplete confirmation")
	}
	// A link holds at most maxConfirmationsPerLink: the oldest makes room.
	first := arm()
	for range maxConfirmationsPerLink {
		now = now.Add(time.Millisecond)
		arm()
	}
	if c.consume(first, "tgl_1", tgSpace, tgAction, "approved", 4) {
		t.Fatal("per-link bound not applied")
	}
	var nilSet *ApprovalConfirmations
	if nilSet.Arm("tgl_1", tgSpace, tgAction, "approved", 4) != "" || nilSet.consume("x", "tgl_1", tgSpace, tgAction, "approved", 4) {
		t.Fatal("nil set")
	}
}

// telegramApproval: advisory approvals pass; enforced ones need the space's opt-in (read for the action's space), the
// chat channel and a confirmation armed for exactly this decision; anything unwired refuses.
func TestTelegramApprovalRule(t *testing.T) {
	p := &auth.Principal{User: &auth.User{ID: "usr_1"}, TelegramLinkID: "tgl_1"}
	a := &human.Action{ID: tgAction, SpaceID: tgSpace, Type: human.TypeApproval}
	confirm := NewApprovalConfirmations(nil)
	var optInAsked []string
	d := &Deps{Telegram: &TelegramPolicy{
		EnforcedApproval: func(_ context.Context, x *human.Action) (bool, error) { return x.DedupKey == "enforced", nil },
		ApprovalsInChat: func(_ context.Context, spaceID string) bool {
			optInAsked = append(optInAsked, spaceID)
			return spaceID == tgSpace
		},
		Confirmations: confirm,
	}}
	chat := context.WithValue(context.Background(), telegramChannelKey{}, channelChat)
	mini := context.WithValue(context.Background(), telegramChannelKey{}, channelMiniApp)
	refused := func(err error, text string) bool { return err != nil && strings.Contains(err.Error(), text) }

	if err := d.telegramApproval(mini, p, a, "approved", 4); err != nil {
		t.Fatalf("advisory: %v", err)
	}
	a.DedupKey = "enforced"
	if err := d.telegramApproval(chat, p, a, "approved", 4); !refused(err, textApprovalConfirm) {
		t.Fatalf("enforced without a confirmation: %v", err)
	}
	tok := confirm.Arm("tgl_1", tgSpace, tgAction, "approved", 4)
	if err := d.telegramApproval(WithApprovalConfirmation(mini, tok), p, a, "approved", 4); !refused(err, textApprovalConfirm) {
		t.Fatalf("enforced from the Mini App: %v", err)
	}
	tok = confirm.Arm("tgl_1", tgSpace, tgAction, "approved", 4)
	if err := d.telegramApproval(WithApprovalConfirmation(chat, tok), p, a, "approved", 4); err != nil {
		t.Fatalf("enforced, confirmed in chat: %v", err)
	}
	if err := d.telegramApproval(WithApprovalConfirmation(chat, tok), p, a, "approved", 4); !refused(err, textApprovalConfirm) {
		t.Fatalf("confirmation reused: %v", err)
	}
	// The opt-in is the action's own space's.
	optInAsked = nil
	b := &human.Action{ID: tgAction, SpaceID: "spc_0000000000000000000000000b", Type: human.TypeApproval, DedupKey: "enforced"}
	tok = confirm.Arm("tgl_1", b.SpaceID, tgAction, "approved", 4)
	if err := d.telegramApproval(WithApprovalConfirmation(chat, tok), p, b, "approved", 4); !refused(err, textApprovalWebOnly) {
		t.Fatalf("enforced without the opt-in: %v", err)
	}
	if len(optInAsked) != 1 || optInAsked[0] != b.SpaceID {
		t.Fatalf("opt-in read for %v", optInAsked)
	}
	// Errors and missing wiring refuse.
	d.Telegram.EnforcedApproval = func(context.Context, *human.Action) (bool, error) { return false, errors.New("db down") }
	if err := d.telegramApproval(chat, p, a, "approved", 4); err == nil {
		t.Fatal("lookup error passed")
	}
	d.Telegram.EnforcedApproval = nil
	if err := d.telegramApproval(chat, p, a, "approved", 4); !refused(err, textApprovalUnwired) {
		t.Fatalf("unwired: %v", err)
	}
	if err := (&Deps{}).telegramApproval(chat, p, a, "approved", 4); !refused(err, textApprovalUnwired) {
		t.Fatalf("no policy: %v", err)
	}
}
