/* views/intelligence.js — 智能编排：智能规则（按标签/时段/时长自动生成节目单）+ 建议（可审批的推荐）。 */

import { get, post, del, listOf } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, modal, pageHead, fmtAgo } from '../ui.js';

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  Promise.all([
    get('/smart-rule/list'),
    get('/suggestion/list'),
    get('/media/list')
  ]).then(function (res) {
    setConn(true);
    draw(root,
      listOf(res[0], ['rules', 'items', 'list']),
      listOf(res[1], ['suggestions', 'items', 'list']),
      listOf(res[2], ['media', 'items', 'list']));
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, rules, suggestions, media) {
  root.innerHTML = '';
  const mediaById = {};
  media.forEach(function (m) { mediaById[m.id] = m; });

  root.appendChild(pageHead('智能编排',
    '智能规则按标签/时段/时长从媒体库自动挑选内容生成节目单；建议中心存放待审批的推荐结果。'));

  /* ================= 智能规则 ================= */
  const rSec = section(root, '智能规则（' + rules.length + '）', function () { openRuleEdit(null); });
  if (!rules.length) {
    rSec.appendChild(emptyCard('暂无规则。例如：「夜间档」= 标签含"电影" + 时段 20:00-24:00 + 单条 ≤ 2 小时。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>名称</th><th>标签</th><th>时段</th><th>单条上限</th><th>防重复</th><th>状态</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    rules.forEach(function (r) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<b>' + esc(r.name) + '</b>'));
      tr.appendChild(el('td', {}, (r.tags || []).map(function (t) {
        return '<span class="badge badge-info">' + esc(t) + '</span>';
      }).join(' ') || '-'));
      tr.appendChild(el('td', { text: (r.timeSlots || []).map(function (s) { return s.startHour + ':00-' + s.endHour + ':00'; }).join(', ') || '-' }));
      tr.appendChild(el('td', { text: r.maxDurationSec ? Math.round(r.maxDurationSec / 60) + ' 分钟' : '不限' }));
      tr.appendChild(el('td', {}, r.avoidRepeat ? '<span class="badge badge-ok">开启</span>' : '<span class="badge badge-neutral">关闭</span>'));
      tr.appendChild(el('td', {}, r.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge badge-neutral">停用</span>'));

      const ops = el('td', { class: 'actions' });
      const row = el('div', { class: 'icon-btn-row' });

      const btnGen = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '生成并建单' });
      btnGen.addEventListener('click', function () {
        const name = window.prompt('生成的节目单名称：', r.name + ' - 自动编排 ' + new Date().toISOString().slice(0, 10));
        if (!name) { return; }
        btnGen.disabled = true; btnGen.textContent = '生成中 ...';
        post('/smart-rule/' + encodeURIComponent(r.id) + '/generate-and-apply', { id: r.id, playlistName: name })
          .then(function (res) {
            const pl = res && res.playlist;
            toast('已生成节目单「' + ((pl && pl.name) || name) + '」（' + ((pl && pl.items && pl.items.length) || 0) + ' 条）', 'ok');
          })
          .catch(function (e2) { toast(e2.message, 'err'); })
          .finally(function () { btnGen.disabled = false; btnGen.textContent = '生成并建单'; });
      });
      row.appendChild(btnGen);

      const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: r.enabled ? '停用' : '启用' });
      btnToggle.addEventListener('click', function () {
        post('/smart-rule/enabled', { id: r.id, enabled: !r.enabled }).then(function () { toast('已更新', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(btnToggle);

      const btnEdit = el('button', { class: 'btn btn-sm', type: 'button', text: '编辑' });
      btnEdit.addEventListener('click', function () { openRuleEdit(r); });
      row.appendChild(btnEdit);

      const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
      btnDel.addEventListener('click', function () {
        if (!window.confirm('删除规则 "' + r.name + '"？')) { return; }
        del('/smart-rule/' + encodeURIComponent(r.id)).then(function () { toast('已删除', 'ok'); render(); })
          .catch(function (e2) { toast(e2.message, 'err'); });
      });
      row.appendChild(btnDel);

      ops.appendChild(row);
      tr.appendChild(ops);
      tbody.appendChild(tr);
    });
    rSec.appendChild(wrap);
  }

  /* ================= 建议中心 ================= */
  const sSec = section(root, '建议（' + suggestions.filter(function (s) { return s.status === 'pending'; }).length + ' 待处理）', null);
  if (!suggestions.length) {
    sSec.appendChild(emptyCard('暂无建议。'));
  } else {
    const wrap = el('div', { class: 'table-wrap' });
    wrap.innerHTML = '<table class="data"><thead><tr>' +
      '<th>类型</th><th>标题</th><th>状态</th><th>创建</th><th class="actions" style="text-align:right">操作</th>' +
      '</tr></thead><tbody></tbody></table>';
    const tbody = wrap.querySelector('tbody');
    const KINDS = { media_recommend: '媒体推荐', title_generate: '标题生成' };
    suggestions.forEach(function (s) {
      const tr = el('tr', {});
      tr.appendChild(el('td', {}, '<span class="badge badge-info">' + esc(KINDS[s.kind] || s.kind || '-') + '</span>'));
      const titleTd = el('td', {}, '<b>' + esc(s.title || '-') + '</b>');
      const keys = Object.keys(s.payload || {});
      if (keys.length) {
        titleTd.appendChild(el('div', { class: 'path dim', text: keys.map(function (k) { return k + '=' + s.payload[k]; }).join('；') }));
      }
      tr.appendChild(titleTd);
      const stBadge = s.status === 'pending' ? '<span class="badge badge-warn">待处理</span>'
        : s.status === 'applied' ? '<span class="badge badge-ok">已采纳</span>'
          : '<span class="badge badge-neutral">已拒绝</span>';
      tr.appendChild(el('td', {}, stBadge));
      tr.appendChild(el('td', { class: 'dim', text: fmtAgo(s.createdAt) }));

      const ops = el('td', { class: 'actions' });
      const row = el('div', { class: 'icon-btn-row' });
      if (s.status === 'pending') {
        const ok = el('button', { class: 'btn btn-sm btn-primary', type: 'button', text: '采纳' });
        ok.addEventListener('click', function () {
          const name = s.kind === 'media_recommend'
            ? (window.prompt('采纳为节目单，名称：', s.title || '推荐节目单') || '')
            : '';
          if (s.kind === 'media_recommend' && !name) { return; }
          post('/suggestion/' + encodeURIComponent(s.id) + '/approve', { id: s.id, playlistName: name || undefined })
            .then(function () { toast('已采纳', 'ok'); render(); })
            .catch(function (e2) { toast(e2.message, 'err'); });
        });
        row.appendChild(ok);
        const no = el('button', { class: 'btn btn-sm', type: 'button', text: '拒绝' });
        no.addEventListener('click', function () {
          post('/suggestion/' + encodeURIComponent(s.id) + '/reject', { id: s.id, reason: '' })
            .then(function () { toast('已拒绝', 'ok'); render(); })
            .catch(function (e2) { toast(e2.message, 'err'); });
        });
        row.appendChild(no);
      }
      ops.appendChild(row);
      tr.appendChild(ops);
      tbody.appendChild(tr);
    });
    sSec.appendChild(wrap);
  }
}

/* ---- 小件 ---- */
function section(root, title, onNew) {
  const sec = el('div', { class: 'section' });
  const row = el('div', { class: 'row' }, '<h2>' + esc(title) + '</h2>');
  if (onNew) {
    const btn = el('button', { class: 'btn btn-primary btn-sm', type: 'button', text: '+ 新建' });
    btn.addEventListener('click', onNew);
    row.appendChild(btn);
  }
  sec.appendChild(row);
  root.appendChild(sec);
  return sec;
}

function emptyCard(msg) {
  const card = el('div', { class: 'card' });
  card.appendChild(emptyView(msg, undefined));
  return card;
}

/* ---- 智能规则模态 ---- */
function openRuleEdit(r) {
  const editing = !!r;
  const slotsVal = editing ? (r.timeSlots || []).map(function (s) { return s.startHour + '-' + s.endHour; }).join('\n') : '';
  const m = modal(el('div', { class: 'modal wide' },
    '<h3>' + (editing ? '编辑规则' : '新建智能规则') + '</h3>' +
    '<div class="form-grid two">' +
    '<div class="field"><label>名称</label><input id="rName" type="text" value="' + esc(editing ? r.name : '') + '"></div>' +
    '<div class="field"><label>说明</label><input id="rDesc" type="text" value="' + esc(editing ? (r.description || '') : '') + '"></div>' +
    '</div>' +
    '<div class="form-grid two mt">' +
    '<div class="field"><label>标签过滤（每行一个；命中任一即入选）</label>' +
    '<textarea id="rTags" class="mono" spellcheck="false" style="min-height:60px" placeholder="电影&#10;纪录片">' + esc(editing ? (r.tags || []).join('\n') : '') + '</textarea></div>' +
    '<div class="field"><label>时段（每行 起-止 小时，如 20-24）</label>' +
    '<textarea id="rSlots" class="mono" spellcheck="false" style="min-height:60px" placeholder="20-24">' + esc(slotsVal) + '</textarea></div>' +
    '</div>' +
    '<div class="form-grid three mt">' +
    '<div class="field"><label>单条时长上限（分钟，0=不限）</label><input id="rMaxDur" type="number" min="0" value="' + (editing ? Math.round((r.maxDurationSec || 0) / 60) : 0) + '"></div>' +
    '<div class="field"><label>条数上限（0=不限）</label><input id="rMaxItems" type="number" min="0" value="' + (editing ? (r.maxItems || 0) : 20) + '"></div>' +
    '<div class="field"><label>防重复回看条数</label><input id="rLookback" type="number" min="0" value="' + (editing ? (r.repeatLookback || 0) : 10) + '"></div>' +
    '</div>' +
    '<div class="cluster mt">' +
    '<label class="check"><input id="rAvoid" type="checkbox"' + (editing && r.avoidRepeat ? ' checked' : '') + '> 防重复（近 N 条不重样）</label>' +
    '<label class="check"><input id="rEnabled" type="checkbox"' + (!editing || r.enabled ? ' checked' : '') + '> 启用</label>' +
    '</div>' +
    '<div class="form-actions"><button id="rCancel" class="btn" type="button">取消</button>' +
    '<button id="rSave" class="btn btn-primary" type="button">保存</button></div>'), true);
  m.querySelector('#rCancel').addEventListener('click', function () { m.remove(); });
  m.querySelector('#rSave').addEventListener('click', function () {
    const name = m.querySelector('#rName').value.trim();
    if (!name) { toast('请填写名称', 'err'); return; }
    const timeSlots = m.querySelector('#rSlots').value.split('\n').map(function (line) {
      const mm = line.trim().match(/^(\d{1,2})\s*-\s*(\d{1,2})$/);
      return mm ? { startHour: parseInt(mm[1], 10), endHour: parseInt(mm[2], 10) } : null;
    }).filter(Boolean);
    const body = {
      name: name,
      description: m.querySelector('#rDesc').value.trim(),
      tags: m.querySelector('#rTags').value.split('\n').map(function (s) { return s.trim(); }).filter(Boolean),
      timeSlots: timeSlots,
      maxDurationSec: (parseInt(m.querySelector('#rMaxDur').value, 10) || 0) * 60,
      maxItems: parseInt(m.querySelector('#rMaxItems').value, 10) || 0,
      avoidRepeat: m.querySelector('#rAvoid').checked,
      repeatLookback: parseInt(m.querySelector('#rLookback').value, 10) || 0,
      enabled: m.querySelector('#rEnabled').checked
    };
    const req = editing
      ? post('/smart-rule/' + encodeURIComponent(r.id) + '/update', Object.assign({ id: r.id }, body))
      : post('/smart-rule/add', body);
    req.then(function () { toast(editing ? '已更新' : '已创建', 'ok'); m.remove(); render(); })
      .catch(function (e) { toast(e.message, 'err'); });
  });
}
