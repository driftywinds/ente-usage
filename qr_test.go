package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQRHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/qr.png", nil)
	handleQR(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /qr.png = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	if got := rec.Body.Bytes(); len(got) < 100 || string(got[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("body is not a PNG (len=%d)", len(got))
	}
	if rec.Body.Len() != len(qrPNG) {
		t.Errorf("served %d bytes, embedded %d", rec.Body.Len(), len(qrPNG))
	}

	// POST must be rejected
	rec2 := httptest.NewRecorder()
	handleQR(rec2, httptest.NewRequest(http.MethodPost, "/qr.png", nil))
	if rec2.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rec2.Code)
	}
}

func TestPublicPageShowsQRTile(t *testing.T) {
	p := Page{Query: "42", LastSampleTS: 0, Rate: 88, PricePerTB: 6.95, UpiID: upiID}
	row := makeRow(42, 1e12, 9e11, 30)
	p.Single = &row
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		`src="/qr.png"`,
		"qrbox",
		"Tap to copy UPI ID",
		"amoghrammohantiwari@okicici", // appears in the title attr and the JS
		"navigator.clipboard",
		"document.execCommand('copy')",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("public page missing %q", want)
		}
	}
	// QR box must sit in the card header, before the cost table
	if i, j := strings.Index(out, "qrbox"), strings.Index(out, "Cost at current usage"); i == -1 || j == -1 || i > j {
		t.Errorf("QR box (at %d) should come before the cost table (at %d)", i, j)
	}
	// admin page must NOT show the QR tile (shared CSS rules are harmless)
	admin := Page{Admin: true, Month: "2026-01", LastSampleTS: 0, UpiID: upiID}
	var b2 strings.Builder
	if err := tmpl.Execute(&b2, admin); err != nil {
		t.Fatalf("admin render: %v", err)
	}
	if strings.Contains(b2.String(), `id="qrbox"`) || strings.Contains(b2.String(), "/qr.png") {
		t.Error("admin page should not show the QR tile")
	}
}
