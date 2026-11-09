import type { Execution } from './types.js';

export interface SandboxErrorDetails {
  statusCode?: number;
  execution?: Execution | Pick<Execution, 'id' | 'sandbox_id'>;
  idempotencyKey?: string;
  cause?: unknown;
}

/** Stable codes are the branching API; messages are for people. */
export class SandboxError extends Error {
  readonly code: string;
  readonly statusCode: number;
  readonly execution: SandboxErrorDetails['execution'];
  readonly idempotencyKey: string | undefined;

  constructor(code: string, message: string, details: SandboxErrorDetails = {}) {
    super(message, { cause: details.cause });
    this.name = 'SandboxError';
    this.code = code;
    this.statusCode = details.statusCode ?? 0;
    this.execution = details.execution;
    this.idempotencyKey = details.idempotencyKey;
  }

  get retryable(): boolean {
    return this.code !== 'callback' && this.code !== 'stream' &&
      (this.code === 'transport' || retryableStatus(this.statusCode));
  }
}

export function retryableStatus(status: number): boolean {
  return [429, 500, 502, 503, 504].includes(status);
}

export function canceled(signal: AbortSignal): SandboxError {
  return new SandboxError('canceled', 'sandbox: request aborted', { cause: signal.reason });
}

export function checkSignal(signal?: AbortSignal): void {
  if (signal?.aborted) throw canceled(signal);
}

export function transportError(cause: unknown): SandboxError {
  return cause instanceof SandboxError ? cause :
    new SandboxError('transport', 'sandbox: request or response stream failed', { cause });
}

export function statusError(status: number, text: string): SandboxError {
  let payload: { code?: unknown; message?: unknown } = {};
  try {
    const value: unknown = JSON.parse(text);
    if (value && typeof value === 'object') payload = value;
  } catch { /* Router and proxy errors can be plain text. */ }
  return new SandboxError(
    typeof payload.code === 'string' ? payload.code : 'http_error',
    typeof payload.message === 'string' ? payload.message : 'sandbox: unexpected HTTP status ' + status,
    { statusCode: status },
  );
}
