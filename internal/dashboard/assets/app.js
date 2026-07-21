const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

const state = {
  token: localStorage.getItem("forge-token") || sessionStorage.getItem("forge-token") || "",
  remember: Boolean(localStorage.getItem("forge-token")),
  repositories: [],
  dialogSubmit: null,
  dialogSubmitLabel: "确认",
  dialogBusy: false,
  routeVersion: 0,
};

const elements = {
  login: $("#login-view"),
  app: $("#app"),
  loginForm: $("#login-form"),
  token: $("#token"),
  remember: $("#remember-token"),
  loginError: $("#login-error"),
  content: $("#content"),
  pageTitle: $("#page-title"),
  breadcrumb: $("#breadcrumb"),
  pageActions: $("#page-actions"),
  dialog: $("#dialog"),
  dialogForm: $("#dialog-form"),
  dialogEyebrow: $("#dialog-eyebrow"),
  dialogTitle: $("#dialog-title"),
  dialogBody: $("#dialog-body"),
  dialogError: $("#dialog-error"),
  dialogClose: $("#dialog-close"),
  dialogCancel: $("#dialog-cancel"),
  dialogSubmit: $("#dialog-submit"),
};

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

function formatDate(value) {
  if (!value) return "-";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return String(value);
  return new Intl.DateTimeFormat("zh-CN", {
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", hour12: false,
  }).format(date);
}

function formatBytes(value) {
  const bytes = Number(value || 0);
  if (bytes < 1024) return `${bytes} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let current = bytes / 1024;
  let unit = units[0];
  for (let index = 1; current >= 1024 && index < units.length; index += 1) {
    current /= 1024;
    unit = units[index];
  }
  return `${current.toFixed(current >= 10 ? 1 : 2)} ${unit}`;
}

function idempotencyKey(prefix) {
  const suffix = crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random()}`;
  return `dashboard-${prefix}-${suffix}`;
}

async function api(path, options = {}) {
  const { json, mutation, ...requestOptions } = options;
  const headers = new Headers(options.headers || {});
  if (state.token) headers.set("Authorization", `Bearer ${state.token}`);
  let body = options.body;
  if (json !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(json);
  }
  if (mutation) headers.set("Idempotency-Key", idempotencyKey(mutation));

  const response = await fetch(path, { ...requestOptions, body, headers });
  if (response.status === 204) return null;
  const contentType = response.headers.get("content-type") || "";
  let payload;
  try {
    payload = contentType.includes("json") ? await response.json() : await response.text();
  } catch {
    payload = "";
  }
  if (!response.ok) {
    const details = payload && typeof payload === "object" ? payload : {};
    const message = details.detail || details.title || (typeof payload === "string" && payload.trim()) || `请求失败 (${response.status})`;
    const error = new Error(message);
    error.status = response.status;
    error.code = details.code;
    error.requestId = details.requestId;
    throw error;
  }
  return payload;
}

