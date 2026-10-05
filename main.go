package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

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
}

type Row struct {
	UserID      int64   `json:"user_id"`
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
}

type HistRow struct {
	Month    string
	AvgHuman  string
	Charge    float64
	ChargeINR float64
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
	Rate        float64
	RateNote    string
	PricePerTB  float64
	MarkupPct   float64
	LastSample  string
}

var (
	cfg  Config
	pg   *sql.DB
	lite *sql.DB
	tmpl = template.Must(template.New("p").Parse(pageHTML))
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
	rate, _ := currentRate()
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

func lastSample() string {
	var ts sql.NullInt64
	lite.QueryRow(`SELECT MAX(ts) FROM samples`).Scan(&ts)
	if !ts.Valid {
		return "never"
	}
	return time.Unix(ts.Int64, 0).UTC().Format("2006-01-02 15:04 UTC")
}

// ---------- USD -> INR rate ----------

var (
	rateMu   sync.RWMutex
	usdInr   float64
	rateNote string
)

func currentRate() (float64, string) {
	rateMu.RLock()
	defer rateMu.RUnlock()
	return usdInr, rateNote
}

func setRate(r float64, note string) {
	rateMu.Lock()
	usdInr, rateNote = r, note
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
	setRate(r, "live rate, fetched "+time.Now().UTC().Format("2006-01-02 15:04 UTC"))
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
		CREATE INDEX IF NOT EXISTS idx_samples_user_ts ON samples(user_id, ts);`)
	return err
}

// ---------- Postgres -> SQLite sampler ----------

func sampleOnce() error {
	rows, err := pg.Query(`
		SELECT u.user_id, us.storage_consumed
		FROM usage us
		JOIN users u ON u.user_id = us.user_id`)
	if err != nil {
		return err
	}
	type pair struct{ id, bytes int64 }
	var got []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.bytes); err != nil {
			rows.Close()
			return err
		}
		got = append(got, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := lite.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO samples(ts, user_id, bytes) VALUES (?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	ts := time.Now().Unix()
	for _, p := range got {
		if _, err := stmt.Exec(ts, p.id, p.bytes); err != nil {
			tx.Rollback()
			return err
		}
	}
	log.Printf("sampled %d users", len(got))
	return tx.Commit()
}

func samplerLoop() {
	t := time.NewTicker(cfg.Interval)
	defer t.Stop()
	for range t.C {
		if err := sampleOnce(); err != nil {
			log.Println("sample failed:", err)
		}
	}
}

// ---------- handlers ----------

// Public: a user enters their own ID and sees only their own usage.
func handleIndex(w http.ResponseWriter, r *http.Request) {
	p := Page{PricePerTB: cfg.PricePerTB, MarkupPct: cfg.MarkupPct, LastSample: lastSample()}
	p.Rate, p.RateNote = currentRate()
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
				p.History = append(p.History, HistRow{Month: m, AvgHuman: human(int64(a)), Charge: charge, ChargeINR: charge * p.Rate})
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
	p := Page{Admin: true, Month: label, PricePerTB: cfg.PricePerTB, MarkupPct: cfg.MarkupPct, LastSample: lastSample()}
	p.Rate, p.RateNote = currentRate()
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
		p.Rows = append(p.Rows, row)
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
	}
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	setRate(cfg.FallbackINR, "fixed rate (USD_TO_INR)")
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

	if err := sampleOnce(); err != nil {
		log.Println("initial sample failed:", err)
	}
	go samplerLoop()

	http.HandleFunc("/", handleIndex)
	if cfg.AdminPass != "" {
		http.HandleFunc("/admin", basicAuth(handleAdmin))
	} else {
		log.Println("ADMIN_PASS not set: /admin is disabled")
	}

	log.Println("listening on", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, nil))
}

const pageHTML = `<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ente storage usage</title>
<style>
  body{font-family:system-ui,sans-serif;max-width:760px;margin:2rem auto;padding:0 1rem;line-height:1.5}
  table{border-collapse:collapse;width:100%}
  th,td{padding:.5rem .75rem;border-bottom:1px solid #ddd;text-align:left}
  td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
  input,select{padding:.5rem;font-size:1rem}
  input{width:16rem}
  button{padding:.5rem 1rem;font-size:1rem}
  .err{color:#b00020}.muted{color:#666;font-size:.9rem}
  .card{border:1px solid #ddd;border-radius:8px;padding:1rem;margin-top:1rem}
  tfoot td{font-weight:600}
</style></head><body>
<h1>Ente storage usage</h1>

{{if .Admin}}
  <form method="get">
    <select name="month" onchange="this.form.submit()">
      {{$m := .Month}}
      {{range .Months}}<option value="{{.}}" {{if eq . $m}}selected{{end}}>{{.}}</option>{{end}}
      {{if not .Months}}<option>{{.Month}}</option>{{end}}
    </select>
    <noscript><button>Go</button></noscript>
  </form>
  <table style="margin-top:1rem">
    <thead><tr><th>User ID</th><th class="n">Latest</th><th class="n">Avg in month</th>{{if .MarkupPct}}<th class="n">B2 cost</th>{{end}}<th class="n">Month (USD)</th><th class="n">Month (INR)</th><th class="n">Now /mo (USD)</th><th class="n">Now /mo (INR)</th></tr></thead>
    <tbody>
    {{range .Rows}}
      <tr><td>{{.UserID}}</td><td class="n">{{.LatestHuman}}</td><td class="n">{{.AvgHuman}}</td>{{if $.MarkupPct}}<td class="n">${{printf "%.4f" .Cost}}</td>{{end}}<td class="n">${{printf "%.4f" .Charge}}</td><td class="n">&#8377;{{printf "%.2f" .ChargeINR}}</td><td class="n">${{printf "%.4f" .NowUSD}}</td><td class="n">&#8377;{{printf "%.2f" .NowINR}}</td></tr>
    {{end}}
    </tbody>
    <tfoot><tr><td>Total</td><td class="n">{{.TotalLatest}}</td><td class="n">{{.TotalAvg}}</td>{{if .MarkupPct}}<td class="n">${{printf "%.4f" .TotalCost}}</td>{{end}}<td class="n">${{printf "%.4f" .TotalCharge}}</td><td class="n">&#8377;{{printf "%.2f" .TotalChargeINR}}</td><td class="n">${{printf "%.4f" .TotalNowUSD}}</td><td class="n">&#8377;{{printf "%.2f" .TotalNowINR}}</td></tr></tfoot>
  </table>
  <p class="muted"><a href="/admin?month={{.Month}}&format=json">JSON</a></p>
{{else}}
  <form method="get">
    <input name="id" value="{{.Query}}" placeholder="Your user ID" inputmode="numeric" autofocus>
    <button>Look up</button>
  </form>
  {{if .Error}}<p class="err">{{.Error}}</p>{{end}}
  {{with .Single}}
    <div class="card">
      <div class="muted">User {{.UserID}}</div>
      <h2 style="margin:.25rem 0">{{.LatestHuman}}</h2>
      <div class="muted">current usage</div>
      <table style="margin-top:.75rem">
        <thead><tr><th></th><th class="n">USD</th><th class="n">INR</th></tr></thead>
        <tbody>
          <tr><td>Cost at current usage (per month)</td><td class="n">${{printf "%.4f" .NowUSD}}</td><td class="n">&#8377;{{printf "%.2f" .NowINR}}</td></tr>
          <tr><td>This month so far (avg {{.AvgHuman}})</td><td class="n">${{printf "%.4f" .Charge}}</td><td class="n">&#8377;{{printf "%.2f" .ChargeINR}}</td></tr>
        </tbody>
      </table>
    </div>
  {{end}}
  {{if .History}}
    <h3>Monthly history</h3>
    <table>
      <thead><tr><th>Month</th><th class="n">Avg stored</th><th class="n">Cost (USD)</th><th class="n">Cost (INR)</th></tr></thead>
      <tbody>{{range .History}}<tr><td>{{.Month}}</td><td class="n">{{.AvgHuman}}</td><td class="n">${{printf "%.4f" .Charge}}</td><td class="n">&#8377;{{printf "%.2f" .ChargeINR}}</td></tr>{{end}}</tbody>
    </table>
  {{end}}
{{end}}

<p class="muted">Rate: ${{printf "%.2f" .PricePerTB}} per TB per month{{if .MarkupPct}} (+{{printf "%.0f" .MarkupPct}}%){{end}}. Sizes are decimal (1 GB = 10<sup>9</sup> bytes). Costs use the average of the periodic usage samples in the month. Exchange rate: 1 USD = &#8377;{{printf "%.2f" .Rate}} ({{.RateNote}}). Last sample: {{.LastSample}}.</p>
</body></html>`