/* views/alarms.js — 告警中心：推流中断、调度失败、Webhook 投递失败等运营告警。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, pageHead, fmtAgo } from '../ui.js';

const LEVEL_BADGE = {
  info: ['badge-info', '提示'],
  warning: ['badge-warn', '警告'],
  critical: ['badge-crit', '严重']
};

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/alarm/list').then(function (res) {
    setConn(true);
    draw(root, listOf(res, ['alarms', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, alarms) {
  root.innerHTML = '';

  const active = alarms.filter(function (a) { return a.status === 'active'; });
  const resolveAllBtn = el('button', { class: 'btn', type: 'button', text: '全部解决 (' + active.length + ')' });
  resolveAllBtn.disabled = !active.length;
  resolveAllBtn.addEventListener('click', function () {
    post('/alarm/resolve-all', {}).then(function (r) {
      toast('已解决 ' + ((r && r.resolved) || 0) + ' 条告警', 'ok'); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  });
  root.appendChild(pageHead('告警', '推流中断、调度失败、投递异常等运营事件的集中处理。', [resolveAllBtn]));

  if (!alarms.length) {
    root.appendChild(el('div', { class: 'card' }, emptyView('没有告警 —— 一切正常。', undefined)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>级别</th><th>标题</th><th>详情</th><th>状态</th><th>时间</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  alarms.forEach(function (a) {
    const lv = LEVEL_BADGE[a.level] || ['badge-neutral', a.level];
    const isActive = a.status === 'active';
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<span class="badge ' + lv[0] + '">' + esc(lv[1]) + '</span>'));
    tr.appendChild(el('td', {}, '<b>' + esc(a.title) + '</b>'));
    tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(a.message || '-') + '</div>'));
    tr.appendChild(el('td', {}, isActive
      ? '<span class="badge badge-crit">待处理</span>'
      : '<span class="badge badge-ok">已解决</span>'));
    tr.appendChild(el('td', { class: 'dim', text: fmtAgo(a.createdAt) + (a.resolvedAt ? '（解决于 ' + fmtAgo(a.resolvedAt) + '）' : '') }));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });
    if (isActive) {
      const btnResolve = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '解决' });
      btnResolve.addEventListener('click', function () {
        post('/alarm/resolve', { id: a.id }).then(function () { toast('已解决', 'ok'); render(); })
          .catch(function (e) { toast(e.message, 'err'); });
      });
      row.appendChild(btnResolve);
    }
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      del('/alarm/' + encodeURIComponent(a.id)).then(function () { toast('已删除', 'ok'); render(); })
        .catch(function (e) { toast(e.message, 'err'); });
    });
    row.appendChild(btnDel);
    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}
