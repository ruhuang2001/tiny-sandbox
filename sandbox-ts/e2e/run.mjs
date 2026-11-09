import assert from 'node:assert/strict';
import { appendFileSync, writeFileSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { setTimeout as delay } from 'node:timers/promises';
import { Client, SandboxError } from '../dist/index.js';
import { FaultProxy } from './proxy.mjs';

const dir = process.env.E2E_ARTIFACT_DIR;
const upstream = process.env.SANDBOX_DOMAIN;
const apiKey = process.env.SANDBOX_API_KEY;
if (!dir || !upstream || !apiKey) throw new Error('Run scripts/e2e-typescript.sh');
for (const file of ['requests.jsonl', 'events.jsonl']) writeFileSync(dir + '/' + file, '');
const proxy = await new FaultProxy(upstream, dir).listen();
const direct = new Client({ domain: upstream, apiKey });
const client = new Client({ domain: proxy.domain, apiKey });
const owned = new Set();
const results = [];
let scenario = '';
const watchdog = setTimeout(() => {
  const failure = { name: scenario, passed: false, error: { message: 'E2E suite exceeded five minutes' } };
  writeFileSync(dir + '/results.json', JSON.stringify({ passed: false, cases: [...results, failure] }, null, 2) + '\n');
  console.error(failure.error.message + ': ' + scenario);
  process.exit(1);
}, 300_000);
watchdog.unref();

function evidence(event) {
  // Large terminal payloads have exact byte lengths and hashes in artifacts.
  // Small events retain their text, making replay assertions independently readable.
  const value = JSON.parse(JSON.stringify(event));
  function summarize(object, field) {
    if (typeof object[field] !== 'string' || object[field].length < 8192) return;
    const text = object[field];
    object[field] = { bytes: Buffer.byteLength(text), sha256: createHash('sha256').update(text).digest('hex') };
  }
  summarize(value, 'data');
  if (value.execution) { summarize(value.execution, 'stdout'); summarize(value.execution, 'stderr'); }
  appendFileSync(dir + '/events.jsonl', JSON.stringify({ scenario, ...value }) + '\n');
}

function errorJSON(error) {
  return { name: error?.name, message: error?.message, code: error?.code, statusCode: error?.statusCode,
    idempotencyKey: error?.idempotencyKey, stack: error?.stack };
}

async function rejected(operation, code) {
  try { await operation; } catch (error) {
    assert.ok(error instanceof SandboxError, 'every SDK failure belongs to SandboxError');
    if (code) assert.equal(error.code, code);
    return error;
  }
  assert.fail('Expected the operation to reject');
}

async function eventually(check, message, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await delay(20);
  }
  assert.fail(message);
}

async function promptly(operation, message) {
  let timer;
  try {
    return await Promise.race([operation, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error(message)), 1500); })]);
  } finally { clearTimeout(timer); }
}

async function create(options = {}) {
  const sandbox = await client.createSandbox({ timeout_seconds: 0, ...options });
  owned.add(sandbox.id);
  return sandbox;
}

async function collect(sandboxId, executionId, c = client) {
  const events = [];
  for await (const event of c.streamExecution(sandboxId, executionId)) { events.push(event); evidence(event); }
  return events;
}

const streamRequests = () => proxy.requests().filter(r => r.path.endsWith('/events'));
const cancelRequests = () => proxy.requests().filter(r => r.path.endsWith('/cancel'));
const starts = () => proxy.requests().filter(r => r.method === 'POST' && r.path.endsWith('/executions'));
const sseJSON = (raw, mutate) => raw.replace(/^data: (.*)$/m, (_, data) => 'data: ' + JSON.stringify(mutate(JSON.parse(data))));

