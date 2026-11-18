// app.ts — the HTTP dispatcher: one entry point that routes a request to the
// REST v3 surface, the GraphQL endpoint, the MCP endpoint or the git
// smart-HTTP surface, and turns whatever a surface throws into a JSON error
// reply so a bad request can never take the server down.

import { Forge } from './forge.ts';
import { ApiError } from './ops.ts';
import { handleRest } from './rest.ts';
import { handleGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';
import { handleGit } from './gitproto.ts';
import { jsonReply, messageReply, type Reply, type ReqCtx } from './http.ts';

let requestCounter = 0;

/** A unique, deterministic request id for the X-GitHub-Request-Id header. */
export function nextRequestId(): string {
  requestCounter += 1;
  return `vinz-${requestCounter.toString(36)}-${((requestCounter * 2654435761) % 0xffffffff).toString(16)}`;
}

const GIT_PATH = /^\/([^/]+)\/([^/]+)\.git\/(info\/refs|git-upload-pack|git-receive-pack)$/;

/** Route one request. Never throws: an internal error is a 500 JSON reply. */
export function dispatch(forge: Forge, ctx: Omit<ReqCtx, 'forge'>): Reply {
  const full: ReqCtx = { ...ctx, forge };
  try {
    let reply: Reply;
    if (full.path === '/healthz') {
      reply = jsonReply(200, { ok: true });
    } else if (full.path === '/api/v3' || full.path.startsWith('/api/v3/')) {
      reply = handleRest(full);
    } else if (full.path === '/api/graphql') {
      reply = handleGraphQL(full);
    } else if (full.path === '/mcp') {
      reply = handleMcp(full);
    } else {
      const git = GIT_PATH.exec(full.path);
      if (git) {
        reply = handleGit(full, decodeURIComponent(git[1]!), decodeURIComponent(git[2]!), git[3]!);
      } else {
        reply = messageReply(404, 'Not Found');
      }
    }
    if (full.path.startsWith('/api/')) {
      reply = { ...reply, headers: { ...(reply.headers ?? {}), 'x-github-request-id': nextRequestId() } };
    }
    return reply;
  } catch (err) {
    if (err instanceof ApiError) return messageReply(err.status, err.message);
    return messageReply(500, 'Internal Server Error');
  }
}

/** A fresh forge instance (one per process). */
export function createForge(): Forge {
  return new Forge();
}
