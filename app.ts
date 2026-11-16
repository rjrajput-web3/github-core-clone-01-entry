// GitForge HTTP server: smart-HTTP git, REST v3, GraphQL v4 and MCP on one
// Node http server, zero dependencies, in-memory Maps. Hot paths (single
// issue GET, single issue POST) are kept allocation-light: cached JSON
// strings, manual URL parsing, shared header objects.

import http from 'node:http';
import {
  VIEWER_LOGIN,
  VIEWER_ID,
  VIEWER_NAME,
  VIEWER_EMAIL,
  DEFAULT_BRANCH,
  FORGE_VERSION,
  gid,
  hashObject,
  type Issue,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';
import { handleInfoRefs, handleUploadPack, handleReceivePack } from './git.ts';
import { executeGraphQL } from './graphql.ts';
import { handleMcp } from './mcp.ts';

let requestCounter = 0;

function readBody(req: http.IncomingMessage, limit = 64 * 1024 * 1024): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > limit) {
        reject(new Error('body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function parseIdent(line: string): { name: string; email: string; date: string } {
  const m = /^(.*) <(.*)> (\d+) ([+-]\d{4})$/.exec(line);
  if (!m) return { name: line, email: '', date: new Date().toISOString() };
  const ts = Number(m[3]);
  return { name: m[1]!, email: m[2]!, date: new Date(ts * 1000).toISOString() };
}

function stripTrailingNewline(s: string): string {
  return s.endsWith('\n') ? s.slice(0, -1) : s;
}

function queryParam(query: string, key: string): string | null {
  if (query === '') return null;
  const parts = query.split('&');
  for (const part of parts) {
    const eq = part.indexOf('=');
    const k = eq >= 0 ? part.slice(0, eq) : part;
    if (k === key) return eq >= 0 ? decodeURIComponent(part.slice(eq + 1).replace(/\+/g, ' ')) : '';
  }
  return null;
}

function paginate<T>(items: T[], perPage: number, page: number): { page: T[]; hasNext: boolean; totalPages: number } {
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const start = (page - 1) * perPage;
  return { page: items.slice(start, start + perPage), hasNext: page < totalPages, totalPages };
}

function repoJson(repo: Repo): Record<string, unknown> {
  let openIssues = 0;
  for (const i of repo.issues.values()) if (i.state === 'open') openIssues += 1;
  return {
    id: repo.id,
    node_id: gid('R', repo.id),
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: repo.owner === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.createdAt,
    html_url: `/${repo.owner}/${repo.name}`,
    clone_url: `/${repo.owner}/${repo.name}.git`,
    git_url: `/${repo.owner}/${repo.name}.git`,
    ssh_url: `git@gitforge.invalid:${repo.owner}/${repo.name}.git`,
    homepage: null,
    language: null,
    topics: [],
    visibility: repo.private ? 'private' : 'public',
    fork: false,
    watchers_count: 0,
    stargazers_count: 0,
    forks_count: 0,
    open_issues_count: openIssues,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    has_pages: false,
    archived: false,
    disabled: false,
  };
}

function issueObject(issue: Issue, repo: Repo): Record<string, unknown> {
  return {
    id: issue.id,
    node_id: gid('I', repo.id, issue.number),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name })),
    user: { login: issue.user, id: issue.user === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    comments: issue.comments.length,
    html_url: `/${repo.owner}/${repo.name}/issues/${issue.number}`,
    comments_url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}/comments`,
  };
}

function issueJsonCached(issue: Issue, repo: Repo): string {
  if (issue.json === null) issue.json = JSON.stringify(issueObject(issue, repo));
  return issue.json;
}

function prObject(pr: Pull, repo: Repo): Record<string, unknown> {
  return {
    id: pr.id,
    node_id: gid('P', repo.id, pr.number),
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.merged ? 'closed' : pr.state,
    merged: pr.merged,
    merged_at: pr.mergedAt,
    merge_commit_sha: pr.mergeCommitSha,
    head: { ref: pr.headRef, sha: repo.refs.get(`refs/heads/${pr.headRef}`) ?? pr.headSha, label: `${repo.owner}:${pr.headRef}` },
    base: { ref: pr.baseRef, sha: repo.refs.get(`refs/heads/${pr.baseRef}`) ?? pr.baseSha, label: `${repo.owner}:${pr.baseRef}` },
    user: { login: pr.user, id: pr.user === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
    created_at: pr.createdAt,
    updated_at: pr.updatedAt,
    html_url: `/${repo.owner}/${repo.name}/pull/${pr.number}`,
  };
}

function sendJson(res: http.ServerResponse, status: number, body: string, requestId: string, extra?: Record<string, string>): void {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-GitHub-Request-Id': requestId,
    ...extra,
  };
  res.writeHead(status, headers);
  res.end(body);
}

function sendError(res: http.ServerResponse, status: number, message: string, requestId: string): void {
  sendJson(res, status, JSON.stringify({ message }), requestId);
}

function sendGit(res: http.ServerResponse, status: number, contentType: string, body: Buffer, requestId: string): void {
  res.writeHead(status, {
    'Content-Type': contentType,
    'X-GitHub-Request-Id': requestId,
    'Content-Length': String(body.length),
  });
  res.end(body);
}

function resolveTip(store: Store, repo: Repo, ref: string | null | undefined): string | undefined {
  if (ref === null || ref === undefined || ref === '') return store.defaultTip(repo);
  const byBranch = repo.refs.get(`refs/heads/${ref}`);
  if (byBranch !== undefined) return byBranch;
  if (/^[0-9a-f]{40}$/.test(ref)) {
    const obj = store.getObject(ref);
    if (obj) return store.peelToCommit(ref);
  }
  return undefined;
}

function countLines(buf: Buffer): number {
  if (buf.length === 0) return 0;
  let n = 0;
  for (let i = 0; i < buf.length; i += 1) if (buf[i] === 10) n += 1;
  if (buf[buf.length - 1] !== 10) n += 1;
  return n;
}

export function createServer(store: Store): http.Server {
  const server = http.createServer((req, res) => {
    void handleRequest(store, req, res).catch((err) => {
      if (!res.headersSent) {
        sendError(res, 500, err instanceof Error ? err.message : 'Internal Server Error', `gf-${++requestCounter}`);
      } else {
        res.end();
      }
    });
  });
  server.on('connection', (socket) => {
    socket.setNoDelay(true);
  });
  server.keepAliveTimeout = 30000;
  server.headersTimeout = 60000;
  return server;
}

async function handleRequest(store: Store, req: http.IncomingMessage, res: http.ServerResponse): Promise<void> {
  const requestId = `gf-${++requestCounter}`;
  const method = req.method ?? 'GET';
  const rawUrl = req.url ?? '/';
  const q = rawUrl.indexOf('?');
  const path = q >= 0 ? rawUrl.slice(0, q) : rawUrl;
  const query = q >= 0 ? rawUrl.slice(q + 1) : '';
  const host = req.headers.host ?? '127.0.0.1';
  const origin = `http://${host}`;

  if (method === 'GET' && path === '/healthz') {
    sendJson(res, 200, '{"ok":true}', requestId);
    return;
  }

  // ---- git smart HTTP (no auth) ----
  const gitIdx = path.lastIndexOf('.git/');
  if (gitIdx > 0) {
    const ownerEnd = path.indexOf('/', 1);
    const owner = decodeURIComponent(path.slice(1, ownerEnd));
    const repoName = decodeURIComponent(path.slice(ownerEnd + 1, gitIdx));
    const rest = path.slice(gitIdx + 5);
    if (rest === 'info/refs') {
      const service = queryParam(query, 'service') ?? '';
      const wantV2 = req.headers['git-protocol'] === 'version=2';
      const out = handleInfoRefs(store, owner, repoName, service, wantV2);
      sendGit(res, out.status, out.contentType, out.body, requestId);
      return;
    }
    if (rest === 'git-upload-pack' && method === 'POST') {
      const body = await readBody(req);
      const out = handleUploadPack(store, owner, repoName, body, req.headers['git-protocol'] === 'version=2');
      sendGit(res, out.status, out.contentType, out.body, requestId);
      return;
    }
    if (rest === 'git-receive-pack' && method === 'POST') {
      const body = await readBody(req);
      const out = handleReceivePack(store, owner, repoName, body);
      sendGit(res, out.status, out.contentType, out.body, requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- auth (any non-empty Bearer token is the viewer) ----
  const auth = req.headers.authorization;
  const authed = typeof auth === 'string' && auth.startsWith('Bearer ') && auth.slice(7).trim() !== '';

  // ---- MCP ----
  if (path === '/mcp') {
    if (method !== 'POST') {
      sendError(res, 405, 'Method Not Allowed', requestId);
      return;
    }
    if (!authed) {
      sendJson(res, 401, JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32001, message: 'Unauthorized' } }), requestId);
      return;
    }
    const body = await readBody(req);
    const out = handleMcp(store, body);
    if (out.status === 202) {
      res.writeHead(202, { 'X-GitHub-Request-Id': requestId });
      res.end();
      return;
    }
    sendJson(res, out.status, out.body, requestId);
    return;
  }

  // ---- GraphQL ----
  if (path === '/api/graphql') {
    if (method !== 'POST') {
      sendError(res, 405, 'Method Not Allowed', requestId);
      return;
    }
    if (!authed) {
      sendError(res, 401, 'Requires authentication', requestId);
      return;
    }
    const body = await readBody(req);
    let parsed: unknown;
    try {
      parsed = JSON.parse(body.toString('utf8'));
    } catch {
      sendError(res, 400, 'Invalid JSON', requestId);
      return;
    }
    const out = executeGraphQL(store, parsed);
    sendJson(res, out.status, JSON.stringify(out.json), requestId);
    return;
  }

  // ---- REST v3 ----
  if (path === '/api/v3' || path === '/api/v3/') {
    if (method !== 'GET') {
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    sendJson(
      res,
      200,
      JSON.stringify({
        current_user_url: `${origin}/api/v3/user`,
        repository_url: `${origin}/api/v3/repos/{owner}/{repo}`,
        user_url: `${origin}/api/v3/users/{username}`,
        rate_limit_url: `${origin}/api/v3/rate_limit`,
        meta_url: `${origin}/api/v3/meta`,
      }),
      requestId,
      { 'X-OAuth-Scopes': 'repo, read:org' },
    );
    return;
  }
  if (path === '/api/v3/meta' && method === 'GET') {
    sendJson(res, 200, JSON.stringify({ installed_version: FORGE_VERSION, verifiable_password_authentication: true, github_enterprise: false, packages: [] }), requestId);
    return;
  }
  if (!path.startsWith('/api/v3/')) {
    sendError(res, 404, 'Not Found', requestId);
    return;
  }
  if (!authed) {
    sendError(res, 401, 'Requires authentication', requestId);
    return;
  }

  // Fast path: GET /api/v3/repos/{owner}/{repo}/issues/{n} is the read-workload
  // hot route (2,000 sequential requests on one connection). Parse it with
  // indexOf instead of the generic split/filter and serve the cached JSON
  // string directly. Any doubt falls through to the generic router below.
  if (method === 'GET' && path.startsWith('/api/v3/repos/')) {
    const rest = path.slice('/api/v3/repos/'.length);
    const s1 = rest.indexOf('/');
    const s2 = s1 >= 0 ? rest.indexOf('/', s1 + 1) : -1;
    if (s1 > 0 && s2 > s1) {
      const tail = rest.slice(s2 + 1);
      if (tail.startsWith('issues/')) {
        const numStr = tail.slice('issues/'.length);
        if (/^\d+$/.test(numStr)) {
          const owner = decodeURIComponent(rest.slice(0, s1));
          const repoName = decodeURIComponent(rest.slice(s1 + 1, s2));
          const repo = store.getRepo(owner, repoName);
          const issue = repo !== undefined && owner === VIEWER_LOGIN ? repo.issues.get(Number(numStr)) : undefined;
          if (issue !== undefined && repo !== undefined) {
            sendJson(res, 200, issueJsonCached(issue, repo), requestId);
            return;
          }
        }
      }
    }
  }

  const segs = path.split('/').filter((s) => s !== '');
  // segs: ['api','v3', ...]
  if (segs[2] === 'user') {
    await handleUserRoutes(store, req, res, method, segs, query, origin, requestId);
    return;
  }
  if (segs[2] === 'repos' && segs.length >= 5) {
    const owner = decodeURIComponent(segs[3]!);
    const repoName = decodeURIComponent(segs[4]!);
    await handleRepoRoutes(store, req, res, method, owner, repoName, segs.slice(5), query, requestId);
    return;
  }
  sendError(res, 404, 'Not Found', requestId);
}

async function handleUserRoutes(
  store: Store,
  req: http.IncomingMessage,
  res: http.ServerResponse,
  method: string,
  segs: string[],
  query: string,
  origin: string,
  requestId: string,
): Promise<void> {
  if (segs.length === 3 && method === 'GET' && segs[2] === 'user') {
    // /api/v3/user
    sendJson(res, 200, JSON.stringify({
      login: VIEWER_LOGIN,
      id: VIEWER_ID,
      node_id: gid('U', VIEWER_LOGIN),
      name: VIEWER_NAME,
      email: VIEWER_EMAIL,
      type: 'User',
      avatar_url: '',
      html_url: `/${VIEWER_LOGIN}`,
      created_at: new Date(0).toISOString(),
      updated_at: new Date().toISOString(),
    }), requestId);
    return;
  }
  if (segs.length === 4 && segs[3] === 'repos') {
    if (method === 'GET') {
      const perPage = Math.max(1, Math.min(100, Number(queryParam(query, 'per_page') ?? '30') || 30));
      const page = Math.max(1, Number(queryParam(query, 'page') ?? '1') || 1);
      const all = store.listViewerRepos();
      const { page: items, hasNext } = paginate(all, perPage, page);
      const extra: Record<string, string> = {};
      if (hasNext) {
        extra.Link = `<${origin}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}>; rel="next"`;
      }
      sendJson(res, 200, JSON.stringify(items.map((r) => repoJson(r))), requestId, extra);
      return;
    }
    if (method === 'POST') {
      const body = await readBody(req);
      let parsed: Record<string, unknown>;
      try {
        parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Invalid JSON', requestId);
        return;
      }
      const name = typeof parsed.name === 'string' ? parsed.name : '';
      if (name === '') {
        sendError(res, 422, 'name is required', requestId);
        return;
      }
      const repo = store.createRepo(
        VIEWER_LOGIN,
        name,
        typeof parsed.description === 'string' ? parsed.description : '',
        parsed.private === true,
      );
      if (!repo) {
        sendError(res, 422, 'Repository already exists', requestId);
        return;
      }
      sendJson(res, 201, JSON.stringify(repoJson(repo)), requestId);
      return;
    }
  }
  sendError(res, 404, 'Not Found', requestId);
}

async function handleRepoRoutes(
  store: Store,
  req: http.IncomingMessage,
  res: http.ServerResponse,
  method: string,
  owner: string,
  repoName: string,
  tail: string[],
  query: string,
  requestId: string,
): Promise<void> {
  const repo = store.getRepo(owner, repoName);
  const needRepo = (): Repo | null => {
    if (!repo || owner !== VIEWER_LOGIN) return null;
    return repo;
  };

  // /repos/{o}/{r} itself
  if (tail.length === 0) {
    if (method === 'GET') {
      const r = needRepo();
      if (!r) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      sendJson(res, 200, JSON.stringify(repoJson(r)), requestId);
      return;
    }
    if (method === 'PATCH') {
      const r = needRepo();
      if (!r) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const body = await readBody(req);
      let parsed: Record<string, unknown> = {};
      try {
        parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Invalid JSON', requestId);
        return;
      }
      if (typeof parsed.description === 'string') r.description = parsed.description;
      if (parsed.private === true || parsed.private === false) r.private = parsed.private;
      sendJson(res, 200, JSON.stringify(repoJson(r)), requestId);
      return;
    }
    if (method === 'DELETE') {
      const r = needRepo();
      if (!r) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      store.deleteRepo(owner, repoName);
      res.writeHead(204, { 'X-GitHub-Request-Id': requestId, 'Content-Type': 'application/json' });
      res.end();
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  const r = needRepo();
  if (!r) {
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- git refs ----
  if (tail[0] === 'git' && tail[1] === 'refs') {
    if (method === 'POST' && tail.length === 2) {
      const body = await readBody(req);
      let parsed: Record<string, unknown>;
      try {
        parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Invalid JSON', requestId);
        return;
      }
      const ref = typeof parsed.ref === 'string' ? parsed.ref : '';
      const sha = typeof parsed.sha === 'string' ? parsed.sha : '';
      if (!ref.startsWith('refs/') || sha === '' || store.getObject(sha) === undefined) {
        sendError(res, 422, 'Invalid ref or sha', requestId);
        return;
      }
      if (r.refs.has(ref)) {
        sendError(res, 422, 'Reference already exists', requestId);
        return;
      }
      store.setRef(r, ref, sha);
      sendJson(res, 201, JSON.stringify({ ref, object: { sha }, url: `/api/v3/repos/${owner}/${repoName}/git/refs/${ref.slice(5)}` }), requestId);
      return;
    }
    if (method === 'GET' && tail.length >= 3) {
      const ref = `refs/${tail.slice(2).map((s) => decodeURIComponent(s)).join('/')}`;
      const oid = r.refs.get(ref);
      if (oid === undefined) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      sendJson(res, 200, JSON.stringify({ ref, object: { sha: oid } }), requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- branches ----
  if (tail[0] === 'branches') {
    if (method === 'GET' && tail.length === 1) {
      const list: Array<Record<string, unknown>> = [];
      const names = [...r.refs.keys()].filter((k) => k.startsWith('refs/heads/')).sort();
      for (const ref of names) {
        const name = ref.slice('refs/heads/'.length);
        list.push({ name, commit: { sha: r.refs.get(ref) }, protected: false });
      }
      sendJson(res, 200, JSON.stringify(list), requestId);
      return;
    }
    if (method === 'GET' && tail.length >= 2) {
      const branch = tail.slice(1).map((s) => decodeURIComponent(s)).join('/');
      const oid = r.refs.get(`refs/heads/${branch}`);
      if (oid === undefined) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      sendJson(res, 200, JSON.stringify({ name: branch, commit: { sha: oid }, protected: false }), requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- commits ----
  if (tail[0] === 'commits') {
    if (method === 'GET' && tail.length === 1) {
      const shaParam = queryParam(query, 'sha');
      const tip = resolveTip(store, r, shaParam);
      if (tip === undefined) {
        if (shaParam !== null && shaParam !== '') {
          sendError(res, 404, 'No commit found for SHA', requestId);
          return;
        }
        sendJson(res, 200, '[]', requestId);
        return;
      }
      const list = store.listCommits(tip, 100);
      const out = list.map(({ sha, commit }) => {
        const author = parseIdent(commit.author);
        const committer = parseIdent(commit.committer);
        return {
          sha,
          commit: {
            message: stripTrailingNewline(commit.message),
            author: { name: author.name, email: author.email, date: author.date },
            committer: { name: committer.name, email: committer.email, date: committer.date },
            tree: { sha: commit.tree },
          },
          author: { login: VIEWER_LOGIN },
          committer: { login: VIEWER_LOGIN },
          parents: commit.parents.map((p) => ({ sha: p })),
        };
      });
      sendJson(res, 200, JSON.stringify(out), requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- contents ----
  if (tail[0] === 'contents') {
    const filePath = tail.slice(1).map((s) => decodeURIComponent(s)).join('/');
    if (method === 'GET') {
      const tip = resolveTip(store, r, queryParam(query, 'ref'));
      if (tip === undefined) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const commitObj = store.getObject(tip);
      if (!commitObj?.commit) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const flat = store.flattenTree(commitObj.commit.tree);
      if (filePath === '') {
        const children = new Map<string, { type: string; oid: string }>();
        for (const [p, entry] of flat) {
          const slash = p.indexOf('/');
          const first = slash >= 0 ? p.slice(0, slash) : p;
          if (!children.has(first)) {
            children.set(first, { type: slash >= 0 ? 'dir' : 'file', oid: slash >= 0 ? '' : entry.oid });
          }
        }
        const out = [...children.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)).map(([name, v]) => ({
          type: v.type,
          name,
          path: name,
          sha: v.oid,
        }));
        sendJson(res, 200, JSON.stringify(out), requestId);
        return;
      }
      const entry = flat.get(filePath);
      if (entry !== undefined) {
        const blob = store.getObject(entry.oid);
        const data = blob?.data ?? Buffer.alloc(0);
        sendJson(res, 200, JSON.stringify({
          type: 'file',
          encoding: 'base64',
          size: data.length,
          name: filePath.split('/').pop() ?? filePath,
          path: filePath,
          content: data.toString('base64'),
          sha: entry.oid,
        }), requestId);
        return;
      }
      const prefix = `${filePath}/`;
      const children = new Map<string, { type: string; oid: string }>();
      for (const [p, e] of flat) {
        if (!p.startsWith(prefix)) continue;
        const rest = p.slice(prefix.length);
        const slash = rest.indexOf('/');
        const first = slash >= 0 ? rest.slice(0, slash) : rest;
        if (!children.has(first)) {
          children.set(first, { type: slash >= 0 ? 'dir' : 'file', oid: slash >= 0 ? '' : e.oid });
        }
      }
      if (children.size === 0) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const out = [...children.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)).map(([name, v]) => ({
        type: v.type,
        name,
        path: `${filePath}/${name}`,
        sha: v.oid,
      }));
      sendJson(res, 200, JSON.stringify(out), requestId);
      return;
    }
    if (method === 'PUT') {
      const body = await readBody(req);
      let parsed: Record<string, unknown>;
      try {
        parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Invalid JSON', requestId);
        return;
      }
      const message = typeof parsed.message === 'string' ? parsed.message : 'Update file';
      const contentB64 = typeof parsed.content === 'string' ? parsed.content : '';
      const branch = typeof parsed.branch === 'string' ? parsed.branch : r.defaultBranch;
      const givenSha = typeof parsed.sha === 'string' ? parsed.sha : null;
      const content = Buffer.from(contentB64, 'base64');
      let tip = r.refs.get(`refs/heads/${branch}`);
      if (tip === undefined && branch !== r.defaultBranch) {
        tip = store.defaultTip(r);
      }
      const flat = new Map<string, { oid: string; mode: string }>();
      let parents: string[] = [];
      if (tip !== undefined) {
        const commitObj = store.getObject(tip);
        if (commitObj?.commit) {
          parents = [tip];
          for (const [p, e] of store.flattenTree(commitObj.commit.tree)) flat.set(p, { oid: e.oid, mode: e.mode });
        }
      }
      const existing = flat.get(filePath);
      if (givenSha !== null && existing !== undefined && existing.oid !== givenSha) {
        sendError(res, 409, 'sha does not match', requestId);
        return;
      }
      if (givenSha !== null && existing === undefined) {
        sendError(res, 422, 'sha does not match any existing file', requestId);
        return;
      }
      const isCreate = existing === undefined;
      const blobOid = hashObject('blob', content);
      store.putObject('blob', content);
      flat.set(filePath, { oid: blobOid, mode: '100644' });
      const treeOid = store.buildTreeFromFlat(flat);
      const commitOid = store.createCommit(treeOid, parents, message);
      store.setRef(r, `refs/heads/${branch}`, commitOid);
      sendJson(res, isCreate ? 201 : 200, JSON.stringify({
        content: {
          name: filePath.split('/').pop() ?? filePath,
          path: filePath,
          sha: blobOid,
          size: content.length,
          encoding: 'base64',
          content: contentB64,
          type: 'file',
        },
        commit: { sha: commitOid, message },
      }), requestId);
      return;
    }
    if (method === 'DELETE') {
      const body = await readBody(req);
      let parsed: Record<string, unknown> = {};
      try {
        parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Invalid JSON', requestId);
        return;
      }
      const message = typeof parsed.message === 'string' ? parsed.message : 'Delete file';
      const givenSha = typeof parsed.sha === 'string' ? parsed.sha : null;
      const branch = typeof parsed.branch === 'string' ? parsed.branch : r.defaultBranch;
      const tip = r.refs.get(`refs/heads/${branch}`);
      if (tip === undefined) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const commitObj = store.getObject(tip);
      if (!commitObj?.commit) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const flat = new Map<string, { oid: string; mode: string }>();
      for (const [p, e] of store.flattenTree(commitObj.commit.tree)) flat.set(p, { oid: e.oid, mode: e.mode });
      const existing = flat.get(filePath);
      if (existing === undefined) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      if (givenSha !== null && existing.oid !== givenSha) {
        sendError(res, 409, 'sha does not match', requestId);
        return;
      }
      flat.delete(filePath);
      const treeOid = store.buildTreeFromFlat(flat);
      const commitOid = store.createCommit(treeOid, [tip], message);
      store.setRef(r, `refs/heads/${branch}`, commitOid);
      sendJson(res, 200, JSON.stringify({ commit: { sha: commitOid, message } }), requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- labels ----
  if (tail[0] === 'labels') {
    if (method === 'POST' && tail.length === 1) {
      const body = await readBody(req);
      let parsed: Record<string, unknown>;
      try {
        parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
      } catch {
        sendError(res, 400, 'Invalid JSON', requestId);
        return;
      }
      const name = typeof parsed.name === 'string' ? parsed.name : '';
      const color = typeof parsed.color === 'string' ? parsed.color : '000000';
      if (name === '') {
        sendError(res, 422, 'name is required', requestId);
        return;
      }
      r.labels.set(name, { name, color });
      sendJson(res, 201, JSON.stringify({ name, color }), requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- issues ----
  if (tail[0] === 'issues') {
    if (tail.length === 1) {
      if (method === 'GET') {
        const state = queryParam(query, 'state') ?? 'open';
        const sort = queryParam(query, 'sort') ?? 'created';
        const direction = queryParam(query, 'direction') ?? 'desc';
        const perPage = Math.max(1, Math.min(100, Number(queryParam(query, 'per_page') ?? '30') || 30));
        const page = Math.max(1, Number(queryParam(query, 'page') ?? '1') || 1);
        let list = [...r.issues.values()];
        if (state !== 'all') list = list.filter((i) => i.state === state);
        if (sort === 'created') list.sort((a, b) => (direction === 'asc' ? a.seq - b.seq : b.seq - a.seq));
        const { page: items } = paginate(list, perPage, page);
        sendJson(res, 200, JSON.stringify(items.map((i) => issueObject(i, r))), requestId);
        return;
      }
      if (method === 'POST') {
        const body = await readBody(req);
        let parsed: Record<string, unknown>;
        try {
          parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
        } catch {
          sendError(res, 400, 'Invalid JSON', requestId);
          return;
        }
        const title = typeof parsed.title === 'string' ? parsed.title : '';
        const issueBody = typeof parsed.body === 'string' ? parsed.body : '';
        const rawLabels = Array.isArray(parsed.labels) ? parsed.labels : [];
        const labels: string[] = [];
        for (const l of rawLabels) {
          if (typeof l === 'string') labels.push(l);
          else if (l !== null && typeof l === 'object' && typeof (l as Record<string, unknown>).name === 'string') labels.push((l as Record<string, unknown>).name as string);
        }
        const made = store.createIssue(r, title, issueBody, labels);
        if ('error' in made) {
          sendError(res, 422, made.error, requestId);
          return;
        }
        sendJson(res, 201, issueJsonCached(made, r), requestId);
        return;
      }
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    const num = Number(tail[1]);
    if (!Number.isSafeInteger(num) || num < 1) {
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    const issue = r.issues.get(num);
    if (tail.length === 2) {
      if (method === 'GET') {
        // Hot path: cached JSON string, zero re-serialization.
        if (!issue) {
          sendError(res, 404, 'Not Found', requestId);
          return;
        }
        sendJson(res, 200, issueJsonCached(issue, r), requestId);
        return;
      }
      if (method === 'PATCH') {
        if (!issue) {
          sendError(res, 404, 'Not Found', requestId);
          return;
        }
        const body = await readBody(req);
        let parsed: Record<string, unknown> = {};
        try {
          parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
        } catch {
          sendError(res, 400, 'Invalid JSON', requestId);
          return;
        }
        if (parsed.state === 'open' || parsed.state === 'closed') issue.state = parsed.state;
        if (typeof parsed.title === 'string') issue.title = parsed.title;
        if (typeof parsed.body === 'string') issue.body = parsed.body;
        issue.updatedAt = new Date().toISOString();
        issue.json = null;
        sendJson(res, 200, issueJsonCached(issue, r), requestId);
        return;
      }
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    if (tail.length === 3 && tail[2] === 'comments') {
      if (!issue) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      if (method === 'GET') {
        sendJson(res, 200, JSON.stringify(issue.comments.map((c) => ({
          id: c.id,
          body: c.body,
          user: { login: c.user },
          created_at: c.createdAt,
          updated_at: c.createdAt,
        }))), requestId);
        return;
      }
      if (method === 'POST') {
        const body = await readBody(req);
        let parsed: Record<string, unknown>;
        try {
          parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
        } catch {
          sendError(res, 400, 'Invalid JSON', requestId);
          return;
        }
        const commentBody = typeof parsed.body === 'string' ? parsed.body : '';
        const comment = {
          id: store.nextItemId++,
          body: commentBody,
          user: VIEWER_LOGIN,
          createdAt: new Date().toISOString(),
          seq: store.nextSeq++,
        };
        issue.comments.push(comment);
        issue.json = null;
        sendJson(res, 201, JSON.stringify({
          id: comment.id,
          body: comment.body,
          user: { login: comment.user },
          created_at: comment.createdAt,
          updated_at: comment.createdAt,
        }), requestId);
        return;
      }
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  // ---- pulls ----
  if (tail[0] === 'pulls') {
    if (tail.length === 1) {
      if (method === 'GET') {
        const state = queryParam(query, 'state') ?? 'open';
        const perPage = Math.max(1, Math.min(100, Number(queryParam(query, 'per_page') ?? '30') || 30));
        const page = Math.max(1, Number(queryParam(query, 'page') ?? '1') || 1);
        let list = [...r.pulls.values()];
        if (state !== 'all') list = list.filter((p) => p.state === state && !p.merged);
        list.sort((a, b) => a.number - b.number);
        const { page: items } = paginate(list, perPage, page);
        sendJson(res, 200, JSON.stringify(items.map((p) => prObject(p, r))), requestId);
        return;
      }
      if (method === 'POST') {
        const body = await readBody(req);
        let parsed: Record<string, unknown>;
        try {
          parsed = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
        } catch {
          sendError(res, 400, 'Invalid JSON', requestId);
          return;
        }
        const title = typeof parsed.title === 'string' ? parsed.title : '';
        const prBody = typeof parsed.body === 'string' ? parsed.body : '';
        const head = typeof parsed.head === 'string' ? parsed.head : '';
        const base = typeof parsed.base === 'string' ? parsed.base : r.defaultBranch;
        const made = store.createPull(r, title, prBody, head, base);
        if ('error' in made) {
          sendError(res, 422, made.error, requestId);
          return;
        }
        sendJson(res, 201, JSON.stringify(prObject(made, r)), requestId);
        return;
      }
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    const num = Number(tail[1]);
    if (!Number.isSafeInteger(num) || num < 1) {
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    const pr = r.pulls.get(num);
    if (tail.length === 2) {
      if (method === 'GET') {
        if (!pr) {
          sendError(res, 404, 'Not Found', requestId);
          return;
        }
        sendJson(res, 200, JSON.stringify(prObject(pr, r)), requestId);
        return;
      }
      sendError(res, 404, 'Not Found', requestId);
      return;
    }
    if (tail.length === 3 && tail[2] === 'files' && method === 'GET') {
      if (!pr) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      const files = store.pullFiles(r, pr).map((f) => {
        let additions = 0;
        let deletions = 0;
        if (f.newOid !== null) {
          const obj = store.getObject(f.newOid);
          if (obj) additions = countLines(obj.data);
        }
        if (f.oldOid !== null) {
          const obj = store.getObject(f.oldOid);
          if (obj) deletions = countLines(obj.data);
        }
        if (f.status === 'removed') additions = 0;
        if (f.status === 'added') deletions = 0;
        if (f.status === 'modified' && additions === 0 && deletions === 0) additions = 1;
        return { filename: f.path, status: f.status, additions, deletions, changes: additions + deletions };
      });
      sendJson(res, 200, JSON.stringify(files), requestId);
      return;
    }
    if (tail.length === 3 && tail[2] === 'merge' && method === 'PUT') {
      if (!pr) {
        sendError(res, 404, 'Not Found', requestId);
        return;
      }
      await readBody(req).catch(() => Buffer.alloc(0));
      const merged = store.mergePull(r, pr);
      if (!merged.ok) {
        sendError(res, 405, merged.reason, requestId);
        return;
      }
      sendJson(res, 200, JSON.stringify({ merged: true, sha: merged.sha, message: `Merge pull request #${pr.number} from ${pr.headRef}` }), requestId);
      return;
    }
    sendError(res, 404, 'Not Found', requestId);
    return;
  }

  sendError(res, 404, 'Not Found', requestId);
}
