/* views/caches.js — 缓存任务：预生成缓存的管理台账（状态流转手动标记）。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

const STATUS = {
  pending: ['badge-neutral', '待处理'],
  running: ['badge-warn', '进行中'],
  done: ['badge-ok', '完成'],
  failed: ['badge-crit', '失败']
};

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([get('/cache-task/list'), get('/media/list')]).then(function (res) {
    setConn(true);
    draw(root,
      listOf(res[0], ['cacheTasks', 'items', 'list']),
      listOf(res[1], ['media', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, tasks, media) {
  root.innerHTML = '';
  const mediaById = {};
  media.forEach(function (m) { mediaById[m.id] = m; });

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建缓存任务' });
  newBtn.addEventListener('click', function () { openEdit(null, media); });
  root.appendChild(pageHead('缓存任务',
    '预生成缓存的台账管理：登记要预热的媒体并跟踪状态流转（当前 FFmpeg 引擎为实时转码，此页用于规划与跟踪）。',
    [newBtn]));

  if (!tasks.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '新建第一个缓存任务' });
    act.addEventListener('click', function () { openEdit(null, media); });
    root.appendChild(el('div', { class: 'card' }, emptyView('暂无缓存任务。', act)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>媒体</th><th>状态</th><th>备注</th><th>更新</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  tasks.forEach(function (t) {
    const st = STATUS[t.status] || ['badge-neutral', t.status || '-'];
    const m = mediaById[t.mediaId];
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<b>' + esc(m ? (m.name || m.path) : (t.mediaId || '-')) + '</b>'));
    tr.appendChild(el('td', {}, '<span class="badge ' + st[0] + '">' + esc(st[1]) + '</span>'));
    tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(t.note || '-') + '</div>'));
    tr.appendChild(el('td', { class: 'dim', text: fmtAgo(t.updatedAt) }));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });
    [['running', '标为进行中'], ['done', '标为完成']].forEach(function (act) {
      if (t.status === act[0]) { return; }
      const b = el('button', { class: 'btn btn-sm', type: 'button', text: act[1] });
      b.addEventListener('click', function () {
        post('/cache-task/' + encodeURIComponent(t.id) + '/' + act[0], {})
          .then(function () { toast('已更新', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(b);
    });
    if (t.status !== 'failed') {
      const bf = el('button', { class: 'btn btn-sm', type: 'button', text: '标为失败' });
      bf.addEventListener('click', function () {
        const note = window.prompt('失败备注（可选）：') || '';
        post('/cache-task/' + encodeURIComponent(t.id) + '/failed', { id: t.id, note: note })
          .then(function () { toast('已更新', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(bf);
    }
    const bd = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    bd.addEventListener('click', function () {
      if (!window.confirm('删除该缓存任务？')) { return; }
      del('/cache-task/' + encodeURIComponent(t.id)).then(function () { toast('已删除', 'ok'); render(); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(bd);
    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}

function openEdit(t, media) {
  const editing = !!t;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑缓存任务' : '新建缓存任务') + '</h3>' +
    '<div class="field"><label>媒体</label><select id="cMedia" class="mono"></select></div>' +
    '<div class="field mt"><label>备注（可选）</label><input id="cNote" type="text" value="' + esc(editing ? (t.note || '') : '') + '"></div>' +
    '<div class="form-actions"><button id="cCancel" class="btn" type="button">取消</button>' +
    '<button id="cSave" class="btn btn-primary" type="button">保存</button></div>'));
  const sel = m.querySelector('#cMedia');
  sel.appendChild(el('option', { value: '', text: '-- 选择媒体 --' }));
  media.forEach(function (mm) {
    sel.appendChild(el('option', { value: mm.id, text: (mm.isDir ? '[目录] ' : '') + (mm.name || mm.path) }));
  });
  if (editing) { sel.value = t.mediaId; }
  m.querySelector('#cCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#cSave').addEventListener('click', function () {
    const mediaId = sel.value;
    if (!mediaId) { toast('请选择媒体', 'err'); return; }
    const body = { mediaId: mediaId, note: m.querySelector('#cNote').value.trim(), status: editing ? t.status : 'pending' };
    const req = editing
      ? post('/cache-task/' + encodeURIComponent(t.id) + '/update', Object.assign({ id: t.id }, body))
      : post('/cache-task/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
