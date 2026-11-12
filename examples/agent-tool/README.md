# Agent tool example: run_python

This directory demonstrates how an AI agent can use tiny-sandbox to execute
Python. It is an **example tool adapter built on the Go SDK**: the model generates
tool arguments, your application calls the adapter, and your application sends
the result back to the model.

## How the components fit together

| Layer | Responsibility |
| --- | --- |
| AI model | Read tool definitions, generate code and tool calls, interpret results |
| Your agent application | Call the model, dispatch tools, associate call IDs, return results, control the loop |
| This adapter | Expose Python execution through ToolDefinition, Tool.Run, and ResultJSON |
| [Go SDK](../../docs/sdk.md) | Manage sandboxes, execute commands, collect output, and handle errors over HTTP |
| sandboxd | Create containers and execute programs through the Docker backend |

    User task -> model generates a run_python call
                             |
                      Your agent application
                             |
                 agenttool.Tool.Run(ctx, input)
                             |
                   Go SDK -> sandboxd -> Docker / Python
                             |
                  Result -> tool-result message -> model continues

Your application calls the SDK; the model does not call Go methods directly.
This example does not include a model API client, a conversation loop, or an MCP
server, and it does not register itself with a chat client.

Use the [sandboxctl CLI](../../docs/cli.md) to run code manually. Use the SDK
directly to manage sandboxes, files, or other commands. Use this adapter as a
starting point when exposing a run_python tool to an agent.

## Try the simulated demo without Docker

Run these commands from the repository root with Go 1.27 or newer:

    # Print the tool name, description, and input schema.
    go run ./examples/agent-tool/cmd/demo -definition

    # Unset the domain to select the simulated backend.
    env -u SANDBOX_DOMAIN go run ./examples/agent-tool/cmd/demo -code 'print(2 ** 10)'

The second command returns is_error=false, outcome=exited, exit_code=0, and stdout
containing 1024 followed by a newline. A message on stderr identifies the
in-memory demo server.

**This mode does not run Python.** Its stub recognizes only the exact string
print(2 ** 10). All other code, including equivalent expressions with different
spacing, returns a predefined nonzero_exit result. It demonstrates the tool
protocol and result format; it cannot validate Python programs.

This command demonstrates a simulated failure; a nonzero exit status is expected:

    env -u SANDBOX_DOMAIN go run ./examples/agent-tool/cmd/demo -code 'print(1)'

Do not insert -- between cmd/demo and its flags. Go's standard flag package
treats -- as the end of flag parsing, so subsequent flags would be ignored.

## Execute real Python with sandboxd

Start a Docker daemon, then start the service in one terminal:

    go run ./cmd/sandboxd

The default listen address is 127.0.0.1:8080 and the default Docker socket is
/var/run/docker.sock. For a different Unix socket, set
DOCKER_HOST=unix:///path/to/docker.sock or pass -docker /path/to/docker.sock
to sandboxd.

In a second terminal, run arbitrary Python:

    SANDBOX_DOMAIN=http://127.0.0.1:8080 go run ./examples/agent-tool/cmd/demo -timeout 60 -code 'print(sum([10, 20, 30]))'

On success, stdout contains 60 followed by a newline. If the image is missing,
the Docker backend pulls python:3.12-alpine, which requires registry access.
If the server uses SANDBOX_API_KEY, configure the client with the same value.

Each Tool.Run call creates a fresh sandbox:

- The image is fixed to python:3.12-alpine, with a 256 MB memory limit.
- The default sandbox lifetime is 60 seconds. Set timeout_seconds in tool
  arguments, or -timeout in the demo, to change it. This is the sandbox lifetime,
  not a separate Python CPU time limit.
- The adapter runs python3 -c with the supplied code and collects a final result
  without streaming output to its caller.
- It requests cleanup after execution. Even if the caller's context is canceled,
  cleanup uses an independent context with a 10-second timeout. The example
  currently ignores cleanup request errors.

