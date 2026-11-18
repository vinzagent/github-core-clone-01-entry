// ops.ts — the domain operations every surface shares: committing a file
// change through the contents API, opening a pull request, computing its
// changed files and squash-merging it. REST, GraphQL and MCP all act on the
// same store through these functions, so an object created on one surface is
// the same object on the others.

import {
  buildCommitData,
  buildTreeFromFlat,
  countLines,
  diffTrees,
  flattenTree,
  headRef,
  isoOf,
  mergeBase,
  parseCommit,
  threeWayMerge,
  tickEpochSec,
  viewerIdent,
  type Forge,
  type PullRec,
  type RepoRec,
} from './forge.ts';

/** An error that maps onto an HTTP status and a `{message}` body. */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export function notFound(what = 'Not Found'): ApiError {
  return new ApiError(404, what);
}

/**
 * Commit one file change (create, update, or — with `content: null` — delete)
 * on a branch, creating the branch from the default branch's tip when it does
 * not exist yet, or a root commit when the repository is still empty.
 */
export function commitFileChange(
  repo: RepoRec,
  options: { branch?: string; path: string; content: Buffer | null; message: string; sha?: string },
): { commitSha: string; blobSha: string | null; created: boolean } {
  const branch = options.branch ?? repo.defaultBranch;
  const ref = `refs/heads/${branch}`;
  const tip = repo.refs.get(ref);
  const store = repo.store;

  let baseTree: string | null = null;
  let parents: string[] = [];
  if (tip) {
    const commit = store.get(tip);
    if (!commit || commit.type !== 'commit') throw new ApiError(422, 'Unprocessable Entity');
    baseTree = parseCommit(commit.data).tree;
    parents = [tip];
  } else {
    const fallback = repo.refs.get(headRef(repo));
    if (fallback && branch !== repo.defaultBranch) {
      const commit = store.get(fallback);
      if (commit && commit.type === 'commit') {
        baseTree = parseCommit(commit.data).tree;
        parents = [fallback];
      }
    }
  }

  const flat = baseTree ? flattenTree(store, baseTree) : new Map<string, string>();
  const existing = flat.get(options.path);
  if (options.content === null) {
    if (existing === undefined) throw notFound('Not Found');
    if (options.sha !== undefined && options.sha !== existing) throw new ApiError(409, 'Conflict: the file sha does not match');
    flat.delete(options.path);
  } else {
    if (existing !== undefined) {
      if (options.sha === undefined) throw new ApiError(422, 'Validation Failed: sha is required when updating a file');
      if (options.sha !== existing) throw new ApiError(409, 'Conflict: the file sha does not match');
    } else if (options.sha !== undefined) {
      throw new ApiError(422, 'Validation Failed: sha supplied for a file that does not exist');
    }
    flat.set(options.path, store.put('blob', options.content));
  }

  const tree = buildTreeFromFlat(store, flat);
  const epoch = tickEpochSec();
  const commitSha = store.put(
    'commit',
    buildCommitData({ tree, parents, message: options.message, ident: viewerIdent(epoch) }),
  );
  repo.refs.set(ref, commitSha);
  repo.updatedEpoch = epoch;
  const blobSha = options.content === null ? null : flat.get(options.path) ?? null;
  return { commitSha, blobSha, created: existing === undefined && options.content !== null };
}

/** The tree oid at a commit, or null. */
export function treeOfCommit(repo: RepoRec, commitSha: string | undefined): string | null {
  if (!commitSha) return null;
  const commit = repo.store.get(commitSha);
  if (!commit || commit.type !== 'commit') return null;
  return parseCommit(commit.data).tree;
}

export interface PullFile {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
  sha: string | null;
}

