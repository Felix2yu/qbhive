// QBHive 前端主脚本
const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];

const API = window.location.origin + "/api";
const genID = () => Math.random().toString(36).slice(2, 10);

function toast(msg, type = "ok") {
  const el = document.createElement("div");
  el.className = `toast ${type}`;
  el.textContent = msg;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 2500);
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(API + path, opts);
  const data = await resp.json();
  return data;
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
  root.innerHTML = '<div class="empty">加载中…</div>';
  try {
    await views[name](root);
  } catch (e) {
    root.innerHTML = `<div class="empty">出错了：${e.message}</div>`;
  }
}

$$(".tab").forEach(b => b.addEventListener("click", () => switchView(b.dataset.view)));

// ---------- 概览 ----------
async function renderDashboard(root) {
  const t = await api("GET", "/torrents");
  const list = t.data || [];
  const total = list.length;
  const active = list.filter(x => ["downloading", "stalledDL", "metaDL"].includes(x.state)).length;
  const uploading = list.filter(x => x.state === "uploading").length;
  const done = list.filter(x => x.state === "pausedUP" || x.state === "stalledUP" || x.state === "uploading").length;
  const dlSpeed = list.reduce((s, x) => s + (x.dlspeed || 0), 0);
  const upSpeed = list.reduce((s, x) => s + (x.upspeed || 0), 0);

  root.innerHTML = `
    <div class="grid grid-3">
      <div class="card"><div class="stat"><div class="num">${total}</div><div class="lbl">任务总数</div></div></div>
      <div class="card"><div class="stat"><div class="num">${active}</div><div class="lbl">下载中</div></div></div>
      <div class="card"><div class="stat"><div class="num">${uploading}</div><div class="lbl">做种中</div></div></div>
      <div class="card"><div class="stat"><div class="num">${done}</div><div class="lbl">已完成</div></div></div>
      <div class="card"><div class="stat"><div class="num">${humanSpeed(dlSpeed)}</div><div class="lbl">当前下载速度</div></div></div>
      <div class="card"><div class="stat"><div class="num">${humanSpeed(upSpeed)}</div><div class="lbl">当前上传速度</div></div></div>
    </div>

    <div class="card">
      <h2>最近的任务</h2>
      ${torrentTable(list.slice(0, 10), false)}
    </div>
  `;
}

function stateTag(s) {
  const map = {
    downloading: { t: "下载中", c: "var(--accent)" },
    stalledDL:   { t: "下载停滞", c: "var(--warn)" },
    pausedDL:    { t: "暂停下载", c: "var(--text-dim)" },
    pausedUP:    { t: "已完成", c: "var(--success)" },
    stalledUP:   { t: "做种停滞", c: "var(--warn)" },
    uploading:   { t: "做种中", c: "var(--success)" },
    metaDL:      { t: "元数据", c: "var(--accent-2)" },
    checkingDL:  { t: "校验中", c: "var(--accent-2)" },
    errored:     { t: "错误", c: "var(--danger)" },
  };
  const v = map[s] || { t: s, c: "var(--text-dim)" };
  return `<span style="color:${v.c}">● ${v.t}</span>`;
}

function torrentTable(list, withActions) {
  if (!list.length) return '<div class="empty">暂无任务</div>';
  const rows = list.map(t => {
    const pct = (t.progress * 100).toFixed(1);
    return `
      <tr>
        <td><div title="${t.name}">${t.name.length > 40 ? t.name.slice(0, 40) + "…" : t.name}</div>
            <div style="color:var(--text-dim);font-size:11px">${humanSize(t.size)} · ${t.category || "-"}</div>
        </td>
        <td>${stateTag(t.state)}</td>
        <td>
          <div style="display:flex;align-items:center">
            <div class="progress-bar"><div style="width:${pct}%"></div></div>
            <span class="progress-text">${pct}%</span>
          </div>
        </td>
        <td style="white-space:nowrap">${humanSpeed(t.dlspeed)} / ${humanSpeed(t.upspeed)}</td>
        ${withActions ? `
        <td>
          <div class="speed-input">
            <input type="number" min="0" max="102400" placeholder="KB/s" id="limit-${t.hash}" />
            <button class="btn small" data-limit="${t.hash}">应用</button>
          </div>
        </td>` : ""}
      </tr>`;
  }).join("");
  return `<table>
    <thead><tr>
      <th>名称</th><th>状态</th><th>进度</th><th>速度 (下/上)</th>
      ${withActions ? "<th>上传限速 (KB/s, 0 清除)</th>" : ""}
    </tr></thead>
    <tbody>${rows}</tbody></table>`;
}

