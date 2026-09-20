# Deploying the Used-Car Deal Finder (carbuyer) to Cloudflare

The exact steps to run it **privately on real listings**: a Worker + D1 database +
daily cron, a Pages dashboard with snipe alerts, Cloudflare Access so only your email
can open it, and a crawl every 30 minutes (GitHub Actions or your Windows PC).

Already deployed and only updating? Go to
[Upgrade: snipe alerts (migration 0003)](#upgrade-snipe-alerts-migration-0003).

```
 GitHub Actions / your PC ──POST /api/listings, Bearer token──► Worker carbuyer-api ──► D1 "carbuyer"
 (crawl every 30 min)                                              │  new/cheaper + strong deal
                                                                   │  ──► new_deal_alerts
                                                             ▲      ▲ cron 11:00 UTC: snapshot
 you (browser) ──Cloudflare Access──► Pages carbuyer-dashboard│
                     (email login)        /api/* ──service binding┘   ("New deals" + all deals)
```

Everything runs on the Workers Free plan at this size. See "Limits" at the end.

All commands use `npx wrangler` (v4). Run them from the folder shown.

## 0. Before you start

- A Cloudflare account.
- Build tools already used locally: Rust with the `wasm32-unknown-unknown`
  target (`rustup target add wasm32-unknown-unknown`), Node 20+, Go.
- A long random token for the crawler. Make one and keep it somewhere safe
  (a password manager):

  ```sh
  node -e "console.log(require('crypto').randomBytes(32).toString('base64url'))"
  ```

## 1. Log in

```sh
npx wrangler login          # opens the browser; allow access
npx wrangler whoami         # shows your account name and account id
```

If you have more than one account, uncomment `account_id` in
`cloudflare/worker/wrangler.toml` and paste the id from `whoami`.

## 2. Create the D1 database

```sh
cd cloudflare/worker
npx wrangler d1 create carbuyer
```

It prints a block with `database_id = "..."`. Open `cloudflare/worker/wrangler.toml`
and replace the placeholder `00000000-0000-0000-0000-000000000000` with that id.
(If wrangler offers to add the binding to the config for you, say no: it is already there.)

## 3. Apply the migrations to the real database

```sh
cd cloudflare/worker
npx wrangler d1 migrations apply carbuyer --remote
```

It lists `0001_listings.sql`, `0002_deal_snapshots.sql` and `0003_new_deal_alerts.sql`; confirm with `y`.
Check:

```sh
npx wrangler d1 execute carbuyer --remote --command "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
```

You should see `crawls, deal_snapshot_items, deal_snapshots, listings, meta, new_deal_alerts, price_history`
(plus D1's own `_cf_KV` / `d1_migrations`).

## 4. Set the ingest token secret

```sh
cd cloudflare/worker
npx wrangler secret put INGEST_TOKEN
```

Paste the token from step 0 when asked. It is stored encrypted by Cloudflare,
never in the repo. (If the Worker does not exist yet, wrangler offers to create
it; say yes, or run step 5 first and then this step.)

## 5. Deploy the Worker

```sh
cd cloudflare/worker
npx wrangler deploy
```

This runs `node build.mjs` (worker-build compiles the Rust to wasm), uploads
it, and registers the daily cron `0 11 * * *`. It prints the URL, for example
`https://carbuyer-api.<your-subdomain>.workers.dev`. Note it down.

Check it (health is public and harmless):

```sh
curl https://carbuyer-api.<your-subdomain>.workers.dev/api/health
# {"ok":true,"time":"..."}
curl -i https://carbuyer-api.<your-subdomain>.workers.dev/api/deals
# 404: reads are closed on the public hostname by design (see step 8)
```

## 6. Push listings from the crawler

From your machine (bash; in PowerShell use `$env:CARBUYER_INGEST_TOKEN = "..."`):

```sh
cd crawler
export CARBUYER_INGEST_TOKEN='<the token from step 0>'
go run ./cmd/carbuyer --make toyota --model rav4 --pages 3 \
  --push https://carbuyer-api.<your-subdomain>.workers.dev
```

The CLI still saves to local SQLite and prints deals as before, then prints
`pushed to .../api/listings: N seen, N added, ...`. The token is only read
from the environment, and plain `http://` is refused except for localhost.

## 7. Deploy the dashboard (Pages)

```sh
cd cloudflare/pages
npx wrangler pages project create carbuyer-dashboard --production-branch main
npx wrangler pages deploy --branch main
```

`wrangler.toml` there sets the output folder (`public/`) and the service
binding `API -> carbuyer-api`, so `/api/*` on the dashboard reaches the Worker
internally. It prints `https://carbuyer-dashboard.pages.dev` (plus a
per-deployment URL like `https://<hash>.carbuyer-dashboard.pages.dev`).

Check the binding: Cloudflare dashboard > Workers & Pages > carbuyer-dashboard >
Settings > Bindings should list `API` (Service binding, carbuyer-api).

Until step 8 is done, the dashboard is public. Do step 8 right away, or deploy
Pages only after setting up Access.

## 8. Cloudflare Access (only your email can open it)

### What gets protected, and why

| Hostname / path | Who can reach it | How |
|---|---|---|
| `carbuyer-dashboard.pages.dev` (all paths: `/`, `/api/deals`, `/api/snapshots/latest`, `/api/alerts`, `/api/health`) | only you | **Access application** below |
| `*.carbuyer-dashboard.pages.dev` (preview / per-deployment URLs) | only you | same Access application |
| `carbuyer-api.<sub>.workers.dev/api/listings` (POST) | the crawler | **bearer token** (INGEST_TOKEN). Keep it out of interactive Access, or the crawler gets a login page |
| `carbuyer-api.<sub>.workers.dev/api/deals`, `/api/snapshots/latest`, `/api/alerts` | nobody (404) | closed in code: they answer only on the internal service-binding host, because `PUBLIC_READ_API = "false"` in `wrangler.toml` |
| `carbuyer-api.<sub>.workers.dev/api/health` | anyone | returns `{"ok":true}` only |

So one Access application (on Pages) covers the dashboard and every read. The
Worker's public hostname only exposes the token-protected ingest.

### Steps

1. Open **Zero Trust** in the Cloudflare dashboard (left sidebar). The first
   time, pick a team name (e.g. `yourname`, giving `yourname.cloudflareaccess.com`)
   and the **Free** plan (up to 50 users; asks for a payment method on file but costs nothing).
2. **Settings > Authentication > Login methods**: make sure **One-time PIN** is
   there (it is by default). You log in with a code sent to your email.
3. **Access > Applications > Add an application > Self-hosted**:
   - Application name: `carbuyer dashboard`
   - Session duration: e.g. `24 hours`
   - Public hostname #1: domain `carbuyer-dashboard.pages.dev`, path empty
   - Public hostname #2: domain `*.carbuyer-dashboard.pages.dev`, path empty
     (covers preview deployments)
   - Policy: name `only me`, action **Allow**, Include > **Emails** > your email address
   - Save.
4. Test in a private browser window: `https://carbuyer-dashboard.pages.dev`
   should show the Cloudflare Access login, send a code to your email, then
   show the deals. `curl -i https://carbuyer-dashboard.pages.dev/api/deals`
   (no cookie) must return a redirect to `...cloudflareaccess.com`, not JSON.

Shortcut for previews only: Pages project > Settings > General > "Enable access
policy" protects `*.carbuyer-dashboard.pages.dev` but **not** the production
`carbuyer-dashboard.pages.dev`; that is why step 3 adds both hostnames by hand.

### Optional: reading the API directly (without the dashboard)

Only if you want `curl` access to `/api/deals` on workers.dev:

1. Worker > Settings > Domains & Routes > workers.dev > **Enable Cloudflare Access**
   (restrict it to your email, as above).
2. Add a second self-hosted application for
   `carbuyer-api.<sub>.workers.dev` with path `api/listings` and a **Bypass**
   policy (Include: Everyone), so the crawler's bearer-token requests still get
   through. The more specific path wins.
3. Set `PUBLIC_READ_API = "true"` in `cloudflare/worker/wrangler.toml` and
   `npx wrangler deploy` again.

If path-level bypass is not available for your workers.dev hostname, give the
Worker a custom domain in a zone you own (Worker > Settings > Domains & Routes >
Add > Custom domain, e.g. `carbuyer-api.example.com`), put the Access apps on that
hostname instead, and push to it with `--push https://carbuyer-api.example.com`.

## 9. Cron and snapshots

The cron runs daily at 11:00 UTC. It re-scores every active listing and stores
the top 100 deals in `deal_snapshots` / `deal_snapshot_items` (the last 30 runs
are kept). The dashboard's "Data: last daily snapshot" reads it. To see it ran:

```sh
cd cloudflare/worker
npx wrangler d1 execute carbuyer --remote --command "SELECT id, created_at, comps, scored FROM deal_snapshots ORDER BY id DESC LIMIT 5"
npx wrangler tail            # live logs; shows "snapshot stored: ..." when the cron fires
```

Change the time in `[triggers] crons` (always UTC) and redeploy. To use a lower
comps threshold by default (like the CLI's `--min-comps 3`), set
`DEFAULT_MIN_COMPS = "3"` under `[vars]` and redeploy.

## 10. Crawl real listings every 30 minutes

The searches live in `crawler/searches.yml` (make/model slugs, price cap, pages,
newest first). Edit it, commit, and the next run uses it. Pick **one** runner.

### Option A: GitHub Actions (`.github/workflows/crawl.yml`)

After the repo is on GitHub, add one secret and one variable. In the browser:
repo > **Settings > Secrets and variables > Actions**

- **Secrets** tab > **New repository secret**: name `CARBUYER_INGEST_TOKEN`,
  value = the same token you gave `wrangler secret put INGEST_TOKEN` (step 4).
- **Variables** tab > **New repository variable**: name `CARBUYER_API_URL`,
  value = `https://carbuyer-api.<your-subdomain>.workers.dev` (no trailing slash).

Or with the GitHub CLI, from the repo folder (the secret is read from your
environment, so it is never typed on the command line or saved in history):

```sh
# bash
printf '%s' "$CARBUYER_INGEST_TOKEN" | gh secret set CARBUYER_INGEST_TOKEN
gh variable set CARBUYER_API_URL --body "https://carbuyer-api.<your-subdomain>.workers.dev"
```

```powershell
# PowerShell
$env:CARBUYER_INGEST_TOKEN | gh secret set CARBUYER_INGEST_TOKEN
gh variable set CARBUYER_API_URL --body "https://carbuyer-api.<your-subdomain>.workers.dev"
```

Then **Actions > crawl > Run workflow** once and read the log: one line per
search (`toyota rav4: 40 listings ...`) and `pushed to ...: N seen, N added, ...,
N new deal alerts`. After that it runs every 30 minutes by itself.

Good to know:
- Scheduled runs only run from the default branch, may start a few minutes late,
  and GitHub pauses schedules after 60 days without repo activity (re-enable
  in the Actions tab).
- Until `CARBUYER_API_URL` is set, scheduled runs are skipped, so forks stay quiet.
- If the site starts refusing the GitHub runners (the run stops at the first
  error, so you would see `stopping: 0 of N searches done`), use option B.

### Option B: your Windows PC (`scripts/crawl-and-push.ps1`)

```powershell
# once: the URL (the token is already in CARBUYER_INGEST_TOKEN)
[Environment]::SetEnvironmentVariable('CARBUYER_API_URL', 'https://carbuyer-api.<your-subdomain>.workers.dev', 'User')
# try it (open a new PowerShell window first so it sees the variable)
powershell -NoProfile -ExecutionPolicy Bypass -File scripts\crawl-and-push.ps1
```

The script header has the `Register-ScheduledTask` lines for every 30 minutes.
Logs: `%LOCALAPPDATA%\carbuyer\logs\crawl-YYYYMMDD.log`.

## 11. Snipe alerts

Every push checks the listings that are new, relisted or cheaper. A car at least
`ALERT_MIN_DISCOUNT_PCT` % under a baseline of at least `ALERT_MIN_COMPS` comparable
cars (and not damaged, thin, a parts car or a conditional price) is stored once in
`new_deal_alerts` and shown under **New deals** at the top of the dashboard.

Tune it in `cloudflare/worker/wrangler.toml` under `[vars]`, then `npx wrangler deploy`:

| Var | Default | Meaning |
|---|---|---|
| `ALERTS_ENABLED` | `"true"` | `"false"` turns alerts off (ingest is unchanged) |
| `ALERT_MIN_DISCOUNT_PCT` | `"15"` | % under the local baseline |
| `ALERT_MIN_COMPS` | `"6"` | comparable cars needed (also the scorer's minComps for this check) |
| `ALERT_INCLUDE_DAMAGED` | `"false"` | damaged cars look cheap because of the damage |

A bad value is logged (`npx wrangler tail`) and the defaults are used. To see alerts
from the command line:

```sh
cd cloudflare/worker
npx wrangler d1 execute carbuyer --remote --command "SELECT created_at, discount_pct, year, make, model, price, url FROM new_deal_alerts ORDER BY created_at DESC LIMIT 10"
```

### Email notifications later

Alerts go through the `Notifier` trait in `cloudflare/worker/src/alerts.rs`; today
`entry.rs` passes `NoNotifier` (dashboard only). Alerts a notifier reports as delivered
get `notified_at` set; if it fails, the alerts stay stored and the crawler's log shows
the error. To add email:

1. Cloudflare dashboard > your domain > **Email > Email Routing**: enable it and
   add your address under **Destination addresses** (Cloudflare sends a
   verification link; Email Workers can only send to verified addresses).
2. Add the binding to `cloudflare/worker/wrangler.toml`:

   ```toml
   [[send_email]]
   name = "ALERT_EMAIL"
   destination_address = "you@example.com"   # your verified address
   ```

3. Write `EmailNotifier` implementing `Notifier`: build a short message from
   `alerts::summary(alerts)` (one line per car: discount, car, price vs baseline,
   link) and send it through the `ALERT_EMAIL` binding. The sender must be an
   address on a domain that uses Email Routing. workers-rs has no typed wrapper for
   `send_email`; call the binding's `send()` through `wasm-bindgen` (a few lines), or
   use an HTTP push service instead (ntfy, Pushover) with `worker::Fetch`, no binding needed.
4. In `entry.rs`, pass `&EmailNotifier { .. }` instead of `&NoNotifier`, then
   `npx wrangler deploy`. Nothing else changes: the rule, the table and the
   once-only logic stay the same, and `service.rs` tests already cover a notifier
   that delivers and one that fails.

## Upgrade: snipe alerts (migration 0003)

For a deployment made before alerts existed (listings already in D1). From the
repo root, logged in with `npx wrangler login`:

```sh
cd cloudflare/worker
npx wrangler d1 migrations apply carbuyer --remote        # lists 0003_new_deal_alerts.sql, confirm with y
npx wrangler d1 execute carbuyer --remote --command "SELECT name FROM sqlite_master WHERE type='table' AND name='new_deal_alerts'"
npx wrangler deploy                                        # Worker: alerts + GET /api/alerts
cd ../pages
npx wrangler pages deploy --branch main                    # dashboard: "New deals" + /api/alerts proxy
```

Apply the migration **before** deploying the Worker: a Worker that writes alerts
into a missing table only logs `alerts: ...` errors (ingest keeps working), but
it is cleaner in this order. Listings stored before the upgrade are not alerted
retroactively; alerts start with the next crawl that finds new or cheaper cars.

## Updating later

```sh
cd cloudflare/worker && npx wrangler deploy                    # code changes
cd cloudflare/worker && npx wrangler d1 migrations apply carbuyer --remote   # new migration files
cd cloudflare/pages  && npx wrangler pages deploy --branch main  # dashboard changes
npx wrangler secret put INGEST_TOKEN                              # rotate the token (then update CARBUYER_INGEST_TOKEN)
```

## Limits worth knowing (Free plan)

- **CPU time**: the Free plan gives a Worker about 10 ms of CPU per request.
  Scoring a few hundred listings fits; many thousands may not. If `/api/deals`
  starts failing with "exceeded CPU", use the snapshot view (the cron has a
  larger budget), filter by make/model, or move to Workers Paid and raise
  `[limits] cpu_ms` in `wrangler.toml`.
- **D1 queries per invocation** are capped by plan. The crawler sends 50
  listings per request (about 100 statements, one D1 batch). Lower
  `apistore.DefaultBatchSize` in `crawler/internal/store/apistore` if a push
  fails with a D1 limit error.
- **Worker size**: the compiled wasm is about 1.8 MB, 0.64 MB gzipped (Free limit: 3 MB gzipped).

## Windows notes (this machine)

- On the development PC, `C:\Windows\System32\vcruntime140.dll` (and
  `msvcp140.dll`, `vcruntime140_1.dll`) cannot be read (access denied). The
  real fix is to repair the "Microsoft Visual C++ 2015-2022 Redistributable
  (x64)" in Settings > Apps. Until then:
  - Rust build scripts are linked with a static C runtime by
    `cloudflare/worker/.cargo/config.toml` + `build.mjs` (sets `RUSTC_BOOTSTRAP=1`
    on Windows only). `wrangler deploy` works as is.
  - Local `wrangler dev` needs `workerd.exe` to start. Copy the three DLLs from a
    readable folder (e.g. `C:\Program Files\Common Files\microsoft shared\ClickToRun\`)
    next to `workerd.exe` in the npx cache
    (`%LOCALAPPDATA%\npm-cache\_npx\<hash>\node_modules\@cloudflare\workerd-windows-64\bin\`).
    Deploying does not need this.
- `wrangler pages dev` and `wrangler dev` may look in different local registry
  folders; set the same `WRANGLER_REGISTRY_PATH` in both terminals. See
  [docs/local-e2e.md](docs/local-e2e.md).
