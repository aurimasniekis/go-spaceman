// Package miniapp holds the Telegram Mini App's authentication (ADR-0083, plan §3.4.2): the validation of the
// initData Telegram hands the Mini App, its single-use replay cache, and the in-memory sessions the exchange issues.
package miniapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxInitDataBytes bounds the raw initData accepted (the exchange's body limit is 8 KiB).
const MaxInitDataBytes = 8 << 10

// MaxFutureSkew is how far auth_date may lie ahead of central's clock.
const MaxFutureSkew = 30 * time.Second

// The validation errors. Callers answer every one of them with the same 401; the distinction is for tests and logs.
var (
	ErrMalformed = errors.New("miniapp: malformed initData")
	ErrSignature = errors.New("miniapp: initData signature mismatch")
	ErrExpired   = errors.New("miniapp: initData too old")
	ErrFuture    = errors.New("miniapp: initData auth_date in the future")
)

// InitData is what central uses of a validated initData. start_param and the other fields are routing hints at most;
// they are never used for authorization.
type InitData struct {
	// UserID is the Telegram user the data was signed for.
	UserID int64
	// AuthDate is when Telegram signed it.
	AuthDate time.Time
	// Hash is the signature: an HMAC over the canonical data-check-string, so it identifies the signed content however
	// the query string is encoded or ordered (the replay cache's key).
	Hash [sha256.Size]byte
}

// SecretKey derives the key initData is signed with from the bot token: HMAC-SHA256(key="WebAppData", msg=token)
// (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app).
func SecretKey(botToken string) []byte {
	m := hmac.New(sha256.New, []byte("WebAppData"))
	m.Write([]byte(botToken))
	return m.Sum(nil)
}

// validKey reports whether an initData field name has the documented shape (lower case, digits and '_').
func validKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	for _, c := range k {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

// Validate checks raw initData against secret (SecretKey of the bot token), as Telegram documents it, and strictly:
//   - the query string must parse with every key once (a duplicate is refused), known-shaped keys, and no value
//     containing a newline (which would make the data-check-string ambiguous);
//   - the data-check-string is every field but hash, sorted by key, "key=value" joined with "\n";
//   - hash must be 64 hex digits and equal HMAC-SHA256(secret, data-check-string), compared with hmac.Equal (constant
//     time);
//   - auth_date must be at most maxAge old and at most MaxFutureSkew ahead of now;
//   - user must be a JSON object with a positive id.
func Validate(raw string, secret []byte, now time.Time, maxAge time.Duration) (InitData, error) {
	var out InitData
	if raw == "" || len(raw) > MaxInitDataBytes || len(secret) == 0 {
		return out, ErrMalformed
	}
	fields := map[string]string{}
	for pair := range strings.SplitSeq(raw, "&") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return out, ErrMalformed
		}
		k, err := url.QueryUnescape(k)
		if err != nil || !validKey(k) {
			return out, ErrMalformed
		}
		v, err = url.QueryUnescape(v)
		if err != nil || strings.ContainsAny(v, "\n\r") {
			return out, ErrMalformed
		}
		if _, dup := fields[k]; dup {
			return out, ErrMalformed
		}
		fields[k] = v
	}
	hashHex, ok := fields["hash"]
	if !ok || len(hashHex) != 2*sha256.Size || strings.ToLower(hashHex) != hashHex {
		return out, ErrMalformed
	}
	got, err := hex.DecodeString(hashHex)
	if err != nil {
		return out, ErrMalformed
	}
	delete(fields, "hash")
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	m := hmac.New(sha256.New, secret)
	for i, k := range keys {
		if i > 0 {
			m.Write([]byte{'\n'})
		}
		m.Write([]byte(k))
		m.Write([]byte{'='})
		m.Write([]byte(fields[k]))
	}
	if !hmac.Equal(m.Sum(nil), got) {
		return out, ErrSignature
	}
	// Only signed content is read from here on.
	sec, err := strconv.ParseInt(fields["auth_date"], 10, 64)
	if err != nil || sec <= 0 {
		return out, ErrMalformed
	}
	at := time.Unix(sec, 0)
	if at.Sub(now) > MaxFutureSkew {
		return out, ErrFuture
	}
	if now.Sub(at) > maxAge {
		return out, ErrExpired
	}
	var u struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(fields["user"]), &u); err != nil || u.ID <= 0 {
		return out, ErrMalformed
	}
	out.UserID, out.AuthDate = u.ID, at
	copy(out.Hash[:], got)
	return out, nil
}
