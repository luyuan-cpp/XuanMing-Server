# Pandora 服务 / RPC 临时关停(Kill-Switch)与自动防护

> 某个 service 出重大问题、或某个 RPC 有 bug,想「临时关掉、修好再开」,不发版、不重启、秒级热生效。
> 本文是操作手册 + 设计说明。代码:`pkg/killswitch`、`pkg/middleware/{killswitch,ratelimit,circuitbreaker}.go`。

## 0. 大厂分层手段(本项目落地状态)

| 层 | 手段 | 粒度 | 生效方式 | Pandora 状态 |
|---|---|---|---|---|
| 1 | Envoy route `direct_response` 503 / 维护页 | 对外整组 / 单 RPC | 改 envoy.yaml + restart | ✅ 示例已就位(注释态) |
| 2 | 注册中心 deregister + 优雅下线 | 整服 | 服务发现刷新 | 🟡 待 etcd registry 真接入(用 `<svc>/*` Kill-Switch 暂代) |
| 3 | **Kill-Switch 功能开关**(本文核心) | 单 RPC / 功能组 / 整服 | etcd/file 热更,秒级 | ✅ 已落地 |
| 4 | 熔断 / 限流(自动防护) | 自动按负载/错误率 | 自动触发 | ✅ 已落地(BBR 限流 + SRE 熔断) |
| 5 | 优雅关闭(先 deregister 再 drain) | 整服 | 配合第 2 层 | 🟡 待 registry |

**经验法则**:临时关一个 RPC → 用第 3 层(Kill-Switch,最细最快可回滚);对外整组紧急挡 → 用第 1 层(Envoy);过载/雪崩 → 第 4 层自动扛,无需人工。

---

## 1. Kill-Switch 是怎么工作的(第 3 层)

- 所有 Kratos 业务服的 gRPC server 默认 middleware 链里挂了 `pmw.KillSwitch()`(`pkg/grpcserver`)。
- 它查全局开关源:命中规则的 RPC 直接短路返回 `ErrServiceDisabled(=13)`,不进业务逻辑。
- 开关源由 `pkg/svc.BaseContext` 在启动时按配置装配(`killswitch:` 配置段)。
- **fail-open 铁律**:开关源未配置 / 建不起来 / 没命中 → 一律放行。Kill-Switch 自身故障绝不拖垮服务。

### 规则三级粒度(一套 key 通配)

| 粒度 | key 形式 | 例 |
|---|---|---|
| 单 RPC | `<package.Service>/<Method>` | `pandora.match.v1.MatchService/StartMatch` |
| 整服 | `<package.Service>/*` | `pandora.match.v1.MatchService/*` |
| 功能组 | `feature/<name>` | `feature/trade`(需服务先 `killswitch.RegisterFeature` 归组) |
| 全局维护 | `*` | 关掉该服务所有 RPC,慎用 |

value = 关停原因(回传给客户端的 message,可留空,留空用默认文案)。

### 配置(每个服务 yaml 的 `killswitch:` 段)

```yaml
killswitch:
  enabled: true
  source: "file"                 # dev:file(改文件即生效);prod:etcd(多实例一致)
  file_path: "etc/killswitch.yaml"
  # source: "etcd"
  # etcd_endpoints: ["127.0.0.1:2379"]
  # etcd_prefix: "/pandora/killswitch/"
  # etcd_dial_timeout: "5s"
  # fail_closed: false           # 默认 false=fail-open;true=源建不起来则 fatal
```

`enabled: false`(或整段不写)= 不启用,该服务全放行(middleware 仍在链上,只是 no-op)。

---

## 2. 操作:关 / 开一个 RPC

### 2.1 file 源(dev / 无 etcd)

编辑该服务的 `etc/killswitch.yaml`,保存即热生效(~200ms 去抖后):

```yaml
rules:
  # 临时关 login 的 Login(修好后删掉这一行即恢复)
  "pandora.login.v1.LoginService/Login": "登录链路热修中 #123"
  # 临时关整个 match 服务
  "pandora.match.v1.MatchService/*": "matchmaker 维护"
```

恢复:删掉对应行(或把 `rules:` 清成 `rules: {}`)保存即可。

### 2.2 etcd 源(prod / 多实例)

前置:该服务 `main.go` 里 blank import 启用 etcd 源(见 §4),配置 `source: "etcd"`。

```bash
# 关单个 RPC
etcdctl put /pandora/killswitch/pandora.match.v1.MatchService/StartMatch "fixing bug #123"
# 关整服
etcdctl put "/pandora/killswitch/pandora.match.v1.MatchService/*" "match maintenance"
# 关功能组
etcdctl put "/pandora/killswitch/feature/trade" "trade frozen"
# 全局维护
etcdctl put "/pandora/killswitch/*" "full maintenance"

# 恢复(删 key)
etcdctl del /pandora/killswitch/pandora.match.v1.MatchService/StartMatch
```

所有实例 watch 同一 prefix,秒级一致生效,无需逐台操作。

### 2.3 功能组(feature)

把一组相关 RPC 归到一个开关下,一键关一类玩法。服务在装配处注册:

```go
killswitch.RegisterFeature("match",
    "pandora.match.v1.MatchService/StartMatch",
    "pandora.match.v1.MatchService/ConfirmMatch",
)
```

之后开关 `feature/match` 即同时关停这两个 RPC。

### 2.4 关闭整个服务 / 恢复服务

这里的"关闭服务"指**临时拒绝该服务的全部 RPC**,进程仍然存活、metrics 仍然可观测、修好后可秒级恢复。
它适合热修、数据修复、下游异常隔离等场景;不是删除 pod / 停进程。

