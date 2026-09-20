#!/usr/bin/env node
/**
 * Parity check: the original JavaScript carbuyer vs this Go + Rust port.
 *
 *   node parity/parity.mjs                 # original JS expected at ../carbuyer
 *   CARBUYER_JS=/path/to/carbuyer node parity/parity.mjs
 *
 * Offline only. Nothing here touches autohebdo.net. It imports the original
 * modules read-only; it never writes inside the original project.
 *
 * Checks:
 *   1. parser  — JS parseSearchPage vs Go parse on fixtures/rav4-qc.html
 *   2. scorer  — JS buildPriceModel/measureDiscount/scoreAgainst (wired like
 *                buildAppraiser) vs the Rust binary, on the fixture (Go-parsed
 *                for Rust, JS-parsed for JS) with default and loosened options
 *   3. scorer  — same, on a deterministic synthetic market of 4 000 listings
 *                big enough to exercise every fallback tier, trim factors and
 *                the private-party discount
 *   4. schema  — a database created by JS openDb() vs one created by Go
 */
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const PORT = resolve(HERE, '..');
const JS_ROOT = resolve(process.env.CARBUYER_JS ?? join(PORT, '..', 'carbuyer'));
const GO = process.env.GO ?? 'go';
const EXE = process.platform === 'win32' ? '.exe' : '';
const SCORER = process.env.CARBUYER_SCORER ?? join(PORT, 'scorer', 'target', 'release', `carbuyer-scorer${EXE}`);
const FIXTURE = join(PORT, 'crawler', 'testdata', 'rav4-qc.html');

const load = (rel) => import(pathToFileURL(join(JS_ROOT, rel)).href);
const { parseSearchPage } = await load('src/parse.js');
const { buildPriceModel, scoreAgainst } = await load('src/price-model.js');
const { measureDiscount } = await load('src/discount.js');

if (!existsSync(SCORER)) {
  console.error(`Rust scorer not built at ${SCORER}. Run: cargo build --release (in scorer/)`);
  process.exit(2);
}

// ---------------------------------------------------------------- helpers

/** Every difference between two JSON values, as "path: a vs b". */
function diff(a, b, path = '$', out = []) {
  if (out.length > 25) return out;
  if (typeof a === 'number' && typeof b === 'number') {
    if (!Object.is(a, b) && !(a === 0 && b === 0)) out.push(`${path}: ${a} vs ${b}`);
    return out;
  }
  if (a === null || b === null || typeof a !== 'object' || typeof b !== 'object') {
    if (a !== b) out.push(`${path}: ${JSON.stringify(a)} vs ${JSON.stringify(b)}`);
    return out;
  }
  if (Array.isArray(a) !== Array.isArray(b)) {
    out.push(`${path}: array vs object`);
    return out;
  }
  const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
  for (const k of keys) {
    if (!(k in a)) out.push(`${path}.${k}: missing in JS`);
    else if (!(k in b)) out.push(`${path}.${k}: missing in port`);
    else diff(a[k], b[k], `${path}.${k}`, out);
  }
  return out;
}

const json = (v) => JSON.parse(JSON.stringify(v, (_, x) => (x instanceof Map ? Object.fromEntries(x) : x)));

let failures = 0;
function report(name, differences, detail = '') {
  if (differences.length === 0) {
    console.log(`  PASS  ${name}${detail ? ` — ${detail}` : ''}`);
  } else {
    failures += 1;
    console.log(`  FAIL  ${name}`);
    for (const d of differences.slice(0, 25)) console.log(`        ${d}`);
  }
}

/** The JS appraiser, wired exactly like buildAppraiser() in src/watch.js. */
function appraiseJs(listings, { model = {}, discount = {}, privateSources = ['autohebdo', 'kijiji', 'lespac', 'marketplace'] } = {}) {
  const dealerModel = buildPriceModel(
    listings.filter((l) => l.sellerType === 'Dealer' && l.source === 'autohebdo'),
    model,
  );
  const discounts = new Map();
  const allPrivate = [];
  for (const source of privateSources) {
    const rows = listings.filter((l) => l.sellerType === 'PrivateSeller' && l.source === source);
    if (rows.length === 0) continue;
    allPrivate.push(...rows);
    discounts.set(source, measureDiscount(dealerModel, rows, discount));
  }
  const privateModel = buildPriceModel(allPrivate, { ...model, market: 'private' });
  const score = (l) => {
    const d = discounts.get(l.source);
    if (!d) return scoreAgainst([dealerModel], l);
    return scoreAgainst([dealerModel, privateModel], l, { discount: d });
  };
  const summary = {
    sources: [...discounts.keys()],
    dealerListings: dealerModel.coverage.usable,
    privateListings: privateModel.coverage.usable,
    dealerCoverage: dealerModel.coverage,
    privateCoverage: privateModel.coverage,
    discounts: Object.fromEntries(
      [...discounts].map(([source, d]) => [source, {
        global: d.global,
        byModel: Object.fromEntries(d.byModel),
        samples: d.samples.length,
        rejected: d.rejected,
        basisMix: d.basisMix,
        coverage: d.coverage,
        considered: d.considered,
      }]),
    ),
  };
  return { summary: json(summary), scores: listings.map((l) => ({ id: l.id, score: json(score(l)) })) };
}

