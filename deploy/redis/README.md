# Pandora Redis 高可用 / 扩展部署

> 解决 [`scale-dau-2m.md`](../../docs/design/scale-dau-2m.md) §2.1「单 Redis 单点」。
> 两个跨 slot 服务(trade、hub_allocator)已按
> [`decision-revisit-trade-crossslot.md`](../../docs/design/decision-revisit-trade-crossslot.md)、
> [`decision-revisit-hub-crossslot.md`](../../docs/design/decision-revisit-hub-crossslot.md)
> 改造完成,Sentinel 与 Cluster 两条路都已就绪。

## 0. 谁来起实例

⚠️ 按 [`AGENTS.md`](../../AGENTS.md) §11.1:部署配置由 Claude 编写,**起容器 / 建集群 /
信任凭证由 Codex 或人执行**。下面命令供 Codex / 人执行,Claude 不代跑。

## 1. 两条路线怎么选

| 维度 | Sentinel(一主两从 + 三哨兵) | Cluster(分片) |
|---|---|---|
| 解决 | 主故障自动转移 + 读副本扩展 | 上面全部 + **写吞吐/容量水平扩展** |
| 多键事务 / Lua | ✅ 不受限(单 slot 命名空间) | ⚠️ 仅同 hash tag `{}` 同 slot 可原子 |
| 改造成本 | 0(业务无需 hash tag) | 已付清(跨 slot 改造已完成) |
| 容量上限 | 单主内存 / 单核写 | 随主分片线性扩 |
| 定位 | **第一步:立即去单点** | **目标态:30 万 CCU 写扩展** |

**推进顺序**:先 Sentinel 立刻拿到可用性(低风险、零业务改动)→ 压测确认单主写吞吐
触顶后切 Cluster(改造已完成,直接换 addrs 即可)。

## 2. 本地启动

```powershell
# 路线 A:Sentinel(一主两从 + 三哨兵)
$env:REDIS_PASSWORD = "pandora_dev_pwd"   # 自定义,生产用强随机
docker compose -f deploy/docker-compose.redis-sentinel.yml up -d
# 验证:任一哨兵
docker exec -it pandora-sentinel-1 redis-cli -p 26379 sentinel master pandora-master

# 路线 B:Cluster(本地 3 主 3 从最小集群,验 CROSSSLOT)
docker compose -f deploy/docker-compose.redis-cluster.yml up -d
# cluster-init 自动建簇,验证:
docker exec -it pandora-rc-node-1 redis-cli -c -p 6379 cluster info     # cluster_state:ok
docker exec -it pandora-rc-node-1 redis-cli -c -p 6379 cluster shards
```

## 3. 业务侧配置(`node.redis_client`,见 pkg/config RedisConf)

底层 `pkg/redisx.NewUniversalClient` 按字段自动选驱动,业务代码零改动:

```yaml
# 路线 A:Sentinel —— 填 master_name + 哨兵 addrs(不是数据节点)
node:
  redis_client:
    master_name: "pandora-master"
    addrs:
      - "sentinel-1:26379"
      - "sentinel-2:26379"
      - "sentinel-3:26379"
    password: "__REDIS_PASSWORD__"
    db: 0

# 路线 B:Cluster —— 填全部数据节点 addrs,**留空 master_name**
node:
  redis_client:
    addrs:
      - "redis-node-1:6379"
      - "redis-node-2:6379"
      - "redis-node-3:6379"
      - "redis-node-4:6379"
      - "redis-node-5:6379"
      - "redis-node-6:6379"
    password: "__REDIS_PASSWORD__"
    # db 在 Cluster 模式下只能是 0
```

判定逻辑:`master_name` 非空 → Sentinel 故障转移客户端;否则多 `addrs` → Cluster 客户端;
单 addr → 单实例。Cluster 模式下 `db` 必须为 0。

## 4. 生产分片数决策(6 主 6 从)

详见 [`scale-dau-2m.md`](../../docs/design/scale-dau-2m.md) §2.1。摘要:

- **6 主 6 从**:6 主分摊写,每主 1 从做故障转移 + 读分流。16384 slot / 6 ≈ 2731 slot/主。
- **容量测算(30 万 CCU)**:在线态键(session / player_locator / hub assign / 队列 / 交易索引)
  估 ~30 GB,留 50% 余量 → 单主 ≤ 8 GB(maxmemory),6 主合计 48 GB 物理可用。
- **写 QPS**:峰值估 ~50 万 ops/s,单主 ~8 万安全水位,6 主有冗余;触顶时 reshard 加主。
- **物理隔离**:6 主 6 从分布到 ≥3 台物理机/可用区,主从不同机,避免单机故障带走一个分片对。
- 生产用 k8s redis-operator / helm(由它管 announce-ip、reshard、故障转移),不用本 compose。

## 5. 文件清单

| 文件 | 作用 |
|---|---|
| [`../docker-compose.redis-sentinel.yml`](../docker-compose.redis-sentinel.yml) | Sentinel 一主两从 + 三哨兵 |
| [`sentinel-entrypoint.sh`](./sentinel-entrypoint.sh) | Sentinel 启动脚本(生成可写 conf) |
| [`../docker-compose.redis-cluster.yml`](../docker-compose.redis-cluster.yml) | 本地 3 主 3 从最小集群 |

## 6. 交接给 Codex / 人

1. 起实例(§2)并确认 `cluster_state:ok` / Sentinel `master` 可见。
2. 把所选路线的配置片段(§3)填进各服务 `*-prod.yaml`,密码用强随机(release_preflight 校验)。
3. 生产 Cluster 用 k8s operator 起 6 主 6 从(§4)。
4. PROGRESS.md 追加 + commit(Claude 不代跑 git)。
