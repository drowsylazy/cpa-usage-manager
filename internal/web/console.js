/* CPA 用量管理 · 控制台逻辑
   单文件、零依赖；密钥仅存 sessionStorage，全部数据经宿主管理 API 加载。 */
(function () {
'use strict';

// ---------- 工具 ----------
const $ = id => document.getElementById(id);
const API = '/v0/management/plugins/cpa-usage-manager';
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pad2 = n => String(n).padStart(2, '0');

function fmtInt(n) {
  n = Number(n) || 0;
  const a = Math.abs(n);
  if (a >= 1e8) return (n / 1e8).toFixed(a >= 1e10 ? 0 : 1) + ' 亿';
  if (a >= 1e4) return (n / 1e4).toFixed(a >= 1e6 ? 0 : 1) + ' 万';
  return n.toLocaleString('zh-CN');
}
// fmtTok Token 计数专用：按 K / M / B 自动升级（<1000 原样显示），整数部分满三位后省去小数；
// 升级阈值取 999.5 的倍数，四舍五入后满千的值（如 999500）直接进位到更大单位（1M 而非 1000K）。
function fmtTok(n) {
  n = Math.round(Number(n) || 0);
  const a = Math.abs(n);
  const dec = s => s.toFixed(Math.abs(s) >= 100 ? 0 : 1).replace(/\.0$/, '');
  if (a >= 999.5e6) return dec(n / 1e9) + 'B';
  if (a >= 999.5e3) return dec(n / 1e6) + 'M';
  if (a >= 999.5) return dec(n / 1e3) + 'K';
  return String(n);
}
// parseTokens 解析 token 数输入，接受 1000 / 1k / 1.5m / 2b 与含千分位逗号的写法。
// 空串返回 null（表示不限），非法输入抛错由调用方转成提示。
function parseTokens(raw) {
  const s = String(raw ?? '').trim().replace(/[,，\s_]/g, '');
  if (!s) return null;
  const m = /^(\d+(?:\.\d+)?)([kKmMbB])?$/.exec(s);
  if (!m) throw new Error('Token 限额格式非法：' + raw + '（可写 500000 或 500k / 1.5m）');
  const mult = { k: 1e3, m: 1e6, b: 1e9 }[(m[2] || '').toLowerCase()] || 1;
  const n = Math.round(parseFloat(m[1]) * mult);
  if (!Number.isFinite(n) || n < 0) throw new Error('Token 限额必须为非负整数');
  if (!Number.isSafeInteger(n)) throw new Error('Token 限额过大');
  return n;
}
function fmtUSD(micro) {
  if (micro === null || micro === undefined) return '不限';
  const v = (Number(micro) || 0) / 1e6, neg = v < 0, a = Math.abs(v);
  let s;
  if (a === 0) s = '0';
  else if (a < 0.01) s = a.toFixed(6);
  else if (a < 1) s = a.toFixed(4);
  else s = a.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  s = s.replace(/(\.\d*?)0+$/, '$1').replace(/\.$/, '');
  return (neg ? '-$' : '$') + s;
}
const fmtPrice = p => (Number(p) || 0) === 0 ? '0' : fmtUSD(p);
function fmtSec(ms) {
  ms = Number(ms) || 0;
  if (ms <= 0) return '-';
  if (ms < 1000) return ms + ' ms';
  return (ms / 1000).toFixed(2) + ' s';
}
function fmtBytes(b) {
  b = Number(b) || 0;
  if (b >= 1 << 20) return (b / (1 << 20)).toFixed(1) + ' MB';
  if (b >= 1 << 10) return (b / (1 << 10)).toFixed(1) + ' KB';
  return b + ' B';
}
function fmtDT(ts, withSec) {
  if (!ts) return '-';
  const d = new Date(ts), now = new Date();
  const md = pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  const t = pad2(d.getHours()) + ':' + pad2(d.getMinutes()) + (withSec ? ':' + pad2(d.getSeconds()) : '');
  return d.getFullYear() === now.getFullYear() ? md + ' ' + t : d.getFullYear() + '-' + md + ' ' + t;
}
function rel(ts) {
  if (!ts) return '从未使用';
  const s = (Date.now() - new Date(ts).getTime()) / 1000;
  if (s < 60) return '刚刚';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  if (s < 86400 * 30) return Math.floor(s / 86400) + ' 天前';
  return fmtDT(ts);
}
function debounce(fn, ms) { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; }
function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) return navigator.clipboard.writeText(text);
  return new Promise((res, rej) => {
    const ta = document.createElement('textarea');
    ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta); ta.select();
    try { document.execCommand('copy') ? res() : rej(new Error('copy 失败')); } catch (e) { rej(e); }
    ta.remove();
  });
}
function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast' + (kind ? ' ' + kind : '');
  el.textContent = msg;
  $('toasts').appendChild(el);
  const kill = () => el.remove();
  el.addEventListener('click', kill);
  setTimeout(kill, kind === 'err' ? 6000 : 3500);
}

// ---------- 下拉组件 ----------
// 自建 listbox 替代原生 select / datalist：原生 select 的弹出列表无法跨浏览器统一样式，
// datalist 在 Firefox 只显示 value 不显示 label（密钥筛选会只见 kid 不见标签）。
//
// 弹层必须挂 body 且 position:fixed —— .panel{overflow:hidden} 会裁掉面板内的绝对定位弹层。
const CARET = '<svg class="sel-caret" viewBox="0 0 24 24" aria-hidden="true"><path d="m6 9 6 6 6-6"/></svg>';
const TICK = '<svg class="so-tick" viewBox="0 0 24 24" aria-hidden="true"><path d="m5 13 4 4L19 7"/></svg>';
const BOXTICK = '<span class="so-box"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="m5 13 4 4L19 7"/></svg></span>';
let openSel = null; // 当前展开的下拉，全局只允许一个

function closeAnySel(focusBack) {
  if (!openSel) return;
  const s = openSel;
  openSel = null;
  s.pop.remove();
  s.trigger.setAttribute('aria-expanded', 'false');
  if (focusBack) s.trigger.focus();
}
document.addEventListener('click', e => {
  if (openSel && !e.target.closest('.sel-pop') && !e.target.closest('.sel,.combo')) closeAnySel(false);
});
window.addEventListener('resize', () => closeAnySel(false));
// 滚动时重定位（弹层是 fixed，不随容器滚动）
window.addEventListener('scroll', () => { if (openSel) openSel.place(); }, true);

// placePop 把弹层定位到触发器下方；下方空间不足时向上翻转。
function placePop(pop, trigger) {
  const r = trigger.getBoundingClientRect();
  pop.style.visibility = 'hidden';
  pop.style.left = '0px';
  pop.style.top = '0px';
  const ph = pop.offsetHeight, pw = pop.offsetWidth;
  const below = window.innerHeight - r.bottom - 8;
  const flip = below < ph && r.top > below;
  pop.style.top = (flip ? Math.max(8, r.top - ph - 6) : r.bottom + 6) + 'px';
  pop.style.left = Math.max(8, Math.min(window.innerWidth - pw - 8, r.left)) + 'px';
  pop.style.minWidth = Math.max(r.width, 180) + 'px';
  pop.style.visibility = '';
}

// Select 单选下拉。opts: [{value,label,sub}]；onChange(value) 在选择后调用。
// 用法与原生 select 贴近：sel.value 读写当前值。
function Select(mountId, opts, onChange, o = {}) {
  const mount = $(mountId);
  const label = mount.dataset.label || '';
  let value = o.value !== undefined ? o.value : (opts[0] ? opts[0].value : '');
  let items = opts.slice();

  mount.innerHTML = '<button type="button" class="btn sel-btn" aria-haspopup="listbox" aria-expanded="false"'
    + (label ? ' aria-label="' + esc(label) + '"' : '') + '><span class="sel-text"></span>' + CARET + '</button>';
  const trigger = mount.querySelector('.sel-btn');
  const cur = () => items.find(x => x.value === value);

  function paint() {
    const c = cur();
    trigger.querySelector('.sel-text').textContent = c ? c.label : (o.placeholder || '请选择');
    // 「全部…」这类空值不算激活，避免筛选器默认态就高亮
    trigger.dataset.active = String(!!value);
  }

  function open() {
    if (openSel && openSel.trigger === trigger) { closeAnySel(true); return; }
    closeAnySel(false);
    const pop = document.createElement('div');
    pop.className = 'sel-pop';
    pop.setAttribute('role', 'listbox');
    if (label) pop.setAttribute('aria-label', label);
    pop.innerHTML = (o.head ? '<div class="pop-head">' + esc(o.head) + '</div>' : '')
      + items.map((x, i) => '<button type="button" class="sel-opt" role="option" data-i="' + i + '"'
        + ' aria-selected="' + String(x.value === value) + '">'
        + '<span class="so-main"><span class="so-name">' + esc(x.label) + '</span>'
        + (x.sub ? '<span class="so-sub">' + esc(x.sub) + '</span>' : '') + '</span>' + TICK + '</button>').join('');
    document.body.appendChild(pop);
    trigger.setAttribute('aria-expanded', 'true');
    const place = () => placePop(pop, trigger);
    place();
    openSel = { trigger, pop, place };

    const optEls = [...pop.querySelectorAll('.sel-opt')];
    let ci = Math.max(0, items.findIndex(x => x.value === value));
    const cursor = i => {
      ci = (i + optEls.length) % optEls.length;
      optEls.forEach((el, k) => el.dataset.cursor = String(k === ci));
      optEls[ci].scrollIntoView({ block: 'nearest' });
    };
    if (optEls.length) cursor(ci);
    const pick = i => {
      value = items[i].value;
      paint();
      closeAnySel(true);
      if (onChange) onChange(value);
    };
    pop.addEventListener('click', e => {
      const b = e.target.closest('.sel-opt');
      if (b) pick(+b.dataset.i);
    });
    pop.addEventListener('mousemove', e => {
      const b = e.target.closest('.sel-opt');
      if (b) cursor(+b.dataset.i);
    });
    // 键盘处理挂在触发器的常驻监听上（见下），这里只登记当次的处理函数，
    // 避免每次展开都往触发器上再加一个监听器。
    trigger._selKey = e => {
      if (e.key === 'Escape') { e.preventDefault(); closeAnySel(true); return; }
      if (e.key === 'ArrowDown') { e.preventDefault(); cursor(ci + 1); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); cursor(ci - 1); return; }
      if (e.key === 'Home') { e.preventDefault(); cursor(0); return; }
      if (e.key === 'End') { e.preventDefault(); cursor(optEls.length - 1); return; }
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); if (optEls.length) pick(ci); return; }
      if (e.key.length === 1) { // 首字符跳转
        const ch = e.key.toLowerCase();
        const from = items.findIndex((x, k) => k > ci && x.label.toLowerCase().startsWith(ch));
        const idx = from >= 0 ? from : items.findIndex(x => x.label.toLowerCase().startsWith(ch));
        if (idx >= 0) cursor(idx);
      }
    };
  }

  trigger.addEventListener('click', open);
  trigger.addEventListener('keydown', e => {
    const isOpen = trigger.getAttribute('aria-expanded') === 'true';
    if (isOpen && trigger._selKey) { trigger._selKey(e); return; }
    if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
  });
  paint();
  return {
    get value() { return value; },
    set value(v) { value = v; paint(); },
    setOptions(next) { items = next.slice(); paint(); },
  };
}

// MultiSelect 多选下拉（列偏好）。onChange(Set) 在每次勾选后调用。
function MultiSelect(mountId, opts, selected, onChange, o = {}) {
  const mount = $(mountId);
  const label = mount.dataset.label || '';
  const sel = new Set(selected);
  mount.innerHTML = '<button type="button" class="btn sel-btn" aria-haspopup="listbox" aria-expanded="false"'
    + (label ? ' aria-label="' + esc(label) + '"' : '') + '>'
    + (o.icon || '') + '<span class="sel-text"></span>' + CARET + '</button>';
  const trigger = mount.querySelector('.sel-btn');
  const paint = () => { trigger.querySelector('.sel-text').textContent = (o.text || '列') + ' ' + sel.size; };

  function open() {
    if (openSel && openSel.trigger === trigger) { closeAnySel(true); return; }
    closeAnySel(false);
    const pop = document.createElement('div');
    pop.className = 'sel-pop';
    pop.setAttribute('role', 'listbox');
    pop.setAttribute('aria-multiselectable', 'true');
    if (label) pop.setAttribute('aria-label', label);
    const render = () => {
      pop.innerHTML = (o.head ? '<div class="pop-head">' + esc(o.head) + '</div>' : '')
        + opts.map((x, i) => '<button type="button" class="sel-opt multi" role="option" data-i="' + i + '"'
          + ' aria-selected="' + String(sel.has(x.value)) + '"' + (x.fixed ? ' disabled' : '') + '>'
          + BOXTICK + '<span class="so-main"><span class="so-name">' + esc(x.label) + '</span></span></button>').join('')
        + '<div class="sel-foot"><button type="button" class="btn small" data-act="reset">恢复默认</button></div>';
    };
    render();
    document.body.appendChild(pop);
    trigger.setAttribute('aria-expanded', 'true');
    const place = () => placePop(pop, trigger);
    place();
    openSel = { trigger, pop, place };
    pop.addEventListener('click', e => {
      if (e.target.closest('[data-act="reset"]')) {
        sel.clear();
        (o.defaults || []).forEach(v => sel.add(v));
        render(); paint(); if (onChange) onChange(sel);
        return;
      }
      const b = e.target.closest('.sel-opt');
      if (!b || b.disabled) return;
      const v = opts[+b.dataset.i].value;
      if (sel.has(v)) { if (sel.size > 1) sel.delete(v); } else sel.add(v);
      render(); paint();
      if (onChange) onChange(sel);
    });
    trigger._selKey = e => { if (e.key === 'Escape') { e.preventDefault(); closeAnySel(true); } };
  }
  trigger.addEventListener('click', open);
  trigger.addEventListener('keydown', e => {
    if (trigger.getAttribute('aria-expanded') === 'true' && trigger._selKey) { trigger._selKey(e); return; }
    if (e.key === 'ArrowDown' || e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(); }
  });
  paint();
  return { get selected() { return sel; } };
}

// Combo 可输入组合框：保留手动输入，同时给出候选下拉（替代 datalist）。
function Combo(mountId, onChange) {
  const mount = $(mountId);
  const label = mount.dataset.label || '';
  let items = [];
  mount.innerHTML = '<input type="text" spellcheck="false" placeholder="'
    + esc(mount.dataset.placeholder || '') + '"' + (label ? ' aria-label="' + esc(label) + '"' : '')
    + ' role="combobox" aria-expanded="false" aria-autocomplete="list" autocomplete="off">'
    + '<button type="button" tabindex="-1" aria-label="展开候选"><svg viewBox="0 0 24 24"><path d="m6 9 6 6 6-6"/></svg></button>';
  const input = mount.querySelector('input');
  const btn = mount.querySelector('button');

  function open(filterText) {
    closeAnySel(false);
    const q = (filterText || '').trim().toLowerCase();
    const list = q
      ? items.filter(x => x.label.toLowerCase().includes(q) || (x.sub || '').toLowerCase().includes(q))
      : items.slice();
    const show = list.slice(0, 60);
    const pop = document.createElement('div');
    pop.className = 'sel-pop';
    pop.setAttribute('role', 'listbox');
    pop.innerHTML = show.length
      ? show.map((x, i) => '<button type="button" class="sel-opt" role="option" data-i="' + i + '">'
        + '<span class="so-main"><span class="so-name">' + esc(x.label) + '</span>'
        + (x.sub ? '<span class="so-sub">' + esc(x.sub) + '</span>' : '') + '</span></button>').join('')
      : '<div class="sel-empty">无匹配候选</div>';
    document.body.appendChild(pop);
    input.setAttribute('aria-expanded', 'true');
    const place = () => placePop(pop, mount);
    place();
    openSel = { trigger: input, pop, place };
    let ci = -1;
    const optEls = [...pop.querySelectorAll('.sel-opt')];
    const cursor = i => {
      ci = (i + optEls.length) % optEls.length;
      optEls.forEach((el, k) => el.dataset.cursor = String(k === ci));
      optEls[ci].scrollIntoView({ block: 'nearest' });
    };
    const pick = i => {
      // 值取 sub（kid）优先，没有则取 label：密钥筛选要提交 kid，模型筛选提交模型名
      input.value = show[i].value;
      closeAnySel(false);
      input.focus();
      mount.dataset.active = String(!!input.value);
      if (onChange) onChange(input.value);
    };
    pop.addEventListener('click', e => {
      const b = e.target.closest('.sel-opt');
      if (b) pick(+b.dataset.i);
    });
    pop.addEventListener('mousemove', e => {
      const b = e.target.closest('.sel-opt');
      if (b) cursor(+b.dataset.i);
    });
    input._onKey = e => {
      if (e.key === 'Escape') { e.preventDefault(); closeAnySel(false); return; }
      if (e.key === 'ArrowDown') { e.preventDefault(); if (optEls.length) cursor(ci + 1); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); if (optEls.length) cursor(ci - 1); return; }
      if (e.key === 'Enter' && ci >= 0) { e.preventDefault(); pick(ci); }
    };
  }
  input.addEventListener('keydown', e => {
    if (openSel && openSel.trigger === input && input._onKey) { input._onKey(e); return; }
    if (e.key === 'ArrowDown') { e.preventDefault(); open(input.value); }
  });
  input.addEventListener('blur', () => { input.setAttribute('aria-expanded', 'false'); });
  btn.addEventListener('click', () => {
    if (openSel && openSel.trigger === input) { closeAnySel(false); return; }
    open('');
    input.focus();
  });
  input.addEventListener('input', debounce(() => {
    mount.dataset.active = String(!!input.value.trim());
    if (onChange) onChange(input.value.trim());
  }, 400));
  return {
    get value() { return input.value.trim(); },
    setOptions(next) { items = next.slice(); },
  };
}

// ---------- 会话与 API ----------
let key = sessionStorage.getItem('cpa-management-key') || '';

async function api(path, opts = {}) {
  const isRaw = opts.body instanceof Blob || opts.body instanceof File;
  const r = await fetch(API + path, {
    method: opts.method || 'GET',
    headers: Object.assign(
      { Authorization: 'Bearer ' + key },
      opts.body !== undefined && !isRaw ? { 'Content-Type': 'application/json' } : {},
      opts.headers || {}
    ),
    body: opts.body,
  });
  if (r.status === 401) { logout(); throw new Error('管理密钥无效或已失效'); }
  if (!r.ok) {
    let msg = 'HTTP ' + r.status;
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) { /* 非 JSON 错误体 */ }
    throw new Error(withStorageHint(msg));
  }
  const ct = r.headers.get('Content-Type') || '';
  return ct.includes('json') ? r.json() : r;
}

// withStorageHint 为存储层错误补上处置建议。
//
// 后端在重试耗尽后已经带上中文成因（见 store.transientCause），此时不再追加，
// 避免同一句话出现两遍；只有透出的是原始英文 SQLite 错误时才由前端补一句。
function withStorageHint(msg) {
  if (/malformed|SQLITE_CORRUPT|not a database/i.test(msg))
    return msg + ' —— 数据库文件已损坏，重试无效：请在「系统」页备份后重建数据库';
  if (/数据目录|已重试|检查 data_dir/.test(msg)) return msg;
  if (/database is locked|SQLITE_BUSY|SQLITE_LOCKED/i.test(msg))
    return msg + ' —— 数据库被其他进程占用；检查是否有第二个宿主实例共用同一 data_dir';
  if (/unable to open database file/i.test(msg))
    return msg + ' —— 无法打开数据库或临时文件；检查 data_dir 权限、磁盘剩余空间与 TEMP 目录';
  if (/disk I\/O error|SQLITE_IOERR/i.test(msg))
    return msg + ' —— 数据目录可能被杀毒软件/同步盘占用，或磁盘空间异常；建议把 data_dir 加入杀毒排除列表并移出同步盘';
  return msg;
}
const post = (path, body) => api(path, { method: 'POST', body: JSON.stringify(body) });

function logout() {
  sessionStorage.removeItem('cpa-management-key');
  key = '';
  $('app').hidden = true;
  $('gate').hidden = false;
  $('gate-key').focus();
}

// ---------- 全局缓存 ----------
const S = { fx: null, stats: null };

