package main

import (
	"strings"
	"testing"
)

// The page must carry raw unix timestamps so the browser can reformat them
// into its own timezone, with UTC text as the no-JS fallback.
func TestTimezoneTimestamps(t *testing.T) {
	// last sample = 2026-01-19 12:00:00 UTC
	const ts = int64(1768824000)
	p := Page{
		Admin: true, Month: "2026-01", Months: []string{"2026-01"},
		LastSampleTS: ts, Rate: 88, RateNote: "live rate", RateFetchedTS: ts,
		Notice: "sampled 12 users, last sample", NoticeTS: ts,
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("admin render: %v", err)
	}
	out := b.String()

	// all three timestamps must be <time data-ts> elements
	if n := strings.Count(out, `data-ts="1768824000"`); n != 3 {
		t.Errorf("data-ts occurrences = %d, want 3 (last sample, FX fetch, notice)", n)
	}
	// UTC fallback text must be present for no-JS clients
	if !strings.Contains(out, "2026-01-19 12:00 UTC") {
		t.Error("missing UTC fallback text")
	}
	// no pre-formatted "UTC" strings outside <time> elements (notice text
	// must not embed its own timestamp anymore)
	if strings.Contains(out, "sampled 12 users, last sample 20") {
		t.Error("notice must not embed a formatted timestamp")
	}
	// the client-side reformatter must be present on the admin page too
	if !strings.Contains(out, "time[data-ts]") {
		t.Error("missing client-side timezone script")
	}

	// zero timestamps: no <time> elements, "never" shown instead
	p2 := Page{Admin: true, Month: "2026-01", Rate: 88, RateNote: "fixed rate (USD_TO_INR)"}
	var b2 strings.Builder
	if err := tmpl.Execute(&b2, p2); err != nil {
		t.Fatalf("render: %v", err)
	}
	out2 := b2.String()
	if strings.Contains(out2, "<time") {
		t.Error("no <time> elements expected when timestamps are zero")
	}
	if !strings.Contains(out2, "Last sample: never") {
		t.Error(`want "Last sample: never"`)
	}
	// fixed rate has no fetch timestamp -> no ", fetched" fragment
	if strings.Contains(out2, ", fetched") {
		t.Error("fixed rate must not show a fetched fragment")
	}
}

// Public page must carry the same timezone machinery.
func TestPublicTimezoneTimestamps(t *testing.T) {
	p := Page{Query: "42", LastSampleTS: 1768804800, Rate: 88,
		RateNote: "live rate", RateFetchedTS: 1768800000, PricePerTB: 6.95, UpiID: upiID}
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, `data-ts="1768804800"`) {
		t.Error("public page missing last-sample timestamp element")
	}
	if !strings.Contains(out, `data-ts="1768800000"`) {
		t.Error("public page missing FX fetch timestamp element")
	}
	if !strings.Contains(out, "time[data-ts]") {
		t.Error("public page missing client-side timezone script")
	}
}
