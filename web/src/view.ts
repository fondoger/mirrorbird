import { button, el, icon, iconButton, setText, textButton, type IconName } from './dom.ts';
import { CompanionStore, type AppState } from './store.ts';
import { approvalContext, messageRows, messageText, pendingTitle, updateTurn, patchElement } from './transcript.ts';
import { sourceName, type Item, type Turn } from './types.ts';

const linkLabels: Record<AppState['link'], string> = {
  live: '已连接', syncing: '正在同步', reconnecting: '重新连接中',
  offline: '连接中断', 'desktop-offline': 'Desktop 未连接', 'remote-off': '共享已关闭',
};
const effortLabels: Record<string, string> = { none: '无', minimal: '极低', low: '低', medium: '中等', high: '高', xhigh: '极高', ultra: 'Ultra' };
const projectName = (path = '') => path.replace(/[/\\]+$/, '').split(/[/\\]/).at(-1) || '未指定项目';
interface MessageTarget { threadId: string; turn: Turn; item: Item; editable: boolean }

export class CompanionView {
  private readonly store: CompanionStore;
  private readonly root: HTMLElement;
  private readonly app = el('div', 'app');
  private readonly sidebar = el('aside', 'sidebar');
  private readonly main = el('main', 'main');
  private readonly list = el('div', 'session-list');
  private readonly search = el('input');
  private readonly moreThreads = button('加载更多', 'load-more', () => void this.store.loadMore());
  private readonly listStatus = el('p', 'list-end');
  private readonly listContent = el('div');
  private readonly scrim = button('', 'drawer-scrim', () => this.closeDrawer());
  private readonly drawerButton = iconButton('打开会话列表', 'menu', () => this.openDrawer(), 'icon-btn mobile-nav');
  private readonly title = el('h1', 'topbar-title', 'MirrorBird');
  private readonly subtitle = el('div', 'topbar-subtitle');
  private readonly connection = button('', 'connection', () => this.openConnection());
  private readonly connectionLabel = el('span', 'connection-label');
  private readonly more = iconButton('更多会话操作', 'more', () => this.openMenu(), 'icon-btn');
  private readonly reconnect = el('div', 'reconnect-notice');
  private readonly reconnectText = el('span');
  private readonly scroller = el('section', 'conversation');
  private readonly thread = el('div', 'thread');
  private readonly history = button('下拉加载更早的消息', 'history-loader', () => void this.store.loadHistory());
  private readonly messages = el('div', 'messages');
  private readonly approvals = el('section', 'approvals');
  private readonly newConversation = el('section', 'new-conversation');
  private readonly projectSelect = el('select');
  private readonly projectError = el('p', 'popup-error');
  private readonly retryProjects = button('重新加载项目', 'btn btn-secondary', () => void this.store.loadOptions('projects'));
  private readonly empty = el('div', 'empty-state');
  private readonly latest = textButton('回到最新', 'arrow-down', () => this.scrollToBottom(), 'latest-button');
  private readonly queue = el('section', 'pending-queue');
  private readonly textarea = el('textarea');
  private readonly send = el('button', 'send-button');
  private readonly add = iconButton('会话操作与 Skills', 'plus', () => this.openComposer('skills'), 'icon-btn composer-add');
  private readonly model = button('模型', 'composer-choice', () => this.openComposer('models'));
  private readonly effort = button('思考强度', 'composer-choice', () => this.openComposer('effort'));
  private readonly skill = button('', 'skill-chip', () => this.store.removeSkill());
  private readonly notice = el('div', 'operation-notice');
  private readonly noticeText = el('span');
  private readonly review = button('重新同步', 'notice-action', () => {
    if (this.store.state.selectedId === '@new') {
      this.confirm('已核对新建结果？', '请先在会话列表中确认是否已经创建成功。这只会解除新建保护，不会自动重发。再次发送可能创建重复会话。', '已核对，解除保护', () => this.store.reviewCreation());
    } else void (this.store.state.snapshot?.resyncRequired ? this.store.resync() : this.store.refresh());
  });
  private readonly popup = el('div', 'composer-popup');
  private readonly messageMenu = el('div', 'message-popover');
  private popupKind?: 'models' | 'effort' | 'skills' | 'menu';
  private popupAnchor?: HTMLElement;
  private readonly info = el('dialog', 'app-dialog');
  private readonly infoSummary = el('div', 'connection-summary');
  private readonly infoSync = el('strong');
  private readonly infoCache = el('p', 'dialog-note');
  private readonly live = el('div', 'toast');
  private toastTimer?: ReturnType<typeof setTimeout>;
  private lastNotice = '';
  private lastSelection?: string;
  private lastList = '';
  private lastQueue = '';
  private lastApprovals = '';
  private lastProjects = '';
  private readonly nodes = new Map<string, { turn: Turn; node: HTMLElement }>();
  private readonly disclosures = new Set<string>();
  private readonly scrolls = new Map<string, number>();
  private drawerOrigin?: HTMLElement;
  private messagePointer?: { x: number; y: number; time: number };
  private messageMenuShell?: HTMLElement;
  private selectionBoundary?: HTMLElement;
  private pullY?: number;

  constructor(root: HTMLElement, store: CompanionStore) {
    this.root = root; this.store = store;
    this.buildSidebar(); this.buildMain(); this.buildConnection();
    this.popup.popover = 'auto'; this.popup.setAttribute('role', 'dialog');
    this.messageMenu.popover = 'auto'; this.messageMenu.setAttribute('role', 'menu'); this.messageMenu.setAttribute('aria-label', '消息操作');
    this.popup.addEventListener('toggle', () => {
      if (!this.popup.matches(':popover-open')) {
        this.popupAnchor?.setAttribute('aria-expanded', 'false'); this.popupKind = undefined;
      }
    });
    this.live.hidden = true; this.live.setAttribute('role', 'status');
    this.scrim.setAttribute('aria-label', '关闭会话列表'); this.scrim.tabIndex = -1;
    this.app.append(this.sidebar, this.scrim, this.main);
    this.root.append(this.app, this.popup, this.messageMenu, this.info, this.live);
    this.bindMessages(); this.bindKeyboard();
    window.addEventListener('resize', () => {
      if (matchMedia('(min-width:768px)').matches) this.closeDrawer();
      this.positionPopup();
    });
    window.visualViewport?.addEventListener('resize', () => this.positionPopup());
    window.visualViewport?.addEventListener('scroll', () => this.positionPopup());
    store.subscribe(() => this.render()); this.render();
  }

