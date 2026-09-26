// QBHive 前端主脚本
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const API = window.location.origin + "/api";
const genID = () => Math.random().toString(36).slice(2, 10);

// escapeHTML 防 XSS：把外部可控文本安全地嵌入 innerHTML / 属性
function escapeHTML(s) {
  if (s == null) return "";
  return String(s).replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  }[c]));
}

function toast(msg, type = "ok") {
  const el = document.createElement("div");
  el.className = `toast ${type}`;
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 2500);
}
// --- 工具函数 ---
function debounce(fn, wait = 300) {
  let t; return (...args) => { clearTimeout(t); t = setTimeout(() => fn(...args), wait); };
}
function chunkRender(items, perFrame, renderItem, container, done) {
  let i = 0; container.innerHTML = "";
  function step() {
    const frag = document.createDocumentFragment();
    const end = Math.min(i + perFrame, items.length);
    for (; i < end; i++) frag.appendChild(renderItem(items[i], i));
    container.appendChild(frag);
    if (i < items.length) requestAnimationFrame(step);
    else done && done();
  }
  requestAnimationFrame(step);
}
function paginate(total, page, pageSize) {
  const pages = Math.max(1, Math.ceil(total / pageSize));
  page = Math.max(1, Math.min(page, pages));
  return { pages, page, start: (page - 1) * pageSize, end: Math.min(page * pageSize, total) };
}
function pageNav(total, page, pageSize, onChange) {
  const { pages, page: cur } = paginate(total, page, pageSize);
  if (pages <= 1) return "";
  const btn = (label, target, cls = "") =>
    `<button class="btn small ${cls}" ${target === cur ? "disabled" : ""} data-p="${target}">${label}</button>`;
  const parts = [btn("«", 1), btn("‹", cur - 1)];
  // 省略号逻辑：只显示前3、后3、当前页附近1页
  const windowSet = new Set([1, 2, 3, pages - 2, pages - 1, pages, cur - 1, cur, cur + 1].filter(x => x >= 1 && x <= pages));
  let last = 0;
  for (const n of [...windowSet].sort((a, b) => a - b)) {
    if (n > last + 1) parts.push(`<span style="color:var(--text-dim)">…</span>`);
    parts.push(btn(String(n), n, n === cur ? "primary" : ""));
    last = n;
  }
  parts.push(btn("›", cur + 1), btn("»", pages));
  return `<div class="actions-bar" style="justify-content:center;margin-top:12px;gap:4px">${parts.join("")}</div>`;
}


async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: "include" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  let resp = await fetch(API + path, opts);
  if (resp.status === 401) {
    // 需要登录
    const ok = await showLoginOverlay();
    if (!ok) {
      toast("需要 token 才能访问", "err");
      return { success: false, message: "unauthorized" };
    }
    // 登录成功后重试
    resp = await fetch(API + path, opts);
  }
  // 响应可能不是 JSON（如后端 panic 时 gin 返回 500 空 body、反代返回 HTML 错误页），
  // 解析失败必须返回可展示的错误，不能抛异常让页面永远停在"加载中…"
  try {
    return await resp.json();
  } catch {
    return {
      success: false,
      message: resp.ok ? "服务端返回了非 JSON 响应" : `服务端错误 (HTTP ${resp.status})`,
    };
  }
}

// 登录覆盖层：输入 token，成功后 set cookie
function showLoginOverlay() {
  return new Promise(resolve => {
    const existing = document.getElementById("__qbhive_login__");
    if (existing) existing.remove();

    const wrap = document.createElement("div");
    wrap.id = "__qbhive_login__";
    wrap.innerHTML = `
      <div style="position:fixed;inset:0;background:rgba(0,0,0,.7);display:flex;align-items:center;justify-content:center;z-index:99999">
        <div style="background:var(--panel);border:1px solid var(--border);border-radius:10px;padding:24px;min-width:320px">
          <h3 style="margin:0 0 12px">🔒 需要访问 token</h3>
          <p style="color:var(--text-dim);font-size:12px;margin:0 0 12px">
            服务端设置了 <code>QBHIVE_TOKEN</code> 环境变量，请输入对应的访问 token。
          </p>
          <input id="__qbhive_token__" type="password" placeholder="token" style="width:100%;padding:8px;background:var(--panel-2);border:1px solid var(--border);color:var(--text);border-radius:6px;outline:none" />
          <div style="display:flex;justify-content:flex-end;gap:8px;margin-top:12px">
            <button id="__qbhive_cancel__" class="btn">取消</button>
            <button id="__qbhive_ok__" class="btn primary">登录</button>
          </div>
        </div>
      </div>`;
    document.body.appendChild(wrap);
    const input = wrap.querySelector("#__qbhive_token__");
    input.focus();

    const finish = (ok) => { wrap.remove(); resolve(ok); };
    wrap.querySelector("#__qbhive_cancel__").onclick = () => finish(false);
    wrap.querySelector("#__qbhive_ok__").onclick = async () => {
      const token = input.value.trim();
      if (!token) return;
      const r = await fetch(API + "/login", {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token }),
      });
      finish(r.ok);
    };
    input.onkeydown = (e) => { if (e.key === "Enter") wrap.querySelector("#__qbhive_ok__").click(); };
  });
}

function humanSize(n) {
  if (n == null) return "-";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return v.toFixed(v < 10 && i > 0 ? 2 : 1) + " " + units[i];
}

function humanSpeed(bps) {
  if (!bps) return "0 KB/s";
  return humanSize(bps) + "/s";
}

