package main

import (
	_ "embed"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// qrPNG is the UPI QR code shipped with the binary (qr.png in the repo root).
//
//go:embed qr.png
var qrPNG []byte

// upiID is copied to the user's clipboard when they tap the QR image.
const upiID = "amoghrammohantiwari@okicici"

type Config struct {
	DatabaseURL string        // Ente Postgres (read-only queries, sampled on a timer)
	SQLitePath  string        // the app's own data
	Interval    time.Duration // how often to sample Postgres
	PricePerTB  float64       // B2 USD per TB per month (decimal TB = 1e12 bytes)
	MarkupPct   float64       // optional margin on top of B2 cost
	FallbackINR float64       // USD->INR rate used if the live lookup fails
	FXLive      bool          // fetch the live USD->INR rate
	AdminUser   string
	AdminPass   string
	Listen      string
	PublicBase  string // optional external origin for admin -> public links
}

type Row struct {
	UserID      int64   `json:"user_id"`
	Name        string  `json:"name"`
	Latest      int64   `json:"latest_bytes"`
	LatestHuman string  `json:"latest_human"`
	AvgBytes    int64   `json:"avg_bytes"`
	AvgHuman    string  `json:"avg_human"`
	Samples     int     `json:"samples"`
	Cost        float64 `json:"b2_cost_usd"`
	Charge      float64 `json:"charge_usd"`
	CostINR     float64 `json:"b2_cost_inr"`
	NowUSD      float64 `json:"current_rate_usd_per_month"`
	NowINR      float64 `json:"current_rate_inr_per_month"`
	ChargeINR   float64 `json:"charge_inr"`
	Billed      bool    `json:"billed"`
}

type HistRow struct {
	Month    string
	AvgHuman  string
	Charge    float64
	ChargeINR float64
	Billed    bool
}

type Page struct {
	Rows        []Row
	Single      *Row
	History     []HistRow
	Months      []string
	Month       string
	Query       string
	Error       string
	Admin       bool
	TotalLatest string
	TotalAvg    string
	TotalCost   float64
	TotalCharge float64
	TotalCostINR   float64
	TotalNowUSD    float64
	TotalNowINR    float64
	TotalChargeINR float64
	Rate         float64
	RateNote     string
	RateFetchedTS int64 // unix ts of the FX fetch; 0 = fixed rate (no timestamp)
	PricePerTB   float64
	MarkupPct    float64
	LastSampleTS int64 // unix ts of the newest sample; 0 = never
	Notice       string
	NoticeTS     int64 // optional unix ts appended to a notice (e.g. poll time)
	UpiID        string
	BilledCount  int // users marked paid for the selected month
	BilledTotal  int // users shown for the selected month
	Base         string // external origin for admin -> public lookup links
}

var (
	cfg  Config
	pg   *sql.DB
	lite *sql.DB
	tmpl = template.Must(template.New("p").Funcs(template.FuncMap{
		// ts renders a unix timestamp as a <time> element carrying the raw
		// value, so the browser can reformat it into its own timezone.
		// The inner text is a UTC fallback for no-JS clients.
		"ts": func(v int64) template.HTML {
			if v <= 0 {
				return ""
			}
			return template.HTML(fmt.Sprintf(`<time data-ts="%d">%s</time>`,
				v, time.Unix(v, 0).UTC().Format("2006-01-02 15:04 UTC")))
		},
		// pct computes the width percentage for the paid progress meter.
		"pct": func(a, b int) int {
			if b <= 0 {
				return 0
			}
			p := int(math.Round(float64(a) / float64(b) * 100.0))
			if p > 100 {
				p = 100
			}
			return p
		},
	}).Parse(pageHTML))

	sampleMu sync.Mutex // serializes samplerLoop and the on-demand admin poll
)

// ---------- helpers ----------

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func human(b int64) string {
	const unit = 1000.0 // decimal, matches how B2 bills
	f := float64(b)
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", b)
	}
	return fmt.Sprintf("%.2f %s", f, units[i])
}

func costs(avgBytes float64) (cost, charge float64) {
	cost = avgBytes / 1e12 * cfg.PricePerTB
	return cost, cost * (1 + cfg.MarkupPct/100)
}

func makeRow(id, latest int64, avg float64, n int) Row {
	cost, charge := costs(avg)
	rate, _, _ := currentRate()
	now := float64(latest) / 1e12 * cfg.PricePerTB * (1 + cfg.MarkupPct/100)
	return Row{
		UserID: id, Latest: latest, LatestHuman: human(latest),
		AvgBytes: int64(avg), AvgHuman: human(int64(avg)),
		Samples: n, Cost: cost, Charge: charge,
		CostINR: cost * rate, ChargeINR: charge * rate,
		NowUSD: now, NowINR: now * rate,
	}
}

// monthRange returns the unix range [start,end) for "YYYY-MM" (UTC); empty = current month.
func monthRange(m string) (start, end int64, label string, err error) {
	if m == "" {
		m = time.Now().UTC().Format("2006-01")
	}
	t, err := time.Parse("2006-01", m)
	if err != nil {
		return 0, 0, "", err
	}
	return t.Unix(), t.AddDate(0, 1, 0).Unix(), m, nil
}

// lastSample returns the unix ts of the newest sample (0 = never).
func lastSample() int64 {
	var ts sql.NullInt64
	lite.QueryRow(`SELECT MAX(ts) FROM samples`).Scan(&ts)
	if !ts.Valid {
		return 0
	}
	return ts.Int64
}

// ---------- USD -> INR rate ----------

var (
	rateMu        sync.RWMutex
	usdInr        float64
	rateNote      string
	rateFetchedTS int64 // unix ts of last successful FX fetch; 0 = fixed rate
)

// currentRate returns the rate, a short label, and the unix ts when the
// rate was fetched (0 when it's a fixed/fallback rate with no timestamp).
func currentRate() (float64, string, int64) {
	rateMu.RLock()
	defer rateMu.RUnlock()
	return usdInr, rateNote, rateFetchedTS
}

