# Future directions

The current implementation covers sandbox lifecycle, execution, streaming, error classification, and CLI diagnostics. The capabilities below are **not implemented**; they describe potential extensions rather than committed delivery plans.

| Capability | Next design step | Main cost or risk |
|---|---|---|
| Templates and snapshots | Immutable, versioned images; separate filesystem snapshots from memory checkpoints; record runtime compatibility | Storage, cold-start time, misleading "restore" semantics |
| Persistent volumes | Define mount ownership, quota, and cleanup independently of the sandbox lifetime | Data isolation, orphaned storage |
| Secret injection | Short-lived, narrowly scoped credentials; redaction in logs and server-side audit | Leakage through logs, environment variables, or untrusted code |
| Multi-tenancy and billing | Tenant-scoped scheduling, quotas, metering, and explicit resource termination | Noisy neighbours, authorization, billing correctness |
| Browser automation | Isolated browser images, network controls, artifact capture, session timeouts | Larger attack surface, heavier resource use |
| Additional SDK languages beyond Go and Python | Generate plain requests from the stable OpenAPI core, then implement and verify retries, replay, errors and cancellation per language | Semantic drift between clients |
| Kubernetes or microVM backend | Implement the existing `daemon.Backend` interface and verify lifecycle and execution semantics against the new runtime | Pause, OOM, eviction, and network semantics differ |
| Durable control plane | Persist records, reconcile with the runtime on boot, resume after restart | Recovery after partial failures, orphan cleanup, and event continuity |
| Persistent output storage | Quota-backed stdout/stderr and replay buffers, sharded fetches for large results, an explicit over-limit error | Preserve explicit truncation signals and retention limits across restarts |

The entry gate for any capability is an end-to-end scenario that exposes both its value and its failure mode. Matching method names do not prove that two runtime backends behave alike; a pause in particular must state whether it preserves memory, files, or both.
