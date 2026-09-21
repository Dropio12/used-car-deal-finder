# Used-Car Deal Finder (carbuyer)

- A robot reads used-car ads in Ontario, every 30 minutes.
- It checks what cars like each one usually sell for.
- When a car is a lot cheaper than that, it sends you a message so you get there first.

## How it works

- **The looker** (a Go program) opens car websites and writes down every car it sees.
- **The math friend** (a Rust program) compares each car to cars like it and says how cheap it is.
- **The toy box** (a database, a place that keeps lists) remembers every car and every price it had.
- **The cloud helper** (a Cloudflare Worker) gets the new cars and rings a bell for the good ones.
- **The bell** is a snipe alert: a Telegram message saying "this one is cheap, go now".
- **The eyes** (an AI on Baseten) look at the photos for rust or dents before you drive out.
- **The alarm clock** (GitHub Actions) wakes the looker up every 30 minutes.

## What "cheap" means

- Take every car like this one: same model, close year, close km.
- Their usual price is the **baseline**.
- The bell rings when a car is **15 % or more under** the baseline and there were **at least 6** cars to compare.
- Broken cars, parts cars and "this price only with our loan" ads never ring it.
- Ontario cars are only compared to Ontario cars. Quebec to Quebec, if you turn Quebec on.

## The big picture (C4, level 1)

- C4 is a map with 4 zoom levels. Level 1 shows who talks to whom.
- Dark blue is a person, blue is this project, grey is someone else's service.
- Arrows go down, in the order things happen.

```mermaid
flowchart TB
  gha["⏰ <b>GitHub Actions</b><br/><i>[external system]</i><br/>Alarm clock, every 30 min"]
  carbuyer["🚗 <b>Used-Car Deal Finder</b><br/><i>[our system]</i><br/>Finds cars priced under their market value"]
  sites["🌐 <b>6 car websites</b><br/><i>[external systems]</i><br/>AutoHebdo · Kijiji · LesPAC<br/>Facebook · Craigslist · CarGurus"]
  ai["📸 <b>Baseten AI</b><br/><i>[external system]</i><br/>Looks at the car photos"]
  tg["📱 <b>Telegram via Composio</b><br/><i>[external system]</i><br/>Carries the bell to your phone"]
  access["🔐 <b>Cloudflare Access</b><br/><i>[external system]</i><br/>Door guard: only you get in"]
  you(["🧑 <b>You, the car flipper</b><br/><i>[person]</i><br/>Wants cheap cars first"])

  gha -- "1 · wakes it up" --> carbuyer
  carbuyer -- "2 · reads car ads" --> sites
  carbuyer -- "3 · any rust?" --> ai
  carbuyer -- "4 · rings the bell" --> tg
  carbuyer -- "shows the dashboard" --> access
  tg -- "5 · buzz! cheap car" --> you
  access -- "6 · New deals page" --> you

  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef system fill:#1168bd,stroke:#0b4884,color:#fff
  classDef ext fill:#8a8a8a,stroke:#6b6b6b,color:#fff
  class you person
  class carbuyer system
  class gha,sites,ai,tg,access ext
```

## The rooms inside (C4, level 2)

- The dashed boxes are where things run: your PC or a GitHub robot, and Cloudflare.

