/* views/snapshots.js — 配置快照：业务数据的手动备份与回滚。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtTime } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/config-snapshot/list').then(function (res) {
    setConn(true);
    draw(root, listOf(res, ['snapshots', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, snapshots) {
  root.innerHTML = '';

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 创建快照' });
  newBtn.addEventListener('click', openCreate);
  root.appendChild(pageHead('配置快照',
    '把当前全部业务数据（媒体/节目单/任务/推流任务等）存为快照，可随时回滚。', [newBtn]));

  if (!snapshots.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '创建第一个快照' });
    act.addEventListener('click', openCreate);
    root.appendChild(el('div', { class: 'card' }, emptyView('还没有快照。大改配置前先拍一个。', act)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>时间</th><th>操作人</th><th>说明</th><th>数据指纹</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  snapshots.forEach(function (s) {
    const tr = el('tr', {});
    tr.appendChild(el('td', { text: fmtTime(s.createdAt) }));
    tr.appendChild(el('td', { text: s.operator || '-' }));
    tr.appendChild(el('td', { text: s.description || '-' }));
    tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc((s.dataHash || '-').slice(0, 16)) + '</div>'));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });
    const btnRestore = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '回滚到此' });
    btnRestore.addEventListener('click', function () {
      if (!window.confirm('回滚到该快照？当前业务数据将被快照内容覆盖（引擎配置不受影响）。')) { return; }
      post('/config-snapshot/' + encodeURIComponent(s.id) + '/restore', {})
        .then(function () { toast('已回滚', 'ok'); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnRestore);
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('删除该快照？')) { return; }
      del('/config-snapshot/' + encodeURIComponent(s.id)).then(function () { toast('已删除', 'ok'); render(); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnDel);
    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}

function openCreate() {
  const m = modal(el('div', { class: 'modal' },
    '<h3>创建配置快照</h3>' +
    '<div class="field"><label>说明（可选）</label><input id="sDesc" type="text" placeholder="例如：大改节目前备份"></div>' +
    '<div class="form-actions"><button id="sCancel" class="btn" type="button">取消</button>' +
    '<button id="sSave" class="btn btn-primary" type="button">创建</button></div>'));
  m.querySelector('#sCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#sSave').addEventListener('click', function () {
    post('/config-snapshot/add', { operator: 'console', description: m.querySelector('#sDesc').value.trim() })
      .then(function () { toast('快照已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
