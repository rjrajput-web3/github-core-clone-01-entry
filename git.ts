// git.ts — git smart HTTP: protocol v2 upload-pack (ls-refs, fetch), protocol v0
// upload-pack, and protocol v0 receive-pack. No git binary: packs and pkt-lines are
// built and parsed here, on the store in store.ts.

import {
  ZERO_OID,
  buildPack,
  decodePktLines,
  isOid,
  parseTag,
  pktDelim,
  pktFlush,
  pktLine,
  readPack,
  type GitObject,
  type Repo,
  type Store,
} from './store.ts';

const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
}

function ok(contentType: string, body: Buffer): GitResponse {
  return { status: 200, contentType, body };
}

function notFound(): GitResponse {
  return { status: 404, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'Not Found' })) };
}

const UPLOAD_V0_CAPS = 'side-band-64k shallow thin-pack ofs-delta multi_ack multi_ack_detailed symref=HEAD:refs/heads/main agent=forge/1.0';
const RECEIVE_V0_CAPS = 'report-status side-band-64k delete-refs ofs-delta agent=forge/1.0';

/** Refs sorted the way an advertisement lists them: HEAD first, then heads, then tags. */
function sortedRefNames(repo: Repo): string[] {
  const names = [...repo.refs.keys()];
  names.sort((a, b) => {
    const rank = (n: string): number => (n.startsWith('refs/heads/') ? 0 : n.startsWith('refs/tags/') ? 1 : 2);
    const ra = rank(a);
    const rb = rank(b);
    if (ra !== rb) return ra - rb;
    return a < b ? -1 : a > b ? 1 : 0;
  });
  return names;
}

/** The peeled (commit) target of a ref, when it names an annotated tag object. */
function peeledOf(repo: Repo, oid: string): string | undefined {
  const o = repo.getObject(oid);
  if (!o || o.type !== 'tag') return undefined;
  const target = parseTag(o.data).object;
  const t = repo.getObject(target);
  if (t && t.type === 'commit') return target;
  return undefined;
}

function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_V0_CAPS : RECEIVE_V0_CAPS;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), pktFlush];
  const names = sortedRefNames(repo);
  if (names.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    const head = repo.tip('refs/heads/main') ?? repo.tip(`refs/heads/${repo.defaultBranch}`) ?? names.map((n) => repo.tip(n)).find((o) => o !== undefined);
    if (head !== undefined) parts.push(pktLine(`${head} HEAD\0${caps}\n`));
    else parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    for (const name of names) {
      const oid = repo.tip(name)!;
      parts.push(pktLine(`${oid} ${name}\n`));
      const peeled = peeledOf(repo, oid);
      if (peeled !== undefined) parts.push(pktLine(`${peeled} ${name}^{}\n`));
    }
  }
  parts.push(pktFlush);
  return Buffer.concat(parts);
}

function v2Advertisement(): Buffer {
  return Buffer.concat([
    pktLine('version 2\n'),
    pktLine('ls-refs\n'),
    pktLine('fetch=shallow thin-pack ofs-delta filter\n'),
    pktLine('object-format=sha1\n'),
    pktLine('agent=forge/1.0\n'),
    pktFlush,
  ]);
}

/** GET /{owner}/{repo}.git/info/refs?service=... */
export function infoRefs(store: Store, owner: string, repoName: string, service: string, wantV2: boolean): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  if (service === 'git-upload-pack') {
    if (wantV2) return ok(UPLOAD_AD, v2Advertisement());
    return ok(UPLOAD_AD, v0Advertisement(repo, 'git-upload-pack'));
  }
  if (service === 'git-receive-pack') {
    return ok(RECEIVE_AD, v0Advertisement(repo, 'git-receive-pack'));
  }
  return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'unknown service' })) };
}

// -------------------------------------------------------------------------------------------
// upload-pack
// -------------------------------------------------------------------------------------------

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth?: number;
}

function parseFetchArgs(lines: string[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [] };
  for (const raw of lines) {
    // v0 carries capabilities after a NUL (old style) or space-separated
    // (modern git) on the first want line; the oid is the first token.
    const t = raw.replace(/\n$/, '').split('\0')[0]!;
    if (t.startsWith('want ')) args.wants.push(t.slice(5).trim().split(/\s/)[0]!);
    else if (t.startsWith('have ')) args.haves.push(t.slice(5).trim().split(/\s/)[0]!);
    else if (t.startsWith('shallow ')) args.shallow.push(t.slice(8).trim().split(/\s/)[0]!);
    else if (t.startsWith('deepen ')) {
      const n = Number(t.slice(7).trim());
      if (Number.isFinite(n) && n > 0) args.depth = Math.trunc(n);
    }
  }
  return args;
}