function scoreRust(listings, options) {
  const input = JSON.stringify(options ? { listings, options } : listings);
  const out = execFileSync(SCORER, { input, maxBuffer: 512 * 1024 * 1024 });
  const parsed = JSON.parse(out.toString('utf8'));
  return { summary: parsed.appraiser, scores: parsed.scores };
}

function compareScorers(name, jsListings, portListings, options) {
  const js = appraiseJs(jsListings, options);
  const rust = scoreRust(portListings, options);
  report(`${name}: appraiser summary`, diff(js.summary, rust.summary));
  const scored = js.scores.filter((s) => s.score).length;
  const tiers = {};
  for (const s of js.scores) if (s.score) tiers[s.score.basis] = (tiers[s.score.basis] ?? 0) + 1;
  report(
    `${name}: ${js.scores.length} listings scored identically`,
    diff(js.scores, rust.scores),
    `${scored} priced, ${js.scores.length - scored} refused; bases ${JSON.stringify(tiers)}`,
  );
  return { scored, total: js.scores.length };
}

// ---------------------------------------------------------------- 1. parser

console.log(`Original JS: ${JS_ROOT}`);
console.log(`Rust scorer: ${SCORER}\n`);
console.log('1. Parser (fixtures/rav4-qc.html)');
const html = readFileSync(FIXTURE, 'utf8');
const jsPage = json(parseSearchPage(html));
const goPage = JSON.parse(
  execFileSync(GO, ['run', './cmd/parsefixture', FIXTURE], { cwd: join(PORT, 'crawler'), maxBuffer: 64 * 1024 * 1024 }).toString('utf8'),
);
report('page totals, query and injected count',
  diff({ total: jsPage.total, pages: jsPage.pages, injectedCount: jsPage.injectedCount, query: jsPage.query },
    { total: goPage.total, pages: goPage.pages, injectedCount: goPage.injectedCount, query: goPage.query }));
report(`${jsPage.listings.length} organic listings, every field`, diff(jsPage.listings, goPage.listings));
report(`${jsPage.allListings.length} listings incl. injected`, diff(jsPage.allListings, goPage.allListings));

// ---------------------------------------------------------------- 2. scorer on the fixture

console.log('\n2. Scorer on the fixture (JS-parsed -> JS scorer, Go-parsed -> Rust scorer)');
compareScorers('defaults', jsPage.listings, goPage.listings);
compareScorers('minComps 3, minThinComps 2', jsPage.listings, goPage.listings, { model: { minComps: 3, minThinComps: 2 } });

// ---------------------------------------------------------------- 3. scorer on a synthetic market

