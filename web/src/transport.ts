import {
  parseConnection, parseSnapshot, parseThreadList, record, string,
  type ConnectionInfo, type Mutation, type ReadOptions, type Snapshot, type ThreadList, type ThreadQuery,
} from './types.ts';

export type Fetcher = (input: string, init: RequestInit) => Promise<Response>;

export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly unknownOutcome: boolean;

  constructor(status: number, code: string, unknownOutcome = false) {
    // Server error text is deliberately not copied into the UI or console.
    super(code || 'request_failed');
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.unknownOutcome = unknownOutcome;
  }
}

const rejectedCodes = new Set([
  'operation_rejected', 'resync_required', 'state_changed', 'disconnected',
  'request_id_reused', 'invalid_request', 'sharing_disabled', 'csrf_invalid',
  'rate_limited', 'read_only', 'not_found', 'capacity_limited', 'invalid_input',
]);

export interface Api {
  connection(signal?: AbortSignal): Promise<ConnectionInfo>;
  threads(query?: ThreadQuery, signal?: AbortSignal): Promise<ThreadList>;
  snapshot(id: string, options?: ReadOptions, signal?: AbortSignal): Promise<Snapshot>;
  mutate(threadId: string, csrf: string, mutation: Mutation, requestId: string): Promise<void>;
}

export class HttpApi implements Api {
  private readonly fetcher: Fetcher;

  constructor(fetcher: Fetcher = (path, init) => fetch(path, init)) {
    this.fetcher = fetcher;
  }

  private async request(
    path: string,
    options: { body?: unknown; csrf?: string; signal?: AbortSignal; mutation?: boolean; timeout?: number } = {},
  ): Promise<unknown> {
    const post = options.body !== undefined;
    const controller = new AbortController();
    const abort = () => controller.abort();
    options.signal?.addEventListener('abort', abort, { once: true });
    if (options.signal?.aborted) controller.abort();
    const timeout = setTimeout(abort, options.timeout ?? (post ? 52_000 : 20_000));
    try {
      // One fetch, once. POSTs are never retried, including redirects, timeouts,
      // dropped responses, and malformed success responses.
      const response = await this.fetcher(path, {
        method: post ? 'POST' : 'GET',
        credentials: 'same-origin', cache: 'no-store', redirect: 'error',
        referrerPolicy: 'no-referrer', signal: controller.signal,
        headers: {
          Accept: 'application/json',
          ...(post ? { 'Content-Type': 'application/json' } : {}),
          ...(options.csrf ? { 'X-MirrorBird-CSRF': options.csrf } : {}),
        },
        ...(post ? { body: JSON.stringify(options.body) } : {}),
      });
      let data: unknown;
      try {
        if (!response.headers.get('content-type')?.includes('application/json')) throw new Error('Not JSON');
        data = await response.json();
      } catch {
        throw new ApiError(response.status, 'invalid_response', post);
      }
      if (!response.ok) {
        const rawCode = string(record(data).code);
        const code = rawCode.length <= 80 && /^[a-z_]*$/.test(rawCode) ? rawCode : '';
        const unknown = post && (
          code === 'outcome_unknown' || code === 'operation_failed_or_outcome_unknown'
          || (!rejectedCodes.has(code) && ![400, 401, 403, 404, 405, 413, 415, 429].includes(response.status))
        );
        throw new ApiError(response.status, code, unknown);
      }
      if (options.mutation && (response.status !== 202 || record(data).accepted !== true)) {
        throw new ApiError(response.status, 'invalid_response', true);
      }
      return data;
    } catch (error) {
      if (error instanceof ApiError) throw error;
      if (options.signal?.aborted && !post) throw error;
      throw new ApiError(0, 'network_error', post);
    } finally {
      clearTimeout(timeout);
      options.signal?.removeEventListener('abort', abort);
    }
  }

  private parse<T>(value: unknown, parser: (data: unknown) => T, post = false): T {
    try { return parser(value); }
    catch { throw new ApiError(200, 'invalid_response', post); }
  }

  async connection(signal?: AbortSignal): Promise<ConnectionInfo> {
    return this.parse(await this.request('/api/connection', { signal }), parseConnection);
  }

  async threads(query: ThreadQuery = {}, signal?: AbortSignal): Promise<ThreadList> {
    const params = new URLSearchParams({ limit: String(query.limit ?? 50) });
    if (query.cursor) params.set('cursor', query.cursor);
    if (query.search) params.set('search', query.search);
    if (query.archived) params.set('archived', 'true');
    return this.parse(await this.request(`/api/threads?${params}`, { signal }), parseThreadList);
  }

  async snapshot(id: string, options: ReadOptions = {}, signal?: AbortSignal): Promise<Snapshot> {
    const params = new URLSearchParams();
    if (options.archived) params.set('archived', 'true');
    if (options.resync) params.set('resync', 'true');
    const query = params.size ? `?${params}` : '';
    // Lazy projection initialization may take up to the server's 45-second budget.
    return this.parse(await this.request(`/api/threads/${encodeURIComponent(id)}${query}`, { signal, timeout: 50_000 }), parseSnapshot);
  }

  async mutate(threadId: string, csrf: string, mutation: Mutation, requestId: string): Promise<void> {
    const { action, ...body } = mutation;
    await this.request(`/api/threads/${encodeURIComponent(threadId)}/${action}`, {
      body: { ...body, requestId }, csrf, mutation: true,
    });
  }
}

export function friendlyError(error: unknown): string {
  if (!(error instanceof ApiError)) return '暂时无法完成操作，请刷新后再试。';
  switch (error.code) {
    case 'resync_required': return '同步需要重新建立。请点击“重新同步”，核对最新会话状态。';
    case 'sharing_disabled': return '电脑已关闭远程服务。请在电脑端开启 MirrorBird 的远程服务。';
    case 'csrf_invalid': return '远程服务状态已改变。请刷新并核对，没有重发任何操作。';
    case 'read_only': return '后端不允许此会话接受输入，当前为只读。';
    case 'not_found': return '暂时找不到这个会话。可以刷新列表，或在 Desktop 检查。';
    case 'capacity_limited': return '暂时无法打开更多会话，请稍后重试。';
    case 'state_changed': return 'Desktop 的会话状态已改变。请刷新，核对后再发送。';
    case 'disconnected': return 'Desktop 暂未连接。请在电脑端检查 MirrorBird。';
    case 'request_id_reused': return '此操作标识已被使用。请刷新并核对结果，不要直接重复操作。';
    case 'operation_rejected': return 'Desktop 未接受这次操作。请刷新并检查会话状态。';
    case 'invalid_response': return '收到的响应无法确认。请检查电脑端版本并刷新。';
  }
  if (error.unknownOutcome) return '这次操作的结果尚不确定，可能已在 Desktop 执行。没有自动重试，请先刷新并核对。';
  if (error.status === 429) return '请求过于频繁。请稍等片刻，再刷新重试。';
  if (error.status === 401) return '远程连接暂时不可用。请重新打开电脑端提供的远程地址，并检查 Microsoft 隧道访问状态。';
  if (error.status === 403) return '目前无法操作。请刷新并检查电脑端远程服务状态。';
  if (error.status === 409) return '会话状态已改变。请刷新并核对后再操作。';
  if (error.status === 400 || error.status === 413) return '操作未被接受，请检查输入内容和长度。';
  return '连接暂时不可用。正在尝试重新同步，未重发任何操作。';
}