// ---------- 主题 ----------
// 无手动切换：始终跟随宿主页面（嵌入时镜像 management.html 的深浅色：
// class / data-theme / 背景亮度三重探测，MutationObserver 实时跟随）；
// 独立打开时回退系统偏好。
function parentDoc() {
  try { return window.parent && window.parent !== window ? window.parent.document : null; }
  catch (_) { return null; } // 跨域 iframe
}
function detectParentDark() {
  const doc = parentDoc();
  if (!doc) return null;
  const els = [doc.documentElement, doc.body];
  for (const el of els) {
    if (!el) continue;
    const dt = el.getAttribute('data-theme') || el.getAttribute('data-color-mode') || '';
    if (/dark|night|black/i.test(dt)) return true;
    if (/light|day|white/i.test(dt)) return false;
    if (el.classList.contains('dark') || el.classList.contains('dark-mode') || el.classList.contains('theme-dark')) return true;
    if (el.classList.contains('light') || el.classList.contains('light-mode') || el.classList.contains('theme-light')) return false;
  }
  for (const el of els) {
    if (!el) continue;
    const bg = getComputedStyle(el).backgroundColor;
    const m = bg && bg.match(/rgba?\((\d+)[,\s]+(\d+)[,\s]+(\d+)/);
    if (m && !/^\s*rgba?\(\s*\d+\s*,\s*\d+\s*,\s*\d+\s*,\s*0\s*\)/.test(bg)) {
      const lum = (0.2126 * +m[1] + 0.7152 * +m[2] + 0.0722 * +m[3]) / 255;
      return lum < 0.45;
    }
  }
  return null;
}
function applyTheme() {
  let dark = detectParentDark();
  if (dark === null) dark = !!(window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
}
(function watchParentTheme() {
  const doc = parentDoc();
  if (!doc) return;
  const re = debounce(applyTheme, 60);
  const obs = new MutationObserver(re);
  const opts = { attributes: true, attributeFilter: ['class', 'style', 'data-theme', 'data-color-mode'] };
  if (doc.documentElement) obs.observe(doc.documentElement, opts);
  if (doc.body) obs.observe(doc.body, opts);
  window.addEventListener('focus', re);
})();

// ---------- 时间范围 ----------
const PRESETS = [
  { id: 'today', label: '今天' },
  { id: '5h', label: '近 5 小时' },
  { id: '24h', label: '近 24 小时' },
  { id: '7d', label: '近 7 天' },
  { id: '30d', label: '近 30 天' },
  { id: 'month', label: '本月' },
  { id: 'all', label: '全部时间' },
];
const rangeState = { id: localStorage.getItem('console-range') || '7d', from: null, to: null };

function computeRange() {
  const now = new Date();
  const day = 864e5;
  switch (rangeState.id) {
    case 'today': { const d = new Date(now); d.setHours(0, 0, 0, 0); return { from: d, to: null }; }
    case '5h': return { from: new Date(now - 5 * 36e5), to: null };
    case '24h': return { from: new Date(now - day), to: null };
    case '7d': return { from: new Date(now - 7 * day), to: null };
    case '30d': return { from: new Date(now - 30 * day), to: null };
    case 'month': { const d = new Date(now.getFullYear(), now.getMonth(), 1); return { from: d, to: null }; }
    case 'custom': return { from: rangeState.from, to: rangeState.to };
    default: return { from: null, to: null };
  }
}
function rangeParams() {
  const { from, to } = computeRange();
  const p = {};
  if (from) p.from = from.toISOString();
  if (to) p.to = to.toISOString();
  return p;
}
function rangeLabel() {
  if (rangeState.id === 'custom') {
    const f = x => x ? x.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' }) : '…';
    return f(rangeState.from) + ' → ' + f(rangeState.to);
  }
  return (PRESETS.find(p => p.id === rangeState.id) || PRESETS[3]).label;
}
function toLocalInput(d) {
  if (!d) return '';
  return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate())
    + 'T' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
(function initRange() {
  if (!PRESETS.some(p => p.id === rangeState.id)) rangeState.id = '7d';
  $('range-presets').innerHTML = PRESETS.map(p =>
    '<button type="button" class="pop-item" data-id="' + p.id + '"><span>' + p.label + '</span></button>').join('');
  $('range-presets').addEventListener('click', e => {
    const b = e.target.closest('.pop-item');
    if (!b) return;
    rangeState.id = b.dataset.id;
    localStorage.setItem('console-range', rangeState.id);
    closeRangePop();
    renderRangeUI();
    reloadActive();
  });
  $('range-btn').addEventListener('click', () => {
    const pop = $('range-pop');
    pop.hidden = !pop.hidden;
    $('range-btn').setAttribute('aria-expanded', String(!pop.hidden));
    if (!pop.hidden) {
      const { from, to } = computeRange();
      $('range-from').value = toLocalInput(from);
      $('range-to').value = toLocalInput(to);
    }
  });
  document.addEventListener('click', e => {
    if (!$('range-pop').hidden && !e.target.closest('.range')) closeRangePop();
  });
  $('range-apply').addEventListener('click', () => {
    const f = $('range-from').value ? new Date($('range-from').value) : null;
    const t = $('range-to').value ? new Date($('range-to').value) : null;
    if (f && t && f > t) { toast('起始时间晚于结束时间', 'err'); return; }
    rangeState.id = 'custom';
    rangeState.from = f; rangeState.to = t;
    localStorage.setItem('console-range', 'custom');
    closeRangePop();
    renderRangeUI();
    reloadActive();
  });
  renderRangeUI();
})();
function closeRangePop() {
  $('range-pop').hidden = true;
  $('range-btn').setAttribute('aria-expanded', 'false');
}
function renderRangeUI() {
  $('range-label').textContent = rangeLabel();
  [...$('range-presets').children].forEach(el =>
    el.setAttribute('aria-current', el.dataset.id === rangeState.id ? 'true' : 'false'));
}

// ---------- 抽屉弹窗 ----------
const sheet = $('sheet');
let sheetOk = null;
function openSheet(o) {
  $('sheet-title').textContent = o.title;
  $('sheet-body').innerHTML = o.body || '';
  $('sheet-note').textContent = o.note || '';
  $('sheet-copy').hidden = !$('sheet-note').textContent;
  const ok = $('sheet-ok');
  const cancel = $('sheet-cancel');
  ok.textContent = o.okText || '确定';
  ok.className = 'btn' + (o.danger ? ' danger' : ' primary');
  ok.disabled = false;
  // 信息展示类弹窗（无 onOk）：主按钮本身就是「关闭」，再摆一个取消
  // 就是两个按钮做同一件事，只留主按钮。
  cancel.hidden = !o.onOk;
  sheetOk = o.onOk || null;
  sheet.classList.remove('closing');
  sheet.showModal();
  if (!o.noFocus) {
    const first = $('sheet-body').querySelector('input,select,textarea');
    if (first) first.focus();
  }
}
function animateCloseSheet() {
  if (!sheet.open || sheet.classList.contains('closing')) return;
  sheet.classList.add('closing');
  setTimeout(() => {
    if (!sheet.classList.contains('closing')) return; // 动画期间被重新打开
    sheet.classList.remove('closing');
    sheet.close();
  }, 150);
}
$('sheet-x').addEventListener('click', () => animateCloseSheet());
$('sheet-cancel').addEventListener('click', () => animateCloseSheet());
sheet.addEventListener('cancel', e => { e.preventDefault(); animateCloseSheet(); });
$('sheet-form').addEventListener('submit', e => { e.preventDefault(); $('sheet-ok').click(); });
$('sheet-copy').addEventListener('click', () => {
  const text = $('sheet-note').textContent;
  if (!text) return;
  copyText(text).then(() => toast('已复制', 'ok')).catch(e => toast(e.message, 'err'));
});
$('sheet-ok').addEventListener('click', async () => {
  if (!sheetOk) { animateCloseSheet(); return; }
  const btn = $('sheet-ok');
  btn.disabled = true;
  try {
    const stay = await sheetOk();
    if (stay === false) return; // onOk 已接管界面（如展示明文），保持打开
    animateCloseSheet();
  } catch (e) {
    toast(e.message, 'err');
    // 报错同时落到底部信息栏并亮出复制按钮，方便用户拷贝完整报错。
    $('sheet-note').textContent = e.message;
    $('sheet-copy').hidden = false;
    btn.disabled = false;
  }
});
function staySheet(okText) {
  const btn = $('sheet-ok');
  btn.textContent = okText || '完成';
  btn.disabled = false;
  $('sheet-cancel').hidden = true; // 结果态只剩一个关闭按钮
  sheetOk = null; // 下一次点击直接关闭
}
function fieldRow(label, inner, cls) {
  return '<label class="field ' + (cls || '') + '"><span class="field-label">' + label + '</span>' + inner + '</label>';
}
// infoTip 生成一个 ⓘ 徽标，鼠标悬浮/键盘聚焦显示说明。
//
// 面板里有若干术语对中文读者并不自明（「缓存写」「缓存读」「额度口径」等），
// 它们背后是上游计费口径的差异，靠标题文字讲不清也不该占版面。用原生 title
// 承载完整说明：零依赖、可访问性由浏览器保证、移动端长按亦可见。
function infoTip(text) {
  return '<span class="info" tabindex="0" role="img" aria-label="说明：' + esc(text) + '"'
    + ' title="' + esc(text) + '">i</span>';
}
// labelWithTip 给字段标签追加 ⓘ 说明。
function labelWithTip(label, tip) {
  return esc(label) + infoTip(tip);
}
// TIPS 集中收拢术语解释，避免同一说明在多处漂移。
const TIPS = {
  cacheRead: '缓存读：命中上游提示词缓存的输入 token，单价通常远低于普通输入。'
    + '两种上游口径已归一——Claude 的 cache_read_tokens 独立于输入，'
    + 'OpenAI/Gemini 的 cached_tokens 含在输入内，取较大者避免重复计费。',
  cacheWrite: '缓存写：为建立提示词缓存而写入的 token，只在首次或缓存失效时产生，'
    + '单价通常高于普通输入（Claude 约为 1.25 倍）。后续命中即按「缓存读」计费。',
  input: '输入：本次请求发送给模型的提示词 token（已扣除命中缓存的部分）。',
  output: '输出：模型生成的 token。推理（thinking）token 也并入此项按输出价计费，'
    + '因此无需单独设置推理价。',
  tokenLimit: 'Token 限额与金额限额并列生效，任一触顶即拒绝请求。'
    + '统计口径为计费四类合计（输入＋输出＋缓存读＋缓存写），与费用同一口径。'
    + '混合模型时价差可达数十倍，用 token 约束用量比金额更精确。留空为不限。',
  moneyLimit: '按实际结算金额扣减，跨周期自动归零。留空为不限。',
  callerScope: '归属 caller 共享：额度与同一 caller 下的其他 Key 合并计算。'
    + '独立计额：本 Key 单独一份额度，不受同伴影响。',
  accountingMode: '缓存口径：inclusive 表示上游把缓存命中计入了输入总数（OpenAI/Gemini），'
    + 'exclusive 表示缓存命中独立于输入（Claude）。default 按上游字段自动判断。',
  billingMode: 'token 按 token 计价；per_image 按张计价（图像模型）；free 恒为免费。',
  priority: '同一模型命中多条规则时，优先级数值大的先生效；相同优先级按 id 升序。',
  matchKind: 'exact 完全匹配模型名；glob 支持 * 与 ? 通配；regexp 为正则匹配。',
};
function fact(name, value) {
  return '<dl class="fact"><dt>' + esc(name) + '</dt><dd>' + esc(value) + '</dd></dl>';
}
function secretBlock(plain) {
  return '<div class="secret"><code id="secret-code">' + esc(plain) + '</code>'
    + '<div class="btn-row"><button type="button" class="btn small" id="secret-copy">复制</button>'
    + '<span class="secret-warn">明文仅此一次展示，关闭后无法找回。</span></div></div>';
}
function wireSecretCopy() {
  const b = $('secret-copy');
  if (b) b.addEventListener('click', async () => {
    try { await copyText($('secret-code').textContent); b.textContent = '已复制'; setTimeout(() => b.textContent = '复制', 1500); }
    catch (e) { toast('复制失败，请手动选择文本', 'err'); }
  });
}
function confirmSheet(title, note, action) {
  openSheet({
    title, danger: true, okText: '确认执行',
    body: '<p>' + esc(note) + '</p>',
    onOk: async () => { await action(); toast('已完成', 'ok'); },
  });
}

// ---------- 页签调度 ----------
const loaders = {};
let activeTab = 'overview';
function switchTab(name) {
  activeTab = name;
  // roving tabindex：选中项才进 Tab 序，其余用方向键在 tablist 内移动（ARIA tabs 模式）
  document.querySelectorAll('.tab').forEach(t => {
    const on = t.dataset.tab === name;
    t.setAttribute('aria-selected', String(on));
    t.tabIndex = on ? 0 : -1;
  });
  document.querySelectorAll('.view').forEach(v => { v.hidden = v.id !== 'view-' + name; });
  reloadActive();
}
function reloadActive() {
  const fn = loaders[activeTab];
  if (fn) fn().catch(e => toast(e.message, 'err'));
}
document.querySelectorAll('.tab').forEach(t => t.addEventListener('click', () => switchTab(t.dataset.tab)));
$('tabs').addEventListener('keydown', e => {
  const tabs = [...document.querySelectorAll('.tab')];
  const i = tabs.findIndex(t => t.dataset.tab === activeTab);
  if (i < 0) return;
  let to = -1;
  if (e.key === 'ArrowRight') to = (i + 1) % tabs.length;
  else if (e.key === 'ArrowLeft') to = (i - 1 + tabs.length) % tabs.length;
  else if (e.key === 'Home') to = 0;
  else if (e.key === 'End') to = tabs.length - 1;
  if (to < 0) return;
  e.preventDefault();
  switchTab(tabs[to].dataset.tab);
  tabs[to].focus();
});
// Escape 统一关闭浮层：先关下拉，再关时间范围弹层（原先范围弹层无法用键盘关闭）
document.addEventListener('keydown', e => {
  if (e.key !== 'Escape') return;
  if (openSel) { closeAnySel(true); return; }
  if (!$('range-pop').hidden) {
    closeRangePop();
    $('range-btn').focus();
  }
});
$('refresh-btn').addEventListener('click', reloadActive);
$('logout-btn').addEventListener('click', logout);
function stamp() { $('stamp').textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false }); }

// ---------- 概览 ----------
const GRAINS = [
  { value: 'minute', label: '按分钟' },
  { value: 'hour', label: '按小时' },
  { value: 'day', label: '按天' },
  { value: 'week', label: '按周' },
  { value: 'month', label: '按月' },
];
const trend = { points: [], off: new Set(), grainManual: false, view: 'chart' };

// Token 口径：上游 total 缺失（0）时按计费四类合计兜底；
// 缓存命中取「Claude 口径读写」与「OpenAI/Gemini 口径 cached」的较大者，避免双计。
function effTokens(r) {
  const t = Number(r.total_tokens) || 0;
  if (t > 0) return t;
  return (Number(r.input_tokens) || 0) + (Number(r.output_tokens) || 0)
    + (Number(r.cache_read_tokens) || 0) + (Number(r.cache_creation_tokens) || 0);
}
function cacheHit(r) {
  return Math.max((Number(r.cache_read_tokens) || 0) + (Number(r.cache_creation_tokens) || 0),
    Number(r.cached_tokens) || 0);
}
// cacheReadOf 统一两种上游口径的「缓存读」：Claude 的 cache_read 独立于输入，
// OpenAI/Gemini 的 cached 含在输入内。取较大者，避免双计，与缓存命中率同口径。
function cacheReadOf(r) {
  return Math.max(Number(r.cache_read_tokens) || 0, Number(r.cached_tokens) || 0);
}

loaders.overview = async () => {
  if (!trend.grainManual) trendGrainSel.value = autoGrain();
  const p = rangeParams();
  // 汇率与四路数据并发拉取（原先串行在 Promise.all 之后，多付一个 RTT）。
  const [dimModel, dimKey, points, costs, fx] = await Promise.all([
    api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '50', ...p })),
    api('/usage/dimension?' + new URLSearchParams({ dimension: 'key_id', limit: '50', ...p })),
    api('/trends?' + new URLSearchParams({ grain: trendGrainSel.value, ...p })),
    api('/costs?' + new URLSearchParams(p)),
    S.fx ? null : api('/exchange-rate').catch(() => null),
  ]);
  if (!S.fx && fx) S.fx = fx;
  trend.points = Array.isArray(points) ? points : [];
  ovCache.models = dimModel.rows || [];
  ovCache.modelCount = dimModel.count;
  ovCache.keys = dimKey.rows || [];
  ovCache.keyCount = dimKey.count;
  renderReadouts(dimModel.total || {}, costs);
  renderModels(ovCache.models);
  renderKeySpend(ovCache.keys);
  renderCostCoverage(costs);
  renderTrend();
  stamp();
};

function readout(label, value, sub, alarm) {
  return '<div class="readout' + (alarm ? ' alarm' : '') + '"><div class="readout-top">'
    + '<span class="readout-label">' + label + '</span></div>'
    + '<div class="readout-value">' + value + '</div>'
    + (sub ? '<div class="readout-sub">' + sub + '</div>' : '') + '</div>';
}
// cacheHitRate 缓存命中率 = 命中 token / (输入 + 缓存读 + 缓存写)。
// OpenAI 口径的 cached_tokens 已含在输入内，Claude 口径的 cache_read 独立，分母对两者均成立。
function cacheHitRate(t) {
  const denom = (+t.input_tokens || 0) + (+t.cache_read_tokens || 0) + (+t.cache_creation_tokens || 0);
  return denom > 0 ? cacheHit(t) / denom * 100 : -1;
}
function renderReadouts(total, costs) {
  const failRate = total.requests ? (total.failures / total.requests * 100).toFixed(1) + '%' : '0%';
  const cover = costs.requests ? Math.round(costs.priced_requests / costs.requests * 100) : 0;
  const rate = S.fx;
  const cny = rate && total.cost_micro_usd
    ? ' ≈ ¥' + fmtUSD(Math.round(total.cost_micro_usd * rate.usd_to_cny_micro / 1e6)).slice(1) : '';
  const hitPct = cacheHitRate(total);
  $('ov-readouts').innerHTML =
    readout('请求总数', fmtInt(total.requests),
      '失败 <b>' + fmtInt(total.failures) + '</b> · 失败率 ' + failRate,
      total.requests > 0 && total.failures / total.requests > 0.05)
    + readout('总消耗 Token', effTokens(total).toLocaleString('zh-CN'),
      '输入 <b>' + fmtTok(total.input_tokens) + '</b> · 输出 <b>' + fmtTok(total.output_tokens)
      + '</b> · 缓存命中 <b>' + fmtTok(cacheHit(total)) + '</b>')
    + readout('总费用', fmtUSD(total.cost_micro_usd),
      '计价覆盖 <b>' + cover + '%</b>' + esc(cny))
    + readout('缓存命中率', hitPct < 0 ? '—' : hitPct.toFixed(1) + '%',
      '读 <b>' + fmtTok(cacheReadOf(total)) + '</b> · 写 <b>' + fmtTok(total.cache_creation_tokens)
      + '</b>' + ((total.cached_tokens || 0) > (total.cache_read_tokens || 0) + (total.cache_creation_tokens || 0)
        ? ' · 含上游缓存口径' : ''));
}

// ---------- 概览占比卡：指标切换（费用 / Token / 请求） ----------
const ovCache = { models: [], modelCount: null, keys: [], keyCount: null };
const ovMetric = {
  models: localStorage.getItem('ov-models-metric') || 'tokens',
  keys: localStorage.getItem('ov-keys-metric') || 'cost',
};
function metricVal(r, m) {
  if (m === 'cost') return Number(r.cost_micro_usd) || 0;
  if (m === 'requests') return Number(r.requests) || 0;
  return effTokens(r);
}
function metricText(v, m) {
  return m === 'cost' ? fmtUSD(v) : m === 'tokens' ? fmtTok(v) : fmtInt(v);
}
const METRIC_SUBS = { tokens: '按 Token 计量', cost: '按费用计量', requests: '按请求次数计量' };
function bindMetricSeg(id, key, apply) {
  const seg = $(id);
  seg.querySelectorAll('button').forEach(b =>
    b.classList.toggle('on', b.dataset.m === ovMetric[key]));
  seg.addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (!b || b.dataset.m === ovMetric[key]) return;
    ovMetric[key] = b.dataset.m;
    localStorage.setItem(id, b.dataset.m);
    seg.querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
    apply();
  });
}
bindMetricSeg('ov-models-metric', 'models', () => renderModels(ovCache.models));
bindMetricSeg('ov-keys-metric', 'keys', () => renderKeySpend(ovCache.keys));

