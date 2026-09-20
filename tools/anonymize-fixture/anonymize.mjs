#!/usr/bin/env node
/**
 * Anonymize a saved AutoHebdo search page so it can live in a public repo.
 *
 *   node tools/anonymize-fixture/anonymize.mjs crawler/testdata/rav4-qc.html            # in place
 *   node tools/anonymize-fixture/anonymize.mjs saved-page.html out.html                  # to a new file
 *   node tools/anonymize-fixture/anonymize.mjs out.html --verify-against saved-page.html # leak check
 *
 * What it does
 * - Keeps ONLY the <script id="__NEXT_DATA__"> payload (the parsers read
 *   nothing else). The rendered markup and third-party scripts, which repeat
 *   dealer names/phones and carry a client-side maps key, are dropped.
 * - Inside __NEXT_DATA__ every key is kept (same structure). Values that
 *   identify a real seller or listing are replaced with consistent fakes:
 *     listing id, crossReferenceId, externalCustomerId, url, images,
 *     dealBuilderUrl*, seller id / companyName / links / logo / phones,
 *     location street, the last 3 characters of the postal code, and the
 *     visitor (cookie) id of whoever saved the page.
 *   Dealer names become "Dealer A", "Dealer B", ... (same dealer = same letter).
 * - Descriptions keep their text (the damage/parts reader uses it) but lose
 *   emails, URLs, domains, phone numbers, street addresses and dealer names.
 * - Everything the parser and scorer use for pricing is untouched: price,
 *   year, km, make/model/trim, seller type, province, city, the postal-code
 *   area (first 3 characters), offer type, damage flags.
 *
 * Deterministic and idempotent: running it again on its own output changes
 * nothing. Contains no real names: dealer aliases are derived from the input.
 */
import { readFileSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const NEXT_DATA = /<script id="__NEXT_DATA__"[^>]*>([\s\S]*?)<\/script>/;
const TITLE = /<title>([\s\S]*?)<\/title>/;
const MARKER = 'anonymized fixture: tools/anonymize-fixture/anonymize.mjs';

// ------------------------------------------------------------ fake values

const letters = (i) => (i < 26 ? String.fromCharCode(65 + i) : letters(Math.floor(i / 26) - 1) + letters(i % 26));
const pad = (n, w) => String(n).padStart(w, '0');
const fakeId = (i) => `00000000-0000-4000-8000-${pad(i + 1, 12)}`;
const fakeRef = (i) => `9000${pad(i + 1, 4)}`;
const dealerLabel = (i) => `Dealer ${letters(i)}`;
const dealerSlug = (i) => `dealer-${letters(i).toLowerCase()}`;
const fakeSellerId = (i) => `1000${pad(i + 1, 4)}`;
// 555-01xx is reserved for fiction in the North American numbering plan.
const fakePhone = (i) => `514-555-${pad(100 + (i % 100), 4)}`;
const fakeCallTo = (i) => `+1514555${pad(100 + (i % 100), 4)}`;
const IS_FAKE_DEALER = /^Dealer [A-Z]{1,2}$/;

// Canadian postal code: keep the area (FSA, first 3 chars) that geo uses,
// replace the local part. Keeps the original spacing.
function fakeZip(zip) {
  if (typeof zip !== 'string') return zip;
  const m = zip.match(/^([A-Za-z]\d[A-Za-z])(\s?)([0-9A-Za-z]{3})$/);
  return m ? `${m[1].toUpperCase()}${m[2]}0A0` : zip;
}

// Image URL: keep a trailing "/250x188.webp"-style size segment (and anything
// after it) because the parser rewrites it; replace host and path.
function fakeImage(url, listingIndex, n) {
  if (typeof url !== 'string') return url;
  const m = url.match(/(\/\d+x\d+\.(?:webp|jpg|png)(?:\?.*)?)$/i);
  return `https://images.example.com/listing-images/${fakeId(listingIndex)}-${n + 1}.jpg${m ? m[1] : ''}`;
}

// ------------------------------------------------------------ text scrubbing

const fold = (s) => s.normalize('NFD').replace(/\p{M}/gu, '');
const escapeRe = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const GENERIC = new Set([
  'auto', 'autos', 'automobile', 'automobiles', 'direct', 'des', 'de', 'du', 'la', 'le', 'les', 'et',
  'saint', 'sainte', 'st', 'ste', 'com', 'inc', 'ltee', 'ltd', 'occasion', 'centre', 'groupe', 'dealer',
  // car brands: a dealer called "<Place> Toyota" must not erase "Toyota" everywhere
  'toyota', 'honda', 'hyundai', 'kia', 'ford', 'mazda', 'nissan', 'subaru', 'chevrolet', 'gmc', 'buick',
  'dodge', 'jeep', 'chrysler', 'ram', 'volkswagen', 'audi', 'bmw', 'mercedes', 'lexus', 'acura', 'mitsubishi',
]);

/** Words that name one dealer (long, not generic, not a city of the page). */
function aliasTokens(companyName, cities) {
  if (typeof companyName !== 'string' || IS_FAKE_DEALER.test(companyName)) return [];
  return fold(companyName)
    .split(/[^\p{L}\p{N}]+/u)
    .filter((t) => t.length >= 5 && !GENERIC.has(t.toLowerCase()) && !cities.has(t.toLowerCase()));
}

/** Accent- and case-insensitive whole-word regex for a folded token. */
function wordRe(token) {
  const chars = [...token].map((c) => {
    const variants = new Set([c, c.toLowerCase(), c.toUpperCase()]);
    // match accented forms too: e -> [eéèêëEÉÈÊË]
    const accented = { a: 'àâäáã', e: 'éèêë', i: 'îïíì', o: 'ôöóò', u: 'ùûüú', c: 'ç', n: 'ñ' }[c.toLowerCase()] ?? '';
    for (const a of accented) {
      variants.add(a);
      variants.add(a.toUpperCase());
    }
    return `[${[...variants].map(escapeRe).join('')}]`;
  });
  return new RegExp(`(?<![\\p{L}\\p{N}])${chars.join('')}(?![\\p{L}\\p{N}])`, 'gu');
}

const EMAIL = /[\p{L}\p{N}._%+-]+@[\p{L}\p{N}.-]+\.[a-z]{2,}/giu;
const URL_RE = /\b(?:https?:\/\/|www\.)[^\s<>"']+/gi;
const DOMAIN = /(?<![@\p{L}\p{N}.-])[\p{L}\p{N}-]+(?:\.[\p{L}\p{N}-]+)*\.(?:com|ca|net|org|qc\.ca)\b/giu;
// 10-digit North American numbers: 514-555-1234, (514) 555 1234, +1 514.555.1234, 5145551234
const PHONE = /(?<!\d)(?:\+?1[\s.-]?)?\(?[2-9]\d{2}\)?[\s.-]?\d{3}[\s.-]?\d{4}(?!\d)/g;

function scrubText(text, { dealerRes, streets }) {
  if (typeof text !== 'string') return text;
  let s = text
    .replace(EMAIL, 'contact@example.com')
    .replace(URL_RE, (u) => (/example\.com/i.test(u) ? u : 'https://www.example.com'))
    .replace(DOMAIN, (d) => (/example\.com$/i.test(d) ? d : 'example.com'))
    .replace(PHONE, '555-0100');
  for (const street of streets) s = s.replace(new RegExp(escapeRe(street), 'gi'), '100 Rue Exemple');
  for (const [re, label] of dealerRes) s = s.replace(re, label);
  return s;
}

// ------------------------------------------------------------ the page

export function anonymizeNextData(data) {
  const props = data?.props?.pageProps;
  if (!props || !Array.isArray(props.listings)) throw new Error('not an AutoHebdo search page: no props.pageProps.listings');
  const listings = props.listings;

  // The cookie id of whoever saved the page. (pageQuery.query_id / trackingId
  // are server-side search ids, kept so the parsed query stays identical.)
  if (props.userSession && typeof props.userSession.visitorId === 'string') {
    props.userSession.visitorId = '00000000-0000-4000-8000-000000000000';
  }
  // Site texts outside the listings (e.g. a form's sample phone number):
  // no phone-shaped string survives anywhere in the fixture.
  const scrubPhones = (v, skip) => {
    if (v === skip) return v;
    if (Array.isArray(v)) return v.map((x) => scrubPhones(x, skip));
    if (v && typeof v === 'object') {
      for (const k of Object.keys(v)) v[k] = scrubPhones(v[k], skip);
      return v;
    }
    return typeof v === 'string' ? v.replace(PHONE, '555-0100') : v;
  };
  scrubPhones(data, listings);

  // Dealers, in order of first appearance.
  const dealers = new Map();
  const dealerOf = (name) => {
    if (!dealers.has(name)) dealers.set(name, IS_FAKE_DEALER.test(name) ? name : dealerLabel(dealers.size));
    return dealers.get(name);
  };
  for (const l of listings) if (typeof l?.seller?.companyName === 'string') dealerOf(l.seller.companyName);
  const dealerIndex = (label) => [...new Set(dealers.values())].indexOf(label);

  const cities = new Set(listings.map((l) => l?.location?.city).filter(Boolean).flatMap((c) => fold(c).toLowerCase().split(/[^\p{L}\p{N}]+/u)));
  const dealerRes = [];
  for (const [name, label] of dealers) {
    if (name === label) continue;
    dealerRes.push([new RegExp(escapeRe(name), 'giu'), label]);
    for (const t of aliasTokens(name, cities)) dealerRes.push([wordRe(t), label]);
  }
  // Longest patterns first, so a full name wins over one of its words.
  dealerRes.sort((a, b) => b[0].source.length - a[0].source.length);
  const streets = [...new Set(listings.map((l) => l?.location?.street).filter((s) => typeof s === 'string' && s !== '100 Rue Exemple'))];
  const ctx = { dealerRes, streets };

  listings.forEach((l, i) => {
    if (!l || typeof l !== 'object') return;
    const oldId = l.id;
    if ('id' in l) l.id = fakeId(i);
    if ('crossReferenceId' in l && l.crossReferenceId != null) l.crossReferenceId = fakeRef(i);
    if (l.identifier && l.identifier.crossReferenceId != null) l.identifier.crossReferenceId = fakeRef(i);
    if (l.identifier && l.identifier.legacyId != null) l.identifier.legacyId = null;
    if (typeof l.externalCustomerId === 'string') l.externalCustomerId = `CUST${pad(i + 1, 6)}`;
    if (typeof l.url === 'string') l.url = `https://www.example.com/annonces/listing-${pad(i + 1, 3)}`;
    if (Array.isArray(l.images)) l.images = l.images.map((u, n) => fakeImage(u, i, n));
    if (typeof l.description === 'string') l.description = scrubText(l.description, ctx);

    const seller = l.seller;
    if (seller && typeof seller === 'object') {
      const label = typeof seller.companyName === 'string' ? dealerOf(seller.companyName) : dealerLabel(dealers.size + i);
      const d = Math.max(0, dealerIndex(label));
      if ('companyName' in seller && seller.companyName != null) seller.companyName = label;
      if (seller.id != null) seller.id = fakeSellerId(d);
      if (seller.links && typeof seller.links === 'object') {
        for (const k of Object.keys(seller.links)) {
          if (typeof seller.links[k] === 'string') seller.links[k] = `https://dealers.example.com/${dealerSlug(d)}/${k}`;
        }
      }
      if (seller.logo && typeof seller.logo === 'object') {
        for (const size of Object.values(seller.logo)) {
          if (size && typeof size.href === 'string') size.href = `https://images.example.com/dealer-logo/${dealerSlug(d)}.png`;
        }
      }
      if (Array.isArray(seller.phones)) {
        seller.phones.forEach((p, n) => {
          if (typeof p.formattedNumber === 'string') p.formattedNumber = fakePhone(d * 3 + n);
          if (typeof p.callTo === 'string') p.callTo = fakeCallTo(d * 3 + n);
        });
      }
      for (const k of ['dealBuilderUrlEN', 'dealBuilderUrlFR']) {
        if (typeof l[k] === 'string') l[k] = `https://${dealerSlug(d)}.example.com/${k.endsWith('EN') ? 'en' : 'fr'}/details/${pad(i + 1, 6)}`;
      }
    }

    const loc = l.location;
    if (loc && typeof loc === 'object') {
      if (typeof loc.street === 'string') loc.street = '100 Rue Exemple';
      if ('zip' in loc) loc.zip = fakeZip(loc.zip);
    }
    // Nothing else may still carry the real listing id (tracking blobs etc.).
    if (typeof oldId === 'string' && oldId !== l.id) {
      const re = new RegExp(escapeRe(oldId), 'g');
      const walk = (v) => {
        if (Array.isArray(v)) return v.map(walk);
        if (v && typeof v === 'object') {
          for (const k of Object.keys(v)) v[k] = walk(v[k]);
          return v;
        }
        return typeof v === 'string' ? v.replace(re, l.id) : v;
      };
      walk(l);
    }
  });
  return data;
}

export function anonymizeHtml(html) {
  const m = html.match(NEXT_DATA);
  if (!m) throw new Error('__NEXT_DATA__ script tag not found');
  const data = anonymizeNextData(JSON.parse(m[1]));
  const title = (html.match(TITLE)?.[1] ?? 'search page').trim();
  // Escape "<" like Next.js does, so text can never close the script tag.
  const json = JSON.stringify(data).replace(/</g, '\\u003c');
  return (
    '<!DOCTYPE html><html lang="fr-CA"><head><meta charSet="utf-8"/>' +
    `<title>${title}</title><!-- ${MARKER} --></head><body>` +
    `<script id="__NEXT_DATA__" type="application/json">${json}</script></body></html>\n`
  );
}

/** Every identifying value of `original` that still appears in `anonymized`. */
export function leaks(originalHtml, anonymizedHtml) {
  const data = JSON.parse(originalHtml.match(NEXT_DATA)[1]);
  const secrets = new Set();
  const add = (v) => typeof v === 'string' && v.length >= 4 && secrets.add(v);
  for (const l of data.props.pageProps.listings) {
    [l.id, l.crossReferenceId, l.externalCustomerId, l.url, l.dealBuilderUrlEN, l.dealBuilderUrlFR,
      l.seller?.id, l.seller?.companyName, l.location?.street, l.location?.zip,
      ...(l.images ?? []), ...(l.seller?.phones ?? []).flatMap((p) => [p.formattedNumber, p.callTo]),
      ...Object.values(l.seller?.links ?? {})].forEach(add);
  }
  add(data.props.pageProps.userSession?.visitorId);
  for (const k of (originalHtml.match(/AIza[0-9A-Za-z_-]{20,}/g) ?? [])) add(k);
  const hay = anonymizedHtml.toLowerCase();
  return [...secrets].filter((s) => hay.includes(s.toLowerCase()));
}

// ------------------------------------------------------------ CLI

const isMain = process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url;
if (isMain) {
  const args = process.argv.slice(2);
  const vi = args.indexOf('--verify-against');
  if (vi >= 0) {
    const [target] = args.filter((_, i) => i !== vi && i !== vi + 1);
    const found = leaks(readFileSync(args[vi + 1], 'utf8'), readFileSync(target, 'utf8'));
    if (found.length) {
      console.error(`${found.length} identifying value(s) still present in ${target}`);
      process.exit(1);
    }
    console.log(`no identifying value of ${args[vi + 1]} found in ${target}`);
  } else if (args.length === 1 || args.length === 2) {
    const [input, output = input] = args;
    writeFileSync(output, anonymizeHtml(readFileSync(input, 'utf8')));
    console.log(`anonymized ${input} -> ${output}`);
  } else {
    console.error('usage: anonymize.mjs <in.html> [out.html]  |  anonymize.mjs <file.html> --verify-against <original.html>');
    process.exit(2);
  }
}
