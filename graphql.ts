// graphql.ts — a small GraphQL v4 implementation: parser, executor and the schema
// surface the suite and github-mcp-server-style clients use (viewer, repository,
// issues, pull requests, node, __type introspection, and the issue/PR/comment
// mutations). No external dependency.

import {
  VIEWER,
  buildCommit,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import { prFiles, prMergeable } from './rest.ts';

// -------------------------------------------------------------------------------------------
// parser
// -------------------------------------------------------------------------------------------

type Tok =
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'number'; value: string }
  | { kind: 'punct'; value: string }
  | { kind: 'spread'; value: string }
  | { kind: 'var'; value: string };

function tokenize(src: string): Tok[] {
  const toks: Tok[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i]!;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i++;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i++;
      continue;
    }
    if (src.startsWith('...', i)) {
      toks.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if (ch === '$') {
      const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(src.slice(i + 1));
      if (!m) throw new Error('bad variable');
      toks.push({ kind: 'var', value: m[0] });
      i += 1 + m[0].length;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1]!;
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          out += src[j]!;
          j++;
        }
      }
      toks.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (ch === '-' || (ch >= '0' && ch <= '9')) {
      const m = /^-?\d+(\.\d+)?/.exec(src.slice(i));
      if (m) {
        toks.push({ kind: 'number', value: m[0] });
        i += m[0].length;
        continue;
      }
    }
    if (/[A-Za-z_]/.test(ch)) {
      const m = /^[A-Za-z_][A-Za-z0-9_]*/.exec(src.slice(i));
      toks.push({ kind: 'name', value: m![0] });
      i += m![0].length;
      continue;
    }
    if ('{}()[]:!@='.includes(ch)) {
      toks.push({ kind: 'punct', value: ch });
      i++;
      continue;
    }
    throw new Error(`unexpected character ${ch}`);
  }
  return toks;
}

type GqlValue =
  | null
  | boolean
  | number
  | string
  | GqlValue[]
  | { [key: string]: GqlValue }
  | { __var: string }
  | { __enum: string };

export interface FieldNode {
  alias: string | null;
  name: string;
  args: Record<string, GqlValue>;
  selections: FieldNode[] | null;
  typeCondition?: string;
  isFragment?: boolean;
}

class Parser {
  private pos = 0;
  private readonly toks: Tok[];
  constructor(toks: Tok[]) {
    this.toks = toks;
  }