// ---------- 概览圆环图 ----------
// DONUT_COLORS 是环分段配色（Tableau 定性色板，深浅主题均可辨）；第 6 段起并入「其他」。
const DONUT_COLORS = ['#4e79a7', '#f28e2b', '#59a14f', '#e15759', '#b07aa1'];
const DONUT_OTHER = '#9aa5b1';
function donutEntries(rows, metric) {
  const sorted = rows.filter(r => metricVal(r, metric) > 0)
    .sort((a, b) => metricVal(b, metric) - metricVal(a, metric));
  const top = sorted.slice(0, 5).map((r, i) => ({
    label: r.value || '(空)', color: DONUT_COLORS[i],
    value: metricVal(r, metric), cost: r.cost_micro_usd, requests: r.requests,
  }));
  const rest = sorted.slice(5);
  if (rest.length) {
    top.push({
      label: '其他（' + rest.length + ' 项）', color: DONUT_OTHER,
      value: rest.reduce((a, r) => a + metricVal(r, metric), 0),
      cost: rest.reduce((a, r) => a + (Number(r.cost_micro_usd) || 0), 0),
      requests: rest.reduce((a, r) => a + (Number(r.requests) || 0), 0),
    });
  }
  return { items: top, total: sorted.reduce((a, r) => a + metricVal(r, metric), 0), count: sorted.length };
}
// drawDonut 渲染单圆环（默认前 5 + 其他合并段）。
//
// 入场动画是**顺时针单前沿扫描**：每段的过渡时长与其弧长占比成正比、延迟为
// 前序段时长之和，衔接处速度一致 —— 视觉上只有一个前沿从顶部顺时针推进，
// 走到哪里哪段显色，而不是各段各自冒出来。
const DONUT_ANIM_MS = 550;
function drawDonut(mountId, entries, fmt) {
  const mount = $(mountId);
  const total = entries.total;
  if (!total || !entries.items.length) {
    mount.innerHTML = '<div class="empty"><p class="empty-title">暂无数据</p>'
      + '<p class="empty-hint">所选时间范围内没有可统计的记录</p></div>';
    return;
  }
  const R = 42, C = 2 * Math.PI * R;
  let offset = 0;
  const segs = entries.items.map((it, i) => {
    const frac = it.value / total;
    const seg = '<circle class="donut-seg" data-i="' + i + '" cx="60" cy="60" r="' + R + '" fill="none"'
      + ' stroke="' + it.color + '" stroke-width="17" stroke-linecap="butt"'
      + ' style="stroke-dasharray:0 ' + (C + 10).toFixed(2) + ';stroke-dashoffset:' + (-offset).toFixed(2) + '"'
      + ' tabindex="0"><title>' + esc(it.label + ' · ' + metricText(it.value, fmt.metric)
        + ' · ' + (frac * 100).toFixed(1) + '%') + '</title></circle>';
    offset += frac * C;
    return seg;
  }).join('');
  const legend = entries.items.map((it, i) => {
    const pct = it.value / total * 100;
    return '<button type="button" class="donut-legend-item" data-i="' + i + '">'
      + '<span class="swatch" style="background:' + it.color + '"></span>'
      + '<span class="dl-name" title="' + esc(it.label) + '">' + esc(it.label) + '</span>'
      + '<span class="dl-pct mono">' + (pct < 10 ? pct.toFixed(1) : pct.toFixed(0)) + '%</span></button>';
  }).join('');
  mount.innerHTML = '<div class="donut-flex">'
    + '<div class="donut-ring"><svg viewBox="0 0 120 120" role="img">' + segs + '</svg>'
    + '<div class="donut-center"><div class="donut-center-main"></div><div class="donut-center-sub"></div></div></div>'
    + '<div class="donut-legend">' + legend + '</div></div>';

  const centerMain = mount.querySelector('.donut-center-main');
  const centerSub = mount.querySelector('.donut-center-sub');
  const showTotal = () => {
    centerMain.textContent = metricText(total, fmt.metric);
    centerSub.textContent = fmt.center;
  };
  showTotal();
  const circles = [...mount.querySelectorAll('.donut-seg')];
  // 入场：rAF 两帧后（首帧已绘制空弧）按弧长比例分配时长、以前序累计作延迟，
  // linear 缓动保证段间前沿速度一致；结束后移除内联过渡，交还 hover 效果。
  requestAnimationFrame(() => requestAnimationFrame(() => {
    let acc = 0;
    entries.items.forEach((it, i) => {
      const frac = it.value / total;
      const len = Math.max(frac * C - 1.5, frac > 0 ? 0.6 : 0); // 1.5 单位留缝
      const c = circles[i];
      c.style.transition = 'stroke-dasharray ' + Math.max(frac * DONUT_ANIM_MS, 30).toFixed(0)
        + 'ms linear ' + (acc * DONUT_ANIM_MS).toFixed(0) + 'ms';
      c.style.strokeDasharray = len.toFixed(2) + ' ' + (C - len + 10).toFixed(2);
      acc += frac;
    });
    setTimeout(() => circles.forEach(c => { c.style.transition = ''; }), DONUT_ANIM_MS + 120);
  }));
  const highlight = i => {
    circles.forEach((c, j) => c.classList.toggle('dim', i >= 0 && j !== i));
    if (i >= 0) {
      const it = entries.items[i];
      centerMain.textContent = metricText(it.value, fmt.metric);
      centerSub.textContent = it.label;
    } else showTotal();
  };
  const bindHl = (el, i) => {
    el.addEventListener('mouseenter', () => highlight(i));
    el.addEventListener('mouseleave', () => highlight(-1));
    el.addEventListener('focus', () => highlight(i));
    el.addEventListener('blur', () => highlight(-1));
  };
  circles.forEach((c, i) => bindHl(c, i));
  mount.querySelectorAll('.donut-legend-item').forEach(el => bindHl(el, Number(el.dataset.i)));
}
function renderModels(rows) {
  const m = ovMetric.models;
  const e = donutEntries(rows, m);
  const n = Number.isInteger(ovCache.modelCount) ? ovCache.modelCount : rows.length;
  $('ov-models-sub').textContent = METRIC_SUBS[m] + ' · 前 5 + 其他，共 ' + n + ' 项';
  drawDonut('ov-models', e, { metric: m, center: n + ' 个模型' });
}
function renderKeySpend(rows) {
  const m = ovMetric.keys;
  const withKey = rows.filter(r => r.value).map(r => Object.assign({}, r, {
    value: keyLabelOf(r.value) || '(无标签)',
  }));
  const e = donutEntries(withKey, m);
  const n = Number.isInteger(ovCache.keyCount) ? ovCache.keyCount : rows.length;
  $('ov-keys-sub').textContent = METRIC_SUBS[m] + ' · 前 5 + 其他，共 ' + n + ' 枚';
  drawDonut('ov-keys', e, { metric: m, center: n + ' 枚密钥' });
}

// ---------- 趋势图（内联 SVG 堆叠面积）----------
function niceMax(v) {
  if (v <= 0) return 1;
  const exp = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 2, 2.5, 5, 10]) if (v <= m * exp) return m * exp;
  return 10 * exp;
}
function bucketLabel(ts, grain) {
  const d = new Date(ts);
  if (grain === 'month') return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  if (grain === 'day' || grain === 'week')
    return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()) + ' ' + pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function bucketTick(ts, grain) {
  const d = new Date(ts);
  if (grain === 'month') return d.getFullYear() + '-' + pad2(d.getMonth() + 1);
  if (grain === 'day' || grain === 'week') return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}
function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

// 按时间跨度自动选粒度（用户手动选过则不再覆盖）。
function autoGrain() {
  const { from } = computeRange();
  const span = from ? Date.now() - from.getTime() : Infinity;
  if (span < 3 * 36e5) return 'minute';
  if (span < 96 * 36e5) return 'hour';
  if (span < 120 * 864e5) return 'day';
  return 'month';
}
// 与服务端口径一致的桶对齐：分/时/日按 UTC 整除，周为 UTC 周一，月为 UTC 月初。
function bucketStepMs(grain) {
  return { minute: 6e4, hour: 36e5, day: 864e5, week: 6048e5, month: 0 }[grain] || 0;
}
function alignBucket(ms, grain) {
  if (grain === 'week') {
    const d = new Date(ms);
    const u = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
    const dow = (new Date(u).getUTCDay() + 6) % 7;
    return u - dow * 864e5;
  }
  if (grain === 'month') {
    const d = new Date(ms);
    return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
  }
  const step = bucketStepMs(grain);
  return Math.floor(ms / step) * step;
}
// 在所选时间范围内补零值桶，让面积图连续；桶数超上限时退回原始点。
function fillTrendPoints(raw, grain) {
  const pts = raw.slice().sort((a, b) => new Date(a.bucket) - new Date(b.bucket));
  if (!pts.length) return pts;
  const step = bucketStepMs(grain);
  const { from, to } = computeRange();
  let start = from ? alignBucket(from.getTime(), grain) : alignBucket(new Date(pts[0].bucket).getTime(), grain);
  let end = to ? Math.min(to.getTime(), Date.now()) : Date.now();
  end = alignBucket(end, grain);
  if (!step && grain === 'month') {
    // 按自然月推进
    const sd = new Date(start), ed = new Date(end);
    const months = (ed.getUTCFullYear() - sd.getUTCFullYear()) * 12 + ed.getUTCMonth() - sd.getUTCMonth();
    if (months > 2000) return pts;
  } else if (step && (end - start) / step > 2000) {
    return pts;
  }
  const byKey = new Map(pts.map(p => [alignBucket(new Date(p.bucket).getTime(), grain), p]));
  const out = [];
  const zero = () => ({
    requests: 0, failures: 0, input_tokens: 0, output_tokens: 0, cached_tokens: 0,
    cache_read_tokens: 0, cache_creation_tokens: 0, total_tokens: 0, cost_micro_usd: 0,
  });
  if (grain === 'month') {
    const cur = new Date(start);
    while (cur.getTime() <= end) {
      const k = cur.getTime();
      out.push(Object.assign({ bucket: new Date(k).toISOString() }, byKey.get(k) || zero()));
      cur.setUTCMonth(cur.getUTCMonth() + 1);
    }
  } else {
    for (let t = start; t <= end; t += step)
      out.push(Object.assign({ bucket: new Date(t).toISOString() }, byKey.get(t) || zero()));
  }
  return out.length ? out : pts;
}

// downsampleTrend 把过密的桶按相邻 k 个合并，避免柱状图 DOM 爆炸（仅影响显示）。
function downsampleTrend(pts, maxN) {
  if (pts.length <= maxN) return pts;
  const k = Math.ceil(pts.length / maxN);
  const keys = ['requests', 'failures', 'input_tokens', 'output_tokens', 'cached_tokens',
    'cache_read_tokens', 'cache_creation_tokens', 'total_tokens', 'cost_micro_usd'];
  const out = [];
  for (let i = 0; i < pts.length; i += k) {
    const g = { bucket: pts[i].bucket };
    for (const key of keys) g[key] = 0;
    for (let j = i; j < Math.min(i + k, pts.length); j++)
      for (const key of keys) g[key] += +pts[j][key] || 0;
    out.push(g);
  }
  return out;
}

function trendSeries() {
  const metric = trendMetricSel.value;
  // 「成功/失败」是状态语义（good/bad），保留状态色；其余是身份语义，用图表专用系列色。
  // 次级编码：失败恒在栈顶 + 段间 2px 间隙 + 图例常在，不依赖单一色相区分。
  if (metric === 'requests') return [
    { key: 'ok', label: '成功', color: cssVar('--live'), val: p => Math.max(0, p.requests - p.failures) },
    { key: 'fail', label: '失败', color: cssVar('--alarm'), val: p => p.failures },
  ];
  if (metric === 'cost') return [
    { key: 'cost', label: '费用', color: cssVar('--series-1'), val: p => p.cost_micro_usd, money: true },
  ];
  return [
    // 堆叠必须互不重叠：OpenAI/Gemini 的 cached_tokens 已含在 input_tokens 内，
    // 先从输入中拆出再单列「缓存读」，否则缓存命中被计两遍，
    // 堆叠总量会虚高且与概览「总消耗 Token」（EffectiveTotal 口径）对不上。
    // Claude 口径 cached_tokens 恒为 0，减法无影响。
    { key: 'input', label: '输入', color: cssVar('--series-1'), tok: true,
      val: p => Math.max(0, (+p.input_tokens || 0) - Math.min(+p.cached_tokens || 0, +p.input_tokens || 0)) },
    { key: 'output', label: '输出', color: cssVar('--series-2'), tok: true, val: p => p.output_tokens },
    { key: 'cache-read', label: '缓存读', color: cssVar('--series-3'), tok: true, val: p => cacheReadOf(p) },
    { key: 'cache-creation', label: '缓存写', color: cssVar('--series-4'), tok: true, val: p => p.cache_creation_tokens || 0 },
  ];
}
function renderLegend() {
  // 单系列不需要图例（标题已说明画的是什么），≥2 系列图例常在。
  const defs = trendSeries();
  $('trend-legend').innerHTML = defs.length < 2 ? '' : defs.map(d =>
    '<button type="button" class="legend-item" data-key="' + d.key + '" aria-pressed="'
    + String(!trend.off.has(d.key)) + '"><span class="swatch" style="background:' + d.color + '"></span>'
    + d.label + '</button>').join('');
}
$('trend-legend').addEventListener('click', e => {
  const b = e.target.closest('.legend-item');
  if (!b) return;
  const k = b.dataset.key;
  if (trend.off.has(k)) trend.off.delete(k); else trend.off.add(k);
  if (trend.off.size === trendSeries().length) trend.off.delete(k);
  renderTrend();
});

function renderTrend() {
  const grain = trendGrainSel.value;
  const filled = fillTrendPoints(trend.points, grain);
  const pts = downsampleTrend(filled, 1200);
  const box = $('trend-chart');
  renderLegend();
  const grainText = (GRAINS.find(g => g.value === grain) || {}).label || '';
  $('trend-sub').textContent = '按' + grainText.replace('按', '')
    + '堆叠 · ' + rangeLabel() + ' · ' + pts.length + ' 个桶'
    + (pts.length < filled.length ? '（过密已合并显示）' : '');
  const defs = trendSeries().map(d => Object.assign(d, { on: !trend.off.has(d.key) }));
  trend.view === 'table' ? renderTrendTable(pts, defs, grain) : $('trend-table').hidden = true;
  if (trend.view === 'table') { box.hidden = true; return; }
  box.hidden = false;
  if (!pts.length) {
    box.innerHTML = '<div class="empty" style="height:100%"><p class="empty-title">暂无趋势数据</p>'
      + '<p class="empty-hint">所选时间范围与粒度下没有聚合记录</p></div>';
    return;
  }
  // 用像素级 width/height 而非 preserveAspectRatio=none，避免坐标轴文字被拉伸。
  const W = Math.max(320, Math.floor(box.clientWidth || 800));
  const H = Math.max(240, Math.floor(box.clientHeight || 300));
  const padL = 56, padR = 14, padT = 14, padB = 26;
  const iw = W - padL - padR, ih = H - padT - padB;
  const stacks = pts.map(p => defs.reduce((acc, d) => d.on ? acc + Math.max(0, d.val(p)) : acc, 0));
  const ymax = niceMax(Math.max(...stacks, 1));
  const y = v => padT + ih - (v / ymax) * ih;
  const n = pts.length;
  const slot = iw / n;                 // 每个桶的槽宽
  // 细粒度（分钟/小时）桶多，柱宽上限 24px 即可；天/周/月桶少槽宽大，
  // 改为「槽宽减去固定间隙」，避免柱子孤零零缩在槽中央、两侧大片留白。
  const coarse = grain === 'day' || grain === 'week' || grain === 'month';
  const barW = coarse
    ? Math.max(2, Math.min(96, slot - Math.min(12, slot * 0.18)))
    : Math.max(2, Math.min(24, slot * 0.7));
  const cx = i => padL + slot * i + slot / 2;

  let grid = '', labels = '';
  const isMoney = defs.some(d => d.money);
  const isTokens = defs.some(d => d.tok);
  const fmtAxis = v => isMoney ? fmtUSD(v) : isTokens ? fmtTok(v) : fmtInt(v);
  for (let g = 0; g <= 4; g++) {
    const gv = ymax * g / 4, gy = y(gv);
    grid += '<line class="gridline" x1="' + padL + '" y1="' + gy.toFixed(1) + '" x2="' + (W - padR) + '" y2="' + gy.toFixed(1) + '"/>';
    labels += '<text class="axis-text" x="' + (padL - 8) + '" y="' + (gy + 3.5).toFixed(1) + '" text-anchor="end">'
      + fmtAxis(gv) + '</text>';
  }
  const tickStep = Math.max(1, Math.ceil(n / Math.floor(iw / 64)));
  let lastTickX = -1e9;
  for (let i = 0; i < n; i++) {
    const tx = cx(i);
    const isLast = i === n - 1;
    if ((i % tickStep !== 0 && !isLast) || tx - lastTickX < 40) continue;
    lastTickX = tx;
    labels += '<text class="axis-text" x="' + tx.toFixed(1) + '" y="' + (H - 8) + '" text-anchor="middle">'
      + esc(bucketTick(pts[i].bucket, grain)) + '</text>';
  }

  // 堆叠柱：自下而上逐系列叠加。
  // 段间留 2px 表面色间隙（用间隙分隔，不画描边）：间隙统一开在每段的**顶边**，
  // 这样最底段仍然坐在基线上（柱体从单一基线生长，基线端方角）。
  // 高度不足以让出间隙的薄段照原高绘制、不加间隙 —— 宁可少一条分隔，
  // 也不能把数据段整段丢掉或抬高（精确值另有 tooltip 与表格视图承载）。
  const GAP = 2, R = 4;
  const on = defs.filter(d => d.on);
  let bars = '', running = new Array(n).fill(0);
  // 先定位每根柱子最上面的可见段，供圆角判定
  const topSeg = new Array(n).fill(-1);
  for (let i = 0; i < n; i++)
    for (let k = 0; k < on.length; k++)
      if (Math.max(0, on[k].val(pts[i])) > 0) topSeg[i] = k;
  for (let k = 0; k < on.length; k++) {
    const d = on[k];
    for (let i = 0; i < n; i++) {
      const v = Math.max(0, d.val(pts[i]));
      if (v <= 0) continue;
      const yBase = y(running[i]);
      const yTop = y(running[i] + v);
      running[i] += v;
      const rawH = yBase - yTop;
      if (rawH <= 0) continue;
      const isTop = k === topSeg[i];
      const gap = (!isTop && rawH > GAP + 1) ? GAP : 0;
      // 非零段至少画 1px：亚像素高度等于画了个看不见的东西，
      // 「有但极小」比「看不到」更诚实（精确值在 tooltip 与表格视图里）。
      const h = Math.max(1, rawH - gap);
      const yDraw = yTop + gap;
      const x = cx(i) - barW / 2;
      bars += isTop
        ? '<path d="' + topRoundedBar(x, yDraw, barW, h, R) + '" fill="' + d.color + '"/>'
        : '<rect x="' + x.toFixed(1) + '" y="' + yDraw.toFixed(1) + '" width="' + barW.toFixed(1)
          + '" height="' + h.toFixed(1) + '" fill="' + d.color + '"/>';
    }
  }
  // 悬停/聚焦热区（置于柱体之下、网格之上）。tabindex 让键盘也能读到数值。
  let hover = '';
  for (let i = 0; i < n; i++)
    hover += '<rect class="bar-hover" tabindex="0" role="button" data-i="' + i + '"'
      + ' aria-label="' + esc(bucketLabel(pts[i].bucket, grain) + '，合计 ' + fmtAxis(stacks[i])) + '"'
      + ' x="' + (padL + slot * i).toFixed(1) + '" y="' + padT + '" width="' + slot.toFixed(1)
      + '" height="' + ih + '"/>';

  box.innerHTML = '<svg width="' + W + '" height="' + H + '" viewBox="0 0 ' + W + ' ' + H
    + '" role="img" aria-label="用量趋势图">' + grid + hover + bars + labels + '</svg>'
    + '<div class="chart-tip" id="trend-tip" hidden></div>';

  const svg = box.querySelector('svg'), tip = $('trend-tip');
  let hotIdx = -1;
  function setHot(i) {
    if (i === hotIdx) return;
    hotIdx = i;
    svg.querySelectorAll('.bar-hover').forEach(r =>
      r.classList.toggle('on', +r.dataset.i === i));
  }
  // showTip 鼠标与键盘共用：tooltip 上挂在柱顶，空间不足时下翻。
  function showTip(idx) {
    setHot(idx);
    const p = pts[idx];
    let rows = defs.filter(d => d.on).map(d =>
      '<div class="tip-row"><span class="swatch" style="background:' + d.color + '"></span><span>' + d.label
      + '</span><b>' + (d.money ? fmtUSD(d.val(p)) : d.tok ? fmtTok(d.val(p)) : fmtInt(d.val(p))) + '</b></div>').join('');
    rows += '<div class="tip-row tip-total"><span></span><span>合计</span><b>' + fmtAxis(stacks[idx]) + '</b></div>';
    tip.innerHTML = '<div class="tip-head">' + esc(bucketLabel(p.bucket, grain)) + '</div>' + rows;
    tip.hidden = false;
    const rect = svg.getBoundingClientRect();
    const sx = cx(idx) / W * rect.width;
    const topPx = y(stacks[idx]) / H * rect.height;
    const wantAbove = topPx - tip.offsetHeight - 8 >= 0;
    tip.classList.toggle('below', !wantAbove);
    tip.style.left = Math.max(90, Math.min(rect.width - 90, sx)) + 'px';
    tip.style.top = (wantAbove ? topPx - 8 : topPx + 8) + 'px';
  }
  function hideTip() { tip.hidden = true; setHot(-1); }
  svg.addEventListener('mousemove', ev => {
    const rect = svg.getBoundingClientRect();
    const sx = (ev.clientX - rect.left) * (W / rect.width);
    let idx = Math.floor((sx - padL) / slot);
    showTip(Math.max(0, Math.min(n - 1, idx)));
  });
  svg.addEventListener('mouseleave', hideTip);
  // 键盘：Tab 进入热区即出 tooltip，左右键在桶之间移动
  svg.addEventListener('focusin', e => {
    const r = e.target.closest('.bar-hover');
    if (r) showTip(+r.dataset.i);
  });
  svg.addEventListener('focusout', e => {
    if (!svg.contains(e.relatedTarget)) hideTip();
  });
  svg.addEventListener('keydown', e => {
    const r = e.target.closest('.bar-hover');
    if (!r) return;
    const i = +r.dataset.i;
    const to = e.key === 'ArrowRight' ? i + 1 : e.key === 'ArrowLeft' ? i - 1 : -1;
    if (to < 0 || to >= n) return;
    e.preventDefault();
    svg.querySelector('.bar-hover[data-i="' + to + '"]').focus();
  });
}
// topRoundedBar 顶端圆角、基线方角的柱体路径。
function topRoundedBar(x, y, w, h, r) {
  const R = Math.max(0, Math.min(r, w / 2, h));
  return 'M' + x.toFixed(1) + ' ' + (y + h).toFixed(1)
    + 'V' + (y + R).toFixed(1)
    + 'a' + R.toFixed(1) + ' ' + R.toFixed(1) + ' 0 0 1 ' + R.toFixed(1) + ' -' + R.toFixed(1)
    + 'h' + (w - 2 * R).toFixed(1)
    + 'a' + R.toFixed(1) + ' ' + R.toFixed(1) + ' 0 0 1 ' + R.toFixed(1) + ' ' + R.toFixed(1)
    + 'V' + (y + h).toFixed(1) + 'Z';
}
// renderTrendTable 图表的表格孪生视图：数值不再只能靠悬停读取。
function renderTrendTable(pts, defs, grain) {
  const host = $('trend-table');
  host.hidden = false;
  const on = defs.filter(d => d.on);
  const fmtOf = d => v => d.money ? fmtUSD(v) : d.tok ? fmtTok(v) : fmtInt(v);
  if (!pts.length) {
    host.innerHTML = '<div class="empty"><p class="empty-title">暂无趋势数据</p>'
      + '<p class="empty-hint">所选时间范围与粒度下没有聚合记录</p></div>';
    return;
  }
  const rows = pts.slice().reverse();
  host.innerHTML = '<table class="data"><thead><tr><th>时间桶</th>'
    + on.map(d => '<th class="num">' + esc(d.label) + '</th>').join('')
    + '<th class="num">合计</th></tr></thead><tbody>'
    + rows.map(p => {
      const total = on.reduce((a, d) => a + Math.max(0, d.val(p)), 0);
      const f = on.length ? fmtOf(on[0]) : fmtInt;
      return '<tr><td class="cell-mono">' + esc(bucketLabel(p.bucket, grain)) + '</td>'
        + on.map(d => '<td class="num">' + fmtOf(d)(Math.max(0, d.val(p))) + '</td>').join('')
        + '<td class="num"><b>' + f(total) + '</b></td></tr>';
    }).join('')
    + '</tbody></table>';
}
const trendMetricSel = new Select('trend-metric', [
  { value: 'tokens', label: 'Token' },
  { value: 'requests', label: '请求' },
  { value: 'cost', label: '费用' },
], () => { trend.off.clear(); renderTrend(); }, { value: 'tokens', head: '趋势指标' });
const trendGrainSel = new Select('trend-grain', GRAINS,
  () => { trend.grainManual = true; reloadActive(); }, { value: 'day', head: '聚合粒度' });
