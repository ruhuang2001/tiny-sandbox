import { checkSignal, SandboxError } from './errors.js';
import { Transport } from './http.js';
import { consume, streamEvents } from './stream.js';
import type {
  ClientConfig, CompletedExecution, CreateSandboxRequest, Event, EventHandler,
  ExecuteRequest, Execution, ExecutionOutcome, RequestOptions, Sandbox,
} from './types.js';

function sandboxPath(id: string): string { return '/v1/sandboxes/' + encodeURIComponent(id); }
function executionPath(sandboxID: string, executionID: string): string {
  return sandboxPath(sandboxID) + '/executions/' + encodeURIComponent(executionID);
}

function newKey(): string {
  try {
    const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
    return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
  } catch (cause) {
    throw new SandboxError('invalid_config', 'sandbox: Web Crypto is required to generate idempotency keys', { cause });
  }
}

/** Convert completed execution data; nonzero exits and server deadlines resolve. */
export function executionOutcome(execution: Execution): ExecutionOutcome {
  if (execution.state !== 'completed') {
    throw new SandboxError('invalid_state', 'sandbox: execution has not completed', { execution });
  }
  const completed = execution as CompletedExecution;
  const reason = completed.termination_reason;
  switch (reason) {
    case 'exited': return { reason: 'exited', execution: { ...completed, termination_reason: 'exited' } };
    case 'canceled': return { reason: 'canceled', execution: { ...completed, termination_reason: 'canceled' } };
    case 'timeout': return { reason: 'timeout', execution: { ...completed, termination_reason: 'timeout' } };
    case 'oom': return { reason: 'oom', execution: { ...completed, termination_reason: 'oom' } };
    case 'evicted': return { reason: 'evicted', execution: { ...completed, termination_reason: 'evicted' } };
    default: return { reason: 'unknown', terminationReason: reason, execution: completed };
  }
}

/** One HTTP endpoint, no global deadline and no runtime dependencies. */
export class Client {
  private readonly transport: Transport;

  constructor(config: ClientConfig = {}) { this.transport = new Transport(config); }

  createSandbox(request: CreateSandboxRequest = {}, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('POST', '/v1/sandboxes', false, options.signal, request);
  }

  getSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('GET', sandboxPath(sandboxID), true, options.signal);
  }

  async listSandboxes(options: RequestOptions = {}): Promise<Sandbox[]> {
    const list = await this.transport.json<{ sandboxes: Sandbox[] }>('GET', '/v1/sandboxes', true, options.signal);
    return list.sandboxes;
  }

  pauseSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('POST', sandboxPath(sandboxID) + '/pause', true, options.signal);
  }

  resumeSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('POST', sandboxPath(sandboxID) + '/resume', true, options.signal);
  }

  killSandbox(sandboxID: string, options: RequestOptions = {}): Promise<Sandbox> {
    return this.transport.json('DELETE', sandboxPath(sandboxID), true, options.signal);
  }

  async startExecution(sandboxID: string, request: ExecuteRequest, options: RequestOptions = {}): Promise<Execution> {
    // An already aborted start sends nothing, including recovery requests.
    checkSignal(options.signal);
    if (!Array.isArray(request.command) || request.command.length === 0 || request.command.some(arg => typeof arg !== 'string')) {
      throw new SandboxError('invalid_request', 'sandbox: command must be a nonempty string array');
    }
    const keyed: ExecuteRequest = { ...request, command: [...request.command], idempotency_key: request.idempotency_key || newKey() };
    try {
      return await this.start(sandboxID, keyed, options.signal);
    } catch (cause) {
      if (!options.signal?.aborted) throw cause;
      throw await this.recover(sandboxID, undefined, options.signal.reason, keyed);
    }
  }

  private start(sandboxID: string, request: ExecuteRequest, signal?: AbortSignal): Promise<Execution> {
    return this.transport.json('POST', sandboxPath(sandboxID) + '/executions', true, signal, request);
  }

  getExecution(sandboxID: string, executionID: string, options: RequestOptions = {}): Promise<Execution> {
    return this.transport.json('GET', executionPath(sandboxID, executionID), true, options.signal);
  }

  async listExecutions(sandboxID: string, options: RequestOptions = {}): Promise<Execution[]> {
    const list = await this.transport.json<{ executions: Execution[] }>(
      'GET', sandboxPath(sandboxID) + '/executions', true, options.signal,
    );
    return list.executions;
  }

  /** Returns the server snapshot; state may still be running during removal. */
  cancelExecution(sandboxID: string, executionID: string, options: RequestOptions = {}): Promise<Execution> {
    return this.transport.json('POST', executionPath(sandboxID, executionID) + '/cancel', true, options.signal);
  }

  streamExecution(sandboxID: string, executionID: string, options?: RequestOptions): AsyncIterableIterator<Event>;
  streamExecution(sandboxID: string, executionID: string, onEvent: EventHandler, options?: RequestOptions): Promise<Execution>;
  streamExecution(sandboxID: string, executionID: string,
    handlerOrOptions: EventHandler | RequestOptions = {}, options: RequestOptions = {}): AsyncIterableIterator<Event> | Promise<Execution> {
    const opts = typeof handlerOrOptions === 'function' ? options : handlerOrOptions;
    const events = streamEvents(this.transport, executionPath(sandboxID, executionID) + '/events', opts,
      () => this.recover(sandboxID, executionID, opts.signal?.reason));
    return typeof handlerOrOptions === 'function' ? consume(events, handlerOrOptions) : events;
  }

  async exec(sandboxID: string, command: readonly string[], options: RequestOptions = {}): Promise<ExecutionOutcome> {
    const started = await this.startExecution(sandboxID, { command }, options);
    return executionOutcome(await consume(this.streamExecution(sandboxID, started.id, options)));
  }

  async execStream(sandboxID: string, command: readonly string[], onEvent: EventHandler,
    options: RequestOptions = {}): Promise<ExecutionOutcome> {
    const started = await this.startExecution(sandboxID, { command }, options);
    return executionOutcome(await this.streamExecution(sandboxID, started.id, onEvent, options));
  }

  readFile(sandboxID: string, path: string, options: RequestOptions = {}): Promise<Uint8Array> {
    return this.transport.request('GET', sandboxPath(sandboxID) + '/files?path=' + encodeURIComponent(path), true, options.signal);
  }

  async writeFile(sandboxID: string, path: string, data: Uint8Array, options: RequestOptions = {}): Promise<void> {
    await this.transport.request('PUT', sandboxPath(sandboxID) + '/files?path=' + encodeURIComponent(path), true,
      options.signal, new Uint8Array(data));
  }

  /** One fresh bounded recovery attempt, sharing the deadline across replay and cancel. */
  private async recover(sandboxID: string, executionID: string | undefined, cause: unknown,
    request?: ExecuteRequest): Promise<SandboxError> {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 5000);
    let execution: Execution | Pick<Execution, 'id' | 'sandbox_id'> | undefined = executionID === undefined ?
      undefined : { id: executionID, sandbox_id: sandboxID };
    const key = request?.idempotency_key;
    try {
      if (request) execution = await this.start(sandboxID, request, controller.signal);
      if (!execution) throw new Error('no execution identity');
      const canceledExecution = await this.cancelExecution(sandboxID, execution.id, { signal: controller.signal });
      execution = canceledExecution;
      if (canceledExecution.state !== 'completed') {
        throw new Error('server cancellation response is not terminal');
      }
      return new SandboxError('canceled', 'sandbox: caller aborted; remote execution is terminal', {
        execution, cause, ...(key ? { idempotencyKey: key } : {}),
      });
    } catch (recoveryCause) {
      return new SandboxError('cancel_unconfirmed', 'sandbox: could not confirm remote execution stopped', {
        cause: recoveryCause, ...(execution ? { execution } : {}), ...(key ? { idempotencyKey: key } : {}),
      });
    } finally { clearTimeout(timer); }
  }
}
