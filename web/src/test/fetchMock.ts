import { vi } from 'vitest';

export interface Call {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: unknown;
}

type Reply = { status?: number; body?: unknown };
type Handler = (call: Call) => Reply | undefined;

// mockFetch replaces fetch with a route table keyed "METHOD /path" (the query
// is ignored for matching but kept in call.url). A route is a reply or a
// function of the call. An unknown route answers 404. Every call is recorded.
export function mockFetch(routes: Record<string, Reply | Handler>) {
  const calls: Call[] = [];
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = (init?.method ?? 'GET').toUpperCase();
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
    const call = { method, url, headers, body };
    calls.push(call);
    const path = url.split('?')[0];
    const route = routes[`${method} ${path}`];
    const reply = typeof route === 'function' ? route(call) : route;
    const status = reply?.status ?? (reply ? 200 : 404);
    const payload = reply ? reply.body : { error: 'not found' };
    const text = payload === undefined ? '' : JSON.stringify(payload);
    return new Response(status === 204 || status === 202 ? null : text, { status });
  });
  vi.stubGlobal('fetch', fn);
  return { calls, fn };
}

export function lastCall(calls: Call[], method: string, path: string): Call | undefined {
  return [...calls].reverse().find((c) => c.method === method && c.url.split('?')[0] === path);
}