// 前端轻量校验：返回错误消息字符串，空串表示通过
function validateConfigJS(cfg) {
  const c = cfg || {};
  // qB URL
  if (c.qbittorrent && c.qbittorrent.url && c.qbittorrent.url.trim()) {
    const u = c.qbittorrent.url.trim();
    if (!/^https?:\/\//.test(u)) return "qBittorrent URL 必须以 http:// 或 https:// 开头";
  }
  // 间隔字段
  if (c.rss && c.rss.enabled) {
    if ((c.rss.interval || 0) < 1) return "RSS 刷新间隔必须 ≥ 1 分钟";
  }
  if (c.limiter && c.limiter.enabled) {
    if ((c.limiter.interval || 0) < 1) return "限速器刷新间隔必须 ≥ 1 秒";
  }
  if (c.fileManager && c.fileManager.enabled) {
    if ((c.fileManager.scanInterval || 0) < 1) return "文件管理扫描间隔必须 ≥ 1 秒";
  }
  // Apprise URL 协议
  if (c.notifier) {
    const urls = (c.notifier.appriseUrls || []).map(s => (s || "").trim()).filter(Boolean);
    for (const u of urls) {
      if (!u.includes("://")) return `通知 URL 缺少协议前缀: ${u.slice(0, 60)}`;
    }
  }
  // RSS feeds
  if (c.rss) {
    for (const feed of (c.rss.feeds || [])) {
      if (feed.url && feed.url.trim() && !feed.url.trim().startsWith("http")) {
        return `RSS 订阅源 "${feed.name || feed.url}" 的 URL 必须以 http 开头`;
      }
      for (const r of (feed.rules || [])) {
        if (r.mode === "regex") {
          if (r.include && r.include.trim()) {
            try { new RegExp(r.include); } catch (e) {
              return `订阅源 ${feed.name || ""} / 规则 ${r.name || ""} 的 include 正则无效: ${e.message}`;
            }
          }
          if (r.exclude && r.exclude.trim()) {
            try { new RegExp(r.exclude); } catch (e) {
              return `订阅源 ${feed.name || ""} / 规则 ${r.name || ""} 的 exclude 正则无效: ${e.message}`;
            }
          }
        }
      }
    }
  }
  // Limiter rules
  for (const r of ((c.limiter && c.limiter.rules) || [])) {
    if (r.match && r.match.trim()) {
      try { new RegExp(r.match); } catch (e) {
        return `限速规则 "${r.name || ""}" 的 match 正则无效: ${e.message}`;
      }
    }
  }
  return "";
}

// ---------- 路由 ----------
const views = {
  dashboard: renderDashboard,
  torrents: renderTorrents,
  rss: renderRSS,
  settings: renderSettings,
};

let currentView = "dashboard";
async function switchView(name) {
  currentView = name;
  $$(".tab").forEach(b => b.classList.toggle("active", b.dataset.view === name));
  const root = $("#content");
  root.dataset.view = name;
  root.innerHTML = '<div class="empty">加载中…</div>';
  try {
    await views[name](root);
  } catch (e) {
    root.innerHTML = `<div class="empty">出错了：${e.message}</div>`;
  }
}

$$(".tab").forEach(b => b.addEventListener("click", () => switchView(b.dataset.view)));

// ---------- 主题 ----------
// 三种模式：auto（跟随系统）、dark、light；持久化到 localStorage
(function initTheme() {
  const STORAGE_KEY = "qbhive-theme";
  const root = document.documentElement;

  function applyTheme(mode) {
    root.setAttribute("data-theme", mode);
    // 高亮当前选中按钮
    document.querySelectorAll(".theme-switch button").forEach(b => {
      b.classList.toggle("active", b.dataset.themeOpt === mode);
    });
  }

  // 初始化：localStorage 有就用存的；没有默认 auto
  let saved = localStorage.getItem(STORAGE_KEY);
  if (!saved || !["auto", "dark", "light"].includes(saved)) saved = "auto";
  applyTheme(saved);

  // 按钮点击切换
  document.querySelectorAll(".theme-switch button").forEach(b => {
    b.addEventListener("click", () => {
      const mode = b.dataset.themeOpt;
      applyTheme(mode);
      localStorage.setItem(STORAGE_KEY, mode);
    });
  });

  // 系统主题变化时自动响应（仅 auto 模式需要）
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: light)").addEventListener("change", e => {
      if (root.getAttribute("data-theme") === "auto") {
        // CSS @media 已自动切变量，但加个 class 刷新一下也无妨（CSS 纯变量切换不需要额外动作）
      }
    });
  }
})();

// ---------- 概览 ----------
// 防抖：同一时刻只跑一次自动刷新
let _refreshRunning = false;

async function renderDashboard(root) {
  const t = await api("GET", "/torrents/stats");
  const s = t.data || {};
  const active    = s.activeCount || 0;
  const stoppedUp = s.stoppedUP || 0;
  const dlSpeed   = s.dlSpeed || 0;
  const upSpeed   = s.upSpeed || 0;

  root.innerHTML = `
    <div class="grid grid-2">
      <div class="card"><div class="stat"><div class="num">${active}</div><div class="lbl">活跃任务</div></div></div>
      <div class="card"><div class="stat"><div class="num">${stoppedUp}</div><div class="lbl">已完成历史</div></div></div>
      <div class="card"><div class="stat"><div class="num">${humanSpeed(dlSpeed)}</div><div class="lbl">当前下载速度</div></div></div>
      <div class="card"><div class="stat"><div class="num">${humanSpeed(upSpeed)}</div><div class="lbl">当前上传速度</div></div></div>
    </div>
    <div class="card">
      <h2>最近活跃任务</h2>
      <div id="dash-latest"><div style="color:var(--text-dim)">加载中…</div></div>
    </div>
  `;

  // 概览只拉 top 10 active，分块渲避免卡
  const top10 = await api("GET", "/torrents?filter=active&limit=10&sort=added_on&reverse=true");
  const list = top10.data || [];
  if (list.length === 0) {
    $("#dash-latest").innerHTML = '<div class="empty">当前没有活跃任务</div>';
  } else {
    $("#dash-latest").innerHTML = torrentTable(list, false);
  }
}


