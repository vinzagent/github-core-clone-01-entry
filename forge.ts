// forge.ts — the in-memory heart of the forge: users, repositories, the git object
// store, issues, labels and pull requests, plus the pure git plumbing (trees,
// commits, packs, deltas, reachability, merges) every surface shares.
//
// Everything is deterministic: ids come from counters and timestamps from a
// fixed base epoch plus a counter, never from the wall clock or a random
// source, so the same request sequence always produces the same bytes.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

// -------------------------------------------------------------------------------------------
// identity

/** The one user this forge knows: the bearer token's owner. */
export interface UserRec {
  readonly login: string;
  readonly id: number;
  readonly name: string;
  readonly email: string;
  readonly type: string;
}

export const VIEWER: UserRec = {
  login: 'adaiversen',
  id: 1000,
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
  type: 'User',
};

/** Deterministic clock: a fixed base epoch, advanced one second per tick. */
const BASE_EPOCH_SEC = 1_700_000_000;
let clockTicks = 0;

export function tickEpochSec(): number {
  clockTicks += 1;
  return BASE_EPOCH_SEC + clockTicks;
}

export function isoOf(epochSec: number): string {
  return new Date(epochSec * 1000).toISOString();
}

/** The git author/committer line for the viewer at a tick. */
export function viewerIdent(epochSec: number): string {
  return `${VIEWER.name} <${VIEWER.email}> ${epochSec} +0000`;
}

// -------------------------------------------------------------------------------------------
// git objects

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

export const ZERO_OID = '0'.repeat(40);
const TYPE_CODE: Readonly<Record<GitObjectType, number>> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Readonly<Record<number, GitObjectType>> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function hashObject(type: GitObjectType, data: Buffer): string {
  const hash = createHash('sha1');
  hash.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  hash.update(data);
  return hash.digest('hex');
}

/** A content-addressed object store: one per repository. */
export class ObjectStore {
  readonly objects = new Map<string, GitObject>();

  put(type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data });
    return oid;
  }

  get(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  has(oid: string): boolean {
    return this.objects.has(oid);
  }
}

