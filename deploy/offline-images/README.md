# 离线镜像分发目录(随仓库同步,免联网拷贝)

拉不到 Docker Hub / 国内加速站的机器(内网 / 断网 / 加速器被墙)用这里的镜像包离线起服务。

## 里面放什么

- `pandora-images.tar` —— 17 个业务镜像(`pandora/*:dev`)打包,约 150 MB。
  由能联网的机器用 `tools/scripts/export_images.ps1 -Build` 生成后放到这里,
  随仓库(git / svn)同步到其它机器,**不用 U 盘、不用联网拷贝**。

> 说明:业务运行镜像用 `scratch` 基底(见 `deploy/services/Dockerfile`),体积很小,
> 适合入库同步。基础设施镜像(mysql/redis/kafka/etcd/prometheus/grafana/envoy)不在此包内——
> 目标机通常已经拉到过并在跑;若目标机也缺,用 `export_images.ps1 -Build -IncludeInfra`
> 打个含基础设施的大包(会大很多,按需)。

## 生成(能联网的机器)

```powershell
# 构建 17 个业务镜像并打包到本目录
pwsh tools/scripts/export_images.ps1 -Build
```

生成后把 `pandora-images.tar` 纳入版本控制(`git add` / `svn add`)并提交/同步。

## 使用(拉不到镜像的机器)

```powershell
# 1) svn update / git pull 拿到本目录的 pandora-images.tar
# 2) 直接双击「策划一键启动-含战斗.cmd」即可 —— 启动脚本会自动检测并导入离线镜像,无需手动命令。
```

启动脚本(`start.ps1` 的 `Build-AllImages`)会判定:本机缺业务镜像 + 无 golang 构建基础镜像
(= 这台机器多半构建不了)→ 自动 `docker load` 本目录的 tar,导入后齐全就跳过构建直接起服务。

### 打包机 / 内网运行机(不改代码):设一次强制纯离线

如果这台机器**只跑不改代码**(打包机 / 内网运行机),不想每次先试构建、遇 Docker DNS 抖动才兜底,
在它上面**一次性**设一个环境变量,以后双击 cmd 就直接用离线包、完全不 `docker build`:

```powershell
setx PANDORA_OFFLINE 1      # 只需执行一次;之后新开的终端 / 双击 cmd 都会走纯离线
```

设了 `PANDORA_OFFLINE=1` 后,启动脚本直接导入离线包 → 校验齐全 → 起服务,**不联网、不构建、不受 DNS 影响**。
需要临时构建最新代码时,加 `-Rebuild` 覆盖(如 `pwsh tools/scripts/start.ps1 -Mode battle -Rebuild`)。

手动导入(可选,一般用不到):

```powershell
pwsh tools/scripts/import_images.ps1     # 默认读本目录的 pandora-images.tar
```

## 注意

- tar 是二进制大文件,入库会增大仓库体积;更新镜像时**覆盖同名文件**再提交,避免堆历史。
- 镜像有业务改动 → 在联网机重跑 `export_images.ps1 -Build` 覆盖此 tar 再提交。
## ❗ 这是「过渡方案」，不是长期正规做法

把镜像 tar 塞进源码仓库不是大厂标准做法，只是「当前没有内网镜像仓库 + 目标机拉不到公网镜像」时的务实过渡方案。

- **正规做法**：搭内网私有 Registry（Harbor / Nexus / 云厂商 ACR），各机器 `docker pull 内网harbor/pandora/<svc>:<tag>`；源码仓只放文本（代码 / Dockerfile / yaml），二进制制品走制品库。
- **本方案适用前提**：没有内网 Registry、目标机又在受限内网拉不到公网镜像；且运行镜像已用 `scratch` 基底（~150MB）体积可控。
- **后续迁移**：一旦有了内网 Harbor，就把分发切成 `docker pull`，本目录退役。

> 决策记录见本文件；如后续搭建 Harbor，在 `docs/design/pandora-arch.md` §11 补一笔迁移决策。