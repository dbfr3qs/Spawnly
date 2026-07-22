import { test as setup, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { contextFor } from '../helpers/auth';

// remote-substrate: bring org A's (acme) dispatch channel ONLINE before any spec
// runs, as a Playwright `setup` project (mirroring auth.setup.ts).
//
// Why this exists — connecting a substrate is a TWO-sided operation and neither
// side alone is enough:
//   1. the control plane ISSUES a one-time org-bound identity (POST connect), and
//   2. that identity must be INSTALLED into the substrate cluster (the Secret the
//      SPIRE agent mounts) and the agent restarted so it presents the new leaf,
//      attests as the org's x509pop node, obtains its bootstrap SVID, and dials
//      the orchestrator's dispatch channel.
// `make substrate-e2e-setup` does (2) with a locally-signed stand-in leaf but
// never does (1); the connect spec does (1) but cannot do (2). Neither produced a
// live channel, so substrate-dispatch/-lifecycle skipped for want of one — and,
// because Playwright orders files alphabetically, substrate.spec.ts (the only spec
// that connected) ran LAST, after they had already given up.
//
// Doing both here, once, before the chromium project, makes the dependent specs
// self-sufficient instead of relying on another spec file's side effects. It is a
// no-op (skips) when the sim isn't installed, so a plain `make e2e` is unaffected.
//
// NOTE: the connect endpoint ROTATES the identity, which invalidates whatever the
// agent currently presents. That is why the dependent specs must NOT re-connect —
// they assert the channel is active and skip otherwise (see substrate-lifecycle).

const NS = process.env.SUBSTRATE_NAMESPACE ?? 'substrate-sim';
const LEAF_SECRET = 'substrate-identity';

// kubectl runs a command and returns stdout, or '' on failure. The timeout sits
// ABOVE the longest `--timeout=` we pass to kubectl itself (180s rollout waits), so
// this wrapper never truncates a wait it is supposed to be observing. Failures are
// logged rather than swallowed silently — a failed identity install would otherwise
// degrade into an unexplained wait-then-warn.
function kubectl(args: string[], input?: string): string {
  try {
    return execFileSync('kubectl', args, { encoding: 'utf8', input, timeout: 240_000 });
  } catch (e: unknown) {
    const err = e as { stderr?: string; message?: string };
    console.warn(`substrate.setup: kubectl ${args.join(' ')} failed: ${err.stderr ?? err.message ?? 'unknown'}`);
    return '';
  }
}

function simUp(): boolean {
  return kubectl(['get', 'deployment', 'substrate-agent', '-n', NS, '--ignore-not-found', '-o', 'name']).includes(
    'substrate-agent',
  );
}

async function substrateStatus(page: Page): Promise<string> {
  const r = await page.request.get('/api/org');
  if (!r.ok()) return '';
  return (await r.json()).substrate?.status ?? '';
}

async function waitUntil(cond: () => Promise<boolean>, tries = 60, delayMs = 2_000): Promise<boolean> {
  for (let i = 0; i < tries; i++) {
    if (await cond()) return true;
    await new Promise((r) => setTimeout(r, delayMs));
  }
  return false;
}

// installIdentity writes the one-time connect material into the Secret the
// substrate SPIRE agent mounts (svid.crt / svid.key / bundle.crt — the same
// mapping scripts/connect-substrate.sh uses), via a 0700 temp dir that is removed
// immediately. Key material never touches the repo.
function installIdentity(certPem: string, keyPem: string, bundlePem: string): boolean {
  const dir = mkdtempSync(join(tmpdir(), 'substrate-identity-'));
  try {
    writeFileSync(join(dir, 'svid.crt'), certPem, { mode: 0o600 });
    writeFileSync(join(dir, 'svid.key'), keyPem, { mode: 0o600 });
    writeFileSync(join(dir, 'bundle.crt'), bundlePem, { mode: 0o600 });
    const manifest = kubectl([
      '-n', NS, 'create', 'secret', 'generic', LEAF_SECRET,
      `--from-file=svid.crt=${join(dir, 'svid.crt')}`,
      `--from-file=svid.key=${join(dir, 'svid.key')}`,
      `--from-file=bundle.crt=${join(dir, 'bundle.crt')}`,
      '--dry-run=client', '-o', 'yaml',
    ]);
    // Deliberately no assertion on the manifest body: it embeds the private key, so
    // it must never reach an assertion message or test artifact. Warn-and-return
    // keeps a broken sim from failing this dependency project (which would skip the
    // whole suite).
    if (!manifest.includes('svid.crt')) {
      console.warn('substrate.setup: could not render the identity Secret manifest');
      return false;
    }
    return kubectl(['apply', '-f', '-'], manifest) !== '';
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

setup('connect the acme substrate and bring its dispatch channel online', async ({ browser }) => {
  setup.skip(!simUp(), 'substrate sim not installed — run `make substrate-e2e-setup` first');
  setup.setTimeout(420_000);

  const ctx = await contextFor(browser, 'alice', 'acme');
  const page = await ctx.newPage();
  try {
    await page.goto('/');

    // Idempotent: an already-active channel (e.g. a re-run against a warm cluster)
    // needs nothing, and re-connecting would needlessly rotate the identity.
    if ((await substrateStatus(page)) === 'active') return;

    const r = await page.request.post('/api/org/substrate');
    if (r.status() === 503) {
      setup.skip(true, 'orchestrator dev-CA issuer not wired — run `make substrate-e2e-setup`');
      return;
    }
    // WARN-and-return rather than hard-fail from here on. `chromium` depends on this
    // project, so a thrown expect would fail the dependency and Playwright would skip
    // the ENTIRE suite — a degraded substrate sim must not zero out the ~60 tests that
    // have nothing to do with it. The substrate specs skip cleanly on their own
    // channel-not-active guard, which is the correct blast radius.
    if (!r.ok()) {
      console.warn(`substrate.setup: connect failed: ${r.status()} ${await r.text()}`);
      return;
    }
    const resp = await r.json();
    const cert = resp.identity?.certPem;
    const key = resp.identity?.keyPem;
    const bundle = resp.identity?.trustBundlePem;
    if (
      !cert?.includes('BEGIN CERTIFICATE') ||
      !key?.includes('PRIVATE KEY') ||
      !bundle?.includes('BEGIN CERTIFICATE')
    ) {
      // Never log the payload itself — it carries the one-time private key.
      console.warn('substrate.setup: connect response missing identity material (cert/key/bundle)');
      return;
    }

    if (!installIdentity(cert, key, bundle)) return;

    // Restart both: the SPIRE agent must present the NEW leaf (a changed Secret
    // does not roll the pod), and the substrate-agent must re-fetch its bootstrap
    // SVID, which only exists once the CP provisioned it during connect above.
    kubectl(['-n', NS, 'rollout', 'restart', 'daemonset/substrate-spire-agent']);
    kubectl(['-n', NS, 'rollout', 'restart', 'deployment/substrate-agent']);
    kubectl(['-n', NS, 'rollout', 'status', 'daemonset/substrate-spire-agent', '--timeout=180s']);
    kubectl(['-n', NS, 'rollout', 'status', 'deployment/substrate-agent', '--timeout=180s']);

    // The record flips pending → active when the substrate-agent's channel lands.
    // Generous: node attestation + entry propagation + the agent's own retry loop.
    const active = await waitUntil(async () => (await substrateStatus(page)) === 'active', 90);
    if (!active) {
      // Not a hard failure: the dependent specs skip cleanly on a non-active
      // channel. Surface the agent's own diagnosis so the cause is obvious.
      const logs = kubectl(['-n', NS, 'logs', 'deployment/substrate-agent', '--tail=15']);
      console.warn(`substrate channel did not activate; substrate-agent logs:\n${logs}`);
    }
  } finally {
    await ctx.close();
  }
});
