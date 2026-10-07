# ente-usage

A small, self-contained web service that tracks per-user storage usage on an
[Ente](https://ente.io) deployment, estimates what each user costs per month,
and lets an admin keep track of who has paid.

It periodically samples your Ente **Postgres** database (read-only), stores the
history in its own **SQLite** file, and serves a two-view web UI:

- **Public** — any user looks up their own user ID and sees their current
  usage, this month's estimated cost (USD & INR), and a monthly history with a
  paid/unpaid indicator.
- **Admin** (HTTP basic auth) — every user for a selected month with sizes,
  costs, totals, assignable display names, on-demand re-sampling, and a
  billed/unpaid checkbox per user.

Everything lives in a single Go file plus one image — no framework, no build
system, no external services beyond the Postgres it reads and an optional
live FX rate lookup.

## How it works

```
Ente Postgres ──(sample every SAMPLE_INTERVAL)──▶ SQLite ──▶ web UI
  usage/users                                   samples
                                                user_names   ◀─ admin edits
                                                bills        ◀─ admin edits
```

- **Sampling** reads `usage.storage_consumed` joined against `users` and writes
  one row per user per sample tick (`INSERT OR REPLACE`, so a tick is
  idempotent). An on-demand "poll" button triggers the same code path from the
  admin UI; a mutex serializes the timer and manual triggers.
- **Costs** are computed from the *average* of the samples within the month, at
  a configurable USD/TB/month price (B2-style decimal sizing: 1 GB = 10⁹
  bytes, 1 TB = 10¹² bytes) with an optional markup.
- **Currency** is USD with a live USD→INR rate fetched every 12 hours
  (disabled with `FX_LIVE=false`, falling back to a fixed rate).

## Features

### Public view (`/`)

- Lookup form: enter a user ID, see only that user's data.
- Current usage, plus two cost lines — *cost at current usage* and *this month
  so far* (sample average) — each in USD and INR.
- **Monthly history** table: month, average stored, cost in USD and INR, and a
  **Billed** column showing a green ✓ for months the admin has marked as paid
  (unsettled months show an empty cell).
- **UPI payment tile**: a QR image rendered beside the usage summary.
  Tapping/clicking it copies the payee UPI ID to the device clipboard (with a
  legacy `execCommand` fallback and a visible "Copied" confirmation), so the
  user can pay right after reading the estimate. The image is embedded in the
  binary and served from `/qr.png`.

### Admin view (`/admin`, HTTP basic auth)

- **Month selector** — every number on the page is scoped to the selected
  `YYYY-MM`.
- **Roomy overview table** — one row per user: display name, user ID, latest
  usage, month average, B2 cost (when a markup is set), month charge and
  now-per-month charge in USD/INR, with totals in the footer and striped,
  non-wrapping columns that scroll horizontally on narrow screens. User IDs
  are links that open that user's public lookup page (`/?id=<user id>`) in a
  new tab for quick reference.
- **Display names** — assign a label to any user ID (inline per-row form, plus
  a standalone form for IDs with no samples yet). Names are stored in the
  app's own database and appear as pills in the table; saving an empty name
  clears it.
- **Billed tracking** — a checkbox column marks the selected month as paid per
  user. Unpaid rows are amber-tinted, the footer shows `paid/total`, and a
  *Mark all paid* button (with confirmation) settles the whole month at once.
  Billing state is stored per `(user, month)`, so switching months never
  shifts it.
- **Poll latest stats** — pulls fresh usage from Postgres immediately instead
  of waiting for the next interval, then returns to the same month with a
  confirmation notice (sample count + timestamp).
- **JSON export** — `?format=json` returns the current month's rows,
  including names and billed flags.

### Notes

- The admin UI, name assignment, polling, and billing toggles are all
  POST-only endpoints behind basic auth; the public page exposes no admin
  markup or endpoints.
- The QR code is compiled into the binary (`go:embed`), so the container image
  needs only the executable.

## Configuration

All configuration is via environment variables:

| Variable           | Default           | Description                                                        |
| ------------------ | ----------------- | ------------------------------------------------------------------ |
| `DATABASE_URL`     | *(required)*      | Postgres connection string for the Ente database (read-only use).  |
| `SQLITE_PATH`      | `/data/usage.db`  | Path to the app's own SQLite data file (samples, names, billing).  |
| `SAMPLE_INTERVAL`  | `1h`              | How often to sample Postgres. Any Go duration, minimum `1m`.       |
| `B2_PRICE_PER_TB`  | `6.95`            | USD per TB per month used for cost estimates.                      |
| `MARKUP_PERCENT`   | `0`               | Optional margin added on top of the raw cost.                      |
| `USD_TO_INR`       | `88`              | Fixed USD→INR rate / fallback when the live lookup fails.          |
| `FX_LIVE`          | `true`            | Set to `false` to disable the live FX lookup and use `USD_TO_INR`. |
| `ADMIN_USER`       | `admin`           | Username for HTTP basic auth on the admin routes.                  |
| `ADMIN_PASS`       | *(unset)*         | Password for admin auth. **If unset, `/admin` is disabled.**      |
| `LISTEN_ADDR`      | `:8080`           | Listen address for the HTTP server.                                |
| `PUBLIC_BASE_URL`  | *(auto)*          | External origin (e.g. `https://usage.example.com`) used to build admin → public user links. When unset it is derived from the request, honouring `X-Forwarded-Proto`. |

The USD→INR rate and the last sample time are shown in the page footer, so you
can always tell how fresh the numbers are. Timestamps (last sample, FX fetch
time, and the on-demand poll confirmation) are rendered as `<time>` elements
from raw unix values and formatted **in each viewer's own browser timezone and
locale** — an IST browser sees IST, with the UTC string kept as a fallback for
clients without JavaScript.

## Data stored in SQLite

The app owns three tables in its own database file:

| Table        | Key                  | Purpose                                            |
| ------------ | -------------------- | -------------------------------------------------- |
| `samples`    | `(ts, user_id)`      | Usage readings pulled from Postgres over time.     |
| `user_names` | `user_id`            | Admin-assigned display names for user IDs.         |
| `bills`      | `(user_id, month)`   | Paid/unpaid flag per user per `YYYY-MM` month.     |

Schema is created idempotently at startup (`CREATE TABLE IF NOT EXISTS`), so
upgrades against an existing file are safe. The Postgres side is only ever
read with a single `SELECT`; nothing is written back to Ente's database.

## Running

### Docker

The provided `Dockerfile` builds a static binary and ships it on a minimal
distroless image:

```sh
docker build -t ente-usage .
docker run -d --name ente-usage \
  -p 8080:8080 \
  -v ente-data:/data \
  -e DATABASE_URL="postgres://user:pass@host:5432/ente?sslmode=disable" \
  -e ADMIN_PASS="choose-a-password" \
  ente-usage
```

Mount a volume at `/data` so `usage.db` (samples, names, billing) survives
recreates.

### Locally

Requires Go. There is no checked-in `go.mod` — initialize it the same way the
Dockerfile does:

```sh
go mod init ente-usage
go mod tidy
go build -o ente-usage .

DATABASE_URL="postgres://..." SQLITE_PATH=./usage.db ./ente-usage
```

Then open `http://localhost:8080/` (public) and
`http://localhost:8080/admin` (basic auth).

## HTTP routes

| Route              | Method        | Auth  | Description                                  |
| ------------------ | ------------- | ----- | -------------------------------------------- |
| `/`                | `GET`         | none  | Public usage lookup (`?id=...`).             |
| `/qr.png`          | `GET`/`HEAD`  | none  | Embedded UPI QR image, cached for 24 h.      |
| `/admin`           | `GET`         | basic | Monthly overview (`?month=YYYY-MM`, `?format=json`). |
| `/admin/name`      | `POST`        | basic | Assign or clear a display name for a user ID. |
| `/admin/sample`    | `POST`        | basic | Sample Postgres on demand, then redirect back. |
| `/admin/bill`      | `POST`        | basic | Toggle the paid flag for one user + month.   |
| `/admin/bill/all`  | `POST`        | basic | Mark every user in a month as paid.          |

Admin routes return `405` for the wrong method and `401` for bad credentials.
Admin actions redirect back to `/admin` with a `?notice=...` message.

## Customizing the payment QR

- Replace `qr.png` in the repository root with your own QR image (any square
  PNG; it is rendered on a white tile, so a transparent background works).
- The payee UPI ID that gets copied on tap is a constant near the top of
  `main.go` — change it there and rebuild. It is used for both the copy
  action and the tooltip.

## Development

The project is a single `main.go` (~1000 lines) containing config, the
sampler, SQLite helpers, handlers, and the inline HTML/CSS/JS template, plus
`*_test.go` files. Since `go.mod` is generated (see *Running → Locally*),
initialize the module first:

```sh
go mod init ente-usage && go mod tidy

go vet ./...    # static checks
go test ./...   # template rendering, SQLite round-trips, handler guards
```

Tests cover template rendering for both views, the name/billing SQLite
round-trips, month isolation, HTTP method guards, and the QR endpoint.

## License

[AGPL-3.0](LICENSE).
