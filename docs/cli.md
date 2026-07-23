# artifactctl

`artifactctl` 是 Artifact Repository 的终端客户端。首版覆盖制品上传、按路径下载、
元数据查看，以及从已签名的 Channel 解析并拉取制品。

如果要让业务 CLI 自己在启动时检查并升级，请使用
[`pkg/forgeupdate`](../pkg/forgeupdate)；完整的 Product、Install Key、两种安装策略和
嵌入示例见 [`updater.md`](updater.md)。

## 构建

```bash
mkdir -p ./bin
go build -o ./bin/artifactctl ./cmd/artifactctl
./bin/artifactctl help
```

## 连接与认证

配置 API 地址和 Bearer Token：

```bash
export ARTIFACT_REPOSITORY_URL=https://artifacts.example.com
export ARTIFACT_REPOSITORY_TOKEN='ar1...'
```

也可以将 Token 保存在权限受限的文件中，并使用 `--token-file`。CLI 不接受命令行
明文 Token 参数，避免 Token 出现在 shell history 或进程列表中。全局参数需要写在命令
之前：

```bash
artifactctl --url https://artifacts.example.com \
  --token-file "$HOME/.config/artifact-repository/token" \
  inspect releases/linux/arm64/edgecli
```

## 上传

远端位置使用 `<repository>/<artifact-path>` 形式。CLI 会先计算本地文件的 SHA-256，
然后以确定的 `Content-Length` 流式上传；服务端返回的路径、大小和校验和必须与本地
文件一致，命令才会成功。

```bash
artifactctl upload ./dist/edgecli \
  releases/linux/arm64/1.2.0/edgecli

artifactctl upload \
  --media-type application/octet-stream \
  --properties '{"commit":"abc123","pipeline":42}' \
  ./dist/edgecli releases/linux/arm64/1.2.0/edgecli
```

成功时，完整的 Artifact 元数据会以 JSON 写到标准输出，便于流水线继续处理。

## 按路径查看和下载

```bash
artifactctl inspect releases/linux/arm64/1.2.0/edgecli

artifactctl download \
  -o ./downloads/edgecli \
  releases/linux/arm64/1.2.0/edgecli
```

下载默认通过 API 代理进行。CLI 会先读取服务端元数据，下载到目标目录内的临时文件，
校验大小和 SHA-256 后再原子安装到目标路径。目标已存在时默认失败；只有显式传入
`--force` 才会替换：

```bash
artifactctl download --force -o ./downloads/edgecli \
  releases/linux/arm64/1.2.0/edgecli
```

服务端使用支持公开预签名 URL 的对象存储后端时，可以使用 `--redirect`。filesystem
模式不支持重定向。CLI 在访问预签名 URL 前会移除 Bearer Token：

```bash
artifactctl download --redirect -o ./downloads/edgecli \
  releases/linux/arm64/1.2.0/edgecli
```

使用 `-o -` 可以把制品写到标准输出。此模式仍会在流结束时校验，但校验失败前已经
写出的字节无法撤回，因此安装可执行文件时应优先使用普通文件输出。

## 从 Channel 拉取

`pull` 用于终端用户或设备端从 `stable`/`candidate` Channel 获取匹配平台的制品。
它要求独立保存的 Ed25519 公钥作为信任根，不会从 signing-key API 自动获取该公钥。
可以通过 `--public-key` 或 `ARTIFACT_REPOSITORY_PUBLIC_KEY` 指定：

```bash
export ARTIFACT_REPOSITORY_PUBLIC_KEY="$PWD/public.pem"

artifactctl pull \
  --channel stable \
  --os linux \
  --arch arm64 \
  --role binary \
  -o ./downloads/edgecli \
  releases/edgecli
```

`pull` 会先验证公钥与 `keyId`、Ed25519 签名、Manifest 中的仓库/Package/版本，以及
所选制品的坐标、路径、大小和 SHA-256；随后下载字节并再次验证大小和 SHA-256。
默认平台来自运行 CLI 的 `GOOS`/`GOARCH`，默认 Channel 为 `stable`，默认 role 为
`binary`。可使用 `--variant`、`--redirect` 和 `--force` 调整行为。

## 退出码

- `0`：成功。
- `1`：网络、认证、服务端响应或完整性校验失败。
- `2`：命令或参数使用错误。
