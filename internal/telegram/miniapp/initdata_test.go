package miniapp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testToken makes a bot token of the documented shape at test time (no secret fixtures).
func testToken(t testing.TB) string {
	t.Helper()
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "123456789:" + hex.EncodeToString(b)[:35]
}

// sign builds initData as Telegram does (https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app),
// written independently of Validate: fields sorted, "k=v" joined by "\n", HMAC-SHA256 under
// HMAC-SHA256("WebAppData", token), hex.
func sign(token string, fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = k + "=" + fields[k]
	}
	sk := hmac.New(sha256.New, []byte("WebAppData"))
	sk.Write([]byte(token))
	m := hmac.New(sha256.New, sk.Sum(nil))
	m.Write([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

func encode(fields map[string]string, hash string) string {
	v := url.Values{}
	for k, x := range fields {
		v.Set(k, x)
	}
	if hash != "" {
		v.Set("hash", hash)
	}
	return v.Encode()
}

func fieldsAt(userID int64, at time.Time) map[string]string {
	return map[string]string{
		"user":          `{"id":` + strconv.FormatInt(userID, 10) + `,"first_name":"Jane","username":"jane"}`,
		"auth_date":     strconv.FormatInt(at.Unix(), 10),
		"query_id":      "AAHdF6IQAAAAAN0XohDhrOrc",
		"chat_instance": "-123",
		"start_param":   "space_x",
	}
}

func signed(token string, f map[string]string) string { return encode(f, sign(token, f)) }

var now0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// A correctly signed initData passes; everything forged, tampered, malformed, too old, future-dated or ambiguous is
// refused with the right error (SM-TG-009 initData vectors, plan §3.4.2).
func TestValidateInitData(t *testing.T) {
	t.Parallel()
	tok := testToken(t)
	key := SecretKey(tok)
	maxAge := 5 * time.Minute
	good := fieldsAt(42, now0.Add(-time.Minute))
	d, err := Validate(signed(tok, good), key, now0, maxAge)
	if err != nil {
		t.Fatalf("valid initData: %v", err)
	}
	if d.UserID != 42 || !d.AuthDate.Equal(now0.Add(-time.Minute)) {
		t.Fatalf("parsed %+v", d)
	}

	// Boundaries: exactly max age old, and MaxFutureSkew ahead, pass.
	for name, at := range map[string]time.Time{"max age": now0.Add(-maxAge), "skew": now0.Add(MaxFutureSkew)} {
		if _, err := Validate(signed(tok, fieldsAt(42, at)), key, now0, maxAge); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	tampered := func(mut func(map[string]string)) string {
		f := fieldsAt(42, now0)
		h := sign(tok, f)
		mut(f)
		return encode(f, h)
	}
	goodHash := sign(tok, good)
	flip := []byte(goodHash)
	if flip[0] == 'a' {
		flip[0] = 'b'
	} else {
		flip[0] = 'a'
	}
	cases := []struct {
		name string
		raw  string
		want error
	}{
		{"signed with another bot's token", signed(testToken(t), good), ErrSignature},
		{"wrong hash", encode(good, string(flip)), ErrSignature},
		{"hash of other data", encode(good, sign(tok, fieldsAt(43, now0))), ErrSignature},
		{"user swapped after signing", tampered(func(f map[string]string) { f["user"] = `{"id":1}` }), ErrSignature},
		{"auth_date moved after signing", tampered(func(f map[string]string) { f["auth_date"] = strconv.FormatInt(now0.Unix()+1, 10) }), ErrSignature},
		{"field added after signing", tampered(func(f map[string]string) { f["extra"] = "1" }), ErrSignature},
		{"field removed after signing", tampered(func(f map[string]string) { delete(f, "query_id") }), ErrSignature},
		{"signed by key = token (no WebAppData step)", func() string {
			m := hmac.New(sha256.New, []byte(tok))
			f := fieldsAt(42, now0)
			ks := []string{"auth_date", "chat_instance", "query_id", "start_param", "user"}
			var l []string
			for _, k := range ks {
				l = append(l, k+"="+f[k])
			}
			m.Write([]byte(strings.Join(l, "\n")))
			return encode(f, hex.EncodeToString(m.Sum(nil)))
		}(), ErrSignature},
		{"older than max age", signed(tok, fieldsAt(42, now0.Add(-maxAge-time.Second))), ErrExpired},
		{"a day old", signed(tok, fieldsAt(42, now0.Add(-24*time.Hour))), ErrExpired},
		{"future auth_date", signed(tok, fieldsAt(42, now0.Add(MaxFutureSkew+time.Second))), ErrFuture},
		{"far future auth_date", signed(tok, fieldsAt(42, now0.Add(time.Hour))), ErrFuture},
		{"no hash", encode(good, ""), ErrMalformed},
		{"upper-case hash", encode(good, strings.ToUpper(goodHash)), ErrMalformed},
		{"short hash", encode(good, goodHash[:62]), ErrMalformed},
		{"non-hex hash", encode(good, "zz"+goodHash[2:]), ErrMalformed},
		{"empty", "", ErrMalformed},
		{"too long", strings.Repeat("a", MaxInitDataBytes+1), ErrMalformed},
		{"duplicate key", signed(tok, good) + "&user=" + url.QueryEscape(`{"id":1}`), ErrMalformed},
		{"duplicate hash", signed(tok, good) + "&hash=" + goodHash, ErrMalformed},
		{"pair without =", signed(tok, good) + "&junk", ErrMalformed},
		{"bad escape", signed(tok, good) + "&x=%zz", ErrMalformed},
		{"odd key", signed(tok, good) + "&User=1", ErrMalformed},
		{"no auth_date", func() string { f := fieldsAt(42, now0); delete(f, "auth_date"); return signed(tok, f) }(), ErrMalformed},
		{"no user", func() string { f := fieldsAt(42, now0); delete(f, "user"); return signed(tok, f) }(), ErrMalformed},
		{"user id 0", func() string { f := fieldsAt(42, now0); f["user"] = `{"id":0}`; return signed(tok, f) }(), ErrMalformed},
		{"user not JSON", func() string { f := fieldsAt(42, now0); f["user"] = `42`; return signed(tok, f) }(), ErrMalformed},
		{"newline in a value", func() string {
			f := fieldsAt(42, now0)
			f["query_id"] = "x\nauth_date=1"
			return signed(tok, f)
		}(), ErrMalformed},
	}
	for _, c := range cases {
		if _, err := Validate(c.raw, key, now0, maxAge); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := Validate(signed(tok, good), nil, now0, maxAge); !errors.Is(err, ErrMalformed) {
		t.Errorf("no key: %v", err)
	}
}

// The replay key is the signature, so the same signed content in another order or encoding has the same key.
func TestInitDataHashIsCanonical(t *testing.T) {
	t.Parallel()
	tok := testToken(t)
	f := fieldsAt(42, now0)
	h := sign(tok, f)
	a := encode(f, h)
	parts := strings.Split(a, "&")
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	b := strings.Replace(strings.Join(parts, "&"), "start_param=space_x", "start_param=space%5Fx", 1)
	da, err := Validate(a, SecretKey(tok), now0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	db, err := Validate(b, SecretKey(tok), now0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if a == b || da.Hash != db.Hash {
		t.Fatalf("same content, different keys (%q vs %q)", a, b)
	}
}

// The signature is compared in constant time: Validate calls hmac.Equal on the computed MAC, and nothing in the file
// compares bytes or strings with a short-circuiting function (bytes.Equal, bytes.Compare, strings.Compare/EqualFold
// on the hash) or compares the computed MAC with ==.
func TestSignatureComparedInConstantTime(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "initdata.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "bytes" {
			t.Errorf("initdata.go imports %s: compare signatures with hmac.Equal only", p)
		}
	}
	var hmacEqual int
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok {
					switch x.Name + "." + sel.Sel.Name {
					case "hmac.Equal":
						hmacEqual++
					case "strings.Compare", "strings.EqualFold":
						t.Errorf("%s: %s.%s near a signature: use hmac.Equal", fset.Position(call.Pos()), x.Name, sel.Sel.Name)
					}
				}
			}
		}
		if be, ok := n.(*ast.BinaryExpr); ok && (be.Op == token.EQL || be.Op == token.NEQ) {
			for _, side := range []ast.Expr{be.X, be.Y} {
				if id, ok := side.(*ast.Ident); ok && (id.Name == "got" || id.Name == "mac") {
					t.Errorf("%s: signature compared with %s", fset.Position(be.Pos()), be.Op)
				}
			}
		}
		return true
	})
	if hmacEqual != 1 {
		t.Errorf("hmac.Equal called %d times in initdata.go, want 1 (the signature check)", hmacEqual)
	}
}

func FuzzValidate(f *testing.F) {
	tok := "123456789:" + strings.Repeat("a", 35)
	f.Add(signed(tok, fieldsAt(42, now0)))
	f.Add("user=%7B%7D&hash=00&auth_date=1")
	f.Fuzz(func(t *testing.T, raw string) {
		d, err := Validate(raw, SecretKey(tok), now0, time.Hour)
		if err == nil && d.UserID <= 0 {
			t.Fatalf("accepted without a user: %q", raw)
		}
	})
}
