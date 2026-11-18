// mcp.ts — the Model Context Protocol surface at POST /mcp (JSON-RPC 2.0 over
// streamable HTTP). It speaks both protocol eras the suite accepts: modern
// `server/discover` (revision 2026-07-28, no handshake) and the legacy
// `initialize` + `notifications/initialized` handshake. The tools act on the
// same store as REST and GraphQL, so an object made over MCP is the same
// object everywhere. The tools/list answer is built once and cached, so two
// calls return byte-identical lists.

import { VIEWER, tickEpochSec, type IssueRec, type PullRec, type RepoRec } from './forge.ts';
import { ApiError, openPull, squashMerge } from './ops.ts';
import { isRecord, jsonReply, type Reply, type ReqCtx } from './http.ts';

const SERVER_INFO = { name: 'vinz-forge', version: '1.0.0' };
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

class ToolError extends Error {}

interface ToolDef {
  readonly name: string;
  readonly description: string;
  readonly properties: Readonly<Record<string, { type: string; description: string }>>;
  readonly required: readonly string[];
  readonly run: (ctx: ReqCtx, args: Record<string, unknown>) => unknown;
}

function repoSummary(repo: RepoRec) {
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `/${repo.owner}/${repo.name}`,
  };
}

function issueSummary(repo: RepoRec, issue: IssueRec) {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name })),
    html_url: `/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function pullSummary(repo: RepoRec, pull: PullRec) {
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: repo.refs.get(`refs/heads/${pull.headRef}`) ?? '' },
    base: { ref: pull.baseRef, sha: repo.refs.get(`refs/heads/${pull.baseRef}`) ?? '' },
    html_url: `/${repo.owner}/${repo.name}/pull/${pull.number}`,
  };
}

function needRepo(ctx: ReqCtx, args: Record<string, unknown>): RepoRec {
  const owner = typeof args.owner === 'string' ? args.owner : '';
  const name = typeof args.repo === 'string' ? args.repo : '';
  const repo = ctx.forge.getRepo(owner, name);
  if (!repo) throw new ToolError(`repository ${owner}/${name} not found`);
  return repo;
}

function needString(args: Record<string, unknown>, field: string): string {
  const value = args[field];
  if (typeof value !== 'string' || value === '') throw new ToolError(`missing required argument: ${field}`);
  return value;
}

const TOOLS: readonly ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    properties: {
      name: { type: 'string', description: 'The repository name.' },
      description: { type: 'string', description: 'A short description of the repository.' },
      private: { type: 'boolean', description: 'Whether the repository is private.' },
    },
    required: ['name'],
    run: (ctx, args) => {
      const name = needString(args, 'name');
      const existing = ctx.forge.getRepo(VIEWER.login, name);
      if (existing) return repoSummary(existing);
      const repo = ctx.forge.createRepo(name, {
        isPrivate: args.private === true,
        description: typeof args.description === 'string' ? args.description : '',
      });
      return repoSummary(repo);
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
    },
    required: ['owner', 'repo'],
    run: (ctx, args) => repoSummary(needRepo(ctx, args)),
  },
  {
    name: 'create_issue',
    description: 'Open a new issue in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The issue title.' },
      body: { type: 'string', description: 'The issue body.' },
    },
    required: ['owner', 'repo', 'title'],
    run: (ctx, args) => {
      const repo = needRepo(ctx, args);
      const title = needString(args, 'title');
      const stamp = tickEpochSec();
      const issue: IssueRec = {
        id: ctx.forge.allocObjectId(),
        number: repo.nextNumber++,
        title,
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open',
        labels: [],
        createdEpoch: stamp,
        updatedEpoch: stamp,
        comments: [],
      };
      repo.issues.set(issue.number, issue);
      return issueSummary(repo, issue);
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state (open, closed or all).',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', description: 'open, closed or all (default open).' },
    },
    required: ['owner', 'repo'],
    run: (ctx, args) => {
      const repo = needRepo(ctx, args);
      const state = typeof args.state === 'string' ? args.state : 'open';
      const issues = [...repo.issues.values()]
        .filter((issue) => state === 'all' || issue.state === state)
        .sort((a, b) => a.number - b.number);
      return issues.map((issue) => issueSummary(repo, issue));
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The pull request title.' },
      body: { type: 'string', description: 'The pull request body.' },
      head: { type: 'string', description: 'The head branch name.' },
      base: { type: 'string', description: 'The base branch name.' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
    run: (ctx, args) => {
      const repo = needRepo(ctx, args);
      try {
        const pull = openPull(ctx.forge, repo, {
          title: needString(args, 'title'),
          body: typeof args.body === 'string' ? args.body : '',
          head: needString(args, 'head'),
          base: needString(args, 'base'),
        });
        return pullSummary(repo, pull);
      } catch (err) {
        if (err instanceof ApiError) throw new ToolError(err.message);
        throw err;
      }
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge an open pull request into its base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      pull_number: { type: 'number', description: 'The pull request number.' },
    },
    required: ['owner', 'repo', 'pull_number'],
    run: (ctx, args) => {
      const repo = needRepo(ctx, args);
      const number = typeof args.pull_number === 'number' ? args.pull_number : Number(args.pull_number);
      const pull = repo.pulls.get(number);
      if (!pull) throw new ToolError(`pull request #${number} not found`);
      try {
        squashMerge(repo, pull);
      } catch (err) {
        if (err instanceof ApiError) throw new ToolError(err.message);
        throw err;
      }
      return pullSummary(repo, pull);
    },
  },
];

