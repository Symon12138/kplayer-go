/* views/tasks.js — 定时任务：到点自动开播/关播，无人值守的核心。 */

import { get, post, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([get('/task/list'), get('/playlist/list'), get('/scheduler')]).then(function (res) {
    setConn(true);
    const tasks = listOf(res[0], ['tasks', 'items', 'list']);
    const playlists = listOf(res[1], ['playlists', 'items', 'list']);
    const sched = res[2] || {};
    draw(root, tasks, playlists, !!sched.running);
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, tasks, playlists, schedRunning) {
  root.innerHTML = '';
  const plById = {};
  playlists.forEach(function (p) { plById[p.id] = p; });

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建任务' });
  newBtn.addEventListener('click', function () { openEdit(null, playlists); });
  // 调度器开关：到点触发依赖调度器运行
  const schedBtn = el('button', { class: 'btn btn-sm' + (schedRunning ? '' : ' btn-danger'), type: 'button' });
  schedBtn.textContent = schedRunning ? '调度器：运行中（点击停止）' : '调度器：已停止（点击启动）';
  schedBtn.title = '定时任务到点自动执行的前提是调度器在运行';
  schedBtn.addEventListener('click', function () {
    post('/scheduler/' + (schedRunning ? 'stop' : 'start'), {}).then(function () {
      toast(schedRunning ? '调度器已停止 —— 定时任务不再触达' : '调度器已启动', 'ok'); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  });
  root.appendChild(pageHead('定时任务',
    '到点自动执行：开播（播放节目单最新内容）或关播（停止推流）。适合每天定时开/关播的无人值守场景。',
    [schedBtn, newBtn]));

  if (!tasks.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '新建第一个定时任务' });
    act.addEventListener('click', function () { openEdit(null, playlists); });
    const card = el('div', { class: 'card' });
    card.appendChild(emptyView('暂无定时任务。例如：每天 8:00 开播「早间节目」，23:00 自动关播。', act));
    root.appendChild(card);
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>名称</th><th>动作</th><th>目标</th><th>时间</th><th>上次执行</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  tasks.forEach(function (t) {
    const isStop = t.action === 'stop';
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<b>' + esc(t.name) + '</b>'));
    tr.appendChild(el('td', {}, isStop
      ? '<span class="badge badge-warn">关播</span>'
      : '<span class="badge badge-ok">开播</span>'));
    tr.appendChild(el('td', { text: isStop ? '-' : ((plById[t.playlistId || t.playlist_id] && plById[t.playlistId || t.playlist_id].name) || t.playlistId || '-') }));
    const cron = t.cron || '';
    tr.appendChild(el('td', { class: 'mono', text: cron ? cron : (t.interval != null ? '每 ' + t.interval + ' 秒' : '-') }));
    tr.appendChild(el('td', { class: 'dim', text: fmtAgo(t.lastRun || t.last_run) }));
    tr.appendChild(el('td', {}, t.enabled
      ? '<span class="badge badge-ok">启用</span>'
      : '<span class="badge badge-neutral">停用</span>'));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });
    const btnRun = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '立即执行' });
    btnRun.addEventListener('click', function () {
      btnRun.disabled = true; btnRun.textContent = '执行中 ...';
      post('/task/' + t.id + '/run', {}).then(function () {
        toast(isStop ? '已停止推流' : '已开始播放节目单', 'ok');
        setTimeout(render, 1200);
      }).catch(function (e) { toast(e.message, 'err'); })
        .finally(function () { btnRun.disabled = false; btnRun.textContent = '立即执行'; });
    });
    row.appendChild(btnRun);
    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openEdit(t, playlists); });
    row.appendChild(btnEdit);
    const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: t.enabled ? '停用' : '启用' });
    btnToggle.addEventListener('click', function () {
      post('/task/enabled', { id: t.id, enabled: !t.enabled }).then(function () {
        toast('任务已更新', 'ok'); render();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    row.appendChild(btnToggle);
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('删除任务 "' + t.name + '"?')) { return; }
      post('/task/remove', { id: t.id }).then(function () {
        toast('任务已删除', 'ok'); render();
      }).catch(function (e) { toast(e.message, 'err'); });
    });
    row.appendChild(btnDel);
    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}

