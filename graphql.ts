// graphql.ts — the GraphQL v4 surface at POST /api/graphql: a small
// tokenizer, parser and resolver for the documents gh and github-mcp-server
// send (viewer, repository reads, issue/pull connections, the create/close/
// merge/comment mutations, __type introspection, node(id:) and operation
// batching). It resolves against the same store as REST, so ids and objects
// agree across surfaces. Global ids are opaque, stable encodings of the
// object's kind and key — never the plain database id.

import { VIEWER, tickEpochSec, type IssueRec, type PullRec, type RepoRec } from './forge.ts';
import { ApiError, openPull, pullAnalysis, squashMerge } from './ops.ts';
import { isRecord, jsonReply, type Reply, type ReqCtx } from './http.ts';

// -------------------------------------------------------------------------------------------
// global ids

function encodeId(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from([kind, ...parts].join(':'), 'utf8').toString('base64url');
}

function decodeId(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const parts = text.split(':');
    if (parts.length < 2) return null;
    return { kind: parts[0]!, parts: parts.slice(1) };
  } catch {
    return null;
  }
}

const repoGlobalId = (repo: RepoRec): string => encodeId('repo', repo.id);
const issueGlobalId = (repo: RepoRec, issue: IssueRec): string => encodeId('issue', repo.id, issue.number);
const pullGlobalId = (repo: RepoRec, pull: PullRec): string => encodeId('pull', repo.id, pull.number);
const commentGlobalId = (repo: RepoRec, issue: IssueRec, commentId: number): string => encodeId('comment', repo.id, issue.number, commentId);

// -------------------------------------------------------------------------------------------
// tokenizer

type Token =
  | { readonly kind: 'name'; readonly value: string }
  | { readonly kind: 'string'; readonly value: string }
  | { readonly kind: 'int'; readonly value: number }
  | { readonly kind: 'float'; readonly value: number }
  | { readonly kind: 'punct'; readonly value: string }
  | { readonly kind: 'eof' };

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let index = 0;
  const isNameStart = (ch: string) => /[A-Za-z_]/.test(ch);
  const isNameChar = (ch: string) => /[A-Za-z0-9_]/.test(ch);
  while (index < source.length) {
    const ch = source[index]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '\ufeff') {
      index += 1;
      continue;
    }
    if (ch === '#') {
      while (index < source.length && source[index] !== '\n') index += 1;
      continue;
    }
    if (source.startsWith('...', index)) {
      tokens.push({ kind: 'punct', value: '...' });
      index += 3;
      continue;
    }
    if ('{}()[]:=@$!|&'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      index += 1;
      continue;
    }
    if (ch === '"') {
      if (source.startsWith('"""', index)) {
        const end = source.indexOf('"""', index + 3);
        const value = source.slice(index + 3, end < 0 ? source.length : end);
        tokens.push({ kind: 'string', value });
        index = end < 0 ? source.length : end + 3;
        continue;
      }
      let value = '';
      index += 1;
      while (index < source.length && source[index] !== '"') {
        if (source[index] === '\\') {
          const esc = source[index + 1]!;
          const escapes: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
          if (esc === 'u') {
            value += String.fromCharCode(Number.parseInt(source.slice(index + 2, index + 6), 16));
            index += 6;
          } else {
            value += escapes[esc] ?? esc;
            index += 2;
          }
        } else {
          value += source[index];
          index += 1;
        }
      }
      index += 1;
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (/[0-9-]/.test(ch)) {
      let text = '';
      while (index < source.length && /[0-9.eE+-]/.test(source[index]!)) {
        text += source[index];
        index += 1;
      }
      if (text.includes('.') || text.includes('e') || text.includes('E')) tokens.push({ kind: 'float', value: Number(text) });
      else tokens.push({ kind: 'int', value: Number.parseInt(text, 10) });
      continue;
    }
    if (isNameStart(ch)) {
      let name = '';
      while (index < source.length && isNameChar(source[index]!)) {
        name += source[index];
        index += 1;
      }
      tokens.push({ kind: 'name', value: name });
      continue;
    }
    throw new Error(`unexpected character ${JSON.stringify(ch)} in the document`);
  }
  tokens.push({ kind: 'eof' });
  return tokens;
}

// -------------------------------------------------------------------------------------------
// parser

type Value =
  | { readonly kind: 'variable'; readonly name: string }
  | { readonly kind: 'string'; readonly value: string }
  | { readonly kind: 'int'; readonly value: number }
  | { readonly kind: 'float'; readonly value: number }
  | { readonly kind: 'boolean'; readonly value: boolean }
  | { readonly kind: 'null' }
  | { readonly kind: 'enum'; readonly value: string }
  | { readonly kind: 'list'; readonly values: readonly Value[] }
  | { readonly kind: 'object'; readonly fields: ReadonlyArray<{ name: string; value: Value }> };