// -------------------------------------------------------------------------------------------
// trees

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) break;
    entries.push({
      mode: data.subarray(offset, space).toString('ascii'),
      name: data.subarray(space + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    offset = nul + 21;
  }
  return entries;
}

function compareTreeEntries(a: TreeEntry, b: TreeEntry): number {
  const left = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const right = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(left, right);
}

export function buildTreeData(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort(compareTreeEntries);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** Flatten a tree (recursively) into path -> blob oid. */
export function flattenTree(store: ObjectStore, treeOid: string, prefix = '', out = new Map<string, string>()): Map<string, string> {
  const tree = store.get(treeOid);
  if (!tree || tree.type !== 'tree') return out;
  for (const entry of parseTree(tree.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(store, entry.oid, path, out);
    else out.set(path, entry.oid);
  }
  return out;
}

/** Build nested trees from a flat path -> blob oid map; returns the root tree oid. */
export function buildTreeFromFlat(store: ObjectStore, flat: ReadonlyMap<string, string>): string {
  interface Node {
    files: Map<string, string>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, oid] of flat) {
    const segments = path.split('/');
    let node = root;
    for (let index = 0; index < segments.length - 1; index += 1) {
      const name = segments[index]!;
      if (!node.dirs.has(name)) node.dirs.set(name, make());
      node = node.dirs.get(name)!;
    }
    node.files.set(segments[segments.length - 1]!, oid);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, oid] of node.files) entries.push({ mode: '100644', name, oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    return store.put('tree', buildTreeData(entries));
  };
  return build(root);
}

/** The entries of the tree at `path` inside `rootTreeOid` ('' = the root itself), or null. */
export function subtreeAt(store: ObjectStore, rootTreeOid: string, path: string): { oid: string; entries: TreeEntry[] } | null {
  let current = rootTreeOid;
  if (path !== '') {
    for (const segment of path.split('/')) {
      const tree = store.get(current);
      if (!tree || tree.type !== 'tree') return null;
      const entry = parseTree(tree.data).find((candidate) => candidate.name === segment);
      if (!entry || entry.mode !== '40000') return null;
      current = entry.oid;
    }
  }
  const tree = store.get(current);
  if (!tree || tree.type !== 'tree') return null;
  return { oid: current, entries: parseTree(tree.data) };
}

// -------------------------------------------------------------------------------------------
// commits and tags

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const view: CommitView = { tree: '', parents: [], author: '', committer: '', message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8') };
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    if (!line) continue;
    const space = line.indexOf(' ');
    const key = space < 0 ? line : line.slice(0, space);
    const value = space < 0 ? '' : line.slice(space + 1);
    if (key === 'tree') view.tree = value;
    else if (key === 'parent') view.parents.push(value);
    else if (key === 'author') view.author = value;
    else if (key === 'committer') view.committer = value;
  }
  return view;
}

export function buildCommitData(fields: { tree: string; parents: readonly string[]; message: string; ident: string }): Buffer {
  const lines = [`tree ${fields.tree}`];
  for (const parent of fields.parents) lines.push(`parent ${parent}`);
  lines.push(`author ${fields.ident}`, `committer ${fields.ident}`, '');
  const message = fields.message.endsWith('\n') ? fields.message : `${fields.message}\n`;
  return Buffer.from(`${lines.join('\n')}\n${message}`, 'utf8');
}

export function parseTag(data: Buffer): { object: string; type: string; tag: string } {
  const blank = data.indexOf('\n\n');
  const headerEnd = blank < 0 ? data.length : blank;
  const headers = new Map<string, string>();
  for (const line of data.subarray(0, headerEnd).toString('utf8').split('\n')) {
    const space = line.indexOf(' ');
    if (space > 0) headers.set(line.slice(0, space), line.slice(space + 1));
  }
  return { object: headers.get('object') ?? '', type: headers.get('type') ?? '', tag: headers.get('tag') ?? '' };
}

/** Peel a tag chain to the commit it ultimately names (or the object itself). */
export function peelToCommit(store: ObjectStore, oid: string): string {
  let current = oid;
  for (let guard = 0; guard < 32; guard += 1) {
    const object = store.get(current);
    if (!object) return current;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') {
      current = parseTag(object.data).object;
      continue;
    }
    return current;
  }
  return current;
}

/** Peel one level: a tag's target, for `^{}` advertisement lines. */
export function peelOne(store: ObjectStore, oid: string): string | null {
  const object = store.get(oid);
  if (!object || object.type !== 'tag') return null;
  return parseTag(object.data).object;
}

// -------------------------------------------------------------------------------------------
// reachability

/** Every object reachable from `roots`. Missing objects are skipped when `allowMissing`. */
export function collectReachable(store: ObjectStore, roots: Iterable<string>, options: { allowMissing?: boolean } = {}): Set<string> {
  const seen = new Set<string>();
  const queue: string[] = [...roots];
  while (queue.length) {
    const oid = queue.pop()!;
    if (seen.has(oid) || oid === ZERO_OID) continue;
    const object = store.get(oid);
    if (!object) {
      if (options.allowMissing) continue;
      continue;
    }
    seen.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      queue.push(commit.tree);
      for (const parent of commit.parents) queue.push(parent);
    } else if (object.type === 'tag') {
      queue.push(parseTag(object.data).object);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode === '160000') continue;
        queue.push(entry.oid);
      }
    }
  }
  return seen;
}

/** The set of commits reachable from `tip` (inclusive), following parents only. */
export function commitAncestors(store: ObjectStore, tip: string): Set<string> {
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length) {
    const oid = queue.pop()!;
    if (seen.has(oid)) continue;
    const object = store.get(oid);
    if (!object || object.type !== 'commit') continue;
    seen.add(oid);
    for (const parent of parseCommit(object.data).parents) queue.push(parent);
  }
  return seen;
}

export function isAncestor(store: ObjectStore, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  return commitAncestors(store, descendant).has(ancestor);
}