// TCP may coalesce proxy writes. This adapter still fetches the real daemon
// response, but makes parser reads exactly one byte to prove split UTF-8/CRLF.
async function bytewiseFetch(input, init) {
  const response = await fetch(input, init);
  if (!response.body || !response.headers.get('content-type')?.includes('text/event-stream')) return response;
  const reader = response.body.getReader();
  let bytes = new Uint8Array();
  let offset = 0;
  const body = new ReadableStream({
    async pull(controller) {
      if (offset === bytes.length) {
        const result = await reader.read();
        if (result.done) { controller.close(); return; }
        bytes = result.value;
        offset = 0;
      }
      controller.enqueue(bytes.subarray(offset, ++offset));
    },
    cancel(reason) { return reader.cancel(reason); },
  });
  return new Response(body, { status: response.status, headers: response.headers });
}

async function run(name, operation) {
  scenario = name;
  proxy.configure(name);
  const startedAt = Date.now();
  let error;
  let details;
  try { details = await operation(); } catch (caught) { error = caught; }
  proxy.configure(name + ':cleanup');
  for (const id of [...owned]) {
    try { await direct.killSandbox(id); owned.delete(id); } catch (caught) { error ??= caught; }
  }
  const result = { name, passed: !error, durationMs: Date.now() - startedAt, ...(details ? { details } : {}), ...(error ? { error: errorJSON(error) } : {}) };
  results.push(result);
  writeFileSync(dir + '/results.json', JSON.stringify({ passed: results.every(r => r.passed), cases: results }, null, 2) + '\n');
  console.log((error ? 'FAIL ' : 'PASS ') + name + ' (' + result.durationMs + ' ms)');
  if (error) console.error(error.stack ?? error);
}

