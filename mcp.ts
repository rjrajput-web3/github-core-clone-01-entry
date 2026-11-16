// mcp.ts — Model Context Protocol over Streamable HTTP (JSON-RPC 2.0, plain JSON
// responses): server/discover + initialize handshake, tools/list and tools/call.
// Tools operate on the same store as REST and GraphQL.

import { VIEWER, buildCommit, type Repo, type Store } from './store.ts';
import { issueJson, prMergeable, pullJson, repoJson } from './rest.ts';

const SERVER_NAME = 'forge-mcp';
const SERVER_VERSION = '1.0.0';

interface ToolDef {
  name: string;
  description: string;
  inputSchema: {
    type: 'object';
    properties: Record<string, { type: string; description: string; enum?: string[] }>;
    required?: string[];
  };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the viewer.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        owner: { type: 'string', description: 'Owner login (defaults to the viewer)' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'list_repos',
    description: 'List repositories owned by a user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login (defaults to the viewer)' },
      },
      required: [],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state filter', enum: ['open', 'closed', 'all'] },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'get_issue',
    description: 'Get one issue by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        issue_number: { type: 'integer', description: 'Issue number' },
      },
      required: ['owner', 'repo', 'issue_number'],
    },
  },
  {
    name: 'close_issue',
    description: 'Close an issue by number.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        issue_number: { type: 'integer', description: 'Issue number' },
      },
      required: ['owner', 'repo', 'issue_number'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request between two branches.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
        body: { type: 'string', description: 'Pull request body' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash commit.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

function toolResult(obj: unknown): Record<string, unknown> {
  const text = typeof obj === 'string' ? obj : JSON.stringify(obj);
  return { content: [{ type: 'text', text }], structuredContent: obj };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

function strArg(args: Record<string, unknown>, ...keys: string[]): string | undefined {
  for (const k of keys) {
    const v = args[k];
    if (typeof v === 'string' && v !== '') return v;
  }
  return undefined;
}

function numArg(args: Record<string, unknown>, ...keys: string[]): number | undefined {
  for (const k of keys) {
    const v = args[k];
    if (typeof v === 'number' && Number.isFinite(v)) return Math.trunc(v);
    if (typeof v === 'string' && /^\d+$/.test(v)) return Number(v);
  }
  return undefined;
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const owner = strArg(args, 'owner') ?? VIEWER.login;
  const repoName = strArg(args, 'repo', 'repository');
  const repo: Repo | undefined = repoName !== undefined ? store.getRepo(owner, repoName) : undefined;

  switch (name) {
    case 'create_repo': {
      const repoNameArg = strArg(args, 'name', 'repo', 'repository');
      if (repoNameArg === undefined) return toolError('name is required');
      const repoOwner = strArg(args, 'owner') ?? VIEWER.login;
      if (store.getRepo(repoOwner, repoNameArg)) return toolError('Repository already exists');
      const created = store.createRepo(
        repoOwner,
        repoNameArg,
        strArg(args, 'description') ?? '',
        args.private === true,
      );
      return toolResult(repoJson(store, created));
    }
    case 'get_repo': {
      if (repo === undefined) return toolError('Repository not found');
      return toolResult(repoJson(store, repo));
    }
    case 'list_repos': {
      const listOwner = strArg(args, 'owner') ?? VIEWER.login;
      const repos = store.listRepos(listOwner).map((r) => repoJson(store, r));
      return toolResult({ repositories: repos, total_count: repos.length });
    }
    case 'create_issue': {
      if (repo === undefined) return toolError('Repository not found');
      const title = strArg(args, 'title');
      if (title === undefined) return toolError('title is required');
      const number = repo.nextNumber++;
      const seq = store.nextSequence();
      const issue = {
        number,
        dbId: store.allocDbId(),
        title,
        body: strArg(args, 'body') ?? '',
        state: 'open' as const,
        labels: [] as string[],
        comments: [] as Array<{ id: number; body: string; seq: number; createdAt: string }>,
        seq,
        createdAt: store.stamp(seq),
        updatedAt: store.stamp(seq),
      };
      repo.issues.set(number, issue);
      return toolResult(issueJson(store, repo, issue));
    }
    case 'list_issues': {
      if (repo === undefined) return toolError('Repository not found');
      const state = strArg(args, 'state') ?? 'open';
      let list = [...repo.issues.values()];
      if (state !== 'all') list = list.filter((i) => i.state === state);
      list.sort((a, b) => a.seq - b.seq);
      return toolResult({
        repository: repo.fullName,
        issues: list.map((i) => issueJson(store, repo, i)),
        total_count: list.length,
      });
    }
    case 'get_issue':
    case 'close_issue': {
      if (repo === undefined) return toolError('Repository not found');
      const number = numArg(args, 'issue_number', 'number');
      if (number === undefined) return toolError('issue_number is required');
      const issue = repo.issues.get(number);
      if (!issue) return toolError('Issue not found');
      if (name === 'close_issue') {
        issue.state = 'closed';
        issue.updatedAt = store.stamp(store.nextSequence());
      }
      return toolResult(issueJson(store, repo, issue));
    }
    case 'create_pull_request': {
      if (repo === undefined) return toolError('Repository not found');
      const title = strArg(args, 'title');
      const head = strArg(args, 'head');
      const base = strArg(args, 'base');
      if (title === undefined || head === undefined || base === undefined) {
        return toolError('title, head and base are required');
      }
      const headSha = repo.tip(`refs/heads/${head}`);
      const baseSha = repo.tip(`refs/heads/${base}`);
      if (headSha === undefined || baseSha === undefined) return toolError('head or base branch not found');
      const existing = [...repo.pulls.values()].find((p) => p.state === 'open' && p.headRef === head && p.baseRef === base);
      if (existing) return toolError('A pull request already exists for these branches');
      const number = repo.nextNumber++;
      const seq = store.nextSequence();
      const pr = {
        number,
        dbId: store.allocDbId(),
        title,
        body: strArg(args, 'body') ?? '',
        state: 'open' as const,
        merged: false,
        mergedAt: null as string | null,
        mergeCommitSha: null as string | null,
        headRef: head,
        baseRef: base,
        headSha,
        baseSha,
        seq,
        createdAt: store.stamp(seq),
        updatedAt: store.stamp(seq),
      };
      repo.pulls.set(number, pr);
      return toolResult(pullJson(store, repo, pr, prMergeable(repo, pr)));
    }
    case 'merge_pull_request': {
      if (repo === undefined) return toolError('Repository not found');
      const number = numArg(args, 'pull_number', 'number');
      if (number === undefined) return toolError('pull_number is required');
      const pr = repo.pulls.get(number);
      if (!pr) return toolError('Pull request not found');
      if (pr.state !== 'open') return toolError('Pull request is already closed');
      if (!prMergeable(repo, pr)) return toolError('Pull request cannot be merged due to conflicts');
      const baseTip = repo.tip(`refs/heads/${pr.baseRef}`);
      const headCommit = repo.commitOf(pr.headSha);
      if (baseTip === undefined || headCommit === undefined) return toolError('base or head commit not found');
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
      return toolResult({ merged: true, sha: mergeSha, message: 'Pull request successfully merged' });
    }
    default:
      return toolError(`unknown tool ${name}`);
  }
}

export interface McpOutcome {
  status: number;
  body: unknown;
}

/** Handle one JSON-RPC request object. `malformed` means the HTTP body was not JSON. */
export function handleMcp(store: Store, body: unknown, malformed: boolean): McpOutcome {
  if (malformed) {
    return {
      status: 400,
      body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } },
    };
  }
  if (body === null || typeof body !== 'object' || Array.isArray(body)) {
    return {
      status: 400,
      body: { jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid Request' } },
    };
  }
  const req = body as Record<string, unknown>;
  const id = (req.id ?? null) as string | number | null;
  const method = typeof req.method === 'string' ? req.method : '';
  const params = (req.params ?? {}) as Record<string, unknown>;

  const result = (r: unknown): McpOutcome => ({ status: 200, body: { jsonrpc: '2.0', id, result: r } });
  const error = (code: number, message: string): McpOutcome => ({
    status: 200,
    body: { jsonrpc: '2.0', id, error: { code, message } },
  });

  if (method.startsWith('notifications/')) {
    return { status: 202, body: null };
  }
  if (method === 'ping') return result({});
  if (method === 'server/discover') {
    return result({
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      capabilities: { tools: {} },
      supportedVersions: ['2026-07-28', '2025-11-25'],
      protocolVersion: '2026-07-28',
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }
  if (method === 'initialize') {
    return result({
      protocolVersion: '2026-07-28',
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'tools/list') {
    return result({ tools: TOOLS });
  }
  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const def = TOOLS.find((t) => t.name === toolName);
    if (def === undefined) return error(-32601, `Method not found: ${toolName}`);
    const missing = (def.inputSchema.required ?? []).filter(
      (r) => args[r] === undefined || args[r] === null || args[r] === '',
    );
    if (missing.length > 0) {
      return error(-32602, `Missing required arguments: ${missing.join(', ')}`);
    }
    return result(callTool(store, toolName, args));
  }
  return error(-32601, `Method not found: ${method}`);
}