// ---------- 任务 ----------
async function renderTorrents(root) {
  const t = await api("GET", "/torrents");
  const list = t.data || [];
  root.innerHTML = `
    <div class="card">
      <h2>下载任务 <span class="badge">可配置单任务上传限速</span></h2>
      ${torrentTable(list, true)}
    </div>
  `;
  $$("[data-limit]").forEach(btn => {
    btn.addEventListener("click", async () => {
      const hash = btn.dataset.limit;
      const input = document.getElementById("limit-" + hash);
      const v = parseInt(input.value || "0", 10);
      const r = await api("POST", `/torrents/${hash}/limit`, { uploadLimit: v });
      toast(r.success ? "已应用限速" : (r.message || "失败"), r.success ? "ok" : "err");
    });
  });
}

// ---------- RSS ----------
async function renderRSS(root) {
  const cfg = (await api("GET", "/config")).data;
  const rss = cfg.rss;
  if (!rss.feeds) rss.feeds = [];

  root.innerHTML = `
    <div class="card">
      <h2>RSS 订阅代理</h2>
      <div class="form-row">
        <label class="inline-check"><input type="checkbox" id="rss-enabled" ${rss.enabled ? "checked" : ""}> 启用 RSS 代理</label>
        <div class="actions-bar">
          <div class="form-row" style="margin:0"><label>刷新间隔 (分钟)</label><input type="number" id="rss-interval" value="${rss.interval || 15}" min="1" style="width:90px"/></div>
          <div class="spacer"></div>
          <button class="btn" id="rss-force">立即拉取一次</button>
          <button class="btn primary" id="rss-save">保存配置</button>
          <button class="btn" id="rss-add">+ 新增订阅源</button>
        </div>
      </div>
    </div>

    ${rss.feeds.length === 0 ? `<div class="card"><div class="empty">还没有订阅源，点击"+ 新增订阅源"开始</div></div>` :
      rss.feeds.map((f, fi) => renderFeedBlock(f, fi)).join("")}
  `;

  $("#rss-force").onclick = async () => {
    await api("POST", "/rss/force");
    toast("已触发 RSS 拉取");
  };
  $("#rss-save").onclick = () => saveRSS(rss);
  $("#rss-add").onclick = () => {
    const newFeed = { id: genID(), name: "新订阅源", url: "", enabled: true, rules: [] };
    rss.feeds.push(newFeed);
    renderRSS(root);
  };
  bindFeedEvents(rss);
}

