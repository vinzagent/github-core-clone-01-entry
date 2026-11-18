// gitproto.ts — git smart HTTP, hand-rolled: ref advertisement (protocol v0
// and v2), ls-refs, fetch (v2 sections and the v0 sideband form, with shallow
// deepen support) and receive-pack pushes with report-status. No git binary:
// the object store, pack reader/writer and delta code live in forge.ts.

import {
  ZERO_OID,
  collectReachable,
  headRef,
  isAncestor,
  parseCommit,
  parseTag,
  peelToCommit,
  readPackInto,
  writePack,
  type GitObject,
  type RepoRec,
} from './forge.ts';
import { jsonReply, type Reply, type ReqCtx } from './http.ts';

const AGENT = 'vinz-forge/1.0';

// -------------------------------------------------------------------------------------------
// pkt-lines

const PKT_MAX_PAYLOAD = 65516;

export function pktLine(payload: Buffer | string): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const header = Buffer.from((data.length + 4).toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, data]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

type Packet =
  | { readonly kind: 'data'; readonly payload: Buffer }
  | { readonly kind: 'flush' }
  | { readonly kind: 'delim' }
  | { readonly kind: 'end' };

/** Parse pkt-lines from `offset`; returns the packets and the unconsumed tail. */
function parsePacketStream(buffer: Buffer, offset = 0): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let cursor = offset;
  while (cursor + 4 <= buffer.length) {
    const header = buffer.subarray(cursor, cursor + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(header)) break;
    const length = Number.parseInt(header, 16);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      cursor += 4;
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      cursor += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'end' });
      cursor += 4;
      continue;
    }
    if (length < 4 || cursor + length > buffer.length) break;
    packets.push({ kind: 'data', payload: buffer.subarray(cursor + 4, cursor + length) });
    cursor += length;
  }
  return { packets, rest: buffer.subarray(cursor) };
}

/**
 * Split a request body into argument groups: protocol v2 separates the
 * command/capability group from the argument group with a delim packet and
 * ends with a flush; protocol v0 separates its two groups with flushes.
 * Either way the groups come back in order, data payloads only.
 */
function parseGroups(buffer: Buffer): Buffer[][] {
  const { packets } = parsePacketStream(buffer);
  const groups: Buffer[][] = [[]];
  for (const packet of packets) {
    if (packet.kind === 'data') groups[groups.length - 1]!.push(packet.payload);
    else if (packet.kind === 'delim' || packet.kind === 'flush') groups.push([]);
  }
  while (groups.length > 1 && groups[groups.length - 1]!.length === 0) groups.pop();
  return groups;
}

function linesOf(packets: readonly Buffer[]): string[] {
  return packets.map((packet) => packet.toString('utf8').replace(/\n$/, ''));
}

// -------------------------------------------------------------------------------------------
// advertisement

interface RefLine {
  oid: string;
  name: string;
  peeled?: string;
}

function sortedRefLines(repo: RepoRec): RefLine[] {
  const lines: RefLine[] = [];
  const names = [...repo.refs.keys()].sort();
  for (const name of names) {
    const oid = repo.refs.get(name)!;
    lines.push({ oid, name });
    const object = repo.store.get(oid);
    if (object && object.type === 'tag') {
      lines.push({ oid: peelToCommit(repo.store, oid), name: `${name}^{}` });
    }
  }
  return lines;
}

function uploadCaps(repo: RepoRec): string {
  return [
    'multi_ack_detailed',
    'side-band-64k',
    'thin-pack',
    'ofs-delta',
    'shallow',
    'deepen-since',
    'deepen-not',
    `agent=${AGENT}`,
    `symref=HEAD:${headRef(repo)}`,
  ].join(' ');
}

function receiveCaps(repo: RepoRec): string {
  return ['report-status', 'side-band-64k', 'delete-refs', 'ofs-delta', `agent=${AGENT}`, `symref=HEAD:${headRef(repo)}`].join(' ');
}

