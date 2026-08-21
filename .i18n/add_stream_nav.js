const fs = require('fs');
const p = 'E:/Project/AI/grok/kplayer-go/webconsole/static/index.html';
let s = fs.readFileSync(p, 'utf8');
const anchor = '<button class="nav-item" data-view="engine" data-title="推流引擎" type="button">推流引擎</button>';
const add = anchor + '\n        <button class="nav-item" data-view="streams" data-title="多路推流" type="button">多路推流</button>';
if (s.includes(anchor) && !s.includes('data-view="streams"')) {
  s = s.replace(anchor, add);
  fs.writeFileSync(p, s);
  console.log('nav added');
} else {
  console.log('anchor missing or already added');
}