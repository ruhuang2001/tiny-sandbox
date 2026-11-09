import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import vm from 'node:vm';
import { build } from 'esbuild';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const out = process.env.E2E_ARTIFACT_DIR;
const domain = process.env.SANDBOX_DOMAIN;
const apiKey = process.env.SANDBOX_API_KEY;
if (!out || !domain || !apiKey) throw new Error('Run scripts/e2e-typescript.sh');
const results = [];

try {
  const bundled = await build({
    entryPoints: [resolve(root, 'dist/index.js')], bundle: true,
    platform: 'browser', format: 'iife', globalName: 'TinySandbox',
    write: false, metafile: true,
  });
  const bundle = bundled.outputFiles[0].text;
  writeFileSync(resolve(out, 'browser-bundle.js'), bundle);
  writeFileSync(resolve(out, 'browser-metafile.json'), JSON.stringify(bundled.metafile, null, 2));
  const context = vm.createContext({
    // Deliberately only Web APIs. No process, Buffer, require, or Node modules.
    fetch, Headers, Request, Response, ReadableStream, TextEncoder, TextDecoder,
    URL, AbortController, AbortSignal, crypto: globalThis.crypto, setTimeout, clearTimeout,
    configuration: { domain, apiKey },
  });
  vm.runInContext(bundle, context);
  const browserResult = await vm.runInContext(`
    (async () => {
      const absentGlobals = ['process', 'Buffer', 'require'].filter(name => typeof globalThis[name] === 'undefined');
      const client = new TinySandbox.Client(configuration);
      const sandbox = await client.createSandbox({ timeout_seconds: 0 });
      try {
        await client.writeFile(sandbox.id, '/work/browser.bin', new Uint8Array([0, 255, 128, 10]));
        const bytes = Array.from(await client.readFile(sandbox.id, '/work/browser.bin'));
        const events = [];
        const outcome = await client.execStream(sandbox.id, ['sh', '-c', 'echo browser; echo web >&2'], event => events.push(event.type));
        return { absentGlobals, bytes, events, reason: outcome.reason, stdout: outcome.execution.stdout, stderr: outcome.execution.stderr };
      } finally { await client.killSandbox(sandbox.id); }
    })()
  `, context);
  assert.equal(browserResult.absentGlobals.length, 3);
  assert.equal(JSON.stringify(browserResult.bytes), '[0,255,128,10]');
  assert.equal(browserResult.reason, 'exited');
  assert.equal(browserResult.stdout, 'browser\n');
  assert.equal(browserResult.stderr, 'web\n');
  assert.ok(browserResult.events.includes('complete'));
  writeFileSync(resolve(out, 'browser-result.json'), JSON.stringify(browserResult, null, 2) + '\n');
  results.push({ name: 'browser bundle with Web APIs only reaches real daemon', passed: true });
  console.log('PASS browser bundle without Node globals');

  const packDir = resolve(out, 'package');
  const consumer = resolve(out, 'package-consumer');
  mkdirSync(packDir, { recursive: true });
  mkdirSync(consumer, { recursive: true });
  const packedOutput = execFileSync('npm', ['pack', '--json', '--pack-destination', packDir], { cwd: root, encoding: 'utf8' });
  writeFileSync(resolve(out, 'npm-pack.json'), packedOutput);
  const packed = JSON.parse(packedOutput)[0];
  const packedPaths = packed.files.map(file => file.path);
  assert.ok(packedPaths.includes('README.md'), 'package includes README.md');
  assert.ok(packedPaths.includes('LICENSE'), 'package includes LICENSE');
  assert.equal(packedPaths.filter(path => path.endsWith('.map')).length, 0, 'clean package excludes stale sourcemaps');
  results.push({ name: 'tarball contains docs and no stale maps', passed: true, files: packedPaths });
  writeFileSync(resolve(consumer, 'package.json'), JSON.stringify({ name: 'tiny-sandbox-e2e-consumer', private: true, type: 'module' }, null, 2));
  const installOutput = execFileSync('npm', ['install', '--ignore-scripts', '--no-audit', '--no-fund', resolve(packDir, packed.filename)], { cwd: consumer, encoding: 'utf8' });
  writeFileSync(resolve(out, 'npm-install.log'), installOutput);
  writeFileSync(resolve(consumer, 'contract.ts'), `
import { Client, SandboxError, type Event, type ExecutionOutcome } from '@tiny-sandbox/sdk';
const client = new Client({ domain: 'http://127.0.0.1', maxRetries: 2 });
const controller = new AbortController();
const events: AsyncIterable<Event> = client.streamExecution('sandbox', 'execution', { signal: controller.signal });
const completion = client.streamExecution('sandbox', 'execution', async event => { if (event.type === 'stdout') event.data.toUpperCase(); });
const outcome: Promise<ExecutionOutcome> = client.exec('sandbox', ['true']);
function explain(value: ExecutionOutcome): string {
  switch (value.reason) {
    case 'unknown': return value.terminationReason ?? '';
    case 'exited': case 'canceled': case 'timeout': case 'oom': case 'evicted': return value.execution.id;
    default: { const unreachable: never = value; return unreachable; }
  }
}
void [events, completion, outcome, explain, SandboxError];
`);
  const typesOutput = execFileSync(process.execPath, [resolve(root, 'node_modules/typescript/bin/tsc'), '--noEmit', '--strict', '--module', 'NodeNext', '--moduleResolution', 'NodeNext', '--target', 'ES2022', '--lib', 'ES2022,DOM,DOM.Iterable', 'contract.ts'], { cwd: consumer, encoding: 'utf8' });
  writeFileSync(resolve(out, 'installed-types.log'), typesOutput + 'PASS: installed tarball strict TypeScript contract\n');
  writeFileSync(resolve(consumer, 'smoke.mjs'), `
import { Client } from '@tiny-sandbox/sdk';
import { writeFileSync } from 'node:fs';
const client = new Client({ domain: process.env.SANDBOX_DOMAIN, apiKey: process.env.SANDBOX_API_KEY });
const sandbox = await client.createSandbox({ timeout_seconds: 0 });
try {
  const outcome = await client.exec(sandbox.id, ['echo', 'installed-tarball']);
  if (outcome.reason !== 'exited' || outcome.execution.stdout !== 'installed-tarball\\n') throw new Error('installed package execution failed');
  writeFileSync(process.env.E2E_ARTIFACT_DIR + '/installed-result.json', JSON.stringify({ reason: outcome.reason, stdout: outcome.execution.stdout }, null, 2));
} finally { await client.killSandbox(sandbox.id); }
`);
  const smokeOutput = execFileSync(process.execPath, [resolve(consumer, 'smoke.mjs')], { cwd: consumer, encoding: 'utf8' });
  writeFileSync(resolve(out, 'installed-runtime.log'), smokeOutput + 'PASS: installed tarball ESM import reaches real daemon\n');
  assert.equal(JSON.parse(readFileSync(resolve(out, 'installed-result.json'), 'utf8')).reason, 'exited');
  results.push({ name: 'installed tarball ESM runtime and strict types', passed: true, filename: packed.filename, integrity: packed.integrity });
  console.log('PASS installed tarball ESM import and strict types');
} catch (error) {
  results.push({ name: 'package smoke failure', passed: false, message: error.message, stack: error.stack });
  process.exitCode = 1;
  console.error(error);
} finally {
  writeFileSync(resolve(out, 'package-results.json'), JSON.stringify({ passed: results.every(result => result.passed), cases: results }, null, 2) + '\n');
}
