/* views/media.js — 媒体库：推流内容源的登记、扫描与管理。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtBytes, fmtDur, fmtAgo } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/media/list').then(function (res) {
    setConn(true);
    const media = listOf(res, ['media', 'items', 'list']);
    draw(root, media);
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, media) {
  root.innerHTML = '';

  const addBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 登记媒体' });
  addBtn.addEventListener('click', function () { openRegister(null); });
  const scanBtn = el('button', { class: 'btn', type: 'button', text: '扫描目录' });
  scanBtn.addEventListener('click', openScan);
  root.appendChild(pageHead('媒体库', '推流的内容源：视频文件或整个目录（目录按排序逐集连续播放）。', [scanBtn, addBtn]));

  /* 搜索 */
  let filtered = media;
  const search = el('input', { type: 'text', placeholder: '按名称 / 路径搜索 ...', style: 'max-width:340px;margin-bottom:14px' });
  search.addEventListener('input', function () {
    filtered = doSearch(media, search.value.trim());
    redrawTable(root, filtered, media);
  });
  root.appendChild(search);
  root.appendChild(el('div', { id: 'mediaTableHost' }));
  redrawTable(root, filtered, media);
}

function doSearch(media, q) {
  if (!q) { return media; }
  const lq = q.toLowerCase();
  return media.filter(function (m) {
    return String(m.name || '').toLowerCase().indexOf(lq) >= 0 ||
      String(m.path || '').toLowerCase().indexOf(lq) >= 0;
  });
}

function redrawTable(root, rows, all) {
  const host = root.querySelector('#mediaTableHost');
  if (!host) { return; }
  host.innerHTML = '';

  if (!all.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '登记第一个媒体' });
    act.addEventListener('click', function () { openRegister(null); });
    const card = el('div', { class: 'card' });
    card.appendChild(emptyView('媒体库为空：登记一个视频文件或整个目录，作为推流的内容源。', act));
    host.appendChild(card);
    return;
  }
  if (!rows.length) {
    const card = el('div', { class: 'card' });
    card.appendChild(emptyView('没有匹配的媒体。', undefined));
    host.appendChild(card);
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>名称</th><th>路径</th><th>时长</th><th>分辨率</th><th>大小</th><th>更新</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');
  rows.forEach(function (m) {
    const tr = el('tr', {});
    const nameTd = el('td', {});
    nameTd.appendChild(el('b', { text: (m.isDir ? '[目录] ' : '') + (m.name || m.id) }));
    if (m.audioPath || m.subtitlePath) {
      const tags = el('div', { style: 'margin-top:2px' });
      if (m.audioPath) { tags.appendChild(el('span', { class: 'badge badge-info', text: '外挂音频' })); }
      if (m.subtitlePath) { tags.appendChild(el('span', { class: 'badge badge-info', text: '字幕' })); }
      nameTd.appendChild(tags);
    }
    tr.appendChild(nameTd);

    const pathTd = el('td', { class: 'cell-path' });
    pathTd.appendChild(el('div', { class: 'path', text: m.path || '-' }));
    if (m.audioPath) { pathTd.appendChild(el('div', { class: 'path dim', text: '音: ' + m.audioPath })); }
    if (m.subtitlePath) { pathTd.appendChild(el('div', { class: 'path dim', text: '字: ' + m.subtitlePath })); }
    tr.appendChild(pathTd);

    tr.appendChild(el('td', { text: m.duration ? fmtDur(m.duration) : (m.probed ? '-' : '-') }));
    tr.appendChild(el('td', { text: (m.width && m.height) ? m.width + 'x' + m.height : '-' }));
    tr.appendChild(el('td', { text: m.isDir ? '-' : fmtBytes(m.size) }));
    tr.appendChild(el('td', { text: fmtAgo(m.updatedAt) }));

    const ops = el('td', { class: 'actions' });
    const opsRow = el('div', { class: 'icon-btn-row' });
    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openRegister(m); });
    opsRow.appendChild(btnEdit);
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '移除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('移除媒体 "' + (m.name || m.path) + '"？（不影响磁盘文件；引用它的节目单条目将失效）')) { return; }
      del('/media/remove/' + encodeURIComponent(m.id)).then(function () {
        toast('媒体已移除', 'ok'); render();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    opsRow.appendChild(btnDel);
    ops.appendChild(opsRow);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });
  host.appendChild(wrap);
  host.appendChild(el('p', { class: 'muted', style: 'margin:10px 2px 0;font-size:12px' },
    '共 ' + rows.length + ' / ' + all.length + ' 项'));
}

