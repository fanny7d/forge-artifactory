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
- 基于 MinIO 的全局 Blob 去重。
- Package、不可变 Release、签名 Manifest 和 ReleaseArtifact。
- `candidate`、`stable` Channel 及原子晋级历史。
- 服务账号，以及具有 Scope 和仓库范围限制的 API Token。
- 所有写操作及安全敏感操作的审计记录。
- 基于 PostgreSQL 的后台任务，用于 staging 清理和异常恢复。
- 短期预签名下载地址。
- Docker Compose、Docker 镜像、Helm Chart、指标、健康检查和运维手册。

MVP 不包含：

- Remote Repository 和 Virtual Repository。
- Maven、npm、OCI、Docker Registry、PyPI 等生态专用协议。
- 可变仓库或 Allow Redeploy 模式。
- Artifact、Release、ChannelRevision 删除 API。
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
- `blob`：MinIO 内容寻址存储和去重。
- `release`：Package、Draft Release、ReleaseArtifact、发布和 Manifest。
- `channel`：candidate/stable 晋级和不可变历史。
- `signing`：RFC 8785 Manifest 规范化和 Ed25519 签名。
- `audit`：不可变的操作和安全审计事件。
- `jobs`：基于 PostgreSQL 的清理及恢复任务。
- `api`：REST Handler、中间件、校验、错误协议、指标和 OpenAPI。
- `storage`：MinIO 适配器。
- `database`：PostgreSQL 事务、迁移和类型安全查询。

PostgreSQL 是元数据、可见性、授权和事务状态的唯一事实来源。MinIO 只是内部
文件存储，用于保存 Artifact Blob、Manifest 原始字节和 Signature。调用方不
接触 MinIO Bucket 或对象路径。

后台任务记录在 PostgreSQL 中，通过 `FOR UPDATE SKIP LOCKED` 领取。MVP 不
引入 Kafka、NATS、Redis 或其他消息队列。

## 4. 技术栈与工程结构

- 当前稳定版本的 Go。
- `net/http` + `chi`：HTTP 路由和中间件。
- `pgx/v5`：PostgreSQL 访问和事务。
- `sqlc`：从 SQL 生成类型安全的数据访问代码。
- `goose`：数据库迁移。
- MinIO Go SDK：对象存储。
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

HTTP Handler 只调用领域 Service，不直接执行 SQL 或调用 MinIO。事务边界由领域
Service 控制。

## 5. 领域模型

### 5.1 Repository

Repository 包含唯一 `key`、显示名称、固定类型 `local/raw`、创建审计字段和
不可变写入策略。MVP 不提供可变仓库模式。

### 5.2 Blob

Blob 使用全局 SHA-256 标识，记录大小和内部 MinIO Object Key。物理对象采用
内容寻址布局：

```text
blobs/sha256/ab/cd/<完整 SHA-256>
```

多个 Artifact 可以引用同一个 Blob。

### 5.3 Artifact

Artifact 属于一个 Repository，包含规范化逻辑路径、Blob 引用、Media Type、
文件名、JSON 属性、创建人和创建时间。
`(repository_id, logical_path)` 必须唯一。Artifact 一旦可见就永久不可变。

### 5.4 Package

Package 表示可发布产品，例如 `edgecli`。Package 属于一个 Repository，并拥有
Release 和 Channel。Raw Artifact 可以独立存在，不强制归属 Package。

### 5.5 Release 与 ReleaseArtifact

Release 以 `(package_id, version)` 唯一，状态包括 `draft`、`publishing` 和
`published`。Draft Release 只能添加已有的不可变 Artifact 引用。

ReleaseArtifact 记录 Artifact 引用、`os`、`arch`、可选 `variant` 和 `role`。
同一 Release 内的平台坐标必须唯一。

Release 发布后，其元数据及所有 Artifact 引用永久冻结。

### 5.6 ReleaseManifest

已发布 Release 保存 RFC 8785 规范化 JSON 的精确原始字节、Manifest SHA-256、
Ed25519 Key ID、Signature，以及 Manifest 和 Signature 的不可变 Blob 引用。
数据库保存验证这些原始字节所需的元数据。

### 5.7 Channel 与 ChannelRevision

Channel 属于 Package，名称例如 `candidate` 或 `stable`，只能指向已发布 Release。

每次晋级创建不可变 ChannelRevision，记录原 Release、新 Release、操作者、原因、
Request ID 和时间。回滚不是改写历史，而是创建一条指向旧 Release 的新 Revision。

### 5.8 ServiceAccount 与 APIToken

