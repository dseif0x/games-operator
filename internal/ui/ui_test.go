//go:build ui

package ui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b), rec.Header()
}

func TestBaseHrefAndFallback(t *testing.T) {
	h := Handler("/hub")
	code, body, hdr := get(t, h, "/")
	if code != 200 || !strings.Contains(body, `<base href="/hub/"`) {
		t.Fatalf("index: %d %q", code, body)
	}
	if !strings.Contains(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("content type %q", hdr.Get("Content-Type"))
	}
	if code, deep, _ := get(t, h, "/apps/123"); code != 200 || deep != body {
		t.Fatalf("spa fallback: %d", code)
	}
	if code, root, _ := get(t, Handler(""), "/"); code != 200 || !strings.Contains(root, `<base href="/"`) {
		t.Fatalf("root base: %d", code)
	}
	if code, _, _ := get(t, h, "/favicon.svg"); code != 200 {
		t.Fatalf("static file: %d", code)
	}
}
