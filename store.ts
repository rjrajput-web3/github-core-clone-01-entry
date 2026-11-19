import { createHash } from 'node:crypto';

/** The login the trial token names, from the kit's fixed scenario seed. */
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'Ada Iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const ZERO_OID = '0000000000000000000000000000000000000000';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjectType;
  data: Buffer;
}

export interface Label {
  name: string;
  color: string;
}

export interface IssueComment {
  id: number;
  body: string;
  seq: number;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  seq: number;
  comments: IssueComment[];
}

export interface PullFile {
  filename: string;
  status: 'added' | 'modified' | 'removed';
  additions: number;
  deletions: number;
  changes: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeSha: string | null;
  head: string;
  base: string;
  headSha: string;
  baseSha: string;
  baseShaAtCreate: string;
  files: PullFile[];
  seq: number;
}

export interface Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  isPrivate: boolean;
  defaultBranch: string;
  seq: number;
  refs: Map<string, string>;
  objects: Map<string, GitObject>;
  labels: Map<string, Label>;
  issues: Issue[];
  pulls: Pull[];
  nextNumber: number;
}

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: ObjectType, data: Buffer): string {
  const head = Buffer.from(`${type} ${data.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([head, data]));
}

export function globalId(kind: string, value: string): string {
  return Buffer.from(`${kind}:${value}`, 'utf8').toString('base64url');
}

export function parseGlobalId(id: string): { kind: string; value: string } | null {
  try {
    const text = Buffer.from(id, 'base64url').toString('utf8');
    const colon = text.indexOf(':');
    if (colon < 1) return null;
    return { kind: text.slice(0, colon), value: text.slice(colon + 1) };
  } catch {
    return null;
  }
}

/** A monotonically increasing clock: every timestamp derives from a counter, never the wall clock. */
export class Clock {
  private seq = 0;
  next(): number {
    this.seq += 1;
    return this.seq;
  }
  iso(seq: number): string {
    return new Date((1735689600 + seq) * 1000).toISOString();
  }
  gitStamp(seq: number): number {
    return 1735689600 + seq;
  }
}

export class Store {
  readonly clock = new Clock();
  readonly repos = new Map<string, Repo>();
  private nextRepoId = 5000;
  private nextItemId = 100000;

  repoKey(owner: string, name: string): string {
    return `${owner}/${name}`;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.repoKey(owner, name));
  }

  createRepo(name: string, isPrivate: boolean, description: string, defaultBranch: string): Repo | null {
    if (this.getRepo(VIEWER_LOGIN, name) !== undefined) return null;
    const seq = this.clock.next();
    const repo: Repo = {
      id: this.nextRepoId++,
      name,
      owner: VIEWER_LOGIN,
      description,
      isPrivate,
      defaultBranch: defaultBranch === '' ? 'main' : defaultBranch,
      seq,
      refs: new Map(),
      objects: new Map(),
      labels: new Map(),
      issues: [],
      pulls: [],
      nextNumber: 1,
    };
    this.repos.set(this.repoKey(VIEWER_LOGIN, name), repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.repoKey(owner, name));
  }

  listRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => a.seq - b.seq);
  }

  nextItem(): number {
    this.nextItemId += 1;
    return this.nextItemId;
  }

  putObject(repo: Repo, type: ObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  issueGlobalId(repo: Repo, issue: Issue): string {
    return globalId('issue', `${repo.owner}/${repo.name}:${issue.number}`);
  }

  pullGlobalId(repo: Repo, pull: Pull): string {
    return globalId('pull', `${repo.owner}/${repo.name}:${pull.number}`);
  }

  repoGlobalId(repo: Repo): string {
    return globalId('repo', `${repo.owner}/${repo.name}`);
  }

  commentGlobalId(repo: Repo, issue: Issue, comment: IssueComment): string {
    return globalId('comment', `${repo.owner}/${repo.name}:${issue.number}:${comment.id}`);
  }

  findByGlobalId(id: string): { kind: string; repo: Repo; issue?: Issue; pull?: Pull } | null {
    const parsed = parseGlobalId(id);
    if (parsed === null) return null;
    if (parsed.kind === 'repo') {
      const slash = parsed.value.indexOf('/');
      if (slash < 0) return null;
      const repo = this.getRepo(parsed.value.slice(0, slash), parsed.value.slice(slash + 1));
      return repo === undefined ? null : { kind: 'repo', repo };
    }
    const match = /^([^/]+)\/([^:]+):(\d+)$/.exec(parsed.value);
    if (match === null) return null;
    const repo = this.getRepo(match[1], match[2]);
    if (repo === undefined) return null;
    const number = Number(match[3]);
    if (parsed.kind === 'issue') {
      const issue = repo.issues.find((entry) => entry.number === number);
      return issue === undefined ? null : { kind: 'issue', repo, issue };
    }
    if (parsed.kind === 'pull') {
      const pull = repo.pulls.find((entry) => entry.number === number);
      return pull === undefined ? null : { kind: 'pull', repo, pull };
    }
    return null;
  }
}

// -------------------------------------------------------------------------------------------
// git object parsing and construction
// -------------------------------------------------------------------------------------------

export interface ParsedCommit {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2).replace(/\n+$/, '');
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
  return { tree, parents, author, committer, message };
}

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
    const mode = data.subarray(offset, space).toString('ascii');
    const nul = data.indexOf(0, space + 1);
    if (nul < 0) break;
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const parts: Buffer[] = [];
  const sorted = [...entries].sort((a, b) => {
    const an = a.mode === '40000' ? `${a.name}/` : a.name;
    const bn = b.mode === '40000' ? `${b.name}/` : b.name;
    return an < bn ? -1 : an > bn ? 1 : 0;
  });
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export function buildCommit(tree: string, parents: string[], message: string, stamp: number): Buffer {
  const identity = `${VIEWER_NAME} <${VIEWER_EMAIL}> ${stamp} +0000`;
  const lines: string[] = [`tree ${tree}`];
  for (const parent of parents) lines.push(`parent ${parent}`);
  lines.push(`author ${identity}`, `committer ${identity}`, '');
  return Buffer.from(`${lines.join('\n')}\n${message.endsWith('\n') ? message : `${message}\n`}`, 'utf8');
}

export function parseTag(data: Buffer): { object: string; type: string; tag: string; tagger: string } {
  const text = data.toString('utf8');
  const head = text.split('\n\n')[0] ?? '';
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

/** Flatten a tree object into path -> {oid, mode} for every file it holds. */
export function flattenTree(repo: Repo, treeOid: string, prefix: string, out: Map<string, { oid: string; mode: string }>): void {
  const obj = repo.objects.get(treeOid);
  if (obj === undefined || obj.type !== 'tree') return;
  for (const entry of parseTree(obj.data)) {
    const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
    if (entry.mode === '40000') flattenTree(repo, entry.oid, path, out);
    else out.set(path, { oid: entry.oid, mode: entry.mode });
  }
}

/** Build (and store) a tree object from a flat path -> blob-oid map. */
export function buildTreeFromFiles(repo: Repo, files: Map<string, { oid: string; mode: string }>): string {
  interface Node {
    files: Map<string, { oid: string; mode: string }>;
    dirs: Map<string, Node>;
  }
  const make = (): Node => ({ files: new Map(), dirs: new Map() });
  const root = make();
  for (const [path, value] of files) {
    const parts = path.split('/');
    let node = root;
    for (let i = 0; i < parts.length - 1; i += 1) {
      const part = parts[i];
      let child = node.dirs.get(part);
      if (child === undefined) {
        child = make();
        node.dirs.set(part, child);
      }
      node = child;
    }
    node.files.set(parts[parts.length - 1], value);
  }
  const build = (node: Node): string => {
    const entries: TreeEntry[] = [];
    for (const [name, value] of node.files) entries.push({ mode: value.mode, name, oid: value.oid });
    for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
    const data = buildTree(entries);
    return storeObject(repo, 'tree', data);
  };
  return build(root);
}

export function storeObject(repo: Repo, type: ObjectType, data: Buffer): string {
  const oid = hashObject(type, data);
  if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
  return oid;
}

/** Create a commit object on top of a tree, store it, return its oid. */
export function makeCommit(repo: Repo, store: Store, treeOid: string, parents: string[], message: string): string {
  const seq = store.clock.next();
  const data = buildCommit(treeOid, parents, message, store.clock.gitStamp(seq));
  return storeObject(repo, 'commit', data);
}

/** Walk a commit's history (first-parent and all parents) and collect reachable object ids. */
export function collectReachable(repo: Repo, tips: Iterable<string>, out: Set<string>): void {
  const queue: string[] = [...tips];
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (oid === ZERO_OID || out.has(oid)) continue;
    const obj = repo.objects.get(oid);
    if (obj === undefined) continue;
    out.add(oid);
    if (obj.type === 'commit') {
      const parsed = parseCommit(obj.data);
      queue.push(parsed.tree);
      for (const parent of parsed.parents) queue.push(parent);
    } else if (obj.type === 'tree') {
      for (const entry of parseTree(obj.data)) queue.push(entry.oid);
    } else if (obj.type === 'tag') {
      queue.push(parseTag(obj.data).object);
    }
  }
}

/** Peel a ref target to a commit oid (through tag objects). */
export function peelToCommit(repo: Repo, oid: string): string {
  let current = oid;
  for (let i = 0; i < 16; i += 1) {
    const obj = repo.objects.get(current);
    if (obj === undefined || obj.type !== 'tag') return current;
    current = parseTag(obj.data).object;
  }
  return current;
}

/** True when `ancestor` is reachable from `tip` (or equal). */
export function isAncestor(repo: Repo, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) queue.push(parent);
  }
  return false;
}

/** Commit distance (generations) from any of `tips`, for commits reachable from them. */
export function commitDepths(repo: Repo, tips: Iterable<string>): Map<string, number> {
  const depths = new Map<string, number>();
  const queue: Array<[string, number]> = [...tips].map((tip) => [tip, 0] as [string, number]);
  while (queue.length > 0) {
    const [oid, depth] = queue.shift() as [string, number];
    const known = depths.get(oid);
    if (known !== undefined && known <= depth) continue;
    depths.set(oid, depth);
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    for (const parent of parseCommit(obj.data).parents) queue.push([parent, depth + 1]);
  }
  return depths;
}

export function lineCount(text: string): number {
  if (text === '') return 0;
  const parts = text.split('\n');
  if (parts.length > 0 && parts[parts.length - 1] === '') parts.pop();
  return parts.length;
}

/** A small line diff: additions/deletions between two blob texts. */
export function diffLines(oldText: string, newText: string): { additions: number; deletions: number } {
  const oldLines = new Set(oldText.split('\n'));
  const newLines = new Set(newText.split('\n'));
  let additions = 0;
  let deletions = 0;
  for (const line of newText.split('\n')) if (!oldLines.has(line)) additions += 1;
  for (const line of oldText.split('\n')) if (!newLines.has(line)) deletions += 1;
  if (additions === 0 && deletions === 0 && oldText !== newText) {
    additions = 1;
    deletions = 1;
  }
  return { additions, deletions };
}

// -------------------------------------------------------------------------------------------
// pull-request helpers shared by the REST, GraphQL and MCP surfaces
// -------------------------------------------------------------------------------------------

export function refTip(repo: Repo, branch: string): string | undefined {
  return repo.refs.get(`refs/heads/${branch}`);
}

export function treeFiles(repo: Repo, tip: string | undefined): Map<string, { oid: string; mode: string }> {
  const out = new Map<string, { oid: string; mode: string }>();
  if (tip === undefined) return out;
  const obj = repo.objects.get(tip);
  if (obj === undefined || obj.type !== 'commit') return out;
  flattenTree(repo, parseCommit(obj.data).tree, '', out);
  return out;
}

export function computePullFiles(store: Store, repo: Repo, pull: Pull): void {
  const baseFiles = treeFiles(repo, pull.baseShaAtCreate);
  const headFiles = treeFiles(repo, pull.headSha);
  const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
  const files: Pull['files'] = [];
  for (const path of [...paths].sort()) {
    const base = baseFiles.get(path);
    const head = headFiles.get(path);
    if (base !== undefined && head !== undefined && base.oid === head.oid) continue;
    if (base === undefined && head !== undefined) {
      const text = repo.objects.get(head.oid)?.data.toString('utf8') ?? '';
      const additions = Math.max(1, lineCount(text));
      files.push({ filename: path, status: 'added', additions, deletions: 0, changes: additions });
    } else if (base !== undefined && head === undefined) {
      const text = repo.objects.get(base.oid)?.data.toString('utf8') ?? '';
      const deletions = Math.max(1, lineCount(text));
      files.push({ filename: path, status: 'removed', additions: 0, deletions, changes: deletions });
    } else if (base !== undefined && head !== undefined) {
      const oldText = repo.objects.get(base.oid)?.data.toString('utf8') ?? '';
      const newText = repo.objects.get(head.oid)?.data.toString('utf8') ?? '';
      const { additions, deletions } = diffLines(oldText, newText);
      files.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
    }
  }
  pull.files = files;
}

export function pullConflicts(store: Store, repo: Repo, pull: Pull): boolean {
  if (pull.state !== 'open') return false;
  const baseAtCreate = treeFiles(repo, pull.baseShaAtCreate);
  const baseNow = treeFiles(repo, refTip(repo, pull.base));
  const headNow = treeFiles(repo, pull.headSha);
  for (const file of pull.files) {
    const path = file.filename;
    const atCreate = baseAtCreate.get(path)?.oid ?? null;
    const now = baseNow.get(path)?.oid ?? null;
    const head = headNow.get(path)?.oid ?? null;
    if (now !== atCreate && head !== now) return true;
  }
  return false;
}

export function squashMerge(store: Store, repo: Repo, pull: Pull): { sha: string } | { conflict: true } {
  if (pullConflicts(store, repo, pull)) return { conflict: true };
  const baseTip = refTip(repo, pull.base);
  if (baseTip === undefined) return { conflict: true };
  const baseFiles = treeFiles(repo, baseTip);
  const headFiles = treeFiles(repo, pull.headSha);
  for (const file of pull.files) {
    if (file.status === 'removed') baseFiles.delete(file.filename);
    else {
      const entry = headFiles.get(file.filename);
      if (entry !== undefined) baseFiles.set(file.filename, entry);
    }
  }
  const treeOid = buildTreeFromFiles(repo, baseFiles);
  const sha = makeCommit(repo, store, treeOid, [baseTip], `${pull.title} (#${pull.number})`);
  repo.refs.set(`refs/heads/${pull.base}`, sha);
  return { sha };
}
