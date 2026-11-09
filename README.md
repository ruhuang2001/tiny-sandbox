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

## Install

On macOS or Linux, paste this command to download the latest
release into `$HOME/tiny-sandbox-quickstart`. Docker is required to run it.

Supported platforms are macOS and Linux on amd64 and arm64. Windows is not
supported, and no Windows binaries are published. WSL2 is not a tested or
supported environment.

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

Use the [Go SDK](docs/sdk.md) to manage sandboxes and execute commands from
your application, or connect directly through the [HTTP API](docs/api.md)
and [OpenAPI specification](api/openapi.yaml).

Add the Go SDK to your module with:

```sh
go get github.com/ruhuang2001/tiny-sandbox/sandbox-go@latest
```

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
- [HTTP API reference](docs/api.md)
- [Versioning and releases](docs/versioning.md)
- [Contributing](CONTRIBUTING.md)

## License

[MIT](LICENSE)