// 图表 / 表格切换：表格孪生视图让数值不必依赖悬停
$('trend-view').addEventListener('click', e => {
  const b = e.target.closest('button[data-v]');
  if (!b || b.dataset.v === trend.view) return;
  trend.view = b.dataset.v;
  $('trend-view').querySelectorAll('button').forEach(x => x.classList.toggle('on', x === b));
  renderTrend();
});
window.addEventListener('resize', debounce(() => { if (activeTab === 'overview') renderTrend(); }, 200));

// ---------- 密钥 ----------
const keysView = {
  cache: [], total: 0, statusCounts: {}, page: 0, size: 20, search: '', caller: '', status: '',
  balanceSeq: 0,    // 余额异步响应的竞态守卫：快速切换抽屉对象时丢弃晚到的旧响应
};
// keyLabelOf 由 kid 查密钥标签；无标签或缓存未热时返回空串，由调用方决定回落值。
function keyLabelOf(kid) {
  const k = keysView.cache.find(x => x.kid === kid);
  return k && k.label ? k.label : '';
}

loaders.keys = async () => { await refreshKeys(); };
// 密钥列表走服务端分页：limit/offset/status 都下推到 SQL，避免大基数用户
// 一次拉上千条。status_counts 由后端附带，徽标与「共 N 枚」仍拿得到全量口径。
async function refreshKeys() {
  const q = new URLSearchParams({ limit: String(keysView.size), offset: String(keysView.page * keysView.size) });
  if (keysView.search) q.set('search', keysView.search);
  if (keysView.caller) q.set('caller_id', keysView.caller);
  if (keysView.status) q.set('status', keysView.status);
  const r = await api('/keys?' + q);
  keysView.cache = r.items || [];
  keysView.total = r.total || 0;
  keysView.statusCounts = r.status_counts || {};
  const pages = Math.max(1, Math.ceil(keysView.total / keysView.size));
  if (keysView.page >= pages && keysView.page > 0) {
    keysView.page = pages - 1;
    return refreshKeys();
  }
  renderKeys();
  updateBadges();
  // 详情 dialog 开着时同步刷新其内容；对象已被删除则关闭。
  const d = $('key-dialog');
  if (d.open && d.dataset.kid) {
    const nk = keysView.cache.find(x => x.kid === d.dataset.kid);
    if (nk) renderKeyDialog(nk); else animateCloseKeyDialog();
  }
  stamp();
}
function keyStatus(k) {
  if (k.revoked_at) return 'revoked';
  if (!k.enabled) return 'disabled';
  if (k.expires_at && new Date(k.expires_at) <= new Date()) return 'expired';
  return 'active';
}
const STATUS_META = {
  active: { label: '启用中', pill: 'live' },
  disabled: { label: '已禁用', pill: 'warn' },
  revoked: { label: '已撤销', pill: 'alarm' },
  expired: { label: '已过期', pill: '' },
};
function cycleKeysNow(d = new Date()) {
  const y = d.getUTCFullYear(), m = d.getUTCMonth(), day = d.getUTCDate();
  const u = new Date(Date.UTC(y, m, day));
  const dow = (u.getUTCDay() + 6) % 7;
  const thu = new Date(u); thu.setUTCDate(u.getUTCDate() - dow + 3);
  const jan4 = new Date(Date.UTC(thu.getUTCFullYear(), 0, 4));
  const jdow = (jan4.getUTCDay() + 6) % 7;
  jan4.setUTCDate(jan4.getUTCDate() - jdow + 3);
  const week = 1 + Math.round((thu - jan4) / (7 * 864e5));
  return {
    daily: u.toISOString().slice(0, 10),
    weekly: String(thu.getUTCFullYear()).padStart(4, '0') + '-W' + pad2(week),
    monthly: u.toISOString().slice(0, 7),
  };
}
function todaySpent(k) {
  return k.daily_cycle_key === cycleKeysNow().daily ? k.daily_spent_micro_usd : 0;
}
// balRow 配额清单的一行：名称 | 进度条 | 余 X / 上限，单行三列。
// 「不限」/数据缺失行也渲染空轨道：骨架→数据替换时行高不变、进度条不突兀消失。
function balRow(name, limit, remain, fmt) {
  if (!limit || limit <= 0) {
    return '<div class="bal-row bal-off"><span class="bal-name">' + name + '</span>'
      + '<span class="bal-bar"></span>'
      + '<span class="bal-free">不限</span></div>';
  }
  if (remain === null || remain === undefined) {
    return '<div class="bal-row"><span class="bal-name">' + name + '</span>'
      + '<span class="bal-bar"></span>'
      + '<span class="bal-val" title="余量数据缺失">—</span></div>';
  }
  const used = Math.min(limit, Math.max(0, limit - remain));
  const pct = Math.min(100, Math.max(0, used / limit * 100));
  const state = pct >= 95 ? 'alarm' : pct >= 80 ? 'warn' : '';
  return '<div class="bal-row" data-state="' + state + '">'
    + '<span class="bal-name">' + name + '</span>'
    + '<span class="bal-bar"><span style="width:' + pct.toFixed(1) + '%"></span></span>'
    + '<span class="bal-val mono">余 ' + fmt(Math.max(0, remain)) + ' / ' + fmt(limit) + '</span></div>';
}
function renderKeys() {
  // 服务端分页：cache 即当前页，total/status_counts 是筛选后的全量口径。
  const rows = keysView.cache;
  const allTotal = Object.values(keysView.statusCounts || {}).reduce((a, b) => a + Number(b) || 0, 0);
  const filtered = keysView.search || keysView.caller || keysView.status;
  $('key-count').textContent = '共 ' + allTotal + ' 枚'
    + (filtered ? ' · 筛选后 ' + keysView.total + ' 枚' : '');
  $('key-rows').innerHTML = rows.map(keyCardHTML).join('')
    || '<div class="empty"><p class="empty-title">没有匹配的密钥</p>'
    + '<p class="empty-hint">调整筛选条件，或点击右上角「签发密钥」</p></div>';

  const pages = Math.max(1, Math.ceil(keysView.total / keysView.size));
  $('key-pager').innerHTML = '<span class="mono">第 ' + (keysView.page + 1) + ' / ' + pages + ' 页</span>'
    + '<span class="grow"></span>'
    + '<button type="button" class="btn small" id="key-prev"' + (keysView.page <= 0 ? ' disabled' : '') + '>上一页</button>'
    + '<button type="button" class="btn small" id="key-next"'
    + (keysView.page + 1 >= pages ? ' disabled' : '') + '>下一页</button>';
  const prev = $('key-prev'), next = $('key-next');
  if (prev) prev.onclick = () => { keysView.page--; refreshKeys().catch(e => toast(e.message, 'err')); };
  if (next) next.onclick = () => { keysView.page++; refreshKeys().catch(e => toast(e.message, 'err')); };
}

// usdPick / tokPick 选出卡片余量块展示的那一档：优先总额，其次当前周期
// 尚未滚动的日/周/月（cycle key 不匹配即已跨期归零，读数按 0 计）。
// 用 null 判空而非真值判断：0 是「禁用」级真实限额，要照常渲染。
function usdPick(k) {
  const c = cycleKeysNow();
  if (k.quota_micro_usd !== null && k.quota_micro_usd !== undefined) return { lim: k.quota_micro_usd, used: k.spent_micro_usd };
  if (k.daily_micro_usd !== null && k.daily_micro_usd !== undefined) return { lim: k.daily_micro_usd, used: k.daily_cycle_key === c.daily ? k.daily_spent_micro_usd : 0 };
  if (k.weekly_micro_usd !== null && k.weekly_micro_usd !== undefined) return { lim: k.weekly_micro_usd, used: k.weekly_cycle_key === c.weekly ? k.weekly_spent_micro_usd : 0 };
  if (k.monthly_micro_usd !== null && k.monthly_micro_usd !== undefined) return { lim: k.monthly_micro_usd, used: k.monthly_cycle_key === c.monthly ? k.monthly_spent_micro_usd : 0 };
  return null;
}
function tokPick(k) {
  const c = cycleKeysNow();
  if (k.token_limit !== null && k.token_limit !== undefined) return { lim: k.token_limit, used: k.tokens_used };
  if (k.daily_token_limit !== null && k.daily_token_limit !== undefined) return { lim: k.daily_token_limit, used: k.daily_cycle_key === c.daily ? k.daily_tokens_used : 0 };
  if (k.weekly_token_limit !== null && k.weekly_token_limit !== undefined) return { lim: k.weekly_token_limit, used: k.weekly_cycle_key === c.weekly ? k.weekly_tokens_used : 0 };
  if (k.monthly_token_limit !== null && k.monthly_token_limit !== undefined) return { lim: k.monthly_token_limit, used: k.monthly_cycle_key === c.monthly ? k.monthly_tokens_used : 0 };
  return null;
}
// keyQuotaCell 卡片半格：标签、大字「余额 + 已用」一行，细进度条贴底
// 像一条边框带；两半的条同高对齐，读起来就是卡片下缘的一圈刻度。
function keyQuotaCell(q, kind, label, fmt) {
  const remain = Math.max(0, q.lim - q.used);
  const pct = q.lim > 0 ? Math.min(100, q.used / q.lim * 100) : 100;
  const state = q.lim <= 0 || pct >= 95 ? 'alarm' : pct >= 80 ? 'warn' : '';
  return '<div class="ky-cell" data-kind="' + kind + '" data-state="' + state + '"'
    + ' title="已用 ' + fmt(q.used) + ' / 上限 ' + fmt(q.lim) + '">'
    + '<span class="ky-cell-label">' + label + '</span>'
    + '<span class="ky-quota-row"><span class="ky-quota-num mono">余 ' + fmt(remain) + '</span>'
    + '<span class="ky-used mono">已用 ' + fmt(q.used) + '</span></span>'
    + '<span class="ky-bar"><span style="width:' + pct.toFixed(1) + '%"></span></span>'
    + '</div>';
}
const COPY_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>';
function kidShort(kid) {
  return kid.length > 12 ? kid.slice(0, 6) + '…' + kid.slice(-4) : kid;
}
function keyCardHTML(k) {
  const meta = STATUS_META[keyStatus(k)];
  const u = usdPick(k), t = tokPick(k);
  const cells =
    (u ? keyQuotaCell(u, 'usd', '金额（USD）', fmtUSD) : '')
    + (t ? keyQuotaCell(t, 'tok', 'Token', fmtTok) : '');
  return '<article class="ky-card" data-kid="' + esc(k.kid) + '" role="listitem" tabindex="0">'
    + '<div class="ky-card-top">'
    + '<span class="pill ' + meta.pill + '">' + meta.label + '</span>'
    + '<span class="ky-when">' + esc(rel(k.last_used_at)) + '</span></div>'
    + '<div class="ky-name-row">'
    + '<h3 class="ky-name">' + (k.label ? esc(k.label) : '<i>无标签</i>') + '</h3>'
    + '<button type="button" class="ky-kid mono" data-copy="' + esc(k.kid) + '" title="点击复制完整 kid：' + esc(k.kid) + '">'
    + '<span>' + esc(kidShort(k.kid)) + '</span>' + COPY_SVG + '</button>'
    + '</div>'
    + (cells
      ? '<div class="ky-split' + (u && t ? '' : ' ky-single') + '">' + cells + '</div>'
      : '<div class="ky-spent mono">累计已用 ' + fmtUSD(k.spent_micro_usd) + '</div>')
    + '<footer class="ky-meta">'
    + '<span class="ky-pair"><b>' + esc(k.caller_id || '-') + '</b>' + (k.caller_scope === 'key' ? '独立计额' : '归属 caller') + '</span>'
    + '<span>并发 ' + (k.max_concurrent_requests > 0 ? '≤ ' + k.max_concurrent_requests : '不限') + '</span>'
    + '</footer></article>';
}

// ---------- 详情 dialog ----------
// balanceSeq 守卫：快速连续打开时，晚返回的旧余额不得覆盖当前内容。
// balSkeletonRow 余额加载占位行：与 balRow 结构一致，数据到达原位替换不跳动。
function balSkeletonRow(name) {
  return '<div class="bal-row"><span class="bal-name">' + name + '</span>'
    + '<span class="bal-bar"><span class="skel"></span></span>'
    + '<span class="bal-val">—</span></div>';
}
function renderKeyDialog(k) {
  const kid = k.kid;
  const st = keyStatus(k);
  const meta = STATUS_META[st];
  // 只有配了 token 限额的 Key 才显示 token 那组配额，避免未用该功能的 Key
  // 详情里多出四行「不限」的空清单。骨架结构据此预先确定。
  const hasTok = [k.token_limit, k.daily_token_limit, k.weekly_token_limit, k.monthly_token_limit]
    .some(v => v !== null && v !== undefined);
  const d = $('key-dialog');
  d.dataset.kid = kid;
  d.innerHTML =
    '<header class="kd-head">'
    + '<h3>' + (k.label ? esc(k.label) : '<i>无标签</i>') + '</h3>'
    + '<span class="pill ' + meta.pill + '">' + meta.label + '</span>'
    + '<button type="button" class="ky-kid mono" data-copy="' + esc(kid) + '" title="点击复制完整 kid：' + esc(kid) + '">'
    + '<span>' + esc(kidShort(kid)) + '</span>' + COPY_SVG + '</button>'
    + '<button type="button" class="kd-close" aria-label="关闭详情">'
    + '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M6 6l12 12M18 6L6 18"/></svg>'
    + '</button></header>'
    + '<div class="kd-body">'
    + '<div class="detail-facts">'
    + fact('principal', k.principal || '-')
    + fact('额度口径', k.caller_scope === 'key' ? '独立计额' : '归属 caller')
    + fact('过期时间', k.expires_at ? fmtDT(k.expires_at) : '永不')
    + fact('创建于', fmtDT(k.created_at))
    + fact('周期计数', (k.daily_cycle_key || '-') + ' / ' + (k.weekly_cycle_key || '-') + ' / ' + (k.monthly_cycle_key || '-'))
    + fact('指纹', k.fingerprint || '-')
    + fact('可用模型', (k.allowed_models && k.allowed_models.length) ? k.allowed_models.join(', ') : '不限制')
    + fact('最近使用', rel(k.last_used_at))
    + '</div>'
    // 金额 | Token 对半两块；未配 token 时金额块独占整行
    + '<div class="kd-meters" id="kd-meters">'
    + '<section class="kd-quota-block"><div class="bal-title">金额额度（USD）</div>'
    + balSkeletonRow('总额度') + balSkeletonRow('今日') + balSkeletonRow('本周') + balSkeletonRow('本月')
    + '</section>'
    + (hasTok
      ? '<section class="kd-quota-block"><div class="bal-title">Token 限额</div>'
        + balSkeletonRow('总量') + balSkeletonRow('今日') + balSkeletonRow('本周') + balSkeletonRow('本月')
        + '</section>'
      : '')
    + '</div>'
    + '<p class="note" id="kd-note">余额核算中…</p>'
    + '<div class="btn-row">'
    + '<button type="button" class="btn small primary" data-act="edit">编辑</button>'
    + '<button type="button" class="btn small" data-act="rotate">轮换</button>'
    + '<button type="button" class="btn small" data-act="reveal">查看明文</button>'
    + (st !== 'revoked' ? '<button type="button" class="btn small danger" data-act="revoke">撤销</button>' : '')
    + '<button type="button" class="btn small danger" data-act="delete">删除</button>'
    + '</div></div>';

  const wire = act => d.querySelector('[data-act="' + act + '"]');
  wire('edit').onclick = () => editKeySheet(k);
  wire('rotate').onclick = () => rotateSheet(kid);
  wire('reveal').onclick = () => revealSheet(kid);
  const rv = wire('revoke');
  if (rv) rv.onclick = () => confirmSheet('撤销密钥 ' + kid,
    '撤销不可逆，该 Key 将立即无法通过鉴权。历史用量保留。',
    () => post('/keys/revoke', { kid, actor: 'console' }).then(refreshKeys));
  wire('delete').onclick = () => confirmSheet('删除密钥 ' + kid,
    '永久删除该 Key（历史用量保留）。操作不可逆。',
    () => post('/keys/delete', { kid, actor: 'console' }).then(refreshKeys));
  d.querySelector('.kd-close').onclick = animateCloseKeyDialog;

  const seq = ++keysView.balanceSeq;
  api('/balance?key_id=' + encodeURIComponent(kid)).then(b => {
    if (seq !== keysView.balanceSeq || !d.open || d.dataset.kid !== kid) return;
    const wrap = $('kd-meters');
    if (!wrap) return;
    // 字段名必须与 service.Balance 的 JSON tag 一致。结构与骨架一致，原位替换无跳动。
    wrap.innerHTML =
      '<section class="kd-quota-block"><div class="bal-title">金额额度（USD）</div>'
      + balRow('总额度', k.quota_micro_usd, b.total_remaining_micro_usd, fmtUSD)
      + balRow('今日', k.daily_micro_usd, b.daily_remaining_micro_usd, fmtUSD)
      + balRow('本周', k.weekly_micro_usd, b.weekly_remaining_micro_usd, fmtUSD)
      + balRow('本月', k.monthly_micro_usd, b.monthly_remaining_micro_usd, fmtUSD)
      + '</section>'
      + (hasTok
        ? '<section class="kd-quota-block"><div class="bal-title">Token 限额</div>'
          + balRow('总量', k.token_limit, b.total_remaining_tokens, fmtTok)
          + balRow('今日', k.daily_token_limit, b.daily_remaining_tokens, fmtTok)
          + balRow('本周', k.weekly_token_limit, b.weekly_remaining_tokens, fmtTok)
          + balRow('本月', k.monthly_token_limit, b.monthly_remaining_tokens, fmtTok)
          + '</section>'
        : '');
    const note = $('kd-note');
    if (note) note.textContent = '在途预占 ' + fmtUSD(b.held_micro_usd || 0)
      + (hasTok ? ' / ' + fmtTok(b.held_tokens) + ' token' : '')
      + ' · 当前周期 ' + cycleKeysNow().daily;
  }).catch(() => { /* 余额核算失败不打断详情 */ });
}

let kdOpener = null;   // 关闭 dialog 后焦点回到打开它的卡片
function openKeyDialog(kid, opener) {
  const k = keysView.cache.find(x => x.kid === kid);
  if (!k) return;
  if (opener) kdOpener = opener;
  renderKeyDialog(k);
  $('key-dialog').showModal();
}
// 关闭过渡：原生 close() 是瞬时消失，先播退出动画再真正关闭
function animateCloseKeyDialog() {
  const dlg = $('key-dialog');
  if (!dlg.open || dlg.classList.contains('closing')) return;
  dlg.classList.add('closing');
  setTimeout(() => { dlg.classList.remove('closing'); dlg.close(); }, 150);
}

$('key-rows').addEventListener('click', e => {
  const copy = e.target.closest('[data-copy]');
  if (copy) {
    copyText(copy.dataset.copy).then(() => toast('kid 已复制')).catch(() => toast('复制失败', 'err'));
    return;
  }
  const card = e.target.closest('.ky-card');
  if (card) openKeyDialog(card.dataset.kid, card);
});
$('key-rows').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const card = e.target.closest('.ky-card');
  if (!card || e.target !== card) return;
  e.preventDefault();
  openKeyDialog(card.dataset.kid, card);
});
// dialog：背板点击关闭；内部 kid 复制行与网格同一套 data-copy 约定。
// Esc 经 cancel 事件接入同一套退出动画（preventDefault 后自行关闭）。
$('key-dialog').addEventListener('click', e => {
  if (e.target === e.currentTarget) { animateCloseKeyDialog(); return; }
  const copy = e.target.closest('[data-copy]');
  if (copy) copyText(copy.dataset.copy).then(() => toast('kid 已复制')).catch(() => toast('复制失败', 'err'));
});
$('key-dialog').addEventListener('cancel', e => {
  e.preventDefault();
  animateCloseKeyDialog();
});
$('key-dialog').addEventListener('close', () => {
  if (kdOpener && document.contains(kdOpener)) kdOpener.focus();
  kdOpener = null;
});