function renderFeedBlock(f, fi) {
  return `
  <div class="card">
    <div class="rule-block">
      <div class="rule-header">
        <input type="checkbox" id="f-${f.id}-enabled" ${f.enabled ? "checked" : ""}>
        <input type="text" id="f-${f.id}-name" value="${f.name}" style="flex:1;margin:0 12px" />
        <button class="btn danger small" data-del-feed="${f.id}">删除</button>
      </div>
      <input type="url" id="f-${f.id}-url" value="${f.url}" placeholder="https://..." />
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
  return `
    <div class="rule-block">
      <div class="rule-header">
        <input type="checkbox" data-rule-enabled="${fid}-${ri}" ${r.enabled !== false ? "checked" : ""}>
        <input type="text" data-rule-name="${fid}-${ri}" value="${r.name || "规则 " + (ri+1)}" style="flex:1;margin:0 12px" />
        <button class="btn danger small" data-del-rule="${fid}-${ri}">删除</button>
      </div>
      <div class="form-row"><label>匹配模式</label>
        <select data-rule-mode="${fid}-${ri}">
          <option value="keyword" ${r.mode !== "regex" ? "selected" : ""}>关键字 (包含/排除)</option>
          <option value="regex" ${r.mode === "regex" ? "selected" : ""}>正则表达式</option>
        </select>
      </div>
      <div class="form-row"><label>匹配（Include）</label><input type="text" data-rule-include="${fid}-${ri}" value="${r.include || ""}" placeholder="${r.mode === "regex" ? "正则，需匹配" : "标题必须包含此关键字"}" /></div>
      <div class="form-row"><label>排除（Exclude）</label><input type="text" data-rule-exclude="${fid}-${ri}" value="${r.exclude || ""}" placeholder="命中此条件的跳过" /></div>
      <div class="form-row"><label>保存路径</label><input type="text" data-rule-path="${fid}-${ri}" value="${r.savePath || ""}" placeholder="qBittorrent 保存路径 (可空)" /></div>
      <div class="form-row"><label>分类 / 标签</label>
        <div style="display:flex;gap:8px;flex:1">
          <input type="text" data-rule-category="${fid}-${ri}" value="${r.category || ""}" placeholder="Category" style="flex:1"/>
          <input type="text" data-rule-tags="${fid}-${ri}" value="${r.tags || ""}" placeholder="Tags (逗号分隔)" style="flex:1"/>
        </div>
      </div>
      <div class="form-row"><label>上传限速 (KB/s)</label><input type="number" data-rule-upload="${fid}-${ri}" value="${r.uploadLimit || 0}" min="0" placeholder="0 = 不限" style="width:150px"/></div>
    </div>`;
}

function bindFeedEvents(rss) {
  $$("[data-del-feed]").forEach(b => b.addEventListener("click", () => {
    const id = b.dataset.delFeed;
    rss.feeds = rss.feeds.filter(f => f.id !== id);
    renderRSS(document.getElementById("content"));
  }));
  $$("[data-add-rule]").forEach(b => b.addEventListener("click", () => {
    const fid = b.dataset.addRule;
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules = feed.rules || [];
    feed.rules.push({ id: genID(), name: "规则 " + (feed.rules.length + 1), enabled: true, mode: "keyword", include: "", exclude: "", savePath: "", category: "", tags: "", uploadLimit: 0 });
    renderRSS(document.getElementById("content"));
  }));
  $$("[data-del-rule]").forEach(b => b.addEventListener("click", () => {
    const [fid, ri] = b.dataset.delRule.split("-");
    const feed = rss.feeds.find(f => f.id === fid);
    feed.rules.splice(parseInt(ri, 10), 1);
    renderRSS(document.getElementById("content"));
  }));
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
          <input type="text" data-lm-name="${i}" value="${r.name}" style="flex:1;margin:0 12px" />
          <button class="btn danger small" data-lm-del="${i}">删除</button>
        </div>
        <div class="form-row"><label>匹配正则</label><input type="text" data-lm-match="${i}" value="${r.match}" placeholder="如 \\.mkv$ 或 4k" /></div>
        <div class="form-row"><label>上传限速 (KB/s)</label><input type="number" data-lm-limit="${i}" value="${r.uploadLimit}" min="0" style="width:150px"/> <div class="hint">0 = 无限制</div></div>
      </div>`).join("");
  // 重新绑定删除按钮（checkbox/input 的 change 不丢值，不需要重绑）
  $$("[data-lm-del]", box).forEach(b => b.addEventListener("click", onLimDel));
}

async function renderSettings(root) {
  const cfg = (await api("GET", "/config")).data;
  _settingsCfg = cfg;

  root.innerHTML = `
    <!-- qBittorrent -->
    <div class="card">
      <h2>qBittorrent 连接</h2>
      <div class="form-row"><label>WebUI 地址</label><input type="url" id="qb-url" value="${cfg.qbittorrent.url}" placeholder="http://192.168.1.10:8080" /></div>
      <div class="form-row"><label>用户名</label><input type="text" id="qb-user" value="${cfg.qbittorrent.username}" /></div>
      <div class="form-row"><label>密码</label><input type="password" id="qb-pass" value="${cfg.qbittorrent.password}" placeholder="已隐藏（留空则不修改）" /></div>
      <div class="form-row"><label>API key</label>
        <div style="flex:1">
          <input type="password" id="qb-apikey" value="${cfg.qbittorrent.apiKey || ''}" placeholder="qBittorrent v5.2.0+ 可用；填写则优先使用，跳过用户名密码" />
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
      <div class="form-row"><label>监听地址</label><input type="text" id="srv-listen" value="${cfg.server.listen}" placeholder=":8088" /></div>
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
          <textarea id="nt-urls" placeholder="每行一个 Apprise URL，例如：\ntelegram://BOT_TOKEN/CHAT_ID\ndiscord://WEBHOOK_ID/WEBHOOK_TOKEN\ngotify://TOKEN@HOST:PORT\nhttps://hooks.slack.com/services/...">${(cfg.notifier.appriseUrls || []).join("\n")}</textarea>
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
    const r = await api("GET", "/torrents");
    const el = $("#qb-status");
    if (r.success) { el.textContent = "qb 已连接 · " + (r.data?.length || 0) + " 任务"; el.className = "status ok"; }
    else { el.textContent = "qb 未连接"; el.className = "status bad"; }
  } catch {
    const el = $("#qb-status");
    el.textContent = "服务异常"; el.className = "status bad";
  }
}

// ---------- 启动 ----------
switchView("dashboard");
updateStatus();
setInterval(updateStatus, 30000);
// 概览/任务页自动刷新
setInterval(() => {
  if (currentView === "dashboard" || currentView === "torrents") switchView(currentView);
}, 15000);
