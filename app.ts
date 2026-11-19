import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import {
  buildTreeFromFiles,
  computePullFiles,
  flattenTree,
  makeCommit,
  parseCommit,
  pullConflicts,
  refTip,
  squashMerge,
  storeObject,
  treeFiles,
  VIEWER_EMAIL,
  VIEWER_ID,
  VIEWER_LOGIN,
  VIEWER_NAME,
  type Issue,
  type Label,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack, type GitRequest } from './git.ts';
import { handleGraphQL, handleMcp } from './gql.ts';

const JSON_TYPE = 'application/json; charset=utf-8';

interface Ctx {
  store: Store;
  token: string;
  reqId: number;
}

function nextReqId(ctx: Ctx): string {
  ctx.reqId += 1;
  return `req-${ctx.reqId}`;
}

function sendJson(ctx: Ctx, res: ServerResponse, status: number, body: unknown, extra: Record<string, string> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': nextReqId(ctx),
    ...extra,
  });
  res.end(text);
}

function sendEmpty(ctx: Ctx, res: ServerResponse, status: number): void {
  res.writeHead(status, { 'x-github-request-id': nextReqId(ctx) });
  res.end();
}

function sendError(ctx: Ctx, res: ServerResponse, status: number, message: string): void {
  sendJson(ctx, res, status, { message });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => {
      chunks.push(chunk);
      if (Buffer.concat(chunks).length > 16 * 1024 * 1024) reject(new Error('body too large'));
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function bearerToken(req: IncomingMessage): string | null {
  const header = req.headers['authorization'];
  if (typeof header !== 'string') return null;
  const match = /^Bearer (.+)$/.exec(header.trim());
  return match === null ? null : match[1];
}

function isAuthed(ctx: Ctx, req: IncomingMessage): boolean {
  const token = bearerToken(req);
  if (token === null || token === '') return false;
  if (ctx.token === '') return true;
  return token === ctx.token;
}

function parseJsonBody(raw: Buffer): { ok: true; value: unknown } | { ok: false } {
  if (raw.length === 0) return { ok: true, value: undefined };
  try {
    return { ok: true, value: JSON.parse(raw.toString('utf8')) };
  } catch {
    return { ok: false };
  }
}

function asRecord(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function asString(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

function asBool(value: unknown, fallback = false): boolean {
  return typeof value === 'boolean' ? value : fallback;
}

// -------------------------------------------------------------------------------------------
// JSON views
// -------------------------------------------------------------------------------------------

function userJson(): Record<string, unknown> {
  return { login: VIEWER_LOGIN, id: VIEWER_ID, node_id: `U_${VIEWER_LOGIN}`, type: 'User', name: VIEWER_NAME, email: VIEWER_EMAIL };
}

export function repoJson(ctx: Ctx, repo: Repo): Record<string, unknown> {
  const created = ctx.store.clock.iso(repo.seq);
  return {
    id: repo.id,
    node_id: ctx.store.repoGlobalId(repo),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: userJson(),
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: created,
    updated_at: created,
    pushed_at: created,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    fork: false,
    archived: false,
    disabled: false,
    visibility: repo.isPrivate ? 'private' : 'public',
    license: null,
    topics: [],
    open_issues_count: repo.issues.filter((issue) => issue.state === 'open').length,
    watchers: 0,
    stargazers_count: 0,
    forks_count: 0,
    size: 0,
  };
}

function labelJson(ctx: Ctx, repo: Repo, label: Label): Record<string, unknown> {
  return {
    id: ctx.store.nextItem(),
    node_id: `L_${repo.id}_${label.name}`,
    name: label.name,
    color: label.color,
    description: '',
    default: false,
  };
}

function issueJson(ctx: Ctx, repo: Repo, issue: Issue): Record<string, unknown> {
  const created = ctx.store.clock.iso(issue.seq);
  return {
    id: issue.id,
    node_id: ctx.store.issueGlobalId(repo, issue),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.get(name) ?? { name, color: 'ededed' };
      return { id: ctx.store.nextItem(), node_id: `L_${repo.id}_${name}`, name, color: label.color, description: '', default: false };
    }),
    user: userJson(),
    assignee: null,
    milestone: null,
    locked: false,
    comments: issue.comments.length,
    created_at: created,
    updated_at: created,
    closed_at: issue.state === 'closed' ? created : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
    repository_url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    author_association: 'OWNER',
  };
}

function pullJson(ctx: Ctx, repo: Repo, pull: Pull): Record<string, unknown> {
  const created = ctx.store.clock.iso(pull.seq);
  const headSha = repo.refs.get(`refs/heads/${pull.head}`) ?? pull.headSha;
  const baseSha = repo.refs.get(`refs/heads/${pull.base}`) ?? pull.baseSha;
  return {
    id: pull.id,
    node_id: ctx.store.pullGlobalId(repo, pull),
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeSha,
    mergeable: !pullConflicts(ctx.store, repo, pull),
    draft: false,
    head: { ref: pull.head, sha: headSha, label: `${repo.owner}:${pull.head}`, user: userJson() },
    base: { ref: pull.base, sha: baseSha, label: `${repo.owner}:${pull.base}`, user: userJson() },
    user: userJson(),
    created_at: created,
    updated_at: created,
    closed_at: pull.state === 'closed' ? created : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

function commentJson(ctx: Ctx, repo: Repo, issue: Issue, comment: { id: number; body: string; seq: number }): Record<string, unknown> {
  const created = ctx.store.clock.iso(comment.seq);
  return {
    id: comment.id,
    node_id: ctx.store.commentGlobalId(repo, issue, comment),
    body: comment.body,
    user: userJson(),
    created_at: created,
    updated_at: created,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}#issuecomment-${comment.id}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments/${comment.id}`,
  };
}

// -------------------------------------------------------------------------------------------
// git-backed content and pull helpers
// -------------------------------------------------------------------------------------------

function commitFiles(ctx: Ctx, repo: Repo, branch: string, message: string, mutate: (files: Map<string, { oid: string; mode: string }>) => void): string | null {
  const tip = refTip(repo, branch);
  const files = treeFiles(repo, tip);
  mutate(files);
  const treeOid = buildTreeFromFiles(repo, files);
  const parents = tip === undefined ? [] : [tip];
  const sha = makeCommit(repo, ctx.store, treeOid, parents, message);
  repo.refs.set(`refs/heads/${branch}`, sha);
  return sha;
}

function commitList(repo: Repo, tip: string | undefined, limit = 100): Array<Record<string, unknown>> {
  const out: Array<Record<string, unknown>> = [];
  if (tip === undefined) return out;
  const seen = new Set<string>();
  const queue = [tip];
  while (queue.length > 0 && out.length < limit) {
    const oid = queue.shift() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') continue;
    const parsed = parseCommit(obj.data);
    const author = parseIdentity(parsed.author);
    const committer = parseIdentity(parsed.committer);
    out.push({
      sha: oid,
      node_id: `C_${oid}`,
      commit: {
        message: parsed.message,
        author: { name: author.name, email: author.email, date: new Date(author.stamp * 1000).toISOString() },
        committer: { name: committer.name, email: committer.email, date: new Date(committer.stamp * 1000).toISOString() },
        tree: { sha: parsed.tree },
      },
      author: { login: VIEWER_LOGIN, id: VIEWER_ID },
      committer: { login: VIEWER_LOGIN, id: VIEWER_ID },
      parents: parsed.parents.map((parent) => ({ sha: parent })),
    });
    for (const parent of parsed.parents) queue.push(parent);
  }
  return out;
}

function parseIdentity(line: string): { name: string; email: string; stamp: number } {
  const match = /^(.*) <([^>]*)> (\d+) [+-]\d{4}$/.exec(line);
  if (match === null) return { name: VIEWER_NAME, email: VIEWER_EMAIL, stamp: 1735689600 };
  return { name: match[1], email: match[2], stamp: Number(match[3]) };
}

// -------------------------------------------------------------------------------------------
// routing
// -------------------------------------------------------------------------------------------

function splitPath(path: string): string[] {
  return path.split('/').filter((part) => part !== '');
}

function pageOf(query: URLSearchParams): { page: number; perPage: number } {
  const page = Math.max(1, Number(query.get('page') ?? '1') || 1);
  const perPage = Math.min(100, Math.max(1, Number(query.get('per_page') ?? '30') || 30));
  return { page, perPage };
}

function paginate<T>(items: T[], page: number, perPage: number): T[] {
  return items.slice((page - 1) * perPage, page * perPage);
}

export function createApp(store: Store, token: string): Server {
  const ctx: Ctx = { store, token, reqId: 0 };
  return createServer(async (req: IncomingMessage, res: ServerResponse) => {
    try {
      await handle(ctx, req, res);
    } catch (err) {
      if (!res.headersSent) sendError(ctx, res, 500, err instanceof Error ? err.message : 'internal error');
      else res.end();
    }
  });
}

async function handle(ctx: Ctx, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const method = req.method ?? 'GET';
  let url: URL;
  try {
    url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'entry'}`);
  } catch {
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  const path = url.pathname;
  const query = url.searchParams;
  const parts = splitPath(path);

  if (method === 'GET' && path === '/healthz') {
    sendJson(ctx, res, 200, { ok: true });
    return;
  }

  // Git smart HTTP: /{owner}/{repo}.git/...
  if (parts.length >= 2 && parts[1].endsWith('.git')) {
    const owner = parts[0];
    const repoName = parts[1].slice(0, -4);
    const rest = parts.slice(2);
    const body = await readBody(req);
    const gitReq: GitRequest = {
      owner,
      repoName,
      service: 'git-upload-pack',
      query,
      headers: {
        'git-protocol': req.headers['git-protocol'],
        'content-type': req.headers['content-type'],
      },
      body,
    };
    if (rest.length === 2 && rest[0] === 'info' && rest[1] === 'refs') {
      const service = query.get('service') ?? '';
      gitReq.service = service === 'git-receive-pack' ? 'git-receive-pack' : 'git-upload-pack';
      const out = handleInfoRefs(ctx.store, gitReq);
      res.writeHead(out.status, { 'content-type': out.contentType, 'content-length': out.body.length });
      res.end(out.body);
      return;
    }
    if (rest.length === 1 && rest[0] === 'git-upload-pack' && method === 'POST') {
      const out = handleUploadPack(ctx.store, gitReq);
      res.writeHead(out.status, { 'content-type': out.contentType, 'content-length': out.body.length });
      res.end(out.body);
      return;
    }
    if (rest.length === 1 && rest[0] === 'git-receive-pack' && method === 'POST') {
      const out = handleReceivePack(ctx.store, gitReq);
      res.writeHead(out.status, { 'content-type': out.contentType, 'content-length': out.body.length });
      res.end(out.body);
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  // GraphQL and MCP live outside /api/v3 but share the bearer token.
  if (path === '/api/graphql' && method === 'POST') {
    const raw = await readBody(req);
    await handleGraphQL(ctx.store, req, res, raw, isAuthed(ctx, req), () => nextReqId(ctx));
    return;
  }
  if (path === '/mcp' && method === 'POST') {
    const raw = await readBody(req);
    await handleMcp(ctx.store, req, res, raw, isAuthed(ctx, req));
    return;
  }

  if (parts[0] !== 'api' || parts[1] !== 'v3') {
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  const raw = await readBody(req);
  const parsed = parseJsonBody(raw);
  if (!parsed.ok) {
    sendError(ctx, res, 400, 'malformed JSON body');
    return;
  }
  if (!isAuthed(ctx, req)) {
    sendError(ctx, res, 401, 'Requires authentication');
    return;
  }
  const body = asRecord(parsed.value);
  const api = parts.slice(2);

  // API root and meta.
  if (api.length === 0) {
    if (method === 'GET') {
      sendJson(ctx, res, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  if (api.length === 1 && api[0] === 'meta' && method === 'GET') {
    sendJson(ctx, res, 200, { installed_version: '1.0.0', verifiable_password_authentication: true });
    return;
  }
  if (api.length === 1 && api[0] === 'user' && method === 'GET') {
    sendJson(ctx, res, 200, userJson());
    return;
  }

  // POST /user/repos and GET /user/repos.
  if (api.length === 2 && api[0] === 'user' && api[1] === 'repos') {
    if (method === 'POST') {
      const name = asString(body.name);
      if (name === '') {
        sendError(ctx, res, 422, 'name is required');
        return;
      }
      const repo = ctx.store.createRepo(name, asBool(body.private, false), asString(body.description), asString(body.default_branch, 'main'));
      if (repo === null) {
        sendError(ctx, res, 422, 'Repository already exists');
        return;
      }
      sendJson(ctx, res, 201, repoJson(ctx, repo));
      return;
    }
    if (method === 'GET') {
      const { page, perPage } = pageOf(query);
      const all = ctx.store.listRepos();
      const items = paginate(all, page, perPage);
      const extra: Record<string, string> = {};
      if (page * perPage < all.length) {
        const next = new URL(url.toString());
        next.searchParams.set('page', String(page + 1));
        next.searchParams.set('per_page', String(perPage));
        extra.link = `<${next.toString()}>; rel="next"`;
      }
      sendJson(ctx, res, 200, items.map((repo) => repoJson(ctx, repo)), extra);
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  // /repos/{owner}/{name}/...
  if (api.length >= 3 && api[0] === 'repos') {
    const owner = api[1];
    const name = api[2];
    const repo = owner === VIEWER_LOGIN ? ctx.store.getRepo(owner, name) : undefined;
    const tail = api.slice(3);
    if (repo === undefined) {
      sendError(ctx, res, 404, 'Not Found');
      return;
    }
    await handleRepo(ctx, req, res, repo, tail, method, body, query);
    return;
  }

  sendError(ctx, res, 404, 'Not Found');
}

async function handleRepo(
  ctx: Ctx,
  req: IncomingMessage,
  res: ServerResponse,
  repo: Repo,
  tail: string[],
  method: string,
  body: Record<string, unknown>,
  query: URLSearchParams,
): Promise<void> {
  // The repository itself.
  if (tail.length === 0) {
    if (method === 'GET') {
      sendJson(ctx, res, 200, repoJson(ctx, repo));
      return;
    }
    if (method === 'PATCH') {
      if (typeof body.description === 'string') repo.description = body.description;
      if (typeof body.name === 'string' && body.name !== '') {
        ctx.store.deleteRepo(repo.owner, repo.name);
        repo.name = body.name;
        ctx.store.repos.set(ctx.store.repoKey(repo.owner, repo.name), repo);
      }
      sendJson(ctx, res, 200, repoJson(ctx, repo));
      return;
    }
    if (method === 'DELETE') {
      ctx.store.deleteRepo(repo.owner, repo.name);
      sendEmpty(ctx, res, 204);
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  const head = tail[0];

  if (head === 'branches' && method === 'GET' && tail.length === 1) {
    const branches = [...repo.refs.keys()]
      .filter((ref) => ref.startsWith('refs/heads/'))
      .sort()
      .map((ref) => ({ name: ref.slice('refs/heads/'.length), commit: { sha: repo.refs.get(ref) as string }, protected: false }));
    sendJson(ctx, res, 200, branches);
    return;
  }

  if (head === 'commits' && method === 'GET' && tail.length === 1) {
    const shaParam = query.get('sha');
    const tip = shaParam !== null ? (refTip(repo, shaParam) ?? (repo.objects.has(shaParam) ? shaParam : undefined)) : refTip(repo, repo.defaultBranch);
    sendJson(ctx, res, 200, commitList(repo, tip));
    return;
  }

  if (head === 'git' && tail.length === 2 && tail[1] === 'refs' && method === 'POST') {
    const ref = asString(body.ref);
    const sha = asString(body.sha);
    if (!ref.startsWith('refs/') || !/^[0-9a-f]{40}$/.test(sha) || !repo.objects.has(sha)) {
      sendError(ctx, res, 422, 'invalid ref');
      return;
    }
    if (repo.refs.has(ref)) {
      sendError(ctx, res, 422, 'Reference already exists');
      return;
    }
    repo.refs.set(ref, sha);
    sendJson(ctx, res, 201, { ref, node_id: `R_${ref}`, url: `/api/v3/repos/${repo.owner}/${repo.name}/git/refs/${encodeURIComponent(ref)}`, object: { sha, type: 'commit', url: '' } });
    return;
  }

  if (head === 'labels' && tail.length === 1) {
    if (method === 'POST') {
      const name = asString(body.name);
      if (name === '') {
        sendError(ctx, res, 422, 'name is required');
        return;
      }
      let label = repo.labels.get(name);
      if (label === undefined) {
        label = { name, color: asString(body.color, 'ededed') };
        repo.labels.set(name, label);
      }
      sendJson(ctx, res, 201, labelJson(ctx, repo, label));
      return;
    }
    if (method === 'GET') {
      sendJson(ctx, res, 200, [...repo.labels.values()].map((label) => labelJson(ctx, repo, label)));
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  if (head === 'contents') {
    await handleContents(ctx, req, res, repo, tail.slice(1), method, body, query);
    return;
  }

  if (head === 'issues') {
    await handleIssues(ctx, req, res, repo, tail.slice(1), method, body, query);
    return;
  }

  if (head === 'pulls') {
    await handlePulls(ctx, req, res, repo, tail.slice(1), method, body, query);
    return;
  }

  sendError(ctx, res, 404, 'Not Found');
}

async function handleContents(
  ctx: Ctx,
  req: IncomingMessage,
  res: ServerResponse,
  repo: Repo,
  pathParts: string[],
  method: string,
  body: Record<string, unknown>,
  query: URLSearchParams,
): Promise<void> {
  const refParam = query.get('ref');
  const tip = refParam !== null
    ? (refTip(repo, refParam) ?? (repo.objects.has(refParam) ? refParam : undefined))
    : refTip(repo, repo.defaultBranch);
  const filePath = pathParts.map((part) => decodeURIComponent(part)).join('/');
  const files = treeFiles(repo, tip);

  if (method === 'GET') {
    const entry = files.get(filePath);
    if (entry !== undefined) {
      const blob = repo.objects.get(entry.oid);
      const bytes = blob === undefined ? Buffer.alloc(0) : blob.data;
      sendJson(ctx, res, 200, {
        type: 'file',
        encoding: 'base64',
        size: bytes.length,
        name: filePath.split('/').pop() ?? filePath,
        path: filePath,
        content: bytes.toString('base64'),
        sha: entry.oid,
      });
      return;
    }
    // Directory listing: entries directly under filePath (or the root when empty).
    const prefix = filePath === '' ? '' : `${filePath}/`;
    const seen = new Map<string, { type: string; name: string; path: string; sha: string }>();
    for (const [path, value] of files) {
      if (!path.startsWith(prefix)) continue;
      const rest = path.slice(prefix.length);
      const slash = rest.indexOf('/');
      if (slash < 0) {
        seen.set(rest, { type: 'file', name: rest, path, sha: value.oid });
      } else {
        const dir = rest.slice(0, slash);
        if (!seen.has(dir)) {
          // The directory's tree oid: rebuild the subtree's tree object id from the walk.
          const dirFiles = new Map<string, { oid: string; mode: string }>();
          for (const [p2, v2] of files) {
            if (p2.startsWith(`${prefix}${dir}/`)) dirFiles.set(p2.slice(prefix.length + dir.length + 1), v2);
          }
          const treeOid = buildTreeFromFiles(repo, dirFiles);
          seen.set(dir, { type: 'dir', name: dir, path: `${prefix}${dir}`, sha: treeOid });
        }
      }
    }
    if (seen.size === 0) {
      sendError(ctx, res, 404, 'Not Found');
      return;
    }
    sendJson(ctx, res, 200, [...seen.values()].sort((a, b) => (a.name < b.name ? -1 : 1)));
    return;
  }

  if (method === 'PUT') {
    const branch = asString(body.branch, repo.defaultBranch);
    const branchTip = refTip(repo, branch);
    if (branchTip === undefined && branch !== repo.defaultBranch) {
      sendError(ctx, res, 404, 'Not Found');
      return;
    }
    const content = asString(body.content);
    if (content === '') {
      sendError(ctx, res, 422, 'content is required');
      return;
    }
    const bytes = Buffer.from(content, 'base64');
    const current = treeFiles(repo, branchTip).get(filePath);
    const givenSha = asString(body.sha);
    if (current !== undefined) {
      if (givenSha !== '' && givenSha !== current.oid) {
        sendError(ctx, res, 409, 'sha does not match');
        return;
      }
    }
    const blobOid = storeObject(repo, 'blob', bytes);
    const existed = current !== undefined;
    const sha = commitFiles(ctx, repo, branch, asString(body.message, 'update'), (next) => {
      next.set(filePath, { oid: blobOid, mode: '100644' });
    });
    if (sha === null) {
      sendError(ctx, res, 422, 'could not commit');
      return;
    }
    const status = existed ? 200 : 201;
    sendJson(ctx, res, status, {
      content: {
        name: filePath.split('/').pop() ?? filePath,
        path: filePath,
        sha: blobOid,
        size: bytes.length,
        type: 'file',
        encoding: 'base64',
        content: bytes.toString('base64'),
      },
      commit: { sha, message: asString(body.message, 'update') },
    });
    return;
  }

  if (method === 'DELETE') {
    const branch = asString(body.branch, repo.defaultBranch);
    const branchTip = refTip(repo, branch);
    if (branchTip === undefined) {
      sendError(ctx, res, 404, 'Not Found');
      return;
    }
    const current = treeFiles(repo, branchTip).get(filePath);
    if (current === undefined) {
      sendError(ctx, res, 404, 'Not Found');
      return;
    }
    const sha = commitFiles(ctx, repo, branch, asString(body.message, 'delete'), (next) => {
      next.delete(filePath);
    });
    if (sha === null) {
      sendError(ctx, res, 422, 'could not commit');
      return;
    }
    sendJson(ctx, res, 200, { commit: { sha, message: asString(body.message, 'delete') } });
    return;
  }

  sendError(ctx, res, 404, 'Not Found');
}

async function handleIssues(
  ctx: Ctx,
  req: IncomingMessage,
  res: ServerResponse,
  repo: Repo,
  tail: string[],
  method: string,
  body: Record<string, unknown>,
  query: URLSearchParams,
): Promise<void> {
  if (tail.length === 0) {
    if (method === 'POST') {
      const labels = Array.isArray(body.labels) ? (body.labels as unknown[]).filter((x) => typeof x === 'string') as string[] : [];
      for (const name of labels) {
        if (!repo.labels.has(name)) {
          sendError(ctx, res, 422, `label ${name} does not exist`);
          return;
        }
      }
      const number = repo.nextNumber++;
      const seq = ctx.store.clock.next();
      const issue: Issue = {
        id: ctx.store.nextItem(),
        number,
        title: asString(body.title),
        body: asString(body.body),
        state: 'open',
        labels,
        seq,
        comments: [],
      };
      repo.issues.push(issue);
      sendJson(ctx, res, 201, issueJson(ctx, repo, issue));
      return;
    }
    if (method === 'GET') {
      const { page, perPage } = pageOf(query);
      const state = query.get('state') ?? 'open';
      const sort = query.get('sort') ?? 'created';
      const direction = query.get('direction') ?? 'desc';
      let list = [...repo.issues];
      if (state !== 'all') list = list.filter((issue) => issue.state === state);
      if (sort === 'created') {
        list.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
      }
      sendJson(ctx, res, 200, paginate(list, page, perPage).map((issue) => issueJson(ctx, repo, issue)));
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  const number = Number(tail[0]);
  const issue = repo.issues.find((entry) => entry.number === number);
  if (issue === undefined) {
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  if (tail.length === 1) {
    if (method === 'GET') {
      sendJson(ctx, res, 200, issueJson(ctx, repo, issue));
      return;
    }
    if (method === 'PATCH') {
      if (typeof body.state === 'string' && (body.state === 'open' || body.state === 'closed')) issue.state = body.state;
      if (typeof body.title === 'string') issue.title = body.title;
      if (typeof body.body === 'string') issue.body = body.body;
      sendJson(ctx, res, 200, issueJson(ctx, repo, issue));
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  if (tail.length === 2 && tail[1] === 'comments') {
    if (method === 'POST') {
      const comment = { id: ctx.store.nextItem(), body: asString(body.body), seq: ctx.store.clock.next() };
      issue.comments.push(comment);
      sendJson(ctx, res, 201, commentJson(ctx, repo, issue, comment));
      return;
    }
    if (method === 'GET') {
      sendJson(ctx, res, 200, issue.comments.map((comment) => commentJson(ctx, repo, issue, comment)));
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  sendError(ctx, res, 404, 'Not Found');
}

async function handlePulls(
  ctx: Ctx,
  req: IncomingMessage,
  res: ServerResponse,
  repo: Repo,
  tail: string[],
  method: string,
  body: Record<string, unknown>,
  query: URLSearchParams,
): Promise<void> {
  if (tail.length === 0) {
    if (method === 'POST') {
      const head = asString(body.head);
      const base = asString(body.base, repo.defaultBranch);
      const headTip = refTip(repo, head);
      const baseTip = refTip(repo, base);
      if (headTip === undefined || baseTip === undefined) {
        sendError(ctx, res, 422, 'head or base branch does not exist');
        return;
      }
      const duplicate = repo.pulls.find((pull) => pull.state === 'open' && pull.head === head && pull.base === base);
      if (duplicate !== undefined) {
        sendError(ctx, res, 422, 'a pull request for this head already exists');
        return;
      }
      const number = repo.nextNumber++;
      const seq = ctx.store.clock.next();
      const pull: Pull = {
        id: ctx.store.nextItem(),
        number,
        title: asString(body.title),
        body: asString(body.body),
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
      computePullFiles(ctx.store, repo, pull);
      repo.pulls.push(pull);
      sendJson(ctx, res, 201, pullJson(ctx, repo, pull));
      return;
    }
    if (method === 'GET') {
      const { page, perPage } = pageOf(query);
      const state = query.get('state') ?? 'open';
      let list = [...repo.pulls];
      if (state !== 'all') list = list.filter((pull) => pull.state === state);
      list.sort((a, b) => b.seq - a.seq);
      sendJson(ctx, res, 200, paginate(list, page, perPage).map((pull) => pullJson(ctx, repo, pull)));
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }

  const number = Number(tail[0]);
  const pull = repo.pulls.find((entry) => entry.number === number);
  if (pull === undefined) {
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  if (tail.length === 1) {
    if (method === 'GET') {
      sendJson(ctx, res, 200, pullJson(ctx, repo, pull));
      return;
    }
    sendError(ctx, res, 404, 'Not Found');
    return;
  }
  if (tail.length === 2 && tail[1] === 'files' && method === 'GET') {
    sendJson(ctx, res, 200, pull.files.map((file) => ({
      sha: pull.headSha,
      filename: file.filename,
      status: file.status,
      additions: file.additions,
      deletions: file.deletions,
      changes: file.changes,
      blob_url: '',
      raw_url: '',
      contents_url: '',
      patch: '',
    })));
    return;
  }
  if (tail.length === 2 && tail[1] === 'merge' && method === 'PUT') {
    if (pull.state !== 'open') {
      sendError(ctx, res, 405, 'pull request is not open');
      return;
    }
    const result = squashMerge(ctx.store, repo, pull);
    if ('conflict' in result) {
      sendError(ctx, res, 405, 'merge conflict');
      return;
    }
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = ctx.store.clock.iso(ctx.store.clock.next());
    pull.mergeSha = result.sha;
    sendJson(ctx, res, 200, { sha: result.sha, merged: true, message: 'Pull request successfully merged' });
    return;
  }
  sendError(ctx, res, 404, 'Not Found');
}
