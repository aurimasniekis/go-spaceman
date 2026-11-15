package miniapp

import (
	"errors"
	"strconv"
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
}

// One link minting past its cap drops its own oldest grants and never another link's.
func TestDownloadPerLinkCapNeverEvictsOthers(t *testing.T) {
	d := NewDownloads(clock.NewFake(time.Unix(1000, 0)))
	grantB := DownloadGrant{LinkID: "tgl_b", SpaceID: "spc_1", AttachmentID: "att_1"}
	b, err := d.Mint(grantB)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < downloadPerLink*3; i++ {
		id, err := d.Mint(grantA)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if n := len(d.byLink["tgl_a"]); n != downloadPerLink {
		t.Fatalf("link a holds %d grants, want %d", n, downloadPerLink)
	}
	if d.Len() != downloadPerLink+1 {
		t.Fatalf("Len = %d, want %d", d.Len(), downloadPerLink+1)
	}
	if _, err := d.Redeem(ids[0]); !errors.Is(err, ErrDownloadGone) {
		t.Error("the link's oldest grant survived its cap")
	}
	if _, err := d.Redeem(ids[len(ids)-1]); err != nil {
		t.Errorf("the link's newest grant: %v", err)
	}
	if g, err := d.Redeem(b); err != nil || g != grantB {
		t.Errorf("another link's grant was evicted: %v", err)
	}
}

// At the global cap Mint refuses rather than evict a live grant; room comes back when grants expire or are used up.
func TestDownloadGlobalCapRefuses(t *testing.T) {
	clk := clock.NewFake(time.Unix(1000, 0))
	d := NewDownloads(clk)
	first, err := d.Mint(DownloadGrant{LinkID: "tgl_0", SpaceID: "s", AttachmentID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < downloadLimit; i++ {
		if _, err := d.Mint(DownloadGrant{LinkID: "tgl_" + strconv.Itoa(i/downloadPerLink), SpaceID: "s", AttachmentID: "a"}); err != nil {
			t.Fatalf("mint %d: %v", i, err)
		}
	}
	if d.Len() != downloadLimit {
		t.Fatalf("Len = %d, want %d", d.Len(), downloadLimit)
	}
	if _, err := d.Mint(DownloadGrant{LinkID: "tgl_new", SpaceID: "s", AttachmentID: "a"}); !errors.Is(err, ErrDownloadsFull) {
		t.Fatalf("mint at the cap = %v, want ErrDownloadsFull", err)
	}
	if d.Len() != downloadLimit {
		t.Fatalf("a refused mint changed Len to %d", d.Len())
	}
	if _, err := d.Redeem(first); err != nil {
		t.Fatalf("the refused mint evicted a live grant: %v", err)
	}
	clk.Advance(DownloadTTL)
	if _, err := d.Mint(DownloadGrant{LinkID: "tgl_new", SpaceID: "s", AttachmentID: "a"}); err != nil {
		t.Fatalf("mint after expiry: %v", err)
	}
}
