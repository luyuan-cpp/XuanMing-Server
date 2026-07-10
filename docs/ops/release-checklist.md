# Pandora 发布前清单（Release Checklist）

> 用途:把发布前**必须从 dev 切到生产**的所有开关、证书、配置列成一张可勾选的清单,
> 防止"开发环境好好的,打包发给玩家就连不上 / 不安全"。
> 配套一键预检脚本:[`tools/scripts/release_preflight.ps1`](../../tools/scripts/release_preflight.ps1)。

## 0. 为什么需要这份清单

很多 dev 开关的**默认值就是 dev 态**(尤其 UE 客户端),直接打包会把 dev hack 发给玩家。典型后果:

- `GatewayHost = 127.0.0.1`(UE 头文件默认)→ 打包后连本机,**玩家根本连不上**。
- `bAutoLoginForDev = true`(UE 头文件默认)→ 自动用 dev 账号登录,必须在生产 ini 显式关闭。
- 后端 `dev_skip_password: true` → **任意账号名可登录任意 player_id**。
- mkcert 自签证书 → 玩家设备不信任,握手失败(就是本机 UE 当前遇到的问题)。

**铁律:发布前必须跑预检脚本,FAIL 一项都不许打包。**

---

## 1. 一键预检（先跑这个）

```powershell
pwsh tools/scripts/release_preflight.ps1 `
  -UeGameIni <UE工程路径>\Config\DefaultGame.ini `
  -BackendConfigDir E:\work\Pandora\services `
  -ConfigGlob '*-prod.yaml' `
  -EnvoyCert <生产证书 cert.pem 路径> `
  -ExpectedGatewayHost gateway.yourgame.com
