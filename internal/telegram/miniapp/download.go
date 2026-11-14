package miniapp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
)

// Signed single-purpose download links (ADR-0083 §3.4.3). A link is a random lookup key, not a signature: the grant is
// held in memory under the key's SHA-256, valid for DownloadTTL and DownloadMaxUses redemptions, and names one
// attachment of one space and the one Telegram link it was minted for.
const (
	// DownloadTTL is how long a minted download link works.
	DownloadTTL = 60 * time.Second
	// DownloadMaxUses is how many redemptions one link allows (download managers retry with Range requests).
	DownloadMaxUses = 3
	// downloadLimit caps the grants held in memory; expired ones go first, then the oldest.
	downloadLimit = 10000
	// downloadIDLen is the length of an ID: 32 random bytes as unpadded base64url.
	downloadIDLen = 43
)

// ErrDownloadGone is Redeem's answer for an ID that is unknown, expired or used up. The three are not told apart.
var ErrDownloadGone = errors.New("miniapp: download link is not valid")

// DownloadGrant is what a download link is bound to.
type DownloadGrant struct {
	LinkID       string // the Telegram link whose principal is re-authorized on every redemption
	SpaceID      string
	AttachmentID string
}

type downloadEntry struct {
	DownloadGrant
	expires time.Time
	uses    int
}

// Downloads holds the live download links. Only SHA-256 hashes of the IDs are kept.
type Downloads struct {
	clk clock.Clock

	mu      sync.Mutex
	entries map[[sha256.Size]byte]*downloadEntry
	order   [][sha256.Size]byte // insertion order, for dropping the oldest
}

// NewDownloads returns an empty store (a nil clock is the system clock).
func NewDownloads(clk clock.Clock) *Downloads {
	if clk == nil {
		clk = clock.System{}
	}
	return &Downloads{clk: clk, entries: map[[sha256.Size]byte]*downloadEntry{}}
}

// ValidDownloadID reports whether s has the shape of an ID, so a malformed one is refused without a lookup.
func ValidDownloadID(s string) bool {
	if len(s) != downloadIDLen {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// Mint stores a grant and returns its ID: the only copy of it that exists.
func (d *Downloads) Mint(g DownloadGrant) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(b[:])
	h := sha256.Sum256([]byte(id))
	now := d.clk.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(now)
	for len(d.entries) >= downloadLimit && len(d.order) > 0 {
		delete(d.entries, d.order[0])
		d.order = d.order[1:]
	}
	d.entries[h] = &downloadEntry{DownloadGrant: g, expires: now.Add(DownloadTTL)}
	d.order = append(d.order, h)
	return id, nil
}

// Redeem counts one use of id and returns its grant. An unknown, expired or used-up ID is ErrDownloadGone; the
// last permitted use drops the entry. A use is counted before the caller re-authorizes, so a refused attempt is spent.
func (d *Downloads) Redeem(id string) (DownloadGrant, error) {
	if !ValidDownloadID(id) {
		return DownloadGrant{}, ErrDownloadGone
	}
	h := sha256.Sum256([]byte(id))
	now := d.clk.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[h]
	if !ok {
		return DownloadGrant{}, ErrDownloadGone
	}
	if !now.Before(e.expires) {
		d.dropLocked(h)
		return DownloadGrant{}, ErrDownloadGone
	}
	e.uses++
	g := e.DownloadGrant
	if e.uses >= DownloadMaxUses {
		d.dropLocked(h)
	}
	return g, nil
}

// Len is the number of live grants (tests, metrics).
func (d *Downloads) Len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

func (d *Downloads) dropLocked(h [sha256.Size]byte) {
	delete(d.entries, h)
	for i, o := range d.order {
		if o == h {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
}

// sweepLocked drops expired grants from the front of the order (entries are minted with one TTL, so the order is by
// expiry).
func (d *Downloads) sweepLocked(now time.Time) {
	for len(d.order) > 0 {
		e, ok := d.entries[d.order[0]]
		if ok && now.Before(e.expires) {
			return
		}
		delete(d.entries, d.order[0])
		d.order = d.order[1:]
	}
}
