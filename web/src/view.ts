import { button, el, icon, iconButton, setText, textButton } from './dom.ts';
import { CompanionStore, type AppState } from './store.ts';
import { approvalContext, concisePath, messageRows, pendingTitle } from './transcript.ts';
import type { Snapshot, ThreadSummary } from './types.ts';

const linkLabels: Record<AppState['link'], string> = {
  live: '已连接', syncing: '正在同步', reconnecting: '重新连接中', offline: '网络已断开', 'desktop-offline': 'Desktop 未连接', 'remote-off': '远程服务已关闭',
};
const threadStatuses: Record<string, string> = {
  active: '正在运行', inProgress: '正在运行', idle: '等待输入', notLoaded: '等待 Desktop', systemError: '需要检查',
};

export class CompanionView {
  private readonly store: CompanionStore;
  private readonly root: HTMLElement;
  private readonly headerStatus = button('', 'connection-badge', () => this.openInfo());
  private readonly headerStatusText = el('span');
  private readonly app = el('main', 'workspace');
  private readonly sidebarList = el('div', 'thread-list');
  private readonly drawerList = el('div', 'thread-list');
  private readonly drawer = el('dialog', 'thread-drawer');
  private readonly drawerButton = textButton('会话', 'threads', () => this.drawer.showModal(), 'thread-switch');
  private readonly title = el('h1', 'conversation-title', '会话');
  private readonly path = el('span', 'conversation-path');
  private readonly turnStatus = el('span', 'turn-status');
  private readonly refresh = iconButton('刷新会话和连接状态', 'refresh', () => void this.store.refresh());
  private readonly resync = textButton('重新同步', 'refresh', () => void this.store.resync(), 'notice-action');
  private readonly connectionNotice = el('div', 'connection-notice');
  private readonly connectionNoticeText = el('span');
  private readonly catalogueControls: { search: HTMLInputElement; archived: HTMLSelectElement; more: HTMLButtonElement; status: HTMLElement }[] = [];
  private readonly transcript = el('section', 'transcript');
  private readonly transcriptInner = el('div', 'transcript-inner');
  private readonly historyNotice = el('p', 'history-notice', '这里只展示部分历史。更早的内容请在 Desktop 查看。');
  private readonly messages = el('div', 'messages');
  private readonly approvals = el('section', 'approvals');
  private readonly running = el('p', 'running-note');
  private readonly empty = el('div', 'empty-state');
  private readonly emptyTitle = el('h2');
  private readonly emptyDescription = el('p');
  private readonly latest = textButton('回到最新', 'arrow-down', () => this.scrollToLatest(), 'latest-button');
  private readonly composer = el('form', 'composer');
  private readonly operationNotice = el('div', 'operation-notice');
  private readonly operationNoticeTitle = el('strong');
  private readonly operationNoticeText = el('p');
  private readonly operationRefresh = textButton('刷新并核对', 'refresh', () => void this.store.refresh(), 'notice-action');
  private readonly start = button('开始新轮次', 'mode-button', () => this.store.setMode('start'));
  private readonly steer = button('补充指令', 'mode-button', () => this.store.setMode('steer'));
  private readonly interrupt = textButton('停止本轮', 'stop', () => void this.store.interrupt(), 'interrupt-button');
  private readonly textarea = el('textarea', 'composer-input');
  private readonly send = el('button', 'send-button');
  private readonly sendText = el('span', '', '发送');
  private readonly composerHint = el('p', 'composer-hint');
  private readonly info = el('dialog', 'info-dialog');
  private readonly infoService = el('dd');
  private readonly infoConnection = el('dd');
  private readonly live = el('div', 'sr-only');
  private readonly itemNodes = new Map<string, { signature: string; node: HTMLElement }>();
  private lastSnapshot?: Snapshot;
  private lastThreadId?: string;
  private lastThreadSignature = '';
  private lastPendingSignature = '';
  private hasNewMessages = false;
  private lastAnnouncement = '';

