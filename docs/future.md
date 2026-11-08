# Beyond the demo

The demo covers lifecycle, execution and streaming, error classification, and CLI diagnostics. Everything below is a design direction, **not implemented**. Each entry names the main cost, because that cost is usually the reason the feature is not in the demo.

| Capability | Next design step | Main cost or risk |
|---|---|---|
| Templates and snapshots | Immutable, versioned images; separate filesystem snapshots from memory checkpoints; record runtime compatibility | Storage, cold-start time, misleading "restore" semantics |
| Persistent volumes | Define mount ownership, quota, and cleanup independently of the sandbox lifetime | Data isolation, orphaned storage |
| Secret injection | Short-lived, narrowly scoped credentials; redaction in logs and server-side audit | Leakage through logs, environment variables, or untrusted code |
| Multi-tenancy and billing | Tenant-scoped scheduling, quotas, metering, and explicit resource termination | Noisy neighbours, authorization, billing correctness |
| Browser automation | Isolated browser images, network controls, artifact capture, session timeouts | Larger attack surface, heavier resource use |
| More SDK languages | Generate from the stable OpenAPI core, then test idiomatic errors, streaming, and cancellation per language | Semantic drift across Python, TypeScript, and Go |
| Kubernetes or microVM backend | Introduce a runtime interface once Docker behaviour and the contract are stable | Pause, OOM, eviction, and network semantics differ |
| Durable control plane | Persist records, reconcile with the runtime on boot, resume after restart | The reconciler is the interesting part and is real engineering |
| Bounded output in storage | Quota-backed stdout/stderr and replay buffers, sharded fetches for large results, an explicit over-limit error | Silent truncation breaks the contract; the demo's in-memory bound is a stopgap |

The entry gate for any capability is an end-to-end scenario that exposes both its value and its failure mode. Matching method names do not prove that two runtime backends behave alike; a pause in particular must state whether it preserves memory, files, or both.
