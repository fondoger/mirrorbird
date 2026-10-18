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

export type IconName = 'mirror' | 'chevron' | 'refresh' | 'arrow-up' | 'close' | 'terminal' | 'file' | 'check' | 'stop' | 'lock' | 'arrow-down' | 'threads' | 'external';
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

// Minimal text formatting, never HTML parsing, links, images, or remote fetches.
// Raw HTML/Markdown links are intentionally visible literal text.
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
    } else if (/^\s*[-*] /.test(line)) {
      flush();
      const list = el('ul', 'message-list');
      const add = (value: string) => { const li = el('li'); inlineText(li, value.replace(/^\s*[-*] /, '')); list.append(li); };
      add(line);
      while (index < lines.length && /^\s*[-*] /.test(lines[index] ?? '')) add(lines[index++] ?? '');
      fragment.append(list);
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
  const pattern = /(`[^`\n]+`|\*\*[^*\n]+\*\*)/g;
  let from = 0;
  for (const match of text.matchAll(pattern)) {
    const index = match.index;
    node.append(document.createTextNode(text.slice(from, index)));
    const code = match[0].startsWith('`');
    node.append(el(code ? 'code' : 'strong', code ? 'inline-code' : '', match[0].slice(code ? 1 : 2, code ? -1 : -2)));
    from = index + match[0].length;
  }
  node.append(document.createTextNode(text.slice(from)));
}