$('key-search').addEventListener('input', debounce(() => {
  keysView.search = $('key-search').value.trim();
  keysView.page = 0;
  refreshKeys().catch(e => toast(e.message, 'err'));
}, 350));
$('key-search').addEventListener('keydown', e => { if (e.key === 'Enter') e.preventDefault(); });
// 状态筛选 chips：与 STATUS_META 的文案保持一致
const KEY_STATUS_CHIPS = [
  ['', '全部'], ['active', '启用中'], ['disabled', '已禁用'], ['revoked', '已撤销'], ['expired', '已过期'],
];
function renderStatusChips() {
  $('key-status-chips').innerHTML = KEY_STATUS_CHIPS.map(([v, label]) =>
    '<button type="button" class="ky-chip' + (keysView.status === v ? ' on' : '') + '"'
    + ' data-v="' + v + '" aria-pressed="' + (keysView.status === v) + '">' + label + '</button>').join('');
}
$('key-status-chips').addEventListener('click', e => {
  const b = e.target.closest('button[data-v]');
  if (!b || b.dataset.v === keysView.status) return;
  keysView.status = b.dataset.v;
  keysView.page = 0;
  renderStatusChips();
  refreshKeys().catch(e2 => toast(e2.message, 'err'));
});
renderStatusChips();
const keyCallerSel = new Select('key-caller', [{ value: '', label: '全部 caller' }], v => {
  keysView.caller = v;
  keysView.page = 0;
  refreshKeys().catch(e => toast(e.message, 'err'));
}, { value: '', head: '按 caller 过滤' });
// callersCache 供签发弹窗重建 caller 下拉（原先克隆 select 的 innerHTML，改组件后必须走数据源）
let callersCache = [];
async function loadCallers() {
  try {
    const r = await api('/callers');
    callersCache = r.items || [];
    keyCallerSel.setOptions([{ value: '', label: '全部 caller' }].concat(
      callersCache.map(c => ({ value: c.id, label: (c.display_name || c.id) + (c.enabled ? '' : '（停用）') }))));
  } catch (e) { /* caller 下拉失败不阻塞 */ }
}
// callerOptionsHTML 给弹窗内的原生 <select> 用（弹窗表单保持原生控件）
function callerOptionsHTML() {
  return '<option value="">默认 caller</option>' + callersCache.map(c =>
    '<option value="' + esc(c.id) + '">' + esc(c.display_name || c.id)
    + (c.enabled ? '' : '（停用）') + '</option>').join('');
}

// 签发
$('key-issue-btn').addEventListener('click', () => {
  openSheet({
    title: '签发插件密钥',
    okText: '签发',
    body: '<div class="form-grid">'
      + fieldRow('标签', '<input id="f-label" placeholder="如：张三的测试 Key">')
      + fieldRow('principal', '<input id="f-principal" placeholder="可选，属主标识">')
      + fieldRow('caller', '<select id="f-caller">' + callerOptionsHTML() + '</select>')
      + fieldRow(labelWithTip('额度口径', TIPS.callerScope),
        '<select id="f-scope"><option value="caller">归属 caller 共享</option><option value="key">独立计额</option></select>')
      + fieldRow('过期时间', '<input id="f-expires" type="datetime-local">')
      + fieldRow('最大并发', '<input id="f-conc" type="number" min="0" placeholder="0 为不限">')
      + '<div class="form-sep wide kd-mode-row"><div class="seg" id="f-mode" role="group" aria-label="计费方式">'
      + '<button type="button" data-m="usd">按金额（USD）</button>'
      + '<button type="button" data-m="tok">按 Token</button></div>'
      + '<span class="note">二选一：一个 Key 只能用一种计费方式</span></div>'
      + '<div class="fg-sub" id="g-money">'
      + fieldRow(labelWithTip('总额度', TIPS.moneyLimit), '<input id="f-quota" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('日限额', '<input id="f-daily" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('周限额', '<input id="f-weekly" inputmode="decimal" placeholder="留空为不限">')
      + fieldRow('月限额', '<input id="f-monthly" inputmode="decimal" placeholder="留空为不限">')
      + '</div>'
      + '<div class="fg-sub off" id="g-tok">'
      + fieldRow(labelWithTip('总量', TIPS.tokenLimit), '<input id="f-tok" inputmode="numeric" placeholder="留空为不限，支持 500k / 1.5m">')
      + fieldRow('日限额', '<input id="f-tok-daily" inputmode="numeric" placeholder="留空为不限">')
      + fieldRow('周限额', '<input id="f-tok-weekly" inputmode="numeric" placeholder="留空为不限">')
      + fieldRow('月限额', '<input id="f-tok-monthly" inputmode="numeric" placeholder="留空为不限">')
      + '</div>'
      + fieldRow('可用模型', '<textarea id="f-models" placeholder="逗号或换行分隔，支持 * 通配；留空不限制"></textarea>', 'wide')
      + '</div>',
    note: '明文只在签发结果里出现一次。计费方式金额/Token 二选一；限额留空为不限。',
    onOk: async () => {
      const mode = document.querySelector('#f-mode button.on').dataset.m;
      const num = id => {
        const v = $(id).value.trim();
        if (!v || v === '-1') return null;
        const n = parseFloat(v);
        if (!isFinite(n) || n < 0) throw new Error('金额限额须为不小于 0 的数字');
        return Math.round(n * 1e6);
      };
      // token 数支持 1000 / 1k / 1.5m / 2b 几种写法，避免手数零
      const tok = id => {
        const v = $(id).value.trim();
        if (!v || v === '-1') return null;
        const n = parseTokens(v);
        if (n < 0) throw new Error('Token 限额不接受负数');
        return n;
      };
      const models = $('f-models').value.split(/[\n,，]/).map(s => s.trim()).filter(Boolean);
      const expires = $('f-expires').value ? new Date($('f-expires').value).toISOString() : null;
      const r = await post('/keys/issue', {
        label: $('f-label').value.trim(),
        principal: $('f-principal').value.trim(),
        caller_id: $('f-caller').value || 'default',
        caller_scope: $('f-scope').value,
        quota_micro_usd: mode === 'usd' ? num('f-quota') : null,
        daily_micro_usd: mode === 'usd' ? num('f-daily') : null,
        weekly_micro_usd: mode === 'usd' ? num('f-weekly') : null,
        monthly_micro_usd: mode === 'usd' ? num('f-monthly') : null,
        token_limit: mode === 'tok' ? tok('f-tok') : null,
        daily_token_limit: mode === 'tok' ? tok('f-tok-daily') : null,
        weekly_token_limit: mode === 'tok' ? tok('f-tok-weekly') : null,
        monthly_token_limit: mode === 'tok' ? tok('f-tok-monthly') : null,
        max_concurrent_requests: parseInt($('f-conc').value, 10) || 0,
        allowed_models: models,
        expires_at: expires,
        actor: 'console',
      });
      $('sheet-title').textContent = '密钥已签发 · ' + r.KID;
      $('sheet-body').innerHTML = secretBlock(r.Key)
        + '<p class="note">指纹 ' + esc(r.Fingerprint) + '</p>';
      $('sheet-note').textContent = '';
      wireSecretCopy();
      staySheet('完成');
      refreshKeys().catch(() => {});
      return false;
    },
  });
  // 计费方式切换（openSheet 同步建好 DOM，这里直接绑）
  const setMode = m => {
    $('g-money').classList.toggle('off', m !== 'usd');
    $('g-tok').classList.toggle('off', m !== 'tok');
    document.querySelectorAll('#f-mode button').forEach(b => b.classList.toggle('on', b.dataset.m === m));
  };
  $('f-mode').addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (b) setMode(b.dataset.m);
  });
  setMode('usd');
});

// 编辑
function editKeySheet(k) {
  const MONEY_FIELDS = [
    ['e-quota', 'quota_micro_usd', '总额度'],
    ['e-daily', 'daily_micro_usd', '日限额'],
    ['e-weekly', 'weekly_micro_usd', '周限额'],
    ['e-monthly', 'monthly_micro_usd', '月限额'],
  ];
  const TOKEN_FIELDS = [
    ['e-tok', 'token_limit', '总量'],
    ['e-tok-daily', 'daily_token_limit', '日限额'],
    ['e-tok-weekly', 'weekly_token_limit', '周限额'],
    ['e-tok-monthly', 'monthly_token_limit', '月限额'],
  ];
  // 全量回填当前生效值：不限显示 -1，用户在现有基础上直接改。
  // 提交时所有字段原样发回（-1 由后端归一为不限），不再有「留空=不改」的隐式语义。
  const curMoney = f => k[f] === null || k[f] === undefined ? '-1' : String(k[f] / 1e6);
  const curTok = f => k[f] === null || k[f] === undefined ? '-1' : String(k[f]);
  openSheet({
    title: '编辑密钥 ' + k.kid,
    okText: '保存',
    body: '<div class="form-grid">'
      + fieldRow('标签', '<input id="e-label" value="' + esc(k.label || '') + '">')
      + fieldRow('启用', '<select id="e-enabled"><option value="true"' + (k.enabled ? ' selected' : '') + '>是</option>'
        + '<option value="false"' + (!k.enabled ? ' selected' : '') + '>否</option></select>')
      + fieldRow('过期时间', '<input id="e-expires" type="datetime-local" value="'
        + (k.expires_at ? toLocalInput(new Date(k.expires_at)) : '') + '">')
      + fieldRow('最大并发', '<input id="e-conc" type="number" min="0" value="' + (k.max_concurrent_requests || 0) + '">')
      + '<div class="form-sep wide kd-mode-row"><div class="seg" id="e-mode" role="group" aria-label="计费方式">'
      + '<button type="button" data-m="usd">按金额（USD）</button>'
      + '<button type="button" data-m="tok">按 Token</button></div>'
      + '<span class="note">二选一：切换后另一族限额会被清除</span></div>'
      + '<div class="fg-sub" id="g-money">'
      + MONEY_FIELDS.map(([id, field, label]) =>
        fieldRow(label, '<input id="' + id + '" inputmode="decimal" value="'
          + esc(curMoney(field)) + '">')).join('')
      + '</div>'
      + '<div class="fg-sub off" id="g-tok">'
      + TOKEN_FIELDS.map(([id, field, label]) =>
        fieldRow(label, '<input id="' + id + '" inputmode="numeric" value="'
          + esc(curTok(field)) + '">')).join('')
      + '</div>'
      + fieldRow('可用模型', '<textarea id="e-models" placeholder="留空表示不限制模型">'
        + esc((k.allowed_models || []).join(', ')) + '</textarea>', 'wide')
      + '</div>',
    note: '字段已按当前值回填，改完保存即可。计费方式金额/Token 二选一，切换后另一族清除。限额：正数=上限，0=禁用，-1=不限；Token 也接受 500k / 1.5m 写法。过期时间留空表示永不过期。',
    onOk: async () => {
      const mode = $('g-tok').classList.contains('off') ? 'usd' : 'tok';
      const body = { kid: k.kid, actor: 'console' };
      body.label = $('e-label').value.trim();
      body.enabled = $('e-enabled').value === 'true';
      const exp = $('e-expires').value;
      body.expires_at = exp ? new Date(exp).toISOString() : null;
      body.max_concurrent_requests = parseInt($('e-conc').value, 10) || 0;
      for (const [id, field] of TOKEN_FIELDS) {
        if (mode !== 'tok') { body[field] = -1; continue; }
        const raw = $(id).value.trim();
        if (raw === '' || raw === '-1') { body[field] = -1; continue; }
        const n = parseTokens(raw); // 非法写法直接抛错，由 sheet 捕获成提示
        if (n < 0) throw new Error('Token 限额不接受负数；-1 表示不限');
        body[field] = n;
      }
      for (const [id, field] of MONEY_FIELDS) {
        if (mode !== 'usd') { body[field] = -1; continue; }
        const raw = $(id).value.trim();
        if (raw === '' || raw === '-1') { body[field] = -1; continue; }
        const numv = parseFloat(raw);
        if (!isFinite(numv) || numv < 0) throw new Error('金额限额须为不小于 0 的数字（-1 表示不限）');
        body[field] = Math.round(numv * 1e6);
      }
      const modelsRaw = $('e-models').value.trim();
      body.allowed_models = modelsRaw
        ? modelsRaw.split(/[\n,，]/).map(s => s.trim()).filter(Boolean)
        : [];
      await post('/keys/update', body);
      toast('密钥已更新', 'ok');
      refreshKeys().catch(() => {});
    },
  });
  // 计费方式切换：默认沿用该 Key 现有口径（配了 token 即 tok），切换后另一族提交 -1 清除
  const setMode = m => {
    $('g-money').classList.toggle('off', m !== 'usd');
    $('g-tok').classList.toggle('off', m !== 'tok');
    document.querySelectorAll('#e-mode button').forEach(b => b.classList.toggle('on', b.dataset.m === m));
  };
  $('e-mode').addEventListener('click', e => {
    const b = e.target.closest('button[data-m]');
    if (b) setMode(b.dataset.m);
  });
  setMode(tokPick(k) ? 'tok' : 'usd');
}
function rotateSheet(kid) {
  openSheet({
    title: '轮换 ' + kid, danger: true, okText: '轮换',
    body: '<p>旧 Key 立即失效，并生成新明文（仅展示一次）。</p>',
    onOk: async () => {
      const r = await post('/keys/rotate', { kid, actor: 'console' });
      $('sheet-title').textContent = '已轮换 · ' + r.KID;
      $('sheet-body').innerHTML = secretBlock(r.Key);
      wireSecretCopy();
      staySheet('完成');
      refreshKeys().catch(() => {});
      return false;
    },
  });
}
function revealSheet(kid) {
  openSheet({
    title: '查看明文 ' + kid, okText: '解密',
    body: '<p>解密该 Key 的明文用于配置客户端。</p>',
    onOk: async () => {
      const r = await post('/keys/reveal', { kid, actor: 'console' });
      $('sheet-title').textContent = '明文 · ' + kid;
      $('sheet-body').innerHTML = secretBlock(r.key);
      wireSecretCopy();
      staySheet('关闭');
      return false;
    },
  });
}

// ---------- 用量 ----------
const REQ_SIZES = [20, 50, 100];
const reqView = {
  page: 0, size: +(localStorage.getItem('req-size') || 20) || 20,
  sort: 'ts', order: 'desc', model: '', keyId: '', result: '',
};

// 请求明细列定义。
// 默认列刻意收进视口宽度内：13 列独立排布时表格 1322px > 容器 1184px，
// 叠加外层 max-height 的竖向滚动后一个卡片里两个方向都要拖。
// 计量三列与延迟两列合并成复合单元格，其余低频列改为按需开启。
//
// sort 只填后端 ListRequests 白名单里真实存在的键（ts/cost/tokens/latency/model）：
// 其余列不给排序 affordance，避免「看起来能点但点了没反应」。
const REQ_COLS = [
  { id: 'ts', label: '时间', sort: 'ts', fixed: true, cell: x => '<td class="cell-mono">' + fmtDT(x.ts, true) + '</td>' },
  {
    id: 'key', label: '密钥', cell: x => '<td class="cell-mono" title="' + esc(x.key_id || '') + '">'
      + esc(keyLabelOf(x.key_id) || x.key_id || '-') + '</td>',
  },
  {
    id: 'model', label: '模型', sort: 'model', cell: x => '<td class="cell-mono cell-clip" title="' + esc(x.model) + '">'
      + esc(x.model || '-') + '</td>',
  },
  { id: 'provider', label: '提供方', off: true, cell: x => '<td class="cell-dim">' + esc(x.provider || '-') + '</td>' },
  {
    id: 'result', label: '结果', cell: x => '<td><span class="pill '
      + (x.result === 'ok' ? 'live' : x.result === 'blocked' ? 'warn' : 'alarm') + '">' + esc(x.result) + '</span></td>',
  },
  {
    id: 'toks', label: '输入 / 输出 / 缓存读', num: true,
    tip: '三段分别为 输入 / 输出 / 缓存读。' + TIPS.cacheRead,
    cell: x => '<td class="num"><span class="cell-toks" title="输入 ' + esc(fmtTok(x.input_tokens))
      + ' · 输出 ' + esc(fmtTok(x.output_tokens)) + ' · 缓存读 ' + esc(fmtTok(cacheReadOf(x))) + '">'
      + '<span class="tk">' + fmtTok(x.input_tokens) + '</span><span class="sep">/</span>'
      + '<span class="tk out">' + fmtTok(x.output_tokens) + '</span><span class="sep">/</span>'
      + '<span class="tk cr">' + fmtTok(cacheReadOf(x)) + '</span></span></td>',
  },
  {
    id: 'tokens', label: '总 Token', sort: 'tokens', num: true,
    tip: '计费四类合计：输入＋输出＋缓存读＋缓存写。与 Token 限额同一口径。',
    cell: x => '<td class="num">' + reqTokenCell(x) + '</td>',
  },
  { id: 'cost', label: '费用', sort: 'cost', num: true, cell: x => '<td class="num">' + fmtUSD(x.cost_micro_usd) + '</td>' },
  {
    // 排序键指向 latency（总延迟）。旧版把「首字」表头标成 data-sort="latency"，
    // 而 latency 在后端映射到 latency_ms，点「首字」实际按总延迟排 —— 表头与行为不一致。
    id: 'lat', label: '延迟 首字→总', sort: 'latency', num: true,
    tip: '首字延迟 → 总延迟。首字延迟是收到第一个 token 的耗时，总延迟含整段生成。',
    cell: x => '<td class="num"><span class="cell-lat"><span class="ttft">' + fmtSec(x.ttft_ms)
      + '</span><span class="arrow">→</span><span>' + fmtSec(x.latency_ms) + '</span></span></td>',
  },
  { id: 'tps', label: 'TPS', num: true, off: true, cell: x => '<td class="num">' + fmtTPS(x.tps_milli) + '</td>' },
  { id: 'reasoning', label: '推理', num: true, off: true, cell: x => '<td class="num">' + fmtTok(x.reasoning_tokens) + '</td>' },
  { id: 'tier', label: '档位', off: true, cell: x => '<td class="cell-dim">' + esc(x.tier || '-') + '</td>' },
];
const REQ_COLS_DEFAULT = REQ_COLS.filter(c => !c.off).map(c => c.id);
function loadReqCols() {
  try {
    const saved = JSON.parse(localStorage.getItem('req-cols') || 'null');
    if (Array.isArray(saved) && saved.length) {
      const valid = saved.filter(id => REQ_COLS.some(c => c.id === id));
      if (valid.length) return new Set(valid.concat(REQ_COLS.filter(c => c.fixed).map(c => c.id)));
    }
  } catch (_) { /* 偏好损坏则回默认 */ }
  return new Set(REQ_COLS_DEFAULT);
}
let reqCols = loadReqCols();
const activeReqCols = () => REQ_COLS.filter(c => reqCols.has(c.id));
// reqTokenCell 总 Token 单元格：上游未返回用量但已按预占扣费时给出显式标记。
function reqTokenCell(x) {
  const noUsage = !(+x.input_tokens || 0) && !(+x.output_tokens || 0)
    && !(+x.cache_read_tokens || 0) && !(+x.cache_creation_tokens || 0);
  return noUsage && (+x.cost_micro_usd || 0) > 0
    ? '<span class="pill warn mono" title="上游未返回用量，费用按预占估算扣费">未捕获</span>'
    : '<b>' + fmtTok(effTokens(x)) + '</b>';
}

// fillReqSuggestions 填充模型/密钥筛选框的联想候选（自建组合框，标签与 kid 同时可见）。
function fillReqSuggestions() {
  api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '200' }))
    .then(r => reqModelCombo.setOptions((r.rows || []).filter(x => x.value)
      .map(x => ({ value: x.value, label: x.value }))))
    .catch(() => {});
  // 值提交 kid，但标签同时显示 —— datalist 在 Firefox 只显示 value，标签会丢
  // 注：密钥列表改为服务端分页后，此处候选只覆盖当前页（全量候选需单独轻量
  // 接口，待产品确认后再加）。
  reqKeyCombo.setOptions(keysView.cache.map(k => ({
    value: k.kid, label: k.label || '(无标签)', sub: k.kid,
  })));
}

loaders.usage = async () => {
  await Promise.all([loadDim(), loadRoutes().catch(e => { $('route-body').innerHTML = '<div class="empty"><p class="empty-hint">' + esc(e.message) + '</p></div>'; })]);
  await loadRequests();
  fillReqSuggestions();
  stamp();
};
let routeRows = [], routePage = 0, routeModel = '';
const ROUTE_PAGE_SIZE = 5;
// 必须选中本地别名才展示路由（无「全部」态）；未选时 value='' 显示占位符。
const routeModelSel = new Select('route-model', [],
  v => { routeModel = v; routePage = 0; renderRoutes(); }, { head: '按别名筛选', placeholder: '选择本地别名…' });
