/* views/overview.js — 总览：只读仪表盘。
 * KPI 指标、当前推流（含进度与快捷控制）、任务运行状态。 */

import { get, post, listOf, objOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.allSettled([
    get('/media/list'),
    get('/playlist/list'),
    get('/stream/list'),
    get('/engine/status')
  ]).then(function (res) {
    setConn(res.some(function (r) { return r.status === 'fulfilled'; }));

    const media = listOf(res[0].status === 'fulfilled' ? res[0].value : [], ['media', 'items', 'list']);
    const playlists = listOf(res[1].status === 'fulfilled' ? res[1].value : [], ['playlists', 'items', 'list']);
    const streams = listOf(res[2].status === 'fulfilled' ? res[2].value : [], ['streams', 'items', 'list']);
    const eng = objOf(res[3].status === 'fulfilled' ? res[3].value : {}, ['status']);

    root.innerHTML = '';

    /* ---- KPI ---- */
    const running = streams.filter(function (s) { return !!s.running; }).length;
    const engRunning = !!(eng && eng.running);
    const engPaused = !!(eng && eng.paused);
    const live = engRunning || engPaused;
    const kpis = [
      { label: '媒体', value: String(media.length), sub: '推流内容源', live: false },
      { label: '节目单', value: String(playlists.length), sub: '播出计划', live: false },
      { label: '推流任务', value: running + ' / ' + streams.length, sub: '运行中 / 总数', live: running > 0 },
      { label: '引擎状态', value: engPaused ? '已暂停' : (engRunning ? '推流中' : '已停止'), sub: live ? (eng.uptime || '-') : '待机', live: engRunning }
    ];
    const kpiGrid = el('div', { class: 'grid-cards' });
    kpis.forEach(function (c) {
      kpiGrid.appendChild(el('div', { class: 'stat' + (c.live ? ' live' : '') },
        '<div class="stat-label">' + esc(c.label) + '</div>' +
        '<div class="stat-value">' + esc(c.value) + '</div>' +
        '<div class="stat-sub">' + esc(c.sub) + '</div>'));
    });
    root.appendChild(kpiGrid);

    /* ---- 当前推流 hero ---- */
    const stateBadge = engPaused
      ? '<span class="badge badge-warn">已暂停</span>'
      : (engRunning
        ? '<span class="badge badge-ok"><span class="dot ok"></span>推流中</span>'
        : '<span class="badge badge-neutral">已停止</span>');
    const pct = Math.max(0, Math.min(100, Number(eng.progress) || 0));

    const hero = el('div', { class: 'now-playing section' + (engPaused ? ' paused' : live ? ' live' : '') });
    const head = el('div', { class: 'now-head' });

    const titleBox = el('div', { style: 'min-width:0' });
    const titleRow = el('div', { style: 'display:flex;align-items:center;gap:10px;flex-wrap:wrap' });
    titleRow.appendChild(el('span', { html: stateBadge }));
    titleRow.appendChild(el('span', { class: 'now-title', text: (eng && eng.sourcePath) || '当前无推流' }));
    titleBox.appendChild(titleRow);
    titleBox.appendChild(el('div', { class: 'now-sub' }, live
      ? ('ffmpeg 进程 ' + esc((eng && eng.pid) || '-') + (engPaused ? ' · 已暂停，可点「继续」恢复' : ' · 推送中'))
      : '在「推流任务」页启动一个任务，或使用快速推流'));
    head.appendChild(titleBox);

    const controls = el('div', { class: 'now-controls' });
    if (live) {
      [['pause', '暂停'], ['continue', '继续'], ['skip', '跳过']].forEach(function (p) {
        const b = el('button', { class: 'btn btn-sm', type: 'button', text: p[1] });
        b.addEventListener('click', function () {
          post('/player/' + p[0], {}).then(function () { toast('已' + p[1], 'ok'); render(); })
            .catch(function (e) { toast(e.message, 'err'); });
        });
        controls.appendChild(b);
      });
      const stop = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '停止' });
      stop.addEventListener('click', function () {
        post('/player/stop', {}).then(function () { toast('已停止', 'ok'); render(); })
          .catch(function (e) { toast(e.message, 'err'); });
      });
      controls.appendChild(stop);
    }
    controls.appendChild(el('a', { class: 'btn btn-sm btn-primary', href: '#streams', text: live ? '管理推流' : '去推流' }));
    head.appendChild(controls);
    hero.appendChild(head);

    if (live) {
      hero.appendChild(el('div', { class: 'progress-track' },
        '<div class="progress-fill" style="width:' + pct + '%"></div>'));
      hero.appendChild(el('div', { class: 'time-row' },
        '<span>' + pct.toFixed(1) + '%</span><span>运行时长 ' + esc((eng && eng.uptime) || '-') + '</span>'));
    }
    if (live && eng.outputURLs && eng.outputURLs.length) {
      const outs = el('div', { class: 'list-item-meta' });
      eng.outputURLs.forEach(function (u) { outs.appendChild(el('span', { class: 'badge badge-info mono', text: u })); });
      hero.appendChild(outs);
    }
    root.appendChild(hero);

    /* ---- 任务运行状态表 ---- */
    root.appendChild(el('div', { class: 'row' },
      '<h2>推流任务</h2>' +
      '<a class="btn btn-sm" href="#streams">管理全部任务</a>'));
    const tbl = el('div', { class: 'table-wrap' });
    tbl.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>输出线路</th><th>状态</th><th>码率</th><th>进度</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = tbl.querySelector('tbody');
    if (!streams.length) {
      tbody.appendChild(el('tr', {},
        '<td colspan="5"><div class="empty" style="padding:20px"><p>还没有推流任务 —— 到「推流任务」页新建一个。</p></div></td>'));
    } else {
      streams.forEach(function (s) {
        const tr = el('tr', {});
        tr.appendChild(el('td', { text: s.name || s.id }));
        const outTd = el('td', { class: 'cell-path' });
        const outs = s.outputs || [];
        if (!outs.length) {
          outTd.textContent = '-';
        } else {
          outs.forEach(function (o, i) {
            if (i > 0) { outTd.appendChild(el('div', { class: 'dim', text: '·' })); }
            outTd.appendChild(el('div', { class: 'path', text: o.url || '-' }));
          });
        }
        tr.appendChild(outTd);
        tr.appendChild(el('td', {}, s.running
          ? '<span class="badge badge-ok"><span class="dot ok"></span>运行中</span>'
          : '<span class="badge badge-neutral">已停止</span>'));
        tr.appendChild(el('td', { text: s.bitrateKbps ? s.bitrateKbps + ' kbps' : '-' }));
        tr.appendChild(el('td', { text: s.running ? Math.round(Number(s.progress) || 0) + '%' : '-' }));
        tbody.appendChild(tr);
      });
    }
    root.appendChild(tbl);
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}
