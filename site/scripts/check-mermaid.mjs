// Copied from Porch site/scripts/check-mermaid.mjs (Apache-2.0, same author).
// Parses every ```mermaid block in docs/, site/ and README.md with the real Mermaid parser in headless Chromium and fails on any
// syntax error. VitePress renders diagrams client-side, so a broken diagram would otherwise only show up in a browser.
// Flowchart node and edge labels must also be quoted (SM-DOCS-004): unquoted labels break on parentheses, colons and
// other punctuation.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { chromium } from 'playwright-core';

const root = path.resolve(import.meta.dirname, '..', '..');
const files = [];
for (const dir of ['docs', 'site']) {
  const walk = (d) => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      if (e.name === 'node_modules' || e.name.startsWith('.')) continue;
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else if (p.endsWith('.md')) files.push(p);
    }
  };
  walk(path.join(root, dir));
}
files.push(path.join(root, 'README.md'));
const blocks = [];
for (const f of files) {
  const re = /```mermaid\n([\s\S]*?)```/g;
  const s = fs.readFileSync(f, 'utf8');
  let m;
  let i = 0;
  while ((m = re.exec(s))) blocks.push({ f: path.relative(root, f), i: i++, src: m[1] });
}
// Returns the lines of a flowchart whose node or edge labels are not quoted.
function unquotedLabels(src) {
  if (!/^\s*(flowchart|graph)\b/.test(src)) return [];
  const bad = [];
  for (const line of src.split('\n').slice(1)) {
    let l = line.split('%%')[0].replace(/"[^"]*"/g, '""');
    l = l.replace(/(\[\(|\(\[|\[\[|\(\(|\{\{|\[|\(|\{|>)""/g, '');
    const edges = [...l.matchAll(/\|([^|]*)\|/g)].map((m) => m[1]);
    if (/\w\s*[[({>]/.test(l.replace(/\|[^|]*\|/g, '')) || edges.some((e) => e !== '""')) bad.push(line.trim());
  }
  return bad;
}
let unquoted = 0;
for (const b of blocks) {
  for (const line of unquotedLabels(b.src)) {
    unquoted++;
    console.error(`${b.f} diagram #${b.i}: quote this label: ${line}`);
  }
}
const mermaidJS = createRequire(import.meta.url).resolve('mermaid/dist/mermaid.min.js');
// SM_CHROMIUM_ARGS: extra launch arguments, space-separated, for hosts whose sandbox stops Chromium from starting (the
// same hook as web/playwright.config.ts). Unset by default.
const args = (process.env.SM_CHROMIUM_ARGS ?? '').split(/\s+/).filter(Boolean);
const browser = await chromium.launch(args.length ? { args } : {});
const page = await browser.newPage();
await page.setContent('<!doctype html><html><body></body></html>');
await page.addScriptTag({ path: mermaidJS });
let bad = 0;
for (const b of blocks) {
  const err = await page.evaluate(async (src) => {
    try {
      await window.mermaid.parse(src);
      return null;
    } catch (e) {
      return String(e?.message ?? e).slice(0, 400);
    }
  }, b.src);
  if (err) {
    bad++;
    console.error(`${b.f} diagram #${b.i}:\n${err}\n`);
  }
}
await browser.close();
console.log(`mermaid: ${blocks.length} diagrams, ${bad} with errors, ${unquoted} unquoted labels`);
process.exit(bad || unquoted ? 1 : 0);