async function loadRoutes() {
  const r = await api('/routes?' + new URLSearchParams(rangeParams()));
  routeRows = r.items || [];
  const names = [...new Set(routeRows.flatMap(r => r.models || []))].sort((a, b) => a.localeCompare(b));
  if (routeModel && !names.includes(routeModel)) { routeModel = ''; routeModelSel.value = ''; }
  routeModelSel.setOptions(names.map(n => ({ value: n, label: n })));
  renderRoutes();
}
function renderRoutes() {
  // 表格区与换页栏始终同构渲染：fixed5 撑住高度，换页栏位置不随内容浮动。
  if (!routeModel) {
    $('route-body').innerHTML = '<div class="table-wrap fixed5"><div class="empty">'
      + '<p class="empty-title">未选择本地别名</p>'
      + '<p class="empty-hint">在右上角选择一个本地别名，查看它实际路由到的上游模型</p></div></div>'
      + '<div class="pager"><span class="mono">第 0 / 0 页 · 共 0 条映射</span><span class="grow"></span>'
      + '<button type="button" class="btn small" disabled>上一页</button>'
      + '<button type="button" class="btn small" disabled>下一页</button></div>';
    $('route-count').textContent = '';
    return;
  }
  const rowsAll = routeRows.filter(r => (r.models || []).includes(routeModel));
  const total = rowsAll.reduce((a, r) => a + (Number(r.requests) || 0), 0);
  const maxReq = Math.max(1, ...rowsAll.map(r => Number(r.requests) || 0));
  const shareOf = r => total > 0 ? (Number(r.requests) || 0) / total * 100 : 0;
  const pages = Math.max(1, Math.ceil(rowsAll.length / ROUTE_PAGE_SIZE));
  if (routePage >= pages) routePage = pages - 1;
  const rows = rowsAll.slice(routePage * ROUTE_PAGE_SIZE, (routePage + 1) * ROUTE_PAGE_SIZE);
  // 首列与维度聚合同构：名称行 + 占比 + 条形；行按上游真名聚合，不展示本地别名与提供商。
  $('route-body').innerHTML = '<div class="table-wrap fixed5"><table class="data"><thead><tr>'
    + '<th class="w-grow">上游模型</th>'
    + '<th class="num">请求</th><th class="num">Token</th></tr></thead><tbody>'
    + rows.map(rw => {
      const up = rw.upstream_model || '(未知)';
      const name = '<span class="bar-name">' + esc(up) + '</span>';
      return '<tr>'
      + '<td><div class="bar-cell" title="' + esc(up) + '">'
        + '<div class="bar-top">' + name + '<span class="bar-pct">'
        + (shareOf(rw) < 10 ? shareOf(rw).toFixed(1) : shareOf(rw).toFixed(0)) + '%</span></div>'
        + '<div class="bar-line"><span style="width:'
        + ((Number(rw.requests) || 0) / maxReq * 100).toFixed(1) + '%"></span></div></div></td>'
      + '<td class="num">' + fmtInt(rw.requests) + '</td>'
      + '<td class="num">' + fmtTok(rw.total_tokens) + '</td></tr>';
    }).join('')
    + '</tbody></table></div>'
    + '<div class="pager" id="route-pager"><span class="mono">第 ' + (routePage + 1) + ' / ' + pages + ' 页 · 共 '
      + fmtInt(rowsAll.length) + ' 条映射</span><span class="grow"></span>'
      + '<button type="button" class="btn small" id="route-prev"' + (routePage <= 0 ? ' disabled' : '') + '>上一页</button>'
      + '<button type="button" class="btn small" id="route-next"'
      + ((routePage + 1) * ROUTE_PAGE_SIZE >= rowsAll.length ? ' disabled' : '') + '>下一页</button></div>';
  $('route-count').textContent = fmtInt(total) + ' 次请求 · ' + fmtInt(rowsAll.length) + ' 条映射';
  const prev = $('route-prev'), next = $('route-next');
  if (prev) prev.onclick = () => { routePage--; renderRoutes(); };
  if (next) next.onclick = () => { routePage++; renderRoutes(); };
}
const DIMS = [
  { value: 'model', label: '模型' },
  { value: 'provider', label: '提供方' },
  { value: 'source', label: '来源' },
  { value: 'auth_type', label: '认证类型' },
  { value: 'auth_label', label: '认证账号' },
  { value: 'result', label: '结果' },
  { value: 'key_id', label: '密钥' },
  { value: 'caller_id', label: 'caller' },
];
let dimRows = [], dimPage = 0, dimSort = 'cost', dimDir = 'desc';
const DIM_PAGE_SIZE = 5;
async function loadDim() {
  const dim = dimSel.value;
  // 与概览页口径一致：显式带 limit，后端还有 500 硬上限兜底。
  const r = await api('/usage/dimension?' + new URLSearchParams({ dimension: dim, limit: '50', ...rangeParams() }));
  dimRows = r.rows || [];
  sortDimRows();
  dimPage = 0;
  renderDim();
}
// 排序键由表头点击驱动（与请求明细同款交互），客户端全量重排。
function sortDimRows() {
  const val = r => dimSort === 'requests' ? (Number(r.requests) || 0)
    : dimSort === 'failures' ? (Number(r.failures) || 0)
    : dimSort === 'tokens' ? (Number(effTokens(r)) || 0)
    : (Number(r.cost_micro_usd) || 0);
  const sgn = dimDir === 'asc' ? 1 : -1;
  dimRows.sort((a, b) => sgn * (val(a) - val(b)));
}
function renderDim() {
  const rowsAll = dimRows;
  // 占比 = 本行请求数 / 所有分组请求数之和，各行之和为 100%（四舍五入误差除外）。
  //
  // 曾用「最大行请求数」作分母（v0.3.0 为修 >100% 而引入），那算的是「相对最大值的
  // 比例」而不是占比：最大行恒显示 100%，且各行相加远超 100%（result 维度下
  // ok 100% + error 8% = 108%，provider 维度累计 263%），与列名和常识都不符。
  //
  // 分母用全量行之和而非服务端 total —— 服务端的 total 是**只对返回行**累加的，
  // 带分页后每页只渲染一部分，占比与条长必须按全量数据归一才稳定。
  const denom = rowsAll.reduce((a, row) => a + (Number(row.requests) || 0), 0);
  const maxReq = Math.max(1, ...rowsAll.map(row => Number(row.requests) || 0));
  const shareOf = row => denom > 0 ? (Number(row.requests) || 0) / denom * 100 : 0;
  // 密钥维度的分组值是 kid，显示标签更可读（与请求表/概览/详情弹窗同口径），
  // kid 保留在 title 里。其余维度分组值本身就是可读文本。
  const nameOf = row => {
    const v = row.value || '';
    if (dimSel.value === 'key_id' && v) return keyLabelOf(v) || v;
    return v || '(空)';
  };
  const titleOf = row => {
    const v = row.value || '';
    if (dimSel.value === 'key_id' && v) {
      const label = keyLabelOf(v);
      return label ? label + ' · ' + v : v;
    }
    return v || '(空)';
  };
  const pages = Math.max(1, Math.ceil(rowsAll.length / DIM_PAGE_SIZE));
  if (dimPage >= pages) dimPage = pages - 1;
  const rows = rowsAll.slice(dimPage * DIM_PAGE_SIZE, (dimPage + 1) * DIM_PAGE_SIZE);
  // 数值列表头可点击排序（同请求明细：th.sort + data-dir，点击换键/切向）。
  const th = (label, key, num, wide) => '<th class="' + (wide ? 'w-grow' : num ? 'num' : '')
    + (key ? ' sort"' : '"')
    + (key ? ' data-sort="' + key + '"' : '')
    + (key && dimSort === key ? ' data-dir="' + dimDir + '"' : '') + '>' + label + '</th>';
  $('dim-body').innerHTML = '<div class="table-wrap fixed5"><table class="data"><thead><tr>'
    + th(esc((DIMS.find(d => d.value === dimSel.value) || {}).label || dimSel.value), '', false, true)
    + th('请求', 'requests', true)
    + th('失败', 'failures', true)
    + th('Token', 'tokens', true)
    + th('费用', 'cost', true)
    + th('缓存', '', true)
    + th('平均延迟', '', true)
    + th('TPS', '', true) + '</tr></thead><tbody>'
    + rows.map(row => {
      const share = shareOf(row);
      const hit = cacheHitRate(row);
      return '<tr>'
      + '<td><div class="bar-cell" title="' + esc(titleOf(row)) + '"><div class="bar-top"><span class="bar-name">'
      + esc(nameOf(row)) + '</span><span class="bar-pct">'
      + (share < 10 ? share.toFixed(1) : share.toFixed(0)) + '%</span></div>'
      + '<div class="bar-line"><span style="width:'
      + ((Number(row.requests) || 0) / maxReq * 100).toFixed(1) + '%"></span></div></div></td>'
      + '<td class="num">' + fmtInt(row.requests) + '</td>'
      + '<td class="num">' + (row.failures ? '<span class="pill alarm mono">' + fmtInt(row.failures) + '</span>' : '0') + '</td>'
      + '<td class="num">' + fmtTok(effTokens(row)) + '</td>'
      + '<td class="num">' + fmtUSD(row.cost_micro_usd) + '</td>'
      + '<td class="num">' + (hit >= 0 ? hit.toFixed(1) + '%' : '—') + '</td>'
      + '<td class="num">' + fmtSec(row.latency_avg_ms) + '</td>'
      + '<td class="num">' + fmtTPS(row.tps_avg_milli) + '</td></tr>';
    }).join('')
    + '</tbody></table></div>'
    + '<div class="pager" id="dim-pager"><span class="mono">第 ' + (dimPage + 1) + ' / ' + pages + ' 页 · 共 '
      + fmtInt(rowsAll.length) + ' 项</span><span class="grow"></span>'
      + '<button type="button" class="btn small" id="dim-prev"' + (dimPage <= 0 ? ' disabled' : '') + '>上一页</button>'
      + '<button type="button" class="btn small" id="dim-next"'
      + ((dimPage + 1) * DIM_PAGE_SIZE >= rowsAll.length ? ' disabled' : '') + '>下一页</button></div>';
  const prev = $('dim-prev'), next = $('dim-next');
  if (prev) prev.onclick = () => { dimPage--; renderDim(); };
  if (next) next.onclick = () => { dimPage++; renderDim(); };
}
// 表头排序：#dim-body 常驻不重建，事件委托一次绑定即可。
$('dim-body').addEventListener('click', e => {
  const thEl = e.target.closest('th.sort');
  if (!thEl) return;
  const keyName = thEl.dataset.sort;
  if (dimSort === keyName) dimDir = dimDir === 'desc' ? 'asc' : 'desc';
  else { dimSort = keyName; dimDir = 'desc'; }
  sortDimRows();
  dimPage = 0;
  renderDim();
});
// fmtTPS 展示 TPS：超过 3000 token/s 视为宿主缓冲整转产生的坏测量
// （与后端落库上限同口径），v0.3.0 之前入库的历史脏行在展示层一并隐藏。
const maxPlausibleTPS = 3000;
const fmtTPS = milli => milli > 0 && milli / 1000 <= maxPlausibleTPS ? (milli / 1000).toFixed(1) : '-';

function kv(name, value) { return '<div class="kv-row"><dt>' + name + '</dt><dd>' + value + '</dd></div>'; }
// renderCostCoverage 渲染概览第三卡（计价覆盖）；costs 由 overview loader 统一拉取。
function renderCostCoverage(costs) {
  const cover = costs.requests ? Math.round(costs.priced_requests / costs.requests * 100) : 0;
  const rate = S.fx;
  const cny = rate && costs.cost_micro_usd
    ? '¥' + fmtUSD(Math.round(costs.cost_micro_usd * rate.usd_to_cny_micro / 1e6)).slice(1) : '-';
  $('ov-cost-body').innerHTML = '<div class="kv">'
    + kv('请求总数', fmtInt(costs.requests))
    + kv('已计价请求', fmtInt(costs.priced_requests))
    + kv('价格覆盖率', '<span class="pill ' + (cover >= 90 ? 'live' : cover >= 60 ? 'warn' : 'alarm') + ' mono">' + cover + '%</span>')
    + kv('总费用', '<b class="mono">' + fmtUSD(costs.cost_micro_usd) + '</b>')
    + kv('约合人民币', cny)
    + '</div><p class="note" style="margin-top:10px">未命中价格的请求不计费用；汇率来源 '
    + esc(rate ? rate.source + (rate.fallback ? '（兜底）' : '') : '未知') + '。</p>';
}
async function loadRequests() {
  const q = new URLSearchParams({
    limit: String(reqView.size), offset: String(reqView.page * reqView.size),
    sort: reqView.sort, order: reqView.order, ...rangeParams(),
  });
  if (reqView.model) q.set('model', reqView.model);
  if (reqView.keyId) q.set('key_id', reqView.keyId);
  if (reqView.result) q.set('result', reqView.result);
  const r = await api('/requests?' + q);
  const items = r.items || [], total = r.total || 0;
  const pages = Math.max(1, Math.ceil(total / reqView.size));
  const cols = activeReqCols();
  $('req-count').textContent = '共 ' + fmtInt(total) + ' 条 · 第 ' + (reqView.page + 1) + ' / ' + pages + ' 页';
  // 表头随列偏好重建；只有后端支持的排序键才带 .sort
  $('req-head').innerHTML = cols.map(c => '<th' + (c.num ? ' class="num' + (c.sort ? ' sort' : '') + '"'
    : (c.sort ? ' class="sort"' : '')) + (c.sort ? ' data-sort="' + c.sort + '"' : '')
    + (c.sort && reqView.sort === c.sort ? ' data-dir="' + reqView.order + '"' : '')
    + '>' + (c.tip ? labelWithTip(c.label, c.tip) : esc(c.label)) + '</th>').join('');
  $('req-rows').innerHTML = items.map(x =>
    '<tr class="row" data-id="' + esc(x.id) + '">' + cols.map(c => c.cell(x)).join('') + '</tr>').join('')
    || '<tr><td colspan="' + cols.length + '"><div class="empty"><p class="empty-title">没有匹配的请求</p>'
    + '<p class="empty-hint">调整筛选条件或时间范围</p></div></td></tr>';
  $('req-rows').dataset.items = JSON.stringify(items);
  // 分页：75 页时只有上/下一页不够，补每页条数与跳页
  $('req-pager').innerHTML = '<span class="jump">每页'
    + REQ_SIZES.map(s => '<button type="button" class="btn small" data-size="' + s + '"'
      + (s === reqView.size ? ' disabled' : '') + '>' + s + '</button>').join('') + '</span>'
    + '<span class="grow"></span>'
    + '<button type="button" class="btn small" id="req-prev"' + (reqView.page <= 0 ? ' disabled' : '') + '>上一页</button>'
    + '<span class="jump"><input type="number" id="req-jump" min="1" max="' + pages + '" value="' + (reqView.page + 1)
    + '" aria-label="跳转到页码"><span class="mono">/ ' + pages + '</span></span>'
    + '<button type="button" class="btn small" id="req-next"'
    + ((reqView.page + 1) * reqView.size >= total ? ' disabled' : '') + '>下一页</button>';
  const go = () => loadRequests().catch(e => toast(e.message, 'err'));
  const prev = $('req-prev'), next = $('req-next'), jump = $('req-jump');
  if (prev) prev.onclick = () => { reqView.page--; go(); };
  if (next) next.onclick = () => { reqView.page++; go(); };
  $('req-pager').querySelectorAll('[data-size]').forEach(b => b.onclick = () => {
    reqView.size = +b.dataset.size;
    localStorage.setItem('req-size', String(reqView.size));
    reqView.page = 0;
    go();
  });
  if (jump) {
    const apply = () => {
      const p = Math.max(1, Math.min(pages, parseInt(jump.value, 10) || 1));
      if (p - 1 === reqView.page) { jump.value = String(p); return; }
      reqView.page = p - 1;
      go();
    };
    jump.onchange = apply;
    jump.onkeydown = e => { if (e.key === 'Enter') { e.preventDefault(); apply(); } };
  }
}
$('req-table').querySelector('thead').addEventListener('click', e => {
  const th = e.target.closest('th.sort');
  if (!th) return;
  const keyName = th.dataset.sort;
  if (reqView.sort === keyName) reqView.order = reqView.order === 'desc' ? 'asc' : 'desc';
  else { reqView.sort = keyName; reqView.order = 'desc'; }
  reqView.page = 0;
  loadRequests().catch(err => toast(err.message, 'err'));
});
$('req-rows').addEventListener('click', e => {
  const tr = e.target.closest('tr.row');
  if (!tr) return;
  let items = [];
  try { items = JSON.parse($('req-rows').dataset.items || '[]'); } catch (_) { /* 忽略 */ }
  const x = items.find(i => i.id === tr.dataset.id);
  if (!x) return;
  openSheet({
    title: '请求明细 · ' + fmtDT(x.ts, true),
    okText: '关闭', noFocus: true,
    body: '<div class="detail-facts">'
      + fact('模型', x.model || '-') + fact('提供方', x.provider || '-')
      + fact('来源', x.source || '-') + fact('结果', x.result)
      + fact('密钥', x.key_id ? (keyLabelOf(x.key_id) || x.key_id) : '-') + fact('caller', x.caller_id || '-')
      + fact('认证账号', x.auth_label || x.auth_id || '-') + fact('认证类型', x.auth_type || '-')
      + fact('档位', x.tier || '-') + fact('思考强度', x.thinking_intensity || '-')
      + fact('输入 Token', fmtTok(x.input_tokens)) + fact('输出 Token', fmtTok(x.output_tokens))
      + fact('推理 Token', fmtTok(x.reasoning_tokens)) + fact('缓存读', fmtTok(cacheReadOf(x)))
      + fact('缓存写', fmtTok(x.cache_creation_tokens)) + fact('总 Token', fmtTok(effTokens(x)))
      + (!(+x.input_tokens || 0) && !(+x.output_tokens || 0) && (+x.cost_micro_usd || 0) > 0
        ? fact('用量捕获', '上游未返回用量，费用按预占估算扣费') : '')
      + fact('首字延迟', fmtSec(x.ttft_ms))
      + fact('生成耗时', fmtSec(x.generation_ms))
      + fact('TPS', fmtTPS(x.tps_milli))
      + fact('总延迟', fmtSec(x.latency_ms))
      + fact('费用', fmtUSD(x.cost_micro_usd)) + fact('命中计价', x.priced ? '是' : '否')
      + (x.reservation_id ? fact('预占 ID', x.reservation_id) : '')
      + '</div>',
  });
});
const reqFilterChanged = () => {
  reqView.model = reqModelCombo.value;
  reqView.keyId = reqKeyCombo.value;
  reqView.result = reqResultSel.value;
  reqView.page = 0;
  loadRequests().catch(e => toast(e.message, 'err'));
};
const reqModelCombo = new Combo('req-model', reqFilterChanged);
const reqKeyCombo = new Combo('req-key', reqFilterChanged);
const reqResultSel = new Select('req-result', [
  { value: '', label: '全部结果' },
  { value: 'ok', label: '成功 ok' },
  { value: 'error', label: '失败 error' },
], reqFilterChanged, { value: '', head: '按结果过滤' });
const dimSel = new Select('dim', DIMS, () => loadDim().catch(e => toast(e.message, 'err')),
  { value: 'model', head: '聚合维度' });
// 列偏好：默认列收进视口，低频列按需开启（偏好存 localStorage）
new MultiSelect('req-cols', REQ_COLS.map(c => ({ value: c.id, label: c.label, fixed: c.fixed })),
  reqCols, sel => {
    reqCols = new Set(sel);
    localStorage.setItem('req-cols', JSON.stringify([...sel]));
    loadRequests().catch(e => toast(e.message, 'err'));
  }, { text: '列', head: '显示列', defaults: REQ_COLS_DEFAULT });
