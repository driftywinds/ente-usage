package main

import (
	"os"
	"testing"
)

// TestWritePreview dumps rendered admin HTML so the layout can be eyeballed.
func TestWritePreview(t *testing.T) {
	if os.Getenv("WRITE_PREVIEW") == "" {
		t.Skip("set WRITE_PREVIEW=1 to write preview html")
	}
	rows := []Row{
		{UserID: 1042, Name: "Alice Sharma", LatestHuman: "4.21 TB", AvgHuman: "4.10 TB",
			Charge: 28.5, ChargeINR: 2508.0, NowUSD: 29.26, NowINR: 2574.9, Samples: 720, Cost: 28.0,
			Billed: true},
		{UserID: 2087, Name: "Bob", LatestHuman: "1.02 TB", AvgHuman: "990 GB",
			Charge: 6.88, ChargeINR: 605.4, NowUSD: 7.09, NowINR: 623.9, Samples: 720, Cost: 6.78,
			Billed: true},
		{UserID: 3311, LatestHuman: "120 GB", AvgHuman: "118 GB",
			Charge: 0.82, ChargeINR: 72.2, NowUSD: 0.84, NowINR: 73.8, Samples: 719, Cost: 0.81,
			Billed: false},
	}
	p := Page{
		Admin: true, Month: "2026-01", Months: []string{"2026-01", "2025-12", "2025-11"},
		Notice: "user 3311 marked paid for 2026-01", Rows: rows, MarkupPct: 10, PricePerTB: 6.95, Rate: 88,
		RateNote: "live rate", TotalLatest: "5.35 TB", TotalAvg: "5.21 TB",
		TotalCharge: 36.2, TotalChargeINR: 3185.6, TotalNowUSD: 37.19, TotalNowINR: 3272.7,
		TotalCost: 35.59, TotalCostINR: 3131.9, LastSampleTS: 1768824000, // 2026-01-19 12:00 UTC
		BilledCount: 2, BilledTotal: 3,
	}
	f, err := os.Create("preview_admin.html")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := tmpl.Execute(f, p); err != nil {
		t.Fatal(err)
	}
}
