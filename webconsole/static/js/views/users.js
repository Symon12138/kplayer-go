/* views/users.js — 用户与权限（仅管理员可见）。
 * 角色：admin 全权 / operator 播控 / auditor 只读。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

const ROLES = [['admin', '管理员'], ['operator', '操作员'], ['auditor', '审计（只读）']];

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/user/list').then(function (res) {
    setConn(true);
    draw(root, listOf(res, ['users', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function roleLabel(r) {
  const hit = ROLES.find(function (x) { return x[0] === r; });
  return hit ? hit[1] : (r || '-');
}

function draw(root, users) {
  root.innerHTML = '';

  const newBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 新建用户' });
  newBtn.addEventListener('click', function () { openEdit(null); });
  root.appendChild(pageHead('用户与权限',
    '控制台登录账号与角色：admin 全权，operator 可播控，auditor 只读。', [newBtn]));

  if (!users.length) {
    const act = el('button', { class: 'btn btn-primary', type: 'button', text: '创建第一个用户' });
    act.addEventListener('click', function () { openEdit(null); });
    root.appendChild(el('div', { class: 'card' }, emptyView('还没有用户。开启鉴权后需要账号登录。', act)));
    return;
  }

  const wrap = el('div', { class: 'table-wrap' });
  wrap.innerHTML = '<table class="data"><thead><tr>' +
    '<th>用户名</th><th>角色</th><th>状态</th><th>更新</th><th class="actions" style="text-align:right">操作</th>' +
    '</tr></thead><tbody></tbody></table>';
  const tbody = wrap.querySelector('tbody');

  users.forEach(function (u) {
    const tr = el('tr', {});
    tr.appendChild(el('td', {}, '<b>' + esc(u.username) + '</b>'));
    tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(roleLabel(u.role)) + '</span>'));
    tr.appendChild(el('td', {}, u.enabled
      ? '<span class="badge badge-ok">启用</span>'
      : '<span class="badge badge-neutral">停用</span>'));
    tr.appendChild(el('td', { class: 'dim', text: fmtAgo(u.updatedAt) }));

    const ops = el('td', { class: 'actions' });
    const row = el('div', { class: 'icon-btn-row' });

    const btnPwd = el('button', { class: 'btn btn-sm', type: 'button', text: '改密' });
    btnPwd.addEventListener('click', function () {
      const pwd = window.prompt('为 "' + u.username + '" 设置新密码：');
      if (!pwd) { return; }
      post('/user/' + encodeURIComponent(u.id) + '/password', { password: pwd })
        .then(function () { toast('密码已更新', 'ok'); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnPwd);

    const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: u.enabled ? '停用' : '启用' });
    btnToggle.addEventListener('click', function () {
      post('/user/' + encodeURIComponent(u.id) + '/enabled', { enabled: !u.enabled })
        .then(function () { toast('已更新', 'ok'); render(); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnToggle);

    const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
    btnEdit.addEventListener('click', function () { openEdit(u); });
    row.appendChild(btnEdit);

    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      if (!window.confirm('删除用户 "' + u.username + '"？其会话将立即失效。')) { return; }
      del('/user/' + encodeURIComponent(u.id)).then(function () { toast('已删除', 'ok'); render(); })
        .catch(function (e2) { toast(e2.message, 'err'); });
    });
    row.appendChild(btnDel);

    ops.appendChild(row);
    tr.appendChild(ops);
    tbody.appendChild(tr);
  });

  root.appendChild(wrap);
}

function openEdit(u) {
  const editing = !!u;
  const m = modal(el('div', { class: 'modal' },
    '<h3>' + (editing ? '编辑用户 — ' + esc(u.username) : '新建用户') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>用户名</label><input id="uName" type="text" value="' + esc(editing ? u.username : '') + '"' + (editing ? ' disabled' : '') + '></div>' +
    '<div class="field"><label>角色</label><select id="uRole">' +
    ROLES.map(function (r) { return '<option value="' + r[0] + '">' + r[1] + '</option>'; }).join('') +
    '</select></div>' +
    '</div>' +
    (!editing
      ? '<div class="field mt"><label>密码</label><input id="uPass" type="password" autocomplete="new-password"></div>'
      : '') +
    '<div class="cluster mt"><label class="check"><input id="uEnabled" type="checkbox"' + (!editing || u.enabled ? ' checked' : '') + '> 启用</label></div>' +
    '<div class="form-actions"><button id="uCancel" class="btn" type="button">取消</button>' +
    '<button id="uSave" class="btn btn-primary" type="button">保存</button></div>'));

  if (editing) { m.querySelector('#uRole').value = u.role || 'operator'; }
  m.querySelector('#uCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#uSave').addEventListener('click', function () {
    const role = m.querySelector('#uRole').value;
    const enabled = m.querySelector('#uEnabled').checked;
    let req;
    if (editing) {
      req = post('/user/' + encodeURIComponent(u.id) + '/update', Object.assign({ id: u.id }, { username: u.username, role: role, enabled: enabled }));
    } else {
      const username = m.querySelector('#uName').value.trim();
      const password = m.querySelector('#uPass').value;
      if (!username) { toast('请填写用户名', 'err'); return; }
      if (!password) { toast('请填写密码', 'err'); return; }
      req = post('/user', { username: username, password: password, role: role, enabled: enabled });
    }
    req.then(function () { toast(editing ? '用户已更新' : '用户已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
