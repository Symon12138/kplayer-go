const fs = require('fs');
const p = 'E:/Project/AI/grok/kplayer-go/webconsole/static/app.js';
let s = fs.readFileSync(p, 'utf8');
const pairs = [
  ['type="button">New task</button>', 'type="button">新建任务</button>'],
  ['type="button">Cancel</button>', 'type="button">取消</button>'],
  ['type="button">Save</button>', 'type="button">保存</button>'],
  ['type="button">Add output</button>', 'type="button">添加输出</button>'],
  ["<label>Playlist</label>", "<label>节目单</label>"],
  ["<label>Media</label>", "<label>媒体</label>"],
  ["<label>Path</label>", "<label>路径</label>"],
  ["toast('Name is required', 'err')", "toast('请输入名称', 'err')"],
  ["'<h2>Server</h2>'", "'<h2>服务器</h2>'"],
  ['<th>Status</th>', '<th>状态</th>'],
  ['<th>Created</th>', '<th>创建时间</th>'],
  ["placeholder=\"rtmp://...\"", "placeholder=\"rtmp://...\""],
];
let total = 0;
for (const [o, n] of pairs) {
  const c = s.split(o).length - 1;
  if (c > 0) { s = s.split(o).join(n); total += c; }
}
fs.writeFileSync(p, s);
console.log('patched ' + total + ' spots');
