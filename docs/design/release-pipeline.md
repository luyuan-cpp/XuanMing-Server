# Pandora 打包发布线(制品不进版本库)

> 2026-07-23 落地。回应「Packages 提交进 SVN / pandora-images.tar 提交进 git」两个反模式,
> 按业界标准四层分离改造:版本库只放源码,构建产物进制品目录,发布按 manifest 可追溯可回滚。
> 决策登记:`pandora-arch.md` §11「镜像分发 2026-07-23」行;旧「离线镜像包随仓库同步」过渡方案同日退役。

## 1. 四层架构

```
版本库层(SVN/git,只有源码) → CI 构建层(Jenkins) → 制品目录层(不可变+版本号) → 发布层(manifest 晋级)
```

| 层 | 载体 | 本项目落点 |
|---|---|---|
| 版本库 | 客户端 SVN + 后端 git | Packages/ 已解除纳管+svn:ignore;镜像 tar 已移出 git;服务端钩子拒收回流 |
| CI | Jenkins | 客户端 `Tool/Build/Jenkinsfile`(已有,本次改造)+ 后端仓根 `Jenkinsfile`(新增) |
| 制品目录 | 本地/共享盘目录 | `PANDORA_ARTIFACT_ROOT`(默认 `F:\work\artifacts`);将来可平移 FTP/MinIO/Harbor |
| 发布 | release manifest | `make_release.ps1` 产出 `releases/<name>.json`,离线交付按 manifest 取制品 |

## 2. 制品目录布局与铁律

**两轨分仓**(Snapshot 快照轨 / Release 发布版本轨,类比 Nexus SNAPSHOT/RELEASE 仓):

```
<PANDORA_ARTIFACT_ROOT>\
├── snapshots\                                    dev 快照轨(来源戳命名,激进清理)
│   ├── client\<branch>\<flavor>\r<svn版本>\        UE 包
│   ├── images\<g+git短sha>\pandora-images.tar      业务镜像离线包
│   └── images\latest.json                          快照指针
└── releases\                                     发布版本轨(语义版本,不可变,永久保留)
    ├── client\<branch>\<flavor>\<版本>\             UE 包
    ├── images\<版本>\pandora-images.tar             业务镜像离线包
    ├── images\latest.json                          发布指针
    └── manifests\<版本>.json / <版本>.md            release manifest + 人可读 notes
```

轨道由**有没有版本号**决定:发布脚本带 `-Version` = release 轨(releases\,目录名=版本号);
不带 = snapshot 轨(snapshots\,目录名=git sha / svn rev)。每个制品目录都带 `build-info.json`(含 channel)+ `sha256sums.txt`。

三条铁律(脚本强制):

1. **不可变**:版本目录已存在即拒绝覆盖(CI 幂等重跑用 `-SkipIfExists` 静默跳过);
2. **原子发布**:内容先写 `.tmp-` staging,再整目录 rename 上线,不存在半截制品;
3. **可追溯**:snapshot 版本号=源码版本(SVN rev / git sha);release 版本号=语义版本。
   脏工作区在 release 轨默认拒绝(`-AllowDirty` 仅限内测);snapshot 轨允许脏(带 `-dirty-时间戳`)。

