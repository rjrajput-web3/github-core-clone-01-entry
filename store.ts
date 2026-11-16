// store.ts — the forge's in-memory store: git object primitives (hashing, packfiles,
// pkt-lines, reachability) and the repository/issue/pull-request data model.
//
// Everything lives in memory: no disk, no database, no git binary. Object ids are
// SHA-1 over "<type> <size>\0" + bytes, exactly as git computes them.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0'.repeat(40);
export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjType;
  data: Buffer;
}

const TYPE_CODE: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };
const HEX40 = /^[0-9a-f]{40}$/;

export function isOid(v: string): boolean {
  return HEX40.test(v);
}

export function hashObject(type: ObjType, data: Buffer): string {
  const h = createHash('sha1');
  h.update(Buffer.from(`${type} ${data.length}\0`, 'utf8'));
  h.update(data);
  return h.digest('hex');
}

export interface CommitView {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): CommitView {
  const blank = data.indexOf('\n\n');
  const head = blank < 0 ? data.toString('utf8') : data.subarray(0, blank).toString('utf8');
  const message = blank < 0 ? '' : data.subarray(blank + 2).toString('utf8');
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    if (sp < 0) continue;
    const k = line.slice(0, sp);
    const v = line.slice(sp + 1);
    if (k === 'tree') tree = v;
    else if (k === 'parent') parents.push(v);
    else if (k === 'author') author = v;
    else if (k === 'committer') committer = v;
  }
  return { tree, parents, author, committer, message };
}

export function buildCommit(o: {
  tree: string;
  parents?: string[];
  author: string;
  committer: string;
  timestamp: number;
  message: string;
}): Buffer {
  const lines = [`tree ${o.tree}`];
  for (const p of o.parents ?? []) lines.push(`parent ${p}`);
  lines.push(`author ${o.author} ${o.timestamp} +0000`);
  lines.push(`committer ${o.committer} ${o.timestamp} +0000`);
  lines.push('');
  return Buffer.from(`${lines.join('\n')}\n${o.message}`, 'utf8');
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const out: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    const nul = data.indexOf(0x00, sp + 1);
    if (sp < 0 || nul < 0 || nul + 21 > data.length) throw new Error('corrupt tree object');
    out.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return out;
}

function treeCompare(a: TreeEntry, b: TreeEntry): number {
  const l = Buffer.from(a.mode === '40000' ? `${a.name}/` : a.name, 'utf8');
  const r = Buffer.from(b.mode === '40000' ? `${b.name}/` : b.name, 'utf8');
  return Buffer.compare(l, r);
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort(treeCompare);
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface TagView {
  object: string;
  type: ObjType;
  tag: string;
  tagger: string;
  message: string;
}

export function parseTag(data: Buffer): TagView {
  const blank = data.indexOf('\n\n');
  const head = blank < 0 ? data.toString('utf8') : data.subarray(0, blank).toString('utf8');
  const headers = new Map<string, string>();
  for (const line of head.split('\n')) {
    const sp = line.indexOf(' ');
    if (sp >= 0) headers.set(line.slice(0, sp), line.slice(sp + 1));
  }
  return {
    object: headers.get('object') ?? '',
    type: (headers.get('type') ?? 'commit') as ObjType,
    tag: headers.get('tag') ?? '',
    tagger: headers.get('tagger') ?? '',
    message: blank < 0 ? '' : data.subarray(blank + 2).toString('utf8'),
  };
}

// -------------------------------------------------------------------------------------------
// pkt-line framing
// -------------------------------------------------------------------------------------------

export function pktLine(payload: string | Buffer): Buffer {
  const data = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.alloc(4);
  head.write((data.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, data]);
}

export const pktFlush = Buffer.from('0000', 'ascii');
export const pktDelim = Buffer.from('0001', 'ascii');

export type Pkt = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Decode pkt-lines from the start of `buf`; returns packets and the offset just past them. */
export function decodePktLines(buf: Buffer, start = 0): { packets: Pkt[]; offset: number } {
  const packets: Pkt[] = [];
  let off = start;
  while (off + 4 <= buf.length) {
    const head = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(head)) break;
    const len = parseInt(head, 16);
    if (len === 0) {
      packets.push({ kind: 'flush' });
      off += 4;
      continue;
    }
    if (len === 1) {
      packets.push({ kind: 'delim' });
      off += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'end' });
      off += 4;
      continue;
    }
    if (len < 4 || off + len > buf.length) break;
    packets.push({ kind: 'data', payload: buf.subarray(off + 4, off + len) });
    off += len;
  }
  return { packets, offset: off };
}

