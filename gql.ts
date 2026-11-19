import type { IncomingMessage, ServerResponse } from 'node:http';
import {
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import { computePullFiles, pullConflicts, refTip, squashMerge, treeFiles } from './store.ts';

// -------------------------------------------------------------------------------------------
// shared views
// -------------------------------------------------------------------------------------------

function repoView(store: Store, repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: store.repoGlobalId(repo),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: store.clock.iso(repo.seq),
    updated_at: store.clock.iso(repo.seq),
  };
}

function issueView(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: store.issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name })),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID },
    created_at: store.clock.iso(issue.seq),
    updated_at: store.clock.iso(issue.seq),
  };
}

function pullView(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    id: pull.id,
    node_id: store.pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
    head: { ref: pull.head, sha: repo.refs.get(`refs/heads/${pull.head}`) ?? pull.headSha },
    base: { ref: pull.base, sha: repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha },
  };
}

function resolveRepo(store: Store, args: Record<string, unknown>): Repo | undefined {
  const full = pickString(args, 'full_name', 'repo_full_name', 'repository_full_name');
  if (full !== '') {
    const slash = full.indexOf('/');
    if (slash > 0) return store.getRepo(full.slice(0, slash), full.slice(slash + 1));
  }
  const owner = pickString(args, 'owner', 'owner_login', 'org', 'user', 'login') || VIEWER_LOGIN;
  const name = pickString(args, 'repo', 'repository', 'repository_name', 'repo_name', 'name');
  if (name === '') return undefined;
  return store.getRepo(owner, name);
}

function pickString(args: Record<string, unknown>, ...names: string[]): string {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'string' && value !== '') return value;
  }
  return '';
}

function pickNumber(args: Record<string, unknown>, ...names: string[]): number | null {
  for (const name of names) {
    const value = args[name];
    if (typeof value === 'number') return value;
  }
  return null;
}

// -------------------------------------------------------------------------------------------
// GraphQL: a small parser for the documents the suite sends
// -------------------------------------------------------------------------------------------

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: Array<{ name: string; value: ValueNode }> };

interface FieldNode {
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: ValueNode }>;
  children: SelectionNode[];
}

type SelectionNode =
  | { kind: 'field'; field: FieldNode }
  | { kind: 'inlineFragment'; type: string | null; children: SelectionNode[] };

interface OperationNode {
  kind: 'query' | 'mutation';
  name: string | null;
  variables: Record<string, ValueNode>;
  selections: SelectionNode[];
}

function tokenize(text: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < text.length) {
    const ch = text[i];
    if (ch === ' ' || ch === '\n' || ch === '\r' || ch === '\t' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < text.length && text[i] !== '\n') i += 1;
      continue;
    }
    if ('{}():!=$@[]'.includes(ch)) {
      tokens.push(ch);
      i += 1;
      continue;
    }
    if (ch === '.' && text.slice(i, i + 3) === '...') {
      tokens.push('...');
      i += 3;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < text.length && text[j] !== '"') {
        if (text[j] === '\\' && j + 1 < text.length) {
          const esc = text[j + 1];
          out += esc === 'n' ? '\n' : esc === 't' ? '\t' : esc === 'r' ? '\r' : esc;
          j += 2;
        } else {
          out += text[j];
          j += 1;
        }
      }
      tokens.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    let j = i;
    while (j < text.length && /[A-Za-z0-9_\-.]/.test(text[j])) j += 1;
    if (j === i) {
      i += 1;
      continue;
    }
    tokens.push(text.slice(i, j));
    i = j;
  }
  return tokens;
}

class Parser {
  private pos = 0;
  private readonly tokens: string[];
  constructor(tokens: string[]) {
    this.tokens = tokens;
  }

  peek(): string {
    return this.tokens[this.pos] ?? '';
  }

  next(): string {
    return this.tokens[this.pos++] ?? '';
  }

  expect(token: string): void {
    if (this.next() !== token) throw new Error(`expected ${token}`);
  }

  parseDocument(): OperationNode[] {
    const operations: OperationNode[] = [];
    while (this.pos < this.tokens.length) {
      const token = this.peek();
      if (token === 'query' || token === 'mutation') {
        this.next();
        let name: string | null = null;
        if (this.peek() !== '(' && this.peek() !== '{') name = this.next();
        const variables: Record<string, ValueNode> = {};
        if (this.peek() === '(') {
          this.next();
          while (this.peek() !== ')' && this.pos < this.tokens.length) {
            if (this.peek() === '$') {
              this.next();
              const varName = this.next();
              this.expect(':');
              // Skip the type annotation (possibly [String!]! etc).
              let depth = 0;
              while (this.pos < this.tokens.length) {
                const t = this.next();
                if (t === '[') depth += 1;
                else if (t === ']') depth -= 1;
                else if (t === ',' && depth === 0) break;
                else if (t === ')' && depth === 0) {
                  this.pos -= 1;
                  break;
                }
              }
              variables[varName] = { kind: 'null' };
            } else this.next();
          }
          this.expect(')');
        }
        const selections = this.parseSelections();
        operations.push({ kind: token as 'query' | 'mutation', name, variables, selections });
      } else if (token === '{') {
        operations.push({ kind: 'query', name: null, variables: {}, selections: this.parseSelections() });
      } else {
        this.next();
      }
    }
    return operations;
  }

