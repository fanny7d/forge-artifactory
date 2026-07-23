# 通用二进制制品库 MVP 设计

日期：2026-07-11

## 1. 建设目标

使用 Go 开发一个单租户、API First 的通用二进制制品库。第一期接入
EdgeCLI，但领域模型、存储结构和接口不能包含 EdgeCLI 专用逻辑，后续应能
接入 `edge-agent` 等其他二进制产品。

系统参考 JFrog Artifactory 的核心思想：调用方操作的是逻辑仓库和逻辑路径，
物理 Blob 存储对调用方透明；二进制内容按校验和寻址，元数据、权限、发布和
晋级通过稳定的 HTTP API 管理。

MVP 不建设 Web 管理控制台。交付内容包括 REST API、OpenAPI 文档、适合 CI
调用的发布流程、Docker 和 Kubernetes 部署资源，以及完整的自动化验收测试。

## 2. MVP 范围

MVP 包含：

- 单租户部署。
- 仅支持 `local/raw` 类型的本地仓库。
- 逻辑制品路径不可变，不允许覆盖发布。
- 流式上传，由服务端计算 SHA-256 和文件大小。
- 当目标 Blob 已存在时支持 Checksum Deploy。
- 基于文件系统内容寻址布局的全局 Blob 去重。
- Package、不可变 Release、签名 Manifest 和 ReleaseArtifact。
- `candidate`、`stable` Channel 及原子晋级历史。
- 服务账号，以及具有 Scope 和仓库范围限制的 API Token。
- Draft Release 的取消（Cancel），不涉及已发布对象。
- 所有写操作及安全敏感操作的审计记录。
- 基于 PostgreSQL 的后台任务，用于 staging 清理和异常恢复。
- 通过 API 鉴权代理下载。
- Docker Compose、Docker 镜像、Helm Chart、指标、健康检查和运维手册。

MVP 不包含：

- Remote Repository 和 Virtual Repository。
- Maven、npm、OCI、Docker Registry、PyPI 等生态专用协议。
- 可变仓库或 Allow Redeploy 模式。
- 已发布 Artifact、Release、ChannelRevision 的删除 API（Draft Release 取消
  不受此限制，见 5.5 节）。
- 多租户、计费、配额和组织管理。
- Web 管理控制台。
- 外部消息队列。

## 3. 总体架构

系统采用 Go 模块化单体，一个二进制完成交付。API 进程和后台 Worker 使用同一
二进制的不同子命令或启动参数，后续可独立扩缩容。

内部模块包括：

- `auth`：服务账号、Token、Scope 和仓库级授权。
- `repository`：`local/raw` 仓库配置和不可变策略。
- `artifact`：逻辑路径、属性、上传、元数据和下载。
- `blob`：文件系统内容寻址存储和去重。
- `release`：Package、Draft Release、ReleaseArtifact、发布和 Manifest。
- `channel`：candidate/stable 晋级和不可变历史。
- `signing`：RFC 8785 Manifest 规范化和 Ed25519 签名。
- `audit`：不可变的操作和安全审计事件。
- `jobs`：基于 PostgreSQL 的清理及恢复任务。
- `api`：REST Handler、中间件、校验、错误协议、指标和 OpenAPI。
- `storage`：持久文件系统适配器。
- `database`：PostgreSQL 事务、迁移和类型安全查询。

PostgreSQL 是元数据、可见性、授权和事务状态的唯一事实来源。持久文件系统保存
Artifact Blob、Manifest 原始字节和 Signature。调用方不感知内容寻址对象路径，
所有操作都通过逻辑仓库和逻辑路径寻址，下载统一经 API 鉴权代理。

后台任务记录在 PostgreSQL 中，通过 `FOR UPDATE SKIP LOCKED` 领取。MVP 不
引入 Kafka、NATS、Redis 或其他消息队列。

## 4. 技术栈与工程结构

- 当前稳定版本的 Go。
- `net/http` + `chi`：HTTP 路由和中间件。
- `pgx/v5`：PostgreSQL 访问和事务。
- `sqlc`：从 SQL 生成类型安全的数据访问代码。
- `goose`：数据库迁移。
- Go 标准库文件系统接口：持久字节存储。
- 标准库 `slog`：结构化日志。
- Prometheus Go Client：指标。
- OpenTelemetry：Tracing 接口，MVP 默认关闭。
- OpenAPI 3.1：HTTP 契约。

系统不使用 ORM，也不引入依赖注入框架。

目录结构：

```text
cmd/artifact-repository/
internal/
  api/
  artifact/
  audit/
  auth/
  blob/
  channel/
  database/
  jobs/
  release/
  repository/
  signing/
  storage/
migrations/
openapi/
deploy/
```

HTTP Handler 只调用领域 Service，不直接执行 SQL 或操作存储文件。事务边界由领域
Service 控制。

## 5. 领域模型

### 5.1 Repository

Repository 包含唯一 `key`、显示名称、固定类型 `local/raw`、创建审计字段和
不可变写入策略。MVP 不提供可变仓库模式。

### 5.2 Blob

Blob 使用全局 SHA-256 标识，记录大小、内部存储路径、状态
`creating|ready|deleting`、Lease 和最近引用时间。只有 `ready` 状态的 Blob
可以被 Artifact、Manifest 或 Signature 新增引用。物理对象采用内容寻址布局：

```text
blobs/sha256/ab/cd/<完整 SHA-256>
```

多个 Artifact 可以引用同一个 Blob。

由于 MVP 不提供已发布对象删除 API，Blob 一旦被任意 Artifact、Manifest 或
Signature 引用即视为永久保留，不需要维护可递减的引用计数。『无引用』状态只
可能出现在上传或发布事务失败的中间态，由第 7 节的清理 Job 处理；清理判定通过
查询实际外键引用完成。Blob 的状态和 Lease 用于在 PostgreSQL 与文件系统之间提供
删除 fencing，不能把跨系统删除描述为单一数据库事务。后续引入删除能力时，必须
增加引用计数（或定期扫描孤儿对象），否则删除 Artifact 会导致仍被其他 Artifact
引用的 Blob 被误删。

### 5.3 Artifact