  private buildSidebar(): void {
    this.sidebar.setAttribute('aria-label', '会话导航');
    const brand = el('div', 'brand');
    const logo = el('img', 'brand-logo'); logo.src = '/favicon.svg'; logo.alt = '';
    brand.append(logo, el('span', 'brand-name', 'MirrorBird'),
      iconButton('关闭会话列表', 'close', () => this.closeDrawer(), 'icon-btn sidebar-close'));
    const create = textButton('新建会话', 'plus', () => {
      this.store.newThread(); this.closeDrawer(); void this.store.loadOptions('projects');
    }, 'new-session-button');
    const label = el('label', 'search-label'); label.append(icon('search'));
    this.search.type = 'search'; this.search.placeholder = '搜索会话'; this.search.autocomplete = 'off'; this.search.maxLength = 512;
    this.search.setAttribute('aria-label', '搜索标题、项目或来源');
    this.search.addEventListener('input', () => this.store.setSearch(this.search.value));
    label.append(this.search, el('span', 'search-key', navigator.platform.includes('Mac') ? '⌘ K' : 'Ctrl K'));
    const heading = el('div', 'recent-heading');
    heading.append(el('h2', '', '最近会话'));
    this.list.append(this.listContent, this.moreThreads, this.listStatus);
    this.sidebar.append(brand, create, label, heading, this.list);
  }