/** A merge base of two commits: the first commit reachable from `b` that is an ancestor of `a`. */
export function mergeBase(store: ObjectStore, a: string, b: string): string | null {
  const ancestorsOfA = commitAncestors(store, a);
  const seen = new Set<string>();
  const queue = [b];
  while (queue.length) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    if (ancestorsOfA.has(oid)) return oid;
    const object = store.get(oid);
    if (object && object.type === 'commit') queue.push(...parseCommit(object.data).parents);
  }
  return null;
}

/** Commits in history order from a tip: newest first, parents after their children. */
export function listCommits(store: ObjectStore, tip: string): string[] {
  const order: string[] = [];
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length) {
    const oid = queue.shift()!;
    if (seen.has(oid)) continue;
    const object = store.get(oid);
    if (!object || object.type !== 'commit') continue;
    seen.add(oid);
    order.push(oid);
    queue.push(...parseCommit(object.data).parents);
  }
  return order;
}

// -------------------------------------------------------------------------------------------
// tree diffs and merges

export interface PathChange {
  from: string | null;
  to: string | null;
}

/** Path-level diff between two trees (by root tree oid). */
export function diffTrees(store: ObjectStore, fromTree: string, toTree: string): Map<string, PathChange> {
  const from = flattenTree(store, fromTree);
  const to = flattenTree(store, toTree);
  const changes = new Map<string, PathChange>();
  for (const [path, oid] of to) {
    const before = from.get(path);
    if (before === undefined) changes.set(path, { from: null, to: oid });
    else if (before !== oid) changes.set(path, { from: before, to: oid });
  }
  for (const [path, oid] of from) {
    if (!to.has(path)) changes.set(path, { from: oid, to: null });
  }
  return changes;
}

export interface MergeOutcome {
  ok: boolean;
  tree: string;
  conflicts: string[];
}

/**
 * Three-way tree merge for a squash: apply the head side's changes (against the
 * merge base) onto the base side's tree. A path changed on both sides to
 * different results is a conflict.
 */
export function threeWayMerge(store: ObjectStore, baseTree: string, headTree: string, ontoTree: string): MergeOutcome {
  const headChanges = diffTrees(store, baseTree, headTree);
  const ontoChanges = diffTrees(store, baseTree, ontoTree);
  const conflicts: string[] = [];
  for (const [path, change] of headChanges) {
    const onto = ontoChanges.get(path);
    if (onto && onto.to !== change.to) conflicts.push(path);
  }
  if (conflicts.length) return { ok: false, tree: ontoTree, conflicts };
  const flat = flattenTree(store, ontoTree);
  for (const [path, change] of headChanges) {
    if (change.to === null) flat.delete(path);
    else flat.set(path, change.to);
  }
  return { ok: true, tree: buildTreeFromFlat(store, flat), conflicts: [] };
}

// -------------------------------------------------------------------------------------------
// packfiles