interface FieldNode {
  readonly kind: 'field';
  readonly alias: string | null;
  readonly name: string;
  readonly args: ReadonlyArray<{ name: string; value: Value }>;
  readonly selectionSet: readonly SelectionNode[] | null;
}

type SelectionNode =
  | FieldNode
  | { readonly kind: 'inline'; readonly typeCondition: string | null; readonly selectionSet: readonly SelectionNode[] }
  | { readonly kind: 'spread'; readonly name: string };

interface OperationNode {
  readonly type: 'query' | 'mutation';
  readonly name: string | null;
  readonly selectionSet: readonly SelectionNode[];
}

interface DocumentNode {
  readonly operations: readonly OperationNode[];
  readonly fragments: ReadonlyMap<string, { typeCondition: string | null; selectionSet: readonly SelectionNode[] }>;
}

class Parser {
  private tokens: Token[];
  private pos = 0;

  constructor(source: string) {
    this.tokens = tokenize(source);
  }

  private peek(): Token {
    return this.tokens[this.pos]!;
  }

  private next(): Token {
    const token = this.tokens[this.pos]!;
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`expected "${value}" in the document`);
  }

  private skipPunct(value: string): boolean {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === value) {
      this.pos += 1;
      return true;
    }
    return false;
  }

  private expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error('expected a name in the document');
    return token.value;
  }

  parseDocument(): DocumentNode {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, { typeCondition: string | null; selectionSet: readonly SelectionNode[] }>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ type: 'query', name: null, selectionSet: this.parseSelectionSet() });
        continue;
      }
      if (token.kind !== 'name') throw new Error('expected an operation in the document');
      if (token.value === 'query' || token.value === 'mutation') {
        const type = token.value;
        this.next();
        let name: string | null = null;
        if (this.peek().kind === 'name') name = this.expectName();
        if (this.peek().kind === 'punct' && this.peek().value === '(') this.parseVariableDefinitions();
        this.skipDirectives();
        operations.push({ type, name, selectionSet: this.parseSelectionSet() });
        continue;
      }
      if (token.value === 'fragment') {
        this.next();
        const fragName = this.expectName();
        const onToken = this.next();
        if (onToken.kind !== 'name' || onToken.value !== 'on') throw new Error('expected "on" in a fragment definition');
        const typeCondition = this.expectName();
        this.skipDirectives();
        fragments.set(fragName, { typeCondition, selectionSet: this.parseSelectionSet() });
        continue;
      }
      throw new Error(`unexpected definition ${token.value} in the document`);
    }
    return { operations, fragments };
  }

  private parseVariableDefinitions(): void {
    this.expectPunct('(');
    while (!this.skipPunct(')')) {
      this.expectPunct('$');
      this.expectName();
      this.expectPunct(':');
      this.parseType();
      if (this.skipPunct('=')) this.parseValue();
      this.skipDirectives();
    }
  }

  private parseType(): void {
    if (this.skipPunct('[')) {
      this.parseType();
      this.expectPunct(']');
    } else {
      this.expectName();
    }
    this.skipPunct('!');
  }

  private skipDirectives(): void {
    while (this.skipPunct('@')) {
      this.expectName();
      if (this.peek().kind === 'punct' && this.peek().value === '(') {
        this.expectPunct('(');
        while (!this.skipPunct(')')) {
          this.expectName();
          this.expectPunct(':');
          this.parseValue();
        }
      }
    }
  }

  private parseSelectionSet(): readonly SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (!this.skipPunct('}')) {
      selections.push(this.parseSelection());
    }
    return selections;
  }

  private parseSelection(): SelectionNode {
    if (this.skipPunct('...')) {
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'on') {
        this.next();
        const typeCondition = this.expectName();
        this.skipDirectives();
        return { kind: 'inline', typeCondition, selectionSet: this.parseSelectionSet() };
      }
      const name = this.expectName();
      this.skipDirectives();
      return { kind: 'spread', name };
    }
    let name = this.expectName();
    let alias: string | null = null;
    if (this.skipPunct(':')) {
      alias = name;
      name = this.expectName();
    }
    const args: Array<{ name: string; value: Value }> = [];
    if (this.skipPunct('(')) {
      while (!this.skipPunct(')')) {
        const argName = this.expectName();
        this.expectPunct(':');
        args.push({ name: argName, value: this.parseValue() });
      }
    }
    this.skipDirectives();
    let selectionSet: readonly SelectionNode[] | null = null;
    if (this.peek().kind === 'punct' && this.peek().value === '{') selectionSet = this.parseSelectionSet();
    return { kind: 'field', alias, name, args, selectionSet };
  }

  private parseValue(): Value {
    const token = this.next();
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'int') return { kind: 'int', value: token.value };
    if (token.kind === 'float') return { kind: 'float', value: token.value };
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    if (token.kind === 'punct' && token.value === '$') return { kind: 'variable', name: this.expectName() };
    if (token.kind === 'punct' && token.value === '[') {
      const values: Value[] = [];
      while (!this.skipPunct(']')) values.push(this.parseValue());
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: Array<{ name: string; value: Value }> = [];
      while (!this.skipPunct('}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue() });
      }
      return { kind: 'object', fields };
    }
    throw new Error('unexpected value in the document');
  }
}

