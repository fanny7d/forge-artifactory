const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => [...root.querySelectorAll(selector)];

const STRICT_SEMVER = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/;
const PLATFORM_OPTIONS = {
  os: [
    ["linux", "Linux"],
    ["darwin", "macOS"],
    ["windows", "Windows"],
  ],
  arch: [
    ["amd64", "AMD64"],
    ["arm64", "ARM64"],
  ],
};
const BUNDLE_HOOKS = [
  ["preflight", "安装前", "在切换到新版本前执行"],
  ["post-install", "安装后", "新版本就位后执行"],
  ["verify", "验证", "确认新版本可以正常使用"],
];
const DEFAULT_HOOK_TIMEOUT_SECONDS = 30;

const state = {
  token: localStorage.getItem("forge-token") || sessionStorage.getItem("forge-token") || "",
  remember: Boolean(localStorage.getItem("forge-token")),
  products: [],
  publishFiles: [],
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
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
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
  const { json, mutation, idempotency, ...requestOptions } = options;
  const headers = new Headers(requestOptions.headers || {});
  if (state.token) headers.set("Authorization", `Bearer ${state.token}`);
  let body = requestOptions.body;
  if (json !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(json);
  }
  if (idempotency) headers.set("Idempotency-Key", idempotency);
  else if (mutation) headers.set("Idempotency-Key", idempotencyKey(mutation));

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
    if (Array.isArray(result)) {
      items.push(...result);
      break;
    }
    items.push(...(result.items || []));
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
  if (error.status === 403) return "当前凭据无法执行此操作。";
  const request = error.requestId ? `（请求 ${error.requestId}）` : "";
  return `${error.message || "请求失败"}${request}`;
}

function emptyState(title, detail, action = "") {
  return `<div class="empty-state"><strong>${escapeHTML(title)}</strong><span>${escapeHTML(detail)}</span>${action}</div>`;
}

function setPage(title, breadcrumb, actions = "") {
  elements.pageTitle.textContent = title;
  elements.breadcrumb.textContent = breadcrumb;
  elements.pageActions.innerHTML = actions;
  $$("#primary-nav a").forEach((link) => link.classList.add("active"));
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
    location.hash = "#/products";
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
  state.publishFiles = [];
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
    location.hash = "#/products";
    return;
  }
  const route = parts[0] || "products";
  loading();
  try {
    if (route === "products" && parts.length === 1) await renderProductList(version);
    else if (route === "products" && parts.length === 2) await renderProductDetail(parts[1], version);
    else location.hash = "#/products";
  } catch (error) {
    if (version !== state.routeVersion) return;
    if (error.status === 401) {
      logout(errorMessage(error));
      return;
    }
    setPage("加载失败", "CLI 制品库");
    elements.content.innerHTML = emptyState("无法加载页面", errorMessage(error), '<p><button class="button secondary" data-action="retry">重试</button></p>');
  }
}

function isCurrentRoute(version) {
  return version === state.routeVersion;
}

function platformName(os, arch, variant = "") {
  const osNames = { linux: "Linux", darwin: "macOS", windows: "Windows" };
  const archNames = { amd64: "AMD64", arm64: "ARM64" };
  const base = `${osNames[os] || os || "未知系统"} · ${archNames[arch] || arch || "未知架构"}`;
  return variant ? `${base} · ${variant}` : base;
}

function platformValue(platform) {
  if (typeof platform === "string") return platform.replaceAll("/", " · ");
  return platformName(platform?.os, platform?.arch, platform?.variant);
}

function platformBadges(platforms) {
  if (!platforms?.length) return '<span class="muted">尚未发布</span>';
  return `<span class="platform-badges">${platforms.map((platform) => `<span class="badge">${escapeHTML(platformValue(platform))}</span>`).join("")}</span>`;
}

function releasePlatforms(release) {
  return (release?.artifacts || []).map((item) => ({
    os: item.os,
    arch: item.arch,
    variant: item.variant,
  }));
}

function releaseStatus(release, currentVersion) {
  if (release.version === currentVersion) return '<span class="badge success">当前版本</span>';
  const values = {
    published: ["已发布", "success"],
    draft: ["未完成", "info"],
    publishing: ["发布中", "warning"],
    publish_failed: ["发布失败", "danger"],
  };
  const [label, kind] = values[release.state] || [release.state || "未知", ""];
  return `<span class="badge ${kind}">${escapeHTML(label)}</span>`;
}