  constructor(root: HTMLElement, store: CompanionStore) {
    this.root = root; this.store = store;
    const skip = button('跳到输入框', 'skip-link', () => this.textarea.focus());
    const header = el('header', 'app-header');
    const brand = el('div', 'brand');
    const mark = el('span', 'brand-mark'); mark.append(icon('mirror'));
    const wordmark = el('div', 'wordmark');
    wordmark.append(el('span', 'brand-name', 'MirrorBird'), el('span', 'brand-description', '同一个 Desktop 会话'));
    brand.append(mark, wordmark);
    this.headerStatus.append(el('span', 'status-dot'), this.headerStatusText, icon('chevron'));
    this.headerStatus.setAttribute('aria-label', '查看连接信息');
    header.append(brand, this.headerStatus);
    this.live.setAttribute('role', 'status'); this.live.setAttribute('aria-live', 'polite'); this.live.setAttribute('aria-atomic', 'true');
    this.buildWorkspace();
    this.buildInfo();
    this.root.append(skip, header, this.app, this.drawer, this.info, this.live);
    this.store.subscribe(() => this.render());
    this.render();
  }

  private buildWorkspace(): void {
    const sidebar = el('aside', 'sidebar'); sidebar.setAttribute('aria-label', '会话列表');
    const sidebarHeading = el('div', 'sidebar-heading');
    sidebarHeading.append(el('h2', '', '会话'), el('span', 'sidebar-caption', '来自你的电脑'));
    const sidebarFoot = el('p', 'sidebar-footnote'); sidebarFoot.append(icon('mirror'), el('span', '', '消息、运行与审批，和 Desktop 保持同一份。'));
    const sidebarControls = this.buildCatalogueControls();
    sidebar.append(sidebarHeading, sidebarControls.filters, this.sidebarList, sidebarControls.footer, sidebarFoot);
    const conversation = el('section', 'conversation'); conversation.setAttribute('aria-label', '当前会话');
    const conversationHeader = el('header', 'conversation-header');
    const titleGroup = el('div', 'conversation-heading');
    const metadata = el('div', 'conversation-meta'); metadata.append(this.turnStatus, this.path);
    titleGroup.append(this.title, metadata);
    const actions = el('div', 'conversation-actions'); actions.append(this.drawerButton, this.refresh);
    conversationHeader.append(titleGroup, actions);
    this.connectionNotice.setAttribute('role', 'status');
    this.connectionNotice.append(this.connectionNoticeText, this.resync);
    this.transcript.tabIndex = 0; this.transcript.id = 'transcript'; this.transcript.setAttribute('aria-label', '会话消息与工具');
    this.approvals.setAttribute('aria-label', '等待处理的请求');
    this.historyNotice.hidden = true;
    this.running.append(el('span', 'working-dot'), el('span', '', 'Desktop 正在继续这一轮…'));
    const emptyMark = el('div', 'empty-mark'); emptyMark.append(icon('threads'));
    this.empty.append(emptyMark, this.emptyTitle, this.emptyDescription);
    this.transcriptInner.append(this.historyNotice, this.messages, this.approvals, this.running, this.empty);
    this.transcript.append(this.transcriptInner);
    this.transcript.addEventListener('scroll', () => {
      if (this.nearBottom()) this.hasNewMessages = false;
      this.latest.hidden = !this.hasNewMessages;
    }, { passive: true });
    this.latest.hidden = true;
    this.buildComposer();
    conversation.append(conversationHeader, this.connectionNotice, this.transcript, this.latest, this.composer);
    this.app.append(sidebar, conversation);
    const drawerHeader = el('div', 'dialog-header');
    const drawerTitle = el('h2', '', '会话'); drawerTitle.id = 'drawer-title';
    this.drawer.setAttribute('aria-labelledby', drawerTitle.id);
    drawerHeader.append(drawerTitle, iconButton('关闭会话列表', 'close', () => this.drawer.close()));
    const drawerControls = this.buildCatalogueControls();
    this.drawer.append(drawerHeader, drawerControls.filters, this.drawerList, drawerControls.footer,
      el('p', 'drawer-note', '归档会话可通过筛选查看，打开后保持只读。'));
    this.drawer.addEventListener('click', (event) => { if (event.target === this.drawer) this.drawer.close(); });
  }