$('req-export').addEventListener('click', async () => {
  try {
    const r = await api('/export/csv', {
      method: 'POST',
      body: JSON.stringify({
        kind: 'requests', limit: 50000,
        filter: Object.assign({ model: reqView.model, key_id: reqView.keyId, result: reqView.result }, rangeParams()),
      }),
    });
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'cpa-usage-manager-requests.csv';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('CSV 已导出', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});

// ---------- 价格 ----------
const pricingCache = { items: [] };
let pricingPage = 0;
const PRICING_PAGE_SIZE = 10;
loaders.pricing = async () => {
  const [r, fx] = await Promise.all([api('/pricing'), api('/exchange-rate')]);
  S.fx = fx;
  $('fx-info').textContent = fx && fx.usd_to_cny_micro
    ? 'USD→CNY ' + (fx.usd_to_cny_micro / 1e6).toFixed(4) + ' · ' + fx.source + (fx.fallback ? '（兜底）' : '') : '';
  pricingCache.items = (r.items || []).slice().sort((a, b) => b.priority - a.priority || a.id - b.id);
  pricingPage = 0;
  renderPricing();
  stamp();
};
function renderPricing() {
  const items = pricingCache.items;
  // 表头带 ⓘ 说明：「缓存读/缓存写」这类术语中文里不自明，悬浮给出上游口径解释
  $('pricing-head').innerHTML =
    '<th class="num">' + labelWithTip('优先级', TIPS.priority) + '</th>'
    + '<th>' + labelWithTip('匹配', TIPS.matchKind) + '</th>'
    + '<th class="w-grow">模式</th><th>状态</th>'
    + '<th class="num">' + labelWithTip('输入', TIPS.input) + '</th>'
    + '<th class="num">' + labelWithTip('输出', TIPS.output) + '</th>'
    + '<th class="num">' + labelWithTip('缓存读', TIPS.cacheRead) + '</th>'
    + '<th class="num">' + labelWithTip('缓存写', TIPS.cacheWrite) + '</th>'
    + '<th>来源</th><th class="w-act"></th>';
  const pages = Math.max(1, Math.ceil(items.length / PRICING_PAGE_SIZE));
  const rows = items.slice(pricingPage * PRICING_PAGE_SIZE, (pricingPage + 1) * PRICING_PAGE_SIZE);
  $('pricing-rows').innerHTML = rows.map(p => '<tr>'
    + '<td class="num cell-mono">' + p.priority + '</td>'
    + '<td><span class="pill signal mono">' + esc(p.match_kind) + '</span></td>'
    + '<td class="cell-mono w-grow" title="' + esc(p.pattern) + '">' + esc(p.pattern) + '</td>'
    + '<td><span class="pill ' + (p.enabled ? 'live' : '') + '">' + (p.enabled ? '启用' : '停用') + '</span></td>'
    + '<td class="num">' + fmtPrice(p.price_input) + '</td>'
    + '<td class="num">' + fmtPrice(p.price_output) + '</td>'
    + '<td class="num">' + fmtPrice(p.price_cache_read) + '</td>'
    + '<td class="num">' + fmtPrice(p.price_cache_creation) + '</td>'
    + '<td class="cell-dim">' + esc(p.source === 'models_dev' ? 'models.dev' : '手动') + '</td>'
    + '<td class="w-act"><button type="button" class="btn small" data-edit="' + p.id + '">编辑</button>'
    + '<button type="button" class="btn small danger" data-id="' + p.id + '">删除</button></td></tr>').join('')
    || '<tr><td colspan="10"><div class="empty"><p class="empty-title">还没有计价规则</p>'
    + '<p class="empty-hint">新增规则，或在上方搜索 models.dev 后按条添加</p></div></td></tr>';
  $('pricing-pager').innerHTML = pages > 1
    ? '<span class="mono">第 ' + (pricingPage + 1) + ' / ' + pages + ' 页 · 共 ' + fmtInt(items.length) + ' 条</span><span class="grow"></span>'
      + '<button type="button" class="btn small" id="pricing-prev"' + (pricingPage <= 0 ? ' disabled' : '') + '>上一页</button>'
      + '<button type="button" class="btn small" id="pricing-next"'
      + ((pricingPage + 1) * PRICING_PAGE_SIZE >= items.length ? ' disabled' : '') + '>下一页</button>'
    : '';
  const prev = $('pricing-prev'), next = $('pricing-next');
  if (prev) prev.onclick = () => { pricingPage--; renderPricing(); };
  if (next) next.onclick = () => { pricingPage++; renderPricing(); };
}
$('pricing-rows').addEventListener('click', e => {
  const del = e.target.closest('button[data-id]');
  if (!del) return;
  confirmSheet('删除计价规则 #' + del.dataset.id,
    '删除后相关模型将回落到兜底规则（通常免费）。',
    async () => {
      await post('/pricing/delete', { id: parseInt(del.dataset.id, 10), actor: 'console' });
      loaders.pricing().catch(() => {});
    });
});
// micro-USD/百万 token → 弹窗里的 $/M 数值文本。
const priceInputVal = v => String((Number(v) || 0) / 1e6);
function pricingFormBody(p) {
  const sel = (kind, cur) => '<option value="' + kind + '"' + (kind === cur ? ' selected' : '') + '>' + kind + '</option>';
  const kindSel = p
    ? ['exact', 'glob', 'regexp'].map(k => sel(k, p.match_kind)).join('')
    : '<option value="exact">exact 完全匹配</option><option value="glob" selected>glob 通配</option><option value="regexp">regexp 正则</option>';
  // 只保留真正参与计算的四档：推理并入输出、cached 并入缓存读，独立档位无处可用。
  return '<div class="form-grid">'
    + fieldRow(labelWithTip('匹配方式', TIPS.matchKind), '<select id="p-kind">' + kindSel + '</select>')
    + fieldRow(labelWithTip('优先级', TIPS.priority), '<input id="p-priority" type="number" value="' + (p ? p.priority : 100) + '">')
    + fieldRow('模式', '<input id="p-pattern" value="' + (p ? esc(p.pattern) : '') + '" placeholder="如 gpt-* 或 claude-sonnet-4" spellcheck="false">')
    + fieldRow('状态', '<select id="p-enabled"><option value="true"' + (!p || p.enabled ? ' selected' : '') + '>启用</option>'
      + '<option value="false"' + (p && !p.enabled ? ' selected' : '') + '>停用</option></select>')
    + '<div class="form-sep wide">单价（每百万 token 美元）</div>'
    + fieldRow(labelWithTip('输入', TIPS.input), '<input id="p-in" inputmode="decimal" value="' + (p ? priceInputVal(p.price_input) : '') + '" placeholder="0">')
    + fieldRow(labelWithTip('输出', TIPS.output), '<input id="p-out" inputmode="decimal" value="' + (p ? priceInputVal(p.price_output) : '') + '" placeholder="0">')
    + fieldRow(labelWithTip('缓存读', TIPS.cacheRead), '<input id="p-cache-read" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cache_read) : '') + '" placeholder="0">')
    + fieldRow(labelWithTip('缓存写', TIPS.cacheWrite), '<input id="p-cache-create" inputmode="decimal" value="' + (p ? priceInputVal(p.price_cache_creation) : '') + '" placeholder="0">')
    + '</div>';
}
function pricingSubmit() {
  const num = id => Math.round((parseFloat($(id).value) || 0) * 1e6);
  return {
    match_kind: $('p-kind').value,
    pattern: $('p-pattern').value.trim() || '*',
    priority: parseInt($('p-priority').value, 10) || 0,
    enabled: $('p-enabled').value === 'true',
    price_input: num('p-in'), price_output: num('p-out'),
    price_cache_read: num('p-cache-read'), price_cache_creation: num('p-cache-create'),
    accounting_mode: 'default', billing_mode: 'token', per_image_micro_usd: 0,
    source: 'manual',
  };
}
$('pricing-add').addEventListener('click', () => {
  openSheet({
    title: '新增计价规则', okText: '保存',
    body: pricingFormBody(null),
    note: '单价为每百万 Token 的美元金额；同匹配方式同模式重复添加将覆盖原规则。',
    onOk: async () => {
      await post('/pricing', pricingSubmit());
      toast('规则已保存', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
$('pricing-rows').addEventListener('click', e => {
  const b = e.target.closest('button[data-edit]');
  if (!b) return;
  const p = pricingCache.items.find(x => x.id === parseInt(b.dataset.edit, 10));
  if (!p) return;
  openSheet({
    title: '编辑计价规则 #' + p.id, okText: '保存',
    body: pricingFormBody(p),
    note: '修改匹配方式或模式会按新键生效；若新键已存在，两条将合并为一条。计价口径与来源保持原值。',
    onOk: async () => {
      const body = pricingSubmit();
      body.id = p.id;
      // 口径字段不在表单里，保留原值避免把按张计价 / models.dev 规则改坏。
      body.accounting_mode = p.accounting_mode;
      body.billing_mode = p.billing_mode;
      body.per_image_micro_usd = p.per_image_micro_usd;
      body.source = p.source;
      body.models_dev_id = p.models_dev_id;
      await post('/pricing', body);
      toast('规则已更新', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
// models.dev 搜索后按条添加：目录在服务端缓存 10 分钟，避免整本同步的瞬时 IO。
async function pricingSearchRun() {
  const q = $('pricing-search-input').value.trim();
  if (!q) { toast('先输入模型关键词', 'err'); return; }
  const box = $('pricing-search-results');
  box.hidden = false;
  box.innerHTML = pricingSearchHead() + '<p class="note" style="padding:10px 14px">正在搜索 models.dev…</p>';
  try {
    const list = await api('/pricing/search?' + new URLSearchParams({ q, limit: '20' }));
    box.dataset.items = JSON.stringify(list);
    box.innerHTML = pricingSearchHead()
      + (list.length
        ? '<div class="search-list">' + list.map((c, i) =>
          '<div class="search-item"><div class="si-main">'
          + '<span class="si-name">' + esc(c.name || c.model_id) + '</span>'
          + '<span class="si-id mono">' + esc(c.provider_id) + '/' + esc(c.model_id) + '</span></div>'
          + '<span class="si-price mono">入 ' + fmtPrice(c.price_input) + ' · 出 ' + fmtPrice(c.price_output) + '</span>'
          + '<button type="button" class="btn small primary" data-si="' + i + '">添加</button></div>').join('') + '</div>'
        : '<p class="note" style="padding:10px 14px">没有匹配的模型，换个关键词试试。</p>');
  } catch (e) { box.innerHTML = pricingSearchHead() + '<p class="note" style="padding:10px 14px">' + esc(e.message) + '</p>'; }
}
// pricingSearchHead 搜索结果面板的标题栏，带关闭按钮（结果面板本身没有原生收起入口）。
function pricingSearchHead() {
  return '<div class="search-head"><span>models.dev 搜索结果</span>'
    + '<button type="button" class="btn small" data-search-close>关闭</button></div>';
}
$('pricing-search-results').addEventListener('click', e => {
  if (!e.target.closest('[data-search-close]')) return;
  $('pricing-search-results').hidden = true;
});
$('pricing-search-btn').addEventListener('click', pricingSearchRun);
$('pricing-search-input').addEventListener('keydown', e => {
  if (e.key === 'Enter') { e.preventDefault(); pricingSearchRun(); }
});
$('pricing-reset').addEventListener('click', () => {
  openSheet({
    title: '清空计价规则',
    okText: '清空', danger: true,
    body: '<p>将删除全部自定义计价规则，只保留全模型免费兜底规则（glob:*）。'
      + '密钥额度与用量数据不受影响。此操作不可撤销，确定继续吗？</p>',
    onOk: async () => {
      const r = await post('/pricing/reset', { actor: 'console' });
      toast('已清空 ' + (r.deleted || 0) + ' 条计价规则', 'ok');
      loaders.pricing().catch(() => {});
    },
  });
});
$('pricing-search-results').addEventListener('click', async e => {
  const b = e.target.closest('button[data-si]');
  if (!b) return;
  let list = [];
  try { list = JSON.parse($('pricing-search-results').dataset.items || '[]'); } catch (_) { /* 忽略 */ }
  const c = list[+b.dataset.si];
  if (!c) return;
  b.disabled = true;
  b.textContent = '已添加';
  try {
    await post('/pricing', {
      match_kind: 'exact', pattern: c.pattern, priority: 100, enabled: true,
      price_input: c.price_input, price_output: c.price_output,
      price_cache_read: c.price_cache_read, price_cache_creation: c.price_cache_creation,
      accounting_mode: 'default', billing_mode: 'token', per_image_micro_usd: 0,
      source: c.source, models_dev_id: c.models_dev_id,
    });
    toast('已添加 ' + c.model_id, 'ok');
    loaders.pricing().catch(() => {});
  } catch (err) {
    b.disabled = false;
    b.textContent = '添加';
    toast(err.message, 'err');
  }
});
$('fx-refresh').addEventListener('click', async () => {
  try {
    S.fx = await post('/exchange-rate');
    toast('汇率已刷新', 'ok');
    loaders.pricing().catch(() => {});
  } catch (e) { toast(e.message, 'err'); }
});

// ---------- 模型集合（路由别名） ----------
// 集合别名 = 一条规则脚本 + 有序目标链；保存期服务端编译校验并回填 refs。
const routeCache = { items: [], judge: { model: '', timeout_ms: 8000 }, modelsLoaded: false };
const TIPS_routeRule = 'when 条件 -> 候选链；候选链为 "模型"、priority ["a","b"]（声明序即回退序）'
  + '或 weighted {"a":3,"b":1}（加权随机，选中者排首）。最后必须有一条无条件兜底分支（-> ...）。\n'
  + '变量：input_tokens / body_len / model / stream / thinking_effort / source。'
  + 'ai_judge(["simple","hard"]) 返回其中一项，可与 == 连用做智能分级（需先配置评判模型）。\n'
  + '# 开头是注释。';

loaders.routes = async () => {
  const r = await api('/model-routes');
  routeCache.items = r.items || [];
  routeCache.judge = r.judge || routeCache.judge;
  renderRouteJudgeState();
  renderRouteCards();
  stamp();
};
function renderRouteJudgeState() {
  const badge = $('route-judge-state');
  if (badge) badge.hidden = !!routeCache.judge.model;
}
function routeCard(r) {
  const refs = (r.refs || []).map(x => '<span class="rt-ref">' + esc(x) + '</span>').join('')
    || '<span class="note">规则未引用任何目标</span>';
  const rulePreview = esc(r.rule || '');
  const modeBadge = r.pricing_mode === 'alias' ? '<span class="pill trace">按别名声价</span>' : '<span class="pill signal">按目标计价</span>';
  return '<div class="rt-card' + (r.enabled ? '' : ' off') + '" role="listitem" data-id="' + r.id + '">'
    + '<div class="rt-top"><span class="rt-alias">' + esc(r.alias) + '</span>'
    + '<span class="pill ' + (r.enabled ? 'live' : '') + '">' + (r.enabled ? '启用' : '停用') + '</span></div>'
    + '<div class="rt-meta">' + modeBadge
    + '<span>冷却 <b>' + (r.cooldown_seconds || 0) + 's</b></span></div>'
    + '<pre class="rt-rule" title="规则脚本">' + rulePreview + '</pre>'
    + '<div class="rt-chips">' + refs + '</div>'
    + '<div class="rt-acts">'
    + '<button type="button" class="btn small" data-route-toggle="' + r.id + '">' + (r.enabled ? '停用' : '启用') + '</button>'
    + '<button type="button" class="btn small" data-route-edit="' + r.id + '">编辑</button>'
    + '<button type="button" class="btn small danger" data-route-del="' + r.id + '">删除</button>'
    + '</div></div>';
}
function renderRouteCards() {
  const items = routeCache.items;
  $('route-rows').innerHTML = items.length
    ? items.map(routeCard).join('')
    : '<div class="empty"><p class="empty-title">还没有模型集合</p>'
      + '<p class="empty-hint">新建集合后，插件 Key 请求别名即按规则路由到健康目标，失败自动转移。</p></div>';
}
async function loadRouteModelList() {
  if (routeCache.modelsLoaded) return;
  try {
    const r = await api('/usage/dimension?' + new URLSearchParams({ dimension: 'model', limit: '200' }));
    $('route-model-list').innerHTML = (r.rows || []).filter(x => x.value)
      .map(x => '<option value="' + esc(x.value) + '">').join('');
    routeCache.modelsLoaded = true;
  } catch (_) { /* 候选列表只是辅助，失败不打断编辑 */ }
}
function routeFormBody(r) {
  return '<div class="form-grid">'
    + fieldRow('别名', '<input id="rt-alias" list="route-model-list" value="' + (r ? esc(r.alias) : '') + '" placeholder="如 auto 或 grp/name（可含 /，撞真实模型名会被拒绝）" spellcheck="false" maxlength="128">')
    + fieldRow('状态', '<select id="rt-enabled"><option value="true"' + (!r || r.enabled ? ' selected' : '') + '>启用</option>'
      + '<option value="false"' + (r && !r.enabled ? ' selected' : '') + '>停用</option></select>')
    + fieldRow('计价模式', '<select id="rt-mode"><option value="target"' + (!r || r.pricing_mode !== 'alias' ? ' selected' : '') + '>按实际目标计价</option>'
      + '<option value="alias"' + (r && r.pricing_mode === 'alias' ? ' selected' : '') + '>按别名自身计价</option></select>')
    + fieldRow(labelWithTip('冷却秒数', '目标失败后的进程内冷却时长；冷却期内该目标被跳过，到期自动恢复。0 为不冷却。'),
      '<input id="rt-cooldown" type="number" min="0" max="86400" value="' + (r ? (r.cooldown_seconds || 0) : 60) + '">')
    + '</div>'
    + fieldRow(labelWithTip('规则脚本', TIPS_routeRule),
      '<textarea id="rt-rule" class="mono-area" spellcheck="false" placeholder=\'-> "gpt-4o-mini"\'>'
      + esc(r ? r.rule : '') + '</textarea>');
}
function collectRouteForm() {
  return {
    alias: $('rt-alias').value.trim(),
    enabled: $('rt-enabled').value === 'true',
    pricing_mode: $('rt-mode').value,
    cooldown_seconds: Math.max(0, Math.min(86400, parseInt($('rt-cooldown').value, 10) || 0)),
    rule: $('rt-rule').value,
  };
}
$('route-add').addEventListener('click', () => {
  loadRouteModelList();
  openSheet({
    title: '新建模型集合', okText: '保存',
    body: routeFormBody(null),
    note: TIPS_routeRule,
    onOk: async () => {
      const body = collectRouteForm();
      const res = await post('/model-routes/save', { ...body, actor: 'console' });
      if (res.warning) toast(res.warning, 'err');
      toast('集合已创建', 'ok');
      loaders.routes().catch(() => {});
    },
  });
});
$('route-rows').addEventListener('click', e => {
  const editBtn = e.target.closest('button[data-route-edit]');
  if (editBtn) {
    const r = routeCache.items.find(x => x.id === parseInt(editBtn.dataset.routeEdit, 10));
    if (!r) return;
    loadRouteModelList();
    openSheet({
      title: '编辑集合 · ' + r.alias, okText: '保存',
      body: routeFormBody(r),
      note: TIPS_routeRule,
      onOk: async () => {
        const body = { id: r.id, actor: 'console', ...collectRouteForm() };
        const res = await post('/model-routes/save', body);
        if (res.warning) toast(res.warning, 'err');
        toast('集合已更新', 'ok');
        loaders.routes().catch(() => {});
      },
    });
    return;
  }
  const delBtn = e.target.closest('button[data-route-del]');
  if (delBtn) {
    const id = parseInt(delBtn.dataset.routeDel, 10);
    const r = routeCache.items.find(x => x.id === id);
    confirmSheet('删除集合 · ' + (r ? r.alias : '#' + id),
      '删除后请求该别名将不再被接管（宿主按未知模型报错）。历史统计不受影响。',
      async () => {
        await post('/model-routes/delete', { id, actor: 'console' });
        loaders.routes().catch(() => {});
      });
    return;
  }
  const toggleBtn = e.target.closest('button[data-route-toggle]');
  if (toggleBtn) {
    const id = parseInt(toggleBtn.dataset.routeToggle, 10);
    const r = routeCache.items.find(x => x.id === id);
    if (!r) return;
    // 快捷开关复用 save 端点全量提交，避免额外端点。
    post('/model-routes/save', {
      id: r.id, alias: r.alias, rule: r.rule,
      cooldown_seconds: r.cooldown_seconds, pricing_mode: r.pricing_mode,
      enabled: !r.enabled, actor: 'console',
    }).then(() => loaders.routes().catch(() => {}))
      .catch(err => toast(err.message, 'err'));
  }
});
$('route-judge-btn').addEventListener('click', () => {
  const j = routeCache.judge || {};
  openSheet({
    title: 'AI 评判设置', okText: '保存',
    body: '<div class="form-grid">'
      + fieldRow(labelWithTip('评判模型', '执行 ai_judge 时调用的模型名，经宿主正常转发与计费；留空表示未配置，含 ai_judge 的规则将无法保存或回落兜底分支。'),
        '<input id="jd-model" value="' + esc(j.model || '') + '" placeholder="如 gpt-4o-mini" spellcheck="false">')
      + fieldRow(labelWithTip('超时（毫秒）', 'ai_judge 在转发前同步执行，最长等待此时长；超时即回落兜底分支。500~120000。'),
        '<input id="jd-timeout" type="number" min="500" max="120000" step="100" value="' + (j.timeout_ms || 8000) + '">')
      + '</div>',
    note: '发送给评判模型的是脱敏摘要：结构化指标加对话文本前 2000 字符，绝不发送完整请求体。同一输入组合的结论缓存 10 分钟。',
    onOk: async () => {
      const model = $('jd-model').value.trim();
      const timeout_ms = Math.max(500, Math.min(120000, parseInt($('jd-timeout').value, 10) || 8000));
      const saved = await post('/model-routes/judge', { model, timeout_ms });
      routeCache.judge = saved && saved.model !== undefined ? saved : { model, timeout_ms };
      renderRouteJudgeState();
      toast('评判设置已保存', 'ok');
    },
  });
});

// ---------- 认证额度 ----------
loaders.auth = async () => {
  const r = await api('/auth-quotas');
  const items = r.items || [];
  $('auth-body').innerHTML = items.length
    ? '<div class="auth-grid">' + items.map(a => {
      const pill = a.status === 'ok' ? 'live' : a.status === 'error' ? 'alarm' : '';
      const snap = a.snapshot ? Object.entries(a.snapshot)
        .filter(([, v]) => v !== null && typeof v !== 'object')
        .map(([k, v]) => kv(esc(k), esc(String(v)))).join('') : '';
      const wins = (a.windows || []).map(w => '<div class="kv-row"><dt>'
        + '<span class="pill mono">' + esc(w.window_id) + '</span></dt>'
        + '<dd>观测 ' + fmtInt(w.observed) + ' · 增量 ' + fmtInt(w.delta) + '</dd></div>').join('');
      return '<div class="auth-card"><div class="auth-card-top">'
        + '<span class="pill ' + pill + '">' + esc(a.status || 'unknown') + '</span>'
        + '<div class="who"><span class="prov">' + esc(a.provider) + '</span>'
        + '<span class="aid">' + esc(a.auth_id) + '</span></div></div>'
        + (snap ? '<div class="kv">' + snap + '</div>' : '')
        + (wins ? '<div class="kv">' + wins + '</div>' : '')
        + '<p class="note">抓取于 ' + (a.fetched_at ? fmtDT(a.fetched_at, true) : '-') + '</p></div>';
    }).join('') + '</div>'
    : '<div class="empty"><p class="empty-title">暂无上游认证额度</p>'
    + '<p class="empty-hint">宿主侧产生 OAuth 用量观测后会出现在这里</p></div>';
  stamp();
};

// ---------- 系统 ----------
loaders.system = async () => {
  const h = await api('/health');
  const s = h.stats || {};
  S.stats = s;
  $('sys-readouts').innerHTML =
    readout('数据库文件', fmtBytes(s.file_bytes),
      'schema v' + s.schema_version + ' · ' + (s.writable ? '可写' : '只读'), !s.writable)
    + readout('请求明细', fmtInt(s.requests), '逐请求记录')
    + readout('分钟聚合', fmtInt(s.rollups), '趋势与维度查询的数据源')
    + readout('在途预占', fmtInt(s.held_reservations), '未结算的额度预占', s.held_reservations > 0)
    + readout('密钥', fmtInt(s.keys), '含已撤销 / 过期')
    + readout('Caller', fmtInt(s.callers), '归属记录')
    + readout('计价规则', fmtInt(s.pricing_rules), 'manual + models.dev')
    + readout('存储重试', fmtInt(s.io_retries || 0),
      s.io_retries > 0 ? '瞬时 I/O 故障已自动重试；持续增长请把 data_dir 移出杀毒/同步盘' : '本次运行未出现瞬时 I/O 故障',
      s.io_retries > 0);
  $('db-note').textContent = s.writable
    ? '备份为单文件 SQLite 快照；恢复前服务端会做一致性检查。'
    : '当前实例处于只读模式（可能存在跨进程写者），备份可用，恢复不可用。';
  await loadNotify();
  await loadReports();
  updateBadges();
  stamp();
};
$('backup-btn').addEventListener('click', async () => {
  try {
    const r = await api('/backup');
    const blob = await r.blob();
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = 'cpa-usage-manager-backup.db';
    a.click();
    URL.revokeObjectURL(a.href);
    toast('备份已下载（' + fmtBytes(blob.size) + '）', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});
$('restore-file').addEventListener('change', () => {
  const f = $('restore-file').files[0];
  $('restore-name').textContent = f ? f.name + '（' + fmtBytes(f.size) + '）' : '选择备份文件…';
});
$('restore-btn').addEventListener('click', () => {
  const f = $('restore-file').files[0];
  if (!f) { toast('请先选择备份文件', 'err'); return; }
  openSheet({
    title: '整库恢复', danger: true, okText: '确认恢复',
    body: '<p>将用 <b>' + esc(f.name) + '</b> 替换当前数据库的全部内容。'
      + '若备份来自其他机器，必须同时迁移 <span class="mono">data_dir/key-peppers</span>，否则密文无法解密。</p>',
    onOk: async () => {
      const res = await fetch(API + '/restore?actor=console', {
        method: 'POST',
        headers: { Authorization: 'Bearer ' + key, 'X-Confirm-Restore': 'replace' },
        body: f,
      });
      if (!res.ok) {
        let msg = 'HTTP ' + res.status;
        try { const j = await res.json(); if (j.error) msg = j.error; } catch (_) { /* 忽略 */ }
        throw new Error(msg);
      }
      const j = await res.json();
      const rows = j.tables ? Object.values(j.tables).reduce((a, b) => a + b, 0) : 0;
      toast('恢复完成：' + fmtBytes(j.bytes || f.size) + ' · ' + rows + ' 行', 'ok');
      loaders.system().catch(() => {});
    },
  });
});
function maintainRun(vacuum) {
  return async () => {
    try {
      const r = await post('/maintain', { vacuum, actor: 'console' });
      const g = k => r[k] !== undefined ? r[k] : r[k.charAt(0).toUpperCase() + k.slice(1)];
      $('maintain-note').textContent = '上次结果：清理请求 ' + fmtInt(g('requests'))
        + ' · 聚合 ' + fmtInt(g('rollups')) + ' · 预占 ' + fmtInt(g('reservations'))
        + ' · 合并重复 ' + fmtInt(g('deduped'))
        + (vacuum ? ' · 已 VACUUM' : '');
      toast(vacuum ? '清理并 VACUUM 完成' : '清理完成', 'ok');
      loaders.system().catch(() => {});
    } catch (e) { toast(e.message, 'err'); }
  };
}
$('maintain-btn').addEventListener('click', maintainRun(false));
$('vacuum-btn').addEventListener('click', maintainRun(true));
$('dedupe-btn').addEventListener('click', async () => {
  const btn = $('dedupe-btn');
  btn.disabled = true;
  try {
    const r = await post('/dedupe', { actor: 'console' });
    const n = r.merged || 0;
    $('maintain-note').textContent = n > 0
      ? '上次对账：合并了 ' + fmtInt(n) + ' 条重复请求行，费用口径不变（保留执行器行的结算金额）。'
      : '上次对账：未发现重复请求行。';
    toast(n > 0 ? '对账完成：合并 ' + fmtInt(n) + ' 条' : '对账完成：无重复', 'ok');
    loaders.system().catch(() => {});
  } catch (e) { toast(e.message, 'err'); } finally { btn.disabled = false; }
});
$('reset-confirm').addEventListener('input', () => {
  $('reset-btn').disabled = $('reset-confirm').value !== 'reset';
});
$('reset-btn').addEventListener('click', () => {
  openSheet({
    title: '重置统计', danger: true, okText: '确认重置',
    body: '<p>将清空逐请求明细、分钟聚合、已终结预占与密钥周期计数器。'
      + '<b>密钥与计价规则保留</b>。</p>',
    onOk: async () => {
      const r = await post('/reset', { confirm: 'reset', actor: 'console' });
      toast('重置完成：请求 ' + fmtInt(r.requests) + ' · 聚合 ' + fmtInt(r.rollups), 'ok');
      $('reset-confirm').value = '';
      $('reset-btn').disabled = true;
      loaders.system().catch(() => {});
    },
  });
});

// ---------- 通知（shoutrrr 多端点） ----------
let notifyCache = { settings: null, endpoints: [] };
async function loadNotify() {
  const r = await api('/notify');
  notifyCache = { settings: r.settings || null, endpoints: r.endpoints || [] };
  renderNotify();
}
function renderNotify() {
  const st = notifyCache.settings || {};
  $('nt-enabled').checked = !!st.enabled;
  $('nt-errors').checked = !!st.error_alerts;
  $('nt-warn').value = st.warn_pct ?? 20;
  const eps = notifyCache.endpoints;
  if (!eps.length) {
    $('nt-list').innerHTML = '<p class="note">尚未配置通知端点。</p>';
    return;
  }
  $('nt-list').innerHTML = eps.map(e => {
    const scheme = (String(e.url).split('://')[0] || '?').toLowerCase();
    const status = e.last_error
      ? '<span class="nt-err">✗ ' + esc(e.last_error) + '</span>'
      : e.last_ok_at
        ? '<span class="nt-ok">✓ 上次发送成功 · ' + esc(fmtDT(e.last_ok_at)) + '</span>'
        : '<span class="nt-dim">从未发送</span>';
    return '<div class="nt-item' + (e.enabled ? '' : ' off') + '">'
      + '<span class="nt-scheme">' + esc(scheme) + '</span>'
      + '<div class="nt-main">'
      + '<div class="nt-head"><b>' + esc(e.label || '未命名端点') + '</b>'
      + (e.enabled ? '' : ' <span class="pill">停用</span>') + '</div>'
      + '<div class="nt-url mono" title="' + esc(e.url) + '">' + esc(e.url) + '</div>'
      + '<div class="nt-status">' + status + '</div>'
      + '</div>'
      + '<div class="btn-row">'
      + '<button type="button" class="btn" data-nt-test="' + e.id + '">测试</button>'
      + '<button type="button" class="btn" data-nt-edit="' + e.id + '">编辑</button>'
      + '<button type="button" class="btn danger" data-nt-del="' + e.id + '">删除</button>'
      + '</div></div>';
  }).join('');
}
function openEndpointSheet(ep) {
  openSheet({
    title: ep ? '编辑通知端点' : '新增通知端点',
    okText: ep ? '保存' : '添加',
    body:
      fieldRow('标签', '<input id="f-nt-label" placeholder="如：飞书值班群" value="' + esc(ep ? ep.label : '') + '">')
      + fieldRow('shoutrrr URL',
        '<textarea id="f-nt-url" rows="3" spellcheck="false" autocomplete="off" '
        + 'placeholder="telegram://… / discord://… / lark://… / generic://…"'
        + '>' + esc(ep ? ep.url : '') + '</textarea>')
      + '<label class="check-row"><input type="checkbox" id="f-nt-enabled"'
      + (!ep || ep.enabled ? ' checked' : '') + '> 启用该端点</label>',
    note: 'URL 里通常带 bot token / webhook secret，仅存本机数据库并加密；完整服务列表见 shoutrrr 文档。',
    onOk: async () => {
      await post('/notify/endpoint/save', {
        id: ep ? ep.id : 0,
        label: $('f-nt-label').value.trim(),
        url: $('f-nt-url').value.trim(),
        enabled: $('f-nt-enabled').checked,
        actor: 'console',
      });
      toast(ep ? '端点已更新' : '端点已添加', 'ok');
      await loadNotify();
    },
  });
}
$('nt-add-btn').addEventListener('click', () => openEndpointSheet(null));
$('nt-save-btn').addEventListener('click', async () => {
  const warn = Math.round(Number($('nt-warn').value));
  try {
    const r = await post('/notify/settings', {
      enabled: $('nt-enabled').checked,
      error_alerts: $('nt-errors').checked,
      warn_pct: Number.isFinite(warn) && warn > 0 ? warn : 20,
      actor: 'console',
    });
    notifyCache.settings = r;
    toast('通知设置已保存', 'ok');
  } catch (e) { toast(e.message, 'err'); }
});
$('nt-list').addEventListener('click', ev => {
  const t = ev.target.closest('button[data-nt-test],button[data-nt-edit],button[data-nt-del]');
  if (!t) return;
  const id = Number(t.dataset.ntTest || t.dataset.ntEdit || t.dataset.ntDel);
  const ep = (notifyCache.endpoints || []).find(x => x.id === id);
  if (!ep) return;
  if (t.dataset.ntTest !== undefined) {
    (async () => {
      try {
        await post('/notify/endpoint/test', { id, actor: 'console' });
        toast('测试消息已发送，请到对应渠道查收', 'ok');
        renderNotify();
      } catch (e) { toast(e.message, 'err'); renderNotify(); }
    })();
  } else if (t.dataset.ntEdit !== undefined) {
    openEndpointSheet(ep);
  } else {
    openSheet({
      title: '删除通知端点', danger: true, okText: '删除',
      body: '<p>删除端点「<b>' + esc(ep.label || ep.url) + '</b>」？该操作不可撤销。</p>',
      onOk: async () => {
        await post('/notify/endpoint/delete', { id, actor: 'console' });
        toast('端点已删除', 'ok');
        await loadNotify();
      },
    });
  }
});

// ---------- 定期报告（日/周/月报） ----------
let reportsCache = [];
async function loadReports() {
  const r = await api('/reports');
  reportsCache = r.items || [];
  renderReports();
}
function renderReports() {
  const el = $('rp-list');
  if (!reportsCache.length) {
    el.innerHTML = '<p class="note">尚未配置定期报告。</p>';
    return;
  }
  const freqName = { daily: '日报', weekly: '周报', monthly: '月报' };
  const epName = id => {
    const e = (notifyCache.endpoints || []).find(x => x.id === id);
    return e ? (e.label || e.url) : '#' + id;
  };
  el.innerHTML = reportsCache.map(c => {
    let sched = '每天 ' + c.time_of_day;
    if (c.frequency === 'weekly') sched = '每周' + '一二三四五六日'[c.weekday - 1] + ' ' + c.time_of_day;
    if (c.frequency === 'monthly') sched = '每月 ' + c.monthday + ' 日 ' + c.time_of_day;
    const tz = c.tz_offset_min ? ' · UTC' + (c.tz_offset_min > 0 ? '+' : '') + Math.round(c.tz_offset_min / 60 * 10) / 10 : ' · UTC';
    const eps = (c.endpoint_ids || []).map(epName).join('、') || '无端点';
    const status = c.last_error
      ? '<span class="nt-err">✗ ' + esc(c.last_error) + '</span>'
      : c.last_sent_at
        ? '<span class="nt-ok">✓ 上次发送 · ' + esc(fmtDT(c.last_sent_at)) + '</span>'
        : '<span class="nt-dim">从未发送</span>';
    return '<div class="nt-item' + (c.enabled ? '' : ' off') + '">'
      + '<span class="nt-scheme">' + esc(freqName[c.frequency] || c.frequency) + '</span>'
      + '<div class="nt-main">'
      + '<div class="nt-head"><b>' + esc(c.name || '未命名报告') + '</b>' + (c.enabled ? '' : ' <span class="pill">停用</span>') + '</div>'
      + '<div class="nt-status">' + esc(sched + tz) + ' → ' + esc(eps) + '</div>'
      + '<div class="nt-status">' + status + '</div>'
      + '</div>'
      + '<div class="btn-row">'
      + '<button type="button" class="btn" data-rp-test="' + c.id + '">测试</button>'
      + '<button type="button" class="btn" data-rp-edit="' + c.id + '">编辑</button>'
      + '<button type="button" class="btn danger" data-rp-del="' + c.id + '">删除</button>'
      + '</div></div>';
  }).join('');
}
const RP_METRICS = [['cost', '费用'], ['tokens', 'Token'], ['requests', '请求数']];
function rpMetricSel(id, cur) {
  return '<select id="' + id + '" class="rp-select">'
    + RP_METRICS.map(m => '<option value="' + m[0] + '"' + (cur === m[0] ? ' selected' : '') + '>' + m[1] + '</option>').join('')
    + '</select>';
}
function openReportSheet(c) {
  const eps = notifyCache.endpoints || [];
  if (!eps.length) { toast('请先在「通知」面板配置至少一个端点', 'err'); return; }
  const s = (c && c.sections) || {};
  const bm = s.by_model || { on: !c, top: 5, metric: 'cost' };
  const bk = s.by_key || { on: false, top: 5, metric: 'cost' };
  const bc = s.by_caller || { on: false, top: 5, metric: 'cost' };
  const ids = (c && c.endpoint_ids) || [];
  const epChecks = eps.map(e =>
    '<label class="check-row"><input type="checkbox" class="rp-ep" value="' + e.id + '"'
    + (ids.includes(e.id) ? ' checked' : '') + '> ' + esc(e.label || e.url) + '</label>').join('');
  const topBlock = (key, label, t) =>
    '<div class="rp-top-row">'
    + '<label class="check-row"><input type="checkbox" id="rp-' + key + '-on"' + (t.on ? ' checked' : '') + '> ' + label + ' Top</label>'
    + '<input type="number" id="rp-' + key + '-top" min="1" max="20" value="' + (t.top || 5) + '">'
    + rpMetricSel('rp-' + key + '-metric', t.metric || 'cost')
    + '</div>';
  openSheet({
    title: c ? '编辑报告 · ' + (c.name || '') : '新增定期报告',
    okText: c ? '保存' : '添加',
    body:
      fieldRow('名称', '<input id="rp-name" placeholder="如：每日用量日报" value="' + esc(c ? c.name : '') + '">')
      + '<div class="form-grid">'
      + fieldRow('频率', '<select id="rp-freq" class="rp-select">'
        + [['daily', '日报'], ['weekly', '周报'], ['monthly', '月报']].map(f =>
          '<option value="' + f[0] + '"' + ((c ? c.frequency : 'daily') === f[0] ? ' selected' : '') + '>' + f[1] + '</option>').join('') + '</select>')
      + fieldRow('发送时刻', '<input id="rp-time" type="time" value="' + esc(c ? c.time_of_day : '09:00') + '">')
      + '<span id="rp-weekday-row">' + fieldRow('每周几（周报）', '<select id="rp-weekday" class="rp-select">'
        + ['周一', '周二', '周三', '周四', '周五', '周六', '周日'].map((d, i) =>
          '<option value="' + (i + 1) + '"' + ((c ? c.weekday : 1) === i + 1 ? ' selected' : '') + '>' + d + '</option>').join('') + '</select>')
      + '</span>'
      + '<span id="rp-monthday-row">' + fieldRow('每月几号（月报）', '<input type="number" id="rp-monthday" min="1" max="28" value="' + (c ? c.monthday : 1) + '">') + '</span>'
      + fieldRow('时区偏移（分钟，北京 +480）', '<input type="number" id="rp-tz" min="-840" max="840" step="15" value="' + (c ? c.tz_offset_min : 0) + '">')
      + '</div>'
      + '<div class="form-sep">内容板块</div>'
      + '<div class="rp-secs">'
      + '<label class="check-row"><input type="checkbox" id="rp-summary"' + (s.summary || !c ? ' checked' : '') + '> 汇总行（请求 / 费用 / Token / 成功率 / 缓存命中）</label>'
      + '<label class="check-row"><input type="checkbox" id="rp-failures"' + (s.failures ? ' checked' : '') + '> 失败请求明细</label>'
      + topBlock('by_model', '模型', bm)
      + topBlock('by_key', '密钥', bk)
      + topBlock('by_caller', '归属', bc)
      + '</div>'
      + '<div class="form-sep">发送端点</div>'
      + '<div class="rp-eps">' + epChecks + '</div>'
      + '<label class="check-row"><input type="checkbox" id="rp-enabled"' + (!c || c.enabled ? ' checked' : '') + '> 启用该报告</label>',
    note: '报告覆盖上一个已完成周期；测试按钮按同一周期立即生成发送，不影响计划。',
    onOk: async () => {
      const endpointIDs = [...document.querySelectorAll('.rp-ep:checked')].map(x => Number(x.value));
      if (!endpointIDs.length) throw new Error('至少选择一个发送端点');
      await post('/reports/save', {
        id: c ? c.id : 0,
        name: $('rp-name').value.trim(),
        enabled: $('rp-enabled').checked,
        frequency: $('rp-freq').value,
        time_of_day: $('rp-time').value || '09:00',
        weekday: Number($('rp-weekday').value),
        monthday: Number($('rp-monthday').value),
        tz_offset_min: Number($('rp-tz').value) || 0,
        sections: {
          summary: $('rp-summary').checked,
          failures: $('rp-failures').checked,
          by_model: { on: $('rp-by_model-on').checked, top: Number($('rp-by_model-top').value) || 5, metric: $('rp-by_model-metric').value },
          by_key: { on: $('rp-by_key-on').checked, top: Number($('rp-by_key-top').value) || 5, metric: $('rp-by_key-metric').value },
          by_caller: { on: $('rp-by_caller-on').checked, top: Number($('rp-by_caller-top').value) || 5, metric: $('rp-by_caller-metric').value },
        },
        endpoint_ids: endpointIDs,
        actor: 'console',
      });
      toast(c ? '报告已更新' : '报告已添加', 'ok');
      await loadReports();
    },
  });
  const freqSel = $('rp-freq');
  const syncFreq = () => {
    $('rp-weekday-row').hidden = freqSel.value !== 'weekly';
    $('rp-monthday-row').hidden = freqSel.value !== 'monthly';
  };
  freqSel.addEventListener('change', syncFreq);
  syncFreq();
}
$('rp-add-btn').addEventListener('click', () => openReportSheet(null));
$('rp-list').addEventListener('click', ev => {
  const t = ev.target.closest('button[data-rp-test],button[data-rp-edit],button[data-rp-del]');
  if (!t) return;
  const id = Number(t.dataset.rpTest || t.dataset.rpEdit || t.dataset.rpDel);
  const cfg = (reportsCache || []).find(x => x.id === id);
  if (!cfg) return;
  if (t.dataset.rpTest !== undefined) {
    (async () => {
      try {
        await post('/reports/test', { id, actor: 'console' });
        toast('测试报告已发送，请到对应渠道查收', 'ok');
        renderReports();
      } catch (e) { toast(e.message, 'err'); renderReports(); }
    })();
  } else if (t.dataset.rpEdit !== undefined) {
    openReportSheet(cfg);
  } else {
    openSheet({
      title: '删除定期报告', danger: true, okText: '删除',
      body: '<p>删除报告「<b>' + esc(cfg.name || cfg.frequency) + '</b>」？该操作不可撤销。</p>',
      onOk: async () => {
        await post('/reports/delete', { id, actor: 'console' });
        toast('报告已删除', 'ok');
        await loadReports();
      },
    });
  }
});

// ---------- 徽标 ----------
function updateBadges() {
  const sc = keysView.statusCounts || {};
  const bad = (Number(sc.disabled) || 0) + (Number(sc.revoked) || 0) + (Number(sc.expired) || 0);
  const dot = document.querySelector('[data-badge="keys"]');
  if (dot) { dot.hidden = bad === 0; dot.textContent = bad > 99 ? '99+' : String(bad); }
  const sys = document.querySelector('[data-badge="system"]');
  if (sys) sys.hidden = !(S.stats && S.stats.writable === false);
}

// ---------- 登录门 ----------
$('gate-form').addEventListener('submit', async e => {
  e.preventDefault();
  const btn = $('gate-submit');
  const k = $('gate-key').value.trim();
  if (!k) return;
  btn.disabled = true;
  btn.textContent = '验证中…';
  $('gate-error').hidden = true;
  const saved = key;
  key = k;
  try {
    await api('/health');
    sessionStorage.setItem('cpa-management-key', k);
    $('gate-key').value = '';
    showApp();
  } catch (err) {
    key = saved;
    $('gate-error').textContent = err.message === '管理密钥无效或已失效' ? '管理密钥不正确' : err.message;
    $('gate-error').hidden = false;
  } finally {
    btn.disabled = false;
    btn.textContent = '进入面板';
  }
});

function showApp() {
  $('gate').hidden = true;
  $('app').hidden = false;
  loadCallers();
  refreshKeys().catch(() => {}); // 预热徽标
  api('/health').then(h => { S.stats = h.stats; updateBadges(); }).catch(() => {});
  switchTab('overview');
}

// ---------- 启动 ----------
applyTheme();
if (key) showApp();
else { $('gate').hidden = false; $('gate-key').focus(); }
})();
