import { record, type Snapshot, type SnapshotMemo } from './types.ts';

export interface SyncState {
  cursor: string;
  raw: unknown;
  snapshot: Snapshot;
}

// A patch is applied transactionally to the exact cursor's raw JSON, not to the
// normalized UI model. Copy-on-write preserves untouched turn/item identities.
export function parseSync(value: unknown, id: string, base: SyncState | undefined, memo: SnapshotMemo): SyncState {
  const data = record(value);
  if (typeof data.cursor !== 'string' || data.cursor.length > 128) throw new Error('Invalid sync cursor');
  let raw: unknown;
  if (data.mode === 'snapshot') {
    if (!data.cursor && data.resetReason !== 'capacity') throw new Error('Missing sync cursor');
    raw = data.snapshot;
  } else if (data.mode === 'delta') {
    if (!base || !base.cursor || base.snapshot.thread.id !== id || data.baseCursor !== base.cursor || !data.cursor
      || !Array.isArray(data.patch) || data.patch.length > 2048) throw new Error('Sync baseline mismatch');
    raw = base.raw;
    for (const operation of data.patch) raw = applyPatch(raw, operation);
  } else throw new Error('Invalid sync mode');
  const snapshot = base && raw === base.raw ? base.snapshot : memo.parse(raw);
  if (snapshot.thread.id !== id || (base && snapshot.revision < base.snapshot.revision)) throw new Error('Stale sync state');
  return { cursor: data.cursor, raw, snapshot };
}

function applyPatch(root: unknown, value: unknown): unknown {
  const patch = record(value), path = patch.path;
  if (!Array.isArray(path) || !path.length || path.length > 16
    || path.some((key) => typeof key === 'string' ? !key || ['__proto__', 'prototype', 'constructor'].includes(key)
      : !Number.isSafeInteger(key) || key < 0)) throw new Error('Invalid patch path');
  const transform = (target: unknown): unknown => {
    switch (patch.op) {
      case 'set':
        if (!Object.hasOwn(patch, 'value')) throw new Error('Missing replacement');
        return patch.value;
      case 'append':
        if (typeof target !== 'string' || typeof patch.value !== 'string') throw new Error('Invalid text append');
        return target + patch.value;
      case 'splice': {
        const edit = record(patch.value);
        const index = edit.index, count = edit.deleteCount;
        if (!Array.isArray(target) || typeof index !== 'number' || typeof count !== 'number'
          || !Number.isSafeInteger(index) || !Number.isSafeInteger(count) || index < 0 || count < 0
          || index > target.length || count > target.length - index || !Array.isArray(edit.items)
          || target.length - count + edit.items.length > 4096) throw new Error('Invalid list edit');
        return [...target.slice(0, index), ...edit.items, ...target.slice(index + count)];
      }
      default: throw new Error('Invalid patch operation');
    }
  };
  const visit = (target: unknown, depth: number): unknown => {
    const key: unknown = path[depth], last = depth === path.length - 1;
    if (Array.isArray(target)) {
      if (typeof key !== 'number' || key >= target.length || patch.op === 'remove' && last) throw new Error('Invalid list index');
      const copy: unknown[] = target.slice();
      copy[key] = last ? transform(target[key]) : visit(target[key], depth + 1);
      return copy;
    }
    if (!target || typeof target !== 'object' || typeof key !== 'string') throw new Error('Invalid object path');
    const object = record(target), exists = Object.hasOwn(object, key);
    if (!exists && (!last || patch.op !== 'set')) throw new Error('Missing patch target');
    const copy = { ...object };
    if (last && patch.op === 'remove') delete copy[key];
    else copy[key] = last ? transform(object[key]) : visit(object[key], depth + 1);
    return copy;
  };
  return visit(root, 0);
}
