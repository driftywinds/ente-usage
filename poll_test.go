package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPollButtonRenders(t *testing.T) {
	p := Page{
		Admin: true, Month: "2026-01", Months: []string{"2026-01"},
		Notice: "sampled 12 users", PricePerTB: 6.95, Rate: 88, LastSample: "never",
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, `action="/admin/sample"`) {
		t.Error("missing /admin/sample form")
	}
	if !strings.Contains(out, "Poll latest stats") {
		t.Error("missing poll button label")
	}
	if !strings.Contains(out, "sampled 12 users") {
		t.Error("notice not shown")
	}
}

func TestSampleHandlerMethodGuard(t *testing.T) {
	// GET must 405 without touching the databases.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/sample", nil)
	handleAdminSample(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", rec.Code)
	}
}
