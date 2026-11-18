// main.ts — the entrypoint: the HTTP server bootstrap and the entry <-> env
// lifecycle. The server binds an ephemeral port (or $PORT when set for local
// development), and when the env is present ($MAKEFUN_ENV_URL) it registers
// its port with the env and long-polls the env's /done until the trial is
// over, then exits 0. Without an env it simply serves, which is how the
// conformance suite is driven locally.
//
// The forge itself lives in app.ts and the surface modules; this file only
// owns the socket, the request reading and the lifecycle.

import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { createForge, dispatch } from './app.ts';
import type { Reply } from './http.ts';

const MAX_BODY_BYTES = 256 * 1024 * 1024;

async function readBody(req: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let total = 0;
  for await (const chunk of req) {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk as Uint8Array);
    total += buffer.length;
    if (total > MAX_BODY_BYTES) throw new Error('request body too large');
    chunks.push(buffer);
  }
  return Buffer.concat(chunks);
}

function writeReply(res: ServerResponse, reply: Reply): void {
  const headers: Record<string, string> = { 'content-type': reply.contentType, ...(reply.headers ?? {}) };
  if (reply.status !== 204) headers['content-length'] = String(reply.body.length);
  res.writeHead(reply.status, headers);
  res.end(reply.body);
}

function decodePath(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

async function main(): Promise<void> {
  const envUrl = process.env.MAKEFUN_ENV_URL ?? '';
  const expectedToken = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  const forge = createForge();

  const server = createServer((req, res) => {
    void (async () => {
      try {
        const body = req.method === 'GET' || req.method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
        const url = new URL(req.url ?? '/', 'http://localhost');
        const authorization = req.headers.authorization ?? '';
        const reply = dispatch(forge, {
          method: req.method ?? 'GET',
          path: decodePath(url.pathname),
          query: url.searchParams,
          origin: `http://${req.headers.host ?? 'localhost'}`,
          headers: req.headers as Record<string, string | undefined>,
          body,
          authorized: expectedToken !== '' && authorization === `Bearer ${expectedToken}`,
        });
        writeReply(res, reply);
      } catch {
        if (!res.headersSent) {
          res.writeHead(500, { 'content-type': 'application/json; charset=utf-8' });
        }
        res.end(JSON.stringify({ message: 'Internal Server Error' }));
      }
    })();
  });
  server.keepAliveTimeout = 75_000;
  server.headersTimeout = 80_000;

  const requestedPort = Number(process.env.PORT ?? '0');
  let port = Number.isFinite(requestedPort) && requestedPort > 0 ? Math.trunc(requestedPort) : 0;
  if (envUrl !== '' && port === 8080) port = 0; // the env owns 8080

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, () => resolve());
  });
  const address = server.address();
  const boundPort = typeof address === 'object' && address !== null ? address.port : port;
  process.stdout.write(`vinz-forge listening on ${boundPort}\n`);

  if (envUrl === '') return; // local development: serve until killed

  // Register with the env: the first register wins; a 409 means this process
  // lost a race and simply keeps waiting like any other.
  const registerUrl = `${envUrl.replace(/\/+$/, '')}/register`;
  let registered = false;
  for (let attempt = 0; attempt < 50 && !registered; attempt += 1) {
    try {
      const response = await fetch(registerUrl, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port: boundPort }),
      });
      await response.text().catch(() => '');
      if (response.status === 200 || response.status === 409) registered = true;
      else if (response.status === 400 || response.status === 405) break;
    } catch {
      // the env may not be up yet; keep trying inside the boot budget
    }
    if (!registered) await new Promise((resolve) => setTimeout(resolve, 200));
  }

  // Long-poll /done until the env says the trial is over.
  const doneUrl = `${envUrl.replace(/\/+$/, '')}/done`;
  for (;;) {
    let done = false;
    try {
      const response = await fetch(doneUrl);
      const body = (await response.json().catch(() => null)) as { done?: unknown } | null;
      done = body?.done === true;
    } catch {
      done = false;
    }
    if (done) break;
    await new Promise((resolve) => setTimeout(resolve, 750));
  }
  await new Promise<void>((resolve) => server.close(() => resolve()));
  process.exit(0);
}

void main().catch((err: unknown) => {
  process.stderr.write(`fatal: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
