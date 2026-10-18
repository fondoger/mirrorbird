import { el, icon, renderText } from './dom.ts';
import { record, string, type Item, type PendingRequest, type Snapshot, type Turn } from './types.ts';

const statusLabels: Record<string, string> = {
  inProgress: '进行中', completed: '已完成', failed: '未完成', interrupted: '已停止',
  declined: '已拒绝', approved: '已允许', pending: '等待中', waiting: '等待中',
};
export const statusLabel = (status: string): string => statusLabels[status] ?? '';

export function inputText(content: unknown): string {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return '';
  return content.map((entry) => {
    const part = record(entry);
    if (typeof part.text === 'string') return part.text;
    switch (part.type) {
      case 'skill': return `Skill · ${string(part.name, '已选择的 Skill')}`;
      case 'image':
      case 'localImage': return '［图片，请在 Desktop 查看］';
      case 'file': return '［文件，请在 Desktop 查看］';
      default: return '［此输入类型请在 Desktop 查看］';
    }
  }).join('\n');
}

export function safeStructured(value: unknown): string {
  if (typeof value === 'string') return value;
  if (value === undefined || value === null) return '';
  try { return JSON.stringify(value, null, 2); }
  catch { return '此详情无法显示，请在 Desktop 查看。'; }
}

const toolNames: Record<string, string> = {
  commandExecution: '运行命令', fileChange: '文件改动', mcpToolCall: '工具调用',
  dynamicToolCall: '工具调用', webSearch: '搜索', imageView: '查看图片',
  imageGeneration: '生成图片', collabAgentToolCall: '协作任务',
  contextCompaction: '上下文整理', plan: '执行计划', reasoning: '思考摘要',
};

export interface MessageRow { key: string; signature: string; build: () => HTMLElement }

export function messageRows(snapshot: Snapshot, history: Turn[] = []): MessageRow[] {
  const result: MessageRow[] = [];
  const turns = [...new Map([...history, ...snapshot.thread.turns].map((turn) => [turn.id, turn])).values()];
  for (const turn of turns) {
    result.push({
      key: turn.id, signature: JSON.stringify(turn),
      build: () => renderTurn(turn),
    });
  }
  return result;
}

export function messageText(item: Item): string { return item.text || inputText(item.content); }

export function durationLabel(turn: Turn): string {
  const ms = turn.durationMs ?? (turn.startedAt && turn.completedAt ? (turn.completedAt - turn.startedAt) * 1000 : undefined);
  if (ms === undefined) return statusLabel(turn.status) || '已结束';
  const seconds = Math.max(0, Math.round(ms / 1000));
  return `${turn.status === 'interrupted' ? '已停止 · ' : ''}用时 ${seconds >= 60 ? `${Math.floor(seconds / 60)}m ` : ''}${seconds % 60}s`;
}

function renderTurn(turn: Turn): HTMLElement {
  const node = el('section', 'turn'); node.dataset.turn = turn.id;
  const active = turn.status === 'inProgress';
  const user = turn.items.find((item) => item.type === 'userMessage');
  if (user) { const message = renderItem(user); message.dataset.editable = 'true'; node.append(message); }
  const lastAgent = turn.items.filter((item) => item.type === 'agentMessage').at(-1);
  const final = turn.items.filter((item) => item.type === 'agentMessage' &&
    (item.phase === 'final_answer' || (!active && !item.phase && item === lastAgent)));
  const process = turn.items.filter((item) => item !== user && !final.includes(item));
  const body = el('article', 'assistant-message');
  const processBody = el('div', 'process-body');
  for (let index = 0; index < process.length;) {
    const item = process[index]!;
    if (!isTool(item)) { processBody.append(renderItem(item)); index++; continue; }
    const tools: Item[] = [];
    while (index < process.length && isTool(process[index]!)) tools.push(process[index++]!);
    if (tools.length === 1) processBody.append(renderItem(tools[0]!));
    else {
      const group = el('details', 'tool-group'); group.dataset.disclosure = `group:${tools[0]!.id}`;
      const summary = el('summary'); summary.append(icon('terminal'), el('span', 'tool-caption', toolSummary(tools)), icon('chevron'));
      group.append(summary, ...tools.map(renderItem)); processBody.append(group);
    }
  }
  if (turn.plan) labeledCode(processBody, '执行计划', safeStructured(turn.plan));
  if (turn.diff && !process.some((item) => item.type === 'fileChange')) processBody.append(renderDiff(turn.diff));
  if (active) body.append(processBody);
  else if (processBody.childElementCount) {
    const details = el('details', 'turn-process'); details.dataset.disclosure = `process:${turn.id}`;
    const summary = el('summary'); summary.append(el('span', '', durationLabel(turn)), icon('chevron'));
    details.append(summary, processBody); body.append(details);
  } else body.append(el('div', 'turn-time', durationLabel(turn)));
  for (const message of final) body.append(renderItem(message));
  if (active) {
    const current = [...process].reverse().find((item) => item.status === 'inProgress');
    const running = el('div', 'current-operation'); const spinner = icon('refresh'); spinner.classList.add('spin');
    const label = el('span', '', current?.command || current?.name || '正在生成回复…');
    label.title = label.textContent ?? '';
    running.append(spinner, label); body.append(running);
  } else if (!final.length) body.append(el('p', 'turn-note', turn.status === 'failed' ? '本轮未能完成，请查看执行过程。' : '本轮未生成最终回复。'));
  if (turn.error) labeledCode(body, '任务错误', safeStructured(turn.error));
  node.append(body); return node;
}

