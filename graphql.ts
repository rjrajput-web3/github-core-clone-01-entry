// GitForge GraphQL v4: a small parser and executor for the documents gh and
// github-mcp-server actually send (viewer, repository reads, issue and pull
// request reads, the create/close/merge mutations, __type introspection and
// Relay node()). Written from scratch; no GraphQL library is available in
// the trial, and only these shapes are needed.

import {
  VIEWER_LOGIN,
  VIEWER_ID,
  gid,
  ungid,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

// ---------------------------------------------------------------------------
// tiny GraphQL parser (documents, selection sets, arguments, fragments)
// ---------------------------------------------------------------------------

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
  kind: 'field';
  alias: string | null;
  name: string;
  args: Array<{ name: string; value: ValueNode }>;
  selections: SelectionNode[] | null;
}

interface FragmentNode {
  kind: 'fragment';
  typeCondition: string | null;
  selections: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentNode;

interface OperationNode {
  type: 'query' | 'mutation';
  name: string | null;
  variables: string[];
  selections: SelectionNode[];
}

function tokenize(src: string): string[] {
  const tokens: string[] = [];
  let i = 0;
  while (i < src.length) {
    const c = src[i];
    if (c === ' ' || c === '\t' || c === '\n' || c === '\r' || c === ',') {
      i += 1;
      continue;
    }
    if (c === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (c === '.' && src.slice(i, i + 3) === '...') {
      tokens.push('...');
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@='.includes(c!)) {
      tokens.push(c!);
      i += 1;
      continue;
    }
    if (c === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const n = src[j + 1];
          out += n === 'n' ? '\n' : n === 't' ? '\t' : n === 'r' ? '\r' : n;
          j += 2;
        } else {
          out += src[j];
          j += 1;
        }
      }
      tokens.push(JSON.stringify(out));
      i = j + 1;
      continue;
    }
    let j = i;
    while (j < src.length && /[A-Za-z0-9_.-]/.test(src[j]!)) j += 1;
    if (j === i) {
      i += 1;
      continue;
    }
    tokens.push(src.slice(i, j));
    i = j;
  }
  return tokens;
}