Artifact 属于一个 Repository，包含规范化逻辑路径、Blob 引用、Media Type、
文件名、JSON 属性、创建人和创建时间。
`(repository_id, logical_path)` 必须唯一。Artifact 一旦可见就永久不可变。

### 5.4 Package

Package 表示可发布产品，例如 `edgecli`。Package 属于一个 Repository，并拥有
Release 和 Channel；名称只在所属 Repository 内唯一，数据库唯一约束为
`(repository_id, name)`。所有 Package、Release 和 Channel API 都在 Repository
路径下寻址，避免同名单 Package 产生路由歧义。Raw Artifact 可以独立存在，不
强制归属 Package。

Release 引用的每个 Artifact 必须属于 Package 所属的同一 Repository；添加跨
Repository 的 Artifact 引用返回 `422 Unprocessable Entity`。该约束是必须的：
Token 的仓库范围限制（见 5.8 节）是唯一的跨仓库隔离机制，如果 Release 允许
引用任意 Repository 下的 Artifact，持有某仓库 `release:publish` Scope 的调用方
就能把仓库白名单之外的 Artifact 内容（路径、SHA-256、大小）间接暴露到自己
可见的 Manifest 中，绕过仓库级授权边界。

### 5.5 Release 与 ReleaseArtifact

Release 以 `(package_id, version)` 唯一，状态机为
`draft -> publishing -> published`，以及两条失败路径
`publishing -> draft`（仅在确认没有可见或待提交的签名结果时中止本次尝试）和
`publishing -> publish_failed`（终态，签名或存储不可重试失败时进入，需人工
介入或调用方放弃并新建版本）。API 请求同步执行发布主流程（第 8 节步骤 1-6）；
Worker 只负责扫描『孤儿』`publishing` Release（持有该发布请求的进程已崩溃、
Lease 已过期）并恢复、完成或终止该尝试，不参与正常路径的发布执行。

Draft 阶段允许添加或移除 ReleaseArtifact（引用的 Artifact 本身仍是不可变
的），一旦进入 `publishing` 即立即冻结 ReleaseArtifact 集合——`publishing`
和 `published` 状态下 `POST .../releases/{version}/artifacts` 一律返回
`409`，以保证 Worker 恢复发布时重建的 Manifest 与首次尝试逐字节一致。

每次从 `draft` 进入 `publishing` 都创建不可变 PublishAttempt，记录随机
`attempt_id`、`published_at`、Idempotency Key、ReleaseArtifact 快照及其 SHA-256、
签名 Key ID、`lease_owner`、`lease_generation` 和 `lease_expires_at`。正常发布进程
定期续租；Worker 接管过期 Lease 时必须递增 `lease_generation`。任何续租、状态
变更和第 8 节步骤 6 的提交都必须使用
`(attempt_id, lease_owner, lease_generation)` 做条件更新，旧持有者失去 Lease 后
只能留下可清理的内容寻址对象，不能再改变 Release 状态。

Worker 若发现 Manifest/Signature 已落盘而状态未提交，则使用持久化快照续做
第 8 节步骤 6；若步骤可重试则继续同一 Attempt；若判定不可重试则转入
`publish_failed`。只有确认没有需要恢复的签名结果时才允许将 Release 回退为
`draft`，同时把本次 Attempt 和对应 Idempotency 记录终结为失败。回退后允许再次
修改 ReleaseArtifact，但必须使用新的 Idempotency Key 创建新的 Attempt，不能把
旧 Key 解释为同一次发布。该机制保证同一 Attempt 的 Manifest 原始字节和 Ed25519
Signature 确定一致，也阻止超时进程覆盖 Worker 或后续 Attempt 的结果。

ReleaseArtifact 记录 Artifact 引用、`os`、`arch`、`variant` 和 `role`。
`variant`、`role` 均定义为 `NOT NULL DEFAULT ''` 的服务端不解释语义的自由
字符串（而非可空列）：PostgreSQL 唯一索引中 `NULL` 彼此不相等，若允许为空
会让『同一 Release 内 `(os, arch, variant, role)` 坐标必须唯一』这一核心约束
被静默突破，进而破坏 Manifest 排序键的确定性。建议约定 `role` 取值包括
`binary`、`archive`、`checksum`、`signature`、`debug-symbols`，`variant`
用于区分同一 `(os, arch)` 下的构建差异（例如 `musl`、`glibc`），留空表示
『无变体』。

Release 发布后，其元数据及所有 Artifact 引用永久冻结。Draft Release 支持
`DELETE /api/v1/repositories/{repo}/packages/{package}/releases/{version}` 取消
（仅允许 `draft` 状态，`publishing`/`published`/`publish_failed` 拒绝），释放
`(package_id, version)` 坐标供重新创建；该操作写入 AuditEvent。这不违反
『已发布对象不可删除』的不可变性承诺——Draft 从未对外可见。若调用方未显式
取消，Draft 允许无限期存在，MVP 不提供自动 TTL 清理。

### 5.6 ReleaseManifest

已发布 Release 保存 RFC 8785 规范化 JSON 的精确原始字节、Manifest SHA-256、
Ed25519 Key ID、Signature，以及 Manifest 和 Signature 的不可变 Blob 引用。
数据库保存验证这些原始字节所需的元数据。

Manifest 与 Signature 的原始字节按 5.2 节相同的方式以内容寻址 Blob 存储
（同样的 `blobs/sha256/...` 布局），与 Artifact Blob 共用同一张 Blob 表，
不额外引入存储路径规则。

签名公钥不是通过 Manifest 响应自证可信。Docker Compose 和 Helm 交付必须同时
输出与私钥匹配的 Ed25519 公钥文件及其 SHA-256 Fingerprint；EdgeCLI Update
Gateway 通过独立部署配置固定该公钥或 Fingerprint，并按 Key ID 选择信任根。
`GET /api/v1/signing-keys/{key-id}` 只提供公钥发现和运维检查，客户端不得仅凭同一
API 返回的公钥建立信任。MVP 只有一个活动 Key ID，但数据模型允许保存多个历史
公钥，以保证将来轮换后仍能验证历史 Manifest。

### 5.7 Channel 与 ChannelRevision