```mermaid
flowchart TB
  sites["🌐 <b>6 car websites</b><br/><i>[external systems]</i>"]

  subgraph PC[" "]
    direction TB
    pcT["💻 <b>Your PC or a GitHub robot</b>"]:::title
    cli["🔍 <b>The looker</b><br/><i>[Go program]</i><br/>Visits pages, writes down cars"]
    scorer["🧮 <b>The math friend</b><br/><i>[Rust program]</i><br/>Says how cheap each car is"]
    sqlite[("📦 <b>Home toy box</b><br/><i>[SQLite]</i><br/>Every car + price history")]
    cli -- "is it cheap?" --> scorer
    cli -- "saves cars" --> sqlite
  end

  subgraph CF[" "]
    direction TB
    cfT["☁️ <b>Cloudflare cloud</b>"]:::title
    cron["⏲️ <b>Daily timer</b><br/><i>[Cron, 11:00 UTC]</i><br/>Re-scores once a day"]
    worker["🛎️ <b>The cloud helper</b><br/><i>[Rust Worker]</i><br/>Catches cars, scores them, rings the bell"]
    d1[("📦 <b>Cloud toy box</b><br/><i>[D1]</i><br/>Cars, alerts, daily top 100")]
    pages["🖥️ <b>The dashboard</b><br/><i>[Pages]</i><br/>Web page with New deals"]
    cron -- "wake up" --> worker
    worker -- "saves cars + alerts" --> d1
    worker -- "best deals" --> pages
  end

  ai["📸 <b>Baseten AI</b><br/><i>[external system]</i>"]
  tg["📱 <b>Telegram via Composio</b><br/><i>[external system]</i>"]
  you(["🧑 <b>You</b><br/><i>[person]</i>"])

  sites -- "car pages, 1.5 s apart" --> cli
  cli -- "new cars + secret token" --> worker
  worker -- "check photos" --> ai
  worker -- "send alert" --> tg
  tg -- "buzz" --> you
  pages -- "New deals" --> you

  pcT ~~~ cli
  pcT ~~~ sqlite
  cfT ~~~ cron
  cfT ~~~ worker
  classDef title fill:none,stroke:none,color:#333,font-size:16px
  classDef person fill:#08427b,stroke:#052e56,color:#fff
  classDef cont fill:#438dd5,stroke:#2e6295,color:#fff
  classDef ext fill:#8a8a8a,stroke:#6b6b6b,color:#fff
  class you person
  class cli,scorer,sqlite,cron,worker,d1,pages cont
  class sites,ai,tg ext
  style PC fill:#f4f8fc,stroke:#438dd5,stroke-dasharray: 5 5
  style CF fill:#fff7ec,stroke:#f38020,stroke-dasharray: 5 5
```

- Level 3 (the parts inside the Go program) is in [docs/architecture.md](docs/architecture.md).

## A car's trip

```mermaid
flowchart TB
  A["🌐 Car ad on one of the 6 sites"] --> B["🔍 Looker reads it"]
  B --> C["📦 Saved in the toy box"]
  C --> D{"🆕 New, back, or cheaper?"}
  D -- yes --> E["🧮 Compare to look-alikes"]
  D -- no --> Z["😴 Nothing to do"]
  E --> F{"💸 15 % under,<br/>6+ look-alikes?"}
  F -- yes --> G["🔔 Snipe alert saved"]
  F -- no --> Z2["😴 Nothing to do"]
  G --> H["📸 AI checks photos"]
  G --> J["🖥️ Dashboard: New deals"]
  H --> I["📱 Telegram buzz"]
```

## Every 30 minutes

```mermaid
sequenceDiagram
  autonumber
  participant Clock as ⏰ GitHub Actions
  participant Looker as 🔍 Go crawler
  participant Site as 🌐 6 car sites
  participant Helper as ☁️ Worker
  participant Store as 📦 D1
  participant AI as 📸 Baseten
  participant Phone as 📱 Telegram
  Clock->>Looker: Wake up, run searches.yml
  loop every saved search, page by page
    Looker->>Site: Give me a page of cars
    Site-->>Looker: Page with cars
  end
  Looker->>Helper: POST /api/listings (secret token)
  Helper->>Store: Save cars, remember old prices
  Helper->>Helper: Score new or cheaper cars
  alt a car is a steal
    Helper->>Store: Save snipe alert (once per price)
    Helper->>AI: Look at up to 6 photos
    AI-->>Helper: "Small rust on the wheel arch"
    Helper->>Phone: Cheap car! link + price + photo notes
  else nothing special
    Helper-->>Looker: Saved, no alerts
  end
```

## Being polite to the websites

- One page at a time, at least 1.5 seconds apart, across all the sites together.
- It says it is a normal browser. No logins, no cookies, only pages anyone can open.
- At most 25 searches of 10 pages each per run.
- If a site says no even once, the run stops right there.

---

# For grown-ups

## The code

- `crawler/` (Go): one source per website, all turning ads into the same shape.
  The CLI saves them to SQLite, asks the scorer for prices, and can push them to the Worker.