class Parser {
  tokens: string[];
  pos = 0;
  constructor(src: string) {
    this.tokens = tokenize(src);
  }
  peek(): string | undefined {
    return this.tokens[this.pos];
  }
  next(): string {
    return this.tokens[this.pos++] ?? '';
  }
  expect(tok: string): void {
    if (this.next() !== tok) throw new Error(`expected ${tok}`);
  }
  parseDocument(): { operations: OperationNode[]; fragments: Map<string, FragmentNode> } {
    const operations: OperationNode[] = [];
    const fragments = new Map<string, FragmentNode>();
    while (this.pos < this.tokens.length) {
      const tok = this.peek();
      if (tok === 'query' || tok === 'mutation') {
        this.next();
        let name: string | null = null;
        if (this.peek() !== undefined && this.peek() !== '(' && this.peek() !== '{') name = this.next();
        const variables: string[] = [];
        if (this.peek() === '(') {
          this.next();
          while (this.peek() !== ')' && this.pos < this.tokens.length) {
            const t = this.next();
            if (t === '$') variables.push(this.next());
          }
          this.expect(')');
        }
        operations.push({ type: tok as 'query' | 'mutation', name, variables, selections: this.parseSelections() });
      } else if (tok === 'fragment') {
        this.next();
        const name = this.next();
        this.expect('on');
        const typeCondition = this.next();
        fragments.set(name, { kind: 'fragment', typeCondition, selections: this.parseSelections() });
      } else if (tok === '{') {
        operations.push({ type: 'query', name: null, variables: [], selections: this.parseSelections() });
      } else {
        this.next();
      }
    }
    return { operations, fragments };
  }
  parseSelections(): SelectionNode[] {
    this.expect('{');
    const out: SelectionNode[] = [];
    while (this.peek() !== '}' && this.pos < this.tokens.length) {
      if (this.peek() === '...') {
        this.next();
        const next = this.peek();
        if (next === 'on') {
          this.next();
          const typeCondition = this.next();
          out.push({ kind: 'fragment', typeCondition, selections: this.parseSelections() });
        } else {
          this.next(); // fragment name; spreads are not used by the suite's documents
          out.push({ kind: 'fragment', typeCondition: null, selections: [] });
        }
        continue;
      }
      let name = this.next();
      let alias: string | null = null;
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
      let selections: SelectionNode[] | null = null;
      if (this.peek() === '{') selections = this.parseSelections();
      out.push({ kind: 'field', alias, name, args, selections });
    }
    this.expect('}');
    return out;
  }
  parseValue(): ValueNode {
    const tok = this.next();
    if (tok === '$') return { kind: 'variable', name: this.next() };
    if (tok === '[') {
      const values: ValueNode[] = [];
      while (this.peek() !== ']' && this.pos < this.tokens.length) values.push(this.parseValue());
      this.expect(']');
      return { kind: 'list', values };
    }
    if (tok === '{') {
      const fields: Array<{ name: string; value: ValueNode }> = [];
      while (this.peek() !== '}' && this.pos < this.tokens.length) {
        const name = this.next();
        this.expect(':');
        fields.push({ name, value: this.parseValue() });
      }
      this.expect('}');
      return { kind: 'object', fields };
    }
    if (tok === 'true') return { kind: 'boolean', value: true };
    if (tok === 'false') return { kind: 'boolean', value: false };
    if (tok === 'null') return { kind: 'null' };
    if (tok.startsWith('"')) return { kind: 'string', value: JSON.parse(tok) as string };
    if (/^-?\d+$/.test(tok)) return { kind: 'int', value: Number(tok) };
    if (/^-?\d+\.\d+$/.test(tok)) return { kind: 'float', value: Number(tok) };
    return { kind: 'enum', value: tok };
  }
}

function valueOf(node: ValueNode, variables: Record<string, unknown>): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name] ?? null;
    case 'string':
    case 'int':
    case 'float':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((v) => valueOf(v, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const f of node.fields) out[f.name] = valueOf(f.value, variables);
      return out;
    }
  }
}

function argsOf(field: FieldNode, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const a of field.args) out[a.name] = valueOf(a.value, variables);
  return out;
}

// ---------------------------------------------------------------------------
// schema surface for __type
// ---------------------------------------------------------------------------

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'labels', 'user', 'comments', 'url'],
  PullRequest: ['id', 'number', 'title', 'body', 'state', 'merged', 'mergedAt', 'mergeCommitSha', 'headRefName', 'baseRefName', 'headRef', 'baseRef', 'mergeable', 'files', 'createdAt', 'updatedAt'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'private', 'createdAt', 'updatedAt'],
  User: ['id', 'login', 'name', 'email'],
  SearchType: [],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT'],
  IssueState: ['OPEN', 'CLOSED'],
  PullRequestState: ['OPEN', 'CLOSED', 'MERGED'],
  MergeableState: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'],
  OrderDirection: ['ASC', 'DESC'],
  IssueOrderField: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'],
  RepositoryPermission: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'],
  PullRequestMergeMethod: ['MERGE', 'SQUASH', 'REBASE'],
};

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type NodeValue =
  | { __type: 'Query' }
  | { __type: 'User'; login: string }
  | { __type: 'Repository'; repo: Repo }
  | { __type: 'Ref'; name: string }
  | { __type: 'Issue'; repo: Repo; issue: Issue }
  | { __type: 'IssueConnection'; repo: Repo; issues: Issue[]; endCursor: string | null; hasNextPage: boolean }
  | { __type: 'PageInfo'; hasNextPage: boolean; endCursor: string | null }
  | { __type: 'PullRequest'; repo: Repo; pr: Pull }
  | { __type: 'PullRequestConnection'; repo: Repo; prs: Pull[] }
  | { __type: 'PullRequestChangedFileConnection'; files: Array<{ path: string }> }
  | { __type: 'PullRequestChangedFile'; path: string }
  | { __type: 'Type'; name: string }
  | { __type: 'CommentEdge'; id: string; body: string }
  | { __type: 'AddCommentPayload'; id: string; body: string }
  | { __type: 'CreateIssuePayload'; repo: Repo; issue: Issue }
  | { __type: 'CloseIssuePayload'; repo: Repo; issue: Issue }
  | { __type: 'CreatePullRequestPayload'; repo: Repo; pr: Pull }
  | { __type: 'MergePullRequestPayload'; repo: Repo; pr: Pull };