function renderProductRows(products) {
  if (!products.length) {
    return emptyState(
      "还没有 CLI 制品库",
      "添加第一个 CLI，然后发布它的多平台版本。",
      '<p><button class="button primary" data-action="add-product">添加 CLI</button></p>',
    );
  }
  return `<table>
    <thead><tr><th>CLI</th><th>当前版本</th><th>平台</th><th>最近发布</th><th></th></tr></thead>
    <tbody>${products.map((product) => `
      <tr data-clickable data-href="#/products/${encodeURIComponent(product.slug)}">
        <td><span class="cell-title">${escapeHTML(product.displayName)}</span><span class="cell-subtitle mono">${escapeHTML(product.commandName)}</span></td>
        <td>${product.currentVersion ? `<span class="version-value mono">${escapeHTML(product.currentVersion)}</span>` : '<span class="badge info">待首次发布</span>'}</td>
        <td>${platformBadges(product.platforms)}</td>
        <td>${formatDate(product.publishedAt)}</td>
        <td><div class="row-actions"><a class="button subtle small" href="#/products/${encodeURIComponent(product.slug)}">打开</a></div></td>
      </tr>`).join("")}</tbody>
  </table>`;
}

async function renderProductList(version) {
  setPage("CLI 制品库", "Forge", '<button class="button primary" data-action="add-product">添加 CLI</button>');
  const products = await listAll("/api/v1/products");
  if (!isCurrentRoute(version)) return;
  state.products = products;
  elements.content.innerHTML = `
    <section class="page-intro">
      <div><h2>所有 CLI</h2><p>集中管理每个 CLI 的当前版本、多平台制品和发布历史。</p></div>
      <label class="search-field"><span class="sr-only">搜索 CLI</span><input id="product-search" type="search" placeholder="搜索名称或命令"></label>
    </section>
    <div id="product-list" class="panel table-wrap">${renderProductRows(products)}</div>`;

  $("#product-search")?.addEventListener("input", (event) => {
    const query = event.target.value.trim().toLocaleLowerCase();
    const filtered = query
      ? products.filter((product) => `${product.displayName} ${product.slug} ${product.commandName}`.toLocaleLowerCase().includes(query))
      : products;
    $("#product-list").innerHTML = renderProductRows(filtered);
  });
}

function installCommand(product) {
  const path = `/i/${encodeURIComponent(product.installKey)}/${encodeURIComponent(product.slug)}/install`;
  return `curl -fsSL ${location.origin}${path} | sh`;
}

function currentArtifacts(release) {
  if (!release?.artifacts?.length) {
    return emptyState("当前版本没有可见制品", "请重新发布该版本或检查制品状态。");
  }
  return `<div class="artifact-list">${release.artifacts.map((item) => `
    <article class="artifact-item">
      <div>
        <strong>${escapeHTML(platformName(item.os, item.arch, item.variant))}</strong>
        <div class="artifact-meta">${escapeHTML(item.artifact?.filename || item.artifact?.path || "-")} · ${formatBytes(item.artifact?.size)}</div>
      </div>
      <code class="checksum" title="${escapeHTML(item.artifact?.sha256 || "")}">${escapeHTML((item.artifact?.sha256 || "").slice(0, 12))}${item.artifact?.sha256 ? "…" : ""}</code>
    </article>`).join("")}</div>`;
}

function releaseHistory(product, releases, currentVersion) {
  if (!releases.length) return emptyState("还没有版本", "发布第一个版本后会显示在这里。");
  return `<table>
    <thead><tr><th>版本</th><th>状态</th><th>平台</th><th>大小</th><th>发布时间</th><th></th></tr></thead>
    <tbody>${releases.map((release) => {
      const size = (release.artifacts || []).reduce((total, item) => total + Number(item.artifact?.size || 0), 0);
      let action = "";
      if (release.state === "published" && release.version !== currentVersion) {
        action = `<button class="button subtle small" data-action="set-current" data-slug="${escapeHTML(product.slug)}" data-version="${escapeHTML(release.version)}">设为当前</button>`;
      } else if (release.state === "draft") {
        action = `<button class="button danger small" data-action="cancel-draft" data-slug="${escapeHTML(product.slug)}" data-version="${escapeHTML(release.version)}">删除草稿</button>`;
      }
      return `<tr>
        <td><span class="cell-title mono">${escapeHTML(release.version)}</span></td>
        <td>${releaseStatus(release, currentVersion)}</td>
        <td>${platformBadges(releasePlatforms(release))}</td>
        <td>${formatBytes(size)}</td>
        <td>${formatDate(release.publishedAt || release.createdAt)}</td>
        <td><div class="row-actions">${action}</div></td>
      </tr>`;
    }).join("")}</tbody>
  </table>`;
}

