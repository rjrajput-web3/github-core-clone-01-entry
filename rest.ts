// rest.ts — the GitHub REST v3 surface: repositories, branches, commits, contents,
// labels, issues, comments and pull requests, all on the store in store.ts.

import {
  VIEWER,
  buildCommit,
  hashObject,
  isOid,
  parseCommit,
  type Issue,
  type Label,
  type Pull,
  type Repo,
  type Store,
} from './store.ts';

export interface RestResponse {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const ok = (body: unknown, status = 200, headers?: Record<string, string>): RestResponse => ({ status, body, headers });
const err = (status: number, message: string): RestResponse => ({ status, body: { message } });
const notFound = (what = 'Not Found'): RestResponse => err(404, what);

function b64decode(content: string): Buffer {
  return Buffer.from(content.replace(/[\r\n\t ]+/g, ''), 'base64');
}

// -------------------------------------------------------------------------------------------
// serializers
// -------------------------------------------------------------------------------------------

export function repoJson(store: Store, repo: Repo): Record<string, unknown> {
  return {
    id: repo.id,
    node_id: store.globalId('R', repo.owner, repo.name, repo.id),
    name: repo.name,
    full_name: repo.fullName,
    owner: { login: repo.owner, id: VIEWER.id, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: store.stamp(repo.seq),
    updated_at: store.stamp(repo.seq),
    html_url: `https://example.invalid/${repo.fullName}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    clone_url: `https://example.invalid/${repo.fullName}.git`,
    git_url: `git://example.invalid/${repo.fullName}.git`,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    forks_count: 0,
    stargazers_count: 0,
    watchers_count: 0,
    open_issues_count: [...repo.issues.values()].filter((i) => i.state === 'open').length,
  };
}

function labelJson(label: Label): Record<string, unknown> {
  return { name: label.name, color: label.color, description: label.description };
}

export function issueJson(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.dbId,
    node_id: store.globalId('I', repo.owner, repo.name, issue.number),
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const l = repo.labels.get(name);
      return l ? labelJson(l) : { name, color: 'ededed', description: '' };
    }),
    user: { login: VIEWER.login, id: VIEWER.id, type: 'User' },
    assignee: null,
    assignees: [],
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    html_url: `https://example.invalid/${repo.fullName}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function userRef(login: string): Record<string, unknown> {
  return { login, id: login === VIEWER.login ? VIEWER.id : 0, type: 'User' };
}

export function pullJson(store: Store, repo: Repo, pr: Pull, mergeable: boolean): Record<string, unknown> {
  return {
    id: pr.dbId,
    node_id: store.globalId('PR', repo.owner, repo.name, pr.number),
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.state,
    merged: pr.merged,
    merged_at: pr.mergedAt,
    merge_commit_sha: pr.mergeCommitSha,
    mergeable,
    mergeable_state: mergeable ? 'clean' : 'dirty',
    head: {
      ref: pr.headRef,
      sha: pr.headSha,
      label: `${repo.owner}:${pr.headRef}`,
      user: userRef(repo.owner),
    },
    base: {
      ref: pr.baseRef,
      sha: pr.baseSha,
      label: `${repo.owner}:${pr.baseRef}`,
      user: userRef(repo.owner),
    },
    user: userRef(repo.owner),
    created_at: pr.createdAt,
    updated_at: pr.updatedAt,
    html_url: `https://example.invalid/${repo.fullName}/pull/${pr.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pr.number}`,
  };
}

function commitJson(store: Store, repo: Repo, sha: string): Record<string, unknown> | null {
  const o = repo.getObject(sha);
  if (!o || o.type !== 'commit') return null;
  const c = parseCommit(o.data);
  const stamp = store.stamp(repo.seq);
  return {
    sha,
    node_id: store.globalId('C', repo.owner, repo.name, sha),
    commit: {
      // GitHub's REST surface returns the message without the trailing
      // newline the commit object itself stores.
      message: c.message.endsWith('\n') ? c.message.slice(0, -1) : c.message,
      author: { name: c.author.split(' <')[0] ?? '', email: VIEWER.email, date: stamp },
      committer: { name: c.committer.split(' <')[0] ?? '', email: VIEWER.email, date: stamp },
    },
    author: userRef(repo.owner),
    committer: userRef(repo.owner),
    parents: c.parents.map((p) => ({ sha: p })),
  };
}

