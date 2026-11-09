// Written before implementation: a strict, DOM-only consumer contract.
import { Client, SandboxError, type Event, type ExecutionOutcome, type Sandbox } from '../src/index.js';

function describe(result: ExecutionOutcome): string {
  switch (result.reason) {
    case 'exited': return String(result.execution.exit_code);
    case 'canceled': return result.execution.id;
    case 'timeout': return result.execution.id;
    case 'oom': return result.execution.id;
    case 'evicted': return result.execution.id;
    case 'unknown': return String(result.terminationReason);
    default: { const exhaustive: never = result; return exhaustive; }
  }
}

async function consumer(client = new Client(), signal?: AbortSignal) {
  const options = signal ? { signal } : {};
  const sandbox = await client.createSandbox({ timeout_seconds: 0, nano_cpus: 0 }, options);
  const started = await client.startExecution(sandbox.id, { command: ['echo', 'hello'] }, options);
  const events: AsyncIterable<Event> = client.streamExecution(sandbox.id, started.id, options);
  for await (const event of events) {
    switch (event.type) {
      case 'stdout': case 'stderr': event.data satisfies string; break;
      case 'truncated': event.limit_bytes satisfies number; break;
      case 'complete': event.execution.id satisfies string; break;
      case 'unknown': event.event_type satisfies string; break;
      default: { const exhaustive: never = event; void exhaustive; }
    }
  }
  await client.streamExecution(sandbox.id, started.id, async event => { void event; }, options);
  describe(await client.exec(sandbox.id, ['false'], options));
  describe(await client.execStream(sandbox.id, ['true'], () => {}, options));
  await client.writeFile(sandbox.id, '/tmp/file', new Uint8Array([0, 255]), options);
  const bytes: Uint8Array = await client.readFile(sandbox.id, '/tmp/file', options);
  const future: Sandbox['state'] = 'future_state';
  const retryable: boolean = new SandboxError('transport', 'failed').retryable;
  void [bytes, future, retryable];
}
void consumer;
