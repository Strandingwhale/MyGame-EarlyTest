# 无底深城（Abyssal City）

面向后端岗位的分布式塔防作品集项目：**Go 四进程服务端 + 团结引擎（Tuanjie 1.11.0，Unity 2022.3 分支）演示客户端**。服务端权威计算，客户端只做表现层。

- 服务端：Go + TCP + Protobuf + MySQL + Redis，拆分为 gateway / login / battle / datasave 四个进程
- 客户端：团结引擎极简演示客户端，仅用于联调与录屏
- 产出物：源码、协议文档、架构 C4 图 + 时序图、部署手册、压测报告、踩坑复盘、演示录屏、可执行 exe

## 仓库结构

```
├─ Abyssal-City-server/   服务端（Go）
│  ├─ cmd/                gateway / battle / datasave / configtool 入口
│  ├─ internal/           gateway / login / battle / datasave / config / common
│  ├─ api/                base.proto、config.proto、config.pb
│  ├─ config/             level.xlsx、operator.xlsx、config.pb
│  ├─ db/schema.sql       account / player / battle_record
│  ├─ docs/               protocol / architecture / deploy / loadtest-report / pitfalls
│  ├─ tmpverify/          端到端与压测工具
│  ├─ bin/                编译产物 {gateway,login,battle,datasave}.exe
│  └─ 启动服务端.bat / 停止服务端.bat
├─ unity/无底深城/         团结引擎客户端工程（2D URP）
│  └─ Assets/Scripts/     ClientNet.cs、GameUI.cs、BattleRenderer.cs、Proto/*.cs（protoc 生成，勿手改）
├─ Build/                 客户端打包输出（已被 .gitignore 忽略）
└─ 项目交接.md             完整交接文档（路径、协议要点、踩坑、完成度）
```

## 快速开始

**依赖**：本机 MySQL `root@127.0.0.1:3306/abyssal_city`、本机 Redis `127.0.0.1:6379`（无密码），执行 `Abyssal-City-server/db/schema.sql` 建表。

**服务端**（启动顺序固定，battle 启动时要读配置、login 要先起）：

```
datasave(9997) → login(9999) → battle(9998) → gateway(8888)
```

- 一键启动：双击 `Abyssal-City-server/启动服务端.bat`（停止用 `停止服务端.bat`）
- 编译 exe：`cd Abyssal-City-server && go build -o bin/xxx.exe ./cmd/xxx`
- 源码运行：`go run ./cmd/datasave` / `./internal/login` / `./cmd/battle` / `./cmd/gateway`
- 配置表：改 `config/*.xlsx` 后 `go run ./cmd/configtool` 重新生成 `config/config.pb`

gateway 是唯一对外入口（8888），客户端只连它。

**客户端**：用团结引擎打开 `unity/无底深城`，场景 `Assets/Scenes/SampleScene.scene` 挂载 `Network`(ClientNet)、`BattleView`(BattleRenderer)、`UI`(GameUI)，点 ▶播放。

**玩家路径**：启动 MySQL/Redis → `启动服务端.bat` → 启动客户端 → 注册 → 登录 → 选关 → 战斗。

**客户端操作**：点第 1 行空格部署干员（左半格朝左/右半格朝右）、点干员撤回、`Q` 技能、`R` 演示断线重连。

## 协议速览

帧格式：`[4B BodyLen][2B msgId][8B connID][payload]`，大端；`connID` 客户端填 0，网关转发时覆盖。

| 号段 | 用途 |
|---|---|
| 0 / 1 | Ping / Pong（网关本地处理） |
| 1001–1004 | 登录、注册 |
| 2000–2009 | 进房、部署、撤回、技能；2004 每 tick 战斗推送，2005 结算 |
| 2010 | 网关→battle **内部**断线通知（不下发客户端） |
| 3000 / 3100 段 | DataSave 内部调用（客户端不可达） |

关键约定：2000 进房 `room_id=0` 新建房间；断线重连带旧 `room_id` 且 uid 一致才恢复原局。完整字段见 `Abyssal-City-server/docs/protocol.md`。

战斗常量：地图 9×2，`pathY=1`，敌人 (0,1)→(8,1)；`offlineTimeout=30s`（断线 30 秒无重连销毁房间）。

## 已验证的链路

- 注册 → 登录（MySQL + bcrypt）→ Redis token（TTL 2h）
- 进房扣体力（DataSave 3001）→ 多波次敌人 → 部署 / 撤回返费 / 技能（扣 3 费、15 帧冷却、60 伤害）
- 每 tick 推 2004 → 结算 2005 → 异步写 `battle_record`、累加 exp/gold
- 网关令牌桶限流（容量 50 / 5 秒，长期 10 条/秒/连接，超频丢弃）
- 心跳 10 秒超时 + 断线 2010 + `resume()` 重连（帧号连续不归零）
- 客户端完整流程：登录 → 主界面 → 选关 → 战斗 → 结算

## 压测

```
go run ./tmpverify/loadtest -conns 200 -games 30 [-max-inflight 8]
```

结果：200 连接 / 30 局全胜 / 战斗 p95 ≈ 7.2–7.8ms / 错误率 0%；认证 p95 从 14s 优化到 1.7s。详见 `docs/loadtest-report.md`。

## 打包 exe

团结引擎：`文件 → 构建设置 → 平台 Windows/x86_64 → 添加已打开场景（SampleScene）→ 播放器设置取消全屏、1280×720 → 构建`，输出 `Build/无底深城.exe`。产物必须整目录分发（exe + `_Data` + `UnityPlayer.dll` + `MonoBleedingEdge`）。

## 完成度

整体约 **80%（50.5/63 条）**，核心链路 100%：

| 模块 | 状态 |
|---|---|
| 网关 Gateway | 8/8 |
| 战斗 Battle | 12/13 |
| 塔防功能 | 8/8 |
| 压测与性能 | 3/3 |
| 文档与产出物 | 7.5/8 |
| 登录 Login | 4/6 |
| DataSave | 3/6 |
| 账号角色 | 1.5/4 |
| Unity 客户端 | 3.5/5 |
| 可观测 Grafana | 0/1 |
| 单元测试 | 0/1 |

下一步优先级：① Prometheus + Grafana 埋点 → ② 战斗核心单元测试 → ③ Login 补全 → ④ DataSave Redis 缓存层 → ⑤ Docker Compose 一键启动 + Linux 实测。

## 文档

| 文档 | 内容 |
|---|---|
| `Abyssal-City-server/README.md` | 服务端目录、启动、端口、消息号、压测 |
| `Abyssal-City-server/docs/protocol.md` | 帧格式、消息号全表、错误码 |
| `Abyssal-City-server/docs/architecture.md` | C4 上下文图 + 容器图 + 5 张时序图 |
| `Abyssal-City-server/docs/deploy.md` | 部署、环境依赖、排障 |
| `Abyssal-City-server/docs/loadtest-report.md` | 三轮压测对比 + pprof 结论 |
| `Abyssal-City-server/docs/pitfalls.md` | 15 条踩坑复盘 |
| `项目交接.md` | 交接总览（关键路径、协议要点、完成度、协作约定） |
