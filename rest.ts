// rest.ts — the GitHub REST v3 surface under /api/v3: repositories, branches,
// commits, contents, labels, issues, comments and pull requests, with the
// statuses and shapes the wire contract states. Every reply is JSON and the
// dispatcher adds the X-GitHub-Request-Id header.

import {
  VIEWER,
  branchTip,
  buildCommitData,
  isoOf,
  listCommits,
  parseCommit,
  parseTree,
  resolveRef,
  threeWayMerge,
  tickEpochSec,
  viewerIdent,
  type IssueRec,
  type PullRec,
  type RepoRec,
} from './forge.ts';
import { ApiError, commitFileChange, notFound, openPull, pullAnalysis, squashMerge, treeOfCommit } from './ops.ts';
import { bodyJson, bodyObject, isRecord, jsonReply, messageReply, pathSegments, stringField, type Reply, type ReqCtx } from './http.ts';

const API_ROOT = '/api/v3';

function nodeId(kind: string, id: number): string {
  return Buffer.from(`${kind}${id}`, 'utf8').toString('base64');
}

function userJson(ctx: ReqCtx) {
  return {
    login: VIEWER.login,
    id: VIEWER.id,
    node_id: nodeId('04:User', VIEWER.id),
    avatar_url: '',
    gravatar_id: '',
    url: `${ctx.origin}${API_ROOT}/users/${VIEWER.login}`,
    html_url: `${ctx.origin}/${VIEWER.login}`,
    type: VIEWER.type,
    site_admin: false,
  };
}

function repoJson(ctx: ReqCtx, repo: RepoRec) {
  const base = `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}`;
  return {
    id: repo.id,
    node_id: nodeId('04:Repository', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: userJson(ctx),
    private: repo.isPrivate,
    html_url: `${ctx.origin}/${repo.owner}/${repo.name}`,
    description: repo.description,
    fork: false,
    url: base,
    created_at: isoOf(repo.createdEpoch),
    updated_at: isoOf(repo.updatedEpoch),
    pushed_at: isoOf(repo.updatedEpoch),
    git_url: `git://${ctx.origin.replace(/^https?:\/\//, '')}/${repo.owner}/${repo.name}.git`,
    clone_url: `${ctx.origin}/${repo.owner}/${repo.name}.git`,
    homepage: '',
    size: 1,
    stargazers_count: 0,
    watchers_count: 0,
    language: null,
    has_issues: true,
    has_projects: true,
    has_downloads: true,
    has_wiki: true,
    has_pages: false,
    forks_count: 0,
    open_issues_count: [...repo.issues.values()].filter((issue) => issue.state === 'open').length,
    license: { key: 'mit', name: 'MIT License', spdx_id: 'MIT', url: null, node_id: 'MDc6TGljZW5zZW1pdA==' },
    allow_forking: true,
    is_template: false,
    topics: [],
    visibility: repo.isPrivate ? 'private' : 'public',
    forks: 0,
    open_issues: [...repo.issues.values()].filter((issue) => issue.state === 'open').length,
    watchers: 0,
    default_branch: repo.defaultBranch,
  };
}

function labelJson(ctx: ReqCtx, repo: RepoRec, name: string) {
  const label = repo.labels.get(name);
  return {
    id: Math.abs([...name].reduce((acc, ch) => acc * 31 + ch.charCodeAt(0) | 0, 7)),
    node_id: nodeId('05:Label', repo.id * 1000 + name.length),
    url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/labels/${encodeURIComponent(name)}`,
    name,
    color: label?.color ?? 'ededed',
    default: false,
    description: label?.description ?? null,
  };
}

function issueJson(ctx: ReqCtx, repo: RepoRec, issue: IssueRec) {
  const base = `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}`;
  return {
    id: issue.id,
    node_id: nodeId('05:Issue', issue.id),
    url: `${base}/issues/${issue.number}`,
    repository_url: base,
    labels_url: `${base}/issues/${issue.number}/labels{/name}`,
    comments_url: `${base}/issues/${issue.number}/comments`,
    events_url: `${base}/issues/${issue.number}/events`,
    html_url: `${ctx.origin}/${repo.owner}/${repo.name}/issues/${issue.number}`,
    number: issue.number,
    state: issue.state,
    title: issue.title,
    body: issue.body,
    user: userJson(ctx),
    labels: issue.labels.map((name) => labelJson(ctx, repo, name)),
    assignee: null,
    assignees: [],
    locked: false,
    comments: issue.comments.length,
    closed_at: issue.state === 'closed' ? isoOf(issue.updatedEpoch) : null,
    created_at: isoOf(issue.createdEpoch),
    updated_at: isoOf(issue.updatedEpoch),
  };
}

function refSideJson(ctx: ReqCtx, repo: RepoRec, ref: string) {
  const tip = repo.refs.get(`refs/heads/${ref}`);
  return {
    label: `${repo.owner}:${ref}`,
    ref,
    sha: tip ?? '',
    user: userJson(ctx),
    repo: repoJson(ctx, repo),
  };
}

function pullJson(ctx: ReqCtx, repo: RepoRec, pull: PullRec) {
  const base = `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}`;
  return {
    id: pull.id,
    node_id: nodeId('05:PullRequest', pull.id),
    url: `${base}/pulls/${pull.number}`,
    html_url: `${ctx.origin}/${repo.owner}/${repo.name}/pull/${pull.number}`,
    diff_url: `${ctx.origin}/${repo.owner}/${repo.name}/pull/${pull.number}.diff`,
    patch_url: `${ctx.origin}/${repo.owner}/${repo.name}/pull/${pull.number}.patch`,
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    body: pull.body,
    user: userJson(ctx),
    created_at: isoOf(pull.createdEpoch),
    updated_at: isoOf(pull.updatedEpoch),
    closed_at: pull.state === 'closed' ? isoOf(pull.updatedEpoch) : null,
    merged_at: pull.merged && pull.mergedEpoch !== null ? isoOf(pull.mergedEpoch) : null,
    merge_commit_sha: pull.mergeCommitSha,
    merged: pull.merged,
    head: refSideJson(ctx, repo, pull.headRef),
    base: refSideJson(ctx, repo, pull.baseRef),
  };
}

function commentJson(ctx: ReqCtx, repo: RepoRec, issue: IssueRec, comment: { id: number; body: string; createdEpoch: number }) {
  return {
    id: comment.id,
    node_id: nodeId('05:IssueComment', comment.id),
    url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/issues/comments/${comment.id}`,
    html_url: `${ctx.origin}/${repo.owner}/${repo.name}/issues/${issue.number}#issuecomment-${comment.id}`,
    body: comment.body,
    user: userJson(ctx),
    created_at: isoOf(comment.createdEpoch),
    updated_at: isoOf(comment.createdEpoch),
  };
}

