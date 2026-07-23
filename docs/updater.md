# 私有 CLI 发布与自更新

Forge 把每个 CLI 作为独立的 Product 管理。Product 自动绑定内部
`cli-releases/<slug>` Package、`candidate`/`stable` Channel 和一个只读
Install Key。管理控制台只呈现 CLI、版本、平台制品和安装命令，不要求使用者理解底层
Repository、Package 或权限模型。

## 发布模型

每个版本使用严格 SemVer，并可包含多个 `os/arch/variant` 制品。每个平台选择一种安装
策略：

| 策略 | 制品 | 适用场景 |
| --- | --- | --- |
| `self-replace` | 单个原始可执行文件 | CLI 自身就是完整程序，包括从 U 盘以 `./cli` 启动的程序 |
| `bundle` | `.tar.gz` 或 `.zip` | 解压后包含多个文件、配置模板和安装脚本 |

Bundle 必须声明归档内入口。可选 Hook 只有三个签名阶段：

- `preflight`：版本目录提交和切换前执行；
- `post-install`：新版本成为 `current` 后执行；
- `verify`：最后验证新安装；失败会恢复旧 `current`。

Hook 的路径、参数和超时都在 Ed25519 签名 Manifest 中，更新器不通过 shell 拼接命令。
归档解压会拒绝绝对路径、`..`、符号链接、硬链接和设备文件，并限制文件数及解压大小。

## Web 与 CI

首次创建 CLI 最简单的方式是打开 `/dashboard/`：

1. 点击“添加 CLI”，填写显示名称、稳定标识和命令名；
2. 在 CLI 详情页点击“发布新版本”；
3. 一次选择多个平台文件，确认自动识别的系统和架构；
4. 发布完成后复制页面中的当前用户安装命令。

原始二进制会自动使用 `self-replace/raw/0755`。`.tar.gz`、`.tgz` 和 `.zip`
会使用 `bundle`，页面默认只额外要求归档入口；需要执行安装脚本时，再展开“高级安装
设置”配置 `preflight`、`post-install` 或 `verify` Hook。已经发布但尚未成为 stable
的版本，可以在版本历史中点击“设为当前”；中断产生的 draft 可以删除后用同一路径
制品重试。

CI 使用同一套 API。Product 预先创建后，内部坐标固定为
`cli-releases/<product-slug>`。流水线依次上传不可变 Artifact、创建 Release、用
`install` 字段关联平台、发布并晋级 stable。例如单文件的关联请求为：

```json
{
  "artifactPath": "products/edgectl/1.4.0/linux/arm64/edgectl",
  "os": "linux",
  "arch": "arm64",
  "role": "binary",
  "install": {
    "strategy": "self-replace",
    "format": "raw",
    "mode": "0755"
  }
}
```

Bundle 的 `install` 示例：

```json
{
  "strategy": "bundle",
  "format": "tar.gz",
  "entrypoint": "bin/edgectl",
  "mode": "0755",
  "hooks": [
    {
      "phase": "verify",
      "path": "bin/edgectl",
      "args": ["version"],
      "timeoutSeconds": 15
    }
  ]
}
```

## 首次安装

控制台给出的命令形如：

```bash
curl -fsSL https://forge.example/i/<install-key>/edgectl/install | sh
```

默认只写当前用户目录：

- 命令入口：`$HOME/.local/bin/<command>`；
- Bundle 版本：`$HOME/.local/share/forge/<product>/versions/<version>`；
- Bundle 当前版本：`$HOME/.local/share/forge/<product>/current`。

可以用 `FORGE_INSTALL_DIR` 和 `FORGE_INSTALL_ROOT` 覆盖这两个根目录，不使用
`sudo`。Install Key 是永久、只读、仅限该 Product 的下载凭据，可以直接编译进 CLI；
仍应按密码保护，不要写入普通日志。轮换后旧 Key 保留 30 天，应该在这个窗口内发布
内嵌新 Key 的版本。

## 在 Go CLI 中嵌入更新

`pkg/forgeupdate` 把“检查”和“安装”分开：每次启动调用 `Check`，只有发现更高版本时
才由宿主 CLI 提示 `y/n`，用户同意后再调用 `Apply`。`Check` 只拉取并验证签名
Manifest，不下载制品。

```go
executable, err := os.Executable()
if err != nil {
    return err
}
target, err := filepath.EvalSymlinks(executable)
if err != nil {
    return err
}

source, err := forgeupdate.NewHTTPSource(forgeupdate.HTTPSourceConfig{
    BaseURL:    "https://forge.example",
    Product:    "edgectl",
    InstallKey: embeddedInstallKey,
})
if err != nil {
    return err
}

client, err := forgeupdate.NewClient(forgeupdate.ClientConfig{
    Source: source,
    Verifier: forgeupdate.Verifier{
        TrustedKeys: map[string]ed25519.PublicKey{
            forgeupdate.KeyID(releasePublicKey): releasePublicKey,
        },
    },
    SelfBinary: forgeupdate.SelfBinaryOptions{
        Target: target,
        Probe: func(ctx context.Context, candidate forgeupdate.Candidate) error {
            output, err := exec.CommandContext(ctx, candidate.Path, "version").CombinedOutput()
            if err != nil {
                return err
            }
            if !bytes.Contains(output, []byte(candidate.Version)) {
                return fmt.Errorf("candidate reports the wrong version")
            }
            return nil
        },
    },
    Bundle: forgeupdate.BundleOptions{
        Root: filepath.Join(userHome, ".local", "share", "forge", "edgectl"),
    },
})
if err != nil {
    return err
}

plan, err := client.Check(ctx, forgeupdate.Selection{
    Repository:     "cli-releases",
    Package:        "edgectl",
    CurrentVersion: currentVersion,
    OS:             runtime.GOOS,
    Arch:           runtime.GOARCH,
    Role:           "binary",
})
if errors.Is(err, forgeupdate.ErrNoUpdate) {
    return nil
}
if err != nil {
    return err
}

fmt.Printf("发现新版本 %s，是否升级？[y/N] ", plan.Version())
if !readYes(os.Stdin) {
    return nil
}
result, err := client.Apply(ctx, plan)
if err != nil {
    return err
}
if receipt := result.SelfBinary(); receipt != nil {
    return receipt.Finalize()
}
if receipt := result.Bundle(); receipt != nil {
    return receipt.Finalize()
}
return nil
```

`releasePublicKey` 必须随 CLI 独立分发，不能从同一更新响应动态信任。HTTP Source
不会发送后台管理 Bearer Token、Cookie 或跟随重定向，并将下载 URL 固定到已验签的
`version + sha256`。

`self-replace` 会在目标二进制同一目录创建候选文件，校验大小和 SHA-256，执行
`Probe`，备份旧文件后原子替换。以 `/Volumes/.../cli` 或其他 U 盘路径启动时，只要
介质仍可写，替换的就是该路径；只读或已拔出的介质会返回错误并保留旧文件。

Windows 正在运行的 `.exe` 不能由自身可靠替换，更新器会返回
`ErrHelperRequired`，宿主需要使用独立 helper 完成最终 rename。
