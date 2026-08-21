/* views/nodes.js — 节点管理：推流节点、节点上的实例、下发到节点的远程命令。
 * 多机部署的控制面；单机部署时此页仅作台账。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

const NODE_STATUS = { online: ['badge-ok', '在线'], offline: ['badge-neutral', '离线'], unknown: ['badge-warn', '未知'] };
const INST_STATUS = { running: ['badge-ok', '运行中'], stopped: ['badge-neutral', '已停止'], unknown: ['badge-warn', '未知'] };
const CMD_STATUS = { pending: ['badge-warn', '待执行'], sent: ['badge-info', '已下发'], success: ['badge-ok', '成功'], failed: ['badge-crit', '失败'] };
const ACTIONS = [['start', '启动'], ['stop', '停止'], ['restart', '重启']];

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([
    get('/node/list'),
    get('/instance/list'),
    get('/remote-command/list')
  ]).then(function (res) {
    setConn(true);
    draw(root,
      listOf(res[0], ['nodes', 'items', 'list']),
      listOf(res[1], ['instances', 'items', 'list']),
      listOf(res[2], ['commands', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, nodes, instances, commands) {
  root.innerHTML = '';
  const nodeById = {};
  nodes.forEach(function (n) { nodeById[n.id] = n; });
  const instById = {};
  instances.forEach(function (i) { instById[i.id] = i; });

  root.appendChild(pageHead('节点管理',
    '多机部署控制面：登记推流节点 → 管理节点上的实例 → 下发启动/停止/重启命令。'));

  /* ================= 节点 ================= */
  const nSec = section(root, '节点（' + nodes.length + '）', function () { openNodeEdit(null); });
  if (!nodes.length) {
    nSec.appendChild(emptyCard('暂无节点。登记一台推流服务器（地址供探活/命令下发用）。'));
  } else {
    nSec.appendChild(table(nodes, [
      { h: '名称', cell: function (n) { return '<b>' + esc(n.name) + '</b>'; } },
      { h: '地址', cell: function (n) { return '<div class="path">' + esc(n.address || '-') + '</div>'; }, cls: 'cell-path' },
      { h: '状态', cell: function (n) { const s = NODE_STATUS[n.status] || NODE_STATUS.unknown; return '<span class="badge ' + s[0] + '">' + s[1] + '</span>'; } },
      { h: '最近心跳', cell: function (n) { return '<span class="dim">' + esc(fmtAgo(n.lastSeen)) + '</span>'; } },
      { h: '状态开关', cell: function (n) { return n.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'; } }
    ], [
      { text: '心跳', fn: function (n) { return post('/node/' + encodeURIComponent(n.id) + '/heartbeat', {}); } },
      { text: '启停切换', fn: function (n) { return post('/node/enabled', { id: n.id, enabled: !n.enabled }); } },
      { text: '编辑', edit: function (n) { openNodeEdit(n); } },
      { text: '删除', danger: true, confirm: function (n) { return '删除节点 "' + n.name + '"？（其实例与命令记录保留）'; }, fn: function (n) { return del('/node/' + encodeURIComponent(n.id)); } }
    ]));
  }

  /* ================= 实例 ================= */
  const iSec = section(root, '实例（' + instances.length + '）', function () { openInstanceEdit(null, nodes); });
  if (!instances.length) {
    iSec.appendChild(emptyCard('暂无实例。实例 = 某节点上的一个 KPlayer 进程/频道。'));
  } else {
    iSec.appendChild(table(instances, [
      { h: '名称', cell: function (i) { return '<b>' + esc(i.name) + '</b>'; } },
      { h: '节点', cell: function (i) { return esc((nodeById[i.nodeId] && nodeById[i.nodeId].name) || i.nodeId || '-'); } },
      { h: '频道 ID', cell: function (i) { return '<span class="path">' + esc(i.channelId || '-') + '</span>'; }, cls: 'cell-path' },
      { h: '状态', cell: function (i) { const s = INST_STATUS[i.status] || INST_STATUS.unknown; return '<span class="badge ' + s[0] + '">' + s[1] + '</span>'; } }
    ], [
      { text: '标为运行', fn: function (i) { return post('/instance/status', { id: i.id, status: 'running' }); } },
      { text: '标为停止', fn: function (i) { return post('/instance/status', { id: i.id, status: 'stopped' }); } },
      { text: '编辑', edit: function (i) { openInstanceEdit(i, nodes); } },
      { text: '删除', danger: true, confirm: function (i) { return '删除实例 "' + i.name + '"？'; }, fn: function (i) { return del('/instance/' + encodeURIComponent(i.id)); } }
    ]));
  }

  /* ================= 远程命令 ================= */
  const cSec = section(root, '远程命令（' + commands.length + '）', function () { openCommandEdit(nodes, instances); }, function () {
    post('/remote-command/purge', { maxKeep: 50 }).then(function (r) {
      toast('已清理 ' + ((r && r.removed) || 0) + ' 条终态命令', 'ok'); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  }, '清理终态');
  if (!commands.length) {
    cSec.appendChild(emptyCard('暂无远程命令。向节点下发启动/停止/重启实例的指令。'));
  } else {
    cSec.appendChild(table(commands, [
      { h: '节点', cell: function (c) { return esc((nodeById[c.nodeId] && nodeById[c.nodeId].name) || c.nodeId || '-'); } },
      { h: '实例', cell: function (c) { return esc((instById[c.instanceId] && instById[c.instanceId].name) || c.instanceId || '-'); } },
      { h: '动作', cell: function (c) { const a = ACTIONS.find(function (x) { return x[0] === c.action; }); return '<span class="badge badge-info">' + esc(a ? a[1] : (c.action || '-')) + '</span>'; } },
      { h: '状态', cell: function (c) { const s = CMD_STATUS[c.status] || CMD_STATUS.pending; return '<span class="badge ' + s[0] + '">' + s[1] + '</span>'; } },
      { h: '错误', cell: function (c) { return '<div class="path">' + esc(c.error || '-') + '</div>'; }, cls: 'cell-path' },
      { h: '创建', cell: function (c) { return '<span class="dim">' + esc(fmtAgo(c.createdAt)) + '</span>'; } }
    ], [
      { text: '标为已下发', fn: function (c) { return post('/remote-command/' + encodeURIComponent(c.id) + '/sent', {}); } },
      { text: '标为成功', fn: function (c) { return post('/remote-command/' + encodeURIComponent(c.id) + '/success', {}); } },
      {
        text: '标为失败', prompt: function () { return window.prompt('失败原因（可选）：') || ''; },
        fn: function (c, note) { return post('/remote-command/' + encodeURIComponent(c.id) + '/failed', { id: c.id, error: note }); }
      }
    ]));
  }
}

/* ---- 通用小件 ---- */
function section(root, title, onNew, onExtra, extraLabel) {
  const sec = el('div', { class: 'section' });
  const row = el('div', { class: 'row' }, '<h2>' + esc(title) + '</h2>');
  const box = el('div', { class: 'cluster' });
  if (onExtra) {
    const eb = el('button', { class: 'btn btn-sm', type: 'button', text: extraLabel || '清理' });
    eb.addEventListener('click', onExtra);
    box.appendChild(eb);
  }
  const btn = el('button', { class: 'btn btn-primary btn-sm', type: 'button', text: '+ 新建' });
  btn.addEventListener('click', onNew);
  box.appendChild(btn);
  row.appendChild(box);
  sec.appendChild(row);
  root.appendChild(sec);
  return sec;
}

function emptyCard(msg) {
  const card = el('div', { class: 'card' });
  card.appendChild(emptyView(msg, undefined));
  return card;
}

function table(rows, cols, actions) {
  const wrap = el('div', { class: 'table-wrap' });
  let head = cols.map(function (c) { return '<th>' + esc(c.h) + '</th>'; }).join('');
  head += '<th class="actions" style="text-align:right">操作</th>';
  wrap.innerHTML = '<table class="data"><thead><tr>' + head + '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');
  rows.forEach(function (r) {
    const tr = el('tr', {});
    cols.forEach(function (c) {
      tr.appendChild(el('td', { class: c.cls || '' }, c.cell(r)));
    });
    const td = el('td', { class: 'actions' });
    const box = el('div', { class: 'icon-btn-row' });
    actions.forEach(function (a) {
      const b = el('button', { class: 'btn btn-sm' + (a.danger ? ' btn-danger' : ''), type: 'button', text: a.text });
      b.addEventListener('click', function () {
        if (a.edit) { a.edit(r); return; }
        const note = a.prompt ? a.prompt(r) : undefined;
        if (a.prompt && note === null) { return; }
        const msg = a.confirm ? a.confirm(r) : '';
        if (a.confirm && !window.confirm(msg)) { return; }
        a.fn(r, note).then(function () { toast('已' + a.text, 'ok'); render(); })
          .catch(function (e) { toast(e.message, 'err'); });
      });
      box.appendChild(b);
    });
    td.appendChild(box);
    tr.appendChild(td);
    tbody.appendChild(tr);
  });
  return wrap;
}

/* ---- 节点模态 ---- */
function openNodeEdit(n) {
  const editing = !!n;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑节点' : '登记节点') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="nName" type="text" value="' + esc(editing ? n.name : '') + '"></div>' +
    '<div class="field"><label>地址（host:port 或 URL）</label><input id="nAddr" type="text" class="mono" value="' + esc(editing ? (n.address || '') : '') + '" placeholder="192.168.1.10:4156"></div>' +
    '</div>' +
    '<div class="cluster mt"><label class="check"><input id="nEnabled" type="checkbox"' + (!editing || n.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="nCancel" class="btn" type="button">取消</button>' +
    '<button id="nSave" class="btn btn-primary" type="button">保存</button></div>'));
  m.querySelector('#nCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#nSave').addEventListener('click', function () {
    const name = m.querySelector('#nName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    const body = { name: name, address: m.querySelector('#nAddr').value.trim(), enabled: m.querySelector('#nEnabled').checked };
    const req = editing
      ? post('/node/' + encodeURIComponent(n.id) + '/update', Object.assign({ id: n.id }, body))
      : post('/node/add', body);
    req.then(function () { toast(editing ? '已更新' : '已登记', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}

/* ---- 实例模态 ---- */
function openInstanceEdit(i, nodes) {
  const editing = !!i;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑实例' : '登记实例') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="iName" type="text" value="' + esc(editing ? i.name : '') + '"></div>' +
    '<div class="field"><label>所属节点</label><select id="iNode" class="mono"></select></div>' +
    '<div class="field"><label>状态</label><select id="iStatus">' +
    '<option value="stopped">已停止</option><option value="running">运行中</option><option value="unknown">未知</option>' +
    '</select></div>' +
    '<div class="field"><label>频道 ID（可选）</label><input id="iChannel" type="text" class="mono" value="' + esc(editing ? (i.channelId || '') : '') + '"></div>' +
    '</div>' +
    '<div class="form-actions"><button id="iCancel" class="btn" type="button">取消</button>' +
    '<button id="iSave" class="btn btn-primary" type="button">保存</button></div>'));
  const sel = m.querySelector('#iNode');
  nodes.forEach(function (n) { sel.appendChild(el('option', { value: n.id, text: n.name })); });
  if (editing) {
    sel.value = i.nodeId;
    m.querySelector('#iStatus').value = i.status || 'stopped';
  }
  m.querySelector('#iCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#iSave').addEventListener('click', function () {
    const name = m.querySelector('#iName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    if (!sel.value) { toast('请选择节点', 'err'); return; }
    const body = {
      nodeId: sel.value, name: name,
      status: m.querySelector('#iStatus').value,
      channelId: m.querySelector('#iChannel').value.trim()
    };
    const req = editing
      ? post('/instance/' + encodeURIComponent(i.id) + '/update', Object.assign({ id: i.id }, body))
      : post('/instance/add', body);
    req.then(function () { toast(editing ? '已更新' : '已登记', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}

/* ---- 远程命令模态 ---- */
function openCommandEdit(nodes, instances) {
  const m = modal(el('div', { class: 'modal' },
    '<h3>下发远程命令</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>目标节点</label><select id="cmdNode" class="mono"></select></div>' +
    '<div class="field"><label>实例（可选）</label><select id="cmdInst" class="mono"></select></div>' +
    '</div>' +
    '<div class="field mt"><label>动作</label><select id="cmdAction">' +
    ACTIONS.map(function (a) { return '<option value="' + a[0] + '">' + a[1] + '</option>'; }).join('') +
    '</select></div>' +
    '<div class="form-actions"><button id="cmdCancel" class="btn" type="button">取消</button>' +
    '<button id="cmdSave" class="btn btn-primary" type="button">下发</button></div>'));
  const nodeSel = m.querySelector('#cmdNode');
  const instSel = m.querySelector('#cmdInst');
  nodes.forEach(function (n) { nodeSel.appendChild(el('option', { value: n.id, text: n.name })); });
  instSel.appendChild(el('option', { value: '', text: '-- 整个节点 --' }));
  function fillInst() {
    instSel.innerHTML = '';
    instSel.appendChild(el('option', { value: '', text: '-- 整个节点 --' }));
    instances.filter(function (i) { return i.nodeId === nodeSel.value; })
      .forEach(function (i) { instSel.appendChild(el('option', { value: i.id, text: i.name })); });
  }
  nodeSel.addEventListener('change', fillInst);
  fillInst();
  m.querySelector('#cmdCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#cmdSave').addEventListener('click', function () {
    if (!nodeSel.value) { toast('请选择节点', 'err'); return; }
    post('/remote-command/add', {
      nodeId: nodeSel.value,
      instanceId: instSel.value || undefined,
      action: m.querySelector('#cmdAction').value
    }).then(function () { toast('命令已入队', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