async function listAll(path, maxPages = 20) {
  const items = [];
  let cursor = null;
  for (let page = 0; page < maxPages; page += 1) {
    const separator = path.includes("?") ? "&" : "?";
    const result = await api(`${path}${separator}limit=100${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);
    items.push(...result.items);
    cursor = result.nextCursor;
    if (!cursor) break;
  }
  return items;
}

function toast(message, kind = "success") {
  const node = document.createElement("div");
  node.className = `toast ${kind === "error" ? "error" : ""}`;
  node.textContent = message;
  $("#toast-region").append(node);
  setTimeout(() => node.remove(), 4200);
}

function errorMessage(error) {
  if (error.status === 401) return "Token 无效或已过期，请重新登录。";
  if (error.status === 403) return "当前 Token 没有执行此操作的权限。";
  const request = error.requestId ? `（请求 ${error.requestId}）` : "";
  return `${error.message || "请求失败"}${request}`;
}

function badge(value) {
  const normalized = String(value || "unknown");
  const kinds = {
    published: "success", success: "success", active: "success",
    draft: "info", publishing: "warning", denied: "danger",
    failure: "danger", publish_failed: "danger", revoked: "danger",
  };
  return `<span class="badge ${kinds[normalized] || ""}">${escapeHTML(normalized)}</span>`;
}

function emptyState(title, detail, action = "") {
  return `<div class="empty-state"><strong>${escapeHTML(title)}</strong><span>${escapeHTML(detail)}</span>${action}</div>`;
}

function setPage(title, breadcrumb, actions = "") {
  elements.pageTitle.textContent = title;
  elements.breadcrumb.textContent = breadcrumb;
  elements.pageActions.innerHTML = actions;
  const route = location.hash.split("/")[1] || "overview";
  $$("#primary-nav a").forEach((link) => link.classList.toggle("active", link.dataset.route === route));
}

function loading() {
  elements.content.innerHTML = '<div class="loading"><span>加载中</span></div>';
}

function showLogin(message = "") {
  closeDialog(true);
  elements.app.hidden = true;
  elements.login.hidden = false;
  elements.token.value = state.token;
  elements.remember.checked = state.remember;
  elements.loginError.textContent = message;
  setTimeout(() => elements.token.focus(), 0);
}

function showApp() {
  elements.login.hidden = true;
  elements.app.hidden = false;
  if (!location.hash) {
    location.hash = "#/overview";
    return;
  }
  void renderRoute();
}

async function checkHealth() {
  const update = (online, text) => {
    for (const id of ["#health-dot", "#login-health-dot"]) {
      $(id).className = `status-dot ${online ? "online" : "offline"}`;
    }
    $("#health-text").textContent = text;
    $("#login-health").textContent = text;
  };
  try {
    const response = await fetch("/readyz");
    update(response.ok, response.ok ? "服务运行正常" : "服务尚未就绪");
  } catch {
    update(false, "无法连接服务");
  }
}

function openDialog({ eyebrow = "", title, body, submit = "确认", onSubmit }) {
  elements.dialogEyebrow.textContent = eyebrow;
  elements.dialogTitle.textContent = title;
  elements.dialogBody.innerHTML = body;
  elements.dialogSubmit.textContent = submit;
  elements.dialogSubmit.disabled = false;
  elements.dialogError.textContent = "";
  state.dialogSubmit = onSubmit;
  state.dialogSubmitLabel = submit;
  state.dialogBusy = false;
  elements.dialog.showModal();
  setTimeout(() => $("input, select, textarea", elements.dialogBody)?.focus(), 0);
}

async function handleDialogSubmit(event) {
  event.preventDefault();
  if (!state.dialogSubmit) return;
  state.dialogBusy = true;
  elements.dialogSubmit.disabled = true;
  elements.dialogClose.disabled = true;
  elements.dialogCancel.disabled = true;
  elements.dialogForm.setAttribute("aria-busy", "true");
  elements.dialogError.textContent = "";
  try {
    const submit = state.dialogSubmit;
    const shouldClose = await submit(new FormData(elements.dialogForm));
    if (shouldClose !== false && elements.dialog.open) elements.dialog.close("submit");
  } catch (error) {
    if (!handleUnauthorized(error)) {
      elements.dialogError.textContent = errorMessage(error);
      elements.dialogSubmit.textContent = state.dialogSubmitLabel;
    }
  } finally {
    state.dialogBusy = false;
    elements.dialogSubmit.disabled = false;
    elements.dialogClose.disabled = false;
    elements.dialogCancel.disabled = false;
    elements.dialogForm.removeAttribute("aria-busy");
  }
}

function closeDialog(force = false) {
  if (state.dialogBusy && !force) return;
  if (elements.dialog.open) elements.dialog.close("cancel");
}

function resetDialog() {
  state.dialogSubmit = null;
  state.dialogSubmitLabel = "确认";
  state.dialogBusy = false;
  elements.dialogForm.reset();
  elements.dialogBody.replaceChildren();
  elements.dialogError.textContent = "";
  elements.dialogSubmit.textContent = "确认";
  elements.dialogSubmit.disabled = false;
  elements.dialogClose.disabled = false;
  elements.dialogCancel.disabled = false;
  elements.dialogForm.removeAttribute("aria-busy");
}

function routeParts() {
  try {
    return location.hash.replace(/^#\/?/, "").split("/").filter(Boolean).map(decodeURIComponent);
  } catch {
    return null;
  }
}

async function renderRoute() {
  const version = ++state.routeVersion;
  closeDialog(true);
  const parts = routeParts();
  if (!parts) {
    location.hash = "#/overview";
    return;
  }
  const route = parts[0] || "overview";
  loading();
  try {
    if (route === "overview" && parts.length === 1) await renderOverview(version);
    else if (route === "repositories" && parts.length === 1) await renderRepositories(version);
    else if (route === "repositories" && parts.length === 2) await renderRepository(parts[1], version);
    else if (route === "repositories" && parts.length === 4 && parts[2] === "packages") await renderPackage(parts[1], parts[3], version);
    else if (route === "repositories" && parts.length === 6 && parts[2] === "packages" && parts[4] === "releases") await renderRelease(parts[1], parts[3], parts[5], version);
    else if (route === "accounts" && parts.length <= 2) await renderAccounts(parts[1], version);
    else if (route === "audit" && parts.length === 1) await renderAudit(version);
    else {
      location.hash = "#/overview";
    }
  } catch (error) {
    if (version !== state.routeVersion) return;
    if (error.status === 401) {
      logout(errorMessage(error));
      return;
    }
    setPage("加载失败", "控制台");
    elements.content.innerHTML = emptyState("无法加载页面", errorMessage(error), '<p><button class="button secondary" data-action="retry">重试</button></p>');
  }
}

function isCurrentRoute(version) {
  return version === state.routeVersion;
}

async function renderOverview(version) {
  setPage("总览", "控制台 / 总览", '<button class="button secondary" data-action="refresh">刷新</button><button class="button primary" data-action="create-repository">新建仓库</button>');
  const repositories = await listAll("/api/v1/repositories");
  state.repositories = repositories;
  const packageGroups = await Promise.all(repositories.map(async (repo) => {
    const packages = await listAll(`/api/v1/repositories/${encodeURIComponent(repo.key)}/packages`);
    return { repo, packages };
  }));
  const packages = packageGroups.flatMap((group) => group.packages);
  const releaseGroups = await Promise.all(packageGroups.flatMap((group) => group.packages.map(async (pkg) => ({
    repo: group.repo.key,
    package: pkg.name,
    releases: await listAll(`/api/v1/repositories/${encodeURIComponent(group.repo.key)}/packages/${encodeURIComponent(pkg.name)}/releases`),
  }))));
  const releases = releaseGroups.flatMap((group) => group.releases.map((release) => ({ ...release, repo: group.repo, package: group.package })));
  let accounts = [];
  let audits = [];
  let canViewAccounts = true;
  try { accounts = await listAll("/api/v1/service-accounts"); } catch (error) { if (error.status === 403) canViewAccounts = false; else throw error; }
  try { audits = await listAll("/api/v1/audit-events", 1); } catch (error) { if (error.status !== 403) throw error; }
  if (!isCurrentRoute(version)) return;
  const published = releases.filter((release) => release.state === "published").length;

  elements.content.innerHTML = `
    <section class="metrics" aria-label="关键指标">
      <div class="metric"><span class="metric-label">仓库</span><strong class="metric-value">${repositories.length}</strong><span class="metric-note">当前 Token 可访问</span></div>
      <div class="metric"><span class="metric-label">Package</span><strong class="metric-value">${packages.length}</strong><span class="metric-note">跨全部可见仓库</span></div>
      <div class="metric"><span class="metric-label">Release</span><strong class="metric-value">${releases.length}</strong><span class="metric-note">${published} 个已发布</span></div>
      <div class="metric"><span class="metric-label">服务账号</span><strong class="metric-value">${canViewAccounts ? accounts.length : "-"}</strong><span class="metric-note">${canViewAccounts ? "管理员可见" : "当前 Token 无管理员权限"}</span></div>
    </section>
    <section class="section">
      <div class="section-header"><div><h2>最近 Release</h2><p>按创建时间查看最新版本状态</p></div><a class="button subtle small" href="#/repositories">查看仓库</a></div>
      <div class="panel table-wrap">${releaseTable(releases.sort((a, b) => new Date(b.createdAt) - new Date(a.createdAt)).slice(0, 8), true)}</div>
    </section>
    <section class="section">
      <div class="section-header"><div><h2>最近活动</h2><p>管理员可查看完整审计记录</p></div><a class="button subtle small" href="#/audit">查看审计</a></div>
      <div class="panel table-wrap">${auditTable(audits.slice(0, 8))}</div>
    </section>`;
}

function releaseTable(releases, showLocation = false) {
  if (!releases.length) return emptyState("还没有 Release", "创建 Release 后会显示在这里。");
  return `<table><thead><tr>${showLocation ? "<th>位置</th>" : ""}<th>版本</th><th>状态</th><th>制品</th><th>创建时间</th><th></th></tr></thead><tbody>${releases.map((release) => `
    <tr data-clickable data-href="#/repositories/${encodeURIComponent(release.repository || release.repo)}/packages/${encodeURIComponent(release.package)}/releases/${encodeURIComponent(release.version)}">
      ${showLocation ? `<td><span class="cell-title">${escapeHTML(release.package)}</span><span class="cell-subtitle">${escapeHTML(release.repository || release.repo)}</span></td>` : ""}
      <td><span class="cell-title mono">${escapeHTML(release.version)}</span></td>
      <td>${badge(release.state)}</td><td>${release.artifacts?.length || 0}</td><td>${formatDate(release.createdAt)}</td>
      <td><div class="row-actions"><a class="button subtle small" href="#/repositories/${encodeURIComponent(release.repository || release.repo)}/packages/${encodeURIComponent(release.package)}/releases/${encodeURIComponent(release.version)}">查看</a></div></td>
    </tr>`).join("")}</tbody></table>`;
}

function auditTable(events) {
  if (!events.length) return emptyState("没有可见的审计事件", "当前 Token 可能没有管理员权限。");
  return `<table><thead><tr><th>结果</th><th>操作</th><th>资源</th><th>时间</th><th>Request ID</th></tr></thead><tbody>${events.map((event) => `
    <tr><td>${badge(event.outcome)}</td><td><span class="cell-title">${escapeHTML(event.action)}</span>${event.code ? `<span class="cell-subtitle">${escapeHTML(event.code)}</span>` : ""}</td>
    <td><span class="cell-title">${escapeHTML(event.resourceType)}</span><span class="cell-subtitle truncate">${escapeHTML(event.resourceId || "-")}</span></td>
    <td>${formatDate(event.createdAt)}</td><td class="mono">${escapeHTML(event.requestId)}</td></tr>`).join("")}</tbody></table>`;
}

async function renderRepositories(version) {
  setPage("仓库", "控制台 / 仓库", '<button class="button primary" data-action="create-repository">新建仓库</button>');
  const repositories = await listAll("/api/v1/repositories");
  if (!isCurrentRoute(version)) return;
  state.repositories = repositories;
  elements.content.innerHTML = `<div class="panel table-wrap">${repositories.length ? `<table><thead><tr><th>仓库</th><th>类型</th><th>创建时间</th><th></th></tr></thead><tbody>${repositories.map((repo) => `
    <tr data-clickable data-href="#/repositories/${encodeURIComponent(repo.key)}"><td><span class="cell-title">${escapeHTML(repo.displayName)}</span><span class="cell-subtitle mono">${escapeHTML(repo.key)}</span></td>
    <td>${badge(repo.type)}</td><td>${formatDate(repo.createdAt)}</td><td><div class="row-actions"><a class="button subtle small" href="#/repositories/${encodeURIComponent(repo.key)}">打开</a></div></td></tr>`).join("")}</tbody></table>` : emptyState("还没有仓库", "创建第一个本地制品仓库开始使用。")}</div>`;
}

async function renderRepository(repoKey, version) {
  const [repo, packages] = await Promise.all([
    api(`/api/v1/repositories/${encodeURIComponent(repoKey)}`),
    listAll(`/api/v1/repositories/${encodeURIComponent(repoKey)}/packages`),
  ]);
  if (!isCurrentRoute(version)) return;
  setPage(repo.displayName, `仓库 / ${repo.key}`, `<button class="button secondary" data-action="upload-artifact" data-repo="${escapeHTML(repo.key)}">上传制品</button><button class="button primary" data-action="create-package" data-repo="${escapeHTML(repo.key)}">新建 Package</button>`);
  elements.content.innerHTML = `
    <div class="split-layout">
      <section><div class="section-header"><div><h2>Packages</h2><p>${packages.length} 个 Package</p></div></div>
        <div class="panel table-wrap">${packages.length ? `<table><thead><tr><th>名称</th><th>Channels</th><th>创建时间</th><th></th></tr></thead><tbody>${packages.map((pkg) => `
          <tr data-clickable data-href="#/repositories/${encodeURIComponent(repo.key)}/packages/${encodeURIComponent(pkg.name)}"><td><span class="cell-title">${escapeHTML(pkg.name)}</span><span class="cell-subtitle mono">${escapeHTML(pkg.id)}</span></td>
          <td>${pkg.channels.map((channel) => `<span class="badge">${escapeHTML(channel)}</span>`).join(" ")}</td><td>${formatDate(pkg.createdAt)}</td><td><div class="row-actions"><a class="button subtle small" href="#/repositories/${encodeURIComponent(repo.key)}/packages/${encodeURIComponent(pkg.name)}">打开</a></div></td></tr>`).join("")}</tbody></table>` : emptyState("还没有 Package", "Package 用于组织同一个软件的多个 Release。")}</div>
      </section>
      <aside class="detail-panel"><header><h2>仓库信息</h2></header><dl class="detail-list">
        <div><dt>Key</dt><dd class="mono">${escapeHTML(repo.key)}</dd></div><div><dt>类型</dt><dd>${escapeHTML(repo.type)}</dd></div>
        <div><dt>ID</dt><dd class="mono">${escapeHTML(repo.id)}</dd></div><div><dt>创建时间</dt><dd>${formatDate(repo.createdAt)}</dd></div>
      </dl></aside>
    </div>`;
}

async function channelState(repo, pkg, channel) {
  try {
    return await api(`/api/v1/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(pkg)}/channels/${channel}`);
  } catch (error) {
    if (error.status === 404) return { name: channel, currentVersion: null };
    throw error;
  }
}

async function renderPackage(repoKey, packageName, version) {
  const base = `/api/v1/repositories/${encodeURIComponent(repoKey)}/packages/${encodeURIComponent(packageName)}`;
  const [pkg, releases, candidate, stable] = await Promise.all([
    api(base), listAll(`${base}/releases`), channelState(repoKey, packageName, "candidate"), channelState(repoKey, packageName, "stable"),
  ]);
  if (!isCurrentRoute(version)) return;
  setPage(pkg.name, `仓库 / ${repoKey} / ${pkg.name}`, `<button class="button secondary" data-action="promote" data-repo="${escapeHTML(repoKey)}" data-package="${escapeHTML(packageName)}">晋级 Channel</button><button class="button primary" data-action="create-release" data-repo="${escapeHTML(repoKey)}" data-package="${escapeHTML(packageName)}">新建 Release</button>`);
  const channelBox = (channel) => `<article class="channel-box"><header><h3>${escapeHTML(channel.name)}</h3>${channel.currentVersion ? badge("active") : badge("empty")}</header><strong class="channel-version mono">${escapeHTML(channel.currentVersion || "未设置")}</strong><div class="channel-meta">当前指向版本</div></article>`;
  elements.content.innerHTML = `
    <section class="channel-grid">${channelBox(candidate)}${channelBox(stable)}</section>
    <section class="section"><div class="section-header"><div><h2>Releases</h2><p>${releases.length} 个版本</p></div></div><div class="panel table-wrap">${releaseTable(releases)}</div></section>`;
}

async function renderRelease(repoKey, packageName, releaseVersion, routeVersion) {
  const base = `/api/v1/repositories/${encodeURIComponent(repoKey)}/packages/${encodeURIComponent(packageName)}/releases/${encodeURIComponent(releaseVersion)}`;
  const release = await api(base);
  if (!isCurrentRoute(routeVersion)) return;
  const actions = release.state === "draft"
    ? `<button class="button secondary" data-action="attach-artifact" data-repo="${escapeHTML(repoKey)}" data-package="${escapeHTML(packageName)}" data-version="${escapeHTML(releaseVersion)}">关联制品</button><button class="button primary" data-action="publish-release" data-repo="${escapeHTML(repoKey)}" data-package="${escapeHTML(packageName)}" data-version="${escapeHTML(releaseVersion)}" ${release.artifacts.length ? "" : "disabled"}>发布</button>`
    : `<button class="button primary" data-action="promote-version" data-repo="${escapeHTML(repoKey)}" data-package="${escapeHTML(packageName)}" data-version="${escapeHTML(releaseVersion)}">晋级版本</button>`;
  setPage(`Release ${release.version}`, `仓库 / ${repoKey} / ${packageName} / ${releaseVersion}`, actions);
  elements.content.innerHTML = `<div class="split-layout"><section>
    <div class="section-header"><div><h2>制品</h2><p>Release 中的不可变文件</p></div></div>
    ${release.artifacts.length ? `<div class="artifact-list">${release.artifacts.map((item) => `<article class="artifact-item"><div><strong class="mono">${escapeHTML(item.artifact.path)}</strong><div class="artifact-meta">${escapeHTML(item.os)}/${escapeHTML(item.arch)}${item.variant ? ` · ${escapeHTML(item.variant)}` : ""} · ${formatBytes(item.artifact.size)}</div></div>${release.state === "draft" ? `<button class="button danger small" data-action="remove-artifact" data-repo="${escapeHTML(repoKey)}" data-package="${escapeHTML(packageName)}" data-version="${escapeHTML(releaseVersion)}" data-id="${escapeHTML(item.id)}">移除</button>` : badge("locked")}</article>`).join("")}</div>` : emptyState("还没有关联制品", "先上传制品，然后将其关联到 Draft Release。")}
  </section><aside class="detail-panel"><header><h2>Release 信息</h2></header><dl class="detail-list">
    <div><dt>状态</dt><dd>${badge(release.state)}</dd></div><div><dt>版本</dt><dd class="mono">${escapeHTML(release.version)}</dd></div>
    <div><dt>创建时间</dt><dd>${formatDate(release.createdAt)}</dd></div><div><dt>发布时间</dt><dd>${formatDate(release.publishedAt)}</dd></div>
    <div><dt>ID</dt><dd class="mono">${escapeHTML(release.id)}</dd></div>${release.failureCode ? `<div><dt>失败代码</dt><dd>${escapeHTML(release.failureCode)}</dd></div>` : ""}
  </dl></aside></div>`;
}

async function renderAccounts(selectedID, version) {
  setPage("服务账号", "控制台 / 服务账号", '<button class="button primary" data-action="create-account">新建账号</button>');
  const [accounts, repositories] = await Promise.all([
    listAll("/api/v1/service-accounts"),
    listAll("/api/v1/repositories"),
  ]);
  if (!isCurrentRoute(version)) return;
  state.repositories = repositories;
  const selected = accounts.find((account) => account.id === selectedID) || accounts[0];
  const tokens = selected ? await listAll(`/api/v1/service-accounts/${selected.id}/tokens`) : [];
  elements.content.innerHTML = `<div class="split-layout"><section><div class="panel table-wrap">${accounts.length ? `<table><thead><tr><th>账号</th><th>创建时间</th><th></th></tr></thead><tbody>${accounts.map((account) => `<tr data-clickable data-href="#/accounts/${account.id}" class="${account.id === selected?.id ? "selected" : ""}"><td><span class="cell-title">${escapeHTML(account.name)}</span><span class="cell-subtitle mono">${escapeHTML(account.id)}</span></td><td>${formatDate(account.createdAt)}</td><td><div class="row-actions"><a class="button subtle small" href="#/accounts/${account.id}">Token</a></div></td></tr>`).join("")}</tbody></table>` : emptyState("还没有服务账号", "创建账号后可为 CI 或客户端签发 Token。")}</div></section>
    <aside class="detail-panel"><header><div class="section-header"><div><h2>${escapeHTML(selected?.name || "Token")}</h2><p>${tokens.length} 个 Token</p></div>${selected ? `<button class="button primary small" data-action="issue-token" data-account="${selected.id}">签发</button>` : ""}</div></header>
      ${selected ? `<div class="artifact-list" style="padding:12px">${tokens.length ? tokens.map((token) => `<article class="artifact-item"><div><strong class="mono">${escapeHTML(token.id)}</strong><div class="artifact-meta">${token.scopes.map(escapeHTML).join(", ")}<br>过期：${formatDate(token.expiresAt)}</div></div>${token.revoked ? badge("revoked") : `<button class="button danger small" data-action="revoke-token" data-id="${token.id}">吊销</button>`}</article>`).join("") : emptyState("没有 Token", "为此账号签发第一个 Token。")}</div>` : emptyState("选择服务账号", "查看和管理账号 Token。")}
    </aside></div>`;
}

async function renderAudit(version) {
  setPage("审计日志", "控制台 / 审计日志", '<button class="button secondary" data-action="refresh">刷新</button>');
  const events = await listAll("/api/v1/audit-events");
  if (!isCurrentRoute(version)) return;
  elements.content.innerHTML = `<div class="panel table-wrap">${auditTable(events)}</div>`;
}

function createRepository() {
  openDialog({ eyebrow: "Repository", title: "新建仓库", submit: "创建仓库", body: `
    <div class="field"><label for="repo-key">仓库 Key</label><input id="repo-key" name="key" pattern="[a-z][a-z0-9._-]{1,63}" placeholder="edge-tools" required><span class="field-help">小写字母开头，可使用数字、点、下划线和连字符。</span></div>
    <div class="field"><label for="repo-name">显示名称</label><input id="repo-name" name="displayName" maxlength="128" placeholder="Edge Tools" required></div>`,
    onSubmit: async (data) => {
      const created = await api("/api/v1/repositories", { method: "POST", mutation: "repository", json: { key: data.get("key"), displayName: data.get("displayName") } });
      toast(`仓库 ${created.key} 已创建`); location.hash = `#/repositories/${encodeURIComponent(created.key)}`;
    },
  });
}

