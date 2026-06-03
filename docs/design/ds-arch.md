# Pandora UE DS 架构设计

> Hub DS / Battle DS 的运行时设计、Iris + GAS 配置、500 人 PvP 关键路径、跨 DS 切换流程。

## 1. DS 双形态对比

| 维度 | Hub DS(大厅) | Battle DS(战斗) |
|---|---|---|
| Map | `HubMap`(单城镇 ~1km²) | `BattleMap`(MOBA 三路一河) |
| 玩家容量 | **500 人/实例**(目标) | 固定 10 人(5v5) |
| 生命周期 | 常驻 + 滚动热更 | 一局一进程,~25min,结束销毁 |
| Tick rate | 20~30 Hz | 30~60 Hz |
| GameMode | `APandoraHubGameMode` | `APandoraBattleGameMode` |
| GameState | `APandoraHubGameState` | `APandoraBattleGameState` |
| Replication | **Iris + AOI 网格**(强制) | Iris(默认即可) |
| GAS | 启用,大厅可放技能可互打 | 启用 |
| 死亡处理 | 复活点重生 | 等待复活计时 |
| 持久化 | 实时(玩家位置、状态) | 全内存,结算时一次 kafka 落库 |
| 接 go 服务 | 频繁(NPC / 商店 / 交易 / 组队 / 匹配) | 少量(开始 / 结束 / 异常) |
| Agones Fleet | `pandora-hub-fleet`(常驻) | `pandora-battle-fleet`(allocate on demand) |

## 2. UE 工程模块划分

```
F:/work/Pandora-Client/Source/
├── Pandora/                  # 客户端
│   ├── PandoraGameInstance   # 登录流程、跨 DS 切换
│   ├── PandoraPlayerController
│   ├── PandoraHUD
│   └── UI/                   # UMG 蓝图
│
├── PandoraShared/            # 客户端 + DS 共用
│   ├── Auth/
│   │   ├── TicketVerifier    # JWT 票据校验
│   │   └── DSCredentials
│   ├── Network/
│   │   └── GrpcClient        # 用 grpc-cpp 包一层
│   ├── GAS/
│   │   ├── PandoraAttributeSet
│   │   ├── PandoraAbilitySystemComp
│   │   ├── PandoraGameplayAbility
│   │   ├── PandoraGameplayEffect
│   │   └── PandoraGameplayCue
│   ├── Character/
│   │   ├── PandoraCharacterBase
│   │   └── HeroData
│   └── Proto/
│       └── Generated/        # 从 Pandora/proto/ 生成
│
├── PandoraHubServer/         # 大厅 DS 专属
│   ├── HubGameMode
│   ├── HubGameState
│   ├── HubPlayerController
│   ├── HubCharacter
│   ├── AOI/
│   │   └── HubAOIGrid        # 自研 AOI 网格(500 人必须)
│   ├── Replication/
│   │   └── HubReplicationGraph (退路方案)
│   ├── Service/
│   │   ├── NPCService
│   │   ├── ShopService
│   │   ├── TransferService
│   │   └── EnterBattleService
│   └── Agones/
│       └── HubAgonesIntegration
│
└── PandoraBattleServer/      # 战斗 DS 专属
    ├── BattleGameMode
    ├── BattleGameState
    ├── BattlePlayerController
    ├── BattleCharacter
    ├── Match/
    │   ├── MatchPhaseController
    │   └── MMRReporter
    └── Agones/
        └── BattleAgonesIntegration
```

## 3. Iris vs Replication Graph 决策

### 3.1 默认走 Iris

UE 5.7 时代 Iris 应该已经 production-ready,**默认开 Iris**:

```ini
; Config/DefaultEngine.ini
[/Script/IrisCore.ReplicationSystem]
bEnableIris=True

[SystemSettings]
net.Iris.UseIrisReplication=1
```

**Iris 优势**(对 500 人 PvP 是关键):
- 数据驱动,不用 PreReplication 钩子
- 内置 prioritization
- 支持 partial state
- 内置 NetCullDistance + 自定义 filter