Channel 属于 Package，名称例如 `candidate` 或 `stable`，只能指向同一 Package
下的已发布 Release。创建 Package 的事务同时创建空的 `candidate` 和 `stable`
Channel；MVP 不提供任意 Channel 创建 API。Channel 初始不指向任何 Release
（`current_release_id` 为空），首次晋级记录的『原 Release』字段为空值。数据库
通过包含 `package_id` 的复合外键或等价约束防止 Channel 指向其他 Package 的
Release。Resolve API（6.5 节）在 Channel 尚未指向任何 Release 时返回 `404`。

每次晋级创建不可变 ChannelRevision，记录原 Release、新 Release、操作者、原因、
Request ID 和时间。回滚不是改写历史，而是创建一条指向旧 Release 的新 Revision。

### 5.8 ServiceAccount 与 APIToken

ServiceAccount 可以拥有多个 APIToken。Token 包含公开 Token ID 和 256 位随机
Secret。APIToken 表只保存使用服务端 Pepper 计算的 HMAC-SHA-256、Scope、仓库
白名单、过期时间、吊销状态和最后使用时间，不保存可恢复 Secret。Token 创建接口
为了安全重放而暂存的加密响应属于独立 IdempotencyRecord，使用与 Pepper、签名
私钥都不同的 256 位 `IDEMPOTENCY_RESPONSE_KEY` 通过 AES-256-GCM 加密，并随
Idempotency 记录在 24 小时后删除；数据库中不得出现明文 Secret。

MVP Scope：

- `artifact:read`
- `artifact:write`
- `release:publish`
- `channel:promote`
- `admin`

`admin` Scope 隐含以上全部 Scope，且不受仓库白名单限制。其余 Scope 必须同时
匹配 Token 的仓库白名单才能操作对应 Repository 及其下的 Package/Channel；
Release 与 Channel 的仓库归属以其所属 Package 的 Repository 为准（见 5.4 节的
跨仓库约束）。仓库白名单为空表示『不能访问任何 Repository』，不是『可以访问
全部 Repository』——这是默认拒绝语义，创建 Token 时必须显式列出至少一个
Repository（`admin` Token 除外）。ServiceAccount 本身没有独立的启用/禁用
状态，停用一个服务账号需要吊销其名下的全部 Token。各 Endpoint 所需 Scope 及
是否受仓库白名单约束见 6.8 节的权限矩阵。

### 5.9 AuditEvent 与 Job

所有写操作和安全敏感结果都创建 AuditEvent。审计内容不得包含 Token、签名私钥
或预签名 URL。

Job 记录清理和恢复任务，包含有限重试、Lease 和结构化失败码。

### 5.10 名称与路径约束

Repository Key、Package Name 和 Channel Name 使用小写 ASCII，必须匹配：

```text
^[a-z][a-z0-9._-]{1,63}$
```

Release Version 是大小写敏感的不透明版本标识，不强制使用 SemVer，但必须匹配：

```text
^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$
```

Artifact 逻辑路径最长 1024 字节，以 `/` 分隔，每个 Segment 为 1 至 255 字节，
只能使用 URI Unreserved 字符 `[A-Za-z0-9._~-]`。服务只进行一次 Percent Decode，
拒绝空 Segment、`.`、`..`、反斜线、编码后的分隔符、控制字符和非规范编码。

## 6. REST API

所有 API 使用 `/api/v1` 前缀。JSON 错误采用 RFC 9457 Problem Details，返回
稳定的机器可读错误码和 Request ID。列表使用 Cursor Pagination。有副作用的
POST 请求支持 `Idempotency-Key`。

### 6.1 Repository API

```text
POST /api/v1/repositories
GET  /api/v1/repositories
GET  /api/v1/repositories/{repo}
```

### 6.2 Artifact API

```text
PUT  /api/v1/repositories/{repo}/artifacts/{path...}
HEAD /api/v1/repositories/{repo}/artifacts/{path...}
GET  /api/v1/repositories/{repo}/artifacts/{path...}
GET  /api/v1/repositories/{repo}/metadata/{path...}
```

上传请求通过 Go 服务流式写入 staging 对象，同时计算 SHA-256 和大小。客户端可
使用 `X-Checksum-Sha256` 声明预期摘要，摘要不一致时失败且不创建可见 Artifact。
逻辑路径已存在时返回 `409 Conflict`。服务端限制单次上传的最大 Body 大小（默认
10 GiB）和上传空闲超时（默认 10 分钟无数据传输即中断），具体默认值见第 11 节。
上传必须提供 `Content-Length`（不支持 `Transfer-Encoding: chunked`），以便在
写入 staging 前进行早期容量校验。
`Content-Type` 保存为 Artifact Media Type；文件名固定取规范化逻辑路径的最后
一个 Segment。可选 JSON 属性通过 `X-Artifact-Properties` 传递，其值为 Base64URL
编码的 UTF-8 JSON Object，解码后最多 16 KiB；数组、标量、重复 Key、无效 UTF-8
或超过限制均返回 `400`。Checksum Deploy 使用相同的 Media Type、文件名和属性
规则。

Checksum Deploy 通过请求头 `X-Checksum-Deploy: true` 与必填的
`X-Checksum-Sha256` 触发，请求体必须为空（`Content-Length: 0`）。服务端在
同一事务内校验并创建引用，避免与清理 Job 之间的竞态（见第 7 节）：只有当该
SHA-256 对应的 Blob **已被调用方具有读权限的至少一个 Repository 中的现有
Artifact 引用**，且调用方具有目标 Repository 的写权限时，才不上传 Body 直接
创建逻辑 Artifact 引用；否则统一返回 `404`，不会静默退回普通上传模式。这一
限定把 Checksum Deploy 的存在性判定收窄到调用方本就可见的范围内，避免其被
用作跨仓库探测任意内容是否存在于系统中的 Oracle（见第 10 节）。命中的 Blob
会刷新其『最近引用时间』，防止清理 Job 在竞态窗口内将其误判为孤儿对象。

`GET .../metadata/{path...}` 返回 Artifact 的完整元数据：逻辑路径、文件名、
Media Type、大小、SHA-256、JSON 属性、创建人和创建时间，不包含下载 URL。
HEAD 只返回大小、SHA-256、创建时间和 ETag，同样不生成下载 URL。

GET 由服务端以流式方式代理下载，API 进程转发 `Range` 请求头并保持状态码语义。
调用方必须使用 `redirect=false`；显式请求重定向返回
`409 public-endpoint-unavailable`。