/** Objects to send for a fetch: reachable from wants (depth-limited), minus haves' closure. */
function computeFetch(
  repo: Repo,
  args: FetchArgs,
): { objects: GitObject[]; shallow: string[]; unshallow: string[]; acks: string[] } {
  // Commit distances from the wants (tags peel to their target at distance 0).
  const dist = new Map<string, number>();
  const queue: Array<{ oid: string; d: number }> = [];
  const wantCommits: string[] = [];
  for (const w of args.wants) {
    if (!isOid(w)) continue;
    const o = repo.getObject(w);
    if (!o) continue;
    if (o.type === 'tag') {
      const target = parseTag(o.data).object;
      if (repo.getObject(target)?.type === 'commit' && !dist.has(target)) {
        dist.set(target, 0);
        queue.push({ oid: target, d: 0 });
      }
      wantCommits.push(target);
    } else if (o.type === 'commit') {
      if (!dist.has(w)) {
        dist.set(w, 0);
        queue.push({ oid: w, d: 0 });
      }
      wantCommits.push(w);
    }
  }
  const maxDepth = args.depth ?? Infinity;
  while (queue.length) {
    const cur = queue.shift()!;
    if (cur.d + 1 >= maxDepth) continue;
    const c = repo.commitOf(cur.oid);
    if (!c) continue;
    for (const p of c.parents) {
      if (!dist.has(p)) {
        dist.set(p, cur.d + 1);
        queue.push({ oid: p, d: cur.d + 1 });
      }
    }
  }
  const included = new Set<string>(dist.keys());
  // Objects of the included commits only — each commit's own tree+blobs, never
  // its ancestors (a depth-1 fetch must carry the tip only).
  const includedObjects = new Set<string>();
  for (const oid of included) {
    for (const o of repo.commitObjects(oid)) includedObjects.add(o);
  }
  for (const w of args.wants) {
    if (repo.getObject(w)) includedObjects.add(w);
  }
  // Everything the client already holds. A shallow client holds its boundary
  // commits but not their parents, so the walk must not cross shallow lines.
  const held = repo.reachableObjectsBounded(args.haves.filter((h) => isOid(h)), args.shallow);
  for (const h of args.haves) if (repo.getObject(h)) held.add(h);
  const send = new Set<string>();
  for (const oid of includedObjects) if (!held.has(oid)) send.add(oid);

  const shallow: string[] = [];
  if (args.depth !== undefined) {
    for (const [oid, d] of dist) {
      if (d !== args.depth - 1) continue;
      const c = repo.commitOf(oid);
      if (c && c.parents.length > 0) shallow.push(oid);
    }
  }
  const unshallow: string[] = [];
  for (const b of args.shallow) {
    if (included.has(b) && !shallow.includes(b)) unshallow.push(b);
  }
  const acks: string[] = [];
  if (args.haves.length > 0 && args.haves.every((h) => repo.getObject(h) !== undefined)) {
    acks.push(...args.haves);
  }
  const objects: GitObject[] = [];
  for (const oid of send) {
    const o = repo.getObject(oid);
    if (o) objects.push(o);
  }
  // Deterministic pack order: commits, then tags, then trees, then blobs; oid order inside.
  const rank = (t: string): number => (t === 'commit' ? 0 : t === 'tag' ? 1 : t === 'tree' ? 2 : 3);
  objects.sort((a, b) => {
    const ra = rank(a.type);
    const rb = rank(b.type);
    if (ra !== rb) return ra - rb;
    return 0;
  });
  return { objects, shallow, unshallow, acks };
}

/** Sideband-64k framing of a pack: pkt-lines of "\x01" + up to 65515 pack bytes. */
function sidebandPack(pack: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const CHUNK = 65515;
  for (let off = 0; off < pack.length; off += CHUNK) {
    const slice = pack.subarray(off, Math.min(pack.length, off + CHUNK));
    out.push(pktLine(Buffer.concat([Buffer.from([1]), slice])));
  }
  return out;
}