  private buildMain(): void {
    const topbar = el('header', 'topbar'), title = el('div', 'topbar-title-block'), actions = el('div', 'topbar-actions');
    title.append(this.title, this.subtitle);
    this.connection.append(el('span', 'live-dot'), this.connectionLabel, icon('link'));
    actions.append(this.connection, this.more); topbar.append(this.drawerButton, title, actions);
    this.drawerButton.setAttribute('aria-expanded', 'false');
    this.reconnect.setAttribute('role', 'status');
    this.reconnect.append(this.reconnectText, button('重新连接', '', () => void this.store.recover()));
    this.scroller.tabIndex = 0; this.scroller.setAttribute('aria-label', '会话消息');
    this.newConversation.append(el('h2', '', '新会话'));
    const projectField = el('div', 'project-field'), projectLabel = el('label', '', 'Project');
    this.projectSelect.id = 'projectSelect'; projectLabel.htmlFor = this.projectSelect.id;
    this.projectSelect.setAttribute('aria-label', '选择 Desktop 已配置的 Project');
    this.projectSelect.addEventListener('change', () => this.store.setProject(this.projectSelect.value));
    projectField.append(projectLabel, this.projectSelect);
    this.projectError.setAttribute('role', 'status');
    this.newConversation.append(projectField, this.projectError, this.retryProjects,
      el('p', 'dialog-note', '在电脑上已配置的项目中开始，沿用 Desktop 的权限设置。'));
    this.thread.append(this.history, this.newConversation, this.empty, this.messages, this.approvals);
    this.scroller.append(this.thread);
    this.scroller.addEventListener('scroll', () => {
      this.messagePointer = undefined; this.closeMessageMenu(); this.latest.hidden = this.nearBottom();
      if (this.store.state.selectedId) this.scrolls.set(this.store.state.selectedId, this.scroller.scrollTop);
    }, { passive: true });
    this.scroller.addEventListener('touchstart', (event) => {
      this.pullY = this.scroller.scrollTop <= 2 ? event.touches[0]?.clientY : undefined;
    }, { passive: true });
    this.scroller.addEventListener('touchend', (event) => {
      if (this.pullY !== undefined && (event.changedTouches[0]?.clientY ?? 0) - this.pullY > 64) void this.store.loadHistory();
      this.pullY = undefined;
    }, { passive: true });
    const footer = el('footer', 'composer-wrap'), inner = el('div', 'composer-inner'), form = el('form', 'composer');
    this.queue.setAttribute('aria-label', '等待消息队列');
    this.notice.setAttribute('role', 'status'); this.notice.append(this.noticeText, this.review);
    this.textarea.rows = 2; this.textarea.maxLength = 32768; this.textarea.setAttribute('aria-label', '消息输入');
    this.textarea.setAttribute('enterkeyhint', 'enter');
    this.textarea.addEventListener('input', () => this.store.setDraft(this.textarea.value));
    this.textarea.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey && !event.isComposing &&
        (event.ctrlKey || event.metaKey || matchMedia('(min-width:768px) and (pointer:fine)').matches)) {
        event.preventDefault(); void this.store.submit();
      }
    });
    const foot = el('div', 'composer-foot'), choices = el('div', 'composer-options');
    for (const node of [this.add, this.model, this.effort]) {
      node.setAttribute('aria-haspopup', 'dialog'); node.setAttribute('aria-expanded', 'false');
    }
    choices.append(this.model, this.effort, this.send); foot.append(this.add, choices);
    this.skill.setAttribute('aria-label', '移除所选 Skill');
    form.append(this.skill, this.textarea, foot);
    form.addEventListener('submit', (event) => { event.preventDefault(); void this.store.submit(); });
    this.send.addEventListener('click', () => { if (this.send.dataset.action === 'stop') this.confirmStop(); });
    inner.append(this.notice, this.queue, form); footer.append(inner);
    this.latest.hidden = true;
    this.main.append(topbar, this.reconnect, this.scroller, this.latest, footer);
  }

  private buildConnection(): void {
    const head = this.dialogHead(this.info, '连接详情');
    const body = el('div', 'dialog-body');
    body.append(this.infoSummary);
    for (const [name, value] of [['会话来源', 'ChatGPT Desktop / Codex'], ['访问方式', location.hostname.endsWith('.devtunnels.ms') ? 'Microsoft 私有 Dev Tunnel' : '当前站点']]) {
      const row = el('div', 'connection-row'); row.append(el('span', '', name), el('strong', '', value)); body.append(row);
    }
    const sync = el('div', 'connection-row'); sync.append(el('span', '', '最近同步'), this.infoSync); body.append(sync);
    body.append(el('p', 'dialog-note', '会话在电脑上运行。网页断开不会停止任务；已排队的消息仍会依次执行。停止共享、退出 MirrorBird 或断开 Desktop 会取消尚未执行的队列。'),
      el('p', 'dialog-note', '此设备会缓存少量会话和草稿，离线时可阅读与复制。不会缓存登录凭据或自动重发操作。请只在可信设备上使用。'), this.infoCache);
    const actions = el('div', 'dialog-actions');
    actions.append(button('清除离线数据', 'btn btn-secondary', () => this.confirm('清除离线内容？', '清除这台设备缓存的会话和草稿，不会删除 Desktop 上的内容。', '清除', async () => { this.store.clearCache(); return true; })),
      button('重新连接', 'btn btn-primary', () => { this.info.close(); void this.store.recover(); }));
    this.info.append(head, body, actions);
  }

  render(): void {
    const state = this.store.state, snapshot = state.snapshot, id = state.selectedId;
    const changedSelection = id !== this.lastSelection;
    if (changedSelection) {
      this.closePopup(); this.closeMessageMenu(); this.nodes.clear();
      this.messages.replaceChildren(); this.lastQueue = this.lastApprovals = '';
      this.lastSelection = id;
    }
    this.renderList(state);
    const selected = state.threads.find((thread) => thread.id === id);
    setText(this.title, id === '@new' ? '新会话' : snapshot?.thread.name || selected?.name || 'MirrorBird');
    const path = snapshot?.thread.cwd || selected?.cwd || '';
    const source = snapshot?.thread.source ?? selected?.source;
    setText(this.subtitle, `${sourceName(source)} · ${projectName(path)}`);
    this.subtitle.title = path;
    const online = state.link === 'live';
    this.connection.classList.toggle('offline', !online);
    setText(this.connectionLabel, linkLabels[state.link]);
    this.connection.setAttribute('aria-label', `${linkLabels[state.link]}，查看连接详情`);
    this.reconnect.hidden = online;
    setText(this.reconnectText, state.link === 'offline' || state.link === 'reconnecting'
      ? '网络连接中断，已加载的内容和草稿仍保留' : linkLabels[state.link]);
    this.infoSummary.classList.toggle('offline', !online);
    setText(this.infoSummary, online ? '电脑与网页已连接' : `${linkLabels[state.link]} · 显示最近同步的内容`);
    setText(this.infoSync, state.lastSynced ? new Date(state.lastSynced).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '尚未同步');
    setText(this.infoCache, state.cacheError || '离线缓存最多保留 8 个近期会话，每个会话保留少量消息，7 天后过期。');
    const newThread = id === '@new';
    this.newConversation.hidden = !newThread;
    const projectsSignature = JSON.stringify([state.projects, state.projectId, state.optionsLoading, state.optionsError]);
    if (projectsSignature !== this.lastProjects) {
      this.lastProjects = projectsSignature; this.projectSelect.replaceChildren();
      if (!state.projects.length) {
        const option = el('option', '', state.optionsLoading ? '正在加载项目…' : state.optionsError ? '项目加载失败' : '暂无已配置的 Project');
        option.value = ''; this.projectSelect.append(option);
      }
      for (const project of state.projects) {
        const option = el('option', '', project.name); option.value = project.id; this.projectSelect.append(option);
      }
      this.projectSelect.value = state.projectId;
    }
    this.projectSelect.disabled = state.optionsLoading || !online;
    setText(this.projectError, state.optionsError ?? '');
    this.projectError.hidden = !state.optionsError;
    this.retryProjects.hidden = !state.optionsError;
    this.retryProjects.disabled = state.optionsLoading || !online;
    this.history.hidden = !state.history.nextCursor && !state.history.error;
    this.history.disabled = !online || state.history.loading;
    setText(this.history, state.history.loading ? '正在加载历史…' : state.history.error ? `${state.history.error} 点击重试` : '下拉或点击加载更早的消息');
    this.empty.hidden = newThread || Boolean(snapshot?.thread.turns.length);
    setText(this.empty, snapshot ? '在这个会话中开始交流。' : id ? '正在连接并加载会话…' : '打开一个会话，继续电脑上的工作。');
    if (snapshot) this.renderTranscript(state, changedSelection);
    this.renderApprovals(state); this.renderQueue(state); this.renderComposer(state);
    this.updateMessageMenu();
    if (this.popupKind) this.renderPopup();
    const notice = state.gate?.text || state.notice?.text || state.cacheError || '';
    if (notice && notice !== this.lastNotice) {
      this.lastNotice = notice; setText(this.live, notice); this.live.hidden = false;
      clearTimeout(this.toastTimer); this.toastTimer = setTimeout(() => { this.live.hidden = true; }, 4500);
    }
  }

  private renderList(state: AppState): void {
    if (this.search.value !== state.catalogue.search) this.search.value = state.catalogue.search;
    this.moreThreads.hidden = !state.catalogue.nextCursor;
    this.moreThreads.disabled = state.catalogue.loading || state.link !== 'live';
    setText(this.moreThreads, state.catalogue.loading ? '正在加载…' : '加载更多');
    setText(this.listStatus, state.catalogue.error || (state.catalogue.loading ? '正在加载…' : state.catalogue.nextCursor ? '' : '已加载全部会话'));
    const signature = JSON.stringify([state.threads, state.selectedId]);
    if (signature === this.lastList) return;
    this.lastList = signature;
    const scroll = this.list.scrollTop; this.listContent.replaceChildren();
    if (!state.threads.length) this.listContent.append(el('div', 'empty-state', state.catalogue.search ? '没有找到会话，试试其他标题或项目名。' : '暂无可用会话'));
    let lastGroup = '';
    const today = new Date(); today.setHours(0, 0, 0, 0);
    for (const thread of state.threads) {
      const date = (thread.updatedAt ?? 0) * 1000;
      const group = date >= today.getTime() ? '今天' : date >= today.getTime() - 86400000 ? '昨天' : '更早';
      if (group !== lastGroup) { this.listContent.append(el('div', 'group-label', group)); lastGroup = group; }
      const selected = thread.id === state.selectedId, active = ['active', 'inProgress'].includes(thread.status);
      const node = button('', `session${selected ? ' selected' : ''}`, () => { this.store.selectThread(thread.id); this.closeDrawer(); });
      if (selected) node.setAttribute('aria-current', 'page');
      const mark = el('span', 'session-icon'); mark.append(icon('threads'));
      const content = el('span', 'session-content'), meta = el('span', 'session-meta');
      if (active) meta.append(el('span', 'activity-dot'));
      const time = date ? new Date(date).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : '历史会话';
      meta.append(el('span', '', active ? '正在执行' : group === '今天' ? time : group), el('span', '', '·'), el('span', '', sourceName(thread.source)));
      content.append(el('span', 'session-title', thread.name), meta);
      node.append(mark, content); this.listContent.append(node);
    }
    this.list.scrollTop = scroll;
  }

  private renderTranscript(state: AppState, changedSelection: boolean): void {
    const snapshot = state.snapshot!;
    const bottom = this.nearBottom(), previousHeight = this.scroller.scrollHeight, previousTop = this.scroller.scrollTop;
    const rows = messageRows(snapshot, state.history.turns);
    const wanted = new Set(rows.map((row) => row.key));
    for (const [key, item] of this.nodes) if (!wanted.has(key)) { item.node.remove(); this.nodes.delete(key); }
    let cursor: ChildNode | null = this.messages.firstChild;
    for (const row of rows) {
      let existing = this.nodes.get(row.key);
      if (!existing || (existing.turn !== row.turn && !updateTurn(existing.node, existing.turn, row.turn)
        && JSON.stringify(existing.turn) !== JSON.stringify(row.turn))) {
        let node = row.build();
        for (const detail of node.querySelectorAll<HTMLDetailsElement>('details[data-disclosure]')) {
          detail.open = this.disclosures.has(`${state.selectedId}:${detail.dataset.disclosure}`);
        }
        if (existing) {
          patchElement(existing.node, node);
          node = existing.node;
        }
        existing = { turn: row.turn, node }; this.nodes.set(row.key, existing);
      }
      existing.turn = row.turn;
      if (existing.node !== cursor) this.messages.insertBefore(existing.node, cursor);
      cursor = existing.node.nextSibling;
    }
    const latestTurn = snapshot.thread.turns.at(-1);
    const blocked = Boolean(this.store.mutationBlock());
    for (const shell of this.messages.querySelectorAll<HTMLElement>('[data-message]')) {
      const turnID = shell.closest<HTMLElement>('[data-turn]')?.dataset.turn;
      const edit = shell.querySelector<HTMLButtonElement>('[data-message-action="edit"]');
      if (edit) {
        edit.hidden = !shell.dataset.editable || turnID !== latestTurn?.id || Boolean(snapshot.activeTurnId || snapshot.queue?.length || snapshot.readOnly);
        edit.disabled = blocked;
      }
      const fork = shell.querySelector<HTMLButtonElement>('[data-message-action="fork"]');
      if (fork) fork.disabled = blocked || turnID === snapshot.activeTurnId;
    }
    for (const spinner of this.messages.querySelectorAll('.current-operation .icon')) spinner.classList.toggle('spin', state.link === 'live');
    if (changedSelection) this.scroller.scrollTop = this.scrolls.get(state.selectedId ?? '') ?? this.scroller.scrollHeight;
    else if (bottom) this.scroller.scrollTop = this.scroller.scrollHeight;
    else if (previousTop < 100) this.scroller.scrollTop = previousTop + this.scroller.scrollHeight - previousHeight;
    this.latest.hidden = this.nearBottom();
  }

  private renderApprovals(state: AppState): void {
    const signature = JSON.stringify([state.snapshot?.pending, this.store.mutationBlock()]);
    if (signature === this.lastApprovals) return;
    this.lastApprovals = signature; this.approvals.replaceChildren();
    for (const request of state.snapshot?.pending ?? []) {
      const context = approvalContext(request), block = el('section', 'approval'), body = el('div', 'approval-body');
      const title = el('h2', 'approval-title'); title.append(icon('lock'), el('span', '', pendingTitle(request)));
      body.append(title, el('pre', 'command', context.context));
      if (context.explanation) body.append(el('p', 'approval-copy', context.explanation));
      block.append(body);
      if (context.canRespond) {
        const actions = el('div', 'approval-actions');
        const deny = button('拒绝', 'btn btn-secondary', () => void this.store.respond(request.id, 'decline'));
        const allow = button('允许本次', 'btn btn-primary', () => void this.store.respond(request.id, 'accept'));
        deny.disabled = allow.disabled = Boolean(this.store.mutationBlock()); actions.append(deny, allow); block.append(actions);
      }
      this.approvals.append(block);
    }
  }

  private renderQueue(state: AppState): void {
    const queue = state.snapshot?.queue ?? [], blocked = Boolean(this.store.mutationBlock());
    const submitting = state.submitting?.threadId === state.selectedId ? state.submitting : undefined;
    const signature = JSON.stringify([queue, submitting, blocked, Boolean(state.draft.text.trim()), state.link]);
    if (signature === this.lastQueue) return;
    this.lastQueue = signature;
    const expanded = new Set([...this.queue.querySelectorAll<HTMLElement>('[aria-expanded="true"]')].map((node) => node.dataset.queueId));
    this.queue.hidden = !queue.length && !submitting;
    this.queue.replaceChildren(el('h2', 'pending-heading', state.link === 'live' ? `等待消息 · ${queue.length}` : '等待消息 · 离线快照'));
    for (const [index, message] of queue.entries()) {
      const item = el('article', 'pending-item'), text = button(message.text, 'pending-text', () => {
        text.setAttribute('aria-expanded', String(text.getAttribute('aria-expanded') !== 'true'));
      });
      text.dataset.queueId = message.id; text.setAttribute('aria-expanded', String(expanded.has(message.id)));
      const meta = el('div', 'pending-meta'), status = el('span', `pending-status${message.status === 'failed' ? ' failed' : ''}`);
      const label = message.status === 'sending' ? '正在进入新一轮…' : message.status === 'failed' ? '需要核对' : state.link === 'live' ? '已在电脑端排队' : '断开前已排队';
      status.append(icon(message.status === 'queued' ? 'check' : 'refresh'), el('span', '', label));
      const actions = el('div', 'pending-actions');
      const action = (label: string, fn: () => void, disabled = false) => {
        const node = button(label, '', fn); node.disabled = blocked || message.status === 'sending' || disabled; actions.append(node); return node;
      };
      const guide = action('引导会话', () => void this.store.action('queue-guide', { queueId: message.id }), index !== 0 || message.status !== 'queued');
      guide.dataset.pendingAction = 'guide';
      action('撤回编辑', () => void this.store.withdraw(message.id), Boolean(state.draft.text.trim()));
      action('删除', () => void this.store.action('queue-delete', { queueId: message.id }));
      if (message.status === 'failed') action('重试', () => this.confirm('确认重新发送？', '上一次可能已经执行。请核对会话记录；重新发送可能重复执行同一任务。', '已核对，重新发送',
        () => this.store.action('queue-retry', { queueId: message.id })));
      meta.append(status, actions); item.append(text, meta);
      if (message.error) item.append(el('p', 'pending-error', message.error));
      this.queue.append(item);
    }
    if (submitting) {
      const item = el('article', 'pending-item submitting');
      item.append(el('p', 'pending-text', submitting.text), el('p', 'pending-status', '发送中，正在等待服务器确认…'));
      this.queue.append(item);
    }
  }

  private renderComposer(state: AppState): void {
    if (this.textarea.value !== state.draft.text) this.textarea.value = state.draft.text;
    const block = this.store.mutationBlock(), hasText = Boolean(state.draft.text.trim());
    const active = Boolean(state.snapshot?.activeTurnId), stop = active && !hasText;
    this.send.dataset.action = stop ? 'stop' : 'send'; this.send.type = stop ? 'button' : 'submit';
    this.send.replaceChildren(icon(stop ? 'stop' : 'arrow-up'));
    this.send.disabled = Boolean(block) || (!stop && !hasText);
    this.send.setAttribute('aria-label', stop ? '停止任务' : active ? '发送并加入等待队列' : '发送消息');
    this.textarea.disabled = !state.selectedId;
    this.textarea.placeholder = state.link !== 'live' ? '连接中断，可先写下草稿…'
      : state.snapshot?.readOnly ? '此会话为只读' : active ? '发送后等待当前轮结束…' : '在这个会话中继续…';
    this.skill.hidden = !state.config.skillId; setText(this.skill, `${state.config.skillName || 'Skill'} ×`);
    this.model.replaceChildren(el('span', '', state.models.find((model) => model.model === state.config.model)?.displayName || state.config.model || '模型'), icon('chevron'));
    this.effort.replaceChildren(el('span', '', effortLabels[state.config.effort] || state.config.effort || '默认'), icon('chevron'));
    this.add.disabled = this.model.disabled = this.effort.disabled = Boolean(block);
    const message = state.gate?.text || (state.snapshot?.resyncRequired ? '会话内容已改变，需要重新同步。' : '')
      || (state.selectedId === '@new' ? state.optionsError : '') || (state.notice?.kind === 'error' ? state.notice.text : '');
    this.notice.hidden = !message; setText(this.noticeText, message || '');
    this.review.hidden = state.selectedId === '@new' && !state.gate;
    setText(this.review, state.selectedId === '@new' ? '核对新建结果' : '重新同步');
  }

  private openComposer(kind: 'models' | 'effort' | 'skills'): void {
    this.closeMessageMenu();
    this.popupKind = kind; this.popupAnchor = kind === 'models' ? this.model : kind === 'effort' ? this.effort : this.add;
    this.showPopup();
    if (kind === 'models' || kind === 'effort') void this.store.loadOptions('models');
    else if (this.store.state.selectedId !== '@new') void this.store.loadOptions('skills');
  }

  private openMenu(): void {
    this.popupKind = 'menu'; this.popupAnchor = this.more; this.showPopup();
  }

  private showPopup(): void {
    this.renderPopup(); this.popup.showPopover(); this.popupAnchor?.setAttribute('aria-expanded', 'true');
    this.positionPopup(); this.popup.querySelector<HTMLButtonElement>('button')?.focus({ preventScroll: true });
  }

  private renderPopup(): void {
    const state = this.store.state, kind = this.popupKind;
    const focused = this.popup.contains(document.activeElement) ? (document.activeElement as HTMLElement).dataset.option : undefined;
    this.popup.replaceChildren();
    const heading = el('div', 'popup-heading'), title = kind === 'models' ? '选择模型' : kind === 'effort' ? '思考强度' : kind === 'skills' ? '会话操作' : '更多会话操作';
    this.popup.setAttribute('aria-label', title);
    heading.append(el('h2', '', title), iconButton('关闭菜单', 'close', () => this.closePopup(true), 'icon-btn')); this.popup.append(heading);
    const option = (key: string, label: string, description: string, glyph: IconName, action: () => void, selected = false, disabled = false) => {
      const node = button('', 'popup-action', action); node.dataset.option = key; node.disabled = disabled;
      if (kind === 'models' || kind === 'effort') node.setAttribute('aria-pressed', String(selected));
      const content = el('span', 'popup-content'); content.append(el('span', 'popup-choice-title', label));
      if (description) content.append(el('span', 'popup-choice-description', description));
      node.append(icon(glyph), content); if (selected) node.append(icon('check')); this.popup.append(node); return node;
    };
    const blocked = Boolean(this.store.mutationBlock());
    if (kind === 'menu') {
      option('rename', '重命名会话', '', 'edit', () => this.rename(), false, blocked || state.selectedId === '@new');
      option('sync', '重新同步', '', 'refresh', () => { this.closePopup(); void this.store.resync(); });
      option('connection', '查看连接', '', 'link', () => this.openConnection());
    } else if (kind === 'models') {
      for (const model of state.models) option(model.id, model.displayName, model.description, 'threads', () => {
        this.closePopup(true); void this.store.configure(model.model, model.defaultReasoningEffort);
      }, model.model === state.config.model, blocked);
      if (!state.models.length && !state.optionsLoading && !state.optionsError) this.popup.append(el('p', 'skill-empty', '当前没有可用模型。'));
    } else if (kind === 'effort') {
      const model = state.models.find((model) => model.model === state.config.model);
      for (const effort of model?.supportedReasoningEfforts ?? []) option(effort.reasoningEffort, effortLabels[effort.reasoningEffort] || effort.reasoningEffort,
        effort.description, 'skill', () => {
          this.closePopup(true); void this.store.configure(state.config.model, effort.reasoningEffort);
        }, effort.reasoningEffort === state.config.effort, blocked);
      if (!model?.supportedReasoningEfforts.length && !state.optionsLoading) this.popup.append(el('p', 'skill-empty', '此模型未提供可选思考强度。'));
    } else if (kind === 'skills') {
      option('compact', '压缩会话', '保留关键信息，精简上下文', 'compact', () => {
        this.closePopup(true); void this.store.action('compact');
      }, false, blocked || state.selectedId === '@new' || Boolean(state.snapshot?.activeTurnId || state.snapshot?.queue?.length));
      this.popup.append(el('hr', 'popup-rule'));
      for (const skill of state.skills) option(skill.id, skill.name, skill.description, 'skill', () => {
        this.closePopup(); this.store.useSkill(skill); this.textarea.focus();
      }, false, blocked);
      if (!state.skills.length && !state.optionsLoading && !state.optionsError) this.popup.append(el('p', 'skill-empty', state.selectedId === '@new' ? '创建会话后可选择项目 Skills。' : '暂无已启用的 Skills。'));
    }
    if (state.optionsLoading && kind !== 'menu') this.popup.append(el('p', 'skill-empty', '正在从 Desktop 加载…'));
    if (state.optionsError && kind !== 'menu') this.popup.append(el('p', 'popup-error', state.optionsError));
    if (focused) [...this.popup.querySelectorAll<HTMLElement>('[data-option]')].find((node) => node.dataset.option === focused)?.focus({ preventScroll: true });
    this.positionPopup();
  }

  private positionPopup(): void {
    if (!this.popup.matches(':popover-open') || !this.popupAnchor) return;
    const viewport = window.visualViewport, left = viewport?.offsetLeft ?? 0, top = viewport?.offsetTop ?? 0;
    const width = viewport?.width ?? innerWidth, height = viewport?.height ?? innerHeight;
    const anchor = this.popupAnchor.getBoundingClientRect(), above = anchor.top - top - 20, below = top + height - anchor.bottom - 20;
    const useAbove = above > below;
    this.popup.style.maxHeight = `${Math.max(80, Math.min(430, useAbove ? above : below))}px`;
    const bounds = this.popup.getBoundingClientRect();
    this.popup.style.left = `${Math.max(left + 12, Math.min(anchor.left, left + width - bounds.width - 12))}px`;
    this.popup.style.top = `${Math.max(top + 12, useAbove ? anchor.top - bounds.height - 8 : anchor.bottom + 8)}px`;
  }

  private closePopup(focus = false): void {
    if (this.popup.matches(':popover-open')) this.popup.hidePopover();
    this.popupKind = undefined; this.popupAnchor?.setAttribute('aria-expanded', 'false');
    if (focus) this.popupAnchor?.focus({ preventScroll: true });
  }

  private openDrawer(): void {
    this.drawerOrigin = document.activeElement instanceof HTMLElement ? document.activeElement : undefined;
    this.closePopup(); this.sidebar.classList.add('drawer-open'); this.scrim.classList.add('visible');
    this.main.inert = true; this.sidebar.setAttribute('role', 'dialog'); this.sidebar.setAttribute('aria-modal', 'true');
    this.drawerButton.setAttribute('aria-expanded', 'true');
    this.sidebar.querySelector<HTMLButtonElement>('.sidebar-close')?.focus({ preventScroll: true });
  }

  private closeDrawer(): void {
    const open = this.sidebar.classList.contains('drawer-open');
    this.sidebar.classList.remove('drawer-open'); this.scrim.classList.remove('visible'); this.main.inert = false;
    this.sidebar.removeAttribute('role'); this.sidebar.removeAttribute('aria-modal'); this.drawerButton.setAttribute('aria-expanded', 'false');
    if (open) this.drawerOrigin?.focus({ preventScroll: true });
  }

  private dialogHead(dialog: HTMLDialogElement, title: string): HTMLElement {
    const head = el('div', 'dialog-head'), heading = el('h2', '', title); heading.id = `dialog-${crypto.randomUUID()}`;
    dialog.setAttribute('aria-labelledby', heading.id);
    head.append(heading, iconButton('关闭对话框', 'close', () => dialog.close(), 'icon-btn')); return head;
  }

  private showDialog(dialog: HTMLDialogElement): void {
    const origin = document.activeElement;
    this.root.append(dialog);
    dialog.addEventListener('close', () => { dialog.remove(); if (origin instanceof HTMLElement && origin.isConnected) origin.focus({ preventScroll: true }); }, { once: true });
    dialog.showModal();
  }

  private confirm(title: string, text: string, label: string, run: () => Promise<boolean>): void {
    const dialog = el('dialog', 'app-dialog'), body = el('div', 'dialog-body', text), actions = el('div', 'dialog-actions');
    const accept = button(label, 'btn btn-danger', () => {
      accept.disabled = true;
      void run().then((ok) => { if (ok) dialog.close(); else accept.disabled = false; });
    });
    actions.append(button('取消', 'btn btn-secondary', () => dialog.close()), accept);
    dialog.append(this.dialogHead(dialog, title), body, actions); this.showDialog(dialog);
  }

  private confirmStop(): void {
    const id = this.store.state.selectedId, turnId = this.store.state.snapshot?.activeTurnId;
    this.confirm('停止当前任务？', '当前轮次将停止，消息和文件改动会保留。已排队的消息会继续进入下一轮。', '停止任务', async () => {
      if (id !== this.store.state.selectedId || turnId !== this.store.state.snapshot?.activeTurnId) { this.store.notify('当前轮次已改变，请关闭后重试。'); return false; }
      await this.store.interrupt(); return !this.store.state.gate;
    });
  }

  private rename(): void {
    this.closePopup();
    const id = this.store.state.selectedId, dialog = el('dialog', 'app-dialog'), form = el('form'), input = el('input', 'text-field');
    input.value = this.title.textContent || ''; input.maxLength = 80; input.required = true; input.setAttribute('aria-label', '会话名称');
    const body = el('div', 'dialog-body'); body.append(input);
    const actions = el('div', 'dialog-actions'), save = el('button', 'btn btn-primary', '保存'); save.type = 'submit';
    actions.append(button('取消', 'btn btn-secondary', () => dialog.close()), save);
    form.append(this.dialogHead(dialog, '重命名会话'), body, actions); dialog.append(form);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      if (!input.value.trim()) { input.setCustomValidity('请输入会话名称。'); input.reportValidity(); return; }
      if (id !== this.store.state.selectedId) { this.store.notify('当前会话已改变。'); return; }
      save.disabled = true;
      void this.store.action('rename', { name: input.value.trim() }).then((ok) => { if (ok) dialog.close(); else save.disabled = false; });
    });
    input.addEventListener('input', () => input.setCustomValidity(''));
    this.showDialog(dialog); input.focus(); input.select();
  }

  private openConnection(): void { this.closePopup(); this.info.showModal(); }
  private nearBottom(): boolean { return this.scroller.scrollHeight - this.scroller.clientHeight - this.scroller.scrollTop < 80; }
  private scrollToBottom(): void { this.scroller.scrollTop = this.scroller.scrollHeight; this.latest.hidden = true; }

  private target(shell: HTMLElement): MessageTarget | undefined {
    const threadId = this.store.state.selectedId, turnID = shell.closest<HTMLElement>('[data-turn]')?.dataset.turn;
    const turns = [...this.store.state.snapshot?.thread.turns ?? [], ...this.store.state.history.turns];
    const turn = turns.find((turn) => turn.id === turnID), item = turn?.items.find((item) => item.id === shell.dataset.message);
    if (!threadId || !turn || !item) return undefined;
    return { threadId, turn, item, editable: shell.querySelector<HTMLButtonElement>('[data-message-action="edit"]')?.hidden === false };
  }

  private messageAction(action: string, target: MessageTarget): void {
    if (action === 'copy') { void this.copy(messageText(target.item)); return; }
    if (target.threadId !== this.store.state.selectedId) return;
    if (action === 'fork') { void this.store.action('fork', { turnId: target.turn.id }); return; }
    if (action !== 'edit' || !target.editable || this.store.mutationBlock()) return;
    const dialog = el('dialog', 'app-dialog'), form = el('form'), body = el('div', 'dialog-body'), input = el('textarea', 'edit-field');
    input.value = messageText(target.item); input.maxLength = 32768; input.required = true; input.setAttribute('aria-label', '消息内容');
    body.append(input, el('p', 'edit-note', '将替换这一轮及之后的消息，并重新生成回复。不会还原文件改动。'));
    const actions = el('div', 'dialog-actions'), save = el('button', 'btn btn-primary', '保存并重新生成'); save.type = 'submit';
    actions.append(button('取消', 'btn btn-secondary', () => dialog.close()), save);
    form.append(this.dialogHead(dialog, '编辑消息'), body, actions); dialog.append(form);
    form.addEventListener('submit', (event) => {
      event.preventDefault();
      if (!input.value.trim() || target.threadId !== this.store.state.selectedId) return;
      save.disabled = true;
      void this.store.action('edit', { turnId: target.turn.id, text: input.value }).then((ok) => { if (ok) dialog.close(); else save.disabled = false; });
    });
    this.showDialog(dialog); input.focus();
  }

  private async copy(text: string): Promise<void> {
    try { await navigator.clipboard.writeText(text); this.store.notify('消息已复制', 'success'); }
    catch {
      const dialog = el('dialog', 'app-dialog'), body = el('div', 'dialog-body'), field = el('textarea', 'edit-field');
      field.readOnly = true; field.value = text; field.setAttribute('aria-label', '消息原文');
      body.append(field, el('p', 'dialog-note', '自动复制不可用，请选择并复制上方内容。'));
      dialog.append(this.dialogHead(dialog, '复制消息'), body); this.showDialog(dialog); field.focus(); field.select();
    }
  }

  private openMessageMenu(shell: HTMLElement, focus = false): void {
    const target = this.target(shell); if (!target) return;
    this.closePopup(); this.messageMenu.replaceChildren();
    this.messageMenuShell = shell;
    const action = (name: string, label: string, glyph: IconName) => {
      const node = textButton(label, glyph, () => {
        const latest = shell.isConnected ? this.target(shell) : undefined;
        this.closeMessageMenu();
        if (latest) this.messageAction(name, latest);
      }, '');
      node.setAttribute('role', 'menuitem'); node.dataset.menuAction = name; this.messageMenu.append(node);
    };
    action('copy', '复制消息', 'copy');
    if (target.item.type === 'userMessage') action('edit', '编辑消息', 'edit');
    if (target.item.type === 'agentMessage') action('fork', '分支到新聊天', 'fork');
    this.messageMenu.showPopover();
    this.updateMessageMenu();
    const anchor = shell.getBoundingClientRect(), bounds = this.messageMenu.getBoundingClientRect();
    const viewport = visualViewport, left = viewport?.offsetLeft ?? 0, top = viewport?.offsetTop ?? 0;
    this.messageMenu.style.left = `${Math.max(left + 12, Math.min(anchor.left, left + (viewport?.width ?? innerWidth) - bounds.width - 12))}px`;
    this.messageMenu.style.top = `${Math.max(top + 12, Math.min(anchor.bottom, top + (viewport?.height ?? innerHeight) - bounds.height - 12))}px`;
    if (focus) this.messageMenu.querySelector('button')?.focus({ preventScroll: true });
  }

  private updateMessageMenu(): void {
    if (!this.messageMenu.matches(':popover-open')) { this.messageMenuShell = undefined; return; }
    const target = this.messageMenuShell?.isConnected ? this.target(this.messageMenuShell) : undefined;
    if (!target) { this.closeMessageMenu(); return; }
    const blocked = Boolean(this.store.mutationBlock());
    for (const button of this.messageMenu.querySelectorAll<HTMLButtonElement>('[data-menu-action]')) {
      const action = button.dataset.menuAction;
      button.hidden = action === 'edit' && !target.editable;
      button.disabled = action !== 'copy' && (blocked || (action === 'fork' && target.turn.status === 'inProgress'));
    }
  }

  private closeMessageMenu(): void {
    this.messageMenuShell = undefined;
    if (this.messageMenu.matches(':popover-open')) this.messageMenu.hidePopover();
  }

  private containSelection(): void {
    const selection = document.getSelection();
    if (!selection || selection.isCollapsed || !selection.rangeCount) { this.selectionBoundary = undefined; return; }
    const range = selection.getRangeAt(0).cloneRange();
    const anchor = selection.anchorNode instanceof Element ? selection.anchorNode : selection.anchorNode?.parentElement;
    this.selectionBoundary = this.selectionBoundary?.isConnected ? this.selectionBoundary
      : anchor?.closest<HTMLElement>('[data-selection-boundary]') ?? undefined;
    if (!this.selectionBoundary && range.intersectsNode(this.messages)) {
      this.selectionBoundary = [...this.messages.querySelectorAll<HTMLElement>('[data-selection-boundary]')]
        .find((node) => node.getClientRects().length > 0 && range.intersectsNode(node));
    }
    const boundary = this.selectionBoundary;
    if (!boundary || !this.messages.contains(boundary)) return;
    this.closeMessageMenu();
    const bounds = document.createRange();
    bounds.selectNodeContents(boundary);
    const start = bounds.comparePoint(range.startContainer, range.startOffset), end = bounds.comparePoint(range.endContainer, range.endOffset);
    if (start === 0 && end === 0) return;
    if (start > 0 || end < 0) { selection.removeAllRanges(); return; }
    const backwards = selection.anchorNode === range.endContainer && selection.anchorOffset === range.endOffset;
    if (start < 0) range.setStart(bounds.startContainer, bounds.startOffset);
    if (end > 0) range.setEnd(bounds.endContainer, bounds.endOffset);
    if (backwards) selection.setBaseAndExtent(range.endContainer, range.endOffset, range.startContainer, range.startOffset);
    else selection.setBaseAndExtent(range.startContainer, range.startOffset, range.endContainer, range.endOffset);
  }

  private bindMessages(): void {
    this.messages.addEventListener('toggle', (event) => {
      const details = event.target;
      if (!(details instanceof HTMLDetailsElement) || !this.messages.contains(details)) return;
      const key = `${this.store.state.selectedId}:${details.dataset.disclosure}`;
      if (details.open) this.disclosures.add(key); else this.disclosures.delete(key);
    }, true);
    this.messages.addEventListener('click', (event) => {
      if (!(event.target instanceof Element)) return;
      const action = event.target.closest<HTMLButtonElement>('[data-message-action]'), shell = action?.closest<HTMLElement>('[data-message]');
      if (action) {
        if (!action.disabled && shell) {
          const target = this.target(shell); if (target) this.messageAction(action.dataset.messageAction ?? '', target);
        }
        return;
      }
      if (event.target.closest('a,button,summary') || event.detail > 1 || !document.getSelection()?.isCollapsed) return;
      const pointer = this.messagePointer;
      if (pointer && (Date.now() - pointer.time > 450 || Math.hypot(event.clientX - pointer.x, event.clientY - pointer.y) > 10)) return;
      const message = event.target.closest<HTMLElement>('[data-message]');
      if (message) this.openMessageMenu(message);
    });
    this.messages.addEventListener('pointerdown', (event) => {
      this.messagePointer = { x: event.clientX, y: event.clientY, time: Date.now() };
      this.selectionBoundary = undefined;
    });
    document.addEventListener('selectionchange', () => this.containSelection());
    this.messages.addEventListener('keydown', (event) => {
      if (!(event.target instanceof HTMLElement) || !event.target.matches('[data-message]')) return;
      if (event.key === 'Enter' || event.key === 'ContextMenu' || (event.key === 'F10' && event.shiftKey)) {
        event.preventDefault(); this.openMessageMenu(event.target, true);
      }
    });
  }

  private bindKeyboard(): void {
    document.addEventListener('keydown', (event) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k' && !document.querySelector('dialog[open]')) {
        event.preventDefault();
        if (matchMedia('(max-width:767px)').matches) this.openDrawer();
        this.search.focus({ preventScroll: true });
      }
      if (event.key === 'Escape') {
        if (this.popup.matches(':popover-open')) { this.closePopup(true); return; }
        this.closeDrawer();
      }
      const menu = [this.popup, this.messageMenu].find((node) => node.matches(':popover-open') && node.contains(document.activeElement));
      if (menu && ['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) {
        event.preventDefault();
        const buttons = [...menu.querySelectorAll<HTMLButtonElement>('button:not(:disabled)')], index = buttons.indexOf(document.activeElement as HTMLButtonElement);
        const next = event.key === 'Home' ? 0 : event.key === 'End' ? buttons.length - 1 : (index + (event.key === 'ArrowDown' ? 1 : -1) + buttons.length) % buttons.length;
        buttons[next]?.focus();
      }
      if (event.key === 'Tab' && this.sidebar.classList.contains('drawer-open')) {
        const nodes = [...this.sidebar.querySelectorAll<HTMLElement>('button:not(:disabled),input,select')].filter((node) => node.offsetParent !== null);
        if (event.shiftKey && document.activeElement === nodes[0]) { event.preventDefault(); nodes.at(-1)?.focus(); }
        else if (!event.shiftKey && document.activeElement === nodes.at(-1)) { event.preventDefault(); nodes[0]?.focus(); }
      }
    });
  }
}