// -------------------------------------------------------------------------------------------
// schema (the field lists introspection reports, and validation enforces)

const OBJECT_FIELDS: Readonly<Record<string, readonly string[]>> = {
  Query: ['viewer', 'repository', 'node'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  User: ['login', 'id', 'name', 'email', 'url', 'databaseId'],
  Repository: [
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequest', 'pullRequests', 'isPrivate', 'description',
    'url', 'createdAt', 'updatedAt',
  ],
  BranchRef: ['name', 'prefix', 'target'],
  Issue: ['id', 'number', 'title', 'state', 'body', 'url', 'createdAt', 'updatedAt', 'author'],
  IssueConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  IssueEdge: ['node', 'cursor'],
  PullRequest: [
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergeable', 'headRefName', 'baseRefName',
    'files', 'url', 'createdAt', 'updatedAt',
  ],
  PullRequestConnection: ['nodes', 'edges', 'pageInfo', 'totalCount'],
  PullRequestEdge: ['node', 'cursor'],
  PullRequestChangedFile: ['path', 'additions', 'deletions', 'changeType'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Comment: ['id', 'body', 'createdAt', 'author'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'subject', 'clientMutationId'],
  CommentEdge: ['node', 'cursor'],
  __Type: ['name', 'kind', 'description', 'fields', 'enumValues', 'possibleTypes', 'inputFields', 'ofType'],
  __Field: ['name', 'description', 'type', 'isDeprecated', 'deprecationReason', 'args'],
  __EnumValue: ['name', 'description', 'isDeprecated', 'deprecationReason'],
  __Schema: ['types', 'queryType', 'mutationType', 'directives'],
};

const ENUM_VALUES: Readonly<Record<string, readonly string[]>> = {
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'PULL_REQUEST', 'COMMIT', 'CODE', 'WIKI', 'PROJECT', 'REGISTRY_PACKAGE', 'TOPIC', 'MARKETPLACE'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  OrderDirection: ['ASC', 'DESC'],
  State: ['OPEN', 'CLOSED'],
};

// -------------------------------------------------------------------------------------------
// execution

interface ExecCtx {
  readonly forge: ReqCtx['forge'];
  readonly variables: Readonly<Record<string, unknown>>;
  readonly fragments: DocumentNode['fragments'];
  readonly errors: Array<{ message: string }>;
}

type Source =
  | { readonly type: 'Query' }
  | { readonly type: 'Mutation' }
  | { readonly type: 'User' }
  | { readonly type: 'Repository'; readonly repo: RepoRec }
  | { readonly type: 'BranchRef'; readonly name: string }
  | { readonly type: 'Issue'; readonly repo: RepoRec; readonly issue: IssueRec }
  | { readonly type: 'PullRequest'; readonly repo: RepoRec; readonly pull: PullRec }
  | { readonly type: 'Comment'; readonly id: string; readonly body: string }
  | { readonly type: 'IssueConnection'; readonly repo: RepoRec; readonly issues: readonly IssueRec[]; readonly pageInfo: PageInfoValue }
  | { readonly type: 'PullRequestConnection'; readonly repo: RepoRec; readonly pulls: readonly PullRec[]; readonly pageInfo: PageInfoValue }
  | { readonly type: 'PullRequestChangedFile'; readonly path: string; readonly additions: number; readonly deletions: number; readonly changeType: string }
  | { readonly type: 'FileConnection'; readonly files: readonly Source[]; readonly totalCount: number }
  | { readonly type: 'PageInfo'; readonly info: PageInfoValue }
  | { readonly type: '__Type'; readonly name: string }
  | { readonly type: 'Payload'; readonly payloadType: string; readonly values: Readonly<Record<string, Source | null>> }
  | { readonly type: 'CommentEdge'; readonly node: Source }
  | { readonly type: 'Raw'; readonly value: unknown };

interface PageInfoValue {
  hasNextPage: boolean;
  hasPreviousPage: boolean;
  startCursor: string | null;
  endCursor: string | null;
}

function valueOf(value: Value, ctx: ExecCtx): unknown {
  switch (value.kind) {
    case 'variable':
      return ctx.variables[value.name] ?? null;
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return value.value;
    case 'null':
      return null;
    case 'enum':
      return value.value;
    case 'list':
      return value.values.map((entry) => valueOf(entry, ctx));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of value.fields) out[field.name] = valueOf(field.value, ctx);
      return out;
    }
  }
}

