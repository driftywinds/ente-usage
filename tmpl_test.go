package main

import (
	"strings"
	"testing"
	"time"
)

func TestTemplateRenders(t *testing.T) {
	rows := []Row{
		{UserID: 42, Name: "Alice", LatestHuman: "1.00 TB", AvgHuman: "900 GB",
			Charge: 6.255, ChargeINR: 550.4, NowUSD: 6.95, NowINR: 611.6, Samples: 30},
		{UserID: 7, LatestHuman: "100 GB", AvgHuman: "95 GB",
			Charge: 0.6603, ChargeINR: 58.1, NowUSD: 0.695, NowINR: 61.16, Samples: 30},
	}
	p := Page{
		Admin: true, Month: "2026-01", Months: []string{"2026-01", "2025-12"},
		Notice: "saved", Rows: rows, MarkupPct: 10, PricePerTB: 6.95, Rate: 88,
		TotalLatest: "1.10 TB", TotalAvg: "995 GB", TotalCharge: 6.9153,
		TotalChargeINR: 608.5, TotalNowUSD: 7.645, TotalNowINR: 672.76,
		TotalCost: 6.28, TotalCostINR: 552.6, LastSample: "never",
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("admin render: %v", err)
	}
	out := b.String()
	for _, want := range []string{"Alice", "Save name", "User ID", "2026-01", "Total"} {
		if !strings.Contains(out, want) {
			t.Errorf("admin page missing %q", want)
		}
	}

	pub := Page{Query: "42", LastSample: "never", Rate: 88, PricePerTB: 6.95}
	row := makeRow(42, 1e12, 9e11, 30)
	row.Name = "Alice"
	pub.Single = &row
	pub.History = []HistRow{{Month: "2025-12", AvgHuman: "800 GB", Charge: 5.56, ChargeINR: 489.3}}
	var b2 strings.Builder
	if err := tmpl.Execute(&b2, pub); err != nil {
		t.Fatalf("public render: %v", err)
	}
	_ = time.Now()
}

func TestSaveNameRoundTrip(t *testing.T) {
	// exercise saveName/loadNames against an in-memory db
	cfg.SQLitePath = ":memory:"
	if err := initSQLite(); err != nil {
		t.Skipf("init sqlite: %v", err)
	}
	if err := saveName(42, "  Alice  "); err != nil {
		t.Fatalf("save: %v", err)
	}
	names := loadNames()
	if names[42] != "Alice" {
		t.Errorf("got %q want Alice", names[42])
	}
	if err := saveName(42, "   "); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n := loadNames(); len(n) != 0 {
		t.Errorf("expected empty map after clear, got %v", n)
	}
}