  parseSelections(): SelectionNode[] {
    this.expect('{');
    const out: SelectionNode[] = [];
    while (this.peek() !== '}' && this.pos < this.tokens.length) {
      out.push(this.parseSelection());
    }
    this.expect('}');
    return out;
  }

  parseSelection(): SelectionNode {
    if (this.peek() === '...') {
      this.next();
      let type: string | null = null;
      if (this.peek() === 'on') {
        this.next();
        type = this.next();
      }
      return { kind: 'inlineFragment', type, children: this.parseSelections() };
    }
    let alias: string | null = null;
    let name = this.next();
    if (this.peek() === ':') {
      this.next();
      alias = name;
      name = this.next();
    }
    const args: Array<{ name: string; value: ValueNode }> = [];
    if (this.peek() === '(') {
      this.next();
      while (this.peek() !== ')' && this.pos < this.tokens.length) {
        const argName = this.next();
        this.expect(':');
        args.push({ name: argName, value: this.parseValue() });
      }
      this.expect(')');
    }
    // Directives (@...) are ignored.
    while (this.peek() === '@') {
      this.next();
      this.next();
      if (this.peek() === '(') {
        this.next();
        let depth = 1;
        while (depth > 0 && this.pos < this.tokens.length) {
          const t = this.next();
          if (t === '(') depth += 1;
          else if (t === ')') depth -= 1;
        }
      }
    }
    const children = this.peek() === '{' ? this.parseSelections() : [];
    return { kind: 'field', field: { alias, name, args, children } };
  }

  parseValue(): ValueNode {
    const token = this.next();
    if (token === '$') return { kind: 'variable', name: this.next() };
    if (token === 'true') return { kind: 'boolean', value: true };
    if (token === 'false') return { kind: 'boolean', value: false };
    if (token === 'null') return { kind: 'null' };
    if (token.startsWith('"')) return { kind: 'string', value: JSON.parse(token) as string };
    if (/^-?\d+$/.test(token)) return { kind: 'int', value: Number(token) };
    if (/^-?\d+\.\d+$/.test(token)) return { kind: 'float', value: Number(token) };
    if (token === '[') {
      const values: ValueNode[] = [];
      while (this.peek() !== ']' && this.pos < this.tokens.length) values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (token === '{') {
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (this.peek() !== '}' && this.pos < this.tokens.length) {
        const name = this.next();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    return { kind: 'enum', value: token };
  }
}

function resolveValue(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable': return variables[node.name];
    case 'string': case 'int': case 'float': case 'boolean': return node.value;
    case 'null': return null;
    case 'enum': return node.value;
    case 'list': return node.values.map((child) => resolveValue(child, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = resolveValue(field.value, variables);
      return out;
    }
  }
}

function argMap(field: FieldNode, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of field.args) out[arg.name] = resolveValue(arg.value, variables);
  return out;
}

// The schema the suite introspects: field names per type.
const SCHEMA: Record<string, string[]> = {
  Query: ['viewer', 'repository', 'node', '__type', '__schema'],
  User: ['login', 'id', 'name', 'email'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  Ref: ['name', 'target'],
  Issue: ['id', 'number', 'title', 'state', 'body', 'author', 'labels', 'comments'],
  PullRequest: ['id', 'number', 'title', 'state', 'body', 'headRefName', 'baseRefName', 'mergeable', 'merged', 'files'],
  IssueConnection: ['nodes', 'edges', 'totalCount', 'pageInfo'],
  PullRequestConnection: ['nodes', 'edges', 'totalCount', 'pageInfo'],
  ChangedFilesConnection: ['nodes', 'edges', 'totalCount', 'pageInfo'],
  ChangedFile: ['path', 'additions', 'deletions', 'changes'],
  PageInfo: ['hasNextPage', 'hasPreviousPage', 'startCursor', 'endCursor'],
  Type: ['name', 'kind', 'fields', 'enumValues', 'description'],
  Field: ['name', 'description'],
  EnumValue: ['name', 'description'],
  Mutation: ['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment'],
  CreateIssuePayload: ['issue', 'clientMutationId'],
  CloseIssuePayload: ['issue', 'clientMutationId'],
  CreatePullRequestPayload: ['pullRequest', 'clientMutationId'],
  MergePullRequestPayload: ['pullRequest', 'clientMutationId'],
  AddCommentPayload: ['commentEdge', 'clientMutationId'],
  IssueCommentEdge: ['node', 'cursor'],
  IssueComment: ['id', 'body'],
  Node: ['id'],
};

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'author', 'labels', 'comments', 'createdAt', 'updatedAt'],
  PullRequest: ['id', 'number', 'title', 'state', 'body', 'headRefName', 'baseRefName', 'mergeable', 'merged', 'files'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest'],
  SearchType: [],
  User: ['login', 'id', 'name', 'email'],
};

const TYPE_KIND: Record<string, string> = {
  Issue: 'OBJECT',
  PullRequest: 'OBJECT',
  Repository: 'OBJECT',
  User: 'OBJECT',
  SearchType: 'ENUM',
};

function encodeCursor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64url');
}

function decodeCursor(cursor: unknown): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const match = /^cursor:(\d+)$/.exec(text);
    return match === null ? null : Number(match[1]);
  } catch {
    return null;
  }
}