function argsOf(field: FieldNode, ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.args) out[arg.name] = valueOf(arg.value, ctx);
  return out;
}

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64url');
}

function numberFromCursor(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(text);
    return match ? Number(match[1]) : null;
  } catch {
    return null;
  }
}

interface ConnectionPage<T> {
  items: T[];
  pageInfo: PageInfoValue;
}

function pageOf<T>(items: readonly T[], first: unknown, after: unknown, numberOf: (item: T) => number): ConnectionPage<T> {
  let start = 0;
  if (typeof after === 'string' && after !== '') {
    const afterNumber = numberFromCursor(after);
    if (afterNumber !== null) {
      const index = items.findIndex((item) => numberOf(item) === afterNumber);
      if (index >= 0) start = index + 1;
    }
  }
  const limit = typeof first === 'number' && first > 0 ? Math.trunc(first) : items.length;
  const page = items.slice(start, start + limit);
  return {
    items: page,
    pageInfo: {
      hasNextPage: start + page.length < items.length,
      hasPreviousPage: start > 0,
      startCursor: page.length ? cursorFor(numberOf(page[0]!)) : null,
      endCursor: page.length ? cursorFor(numberOf(page[page.length - 1]!)) : null,
    },
  };
}

function pullState(pull: PullRec): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function resolveField(source: Source, field: FieldNode, ctx: ExecCtx): unknown {
  const args = argsOf(field, ctx);
  if (field.name === '__typename') return source.type === 'Raw' ? null : source.type;
  switch (source.type) {
    case 'Query':
      return resolveQuery(field, args, ctx);
    case 'Mutation':
      return resolveMutation(field, args, ctx);
    case 'User':
      switch (field.name) {
        case 'login': return VIEWER.login;
        case 'id': return encodeId('user', VIEWER.id);
        case 'databaseId': return VIEWER.id;
        case 'name': return VIEWER.name;
        case 'email': return VIEWER.email;
        case 'url': return `/${VIEWER.login}`;
        default: return unknownField(source.type, field, ctx);
      }
    case 'Repository':
      return resolveRepository(source.repo, field, args, ctx);
    case 'BranchRef':
      return field.name === 'name' ? source.name : field.name === 'prefix' ? 'refs/heads/' : null;
    case 'Issue':
      switch (field.name) {
        case 'id': return issueGlobalId(source.repo, source.issue);
        case 'number': return source.issue.number;
        case 'title': return source.issue.title;
        case 'body': return source.issue.body;
        case 'state': return source.issue.state === 'open' ? 'OPEN' : 'CLOSED';
        case 'url': return `/${source.repo.owner}/${source.repo.name}/issues/${source.issue.number}`;
        case 'createdAt': return new Date(source.issue.createdEpoch * 1000).toISOString();
        case 'updatedAt': return new Date(source.issue.updatedEpoch * 1000).toISOString();
        case 'author': return { type: 'User' } as Source;
        default: return unknownField(source.type, field, ctx);
      }
    case 'PullRequest':
      return resolvePullRequest(source, field, args, ctx);
    case 'Comment':
      switch (field.name) {
        case 'id': return source.id;
        case 'body': return source.body;
        case 'author': return { type: 'User' } as Source;
        default: return unknownField(source.type, field, ctx);
      }
    case 'IssueConnection':
      if (field.name === 'nodes') return source.issues.map((issue) => ({ type: 'Issue', repo: source.repo, issue }) as Source);
      if (field.name === 'pageInfo') return { type: 'PageInfo', info: source.pageInfo } as Source;
      if (field.name === 'totalCount') return source.issues.length;
      if (field.name === 'edges') {
        return source.issues.map((issue) => ({
          type: 'Raw',
          value: { node: { type: 'Issue', repo: source.repo, issue } as Source, cursor: cursorFor(issue.number) },
        }) as Source);
      }
      return unknownField(source.type, field, ctx);
    case 'FileConnection':
      if (field.name === 'nodes') return [...source.files];
      if (field.name === 'totalCount') return source.totalCount;
      if (field.name === 'pageInfo') {
        return { type: 'PageInfo', info: { hasNextPage: false, hasPreviousPage: false, startCursor: null, endCursor: null } } as Source;
      }
      return unknownField(source.type, field, ctx);
    case 'PullRequestConnection':
      if (field.name === 'nodes') return source.pulls.map((pull) => ({ type: 'PullRequest', repo: source.repo, pull }) as Source);
      if (field.name === 'pageInfo') return { type: 'PageInfo', info: source.pageInfo } as Source;
      if (field.name === 'totalCount') return source.pulls.length;
      return unknownField(source.type, field, ctx);
    case 'PullRequestChangedFile':
      switch (field.name) {
        case 'path': return source.path;
        case 'additions': return source.additions;
        case 'deletions': return source.deletions;
        case 'changeType': return source.changeType;
        default: return unknownField(source.type, field, ctx);
      }
    case 'PageInfo':
      switch (field.name) {
        case 'hasNextPage': return source.info.hasNextPage;
        case 'hasPreviousPage': return source.info.hasPreviousPage;
        case 'startCursor': return source.info.startCursor;
        case 'endCursor': return source.info.endCursor;
        default: return unknownField(source.type, field, ctx);
      }
    case '__Type':
      return resolveType(source.name, field, args, ctx);
    case 'Payload': {
      const value = source.values[field.name];
      return value === undefined ? unknownField(source.payloadType, field, ctx) : value;
    }
    case 'CommentEdge':
      if (field.name === 'node') return source.node;
      if (field.name === 'cursor') return cursorFor(0);
      return unknownField(source.type, field, ctx);
    case 'Raw':
      if (isRecord(source.value) && field.name in source.value) return source.value[field.name];
      return null;
  }
}