func setRate(r float64, note string, fetched int64) {
	rateMu.Lock()
	usdInr, rateNote, rateFetchedTS = r, note, fetched
	rateMu.Unlock()
}

func fetchRate() error {
	c := http.Client{Timeout: 10 * time.Second}
	resp, err := c.Get("https://open.er-api.com/v6/latest/USD")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var d struct {
		Result string             `json:"result"`
		Rates  map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return err
	}
	r := d.Rates["INR"]
	if d.Result != "success" || r <= 0 {
		return fmt.Errorf("unexpected FX response")
	}
	setRate(r, "live rate", time.Now().Unix())
	return nil
}

func fxLoop() {
	for {
		if err := fetchRate(); err != nil {
			log.Println("fx fetch failed (keeping previous rate):", err)
		}
		time.Sleep(12 * time.Hour)
	}
}

// ---------- SQLite ----------

func initSQLite() error {
	var err error
	dsn := "file:" + cfg.SQLitePath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	lite, err = sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	lite.SetMaxOpenConns(4)
	_, err = lite.Exec(`
		CREATE TABLE IF NOT EXISTS samples (
			ts      INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			bytes   INTEGER NOT NULL,
			PRIMARY KEY (ts, user_id)
		);
		CREATE INDEX IF NOT EXISTS idx_samples_user_ts ON samples(user_id, ts);
		CREATE TABLE IF NOT EXISTS user_names (
			user_id    INTEGER PRIMARY KEY,
			name       TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS bills (
			user_id    INTEGER NOT NULL,
			month      TEXT NOT NULL,
			billed     INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (user_id, month)
		);`)
	return err
}

// ---------- user names (admin-assigned labels for user IDs) ----------

// loadNames returns user_id -> assigned name.
func loadNames() map[int64]string {
	names := map[int64]string{}
	rs, err := lite.Query(`SELECT user_id, name FROM user_names`)
	if err != nil {
		log.Println("loadNames:", err)
		return names
	}
	defer rs.Close()
	for rs.Next() {
		var id int64
		var n string
		if err := rs.Scan(&id, &n); err != nil {
			log.Println("loadNames scan:", err)
			continue
		}
		names[id] = n
	}
	return names
}

// saveName stores the name for a user ID. An empty name removes the label.
func saveName(id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		_, err := lite.Exec(`DELETE FROM user_names WHERE user_id = ?`, id)
		return err
	}
	_, err := lite.Exec(`
		INSERT INTO user_names(user_id, name, updated_at) VALUES(?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at`,
		id, name, time.Now().Unix())
	return err
}

// ---------- billing (admin-checked "paid" flag per user + month) ----------

// validMonth reports whether m looks like "YYYY-MM".
func validMonth(m string) bool {
	_, err := time.Parse("2006-01", m)
	return err == nil
}

// loadBills returns user_id -> billed for a month ("YYYY-MM").
func loadBills(month string) map[int64]bool {
	bills := map[int64]bool{}
	rs, err := lite.Query(`SELECT user_id, billed FROM bills WHERE month = ?`, month)
	if err != nil {
		log.Println("loadBills:", err)
		return bills
	}
	defer rs.Close()
	for rs.Next() {
		var id int64
		var b int
		if err := rs.Scan(&id, &b); err != nil {
			log.Println("loadBills scan:", err)
			continue
		}
		bills[id] = b != 0
	}
	return bills
}

// loadUserBills returns month -> billed (only months with a stored row)
// for one user, for the public history table.
func loadUserBills(userID int64) map[string]bool {
	bills := map[string]bool{}
	rs, err := lite.Query(`SELECT month, billed FROM bills WHERE user_id = ?`, userID)
	if err != nil {
		log.Println("loadUserBills:", err)
		return bills
	}
	defer rs.Close()
	for rs.Next() {
		var m string
		var b int
		if err := rs.Scan(&m, &b); err != nil {
			log.Println("loadUserBills scan:", err)
			continue
		}
		bills[m] = b != 0
	}
	return bills
}

// setBilled stores the paid/unpaid flag for a user in a month.
func setBilled(id int64, month string, billed bool) error {
	v := 0
	if billed {
		v = 1
	}
	_, err := lite.Exec(`
		INSERT INTO bills(user_id, month, billed, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(user_id, month) DO UPDATE SET billed = excluded.billed, updated_at = excluded.updated_at`,
		id, month, v, time.Now().Unix())
	return err
}

