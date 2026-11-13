package httpwrap

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoute(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		"GET /api/v1/requests/{id}":     "/api/v1/requests/{id}",
		"POST example.org/hooks/{id}":   "/hooks/{id}",
		"/static/":                      "/static/",
		"/files/{path...}":              "/files/{path...}",
		"GET  /x/{$}":                   "/x/{$}",
		"example.org":                   "",
		"/a b":                          "",
		"/" + string(make([]byte, 200)): "",
	} {
		if got := Route(in); got != want {
			t.Errorf("Route(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMethodAndCodeClass(t *testing.T) {
	if Method("GET") != "GET" || Method("PROPFIND") != "other" {
		t.Error("method normalization")
	}
	for code, want := range map[int]string{101: "1xx", 204: "2xx", 302: "3xx", 404: "4xx", 503: "5xx", 0: "other", 999: "other"} {
		if got := CodeClass(code); got != want {
			t.Errorf("CodeClass(%d) = %q", code, got)
		}
	}
}

func TestRecorder(t *testing.T) {
	base := httptest.NewRecorder()
	r := New(base)
	if r.Status() != http.StatusOK {
		t.Error("default status")
	}
	r.WriteHeader(http.StatusCreated)
	r.WriteHeader(http.StatusInternalServerError) // ignored: first status wins
	_, _ = r.Write([]byte("x"))
	r.Flush()
	if r.Status() != http.StatusCreated || !base.Flushed || r.Unwrap() != base {
		t.Errorf("status=%d flushed=%v", r.Status(), base.Flushed)
	}
}

// countingWriter counts WriteHeader calls that reach the underlying writer (net/http logs each one after the first as
// "superfluous response.WriteHeader call").
type countingWriter struct {
	http.ResponseWriter
	headers []int
}

func (c *countingWriter) WriteHeader(code int) {
	c.headers = append(c.headers, code)
	c.ResponseWriter.WriteHeader(code)
}

// A handler that writes an error after the response started (a request the client aborted) must not reach the server
// with a second WriteHeader; informational 1xx headers still pass before the final one.
func TestRecorderDropsSuperfluousWriteHeader(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(r *Recorder)
		want  []int
		code  int
	}{
		{"twice", func(r *Recorder) { r.WriteHeader(200); r.WriteHeader(500) }, []int{200}, 200},
		{"after body", func(r *Recorder) { _, _ = r.Write([]byte("x")); r.WriteHeader(500) }, nil, 200},
		{"after flush", func(r *Recorder) { r.Flush(); r.WriteHeader(500) }, nil, 200},
		{"informational", func(r *Recorder) { r.WriteHeader(103); r.WriteHeader(201) }, []int{103, 201}, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &countingWriter{ResponseWriter: httptest.NewRecorder()}
			r := New(c)
			tc.write(r)
			if len(c.headers) != len(tc.want) || r.Status() != tc.code {
				t.Fatalf("headers %v status %d; want %v %d", c.headers, r.Status(), tc.want, tc.code)
			}
			for i := range tc.want {
				if c.headers[i] != tc.want[i] {
					t.Fatalf("headers %v; want %v", c.headers, tc.want)
				}
			}
		})
	}
}
