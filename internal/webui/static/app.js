/* 公共工具：请求、提示、主题、复制 */
(function () {
  'use strict';

  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.prototype.slice.call((root || document).querySelectorAll(sel));
  window.$ = $;
  window.$$ = $$;

  window.esc = function (s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  };

  window.api = async function (path, opts) {
    opts = opts || {};
    let res;
    try {
      res = await fetch(path, Object.assign({ credentials: 'same-origin', headers: { Accept: 'application/json' } }, opts));
    } catch (e) {
      throw new Error('网络错误：' + e.message);
    }
    const text = await res.text();
    let data = null;
    if (text) {
      try { data = JSON.parse(text); } catch (e) { data = { error: text.slice(0, 300) }; }
    }
    if (res.status === 401) {
      if (!/^\/stream\/|^\/proxy\//.test(path)) {
        location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search);
      }
      throw new Error('登录已过期，请重新登录');
    }
    if (!res.ok) {
      throw new Error((data && data.error) || ('请求失败 HTTP ' + res.status));
    }
    return data;
  };

  window.postJSON = function (path, body) {
    return window.api(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {})
    });
  };
  window.delJSON = (path) => window.api(path, { method: 'DELETE' });

  let toastTimer = null;
  window.toast = function (msg, kind) {
    const el = $('#toast');
    if (!el) { return; }
    el.textContent = msg;
    el.className = 'toast' + (kind ? ' ' + kind : '');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { el.classList.add('hidden'); }, 2800);
  };

  // 主题切换
  const themeBtn = $('#themeBtn');
  function syncThemeBtn() {
    if (themeBtn) {
      themeBtn.textContent = document.documentElement.dataset.theme === 'light' ? '☀️' : '🌙';
    }
  }
  if (themeBtn) {
    syncThemeBtn();
    themeBtn.addEventListener('click', function () {
      const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
      document.documentElement.dataset.theme = next;
      try { localStorage.setItem('tvhub-theme', next); } catch (e) { }
      syncThemeBtn();
    });
  }

  // data-copy="#selector" 复制按钮
  document.addEventListener('click', function (e) {
    const btn = e.target.closest('[data-copy]');
    if (!btn) { return; }
    const input = $(btn.getAttribute('data-copy'));
    if (!input) { return; }
    const val = input.value !== undefined ? input.value : input.textContent;
    const ok = function () { window.toast('已复制到剪贴板', 'ok'); };
    const fallback = function () {
      if (input.select) { input.select(); }
      try { document.execCommand('copy'); ok(); } catch (err) { window.toast('复制失败，请手动复制', 'err'); }
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(val).then(ok, fallback);
    } else {
      fallback();
    }
  });

  window.confirmAsk = function (msg) { return window.confirm(msg); };
})();