  private buildCatalogueControls(): { filters: HTMLElement; footer: HTMLElement } {
    const filters = el('div', 'catalogue-filters');
    const search = el('input', 'text-input'); search.type = 'search'; search.maxLength = 512;
    search.placeholder = '搜索会话'; search.setAttribute('aria-label', '搜索会话'); search.autocomplete = 'off';
    search.addEventListener('input', () => this.store.setSearch(search.value));
    const archived = el('select', 'text-input'); archived.setAttribute('aria-label', '按归档状态筛选');
    const active = el('option', '', '未归档'); active.value = 'false';
    const archive = el('option', '', '已归档'); archive.value = 'true';
    archived.append(active, archive); archived.addEventListener('change', () => this.store.setArchived(archived.value === 'true'));
    filters.append(search, archived);
    const footer = el('div', 'catalogue-footer');
    const status = el('p', 'catalogue-status'); status.setAttribute('role', 'status');
    const more = button('加载更多', 'secondary-button', () => void this.store.loadMore());
    footer.append(status, more);
    this.catalogueControls.push({ search, archived, more, status });
    return { filters, footer };
  }

  private buildComposer(): void {
    const noticeBody = el('div'); noticeBody.append(this.operationNoticeTitle, this.operationNoticeText);
    this.operationNotice.setAttribute('role', 'status');
    this.operationNotice.append(noticeBody, this.operationRefresh);
    const toolbar = el('div', 'composer-toolbar');
    const modes = el('div', 'composer-modes'); modes.setAttribute('role', 'group'); modes.setAttribute('aria-label', '发送方式');
    this.start.setAttribute('aria-pressed', 'true'); this.steer.setAttribute('aria-pressed', 'false');
    modes.append(this.start, this.steer); toolbar.append(modes, this.interrupt);
    this.textarea.id = 'composer'; this.textarea.rows = 2; this.textarea.maxLength = 32_768;
    this.textarea.placeholder = '继续和 Desktop 一起做…';
    this.textarea.setAttribute('aria-label', '发送给当前会话的内容');
    this.textarea.setAttribute('aria-describedby', 'composer-hint');
    this.textarea.setAttribute('enterkeyhint', 'enter');
    this.textarea.autocomplete = 'off';
    this.textarea.addEventListener('input', () => this.store.setDraft(this.textarea.value));
    this.textarea.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && (event.ctrlKey || event.metaKey) && !event.isComposing) {
        event.preventDefault(); void this.store.send();
      }
    });
    const input = el('div', 'composer-input-wrap'); input.append(this.textarea);
    const sendRow = el('div', 'composer-send-row');
    const identity = el('span', 'composer-identity'); identity.append(icon('mirror'), el('span', '', '发往同一个 Desktop 会话'));
    this.send.type = 'submit'; this.send.append(this.sendText, icon('arrow-up'));
    sendRow.append(identity, this.send); input.append(sendRow);
    this.composerHint.id = 'composer-hint';
    this.composer.append(this.operationNotice, toolbar, input, this.composerHint);
    this.composer.addEventListener('submit', (event) => { event.preventDefault(); void this.store.send(); });
  }

  private buildInfo(): void {
    const head = el('div', 'dialog-header'); const heading = el('h2', '', '连接状态'); heading.id = 'info-title';
    head.append(heading, iconButton('关闭连接信息', 'close', () => this.info.close()));
    this.info.setAttribute('aria-labelledby', heading.id);
    const identity = el('p', 'info-identity'); identity.append(icon('mirror'), el('strong', '', '同一个会话，不是另开一份'));
    const description = el('p', 'info-description', '手机上的消息、补充指令、停止请求和审批，都交给正在电脑上运行的同一个 Desktop 会话。');
    const list = el('dl', 'connection-info');
    list.append(el('dt', '', '远程服务'), this.infoService, el('dt', '', '连接'), this.infoConnection);
    const note = el('p', 'info-note', '可随时在电脑端停止共享，关闭远程服务。此页不把聊天内容或草稿写入浏览器存储；关闭或刷新页面会丢失草稿。');
    const network = el('p', 'info-note', '远程访问经过 Microsoft 私有隧道，电脑需保持在线。Microsoft 会终止转发连接的 TLS，这不是针对中继方的端到端加密。');
    const done = button('知道了', 'primary-button', () => this.info.close());
    this.info.append(head, identity, description, list, note, network, done);
    this.info.addEventListener('click', (event) => { if (event.target === this.info) this.info.close(); });
  }

  private openInfo(): void { this.info.showModal(); }

  render(): void {
    const state = this.store.state;
    setText(this.headerStatusText, linkLabels[state.link]);
    this.headerStatus.dataset.state = state.link;
    setText(this.infoService, state.connection ? state.connection.sharing ? '已开启' : '已关闭' : '正在检查');
    setText(this.infoConnection, linkLabels[state.link]);
    this.renderThreads(state);
    const snapshot = state.snapshot;
    const selected = state.threads.find((thread) => thread.id === state.selectedId);
    setText(this.title, snapshot?.thread.name || selected?.name || '会话');
    setText(this.path, snapshot ? concisePath(snapshot.thread.cwd) : '');
    this.path.title = snapshot?.thread.cwd ?? '';
    const status = this.store.readOnlyReason() ? state.selectedArchived ? '已归档 · 只读' : '只读'
      : snapshot?.pending.length ? '等待你决定' : snapshot?.activeTurnId ? '正在运行' : '等待输入';
    setText(this.turnStatus, snapshot ? status : '');
    this.turnStatus.dataset.active = String(Boolean(snapshot?.activeTurnId));
    const refreshing = state.refreshing || state.checkingConnection;
    this.refresh.disabled = refreshing; this.refresh.setAttribute('aria-busy', String(refreshing));
    this.drawerButton.setAttribute('aria-label', `切换会话，当前为${this.title.textContent}`);
    this.renderConnection(state);
    this.renderTranscript(state);
    this.renderComposer(state);
    this.announce(state);
  }

  private renderThreads(state: AppState): void {
    for (const controls of this.catalogueControls) {
      if (controls.search.value !== state.catalogue.search) controls.search.value = state.catalogue.search;
      controls.archived.value = String(state.catalogue.archived);
      controls.more.hidden = !state.catalogue.nextCursor;
      controls.more.disabled = state.catalogue.loading || !state.connection?.sharing;
      setText(controls.more, state.catalogue.loading ? '正在加载…' : '加载更多');
      setText(controls.status, state.catalogue.error || (state.catalogue.loading ? '正在读取会话列表…' : ''));
      controls.status.hidden = !controls.status.textContent;
    }
    const signature = JSON.stringify([state.selectedId, state.threads, state.catalogue.loading, state.catalogue.search, state.catalogue.archived, state.link]);
    if (signature === this.lastThreadSignature) return;
    this.lastThreadSignature = signature;
    for (const list of [this.sidebarList, this.drawerList]) {
      list.replaceChildren();
      list.setAttribute('aria-busy', String(state.catalogue.loading));
      if (!state.threads.length && !state.catalogue.loading) list.append(el('p', 'thread-list-empty',
        state.link === 'remote-off' ? '远程服务已关闭。' : !state.connection ? '正在连接电脑…'
          : state.catalogue.search ? '没有匹配的会话。' : state.catalogue.archived ? '没有归档会话。' : '暂无会话。'));
      for (const thread of state.threads) list.append(this.threadButton(thread, thread.id === state.selectedId));
    }
  }

  private threadButton(thread: ThreadSummary, selected: boolean): HTMLButtonElement {
    const node = button('', `thread-option ${selected ? 'is-selected' : ''}`, () => {
      this.store.selectThread(thread.id, thread.archived === true); this.drawer.close();
    });
    node.setAttribute('aria-current', String(selected));
    const symbol = el('span', 'thread-symbol'); symbol.append(icon('threads'));
    const content = el('span', 'thread-option-content');
    const status = thread.archived ? '已归档 · 只读' : thread.canAcceptDirectInput === false ? '只读'
      : threadStatuses[thread.status] || '可查看';
    content.append(el('span', 'thread-name', thread.name), el('span', 'thread-status', status));
    node.append(symbol, content);
    if (selected) node.append(icon('chevron'));
    return node;
  }

  private renderConnection(state: AppState): void {
    let text = '';
    if (state.link === 'remote-off') text = '电脑已关闭远程服务。请在电脑端点击“开启共享”，恢复后会先核对状态，不会重发操作。';
    else if (state.snapshot?.resyncRequired) text = '同步已暂停。请点击“重新同步”重新读取会话状态，不会重发任何操作。';
    else if (state.link === 'offline') text = '网络已断开。草稿留在此页，恢复连接后会先核对最新状态。';
    else if (state.link === 'reconnecting') text = '连接中断，正在重新同步。没有重发任何操作，草稿留在此页。';
    else if (state.link === 'desktop-offline') text = 'Desktop 暂未连接。请在电脑端检查 MirrorBird，手机暂时不能操作。';
    else if (state.link === 'syncing') text = '正在与 Desktop 核对最新状态…';
    setText(this.connectionNoticeText, text); this.connectionNotice.hidden = !text;
    this.resync.hidden = !state.snapshot?.resyncRequired;
    this.resync.disabled = state.refreshing || state.checkingConnection || state.link !== 'live';
    this.connectionNotice.classList.toggle('needs-attention', Boolean(state.snapshot?.resyncRequired) || state.link === 'remote-off');
  }

  private renderTranscript(state: AppState): void {
    const snapshot = state.snapshot;
    const switched = state.selectedId !== this.lastThreadId;
    const atBottom = this.nearBottom();
    const changed = snapshot !== this.lastSnapshot || switched;
    if (changed) {
      if (switched) { this.itemNodes.clear(); this.messages.replaceChildren(); this.lastPendingSignature = ''; }
      this.lastThreadId = state.selectedId; this.lastSnapshot = snapshot;
      if (snapshot) {
        const rows = messageRows(snapshot);
        const keep = new Set(rows.map((row) => row.key));
        for (const [key, old] of this.itemNodes) if (!keep.has(key)) { old.node.remove(); this.itemNodes.delete(key); }
        let previous: HTMLElement | undefined;
        for (const row of rows) {
          let entry = this.itemNodes.get(row.key);
          if (!entry || entry.signature !== row.signature) {
            const next = row.build();
            if (entry?.node instanceof HTMLDetailsElement && next instanceof HTMLDetailsElement) next.open = entry.node.open;
            if (entry) entry.node.replaceWith(next);
            entry = { signature: row.signature, node: next }; this.itemNodes.set(row.key, entry);
          }
          const expected = previous ? previous.nextElementSibling : this.messages.firstElementChild;
          if (expected !== entry.node) this.messages.insertBefore(entry.node, expected);
          previous = entry.node;
        }
      } else {
        this.itemNodes.clear(); this.messages.replaceChildren();
      }
    }
    this.historyNotice.hidden = !snapshot?.historyTruncated;
    this.running.hidden = !snapshot?.activeTurnId || Boolean(snapshot.pending.length) || Boolean(snapshot.resyncRequired);
    const hasMessages = Boolean(this.messages.childElementCount);
    this.empty.hidden = hasMessages || Boolean(snapshot?.pending.length) || Boolean(snapshot?.activeTurnId);
    if (state.link === 'remote-off') {
      setText(this.emptyTitle, '远程服务已关闭');
      setText(this.emptyDescription, '在电脑端点击“开启共享”后，此页会自动连接。已有操作不会自动重试。');
    } else if (state.link === 'offline' && !snapshot) {
      setText(this.emptyTitle, '网络已断开');
      setText(this.emptyDescription, '恢复连接后会自动检查最新状态，草稿留在此页。');
    } else if (!state.connection || state.checkingConnection && !state.selectedId) {
      setText(this.emptyTitle, '正在连接电脑');
      setText(this.emptyDescription, state.notice?.text || '正在检查远程服务和 Desktop 的连接状态。');
    } else if (state.link === 'desktop-offline' && !snapshot) {
      setText(this.emptyTitle, '等待 Desktop 连接');
      setText(this.emptyDescription, '请在电脑端检查 MirrorBird 和 Desktop。');
    } else if (!state.selectedId) {
      setText(this.emptyTitle, state.catalogue.loading ? '正在读取会话' : state.catalogue.search ? '没有匹配的会话' : '暂无会话');
      setText(this.emptyDescription, state.catalogue.error || '可搜索或切换归档筛选查看电脑上的会话。这里不会新建另一个会话。');
    } else if (!snapshot) {
      setText(this.emptyTitle, state.refreshing ? '正在打开会话' : '会话暂时不可用');
      setText(this.emptyDescription, state.notice?.text || '可以使用右上角的刷新按钮重新检查，不会发送消息。');
    } else {
      setText(this.emptyTitle, this.store.readOnlyReason() ? '只读会话' : '从这里接着做');
      setText(this.emptyDescription, this.store.readOnlyReason() || '写下下一步。这条消息会交给电脑上同一个 Desktop 会话。');
    }
    this.renderApprovals(state);
    if (changed) {
      if (switched || atBottom) requestAnimationFrame(() => this.scrollToLatest());
      else { this.hasNewMessages = true; this.latest.hidden = false; }
    }
  }

  private renderApprovals(state: AppState): void {
    const pending = state.snapshot?.pending ?? [];
    const signature = JSON.stringify([state.selectedId, pending]);
    if (signature !== this.lastPendingSignature) {
      this.lastPendingSignature = signature;
      this.approvals.replaceChildren();
      for (const request of pending) {
        const context = approvalContext(request);
        const card = el('article', 'approval');
        const head = el('div', 'approval-heading'); head.append(icon('lock'), el('span', '', '需要你决定'));
        card.append(head, el('h2', '', pendingTitle(request)));
        const details = el('details', 'approval-details'); details.open = true;
        const summary = el('summary', '', '操作详情'); summary.append(icon('chevron'));
        const pre = el('pre', 'approval-context'); pre.append(el('code', '', context.context || '请在 Desktop 查看完整请求。'));
        details.append(summary, pre); card.append(details);
        if (context.canRespond) {
          card.append(el('p', 'approval-note', '决定只针对这次请求，不会更改 Desktop 的权限设置。'));
          const actions = el('div', 'approval-actions');
          const decline = button('不允许', 'secondary-button', () => void this.store.respond(request.id, 'decline'));
          const accept = textButton('仅允许这次', 'check', () => void this.store.respond(request.id, 'accept'), 'primary-button');
          actions.append(decline, accept); card.append(actions);
        } else card.append(el('p', 'approval-note', context.explanation));
        this.approvals.append(card);
      }
    }
    for (const node of this.approvals.querySelectorAll('button')) node.disabled = Boolean(this.store.mutationBlock());
    this.approvals.hidden = !pending.length;
  }

  private renderComposer(state: AppState): void {
    const active = state.snapshot?.activeTurnId;
    this.composer.hidden = !state.selectedId;
    const readOnly = Boolean(this.store.readOnlyReason());
    this.start.disabled = Boolean(active) || !state.snapshot || readOnly;
    this.steer.disabled = !active || readOnly;
    this.start.setAttribute('aria-pressed', String(state.draft.mode === 'start'));
    this.steer.setAttribute('aria-pressed', String(state.draft.mode === 'steer'));
    this.interrupt.hidden = !active;
    this.interrupt.disabled = Boolean(this.store.mutationBlock());
    this.textarea.disabled = !state.selectedId || readOnly || !state.connection?.sharing;
    if (this.textarea.value !== state.draft.text) this.textarea.value = state.draft.text;
    const block = this.store.sendBlock();
    this.send.disabled = Boolean(block);
    setText(this.sendText, state.operation?.action === 'messages' ? '发送中' : state.draft.mode === 'steer' ? '补充' : '发送');
    this.send.setAttribute('aria-label', state.draft.mode === 'steer' ? '补充指令到当前轮次' : '发送并开始新轮次');
    const defaultHint = state.draft.mode === 'steer' ? '补充到正在运行的这一轮，不会新建轮次。' : '开始同一会话的下一轮。Enter 换行。';
    setText(this.composerHint, block && state.draft.text.trim() ? block : this.store.mutationBlock() || defaultHint);
    this.composerHint.classList.toggle('hint-warning', Boolean(block && state.draft.text.trim()));
    const gate = state.gate;
    const notice = gate ?? state.notice;
    this.operationNotice.hidden = !notice;
    this.operationNotice.classList.toggle('notice-error', gate?.kind === 'unknown' || state.notice?.kind === 'error');
    this.operationNotice.classList.toggle('notice-success', !gate && state.notice?.kind === 'success');
    setText(this.operationNoticeTitle, gate?.kind === 'unknown' ? '操作结果尚不确定' : gate ? '操作未完成' : '');
    setText(this.operationNoticeText, notice?.text || '');
    this.operationRefresh.hidden = !gate;
    this.operationRefresh.disabled = state.refreshing || state.checkingConnection;
    this.operationNotice.setAttribute('role', gate ? 'alert' : 'status');
  }

  private announce(state: AppState): void {
    const text = state.gate ? state.gate.text : state.notice?.text || `${linkLabels[state.link]}。${state.snapshot?.pending.length ? '有请求等待你决定。' : ''}`;
    if (text !== this.lastAnnouncement) { this.lastAnnouncement = text; setText(this.live, text); }
  }

  private nearBottom(): boolean {
    return this.transcript.scrollHeight - this.transcript.scrollTop - this.transcript.clientHeight < 100;
  }

  private scrollToLatest(): void {
    this.transcript.scrollTop = this.transcript.scrollHeight;
    this.hasNewMessages = false; this.latest.hidden = true;
  }
}