function advertiseV0(repo: RepoRec, service: string): Reply {
  const caps = service === 'git-receive-pack' ? receiveCaps(repo) : uploadCaps(repo);
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const head = headRef(repo);
  const headTip = repo.refs.get(head);
  const refLines = sortedRefLines(repo);
  let capsPending = true;
  const emit = (text: string) => {
    if (capsPending) {
      parts.push(pktLine(`${text}\0${caps}`));
      capsPending = false;
    } else {
      parts.push(pktLine(text));
    }
  };
  if (headTip) emit(`${headTip} HEAD`);
  else if (refLines.length) emit(`${ZERO_OID} HEAD`);
  for (const line of refLines) emit(`${line.oid} ${line.name}`);
  if (capsPending) parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}`));
  parts.push(PKT_FLUSH);
  return {
    status: 200,
    contentType: `application/x-${service}-advertisement`,
    body: Buffer.concat(parts),
    headers: { 'cache-control': 'no-cache, max-age=0, must-revalidate' },
  };
}

function advertiseV2(service: string): Reply {
  const parts: Buffer[] = [
    pktLine('version 2\n'),
    pktLine(`agent=${AGENT}\n`),
    pktLine('ls-refs=unborn\n'),
    pktLine('fetch=shallow wait-for-done\n'),
    pktLine('object-format=sha1\n'),
    PKT_FLUSH,
  ];
  return {
    status: 200,
    contentType: `application/x-${service}-advertisement`,
    body: Buffer.concat(parts),
    headers: { 'cache-control': 'no-cache, max-age=0, must-revalidate' },
  };
}

// -------------------------------------------------------------------------------------------
// ls-refs

function lsRefs(repo: RepoRec, groups: Buffer[][]): Reply {
  const args = linesOf(groups[1] ?? []);
  const wantPeel = args.includes('peel');
  const wantSymrefs = args.includes('symrefs');
  const wantUnborn = args.includes('unborn');
  const prefixes = args.filter((line) => line.startsWith('ref-prefix ')).map((line) => line.slice('ref-prefix '.length));
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const parts: Buffer[] = [];
  const head = headRef(repo);
  const headTip = repo.refs.get(head);
  if (matches('HEAD')) {
    if (headTip) {
      parts.push(pktLine(`${headTip} HEAD${wantSymrefs ? ` symref-target:${head}` : ''}\n`));
    } else if (wantUnborn) {
      parts.push(pktLine(`unborn HEAD${wantSymrefs ? ` symref-target:${head}` : ''}\n`));
    }
  }
  for (const name of [...repo.refs.keys()].sort()) {
    if (!matches(name)) continue;
    const oid = repo.refs.get(name)!;
    let suffix = '';
    if (wantPeel) {
      const object = repo.store.get(oid);
      if (object && object.type === 'tag') suffix = ` peeled:${peelToCommit(repo.store, oid)}`;
    }
    parts.push(pktLine(`${oid} ${name}${suffix}\n`));
  }
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

// -------------------------------------------------------------------------------------------
// fetch

interface FetchPlan {
  send: GitObject[];
  shallow: string[];
  unshallow: string[];
  ack: string | null;
}

/** The objects of one commit's tree closure (the commit, its trees and blobs — no parents). */
function treeClosure(repo: RepoRec, commitOid: string, into: Set<string>): void {
  const commit = repo.store.get(commitOid);
  if (!commit || commit.type !== 'commit') return;
  into.add(commitOid);
  const tree = parseCommit(commit.data).tree;
  for (const oid of collectReachable(repo.store, [tree], { allowMissing: true })) into.add(oid);
}

function planFetch(repo: RepoRec, wants: string[], haves: string[], shallowLines: string[], depth: number | null): FetchPlan {
  const store = repo.store;
  const shallowSet = new Set(shallowLines);
  const held = new Set<string>();
  for (const have of haves) {
    const object = store.get(have);
    if (!object) continue;
    if (shallowSet.has(have) && object.type === 'commit') {
      treeClosure(repo, have, held);
    } else {
      for (const oid of collectReachable(store, [have], { allowMissing: true })) held.add(oid);
    }
  }

  let ack: string | null = null;
  for (const have of haves) if (store.has(have)) ack = have;

  const sendOids = new Set<string>();
  const newShallow: string[] = [];
  const unshallow: string[] = [];

  if (depth !== null) {
    // Breadth-first by generation: a want is generation 1.
    const generation = new Map<string, number>();
    const queue: Array<{ oid: string; gen: number }> = [];
    for (const want of wants) if (store.has(want)) queue.push({ oid: want, gen: 1 });
    const included = new Set<string>();
    while (queue.length) {
      const { oid, gen } = queue.shift()!;
      if (generation.has(oid) || gen > depth) continue;
      generation.set(oid, gen);
      included.add(oid);
      const object = store.get(oid);
      if (object && object.type === 'commit') {
        for (const parent of parseCommit(object.data).parents) queue.push({ oid: parent, gen: gen + 1 });
      }
    }
    const closure = new Set<string>();
    for (const oid of included) {
      const object = store.get(oid);
      if (!object) continue;
      if (object.type === 'commit') treeClosure(repo, oid, closure);
      else for (const sub of collectReachable(store, [oid], { allowMissing: true })) closure.add(sub);
    }
    for (const oid of closure) if (!held.has(oid)) sendOids.add(oid);
    for (const oid of included) {
      const object = store.get(oid);
      if (!object || object.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      const boundary = parents.some((parent) => store.has(parent) && !included.has(parent));
      if (boundary && !held.has(oid)) newShallow.push(oid);
      else if (boundary && held.has(oid) && shallowSet.has(oid)) newShallow.push(oid);
      if (shallowSet.has(oid) && !boundary) unshallow.push(oid);
    }
  } else {
    const wanted = collectReachable(store, wants.filter((oid) => store.has(oid)), { allowMissing: true });
    for (const oid of wanted) if (!held.has(oid)) sendOids.add(oid);
  }

  const send: GitObject[] = [];
  for (const oid of sendOids) {
    const object = store.get(oid);
    if (object) send.push(object);
  }
  return { send, shallow: newShallow, unshallow, ack };
}

/** Chunk a pack into sideband band-1 pkt-lines. */
function sidebandPackets(pack: Buffer): Buffer[] {
  const parts: Buffer[] = [];
  const chunkSize = PKT_MAX_PAYLOAD - 1;
  for (let offset = 0; offset < pack.length; offset += chunkSize) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + chunkSize));
    parts.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  if (pack.length === 0) parts.push(pktLine(Buffer.from([1])));
  return parts;
}

function fetchV2(repo: RepoRec, groups: Buffer[][]): Reply {
  const args = linesOf(groups[1] ?? []);
  const wants: string[] = [];
  const haves: string[] = [];
  const shallowLines: string[] = [];
  let depth: number | null = null;
  for (const line of args) {
    if (line.startsWith('want ')) wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) shallowLines.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
  }
  const plan = planFetch(repo, wants, haves, shallowLines, depth);
  const parts: Buffer[] = [];
  parts.push(pktLine('acknowledgments\n'));
  parts.push(pktLine(plan.ack ? `ACK ${plan.ack}\n` : 'NAK\n'));
  parts.push(PKT_FLUSH);
  if (plan.shallow.length || plan.unshallow.length) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(PKT_FLUSH);
  }
  parts.push(pktLine('packfile\n'));
  parts.push(...sidebandPackets(writePack(plan.send)));
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

function fetchV0(repo: RepoRec, groups: Buffer[][]): Reply {
  const first = linesOf(groups[0] ?? []);
  const second = linesOf(groups[1] ?? []);
  const wants: string[] = [];
  const shallowLines: string[] = [];
  let depth: number | null = null;
  for (const line of first) {
    if (line.startsWith('want ')) wants.push(line.slice(5).trim().split(' ')[0]!);
    else if (line.startsWith('shallow ')) shallowLines.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7).trim());
  }
  const haves: string[] = [];
  for (const line of second) {
    if (line.startsWith('have ')) haves.push(line.slice(5).trim());
  }
  const plan = planFetch(repo, wants, haves, shallowLines, depth);
  const parts: Buffer[] = [];
  for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  parts.push(pktLine(plan.ack ? `ACK ${plan.ack}\n` : 'NAK\n'));
  parts.push(...sidebandPackets(writePack(plan.send)));
  parts.push(PKT_FLUSH);
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

function uploadPack(repo: RepoRec, ctx: ReqCtx): Reply {
  const groups = parseGroups(ctx.body);
  const commandLine = linesOf(groups[0] ?? []).find((line) => line.startsWith('command='));
  const command = commandLine?.slice('command='.length);
  if (command === 'ls-refs') return lsRefs(repo, groups);
  if (command === 'fetch') return fetchV2(repo, groups);
  // No command line: a protocol v0 fetch negotiation.
  return fetchV0(repo, groups);
}

// -------------------------------------------------------------------------------------------
// receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

function receivePack(repo: RepoRec, ctx: ReqCtx): Reply {
  // Command pkt-lines run to the first flush; everything after it is the pack.
  const { packets } = parsePacketStream(ctx.body);
  let commandBytes = 0;
  {
    let cursor = 0;
    for (const packet of packets) {
      if (packet.kind === 'data') cursor += 4 + packet.payload.length;
      else {
        cursor += 4;
        if (packet.kind === 'flush') break;
      }
    }
    commandBytes = cursor;
  }
  const commandPayloads: Buffer[] = [];
  for (const packet of packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind === 'data') commandPayloads.push(packet.payload);
  }
  const rawLines = linesOf(commandPayloads);
  const commands: PushCommand[] = [];
  let capabilities = '';
  rawLines.forEach((line, index) => {
    let text = line;
    if (index === 0) {
      const nul = text.indexOf('\0');
      if (nul >= 0) {
        capabilities = text.slice(nul + 1);
        text = text.slice(0, nul);
      }
    }
    const parts = text.split(' ');
    if (parts.length >= 3 && /^[0-9a-f]{40}$/.test(parts[0]!) && /^[0-9a-f]{40}$/.test(parts[1]!)) {
      commands.push({ oldOid: parts[0]!.toLowerCase(), newOid: parts[1]!.toLowerCase(), ref: parts.slice(2).join(' ') });
    }
  });
  const sideband = capabilities.split(' ').includes('side-band-64k');

  const report: Buffer[] = [];
  const pack = ctx.body.subarray(commandBytes);
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (pack.length > 0) {
    try {
      readPackInto(repo.store, pack);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'index-pack failed';
    }
  }
  report.push(pktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`));

  if (unpackOk) {
    for (const command of commands) {
      const verdict = applyPushCommand(repo, command);
      report.push(pktLine(verdict.ok ? `ok ${command.ref}\n` : `ng ${command.ref} ${verdict.reason}\n`));
    }
  }
  report.push(PKT_FLUSH);
  const reportStream = Buffer.concat(report);

  if (sideband) {
    const parts: Buffer[] = [];
    const chunkSize = PKT_MAX_PAYLOAD - 1;
    for (let offset = 0; offset < reportStream.length; offset += chunkSize) {
      const chunk = reportStream.subarray(offset, Math.min(reportStream.length, offset + chunkSize));
      parts.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
    }
    parts.push(PKT_FLUSH);
    return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat(parts) };
  }
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body: reportStream };
}