async function renderProductDetail(slug, version) {
  const product = await api(`/api/v1/products/${encodeURIComponent(slug)}`);
  const releaseBase = `/api/v1/repositories/${encodeURIComponent(product.repository)}/packages/${encodeURIComponent(product.package)}/releases`;
  const releases = await listAll(releaseBase);
  let currentRelease = releases.find((release) => release.version === product.currentVersion) || null;
  if (product.currentVersion && !currentRelease) {
    currentRelease = await api(`${releaseBase}/${encodeURIComponent(product.currentVersion)}`);
  }
  if (!isCurrentRoute(version)) return;
  const command = installCommand(product);

  setPage(
    product.displayName,
    `CLI 制品库 / ${product.displayName}`,
    `<button class="button primary" data-action="publish-product" data-slug="${escapeHTML(product.slug)}">发布新版本</button>`,
  );
  elements.content.innerHTML = `
    ${product.description ? `<p class="product-description">${escapeHTML(product.description)}</p>` : ""}
    <section class="detail-grid">
      <article class="panel current-release">
        <header><div><p class="eyebrow">当前版本</p><h2 class="mono">${escapeHTML(product.currentVersion || "尚未发布")}</h2></div>${product.currentVersion ? '<span class="badge success">可升级</span>' : '<span class="badge info">待发布</span>'}</header>
        <div class="current-meta"><span>发布时间：${formatDate(product.publishedAt || currentRelease?.publishedAt)}</span><span>命令：<code>${escapeHTML(product.commandName)}</code></span></div>
        ${product.currentVersion ? currentArtifacts(currentRelease) : emptyState("还没有可安装版本", "点击“发布新版本”上传第一个多平台版本。")}
      </article>
      <aside class="panel install-panel">
        <div><p class="eyebrow">安装命令</p><h2>复制后即可安装或升级</h2><p>脚本会自动选择当前系统对应的最新稳定版本。</p></div>
        <div class="command-box"><code id="install-command">${escapeHTML(command)}</code><button class="button secondary small" data-action="copy-install" data-copy-target="install-command">复制</button></div>
      </aside>
    </section>
    <section class="section">
      <div class="section-header"><div><h2>版本历史</h2><p>${releases.length} 个版本，已发布内容不可修改。</p></div></div>
      <div class="panel table-wrap">${releaseHistory(product, releases, product.currentVersion)}</div>
    </section>`;
}

function slugify(value) {
  return String(value || "")
    .normalize("NFKD")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64);
}

function addProduct() {
  openDialog({
    eyebrow: "新的 CLI",
    title: "添加 CLI 制品库",
    submit: "添加并继续",
    body: `
      <div class="field"><label for="product-name">显示名称</label><input id="product-name" name="displayName" maxlength="128" placeholder="Nxin Edge CLI" required></div>
      <div class="form-grid">
        <div class="field"><label for="product-slug">CLI 标识</label><input id="product-slug" name="slug" pattern="[a-z][a-z0-9._-]{1,63}" placeholder="nxin-edge" required><span class="field-help">用于稳定安装地址，创建后不应修改。</span></div>
        <div class="field"><label for="product-command">命令名称</label><input id="product-command" name="commandName" pattern="[A-Za-z0-9][A-Za-z0-9._-]{0,63}" placeholder="nxin-edge" required></div>
      </div>
      <div class="field"><label for="product-description">说明（可选）</label><textarea id="product-description" name="description" maxlength="512" placeholder="这个 CLI 用于什么"></textarea></div>`,
    onSubmit: async (data) => {
      const created = await api("/api/v1/products", {
        method: "POST",
        mutation: "product",
        json: {
          slug: String(data.get("slug")).trim(),
          displayName: String(data.get("displayName")).trim(),
          description: String(data.get("description")).trim(),
          commandName: String(data.get("commandName")).trim(),
        },
      });
      toast(`${created.displayName} 已添加`);
      location.hash = `#/products/${encodeURIComponent(created.slug)}`;
    },
  });

  const name = $("#product-name");
  const slug = $("#product-slug");
  const command = $("#product-command");
  let slugTouched = false;
  let commandTouched = false;
  slug.addEventListener("input", () => {
    slugTouched = true;
    if (!commandTouched) command.value = slug.value;
  });
  command.addEventListener("input", () => { commandTouched = true; });
  name.addEventListener("input", () => {
    if (slugTouched) return;
    slug.value = slugify(name.value);
    if (!commandTouched) command.value = slug.value;
  });
}