// -------------------------------------------------------------------------------------------
// pagination

interface Page<T> {
  items: T[];
  link: string | null;
}

function paginate<T>(ctx: ReqCtx, all: readonly T[], path: string): Page<T> {
  const perPageRaw = Number(ctx.query.get('per_page') ?? '30');
  const perPage = Number.isFinite(perPageRaw) && perPageRaw > 0 ? Math.min(100, Math.trunc(perPageRaw)) : 30;
  const pageRaw = Number(ctx.query.get('page') ?? '1');
  const page = Number.isFinite(pageRaw) && pageRaw > 0 ? Math.trunc(pageRaw) : 1;
  const start = (page - 1) * perPage;
  const items = all.slice(start, start + perPage);
  const lastPage = Math.max(1, Math.ceil(all.length / perPage));
  const url = (target: number) => `${ctx.origin}${path}?per_page=${perPage}&page=${target}`;
  const links: string[] = [];
  if (page < lastPage) {
    links.push(`<${url(page + 1)}>; rel="next"`);
    links.push(`<${url(lastPage)}>; rel="last"`);
  }
  if (page > 1) {
    links.push(`<${url(1)}>; rel="first"`);
    links.push(`<${url(page - 1)}>; rel="prev"`);
  }
  return { items, link: links.length ? links.join(', ') : null };
}

function pageReply<T>(ctx: ReqCtx, page: Page<T>, map: (item: T) => unknown): Reply {
  return jsonReply(200, page.items.map(map), page.link ? { link: page.link } : undefined);
}

// -------------------------------------------------------------------------------------------
// handlers