/* ---------- 登记 / 编辑媒体 ---------- */
function openRegister(m) {
  const editing = !!m;
  const f = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑媒体' : '登记媒体') + '</h3>' +
    '<div class="field"><label>视频文件或目录的绝对路径</label>' +
    '<input id="mPath" type="text" class="mono" spellcheck="false" placeholder="例如 /data/video/xxx.mp4 或 /data/video" value="' + esc(editing ? (m.path || '') : '') + '">' +
    '<p class="muted" style="margin:3px 0 0;font-size:12px">目录会按其中的视频文件列表逐集连续播放；支持 mp4/mkv/flv/avi/mov/ts/webm 等。</p></div>' +
    '<div class="form-grid two mt">' +
    '<div class="field"><label>外挂音频（可选，替换原音轨）</label>' +
    '<input id="mAudio" type="text" class="mono" spellcheck="false" placeholder="/data/audio.mp3" value="' + esc(editing ? (m.audioPath || '') : '') + '"></div>' +
    '<div class="field"><label>字幕文件（可选，烧录进画面）</label>' +
    '<input id="mSub" type="text" class="mono" spellcheck="false" placeholder="/data/sub.srt" value="' + esc(editing ? (m.subtitlePath || '') : '') + '"></div>' +
    '</div>' +
    '<p class="muted" style="font-size:12px;margin:10px 0 0">视频 + 外挂音频 + 字幕由 ffmpeg 按时间戳对齐合并成一路推流。</p>' +
    '<div class="form-actions"><button id="mCancel" class="btn" type="button">取消</button>' +
    '<button id="mSave" class="btn btn-primary" type="button">保存</button></div>'));

  f.querySelector('#mCancel').addEventListener('click', function () { f.remove(); });
  f.querySelector('#mSave').addEventListener('click', function () {
    const path = f.querySelector('#mPath').value.trim();
    if (!path) { toast('请填写视频路径', 'err'); return; }
    const body = {
      path: path,
      audioPath: f.querySelector('#mAudio').value.trim() || undefined,
      subtitlePath: f.querySelector('#mSub').value.trim() || undefined
    };
    const btn = f.querySelector('#mSave');
    btn.disabled = true; btn.textContent = '保存中 ...';
    const p = editing
      ? post('/media/' + encodeURIComponent(m.id) + '/update', body)
      : post('/media/add', body);
    p.then(function () {
      toast(editing ? '媒体已更新' : '媒体已登记', 'ok');
      f.remove(); render();
    }).catch(function (e) { toast(e.message, 'err'); })
      .finally(function () { btn.disabled = false; btn.textContent = '保存'; });
  });
}

/* ---------- 扫描目录 ---------- */
function openScan() {
  const f = modal(el('div', { class: 'modal' },
    '<h3>扫描目录登记媒体</h3>' +
    '<div class="field"><label>目录绝对路径</label>' +
    '<input id="scanRoot" type="text" class="mono" spellcheck="false" placeholder="例如 /data/video"></div>' +
    '<div class="cluster mt">' +
    '<label class="check"><input id="scanRecursive" type="checkbox" checked> 包含子目录</label>' +
    '<label class="check"><input id="scanProbe" type="checkbox"> 探测元数据（ffprobe，较慢）</label>' +
    '</div>' +
    '<div class="form-actions"><button id="scanCancel" class="btn" type="button">取消</button>' +
    '<button id="scanGo" class="btn btn-primary" type="button">开始扫描</button></div>' +
    '<div class="scan-result mt"></div>'));

  f.querySelector('#scanCancel').addEventListener('click', function () { f.remove(); });
  f.querySelector('#scanGo').addEventListener('click', function () {
    const rootPath = f.querySelector('#scanRoot').value.trim();
    if (!rootPath) { toast('请填写目录路径', 'err'); return; }
    const btn = f.querySelector('#scanGo');
    btn.disabled = true; btn.textContent = '扫描中 ...';
    post('/media/scan', {
      root: rootPath,
      recursive: f.querySelector('#scanRecursive').checked,
      probe: f.querySelector('#scanProbe').checked
    }).then(function (res) {
      const added = res && (res.added != null ? res.added : (res.count != null ? res.count : ''));
      toast('扫描完成' + (added !== '' ? '：新登记 ' + added + ' 项' : ''), 'ok');
      f.remove(); render();
    }).catch(function (e) {
      toast(e.message, 'err');
      btn.disabled = false; btn.textContent = '开始扫描';
    });
  });
}