function inferredPlatform(filename) {
  const name = filename.toLowerCase();
  let os = "";
  let arch = "";
  if (/(^|[._-])(darwin|macos|mac)([._-]|$)/.test(name)) os = "darwin";
  else if (/(^|[._-])(windows|win)([._-]|$)/.test(name) || name.endsWith(".exe")) os = "windows";
  else if (/(^|[._-])linux([._-]|$)/.test(name)) os = "linux";
  if (/(^|[._-])(arm64|aarch64)([._-]|$)/.test(name)) arch = "arm64";
  else if (/(^|[._-])(amd64|x86_64|x64)([._-]|$)/.test(name)) arch = "amd64";
  return { os, arch };
}

function archiveFormat(filename) {
  const name = filename.toLowerCase();
  if (name.endsWith(".tar.gz") || name.endsWith(".tgz")) return "tar.gz";
  if (name.endsWith(".zip")) return "zip";
  return "raw";
}

function selectOptions(options, selected, placeholder) {
  return `<option value="">${escapeHTML(placeholder)}</option>${options.map(([value, label]) => `<option value="${value}" ${value === selected ? "selected" : ""}>${label}</option>`).join("")}`;
}

function renderBundleHookSettings(index) {
  return `<details class="build-advanced">
    <summary>高级安装设置 <span>可选</span></summary>
    <div class="hook-settings">
      <p class="field-help">仅在填写脚本路径时启用。脚本路径必须是归档内的安全相对路径。</p>
      ${BUNDLE_HOOKS.map(([phase, label, description]) => `
        <section class="hook-row">
          <div class="hook-label"><strong>${label}</strong><span>${description}</span></div>
          <div class="field compact hook-path">
            <label for="build-hook-${phase}-path-${index}">脚本路径</label>
            <input id="build-hook-${phase}-path-${index}" maxlength="1024" placeholder="hooks/${phase}">
          </div>
          <div class="field compact hook-args">
            <label for="build-hook-${phase}-args-${index}">参数</label>
            <input id="build-hook-${phase}-args-${index}" placeholder="--check config/default.yaml">
          </div>
          <div class="field compact hook-timeout">
            <label for="build-hook-${phase}-timeout-${index}">超时（秒）</label>
            <input id="build-hook-${phase}-timeout-${index}" type="number" min="1" max="300" step="1" value="${DEFAULT_HOOK_TIMEOUT_SECONDS}">
          </div>
        </section>`).join("")}
      <p class="field-help">参数按空白拆分，最多 16 个；不解析引号，也不支持单个参数包含空格。</p>
    </div>
  </details>`;
}

function renderBuildRows(product) {
  const container = $("#publish-builds");
  if (!container) return;
  if (!state.publishFiles.length) {
    container.innerHTML = '<p class="upload-hint">选择文件后，在这里确认每个制品对应的平台。</p>';
    return;
  }
  container.innerHTML = state.publishFiles.map((file, index) => {
    const inferred = inferredPlatform(file.name);
    const format = archiveFormat(file.name);
    const entrypoint = `bin/${product.commandName}`;
    return `<article class="build-row">
      <div class="build-file"><strong>${escapeHTML(file.name)}</strong><span>${formatBytes(file.size)} · ${format === "raw" ? "可执行文件" : escapeHTML(format)}</span></div>
      <div class="field compact"><label for="build-os-${index}">系统</label><select id="build-os-${index}" required>${selectOptions(PLATFORM_OPTIONS.os, inferred.os, "选择系统")}</select></div>
      <div class="field compact"><label for="build-arch-${index}">架构</label><select id="build-arch-${index}" required>${selectOptions(PLATFORM_OPTIONS.arch, inferred.arch, "选择架构")}</select></div>
      ${format === "raw" ? "" : `<div class="field build-entrypoint"><label for="build-entrypoint-${index}">归档内入口</label><input id="build-entrypoint-${index}" value="${escapeHTML(entrypoint)}" placeholder="bin/${escapeHTML(product.commandName)}" required><span class="field-help">安装后执行的相对路径。</span></div>`}
      ${format === "raw" ? "" : renderBundleHookSettings(index)}
    </article>`;
  }).join("");
}