function stateTag(s) {
  const map = {
    downloading:   { t: "下载中", c: "var(--accent)" },
    stalledDL:     { t: "下载停滞", c: "var(--warn)" },
    stoppedDL:     { t: "暂停下载", c: "var(--text-dim)" },
    stoppedUP:     { t: "已完成", c: "var(--success)" },
    stalledUP:     { t: "做种停滞", c: "var(--warn)" },
    uploading:     { t: "做种中", c: "var(--success)" },
    forcedUP:      { t: "强制做种", c: "var(--success)" },
    queuedUP:      { t: "排队做种", c: "var(--text-dim)" },
    queuedDL:      { t: "排队下载", c: "var(--text-dim)" },
    checkingUP:    { t: "校验中", c: "var(--accent-2)" },
    metaDL:        { t: "元数据", c: "var(--accent-2)" },
    checkingDL:    { t: "校验中", c: "var(--accent-2)" },
    forcedDL:      { t: "强制下载", c: "var(--accent)" },
    allocating:    { t: "分配中", c: "var(--accent-2)" },
    checkingResumeData: { t: "恢复数据校验", c: "var(--accent-2)" },
    missingFiles:  { t: "文件缺失", c: "var(--danger)" },
    errored:       { t: "错误", c: "var(--danger)" },
    error:         { t: "错误", c: "var(--danger)" },
  };
  const v = map[s] || { t: s, c: "var(--text-dim)" };
  return `<span style="color:${v.c}">● ${v.t}</span>`;
}

function torrentTable(list, withActions) {
  if (!list || list.length === 0) return '<div class="empty">暂无任务</div>';
  const rows = list.map(t => torrentRow(t, withActions)).join("");
  return `<table class="torrent-table"><thead><tr>
    <th>名称</th><th>状态</th><th>进度</th><th>大小</th>
    <th>下速</th><th>上速</th><th>分类</th>${withActions ? "<th>限速</th>" : ""}
  </tr></thead><tbody>${rows}</tbody></table>`;
}
function torrentRow(t, withActions) {
  const pct = (t.progress * 100).toFixed(1);
  const safeName = escapeHTML(t.name);
  const shortName = t.name.length > 40 ? escapeHTML(t.name.slice(0, 40)) + "…" : safeName;
  const safeCat = escapeHTML(t.category || "-");
  const safeHash = escapeHTML(t.hash);
  return `<tr>
    <td><div title="${safeName}">${shortName}</div>
        <div style="color:var(--text-dim);font-size:11px">${humanSize(t.size)} · ${safeCat}</div></td>
    <td>${stateTag(t.state)}</td>
    <td><div class="progress-bar"><div style="width:${pct}%"></div></div>${pct}%</td>
    <td>${humanSize(t.downloaded)}/${humanSize(t.size)}</td>
    <td>${humanSpeed(t.dlspeed)}</td>
    <td>${humanSpeed(t.upspeed)}</td>
    <td>${safeCat}</td>
    ${withActions ? `<td><input type="number" id="limit-${safeHash}" style="width:80px" placeholder="KB/s"/>
        <button class="btn small" data-limit="${safeHash}">应用</button></td>` : ""}
  </tr>`;
}
function filterList(list, kw) {
  if (!kw) return list;
  return list.filter(t =>
    (t.name || "").toLowerCase().includes(kw) ||
    (t.category || "").toLowerCase().includes(kw) ||
    (t.tags || "").toLowerCase().includes(kw)
  );
}
function bindLimitButtons() {
  $$("[data-limit]").forEach(btn => {
    if (btn.__bound) return; btn.__bound = true;
    btn.addEventListener("click", async () => {
      const hash = btn.dataset.limit;
      const input = document.getElementById("limit-" + hash);
      const v = parseInt(input.value || "0", 10);
      const r = await api("POST", `/torrents/${hash}/limit`, { uploadLimit: v });
      toast(r.success ? "已应用限速" : (r.message || "失败"), r.success ? "ok" : "err");
    });
  });
}


// ---------- 任务 ----------
// 任务列表状态（模块级，切 filter/limit 不丢）
let _torrentsState = {
  filter: "active",
  limit: 500,
  page: 1,
  pageSize: 50,
  search: "",
  sort: "added_on",
  reverse: "true",
  rawList: [],    // 后端返回的原始列表（未过滤未分页）
};

// 前端友好的窄 filter 选项（带中文标签）
// 值为 qBittorrent v5.0+ 的 torrent state 值，后端会映射到对应的 qB API filter 参数
const torrentFilterOptions = [
  { value: "active",       label: "活跃（下载+做种）" },
  { value: "downloading",  label: "下载中" },
  { value: "stoppedDL",    label: "暂停下载（未完成）" },
  { value: "stoppedUP",    label: "已完成历史（暂停）" },
  { value: "stalledUP",    label: "历史（做种停滞）" },
  { value: "completed",    label: "所有已完成" },
  { value: "all",          label: "全部（⚠️ 可能很慢）" },
];