class GraphQLError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'GraphQLError';
  }
}

interface ExecCtx {
  store: Store;
  variables: Record<string, unknown>;
  errors: Array<{ message: string }>;
}

function validateSelections(selections: SelectionNode[], typeName: string, exec: ExecCtx): void {
  const allowed = SCHEMA[typeName] ?? [];
  for (const selection of selections) {
    if (selection.kind === 'inlineFragment') {
      validateSelections(selection.children, selection.type ?? typeName, exec);
      continue;
    }
    const field = selection.field;
    if (field.name === '__typename') continue;
    if (!allowed.includes(field.name)) {
      exec.errors.push({ message: `Cannot query field "${field.name}" on type "${typeName}".` });
    }
    validateSelections(field.children, childType(typeName, field.name), exec);
  }
}

function childType(parent: string, field: string): string {
  const map: Record<string, Record<string, string>> = {
    Query: { viewer: 'User', repository: 'Repository', node: 'Node', __type: 'Type', __schema: 'Type' },
    Repository: { owner: 'User', defaultBranchRef: 'Ref', issue: 'Issue', issues: 'IssueConnection', pullRequests: 'PullRequestConnection', pullRequest: 'PullRequest' },
    Issue: { author: 'User' },
    PullRequest: { files: 'ChangedFilesConnection' },
    IssueConnection: { nodes: 'Issue', pageInfo: 'PageInfo' },
    PullRequestConnection: { nodes: 'PullRequest', pageInfo: 'PageInfo' },
    ChangedFilesConnection: { nodes: 'ChangedFile', pageInfo: 'PageInfo' },
    Type: { fields: 'Field', enumValues: 'EnumValue' },
    Mutation: { createIssue: 'CreateIssuePayload', closeIssue: 'CloseIssuePayload', createPullRequest: 'CreatePullRequestPayload', mergePullRequest: 'MergePullRequestPayload', addComment: 'AddCommentPayload' },
    CreateIssuePayload: { issue: 'Issue' },
    CloseIssuePayload: { issue: 'Issue' },
    CreatePullRequestPayload: { pullRequest: 'PullRequest' },
    MergePullRequestPayload: { pullRequest: 'PullRequest' },
    AddCommentPayload: { commentEdge: 'IssueCommentEdge' },
    IssueCommentEdge: { node: 'IssueComment' },
  };
  return map[parent]?.[field] ?? 'User';
}

function executeSelections(selections: SelectionNode[], typeName: string, value: unknown, exec: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'inlineFragment') {
      if (selection.type !== null && selection.type !== typeName && !(typeName === 'Node' && value !== null && typeof value === 'object' && (value as Record<string, unknown>).__kind === selection.type)) {
        continue;
      }
      Object.assign(out, executeSelections(selection.children, selection.type ?? typeName, value, exec));
      continue;
    }
    const field = selection.field;
    const key = field.alias ?? field.name;
    out[key] = executeField(field, typeName, value, exec);
  }
  return out;
}

