# @tiny-sandbox/sdk

TypeScript SDK for tiny-sandbox: lifecycle, binary files, authenticated SSE,
bounded retries, idempotent starts and confirmed AbortSignal cancellation.
ESM, Node 22+ or modern browsers, no runtime dependencies.

This package is built from the repository; npm publication is a separate step.
Run npm ci and npm pack here, then install the emitted .tgz in your application.

~~~ts
import { Client, SandboxError } from '@tiny-sandbox/sdk';

const client = new Client({ domain: 'http://localhost:8080' });
const sandbox = await client.createSandbox({ timeout_seconds: 300 });
try {
  const execution = await client.startExecution(sandbox.id, {
    command: ['sh', '-c', 'echo hello; sleep 2; echo done'],
  });
  for await (const event of client.streamExecution(sandbox.id, execution.id)) {
    if (event.type === 'stdout' || event.type === 'stderr') console.log(event.data);
    if (event.type === 'complete') console.log(event.execution.termination_reason);
  }
  const result = await client.exec(sandbox.id, ['sh', '-c', 'exit 42']);
  if (result.reason === 'exited') console.log(result.execution.exit_code);
} catch (error) {
  if (error instanceof SandboxError) console.error(error.code, error.execution);
  else throw error;
} finally {
  await client.killSandbox(sandbox.id);
}
~~~

Pass { signal: controller.signal } as the final method argument to abort.
Aborting streams cancels remote work with fresh five-second recovery; breaking
iteration or throwing from callbacks only closes the stream. Inspect
cancel_unconfirmed errors before assuming work stopped. Completed nonzero
commands resolve outcomes; transport/API/cancellation failures reject.

See the repository's docs/sdk-typescript.md for the complete method, error,
retry and compatibility contract. Browser usage requires same-origin hosting
or a CORS-configured proxy; sandboxd does not itself enable CORS. Do not bundle
privileged bearer credentials.

From the repository root, run bash scripts/e2e-typescript.sh for real Docker
verification with evidence under artifacts/typescript-e2e.