function unknownField(type: string, field: FieldNode, ctx: ExecCtx): unknown {
  ctx.errors.push({ message: `Field '${field.name}' doesn't exist on type '${type}'` });
  return null;
}

function resolveQuery(field: FieldNode, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  switch (field.name) {
    case 'viewer':
      return { type: 'User' } as Source;
    case 'repository': {
      const owner = typeof args.owner === 'string' ? args.owner : '';
      const name = typeof args.name === 'string' ? args.name : '';
      const repo = ctx.forge.getRepo(owner, name);
      return repo ? ({ type: 'Repository', repo } as Source) : null;
    }
    case 'node': {
      const id = typeof args.id === 'string' ? args.id : '';
      return nodeById(id, ctx);
    }
    case '__type': {
      const name = typeof args.name === 'string' ? args.name : '';
      if (OBJECT_FIELDS[name] || ENUM_VALUES[name] || name === 'String' || name === 'Int' || name === 'Boolean' || name === 'ID') {
        return { type: '__Type', name } as Source;
      }
      return null;
    }
    case '__schema':
      return { type: 'Raw', value: { types: [], queryType: { name: 'Query' }, mutationType: { name: 'Mutation' }, directives: [] } } as Source;
    default:
      return unknownField('Query', field, ctx);
  }
}

function nodeById(id: string, ctx: ExecCtx): Source | null {
  const decoded = decodeId(id);
  if (!decoded) return null;
  if (decoded.kind === 'repo') {
    const repo = ctx.forge.findRepoById(Number(decoded.parts[0]));
    return repo ? { type: 'Repository', repo } : null;
  }
  if (decoded.kind === 'issue') {
    const repo = ctx.forge.findRepoById(Number(decoded.parts[0]));
    const issue = repo?.issues.get(Number(decoded.parts[1]));
    return repo && issue ? { type: 'Issue', repo, issue } : null;
  }
  if (decoded.kind === 'pull') {
    const repo = ctx.forge.findRepoById(Number(decoded.parts[0]));
    const pull = repo?.pulls.get(Number(decoded.parts[1]));
    return repo && pull ? { type: 'PullRequest', repo, pull } : null;
  }
  if (decoded.kind === 'user') return { type: 'User' };
  return null;
}