### 6.3 Package 与 Release API

```text
POST /api/v1/repositories/{repo}/packages
GET  /api/v1/repositories/{repo}/packages
GET  /api/v1/repositories/{repo}/packages/{package}

POST   /api/v1/repositories/{repo}/packages/{package}/releases
GET    /api/v1/repositories/{repo}/packages/{package}/releases
POST   /api/v1/repositories/{repo}/packages/{package}/releases/{version}/artifacts
DELETE /api/v1/repositories/{repo}/packages/{package}/releases/{version}/artifacts/{releaseArtifactId}
POST   /api/v1/repositories/{repo}/packages/{package}/releases/{version}/publish
DELETE /api/v1/repositories/{repo}/packages/{package}/releases/{version}
GET    /api/v1/repositories/{repo}/packages/{package}/releases/{version}
GET    /api/v1/repositories/{repo}/packages/{package}/releases/{version}/manifest
```

创建 Package 时在同一事务内创建 `candidate` 和 `stable` Channel。ReleaseArtifact
创建响应返回其不可变 ID；删除 ReleaseArtifact 只允许作用于 `draft` Release。
Release 的 `DELETE` 仅允许作用于 `draft` 状态（取消草稿，见 5.5 节），其余状态
返回 `409`。

### 6.4 Channel API

```text
POST /api/v1/repositories/{repo}/packages/{package}/channels/{channel}/promotions
GET  /api/v1/repositories/{repo}/packages/{package}/channels/{channel}
GET  /api/v1/repositories/{repo}/packages/{package}/channels/{channel}/history
```

晋级请求使用同一 Package 内的 Release Version 标识目标，不接受任意 Release ID。
服务端必须按 `(package_id, version)` 查找目标 Release 并校验状态为 `published`。

### 6.5 Resolve API

```text
GET /api/v1/repositories/{repo}/packages/{package}/channels/{channel}/resolve?os=linux&arch=arm64&variant=&role=binary
```

`os`、`arch` 必填；`variant`、`role` 可选，默认视为空字符串（对应 5.5 节的
『无变体』默认值）。当前 Channel 指向的 Release 中若按
`(os, arch, variant, role)` 精确匹配到唯一 ReleaseArtifact 则返回该项；匹配
到零条返回 `404`；由于坐标唯一性已在 5.5 节强制保证，同一组参数不会匹配到
多条，因此不存在结果歧义。

响应包含已发布版本、Base64URL 编码的精确 Manifest 原始字节、Key ID、Ed25519
Signature、命中的 ReleaseArtifact 的 `variant`/`role`、Artifact SHA-256、大小和
制品库代理下载 URL。Gateway 不持有底层存储路径，所有下载都经过制品库鉴权。

### 6.6 身份和审计 API

```text
POST /api/v1/service-accounts
GET  /api/v1/service-accounts
GET  /api/v1/service-accounts/{id}
POST /api/v1/service-accounts/{id}/tokens
GET  /api/v1/service-accounts/{id}/tokens
POST /api/v1/tokens/{id}/revoke
GET  /api/v1/audit-events
GET  /api/v1/signing-keys/{key-id}
```

首次管理员不通过匿名 HTTP API 创建。运维人员在受控环境执行：

```text
artifact-repository bootstrap-admin --name <service-account-name>
```

该命令直接使用数据库连接，仅在系统不存在 ServiceAccount 时创建第一个 `admin`
Token，并把 Secret 输出一次。后续再次执行必须失败。该操作写入 AuditEvent，命令
不得把 Secret 写入日志或 Shell Debug 输出。

`GET /api/v1/audit-events` 要求 `admin` Scope，不对其他 Scope 开放，避免审计
记录本身成为信息泄露面。

### 6.7 Idempotency-Key

具有副作用的 POST 请求（发布、晋级、创建 ServiceAccount/Token 等）可携带
`Idempotency-Key`。Key 的作用域是
`(token_id, http_method, canonical_resource, idempotency_key)`；
`canonical_resource` 是 Percent Decode、名称规范化后的实际资源路径，必须包含
Repository、Package、Release Version 等路径参数，不能只使用 chi Route 模板。
请求指纹是 HTTP Method、Canonical Resource、Content-Type、影响语义的请求头和
原始 Body SHA-256 的组合摘要。

IdempotencyRecord 状态为 `pending|completed`。幂等执行按副作用类型分成两类，
不能由 Handler 在业务调用前后简单拼接 `Begin`/`Complete`：

- 纯 PostgreSQL 操作（创建 Repository/ServiceAccount/Token/Package/Release、
  增删 ReleaseArtifact、取消 Draft、吊销 Token、Channel 晋级）必须在同一个
  数据库事务内完成 IdempotencyRecord 仲裁、业务变更、AuditEvent 和
  `completed` 响应保存。进程在提交前崩溃时全部回滚；提交后崩溃时重放可以直接
  读取完成响应。Token Secret 的加密响应也在该事务中保存，不能出现 Token 已创建
  但可恢复 Secret 丢失的窗口。实现使用外层事务保存 IdempotencyRecord 和
  AuditEvent、内层 savepoint 执行业务变更：`2xx` 时提交 savepoint；确定性 `4xx`
  时回滚 savepoint，再在仍可用的外层事务中保存脱敏失败审计和 Problem 响应；瞬时
  错误则回滚整个外层事务。这样唯一约束等会中止当前 PostgreSQL 事务的错误也不会
  阻止确定性终态被保存。
- 包含外部 I/O 的发布操作先提交 `pending` 记录和与其一一关联的 PublishAttempt，
  后续由正常请求或 Worker 完成。任何无法确认结果的失败都由 PublishAttempt
  恢复逻辑判定并最终完成该记录。MVP 其他 POST 不允许在没有专用持久化恢复状态的
  情况下跨事务执行外部副作用。

并发的相同数据库操作由唯一约束等待首个短事务；等待超过有界锁超时，或发布请求
看到已提交的 `pending` 时，返回 `409 idempotency-in-progress` 和 `Retry-After`，
不能再次执行。记录转为 `completed` 时保存请求指纹、最终 HTTP 状态码和响应体：

