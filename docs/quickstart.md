# Quick start

Download the latest stable release, execute real Python, and try streaming
output and timeouts.
You need a running Docker daemon and the Docker CLI. No Git
clone, Go installation, local Python, or model API key is needed. First use
requires network access to GitHub and the container image registry.

This walkthrough uses a macOS or Linux shell. On Windows, use the Linux
binaries inside WSL2 with a reachable Docker Unix socket. Native Windows
Docker named pipes are not supported by the server.

## 1. Install

Download and install the latest release:

```sh
curl -fsSL https://raw.githubusercontent.com/ruhuang2001/tiny-sandbox/main/scripts/install.sh | sh
```

The installer selects your platform and puts both binaries in
`$HOME/tiny-sandbox-quickstart`. No sudo is needed.

## 2. Start the server (terminal A)

Start Docker, then run:

```sh
cd "$HOME/tiny-sandbox-quickstart"
docker info
export DOCKER_HOST="${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}')}"
case "$DOCKER_HOST" in
  unix://*) ./sandboxd ;;
  *) echo "A local Docker Unix socket is required; current endpoint: $DOCKER_HOST" ;;
esac
```

Keep this terminal open. The server listens on `127.0.0.1:8080`.

The command uses your existing `DOCKER_HOST`, or discovers the endpoint from
your Docker context. It does not hardcode a user-specific socket path. If it
cannot connect, check `docker info`. You can also specify the Unix socket path
explicitly with `./sandboxd -docker /path/to/docker.sock`.

## 3. Run Python (terminal B)

Open another terminal and run:

```sh
cd "$HOME/tiny-sandbox-quickstart"
export SANDBOX_DOMAIN=http://127.0.0.1:8080
./sandboxctl doctor
./sandboxctl run --rm -- code 'print(2 ** 10)'
```

`doctor` should finish with `Ready`. The Python command prints `1024`, with
an execution status on stderr containing `completed (exited), exit 0`.
Missing container images are pulled on first use, which needs registry access.

Try changing the Python expression and running it again:

```sh
./sandboxctl run --rm -- code 'print(sum([19, 29, 49]))'
```

Expected stdout: `97`. Each run creates a fresh sandbox.

`--rm` requests cleanup of the newly created sandbox after execution. Command
stdout goes to stdout; command stderr and execution status go to stderr. Set `SANDBOX_DOMAIN` to connect to another
server and `SANDBOX_API_KEY` when authentication is enabled.

## 4. Watch streaming output

This program prints `1`, `2`, and `3` about one second apart. `flush=True`
makes Python flush each line so you can see the output arrive:

```sh
./sandboxctl run --rm -- code 'import time
for step in range(1, 4):
    print(step, flush=True)
    time.sleep(1)'
```

## 5. Try an execution timeout

Give a ten-second program a two-second execution deadline:

```sh
./sandboxctl run --rm --exec-timeout 2 -- code 'import time; time.sleep(10)'
echo "Exit code: $?"
```

The command reports `timeout` and exits with status `1`. This is expected:
the execution deadline stopped the program.

## Finish and troubleshoot

The examples request cleanup with `--rm`. If cleanup failed, run
`./sandboxctl list` and remove the relevant sandbox with
`./sandboxctl kill SANDBOX_ID`. Stop the server with Ctrl+C in its terminal.

- If `docker info` fails, start Docker and check access to its daemon.
- If `doctor` fails, follow its printed hint and check `SANDBOX_DOMAIN`.
- If port 8080 is occupied, start `./sandboxd -addr 127.0.0.1:8081` and set
  `SANDBOX_DOMAIN=http://127.0.0.1:8081` in the client terminal.
- If authentication is enabled, use the same `SANDBOX_API_KEY` in both terminals.

## Build from source

Requires Go 1.27+, Make, and a reachable Docker daemon.

```sh
git clone https://github.com/ruhuang2001/tiny-sandbox.git
cd tiny-sandbox
make build
cd bin
./sandboxd
```

Continue with the commands in quick start step 3 in another terminal. Use the
repository's `bin` directory instead of the download directory shown there.

## Next steps

- [CLI reference](cli.md): sandbox reuse, lifecycle controls, and logs.
- [Go SDK](sdk.md): integrate execution into your application.
- [Agent tool example](../examples/agent-tool/README.md): expose Python execution as an agent tool.