  private peek(): Tok | undefined {
    return this.toks[this.pos];
  }
  private next(): Tok {
    const t = this.toks[this.pos];
    if (t === undefined) throw new Error('unexpected end of query');
    this.pos++;
    return t;
  }
  private expectPunct(v: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== v) throw new Error(`expected ${v}`);
  }
  private expectName(): string {
    const t = this.next();
    if (t.kind !== 'name') throw new Error('expected name');
    return t.value;
  }

  parseDocument(): { operation: 'query' | 'mutation'; selections: FieldNode[] } {
    let operation: 'query' | 'mutation' = 'query';
    const first = this.peek();
    if (first?.kind === 'name' && (first.value === 'query' || first.value === 'mutation')) {
      operation = first.value;
      this.next();
      const maybeName = this.peek();
      if (maybeName?.kind === 'name') this.next();
      // variable definitions: ($x: Type = default, ...)
      if (this.peek()?.kind === 'punct' && this.peek()!.value === '(') {
        this.next();
        let depth = 1;
        while (depth > 0) {
          const t = this.next();
          if (t.kind === 'punct' && t.value === '(') depth++;
          else if (t.kind === 'punct' && t.value === ')') depth--;
        }
      }
      // directives like @cached — skip
      while (this.peek()?.kind === 'punct' && this.peek()!.value === '@') {
        this.next();
        this.expectName();
        if (this.peek()?.kind === 'punct' && this.peek()!.value === '(') {
          this.next();
          let depth = 1;
          while (depth > 0) {
            const t = this.next();
            if (t.kind === 'punct' && t.value === '(') depth++;
            else if (t.kind === 'punct' && t.value === ')') depth--;
          }
        }
      }
    }
    const selections = this.parseSelectionSet();
    return { operation, selections };
  }

  parseSelectionSet(): FieldNode[] {
    this.expectPunct('{');
    const fields: FieldNode[] = [];
    while (true) {
      const t = this.peek();
      if (t === undefined) throw new Error('unterminated selection set');
      if (t.kind === 'punct' && t.value === '}') {
        this.next();
        break;
      }
      if (t.kind === 'spread') {
        this.next();
        const on = this.peek();
        if (on?.kind === 'name' && on.value === 'on') {
          this.next();
          const typeName = this.expectName();
          const sels = this.parseSelectionSet();
          fields.push({ alias: null, name: '__fragment', args: {}, selections: sels, typeCondition: typeName, isFragment: true });
        } else {
          // named fragment spread — not used by the suite; skip its selections
          if (this.peek()?.kind === 'name') this.next();
        }
        continue;
      }
      fields.push(this.parseField());
    }
    return fields;
  }

  parseField(): FieldNode {
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    const maybeColon = this.peek();
    if (maybeColon?.kind === 'punct' && maybeColon.value === ':') {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args: Record<string, GqlValue> = {};
    if (this.peek()?.kind === 'punct' && this.peek()!.value === '(') {
      this.next();
      while (true) {
        const t = this.peek();
        if (t === undefined) throw new Error('unterminated arguments');
        if (t.kind === 'punct' && t.value === ')') {
          this.next();
          break;
        }
        const argName = this.expectName();
        this.expectPunct(':');
        args[argName] = this.parseValue();
      }
    }
    // directives on the field — skip
    while (this.peek()?.kind === 'punct' && this.peek()!.value === '@') {
      this.next();
      this.expectName();
      if (this.peek()?.kind === 'punct' && this.peek()!.value === '(') {
        this.next();
        let depth = 1;
        while (depth > 0) {
          const t = this.next();
          if (t.kind === 'punct' && t.value === '(') depth++;
          else if (t.kind === 'punct' && t.value === ')') depth--;
        }
      }
    }
    let selections: FieldNode[] | null = null;
    if (this.peek()?.kind === 'punct' && this.peek()!.value === '{') {
      selections = this.parseSelectionSet();
    }
    return { alias, name, args, selections };
  }

  parseValue(): GqlValue {
    const t = this.next();
    if (t.kind === 'string') return t.value;
    if (t.kind === 'number') return Number(t.value);
    if (t.kind === 'var') return { __var: t.value };
    if (t.kind === 'name') {
      if (t.value === 'true') return true;
      if (t.value === 'false') return false;
      if (t.value === 'null') return null;
      return { __enum: t.value };
    }
    if (t.kind === 'punct' && t.value === '[') {
      const arr: GqlValue[] = [];
      while (true) {
        const p = this.peek();
        if (p === undefined) throw new Error('unterminated list');
        if (p.kind === 'punct' && p.value === ']') {
          this.next();
          break;
        }
        arr.push(this.parseValue());
      }
      return arr;
    }
    if (t.kind === 'punct' && t.value === '{') {
      const obj: Record<string, GqlValue> = {};
      while (true) {
        const p = this.peek();
        if (p === undefined) throw new Error('unterminated object');
        if (p.kind === 'punct' && p.value === '}') {
          this.next();
          break;
        }
        const key = this.expectName();
        this.expectPunct(':');
        obj[key] = this.parseValue();
      }
      return obj;
    }
    throw new Error(`unexpected value token ${t.kind}`);
  }
}

function resolveVars(value: GqlValue, variables: Record<string, unknown>): unknown {
  if (value !== null && typeof value === 'object' && !Array.isArray(value)) {
    if ('__var' in value) return variables[(value as { __var: string }).__var] ?? null;
    if ('__enum' in value) return (value as { __enum: string }).__enum;
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[k] = resolveVars(v as GqlValue, variables);
    return out;
  }
  if (Array.isArray(value)) return value.map((v) => resolveVars(v, variables));
  return value;
}

