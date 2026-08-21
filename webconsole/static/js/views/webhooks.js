/* views/webhooks.js — Webhook 订阅：把领域事件推送到外部系统。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

const EVENTS = [
  ['output_disconnected', '输出断开'],
  ['channel_status_changed', '频道状态变化'],
  ['material_failed', '素材/播放失败'],
  ['task_completed', '任务完成'],
  ['engine_exited', '引擎异常退出']
];

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/webhook/list').then(function (res) {
    setConn(true);
    draw(root, listOf(res, ['webhooks', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, webhooks) {
  root.innerHTML = '';

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建订阅' });
  newBtn.addEventListener('click', function () { openEdit(null); });
  root.appendChild(pageHead('Webhook',
    '把引擎退出、播放失败等事件实时 POST 到外部地址（告警系统/IM 机器人）。', [newBtn]));

  if (!webhooks.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '新建第一个订阅' });
    act.addEventListener('click', function () { openEdit(null); });
    root.appendChild(el('div', { class: 'card' }, emptyView('暂无 Webhook 订阅。', act)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>名称</th><th>地址</th><th>订阅事件</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  webhooks.forEach(function (w) {
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<b>' + esc(w.name || w.id) + '</b>'));
    tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(w.url || '-') + '</div>'));
    const evTd = el('td', {});
    (w.events || []).forEach(function (e2) {
      const hit = EVENTS.find(function (x) { return x[0] === e2; });
      evTd.appendChild(el('span', { class: 'badge badge-info', text: hit ? hit[1] : e2, style: 'margin:1px 4px 1px 0' }));
    });
    if (!(w.events || []).length) { evTd.textContent = '-'; }
    tr.appendChild(evTd);
    tr.appendChild(el('td', {}, w.enabled
      ? '<span class="badge badge-ok">启用</span>'
      : '<span class="badge badge-neutral">停用</span>'));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });

    const btnLog = el('button', { class: 'btn btn-sm', type: 'button', text: '投递记录' });
    btnLog.addEventListener('click', function () { openDeliveries(w); });
    row.appendChild(btnLog);

    const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: w.enabled ? '停用' : '启用' });
    btnToggle.addEventListener('click', function () {
      post('/webhook/enabled', { id: w.id, enabled: !w.enabled }).then(function () {
        toast('已更新', 'ok'); render();
      }).catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnToggle);

    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openEdit(w); });
    row.appendChild(btnEdit);

    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('删除订阅 "' + (w.name || w.id) + '"？（投递历史保留）')) { return; }
      del('/webhook/' + encodeURIComponent(w.id)).then(function () { toast('已删除', 'ok'); render(); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnDel);

    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}

/* ---------- 新建 / 编辑 ---------- */
function openEdit(w) {
  const editing = !!w;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑订阅' : '新建 Webhook 订阅') + '</h3>' +
    '<div class="form-grid">' +
    '<div class="field"><label>名称</label><input id="wName" type="text" value="' + esc(editing ? (w.name || '') : '') + '"></div>' +
    '<div class="field"><label>目标 URL（接收 POST 的 http(s) 地址）</label>' +
    '<input id="wUrl" type="text" class="mono" placeholder="https://example.com/hook" value="' + esc(editing ? (w.url || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>订阅事件（至少选一项）</label><div id="wEvents" class="cluster" style="gap:10px"></div></div>' +
    '<div class="cluster mt"><label class="check"><input id="wEnabled" type="checkbox"' + (!editing || w.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="wCancel" class="btn" type="button">取消</button>' +
    '<button id="wSave" class="btn btn-primary" type="button">保存</button></div>'));

  const evBox = m.querySelector('#wEvents');
  EVENTS.forEach(function (ev) {
    const label = el('label', { class: 'check' });
    const cb = el('input', { type: 'checkbox', value: ev[0] });
    if (editing && (w.events || []).indexOf(ev[0]) >= 0) { cb.checked = true; }
    label.appendChild(cb);
    label.appendChild(document.createTextNode(ev[1]));
    evBox.appendChild(label);
  });

  m.querySelector('#wCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#wSave').addEventListener('click', function () {
    const name = m.querySelector('#wName').value.trim();
    const url = m.querySelector('#wUrl').value.trim();
    const events = Array.from(m.querySelectorAll('#wEvents input:checked')).map(function (c) { return c.value; });
    if (!name) { toast('请填写名称', 'err'); return; }
    if (!url || url.indexOf('http') !== 0) { toast('请填写有效的 http(s) 地址', 'err'); return; }
    if (!events.length) { toast('请至少选择一个事件', 'err'); return; }
    const body = { name: name, url: url, events: events, enabled: m.querySelector('#wEnabled').checked };
    const req = editing
      ? post('/webhook/' + encodeURIComponent(w.id) + '/update', Object.assign({ id: w.id }, body))
      : post('/webhook/add', body);
    req.then(function () { toast(editing ? '订阅已更新' : '订阅已创建', 'ok'); m.remove(); render(); })
      .catch(function (e2) { toast(e2.message, 'err'); });
  });
}

/* ---------- 投递记录 ---------- */
function openDeliveries(w) {
  const m = modal(el('div', { class: 'modal wide' },
    '<h3>投递记录 — ' + esc(w.name || w.id) + '</h3>' +
    '<div class="delivery-host"><div class="loading"><div class="spinner"></div><div class="muted">加载中 ...</div></div></div>' +
    '<div class="form-actions"><button class="btn" type="button">关闭</button></div>'), true);
  m.querySelector('.form-actions .btn').addEventListener('click', function () { m.remove(); });

  get('/webhook/' + encodeURIComponent(w.id) + '/deliveries').then(function (res) {
    const list = listOf(res, ['deliveries', 'items', 'list']);
    const host = m.querySelector('.delivery-host');
    host.innerHTML = '';
    if (!list.length) {
      host.appendChild(el('p', { class: 'muted', text: '还没有投递记录。' }));
      return;
    }
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>事件</th><th>结果</th><th>尝试次数</th><th>错误</th><th>时间</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    list.forEach(function (d) {
      const ok = d.status === 'success';
      const tr = el('tr', {});
      tr.appendChild(el('td', { text: d.event || '-' }));
      tr.appendChild(el('td', {}, ok
        ? '<span class="badge badge-ok">成功</span>'
        : '<span class="badge badge-crit">失败</span>'));
      tr.appendChild(el('td', { text: String(d.attempts != null ? d.attempts : '-') }));
      tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(d.lastError || '-') + '</div>'));
      tr.appendChild(el('td', { class: 'dim', text: fmtAgo(d.deliveredAt || d.createdAt) }));
      tbody.appendChild(tr);
    });
    host.appendChild(wrap);
  }).catch(function (e2) {
    m.querySelector('.delivery-host').innerHTML = '';
    m.querySelector('.delivery-host').appendChild(el('p', { class: 'muted', text: '加载失败：' + e2.message }));
  });
}