function writePackObjectHeader(type: GitObjectType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let remaining = size;
  let first = (code << 4) | (remaining & 0x0f);
  remaining = Math.floor(remaining / 16);
  while (remaining > 0) {
    bytes.push(first | 0x80);
    first = remaining & 0x7f;
    remaining = Math.floor(remaining / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

/** Build a version-2 pack of whole objects (deltas are optional when sending). */
export function writePack(objects: Iterable<GitObject>): Buffer {
  const list = [...objects];
  const header = Buffer.allocUnsafe(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(list.length, 8);
  const parts: Buffer[] = [header];
  for (const object of list) {
    parts.push(writePackObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

function readVarintSize(buffer: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buffer[start]!;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let offset = start + 1;
  while (byte & 0x80) {
    byte = buffer[offset]!;
    offset += 1;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if (shift > 63) throw new Error('pack entry size varint is too long');
  }
  return { size, next: offset, first };
}

function readOfsDeltaBase(buffer: Buffer, start: number): { baseOffset: number; next: number } {
  let byte = buffer[start]!;
  let offset = start + 1;
  let value = byte & 0x7f;
  while (byte & 0x80) {
    byte = buffer[offset]!;
    offset += 1;
    value = (value + 1) * 128 + (byte & 0x7f);
  }
  return { baseOffset: value, next: offset };
}

function inflateAt(buffer: Buffer, offset: number): { data: Buffer; next: number } {
  const result = inflateSync(buffer.subarray(offset), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
  return { data: result.buffer, next: offset + result.engine.bytesWritten };
}

/** Apply a git delta instruction stream to a base object's bytes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readSize = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset]!;
      offset += 1;
      value += (byte & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(byte & 0x80)) break;
    }
    return value;
  };
  const sourceSize = readSize();
  const targetSize = readSize();
  if (sourceSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(targetSize);
  let written = 0;
  while (offset < delta.length) {
    const op = delta[offset]!;
    offset += 1;
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit += 1) {
        if (op & (1 << bit)) {
          copyOffset += delta[offset]! * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit += 1) {
        if (op & (1 << bit)) {
          copySize += delta[offset]! * 2 ** shift;
          offset += 1;
        }
        shift += 8;
      }
      if (copySize === 0) copySize = 0x10000;
      base.copy(out, written, copyOffset, copyOffset + copySize);
      written += copySize;
    } else if (op !== 0) {
      const literal = op & 0x7f;
      delta.copy(out, written, offset, offset + literal);
      offset += literal;
      written += literal;
    } else {
      throw new Error('delta opcode 0 is invalid');
    }
  }
  if (written !== targetSize) throw new Error('delta produced the wrong size');
  return out;
}

/**
 * Read a pack into a store, resolving OFS_DELTA and REF_DELTA entries (a thin
 * pack's base may already live in the store). Returns the object ids stored.
 */
export function readPackInto(store: ObjectStore, pack: Buffer): string[] {
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  interface Raw {
    offset: number;
    typeCode: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
  }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let offset = 12;
  for (let index = 0; index < count; index += 1) {
    const entryOffset = offset;
    const header = readVarintSize(pack, offset);
    offset = header.next;
    const typeCode = (header.first >> 4) & 0x07;
    const entry: Raw = { offset: entryOffset, typeCode, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      const base = readOfsDeltaBase(pack, offset);
      offset = base.next;
      entry.baseOffset = entryOffset - base.baseOffset;
    } else if (typeCode === 7) {
      entry.baseOid = pack.subarray(offset, offset + 20).toString('hex');
      offset += 20;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`unknown pack type code ${typeCode}`);
    }
    const inflated = inflateAt(pack.subarray(0, trailerStart), offset);
    if (inflated.data.length !== header.size) throw new Error('pack entry size mismatch');
    entry.data = inflated.data;
    offset = inflated.next;
    raw.push(entry);
    byOffset.set(entryOffset, entry);
  }
  const resolved = new Map<number, { type: GitObjectType; data: Buffer; oid: string }>();
  const resolving = new Set<number>();
  const resolve = (entry: Raw): { type: GitObjectType; data: Buffer; oid: string } => {
    const cached = resolved.get(entry.offset);
    if (cached) return cached;
    if (resolving.has(entry.offset)) throw new Error('delta cycle in pack');
    resolving.add(entry.offset);
    let type: GitObjectType;
    let data: Buffer;
    if (entry.typeCode === 6) {
      const baseEntry = byOffset.get(entry.baseOffset!);
      if (!baseEntry) throw new Error('ofs-delta base is not in the pack');
      const base = resolve(baseEntry);
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else if (entry.typeCode === 7) {
      const baseEntry = [...byOffset.values()].find((candidate) => resolved.get(candidate.offset)?.oid === entry.baseOid);
      let base: { type: GitObjectType; data: Buffer } | undefined;
      if (baseEntry) base = resolve(baseEntry);
      else base = store.get(entry.baseOid!);
      if (!base) throw new Error('ref-delta base is neither in the pack nor the store');
      type = base.type;
      data = applyDelta(base.data, entry.data);
    } else {
      type = CODE_TYPE[entry.typeCode]!;
      data = entry.data;
    }
    const oid = hashObject(type, data);
    const value = { type, data, oid };
    resolved.set(entry.offset, value);
    resolving.delete(entry.offset);
    return value;
  };
  const stored: string[] = [];
  for (const entry of raw) {
    const object = resolve(entry);
    if (!store.objects.has(object.oid)) store.objects.set(object.oid, { type: object.type, data: object.data });
    stored.push(object.oid);
  }
  return stored;
}

// -------------------------------------------------------------------------------------------
// domain records

export interface LabelRec {
  name: string;
  color: string;
  description: string | null;
}

export interface CommentRec {
  id: number;
  body: string;
  createdEpoch: number;
}

export interface IssueRec {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdEpoch: number;
  updatedEpoch: number;
  comments: CommentRec[];
}

export interface PullRec {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedEpoch: number | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  createdEpoch: number;
  updatedEpoch: number;
}

export interface RepoRec {
  id: number;
  name: string;
  owner: string;
  isPrivate: boolean;
  description: string;
  defaultBranch: string;
  createdEpoch: number;
  updatedEpoch: number;
  store: ObjectStore;
  /** Full ref name (refs/heads/main) -> object id. */
  refs: Map<string, string>;
  labels: Map<string, LabelRec>;
  issues: Map<number, IssueRec>;
  pulls: Map<number, PullRec>;
  /** Issues and pull requests share one number sequence, as on GitHub. */
  nextNumber: number;
}

export class Forge {
  readonly repos = new Map<string, RepoRec>();
  private nextRepoId = 1;
  private nextObjectId = 1;
  private nextCommentId = 1;

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): RepoRec | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(name: string, options: { isPrivate?: boolean; description?: string; defaultBranch?: string }): RepoRec {
    const epoch = tickEpochSec();
    const repo: RepoRec = {
      id: this.nextRepoId++,
      name,
      owner: VIEWER.login,
      isPrivate: options.isPrivate ?? false,
      description: options.description ?? '',
      defaultBranch: options.defaultBranch ?? 'main',
      createdEpoch: epoch,
      updatedEpoch: epoch,
      store: new ObjectStore(),
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
    };
    this.repos.set(this.repoKey(repo.owner, repo.name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  /** The viewer's repositories in creation order. */
  viewerRepos(): RepoRec[] {
    return [...this.repos.values()].filter((repo) => repo.owner === VIEWER.login).sort((a, b) => a.id - b.id);
  }

  allocObjectId(): number {
    return this.nextObjectId++;
  }

  allocCommentId(): number {
    return this.nextCommentId++;
  }

  findRepoById(id: number): RepoRec | undefined {
    for (const repo of this.repos.values()) if (repo.id === id) return repo;
    return undefined;
  }
}

/** The ref a repo's HEAD names: its default branch, born or unborn. */
export function headRef(repo: RepoRec): string {
  return `refs/heads/${repo.defaultBranch}`;
}

export function branchTip(repo: RepoRec, branch: string): string | undefined {
  return repo.refs.get(`refs/heads/${branch}`);
}

/** Resolve a REST `ref` parameter: a branch name, a tag name, or a commit id. */
export function resolveRef(repo: RepoRec, ref: string): string | undefined {
  if (/^[0-9a-f]{40}$/i.test(ref)) {
    const object = repo.store.get(ref.toLowerCase());
    if (object) return object.type === 'tag' ? peelToCommit(repo.store, ref.toLowerCase()) : ref.toLowerCase();
    return undefined;
  }
  const branch = repo.refs.get(`refs/heads/${ref}`);
  if (branch) return branch;
  const tag = repo.refs.get(`refs/tags/${ref}`);
  if (tag) return peelToCommit(repo.store, tag);
  const full = repo.refs.get(ref);
  if (full) return peelToCommit(repo.store, full);
  return undefined;
}

/** Line counts for pull-request file stats. */
export function countLines(data: Buffer): number {
  if (data.length === 0) return 0;
  let lines = 0;
  for (const byte of data) if (byte === 0x0a) lines += 1;
  if (data[data.length - 1] !== 0x0a) lines += 1;
  return lines;
}