function resolveRepository(repo: RepoRec, field: FieldNode, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  switch (field.name) {
    case 'id': return repoGlobalId(repo);
    case 'databaseId': return repo.id;
    case 'name': return repo.name;
    case 'nameWithOwner': return `${repo.owner}/${repo.name}`;
    case 'owner': return { type: 'User' } as Source;
    case 'defaultBranchRef': {
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      return tip ? ({ type: 'BranchRef', name: repo.defaultBranch } as Source) : null;
    }
    case 'viewerPermission': return 'ADMIN';
    case 'hasIssuesEnabled': return true;
    case 'isPrivate': return repo.isPrivate;
    case 'description': return repo.description;
    case 'url': return `/${repo.owner}/${repo.name}`;
    case 'createdAt': return new Date(repo.createdEpoch * 1000).toISOString();
    case 'updatedAt': return new Date(repo.updatedEpoch * 1000).toISOString();
    case 'issue': {
      const issue = repo.issues.get(Number(args.number));
      return issue ? ({ type: 'Issue', repo, issue } as Source) : null;
    }
    case 'issues': {
      let issues = [...repo.issues.values()];
      const states = Array.isArray(args.states) ? args.states.map(String) : null;
      if (states) issues = issues.filter((issue) => states.includes(issue.state === 'open' ? 'OPEN' : 'CLOSED'));
      const orderBy = isRecord(args.orderBy) ? args.orderBy : null;
      const direction = orderBy && typeof orderBy.direction === 'string' ? orderBy.direction : null;
      if (direction === 'DESC') issues.sort((a, b) => b.number - a.number);
      else if (direction === 'ASC') issues.sort((a, b) => a.number - b.number);
      else issues.sort((a, b) => a.number - b.number);
      const page = pageOf(issues, args.first, args.after, (issue) => issue.number);
      return { type: 'IssueConnection', repo, issues: page.items, pageInfo: page.pageInfo } as Source;
    }
    case 'pullRequest': {
      const pull = repo.pulls.get(Number(args.number));
      return pull ? ({ type: 'PullRequest', repo, pull } as Source) : null;
    }
    case 'pullRequests': {
      let pulls = [...repo.pulls.values()];
      const states = Array.isArray(args.states) ? args.states.map(String) : null;
      if (states) pulls = pulls.filter((pull) => states.includes(pullState(pull)));
      pulls.sort((a, b) => b.number - a.number);
      const page = pageOf(pulls, args.first, args.after, (pull) => pull.number);
      return { type: 'PullRequestConnection', repo, pulls: page.items, pageInfo: page.pageInfo } as Source;
    }
    default:
      return unknownField('Repository', field, ctx);
  }
}

function resolvePullRequest(source: Extract<Source, { type: 'PullRequest' }>, field: FieldNode, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  const { repo, pull } = source;
  switch (field.name) {
    case 'id': return pullGlobalId(repo, pull);
    case 'number': return pull.number;
    case 'title': return pull.title;
    case 'body': return pull.body;
    case 'state': return pullState(pull);
    case 'merged': return pull.merged;
    case 'mergeable': {
      if (pull.state !== 'open') return 'UNKNOWN';
      return pullAnalysis(repo, pull).conflict ? 'CONFLICTING' : 'MERGEABLE';
    }
    case 'headRefName': return pull.headRef;
    case 'baseRefName': return pull.baseRef;
    case 'url': return `/${repo.owner}/${repo.name}/pull/${pull.number}`;
    case 'createdAt': return new Date(pull.createdEpoch * 1000).toISOString();
    case 'updatedAt': return new Date(pull.updatedEpoch * 1000).toISOString();
    case 'files': {
      const { files } = pullAnalysis(repo, pull);
      const limit = typeof args.first === 'number' && args.first > 0 ? Math.trunc(args.first) : files.length;
      const mapped = files.slice(0, limit).map((file) => ({
        type: 'PullRequestChangedFile',
        path: file.filename,
        additions: file.additions,
        deletions: file.deletions,
        changeType: file.status === 'added' ? 'ADDED' : file.status === 'removed' ? 'DELETED' : 'MODIFIED',
      }) as Source);
      return { type: 'FileConnection', files: mapped, totalCount: files.length } as Source;
    }
    default:
      return unknownField('PullRequest', field, ctx);
  }
}

function resolveType(name: string, field: FieldNode, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  switch (field.name) {
    case 'name': return name;
    case 'kind': return ENUM_VALUES[name] ? 'ENUM' : 'OBJECT';
    case 'description': return null;
    case 'fields': {
      const fields = OBJECT_FIELDS[name];
      if (!fields) return null;
      return fields.map((fieldName) => ({ type: 'Raw', value: { name: fieldName, description: null, isDeprecated: false, deprecationReason: null } }) as Source);
    }
    case 'enumValues': {
      const values = ENUM_VALUES[name];
      if (!values) return null;
      return values.map((valueName) => ({ type: 'Raw', value: { name: valueName, description: null, isDeprecated: false, deprecationReason: null } }) as Source);
    }
    case 'possibleTypes': return null;
    case 'inputFields': return null;
    case 'ofType': return null;
    default: return unknownField('__Type', field, ctx);
  }
}

