// GitForge git wire layer: pkt-lines, packfiles (read/write, deltas) and the
// smart-HTTP handlers for info/refs, upload-pack (v2 + v0) and receive-pack.
// Written from scratch from the git protocol as documented in the kit's
// wire contract: no git binary, no outside code.

import { createHash as sha1CreateHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';
import {
  ZERO_OID,
  hashObject,
  type GitObjectType,
  type Repo,
  type Store,
  type StoredObject,
} from './store.ts';

export const ZERO = ZERO_OID;

/** One pkt-line: 4 hex length digits + payload. */
export function pktLine(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.alloc(4);
  head.write((body.length + 4).toString(16).padStart(4, '0'), 0, 4, 'ascii');
  return Buffer.concat([head, body]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');

export interface DecodedPacket {
  kind: 'data' | 'flush' | 'delim' | 'response-end';
  payload: Buffer;
}

/**
 * Decode pkt-lines from the front of `buf`. Returns the packets and the offset
 * where the framed section ends (raw trailing bytes, e.g. a pack, start there).
 * Returns null when the bytes are not pkt-line framed at all.
 */
export function decodePktLines(buf: Buffer): { packets: DecodedPacket[]; rest: number } | null {
  const packets: DecodedPacket[] = [];
  let off = 0;
  while (off + 4 <= buf.length) {
    const head = buf.subarray(off, off + 4).toString('ascii');
    if (!/^[0-9a-fA-F]{4}$/.test(head)) return null;
    const len = Number.parseInt(head, 16);
    if (len === 0) {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      off += 4;
      break;
    }
    if (len === 1) {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    if (len === 2) {
      packets.push({ kind: 'response-end', payload: Buffer.alloc(0) });
      off += 4;
      continue;
    }
    if (len < 4 || off + len > buf.length) return null;
    packets.push({ kind: 'data', payload: buf.subarray(off + 4, off + len) });
    off += len;
  }
  return { packets, rest: off };
}

const TYPE_CODE: Record<GitObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPE: Record<number, GitObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

function packObjectHeader(type: GitObjectType, size: number): Buffer {
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

/** Build a packfile from plain objects (no deltas), trailer SHA-1 included. */
export function writePack(objects: ReadonlyArray<{ type: GitObjectType; data: Buffer }>): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 4, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    parts.push(packObjectHeader(object.type, object.data.length));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  const sum = sha1CreateHash('sha1').update(body).digest();
  return Buffer.concat([body, sum]);
}

/** Apply a git delta to its base bytes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let off = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const b = delta[off++] ?? 0;
      value |= (b & 0x7f) << shift;
      shift += 7;
      if ((b & 0x80) === 0) break;
    }
    return value;
  };
  const srcSize = readVarint();
  const tgtSize = readVarint();
  if (srcSize !== base.length) throw new Error('delta source size mismatch');
  const out: Buffer[] = [];
  let total = 0;
  while (off < delta.length) {
    const op = delta[off++]!;
    if (op & 0x80) {
      let cpOff = 0;
      let cpSize = 0;
      if (op & 0x01) cpOff = delta[off++]!;
      if (op & 0x02) cpOff |= delta[off++]! << 8;
      if (op & 0x04) cpOff |= delta[off++]! << 16;
      if (op & 0x08) cpOff |= delta[off++]! << 24;
      if (op & 0x10) cpSize = delta[off++]!;
      if (op & 0x20) cpSize |= delta[off++]! << 8;
      if (op & 0x40) cpSize |= delta[off++]! << 16;
      if (cpSize === 0) cpSize = 0x10000;
      out.push(base.subarray(cpOff, cpOff + cpSize));
      total += cpSize;
    } else if (op !== 0) {
      out.push(delta.subarray(off, off + op));
      total += op;
      off += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  const result = Buffer.concat(out, total);
  if (result.length !== tgtSize) throw new Error('delta target size mismatch');
  return result;
}

export interface PackObject {
  type: GitObjectType;
  data: Buffer;
  oid: string;
}

export interface PackRead {
  objects: PackObject[];
  ok: boolean;
  error?: string;
}

/**
 * Read a packfile. `lookup` resolves REF_DELTA bases the pack itself does not
 * carry (objects the server already stores). The deflated length of each
 * entry is found by re-deflating the inflated bytes (the client encodes with
 * Node's default deflate, which reproduces byte-identically) with a binary
 * search fallback, since inflateSync ignores trailing bytes.
 */
export function readPack(buf: Buffer, lookup?: (oid: string) => StoredObject | undefined): PackRead {
  try {
    if (buf.length < 32) return { objects: [], ok: false, error: 'pack too short' };
    if (buf.subarray(0, 4).toString('ascii') !== 'PACK') return { objects: [], ok: false, error: 'bad pack magic' };
    const version = buf.readUInt32BE(4);
    if (version !== 2 && version !== 3) return { objects: [], ok: false, error: `pack version ${version}` };
    const count = buf.readUInt32BE(8);
    const trailer = buf.subarray(buf.length - 20).toString('hex');
    const sum = sha1CreateHash('sha1').update(buf.subarray(0, buf.length - 20)).digest('hex');
    if (sum !== trailer) return { objects: [], ok: false, error: 'pack checksum mismatch' };
    const end = buf.length - 20;

    const findDeflatedLength = (start: number, declaredSize: number): number => {
      const rest = buf.subarray(start, end);
      const inflated = inflateSync(rest);
      if (inflated.length !== declaredSize) throw new Error('inflated size mismatch');
      const re = deflateSync(inflated);
      if (re.length <= rest.length && rest.subarray(0, re.length).equals(re)) return re.length;
      // Fallback: binary search the smallest prefix that inflates to the size.
      let lo = 1;
      let hi = rest.length;
      while (lo < hi) {
        const mid = (lo + hi) >> 1;
        try {
          if (inflateSync(rest.subarray(0, mid)).length === declaredSize) hi = mid;
          else lo = mid + 1;
        } catch {
          lo = mid + 1;
        }
      }
      return lo;
    };

    interface RawEntry {
      type: GitObjectType | 'ofs-delta' | 'ref-delta';
      data: Buffer;
      baseOid?: string;
      baseOff?: number;
      startOff: number;
    }
    const raw: RawEntry[] = [];
    let off = 12;
    for (let i = 0; i < count; i += 1) {
      if (off >= end) return { objects: [], ok: false, error: 'pack truncated' };
      const startOff = off;
      let b = buf[off++]!;
      const typeCode = (b >> 4) & 0x07;
      let size = b & 0x0f;
      let shift = 4;
      while (b & 0x80) {
        b = buf[off++]!;
        size |= (b & 0x7f) << shift;
        shift += 7;
      }
      if (typeCode === 6) {
        // OFS_DELTA: offset varint, base is startOff - offset.
        let ob = buf[off++]!;
        let offset = ob & 0x7f;
        while (ob & 0x80) {
          ob = buf[off++]!;
          offset = ((offset + 1) << 7) | (ob & 0x7f);
        }
        const len = findDeflatedLength(off, size);
        const data = inflateSync(buf.subarray(off, off + len));
        off += len;
        raw.push({ type: 'ofs-delta', data, baseOff: startOff - offset, startOff });
      } else if (typeCode === 7) {
        const baseOid = buf.subarray(off, off + 20).toString('hex');
        off += 20;
        const len = findDeflatedLength(off, size);
        const data = inflateSync(buf.subarray(off, off + len));
        off += len;
        raw.push({ type: 'ref-delta', data, baseOid, startOff });
      } else {
        const type = CODE_TYPE[typeCode];
        if (!type) return { objects: [], ok: false, error: `unknown pack type ${typeCode}` };
        const len = findDeflatedLength(off, size);
        const data = inflateSync(buf.subarray(off, off + len));
        off += len;
        raw.push({ type, data, startOff });
      }
    }

    const resolved = new Map<number, PackObject>();
    const byStart = new Map<number, RawEntry>();
    for (const entry of raw) byStart.set(entry.startOff, entry);
    const objects: PackObject[] = [];
    const resolve = (index: number, depth: number): PackObject | null => {
      if (depth > 64) return null;
      const hit = resolved.get(index);
      if (hit) return hit;
      const entry = raw[index]!;
      if (entry.type === 'ofs-delta' || entry.type === 'ref-delta') {
        let base: { type: GitObjectType; data: Buffer } | null = null;
        if (entry.type === 'ofs-delta' && entry.baseOff !== undefined) {
          const baseEntry = byStart.get(entry.baseOff);
          if (baseEntry) {
            const baseIndex = raw.indexOf(baseEntry);
            const r = resolve(baseIndex, depth + 1);
            if (r) base = r;
          }
        } else if (entry.type === 'ref-delta' && entry.baseOid) {
          const inPack = raw.findIndex((e) => e.type !== 'ofs-delta' && e.type !== 'ref-delta' && hashObject(e.type as GitObjectType, e.data) === entry.baseOid);
          if (inPack >= 0) {
            const r = resolve(inPack, depth + 1);
            if (r) base = r;
          } else if (lookup) {
            const stored = lookup(entry.baseOid);
            if (stored) base = stored;
          }
        }
        if (!base) return null;
        const data = applyDelta(base.data, entry.data);
        const obj: PackObject = { type: base.type, data, oid: hashObject(base.type, data) };
        resolved.set(index, obj);
        objects.push(obj);
        return obj;
      }
      const obj: PackObject = { type: entry.type as GitObjectType, data: entry.data, oid: hashObject(entry.type as GitObjectType, entry.data) };
      resolved.set(index, obj);
      objects.push(obj);
      return obj;
    };
    for (let i = 0; i < raw.length; i += 1) {
      if (resolve(i, 0) === null) return { objects: [], ok: false, error: 'unresolvable delta' };
    }
    return { objects, ok: true };
  } catch (err) {
    return { objects: [], ok: false, error: err instanceof Error ? err.message : String(err) };
  }
}

// ---------------------------------------------------------------------------
// smart-HTTP handlers
// ---------------------------------------------------------------------------

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
}

const NOT_FOUND_JSON = Buffer.from('{"message":"Not Found"}\n', 'utf8');
const AGENT = 'GitForge/1.0';

function v2Advertisement(): Buffer {
  return Buffer.concat([
    pktLine('version 2\n'),
    pktLine('ls-refs=unborn\n'),
    pktLine('fetch=shallow wait-for-done\n'),
    pktLine('object-format=sha1\n'),
    pktLine(`agent=${AGENT}\n`),
    PKT_FLUSH,
  ]);
}

const V2_ADV = v2Advertisement();

function sortedRefs(repo: Repo): Array<{ name: string; oid: string }> {
  const out: Array<{ name: string; oid: string }> = [];
  for (const [name, oid] of repo.refs) out.push({ name, oid });
  out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return out;
}

function v0Advertisement(store: Store, repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps =
    service === 'git-upload-pack'
      ? `side-band-64k multi_ack_detailed thin-pack ofs-delta shallow symref=HEAD:refs/heads/${repo.defaultBranch} agent=${AGENT}`
      : `report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=${AGENT}`;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = sortedRefs(repo);
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO} capabilities^{}\0${caps}\n`));
  } else {
    let first = true;
    const emit = (line: string): void => {
      parts.push(pktLine(first ? `${line}\0${caps}\n` : `${line}\n`));
      first = false;
    };
    if (tip !== undefined) emit(`${tip} HEAD`);
    for (const ref of refs) {
      emit(`${ref.oid} ${ref.name}`);
      const obj = store.getObject(ref.oid);
      if (obj?.type === 'tag' && obj.tag) {
        const peeled = store.peelToCommit(ref.oid);
        parts.push(pktLine(`${peeled} ${ref.name}^{}\n`));
      }
    }
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** GET /{owner}/{repo}.git/info/refs?service=... */
export function handleInfoRefs(
  store: Store,
  owner: string,
  name: string,
  service: string,
  wantV2: boolean,
): GitResponse {
  const repo = store.getRepo(owner, name);
  if (!repo) return { status: 404, contentType: 'application/json', body: NOT_FOUND_JSON };
  if (service === 'git-upload-pack') {
    if (wantV2) {
      if (!repo.advV2) repo.advV2 = V2_ADV;
      return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: repo.advV2 };
    }
    if (!repo.advV0Up) repo.advV0Up = v0Advertisement(store, repo, 'git-upload-pack');
    return { status: 200, contentType: 'application/x-git-upload-pack-advertisement', body: repo.advV0Up };
  }
  if (service === 'git-receive-pack') {
    if (!repo.advV0Recv) repo.advV0Recv = v0Advertisement(store, repo, 'git-receive-pack');
    return { status: 200, contentType: 'application/x-git-receive-pack-advertisement', body: repo.advV0Recv };
  }
  return { status: 404, contentType: 'application/json', body: NOT_FOUND_JSON };
}

/** Chunk pack bytes into sideband band-1 pkt-lines (32 KiB data chunks). */
function sidebandPack(pack: Buffer): Buffer[] {
  const out: Buffer[] = [];
  for (let off = 0; off < pack.length; off += 32768) {
    const chunk = pack.subarray(off, Math.min(pack.length, off + 32768));
    out.push(pktLine(Buffer.concat([Buffer.from([1]), chunk])));
  }
  if (pack.length === 0) out.push(pktLine(Buffer.concat([Buffer.from([1]), Buffer.alloc(0)])));
  return out;
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  filter: string | null;
}

/** POST /{owner}/{repo}.git/git-upload-pack */
export function handleUploadPack(store: Store, owner: string, name: string, body: Buffer, isV2: boolean): GitResponse {
  const repo = store.getRepo(owner, name);
  if (!repo) return { status: 404, contentType: 'application/json', body: NOT_FOUND_JSON };
  const decoded = decodePktLines(body);
  if (!decoded) return { status: 400, contentType: 'application/json', body: Buffer.from('{"message":"Bad Request"}\n', 'utf8') };
  const lines = decoded.packets.filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const resultType = 'application/x-git-upload-pack-result';

  if (!isV2) {
    // v0 fetch: wants, then flush, then haves + done.
    const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, filter: null };
    for (const line of lines) {
      const text = line.replace(/\n$/, '');
      if (text.startsWith('want ')) args.wants.push(text.slice(5).split(' ')[0]);
      else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
      else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) args.depth = Number(text.slice(7).trim());
    }
    const { objects, shallow, unshallow } = store.collectForFetch(args.wants, args.haves, args.shallow, args.depth);
    const pack = writePack([...objects.values()].map((o) => ({ type: o.type, data: o.data })));
    const parts: Buffer[] = [];
    const ack = args.haves.find((h) => store.getObject(h) !== undefined);
    parts.push(pktLine(ack !== undefined ? `ACK ${ack}\n` : 'NAK\n'));
    for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
    for (const u of unshallow) parts.push(pktLine(`unshallow ${u}\n`));
    parts.push(...sidebandPack(pack));
    parts.push(PKT_FLUSH);
    return { status: 200, contentType: resultType, body: Buffer.concat(parts) };
  }

  const command = (lines[0] ?? '').replace(/\n$/, '');
  if (command === 'command=ls-refs') {
    let peel = false;
    let symrefs = false;
    let unborn = false;
    const prefixes: string[] = [];
    for (const line of lines.slice(1)) {
      const text = line.replace(/\n$/, '');
      if (text === 'peel') peel = true;
      else if (text === 'symrefs') symrefs = true;
      else if (text === 'unborn') unborn = true;
      else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice(11));
    }
    const cacheKey = `ls:${prefixes.join(',')}|${peel ? 1 : 0}${symrefs ? 1 : 0}${unborn ? 1 : 0}`;
    const cached = repo.fetchCache.get(cacheKey);
    if (cached) return { status: 200, contentType: resultType, body: cached };
    const parts: Buffer[] = [];
    const refs = sortedRefs(repo);
    const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    const keep = (refName: string): boolean => prefixes.length === 0 || prefixes.some((p) => refName.startsWith(p));
    if (refs.length === 0) {
      if (unborn) parts.push(pktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    } else {
      // HEAD is listed only when symrefs is asked for and no ref-prefix rules
      // it out (a 'refs/heads/' prefix does not match 'HEAD').
      if (symrefs && tip !== undefined && (prefixes.length === 0 || prefixes.some((p) => 'HEAD'.startsWith(p)))) {
        parts.push(pktLine(`${tip} HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
      }
      for (const ref of refs) {
        if (!keep(ref.name)) continue;
        let line = `${ref.oid} ${ref.name}`;
        if (peel) {
          const obj = store.getObject(ref.oid);
          if (obj?.type === 'tag' && obj.tag) line += ` peeled:${store.peelToCommit(ref.oid)}`;
        }
        parts.push(pktLine(`${line}\n`));
      }
    }
    parts.push(PKT_FLUSH);
    const out = Buffer.concat(parts);
    repo.fetchCache.set(cacheKey, out);
    return { status: 200, contentType: resultType, body: out };
  }

  if (command === 'command=fetch') {
    const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, filter: null };
    for (const line of lines.slice(1)) {
      const text = line.replace(/\n$/, '');
      if (text.startsWith('want ')) args.wants.push(text.slice(5).trim());
      else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
      else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) args.depth = Number(text.slice(7).trim());
      else if (text.startsWith('filter ')) args.filter = text.slice(7).trim();
    }
    if (args.wants.length === 0) {
      return { status: 200, contentType: resultType, body: Buffer.concat([pktLine('acknowledgments\n'), pktLine('NAK\n'), PKT_DELIM, pktLine('packfile\n'), ...sidebandPack(writePack([])), PKT_FLUSH]) };
    }
    const cacheKey = `fetch:${args.wants.join(',')}|${args.haves.join(',')}|${args.depth ?? ''}|${args.shallow.join(',')}`;
    const cached = repo.fetchCache.get(cacheKey);
    if (cached) return { status: 200, contentType: resultType, body: cached };
    const { objects, shallow, unshallow } = store.collectForFetch(args.wants, args.haves, args.shallow, args.depth);
    const pack = writePack([...objects.values()].map((o) => ({ type: o.type, data: o.data })));
    const parts: Buffer[] = [pktLine('acknowledgments\n')];
    const ack = args.haves.find((h) => store.getObject(h) !== undefined);
    if (ack !== undefined) {
      parts.push(pktLine(`ACK ${ack}\n`), pktLine('ready\n'));
    } else {
      parts.push(pktLine('NAK\n'));
    }
    parts.push(PKT_DELIM);
    if (shallow.length > 0 || unshallow.length > 0) {
      parts.push(pktLine('shallow-info\n'));
      for (const s of shallow) parts.push(pktLine(`shallow ${s}\n`));
      for (const u of unshallow) parts.push(pktLine(`unshallow ${u}\n`));
      parts.push(PKT_DELIM);
    }
    parts.push(pktLine('packfile\n'), ...sidebandPack(pack), PKT_FLUSH);
    const out = Buffer.concat(parts);
    repo.fetchCache.set(cacheKey, out);
    return { status: 200, contentType: resultType, body: out };
  }

  return { status: 400, contentType: 'application/json', body: Buffer.from('{"message":"Bad Request"}\n', 'utf8') };
}

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