/** Apply one ref update; the verdict is an ok flag plus the refusal reason. */
function applyPushCommand(repo: RepoRec, command: PushCommand): { ok: boolean; reason: string } {
  const store = repo.store;
  const current = repo.refs.get(command.ref);
  if (command.newOid === ZERO_OID) {
    // A deletion.
    if (current === undefined) return { ok: false, reason: 'reference not found' };
    if (command.ref === headRef(repo)) return { ok: false, reason: 'deletion of the current branch prohibited' };
    if (current !== command.oldOid) return { ok: false, reason: 'stale info' };
    repo.refs.delete(command.ref);
    return { ok: true, reason: '' };
  }
  const object = store.get(command.newOid);
  if (!object) return { ok: false, reason: 'invalid object' };
  if (command.oldOid === ZERO_OID) {
    if (current !== undefined) return { ok: false, reason: 'already exists' };
    repo.refs.set(command.ref, command.newOid);
    return { ok: true, reason: '' };
  }
  if (current !== command.oldOid) return { ok: false, reason: 'stale info' };
  if (command.ref.startsWith('refs/heads/')) {
    const oldObject = store.get(command.oldOid);
    if (oldObject && oldObject.type === 'commit' && object.type === 'commit') {
      if (!isAncestor(store, command.oldOid, command.newOid)) return { ok: false, reason: 'non-fast-forward' };
    }
  }
  repo.refs.set(command.ref, command.newOid);
  return { ok: true, reason: '' };
}

// -------------------------------------------------------------------------------------------
// the route

export function handleGit(ctx: ReqCtx, owner: string, repoName: string, sub: string): Reply {
  if (!ctx.authorized) return jsonReply(401, { message: 'Bad credentials' });
  const repo = ctx.forge.getRepo(owner, repoName);
  if (!repo) {
    return { status: 404, contentType: 'text/plain; charset=utf-8', body: Buffer.from('not found\n', 'utf8') };
  }
  if (sub === 'info/refs') {
    const service = ctx.query.get('service');
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      return { status: 400, contentType: 'text/plain; charset=utf-8', body: Buffer.from('missing service\n', 'utf8') };
    }
    const protocol = ctx.headers['git-protocol'] ?? '';
    if (service === 'git-upload-pack' && /(?:^|,)version=2(?:,|$)/.test(protocol)) return advertiseV2(service);
    return advertiseV0(repo, service);
  }
  if (sub === 'git-upload-pack' && ctx.method === 'POST') return uploadPack(repo, ctx);
  if (sub === 'git-receive-pack' && ctx.method === 'POST') return receivePack(repo, ctx);
  return { status: 404, contentType: 'text/plain; charset=utf-8', body: Buffer.from('not found\n', 'utf8') };
}
