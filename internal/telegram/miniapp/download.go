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
	// downloadLimit caps the grants held in memory. Expired ones are swept first; at the cap Mint refuses
	// (ErrDownloadsFull) rather than evict another link's live grant.
	downloadLimit = 10000
	// downloadPerLink caps the live grants of one Telegram link; a new mint past it drops that link's own oldest.
	downloadPerLink = 16
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

// ErrDownloadsFull is Mint's answer when the store is at its global cap: the caller should retry later (429).
var ErrDownloadsFull = errors.New("miniapp: too many live download links")

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
	byLink  map[string][][sha256.Size]byte
}

// NewDownloads returns an empty store (a nil clock is the system clock).
func NewDownloads(clk clock.Clock) *Downloads {
	if clk == nil {
		clk = clock.System{}
	}
	return &Downloads{clk: clk, entries: map[[sha256.Size]byte]*downloadEntry{},
		byLink: map[string][][sha256.Size]byte{}}
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
	// One link never holds more than downloadPerLink grants: it replaces its own oldest, never another link's.
	for len(d.byLink[g.LinkID]) >= downloadPerLink {
		d.dropLocked(d.byLink[g.LinkID][0])
	}
	if len(d.entries) >= downloadLimit {
		return "", ErrDownloadsFull
	}
	d.entries[h] = &downloadEntry{DownloadGrant: g, expires: now.Add(DownloadTTL)}
	d.order = append(d.order, h)
	d.byLink[g.LinkID] = append(d.byLink[g.LinkID], h)
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
	if e, ok := d.entries[h]; ok {
		l := d.byLink[e.LinkID]
		for i, o := range l {
			if o == h {
				l = append(l[:i], l[i+1:]...)
				break
			}
		}
		if len(l) == 0 {
			delete(d.byLink, e.LinkID)
		} else {
			d.byLink[e.LinkID] = l
		}
	}
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
		d.dropLocked(d.order[0])
	}
}