// -------------------------------------------------------------------------------------------
// packfiles: plain objects out, plain + delta objects in
// -------------------------------------------------------------------------------------------

function packObjectHeader(type: ObjType, size: number): Buffer {
  const code = TYPE_CODE[type];
  const bytes: number[] = [];
  let rem = size;
  let first = (code << 4) | (rem & 0x0f);
  rem = Math.floor(rem / 16);
  while (rem > 0) {
    bytes.push(first | 0x80);
    first = rem & 0x7f;
    rem = Math.floor(rem / 128);
  }
  bytes.push(first);
  return Buffer.from(bytes);
}

export function buildPack(objects: GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 4, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(objects.length, 8);
  parts.push(head);
  for (const o of objects) {
    parts.push(packObjectHeader(o.type, o.data.length));
    parts.push(deflateSync(o.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

function readVarint(buf: Buffer, start: number): { size: number; next: number; first: number } {
  let byte = buf[start]!;
  const first = byte;
  let size = byte & 0x0f;
  let shift = 4;
  let off = start + 1;
  while (byte & 0x80) {
    byte = buf[off++]!;
    size += (byte & 0x7f) * 2 ** shift;
    shift += 7;
  }
  return { size, next: off, first };
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readSize = (): number => {
    let v = 0;
    let shift = 0;
    for (;;) {
      const b = delta[off++]!;
      v += (b & 0x7f) * 2 ** shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    return v;
  };
  const srcSize = readSize();
  const tgtSize = readSize();
  if (srcSize !== base.length) throw new Error('delta base size mismatch');
  const out = Buffer.allocUnsafe(tgtSize);
  let written = 0;
  while (off < delta.length) {
    const op = delta[off++]!;
    if (op & 0x80) {
      let cpOff = 0;
      let cpSize = 0;
      let shift = 0;
      for (let bit = 0; bit < 4; bit++) {
        if (op & (1 << bit)) cpOff += delta[off++]! * 2 ** shift;
        shift += 8;
      }
      shift = 0;
      for (let bit = 4; bit < 7; bit++) {
        if (op & (1 << bit)) cpSize += delta[off++]! * 2 ** shift;
        shift += 8;
      }
      if (cpSize === 0) cpSize = 0x10000;
      base.copy(out, written, cpOff, cpOff + cpSize);
      written += cpSize;
    } else if (op !== 0) {
      const n = op & 0x7f;
      delta.copy(out, written, off, off + n);
      off += n;
      written += n;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (written !== tgtSize) throw new Error('delta size mismatch');
  return out;
}

/** Read a packfile (v2/v3), resolving OFS/REF deltas; returns oid -> object. */
export function readPack(pack: Buffer, resolveBase?: (oid: string) => GitObject | undefined): Map<string, GitObject> {
  if (pack.length < 32 || pack.subarray(0, 4).toString('ascii') !== 'PACK') {
    throw new Error('not a packfile');
  }
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`unsupported pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailerStart = pack.length - 20;
  const trailer = pack.subarray(trailerStart).toString('hex');
  const computed = createHash('sha1').update(pack.subarray(0, trailerStart)).digest('hex');
  if (trailer !== computed) throw new Error('pack trailer checksum mismatch');

  interface Raw {
    offset: number;
    typeCode: number;
    size: number;
    data: Buffer;
    baseOffset?: number;
    baseOid?: string;
  }
  const raw: Raw[] = [];
  const byOffset = new Map<number, Raw>();
  let off = 12;
  for (let i = 0; i < count; i++) {
    const entryOff = off;
    const h = readVarint(pack, off);
    off = h.next;
    const typeCode = (h.first >> 4) & 0x07;
    const e: Raw = { offset: entryOff, typeCode, size: h.size, data: Buffer.alloc(0) };
    if (typeCode === 6) {
      let byte = pack[off]!;
      off += 1;
      let v = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[off++]!;
        v = (v + 1) * 128 + (byte & 0x7f);
      }
      e.baseOffset = entryOff - v;
    } else if (typeCode === 7) {
      e.baseOid = pack.subarray(off, off + 20).toString('hex');
      off += 20;
    } else if (!CODE_TYPE[typeCode]) {
      throw new Error(`unknown pack type code ${typeCode}`);
    }
    const inflated = inflateSync(pack.subarray(off, trailerStart), { info: true }) as unknown as {
      buffer: Buffer;
      engine: { bytesWritten: number };
    };
    if (inflated.buffer.length !== h.size) throw new Error('pack entry size mismatch');
    e.data = inflated.buffer;
    off += inflated.engine.bytesWritten;
    raw.push(e);
    byOffset.set(entryOff, e);
  }

  const objects = new Map<string, GitObject>();
  const resolvedByOid = new Map<string, Raw>();
  const resolving = new Set<number>();
  const resolve = (e: Raw): { type: ObjType; data: Buffer; oid: string } => {
    const cached = (e as unknown as { _r?: { type: ObjType; data: Buffer; oid: string } })._r;
    if (cached) return cached;
    if (resolving.has(e.offset)) throw new Error('delta cycle');
    resolving.add(e.offset);
    let type: ObjType;
    let data: Buffer;
    if (e.typeCode === 6) {
      const base = byOffset.get(e.baseOffset!)!;
      if (!base) throw new Error('ofs-delta base missing');
      const b = resolve(base);
      type = b.type;
      data = applyDelta(b.data, e.data);
    } else if (e.typeCode === 7) {
      const baseEntry = resolvedByOid.get(e.baseOid!);
      if (baseEntry) {
        const b = resolve(baseEntry);
        type = b.type;
        data = applyDelta(b.data, e.data);
      } else {
        const ext = resolveBase?.(e.baseOid!);
        if (!ext) throw new Error(`ref-delta base ${e.baseOid} not available`);
        type = ext.type;
        data = applyDelta(ext.data, e.data);
      }
    } else {
      type = CODE_TYPE[e.typeCode]!;
      data = e.data;
    }
    const oid = hashObject(type, data);
    const r = { type, data, oid };
    (e as unknown as { _r: unknown })._r = r;
    resolvedByOid.set(oid, e);
    resolving.delete(e.offset);
    return r;
  };
  for (const e of raw) {
    const r = resolve(e);
    if (!objects.has(r.oid)) objects.set(r.oid, { type: r.type, data: r.data });
  }
  return objects;
}

// -------------------------------------------------------------------------------------------
// data model
// -------------------------------------------------------------------------------------------

export interface Comment {
  id: number;
  body: string;
  seq: number;
  createdAt: string;
}

export interface Issue {
  number: number;
  dbId: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Pull {
  number: number;
  dbId: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  seq: number;
  createdAt: string;
  updatedAt: string;
}

export interface Label {
  name: string;
  color: string;
  description: string;
}

export const VIEWER = Object.freeze({
  login: 'adaiversen',
  id: 1000,
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
});

export class Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  isPrivate: boolean;
  defaultBranch = 'main';
  seq: number;
  refs = new Map<string, string>();
  objects = new Map<string, GitObject>();
  private commitCache = new Map<string, CommitView>();
  private treeCache = new Map<string, TreeEntry[]>();
  /** ref -> set of commit oids reachable from the ref tip (for fast-forward checks). */
  ancestors = new Map<string, Set<string>>();
  issues = new Map<number, Issue>();
  pulls = new Map<number, Pull>();
  labels = new Map<string, Label>();
  nextNumber = 1;
  private nextCommentId = 1;

  constructor(id: number, owner: string, name: string, description: string, isPrivate: boolean, seq: number) {
    this.id = id;
    this.owner = owner;
    this.name = name;
    this.description = description;
    this.isPrivate = isPrivate;
    this.seq = seq;
  }

  get fullName(): string {
    return `${this.owner}/${this.name}`;
  }

  tip(ref: string): string | undefined {
    return this.refs.get(ref);
  }

  defaultTip(): string | undefined {
    return this.refs.get(`refs/heads/${this.defaultBranch}`);
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  putObject(type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) {
      this.objects.set(oid, { type, data });
      if (type === 'commit') this.commitCache.delete(oid);
      if (type === 'tree') this.treeCache.delete(oid);
    }
    return oid;
  }

  commitOf(oid: string): CommitView | undefined {
    const o = this.objects.get(oid);
    if (!o || o.type !== 'commit') return undefined;
    let c = this.commitCache.get(oid);
    if (!c) {
      c = parseCommit(o.data);
      this.commitCache.set(oid, c);
    }
    return c;
  }

  treeOf(oid: string): TreeEntry[] | undefined {
    const o = this.objects.get(oid);
    if (!o || o.type !== 'tree') return undefined;
    let t = this.treeCache.get(oid);
    if (!t) {
      t = parseTree(o.data);
      this.treeCache.set(oid, t);
    }
    return t;
  }

  /** All commit oids reachable from `roots` (gitlinks skipped). */
  reachableCommits(roots: Iterable<string>): Set<string> {
    const seen = new Set<string>();
    const stack: string[] = [];
    for (const r of roots) if (isOid(r) && r !== ZERO_OID) stack.push(r);
    while (stack.length) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      const c = this.commitOf(oid);
      if (!c) continue;
      seen.add(oid);
      for (const p of c.parents) stack.push(p);
    }
    return seen;
  }

  /** Every object reachable from `roots` (commits, trees, blobs, tags). */
  reachableObjects(roots: Iterable<string>): Set<string> {
    const seen = new Set<string>();
    const stack: string[] = [];
    for (const r of roots) if (isOid(r) && r !== ZERO_OID) stack.push(r);
    while (stack.length) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      const o = this.objects.get(oid);
      if (!o) continue;
      seen.add(oid);
      if (o.type === 'commit') {
        const c = this.commitOf(oid)!;
        stack.push(c.tree);
        for (const p of c.parents) stack.push(p);
      } else if (o.type === 'tag') {
        stack.push(parseTag(o.data).object);
      } else if (o.type === 'tree') {
        for (const e of this.treeOf(oid)!) if (e.mode !== '160000') stack.push(e.oid);
      }
    }
    return seen;
  }

  /** Like reachableObjects, but the walk never crosses the parents of commits
   * in `shallow` — a shallow clone holds the boundary commit but not its history. */
  reachableObjectsBounded(roots: Iterable<string>, shallow: Iterable<string>): Set<string> {
    const shallowSet = new Set(shallow);
    const seen = new Set<string>();
    const stack: string[] = [];
    for (const r of roots) if (isOid(r) && r !== ZERO_OID) stack.push(r);
    while (stack.length) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      const o = this.objects.get(oid);
      if (!o) continue;
      seen.add(oid);
      if (o.type === 'commit') {
        const c = this.commitOf(oid)!;
        stack.push(c.tree);
        if (!shallowSet.has(oid)) for (const p of c.parents) stack.push(p);
      } else if (o.type === 'tag') {
        stack.push(parseTag(o.data).object);
      } else if (o.type === 'tree') {
        for (const e of this.treeOf(oid)!) if (e.mode !== '160000') stack.push(e.oid);
      }
    }
    return seen;
  }
  commitObjects(commitOid: string): Set<string> {
    const seen = new Set<string>();
    const stack: string[] = [commitOid];
    while (stack.length) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      const o = this.objects.get(oid);
      if (!o) continue;
      seen.add(oid);
      if (o.type === 'commit') {
        stack.push(this.commitOf(oid)!.tree);
      } else if (o.type === 'tag') {
        stack.push(parseTag(o.data).object);
      } else if (o.type === 'tree') {
        for (const e of this.treeOf(oid)!) if (e.mode !== '160000') stack.push(e.oid);
      }
    }
    return seen;
  }

  /** Is `ancestor` an ancestor of (or equal to) `oid`? */
  isAncestor(ancestor: string, oid: string): boolean {
    if (ancestor === oid) return true;
    if (ancestor === ZERO_OID) return true;
    const seen = new Set<string>();
    const stack = [oid];
    while (stack.length) {
      const cur = stack.pop()!;
      if (cur === ancestor) return true;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const c = this.commitOf(cur);
      if (!c) continue;
      for (const p of c.parents) stack.push(p);
    }
    return false;
  }

  /** Set a ref, maintaining its ancestor set. */
  setRef(ref: string, oid: string): void {
    if (oid === ZERO_OID) {
      this.refs.delete(ref);
      this.ancestors.delete(ref);
      return;
    }
    const oldAnc = this.ancestors.get(ref) ?? new Set<string>();
    const next = new Set<string>(oldAnc);
    const stack = [oid];
    while (stack.length) {
      const cur = stack.pop()!;
      if (next.has(cur)) continue;
      const c = this.commitOf(cur);
      if (!c) {
        if (cur === oid) {
          // Non-commit tip (e.g. a tag object): the tag itself is the only member.
          next.add(cur);
        }
        continue;
      }
      next.add(cur);
      for (const p of c.parents) stack.push(p);
    }
    this.refs.set(ref, oid);
    this.ancestors.set(ref, next);
  }

  /** First common ancestor of two commits (newest-first walk). */
  mergeBase(a: string, b: string): string | null {
    const bAnc = this.reachableCommits([b]);
    const queue: Array<{ oid: string; depth: number }> = [{ oid: a, depth: 0 }];
    const seen = new Set<string>();
    let best: string | null = null;
    let bestDepth = Infinity;
    while (queue.length) {
      queue.sort((x, y) => x.depth - y.depth);
      const cur = queue.shift()!;
      if (seen.has(cur.oid)) continue;
      seen.add(cur.oid);
      if (bAnc.has(cur.oid)) {
        if (cur.depth < bestDepth) {
          best = cur.oid;
          bestDepth = cur.depth;
        }
        continue;
      }
      const c = this.commitOf(cur.oid);
      if (!c) continue;
      for (const p of c.parents) queue.push({ oid: p, depth: cur.depth + 1 });
    }
    return best;
  }

  /** Flatten a tree (at a commit) to path -> {oid, mode}. */
  flattenTree(treeOid: string): Map<string, { oid: string; mode: string }> {
    const out = new Map<string, { oid: string; mode: string }>();
    const walk = (oid: string, prefix: string): void => {
      const entries = this.treeOf(oid);
      if (!entries) return;
      for (const e of entries) {
        const path = prefix ? `${prefix}/${e.name}` : e.name;
        if (e.mode === '40000') walk(e.oid, path);
        else out.set(path, { oid: e.oid, mode: e.mode });
      }
    };
    walk(treeOid, '');
    return out;
  }

  /** Build (and store) a tree from a flat path map; returns the root tree oid. */
  treeFromFlat(flat: Map<string, { oid: string; mode: string }>): string {
    interface Node {
      files: Map<string, { oid: string; mode: string }>;
      dirs: Map<string, Node>;
    }
    const make = (): Node => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, entry] of flat) {
      const segs = path.split('/');
      let node = root;
      for (let i = 0; i < segs.length - 1; i++) {
        const s = segs[i]!;
        if (!node.dirs.has(s)) node.dirs.set(s, make());
        node = node.dirs.get(s)!;
      }
      node.files.set(segs[segs.length - 1]!, entry);
    }
    const build = (node: Node): string => {
      const entries: TreeEntry[] = [];
      for (const [name, e] of node.files) entries.push({ mode: e.mode, name, oid: e.oid });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      return this.putObject('tree', buildTree(entries));
    };
    return build(root);
  }

  /** The file map of a commit (empty when the commit is missing). */
  filesOfCommit(commitOid: string): Map<string, { oid: string; mode: string }> {
    const c = this.commitOf(commitOid);
    if (!c) return new Map();
    return this.flattenTree(c.tree);
  }

  nextComment(): number {
    return this.nextCommentId++;
  }
}

export class Store {
  repos = new Map<string, Repo>();
  private nextDbId = 1;
  private nextSeq = 1;
  private readonly baseMs: number;

  constructor() {
    const trialNow = Number(process.env.MAKEFUN_TRIAL_NOW ?? '');
    this.baseMs = Number.isFinite(trialNow) && trialNow > 0 ? trialNow : 1700000000000;
  }

  /** A deterministic ISO timestamp; `seq` orders events. */
  stamp(seq: number): string {
    return new Date(this.baseMs + seq * 1000).toISOString();
  }

  nextSequence(): number {
    return this.nextSeq++;
  }

  allocDbId(): number {
    return this.nextDbId++;
  }

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo {
    const seq = this.nextSequence();
    const repo = new Repo(this.allocDbId(), owner, name, description, isPrivate, seq);
    this.repos.set(this.repoKey(owner, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  listRepos(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const r of this.repos.values()) if (r.owner === owner) out.push(r);
    out.sort((a, b) => a.seq - b.seq);
    return out;
  }

  /** Opaque Relay-style global id: base64 of "kind:owner/name:key". */
  globalId(kind: string, owner: string, name: string, key: string | number): string {
    return Buffer.from(`${kind}:${owner}/${name}:${key}`, 'utf8').toString('base64');
  }

  decodeGlobalId(id: string): { kind: string; owner: string; name: string; key: string } | null {
    try {
      const text = Buffer.from(id, 'base64').toString('utf8');
      const m = /^([A-Z]+):([^/]+)\/([^:]+):(.+)$/.exec(text);
      if (!m) return null;
      return { kind: m[1]!, owner: m[2]!, name: m[3]!, key: m[4]! };
    } catch {
      return null;
    }
  }
}
