# tiny-sandbox

Docker-backed code execution for AI agents, with a Go SDK, command-line
client, and HTTP API.

- **Sandbox lifecycle** — create, pause, resume, and remove containers with
  memory limits and configurable lifetime and idle timeouts.
- **Streaming output** — consume stdout and stderr over SSE, with event IDs
  for reconnecting and replaying retained output.
- **Execution control** — cancel process groups and retry execution starts
  safely with idempotency keys.
- **Explicit outcomes** — distinguish command exits, timeouts, cancellation,
  OOM kills, and external eviction.

## Quick start

Requires Go 1.27+, Make, and a reachable Docker daemon.

```sh
git clone https://github.com/ruhuang2001/tiny-sandbox.git
cd tiny-sandbox
make build
./bin/sandboxd
```

The server listens on `127.0.0.1:8080`. In another terminal, from the same
directory:

```sh
./bin/sandboxctl doctor
./bin/sandboxctl run --rm -- sh -c 'echo hello'
./bin/sandboxctl run --rm -- code 'print(2 ** 10)'
```

`--rm` requests cleanup of the newly created sandbox after execution. Command
stdout goes to stdout; command stderr and execution status go to stderr. Set `SANDBOX_DOMAIN` to connect to another
server and `SANDBOX_API_KEY` when authentication is enabled.

## Integrate

Use the [Go SDK](docs/sdk.md) to manage sandboxes and execute commands from
your application, or connect directly through the [HTTP API](docs/api.md)
and [OpenAPI specification](api/openapi.yaml).

The [agent tool example](examples/agent-tool/README.md) shows how to expose
the SDK as a `run_python` tool with structured results and error handling.

## Scope

The server uses Docker container isolation and keeps its state in memory.
It does not provide hardened isolation or multi-tenant access controls, and
sandboxes do not survive server restarts. See [design and
limitations](docs/design.md) and [future directions](docs/future.md).

## Documentation

- [Architecture and execution flows](docs/design.md)
- [CLI reference](docs/cli.md)
- [Go SDK reference](docs/sdk.md)
- [HTTP API reference](docs/api.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)
