/* views/effects.js — 效果与插件：画面叠加效果（水印/字幕/音频/跑马灯/画面调整/转码预设）。
 * 多个效果按顺序叠加，保存后应用到推流输出（下次推流生效）。 */

import { get, post } from '../api.js';
import { el, esc, toast, setConn, loadingView, errorView, emptyView, pageHead } from '../ui.js';

const EFFECT_META = {
  'text-watermark': { label: '文字水印', desc: '画面叠加文字' },
  'image-watermark': { label: '图片水印', desc: '叠加图片 Logo' },
  'subtitle': { label: '字幕', desc: '烧录字幕文件到画面' },
  'audio': { label: '音频', desc: '音量与音画同步偏移' },
  'marquee': { label: '跑马灯', desc: '滚动字幕（右→左循环）' },
  'video-adjust': { label: '画面调整', desc: '亮度/对比度/饱和度' },
  'transcode-preset': { label: '转码预设', desc: '一键切换输出档位' }
};

export function render() {
  const root = el('div', { class: 'view-inner' });
  root.appendChild(loadingView());

  get('/effects').then(function (data) {
    setConn(true);
    const effects = Array.isArray(data.effects) ? data.effects : [];
    draw(root, effects, data.vf || '', data.af || '');
  }).catch(function (err) {
    setConn(false);
    root.innerHTML = '';
    root.appendChild(errorView(err, render));
  });

  return root;
}