// -------------------------------------------------------------------------------------------
// pagination
// -------------------------------------------------------------------------------------------

function pageParams(query: URLSearchParams): { perPage: number; page: number } {
  const perPageRaw = Number(query.get('per_page') ?? '30');
  const pageRaw = Number(query.get('page') ?? '1');
  return {
    perPage: Number.isFinite(perPageRaw) ? Math.min(100, Math.max(1, Math.trunc(perPageRaw))) : 30,
    page: Number.isFinite(pageRaw) ? Math.max(1, Math.trunc(pageRaw)) : 1,
  };
}

function paginate<T>(items: readonly T[], query: URLSearchParams, base: string): { items: T[]; link: string | null } {
  const { perPage, page } = pageParams(query);
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  if (totalPages <= 1) return { items: slice, link: null };
  const urlFor = (p: number): string => {
    const q = new URLSearchParams(query);
    q.set('per_page', String(perPage));
    q.set('page', String(p));
    return `${base}?${q.toString()}`;
  };
  const links: string[] = [];
  if (page < totalPages) links.push(`<${urlFor(page + 1)}>; rel="next"`);
  links.push(`<${urlFor(totalPages)}>; rel="last"`);
  if (page > 1) links.push(`<${urlFor(1)}>; rel="first"`);
  return { items: slice, link: links.join(', ') };
}

// -------------------------------------------------------------------------------------------
// contents helpers
// -------------------------------------------------------------------------------------------

function resolveRefTip(repo: Repo, ref?: string | null): string | undefined {
  if (ref === undefined || ref === null || ref === '') return repo.defaultTip();
  if (ref.startsWith('refs/heads/')) return repo.tip(ref);
  if (repo.refs.has(`refs/heads/${ref}`)) return repo.tip(`refs/heads/${ref}`);
  if (isOid(ref)) {
    const o = repo.getObject(ref);
    if (o && o.type === 'commit') return ref;
  }
  return undefined;
}

function commitFile(
  store: Store,
  repo: Repo,
  opts: { branch?: string; path: string; content: Buffer; message: string },
): { commitSha: string; contentSha: string } {
  const branch = opts.branch ?? repo.defaultBranch;
  let baseTip = repo.tip(`refs/heads/${branch}`);
  if (baseTip === undefined && branch !== repo.defaultBranch) {
    baseTip = repo.defaultTip();
  }
  const flat = baseTip !== undefined ? repo.filesOfCommit(baseTip) : new Map<string, { oid: string; mode: string }>();
  const blobOid = repo.putObject('blob', opts.content);
  flat.set(opts.path, { oid: blobOid, mode: '100644' });
  const treeOid = repo.treeFromFlat(flat);
  const seq = store.nextSequence();
  const commit = buildCommit({
    tree: treeOid,
    parents: baseTip !== undefined ? [baseTip] : [],
    author: `${VIEWER.name} <${VIEWER.email}>`,
    committer: `${VIEWER.name} <${VIEWER.email}>`,
    timestamp: 1700000000 + seq,
    message: opts.message.endsWith('\n') ? opts.message : `${opts.message}\n`,
  });
  const commitSha = repo.putObject('commit', commit);
  repo.setRef(`refs/heads/${branch}`, commitSha);
  return { commitSha, contentSha: blobOid };
}

