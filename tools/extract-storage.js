// Extract console-storage files from the workflow agent result.
const fs = require('fs');
const path = require('path');

const journal = 'C:/Users/wjy13/.claude/projects/D--myProject-cloudplatform/d784c718-8141-4ae0-85a6-78fe87c21dc3/subagents/workflows/wf_a2ab94c7-d0b/journal.jsonl';
const lines = fs.readFileSync(journal, 'utf8').trim().split('\n');
let str = '';
for (const line of lines) {
  const j = JSON.parse(line);
  if (j.type === 'result' && j.agentId === 'a4ef6f9847bd24f6a') {
    str = typeof j.result === 'string' ? j.result : JSON.stringify(j.result);
    break;
  }
}
if (!str) { console.error('storage agent result not found'); process.exit(1); }

const idx = str.lastIndexOf('{"files":');
if (idx < 0) { console.error('no embedded files json'); process.exit(1); }
const jsonStr = str.slice(idx);

// find balanced braces, respecting strings + escapes
let depth = 0, end = 0, inStr = false, esc = false;
for (let i = 0; i < jsonStr.length; i++) {
  const c = jsonStr[i];
  if (esc) { esc = false; continue; }
  if (c === '\\') { esc = true; continue; }
  if (inStr) { if (c === '"') inStr = false; continue; }
  if (c === '"') { inStr = true; continue; }
  if (c === '{') depth++;
  if (c === '}') { depth--; if (depth === 0) { end = i + 1; break; } }
}

let obj;
try { obj = JSON.parse(jsonStr.slice(0, end)); }
catch (e) { console.error('json parse failed:', e.message); process.exit(1); }

console.log('found ' + obj.files.length + ' files');
const base = 'D:/myProject/cloudplatform/platform/frontend/';
for (const f of obj.files) {
  const p = path.join(base, f.path);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, f.content, 'utf8');
  console.log('wrote ' + f.path + ' (' + f.content.length + ' chars)');
}
