import { ApiError, friendlyError, type Api } from './transport.ts';
import { record, sourceName, type ConnectionInfo, type Mutation, type SendMode, type Snapshot, type ThreadSummary,
  type Project, type Model, type Skill, type Turn, type TurnDetails, type MessageInput, type MutationResult, type ActionKind } from './types.ts';
import { approvalContext } from './transcript.ts';
import type { OfflineCache } from './cache.ts';
import type { SyncState } from './sync.ts';

export type LinkState = 'syncing' | 'live' | 'reconnecting' | 'offline' | 'desktop-offline' | 'remote-off';
export interface Notice { kind: 'info' | 'error' | 'success'; text: string }
export interface Draft { text: string; mode: SendMode; targetTurnId?: string; initialized: boolean }
export interface MutationGate { kind: 'unknown' | 'rejected'; text: string }
export interface AppState {
  connection?: ConnectionInfo;
  checkingConnection: boolean;
  link: LinkState;
  threads: ThreadSummary[];
  catalogue: { search: string; nextCursor?: string; loading: boolean; error?: string };
  selectedId?: string;
  snapshot?: Snapshot;
  fresh: boolean; // Full text covers every observed event; required for manual outcome review.
  controlsFresh: boolean; // Action state covers structural events even while text is streaming.
  refreshing: boolean;
  draft: Draft;
  gate?: MutationGate;
  operation?: { threadId: string; action: Mutation['action'] };
  notice?: Notice;
  lastSynced?: number;
  projects: Project[];
  models: Model[];
  skills: Skill[];
  optionsLoading: boolean;
  optionsError?: string;
  newThreadOptions: { loaded: boolean; loading: boolean; error?: string };
  projectId: string;
  config: { model: string; effort: string; skillId?: string; skillName?: string };
  history: { turns: Turn[]; nextCursor?: string; version?: number; loaded: boolean; loading: boolean; error?: string };
  turnDetails: Map<string, TurnDetails>;
  submitting?: MessageInput & { threadId: string };
  cacheError?: string;
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
  cache?: OfflineCache;
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
    catalogue: { search: '', loading: false },
    fresh: false, controlsFresh: false, refreshing: false, draft: emptyDraft(),
    projects: [], models: [], skills: [], optionsLoading: false, projectId: '', config: { model: '', effort: '' },
    newThreadOptions: { loaded: false, loading: false },
    history: { turns: [], loaded: false, loading: false },
    turnDetails: new Map(),
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
  private syncBase?: SyncState;
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
  private readonly cachedSnapshots = new Map<string, Snapshot>();
  private readonly configs = new Map<string, AppState['config']>();
  private cacheTimer?: ReturnType<typeof setTimeout>;
  private historyAbort?: AbortController;
  private readonly itemAborts = new Map<string, AbortController>();
  private optionsVersion = 0;
  private newThreadOptionsAbort?: AbortController;
  private newThreadOptionsTask?: Promise<void>;
  private newThreadRetryTimer?: ReturnType<typeof setTimeout>;
  private newThreadRetryAttempt = 0;
  private newThreadRetryBlocked = false;
  private cacheEnabled = true;
  private readonly cachedThreads = new Map<string, ThreadSummary>();
  private readonly compactions = new Set<string>();
  private foregroundSync?: Promise<void>;

  constructor(api: Api, options: StoreOptions) {
    this.api = api;
    this.options = options;
    this.now = options.now ?? Date.now;
    try {
      const saved = options.cache?.load();
      if (saved) {
        this.state.threads = saved.threads.filter((thread) => !thread.archived);
        for (const thread of this.state.threads) this.cachedThreads.set(thread.id, thread);
        for (const snapshot of saved.snapshots) this.cachedSnapshots.set(snapshot.thread.id, snapshot);
        for (const [id, text] of saved.drafts) this.drafts.set(id, { ...emptyDraft(), text });
        this.state.selectedId = saved.threads.find((thread) => thread.id === saved.selectedId)?.archived ? undefined : saved.selectedId;
        this.state.snapshot = this.state.selectedId ? this.cachedSnapshots.get(this.state.selectedId) : undefined;
        this.state.lastSynced = saved.savedAt;
        this.state.draft = saved.selectedId ? this.drafts.get(saved.selectedId) ?? emptyDraft() : emptyDraft();
        for (const id of saved.uncertain ?? []) this.gates.set(id, { kind: 'unknown', text: '上一次操作的结果尚未核对。请先刷新并确认是否已执行，不会自动重发。' });
      }
    } catch {
      this.state.cacheError = '离线缓存无法读取。联网功能仍可使用，可在连接详情中清除缓存。';
    }
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
    if (this.options.cache && this.cacheEnabled && !this.cacheTimer) this.cacheTimer = setTimeout(() => {
      this.cacheTimer = undefined; this.saveCache();
    }, 400);
  }