function createPackage(repo) {
  openDialog({ eyebrow: repo, title: "新建 Package", submit: "创建 Package", body: `<div class="field"><label for="package-name">Package 名称</label><input id="package-name" name="name" pattern="[a-z][a-z0-9._-]{1,63}" placeholder="edgecli" required></div>`,
    onSubmit: async (data) => { const created = await api(`/api/v1/repositories/${encodeURIComponent(repo)}/packages`, { method: "POST", mutation: "package", json: { name: data.get("name") } }); toast(`Package ${created.name} 已创建`); location.hash = `#/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(created.name)}`; },
  });
}

function createRelease(repo, pkg) {
  openDialog({ eyebrow: `${repo} / ${pkg}`, title: "新建 Release", submit: "创建 Release", body: `<div class="field"><label for="release-version">版本</label><input id="release-version" name="version" pattern="[A-Za-z0-9][A-Za-z0-9._+\-]{0,127}" placeholder="1.0.0" required></div>`,
    onSubmit: async (data) => { const created = await api(`/api/v1/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(pkg)}/releases`, { method: "POST", mutation: "release", json: { version: data.get("version") } }); toast(`Release ${created.version} 已创建`); location.hash = `#/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(pkg)}/releases/${encodeURIComponent(created.version)}`; },
  });
}