async function renderTorrents(root) {
  root.innerHTML = `
    <div class="card">
      <h2>下载任务 <span class="badge">可配置单任务上传限速</span></h2>
      <div class="actions-bar" style="margin:12px 0 16px 0;gap:12px;flex-wrap:wrap">
        <div class="form-row" style="margin:0"><label>状态</label>
          <select id="tf-filter" style="min-width:160px">
            ${torrentFilterOptions.map(o =>
              `<option value="${o.value}" ${o.value===_torrentsState.filter?"selected":""}>${o.label}</option>`
            ).join("")}
          </select>
        </div>
        <div class="form-row" style="margin:0"><label>Top</label>
          <select id="tf-limit" style="min-width:100px">
            ${[100, 200, 500, 1000, 0].map(n =>
              `<option value="${n}" ${n===_torrentsState.limit?"selected":""}>${n===0?"全量":n}</option>`
            ).join("")}
          </select>
        </div>
        <div class="form-row" style="margin:0;flex:1"><label>搜索</label>
          <input type="text" id="tf-search" value="${_torrentsState.search}" placeholder="按任务名 / 分类 / 标签过滤…" style="flex:1;min-width:200px" />
        </div>
        <button class="btn small" id="tf-reload">刷新</button>
      </div>
      <div id="tf-progress" style="color:var(--text-dim);font-size:12px;margin-bottom:8px"></div>
      <div id="tf-body"><div style="color:var(--text-dim)">加载中…</div></div>
    </div>
  `;

  // 全量警告
  if (_torrentsState.filter === "all" || _torrentsState.filter === "completed") {
    $("#tf-progress").textContent = _torrentsState.filter === "all"
      ? "⚠️ 全量模式可能加载数千条任务，首次请求会较慢；后端会返回完整列表后再前端分页。"
      : "⚠️ 已完成包含暂停+做种+停滞，数量可能很多；建议用「已完成历史（暂停）」替代。";
  }

  // 事件绑定
  $("#tf-filter").onchange = e => { _torrentsState.filter = e.target.value; _torrentsState.page = 1; fetchTorrents(); };
  $("#tf-limit").onchange  = e => { _torrentsState.limit  = parseInt(e.target.value, 10); _torrentsState.page = 1; fetchTorrents(); };
  $("#tf-reload").onclick  = fetchTorrents;
  $("#tf-search").oninput  = debounce(e => {
    _torrentsState.search = e.target.value.trim().toLowerCase();
    _torrentsState.page = 1;
    renderPage();
  }, 300);

  async function fetchTorrents() {
    $("#tf-progress").textContent = "拉取中…";
    try {
      const t = await api("GET",
        `/torrents?filter=${_torrentsState.filter}&limit=${_torrentsState.limit}&sort=${_torrentsState.sort}&reverse=${_torrentsState.reverse}`);
      if (!t.success) {
        $("#tf-body").innerHTML = `<div style="color:var(--danger)">加载失败：${escapeHTML(t.message || "未知错误")}</div>`;
        $("#tf-progress").textContent = "";
        return;
      }
      _torrentsState.rawList = t.data || [];
      $("#tf-progress").textContent = `已加载 ${_torrentsState.rawList.length} 条`;
      renderPage();
    } catch (e) {
      // 网络中断 / 超时等：给出明确错误，别停留在"拉取中…"
      $("#tf-body").innerHTML = `<div style="color:var(--danger)">加载失败：${escapeHTML(e.message || String(e))}</div>`;
      $("#tf-progress").textContent = "";
    }
  }

  function renderPage() {
    const list = filterList(_torrentsState.rawList, _torrentsState.search);
    const { pages, page, start, end } = paginate(list.length, _torrentsState.page, _torrentsState.pageSize);
    const pageSlice = list.slice(start, end);

    $("#tf-progress").textContent =
      `已加载 ${_torrentsState.rawList.length} 条${_torrentsState.search ? `，搜索命中 ${list.length} 条` : ""} · 显示 ${start + 1}-${Math.min(end, list.length)} / ${list.length}`;

    if (pageSlice.length === 0) {
      $("#tf-body").innerHTML = `<div class="empty">${_torrentsState.search ? "搜索无匹配" : "当前没有任务"}</div>`;
      return;
    }

    // 分块渲染表格，每帧 30 行（表格 DOM 重，不能一次渲完）
    const container = document.createElement("div");
    chunkRender(pageSlice, 30, (tor, idx) => {
      const tr = document.createElement("div");
      tr.innerHTML = torrentTable([tor], true); // 传单个，torrentTable 会包 <table>
      // 抽出里面的 <tbody> 内容（去掉外层 table 包装）
      const tbody = tr.querySelector("tbody");
      return tbody.children[0]; // 直接返回 <tr>
    }, container, () => {
      // 包裹成完整 table
      $("#tf-body").innerHTML = torrentTableHeader(pageSlice.length) + container.innerHTML + pageNav(list.length, page, _torrentsState.pageSize, p => {
        _torrentsState.page = p; renderPage();
      });
      bindLimitButtons();
    });
  }

  fetchTorrents();
}

function torrentTableHeader(count) {
  return `<table class="torrent-table"><thead><tr>
    <th>名称</th><th>状态</th><th>进度</th><th>大小</th>
    <th>下速</th><th>上速</th><th>分类</th>${count > 0 ? "<th>限速</th>" : ""}
  </tr></thead><tbody>`;
}


// ---------- RSS ----------
// rss 参数可传：首次进入页面或显式传 undefined 时从服务器拉；
// 新增/删除/改规则后传本地已修改的 rss，避免覆盖内存中的改动。
let _rssStatusTimer = null;

async function renderRSS(root, rss) {
  if (rss == null) {
    const cfg = (await api("GET", "/config")).data;
    rss = cfg.rss;
  }
  if (!rss.feeds) rss.feeds = [];

  root.innerHTML = `
    <div class="card">
      <h2>RSS 订阅代理</h2>
      <div class="form-row">
        <label class="inline-check"><input type="checkbox" id="rss-enabled" ${rss.enabled ? "checked" : ""}> 启用 RSS 代理</label>
        <div class="actions-bar">
          <div class="form-row" style="margin:0"><label>刷新间隔 (分钟)</label><input type="number" id="rss-interval" value="${rss.interval || 15}" min="1" style="width:90px"/></div>
          <div class="spacer"></div>
          <button class="btn" id="rss-force">立即拉取</button>
          <button class="btn" id="rss-reset-all">全部重新匹配</button>
          <button class="btn primary" id="rss-save">保存</button>
          <button class="btn" id="rss-add">+ 订阅源</button>
        </div>
      </div>
    </div>

    ${rss.feeds.length === 0 ? `<div class="card"><div class="empty">还没有订阅源，点击"+ 新增订阅源"开始</div></div>` :
      rss.feeds.map((f, fi) => renderFeedBlock(f, fi)).join("")}

    <div class="card">
      <h2>📡 运行状态 <span class="badge auto-refresh" id="rss-status-refresh">自动刷新中</span></h2>
      <div id="rss-status-panel"><div class="empty">加载中…</div></div>
    </div>
  `;

  $("#rss-force").onclick = async () => {
    await api("POST", "/rss/force");
    toast("已触发 RSS 拉取");
    refreshRSSStatus(true); // true = 立即拉一次，显示 fetching
  };
  $("#rss-save").onclick = () => saveRSS(rss);
  $("#rss-add").onclick = () => {
    // 先把用户在 DOM 里已填的内容同步回 rss 对象，避免重渲染丢失
    collectRSS(rss);
    const newFeed = { id: genID(), name: "新订阅源", url: "", enabled: true, rules: [] };
    rss.feeds.push(newFeed);
    renderRSS(root, rss);
  };
  $("#rss-reset-all").onclick = async () => {
    if (!confirm("确定让所有订阅源对历史条目重新匹配规则吗？\n已下载过的不会重复下载。")) return;
    const r = await api("POST", "/rss/reset", { feedId: "" });
    toast(r.success ? "已重置，正在重新拉取…" : (r.message || "重置失败"), r.success ? "ok" : "err");
    refreshRSSStatus(true);
  };
  // 事件委托：订阅源状态块里的重置按钮
  $("#rss-status-panel").addEventListener("click", async (e) => {
    const btn = e.target.closest("[data-rss-reset]");
    if (!btn) return;
    const fid = btn.dataset.rssReset;
    const name = btn.dataset.feedName || "该订阅源";
    if (!confirm(`确定让「${name}」对历史条目重新匹配规则吗？\n已下载过的不会重复下载。`)) return;
    const r = await api("POST", "/rss/reset", { feedId: fid });
    toast(r.success ? "已重置，正在重新拉取…" : (r.message || "重置失败"), r.success ? "ok" : "err");
    refreshRSSStatus(true);
  });
  bindFeedEvents(rss);

  // 启动状态自动刷新（先停掉旧的，避免多个 RSS 页并发）
  if (_rssStatusTimer) clearInterval(_rssStatusTimer);
  await refreshRSSStatus(true);
  _rssStatusTimer = setInterval(() => {
    if (currentView !== "rss") { clearInterval(_rssStatusTimer); _rssStatusTimer = null; return; }
    refreshRSSStatus(false);
  }, 5000);
}

