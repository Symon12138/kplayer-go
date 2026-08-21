/* views/playlists.js — 节目单：播出计划编排。
 * 条目可以是文件或整个目录；四种播放方式；后备节目单。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

const MODES = {
  order: '顺序一遍', loop: '顺序循环',
  random: '随机一遍', 'random-loop': '随机循环'
};

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([get('/playlist/list'), get('/media/list')]).then(function (res) {
    setConn(true);
    const playlists = listOf(res[0], ['playlists', 'items', 'list']);
    const media = listOf(res[1], ['media', 'items', 'list']);
    draw(root, playlists, media);
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, playlists, media) {
  root.innerHTML = '';
  const plById = {};
  playlists.forEach(function (p) { plById[p.id] = p; });

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建节目单' });
  newBtn.addEventListener('click', function () { openEdit(null, media, playlists); });
  root.appendChild(pageHead('节目单', '把媒体编排成播出计划：条目顺序、播放方式、异常时的后备节目单。', [newBtn]));

  if (!playlists.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '新建第一个节目单' });
    act.addEventListener('click', function () { openEdit(null, media, playlists); });
    const card = el('div', { class: 'card' });
    card.appendChild(emptyView('暂无节目单。节目单是推流任务的内容来源。', act));
    root.appendChild(card);
    return;
  }

  const stack = el('div', { class: 'list-stack' });
  playlists.forEach(function (p) {
    const mode = MODES[p.mode || (p.loop ? 'loop' : 'order')] || '顺序一遍';
    const item = el('div', { class: 'list-item' },
      '<div class="list-item-head"><span class="list-item-title">' + esc(p.name) + '</span>' +
      '<div class="icon-btn-row"></div></div>' +
      '<div class="list-item-meta">' +
      '<span>' + ((p.items && p.items.length) || 0) + ' 条</span>' +
      '<span class="badge badge-info">' + esc(mode) + '</span>' +
      (p.fallbackPlaylistId
        ? '<span class="badge badge-warn">后备: ' + esc((plById[p.fallbackPlaylistId] && plById[p.fallbackPlaylistId].name) || p.fallbackPlaylistId) + '</span>'
        : '') +
      '<span class="dim">' + esc(fmtAgo(p.updatedAt || p.updated_at)) + '</span>' +
      '</div>');

    const ops = item.querySelector('.icon-btn-row');
    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openEdit(p, media, playlists); });
    ops.appendChild(btnEdit);
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('确定删除节目单 "' + p.name + '" 吗？')) { return; }
      del('/playlist/remove/' + encodeURIComponent(p.id)).then(function () {
        toast('节目单已删除', 'ok'); render();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    ops.appendChild(btnDel);
    stack.appendChild(item);
  });
  root.appendChild(stack);
}

/* ---------- 新建 / 编辑节目单 ---------- */
function openEdit(p, media, playlists) {
  const editing = !!p;
  const mediaById = {};
  media.forEach(function (m) { mediaById[m.id] = m; });
  const items = editing ? (p.items || []).map(function (it) { return it.mediaId || it.media_id; }) : [];
  const extra = {};

  const m = el('div', { class: 'modal wide' },
    '<h3>' + (editing ? '编辑节目单' : '新建节目单') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="plName" type="text" value="' + esc(editing ? p.name : '') + '"></div>' +
    '<div class="field"><label>播放方式</label><select id="plMode">' +
    '<option value="order">顺序播放一遍</option>' +
    '<option value="loop">顺序循环播放</option>' +
    '<option value="random">随机播放一遍</option>' +
    '<option value="random-loop">随机循环播放</option>' +
    '</select></div>' +
    '</div>' +
    '<div class="form-grid two mt">' +
    '<div class="field"><label>后备节目单（主节目单异常时自动切换）</label><select id="plFallback"></select></div>' +
    '<div class="field"><label>描述（可选）</label><input id="plDesc" type="text" value="' + esc(editing ? (p.desc || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>添加条目：从媒体库选择，或直接输入路径（自动登记）</label>' +
    '<div class="cluster" style="gap:8px">' +
    '<select id="plPick" class="mono" style="flex:1;min-height:36px;background:var(--bg-0);color:var(--txt-0);border:1px solid var(--line-2);border-radius:8px;padding:6px 11px"></select>' +
    '<input id="plItemPath" type="text" class="mono" placeholder="或输路径: /data/video/xx.mp4" spellcheck="false" style="flex:1.2">' +
    '<button id="plAddItem" class="btn" type="button">+ 添加</button>' +
    '</div></div>' +
    '<div class="table-wrap mt"><table class="data"><thead><tr><th style="width:34px">#</th><th>条目</th><th>路径</th><th class="actions" style="text-align:right;width:120px">操作</th></tr></thead><tbody id="plItems"></tbody></table></div>' +
    '<div class="form-actions"><button id="plCancel" class="btn" type="button">取消</button>' +
    '<button id="plSave" class="btn btn-primary" type="button">保存</button></div>');

  /* 后备节目单 */
  const fbSel = m.querySelector('#plFallback');
  const fallbackId = editing ? (p.fallbackPlaylistId || p.fallback_playlist_id || '') : '';
  let foundFb = false;
  fbSel.appendChild(el('option', { value: '', text: '-- 无 --' }));
  playlists.forEach(function (pl) {
    if (editing && pl.id === p.id) { return; }
    if (pl.id === fallbackId) { foundFb = true; }
    fbSel.appendChild(el('option', { value: pl.id, text: pl.name }));
  });
  if (fallbackId && !foundFb) {
    fbSel.appendChild(el('option', { value: fallbackId, text: '（缺失节目单: ' + fallbackId + '）' }));
  }
  fbSel.value = fallbackId;

  if (editing) { m.querySelector('#plMode').value = p.mode || (p.loop ? 'loop' : 'order'); }

  /* 媒体选择器 */
  const pick = m.querySelector('#plPick');
  pick.appendChild(el('option', { value: '', text: '-- 从媒体库选择 --' }));
  media.forEach(function (mm) {
    pick.appendChild(el('option', { value: mm.id, text: (mm.isDir ? '[目录] ' : '') + (mm.name || mm.path) }));
  });

  /* 条目表 */
  const tbody = m.querySelector('#plItems');
  function drawItems() {
    tbody.innerHTML = '';
    items.forEach(function (id, idx) {
      const info = mediaById[id] || extra[id] || {};
      const tr = el('tr', {});
      tr.appendChild(el('td', { text: String(idx + 1) }));
      tr.appendChild(el('td', { text: (info.isDir ? '[目录] ' : '') + (info.name || id) + (info.audioPath || info.subtitlePath ? ' (含音/字)' : '') }));
      tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(info.path || '-') + '</div>'));
      const ops = el('td', { class: 'actions' });
      const row = el('div', { class: 'icon-btn-row' });
      const up = el('button', { class: 'btn btn-sm', type: 'button', text: '↑' });
      const down = el('button', { class: 'btn btn-sm', type: 'button', text: '↓' });
      const del = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '✕' });
      up.addEventListener('click', function () { move(items, idx, idx - 1); drawItems(); });
      down.addEventListener('click', function () { move(items, idx, idx + 1); drawItems(); });
      del.addEventListener('click', function () { items.splice(idx, 1); drawItems(); });
      row.appendChild(up); row.appendChild(down); row.appendChild(del);
      ops.appendChild(row);
      tr.appendChild(ops);
      tbody.appendChild(tr);
    });
    if (!items.length) {
      tbody.appendChild(el('tr', {}, '<td colspan="4"><div class="empty" style="padding:14px"><p>还没有条目 —— 从媒体库选择或直接输入路径添加。</p></div></td>'));
    }
  }
  function move(arr, from, to) {
    if (from < 0 || from >= arr.length || to < 0 || to >= arr.length) { return; }
    const v = arr.splice(from, 1)[0];
    arr.splice(to, 0, v);
  }
  drawItems();

  /* 添加条目：优先媒体选择器，其次路径直输（自动登记） */
  m.querySelector('#plAddItem').addEventListener('click', function () {
    const picked = pick.value;
    const pathInput = m.querySelector('#plItemPath');
    const path = pathInput.value.trim();
    const btn = m.querySelector('#plAddItem');

    if (picked) {
      if (items.indexOf(picked) === -1) { items.push(picked); drawItems(); }
      pick.value = '';
      return;
    }
    if (!path) { toast('请从媒体库选择或输入路径', 'err'); return; }
    btn.disabled = true; btn.textContent = '登记中 ...';
    post('/media/add', { path: path }).then(function (item) {
      const id = item && (item.id || item.mediaId);
      if (!id) { throw new Error('登记失败'); }
      extra[id] = item;
      if (items.indexOf(id) === -1) { items.push(id); drawItems(); }
      pathInput.value = '';
      toast('已添加: ' + (item.name || path), 'ok');
    }).catch(function (e) { toast(e.message, 'err'); })
      .finally(function () { btn.disabled = false; btn.textContent = '+ 添加'; });
  });

  m.querySelector('#plCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#plSave').addEventListener('click', function () {
    const name = m.querySelector('#plName').value.trim();
    if (!name) { toast('必须填写节目单名称', 'err'); return; }
    const body = {
      name: name,
      desc: m.querySelector('#plDesc').value.trim(),
      items: items,
      mode: m.querySelector('#plMode').value,
      fallbackPlaylistId: fbSel.value || undefined
    };
    const req = editing
      ? post('/playlist/update', Object.assign({ id: p.id }, body))
      : post('/playlist/add', body);
    req.then(function () {
      toast(editing ? '节目单已更新' : '节目单已创建', 'ok');
      m.remove(); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  });

  modal(m, true);
}
