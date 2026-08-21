/* views/streams.js — 推流任务：播出操作台。
 * 播放控制条（目标/暂停/继续/跳过/停止）、应急插播、快速推流、任务管理。 */

import { get, post, del, listOf, objOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.allSettled([
    get('/stream/list'),
    get('/playlist/list'),
    get('/media/list'),
    get('/engine/ffmpeg'),
    get('/engine/status')
  ]).then(function (res) {
    setConn(res.some(function (r) { return r.status === 'fulfilled'; }));
    const streams = listOf(res[0].status === 'fulfilled' ? res[0].value : [], ['streams', 'items', 'list']);
    const playlists = listOf(res[1].status === 'fulfilled' ? res[1].value : [], ['playlists', 'items', 'list']);
    const media = listOf(res[2].status === 'fulfilled' ? res[2].value : [], ['media', 'items', 'list']);
    const gcfg = objOf(res[3].status === 'fulfilled' ? res[3].value : {}, ['config', 'data']) || {};
    const eng = objOf(res[4].status === 'fulfilled' ? res[4].value : {}, ['status']);

    root.innerHTML = '';
    root.appendChild(pageHead('推流任务', '一个任务 = 一个内容 + 多条输出线路；多个任务可并行推送不同内容。'));

    /* ---- 播放控制条 ---- */
    root.appendChild(playerCard(playlists, media, eng));

    /* ---- 应急插播（折叠） ---- */
    root.appendChild(interruptCard(playlists, media));

    /* ---- 快速推流 ---- */
    root.appendChild(quickCard(playlists, gcfg));

    /* ---- 引擎设置（全局 ffmpeg 配置） ---- */
    root.appendChild(engineCard(gcfg));

    /* ---- 任务列表 ---- */
    const row = el('div', { class: 'row' },
      '<h2>任务列表（' + streams.length + '）</h2>');
    const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建推流任务' });
    newBtn.addEventListener('click', function () { openStreamEdit(null, playlists, gcfg); });
    row.appendChild(newBtn);
    root.appendChild(row);

    if (!streams.length) {
      const act = el('button', { class: 'btn btn-primary', type: 'button', text: '新建第一个推流任务' });
      act.addEventListener('click', function () { openStreamEdit(null, playlists, gcfg); });
      const card = el('div', { class: 'card' });
      card.appendChild(emptyView('还没有推流任务：选择内容、添加输出平台，即可同时向多个平台推送。', act));
      root.appendChild(card);
    } else {
      root.appendChild(taskTable(streams, playlists, gcfg));
    }
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

/* ---------- 播放控制条 ---------- */
function playerCard(playlists, media, eng) {
  const live = !!(eng && (eng.running || eng.paused));
  const card = el('div', { class: 'card section' }, '<h2>播放控制</h2>');

  if (live) {
    card.appendChild(el('div', { class: 'now-playing' + (eng.paused ? ' paused' : ' live') },
      '<div class="now-head"><span style="min-width:0;word-break:break-all"><span class="muted">正在播放：</span>' +
      esc(eng.sourcePath || '-') +
      (eng.paused ? ' <span class="badge badge-warn">已暂停</span>' : ' <span class="badge badge-ok"><span class="dot ok"></span>推流中</span>') +
      '</span></div>'));
  }

  const grid = el('div', { class: 'form-grid two mt' });
  const targetField = el('div', { class: 'field' },
    '<label>播放目标（节目单或媒体）</label><select class="mono js-target"></select>');
  const tSel = targetField.querySelector('select');
  tSel.appendChild(el('option', { value: '', text: '-- 选择目标 --' }));
  if (playlists.length) {
    const g = el('optgroup', { label: '节目单' });
    playlists.forEach(function (p) { g.appendChild(el('option', { value: p.id, text: p.name, dataset: { kind: 'playlist' } })); });
    tSel.appendChild(g);
  }
  if (media.length) {
    const g = el('optgroup', { label: '媒体' });
    media.forEach(function (m) { g.appendChild(el('option', { value: m.id, text: m.name || m.path, dataset: { kind: 'media' } })); });
    tSel.appendChild(g);
  }
  grid.appendChild(targetField);
  grid.appendChild(el('div', { class: 'field' },
    '<label>起始位置（秒，可选）</label><input type="number" min="0" class="js-seek" placeholder="0 = 从头播放">'));
  card.appendChild(grid);

  const controls = el('div', { class: 'cluster mt' });
  const play = el('button', { class: 'btn btn-primary', type: 'button', text: '播放' });
  play.addEventListener('click', function () {
    const opt = tSel.selectedOptions && tSel.selectedOptions[0];
    if (!opt || !opt.value) { toast('请选择节目单或媒体目标', 'err'); return; }
    const body = opt.dataset.kind === 'playlist' ? { playlistId: opt.value } : { mediaId: opt.value };
    const seekV = parseFloat(card.querySelector('.js-seek').value);
    if (seekV > 0) { body.seekSeconds = seekV; }
    play.disabled = true; play.textContent = '正在启动 ...';
    post('/player/play', body).then(function () { toast('播放已启动', 'ok'); setTimeout(render, 1200); })
      .catch(function (e) { toast(e.message, 'err'); })
      .finally(function () { play.disabled = false; play.textContent = '播放'; });
  });
  controls.appendChild(play);
  [['pause', '暂停'], ['continue', '继续'], ['skip', '跳过']].forEach(function (p) {
    const b = el('button', { class: 'btn', type: 'button', text: p[1] });
    b.addEventListener('click', function () {
      post('/player/' + p[0], {}).then(function () { toast('已' + p[1], 'ok'); setTimeout(render, 900); })
        .catch(function (e) { toast(e.message, 'err'); });
    });
    controls.appendChild(b);
  });
  const stop = el('button', { class: 'btn btn-danger', type: 'button', text: '停止' });
  stop.addEventListener('click', function () {
    post('/player/stop', {}).then(function () { toast('已停止', 'ok'); setTimeout(render, 900); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
  controls.appendChild(stop);
  card.appendChild(controls);
  card.appendChild(el('p', { class: 'muted', style: 'margin:8px 0 0;font-size:12px' },
    '暂停会挂起推流并记住位置；跳过切到下一个内容。'));
  return card;
}

/* ---------- 应急插播 ---------- */
function interruptCard(playlists, media) {
  const details = el('details', { class: 'collapse section' },
    '<summary>应急插播 —— 立即中断当前推流、播放指定内容，可定时自动恢复</summary>');
  const body = el('div', { class: 'collapse-body' });
  const grid = el('div', { class: 'form-grid two mt' });
  const selField = el('div', { class: 'field' }, '<label>插播目标</label><select class="mono js-int-target"></select>');
  const sel = selField.querySelector('select');
  sel.appendChild(el('option', { value: '', text: '-- 选择目标 --' }));
  if (playlists.length) {
    const g = el('optgroup', { label: '节目单' });
    playlists.forEach(function (p) { g.appendChild(el('option', { value: p.id, text: p.name, dataset: { kind: 'playlist' } })); });
    sel.appendChild(g);
  }
  if (media.length) {
    const g = el('optgroup', { label: '媒体' });
    media.forEach(function (m) { g.appendChild(el('option', { value: m.id, text: m.name || m.path, dataset: { kind: 'media' } })); });
    sel.appendChild(g);
  }
  grid.appendChild(selField);
  grid.appendChild(el('div', { class: 'field' },
    '<label>时长（秒，留空为一直插播到手动停止）</label><input type="number" min="1" class="js-int-dur" placeholder="例如 60">'));
  body.appendChild(grid);
  const go = el('button', { class: 'btn btn-primary mt', type: 'button', text: '立即插播' });
  go.addEventListener('click', function () {
    const opt = sel.selectedOptions && sel.selectedOptions[0];
    if (!opt || !opt.value) { toast('请选择插播目标', 'err'); return; }
    const body2 = opt.dataset.kind === 'playlist' ? { playlistId: opt.value } : { mediaId: opt.value };
    const d = Number(body.querySelector('.js-int-dur').value);
    if (d > 0) { body2.duration = d; }
    go.disabled = true; go.textContent = '正在插播 ...';
    post('/player/interrupt', body2).then(function () { toast('插播已启动', 'ok'); setTimeout(render, 1200); })
      .catch(function (e) { toast(e.message, 'err'); })
      .finally(function () { go.disabled = false; go.textContent = '立即插播'; });
  });
  body.appendChild(go);
  details.appendChild(body);
  return details;
}

/* ---------- 快速推流 ---------- */
function quickCard(playlists, gcfg) {
  const card = el('div', { class: 'card section' }, '<h2>快速推流</h2>',
    '<p class="muted" style="margin:-6px 0 10px;font-size:12px">选内容 + 填地址，一步开始（1280x720 / 2500kbps / 25fps）。多线路请用「新建推流任务」。</p>');
  const grid = el('div', { class: 'form-grid three' });
  const plField = el('div', { class: 'field' }, '<label>内容（节目单）</label><select class="mono js-q-pl"></select>');
  const sel = plField.querySelector('select');
  sel.appendChild(el('option', { value: '', text: '-- 选择节目单 --' }));
  playlists.forEach(function (p) { sel.appendChild(el('option', { value: p.id, text: p.name })); });
  grid.appendChild(plField);
  grid.appendChild(el('div', { class: 'field' },
    '<label>推流地址（RTMP）</label><input type="text" class="mono js-q-url" placeholder="rtmp://目标平台/live/流名">'));
  const goField = el('div', { class: 'field' }, '<label>&nbsp;</label>');
  const go = el('button', { class: 'btn btn-primary', type: 'button', text: '开始推流', style: 'width:100%' });
  goField.appendChild(go);
  grid.appendChild(goField);
  card.appendChild(grid);

  go.addEventListener('click', function () {
    const pid = sel.value;
    const url = card.querySelector('.js-q-url').value.trim();
    if (!pid) { toast('请先选择节目单', 'err'); return; }
    if (!url || url.indexOf('rtmp://') !== 0) { toast('请填写有效的推流地址（rtmp://...）', 'err'); return; }
    go.disabled = true; go.textContent = '检查配置 ...';
    // P0 保护：快速推流会整体替换全局引擎输出。已有配置时先确认，
    // 避免隐式覆盖用户手工维护的其他线路。
    get('/engine/ffmpeg').then(function (cur) {
      const existing = (objOf(cur, ['config', 'data']).outputs) || [];
      if (existing.length > 0) {
        const list = existing.map(function (o) { return '  · ' + (o.url || '(空)'); }).join('\n');
        if (!window.confirm('快速推流将替换全局引擎的全部输出线路（现有 ' + existing.length + ' 条）：\n' + list +
          '\n\n继续？\n（取消后可改用「新建推流任务」，任务线路互不影响）')) {
          go.disabled = false; go.textContent = '开始推流';
          return null;
        }
      }
      go.textContent = '推流启动中 ...';
      return post('/engine/ffmpeg', {
        ffmpegPath: gcfg.ffmpegPath || undefined,
        outputs: [{ url: url, width: 1280, height: 720, bitrateKbps: 2500, fps: 25, codec: 'libx264' }]
      }).then(function () { return post('/player/play', { playlistId: pid }); })
        .then(function () { toast('已开始推流', 'ok'); render(); });
    }).catch(function (e) { toast(e.message, 'err'); })
      .finally(function () { go.disabled = false; go.textContent = '开始推流'; });
  });
  return card;
}

/* ---------- 引擎设置（全局 ffmpeg 配置） ---------- */
function engineCard(gcfg) {
  const details = el('details', { class: 'collapse section' },
    '<summary>引擎设置 —— ffmpeg 路径 / 全局输出线路 / 硬件加速（影响直接播放与快速推流）</summary>');
  const body = el('div', { class: 'collapse-body' });

  body.appendChild(el('div', { class: 'form-grid two mt' },
    '<div class="field"><label>ffmpeg 路径（留空 = 自动检测 PATH）</label>' +
    '<input type="text" class="js-eng-ffmpeg mono" spellcheck="false" placeholder="ffmpeg" value="' + esc(gcfg.ffmpegPath || '') + '"></div>' +
    '<div class="field"><label>&nbsp;</label><span class="muted" style="font-size:12px">修改后点「保存配置」；正在推流时需再点「应用到运行中」才生效。</span></div>'));

  const outHost = el('div', { class: 'js-eng-outputs' });
  function engOutRow(o) {
    const row = el('div', { class: 'kp-engine-output' });
    const head = el('div', { class: 'row', style: 'margin-bottom:8px' },
      '<span class="muted">输出线路 ' + (outHost.children.length + 1) + '</span>' +
      '<button type="button" class="btn btn-danger btn-sm js-rm">移除</button>');
    head.querySelector('.js-rm').addEventListener('click', function () { row.remove(); });
    row.appendChild(head);
    row.appendChild(el('div', { class: 'field' },
      '<label>推流地址 (RTMP)</label><input type="text" class="so-url mono" placeholder="rtmp://平台/live/流名" value="' + esc(o.url || '') + '">'));
    row.appendChild(el('div', { class: 'form-grid three mt' },
      '<div class="field"><label>宽度</label><input type="number" class="so-w" value="' + (o.width || 1280) + '"></div>' +
      '<div class="field"><label>高度</label><input type="number" class="so-h" value="' + (o.height || 720) + '"></div>' +
      '<div class="field"><label>码率 (kbps)</label><input type="number" class="so-b" value="' + (o.bitrateKbps || 2500) + '"></div>' +
      '<div class="field"><label>帧率</label><input type="number" class="so-f" value="' + (o.fps || 25) + '"></div>' +
      '<div class="field"><label>硬件加速 (-hwaccel)</label><select class="so-hw">' +
      ['', 'auto', 'vaapi', 'nvenc', 'qsv', 'd3d11va', 'videotoolbox'].map(function (v) {
        return '<option value="' + v + '"' + (o.hwAccel === v ? ' selected' : '') + '>' + (v === '' ? '不用' : v) + '</option>';
      }).join('') + '</select></div>' +
      '<div class="field"><label>声道数</label><input type="number" class="so-ac" min="1" value="' + (o.audioChannels || '') + '" placeholder="默认"></div>' +
      '</div>'));
    row.appendChild(el('div', { class: 'form-grid two mt' },
      '<div class="field"><label>视频滤镜 (-vf，高级)</label><input type="text" class="so-flt mono" placeholder="留空=无" value="' + esc(o.filters || '') + '"></div>' +
      '<div class="field"><label>音频滤镜 (-af，高级)</label><input type="text" class="so-af mono" placeholder="留空=无" value="' + esc(o.audioFilters || '') + '"></div>'));
    return row;
  }
  const outputs = Array.isArray(gcfg.outputs) ? gcfg.outputs : [];
  outputs.forEach(function (o) { outHost.appendChild(engOutRow(o)); });
  if (!outputs.length) {
    outHost.appendChild(el('p', { class: 'muted', style: 'font-size:12px;margin:4px 0' },
      '还没有全局输出线路 —— 用「快速推流」或下面「+ 添加线路」配置。'));
  }
  body.appendChild(el('div', { class: 'field' },
    '<label>全局输出线路</label>'));
  body.appendChild(outHost);
  const addBtn = el('button', { class: 'btn', type: 'button', text: '+ 添加线路' });
  addBtn.addEventListener('click', function () { outHost.appendChild(engOutRow({ width: 1280, height: 720, bitrateKbps: 2500, fps: 25 })); });
  body.appendChild(addBtn);

  const actions = el('div', { class: 'form-actions' });
  const save = el('button', { class: 'btn btn-primary', type: 'button', text: '保存配置' });
  save.addEventListener('click', function () {
    const outs = Array.from(outHost.querySelectorAll('.kp-engine-output')).map(function (row) {
      const o = { url: row.querySelector('.so-url').value.trim() };
      const num = function (cls) { const v = parseInt(row.querySelector(cls).value, 10); return isNaN(v) ? undefined : v; };
      o.width = num('.so-w'); o.height = num('.so-h');
      o.bitrateKbps = num('.so-b'); o.fps = num('.so-f');
      o.audioChannels = num('.so-ac');
      o.codec = 'libx264';
      const hw = row.querySelector('.so-hw').value;
      if (hw) { o.hwAccel = hw; }
      const flt = row.querySelector('.so-flt');
      if (flt && flt.value.trim()) { o.filters = flt.value.trim(); }
      const af = row.querySelector('.so-af');
      if (af && af.value.trim()) { o.audioFilters = af.value.trim(); }
      return o;
    }).filter(function (o) { return o.url; });
    save.disabled = true; save.textContent = '保存中 ...';
    post('/engine/ffmpeg', {
      ffmpegPath: body.querySelector('.js-eng-ffmpeg').value.trim() || undefined,
      outputs: outs
    }).then(function () { toast('引擎配置已保存', 'ok'); render(); })
      .catch(function (e) { toast(e.message, 'err'); })
      .finally(function () { save.disabled = false; save.textContent = '保存配置'; });
  });
  actions.appendChild(save);

  const apply = el('button', { class: 'btn', type: 'button', text: '应用到运行中推流' });
  apply.addEventListener('click', function () {
    if (!window.confirm('把已保存的引擎配置应用到正在运行的推流？（运行中的流会以新配置重启）')) { return; }
    post('/engine/ffmpeg/apply', {}).then(function () { toast('已应用', 'ok'); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
  actions.appendChild(apply);
  body.appendChild(actions);

  details.appendChild(body);
  return details;
}

/* ---------- 任务表格 ---------- */
function taskTable(streams, playlists, gcfg) {
  const plById = {};
  playlists.forEach(function (p) { plById[p.id] = p; });
  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>名称</th><th>内容</th><th>输出线路</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');
  streams.forEach(function (s) {
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<b>' + esc(s.name || s.id) + '</b>'));
    tr.appendChild(el('td', { text: s.playlistId ? ((plById[s.playlistId] && plById[s.playlistId].name) || '节目单') : '-' }));

    const outTd = el('td', { class: 'cell-path' });
    (s.outputs || []).forEach(function (o, i) {
      if (i > 0) { outTd.appendChild(el('div', { class: 'dim', text: '·' })); }
      outTd.appendChild(el('div', { class: 'path', text: o.url || '-' }));
    });
    if (!(s.outputs || []).length) { outTd.textContent = '-'; }
    tr.appendChild(outTd);

    tr.appendChild(el('td', {}, s.running
      ? '<span class="badge badge-ok"><span class="dot ok"></span>运行中</span>'
      : '<span class="badge badge-neutral">已停止</span>'));

    const ops = el('td', { class: 'actions' });
    const opsRow = el('div', { class: 'icon-btn-row' });
    if (s.running) {
      const btnStop = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '停止' });
      btnStop.addEventListener('click', function () {
        post('/stream/' + s.id + '/stop', {}).then(function () { toast('已停止: ' + s.name, 'ok'); render(); })
          .catch(function (e) { toast(e.message, 'err'); });
      });
      opsRow.appendChild(btnStop);
    } else {
      const btnStart = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '启动' });
      btnStart.addEventListener('click', function () {
        btnStart.disabled = true; btnStart.textContent = '启动中 ...';
        post('/stream/' + s.id + '/start', {}).then(function () { toast('已启动: ' + s.name, 'ok'); setTimeout(render, 1500); })
          .catch(function (e) { toast(e.message, 'err'); })
          .finally(function () { btnStart.disabled = false; btnStart.textContent = '启动'; });
      });
      opsRow.appendChild(btnStart);
    }
    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openStreamEdit(s, playlists, gcfg); });
    opsRow.appendChild(btnEdit);
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('删除推流任务 "' + s.name + '"？')) { return; }
      del('/stream/' + s.id).then(function () { toast('任务已删除', 'ok'); render(); })
        .catch(function (e) { toast(e.message, 'err'); });
    });
    opsRow.appendChild(btnDel);
    ops.appendChild(opsRow);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });
  return wrap;
}

/* ---------- 新建/编辑任务模态 ---------- */
export function openStreamEdit(s, playlists, gcfg) {
  const editing = !!s;
  const outputs = editing && Array.isArray(s.outputs) ? s.outputs.map(function (o) { return Object.assign({}, o); }) : [];
  const m = el('div', { class: 'modal wide' },
    '<h3>' + (editing ? '编辑推流任务' : '新建推流任务') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>任务名称</label><input id="stName" type="text" value="' + esc(editing ? s.name : '') + '"></div>' +
    '<div class="field"><label>断线自动重推间隔（秒，0 = 关闭）</label><input id="stReconnect" type="number" min="0" value="' + (editing ? (s.reconnectInterval || 5) : 5) + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>内容（节目单：文件或目录按顺序连续播放）</label><select id="stPlaylist" class="mono"></select></div>' +
    '<div class="field mt"><label>输出线路（一个任务可同时推多条线路）</label><div id="stOutputs"></div>' +
    '<button id="stAddOut" class="btn" type="button">+ 添加线路</button></div>' +
    '<p class="muted" style="font-size:12px;margin:6px 0 0">线路预填常用参数（1280x720 / 2500kbps / 25fps / H.264），只需填写推流地址。</p>' +
    '<div class="form-actions"><button id="stCancel" class="btn" type="button">取消</button>' +
    '<button id="stSave" class="btn btn-primary" type="button">保存</button></div>');

  const plSel = m.querySelector('#stPlaylist');
  plSel.appendChild(el('option', { value: '', text: '-- 选择节目单 --' }));
  playlists.forEach(function (p) { plSel.appendChild(el('option', { value: p.id, text: p.name })); });
  if (editing && s.playlistId) { plSel.value = s.playlistId; }

  const outHost = m.querySelector('#stOutputs');
  function outRow(o) {
    const row = el('div', { class: 'kp-engine-output' });
    const head = el('div', { class: 'row', style: 'margin-bottom:8px' },
      '<span class="muted">输出线路 ' + (outHost.children.length + 1) + '</span>' +
      '<button type="button" class="btn btn-danger btn-sm so-rm">移除</button>');
    head.querySelector('.so-rm').addEventListener('click', function () { row.remove(); });
    row.appendChild(head);
    row.appendChild(el('div', { class: 'field' },
      '<label>推流地址 (RTMP)</label><input type="text" class="so-url mono" placeholder="rtmp://平台/live/流名" value="' + esc(o.url || '') + '">'));
    row.appendChild(el('div', { class: 'form-grid three mt' },
      '<div class="field"><label>宽度</label><input type="number" class="so-w" value="' + (o.width || 1280) + '"></div>' +
      '<div class="field"><label>高度</label><input type="number" class="so-h" value="' + (o.height || 720) + '"></div>' +
      '<div class="field"><label>码率 (kbps)</label><input type="number" class="so-b" value="' + (o.bitrateKbps || 2500) + '"></div>' +
      '<div class="field"><label>帧率</label><input type="number" class="so-f" value="' + (o.fps || 25) + '"></div>' +
      '<div class="field"><label>声道数</label><input type="number" class="so-ac" min="1" value="' + (o.audioChannels || '') + '" placeholder="默认"></div>' +
      '<div class="field"><label>采样率</label><input type="number" class="so-ar" min="1" value="' + (o.audioSampleRate || '') + '" placeholder="默认"></div>'));
    row.appendChild(el('div', { class: 'field mt' },
      '<label>滤镜（高级，ffmpeg -vf 语法；如 drawtext 文字水印、subtitles 字幕烧录）</label>' +
      '<input type="text" class="so-flt mono" placeholder="留空=无" value="' + esc(o.filters || '') + '">'));
    return row;
  }
  const defaultLine = { url: '', width: 1280, height: 720, bitrateKbps: 2500, fps: 25, codec: 'libx264' };
  outputs.forEach(function (o) { outHost.appendChild(outRow(o)); });
  if (!outputs.length) { outHost.appendChild(outRow(defaultLine)); }
  m.querySelector('#stAddOut').addEventListener('click', function () { outHost.appendChild(outRow({})); });

  m.querySelector('#stCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#stSave').addEventListener('click', function () {
    const name = m.querySelector('#stName').value.trim();
    const playlistId = plSel.value;
    const outs = Array.from(outHost.querySelectorAll('.kp-engine-output')).map(function (row) {
      const out = { url: row.querySelector('.so-url').value.trim() };
      const w = parseInt(row.querySelector('.so-w').value, 10);
      const h = parseInt(row.querySelector('.so-h').value, 10);
      const b = parseInt(row.querySelector('.so-b').value, 10);
      const f = parseInt(row.querySelector('.so-f').value, 10);
      const ac = parseInt(row.querySelector('.so-ac').value, 10);
      const ar = parseInt(row.querySelector('.so-ar').value, 10);
      if (!isNaN(w)) { out.width = w; }
      if (!isNaN(h)) { out.height = h; }
      if (!isNaN(b)) { out.bitrateKbps = b; }
      if (!isNaN(f)) { out.fps = f; }
      if (!isNaN(ac) && ac > 0) { out.audioChannels = ac; }
      if (!isNaN(ar) && ar > 0) { out.audioSampleRate = ar; }
      out.codec = 'libx264';
      const flt = row.querySelector('.so-flt');
      if (flt && flt.value.trim()) { out.filters = flt.value.trim(); }
      return out;
    }).filter(function (o) { return o.url; });
    if (!name) { toast('请输入任务名称', 'err'); return; }
    if (!playlistId) { toast('请选择节目单', 'err'); return; }
    if (!outs.length) { toast('请至少添加一个输出地址', 'err'); return; }
    const body = { name: name, playlistId: playlistId, outputs: outs };
    const ri = parseInt(m.querySelector('#stReconnect').value, 10);
    if (!isNaN(ri) && ri >= 0) { body.reconnectInterval = ri; }
    const p = editing
      ? post('/stream/' + s.id + '/replace', Object.assign({ id: s.id }, body))
      : post('/stream/add', body);
    p.then(function () {
      toast(editing ? '任务已更新' : '任务已创建', 'ok');
      m.remove(); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  });
  modal(m, true);
}
