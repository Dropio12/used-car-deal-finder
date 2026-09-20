// Compares the Worker's GET /api/deals answer with the native pipeline's
// `carbuyer --json` report (Go CLI + native Rust scorer binary).
//
//   node compare-deals.mjs cli.json worker.json
//
// Passes when both priced the same listings, rank them in the same order,
// and every score field the CLI reports is identical (full float precision).
// The Go CLI decodes scores into scoring.Score, which does not keep the
// scorer's diagnostic fields (kmSlope, medianKm, trimFactor, yearsPooled);
// those are listed, not compared.
import { readFileSync } from 'node:fs';

const [cliPath, workerPath] = process.argv.slice(2);
if (!cliPath || !workerPath) {
  console.error('usage: node compare-deals.mjs cli.json worker.json');
  process.exit(2);
}
const cli = JSON.parse(readFileSync(cliPath, 'utf8'));
const worker = JSON.parse(readFileSync(workerPath, 'utf8'));

const native = cli.deals.filter((d) => d.score).map((d) => ({ id: d.listing.id, score: d.score }));
const remote = worker.deals.map((d) => ({ id: d.id, score: d.score }));

const canon = (v) =>
  Array.isArray(v) ? v.map(canon)
    : v && typeof v === 'object' ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, canon(v[k])]))
    : v;

const problems = [];
const extra = new Set();
if (native.length !== remote.length) problems.push(`priced: native ${native.length}, worker ${remote.length}`);
native.forEach((n, i) => {
  const w = remote[i];
  if (!w) return;
  if (n.id !== w.id) return problems.push(`rank ${i + 1}: native ${n.id}, worker ${w.id}`);
  const keys = Object.keys(n.score);
  Object.keys(w.score).filter((k) => !keys.includes(k)).forEach((k) => extra.add(k));
  const a = JSON.stringify(canon(n.score));
  const b = JSON.stringify(canon(Object.fromEntries(keys.map((k) => [k, w.score[k]]))));
  if (a !== b) problems.push(`${n.id}: scores differ\n  native ${a}\n  worker ${b}`);
});

console.log(`native: ${cli.comps} comps, ${cli.scored} priced | worker: ${worker.comps} comps, ${worker.scored} priced`);
for (const [i, d] of worker.deals.slice(0, 5).entries())
  console.log(`  #${i + 1} ${d.score.discountPct >= 0 ? '+' : ''}${d.score.discountPct}%  ${d.year} ${d.make} ${d.model}  ${d.price} vs baseline ${d.score.baseline}  (${d.score.basis}, n=${d.score.n})`);
if (problems.length) {
  console.log(`MISMATCH (${problems.length})\n` + problems.join('\n'));
  process.exit(1);
}
if (extra.size) console.log(`  (worker also returns: ${[...extra].sort().join(', ')})`);
console.log(`MATCH: ${remote.length} deals, same order, identical scores`);