- 相同 Key 且请求体 SHA-256 相同的重放，直接返回首次请求的响应，不重复执行
  副作用。
- 相同 Key 但请求体不同，返回 `409 Conflict`（错误码
  `idempotency-key-conflict`），不执行请求。
- 只有已经到达业务终态的 `2xx` 和确定性 `4xx` 响应进入 `completed`。确认尚未
  执行副作用的瞬时依赖失败应删除 `pending` 记录以允许重试；无法确认结果的失败
  保持 `pending`，由对应恢复 Job 判定最终状态后完成记录。
- Token 创建的完成响应按 5.8 节使用 AES-256-GCM 加密保存，AAD 包含该记录的完整
  作用域；其他响应不得包含 Secret，可直接保存 JSON 原始字节。
- 记录默认保留 24 小时（见第 11 节），过期后由清理 Job 删除；过期后使用同一
  Key 重放视为新请求。
- 未携带 `Idempotency-Key` 的请求不享有幂等保证，重试可能因为唯一约束或状态
  校验而返回 `409 Conflict`，而不是静默复用此前的结果。

### 6.8 权限矩阵

每个 Endpoint 必须精确匹配以下 Scope 与仓库白名单约束，避免实现者各自臆断
（`admin` Token 始终放行，不再单列）：

| Endpoint | 所需 Scope | 是否受仓库白名单约束 |
|---|---|---|
| `POST /repositories` | `admin` | 否（全局操作） |
| `GET /repositories`、`GET /repositories/{repo}` | 任意有效 Scope | 是，非 admin 只能看到白名单内的 Repository |
| `PUT/HEAD/GET .../artifacts/{path...}` | `artifact:write`（普通写）/`artifact:read`（读）；Checksum Deploy 还要求对至少一个来源 Repository 具有 `artifact:read` | 是，按目标及来源 Repository |
| `GET .../metadata/{path...}` | `artifact:read` | 是 |
| `POST/GET .../packages`、`GET .../packages/{package}` | `release:publish`（写）/`artifact:read`（读） | 是，按路径中的 Repository |
| `POST .../releases`、`.../artifacts`、`.../publish` | `release:publish` | 是，按 Package 所属 Repository |
| `DELETE .../releases/{version}`、`DELETE .../artifacts/{id}` | `release:publish` | 是，按 Package 所属 Repository |
| `GET .../releases`、`.../releases/{version}`、`.../manifest` | `artifact:read` | 是 |
| `POST .../channels/{channel}/promotions` | `channel:promote` | 是 |
| `GET .../channels/{channel}`、`.../history` | `artifact:read` | 是 |
| `GET .../channels/{channel}/resolve` | `artifact:read` | 是 |
| `POST/GET /service-accounts`、`GET /service-accounts/{id}`、`POST/GET .../tokens`、`POST /tokens/{id}/revoke` | `admin` | 否 |
| `GET /audit-events` | `admin` | 否（不提供仓库过滤，见 5.9 节对审计内容的脱敏要求已足以避免二次泄露） |
| `GET /signing-keys/{key-id}` | 任意有效 Scope | 否（公钥发现接口不构成信任根） |

### 6.9 JSON 契约与 OpenAPI

`openapi/openapi.yaml` 是 HTTP DTO、状态码和 Problem Code 的唯一契约来源，必须
在实现业务 Handler 前完成并通过 OpenAPI 3.1 校验。所有 JSON 请求拒绝未知字段，
Body 上限默认 64 KiB；名称、Reason 和 Cursor 的长度约束在 Schema 中固定。核心
请求 DTO 如下：

| 操作 | JSON Body | 成功状态 |
|---|---|---|
| 创建 Repository | `{"key":"releases","displayName":"Releases"}` | `201` |
| 创建 Package | `{"name":"edgecli"}` | `201` |
| 创建 Release | `{"version":"1.2.3"}` | `201` |
| 添加 ReleaseArtifact | `{"artifactPath":"edgecli/1.2.3/linux-arm64","os":"linux","arch":"arm64","variant":"","role":"binary"}` | `201` |
| 发布 Release | 空 Body（`Content-Length: 0`） | `200` |
| 晋级 Channel | `{"version":"1.2.3","reason":"CI promotion"}` | `200` |
| 创建 ServiceAccount | `{"name":"edgecli-ci"}` | `201` |
| 创建 Token | `{"scopes":["artifact:read"],"repositories":["releases"],"expiresAt":"2026-08-01T00:00:00Z"}` | `201` |

创建和详情响应都包含稳定 UUID、领域字段及 RFC 3339 UTC 审计时间。Token 创建响应
额外包含只在首次响应或有效 Idempotency 重放中出现的 `secret`；Token 列表永远不
返回 Secret/HMAC。列表统一返回 `{"items":[],"nextCursor":null}`。删除 Draft、
删除 ReleaseArtifact 和吊销 Token 成功返回 `204`。Resolve Schema 明确定义
`version`、`manifest`、`keyId`、`signature`、`artifact`（含 path、variant、role、
sha256、size）和 `downloadUrl`。OpenAPI 契约测试必须覆盖每个 Endpoint 的成功
响应及所有稳定 Problem Code。

## 7. 上传与存储一致性

上传执行顺序：

1. 认证 Token，并校验其 Repository 写权限。
2. 规范化并校验逻辑路径。
3. 在 PostgreSQL 创建 UploadSession，记录 `upload_id`、目标 Repository/Path、
   `active` 状态、Lease、心跳和最长结束时间。
4. 流式写入 `staging/<upload-id>`，同时计算 SHA-256 和大小，并定期刷新
   UploadSession Lease；校验客户端声明的摘要和 Content-Length。
5. 在短事务中 Upsert 并锁定 Blob 行：不存在时创建 `creating` Blob 并把本次
   UploadSession 设为 owner；`ready` 时直接复用；其他未过期 owner 的
   `creating` 或 `deleting` 状态返回可重试冲突，不能绕过状态直接引用。
6. Blob owner 将 staging 对象复制到内容寻址 Blob Key，校验对象大小和摘要，再在
   短事务中按 owner 和 Lease 条件把 Blob 改为 `ready`。如果对象已经存在，也必须
   校验大小；SHA-256 相同但大小不一致视为存储损坏并告警。