function v2FetchResponse(repo: Repo, args: FetchArgs): Buffer {
  // Stateless RPC (what git and the suite drive): the shallow-info and
  // packfile sections are always present (the pack may be empty). An
  // acknowledgments section is sent only when the client sent haves — git
  // expects it then, and dies if it appears when no haves were sent.
  const { objects, shallow, unshallow, acks } = computeFetch(repo, args);
  const parts: Buffer[] = [];
  if (args.haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    if (acks.length) {
      for (const a of acks) parts.push(pktLine(`ACK ${a}\n`));
      parts.push(pktLine('ready\n'));
    } else {
      parts.push(pktLine('NAK\n'));
    }
    parts.push(pktDelim);
  }
  if (shallow.length || unshallow.length) {
    parts.push(pktLine('shallow-info\n'));
    for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
    for (const s of unshallow) parts.push(pktLine(`unshallow ${s}\n`));
    parts.push(pktDelim);
  }
  const pack = buildPack(objects);
  parts.push(pktLine('packfile\n'));
  parts.push(...sidebandPack(pack));
  parts.push(pktFlush);
  return Buffer.concat(parts);
}

function v0FetchResponse(repo: Repo, args: FetchArgs): Buffer {
  const { objects, shallow, unshallow, acks } = computeFetch(repo, args);
  const parts: Buffer[] = [];
  if (acks.length) {
    for (const a of acks) parts.push(pktLine(`ACK ${a}\n`));
  } else {
    parts.push(pktLine('NAK\n'));
  }
  for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
  for (const s of unshallow) parts.push(pktLine(`unshallow ${s}\n`));
  const pack = buildPack(objects);
  parts.push(...sidebandPack(pack));
  parts.push(pktFlush);
  return Buffer.concat(parts);
}

function v2LsRefsResponse(repo: Repo, args: { prefixes: string[]; peel: boolean; symrefs: boolean }): Buffer {
  const parts: Buffer[] = [];
  const headTip = repo.tip(`refs/heads/${repo.defaultBranch}`);
  const matches = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((p) => name.startsWith(p));
  if (headTip !== undefined && matches('HEAD')) {
    const extra = args.symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : '';
    parts.push(pktLine(`${headTip} HEAD${extra}\n`));
  }
  for (const name of sortedRefNames(repo)) {
    if (!matches(name)) continue;
    const oid = repo.tip(name)!;
    let extra = '';
    if (args.peel) {
      const peeled = peeledOf(repo, oid);
      if (peeled !== undefined) extra += ` peeled:${peeled}`;
    }
    parts.push(pktLine(`${oid} ${name}${extra}\n`));
  }
  parts.push(pktFlush);
  return Buffer.concat(parts);
}

/** POST /{owner}/{repo}.git/git-upload-pack */
export function uploadPack(store: Store, owner: string, repoName: string, body: Buffer, wantV2: boolean): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  const { packets } = decodePktLines(body);
  const lines: string[] = [];
  let sawDelim = false;
  const afterDelim: string[] = [];
  for (const p of packets) {
    if (p.kind === 'delim') {
      sawDelim = true;
      continue;
    }
    if (p.kind === 'flush' || p.kind === 'end') break;
    if (p.kind !== 'data') continue;
    const text = p.payload.toString('utf8');
    if (sawDelim) afterDelim.push(text);
    else lines.push(text);
  }
  if (wantV2) {
    const command = (lines[0] ?? '').replace(/\n$/, '');
    if (command === 'command=ls-refs') {
      const prefixes: string[] = [];
      let peel = false;
      let symrefs = false;
      for (const raw of afterDelim) {
        const t = raw.replace(/\n$/, '');
        if (t === 'peel') peel = true;
        else if (t === 'symrefs') symrefs = true;
        else if (t.startsWith('ref-prefix ')) prefixes.push(t.slice('ref-prefix '.length));
      }
      return ok(UPLOAD_RESULT, v2LsRefsResponse(repo, { prefixes, peel, symrefs }));
    }
    if (command === 'command=fetch') {
      return ok(UPLOAD_RESULT, v2FetchResponse(repo, parseFetchArgs(afterDelim)));
    }
    return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'unknown v2 command' })) };
  }
  // v0: two sections split by a flush. Re-decode section by section.
  const sections: string[][] = [[]];
  let current = 0;
  for (const p of packets) {
    if (p.kind === 'flush') {
      current += 1;
      sections[current] = [];
      continue;
    }
    if (p.kind !== 'data') continue;
    sections[current]!.push(p.payload.toString('utf8'));
  }
  const args = parseFetchArgs([...(sections[0] ?? []), ...(sections[1] ?? [])]);
  return ok(UPLOAD_RESULT, v0FetchResponse(repo, args));
}