try {
  await run('authenticated lifecycle and exact binary files', async () => {
    const unauthorized = await rejected(new Client({ domain: proxy.domain, apiKey: 'wrong' }).listSandboxes(), 'unauthorized');
    assert.equal(unauthorized.statusCode, 401);
    assert.equal(unauthorized.retryable, false);
    assert.equal(proxy.requests().length, 1, 'auth failures are not retried');
    const sb = await create();
    assert.equal(sb.state, 'running');
    assert.ok((await client.listSandboxes()).some(value => value.id === sb.id));
    assert.equal((await client.getSandbox(sb.id)).id, sb.id);
    assert.equal((await client.pauseSandbox(sb.id)).state, 'paused');
    assert.equal((await client.resumeSandbox(sb.id)).state, 'running');
    const bytes = Uint8Array.from([0, 1, 127, 128, 255, 0, 10, 13, 240, 159, 140, 141]);
    const path = '/work/a space/雪?&file.bin';
    await client.writeFile(sb.id, path, bytes);
    const read = await client.readFile(sb.id, path);
    assert.ok(read instanceof Uint8Array);
    assert.deepEqual(read, bytes);
    await rejected(client.readFile(sb.id, '/missing'), 'not_found');
    assert.equal((await client.killSandbox(sb.id)).state, 'killed');
    owned.delete(sb.id);
    return { binaryBytes: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex') };
  });

  await run('success nonzero and server timeout outcomes', async () => {
    const sb = await create();
    const ok = await client.exec(sb.id, ['sh', '-c', 'printf out; printf err >&2']);
    assert.equal(ok.reason, 'exited');
    assert.equal(ok.execution.exit_code, 0);
    assert.equal(ok.execution.stdout, 'out');
    assert.equal(ok.execution.stderr, 'err');
    const nonzero = await client.execStream(sb.id, ['sh', '-c', 'exit 42'], evidence);
    assert.equal(nonzero.reason, 'exited');
    assert.equal(nonzero.execution.exit_code, 42);
    const ex = await client.startExecution(sb.id, { command: ['sleep', '30'], timeout_seconds: 1 });
    const terminal = await client.streamExecution(sb.id, ex.id, evidence);
    assert.equal(terminal.termination_reason, 'timeout');
    assert.equal(terminal.state, 'completed');
    assert.equal((await client.getSandbox(sb.id)).state, 'running');
    assert.equal((await client.getExecution(sb.id, ex.id)).termination_reason, 'timeout');
    assert.ok((await client.listExecutions(sb.id)).some(value => value.id === ex.id));
    const timed = await create({ timeout_seconds: 3 });
    const timeoutOutcome = await client.exec(timed.id, ['sleep', '30']);
    assert.equal(timeoutOutcome.reason, 'timeout');
    return { exitCodes: [ok.execution.exit_code, nonzero.execution.exit_code], timeoutReason: timeoutOutcome.reason };
  });

  await run('authenticated reconnect resumes without loss or duplicates', async () => {
    const sb = await create();
    let firstFrame = '';
    let connections = 0;
    proxy.configure(scenario, record => {
      if (!record.path.endsWith('/events')) return {};
      connections += 1;
      return connections === 1
        ? { name: 'drop-after-first-output', sse: { dropAfter: 1, onFrame: (raw, event) => { if (event.type === 'stdout') firstFrame = raw; } } }
        : { name: 'replay-duplicate-frame', sse: { prefix: firstFrame } };
    });
    const execution = await client.startExecution(sb.id, { command: ['sh', '-c', 'for i in 1 2 3 4; do echo line-$i; sleep 0.4; done'] });
    const events = await collect(sb.id, execution.id);
    assert.equal(events.filter(event => event.type === 'stdout').map(event => event.data).join(''), 'line-1\nline-2\nline-3\nline-4\n');
    assert.equal(new Set(events.map(event => event.id)).size, events.length);
    assert.equal(events.at(-1).type, 'complete');
    const requests = streamRequests();
    assert.equal(requests.length, 2);
    assert.ok(requests.every(record => record.authenticated));
    assert.equal(requests[1].lastEventId, String(events[0].id));
    return { reconnects: requests.length - 1, eventIds: events.map(event => event.id) };
  });

  await run('entry gate reconnects without duplicates then confirms cancellation', async () => {
    const sb = await create();
    const controller = new AbortController();
    const delivered = [];
    let firstFrame = '';
    let connections = 0;
    proxy.configure(scenario, record => {
      if (!record.path.endsWith('/events')) return {};
      return ++connections === 1
        ? { name: 'entry-gate-drop-after-first-output', sse: { dropAfter: 1, onFrame: (raw, event) => { if (event.type === 'stdout') firstFrame = raw; } } }
        : { name: 'entry-gate-replay-duplicate', sse: { prefix: firstFrame } };
    });
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'i=1; while [ $i -le 30 ]; do echo entry-$i; i=$((i+1)); sleep 0.5; done'] });
    const error = await rejected(client.streamExecution(sb.id, ex.id, event => {
      evidence(event);
      delivered.push(event);
      if (delivered.filter(value => value.type === 'stdout').length >= 4) controller.abort();
    }, { signal: controller.signal }), 'canceled');
    const output = delivered.filter(event => event.type === 'stdout').map(event => event.data).join('');
    assert.equal(output, 'entry-1\nentry-2\nentry-3\nentry-4\n');
    assert.equal(new Set(delivered.map(event => event.id)).size, delivered.length);
    const requests = streamRequests();
    assert.equal(requests.length, 2);
    assert.equal(requests[1].lastEventId, String(delivered[0].id));
    assert.ok(requests.every(record => record.authenticated));
    assert.equal(cancelRequests().length, 1);
    assert.equal(error.execution.state, 'completed');
    assert.equal(error.execution.termination_reason, 'canceled');
    assert.ok(error.execution.stdout.startsWith(output), 'retained output includes every delivered byte');
    const terminal = await direct.getExecution(sb.id, ex.id);
    assert.equal(terminal.state, 'completed');
    assert.equal(terminal.termination_reason, 'canceled');
    assert.ok(terminal.stdout.startsWith(output));
    return { executionId: ex.id, deliveredOutput: output, retainedOutput: terminal.stdout, eventIds: delivered.map(event => event.id), resumedAfterId: requests[1].lastEventId, terminalState: terminal.state, terminationReason: terminal.termination_reason };
  });

  await run('partial SSE frame at clean EOF reconnects from delivered cursor', async () => {
    const sb = await create();
    let connections = 0;
    proxy.configure(scenario, record => record.path.endsWith('/events') && ++connections === 1
      ? { name: 'clean-eof-mid-second-output-frame', sse: { partialOutputFrame: 2 } } : {});
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'for i in 1 2 3; do echo frame-$i; sleep 0.3; done'] });
    const events = await collect(sb.id, ex.id);
    const output = events.filter(event => event.type === 'stdout');
    assert.equal(output.map(event => event.data).join(''), 'frame-1\nframe-2\nframe-3\n');
    assert.equal(new Set(events.map(event => event.id)).size, events.length);
    assert.equal(events.at(-1).type, 'complete');
    const requests = streamRequests();
    assert.equal(requests.length, 2);
    assert.ok(requests[0].partialFrameId > output[0].id, 'fault cut a later event');
    assert.equal(requests[1].lastEventId, String(output[0].id), 'partial event never advances cursor');
    assert.equal(cancelRequests().length, 0);
    return { partialFrameId: requests[0].partialFrameId, resumedAfterId: requests[1].lastEventId, deliveredIds: events.map(event => event.id) };
  });

  await run('valid unterminated final SSE event completes exactly once', async () => {
    const sb = await create();
    proxy.configure(scenario, record => record.path.endsWith('/events')
      ? { name: 'omit-final-frame-delimiter', sse: { transform: (raw, event) => event.type === 'complete' ? raw.replace(/\r?\n\r?\n$/, '') : raw } } : {});
    const ex = await client.startExecution(sb.id, { command: ['echo', 'unterminated'] });
    const events = await collect(sb.id, ex.id);
    assert.equal(events.filter(event => event.type === 'complete').length, 1);
    assert.equal(events.at(-1).execution.stdout, 'unterminated\n');
    assert.equal(streamRequests().length, 1);
    return { completeEvents: 1, streamAttempts: 1 };
  });

  await run('output never resets the stream retry budget', async () => {
    const sb = await create();
    proxy.configure(scenario, record => record.path.endsWith('/events') ? { name: 'drop-every-connection', sse: { dropAfter: 1 } } : {});
    const c = new Client({ domain: proxy.domain, apiKey, maxRetries: 2 });
    const ex = await c.startExecution(sb.id, { command: ['sh', '-c', 'i=0; while [ $i -lt 30 ]; do echo $i; i=$((i+1)); sleep 0.4; done'] });
    let outputCount = 0;
    const error = await rejected(c.streamExecution(sb.id, ex.id, event => { evidence(event); if (event.type === 'stdout') outputCount += 1; }));
    assert.equal(streamRequests().length, 3);
    assert.equal(outputCount, 3);
    assert.equal(error.retryable, true);
    assert.equal(cancelRequests().length, 0);
    assert.equal((await direct.getExecution(sb.id, ex.id)).state, 'running');
    return { attempts: streamRequests().length, deliveredOutputEvents: outputCount };
  });

  await run('Retry-After overrides backoff on repeatable requests', async () => {
    const sb = await create();
    let calls = 0;
    proxy.configure(scenario, record => record.method === 'GET' && record.path === '/v1/sandboxes/' + sb.id && ++calls === 1
      ? { name: 'retry-after-one-second', statusCode: 503, retryAfter: '1' } : {});
    assert.equal((await client.getSandbox(sb.id)).id, sb.id);
    const requests = proxy.requests().filter(r => r.method === 'GET');
    assert.equal(requests.length, 2);
    assert.ok(requests[1].startedAt - requests[0].closedAt >= 950);
    return { observedDelayMs: requests[1].startedAt - requests[0].closedAt };
  });

  await run('zero retry configuration uses default negative disables retries', async () => {
    proxy.configure(scenario, () => ({ name: 'retryable-every-response', statusCode: 503 }));
    const zero = new Client({ domain: proxy.domain, apiKey, maxRetries: 0 });
    await rejected(zero.listSandboxes(), 'unavailable');
    assert.equal(proxy.requests().length, 4);
    const negative = new Client({ domain: proxy.domain, apiKey, maxRetries: -1 });
    await rejected(negative.listSandboxes(), 'unavailable');
    assert.equal(proxy.requests().length, 5);
    return { defaultAttempts: 4, disabledAttempts: 1 };
  });

  await run('all retryable statuses and truncated HTTP bodies repeat safely', async () => {
    const sb = await create();
    const cases = [];
    for (const status of [429, 500, 502, 504, 'truncated']) {
      let attempts = 0;
      proxy.configure(scenario, record => {
        if (record.method !== 'GET' || record.path !== '/v1/sandboxes/' + sb.id || ++attempts !== 1) return {};
        return status === 'truncated' ? { name: 'cut-before-content-length', truncate: true } : { name: 'retry-status-' + status, statusCode: status };
      });
      assert.equal((await client.getSandbox(sb.id)).id, sb.id);
      assert.equal(attempts, 2);
      cases.push({ fault: status, attempts });
    }
    return { cases };
  });

  await run('ambiguous sandbox create is never retried', async () => {
    let created;
    proxy.configure(scenario, record => record.method === 'POST' && record.path === '/v1/sandboxes'
      ? { name: 'lose-create-response', disconnect: true, onResponse: value => { created = value; owned.add(value.id); } } : {});
    await rejected(client.createSandbox({ timeout_seconds: 0 }));
    assert.ok(created?.id);
    assert.equal(proxy.requests().filter(r => r.method === 'POST').length, 1);
    assert.ok((await direct.listSandboxes()).some(value => value.id === created.id));
    return { createdSandbox: created.id, createAttempts: 1 };
  });

  await run('keyed start recovery never runs a second command', async () => {
    const sb = await create();
    let calls = 0;
    proxy.configure(scenario, record => record.method === 'POST' && record.path.endsWith('/executions') && ++calls === 1
      ? { name: 'lose-start-response', disconnect: true } : {});
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'mkdir -p /work; echo once >> /work/starts; sleep 30'] });
    const requests = starts();
    assert.equal(requests.length, 2);
    assert.match(requests[0].body.idempotency_key, /^[a-f0-9]{32}$/);
    assert.equal(requests[0].body.idempotency_key, requests[1].body.idempotency_key);
    assert.equal((await direct.listExecutions(sb.id)).length, 1);
    assert.equal(new TextDecoder().decode(await direct.readFile(sb.id, '/work/starts')), 'once\n');
    await rejected(client.startExecution(sb.id, { command: ['echo', 'different'], idempotency_key: requests[0].body.idempotency_key }), 'idempotency_conflict');
    assert.equal((await client.cancelExecution(sb.id, ex.id)).state, 'completed');
    return { executionId: ex.id, idempotencyKey: requests[0].body.idempotency_key };
  });

  await run('abort during start recovers same key and confirms cancellation', async () => {
    const sb = await create();
    const controller = new AbortController();
    let calls = 0;
    proxy.configure(scenario, record => record.method === 'POST' && record.path.endsWith('/executions') && ++calls === 1
      ? { name: 'abort-after-server-acceptance', hold: true, onResponse: () => controller.abort() } : {});
    const error = await rejected(client.startExecution(sb.id, { command: ['sleep', '30'] }, { signal: controller.signal }), 'canceled');
    assert.equal(error.execution.state, 'completed');
    assert.equal(error.execution.termination_reason, 'canceled');
    assert.equal(starts().length, 2);
    assert.equal(starts()[0].body.idempotency_key, starts()[1].body.idempotency_key);
    assert.equal(cancelRequests().length, 1);
    assert.equal((await direct.listExecutions(sb.id)).length, 1);
    return { executionId: error.execution.id, recoveryKey: starts()[0].body.idempotency_key };
  });

  await run('unconfirmed start retains cleanup key', async () => {
    const sb = await create();
    const controller = new AbortController();
    let calls = 0;
    proxy.configure(scenario, record => {
      if (record.method !== 'POST' || !record.path.endsWith('/executions')) return {};
      return ++calls === 1 ? { name: 'abort-after-start', hold: true, onResponse: () => controller.abort() }
        : { name: 'lose-recovery-response', statusCode: 503 };
    });
    const c = new Client({ domain: proxy.domain, apiKey, maxRetries: -1 });
    const error = await rejected(c.startExecution(sb.id, { command: ['sleep', '30'] }, { signal: controller.signal }), 'cancel_unconfirmed');
    assert.equal(error.idempotencyKey, starts()[0].body.idempotency_key);
    assert.equal(starts()[1].body.idempotency_key, error.idempotencyKey);
    assert.equal((await direct.listExecutions(sb.id)).length, 1);
    return { idempotencyKey: error.idempotencyKey };
  });

  await run('already aborted start sends nothing', async () => {
    const sb = await create();
    const controller = new AbortController();
    controller.abort();
    await rejected(client.startExecution(sb.id, { command: ['true'] }, { signal: controller.signal }), 'canceled');
    assert.equal(starts().length, 0);
  });

  await run('ambiguous start recovery has a fresh five second deadline', async () => {
    const sb = await create();
    const controller = new AbortController();
    let calls = 0;
    let recoveryStartedAt;
    proxy.configure(scenario, record => {
      if (record.method !== 'POST' || !record.path.endsWith('/executions')) return {};
      return ++calls === 1 ? { name: 'abort-after-accepted-start', hold: true, onResponse: () => controller.abort() }
        : { name: 'hold-recovery-until-deadline', hold: true, onResponse: () => { recoveryStartedAt = Date.now(); } };
    });
    const error = await rejected(client.startExecution(sb.id, { command: ['sleep', '30'] }, { signal: controller.signal }), 'cancel_unconfirmed');
    const elapsed = Date.now() - recoveryStartedAt;
    assert.ok(elapsed >= 4700 && elapsed < 8000, 'recovery owns a fresh bounded deadline');
    assert.equal(starts().length, 2);
    assert.equal(error.idempotencyKey, starts()[0].body.idempotency_key);
    return { recoveryElapsedMs: elapsed, idempotencyKey: error.idempotencyKey };
  });

  await run('callback failures preserve running work including SDK errors', async () => {
    const sb = await create();
    const typed = await rejected(new Client({ domain: proxy.domain, apiKey: 'wrong' }).listSandboxes(), 'unauthorized');
    for (const cause of [new Error('renderer failed'), typed]) {
      const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
      const error = await rejected(client.streamExecution(sb.id, ex.id, event => { evidence(event); throw cause; }), 'callback');
      assert.equal(error.cause, cause);
      assert.equal(error.retryable, false);
      assert.equal(cancelRequests().length, 0);
      assert.equal((await direct.getExecution(sb.id, ex.id)).state, 'running');
      await direct.cancelExecution(sb.id, ex.id);
    }
    return { callbackCauses: ['Error', 'SandboxError'], implicitCancelRequests: 0 };
  });

  await run('stream AbortSignal confirms terminal remote cancellation', async () => {
    const sb = await create();
    const controller = new AbortController();
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
    const error = await rejected(client.streamExecution(sb.id, ex.id, event => { evidence(event); if (event.type === 'stdout') controller.abort(); }, { signal: controller.signal }), 'canceled');
    assert.equal(error.execution.state, 'completed');
    assert.equal(error.execution.termination_reason, 'canceled');
    assert.equal(cancelRequests().length, 1);
    assert.equal((await direct.getExecution(sb.id, ex.id)).state, 'completed');
  });

  await run('nonterminal cancel response remains unconfirmed', async () => {
    const sb = await create();
    const controller = new AbortController();
    proxy.configure(scenario, record => record.path.endsWith('/cancel') ? { name: 'nonterminal-cancel-snapshot', transformJSON: value => ({ ...value, state: 'running', finished_at: null }) } : {});
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
    const error = await rejected(client.streamExecution(sb.id, ex.id, event => { evidence(event); controller.abort(); }, { signal: controller.signal }), 'cancel_unconfirmed');
    assert.equal(error.execution.id, ex.id);
    assert.equal(cancelRequests().length, 1);
  });

  await run('early iterator break closes connection without cancellation', async () => {
    const sb = await create();
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
    for await (const event of client.streamExecution(sb.id, ex.id)) { evidence(event); break; }
    await delay(100);
    assert.equal(cancelRequests().length, 0);
    assert.equal(streamRequests().length, 1);
    assert.ok(streamRequests()[0].closedAt, 'breaking iteration closes the response');
    assert.equal((await direct.getExecution(sb.id, ex.id)).state, 'running');
    await client.cancelExecution(sb.id, ex.id);
  });

  await run('iterator return interrupts a pending silent next without cancel', async () => {
    const sb = await create();
    const ex = await client.startExecution(sb.id, { command: ['sleep', '30'] });
    const iterator = client.streamExecution(sb.id, ex.id)[Symbol.asyncIterator]();
    const pending = iterator.next();
    pending.catch(() => {});
    await eventually(() => streamRequests().some(r => r.upstreamStatus === 200), 'silent stream never opened');
    const returned = await promptly(iterator.return(), 'iterator.return() stayed blocked behind a pending next()');
    assert.equal(returned.done, true);
    assert.equal((await promptly(pending, 'pending next() did not settle after return()')).done, true);
    await eventually(() => Boolean(streamRequests()[0].closedAt), 'return did not close the HTTP connection');
    assert.equal(cancelRequests().length, 0);
    assert.equal((await direct.getExecution(sb.id, ex.id)).state, 'running');
  });

  await run('AbortSignal cleans up while iterator is suspended', async () => {
    const sb = await create();
    const controller = new AbortController();
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
    const iterator = client.streamExecution(sb.id, ex.id, { signal: controller.signal })[Symbol.asyncIterator]();
    assert.equal((await iterator.next()).value.type, 'stdout');
    controller.abort();
    await eventually(() => cancelRequests().length === 1, 'abort did not start cleanup while iterator was suspended');
    await eventually(async () => (await direct.getExecution(sb.id, ex.id)).state === 'completed', 'remote work stayed running', 3000);
    const error = await rejected(iterator.next(), 'canceled');
    assert.equal(error.execution.state, 'completed');
  });

  await run('AbortSignal cleans up before a waiting callback resumes', async () => {
    const sb = await create();
    const controller = new AbortController();
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
    let release;
    let entered = false;
    const gate = new Promise(resolve => { release = resolve; });
    const consuming = client.streamExecution(sb.id, ex.id, async event => { evidence(event); entered = true; await gate; }, { signal: controller.signal });
    consuming.catch(() => {});
    await eventually(() => entered, 'output callback never started');
    try {
      controller.abort();
      await eventually(() => cancelRequests().length === 1, 'abort waited for callback before starting remote cleanup');
      await eventually(async () => (await direct.getExecution(sb.id, ex.id)).state === 'completed', 'remote work stayed running', 3000);
    } finally { release(); }
    const error = await rejected(consuming, 'canceled');
    assert.equal(error.execution.state, 'completed');
  });

  await run('real truncation preserves a multi-megabyte escaped completion', async () => {
    const sb = await create();
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'head -c 1100000 /dev/zero'] });
    const events = await collect(sb.id, ex.id);
    const truncated = events.filter(event => event.type === 'truncated');
    assert.equal(truncated.length, 1);
    assert.equal(truncated[0].limit_bytes, 1048576);
    const terminal = events.at(-1);
    assert.equal(terminal.type, 'complete');
    assert.equal(terminal.execution.output_truncated, true);
    // The server drops a whole chunk that would cross its byte budget.
    assert.ok(terminal.execution.stdout.length <= 1048576);
    assert.ok(terminal.execution.stdout.length >= 1048576 - 32768);
    assert.ok(JSON.stringify(terminal.execution).length > 5 * 1024 * 1024);
    assert.equal(events.filter(e => e.type === 'stdout').map(e => e.data).join(''), terminal.execution.stdout);
    return { retainedBytes: terminal.execution.stdout.length, completionJSONBytes: JSON.stringify(terminal.execution).length };
  });

  await run('CRLF fragmented UTF8 and unknown SSE events', async () => {
    const sb = await create();
    proxy.configure(scenario, record => record.path.endsWith('/events') ? { name: 'future-event-crlf-fragmented-utf8', sse: { crlf: true, fragmented: true, transform: (raw, event) => {
      const shifted = raw.replace(/^id: \d+$/m, 'id: ' + (event.id * 2));
      return event.type === 'stdout' ? 'id: ' + (event.id * 2 - 1) + '\nevent: future_status\ndata: {"detail":"雪"}\n\n' + shifted : shifted;
    } } } : {});
    const ex = await client.startExecution(sb.id, { command: ['printf', '雪🌍\n'] });
    const c = new Client({ domain: proxy.domain, apiKey, fetch: bytewiseFetch });
    const events = await collect(sb.id, ex.id, c);
    assert.equal(events.filter(e => e.type === 'stdout').map(e => e.data).join(''), '雪🌍\n');
    const unknown = events.find(e => e.type === 'unknown');
    assert.equal(unknown.event_type, 'future_status');
    assert.equal(unknown.data, '{"detail":"雪"}');
    assert.equal(events.at(-1).type, 'complete');
  });

  await run('additive fields and future termination reason stay usable', async () => {
    const sb = await create();
    proxy.configure(scenario, record => record.path.endsWith('/events')
      ? { name: 'future-completion-reason', sse: { transform: (raw, event) => event.type === 'complete' ? sseJSON(raw, value => ({ ...value, termination_reason: 'future_reason', future_field: { accepted: true } })) : raw } }
      : { name: 'optional-fields-omitted-additive-json-field', transformJSON: value => {
        const { pids_limit, nano_cpus, network, ...olderResource } = value;
        return { ...olderResource, future_field: { accepted: true } };
      } });
    const olderResource = await client.getSandbox(sb.id);
    assert.equal(olderResource.id, sb.id);
    assert.equal(olderResource.pids_limit, undefined);
    assert.equal(olderResource.nano_cpus, undefined);
    assert.equal(olderResource.network, undefined);
    const outcome = await client.exec(sb.id, ['true']);
    assert.equal(outcome.reason, 'unknown');
    assert.equal(outcome.terminationReason, 'future_reason');
    assert.equal(outcome.execution.termination_reason, 'future_reason');
  });

  await run('malformed known SSE payload fails without retry or cancel', async () => {
    const sb = await create();
    proxy.configure(scenario, record => record.path.endsWith('/events') ? { name: 'invalid-output-json', sse: { transform: (raw, event) => event.type === 'stdout' ? raw.replace(/^data: .*$/m, 'data: {broken') : raw } } : {});
    const ex = await client.startExecution(sb.id, { command: ['sh', '-c', 'echo ready; sleep 30'] });
    const error = await rejected(client.streamExecution(sb.id, ex.id, evidence), 'stream');
    assert.equal(error.retryable, false);
    assert.equal(streamRequests().length, 1);
    assert.equal(cancelRequests().length, 0);
    assert.equal((await direct.getExecution(sb.id, ex.id)).state, 'running');
  });
} finally {
  proxy.configure('final-cleanup');
  for (const id of owned) { try { await direct.killSandbox(id); } catch { /* Shell cleanup remains authoritative. */ } }
  await proxy.close();
  clearTimeout(watchdog);
}

console.log(results.filter(result => result.passed).length + '/' + results.length + ' E2E scenarios passed. Artifacts: ' + dir);
process.exitCode = results.every(result => result.passed) ? 0 : 1;
