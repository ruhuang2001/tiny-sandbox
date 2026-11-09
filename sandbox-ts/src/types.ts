/** Open response enums let older clients preserve values from newer servers. */
export type OpenString<T extends string> = T | (string & {});
export type Action = 'kill' | 'pause';
export type TerminationReason = 'exited' | 'canceled' | 'timeout' | 'oom' | 'evicted';

export interface Sandbox {
  id: string;
  state: OpenString<'pending' | 'running' | 'paused' | 'killed'>;
  image: string;
  memory_mb: number;
  /** Additive controls can be absent on older development servers. */
  pids_limit?: number;
  nano_cpus?: number;
  network?: OpenString<'bridge' | 'none'>;
  timeout_seconds: number;
  timeout_action: OpenString<Action>;
  idle_timeout_seconds: number;
  idle_timeout_action: OpenString<Action>;
  created_at: string;
  expires_at: string | null;
  kill_reason: OpenString<'requested' | 'timeout' | 'idle_timeout' | 'evicted'> | null;
  [field: string]: unknown;
}

export interface Execution {
  id: string;
  sandbox_id: string;
  command: string[];
  state: OpenString<'running' | 'completed'>;
  exit_code: number | null;
  termination_reason: OpenString<TerminationReason> | null;
  /** List responses omit output. No artificial empty-string defaults. */
  stdout?: string;
  stderr?: string;
  output_truncated: boolean;
  started_at: string;
  finished_at: string | null;
  [field: string]: unknown;
}

export type CompletedExecution = Execution & { state: 'completed' };

/** New wire reasons map to unknown, keeping consumer switches exhaustive. */
export type ExecutionOutcome = {
  [R in TerminationReason]: {
    reason: R;
    execution: CompletedExecution & { termination_reason: R };
  }
}[TerminationReason] | {
  reason: 'unknown';
  terminationReason: string | null;
  execution: CompletedExecution;
};

export interface CreateSandboxRequest {
  image?: string;
  memory_mb?: number;
  pids_limit?: number;
  nano_cpus?: number;
  network?: 'bridge' | 'none';
  timeout_seconds?: number;
  timeout_action?: Action;
  idle_timeout_seconds?: number;
  idle_timeout_action?: Action;
}

export interface ExecuteRequest {
  command: readonly string[];
  idempotency_key?: string;
  timeout_seconds?: number;
}

export interface RequestOptions { signal?: AbortSignal }

export type Event =
  | { id: number; type: 'stdout' | 'stderr'; data: string }
  | { id: number; type: 'truncated'; limit_bytes: number }
  | { id: number; type: 'complete'; execution: CompletedExecution }
  | { id: number; type: 'unknown'; event_type: string; data: string };

export type EventHandler = (event: Event) => void | Promise<void>;

export interface ClientConfig {
  /** Defaults to SANDBOX_DOMAIN in Node, then http://localhost:8080. */
  domain?: string;
  /** Defaults to SANDBOX_API_KEY in Node. Never put keys in browser bundles. */
  apiKey?: string;
  /** Zero/omitted: 3; negative: no retries. Same convention as the Go SDK. */
  maxRetries?: number;
  /** Fetch-compatible transport; it must honor AbortSignal and stream bodies. */
  fetch?: typeof globalThis.fetch;
}
