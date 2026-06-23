# 决策复议:UE 客户端改用生成的 protobuf pb,废弃手写 wire codec

> 状态:**已拍板(2026-06-09 人确认采纳)** —— 采用方案 A 中间路线,protobuf **v35.0**,**源码随 UE(UBT)构建**。
> 提出人:Claude(Opus)/ 2026-06-09
> 关联决策:`docs/design/pandora-arch.md` §11 与 `PROGRESS.md` 中 2026-06-08 UE 客户端 gRPC-Web 骨架记录。

---

## 1. 背景:要复议哪条决策

2026-06-08 落地 UE gRPC-Web 客户端时,定了「**客户端零额外依赖**」路线:
不引 grpc-cpp / libprotobuf,改用 `FHttpModule` + 自研极简 protobuf wire codec
(`FPandoraProtoWriter` / `FPandoraProtoReader`)手工编解码消息,字段号在 C++ 里手填常量。

随 W4 客户端逐步接入 login / push / team / match,这条路线暴露两个实质问题。

## 2. 问题:手写 codec 违反 proto「唯一真相源」与兼容性保证

### 2.1 与 CLAUDE.md §5.8 直接冲突

- §5.8:**禁止手写与 proto 重复、会漂移的并行结构**。
- 手写 `FPandoraProtoWriter` 填 `team_a=7` / `match_id=1` 等字段号,本质就是把 proto 已生成好的
  序列化逻辑**重抄一遍**。proto 改字段号、调结构,UE 侧不同步就**静默错位**,无编译期保护。

### 2.2 丢掉了 protobuf 最值钱的前后向兼容

- protobuf wire format 的核心价值:**新增字段(新字段号)老端自动跳过,前后向兼容**
  (对齐 §9 不变量 5「字段编号上线后不复用」)。
- 手写 codec 对「未知字段跳过 / packed repeated / 默认值省略」等边界处理不如生成代码严谨,
  且字段号靠人维护 → **兼容性保证形同虚设**。后端 service 加字段,UE 手写 codec 不跟,
  轻则丢字段,重则错位解析。

## 3. 为什么当初选了零依赖(诚实记录,非翻案抹黑)

把 grpc-cpp 链进 UE 有真实摩擦,当时的顾虑成立:

- **grpc-cpp 极重**(数十 MB),跨平台(Android/iOS/主机)编译是出名的坑;
- libprotobuf 用到 RTTI / 异常,UE 默认 `bUseRTTI=false` / `bEnableExceptions=false`,需逐 module 开;
- M1.5 阶段先求"能联通",自研 grpc-web 绕开依赖,快速打通 login/push 链路。

**但当时把「grpc-cpp 很重」与「protobuf 也不能用」错误绑定了** —— 这两件事可以分开。

## 4. 提议:中间路线 = 只链 libprotobuf,不链 grpc-cpp

| 层 | 现状(妥协) | 改为 |
|---|---|---|
| 消息序列化 | 手写 `FPandoraProtoWriter` 填字段号 | **生成的 `*.pb.h` `SerializeToArray` / `ParseFromArray`**(libprotobuf) |
| gRPC-Web 帧（5 字节头 + trailer） | `FPandoraGrpcWeb` | **保留**（经 Envoy 是 gRPC-Web over HTTP，本就不需要 grpc-cpp） |
| HTTP 传输 | `FHttpModule` | **保留** |

**关键认知：gRPC-Web 经 Envoy 是普通 HTTP 请求，根本不需要 grpc-cpp 的传输栈。**
> ⚠️ 2026-06-23 更新：客户端连接②（`PandoraBackendSubsystem`）已显式钉死 **HTTP/2 over TLS**
> （`SetOption(HttpVersion, VERSION_2TLS)`，ALPN 协商不到 h2 时回落 HTTP/1.1）。早期本文写的
> “是 HTTP/1.1”已过时；但这不影响本节结论——**无论 h1.1 还是 h2，都不需要 grpc-cpp 传输栈**。
只有「消息编解码」需要 protobuf 运行时,而 **libprotobuf 比 grpc-cpp 轻一个量级**;
后续若确认 UE 侧不依赖 descriptor/反射/text format,再评估 `optimize_for=LITE_RUNTIME`
切到 lite runtime 减体积。

收益:

- proto 重新成为**唯一真相源**,字段号不再手填 → 消灭漂移(回归 §5.8)。
- 前后向兼容由 protobuf 运行时保证 → **后端加字段,UE 老端自动跳过不报错**(满足用户诉求)。
- cpp pb 产物链路已存在([proto/buf.gen.cpp.yaml](../../proto/buf.gen.cpp.yaml) → `proto/gen/cpp/**`),
  proto 本就设计给 UE 用(§5.2「cpp pb 推送到 UE 仓库 `Source/Pandora/Generated/Proto/`」),
  不是凭空加依赖,是**把既有生成物真正用起来**。

## 5. 落地方案与分工(对齐 AGENTS §11.1)

### 5.0 已拍板的关键选择(2026-06-09)

