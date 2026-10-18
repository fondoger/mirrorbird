export interface ConnectionInfo {
  sharing: boolean;
  connected: boolean;
  csrf?: string;
}

export interface ThreadSummary {
  id: string;
  name: string;
  status: string;
  archived?: boolean;
  cwd?: string;
  updatedAt?: number;
  canAcceptDirectInput?: boolean | null;
}

export interface ThreadList {
  threads: ThreadSummary[];
  nextCursor?: string;
  connected: boolean;
}

export interface ThreadQuery {
  cursor?: string;
  search?: string;
  archived?: boolean;
  limit?: number;
}

export interface ReadOptions {
  archived?: boolean;
  resync?: boolean;
}

export interface Item {
  id: string;
  type: string;
  text?: string;
  content?: unknown;
  status?: string;
  command?: string;
  aggregatedOutput?: string;
  changes?: unknown;
  name?: string;
}

export interface Turn {
  id: string;
  status: string;
  items: Item[];
}

export interface PendingRequest {
  // These are opaque server-issued handles, never JSON-RPC numeric IDs.
  id: string;
  method: string;
  params: unknown;
  desktopOnly?: boolean;
}

export interface Snapshot {
  thread: {
    id: string;
    name: string;
    cwd: string;
    status: { type: string };
    turns: Turn[];
    canAcceptDirectInput?: boolean | null;
  };
  readOnly?: boolean;
  revision: number;
  activeTurnId?: string;
  pending: PendingRequest[];
  historyTruncated?: boolean;
  resyncRequired?: boolean;
  resyncReason?: string;
}

export type SendMode = 'start' | 'steer';
export type Mutation =
  | { action: 'messages'; text: string; mode: SendMode; expectedTurnId?: string }
  | { action: 'interrupt'; turnId: string }
  | { action: 'respond'; id: string; decision: 'accept' | 'decline' };

export function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

export function string(value: unknown, fallback = ''): string {
  return typeof value === 'string' ? value : fallback;
}

function array(value: unknown): unknown[] {
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value)) throw new Error('Invalid collection');
  return value;
}

function identifier(value: unknown): string {
  if (typeof value !== 'string' || !value || value.length > 4096) {
    throw new Error('Invalid identifier');
  }
  return value;
}

export function parseConnection(value: unknown): ConnectionInfo {
  const data = record(value);
  if (typeof data.sharing !== 'boolean' || typeof data.connected !== 'boolean') {
    throw new Error('Invalid connection');
  }
  if (data.sharing && (typeof data.csrf !== 'string' || !data.csrf)) {
    throw new Error('Missing operation proof');
  }
  return { sharing: data.sharing, connected: data.connected, csrf: data.sharing ? string(data.csrf) : undefined };
}

export function parseThreadList(value: unknown): ThreadList {
  const data = record(value);
  if (typeof data.connected !== 'boolean') throw new Error('Invalid thread list');
  if (data.nextCursor != null && typeof data.nextCursor !== 'string') throw new Error('Invalid cursor');
  return {
    connected: data.connected,
    nextCursor: string(data.nextCursor) || undefined,
    threads: array(data.threads).map((entry) => {
      const item = record(entry);
      return {
        id: identifier(item.id), name: string(item.name, '未命名会话'), status: string(item.status),
        archived: item.archived === true, cwd: string(item.cwd),
        updatedAt: typeof item.updatedAt === 'number' && Number.isFinite(item.updatedAt) ? item.updatedAt : undefined,
        canAcceptDirectInput: typeof item.canAcceptDirectInput === 'boolean' ? item.canAcceptDirectInput : null,
      };
    }),
  };
}

export function parseSnapshot(value: unknown): Snapshot {
  const data = record(value);
  const thread = record(data.thread);
  if (typeof data.revision !== 'number' || !Number.isFinite(data.revision) || data.revision < 0) {
    throw new Error('Invalid snapshot revision');
  }
  const pending = array(data.pending).map((entry): PendingRequest => {
    const request = record(entry);
    // Do not coerce a number to a string: that would silently lose precision and
    // could approve a different request. A protocol mismatch fails closed.
    return {
      id: identifier(request.id), method: string(request.method), params: request.params,
      desktopOnly: request.desktopOnly === true,
    };
  });
  const turns = array(thread.turns).map((entry): Turn => {
    const turn = record(entry);
    return {
      id: identifier(turn.id),
      status: string(turn.status),
      items: array(turn.items).map((raw): Item => {
        const item = record(raw);
        return {
          id: identifier(item.id), type: string(item.type, 'unknown'),
          text: typeof item.text === 'string' ? item.text : undefined,
          content: item.content, status: string(item.status),
          command: string(item.command), aggregatedOutput: string(item.aggregatedOutput),
          changes: item.changes, name: string(item.name),
        };
      }),
    };
  });
  return {
    thread: {
      id: identifier(thread.id), name: string(thread.name, '未命名会话'), cwd: string(thread.cwd),
      status: { type: string(record(thread.status).type) }, turns,
      canAcceptDirectInput: typeof thread.canAcceptDirectInput === 'boolean' ? thread.canAcceptDirectInput : null,
    },
    readOnly: data.readOnly === true,
    revision: data.revision,
    activeTurnId: typeof data.activeTurnId === 'string' && data.activeTurnId ? data.activeTurnId : undefined,
    pending,
    historyTruncated: data.historyTruncated === true,
    resyncRequired: data.resyncRequired === true,
    resyncReason: string(data.resyncReason),
  };
}
