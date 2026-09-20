// Build the Worker with worker-build (workers-rs). Used by wrangler's
// [build] command, so `wrangler dev` and `wrangler deploy` both run it.
//
// On Windows it sets RUSTC_BOOTSTRAP=1 so the [unstable]/[host] settings in
// .cargo/config.toml apply: host build scripts then link the C runtime
// statically (see the comment there). Elsewhere it just runs worker-build.
import { spawnSync } from 'node:child_process';

const env = { ...process.env };
if (process.platform === 'win32') env.RUSTC_BOOTSTRAP = '1';

const run = (cmd, args) => {
  const r = spawnSync(cmd, args, { stdio: 'inherit', env });
  if (r.status !== 0) process.exit(r.status ?? 1);
};

const have = spawnSync('worker-build', ['--version'], { env });
if (have.status !== 0) run('cargo', ['install', '-q', 'worker-build']);
run('worker-build', ['--release']);
