export interface ConnectionInfo {
  sharing: boolean;
  connected: boolean;
  csrf?: string;
  syncVersion?: 1;
}

export function sourceName(_source: unknown): string {
  // Protocol provenance (such as "vscode") is not the connected product.
  return 'Codex';
}

export interface ThreadSummary {
  id: string;
  name: string;
  status: string;
  archived?: boolean;
  cwd?: string;
  updatedAt?: number;
  canAcceptDirectInput?: boolean | null;
  projectId?: string;
  source?: unknown;
}

export interface ThreadList {
  threads: ThreadSummary[];
  nextCursor?: string;
  connected: boolean;
}

export interface ThreadQuery {
  cursor?: string;
  search?: string;
  limit?: number;
}

export interface ReadOptions {
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
  phase?: string;
  summary?: string[];
  commandActions?: unknown;
  tool?: string;
  arguments?: unknown;
  result?: unknown;
  error?: unknown;
  progress?: string;
  exitCode?: number;
}

export interface Turn {
  id: string;
  status: string;
  items: Item[];
  itemsView?: string;
  startedAt?: number;
  completedAt?: number;
  durationMs?: number;
  diff?: string;
  plan?: unknown;
  explanation?: string;
  error?: unknown;
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
    model?: string;
    reasoningEffort?: string;
    projectId?: string;
    source?: unknown;
    updatedAt?: number;
  };
  readOnly?: boolean;
  revision: number;
  activeTurnId?: string;
  pending: PendingRequest[];
  historyTruncated?: boolean;
  resyncRequired?: boolean;
  resyncReason?: string;
  historyCursor?: string;
  historyVersion?: number;
  queue?: QueuedMessage[];
}

export interface MessageInput { text: string; model?: string; effort?: string; skillId?: string; skillName?: string }
export interface QueuedMessage extends MessageInput {
  id: string; status: 'queued' | 'sending' | 'failed'; error?: string; createdAt?: number;
}
export interface Project { id: string; name: string; roots: { path: string }[] }
export interface Model {
  id: string; model: string; displayName: string; description: string; isDefault: boolean;
  defaultReasoningEffort: string; supportedReasoningEfforts: { reasoningEffort: string; description: string }[];
}
export interface Skill { id: string; name: string; description: string }
export interface ComposerOptions { projects: Project[]; models: Model[]; skills: Skill[]; warnings: string[]; nextCursor?: string }
export interface HistoryPage { turns: Turn[]; nextCursor?: string; historyVersion: number }
export interface ItemsPage { turnId: string; items: Item[]; nextCursor?: string; historyVersion: number }
export interface TurnDetails {
  items: Item[];
  nextCursor?: string;
  loaded: boolean;
  loading: boolean;
  error?: string;
}
export interface MutationResult { threadId?: string }
export type ActionKind = 'rename' | 'settings' | 'compact' | 'edit' | 'fork' | 'queue-delete' | 'queue-guide' | 'queue-retry';
export type SendMode = 'start' | 'steer';
export type Mutation =
  | { action: 'messages'; text: string; mode: SendMode; expectedTurnId?: string }
  | { action: 'interrupt'; turnId: string }
  | { action: 'respond'; id: string; decision: 'accept' | 'decline' }
  | ({ action: 'queue' } & MessageInput)
  | ({ action: 'create'; projectId: string } & MessageInput)
  | { action: 'actions'; kind: ActionKind; name?: string; turnId?: string; queueId?: string; text?: string; model?: string; effort?: string };

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
  return { sharing: data.sharing, connected: data.connected, csrf: data.sharing ? string(data.csrf) : undefined,
    ...(data.syncVersion === 1 ? { syncVersion: 1 as const } : {}) };
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
        projectId: string(item.projectId), source: item.source,
      };
    }),
  };
}

export class SnapshotMemo {
  readonly turns = new WeakMap<object, Turn>();
  readonly items = new WeakMap<object, Item>();
  readonly lists = new WeakMap<object, Turn[]>();
  parse(value: unknown): Snapshot { return readSnapshot(value, this); }
}

export function parseSnapshot(value: unknown): Snapshot { return readSnapshot(value); }