### 3.2 GAS 在 Iris 下的注意

GAS 早期是为 RepGraph 写的,Iris 适配在 5.5+ 才完整。已知坑:
- `FActiveGameplayEffectsContainer` 用 Fast Array Serializer
- `FGameplayAbilitySpec` 同上
- Prediction Key 跟 Iris 的 frame 模型对接

**风险缓解**:
- W5(GAS 集成阶段)留 1 周 buffer
- 退路:回退 Replication Graph(已有大量 GAS + RepGraph 案例)

### 3.3 Replication Graph 退路方案

预留 `Source/PandoraHubServer/Replication/HubReplicationGraph.h`,如果 Iris 不行就启用:
- 4 个 Connection Graph Node:
  1. `UReplicationGraphNode_GridSpatialization2D`
  2. `UReplicationGraphNode_DormancyNode`
  3. `UReplicationGraphNode_AlwaysRelevant`
  4. 自定义 `UPandoraHubVisibilityNode`

## 4. 500 人 Hub PvP 关键路径

### 4.1 网络预算

**单玩家上行**:目标 ≤ 20 KB/s
**单玩家下行**:目标 ≤ 100 KB/s
**总入站**:500 × 20 = 10 MB/s ≈ 80 Mbps
**总出站**:500 × 100 = 50 MB/s ≈ 400 Mbps

⚠️ **千兆网卡上行接近极限**,生产要走万兆 + 多机分片。

### 4.2 AOI 网格设计

**网格尺寸**:50m × 50m
**每格典型容纳**:5~30 人
**关注半径**:周围 9 格 = ~50~270 人

**复制规则**:
- 角色完整状态:仅 9 格内复制
- 心跳信号:18 格半径
- 全局事件(聊天 / 系统):全图(单独 channel)

### 4.3 技能命中判定

**禁止用 `OverlapMultiByObjectType`**(500 人时遍历开销爆炸)。

**自研空间索引**:`FHubSpatialIndex`,每 tick 维护一个 50m 网格的 `TMap`,O(1) 查格 + O(K) 遍历(K ≤ 30)。

```cpp
class FHubSpatialIndex {
public:
    void AddActor(APandoraCharacter* Actor);
    void RemoveActor(APandoraCharacter* Actor);
    void UpdateActor(APandoraCharacter* Actor);
    TArray<APandoraCharacter*> QueryRadius(FVector Center, float Radius);
private:
    TMap<FIntVector, TArray<TWeakObjectPtr<APandoraCharacter>>> Grid;
    static constexpr float CellSize = 5000.f;  // 50m
};
```

### 4.4 技能限流

**预算**:每 tick 最多处理 50 个技能激活,超出排队下一 tick(优先级队列)。

```cpp
TPriorityQueue<FAbilityActivationRequest> PendingAbilities;
constexpr int32 MaxAbilitiesPerTick = 50;
```

优先级:
1. 玩家正在被攻击 → 防御类优先
2. 队友受击 → 治疗优先
3. 普通主动技能
4. 自我增益

### 4.5 移动同步降频

- 0~30m:30Hz 完整同步
- 30~50m:15Hz
- 50~100m:5Hz(只位置)
- > 100m:每秒 1 次心跳

## 5. GAS 框架设计

### 5.1 Attribute 清单(初版)

`UPandoraAttributeSet`:

```cpp
ATTRIBUTE_ACCESSORS(MaxHealth)
ATTRIBUTE_ACCESSORS(Health)
ATTRIBUTE_ACCESSORS(MaxMana)
ATTRIBUTE_ACCESSORS(Mana)
ATTRIBUTE_ACCESSORS(AttackDamage)
ATTRIBUTE_ACCESSORS(AbilityPower)
ATTRIBUTE_ACCESSORS(Armor)
ATTRIBUTE_ACCESSORS(MagicResist)
ATTRIBUTE_ACCESSORS(MoveSpeed)
ATTRIBUTE_ACCESSORS(AttackSpeed)
ATTRIBUTE_ACCESSORS(CritChance)
ATTRIBUTE_ACCESSORS(CritDamage)
ATTRIBUTE_ACCESSORS(CooldownReduction)
ATTRIBUTE_ACCESSORS(LifeSteal)
ATTRIBUTE_ACCESSORS(Tenacity)

// Meta(只在服务端临时计算)
ATTRIBUTE_ACCESSORS(IncomingDamage)
ATTRIBUTE_ACCESSORS(IncomingHealing)

// 经济(战斗 DS only)
ATTRIBUTE_ACCESSORS(Gold)
ATTRIBUTE_ACCESSORS(Experience)
```

### 5.2 Ability 类型

`UPandoraGameplayAbility` 子类:

| 类 | 说明 | 例子 |
|---|---|---|
| `UPandoraAbility_Passive` | 被动 | 嗜血、暴击 |
| `UPandoraAbility_Targeted` | 单体瞄准 | 普攻、冲刺 |
| `UPandoraAbility_Skillshot` | 技能弹道 | 直线技能、AOE |
| `UPandoraAbility_AoE` | 范围 | 大招 |
| `UPandoraAbility_Channel` | 引导 | 持续治疗 |
| `UPandoraAbility_Movement` | 位移 | 闪现、突进 |

### 5.3 GameplayCue(表现层)

服务端**只算逻辑**,客户端**只播表现**。Cue 走 Multicast(AOI 过滤后)。

```
GameplayCue.Skill.Fireball.Hit       → 火球命中音效+特效
GameplayCue.Skill.Heal.Apply         → 治疗光环
GameplayCue.Character.Death          → 死亡表现
GameplayCue.UI.DamageNumber          → 飘字
```

### 5.4 Hero DataAsset

```cpp
class UPandoraHeroData : public UPrimaryDataAsset {
    int32 HeroId;
    FText DisplayName;
    UCurveTable* AttributeGrowth;
    TArray<TSubclassOf<UGameplayAbility>> StartingAbilities;
    TArray<TSubclassOf<UGameplayEffect>> StartingPassives;
    USkeletalMesh* Mesh;
    UAnimBlueprint* AnimBP;
};
```

## 6. 跨 DS 切换流程

### 6.1 Hub → Battle

```
Client (在 Hub DS)
   │ 1. 点开始匹配
   ▼
Hub DS → matchmaker (gRPC StartMatch)
   ▼
匹配成功 → matchmaker 通知 Hub DS
   ▼
Hub DS → Client (RPC: BattleReady{addr, ticket})
   ▼
Client:
   - SaveGame
   - UPandoraGameInstance::TravelToBattle(addr, ticket)
   - DisconnectFromHub → ConnectToBattle
```

**关键**:Hub DS 保留玩家"占位" 30s,Battle DS 没收到玩家连入则告警。

### 6.2 Battle → Hub

```
Battle DS 战斗结束
   ▼
Battle DS:
   - kafka 发 pandora.battle.result
   - 给客户端 RPC: BattleEnded{result, hub_ds_addr, hub_ticket}
   - 留 10s 看战绩
   - 主动断开
   - 通知 ds_allocator 释放
```

### 6.3 Hub 跨分片切换

```
Client 走到传送点 → Hub DS A
   ▼
Hub A → hub_allocator: TransferHub
   ▼
返回 hub_b_addr + ticket
   ▼
Client:TravelToHub(2 秒内完成)
```

**优化点**:客户端预加载 Hub B Map(后台异步),ticket 提前签发。

## 7. Agones 集成

### 7.1 SDK 接入点

每个 DS 进程必须:
- **启动后**:`SDK::Ready()`
- **每 5s**:`SDK::Health()`(超时 15s 视为崩溃)
- **退出前**:`SDK::Shutdown()`
- **玩家进出**:`SDK::Alpha::PlayerConnect/Disconnect()`(可选)