function uploadArtifact(repo) {
  openDialog({ eyebrow: repo, title: "上传制品", submit: "计算校验并上传", body: `
    <div class="field"><label for="artifact-file">文件</label><input id="artifact-file" name="file" type="file" required></div>
    <div class="field"><label for="artifact-path">制品路径</label><input id="artifact-path" name="path" placeholder="linux/arm64/1.0.0/edgecli" required><span class="field-help">路径一旦写入不可修改。</span></div>`,
    onSubmit: async (data) => {
      const file = data.get("file");
      const path = String(data.get("path")).replace(/^\/+/, "");
      if (!file?.size) throw new Error("请选择非空文件");
      elements.dialogSubmit.textContent = "正在计算 SHA-256";
      const digest = await crypto.subtle.digest("SHA-256", await file.arrayBuffer());
      const sha = [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
      elements.dialogSubmit.textContent = "正在上传";
      await api(`/api/v1/repositories/${encodeURIComponent(repo)}/artifacts/${path.split("/").map(encodeURIComponent).join("/")}`, { method: "PUT", headers: { "Content-Type": file.type || "application/octet-stream", "X-Checksum-Sha256": sha }, body: file });
      toast(`制品 ${path} 已上传`); renderRoute();
    },
  });
  $("#artifact-file").addEventListener("change", (event) => {
    const path = $("#artifact-path");
    if (event.target.files[0] && !path.value) path.value = event.target.files[0].name;
  });
}

function attachArtifact(repo, pkg, version) {
  openDialog({ eyebrow: `Release ${version}`, title: "关联制品", submit: "关联", body: `
    <div class="field"><label for="artifact-path">制品路径</label><input id="artifact-path" name="artifactPath" placeholder="linux/arm64/1.0.0/edgecli" required></div>
    <div class="form-grid"><div class="field"><label for="artifact-os">操作系统</label><input id="artifact-os" name="os" value="linux" required></div><div class="field"><label for="artifact-arch">架构</label><input id="artifact-arch" name="arch" value="arm64" required></div></div>
    <div class="form-grid"><div class="field"><label for="artifact-variant">Variant</label><input id="artifact-variant" name="variant"></div><div class="field"><label for="artifact-role">Role</label><input id="artifact-role" name="role"></div></div>`,
    onSubmit: async (data) => {
      const json = { artifactPath: data.get("artifactPath"), os: data.get("os"), arch: data.get("arch") };
      if (data.get("variant")) json.variant = data.get("variant");
      if (data.get("role")) json.role = data.get("role");
      await api(`/api/v1/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(pkg)}/releases/${encodeURIComponent(version)}/artifacts`, { method: "POST", mutation: "release-artifact", json });
      toast("制品已关联到 Release"); renderRoute();
    },
  });
}

function promote(repo, pkg, fixedVersion = "") {
  openDialog({ eyebrow: `${repo} / ${pkg}`, title: "晋级 Channel", submit: "执行晋级", body: `
    <div class="field"><label for="channel">目标 Channel</label><select id="channel" name="channel"><option value="candidate">candidate</option><option value="stable">stable</option></select></div>
    <div class="field"><label for="promotion-version">版本</label><input id="promotion-version" name="version" value="${escapeHTML(fixedVersion)}" placeholder="1.0.0" required></div>
    <div class="field"><label for="promotion-reason">原因</label><textarea id="promotion-reason" name="reason" maxlength="512" placeholder="通过验收，晋级发布" required></textarea></div>`,
    onSubmit: async (data) => { await api(`/api/v1/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(pkg)}/channels/${data.get("channel")}/promotions`, { method: "POST", mutation: "promotion", json: { version: data.get("version"), reason: data.get("reason") } }); toast(`${data.get("channel")} 已指向 ${data.get("version")}`); location.hash = `#/repositories/${encodeURIComponent(repo)}/packages/${encodeURIComponent(pkg)}`; },
  });
}

function createAccount() {
  openDialog({ eyebrow: "Identity", title: "新建服务账号", submit: "创建账号", body: `<div class="field"><label for="account-name">账号名称</label><input id="account-name" name="name" maxlength="128" placeholder="release-publisher" required></div>`,
    onSubmit: async (data) => { const created = await api("/api/v1/service-accounts", { method: "POST", mutation: "service-account", json: { name: data.get("name") } }); toast(`账号 ${created.name} 已创建`); location.hash = `#/accounts/${created.id}`; },
  });
}

function issueToken(accountID) {
  const repositories = state.repositories.length ? state.repositories : [];
  const scopes = ["artifact:read", "artifact:write", "release:publish", "channel:promote", "admin"];
  openDialog({ eyebrow: "一次性 Secret", title: "签发 API Token", submit: "签发 Token", body: `
    <div class="field"><label for="token-expiry">过期时间</label><input id="token-expiry" name="expiresAt" type="datetime-local" required></div>
    <div class="field"><label>Scopes</label><div class="scope-list">${scopes.map((scope) => `<label><input type="checkbox" name="scopes" value="${scope}" ${scope === "artifact:read" ? "checked" : ""}>${scope}</label>`).join("")}</div></div>
    <div class="field"><label for="token-repositories">仓库范围</label><select id="token-repositories" name="repositories" multiple size="${Math.min(5, Math.max(2, repositories.length))}">${repositories.map((repo) => `<option value="${escapeHTML(repo.key)}">${escapeHTML(repo.key)}</option>`).join("")}</select><span class="field-help">Admin Token 可不选择仓库；其他 Token 至少选择一个。</span></div>`,
    onSubmit: async (data) => {
      const expiresAt = new Date(data.get("expiresAt")).toISOString();
      const issued = await api(`/api/v1/service-accounts/${accountID}/tokens`, { method: "POST", mutation: "token", json: { expiresAt, scopes: data.getAll("scopes"), repositories: data.getAll("repositories") } });
      elements.dialogBody.innerHTML = `<div class="field"><label>Token Secret</label><div class="secret-result"><code>${escapeHTML(issued.secret)}</code></div><span class="field-help">Secret 只显示一次，请立即存入安全的密钥管理系统。</span></div>`;
      elements.dialogSubmit.textContent = "完成";
      state.dialogSubmit = async () => { renderRoute(); };
      return false;
    },
  });
  const local = new Date(Date.now() + 24 * 60 * 60 * 1000);
  local.setMinutes(local.getMinutes() - local.getTimezoneOffset());
  $("#token-expiry").value = local.toISOString().slice(0, 16);
}

async function handleAction(button) {
  const action = button.dataset.action;
  if (action === "refresh" || action === "retry") return renderRoute();
  if (action === "create-repository") return createRepository();
  if (action === "create-package") return createPackage(button.dataset.repo);
  if (action === "create-release") return createRelease(button.dataset.repo, button.dataset.package);
  if (action === "upload-artifact") return uploadArtifact(button.dataset.repo);
  if (action === "attach-artifact") return attachArtifact(button.dataset.repo, button.dataset.package, button.dataset.version);
  if (action === "promote") return promote(button.dataset.repo, button.dataset.package);
  if (action === "promote-version") return promote(button.dataset.repo, button.dataset.package, button.dataset.version);
  if (action === "create-account") return createAccount();
  if (action === "issue-token") return issueToken(button.dataset.account);
  if (action === "publish-release") {
    if (!confirm(`确认发布 Release ${button.dataset.version}？发布后内容不可修改。`)) return;
    await runPendingAction(button, async () => {
      await api(`/api/v1/repositories/${encodeURIComponent(button.dataset.repo)}/packages/${encodeURIComponent(button.dataset.package)}/releases/${encodeURIComponent(button.dataset.version)}/publish`, { method: "POST", mutation: "publish" });
      toast(`Release ${button.dataset.version} 已发布`);
      void renderRoute();
    });
  }
  if (action === "remove-artifact") {
    if (!confirm("确认从 Draft Release 移除此制品关联？")) return;
    await runPendingAction(button, async () => {
      await api(`/api/v1/repositories/${encodeURIComponent(button.dataset.repo)}/packages/${encodeURIComponent(button.dataset.package)}/releases/${encodeURIComponent(button.dataset.version)}/artifacts/${button.dataset.id}`, { method: "DELETE", mutation: "remove-artifact" });
      toast("制品关联已移除");
      void renderRoute();
    });
  }
  if (action === "revoke-token") {
    if (!confirm("确认吊销此 Token？该操作不可撤销。")) return;
    await runPendingAction(button, async () => {
      await api(`/api/v1/tokens/${button.dataset.id}/revoke`, { method: "POST", mutation: "revoke-token" });
      toast("Token 已吊销");
      void renderRoute();
    });
  }
}

async function runPendingAction(button, action) {
  if (button.disabled) return;
  button.disabled = true;
  try {
    await action();
  } catch (error) {
    reportActionError(error);
  } finally {
    button.disabled = false;
  }
}

function handleUnauthorized(error) {
  if (error.status !== 401) return false;
  logout(errorMessage(error));
  return true;
}

function reportActionError(error) {
  if (!handleUnauthorized(error)) toast(errorMessage(error), "error");
}

function logout(message = "") {
  state.routeVersion += 1;
  state.token = "";
  localStorage.removeItem("forge-token");
  sessionStorage.removeItem("forge-token");
  showLogin(message);
}

elements.loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  elements.loginError.textContent = "";
  const submit = $("button[type=submit]", elements.loginForm);
  submit.disabled = true;
  state.token = elements.token.value.trim();
  try {
    await api("/api/v1/repositories?limit=1");
    state.remember = elements.remember.checked;
    (state.remember ? localStorage : sessionStorage).setItem("forge-token", state.token);
    (state.remember ? sessionStorage : localStorage).removeItem("forge-token");
    showApp();
  } catch (error) {
    state.token = "";
    elements.loginError.textContent = errorMessage(error);
  } finally {
    submit.disabled = false;
  }
});