function readSnapshot(value: unknown, memo?: SnapshotMemo): Snapshot {
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
  const turns = parseTurns(thread.turns, memo);
  return {
    thread: {
      id: identifier(thread.id), name: string(thread.name, '未命名会话'), cwd: string(thread.cwd),
      status: { type: string(record(thread.status).type) }, turns,
      canAcceptDirectInput: typeof thread.canAcceptDirectInput === 'boolean' ? thread.canAcceptDirectInput : null,
      model: string(thread.model), reasoningEffort: string(thread.reasoningEffort),
      projectId: string(thread.projectId), source: thread.source, updatedAt: finiteNumber(thread.updatedAt),
    },
    readOnly: data.readOnly === true, revision: data.revision,
    activeTurnId: typeof data.activeTurnId === 'string' && data.activeTurnId ? data.activeTurnId : undefined,
    pending, historyTruncated: data.historyTruncated === true,
    resyncRequired: data.resyncRequired === true, resyncReason: string(data.resyncReason),
    historyCursor: string(data.historyCursor) || undefined, historyVersion: finiteNumber(data.historyVersion),
    queue: array(data.queue).map((entry) => {
      const item = record(entry);
      if (!['queued', 'sending', 'failed'].includes(string(item.status))) throw new Error('Invalid queue status');
      return {
        id: identifier(item.id), text: string(item.text),
        status: item.status as QueuedMessage['status'], error: string(item.error),
        model: string(item.model), effort: string(item.effort), skillId: string(item.skillId), skillName: string(item.skillName),
        createdAt: finiteNumber(item.createdAt),
      };
    }),
  };
}

function finiteNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

export function parseTurns(value: unknown, memo?: SnapshotMemo): Turn[] {
  const entries = array(value), cached = memo?.lists.get(entries);
  if (cached) return cached;
  const result = entries.map((entry): Turn => {
    const turn = record(entry);
    const cached = memo?.turns.get(turn);
    if (cached) return cached;
    const parsed: Turn = {
      id: identifier(turn.id),
      status: string(turn.status),
      itemsView: string(turn.itemsView),
      startedAt: finiteNumber(turn.startedAt), completedAt: finiteNumber(turn.completedAt),
      durationMs: finiteNumber(turn.durationMs), diff: string(turn.diff), plan: turn.plan,
      explanation: string(turn.explanation), error: turn.error,
      items: parseItems(turn.items, memo),
    };
    memo?.turns.set(turn, parsed);
    return parsed;
  });
  memo?.lists.set(entries, result);
  return result;
}

function parseItems(value: unknown, memo?: SnapshotMemo): Item[] {
  return array(value).map((raw): Item => {
    const item = record(raw);
    const cached = memo?.items.get(item);
    if (cached) return cached;
    const parsed: Item = {
      id: identifier(item.id), type: string(item.type, 'unknown'),
      text: typeof item.text === 'string' ? item.text : undefined,
      content: item.content, status: string(item.status),
      command: string(item.command), aggregatedOutput: string(item.aggregatedOutput),
      changes: item.changes, name: string(item.name),
      phase: string(item.phase), summary: array(item.summary).map((line) => string(line)),
      commandActions: item.commandActions, tool: string(item.tool), arguments: item.arguments,
      result: item.result, error: item.error, progress: string(item.progress), exitCode: finiteNumber(item.exitCode),
    };
    memo?.items.set(item, parsed);
    return parsed;
  });
}

export function parseItemsPage(value: unknown): ItemsPage {
  const data = record(value), version = finiteNumber(data.historyVersion);
  if (version === undefined || !Array.isArray(data.items) ||
    (data.nextCursor != null && typeof data.nextCursor !== 'string')) throw new Error('Invalid items page');
  return { turnId: identifier(data.turnId), items: parseItems(data.items), historyVersion: version,
    nextCursor: string(data.nextCursor) || undefined };
}

export function parseOptions(value: unknown): ComposerOptions {
  const data = record(value);
  return {
    projects: array(data.projects).map((value) => {
      const p = record(value);
      return { id: identifier(p.id), name: string(p.name), roots: array(p.roots).map((r) => ({ path: string(record(r).path) })) };
    }),
    models: array(data.models).map((value) => {
      const m = record(value);
      return {
        id: identifier(m.id), model: identifier(m.model), displayName: string(m.displayName),
        description: string(m.description), isDefault: m.isDefault === true, defaultReasoningEffort: string(m.defaultReasoningEffort),
        supportedReasoningEfforts: array(m.supportedReasoningEfforts).map((v) => ({
          reasoningEffort: identifier(record(v).reasoningEffort), description: string(record(v).description),
        })),
      };
    }),
    skills: array(data.skills).map((v) => ({ id: identifier(record(v).id), name: string(record(v).name), description: string(record(v).description) })),
    warnings: array(data.warnings).map((v) => string(v)), nextCursor: string(data.nextCursor) || undefined,
  };
}

export function parseHistory(value: unknown): HistoryPage {
  const data = record(value), version = finiteNumber(data.historyVersion);
  if (version === undefined) throw new Error('Invalid history version');
  return { turns: parseTurns(data.turns), historyVersion: version, nextCursor: string(data.nextCursor) || undefined };
}
