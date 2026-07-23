# 离线镜像本地目录(构建/拉取落点,不入库)

拉不到 Docker Hub / 内网受限的机器用离线镜像包起服务。**本目录只是 tar 的本地落点,
tar 本身不进 git/svn**(2026-07-23 起走制品目录发布线,旧「tar 入库随仓库同步」过渡方案退役,
见 `docs/design/release-pipeline.md`)。

## 里面放什么

- `pandora-images.tar` —— 21 个业务镜像(`pandora/*:dev`)打包,约 150 MB。
  由构建机 `publish_offline_images.ps1` 生成并发布到制品目录;目标机用
  `fetch_offline_images.ps1` 拉到本目录。`.gitignore` 已排除,不会被提交。

> 业务运行镜像用 `scratch` 基底(见 `deploy/services/Dockerfile`),体积小。
> 基础设施镜像(mysql/redis/kafka/etcd/prometheus/grafana/loki/alloy/envoy)不在此包内;
> 目标机也缺时用 `export_images.ps1 -Build -IncludeInfra -Out D:\pandora-full-images.tar`
> 打制品目录外的完整大包。

## 构建机:构建并发布

```powershell
# 从当前源码重建 21 个业务镜像并发布到制品目录(要求 git 工作区干净,版本号 = git sha)
pwsh tools/scripts/publish_offline_images.ps1
```

制品落在 `<PANDORA_ARTIFACT_ROOT>\images\<版本>\`(默认根 `F:\work\artifacts`),
带 `images-manifest.json`(每镜像 ID)与 `sha256sums.txt`,版本目录不可变。

## 目标机:拉取并启动

```powershell
# 1) 目标机能访问制品目录(本机路径或共享盘;必要时 setx PANDORA_ARTIFACT_ROOT \\共享机\artifacts)
# 2) 拉取(自动校验 sha256):
pwsh tools/scripts/fetch_offline_images.ps1
# 3) 直接双击一键启动 .cmd —— 启动脚本自动检测并 docker load 本目录的 tar,无需手动命令。
```

只跑不改代码的机器(打包机/内网运行机)仍可 `setx PANDORA_OFFLINE 1` 强制纯离线:
启动脚本直接导入离线包 → 校验齐全 → 起服务,不联网、不构建。
需要临时构建最新代码时加 `-Rebuild`(如 `pwsh tools/scripts/start.ps1 -Mode docker -Rebuild`)。

手动导入(一般用不到):`pwsh tools/scripts/import_images.ps1`。

## 注意

- **不要把 tar 提交进任何版本库**;历史上的入库过渡方案已退役,git 服务端规则会拒收 `*.tar`。
- 镜像有业务改动 → 构建机重跑 `publish_offline_images.ps1`(新 git sha = 新版本目录,不覆盖旧版);
  目标机重新 `fetch_offline_images.ps1` 即可。
- 正式对外交付按 `make_release.ps1` 生成的 release manifest 取对应版本,不直接拿 latest。
