# Artifact Repository

一个面向公司私有 CLI 的多产品发布与自更新平台。每个 CLI 独立管理版本和多平台
制品，支持单二进制原子自替换、`tar.gz`/`zip` Bundle、签名安装 Hook、当前用户
`curl | sh` 首装，以及 CLI 启动时检查 stable 并提示升级。底层仍提供 API 优先的
不可变 Artifact、签名 Release 和 `candidate`/`stable` Channel。

产品模型、CI 发布契约和 CLI 嵌入示例见
[`docs/updater.md`](docs/updater.md)。

## 本地流程

以下命令需要 Docker Compose、`curl`、`jq`、Go 和 OpenSSL。命令使用具名
Compose 项目，后续可以把同一个项目名传给 E2E 测试。

如果不通过 Compose、直接运行二进制程序，先生成一对 Ed25519 密钥：

```bash
mkdir -p .local/signing
go run ./cmd/artifact-repository keygen \
  --private-key-file .local/signing/private.pem \
  --public-key-file .local/signing/public.pem
```

Compose 会在 `signing-keys` 卷中维护一对不会被覆盖的签名密钥，并让 API 与
Worker 共享 `artifact-data` 文件系统卷。启动完整堆栈，
并保存其公钥作为独立信任根；不要从 signing-key API 获取验签信任根：

```bash
export AR_PROJECT=artifact-repository
export BASE_URL=http://127.0.0.1:8080
mkdir -p .local
docker compose -p "$AR_PROJECT" up -d --build
docker compose -p "$AR_PROJECT" ps
curl --fail "$BASE_URL/readyz"
curl --fail http://127.0.0.1:8081/readyz
docker compose -p "$AR_PROJECT" exec -T api cat /app/keys/public.pem \
  >.local/compose-public.pem
```