function renderFeedBlock(f, fi) {
  const sName = escapeHTML(f.name || "");
  const sUrl  = escapeHTML(f.url || "");
  return `
  <div class="card">
    <div class="rule-block">
      <div class="rule-header">
        <input type="checkbox" id="f-${f.id}-enabled" ${f.enabled ? "checked" : ""}>
        <input type="text" id="f-${f.id}-name" value="${sName}" style="flex:1;margin:0 12px" />
        <button class="btn danger small" data-del-feed="${f.id}">删除</button>
      </div>
      <input type="url" id="f-${f.id}-url" value="${sUrl}" placeholder="https://..." />
    </div>

    <div style="margin-top:14px;display:flex;justify-content:space-between;align-items:center">
      <strong>匹配规则</strong>
      <button class="btn small" data-add-rule="${f.id}">+ 添加规则</button>
    </div>

    ${(!f.rules || f.rules.length === 0) ? `<div class="empty">暂无规则，点击添加</div>` :
      f.rules.map((r, ri) => renderRuleBlock(f.id, r, ri)).join("")}
  </div>`;
}

function renderRuleBlock(fid, r, ri) {
  const sName     = escapeHTML(r.name || ("规则 " + (ri + 1)));
  const sInclude  = escapeHTML(r.include || "");
  const sExclude  = escapeHTML(r.exclude || "");
  const sPath     = escapeHTML(r.savePath || "");
  const sCategory = escapeHTML(r.category || "");
  const sTags     = escapeHTML(r.tags || "");
  return `
    <div class="rule-block">
      <div class="rule-header">
        <input type="checkbox" data-rule-enabled="${fid}-${ri}" ${r.enabled !== false ? "checked" : ""}>
        <input type="text" data-rule-name="${fid}-${ri}" value="${sName}" style="flex:1;margin:0 12px" />
        <button class="btn danger small" data-del-rule="${fid}-${ri}">删除</button>
      </div>
      <div class="form-row"><label>匹配模式</label>
        <select data-rule-mode="${fid}-${ri}">
          <option value="keyword" ${r.mode !== "regex" ? "selected" : ""}>关键字 (包含/排除)</option>
          <option value="regex" ${r.mode === "regex" ? "selected" : ""}>正则表达式</option>
        </select>
      </div>
      <div class="form-row"><label>匹配（Include）</label><textarea wrap="soft" class="auto-h" data-rule-include="${fid}-${ri}" placeholder="${r.mode === "regex" ? "正则，需匹配" : "例: ManoJob 720p|MrLucky（|=OR  空格=AND）"}">${sInclude}</textarea></div>
      <div class="form-row"><label>排除（Exclude）</label><textarea wrap="soft" class="auto-h" data-rule-exclude="${fid}-${ri}" placeholder="例: 1080p|2160p  命中任一跳过">${sExclude}</textarea></div>
      <div class="form-row"><label>保存路径</label><input type="text" data-rule-path="${fid}-${ri}" value="${sPath}" placeholder="qBittorrent 保存路径 (可空)" /></div>
      <div class="form-row"><label>分类 / 标签</label>
        <div style="display:flex;gap:8px;flex:1">
          <input type="text" data-rule-category="${fid}-${ri}" value="${sCategory}" placeholder="Category" style="flex:1"/>
          <input type="text" data-rule-tags="${fid}-${ri}" value="${sTags}" placeholder="Tags (逗号分隔)" style="flex:1"/>
        </div>
      </div>
      <div class="form-row"><label>上传限速 (KB/s)</label><input type="number" data-rule-upload="${fid}-${ri}" value="${r.uploadLimit || 0}" min="0" placeholder="0 = 不限" style="width:150px"/></div>
    </div>`;
}

function bindFeedEvents(rss) {
  $$("[data-del-feed]").forEach(b => b.addEventListener("click", () => {
    // 先同步 DOM 输入 → rss 对象，避免其他正在编辑的 feed 丢值
    collectRSS(rss);
    const id = b.dataset.delFeed;
    rss.feeds = rss.feeds.filter(f => f.id !== id);
    renderRSS(document.getElementById("content"), rss);
  }));
  $$("[data-add-rule]").forEach(b => b.addEventListener("click", () => {
    collectRSS(rss);
    const fid = b.dataset.addRule;
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules = feed.rules || [];
    feed.rules.push({ id: genID(), name: "规则 " + (feed.rules.length + 1), enabled: true, mode: "keyword", include: "", exclude: "", savePath: "", category: "", tags: "", uploadLimit: 0 });
    renderRSS(document.getElementById("content"), rss);
  }));
  $$("[data-del-rule]").forEach(b => b.addEventListener("click", () => {
    collectRSS(rss);
    const [fid, ri] = b.dataset.delRule.split("-");
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules.splice(parseInt(ri, 10), 1);
    renderRSS(document.getElementById("content"), rss);
  }));
}

