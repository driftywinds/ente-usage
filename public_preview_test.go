package main

import (
	"os"
	"testing"
)

// TestWritePublicPreview dumps the public user page for eyeballing.
func TestWritePublicPreview(t *testing.T) {
	if os.Getenv("WRITE_PREVIEW") == "" {
		t.Skip("set WRITE_PREVIEW=1")
	}
	p := Page{Query: "1580559962386442", LastSampleTS: 1791264420, // 2026-10-06 05:27 UTC
		Rate: 96.38, RateNote: "live rate", RateFetchedTS: 1791261720, // 2026-10-06 04:42 UTC
		PricePerTB: 6.95, UpiID: upiID}
	row := makeRow(1580559962386442, 3300000000, 3100000000, 30)
	p.Single = &row
	p.History = []HistRow{
		{Month: "2026-10", AvgHuman: "3.10 GB", Charge: 0.0216, ChargeINR: 2.08, Billed: true},
		{Month: "2026-09", AvgHuman: "2.95 GB", Charge: 0.0205, ChargeINR: 1.98, Billed: true},
		{Month: "2026-08", AvgHuman: "2.40 GB", Charge: 0.0167, ChargeINR: 1.61, Billed: false},
	}
	f, err := os.Create("preview_public.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tmpl.Execute(f, p); err != nil {
		t.Fatal(err)
	}
}
