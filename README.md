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

Download the archive for your operating system and architecture from
[v0.0.1](https://github.com/ruhuang2001/tiny-sandbox/releases/tag/v0.0.1).
Verify its SHA256 against the release's `checksums.txt`, extract it, and put
`sandboxd` and `sandboxctl` on your `PATH`. Releases include Linux, macOS, and
Windows binaries for amd64 and arm64. A reachable Docker daemon is still
required; `sandboxctl doctor` also needs the Docker CLI for local checks.

`sandboxd` connects through a Docker Unix socket, `/var/run/docker.sock` by
default. Override it with `-docker /path/to/docker.sock` or
`DOCKER_HOST=unix:///path/to/docker.sock`; TCP endpoints and Windows named
pipes are not supported. The CLI can connect to a server on another machine
using `SANDBOX_DOMAIN`.

To build from source instead, follow the quick start below.

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

Add the Go SDK to your module with:

```sh
go get github.com/ruhuang2001/tiny-sandbox/sandbox-go@v0.0.1
```

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