function validRelativePath(value) {
  return Boolean(value) &&
    value.length <= 1024 &&
    !value.startsWith("/") &&
    !value.includes("\\") &&
    !value.includes("\0") &&
    value.split("/").every((segment) => segment && segment !== "." && segment !== "..");
}

function collectBundleHooks(filename, index) {
  return BUNDLE_HOOKS.flatMap(([phase, label]) => {
    const path = $(`#build-hook-${phase}-path-${index}`)?.value.trim() || "";
    const rawArgs = $(`#build-hook-${phase}-args-${index}`)?.value.trim() || "";
    if (!path) {
      if (rawArgs) throw new Error(`${filename} 的${label}脚本缺少路径`);
      return [];
    }
    if (!validRelativePath(path)) {
      throw new Error(`${filename} 的${label}脚本必须使用归档内的安全相对路径`);
    }
    if (rawArgs.includes('"') || rawArgs.includes("'")) {
      throw new Error(`${filename} 的${label}参数不支持引号，请直接用空格分隔参数`);
    }
    const args = rawArgs ? rawArgs.split(/\s+/) : [];
    if (args.length > 16) throw new Error(`${filename} 的${label}参数最多 16 个`);
    if (args.some((argument) => !argument || argument.length > 1024 || argument.includes("\0"))) {
      throw new Error(`${filename} 的${label}参数无效或过长`);
    }
    const timeoutValue = $(`#build-hook-${phase}-timeout-${index}`)?.value || "";
    const timeoutSeconds = Number(timeoutValue);
    if (!Number.isInteger(timeoutSeconds) || timeoutSeconds < 1 || timeoutSeconds > 300) {
      throw new Error(`${filename} 的${label}脚本超时必须是 1 到 300 秒的整数`);
    }
    const hook = { phase, path, timeoutSeconds };
    if (args.length) hook.args = args;
    return [hook];
  });
}

function collectPublishSubmission() {
  const version = $("#release-version")?.value.trim() || "";
  if (!STRICT_SEMVER.test(version)) throw new Error("版本号必须是严格 SemVer，例如 1.2.3 或 1.2.3-rc.1");
  if (!state.publishFiles.length) throw new Error("请至少选择一个制品文件");

  const coordinates = new Set();
  const builds = state.publishFiles.map((file, index) => {
    if (!file.size) throw new Error(`${file.name} 是空文件`);
    const os = $(`#build-os-${index}`)?.value || "";
    const arch = $(`#build-arch-${index}`)?.value || "";
    if (!os || !arch) throw new Error(`请为 ${file.name} 选择系统和架构`);
    const coordinate = `${os}/${arch}`;
    if (coordinates.has(coordinate)) throw new Error(`同一版本不能重复上传 ${platformName(os, arch)}`);
    coordinates.add(coordinate);

    const format = archiveFormat(file.name);
    const install = format === "raw"
      ? { strategy: "self-replace", format: "raw", mode: "0755" }
      : {
          strategy: "bundle",
          format,
          entrypoint: $(`#build-entrypoint-${index}`)?.value.trim() || "",
          mode: "0755",
        };
    if (format !== "raw" && !validRelativePath(install.entrypoint)) {
      throw new Error(`${file.name} 的归档内入口必须是安全的相对路径`);
    }
    if (format !== "raw") {
      const hooks = collectBundleHooks(file.name, index);
      if (hooks.length) install.hooks = hooks;
    }
    return { file, os, arch, install };
  });
  return { version, builds };
}

function setPublishControlsDisabled(disabled) {
  $$("input, select, textarea", elements.dialogBody).forEach((control) => { control.disabled = disabled; });
}

function renderPublishProgress(flow) {
  const progress = $("#publish-progress");
  if (!progress) return;
  progress.hidden = false;
  progress.innerHTML = flow.steps.map((step) => `
    <li class="${step.state}">
      <span>${escapeHTML(step.label)}</span>
      <strong>${step.state === "done" ? "完成" : step.state === "active" ? "进行中" : step.state === "failed" ? "失败" : "等待"}</strong>
    </li>`).join("");
}

