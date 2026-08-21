/* views/ha.js — 输出高可用：输出分组、主备切换、健康策略。
 * 三个板块共用一套「表格 + 模态」模式。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([
    get('/output-group/list'),
    get('/failover/list'),
    get('/health-policy/list')
  ]).then(function (res) {
    setConn(true);
    draw(root,
      listOf(res[0], ['groups', 'items', 'list']),
      listOf(res[1], ['failovers', 'items', 'list']),
      listOf(res[2], ['healthPolicies', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, groups, failovers, policies) {
  root.innerHTML = '';
  root.appendChild(pageHead('输出高可用',
    '输出分组按平台/业务归类线路；主备切换在主输出异常时切到备用；健康策略约束重试与跳过。'));

  /* ---- 输出分组 ---- */
  const gSec = section(root, '输出分组（' + groups.length + '）', '把多条输出线路按平台/区域/业务归类管理。', function () { openGroupEdit(null); });
  if (!groups.length) {
    gSec.appendChild(emptyCard('暂无输出分组。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>平台</th><th>区域</th><th>业务</th><th>线路数</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    groups.forEach(function (g) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<b>' + esc(g.name) + '</b>'));
      tr.appendChild(el('td', { text: g.platform || '-' }));
      tr.appendChild(el('td', { text: g.region || '-' }));
      tr.appendChild(el('td', { text: g.business || '-' }));
      tr.appendChild(el('td', { text: String((g.outputs || []).length) }));
      tr.appendChild(el('td', {}, g.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));
      tr.appendChild(actionsTd([
        { text: g.enabled ? '停用' : '启用', fn: function () { return post('/output-group/enabled', { id: g.id, enabled: !g.enabled }); } },
        { text: '编辑', fn: null, edit: function () { openGroupEdit(g); } },
        {
          text: '删除', danger: true, confirm: '删除分组 "' + g.name + '"？',
          fn: function () { return del('/output-group/' + encodeURIComponent(g.id)); }
        }
      ], render));
      tbody.appendChild(tr);
    });
    gSec.appendChild(wrap);
  }

  /* ---- 主备切换 ---- */
  const fSec = section(root, '主备切换（' + failovers.length + '）', '监控主输出；超过阈值秒数无数据时切换到备用输出。', function () { openFailoverEdit(null); });
  if (!failovers.length) {
    fSec.appendChild(emptyCard('暂无主备切换规则。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>主输出</th><th>备用输出</th><th>策略</th><th>阈值(秒)</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    failovers.forEach(function (f) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<b>' + esc(f.name) + '</b>'));
      tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(f.primaryUnique || '-') + '</div>'));
      tr.appendChild(el('td', { class: 'cell-path' }, '<div class="path">' + esc(f.backupUnique || '-') + '</div>'));
      tr.appendChild(el('td', {}, '<span class="badge badge-info">' + (f.policy === 'manual' ? '手动切回' : '自动切回') + '</span>'));
      tr.appendChild(el('td', { text: String(f.thresholdSeconds != null ? f.thresholdSeconds : '-') }));
      tr.appendChild(el('td', {}, f.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));
      tr.appendChild(actionsTd([
        { text: f.enabled ? '停用' : '启用', fn: function () { return post('/failover/enabled', { id: f.id, enabled: !f.enabled }); } },
        { text: '编辑', edit: function () { openFailoverEdit(f); } },
        {
          text: '删除', danger: true, confirm: '删除规则 "' + f.name + '"？',
          fn: function () { return del('/failover/' + encodeURIComponent(f.id)); }
        }
      ], render));
      tbody.appendChild(tr);
    });
    fSec.appendChild(wrap);
  }

  /* ---- 健康策略 ---- */
  const pSec = section(root, '健康策略（' + policies.length + '）', '失败重试次数 / 重试窗口 / 失败自动跳过。', function () { openPolicyEdit(null); });
  if (!policies.length) {
    pSec.appendChild(emptyCard('暂无健康策略。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>最大重试</th><th>重试窗口(秒)</th><th>失败自动跳过</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    policies.forEach(function (p) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<b>' + esc(p.name) + '</b>'));
      tr.appendChild(el('td', { text: String(p.maxRetries != null ? p.maxRetries : '-') }));
      tr.appendChild(el('td', { text: String(p.retryWindowSeconds != null ? p.retryWindowSeconds : '-') }));
      tr.appendChild(el('td', {}, p.autoSkipOnFailure ? '<span class="badge badge-ok">是</span>' : '<span class="badge badge-neutral">否</span>'));
      tr.appendChild(el('td', {}, p.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));
      tr.appendChild(actionsTd([
        { text: p.enabled ? '停用' : '启用', fn: function () { return post('/health-policy/enabled', { id: p.id, enabled: !p.enabled }); } },
        { text: '编辑', edit: function () { openPolicyEdit(p); } },
        {
          text: '删除', danger: true, confirm: '删除策略 "' + p.name + '"？',
          fn: function () { return del('/health-policy/' + encodeURIComponent(p.id)); }
        }
      ], render));
      tbody.appendChild(tr);
    });
    pSec.appendChild(wrap);
  }
}

