import { parseSnapshot, parseThreadList, record, string, type Snapshot, type ThreadSummary } from './types.ts';

export interface CachedWorkspace {
  threads: ThreadSummary[];
  snapshots: Snapshot[];
  drafts: [string, string][];
  selectedId?: string;
  savedAt: number;
  uncertain?: string[];
}

const key = 'mirrorbird.workspace.v1';
const maxBytes = 2_000_000;
const maxAge = 7 * 24 * 60 * 60 * 1000;

// Cache contains only read-only views and drafts, never CSRF proofs or executable requests.
export class OfflineCache {
  private readonly storage: Storage;
  constructor(storage: Storage) { this.storage = storage; }

  load(): CachedWorkspace | undefined {
    const text = this.storage.getItem(key);
    if (!text) return undefined;
    if (text.length > maxBytes) throw new Error('离线缓存过大，请清除离线数据。');
    const data = record(JSON.parse(text));
    if (typeof data.savedAt !== 'number' || Date.now() - data.savedAt > maxAge) {
      this.clear(); return undefined;
    }
    const snapshots = Array.isArray(data.snapshots) ? data.snapshots.slice(0, 8).map(parseSnapshot) : [];
    const drafts: [string, string][] = [];
    if (Array.isArray(data.drafts)) for (const value of data.drafts.slice(0, 40)) {
      if (Array.isArray(value) && typeof value[0] === 'string' && typeof value[1] === 'string' && value[1].length <= 32768) {
        drafts.push([value[0], value[1]]);
      }
    }
    return {
      threads: parseThreadList({ threads: data.threads, connected: false }).threads.slice(0, 100),
      snapshots, drafts, selectedId: string(data.selectedId) || undefined, savedAt: data.savedAt,
      uncertain: Array.isArray(data.uncertain) ? data.uncertain.filter((id): id is string => typeof id === 'string').slice(0, 40) : [],
    };
  }

  save(workspace: CachedWorkspace): void {
    const snapshots: Snapshot[] = [];
    for (const source of workspace.snapshots.slice(0, 8)) {
      const snapshot = { ...source, pending: [], thread: { ...source.thread, turns: source.thread.turns.slice(-8) } };
      if (JSON.stringify(snapshot).length <= 200_000) snapshots.push(snapshot);
    }
    const data = {
      threads: workspace.threads.slice(0, 100), snapshots,
      drafts: workspace.drafts.filter(([, text]) => text).slice(-40), selectedId: workspace.selectedId, savedAt: Date.now(),
      uncertain: workspace.uncertain?.slice(0, 40),
    };
    let encoded = JSON.stringify(data);
    while (encoded.length > maxBytes && data.snapshots.length) {
      data.snapshots.pop(); encoded = JSON.stringify(data);
    }
    if (encoded.length > maxBytes) throw new Error('草稿缓存已满，请清理不再需要的草稿。');
    this.storage.setItem(key, encoded);
  }

  clear(): void { this.storage.removeItem(key); }
}