function setFlowStep(flow, index, stateValue, label) {
  flow.steps[index].state = stateValue;
  if (label) flow.steps[index].label = label;
  renderPublishProgress(flow);
}

function safeArtifactName(filename) {
  const value = filename.normalize("NFKC").replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^-+|-+$/g, "");
  return value || "cli-binary";
}

function artifactPath(product, version, build) {
  return `products/${product.slug}/${version}/${build.os}/${build.arch}/${safeArtifactName(build.file.name)}`;
}

function artifactAPIPath(repository, path) {
  return `/api/v1/repositories/${encodeURIComponent(repository)}/artifacts/${path.split("/").map(encodeURIComponent).join("/")}`;
}

async function sha256(file) {
  const digest = await crypto.subtle.digest("SHA-256", await file.arrayBuffer());
  return [...new Uint8Array(digest)].map((byte) => byte.toString(16).padStart(2, "0")).join("");
}

async function ensureArtifact(product, build, path, checksum) {
  try {
    await api(artifactAPIPath(product.repository, path), {
      method: "PUT",
      headers: {
        "Content-Type": build.file.type || "application/octet-stream",
        "X-Checksum-Sha256": checksum,
      },
      body: build.file,
    });
  } catch (error) {
    if (error.status !== 409) throw error;
    const metadataPath = `/api/v1/repositories/${encodeURIComponent(product.repository)}/metadata/${path.split("/").map(encodeURIComponent).join("/")}`;
    const existing = await api(metadataPath);
    if (existing.sha256 !== checksum || Number(existing.size) !== build.file.size) throw error;
  }
}

async function runPublishFlow(product, submission, flow) {
  if (!product.repository || !product.package) throw new Error("CLI 制品库缺少内部发布坐标");
  const base = `/api/v1/repositories/${encodeURIComponent(product.repository)}/packages/${encodeURIComponent(product.package)}`;
  setFlowStep(flow, 0, "active");
  await api(`${base}/releases`, {
    method: "POST",
    idempotency: `dashboard-publish-${flow.id}-create`,
    json: { version: submission.version },
  });
  setFlowStep(flow, 0, "done");

  for (let index = 0; index < submission.builds.length; index += 1) {
    const stepIndex = index + 1;
    const build = submission.builds[index];
    setFlowStep(flow, stepIndex, "active", `计算 ${build.file.name} 校验值`);
    try {
      const checksum = await sha256(build.file);
      const path = artifactPath(product, submission.version, build);
      setFlowStep(flow, stepIndex, "active", `上传 ${build.file.name}`);
      await ensureArtifact(product, build, path, checksum);
      setFlowStep(flow, stepIndex, "active", `关联 ${platformName(build.os, build.arch)}`);
      await api(`${base}/releases/${encodeURIComponent(submission.version)}/artifacts`, {
        method: "POST",
        idempotency: `dashboard-publish-${flow.id}-attach-${index}`,
        json: {
          artifactPath: path,
          os: build.os,
          arch: build.arch,
          role: "binary",
          install: build.install,
        },
      });
      setFlowStep(flow, stepIndex, "done", `${platformName(build.os, build.arch)} · ${build.file.name}`);
    } catch (error) {
      setFlowStep(flow, stepIndex, "failed", `${platformName(build.os, build.arch)} · ${build.file.name}`);
      throw error;
    }
  }

  const publishIndex = submission.builds.length + 1;
  setFlowStep(flow, publishIndex, "active");
  await api(`${base}/releases/${encodeURIComponent(submission.version)}/publish`, {
    method: "POST",
    idempotency: `dashboard-publish-${flow.id}-publish`,
  });
  setFlowStep(flow, publishIndex, "done");

  const stableIndex = submission.builds.length + 2;
  setFlowStep(flow, stableIndex, "active");
  await api(`${base}/channels/stable/promotions`, {
    method: "POST",
    idempotency: `dashboard-publish-${flow.id}-stable`,
    json: {
      version: submission.version,
      reason: "通过 Forge 控制台发布",
    },
  });
  setFlowStep(flow, stableIndex, "done");
}