/* ---- 小工具 ---- */
function section(root, title, desc, onNew) {
  const sec = el('div', { class: 'section' });
  const row = el('div', { class: 'row' },
    '<div><h2>' + esc(title) + '</h2><p class="muted" style="margin:2px 0 0;font-size:12px">' + esc(desc) + '</p></div>');
  const btn = el('button', { class: 'btn btn-primary btn-sm', type: 'button', text: '+ 新建' });
  btn.addEventListener('click', onNew);
  row.appendChild(btn);
  sec.appendChild(row);
  root.appendChild(sec);
  return sec;
}

function emptyCard(msg) {
  const card = el('div', { class: 'card' });
  card.appendChild(emptyView(msg, undefined));
  return card;
}

function actionsTd(actions, refresh) {
  const td = el('td', { class: 'actions' });
  const row = el('div', { class: 'icon-btn-row' });
  actions.forEach(function (a) {
    const b = el('button', { class: 'btn btn-sm' + (a.danger ? ' btn-danger' : ''), type: 'button', text: a.text });
    b.addEventListener('click', function () {
      if (a.edit) { a.edit(); return; }
      if (a.confirm && !window.confirm(a.confirm)) { return; }
      a.fn().then(function () { toast('已' + a.text, 'ok'); refresh(); })
        .catch(function (e) { toast(e.message, 'err'); });
    });
    row.appendChild(b);
  });
  td.appendChild(row);
  return td;
}