function rootJson(ctx: ReqCtx) {
  const root = `${ctx.origin}${API_ROOT}`;
  return {
    current_user_url: `${root}/user`,
    current_user_authorizations_html_url: `${ctx.origin}/settings/connections/applications{/client_id}`,
    authorizations_url: `${root}/authorizations`,
    code_search_url: `${root}/search/code?q={query}{&page,per_page,sort,order}`,
    commit_search_url: `${root}/search/commits?q={query}{&page,per_page,sort,order}`,
    emails_url: `${root}/user/emails`,
    emojis_url: `${root}/emojis`,
    events_url: `${root}/events`,
    feeds_url: `${root}/feeds`,
    followers_url: `${root}/user/followers`,
    following_url: `${root}/user/following{/target}`,
    gists_url: `${root}/gists{/gist_id}`,
    hub_url: `${root}/hub`,
    issue_search_url: `${root}/search/issues?q={query}{&page,per_page,sort,order}`,
    issues_url: `${root}/issues`,
    keys_url: `${root}/user/keys`,
    label_search_url: `${root}/search/labels?q={query}{&repository_id,page,per_page}`,
    notifications_url: `${root}/notifications`,
    organization_url: `${root}/orgs/{org}`,
    organization_repositories_url: `${root}/orgs/{org}/repos{?type,page,per_page,sort}`,
    organization_teams_url: `${root}/orgs/{org}/teams`,
    public_gists_url: `${root}/gists/public`,
    rate_limit_url: `${root}/rate_limit`,
    repository_url: `${root}/repos/{owner}/{repo}`,
    repository_search_url: `${root}/search/repositories?q={query}{&page,per_page,sort,order}`,
    current_user_repositories_url: `${root}/user/repos{?type,page,per_page,sort}`,
    starred_url: `${root}/user/starred{/owner}{/repo}`,
    user_url: `${root}/users/{user}`,
    user_organizations_url: `${root}/users/{user}/orgs`,
    user_repositories_url: `${root}/users/{user}/repos{?type,page,per_page,sort}`,
    user_search_url: `${root}/search/users?q={query}{&page,per_page,sort,order}`,
  };
}

function requireRepo(ctx: ReqCtx, owner: string, name: string): RepoRec {
  const repo = ctx.forge.getRepo(owner, name);
  if (!repo) throw notFound('Not Found');
  return repo;
}

function createRepo(ctx: ReqCtx): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const name = stringField(body, 'name');
  if (!name) throw new ApiError(422, 'Validation Failed: name is required');
  if (ctx.forge.getRepo(VIEWER.login, name)) {
    throw new ApiError(422, 'Validation Failed: name already exists on this account');
  }
  const repo = ctx.forge.createRepo(name, {
    isPrivate: body.private === true,
    description: stringField(body, 'description') ?? '',
    defaultBranch: stringField(body, 'default_branch') ?? 'main',
  });
  return jsonReply(201, repoJson(ctx, repo));
}

function patchRepo(ctx: ReqCtx, repo: RepoRec): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const description = stringField(body, 'description');
  if (description !== undefined) repo.description = description;
  if (typeof body.private === 'boolean') repo.isPrivate = body.private;
  const defaultBranch = stringField(body, 'default_branch');
  if (defaultBranch) repo.defaultBranch = defaultBranch;
  repo.updatedEpoch = tickEpochSec();
  return jsonReply(200, repoJson(ctx, repo));
}

// ---- contents

function contentsGet(ctx: ReqCtx, repo: RepoRec, path: string): Reply {
  const refParam = ctx.query.get('ref') ?? repo.defaultBranch;
  const tip = resolveRef(repo, refParam);
  if (!tip) throw notFound('Not Found');
  const tree = treeOfCommit(repo, tip);
  if (!tree) throw notFound('Not Found');
  const store = repo.store;
  const base = `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/contents`;
  if (path !== '') {
    // Walk to the entry.
    let treeOid = tree;
    const segments = path.split('/');
    for (let index = 0; index < segments.length; index += 1) {
      const treeObject = store.get(treeOid);
      if (!treeObject || treeObject.type !== 'tree') throw notFound('Not Found');
      const entry = parseTree(treeObject.data).find((candidate) => candidate.name === segments[index]);
      if (!entry) throw notFound('Not Found');
      if (index === segments.length - 1) {
        if (entry.mode === '40000') return jsonReply(200, dirListing(ctx, repo, entry.oid, path));
        const blob = store.get(entry.oid);
        if (!blob) throw notFound('Not Found');
        return jsonReply(200, {
          name: entry.name,
          path,
          sha: entry.oid,
          size: blob.data.length,
          url: `${base}/${path}?ref=${encodeURIComponent(refParam)}`,
          html_url: `${ctx.origin}/${repo.owner}/${repo.name}/blob/${encodeURIComponent(refParam)}/${path}`,
          git_url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/blobs/${entry.oid}`,
          download_url: `${ctx.origin}/${repo.owner}/${repo.name}/raw/${encodeURIComponent(refParam)}/${path}`,
          type: 'file',
          content: blob.data.toString('base64').replace(/.{60}(?=.)/g, '$&\n'),
          encoding: 'base64',
          _links: { self: `${base}/${path}?ref=${encodeURIComponent(refParam)}`, git: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/blobs/${entry.oid}`, html: `${ctx.origin}/${repo.owner}/${repo.name}/blob/${encodeURIComponent(refParam)}/${path}` },
        });
      }
      if (entry.mode !== '40000') throw notFound('Not Found');
      treeOid = entry.oid;
    }
  }
  return jsonReply(200, dirListing(ctx, repo, tree, ''));
}

