/* views/templates.js — 模板中心：配置模板（参数化 JSON）+ 行业模板（一键落地整套配置）。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([
    get('/config-template/list'),
    get('/industry-template/list')
  ]).then(function (res) {
    setConn(true);
    draw(root,
      listOf(res[0], ['templates', 'items', 'list']),
      listOf(res[1], ['templates', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, cfgTemplates, indTemplates) {
  root.innerHTML = '';
  root.appendChild(pageHead('模板中心',
    '配置模板：参数化 JSON 片段，按变量展开；行业模板：门店/校园等场景一键生成节目单+任务。'));

  /* ================= 配置模板 ================= */
  const cSec = section(root, '配置模板（' + cfgTemplates.length + '）', function () { openCfgEdit(null); });
  if (!cfgTemplates.length) {
    cSec.appendChild(emptyCard('暂无配置模板。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>类型</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    cfgTemplates.forEach(function (t) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<b>' + esc(t.name) + '</b>'));
      tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(t.type || '-') + '</span>'));
      tr.appendChild(el('td', {}, t.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));

      const ops = el('td', { class: 'actions' });
      const row = el('div', { class: 'icon-btn-row' });
      const btnExpand = el('button', { class: 'btn btn-sm', type: 'button', text: '展开预览' });
      btnExpand.addEventListener('click', function () { openExpand(t); });
      row.appendChild(btnExpand);
      const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: t.enabled ? '停用' : '启用' });
      btnToggle.addEventListener('click', function () {
        post('/config-template/enabled', { id: t.id, enabled: !t.enabled }).then(function () { toast('已更新', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(btnToggle);
      const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
      btnEdit.addEventListener('click', function () { openCfgEdit(t); });
      row.appendChild(btnEdit);
      const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
      btnDel.addEventListener('click', function () {
        if (!window.confirm('删除模板 "' + t.name + '"？')) { return; }
        del('/config-template/' + encodeURIComponent(t.id)).then(function () { toast('已删除', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(btnDel);
      ops.appendChild(row);
      tr.appendChild(ops);
      tbody.appendChild(tr);
    });
    cSec.appendChild(wrap);
  }

  /* ================= 行业模板 ================= */
  const iSec = section(root, '行业模板（' + indTemplates.length + '）', function () { openIndEdit(null); });
  if (!indTemplates.length) {
    iSec.appendChild(emptyCard('暂无行业模板。模板描述一套场景：节目单名 + 素材占位 + 场景 + 定时任务，部署时按提示填参落地。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>说明</th><th>素材占位</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    indTemplates.forEach(function (t) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<b>' + esc(t.name) + '</b>'));
      tr.appendChild(el('td', { class: 'dim', text: t.description || '-' }));
      const ph = el('td', { class: 'cell-path' });
      (t.mediaPlaceholders || []).forEach(function (p) { ph.appendChild(el('div', { class: 'path', text: p })); });
      if (!(t.mediaPlaceholders || []).length) { ph.textContent = '-'; }
      tr.appendChild(ph);
      tr.appendChild(el('td', {}, t.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));

      const ops = el('td', { class: 'actions' });
      const row = el('div', { class: 'icon-btn-row' });
      const btnDeploy = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '部署' });
      btnDeploy.addEventListener('click', function () { openDeploy(t); });
      row.appendChild(btnDeploy);
      const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: t.enabled ? '停用' : '启用' });
      btnToggle.addEventListener('click', function () {
        post('/industry-template/enabled', { id: t.id, enabled: !t.enabled }).then(function () { toast('已更新', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(btnToggle);
      const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
      btnEdit.addEventListener('click', function () { openIndEdit(t); });
      row.appendChild(btnEdit);
      const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
      btnDel.addEventListener('click', function () {
        if (!window.confirm('删除行业模板 "' + t.name + '"？（已部署的内容不受影响）')) { return; }
        del('/industry-template/' + encodeURIComponent(t.id)).then(function () { toast('已删除', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(btnDel);
      ops.appendChild(row);
      tr.appendChild(ops);
      tbody.appendChild(tr);
    });
    iSec.appendChild(wrap);
  }
}

/* ---- 小件 ---- */
function section(root, title, onNew) {
  const sec = el('div', { class: 'section' });
  const row = el('div', { class: 'row' }, '<h2>' + esc(title) + '</h2>');
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

/* ---- 配置模板模态 ---- */
function openCfgEdit(t) {
  const editing = !!t;
  const m = modal(el('div', { class: 'modal wide' },
    '<h3>' + (editing ? '编辑配置模板' : '新建配置模板') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="ctName" type="text" value="' + esc(editing ? t.name : '') + '"></div>' +
    '<div class="field"><label>类型（自定义标识，如 output/play）</label><input id="ctType" type="text" class="mono" value="' + esc(editing ? (t.type || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>模板体（JSON；可用 {{var}} 占位）</label>' +
    '<textarea id="ctBody" class="mono" spellcheck="false" style="min-height:160px">' +
    esc(editing && t.body != null ? (typeof t.body === 'string' ? t.body : JSON.stringify(t.body, null, 2)) : '{\n  "example": "{{value}}"\n}') +
    '</textarea></div>' +
    '<div class="cluster mt"><label class="check"><input id="ctEnabled" type="checkbox"' + (!editing || t.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="ctCancel" class="btn" type="button">取消</button>' +
    '<button id="ctSave" class="btn btn-primary" type="button">保存</button></div>'), true);
  m.querySelector('#ctCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#ctSave').addEventListener('click', function () {
    const name = m.querySelector('#ctName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    let body;
    try { body = JSON.parse(m.querySelector('#ctBody').value); }
    catch (e) { toast('模板体不是合法 JSON：' + e.message, 'err'); return; }
    const payload = { name: name, type: m.querySelector('#ctType').value.trim(), body: body, enabled: m.querySelector('#ctEnabled').checked };
    const req = editing
      ? post('/config-template/' + encodeURIComponent(t.id) + '/update', Object.assign({ id: t.id }, payload))
      : post('/config-template/add', payload);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}

/* ---- 展开预览 ---- */
function openExpand(t) {
  const paramsRaw = window.prompt('模板变量（每行 key = value，留空直接展开）：') || '';
  const params = {};
  paramsRaw.split('\n').forEach(function (line) {
    const i = line.indexOf('=');
    if (i > 0) { params[line.slice(0, i).trim()] = line.slice(i + 1).trim(); }
  });
  post('/config-template/expand', { id: t.id, params: params }).then(function (r) {
    const m = modal(el('div', { class: 'modal wide' },
      '<h3>展开结果 — ' + esc(t.name) + '</h3>' +
      '<pre style="max-height:50vh;overflow:auto;background:var(--bg-0);border:1px solid var(--line-1);border-radius:8px;padding:12px;font-family:var(--mono);font-size:12px;white-space:pre-wrap;word-break:break-all">' +
      esc(JSON.stringify(r && r.body, null, 2)) + '</pre>' +
      '<div class="form-actions"><button class="btn" type="button">关闭</button></div>'), true);
    m.querySelector('.form-actions .btn').addEventListener('click', function () { m.remove(); });
  }).catch(function (e) { toast(e.message, 'err'); });
}

/* ---- 行业模板模态 ---- */
function openIndEdit(t) {
  const editing = !!t;
  const taskJson = editing && t.task ? JSON.stringify(t.task, null, 2) : '';
  const m = modal(el('div', { class: 'modal wide' },
    '<h3>' + (editing ? '编辑行业模板' : '新建行业模板') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="itName" type="text" value="' + esc(editing ? t.name : '') + '" placeholder="如 门店营业频道"></div>' +
    '<div class="field"><label>生成的节目单名</label><input id="itPlName" type="text" value="' + esc(editing ? (t.playlistName || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>说明</label><input id="itDesc" type="text" value="' + esc(editing ? (t.description || '') : '') + '"></div>' +
    '<div class="field mt"><label>素材占位（每行一个，部署时逐个填实际路径）</label>' +
    '<textarea id="itPh" class="mono" spellcheck="false" style="min-height:70px" placeholder="${main_video}&#10;${logo}">' +
    esc(editing ? (t.mediaPlaceholders || []).join('\n') : '') + '</textarea></div>' +
    '<div class="field mt"><label>场景类型（每行一个 kind，可选）</label>' +
    '<textarea id="itKinds" class="mono" spellcheck="false" style="min-height:52px">' +
    esc(editing ? (t.sceneKinds || []).join('\n') : '') + '</textarea></div>' +
    '<div class="field mt"><label>定时任务（JSON，可选；type: interval|cron）</label>' +
    '<textarea id="itTask" class="mono" spellcheck="false" style="min-height:90px" placeholder=\'{"type":"cron","cron":"0 9 * * *","enabled":true}\'>' +
    esc(taskJson) + '</textarea></div>' +
    '<div class="cluster mt"><label class="check"><input id="itEnabled" type="checkbox"' + (!editing || t.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="itCancel" class="btn" type="button">取消</button>' +
    '<button id="itSave" class="btn btn-primary" type="button">保存</button></div>'), true);
  m.querySelector('#itCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#itSave').addEventListener('click', function () {
    const name = m.querySelector('#itName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    let task;
    const taskRaw = m.querySelector('#itTask').value.trim();
    if (taskRaw) {
      try { task = JSON.parse(taskRaw); }
      catch (e) { toast('定时任务不是合法 JSON：' + e.message, 'err'); return; }
    }
    const body = {
      name: name,
      description: m.querySelector('#itDesc').value.trim(),
      playlistName: m.querySelector('#itPlName').value.trim(),
      mediaPlaceholders: m.querySelector('#itPh').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean),
      sceneKinds: m.querySelector('#itKinds').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean),
      task: task,
      enabled: m.querySelector('#itEnabled').checked
    };
    const req = editing
      ? post('/industry-template/' + encodeURIComponent(t.id) + '/update', Object.assign({ id: t.id }, body))
      : post('/industry-template/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}

/* ---- 部署 ---- */
function openDeploy(t) {
  const placeholders = t.mediaPlaceholders || [];
  let body;
  if (!placeholders.length) {
    body = el('div', { class: 'modal' },
      '<h3>部署 — ' + esc(t.name) + '</h3>' +
      '<p class="muted">该模板没有素材占位，直接部署？将创建节目单「' + esc(t.playlistName || t.name) + '」' +
      ((t.task) ? '及定时任务' : '') + '。</p>' +
      '<div class="form-actions"><button class="btn js-cancel" type="button">取消</button>' +
      '<button class="btn btn-primary js-go" type="button">立即部署</button></div>');
  } else {
    body = el('div', { class: 'modal' },
      '<h3>部署 — ' + esc(t.name) + '</h3>' +
      '<p class="muted" style="margin:-6px 0 10px;font-size:12px">为每个素材占位填对应的媒体库 ID（在媒体库页可查）：</p>' +
      '<div class="js-ph"></div>' +
      '<div class="form-actions"><button class="btn js-cancel" type="button">取消</button>' +
      '<button class="btn btn-primary js-go" type="button">立即部署</button></div>');
    const host = body.querySelector('.js-ph');
    placeholders.forEach(function (p) {
      host.appendChild(el('div', { class: 'field' },
        '<label>' + esc(p) + '</label><input type="text" class="mono js-ph-input" data-ph="' + esc(p) + '" spellcheck="false" placeholder="填该占位的实际路径">'));
    });
  }
  const m = modal(body);
  body.querySelector('.js-cancel').addEventListener('click', function () { m.remove(); });
  body.querySelector('.js-go').addEventListener('click', function () {
    const inputs = Array.from(body.querySelectorAll('.js-ph-input'));
    for (const inp of inputs) {
      if (!inp.value.trim()) { toast('请填写 ' + inp.dataset.ph, 'err'); return; }
    }
    const params = {};
    inputs.forEach(function (inp) { params[inp.dataset.ph] = inp.value.trim(); });
    post('/industry-template/deploy', { id: t.id, params: params }).then(function (r) {
      toast('部署完成：已生成节目单等配置', 'ok');
      m.remove();
    }).catch(function (e) { toast(e.message, 'err'); });
  });
}