function issueGid(repo: Repo, issue: Issue): string {
  return gid('I', repo.id, issue.number);
}
function prGid(repo: Repo, pr: Pull): string {
  return gid('P', repo.id, pr.number);
}
function repoGid(repo: Repo): string {
  return gid('R', repo.id);
}

function repoById(store: Store, id: number): Repo | undefined {
  for (const repo of store.repos.values()) if (repo.id === id) return repo;
  return undefined;
}

function cursorFor(number: number): string {
  return Buffer.from(`c:${number}`, 'utf8').toString('base64url');
}

function cursorNumber(cursor: string | null | undefined): number | null {
  if (typeof cursor !== 'string' || cursor === '') return null;
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    if (!text.startsWith('c:')) return null;
    const n = Number(text.slice(2));
    return Number.isSafeInteger(n) ? n : null;
  } catch {
    return null;
  }
}

class Executor {
  store: Store;
  errors: Array<{ message: string }> = [];
  constructor(store: Store) {
    this.store = store;
  }

  unknownField(typeName: string, field: string): void {
    this.errors.push({ message: `Field '${field}' doesn't exist on type '${typeName}'` });
  }

  resolveSelections(node: NodeValue, selections: SelectionNode[] | null): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    if (!selections) return out;
    for (const sel of selections) {
      if (sel.kind === 'fragment') {
        if (sel.typeCondition !== null && sel.typeCondition !== node.__type) continue;
        Object.assign(out, this.resolveSelections(node, sel.selections));
        continue;
      }
      const key = sel.alias ?? sel.name;
      out[key] = this.resolveField(node, sel);
    }
    return out;
  }

  resolveField(node: NodeValue, field: FieldNode): unknown {
    const args = argsOf(field, this.variables);
    switch (node.__type) {
      case 'Query':
        return this.queryField(field, args);
      case 'User':
        if (field.name === 'login') return node.login;
        if (field.name === 'id') return gid('U', node.login);
        this.unknownField('User', field.name);
        return null;
      case 'Repository':
        return this.repositoryField(node, field, args);
      case 'Ref':
        if (field.name === 'name') return node.name;
        this.unknownField('Ref', field.name);
        return null;
      case 'Issue':
        return this.issueField(node, field);
      case 'IssueConnection': {
        if (field.name === 'nodes') {
          return node.issues.map((issue) => this.resolveSelections({ __type: 'Issue', repo: node.repo, issue }, field.selections));
        }
        if (field.name === 'pageInfo') {
          return this.resolveSelections({ __type: 'PageInfo', hasNextPage: node.hasNextPage, endCursor: node.endCursor }, field.selections);
        }
        if (field.name === 'totalCount') return node.issues.length;
        this.unknownField('IssueConnection', field.name);
        return null;
      }
      case 'PageInfo':
        if (field.name === 'hasNextPage') return node.hasNextPage;
        if (field.name === 'endCursor') return node.endCursor;
        this.unknownField('PageInfo', field.name);
        return null;
      case 'PullRequest':
        return this.pullRequestField(node, field, args);
      case 'PullRequestConnection': {
        if (field.name === 'nodes') {
          return node.prs.map((pr) => this.resolveSelections({ __type: 'PullRequest', repo: node.repo, pr }, field.selections));
        }
        if (field.name === 'totalCount') return node.prs.length;
        this.unknownField('PullRequestConnection', field.name);
        return null;
      }
      case 'PullRequestChangedFileConnection': {
        if (field.name === 'nodes') {
          return node.files.map((f) => this.resolveSelections({ __type: 'PullRequestChangedFile', path: f.path }, field.selections));
        }
        this.unknownField('PullRequestChangedFileConnection', field.name);
        return null;
      }
      case 'PullRequestChangedFile':
        if (field.name === 'path') return node.path;
        this.unknownField('PullRequestChangedFile', field.name);
        return null;
      case 'Type': {
        if (field.name === 'fields') {
          return (Object.prototype.hasOwnProperty.call(TYPE_FIELDS, node.name) ? TYPE_FIELDS[node.name]! : []).map((n) => ({ name: n }));
        }
        if (field.name === 'enumValues') {
          return (Object.prototype.hasOwnProperty.call(TYPE_ENUM_VALUES, node.name) ? TYPE_ENUM_VALUES[node.name]! : []).map((n) => ({ name: n }));
        }
        if (field.name === 'name') return node.name;
        this.unknownField('__Type', field.name);
        return null;
      }
      case 'CommentEdge':
        if (field.name === 'node') return this.resolveSelections({ __type: 'AddCommentPayload', id: node.id, body: node.body }, field.selections);
        this.unknownField('IssueCommentEdge', field.name);
        return null;
      case 'AddCommentPayload':
        if (field.name === 'commentEdge') return this.resolveSelections({ __type: 'CommentEdge', id: node.id, body: node.body }, field.selections);
        if (field.name === 'id') return node.id;
        if (field.name === 'body') return node.body;
        this.unknownField('AddCommentPayload', field.name);
        return null;
      case 'CreateIssuePayload':
        if (field.name === 'issue') return this.resolveSelections({ __type: 'Issue', repo: node.repo, issue: node.issue }, field.selections);
        this.unknownField('CreateIssuePayload', field.name);
        return null;
      case 'CloseIssuePayload':
        if (field.name === 'issue') return this.resolveSelections({ __type: 'Issue', repo: node.repo, issue: node.issue }, field.selections);
        this.unknownField('CloseIssuePayload', field.name);
        return null;
      case 'CreatePullRequestPayload':
        if (field.name === 'pullRequest') return this.resolveSelections({ __type: 'PullRequest', repo: node.repo, pr: node.pr }, field.selections);
        this.unknownField('CreatePullRequestPayload', field.name);
        return null;
      case 'MergePullRequestPayload':
        if (field.name === 'pullRequest') return this.resolveSelections({ __type: 'PullRequest', repo: node.repo, pr: node.pr }, field.selections);
        this.unknownField('MergePullRequestPayload', field.name);
        return null;
    }
  }

  variables: Record<string, unknown> = {};

  queryField(field: FieldNode, args: Record<string, unknown>): unknown {
    if (field.name === 'viewer') {
      return this.resolveSelections({ __type: 'User', login: VIEWER_LOGIN }, field.selections);
    }
    if (field.name === 'repository') {
      const owner = args.owner as string;
      const name = args.name as string;
      const repo = typeof owner === 'string' && typeof name === 'string' ? this.store.getRepo(owner, name) : undefined;
      if (!repo) return null;
      return this.resolveSelections({ __type: 'Repository', repo }, field.selections);
    }
    if (field.name === 'node') {
      const decoded = typeof args.id === 'string' ? ungid(args.id) : null;
      if (!decoded) return null;
      if (decoded.kind === 'R') {
        const repo = repoById(this.store, Number(decoded.parts[0]));
        if (!repo) return null;
        return this.resolveSelections({ __type: 'Repository', repo }, field.selections);
      }
      if (decoded.kind === 'I') {
        const repo = repoById(this.store, Number(decoded.parts[0]));
        const issue = repo?.issues.get(Number(decoded.parts[1]));
        if (!repo || !issue) return null;
        return this.resolveSelections({ __type: 'Issue', repo, issue }, field.selections);
      }
      if (decoded.kind === 'P') {
        const repo = repoById(this.store, Number(decoded.parts[0]));
        const pr = repo?.pulls.get(Number(decoded.parts[1]));
        if (!repo || !pr) return null;
        return this.resolveSelections({ __type: 'PullRequest', repo, pr }, field.selections);
      }
      return null;
    }
    if (field.name === '__type') {
      const name = args.name as string;
      if (typeof name !== 'string' || (!Object.prototype.hasOwnProperty.call(TYPE_FIELDS, name) && !Object.prototype.hasOwnProperty.call(TYPE_ENUM_VALUES, name))) return null;
      return this.resolveSelections({ __type: 'Type', name }, field.selections);
    }
    this.unknownField('Query', field.name);
    return null;
  }

  repositoryField(node: { repo: Repo }, field: FieldNode, args: Record<string, unknown>): unknown {
    const repo = node.repo;
    switch (field.name) {
      case 'id':
        return repoGid(repo);
      case 'databaseId':
        return repo.id;
      case 'name':
        return repo.name;
      case 'nameWithOwner':
        return `${repo.owner}/${repo.name}`;
      case 'owner':
        return this.resolveSelections({ __type: 'User', login: repo.owner }, field.selections);
      case 'defaultBranchRef': {
        const tip = this.store.defaultTip(repo);
        if (tip === undefined) return null;
        return this.resolveSelections({ __type: 'Ref', name: repo.defaultBranch }, field.selections);
      }
      case 'viewerPermission':
        return 'ADMIN';
      case 'hasIssuesEnabled':
        return true;
      case 'description':
        return repo.description;
      case 'private':
        return repo.private;
      case 'issue': {
        const issue = repo.issues.get(Number(args.number));
        if (!issue) return null;
        return this.resolveSelections({ __type: 'Issue', repo, issue }, field.selections);
      }
      case 'issues': {
        let list = [...repo.issues.values()];
        const states = args.states as string[] | undefined;
        if (Array.isArray(states) && states.length > 0) {
          const wanted = new Set(states.map((s) => String(s).toLowerCase()));
          list = list.filter((i) => wanted.has(i.state));
        }
        const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
        const desc = orderBy?.direction === 'DESC';
        list.sort((a, b) => (desc ? b.number - a.number : a.number - b.number));
        const afterNum = cursorNumber(args.after as string | null | undefined);
        if (afterNum !== null) {
          const idx = list.findIndex((i) => i.number === afterNum);
          if (idx >= 0) list = list.slice(idx + 1);
        }
        const first = typeof args.first === 'number' ? args.first : list.length;
        const page = list.slice(0, first);
        const hasNextPage = list.length > page.length;
        const endCursor = page.length > 0 ? cursorFor(page[page.length - 1]!.number) : null;
        return this.resolveSelections({ __type: 'IssueConnection', repo, issues: page, endCursor, hasNextPage }, field.selections);
      }
      case 'pullRequests': {
        let list = [...repo.pulls.values()].filter((p) => p.state === 'open');
        list.sort((a, b) => a.number - b.number);
        const first = typeof args.first === 'number' ? args.first : list.length;
        list = list.slice(0, first);
        return this.resolveSelections({ __type: 'PullRequestConnection', repo, prs: list }, field.selections);
      }
      case 'pullRequest': {
        const pr = repo.pulls.get(Number(args.number));
        if (!pr) return null;
        return this.resolveSelections({ __type: 'PullRequest', repo, pr }, field.selections);
      }
      default:
        this.unknownField('Repository', field.name);
        return null;
    }
  }

  issueField(node: { repo: Repo; issue: Issue }, field: FieldNode): unknown {
    const { repo, issue } = node;
    switch (field.name) {
      case 'id':
        return issueGid(repo, issue);
      case 'number':
        return issue.number;
      case 'title':
        return issue.title;
      case 'state':
        return issue.state === 'open' ? 'OPEN' : 'CLOSED';
      case 'body':
        return issue.body;
      default:
        this.unknownField('Issue', field.name);
        return null;
    }
  }

  pullRequestField(node: { repo: Repo; pr: Pull }, field: FieldNode, args: Record<string, unknown>): unknown {
    const { repo, pr } = node;
    switch (field.name) {
      case 'id':
        return prGid(repo, pr);
      case 'number':
        return pr.number;
      case 'title':
        return pr.title;
      case 'body':
        return pr.body;
      case 'state':
        return pr.merged ? 'MERGED' : pr.state === 'open' ? 'OPEN' : 'CLOSED';
      case 'merged':
        return pr.merged;
      case 'headRefName':
        return pr.headRef;
      case 'baseRefName':
        return pr.baseRef;
      case 'mergeable':
        return this.store.pullMergeable(repo, pr);
      case 'files': {
        const files = this.store.pullFiles(repo, pr).map((f) => ({ path: f.path }));
        const first = typeof args.first === 'number' ? args.first : files.length;
        return this.resolveSelections({ __type: 'PullRequestChangedFileConnection', files: files.slice(0, first) }, field.selections);
      }
      default:
        this.unknownField('PullRequest', field.name);
        return null;
    }
  }

  runMutation(field: FieldNode, args: Record<string, unknown>): unknown {
    const input = (args.input ?? {}) as Record<string, unknown>;
    if (field.name === 'createIssue') {
      const decoded = typeof input.repositoryId === 'string' ? ungid(input.repositoryId) : null;
      const repo = decoded?.kind === 'R' ? repoById(this.store, Number(decoded.parts[0])) : undefined;
      if (!repo) {
        this.errors.push({ message: 'createIssue: unknown repository' });
        return null;
      }
      const title = typeof input.title === 'string' ? input.title : '';
      const body = typeof input.body === 'string' ? input.body : '';
      const made = this.store.createIssue(repo, title, body, []);
      if ('error' in made) {
        this.errors.push({ message: `createIssue: ${made.error}` });
        return null;
      }
      return this.resolveSelections({ __type: 'CreateIssuePayload', repo, issue: made }, field.selections);
    }
    if (field.name === 'closeIssue') {
      const decoded = typeof input.issueId === 'string' ? ungid(input.issueId) : null;
      const repo = decoded?.kind === 'I' ? repoById(this.store, Number(decoded.parts[0])) : undefined;
      const issue = repo?.issues.get(Number(decoded?.parts[1]));
      if (!repo || !issue) {
        this.errors.push({ message: 'closeIssue: unknown issue' });
        return null;
      }
      issue.state = 'closed';
      issue.updatedAt = new Date().toISOString();
      issue.json = null;
      return this.resolveSelections({ __type: 'CloseIssuePayload', repo, issue }, field.selections);
    }
    if (field.name === 'createPullRequest') {
      const decoded = typeof input.repositoryId === 'string' ? ungid(input.repositoryId) : null;
      const repo = decoded?.kind === 'R' ? repoById(this.store, Number(decoded.parts[0])) : undefined;
      if (!repo) {
        this.errors.push({ message: 'createPullRequest: unknown repository' });
        return null;
      }
      const made = this.store.createPull(
        repo,
        typeof input.title === 'string' ? input.title : '',
        typeof input.body === 'string' ? input.body : '',
        typeof input.headRefName === 'string' ? input.headRefName : '',
        typeof input.baseRefName === 'string' ? input.baseRefName : '',
      );
      if ('error' in made) {
        this.errors.push({ message: `createPullRequest: ${made.error}` });
        return null;
      }
      return this.resolveSelections({ __type: 'CreatePullRequestPayload', repo, pr: made }, field.selections);
    }
    if (field.name === 'mergePullRequest') {
      const decoded = typeof input.pullRequestId === 'string' ? ungid(input.pullRequestId) : null;
      const repo = decoded?.kind === 'P' ? repoById(this.store, Number(decoded.parts[0])) : undefined;
      const pr = repo?.pulls.get(Number(decoded?.parts[1]));
      if (!repo || !pr) {
        this.errors.push({ message: 'mergePullRequest: unknown pull request' });
        return null;
      }
      const merged = this.store.mergePull(repo, pr);
      if (!merged.ok) {
        this.errors.push({ message: `mergePullRequest: ${merged.reason}` });
        return null;
      }
      return this.resolveSelections({ __type: 'MergePullRequestPayload', repo, pr }, field.selections);
    }
    if (field.name === 'addComment') {
      const decoded = typeof input.subjectId === 'string' ? ungid(input.subjectId) : null;
      const repo = decoded?.kind === 'I' ? repoById(this.store, Number(decoded.parts[0])) : undefined;
      const issue = repo?.issues.get(Number(decoded?.parts[1]));
      if (!repo || !issue) {
        this.errors.push({ message: 'addComment: unknown subject' });
        return null;
      }
      const body = typeof input.body === 'string' ? input.body : '';
      const comment = {
        id: this.store.nextItemId++,
        body,
        user: VIEWER_LOGIN,
        createdAt: new Date().toISOString(),
        seq: this.store.nextSeq++,
      };
      issue.comments.push(comment);
      issue.json = null;
      const nodeId = gid('C', comment.id);
      return this.resolveSelections({ __type: 'AddCommentPayload', id: nodeId, body }, field.selections);
    }
    this.unknownField('Mutation', field.name);
    return null;
  }
}