function dirListing(ctx: ReqCtx, repo: RepoRec, treeOid: string, prefix: string) {
  const tree = repo.store.get(treeOid);
  if (!tree || tree.type !== 'tree') throw notFound('Not Found');
  const base = `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/contents`;
  return parseTree(tree.data).map((entry) => {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    const isDir = entry.mode === '40000';
    const blob = isDir ? undefined : repo.store.get(entry.oid);
    return {
      name: entry.name,
      path,
      sha: entry.oid,
      size: isDir ? 0 : blob?.data.length ?? 0,
      url: `${base}/${path}`,
      html_url: `${ctx.origin}/${repo.owner}/${repo.name}/${isDir ? 'tree' : 'blob'}/${repo.defaultBranch}/${path}`,
      git_url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/${isDir ? 'trees' : 'blobs'}/${entry.oid}`,
      download_url: isDir ? null : `${ctx.origin}/${repo.owner}/${repo.name}/raw/${repo.defaultBranch}/${path}`,
      type: isDir ? 'dir' : 'file',
      _links: { self: `${base}/${path}`, git: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/${isDir ? 'trees' : 'blobs'}/${entry.oid}`, html: `${ctx.origin}/${repo.owner}/${repo.name}/${isDir ? 'tree' : 'blob'}/${repo.defaultBranch}/${path}` },
    };
  });
}

