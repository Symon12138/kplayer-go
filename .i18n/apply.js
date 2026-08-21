// Apply translation maps to app.js with uniqueness validation.
const fs = require('fs');
const path = 'E:/Project/AI/grok/kplayer-go/webconsole/static/app.js';
let src = fs.readFileSync(path, 'utf8');
const zones = ['zoneA', 'zoneB', 'zoneC', 'zoneD'];
let applied = 0, skipped = 0, conflicted = 0, errors = [];
for (const z of zones) {
  const f = 'E:/Project/AI/grok/kplayer-go/.i18n/' + z + '.json';
  if (!fs.existsSync(f)) { console.log(z + ': MISSING'); continue; }
  let arr;
  try { arr = JSON.parse(fs.readFileSync(f, 'utf8')); } catch (e) { console.log(z + ': JSON ERROR ' + e.message); continue; }
  for (const item of arr) {
    const o = item.o, n = item.n;
    if (typeof o !== 'string' || typeof n !== 'string' || !o) { errors.push(z + ': bad item ' + JSON.stringify(item).slice(0, 80)); continue; }
    const count = src.split(o).length - 1;
    if (count === 0) { skipped++; continue; }
    if (count > 1) { conflicted++; errors.push(z + ': NOT UNIQUE (' + count + 'x): ' + o.slice(0, 90)); continue; }
    src = src.split(o).join(n);
    applied++;
  }
  console.log(z + ': ' + arr.length + ' items');
}
fs.writeFileSync(path, src);
console.log('APPLIED=' + applied + ' SKIPPED=' + skipped + ' CONFLICTED=' + conflicted);
if (errors.length) { console.log('--- errors (first 20) ---'); errors.slice(0, 20).forEach(e => console.log(e)); }