// markAllBilled flags every user with samples in the given month as paid.
func markAllBilled(month string) (int64, error) {
	start, end, _, err := monthRange(month)
	if err != nil {
		return 0, err
	}
	res, err := lite.Exec(`
		INSERT INTO bills(user_id, month, billed, updated_at)
		SELECT DISTINCT user_id, ?, 1, ? FROM samples WHERE ts >= ? AND ts < ?
		ON CONFLICT(user_id, month) DO UPDATE SET billed = 1, updated_at = excluded.updated_at`,
		month, time.Now().Unix(), start, end)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------- Postgres -> SQLite sampler ----------

// sampleOnce pulls the current usage from Postgres and stores one sample
// per user. It returns the number of users sampled. The mutex serializes
// the timer loop and the on-demand admin trigger.
func sampleOnce() (int, error) {
	sampleMu.Lock()
	defer sampleMu.Unlock()

	rows, err := pg.Query(`
		SELECT u.user_id, us.storage_consumed
		FROM usage us
		JOIN users u ON u.user_id = us.user_id`)
	if err != nil {
		return 0, err
	}
	type pair struct{ id, bytes int64 }
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.bytes); err != nil {
			rows.Close()
			return 0, err
		}
		got = append(got, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := lite.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO samples(ts, user_id, bytes) VALUES (?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()
	ts := time.Now().Unix()
	for _, p := range got {
		if _, err := stmt.Exec(ts, p.id, p.bytes); err != nil {
			tx.Rollback()
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	log.Printf("sampled %d users", len(got))
	return len(got), nil
}

func samplerLoop() {
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for range t.C {
		if _, err := sampleOnce(); err != nil {
			log.Println("sample failed:", err)
		}
	}
}

// ---------- handlers ----------

// publicBase returns the externally visible origin (scheme://host) used to
// build admin -> public lookup links. PUBLIC_BASE_URL wins if set; otherwise
// it is derived from the incoming request, honouring X-Forwarded-Proto so it
// works behind a TLS-terminating reverse proxy.
func publicBase(r *http.Request) string {
	if cfg.PublicBase != "" {
		return strings.TrimRight(cfg.PublicBase, "/")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// Public: serves the embedded UPI QR image with a long cache (it's static).
func handleQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(qrPNG)
}

// Public: a user enters their own ID and sees only their own usage.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	p := Page{PricePerTB: cfg.PricePerTB, MarkupPct: cfg.MarkupPct, LastSampleTS: lastSample(), UpiID: upiID}
	p.Rate, p.RateNote, p.RateFetchedTS = currentRate()
	q := r.URL.Query().Get("id")
	if q == "" {
		render(w, p)
		return
	}
	p.Query = q
	id, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		p.Error = "User ID must be a number."
		render(w, p)
		return
	}

	start, end, _, _ := monthRange("")
	var latest sql.NullInt64
	var avg sql.NullFloat64
	var n int
	err = lite.QueryRow(`
		SELECT (SELECT bytes FROM samples WHERE user_id = ? ORDER BY ts DESC LIMIT 1),
		       AVG(bytes), COUNT(*)
		FROM samples WHERE user_id = ? AND ts >= ? AND ts < ?`,
		id, id, start, end).Scan(&latest, &avg, &n)
	if err != nil {
		log.Println("lookup:", err)
		p.Error = "Something went wrong."
		render(w, p)
		return
	}
	if !latest.Valid {
		p.Error = "No usage found for that ID."
		render(w, p)
		return
	}
	row := makeRow(id, latest.Int64, avg.Float64, n)
	p.Single = &row

	userBills := loadUserBills(id)
	hr, err := lite.Query(`
		SELECT strftime('%Y-%m', ts, 'unixepoch') AS m, AVG(bytes)
		FROM samples WHERE user_id = ?
		GROUP BY m ORDER BY m DESC LIMIT 12`, id)
	if err == nil {
		defer hr.Close()
		for hr.Next() {
			var m string
			var a float64
			if hr.Scan(&m, &a) == nil {
				_, charge := costs(a)
				p.History = append(p.History, HistRow{Month: m, AvgHuman: human(int64(a)),
					Charge: charge, ChargeINR: charge * p.Rate, Billed: userBills[m]})
			}
		}
	}
	render(w, p)
}

// Admin: all users for a month (?month=YYYY-MM), with totals.
func handleAdmin(w http.ResponseWriter, r *http.Request) {
	start, end, label, err := monthRange(r.URL.Query().Get("month"))
	if err != nil {
		http.Error(w, "month must look like 2026-10", 400)
		return
	}
	rs, err := lite.Query(`
		SELECT user_id, AVG(bytes), COUNT(*),
		       (SELECT bytes FROM samples s2
		        WHERE s2.user_id = s.user_id AND s2.ts >= ? AND s2.ts < ?
		        ORDER BY s2.ts DESC LIMIT 1)
		FROM samples s
		WHERE ts >= ? AND ts < ?
		GROUP BY user_id
		ORDER BY AVG(bytes) DESC`, start, end, start, end)
	if err != nil {
		log.Println("admin:", err)
		http.Error(w, "db error", 500)
		return
	}
	p := Page{Admin: true, Month: label, PricePerTB: cfg.PricePerTB, MarkupPct: cfg.MarkupPct, LastSampleTS: lastSample()}
	p.Rate, p.RateNote, p.RateFetchedTS = currentRate()
	p.Base = publicBase(r)
	p.Notice = r.URL.Query().Get("notice")
	if tsStr := r.URL.Query().Get("ts"); tsStr != "" {
		if ts, err := strconv.ParseInt(tsStr, 10, 64); err == nil && ts > 0 {
			p.NoticeTS = ts
		}
	}
	names := loadNames()
	bills := loadBills(label)
	var totLatest, totAvg int64
	for rs.Next() {
		var id, latest int64
		var avg float64
		var n int
		if err := rs.Scan(&id, &avg, &n, &latest); err != nil {
			rs.Close()
			http.Error(w, "db error", 500)
			return
		}
		row := makeRow(id, latest, avg, n)
		row.Name = names[id]
		row.Billed = bills[id]
		p.Rows = append(p.Rows, row)
		p.BilledTotal++
		if row.Billed {
			p.BilledCount++
		}
		totLatest += row.Latest
		totAvg += row.AvgBytes
		p.TotalCost += row.Cost
		p.TotalCharge += row.Charge
		p.TotalNowUSD += row.NowUSD
	}
	rs.Close()
	p.TotalLatest, p.TotalAvg = human(totLatest), human(totAvg)
	p.TotalCostINR, p.TotalChargeINR = p.TotalCost*p.Rate, p.TotalCharge*p.Rate
	p.TotalNowINR = p.TotalNowUSD * p.Rate

	if mr, err := lite.Query(`SELECT DISTINCT strftime('%Y-%m', ts, 'unixepoch') m FROM samples ORDER BY m DESC`); err == nil {
		for mr.Next() {
			var m string
			if mr.Scan(&m) == nil {
				p.Months = append(p.Months, m)
			}
		}
		mr.Close()
	}

	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p.Rows)
		return
	}
	render(w, p)
}

// Admin: assign (or clear) a display name for a user ID.
func handleAdminSaveName(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	notice := "saved"
	idStr := strings.TrimSpace(r.FormValue("user_id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if idStr == "" {
		notice = "user ID is required"
	} else if id, err := strconv.ParseInt(idStr, 10, 64); err != nil {
		notice = "user ID must be a number"
	} else if len(name) > 100 {
		notice = "name too long (max 100 chars)"
	} else if err := saveName(id, name); err != nil {
		log.Println("saveName:", err)
		notice = "database error"
	}
	q := url.Values{}
	if month != "" {
		q.Set("month", month)
	}
	q.Set("notice", notice)
	http.Redirect(w, r, "/admin?"+q.Encode(), http.StatusSeeOther)
}

// Admin: pull fresh usage from Postgres on demand, then redirect back.
func handleAdminSample(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	q := url.Values{}
	if m := strings.TrimSpace(r.FormValue("month")); m != "" {
		q.Set("month", m)
	}
	n, err := sampleOnce()
	var notice string
	switch {
	case err != nil:
		log.Println("manual sample failed:", err)
		notice = "sample failed: " + err.Error()
	case n == 0:
		notice = "sample ran but no users were returned"
	default:
		// timestamp travels separately so the browser can show it in local time
		notice = fmt.Sprintf("sampled %d users, last sample", n)
		q.Set("ts", strconv.FormatInt(time.Now().Unix(), 10))
	}
	q.Set("notice", notice)
	http.Redirect(w, r, "/admin?"+q.Encode(), http.StatusSeeOther)
}

// Admin: mark one user's month as paid/unpaid.
func handleAdminToggleBill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	month := strings.TrimSpace(r.FormValue("month"))
	notice := "billing updated"
	switch {
	case !validMonth(month):
		notice = "month must look like 2026-10"
	default:
		id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("user_id")), 10, 64)
		if err != nil {
			notice = "user ID must be a number"
			break
		}
		// checkbox sends "true" when checked; absent means unchecked
		billed := r.FormValue("billed") == "true"
		if err := setBilled(id, month, billed); err != nil {
			log.Println("setBilled:", err)
			notice = "database error"
			break
		}
		state := "unpaid"
		if billed {
			state = "paid"
		}
		notice = fmt.Sprintf("user %d marked %s for %s", id, state, month)
	}
	q := url.Values{}
	q.Set("month", month)
	q.Set("notice", notice)
	http.Redirect(w, r, "/admin?"+q.Encode(), http.StatusSeeOther)
}

