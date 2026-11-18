// http.ts — the shared HTTP plumbing types: what a request looks like once
// main.ts has read it, and what a surface hands back as a reply.

import type { Forge } from './forge.ts';

export interface ReqCtx {
  readonly forge: Forge;
  readonly method: string;
  /** The decoded URL pathname, e.g. /api/v3/repos/adaiversen/demo/issues. */
  readonly path: string;
  readonly query: URLSearchParams;
  /** `http://<host>` of this very request, for absolute URLs in replies. */
  readonly origin: string;
  readonly headers: Readonly<Record<string, string | undefined>>;
  readonly body: Buffer;
  /** True when an `Authorization: Bearer <token>` header names the forge token. */
  readonly authorized: boolean;
}

export interface Reply {
  readonly status: number;
  readonly contentType: string;
  readonly body: Buffer;
  readonly headers?: Readonly<Record<string, string>>;
}

export function jsonReply(status: number, value: unknown, headers?: Record<string, string>): Reply {
  return {
    status,
    contentType: 'application/json; charset=utf-8',
    body: Buffer.from(JSON.stringify(value), 'utf8'),
    headers,
  };
}

export function messageReply(status: number, message: string): Reply {
  return jsonReply(status, { message });
}

/** Parse the request body as JSON; null when it is empty or not an object/array. */
export function bodyJson(ctx: ReqCtx): unknown {
  const text = ctx.body.toString('utf8').trim();
  if (text === '') return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** The request body as a JSON object, or null when it is not one. */
export function bodyObject(ctx: ReqCtx): Record<string, unknown> | null {
  const parsed = bodyJson(ctx);
  return isRecord(parsed) ? parsed : null;
}

export function stringField(body: Record<string, unknown>, field: string): string | undefined {
  const value = body[field];
  return typeof value === 'string' ? value : undefined;
}

export function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/** Split a path into decoded segments, dropping empty ones. */
export function pathSegments(path: string): string[] {
  return path.split('/').filter((segment) => segment !== '').map(decodeSegment);
}
