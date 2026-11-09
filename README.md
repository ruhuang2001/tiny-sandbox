# tiny-sandbox

Docker-backed code execution for AI agents, with Go and Python SDKs, a command-line
client, and HTTP API.

- **Sandbox lifecycle** — create, pause, resume, and remove containers with
  memory limits and configurable lifetime and idle timeouts.
- **Streaming output** — consume stdout and stderr over SSE, with event IDs
  for reconnecting and replaying retained output.
- **Execution control** — cancel process groups and retry execution starts
  safely with idempotency keys.
- **Explicit outcomes** — distinguish command exits, timeouts, cancellation,
  OOM kills, and external eviction.

## Install

On macOS or Linux (including WSL2), paste this command to download the latest
release into `$HOME/tiny-sandbox-quickstart`. Docker is required to run it.

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/tiny-sandbox/main/scripts/install.sh | sh
```

## Quick start

Follow the [Quick start tutorial](docs/quickstart.md) to download the latest
release, run Python locally in Docker, and try streaming output and timeouts.
No Go installation or model API key is needed.

## Build from source

Requires Go 1.27+, Make, and a reachable Docker daemon.

```sh
git clone https://github.com/ruhuang2001/tiny-sandbox.git
cd tiny-sandbox
make build
```

Start the service with `./bin/sandboxd`, then use `./bin/sandboxctl` from a
second terminal. See the [CLI reference](docs/cli.md) for commands.

## Integrate

Use the [Go SDK](docs/sdk.md) or [Python SDK](docs/sdk-python.md) to manage
sandboxes and execute commands, or connect directly through the [HTTP API](docs/api.md)
and [OpenAPI specification](api/openapi.yaml).

Add the Go SDK to your module with:

```sh
go get github.com/ruhuang2001/tiny-sandbox/sandbox-go@latest
```

Install the synchronous, standard-library-only Python SDK (Python 3.10+)
from this checkout:

```sh
python3 -m pip install ./sandbox-python
```

Import it as `tiny_sandbox`; the [Python reference](docs/sdk-python.md) includes
streaming, typed errors, cancellation, and sandbox context managers.

The [agent tool example](examples/agent-tool/README.md) shows how to expose
the SDK as a `run_python` tool with structured results and error handling.

## Scope

The server uses Docker container isolation and keeps its state in memory.
It does not provide hardened isolation or multi-tenant access controls, and
sandboxes do not survive server restarts. See [design and
limitations](docs/design.md) and [future directions](docs/future.md).

## Documentation

- [Quick start](docs/quickstart.md)
- [Architecture and execution flows](docs/design.md)
- [CLI reference](docs/cli.md)
- [Go SDK reference](docs/sdk.md)
- [Python SDK reference](docs/sdk-python.md)
- [HTTP API reference](docs/api.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)
