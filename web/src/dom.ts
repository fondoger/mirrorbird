export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K, className = '', text?: string,
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

export function button(label: string, className: string, action: () => void): HTMLButtonElement {
  const node = el('button', className, label);
  node.type = 'button';
  node.addEventListener('click', action);
  return node;
}

export type IconName = 'mirror' | 'chevron' | 'refresh' | 'arrow-up' | 'close' | 'terminal' | 'file' | 'check' | 'stop' | 'lock' | 'arrow-down' | 'threads' | 'external' | 'menu' | 'plus' | 'search' | 'more' | 'copy' | 'edit' | 'fork' | 'compact' | 'skill' | 'link';
const paths: Record<IconName, string[]> = {
  mirror: ['M4 5h12a2 2 0 0 1 2 2v9H4a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2Z', 'M6 20h7M9.5 16v4', 'M16 10h5a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1h-5a1 1 0 0 1-1-1V11a1 1 0 0 1 1-1Z', 'M18 19h1'],
  chevron: ['m9 5 7 7-7 7'],
  refresh: ['M20 7v5h-5', 'M4 17v-5h5', 'M6.1 7a7 7 0 0 1 11.6-1L20 9', 'M4 15l2.3 3A7 7 0 0 0 18 17'],
  'arrow-up': ['M12 19V5', 'm6 11 6-6 6 6'],
  close: ['m6 6 12 12', 'M18 6 6 18'],
  terminal: ['M4 5h16v14H4Z', 'm7 9 3 3-3 3', 'M13 15h4'],
  file: ['M14 3H5v18h14V8l-5-5Z', 'M14 3v6h5', 'M8 13h8M8 17h5'],
  check: ['m5 12 4 4L19 6'],
  stop: ['M6 6h12v12H6Z'],
  lock: ['M7 10V7a5 5 0 0 1 10 0v3', 'M5 10h14v11H5Z', 'M12 14v3'],
  'arrow-down': ['M12 5v14', 'm6 13 6 6 6-6'],
  threads: ['M4 4h16v12H9l-5 4V4Z', 'M8 8h8M8 12h5'],
  external: ['M14 3h7v7', 'm21 3-9 9', 'M10 5H3v16h16v-7'],
  menu: ['M4 6h16M4 12h16M4 18h16'],
  plus: ['M12 5v14M5 12h14'],
  search: ['m21 21-5.2-5.2M18 10.5a7.5 7.5 0 1 1-15 0 7.5 7.5 0 0 1 15 0'],
  more: ['M5 12h.01M12 12h.01M19 12h.01'],
  copy: ['M9 9h12v12H9zM15 9V3H3v12h6'],
  edit: ['m16 3 5 5M4 20l4-1L21 6l-4-4L4 15l-1 6 5-2'],
  fork: ['M6 3v12a5 5 0 0 0 5 5h7M6 9h7a5 5 0 0 0 5-5V3m-3 14 3 3-3 3'],
  compact: ['M5 3v5h5M19 3v5h-5M5 21v-5h5M19 21v-5h-5'],
  skill: ['m14 3 1.5 5.5L21 10l-5.5 1.5L14 17l-1.5-5.5L7 10l5.5-1.5L14 3M5 15v6M2 18h6'],
  link: ['m10 13 4-4m-6 6-1 1a3.5 3.5 0 0 1-5-5l4-4a3.5 3.5 0 0 1 5 0m2 2 1-1a3.5 3.5 0 0 1 5 5l-4 4a3.5 3.5 0 0 1-5 0'],
};

export function icon(name: IconName): SVGSVGElement {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.7');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');
  svg.classList.add('icon');
  for (const d of paths[name]) {
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', d);
    svg.append(path);
  }
  return svg;
}

export function iconButton(label: string, name: IconName, action: () => void, className = 'icon-button'): HTMLButtonElement {
  const node = button('', className, action);
  node.setAttribute('aria-label', label);
  node.title = label;
  node.append(icon(name));
  return node;
}

export function textButton(label: string, name: IconName, action: () => void, className: string): HTMLButtonElement {
  const node = button('', className, action);
  node.append(icon(name), el('span', '', label));
  return node;
}