// -------------------------------------------------------------------------------------------
// receive-pack
// -------------------------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** POST /{owner}/{repo}.git/git-receive-pack */
export function receivePack(store: Store, owner: string, repoName: string, body: Buffer): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return notFound();
  const { packets, offset } = decodePktLines(body);
  const commands: PushCommand[] = [];
  let sawFlush = false;
  for (const p of packets) {
    if (p.kind === 'flush') {
      sawFlush = true;
      break;
    }
    if (p.kind !== 'data') continue;
    let text = p.payload.toString('utf8').replace(/\n$/, '');
    const nul = text.indexOf('\0');
    if (nul >= 0) text = text.slice(0, nul);
    const parts = text.split(' ');
    if (parts.length >= 3) {
      commands.push({ oldOid: parts[0]!, newOid: parts[1]!, ref: parts[2]! });
    }
  }
  const reportLines: string[] = [];
  let unpackOk = true;
  let unpackDetail = 'ok';
  const packBytes = sawFlush ? body.subarray(offset) : Buffer.alloc(0);
  if (packBytes.length > 0) {
    let parsed = false;
    for (const candidate of [packBytes, packBytes.subarray(0, Math.max(0, packBytes.length - 4))]) {
      if (candidate.length === 0) break;
      try {
        const objects = readPack(candidate, (oid) => repo.getObject(oid));
        for (const [oid, o] of objects) {
          if (!repo.getObject(oid)) repo.objects.set(oid, o);
        }
        parsed = true;
        break;
      } catch {
        // A trailing flush packet after the pack breaks the trailer check;
        // retry once without it.
      }
    }
    if (!parsed) {
      unpackOk = false;
      unpackDetail = 'unpacker error';
    }
  }
  if (commands.length === 0) {
    unpackOk = false;
    unpackDetail = 'unpacker error';
  }
  for (const cmd of commands) {
    if (!unpackOk) {
      reportLines.push(`ng ${cmd.ref} unpacker error`);
      continue;
    }
    const current = repo.tip(cmd.ref) ?? ZERO_OID;
    if (cmd.newOid === ZERO_OID) {
      // Delete.
      if (current === ZERO_OID) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
      } else if (cmd.oldOid !== ZERO_OID && cmd.oldOid !== current) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
      } else {
        repo.setRef(cmd.ref, ZERO_OID);
        reportLines.push(`ok ${cmd.ref}`);
      }
      continue;
    }
    const target = repo.getObject(cmd.newOid);
    if (!target || (target.type !== 'commit' && target.type !== 'tag')) {
      reportLines.push(`ng ${cmd.ref} missing object`);
      continue;
    }
    if (current !== ZERO_OID && cmd.oldOid !== current) {
      reportLines.push(`ng ${cmd.ref} non-fast-forward`);
      continue;
    }
    if (current === ZERO_OID && cmd.oldOid !== ZERO_OID) {
      reportLines.push(`ng ${cmd.ref} non-fast-forward`);
      continue;
    }
    if (current !== ZERO_OID) {
      // Fast-forward: old tip must be an ancestor of the new tip.
      const newCommit = target.type === 'tag' ? parseTag(target.data).object : cmd.newOid;
      if (!repo.isAncestor(current, newCommit)) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
    }
    repo.setRef(cmd.ref, cmd.newOid);
    reportLines.push(`ok ${cmd.ref}`);
  }
  const inner = Buffer.concat([
    pktLine(`unpack ${unpackOk ? 'ok' : unpackDetail}\n`),
    ...reportLines.map((l) => pktLine(`${l}\n`)),
    // The report-status stream inside band 1 ends with its own flush packet;
    // without it git's packet reader hits EOF and dies with "the remote end
    // hung up unexpectedly".
    pktFlush,
  ]);
  // The sideband stream ends with a flush, and the stateless-RPC response
  // ends with a second flush; git waits for both.
  const outer = Buffer.concat([pktLine(Buffer.concat([Buffer.from([1]), inner])), pktFlush, pktFlush]);
  return ok(RECEIVE_RESULT, outer);
}