/* ---- 输出分组模态 ---- */
function openGroupEdit(g) {
  const editing = !!g;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑输出分组' : '新建输出分组') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="gName" type="text" value="' + esc(editing ? g.name : '') + '"></div>' +
    '<div class="field"><label>平台</label><input id="gPlatform" type="text" value="' + esc(editing ? (g.platform || '') : '') + '" placeholder="如 douyu/huya/bilibili"></div>' +
    '<div class="field"><label>区域</label><input id="gRegion" type="text" value="' + esc(editing ? (g.region || '') : '') + '"></div>' +
    '<div class="field"><label>业务</label><input id="gBusiness" type="text" value="' + esc(editing ? (g.business || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>输出 unique 列表（每行一个，对应推流线路的唯一标识）</label>' +
    '<textarea id="gOutputs" class="mono" spellcheck="false" placeholder="out-1&#10;out-2">' + esc(editing ? (g.outputs || []).join('\n') : '') + '</textarea></div>' +
    '<div class="cluster mt"><label class="check"><input id="gEnabled" type="checkbox"' + (!editing || g.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="gCancel" class="btn" type="button">取消</button>' +
    '<button id="gSave" class="btn btn-primary" type="button">保存</button></div>'));
  m.querySelector('#gCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#gSave').addEventListener('click', function () {
    const name = m.querySelector('#gName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    const outputs = m.querySelector('#gOutputs').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean);
    const body = {
      name: name,
      platform: m.querySelector('#gPlatform').value.trim(),
      region: m.querySelector('#gRegion').value.trim(),
      business: m.querySelector('#gBusiness').value.trim(),
      outputs: outputs,
      enabled: m.querySelector('#gEnabled').checked
    };
    const req = editing
      ? post('/output-group/' + encodeURIComponent(g.id) + '/update', Object.assign({ id: g.id }, body))
      : post('/output-group/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}

/* ---- 主备切换模态 ---- */
function openFailoverEdit(f) {
  const editing = !!f;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑主备切换' : '新建主备切换') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="fName" type="text" value="' + esc(editing ? f.name : '') + '"></div>' +
    '<div class="field"><label>切换阈值（秒）</label><input id="fThreshold" type="number" min="1" value="' + (editing ? (f.thresholdSeconds || 10) : 10) + '"></div>' +
    '<div class="field"><label>主输出 unique</label><input id="fPrimary" type="text" class="mono" value="' + esc(editing ? (f.primaryUnique || '') : '') + '"></div>' +
    '<div class="field"><label>备用输出 unique</label><input id="fBackup" type="text" class="mono" value="' + esc(editing ? (f.backupUnique || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>恢复策略</label><select id="fPolicy">' +
    '<option value="automatic">自动切回主输出</option>' +
    '<option value="manual">切到备用后人工处理</option>' +
    '</select></div>' +
    '<div class="cluster mt"><label class="check"><input id="fEnabled" type="checkbox"' + (!editing || f.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="fCancel" class="btn" type="button">取消</button>' +
    '<button id="fSave" class="btn btn-primary" type="button">保存</button></div>'));
  if (editing) { m.querySelector('#fPolicy').value = f.policy || 'automatic'; }
  m.querySelector('#fCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#fSave').addEventListener('click', function () {
    const name = m.querySelector('#fName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    const body = {
      name: name,
      primaryUnique: m.querySelector('#fPrimary').value.trim(),
      backupUnique: m.querySelector('#fBackup').value.trim(),
      policy: m.querySelector('#fPolicy').value,
      thresholdSeconds: parseInt(m.querySelector('#fThreshold').value, 10) || 10,
      enabled: m.querySelector('#fEnabled').checked
    };
    const req = editing
      ? post('/failover/' + encodeURIComponent(f.id) + '/update', Object.assign({ id: f.id }, body))
      : post('/failover/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}

/* ---- 健康策略模态 ---- */
function openPolicyEdit(p) {
  const editing = !!p;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑健康策略' : '新建健康策略') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="pName" type="text" value="' + esc(editing ? p.name : '') + '"></div>' +
    '<div class="field"><label>最大重试次数</label><input id="pRetries" type="number" min="0" value="' + (editing ? (p.maxRetries != null ? p.maxRetries : 3) : 3) + '"></div>' +
    '<div class="field"><label>重试窗口（秒）</label><input id="pWindow" type="number" min="1" value="' + (editing ? (p.retryWindowSeconds != null ? p.retryWindowSeconds : 60) : 60) + '"></div>' +
    '</div>' +
    '<div class="cluster mt"><label class="check"><input id="pSkip" type="checkbox"' + (editing && p.autoSkipOnFailure ? ' checked' : '') + '> 重试耗尽后自动跳过该内容</label></div>' +
    '<div class="cluster mt"><label class="check"><input id="pEnabled" type="checkbox"' + (!editing || p.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="pCancel" class="btn" type="button">取消</button>' +
    '<button id="pSave" class="btn btn-primary" type="button">保存</button></div>'));
  m.querySelector('#pCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#pSave').addEventListener('click', function () {
    const name = m.querySelector('#pName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    const body = {
      name: name,
      maxRetries: parseInt(m.querySelector('#pRetries').value, 10) || 0,
      retryWindowSeconds: parseInt(m.querySelector('#pWindow').value, 10) || 60,
      autoSkipOnFailure: m.querySelector('#pSkip').checked,
      enabled: m.querySelector('#pEnabled').checked
    };
    const req = editing
      ? post('/health-policy/' + encodeURIComponent(p.id) + '/update', Object.assign({ id: p.id }, body))
      : post('/health-policy/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
