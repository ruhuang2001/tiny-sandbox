import { checkSignal, canceled, SandboxError, statusError, transportError } from './errors.js';
import type { ClientConfig } from './types.js';

export function backoff(attempt: number): number {
  const cap = Math.min(2000, 200 * 2 ** Math.min(attempt, 4));
  return cap / 2 + Math.random() * cap / 2;
}

function retryAfter(header: string | null): number {
  if (header === null || !/^\d+$/.test(header.trim())) return 0;
  const seconds = Number(header);
  return Number.isSafeInteger(seconds) && seconds > 0 ? seconds * 1000 : 0;
}

/** Chunk long Retry-After delays to avoid setTimeout's signed-32-bit overflow. */
export async function sleep(delay: number, signal?: AbortSignal): Promise<void> {
  checkSignal(signal);
  while (delay > 0) {
    const slice = Math.min(delay, 2_147_483_647);
    await new Promise<void>((resolve, reject) => {
      const done = () => { signal?.removeEventListener('abort', abort); resolve(); };
      const timer = setTimeout(done, slice);
      const abort = () => {
        clearTimeout(timer);
        signal?.removeEventListener('abort', abort);
        reject(canceled(signal!));
      };
      signal?.addEventListener('abort', abort, { once: true });
    });
    delay -= slice;
    checkSignal(signal);
  }
}

export class Transport {
  readonly maxRetries: number;
  private readonly domain: string;
  private readonly apiKey: string;
  private readonly fetcher: typeof globalThis.fetch;

  constructor(config: ClientConfig) {
    // Optional property access, no Node imports or Node type dependency.
    const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env;
    let domain = config.domain || env?.SANDBOX_DOMAIN || 'http://localhost:8080';
    if (!domain.includes('://')) domain = 'http://' + domain;
    try {
      const url = new URL(domain);
      if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) {
        throw new Error('expected an HTTP(S) base URL without credentials, query, or fragment');
      }
      this.domain = url.toString().replace(/\/+$/, '');
    } catch (cause) {
      throw new SandboxError('invalid_config', 'sandbox: invalid domain', { cause });
    }
    this.apiKey = config.apiKey || env?.SANDBOX_API_KEY || '';
    const retries = config.maxRetries ?? 0;
    if (!Number.isSafeInteger(retries)) {
      throw new SandboxError('invalid_config', 'sandbox: maxRetries must be an integer');
    }
    this.maxRetries = retries === 0 ? 3 : Math.max(0, retries);
    const fetcher = config.fetch ?? globalThis.fetch;
    if (typeof fetcher !== 'function') {
      throw new SandboxError('invalid_config', 'sandbox: this runtime must provide fetch');
    }
    this.fetcher = fetcher.bind(globalThis);
  }

  async open(path: string, init: RequestInit): Promise<Response> {
    const headers = new Headers(init.headers);
    if (this.apiKey) headers.set('Authorization', 'Bearer ' + this.apiKey);
    // Redirects must not turn a retry-safe request into a different operation.
    return this.fetcher(this.domain + path, { ...init, headers, redirect: 'error' });
  }

  async request(method: string, path: string, repeatable: boolean, signal?: AbortSignal,
    body?: string | Uint8Array): Promise<Uint8Array> {
    for (let attempt = 0; ; attempt++) {
      checkSignal(signal);
      let after = 0;
      let error: SandboxError;
      try {
        const init: RequestInit = { method };
        if (signal) init.signal = signal;
        if (body !== undefined) {
          // Copy raw bytes: a retry must not observe caller mutations.
          init.body = typeof body === 'string' ? body : new Uint8Array(body);
          init.headers = { 'Content-Type': typeof body === 'string' ? 'application/json' : 'application/octet-stream' };
        }
        const response = await this.open(path, init);
        after = retryAfter(response.headers.get('Retry-After'));
        // Consume fully before accepting the response, including empty PUTs.
        const bytes = new Uint8Array(await response.arrayBuffer());
        if (response.ok) return bytes;
        error = statusError(response.status, new TextDecoder().decode(bytes));
      } catch (cause) {
        if (signal?.aborted) throw canceled(signal);
        error = transportError(cause);
      }
      if (!repeatable || !error.retryable || attempt >= this.maxRetries) throw error;
      await sleep(after || backoff(attempt), signal);
    }
  }

  async json<T>(method: string, path: string, repeatable: boolean, signal?: AbortSignal,
    body?: unknown): Promise<T> {
    let encoded: string | undefined;
    if (body !== undefined) {
      try { encoded = JSON.stringify(body); }
      catch (cause) { throw new SandboxError('invalid_request', 'sandbox: cannot encode request', { cause }); }
    }
    const bytes = await this.request(method, path, repeatable, signal, encoded);
    try { return JSON.parse(new TextDecoder().decode(bytes)) as T; }
    catch (cause) { throw new SandboxError('decode', 'sandbox: invalid JSON response', { cause }); }
  }
}