  saveCache(): void {
    if (!this.options.cache || !this.cacheEnabled) return;
    try {
      this.options.cache.save({
        threads: [...this.cachedThreads.values()], snapshots: [...this.cachedSnapshots.values()].reverse(),
        drafts: [...this.drafts].map(([id, draft]) => [id, draft.text]),
        selectedId: this.state.selectedId, savedAt: this.now(),
        uncertain: [...this.gates.keys(), ...(this.state.operation ? [this.state.operation.threadId] : [])],
      });
    } catch {
      if (!this.state.cacheError) {
        this.state.cacheError = '无法保存离线缓存，可能是存储空间不足或浏览器限制。草稿目前仅保留在此页。';
        for (const listener of this.listeners) listener();
      }
    }
  }

  clearCache(): void {
    try {
      this.options.cache?.clear(); this.cachedSnapshots.clear(); this.cachedThreads.clear(); this.drafts.clear();
      this.cacheEnabled = false;
      clearTimeout(this.cacheTimer); this.cacheTimer = undefined;
      this.state.cacheError = undefined;
      this.state.notice = { kind: 'success', text: '离线内容已清除。之后加载的内容会重新缓存。' };
    } catch { this.state.cacheError = '浏览器拒绝清除离线数据。请在浏览器的站点设置中清理。'; }
    this.emit();
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

  // Returning to a healthy page verifies state without discarding its stream,
  // event watermarks, or a pending request's outcome protection.
  foreground(): Promise<void> {
    if (this.foregroundSync) return this.foregroundSync;
    if (this.disposed || this.state.checkingConnection) return Promise.resolve();
    this.foregroundSync = (this.state.link === 'live' && this.streamReady ? this.synchronize() : this.recover())
      .finally(() => { this.foregroundSync = undefined; });
    return this.foregroundSync;
  }

  // Online/SSE recovery is not a deliberate review of an uncertain POST.
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

  selectThread(id: string): void {
    if (this.disposed || !id || this.state.selectedId === id) return;
    this.cancelNewThreadOptions();
    this.selectionVersion++;
    this.invalidateRead();
    this.state.selectedId = id;
    this.state.snapshot = this.cachedSnapshots.get(id);
    this.historyAbort?.abort();
    this.state.history = { turns: [], loaded: false, loading: false };
    this.clearTurnDetails();
    this.state.config = this.configs.get(id) ?? { model: '', effort: '' };
    this.state.skills = [];
    this.optionsVersion++;
    this.state.optionsLoading = false;
    this.state.optionsError = undefined;
    this.state.fresh = this.state.controlsFresh = false;
    this.state.notice = undefined;
    if (!this.drafts.has(id)) this.drafts.set(id, emptyDraft());
    this.emit();
    if (id === '@new') void this.loadNewThreadOptions();
    else void this.enqueue(true);
  }

  newThread(): void {
    this.selectThread('@new');
    void this.loadNewThreadOptions();
  }

  setProject(id: string): void { this.state.projectId = id; this.emit(); }

  loadNewThreadOptions(force = false): Promise<void> {
    if (this.disposed || this.state.selectedId !== '@new' || !this.connectionConfirmed ||
      !this.state.connection?.connected || !this.state.connection.sharing) return Promise.resolve();
    if (this.newThreadOptionsTask) return this.newThreadOptionsTask;
    if (!force && (this.state.newThreadOptions.loaded || this.newThreadRetryBlocked || this.newThreadRetryTimer)) return Promise.resolve();
    if (force) { this.newThreadRetryAttempt = 0; this.newThreadRetryBlocked = false; }
    clearTimeout(this.newThreadRetryTimer); this.newThreadRetryTimer = undefined;
    const controller = new AbortController(), selection = this.selectionVersion, lifetime = this.lifetime;
    this.newThreadOptionsAbort = controller;
    const current = () => !controller.signal.aborted && !this.disposed && this.newThreadOptionsAbort === controller &&
      selection === this.selectionVersion && lifetime === this.lifetime && this.state.selectedId === '@new';
    this.state.newThreadOptions = { loaded: false, loading: true };
    this.emit();
    const task = Promise.resolve().then(async () => {
      try {
        if (!current()) return;
        const options = await this.api.newThreadOptions(controller.signal);
        if (!current()) return;
        this.state.projects = options.projects;
        this.state.models = options.models;
        if (!options.projects.some((project) => project.id === this.state.projectId)) {
          this.state.projectId = options.projects[0]?.id ?? '';
        }
        // Only explicit choices are saved in configs for @new. Reloading the
        // defaults must not replace a model/effort the user already selected.
        const saved = this.configs.get('@new'), model = options.models.find((entry) => entry.model === saved?.model);
        if (saved && model) {
          const supported = model.supportedReasoningEfforts.some((entry) => entry.reasoningEffort === saved.effort);
          this.state.config = { ...saved, effort: supported ? saved.effort
            : options.defaults.model === model.model ? options.defaults.effort : model.defaultReasoningEffort };
          this.configs.set('@new', this.state.config);
        } else {
          this.configs.delete('@new');
          this.state.config = { ...options.defaults };
        }
        this.state.newThreadOptions = { loaded: true, loading: false };
        this.newThreadRetryAttempt = 0;
      } catch (error) {
        if (!current()) return;
        this.state.newThreadOptions = { loaded: false, loading: false, error: error instanceof ApiError && error.code === 'resync_required'
          ? '新会话配置暂不可用，请重新加载，或在 Desktop 检查默认模型与思考强度。' : friendlyError(error) };
        if (error instanceof ApiError && error.code === 'sharing_disabled') this.remoteOff();
        else if (error instanceof ApiError && [401, 403].includes(error.status)) {
          this.state.notice = { kind: 'error', text: friendlyError(error) };
          this.resetConnection();
          this.scheduleRetry(true);
        }
        else if (error instanceof ApiError && ['network_error', 'timeout', 'backend_unavailable', 'disconnected',
          'capacity_limited', 'rate_limited', 'state_changed'].includes(error.code) && this.newThreadRetryAttempt < 3) {
          const delay = Math.min(20_000, (this.options.retryDelay ?? 1200) * 2 ** this.newThreadRetryAttempt++);
          this.newThreadRetryTimer = setTimeout(() => {
            this.newThreadRetryTimer = undefined;
            void this.loadNewThreadOptions();
          }, delay);
        } else this.newThreadRetryBlocked = true;
      } finally {
        if (this.newThreadOptionsAbort === controller) {
          this.newThreadOptionsAbort = undefined;
          this.newThreadOptionsTask = undefined;
          this.emit();
        }
      }
    });
    this.newThreadOptionsTask = task;
    return task;
  }

  private cancelNewThreadOptions(): void {
    this.newThreadOptionsAbort?.abort();
    this.newThreadOptionsAbort = undefined;
    this.newThreadOptionsTask = undefined;
    clearTimeout(this.newThreadRetryTimer); this.newThreadRetryTimer = undefined;
    this.newThreadRetryAttempt = 0;
    this.newThreadRetryBlocked = false;
    this.state.newThreadOptions = { loaded: false, loading: false };
  }

  async loadOptions(kind: 'projects' | 'models' | 'skills'): Promise<void> {
    if (this.state.selectedId === '@new') {
      if (kind !== 'skills') await this.loadNewThreadOptions();
      return;
    }
    const id = this.state.selectedId, version = ++this.optionsVersion;
    if (!this.state.connection?.connected || !this.state.connection.sharing) return;
    this.state.optionsLoading = true; this.state.optionsError = undefined; this.emit();
    try {
      let cursor: string | undefined;
      const projects: Project[] = [], models: Model[] = [], skills: Skill[] = [];
      do {
        const page = await this.api.options(kind, id, cursor);
        if (version !== this.optionsVersion || id !== this.state.selectedId) return;
        projects.push(...page.projects); models.push(...page.models); skills.push(...page.skills);
        if (page.warnings.length) this.state.optionsError = page.warnings.join('\n');
        if (page.nextCursor && page.nextCursor === cursor) throw new ApiError(200, 'invalid_response');
        cursor = page.nextCursor;
      } while (cursor);
      if (kind === 'projects') {
        this.state.projects = projects;
        if (!projects.some((project) => project.id === this.state.projectId)) this.state.projectId = projects[0]?.id ?? '';
      } else if (kind === 'models') {
        this.state.models = models;
        if (!this.state.config.model) {
          const model = models.find((m) => m.isDefault) ?? models[0];
          if (model) this.state.config = { ...this.state.config, model: model.model, effort: model.defaultReasoningEffort };
        }
      } else this.state.skills = skills;
    } catch (error) { if (version === this.optionsVersion) this.state.optionsError = friendlyError(error); }
    finally { if (version === this.optionsVersion) { this.state.optionsLoading = false; this.emit(); } }
  }

  async configure(model: string, effort: string): Promise<void> {
    const id = this.state.selectedId;
    if (!id) return;
    const config = { ...this.state.config, model, effort };
    if (id !== '@new') {
      const result = await this.perform({ action: 'actions', kind: 'settings', model, effort });
      if (!result) return;
    }
    this.configs.set(id, config);
    if (this.state.selectedId === id) this.state.config = config;
    this.emit();
  }

  useSkill(skill: Skill): void {
    this.state.config = { ...this.state.config, skillId: skill.id, skillName: skill.name };
    if (this.state.selectedId) this.configs.set(this.state.selectedId, this.state.config);
    this.setDraft(this.state.draft.text || `使用 $${skill.name} `);
  }

  removeSkill(): void {
    this.state.config = { ...this.state.config, skillId: undefined, skillName: undefined };
    if (this.state.selectedId) this.configs.set(this.state.selectedId, this.state.config);
    this.emit();
  }

  async loadHistory(): Promise<void> {
    const id = this.state.selectedId, history = this.state.history;
    if (!id || !history.nextCursor || history.version === undefined || history.loading || this.state.link !== 'live') return;
    const controller = new AbortController(); this.historyAbort = controller;
    history.loading = true; history.error = undefined; this.emit();
    try {
      const page = await this.api.history(id, history.nextCursor, history.version, controller.signal);
      if (controller.signal.aborted || this.state.history !== history || this.state.selectedId !== id) return;
      if (page.historyVersion !== history.version) throw new ApiError(409, 'state_changed');
      history.turns = [...new Map([...page.turns, ...history.turns].map((turn) => [turn.id, turn])).values()];
      history.nextCursor = page.nextCursor; history.loaded = true;
    } catch (error) { if (!controller.signal.aborted) history.error = friendlyError(error); }
    finally { if (this.state.history === history) { history.loading = false; this.emit(); } }
  }

  async loadTurnItems(turnId: string): Promise<void> {
    const id = this.state.selectedId, version = this.state.snapshot?.historyVersion;
    const turn = [...this.state.snapshot?.thread.turns ?? [], ...this.state.history.turns].find((turn) => turn.id === turnId);
    const details = this.state.turnDetails, previous = details.get(turnId);
    if (!id || version === undefined || !turn || turn.status === 'inProgress' ||
      !['summary', 'notLoaded'].includes(turn.itemsView ?? '') || this.state.link !== 'live' ||
      previous?.loading || previous?.loaded && !previous.nextCursor) return;
    const controller = new AbortController(), lifetime = this.lifetime;
    this.itemAborts.set(turnId, controller);
    let value: TurnDetails = { items: previous?.items ?? [], nextCursor: previous?.nextCursor,
      loaded: previous?.loaded ?? false, loading: true };
    const current = () => !controller.signal.aborted && !this.disposed && lifetime === this.lifetime &&
      this.state.selectedId === id && this.state.turnDetails === details && this.state.snapshot?.historyVersion === version;
    details.set(turnId, value); this.emit();
    const cursors = new Set<string>(), seen = new Set(value.items.map((item) => item.id));
    try {
      do {
        const cursor = value.nextCursor ?? '';
        cursors.add(cursor);
        const page = await this.api.items(id, turnId, value.nextCursor, version, controller.signal);
        if (!current()) return;
        if (page.historyVersion !== version || page.turnId !== turnId) throw new ApiError(409, 'state_changed');
        if (page.nextCursor && (cursors.has(page.nextCursor) || !page.items.length) ||
          page.items.some((item) => { if (seen.has(item.id)) return true; seen.add(item.id); return false; })) {
          throw new ApiError(200, 'invalid_response');
        }
        value = { items: [...value.items, ...page.items], nextCursor: page.nextCursor, loaded: true, loading: true };
        details.set(turnId, value); this.emit();
      } while (value.nextCursor && current());
    } catch (error) {
      if (current()) value = { ...value, error: friendlyError(error) };
    } finally {
      if (this.itemAborts.get(turnId) === controller) this.itemAborts.delete(turnId);
      if (this.state.turnDetails === details) { details.set(turnId, { ...value, loading: false }); this.emit(); }
    }
  }

  cancelTurnItems(turnId: string): void { this.itemAborts.get(turnId)?.abort(); }

  private clearTurnDetails(): void {
    for (const controller of this.itemAborts.values()) controller.abort();
    this.itemAborts.clear();
    this.state.turnDetails = new Map();
  }

  setSearch(search: string): void {
    if (this.state.catalogue.search === search) return;
    this.state.catalogue.search = search;
    this.changeQuery();
  }

  private changeQuery(): void {
    this.cancelCatalogue();
    this.state.threads = [];
    this.state.catalogue.nextCursor = undefined;
    this.state.catalogue.error = undefined;
    if (!this.connectionConfirmed || this.state.link === 'offline') {
      const search = this.state.catalogue.search.toLowerCase();
      this.state.threads = [...this.cachedThreads.values()].filter((thread) => !thread.archived &&
        `${thread.name} ${thread.cwd ?? ''} ${sourceName(thread.source)}`.toLowerCase().includes(search));
    }
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
        cursor, search: this.state.catalogue.search, limit: 50,
      }, controller.signal);
      if (!current()) return;
      this.state.threads = cursor
        ? [...new Map([...this.state.threads, ...page.threads].map((thread) => [thread.id, thread])).values()]
        : page.threads;
      for (const thread of page.threads) this.cachedThreads.set(thread.id, thread);
      while (this.cachedThreads.size > 100) this.cachedThreads.delete(this.cachedThreads.keys().next().value!);
      this.cacheEnabled = true;
      this.state.catalogue.nextCursor = page.nextCursor;
      // A page or search result is not an authorization list.
      // Absence never evicts the selected thread, its draft or an uncertain POST.
      if (!this.state.selectedId && page.threads[0]) this.selectThread(page.threads[0].id);
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
    if (!id) return;
    const draft = this.drafts.get(id) ?? emptyDraft();
    this.cacheEnabled = true;
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
    const snapshot = this.state.snapshot;
    if (!snapshot) return '正在确认此会话是否可以接受输入。';
    if (snapshot.readOnly || snapshot.thread.canAcceptDirectInput === false) return '后端不允许此会话接受输入，当前为只读。';
    if (snapshot.thread.canAcceptDirectInput !== true) return '后端尚未确认此会话可接受输入，当前为只读。';
    return '';
  }

  mutationBlock(): string {
    const state = this.state;
    if (!state.connection?.sharing) return state.link === 'remote-off' ? '电脑已关闭远程服务。' : '正在检查远程连接。';
    if (!state.selectedId || (!state.snapshot && state.selectedId !== '@new')) return '请先打开一个会话。';
    if (state.operation) return '正在等待这次操作的结果。';
    if (state.gate) return '请先刷新并核对上一次操作的结果。';
    if (state.snapshot?.resyncRequired) return '请点击“重新同步”，核对最新会话状态。';
    if (!state.connection.connected || state.link === 'desktop-offline') return 'Desktop 暂未连接，草稿会留在此页。';
    if (state.selectedId === '@new') {
      if (state.link !== 'live') return '正在确认连接，暂时不能操作。';
      if (!state.newThreadOptions.loaded) return state.newThreadOptions.error
        ? '新会话配置加载失败，请重新加载。' : '正在加载新会话配置，草稿会留在此页。';
      return '';
    }
    if (!state.controlsFresh || state.link !== 'live') return '正在确认最新状态，暂时不能操作。';
    return this.readOnlyReason();
  }

  async submit(): Promise<void> {
    const state = this.state;
    const reason = this.mutationBlock();
    if (reason || !state.draft.text.trim()) {
      if (reason) this.notify(reason);
      return;
    }
    if (new TextEncoder().encode(state.draft.text).length > 32768) { this.notify('内容过长，请缩短到 32 KB 以内。'); return; }
    const id = state.selectedId!;
    const input: MessageInput = { text: state.draft.text, ...state.config };
    if (state.selectedId === '@new' && !state.projectId) { this.notify('请先选择 Desktop 中已配置的 Project。'); return; }
    const pending = { ...input, threadId: id };
    state.submitting = pending;
    const result = await this.perform(state.selectedId === '@new'
      ? { action: 'create', projectId: state.projectId, ...input } : { action: 'queue', ...input }, input.text);
    if (state.submitting === pending) state.submitting = undefined;
    if (result) {
      const config = this.configs.get(id);
      if (config && config.skillId === input.skillId) {
        const cleared = { ...config, skillId: undefined, skillName: undefined };
        this.configs.set(id, cleared);
        if (state.selectedId === id) state.config = cleared;
      }
    }
    this.emit();
  }

  async reviewCreation(): Promise<boolean> {
    if (this.state.selectedId !== '@new' || this.state.operation || this.state.link !== 'live') return false;
    await this.refreshCatalogue();
    if (this.state.selectedId !== '@new' || this.state.catalogue.error || this.state.link !== 'live') return false;
    this.gates.delete('@new');
    this.notify('已解除新建保护，没有重发任何操作。请避免重复创建已存在的会话。');
    return true;
  }

  notify(text: string, kind: Notice['kind'] = 'info'): void { this.state.notice = { kind, text }; this.emit(); }

  async action(kind: ActionKind, params: { name?: string; turnId?: string; queueId?: string; text?: string } = {}): Promise<boolean> {
    const result = await this.perform({ action: 'actions', kind, ...params,
      ...(kind === 'edit' ? { model: this.state.config.model, effort: this.state.config.effort } : {}) });
    return Boolean(result);
  }

  async withdraw(queueId: string): Promise<void> {
    if (this.state.draft.text.trim()) { this.notify('请先处理输入框中的草稿。'); return; }
    const message = this.state.snapshot?.queue?.find((q) => q.id === queueId);
    const id = this.state.selectedId;
    if (!message || !id) return;
    if (await this.action('queue-delete', { queueId })) {
      const draft = this.drafts.get(id) ?? emptyDraft();
      this.drafts.set(id, { ...draft, text: draft.text ? `${message.text}\n\n${draft.text}` : message.text });
      this.configs.set(id, { model: message.model ?? '', effort: message.effort ?? '', skillId: message.skillId, skillName: message.skillName });
      if (id === this.state.selectedId) this.state.config = this.configs.get(id)!;
      this.emit();
    }
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

  private async perform(mutation: Mutation, originalText?: string): Promise<MutationResult | undefined> {
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
      const result = await this.api.mutate(threadId, csrf, mutation, requestId);
      if (lifetime !== this.lifetime || this.disposed) return;
      const draft = this.drafts.get(threadId);
      if (['messages', 'queue', 'create'].includes(mutation.action) && draft && draft.text === originalText) this.drafts.set(threadId, { ...draft, text: '' });
      if (result?.threadId) {
        if (this.state.selectedId === threadId) this.selectThread(result.threadId);
        void this.refreshCatalogue();
      }
      if (mutation.action === 'actions' && mutation.kind === 'compact') this.compactions.add(threadId);
      if (this.state.selectedId === threadId) {
        // A GET begun before completion is not proof of the post-operation state.
        this.invalidateRead();
        this.state.fresh = this.state.controlsFresh = false;
        this.state.notice = { kind: 'success', text: mutation.action === 'messages' ? '指令已交给 Desktop，正在同步。'
          : mutation.action === 'interrupt' ? '停止请求已交给 Desktop，正在确认。' : '决定已交给 Desktop，正在同步。' };
        void this.enqueue(true);
      }
      return result ?? {};
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
      await Promise.all([this.enqueue(true, review, resyncTarget), this.refreshCatalogue(), this.loadNewThreadOptions()]);
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
      this.cancelNewThreadOptions();
      this.clearTurnDetails();
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
    if (!connection.sharing || !connection.connected) this.cancelNewThreadOptions();
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
    const remaining = (this.options.syncDelay ?? (this.state.connection?.syncVersion === 1 ? 250 : 750)) - (this.now() - this.lastReadStarted);
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
    if (!id || id === '@new' || !this.connectionConfirmed || !this.state.connection?.sharing || !this.state.connection.connected) return;
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
      const options = { resync: job.resync.has(id) };
      const base = this.syncBase?.snapshot.thread.id === id ? this.syncBase : undefined;
      const synced = this.state.connection.syncVersion === 1 && this.api.sync
        ? await this.api.sync(id, base, options, controller.signal) : undefined;
      const snapshot = synced?.snapshot ?? await this.api.snapshot(id, options, controller.signal);
      if (!current()) return;
      if (snapshot.thread.id !== id) throw new ApiError(200, 'invalid_response');
      if (this.appliedGeneration === generation && this.state.snapshot?.thread.id === id && snapshot.revision < this.state.snapshot.revision) {
        throw new ApiError(200, 'stale_snapshot');
      }
      this.appliedGeneration = generation;
      this.syncBase = synced;
      const previous = this.state.snapshot;
      const history = this.state.history;
      const previousIds = new Set(previous?.thread.turns.map((turn) => turn.id));
      const missedWindow = previousIds.size > 0 && snapshot.thread.turns.length > 0 &&
        !snapshot.thread.turns.some((turn) => previousIds.has(turn.id));
      if (history.version !== snapshot.historyVersion || missedWindow) {
        this.historyAbort?.abort();
        this.clearTurnDetails();
        this.state.history = { turns: [], loaded: false, loading: false, version: snapshot.historyVersion, nextCursor: snapshot.historyCursor };
      } else if (!history.loaded) history.nextCursor = snapshot.historyCursor;
      else if (previous) {
        const ids = new Set(snapshot.thread.turns.map((turn) => turn.id));
        const retired = previous.thread.turns.filter((turn) => !ids.has(turn.id));
        if (retired.length) history.turns = [...new Map([...history.turns, ...retired].map((turn) => [turn.id, turn])).values()];
      }
      this.state.snapshot = snapshot;
      this.cacheEnabled = true;
      if (snapshot.resyncRequired && this.compactions.delete(id)) void this.enqueue(true, undefined, id);
      this.cachedSnapshots.delete(id); this.cachedSnapshots.set(id, snapshot);
      while (this.cachedSnapshots.size > 8) this.cachedSnapshots.delete(this.cachedSnapshots.keys().next().value!);
      this.state.config = { ...this.state.config,
        model: snapshot.thread.model || this.state.config.model,
        effort: snapshot.thread.reasoningEffort || this.state.config.effort,
      };
      this.configs.set(id, this.state.config);
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
      else if (error instanceof ApiError && error.code === 'sync_mismatch') {
        this.syncBase = undefined;
        this.state.notice = { kind: 'error', text: '增量状态无法衔接，正在重新读取完整会话。没有重发任何操作。' };
        void this.enqueue(true, job.reviews.get(id));
      }
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
    this.syncBase = undefined;
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
    this.cancelNewThreadOptions();
    this.clearTurnDetails();
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
    this.saveCache();
    this.cancelNewThreadOptions();
    clearTimeout(this.cacheTimer);
    this.historyAbort?.abort();
    this.clearTurnDetails();
    this.disposed = true;
    this.connectionVersion++;
    this.connectionAbort?.abort();
    this.invalidateRead(); this.cancelCatalogue(); this.closeStream(); this.cancelRetry();
    clearTimeout(this.queueTimer); clearInterval(this.pollTimer);
    if (this.queued) for (const done of this.queued.done) done();
    this.queued = undefined;
    this.syncBase = undefined;
    this.drafts.clear(); this.gates.clear(); this.listeners.clear();
    this.state.snapshot = undefined; this.state.connection = undefined;
  }
}