function executeField(field: FieldNode, typeName: string, value: unknown, exec: ExecCtx): unknown {
  const args = argMap(field, exec.variables);
  const record = (value !== null && typeof value === 'object' ? (value as Record<string, unknown>) : {}) as Record<string, unknown>;
  if (field.name === '__typename') return typeName;
  if (typeName === 'Query') {
    if (field.name === 'viewer') {
      return executeSelections(field.children, 'User', { login: VIEWER_LOGIN, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL }, exec);
    }
    if (field.name === 'repository') {
      const repo = exec.store.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
      if (repo === undefined) return null;
      return executeSelections(field.children, 'Repository', repositoryValue(exec.store, repo), exec);
    }
    if (field.name === 'node') {
      const found = exec.store.findByGlobalId(String(args.id ?? ''));
      if (found === null) return null;
      if (found.kind === 'repo') return executeSelections(field.children, 'Repository', repositoryValue(exec.store, found.repo), exec);
      if (found.kind === 'issue' && found.issue !== undefined) {
        return executeSelections(field.children, 'Issue', issueValue(exec.store, found.repo, found.issue), exec);
      }
      if (found.kind === 'pull' && found.pull !== undefined) {
        return executeSelections(field.children, 'PullRequest', pullValue(exec.store, found.repo, found.pull), exec);
      }
      return null;
    }
    if (field.name === '__type') {
      const name = String(args.name ?? '');
      const kind = TYPE_KIND[name] ?? 'OBJECT';
      const fields = TYPE_FIELDS[name] ?? [];
      const value: Record<string, unknown> = { __kind: 'Type', name, kind, fields, enumValues: [] };
      if (name === 'SearchType') value.enumValues = ['ISSUE', 'PULL_REQUEST', 'REPOSITORY', 'USER'];
      return executeSelections(field.children, 'Type', value, exec);
    }
    return null;
  }
  if (typeName === 'Type') {
    if (field.name === 'fields') {
      const names = (record.fields as string[] | undefined) ?? [];
      return names.map((name) => executeSelections(field.children, 'Field', { name }, exec));
    }
    if (field.name === 'enumValues') {
      const names = (record.enumValues as string[] | undefined) ?? [];
      return names.map((name) => executeSelections(field.children, 'EnumValue', { name }, exec));
    }
    return record[field.name] ?? null;
  }
  if (typeName === 'Repository') {
    const repo = record.__repo as Repo | undefined;
    if (repo === undefined) return record[field.name] ?? null;
    if (field.children.length === 0) return record[field.name] ?? null;
    if (field.name === 'issue') {
      const issue = repo.issues.find((entry) => entry.number === Number(args.number));
      if (issue === undefined) return null;
      return executeSelections(field.children, 'Issue', issueValue(exec.store, repo, issue), exec);
    }
    if (field.name === 'issues') return issuesConnection(exec, repo, field, args);
    if (field.name === 'pullRequests') {
      const first = typeof args.first === 'number' ? args.first : 100;
      const nodes = [...repo.pulls].filter((pull) => pull.state === 'open').sort((a, b) => b.seq - a.seq).slice(0, first);
      return connectionValue(field.children, nodes.map((pull) => pullValue(exec.store, repo, pull)), 'PullRequest', exec);
    }
    if (field.name === 'pullRequest') {
      const pull = repo.pulls.find((entry) => entry.number === Number(args.number));
      if (pull === undefined) return null;
      return executeSelections(field.children, 'PullRequest', pullValue(exec.store, repo, pull), exec);
    }
    return executeSelections(field.children, childType(typeName, field.name), record[field.name], exec);
  }
  if (typeName === 'PullRequest') {
    if (field.name === 'files') {
      const pull = record.__pull as Pull | undefined;
      const nodes = (pull?.files ?? []).map((file) => ({ path: file.filename, additions: file.additions, deletions: file.deletions, changes: file.changes }));
      const first = typeof args.first === 'number' ? args.first : 100;
      return connectionValue(field.children, nodes.slice(0, first), 'ChangedFile', exec);
    }
    if (field.name === 'mergeable') return record.mergeable ?? 'UNKNOWN';
    if (field.name === 'state') return record.state ?? 'OPEN';
    return record[field.name] ?? null;
  }
  if (typeName === 'IssueConnection' || typeName === 'PullRequestConnection' || typeName === 'ChangedFilesConnection') {
    return record[field.name] ?? null;
  }
  if (field.children.length > 0) {
    const child = record[field.name];
    if (child !== null && typeof child === 'object') {
      return executeSelections(field.children, childType(typeName, field.name), child, exec);
    }
    return child ?? null;
  }
  return record[field.name] ?? null;
}

function connectionValue(children: SelectionNode[], nodes: unknown[], nodeType: string, exec: ExecCtx): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of children) {
    if (selection.kind === 'inlineFragment') continue;
    const field = selection.field;
    if (field.name === 'nodes') {
      out[field.alias ?? 'nodes'] = nodes.map((node) => executeSelections(field.children, nodeType, node, exec));
    } else if (field.name === 'edges') {
      out[field.alias ?? 'edges'] = nodes.map((node) => ({ node: executeSelections(field.children, nodeType, node, exec), cursor: '' }));
    } else if (field.name === 'totalCount') {
      out[field.alias ?? 'totalCount'] = nodes.length;
    } else if (field.name === 'pageInfo') {
      out[field.alias ?? 'pageInfo'] = executeSelections(field.children, 'PageInfo', nodes, exec);
    }
  }
  return out;
}

function repositoryValue(store: Store, repo: Repo): Record<string, unknown> {
  const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
  return {
    __kind: 'Repository',
    __repo: repo,
    id: store.repoGlobalId(repo),
    databaseId: repo.id,
    name: repo.name,
    nameWithOwner: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, name: VIEWER_NAME, email: VIEWER_EMAIL },
    defaultBranchRef: tip === undefined ? { name: repo.defaultBranch } : { name: repo.defaultBranch },
    viewerPermission: 'ADMIN',
    hasIssuesEnabled: true,
  };
}