7. 在 PostgreSQL 事务中锁定 `ready` Blob、创建唯一 Artifact 引用并刷新
   `last_referenced_at`。只有数据库事务提交后 Artifact 才可见。
8. 将 UploadSession 标记为 `completed` 并删除 staging 对象；删除失败由 Job 重试。

如果文件系统写入成功而 Artifact 数据库事务失败，只能留下不可见的 `ready` 无引用
Blob，不能产生部分可见 Artifact。清理 Job 不按对象年龄直接删除：

- Staging 对象只有在对应 UploadSession Lease 和最长结束时间均已过期时才能删除；
  活跃上传通过心跳续租，因此连续传输超过安全保留期也不会被清理。
- 清理无引用 Blob 时，先在 PostgreSQL 短事务中锁定 Blob，确认所有 Artifact、
  Manifest 和 Signature 引用均为空且超过安全保留期，再通过条件更新将状态改为
  `deleting` 并递增删除 generation。新上传或 Checksum Deploy 遇到 `deleting`
  必须等待或重试，不能新增引用。
- 提交 `deleting` 状态后删除存储文件，成功后再按 generation 条件删除 Blob 行；
  删除失败保留 `deleting` 状态由 Job 重试。若 Cleaner 发现没有数据库行的旧孤儿
  对象，必须先插入 `deleting` Tombstone 再删除，防止并发上传复用该对象。

PostgreSQL 事务只负责取得删除权和 fencing，文件删除是独立、可重试的外部步骤。
Checksum Deploy 在创建引用前同样锁定 Blob 并要求状态为 `ready`，从而与 Cleaner
串行化。

同一逻辑路径的并发写入由 PostgreSQL 唯一约束仲裁：只有一个请求成功，其余返回
`409 Conflict`。

## 8. 发布与签名一致性

发布执行顺序：

1. 在短事务中锁定 Draft Release，校验状态、至少一个 Artifact、平台坐标唯一、
   Artifact 与 Package 同 Repository；创建包含 ReleaseArtifact 快照、
   `published_at`、Key ID 和快照 SHA-256 的 PublishAttempt，将 Release 改为
   `publishing` 并取得带 generation 的 Lease。
2. 事务提交后只使用持久化快照构建 Manifest，并使用 RFC 8785 规范化；不得在
   外部 I/O 期间持有 PostgreSQL 行锁或长事务。
3. 续租并通过 `Signer` 接口签署精确原始字节。
4. 将 Manifest 和 Signature 写入不可变内容寻址存储，并在 PublishAttempt 中保存
   两者的 SHA-256 和存储完成标记。
5. 再次续租并验证当前 `attempt_id`、owner 和 generation。
6. 在短事务中用上述 fencing 条件原子保存 Manifest/Signature 的 Blob 引用，将
   Release 改为 `published`，完成 IdempotencyRecord 并释放发布 Lease。条件更新
   影响零行时必须停止，旧持有者不得重试提交。

发布操作必须幂等。同一 Idempotency Key、请求指纹和未终结 Attempt 的重试只观察
或恢复同一 Attempt；已完成时返回原结果。停留超过发布租约超时（默认 5 分钟）的
`publishing` Release 由 Worker 递增 generation 后按 5.5 节恢复。任何时候都不得
暴露未签名的 Published Release，也不得让已失去 Lease 的进程改变可见状态。

失败分类和状态转换固定如下：

- Release 状态、Artifact 数量、平台坐标、跨 Repository 引用等确定性校验在创建
  PublishAttempt 前完成；失败保持 `draft`，在同一事务内把 IdempotencyRecord
  完成为对应 `4xx`。
- PostgreSQL 或文件系统的暂时失败属于可重试失败，Release 保持
  `publishing`，Worker 使用指数退避（5 秒起、最大 5 分钟）恢复同一 Attempt，
  默认最多 10 次。
- 达到重试上限后，只有在 `storage_completed=false` 且能够通过持久化标记及对象
  HEAD 确认 Manifest/Signature 都不存在时，Worker 才能终止 Attempt、回退为
  `draft`，并完成 `503 publish-attempt-aborted`；再次发布必须使用新 Key。
- 私钥格式/权限错误、Key ID 不匹配、签名校验失败、同 Hash 不同 Size、已出现
  签名对象但无法证明最终状态等不可重试或不确定完整性错误进入
  `publish_failed`，完成 `500 publish-failed` 并产生高优先级审计与指标。
- 每次恢复、退避和最终分类都写入 PublishAttempt 的有限失败码与尝试次数，不能
  依赖日志文本推断状态。

Manifest Schema 固定包含：`schemaVersion`、`repository`、`package`、`version`、
`publishedAt`、`artifacts`。每个 Artifact 项包含 `path`、`filename`、`os`、
`arch`、`variant`、`role`、`mediaType`、`sha256` 和 `size`。生成 Manifest 前按
`os`、`arch`、`variant`、`role`、`path` 的字节序稳定排序。`publishedAt` 在创建
PublishAttempt 时写入并持久化，同一 Attempt 的重试不得重新生成；新的 Attempt
使用新的 `publishedAt`，从而保证同一 Attempt 产生完全一致的原始字节和
Signature。

MVP Signer 从只读挂载的 Ed25519 私钥文件加载密钥，启动时校验密钥类型和文件
权限，并只暴露 `Signer` 接口。私钥不得写入 PostgreSQL、制品存储、日志、审计事件
或 API 响应。该接口后续可替换为 Vault/KMS 实现。

## 9. Channel 一致性

晋级操作在 PostgreSQL 事务内锁定 Channel，按该 Channel 的 `package_id` 和请求中
的 Version 查找目标 Release，校验目标属于同一 Package 且已发布，插入
ChannelRevision，并在同一事务中更新当前 Release。并发晋级必须串行执行；数据库
应使用包含 `package_id` 的复合外键或等价约束作为最后一道所有权保护。

Channel 状态不依赖文件系统中的可变 Pointer 文件。

## 10. 安全设计