export function setText(node: HTMLElement, value: string): void {
  if (node.textContent !== value) node.textContent = value;
}

// Build Markdown with DOM nodes; source HTML and images never become executable markup.
export function renderText(text: string): DocumentFragment {
  const fragment = document.createDocumentFragment();
  const lines = text.split('\n');
  let index = 0;
  let paragraph: string[] = [];
  const flush = () => {
    if (paragraph.length) {
      const p = el('p', 'message-paragraph');
      inlineText(p, paragraph.join('\n'));
      fragment.append(p); paragraph = [];
    }
  };
  while (index < lines.length) {
    const line = lines[index++] ?? '';
    if (line.startsWith('```')) {
      flush();
      const code: string[] = [];
      const language = line.slice(3).trim().slice(0, 32);
      while (index < lines.length && !lines[index]?.startsWith('```')) code.push(lines[index++] ?? '');
      if (lines[index]?.startsWith('```')) index++;
      const block = el('div', 'code-block');
      if (language) block.append(el('div', 'code-language', language));
      const pre = el('pre'); pre.append(el('code', '', code.join('\n'))); block.append(pre);
      fragment.append(block);
    } else if (/^#{1,3} /.test(line)) {
      flush();
      const heading = el('h3', 'message-heading'); inlineText(heading, line.replace(/^#{1,3} /, '')); fragment.append(heading);
    } else if (/^>\s?/.test(line)) {
      flush();
      const quote = [line.replace(/^>\s?/, '')];
      while (index < lines.length && /^>\s?/.test(lines[index] ?? '')) quote.push((lines[index++] ?? '').replace(/^>\s?/, ''));
      const block = el('blockquote'); block.append(renderText(quote.join('\n'))); fragment.append(block);
    } else if (/^\s*(?:[-*]|\d+\.) /.test(line)) {
      flush();
      const ordered = /^\s*\d+\. /.test(line);
      const pattern = ordered ? /^\s*\d+\. / : /^\s*[-*] /;
      const list = ordered ? el('ol', 'message-list') : el('ul', 'message-list');
      const add = (value: string) => { const li = el('li'); inlineText(li, value.replace(pattern, '')); list.append(li); };
      add(line);
      while (index < lines.length && pattern.test(lines[index] ?? '')) add(lines[index++] ?? '');
      fragment.append(list);
    } else if (line.includes('|') && /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(lines[index] ?? '')) {
      flush(); index++;
      const wrap = el('div', 'table-scroll'), table = el('table');
      const cells = (line: string) => line.trim().replace(/^\||\|$/g, '').split('|').map((v) => v.trim());
      const head = el('tr'); for (const value of cells(line)) { const th = el('th'); inlineText(th, value); head.append(th); }
      table.append(head);
      while (index < lines.length && lines[index]?.includes('|')) {
        const tr = el('tr'); for (const value of cells(lines[index++] ?? '')) { const td = el('td'); inlineText(td, value); tr.append(td); }
        table.append(tr);
      }
      wrap.append(table); fragment.append(wrap);
    } else if (!line.trim()) {
      flush();
    } else {
      paragraph.push(line);
    }
  }
  flush();
  return fragment;
}

function inlineText(node: HTMLElement, text: string): void {
  const pattern = /(`[^`\n]+`|\*\*[^*\n]+\*\*|\*[^*\n]+\*|\[[^\]\n]+\]\(https?:\/\/[^\s)]+\))/g;
  let from = 0;
  for (const match of text.matchAll(pattern)) {
    const index = match.index;
    node.append(document.createTextNode(text.slice(from, index)));
    const link = match[0].match(/^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/);
    if (link) {
      const a = el('a', '', link[1]); a.href = link[2] ?? ''; a.target = '_blank'; a.rel = 'noopener noreferrer'; node.append(a);
    } else {
      const code = match[0].startsWith('`'), strong = match[0].startsWith('**');
      node.append(el(code ? 'code' : strong ? 'strong' : 'em', code ? 'inline-code' : '', match[0].slice(strong ? 2 : 1, strong ? -2 : -1)));
    }
    from = index + match[0].length;
  }
  node.append(document.createTextNode(text.slice(from)));
}