// ---------------------------------------------------------------------------
// entry point: one operation document (or a batch array) -> JSON result
// ---------------------------------------------------------------------------

export function executeGraphQL(store: Store, body: unknown): { status: number; json: unknown } {
  if (Array.isArray(body)) {
    const out: unknown[] = [];
    for (const item of body) out.push(executeOne(store, item));
    return { status: 200, json: out };
  }
  return { status: 200, json: executeOne(store, body) };
}

function executeOne(store: Store, body: unknown): Record<string, unknown> {
  const exec = new Executor(store);
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    return { data: null, errors: [{ message: 'GraphQL body must be an object with query and variables' }] };
  }
  const query = (body as Record<string, unknown>).query;
  const variables = (body as Record<string, unknown>).variables;
  if (typeof query !== 'string' || query.trim() === '') {
    return { data: null, errors: [{ message: 'GraphQL query must be a non-empty string' }] };
  }
  exec.variables = (variables !== null && typeof variables === 'object' && !Array.isArray(variables) ? variables : {}) as Record<string, unknown>;
  let doc;
  try {
    doc = new Parser(query).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: `GraphQL parse error: ${err instanceof Error ? err.message : String(err)}` }] };
  }
  const operationName = (body as Record<string, unknown>).operationName;
  let op = doc.operations[0] ?? null;
  if (typeof operationName === 'string' && operationName !== '') {
    op = doc.operations.find((o) => o.name === operationName) ?? null;
  }
  if (!op) return { data: null, errors: [{ message: 'no operation to execute' }] };
  const data: Record<string, unknown> = {};
  if (op.type === 'mutation') {
    for (const sel of op.selections) {
      if (sel.kind === 'fragment') continue;
      const key = sel.alias ?? sel.name;
      data[key] = exec.runMutation(sel, argsOf(sel, exec.variables));
    }
  } else {
    Object.assign(data, exec.resolveSelections({ __type: 'Query' }, op.selections));
  }
  const out: Record<string, unknown> = { data };
  if (exec.errors.length > 0) out.errors = exec.errors;
  return out;
}

export { VIEWER_ID };
