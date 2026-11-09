import { SandboxError, statusError, transportError } from './errors.js';
import { backoff, sleep, type Transport } from './http.js';
import { parseSSE } from './sse.js';
import type { Event, EventHandler, Execution, RequestOptions } from './types.js';

type Cancel = () => Promise<SandboxError>;

/** An iterator-local controller lets return() interrupt an outstanding next(). */
export function streamEvents(transport: Transport, path: string,
  options: RequestOptions, cancel: Cancel): AsyncIterableIterator<Event> {
  const local = new AbortController();
  const generator = readEvents(transport, path, options, cancel, local);
  return {
    [Symbol.asyncIterator]() { return this; },
    next() { return generator.next(); },
    async return() {
      local.abort();
      return generator.return(undefined);
    },
    async throw(error: unknown) {
      local.abort();
      await generator.return(undefined);
      throw error;
    },
  };
}

async function* readEvents(transport: Transport, path: string,
  options: RequestOptions, cancel: Cancel, local: AbortController): AsyncGenerator<Event> {
  const signal = options.signal;
  let cursor = 0;
  let completed = false;
  let recovery: Promise<SandboxError> | undefined;
  // Also clean up while a consumer is between next() calls or in a callback.
  const abort = () => {
    if (!completed && !recovery) recovery = cancel();
    local.abort();
  };
  signal?.addEventListener('abort', abort, { once: true });
  if (signal?.aborted) abort();

  try {
    for (let attempt = 0; ; attempt++) {
      if (recovery) throw await recovery;
      if (local.signal.aborted) return;
      try {
        const headers = new Headers({ Accept: 'text/event-stream' });
        if (cursor > 0) headers.set('Last-Event-ID', String(cursor));
        const init: RequestInit = { method: 'GET', headers, signal: local.signal };
        const response = await transport.open(path, init);
        if (response.status !== 200) throw statusError(response.status, await response.text());
        if (!response.body || !response.headers.get('Content-Type')?.toLowerCase().startsWith('text/event-stream')) {
          await response.body?.cancel();
          throw new SandboxError('stream', 'sandbox: expected an SSE response');
        }
        for await (const event of parseSSE(response.body)) {
          if (recovery) throw await recovery;
          if (local.signal.aborted) return;
          if (event.id <= cursor) continue;
          cursor = event.id;
          if (event.type === 'complete') completed = true;
          yield event;
          if (completed) return;
        }
        throw new SandboxError('transport', 'sandbox: stream ended before complete');
      } catch (cause) {
        if (recovery) throw await recovery;
        if (local.signal.aborted) return;
        const error = transportError(cause);
        if (!error.retryable || attempt >= transport.maxRetries) throw error;
        // One cumulative retry budget. Progress never resets attempt.
        try { await sleep(backoff(attempt), local.signal); }
        catch (cause) {
          if (recovery) throw await recovery;
          if (local.signal.aborted) return;
          throw cause;
        }
      }
    }
  } finally {
    signal?.removeEventListener('abort', abort);
    // Ordinary iterator return only closes the body. An abort reports cleanup.
    if (recovery) throw await recovery;
  }
}

export async function consume(events: AsyncIterable<Event>, handler?: EventHandler): Promise<Execution> {
  for await (const event of events) {
    if (handler) {
      try { await handler(event); }
      catch (cause) {
        // Even SDK errors thrown by application code are callback failures.
        throw new SandboxError('callback', 'sandbox: event handler failed', { cause });
      }
    }
    if (event.type === 'complete') return event.execution;
  }
  throw new SandboxError('stream', 'sandbox: stream ended without complete');
}