/** The tools/list result, built once: byte-identical across calls. */
const TOOLS_LIST_RESULT = {
  tools: TOOLS.map((tool) => ({
    name: tool.name,
    description: tool.description,
    inputSchema: {
      type: 'object',
      properties: tool.properties,
      required: [...tool.required],
      additionalProperties: false,
    },
  })),
};

// -------------------------------------------------------------------------------------------
// JSON-RPC

interface RpcOutcome {
  readonly status: number;
  readonly payload: unknown;
}

function rpcResult(id: unknown, result: unknown): RpcOutcome {
  return { status: 200, payload: { jsonrpc: '2.0', id, result } };
}

function rpcError(id: unknown, code: number, message: string, status = 200): RpcOutcome {
  return { status, payload: { jsonrpc: '2.0', id, error: { code, message } } };
}

function callTool(ctx: ReqCtx, params: Record<string, unknown>): unknown {
  const name = typeof params.name === 'string' ? params.name : '';
  const tool = TOOLS.find((candidate) => candidate.name === name);
  if (!tool) return { __rpcError: { code: -32602, message: `unknown tool: ${name || '(none)'}` } };
  const args = isRecord(params.arguments) ? params.arguments : {};
  for (const required of tool.required) {
    const value = args[required];
    if (value === undefined || value === null || value === '') {
      return {
        content: [{ type: 'text', text: `missing required argument: ${required}` }],
        isError: true,
      };
    }
  }
  try {
    const payload = tool.run(ctx, args);
    return {
      content: [{ type: 'text', text: JSON.stringify(payload) }],
      structuredContent: payload,
      isError: false,
    };
  } catch (err) {
    if (err instanceof ToolError) {
      return { content: [{ type: 'text', text: err.message }], isError: true };
    }
    throw err;
  }
}

function dispatch(ctx: ReqCtx, request: Record<string, unknown>): RpcOutcome | null {
  const id = 'id' in request ? request.id : null;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = isRecord(request.params) ? request.params : {};
  const isNotification = !('id' in request);
  switch (method) {
    case 'server/discover':
      return rpcResult(id, {
        resultType: 'complete',
        supportedVersions: SUPPORTED_VERSIONS,
        capabilities: { tools: {} },
        serverInfo: SERVER_INFO,
        _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
      });
    case 'initialize': {
      const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      const protocolVersion = SUPPORTED_VERSIONS.includes(offered) ? offered : '2025-11-25';
      return rpcResult(id, { protocolVersion, capabilities: { tools: {} }, serverInfo: SERVER_INFO });
    }
    case 'ping':
      return rpcResult(id, {});
    case 'tools/list':
      return rpcResult(id, TOOLS_LIST_RESULT);
    case 'tools/call': {
      const result = callTool(ctx, params);
      if (isRecord(result) && '__rpcError' in result) {
        const error = (result as { __rpcError: { code: number; message: string } }).__rpcError;
        return rpcError(id, error.code, error.message);
      }
      return rpcResult(id, result);
    }
    default:
      if (isNotification) return null;
      return rpcError(id, -32601, `method not found: ${method}`);
  }
}

export function handleMcp(ctx: ReqCtx): Reply {
  if (ctx.method === 'DELETE') return jsonReply(200, {});
  if (ctx.method !== 'POST') return jsonReply(405, { jsonrpc: '2.0', id: null, error: { code: -32601, message: 'method not allowed' } });
  if (!ctx.authorized) {
    return jsonReply(401, { jsonrpc: '2.0', id: null, error: { code: -32001, message: 'unauthorized' } });
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    return jsonReply(400, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } });
  }
  if (Array.isArray(parsed)) {
    const outcomes: unknown[] = [];
    for (const entry of parsed) {
      if (!isRecord(entry)) continue;
      const outcome = dispatch(ctx, entry);
      if (outcome) outcomes.push(outcome.payload);
    }
    return jsonReply(200, outcomes);
  }
  if (!isRecord(parsed)) return jsonReply(400, { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } });
  const outcome = dispatch(ctx, parsed);
  if (outcome === null) {
    // A notification: accepted, no body.
    return { status: 202, contentType: 'application/json; charset=utf-8', body: Buffer.alloc(0) };
  }
  return jsonReply(outcome.status, outcome.payload);
}
