// Patch pass: apply conflicted (multi-instance) items globally — they are
// shared UI strings (button texts/toasts/labels) with identical semantics.
const fs = require('fs');
const path = 'E:/Project/AI/grok/kplayer-go/webconsole/static/app.js';
let src = fs.readFileSync(path, 'utf8');
const zones = ['zoneA', 'zoneB', 'zoneC', 'zoneD'];
let patched = 0, zero = 0;
for (const z of zones) {
  const f = 'E:/Project/AI/grok/kplayer-go/.i18n/' + z + '.json';
  const arr = JSON.parse(fs.readFileSync(f, 'utf8'));
  for (const item of arr) {
    const o = item.o, n = item.n;
    const count = src.split(o).length - 1;
    if (count === 0) { zero++; continue; }
    if (count === 1) continue; // already handled
    // multi-instance: global replace (same semantics everywhere)
    const parts = src.split(o);
    src = parts.join(n);
    patched += parts.length - 1;
  }
}
fs.writeFileSync(path, src);
console.log('PATCHED=' + patched + ' ZERO=' + zero);
