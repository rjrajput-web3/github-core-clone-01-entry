// app.ts — one node:http server: routing, auth, JSON plumbing, request ids.
// Git smart HTTP, REST v3, GraphQL v4 and MCP all hang off this server.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './store.ts';
import { infoRefs, receivePack, uploadPack } from './git.ts';
import { handleRest } from './rest.ts';
import { executeGraphQL, type GraphQLBody } from './graphql.ts';
import { handleMcp } from './mcp.ts';

let requestCounter = 0;

function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter}`;
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    req.on('data', (chunk: Buffer) => {
      total += chunk.length;
      if (total > 64 * 1024 * 1024) {
        reject(new Error('body too large'));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function sendJson(res: ServerResponse, status: number, value: unknown, extra?: Record<string, string>): void {
  const body = value === null ? '' : JSON.stringify(value);
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(body),
    'X-GitHub-Request-Id': nextRequestId(),
    ...(extra ?? {}),
  });
  res.end(body);
}

function sendBuffer(res: ServerResponse, status: number, contentType: string, body: Buffer): void {
  res.writeHead(status, {
    'Content-Type': contentType,
    'Content-Length': body.length,
    'X-GitHub-Request-Id': nextRequestId(),
  });
  res.end(body);
}

function authFailed(res: ServerResponse): void {
  sendJson(res, 401, { message: 'Requires authentication' });
}

/** The bearer token this instance accepts; empty means "any non-empty bearer". */
function expectedToken(): string {
  return process.env.MAKEFUN_SHOP_TOKEN ?? '';
}

function authorized(req: IncomingMessage): boolean {
  const header = req.headers.authorization ?? '';
  if (!header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length).trim();
  if (token === '') return false;
  const expected = expectedToken();
  if (expected === '') return true;
  return token === expected;
}

export function createApp(store: Store): Server {
  const server = createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://127.0.0.1');
      const method = req.method ?? 'GET';
      const segments = url.pathname.split('/').filter((s) => s !== '').map((s) => {
        try {
          return decodeURIComponent(s);
        } catch {
          return s;
        }
      });

      if (url.pathname === '/healthz') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // ---- REST v3 ----
      if (segments[0] === 'api' && segments[1] === 'v3') {
        if (!authorized(req)) {
          authFailed(res);
          return;
        }
        let body: Record<string, unknown> | null = null;
        if (method === 'POST' || method === 'PATCH' || method === 'PUT' || method === 'DELETE') {
          const raw = await readBody(req);
          if (raw.length > 0) {
            try {
              const parsed: unknown = JSON.parse(raw.toString('utf8'));
              if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
                sendJson(res, 400, { message: 'request body must be a JSON object' });
                return;
              }
              body = parsed as Record<string, unknown>;
            } catch {
              sendJson(res, 400, { message: 'Problems parsing JSON' });
              return;
            }
          }
        }
        const base = `http://${req.headers.host ?? '127.0.0.1'}`;
        const result = handleRest(store, method, segments.slice(2), url.searchParams, body, base);
        if (result.body === null) {
          res.writeHead(result.status, { 'X-GitHub-Request-Id': nextRequestId(), ...(result.headers ?? {}) });
          res.end();
          return;
        }
        sendJson(res, result.status, result.body, result.headers);
        return;
      }

      // ---- GraphQL v4 ----
      if (segments[0] === 'api' && segments[1] === 'graphql') {
        if (method !== 'POST') {
          sendJson(res, 405, { message: 'Method Not Allowed' });
          return;
        }
        if (!authorized(req)) {
          authFailed(res);
          return;
        }
        const raw = await readBody(req);
        let parsed: unknown;
        try {
          parsed = JSON.parse(raw.toString('utf8'));
        } catch {
          sendJson(res, 400, { message: 'Problems parsing JSON' });
          return;
        }
        if (Array.isArray(parsed)) {
          sendJson(res, 200, parsed.map((item) => executeGraphQL(store, (item ?? {}) as GraphQLBody)));
        } else if (parsed !== null && typeof parsed === 'object') {
          sendJson(res, 200, executeGraphQL(store, parsed as GraphQLBody));
        } else {
          sendJson(res, 400, { message: 'request body must be a JSON object' });
        }
        return;
      }

      // ---- MCP ----
      if (segments[0] === 'mcp') {
        if (method !== 'POST') {
          sendJson(res, 405, { message: 'Method Not Allowed' });
          return;
        }
        if (!authorized(req)) {
          authFailed(res);
          return;
        }
        const raw = await readBody(req);
        let parsed: unknown = null;
        let malformed = false;
        try {
          parsed = raw.length > 0 ? JSON.parse(raw.toString('utf8')) : null;
        } catch {
          malformed = true;
        }
        const outcome = handleMcp(store, parsed, malformed);
        if (outcome.body === null) {
          res.writeHead(outcome.status, { 'X-GitHub-Request-Id': nextRequestId() });
          res.end();
          return;
        }
        sendJson(res, outcome.status, outcome.body);
        return;
      }

      // ---- git smart HTTP ----
      // /{owner}/{repo}.git/info/refs, /{owner}/{repo}.git/git-upload-pack,
      // /{owner}/{repo}.git/git-receive-pack (the .git suffix is optional).
      if (segments.length >= 3) {
        const owner = segments[0]!;
        const repoSeg = segments[1]!;
        const repoName = repoSeg.endsWith('.git') ? repoSeg.slice(0, -4) : repoSeg;
        const wantV2 = (req.headers['git-protocol'] ?? '') === 'version=2';
        if (segments.length === 4 && segments[2] === 'info' && segments[3] === 'refs') {
          if (method !== 'GET') {
            sendJson(res, 405, { message: 'Method Not Allowed' });
            return;
          }
          const service = url.searchParams.get('service') ?? '';
          const r = infoRefs(store, owner, repoName, service, wantV2);
          sendBuffer(res, r.status, r.contentType, r.body);
          return;
        }
        if (segments.length === 3 && segments[2] === 'git-upload-pack' && method === 'POST') {
          const raw = await readBody(req);
          const r = uploadPack(store, owner, repoName, raw, wantV2);
          sendBuffer(res, r.status, r.contentType, r.body);
          return;
        }
        if (segments.length === 3 && segments[2] === 'git-receive-pack' && method === 'POST') {
          const raw = await readBody(req);
          const r = receivePack(store, owner, repoName, raw);
          sendBuffer(res, r.status, r.contentType, r.body);
          return;
        }
      }

      sendJson(res, 404, { message: 'Not Found' });
    } catch (e) {
      if (!res.headersSent) {
        sendJson(res, 500, { message: e instanceof Error ? e.message : 'Internal Server Error' });
      } else {
        res.destroy();
      }
    }
  });
  server.keepAliveTimeout = 30000;
  server.headersTimeout = 60000;
  return server;
}