/* ---------- 新建 / 编辑定时任务 ---------- */
function openEdit(t, playlists) {
  const editing = !!t;
  const isStop = editing && t.action === 'stop';
  const cron = editing ? (t.cron || '') : '';

  /* 解析已有 cron：每天固定时间 or 自定义表达式 */
  let timeMode = 'daily';
  if (editing && cron) {
    const parts = cron.split(/\s+/);
    const looksDaily = parts.length >= 5 && !isNaN(parseInt(parts[0], 10)) &&
      parts[2] === '*' && parts[3] === '*' && parts[4] === '*';
    timeMode = looksDaily ? 'daily' : 'cron';
  }
  let dailyHour = 8, dailyMin = 0;
  if (timeMode === 'daily' && cron) {
    const dp = cron.split(/\s+/);
    dailyMin = parseInt(dp[0], 10) || 0;
    dailyHour = parseInt(dp[1], 10) || 0;
  }
  if (editing && isStop && !cron) { dailyHour = 23; dailyMin = 0; }

  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑定时任务' : '新建定时任务') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="tName" type="text" value="' + esc(editing ? t.name : '') + '"></div>' +
    '<div class="field"><label>动作</label><select id="tAction">' +
    '<option value="play">开播（播放节目单）</option>' +
    '<option value="stop">关播（停止推流）</option>' +
    '</select></div>' +
    '</div>' +
    '<div class="field mt" id="tPlField"><label>节目单（到点播放最新内容）</label><select id="tPlaylist" class="mono"></select></div>' +
    '<div class="form-grid two mt">' +
    '<div class="field"><label>时间模式</label><select id="tTimeMode">' +
    '<option value="daily">每天固定时间</option>' +
    '<option value="cron">自定义 cron 表达式</option>' +
    '</select></div>' +
    '<div class="field" id="tDailyField"><label>每天时间</label><div class="cluster" style="gap:6px">' +
    '<input id="tHour" type="number" min="0" max="23" style="max-width:72px" value="' + dailyHour + '"> <span class="muted">时</span>' +
    '<input id="tMin" type="number" min="0" max="59" style="max-width:72px" value="' + dailyMin + '"> <span class="muted">分</span>' +
    '</div></div>' +
    '</div>' +
    '<div class="field" id="tCronField" style="display:none"><label>cron 表达式（5 段）</label>' +
    '<input id="tCron" type="text" class="mono" placeholder="0 8 * * *" value="' + esc(cron) + '">' +
    '<p class="muted" style="margin:3px 0 0;font-size:12px">示例：<code>0 8 * * *</code> 每天 8:00；<code>30 18 * * 1-5</code> 周一至五 18:30</p></div>' +
    '<div class="cluster mt"><label class="check"><input id="tEnabled" type="checkbox"' + ((!editing) || (editing && t.enabled) ? ' checked' : '') + '> 启用</label></div>' +
    '<p class="muted" style="font-size:12px;margin:10px 0 0">保存后可先点列表里的「立即执行」验证一次。</p>' +
    '<div class="form-actions"><button id="tCancel" class="btn" type="button">取消</button>' +
    '<button id="tSave" class="btn btn-primary" type="button">保存</button></div>'));

  const actionSel = m.querySelector('#tAction');
  if (isStop) { actionSel.value = 'stop'; }
  const plField = m.querySelector('#tPlField');
  const plSel = m.querySelector('#tPlaylist');
  playlists.forEach(function (p) { plSel.appendChild(el('option', { value: p.id, text: p.name })); });
  if (editing && t.playlistId) { plSel.value = t.playlistId; }
  function syncAction() { plField.style.display = actionSel.value === 'stop' ? 'none' : ''; }
  actionSel.addEventListener('change', syncAction);
  syncAction();

  const timeSel = m.querySelector('#tTimeMode');
  if (timeMode === 'cron') { timeSel.value = 'cron'; }
  function syncTime() {
    const isCron = timeSel.value === 'cron';
    m.querySelector('#tDailyField').style.display = isCron ? 'none' : '';
    m.querySelector('#tCronField').style.display = isCron ? '' : 'none';
  }
  timeSel.addEventListener('change', syncTime);
  syncTime();

  m.querySelector('#tCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#tSave').addEventListener('click', function () {
    const name = m.querySelector('#tName').value.trim();
    if (!name) { toast('任务名称不能为空', 'err'); return; }
    const action = actionSel.value;
    let cronValue;
    if (timeSel.value === 'cron') {
      cronValue = m.querySelector('#tCron').value.trim();
    } else {
      const hh = parseInt(m.querySelector('#tHour').value, 10);
      const mm = parseInt(m.querySelector('#tMin').value, 10);
      if (isNaN(hh) || isNaN(mm) || hh < 0 || hh > 23 || mm < 0 || mm > 59) {
        toast('请填写有效的时间（时 0-23，分 0-59）', 'err'); return;
      }
      cronValue = mm + ' ' + hh + ' * * *';
    }
    if (action === 'play' && !plSel.value) { toast('请选择节目单', 'err'); return; }
    const body = {
      name: name,
      type: 'cron',
      cron: cronValue,
      action: action,
      playlistId: action === 'play' ? plSel.value : undefined,
      enabled: m.querySelector('#tEnabled').checked
    };
    const req = editing ? post('/task/replace', Object.assign({ id: t.id }, body)) : post('/task/add', body);
    req.then(function () {
      toast(editing ? '任务已更新' : '任务已创建', 'ok');
      m.remove(); render();
    }).catch(function (e) { toast(e.message, 'err'); });
  });
}
