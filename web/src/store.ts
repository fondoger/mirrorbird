import { ApiError, friendlyError, type Api } from './transport.ts';
import { record, type ConnectionInfo, type Mutation, type SendMode, type Snapshot, type ThreadSummary } from './types.ts';
import { approvalContext } from './transcript.ts';

export type LinkState = 'syncing' | 'live' | 'reconnecting' | 'offline' | 'desktop-offline' | 'remote-off';
export interface Notice { kind: 'info' | 'error' | 'success'; text: string }
export interface Draft { text: string; mode: SendMode; targetTurnId?: string; initialized: boolean }
export interface MutationGate { kind: 'unknown' | 'rejected'; text: string }
export interface AppState {
  connection?: ConnectionInfo;
  checkingConnection: boolean;
  link: LinkState;
  threads: ThreadSummary[];
  catalogue: { search: string; archived: boolean; nextCursor?: string; loading: boolean; error?: string };
  selectedId?: string;
  selectedArchived: boolean;
  snapshot?: Snapshot;
  fresh: boolean; // Full text covers every observed event; required for manual outcome review.
  controlsFresh: boolean; // Action state covers structural events even while text is streaming.
  refreshing: boolean;
  draft: Draft;
  gate?: MutationGate;
  operation?: { threadId: string; action: Mutation['action'] };
  notice?: Notice;
  lastSynced?: number;
}

export interface EventStream {
  addEventListener(type: string, callback: (event: { data?: string }) => void): void;
  close(): void;
}
export interface StoreOptions {
  events: () => EventStream;
  uuid: () => string;
  now?: () => number;
  syncDelay?: number;
  catalogueDelay?: number;
  pollInterval?: number;
  retryDelay?: number;
}
interface Review { id: string; gate: MutationGate; lifetime: number }
interface ReadJob { immediate: boolean; reviews: Map<string, Review>; resync: Set<string>; done: (() => void)[] }
interface CatalogueJob { immediate: boolean; done: (() => void)[] }

const emptyDraft = (): Draft => ({ text: '', mode: 'start', initialized: false });

// Connection proof is kept only in memory. Catalogue queries never own selected
// snapshots, drafts or operation gates; each browser tab has its own store.
export class CompanionStore {
  readonly state: AppState = {
    checkingConnection: false, link: 'syncing', threads: [],
    catalogue: { search: '', archived: false, loading: false }, selectedArchived: false,
    fresh: false, controlsFresh: false, refreshing: false, draft: emptyDraft(),
  };
  private readonly api: Api;
  private readonly options: StoreOptions;
  private readonly now: () => number;
  private readonly listeners = new Set<() => void>();
  private readonly drafts = new Map<string, Draft>();
  private readonly gates = new Map<string, MutationGate>();
  private stream?: EventStream;
  private streamReady = false;
  private connectionAbort?: AbortController;
  private connectionVersion = 0;
  private connectionConfirmed = false;
  private lifetime = 0;
  private readAbort?: AbortController;
  private readVersion = 0;
  private selectionVersion = 0;
  private revisionGeneration = 0;
  private appliedGeneration = -1;
  private readonly eventRevisions = new Map<string, number>();
  private readonly threadInvalidations = new Map<string, number>();
  private readonly structuralRevisions = new Map<string, number>();
  private readonly structuralInvalidations = new Map<string, number>();
  private invalidationVersion = 0;
  private globalInvalidation = 0;
  private reading = false;
  private queued?: ReadJob;
  private queueTimer?: ReturnType<typeof setTimeout>;
  private lastReadStarted = -Infinity;
  private catalogueAbort?: AbortController;
  private catalogueQueryVersion = 0;
  private catalogueTask?: Promise<void>;
  private catalogueQueued?: CatalogueJob;
  private catalogueTimer?: ReturnType<typeof setTimeout>;
  private lastCatalogueStarted = -Infinity;
  private retryTimer?: ReturnType<typeof setTimeout>;
  private retryConnection = false;
  private pollTimer?: ReturnType<typeof setInterval>;
  private retryAttempt = 0;
  private disposed = false;

