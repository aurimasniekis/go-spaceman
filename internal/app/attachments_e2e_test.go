package app_test

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// upload posts one file as multipart form data.
func (c *client) upload(path, name string, content []byte, hdr ...string) resp {
	c.h.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	return c.raw("POST", path, &b, append([]string{"Content-Type", mw.FormDataContentType()}, hdr...)...)
}

func (c *client) raw(method, path string, body io.Reader, hdr ...string) resp {
	c.h.t.Helper()
	req, _ := http.NewRequest(method, c.h.srv.URL+api.Prefix+path, body)
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := c.hc.Do(req)
	if err != nil {
		c.h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{status: res.StatusCode, header: res.Header, body: b}
}

// SM-ATT-002/004 over HTTP: multipart upload with idempotency, metadata, safe serving (raster images inline, SVG and
// HTML downloaded with a neutral type, nosniff and a sandbox CSP), space-scoped authorization and the size limit.
func TestAttachmentsOverHTTP(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	rootSecret, _, _ := h.c.Auth.IssueRootToken(ctx)
	root := h.client()
	root.bearer = rootSecret
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, err := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "bravo", Name: "Bravo", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}

	r := root.upload("/spaces/"+a.ID+"/attachments", "dot.png", png1x1, "Idempotency-Key", "upload-dot-1")
	expect(t, r, 201)
	var img api.AttachmentOut
	r.json(t, &img)
	if img.MIME != "image/png" || !img.Inline || img.URI != "spaceman://attachment/"+img.ID || !strings.HasSuffix(img.URL, "/attachments/"+img.ID+"/content") {
		t.Fatalf("uploaded %+v", img)
	}
	r = root.upload("/spaces/"+a.ID+"/attachments", "dot.png", png1x1, "Idempotency-Key", "upload-dot-1")
	var replay api.AttachmentOut
	r.json(t, &replay)
	if replay.ID != img.ID {
		t.Fatalf("idempotent replay created %s, want %s", replay.ID, img.ID)
	}

	r = root.raw("GET", "/spaces/"+a.ID+"/attachments/"+img.ID+"/content", nil)
	expect(t, r, 200)
	if r.header.Get("Content-Type") != "image/png" || !strings.HasPrefix(r.header.Get("Content-Disposition"), "inline") ||
		r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") || !bytes.Equal(r.body, png1x1) {
		t.Fatalf("image served with %v", r.header)
	}
	for name, body := range map[string]string{"x.svg": `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`, "x.html": "<html><script>alert(1)</script></html>"} {
		r = root.upload("/spaces/"+a.ID+"/attachments", name, []byte(body))
		expect(t, r, 201)
		var att api.AttachmentOut
		r.json(t, &att)
		r = root.raw("GET", "/spaces/"+a.ID+"/attachments/"+att.ID+"/content", nil)
		if r.header.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(r.header.Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s served with %v", name, r.header)
		}
	}

	// Another space's ID does not reach it; metadata follows the same rule.
	expect(t, root.raw("GET", "/spaces/"+b.ID+"/attachments/"+img.ID+"/content", nil), 404)
	expect(t, root.do("GET", "/spaces/"+b.ID+"/attachments/"+img.ID, nil), 404)
	expect(t, root.do("GET", "/spaces/"+a.ID+"/attachments/"+img.ID, nil), 200)

	// A viewer reads but cannot upload; anonymous requests reach nothing.
	expect(t, root.do("POST", "/users", map[string]any{"email": "vic@example.com", "display_name": "Vic", "password": "correct horse battery staple",
		"grants": []map[string]string{{"role": "viewer", "domain": a.ID}}}), 201)
	viewer := h.client()
	viewer.bearer = loginAndIssueToken(t, h, "vic@example.com", []map[string]string{{"role": "viewer", "domain": a.ID}})
	expect(t, viewer.raw("GET", "/spaces/"+a.ID+"/attachments/"+img.ID+"/content", nil), 200)
	expect(t, viewer.upload("/spaces/"+a.ID+"/attachments", "v.txt", []byte("v")), 403)
	expect(t, h.client().raw("GET", "/spaces/"+a.ID+"/attachments/"+img.ID+"/content", nil), 401)

	// Over the upload limit (25 MiB by default) is refused.
	if r := root.upload("/spaces/"+a.ID+"/attachments", "big.bin", make([]byte, 26<<20)); r.status < 400 || r.status >= 500 {
		t.Fatalf("oversize upload: %d %s", r.status, r.body)
	}
}

var png1x1 = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")
