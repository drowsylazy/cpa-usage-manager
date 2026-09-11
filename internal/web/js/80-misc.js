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
  syncPrefsFromServer();
  refreshKeys().catch(() => {}); // 预热徽标
  api('/health').then(h => { S.stats = h.stats; updateBadges(); }).catch(() => {});
  $('auto-refresh').checked = localStorage.getItem('auto-refresh') === '1';
  $('auto-refresh-secs').value = localStorage.getItem('auto-refresh-secs') || '30';
  setupAutoRefresh();
  loadDispCurRate();
  switchTab('overview');
}

// ---------- 偏好服务器端同步 ----------
// 偏好改动经 savePref 双写 localStorage 与 /preferences（ui_ 前缀键）；
// 登录后拉回服务器值，与本地不一致时以服务器为准写回本地并整页重载一次
// ——列偏好等组件在构造时读 localStorage，重载是让它们吃到服务器值最
// 稳妥的方式。sessionStorage 标记防重载循环：重载后值已一致即不再触发，
// 若期间无差异则清掉标记，允许后续再次同步。
const PREF_KEYS = ['console-range', 'req-cols', 'req-size', 'ov-models-metric', 'ov-keys-metric', 'disp-cur', 'auto-refresh', 'auto-refresh-secs'];
function validPref(k, v) {
  if (v === null || v === undefined || v === '') return false;
  switch (k) {
    case 'console-range':
      return PRESETS.some(p => p.id === v); // custom 无起止时刻，不做跨设备同步
    case 'req-size':
      return ['20', '50', '100'].includes(v);
    case 'ov-models-metric':
    case 'ov-keys-metric':
      return ['tokens', 'cost', 'requests'].includes(v);
    case 'disp-cur':
      return v === 'usd' || v === 'cny';
    case 'auto-refresh':
      return v === '0' || v === '1';
    case 'auto-refresh-secs': {
      const n = parseInt(v, 10);
      return isFinite(n) && n >= 5 && n <= 86400;
    }
    case 'req-cols': {
      try {
        const arr = JSON.parse(v);
        return Array.isArray(arr) && arr.length > 0 && arr.every(id => REQ_COLS.some(c => c.id === id));
      } catch (_) { return false; }
    }
    default: return false;
  }
}
async function syncPrefsFromServer() {
  try {
    const prefs = await api('/preferences');
    let changed = false;
    for (const k of PREF_KEYS) {
      const v = prefs['ui_' + k];
      if (!validPref(k, v) || localStorage.getItem(k) === v) continue;
      localStorage.setItem(k, v);
      changed = true;
    }
    if (changed) {
      if (!sessionStorage.getItem('ui-prefs-reloaded')) {
        sessionStorage.setItem('ui-prefs-reloaded', '1');
        location.reload();
      }
    } else {
      sessionStorage.removeItem('ui-prefs-reloaded');
    }
  } catch (_) { /* 同步失败不影响本地使用 */ }
}


// ---------- 计价试算器 ----------
// priceCalcMicro 按一条规则的四档单价（micro 原生币种/百万 token）算出
// 指定 token 用量的费用（micro 原生币种）：各档 (量×价+999999)/1e6 向上取整后相加，
// 与服务端 costForRule 的取整口径一致。
function priceCalcMicro(p, inTok, outTok, crTok, cwTok) {
  const ceil = (n, price) => price > 0 ? (BigInt(Math.round(n)) * BigInt(price) + 999999n) / 1000000n : 0n;
  return ceil(inTok, p.price_input || 0) + ceil(outTok, p.price_output || 0)
    + ceil(crTok, p.price_cache_read || 0) + ceil(cwTok, p.price_cache_creation || 0);
}
$('pricing-calc').addEventListener('click', () => {
  const items = pricingCache.items || [];
  if (!items.length) { toast('没有可用规则，请先添加计价规则', 'err'); return; }
  openSheet({
    title: '计价试算', okText: '重新计算',
    body: '<div class="stack">'
      + fieldRow('规则', '<select id="pc-rule" class="input">' + items.map(p =>
        '<option value="' + p.id + '">' + esc(p.pattern) + '（' + esc(p.match_kind) + (p.currency === 'CNY' ? ' · CNY' : '') + '）</option>').join('') + '</select>')
      + fieldRow('输入 Token', '<input type="number" id="pc-in" class="input" min="0" step="1000" value="100000">')
      + fieldRow('输出 Token', '<input type="number" id="pc-out" class="input" min="0" step="1000" value="20000">')
      + fieldRow('缓存读 Token', '<input type="number" id="pc-cr" class="input" min="0" step="1000" value="0">')
      + fieldRow('缓存写 Token', '<input type="number" id="pc-cw" class="input" min="0" step="1000" value="0">')
      + '<p class="note" id="pc-result">填入用量后点击下方按钮计算。</p></div>',
    onOk: () => {
      const id = Number($('pc-rule').value);
      const p = items.find(x => x.id === id);
      if (!p) return;
      const micro = priceCalcMicro(p,
        Number($('pc-in').value) || 0, Number($('pc-out').value) || 0,
        Number($('pc-cr').value) || 0, Number($('pc-cw').value) || 0);
      const num = Number(micro);
      let text = p.currency === 'CNY'
        ? '费用：' + fmtMoney(num, '¥') + '（人民币规则；美元等值按当前实时汇率折算）'
        : '费用：$' + (num / 1e6).toFixed(4);
      $('pc-result').textContent = text;
      return false; // 保持弹窗打开，便于反复调参对比
    },
  });
});

