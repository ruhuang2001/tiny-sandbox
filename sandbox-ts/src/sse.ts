import { SandboxError } from './errors.js';
import type { CompletedExecution, Event } from './types.js';

// Worst-case complete events contain ~12 MiB of escaped command and output.
const maxFrameLength = 16 * 1024 * 1024;

function invalid(message: string, cause?: unknown): SandboxError {
  return new SandboxError('stream', 'sandbox: ' + message, { cause });
}

function parseEvent(id: string, type: string, data: string): Event {
  const number = Number(id);
  if (!/^\d+$/.test(id) || !Number.isSafeInteger(number) || number <= 0) {
    throw invalid('event id must be a positive safe integer');
  }
  if (!['stdout', 'stderr', 'truncated', 'complete'].includes(type)) {
    return { id: number, type: 'unknown', event_type: type, data };
  }
  let payload: Record<string, unknown>;
  try {
    const decoded: unknown = JSON.parse(data);
    if (!decoded || typeof decoded !== 'object' || Array.isArray(decoded)) throw new Error('expected object');
    payload = decoded as Record<string, unknown>;
  } catch (cause) { throw invalid('invalid event JSON', cause); }
  if (type === 'stdout' || type === 'stderr') {
    if (typeof payload.data !== 'string') throw invalid('output event has no text');
    return { id: number, type, data: payload.data };
  }
  if (type === 'truncated') {
    if (typeof payload.limit_bytes !== 'number' || !Number.isSafeInteger(payload.limit_bytes) || payload.limit_bytes < 0) {
      throw invalid('truncated event has no valid limit');
    }
    return { id: number, type, limit_bytes: payload.limit_bytes };
  }
  if (payload.state !== 'completed' || typeof payload.id !== 'string' || typeof payload.sandbox_id !== 'string' ||
    !(payload.termination_reason === null || typeof payload.termination_reason === 'string') ||
    !(payload.exit_code === null || (typeof payload.exit_code === 'number' && Number.isInteger(payload.exit_code)))) {
    throw invalid('complete event has no terminal execution');
  }
  return { id: number, type: 'complete', execution: payload as CompletedExecution };
}

/** Incremental SSE framing: UTF-8, CR/LF/CRLF, comments, and multiline data. */
export async function* parseSSE(body: ReadableStream<Uint8Array>): AsyncGenerator<Event> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let fragments: string[] = [];
  let lineLength = 0;
  let skipLF = false;
  let id = '';
  let type = '';
  let data: string[] = [];
  let frameLength = 0;

  function line(value: string): Event | undefined {
    if (value.length > maxFrameLength) throw invalid('SSE line exceeds 16 Mi characters');
    if (!value) {
      const event = data.length || type ? parseEvent(id, type, data.join('\n')) : undefined;
      id = ''; type = ''; data = []; frameLength = 0;
      return event;
    }
    if (value.startsWith(':')) return undefined;
    frameLength += value.length;
    if (frameLength > maxFrameLength) throw invalid('SSE frame exceeds 16 Mi characters');
    const colon = value.indexOf(':');
    const name = colon < 0 ? value : value.slice(0, colon);
    let field = colon < 0 ? '' : value.slice(colon + 1);
    if (field.startsWith(' ')) field = field.slice(1);
    if (name === 'id') id = field;
    else if (name === 'event') type = field;
    else if (name === 'data') data.push(field);
    return undefined;
  }

  try {
    for (;;) {
      const chunk = await reader.read();
      const buffer = decoder.decode(chunk.value, { stream: !chunk.done });
      let start = 0;
      for (let index = 0; index < buffer.length; index++) {
        const char = buffer[index];
        if (skipLF) {
          skipLF = false;
          if (char === '\n') { start = index + 1; continue; }
        }
        if (char !== '\r' && char !== '\n') continue;
        fragments.push(buffer.slice(start, index));
        const event = line(fragments.join(''));
        fragments = [];
        lineLength = 0;
        skipLF = char === '\r';
        start = index + 1;
        if (event) yield event;
      }
      const rest = buffer.slice(start);
      if (rest) fragments.push(rest);
      lineLength += rest.length;
      if (lineLength > maxFrameLength) throw invalid('SSE line exceeds 16 Mi characters');
      if (chunk.done) {
        // Clean EOF can still cut a frame halfway through JSON. Keep the
        // cursor at the last delivered event so a reconnect can replay it.
        // Explicitly delimited malformed frames remain protocol errors.
        let event: Event | undefined;
        try {
          if (fragments.length) line(fragments.join(''));
          event = line('');
        } catch (cause) {
          throw new SandboxError('transport', 'sandbox: stream ended in an incomplete event', { cause });
        }
        // Valid unterminated events are accepted once, matching Go.
        if (event) yield event;
        return;
      }
    }
  } finally {
    try { await reader.cancel(); } catch { /* Already disconnected/aborted. */ }
    reader.releaseLock();
  }
}
