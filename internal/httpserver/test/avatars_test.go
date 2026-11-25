package test

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/httpserver"
)

func TestBuiltinAvatarServing(t *testing.T) {
	files := avatars.BuiltinFS()
	catalog, err := avatars.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.DefaultID() != 1 || len(catalog.List()) != 66 {
		t.Fatal("unexpected initial catalog")
	}
	router := httpserver.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := httpserver.MountAvatars(router, catalog, files); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if etag != "" {
			r.Header.Set("If-None-Match", etag)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for i, entry := range catalog.List() {
		if entry.ID != int64(i+1) || entry.Path != fmt.Sprintf("images/%03d.webp", i+1) {
			t.Fatalf("bad stable ID mapping: %+v", entry)
		}
		w := request("GET", "/avatars/"+entry.Path, "")
		if w.Code != 200 || w.Header().Get("Content-Type") != "image/webp" || !bytes.HasPrefix(w.Body.Bytes(), []byte("RIFF")) {
			t.Fatalf("image %d: %d %s", entry.ID, w.Code, w.Header())
		}
		if w.Header().Get("ETag") == "" || strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Fatal("unsafe cache policy")
		}
		cached := request("GET", "/avatars/"+entry.Path, w.Header().Get("ETag"))
		if cached.Code != 304 || cached.Body.Len() != 0 {
			t.Fatal("ETag revalidation failed")
		}
	}
	w := request("GET", "/avatars/catalog.json", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("catalog: %d %v", w.Code, w.Header())
	}
	assertAvatarCORS := func(t *testing.T, resp *httptest.ResponseRecorder) {
		t.Helper()
		if resp.Header().Get("Access-Control-Allow-Origin") != "*" ||
			resp.Header().Get("Access-Control-Allow-Methods") != "GET, HEAD, OPTIONS" ||
			resp.Header().Get("Access-Control-Allow-Headers") != "Accept, If-None-Match, Cache-Control" ||
			resp.Header().Get("Access-Control-Expose-Headers") != "ETag, Retry-After" ||
			resp.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" {
			t.Fatalf("missing avatar cross-origin headers: %+v", resp.Header())
		}
	}
	assertAvatarCORS(t, w)
	headResp := request("HEAD", "/avatars/images/001.webp", "")
	if headResp.Code != 200 || headResp.Body.Len() != 0 {
		t.Fatal("HEAD returned a body or unexpected status")
	}
	assertAvatarCORS(t, headResp)
	optResp := request("OPTIONS", "/avatars/images/001.webp", "")
	if optResp.Code != http.StatusNoContent || optResp.Body.Len() != 0 {
		t.Fatalf("OPTIONS avatar: %d %q", optResp.Code, optResp.Body.String())
	}
	assertAvatarCORS(t, optResp)
	for _, path := range []string{"/avatars/", "/avatars/images/", "/avatars/images/067.webp", "/avatars/SOURCE.original.md", "/avatars/../catalog.json", "/avatars/%2e%2e/go.mod", "/avatars/images%5c001.webp", "/assets/avatars/images/001.webp"} {
		w := request("GET", path, "")
		if w.Code != http.StatusNotFound || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
}