// Admin: mark every user with samples in the month as paid.
func handleAdminMarkAllBilled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	_ = r.ParseForm()
	month := strings.TrimSpace(r.FormValue("month"))
	notice := "billing updated"
	if !validMonth(month) {
		notice = "month must look like 2026-10"
	} else if n, err := markAllBilled(month); err != nil {
		log.Println("markAllBilled:", err)
		notice = "database error"
	} else {
		notice = fmt.Sprintf("marked %d users as paid for %s", n, month)
	}
	q := url.Values{}
	q.Set("month", month)
	q.Set("notice", notice)
	http.Redirect(w, r, "/admin?"+q.Encode(), http.StatusSeeOther)
}

func render(w http.ResponseWriter, p Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, p); err != nil {
		log.Println("render:", err)
	}
}

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		okUser := subtle.ConstantTimeCompare([]byte(u), []byte(cfg.AdminUser)) == 1
		okPass := subtle.ConstantTimeCompare([]byte(p), []byte(cfg.AdminPass)) == 1
		if !ok || !okUser || !okPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="admin"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ---------- main ----------

func main() {
	interval, err := time.ParseDuration(env("SAMPLE_INTERVAL", "1h"))
	if err != nil || interval < time.Minute {
		log.Fatal("SAMPLE_INTERVAL must be a duration of at least 1m, e.g. 1h")
	}
	cfg = Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		SQLitePath:  env("SQLITE_PATH", "/data/usage.db"),
		Interval:    interval,
		PricePerTB:  envFloat("B2_PRICE_PER_TB", 6.95),
		MarkupPct:   envFloat("MARKUP_PERCENT", 0),
		FallbackINR: envFloat("USD_TO_INR", 88),
		FXLive:      env("FX_LIVE", "true") != "false",
		AdminUser:   env("ADMIN_USER", "admin"),
		AdminPass:   os.Getenv("ADMIN_PASS"),
		Listen:      env("LISTEN_ADDR", ":8080"),
		PublicBase:  os.Getenv("PUBLIC_BASE_URL"),
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	setRate(cfg.FallbackINR, "fixed rate (USD_TO_INR)", 0)
	if cfg.FXLive {
		go fxLoop()
	}

	if err := initSQLite(); err != nil {
		log.Fatal("sqlite: ", err)
	}

	pg, err = sql.Open("postgres", cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	pg.SetMaxOpenConns(1) // only ever used by the sampler
	pg.SetConnMaxLifetime(5 * time.Minute)
	if err := pg.Ping(); err != nil {
		log.Fatal("cannot reach postgres: ", err)
	}

	if _, err := sampleOnce(); err != nil {
		log.Println("initial sample failed:", err)
	}
	go samplerLoop()

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/qr.png", handleQR)
	if cfg.AdminPass != "" {
		http.HandleFunc("/admin", basicAuth(handleAdmin))
		http.HandleFunc("/admin/name", basicAuth(handleAdminSaveName))
		http.HandleFunc("/admin/sample", basicAuth(handleAdminSample))
		http.HandleFunc("/admin/bill", basicAuth(handleAdminToggleBill))
		http.HandleFunc("/admin/bill/all", basicAuth(handleAdminMarkAllBilled))
	} else {
		log.Println("ADMIN_PASS not set: /admin is disabled")
	}

	log.Println("listening on", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}

const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<meta name="theme-color" content="#f4f6fb" media="(prefers-color-scheme:light)">
<meta name="theme-color" content="#0b0f1d" media="(prefers-color-scheme:dark)">
<title>Ente storage usage</title>
<link rel="icon" href="data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 64 64'%3E%3Crect width='64' height='64' rx='14' fill='%234f46e5'/%3E%3Cpath d='M18 46V30M32 46V16M46 46v-22' stroke='white' stroke-width='7' stroke-linecap='round' fill='none'/%3E%3C/svg%3E">
<style>
:root{
  color-scheme:light dark;
  --bg:#f3f5fa;--glow:#e6ebff;
  --card:#ffffff;--card2:#f7f9fc;
  --ink:#0f172a;--ink2:#415065;--muted:#69758a;
  --line:#e4e7ef;--line2:#d6dbe7;
  --zebra:#f8fafc;--hover:#eef1fa;
  --accent:#4f46e5;--accent2:#8b5cf6;--accent-soft:#ececfe;--accent-ink:#ffffff;
  --ok:#0c8a5f;--ok-bg:#e8f7ef;--ok-line:#bee5d4;
  --warn:#b45309;--warn-bg:#fef3e1;
  --err:#c2334d;--err-bg:#fde8ec;--err-line:#f3c3ce;
  --ring:rgba(79,70,229,.16);
  --shadow:0 1px 2px rgba(15,23,42,.04),0 12px 32px -16px rgba(15,23,42,.18);
  --radius:16px;
}
@media (prefers-color-scheme:dark){
  :root{
    --bg:#000000;--glow:#05070d;
    --card:#0d1117;--card2:#11161f;
    --ink:#e8ecf6;--ink2:#c3cbdc;--muted:#8d97ad;
    --line:#1e242e;--line2:#2a313e;
    --zebra:#0a0d13;--hover:#151b26;
    --accent:#818cf8;--accent2:#a78bfa;--accent-soft:#1b2140;--accent-ink:#0b0f1d;
    --ok:#4ade80;--ok-bg:#0c211a;--ok-line:#173d2c;
    --warn:#fbbf24;--warn-bg:#241c0d;
    --err:#f87171;--err-bg:#291318;--err-line:#4a2027;
    --ring:rgba(129,140,248,.28);
    --shadow:0 1px 2px rgba(0,0,0,.7),0 16px 40px -18px rgba(0,0,0,.85);
  }
}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{
  margin:0;font-family:system-ui,-apple-system,"Segoe UI",Roboto,"Helvetica Neue",Arial,sans-serif;
  color:var(--ink);line-height:1.55;
  background:var(--bg);background-image:radial-gradient(1100px 480px at 50% -260px,var(--glow),transparent 72%);
  -webkit-font-smoothing:antialiased;text-rendering:optimizeLegibility
}
.wrap{max-width:880px;margin:0 auto;padding:2.4rem 1.15rem 3.5rem}
body.wide .wrap{max-width:1500px}
.top{display:flex;align-items:center;gap:.95rem;margin-bottom:1.5rem}
.logo{
  width:46px;height:46px;border-radius:13px;flex:none;display:flex;align-items:center;justify-content:center;color:#fff;
  background:linear-gradient(135deg,var(--accent),var(--accent2));
  box-shadow:0 12px 26px -12px rgba(99,91,255,.7)
}
.top h1{margin:0;font-size:1.32rem;font-weight:700;letter-spacing:-.02em}
.sub{color:var(--muted);font-size:.87rem;margin-top:.08rem}
.badge{
  margin-left:auto;display:inline-flex;align-items:center;gap:.45rem;
  background:var(--card);border:1px solid var(--line2);color:var(--ink2);
  border-radius:99px;padding:.3rem .85rem;font-size:.78rem;font-weight:600
}
.badge i{width:8px;height:8px;border-radius:50%;background:var(--ok);box-shadow:0 0 0 3px var(--ok-bg)}
.card{background:var(--card);border:1px solid var(--line);border-radius:var(--radius);box-shadow:var(--shadow);padding:1.4rem;margin-top:1.15rem}
.cardtitle{margin:0 0 .8rem;font-size:1.02rem;font-weight:700;letter-spacing:-.01em}
input,select,button{font:inherit}
input,select{
  color:var(--ink);background:var(--card);border:1px solid var(--line2);border-radius:10px;
  padding:.55rem .8rem;transition:border-color .15s,box-shadow .15s
}
input::placeholder{color:var(--muted)}
input:focus-visible,select:focus-visible,button:focus-visible{outline:none;border-color:var(--accent);box-shadow:0 0 0 4px var(--ring)}
select{
  appearance:none;-webkit-appearance:none;font-weight:600;
  background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='6'%3E%3Cpath d='M1 1l4 4 4-4' stroke='%2369758a' stroke-width='1.6' fill='none' stroke-linecap='round'/%3E%3C/svg%3E");
  background-repeat:no-repeat;background-position:right .85rem center;padding-right:2.2rem
}
@media (prefers-color-scheme:dark){
  select{
    background-image:url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='6'%3E%3Cpath d='M1 1l4 4 4-4' stroke='%238d97ad' stroke-width='1.6' fill='none' stroke-linecap='round'/%3E%3C/svg%3E")
  }
}
.btn{
  display:inline-flex;align-items:center;justify-content:center;gap:.45rem;white-space:nowrap;cursor:pointer;
  background:var(--card);border:1px solid var(--line2);color:var(--ink);border-radius:10px;
  padding:.55rem 1rem;font-weight:600;font-size:.93rem;
  transition:background .15s,border-color .15s,color .15s,box-shadow .15s,transform .06s
}
.btn:hover{background:var(--hover);border-color:var(--accent);color:var(--accent)}
.btn:active{transform:translateY(1px)}
.btn.primary{
  border:1px solid transparent;color:var(--accent-ink);
  background:linear-gradient(135deg,var(--accent),var(--accent2));
  box-shadow:0 12px 26px -12px rgba(99,91,255,.75)
}
.btn.primary:hover{filter:brightness(1.06);color:var(--accent-ink)}
.chip{
  display:inline-flex;align-items:center;gap:.4rem;margin-left:auto;
  background:var(--card);border:1px solid var(--line2);border-radius:99px;
  padding:.4rem .95rem;font-size:.85rem;font-weight:600;color:var(--ink2);text-decoration:none;
  transition:border-color .15s,color .15s
}
.chip:hover{border-color:var(--accent);color:var(--accent)}
.ok,.err{display:flex;align-items:center;gap:.5rem;border-radius:12px;padding:.65rem 1rem;margin-top:1.15rem;font-weight:600;font-size:.92rem}
.ok{background:var(--ok-bg);color:var(--ok);border:1px solid var(--ok-line)}
.err{background:var(--err-bg);color:var(--err);border:1px solid var(--err-line)}
.muted{color:var(--muted);font-size:.9rem}
.meta{color:var(--muted);font-size:.84rem;margin-top:1.5rem;line-height:1.75}
.lookup{display:flex;gap:.65rem;margin-top:1rem}
.lookup input{flex:1 1 auto;min-width:0;padding:.85rem 1rem;font-size:1.02rem;border-radius:12px}
.cardhead{display:flex;justify-content:space-between;align-items:flex-start;gap:1.25rem;flex-wrap:wrap;flex-direction:row}
.bignum{
  font-size:clamp(2.2rem,5.5vw,2.9rem);font-weight:800;letter-spacing:-.03em;line-height:1.1;margin:.25rem 0;
  font-variant-numeric:tabular-nums;background:linear-gradient(120deg,var(--accent),var(--accent2));
  -webkit-background-clip:text;background-clip:text;color:transparent;-webkit-text-fill-color:transparent
}
.stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(215px,1fr));gap:.95rem;margin-top:1.15rem}
.stat{background:var(--card2);border:1px solid var(--line);border-radius:13px;padding:1rem 1.1rem}
.stat .lab{font-size:.78rem;font-weight:600;color:var(--muted)}
.stat .usd{font-size:1.42rem;font-weight:700;letter-spacing:-.02em;margin-top:.12rem;font-variant-numeric:tabular-nums}
.stat .inr{color:var(--muted);font-size:.9rem;font-variant-numeric:tabular-nums}
.meter{height:8px;border-radius:99px;background:var(--line);overflow:hidden;margin-top:.5rem}
.meter i{display:block;height:100%;border-radius:99px;background:linear-gradient(90deg,var(--accent),var(--accent2))}
table{border-collapse:collapse;width:100%}
.card table tbody tr:last-child td{border-bottom:none}
th,td{padding:.6rem .8rem;text-align:left;vertical-align:middle;border-bottom:1px solid var(--line)}
thead th{font-size:.72rem;font-weight:700;text-transform:uppercase;letter-spacing:.07em;color:var(--muted);background:var(--card2)}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums;white-space:nowrap}
.qrbox{
  display:flex;flex-direction:column;align-items:center;gap:.4rem;cursor:pointer;margin-left:auto;flex-shrink:0;
  background:#fff;border:1px solid var(--line2);border-radius:14px;padding:.6rem;
  user-select:none;-webkit-user-select:none;-webkit-tap-highlight-color:transparent;
  transition:box-shadow .15s,border-color .15s,transform .1s
}
.qrbox:hover,.qrbox:focus-visible{border-color:var(--accent);box-shadow:0 10px 26px -12px rgba(31,41,92,.4);outline:none}
.qrbox:active{transform:scale(.96)}
.qrbox img{display:block;width:132px;height:132px;background:#fff;border-radius:8px}
.qrcap{font-size:.74rem;color:#5b6472;text-align:center;white-space:nowrap}
.qrcap.done{color:#0c8a5f;font-weight:700}
.toolbar{display:flex;gap:.65rem;align-items:center;flex-wrap:wrap;margin-top:1.15rem}
.toolbar form{display:inline-flex}
.toolbar select{min-width:8.5rem}
.assign{display:flex;gap:.75rem;align-items:center;flex-wrap:wrap}
.nameform{display:flex;gap:.6rem;align-items:center;flex-wrap:wrap}
.nameform label{font-size:.85rem;font-weight:600;color:var(--ink2)}
.nameform input[type=number]{width:8.5rem}
.nameform input[type=text]{width:11rem}
.nameform .btn{padding:.5rem .9rem}
.nameform.inline input[type=text]{width:9.5rem}
.pill{display:inline-block;background:var(--accent-soft);color:var(--accent);border-radius:99px;padding:.18rem .72rem;font-size:.84rem;font-weight:600}
a.uidlink{color:var(--accent);text-decoration:none;font-weight:600}
a.uidlink:hover{text-decoration:underline}
.billedcell{text-align:center}
.billchk{width:1.3rem;height:1.3rem;cursor:pointer;accent-color:var(--accent)}
.billform{display:inline-flex}
body.wide table{font-size:.93rem}
body.wide td{padding:.72rem 1rem;white-space:nowrap}
body.wide tbody tr:nth-child(even){background:var(--zebra)}
body.wide tbody tr:hover{background:var(--hover)}
body.wide tfoot td{font-weight:700;background:var(--card2);border-top:2px solid var(--line2);border-bottom:none}
body.wide tr.unpaid{background:var(--warn-bg) !important}
body.wide tr.unpaid td.namecell{box-shadow:inset 3px 0 0 var(--warn)}
.tblcard{padding:0;overflow:hidden}
.tablewrap{overflow-x:auto}
.paidchk{color:var(--ok);font-weight:800;font-size:1.05rem}
.pending{display:inline-block;font-size:.73rem;font-weight:600;color:var(--warn);background:var(--warn-bg);border-radius:99px;padding:.1rem .6rem;line-height:1.4}
.billcount{font-variant-numeric:tabular-nums}
tfoot td{font-weight:700}
</style>
</head>
<body{{if .Admin}} class="wide"{{end}}>
<div class="wrap">
<header class="top">
  <div class="logo" aria-hidden="true"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.8" stroke-linecap="round"><path d="M5 20V11M12 20V4M19 20v-7"/><path d="M5 20V10M12 20V3M19 20v-6" stroke-opacity=".45" stroke-width="2"/></svg></div>
  <div>
    <h1>Ente storage usage</h1>
    <div class="sub">Per-user storage, cost estimates &amp; payment tracking</div>
  </div>
  {{if .Admin}}<span class="badge"><i></i>Admin</span>{{end}}
</header>

{{if .Admin}}
  {{if .Notice}}<p class="ok">&#10003; {{.Notice}}{{if .NoticeTS}} {{ts .NoticeTS}}{{end}}</p>{{end}}

  <div class="toolbar">
    <form method="get">
      <select name="month" onchange="this.form.submit()" aria-label="Month">
        {{$m := .Month}}
        {{range .Months}}<option value="{{.}}" {{if eq . $m}}selected{{end}}>{{.}}</option>{{end}}
        {{if not .Months}}<option>{{.Month}}</option>{{end}}
      </select>
      <noscript><button class="btn">Go</button></noscript>
    </form>
    <form method="post" action="/admin/sample">
      <input type="hidden" name="month" value="{{.Month}}">
      <button class="btn primary" title="Pull fresh usage from Postgres now">&#8635; Poll latest stats</button>
    </form>
    <form method="post" action="/admin/bill/all"
          onsubmit="return confirm('Mark ALL users as paid for {{.Month}}?')">
      <input type="hidden" name="month" value="{{.Month}}">
      <button class="btn" title="Tick every user in this month as paid">&#10003; Mark all paid</button>
    </form>
    <a class="chip" href="/admin?month={{.Month}}&format=json">JSON</a>
  </div>

  <div class="stats">
    <div class="stat"><div class="lab">Users</div><div class="usd">{{.BilledTotal}}</div><div class="inr">with samples in {{.Month}}</div></div>
    <div class="stat"><div class="lab">Total stored</div><div class="usd">{{.TotalLatest}}</div><div class="inr">month average {{.TotalAvg}}</div></div>
    <div class="stat"><div class="lab">Month charge</div><div class="usd">${{printf "%.2f" .TotalCharge}}</div><div class="inr">&#8377;{{printf "%.2f" .TotalChargeINR}}</div></div>
    <div class="stat"><div class="lab">At current usage</div><div class="usd">${{printf "%.2f" .TotalNowUSD}}</div><div class="inr">&#8377;{{printf "%.2f" .TotalNowINR}} per month</div></div>
    <div class="stat"><div class="lab">Paid this month</div><div class="usd">{{.BilledCount}}/{{.BilledTotal}}</div><div class="meter"><i style="width:{{pct .BilledCount .BilledTotal}}%"></i></div></div>
  </div>

  <div class="card assign">
    <form method="post" action="/admin/name" class="nameform">
      <input type="hidden" name="month" value="{{.Month}}">
      <label for="a-id">User ID</label>
      <input type="number" id="a-id" name="user_id" placeholder="e.g. 1234" required>
      <label for="a-name">Name</label>
      <input type="text" id="a-name" name="name" placeholder="person's name" maxlength="100">
      <button class="btn">Save name</button>
    </form>
    <span class="muted">Saving an empty name removes it. Names are stored in this app's own database.</span>
  </div>

  <div class="card tblcard">
  <div class="tablewrap">
  <table>
    <thead><tr><th>Name</th><th class="n">User ID</th><th class="n">Latest</th><th class="n">Avg in month</th>{{if .MarkupPct}}<th class="n">B2 cost</th>{{end}}<th class="n">Month (USD)</th><th class="n">Month (INR)</th><th class="n">Now /mo (USD)</th><th class="n">Now /mo (INR)</th><th class="billedcell">Billed</th><th></th></tr></thead>
    <tbody>
    {{range .Rows}}
      <tr{{if not .Billed}} class="unpaid"{{end}}>
        <td class="namecell">{{if .Name}}<span class="pill">{{.Name}}</span>{{else}}<span class="muted">—</span>{{end}}</td>
        <td class="n"><a class="uidlink" href="{{$.Base}}/?id={{.UserID}}"
             target="_blank" rel="noopener"
             title="Open the public page for user {{.UserID}}">{{.UserID}}</a></td>
        <td class="n">{{.LatestHuman}}</td>
        <td class="n">{{.AvgHuman}}</td>
        {{if $.MarkupPct}}<td class="n">${{printf "%.4f" .Cost}}</td>{{end}}
        <td class="n">${{printf "%.4f" .Charge}}</td>
        <td class="n">&#8377;{{printf "%.2f" .ChargeINR}}</td>
        <td class="n">${{printf "%.4f" .NowUSD}}</td>
        <td class="n">&#8377;{{printf "%.2f" .NowINR}}</td>
        <td class="billedcell">
          <form method="post" action="/admin/bill" class="billform">
            <input type="hidden" name="month" value="{{$.Month}}">
            <input type="hidden" name="user_id" value="{{.UserID}}">
            {{/* unchecked boxes send nothing, so "billed" is present only when ticked */}}
            <input type="checkbox" class="billchk" name="billed" value="true"
                   {{if .Billed}}checked{{end}}
                   onchange="this.form.submit()"
                   title="{{$.Month}}: click to toggle paid/unpaid">
          </form>
        </td>
        <td>
          <form method="post" action="/admin/name" class="nameform inline">
            <input type="hidden" name="month" value="{{$.Month}}">
            <input type="hidden" name="user_id" value="{{.UserID}}">
            <input type="text" name="name" value="{{.Name}}" placeholder="set name" maxlength="100">
            <button class="btn">Save</button>
          </form>
        </td>
      </tr>
    {{end}}
    </tbody>
    <tfoot><tr><td>Total</td><td class="n"></td><td class="n">{{.TotalLatest}}</td><td class="n">{{.TotalAvg}}</td>{{if .MarkupPct}}<td class="n">${{printf "%.4f" .TotalCost}}</td>{{end}}<td class="n">${{printf "%.4f" .TotalCharge}}</td><td class="n">&#8377;{{printf "%.2f" .TotalChargeINR}}</td><td class="n">${{printf "%.4f" .TotalNowUSD}}</td><td class="n">&#8377;{{printf "%.2f" .TotalNowINR}}</td><td class="billedcell billcount">{{.BilledCount}}/{{.BilledTotal}}</td><td></td></tr></tfoot>
  </table>
  </div>
  </div>
  <p class="meta">Billed column: tick a user to mark {{.Month}} as paid (amber rows are unpaid). The count in the footer is paid/total for this month.</p>
{{else}}
  <div class="card">
    <h2 class="cardtitle">Check your usage</h2>
    <p class="muted">Enter your Ente user ID to see your storage usage and what it costs this month.</p>
    <form method="get" class="lookup">
      <input name="id" value="{{.Query}}" placeholder="Your user ID" inputmode="numeric" autofocus>
      <button class="btn primary">Look up</button>
    </form>
    {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  </div>
  {{with .Single}}
    <div class="card">
      <div class="cardhead">
        <div>
          <div class="muted">User {{.UserID}}</div>
          <div class="bignum">{{.LatestHuman}}</div>
          <div class="muted">current usage</div>
        </div>
        <div class="qrbox" id="qrbox" role="button" tabindex="0"
             title="Tap to copy UPI ID {{$.UpiID}}">
          <img src="/qr.png" alt="UPI QR code — tap to copy UPI ID" draggable="false">
          <span class="qrcap" id="qrcap">Tap to copy UPI ID</span>
        </div>
      </div>
      <div class="stats">
        <div class="stat">
          <div class="lab">Cost at current usage</div>
          <div class="usd">${{printf "%.4f" .NowUSD}}</div>
          <div class="inr">&#8377;{{printf "%.2f" .NowINR}} per month</div>
        </div>
        <div class="stat">
          <div class="lab">This month so far &middot; avg {{.AvgHuman}}</div>
          <div class="usd">${{printf "%.4f" .Charge}}</div>
          <div class="inr">&#8377;{{printf "%.2f" .ChargeINR}}</div>
        </div>
      </div>
    </div>
  {{end}}
  {{if .History}}
    <div class="card">
      <h3 class="cardtitle">Monthly history</h3>
      <table>
        <thead><tr><th>Month</th><th class="n">Avg stored</th><th class="n">Cost (USD)</th><th class="n">Cost (INR)</th><th class="n">Billed</th></tr></thead>
        <tbody>{{range .History}}<tr><td>{{.Month}}</td><td class="n">{{.AvgHuman}}</td><td class="n">${{printf "%.4f" .Charge}}</td><td class="n">&#8377;{{printf "%.2f" .ChargeINR}}</td><td class="n">{{if .Billed}}<span class="paidchk" title="Payment received">&#10003;</span>{{else}}<span class="pending" title="Payment pending">pending</span>{{end}}</td></tr>{{end}}</tbody>
      </table>
    </div>
  {{end}}
{{end}}

<p class="meta">Rate: ${{printf "%.2f" .PricePerTB}} per TB per month{{if .MarkupPct}} (+{{printf "%.0f" .MarkupPct}}%){{end}}. Sizes are decimal (1 GB = 10<sup>9</sup> bytes). Costs use the average of the periodic usage samples in the month. Exchange rate: 1 USD = &#8377;{{printf "%.2f" .Rate}} ({{.RateNote}}{{if .RateFetchedTS}}, fetched {{ts .RateFetchedTS}}{{end}}). Last sample: {{if .LastSampleTS}}{{ts .LastSampleTS}}{{else}}never{{end}}.</p>

</div>

{{if .Single}}
<script>
(function () {
  var box = document.getElementById('qrbox');
  if (!box) return;
  var UPI = {{.UpiID}};
  var cap = document.getElementById('qrcap');
  var idle = cap.textContent, timer = null;

  function feedback(ok, msg) {
    cap.textContent = msg;
    cap.classList.toggle('done', ok);
    clearTimeout(timer);
    timer = setTimeout(function () {
      cap.textContent = idle;
      cap.classList.remove('done');
    }, 2500);
  }

  function copy() {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(UPI).then(
        function () { feedback(true, 'Copied: ' + UPI); },
        function () { legacy(); }
      );
    } else {
      legacy();
    }
  }

  function legacy() {
    var ta = document.createElement('textarea');
    ta.value = UPI;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    document.body.appendChild(ta);
    ta.select();
    ta.setSelectionRange(0, UPI.length);
    var ok = false;
    try { ok = document.execCommand('copy'); } catch (e) {}
    document.body.removeChild(ta);
    feedback(ok, ok ? 'Copied: ' + UPI : 'Copy failed — UPI ID: ' + UPI);
  }

  box.addEventListener('click', copy);
  box.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); copy(); }
  });
  box.addEventListener('dragstart', function (e) { e.preventDefault(); });
})();
</script>
{{end}}