function draw(root, effects, vf, af) {
  root.innerHTML = '';

  const addBtn = el('button', { class: 'btn btn-primary', type: 'button', text: '+ 添加效果' });
  addBtn.addEventListener('click', function () {
    effects.push({ id: '', type: 'text-watermark', name: '', enabled: true, params: {} });
    renderList();
  });
  const saveBtn = el('button', { class: 'btn', type: 'button', text: '保存并应用' });
  root.appendChild(pageHead('效果与插件',
    '多个效果按顺序叠加到推流画面；保存后下次推流生效。单条线路的独立滤镜在任务编辑里配置。',
    [saveBtn, addBtn]));

  /* ---- 效果编辑列表 ---- */
  const listHost = el('div', { id: 'efList', class: 'section' });
  root.appendChild(listHost);

  function effectParamsHTML(e) {
    const p = e.params || {};
    switch (e.type) {
      case 'text-watermark':
        return '<div class="form-grid two">' +
          '<div class="field"><label>文字</label><input class="ep-text" value="' + esc(p.text || '') + '" placeholder="例如：KPlayer 直播"></div>' +
          '<div class="field"><label>位置</label><select class="ep-position">' +
          '<option value="tl">左上</option><option value="tr">右上</option><option value="bl">左下</option><option value="br">右下</option><option value="c">居中</option></select></div>' +
          '<div class="field"><label>字号</label><input class="ep-font_size" type="number" min="8" max="200" value="' + esc(p.font_size || '28') + '"></div>' +
          '<div class="field"><label>颜色（white/red/#RRGGBB）</label><input class="ep-color" value="' + esc(p.color || 'white') + '"></div>' +
          '<div class="field"><label>透明度（0-1）</label><input class="ep-opacity" type="number" min="0" max="1" step="0.1" value="' + esc(p.opacity || '1') + '"></div>' +
          '</div>';
      case 'image-watermark':
        return '<div class="form-grid two">' +
          '<div class="field"><label>图片路径</label><input class="ep-path mono" value="' + esc(p.path || '') + '" placeholder="/data/logo.png"></div>' +
          '<div class="field"><label>位置</label><select class="ep-position">' +
          '<option value="tl">左上</option><option value="tr">右上</option><option value="bl">左下</option><option value="br">右下</option><option value="c">居中</option></select></div>' +
          '</div>';
      case 'subtitle':
        return '<div class="form-grid two">' +
          '<div class="field"><label>字幕文件</label><input class="ep-path mono" value="' + esc(p.path || '') + '" placeholder="/data/sub.srt"></div>' +
          '<div class="field"><label>字号</label><input class="ep-font_size" type="number" min="8" max="100" value="' + esc(p.font_size || '18') + '"></div>' +
          '<div class="field"><label>颜色（#RRGGBB）</label><input class="ep-color" value="' + esc(p.color || '#FFFFFF') + '"></div>' +
          '<div class="field"><label>对齐</label><select class="ep-alignment">' +
          '<option value="1">底部左</option><option value="2">底部居中</option><option value="3">底部右</option><option value="5">顶部居中</option></select></div>' +
          '</div>';
      case 'audio':
        return '<div class="form-grid two">' +
          '<div class="field"><label>音量（%）</label><input class="ep-volume" type="number" min="0" max="300" value="' + esc(p.volume || '100') + '"></div>' +
          '<div class="field"><label>音频偏移（毫秒，正=延后；音画不同步时微调）</label><input class="ep-delay_ms" type="number" min="-5000" max="5000" value="' + esc(p.delay_ms || '0') + '"></div>' +
          '</div>';
      case 'marquee':
        return '<div class="form-grid two">' +
          '<div class="field"><label>文字</label><input class="ep-text" value="' + esc(p.text || '') + '" placeholder="例如：欢迎收看直播"></div>' +
          '<div class="field"><label>位置</label><select class="ep-position">' +
          '<option value="top">顶部</option><option value="middle">中部</option><option value="bottom">底部</option></select></div>' +
          '<div class="field"><label>字号</label><input class="ep-font_size" type="number" min="8" max="200" value="' + esc(p.font_size || '24') + '"></div>' +
          '<div class="field"><label>滚动速度（像素/秒）</label><input class="ep-speed" type="number" min="10" max="500" value="' + esc(p.speed || '60') + '"></div>' +
          '<div class="field"><label>颜色</label><input class="ep-color" value="' + esc(p.color || 'white') + '"></div>' +
          '<div class="field"><label>透明度（0-1）</label><input class="ep-opacity" type="number" min="0" max="1" step="0.1" value="' + esc(p.opacity || '1') + '"></div>' +
          '</div>';
      case 'video-adjust':
        return '<div class="form-grid three">' +
          '<div class="field"><label>亮度（-1 ~ 1）</label><input class="ep-brightness" type="number" step="0.05" value="' + esc(p.brightness || '0') + '"></div>' +
          '<div class="field"><label>对比度（0 ~ 2）</label><input class="ep-contrast" type="number" step="0.05" value="' + esc(p.contrast || '1') + '"></div>' +
          '<div class="field"><label>饱和度（0 ~ 3）</label><input class="ep-saturation" type="number" step="0.05" value="' + esc(p.saturation || '1') + '"></div>' +
          '</div>';
      case 'transcode-preset':
        return '<div class="form-grid">' +
          '<div class="field"><label>档位（应用到推流输出）</label><select class="ep-preset">' +
          '<option value="1080p">1080p 超清（1920x1080 / 4000kbps）</option>' +
          '<option value="720p">720p 高清（1280x720 / 2500kbps）</option>' +
          '<option value="480p">480p 标清（854x480 / 1200kbps）</option>' +
          '<option value="360p">360p 流畅（640x360 / 800kbps）</option></select></div>' +
          '</div>';
    }
    return '';
  }

  function effectParamRead(e, rowEl) {
    const p = e.params || {};
    const read = function (cls, key) {
      const n = rowEl.querySelector('.' + cls);
      if (n) { p[key] = n.value.trim(); }
    };
    switch (e.type) {
      case 'text-watermark': read('ep-text', 'text'); read('ep-position', 'position'); read('ep-font_size', 'font_size'); read('ep-color', 'color'); read('ep-opacity', 'opacity'); break;
      case 'image-watermark': read('ep-path', 'path'); read('ep-position', 'position'); break;
      case 'subtitle': read('ep-path', 'path'); read('ep-font_size', 'font_size'); read('ep-color', 'color'); read('ep-alignment', 'alignment'); break;
      case 'audio': read('ep-volume', 'volume'); read('ep-delay_ms', 'delay_ms'); break;
      case 'marquee': read('ep-text', 'text'); read('ep-position', 'position'); read('ep-font_size', 'font_size'); read('ep-speed', 'speed'); read('ep-color', 'color'); read('ep-opacity', 'opacity'); break;
      case 'video-adjust': read('ep-brightness', 'brightness'); read('ep-contrast', 'contrast'); read('ep-saturation', 'saturation'); break;
      case 'transcode-preset': read('ep-preset', 'preset'); break;
    }
    e.params = p;
    return e;
  }

  function addRow(e) {
    const meta = EFFECT_META[e.type] || {};
    const row = el('div', { class: 'list-item' },
      '<div class="list-item-head"><span class="list-item-title">' + esc(meta.label || e.type) +
      (e.name ? ' <span class="dim">· ' + esc(e.name) + '</span>' : '') +
      '</span><div class="icon-btn-row"></div></div>' +
      '<div class="list-item-meta"><span class="badge ' + (e.enabled ? 'badge-ok' : 'badge-neutral') + '">' + (e.enabled ? '启用' : '停用') + '</span>' +
      '<span class="muted">' + esc(meta.desc || '') + '</span></div>' +
      '<div class="form-grid two mt">' +
      '<div class="field"><label>名称（可选）</label><input class="ep-name" value="' + esc(e.name || '') + '"></div>' +
      '<div class="field"><label>类型（更改后请重新填写参数）</label><select class="ep-type">' +
      Object.keys(EFFECT_META).map(function (t2) { return '<option value="' + t2 + '">' + esc(EFFECT_META[t2].label) + '</option>'; }).join('') +
      '</select></div>' +
      '</div>' +
      '<div class="ef-params mt"></div>');

    const typeSel = row.querySelector('.ep-type');
    typeSel.value = e.type;
    const paramsHost = row.querySelector('.ef-params');
    function renderParams() {
      e.type = typeSel.value;
      paramsHost.innerHTML = effectParamsHTML(e);
      const posSel = paramsHost.querySelector('.ep-position');
      if (posSel && e.params && e.params.position) { posSel.value = e.params.position; }
      const alSel = paramsHost.querySelector('.ep-alignment');
      if (alSel && e.params && e.params.alignment) { alSel.value = e.params.alignment; }
    }
    typeSel.addEventListener('change', function () { e.params = {}; renderParams(); });
    renderParams();
    row.querySelector('.ep-name').addEventListener('input', function () { e.name = this.value; });

    const ops = row.querySelector('.icon-btn-row');
    const btnUp = el('button', { class: 'btn btn-sm', type: 'button', text: '↑' });
    btnUp.addEventListener('click', function () {
      const i = effects.indexOf(e);
      if (i > 0) { effects.splice(i, 1); effects.splice(i - 1, 0, e); renderList(); }
    });
    ops.appendChild(btnUp);
    const btnDown = el('button', { class: 'btn btn-sm', type: 'button', text: '↓' });
    btnDown.addEventListener('click', function () {
      const i = effects.indexOf(e);
      if (i >= 0 && i < effects.length - 1) { effects.splice(i, 1); effects.splice(i + 1, 0, e); renderList(); }
    });
    ops.appendChild(btnDown);
    const btnToggle = el('button', { class: 'btn btn-sm', type: 'button', text: e.enabled ? '停用' : '启用' });
    btnToggle.addEventListener('click', function () { e.enabled = !e.enabled; renderList(); });
    ops.appendChild(btnToggle);
    const btnDel = el('button', { class: 'btn btn-sm btn-danger', type: 'button', text: '删除' });
    btnDel.addEventListener('click', function () {
      const i = effects.indexOf(e);
      if (i >= 0) { effects.splice(i, 1); renderList(); }
    });
    ops.appendChild(btnDel);
    return row;
  }

  function renderList() {
    listHost.innerHTML = '';
    if (!effects.length) {
      const act = el('button', { class: 'btn btn-primary', type: 'button', text: '添加第一个效果' });
      act.addEventListener('click', function () {
        effects.push({ id: '', type: 'text-watermark', name: '', enabled: true, params: {} });
        renderList();
      });
      const card = el('div', { class: 'card' });
      card.appendChild(emptyView('还没有效果：文字/图片水印、字幕、跑马灯、画面调整等。', act));
      listHost.appendChild(card);
      return;
    }
    const stack = el('div', { class: 'list-stack' });
    effects.forEach(function (e) { stack.appendChild(addRow(e)); });
    listHost.appendChild(stack);
  }
  renderList();

  /* ---- 保存 ---- */
  saveBtn.addEventListener('click', function () {
    saveBtn.disabled = true; saveBtn.textContent = '保存中 ...';
    const rows = Array.prototype.slice.call(listHost.querySelectorAll('.list-item'));
    const out = [];
    rows.forEach(function (rowEl, i) {
      const e = effects[i];
      effectParamRead(e, rowEl);
      out.push({
        id: e.id,
        type: e.type,
        name: rowEl.querySelector('.ep-name').value.trim() || e.type,
        enabled: e.enabled,
        params: e.params || {}
      });
    });
    post('/effects', { effects: out }).then(function () {
      toast('已保存并应用到推流输出', 'ok');
      setTimeout(render, 900);
    }).catch(function (e2) { toast(e2.message, 'err'); })
      .finally(function () { saveBtn.disabled = false; saveBtn.textContent = '保存并应用'; });
  });

  /* ---- 当前已应用的滤镜 ---- */
  root.appendChild(el('div', { class: 'card' },
    '<h2>当前已应用</h2><div class="kv">' +
    '<div><dt>视频滤镜 (-vf)</dt><dd class="mono">' + esc(vf || '（无）') + '</dd></div>' +
    '<div><dt>音频滤镜 (-af)</dt><dd class="mono">' + esc(af || '（无）') + '</dd></div>' +
    '</div>'));
}