  constructor(api: Api, options: StoreOptions) {
    this.api = api;
    this.options = options;
    this.now = options.now ?? Date.now;
  }

  subscribe(listener: () => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  private emit(): void {
    if (this.disposed) return;
    const id = this.state.selectedId;
    if (this.state.snapshot?.thread.id !== id) this.state.snapshot = undefined;
    if (!id || !this.state.snapshot || !this.state.connection?.sharing) this.state.fresh = this.state.controlsFresh = false;
    this.state.draft = id ? this.drafts.get(id) ?? emptyDraft() : emptyDraft();
    this.state.gate = id ? this.gates.get(id) : undefined;
    for (const listener of this.listeners) listener();
  }

  async start(): Promise<void> {
    const period = this.options.pollInterval ?? 30_000;
    if (period > 0) this.pollTimer = setInterval(() => {
      if (this.state.link !== 'offline' && !this.state.checkingConnection) void this.synchronize();
    }, period);
    await this.synchronize();
  }

  refresh(): Promise<void> { return this.manualRefresh(false); }
  resync(): Promise<void> { return this.manualRefresh(true); }

  private manualRefresh(resync: boolean): Promise<void> {
    const id = this.state.selectedId;
    const gate = id ? this.gates.get(id) : undefined;
    const review = id && gate ? { id, gate, lifetime: this.lifetime } : undefined;
    this.cancelRetry();
    this.state.fresh = this.state.controlsFresh = false;
    this.invalidateRead();
    this.emit();
    return this.synchronize(review, resync ? id : undefined);
  }

  // Focus/online/SSE recovery is not a deliberate review of an uncertain POST.
  recover(): Promise<void> {
    if (this.disposed) return Promise.resolve();
    this.cancelRetry();
    this.resetConnection();
    return this.synchronize();
  }

  offline(): void {
    this.resetConnection();
    this.state.link = 'offline';
    this.cancelRetry();
    this.emit();
  }

  selectThread(id: string, archived = this.state.threads.find((thread) => thread.id === id)?.archived === true): void {
    if (this.disposed || !id || (this.state.selectedId === id && this.state.selectedArchived === archived)) return;
    this.selectionVersion++;
    this.invalidateRead();
    this.state.selectedId = id;
    this.state.selectedArchived = archived;
    this.state.snapshot = undefined;
    this.state.fresh = this.state.controlsFresh = false;
    this.state.notice = undefined;
    if (!this.drafts.has(id)) this.drafts.set(id, emptyDraft());
    this.emit();
    void this.enqueue(true);
  }

  setSearch(search: string): void {
    if (this.state.catalogue.search === search) return;
    this.state.catalogue.search = search;
    this.changeQuery();
  }

  setArchived(archived: boolean): void {
    if (this.state.catalogue.archived === archived) return;
    this.state.catalogue.archived = archived;
    this.changeQuery();
  }

  private changeQuery(): void {
    this.cancelCatalogue();
    this.state.threads = [];
    this.state.catalogue.nextCursor = undefined;
    this.state.catalogue.error = undefined;
    this.emit();
    void this.refreshCatalogue(false);
  }

  refreshCatalogue(immediate = true): Promise<void> {
    if (this.disposed || !this.connectionConfirmed || !this.state.connection?.sharing) return Promise.resolve();
    return new Promise((resolve) => {
      if (!this.catalogueQueued) this.catalogueQueued = { immediate, done: [] };
      this.catalogueQueued.immediate ||= immediate;
      this.catalogueQueued.done.push(resolve);
      this.wakeCatalogue();
    });
  }

  loadMore(): Promise<void> {
    const cursor = this.state.catalogue.nextCursor;
    if (!cursor || this.catalogueTask || this.catalogueQueued || !this.connectionConfirmed || !this.state.connection?.sharing) return Promise.resolve();
    return this.loadCatalogue(cursor);
  }

  private wakeCatalogue(): void {
    if (this.disposed || this.catalogueTask || !this.catalogueQueued) return;
    clearTimeout(this.catalogueTimer);
    const remaining = (this.options.catalogueDelay ?? 500) - (this.now() - this.lastCatalogueStarted);
    if (!this.catalogueQueued.immediate && remaining > 0) {
      this.catalogueTimer = setTimeout(() => this.wakeCatalogue(), remaining);
      return;
    }
    const job = this.catalogueQueued;
    this.catalogueQueued = undefined;
    void this.loadCatalogue().finally(() => { for (const done of job.done) done(); });
  }

  private loadCatalogue(cursor?: string): Promise<void> {
    const task = this.readCatalogue(cursor).finally(() => {
      if (this.catalogueTask !== task) return;
      this.catalogueTask = undefined;
      this.wakeCatalogue();
    });
    this.catalogueTask = task;
    return task;
  }

  private async readCatalogue(cursor?: string): Promise<void> {
    const version = this.catalogueQueryVersion;
    const lifetime = this.lifetime;
    const controller = new AbortController();
    this.catalogueAbort = controller;
    const current = () => !this.disposed && version === this.catalogueQueryVersion
      && lifetime === this.lifetime && !controller.signal.aborted;
    this.lastCatalogueStarted = this.now();
    this.state.catalogue.loading = true;
    this.state.catalogue.error = undefined;
    this.emit();
    try {
      const page = await this.api.threads({
        cursor, search: this.state.catalogue.search, archived: this.state.catalogue.archived, limit: 50,
      }, controller.signal);
      if (!current()) return;
      this.state.threads = cursor
        ? [...new Map([...this.state.threads, ...page.threads].map((thread) => [thread.id, thread])).values()]
        : page.threads;
      this.state.catalogue.nextCursor = page.nextCursor;
      // A page, search result or archive filter is not an authorization list.
      // Absence never evicts the selected thread, its draft or an uncertain POST.
      if (!this.state.selectedId && page.threads[0]) this.selectThread(page.threads[0].id, page.threads[0].archived === true);
      if (!page.connected) {
        this.globalInvalidation = ++this.invalidationVersion;
        this.state.link = 'desktop-offline';
        this.state.fresh = this.state.controlsFresh = false;
        if (this.state.connection) this.state.connection = { ...this.state.connection, connected: false };
      }
    } catch (error) {
      if (!current()) return;
      this.state.catalogue.error = friendlyError(error);
      if (error instanceof ApiError && error.code === 'sharing_disabled') this.remoteOff();
      // A metadata error is not a transcript error. Do not cancel selected reads.
    } finally {
      if (current()) {
        this.state.catalogue.loading = false;
        if (this.catalogueAbort === controller) this.catalogueAbort = undefined;
        this.emit();
      }
    }
  }

  private cancelCatalogue(): void {
    this.catalogueQueryVersion++;
    this.catalogueAbort?.abort();
    this.catalogueAbort = undefined;
    this.catalogueTask = undefined;
    clearTimeout(this.catalogueTimer);
    if (this.catalogueQueued) for (const done of this.catalogueQueued.done) done();
    this.catalogueQueued = undefined;
    this.state.catalogue.loading = false;
  }

  setDraft(text: string): void {
    const id = this.state.selectedId;
    if (!id || !this.state.connection?.sharing) return;
    const draft = this.drafts.get(id) ?? emptyDraft();
    this.drafts.set(id, { ...draft, text });
    this.emit();
  }

  setMode(mode: SendMode): void {
    const id = this.state.selectedId;
    const active = this.state.snapshot?.activeTurnId;
    if (!id || (mode === 'start' && active) || (mode === 'steer' && !active)) return;
    const draft = this.drafts.get(id) ?? emptyDraft();
    this.drafts.set(id, { ...draft, mode, targetTurnId: mode === 'steer' ? active : undefined, initialized: true });
    this.emit();
  }

  readOnlyReason(): string {
    if (this.state.selectedArchived) return '归档会话为只读，不会自动取消归档。';
    const snapshot = this.state.snapshot;
    if (!snapshot) return '正在确认此会话是否可以接受输入。';
    if (snapshot.readOnly || snapshot.thread.canAcceptDirectInput === false) return '后端不允许此会话接受输入，当前为只读。';
    if (snapshot.thread.canAcceptDirectInput !== true) return '后端尚未确认此会话可接受输入，当前为只读。';
    return '';
  }

  mutationBlock(): string {
    const state = this.state;
    if (!state.connection?.sharing) return state.link === 'remote-off' ? '电脑已关闭远程服务。' : '正在检查远程连接。';
    if (!state.selectedId || !state.snapshot) return '请先打开一个会话。';
    if (state.operation) return '正在等待这次操作的结果。';
    if (state.gate) return '请先刷新并核对上一次操作的结果。';
    if (state.snapshot.resyncRequired) return '请点击“重新同步”，核对最新会话状态。';
    if (!state.connection.connected || state.link === 'desktop-offline') return 'Desktop 暂未连接，草稿会留在此页。';
    if (!state.controlsFresh || state.link !== 'live') return '正在确认最新状态，暂时不能操作。';
    return this.readOnlyReason();
  }

  sendBlock(): string {
    const blocked = this.mutationBlock();
    if (blocked) return blocked;
    const { draft, snapshot } = this.state;
    const active = snapshot?.activeTurnId;
    if (draft.mode === 'start' && active) return 'Desktop 已开始运行，请选择“补充指令”。';
    if (draft.mode === 'steer' && !active) return '这一轮已结束，请选择“开始新轮次”。';
    if (draft.mode === 'steer' && draft.targetTurnId !== active) return '当前轮次已改变，请重新选择“补充指令”确认目标。';
    if (!draft.text.trim()) return '写下要交给 Desktop 的内容。';
    if (new TextEncoder().encode(draft.text).length > 32_768) return '内容过长，请缩短到 32 KB 以内再发送。';
    return '';
  }

  async send(): Promise<void> {
    const reason = this.sendBlock();
    if (reason) { this.state.notice = { kind: 'info', text: reason }; this.emit(); return; }
    const draft = { ...this.state.draft };
    await this.perform({
      action: 'messages', text: draft.text, mode: draft.mode,
      ...(draft.mode === 'steer' ? { expectedTurnId: draft.targetTurnId } : {}),
    }, draft.text);
  }

  async interrupt(): Promise<void> {
    const turnId = this.state.snapshot?.activeTurnId;
    if (this.mutationBlock() || !turnId) return;
    await this.perform({ action: 'interrupt', turnId });
  }

  async respond(id: string, decision: 'accept' | 'decline'): Promise<void> {
    if (this.mutationBlock()) return;
    const pending = this.state.snapshot?.pending.find((request) => request.id === id);
    if (!pending || !approvalContext(pending).canRespond) return;
    await this.perform({ action: 'respond', id, decision });
  }

  private async perform(mutation: Mutation, originalText?: string): Promise<void> {
    const threadId = this.state.selectedId;
    const csrf = this.state.connection?.csrf;
    if (!threadId || !csrf || this.mutationBlock()) return;
    let requestId: string;
    try { requestId = this.options.uuid(); }
    catch {
      this.state.notice = { kind: 'error', text: '此浏览器无法生成安全操作标识。请使用 HTTPS 地址和新版浏览器。' };
      this.emit(); return;
    }
    const lifetime = this.lifetime;
    const operation = { threadId, action: mutation.action };
    this.state.operation = operation;
    this.state.notice = undefined;
    this.emit();
    try {
      await this.api.mutate(threadId, csrf, mutation, requestId);
      if (lifetime !== this.lifetime || this.disposed) return;
      const draft = this.drafts.get(threadId);
      if (mutation.action === 'messages' && draft && draft.text === originalText) this.drafts.set(threadId, { ...draft, text: '' });
      if (this.state.selectedId === threadId) {
        // A GET begun before completion is not proof of the post-operation state.
        this.invalidateRead();
        this.state.fresh = this.state.controlsFresh = false;
        this.state.notice = { kind: 'success', text: mutation.action === 'messages' ? '指令已交给 Desktop，正在同步。'
          : mutation.action === 'interrupt' ? '停止请求已交给 Desktop，正在确认。' : '决定已交给 Desktop，正在同步。' };
        void this.enqueue(true);
      }
    } catch (error) {
      if (lifetime !== this.lifetime || this.disposed) return;
      const unknown = error instanceof ApiError && error.unknownOutcome;
      this.gates.set(threadId, { kind: unknown ? 'unknown' : 'rejected', text: friendlyError(error) });
      this.state.operation = undefined;
      if (this.state.selectedId === threadId) {
        this.invalidateRead();
        this.state.fresh = this.state.controlsFresh = false;
        if (error instanceof ApiError && error.code === 'resync_required' && this.state.snapshot) {
          this.state.snapshot = { ...this.state.snapshot, resyncRequired: true };
        }
      }
      if (error instanceof ApiError && error.code === 'sharing_disabled') this.remoteOff();
      else if (unknown || (error instanceof ApiError && [401, 403].includes(error.status))) {
        this.resetConnection();
        this.scheduleRetry(true);
      }
    } finally {
      if (this.state.operation === operation) this.state.operation = undefined;
      this.emit();
    }
  }

  private async synchronize(review?: Review, resyncTarget?: string): Promise<void> {
    if (this.disposed) return;
    const version = ++this.connectionVersion;
    this.connectionAbort?.abort();
    const controller = new AbortController();
    this.connectionAbort = controller;
    const current = () => !this.disposed && version === this.connectionVersion && !controller.signal.aborted;
    this.state.checkingConnection = true;
    this.emit();
    try {
      const connection = await this.api.connection(controller.signal);
      if (!current()) return;
      this.adoptConnection(connection);
      this.connectionConfirmed = true;
      this.state.checkingConnection = false;
      if (this.connectionAbort === controller) this.connectionAbort = undefined;
      this.emit();
      if (!connection.sharing) return;
      this.openStream();
      // Start independently: a slow catalogue must not delay an already selected
      // snapshot, and a slow transcript must not block searching or pagination.
      await Promise.all([this.enqueue(true, review, resyncTarget), this.refreshCatalogue()]);
    } catch (error) {
      if (!current()) return;
      if (error instanceof ApiError && error.code === 'sharing_disabled') this.remoteOff();
      else {
        if (!this.state.gate) this.state.notice = { kind: 'error', text: friendlyError(error) };
        this.resetConnection();
        this.scheduleRetry(true);
      }
    } finally {
      if (this.connectionAbort === controller) {
        this.connectionAbort = undefined;
        this.state.checkingConnection = false;
        this.emit();
      }
    }
  }

  private adoptConnection(connection: ConnectionInfo): void {
    const old = this.state.connection;
    if (old && (old.sharing !== connection.sharing || old.csrf !== connection.csrf)) {
      this.lifetime++;
      this.selectionVersion++;
      this.invalidateRead();
      this.cancelCatalogue();
      this.beginRevisionGeneration();
      this.closeStream();
      this.state.snapshot = undefined;
      this.state.threads = [];
      this.state.catalogue.nextCursor = undefined;
      this.state.fresh = this.state.controlsFresh = false;
      this.state.notice = undefined;
      this.state.lastSynced = undefined;
      // A stopped service cannot certify that an admitted POST did not execute.
      // Ignore its late response, retain the draft, and require deliberate review.
      if (this.state.operation) this.gates.set(this.state.operation.threadId, {
        kind: 'unknown', text: '远程服务已重新开启或关闭，上一次操作可能已在 Desktop 执行。没有自动重试，请刷新并核对结果。',
      });
      this.state.operation = undefined;
    }
    if (old?.connected && !connection.connected) this.globalInvalidation = ++this.invalidationVersion;
    this.state.connection = connection;
    this.state.link = !connection.sharing ? 'remote-off' : !connection.connected ? 'desktop-offline' : this.streamReady ? 'live' : 'syncing';
    if (!connection.sharing || !connection.connected) this.state.fresh = this.state.controlsFresh = false;
  }

  private remoteOff(): void {
    this.connectionVersion++;
    this.connectionAbort?.abort();
    this.connectionAbort = undefined;
    this.state.checkingConnection = false;
    this.adoptConnection({ sharing: false, connected: this.state.connection?.connected ?? false });
    this.connectionConfirmed = true;
    this.cancelRetry();
    this.emit();
  }

  private enqueue(immediate: boolean, review?: Review, resyncTarget?: string): Promise<void> {
    if (this.disposed || !this.connectionConfirmed || !this.state.connection?.sharing || !this.state.connection.connected) return Promise.resolve();
    return new Promise((resolve) => {
      if (!this.queued) this.queued = { immediate, reviews: new Map(), resync: new Set(), done: [] };
      this.queued.immediate ||= immediate;
      if (review) this.queued.reviews.set(review.id, review);
      if (resyncTarget) this.queued.resync.add(resyncTarget);
      this.queued.done.push(resolve);
      this.wake();
    });
  }

  private wake(): void {
    if (this.disposed || this.reading || !this.queued) return;
    clearTimeout(this.queueTimer);
    const remaining = (this.options.syncDelay ?? 750) - (this.now() - this.lastReadStarted);
    if (!this.queued.immediate && remaining > 0) {
      this.queueTimer = setTimeout(() => this.wake(), remaining);
      return;
    }
    const job = this.queued;
    this.queued = undefined;
    this.reading = true;
    this.lastReadStarted = this.now();
    void this.read(job).finally(() => {
      this.reading = false;
      for (const done of job.done) done();
      this.wake();
    });
  }

  private async read(job: ReadJob): Promise<void> {
    const id = this.state.selectedId;
    if (!id || !this.connectionConfirmed || !this.state.connection?.sharing || !this.state.connection.connected) return;
    const version = ++this.readVersion;
    const selection = this.selectionVersion;
    const lifetime = this.lifetime;
    const generation = this.revisionGeneration;
    const invalidation = this.invalidationVersion;
    const controller = new AbortController();
    this.readAbort = controller;
    const current = () => !this.disposed && version === this.readVersion && selection === this.selectionVersion
      && lifetime === this.lifetime && generation === this.revisionGeneration && id === this.state.selectedId && !controller.signal.aborted;
    this.state.refreshing = true;
    this.emit();
    try {
      const snapshot = await this.api.snapshot(id, { archived: this.state.selectedArchived, resync: job.resync.has(id) }, controller.signal);
      if (!current()) return;
      if (snapshot.thread.id !== id) throw new ApiError(200, 'invalid_response');
      if (this.appliedGeneration === generation && this.state.snapshot?.thread.id === id && snapshot.revision < this.state.snapshot.revision) {
        throw new ApiError(200, 'stale_snapshot');
      }
      this.appliedGeneration = generation;
      this.state.snapshot = snapshot;
      // Finish reads during bursts. Text may lag without invalidating controls,
      // but a pre-structural-event GET must never authorize a subsequent action.
      const latestInvalidation = Math.max(this.globalInvalidation, this.threadInvalidations.get(id) ?? 0);
      const latestStructuralInvalidation = Math.max(this.globalInvalidation, this.structuralInvalidations.get(id) ?? 0);
      this.state.controlsFresh = latestStructuralInvalidation <= invalidation && snapshot.revision >= (this.structuralRevisions.get(id) ?? 0);
      this.state.fresh = latestInvalidation <= invalidation && snapshot.revision >= (this.eventRevisions.get(id) ?? 0);
      if (this.state.fresh && !this.gates.has(id) && this.state.notice?.kind === 'success') {
        this.state.notice = { kind: 'success', text: 'Desktop 已接收操作，最新会话状态已同步。' };
      }
      const draft = this.drafts.get(id) ?? emptyDraft();
      if (this.state.controlsFresh && !draft.initialized) this.drafts.set(id, {
        ...draft, initialized: true, mode: snapshot.activeTurnId ? 'steer' : 'start', targetTurnId: snapshot.activeTurnId,
      });
      const review = job.reviews.get(id);
      if (this.state.fresh && review?.lifetime === lifetime && review.gate === this.gates.get(id) && !snapshot.resyncRequired) {
        this.gates.delete(id);
        this.state.notice = { kind: 'info', text: review.gate.kind === 'unknown'
          ? '已获取最新状态。请先核对消息、工具和审批结果；确认没有执行后，再决定是否重新操作。'
          : '已刷新。请核对当前轮次，再决定下一步。' };
      }
      if (this.state.fresh) { this.state.lastSynced = this.now(); this.cancelRetry(); }
      else if (!this.queued) this.scheduleRetry(false);
      this.state.link = !this.state.connection?.connected ? 'desktop-offline' : this.streamReady ? 'live' : 'syncing';
      if (this.state.fresh && !this.gates.has(id) && this.state.notice?.kind === 'error') this.state.notice = undefined;
    } catch (error) {
      if (!current()) return;
      this.state.fresh = this.state.controlsFresh = false;
      if (error instanceof ApiError && error.code === 'sharing_disabled') this.remoteOff();
      else if (error instanceof ApiError && error.code === 'stale_snapshot') {
        this.state.notice = { kind: 'error', text: '新收到的状态落后于此页，正在重新核对。暂时不能操作。' };
        this.scheduleRetry(false);
      } else if (error instanceof ApiError && ['not_found', 'state_changed', 'resync_required'].includes(error.code)) {
        this.state.notice = { kind: 'error', text: friendlyError(error) };
        if (error.code === 'not_found') this.state.snapshot = undefined;
        if (error.code === 'resync_required' && this.state.snapshot) this.state.snapshot = { ...this.state.snapshot, resyncRequired: true };
      } else {
        if (!this.gates.has(id)) this.state.notice = { kind: 'error', text: friendlyError(error) };
        this.resetConnection();
        this.scheduleRetry(true);
      }
    } finally {
      if (this.readAbort === controller) {
        this.state.refreshing = false;
        this.readAbort = undefined;
        this.emit();
      }
    }
  }

  private invalidateRead(): void {
    this.readVersion++;
    this.readAbort?.abort();
  }

  private beginRevisionGeneration(): void {
    this.revisionGeneration++;
    this.eventRevisions.clear();
    this.threadInvalidations.clear();
    this.structuralRevisions.clear();
    this.structuralInvalidations.clear();
    this.invalidationVersion = 0;
    this.globalInvalidation = 0;
  }

  private closeStream(): void {
    this.stream?.close();
    this.stream = undefined;
    this.streamReady = false;
  }

  private resetConnection(): void {
    this.connectionVersion++;
    this.connectionAbort?.abort();
    this.connectionAbort = undefined;
    this.state.checkingConnection = false;
    this.connectionConfirmed = false;
    this.closeStream();
    this.invalidateRead();
    this.cancelCatalogue();
    this.beginRevisionGeneration();
    this.state.link = 'reconnecting';
    this.state.fresh = this.state.controlsFresh = false;
    this.emit();
  }

  private openStream(): void {
    if (this.stream || this.disposed || !this.state.connection?.sharing) return;
    let stream: EventStream;
    try { stream = this.options.events(); }
    catch { this.resetConnection(); this.scheduleRetry(true); return; }
    this.stream = stream;
    const active = () => this.stream === stream && !this.disposed;
    stream.addEventListener('ready', (event) => {
      if (!active()) return;
      const data = this.eventData(event.data);
      if (typeof data.connected !== 'boolean') { void this.recover(); return; }
      this.streamReady = true;
      this.retryAttempt = 0;
      this.connectionConfirmed = false;
      this.state.link = data.connected ? 'syncing' : 'desktop-offline';
      this.beginRevisionGeneration();
      this.state.fresh = this.state.controlsFresh = false;
      this.emit();
      void this.synchronize();
    });
    stream.addEventListener('change', (event) => {
      if (!active()) return;
      const data = this.eventData(event.data);
      if (data.disconnected === true) { void this.recover(); return; }
      const id = typeof data.threadId === 'string' && data.threadId ? data.threadId : undefined;
      if (data.catalogueChanged === true) {
        void this.refreshCatalogue(false);
        // Go may serialize revision:0 on this metadata-only event. It is not a
        // global structural invalidation and cannot affect selected controls.
        if (!id) return;
      }
      const revision = typeof data.revision === 'number' && Number.isFinite(data.revision) && data.revision >= 0
        ? data.revision : undefined;
      const contentOnly = data.contentOnly === true && id !== undefined && revision !== undefined && Number.isSafeInteger(revision);
      const invalidation = ++this.invalidationVersion;
      if (id) {
        this.threadInvalidations.set(id, invalidation);
        if (revision !== undefined) this.eventRevisions.set(id, Math.max(this.eventRevisions.get(id) ?? 0, revision));
        if (!contentOnly) {
          this.structuralInvalidations.set(id, invalidation);
          if (revision !== undefined) this.structuralRevisions.set(id, Math.max(this.structuralRevisions.get(id) ?? 0, revision));
        }
      } else this.globalInvalidation = invalidation;
      if (!id || id === this.state.selectedId) {
        this.state.fresh = false;
        if (!contentOnly) this.state.controlsFresh = false;
        this.emit();
        // Content bursts share one trailing selected-snapshot GET, never a list.
        void this.enqueue(false);
      }
    });
    stream.addEventListener('reset', () => { if (active()) void this.recover(); });
    stream.addEventListener('error', () => {
      if (!active()) return;
      // Close native reconnect; first re-read the service proof, never replay POST.
      this.resetConnection();
      this.scheduleRetry(true);
    });
  }

  private eventData(data?: string): Record<string, unknown> {
    try { return data && data.length < 4096 ? record(JSON.parse(data)) : {}; }
    catch { return {}; }
  }

  private scheduleRetry(connection: boolean): void {
    if (this.disposed || this.state.link === 'remote-off' || this.state.link === 'offline') return;
    this.retryConnection ||= connection;
    if (this.retryTimer) return;
    const delay = Math.min(20_000, (this.options.retryDelay ?? 1200) * 2 ** Math.min(this.retryAttempt++, 5));
    this.retryTimer = setTimeout(() => {
      const reconnect = this.retryConnection;
      this.retryTimer = undefined;
      this.retryConnection = false;
      if (reconnect) void this.recover();
      else void this.enqueue(true);
    }, delay);
  }

  private cancelRetry(): void {
    clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
    this.retryConnection = false;
  }

  dispose(): void {
    this.disposed = true;
    this.connectionVersion++;
    this.connectionAbort?.abort();
    this.invalidateRead(); this.cancelCatalogue(); this.closeStream(); this.cancelRetry();
    clearTimeout(this.queueTimer); clearInterval(this.pollTimer);
    if (this.queued) for (const done of this.queued.done) done();
    this.queued = undefined;
    this.drafts.clear(); this.gates.clear(); this.listeners.clear();
    this.state.snapshot = undefined; this.state.connection = undefined;
  }
}
