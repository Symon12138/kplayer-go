/* views/audit.js — 审计日志：谁在什么时候做了什么操作。 */

import { get, post, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, pageHead, fmtTime } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/audit').then(function (res) {
    setConn(true);
    draw(root, listOf(res, ['audit', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, entries) {
  root.innerHTML = '';

  const pruneBtn = el('button', { class: 'btn', type: 'button', text: '清理日志' });
  pruneBtn.addEventListener('click', function () {
    const n = window.prompt('保留最近多少条？（其余删除）', '500');
    if (n === null) { return; }
    const maxEntries = parseInt(n, 10);
    if (!(maxEntries > 0)) { toast('请输入有效数量', 'err'); return; }
    post('/audit/prune', { maxEntries: maxEntries }).then(function (r) {
      toast('已清理 ' + ((r && r.removed) || 0) + ' 条', 'ok'); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  });
  root.appendChild(pageHead('审计日志', '控制台写操作的完整记录（操作人 / 动作 / 目标 / 结果）。', [pruneBtn]));

  /* 过滤器 */
  const filterBar = el('div', { class: 'cluster', style: 'margin-bottom:14px' });
  const fOp = el('input', { type: 'text', placeholder: '操作人', style: 'max-width:150px;min-height:32px;background:var(--bg-0);color:var(--txt-0);border:1px solid var(--line-2);border-radius:8px;padding:5px 10px' });
  const fAction = el('input', { type: 'text', placeholder: '动作（如 media.add）', style: 'max-width:190px;min-height:32px;background:var(--bg-0);color:var(--txt-0);border:1px solid var(--line-2);border-radius:8px;padding:5px 10px' });
  const fResult = el('select', { style: 'min-height:32px;background:var(--bg-0);color:var(--txt-0);border:1px solid var(--line-2);border-radius:8px;padding:5px 10px' });
  ['', 'success', 'failure'].forEach(function (v) {
    fResult.appendChild(el('option', { value: v, text: v === '' ? '全部结果' : v === 'success' ? '成功' : '失败' }));
  });
  const goBtn = el('button', { class: 'btn btn-sm', type: 'button', text: '筛选' });
  filterBar.appendChild(fOp); filterBar.appendChild(fAction); filterBar.appendChild(fResult); filterBar.appendChild(goBtn);
  root.appendChild(filterBar);

  function applyFilter() {
    const q = new URLSearchParams();
    if (fOp.value.trim()) { q.set('operator', fOp.value.trim()); }
    if (fAction.value.trim()) { q.set('action', fAction.value.trim()); }
    if (fResult.value) { q.set('result', fResult.value); }
    get('/audit' + (q.toString() ? '?' + q.toString() : '')).then(function (res) {
      redrawTable(root, listOf(res, ['audit', 'items', 'list']));
    }).catch(function (e) { toast(e.message, 'err'); });
  }
  goBtn.addEventListener('click', applyFilter);

  redrawTable(root, entries);
}

function redrawTable(root, entries) {
  let host = root.querySelector('#auditTableHost');
  if (!host) {
    host = el('div', { id: 'auditTableHost' });
    root.appendChild(host);
  }
  host.innerHTML = '';

  if (!entries.length) {
    host.appendChild(el('div', { class: 'card' }, emptyView('没有匹配的审计记录。', undefined)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>时间</th><th>操作人</th><th>动作</th><th>目标</th><th>结果</th><th>详情</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');
  entries.forEach(function (a) {
    const tr = el('tr', {});
    tr.appendChild(el('td', { class: 'dim', text: fmtTime(a.time) }));
    tr.appendChild(el('td', { text: a.operator || '-' }));
    tr.appendChild(el('td', {}, '<code class="mono">' + esc(a.action || '-') + '</code>'));
    tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(a.target || '-') + '</div>'));
    tr.appendChild(el('td', {}, a.result === 'failure'
      ? '<span class="badge badge-crit">失败</span>'
      : '<span class="badge badge-ok">成功</span>'));
    tr.appendChild(el('td', { class: 'dim', text: a.detail || '-' }));
    tbody.appendChild(tr);
  });
  host.appendChild(wrap);
  host.appendChild(el('p', { class: 'muted', style: 'margin:10px 2px 0;font-size:12px' }, '共 ' + entries.length + ' 条'));
}