<script>
(function () {
  function two(n) { return n < 10 ? '0' + n : '' + n; }

  function format(ts) {
    var d = new Date(ts * 1000);
    if (isNaN(d.getTime())) return null;
    try {
      var txt = d.toLocaleString(undefined, {
        year: 'numeric', month: '2-digit', day: '2-digit',
        hour: '2-digit', minute: '2-digit', hour12: false
      });
      txt = txt.replace(/(?:^|[,\s])24:00/, function (m0) {
        return m0.replace('24:00', '00:00');
      });
      var tz = '';
      if (typeof Intl !== 'undefined' && Intl.DateTimeFormat) {
        var withTZ = d.toLocaleString(undefined, { timeZoneName: 'short' });
        var m = withTZ.match(/([A-Z]{2,5}|GMT[+-]\d{1,2}(?::\d{2})?)\s*$/);
        if (m) tz = ' ' + m[1];
      }
      return txt + tz;
    } catch (e) {
      return d.getFullYear() + '-' + two(d.getMonth() + 1) + '-' + two(d.getDate()) +
        ' ' + two(d.getHours()) + ':' + two(d.getMinutes());
    }
  }

  var els = document.querySelectorAll('time[data-ts]');
  for (var i = 0; i < els.length; i++) {
    var ts = parseInt(els[i].getAttribute('data-ts'), 10);
    if (!ts) continue;
    var out = format(ts);
    if (out) {
      els[i].textContent = out;
      els[i].setAttribute('datetime', new Date(ts * 1000).toISOString());
      els[i].title = 'Shown in your local timezone (' +
        (Intl.DateTimeFormat().resolvedOptions().timeZone || 'local') + ')';
    }
  }
})();
</script>
</body></html>`