ServiceAccount 可以拥有多个 APIToken。Token 包含公开 Token ID 和 256 位随机
Secret。数据库只保存使用服务端 Pepper 计算的 HMAC-SHA-256、Scope、仓库白名单、
过期时间、吊销状态和最后使用时间。

MVP Scope：

- `artifact:read`
- `artifact:write`
- `release:publish`
- `channel:promote`
- `admin`

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
逻辑路径已存在时返回 `409 Conflict`。

Checksum Deploy 仅在声明的 SHA-256 Blob 已存在且调用方具有写权限时，不上传
Body 而直接创建逻辑 Artifact 引用。

GET 默认返回 `307 Temporary Redirect`，指向短期 MinIO 预签名 URL。HEAD 只返回
大小、SHA-256、创建时间和 ETag，不生成下载 URL。

### 6.3 Package 与 Release API

```text
POST /api/v1/packages
GET  /api/v1/packages/{package}

POST /api/v1/packages/{package}/releases
POST /api/v1/packages/{package}/releases/{version}/artifacts
POST /api/v1/packages/{package}/releases/{version}/publish
GET  /api/v1/packages/{package}/releases/{version}
GET  /api/v1/packages/{package}/releases/{version}/manifest
```

### 6.4 Channel API

```text
POST /api/v1/packages/{package}/channels/{channel}/promotions
GET  /api/v1/packages/{package}/channels/{channel}
GET  /api/v1/packages/{package}/channels/{channel}/history
```

### 6.5 Resolve API

```text
GET /api/v1/packages/{package}/channels/{channel}/resolve?os=linux&arch=arm64
```

响应包含已发布版本、Base64URL 编码的精确 Manifest 原始字节、Key ID、Ed25519
Signature、Artifact SHA-256、大小和短期下载 URL。

EdgeCLI Update Gateway 使用仅具有读取权限且限制仓库范围的 Token 调用此接口，
不再直接访问 MinIO。

### 6.6 身份和审计 API

```text
POST /api/v1/service-accounts
POST /api/v1/service-accounts/{id}/tokens
POST /api/v1/tokens/{id}/revoke
GET  /api/v1/audit-events
```

首次管理员不通过匿名 HTTP API 创建。运维人员在受控环境执行：

```text
artifact-repository bootstrap-admin --name <service-account-name>
```

该命令直接使用数据库连接，仅在系统不存在 ServiceAccount 时创建第一个 `admin`
Token，并把 Secret 输出一次。后续再次执行必须失败。该操作写入 AuditEvent，命令
不得把 Secret 写入日志或 Shell Debug 输出。

## 7. 上传与存储一致性

上传执行顺序：

1. 认证 Token，并校验其 Repository 写权限。
2. 规范化并校验逻辑路径。
3. 流式写入 `staging/<upload-id>`，同时计算 SHA-256 和大小。
4. 校验客户端声明的摘要和 Content-Length。
5. 将对象复制到内容寻址 Blob Key，或复用已有 Blob。
6. 在 PostgreSQL 事务中创建或引用 Blob，并创建唯一 Artifact。
7. 删除 staging 对象。

如果 MinIO 成功而数据库事务失败，可能留下不可见的无引用 Blob，但绝不能产生
部分可见的 Artifact。清理任务只有在超过安全保留时间后，才能删除 staging 对象
和无数据库引用的 Blob。

同一逻辑路径的并发写入由 PostgreSQL 唯一约束仲裁：只有一个请求成功，其余返回
`409 Conflict`。

## 8. 发布与签名一致性

发布执行顺序：

1. 锁定 Draft Release。
2. 校验 Release 状态、至少一个 Artifact，以及平台坐标不重复。
3. 构建 Manifest，并使用 RFC 8785 规范化。
4. 通过 `Signer` 接口签署精确原始字节。
5. 将 Manifest 和 Signature 写入不可变内容寻址存储。
6. 原子保存其元数据，并将 Release 状态改为 `published`。

发布操作必须幂等。相同 Idempotency Key 的重试返回同一结果。失败的
`publishing` 状态由 Worker 根据操作记录恢复，任何时候都不得暴露未签名的
Published Release。

Manifest Schema 固定包含：`schemaVersion`、`repository`、`package`、`version`、
`publishedAt`、`artifacts`。每个 Artifact 项包含 `path`、`filename`、`os`、
`arch`、`variant`、`role`、`mediaType`、`sha256` 和 `size`。生成 Manifest 前按
`os`、`arch`、`variant`、`role`、`path` 的字节序稳定排序。`publishedAt` 在首次
发布操作开始时写入并持久化，重试不得重新生成，从而保证相同发布操作产生完全
一致的原始字节和 Signature。