```

- 全 PASS → 退出码 0,可进入打包/部署。
- 任一 FAIL → 退出码 1,按下面分项修复。**建议把这条命令接进 CI / 打包脚本作为前置门禁。**

---

## 2. 分项清单（脚本拦不全的人工项也在这）

### 2.1 UE 客户端（隐患最大）

UE `PandoraBackendSubsystem` 的 dev 开关默认值在 C++ 头文件里,生产**必须**在打包用的 ini
(`Config/DefaultGame.ini` 或 Shipping/Platform 专用 ini)的 `[/Script/Pandora.PandoraBackendSubsystem]` 段**显式覆盖**:

- [ ] `GatewayHost=` **真实域名**(如 `gateway.yourgame.com`),不是 `127.0.0.1`
- [ ] `GatewayPort=443`(或你的生产网关端口)
- [ ] `bDevInsecureTls=False`  ← **关键安全项**,强制校验 TLS 证书
- [ ] `bAutoLoginForDev=False`  ← 关掉自动 test 账号登录
- [ ] `DevLoginAccount=` / `DevLoginPasswordHash=` 清空(不带 dev 口令进包)
- [ ] 登录密码在客户端**先 sha256** 再填 `password_hash`(对齐 [login.proto](../../proto/pandora/login/v1/login.proto) 契约;当前实现是直接透传,**发布前必须补 sha256**)
- [ ] 打 **Shipping** 配置(非 Development/DebugGame),`UE_BUILD_SHIPPING` 关掉调试日志/作弊指令

> 生产 ini 推荐写法(显式覆盖头文件默认):
> ```ini
> [/Script/Pandora.PandoraBackendSubsystem]
> GatewayHost=gateway.yourgame.com
> GatewayPort=443
> bDevInsecureTls=False
> bAutoLoginForDev=False
> DevLoginAccount=
> DevLoginPasswordHash=
> ```

### 2.2 后端服务

> 9 个现役服务已备好生产模板 `services/**/etc/<svc>-prod.yaml.example`(占位符版,入库安全)。
> 部署时 `cp <svc>-prod.yaml.example <svc>-prod.yaml`,把所有 `__占位符__` 换成真实值,
> 用 `-conf etc/<svc>-prod.yaml` 启动。⚠️ 真实 `*-prod.yaml` 已被 `services/.gitignore` 忽略,
> **绝不入库**(Kratos file source 无 env 替换,secret 直接写在 yaml,故真值文件不能进 git)。

- [ ] 每个服务用独立的 **`*-prod.yaml`**(从 `.example` 复制),不要直接拿 `*-dev.yaml` 上线
- [ ] 所有 `__占位符__` 已替换:`__REDIS_HOST__` / `__REDIS_PASSWORD__` / `__MYSQL_HOST__` /
      `__MYSQL_STRONG_PASSWORD__` / `__KAFKA_BROKER_*__` / `__JWT_SHARED_SECRET_32B_CHANGE_ME__`
- [ ] `login.dev_skip_password: false`(或删除该键)← 关掉免密登录
- [ ] 所有服务 `server.grpc.enable_reflection`: 不写 / false ← 关 reflection,防 schema 泄露
- [ ] DSN / Redis / Kafka 地址改为生产实例,**强密码**(不是 `pandora_dev_pwd`)
- [ ] JWT secret 在 login / matchmaker / hub_allocator / Envoy jwt_authn **四处完全一致**,≥32 字节强随机
- [ ] ds_allocator / hub_allocator `agones.enabled: true`(接真 Agones,不再用 Mock)
- [ ] `insecure_skip_tls_verify`(ds_allocator / hub_allocator 的 Agones 段)保持 false
- [ ] 密码 / token / secret 不写进入库 yaml(真值文件已被 gitignore,只提交 `.example` 模板)
- [ ] Linux DS 符号文件已随版本归档,crash dump / minidump / UE crash report 能自动上传
- [ ] DS metrics 已接 Prometheus / Grafana,至少能看在线、tick p95/p99、CPU、内存、网络、心跳、崩溃次数

> Linux DS 崩溃和性能排障手册见
> [`docs/ops/linux-ds-observability.md`](linux-ds-observability.md)。

### 2.3 边缘 TLS 证书（Envoy）

- [ ] 证书由**公网 CA** 签发(Let's Encrypt 免费 / 商业 CA),**不是 mkcert 自签**
- [ ] 证书 **SAN = 真实域名**,不含 IP(公网 CA 不给 IP 签)
- [ ] 证书自动续期已配置(Let's Encrypt 90 天,certbot/acme 定时续)
- [ ] Envoy listener 绑生产证书 + 真实域名,JWT secret 用生产值(不是 dev 共享 secret)

> 完整证书策略(dev vs 生产、为什么要域名、成本)见
> [`docs/design/gateway-decision.md`](../design/gateway-decision.md) §14。

### 2.4 数据 / 账号

- [ ] 清掉 dev 种子账号(`test` / `test1`~`test10000`,密码 `abc`)
- [ ] 生产 DB 不带任何 dev 测试数据
- [ ] 备份 / 迁移脚本就绪

---

## 3. 开发机本地（不入包、不影响发布）

dev 联调要让本机 UE 用真证书认证连自签 Envoy。跑一次导入脚本即可(**不碰引擎、不进发行包**):

```powershell
pwsh tools/scripts/import_dev_ca.ps1
```

它把公开 dev CA 放进客户端工程 `Config/Certificates/`,并在 `DefaultEngine.ini` 设
`[SSL] DebuggingCertificatePath` 把 dev CA **叠加**到引擎公网 CA 包之上(不替换、仅非 Shipping 生效)。
之后 UE 用 `bDevInsecureTls=false` 也能过 TLS 校验。详见 [deploy/dev-ca/README.md](../../deploy/dev-ca/README.md)。

> ⚠️ 这只让**开发机 / 编辑器**信任本地 dev CA,证书在 `Config/`(不在 `Content`)→ 不打进发行包,与玩家无关。
> 生产靠公网 CA + 真实域名,玩家零配置。

---

## 4. 防呆机制（已落地）

已把 UE 危险开关改成**生产安全默认 + 编译期剔除**(`PandoraBackendSubsystem`):

- `bDevInsecureTls` 头文件默认翻为 **false**(生产安全意图)。dev 联调**推荐用证书认证**(见 §3 方案A 导入 mkcert CA),`bDevInsecureTls` 保持 false 也能正常 TLS 校验。注意:当前字段是意图标记,不直接控制 libcurl;实际信任来自 `[SSL] DebuggingCertificatePath`。
- `bAutoLoginForDev` / dev 账号 / dev 口令在 **Shipping 包编译期剔除**(`#if UE_BUILD_SHIPPING`):即使生产 ini 误设 `bAutoLoginForDev=true`,发行包也不会走自动登录。

