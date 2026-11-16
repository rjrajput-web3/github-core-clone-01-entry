// GitForge MCP server: JSON-RPC 2.0 over Streamable HTTP (POST /mcp), acting on
// the same in-memory store as the REST and GraphQL surfaces. Tools are
// precomputed once so tools/list is byte-identical across calls.

import { VIEWER_LOGIN, FORGE_NAME, FORGE_VERSION, type Store } from './store.ts';

interface ToolDef {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, { type: string; description: string }>; required: string[] };
}

const TOOLS: ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository for the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
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
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
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
        owner: { type: 'string', description: 'Repository owner login' },
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
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        state: { type: 'string', description: 'Issue state filter: open, closed, or all' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

interface RpcError {
  code: number;
  message: string;
}

function rpcResult(id: unknown, result: unknown): { status: number; body: string } {
  return { status: 200, body: JSON.stringify({ jsonrpc: '2.0', id, result }) };
}

function rpcError(id: unknown, err: RpcError, status = 200): { status: number; body: string } {
  return { status, body: JSON.stringify({ jsonrpc: '2.0', id, error: err }) };
}

function toolResult(result: unknown): { content: Array<{ type: 'text'; text: string }>; structuredContent: unknown; isError: false } {
  return { content: [{ type: 'text', text: JSON.stringify(result) }], structuredContent: result, isError: false };
}

function toolError(message: string): { content: Array<{ type: 'text'; text: string }>; structuredContent: null; isError: true } {
  return { content: [{ type: 'text', text: message }], structuredContent: null, isError: true };
}

function missingArg(tool: ToolDef, args: Record<string, unknown>): string | null {
  for (const req of tool.inputSchema.required) {
    if (args[req] === undefined || args[req] === null || args[req] === '') return req;
  }
  return null;
}

/**
 * Handle one POST /mcp request body. Returns {status, body} where body is a
 * string ('' for the 202 notification acknowledgement).
 */
export function handleMcp(store: Store, rawBody: Buffer): { status: number; body: string } {
  let body: unknown;
  try {
    body = JSON.parse(rawBody.toString('utf8'));
  } catch {
    return rpcError(null, { code: -32700, message: 'Parse error' }, 400);
  }
  if (typeof body !== 'object' || body === null || Array.isArray(body)) {
    return rpcError(null, { code: -32600, message: 'Invalid Request' }, 400);
  }
  const req = body as Record<string, unknown>;
  const id = req.id ?? null;
  const method = req.method;

  if (method === 'notifications/initialized') {
    return { status: 202, body: '' };
  }
  if (typeof method !== 'string') {
    return rpcError(id, { code: -32600, message: 'Invalid Request' });
  }

  if (method === 'server/discover') {
    return rpcResult(id, {
      supportedVersions: ['2026-07-28', '2025-06-18', '2025-03-26'],
      capabilities: { tools: {} },
      serverInfo: { name: FORGE_NAME, version: FORGE_VERSION },
    });
  }
  if (method === 'initialize') {
    const params = (req.params ?? {}) as Record<string, unknown>;
    return rpcResult(id, {
      protocolVersion: typeof params.protocolVersion === 'string' ? params.protocolVersion : '2026-07-28',
      capabilities: { tools: {} },
      serverInfo: { name: FORGE_NAME, version: FORGE_VERSION },
    });
  }
  if (method === 'ping') {
    return rpcResult(id, {});
  }
  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const params = (req.params ?? {}) as Record<string, unknown>;
    const name = params.name as string;
    const args = (params.arguments ?? {}) as Record<string, unknown>;
    const tool = TOOLS.find((t) => t.name === name);
    if (!tool) {
      return rpcError(id, { code: -32601, message: `Unknown tool: ${String(name)}` });
    }
    const missing = missingArg(tool, args);
    if (missing !== null) {
      return rpcError(id, { code: -32602, message: `Missing required argument: ${missing}` });
    }
    try {
      const result = callTool(store, name, args);
      return rpcResult(id, result);
    } catch (err) {
      return rpcResult(id, toolError(err instanceof Error ? err.message : String(err)));
    }
  }
  return rpcError(id, { code: -32601, message: `Method not found: ${method}` });
}

