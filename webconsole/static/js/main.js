/* main.js — 应用入口：导航构建、hash 路由、轮询与启动流程。 */

import { setAuthFailureHandler, setToken, get, post, unwrap } from './api.js';
import { el, $, $$, esc } from './ui.js';
import { icon } from './icons.js';
import * as overview from './views/overview.js';
import * as streams from './views/streams.js';
import * as media from './views/media.js';
import * as playlists from './views/playlists.js';
import * as tasks from './views/tasks.js';
import * as effects from './views/effects.js';

const REFRESH_MS = 5000;

/* 导航定义：分组 → 页面。auto = 活动页每 5 秒自动刷新。 */
const NAV = [
  { group: '监控', items: [{ id: 'overview', title: '总览', icon: 'gauge', auto: true, render: overview.render }] },
  { group: '播出', items: [{ id: 'streams', title: '推流任务', icon: 'broadcast', auto: true, render: streams.render }] },
  {
    group: '内容', items: [
      { id: 'media', title: '媒体库', icon: 'film', render: media.render },
      { id: 'playlist', title: '节目单', icon: 'list', render: playlists.render },
      { id: 'tasks', title: '定时任务', icon: 'clock', render: tasks.render }
    ]
  },
  { group: '画面', items: [{ id: 'effects', title: '效果与插件', icon: 'wand', render: effects.render }] }
];

const state = { current: 'overview', authed: false, currentUser: null };

function viewsMap() {
  const map = {};
  NAV.forEach(function (g) { g.items.forEach(function (v) { map[v.id] = v; }); });
  return map;
}

/* ---- 登录（仅在鉴权模式下可见） ---- */
function showLogin() {
  state.authed = false;
  $('#login').hidden = false;
}

function bindLogin() {
  $('#loginForm').addEventListener('submit', function (e) {
    e.preventDefault();
    const username = $('#loginUser').value.trim();
    const password = $('#loginPass').value;
    if (!username || !password) { loginError('请输入用户名和密码。'); return; }
    const btn = $('#loginBtn');
    btn.disabled = true;
    btn.textContent = '登录中 ...';
    post('/auth/login', { username: username, password: password }).then(function (data) {
      const d = unwrap(data) || {};
      if (!d.token) { throw new Error('登录响应未包含令牌'); }
      setToken(d.token);
      state.currentUser = { username: d.username || username, role: d.role || 'user' };
      btn.disabled = false;
      btn.textContent = '登 录';
      enterApp();
    }).catch(function (err) {
      btn.disabled = false;
      btn.textContent = '登 录';
      loginError(err.status === 401 ? '用户名或密码错误。' : (err.message || String(err)));
    });
  });
}

function loginError(msg) {
  const n = $('#loginErr');
  n.textContent = msg;
  n.hidden = false;
}

/* ---- 路由 ---- */
function currentView() {
  const h = (location.hash || '').replace('#', '');
  return viewsMap()[h] ? h : 'overview';
}

function render() {
  if (!state.authed) { return; }
  const v = currentView();
  state.current = v;
  $$('.nav-item[data-view]').forEach(function (b) {
    b.classList.toggle('is-active', b.dataset.view === v);
  });
  const view = viewsMap()[v];
  $('#pageTitle').textContent = view.title;
  document.title = view.title + ' - KPlayer 播控台';
  closeSidebar();
  const host = $('#view');
  host.innerHTML = '';
  host.appendChild(view.render());
}

function closeSidebar() {
  $('#sidebar').classList.remove('is-open');
  $('#menuBtn').setAttribute('aria-expanded', 'false');
  $('#scrim').hidden = true;
}

/* ---- 导航构建 ---- */
function buildNav() {
  const nav = $('#nav');
  nav.innerHTML = '';
  NAV.forEach(function (g) {
    const group = el('div', { class: 'nav-group' });
    group.appendChild(el('div', { class: 'nav-label', text: g.group }));
    g.items.forEach(function (v) {
      const b = el('button', { class: 'nav-item', type: 'button', 'data-view': v.id, 'data-title': v.title });
      b.innerHTML = icon(v.icon) + '<span>' + esc(v.title) + '</span>';
      b.addEventListener('click', function () { location.hash = v.id; });
      group.appendChild(b);
    });
    nav.appendChild(group);
  });
}

function bindChrome() {
  window.addEventListener('hashchange', render);
  $('#menuBtn').addEventListener('click', function () {
    const open = $('#sidebar').classList.toggle('is-open');
    $('#menuBtn').setAttribute('aria-expanded', String(open));
    $('#scrim').hidden = !open;
  });
  $('#scrim').addEventListener('click', closeSidebar);
  $('#collapseBtn').addEventListener('click', closeSidebar);
  $('#refreshBtn').addEventListener('click', render);
}

/* ---- 轮询：仅自动刷新 live 页面，页面隐藏时暂停 ---- */
let pollTimer = null;
function startPolling() {
  if (pollTimer) { return; }
  pollTimer = setInterval(function () {
    if (!state.authed || state.loading || document.hidden) { return; }
    const v = viewsMap()[state.current];
    if (v && v.auto) { render(); }
  }, REFRESH_MS);
}

/* ---- 启动 ---- */
function enterApp() {
  state.authed = true;
  $('#login').hidden = true;
  render();
  startPolling();
}

document.addEventListener('DOMContentLoaded', function () {
  buildNav();
  bindChrome();
  bindLogin();
  /* 401 → 弹登录页（鉴权模式下的会话失效路径） */
  setAuthFailureHandler(function () { showLogin(); });
  /* 无鉴权模式（默认内网部署）：直接进入控制台。
   * 鉴权模式下首个请求会得到 401 并触发登录页。 */
  state.currentUser = { username: 'admin', role: 'admin' };
  enterApp();
  /* 预检一次：鉴权开启时 401 会切到登录页 */
  get('/auth/me').catch(function () { /* 由 401 处理器接管 */ });
});