function publishProduct(product) {
  const flow = {
    id: crypto.randomUUID ? crypto.randomUUID() : `${Date.now()}-${Math.random().toString(16).slice(2)}`,
    submission: null,
    steps: [],
  };
  openDialog({
    eyebrow: product.displayName,
    title: "发布新版本",
    submit: "发布并设为当前版本",
    body: `
      <div class="field"><label for="release-version">版本号</label><input id="release-version" name="version" placeholder="1.2.3" autocomplete="off" required><span class="field-help">使用严格 SemVer；已发布版本不能覆盖。</span></div>
      <div class="field"><label for="release-files">多平台制品</label><input id="release-files" name="files" type="file" multiple required><span class="field-help">可以一次选择 Linux、macOS 和 Windows 的多个构建文件。</span></div>
      <div id="publish-builds" class="build-list"></div>
      <ol id="publish-progress" class="publish-progress" aria-live="polite" hidden></ol>`,
    onSubmit: async () => {
      if (!flow.submission) {
        flow.submission = collectPublishSubmission();
        flow.steps = [
          { label: `创建版本 ${flow.submission.version}`, state: "pending" },
          ...flow.submission.builds.map((build) => ({ label: `准备 ${build.file.name}`, state: "pending" })),
          { label: "生成并签名版本", state: "pending" },
          { label: "设为当前版本", state: "pending" },
        ];
        setPublishControlsDisabled(true);
        renderPublishProgress(flow);
      }
      elements.dialogSubmit.textContent = "正在发布";
      await runPublishFlow(product, flow.submission, flow);
      toast(`${product.displayName} ${flow.submission.version} 已发布`);
      void renderRoute();
    },
  });
  renderBuildRows(product);
  $("#release-files").addEventListener("change", (event) => {
    state.publishFiles = [...event.target.files];
    renderBuildRows(product);
  });
}

async function copyText(value) {
  if (navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(value);
    return;
  }
  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.setAttribute("readonly", "");
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  document.body.append(textarea);
  textarea.select();
  const copied = document.execCommand("copy");
  textarea.remove();
  if (!copied) throw new Error("复制失败，请手动选择命令");
}

async function handleAction(button) {
  const action = button.dataset.action;
  if (action === "refresh" || action === "retry") return renderRoute();
  if (action === "add-product") return addProduct();
  if (action === "publish-product") {
    const product = await api(`/api/v1/products/${encodeURIComponent(button.dataset.slug)}`);
    return publishProduct(product);
  }
  if (action === "set-current") {
    const product = await api(`/api/v1/products/${encodeURIComponent(button.dataset.slug)}`);
    const base = `/api/v1/repositories/${encodeURIComponent(product.repository)}/packages/${encodeURIComponent(product.package)}`;
    await api(`${base}/channels/stable/promotions`, {
      method: "POST",
      mutation: "set-current",
      json: {
        version: button.dataset.version,
        reason: "通过 Forge 控制台设为当前版本",
      },
    });
    toast(`${product.displayName} ${button.dataset.version} 已设为当前版本`);
    return renderRoute();
  }
  if (action === "cancel-draft") {
    if (!window.confirm(`删除未完成版本 ${button.dataset.version}？已上传的制品不会自动删除。`)) return;
    const product = await api(`/api/v1/products/${encodeURIComponent(button.dataset.slug)}`);
    const path = `/api/v1/repositories/${encodeURIComponent(product.repository)}/packages/${encodeURIComponent(product.package)}/releases/${encodeURIComponent(button.dataset.version)}`;
    await api(path, { method: "DELETE", mutation: "cancel-draft" });
    toast(`草稿 ${button.dataset.version} 已删除`);
    return renderRoute();
  }
  if (action === "copy-install") {
    const target = document.getElementById(button.dataset.copyTarget);
    if (!target) throw new Error("没有找到安装命令");
    await copyText(target.textContent);
    toast("安装命令已复制");
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
    await api("/api/v1/products?limit=1");
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
  if (button) {
    event.preventDefault();
    void handleAction(button).catch(reportActionError);
    return;
  }
  const row = event.target.closest("tr[data-href]");
  if (row && !event.target.closest("a,button")) location.hash = row.dataset.href;
});

checkHealth();
setInterval(checkHealth, 30000);
if (state.token) {
  api("/api/v1/products?limit=1").then(showApp).catch(() => logout("登录状态已失效，请重新输入 Token。"));
} else {
  showLogin();
}
