/* views/scenes.js — 场景模板：预设一组画面/播出参数，一键套用到播放。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/scene-template/list').then(function (res) {
    setConn(true);
    draw(root, listOf(res, ['sceneTemplates', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, templates) {
  root.innerHTML = '';

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建模板' });
  newBtn.addEventListener('click', function () { openEdit(null); });
  root.appendChild(pageHead('场景模板',
    '把常用参数组合存成模板（如「门店营业时间」「校园课间」），播放时可按 sceneTemplateId 套用。', [newBtn]));

  if (!templates.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '新建第一个模板' });
    act.addEventListener('click', function () { openEdit(null); });
    root.appendChild(el('div', { class: 'card' }, emptyView('暂无场景模板。', act)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>名称</th><th>类型</th><th>参数</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  templates.forEach(function (t) {
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<b>' + esc(t.name) + '</b>'));
    tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(t.kind || '-') + '</span>'));
    const pTd = el('td', { class: 'cell-path' });
    const keys = Object.keys(t.params || {});
    if (!keys.length) { pTd.textContent = '-'; }
    else {
      keys.forEach(function (k) {
        pTd.appendChild(el('div', { class: 'path', text: k + ' = ' + (t.params[k] || '') }));
      });
    }
    tr.appendChild(pTd);
    tr.appendChild(el('td', {}, t.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });

    const btnDup = el('button', { class: 'btn btn-sm', type: 'button', text: '复制' });
    btnDup.addEventListener('click', function () {
      openEdit(Object.assign({}, t, { id: '', name: t.name + ' 副本' }));
    });
    row.appendChild(btnDup);

    const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: t.enabled ? '停用' : '启用' });
    btnToggle.addEventListener('click', function () {
      post('/scene-template/enabled', { id: t.id, enabled: !t.enabled }).then(function () {
        toast('已更新', 'ok'); render();
      }).catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnToggle);

    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openEdit(t); });
    row.appendChild(btnEdit);

    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('删除模板 "' + t.name + '"？')) { return; }
      del('/scene-template/' + encodeURIComponent(t.id)).then(function () { toast('已删除', 'ok'); render(); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnDel);

    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}

function openEdit(t) {
  const editing = !!t && !!t.id;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑模板' : '新建模板') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="tName" type="text" value="' + esc(editing ? t.name : '') + '"></div>' +
    '<div class="field"><label>类型（kind，自定义标识）</label><input id="tKind" type="text" class="mono" placeholder="如 store-hours" value="' + esc(editing ? (t.kind || '') : '') + '"></div>' +
    '</div>' +
    '<div class="field mt"><label>参数（每行一个 key = value）</label>' +
    '<textarea id="tParams" class="mono" spellcheck="false" style="min-height:110px" placeholder="start = 08:00&#10;end = 22:00">' +
    esc(editing ? Object.keys(t.params || {}).map(function (k) { return k + ' = ' + (t.params[k] || ''); }).join('\n') : '') +
    '</textarea></div>' +
    '<div class="cluster mt"><label class="check"><input id="tEnabled" type="checkbox"' + (!editing || t.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="tCancel" class="btn" type="button">取消</button>' +
    '<button id="tSave" class="btn btn-primary" type="button">保存</button></div>'));
  m.querySelector('#tCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#tSave').addEventListener('click', function () {
    const name = m.querySelector('#tName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    const params = {};
    m.querySelector('#tParams').value.split('\n').forEach(function (line) {
      const i = line.indexOf('=');
      if (i > 0) {
        const k = line.slice(0, i).trim();
        const v = line.slice(i + 1).trim();
        if (k) { params[k] = v; }
      }
    });
    const body = { name: name, kind: m.querySelector('#tKind').value.trim(), params: params, enabled: m.querySelector('#tEnabled').checked };
    const req = editing
      ? post('/scene-template/' + encodeURIComponent(t.id) + '/update', Object.assign({ id: t.id }, body))
      : post('/scene-template/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
