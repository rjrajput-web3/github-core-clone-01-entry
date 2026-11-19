import { deflateSync, inflateSync } from 'node:zlib';
import {
  collectReachable,
  commitDepths,
  hashObject,
  isAncestor,
  parseCommit,
  parseTag,
  parseTree,
  peelToCommit,
  sha1Hex,
  ZERO_OID,
  type GitObject,
  type ObjectType,
  type Repo,
  type Store,
} from './store.ts';

// -------------------------------------------------------------------------------------------
// pkt-line framing
// -------------------------------------------------------------------------------------------

export function pktLine(data: string | Buffer): Buffer {
  const payload = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  const length = payload.length + 4;
  const head = Buffer.from(length.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, payload]);
}

export const FLUSH = Buffer.from('0000', 'ascii');

export function pktLines(lines: Array<string | Buffer>): Buffer {
  return Buffer.concat(lines.map((line) => pktLine(line)));
}

interface Packet {
  kind: 'data' | 'flush' | 'delim';
  payload: Buffer;
}

/** Decode a whole buffer of pkt-lines into packets (trailing bytes after the last packet are returned). */
export function decodePackets(buf: Buffer): { packets: Packet[]; rest: Buffer } {
  const packets: Packet[] = [];
  let offset = 0;
  while (offset + 4 <= buf.length) {
    const head = buf.subarray(offset, offset + 4).toString('ascii');
    if (head === '0000') {
      packets.push({ kind: 'flush', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (head === '0001') {
      packets.push({ kind: 'delim', payload: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    const length = parseInt(head, 16);
    if (!Number.isFinite(length) || length < 4 || offset + length > buf.length) break;
    packets.push({ kind: 'data', payload: buf.subarray(offset + 4, offset + length) });
    offset += length;
  }
  return { packets, rest: buf.subarray(offset) };
}

// -------------------------------------------------------------------------------------------
// packfiles
// -------------------------------------------------------------------------------------------

const TYPE_CODES: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPES: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function buildPack(objects: Array<{ oid: string; type: ObjectType; data: Buffer }>): Buffer {
  const chunks: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(objects.length, 8);
  chunks.push(head);
  for (const object of objects) {
    const deflated = deflateSync(object.data, { level: 6 });
    const size = object.data.length;
    const typeCode = TYPE_CODES[object.type];
    const headerBytes: number[] = [];
    let remaining = size;
    let first = (typeCode << 4) | (remaining & 0x0f);
    remaining >>>= 4;
    if (remaining > 0) first |= 0x80;
    headerBytes.push(first);
    while (remaining > 0) {
      let byte = remaining & 0x7f;
      remaining >>>= 7;
      if (remaining > 0) byte |= 0x80;
      headerBytes.push(byte);
    }
    chunks.push(Buffer.from(headerBytes), deflated);
  }
  const body = Buffer.concat(chunks);
  return Buffer.concat([body, Buffer.from(sha1Hex(body), 'hex')]);
}

interface PackEntry {
  type: ObjectType;
  data: Buffer;
  oid: string;
}

export function parsePack(buf: Buffer, base: Map<string, GitObject>): Map<string, PackEntry> {
  return parsePackSimple(buf, base);
}

/** Apply a git delta (copy/insert instructions) to a base object's bytes. */
export function applyDelta(baseData: Buffer, delta: Buffer): Buffer {
  let offset = 0;
  const readVar = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      const byte = delta[offset++];
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const baseSize = readVar();
  void baseSize;
  const resultSize = readVar();
  const out: Buffer[] = [];
  let total = 0;
  while (offset < delta.length) {
    const op = delta[offset++];
    if (op & 0x80) {
      let copyOffset = 0;
      let copySize = 0;
      for (let i = 0; i < 4; i += 1) if (op & (1 << i)) copyOffset |= delta[offset++] << (8 * i);
      for (let i = 0; i < 3; i += 1) if (op & (0x10 << i)) copySize |= delta[offset++] << (8 * i);
      if (copySize === 0) copySize = 0x10000;
      out.push(baseData.subarray(copyOffset, copyOffset + copySize));
      total += copySize;
    } else if (op !== 0) {
      out.push(delta.subarray(offset, offset + op));
      offset += op;
      total += op;
    }
  }
  void total;
  void resultSize;
  return Buffer.concat(out);
}

// -------------------------------------------------------------------------------------------
// ref listing helpers
// -------------------------------------------------------------------------------------------

export interface RefLine {
  oid: string;
  name: string;
  symrefTarget?: string;
  peeled?: string;
  unborn?: boolean;
}

export function listRefs(repo: Repo): RefLine[] {
  const lines: RefLine[] = [];
  const defaultRef = `refs/heads/${repo.defaultBranch}`;
  lines.push({ oid: repo.refs.get(defaultRef) ?? ZERO_OID, name: 'HEAD', symrefTarget: defaultRef });
  const names = [...repo.refs.keys()].sort();
  for (const name of names) {
    const oid = repo.refs.get(name) as string;
    const line: RefLine = { oid, name };
    const obj = repo.objects.get(oid);
    if (obj !== undefined && obj.type === 'tag') line.peeled = peelToCommit(repo, oid);
    lines.push(line);
  }
  return lines;
}

function sidebandPack(pack: Buffer): Buffer {
  const chunks: Buffer[] = [];
  const max = 65500;
  for (let offset = 0; offset < pack.length; offset += max) {
    const slice = pack.subarray(offset, Math.min(pack.length, offset + max));
    const framed = Buffer.concat([Buffer.from([1]), slice]);
    chunks.push(pktLine(framed));
  }
  if (pack.length === 0) chunks.push(pktLine(Buffer.from([1])));
  chunks.push(FLUSH);
  return Buffer.concat(chunks);
}

// -------------------------------------------------------------------------------------------
// smart HTTP
// -------------------------------------------------------------------------------------------

export interface GitRequest {
  owner: string;
  repoName: string;
  service: 'git-upload-pack' | 'git-receive-pack';
  query: URLSearchParams;
  headers: Record<string, string | undefined>;
  body: Buffer;
}

export interface GitResponse {
  status: number;
  contentType: string;
  body: Buffer;
}

const V2_CAPS = ['agent=forge/1.0', 'ls-refs=unborn', 'fetch=shallow filter', 'object-format=sha1'];
const V0_UPLOAD_CAPS = 'side-band-64k thin-pack ofs-delta shallow multi_ack symref=HEAD:refs/heads/main agent=forge/1.0';
const RECEIVE_CAPS = 'report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=forge/1.0';

function v0Advertisement(repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const preamble = service === 'git-upload-pack' ? '# service=git-upload-pack\n' : '# service=git-receive-pack\n';
  const caps = service === 'git-upload-pack' ? V0_UPLOAD_CAPS : RECEIVE_CAPS;
  const parts: Buffer[] = [pktLine(preamble), FLUSH];
  const refs = listRefs(repo).filter((ref) => ref.name !== 'HEAD');
  const headOid = repo.refs.get(`refs/heads/${repo.defaultBranch}`) ?? ZERO_OID;
  if (refs.length === 0) {
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
  } else {
    parts.push(pktLine(`${headOid} HEAD\0${caps}\n`));
    for (const ref of refs) {
      parts.push(pktLine(`${ref.oid} ${ref.name}\n`));
      if (ref.peeled !== undefined) parts.push(pktLine(`${ref.peeled} ${ref.name}^{}\n`));
    }
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

function v2Advertisement(): Buffer {
  const parts: Buffer[] = [pktLine('version 2\n')];
  for (const cap of V2_CAPS) parts.push(pktLine(`${cap}\n`));
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

export function handleInfoRefs(store: Store, req: GitRequest): GitResponse {
  const repo = store.getRepo(req.owner, req.repoName);
  if (repo === undefined || req.owner !== 'adaiversen') {
    return { status: 404, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8') };
  }
  const service = req.query.get('service') ?? '';
  if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
    return { status: 404, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8') };
  }
  const wantsV2 = req.service === 'git-upload-pack'
    && (req.headers['git-protocol'] ?? '').includes('version=2');
  const contentType = service === 'git-upload-pack'
    ? 'application/x-git-upload-pack-advertisement'
    : 'application/x-git-receive-pack-advertisement';
  if (service === 'git-receive-pack') {
    return { status: 200, contentType, body: v0Advertisement(repo, service) };
  }
  return { status: 200, contentType, body: wantsV2 ? v2Advertisement() : v0Advertisement(repo, service) };
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth?: number;
  deepenSince?: number;
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
  command: string;
}

function parseV2Args(packets: Packet[]): FetchArgs {
  const args: FetchArgs = {
    wants: [], haves: [], shallow: [], peel: false, symrefs: false, unborn: false, prefixes: [], command: '',
  };
  let seenDelim = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind === 'delim') {
      seenDelim = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const line = packet.payload.toString('utf8').replace(/\n$/, '');
    if (line.startsWith('command=')) {
      args.command = line.slice('command='.length);
      continue;
    }
    if (!seenDelim) continue;
    if (line.startsWith('want ')) args.wants.push(line.slice(5).trim());
    else if (line.startsWith('have ')) args.haves.push(line.slice(5).trim());
    else if (line.startsWith('shallow ')) args.shallow.push(line.slice(8).trim());
    else if (line.startsWith('deepen ')) args.depth = Number(line.slice(7));
    else if (line.startsWith('deepen-since ')) args.deepenSince = Number(line.slice(13));
    else if (line === 'peel') args.peel = true;
    else if (line === 'symrefs') args.symrefs = true;
    else if (line === 'unborn') args.unborn = true;
    else if (line.startsWith('ref-prefix ')) args.prefixes.push(line.slice(11));
  }
  return args;
}

function lsRefsResponse(repo: Repo, args: FetchArgs): Buffer {
  const parts: Buffer[] = [];
  const all = listRefs(repo);
  const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const hasRefs = repo.refs.size > 0;
  for (const ref of all) {
    if (!keep(ref.name)) continue;
    if (ref.name === 'HEAD') {
      if (!args.symrefs && !hasRefs) continue;
      if (!hasRefs) {
        if (args.unborn) parts.push(pktLine(`unborn HEAD symref-target:${ref.symrefTarget ?? ''}\n`));
        continue;
      }
      const target = ref.symrefTarget ?? '';
      const oid = repo.refs.get(target) ?? ZERO_OID;
      parts.push(pktLine(args.symrefs ? `${oid} HEAD symref-target:${target}\n` : `${oid} HEAD\n`));
      continue;
    }
    let line = `${ref.oid} ${ref.name}`;
    if (args.symrefs && ref.symrefTarget !== undefined) line += ` symref-target:${ref.symrefTarget}`;
    if (args.peel && ref.peeled !== undefined) line += ` peeled:${ref.peeled}`;
    parts.push(pktLine(`${line}\n`));
  }
  parts.push(FLUSH);
  return Buffer.concat(parts);
}

/** The objects a fetch must send for `wants`, minus what `haves` already hold, honoring depth. */
export function planFetch(repo: Repo, wants: string[], haves: string[], depth: number | undefined, clientShallow: string[]): {
  send: Map<string, GitObject>;
  shallowOut: string[];
  unshallowOut: string[];
} {
  const haveClosure = new Set<string>();
  // The client's have-closure stops at its shallow boundaries: beyond them the
  // client holds nothing, so those ancestors must still be sendable (deepen).
  for (const have of haves) {
    const haveObj = repo.objects.get(have);
    if (haveObj !== undefined && haveObj.type === 'tag') haveClosure.add(have);
    const stack = [peelToCommit(repo, have)];
    const seen = new Set<string>();
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = repo.objects.get(oid);
      if (obj === undefined || obj.type !== 'commit') continue;
      haveClosure.add(oid);
      const treeQueue = [parseCommit(obj.data).tree];
      while (treeQueue.length > 0) {
        const treeOid = treeQueue.pop() as string;
        if (haveClosure.has(treeOid)) continue;
        const treeObj = repo.objects.get(treeOid);
        if (treeObj === undefined) continue;
        haveClosure.add(treeOid);
        if (treeObj.type === 'tree') for (const entry of parseTree(treeObj.data)) treeQueue.push(entry.oid);
      }
      if (clientShallow.includes(oid)) continue;
      for (const parent of parseCommit(obj.data).parents) stack.push(parent);
    }
  }
  const wantCommits = wants.map((want) => peelToCommit(repo, want));
  const depths = commitDepths(repo, wantCommits);
  const shallowOut: string[] = [];
  const unshallowOut: string[] = [];
  const includedCommits = new Set<string>();
  for (const [oid, depthAt] of depths) {
    if (depth === undefined || depthAt < depth) {
      includedCommits.add(oid);
      // The last included generation is the shallow boundary: its parents are cut.
      if (depth !== undefined && depthAt === depth - 1) shallowOut.push(oid);
    }
  }
  // Shallow boundaries the client already holds: if they are now included, they unshallow.
  for (const boundary of clientShallow) {
    if (includedCommits.has(boundary) || depths.has(boundary) && (depth === undefined || (depths.get(boundary) as number) < depth)) {
      unshallowOut.push(boundary);
    } else if (!shallowOut.includes(boundary) && depths.has(boundary)) {
      shallowOut.push(boundary);
    }
  }
  const send = new Map<string, GitObject>();
  const addTreeClosure = (treeOid: string, reached: Set<string>): void => {
    const queue = [treeOid];
    while (queue.length > 0) {
      const oid = queue.pop() as string;
      if (reached.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (obj === undefined) continue;
      reached.add(oid);
      if (obj.type === 'tree') for (const entry of parseTree(obj.data)) queue.push(entry.oid);
    }
  };
  const addReachable = (tips: string[]): void => {
    const reached = new Set<string>();
    for (const oid of includedCommits) {
      const obj = repo.objects.get(oid);
      if (obj === undefined) continue;
      reached.add(oid);
      if (obj.type === 'commit') addTreeClosure(parseCommit(obj.data).tree, reached);
    }
    // Tags pointing at wanted tips travel too, with their target's closure.
    for (const oid of tips) {
      const obj = repo.objects.get(oid);
      if (obj !== undefined && obj.type === 'tag') {
        reached.add(oid);
        const target = peelToCommit(repo, oid);
        if (includedCommits.has(target)) {
          reached.add(target);
          const targetObj = repo.objects.get(target);
          if (targetObj !== undefined && targetObj.type === 'commit') addTreeClosure(parseCommit(targetObj.data).tree, reached);
        }
      }
    }
    for (const oid of reached) {
      if (haveClosure.has(oid)) continue;
      const obj = repo.objects.get(oid);
      if (obj !== undefined) send.set(oid, obj);
    }
  };
  addReachable(wants);
  // When nothing is new, the pack is empty (fetch-nothing-new).
  return { send, shallowOut, unshallowOut };
}

function fetchV2Response(repo: Repo, args: FetchArgs): Buffer {
  const parts: Buffer[] = [];
  const { send, shallowOut, unshallowOut } = planFetch(repo, args.wants, args.haves, args.depth, args.shallow);
  if (args.haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    const acked = args.haves.filter((have) => {
      const closure = new Set<string>();
      for (const want of args.wants) collectReachable(repo, [want], closure);
      return closure.has(have);
    });
    if (acked.length === 0) parts.push(pktLine('NAK\n'));
    else for (const oid of acked) parts.push(pktLine(`ACK ${oid}\n`));
    parts.push(pktLine('ready\n'));
    parts.push(FLUSH);
  }
  if (shallowOut.length > 0 || unshallowOut.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of shallowOut) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of unshallowOut) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(FLUSH);
  }
  parts.push(pktLine('packfile\n'));
  const pack = buildPack([...send.entries()].map(([oid, obj]) => ({ oid, type: obj.type, data: obj.data })));
  parts.push(sidebandPack(pack));
  return Buffer.concat(parts);
}

function fetchV0Response(repo: Repo, lines: string[]): Buffer {
  const wants: string[] = [];
  const haves: string[] = [];
  let depth: number | undefined;
  for (const line of lines) {
    if (line.startsWith('want ')) wants.push(line.slice(5).split(' ')[0]);
    else if (line.startsWith('have ')) haves.push(line.slice(5).trim());
    else if (line.startsWith('deepen ')) depth = Number(line.slice(7));
    else if (line === 'done') break;
  }
  const { send, shallowOut } = planFetch(repo, wants, haves, depth, []);
  const parts: Buffer[] = [];
  const acked = haves.filter((have) => {
    const closure = new Set<string>();
    for (const want of wants) collectReachable(repo, [want], closure);
    return closure.has(have);
  });
  parts.push(pktLine(acked.length === 0 ? 'NAK\n' : `ACK ${acked[0]}\n`));
  for (const oid of shallowOut) parts.push(pktLine(`shallow ${oid}\n`));
  const pack = buildPack([...send.entries()].map(([oid, obj]) => ({ oid, type: obj.type, data: obj.data })));
  parts.push(sidebandPack(pack));
  return Buffer.concat(parts);
}

export function handleUploadPack(store: Store, req: GitRequest): GitResponse {
  const repo = store.getRepo(req.owner, req.repoName);
  if (repo === undefined || req.owner !== 'adaiversen') {
    return { status: 404, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8') };
  }
  const isV2 = (req.headers['git-protocol'] ?? '').includes('version=2');
  const { packets } = decodePackets(req.body);
  if (isV2) {
    const args = parseV2Args(packets);
    if (args.command === 'ls-refs') {
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: lsRefsResponse(repo, args) };
    }
    if (args.command === 'fetch') {
      return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchV2Response(repo, args) };
    }
    return { status: 400, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'unknown command' }), 'utf8') };
  }
  const lines: string[] = [];
  for (const packet of packets) {
    if (packet.kind === 'data') lines.push(packet.payload.toString('utf8').replace(/\n$/, ''));
  }
  return { status: 200, contentType: 'application/x-git-upload-pack-result', body: fetchV0Response(repo, lines) };
}

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function handleReceivePack(store: Store, req: GitRequest): GitResponse {
  const repo = store.getRepo(req.owner, req.repoName);
  if (repo === undefined || req.owner !== 'adaiversen') {
    return { status: 404, contentType: 'application/json', body: Buffer.from(JSON.stringify({ message: 'Not Found' }), 'utf8') };
  }
  const { packets, rest } = decodePackets(req.body);
  const commands: PushCommand[] = [];
  for (const packet of packets) {
    if (packet.kind === 'flush') break;
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8');
    const nul = line.indexOf('\0');
    if (nul >= 0) line = line.slice(0, nul);
    const parts = line.replace(/\n$/, '').split(' ');
    if (parts.length >= 3) commands.push({ oldOid: parts[0], newOid: parts[1], ref: parts[2] });
  }
  // The pack follows the flush packet; unpack it (if any) before deciding per-ref verdicts.
  let unpacked = 0;
  if (rest.length > 0) {
    try {
      const entries = parsePackSimple(rest, repo.objects);
      for (const [oid, entry] of entries) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, { type: entry.type, data: entry.data });
        unpacked += 1;
      }
    } catch {
      // A corrupt pack is reported as an unpack failure below.
      const report = pktLine(Buffer.concat([Buffer.from([1]), Buffer.from('unpack error\n', 'utf8')]));
      return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat([report, FLUSH]) };
    }
  }
  void unpacked;
  const reportLines: string[] = ['unpack ok\n'];
  for (const command of commands) {
    const current = repo.refs.get(command.ref);
    if (command.newOid === ZERO_OID) {
      if (current === undefined) {
        reportLines.push(`ng ${command.ref} no such ref\n`);
      } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
        reportLines.push(`ng ${command.ref} non-fast-forward\n`);
      } else {
        repo.refs.delete(command.ref);
        reportLines.push(`ok ${command.ref}\n`);
      }
      continue;
    }
    const target = repo.objects.get(command.newOid);
    if (target === undefined) {
      reportLines.push(`ng ${command.ref} missing object\n`);
      continue;
    }
    if (current === undefined) {
      if (command.oldOid !== ZERO_OID) {
        reportLines.push(`ng ${command.ref} non-fast-forward\n`);
        continue;
      }
      repo.refs.set(command.ref, command.newOid);
      reportLines.push(`ok ${command.ref}\n`);
      continue;
    }
    if (command.oldOid !== current) {
      reportLines.push(`ng ${command.ref} non-fast-forward\n`);
      continue;
    }
    if (!isAncestor(repo, current, command.newOid)) {
      reportLines.push(`ng ${command.ref} non-fast-forward\n`);
      continue;
    }
    repo.refs.set(command.ref, command.newOid);
    reportLines.push(`ok ${command.ref}\n`);
  }
  // Report-status travels inside sideband band 1, itself pkt-lined.
  const banded = Buffer.concat(reportLines.map((line) => pktLine(Buffer.concat([Buffer.from([1]), Buffer.from(line, 'utf8')]))));
  return { status: 200, contentType: 'application/x-git-receive-pack-result', body: Buffer.concat([banded, FLUSH]) };
}

