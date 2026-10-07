package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func freshDB(t *testing.T) {
	t.Helper()
	cfg.SQLitePath = ":memory:"
	if err := initSQLite(); err != nil {
		t.Fatalf("init sqlite: %v", err)
	}
}

func TestBillsRoundTrip(t *testing.T) {
	freshDB(t)
	month := "2026-01"

	// default: nothing billed
	if got := loadBills(month); len(got) != 0 {
		t.Fatalf("expected no bills, got %v", got)
	}

	// mark paid
	if err := setBilled(42, month, true); err != nil {
		t.Fatalf("setBilled: %v", err)
	}
	if !loadBills(month)[42] {
		t.Error("user 42 should be billed")
	}

	// toggle back to unpaid (row stays, flag flips)
	if err := setBilled(42, month, false); err != nil {
		t.Fatalf("setBilled: %v", err)
	}
	if loadBills(month)[42] {
		t.Error("user 42 should be unpaid again")
	}

	// isolation: another month unaffected
	if loadBills("2026-02")[42] {
		t.Error("billing must be per-month")
	}

	// per-user view for the public page
	if err := setBilled(42, month, true); err != nil {
		t.Fatal(err)
	}
	ub := loadUserBills(42)
	if !ub[month] {
		t.Errorf("loadUserBills missing %s: %v", month, ub)
	}
	if ub["2026-02"] {
		t.Error("loadUserBills leaked another month")
	}
}

func TestMarkAllBilled(t *testing.T) {
	freshDB(t)
	// seed two samples in 2026-01 and one in 2026-02
	start, _, _, _ := monthRange("2026-01")
	for _, s := range []struct {
		ts   int64
		user int64
	}{{start + 100, 1}, {start + 200, 2}, {start + 300, 1}} {
		if _, err := lite.Exec(`INSERT INTO samples(ts, user_id, bytes) VALUES(?,?,?)`, s.ts, s.user, 1e9); err != nil {
			t.Fatal(err)
		}
	}
	febStart, _, _, _ := monthRange("2026-02")
	if _, err := lite.Exec(`INSERT INTO samples(ts, user_id, bytes) VALUES(?,?,?)`, febStart+100, 3, 1e9); err != nil {
		t.Fatal(err)
	}

	n, err := markAllBilled("2026-01")
	if err != nil {
		t.Fatalf("markAllBilled: %v", err)
	}
	if n != 2 {
		t.Errorf("affected %d rows, want 2 (distinct users)", n)
	}
	bills := loadBills("2026-01")
	if !bills[1] || !bills[2] {
		t.Errorf("2026-01 bills = %v, want 1 and 2 billed", bills)
	}
	if bills[3] {
		t.Error("user 3 only has 2026-02 samples; must not be billed for 2026-01")
	}
	if loadBills("2026-02")[3] {
		t.Error("markAllBilled leaked into 2026-02")
	}
}

func TestBillingHandlers(t *testing.T) {
	freshDB(t)

	// GET rejected on both endpoints
	for _, h := range []http.HandlerFunc{handleAdminToggleBill, handleAdminMarkAllBilled} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/admin/bill", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET = %d, want 405", rec.Code)
		}
	}

	// toggle ON
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/bill",
		strings.NewReader("month=2026-01&user_id=7&billed=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handleAdminToggleBill(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("toggle = %d, want 303", rec.Code)
	}
	if !loadBills("2026-01")[7] {
		t.Error("user 7 should be billed after POST")
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "month=2026-01") || !strings.Contains(loc, "notice=") {
		t.Errorf("redirect = %q, want month + notice", loc)
	}

	// toggle OFF (checkbox absent -> billed field missing)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/bill",
		strings.NewReader("month=2026-01&user_id=7"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handleAdminToggleBill(rec, req)
	if loadBills("2026-01")[7] {
		t.Error("user 7 should be unpaid after unchecking")
	}

	// bad month rejected without touching db
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/bill",
		strings.NewReader("month=bogus&user_id=7&billed=true"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handleAdminToggleBill(rec, req)
	if !strings.Contains(rec.Header().Get("Location"), "month+must") &&
		!strings.Contains(strings.ReplaceAll(rec.Header().Get("Location"), "+", " "), "month must") {
		t.Errorf("bad month redirect = %q", rec.Header().Get("Location"))
	}

	// mark all
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/admin/bill/all",
		strings.NewReader("month=2026-01"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	handleAdminMarkAllBilled(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("mark all = %d, want 303", rec.Code)
	}
}

func TestAdminTableRendersBilling(t *testing.T) {
	p := Page{
		Admin: true, Month: "2026-01", Months: []string{"2026-01"},
		Rows: []Row{
			{UserID: 1, Name: "Alice", Billed: true, LatestHuman: "1 GB", AvgHuman: "1 GB"},
			{UserID: 2, Billed: false, LatestHuman: "2 GB", AvgHuman: "2 GB"},
		},
		BilledCount: 1, BilledTotal: 2, LastSampleTS: 0, Rate: 88,
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	for _, want := range []string{
		`action="/admin/bill"`,        // toggle endpoint
		`action="/admin/bill/all"`,    // mark-all endpoint
		">Billed<",                    // header column
		`class="billchk"`,             // checkbox
		"1/2",                         // footer count
		`class="unpaid"`,              // unpaid tint on row 2
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin page missing %q", want)
		}
	}
	// paid row (Alice) must NOT carry the unpaid class; row 2 must.
	aliceRow := out[strings.Index(out, "<tr"):strings.Index(out, "</tr>")]
	if strings.Contains(aliceRow, `class="unpaid"`) {
		t.Error("paid row should not be tinted")
	}
	rest := out[strings.Index(out, "</tr>"):]
	if !strings.Contains(rest, `<tr class="unpaid">`) {
		t.Error("unpaid row should carry the unpaid class")
	}
}

func TestPublicHistoryBilledColumn(t *testing.T) {
	p := Page{
		LastSampleTS: 0, Rate: 88, PricePerTB: 6.95,
		History: []HistRow{
			{Month: "2026-01", AvgHuman: "1 GB", Charge: 1, ChargeINR: 88, Billed: true},
			{Month: "2025-12", AvgHuman: "1 GB", Charge: 1, ChargeINR: 88, Billed: false},
		},
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, p); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, ">Billed<") {
		t.Error("public history missing Billed header")
	}
	// exactly one ✓ for the two rows (only the billed one)
	if n := strings.Count(out, `class="paidchk"`); n != 1 {
		t.Errorf("paid ticks = %d, want 1", n)
	}
	// no admin billing UI must leak to public page (CSS rules share names,
	// so assert on actual markup instead of substrings)
	for _, bad := range []string{`action="/admin/bill`, `class="billchk"`, `class="unpaid"`} {
		if strings.Contains(out, bad) {
			t.Errorf("public page must not contain %q", bad)
		}
	}
}