> 这两项需 Codex/人 编译验证(UE Win64 + Linux DS,Shipping/Development 各编一遍,确认 `#if UE_BUILD_SHIPPING` 两分支均编过),按 [`AGENTS.md`](../../AGENTS.md) §11.1。

仍需人工保证的(脚本 + 清单已覆盖):`GatewayHost` 生产域名覆盖、后端 `*-prod.yaml`、公网 CA 证书。

> **dev 用不用证书认证?** 用,而且推荐用。dev 走证书认证的正确做法就是 §3 方案A(导入 mkcert CA),这样 `bDevInsecureTls=false` 也能过 TLS 校验,和生产同一条链路。

---

## 5. 发布流程顺序

1. 跑 `release_preflight.ps1` → 必须全 PASS
2. 后端用 `*-prod.yaml` 部署,确认 dev_skip_password/reflection 关
3. Envoy 换公网 CA 证书 + 真实域名
4. UE 用生产 ini 打 **Shipping** 包
5. 用真机 / 干净环境(没装过 mkcert CA)验证登录链路:登录 → 进大厅 → 匹配 → 战斗 → 结算
6. git push / tag / release(人手动执行,见 AGENTS §3)

---

## 6. 版本记录与可追溯(线上发布必看)

线上**不提交任何二进制**:git 只存源码 + tag,可部署的二进制以**容器镜像**存在 registry。
用**同一个版本号**把三处串起来,做到「线上某个 pod ↔ git 某次提交」可追溯。

### 6.1 一个版本号,焊进三处

```
git tag v1.2.3                       ① 源码快照(人手动打,AGENTS §3)
  └─同号─► 镜像 pandora/<svc>:v1.2.3  ② 可部署产物(push 到 registry)
            └─同号─► k8s newTag: v1.2.3 ③ 线上实际部署的版本
```

二进制内部也烙了版本(编译期 `-ldflags -X` 注入 [`pkg/version`](../../pkg/version/version.go)):

- 每个服务**启动首行日志**就打印 `version=v1.2.3 commit=abc1234 built=... go=...`(由 `pkg/log.Setup` 统一输出)。
- 版本号来源:`tools/scripts/start.ps1` 的 `Get-VersionInfo` 跑 `git describe --tags` + `git rev-parse --short HEAD`,
  通过 Dockerfile build-arg(`VERSION`/`GIT_COMMIT`/`BUILD_TIME`)注入。
- 本地随手 `go run`(没注入)时回退 `version=dev commit=unknown`,不影响启动。

> 排障时:`kubectl logs <pod> | head -1` 看启动版本行,或 `kubectl get deploy -o jsonpath` 看 image tag,
> 两者应一致 = 没人偷偷热改过镜像。

### 6.2 线上发布打版本(命令)

```powershell
# 1) 人手动打 git tag(冻结源码快照)
git tag v1.2.3
git push origin v1.2.3

# 2) 构建 + 打同号镜像 + push 到 registry + apply 到生产(start.ps1 online 模式已支持)
pwsh tools/scripts/start.ps1 -Mode online -Env prod -ProdKubeContext pandora-prod `
  -Registry registry.yourcorp.com -Tag v1.2.3 -BuildPush `
  -BattleDsImage registry.yourcorp.com/pandora/battle-ds:v1.2.3 `
  -HubDsImage registry.yourcorp.com/pandora/hub-ds:v1.2.3 `
  -DsGatewayAddr pandora-envoy.pandora.svc:8444
```

- `-BuildPush` 会构建 20 个服务镜像(自动带 git 版本烙印)、tag 成 `v1.2.3`、push 到 registry,
  再用该 tag 覆盖 [`overlays/online/kustomization.yaml`](../../deploy/k8s/overlays/online/kustomization.yaml) 部署。
- **git push / tag 由人手动执行**(AGENTS §3),脚本不替你推。

> dev/docker 模式的镜像 tag 固定 `:dev`(跟最新代码走,不需要版本号);**只有线上发布才打 `v1.2.3` 这种版本 tag**。
