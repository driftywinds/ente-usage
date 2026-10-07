package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPublicBaseDerivation(t *testing.T) {
	old := cfg.PublicBase
	defer func() { cfg.PublicBase = old }()

	// 1. explicit override wins, trailing slash trimmed
	cfg.PublicBase = "https://usage.example.com/"
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	if got := publicBase(req); got != "https://usage.example.com" {
		t.Errorf("override = %q", got)
	}

	// 2. derived from X-Forwarded-Proto + Host (reverse proxy TLS)
	cfg.PublicBase = ""
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Host = "usage.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	if got, want := publicBase(req), "https://usage.example.com"; got != want {
		t.Errorf("proxy = %q, want %q", got, want)
	}

	// 3. plain http request with port
	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Host = "localhost:8080"
	if got, want := publicBase(req), "http://localhost:8080"; got != want {
		t.Errorf("plain = %q, want %q", got, want)
	}
}

func TestAdminUserIDLink(t *testing.T) {
	old := cfg.PublicBase
	cfg.PublicBase = "https://usage.example.com"
	defer func() { cfg.PublicBase = old }()

	// through the real handler so publicBase() is exercised end to end
	freshDB(t)
	start, _, _, _ := monthRange("2026-01")
	if _, err := lite.Exec(`INSERT INTO samples(ts, user_id, bytes) VALUES(?,?,?)`,
		start+100, 4242, 5e9); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin?month=2026-01", nil)
	handleAdmin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status = %d", rec.Code)
	}
	out := rec.Body.String()

	// html/template must not mangle the URL (no %3A, no #ZgotmplZ)
	if strings.Contains(out, "#ZgotmplZ") {
		t.Error("template rejected the URL (ZgotmplZ)")
	}
	if strings.Contains(out, "%3A%2F%2F") {
		t.Error("URL got percent-encoded")
	}
	want := `href="https://usage.example.com/?id=4242"`
	if !strings.Contains(out, want) {
		t.Errorf("missing %s", want)
	}
	// must open in a new tab and not leak opener
	if !strings.Contains(out, `target="_blank"`) || !strings.Contains(out, `rel="noopener"`) {
		t.Error("link should open in a new tab with rel=noopener")
	}

	// derived (no override): X-Forwarded-Proto https
	cfg.PublicBase = ""
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin?month=2026-01", nil)
	req.Host = "usage.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	handleAdmin(rec, req)
	if !strings.Contains(rec.Body.String(), `href="https://usage.example.com/?id=4242"`) {
		t.Error("derived link missing")
	}

	// public page must not contain admin links (CSS mentions .uidlink too,
	// so assert on actual markup)
	pubRec := httptest.NewRecorder()
	pubReq := httptest.NewRequest(http.MethodGet, "/?id=4242", nil)
	handleIndex(pubRec, pubReq)
	if strings.Contains(pubRec.Body.String(), `class="uidlink"`) ||
		strings.Contains(pubRec.Body.String(), `target="_blank"`) {
		t.Error("public page must not have uidlink")
	}
}
