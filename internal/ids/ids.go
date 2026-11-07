// Package ids generates type-prefixed, time-sortable identifiers (ADR-0002).
//
// Derived from github.com/aurimasniekis/go-hallpass internal/ids (Apache-2.0, same author).
//
// An ID is "<prefix>_" followed by 26 lowercase Crockford base32 characters encoding a UUIDv7 (RFC 9562).
// IDs are unique and sortable by creation time but are not secrets.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"time"
)

// Prefixes for every entity type. Prefixes are part of the public API contract and are never reused.
const (
	Audit          = "aud"
	Keyring        = "kr"
	Job            = "job"
	Event          = "evt" // outbound outbox event
	Boot           = "boot"
	User           = "usr"
	WebSession     = "wss"
	APIToken       = "atk"
	RootToken      = "rtk"
	OIDCProvider   = "oidc"
	OIDCMapping    = "ogm"
	Grant          = "grt" // role grant (Casbin g row)
	Node           = "nod"
	NodeEnrollment = "enr"
	NodeCredential = "ncr"
	NodeSession    = "nss" // one node connection
	Space          = "spc"
	Lease          = "lse"
	Repository     = "rep"
	Workspace      = "wsp"
	Role           = "rol" // space role
	RoleTemplate   = "rtp"
	Instance       = "ins" // agent instance
	Session        = "ses" // harness conversation lineage
	Run            = "run"
	RunCredential  = "rcr"
	Command        = "cmd" // node protocol command
	Recording      = "rec"
	Segment        = "seg"
	Artifact       = "art"
	Attachment     = "att"
	InboxItem      = "inb"
	Task           = "tsk"
	Message        = "msg"
	HumanAction    = "hum"
	HumanType      = "hty"
	HumanTypeVer   = "htv"
	Comment        = "cmt"
	Approval       = "apv"
	Account        = "acc"
	Prompt         = "prm"
	PromptVersion  = "prv"
	Profile        = "hpf" // harness profile
	Skill          = "skl"
	SkillVersion   = "skv"
	MCPServer      = "mcp"
	MCPGrant       = "mcg"
	EventConn      = "ecn"
	EventTemplate  = "etp"
	EventTemplVer  = "etv"
	Subscription   = "sub"
	Quarantine     = "qrn"
	MemoryColl     = "mem"
	KBRepo         = "kbr"
	KBSession      = "kbs"
	Dream          = "drm"
	XSpaceRequest  = "xsr"
	Backup         = "bkp"
	Relocation     = "rlc"
	Schedule       = "sch"
	Retention      = "ret"
	GitKey         = "gky" // a shared Git SSH key (ADR-0058)
	Takeover       = "tko" // terminal takeover (SM-UI-004)
	Preemption     = "tpr" // a task's or worker's preemption by an incident (SM-QUEUE-009)
	Operation      = "op"  // long-running operation
	HarborPilot    = "hpc" // the Harbor Pilot connection (ADR-0060)
)

const encoding = "0123456789abcdefghjkmnpqrstvwxyz"

const encodedLen = 26

var decodeMap = func() [256]byte {
	var m [256]byte
	for i := range m {
		m[i] = 0xff
	}
	for i := range len(encoding) {
		m[encoding[i]] = byte(i)
		m[strings.ToUpper(encoding)[i]] = byte(i)
	}
	return m
}()

// Generator produces monotonic UUIDv7 values.
type Generator struct {
	mu     sync.Mutex
	now    func() time.Time
	lastMS uint64
	seq    uint16 // 12-bit counter in rand_a
}

// NewGenerator returns a generator using now for timestamps (time.Now when nil).
func NewGenerator(now func() time.Time) *Generator {
	if now == nil {
		now = time.Now
	}
	return &Generator{now: now}
}

var defaultGen = NewGenerator(nil)

// New returns a new ID with the given prefix from the default generator.
func New(prefix string) string { return defaultGen.New(prefix) }

// New returns a new ID with the given prefix.
func (g *Generator) New(prefix string) string {
	u := g.uuid()
	return prefix + "_" + encode(u)
}

func (g *Generator) uuid() [16]byte {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic("ids: crypto/rand failed: " + err.Error())
	}
	g.mu.Lock()
	ms := uint64(g.now().UnixMilli())
	if ms <= g.lastMS {
		// Same or earlier millisecond: keep ordering with a counter (RFC 9562 §6.2, method 1).
		ms = g.lastMS
		g.seq++
		if g.seq > 0x0fff {
			ms++
			g.seq = binary.BigEndian.Uint16(rnd[0:2]) & 0x07ff
		}
	} else {
		g.seq = binary.BigEndian.Uint16(rnd[0:2]) & 0x07ff // leave headroom for increments
	}
	g.lastMS = ms
	seq := g.seq
	g.mu.Unlock()

	var u [16]byte
	// 48-bit unix_ts_ms | 4-bit version (7) | 12-bit counter, then 2-bit variant | 62 random bits.
	binary.BigEndian.PutUint64(u[0:8], ms<<16|0x7000|uint64(seq&0x0fff))
	copy(u[8:], rnd[2:])
	u[8] = 0x80 | (u[8] & 0x3f)
	return u
}

func encode(u [16]byte) string {
	// 128 bits → 26 base32 digits (2 leading zero bits), big-endian.
	hi := binary.BigEndian.Uint64(u[0:8])
	lo := binary.BigEndian.Uint64(u[8:16])
	var out [encodedLen]byte
	for i := encodedLen - 1; i >= 0; i-- {
		out[i] = encoding[lo&0x1f]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// ErrInvalid is returned for malformed identifiers.
var ErrInvalid = errors.New("invalid identifier")

// Parse validates id against prefix and returns the canonical (lowercase) form.
func Parse(prefix, id string) (string, error) {
	p, rest, ok := strings.Cut(id, "_")
	if !ok || p != prefix || len(rest) != encodedLen {
		return "", ErrInvalid
	}
	if decodeMap[rest[0]] > 7 { // top 2 bits must be zero
		return "", ErrInvalid
	}
	for i := range len(rest) {
		if decodeMap[rest[i]] == 0xff {
			return "", ErrInvalid
		}
	}
	return prefix + "_" + strings.ToLower(rest), nil
}

// Valid reports whether id is a well-formed identifier with the given prefix.
func Valid(prefix, id string) bool {
	_, err := Parse(prefix, id)
	return err == nil
}

// Time returns the creation time embedded in a valid id.
func Time(id string) (time.Time, error) {
	_, rest, ok := strings.Cut(id, "_")
	if !ok || len(rest) != encodedLen {
		return time.Time{}, ErrInvalid
	}
	var hi, lo uint64
	for i := range len(rest) {
		v := decodeMap[rest[i]]
		if v == 0xff {
			return time.Time{}, ErrInvalid
		}
		hi = hi<<5 | lo>>59
		lo = lo<<5 | uint64(v)
	}
	return time.UnixMilli(int64(hi >> 16)), nil //nolint:gosec // 48-bit timestamp fits
}
