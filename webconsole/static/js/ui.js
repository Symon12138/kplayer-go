/* ui.js — DOM 构建助手、反馈组件与格式化工具。 */

export function el(tag, attrs, html) {
  const node = document.createElement(tag);
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

export function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
  });
}

export function $(sel, root) { return (root || document).querySelector(sel); }
export function $$(sel, root) {
  return Array.prototype.slice.call((root || document).querySelectorAll(sel));
}

/* ---- Toast ---- */
export function toast(msg, type) {
  const box = $('#toasts');
  if (!box) { return; }
  const node = el('div', { class: 'toast' + (type === 'err' ? ' toast-err' : type === 'ok' ? ' toast-ok' : '') }, esc(msg));
  box.appendChild(node);
  setTimeout(function () {
    node.style.opacity = '0';
    node.style.transition = 'opacity .25s';
    setTimeout(function () { node.remove(); }, 260);
  }, 3200);
}

/* ---- 连接状态徽章 ---- */
export function setConn(online) {
  const b = $('#connBadge');
  if (!b) { return; }
  b.textContent = online ? '在线' : '离线';
  b.className = 'badge ' + (online ? 'conn-online' : 'conn-offline');
}

/* ---- 模态框：内容 remove 时连带移除遮罩 ---- */
export function modal(content, wide) {
  const scrim = el('div', { class: 'modal-scrim' });
  if (wide) { content.classList.add('wide'); }
  scrim.appendChild(content);
  scrim.addEventListener('click', function (e) { if (e.target === scrim) { scrim.remove(); } });
  document.body.appendChild(scrim);
  const contentRemove = content.remove.bind(content);
  content.remove = function () { contentRemove(); scrim.remove(); };
  return scrim;
}

/* ---- 通用状态视图 ---- */
export function loadingView() {
  return el('div', { class: 'loading' },
    '<div class="spinner"></div><div class="muted">正在加载 ...</div>');
}

export function emptyView(msg, action) {
  const node = el('div', { class: 'empty' },
    '<div style="font-size:30px">&#128193;</div><p>' + esc(msg) + '</p>');
  if (action) { node.appendChild(action); }
  return node;
}

export function errorView(err, retry) {
  const node = el('div', { class: 'error' },
    '<div style="font-size:30px">&#9888;&#65039;</div><p><strong>加载失败</strong></p>' +
    '<pre>' + esc(err && err.message ? err.message : String(err)) + '</pre>');
  if (retry) {
    const btn = el('button', { class: 'btn', type: 'button', text: '重试' });
    btn.addEventListener('click', retry);
    node.appendChild(btn);
  }
  return node;
}

/* ---- 页头：标题 + 说明 + 右侧操作 ---- */
export function pageHead(title, desc, actions) {
  const head = el('div', { class: 'page-head' },
    '<div><h2>' + esc(title) + '</h2>' +
    (desc ? '<p class="page-desc">' + esc(desc) + '</p>' : '') + '</div>');
  if (actions && actions.length) {
    const box = el('div', { class: 'page-actions' });
    actions.forEach(function (a) { box.appendChild(a); });
    head.appendChild(box);
  }
  return head;
}

/* ---- 格式化 ---- */
export function fmtBytes(n) {
  n = Number(n) || 0;
  if (n < 1024) { return n + ' B'; }
  const u = ['KB', 'MB', 'GB', 'TB'];
  let i = -1;
  do { n = n / 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return n.toFixed(1) + ' ' + u[i];
}

export function fmtTime(v) {
  if (!v) { return '-'; }
  const d = typeof v === 'number' ? new Date(v * 1000) : new Date(v);
  if (isNaN(d.getTime())) { return String(v); }
  const p = function (x) { return (x < 10 ? '0' : '') + x; };
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

export function fmtDur(sec) {
  sec = Math.max(0, Number(sec) || 0);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = Math.floor(sec % 60);
  const p = function (x) { return (x < 10 ? '0' : '') + x; };
  return (h > 0 ? h + ':' : '') + p(m) + ':' + p(s);
}

export function fmtAgo(v) {
  if (!v) { return '-'; }
  const t = typeof v === 'number' ? v * 1000 : new Date(v).getTime();
  if (isNaN(t)) { return '-'; }
  let diff = Date.now() - t;
  if (diff < 0) { diff = 0; }
  const s = Math.floor(diff / 1000);
  if (s < 60) { return s + ' 秒前'; }
  const m = Math.floor(s / 60);
  if (m < 60) { return m + ' 分钟前'; }
  const h = Math.floor(m / 60);
  if (h < 24) { return h + ' 小时前'; }
  return Math.floor(h / 24) + ' 天前';
}