### 7.2 Agones Fleet 配置(开发期)

```yaml
# deploy/k8s/hub-fleet.yaml
apiVersion: "agones.dev/v1"
kind: Fleet
metadata:
  name: pandora-hub-fleet
  namespace: pandora
spec:
  replicas: 2
  scheduling: Packed
  template:
    spec:
      ports:
      - name: default
        portPolicy: Dynamic
        containerPort: 7777
      health:
        initialDelaySeconds: 30
        periodSeconds: 5
        failureThreshold: 3
      template:
        spec:
          containers:
          - name: pandora-hub
            image: pandora-hub-ds:latest
            resources:
              requests: { cpu: "2", memory: "4Gi" }
              limits:   { cpu: "4", memory: "8Gi" }
```

```yaml
# deploy/k8s/battle-fleet.yaml
apiVersion: "agones.dev/v1"
kind: Fleet
metadata:
  name: pandora-battle-fleet
spec:
  replicas: 5      # 5 个空闲,allocate on demand
```

## 8. DS 监控指标

每个 DS 暴露 `:9100/metrics`(Prometheus):

```
pandora_ds_player_count{ds_type="hub",pod="..."}
pandora_ds_tick_duration_ms_bucket{ds_type,pod,le="..."}
pandora_ds_replication_packet_size_bytes_bucket{ds_type,pod}
pandora_ds_aoi_grid_max_pop{pod}
pandora_ds_ability_activations_total{ds_type,pod}
pandora_ds_kafka_send_total{topic}
pandora_ds_grpc_call_duration_ms_bucket{service,method}
```

## 9. 容错与崩溃恢复

### 9.1 DS 崩溃

- Agones 检测心跳超时 15s → 标记 Unhealthy → kubectl delete pod → Fleet 自动补充
- ds_allocator 收到 `pandora.ds.lifecycle{event=crashed}` → 触发补偿:
  - 战斗 DS:发"未结算战斗"事件 → battle_result 按规则补 / 不算败场
  - 大厅 DS:player_locator 清玩家 → 客户端收到提示

### 9.2 客户端崩溃

- DS 检测 NetDriver 超时 60s → 销毁玩家 actor + 通知 player_locator
- 客户端重启登录 → login → 颁发新票据 → 重连 Hub

### 9.3 网络抖动

- UE 内置 RTT 探测 + 客户端预测/回滚
- 服务端权威,客户端预测错了就回滚
- GAS 的 Prediction Key 在 Iris 下要做适配

## 10. W1 D5-D6 写代码范围

只写**骨架**,不实现业务:

### `PandoraShared`
- [ ] `TicketVerifier`(JWT 占位:固定密钥)
- [ ] `GrpcClient` 包装类
- [ ] `PandoraAttributeSet`(声明属性,初始值)
- [ ] `PandoraAbilitySystemComponent` 空类继承
- [ ] `PandoraCharacterBase` 空类(挂 ASC + Movement)
- [ ] Build.cs

### `PandoraHubServer`
- [ ] `AHubGameMode`(BeginPlay 调 Agones SDK Ready)
- [ ] `AHubGameState`
- [ ] `AHubPlayerController`
- [ ] `AHubCharacter` 继承 `PandoraCharacterBase`
- [ ] `HubAgonesIntegration`(启动 Agones SDK 协程)
- [ ] Build.cs

### `PandoraBattleServer`
- [ ] 同上结构,GameMode 不同

### Config
- [ ] `DefaultEngine.ini`:开 Iris、设 NetCullDistance、设 tick rate
- [ ] `DefaultGame.ini`:GameMode 默认绑 HubGameMode

### 验收标准
1. UE 编辑器编译通过
2. Linux Server target 交叉编译通过
3. Package 出 Linux 二进制 ~200MB
4. 本地起一个 hub DS,UE PIE 客户端连进去,GameMode 打日志 "player joined"
