const fs = require('fs');
const p = 'E:/Project/AI/grok/kplayer-go/webconsole/static/index.html';
let s = fs.readFileSync(p, 'utf8');
const remove = ['audit', 'users', 'nodes', 'remote-commands', 'snapshots', 'templates', 'industry-templates', 'suggestions'];
let removed = 0;
for (const v of remove) {
  const re = new RegExp('<button class="nav-item" data-view="' + v + '"[^>]*>.*?</button>\n', 'g');
  const m = s.match(re);
  if (m) { s = s.replace(re, ''); removed += m.length; }
}
fs.writeFileSync(p, s);
console.log('removed nav buttons: ' + removed);
