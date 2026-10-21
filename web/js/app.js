'use strict';

(() => {
  const $ = (sel) => document.querySelector(sel);

  const els = {
    gate: $('#gate'), gateForm: $('#gateForm'), codeInput: $('#codeInput'), gateError: $('#gateError'),
    board: $('#board'), wall: $('#wall'), empty: $('#empty'),
    roomBar: $('#roomBar'), roomCode: $('#roomCode'), revealBtn: $('#revealBtn'),
    liveStamp: $('#liveStamp'), onlineCount: $('#onlineCount'), leaveBtn: $('#leaveBtn'),
    composer: $('#composer'), text: $('#composerText'), drafts: $('#drafts'), pickBtn: $('#pickBtn'),
    fileInput: $('#fileInput'), pickFileBtn: $('#pickFileBtn'), anyFileInput: $('#anyFileInput'), pinDraft: $('#pinDraft'), sendBtn: $('#sendBtn'), hint: $('#composerHint'),
    lightbox: $('#lightbox'), lightboxImg: $('#lightboxImg'), toast: $('#toast'),
  };

  const state = {
    cfg: { ttlMs: 3 * 864e5, maxImageBytes: 20 << 20, maxFileBytes: 50 << 20, maxTextBytes: 1 << 20, minCodeLen: 4 },
    clips: new Map(),        // id -> { data, el, refs }
    drafts: [],
    sending: false,
    es: null,
    viewed: new Set(),       // 本页已上报过查看的
    pendingViews: new Set(),
    own: new Set(),          // 本设备贴上的，不上报查看
    copyGuard: false,
    lastCopy: new Map(),
    reveal: false,
  };

  const LS_CODE = 'swc.code';
  const store = {
    get(k) { try { return localStorage.getItem(k); } catch { return null; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* 隐私模式 */ } },
    del(k) { try { localStorage.removeItem(k); } catch { /* 隐私模式 */ } },
  };

  const coarse = window.matchMedia('(pointer: coarse)').matches;
  const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  const canCopyImage = !!(window.isSecureContext && navigator.clipboard && navigator.clipboard.write && window.ClipboardItem);

  // ---------- 小工具 ----------

  function h(tag, attrs, ...children) {
    const el = document.createElement(tag);
    if (attrs) {
      for (const [k, v] of Object.entries(attrs)) {
        if (v == null || v === false) continue;
        if (k === 'class') el.className = v;
        else if (k === 'text') el.textContent = v;
        else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
        else el.setAttribute(k, v === true ? '' : v);
      }
    }
    for (const c of children.flat()) {
      if (c == null || c === false) continue;
      el.append(c.nodeType ? c : document.createTextNode(String(c)));
    }
    return el;
  }

  let toastTimer = 0;
  function toast(msg) {
    els.toast.textContent = msg;
    els.toast.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => els.toast.classList.remove('show'), 2600);
  }

  class AuthError extends Error {}

  async function api(method, url, body) {
    const opt = { method, headers: {}, credentials: 'same-origin' };
    if (body !== undefined) {
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetch(url, opt);
    } catch {
      throw new Error('网络连接失败，请稍后再试');
    }
    if (res.status === 401) {
      showGate();
      throw new AuthError('请先输入口令');
    }
    const isJSON = (res.headers.get('content-type') || '').includes('json');
    const data = isJSON ? await res.json().catch(() => null) : null;
    if (!res.ok) throw new Error((data && data.error) || `请求失败（${res.status}）`);
    return data;
  }

  // 由便签 ID 派生的伪随机数，保证每台设备上同一张便签长得一样。
  function seedOf(str) {
    let x = 2166136261;
    for (let i = 0; i < str.length; i++) { x ^= str.charCodeAt(i); x = Math.imul(x, 16777619); }
    return x >>> 0;
  }
  function rng(seed) {
    let a = seed;
    return () => {
      a = (a + 0x6D2B79F5) | 0;
      let t = Math.imul(a ^ (a >>> 15), 1 | a);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }

  // 上沿轻微起伏、下沿像撕下来的锯齿、右上角留出折角。
  function tornClip(r, fold = 18) {
    const p = [];
    const f = (n) => n.toFixed(3);
    const px = (n) => n.toFixed(1) + 'px';
    const top = 14, bottom = 24, side = 6;
    for (let i = 0; i <= top; i++) p.push(`calc((100% - ${fold}px) * ${f(i / top)}) ${px(r() * 2.2)}`);
    p.push(`100% ${fold}px`);
    for (let i = 1; i < side; i++) p.push(`calc(100% - ${px(r() * 1.6)}) calc(${fold}px + (100% - ${fold}px) * ${f(i / side)})`);
    for (let i = bottom; i >= 0; i--) p.push(`${(i / bottom * 100).toFixed(2)}% calc(100% - ${px(r() * 5)})`);
    for (let i = side - 1; i > 0; i--) p.push(`${px(r() * 1.6)} ${(i / side * 100).toFixed(2)}%`);
    return `polygon(${p.join(',')})`;
  }

  function setClip(el, value) {
    el.style.webkitClipPath = value;
    el.style.clipPath = value;
  }

  function underline(r) {
    const y = (base = 4) => (base + r() * 3.5).toFixed(1);
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('class', 'underline');
    svg.setAttribute('viewBox', '0 0 300 12');
    svg.setAttribute('preserveAspectRatio', 'none');
    svg.setAttribute('aria-hidden', 'true');
    for (const d of [
      `M2 ${y()} C 60 ${y()}, 120 ${y()}, 180 ${y()} S 268 ${y()}, 298 ${y()}`,
      `M10 ${y(6)} C 80 ${y(5)}, 150 ${y(6)}, 220 ${y(5)} S 280 ${y(6)}, 290 ${y(6)}`,
    ]) {
      const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
      path.setAttribute('d', d);
      svg.append(path);
    }
    return svg;
  }

  const CLIPPER_SVG = '<svg viewBox="0 0 28 64" fill="none" stroke="#7b8088" stroke-width="2.6" stroke-linecap="round" aria-hidden="true">'
    + '<path d="M9 20 V48 a5 5 0 0 0 10 0 V12 a7 7 0 0 0 -14 0 V52 a9 9 0 0 0 18 0 V22"/></svg>';

  const PAPERS = ['cream', 'pink', 'sage', 'butter', 'sky', 'lilac'];
  const KINDS = { text: '文字', image: '图片', file: '文件' };
  // 能贴成图片便签的格式，其余的一律按文件分享。
  const IMAGE_TYPES = new Set(['image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/bmp',
    'image/avif', 'image/heic', 'image/heif']);
  const pad2 = (n) => String(n).padStart(2, '0');
  const mb = (n) => Math.round(n / 1048576);

  function extOf(name) {
    const m = /\.([A-Za-z0-9]{1,8})$/.exec(name || '');
    return m ? m[1].toUpperCase() : 'FILE';
  }

  function fmtTime(ms, withSeconds) {
    const d = new Date(ms);
    const now = new Date();
    const hm = `${pad2(d.getHours())}:${pad2(d.getMinutes())}${withSeconds ? ':' + pad2(d.getSeconds()) : ''}`;
    if (d.toDateString() === now.toDateString()) return `今天 ${hm}`;
    const y = new Date(now); y.setDate(now.getDate() - 1);
    if (d.toDateString() === y.toDateString()) return `昨天 ${hm}`;
    return `${d.getFullYear() !== now.getFullYear() ? d.getFullYear() + '-' : ''}${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${hm}`;
  }

  function fmtLeft(ms) {
    if (ms <= 0) return '即将清理';
    const m = Math.floor(ms / 6e4), hr = Math.floor(m / 60), d = Math.floor(hr / 24);
    if (d > 0) return `${d} 天 ${hr % 24} 小时`;
    if (hr > 0) return `${hr} 小时 ${m % 60} 分`;
    return `${Math.max(1, m)} 分钟`;
  }

  function fmtSize(n) {
    if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + ' MB';
    if (n >= 1024) return Math.round(n / 1024) + ' KB';
    return n + ' B';
  }

  function fmtTTL(ms) {
    const hr = ms / 36e5;
    if (hr >= 24 && hr % 24 === 0) return `${hr / 24} 天`;
    if (hr >= 1) return `${Math.round(hr * 10) / 10} 小时`;
    return `${Math.round(ms / 6e4)} 分钟`;
  }

  function device(ua) {
    if (!ua) return '未知设备';
    const os = /iPhone/.test(ua) ? 'iPhone' : /iPad/.test(ua) ? 'iPad' : /Android/.test(ua) ? 'Android'
      : /Windows/.test(ua) ? 'Windows' : /Mac OS X|Macintosh/.test(ua) ? 'Mac' : /Linux/.test(ua) ? 'Linux' : '其他设备';
    const br = /MicroMessenger/.test(ua) ? '微信' : /\bQQ\//.test(ua) ? 'QQ' : /EdgA?\/|EdgiOS/.test(ua) ? 'Edge'
      : /Firefox|FxiOS/.test(ua) ? 'Firefox' : /CriOS|Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : '';
    return br ? `${os} · ${br}` : os;
  }

  const ACTIONS = { create: '贴上', view: '查看', copy: '复制', download: '下载', pin: '钉住', unpin: '取消钉' };

  // ---------- 进入 / 离开 ----------

  async function boot() {
    try {
      const s = await api('GET', '/api/session');
      Object.assign(state.cfg, s);
    } catch (err) {
      toast(err.message);
    }
    els.codeInput.placeholder = `至少 ${state.cfg.minCodeLen} 个字符`;
    els.hint.textContent = (coarse ? '长按上面的空白处即可粘贴' : 'Ctrl+V 粘贴文字、截图或文件 · Ctrl+Enter 贴上墙')
      + ` · 文件上限 ${mb(state.cfg.maxFileBytes)} MB · 未钉住的便签 ${fmtTTL(state.cfg.ttlMs)}后自动清理`;
    els.text.placeholder = coarse
      ? '写点什么，或者长按这里粘贴文字 / 图片…'
      : '写点什么，或者在页面任意位置按 Ctrl+V 粘贴文字 / 截图 / 文件，也可以把文件拖进来…';

    if (state.cfg.entered) return showBoard();
    const saved = store.get(LS_CODE);
    if (saved) {
      try { await enter(saved); return; } catch { /* 口令失效，回到输入页 */ }
    }
    showGate();
  }

  async function enter(code) {
    await api('POST', '/api/session', { code });
    store.set(LS_CODE, code);
    showBoard();
  }

  function showGate() {
    if (state.es) { state.es.close(); state.es = null; }
    for (const { el } of state.clips.values()) el.remove();
    state.clips.clear();
    els.board.hidden = true;
    els.roomBar.hidden = true;
    els.gate.hidden = false;
    setTimeout(() => els.codeInput.focus(), 50);
  }

  function showBoard() {
    els.gate.hidden = true;
    els.board.hidden = false;
    els.roomBar.hidden = false;
    renderRoomCode();
    connect();
    refresh();
  }

  function renderRoomCode() {
    const code = store.get(LS_CODE);
    els.revealBtn.hidden = !code;
    els.roomCode.textContent = !code ? '已进入' : state.reveal ? code : '•'.repeat(Math.min(code.length, 8));
    els.revealBtn.textContent = state.reveal ? '隐藏' : '显示';
  }

  els.gateForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    const code = els.codeInput.value.trim();
    els.gateError.textContent = '';
    if ([...code].length < state.cfg.minCodeLen) {
      els.gateError.textContent = `口令至少要 ${state.cfg.minCodeLen} 个字符`;
      return;
    }
    try {
      await enter(code);
      els.codeInput.value = '';
    } catch (err) {
      els.gateError.textContent = err.message;
    }
  });

  els.revealBtn.addEventListener('click', () => { state.reveal = !state.reveal; renderRoomCode(); });

  els.leaveBtn.addEventListener('click', async () => {
    try { await api('DELETE', '/api/session'); } catch { /* 无论如何都回到输入页 */ }
    store.del(LS_CODE);
    state.reveal = false;
    showGate();
  });

  // ---------- 实时同步 ----------

  function connect() {
    if (state.es) state.es.close();
    const es = new EventSource('/api/events');
    state.es = es;
    es.onopen = () => { setLive(true); refresh(); };
    es.onmessage = (e) => {
      let m;
      try { m = JSON.parse(e.data); } catch { return; }
      if (m.type === 'create' || m.type === 'update') upsert(m.clip);
      else if (m.type === 'delete') removeClip(m.id, true);
      else if (m.type === 'presence') els.onlineCount.textContent = `${m.online} 台设备在看`;
    };
    es.onerror = () => {
      setLive(false);
      if (es.readyState === EventSource.CLOSED && state.es === es) {
        // 浏览器放弃重连了（比如口令失效），确认一下会话再决定。
        setTimeout(async () => {
          if (state.es !== es) return;
          try { await api('GET', '/api/clips'); connect(); } catch { /* 401 时 api() 已回到口令页 */ }
        }, 3000);
      }
    };
  }

  function setLive(on) {
    els.liveStamp.textContent = on ? '在线' : '重连中';
    els.liveStamp.className = 'stamp ' + (on ? 'green' : 'red');
  }

  let refreshing = null;
  async function refresh() {
    if (refreshing) return refreshing;
    refreshing = (async () => {
      try {
        const { clips } = await api('GET', '/api/clips');
        const ids = new Set(clips.map((c) => c.id));
        for (const id of [...state.clips.keys()]) if (!ids.has(id)) removeClip(id, false);
        for (const c of clips) upsert(c, true);
        arrange();
      } catch (err) {
        if (!(err instanceof AuthError)) toast(err.message);
      } finally {
        refreshing = null;
      }
    })();
    return refreshing;
  }

  // ---------- 便签渲染 ----------

  function upsert(c, quiet) {
    const item = state.clips.get(c.id);
    if (item) {
      item.data = c;
      updateClip(item);
      if (item.flipped) scheduleLogs(item);
    } else {
      const it = buildClip(c);
      state.clips.set(c.id, it);
      if (!quiet && !reduceMotion) {
        it.el.classList.add('enter');
        it.el.addEventListener('animationend', () => it.el.classList.remove('enter'), { once: true });
      }
      viewObserver.observe(it.el);
    }
    if (!quiet) arrange();
  }

  function arrange() {
    const sorted = [...state.clips.values()].sort((a, b) =>
      (b.data.pinned - a.data.pinned) || (b.data.createdAt - a.data.createdAt));
    let prev = els.composer;
    for (const it of sorted) {
      if (prev.nextElementSibling !== it.el) prev.after(it.el);
      prev = it.el;
    }
    els.empty.hidden = state.clips.size > 0;
  }

  function removeClip(id, animate) {
    const it = state.clips.get(id);
    if (!it) return;
    state.clips.delete(id);
    viewObserver.unobserve(it.el);
    if (animate && !reduceMotion) {
      it.el.classList.add('leaving');
      setTimeout(() => it.el.remove(), 320);
    } else {
      it.el.remove();
    }
    els.empty.hidden = state.clips.size > 0;
  }

  function buildClip(c) {
    const r = rng(seedOf(c.id));
    const el = h('article', { class: `clip paper-${PAPERS[Math.floor(r() * PAPERS.length)]}`, 'data-id': c.id });
    el.style.setProperty('--rot', (r() * 3.2 - 1.6).toFixed(2) + 'deg');

    const tape = h('span', { class: 'tape', 'aria-hidden': 'true' });
    tape.style.setProperty('--tape-rot', (r() * 10 - 5).toFixed(1) + 'deg');
    tape.style.setProperty('--tape-x', (32 + r() * 36).toFixed(0) + '%');
    const clipper = h('span', { class: 'clipper', 'aria-hidden': 'true' });
    clipper.innerHTML = CLIPPER_SVG;

    const refs = {
      no: h('span', { class: 'no' }),
      kind: h('span', { class: 'kind' }),
      stamp: h('span', { class: 'stamp' }),
      lifeLabel: h('span'),
      lifeFill: h('span', { class: 'fill' }),
      views: h('b'), copies: h('b'), downloads: h('b'),
      meta: h('div', { class: 'meta' }),
      pinBtn: h('button', { type: 'button', class: 'btn' }),
    };

    let content, more = null;
    if (c.kind === 'text') {
      const text = h('div', { class: 'text', text: c.content });
      content = h('div', { class: 'content' }, text);
      more = h('button', { type: 'button', class: 'link more', hidden: true, text: '展开全部' });
      more.addEventListener('click', () => {
        const open = el.classList.toggle('expanded');
        more.textContent = open ? '收起' : '展开全部';
      });
      refs.textEl = text;
      requestAnimationFrame(() => {
        const clamped = text.scrollHeight > text.clientHeight + 2;
        content.classList.toggle('clamped', clamped);
        more.hidden = !clamped;
      });
    } else if (c.kind === 'file') {
      content = h('a', { class: 'filecard', href: `/api/clips/${c.id}/file`, download: '', title: '点击下载' },
        h('span', { class: 'doc', 'aria-hidden': 'true' }, h('span', { class: 'ext', text: extOf(c.fileName) })),
        h('span', { class: 'info' },
          h('span', { class: 'name', text: c.fileName }),
          h('span', { class: 'size', text: fmtSize(c.size) })));
    } else {
      const img = h('img', {
        alt: c.fileName || '图片便签', loading: 'lazy', decoding: 'async',
        src: `/api/clips/${c.id}/image${c.hasThumb ? '?thumb' : ''}`,
      });
      img.addEventListener('click', () => openLightbox(c.id));
      content = h('figure', { class: 'photo content' }, img,
        h('figcaption', { text: [c.fileName && !/^image\./i.test(c.fileName) ? c.fileName : '', fmtSize(c.size)].filter(Boolean).join(' · ') }));
    }

    const actions = [];
    if (c.kind === 'text') {
      actions.push(h('button', { type: 'button', class: 'btn', text: '复制', onclick: () => copyClipText(item) }));
    } else if (c.kind === 'file') {
      actions.push(h('a', { class: 'btn', href: `/api/clips/${c.id}/file`, download: '', text: '下载' }));
    } else {
      if (canCopyImage) actions.push(h('button', { type: 'button', class: 'btn', text: '复制', onclick: () => copyClipImage(item) }));
      actions.push(h('a', { class: 'btn', href: `/api/clips/${c.id}/image?dl`, download: '', text: '下载' }));
    }
    refs.pinBtn.addEventListener('click', () => togglePin(item));
    actions.push(refs.pinBtn);
    actions.push(h('button', { type: 'button', class: 'btn danger', text: '撕掉', onclick: () => deleteClip(item) }));

    const front = h('div', { class: 'face front' },
      h('div', { class: 'head' }, refs.no, refs.kind, refs.stamp),
      underline(r),
      content,
      more,
      h('div', { class: 'life' },
        h('div', { class: 'row' }, h('span', { text: '有效期' }), refs.lifeLabel),
        h('div', { class: 'bar' }, refs.lifeFill)),
      h('div', { class: 'stats' },
        h('span', null, '查看', refs.views),
        c.kind !== 'file' ? h('span', null, '复制', refs.copies) : null,
        c.kind !== 'text' ? h('span', null, '下载', refs.downloads) : null),
      refs.meta,
      h('div', { class: 'actions' }, actions,
        h('button', { type: 'button', class: 'flip', text: '日志 ↗', onclick: () => flip(item, true) })),
    );

    refs.logs = h('ol', { class: 'logs' });
    refs.summary = h('div', { class: 'summary' });
    const back = h('div', { class: 'face back', hidden: true },
      h('div', { class: 'head' }, h('span', { class: 'no', text: '访问日志' }), refs.backNo = h('span', { class: 'kind' })),
      underline(r),
      refs.summary,
      refs.logs,
      h('div', { class: 'actions' },
        h('button', { type: 'button', class: 'flip', text: '↖ 翻回去', onclick: () => flip(item, false) })),
    );

    const paper = h('div', { class: 'paper' }, front, back);
    setClip(paper, tornClip(r));
    el.append(tape, clipper, paper);

    const item = { data: c, el, refs, paper, front, back, flipped: false };
    updateClip(item);
    return item;
  }

  function updateClip(item) {
    const c = item.data, r = item.refs;
    r.no.textContent = `NO. ${pad2(c.no)}`;
    r.backNo.textContent = `NO. ${pad2(c.no)}`;
    r.kind.textContent = KINDS[c.kind] || c.kind;
    r.views.textContent = c.views;
    r.copies.textContent = c.copies;
    r.downloads.textContent = c.downloads;
    r.meta.textContent = `${fmtTime(c.createdAt)} 由 ${c.creatorIp || '未知'} 贴上`;
    r.pinBtn.textContent = c.pinned ? '取消钉住' : '钉住';
    r.pinBtn.classList.toggle('on', c.pinned);
    item.el.classList.toggle('pinned', c.pinned);
    updateLife(item);
  }

  function updateLife(item) {
    const c = item.data, r = item.refs;
    if (c.pinned) {
      r.stamp.textContent = '已钉住';
      r.stamp.className = 'stamp red';
      r.lifeLabel.textContent = '钉住了，不会自动删除';
      r.lifeFill.style.setProperty('--f', 1);
      item.el.classList.remove('urgent');
      return;
    }
    const left = c.expiresAt - Date.now();
    const total = Math.max(1, c.expiresAt - c.createdAt);
    const urgent = left < Math.min(6 * 36e5, total / 4);
    const shortLeft = fmtLeft(left).replace(/ \d+ (小时|分)$/, '');
    r.stamp.textContent = left <= 0 ? '已过期' : `剩 ${shortLeft}`;
    r.stamp.className = 'stamp ' + (urgent ? 'red' : 'green');
    r.lifeLabel.textContent = left <= 0 ? '即将清理' : `还剩 ${fmtLeft(left)}`;
    r.lifeFill.style.setProperty('--f', Math.max(0, Math.min(1, left / total)).toFixed(3));
    item.el.classList.toggle('urgent', urgent);
  }

  // 每分钟刷新剩余时间；过期的先在本地藏起来，服务器稍后会真正清理。
  setInterval(() => {
    const now = Date.now();
    for (const it of [...state.clips.values()]) {
      if (!it.data.pinned && it.data.expiresAt <= now) removeClip(it.data.id, true);
      else updateLife(it);
    }
  }, 30000);

  // ---------- 翻面看日志 ----------

  async function flip(item, toBack) {
    if (item.flipping || item.flipped === toBack) return;
    item.flipping = true;
    const { paper } = item;
    if (toBack) paper.style.height = Math.max(paper.offsetHeight, 320) + 'px';
    if (!reduceMotion && paper.animate) {
      await paper.animate([{ transform: 'rotateY(0)' }, { transform: 'rotateY(90deg)' }],
        { duration: 170, easing: 'ease-in' }).finished.catch(() => {});
    }
    item.front.hidden = toBack;
    item.back.hidden = !toBack;
    item.flipped = toBack;
    if (!toBack) paper.style.height = '';
    if (!reduceMotion && paper.animate) {
      paper.animate([{ transform: 'rotateY(-90deg)' }, { transform: 'rotateY(0)' }],
        { duration: 200, easing: 'ease-out' });
    }
    item.flipping = false;
    if (toBack) loadLogs(item, true);
  }

  async function loadLogs(item, showLoading) {
    const { logs, summary } = item.refs;
    if (showLoading) logs.replaceChildren(h('li', { class: 'placeholder', text: '翻找记录中…' }));
    try {
      const data = await api('GET', `/api/clips/${item.data.id}/logs`);
      const ips = new Set(data.logs.map((l) => l.ip));
      summary.textContent = `共 ${data.logs.length} 条记录 · ${ips.size} 个不同 IP`;
      if (!data.logs.length) {
        logs.replaceChildren(h('li', { class: 'placeholder', text: '还没有记录' }));
        return;
      }
      logs.replaceChildren(...data.logs.map((l) => h('li', { class: `log log-${l.action}` },
        h('span', { class: 'act', text: ACTIONS[l.action] || l.action }),
        h('span', { class: 'ip', text: l.ip }),
        h('time', { text: fmtTime(l.at, true) }),
        h('span', { class: 'dev', text: device(l.ua) }),
      )));
    } catch (err) {
      if (!(err instanceof AuthError)) logs.replaceChildren(h('li', { class: 'placeholder', text: err.message }));
    }
  }

  function scheduleLogs(item) {
    clearTimeout(item.logTimer);
    item.logTimer = setTimeout(() => { if (item.flipped) loadLogs(item, false); }, 800);
  }

  // ---------- 复制 / 下载 / 钉住 / 删除 ----------

  async function writeText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      try { await navigator.clipboard.writeText(text); return true; } catch { /* 退回旧方法 */ }
    }
    // HTTP 页面或旧浏览器：借一个隐藏的 textarea 执行复制命令。
    const ta = h('textarea', { readonly: true });
    ta.value = text;
    Object.assign(ta.style, { position: 'fixed', top: '0', left: '0', opacity: '0', fontSize: '16px' });
    document.body.append(ta);
    ta.select();
    ta.setSelectionRange(0, text.length);
    state.copyGuard = true;
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { ok = false; }
    state.copyGuard = false;
    ta.remove();
    return ok;
  }

  async function copyClipText(item) {
    if (await writeText(item.data.content)) {
      toast('已复制到剪贴板');
      reportCopy(item.data.id, true);
    } else {
      toast('复制失败，请手动选中文字复制');
    }
  }

  async function copyClipImage(item) {
    try {
      // Safari 要求在点击的同一时刻创建 ClipboardItem，所以把取图放进 Promise 里。
      const png = fetch(`/api/clips/${item.data.id}/image`).then((r) => r.blob()).then(toPng);
      await navigator.clipboard.write([new ClipboardItem({ 'image/png': png })]);
      toast('图片已复制');
      reportCopy(item.data.id, true);
    } catch {
      toast('这个浏览器不支持复制图片，请右键或长按图片复制');
    }
  }

  async function toPng(blob) {
    if (blob.type === 'image/png') return blob;
    const bmp = await createImageBitmap(blob);
    const cv = document.createElement('canvas');
    cv.width = bmp.width;
    cv.height = bmp.height;
    cv.getContext('2d').drawImage(bmp, 0, 0);
    bmp.close();
    return new Promise((res, rej) => cv.toBlob((b) => (b ? res(b) : rej(new Error('转换失败'))), 'image/png'));
  }

  function reportCopy(id, force) {
    const now = Date.now();
    if (!force && now - (state.lastCopy.get(id) || 0) < 1500) return;
    state.lastCopy.set(id, now);
    api('POST', `/api/clips/${id}/copy`).catch(() => {});
  }

  // 用户自己选中文字按 Ctrl+C / 长按拷贝时也计一次复制。
  document.addEventListener('copy', () => {
    if (state.copyGuard) return;
    const sel = document.getSelection();
    if (!sel || sel.isCollapsed) return;
    const ids = new Set();
    for (const node of [sel.anchorNode, sel.focusNode]) {
      const el = node && (node.nodeType === 1 ? node : node.parentElement);
      const content = el && el.closest('.clip .content');
      if (content) ids.add(content.closest('.clip').dataset.id);
    }
    ids.forEach((id) => reportCopy(id, false));
  });

  async function togglePin(item) {
    try {
      const c = await api('POST', `/api/clips/${item.data.id}/pin`, { pinned: !item.data.pinned });
      upsert(c);
      toast(c.pinned ? '钉住了，不会被自动删除' : `已取消钉住，${fmtLeft(c.expiresAt - Date.now())}后自动删除`);
    } catch (err) {
      if (!(err instanceof AuthError)) toast(err.message);
    }
  }

  async function deleteClip(item) {
    if (!confirm(`撕掉 NO. ${pad2(item.data.no)} 这张便签？\n这面墙上的所有人都将看不到它。`)) return;
    try {
      await api('DELETE', `/api/clips/${item.data.id}`);
      removeClip(item.data.id, true);
    } catch (err) {
      if (!(err instanceof AuthError)) toast(err.message);
    }
  }

  function openLightbox(id) {
    els.lightboxImg.src = `/api/clips/${id}/image`;
    els.lightbox.hidden = false;
  }
  els.lightbox.addEventListener('click', () => {
    els.lightbox.hidden = true;
    els.lightboxImg.removeAttribute('src');
  });

  // ---------- 查看记录 ----------

  const viewObserver = new IntersectionObserver((entries) => {
    for (const en of entries) {
      if (!en.isIntersecting) continue;
      const id = en.target.dataset.id;
      viewObserver.unobserve(en.target);
      if (!state.viewed.has(id)) {
        state.viewed.add(id);
        state.pendingViews.add(id);
      }
    }
  }, { threshold: 0.4 });

  setInterval(() => {
    if (!state.pendingViews.size || document.visibilityState !== 'visible') return;
    const ids = [...state.pendingViews].filter((id) => !state.own.has(id) && state.clips.has(id));
    state.pendingViews.clear();
    if (ids.length) api('POST', '/api/views', { ids }).catch(() => {});
  }, 1500);

  // ---------- 写新便签 ----------

  let draftSeq = 0;

  // 支持的图片格式且不超过图片上限的贴成图片便签，其他（包括超大图片）按文件分享。
  function addDrafts(files) {
    const { maxImageBytes, maxFileBytes } = state.cfg;
    for (const f of files) {
      const name = f.name || '未命名文件';
      if (!f.size) {
        toast(`${name} 是空文件，已跳过`);
        continue;
      }
      const asImage = IMAGE_TYPES.has(f.type) && f.size <= maxImageBytes;
      if (!asImage && f.size > maxFileBytes) {
        toast(`${name} 超过 ${mb(maxFileBytes)} MB，已跳过`);
        continue;
      }
      if (!asImage && IMAGE_TYPES.has(f.type)) toast(`${name} 超过 ${mb(maxImageBytes)} MB，将作为文件分享`);

      const d = { id: ++draftSeq, file: f, kind: asImage ? 'image' : 'file' };
      let preview;
      if (asImage) {
        d.url = URL.createObjectURL(f);
        preview = h('img', { src: d.url, alt: name });
      } else {
        preview = h('div', { class: 'draft-file' },
          h('span', { class: 'ext', text: extOf(f.name) }),
          h('span', { class: 'name', text: name }),
          h('span', { class: 'size', text: fmtSize(f.size) }));
      }
      d.el = h('div', { class: `draft draft-${d.kind}`, title: name }, preview,
        h('button', { type: 'button', class: 'x', 'aria-label': '移除', text: '×', onclick: () => removeDraft(d) }),
        h('span', { class: 'prog' }));
      state.drafts.push(d);
      els.drafts.append(d.el);
    }
  }

  function removeDraft(d) {
    if (d.url) URL.revokeObjectURL(d.url);
    d.el.remove();
    state.drafts = state.drafts.filter((x) => x !== d);
  }

  function insertText(text) {
    const ta = els.text;
    ta.focus();
    const start = ta.selectionStart ?? ta.value.length;
    const end = ta.selectionEnd ?? ta.value.length;
    ta.setRangeText(text, start, end, 'end');
    autosize();
  }

  function autosize() {
    els.text.style.height = 'auto';
    els.text.style.height = els.text.scrollHeight + 'px';
  }
  els.text.addEventListener('input', autosize);

  async function submit() {
    if (state.sending) return;
    const text = els.text.value;
    const drafts = [...state.drafts];
    if (!text.trim() && !drafts.length) {
      toast('先写点什么，或者粘贴一张图片吧');
      els.text.focus();
      return;
    }
    if (new Blob([text]).size > state.cfg.maxTextBytes) {
      toast(`文字太长了，上限是 ${Math.round(state.cfg.maxTextBytes / 1024)} KB`);
      return;
    }
    state.sending = true;
    els.sendBtn.disabled = true;
    const pinned = els.pinDraft.checked;
    let posted = 0;
    try {
      if (text.trim()) {
        const c = await api('POST', '/api/clips', { content: text, pinned });
        state.own.add(c.id);
        upsert(c);
        els.text.value = '';
        autosize();
        posted++;
      }
      for (const d of drafts) {
        d.el.classList.add('uploading');
        const onProgress = (p) => d.el.style.setProperty('--p', p.toFixed(3));
        const c = d.kind === 'image'
          ? await upload('/api/clips/image', d.file, await makeThumb(d.file).catch(() => null), pinned, onProgress)
          : await upload('/api/clips/file', d.file, null, pinned, onProgress);
        state.own.add(c.id);
        upsert(c);
        removeDraft(d);
        posted++;
      }
      els.pinDraft.checked = false;
      toast(posted > 1 ? `已贴上 ${posted} 张便签` : '已贴上墙');
    } catch (err) {
      for (const d of state.drafts) d.el.classList.remove('uploading');
      if (!(err instanceof AuthError)) toast(err.message);
    } finally {
      state.sending = false;
      els.sendBtn.disabled = false;
    }
  }

  // 大图在浏览器里先缩成 720px 的缩略图一起上传，墙上只加载缩略图。
  async function makeThumb(file) {
    if (file.size < 300 * 1024 || file.type === 'image/gif' || !window.createImageBitmap) return null;
    const bmp = await createImageBitmap(file);
    const scale = Math.min(1, 720 / Math.max(bmp.width, bmp.height));
    const cv = document.createElement('canvas');
    cv.width = Math.max(1, Math.round(bmp.width * scale));
    cv.height = Math.max(1, Math.round(bmp.height * scale));
    const ctx = cv.getContext('2d');
    ctx.fillStyle = '#fff';
    ctx.fillRect(0, 0, cv.width, cv.height);
    ctx.drawImage(bmp, 0, 0, cv.width, cv.height);
    bmp.close();
    return new Promise((res) => cv.toBlob(res, 'image/jpeg', 0.82));
  }

  // 用 XHR 而不是 fetch，是为了拿到上传进度。
  function upload(url, file, thumb, pinned, onProgress) {
    return new Promise((resolve, reject) => {
      const fd = new FormData();
      fd.append('pinned', pinned ? '1' : '0');
      if (thumb) fd.append('thumb', thumb, 'thumb.jpg');
      fd.append('file', file, file.name || 'image.png');
      const xhr = new XMLHttpRequest();
      xhr.open('POST', url);
      xhr.responseType = 'json';
      xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); };
      xhr.onload = () => {
        if (xhr.status === 401) { showGate(); reject(new AuthError('请先输入口令')); return; }
        if (xhr.status >= 200 && xhr.status < 300 && xhr.response) { resolve(xhr.response); return; }
        const msg = xhr.response && xhr.response.error;
        reject(new Error(msg || (xhr.status === 413 ? '文件太大了' : `上传失败（${xhr.status}）`)));
      };
      xhr.onerror = () => reject(new Error(`网络错误，${file.name || '文件'} 上传失败`));
      xhr.send(fd);
    });
  }

  els.sendBtn.addEventListener('click', submit);
  els.pickBtn.addEventListener('click', () => els.fileInput.click());
  els.pickFileBtn.addEventListener('click', () => els.anyFileInput.click());
  for (const input of [els.fileInput, els.anyFileInput]) {
    input.addEventListener('change', () => {
      addDrafts([...input.files]);
      input.value = '';
    });
  }

  // ---------- 粘贴 / 拖拽 / 快捷键 ----------

  document.addEventListener('paste', (e) => {
    if (els.board.hidden) return;
    const dt = e.clipboardData;
    if (!dt) return;

    const files = [];
    for (const it of dt.items || []) {
      if (it.kind === 'file') {
        const f = it.getAsFile();
        if (f) files.push(f);
      }
    }
    if (!files.length) files.push(...(dt.files || []));

    const text = dt.getData('text/plain');
    // Excel / Word 复制单元格时会同时带一张图片，这种情况按文字处理；
    // 但只要剪贴板里有非图片的文件（比如在资源管理器里复制的文件），就按文件处理。
    const onlyImages = files.every((f) => f.type.startsWith('image/'));
    if (text && text.trim() && onlyImages) {
      if (files.length) toast('剪贴板里同时有文字和图片，已按文字粘贴');
      if (e.target !== els.text) {
        e.preventDefault();
        insertText(text);
      }
      return;
    }
    if (files.length) {
      e.preventDefault();
      addDrafts(files);
      els.composer.scrollIntoView({ block: 'nearest', behavior: reduceMotion ? 'auto' : 'smooth' });
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !els.lightbox.hidden) {
      els.lightbox.click();
      return;
    }
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey) && !els.board.hidden) {
      if (e.target === els.text || state.drafts.length) {
        e.preventDefault();
        submit();
      }
    }
  });

  let dragDepth = 0;
  const hasFiles = (e) => e.dataTransfer && [...e.dataTransfer.types].includes('Files');
  document.addEventListener('dragenter', (e) => {
    if (els.board.hidden || !hasFiles(e)) return;
    dragDepth++;
    els.board.classList.add('dragging');
  });
  document.addEventListener('dragleave', (e) => {
    if (!hasFiles(e)) return;
    dragDepth = Math.max(0, dragDepth - 1);
    if (!dragDepth) els.board.classList.remove('dragging');
  });
  document.addEventListener('dragover', (e) => { if (!els.board.hidden && hasFiles(e)) e.preventDefault(); });
  document.addEventListener('drop', (e) => {
    if (els.board.hidden || !hasFiles(e)) return;
    e.preventDefault();
    dragDepth = 0;
    els.board.classList.remove('dragging');
    addDrafts([...e.dataTransfer.files]);
  });

  // 写新便签这张纸也撕一个独一无二的边。
  setClip(els.composer.querySelector('.paper'), tornClip(rng(seedOf('composer'))));
  const gatePaper = document.querySelector('.gate-note .paper');
  if (gatePaper) setClip(gatePaper, tornClip(rng(seedOf('gate'))));

  boot();
})();