$("#toggle-token").addEventListener("click", () => {
  const visible = elements.token.type === "text";
  elements.token.type = visible ? "password" : "text";
  $("#toggle-token").textContent = visible ? "显示" : "隐藏";
});
$("#logout").addEventListener("click", () => logout());
elements.dialogForm.addEventListener("submit", handleDialogSubmit);
elements.dialogClose.addEventListener("click", () => closeDialog());
elements.dialogCancel.addEventListener("click", () => closeDialog());
elements.dialog.addEventListener("cancel", (event) => {
  event.preventDefault();
  closeDialog();
});
elements.dialog.addEventListener("close", resetDialog);
elements.dialog.addEventListener("click", (event) => {
  if (event.target === elements.dialog) closeDialog();
});
document.addEventListener("keydown", (event) => {
  if (event.key === "Escape" && elements.dialog.open) {
    event.preventDefault();
    closeDialog();
  }
});
window.addEventListener("hashchange", () => { if (state.token) void renderRoute(); });
document.addEventListener("click", (event) => {
  const button = event.target.closest("[data-action]");
  if (button) { event.preventDefault(); void handleAction(button).catch(reportActionError); return; }
  const row = event.target.closest("tr[data-href]");
  if (row && !event.target.closest("a,button")) location.hash = row.dataset.href.replace(/^#/, "");
});

checkHealth();
setInterval(checkHealth, 30000);
if (state.token) {
  api("/api/v1/repositories?limit=1").then(showApp).catch(() => logout("登录状态已失效，请重新输入 Token。"));
} else {
  showLogin();
}
