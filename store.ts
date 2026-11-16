// GitForge store: the in-memory forge. One process, no database, no git binary.
// Written from scratch for the github-core-clone-01 bounty: git objects, refs,
// repositories, issues, pull requests, labels and comments, all in Maps.
//
// The viewer identity below is the deterministic identity the suite's seeded
// scenario names (the kit's fixed scenario seed): the token owner is adaiversen
// and the stranger login is olgabauer. Repositories belong to the token owner;
// any other owner reads as 404, never 403.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export const ZERO_OID = '0'.repeat(40);
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const STRANGER_LOGIN = 'olgabauer';
export const DEFAULT_BRANCH = 'main';
export const FORGE_NAME = 'GitForge';
export const FORGE_VERSION = '1.0.0';

export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

/** The git object id of `type` + `data` (`"<type> <size>\\0<data>"` hashed). */
export function hashObject(type: GitObjectType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return sha1Hex(Buffer.concat([head, data]));
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export interface CommitInfo {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  authorTs: number;
  message: string;
}

export interface TagInfo {
  object: string;
  type: string;
  tag: string;
  tagger: string;
}

/** Parse a commit object's header lines (stops at the blank line). */
export function parseCommit(data: Buffer): CommitInfo {
  const text = data.toString('utf8');
  const end = text.indexOf('\n\n');
  const head = end === -1 ? text : text.slice(0, end);
  const message = end === -1 ? '' : text.slice(end + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  const tsOf = (line: string): number => {
    const m = /> (\d+) /.exec(line);
    return m ? Number(m[1]) : 0;
  };
  return { tree, parents, author, committer, authorTs: tsOf(author), message };
}

/** Parse a tree object into its entries (mode SP name NUL 20-byte oid). */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let off = 0;
  while (off < data.length) {
    const sp = data.indexOf(0x20, off);
    if (sp === -1) break;
    const nul = data.indexOf(0, sp + 1);
    if (nul === -1 || nul + 21 > data.length) break;
    entries.push({
      mode: data.subarray(off, sp).toString('ascii'),
      name: data.subarray(sp + 1, nul).toString('utf8'),
      oid: data.subarray(nul + 1, nul + 21).toString('hex'),
    });
    off = nul + 21;
  }
  return entries;
}

/** Encode tree entries the way git stores them (sorted by name, dirs as name). */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? `${a.name}/` : a.name;
    const bn = b.mode === '40000' ? `${b.name}/` : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  const parts: Buffer[] = [];
  for (const e of sorted) {
    parts.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    parts.push(Buffer.from(e.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

/** Parse an annotated tag object's headers. */
export function parseTag(data: Buffer): TagInfo {
  const text = data.toString('utf8');
  const end = text.indexOf('\n\n');
  const head = end === -1 ? text : text.slice(0, end);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger };
}

export interface BuildCommitInput {
  tree: string;
  parents: readonly string[];
  message: string;
  author?: string;
  committer?: string;
  timestamp?: number;
}

function identLine(name: string, email: string, ts: number): string {
  return `${name} <${email}> ${ts} +0000`;
}

/** Build a commit object's bytes (message stored with a trailing newline, git-style). */
export function buildCommit(input: BuildCommitInput): Buffer {
  const ts = input.timestamp ?? Math.floor(Date.now() / 1000);
  const author = input.author ?? identLine(VIEWER_NAME, VIEWER_EMAIL, ts);
  const committer = input.committer ?? identLine(VIEWER_NAME, VIEWER_EMAIL, ts);
  const message = input.message.endsWith('\n') ? input.message : `${input.message}\n`;
  let head = `tree ${input.tree}\n`;
  for (const p of input.parents) head += `parent ${p}\n`;
  head += `author ${author}\ncommitter ${committer}\n`;
  return Buffer.from(`${head}\n${message}`, 'utf8');
}

export interface StoredObject {
  type: GitObjectType;
  data: Buffer;
  commit?: CommitInfo;
  entries?: TreeEntry[];
  tag?: TagInfo;
}

export interface Label {
  name: string;
  color: string;
}

export interface Comment {
  id: number;
  body: string;
  user: string;
  createdAt: string;
  seq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  user: string;
  createdAt: string;
  updatedAt: string;
  seq: number;
  comments: Comment[];
  json: string | null;
}

export interface Pull {
  id: number;
  number: number;
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
  user: string;
  createdAt: string;
  updatedAt: string;
  seq: number;
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdAt: string;
  seq: number;
  refs: Map<string, string>;
  labels: Map<string, Label>;
  issues: Map<number, Issue>;
  pulls: Map<number, Pull>;
  nextNumber: number;
  fetchCache: Map<string, Buffer>;
  advV2: Buffer | null;
  advV0Up: Buffer | null;
  advV0Recv: Buffer | null;
}

const isoNow = (): string => new Date().toISOString();

/** Opaque Relay-style global id: base64url of a typed payload, never the bare number. */
export function gid(kind: string, ...parts: Array<string | number>): string {
  return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
}

export function ungid(id: string): { kind: string; parts: string[] } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const sep = text.indexOf(':');
    if (sep < 1) return null;
    return { kind: text.slice(0, sep), parts: text.slice(sep + 1).split(':') };
  } catch {
    return null;
  }
}

export { deflateSync, inflateSync };

/** The whole forge state. Single-threaded: no locks, Maps only, no I/O. */
export class Store {
  objects = new Map<string, StoredObject>();
  repos = new Map<string, Repo>();
  nextRepoId = 1;
  nextItemId = 1;
  nextSeq = 1;
  private flatCache = new Map<string, Map<string, { oid: string; mode: string }>>();

  // ------------------------------------------------------------ git objects

  putObject(type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) {
      const obj: StoredObject = { type, data };
      if (type === 'commit') obj.commit = parseCommit(data);
      if (type === 'tag') obj.tag = parseTag(data);
      this.objects.set(oid, obj);
    }
    return oid;
  }

  getObject(oid: string): StoredObject | undefined {
    return this.objects.get(oid);
  }

  treeEntries(oid: string): TreeEntry[] {
    const obj = this.objects.get(oid);
    if (!obj || obj.type !== 'tree') return [];
    if (obj.entries === undefined) obj.entries = parseTree(obj.data);
    return obj.entries;
  }

  /** Peel a tag (possibly nested) to its commit oid. */
  peelToCommit(oid: string): string {
    let cur = oid;
    for (let i = 0; i < 8; i += 1) {
      const obj = this.objects.get(cur);
      if (!obj || obj.type !== 'tag' || !obj.tag) return cur;
      cur = obj.tag.object;
    }
    return cur;
  }

  /** True when `ancestor` is reachable from `tip` (parents walk, capped). */
  isAncestor(ancestor: string, tip: string): boolean {
    if (ancestor === tip) return true;
    const seen = new Set<string>();
    const stack: string[] = [tip];
    let steps = 0;
    while (stack.length > 0) {
      const cur = stack.pop()!;
      if (cur === ancestor) return true;
      if (seen.has(cur)) continue;
      seen.add(cur);
      if (++steps > 200_000) return false;
      const obj = this.objects.get(cur);
      if (obj?.commit) for (const p of obj.commit.parents) stack.push(p);
    }
    return false;
  }

  /** Best common ancestor of two commits (BFS from `b` over `a`'s ancestor set). */
  mergeBase(a: string, b: string): string | null {
    const ancestors = new Set<string>();
    const stack: string[] = [a];
    let steps = 0;
    while (stack.length > 0) {
      const cur = stack.pop()!;
      if (ancestors.has(cur)) continue;
      ancestors.add(cur);
      if (++steps > 200_000) break;
      const obj = this.objects.get(cur);
      if (obj?.commit) for (const p of obj.commit.parents) stack.push(p);
    }
    const queue: string[] = [b];
    const seen = new Set<string>();
    while (queue.length > 0) {
      const cur = queue.shift()!;
      if (ancestors.has(cur)) return cur;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const obj = this.objects.get(cur);
      if (obj?.commit) for (const p of obj.commit.parents) queue.push(p);
    }
    return null;
  }

  /** Flat path map of a tree (memoized; trees are immutable). */
  flattenTree(treeOid: string): Map<string, { oid: string; mode: string }> {
    const hit = this.flatCache.get(treeOid);
    if (hit) return hit;
    const flat = new Map<string, { oid: string; mode: string }>();
    const walk = (oid: string, prefix: string): void => {
      for (const e of this.treeEntries(oid)) {
        const path = prefix === '' ? e.name : `${prefix}/${e.name}`;
        if (e.mode === '40000') walk(e.oid, path);
        else flat.set(path, { oid: e.oid, mode: e.mode });
      }
    };
    walk(treeOid, '');
    if (this.flatCache.size > 4000) this.flatCache.clear();
    this.flatCache.set(treeOid, flat);
    return flat;
  }

  /** Build (and store) nested trees for a flat path map; returns the root tree oid. */
  buildTreeFromFlat(flat: ReadonlyMap<string, { oid: string; mode: string }>): string {
    const buildDir = (prefix: string): string => {
      const entries: TreeEntry[] = [];
      const dirs = new Set<string>();
      for (const [path, v] of flat) {
        if (prefix !== '' && !path.startsWith(`${prefix}/`)) continue;
        const rest = prefix === '' ? path : path.slice(prefix.length + 1);
        const slash = rest.indexOf('/');
        if (slash === -1) entries.push({ mode: v.mode, name: rest, oid: v.oid });
        else dirs.add(rest.slice(0, slash));
      }
      for (const d of dirs) {
        const sub = prefix === '' ? d : `${prefix}/${d}`;
        entries.push({ mode: '40000', name: d, oid: buildDir(sub) });
      }
      return this.putObject('tree', buildTree(entries));
    };
    return buildDir('');
  }

  /** Create and store a commit object; returns its oid. */
  createCommit(tree: string, parents: readonly string[], message: string): string {
    return this.putObject('commit', buildCommit({ tree, parents, message }));
  }

  /**
   * Objects reachable from `wants`, minus objects reachable from `haves`
   * (haves traversal stops at `shallow` boundaries). `depth` caps commit
   * generations (1 = the wanted tips). Returns the objects to send plus the
   * new shallow boundary list and the boundaries that opened up (unshallow).
   */
  collectForFetch(
    wants: readonly string[],
    haves: readonly string[],
    shallowIn: readonly string[],
    depth: number | null,
  ): { objects: Map<string, StoredObject>; shallow: string[]; unshallow: string[] } {
    const shallowSet = new Set(shallowIn);
    // Exclusion set: everything the client already holds.
    const excluded = new Set<string>();
    {
      const stack: string[] = [...haves];
      while (stack.length > 0) {
        const cur = stack.pop()!;
        if (excluded.has(cur)) continue;
        excluded.add(cur);
        const obj = this.objects.get(cur);
        if (!obj) continue;
        if (obj.type === 'commit') {
          if (obj.commit) {
            stack.push(obj.commit.tree);
            // A shallow boundary still holds its own tree and blobs; only its
            // parents are cut off, so they stay out of the exclusion walk.
            if (!shallowSet.has(cur)) {
              for (const p of obj.commit.parents) stack.push(p);
            }
          }
        } else if (obj.type === 'tree') {
          for (const e of this.treeEntries(cur)) stack.push(e.oid);
        } else if (obj.type === 'tag' && obj.tag) {
          stack.push(obj.tag.object);
        }
      }
    }
    // Wanted set: BFS from wants, tracking commit generation depth.
    const wanted = new Map<string, StoredObject>();
    const shallowSetOut = new Set<string>();
    const queue: Array<{ oid: string; depth: number }> = [];
    const seenCommitDepth = new Map<string, number>();
    const addObj = (oid: string): StoredObject | undefined => {
      const obj = this.objects.get(oid);
      if (obj && !wanted.has(oid)) wanted.set(oid, obj);
      return obj;
    };
    for (const w of wants) queue.push({ oid: w, depth: 1 });
    let qi = 0;
    while (qi < queue.length) {
      const { oid, depth: d } = queue[qi++]!;
      const obj = this.objects.get(oid);
      if (!obj) continue;
      if (obj.type === 'commit') {
        const prev = seenCommitDepth.get(oid);
        if (prev !== undefined && prev <= d) continue;
        seenCommitDepth.set(oid, d);
        addObj(oid);
        // A boundary commit still holds its own tree and blobs; only its
        // parents are cut off, so the tree goes in before the cutoff.
        if (obj.commit) {
          addObj(obj.commit.tree);
          this.pushTreeContents(obj.commit.tree, addObj);
        }
        if (depth !== null && d >= depth) {
          shallowSetOut.add(oid); // boundary: do not expand parents
          continue;
        }
        shallowSetOut.delete(oid); // reached at an interior depth: no longer a boundary
        if (obj.commit) {
          for (const p of obj.commit.parents) queue.push({ oid: p, depth: d + 1 });
        }
      } else if (obj.type === 'tag') {
        addObj(oid);
        if (obj.tag) queue.push({ oid: obj.tag.object, depth: d });
      } else if (obj.type === 'tree') {
        addObj(oid);
        this.pushTreeContents(oid, addObj);
      } else {
        addObj(oid);
      }
    }
    const objects = new Map<string, StoredObject>();
    for (const [oid, obj] of wanted) if (!excluded.has(oid)) objects.set(oid, obj);
    const unshallow: string[] = [];
    for (const old of shallowIn) {
      if (seenCommitDepth.has(old) && !shallowSetOut.has(old)) unshallow.push(old);
    }
    return { objects, shallow: [...shallowSetOut], unshallow };
  }

  private pushTreeContents(treeOid: string, add: (oid: string) => StoredObject | undefined): void {
    const stack: string[] = [treeOid];
    while (stack.length > 0) {
      const cur = stack.pop()!;
      add(cur);
      for (const e of this.treeEntries(cur)) {
        add(e.oid);
        if (e.mode === '40000') stack.push(e.oid);
      }
    }
  }

  // ------------------------------------------------------------ repositories

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  createRepo(owner: string, name: string, description: string, isPrivate: boolean): Repo | null {
    const key = this.repoKey(owner, name);
    if (this.repos.has(key)) return null;
    const repo: Repo = {
      id: this.nextRepoId++,
      owner,
      name,
      description,
      private: isPrivate,
      defaultBranch: DEFAULT_BRANCH,
      createdAt: isoNow(),
      seq: this.nextSeq++,
      refs: new Map(),
      labels: new Map(),
      issues: new Map(),
      pulls: new Map(),
      nextNumber: 1,
      fetchCache: new Map(),
      advV2: null,
      advV0Up: null,
      advV0Recv: null,
    };
    this.repos.set(key, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  listViewerRepos(): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner === VIEWER_LOGIN) out.push(repo);
    out.sort((a, b) => a.seq - b.seq);
    return out;
  }

  invalidateRepoCache(repo: Repo): void {
    repo.fetchCache.clear();
    repo.advV2 = null;
    repo.advV0Up = null;
    repo.advV0Recv = null;
  }

  setRef(repo: Repo, ref: string, oid: string | null): void {
    if (oid === null) repo.refs.delete(ref);
    else repo.refs.set(ref, oid);
    this.invalidateRepoCache(repo);
  }

  refTip(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  defaultTip(repo: Repo): string | undefined {
    return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  }

  // ------------------------------------------------------------ issues / pulls

  createIssue(repo: Repo, title: string, body: string, labels: readonly string[]): Issue | { error: string } {
    for (const name of labels) {
      if (!repo.labels.has(name)) return { error: `label "${name}" does not exist` };
    }
    const now = isoNow();
    const issue: Issue = {
      id: this.nextItemId++,
      number: repo.nextNumber++,
      title,
      body: body ?? '',
      state: 'open',
      labels: [...labels],
      user: VIEWER_LOGIN,
      createdAt: now,
      updatedAt: now,
      seq: this.nextSeq++,
      comments: [],
      json: null,
    };
    repo.issues.set(issue.number, issue);
    return issue;
  }

  createPull(repo: Repo, title: string, body: string, head: string, base: string): Pull | { error: string } {
    const headTip = repo.refs.get(`refs/heads/${head}`);
    const baseTip = repo.refs.get(`refs/heads/${base}`);
    if (headTip === undefined) return { error: `head branch "${head}" does not exist` };
    if (baseTip === undefined) return { error: `base branch "${base}" does not exist` };
    for (const pr of repo.pulls.values()) {
      if (pr.state === 'open' && pr.headRef === head && pr.baseRef === base) {
        return { error: 'a pull request already exists for this head and base' };
      }
    }
    const now = isoNow();
    const pr: Pull = {
      id: this.nextItemId++,
      number: repo.nextNumber++,
      title,
      body: body ?? '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef: head,
      baseRef: base,
      headSha: headTip,
      baseSha: baseTip,
      user: VIEWER_LOGIN,
      createdAt: now,
      updatedAt: now,
      seq: this.nextSeq++,
    };
    repo.pulls.set(pr.number, pr);
    return pr;
  }

  /** Diff of two trees as a flat path list (merge-base vs head for PR files). */
  diffTrees(baseTree: string | null, headTree: string | null): Array<{ path: string; status: string; oldOid: string | null; newOid: string | null }> {
    const base = baseTree === null ? new Map() : this.flattenTree(baseTree);
    const head = headTree === null ? new Map() : this.flattenTree(headTree);
    const out: Array<{ path: string; status: string; oldOid: string | null; newOid: string | null }> = [];
    const paths = new Set([...base.keys(), ...head.keys()]);
    for (const path of paths) {
      const b = base.get(path);
      const h = head.get(path);
      if (b && !h) out.push({ path, status: 'removed', oldOid: b.oid, newOid: null });
      else if (!b && h) out.push({ path, status: 'added', oldOid: null, newOid: h.oid });
      else if (b && h && b.oid !== h.oid) out.push({ path, status: 'modified', oldOid: b.oid, newOid: h.oid });
    }
    out.sort((a, b) => (a.path < b.path ? -1 : 1));
    return out;
  }

  /** Files changed by a pull request (merge-base tree vs head tip tree). */
  pullFiles(repo: Repo, pr: Pull): Array<{ path: string; status: string; oldOid: string | null; newOid: string | null }> {
    const headTip = repo.refs.get(`refs/heads/${pr.headRef}`);
    const baseTip = repo.refs.get(`refs/heads/${pr.baseRef}`);
    if (headTip === undefined || baseTip === undefined) return [];
    const base = this.mergeBase(headTip, baseTip);
    const baseTree = base === null ? null : this.objects.get(base)?.commit?.tree ?? null;
    const headTree = this.objects.get(headTip)?.commit?.tree ?? null;
    return this.diffTrees(baseTree, headTree);
  }

  /** 'MERGEABLE' | 'CONFLICTING' for a pull request's current tips. */
  pullMergeable(repo: Repo, pr: Pull): 'MERGEABLE' | 'CONFLICTING' {
    const headTip = repo.refs.get(`refs/heads/${pr.headRef}`);
    const baseTip = repo.refs.get(`refs/heads/${pr.baseRef}`);
    if (headTip === undefined || baseTip === undefined) return 'CONFLICTING';
    return this.threeWay(repo, baseTip, headTip).conflict ? 'CONFLICTING' : 'MERGEABLE';
  }

  /** Three-way merge of baseTip (ours) with headTip (theirs) over their merge-base. */
  threeWay(repo: Repo, baseTip: string, headTip: string): { conflict: boolean; treeOid: string | null } {
    const base = this.mergeBase(headTip, baseTip);
    const baseTree = base === null ? null : this.objects.get(base)?.commit?.tree ?? null;
    const oursTree = this.objects.get(baseTip)?.commit?.tree ?? null;
    const theirsTree = this.objects.get(headTip)?.commit?.tree ?? null;
    const b = baseTree === null ? new Map() : this.flattenTree(baseTree);
    const o = oursTree === null ? new Map() : this.flattenTree(oursTree);
    const t = theirsTree === null ? new Map() : this.flattenTree(theirsTree);
    const merged = new Map<string, { oid: string; mode: string }>();
    let conflict = false;
    const paths = new Set([...b.keys(), ...o.keys(), ...t.keys()]);
    for (const path of paths) {
      const bv = b.get(path);
      const ov = o.get(path);
      const tv = t.get(path);
      const eq = (x?: { oid: string }, y?: { oid: string }): boolean =>
        (x === undefined && y === undefined) || (x !== undefined && y !== undefined && x.oid === y.oid);
      if (eq(ov, tv)) {
        if (ov) merged.set(path, ov);
      } else if (eq(ov, bv)) {
        if (tv) merged.set(path, tv);
      } else if (eq(tv, bv)) {
        if (ov) merged.set(path, ov);
      } else {
        conflict = true;
      }
    }
    if (conflict) return { conflict: true, treeOid: null };
    return { conflict: false, treeOid: this.buildTreeFromFlat(merged) };
  }

  /** Squash-merge a pull request onto its base branch. */
  mergePull(repo: Repo, pr: Pull): { ok: true; sha: string } | { ok: false; reason: string } {
    if (pr.state !== 'open') return { ok: false, reason: 'pull request is already closed' };
    const headTip = repo.refs.get(`refs/heads/${pr.headRef}`);
    const baseTip = repo.refs.get(`refs/heads/${pr.baseRef}`);
    if (headTip === undefined || baseTip === undefined) return { ok: false, reason: 'head or base branch is missing' };
    const merged = this.threeWay(repo, baseTip, headTip);
    if (merged.conflict || merged.treeOid === null) return { ok: false, reason: 'merge conflict' };
    const sha = this.createCommit(merged.treeOid, [baseTip], `Merge pull request #${pr.number} from ${pr.headRef}`);
    this.setRef(repo, `refs/heads/${pr.baseRef}`, sha);
    pr.state = 'closed';
    pr.merged = true;
    pr.mergedAt = isoNow();
    pr.mergeCommitSha = sha;
    pr.updatedAt = isoNow();
    return { ok: true, sha };
  }

  /** Commits reachable from `tip`, newest first (DFS following parents). */
  listCommits(tip: string, limit = 1000): Array<{ sha: string; commit: CommitInfo }> {
    const out: Array<{ sha: string; commit: CommitInfo }> = [];
    const seen = new Set<string>();
    const stack: string[] = [tip];
    while (stack.length > 0 && out.length < limit) {
      const cur = stack.pop()!;
      if (seen.has(cur)) continue;
      seen.add(cur);
      const obj = this.objects.get(cur);
      if (!obj?.commit) continue;
      out.push({ sha: cur, commit: obj.commit });
      for (let i = obj.commit.parents.length - 1; i >= 0; i -= 1) stack.push(obj.commit.parents[i]);
    }
    return out;
  }
}