function resolveMutation(field: FieldNode, args: Record<string, unknown>, ctx: ExecCtx): unknown {
  const input = isRecord(args.input) ? args.input : {};
  try {
    switch (field.name) {
      case 'createIssue': {
        const repo = repoByGlobalId(input.repositoryId, ctx);
        if (!repo) throw new ApiError(404, 'repository not found');
        const stamp = tickEpochSec();
        const issue: IssueRec = {
          id: ctx.forge.allocObjectId(),
          number: repo.nextNumber++,
          title: typeof input.title === 'string' ? input.title : '',
          body: typeof input.body === 'string' ? input.body : '',
          state: 'open',
          labels: [],
          createdEpoch: stamp,
          updatedEpoch: stamp,
          comments: [],
        };
        repo.issues.set(issue.number, issue);
        return { type: 'Payload', payloadType: 'CreateIssuePayload', values: { issue: { type: 'Issue', repo, issue }, clientMutationId: null } } as Source;
      }
      case 'closeIssue': {
        const located = issueByGlobalId(input.issueId, ctx);
        if (!located) throw new ApiError(404, 'issue not found');
        located.issue.state = 'closed';
        located.issue.updatedEpoch = tickEpochSec();
        return { type: 'Payload', payloadType: 'CloseIssuePayload', values: { issue: { type: 'Issue', repo: located.repo, issue: located.issue }, clientMutationId: null } } as Source;
      }
      case 'createPullRequest': {
        const repo = repoByGlobalId(input.repositoryId, ctx);
        if (!repo) throw new ApiError(404, 'repository not found');
        const pull = openPull(ctx.forge, repo, {
          title: typeof input.title === 'string' ? input.title : '',
          body: typeof input.body === 'string' ? input.body : '',
          head: typeof input.headRefName === 'string' ? input.headRefName : '',
          base: typeof input.baseRefName === 'string' ? input.baseRefName : '',
        });
        return { type: 'Payload', payloadType: 'CreatePullRequestPayload', values: { pullRequest: { type: 'PullRequest', repo, pull }, clientMutationId: null } } as Source;
      }
      case 'mergePullRequest': {
        const located = pullByGlobalId(input.pullRequestId, ctx);
        if (!located) throw new ApiError(404, 'pull request not found');
        if (located.pull.state === 'open') squashMerge(located.repo, located.pull);
        return { type: 'Payload', payloadType: 'MergePullRequestPayload', values: { pullRequest: { type: 'PullRequest', repo: located.repo, pull: located.pull }, clientMutationId: null } } as Source;
      }
      case 'addComment': {
        const located = issueByGlobalId(input.subjectId, ctx);
        if (!located) throw new ApiError(404, 'subject not found');
        const body = typeof input.body === 'string' ? input.body : '';
        const comment = { id: ctx.forge.allocCommentId(), body, createdEpoch: tickEpochSec() };
        located.issue.comments.push(comment);
        const node: Source = { type: 'Comment', id: commentGlobalId(located.repo, located.issue, comment.id), body };
        return { type: 'Payload', payloadType: 'AddCommentPayload', values: { commentEdge: { type: 'CommentEdge', node }, subject: { type: 'Issue', repo: located.repo, issue: located.issue }, clientMutationId: null } } as Source;
      }
      default:
        return unknownField('Mutation', field, ctx);
    }
  } catch (err) {
    if (err instanceof ApiError) {
      ctx.errors.push({ message: err.message });
      return null;
    }
    throw err;
  }
}

function repoByGlobalId(id: unknown, ctx: ExecCtx): RepoRec | null {
  if (typeof id !== 'string') return null;
  const decoded = decodeId(id);
  if (!decoded || decoded.kind !== 'repo') return null;
  return ctx.forge.findRepoById(Number(decoded.parts[0])) ?? null;
}

function issueByGlobalId(id: unknown, ctx: ExecCtx): { repo: RepoRec; issue: IssueRec } | null {
  if (typeof id !== 'string') return null;
  const decoded = decodeId(id);
  if (!decoded || decoded.kind !== 'issue') return null;
  const repo = ctx.forge.findRepoById(Number(decoded.parts[0]));
  const issue = repo?.issues.get(Number(decoded.parts[1]));
  return repo && issue ? { repo, issue } : null;
}

function pullByGlobalId(id: unknown, ctx: ExecCtx): { repo: RepoRec; pull: PullRec } | null {
  if (typeof id !== 'string') return null;
  const decoded = decodeId(id);
  if (!decoded || decoded.kind !== 'pull') return null;
  const repo = ctx.forge.findRepoById(Number(decoded.parts[0]));
  const pull = repo?.pulls.get(Number(decoded.parts[1]));
  return repo && pull ? { repo, pull } : null;
}