- `scorer/` (Rust): builds the baseline and scores each car. The Worker reuses it.
- `cloudflare/`: the Worker, the D1 database, the daily cron and the dashboard.
- `.github/workflows/crawl.yml`: runs `crawler/searches.yml` every 30 minutes.
- It started as a Go and Rust copy of an older JavaScript tool (not in this repo).

```mermaid
flowchart TB
  subgraph GO[" "]
    direction TB
    goT["<b>carbuyer CLI (Go)</b>"]:::title
    subgraph SRCS[" "]
      direction LR
      AH[autohebdo] ~~~ KJ[kijiji] ~~~ LP[lespac]
      FB[facebook] ~~~ CL[craigslist] ~~~ CG[cargurus]
    end
    FETCH["fetch<br/>one shared throttle"]
    PIPE[pipeline]
    STORE[store/sqlitestore]
    API["store/apistore<br/>(--push)"]
    SRCS -- "get pages" --> FETCH
    SRCS -- "listings" --> PIPE
    PIPE --> STORE
    PIPE --> API
  end
  subgraph RS[" "]
    direction TB
    rsT["<b>carbuyer-scorer (Rust)</b>"]:::title
    TRIM[trim] --> PM["price_model (dealer)"] --> DISC["discount (private)"] --> SCORE["score (Appraiser)"]
  end
  DB[("carbuyer.db<br/>SQLite")]
  W["Cloudflare Worker"]
  PIPE -- "JSON on stdin" --> TRIM
  STORE --> DB
  API --> W
  goT ~~~ SRCS
  rsT ~~~ TRIM
  classDef title fill:none,stroke:none,color:#333,font-size:15px
  style GO fill:#f4f8fc,stroke:#438dd5,stroke-dasharray: 5 5
  style RS fill:#fbf1ec,stroke:#b7410e,stroke-dasharray: 5 5
  style SRCS fill:none,stroke:#9ab
```

- The scores come back on stdout as JSON, keyed by listing id.
- Sites that only give a free-text title (LesPAC, Craigslist, Facebook) find make and model
  in it by matching against the makes and models AutoHebdo and Kijiji already saved.
- AutoHebdo cars are priced against AutoHebdo cars. The others are priced against every site,
  because private sellers need the dealer prices to compare to.

## Build and test

- Needs Go 1.27 and stable Rust. No C compiler: SQLite is pure Go (`modernc.org/sqlite`).

```sh
cd scorer && cargo test && cargo build --release    # -> scorer/target/release/carbuyer-scorer
cd ../crawler && go vet ./... && go test ./...
```

## Run it

```sh
cd crawler
go run ./cmd/carbuyer --make toyota --model rav4 --geo reg_on --pages 3
go run ./cmd/carbuyer --source kijiji --geo ontario --pages 2
go run ./cmd/carbuyer --source facebook --geo toronto --pages 2
go run ./cmd/carbuyer --source craigslist --geo toronto
go run ./cmd/carbuyer --source cargurus --geo toronto --min-year 2020
go run ./cmd/carbuyer --make toyota --model rav4 --offline testdata/rav4-qc.html --min-comps 3   # no network
```

- `--source` picks the site, `--geo` the place. `carbuyer --help` lists every `--geo` value.
- The other flags match the old JS CLI, plus `--db`, `--scorer`, `--min-comps`, `--top`,
  `--offline`, `--push`, `--searches` and `--no-score`.
- The scorer also works alone: `carbuyer-scorer < listings.json > scores.json`.

## Scheduled crawl

- `crawler/searches.yml` is the list of searches. It watches Ontario.
  The Quebec searches are in the same file, commented out, ready to turn on.
- Unknown keys are refused, so a typo can't quietly drop a filter.
- Run it by hand with `go run ./cmd/carbuyer --searches searches.yml --no-score`.
  Add `--push <worker url>` and `CARBUYER_INGEST_TOKEN` to send the cars to the Worker.
- GitHub Actions runs it every 30 minutes once the secret `CARBUYER_INGEST_TOKEN` and the
  variable `CARBUYER_API_URL` are set ([DEPLOY.md](DEPLOY.md), step 10).