#### dev / file 源

在该服务的 `etc/killswitch.yaml` 写整服通配:

```yaml
rules:
  "pandora.login.v1.LoginService/*": "login 整服维护,预计 10 分钟"
```

保存后热生效。恢复时删掉这一行或改回:

```yaml
rules: {}
```

#### prod / etcd 源

```bash
# 关闭 login 服务全部 RPC
etcdctl put "/pandora/killswitch/pandora.login.v1.LoginService/*" "login 整服维护,预计 10 分钟"

# 恢复 login 服务
etcdctl del "/pandora/killswitch/pandora.login.v1.LoginService/*"
```

如果一个进程暴露多个 gRPC service,需要分别给每个 `<package.Service>/*` 写规则。
全局 `*` 会关闭该进程内所有 RPC,只在全服维护窗口使用。

#### 对外直接挡流(Envoy)

如果要在客户端入口直接挡住某一组外部 RPC,优先用 §3 的 Envoy `direct_response` 维护路由。
这会让请求不再打到业务进程;内部服务间 RPC 不经 Envoy,仍需用本节的 Kill-Switch 整服通配。

---

## 3. 对外整组紧急挡流(第 1 层,Envoy)

当问题严重到要在网关层直接挡住客户端(连业务服都不想让它收到请求),用 Envoy `direct_response`:

1. 编辑 `deploy/envoy/envoy.yaml`,取消顶部 `routes:` 下「维护模式」注释段,改成要挡的 prefix。
   ⚠️ 维护路由必须放在正常路由**之前**(Envoy 顺序匹配,先命中先返回)。
2. `docker restart pandora-envoy`(或热加载 xDS,本项目暂用 restart)。
3. 客户端经 `:8443` 调该 prefix 收到 503。
4. 修好后注释回去 + 再 restart 恢复。

适用:对外客户端 RPC(login/team/match/friend/chat/trade/dialogue/push)。内部服务间 RPC 不经 Envoy,用 §2 的 Kill-Switch。

---

## 4. 启用 etcd 源(opt-in)

etcd client 依赖较重,做成独立 module + driver 注册,默认不拉。需要 etcd 源的服务:

1. `main.go` 顶部 blank import:
   ```go
   import _ "github.com/luyuancpp/pandora/pkg/killswitch/etcdkv"
   ```
2. 配置 `killswitch.source: "etcd"` + `etcd_endpoints`。
3. 依赖落地(Codex):根 `go.work` 加 `use ./pkg/killswitch/etcdkv`;在该目录 `go mod tidy` 拉 `go.etcd.io/etcd/client/v3` 生成 go.sum。

没 blank import 而配了 `source: etcd` → fail-open(Warn + 全放行),不会崩。

---

## 5. 自动防护(第 4 层)

无需人工操作,自动扛过载 / 雪崩,与手动 Kill-Switch 互补。

### 5.1 限流(server 侧,BBR 自适应)

- `pkg/middleware/ratelimit.go`,底层 `go-kratos/aegis` BBR:按 CPU / inflight / RT 自适应判断过载,过载时拒绝新请求返回 `ErrRateLimited(=9)`。
- 默认 **关**(dev 不干扰联调);prod 在服务 yaml 开:
  ```yaml
  server:
    grpc:
      enable_rate_limit: true
  ```
- 无需配阈值,BBR 自适应。

### 5.2 熔断(client 侧,SRE breaker)

- `pkg/middleware/circuitbreaker.go`,底层 `go-kratos/aegis` SRE breaker:按调用成功/失败比例自动「断 → 半开 → 闭合」。
- 已**默认挂**在 `pkg/grpcclient` 所有出站调用上:下游故障时快速失败返回 `ErrUnavailable(=10)`,避免调用方被拖死。
- 按 endpoint + operation 维度各自统计,无需配置。

---

## 6. 错误码

| code | 名称 | 含义 | 可重试 |
|---|---|---|---|
| 13 | `ERR_SERVICE_DISABLED` | 被 Kill-Switch 临时关停(维护中) | 是 |
| 9 | `ERR_RATE_LIMITED` | 被限流(过载保护) | 是 |
| 10 | `ERR_UNAVAILABLE` | 熔断 / 下游不可用 | 是 |

客户端建议:收到这三个码 → 提示「服务维护中/繁忙,稍后重试」,按退避重试,不当致命错。

---

## 7. 可观测性

- Kill-Switch / 限流拒绝都在 `Metrics` middleware 内层 → `pandora_rpc_total{service,method,code="server_err"|"client_err"|...}` 能看到拒绝次数与分布。当前 metrics label 是低基数粗分类,不会直接把业务错误码 `13` / `9` 作为 label。
- access log(`Logging` middleware)记录被拒 RPC 的 trace_id,便于排查。
- 启动日志:`[killswitch] ready source=file/etcd`、`[killswitch] file reloaded ... rules=N` 确认开关源真的起来了。

---

## 8. 待办(随 etcd registry 落地)

- 第 2 层整服 deregister + 第 5 层优雅 drain:当前用 `<svc>/*` Kill-Switch 暂代「整服关停」,但实例仍在注册表里、仍接连接(只是 RPC 被拒)。真正摘流需 registry 接入后做。
- etcd 源 go.sum 固化:Codex `go mod tidy`(见 §4)。
- 第 4 层 prod 阈值/灰度:BBR/SRE 自适应已够用,如需自定义阈值再评估。