MVP Signer 从只读挂载的 Ed25519 私钥文件加载密钥，启动时校验密钥类型和文件
权限，并只暴露 `Signer` 接口。私钥不得写入 PostgreSQL、MinIO、日志、审计事件
或 API 响应。该接口后续可替换为 Vault/KMS 实现。

## 9. Channel 一致性

晋级操作在 PostgreSQL 事务内锁定 Channel，校验目标 Release 已发布，插入
ChannelRevision，并在同一事务中更新当前 Release。并发晋级必须串行执行。

Channel 状态不依赖 MinIO 中的可变 Pointer 文件。

## 10. 安全设计

- Token 必须高熵、哈希保存、可过期、可吊销且具有最小 Scope。
- Repository 白名单阻止跨仓库访问。
- Artifact Path 拒绝绝对路径、路径穿越、空段、控制字符、歧义编码和平台分隔符。
- 上传限制请求大小、Header 大小、持续时间和并发流数量。
- MinIO Bucket 保持私有，只有制品库服务持有 MinIO 凭据。
- PostgreSQL Role 和 MinIO 凭据使用最小权限。
- 日志必须脱敏 Authorization、Token、签名私钥、MinIO 凭据和预签名 URL。
- 请求完成日志只包含有界 Route ID、Status、Duration、Actor ID、Repository ID
  和 Request ID，不记录 Raw URL 或 Artifact 内容。
- 按 Token 和 Endpoint 类型限流。
- 成功和失败操作都写入不包含敏感信息的 AuditEvent。

## 11. 可观测性与运维

运行端点：

```text
GET /healthz
GET /readyz
GET /metrics
```

`healthz` 只反映进程存活。`readyz` 使用有界超时检查 PostgreSQL 和 MinIO。

指标包括上传次数、字节数、延迟、失败码、下载和 Resolve 结果、Blob 去重命中、
发布和晋级结果、签名失败、staging 年龄、Job 积压及依赖请求延迟。指标 Label
必须有界，不能包含原始路径、版本、Token 或 URL。

部署交付包括非 Root 多阶段 Docker 镜像、本地 Docker Compose、Helm Chart、
数据库迁移、监控规则和运维手册。

## 12. 测试策略

- 单元测试：路径规范化、权限矩阵、Token 校验、状态机、规范化 Manifest 字节和
  签名。
- PostgreSQL 集成测试：唯一约束、事务、并发上传、幂等、晋级串行和重启恢复。
- MinIO 集成测试：流式上传、Checksum Deploy、去重、预签名下载和 staging 清理。
- OpenAPI 契约测试：成功响应和 RFC 9457 错误响应。
- 故障注入测试：MinIO 失败、数据库提交失败、签名失败、请求中断和重复请求。
- 安全测试：路径穿越、超大 Header、无效 Token、跨仓库访问、摘要欺骗和日志
  泄密。
- 端到端测试：创建 Repository 和 Package，上传 Linux ARM64 Artifact，发布
  Release，晋级 candidate，Resolve、下载，并校验 SHA-256 和 Ed25519 Signature。

## 13. MVP 验收标准

1. 全新环境可通过 Docker Compose 启动 PostgreSQL、MinIO 和服务。
2. 数据库迁移可重复执行且不破坏数据。
3. 文档中的完整 API 流程可以自动化执行通过。
4. 同一逻辑路径并发上传时只有一个请求成功。
5. 已发布 Artifact 和 Release 无法覆盖或修改。
6. ReleaseManifest 确定、已签名且可以独立验证。
7. Channel 晋级原子执行、保留完整历史，并通过新 Revision 支持回滚。
8. Resolve 下载内容的 SHA-256 和大小与签名 Manifest 一致。
9. 服务重启不会丢失可见状态，也不会暴露 staging 对象。
10. 未授权和跨仓库操作必须失败，并产生安全的 AuditEvent。
11. EdgeCLI Update Gateway 使用只读 Token Resolve candidate，且不直接访问 MinIO。
12. 单元、集成、契约、安全和端到端测试全部通过。

## 14. 实施顺序

1. Go Module、配置、依赖适配器、Compose 和数据库迁移。
2. ServiceAccount、Token、Scope 和 Audit 基础能力。
3. Repository、Blob、不可变上传、元数据和下载。
4. Package、Release、规范化 Manifest 和 Signer。
5. Channel、晋级历史、回滚和 Resolve API。
6. Job、清理、指标、OpenAPI 完善、Helm 和运维文档。
7. EdgeCLI Update Gateway 集成和完整 candidate 验收。