function deleteFile(
  store: Store,
  repo: Repo,
  opts: { branch?: string; path: string; message: string },
): { commitSha: string } | null {
  const branch = opts.branch ?? repo.defaultBranch;
  const baseTip = repo.tip(`refs/heads/${branch}`);
  if (baseTip === undefined) return null;
  const flat = repo.filesOfCommit(baseTip);
  if (!flat.has(opts.path)) return null;
  flat.delete(opts.path);
  const treeOid = repo.treeFromFlat(flat);
  const seq = store.nextSequence();
  const commit = buildCommit({
    tree: treeOid,
    parents: [baseTip],
    author: `${VIEWER.name} <${VIEWER.email}>`,
    committer: `${VIEWER.name} <${VIEWER.email}>`,
    timestamp: 1700000000 + seq,
    message: opts.message.endsWith('\n') ? opts.message : `${opts.message}\n`,
  });
  const commitSha = repo.putObject('commit', commit);
  repo.setRef(`refs/heads/${branch}`, commitSha);
  return { commitSha };
}

// -------------------------------------------------------------------------------------------
// pull-request diff + mergeability
// -------------------------------------------------------------------------------------------

function lineCount(text: string): number {
  if (text === '') return 0;
  const parts = text.split('\n');
  if (parts.length > 0 && parts[parts.length - 1] === '') parts.pop();
  return parts.length;
}

