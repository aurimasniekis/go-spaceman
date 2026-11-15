package app_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
)

// The Mini App uploads an attachment with its Telegram session (Authorization header, no cookie, no CSRF header), under
// the link's cap ∩ the user, and the space's attachment rules.
func TestTelegramMiniAppUpload(t *testing.T) {
	t.Parallel()
	s := newTelegramSetup(t)
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	up := func(space, name string, content []byte, hdr ...string) resp {
		t.Helper()
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		fw, _ := mw.CreateFormFile("file", name)
		_, _ = fw.Write(content)
		_ = mw.Close()
		req, _ := http.NewRequest(http.MethodPost, s.minisrv.URL+api.Prefix+"/spaces/"+space+"/attachments", &b)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Origin", s.minisrv.URL)
		req.Header.Set("Idempotency-Key", "upload-key-"+name)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return resp{status: res.StatusCode, header: res.Header, body: buf.Bytes()}
	}

	// A session-less upload (the cookie/CSRF way the web composer used) is refused: the Mini App has no cookie.
	expect(t, up(s.space, "a.png", png, "X-CSRF-Token", "x"), http.StatusUnauthorized)
	// With the Telegram session it is stored and sniffed as a PNG.
	r := up(s.space, "IMG_3881.png", png, "Authorization", "Telegram good")
	expect(t, r, http.StatusCreated)
	var out struct {
		ID   string `json:"id"`
		MIME string `json:"mime"`
	}
	r.json(t, &out)
	if out.ID == "" || !strings.HasPrefix(out.MIME, "image/png") {
		t.Fatalf("upload: %s", r.body)
	}
	// A foreign origin is refused even with a valid session.
	expect(t, up(s.space, "b.png", png, "Authorization", "Telegram good", "Origin", "https://evil.example.com"), http.StatusForbidden)
	// The link's cap applies: viewer-capped, the link can't upload.
	s.setLinkRole(t, "viewer")
	expect(t, up(s.space, "c.png", png, "Authorization", "Telegram good"), http.StatusForbidden)
	// Nor into a space the link has no grant in.
	s.setLinkRole(t, "operator")
	expect(t, up("spc_other_none", "d.png", png, "Authorization", "Telegram good"), http.StatusNotFound)
}
