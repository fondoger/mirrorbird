import { el, icon, renderText } from './dom.ts';
import { record, string, type Item, type PendingRequest, type Snapshot } from './types.ts';

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

export function messageRows(snapshot: Snapshot): MessageRow[] {
  const result: MessageRow[] = [];
  for (const turn of snapshot.thread.turns) {
    for (const item of turn.items) {
      result.push({
        key: JSON.stringify([turn.id, item.id]), signature: JSON.stringify(item),
        build: () => renderItem(item),
      });
    }
    if (turn.status === 'interrupted' || turn.status === 'failed') result.push({
      key: JSON.stringify([turn.id, 'turn-status']), signature: turn.status,
      build: () => el('p', `turn-note ${turn.status === 'failed' ? 'turn-note-error' : ''}`,
        turn.status === 'interrupted' ? '本轮已停止。可以继续开始新轮次。' : '本轮未能完成，请在 Desktop 查看详情。'),
    });
  }
  return result;
}

function renderItem(item: Item): HTMLElement {
  if (item.type === 'userMessage' || item.type === 'agentMessage') {
    const user = item.type === 'userMessage';
    const row = el('article', `message ${user ? 'message-user' : 'message-agent'}`);
    row.setAttribute('aria-label', user ? '用户消息，来自共享会话' : 'Codex 消息');
    row.append(el('div', 'message-author', user ? '你与 Desktop' : 'Codex'));
    const body = el('div', 'message-body');
    const text = item.text || inputText(item.content);
    body.append(renderText(text || (user ? '［此消息请在 Desktop 查看］' : '正在接收内容…')));
    row.append(body);
    return row;
  }

  const details = el('details', 'tool-row');
  const summary = el('summary', 'tool-summary');
  const name = toolNames[item.type] ?? 'Desktop 活动';
  summary.append(icon(item.type === 'fileChange' ? 'file' : 'terminal'));
  const heading = el('span', 'tool-heading');
  heading.append(el('span', 'tool-name', name));
  if (item.command || item.name) heading.append(el('span', 'tool-preview', item.command || item.name));
  summary.append(heading);
  const state = statusLabel(item.status ?? '');
  if (state) summary.append(el('span', `tool-status ${item.status === 'failed' ? 'is-error' : ''}`, state));
  summary.append(icon('chevron'));
  const body = el('div', 'tool-body');
  if (item.command) labeledCode(body, '命令', item.command);
  if (item.aggregatedOutput) labeledCode(body, '输出', item.aggregatedOutput);
  if (item.changes !== undefined && item.changes !== null) labeledCode(body, '改动详情', safeStructured(item.changes));
  if (item.text) body.append(renderText(item.text));
  if (item.content !== undefined && item.content !== null) labeledCode(body, '内容', safeStructured(item.content));
  if (!body.childElementCount) body.append(el('p', 'muted', '此活动的完整详情请在 Desktop 查看。'));
  details.append(summary, body);
  return details;
}

function labeledCode(parent: HTMLElement, label: string, text: string): void {
  parent.append(el('p', 'detail-label', label));
  const pre = el('pre', 'tool-output'); pre.append(el('code', '', text)); parent.append(pre);
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