- Or run `scripts/crawl-and-push.ps1` from Windows Task Scheduler, if a site blocks cloud IPs.
  Pick one of the two, not both.

## Cloudflare

- `POST /api/listings` (bearer `INGEST_TOKEN`): saves the pushed cars in D1 and checks
  new, relisted or cheaper ones for snipe alerts.
- `GET /api/deals`, `/api/alerts`, `/api/snapshots/latest`: read by the dashboard,
  only through Cloudflare Access. `GET /api/health` is open.
- Cron `0 11 * * *` (UTC): re-scores everything and keeps the top 100, for 30 days.
- Run it all locally, no account needed:

```sh
cd cloudflare/worker && cp .dev.vars.example .dev.vars
npx wrangler d1 migrations apply carbuyer --local
npx wrangler dev --local --test-scheduled                                          # :8787
cd ../pages && npx wrangler pages dev public --service API=carbuyer-api            # :8788
```

- Locally, the Worker gave the same deals, in the same order, with the same scores as the CLI.
  Details in [docs/local-e2e.md](docs/local-e2e.md). Deploying: [DEPLOY.md](DEPLOY.md).
- Windows: the scorer links the C runtime statically, because `vcruntime140.dll`
  could not be read on the build machine.

## Snipe alerts

- A car is alerted once per price. If the price drops again, it is alerted again.
- The rule lives in `[vars]` in `cloudflare/worker/wrangler.toml` (15 %, 6 cars).
- The dashboard shows alerts at the top under New deals and refreshes every 2 minutes.
- Telegram turns on when `COMPOSIO_API_KEY`, `COMPOSIO_USER_ID` and `TELEGRAM_CHAT_ID` are set.
  Add `BASETEN_API_KEY` and each alert first gets an AI look at up to 6 photos.
- Another channel (email, push) is one new `Notifier` type and one line in `entry.rs`.

## Ported from JavaScript

- Ported: the six sites, fetch and throttle, parser, damage reader, geo, the SQLite store,
  the CLI, and the whole price model (trim, dealer baseline, private discount, appraiser).
- Not yet: the watcher, the old site and server, the other scripts, canonical model names.
- Not yet: saving extra fields some sites give (title, posting date, VIN, CarGurus price rating).
- Not yet: Craigslist remembering which ad pages it already read. It reads at most 20 per run.

## Same answers as the JavaScript

- `node parity/parity.mjs` runs the old JS next to the port, offline. Last run: all 15 checks pass.
- Same parser output on the saved page, and the same prices on 4 000 made-up cars,
  to the last decimal.
- Same database tables, and the old JS reads what Go saved.
- Small on purpose differences: odd input (wrong-type JSON, broken HTML entities,
  emoji inside the description reader's window) is handled without crashing.

## Tests

- Rust: 46 tests. Worker: 31 tests. Go: 437 tests. Clippy and `go vet` are clean.
- Nothing touches a real website. Tests use fake fetchers, local test servers
  and made-up ads (no real names, phone numbers, VINs or ad ids).
- A live check worked on every site in Quebec, and in Ontario on AutoHebdo, Kijiji,
  CarGurus and Craigslist. Facebook in Ontario has not been checked live yet.
- `crawler/testdata/rav4-qc.html` is a real AutoHebdo page with names, phones, addresses,
  ids and links replaced by fakes. Refresh it only with `tools/anonymize-fixture`.

## The websites

- **AutoHebdo**: lots of dealer cars, some private. Ontario (`reg_on`) or Quebec (`reg_qc`).
- **Kijiji**: private and dealer cars. Ontario or Quebec.
- **Facebook Marketplace**: mostly private sellers. 14 Ontario cities, or 4 in Quebec.
- **Craigslist**: private and dealer cars, much bigger in Toronto than in Quebec. Toronto and 11 more Ontario cities,
  or Montreal, Quebec City and Sherbrooke.
- **CarGurus**: dealers only, with CarGurus' own fair price. 100 km around Toronto or Montreal.
- **LesPAC**: private sellers, Quebec only. Kept as an option for Quebec.