/** The merge base and the changed files of a pull request, computed live. */
export function pullAnalysis(repo: RepoRec, pull: PullRec): { base: string | null; files: PullFile[]; conflict: boolean } {
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (!headTip || !baseTip) return { base: null, files: [], conflict: false };
  const base = mergeBase(repo.store, headTip, baseTip);
  if (!base) return { base: null, files: [], conflict: true };
  const baseTree = treeOfCommit(repo, base);
  const headTree = treeOfCommit(repo, headTip);
  const ontoTree = treeOfCommit(repo, baseTip);
  if (!baseTree || !headTree || !ontoTree) return { base, files: [], conflict: false };
  const changes = diffTrees(repo.store, baseTree, headTree);
  const files: PullFile[] = [];
  for (const [path, change] of [...changes.entries()].sort(([a], [b]) => (a < b ? -1 : 1))) {
    const fromBlob = change.from ? repo.store.get(change.from) : undefined;
    const toBlob = change.to ? repo.store.get(change.to) : undefined;
    const additions = toBlob ? countLines(toBlob.data) : 0;
    const deletions = fromBlob ? countLines(fromBlob.data) : 0;
    files.push({
      filename: path,
      status: change.from === null ? 'added' : change.to === null ? 'removed' : 'modified',
      additions,
      deletions,
      changes: additions + deletions,
      sha: change.to,
    });
  }
  const merged = threeWayMerge(repo.store, baseTree, headTree, ontoTree);
  return { base, files, conflict: !merged.ok };
}

/** Open a pull request; the head and base branches must both exist. */
export function openPull(
  forge: Forge,
  repo: RepoRec,
  input: { title: string; body?: string; head: string; base: string },
): PullRec {
  const head = input.head.includes(':') ? input.head.split(':').pop()! : input.head;
  const base = input.base;
  if (!repo.refs.has(`refs/heads/${head}`)) throw new ApiError(422, 'Validation Failed: head branch does not exist');
  if (!repo.refs.has(`refs/heads/${base}`)) throw new ApiError(422, 'Validation Failed: base branch does not exist');
  for (const existing of repo.pulls.values()) {
    if (existing.headRef === head && existing.baseRef === base && existing.state === 'open') {
      throw new ApiError(422, `Validation Failed: a pull request already exists for ${head} into ${base}`);
    }
  }
  const epoch = tickEpochSec();
  const pull: PullRec = {
    id: forge.allocObjectId(),
    number: repo.nextNumber++,
    title: input.title,
    body: input.body ?? '',
    state: 'open',
    merged: false,
    mergedEpoch: null,
    mergeCommitSha: null,
    headRef: head,
    baseRef: base,
    createdEpoch: epoch,
    updatedEpoch: epoch,
  };
  repo.pulls.set(pull.number, pull);
  return pull;
}

/**
 * Squash-merge a pull request: one new commit on the base branch whose tree
 * is the three-way merge of the head's changes. Throws ApiError 405 on a
 * conflict, 422 when there is nothing to merge.
 */
export function squashMerge(repo: RepoRec, pull: PullRec): { sha: string } {
  if (pull.state !== 'open') throw new ApiError(422, 'Validation Failed: the pull request is not open');
  const headTip = repo.refs.get(`refs/heads/${pull.headRef}`);
  const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`);
  if (!headTip || !baseTip) throw new ApiError(422, 'Validation Failed: a branch is missing');
  const base = mergeBase(repo.store, headTip, baseTip);
  if (!base) throw new ApiError(405, 'Conflict: the branches have no common history');
  const baseTree = treeOfCommit(repo, base)!;
  const headTree = treeOfCommit(repo, headTip)!;
  const ontoTree = treeOfCommit(repo, baseTip)!;
  const merged = threeWayMerge(repo.store, baseTree, headTree, ontoTree);
  if (!merged.ok) throw new ApiError(405, 'Conflict: the pull request has conflicts');
  const epoch = tickEpochSec();
  const message = pull.body ? `${pull.title}\n\n${pull.body}` : pull.title;
  const sha = repo.store.put('commit', buildCommitData({ tree: merged.tree, parents: [baseTip], message, ident: viewerIdent(epoch) }));
  repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedEpoch = epoch;
  pull.mergeCommitSha = sha;
  pull.updatedEpoch = epoch;
  repo.updatedEpoch = epoch;
  return { sha };
}

/** A deterministic ISO timestamp for a stored epoch tick. */
export function iso(epoch: number): string {
  return isoOf(epoch);
}
