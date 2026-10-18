// Read installed ASAR metadata without extracting or executing packaged code.
import fs from 'node:fs';
import crypto from 'node:crypto';

const archive = process.argv[2];
if (!archive) throw new Error('Usage: node tools/inspect-desktop.mjs <absolute app.asar path>');
const fd = fs.openSync(archive, 'r');
try {
  const prefix = Buffer.alloc(16);
  fs.readSync(fd, prefix, 0, prefix.length, 0);
  const headerLength = prefix.readUInt32LE(12);
  if (headerLength > 16 * 1024 * 1024) throw new Error('Unexpected ASAR header size');
  const header = Buffer.alloc(headerLength);
  fs.readSync(fd, header, 0, header.length, 16);
  const root = JSON.parse(header.toString('utf8'));
  const dataOffset = 8 + prefix.readUInt32LE(4);
  const readEntry = (entry) => {
    if (entry.unpacked || entry.size > 16 * 1024 * 1024) return '';
    const data = Buffer.alloc(entry.size);
    fs.readSync(fd, data, 0, data.length, dataOffset + Number(entry.offset));
    return data.toString('utf8');
  };
  const pkg = JSON.parse(readEntry(root.files['package.json']));
  const evidence = [];
  function visit(dir, base = '') {
    for (const [name, entry] of Object.entries(dir.files ?? {})) {
      const path = base + name;
      if (entry.files) visit(entry, path + '/');
      else if (path.startsWith('.vite/build/') && path.endsWith('.js')) {
        const text = readEntry(entry);
        const flags = {
          cliOverride: text.includes('CODEX_CLI_PATH'),
          localDaemon: text.includes('CODEX_APP_SERVER_USE_LOCAL_DAEMON'),
          stdioTransport: text.includes('stdio_transport_spawned'),
          userMessageEvents: text.includes('case`item/started`') && text.includes('type===`userMessage`'),
        };
        if (Object.values(flags).some(Boolean)) {
          evidence.push({ path, sha256: crypto.createHash('sha256').update(text).digest('hex'), ...flags });
        }
      }
    }
  }
  visit(root);
  console.log(JSON.stringify({ version: pkg.version, main: pkg.main, evidence }, null, 2));
} finally {
  fs.closeSync(fd);
}