function isTool(item: Item): boolean {
  return !['userMessage', 'agentMessage', 'reasoning', 'plan', 'contextCompaction'].includes(item.type);
}

export function toolSummary(items: Item[]): string {
  const read = new Set<string>(), edited = new Set<string>();
  let commands = 0, other = 0;
  for (const item of items) {
    if (item.type === 'fileChange' && Array.isArray(item.changes)) {
      for (const change of item.changes) edited.add(string(record(change).path));
    } else if (item.type === 'commandExecution') {
      const actions = Array.isArray(item.commandActions) ? item.commandActions.map(record) : [];
      const reads = actions.filter((action) => action.type === 'read');
      if (reads.length && reads.length === actions.length) for (const action of reads) read.add(string(action.path) || string(action.name));
      else commands++;
    } else other++;
  }
  return [
    read.size ? `读取 ${read.size} 个文件` : '', edited.size ? `修改 ${edited.size} 个文件` : '',
    commands ? `运行 ${commands} 条命令` : '', other ? `${other} 个其他操作` : '',
  ].filter(Boolean).join('、') || `${items.length} 个操作`;
}

function renderItem(item: Item): HTMLElement {
  if (item.type === 'userMessage' || item.type === 'agentMessage') {
    const user = item.type === 'userMessage';
    const row = el('div', `message-shell ${user ? 'user-shell' : ''}`);
    row.tabIndex = 0; row.dataset.message = item.id; row.dataset.role = user ? 'user' : 'assistant';
    row.setAttribute('aria-haspopup', 'menu');
    row.setAttribute('aria-label', user ? '用户消息，来自共享会话' : 'Codex 消息');
    const body = el('div', `${user ? 'user-message ' : ''}prose`);
    body.dataset.selectionBoundary = '';
    const text = messageText(item);
    body.append(renderText(text || (user ? '［此消息请在 Desktop 查看］' : '正在接收内容…')));
    row.append(body);
    const actions = el('div', 'message-quick-actions');
    for (const [action, label] of [['copy', '复制消息'], [user ? 'edit' : 'fork', user ? '编辑消息' : '分支到新聊天']] as const) {
      const b = el('button'); b.type = 'button'; b.dataset.messageAction = action; b.setAttribute('aria-label', label); b.title = label;
      b.append(icon(action)); actions.append(b);
    }
    row.append(actions);
    return row;
  }
  if (item.type === 'contextCompaction') {
    const note = el('div', 'timeline-event'); note.append(icon('compact'), el('span', '', '会话已压缩')); return note;
  }
  if (item.type === 'reasoning' || item.type === 'plan') {
    const note = el('div', 'prose reasoning');
    note.dataset.selectionBoundary = '';
    note.append(renderText(item.text || item.summary?.join('\n\n') || inputText(item.content))); return note;
  }

  const details = el('details', 'tool-details'); details.dataset.disclosure = item.id;
  const summary = el('summary', 'tool-summary');
  const name = toolNames[item.type] ?? 'Desktop 活动';
  summary.append(icon(item.type === 'fileChange' ? 'file' : 'terminal'));
  const heading = el('span', 'tool-caption');
  const command = el('span', 'tool-command', item.command || item.name || name);
  command.title = command.textContent ?? '';
  heading.append(command);
  summary.append(heading);
  const state = statusLabel(item.status ?? '');
  if (state) summary.append(el('span', `tool-status ${item.status === 'failed' ? 'is-error' : ''}`, state));
  summary.append(icon('chevron'));
  const body = el('div', 'tool-result');
  if (item.command) labeledCode(body, '命令', item.command);
  if (item.aggregatedOutput) labeledCode(body, '输出', item.aggregatedOutput);
  if (Array.isArray(item.changes)) for (const change of item.changes) {
    const file = record(change), block = el('section', 'change-block');
    const filename = el('div', 'change-header file-name', string(file.path, '文件改动'));
    filename.title = filename.textContent ?? '';
    block.append(filename);
    block.append(renderDiff(string(file.diff) || safeStructured(file))); body.append(block);
  }
  if (item.text) body.append(renderText(item.text));
  if (item.content !== undefined && item.content !== null) labeledCode(body, '内容', safeStructured(item.content));
  for (const [label, value] of [['参数', item.arguments], ['结果', item.result], ['错误', item.error], ['进度', item.progress]] as const) {
    if (value !== undefined && value !== null && value !== '') labeledCode(body, label, safeStructured(value));
  }
  if (item.exitCode !== undefined) body.append(el('p', 'turn-note', `退出代码 ${item.exitCode}`));
  if (!body.childElementCount) body.append(el('p', 'muted', '此活动的完整详情请在 Desktop 查看。'));
  details.append(summary, body);
  return details;
}