function issueValue(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    __kind: 'Issue',
    id: store.issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    state: issue.state === 'open' ? 'OPEN' : 'CLOSED',
    body: issue.body,
    author: { login: VIEWER_LOGIN, id: VIEWER_ID },
  };
}

function pullValue(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  return {
    __kind: 'PullRequest',
    __pull: pull,
    id: store.pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    state: pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED',
    body: pull.body,
    headRefName: pull.head,
    baseRefName: pull.base,
    mergeable: pullConflicts(store, repo, pull) ? 'CONFLICTING' : 'MERGEABLE',
    merged: pull.merged,
  };
}

function issuesConnection(exec: ExecCtx, repo: Repo, field: FieldNode, args: Record<string, unknown>): Record<string, unknown> {
  const first = typeof args.first === 'number' ? args.first : 10;
  const after = decodeCursor(args.after);
  let list = [...repo.issues];
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  if (states !== null) {
    list = list.filter((issue) => states.includes(issue.state === 'open' ? 'OPEN' : 'CLOSED'));
  }
  const orderBy = (args.orderBy ?? {}) as Record<string, unknown>;
  const desc = String(orderBy.direction ?? '') === 'DESC' || String(orderBy.direction ?? '') === '';
  const asc = String(orderBy.direction ?? '') === 'ASC';
  list.sort((a, b) => {
    if (asc) return a.seq - b.seq || a.number - b.number;
    return b.seq - a.seq || b.number - a.number;
  });
  void desc;
  if (after !== null) {
    list = asc ? list.filter((issue) => issue.number > after) : list.filter((issue) => issue.number < after);
  }
  const page = list.slice(0, first);
  const hasNext = list.length > first;
  const endCursor = page.length > 0 ? encodeCursor(page[page.length - 1].number) : null;
  const startCursor = page.length > 0 ? encodeCursor(page[0].number) : null;
  const out: Record<string, unknown> = {};
  for (const selection of field.children) {
    if (selection.kind === 'inlineFragment') continue;
    const child = selection.field;
    const key = child.alias ?? child.name;
    if (child.name === 'nodes') {
      out[key] = page.map((issue) => executeSelections(child.children, 'Issue', issueValue(exec.store, repo, issue), exec));
    } else if (child.name === 'totalCount') {
      out[key] = list.length;
    } else if (child.name === 'pageInfo') {
      out[key] = executeSelections(child.children, 'PageInfo', { hasNextPage: hasNext, hasPreviousPage: after !== null, startCursor, endCursor }, exec);
    }
  }
  return out;
}

function executeOperation(store: Store, op: OperationNode, variables: Record<string, unknown>): { data: unknown; errors: Array<{ message: string }> } {
  const exec: ExecCtx = { store, variables, errors: [] };
  const rootType = op.kind === 'mutation' ? 'Mutation' : 'Query';
  validateSelections(op.selections, rootType, exec);
  if (exec.errors.length > 0) return { data: null, errors: exec.errors };
  if (op.kind === 'mutation') {
    return { data: executeMutation(store, op, variables), errors: [] };
  }
  const data = executeSelections(op.selections, 'Query', {}, exec);
  return { data, errors: exec.errors };
}

