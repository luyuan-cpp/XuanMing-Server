# 策划本地启动手册(Pandora 后端)

> 给策划的最简上手:**只要装 Docker,双击一个文件,整套后端就跑起来。**
> 不需要装 Go、不需要会编译。

## 一、第一次准备(只做一次)

1. 安装 **Docker Desktop**:https://www.docker.com/products/docker-desktop/
   - 装完按提示重启电脑。
   - 启动 Docker Desktop,等右下角**鲸鱼图标变绿**(表示 Docker 已就绪)。
   - (如果你机器有 `winget`,也可以直接双击下面的启动脚本,它会尝试自动安装。)
2. 用 Git 把本仓库拉到本地(已有就 `git pull` 更新到最新)。

## 二、日常使用

| 操作 | 双击这个文件 |
|---|---|
| 启动整套后端 | `策划一键启动.cmd`(仓库根目录) |
| 停止 | `策划一键停止.cmd` |

- **首次启动**:会在容器内编译镜像,稍慢(几分钟),请耐心等。
- **之后启动**:复用缓存,很快。
- **更新后启动**:`git pull` 之后再双击启动,会自动重建有改动的服务。
- 启动成功后,客户端网关在 **https://127.0.0.1:8443**。

## 三、机器拉不到镜像（内网 / 断网 / 镜像加速失效）

若这台机器连不上 Docker Hub / 国内加速站(双击启动时会卡在「拉 golang / alpine 镜像失败」,
TLS 超时 / EOF / 403),**不用做任何额外操作**:仓库里带了离线镜像包
`deploy/offline-images/pandora-images.tar`(随 git/svn 同步),双击启动脚本时会
**自动检测并导入**,导入后直接起服务。

你要做的只有:

1. `git pull` / `svn update` 确保拿到最新的 `deploy/offline-images/pandora-images.tar`。
2. 双击「策划一键启动-含战斗.cmd」即可(脚本自动导入离线镜像 + 起服务,无需手动命令)。

> 离线包由能联网的机器用 `pwsh tools/scripts/export_images.ps1 -Build` 生成并提交。
> 基础设施(mysql/redis/kafka 等)不在包内;若目标机基础设施也拉不到,用 `-IncludeInfra` 重新打包。
> 若这台机器其实能联网、想强制重新构建最新镜像:命令行加 `-Rebuild`(如 `pwsh tools/scripts/start.ps1 -Mode battle -Rebuild`)。

## 四、常见问题

- **提示 Docker 没装 / 没运行**:把 Docker Desktop 启动起来,等鲸鱼图标变绿,再双击启动。
- **第一次特别慢**:正常,是在下载基础镜像 + 编译服务,只有第一次这样。
- **想看服务起没起来**:命令行执行 `pwsh tools/scripts/play.ps1 -Status`。
- **数据会丢吗**:停止不会删数据(MySQL/Redis 数据卷保留),下次启动数据还在。
- **报错了**:把窗口里**红色 `[ERR]`** 那几行截图发给后端同学。

## 五、原理(给好奇的人)

- 策划机器上**只要 Docker**:服务是在 Docker 容器里编译和运行的(多阶段 Dockerfile),
  所以不用在本机装 Go,也不会有"构建产物"提交进仓库。
- 这套脚本是 `tools/scripts/start.ps1 -Mode docker` 的"策划友好包装"
  (`tools/scripts/play.ps1`),真正的构建/启动复用已验证的链路。
- 开发同学要断点调试用 `local` 模式:见 `tools/scripts/start.ps1` 注释。