| 项 | 决定 | 理由 |
|---|---|---|
| protobuf 版本 | **v35.0**(latest release) | 用最新稳定版;gencode 与运行时同版本钉死 |
| 二进制来源 | **源码随 UE(UBT)构建**,不用预编译库下发 | UE Linux DS 用自带 clang+libc++,系统预编译 `.a` 链不进(ABI 撕裂);UBT 编译自动匹配工具链,跨平台「免费」跟着编。UE 5.7.4 源码版集成第三方源码本就容易 |
| 链接范围 | **只链 libprotobuf，不链 grpc-cpp** | gRPC-Web 经 Envoy 是普通 HTTP（客户端钉 HTTP/2 over TLS），传输层 `FPandoraGrpcWeb`+`FHttpModule` 足够；grpc-cpp 数十 MB + 跨平台坑，不引 |
| grpc/cpp 生成插件 | **移除**(buf.gen.cpp.yaml 已删) | `*.grpc.pb.*` include grpc++ 头,UE 侧无该依赖会编不过;只留 `protocolbuffers/cpp` 生成消息 pb |
| C++ 标准 | **C++17**(protobuf v35.0 要求) | UE 5.7 默认 ≥C++17,满足 |

**⚠️ 必须遵守的硬约束:**
- **abseil-cpp 跟着进来**:protobuf v22+ 强依赖 abseil,v35.0 亦然(release notes 多处 abseil)。所以「只链 protobuf」实际也会拖入 abseil,**这正是选源码构建的另一理由**(abseil 也不必逐平台预编译)。`ThirdParty/` 要同时纳管 protobuf v35.0 + 对应 abseil tag,**版本钉死不浮动**。
- **gencode 版本 == 运行时版本**:protobuf C++ 有硬版本检查。`buf.gen.cpp.yaml` 的 `protocolbuffers/cpp:v35.0` 生成的 `*.pb.cc` 必须与 ThirdParty 链接的 libprotobuf **同为 v35.0**,否则编译/运行报版本不匹配。

### 5.1 环境/构建(Codex / 人 —— 触碰构建环境,Claude 不做)

1. 取 **protobuf v35.0 源码** + **对应版本的 abseil-cpp 源码**,放 UE `ThirdParty/Protobuf/` 与 `ThirdParty/Abseil/`(版本钉死,对齐 5.0 表)。
2. 写 `ThirdParty/Protobuf/Protobuf.Build.cs`(+ abseil 的 Build.cs 或合并),让 **UBT 从源码编译**(非预编译 lib),按需开 `bUseRTTI` / `bEnableExceptions` / `bEnableUndefinedIdentifierWarnings=false` 等(protobuf 通常不需 RTTI,异常按编译报错定夺);先保 **Win64(客户端)+ Linux(DS)** 两平台。
3. **确认 buf 远程插件 `protocolbuffers/cpp:v35.0` 是否可用**:可用则 `cd proto && buf generate --template buf.gen.cpp.yaml`;若 tag 滞后不可用,用 v35.0 release 自带 `protoc` 直接生成(反正要从源码构建 v35.0)。生成物同步到 UE `Source/Pandora/Generated/Proto/`。
4. 在 UE 编辑器里编译验证「一个 trivial pb(如 login)能 include + link + round-trip(`SerializeToArray`/`ParseFromArray`)」,Win64 + Linux 各过一遍。
5. (可选优化)评估 cpp pb 是否切 `optimize_for = LITE_RUNTIME` 减体积;**先按完整 runtime 跑通,再决定是否切**(切了去 descriptor/反射/text format,客户端够用但要确认无代码依赖反射)。

### 5.2 代码(Claude 可做 —— 纯 C++ 逻辑,不碰构建环境)

5. 新建 `PandoraProto` UE module,纳管生成的 `*.pb.cc`(隔离 protobuf 编译设置,不污染主模块)。
6. **重构消息编解码**:把 `FPandoraProtoWriter` 手填字段号的消息构造,换成生成 pb 的
   `Msg.set_xxx()` + `SerializeToArray`;响应 `ParseFromArray`。**保留 `FPandoraGrpcWeb` 帧 +
   `FHttpModule`**(传输层不变)。
7. **顺带拆 SRP**(本次一并做):把胖 `UPandoraBackendSubsystem` 拆成传输层 helper +
   per-domain client(`UPandoraLoginClient` / `UPandoraTeamClient` / `UPandoraMatchClient` /
   `UPandoraPushClient`),subsystem 退化为薄 service locator。

### 5.3 顺序

- 文档拍板(本文)→ 5.1 环境(Codex)→ 5.2 代码(Claude)→ Codex/人 UE 编辑器编译验证 → 联调。
- 代码可与环境并行起草(client 只依赖生成 pb 的稳定 API,与 lib 链接方式无关),
  但**合并前必须等 5.1 落地 + UE 编译通过**。

## 6. 风险与回退

- **风险**:跨平台(尤其主机/移动)protobuf 工具链可能有坑 → 先只保 Win64,逐平台补。
- **回退**:若某平台 protobuf 实在链不进,可退守「**用 proto codegen 自动生成 UE 友好序列化器**」
  (仍以 proto 为真相源,但生成而非手写)——比当前手写强,作为 B 计划保留。
- 决策一旦拍板,更新 `docs/design/pandora-arch.md` §11 + 本文状态,并在 UE 仓库 README 记录 ThirdParty 约定。

## 7. 拍板记录(2026-06-09 人确认)

- [x] 同意加 **libprotobuf** 客户端依赖(推翻「零额外依赖」)—— **采纳**。
- [x] protobuf 版本 = **v35.0**(latest)。
- [x] 二进制来源 = **源码随 UE(UBT)构建**(非预编译下发)。
- [x] 链接范围 = **只 libprotobuf,不 grpc-cpp**;buf.gen.cpp.yaml **移除 grpc/cpp 插件**。
- [ ] `optimize_for = LITE_RUNTIME` 切不切:**先按完整 runtime 跑通,再评估**(5.1 第 5 步)。
- [ ] (并行)Claude 起草 5.2 代码,**合并前必须等 5.1 ThirdParty 落地 + UE 编译通过**。