/** POST /{owner}/{repo}.git/git-receive-pack */
export function handleReceivePack(store: Store, owner: string, name: string, body: Buffer): GitResponse {
  const repo = store.getRepo(owner, name);
  if (!repo) return { status: 404, contentType: 'application/json', body: NOT_FOUND_JSON };
  const resultType = 'application/x-git-receive-pack-result';
  const decoded = decodePktLines(body);
  if (!decoded) return { status: 400, contentType: 'application/json', body: Buffer.from('{"message":"Bad Request"}\n', 'utf8') };
  const dataLines = decoded.packets.filter((p) => p.kind === 'data').map((p) => p.payload.toString('utf8'));
  const commands: PushCommand[] = [];
  let sideband = false;
  for (let i = 0; i < dataLines.length; i += 1) {
    let text = dataLines[i].replace(/\n$/, '');
    const nul = text.indexOf('\0');
    if (nul >= 0) {
      if (i === 0) sideband = text.slice(nul + 1).split(' ').includes('side-band-64k');
      text = text.slice(0, nul);
    }
    const parts = text.split(' ');
    if (parts.length < 3) continue;
    commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts.slice(2).join(' ') });
  }
  if (commands.length === 0) {
    return { status: 400, contentType: 'application/json', body: Buffer.from('{"message":"Bad Request"}\n', 'utf8') };
  }

  const reportLines: string[] = [];
  const packBytes = body.subarray(decoded.rest);
  const needsPack = commands.some((c) => c.newOid !== ZERO);
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (needsPack) {
    if (packBytes.length === 0) {
      unpackOk = false;
      unpackDetail = 'missing pack';
    } else {
      const read = readPack(packBytes, (oid) => store.getObject(oid));
      if (!read.ok) {
        unpackOk = false;
        unpackDetail = read.error ?? 'unpack error';
      } else {
        for (const obj of read.objects) store.putObject(obj.type, obj.data);
      }
    }
  }
  if (unpackOk) {
    for (const cmd of commands) {
      const current = repo.refs.get(cmd.ref);
      if (cmd.newOid === ZERO) {
        // Delete.
        if (current === undefined) {
          reportLines.push(`ng ${cmd.ref} remote ref does not exist`);
        } else if (cmd.oldOid !== ZERO && cmd.oldOid !== current) {
          reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        } else {
          store.setRef(repo, cmd.ref, null);
          reportLines.push(`ok ${cmd.ref}`);
        }
        continue;
      }
      if (store.getObject(cmd.newOid) === undefined) {
        reportLines.push(`ng ${cmd.ref} object not found`);
        continue;
      }
      if (current === undefined) {
        if (cmd.oldOid !== ZERO) {
          reportLines.push(`ng ${cmd.ref} non-fast-forward`);
          continue;
        }
        store.setRef(repo, cmd.ref, cmd.newOid);
        reportLines.push(`ok ${cmd.ref}`);
        continue;
      }
      if (cmd.oldOid === ZERO) {
        reportLines.push(`ng ${cmd.ref} already exists`);
        continue;
      }
      if (cmd.oldOid !== current) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
      if (!store.isAncestor(current, cmd.newOid)) {
        reportLines.push(`ng ${cmd.ref} non-fast-forward`);
        continue;
      }
      store.setRef(repo, cmd.ref, cmd.newOid);
      reportLines.push(`ok ${cmd.ref}`);
    }
  }
  const textLines = [`unpack ${unpackOk ? 'ok' : unpackDetail}`, ...reportLines];
  const framed = Buffer.concat(textLines.map((l) => pktLine(`${l}\n`)));
  let out: Buffer;
  if (sideband) {
    const chunks: Buffer[] = [];
    for (let off = 0; off < framed.length; off += 32768) {
      chunks.push(pktLine(Buffer.concat([Buffer.from([1]), framed.subarray(off, Math.min(framed.length, off + 32768))])));
    }
    out = Buffer.concat([...chunks, PKT_FLUSH]);
  } else {
    out = Buffer.concat([framed, PKT_FLUSH]);
  }
  return { status: 200, contentType: resultType, body: out };
}