## Integrate with your agent application

Import github.com/ruhuang2001/tiny-sandbox/examples/agent-tool as agenttool.
The SDK package is github.com/ruhuang2001/tiny-sandbox/sandbox-go.

1. Configure the service address and authentication with sandbox.NewClient,
   then create the tool with agenttool.New(client).
2. Call agenttool.ToolDefinition() to obtain the name, description, and input
   schema. Adapt them to your model API or framework's tool declaration format;
   the input_schema wrapper is not universal across frameworks.
3. Include the declaration in the model request. The model can then request a
   run_python call with arguments such as the JSON below.
4. Check the tool name, decode its JSON arguments into agenttool.Input, handle
   decoding errors, validate the arguments, and call tool.Run(ctx, input).
   A schema declaration is not runtime validation: the schema requires a timeout
   of at least 1, but Run treats nonpositive values as the default of 60 seconds.
5. Serialize with agenttool.ResultJSON(result) and handle its returned error.
   Wrap the body in your model API's tool-result message and associate it with
   the original call ID. Use result.IsError where the framework supports it.
6. Add the result to the conversation and call the model again. Your application
   controls call limits, timeouts, and retry limits until the task is complete.

Example arguments for real execution (not the fixed simulated backend):

    {
      "code": "print(sum([10, 20, 30]) / 3)",
      "timeout_seconds": 60
    }

This is only the argument object, without model-specific call IDs or message
envelopes. Likewise, ResultJSON produces the result body; it does not send a
model request.

## Interpret execution outcomes

Result carries stdout, stderr, an exit_code when available, outcome, is_error,
and optional message and hint fields. When output_truncated is true, retained
output is incomplete. Optional fields, including empty output, may be omitted
from JSON.

| Outcome | Meaning | Possible next step |
| --- | --- | --- |
| exited | Normal exit with status 0 | Use the output or continue the task |
| nonzero_exit | Nonzero exit status | Read stderr and correct the code |
| oom | Killed for exceeding the memory limit | Reduce memory use or process data in chunks |
| timeout | A deadline expired: the sandbox lifetime, or the execution's own timeout | Optimize code or adjust the lifetime or execution timeout |
| canceled | Execution was canceled | Let the application decide whether to continue |
| evicted | Container was stopped externally | Check the environment and recreate the sandbox if appropriate |
| invalid_input | Code is empty | Supply code |
| unavailable | Busy sandbox or another service / SDK error not mapped separately | Check service, configuration, and authentication; apply the application's retry policy |

Branch on outcome instead of guessing the cause from an error message.
A hint is advisory; it does not trigger automatic retries. `ErrCancelUnconfirmed`
is currently mapped to `unavailable`, and the serialized result does not retain
the SDK's execution IDs or idempotency key. If your application needs to reconcile
uncertain executions, retain that context at the SDK layer and verify cleanup
before retrying work that may have side effects.

## Reuse files across calls

Run creates a new sandbox for each call, so files from previous calls are not
retained. To reuse files, create a sandbox containing Python through the SDK,
call tool.RunIn(ctx, sandboxID, input), and clean up the sandbox when finished.

RunIn reuses the container and filesystem, **not a persistent Python
interpreter**. Each call starts a new python3 -c process. Variables and imported
module objects do not survive; use files to pass data. RunIn neither creates
nor cleans up the sandbox, and does not apply Input.TimeoutSeconds to its
lifetime. Set the lifetime when creating the sandbox.

## File guide

- [tool.go](tool.go): tool definition, execution adapter, error mapping, and serialization.
- [cmd/demo/main.go](cmd/demo/main.go): CLI demo selecting a real service or fixed stub.
- [tool_test.go](tool_test.go): checks using the real HTTP handler and an in-memory
  backend; these do not execute real Docker containers or Python.
- [Go SDK contract](../../docs/sdk.md) and [HTTP API contract](../../docs/api.md):
  reference documentation for extending the integration.