function callTool(store: Store, name: string, args: Record<string, unknown>): unknown {
  const owner = args.owner as string | undefined;
  const repoName = args.repo as string | undefined;
  switch (name) {
    case 'create_repo': {
      const repoNameArg = args.name as string;
      const repo = store.createRepo(
        VIEWER_LOGIN,
        repoNameArg,
        typeof args.description === 'string' ? args.description : '',
        args.private === true,
      );
      if (!repo) throw new Error(`Repository already exists: ${VIEWER_LOGIN}/${repoNameArg}`);
      return toolResult({
        id: repo.id,
        name: repo.name,
        full_name: `${repo.owner}/${repo.name}`,
        owner: { login: repo.owner },
        private: repo.private,
        description: repo.description,
        default_branch: repo.defaultBranch,
      });
    }
    case 'get_repo': {
      const repo = store.getRepo(owner!, repoName!);
      if (!repo) throw new Error(`Repository not found: ${owner}/${repoName}`);
      return toolResult({
        id: repo.id,
        name: repo.name,
        full_name: `${repo.owner}/${repo.name}`,
        owner: { login: repo.owner },
        private: repo.private,
        description: repo.description,
        default_branch: repo.defaultBranch,
      });
    }
    case 'create_issue': {
      const repo = store.getRepo(owner!, repoName!);
      if (!repo) throw new Error(`Repository not found: ${owner}/${repoName}`);
      const made = store.createIssue(repo, args.title as string, typeof args.body === 'string' ? args.body : '', []);
      if ('error' in made) throw new Error(made.error);
      return toolResult(issueJson(repo, made));
    }
    case 'list_issues': {
      const repo = store.getRepo(owner!, repoName!);
      if (!repo) throw new Error(`Repository not found: ${owner}/${repoName}`);
      const state = typeof args.state === 'string' ? args.state : 'open';
      const list = [...repo.issues.values()]
        .filter((i) => state === 'all' || i.state === state)
        .sort((a, b) => a.number - b.number)
        .map((i) => issueJson(repo, i));
      return toolResult(list);
    }
    case 'create_pull_request': {
      const repo = store.getRepo(owner!, repoName!);
      if (!repo) throw new Error(`Repository not found: ${owner}/${repoName}`);
      const made = store.createPull(
        repo,
        args.title as string,
        typeof args.body === 'string' ? args.body : '',
        args.head as string,
        args.base as string,
      );
      if ('error' in made) throw new Error(made.error);
      return toolResult(prJson(repo, made));
    }
    case 'merge_pull_request': {
      const repo = store.getRepo(owner!, repoName!);
      if (!repo) throw new Error(`Repository not found: ${owner}/${repoName}`);
      const pr = repo.pulls.get(Number(args.pull_number));
      if (!pr) throw new Error(`Pull request not found: ${String(args.pull_number)}`);
      const merged = store.mergePull(repo, pr);
      if (!merged.ok) throw new Error(merged.reason);
      return toolResult({ merged: true, sha: merged.sha, message: `Merge pull request #${pr.number} from ${pr.headRef}` });
    }
    default:
      throw new Error(`Unknown tool: ${name}`);
  }
}

function issueJson(repo: import('./store.ts').Repo, issue: import('./store.ts').Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => ({ name })),
    user: { login: issue.user },
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    html_url: `/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

function prJson(repo: import('./store.ts').Repo, pr: import('./store.ts').Pull): Record<string, unknown> {
  return {
    id: pr.id,
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.state,
    merged: pr.merged,
    head: { ref: pr.headRef, sha: repo.refs.get(`refs/heads/${pr.headRef}`) ?? pr.headSha },
    base: { ref: pr.baseRef, sha: repo.refs.get(`refs/heads/${pr.baseRef}`) ?? pr.baseSha },
    merged_at: pr.mergedAt,
    merge_commit_sha: pr.mergeCommitSha,
    html_url: `/${repo.owner}/${repo.name}/pull/${pr.number}`,
  };
}