function executeMutation(store: Store, op: OperationNode, variables: Record<string, unknown>): Record<string, unknown> {
  const exec: ExecCtx = { store, variables, errors: [] };
  const out: Record<string, unknown> = {};
  for (const selection of op.selections) {
    if (selection.kind === 'inlineFragment') continue;
    const field = selection.field;
    const key = field.alias ?? field.name;
    const args = argMap(field, variables);
    const input = (args.input ?? {}) as Record<string, unknown>;
    if (field.name === 'createIssue') {
      const found = store.findByGlobalId(String(input.repositoryId ?? ''));
      if (found === null || found.kind !== 'repo') throw new GraphQLError('repository not found');
      const repo = found.repo;
      const number = repo.nextNumber++;
      const seq = store.clock.next();
      const issue: Issue = {
        id: store.nextItem(),
        number,
        title: String(input.title ?? ''),
        body: String(input.body ?? ''),
        state: 'open',
        labels: [],
        seq,
        comments: [],
      };
      repo.issues.push(issue);
      out[key] = executeSelections(field.children, 'CreateIssuePayload', { issue: issueValue(store, repo, issue) }, exec);
    } else if (field.name === 'closeIssue') {
      const found = store.findByGlobalId(String(input.issueId ?? ''));
      if (found === null || found.kind !== 'issue' || found.issue === undefined) throw new GraphQLError('issue not found');
      found.issue.state = 'closed';
      out[key] = executeSelections(field.children, 'CloseIssuePayload', { issue: issueValue(store, found.repo, found.issue) }, exec);
    } else if (field.name === 'createPullRequest') {
      const found = store.findByGlobalId(String(input.repositoryId ?? ''));
      if (found === null || found.kind !== 'repo') throw new GraphQLError('repository not found');
      const repo = found.repo;
      const head = String(input.headRefName ?? '');
      const base = String(input.baseRefName ?? repo.defaultBranch);
      const headTip = refTip(repo, head);
      const baseTip = refTip(repo, base);
      if (headTip === undefined || baseTip === undefined) throw new GraphQLError('head or base branch does not exist');
      const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.head === head && pull.base === base);
      if (duplicate !== undefined) throw new GraphQLError('a pull request for this head already exists');
      const number = repo.nextNumber++;
      const seq = store.clock.next();
      const pull: Pull = {
        id: store.nextItem(),
        number,
        title: String(input.title ?? ''),
        body: String(input.body ?? ''),
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeSha: null,
        head,
        base,
        headSha: headTip,
        baseSha: baseTip,
        baseShaAtCreate: baseTip,
        files: [],
        seq,
      };
      computePullFiles(store, repo, pull);
      repo.pulls.push(pull);
      out[key] = executeSelections(field.children, 'CreatePullRequestPayload', { pullRequest: pullValue(store, repo, pull) }, exec);
    } else if (field.name === 'mergePullRequest') {
      const found = store.findByGlobalId(String(input.pullRequestId ?? ''));
      if (found === null || found.kind !== 'pull' || found.pull === undefined) throw new GraphQLError('pull request not found');
      const pull = found.pull;
      const result = squashMerge(store, found.repo, pull);
      if ('conflict' in result) throw new GraphQLError('merge conflict');
      pull.state = 'closed';
      pull.merged = true;
      pull.mergedAt = store.clock.iso(store.clock.next());
      pull.mergeSha = result.sha;
      out[key] = executeSelections(field.children, 'MergePullRequestPayload', { pullRequest: pullValue(store, found.repo, pull) }, exec);
    } else if (field.name === 'addComment') {
      const found = store.findByGlobalId(String(input.subjectId ?? ''));
      if (found === null || found.kind !== 'issue' || found.issue === undefined) throw new GraphQLError('issue not found');
      const comment = { id: store.nextItem(), body: String(input.body ?? ''), seq: store.clock.next() };
      found.issue.comments.push(comment);
      const node = { id: store.commentGlobalId(found.repo, found.issue, comment), body: comment.body };
      out[key] = executeSelections(field.children, 'AddCommentPayload', { commentEdge: { node } }, exec);
    }
  }
  return out;
}

export async function handleGraphQL(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  raw: Buffer,
  authed: boolean,
  nextReqId: () => string,
): Promise<void> {
  const send = (status: number, body: unknown): void => {
    const text = `${JSON.stringify(body)}\n`;
    res.writeHead(status, {
      'content-type': 'application/json; charset=utf-8',
      'content-length': Buffer.byteLength(text),
      'x-github-request-id': nextReqId(),
    });
    res.end(text);
  };
  if (!authed) {
    send(401, { message: 'Requires authentication' });
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw.toString('utf8'));
  } catch {
    send(400, { message: 'malformed JSON body' });
    return;
  }
  const operations = Array.isArray(parsed) ? parsed : [parsed];
  const results: unknown[] = [];
  for (const operation of operations) {
    const record = (operation !== null && typeof operation === 'object' ? (operation as Record<string, unknown>) : {}) as Record<string, unknown>;
    const queryText = typeof record.query === 'string' ? record.query : '';
    const variables = (record.variables !== null && typeof record.variables === 'object' ? (record.variables as Record<string, unknown>) : {}) as Record<string, unknown>;
    try {
      const parser = new Parser(tokenize(queryText));
      const ops = parser.parseDocument();
      if (ops.length === 0) {
        results.push({ data: null, errors: [{ message: 'no operation' }] });
        continue;
      }
      const op = ops[0];
      results.push(executeOperation(store, op, variables));
    } catch (err) {
      if (err instanceof GraphQLError) {
        results.push({ data: null, errors: [{ message: err.message }] });
      } else {
        results.push({ data: null, errors: [{ message: err instanceof Error ? err.message : 'parse error' }] });
      }
    }
  }
  send(200, Array.isArray(parsed) ? results : results[0]);
}