// -------------------------------------------------------------------------------------------
// selection execution

function executeSelectionSet(source: Source, selections: readonly SelectionNode[], ctx: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'field') {
      const key = selection.alias ?? selection.name;
      const value = resolveField(source, selection, ctx);
      out[key] = completeValue(value, selection, ctx);
    } else if (selection.kind === 'inline') {
      if (selection.typeCondition === null || selection.typeCondition === source.type || selection.typeCondition === 'Node') {
        Object.assign(out, executeSelectionSet(source, selection.selectionSet, ctx));
      }
    } else if (selection.kind === 'spread') {
      const fragment = ctx.fragments.get(selection.name);
      if (fragment && (fragment.typeCondition === null || fragment.typeCondition === source.type || fragment.typeCondition === 'Node')) {
        Object.assign(out, executeSelectionSet(source, fragment.selectionSet, ctx));
      }
    }
  }
  return out;
}

function completeValue(value: unknown, field: FieldNode, ctx: ExecCtx): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) return value.map((entry) => completeValue(entry, field, ctx));
  if (typeof value === 'object' && 'type' in (value as Record<string, unknown>)) {
    const source = value as Source;
    if (!field.selectionSet) return source.type === 'Raw' ? (source as Extract<Source, { type: 'Raw' }>).value : null;
    if (source.type === 'Raw') {
      const raw = source.value;
      if (isRecord(raw)) {
        // Raw wrappers carry pre-shaped values (introspection entries, file connections).
        const shaped: Record<string, unknown> = {};
        for (const selection of field.selectionSet) {
          if (selection.kind !== 'field') continue;
          const key = selection.alias ?? selection.name;
          const inner = (raw as Record<string, unknown>)[selection.name];
          if (Array.isArray(inner)) {
            shaped[key] = inner.map((entry) => (typeof entry === 'object' && entry !== null && 'type' in (entry as Record<string, unknown>) ? completeValue(entry, selection, ctx) : entry));
          } else if (typeof inner === 'object' && inner !== null && 'type' in (inner as Record<string, unknown>)) {
            shaped[key] = completeValue(inner, selection, ctx);
          } else {
            shaped[key] = inner ?? null;
          }
        }
        return shaped;
      }
      return raw;
    }
    return executeSelectionSet(source, field.selectionSet, ctx);
  }
  return value;
}

// -------------------------------------------------------------------------------------------
// the route

function runOperation(document: DocumentNode, operationName: string | null, variables: Record<string, unknown>, forge: ReqCtx['forge']): { data: unknown; errors?: Array<{ message: string }> } {
  const operation = operationName
    ? document.operations.find((candidate) => candidate.name === operationName) ?? document.operations[0]
    : document.operations[0];
  if (!operation) return { data: null, errors: [{ message: 'no operation in the document' }] };
  const ctx: ExecCtx = { forge, variables, fragments: document.fragments, errors: [] };
  const root: Source = operation.type === 'mutation' ? { type: 'Mutation' } : { type: 'Query' };
  const data = executeSelectionSet(root, operation.selectionSet, ctx);
  return ctx.errors.length ? { data, errors: ctx.errors } : { data };
}

export function handleGraphQL(ctx: ReqCtx): Reply {
  if (!ctx.authorized) return jsonReply(401, { message: 'Bad credentials' });
  if (ctx.method !== 'POST') return jsonReply(404, { message: 'Not Found' });
  let parsed: unknown;
  try {
    parsed = JSON.parse(ctx.body.toString('utf8'));
  } catch {
    return jsonReply(400, { errors: [{ message: 'Problems parsing JSON' }] });
  }
  const runOne = (entry: unknown): { data: unknown; errors?: Array<{ message: string }> } => {
    if (!isRecord(entry) || typeof entry.query !== 'string') return { data: null, errors: [{ message: 'a query is required' }] };
    const variables = isRecord(entry.variables) ? entry.variables : {};
    const operationName = typeof entry.operationName === 'string' ? entry.operationName : null;
    try {
      const document = new Parser(entry.query).parseDocument();
      return runOperation(document, operationName, variables, ctx.forge);
    } catch (err) {
      return { data: null, errors: [{ message: err instanceof Error ? err.message : 'could not parse the document' }] };
    }
  };
  if (Array.isArray(parsed)) return jsonReply(200, parsed.map(runOne));
  return jsonReply(200, runOne(parsed));
}