- Token 必须高熵、哈希保存、可过期、可吊销且具有最小 Scope。
- Repository 白名单阻止跨仓库访问。
- Artifact Path 拒绝绝对路径、路径穿越、空段、控制字符、歧义编码和平台分隔符。
- 上传限制请求大小、Header 大小、持续时间和并发流数量。
- 持久卷仅挂载到制品库 Pod，并使用最小文件权限。
- PostgreSQL Role 使用最小权限。
- 日志必须脱敏 Authorization、Token 和签名私钥。
- 请求完成日志只包含有界 Route ID、Status、Duration、Actor ID、Repository ID
  和 Request ID，不记录 Raw URL 或 Artifact 内容。
- 按 Token ID 和 Endpoint 类型限流。MVP 使用单进程内存令牌桶，所有 Token
  （包括 `admin`）都受限；Endpoint 固定分为三类：`GET`/`HEAD` 属于 `read`，
  `POST`/`DELETE` 属于 `mutation`，Artifact `PUT` 属于 `upload`。默认值为：
  `read` 每秒补充 50 个 Token、Burst 100；`mutation` 每秒补充 10 个、Burst 20；
  `upload` 每秒补充 2 个、Burst 4，并且每个 Token 同时最多 4 路上传。以上阈值
  均可通过配置覆盖。超限返回 `429 Too Many Requests`、Problem Code
  `rate-limit-exceeded` 和至少为 1 秒的整数 `Retry-After`；上传并发超限同样返回
  `Retry-After: 1`。没有活跃上传且连续 15 分钟未使用的 Token 桶会被惰性回收，
  防止已过期或已吊销 Token 长期占用内存。MVP 假设 API 在任何时刻只有一个进程，
  Helm 使用 `Recreate` 升级策略；后续水平扩展为多副本时必须迁移到 PostgreSQL
  或其他共享存储实现的限流器，否则实际阈值会随进程数线性放大而失效。
- 成功和失败操作都写入不包含敏感信息的 AuditEvent。
- Checksum Deploy（6.2 节）本质上是『声明已知 SHA-256、免上传创建引用』，
  如果存在性判定覆盖全局 Blob 而不受调用方可读范围限制，会构成跨仓库的内容
  存在性 Oracle（凭猜测 Hash 探测其他 Repository 是否已有某内容）。MVP 通过
  把存在性判定收窄到『调用方可读的 Repository 中已被引用的 Blob』消除了跨
  仓库泄露（见 6.2 节）；剩余的『同仓库内探测内容是否存在』属于调用方权限
  范围内的正常行为，不再是额外风险。全局去重仍在存储层生效，只是不通过
  Checksum Deploy 对调用方可见。

## 11. 可观测性与运维

运行端点：

```text
GET /healthz
GET /readyz
GET /metrics
```

`healthz` 只反映进程存活。`readyz` 使用有界超时检查 PostgreSQL 和持久文件系统。

指标包括上传次数、字节数、延迟、失败码、下载和 Resolve 结果、Blob 去重命中、
发布和晋级结果、签名失败、staging 年龄、Job 积压及依赖请求延迟。指标 Label
必须有界，不能包含原始路径、版本、Token 或 URL。

部署交付包括非 Root 多阶段 Docker 镜像、本地 Docker Compose、Helm Chart、
数据库迁移、监控规则和运维手册。二进制提供独立 `migrate` 子命令；Compose 在
启动 API/Worker 前运行一次性 migrate Service，Helm 使用带 Hook 权重和失败阻断的
pre-install/pre-upgrade Migration Job。API 和 Worker 本身不并发执行迁移，只在
启动时校验 Schema Version，版本不匹配则明确失败。

MVP 关键运行参数（均可通过配置覆盖，以下为默认值）：

- Staging 对象与孤儿 Blob 的安全保留期：24 小时。
- 单次上传最大 Body 大小：10 GiB。
- 上传空闲超时：10 分钟无数据传输即中断。
- 单次上传最长持续时间：12 小时。
- UploadSession Lease：2 分钟，活跃上传每 30 秒续租一次。
- 发布租约（`publishing` Lease）超时：5 分钟。
- 发布 Lease 续租间隔：1 分钟。
- Idempotency-Key 记录保留期：24 小时。
- 发布恢复最大重试次数：10 次，退避从 5 秒指数增长到最多 5 分钟。
- 限流桶空闲回收时间：15 分钟；默认限流阈值见第 10 节。

## 12. 已知风险与设计权衡

以下问题在 MVP 范围内是刻意的简化，不是遗漏，但需要在后续演进前重新评估：

- **签名密钥单一且不支持轮换**：`Signer` 只加载一把 Ed25519 私钥，没有多 Key
  并存签名或自动轮换机制。系统保留历史公钥验证能力，但 MVP 运维流程不自动完成
  私钥轮换；引入 Vault/KMS 时需要支持『多把公钥验证、单把私钥签名』的过渡期，
  并考虑 Key 撤销后历史 Manifest 如何仍能被验证。
- **Token 创建的幂等重放扩大了短期 Secret 处理面**：为保证丢失首次 HTTP 响应后
  能重放同一 Secret，IdempotencyRecord 会保存 AES-256-GCM 密文，最长 24 小时。
  `IDEMPOTENCY_RESPONSE_KEY` 必须独立挂载、不得写入数据库或日志；若部署方不接受
  该风险，可以关闭 Token 创建接口的 Idempotency 支持，但不能退化为明文存储。
- **Draft Release 仅支持显式取消，没有 TTL 自动清理**：调用方可通过
  `DELETE .../releases/{version}` 主动释放版本坑位（见 5.5 节），但遗忘取消
  的 Draft 仍会无限期累积。后续可引入 TTL 自动清理策略，但不得影响任何
  Published Release 的不可变性。
- **无法标记已发布 Release 为不推荐（deprecate/yank）**：当前只能通过重新
  晋级 Channel 到更早版本来事实上『撤回』，Release 本身没有告警状态供
  Resolve API 提示调用方。
- **Checksum Deploy 的存在性判定已收窄到调用方可读范围**（见第 10 节），
  跨仓库 Oracle 风险已消除；但演进到多租户时，『仓库』和『租户』的边界如果
  不一致，仍需要重新评估 Blob 命名空间隔离粒度是否要下沉到租户级别。
- **限流为单实例内存实现**（见第 10 节），水平扩展为多副本前必须替换为共享
  存储方案。