console.log('\n3. Scorer on a synthetic market (seeded, deterministic)');
function mulberry32(seed) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function syntheticMarket(n, seed) {
  const rand = mulberry32(seed);
  const pick = (a) => a[Math.floor(rand() * a.length)];
  const cars = [
    { make: 'Toyota', model: 'RAV4', base: 42000, trims: [['LE', 1], ['XLE', 1.12], ['XSE', 1.2], ['Limited', 1.3], ['Trail', 1.18]] },
    { make: 'Honda', model: 'Civic', base: 30000, trims: [['LX', 1], ['EX', 1.1], ['Sport', 1.08], ['Sport Touring', 1.25], ['Si', 1.3]] },
    { make: 'Ford', model: 'F-150', base: 65000, trims: [['XL', 0.8], ['XLT', 1], ['Lariat', 1.3], ['Platinum', 1.55], ['Big Horn', 1.05]] },
    { make: 'Hyundai', model: 'Elantra', base: 26000, trims: [['Essential', 1], ['Preferred', 1.08], ['Luxury', 1.18], ['N Line', 1.22]] },
    { make: 'RAM', model: '1500', base: 70000, trims: [['Big Horn', 1], ['Sport', 1.15], ['Rebel', 1.2], ['Laramie', 1.3], ['Limited', 1.5]] },
    { make: 'Mazda', model: 'CX-5', base: 38000, trims: [['GX', 1], ['GS', 1.1], ['GT', 1.25], ['Signature', 1.35]] },
  ];
  const noise = ['', ' AWD', ' 4WD SuperCrew', ' - Bas kilométrage - Garantie', ', CAMERA, SIEGES CHAUFFANTS', ' cabine d\'équipement', ' 2019', ' 302A', ' TOIT OUVRANT'];
  const out = [];
  for (let i = 0; i < n; i += 1) {
    const car = pick(cars);
    const [trim, factor] = pick(car.trims);
    const year = 2008 + Math.floor(rand() * 18);
    const age = 2026 - year;
    const km = Math.round(Math.max(5, age * (8000 + rand() * 16000) + rand() * 20000));
    const province = rand() < 0.88 ? 'QC' : pick(['ON', 'NB']);
    const isPrivate = rand() < 0.3;
    const source = isPrivate && rand() < 0.35 ? 'kijiji' : 'autohebdo';
    let price = car.base * factor * Math.pow(0.88, age) - km * 0.05;
    price *= 0.9 + rand() * 0.2;
    if (isPrivate) price *= source === 'kijiji' ? 0.88 : 0.97;
    price = Math.round(Math.max(price, 1500));
    const r = rand();
    const listing = {
      id: `syn-${i}`,
      source,
      make: car.make,
      model: car.model,
      year: r < 0.02 ? null : year,
      km: r > 0.98 ? null : km,
      price: r > 0.02 && r < 0.03 ? 1 : r > 0.03 && r < 0.04 ? 1224 : price,
      trimText: rand() < 0.07 ? pick(['Bas kilométrage', 'JM1BM1M3XE1210873', '', null]) : `${rand() < 0.5 ? trim.toUpperCase() : trim}${pick(noise)}`,
      sellerType: isPrivate ? 'PrivateSeller' : 'Dealer',
      province: rand() < 0.01 ? null : province,
      isDamaged: rand() < 0.03,
      isParts: rand() < 0.01,
      isConditionalPrice: rand() < 0.02,
    };
    out.push(listing);
  }
  return out;
}
const market = syntheticMarket(4000, 20260923);
compareScorers('synthetic defaults', market, market);
compareScorers('synthetic minComps 5, maxYearSpread 2', market, market, { model: { minComps: 5, maxYearSpread: 2 } });
compareScorers('synthetic, discount needs 10 samples', market, market, { discount: { minModelSamples: 10, minGlobalSamples: 20 } });

// ---------------------------------------------------------------- 4. schema

console.log('\n4. SQLite schema (JS openDb vs Go sqlitestore.Open)');
const tmp = mkdtempSync(join(tmpdir(), 'carbuyer-parity-'));
try {
  const { openDb } = await load('src/db.js');
  const jsDb = openDb(join(tmp, 'js.db'));
  const goDbPath = join(tmp, 'go.db');
  execFileSync(GO, ['run', './cmd/carbuyer', '--make', 'toyota', '--model', 'rav4', '--offline', FIXTURE, '--db', goDbPath, '--json', '--scorer', SCORER],
    { cwd: join(PORT, 'crawler'), stdio: ['ignore', 'ignore', 'pipe'] });
  const { DatabaseSync } = await import('node:sqlite');
  const goDb = new DatabaseSync(goDbPath);
  const describe = (db) => {
    const objects = db.prepare("SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name").all();
    const tables = objects.filter((o) => o.type === 'table').map((o) => o.name);
    return json({ objects, columns: Object.fromEntries(tables.map((t) => [t, db.prepare(`PRAGMA table_info(${t})`).all()])) });
  };
  report('tables, columns, types and indexes', diff(describe(jsDb), describe(goDb)));

  // The rows the Go CLI stored, read back by the JS loader, must be the fixture.
  const { loadListings } = await load('src/db.js');
  const back = loadListings(goDb, { sellerType: null });
  const pick = (l) => ({ id: l.id, make: l.make, model: l.model, year: l.year, km: l.km, price: l.price, trimText: l.trimText, sellerType: l.sellerType, province: l.province, isDamaged: l.isDamaged, isParts: l.isParts, description: l.description, imageUrls: l.imageUrls });
  report(`JS loadListings() reads the ${back.length} rows Go stored`, diff(jsPage.listings.map(pick), back.map(pick)));
  jsDb.close();
  goDb.close();
} finally {
  rmSync(tmp, { recursive: true, force: true });
}

console.log(failures === 0 ? '\nParity: all checks passed.' : `\nParity: ${failures} check(s) failed.`);
process.exitCode = failures === 0 ? 0 : 1;