function contentsPut(ctx: ReqCtx, repo: RepoRec, path: string): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const message = stringField(body, 'message') ?? 'Update file';
  const contentB64 = stringField(body, 'content');
  if (contentB64 === undefined) throw new ApiError(422, 'Validation Failed: content is required');
  const content = Buffer.from(contentB64.replace(/\s+/g, ''), 'base64');
  const result = commitFileChange(repo, {
    branch: stringField(body, 'branch'),
    path,
    content,
    message,
    sha: stringField(body, 'sha'),
  });
  const commit = repo.store.get(result.commitSha);
  const view = commit && commit.type === 'commit' ? parseCommit(commit.data) : null;
  return jsonReply(result.created ? 201 : 200, {
    content: {
      name: path.split('/').pop() ?? path,
      path,
      sha: result.blobSha,
      size: content.length,
      url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/contents/${path}`,
      html_url: `${ctx.origin}/${repo.owner}/${repo.name}/blob/${stringField(body, 'branch') ?? repo.defaultBranch}/${path}`,
      git_url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/blobs/${result.blobSha}`,
      download_url: `${ctx.origin}/${repo.owner}/${repo.name}/raw/${repo.defaultBranch}/${path}`,
      type: 'file',
      encoding: 'base64',
    },
    commit: {
      sha: result.commitSha,
      node_id: nodeId('06:Commit', repo.id),
      url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/commits/${result.commitSha}`,
      html_url: `${ctx.origin}/${repo.owner}/${repo.name}/commit/${result.commitSha}`,
      author: { name: VIEWER.name, email: VIEWER.email, date: isoOf(repo.updatedEpoch) },
      committer: { name: VIEWER.name, email: VIEWER.email, date: isoOf(repo.updatedEpoch) },
      tree: { sha: view?.tree ?? '', url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/trees/${view?.tree ?? ''}` },
      message,
      parents: (view?.parents ?? []).map((sha) => ({ sha, url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/commits/${sha}`, html_url: `${ctx.origin}/${repo.owner}/${repo.name}/commit/${sha}` })),
    },
  });
}

function contentsDelete(ctx: ReqCtx, repo: RepoRec, path: string): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const result = commitFileChange(repo, {
    branch: stringField(body, 'branch'),
    path,
    content: null,
    message: stringField(body, 'message') ?? 'Delete file',
    sha: stringField(body, 'sha'),
  });
  return jsonReply(200, {
    content: null,
    commit: {
      sha: result.commitSha,
      url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/commits/${result.commitSha}`,
      message: stringField(body, 'message') ?? 'Delete file',
    },
  });
}

// ---- branches, commits, refs

function branchesList(ctx: ReqCtx, repo: RepoRec): Reply {
  const branches = [...repo.refs.entries()]
    .filter(([ref]) => ref.startsWith('refs/heads/'))
    .map(([ref, sha]) => ({
      name: ref.slice('refs/heads/'.length),
      commit: { sha, url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/commits/${sha}` },
      protected: false,
    }))
    .sort((a, b) => (a.name < b.name ? -1 : 1));
  return jsonReply(200, branches);
}

function branchGet(ctx: ReqCtx, repo: RepoRec, name: string): Reply {
  const sha = branchTip(repo, name);
  if (!sha) throw notFound('Not Found');
  return jsonReply(200, {
    name,
    commit: {
      sha,
      node_id: nodeId('06:Commit', repo.id),
      url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/commits/${sha}`,
      html_url: `${ctx.origin}/${repo.owner}/${repo.name}/commit/${sha}`,
    },
    protected: false,
    _links: { self: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/branches/${name}`, html: `${ctx.origin}/${repo.owner}/${repo.name}/branches/${name}` },
  });
}

function identParts(line: string): { name: string; email: string; date: string } {
  const match = /^(.*) <(.*)> (\d+) [+-]\d{4}$/.exec(line);
  if (!match) return { name: line, email: '', date: isoOf(0) };
  return { name: match[1]!, email: match[2]!, date: isoOf(Number(match[3])) };
}

function commitJson(ctx: ReqCtx, repo: RepoRec, sha: string) {
  const object = repo.store.get(sha);
  if (!object || object.type !== 'commit') throw notFound('Not Found');
  const view = parseCommit(object.data);
  const base = `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}`;
  return {
    sha,
    node_id: nodeId('06:Commit', repo.id),
    commit: {
      author: identParts(view.author),
      committer: identParts(view.committer),
      message: view.message.replace(/\n$/, ''),
      tree: { sha: view.tree, url: `${base}/git/trees/${view.tree}` },
      url: `${base}/git/commits/${sha}`,
      comment_count: 0,
    },
    url: `${base}/commits/${sha}`,
    html_url: `${ctx.origin}/${repo.owner}/${repo.name}/commit/${sha}`,
    comments_url: `${base}/commits/${sha}/comments`,
    author: userJson(ctx),
    committer: userJson(ctx),
    parents: view.parents.map((parent) => ({ sha: parent, url: `${base}/commits/${parent}`, html_url: `${ctx.origin}/${repo.owner}/${repo.name}/commit/${parent}` })),
  };
}

function commitsList(ctx: ReqCtx, repo: RepoRec): Reply {
  const refParam = ctx.query.get('sha') ?? repo.defaultBranch;
  const tip = resolveRef(repo, refParam);
  if (!tip) return jsonReply(200, []);
  const page = paginate(ctx, listCommits(repo.store, tip), `${API_ROOT}/repos/${repo.owner}/${repo.name}/commits`);
  return pageReply(ctx, page, (sha) => commitJson(ctx, repo, sha));
}

function createRef(ctx: ReqCtx, repo: RepoRec): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const ref = stringField(body, 'ref');
  const sha = stringField(body, 'sha');
  if (!ref || !ref.startsWith('refs/')) throw new ApiError(422, 'Validation Failed: ref is invalid');
  if (!sha || !repo.store.has(sha.toLowerCase())) throw new ApiError(422, 'Validation Failed: sha does not name an object');
  if (repo.refs.has(ref)) throw new ApiError(422, 'Reference already exists');
  repo.refs.set(ref, sha.toLowerCase());
  const object = repo.store.get(sha.toLowerCase())!;
  return jsonReply(201, {
    ref,
    node_id: nodeId('07:Ref', repo.id),
    url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/${ref}`,
    object: { type: object.type, sha: sha.toLowerCase(), url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/${object.type === 'commit' ? 'commits' : 'objects'}/${sha.toLowerCase()}` },
  });
}

function getRef(ctx: ReqCtx, repo: RepoRec, ref: string): Reply {
  const sha = repo.refs.get(ref);
  if (!sha) throw notFound('Not Found');
  const object = repo.store.get(sha);
  return jsonReply(200, {
    ref,
    node_id: nodeId('07:Ref', repo.id),
    url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/${ref}`,
    object: { type: object?.type ?? 'commit', sha, url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/commits/${sha}` },
  });
}

// ---- labels, issues, comments

function createLabel(ctx: ReqCtx, repo: RepoRec): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const name = stringField(body, 'name');
  if (!name) throw new ApiError(422, 'Validation Failed: name is required');
  if (repo.labels.has(name)) throw new ApiError(422, 'Validation Failed: a label with that name already exists');
  repo.labels.set(name, { name, color: (stringField(body, 'color') ?? 'ededed').replace(/^#/, ''), description: stringField(body, 'description') ?? null });
  return jsonReply(201, labelJson(ctx, repo, name));
}

function labelNamesOf(body: Record<string, unknown>): string[] {
  const raw = body.labels;
  if (!Array.isArray(raw)) return [];
  const names: string[] = [];
  for (const entry of raw) {
    if (typeof entry === 'string') names.push(entry);
    else if (isRecord(entry) && typeof entry.name === 'string') names.push(entry.name);
  }
  return names;
}

function createIssue(ctx: ReqCtx, repo: RepoRec): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const title = stringField(body, 'title');
  if (!title) throw new ApiError(422, 'Validation Failed: title is required');
  const labels = labelNamesOf(body);
  for (const name of labels) {
    if (!repo.labels.has(name)) throw new ApiError(422, `Validation Failed: label "${name}" does not exist`);
  }
  const epoch = tickEpochSec();
  const issue: IssueRec = {
    id: ctx.forge.allocObjectId(),
    number: repo.nextNumber++,
    title,
    body: stringField(body, 'body') ?? '',
    state: 'open',
    labels,
    createdEpoch: epoch,
    updatedEpoch: epoch,
    comments: [],
  };
  repo.issues.set(issue.number, issue);
  return jsonReply(201, issueJson(ctx, repo, issue));
}

function listIssues(ctx: ReqCtx, repo: RepoRec): Reply {
  const state = ctx.query.get('state') ?? 'open';
  let issues = [...repo.issues.values()];
  if (state === 'open' || state === 'closed') issues = issues.filter((issue) => issue.state === state);
  const direction = ctx.query.get('direction') ?? 'desc';
  issues.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
  const page = paginate(ctx, issues, `${API_ROOT}/repos/${repo.owner}/${repo.name}/issues`);
  return pageReply(ctx, page, (issue) => issueJson(ctx, repo, issue));
}

function requireIssue(repo: RepoRec, number: number): IssueRec {
  const issue = repo.issues.get(number);
  if (!issue) throw notFound('Not Found');
  return issue;
}

function patchIssue(ctx: ReqCtx, repo: RepoRec, number: number): Reply {
  const issue = requireIssue(repo, number);
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const title = stringField(body, 'title');
  if (title !== undefined) issue.title = title;
  const text = stringField(body, 'body');
  if (text !== undefined) issue.body = text;
  const state = stringField(body, 'state');
  if (state === 'closed' || state === 'open') issue.state = state;
  if (Array.isArray(body.labels)) {
    const labels = labelNamesOf(body);
    for (const name of labels) if (!repo.labels.has(name)) throw new ApiError(422, `Validation Failed: label "${name}" does not exist`);
    issue.labels = labels;
  }
  issue.updatedEpoch = tickEpochSec();
  return jsonReply(200, issueJson(ctx, repo, issue));
}

function createComment(ctx: ReqCtx, repo: RepoRec, number: number): Reply {
  const issue = requireIssue(repo, number);
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const text = stringField(body, 'body');
  if (text === undefined) throw new ApiError(422, 'Validation Failed: body is required');
  const comment = { id: ctx.forge.allocCommentId(), body: text, createdEpoch: tickEpochSec() };
  issue.comments.push(comment);
  return jsonReply(201, commentJson(ctx, repo, issue, comment));
}

// ---- pulls

function createPull(ctx: ReqCtx, repo: RepoRec): Reply {
  const body = bodyObject(ctx);
  if (!body) throw new ApiError(400, 'Problems parsing JSON');
  const title = stringField(body, 'title');
  const head = stringField(body, 'head');
  const base = stringField(body, 'base');
  if (!title || !head || !base) throw new ApiError(422, 'Validation Failed: title, head and base are required');
  const pull = openPull(ctx.forge, repo, { title, body: stringField(body, 'body') ?? '', head, base });
  return jsonReply(201, pullJson(ctx, repo, pull));
}

function listPulls(ctx: ReqCtx, repo: RepoRec): Reply {
  const state = ctx.query.get('state') ?? 'open';
  let pulls = [...repo.pulls.values()];
  if (state === 'open' || state === 'closed') pulls = pulls.filter((pull) => pull.state === state);
  const direction = ctx.query.get('direction') ?? 'desc';
  pulls.sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
  const page = paginate(ctx, pulls, `${API_ROOT}/repos/${repo.owner}/${repo.name}/pulls`);
  return pageReply(ctx, page, (pull) => pullJson(ctx, repo, pull));
}

function requirePull(repo: RepoRec, number: number): PullRec {
  const pull = repo.pulls.get(number);
  if (!pull) throw notFound('Not Found');
  return pull;
}

function pullFiles(ctx: ReqCtx, repo: RepoRec, number: number): Reply {
  const pull = requirePull(repo, number);
  const { files } = pullAnalysis(repo, pull);
  const page = paginate(ctx, files, `${API_ROOT}/repos/${repo.owner}/${repo.name}/pulls/${number}/files`);
  return pageReply(ctx, page, (file) => ({
    sha: file.sha,
    filename: file.filename,
    status: file.status,
    additions: file.additions,
    deletions: file.deletions,
    changes: file.changes,
    blob_url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/blobs/${file.sha ?? ''}`,
    raw_url: `${ctx.origin}/${repo.owner}/${repo.name}/raw/${pull.headRef}/${file.filename}`,
    contents_url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/contents/${file.filename}?ref=${encodeURIComponent(pull.headRef)}`,
  }));
}

function mergePull(ctx: ReqCtx, repo: RepoRec, number: number): Reply {
  const pull = requirePull(repo, number);
  const body = bodyObject(ctx);
  const method = body ? stringField(body, 'merge_method') ?? 'merge' : 'merge';
  if (pull.state !== 'open') throw new ApiError(405, 'Pull Request is not mergeable');
  if (method === 'merge') {
    // A true merge commit: the three-way tree, both parents.
    const analysis = pullAnalysis(repo, pull);
    if (analysis.conflict || analysis.base === null) throw new ApiError(405, 'Conflict: the pull request has conflicts');
    const headTip = repo.refs.get(`refs/heads/${pull.headRef}`)!;
    const baseTip = repo.refs.get(`refs/heads/${pull.baseRef}`)!;
    const baseTree = treeOfCommit(repo, analysis.base)!;
    const headTree = treeOfCommit(repo, headTip)!;
    const ontoTree = treeOfCommit(repo, baseTip)!;
    const merged = threeWayMerge(repo.store, baseTree, headTree, ontoTree);
    if (!merged.ok) throw new ApiError(405, 'Conflict: the pull request has conflicts');
    const epoch = tickEpochSec();
    const sha = repo.store.put('commit', buildCommitData({ tree: merged.tree, parents: [baseTip, headTip], message: `Merge pull request #${pull.number} from ${pull.headRef}\n\n${pull.title}`, ident: viewerIdent(epoch) }));
    repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedEpoch = epoch;
    pull.mergeCommitSha = sha;
    pull.updatedEpoch = epoch;
    return jsonReply(200, { sha, merged: true, message: 'Pull Request successfully merged' });
  }
  const { sha } = squashMerge(repo, pull);
  return jsonReply(200, { sha, merged: true, message: 'Pull Request successfully merged' });
}

// -------------------------------------------------------------------------------------------
// the router

export function handleRest(ctx: ReqCtx): Reply {
  try {
    return route(ctx);
  } catch (err) {
    if (err instanceof ApiError) return messageReply(err.status, err.message);
    throw err;
  }
}

function route(ctx: ReqCtx): Reply {
  if (!ctx.authorized) return messageReply(401, 'Bad credentials');
  const segments = pathSegments(ctx.path.slice(API_ROOT.length));
  const method = ctx.method;

  if (segments.length === 0) {
    if (method === 'GET') {
      return {
        status: 200,
        contentType: 'application/json; charset=utf-8',
        body: Buffer.from(JSON.stringify(rootJson(ctx)), 'utf8'),
        headers: { 'x-oauth-scopes': 'repo, read:org', 'x-accepted-oauth-scopes': 'repo, read:org' },
      };
    }
    throw notFound();
  }
  if (segments[0] === 'meta' && segments.length === 1) {
    if (method === 'GET') return jsonReply(200, { installed_version: '1.0.0', hooks_url: `${ctx.origin}${API_ROOT}/hooks`, issue_search_url: `${ctx.origin}${API_ROOT}/search/issues`, repository_search_url: `${ctx.origin}${API_ROOT}/search/repositories`, code_search_url: `${ctx.origin}${API_ROOT}/search/code`, commit_search_url: `${ctx.origin}${API_ROOT}/search/commits`, user_search_url: `${ctx.origin}${API_ROOT}/search/users`, verifiable_password_authentication: false, ssh_key_fingerprints: {}, ssh_keys: [], api_url: `${ctx.origin}${API_ROOT}/` });
    throw notFound();
  }
  if (segments[0] === 'user' && segments.length === 1) {
    if (method === 'GET') return jsonReply(200, { ...userJson(ctx), name: VIEWER.name, email: VIEWER.email, plan: { name: 'forge', space: 0, private_repos: 0, collaborators: 0 } });
    throw notFound();
  }
  if (segments[0] === 'user' && segments[1] === 'repos' && segments.length === 2) {
    if (method === 'GET') {
      const page = paginate(ctx, ctx.forge.viewerRepos(), `${API_ROOT}/user/repos`);
      return pageReply(ctx, page, (repo) => repoJson(ctx, repo));
    }
    if (method === 'POST') {
      // A malformed JSON body is a 400 before anything else.
      if (bodyJson(ctx) === undefined && ctx.body.length > 0) return messageReply(400, 'Problems parsing JSON');
      return createRepo(ctx);
    }
    throw notFound();
  }
  if (segments[0] === 'repos' && segments.length >= 3) {
    const owner = segments[1]!;
    const name = segments[2]!;
    const rest = segments.slice(3);
    if (rest.length === 0) {
      if (method === 'GET') return jsonReply(200, repoJson(ctx, requireRepo(ctx, owner, name)));
      if (method === 'PATCH') return patchRepo(ctx, requireRepo(ctx, owner, name));
      if (method === 'DELETE') {
        if (!ctx.forge.deleteRepo(owner, name)) throw notFound();
        return { status: 204, contentType: 'application/json; charset=utf-8', body: Buffer.alloc(0) };
      }
      throw notFound();
    }
    const repo = requireRepo(ctx, owner, name);
    return routeRepo(ctx, repo, rest);
  }
  // A malformed JSON body on a body-carrying route still answers 400.
  if ((method === 'POST' || method === 'PUT' || method === 'PATCH') && ctx.body.length > 0 && bodyJson(ctx) === undefined) {
    return messageReply(400, 'Problems parsing JSON');
  }
  throw notFound();
}

function routeRepo(ctx: ReqCtx, repo: RepoRec, rest: string[]): Reply {
  const method = ctx.method;
  const head = rest[0]!;
  if (head === 'issues') {
    if (rest.length === 1) {
      if (method === 'GET') return listIssues(ctx, repo);
      if (method === 'POST') return createIssue(ctx, repo);
    } else if (rest.length === 2) {
      const number = Number(rest[1]);
      if (method === 'GET') return jsonReply(200, issueJson(ctx, repo, requireIssue(repo, number)));
      if (method === 'PATCH') return patchIssue(ctx, repo, number);
    } else if (rest.length === 3 && rest[2] === 'comments') {
      const number = Number(rest[1]);
      if (method === 'GET') {
        const issue = requireIssue(repo, number);
        return jsonReply(200, issue.comments.map((comment) => commentJson(ctx, repo, issue, comment)));
      }
      if (method === 'POST') return createComment(ctx, repo, number);
    }
    throw notFound();
  }
  if (head === 'labels') {
    if (rest.length === 1) {
      if (method === 'POST') return createLabel(ctx, repo);
      if (method === 'GET') return jsonReply(200, [...repo.labels.keys()].map((name) => labelJson(ctx, repo, name)));
    }
    throw notFound();
  }
  if (head === 'pulls') {
    if (rest.length === 1) {
      if (method === 'GET') return listPulls(ctx, repo);
      if (method === 'POST') return createPull(ctx, repo);
    } else if (rest.length === 2) {
      const number = Number(rest[1]);
      if (method === 'GET') return jsonReply(200, pullJson(ctx, repo, requirePull(repo, number)));
    } else if (rest.length === 3 && rest[2] === 'files') {
      if (method === 'GET') return pullFiles(ctx, repo, Number(rest[1]));
    } else if (rest.length === 3 && rest[2] === 'merge') {
      if (method === 'PUT') return mergePull(ctx, repo, Number(rest[1]));
    }
    throw notFound();
  }
  if (head === 'branches') {
    if (rest.length === 1 && method === 'GET') return branchesList(ctx, repo);
    if (rest.length >= 2 && method === 'GET') return branchGet(ctx, repo, rest.slice(1).join('/'));
    throw notFound();
  }
  if (head === 'commits') {
    if (rest.length === 1 && method === 'GET') return commitsList(ctx, repo);
    if (rest.length === 2 && method === 'GET') return jsonReply(200, commitJson(ctx, repo, rest[1]!.toLowerCase()));
    throw notFound();
  }
  if (head === 'contents') {
    const path = rest.slice(1).join('/');
    if (method === 'GET') return contentsGet(ctx, repo, path);
    if (method === 'PUT') return contentsPut(ctx, repo, path);
    if (method === 'DELETE') return contentsDelete(ctx, repo, path);
    throw notFound();
  }
  if (head === 'git' && rest[1] === 'refs') {
    if (rest.length === 2) {
      if (method === 'POST') return createRef(ctx, repo);
      if (method === 'GET') {
        return jsonReply(200, [...repo.refs.entries()].map(([ref, sha]) => ({ ref, node_id: nodeId('07:Ref', repo.id), url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/${ref}`, object: { type: repo.store.get(sha)?.type ?? 'commit', sha, url: `${ctx.origin}${API_ROOT}/repos/${repo.owner}/${repo.name}/git/commits/${sha}` } })));
      }
    }
    if (rest.length >= 3 && method === 'GET') return getRef(ctx, repo, rest.slice(1).join('/'));
    throw notFound();
  }
  throw notFound();
}
