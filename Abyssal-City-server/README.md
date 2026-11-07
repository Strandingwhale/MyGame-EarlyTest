# 无底深城 · 分布式塔防服务端

Go + TCP + Protobuf + MySQL + Redis 的分布式塔防服务端。四个进程分工：网关、登录、战斗、数据落库；客户端只发请求、收状态，所有战斗数值由服务端权威计算。

## 目录结构

```
Abyssal-City-server/
├─ api/            base.proto（对外协议）、config.proto（配置表结构）、config.pb
├─ cmd/
│  ├─ gateway/     网关入口（8888）：路由、心跳、令牌桶限流、断线通知
│  ├─ battle/      战斗入口（9998）：房间/帧循环/部署/撤回/技能/结算/重连
│  ├─ datasave/    数据服务入口（9997）：写记录、扣体力、查角色
│  └─ configtool/  Excel → config.pb 的配置表工具
├─ internal/
│  ├─ gateway/     session、限流、路由、断线通知
│  ├─ login/       登录服务（9999）：注册、bcrypt、Redis token
│  ├─ battle/      房间、关卡、敌人路径、帧循环
│  ├─ datasave/    MySQL 事务、角色数据
│  ├─ config/      配置表加载与查询
│  └─ common/      帧编解码
├─ config/         level.xlsx / operator.xlsx / config.pb
├─ db/schema.sql   account / player / battle_record
├─ docs/           protocol / architecture / deploy / loadtest-report / pitfalls
├─ tmpverify/      端到端与压测临时工具
└─ docker-compose.yml
```

## 快速启动

1. **依赖**：MySQL（3306）与 Redis（6379）。可用 `docker-compose.yml`，也可用本机服务。
2. **建表**：执行 `db/schema.sql`（account / player / battle_record）。MySQL 连接串见 `internal/login`；本机密码按自己的环境改。
3. **配置表**：`config/` 下已有 `config.pb`；改了 Excel 就重新生成：
   ```
   go run ./cmd/configtool
   ```
4. **按顺序启动四个服务**（顺序不能乱，battle 启动时要读配置、login 要先起）：
   ```
   go run ./cmd/datasave      # 9997
   go run ./internal/login    # 9999
   go run ./cmd/battle        # 9998
   go run ./cmd/gateway       # 8888
   ```
5. **验证**：`go run ./tmpverify/client`（登录链路）；`go run ./tmpverify/battle -mode=basic`（一局战斗）。

## 端口

| 服务 | 端口 | 说明 |
|---|---|---|
| gateway | 8888 | 唯一对外入口，客户端连这里 |
| login | 9999 | 网关转发，外部不可直连 |
| battle | 9998 | 网关转发 |
| datasave | 9997 | 仅 battle/login 内部调用 |

## 消息号速查

| 号段 | 用途 |
|---|---|
| 0 / 1 | Ping / Pong（网关本地处理） |
| 1001–1004 | 登录、注册 |
| 2000–2010 | 战斗（2010 是网关→battle 内部断线通知） |
| 3000 / 3100 ... | DataSave 内部（客户端不可达） |

完整字段见 `docs/protocol.md`。

关键点：**2000 进房时 `room_id=0` 是新建房间；断线重连要带旧 `room_id` 且 uid 一致**，否则会被当成新开一局。

## 压测

```
go run ./tmpverify/loadtest -conns 200 -games 30
```
三轮对比（串行 / 并发 64 / 加 `-max-inflight 8`）写在 `docs/loadtest-report.md`，结论：认证 p95 从 14s 降到 1.7s，战斗 p95 稳定在 7.8ms。

## 演示客户端

`D:\无底深城\unity\无底深城`（团结引擎 1.11.0，2D URP）。

| 文件 | 作用 |
|---|---|
| `Assets/Scripts/ClientNet.cs` | 连接、注册、登录、进房、部署/撤回/技能、心跳、模拟断线重连（R 键） |
| `Assets/Scripts/BattleRenderer.cs` | 9×2 战场渲染 + HUD（OnGUI）+ 鼠标点格子交互 |
| `Assets/Scripts/Proto/*.cs` | protoc 生成的协议类（勿手改） |
| `Assets/Plugins/*.dll` | Google.Protobuf 及其 4 个依赖 dll |

操作：点第 1 行空格部署、点干员撤回、`Q` 放技能、`R` 演示断线重连（1.5 秒后恢复，帧号连续）。

## 文档

| 文档 | 内容 |
|---|---|
| `docs/protocol.md` | 帧格式、消息号全表、错误码 |
| `docs/architecture.md` | 分层架构、数据流、并发模型 |
| `docs/deploy.md` | 部署、启动、排障 |
| `docs/loadtest-report.md` | 压测三轮对比与 pprof 结论 |
| `docs/pitfalls.md` | 实际踩到的坑与解决（含 Unity/protobuf 依赖链） |

## 验证过的链路

- 注册 → 登录 → MySQL bcrypt 校验 → Redis token（TTL 2h）
- 进房扣体力（DataSave 3001）→ 多波次敌人 → 部署 / 撤回返费 / 技能（扣 3 费、15 帧冷却、60 伤害）
- 每 tick 推送 2004 → 结算 2005 → 异步写 battle_record 并累加 exp/gold
- 网关限流（容量 50 / 5 秒，长期 10 条/秒）超频丢弃
- 断线 → 网关发 2010 → battle 等 30 秒 → 同 uid 带旧 room_id 重连 → 从原帧继续