- **Package 与 Repository 类型强绑定**：MVP 只有 `local/raw` 一种仓库类型，
  Package 借用 Repository 作为权限与命名空间边界（见 5.4 节的跨仓库约束）。
  引入 Remote/Virtual Repository 或专门的『Release 仓库类型』时，需要重新
  评估该边界是否仍然合适，是否要允许 Package 跨多个 Repository 聚合内容。
- **`readyz` 直接反映 PostgreSQL/文件系统可用性**：作为 K8s Readiness Probe 时，
  依赖抖动会导致全部副本同时被判定为 NotReady 从而级联下线。建议为 `readyz`
  增加短暂的失败容忍窗口（连续失败 N 次才转为 NotReady），避免瞬时抖动引发
  雪崩。

## 13. 测试策略

- 单元测试：路径规范化、权限矩阵、Token 校验、状态机、规范化 Manifest 字节和
  签名。
- PostgreSQL 集成测试：唯一约束、事务、并发上传、幂等 Pending 仲裁、发布 fencing、
  跨 Package 晋级拒绝、晋级串行和重启恢复。
- 文件系统集成测试：流式上传、UploadSession 续租、Checksum Deploy、去重、
  代理下载、`deleting` Blob 竞态和 staging 清理。
- OpenAPI 契约测试：成功响应和 RFC 9457 错误响应。
- 故障注入测试：文件系统失败、数据库提交失败、签名失败、请求中断和重复请求。
- 安全测试：路径穿越、超大 Header、无效 Token、跨仓库访问、跨 Package 晋级、
  摘要欺骗、Token Secret 仅以密文暂存，以及日志泄密。
- 端到端测试：创建 Repository 和 Package，上传 Linux ARM64 Artifact，发布
  Release，晋级 candidate，Resolve、下载，并使用部署时固定的公钥校验 SHA-256
  和 Ed25519 Signature。

## 14. MVP 验收标准

1. 全新环境可通过 Docker Compose 启动 PostgreSQL 和服务。
2. `migrate` 子命令可重复执行且不破坏数据；Compose 一次性迁移和 Helm Migration
   Job 都会在 API/Worker 启动前完成，Schema Version 不匹配时服务拒绝启动。
3. 文档中的完整 API 流程可以自动化执行通过。
4. 同一逻辑路径并发上传时只有一个请求成功。
5. 已发布 Artifact 和 Release 无法覆盖或修改。
6. ReleaseManifest 确定、已签名，并可使用部署时独立固定的公钥验证；仅从 API
   获取公钥不能满足该验收项。
7. Channel 晋级原子执行、保留完整历史，并通过新 Revision 支持回滚。
8. Resolve 下载内容的 SHA-256 和大小与签名 Manifest 一致。
9. 服务重启不会丢失可见状态，也不会暴露 staging 对象。
10. 未授权和跨仓库操作必须失败，并产生安全的 AuditEvent。
11. EdgeCLI Update Gateway 使用只读 Token Resolve candidate，不持有底层存储路径，
    并通过 API 代理下载。
12. 单元、集成、契约、安全和端到端测试全部通过。
13. 向 Release 添加仓库范围之外的 Artifact 引用必须失败，验证跨 Repository
    隔离在 Release 层面同样生效。
14. 相同 `Idempotency-Key`、Canonical Resource 和请求指纹的重放返回首次结果；
    指纹不同的重放返回 `409 Conflict`，并发重放在首次请求未完成时返回
    `409 idempotency-in-progress` 且不重复执行副作用。
15. 强制暂停处于 `publishing` 状态的原进程直到 Lease 过期，Worker 接管并递增
    generation 后再恢复原进程；旧进程的最终状态提交必须影响零行，Release 只
    发布 Worker 恢复的同一快照，且不产生不一致 Manifest。
16. 同一 Release 内添加 `variant`/`role` 均为空的两条同平台 ReleaseArtifact，
    第二条必须被唯一约束拒绝（验证 `NOT NULL DEFAULT ''` 未被空值绕过）。
17. 空仓库白名单的 Token 对任何 Repository 的操作必须失败，验证『空白名单 =
    无权限』而非『空白名单 = 全部权限』。
18. 取消 Draft Release 后可用相同 `version` 重新创建 Release；取消
    `publishing`/`published`/`publish_failed` 状态的 Release 必须失败。
19. 创建两个 Repository 下的同名 Package 必须成功，所有 Package API 都按路径中
    的 Repository 唯一解析；不允许省略 Repository 的旧路径。
20. 向 Channel 晋级其他 Package（包括其他 Repository）下的已发布 Release 必须
    失败，数据库所有权约束和 Service 校验均有测试覆盖。
21. Token 创建的 IdempotencyRecord 在数据库中只包含 AES-256-GCM 密文；相同 Key
    在 24 小时内可重放同一 Secret，过期清理后数据库不再保留可恢复响应。
22. Cleaner 与上传/Checksum Deploy 并发操作同一 Blob 时，新引用只能发生在
    `ready` 状态；一旦进入 `deleting`，写入方必须重试，最终不得出现可见 Artifact
    指向不存在的存储文件。
23. Draft ReleaseArtifact 可以通过 API 删除；Package 创建后 `candidate` 和
    `stable` Channel 已存在且初始 Resolve 返回 `404`。
24. 对纯 PostgreSQL Endpoint 注入“业务变更后、HTTP 响应前”进程崩溃，重启后
    使用相同 Idempotency Key 必须重放已提交结果；Token 创建必须重放同一 Secret。
25. 显式请求重定向下载必须失败，代理下载必须保持鉴权、Range、大小和摘要语义。

## 15. 实施顺序

1. Go Module、配置、完整 OpenAPI 3.1 DTO/错误契约和 Handler 契约测试骨架。
2. PostgreSQL Schema、`migrate` 子命令、sqlc 和数据库集成测试。
3. ServiceAccount、Token、Scope、事务型 Idempotency 和 Audit 基础能力。
4. Repository、Package、Release Draft 与 ReleaseArtifact 生命周期。
5. Blob、不可变上传、元数据、下载和清理 fencing。
6. PublishAttempt、规范化 Manifest、Signer 和发布恢复。
7. Channel、晋级历史、回滚和 Resolve API。
8. Job 完整清理、指标、Compose、Helm 和运维文档。
9. EdgeCLI Update Gateway 集成和完整 candidate 验收。
