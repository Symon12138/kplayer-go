/* api.js — 控制台唯一的数据通道。
 * 所有请求走相对前缀 /console/api，由 Go 侧反向代理转发到后端；
 * 会话令牌存 localStorage 并以 Authorization: Bearer 头携带。 */

export const API = '/console/api';
const LS_TOKEN = 'kplayer.console.token';

export function getToken() {
  try { return localStorage.getItem(LS_TOKEN) || ''; } catch (_) { return ''; }
}
export function setToken(v) {
  try {
    if (v) { localStorage.setItem(LS_TOKEN, v); } else { localStorage.removeItem(LS_TOKEN); }
  } catch (_) { /* storage unavailable */ }
}

/* 401 时的会话失效回调，由 main.js 注册（弹出登录页）。 */
let onAuthFailure = null;
export function setAuthFailureHandler(fn) { onAuthFailure = fn; }

function request(method, path, body) {
  const headers = {};
  const token = getToken();
  if (token) {
    // 兼容旧版本在 localStorage 里存的 "Bearer xxx" 前缀格式。
    const cred = token.indexOf('Bearer ') === 0 ? token.slice(7) : token;
    headers['Authorization'] = 'Bearer ' + cred;
  }
  const opts = { method, headers };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  return fetch(API + path, opts).then(function (res) {
    const ct = (res.headers.get('content-type') || '');
    if (ct.indexOf('application/json') === -1) {
      return res.text().then(function (t) {
        if (!res.ok) { throw new Error(t || ('HTTP ' + res.status)); }
        return null;
      });
    }
    return res.text().then(function (text) {
      let data = null;
      if (text) { try { data = JSON.parse(text); } catch (_) { data = null; } }
      if (!res.ok) {
        const msg = (data && (data.message || data.error)) || ('HTTP ' + res.status);
        const err = new Error(msg);
        err.status = res.status;
        // 会话中途失效：丢弃令牌回到登录页。/auth/login 的 401 表示
        // 用户名或密码错误，由登录表单自己处理。
        if (res.status === 401 && path !== '/auth/login' && onAuthFailure) {
          onAuthFailure(err);
        }
        throw err;
      }
      return data;
    });
  });
}

export function get(path) { return request('GET', path); }
export function post(path, body) { return request('POST', path, body); }
export function patch(path, body) { return request('PATCH', path, body); }
export function del(path) { return request('DELETE', path); }

/* ---- 防御性 JSON 读取：后端契约仍在收敛，兼容裸数组 / 键控集合 / 信封 ---- */
export function unwrap(d) {
  if (d && typeof d === 'object' && !Array.isArray(d) && 'data' in d) { return d.data; }
  return d;
}
export function listOf(d, keys) {
  d = unwrap(d);
  if (Array.isArray(d)) { return d; }
  if (d && typeof d === 'object') {
    for (let i = 0; i < keys.length; i++) {
      const v = d[keys[i]];
      if (Array.isArray(v)) { return v; }
    }
  }
  return [];
}
export function objOf(d, keys) {
  d = unwrap(d);
  if (d && typeof d === 'object' && !Array.isArray(d)) {
    for (let i = 0; i < keys.length; i++) {
      if (d[keys[i]] && typeof d[keys[i]] === 'object') { return d[keys[i]]; }
    }
    return d;
  }
  return {};
}