function asStr(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function asNum(v: unknown): number | undefined {
  return typeof v === 'number' ? v : undefined;
}

// -------------------------------------------------------------------------------------------
// resolved objects
// -------------------------------------------------------------------------------------------

type GObj =
  | { t: 'Query' }
  | { t: 'Mutation' }
  | { t: 'Viewer' }
  | { t: 'Repository'; repo: Repo }
  | { t: 'Issue'; repo: Repo; issue: Issue }
  | { t: 'PullRequest'; repo: Repo; pr: Pull }
  | { t: 'Comment'; id: number; body: string; repo: Repo }
  | { t: 'Label'; name: string; color: string }
  | { t: 'Ref'; repo: Repo; name: string; oid: string }
  | { t: 'Commit'; repo: Repo; oid: string }
  | { t: 'IssueConnection'; repo: Repo; items: Issue[]; endCursor: string | null; hasNext: boolean }
  | { t: 'PullConnection'; repo: Repo; items: Pull[]; endCursor: string | null; hasNext: boolean }
  | { t: 'FileConnection'; repo: Repo; pr: Pull }
  | { t: 'CommentConnection'; repo: Repo; comments: Array<{ id: number; body: string }> }
  | { t: 'Edge'; node: GObj }
  | { t: 'PageInfo'; hasNext: boolean; endCursor: string | null }
  | { t: 'TypeInfo'; name: string }
  | { t: 'Payload'; fields: Record<string, unknown> };

const TYPE_FIELDS: Record<string, Set<string>> = {
  Query: new Set(['viewer', 'repository', 'node', '__type', '__schema', 'search']),
  Mutation: new Set(['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment']),
  Viewer: new Set(['login', 'id', 'name', 'email', 'avatarUrl', 'url']),
  Repository: new Set([
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description',
    'isPrivate', 'createdAt', 'url', 'issuesCount', 'pullRequestsCount',
  ]),
  Issue: new Set(['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'author', 'comments', 'url', 'labels', 'repository']),
  PullRequest: new Set([
    'id', 'number', 'title', 'body', 'state', 'merged', 'mergedAt', 'mergeable',
    'headRefName', 'baseRefName', 'headRefOid', 'baseRefOid', 'files', 'author',
    'createdAt', 'updatedAt', 'url', 'repository',
  ]),
  Comment: new Set(['id', 'body', 'author', 'createdAt', 'updatedAt']),
  Label: new Set(['id', 'name', 'color', 'description']),
  Ref: new Set(['name', 'prefix', 'target']),
  Commit: new Set(['oid', 'message', 'abbreviatedOid', 'author']),
  IssueConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  PullConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  FileConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  CommentConnection: new Set(['nodes', 'edges', 'pageInfo', 'totalCount']),
  Edge: new Set(['node', 'cursor']),
  PageInfo: new Set(['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor']),
  TypeInfo: new Set(['name', 'kind', 'fields', 'enumValues', 'description']),
  Payload: new Set([]), // payloads accept any field present in their record
  TypeField: new Set(['name', 'description', 'args', 'type']),
  EnumValue: new Set(['name', 'description']),
};

function cursorEncode(n: number): string {
  return Buffer.from(`c:${n}`, 'utf8').toString('base64');
}

function cursorDecode(after: string | undefined): number | null {
  if (after === undefined) return null;
  try {
    const text = Buffer.from(after, 'base64').toString('utf8');
    const m = /^c:(\d+)$/.exec(text);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

class GqlError extends Error {}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function prState(pr: Pull): string {
  if (pr.state === 'open') return 'OPEN';
  return pr.merged ? 'MERGED' : 'CLOSED';
}

function paginateConnection<T>(items: T[], first: number | undefined, after: string | undefined, keyOf: (x: T) => number): { page: T[]; endCursor: string | null; hasNext: boolean } {
  const afterKey = cursorDecode(after);
  let rest = items;
  if (afterKey !== null) {
    const idx = items.findIndex((x) => keyOf(x) === afterKey);
    rest = idx >= 0 ? items.slice(idx + 1) : [];
  }
  const limit = first ?? rest.length;
  const page = rest.slice(0, limit);
  const hasNext = rest.length > page.length;
  const endCursor = page.length ? cursorEncode(keyOf(page[page.length - 1]!)) : null;
  return { page, endCursor, hasNext };
}

// -------------------------------------------------------------------------------------------
// executor
// -------------------------------------------------------------------------------------------

interface ExecCtx {
  store: Store;
  errors: Array<{ message: string }>;
}

function fieldAllowed(obj: GObj, name: string): boolean {
  if (name === '__typename') return true;
  if (obj.t === 'Payload') return name in obj.fields;
  const set = TYPE_FIELDS[obj.t];
  return set !== undefined && set.has(name);
}

function resolveField(ctx: ExecCtx, obj: GObj, field: FieldNode, variables: Record<string, unknown>): unknown {
  const name = field.name;
  if (name === '__typename') {
    return obj.t === 'Viewer' ? 'User' : obj.t;
  }
  if (!fieldAllowed(obj, name)) {
    throw new GqlError(`Cannot query field "${name}" on type "${obj.t}".`);
  }
  const args: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(field.args)) args[k] = resolveVars(v, variables);
  const { store } = ctx;

  switch (obj.t) {
    case 'Query': {
      if (name === 'viewer') return { t: 'Viewer' } satisfies GObj;
      if (name === 'repository') {
        const repo = store.getRepo(asStr(args.owner) ?? '', asStr(args.name) ?? '');
        return repo ? ({ t: 'Repository', repo } satisfies GObj) : null;
      }
      if (name === 'node') {
        const id = asStr(args.id);
        if (id === undefined) return null;
        const decoded = store.decodeGlobalId(id);
        if (!decoded) return null;
        const repo = store.getRepo(decoded.owner, decoded.name);
        if (!repo) return null;
        if (decoded.kind === 'R') return { t: 'Repository', repo } satisfies GObj;
        if (decoded.kind === 'I') {
          const issue = repo.issues.get(Number(decoded.key));
          return issue ? ({ t: 'Issue', repo, issue } satisfies GObj) : null;
        }
        if (decoded.kind === 'PR') {
          const pr = repo.pulls.get(Number(decoded.key));
          return pr ? ({ t: 'PullRequest', repo, pr } satisfies GObj) : null;
        }
        return null;
      }
      if (name === '__type') {
        const typeName = asStr(args.name);
        if (typeName === undefined) return null;
        if (['Issue', 'PullRequest', 'Repository', 'SearchType', 'Query', 'Mutation', 'User'].includes(typeName)) {
          return { t: 'TypeInfo', name: typeName } satisfies GObj;
        }
        return null;
      }
      if (name === '__schema') {
        return { t: 'Payload', fields: { queryType: { t: 'TypeInfo', name: 'Query' }, mutationType: { t: 'TypeInfo', name: 'Mutation' } } } satisfies GObj;
      }
      if (name === 'search') return { t: 'Payload', fields: { nodes: [], edges: [], pageInfo: { t: 'PageInfo', hasNext: false, endCursor: null } } } satisfies GObj;
      return null;
    }
    case 'Mutation': {
      if (name === 'createIssue') return mutateCreateIssue(ctx, args);
      if (name === 'closeIssue') return mutateCloseIssue(ctx, args);
      if (name === 'createPullRequest') return mutateCreatePullRequest(ctx, args);
      if (name === 'mergePullRequest') return mutateMergePullRequest(ctx, args);
      if (name === 'addComment') return mutateAddComment(ctx, args);
      return null;
    }
    case 'Viewer': {
      if (name === 'login') return VIEWER.login;
      if (name === 'id') return String(VIEWER.id);
      if (name === 'name') return VIEWER.name;
      if (name === 'email') return VIEWER.email;
      if (name === 'avatarUrl') return '';
      if (name === 'url') return 'https://example.invalid/adaiversen';
      return null;
    }
    case 'Repository': {
      const repo = obj.repo;
      if (name === 'id') return store.globalId('R', repo.owner, repo.name, repo.id);
      if (name === 'databaseId') return repo.id;
      if (name === 'name') return repo.name;
      if (name === 'nameWithOwner') return repo.fullName;
      if (name === 'owner') return { t: 'Viewer' } satisfies GObj;
      if (name === 'description') return repo.description;
      if (name === 'isPrivate') return repo.isPrivate;
      if (name === 'createdAt') return store.stamp(repo.seq);
      if (name === 'url') return `https://example.invalid/${repo.fullName}`;
      if (name === 'viewerPermission') return 'ADMIN';
      if (name === 'hasIssuesEnabled') return true;
      if (name === 'defaultBranchRef') {
        const tip = repo.defaultTip();
        if (tip === undefined) return null;
        return { t: 'Ref', repo, name: repo.defaultBranch, oid: tip } satisfies GObj;
      }
      if (name === 'issue') {
        const issue = repo.issues.get(asNum(args.number) ?? -1);
        return issue ? ({ t: 'Issue', repo, issue } satisfies GObj) : null;
      }
      if (name === 'issues') {
        let list = [...repo.issues.values()];
        if (Array.isArray(args.states)) {
          const wanted = new Set(args.states as string[]);
          list = list.filter((i) => wanted.has(issueState(i)));
        }
        const orderBy = args.orderBy as Record<string, unknown> | undefined;
        if (orderBy !== undefined) {
          const dir = orderBy.direction === 'DESC' ? -1 : 1;
          list.sort((a, b) => (a.seq - b.seq) * dir);
        } else {
          list.sort((a, b) => a.seq - b.seq);
        }
        const { page, endCursor, hasNext } = paginateConnection(list, asNum(args.first), asStr(args.after), (i) => i.number);
        return { t: 'IssueConnection', repo, items: page, endCursor, hasNext } satisfies GObj;
      }
      if (name === 'pullRequest') {
        const pr = repo.pulls.get(asNum(args.number) ?? -1);
        return pr ? ({ t: 'PullRequest', repo, pr } satisfies GObj) : null;
      }
      if (name === 'pullRequests') {
        let list = [...repo.pulls.values()];
        if (Array.isArray(args.states)) {
          const wanted = new Set(args.states as string[]);
          list = list.filter((p) => wanted.has(prState(p)));
        }
        list.sort((a, b) => a.seq - b.seq);
        const { page, endCursor, hasNext } = paginateConnection(list, asNum(args.first), asStr(args.after), (p) => p.number);
        return { t: 'PullConnection', repo, items: page, endCursor, hasNext } satisfies GObj;
      }
      if (name === 'issuesCount') return repo.issues.size;
      if (name === 'pullRequestsCount') return repo.pulls.size;
      return null;
    }
    case 'Issue': {
      const { issue, repo } = obj;
      if (name === 'id') return store.globalId('I', repo.owner, repo.name, issue.number);
      if (name === 'number') return issue.number;
      if (name === 'title') return issue.title;
      if (name === 'state') return issueState(issue);
      if (name === 'body') return issue.body;
      if (name === 'createdAt') return issue.createdAt;
      if (name === 'updatedAt') return issue.updatedAt;
      if (name === 'url') return `https://example.invalid/${repo.fullName}/issues/${issue.number}`;
      if (name === 'author') return { t: 'Viewer' } satisfies GObj;
      if (name === 'repository') return { t: 'Repository', repo } satisfies GObj;
      if (name === 'labels') {
        return issue.labels.map((l) => ({ t: 'Label', name: l, color: repo.labels.get(l)?.color ?? 'ededed' }) satisfies GObj);
      }
      if (name === 'comments') {
        return { t: 'CommentConnection', repo, comments: issue.comments } satisfies GObj;
      }
      return null;
    }
    case 'PullRequest': {
      const { pr, repo } = obj;
      if (name === 'id') return store.globalId('PR', repo.owner, repo.name, pr.number);
      if (name === 'number') return pr.number;
      if (name === 'title') return pr.title;
      if (name === 'body') return pr.body;
      if (name === 'state') return prState(pr);
      if (name === 'merged') return pr.merged;
      if (name === 'mergedAt') return pr.mergedAt;
      if (name === 'mergeable') return prMergeable(repo, pr) ? 'MERGEABLE' : 'CONFLICTING';
      if (name === 'headRefName') return pr.headRef;
      if (name === 'baseRefName') return pr.baseRef;
      if (name === 'headRefOid') return pr.headSha;
      if (name === 'baseRefOid') return pr.baseSha;
      if (name === 'createdAt') return pr.createdAt;
      if (name === 'updatedAt') return pr.updatedAt;
      if (name === 'url') return `https://example.invalid/${repo.fullName}/pull/${pr.number}`;
      if (name === 'author') return { t: 'Viewer' } satisfies GObj;
      if (name === 'repository') return { t: 'Repository', repo } satisfies GObj;
      if (name === 'files') {
        return { t: 'FileConnection', repo, pr } satisfies GObj;
      }
      return null;
    }
    case 'Comment': {
      if (name === 'id') return store.globalId('C', obj.repo.owner, obj.repo.name, obj.id);
      if (name === 'body') return obj.body;
      if (name === 'author') return { t: 'Viewer' } satisfies GObj;
      if (name === 'createdAt') return store.stamp(obj.id);
      if (name === 'updatedAt') return store.stamp(obj.id);
      return null;
    }
    case 'Label': {
      if (name === 'id') return obj.name;
      if (name === 'name') return obj.name;
      if (name === 'color') return obj.color;
      if (name === 'description') return '';
      return null;
    }
    case 'Ref': {
      if (name === 'name') return obj.name;
      if (name === 'prefix') return 'refs/heads/';
      if (name === 'target') return { t: 'Commit', repo: obj.repo, oid: obj.oid } satisfies GObj;
      return null;
    }
    case 'Commit': {
      if (name === 'oid') return obj.oid;
      if (name === 'abbreviatedOid') return obj.oid.slice(0, 7);
      if (name === 'message') {
        const c = obj.repo.commitOf(obj.oid);
        return c ? c.message : '';
      }
      if (name === 'author') return { t: 'Viewer' } satisfies GObj;
      return null;
    }
    case 'IssueConnection': {
      if (name === 'nodes') return obj.items.map((i) => ({ t: 'Issue', repo: obj.repo, issue: i }) satisfies GObj);
      if (name === 'edges') return obj.items.map((i) => ({ t: 'Edge', node: { t: 'Issue', repo: obj.repo, issue: i } }) satisfies GObj);
      if (name === 'pageInfo') return { t: 'PageInfo', hasNext: obj.hasNext, endCursor: obj.endCursor } satisfies GObj;
      if (name === 'totalCount') return obj.items.length;
      return null;
    }
    case 'PullConnection': {
      if (name === 'nodes') return obj.items.map((p) => ({ t: 'PullRequest', repo: obj.repo, pr: p }) satisfies GObj);
      if (name === 'edges') return obj.items.map((p) => ({ t: 'Edge', node: { t: 'PullRequest', repo: obj.repo, pr: p } }) satisfies GObj);
      if (name === 'pageInfo') return { t: 'PageInfo', hasNext: obj.hasNext, endCursor: obj.endCursor } satisfies GObj;
      if (name === 'totalCount') return obj.items.length;
      return null;
    }
    case 'FileConnection': {
      const files = prFiles(obj.repo, obj.pr);
      if (name === 'nodes') {
        const first = asNum(args.first);
        const slice = first !== undefined ? files.slice(0, first) : files;
        return slice.map((f) => ({ t: 'Payload', fields: { path: f.filename, additions: f.additions, deletions: f.deletions, changeType: String(f.status).toUpperCase() } }) satisfies GObj);
      }
      if (name === 'edges') {
        return files.map((f) => ({ t: 'Edge', node: { t: 'Payload', fields: { path: f.filename } } }) satisfies GObj);
      }
      if (name === 'pageInfo') return { t: 'PageInfo', hasNext: false, endCursor: null } satisfies GObj;
      if (name === 'totalCount') return files.length;
      return null;
    }
    case 'CommentConnection': {
      if (name === 'nodes') return obj.comments.map((c) => ({ t: 'Comment', id: c.id, body: c.body, repo: obj.repo }) satisfies GObj);
      if (name === 'edges') return obj.comments.map((c) => ({ t: 'Edge', node: { t: 'Comment', id: c.id, body: c.body, repo: obj.repo } }) satisfies GObj);
      if (name === 'pageInfo') return { t: 'PageInfo', hasNext: false, endCursor: null } satisfies GObj;
      if (name === 'totalCount') return obj.comments.length;
      return null;
    }
    case 'Edge': {
      if (name === 'node') return obj.node;
      if (name === 'cursor') return '';
      return null;
    }
    case 'PageInfo': {
      if (name === 'hasNextPage') return obj.hasNext;
      if (name === 'hasPreviousPage') return false;
      if (name === 'startCursor') return null;
      if (name === 'endCursor') return obj.endCursor;
      return null;
    }
    case 'TypeInfo': {
      if (name === 'name') return obj.name;
      if (name === 'description') return null;
      if (name === 'kind') return obj.name === 'SearchType' ? 'ENUM' : 'OBJECT';
      if (name === 'fields') {
        if (obj.name === 'SearchType') return null;
        const set = TYPE_FIELDS[obj.name] ?? new Set<string>();
        return [...set].map((f) => ({ t: 'Payload', fields: { name: f, description: null, args: [], type: { t: 'Payload', fields: { name: 'String' } } } }) satisfies GObj);
      }
      if (name === 'enumValues') {
        if (obj.name !== 'SearchType') return null;
        return ['ISSUE', 'USER', 'REPOSITORY', 'DISCUSSION', 'COMMIT'].map((v) => ({ t: 'Payload', fields: { name: v, description: null } }) satisfies GObj);
      }
      return null;
    }
    case 'Payload': {
      const v = obj.fields[name];
      return v === undefined ? null : v;
    }
  }
}

function executeSelections(
  ctx: ExecCtx,
  obj: GObj,
  selections: FieldNode[],
  variables: Record<string, unknown>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const field of selections) {
    if (field.isFragment) {
      const typeName = field.typeCondition;
      const actual = obj.t === 'Viewer' ? 'User' : obj.t;
      if (typeName !== undefined && typeName !== actual) continue;
      Object.assign(out, executeSelections(ctx, obj, field.selections ?? [], variables));
      continue;
    }
    const key = field.alias ?? field.name;
    try {
      const value = resolveField(ctx, obj, field, variables);
      out[key] = materialize(ctx, value, field, variables);
    } catch (e) {
      if (e instanceof GqlError) {
        ctx.errors.push({ message: e.message });
        out[key] = null;
      } else {
        throw e;
      }
    }
  }
  return out;
}

function materialize(ctx: ExecCtx, value: unknown, field: FieldNode, variables: Record<string, unknown>): unknown {
  if (value === null || value === undefined) return null;
  if (Array.isArray(value)) {
    return value.map((v) => materialize(ctx, v, field, variables));
  }
  if (typeof value === 'object' && 't' in (value as Record<string, unknown>)) {
    const obj = value as GObj;
    if (field.selections === null) return null;
    return executeSelections(ctx, obj, field.selections, variables);
  }
  return value;
}

// -------------------------------------------------------------------------------------------
// mutations
// -------------------------------------------------------------------------------------------

function findRepoByGlobalId(store: Store, id: string | undefined): Repo | null {
  if (id === undefined) return null;
  const decoded = store.decodeGlobalId(id);
  if (!decoded) return null;
  return store.getRepo(decoded.owner, decoded.name) ?? null;
}

function mutateCreateIssue(ctx: ExecCtx, args: Record<string, unknown>): GObj {
  const input = (args.input ?? {}) as Record<string, unknown>;
  const repo = findRepoByGlobalId(ctx.store, asStr(input.repositoryId));
  if (!repo) throw new GqlError('Repository not found');
  const title = asStr(input.title);
  if (title === undefined || title === '') throw new GqlError('title is required');
  const number = repo.nextNumber++;
  const seq = ctx.store.nextSequence();
  const issue: Issue = {
    number,
    dbId: ctx.store.allocDbId(),
    title,
    body: asStr(input.body) ?? '',
    state: 'open',
    labels: [],
    comments: [],
    seq,
    createdAt: ctx.store.stamp(seq),
    updatedAt: ctx.store.stamp(seq),
  };
  repo.issues.set(number, issue);
  return { t: 'Payload', fields: { issue: { t: 'Issue', repo, issue } satisfies GObj } };
}

function mutateCloseIssue(ctx: ExecCtx, args: Record<string, unknown>): GObj {
  const input = (args.input ?? {}) as Record<string, unknown>;
  const id = asStr(input.issueId);
  const decoded = id !== undefined ? ctx.store.decodeGlobalId(id) : null;
  if (!decoded) throw new GqlError('Issue not found');
  const repo = ctx.store.getRepo(decoded.owner, decoded.name);
  const issue = repo?.issues.get(Number(decoded.key));
  if (!issue || !repo) throw new GqlError('Issue not found');
  issue.state = 'closed';
  issue.updatedAt = ctx.store.stamp(ctx.store.nextSequence());
  return { t: 'Payload', fields: { issue: { t: 'Issue', repo, issue } satisfies GObj } };
}

function mutateCreatePullRequest(ctx: ExecCtx, args: Record<string, unknown>): GObj {
  const input = (args.input ?? {}) as Record<string, unknown>;
  const repo = findRepoByGlobalId(ctx.store, asStr(input.repositoryId));
  if (!repo) throw new GqlError('Repository not found');
  const head = asStr(input.headRefName);
  const base = asStr(input.baseRefName);
  const title = asStr(input.title);
  if (head === undefined || base === undefined || title === undefined) throw new GqlError('headRefName, baseRefName and title are required');
  const headSha = repo.tip(`refs/heads/${head}`);
  const baseSha = repo.tip(`refs/heads/${base}`);
  if (headSha === undefined || baseSha === undefined) throw new GqlError('head or base branch not found');
  const existing = [...repo.pulls.values()].find((p) => p.state === 'open' && p.headRef === head && p.baseRef === base);
  if (existing) {
    return { t: 'Payload', fields: { pullRequest: { t: 'PullRequest', repo, pr: existing } satisfies GObj } };
  }
  const number = repo.nextNumber++;
  const seq = ctx.store.nextSequence();
  const pr: Pull = {
    number,
    dbId: ctx.store.allocDbId(),
    title,
    body: asStr(input.body) ?? '',
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    headRef: head,
    baseRef: base,
    headSha,
    baseSha,
    seq,
    createdAt: ctx.store.stamp(seq),
    updatedAt: ctx.store.stamp(seq),
  };
  repo.pulls.set(number, pr);
  return { t: 'Payload', fields: { pullRequest: { t: 'PullRequest', repo, pr } satisfies GObj } };
}

function mutateMergePullRequest(ctx: ExecCtx, args: Record<string, unknown>): GObj {
  const input = (args.input ?? {}) as Record<string, unknown>;
  const id = asStr(input.pullRequestId);
  const decoded = id !== undefined ? ctx.store.decodeGlobalId(id) : null;
  if (!decoded) throw new GqlError('Pull request not found');
  const repo = ctx.store.getRepo(decoded.owner, decoded.name);
  const pr = repo?.pulls.get(Number(decoded.key));
  if (!pr || !repo) throw new GqlError('Pull request not found');
  if (pr.state !== 'open') throw new GqlError('Pull request is already closed');
  if (!prMergeable(repo, pr)) throw new GqlError('Pull request cannot be merged due to conflicts');
  const baseTip = repo.tip(`refs/heads/${pr.baseRef}`);
  const headCommit = repo.commitOf(pr.headSha);
  if (baseTip === undefined || headCommit === undefined) throw new GqlError('base or head commit not found');
  const seq = ctx.store.nextSequence();
  const mergeCommit = buildCommit({
    tree: headCommit.tree,
    parents: [baseTip],
    author: `${VIEWER.name} <${VIEWER.email}>`,
    committer: `${VIEWER.name} <${VIEWER.email}>`,
    timestamp: 1700000000 + seq,
    message: `${pr.title} (#${pr.number})\n`,
  });
  const mergeSha = repo.putObject('commit', mergeCommit);
  repo.setRef(`refs/heads/${pr.baseRef}`, mergeSha);
  pr.state = 'closed';
  pr.merged = true;
  pr.mergedAt = ctx.store.stamp(seq);
  pr.mergeCommitSha = mergeSha;
  pr.updatedAt = ctx.store.stamp(seq);
  return { t: 'Payload', fields: { pullRequest: { t: 'PullRequest', repo, pr } satisfies GObj } };
}

function mutateAddComment(ctx: ExecCtx, args: Record<string, unknown>): GObj {
  const input = (args.input ?? {}) as Record<string, unknown>;
  const id = asStr(input.subjectId);
  const body = asStr(input.body);
  const decoded = id !== undefined ? ctx.store.decodeGlobalId(id) : null;
  if (!decoded || body === undefined) throw new GqlError('subjectId and body are required');
  const repo = ctx.store.getRepo(decoded.owner, decoded.name);
  if (!repo) throw new GqlError('subject not found');
  if (decoded.kind === 'I') {
    const issue = repo.issues.get(Number(decoded.key));
    if (!issue) throw new GqlError('subject not found');
    const seq = ctx.store.nextSequence();
    const comment = { id: repo.nextComment(), body, seq, createdAt: ctx.store.stamp(seq) };
    issue.comments.push(comment);
    return {
      t: 'Payload',
      fields: { commentEdge: { t: 'Payload', fields: { node: { t: 'Comment', id: comment.id, body, repo } satisfies GObj } } satisfies GObj },
    };
  }
  // Pull-request subjects: return a synthetic comment edge.
  const syntheticId = ctx.store.allocDbId();
  return {
    t: 'Payload',
    fields: { commentEdge: { t: 'Payload', fields: { node: { t: 'Comment', id: syntheticId, body, repo } satisfies GObj } } satisfies GObj },
  };
}

// -------------------------------------------------------------------------------------------
// entry point
// -------------------------------------------------------------------------------------------

export interface GraphQLBody {
  query?: string;
  variables?: Record<string, unknown>;
  operationName?: string;
}

export function executeGraphQL(store: Store, body: GraphQLBody): Record<string, unknown> {
  const query = body.query ?? '';
  const variables = body.variables ?? {};
  let parsed: { operation: 'query' | 'mutation'; selections: FieldNode[] };
  try {
    parsed = new Parser(tokenize(query)).parseDocument();
  } catch (e) {
    return { errors: [{ message: e instanceof Error ? e.message : 'parse error' }] };
  }
  const ctx: ExecCtx = { store, errors: [] };
  const root: GObj = parsed.operation === 'mutation' ? { t: 'Mutation' } : { t: 'Query' };
  let data: Record<string, unknown>;
  try {
    data = executeSelections(ctx, root, parsed.selections, variables);
  } catch (e) {
    return { errors: [{ message: e instanceof Error ? e.message : 'execution error' }] };
  }
  const out: Record<string, unknown> = { data };
  if (ctx.errors.length) out.errors = ctx.errors;
  return out;
}
