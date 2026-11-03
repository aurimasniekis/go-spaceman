package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/keyring"
)

const (
	id1 = "bkp_00000000000000000000000001"
	id2 = "bkp_00000000000000000000000002"
)

func testKEK(t testing.TB) *keyring.LocalKEK {
	t.Helper()
	k, err := keyring.GenerateKEK()
	if err != nil {
		t.Fatal(err)
	}
	w, err := keyring.NewLocalKEK(k)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func seal(t testing.TB, kek keyring.KeyWrapper, data []byte, kind, id string) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := Seal(&b, bytes.NewReader(data), kek, kind, id); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func open(kek keyring.KeyWrapper, sealed []byte, kind, id string) ([]byte, error) {
	r, _, err := OpenSealed(bytes.NewReader(sealed), kek, kind, id)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

var kindDB = SealedKindFile(DBFile)

// Ported from go-hallpass internal/backup/sealed_test.go at bdd8329.
func TestSealedRoundTrip(t *testing.T) {
	kek := testKEK(t)
	for _, n := range []int{0, 1, sealedFrameMax - 1, sealedFrameMax, sealedFrameMax + 1, 3 * sealedFrameMax, 3*sealedFrameMax + 7} {
		data := make([]byte, n)
		_, _ = rand.Read(data)
		sealed := seal(t, kek, data, kindDB, id1)
		if bytes.Contains(sealed, data[:min(n, 64)]) && n >= 16 {
			t.Fatalf("%d bytes: plaintext visible in the sealed object", n)
		}
		got, err := open(kek, sealed, kindDB, id1)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%d bytes: round trip %v (%d bytes back)", n, err, len(got))
		}
	}
}

func TestSealedRejects(t *testing.T) {
	kek := testKEK(t)
	data := make([]byte, 2*sealedFrameMax+100)
	_, _ = rand.Read(data)
	sealed := seal(t, kek, data, kindDB, id1)

	if _, err := open(testKEK(t), sealed, kindDB, id1); !errors.Is(err, ErrKEKMismatch) {
		t.Errorf("other KEK: %v", err)
	}
	if _, err := open(kek, sealed, SealedKindManifest, id1); !errors.Is(err, ErrSealed) {
		t.Errorf("wrong kind: %v", err)
	}
	if _, err := open(kek, sealed, kindDB, id2); !errors.Is(err, ErrSealed) {
		t.Errorf("wrong backup: %v", err)
	}
	// Truncation at every frame boundary region and mid-frame.
	for _, cut := range []int{10, len(SealedMagic) + 4 + 20, len(sealed) - 1, len(sealed) - 16, len(sealed) - 200, len(sealed) / 2} {
		if _, err := open(kek, sealed[:cut], kindDB, id1); !errors.Is(err, ErrSealed) {
			t.Errorf("truncated at %d of %d accepted: %v", cut, len(sealed), err)
		}
	}
	// Dropping the final frame entirely (cut exactly after the second full frame).
	full := len(SealedMagic) + 4 + headerLen(sealed) + 2*(4+sealedFrameMax+16)
	if _, err := open(kek, sealed[:full], kindDB, id1); !errors.Is(err, ErrSealed) {
		t.Errorf("missing final frame accepted: %v", err)
	}
	if _, err := open(kek, append(append([]byte(nil), sealed...), 0), kindDB, id1); !errors.Is(err, ErrSealed) {
		t.Errorf("trailing data accepted: %v", err)
	}
	for _, pos := range []int{len(SealedMagic) + 4 + 3, len(sealed) / 3, len(sealed) - 5} {
		bad := append([]byte(nil), sealed...)
		bad[pos] ^= 0x40
		if _, err := open(kek, bad, kindDB, id1); err == nil {
			t.Errorf("bit flip at %d accepted", pos)
		}
	}
	// Another file of the same backup (the kind names the path).
	if _, err := open(kek, sealed, SealedKindFile("recordings/x"), id1); !errors.Is(err, ErrSealed) {
		t.Errorf("wrong path: %v", err)
	}
	// A wrapped key copied into another object's header does not open it (the AAD names the backup).
	other := seal(t, kek, []byte("x"), kindDB, id2)
	swapped := append(append([]byte(nil), other[:len(SealedMagic)+4+headerLen(other)]...), sealed[len(SealedMagic)+4+headerLen(sealed):]...)
	if _, err := open(kek, swapped, kindDB, id2); err == nil {
		t.Error("frames of one object opened under another object's header")
	}
}

func headerLen(sealed []byte) int {
	return int(sealed[len(SealedMagic)])<<24 | int(sealed[len(SealedMagic)+1])<<16 | int(sealed[len(SealedMagic)+2])<<8 | int(sealed[len(SealedMagic)+3])
}

func FuzzOpenSealed(f *testing.F) {
	kek := testKEK(f)
	f.Add(seal(f, kek, []byte("hello"), SealedKindManifest, id1))
	f.Add(seal(f, kek, nil, SealedKindManifest, id1))
	f.Add([]byte(SealedMagic))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, _, err := OpenSealed(bytes.NewReader(b), kek, "", "")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(r, 1<<20))
	})
}