// RSS 运行状态面板辅助
function fmtTimeAgo(iso) {
  if (!iso) return "从未";
  const t = new Date(iso);
  if (isNaN(t.getTime())) return "未知";
  const diff = Math.max(0, Math.floor((Date.now() - t.getTime()) / 1000));
  if (diff < 5) return "刚刚";
  if (diff < 60) return `${diff} 秒前`;
  const m = Math.floor(diff / 60);
  if (m < 60) return `${m} 分钟前`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} 小时前`;
  const d = Math.floor(h / 24);
  return `${d} 天前`;
}

async function refreshRSSStatus(showLoading) {
  const panel = document.getElementById("rss-status-panel");
  if (!panel) return;
  if (showLoading) panel.innerHTML = '<div style="color:var(--text-dim)">⏳ 拉取中…</div>';
  const res = await api("GET", "/rss/status");
  if (!res.success) {
    panel.innerHTML = `<div style="color:var(--danger)">加载状态失败：${escapeHTML(res.message || "未知错误")}</div>`;
    return;
  }
  panel.innerHTML = renderRSSStatusList(res.data || []);
}

function renderRSSStatusList(list) {
  if (list.length === 0) {
    return '<div class="empty">暂无可监控的订阅源</div>';
  }
  return list.map(s => {
    const statusBadge = renderFeedStatusBadge(s);
    const urlShort = escapeHTML(s.url || "(未填)");
    const seenLine = `累计已见条目 <b>${s.seenCount}</b>`;
    let detailLine = "";
    if (s.lastOk != null) {
      if (s.lastOk) {
        detailLine = `解析 <b>${s.itemCount}</b> 条 · 命中规则 <b>${s.matched}</b> · 下载 <b style="color:var(--success)">${s.downloaded}</b>${s.failed > 0 ? ` · 失败 <b style="color:var(--danger)">${s.failed}</b>` : ""}`;
        if (s.snapshot) detailLine += ` · <span style="color:var(--warn)">快照模式（首次接入）</span>`;
      } else {
        detailLine = `<span style="color:var(--danger)">${escapeHTML(s.lastError || "拉取失败")}</span>`;
      }
    } else {
      detailLine = '<span style="color:var(--text-dim)">尚未拉取过</span>';
    }

    const recent = (s.recentItems || []).map(r => renderRecentItem(r)).join("");
    const recentBlock = recent
      ? `<details open><summary style="cursor:pointer;color:var(--text-dim);font-size:12px;margin-top:6px">最近 ${(s.recentItems || []).length} 条处理记录（点击展开/折叠）</summary>
         <div class="rss-recent">${recent}</div></details>`
      : "";

    return `
    <div class="rss-status-block">
      <div class="rss-status-head">
        <span class="rss-status-name">${escapeHTML(s.feedName)}</span>
        ${statusBadge}
        <span class="rss-status-time">${fmtTimeAgo(s.lastFetchAt)} 拉取 · ${seenLine}</span>
        <button class="btn small" data-rss-reset="${s.feedId}" data-feed-name="${escapeHTML(s.feedName)}" ${s.fetching ? "disabled" : ""} title="清空已见过条目，对所有历史条目重新跑匹配规则">🔄 重新匹配</button>
      </div>
      <div class="rss-status-detail">${detailLine}</div>
      <div class="rss-status-url" title="${urlShort}">${urlShort}</div>
      ${recentBlock}
    </div>`;
  }).join("");
}

function renderFeedStatusBadge(s) {
  if (!s.enabled) return '<span class="badge" style="background:var(--text-dim);color:#fff">已禁用</span>';
  if (s.fetching) return '<span class="badge" style="background:var(--accent);color:#fff">拉取中…</span>';
  if (s.lastOk == null) return '<span class="badge" style="background:var(--text-dim);color:#fff">等待首次</span>';
  if (s.lastOk) return '<span class="badge" style="background:var(--success);color:#fff">✓ 正常</span>';
  return '<span class="badge" style="background:var(--danger);color:#fff">✗ 异常</span>';
}

function renderRecentItem(r) {
  const actionMap = {
    downloaded: { icon: "✅", label: "已提交 qB", cls: "ok" },
    skipped_snapshot: { icon: "⚪", label: "快照跳过", cls: "dim" },
    skipped_no_rule: { icon: "⚪", label: "无规则命中", cls: "dim" },
    failed: { icon: "❌", label: r.error || "失败", cls: "err" },
    seen_duplicate: { icon: "🔁", label: "已见过", cls: "dim" },
  };
  const a = actionMap[r.action] || { icon: "•", label: r.action || "", cls: "dim" };
  return `<div class="rss-recent-item">
    <span class="rss-recent-icon">${a.icon}</span>
    <span class="rss-recent-title">${escapeHTML(r.title)}</span>
    ${r.ruleName ? `<span class="rss-recent-rule">· ${escapeHTML(r.ruleName)}</span>` : ""}
    <span class="rss-recent-time">· ${fmtTimeAgo(r.processedAt)}</span>
    <span class="rss-recent-msg ${a.cls}">— ${a.label}</span>
  </div>`;
}

function collectRSS(rss) {
  rss.enabled = $("#rss-enabled").checked;
  rss.interval = parseInt($("#rss-interval").value || "15", 10);
  rss.feeds.forEach(f => {
    f.enabled = document.getElementById("f-" + f.id + "-enabled").checked;
    f.name = document.getElementById("f-" + f.id + "-name").value;
    f.url  = document.getElementById("f-" + f.id + "-url").value;
    f.rules = f.rules || [];
    f.rules.forEach((r, ri) => {
      const key = `${f.id}-${ri}`;
      r.enabled = document.querySelector(`[data-rule-enabled="${key}"]`).checked;
      r.name = document.querySelector(`[data-rule-name="${key}"]`).value;
      r.mode = document.querySelector(`[data-rule-mode="${key}"]`).value;
      r.include = document.querySelector(`[data-rule-include="${key}"]`).value;
      r.exclude = document.querySelector(`[data-rule-exclude="${key}"]`).value;
      r.savePath = document.querySelector(`[data-rule-path="${key}"]`).value;
      r.category = document.querySelector(`[data-rule-category="${key}"]`).value;
      r.tags = document.querySelector(`[data-rule-tags="${key}"]`).value;
      r.uploadLimit = parseInt(document.querySelector(`[data-rule-upload="${key}"]`).value || "0", 10);
    });
  });
  return rss;
}

async function saveRSS(rss) {
  collectRSS(rss);
  const cfg = (await api("GET", "/config")).data;
  cfg.rss = rss;
  const vmsg = validateConfigJS(cfg);
  if (vmsg) { toast("❌ " + vmsg, "err"); return; }
  const res = await api("POST", "/config", cfg);
  toast(res.success ? "RSS 配置已保存" : (res.message || "保存失败"), res.success ? "ok" : "err");
}

// ---------- 设置 ----------
// settings 页的当前配置对象（模块级，避免每次操作都重建页面丢值）
let _settingsCfg = null;

// 把当前 DOM 里 settings 页所有输入同步回 _settingsCfg
function syncFormToCfg() {
  if (!_settingsCfg) return;
  const c = _settingsCfg;
  c.qbittorrent.url = $("#qb-url").value;
  c.qbittorrent.username = $("#qb-user").value;
  // 掩码值 "********" 不覆盖原值；用户清空密码字段时原样保存（后端会保留原值）
  const pwd = $("#qb-pass").value;
  if (pwd && pwd !== "********") c.qbittorrent.password = pwd;
  const key = $("#qb-apikey").value;
  if (key && key !== "********") c.qbittorrent.apiKey = key;
  c.server.listen = $("#srv-listen").value;
  c.limiter.enabled = $("#lim-enabled").checked;
  c.limiter.interval = parseInt($("#lim-interval").value || "10", 10);
  c.limiter.rules = $$("[data-lm-name]").map(n => {
    const i = n.dataset.lmName;
    return {
      id: c.limiter.rules[i]?.id || genID(),
      enabled: document.querySelector(`[data-lm-enabled="${i}"]`).checked,
      name: n.value,
      match: document.querySelector(`[data-lm-match="${i}"]`).value,
      uploadLimit: parseInt(document.querySelector(`[data-lm-limit="${i}"]`).value || "0", 10),
    };
  });
  c.notifier.enabled = $("#nt-enabled").checked;
  c.notifier.appriseUrls = $("#nt-urls").value.split("\n").map(s => s.trim()).filter(Boolean);
  c.fileManager.enabled = $("#fm-enabled").checked;
  c.fileManager.scanInterval = parseInt($("#fm-interval").value || "15", 10);
}

// 只重渲 limiter 规则那一块（加/删规则时调用，不碰整页）
function renderLimiterRules() {
  if (!_settingsCfg) return;
  const box = $("#limiter-rules");
  if (!box) return;
  const rules = _settingsCfg.limiter.rules || [];
  box.innerHTML = rules.length === 0
    ? `<div class="empty">暂无规则</div>`
    : rules.map((r, i) => `
      <div class="rule-block">
        <div class="rule-header">
          <input type="checkbox" data-lm-enabled="${i}" ${r.enabled !== false ? "checked" : ""}>
          <input type="text" data-lm-name="${i}" value="${escapeHTML(r.name || "")}" style="flex:1;margin:0 12px" />
          <button class="btn danger small" data-lm-del="${i}">删除</button>
        </div>
        <div class="form-row"><label>匹配正则</label><input type="text" data-lm-match="${i}" value="${escapeHTML(r.match || "")}" placeholder="如 \\.mkv$ 或 4k" /></div>
        <div class="form-row"><label>上传限速 (KB/s)</label><input type="number" data-lm-limit="${i}" value="${r.uploadLimit}" min="0" style="width:150px"/> <div class="hint">0 = 无限制</div></div>
      </div>`).join("");
  // 重新绑定删除按钮（checkbox/input 的 change 不丢值，不需要重绑）
  $$("[data-lm-del]", box).forEach(b => b.addEventListener("click", onLimDel));
}

async function renderSettings(root) {
  const cfg = (await api("GET", "/config")).data;
  _settingsCfg = cfg;

  const sQbUrl    = escapeHTML(cfg.qbittorrent.url || "");
  const sQbUser   = escapeHTML(cfg.qbittorrent.username || "");
  const sQbPass   = escapeHTML(cfg.qbittorrent.password || "");
  const sQbKey    = escapeHTML(cfg.qbittorrent.apiKey || "");
  const sListen   = escapeHTML(cfg.server.listen || "");
  const sNotifyUrl = escapeHTML((cfg.notifier.appriseUrls || []).join("\n"));

  root.innerHTML = `
    <!-- qBittorrent -->
    <div class="card">
      <h2>qBittorrent 连接</h2>
      <div class="form-row"><label>WebUI 地址</label><input type="url" id="qb-url" value="${sQbUrl}" placeholder="http://192.168.1.10:8080" /></div>
      <div class="form-row"><label>用户名</label><input type="text" id="qb-user" value="${sQbUser}" /></div>
      <div class="form-row"><label>密码</label><input type="password" id="qb-pass" value="${sQbPass}" placeholder="已隐藏（留空则不修改）" /></div>
      <div class="form-row"><label>API key</label>
        <div style="flex:1">
          <input type="password" id="qb-apikey" value="${sQbKey}" placeholder="qBittorrent v5.2.0+ 可用；填写则优先使用，跳过用户名密码" />
          <div class="hint">在 qBittorrent WebUI → 设置 → WebUI 里生成，形如 <code>qbt_xxxxxxxxxxxxxxxx</code>。填写后直接走 <code>Authorization: Bearer</code>，无额外 round-trip。</div>
        </div>
      </div>
      <div class="actions-bar" style="margin-left:160px">
        <button class="btn" id="qb-test">测试连接</button>
      </div>
    </div>

    <!-- 服务器 -->
    <div class="card">
      <h2>服务监听</h2>
      <div class="form-row"><label>监听地址</label><input type="text" id="srv-listen" value="${sListen}" placeholder=":8088" /></div>
    </div>

    <!-- 限速全局 -->
    <div class="card">
      <h2>限速规则（按名称批量匹配）</h2>
      <div class="form-row"><label class="inline-check"><input type="checkbox" id="lim-enabled" ${cfg.limiter.enabled ? "checked" : ""}> 启用</label>
        <div class="actions-bar">
          <div class="form-row" style="margin:0"><label>刷新间隔 (秒)</label><input type="number" id="lim-interval" value="${cfg.limiter.interval || 10}" min="1" style="width:100px"/></div>
          <div class="spacer"></div>
          <button class="btn" id="lim-add">+ 添加规则</button>
        </div>
      </div>
      <div id="limiter-rules"></div>
    </div>

    <!-- 通知 -->
    <div class="card">
      <h2>完成通知 <span class="badge">Apprise-Go</span></h2>
      <div class="form-row"><label class="inline-check"><input type="checkbox" id="nt-enabled" ${cfg.notifier.enabled ? "checked" : ""}> 启用</label>
        <div style="margin-left:160px"><button class="btn small" id="nt-test">发送测试通知</button></div>
      </div>
      <div class="form-row"><label>通知 URL</label>
        <div style="flex:1">
          <textarea id="nt-urls" placeholder="每行一个 Apprise URL，例如：\ntelegram://BOT_TOKEN/CHAT_ID\ndiscord://WEBHOOK_ID/WEBHOOK_TOKEN\ngotify://TOKEN@HOST:PORT\nhttps://hooks.slack.com/services/...">${sNotifyUrl}</textarea>
          <div class="hint">使用 <a href="https://github.com/unraid/apprise-go" target="_blank">Apprise-Go</a>，原生支持上百种渠道（Telegram / Discord / Slack / 企业微信 / 邮件 / Gotify / Bark ...）。每行填一个 URL，格式参见 <a href="https://github.com/caronc/apprise/wiki" target="_blank">Apprise Wiki</a>。</div>
        </div>
      </div>
    </div>

    <!-- 文件管理 -->
    <div class="card">
      <h2>文件管理（单文件自动归档）</h2>
      <div class="form-row"><label class="inline-check"><input type="checkbox" id="fm-enabled" ${cfg.fileManager.enabled ? "checked" : ""}> 启用</label>
        <div class="form-row" style="margin:0"><label>扫描间隔 (秒)</label><input type="number" id="fm-interval" value="${cfg.fileManager.scanInterval || 15}" min="1" style="width:100px"/></div>
      </div>
      <div style="color:var(--text-dim);font-size:12px;margin-left:160px">完成的下载如果 savePath/torrentName/ 下只有一个文件，会自动上移并清理空目录。</div>
    </div>

    <!-- 保存 -->
    <div class="card">
      <div class="actions-bar" style="justify-content:flex-end">
        <button class="btn primary" id="save-all">保存全部设置</button>
      </div>
    </div>
  `;

  // 首次渲染 limiter 规则
  renderLimiterRules();

  // --- 事件 ---
  $("#qb-test").onclick = async () => {
    const r = await api("POST", "/test-qb", {
      url: $("#qb-url").value, username: $("#qb-user").value,
      password: $("#qb-pass").value, apiKey: $("#qb-apikey").value,
    });
    toast(r.success ? "连接成功 ✓" : (r.message || "连接失败"), r.success ? "ok" : "err");
    if (r.success) updateStatus("ok", "qb 已连接");
  };

  $("#lim-add").onclick = () => {
    syncFormToCfg();
    _settingsCfg.limiter.rules.push({ id: genID(), name: `规则 ${(_settingsCfg.limiter.rules.length + 1)}`, enabled: true, match: "", uploadLimit: 0 });
    renderLimiterRules();
  };

  $$("[data-lm-del]").forEach(b => b.addEventListener("click", onLimDel));

  $("#nt-test").onclick = async () => {
    syncFormToCfg();
    const urls = _settingsCfg.notifier.appriseUrls;
    if (urls.length === 0) { toast("请先填写通知 URL", "err"); return; }
    toast("正在发送测试通知...");
    const r = await api("POST", "/notify/test", { enabled: _settingsCfg.notifier.enabled, appriseUrls: urls });
    toast(r.message || (r.success ? "测试通知已发送 ✓" : "失败"), r.success ? "ok" : "err");
  };

  $("#save-all").onclick = async () => {
    syncFormToCfg();
    const out = _settingsCfg; // 掩码值已在 syncFormToCfg 里被忽略，后端会保留原密码/apiKey
    const vmsg = validateConfigJS(out);
    if (vmsg) { toast("❌ " + vmsg, "err"); return; }
    const res = await api("POST", "/config", out);
    toast(res.success ? "设置已保存" : (res.message || "保存失败"), res.success ? "ok" : "err");
    // 保存成功后重新 GET 一次，让密码/API key 回到掩码状态
    if (res.success) {
      const fresh = (await api("GET", "/config")).data;
      _settingsCfg = fresh;
    }
  };
}

function onLimDel() {
  syncFormToCfg();
  const idx = parseInt(this.dataset.lmDel, 10);
  _settingsCfg.limiter.rules.splice(idx, 1);
  renderLimiterRules();
}
// ---------- 状态 ----------
async function updateStatus() {
  try {
    const r = await api("GET", "/torrents/stats");
    const el = $("#qb-status");
    if (r.success) {
      const s = r.data || {};
      el.textContent = "qb 已连接 · 活跃 " + (s.activeCount || 0) + " · 历史 " + (s.stoppedUP || 0);
      el.className = "status ok";
    } else {
      el.textContent = "qb 未连接"; el.className = "status bad";
    }
  } catch {
    const el = $("#qb-status");
    el.textContent = "服务异常"; el.className = "status bad";
  }
}


// ---------- 启动 ----------
switchView("dashboard");
updateStatus();
setInterval(updateStatus, 30000);
// 概览/任务页自动刷新（防抖：上次请求没回来就跳过；间隔 8s，active 下足够快）
setInterval(async () => {
  if (_refreshRunning) return;
  if (currentView === "dashboard" || currentView === "torrents") {
    _refreshRunning = true;
    try { await switchView(currentView); } finally { _refreshRunning = false; }
  }
}, 8000);
