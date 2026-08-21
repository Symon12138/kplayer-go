/* views/analytics.js — 数据统计：播放概览、输出稳定趋势、媒体失败率。 */

import { get } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, pageHead } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([
    get('/metrics/summary'),
    get('/metrics/trend?days=14')
  ]).then(function (res) {
    setConn(true);
    draw(root, (res[0] && res[0].summary) || {}, (res[1] && res[1].days) || []);
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, summary, trend) {
  root.innerHTML = '';
  root.appendChild(pageHead('数据统计', '基于播放事件日志的汇总：成功率、断流趋势、按媒体失败率。'));

  /* ---- 概览 KPI ---- */
  const kpis = [
    { label: '总播放次数', value: String(summary.totalPlays != null ? summary.totalPlays : '-') },
    { label: '成功', value: String(summary.successes != null ? summary.successes : '-') },
    { label: '失败', value: String(summary.failures != null ? summary.failures : '-') },
    { label: '成功率', value: summary.successRate != null ? (Math.round(Number(summary.successRate) * 1000) / 10) + '%' : '-' }
  ];
  const kpiGrid = el('div', { class: 'grid-cards' });
  kpis.forEach(function (c) {
    kpiGrid.appendChild(el('div', { class: 'stat' + (c.label === '成功率' && Number(summary.successRate) >= 0.95 ? ' live' : '') },
      '<div class="stat-label">' + esc(c.label) + '</div>' +
      '<div class="stat-value">' + esc(c.value) + '</div>'));
  });
  root.appendChild(kpiGrid);

  /* ---- 趋势（纯 CSS 条形） ---- */
  const trendCard = el('div', { class: 'card section' }, '<h2>近 14 天断流/失败趋势</h2>');
  if (!trend.length) {
    trendCard.appendChild(el('p', { class: 'muted', text: '暂无数据 —— 播放事件积累后这里会出现趋势图。' }));
  } else {
    const max = Math.max.apply(null, trend.map(function (d) { return Number(d.count) || 0; }).concat([1]));
    const chart = el('div', { style: 'display:flex;align-items:flex-end;gap:6px;height:140px;padding-top:8px' });
    trend.forEach(function (d) {
      const h = Math.max(2, Math.round((Number(d.count) || 0) / max * 120));
      const bar = el('div', {
        title: d.date + '：' + d.count + ' 次',
        style: 'flex:1;background:linear-gradient(180deg,var(--brand),#2f6fed);border-radius:4px 4px 0 0;height:' + h + 'px;min-width:10px'
      });
      const col = el('div', { style: 'flex:1;display:flex;flex-direction:column;align-items:center;justify-content:flex-end;height:100%;gap:4px' });
      col.appendChild(el('span', { class: 'dim', style: 'font-size:11px', text: String(d.count) }));
      col.appendChild(bar);
      col.appendChild(el('span', { class: 'dim', style: 'font-size:10px;white-space:nowrap', text: String(d.date).slice(5) }));
      chart.appendChild(col);
    });
    trendCard.appendChild(chart);
  }
  root.appendChild(trendCard);

  /* ---- 按媒体失败率 ---- */
  const frCard = el('div', { class: 'card' }, '<h2>按媒体查失败率</h2>' +
    '<p class="muted" style="margin:-6px 0 10px;font-size:12px">填媒体 ID（媒体库表格路径列旁可复制），查询该内容的播放成功/失败统计。</p>');
  const row = el('div', { class: 'cluster' });
  const input = el('input', { type: 'text', class: 'mono', placeholder: '媒体 ID', spellcheck: 'false', style: 'min-height:34px;background:var(--bg-0);color:var(--txt-0);border:1px solid var(--line-2);border-radius:8px;padding:5px 10px;width:280px' });
  const btn = el('button', { class: 'btn btn-primary btn-sm', type: 'button', text: '查询' });
  const out = el('div', { class: 'mt' });
  btn.addEventListener('click', function () {
    const id = input.value.trim();
    if (!id) { toast('请填写媒体 ID', 'err'); return; }
    get('/metrics/failure-rate?mediaId=' + encodeURIComponent(id)).then(function (r) {
      out.innerHTML = '<div class="kv">' +
        '<div><dt>播放</dt><dd>' + esc(String((r && r.plays) != null ? r.plays : '-')) + '</dd></div>' +
        '<div><dt>失败</dt><dd>' + esc(String((r && r.failures) != null ? r.failures : '-')) + '</dd></div>' +
        '<div><dt>失败率</dt><dd>' + esc(r && r.rate != null ? (Math.round(Number(r.rate) * 1000) / 10) + '%' : '-') + '</dd></div>' +
        '</div>';
    }).catch(function (e) { toast(e.message, 'err'); });
  });
  row.appendChild(input); row.appendChild(btn);
  frCard.appendChild(row); frCard.appendChild(out);
  root.appendChild(frCard);
}