API 启动后，可通过 [`http://127.0.0.1:8080/dashboard/`](http://127.0.0.1:8080/dashboard/)
打开内置管理控制台。界面只保留“CLI 制品库”：添加多个 CLI、查看当前版本、一次发布
多个平台文件，以及复制永久安装命令。Repository、Package、Channel、服务账号和审计
等底层对象不再暴露在日常界面中。控制台使用管理员 Bearer Token 登录；默认只在当前
浏览器会话中保存，只有显式勾选“保持登录”才写入浏览器本地存储。

终端用户可以使用 `artifactctl` 上传、下载和查看制品，或验证签名后从 Channel 拉取
匹配平台的 Release。构建、认证和命令示例见 [`docs/cli.md`](docs/cli.md)：

```bash
mkdir -p ./bin
go build -o ./bin/artifactctl ./cmd/artifactctl
export ARTIFACT_REPOSITORY_URL=http://127.0.0.1:8080
export ARTIFACT_REPOSITORY_TOKEN='ar1...'
./bin/artifactctl upload ./dist/edgecli releases/linux/arm64/edgecli
```

控制台发布失败后可以继续重试；已经发布但未晋级的版本可直接“设为当前”，未完成的
draft 可从版本历史清理。CI 仍可通过 OpenAPI 使用完整底层能力。

首次管理员初始化只能执行一次。请避免让返回的 Bearer Token 出现在 shell
跟踪信息或日志中：

```bash
export ADMIN_TOKEN="$(docker compose -p "$AR_PROJECT" exec -T api \
  /app/artifact-repository bootstrap-admin --name local-admin)"
```

创建 Repository、发布者账号、发布者 Token 和 Package。每个 JSON 写操作都必须
携带 `Idempotency-Key`：

```bash
export RUN_ID="local-$(date +%s)"
export REPO="repo-$RUN_ID"
export PACKAGE=edgecli
export EXPIRES_AT="$(date -u -d '+24 hours' +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
  date -u -v+24H +%Y-%m-%dT%H:%M:%SZ)"

curl --fail-with-body -sS -X POST "$BASE_URL/api/v1/repositories" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-repository" \
  --data "$(jq -nc --arg key "$REPO" '{key:$key,displayName:"Local artifacts"}')" \
  | jq .

ACCOUNT_ID="$(curl --fail-with-body -sS -X POST "$BASE_URL/api/v1/service-accounts" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-publisher-account" \
  --data "$(jq -nc --arg name "publisher-$RUN_ID" '{name:$name}')" | jq -r .id)"

export PUBLISHER_TOKEN="$(curl --fail-with-body -sS -X POST \
  "$BASE_URL/api/v1/service-accounts/$ACCOUNT_ID/tokens" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-publisher-token" \
  --data "$(jq -nc --arg repo "$REPO" --arg expires "$EXPIRES_AT" \
    '{scopes:["artifact:read","artifact:write","release:publish","channel:promote"],repositories:[$repo],expiresAt:$expires}')" \
  | jq -r .secret)"

curl --fail-with-body -sS -X POST "$BASE_URL/api/v1/repositories/$REPO/packages" \
  -H "Authorization: Bearer $PUBLISHER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-package" \
  --data "$(jq -nc --arg name "$PACKAGE" '{name:$name}')" | jq .
```

上传一个 Linux ARM64 二进制文件。API 会校验 `Content-Length` 和提交的
SHA-256，校验通过后才会让不可变路径可见：

```bash
printf '\177ELFartifact-repository-linux-arm64\n' >.local/edgecli-linux-arm64
export ARTIFACT_PATH="linux/arm64/$RUN_ID/edgecli"
export ARTIFACT_SHA="$(openssl dgst -sha256 .local/edgecli-linux-arm64 | awk '{print $NF}')"

curl --fail-with-body -sS -X PUT \
  "$BASE_URL/api/v1/repositories/$REPO/artifacts/$ARTIFACT_PATH" \
  -H "Authorization: Bearer $PUBLISHER_TOKEN" \
  -H 'Content-Type: application/octet-stream' \
  -H "X-Checksum-Sha256: $ARTIFACT_SHA" \
  --data-binary @.local/edgecli-linux-arm64 | jq .
```

创建 Release，关联 Artifact，执行发布，并将其晋级到 `candidate`。可执行程序应显式使用 `role=binary`，以免设备端更新器误取同一 Release 中的配置或附属制品：

```bash
export VERSION=1.0.0
export RELEASE_URL="$BASE_URL/api/v1/repositories/$REPO/packages/$PACKAGE/releases/$VERSION"

curl --fail-with-body -sS -X POST \
  "$BASE_URL/api/v1/repositories/$REPO/packages/$PACKAGE/releases" \
  -H "Authorization: Bearer $PUBLISHER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-release" \
  --data "$(jq -nc --arg version "$VERSION" '{version:$version}')" | jq .

curl --fail-with-body -sS -X POST "$RELEASE_URL/artifacts" \
  -H "Authorization: Bearer $PUBLISHER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-release-artifact" \
  --data "$(jq -nc --arg path "$ARTIFACT_PATH" \
    '{artifactPath:$path,os:"linux",arch:"arm64",role:"binary"}')" | jq .

curl --fail-with-body -sS -X POST "$RELEASE_URL/publish" \
  -H "Authorization: Bearer $PUBLISHER_TOKEN" \
  -H "Idempotency-Key: $RUN_ID-publish" | jq .

curl --fail-with-body -sS -X POST \
  "$BASE_URL/api/v1/repositories/$REPO/packages/$PACKAGE/channels/candidate/promotions" \
  -H "Authorization: Bearer $PUBLISHER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-promote" \
  --data "$(jq -nc --arg version "$VERSION" \
    '{version:$version,reason:"local acceptance"}')" | jq .
```

验收通过后，再将同一个 Release 以相同的 `version` 和可追溯 `reason` 提交到 `POST /api/v1/repositories/{repo}/packages/{package}/channels/stable/promotions`。设备端自动更新只查询 `stable` Channel，不会自动接受 `candidate`。

签发只读 Token，Resolve `candidate`，然后通过 API 代理下载。读者不需要获得
底层存储凭据、目录名称或 Object Key：

```bash
READER_ID="$(curl --fail-with-body -sS -X POST "$BASE_URL/api/v1/service-accounts" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-reader-account" \
  --data "$(jq -nc --arg name "reader-$RUN_ID" '{name:$name}')" | jq -r .id)"

export READER_TOKEN="$(curl --fail-with-body -sS -X POST \
  "$BASE_URL/api/v1/service-accounts/$READER_ID/tokens" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RUN_ID-reader-token" \
  --data "$(jq -nc --arg repo "$REPO" --arg expires "$EXPIRES_AT" \
    '{scopes:["artifact:read"],repositories:[$repo],expiresAt:$expires}')" \
  | jq -r .secret)"

RESOLVE="$(curl --fail-with-body -sS \
  "$BASE_URL/api/v1/repositories/$REPO/packages/$PACKAGE/channels/candidate/resolve?os=linux&arch=arm64&role=binary&redirect=false" \
  -H "Authorization: Bearer $READER_TOKEN")"
PROXY_PATH="$(jq -r .downloadUrl <<<"$RESOLVE")"
curl --fail-with-body -sS "$BASE_URL$PROXY_PATH" \
  -H "Authorization: Bearer $READER_TOKEN" -o .local/edgecli.downloaded
test "$(openssl dgst -sha256 .local/edgecli.downloaded | awk '{print $NF}')" = \
  "$(jq -r .artifact.sha256 <<<"$RESOLVE")"
test "$(wc -c <.local/edgecli.downloaded | tr -d ' ')" = \
  "$(jq -r .artifact.size <<<"$RESOLVE")"
```

使用 API 流程开始前保存的公钥，校验精确的规范化 Manifest 和 Ed25519 签名：

```bash
decode_b64url() {
  value="$1"
  case $((${#value} % 4)) in
    2) value="${value}==" ;;
    3) value="${value}=" ;;
  esac
  printf '%s' "$value" | tr '_-' '/+' | openssl base64 -d -A
}

MANIFEST_RESPONSE="$(curl --fail-with-body -sS "$RELEASE_URL/manifest" \
  -H "Authorization: Bearer $READER_TOKEN")"
decode_b64url "$(jq -r .manifest <<<"$MANIFEST_RESPONSE")" >.local/manifest.json
decode_b64url "$(jq -r .signature <<<"$MANIFEST_RESPONSE")" >.local/manifest.sig
test "$(openssl dgst -sha256 .local/manifest.json | awk '{print $NF}')" = \
  "$(jq -r .manifestSha256 <<<"$MANIFEST_RESPONSE")"
openssl pkeyutl -verify -pubin -inkey .local/compose-public.pem -rawin \
  -in .local/manifest.json -sigfile .local/manifest.sig
```

文件系统存储不提供公开预签名 URL。下载必须使用 `redirect=false`，由 API
代理并鉴权：

```bash
REDIRECT_RESOLVE="$(curl --fail-with-body -sS \
    "$BASE_URL/api/v1/repositories/$REPO/packages/$PACKAGE/channels/candidate/resolve?os=linux&arch=arm64&role=binary&redirect=false" \
  -H "Authorization: Bearer $READER_TOKEN")"
curl --fail-with-body -sS "$(jq -r .downloadUrl <<<"$REDIRECT_RESOLVE")" \
  -o .local/edgecli.redirected
```

## 验证

除非显式启用，E2E 测试会在访问 Docker 或 HTTP 之前跳过。手动完成初始化后，
可以复用管理员 Token：

```bash
go test ./tests/e2e -count=1 -v
ARTIFACT_REPOSITORY_E2E=1 \
E2E_COMPOSE_PROJECT="$AR_PROJECT" \
E2E_ADMIN_TOKEN="$ADMIN_TOKEN" \
E2E_PUBLIC_KEY_FILE="$PWD/.local/compose-public.pem" \
go test ./tests/e2e -count=1 -v

go test ./... -race -count=1
go vet ./...
test -z "$(gofmt -l .)"
git diff --check
```

默认 E2E 会验证 filesystem 的代理下载与显式重定向拒绝。

这些命令只会修改本地服务数据和文件，不会自动暂存、提交或推送 Git 变更。

运维配置、Helm 安装、备份和恢复流程见
[`docs/operations.md`](docs/operations.md)。

已认证请求按 Token 分为 `read`、`mutation` 和 `upload` 三类，并使用可配置的
限流阈值。超出阈值时返回带有 `Retry-After` 的 `429 rate-limit-exceeded`；
默认值和环境变量见运维指南。
