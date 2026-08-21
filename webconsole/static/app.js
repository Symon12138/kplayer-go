/* KPlayer Console - operations dashboard.
 * Pure ASCII source, no external dependencies, no CDN.
 * All backend calls go through the relative /console/api/... prefix, which
 * the webconsole handler proxies to the KPlayer REST backend. The parsing
 * helpers are deliberately defensive so the frontend works with the REST JSON
 * contract as it is finalized by the backend.
 */
(function () {
  'use strict';

  /* ------------------------------------------------------------------ *
   * Constants and shared state
   * ------------------------------------------------------------------ */
  var API = '/console/api';
  var LS_TOKEN = 'kplayer.console.token';
  var REFRESH_MS = 5000;

  // 只保留推流操作所需页面：总览 / 多路推流 / 节目单 / 定时任务。
  // 内容统一从节目单来：节目单条目可以是单个文件或整个目录（目录内按
  // 文件名排序连续播放）；媒体库页已移除（登记在节目单编辑时自动完成）。
  var views = {
    overview: { title: '总览', auto: true },
    streams:  { title: '多路推流', auto: true },
    playlist: { title: '节目单', auto: false },
    tasks:    { title: '定时任务', auto: false },
    effects:  { title: '效果与插件', auto: false }
  };

  var state = {
    current: 'overview',
    mediaCache: [],
    playlistCache: [],
    auditFilter: { operator: '', action: '', result: '' },
    loading: false,
    // Session state: authed gates rendering/polling, currentUser carries
    // the signed-in identity ({username, role}) for role-aware UI.
    authed: false,
    currentUser: null
  };

  /* ------------------------------------------------------------------ *
   * Small DOM helpers
   * ------------------------------------------------------------------ */
  function $(sel) { return document.querySelector(sel); }
  function $$(sel) { return Array.prototype.slice.call(document.querySelectorAll(sel)); }

  function el(tag, attrs, html) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        if (k === 'class') { node.className = attrs[k]; }
        else if (k === 'text') { node.textContent = attrs[k]; }
        else if (k === 'dataset') {
          Object.keys(attrs[k]).forEach(function (dk) { node.dataset[dk] = attrs[k][dk]; });
        } else { node.setAttribute(k, attrs[k]); }
      });
    }
    if (html !== undefined) { node.innerHTML = html; }
    return node;
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function toast(msg, type) {
    var box = $('#toasts');
    var node = el('div', { class: 'toast' + (type === 'err' ? ' toast-err' : type === 'ok' ? ' toast-ok' : '') }, esc(msg));
    box.appendChild(node);
    setTimeout(function () {
      node.style.opacity = '0';
      node.style.transition = 'opacity .25s';
      setTimeout(function () { node.remove(); }, 260);
    }, 3200);
  }

  /* ------------------------------------------------------------------ *
   * API layer
   * ------------------------------------------------------------------ */
  function getToken() {
    try { return localStorage.getItem(LS_TOKEN) || ''; } catch (_) { return ''; }
  }
  function setToken(v) {
    try {
      if (v) { localStorage.setItem(LS_TOKEN, v); } else { localStorage.removeItem(LS_TOKEN); }
    } catch (_) { /* storage unavailable */ }
  }

  function request(method, path, body) {
    var headers = {};
    var token = getToken();
    if (token) {
      // The REST backend authenticates with "Authorization: Bearer <token>".
      // The webconsole proxy passes a client Authorization header through
      // untouched (a server-configured KPLAYER_CONSOLE_TOKEN would override
      // it). This replaces the legacy X-Console-Token header, which the
      // proxy still maps for older clients; strip a "Bearer " prefix that
      // older console versions stored in localStorage so old tokens keep
      // working without re-login.
      var cred = token;
      if (cred.indexOf('Bearer ') === 0) { cred = cred.slice(7); }
      headers['Authorization'] = 'Bearer ' + cred;
    }
    var opts = { method: method, headers: headers };
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(API + path, opts).then(function (res) {
      var ct = (res.headers.get('content-type') || '');
      if (ct.indexOf('application/json') === -1) {
        return res.text().then(function (t) {
          if (!res.ok) { throw new Error(t || ('HTTP ' + res.status)); }
          return null;
        });
      }
      return res.text().then(function (text) {
        var data = null;
        if (text) { try { data = JSON.parse(text); } catch (_) { data = null; } }
        if (!res.ok) {
          var msg = (data && (data.message || data.error)) || ('HTTP ' + res.status);
          var err = new Error(msg);
          err.status = res.status;
          // Session expired or revoked mid-use: drop the token and return
          // to the login screen. /auth/login is exempt - a 401 there just
          // means wrong credentials and is handled by the login form.
          if (res.status === 401 && path !== '/auth/login') {
            handleAuthFailure(err);
          }
          throw err;
        }
        return data;
      });
    });
  }

  function get(path) { return request('GET', path); }
  function post(path, body) { return request('POST', path, body); }
  function patch(path, body) { return request('PATCH', path, body); }
  function del(path) { return request('DELETE', path); }

  /* Defensive JSON readers. The backend contract is still settling, so these
   * accept: a raw array, a keyed collection, or a {code,data,...} envelope. */
  function unwrap(d) {
    if (d && typeof d === 'object' && !Array.isArray(d) && 'data' in d) { return d.data; }
    return d;
  }
  function listOf(d, keys) {
    d = unwrap(d);
    if (Array.isArray(d)) { return d; }
    if (d && typeof d === 'object') {
      for (var i = 0; i < keys.length; i++) {
        var v = d[keys[i]];
        if (Array.isArray(v)) { return v; }
      }
    }
    return [];
  }
  function objOf(d, keys) {
    d = unwrap(d);
    if (d && typeof d === 'object' && !Array.isArray(d)) {
      for (var i = 0; i < keys.length; i++) {
        if (d[keys[i]] && typeof d[keys[i]] === 'object') { return d[keys[i]]; }
      }
      return d;
    }
    return {};
  }

  /* /resource/current returns a ResourceCurrentReply whose relevant fields are
   * split between the top level (duration, duration_format, seek, seek_format,
   * hit_cache) and a nested `resource` object (path, unique, seek, end). This
   * normalizer merges them into one flat object so the view code can read
   * current.path / current.unique / current.seek / current.seek_format /
   * current.duration / current.duration_format without caring where they live.
   * It falls back to the other location when a field is absent and returns an
   * empty object when no field at all is present. */
  function normalizeCurrent(d) {
    d = unwrap(d);
    var flat = (d && typeof d === 'object' && !Array.isArray(d)) ? d : {};
    var nested = (flat.resource && typeof flat.resource === 'object' && !Array.isArray(flat.resource))
      ? flat.resource : {};
    var out = {};
    var has = false;
    function take(dst, src) {
      if (!has && (src.path != null || src.unique != null || src.seek != null ||
          src.seek_format != null || src.duration != null || src.duration_format != null)) {
        has = true;
      }
      if (src.path != null) { dst.path = src.path; }
      if (src.unique != null) { dst.unique = src.unique; }
      if (src.seek != null) { dst.seek = src.seek; }
      if (src.seek_format != null) { dst.seek_format = src.seek_format; }
      if (src.duration != null) { dst.duration = src.duration; }
      if (src.duration_format != null) { dst.duration_format = src.duration_format; }
    }
    /* path/unique live in the nested resource object; prefer it there, but
     * fall back to the top level in case the backend flattens them. */
    take(out, nested);
    take(out, flat);
    return has ? out : {};
  }

  function setConn(online) {
    var b = $('#connBadge');
    if (!b) { return; }
    b.textContent = online ? '在线' : '离线';
    b.className = 'badge ' + (online ? 'conn-online' : 'conn-offline');
  }

  /* ------------------------------------------------------------------ *
   * Formatting helpers
   * ------------------------------------------------------------------ */
  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n < 1024) { return n + ' B'; }
    var u = ['KB', 'MB', 'GB', 'TB'];
    var i = -1;
    do { n = n / 1024; i++; } while (n >= 1024 && i < u.length - 1);
    return n.toFixed(1) + ' ' + u[i];
  }

  function fmtTime(v) {
    if (!v) { return '-'; }
    var d;
    if (typeof v === 'number') { d = new Date(v * 1000); }
    else { d = new Date(v); }
    if (isNaN(d.getTime())) { return String(v); }
    var p = function (x) { return (x < 10 ? '0' : '') + x; };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
      p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
  }

  function fmtDur(sec) {
    sec = Math.max(0, Number(sec) || 0);
    var h = Math.floor(sec / 3600);
    var m = Math.floor((sec % 3600) / 60);
    var s = Math.floor(sec % 60);
    var p = function (x) { return (x < 10 ? '0' : '') + x; };
    return (h > 0 ? h + ':' : '') + p(m) + ':' + p(s);
  }

  function fmtAgo(v) {
    if (!v) { return '-'; }
    var t;
    if (typeof v === 'number') { t = v * 1000; } else { t = new Date(v).getTime(); }
    if (isNaN(t)) { return '-'; }
    var diff = Date.now() - t;
    if (diff < 0) { diff = 0; }
    var s = Math.floor(diff / 1000);
    if (s < 60) { return s + ' 秒前'; }
    var m = Math.floor(s / 60);
    if (m < 60) { return m + ' 分钟前'; }
    var h = Math.floor(m / 60);
    if (h < 24) { return h + ' 小时前'; }
    return Math.floor(h / 24) + ' 天前';
  }

  /* ------------------------------------------------------------------ *
   * Generic state renderers
   * ------------------------------------------------------------------ */
  function loadingView() {
    return el('div', { class: 'loading' },
      '<div class="spinner"></div><div class="muted">正在加载 ...</div>');
  }

  function emptyView(msg, actionHtml) {
    return el('div', { class: 'empty' },
      '<div class="state-icon">&#128193;</div><p>' + esc(msg) + '</p>' + (actionHtml || ''));
  }

  function errorView(err, retry) {
    var node = el('div', { class: 'error' },
      '<div class="state-icon">&#9888;&#65039;</div><p><strong>加载失败</strong></p>' +
      '<pre>' + esc(err && err.message ? err.message : String(err)) + '</pre>');
    if (retry) {
      node.appendChild(el('button', { class: 'btn', type: 'button', text: '重试' },
        undefined));
      node.querySelector('button').addEventListener('click', retry);
    }
    return node;
  }

  function stateBlock() {
    var box = el('div', { class: 'state' });
    box.appendChild(loadingView().firstChild.cloneNode(true));
    box.appendChild(el('p', { text: '正在加载 ...' }));
    return box;
  }

  function setView(html) { view.innerHTML = ''; view.appendChild(html); }

  /* ------------------------------------------------------------------ *
   * Overview
   * ------------------------------------------------------------------ */
  function renderOverview() {
    setView(loadingView());
    var cards = [];
    var panels = [];

    Promise.allSettled([
      get('/media/list'),
      get('/playlist/list'),
      get('/stream'),
      get('/engine/status')
    ]).then(function (res) {
      setConn(res.some(function (r) { return r.status === 'fulfilled'; }));

      var media = listOf(res[0].status === 'fulfilled' ? res[0].value : [], ['media', 'items', 'list']);
      var playlists = listOf(res[1].status === 'fulfilled' ? res[1].value : [], ['playlists', 'items', 'list']);
      var streams = listOf(res[2].status === 'fulfilled' ? res[2].value : [], ['streams', 'items', 'list']);
      var eng = objOf(res[3].status === 'fulfilled' ? res[3].value : {}, ['status']);

      var running = streams.filter(function (s) { return !!s.running; }).length;
      var engRunning = !!(eng && eng.running);
      var engPaused = !!(eng && eng.paused);
      var engState = engPaused ? '已暂停' : (engRunning ? '推流中' : '已停止');

      cards = [
        { label: '媒体库', value: media.length, sub: '推流内容源' },
        { label: '节目单', value: playlists.length, sub: '播出计划' },
        { label: '推流任务', value: running + '/' + streams.length, sub: '运行中 / 总数' },
        { label: '当前推流', value: engState, sub: engRunning ? (eng.sourcePath || '-') : '播放控制 → 播放', ok: engRunning }
      ];

      panels.push(el('div', { class: 'card' },
        '<h2>当前推流</h2>' +
        '<div class="kv">' +
        '<dt>状态</dt><dd>' + esc(engState) + '</dd>' +
        '<dt>内容源</dt><dd class="mono">' + esc((eng && eng.sourcePath) || '-') + '</dd>' +
        '<dt>输出</dt><dd class="mono">' + esc((eng && eng.outputURLs && eng.outputURLs.join('；')) || '-') + '</dd>' +
        '<dt>进度</dt><dd>' + (eng ? Math.round(Number(eng.progress) || 0) + '%' : '-') + '</dd>' +
        '<dt>运行时长</dt><dd>' + esc((eng && eng.uptime) || '-') + '</dd>' +
        '</div>'));

      panels.push(el('div', { class: 'card' },
        '<h2>服务器</h2>' +
        '<div class="kv">' +
        '<dt>状态</dt><dd><span class="badge ' + (engRunning ? 'badge-ok' : 'badge-neutral') + '">' + (engRunning ? '推流中' : '待机') + '</span></dd>' +
        '<dt>推流任务</dt><dd>' + running + ' / ' + streams.length + ' 运行中</dd>' +
        '<dt>媒体</dt><dd>' + media.length + ' 条</dd>' +
        '<dt>节目单</dt><dd>' + playlists.length + ' 个</dd>' +
        '</div>'));

      var wrap = el('div', { class: 'grid' });

      var cardGrid = el('div', { class: 'grid grid-cards' });
      cards.forEach(function (c) {
        var style = c.ok === false ? ' color:var(--crit);' : '';
        cardGrid.appendChild(el('div', { class: 'card stat' },
          '<div class="stat-label">' + esc(c.label) + '</div>' +
          '<div class="stat-value" style="' + style + '">' + esc(c.value) + '</div>' +
          '<div class="stat-sub">' + esc(c.sub) + '</div>'));
      });
      wrap.appendChild(cardGrid);

      var two = el('div', { class: 'grid grid-2 section' });
      panels.forEach(function (p) { two.appendChild(p); });
      wrap.appendChild(two);

      // 当前推流操作：播放目标 / 暂停 / 继续 / 跳过 / 停止 / 应急插播。
      // 与独立播放页相同的交互，集中在这里。
      var ctrlCard = el('div', { class: 'card section' },
        '<h2>当前推流</h2>' +
        '<div class="field"><label>播放目标（媒体/目录 或 节目单；目录/节目单按各自顺序连续播放）</label>' +
        '<select id="pbTarget" class="mono"></select></div>' +
        '<div class="form-grid two" style="margin-top:10px">' +
        '<div class="field"><label for="pbSeek">起始位置（秒，可选）</label><input id="pbSeek" type="number" min="0" placeholder="0 = 从头播放"></div>' +
        '<div class="cluster" style="align-items:flex-end"><label class="check"><input id="pbRandom" type="checkbox"> 节目单随机播放</label></div>' +
        '</div>' +
        '<div class="controls" style="margin-top:10px">' +
        '<button id="pbPlay" class="btn btn-primary" type="button">播放</button>' +
        '<button class="btn" type="button" data-ctrl="pause">暂停</button>' +
        '<button class="btn" type="button" data-ctrl="continue">继续</button>' +
        '<button class="btn" type="button" data-ctrl="skip">跳过</button>' +
        '<button class="btn btn-danger" type="button" data-ctrl="stop">停止</button>' +
        '</div>' +
        '<p class="muted mt">暂停会挂起推流并记住位置，继续从暂停位置恢复；跳过切到下一个内容。</p>');
      var pbSel = ctrlCard.querySelector('#pbTarget');
      pbSel.appendChild(el('option', { value: '', text: '-- 选择节目单 --' }));
      playlists.forEach(function (p) {
        pbSel.appendChild(el('option', { value: p.id, text: p.name, dataset: { kind: 'playlist' } }));
      });
      ctrlCard.querySelector('#pbPlay').addEventListener('click', function () {
        var opt = pbSel.selectedOptions && pbSel.selectedOptions[0];
        if (!opt || !opt.value) { toast('请选择节目单或媒体目标', 'err'); return; }
        var body = opt.dataset && opt.dataset.kind === 'playlist' ? { playlistId: opt.value } : { mediaId: opt.value };
        var seekV = parseFloat(ctrlCard.querySelector('#pbSeek').value);
        if (seekV > 0) { body.seekSeconds = seekV; }
        if (ctrlCard.querySelector('#pbRandom').checked) { body.random = true; }
        var btn = ctrlCard.querySelector('#pbPlay');
        btn.disabled = true; btn.textContent = '正在启动 ...';
        post('/player/play', body).then(function () {
          toast('播放已启动', 'ok');
          setTimeout(renderOverview, 1200);
        }).catch(function (e) { toast(e.message, 'err'); })
          .finally(function () { btn.disabled = false; btn.textContent = '播放'; });
      });
      ctrlCard.querySelector('[data-ctrl=pause]').addEventListener('click', function () {
        post('/player/pause', {}).then(function () { toast('已暂停（可继续）', 'ok'); setTimeout(renderOverview, 1200); }).catch(function (e) { toast(e.message, 'err'); });
      });
      ctrlCard.querySelector('[data-ctrl=continue]').addEventListener('click', function () {
        post('/player/continue', {}).then(function () { toast('已继续', 'ok'); setTimeout(renderOverview, 1200); }).catch(function (e) { toast(e.message, 'err'); });
      });
      ctrlCard.querySelector('[data-ctrl=skip]').addEventListener('click', function () {
        post('/player/skip', {}).then(function () { toast('已跳过', 'ok'); setTimeout(renderOverview, 1200); }).catch(function (e) { toast(e.message, 'err'); });
      });
      ctrlCard.querySelector('[data-ctrl=stop]').addEventListener('click', function () {
        post('/player/stop', {}).then(function () { toast('已停止', 'ok'); setTimeout(renderOverview, 1200); }).catch(function (e) { toast(e.message, 'err'); });
      });
      wrap.appendChild(ctrlCard);

      // 应急插播
      var intCard = el('div', { class: 'card section' },
        '<h2>应急插播</h2>' +
        '<div class="field"><label for="pbIntTarget">插播目标</label><select id="pbIntTarget" class="mono"></select></div>' +
        '<div class="field"><label for="pbIntDur">时长（秒，留空为一直插播到手动停止）</label><input id="pbIntDur" type="number" min="1" placeholder="例如 60"></div>' +
        '<div class="form-actions"><button id="pbIntGo" class="btn btn-primary" type="button">立即插播</button></div>' +
        '<p class="muted mt">插播会立即中断当前推流播放指定内容；填了时长则到时自动恢复原内容。</p>');
      var intSel = intCard.querySelector('#pbIntTarget');
      intSel.appendChild(el('option', { value: '', text: '-- 选择节目单 --' }));
      playlists.forEach(function (p) {
        intSel.appendChild(el('option', { value: p.id, text: p.name, dataset: { kind: 'playlist' } }));
      });
      intCard.querySelector('#pbIntGo').addEventListener('click', function () {
        var opt = intSel.selectedOptions && intSel.selectedOptions[0];
        if (!opt || !opt.value) { toast('请选择插播目标', 'err'); return; }
        var body = opt.dataset && opt.dataset.kind === 'playlist' ? { playlistId: opt.value } : { mediaId: opt.value };
        var d = Number(intCard.querySelector('#pbIntDur').value);
        if (d > 0) { body.duration = d; }
        var btn = intCard.querySelector('#pbIntGo');
        btn.disabled = true; btn.textContent = '正在插播 ...';
        post('/player/interrupt', body).then(function () {
          toast('插播已启动', 'ok');
          setTimeout(renderOverview, 1200);
        }).catch(function (e) { toast(e.message, 'err'); })
          .finally(function () { btn.disabled = false; btn.textContent = '立即插播'; });
      });
      wrap.appendChild(intCard);

      setView(wrap);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderOverview));
    });
  }

  /* ------------------------------------------------------------------ *
   * Playback
   * ------------------------------------------------------------------ */
  function renderPlayback() {
    setView(loadingView());
    Promise.allSettled([
      get('/resource/current'),
      get('/play/duration'),
      get('/play/information'),
      get('/play/encode'),
      get('/media/list'),
      get('/playlist/list'),
      get('/engine/status')
    ]).then(function (res) {
      setConn(res.some(function (r) { return r.status === 'fulfilled'; }));
      var current = normalizeCurrent(res[0].status === 'fulfilled' ? res[0].value : {});
      var dur = objOf(res[1].status === 'fulfilled' ? res[1].value : {}, ['data', 'duration']);
      var info = objOf(res[2].status === 'fulfilled' ? res[2].value : {}, ['data', 'information']);
      var enc = objOf(res[3].status === 'fulfilled' ? res[3].value : {}, ['data', 'encode']);
      var mediaItems = listOf(res[4].status === 'fulfilled' ? res[4].value : [], ['media', 'items', 'list']);
      var playlists = listOf(res[5].status === 'fulfilled' ? res[5].value : [], ['playlists', 'items', 'list']);
      var eng = objOf(res[6].status === 'fulfilled' ? res[6].value : {}, ['status']);

      var playing = current.path || current.unique;
      var seek = Number(current.seek) || 0;
      var total = Number(current.duration) || 0;
      var pct = total > 0 ? Math.min(100, (seek / total) * 100) : 0;

      // 引擎模式下以真实引擎状态为准：显示真实源/进度/运行或暂停。
      if (eng && (eng.running || eng.paused)) {
        playing = eng.sourcePath || playing;
        var engPct = Math.max(0, Math.min(100, Number(eng.progress) || 0));
        var engBadge = eng.paused ? '<span class="badge badge-warn">已暂停</span>'
          : (eng.running ? '<span class="badge badge-ok">推流中</span>' : '<span class="badge badge-neutral">已停止</span>');
        pct = engPct;
        total = 0; seek = 0;
      }

      var wrap = el('div', { class: 'grid' });

      var now = el('div', { class: 'now-playing' },
        '<div class="now-title">' + esc(playing || '当前无播放') + ' ' + (eng && (eng.running || eng.paused) ? engBadge : '') + '</div>' +
        '<div class="now-sub">' + (eng && (eng.running || eng.paused) ? ('引擎状态: ' + (eng.paused ? '已暂停，可点「继续」恢复' : 'ffmpeg 进程 ' + (eng.pid || '-') + ' 推送中')) : esc(current.unique ? '唯一标识: ' + current.unique : '无活动资源')) + '</div>' +
        '<div class="progress-track"><div class="progress-fill" style="width:' + pct + '%"></div></div>' +
        '<div class="time-row"><span>' + (total > 0 ? fmtDur(seek) : (eng && (eng.running || eng.paused) ? Math.round(engPct || 0) + '%' : fmtDur(seek))) + '</span><span>' + (total > 0 ? fmtDur(total) : (eng && (eng.running || eng.paused) ? '进度' : fmtDur(total))) + '</span></div>');

      var controls = el('div', { class: 'controls' });
      var btnPause = el('button', { class: 'btn', type: 'button', text: '暂停' });
      var btnCont = el('button', { class: 'btn', type: 'button', text: '继续' });
      var btnSkip = el('button', { class: 'btn', type: 'button', text: '跳过' });
      var btnStop = el('button', { class: 'btn btn-danger', type: 'button', text: '停止' });

      btnPause.addEventListener('click', function () {
        post('/player/pause', {}).then(function () { toast('播放已暂停（推流已挂起，可继续）', 'ok'); setTimeout(renderPlayback, 900); }).catch(function (e) { toast(e.message, 'err'); });
      });
      btnCont.addEventListener('click', function () {
        post('/player/continue', {}).then(function () { toast('播放已继续', 'ok'); setTimeout(renderPlayback, 900); }).catch(function (e) { toast(e.message, 'err'); });
      });
      btnSkip.addEventListener('click', function () {
        post('/player/skip', {}).then(function () { toast('已跳到下一个资源', 'ok'); setTimeout(renderPlayback, 900); }).catch(function (e) { toast(e.message, 'err'); });
      });
      btnStop.addEventListener('click', function () {
        post('/player/stop', {}).then(function () { toast('播放已停止', 'ok'); setTimeout(renderPlayback, 900); }).catch(function (e) { toast(e.message, 'err'); });
      });
      [btnPause, btnCont, btnSkip, btnStop].forEach(function (b) { controls.appendChild(b); });
      now.appendChild(controls);

      var seekRow = el('div', { class: 'seek-row' });
      var seekIn = el('input', { type: 'number', min: '0', placeholder: '进度（秒）', value: '' });
      seekIn.style.maxWidth = '160px';
      var btnSeek = el('button', { class: 'btn btn-primary', type: 'button', text: '跳转' });
      btnSeek.addEventListener('click', function () {
        var v = Number(seekIn.value);
        if (!(v >= 0)) { toast('请输入有效的跳转秒数', 'err'); return; }
        post('/player/seek', { seekSeconds: v }).then(function () { toast('已跳转到 ' + v + ' 秒', 'ok'); setTimeout(renderPlayback, 900); }).catch(function (e) { toast(e.message, 'err'); });
      });
      seekRow.appendChild(seekIn);
      seekRow.appendChild(btnSeek);
      // 「跳到结尾前 10 秒」仅在已知总时长时可用（引擎模式下总时长未知，隐藏）。
      if (total > 0) {
        var btnSkipTo = el('button', { class: 'btn', type: 'button', text: '跳到结尾前 10 秒' });
        btnSkipTo.addEventListener('click', function () {
          var v = Math.max(0, total - 10);
          post('/player/seek', { seekSeconds: v }).then(function () { toast('已跳到接近结尾处', 'ok'); setTimeout(renderPlayback, 900); }).catch(function (e) { toast(e.message, 'err'); });
        });
        seekRow.appendChild(btnSkipTo);
      }
      now.appendChild(seekRow);
      wrap.appendChild(now);

      // 一键推流：选媒体 + 填推流地址，一步完成输出配置与播放。
      var oneClick = el('div', { class: 'card section' },
        '<h2>一键推流</h2>' +
        '<p class="muted" style="margin:0 0 12px">选一个媒体，填推流地址，点一下即完成输出配置并开始推流（使用 1280x720 / 2500kbps / 25fps / H.264，将替换现有输出配置）。</p>' +
        '<div class="form-grid">' +
        '<div class="field"><label for="ocMedia">媒体</label><select id="ocMedia" class="mono"></select></div>' +
        '<div class="field"><label for="ocUrl">推流地址（RTMP）</label><input id="ocUrl" type="text" placeholder="rtmp://目标平台/live/流名"></div>' +
        '</div>' +
        '<div class="form-actions"><button id="ocGo" class="btn btn-primary" type="button">开始推流</button></div>');
      var ocSel = oneClick.querySelector('#ocMedia');
      ocSel.appendChild(el('option', { value: '', text: '-- 选择媒体 --' }));
      mediaItems.forEach(function (mm) {
        ocSel.appendChild(el('option', { value: mm.id, text: (mm.name || mm.path) + '  [' + mm.id + ']' }));
      });
      oneClick.querySelector('#ocGo').addEventListener('click', function () {
        var mid = ocSel.value;
        var url = oneClick.querySelector('#ocUrl').value.trim();
        if (!mid) { toast('请先选择媒体', 'err'); return; }
        if (!url || url.indexOf('rtmp://') !== 0) { toast('请填写有效的推流地址（rtmp://...）', 'err'); return; }
        var btn = oneClick.querySelector('#ocGo');
        btn.disabled = true; btn.textContent = '推流启动中 ...';
        get('/engine/ffmpeg').then(function (cur) {
          // 保留用户已配置的 ffmpeg 路径，避免被默认值覆盖
          var cfg = objOf(cur, ['config', 'data']) || {};
          return post('/engine/ffmpeg', {
            ffmpegPath: cfg.ffmpegPath || undefined,
            outputs: [{ url: url, width: 1280, height: 720, bitrateKbps: 2500, fps: 25, codec: 'libx264' }]
          });
        }).then(function () {
          return post('/player/play', { mediaId: mid });
        }).then(function () {
          toast('已配置输出并开始推流', 'ok');
          setTimeout(renderPlayback, 1500);
        }).catch(function (e) { toast(e.message, 'err'); })
          .finally(function () { btn.disabled = false; btn.textContent = '开始推流'; });
      });
      wrap.appendChild(oneClick);

      // Play target: start playback of an already-loaded media item or a
      // playlist through the management /player/play endpoint.
      var targetCard = el('div', { class: 'card section' },
        '<h2>播放目标</h2>' +
        '<div class="field"><label for="pbTarget">目标</label><select id="pbTarget" class="mono"></select></div>' +
        '<div class="form-grid two" style="margin-top:10px">' +
        '<div class="field"><label for="pbSeek">起始位置（秒，可选）</label><input id="pbSeek" type="number" min="0" placeholder="0 = 从头播放"></div>' +
        '<div class="cluster" style="align-items:flex-end"><label class="check"><input id="pbRandom" type="checkbox"> 随机播放（节目单时随机选一项）</label></div>' +
        '</div>' +
        '<div class="form-actions"><button id="pbPlay" class="btn btn-primary" type="button">播放</button></div>');
      var pbSel = targetCard.querySelector('#pbTarget');
      pbSel.appendChild(el('option', { value: '', text: '-- 选择目标 --' }));
      if (playlists.length) {
        var gPl = el('optgroup', { label: '节目单' });
        playlists.forEach(function (p) {
          gPl.appendChild(el('option', { value: p.id, text: p.name, dataset: { kind: 'playlist' } }));
        });
        pbSel.appendChild(gPl);
      }
      if (mediaItems.length) {
        var gMd = el('optgroup', { label: '媒体' });
        mediaItems.forEach(function (mm) {
          gMd.appendChild(el('option', { value: mm.id, text: (mm.name || mm.path) + '  [' + mm.id + ']', dataset: { kind: 'media' } }));
        });
        pbSel.appendChild(gMd);
      }
      var pbBtn = targetCard.querySelector('#pbPlay');
      pbBtn.addEventListener('click', function () {
        var opt = pbSel.selectedOptions && pbSel.selectedOptions[0];
        if (!opt || !opt.value) { toast('请选择节目单或媒体目标', 'err'); return; }
        var body = opt.dataset && opt.dataset.kind === 'playlist'
          ? { playlistId: opt.value }
          : { mediaId: opt.value };
        var seekV = parseFloat(targetCard.querySelector('#pbSeek').value);
        if (seekV > 0) { body.seekSeconds = seekV; }
        if (targetCard.querySelector('#pbRandom').checked) { body.random = true; }
        pbBtn.disabled = true;
        pbBtn.textContent = '正在启动 ...';
        post('/player/play', body).then(function () {
          toast('播放已启动', 'ok');
          setTimeout(renderPlayback, 900);
        }).catch(function (e) { toast(e.message, 'err'); })
          .finally(function () { pbBtn.disabled = false; pbBtn.textContent = '播放'; });
      });
      wrap.appendChild(targetCard);

      // Interrupt: play a target immediately over the current program. A
      // duration > 0 makes the backend restore the previous playlist when it
      // elapses; omitted/0 is a one-shot interrupt.
      var intCard = el('div', { class: 'card section' },
        '<h2>插播</h2>' +
        '<div class="field"><label for="pbIntTarget">目标</label><select id="pbIntTarget" class="mono"></select></div>' +
        '<div class="field"><label for="pbIntDur">时长（秒，留空为单次）</label><input id="pbIntDur" type="number" min="1" placeholder="例如 60"></div>' +
        '<div class="form-actions"><button id="pbIntGo" class="btn btn-primary" type="button">立即插播</button></div>');
      var intSel = intCard.querySelector('#pbIntTarget');
      intSel.appendChild(el('option', { value: '', text: '-- 选择目标 --' }));
      if (playlists.length) {
        var igPl = el('optgroup', { label: '节目单' });
        playlists.forEach(function (p) {
          igPl.appendChild(el('option', { value: p.id, text: p.name, dataset: { kind: 'playlist' } }));
        });
        intSel.appendChild(igPl);
      }
      if (mediaItems.length) {
        var igMd = el('optgroup', { label: '媒体' });
        mediaItems.forEach(function (mm) {
          igMd.appendChild(el('option', { value: mm.id, text: (mm.name || mm.path) + '  [' + mm.id + ']', dataset: { kind: 'media' } }));
        });
        intSel.appendChild(igMd);
      }
      var intBtn = intCard.querySelector('#pbIntGo');
      var intDur = intCard.querySelector('#pbIntDur');
      intBtn.addEventListener('click', function () {
        var opt = intSel.selectedOptions && intSel.selectedOptions[0];
        if (!opt || !opt.value) { toast('请选择节目单或媒体目标', 'err'); return; }
        var body = opt.dataset && opt.dataset.kind === 'playlist'
          ? { playlistId: opt.value }
          : { mediaId: opt.value };
        var d = Number(intDur.value);
        if (d > 0) { body.duration = d; }
        intBtn.disabled = true;
        intBtn.textContent = '正在插播 ...';
        post('/player/interrupt', body).then(function () {
          toast('插播已启动', 'ok');
          setTimeout(renderPlayback, 900);
        }).catch(function (e) { toast(e.message, 'err'); })
          .finally(function () { intBtn.disabled = false; intBtn.textContent = '立即插播'; });
      });
      wrap.appendChild(intCard);

      var infoCard = el('div', { class: 'card section' },
        '<h2>引擎</h2><div class="kv">' +
        '<dt>主版本</dt><dd>' + esc(info.major_version || '-') + '</dd>' +
        '<dt>库版本</dt><dd>' + esc(info.libkplayer_version || '-') + '</dd>' +
        '<dt>插件</dt><dd>' + esc(info.plugin_version || '-') + '</dd>' +
        '<dt>授权</dt><dd>' + esc(info.license_version || '-') + '</dd>' +
        '<dt>启动时间</dt><dd>' + esc(fmtTime(info.start_time_timestamp || info.start_time)) + '</dd>' +
        '</div>');

      var encCard = el('div', { class: 'card section' },
        '<h2>编码配置</h2><div class="kv">' +
        '<dt>分辨率</dt><dd>' + esc((enc.video_width || '?') + 'x' + (enc.video_height || '?')) + '</dd>' +
        '<dt>帧率</dt><dd>' + esc(enc.video_fps || '-') + '</dd>' +
        '<dt>音频采样率</dt><dd>' + esc(enc.audio_sample_rate || '-') + ' Hz</dd>' +
        '<dt>码率</dt><dd>' + esc(enc.bit_rate || '-') + '</dd>' +
        '<dt>质量</dt><dd>' + esc(enc.avg_quality || '-') + ' (1-30)</dd>' +
        '</div>');

      var engineRow = el('div', { class: 'grid grid-2 section' });
      engineRow.appendChild(infoCard);
      engineRow.appendChild(encCard);
      wrap.appendChild(engineRow);

      setView(wrap);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderPlayback));
    });
  }

  /* ------------------------------------------------------------------ *
   * Stream Engine (ffmpeg)
   * ------------------------------------------------------------------ */
  /* ------------------------------------------------------------------ *
   * 多路推流：多个推流任务并行，每个任务独立内容与独立输出平台
   * ------------------------------------------------------------------ */
  function renderStreams() {
    setView(loadingView());
    Promise.all([get('/stream/list'), get('/playlist/list'), get('/engine/ffmpeg')]).then(function (res) {
      setConn(true);
      var streams = listOf(res[0], ['streams', 'items', 'list']);
      var playlists = listOf(res[1], ['playlists', 'items', 'list']);
      var gcfg = objOf(res[2], ['config', 'data']) || {};
      drawStreams(streams, playlists, gcfg);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderStreams));
    });
  }

  function drawStreams(streams, playlists, gcfg) {
    var plById = {};
    playlists.forEach(function (p) { plById[p.id] = p; });
    var wrap = el('div', {});

    // 快速推流：选节目单 + 填地址，一步开始（替换当前引擎输出配置）。
    var quick = el('div', { class: 'card section' },
      '<h2>快速推流</h2>' +
      '<p class="muted" style="margin:0 0 12px">选一个节目单，填推流地址，点一下立即开始推流（1280x720 / 2500kbps / 25fps）。需要多条线路或多个内容，请使用下面的「新建推流任务」。</p>' +
      '<div class="form-grid">' +
      '<div class="field"><label for="ocSearch">搜索节目单</label><input id="ocSearch" type="text" placeholder="按名称搜索..."></div>' +
      '<div class="field"><label for="ocPlaylist">节目单</label><select id="ocPlaylist" class="mono"></select></div>' +
      '<div class="field"><label for="ocUrl">推流地址（RTMP）</label><input id="ocUrl" type="text" placeholder="rtmp://目标平台/live/流名"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="ocGo" class="btn btn-primary" type="button">开始推流</button></div>');
    var ocSel = quick.querySelector('#ocPlaylist');
    function fillOcOptions(q) {
      ocSel.innerHTML = '';
      ocSel.appendChild(el('option', { value: '', text: '-- 选择节目单 --' }));
      playlists.forEach(function (p) {
        if (q && String(p.name || '').toLowerCase().indexOf(q.toLowerCase()) < 0) { return; }
        ocSel.appendChild(el('option', { value: p.id, text: p.name }));
      });
    }
    fillOcOptions('');
    quick.querySelector('#ocSearch').addEventListener('input', function () {
      fillOcOptions(quick.querySelector('#ocSearch').value.trim());
    });
    quick.querySelector('#ocGo').addEventListener('click', function () {
      var pid = ocSel.value;
      var url = quick.querySelector('#ocUrl').value.trim();
      if (!pid) { toast('请先选择节目单', 'err'); return; }
      if (!url || url.indexOf('rtmp://') !== 0) { toast('请填写有效的推流地址（rtmp://...）', 'err'); return; }
      var btn = quick.querySelector('#ocGo');
      btn.disabled = true; btn.textContent = '推流启动中 ...';
      post('/engine/ffmpeg', {
        ffmpegPath: gcfg.ffmpegPath || undefined,
        outputs: [{ url: url, width: 1280, height: 720, bitrateKbps: 2500, fps: 25, codec: 'libx264' }]
      }).then(function () {
        return post('/player/play', { playlistId: pid });
      }).then(function () {
        toast('已开始推流', 'ok');
        renderStreams();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btn.disabled = false; btn.textContent = '开始推流'; });
    });
    wrap.appendChild(quick);

    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0">推流任务 (' + streams.length + ')</h2>' +
      '<button id="newStream" class="btn btn-primary" type="button">新建推流任务</button>'));

    if (!streams.length) {
      wrap.appendChild(el('div', { class: 'card' },
        emptyView('还没有推流任务。新建一个任务：选择内容（媒体）、添加输出平台（RTMP 地址），即可同时向多个平台推送不同的内容。', undefined).innerHTML));
    } else {
      var stack = el('div', { class: 'list-stack' });
      streams.forEach(function (s) {
        var item = el('div', { class: 'list-item' },
          '<div class="list-item-head"><span class="list-item-title">' + esc(s.name || s.id) +
          '</span><div class="icon-btn-row"></div></div>' +
          '<div class="list-item-meta">' +
          '<span class="muted">内容: ' + esc(s.playlistId ? ((plById[s.playlistId] && plById[s.playlistId].name) || '节目单') : '-') + '</span>' +
          '<span class="badge ' + (s.running ? 'badge-ok' : 'badge-neutral') + '">' + (s.running ? '运行中' : '已停止') + '</span>' +
          (s.bitrateKbps ? '<span>码率 ' + s.bitrateKbps + ' kbps</span>' : '') +
          '</div>');
        var outs = el('div', { class: 'list-item-meta' });
        (s.outputs || []).forEach(function (o) {
          outs.appendChild(el('span', { class: 'badge badge-info', text: o.url }));
        });
        item.appendChild(outs);
        var ops = item.querySelector('.icon-btn-row');
        if (s.running) {
          var btnStop = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '停止' });
          btnStop.addEventListener('click', function () {
            post('/stream/' + s.id + '/stop', {}).then(function () { toast('已停止: ' + s.name, 'ok'); renderStreams(); })
              .catch(function (e) { toast(e.message, 'err'); });
          });
          ops.appendChild(btnStop);
        } else {
          var btnStart = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '启动' });
          btnStart.addEventListener('click', function () {
            var b = btnStart;
            b.disabled = true; b.textContent = '启动中 ...';
            post('/stream/' + s.id + '/start', {}).then(function () { toast('已启动: ' + s.name, 'ok'); setTimeout(renderStreams, 1500); })
              .catch(function (e) { toast(e.message, 'err'); })
              .finally(function () { b.disabled = false; b.textContent = '启动'; });
          });
          ops.appendChild(btnStart);
        }
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        btnEdit.addEventListener('click', function () { openStreamEdit(s, playlists, gcfg); });
        ops.appendChild(btnEdit);
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除推流任务 "' + s.name + '"？')) { return; }
          del('/stream/' + s.id).then(function () { toast('任务已删除', 'ok'); renderStreams(); })
            .catch(function (e) { toast(e.message, 'err'); });
        });
        ops.appendChild(btnDel);
        stack.appendChild(item);
      });
      wrap.appendChild(stack);
    }
    wrap.querySelector('#newStream').addEventListener('click', function () { openStreamEdit(null, playlists, gcfg); });
    setView(wrap);
  }

  function openStreamEdit(s, playlists, gcfg) {
    var editing = !!s;
    var outputs = editing && Array.isArray(s.outputs) ? s.outputs.map(function (o) { return Object.assign({}, o); }) : [];
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑推流任务' : '新建推流任务') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>任务名称</label><input id="stName" type="text" value="' + esc(editing ? s.name : '') + '"></div>' +
      '<div class="field"><label>搜索节目单</label><input id="stSearch" type="text" placeholder="按名称搜索..."></div>' +
      '<div class="field"><label>内容（节目单：文件或目录按顺序连续播放）</label><select id="stPlaylist" class="mono"></select></div>' +
      '<div class="field"><label>断线自动重推间隔（秒，0 = 关闭）</label><input id="stReconnect" type="number" min="0" value="' + (editing ? (s.reconnectInterval || 5) : 5) + '"></div>' +
      '</div>' +
      '<div class="field mt"><label>输出线路（一个任务可同时推多条线路，添加多条）</label><div id="stOutputs"></div>' +
      '<button id="stAddOut" class="btn" type="button">+ 添加线路</button></div>' +
      '<p class="muted">线路预填常用参数（1280x720 / 2500kbps / 25fps / H.264），只需填写推流地址（rtmp://...）。</p>' +
      '<div class="form-actions"><button id="stCancel" class="btn" type="button">取消</button>' +
      '<button id="stSave" class="btn btn-primary" type="button">保存</button></div>');

    var plSel = m.querySelector('#stPlaylist');
    function fillPlOptions(q) {
      plSel.innerHTML = '';
      plSel.appendChild(el('option', { value: '', text: '-- 选择节目单 --' }));
      playlists.forEach(function (p) {
        if (q && String(p.name || '').toLowerCase().indexOf(q.toLowerCase()) < 0) { return; }
        plSel.appendChild(el('option', { value: p.id, text: p.name }));
      });
    }
    fillPlOptions('');
    m.querySelector('#stSearch').addEventListener('input', function () {
      fillPlOptions(m.querySelector('#stSearch').value.trim());
    });
    if (editing && s.playlistId) { plSel.value = s.playlistId; }

    var outHost = m.querySelector('#stOutputs');
    function outRow(o) {
      var row = el('div', { class: 'kp-engine-output' });
      var head = el('div', { class: 'row' },
        '<span class="muted">输出 ' + (outHost.children.length + 1) + '</span>' +
        '<button type="button" class="btn btn-danger btn-sm so-rm">移除</button>');
      head.querySelector('.so-rm').addEventListener('click', function () { row.remove(); });
      row.appendChild(head);
      row.appendChild(el('div', { class: 'field' },
        '<label>推流地址 (RTMP)</label><input type="text" class="so-url" placeholder="rtmp://平台/live/流名" value="' + esc(o.url || '') + '">'));
      row.appendChild(el('div', { class: 'form-grid two' },
        '<div class="field"><label>宽度</label><input type="number" class="so-w" value="' + (o.width || 1280) + '"></div>' +
        '<div class="field"><label>高度</label><input type="number" class="so-h" value="' + (o.height || 720) + '"></div>' +
        '<div class="field"><label>码率 (kbps)</label><input type="number" class="so-b" value="' + (o.bitrateKbps || 2500) + '"></div>' +
        '<div class="field"><label>帧率</label><input type="number" class="so-f" value="' + (o.fps || 25) + '"></div>' +
        '<div class="field"><label>声道数</label><input type="number" class="so-ac" min="1" value="' + (o.audioChannels || '') + '" placeholder="默认"></div>' +
        '<div class="field"><label>采样率</label><input type="number" class="so-ar" min="1" value="' + (o.audioSampleRate || '') + '" placeholder="默认"></div>'));
      row.appendChild(el('div', { class: 'field mt' },
        '<label>滤镜（高级，ffmpeg -vf 语法；如文字水印 drawtext=text="台标":x=20:y=20:fontsize=24，字幕烧录 subtitles=/data/sub.srt）</label>' +
        '<input type="text" class="so-flt" placeholder="留空=无" value="' + esc(o.filters || '') + '">'));
      return row;
    }
    var defaultLine = { url: '', width: 1280, height: 720, bitrateKbps: 2500, fps: 25, codec: 'libx264' };
    outputs.forEach(function (o) { outHost.appendChild(outRow(o)); });
    if (!outputs.length) { outHost.appendChild(outRow(defaultLine)); }
    m.querySelector('#stAddOut').addEventListener('click', function () { outHost.appendChild(outRow({})); });

    m.querySelector('#stCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#stSave').addEventListener('click', function () {
      var name = m.querySelector('#stName').value.trim();
      var playlistId = plSel.value;
      var outs = Array.from(outHost.querySelectorAll('.kp-engine-output')).map(function (row) {
        var out = { url: row.querySelector('.so-url').value.trim() };
        var w = parseInt(row.querySelector('.so-w').value, 10);
        var h = parseInt(row.querySelector('.so-h').value, 10);
        var b = parseInt(row.querySelector('.so-b').value, 10);
        var f = parseInt(row.querySelector('.so-f').value, 10);
        var ac = parseInt(row.querySelector('.so-ac').value, 10);
        var ar = parseInt(row.querySelector('.so-ar').value, 10);
        if (!isNaN(w)) { out.width = w; }
        if (!isNaN(h)) { out.height = h; }
        if (!isNaN(b)) { out.bitrateKbps = b; }
        if (!isNaN(f)) { out.fps = f; }
        if (!isNaN(ac) && ac > 0) { out.audioChannels = ac; }
        if (!isNaN(ar) && ar > 0) { out.audioSampleRate = ar; }
        out.codec = 'libx264';
        var flt = row.querySelector('.so-flt');
        if (flt && flt.value.trim()) { out.filters = flt.value.trim(); }
        return out;
      }).filter(function (o) { return o.url; });
      if (!name) { toast('请输入任务名称', 'err'); return; }
      if (!playlistId) { toast('请选择节目单', 'err'); return; }
      if (!outs.length) { toast('请至少添加一个输出地址', 'err'); return; }
      var body = { name: name, playlistId: playlistId, outputs: outs };
      var ri = parseInt(m.querySelector('#stReconnect').value, 10);
      if (!isNaN(ri) && ri >= 0) { body.reconnectInterval = ri; }
      var p = editing
        ? post('/stream/' + s.id + '/replace', Object.assign({ id: s.id }, body))
        : post('/stream/add', body);
      p.then(function () {
        toast(editing ? '任务已更新' : '任务已创建', 'ok');
        m.remove(); renderStreams();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  function renderEngine() {
    setView(loadingView());
    Promise.allSettled([
      get('/engine/status'),
      get('/engine/ffmpeg')
    ]).then(function (res) {
      setConn(res.some(function (r) { return r.status === 'fulfilled'; }));
      var st = objOf(res[0].status === 'fulfilled' ? res[0].value : {}, ['status']);
      var cfg = objOf(res[1].status === 'fulfilled' ? res[1].value : {}, ['config']);
      drawEngine(st, cfg);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderEngine));
    });
  }

  function drawEngine(st, cfg) {
    var running = !!st.running;
    var paused = !!st.paused;
    var badgeClass = paused ? 'badge-warn' : (running ? 'badge-ok' : (st.exitCode !== 0 ? 'badge-crit' : 'badge-neutral'));
    var badgeText = paused ? '已暂停（点「继续」从暂停位置恢复）' : (running ? '运行中' : (st.exitCode !== 0 ? '已停止（退出码 ' + st.exitCode + '）' : '已停止'));

    var wrap = el('div', { class: 'grid' });

    // Status card: live engine counters plus the source and output targets.
    var statusCard = el('div', { class: 'card section' },
      '<h2>状态</h2>' +
      '<div class="kv">' +
      '<dt>状态</dt><dd><span class="badge ' + badgeClass + '">' + badgeText + '</span></dd>' +
      '<dt>进程号</dt><dd>' + (st.pid ? esc(String(st.pid)) : '-') + '</dd>' +
      '<dt>运行时长</dt><dd>' + esc(st.uptime || '-') + '</dd>' +
      '<dt>码率</dt><dd>' + (st.bitrateKbps ? esc(String(st.bitrateKbps)) + ' kbps' : '-') + '</dd>' +
      '<dt>帧率</dt><dd>' + (st.fps ? esc(String(st.fps)) : '-') + '</dd>' +
      '<dt>帧</dt><dd>' + (st.frame ? esc(String(st.frame)) : '-') + '</dd>' +
      '<dt>源</dt><dd class="mono">' + esc(st.sourcePath || '-') + '</dd>' +
      '</div>');
    var pct = Math.max(0, Math.min(100, Number(st.progress) || 0));
    statusCard.appendChild(el('div', {},
      '<div class="progress-track"><div class="progress-fill" style="width:' + pct + '%"></div></div>' +
      '<div class="time-row"><span>进度</span><span>' + pct + '%</span></div>'));
    if (Array.isArray(st.outputURLs) && st.outputURLs.length) {
      var urlBox = el('div', { class: 'field mt' });
      urlBox.appendChild(el('label', { text: '输出地址' }));
      st.outputURLs.forEach(function (u) {
        urlBox.appendChild(el('div', { class: 'mono' }, esc(u)));
      });
      statusCard.appendChild(urlBox);
    }
    // An abnormal exit leaves the last ffmpeg stderr lines behind: surface
    // them as a red bar so the interruption is unmissable.
    if (st.exitCode !== 0) {
      statusCard.appendChild(el('div', { class: 'kp-engine-error' },
        '<p><strong>流中断</strong> - ffmpeg 进程以退出码 ' + st.exitCode + ' 退出。请从播放视图重新开始播放。</p>' +
        (st.lastError ? '<pre>' + esc(st.lastError) + '</pre>' : '')));
    }

    // Actions: stopping the engine process; restart happens through the
    // Playback view's play target.
    var actCard = el('div', { class: 'card section' },
      '<h2>操作</h2>' +
      '<div class="form-actions"><button id="engStop" class="btn btn-danger" type="button">停止</button></div>' +
      '<p class="muted mt">停止正在运行的 ffmpeg 进程。要重新开始推流，请在播放视图选择媒体目标。</p>');
    var btnStop = actCard.querySelector('#engStop');
    btnStop.disabled = !running;
    btnStop.addEventListener('click', function () {
      btnStop.disabled = true;
      post('/player/stop', {}).then(function () {
        toast('流已停止', 'ok');
        renderEngine();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btnStop.disabled = false; });
    });

    var topRow = el('div', { class: 'grid grid-2 section' });
    topRow.appendChild(statusCard);
    topRow.appendChild(actCard);
    wrap.appendChild(topRow);

    // Config card: ffmpeg path plus one dynamic row of encode parameters
    // per output; rows can be added and removed before saving. Every field
    // has a working default so a fresh install needs no manual setup: the
    // ffmpeg path is auto-detected, reconnect defaults to 5s and new output
    // rows carry the common 1280x720/2500kbps/25fps H.264 preset.
    var cfgCard = el('div', { class: 'card section' },
      '<h2>引擎配置</h2>' +
      '<div class="field"><label for="engFfmpeg">ffmpeg 路径（已自动检测，一般无需修改）</label>' +
      '<input id="engFfmpeg" type="text" spellcheck="false"></div>' +
      '<div class="field"><label for="engReconnect">断线自动重推间隔（秒，0 = 关闭；推流异常断开后自动恢复）</label>' +
      '<input id="engReconnect" type="number" min="0" value="' + (cfg.reconnectInterval || 5) + '"></div>' +
      '<div id="engOutputs"></div>' +
      '<div class="form-actions">' +
      '<button id="engAddOutput" class="btn" type="button">添加输出</button>' +
      '<button id="engSave" class="btn" type="button">保存配置</button>' +
      '<button id="engApply" class="btn btn-primary" type="button">立即生效</button>' +
      '</div>' +
      '<p class="muted mt" style="margin-top:10px">「保存配置」只保存不中断推流；确认无误后点「立即生效」应用到正在进行的推流。输出行已预填常用参数，只需填推流地址。</p>');
    var ffmpegIn = cfgCard.querySelector('#engFfmpeg');
    var detectedPath = cfg.ffmpegPath || '';
    ffmpegIn.value = detectedPath || 'ffmpeg';

    function outputRow(o, idx) {
      var row = el('div', { class: 'kp-engine-output' });
      var head = el('div', { class: 'row' },
        '<h3 style="margin:0;font-size:13px;color:var(--text-dim);text-transform:uppercase;letter-spacing:.4px;">输出 ' + (idx + 1) + '</h3>' +
        '<button type="button" class="btn btn-danger btn-sm">移除</button>');
      head.querySelector('button').addEventListener('click', function () {
        var i = rows.indexOf(rowState);
        if (i >= 0) { rows.splice(i, 1); }
        row.remove();
      });
      row.appendChild(head);
      var grid = el('div', { class: 'form-grid' });
      function field(label, value, type) {
        var f = el('div', { class: 'field' });
        f.appendChild(el('label', { text: label }));
        var input = el('input', { type: type || 'text', spellcheck: 'false' });
        input.value = value == null ? '' : String(value);
        f.appendChild(input);
        grid.appendChild(f);
        return input;
      }
      var fUrl = field('URL', o.url);
      var fWidth = field('宽度', o.width, 'number');
      var fHeight = field('高度', o.height, 'number');
      var fBitrate = field('码率（kbps）', o.bitrateKbps, 'number');
      var fFps = field('帧率', o.fps, 'number');
      var fCodec = field('编码器', o.codec);
      var fHwAccel = field('硬件加速', o.hwAccel);
      var fFilters = field('滤镜', o.filters);
      var fAudioCh = field('声道数', o.audioChannels, 'number');
      var fAudioSr = field('采样率', o.audioSampleRate, 'number');
      row.appendChild(grid);
      var rowState = { row: row, url: fUrl, width: fWidth, height: fHeight, bitrate: fBitrate, fps: fFps, codec: fCodec, hwAccel: fHwAccel, filters: fFilters, audioCh: fAudioCh, audioSr: fAudioSr };
      return rowState;
    }

    var outputsBox = cfgCard.querySelector('#engOutputs');
    var rows = [];
    var defaultOut = { width: 1280, height: 720, bitrateKbps: 2500, fps: 25, codec: 'libx264' };
    var initial = Array.isArray(cfg.outputs) && cfg.outputs.length ? cfg.outputs : [defaultOut];
    initial.forEach(function (o, i) {
      var r = outputRow(o, i);
      rows.push(r);
      outputsBox.appendChild(r.row);
    });

    cfgCard.querySelector('#engAddOutput').addEventListener('click', function () {
      var r = outputRow(defaultOut, rows.length);
      rows.push(r);
      outputsBox.appendChild(r.row);
    });
    cfgCard.querySelector('#engSave').addEventListener('click', function () {
      var body = { outputs: rows.map(function (r) {
        var out = { url: r.url.value.trim() };
        var w = parseInt(r.width.value, 10);
        var h = parseInt(r.height.value, 10);
        var b = parseInt(r.bitrate.value, 10);
        var f = parseInt(r.fps.value, 10);
        if (!isNaN(w)) { out.width = w; }
        if (!isNaN(h)) { out.height = h; }
        if (!isNaN(b)) { out.bitrateKbps = b; }
        if (!isNaN(f)) { out.fps = f; }
        if (r.codec.value.trim()) { out.codec = r.codec.value.trim(); }
        if (r.hwAccel.value.trim()) { out.hwAccel = r.hwAccel.value.trim(); }
        if (r.filters.value.trim()) { out.filters = r.filters.value.trim(); }
        var ac = parseInt(r.audioCh.value, 10);
        var sr = parseInt(r.audioSr.value, 10);
        if (!isNaN(ac) && ac > 0) { out.audioChannels = ac; }
        if (!isNaN(sr) && sr > 0) { out.audioSampleRate = sr; }
        return out;
      }) };
      if (ffmpegIn.value.trim()) { body.ffmpegPath = ffmpegIn.value.trim(); }
      var ri = parseInt(cfgCard.querySelector('#engReconnect').value, 10);
      if (!isNaN(ri) && ri >= 0) { body.reconnectInterval = ri; }
      post('/engine/ffmpeg', body).then(function () {
        toast('配置已保存（尚未生效，点「立即生效」应用）', 'ok');
        renderEngine();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    cfgCard.querySelector('#engApply').addEventListener('click', function () {
      var btn = cfgCard.querySelector('#engApply');
      btn.disabled = true; btn.textContent = '生效中 ...';
      post('/engine/ffmpeg/apply', {}).then(function () {
        toast('新配置已生效', 'ok');
        renderEngine();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btn.disabled = false; btn.textContent = '立即生效'; });
    });
    wrap.appendChild(cfgCard);

    setView(wrap);
  }

  /* ------------------------------------------------------------------ *
   * Media (scan / list)
   * ------------------------------------------------------------------ */
  function renderMedia() {
    setView(loadingView());
    get('/media/list').then(function (data) {
      setConn(true);
      var media = listOf(data, ['media', 'items', 'list']);
      state.mediaCache = media;
      drawMedia(media);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderMedia));
    });
  }

  function drawMedia(media) {
    var wrap = el('div', {});

    // 用途说明
    wrap.appendChild(el('div', { class: 'card section' },
      '<h2>媒体库</h2>' +
      '<p class="muted" style="margin:0">登记推流内容：单个视频文件，或整个视频目录（目录按排序方式逐集连续播放）。' +
      '支持常见视频格式（mp4 / mkv / flv / avi / mov / ts / webm / m2ts 等，ffmpeg 可解码的均可）。' +
      '登记时可给内容配外挂音频和字幕：推流时自动合并（音频替换原音轨，字幕烧录进画面）。</p>'));

    // Scan form
    var scanCard = el('div', { class: 'card section' },
      '<h2>扫描目录</h2>' +
      '<div class="form-grid">' +
      '<div class="field"><label for="scanRoot">根路径（默认 ./video，即数据目录下的 video 文件夹）</label>' +
      '<input id="scanRoot" type="text" value="video"></div>' +
      '<div class="field"><label for="scanExt">扩展名过滤（可选，逗号分隔，如 mp4,flv）</label>' +
      '<input id="scanExt" type="text" placeholder="mp4,flv,mkv"></div>' +
      '</div>' +
      '<div class="cluster mt">' +
      '<label class="check"><input id="scanProbe" type="checkbox"> 探测元数据（ffprobe）</label>' +
      '<label class="check"><input id="scanRecursive" type="checkbox" checked> 包含子目录</label>' +
      '</div>' +
      '<div class="form-actions"><button id="scanBtn" class="btn btn-primary" type="button">扫描</button>' +
      '<button id="addBtn" class="btn" type="button">登记媒体</button></div>');

    var scanBtn = scanCard.querySelector('#scanBtn');
    scanBtn.addEventListener('click', function () {
      var root = scanCard.querySelector('#scanRoot').value.trim();
      if (!root) { toast('请输入要扫描的目录', 'err'); return; }
      scanBtn.disabled = true;
      scanBtn.textContent = '正在扫描 ...';
      var extVal = scanCard.querySelector('#scanExt').value.trim();
      var extensions = extVal ? extVal.split(/[,，\s]+/).filter(Boolean) : undefined;
      post('/media/scan', {
        root: root,
        probe: scanCard.querySelector('#scanProbe').checked,
        includeSubdirs: scanCard.querySelector('#scanRecursive').checked,
        recursive: scanCard.querySelector('#scanRecursive').checked,
        extensions: extensions
      }).then(function (res) {
        var r = objOf(res, ['data', 'result', 'scan']);
        toast('扫描完成：新增 ' + (r.added ? r.added.length : 0) + ' 个，' +
          (r.updated ? r.updated.length : 0) + ' 个更新，' + (r.skipped || 0) + ' 个跳过', 'ok');
        renderMedia();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { scanBtn.disabled = false; scanBtn.textContent = '扫描'; });
    });

    var addBtn = scanCard.querySelector('#addBtn');
    addBtn.addEventListener('click', function () { openMediaAdd(); });
    wrap.appendChild(scanCard);

    // Table
    var row = el('div', { class: 'row section' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">媒体库（' + media.length + '）</h2>');

    var table = el('div', { class: 'table-wrap section' });
    if (media.length === 0) {
      table.appendChild(emptyView('尚未登记任何媒体。请扫描一个目录以开始。',
        '<button class="btn" type="button" id="emptyScan">扫描目录</button>'));
      table.querySelector('#emptyScan').addEventListener('click', function () {
        var root = scanCard.querySelector('#scanRoot').value.trim() || '/';
        post('/media/scan', { root: root, probe: scanCard.querySelector('#scanProbe').checked, includeSubdirs: true, recursive: true })
          .then(function () { renderMedia(); }).catch(function (e) { toast(e.message, 'err'); });
      });
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr>' +
        '<th>名称</th><th>类型</th><th>大小</th><th>时长</th><th>分辨率</th><th>修改时间</th><th></th>' +
        '</tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      media.forEach(function (m) {
        var tr = el('tr', {});
        var delBtn = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        delBtn.addEventListener('click', function () {
          if (!confirm('确定从媒体库移除媒体 "' + (m.name || m.path) + '" 吗？')) { return; }
          del('/media/remove/' + encodeURIComponent(m.id)).then(function () {
            toast('媒体已移除', 'ok'); renderMedia();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        var editBtn = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        editBtn.addEventListener('click', function () { openMediaAdd(m); });
        var typeText = m.isDir ? '📁 目录' : ((m.ext || '').replace('.', '').toUpperCase() || '-');
        if (m.isDir) {
          var sortNames = { name: '按文件名', time: '按修改时间', random: '随机' };
          typeText += '（' + (sortNames[m.sortBy] || '按文件名') + '）';
        }
        if (m.audioPath) { typeText += ' +音频'; }
        if (m.subtitlePath) { typeText += ' +字幕'; }
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(m.name || '-') + '</span><span class="sub">' + esc(m.id) + '</span></div>'));
        tr.appendChild(el('td', { text: typeText }));
        tr.appendChild(el('td', { text: m.size != null ? fmtBytes(m.size) : '-' }));
        tr.appendChild(el('td', { text: m.duration != null ? fmtDur(m.duration) : (m.probed ? '已探测' : '-') }));
        tr.appendChild(el('td', { text: (m.width && m.height) ? m.width + 'x' + m.height : '-' }));
        tr.appendChild(el('td', { text: fmtAgo(m.modTime || m.modifiedAt) }));
        tr.appendChild(el('td', {}, '<div class="icon-btn-row"></div>'));
        tr.lastChild.firstChild.appendChild(editBtn);
        tr.lastChild.firstChild.appendChild(delBtn);
        tb.appendChild(tr);
      });
      table.appendChild(t);
    }
    row.appendChild(table);
    wrap.appendChild(row);
    setView(wrap);
  }

  function openMediaAdd(m) {
    var editing = !!m;
    var modal = modalScrim(el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑媒体' : '登记媒体') + '</h3>' +
      '<div class="field"><label>路径（单个视频文件，或一个视频目录；目录会按下方排序方式连续播放）</label>' +
      '<input id="mPath" type="text" value="' + esc(editing ? m.path : '') + '"' + (editing ? ' disabled' : '') + '></div>' +
      '<div class="field"><label>名称（可选）</label><input id="mName" type="text" value="' + esc(editing ? (m.name || '') : '') + '"></div>' +
      '<div class="field"><label>目录播放排序（仅目录有效）</label>' +
      '<select id="mSortBy">' +
      '<option value="name">按文件名排序</option>' +
      '<option value="time">按修改时间排序</option>' +
      '<option value="random">随机顺序</option>' +
      '</select></div>' +
      '<div class="field"><label>外挂音频文件（可选，推流时替换原音轨，如 .mp3/.aac/.m4a）</label>' +
      '<input id="mAudio" type="text" value="' + esc(editing ? (m.audioPath || '') : '') + '" placeholder="/data/media/audio.mp3"></div>' +
      '<div class="field"><label>外挂字幕文件（可选，烧录到画面，如 .srt/.ass）</label>' +
      '<input id="mSub" type="text" value="' + esc(editing ? (m.subtitlePath || '') : '') + '" placeholder="/data/media/sub.srt"></div>' +
      '<p class="muted">支持常见视频格式（mp4/mkv/flv/avi/mov/ts/webm 等，ffmpeg 可解码的均可）。' +
      '视频+音频+字幕会自动合并成一路推流：外挂音频替换原音，字幕烧录进画面（RTMP 不支持字幕轨）。</p>' +
      '<div class="form-actions"><button id="mCancel" class="btn" type="button">取消</button>' +
      '<button id="mSave" class="btn btn-primary" type="button">' + (editing ? '保存' : '添加') + '</button></div>'));
    var sortSel = modal.querySelector('#mSortBy');
    if (editing) { sortSel.value = m.sortBy || 'name'; }
    modal.querySelector('#mCancel').addEventListener('click', function () { modal.remove(); });
    modal.querySelector('#mSave').addEventListener('click', function () {
      var path = modal.querySelector('#mPath').value.trim();
      if (!path) { toast('必须填写路径', 'err'); return; }
      var body = {
        path: path,
        name: modal.querySelector('#mName').value.trim() || undefined,
        sortBy: sortSel.value,
        audioPath: modal.querySelector('#mAudio').value.trim() || undefined,
        subtitlePath: modal.querySelector('#mSub').value.trim() || undefined
      };
      var p = editing ? post('/media/' + encodeURIComponent(m.id) + '/update', body) : post('/media/add', body);
      p.then(function () { toast(editing ? '媒体已更新' : '媒体已添加', 'ok'); modal.remove(); renderMedia(); })
        .catch(function (e) { toast(e.message, 'err'); });
    });
    document.body.appendChild(modal);
  }

  /* ------------------------------------------------------------------ *
   * Playlist (program schedule)
   * ------------------------------------------------------------------ */
  function renderPlaylist() {
    setView(loadingView());
    Promise.all([
      get('/playlist/list'),
      get('/media/list')
    ]).then(function (res) {
      setConn(true);
      var playlists = listOf(res[0], ['playlists', 'items', 'list']);
      var media = listOf(res[1], ['media', 'items', 'list']);
      state.mediaCache = media;
      state.playlistCache = playlists;
      drawPlaylist(playlists, media);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderPlaylist));
    });
  }

  function modeName(mode) {
    return { order: '顺序一遍', loop: '顺序循环', random: '随机一遍', 'random-loop': '随机循环' }[mode] || '顺序一遍';
  }

  function drawPlaylist(playlists, media) {
    var mediaById = {};
    media.forEach(function (m) { mediaById[m.id] = m; });
    var plById = {};
    playlists.forEach(function (pl) { plById[pl.id] = pl; });

    var wrap = el('div', {});
    var row = el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">节目单（' + playlists.length + '）</h2>' +
      '<button id="newPl" class="btn btn-primary" type="button">新建节目单</button>');
    wrap.appendChild(row);

    if (playlists.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无节目单。', undefined).innerHTML));
    } else {
      var stack = el('div', { class: 'list-stack' });
      playlists.forEach(function (p) {
        var item = el('div', { class: 'list-item' },
          '<div class="list-item-head"><span class="list-item-title">' + esc(p.name) + '</span>' +
          '<div class="icon-btn-row"></div></div>' +
          '<div class="list-item-meta"><span>' + (p.items ? p.items.length : 0) + ' 条</span>' +
          '<span class="badge badge-info">' + modeName(p.mode || (p.loop ? 'loop' : 'order')) + '</span>' +
          (p.fallbackPlaylistId ? '<span class="badge badge-info">回退: ' + esc(plById[p.fallbackPlaylistId] ? plById[p.fallbackPlaylistId].name : p.fallbackPlaylistId) + '</span>' : '') +
          '<span class="muted">' + fmtAgo(p.updatedAt || p.updated_at) + '</span></div>');

        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEdit.addEventListener('click', function () { openPlaylistEdit(p, media, mediaById, playlists); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除节目单 "' + p.name + '" 吗？')) { return; }
          del('/playlist/remove/' + encodeURIComponent(p.id)).then(function () {
            toast('节目单已删除', 'ok'); renderPlaylist();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        item.querySelector('.icon-btn-row').appendChild(btnEdit);
        item.querySelector('.icon-btn-row').appendChild(btnDel);
        stack.appendChild(item);
      });
      wrap.appendChild(stack);
    }
    wrap.querySelector('#newPl').addEventListener('click', function () { openPlaylistEdit(null, media, mediaById, playlists); });
    setView(wrap);
  }

  function openPlaylistEdit(p, media, mediaById, playlists) {
    var editing = !!p;
    var items = editing ? (p.items || []).map(function (it) { return it.mediaId || it.media_id; }) : [];

    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑节目单' : '新建节目单') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="plName" type="text" value="' + esc(editing ? p.name : '') + '"></div>' +
      '<div class="field"><label>描述</label><input id="plDesc" type="text" value="' + esc(editing ? (p.desc || '') : '') + '"></div>' +
      '</div>' +
      '<div class="field mt"><label>回退节目单</label><select id="plFallback"></select></div>' +
      '<div class="field mt"><label>播放方式</label>' +
      '<select id="plMode">' +
      '<option value="order">顺序播放一遍</option>' +
      '<option value="loop">顺序循环播放</option>' +
      '<option value="random">随机播放一遍</option>' +
      '<option value="random-loop">随机循环播放</option>' +
      '</select>' +
      '<p class="muted" style="margin:2px 0 0">目录条目会按目录的排序方式展开成多集连续播放。</p></div>' +
      '<div class="field mt"><label>添加条目：输入视频文件或目录的路径（目录内按文件名排序逐集播放）</label>' +
      '<input id="plItemPath" type="text" placeholder="/data/video/xxx.mp4 或 /data/video/目录" spellcheck="false">' +
      '<p class="muted" style="margin:2px 0 0">支持常见格式（mp4/mkv/flv/avi/mov/ts/webm 等）；目录会展开为其中的全部视频文件。</p></div>' +
      '<div class="form-grid">' +
      '<div class="field"><label>外挂音频（可选，与视频同步合并，替换原音轨）</label>' +
      '<input id="plItemAudio" type="text" placeholder="/data/audio.mp3" spellcheck="false"></div>' +
      '<div class="field"><label>字幕文件（可选，烧录进画面，与视频同步）</label>' +
      '<input id="plItemSub" type="text" placeholder="/data/sub.srt" spellcheck="false"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="plAddItem" class="btn" type="button">+ 添加条目</button>' +
      '<button id="plClear" class="btn" type="button">清空</button></div>' +
      '<p class="muted" style="margin-top:6px">同步方式：视频+外挂音频+字幕由 ffmpeg 按时间戳对齐合并成一路推流；音频/字幕与画面不同步时，可在「效果与插件」页设置音频偏移微调。</p>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>#</th><th>名称</th><th>路径</th><th></th></tr></thead><tbody id="plItems"></tbody></table></div>' +
      '<div class="form-actions"><button id="plCancel" class="btn" type="button">取消</button>' +
      '<button id="plSave" class="btn btn-primary" type="button">保存</button></div>');

    var fbSel = m.querySelector('#plFallback');
    var fallbackId = editing ? (p.fallbackPlaylistId || p.fallback_playlist_id || '') : '';
    var foundFb = false;
    fbSel.appendChild(el('option', { value: '', text: '-- 无 --' }));
    playlists.forEach(function (pl) {
      if (editing && pl.id === p.id) { return; }
      if (pl.id == fallbackId) { foundFb = true; }
      fbSel.appendChild(el('option', { value: pl.id, text: pl.name }));
    });
    if (fallbackId && !foundFb) {
      fbSel.appendChild(el('option', { value: fallbackId, text: '（缺失节目单: ' + fallbackId + '）' }));
    }
    fbSel.value = fallbackId;

    var modeSel = m.querySelector('#plMode');
    if (editing) {
      modeSel.value = p.mode || (p.loop ? 'loop' : 'order');
    }

    var tbody = m.querySelector('#plItems');
    function drawItems() {
      tbody.innerHTML = '';
      items.forEach(function (id, idx) {
        var info = mediaById[id] || extra[id] || {};
        var tr = el('tr', {});
        var up = el('button', { class: 'btn btn-sm', type: 'button', text: '^' });
        var down = el('button', { class: 'btn btn-sm', type: 'button', text: 'v' });
        var del = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: 'x' });
        up.addEventListener('click', function () { move(items, idx, idx - 1); drawItems(); });
        down.addEventListener('click', function () { move(items, idx, idx + 1); drawItems(); });
        del.addEventListener('click', function () { items.splice(idx, 1); drawItems(); });
        tr.appendChild(el('td', { text: String(idx) }));
        tr.appendChild(el('td', { text: (info.isDir ? '📁 [目录] ' : '') + (info.name || id) }));
        tr.appendChild(el('td', {}, '<div class="cell"><span class="mono">' + esc(info.path || '-') + '</span></div>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(up);
        ops.firstChild.appendChild(down);
        ops.firstChild.appendChild(del);
        tr.appendChild(ops);
        tbody.appendChild(tr);
      });
    }
    function move(arr, from, to) {
      if (from < 0 || from >= arr.length || to < 0 || to >= arr.length) { return; }
      var v = arr.splice(from, 1)[0];
      arr.splice(to, 0, v);
    }
    drawItems();

    var extra = {};
    m.querySelector('#plAddItem').addEventListener('click', function () {
      var path = m.querySelector('#plItemPath').value.trim();
      if (!path) { toast('请输入文件或目录路径', 'err'); return; }
      var btn = m.querySelector('#plAddItem');
      btn.disabled = true; btn.textContent = '登记中 ...';
      var audio = m.querySelector('#plItemAudio').value.trim() || undefined;
      var sub = m.querySelector('#plItemSub').value.trim() || undefined;
      post('/media/add', { path: path, audioPath: audio, subtitlePath: sub }).then(function (item) {
        var id = item && (item.id || item.mediaId);
        if (!id) { throw new Error('登记失败'); }
        extra[id] = item;
        if (items.indexOf(id) === -1) { items.push(id); drawItems(); }
        m.querySelector('#plItemPath').value = '';
        m.querySelector('#plItemAudio').value = '';
        m.querySelector('#plItemSub').value = '';
        toast('已添加: ' + (item.name || path) + (audio || sub ? '（含音/字幕）' : ''), 'ok');
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btn.disabled = false; btn.textContent = '+ 添加条目'; });
    });
    m.querySelector('#plClear').addEventListener('click', function () { items = []; drawItems(); });
    m.querySelector('#plCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#plSave').addEventListener('click', function () {
      var name = m.querySelector('#plName').value.trim();
      if (!name) { toast('必须填写节目单名称', 'err'); return; }
      var body = {
        name: name,
        desc: m.querySelector('#plDesc').value.trim(),
        items: items,
        mode: modeSel.value,
        fallbackPlaylistId: fbSel.value || undefined
      };
      if (editing) {
        post('/playlist/update', Object.assign({ id: p.id }, body)).then(function () {
          toast('节目单已更新', 'ok'); m.remove(); renderPlaylist();
        }).catch(function (e) { toast(e.message, 'err'); });
      } else {
        post('/playlist/add', body).then(function () {
          toast('节目单已创建', 'ok'); m.remove(); renderPlaylist();
        }).catch(function (e) { toast(e.message, 'err'); });
      }
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Effects & plugins (多插件：文字/图片水印、字幕、音频；对应官方 plugin.lists)
   * ------------------------------------------------------------------ */
  var EFFECT_META = {
    'text-watermark': { label: '文字水印', desc: '在画面上叠加文字（对应官方 show-text / watermark 插件）' },
    'image-watermark': { label: '图片水印', desc: '叠加图片 Logo（对应官方 show-picture / watermark 插件）' },
    'subtitle': { label: '字幕', desc: '烧录字幕文件到画面，可调样式（对应官方 subtitle 插件）' },
    'audio': { label: '音频', desc: '音量与音画同步偏移（对应官方 audio-resample 插件）' },
    'marquee': { label: '跑马灯', desc: '滚动字幕：文字从右向左循环滚动（直播常用）' },
    'video-adjust': { label: '画面调整', desc: '亮度/对比度/饱和度调节' },
    'transcode-preset': { label: '转码预设', desc: '一键切换输出档位（1080p/720p/480p/360p）' }
  };

  function renderEffects() {
    setView(loadingView());
    get('/effects').then(function (data) {
      setConn(true);
      var effects = Array.isArray(data.effects) ? data.effects : [];
      drawEffects(effects, data.vf || '', data.af || '');
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderEffects));
    });
  }

  function effectParamsHTML(e) {
    var p = e.params || {};
    switch (e.type) {
      case 'text-watermark':
        return '<div class="form-grid">' +
          '<div class="field"><label>文字</label><input class="ep-text" value="' + esc(p.text || '') + '" placeholder="例如：KPlayer 直播"></div>' +
          '<div class="field"><label>位置</label><select class="ep-position">' +
          '<option value="tl">左上</option><option value="tr">右上</option><option value="bl">左下</option><option value="br">右下</option><option value="c">居中</option></select></div>' +
          '<div class="field"><label>字号</label><input class="ep-font_size" type="number" min="8" max="200" value="' + esc(p.font_size || '28') + '"></div>' +
          '<div class="field"><label>颜色（white/red/#RRGGBB）</label><input class="ep-color" value="' + esc(p.color || 'white') + '"></div>' +
          '<div class="field"><label>透明度（0-1）</label><input class="ep-opacity" type="number" min="0" max="1" step="0.1" value="' + esc(p.opacity || '1') + '"></div>' +
          '</div>';
      case 'image-watermark':
        return '<div class="form-grid">' +
          '<div class="field"><label>图片路径</label><input class="ep-path" value="' + esc(p.path || '') + '" placeholder="/data/logo.png"></div>' +
          '<div class="field"><label>位置</label><select class="ep-position">' +
          '<option value="tl">左上</option><option value="tr">右上</option><option value="bl">左下</option><option value="br">右下</option><option value="c">居中</option></select></div>' +
          '</div>';
      case 'subtitle':
        return '<div class="form-grid">' +
          '<div class="field"><label>字幕文件</label><input class="ep-path" value="' + esc(p.path || '') + '" placeholder="/data/sub.srt"></div>' +
          '<div class="field"><label>字号</label><input class="ep-font_size" type="number" min="8" max="100" value="' + esc(p.font_size || '18') + '"></div>' +
          '<div class="field"><label>颜色（#RRGGBB）</label><input class="ep-color" value="' + esc(p.color || '#FFFFFF') + '"></div>' +
          '<div class="field"><label>对齐</label><select class="ep-alignment">' +
          '<option value="1">底部左</option><option value="2">底部居中</option><option value="3">底部右</option><option value="5">顶部居中</option></select></div>' +
          '</div>';
      case 'audio':
        return '<div class="form-grid">' +
          '<div class="field"><label>音量（%）</label><input class="ep-volume" type="number" min="0" max="300" value="' + esc(p.volume || '100') + '"></div>' +
          '<div class="field"><label>音频偏移（毫秒，正=延后、负=提前；音画不同步时微调）</label><input class="ep-delay_ms" type="number" min="-5000" max="5000" value="' + esc(p.delay_ms || '0') + '"></div>' +
          '</div>';
      case 'marquee':
        return '<div class="form-grid">' +
          '<div class="field"><label>文字</label><input class="ep-text" value="' + esc(p.text || '') + '" placeholder="例如：欢迎收看直播"></div>' +
          '<div class="field"><label>位置</label><select class="ep-position">' +
          '<option value="top">顶部</option><option value="middle">中部</option><option value="bottom">底部</option></select></div>' +
          '<div class="field"><label>字号</label><input class="ep-font_size" type="number" min="8" max="200" value="' + esc(p.font_size || '24') + '"></div>' +
          '<div class="field"><label>滚动速度（像素/秒）</label><input class="ep-speed" type="number" min="10" max="500" value="' + esc(p.speed || '60') + '"></div>' +
          '<div class="field"><label>颜色</label><input class="ep-color" value="' + esc(p.color || 'white') + '"></div>' +
          '<div class="field"><label>透明度（0-1）</label><input class="ep-opacity" type="number" min="0" max="1" step="0.1" value="' + esc(p.opacity || '1') + '"></div>' +
          '</div>';
      case 'video-adjust':
        return '<div class="form-grid">' +
          '<div class="field"><label>亮度（-1 ~ 1，0=不变）</label><input class="ep-brightness" type="number" step="0.05" value="' + esc(p.brightness || '0') + '"></div>' +
          '<div class="field"><label>对比度（0 ~ 2，1=不变）</label><input class="ep-contrast" type="number" step="0.05" value="' + esc(p.contrast || '1') + '"></div>' +
          '<div class="field"><label>饱和度（0 ~ 3，1=不变）</label><input class="ep-saturation" type="number" step="0.05" value="' + esc(p.saturation || '1') + '"></div>' +
          '</div>';
      case 'transcode-preset':
        return '<div class="form-grid">' +
          '<div class="field"><label>档位（应用到推流输出）</label><select class="ep-preset">' +
          '<option value="1080p">1080p 超清（1920x1080 / 4000kbps）</option>' +
          '<option value="720p">720p 高清（1280x720 / 2500kbps）</option>' +
          '<option value="480p">480p 标清（854x480 / 1200kbps）</option>' +
          '<option value="360p">360p 流畅（640x360 / 800kbps）</option></select></div>' +
          '</div>';
    }
    return '';
  }

  function effectParamRead(e, root) {
    var p = e.params || {};
    var read = function (cls, key) {
      var el0 = root.querySelector('.' + cls);
      if (el0) { p[key] = el0.value.trim(); }
    };
    switch (e.type) {
      case 'text-watermark':
        read('ep-text', 'text'); read('ep-position', 'position'); read('ep-font_size', 'font_size');
        read('ep-color', 'color'); read('ep-opacity', 'opacity');
        break;
      case 'image-watermark':
        read('ep-path', 'path'); read('ep-position', 'position');
        break;
      case 'subtitle':
        read('ep-path', 'path'); read('ep-font_size', 'font_size'); read('ep-color', 'color'); read('ep-alignment', 'alignment');
        break;
      case 'audio':
        read('ep-volume', 'volume'); read('ep-delay_ms', 'delay_ms');
        break;
      case 'marquee':
        read('ep-text', 'text'); read('ep-position', 'position'); read('ep-font_size', 'font_size');
        read('ep-speed', 'speed'); read('ep-color', 'color'); read('ep-opacity', 'opacity');
        break;
      case 'video-adjust':
        read('ep-brightness', 'brightness'); read('ep-contrast', 'contrast'); read('ep-saturation', 'saturation');
        break;
      case 'transcode-preset':
        read('ep-preset', 'preset');
        break;
    }
    e.params = p;
    return e;
  }

  function drawEffects(effects, vf, af) {
    var wrap = el('div', {});

    var head = el('div', { class: 'card section' },
      '<h2>效果与插件（支持多个，按顺序叠加）</h2>' +
      '<p class="muted" style="margin:0 0 10px">对应官方 KPlayer 的 plugin.lists：可添加<b>多个</b>插件（文字水印、图片水印、字幕、音频），按列表顺序叠加到输出画面。保存后自动应用到当前推流输出（下次推流生效）。</p>' +
      '<div class="form-actions"><button id="efAdd" class="btn btn-primary" type="button">+ 添加插件</button>' +
      '<button id="efSave" class="btn" type="button">保存并应用</button></div>');
    var listHost = el('div', { id: 'efList' });
    head.appendChild(listHost);

    function addRow(e, idx) {
      var row = el('div', { class: 'list-item' },
        '<div class="list-item-head"><span class="list-item-title">' + esc((EFFECT_META[e.type] || {}).label || e.type) +
        (e.name ? ' · ' + esc(e.name) : '') +
        '</span><div class="icon-btn-row"></div></div>' +
        '<div class="list-item-meta"><span class="badge ' + (e.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (e.enabled ? '启用' : '停用') + '</span>' +
        '<span class="muted">' + esc(((EFFECT_META[e.type] || {}).desc) || '') + '</span></div>' +
        '<div class="field"><label>名称（可选）</label><input class="ep-name" value="' + esc(e.name || '') + '"></div>' +
        '<div class="field"><label>类型（更改后请重新填写参数）</label><select class="ep-type">' +
        Object.keys(EFFECT_META).map(function (t) { return '<option value="' + t + '">' + EFFECT_META[t].label + '</option>'; }).join('') +
        '</select></div>' +
        '<div class="ef-params"></div>');
      var typeSel = row.querySelector('.ep-type');
      typeSel.value = e.type;
      var paramsHost = row.querySelector('.ef-params');
      function renderParams() {
        e.type = typeSel.value;
        paramsHost.innerHTML = effectParamsHTML(e);
        var posSel = paramsHost.querySelector('.ep-position');
        if (posSel && e.params && e.params.position) { posSel.value = e.params.position; }
        var alSel = paramsHost.querySelector('.ep-alignment');
        if (alSel && e.params && e.params.alignment) { alSel.value = e.params.alignment; }
      }
      typeSel.addEventListener('change', function () { e.params = {}; renderParams(); });
      renderParams();
      row.querySelector('.ep-name').addEventListener('input', function () { e.name = this.value; });

      var ops = row.querySelector('.icon-btn-row');
      var btnUp = el('button', { class: 'btn btn-sm', type: 'button', text: '↑' });
      var btnDown = el('button', { class: 'btn btn-sm', type: 'button', text: '↓' });
      var btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: e.enabled ? '停用' : '启用' });
      var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
      btnUp.addEventListener('click', function () {
        var i = effects.indexOf(e);
        if (i > 0) { effects.splice(i, 1); effects.splice(i - 1, 0, e); renderList(); }
      });
      btnDown.addEventListener('click', function () {
        var i = effects.indexOf(e);
        if (i >= 0 && i < effects.length - 1) { effects.splice(i, 1); effects.splice(i + 1, 0, e); renderList(); }
      });
      btnToggle.addEventListener('click', function () { e.enabled = !e.enabled; renderList(); });
      btnDel.addEventListener('click', function () {
        var i = effects.indexOf(e);
        if (i >= 0) { effects.splice(i, 1); renderList(); }
      });
      ops.appendChild(btnUp);
      ops.appendChild(btnDown);
      ops.appendChild(btnToggle);
      ops.appendChild(btnDel);
      return row;
    }

    function renderList() {
      listHost.innerHTML = '';
      effects.forEach(function (e, i) {
        listHost.appendChild(addRow(e, i));
      });
      if (!effects.length) {
        listHost.appendChild(el('div', { class: 'muted' }, '还没有插件。点「+ 添加插件」开始。'));
      }
    }
    renderList();

    head.querySelector('#efAdd').addEventListener('click', function () {
      effects.push({ id: '', type: 'text-watermark', name: '', enabled: true, params: { text: '', position: 'tl', font_size: '28', color: 'white', opacity: '1' } });
      renderList();
    });
    head.querySelector('#efSave').addEventListener('click', function () {
      var btn = head.querySelector('#efSave');
      btn.disabled = true; btn.textContent = '保存中 ...';
      // 收集每行参数
      var rows = Array.prototype.slice.call(listHost.querySelectorAll('.list-item'));
      var out = [];
      rows.forEach(function (rowEl, i) {
        var e = effects[i];
        effectParamRead(e, rowEl);
        out.push({ id: e.id, type: e.type, name: rowEl.querySelector('.ep-name').value.trim() || e.type, enabled: e.enabled, params: e.params || {} });
      });
      post('/effects', { effects: out }).then(function () {
        toast('已保存并应用到推流输出', 'ok');
        setTimeout(renderEffects, 900);
      }).catch(function (e2) { toast(e2.message, 'err'); })
        .finally(function () { btn.disabled = false; btn.textContent = '保存并应用'; });
    });
    wrap.appendChild(head);

    // 当前渲染预览 + 官方对照
    var preview = el('div', { class: 'card section' },
      '<h2>当前效果（已应用）</h2>' +
      '<div class="kv">' +
      '<dt>视频滤镜 (-vf)</dt><dd class="mono">' + esc(vf || '（无）') + '</dd>' +
      '<dt>音频滤镜 (-af)</dt><dd class="mono">' + esc(af || '（无）') + '</dd>' +
      '</div>' +
      '<p class="muted mt">修改后需点「保存并应用」才会生效；任务线路（多路推流）各自独立，可在其编辑表单的滤镜字段单独配置。</p>');
    wrap.appendChild(preview);

    // 支持插件总览
    var supported = el('div', { class: 'card section' },
      '<h2>已支持的插件类型</h2>' +
      '<table class="data"><thead><tr><th>插件</th><th>对应官方插件</th><th>参数</th><th>实现</th></tr></thead><tbody>' +
      '<tr><td><b>文字水印</b></td><td>show-text / watermark</td><td>文字、位置（四角/居中）、字号、颜色、透明度</td><td>ffmpeg drawtext 滤镜</td></tr>' +
      '<tr><td><b>图片水印</b></td><td>show-picture / watermark</td><td>图片路径、位置（四角/居中）</td><td>ffmpeg overlay 滤镜</td></tr>' +
      '<tr><td><b>字幕</b></td><td>kplayer-plugin-subtitle</td><td>字幕文件、字号、颜色、对齐（烧录进画面）</td><td>ffmpeg subtitles（libass）</td></tr>' +
      '<tr><td><b>音频</b></td><td>kplayer-plugin-audio-resample</td><td>音量（%）、音频偏移（毫秒，音画同步微调）</td><td>ffmpeg volume / adelay</td></tr>' +
      '<tr><td><b>跑马灯</b></td><td>show-text（滚动）</td><td>文字、位置（顶/中/底）、字号、速度、颜色、透明度</td><td>ffmpeg drawtext 时间表达式</td></tr>' +
      '<tr><td><b>画面调整</b></td><td>watermark 系列</td><td>亮度、对比度、饱和度</td><td>ffmpeg eq 滤镜</td></tr>' +
      '<tr><td><b>转码预设</b></td><td>kplayer-plugin-transcode</td><td>1080p / 720p / 480p / 360p 一键档位</td><td>输出编码参数</td></tr>' +
      '<tr><td><b>远程内容</b></td><td>kplayer-cache-http</td><td>http(s) 视频地址可直接作为推流内容（ffmpeg 拉流）</td><td>引擎远程源</td></tr>' +
      '<tr><td><b>转码（预设）</b></td><td>kplayer-plugin-transcode</td><td>每条线路的编码参数：分辨率/码率/帧率/编码器/采样率</td><td>线路参数（多路推流）</td></tr>' +
      '<tr><td><b>自动告警</b></td><td>plugin-auto-alarm</td><td>调度失败、引擎异常退出自动告警</td><td>后端内置</td></tr>' +
      '<tr><td><b>间隔任务</b></td><td>plugin-interval-task</td><td>定时开播/关播（cron）</td><td>定时任务页</td></tr>' +
      '</tbody></table>' +
      '<p class="muted mt">说明：官方 WASM 自定义插件需在官方核心内运行，本部署以 ffmpeg 原生等效实现所有效果类插件；去重/移动文件/HTTP 缓存等素材文件管理类插件与推流无直接关系，未启用。</p>');
    wrap.appendChild(supported);

    var guide = el('div', { class: 'card section' },
      '<h2>官方配置对照</h2>' +
      '<table class="data"><thead><tr><th>官方模块</th><th>本控制台位置 / 说明</th></tr></thead><tbody>' +
      '<tr><td>plugin.lists（可多个插件）</td><td>本页：多个效果按顺序叠加，保存后应用到主推流输出</td></tr>' +
      '<tr><td>output.lists（多路输出）</td><td>多路推流 → 任务线路（一个任务多条线路）</td></tr>' +
      '<tr><td>resource.lists / extensions</td><td>节目单 → 条目（路径自动登记；目录展开）</td></tr>' +
      '<tr><td>play.play_model（list/order/random）</td><td>节目单 → 播放方式（顺序/循环/随机/随机循环）</td></tr>' +
      '<tr><td>play.encode（分辨率/码率/帧率/采样率）</td><td>多路推流 → 线路参数；快速推流默认 1280x720/2500k/25fps</td></tr>' +
      '<tr><td>output.reconnect_internal</td><td>断线自动重推（默认 5 秒，任务表单可调）</td></tr>' +
      '<tr><td>plugin 间隔任务 / 自动告警 / Webhook</td><td>定时任务（开播/关播）；告警与事件通知后端内置</td></tr>' +
      '<tr><td>cache（自动缓存/预合成）</td><td>推流时实时合成（视频+音频+字幕按时间戳对齐），无需预缓存</td></tr>' +
      '</tbody></table>');
    wrap.appendChild(guide);

    setView(wrap);
  }

  /* ------------------------------------------------------------------ *
   * Tasks
   * ------------------------------------------------------------------ */
  function renderTasks() {
    setView(loadingView());
    Promise.all([get('/task/list'), get('/playlist/list'), get('/media/list'), get('/scene-template')]).then(function (res) {
      setConn(true);
      var tasks = listOf(res[0], ['tasks', 'items', 'list']);
      var playlists = listOf(res[1], ['playlists', 'items', 'list']);
      var media = listOf(res[2], ['media', 'items', 'list']);
      var templates = listOf(res[3], ['sceneTemplates', 'items', 'list']);
      drawTasks(tasks, playlists, media, templates);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderTasks));
    });
  }

  function drawTasks(tasks, playlists, media, templates) {
    var plById = {};
    playlists.forEach(function (p) { plById[p.id] = p; });
    var wrap = el('div', {});
    // 用途说明
    wrap.appendChild(el('div', { class: 'card section' },
      '<h2>定时任务</h2>' +
      '<p class="muted" style="margin:0">定时任务 = <b>到点自动执行</b>一个动作，适合无人值守直播：</p>' +
      '<ul class="muted" style="margin:8px 0 0;padding-left:18px">' +
      '<li><b>开播</b>：到点自动播放所选节目单（从头播放最新内容；正在推流则直接切换）——例：每天 8:00 开播播放「早间节目」</li>' +
      '<li><b>关播</b>：到点自动停止推流——例：每天 23:00 关播</li>' +
      '<li>节目单内容随时更新，到点播放的就是最新内容；新建后可点「立即执行」验证效果</li>' +
      '</ul>'));
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">定时任务 (' + tasks.length + ')</h2>' +
      '<button id="newTask" class="btn btn-primary" type="button">新建任务</button>'));

    if (tasks.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无定时任务。新建一个：选动作（开播/关播）+ 时间，到点自动执行。', undefined).innerHTML));
    } else {
      var stack = el('div', { class: 'list-stack' });
      tasks.forEach(function (t) {
        var isStop = t.action === 'stop';
        var actionText = isStop ? '关播（停止推流）' : '开播（播放节目单）';
        var targetName = isStop ? '-' : esc((plById[t.playlistId || t.playlist_id] && plById[t.playlistId || t.playlist_id].name) || (t.playlistId || t.playlist_id) || '-');
        var cron = t.cron || '';
        var schedule = cron ? cron : (t.interval != null ? '每 ' + t.interval + ' 秒' : '-');
        var item = el('div', { class: 'list-item' },
          '<div class="list-item-head"><span class="list-item-title">' + esc(t.name) +
          '</span><div class="icon-btn-row"></div></div>' +
          '<div class="list-item-meta">' +
          '<span class="badge ' + (isStop ? 'badge-warn' : 'badge-ok') + '">' + actionText + '</span>' +
          (isStop ? '' : '<span class="muted">节目单: ' + targetName + '</span>') +
          '<span class="muted">时间: ' + esc(schedule) + '</span>' +
          '<span class="muted">上次执行: ' + fmtAgo(t.lastRun || t.last_run) + '</span>' +
          '<span class="badge ' + (t.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (t.enabled ? '启用' : '停用') + '</span></div>');

        var btnRun = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '立即执行' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: t.enabled ? '停用' : '启用' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnRun.addEventListener('click', function () {
          btnRun.disabled = true; btnRun.textContent = '执行中 ...';
          post('/task/' + t.id + '/run', {}).then(function () {
            toast(isStop ? '已停止推流' : '已开始播放节目单', 'ok');
            setTimeout(renderTasks, 1200);
          }).catch(function (e) { toast(e.message, 'err'); })
            .finally(function () { btnRun.disabled = false; btnRun.textContent = '立即执行'; });
        });
        btnEdit.addEventListener('click', function () { openTaskEdit(t, playlists); });
        btnToggle.addEventListener('click', function () {
          post('/task/enabled', { id: t.id, enabled: !t.enabled }).then(function () {
            toast('任务已更新', 'ok'); renderTasks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除任务 "' + t.name + '"?')) { return; }
          post('/task/remove', { id: t.id }).then(function () {
            toast('任务已删除', 'ok'); renderTasks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        item.querySelector('.icon-btn-row').appendChild(btnRun);
        item.querySelector('.icon-btn-row').appendChild(btnEdit);
        item.querySelector('.icon-btn-row').appendChild(btnToggle);
        item.querySelector('.icon-btn-row').appendChild(btnDel);
        stack.appendChild(item);
      });
      wrap.appendChild(stack);
    }

    wrap.querySelector('#newTask').addEventListener('click', function () { openTaskEdit(null, playlists); });
    setView(wrap);
  }

  function openTaskEdit(t, playlists) {
    var editing = !!t;
    var isStop = editing && t.action === 'stop';
    var cron = editing ? (t.cron || '') : '';
    var timeMode = 'daily';
    if (editing && cron) {
      var parts = cron.split(/\s+/);
      var looksDaily = parts.length >= 5 && !isNaN(parseInt(parts[0], 10)) && parts[2] === '*' && parts[3] === '*' && parts[4] === '*';
      timeMode = looksDaily ? 'daily' : 'cron';
    }
    var dailyHour = 8, dailyMin = 0;
    if (timeMode === 'daily' && cron) {
      var dp = cron.split(/\s+/);
      dailyMin = parseInt(dp[0], 10) || 0;
      dailyHour = parseInt(dp[1], 10) || 0;
    }
    if (editing && isStop && !cron) { dailyHour = 23; dailyMin = 0; }

    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑定时任务' : '新建定时任务') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="tName" type="text" value="' + esc(editing ? t.name : '') + '"></div>' +
      '<div class="field"><label>动作</label><select id="tAction">' +
      '<option value="play">开播（播放节目单）</option>' +
      '<option value="stop">关播（停止推流）</option>' +
      '</select></div>' +
      '<div class="field" id="tPlField"><label>节目单（到点播放最新内容）</label><select id="tPlaylist" class="mono"></select></div>' +
      '<div class="field"><label>时间模式</label><select id="tTimeMode">' +
      '<option value="daily">每天固定时间</option>' +
      '<option value="cron">自定义 cron 表达式</option>' +
      '</select></div>' +
      '<div class="field" id="tDailyField">' +
      '<label>每天时间</label><div class="cluster" style="gap:8px">' +
      '<input id="tHour" type="number" min="0" max="23" style="max-width:80px" value="' + dailyHour + '"> <span>时</span>' +
      '<input id="tMin" type="number" min="0" max="59" style="max-width:80px" value="' + dailyMin + '"> <span>分</span>' +
      '</div></div>' +
      '<div class="field" id="tCronField" style="display:none"><label>cron 表达式（5 段）</label>' +
      '<input id="tCron" type="text" placeholder="0 8 * * *" value="' + esc(cron) + '">' +
      '<p class="muted" style="margin:2px 0 0">示例：<code>0 8 * * *</code> 每天 8:00；<code>30 18 * * 1-5</code> 周一至五 18:30</p></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="tEnabled" type="checkbox"' + ((!editing) || (editing && t.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<p class="muted mt">开播动作到点自动播放节目单（从头播放最新内容）；关播动作到点停止推流。保存后可用「立即执行」先试一次。</p>' +
      '<div class="form-actions"><button id="tCancel" class="btn" type="button">取消</button>' +
      '<button id="tSave" class="btn btn-primary" type="button">保存</button></div>');

    var actionSel = m.querySelector('#tAction');
    if (isStop) { actionSel.value = 'stop'; }
    var plField = m.querySelector('#tPlField');
    var plSel = m.querySelector('#tPlaylist');
    playlists.forEach(function (p) {
      plSel.appendChild(el('option', { value: p.id, text: p.name }));
    });
    if (editing && t.playlistId) { plSel.value = t.playlistId; }
    function syncAction() {
      plField.style.display = actionSel.value === 'stop' ? 'none' : '';
    }
    actionSel.addEventListener('change', syncAction);
    syncAction();

    var timeSel = m.querySelector('#tTimeMode');
    if (timeMode === 'cron') { timeSel.value = 'cron'; }
    function syncTime() {
      var isCron = timeSel.value === 'cron';
      m.querySelector('#tDailyField').style.display = isCron ? 'none' : '';
      m.querySelector('#tCronField').style.display = isCron ? '' : 'none';
    }
    timeSel.addEventListener('change', syncTime);
    syncTime();

    m.querySelector('#tCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#tSave').addEventListener('click', function () {
      var name = m.querySelector('#tName').value.trim();
      if (!name) { toast('任务名称不能为空', 'err'); return; }
      var action = actionSel.value;
      var cronValue;
      if (timeSel.value === 'cron') {
        cronValue = m.querySelector('#tCron').value.trim();
      } else {
        var hh = parseInt(m.querySelector('#tHour').value, 10);
        var mm = parseInt(m.querySelector('#tMin').value, 10);
        if (isNaN(hh) || isNaN(mm) || hh < 0 || hh > 23 || mm < 0 || mm > 59) {
          toast('请填写有效的时间（时 0-23，分 0-59）', 'err'); return;
        }
        cronValue = mm + ' ' + hh + ' * * *';
      }
      if (action === 'play' && !plSel.value) { toast('请选择节目单', 'err'); return; }
      var body = {
        name: name,
        type: 'cron',
        cron: cronValue,
        action: action,
        playlistId: action === 'play' ? plSel.value : undefined,
        enabled: m.querySelector('#tEnabled').checked
      };
      var p = editing ? post('/task/replace', Object.assign({ id: t.id }, body)) : post('/task/add', body);
      p.then(function () {
        toast(editing ? '任务已更新' : '任务已创建', 'ok'); m.remove(); renderTasks();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

function renderOutput() {
    setView(loadingView());
    get('/output/list').then(function (data) {
      setConn(true);
      var outputs = listOf(data, ['outputs', 'items', 'list']);
      drawOutput(outputs);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderOutput));
    });
  }

  function drawOutput(outputs) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">输出 (' + outputs.length + ')</h2>' +
      '<button id="addOut" class="btn btn-primary" type="button">添加输出</button>'));

    if (outputs.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('尚未配置输出。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>状态</th><th>路径</th><th>标识</th><th>开始时间</th><th>结束时间</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      outputs.forEach(function (o) {
        var tr = el('tr', {});
        var delBtn = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        delBtn.addEventListener('click', function () {
          if (!confirm('移除输出 "' + (o.unique || o.path) + '"?')) { return; }
          del('/output/remove/' + encodeURIComponent(o.unique)).then(function () {
            toast('输出已移除', 'ok'); renderOutput();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<span class="badge ' + (o.connected ? 'badge-ok' : 'badge-neutral') + '">' + (o.connected ? '已连接' : '未连接') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(o.path || '-') + '</span>'));
        tr.appendChild(el('td', { text: o.unique || '-' }));
        tr.appendChild(el('td', { text: fmtTime(o.start_time) }));
        tr.appendChild(el('td', { text: fmtTime(o.end_time) }));
        tr.appendChild(el('td', {}, '<div class="icon-btn-row"></div>'));
        tr.lastChild.firstChild.appendChild(delBtn);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#addOut').addEventListener('click', function () { openOutputAdd(); });
    setView(wrap);
  }

  function openOutputAdd() {
    var m = el('div', { class: 'modal' },
      '<h3>添加输出</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>路径</label><input id="oPath" type="text" placeholder="rtmp://..."></div>' +
      '<div class="field"><label>标识</label><input id="oUnique" type="text"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="oCancel" class="btn" type="button">取消</button>' +
      '<button id="oSave" class="btn btn-primary" type="button">添加</button></div>');
    m.querySelector('#oCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#oSave').addEventListener('click', function () {
      var path = m.querySelector('#oPath').value.trim();
      if (!path) { toast('输出路径不能为空', 'err'); return; }
      post('/output/add', { path: path, unique: m.querySelector('#oUnique').value.trim() || undefined })
        .then(function () { toast('输出已添加', 'ok'); m.remove(); renderOutput(); })
        .catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Output groups (management)
   * ------------------------------------------------------------------ */
  function renderOutputGroups() {
    setView(loadingView());
    get('/output-group').then(function (data) {
      setConn(true);
      var groups = listOf(data, ['groups', 'items', 'list']);
      drawOutputGroups(groups);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderOutputGroups));
    });
  }

  function drawOutputGroups(groups) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">输出组 (' + groups.length + ')</h2>' +
      '<button id="newOg" class="btn btn-primary" type="button">新建组</button>'));

    if (groups.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无输出组。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>平台</th><th>地域</th><th>业务</th><th>输出</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      groups.forEach(function (g) {
        var tr = el('tr', {});
        var outs = (g.outputs || []).length;
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: g.enabled ? 'Disable' : 'Enable' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/output-group/' + encodeURIComponent(g.id) + '/enabled', { enabled: !g.enabled }).then(function () {
            toast('输出组 ' + (g.enabled ? '已停用' : '已启用'), 'ok'); renderOutputGroups();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openOutputGroupEdit(g); });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除输出组 "' + g.name + '"?')) { return; }
          del('/output-group/' + encodeURIComponent(g.id)).then(function () {
            toast('输出组已删除', 'ok'); renderOutputGroups();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(g.name || '-') + '</span>' +
          (g.description ? '<span class="sub">' + esc(g.description) + '</span>' : '') + '</div>'));
        tr.appendChild(el('td', { text: g.platform || '-' }));
        tr.appendChild(el('td', { text: g.region || '-' }));
        tr.appendChild(el('td', { text: g.business || '-' }));
        tr.appendChild(el('td', { text: String(outs) }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (g.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (g.enabled ? '启用' : '禁用') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newOg').addEventListener('click', function () { openOutputGroupEdit(null); });
    setView(wrap);
  }

  function openOutputGroupEdit(g) {
    var editing = !!g;
    var outputs = editing ? (g.outputs || []).slice() : [];

    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑输出组' : '新建输出组') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="ogName" type="text" value="' + esc(editing ? g.name : '') + '"></div>' +
      '<div class="field"><label>平台</label><input id="ogPlatform" type="text" value="' + esc(editing ? (g.platform || '') : '') + '"></div>' +
      '<div class="field"><label>地域</label><input id="ogRegion" type="text" value="' + esc(editing ? (g.region || '') : '') + '"></div>' +
      '<div class="field"><label>业务</label><input id="ogBusiness" type="text" value="' + esc(editing ? (g.business || '') : '') + '"></div>' +
      '<div class="field" style="grid-column:1/-1"><label>描述</label><input id="ogDesc" type="text" value="' + esc(editing ? (g.description || '') : '') + '"></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="ogEnabled" type="checkbox"' + ((!editing) || (editing && g.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="field mt"><label>输出引用</label>' +
      '<div class="cluster"><input id="ogOutAdd" type="text" placeholder="唯一标识或 URL" style="flex:1;min-width:180px">' +
      '<button id="ogOutBtn" class="btn" type="button">+ 添加</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>输出引用</th><th></th></tr></thead><tbody id="ogOutList"></tbody></table></div>' +
      '<div class="form-actions"><button id="ogCancel" class="btn" type="button">取消</button>' +
      '<button id="ogSave" class="btn btn-primary" type="button">保存</button></div>');

    var listBody = m.querySelector('#ogOutList');
    function drawOutputs() {
      listBody.innerHTML = '';
      outputs.forEach(function (u, idx) {
        var tr = el('tr', {});
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { outputs.splice(idx, 1); drawOutputs(); });
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(u) + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        listBody.appendChild(tr);
      });
    }
    drawOutputs();

    var addInput = m.querySelector('#ogOutAdd');
    function addOutput() {
      var v = addInput.value.trim();
      if (!v) { toast('请输入输出引用', 'err'); return; }
      if (outputs.indexOf(v) !== -1) { toast('已在列表中', 'err'); return; }
      outputs.push(v);
      addInput.value = '';
      drawOutputs();
    }
    m.querySelector('#ogOutBtn').addEventListener('click', addOutput);
    addInput.addEventListener('keydown', function (e) { if (e.key === 'Enter') { addOutput(); } });

    m.querySelector('#ogCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#ogSave').addEventListener('click', function () {
      var name = m.querySelector('#ogName').value.trim();
      if (!name) { toast('组名称不能为空', 'err'); return; }
      var body = {
        name: name,
        description: m.querySelector('#ogDesc').value.trim() || undefined,
        platform: m.querySelector('#ogPlatform').value.trim() || undefined,
        region: m.querySelector('#ogRegion').value.trim() || undefined,
        business: m.querySelector('#ogBusiness').value.trim() || undefined,
        outputs: outputs,
        enabled: m.querySelector('#ogEnabled').checked
      };
      var p = editing
        ? post('/output-group/update', Object.assign({ id: g.id }, body))
        : post('/output-group', body);
      p.then(function () {
        toast(editing ? '输出组已更新' : '输出组已创建', 'ok');
        m.remove(); renderOutputGroups();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Failovers (output failover pairs)
   * ------------------------------------------------------------------ */
  function renderFailovers() {
    setView(loadingView());
    get('/failover').then(function (data) {
      setConn(true);
      var failovers = listOf(data, ['failovers', 'items', 'list']);
      drawFailovers(failovers);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderFailovers));
    });
  }

  function drawFailovers(failovers) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">主备切换 (' + failovers.length + ')</h2>' +
      '<button id="newFo" class="btn btn-primary" type="button">新建主备切换</button>'));

    if (failovers.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('尚未配置主备切换。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>主输出</th><th>备输出</th><th>策略</th><th>阈值</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      failovers.forEach(function (f) {
        var tr = el('tr', {});
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: f.enabled ? 'Disable' : 'Enable' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/failover/' + encodeURIComponent(f.id) + '/enabled', { enabled: !f.enabled }).then(function () {
            toast('主备切换 ' + (f.enabled ? '已停用' : '已启用'), 'ok'); renderFailovers();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openFailoverEdit(f); });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除主备切换 "' + f.name + '"?')) { return; }
          del('/failover/' + encodeURIComponent(f.id)).then(function () {
            toast('主备切换已删除', 'ok'); renderFailovers();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(f.name || '-') + '</span>' +
          '<span class="sub">' + esc(f.id) + '</span></div>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(f.primaryUnique || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(f.backupUnique || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + (f.policy === 'manual' ? 'badge-neutral' : 'badge-info') + '">' + esc(f.policy || 'automatic') + '</span>'));
        tr.appendChild(el('td', { text: f.thresholdSeconds != null ? f.thresholdSeconds + 's' : '-' }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (f.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (f.enabled ? '启用' : '禁用') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newFo').addEventListener('click', function () { openFailoverEdit(null); });
    setView(wrap);
  }

  function openFailoverEdit(f) {
    var editing = !!f;
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑主备切换' : '新建主备切换') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="foName" type="text" value="' + esc(editing ? f.name : '') + '"></div>' +
      '<div class="field"><label>主输出唯一标识</label><input id="foPrimary" type="text" value="' + esc(editing ? (f.primaryUnique || '') : '') + '"></div>' +
      '<div class="field"><label>备输出唯一标识</label><input id="foBackup" type="text" value="' + esc(editing ? (f.backupUnique || '') : '') + '"></div>' +
      '<div class="field"><label>策略</label><select id="foPolicy"><option value="automatic">自动</option><option value="manual">手动</option></select></div>' +
      '<div class="field"><label>阈值（秒）</label><input id="foThreshold" type="number" min="1" value="' + (editing && f.thresholdSeconds != null ? f.thresholdSeconds : 30) + '"></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="foEnabled" type="checkbox"' + ((!editing) || (editing && f.enabled) ? ' checked' : '') + '> Enabled</label></div>' +
      '<div class="form-actions"><button id="foCancel" class="btn" type="button">取消</button>' +
      '<button id="foSave" class="btn btn-primary" type="button">保存</button></div>');

    if (editing && f.policy) { m.querySelector('#foPolicy').value = f.policy; }
    m.querySelector('#foCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#foSave').addEventListener('click', function () {
      var name = m.querySelector('#foName').value.trim();
      var primary = m.querySelector('#foPrimary').value.trim();
      var backup = m.querySelector('#foBackup').value.trim();
      if (!name) { toast('请输入名称', 'err'); return; }
      if (!primary) { toast('主输出唯一标识不能为空', 'err'); return; }
      if (!backup) { toast('备输出唯一标识不能为空', 'err'); return; }
      var threshold = Number(m.querySelector('#foThreshold').value);
      if (!(threshold > 0)) { toast('阈值必须为正数秒', 'err'); return; }
      var body = {
        name: name,
        primaryUnique: primary,
        backupUnique: backup,
        policy: m.querySelector('#foPolicy').value,
        thresholdSeconds: threshold,
        enabled: m.querySelector('#foEnabled').checked
      };
      var p = editing
        ? post('/failover/update', Object.assign({ id: f.id }, body))
        : post('/failover', body);
      p.then(function () {
        toast(editing ? '主备切换已更新' : '主备切换已创建', 'ok');
        m.remove(); renderFailovers();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Health policies
   * ------------------------------------------------------------------ */
  function renderHealthPolicies() {
    setView(loadingView());
    get('/health-policy').then(function (data) {
      setConn(true);
      var policies = listOf(data, ['healthPolicies', 'items', 'list']);
      drawHealthPolicies(policies);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderHealthPolicies));
    });
  }

  function drawHealthPolicies(policies) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">健康策略 (' + policies.length + ')</h2>' +
      '<button id="newHp" class="btn btn-primary" type="button">新建策略</button>'));

    if (policies.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无健康策略。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>最大重试</th><th>重试窗口</th><th>自动跳过</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      policies.forEach(function (h) {
        var tr = el('tr', {});
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: h.enabled ? 'Disable' : 'Enable' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/health-policy/' + encodeURIComponent(h.id) + '/enabled', { enabled: !h.enabled }).then(function () {
            toast('健康策略 ' + (h.enabled ? '已停用' : '已启用'), 'ok'); renderHealthPolicies();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openHealthPolicyEdit(h); });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除健康策略 "' + h.name + '"?')) { return; }
          del('/health-policy/' + encodeURIComponent(h.id)).then(function () {
            toast('健康策略已删除', 'ok'); renderHealthPolicies();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(h.name || '-') + '</span>' +
          '<span class="sub">' + esc(h.id) + '</span></div>'));
        tr.appendChild(el('td', { text: h.maxRetries != null ? String(h.maxRetries) : '-' }));
        tr.appendChild(el('td', { text: h.retryWindowSeconds != null ? h.retryWindowSeconds + 's' : '-' }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (h.autoSkipOnFailure ? 'badge-ok' : 'badge-neutral') + '">' + (h.autoSkipOnFailure ? '是' : '否') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + (h.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (h.enabled ? '启用' : '禁用') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newHp').addEventListener('click', function () { openHealthPolicyEdit(null); });
    setView(wrap);
  }

  function openHealthPolicyEdit(h) {
    var editing = !!h;
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑健康策略' : '新建健康策略') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="hpName" type="text" value="' + esc(editing ? h.name : '') + '"></div>' +
      '<div class="field"><label>最大重试次数</label><input id="hpRetries" type="number" min="0" value="' + (editing && h.maxRetries != null ? h.maxRetries : 3) + '"></div>' +
      '<div class="field"><label>重试窗口（秒）</label><input id="hpWindow" type="number" min="0" value="' + (editing && h.retryWindowSeconds != null ? h.retryWindowSeconds : 60) + '"></div>' +
      '</div>' +
      '<div class="cluster mt">' +
      '<label class="check"><input id="hpAutoSkip" type="checkbox"' + (editing && h.autoSkipOnFailure ? ' checked' : '') + '> 失败自动跳过</label>' +
      '<label class="check"><input id="hpEnabled" type="checkbox"' + ((!editing) || (editing && h.enabled) ? ' checked' : '') + '> Enabled</label>' +
      '</div>' +
      '<div class="form-actions"><button id="hpCancel" class="btn" type="button">取消</button>' +
      '<button id="hpSave" class="btn btn-primary" type="button">保存</button></div>');

    m.querySelector('#hpCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#hpSave').addEventListener('click', function () {
      var name = m.querySelector('#hpName').value.trim();
      if (!name) { toast('请输入名称', 'err'); return; }
      var retries = Number(m.querySelector('#hpRetries').value);
      var retryWindow = Number(m.querySelector('#hpWindow').value);
      if (!(retries >= 0) || !(retryWindow >= 0)) {
        toast('最大重试次数与重试窗口必须为非负数', 'err'); return;
      }
      var body = {
        name: name,
        maxRetries: retries,
        retryWindowSeconds: retryWindow,
        autoSkipOnFailure: m.querySelector('#hpAutoSkip').checked,
        enabled: m.querySelector('#hpEnabled').checked
      };
      var p = editing
        ? post('/health-policy/update', Object.assign({ id: h.id }, body))
        : post('/health-policy', body);
      p.then(function () {
        toast(editing ? '健康策略已更新' : '健康策略已创建', 'ok');
        m.remove(); renderHealthPolicies();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Cache tasks
   * ------------------------------------------------------------------ */
  function renderCacheTasks() {
    setView(loadingView());
    Promise.all([get('/cache-task'), get('/media/list')]).then(function (res) {
      setConn(true);
      var tasks = listOf(res[0], ['cacheTasks', 'items', 'list']);
      var media = listOf(res[1], ['media', 'items', 'list']);
      drawCacheTasks(tasks, media);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderCacheTasks));
    });
  }

  function drawCacheTasks(tasks, media) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">缓存任务 (' + tasks.length + ')</h2>' +
      '<button id="newCt" class="btn btn-primary" type="button">新建任务</button>'));

    if (tasks.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无缓存任务。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>媒体</th><th>状态</th><th>备注</th><th>更新时间</th><th>完成时间</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      tasks.forEach(function (c) {
        var tr = el('tr', {});
        var status = (c.status || 'pending').toLowerCase();
        var btnStart = el('button', { class: 'btn btn-sm', type: 'button', text: '开始' });
        var btnDone = el('button', { class: 'btn btn-sm', type: 'button', text: '标记完成' });
        var btnFail = el('button', { class: 'btn btn-sm', type: 'button', text: '标记失败' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnStart.addEventListener('click', function () {
          post('/cache-task/' + encodeURIComponent(c.id) + '/running', {}).then(function () {
            toast('缓存任务已启动', 'ok'); renderCacheTasks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnDone.addEventListener('click', function () {
          post('/cache-task/' + encodeURIComponent(c.id) + '/done', {}).then(function () {
            toast('缓存任务已标记完成', 'ok'); renderCacheTasks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnFail.addEventListener('click', function () { openCacheTaskFail(c); });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除缓存任务 "' + (c.mediaId || c.id) + '"?')) { return; }
          del('/cache-task/' + encodeURIComponent(c.id)).then(function () {
            toast('缓存任务已删除', 'ok'); renderCacheTasks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(c.mediaId || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + (status === 'done' ? 'badge-ok' : status === 'failed' ? 'badge-crit' : status === 'running' ? 'badge-info' : 'badge-neutral') + '">' + esc(status) + '</span>'));
        tr.appendChild(el('td', { text: c.note || '-' }));
        tr.appendChild(el('td', { text: fmtAgo(c.updatedAt || c.updated_at) }));
        tr.appendChild(el('td', { text: c.completedAt ? fmtAgo(c.completedAt) : '-' }));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        if (status === 'pending' || status === 'failed') { ops.firstChild.appendChild(btnStart); }
        if (status === 'pending' || status === 'running') {
          ops.firstChild.appendChild(btnDone);
          ops.firstChild.appendChild(btnFail);
        }
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newCt').addEventListener('click', function () { openCacheTaskAdd(media); });
    setView(wrap);
  }

  function openCacheTaskAdd(media) {
    media = media || [];
    var m = el('div', { class: 'modal' },
      '<h3>新建缓存任务</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>媒体</label><select id="ctMedia"></select></div>' +
      '<div class="field"><label>或手动输入媒体 ID</label><input id="ctMediaManual" type="text" placeholder="媒体唯一标识"></div>' +
      '<div class="field"><label>备注（可选）</label><input id="ctNote" type="text"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="ctCancel" class="btn" type="button">取消</button>' +
      '<button id="ctSave" class="btn btn-primary" type="button">创建</button></div>');
    var mediaSel = m.querySelector('#ctMedia');
    mediaSel.appendChild(el('option', { value: '', text: media.length ? '-- 选择媒体 --' : '尚未注册媒体' }));
    media.forEach(function (mm) {
      mediaSel.appendChild(el('option', { value: mm.id, text: (mm.name || mm.path) + '  [' + mm.id + ']' }));
    });
    m.querySelector('#ctCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#ctSave').addEventListener('click', function () {
      var mediaId = mediaSel.value || m.querySelector('#ctMediaManual').value.trim();
      if (!mediaId) { toast('媒体 ID 不能为空', 'err'); return; }
      post('/cache-task', {
        mediaId: mediaId,
        note: m.querySelector('#ctNote').value.trim() || undefined
      }).then(function () {
        toast('缓存任务已创建', 'ok'); m.remove(); renderCacheTasks();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  function openCacheTaskFail(c) {
    var m = el('div', { class: 'modal' },
      '<h3>标记缓存任务失败</h3>' +
      '<div class="field"><label>备注（可选）</label><input id="ctfNote" type="text" value="' + esc(c.note || '') + '"></div>' +
      '<div class="form-actions"><button id="ctfCancel" class="btn" type="button">取消</button>' +
      '<button id="ctfSave" class="btn btn-danger" type="button">标记失败</button></div>');
    m.querySelector('#ctfCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#ctfSave').addEventListener('click', function () {
      post('/cache-task/' + encodeURIComponent(c.id) + '/failed', {
        note: m.querySelector('#ctfNote').value.trim() || undefined
      }).then(function () {
        toast('缓存任务已标记失败', 'ok'); m.remove(); renderCacheTasks();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Scene templates
   * ------------------------------------------------------------------ */
  function renderSceneTemplates() {
    setView(loadingView());
    get('/scene-template').then(function (data) {
      setConn(true);
      var templates = listOf(data, ['sceneTemplates', 'items', 'list']);
      drawSceneTemplates(templates);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderSceneTemplates));
    });
  }

  function drawSceneTemplates(templates) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">场景模板 (' + templates.length + ')</h2>' +
      '<button id="newSt" class="btn btn-primary" type="button">新建模板</button>'));

    if (templates.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无场景模板。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>类型</th><th>参数</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      templates.forEach(function (s) {
        var tr = el('tr', {});
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: s.enabled ? 'Disable' : 'Enable' });
        var btnDup = el('button', { class: 'btn btn-sm', type: 'button', text: '复制' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/scene-template/' + encodeURIComponent(s.id) + '/enabled', { enabled: !s.enabled }).then(function () {
            toast('场景模板 ' + (s.enabled ? '已停用' : '已启用'), 'ok'); renderSceneTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnDup.addEventListener('click', function () {
          post('/scene-template/' + encodeURIComponent(s.id) + '/duplicate', {}).then(function () {
            toast('场景模板已复制', 'ok'); renderSceneTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openSceneTemplateEdit(s); });
        btnDel.addEventListener('click', function () {
          if (!confirm('删除场景模板 "' + s.name + '"?')) { return; }
          del('/scene-template/' + encodeURIComponent(s.id)).then(function () {
            toast('场景模板已删除', 'ok'); renderSceneTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(s.name || '-') + '</span>' +
          '<span class="sub">' + esc(s.id) + '</span></div>'));
        tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(s.kind || '-') + '</span>'));
        tr.appendChild(el('td', { text: s.params ? String(Object.keys(s.params).length) : '0' }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (s.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (s.enabled ? '启用' : '禁用') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnDup);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newSt').addEventListener('click', function () { openSceneTemplateEdit(null); });
    setView(wrap);
  }

  var SCENE_KINDS = ['logo', 'clock', 'title', 'scroll', 'watermark', 'progress', 'intro', 'outro', 'background'];

  function openSceneTemplateEdit(s) {
    var editing = !!s;
    var params = editing && s.params
      ? Object.keys(s.params).map(function (k) { return { k: k, v: s.params[k] }; })
      : [];

    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑场景模板' : '新建场景模板') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="stName" type="text" value="' + esc(editing ? s.name : '') + '"></div>' +
      '<div class="field"><label>类型</label><select id="stKind"></select></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="stEnabled" type="checkbox"' + ((!editing) || (editing && s.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="field mt"><label>参数（键 / 值）</label>' +
      '<div class="cluster"><input id="stKeyAdd" type="text" placeholder="键" style="flex:1;min-width:120px">' +
      '<input id="stValAdd" type="text" placeholder="值" style="flex:1;min-width:120px">' +
      '<button id="stAddBtn" class="btn" type="button">+ 添加</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>键</th><th>值</th><th></th></tr></thead><tbody id="stParamList"></tbody></table></div>' +
      '<div class="form-actions"><button id="stCancel" class="btn" type="button">取消</button>' +
      '<button id="stSave" class="btn btn-primary" type="button">保存</button></div>');

    var kindSel = m.querySelector('#stKind');
    SCENE_KINDS.forEach(function (k) {
      kindSel.appendChild(el('option', { value: k, text: k }));
    });
    if (editing && s.kind) { kindSel.value = s.kind; }

    var listBody = m.querySelector('#stParamList');
    function drawParams() {
      listBody.innerHTML = '';
      params.forEach(function (pr, idx) {
        var tr = el('tr', {});
        var keyIn = el('input', { type: 'text', value: pr.k, style: 'width:100%' });
        var valIn = el('input', { type: 'text', value: pr.v, style: 'width:100%' });
        keyIn.addEventListener('input', function () { pr.k = keyIn.value; });
        valIn.addEventListener('input', function () { pr.v = valIn.value; });
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { params.splice(idx, 1); drawParams(); });
        var ktd = el('td', {}, undefined);
        ktd.appendChild(keyIn);
        var vtd = el('td', {}, undefined);
        vtd.appendChild(valIn);
        tr.appendChild(ktd);
        tr.appendChild(vtd);
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        listBody.appendChild(tr);
      });
    }
    drawParams();

    function addParam() {
      var k = m.querySelector('#stKeyAdd').value.trim();
      var v = m.querySelector('#stValAdd').value;
      if (!k) { toast('参数键不能为空', 'err'); return; }
      var dup = params.some(function (pr) { return pr.k === k; });
      if (dup) { toast('键已在列表中', 'err'); return; }
      params.push({ k: k, v: v });
      m.querySelector('#stKeyAdd').value = '';
      m.querySelector('#stValAdd').value = '';
      drawParams();
    }
    m.querySelector('#stAddBtn').addEventListener('click', addParam);
    m.querySelector('#stKeyAdd').addEventListener('keydown', function (e) { if (e.key === 'Enter') { addParam(); } });

    m.querySelector('#stCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#stSave').addEventListener('click', function () {
      var name = m.querySelector('#stName').value.trim();
      if (!name) { toast('模板名称不能为空', 'err'); return; }
      var out = {};
      var dupKey = false;
      params.forEach(function (pr) {
        var k = pr.k.trim();
        if (!k) { return; }
        if (Object.prototype.hasOwnProperty.call(out, k)) { dupKey = true; return; }
        out[k] = pr.v;
      });
      if (dupKey) { toast('不允许重复的参数键', 'err'); return; }
      var body = {
        name: name,
        kind: kindSel.value,
        params: out,
        enabled: m.querySelector('#stEnabled').checked
      };
      var p = editing
        ? post('/scene-template/update', Object.assign({ id: s.id }, body))
        : post('/scene-template', body);
      p.then(function () {
        toast(editing ? '场景模板已更新' : '场景模板已创建', 'ok');
        m.remove(); renderSceneTemplates();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Plugins
   * ------------------------------------------------------------------ */
  // Plugin endpoints are "core control": the permission matrix grants write
  // access to admin/operator while auditor is read-only. The backend gateway
  // does not enforce the matrix, so the frontend hides the write controls
  // for other roles (mirroring applyRoleUI). If state.currentUser is
  // unavailable all controls are shown rather than hiding functionality.
  function pluginCanWrite() {
    var u = state.currentUser;
    if (!u) { return true; }
    return u.role === 'admin' || u.role === 'operator';
  }

  function renderPlugins() {
    setView(loadingView());
    get('/plugin/list').then(function (data) {
      setConn(true);
      var plugins = listOf(data, ['plugins', 'items', 'list']);
      drawPlugins(plugins);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderPlugins));
    });
  }

  // Key/value preview: first few pairs, with a count of the remainder.
  function pluginParamsPreview(p) {
    var params = p && p.params && typeof p.params === 'object' ? p.params : {};
    var keys = Object.keys(params);
    if (!keys.length) { return '-'; }
    var parts = keys.slice(0, 3).map(function (k) { return k + '=' + params[k]; });
    if (keys.length > 3) { parts.push('+更多数' + (keys.length - 3) + ''); }
    return parts.join(', ');
  }

  function drawPlugins(plugins) {
    var canWrite = pluginCanWrite();
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">插件 (' + plugins.length + ')</h2>' +
      (canWrite ? '<button id="newPl" class="btn btn-primary" type="button">添加插件</button>' : '')));

    if (plugins.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('尚未加载插件。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>路径</th><th>标识</th><th>参数</th><th>加载时间</th><th>创建时间</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      plugins.forEach(function (p) {
        var tr = el('tr', {});
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑参数' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        btnEdit.addEventListener('click', function () { openPluginParams(p); });
        btnDel.addEventListener('click', function () {
          if (!confirm('移除插件 "' + p.unique + '"?')) { return; }
          del('/plugin/remove/' + encodeURIComponent(p.unique)).then(function () {
            toast('插件已移除', 'ok'); renderPlugins();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(p.path || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(p.unique || '-') + '</span>'));
        tr.appendChild(el('td', { text: pluginParamsPreview(p) }));
        tr.appendChild(el('td', { text: fmtTime(p.loaded_time || p.loadedTime) }));
        tr.appendChild(el('td', { text: fmtTime(p.create_time || p.createTime) }));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        if (canWrite) {
          ops.firstChild.appendChild(btnEdit);
          ops.firstChild.appendChild(btnDel);
        } else {
          ops.firstChild.appendChild(el('span', { class: 'muted', text: '只读' }));
        }
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    if (canWrite) {
      wrap.querySelector('#newPl').addEventListener('click', function () { openPluginAdd(); });
    }
    setView(wrap);
  }

  function openPluginAdd() {
    var params = [];
    var m = el('div', { class: 'modal' },
      '<h3>添加插件</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>路径</label><input id="plPath" type="text" placeholder="插件文件路径或 URL"></div>' +
      '<div class="field"><label>唯一标识（可选）</label><input id="plUnique" type="text" placeholder="别名"></div>' +
      '</div>' +
      '<div class="field mt"><label>参数（键 / 值）</label>' +
      '<div class="cluster"><input id="plKeyAdd" type="text" placeholder="键" style="flex:1;min-width:120px">' +
      '<input id="plValAdd" type="text" placeholder="值" style="flex:1;min-width:120px">' +
      '<button id="plAddBtn" class="btn" type="button">+ Add</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>键</th><th>值</th><th></th></tr></thead><tbody id="plParamList"></tbody></table></div>' +
      '<div class="form-actions"><button id="plCancel" class="btn" type="button">取消</button>' +
      '<button id="plSave" class="btn btn-primary" type="button">添加</button></div>');

    var listBody = m.querySelector('#plParamList');
    function drawParams() {
      listBody.innerHTML = '';
      params.forEach(function (pr, idx) {
        var tr = el('tr', {});
        var keyIn = el('input', { type: 'text', value: pr.k, style: 'width:100%' });
        var valIn = el('input', { type: 'text', value: pr.v, style: 'width:100%' });
        keyIn.addEventListener('input', function () { pr.k = keyIn.value; });
        valIn.addEventListener('input', function () { pr.v = valIn.value; });
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { params.splice(idx, 1); drawParams(); });
        var ktd = el('td', {}, undefined);
        ktd.appendChild(keyIn);
        var vtd = el('td', {}, undefined);
        vtd.appendChild(valIn);
        tr.appendChild(ktd);
        tr.appendChild(vtd);
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        listBody.appendChild(tr);
      });
    }
    drawParams();

    function addParam() {
      var k = m.querySelector('#plKeyAdd').value.trim();
      var v = m.querySelector('#plValAdd').value;
      if (!k) { toast('参数键不能为空', 'err'); return; }
      var dup = params.some(function (pr) { return pr.k === k; });
      if (dup) { toast('键已在列表中', 'err'); return; }
      params.push({ k: k, v: v });
      m.querySelector('#plKeyAdd').value = '';
      m.querySelector('#plValAdd').value = '';
      drawParams();
    }
    m.querySelector('#plAddBtn').addEventListener('click', addParam);
    m.querySelector('#plKeyAdd').addEventListener('keydown', function (e) { if (e.key === 'Enter') { addParam(); } });

    m.querySelector('#plCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#plSave').addEventListener('click', function () {
      var path = m.querySelector('#plPath').value.trim();
      if (!path) { toast('插件路径不能为空', 'err'); return; }
      var out = {};
      var dupKey = false;
      params.forEach(function (pr) {
        var k = pr.k.trim();
        if (!k) { return; }
        if (Object.prototype.hasOwnProperty.call(out, k)) { dupKey = true; return; }
        out[k] = pr.v;
      });
      if (dupKey) { toast('不允许出现重复的参数键', 'err'); return; }
      post('/plugin/add', {
        path: path,
        unique: m.querySelector('#plUnique').value.trim() || undefined,
        params: out
      }).then(function () {
        toast('插件已添加', 'ok'); m.remove(); renderPlugins();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  function openPluginParams(p) {
    var params = p && p.params && typeof p.params === 'object'
      ? Object.keys(p.params).map(function (k) { return { k: k, v: p.params[k] }; })
      : [];
    var m = el('div', { class: 'modal' },
      '<h3>插件参数 - ' + esc(p.unique || p.path) + '</h3>' +
      '<div class="field mt"><label>参数（键 / 值）</label>' +
      '<div class="cluster"><input id="ppKeyAdd" type="text" placeholder="键" style="flex:1;min-width:120px">' +
      '<input id="ppValAdd" type="text" placeholder="值" style="flex:1;min-width:120px">' +
      '<button id="ppAddBtn" class="btn" type="button">+ Add</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>键</th><th>值</th><th></th></tr></thead><tbody id="ppParamList"></tbody></table></div>' +
      '<div class="form-actions"><button id="ppCancel" class="btn" type="button">取消</button>' +
      '<button id="ppSave" class="btn btn-primary" type="button">保存</button></div>');

    var listBody = m.querySelector('#ppParamList');
    function drawParams() {
      listBody.innerHTML = '';
      params.forEach(function (pr, idx) {
        var tr = el('tr', {});
        var keyIn = el('input', { type: 'text', value: pr.k, style: 'width:100%' });
        var valIn = el('input', { type: 'text', value: pr.v, style: 'width:100%' });
        keyIn.addEventListener('input', function () { pr.k = keyIn.value; });
        valIn.addEventListener('input', function () { pr.v = valIn.value; });
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { params.splice(idx, 1); drawParams(); });
        var ktd = el('td', {}, undefined);
        ktd.appendChild(keyIn);
        var vtd = el('td', {}, undefined);
        vtd.appendChild(valIn);
        tr.appendChild(ktd);
        tr.appendChild(vtd);
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        listBody.appendChild(tr);
      });
    }
    drawParams();

    function addParam() {
      var k = m.querySelector('#ppKeyAdd').value.trim();
      var v = m.querySelector('#ppValAdd').value;
      if (!k) { toast('参数键不能为空', 'err'); return; }
      var dup = params.some(function (pr) { return pr.k === k; });
      if (dup) { toast('键已在列表中', 'err'); return; }
      params.push({ k: k, v: v });
      m.querySelector('#ppKeyAdd').value = '';
      m.querySelector('#ppValAdd').value = '';
      drawParams();
    }
    m.querySelector('#ppAddBtn').addEventListener('click', addParam);
    m.querySelector('#ppKeyAdd').addEventListener('keydown', function (e) { if (e.key === 'Enter') { addParam(); } });

    m.querySelector('#ppCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#ppSave').addEventListener('click', function () {
      var out = {};
      var dupKey = false;
      params.forEach(function (pr) {
        var k = pr.k.trim();
        if (!k) { return; }
        if (Object.prototype.hasOwnProperty.call(out, k)) { dupKey = true; return; }
        out[k] = pr.v;
      });
      if (dupKey) { toast('不允许出现重复的参数键', 'err'); return; }
      patch('/plugin/update', {
        unique: p.unique,
        params: out
      }).then(function () {
        toast('插件参数已更新', 'ok'); m.remove(); renderPlugins();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Webhooks
   * ------------------------------------------------------------------ */
  var WEBHOOK_EVENTS = ['output_disconnected', 'channel_status_changed', 'material_failed', 'task_completed'];

  function renderWebhooks() {
    setView(loadingView());
    get('/webhook').then(function (data) {
      setConn(true);
      var webhooks = listOf(data, ['webhooks', 'items', 'list']);
      drawWebhooks(webhooks);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderWebhooks));
    });
  }

  function drawWebhooks(webhooks) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">Webhook（' + webhooks.length + '）</h2>' +
      '<button id="newWh" class="btn btn-primary" type="button">新建 Webhook</button>'));

    if (webhooks.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('尚未配置任何 Webhook。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>URL</th><th>事件</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      webhooks.forEach(function (w) {
        var tr = el('tr', {});
        var events = Array.isArray(w.events) ? w.events : [];
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: w.enabled ? '停用' : '启用' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDelv = el('button', { class: 'btn btn-sm', type: 'button', text: '投递记录' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/webhook/' + encodeURIComponent(w.id) + '/enabled', { enabled: !w.enabled }).then(function () {
            toast('Webhook ' + (w.enabled ? '已停用' : '已启用'), 'ok'); renderWebhooks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openWebhookEdit(w); });
        btnDelv.addEventListener('click', function () { openWebhookDeliveries(w); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除 Webhook "' + w.name + '" 吗？')) { return; }
          del('/webhook/' + encodeURIComponent(w.id)).then(function () {
            toast('Webhook 已删除', 'ok'); renderWebhooks();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(w.name || '-') + '</span>' +
          '<span class="sub">' + esc(w.id) + '</span></div>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(w.url || '-') + '</span>'));
        var evTd = el('td', {}, '<div class="cluster" style="gap:4px;"></div>');
        WEBHOOK_EVENTS.forEach(function (ev) {
          var on = events.indexOf(ev) !== -1;
          evTd.firstChild.appendChild(el('span', { class: 'badge ' + (on ? 'badge-info' : 'badge-neutral'), text: ev }));
        });
        tr.appendChild(evTd);
        tr.appendChild(el('td', {}, '<span class="badge ' + (w.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (w.enabled ? 'enabled' : 'disabled') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDelv);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newWh').addEventListener('click', function () { openWebhookEdit(null); });
    setView(wrap);
  }

  function openWebhookEdit(w) {
    var editing = !!w;
    var events = editing && Array.isArray(w.events) ? w.events : [];
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑 Webhook' : '新建 Webhook') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="whName" type="text" value="' + esc(editing ? w.name : '') + '"></div>' +
      '<div class="field"><label>URL</label><input id="whUrl" type="text" value="' + esc(editing ? (w.url || '') : '') + '" placeholder="https://..."></div>' +
      '</div>' +
      '<div class="field mt"><label>事件（至少一个）</label><div id="whEvents"></div></div>' +
      '<div class="cluster mt"><label class="check"><input id="whEnabled" type="checkbox"' + ((!editing) || (editing && w.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="form-actions"><button id="whCancel" class="btn" type="button">取消</button>' +
      '<button id="whSave" class="btn btn-primary" type="button">保存</button></div>');

    var evHost = m.querySelector('#whEvents');
    WEBHOOK_EVENTS.forEach(function (ev) {
      evHost.appendChild(el('label', { class: 'check' },
        '<input type="checkbox" data-ev="' + ev + '"' + (events.indexOf(ev) !== -1 ? ' checked' : '') + '> ' + esc(ev)));
    });

    m.querySelector('#whCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#whSave').addEventListener('click', function () {
      var name = m.querySelector('#whName').value.trim();
      if (!name) { toast('必须填写 Webhook 名称', 'err'); return; }
      var url = m.querySelector('#whUrl').value.trim();
      if (!url) { toast('必须填写 Webhook URL', 'err'); return; }
      var checked = [];
      evHost.querySelectorAll('input[data-ev]').forEach(function (cb) {
        if (cb.checked) { checked.push(cb.dataset.ev); }
      });
      if (!checked.length) { toast('请至少选择一个事件', 'err'); return; }
      var body = {
        name: name,
        url: url,
        events: checked,
        enabled: m.querySelector('#whEnabled').checked
      };
      var p = editing
        ? post('/webhook/update', Object.assign({ id: w.id }, body))
        : post('/webhook', body);
      p.then(function () {
        toast(editing ? 'Webhook 已更新' : 'Webhook 已创建', 'ok');
        m.remove(); renderWebhooks();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  function openWebhookDeliveries(w) {
    var m = el('div', { class: 'modal' },
      '<h3>投递记录 - ' + esc(w.name || w.id) + '</h3>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>事件</th><th>状态</th><th>尝试次数</th><th>最后错误</th><th>投递时间</th></tr></thead><tbody id="wdBody"><tr><td colspan="5" class="muted">加载中 ...</td></tr></tbody></table></div>' +
      '<div class="form-actions"><button id="wdRefresh" class="btn" type="button">刷新</button>' +
      '<button id="wdClose" class="btn btn-primary" type="button">关闭</button></div>');

    var body = m.querySelector('#wdBody');
    function load() {
      body.innerHTML = '';
      body.appendChild(el('tr', {}, '<td colspan="5" class="muted">加载中 ...</td>'));
      get('/webhook/' + encodeURIComponent(w.id) + '/deliveries').then(function (data) {
        var deliveries = listOf(data, ['deliveries', 'items', 'list']);
        body.innerHTML = '';
        if (!deliveries.length) {
          body.appendChild(el('tr', {}, '<td colspan="5" class="muted">暂无投递记录。</td>'));
          return;
        }
        deliveries.forEach(function (d) {
          var status = (d.status || '').toLowerCase();
          var tr = el('tr', {});
          tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(d.event || '-') + '</span>'));
          tr.appendChild(el('td', {}, '<span class="badge ' + (status === 'success' ? 'badge-ok' : status === 'failed' ? 'badge-crit' : 'badge-neutral') + '">' + esc(status || '-') + '</span>'));
          tr.appendChild(el('td', { text: d.attempts != null ? String(d.attempts) : '-' }));
          tr.appendChild(el('td', {}, '<span class="mono">' + esc(d.lastError || '-') + '</span>'));
          tr.appendChild(el('td', { text: fmtTime(d.deliveredAt || d.createdAt || d.delivered_at) }));
          body.appendChild(tr);
        });
      }).catch(function (e) {
        body.innerHTML = '';
        body.appendChild(el('tr', {}, '<td colspan="5" class="muted">' + esc(e.message) + '</td>'));
      });
    }
    load();
    m.querySelector('#wdRefresh').addEventListener('click', load);
    m.querySelector('#wdClose').addEventListener('click', function () { m.remove(); });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Audit log
   * ------------------------------------------------------------------ */
  function renderAudit() {
    setView(loadingView());
    var q = [];
    var f = state.auditFilter;
    if (f.operator) { q.push('operator=' + encodeURIComponent(f.operator)); }
    if (f.action) { q.push('action=' + encodeURIComponent(f.action)); }
    if (f.result) { q.push('result=' + encodeURIComponent(f.result)); }
    get('/audit' + (q.length ? '?' + q.join('&') : '')).then(function (data) {
      setConn(true);
      var entries = listOf(data, ['audit', 'items', 'list']);
      drawAudit(entries);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderAudit));
    });
  }

  function drawAudit(entries) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">审计日志（' + entries.length + '）</h2>' +
      '<button id="auPrune" class="btn" type="button">清理</button>'));

    var card = el('div', { class: 'card section' },
      '<div class="form-grid">' +
      '<div class="field"><label>操作人</label><input id="auOp" type="text" placeholder="按操作人筛选"></div>' +
      '<div class="field"><label>动作</label><input id="auAction" type="text" placeholder="按动作筛选"></div>' +
      '<div class="field"><label>结果</label><select id="auResult"><option value="">任意</option><option value="success">成功</option><option value="failure">失败</option></select></div>' +
      '</div>' +
      '<div class="form-actions"><button id="auFilter" class="btn btn-primary" type="button">筛选</button>' +
      '<button id="auClear" class="btn" type="button">清除</button></div>');
    card.querySelector('#auOp').value = state.auditFilter.operator;
    card.querySelector('#auAction').value = state.auditFilter.action;
    card.querySelector('#auResult').value = state.auditFilter.result;
    card.querySelector('#auFilter').addEventListener('click', function () {
      state.auditFilter.operator = card.querySelector('#auOp').value.trim();
      state.auditFilter.action = card.querySelector('#auAction').value.trim();
      state.auditFilter.result = card.querySelector('#auResult').value;
      renderAudit();
    });
    card.querySelector('#auClear').addEventListener('click', function () {
      state.auditFilter = { operator: '', action: '', result: '' };
      renderAudit();
    });
    wrap.appendChild(card);

    if (entries.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无审计记录。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>时间</th><th>操作人</th><th>动作</th><th>目标</th><th>结果</th><th>详情</th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      entries.forEach(function (a) {
        var result = (a.result || '').toLowerCase();
        var tr = el('tr', {});
        tr.appendChild(el('td', { text: fmtTime(a.time) }));
        tr.appendChild(el('td', { text: a.operator || '-' }));
        tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(a.action || '-') + '</span>'));
        tr.appendChild(el('td', { text: a.target || '-' }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (result === 'success' ? 'badge-ok' : result === 'failure' ? 'badge-crit' : 'badge-neutral') + '">' + esc(result || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(a.detail || '-') + '</span>'));
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap section' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#auPrune').addEventListener('click', openAuditPrune);
    setView(wrap);
  }

  function openAuditPrune() {
    var m = el('div', { class: 'modal' },
      '<h3>清理审计日志</h3>' +
      '<div class="field"><label>保留的最大条数</label><input id="auKeep" type="number" min="0" step="1" placeholder="e.g. 500"></div>' +
      '<div class="muted">早于最新 N 条的旧记录将被永久删除。</div>' +
      '<div class="form-actions"><button id="auPCancel" class="btn" type="button">取消</button>' +
      '<button id="auPSave" class="btn btn-danger" type="button">清理</button></div>');
    m.querySelector('#auPCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#auPSave').addEventListener('click', function () {
      var n = Number(m.querySelector('#auKeep').value);
      if (!(n >= 0) || Math.floor(n) !== n) { toast('请输入非负整数', 'err'); return; }
      var save = m.querySelector('#auPSave');
      save.disabled = true;
      save.textContent = '清理中 ...';
      post('/audit/prune', { maxEntries: n }).then(function (res) {
        var r = unwrap(res);
        var removed = (r && r.removed != null) ? r.removed : 0;
        toast('审计日志已清理：移除 ' + removed + ' 条记录', 'ok');
        m.remove();
        renderAudit();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { save.disabled = false; save.textContent = '清理'; });
    });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Alarms
   * ------------------------------------------------------------------ */
  function renderAlarms() {
    setView(loadingView());
    get('/alarm/list').then(function (data) {
      setConn(true);
      var alarms = listOf(data, ['alarms', 'items', 'list']);
      drawAlarms(alarms);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderAlarms));
    });
  }

  function drawAlarms(alarms) {
    var wrap = el('div', {});
    var active = alarms.filter(function (a) { return (a.status || '').toLowerCase() === 'active'; });

    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">告警（' + active.length + ' 条待处理 / 共 ' + alarms.length + ' 条）</h2>' +
      '<button id="resolveAll" class="btn" type="button">全部解决</button>'));

    if (alarms.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无告警，一切正常。', undefined).innerHTML));
    } else {
      var stack = el('div', { class: 'list-stack' });
      alarms.forEach(function (a) {
        var level = (a.level || 'info').toLowerCase();
        var badgeClass = level === 'critical' ? 'badge-crit' : level === 'warning' ? 'badge-warn' : 'badge-info';
        var isActive = (a.status || '').toLowerCase() === 'active';
        var item = el('div', { class: 'list-item' },
          '<div class="list-item-head"><span class="badge ' + badgeClass + '">' + esc(level) + '</span>' +
          '<span class="list-item-title">' + esc(a.title) + '</span>' +
          '<div class="icon-btn-row"></div></div>' +
          (a.message ? '<div class="mt muted">' + esc(a.message) + '</div>' : '') +
          '<div class="list-item-meta"><span>' + fmtAgo(a.createdAt || a.created_at) + '</span>' +
          '<span class="badge ' + (isActive ? 'badge-warn' : 'badge-neutral') + '">' + (isActive ? '待处理' : '已解决') + '</span></div>');

        if (isActive) {
          var btnResolve = el('button', { class: 'btn btn-sm', type: 'button', text: '解决' });
          btnResolve.addEventListener('click', function () {
            post('/alarm/resolve', { id: a.id }).then(function () {
              toast('告警已解决', 'ok'); renderAlarms();
            }).catch(function (e) { toast(e.message, 'err'); });
          });
          item.querySelector('.icon-btn-row').appendChild(btnResolve);
        }
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnDel.addEventListener('click', function () {
          post('/alarm/delete', { id: a.id }).then(function () {
            toast('告警已删除', 'ok'); renderAlarms();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        item.querySelector('.icon-btn-row').appendChild(btnDel);
        stack.appendChild(item);
      });
      wrap.appendChild(stack);
    }

    wrap.querySelector('#resolveAll').addEventListener('click', function () {
      post('/alarm/resolve-all', {}).then(function () {
        toast('所有待处理告警已解决', 'ok'); renderAlarms();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    setView(wrap);
  }

  /* ------------------------------------------------------------------ *
   * User management (admin only)
   * ------------------------------------------------------------------ */
  var USER_ROLES = ['admin', 'operator', 'auditor'];

  function roleBadgeClass(role) {
    if (role === 'admin') { return 'badge-warn'; }
    if (role === 'operator') { return 'badge-info'; }
    return 'badge-neutral';
  }

  function renderUsers() {
    setView(loadingView());
    get('/user').then(function (data) {
      setConn(true);
      var users = listOf(data, ['users', 'items', 'list']);
      drawUsers(users);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderUsers));
    });
  }

  function drawUsers(users) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">用户（' + users.length + '）</h2>' +
      '<button id="newUser" class="btn btn-primary" type="button">新建用户</button>'));

    if (users.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无用户。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>用户名</th><th>角色</th><th>状态</th><th>创建时间</th><th>更新时间</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      users.forEach(function (u) {
        var tr = el('tr', {});
        // Never let the signed-in admin lock themselves out by disabling or
        // deleting their own account from the UI.
        var isSelf = state.currentUser && u.username === state.currentUser.username;
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: u.enabled ? '停用' : '启用' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnPw = el('button', { class: 'btn btn-sm', type: 'button', text: '重置密码' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/user/' + encodeURIComponent(u.id) + '/enabled', { enabled: !u.enabled }).then(function () {
            toast('用户' + (u.enabled ? '已停用' : '已启用'), 'ok'); renderUsers();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openUserEdit(u); });
        btnPw.addEventListener('click', function () { openUserPassword(u); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除用户 "' + u.username + '" 吗？')) { return; }
          del('/user/' + encodeURIComponent(u.id)).then(function () {
            toast('用户已删除', 'ok'); renderUsers();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(u.username || '-') + '</span>' +
          '<span class="sub">' + esc(u.id) + '</span></div>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + roleBadgeClass(u.role) + '">' + esc(u.role || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + (u.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (u.enabled ? 'enabled' : 'disabled') + '</span>'));
        tr.appendChild(el('td', { text: fmtTime(u.createdAt || u.created_at) }));
        tr.appendChild(el('td', { text: fmtTime(u.updatedAt || u.updated_at) }));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        if (!isSelf) { ops.firstChild.appendChild(btnEn); }
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnPw);
        if (!isSelf) { ops.firstChild.appendChild(btnDel); }
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newUser').addEventListener('click', function () { openUserEdit(null); });
    setView(wrap);
  }

  function openUserEdit(u) {
    var editing = !!u;
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑用户' : '新建用户') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>用户名</label><input id="uName" type="text" value="' + esc(editing ? u.username : '') + '"></div>' +
      '<div class="field"><label>角色</label><select id="uRole"></select></div>' +
      (!editing ? '<div class="field"><label>密码（最少 8 个字符）</label><input id="uPass" type="password" autocomplete="new-password"></div>' : '') +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="uEnabled" type="checkbox"' + ((!editing) || (editing && u.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="form-actions"><button id="uCancel" class="btn" type="button">取消</button>' +
      '<button id="uSave" class="btn btn-primary" type="button">保存</button></div>');

    var roleSel = m.querySelector('#uRole');
    USER_ROLES.forEach(function (r) {
      roleSel.appendChild(el('option', { value: r, text: r }));
    });
    if (editing && u.role) { roleSel.value = u.role; }

    m.querySelector('#uCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#uSave').addEventListener('click', function () {
      var name = m.querySelector('#uName').value.trim();
      if (!name) { toast('必须填写用户名', 'err'); return; }
      var body = {
        username: name,
        role: roleSel.value,
        enabled: m.querySelector('#uEnabled').checked
      };
      if (editing) {
        post('/user/update', Object.assign({ id: u.id }, body)).then(function () {
          toast('用户已更新', 'ok'); m.remove(); renderUsers();
        }).catch(function (e) { toast(e.message, 'err'); });
      } else {
        var pw = m.querySelector('#uPass').value;
        if (pw.length < 8) { toast('密码必须至少 8 个字符', 'err'); return; }
        body.password = pw;
        post('/user', body).then(function () {
          toast('用户已创建', 'ok'); m.remove(); renderUsers();
        }).catch(function (e) { toast(e.message, 'err'); });
      }
    });

    modalScrim(m);
  }

  function openUserPassword(u) {
    var m = el('div', { class: 'modal' },
      '<h3>重置密码 - ' + esc(u.username) + '</h3>' +
      '<div class="field"><label>新密码（最少 8 个字符）</label><input id="upPass" type="password" autocomplete="new-password"></div>' +
      '<div class="form-actions"><button id="upCancel" class="btn" type="button">取消</button>' +
      '<button id="upSave" class="btn btn-primary" type="button">设置密码</button></div>');
    m.querySelector('#upCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#upSave').addEventListener('click', function () {
      var pw = m.querySelector('#upPass').value;
      if (pw.length < 8) { toast('密码必须至少 8 个字符', 'err'); return; }
      post('/user/' + encodeURIComponent(u.id) + '/password', { password: pw }).then(function () {
        toast('密码已更新', 'ok'); m.remove();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Nodes (management)
   * ------------------------------------------------------------------ */
  function renderNodes() {
    setView(loadingView());
    Promise.all([get('/node'), get('/instance')]).then(function (res) {
      setConn(true);
      var nodes = listOf(res[0], ['nodes', 'items', 'list']);
      var instances = listOf(res[1], ['instances', 'items', 'list']);
      drawNodes(nodes, instances);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderNodes));
    });
  }

  function nodeStatusClass(s) {
    s = String(s || '').toLowerCase();
    if (s === 'online') { return 'badge-ok'; }
    if (s === 'offline') { return 'badge-crit'; }
    return 'badge-neutral';
  }

  function instanceStatusClass(s) {
    s = String(s || '').toLowerCase();
    if (s === 'online' || s === 'active' || s === 'running') { return 'badge-ok'; }
    if (s === 'offline' || s === 'inactive' || s === 'stopped' || s === 'failed') { return 'badge-crit'; }
    return 'badge-neutral';
  }

  function drawNodes(nodes, instances) {
    var byNode = {};
    instances.forEach(function (i) { (byNode[i.nodeId] = byNode[i.nodeId] || []).push(i); });

    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">Nodes (' + nodes.length + ')</h2>' +
      '<button id="newNode" class="btn btn-primary" type="button">新建节点</button>'));

    if (nodes.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无节点。', undefined).innerHTML));
    } else {
      var stack = el('div', { class: 'list-stack' });
      nodes.forEach(function (n) {
        var item = el('div', { class: 'list-item' },
          '<div class="list-item-head"><span class="list-item-title">' + esc(n.name || n.id) + '</span>' +
          '<div class="icon-btn-row"></div></div>' +
          '<div class="list-item-meta">' +
          '<span class="mono">' + esc(n.address || '-') + '</span>' +
          '<span class="badge ' + nodeStatusClass(n.status) + '">' + esc(n.status || 'unknown') + '</span>' +
          '<span>上次在线：' + fmtAgo(n.lastSeen) + '</span>' +
          '<span class="badge ' + (n.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (n.enabled ? 'enabled' : 'disabled') + '</span>' +
          '</div>');

        var btnBeat = el('button', { class: 'btn btn-sm', type: 'button', text: '心跳' });
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: n.enabled ? '停用' : '启用' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnBeat.addEventListener('click', function () {
          post('/node/' + encodeURIComponent(n.id) + '/heartbeat', {}).then(function (res) {
            var r = objOf(res, ['node', 'data']);
            toast('心跳已发送 - 节点状态：' + (r.status || '在线'), 'ok');
            renderNodes();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEn.addEventListener('click', function () {
          post('/node/' + encodeURIComponent(n.id) + '/enabled', { enabled: !n.enabled }).then(function () {
            toast('节点' + (n.enabled ? '已停用' : '已启用'), 'ok'); renderNodes();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnEdit.addEventListener('click', function () { openNodeEdit(n); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除节点 "' + (n.name || n.id) + '" 吗？')) { return; }
          del('/node/' + encodeURIComponent(n.id)).then(function () {
            toast('节点已删除', 'ok'); renderNodes();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        item.querySelector('.icon-btn-row').appendChild(btnBeat);
        item.querySelector('.icon-btn-row').appendChild(btnEn);
        item.querySelector('.icon-btn-row').appendChild(btnEdit);
        item.querySelector('.icon-btn-row').appendChild(btnDel);

        var insts = byNode[n.id] || [];
        var instRow = el('div', { class: 'cluster mt' });
        instRow.appendChild(el('span', { class: 'muted', text: '实例（' + insts.length + '）' }));
        var btnAddInst = el('button', { class: 'btn btn-sm', type: 'button', text: '+ 添加实例' });
        btnAddInst.addEventListener('click', function () { openInstanceEdit(null, nodes, n.id); });
        instRow.appendChild(btnAddInst);
        item.appendChild(instRow);

        if (insts.length) {
          var tWrap = el('div', { class: 'table-wrap mt' }, undefined);
          var it = el('table', { class: 'data' },
            '<thead><tr><th>实例</th><th>名称</th><th>状态</th><th>频道</th><th>更新时间</th><th></th></tr></thead><tbody></tbody>');
          var tb = it.querySelector('tbody');
          insts.forEach(function (inst) {
            var tr = el('tr', {});
            var btnIEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
            var btnIDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
            btnIEdit.addEventListener('click', function () { openInstanceEdit(inst, nodes, null); });
            btnIDel.addEventListener('click', function () {
              if (!confirm('确定删除实例 "' + (inst.name || inst.id) + '" 吗？')) { return; }
              del('/instance/' + encodeURIComponent(inst.id)).then(function () {
                toast('实例已删除', 'ok'); renderNodes();
              }).catch(function (e) { toast(e.message, 'err'); });
            });
            tr.appendChild(el('td', {}, '<span class="mono">' + esc(inst.id || '-') + '</span>'));
            tr.appendChild(el('td', { text: inst.name || '-' }));
            tr.appendChild(el('td', {}, '<span class="badge ' + instanceStatusClass(inst.status) + '">' + esc(inst.status || 'unknown') + '</span>'));
            tr.appendChild(el('td', { text: inst.channelId || '-' }));
            tr.appendChild(el('td', { text: fmtAgo(inst.updatedAt || inst.createdAt) }));
            var ops = el('td', {}, '<div class="icon-btn-row"></div>');
            ops.firstChild.appendChild(btnIEdit);
            ops.firstChild.appendChild(btnIDel);
            tr.appendChild(ops);
            tb.appendChild(tr);
          });
          tWrap.appendChild(it);
          item.appendChild(tWrap);
        }
        stack.appendChild(item);
      });
      wrap.appendChild(stack);
    }

    wrap.querySelector('#newNode').addEventListener('click', function () { openNodeEdit(null); });
    setView(wrap);
  }

  function openNodeEdit(n) {
    var editing = !!n;
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑节点' : '新建节点') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="ndName" type="text" value="' + esc(editing ? n.name : '') + '"></div>' +
      '<div class="field"><label>地址</label><input id="ndAddr" type="text" value="' + esc(editing ? (n.address || '') : '') + '"></div>' +
      '<div class="field"><label>状态</label><select id="ndStatus"><option value="online">在线</option><option value="offline">离线</option><option value="unknown">未知</option></select></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="ndEnabled" type="checkbox"' + ((!editing) || (editing && n.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="form-actions"><button id="ndCancel" class="btn" type="button">取消</button>' +
      '<button id="ndSave" class="btn btn-primary" type="button">保存</button></div>');

    if (editing && n.status) { m.querySelector('#ndStatus').value = n.status; }
    m.querySelector('#ndCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#ndSave').addEventListener('click', function () {
      var name = m.querySelector('#ndName').value.trim();
      if (!name) { toast('必须填写节点名称', 'err'); return; }
      var body = {
        name: name,
        address: m.querySelector('#ndAddr').value.trim() || undefined,
        status: m.querySelector('#ndStatus').value || undefined,
        enabled: m.querySelector('#ndEnabled').checked
      };
      var p = editing
        ? post('/node/update', Object.assign({ id: n.id }, body))
        : post('/node', body);
      p.then(function () {
        toast(editing ? '节点已更新' : '节点已创建', 'ok');
        m.remove(); renderNodes();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  function openInstanceEdit(inst, nodes, presetNodeId) {
    var editing = !!inst;
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑实例' : '新建实例') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>节点</label><select id="inNode"></select></div>' +
      '<div class="field"><label>名称</label><input id="inName" type="text" value="' + esc(editing ? inst.name : '') + '"></div>' +
      '<div class="field"><label>状态</label><select id="inStatus"><option value="running">运行中</option><option value="stopped">已停止</option><option value="unknown">未知</option></select></div>' +
      '<div class="field"><label>频道 ID（可选）</label><input id="inChannel" type="text" value="' + esc(editing ? (inst.channelId || '') : '') + '"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="inCancel" class="btn" type="button">取消</button>' +
      '<button id="inSave" class="btn btn-primary" type="button">保存</button></div>');

    var nodeSel = m.querySelector('#inNode');
    var chosen = editing ? (inst.nodeId || '') : (presetNodeId || '');
    var found = false;
    nodeSel.appendChild(el('option', { value: '', text: '-- 请选择节点 --' }));
    nodes.forEach(function (nd) {
      if (nd.id === chosen) { found = true; }
      nodeSel.appendChild(el('option', { value: nd.id, text: nd.name || nd.id }));
    });
    if (chosen && !found) {
      nodeSel.appendChild(el('option', { value: chosen, text: '（缺失节点：' + chosen + '）' }));
    }
    nodeSel.value = chosen;

    if (editing && inst.status) { m.querySelector('#inStatus').value = inst.status; }
    m.querySelector('#inCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#inSave').addEventListener('click', function () {
      var nodeId = nodeSel.value;
      if (!nodeId) { toast('请选择节点', 'err'); return; }
      var body = {
        nodeId: nodeId,
        name: m.querySelector('#inName').value.trim() || undefined,
        status: m.querySelector('#inStatus').value || undefined,
        channelId: m.querySelector('#inChannel').value.trim() || undefined
      };
      var p = editing
        ? post('/instance/update', Object.assign({ id: inst.id }, body))
        : post('/instance', body);
      p.then(function () {
        toast(editing ? '实例已更新' : '实例已创建', 'ok');
        m.remove(); renderNodes();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Remote commands
   * ------------------------------------------------------------------ */
  var COMMAND_ACTIONS = ['start', 'stop', 'restart'];

  function renderRemoteCommands() {
    setView(loadingView());
    Promise.all([get('/remote-command'), get('/node'), get('/instance')]).then(function (res) {
      setConn(true);
      var commands = listOf(res[0], ['commands', 'items', 'list']);
      var nodes = listOf(res[1], ['nodes', 'items', 'list']);
      var instances = listOf(res[2], ['instances', 'items', 'list']);
      drawRemoteCommands(commands, nodes, instances);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderRemoteCommands));
    });
  }

  function commandStatusClass(s) {
    s = String(s || '').toLowerCase();
    if (s === 'success') { return 'badge-ok'; }
    if (s === 'failed') { return 'badge-crit'; }
    if (s === 'sent') { return 'badge-info'; }
    return 'badge-neutral';
  }

  function drawRemoteCommands(commands, nodes, instances) {
    var nodeById = {};
    nodes.forEach(function (n) { nodeById[n.id] = n; });
    var instById = {};
    instances.forEach(function (i) { instById[i.id] = i; });

    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">远程命令（' + commands.length + '）</h2>' +
      '<div class="cluster">' +
      '<button id="newCmd" class="btn btn-primary" type="button">新建命令</button>' +
      '<button id="purgeCmd" class="btn" type="button">清理终端</button>' +
      '</div>'));

    if (commands.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无远程命令。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>创建时间</th><th>节点</th><th>实例</th><th>动作</th><th>状态</th><th>错误</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      commands.forEach(function (c) {
        var tr = el('tr', {});
        var st = (c.status || 'pending').toLowerCase();
        var node = nodeById[c.nodeId];
        var inst = c.instanceId ? instById[c.instanceId] : null;
        var btnSent = el('button', { class: 'btn btn-sm', type: 'button', text: '已发送' });
        var btnOk = el('button', { class: 'btn btn-sm', type: 'button', text: '成功' });
        var btnFail = el('button', { class: 'btn btn-sm', type: 'button', text: '失败' });
        btnSent.addEventListener('click', function () {
          post('/remote-command/' + encodeURIComponent(c.id) + '/sent', {}).then(function () {
            toast('命令已标记为已发送', 'ok'); renderRemoteCommands();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnOk.addEventListener('click', function () {
          post('/remote-command/' + encodeURIComponent(c.id) + '/success', {}).then(function () {
            toast('命令已标记为成功', 'ok'); renderRemoteCommands();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnFail.addEventListener('click', function () { openRemoteCommandFailed(c); });
        tr.appendChild(el('td', { text: fmtTime(c.createdAt || c.created_at) }));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc((node && node.name) || c.nodeId || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc((inst && inst.name) || c.instanceId || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(c.action || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + commandStatusClass(st) + '">' + esc(st) + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(c.error || '-') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        if (st === 'pending') { ops.firstChild.appendChild(btnSent); }
        if (st === 'pending' || st === 'sent') {
          ops.firstChild.appendChild(btnOk);
          ops.firstChild.appendChild(btnFail);
        }
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newCmd').addEventListener('click', function () { openRemoteCommandAdd(nodes, instances); });
    wrap.querySelector('#purgeCmd').addEventListener('click', openRemoteCommandPurge);
    setView(wrap);
  }

  function openRemoteCommandAdd(nodes, instances) {
    var m = el('div', { class: 'modal' },
      '<h3>新建远程命令</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>节点</label><select id="rcNode"></select></div>' +
      '<div class="field"><label>实例（可选）</label><select id="rcInst"></select></div>' +
      '<div class="field"><label>动作</label><select id="rcAction"></select></div>' +
      '</div>' +
      '<div class="form-actions"><button id="rcCancel" class="btn" type="button">取消</button>' +
      '<button id="rcSave" class="btn btn-primary" type="button">发送</button></div>');

    var nodeSel = m.querySelector('#rcNode');
    nodeSel.appendChild(el('option', { value: '', text: '-- 请选择节点 --' }));
    nodes.forEach(function (n) {
      nodeSel.appendChild(el('option', { value: n.id, text: n.name || n.id }));
    });
    var instSel = m.querySelector('#rcInst');
    function syncInstances() {
      var prev = instSel.value;
      var nodeId = nodeSel.value;
      instSel.innerHTML = '';
      instSel.appendChild(el('option', { value: '', text: '-- 无 --' }));
      var list = nodeId ? instances.filter(function (i) { return i.nodeId === nodeId; }) : instances;
      list.forEach(function (i) {
        instSel.appendChild(el('option', { value: i.id, text: i.name || i.id }));
      });
      if (prev) { instSel.value = prev; }
    }
    nodeSel.addEventListener('change', syncInstances);
    syncInstances();

    var actSel = m.querySelector('#rcAction');
    COMMAND_ACTIONS.forEach(function (a) {
      actSel.appendChild(el('option', { value: a, text: a }));
    });

    m.querySelector('#rcCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#rcSave').addEventListener('click', function () {
      if (!nodeSel.value) { toast('请选择节点', 'err'); return; }
      var save = m.querySelector('#rcSave');
      save.disabled = true;
      save.textContent = '发送中 ...';
      post('/remote-command', {
        nodeId: nodeSel.value,
        instanceId: instSel.value || undefined,
        action: actSel.value
      }).then(function () {
        toast('远程命令已创建', 'ok'); m.remove(); renderRemoteCommands();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { save.disabled = false; save.textContent = '发送'; });
    });

    modalScrim(m);
  }

  function openRemoteCommandFailed(c) {
    var m = el('div', { class: 'modal' },
      '<h3>标记命令为失败</h3>' +
      '<div class="field"><label>错误说明（可选）</label><input id="rcfNote" type="text" value="' + esc(c.error || '') + '"></div>' +
      '<div class="form-actions"><button id="rcfCancel" class="btn" type="button">取消</button>' +
      '<button id="rcfSave" class="btn btn-danger" type="button">标记失败</button></div>');
    m.querySelector('#rcfCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#rcfSave').addEventListener('click', function () {
      post('/remote-command/' + encodeURIComponent(c.id) + '/failed', {
        error: m.querySelector('#rcfNote').value.trim() || undefined
      }).then(function () {
        toast('命令已标记为失败', 'ok'); m.remove(); renderRemoteCommands();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  function openRemoteCommandPurge() {
    var m = el('div', { class: 'modal' },
      '<h3>清理终端命令</h3>' +
      '<div class="field"><label>保留的最大命令条数</label><input id="rcKeep" type="number" min="0" step="1" placeholder="e.g. 50"></div>' +
      '<div class="muted">早于最新 N 条的旧命令将被永久删除。</div>' +
      '<div class="form-actions"><button id="rcPCancel" class="btn" type="button">取消</button>' +
      '<button id="rcPSave" class="btn btn-danger" type="button">清理</button></div>');
    m.querySelector('#rcPCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#rcPSave').addEventListener('click', function () {
      var n = Number(m.querySelector('#rcKeep').value);
      if (!(n >= 0) || Math.floor(n) !== n) { toast('请输入非负整数', 'err'); return; }
      post('/remote-command/purge', { maxKeep: n }).then(function () {
        toast('终端命令已清理', 'ok'); m.remove(); renderRemoteCommands();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Config snapshots
   * ------------------------------------------------------------------ */
  function renderSnapshots() {
    setView(loadingView());
    get('/config-snapshot').then(function (data) {
      setConn(true);
      var snapshots = listOf(data, ['snapshots', 'items', 'list']);
      drawSnapshots(snapshots);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderSnapshots));
    });
  }

  function shortHash(h) {
    if (!h) { return '-'; }
    h = String(h);
    return h.length > 16 ? h.slice(0, 16) + '...' : h;
  }

  function drawSnapshots(snapshots) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">配置快照（' + snapshots.length + '）</h2>' +
      '<button id="newSnap" class="btn btn-primary" type="button">创建快照</button>'));

    if (snapshots.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无配置快照。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>创建时间</th><th>操作人</th><th>描述</th><th>数据哈希</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      snapshots.forEach(function (s) {
        var tr = el('tr', {});
        var btnRestore = el('button', { class: 'btn btn-sm', type: 'button', text: '恢复' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnRestore.addEventListener('click', function () {
          if (!confirm('确定从 ' + fmtTime(s.createdAt) + ' 的快照恢复配置吗？')) { return; }
          post('/config-snapshot/' + encodeURIComponent(s.id) + '/restore', {}).then(function () {
            toast('快照已恢复', 'ok'); renderSnapshots();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除 ' + fmtTime(s.createdAt) + ' 的快照吗？')) { return; }
          del('/config-snapshot/' + encodeURIComponent(s.id)).then(function () {
            toast('快照已删除', 'ok'); renderSnapshots();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', { text: fmtTime(s.createdAt || s.created_at) }));
        tr.appendChild(el('td', { text: s.operator || '-' }));
        tr.appendChild(el('td', { text: s.description || '-' }));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(shortHash(s.dataHash)) + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnRestore);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newSnap').addEventListener('click', openSnapshotAdd);
    setView(wrap);
  }

  function openSnapshotAdd() {
    var m = el('div', { class: 'modal' },
      '<h3>创建配置快照</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>操作人（可选）</label><input id="snOp" type="text"></div>' +
      '<div class="field"><label>描述（可选）</label><input id="snDesc" type="text"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="snCancel" class="btn" type="button">取消</button>' +
      '<button id="snSave" class="btn btn-primary" type="button">创建</button></div>');
    m.querySelector('#snCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#snSave').addEventListener('click', function () {
      var save = m.querySelector('#snSave');
      save.disabled = true;
      save.textContent = '创建中 ...';
      post('/config-snapshot', {
        operator: m.querySelector('#snOp').value.trim() || undefined,
        description: m.querySelector('#snDesc').value.trim() || undefined
      }).then(function () {
        toast('快照已创建', 'ok'); m.remove(); renderSnapshots();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { save.disabled = false; save.textContent = '创建'; });
    });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Config templates
   * ------------------------------------------------------------------ */
  function renderTemplates() {
    setView(loadingView());
    get('/config-template').then(function (data) {
      setConn(true);
      var templates = listOf(data, ['templates', 'items', 'list']);
      drawTemplates(templates);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderTemplates));
    });
  }

  function bodyToText(b) {
    if (b == null) { return ''; }
    if (typeof b === 'string') { return b; }
    try { return JSON.stringify(b, null, 2); } catch (_) { return String(b); }
  }

  function bodyPreview(b) {
    var txt = bodyToText(b).replace(/\s+/g, ' ').trim();
    if (!txt) { return '-'; }
    return txt.length > 120 ? txt.slice(0, 120) + ' ...' : txt;
  }

  function drawTemplates(templates) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">配置模板（' + templates.length + '）</h2>' +
      '<button id="newTpl" class="btn btn-primary" type="button">新建模板</button>'));

    if (templates.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无配置模板。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>类型</th><th>内容</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      templates.forEach(function (tp) {
        var tr = el('tr', {});
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: tp.enabled ? '停用' : '启用' });
        var btnExpand = el('button', { class: 'btn btn-sm', type: 'button', text: '展开' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/config-template/' + encodeURIComponent(tp.id) + '/enabled', { enabled: !tp.enabled }).then(function () {
            toast('模板' + (tp.enabled ? '已停用' : '已启用'), 'ok'); renderTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnExpand.addEventListener('click', function () { openTemplateExpand(tp); });
        btnEdit.addEventListener('click', function () { openTemplateEdit(tp); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除配置模板 "' + tp.name + '" 吗？')) { return; }
          del('/config-template/' + encodeURIComponent(tp.id)).then(function () {
            toast('模板已删除', 'ok'); renderTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(tp.name || '-') + '</span>' +
          '<span class="sub">' + esc(tp.id) + '</span></div>'));
        tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(tp.type || '-') + '</span>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(bodyPreview(tp.body)) + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + (tp.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (tp.enabled ? 'enabled' : 'disabled') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnExpand);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newTpl').addEventListener('click', function () { openTemplateEdit(null); });
    setView(wrap);
  }

  function openTemplateEdit(tp) {
    var editing = !!tp;
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑配置模板' : '新建配置模板') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="tplName" type="text" value="' + esc(editing ? tp.name : '') + '"></div>' +
      '<div class="field"><label>类型</label><input id="tplType" type="text" value="' + esc(editing ? (tp.type || '') : '') + '" placeholder="例如 playlist"></div>' +
      '</div>' +
      '<div class="field mt"><label>内容（JSON）</label><textarea id="tplBody" rows="10" spellcheck="false"></textarea></div>' +
      '<div class="cluster mt"><label class="check"><input id="tplEnabled" type="checkbox"' + ((!editing) || (editing && tp.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="form-actions"><button id="tplCancel" class="btn" type="button">取消</button>' +
      '<button id="tplSave" class="btn btn-primary" type="button">保存</button></div>');

    var bodyIn = m.querySelector('#tplBody');
    bodyIn.value = editing ? bodyToText(tp.body) : '{}';

    m.querySelector('#tplCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#tplSave').addEventListener('click', function () {
      var name = m.querySelector('#tplName').value.trim();
      if (!name) { toast('必须填写模板名称', 'err'); return; }
      var parsed;
      try { parsed = JSON.parse(bodyIn.value); }
      catch (e) { toast('内容必须是合法的 JSON', 'err'); return; }
      var body = {
        name: name,
        type: m.querySelector('#tplType').value.trim() || undefined,
        body: parsed,
        enabled: m.querySelector('#tplEnabled').checked
      };
      var p = editing
        ? post('/config-template/update', Object.assign({ id: tp.id }, body))
        : post('/config-template', body);
      p.then(function () {
        toast(editing ? '模板已更新' : '模板已创建', 'ok');
        m.remove(); renderTemplates();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  var RESULT_BOX_STYLE = 'background:var(--bg-elev-2);border:1px solid var(--border);border-radius:6px;padding:10px;font-size:12.5px;max-height:240px;overflow:auto;';

  function openTemplateExpand(tp) {
    var params = [];
    var m = el('div', { class: 'modal' },
      '<h3>展开模板 - ' + esc(tp.name || tp.id) + '</h3>' +
      '<div class="field mt"><label>参数（键 / 值）</label>' +
      '<div class="cluster"><input id="teKeyAdd" type="text" placeholder="键" style="flex:1;min-width:120px">' +
      '<input id="teValAdd" type="text" placeholder="值" style="flex:1;min-width:120px">' +
      '<button id="teAddBtn" class="btn" type="button">+ 添加</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>键</th><th>值</th><th></th></tr></thead><tbody id="teParamList"></tbody></table></div>' +
      '<div class="form-actions"><button id="teExpand" class="btn btn-primary" type="button">展开</button>' +
      '<button id="teClose" class="btn" type="button">关闭</button></div>' +
      '<div class="field mt"><label>展开后的内容</label>' +
      '<pre id="teResult" style="' + RESULT_BOX_STYLE + 'margin:0;white-space:pre-wrap;overflow-wrap:anywhere;">点击「展开」预览结果。</pre></div>');

    var listBody = m.querySelector('#teParamList');
    function drawParams() {
      listBody.innerHTML = '';
      params.forEach(function (pr, idx) {
        var tr = el('tr', {});
        var keyIn = el('input', { type: 'text', value: pr.k, style: 'width:100%' });
        var valIn = el('input', { type: 'text', value: pr.v, style: 'width:100%' });
        keyIn.addEventListener('input', function () { pr.k = keyIn.value; });
        valIn.addEventListener('input', function () { pr.v = valIn.value; });
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { params.splice(idx, 1); drawParams(); });
        var ktd = el('td', {}, undefined);
        ktd.appendChild(keyIn);
        var vtd = el('td', {}, undefined);
        vtd.appendChild(valIn);
        tr.appendChild(ktd);
        tr.appendChild(vtd);
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        listBody.appendChild(tr);
      });
    }
    drawParams();

    function addParam() {
      var k = m.querySelector('#teKeyAdd').value.trim();
      var v = m.querySelector('#teValAdd').value;
      if (!k) { toast('必须填写参数键', 'err'); return; }
      var dup = params.some(function (pr) { return pr.k === k; });
      if (dup) { toast('该键已在列表中', 'err'); return; }
      params.push({ k: k, v: v });
      m.querySelector('#teKeyAdd').value = '';
      m.querySelector('#teValAdd').value = '';
      drawParams();
    }
    m.querySelector('#teAddBtn').addEventListener('click', addParam);
    m.querySelector('#teKeyAdd').addEventListener('keydown', function (e) { if (e.key === 'Enter') { addParam(); } });

    var resultPre = m.querySelector('#teResult');
    m.querySelector('#teExpand').addEventListener('click', function () {
      var out = {};
      var dupKey = false;
      params.forEach(function (pr) {
        var k = pr.k.trim();
        if (!k) { return; }
        if (Object.prototype.hasOwnProperty.call(out, k)) { dupKey = true; return; }
        out[k] = pr.v;
      });
      if (dupKey) { toast('不允许出现重复的参数键', 'err'); return; }
      var btn = m.querySelector('#teExpand');
      btn.disabled = true;
      btn.textContent = '展开中 ...';
      post('/config-template/' + encodeURIComponent(tp.id) + '/expand', { params: out }).then(function (res) {
        var body = (unwrap(res) || {}).body;
        if (body == null) {
          var r = objOf(res, ['result', 'data']);
          if (r && r.body != null) { body = r.body; }
        }
        resultPre.textContent = bodyToText(body);
      }).catch(function (e) {
        resultPre.textContent = e.message;
      }).finally(function () { btn.disabled = false; btn.textContent = '展开'; });
    });
    m.querySelector('#teClose').addEventListener('click', function () { m.remove(); });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Industry templates
   * ------------------------------------------------------------------ */
  function renderIndustryTemplates() {
    setView(loadingView());
    get('/industry-template').then(function (data) {
      setConn(true);
      var templates = listOf(data, ['templates', 'items', 'list']);
      drawIndustryTemplates(templates);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderIndustryTemplates));
    });
  }

  function arrLen(v) {
    return Array.isArray(v) ? v.length : (v && typeof v === 'object' ? Object.keys(v).length : 0);
  }

  function drawIndustryTemplates(templates) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">行业模板（' + templates.length + '）</h2>' +
      '<button id="newIt" class="btn btn-primary" type="button">新建模板</button>'));

    if (templates.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无行业模板。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>节目单</th><th>占位符</th><th>场景</th><th>定时任务</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      templates.forEach(function (tp) {
        var tr = el('tr', {});
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: tp.enabled ? '停用' : '启用' });
        var btnDeploy = el('button', { class: 'btn btn-sm', type: 'button', text: '部署' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/industry-template/' + encodeURIComponent(tp.id) + '/enabled', { enabled: !tp.enabled }).then(function () {
            toast('模板' + (tp.enabled ? '已停用' : '已启用'), 'ok'); renderIndustryTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnDeploy.addEventListener('click', function () { openIndustryTemplateDeploy(tp); });
        btnEdit.addEventListener('click', function () { openIndustryTemplateEdit(tp); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除行业模板 "' + tp.name + '" 吗？')) { return; }
          del('/industry-template/' + encodeURIComponent(tp.id)).then(function () {
            toast('模板已删除', 'ok'); renderIndustryTemplates();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(tp.name || '-') + '</span>' +
          (tp.description ? '<span class="sub">' + esc(tp.description) + '</span>' : '') + '</div>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(tp.playlistName || '-') + '</span>'));
        tr.appendChild(el('td', { text: String(arrLen(tp.mediaPlaceholders)) }));
        tr.appendChild(el('td', { text: String(arrLen(tp.sceneKinds)) }));
        tr.appendChild(el('td', {}, tp.task
          ? '<span class="badge badge-info">' + esc(tp.task.name || 'task') + '</span>'
          : '<span class="muted">-</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + (tp.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (tp.enabled ? 'enabled' : 'disabled') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnDeploy);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newIt').addEventListener('click', function () { openIndustryTemplateEdit(null); });
    setView(wrap);
  }

  function openIndustryTemplateDeploy(tp) {
    var params = [];
    var m = el('div', { class: 'modal' },
      '<h3>部署 - ' + esc(tp.name || tp.id) + '</h3>' +
      '<div class="field mt"><label>参数（键 / 值）</label>' +
      '<div class="cluster"><input id="idKeyAdd" type="text" placeholder="键" style="flex:1;min-width:120px">' +
      '<input id="idValAdd" type="text" placeholder="值" style="flex:1;min-width:120px">' +
      '<button id="idAddBtn" class="btn" type="button">+ 添加</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>键</th><th>值</th><th></th></tr></thead><tbody id="idParamList"></tbody></table></div>' +
      '<div class="form-actions"><button id="idDeploy" class="btn btn-primary" type="button">部署</button>' +
      '<button id="idClose" class="btn" type="button">关闭</button></div>' +
      '<div class="field mt"><label>结果</label>' +
      '<div id="idResult" style="' + RESULT_BOX_STYLE + '">点击「部署」以部署模板。</div></div>');

    var listBody = m.querySelector('#idParamList');
    function drawParams() {
      listBody.innerHTML = '';
      params.forEach(function (pr, idx) {
        var tr = el('tr', {});
        var keyIn = el('input', { type: 'text', value: pr.k, style: 'width:100%' });
        var valIn = el('input', { type: 'text', value: pr.v, style: 'width:100%' });
        keyIn.addEventListener('input', function () { pr.k = keyIn.value; });
        valIn.addEventListener('input', function () { pr.v = valIn.value; });
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { params.splice(idx, 1); drawParams(); });
        var ktd = el('td', {}, undefined);
        ktd.appendChild(keyIn);
        var vtd = el('td', {}, undefined);
        vtd.appendChild(valIn);
        tr.appendChild(ktd);
        tr.appendChild(vtd);
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        listBody.appendChild(tr);
      });
    }
    drawParams();

    function addParam() {
      var k = m.querySelector('#idKeyAdd').value.trim();
      var v = m.querySelector('#idValAdd').value;
      if (!k) { toast('必须填写参数键', 'err'); return; }
      var dup = params.some(function (pr) { return pr.k === k; });
      if (dup) { toast('该键已在列表中', 'err'); return; }
      params.push({ k: k, v: v });
      m.querySelector('#idKeyAdd').value = '';
      m.querySelector('#idValAdd').value = '';
      drawParams();
    }
    m.querySelector('#idAddBtn').addEventListener('click', addParam);
    m.querySelector('#idKeyAdd').addEventListener('keydown', function (e) { if (e.key === 'Enter') { addParam(); } });

    m.querySelector('#idDeploy').addEventListener('click', function () {
      var out = {};
      var dupKey = false;
      params.forEach(function (pr) {
        var k = pr.k.trim();
        if (!k) { return; }
        if (Object.prototype.hasOwnProperty.call(out, k)) { dupKey = true; return; }
        out[k] = pr.v;
      });
      if (dupKey) { toast('不允许出现重复的参数键', 'err'); return; }
      var btn = m.querySelector('#idDeploy');
      btn.disabled = true;
      btn.textContent = '部署中 ...';
      post('/industry-template/' + encodeURIComponent(tp.id) + '/deploy', { params: out }).then(function (res) {
        var r = objOf(res, ['result', 'data']);
        var box = m.querySelector('#idResult');
        box.innerHTML = '';
        var kv = el('div', { class: 'kv' });
        kv.appendChild(el('dt', { text: '节目单 ID' }));
        kv.appendChild(el('dd', { class: 'mono', text: r.playlistId || '-' }));
        kv.appendChild(el('dt', { text: '场景模板' }));
        kv.appendChild(el('dd', {}, '<span class="mono">' + esc((r.sceneTemplateIds || []).join(', ') || '-') + '</span>'));
        kv.appendChild(el('dt', { text: '定时任务 ID' }));
        kv.appendChild(el('dd', { class: 'mono', text: r.taskId || '-' }));
        box.appendChild(kv);
        toast('模板已部署', 'ok');
      }).catch(function (e) {
        var box = m.querySelector('#idResult');
        box.innerHTML = '';
        box.appendChild(el('div', { class: 'muted', text: e.message }));
      }).finally(function () { btn.disabled = false; btn.textContent = '部署'; });
    });
    m.querySelector('#idClose').addEventListener('click', function () { m.remove(); });
    modalScrim(m);
  }

  function openIndustryTemplateEdit(tp) {
    var editing = !!tp;
    var kinds = editing && Array.isArray(tp.sceneKinds) ? tp.sceneKinds.slice() : [];
    var task = editing && tp.task ? {
      name: tp.task.name || '',
      type: tp.task.type || 'interval',
      interval: tp.task.interval || '',
      cron: tp.task.cron || '',
      enabled: !!tp.task.enabled
    } : { name: '', type: 'interval', interval: '', cron: '', enabled: true };

    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑行业模板' : '新建行业模板') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="itName" type="text" value="' + esc(editing ? tp.name : '') + '"></div>' +
      '<div class="field"><label>节目单名称</label><input id="itPlaylist" type="text" value="' + esc(editing ? (tp.playlistName || '') : '') + '"></div>' +
      '<div class="field" style="grid-column:1/-1"><label>描述</label><input id="itDesc" type="text" value="' + esc(editing ? (tp.description || '') : '') + '"></div>' +
      '<div class="field" style="grid-column:1/-1"><label>媒体占位符（逗号分隔）</label><input id="itPlaceholders" type="text" value="' + esc(editing ? (tp.mediaPlaceholders || []).join(', ') : '') + '" placeholder="媒体 id, ${key}, ..."></div>' +
      '</div>' +
      '<div class="field mt"><label>场景类型</label><div id="itKinds"></div></div>' +
      '<div class="cluster mt"><label class="check"><input id="itTaskOn" type="checkbox"' + (editing && tp.task ? ' checked' : '') + '> 包含定时任务</label></div>' +
      '<div id="itTaskFields" class="mt" style="display:none">' +
      '<div class="form-grid">' +
      '<div class="field"><label>任务名称</label><input id="itTaskName" type="text" value="' + esc(task.name) + '"></div>' +
      '<div class="field"><label>任务类型</label><select id="itTaskType"><option value="interval">间隔</option><option value="cron">定时</option></select></div>' +
      '<div class="field" id="itTaskIntervalField"><label>间隔（秒）</label><input id="itTaskInterval" type="number" min="1" value="' + esc(String(task.interval)) + '"></div>' +
      '<div class="field" id="itTaskCronField" style="display:none"><label>定时表达式（5 位）</label><input id="itTaskCron" type="text" placeholder="0 * * * *" value="' + esc(task.cron) + '"></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="itTaskEnabled" type="checkbox"' + (task.enabled ? ' checked' : '') + '> 定时任务启用</label></div>' +
      '</div>' +
      '<div class="cluster mt"><label class="check"><input id="itEnabled" type="checkbox"' + ((!editing) || (editing && tp.enabled) ? ' checked' : '') + '> 启用</label></div>' +
      '<div class="form-actions"><button id="itCancel" class="btn" type="button">取消</button>' +
      '<button id="itSave" class="btn btn-primary" type="button">保存</button></div>');

    var kindHost = m.querySelector('#itKinds');
    SCENE_KINDS.forEach(function (k) {
      kindHost.appendChild(el('label', { class: 'check' },
        '<input type="checkbox" data-kind="' + k + '"' + (kinds.indexOf(k) !== -1 ? ' checked' : '') + '> ' + esc(k)));
    });

    var taskTypeSel = m.querySelector('#itTaskType');
    taskTypeSel.value = task.type;
    function syncTaskType() {
      var isCron = taskTypeSel.value === 'cron';
      m.querySelector('#itTaskIntervalField').style.display = isCron ? 'none' : '';
      m.querySelector('#itTaskCronField').style.display = isCron ? '' : 'none';
    }
    taskTypeSel.addEventListener('change', syncTaskType);
    syncTaskType();

    var taskOn = m.querySelector('#itTaskOn');
    var taskFields = m.querySelector('#itTaskFields');
    function syncTask() {
      taskFields.style.display = taskOn.checked ? '' : 'none';
    }
    taskOn.addEventListener('change', syncTask);
    syncTask();

    m.querySelector('#itCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#itSave').addEventListener('click', function () {
      var name = m.querySelector('#itName').value.trim();
      var playlistName = m.querySelector('#itPlaylist').value.trim();
      if (!name) { toast('必须填写模板名称', 'err'); return; }
      if (!playlistName) { toast('必须填写节目单名称', 'err'); return; }
      var sceneKinds = [];
      kindHost.querySelectorAll('input[data-kind]').forEach(function (cb) {
        if (cb.checked) { sceneKinds.push(cb.dataset.kind); }
      });
      var placeholders = m.querySelector('#itPlaceholders').value.split(',')
        .map(function (s) { return s.trim(); })
        .filter(function (s) { return s; });
      var body = {
        name: name,
        description: m.querySelector('#itDesc').value.trim() || undefined,
        playlistName: playlistName,
        mediaPlaceholders: placeholders,
        sceneKinds: sceneKinds,
        enabled: m.querySelector('#itEnabled').checked
      };
      if (taskOn.checked) {
        var tname = m.querySelector('#itTaskName').value.trim();
        if (!tname) { toast('必须填写任务名称', 'err'); return; }
        var isCron = taskTypeSel.value === 'cron';
        body.task = {
          name: tname,
          type: isCron ? 'cron' : 'interval',
          interval: isCron ? 0 : (Number(m.querySelector('#itTaskInterval').value) || 0),
          cron: isCron ? m.querySelector('#itTaskCron').value.trim() : '',
          enabled: m.querySelector('#itTaskEnabled').checked
        };
      }
      var p = editing
        ? post('/industry-template/update', Object.assign({ id: tp.id }, body))
        : post('/industry-template', body);
      p.then(function () {
        toast(editing ? '模板已更新' : '模板已创建', 'ok');
        m.remove(); renderIndustryTemplates();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Smart rules
   * ------------------------------------------------------------------ */
  function renderSmartRules() {
    setView(loadingView());
    get('/smart-rule').then(function (data) {
      setConn(true);
      var rules = listOf(data, ['rules', 'items', 'list']);
      drawSmartRules(rules);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderSmartRules));
    });
  }

  function slotText(s) {
    var p = function (x) { return (x < 10 ? '0' : '') + x; };
    return p(s.startHour) + ':00-' + p(s.endHour) + ':00';
  }

  function slotsText(slots) {
    if (!Array.isArray(slots) || !slots.length) { return 'all day'; }
    return slots.map(slotText).join(', ');
  }

  function tagsText(tags) {
    if (!Array.isArray(tags) || !tags.length) { return '-'; }
    return tags.join(', ');
  }

  function csvIds(v) {
    return String(v || '').split(',').map(function (s) { return s.trim(); }).filter(function (s) { return s; });
  }

  function drawSmartRules(rules) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">智能规则（' + rules.length + ')</h2>' +
      '<button id="newSr" class="btn btn-primary" type="button">新建规则</button>'));

    if (rules.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无智能规则。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>名称</th><th>时间段</th><th>标签</th><th>避免重复</th><th>最大时长</th><th>最大条数</th><th>状态</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      rules.forEach(function (r) {
        var tr = el('tr', {});
        var btnEn = el('button', { class: 'btn btn-sm', type: 'button', text: r.enabled ? '禁用' : '启用' });
        var btnGen = el('button', { class: 'btn btn-sm', type: 'button', text: '生成' });
        var btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
        var btnDel = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '删除' });
        btnEn.addEventListener('click', function () {
          post('/smart-rule/' + encodeURIComponent(r.id) + '/enabled', { enabled: !r.enabled }).then(function () {
            toast('Smart rule ' + (r.enabled ? '已禁用' : '已启用'), 'ok'); renderSmartRules();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        btnGen.addEventListener('click', function () { openSmartRuleGenerate(r); });
        btnEdit.addEventListener('click', function () { openSmartRuleEdit(r); });
        btnDel.addEventListener('click', function () {
          if (!confirm('确定删除智能规则 "' + r.name + '"?')) { return; }
          del('/smart-rule/' + encodeURIComponent(r.id)).then(function () {
            toast('智能规则已删除', 'ok'); renderSmartRules();
          }).catch(function (e) { toast(e.message, 'err'); });
        });
        tr.appendChild(el('td', {}, '<div class="cell"><span>' + esc(r.name || '-') + '</span>' +
          (r.description ? '<span class="sub">' + esc(r.description) + '</span>' : '') + '</div>'));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(slotsText(r.timeSlots)) + '</span>'));
        tr.appendChild(el('td', { text: tagsText(r.tags) }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (r.avoidRepeat ? 'badge-ok' : 'badge-neutral') + '">' + (r.avoidRepeat ? '是' : '否') + '</span>'));
        tr.appendChild(el('td', { text: r.maxDurationSec > 0 ? r.maxDurationSec + 's' : '-' }));
        tr.appendChild(el('td', { text: r.maxItems > 0 ? String(r.maxItems) : '-' }));
        tr.appendChild(el('td', {}, '<span class="badge ' + (r.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (r.enabled ? '已启用' : '已禁用') + '</span>'));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(btnEn);
        ops.firstChild.appendChild(btnGen);
        ops.firstChild.appendChild(btnEdit);
        ops.firstChild.appendChild(btnDel);
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newSr').addEventListener('click', function () { openSmartRuleEdit(null); });
    setView(wrap);
  }

  function openSmartRuleGenerate(r) {
    var recentIds = [];
    var m = el('div', { class: 'modal' },
      '<h3>生成 - ' + esc(r.name || r.id) + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>最近媒体 ID（逗号分隔，可选）</label><input id="sgRecent" type="text" placeholder="id1, id2, ..."></div>' +
      '<div class="field"><label>条数限制（可选）</label><input id="sgLimit" type="number" min="1" placeholder="例如 20"></div>' +
      '</div>' +
      '<div class="form-actions"><button id="sgGo" class="btn btn-primary" type="button">生成</button></div>' +
      '<div class="field mt"><label>结果</label>' +
      '<div id="sgResult" style="' + RESULT_BOX_STYLE + '">点击“生成”来构建一个选择。</div></div>' +
      '<div class="field mt"><label>节目单名称（用于应用）</label><input id="sgPlaylist" type="text" placeholder="生成的节目单"></div>' +
      '<div class="form-actions"><button id="sgApply" class="btn btn-primary" type="button">应用</button>' +
      '<button id="sgClose" class="btn" type="button">关闭</button></div>');

    var resultBox = m.querySelector('#sgResult');
    m.querySelector('#sgGo').addEventListener('click', function () {
      recentIds = csvIds(m.querySelector('#sgRecent').value);
      var limit = Math.floor(Number(m.querySelector('#sgLimit').value));
      var body = {};
      if (recentIds.length) { body.recent = recentIds; }
      if (limit > 0) { body.limit = limit; }
      var btn = m.querySelector('#sgGo');
      btn.disabled = true;
      btn.textContent = '生成中 ...';
      post('/smart-rule/' + encodeURIComponent(r.id) + '/generate', body).then(function (res) {
        var ids = listOf(res, ['mediaIds', 'items', 'list']);
        resultBox.innerHTML = '';
        if (!ids.length) {
          resultBox.appendChild(el('div', { class: 'muted', text: '没有匹配的媒体。' }));
          return;
        }
        var ol = el('ol', { class: 'mono', style: 'margin:0;padding-left:20px;' });
        ids.forEach(function (id) { ol.appendChild(el('li', { text: id })); });
        resultBox.appendChild(ol);
      }).catch(function (e) {
        resultBox.innerHTML = '';
        resultBox.appendChild(el('div', { class: 'muted', text: e.message }));
      }).finally(function () { btn.disabled = false; btn.textContent = '生成'; });
    });

    m.querySelector('#sgApply').addEventListener('click', function () {
      var name = m.querySelector('#sgPlaylist').value.trim();
      if (!name) { toast('节目单名称必填', 'err'); return; }
      var body = { playlistName: name };
      if (recentIds.length) { body.recent = recentIds; }
      var btn = m.querySelector('#sgApply');
      btn.disabled = true;
      btn.textContent = '应用中 ...';
      post('/smart-rule/' + encodeURIComponent(r.id) + '/generate-and-apply', body).then(function (res) {
        var pl = objOf(res, ['playlist', 'data', 'result']);
        toast('节目单 "' + (pl.name || name) + '" created', 'ok');
        m.remove();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btn.disabled = false; btn.textContent = '应用'; });
    });
    m.querySelector('#sgClose').addEventListener('click', function () { m.remove(); });
    modalScrim(m);
  }

  function openSmartRuleEdit(r) {
    var editing = !!r;
    var slots = editing && Array.isArray(r.timeSlots)
      ? r.timeSlots.map(function (s) { return { start: s.startHour != null ? s.startHour : 0, end: s.endHour != null ? s.endHour : 0 }; })
      : [];
    var m = el('div', { class: 'modal' },
      '<h3>' + (editing ? '编辑智能规则' : '新建智能规则') + '</h3>' +
      '<div class="form-grid">' +
      '<div class="field"><label>名称</label><input id="srName" type="text" value="' + esc(editing ? r.name : '') + '"></div>' +
      '<div class="field"><label>标签（逗号分隔）</label><input id="srTags" type="text" value="' + esc(editing ? (r.tags || []).join(', ') : '') + '"></div>' +
      '<div class="field"><label>最大时长（秒，0 表示不限）</label><input id="srMaxDur" type="number" min="0" value="' + (editing && r.maxDurationSec > 0 ? r.maxDurationSec : 0) + '"></div>' +
      '<div class="field"><label>重复回看（0 表示默认）</label><input id="srLookback" type="number" min="0" value="' + (editing && r.repeatLookback > 0 ? r.repeatLookback : 0) + '"></div>' +
      '<div class="field"><label>最大条数（0 表示默认）</label><input id="srMaxItems" type="number" min="0" value="' + (editing && r.maxItems > 0 ? r.maxItems : 0) + '"></div>' +
      '<div class="field" style="grid-column:1/-1"><label>描述</label><input id="srDesc" type="text" value="' + esc(editing ? (r.description || '') : '') + '"></div>' +
      '</div>' +
      '<div class="field mt"><label>时间段（小时，0-23，开始 &lt;= 结束）</label>' +
      '<div class="cluster"><button id="srAddSlot" class="btn btn-sm" type="button">+ 添加时间段</button></div></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>开始</th><th>结束</th><th></th></tr></thead><tbody id="srSlotList"></tbody></table></div>' +
      '<div class="cluster mt">' +
      '<label class="check"><input id="srAvoid" type="checkbox"' + (editing && r.avoidRepeat ? ' checked' : '') + '> 避免重复</label>' +
      '<label class="check"><input id="srEnabled" type="checkbox"' + ((!editing) || (editing && r.enabled) ? ' checked' : '') + '> 启用</label>' +
      '</div>' +
      '<div class="form-actions"><button id="srCancel" class="btn" type="button">取消</button>' +
      '<button id="srSave" class="btn btn-primary" type="button">保存</button></div>');

    var slotBody = m.querySelector('#srSlotList');
    function drawSlots() {
      slotBody.innerHTML = '';
      slots.forEach(function (s, idx) {
        var tr = el('tr', {});
        var startIn = el('input', { type: 'number', min: '0', max: '23', value: String(s.start), style: 'width:100%' });
        var endIn = el('input', { type: 'number', min: '0', max: '23', value: String(s.end), style: 'width:100%' });
        startIn.addEventListener('input', function () { s.start = Number(startIn.value); });
        endIn.addEventListener('input', function () { s.end = Number(endIn.value); });
        var rm = el('button', { class: 'btn btn-danger btn-sm', type: 'button', text: '移除' });
        rm.addEventListener('click', function () { slots.splice(idx, 1); drawSlots(); });
        var std = el('td', {}, undefined);
        std.appendChild(startIn);
        var etd = el('td', {}, undefined);
        etd.appendChild(endIn);
        tr.appendChild(std);
        tr.appendChild(etd);
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        ops.firstChild.appendChild(rm);
        tr.appendChild(ops);
        slotBody.appendChild(tr);
      });
    }
    drawSlots();

    m.querySelector('#srAddSlot').addEventListener('click', function () {
      slots.push({ start: 0, end: 23 });
      drawSlots();
    });

    m.querySelector('#srCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#srSave').addEventListener('click', function () {
      var name = m.querySelector('#srName').value.trim();
      if (!name) { toast('规则名称必填', 'err'); return; }
      var badSlot = slots.some(function (s) {
        return !(s.start >= 0 && s.start <= 23 && s.end >= 0 && s.end <= 23 && s.start <= s.end);
      });
      if (badSlot) { toast('时间段必须为 0-23 小时且开始 <= 结束', 'err'); return; }
      var body = {
        name: name,
        description: m.querySelector('#srDesc').value.trim() || undefined,
        timeSlots: slots,
        tags: csvIds(m.querySelector('#srTags').value),
        maxDurationSec: Number(m.querySelector('#srMaxDur').value) || 0,
        avoidRepeat: m.querySelector('#srAvoid').checked,
        repeatLookback: Number(m.querySelector('#srLookback').value) || 0,
        maxItems: Number(m.querySelector('#srMaxItems').value) || 0,
        enabled: m.querySelector('#srEnabled').checked
      };
      var p = editing
        ? post('/smart-rule/update', Object.assign({ id: r.id }, body))
        : post('/smart-rule', body);
      p.then(function () {
        toast(editing ? '智能规则已更新' : '智能规则已创建', 'ok');
        m.remove(); renderSmartRules();
      }).catch(function (e) { toast(e.message, 'err'); });
    });

    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Analytics (metrics)
   * ------------------------------------------------------------------ */
  function renderAnalytics() {
    setView(loadingView());
    get('/metrics/summary').then(function (data) {
      setConn(true);
      var summary = objOf(data, ['summary', 'data']);
      drawAnalytics(summary);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderAnalytics));
    });
  }

  function pctText(v) {
    if (v == null || v === '') { return '-'; }
    v = Number(v);
    if (isNaN(v)) { return '-'; }
    return (v <= 1 ? (v * 100) : v).toFixed(1) + '%';
  }

  function drawAnalytics(summary) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">数据分析</h2>'));

    var cardGrid = el('div', { class: 'grid grid-cards section' });
    [
      { label: '总播放', value: summary.totalPlays },
      { label: '成功', value: summary.successes },
      { label: '失败', value: summary.failures },
      { label: '成功率', value: pctText(summary.successRate) }
    ].forEach(function (c) {
      cardGrid.appendChild(el('div', { class: 'card stat' },
        '<div class="stat-label">' + esc(c.label) + '</div>' +
        '<div class="stat-value">' + esc(c.value == null ? '-' : c.value) + '</div>'));
    });
    wrap.appendChild(cardGrid);

    var two = el('div', { class: 'grid grid-2 section' });

    var frCard = el('div', { class: 'card' },
      '<h2>按媒体查看失败率</h2>' +
      '<div class="field"><label>媒体 ID</label><input id="anMedia" type="text" placeholder="媒体唯一 ID"></div>' +
      '<div class="form-actions"><button id="anQuery" class="btn btn-primary" type="button">查询</button></div>' +
      '<div class="field mt"><label>结果</label>' +
      '<div id="anFrResult" style="' + RESULT_BOX_STYLE + '">输入媒体 ID 并查询。</div></div>');

    var frBox = frCard.querySelector('#anFrResult');
    frCard.querySelector('#anQuery').addEventListener('click', function () {
      var mid = frCard.querySelector('#anMedia').value.trim();
      if (!mid) { toast('请输入媒体 ID', 'err'); return; }
      get('/metrics/failure-rate?mediaId=' + encodeURIComponent(mid)).then(function (data) {
        var r = objOf(data, ['data', 'result', 'rate']);
        frBox.innerHTML = '';
        var kv = el('div', { class: 'kv' });
        kv.appendChild(el('dt', { text: '失败率' }));
        kv.appendChild(el('dd', { text: pctText(r.rate) }));
        kv.appendChild(el('dt', { text: '播放次数' }));
        kv.appendChild(el('dd', { text: r.plays != null ? String(r.plays) : '-' }));
        kv.appendChild(el('dt', { text: '失败次数' }));
        kv.appendChild(el('dd', { text: r.failures != null ? String(r.failures) : '-' }));
        frBox.appendChild(kv);
      }).catch(function (e) {
        frBox.innerHTML = '';
        frBox.appendChild(el('div', { class: 'muted', text: e.message }));
      });
    });
    two.appendChild(frCard);

    var trCard = el('div', { class: 'card' },
      '<h2>播放趋势</h2>' +
      '<div class="field"><label>天数</label><input id="anDays" type="number" min="1" value="7"></div>' +
      '<div class="form-actions"><button id="anTrend" class="btn btn-primary" type="button">加载</button></div>' +
      '<div class="table-wrap mt"><table class="data"><thead><tr><th>日期</th><th>播放</th></tr></thead><tbody id="anTrendBody"></tbody></table></div>');

    var trendBody = trCard.querySelector('#anTrendBody');
    function loadTrend() {
      var days = Math.max(1, Math.floor(Number(trCard.querySelector('#anDays').value)) || 7);
      trendBody.innerHTML = '';
      trendBody.appendChild(el('tr', {}, '<td colspan="2" class="muted">加载中 ...</td>'));
      get('/metrics/trend?days=' + days).then(function (data) {
        var daysArr = listOf(data, ['days', 'items', 'list']);
        trendBody.innerHTML = '';
        if (!daysArr.length) {
          trendBody.appendChild(el('tr', {}, '<td colspan="2" class="muted">暂无数据。</td>'));
          return;
        }
        daysArr.forEach(function (d) {
          var tr = el('tr', {});
          tr.appendChild(el('td', {}, '<span class="mono">' + esc(d.date || '-') + '</span>'));
          tr.appendChild(el('td', { text: d.count != null ? String(d.count) : '-' }));
          trendBody.appendChild(tr);
        });
      }).catch(function (e) {
        trendBody.innerHTML = '';
        trendBody.appendChild(el('tr', {}, '<td colspan="2" class="muted">' + esc(e.message) + '</td>'));
      });
    }
    trCard.querySelector('#anTrend').addEventListener('click', loadTrend);
    loadTrend();
    two.appendChild(trCard);

    wrap.appendChild(two);
    setView(wrap);
  }

  /* ------------------------------------------------------------------ *
   * Suggestions
   * ------------------------------------------------------------------ */
  function renderSuggestions() {
    setView(loadingView());
    get('/suggestion').then(function (data) {
      setConn(true);
      var suggestions = listOf(data, ['suggestions', 'items', 'list']);
      drawSuggestions(suggestions);
    }).catch(function (err) {
      setConn(false);
      setView(errorView(err, renderSuggestions));
    });
  }

  function suggestionStatusClass(s) {
    s = String(s || '').toLowerCase();
    if (s === 'applied') { return 'badge-ok'; }
    if (s === 'rejected') { return 'badge-neutral'; }
    return 'badge-warn';
  }

  function payloadPreview(p) {
    if (p == null) { return '-'; }
    var txt = (typeof p === 'string') ? p : JSON.stringify(p);
    txt = String(txt).replace(/\s+/g, ' ').trim();
    return txt.length > 120 ? txt.slice(0, 120) + ' ...' : txt;
  }

  function drawSuggestions(suggestions) {
    var wrap = el('div', {});
    wrap.appendChild(el('div', { class: 'row' },
      '<h2 style="margin:0;color:var(--text-dim);text-transform:uppercase;font-size:13px;letter-spacing:.4px;">智能建议（' + suggestions.length + ')</h2>' +
      '<button id="newSug" class="btn btn-primary" type="button">推荐媒体</button>'));

    if (suggestions.length === 0) {
      wrap.appendChild(el('div', { class: 'card' }, emptyView('暂无智能建议。', undefined).innerHTML));
    } else {
      var t = el('table', { class: 'data' },
        '<thead><tr><th>类型</th><th>标题</th><th>负载</th><th>状态</th><th>创建时间</th><th></th></tr></thead><tbody></tbody>');
      var tb = t.querySelector('tbody');
      suggestions.forEach(function (s) {
        var tr = el('tr', {});
        var st = (s.status || 'pending').toLowerCase();
        var btnApprove = el('button', { class: 'btn btn-sm', type: 'button', text: '批准' });
        var btnReject = el('button', { class: 'btn btn-sm', type: 'button', text: '拒绝' });
        btnApprove.addEventListener('click', function () { openSuggestionApprove(s); });
        btnReject.addEventListener('click', function () { openSuggestionReject(s); });
        tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(s.kind || '-') + '</span>'));
        tr.appendChild(el('td', { text: s.title || '-' }));
        tr.appendChild(el('td', {}, '<span class="mono">' + esc(payloadPreview(s.payload)) + '</span>'));
        tr.appendChild(el('td', {}, '<span class="badge ' + suggestionStatusClass(st) + '">' + esc(st) + '</span>'));
        tr.appendChild(el('td', { text: fmtTime(s.createdAt || s.created_at) }));
        var ops = el('td', {}, '<div class="icon-btn-row"></div>');
        if (st === 'pending') {
          ops.firstChild.appendChild(btnApprove);
          ops.firstChild.appendChild(btnReject);
        }
        tr.appendChild(ops);
        tb.appendChild(tr);
      });
      wrap.appendChild(el('div', { class: 'table-wrap' }, undefined));
      wrap.lastChild.appendChild(t);
    }

    wrap.querySelector('#newSug').addEventListener('click', openSuggestionRecommend);
    setView(wrap);
  }

  function openSuggestionApprove(s) {
    var m = el('div', { class: 'modal' },
      '<h3>批准建议</h3>' +
      '<div class="field"><label>节目单名称</label><input id="saPlaylist" type="text" placeholder="要创建的节目单"></div>' +
      '<div class="form-actions"><button id="saCancel" class="btn" type="button">取消</button>' +
      '<button id="saSave" class="btn btn-primary" type="button">批准</button></div>');
    m.querySelector('#saCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#saSave').addEventListener('click', function () {
      var name = m.querySelector('#saPlaylist').value.trim();
      if (!name) { toast('节目单名称必填', 'err'); return; }
      post('/suggestion/' + encodeURIComponent(s.id) + '/approve', { playlistName: name }).then(function () {
        toast('建议已批准', 'ok'); m.remove(); renderSuggestions();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  function openSuggestionReject(s) {
    var m = el('div', { class: 'modal' },
      '<h3>拒绝建议</h3>' +
      '<div class="field"><label>原因（可选）</label><input id="srjReason" type="text"></div>' +
      '<div class="form-actions"><button id="srjCancel" class="btn" type="button">取消</button>' +
      '<button id="srjSave" class="btn btn-danger" type="button">拒绝</button></div>');
    m.querySelector('#srjCancel').addEventListener('click', function () { m.remove(); });
    m.querySelector('#srjSave').addEventListener('click', function () {
      post('/suggestion/' + encodeURIComponent(s.id) + '/reject', {
        reason: m.querySelector('#srjReason').value.trim() || undefined
      }).then(function () {
        toast('建议已拒绝', 'ok'); m.remove(); renderSuggestions();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    modalScrim(m);
  }

  function openSuggestionRecommend() {
    var mediaIds = [];
    var m = el('div', { class: 'modal' },
      '<h3>推荐媒体</h3>' +
      '<div class="field"><label>条数限制（可选）</label><input id="sugLimit" type="number" min="1" placeholder="e.g. 10"></div>' +
      '<div class="form-actions"><button id="sugGo" class="btn btn-primary" type="button">推荐</button></div>' +
      '<div class="field mt"><label>结果</label>' +
      '<div id="sugResult" style="' + RESULT_BOX_STYLE + '">点击“推荐”获取媒体建议。</div></div>' +
      '<div class="form-actions"><button id="sugCreate" class="btn btn-primary" type="button">创建建议</button>' +
      '<button id="sugClose" class="btn" type="button">关闭</button></div>');

    var resultBox = m.querySelector('#sugResult');
    var createBtn = m.querySelector('#sugCreate');
    createBtn.disabled = true;
    m.querySelector('#sugGo').addEventListener('click', function () {
      var limit = Math.floor(Number(m.querySelector('#sugLimit').value));
      var body = {};
      if (limit > 0) { body.limit = limit; }
      var btn = m.querySelector('#sugGo');
      btn.disabled = true;
      btn.textContent = '推荐中 ...';
      post('/suggestion/recommend', body).then(function (res) {
        mediaIds = listOf(res, ['mediaIds', 'items', 'list']);
        resultBox.innerHTML = '';
        if (!mediaIds.length) {
          resultBox.appendChild(el('div', { class: 'muted', text: '暂无推荐。' }));
          return;
        }
        var ol = el('ol', { class: 'mono', style: 'margin:0;padding-left:20px;' });
        mediaIds.forEach(function (id) { ol.appendChild(el('li', { text: id })); });
        resultBox.appendChild(ol);
        createBtn.disabled = false;
      }).catch(function (e) {
        resultBox.innerHTML = '';
        resultBox.appendChild(el('div', { class: 'muted', text: e.message }));
      }).finally(function () { btn.disabled = false; btn.textContent = '推荐'; });
    });

    createBtn.addEventListener('click', function () {
      if (!mediaIds.length) { toast('请先推荐媒体', 'err'); return; }
      var btn = createBtn;
      btn.disabled = true;
      btn.textContent = '创建中 ...';
      // The backend decodes payload as map[string]string and Approve reads
      // payload["media_id"] (one media per playlist), so each recommended
      // media becomes its own suggestion.
      var ids = mediaIds.slice();
      Promise.all(ids.map(function (id) {
        return post('/suggestion', {
          kind: 'media_recommend',
          title: '推荐媒体 ' + id,
          payload: { media_id: String(id) }
        });
      })).then(function () {
        toast('已创建 ' + ids.length + ' 条建议' + (ids.length === 1 ? '' : ''), 'ok');
        m.remove(); renderSuggestions();
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btn.disabled = false; btn.textContent = '创建建议'; });
    });
    m.querySelector('#sugClose').addEventListener('click', function () { m.remove(); });
    modalScrim(m);
  }

  /* ------------------------------------------------------------------ *
   * Authentication (login / logout / session)
   * ------------------------------------------------------------------ */
  function loginError(msg) {
    var box = $('#loginErr');
    if (!box) { return; }
    box.hidden = !msg;
    box.textContent = msg || '';
  }

  function showLogin(msg) {
    state.authed = false;
    state.currentUser = null;
    stopPolling();
    var app = $('#app');
    if (app) { app.hidden = true; }
    var login = $('#login');
    if (login) { login.hidden = false; }
    loginError(msg);
    var pass = $('#loginPass');
    if (pass) { pass.value = ''; }
    var user = $('#loginUser');
    if (user) { user.focus(); }
    document.title = '登录 - KPlayer 控制台';
  }

  function enterApp() {
    state.authed = true;
    var app = $('#app');
    if (app) { app.hidden = false; }
    var login = $('#login');
    if (login) { login.hidden = true; }
    applyRoleUI();
    render();
    startPolling();
  }

  // 无鉴权模式：401 一律忽略，不跳登录页。
  function handleAuthFailure(err) { /* no-op */ }

  function doLogout() {
    var token = getToken();
    stopPolling();
    state.authed = false;
    state.currentUser = null;
    setToken('');
    showLogin();
    if (token) {
      // Best-effort server-side invalidation; failures are ignored since
      // the local token is already gone and the login screen is showing.
      post('/auth/logout', { token: token }).catch(function () {});
    }
  }

  function bindLogin() {
    var form = $('#loginForm');
    if (!form) { return; }
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var username = $('#loginUser').value.trim();
      var password = $('#loginPass').value;
      if (!username || !password) { loginError('请输入用户名和密码。'); return; }
      var btn = $('#loginBtn');
      btn.disabled = true;
      btn.textContent = '登录中 ...';
      post('/auth/login', { username: username, password: password }).then(function (data) {
        var d = unwrap(data) || {};
        var token = d.token || '';
        if (!token) { throw new Error('登录响应未包含令牌'); }
        setToken(token);
        state.currentUser = { username: d.username || username, role: d.role || 'user' };
        enterApp();
      }).catch(function (err) {
        btn.disabled = false;
        btn.textContent = '登录';
        loginError(err.status === 401 ? '用户名或密码错误。' : (err.message || String(err)));
      });
    });
  }

  // Role-aware UI, minimal gating: only the Users view entry is hidden for
  // non-admin roles. Every other view stays available to all roles and the
  // backend enforces the permission matrix (auditor read-only, operator
  // whitelist, admin full) - a forbidden write simply surfaces as a 403
  // toast from the failing request.
  function applyRoleUI() {
    var isAdmin = state.currentUser && state.currentUser.role === 'admin';
    var usersNav = $('.nav-item[data-view="users"]');
    if (usersNav) { usersNav.style.display = isAdmin ? '' : 'none'; }
  }

  // Establish a session on boot. There is always a /auth/me probe: a
  // stored token is validated against it, and without a token the probe
  // distinguishes the two deployment modes. With auth disabled (the
  // default for a private single-machine console) the backend answers 200
  // with an anonymous admin principal and the app enters directly; with
  // auth enabled it answers 401 and the login screen shows.
  // 无鉴权模式：直接进入控制台，不做任何登录探测。
  function bootAuth() {
    state.currentUser = { username: 'admin', role: 'admin' };
    enterApp();
  }

  /* ------------------------------------------------------------------ *
   * Settings
   * ------------------------------------------------------------------ */
  function renderSettings() {
    var m = el('div', { class: 'grid' });
    m.appendChild(el('div', { class: 'card' },
      '<h2>快速上手</h2>' +
      '<ol style="margin:0;padding-left:20px;line-height:2">' +
      '<li><b>登记内容</b>：媒体库 → 登记媒体（填视频绝对路径）或扫描目录</li>' +
      '<li><b>配置推流</b>：推流引擎 → 填 ffmpeg 路径 + 添加输出（RTMP 地址/分辨率/码率）→ 保存</li>' +
      '<li><b>开始播放</b>：播放控制 → 选媒体 → 播放；推流引擎状态卡显示"运行中"即成功</li>' +
      '<li><b>自动排播</b>：节目单 → 新建（可循环/后备）→ 定时任务 → 新建间隔或定时任务并启用</li>' +
      '</ol>' +
      '<p class="muted mt">完整说明见仓库 docs/使用指南.md（媒体/节目单/任务/引擎/告警/Webhook 等全部页面用途）。</p>'));
    m.appendChild(el('div', { class: 'card' },
      '<h2>后端</h2>' +
      '<div class="kv"><dt>API 前缀</dt><dd class="mono">' + API + '/...</dd></div>' +
      '<p class="muted mt">控制台将 /console/api 下的请求代理到 KPlayer REST 后端。每次请求都会将会话令牌作为 Authorization（Bearer）标头发送。</p>'));

    // Account card. The manual token input from earlier versions is gone:
    // with the login flow the stored token is owned by the login/logout
    // cycle, so hand-editing it would fight the session state. Deployments
    // that need a fixed token can still set KPLAYER_CONSOLE_TOKEN
    // server-side, which the proxy injects and which wins over the client.
    var card = el('div', { class: 'card' },
      '<h2>访问模式</h2>' +
      '<p class="muted" style="margin:0">本机内网模式：无需登录，所有配置即时生效并持久化保存。</p>');

    m.appendChild(card);
    setView(m);
  }

  /* ------------------------------------------------------------------ *
   * Modal helper
   * ------------------------------------------------------------------ */
  function modalScrim(content) {
    var scrim = el('div', { class: 'modal-scrim' });
    scrim.appendChild(content);
    scrim.addEventListener('click', function (e) { if (e.target === scrim) { scrim.remove(); } });
    document.body.appendChild(scrim);
    // Most callers keep the content element (m) and close modals with
    // m.remove(); without this hook the empty full-screen scrim shell would
    // be left behind, silently intercepting every later click. Removing the
    // content therefore also removes its scrim shell.
    var contentRemove = content.remove.bind(content);
    content.remove = function () {
      contentRemove();
      scrim.remove();
    };
    return scrim;
  }

  /* ------------------------------------------------------------------ *
   * Routing + nav
   * ------------------------------------------------------------------ */
  function currentView() {
    var h = (location.hash || '').replace('#', '');
    return views[h] ? h : 'overview';
  }

  function render() {
    if (!state.authed) { return; }
    var v = currentView();
    // The Users view is admin-only; other roles fall back to Overview (the
    // nav entry is hidden for them as well).
    if (v === 'users' && !(state.currentUser && state.currentUser.role === 'admin')) {
      v = 'overview';
    }
    state.current = v;
    $$('.nav-item').forEach(function (b) {
      b.classList.toggle('is-active', b.dataset.view === v);
    });
    var title = views[v].title;
    $('#pageTitle').textContent = title;
    document.title = title + ' - KPlayer 控制台';
    closeSidebar();
    var renderers = {
      overview: renderOverview,
      streams: renderStreams,
      playlist: renderPlaylist,
      tasks: renderTasks,
      effects: renderEffects
    };
    renderers[v]();
  }

  function closeSidebar() {
    $('#sidebar').classList.remove('is-open');
    $('#menuBtn').setAttribute('aria-expanded', 'false');
    $('#scrim').hidden = true;
  }

  function bindNav() {
    $$('.nav-item[data-view]').forEach(function (b) {
      b.addEventListener('click', function () {
        location.hash = b.dataset.view;
      });
    });
    window.addEventListener('hashchange', render);

    $('#menuBtn').addEventListener('click', function () {
      var open = $('#sidebar').classList.toggle('is-open');
      $('#menuBtn').setAttribute('aria-expanded', String(open));
      $('#scrim').hidden = !open;
    });
    $('#scrim').addEventListener('click', closeSidebar);
    $('#collapseBtn').addEventListener('click', closeSidebar);
    $('#refreshBtn').addEventListener('click', render);
  }

  /* Auto-refresh the active view when it is a live dashboard section.
   * Polling only runs while a session is active. */
  var pollTimer = null;
  function startPolling() {
    if (pollTimer) { return; }
    pollTimer = setInterval(function () {
      if (!state.authed) { return; }
      var v = views[state.current];
      if (v && v.auto && !state.loading && !document.hidden) {
        render();
      }
    }, REFRESH_MS);
  }
  function stopPolling() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  }

  /* ------------------------------------------------------------------ *
   * Boot
   * ------------------------------------------------------------------ */
  document.addEventListener('DOMContentLoaded', function () {
    bindNav();
    bindLogin();
    bootAuth();
  });
})();