清理:`artifacts_retention.ps1` **只清 snapshots\**(每流留最近 N),`releases\` 永不触碰。

## 3. 脚本清单

| 脚本 | 仓库 | 职责 |
|---|---|---|
| `Tool/Build/PublishPackages.ps1` | 客户端 | Packages\<flavor> → `client\...\r<rev>`;svnversion 强校验 |
| `tools/scripts/publish_offline_images.ps1` | 后端 | 复用 `export_images.ps1` 出 tar → `images\<gitsha>`;从 tar manifest 提镜像 ID |
| `tools/scripts/fetch_offline_images.ps1` | 后端 | 制品目录 → `deploy/offline-images/pandora-images.tar`(校验后落地;下游一键启动/import 流程不变) |
| `tools/scripts/make_release.ps1` | 后端 | 生成 release manifest(镜像版本+UE 包引用+configtable manifest 摘要) |
| `tools/scripts/artifacts_retention.ps1` | 后端 | 每流保留最近 N 版(默认 10);release 引用的版本永不删;默认 dry-run |
| `tools/scripts/artifacts_lib.ps1` | 后端 | 公共函数(root 解析/sha256sums/原子发布),被上述脚本 dot-source |
| `tools/scripts/ci_backend.ps1` | 后端 | CI 构建入口:按 go.work use 清单逐模块 build+test |

## 4. 版本库防回流(服务端钩子,`tools/vcs-hooks/`)

本地 ignore 只是君子协定,强制力在服务器端:

- **SVN**(客户端仓):`svn-pre-commit.sh`(Linux svnserve/Apache)/ `svn-pre-commit.bat` + `.ps1`(VisualSVN)。
  黑名单:`Packages/`、任意层级 `Saved/ Intermediate/ DerivedDataCache/`、`*.tar *.pak *.ucas *.utoc`。
  **注意:本仓有意纳管 `Pandora/Binaries`(策划靠 svn 同步编辑器 DLL),Binaries 不拉黑。**
  救急放行:提交日志带 `[hook-override]`(仅管理员)。部署需 SVN 服务器管理员按脚本头部说明挂载。
- **git**(后端仓):`git-pre-receive.sh`(自建裸仓库);托管平台改用 GitHub push ruleset / GitLab push rules
  (路径 `*.tar` 拒收 + 单文件 50MB 上限)。

## 5. CI 流水线(两轨:dev 快照自动 / release 版本手动)

每个仓库两条流水线,同分支双触发:

| 轨 | 后端 | 客户端 | 触发 | 产出 |
|---|---|---|---|---|
| **dev 快照** | `Jenkinsfile` | `Tool/Build/Jenkinsfile` | pollSCM 自动 | snapshots\(git sha / r<rev>) |
| **release 版本** | `Jenkinsfile.release` | `Tool/Build/Jenkinsfile.release` | 手动传 VERSION | releases\(vX.Y.Z) + manifest |

- **dev 后端**:pollSCM → `ci_backend.ps1`(全模块 build+test)→ `publish_offline_images.ps1 -SkipIfExists`(无版本 → snapshot)。
- **dev 客户端**:改动检测 → Preflight → Package.bat → `PublishPackages.ps1 -SkipIfExists`(无版本 → snapshot);
  `Package.bat` 在 `BUILD_INFO.txt` 落 `Version=<ProjectVersion>` + `Revision=<svn rev>`。
- **release 后端**:手动 VERSION → build+test → `publish_offline_images.ps1 -Version <V>`(镜像自报版本)→ `make_release.ps1 -Version <V>`。
- **release 客户端**:手动 VERSION → `PackageSet.ps1 -Version <V>`(构建 + `PublishPackages -Version` 发 releases;DS 自动锁源码引擎)。
- **UE DS 引擎坑**:Server(DS)目标必须用能编 Server 的**源码/自制引擎**;Epic 发行版(launcher)引擎会报
  `Server targets are not currently supported from this engine distribution`。`PackageSet.ps1` 打 Server 时
  自动从注册表挑源码引擎(或 `-EnginePath` 指定),避免默认解析翻到发行版。
  构建机要求:Go 1.26.5、Docker Desktop、pwsh、git、svn 命令行(客户端节点另需 UE 引擎)。

镜像**在线发布**(推 registry)已有独立机制:`start.ps1 -BuildPush`(clean commit 强制 + 不可变 tag 门禁),
与本离线制品线并行,互不替代;有内网 Harbor 后,离线 tar 流退化为"发布时从 registry 现场导出"。

## 6. 分发方式迁移对照

| 消费场景 | 旧方式 | 新方式 |
|---|---|---|
| 内网机起后端服务 | svn/git 同步拿入库 tar | `fetch_offline_images.ps1`(共享盘设 `PANDORA_ARTIFACT_ROOT`)→ 一键启动照常 |
| 拿 UE 打包产物 | `svn update` Packages | 制品目录 `client\<branch>\<flavor>\r<rev>\` 直接取(带校验和) |
| DS 镜像构建取 Linux 包 | 同级仓库 Packages 自动发现 | 不变(本机构建输出仍在 Packages);跨机时 `-SourcePkg` 指到制品路径 |
| 正式发布 | 无 manifest | `make_release.ps1` → 按 `releases/<name>.json` 交付/回滚 |

## 6b. 版本化发布(语义版本 + 修复内容)

制品的目录名是**来源戳**(svn rev / git sha,溯源用),不是人可读的**发布版本号**。
正规发布另有一层语义版本:

- **版本号来源**:
  - 客户端 UE 包:`Pandora/Config/DefaultGame.ini` 的 `ProjectVersion`,cook 时烙进包内,运行时自报。发布前手动 bump。
  - 后端镜像:git tag,`git describe --tags` 自动注入 `pkg/version.Version`(机制已在,只需打 tag)。
- **修复内容来源**:仓库根 `CHANGELOG.md`(Keep a Changelog 格式),每次发布前在顶部新增版本段落。
- **绑定**:`make_release.ps1 -Version <版本>` 读 CHANGELOG 对应段 → 写进 `releases/<版本>.json`(机器可读)
  与 `releases/<版本>.md`(人可读),并记录每个制品的来源戳 / 镜像 digest。dirty 来源默认拒绝(`-AllowDirty` 放行内测)。

标准发布流:

```
① 定版本 v0.1.0:CHANGELOG.md 加 [0.1.0] 段 + 客户端 DefaultGame.ini ProjectVersion=0.1.0
② 后端:git tag v0.1.0 <clean commit> → publish_offline_images.ps1(镜像自报 v0.1.0)
③ 客户端:PackageSet.ps1(包自报 0.1.0)→ PublishPackages 发布
④ make_release.ps1 -Version v0.1.0 -ClientPackages <5 个包路径>
   → release manifest + notes(含修复内容)
```

版本号三处必须一致:`DefaultGame.ini ProjectVersion` = 后端 `git tag` = `make_release -Version` = `CHANGELOG` 段落。

## 7. 剩余事项(诚实清单)

- SVN 服务端钩子需仓库管理员部署(本仓只提供脚本);git 托管平台规则需人配置。
- git 历史中的 177MB tar 仍在历史里(仅解除跟踪);要瘦身需 `git filter-repo` 重写历史并全员重新克隆,单独拍板。
- Jenkins 服务本体与构建机 agent 的安装/凭据属环境操作(AGENTS.md §11.1,Codex/人执行)。
- 制品根迁 FTP/MinIO/Harbor 时:只改 `PANDORA_ARTIFACT_ROOT` 语义(换成 rclone remote),脚本布局不变。