function renderDiff(text: string): HTMLElement {
  const block = el('pre', 'diff-code');
  block.dataset.selectionBoundary = '';
  for (const line of text.split('\n')) {
    block.append(el('span', `diff-line${line.startsWith('+') ? ' add' : line.startsWith('-') ? ' remove' : ''}`, `${line}\n`));
  }
  return block;
}

function labeledCode(parent: HTMLElement, label: string, text: string): void {
  parent.append(el('p', 'detail-label', label));
  const pre = el('pre', 'tool-output'); pre.append(el('code', '', text)); parent.append(pre);
  pre.dataset.selectionBoundary = '';
}

export function approvalContext(request: PendingRequest): { context: string; canRespond: boolean; explanation?: string } {
  const supported = ['item/commandExecution/requestApproval', 'item/fileChange/requestApproval'].includes(request.method);
  const params = record(request.params);
  const labels: [string, string][] = [
    ['reason', '原因'], ['command', '命令'], ['cwd', '工作目录'], ['grantRoot', '授权目录'],
    ['files', '文件'], ['changes', '改动'],
  ];
  const values = labels.flatMap(([key, label]) => {
    const value = safeStructured(params[key]);
    return value ? [`${label}\n${value}`] : [];
  });
  const context = values.join('\n\n');
  if (!supported || request.desktopOnly) return {
    context, canRespond: false, explanation: '这类请求需要在 Desktop 回答。手机不会代答或放宽权限。',
  };
  const specificScope = request.method === 'item/commandExecution/requestApproval'
    ? Boolean(string(params.command).trim())
    : Boolean(safeStructured(params.files) || safeStructured(params.changes) || string(params.grantRoot).trim());
  if (!context || !specificScope) return {
    context: context || 'Desktop 尚未提供可核对的操作详情。', canRespond: false,
    explanation: '操作内容或授权范围不完整，请在 Desktop 查看完整请求并作决定。',
  };
  // Never offer acceptance beside a silently truncated command or permission
  // scope. The display is bounded only by falling back to Desktop-only actions.
  if (context.length > 65_536) return {
    context: '此请求的详情较长，手机未展示。', canRespond: false,
    explanation: '请在 Desktop 查看完整请求并作决定。',
  };
  const hasUnsafeControl = Array.from(context).some((character) => {
    const code = character.codePointAt(0) ?? 0;
    return (code < 32 && ![9, 10, 13].includes(code)) || code === 127
      || (code >= 8234 && code <= 8238) || (code >= 8294 && code <= 8297);
  });
  if (hasUnsafeControl) return {
    context: '此请求含有无法可靠展示的控制字符。', canRespond: false,
    explanation: '请在 Desktop 核对实际操作内容后作决定。',
  };
  return { context, canRespond: true };
}

export function pendingTitle(request: PendingRequest): string {
  if (request.method === 'item/commandExecution/requestApproval') return '允许执行这条命令吗？';
  if (request.method === 'item/fileChange/requestApproval') return '允许这次文件改动吗？';
  return '需要在 Desktop 处理';
}

export function concisePath(path: string): string {
  const parts = path.replaceAll('\\', '/').split('/').filter(Boolean);
  return parts.slice(-2).join(' / ');
}

export function pendingPreview(request: PendingRequest): string {
  const params = record(request.params);
  return string(params.command) || string(params.reason) || string(params.cwd);
}