/** Parse a packfile that carries whole objects only (the client's writer never emits deltas). */
export function parsePackSimple(buf: Buffer, base: Map<string, GitObject>): Map<string, PackEntry> {
  const out = new Map<string, PackEntry>();
  if (buf.length < 12 || buf.subarray(0, 4).toString('ascii') !== 'PACK') return out;
  const count = buf.readUInt32BE(8);
  let offset = 12;
  const byOffset = new Map<number, PackEntry>();
  for (let i = 0; i < count; i += 1) {
    const start = offset;
    let byte = buf[offset++];
    const type = (byte >> 4) & 0x7;
    let size = byte & 0x0f;
    let shift = 4;
    while (byte & 0x80) {
      byte = buf[offset++];
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    if (type === 6 || type === 7) {
      // Delta object: resolve against the objects already unpacked or stored.
      let baseObj: GitObject | undefined;
      if (type === 7) {
        const baseOid = buf.subarray(offset, offset + 20).toString('hex');
        offset += 20;
        baseObj = out.get(baseOid) ?? base.get(baseOid);
      } else {
        let b = buf[offset++];
        let baseOffset = b & 0x7f;
        while (b & 0x80) {
          b = buf[offset++];
          baseOffset = ((baseOffset + 1) << 7) | (b & 0x7f);
        }
        const baseEntry = byOffset.get(start - baseOffset);
        baseObj = baseEntry !== undefined ? { type: baseEntry.type, data: baseEntry.data } : undefined;
      }
      const data = inflateAt(buf, offset);
      offset += data.consumed;
      if (baseObj === undefined) throw new Error('delta base missing');
      const resolved = applyDelta(baseObj.data, data.inflated);
      const entry: PackEntry = { type: baseObj.type, data: resolved, oid: hashObject(baseObj.type, resolved) };
      out.set(entry.oid, entry);
      byOffset.set(start, entry);
      continue;
    }
    const objectType = CODE_TYPES[type];
    if (objectType === undefined) throw new Error('unknown pack object type');
    const data = inflateAt(buf, offset);
    offset += data.consumed;
    const entry: PackEntry = { type: objectType, data: data.inflated, oid: hashObject(objectType, data.inflated) };
    out.set(entry.oid, entry);
    byOffset.set(start, entry);
  }
  return out;
}

function inflateAt(buf: Buffer, offset: number): { inflated: Buffer; consumed: number } {
  // Find the deflate stream's end by trial inflation (binary search on the prefix length).
  let low = 1;
  let high = buf.length - offset;
  let best = high;
  let bestOut: Buffer | null = null;
  while (low <= high) {
    const mid = (low + high) >> 1;
    try {
      const out = inflateSync(buf.subarray(offset, offset + mid));
      best = mid;
      bestOut = out;
      high = mid - 1;
    } catch {
      low = mid + 1;
    }
  }
  if (bestOut === null) throw new Error('bad deflate stream');
  return { inflated: bestOut, consumed: best };
}