// -------------------------------------------------------------------------------------------
// MCP
// -------------------------------------------------------------------------------------------

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string }>;
    required: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        owner: { type: 'string', description: 'The owner login; defaults to the authenticated user.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The owner/repo full name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The owner/repo full name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The owner/repo full name.' },
        state: { type: 'string', description: 'open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'get_issue',
    description: 'Read one issue by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The owner/repo full name.' },
        number: { type: 'number', description: 'The issue number.' },
      },
      required: ['owner', 'repo', 'number'],
    },
  },
  {
    name: 'list_repos',
    description: 'List the repositories owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {},
      required: [],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The owner/repo full name.' },
        title: { type: 'string', description: 'The pull request title.' },
        body: { type: 'string', description: 'The pull request body.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        full_name: { type: 'string', description: 'The owner/repo full name.' },
        pull_number: { type: 'number', description: 'The pull request number.' },
        number: { type: 'number', description: 'Alias for pull_number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function normalizeToolName(name: string): string {
  return name.toLowerCase().replace(/[^a-z0-9]/g, '');
}

function findTool(name: string): ToolDef | undefined {
  const wanted = normalizeToolName(name);
  return TOOLS.find((tool) => normalizeToolName(tool.name) === wanted)
    ?? TOOLS.find((tool) => normalizeToolName(tool.name).endsWith(wanted) || wanted.endsWith(normalizeToolName(tool.name)));
}

function toolResult(content: unknown, isError = false): Record<string, unknown> {
  return {
    content: [{ type: 'text', text: typeof content === 'string' ? content : JSON.stringify(content) }],
    structuredContent: content,
    isError,
  };
}

function runTool(store: Store, name: string, args: Record<string, unknown>): { ok: true; result: unknown } | { ok: false; code: number; message: string } {
  const tool = findTool(name);
  if (tool === undefined) return { ok: false, code: -32601, message: `unknown tool ${name}` };
  for (const required of tool.inputSchema.required) {
    if (args[required] === undefined || args[required] === null || args[required] === '') {
      // A full_name may stand in for owner+repo.
      if ((required === 'owner' || required === 'repo') && pickString(args, 'full_name', 'repo_full_name') !== '') continue;
      return { ok: false, code: -32602, message: `missing required argument ${required}` };
    }
  }
  try {
    if (tool.name === 'create_repo') {
      const repoName = pickString(args, 'name', 'repo', 'repository');
      if (repoName === '') return { ok: false, code: -32602, message: 'missing required argument name' };
      const repo = store.createRepo(repoName, args.private === true, pickString(args, 'description', 'body'), 'main');
      if (repo === null) return { ok: true, result: toolResult({ error: 'repository already exists', name: repoName }, true) };
      return { ok: true, result: toolResult(repoView(store, repo)) };
    }
    if (tool.name === 'list_repos') {
      return { ok: true, result: toolResult({ repositories: store.listRepos().map((repo) => repoView(store, repo)) }) };
    }
    const repo = resolveRepo(store, args);
    if (repo === undefined) return { ok: true, result: toolResult({ error: 'repository not found' }, true) };
    if (tool.name === 'get_repo') return { ok: true, result: toolResult(repoView(store, repo)) };
    if (tool.name === 'create_issue') {
      const title = pickString(args, 'title', 'issue_title');
      const number = repo.nextNumber++;
      const seq = store.clock.next();
      const issue: Issue = {
        id: store.nextItem(),
        number,
        title,
        body: pickString(args, 'body', 'issue_body', 'description'),
        state: 'open',
        labels: [],
        seq,
        comments: [],
      };
      repo.issues.push(issue);
      return { ok: true, result: toolResult(issueView(store, repo, issue)) };
    }
    if (tool.name === 'list_issues') {
      const state = pickString(args, 'state', 'status') || 'open';
      let list = [...repo.issues];
      if (state !== 'all') list = list.filter((issue) => issue.state === state);
      list.sort((a, b) => b.seq - a.seq);
      return { ok: true, result: toolResult({ issues: list.map((issue) => issueView(store, repo, issue)) }) };
    }
    if (tool.name === 'get_issue') {
      const number = pickNumber(args, 'number', 'issue_number');
      const issue = repo.issues.find((entry) => entry.number === number);
      if (issue === undefined) return { ok: true, result: toolResult({ error: 'issue not found' }, true) };
      return { ok: true, result: toolResult(issueView(store, repo, issue)) };
    }
    if (tool.name === 'create_pull_request') {
      const head = pickString(args, 'head', 'head_ref', 'head_branch', 'source', 'source_branch');
      const base = pickString(args, 'base', 'base_ref', 'base_branch', 'target', 'target_branch') || repo.defaultBranch;
      const headTip = refTip(repo, head);
      const baseTip = refTip(repo, base);
      if (headTip === undefined || baseTip === undefined) {
        return { ok: true, result: toolResult({ error: 'head or base branch does not exist' }, true) };
      }
      const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.head === head && pull.base === base);
      if (duplicate !== undefined) return { ok: true, result: toolResult({ error: 'a pull request for this head already exists' }, true) };
      const number = repo.nextNumber++;
      const seq = store.clock.next();
      const pull: Pull = {
        id: store.nextItem(),
        number,
        title: pickString(args, 'title', 'pr_title'),
        body: pickString(args, 'body', 'description'),
        state: 'open',
        merged: false,
        mergedAt: null,
        mergeSha: null,
        head,
        base,
        headSha: headTip,
        baseSha: baseTip,
        baseShaAtCreate: baseTip,
        files: [],
        seq,
      };
      computePullFiles(store, repo, pull);
      repo.pulls.push(pull);
      return { ok: true, result: toolResult(pullView(store, repo, pull)) };
    }
    if (tool.name === 'merge_pull_request') {
      const number = pickNumber(args, 'pull_number', 'pull_request_number', 'pr_number', 'number');
      const pull = repo.pulls.find((entry) => entry.number === number);
      if (pull === undefined) return { ok: true, result: toolResult({ error: 'pull request not found' }, true) };
      if (pull.state !== 'open') return { ok: true, result: toolResult({ error: 'pull request is not open' }, true) };
      const result = squashMerge(store, repo, pull);
      if ('conflict' in result) return { ok: true, result: toolResult({ error: 'merge conflict' }, true) };
      pull.state = 'closed';
      pull.merged = true;
      pull.mergedAt = store.clock.iso(store.clock.next());
      pull.mergeSha = result.sha;
      return { ok: true, result: toolResult({ merged: true, sha: result.sha, number: pull.number, state: 'closed' }) };
    }
    return { ok: false, code: -32601, message: `unknown tool ${name}` };
  } catch (err) {
    return { ok: false, code: -32602, message: err instanceof Error ? err.message : 'tool failed' };
  }
}

export async function handleMcp(
  store: Store,
  req: IncomingMessage,
  res: ServerResponse,
  raw: Buffer,
  authed: boolean,
): Promise<void> {
  const send = (status: number, body: unknown, session = false): void => {
    const text = `${JSON.stringify(body)}\n`;
    const headers: Record<string, string> = {
      'content-type': 'application/json',
      'content-length': String(Buffer.byteLength(text)),
    };
    if (session) headers['mcp-session-id'] = 'forge-session-1';
    res.writeHead(status, headers);
    res.end(text);
  };
  const rpcError = (id: unknown, code: number, message: string): Record<string, unknown> => ({
    jsonrpc: '2.0',
    id: id ?? null,
    error: { code, message },
  });
  if (!authed) {
    let id: unknown = null;
    try {
      const parsed = JSON.parse(raw.toString('utf8')) as Record<string, unknown>;
      id = parsed.id ?? null;
    } catch {
      // keep null
    }
    send(401, rpcError(id, -32001, 'Requires authentication'));
    return;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw.toString('utf8'));
  } catch {
    send(400, rpcError(null, -32700, 'Parse error'));
    return;
  }
  const record = (parsed !== null && typeof parsed === 'object' && !Array.isArray(parsed) ? (parsed as Record<string, unknown>) : {}) as Record<string, unknown>;
  const id = record.id ?? null;
  const method = typeof record.method === 'string' ? record.method : '';
  const params = (record.params !== null && typeof record.params === 'object' ? (record.params as Record<string, unknown>) : {}) as Record<string, unknown>;
  const isNotification = record.id === undefined;
  if (isNotification) {
    res.writeHead(202, { 'mcp-session-id': 'forge-session-1' });
    res.end();
    return;
  }
  const result = (value: unknown): Record<string, unknown> => ({ jsonrpc: '2.0', id, result: value });
  if (method === 'server/discover') {
    send(200, result({
      resultType: 'discover',
      supportedVersions: ['2026-07-28', '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'],
      protocolVersion: '2026-07-28',
      capabilities: { tools: {} },
      serverInfo: { name: 'forge', version: '1.0.0' },
      _meta: {
        'io.modelcontextprotocol/serverInfo': { name: 'forge', version: '1.0.0' },
        'io.modelcontextprotocol/protocolVersion': '2026-07-28',
      },
    }), true);
    return;
  }
  if (method === 'initialize') {
    const requested = String((params.protocolVersion as string | undefined) ?? '');
    const chosen = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'].includes(requested) ? requested : '2025-11-25';
    send(200, result({
      protocolVersion: chosen,
      capabilities: { tools: {} },
      serverInfo: { name: 'forge', version: '1.0.0' },
    }), true);
    return;
  }
  if (method === 'ping') {
    send(200, result({}), true);
    return;
  }
  if (method === 'tools/list') {
    send(200, result({ tools: TOOLS }), true);
    return;
  }
  if (method === 'tools/call') {
    const name = String(params.name ?? '');
    const args = (params.arguments !== null && typeof params.arguments === 'object' ? (params.arguments as Record<string, unknown>) : {}) as Record<string, unknown>;
    const outcome = runTool(store, name, args);
    if (!outcome.ok) {
      send(200, rpcError(id, outcome.code, outcome.message), true);
      return;
    }
    send(200, result(outcome.result), true);
    return;
  }
  send(200, rpcError(id, -32601, `unknown method ${method}`), true);
}
