# GitHub Actions 构建

仓库已包含 `.github/workflows/build-fnos-fpk.yml`。

1. 将本目录内容上传到 GitHub 仓库根目录。
2. 打开 GitHub 仓库的 **Actions** 页面。
3. 选择 **Build fnOS FPK**。
4. 点击 **Run workflow**。
5. 构建完成后，在该次运行底部下载 `fnos-speedtest-1.10.13-x86_64` Artifact。

CI 使用 Go 1.22、官方 `github.com/showwin/speedtest-go v1.7.11` 依赖，并下载飞牛官方 `fnpack 1.2.3` 打包。
