import { readFileSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const require = createRequire(new URL('../web/package.json', import.meta.url));
const { chromium } = require('playwright');
const source = readFileSync(new URL('../web/public/favicon.svg', import.meta.url), 'utf8');
const sizes = [16, 24, 32, 48, 64, 128, 192, 256, 512];
const browser = await chromium.launch({ channel: 'msedge', headless: true });
const images = new Map();
try {
  const page = await browser.newPage();
  for (const size of sizes) {
    const data = await page.evaluate(async ({ source, size }) => {
      const image = new Image();
      image.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(source)}`;
      await image.decode();
      const canvas = document.createElement('canvas');
      canvas.width = canvas.height = size;
      canvas.getContext('2d').drawImage(image, 0, 0, size, size);
      return canvas.toDataURL('image/png').split(',')[1];
    }, { source, size });
    images.set(size, Buffer.from(data, 'base64'));
  }
} finally {
  await browser.close();
}

const iconSizes = sizes.filter((size) => size <= 256 && size !== 192);
const directory = Buffer.alloc(6 + iconSizes.length * 16);
directory.writeUInt16LE(1, 2);
directory.writeUInt16LE(iconSizes.length, 4);
let offset = directory.length;
for (const [index, size] of iconSizes.entries()) {
  const entry = 6 + index * 16;
  directory[entry] = directory[entry + 1] = size % 256;
  directory.writeUInt16LE(1, entry + 4);
  directory.writeUInt16LE(32, entry + 6);
  directory.writeUInt32LE(images.get(size).length, entry + 8);
  directory.writeUInt32LE(offset, entry + 12);
  offset += images.get(size).length;
}

for (const [name, data] of [
  ['web/public/icon-192.png', images.get(192)],
  ['web/public/icon-512.png', images.get(512)],
  ['internal/desktop/app-icon.png', images.get(512)],
  ['internal/desktop/app-icon.ico', Buffer.concat([directory, ...iconSizes.map((size) => images.get(size))])],
]) {
  writeFileSync(new URL(`../${name}`, import.meta.url), data);
  console.log(`Generated ${fileURLToPath(new URL(`../${name}`, import.meta.url))}`);
}