function diffStats(oldText: string | null, newText: string | null): { additions: number; deletions: number; status: string } {
  if (oldText === null) {
    return { additions: lineCount(newText ?? ''), deletions: 0, status: 'added' };
  }
  if (newText === null) {
    return { additions: 0, deletions: lineCount(oldText), status: 'removed' };
  }
  if (oldText === newText) return { additions: 0, deletions: 0, status: 'modified' };
  const oldLines = oldText.split('\n');
  const newLines = newText.split('\n');
  const oldCounts = new Map<string, number>();
  for (const l of oldLines) oldCounts.set(l, (oldCounts.get(l) ?? 0) + 1);
  let additions = 0;
  for (const l of newLines) {
    const n = oldCounts.get(l) ?? 0;
    if (n > 0) oldCounts.set(l, n - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const n of oldCounts.values()) deletions += n;
  return { additions, deletions, status: 'modified' };
}

export function prFiles(repo: Repo, pr: Pull): Array<Record<string, unknown>> {
  const baseFiles = repo.filesOfCommit(pr.baseSha);
  const headFiles = repo.filesOfCommit(pr.headSha);
  const paths = new Set<string>([...baseFiles.keys(), ...headFiles.keys()]);
  const out: Array<Record<string, unknown>> = [];
  for (const path of [...paths].sort()) {
    const b = baseFiles.get(path);
    const h = headFiles.get(path);
    if (b && h && b.oid === h.oid) continue;
    const oldText = b ? repo.getObject(b.oid)?.data.toString('utf8') ?? null : null;
    const newText = h ? repo.getObject(h.oid)?.data.toString('utf8') ?? null : null;
    const stats = diffStats(oldText, newText);
    out.push({
      sha: (h ?? b)!.oid,
      filename: path,
      status: stats.status,
      additions: stats.additions,
      deletions: stats.deletions,
      changes: stats.additions + stats.deletions,
    });
  }
  return out;
}

export function prMergeable(repo: Repo, pr: Pull): boolean {
  if (pr.state !== 'open') return false;
  // Three-way check against the CURRENT base-branch tip, not the tip recorded
  // when the PR was created — after another PR merges, the base moves and a
  // same-file change on both sides becomes a conflict.
  const baseTip = repo.tip(`refs/heads/${pr.baseRef}`);
  if (baseTip === undefined) return false;
  const base = repo.mergeBase(pr.headSha, baseTip);
  if (base === null) return false;
  if (base === pr.headSha) return true; // head already inside base
  const baseFiles = repo.filesOfCommit(base);
  const headFiles = repo.filesOfCommit(pr.headSha);
  const tipFiles = repo.filesOfCommit(baseTip);
  const paths = new Set<string>([...baseFiles.keys(), ...headFiles.keys(), ...tipFiles.keys()]);
  for (const path of paths) {
    const o = baseFiles.get(path)?.oid;
    const h = headFiles.get(path)?.oid;
    const t = tipFiles.get(path)?.oid;
    if (h !== o && t !== o && h !== t) return false;
  }
  return true;
}

// -------------------------------------------------------------------------------------------
// router
// -------------------------------------------------------------------------------------------

type Body = Record<string, unknown>;

function bodyString(b: Body, key: string): string | undefined {
  const v = b[key];
  return typeof v === 'string' ? v : undefined;
}

function requireRepo(store: Store, owner: string, name: string): Repo | RestResponse {
  const repo = store.getRepo(owner, name);
  if (!repo) return notFound('Not Found');
  return repo;
}

/**
 * Handle a /api/v3 request.
 * @param segments path segments below /api/v3 (decoded)
 * @param base absolute origin, e.g. http://127.0.0.1:PORT, for pagination Link headers
 */
export function handleRest(
  store: Store,
  method: string,
  segments: string[],
  query: URLSearchParams,
  body: Body | null,
  base: string,
): RestResponse {
  const b: Body = body ?? {};

  // GET /api/v3/ — root
  if (segments.length === 0) {
    if (method !== 'GET') return err(405, 'Method Not Allowed');
    return ok(
      {
        current_user_url: '/api/v3/user',
        repository_url: '/api/v3/repos/{owner}/{repo}',
        user_url: '/api/v3/users/{username}',
        user_repositories_url: '/api/v3/user/repos',
        organization_url: '/api/v3/orgs/{org}',
        search_url: '/api/v3/search',
      },
      200,
      { 'X-OAuth-Scopes': 'repo, read:org' },
    );
  }

  if (segments[0] === 'meta') {
    if (method !== 'GET') return err(405, 'Method Not Allowed');
    return ok({ installed_version: '1.0.0', verifiable_password_authentication: true });
  }

  if (segments[0] === 'user' && segments.length === 1) {
    if (method !== 'GET') return err(405, 'Method Not Allowed');
    return ok({
      login: VIEWER.login,
      id: VIEWER.id,
      node_id: store.globalId('R', VIEWER.login, '', VIEWER.id),
      name: VIEWER.name,
      email: VIEWER.email,
      type: 'User',
    });
  }

  if (segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'GET') {
      const { items, link } = paginate(store.listRepos(VIEWER.login), query, `${base}/api/v3/user/repos`);
      return ok(items.map((r) => repoJson(store, r)), 200, link ? { Link: link } : undefined);
    }
    if (method === 'POST') {
      const name = bodyString(b, 'name');
      if (name === undefined || name === '' || name.includes('/')) return err(422, 'name is required');
      if (store.getRepo(VIEWER.login, name)) return err(422, 'Repository already exists');
      const repo = store.createRepo(
        VIEWER.login,
        name,
        bodyString(b, 'description') ?? '',
        b.private === true,
      );
      if (typeof b.default_branch === 'string' && b.default_branch !== '') repo.defaultBranch = b.default_branch;
      return ok(repoJson(store, repo), 201);
    }
    return err(405, 'Method Not Allowed');
  }

  if (segments[0] === 'repos' && segments.length >= 3) {
    const [owner, name] = [segments[1]!, segments[2]!];
    const rest = segments.slice(3);
    const repoOrErr = requireRepo(store, owner, name);
    if (repoOrErr instanceof Object && 'status' in repoOrErr) return repoOrErr as RestResponse;
    const repo = repoOrErr as Repo;
    const repoBase = `${base}/api/v3/repos/${owner}/${name}`;

    // /repos/{o}/{r}
    if (rest.length === 0) {
      if (method === 'GET') return ok(repoJson(store, repo));
      if (method === 'PATCH') {
        const desc = bodyString(b, 'description');
        if (desc !== undefined) repo.description = desc;
        if (b.private === true || b.private === false) repo.isPrivate = b.private;
        if (typeof b.default_branch === 'string' && b.default_branch !== '') repo.defaultBranch = b.default_branch;
        return ok(repoJson(store, repo));
      }
      if (method === 'DELETE') {
        store.deleteRepo(owner, name);
        return { status: 204, body: null };
      }
      return err(405, 'Method Not Allowed');
    }

    if (rest[0] === 'branches') {
      if (rest.length === 1) {
        if (method !== 'GET') return err(405, 'Method Not Allowed');
        const branches = sortedRefNames(repo)
          .filter((n) => n.startsWith('refs/heads/'))
          .map((n) => ({ name: n.slice('refs/heads/'.length), commit: { sha: repo.tip(n) } }));
        return ok(branches);
      }
      const branch = rest.slice(1).join('/');
      if (method !== 'GET') return err(405, 'Method Not Allowed');
      const sha = repo.tip(`refs/heads/${branch}`);
      if (sha === undefined) return notFound('Branch not found');
      return ok({ name: branch, commit: { sha }, protected: false });
    }

    if (rest[0] === 'commits') {
      if (method !== 'GET') return err(405, 'Method Not Allowed');
      const shaParam = query.get('sha');
      let tip = shaParam !== null ? resolveRefTip(repo, shaParam) : repo.defaultTip();
      if (tip === undefined) return ok([]);
      const commits = repo.reachableCommits([tip]);
      // Newest-first: sort by walk order from the tip.
      const ordered: string[] = [];
      const seen = new Set<string>();
      const stack = [tip];
      while (stack.length) {
        const cur = stack.pop()!;
        if (seen.has(cur) || !commits.has(cur)) continue;
        seen.add(cur);
        ordered.push(cur);
        const c = repo.commitOf(cur);
        if (c) for (const p of c.parents) stack.push(p);
      }
      const out = ordered.map((sha) => commitJson(store, repo, sha)).filter((x): x is Record<string, unknown> => x !== null);
      const { items, link } = paginate(out, query, `${repoBase}/commits`);
      return ok(items, 200, link ? { Link: link } : undefined);
    }

    if (rest[0] === 'git' && rest[1] === 'refs') {
      if (rest.length >= 4 && rest[2] === 'heads' && method === 'GET') {
        const branch = rest.slice(3).join('/');
        const sha = repo.tip(`refs/heads/${branch}`);
        if (sha === undefined) return notFound('Reference does not exist');
        return ok({ ref: `refs/heads/${branch}`, object: { sha, type: 'commit' } });
      }
      if (rest.length === 2 && method === 'POST') {
        const ref = bodyString(b, 'ref');
        const sha = bodyString(b, 'sha');
        if (ref === undefined || sha === undefined) return err(422, 'ref and sha are required');
        if (!ref.startsWith('refs/heads/')) return err(422, 'ref must be refs/heads/<branch>');
        const target = repo.getObject(sha);
        if (!target || (target.type !== 'commit' && target.type !== 'tag')) return err(422, 'sha not found');
        if (repo.tip(ref) !== undefined) return err(422, 'Reference already exists');
        repo.setRef(ref, sha);
        return ok({ ref, object: { sha, type: target.type } }, 201);
      }
      return notFound();
    }

    if (rest[0] === 'contents') {
      const path = rest.slice(1).join('/');
      if (method === 'GET' && path === '') {
        // Root directory listing.
        const tip = resolveRefTip(repo, query.get('ref'));
        if (tip === undefined) return notFound('Not Found');
        const flat = repo.filesOfCommit(tip);
        const entries: Array<Record<string, unknown>> = [];
        const dirs = new Set<string>();
        for (const [p, e] of flat) {
          const seg = p.split('/')[0]!;
          if (p.includes('/')) dirs.add(seg);
          else entries.push({ type: 'file', name: seg, path: p, sha: e.oid, size: repo.getObject(e.oid)?.data.length ?? 0 });
        }
        for (const d of [...dirs].sort()) entries.push({ type: 'dir', name: d, path: d, sha: '' });
        entries.sort((a, b) => String(a.name).localeCompare(String(b.name)));
        return ok(entries);
      }
      if (path === '') return err(422, 'path is required');
      if (method === 'GET') {
        const tip = resolveRefTip(repo, query.get('ref'));
        if (tip === undefined) return notFound('Not Found');
        const flat = repo.filesOfCommit(tip);
        const exact = flat.get(path);
        if (exact !== undefined) {
          const blob = repo.getObject(exact.oid);
          const content = blob ? blob.data : Buffer.alloc(0);
          return ok({
            type: 'file',
            encoding: 'base64',
            size: content.length,
            name: path.split('/').pop(),
            path,
            content: content.toString('base64'),
            sha: exact.oid,
            url: `${repoBase}/contents/${path}`,
          });
        }
        // Directory listing?
        const prefix = `${path}/`;
        const entries: Array<Record<string, unknown>> = [];
        const dirs = new Set<string>();
        for (const [p, e] of flat) {
          if (!p.startsWith(prefix)) continue;
          const rest2 = p.slice(prefix.length);
          const seg = rest2.split('/')[0]!;
          if (rest2.includes('/')) {
            dirs.add(seg);
          } else {
            entries.push({ type: 'file', name: seg, path: p, sha: e.oid, size: repo.getObject(e.oid)?.data.length ?? 0 });
          }
        }
        if (entries.length === 0 && dirs.size === 0) return notFound('Not Found');
        for (const d of [...dirs].sort()) {
          entries.push({ type: 'dir', name: d, path: `${path}/${d}`, sha: '' });
        }
        entries.sort((a, b) => String(a.name).localeCompare(String(b.name)));
        return ok(entries);
      }
      if (method === 'PUT') {
        const message = bodyString(b, 'message') ?? 'Update file';
        const contentRaw = bodyString(b, 'content');
        if (contentRaw === undefined) return err(422, 'content is required');
        const branch = bodyString(b, 'branch');
        const tip = branch !== undefined ? repo.tip(`refs/heads/${branch}`) : repo.defaultTip();
        const existing = tip !== undefined ? repo.filesOfCommit(tip).get(path) : undefined;
        const givenSha = bodyString(b, 'sha');
        if (givenSha !== undefined && existing !== undefined && givenSha !== existing.oid) {
          return err(409, 'sha does not match current file');
        }
        const isUpdate = existing !== undefined;
        const { commitSha, contentSha } = commitFile(store, repo, {
          branch,
          path,
          content: b64decode(contentRaw),
          message,
        });
        return ok(
          {
            content: { name: path.split('/').pop(), path, sha: contentSha, encoding: 'base64', size: b64decode(contentRaw).length },
            commit: { sha: commitSha, message },
          },
          isUpdate ? 200 : 201,
        );
      }
      if (method === 'DELETE') {
        const message = bodyString(b, 'message') ?? 'Delete file';
        const givenSha = bodyString(b, 'sha');
        const branch = bodyString(b, 'branch');
        const tip = branch !== undefined ? repo.tip(`refs/heads/${branch}`) : repo.defaultTip();
        const existing = tip !== undefined ? repo.filesOfCommit(tip).get(path) : undefined;
        if (existing === undefined) return notFound('Not Found');
        if (givenSha !== undefined && givenSha !== existing.oid) return err(409, 'sha does not match current file');
        const done = deleteFile(store, repo, { branch, path, message });
        if (done === null) return notFound('Not Found');
        return ok({ commit: { sha: done.commitSha, message } }, 200);
      }
      return err(405, 'Method Not Allowed');
    }

    if (rest[0] === 'labels') {
      if (method === 'GET') return ok([...repo.labels.values()].map(labelJson));
      if (method === 'POST') {
        const name = bodyString(b, 'name');
        if (name === undefined || name === '') return err(422, 'name is required');
        const existing = repo.labels.get(name);
        if (existing) return ok(labelJson(existing), 200);
        const label: Label = {
          name,
          color: typeof b.color === 'string' ? b.color.replace(/^#/, '') : 'ededed',
          description: bodyString(b, 'description') ?? '',
        };
        repo.labels.set(name, label);
        return ok(labelJson(label), 201);
      }
      return err(405, 'Method Not Allowed');
    }

    if (rest[0] === 'issues') {
      if (rest.length === 1) {
        if (method === 'GET') {
          const state = query.get('state') ?? 'open';
          const sort = query.get('sort') ?? 'created';
          const direction = query.get('direction') ?? (sort === 'created' ? 'asc' : 'desc');
          let list = [...repo.issues.values()];
          if (state !== 'all') list = list.filter((i) => i.state === state);
          list.sort((a, c) => {
            const d = a.seq - c.seq;
            return direction === 'desc' ? -d : d;
          });
          const { items, link } = paginate(list, query, `${repoBase}/issues`);
          return ok(items.map((i) => issueJson(store, repo, i)), 200, link ? { Link: link } : undefined);
        }
        if (method === 'POST') {
          const title = bodyString(b, 'title');
          if (title === undefined || title === '') return err(422, 'title is required');
          const labelNames: string[] = Array.isArray(b.labels) ? (b.labels as unknown[]).filter((x): x is string => typeof x === 'string') : [];
          for (const l of labelNames) {
            if (!repo.labels.has(l)) return err(422, `label ${l} does not exist`);
          }
          const number = repo.nextNumber++;
          const seq = store.nextSequence();
          const issue: Issue = {
            number,
            dbId: store.allocDbId(),
            title,
            body: bodyString(b, 'body') ?? '',
            state: 'open',
            labels: labelNames,
            comments: [],
            seq,
            createdAt: store.stamp(seq),
            updatedAt: store.stamp(seq),
          };
          repo.issues.set(number, issue);
          return ok(issueJson(store, repo, issue), 201);
        }
        return err(405, 'Method Not Allowed');
      }
      const num = Number(rest[1]);
      if (!Number.isInteger(num)) return notFound();
      if (rest.length === 2) {
        const issue = repo.issues.get(num);
        if (!issue) return notFound('Not Found');
        if (method === 'GET') return ok(issueJson(store, repo, issue));
        if (method === 'PATCH') {
          const state = bodyString(b, 'state');
          if (state === 'closed' || state === 'open') issue.state = state;
          const title = bodyString(b, 'title');
          if (title !== undefined) issue.title = title;
          const bodyText = bodyString(b, 'body');
          if (bodyText !== undefined) issue.body = bodyText;
          if (Array.isArray(b.labels)) {
            const names = (b.labels as unknown[]).filter((x): x is string => typeof x === 'string');
            for (const l of names) if (!repo.labels.has(l)) return err(422, `label ${l} does not exist`);
            issue.labels = names;
          }
          issue.updatedAt = store.stamp(store.nextSequence());
          return ok(issueJson(store, repo, issue));
        }
        return err(405, 'Method Not Allowed');
      }
      if (rest[2] === 'comments') {
        const issue = repo.issues.get(num);
        if (!issue) return notFound('Not Found');
        if (method === 'GET') {
          return ok(
            issue.comments.map((c) => ({
              id: c.id,
              node_id: store.globalId('C', repo.owner, repo.name, c.id),
              body: c.body,
              user: userRef(VIEWER.login),
              created_at: c.createdAt,
              updated_at: c.createdAt,
            })),
          );
        }
        if (method === 'POST') {
          const bodyText = bodyString(b, 'body');
          if (bodyText === undefined) return err(422, 'body is required');
          const seq = store.nextSequence();
          const comment = {
            id: repo.nextComment(),
            body: bodyText,
            seq,
            createdAt: store.stamp(seq),
          };
          issue.comments.push(comment);
          return ok(
            {
              id: comment.id,
              node_id: store.globalId('C', repo.owner, repo.name, comment.id),
              body: comment.body,
              user: userRef(VIEWER.login),
              created_at: comment.createdAt,
              updated_at: comment.createdAt,
            },
            201,
          );
        }
        return err(405, 'Method Not Allowed');
      }
      return notFound();
    }

    if (rest[0] === 'pulls') {
      if (rest.length === 1) {
        if (method === 'GET') {
          const state = query.get('state') ?? 'open';
          let list = [...repo.pulls.values()];
          if (state !== 'all') list = list.filter((p) => p.state === state);
          list.sort((a, c) => a.seq - c.seq);
          const { items, link } = paginate(list, query, `${repoBase}/pulls`);
          return ok(items.map((p) => pullJson(store, repo, p, prMergeable(repo, p))), 200, link ? { Link: link } : undefined);
        }
        if (method === 'POST') {
          const head = bodyString(b, 'head');
          const base = bodyString(b, 'base');
          const title = bodyString(b, 'title');
          if (head === undefined || base === undefined || title === undefined) return err(422, 'head, base and title are required');
          const headSha = repo.tip(`refs/heads/${head}`);
          const baseSha = repo.tip(`refs/heads/${base}`);
          if (headSha === undefined || baseSha === undefined) return err(422, 'head or base branch not found');
          const existing = [...repo.pulls.values()].find(
            (p) => p.state === 'open' && p.headRef === head && p.baseRef === base,
          );
          if (existing) return err(422, 'A pull request already exists for these branches');
          const number = repo.nextNumber++;
          const seq = store.nextSequence();
          const pr: Pull = {
            number,
            dbId: store.allocDbId(),
            title,
            body: bodyString(b, 'body') ?? '',
            state: 'open',
            merged: false,
            mergedAt: null,
            mergeCommitSha: null,
            headRef: head,
            baseRef: base,
            headSha,
            baseSha,
            seq,
            createdAt: store.stamp(seq),
            updatedAt: store.stamp(seq),
          };
          repo.pulls.set(number, pr);
          return ok(pullJson(store, repo, pr, prMergeable(repo, pr)), 201);
        }
        return err(405, 'Method Not Allowed');
      }
      const num = Number(rest[1]);
      if (!Number.isInteger(num)) return notFound();
      const pr = repo.pulls.get(num);
      if (rest.length === 2) {
        if (!pr) return notFound('Not Found');
        if (method !== 'GET') return err(405, 'Method Not Allowed');
        return ok(pullJson(store, repo, pr, prMergeable(repo, pr)));
      }
      if (rest[2] === 'files') {
        if (!pr) return notFound('Not Found');
        if (method !== 'GET') return err(405, 'Method Not Allowed');
        return ok(prFiles(repo, pr));
      }
      if (rest[2] === 'merge') {
        if (!pr) return notFound('Not Found');
        if (method !== 'PUT') return err(405, 'Method Not Allowed');
        if (pr.state !== 'open') return err(405, 'Pull request is already closed');
        const mergeMethod = bodyString(b, 'merge_method') ?? 'squash';
        if (!prMergeable(repo, pr)) return err(405, 'Pull request cannot be merged due to conflicts');
        const baseTip = repo.tip(`refs/heads/${pr.baseRef}`);
        const headCommit = repo.commitOf(pr.headSha);
        if (baseTip === undefined || headCommit === undefined) return err(422, 'base or head commit not found');
        const seq = store.nextSequence();
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
        pr.mergedAt = store.stamp(seq);
        pr.mergeCommitSha = mergeSha;
        pr.updatedAt = store.stamp(seq);
        return ok({ merged: true, sha: mergeSha, message: 'Pull request successfully merged' }, 200);
      }
      return notFound();
    }

    return notFound();
  }

  return notFound();
}

function sortedRefNames(repo: Repo): string[] {
  const names = [...repo.refs.keys()];
  names.sort((a, b) => {
    const rank = (n: string): number => (n.startsWith('refs/heads/') ? 0 : n.startsWith('refs/tags/') ? 1 : 2);
    const ra = rank(a);
    const rb = rank(b);
    if (ra !== rb) return ra - rb;
    return a < b ? -1 : a > b ? 1 : 0;
  });
  return names;
}
