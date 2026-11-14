package miniapp

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

var grantA = DownloadGrant{LinkID: "tgl_a", SpaceID: "spc_1", AttachmentID: "att_1"}

func TestDownloadIDShapeAndStorage(t *testing.T) {
	d := NewDownloads(clock.NewFake(time.Unix(1000, 0)))
	id, err := d.Mint(grantA)
	if err != nil || !ValidDownloadID(id) {
		t.Fatalf("Mint = %q, %v", id, err)
	}
	other, _ := d.Mint(grantA)
	if other == id {
		t.Fatal("two mints gave the same ID")
	}
	// The ID is never a key of the store: only its hash is held.
	for h := range d.entries {
		if strings.Contains(string(h[:]), id) {
			t.Fatal("plain ID held")
		}
	}
	for _, bad := range []string{"", "short", id + "x", strings.Repeat("!", downloadIDLen), id[:42] + "="} {
		if _, err := d.Redeem(bad); !errors.Is(err, ErrDownloadGone) {
			t.Errorf("Redeem(%q) = %v, want gone", bad, err)
		}
	}
	// A well-formed ID that was never minted.
	if _, err := d.Redeem(strings.Repeat("A", downloadIDLen)); !errors.Is(err, ErrDownloadGone) {
		t.Errorf("unknown ID: %v", err)
	}
}

func TestDownloadBindingIsReturned(t *testing.T) {
	d := NewDownloads(clock.NewFake(time.Unix(1000, 0)))
	a, _ := d.Mint(grantA)
	b, _ := d.Mint(DownloadGrant{LinkID: "tgl_b", SpaceID: "spc_2", AttachmentID: "att_2"})
	if g, err := d.Redeem(a); err != nil || g != grantA {
		t.Fatalf("a = %+v, %v", g, err)
	}
	if g, err := d.Redeem(b); err != nil || g.LinkID != "tgl_b" || g.AttachmentID != "att_2" || g.SpaceID != "spc_2" {
		t.Fatalf("b = %+v, %v", g, err)
	}
}

func TestDownloadUseCount(t *testing.T) {
	d := NewDownloads(clock.NewFake(time.Unix(1000, 0)))
	id, _ := d.Mint(grantA)
	for i := 0; i < DownloadMaxUses; i++ {
		if _, err := d.Redeem(id); err != nil {
			t.Fatalf("use %d: %v", i+1, err)
		}
	}
	if _, err := d.Redeem(id); !errors.Is(err, ErrDownloadGone) {
		t.Fatalf("use %d = %v, want gone", DownloadMaxUses+1, err)
	}
	if d.Len() != 0 {
		t.Fatalf("used-up grant kept: %d", d.Len())
	}
}

func TestDownloadExpiry(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0))
	d := NewDownloads(clk)
	id, _ := d.Mint(grantA)
	clk.Advance(DownloadTTL - time.Second)
	if _, err := d.Redeem(id); err != nil {
		t.Fatalf("just before the TTL: %v", err)
	}
	clk.Advance(time.Second) // exactly 60 s after minting
	if _, err := d.Redeem(id); !errors.Is(err, ErrDownloadGone) {
		t.Fatalf("at the TTL = %v, want gone", err)
	}
	if d.Len() != 0 {
		t.Fatal("expired grant kept")
	}
}

func TestDownloadSweepAndCap(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0))
	d := NewDownloads(clk)
	for i := 0; i < 5; i++ {
		_, _ = d.Mint(grantA)
	}
	clk.Advance(DownloadTTL)
	_, _ = d.Mint(grantA) // sweeps the expired
	if d.Len() != 1 {
		t.Fatalf("Len = %d after sweep, want 1", d.Len())
	}
	first, _ := d.Mint(grantA)
	for i := 0; i < downloadLimit; i++ {
		_, _ = d.Mint(grantA)
	}
	if d.Len() > downloadLimit {
		t.Fatalf("Len = %d over the cap", d.Len())
	}
	if _, err := d.Redeem(first); !errors.Is(err, ErrDownloadGone) {
		t.Fatal("the oldest grant survived the cap")
	}
}
