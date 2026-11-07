# Abyssal-City 服务端架构说明

分布式塔防游戏服务端：客户端经**网关**单点接入，后端按职责拆成 login / battle / datasave 三个服务，MySQL 持久化、Redis 存会话 token。协议细节见 [protocol.md](protocol.md)，部署见 [deploy.md](deploy.md)，性能数据见 [loadtest-report.md](loadtest-report.md)。

## 1. C4 · 系统上下文图

```mermaid
graph LR
    Player(["玩家<br/>(游戏客户端)"])
    subgraph Abyssal["Abyssal-City 游戏服务端"]
        SVC["4 个 Go 微服务<br/>gateway / login / battle / datasave"]
    end
    MySQL[("MySQL 8<br/>账号/角色/战斗记录")]
    Redis[("Redis 7<br/>登录 token（TTL 2h）")]

    Player -->|"TCP :8888<br/>自定义二进制协议"| SVC
    SVC -->|"读写 :3306"| MySQL
    SVC -->|"读写 :6379"| Redis
```

## 2. C4 · 容器图

```mermaid
graph TB
    Client["游戏客户端<br/>(TCP 长连接)"]
    subgraph Server["Abyssal-City 服务端（可同机/分布式部署）"]
        GW["gateway :8888<br/>━━━━━━━━<br/>客户端唯一入口；会话管理<br/>msgId 路由；心跳超时(10s)<br/>战斗消息令牌桶限流(10条/s)<br/>按 connID 定向回包"]
        Login["login :9999<br/>━━━━━━━━<br/>注册/登录<br/>bcrypt 校验（-max-inflight 限并发）<br/>签发 token 写 Redis"]
        Battle["battle :9998<br/>━━━━━━━━<br/>房间管理（每房间独立 goroutine）<br/>5 帧/秒 tick；部署/撤退/技能<br/>状态推送与结算；断线快照恢复"]
        DS["datasave :9997<br/>━━━━━━━━<br/>数据落库内部服务（3000+ JSON 协议）<br/>扣体力/查角色/写战斗记录"]
    end
    MySQL[("MySQL :3306")]
    Redis[("Redis :6379")]
    Cfg["config/config.pb<br/>(本地配置表，configtool 生成)"]

    Client -->|"msgId 0-2999<br/>[4B len][2B msgId][8B connID][protobuf]"| GW
    GW -->|"msgId 1000-1999"| Login
    GW -->|"msgId 2000+"| Battle
    Login -->|"SQL"| MySQL
    Login -->|"SET token:* (TTL 2h)"| Redis
    Login -->|"3002 GetRole"| DS
    Battle -->|"3001 扣体力 / 3000 写记录"| DS
    DS -->|"SQL"| MySQL
    Battle -.->|"启动时加载"| Cfg
```

## 3. 服务职责表

| 服务 | 端口 | 职责 | 关键设计 |
|---|---|---|---|
| gateway | 8888 | 客户端接入、会话（Session）、路由、限流、心跳超时、connID 定向回包 | 对 login/battle 各一条后端连接，自动重连；所有客户端流量复用 |
| login | 9999 | 注册（bcrypt 哈希入库）、登录校验、签发 token（Redis, 2h）、返回角色面板 | 请求级并发 + `-max-inflight`（默认 8）削峰，防 bcrypt 抢占全机 CPU |
| battle | 9998 | 房间创建/加入、战斗模拟（tick 200ms）、干员部署/撤退/技能、状态推送、结算、断线恢复 | 房间 map + 每房独立 goroutine，全局 RWMutex 仅护 map；断线 30s 内可凭快照续玩 |
| datasave | 9997 | MySQL 唯一出口：扣体力、查角色、写战斗记录并累加 exp/gold | 内部 JSON 协议（3000+）；调用方同步短连接调用 |

## 4. 关键时序图

### ① 注册 / 登录 + token

```mermaid
sequenceDiagram
    participant C as 客户端
    participant G as gateway:8888
    participant L as login:9999
    participant R as Redis
    participant D as datasave:9997
    participant M as MySQL

    C->>G: 1003 RegisterReq(account, password)
    G->>L: 转发(connID)
    L->>M: INSERT account(bcrypt 哈希)
    M-->>L: ok（重复→code=2）
    L-->>C: 1004 RegisterResp(code)

    C->>G: 1001 LoginReq(account, password)
    G->>L: 转发(connID)
    L->>M: SELECT password_hash
    L->>L: bcrypt.CompareHashAndPassword（~70ms，CPU 密集）
    L->>R: SET token:<32位随机> = uid, EX 2h
    L->>D: 3002 GetRoleReq(uid)
    D->>M: SELECT player
    D-->>L: 3102 GetRoleResp(level/stamina/exp/gold)
    L-->>C: 1002 LoginResp(uid, token, session_id, 角色面板)
    G->>G: Session 绑定 uid（SetLogin）
```

### ② 进房（扣体力）

```mermaid
sequenceDiagram
    participant C as 客户端
    participant G as gateway
    participant B as battle:9998
    participant D as datasave:9997
    participant M as MySQL

    C->>G: 2000 EnterRoomReq(room_id=0, level_id, uid)
    G->>B: 转发（限流：令牌桶 10条/s）
    B->>D: 3001 DeductStaminaReq(uid, 1)
    D->>M: UPDATE player SET stamina=stamina-1（ stamina>0 ）
    alt 体力不足
        D-->>B: 3101 code=4
        B-->>C: 2001 code=4（体力不足）
    else 成功
        D-->>B: 3101 code=0, remaining
        B->>B: 创建房间（seed=UnixNano）<br/>记录 connID，启动 tick goroutine
        B-->>C: 2001 EnterRoomResp(room_id, seed)
        G->>G: Session 记录 roomID（断线通知用）
    end
```

### ③ 战斗 tick + 状态推送

```mermaid
sequenceDiagram
    participant C as 客户端
    participant G as gateway
    participant B as battle（房间 goroutine）

    loop 每 200ms 一帧（5 帧/秒）
        B->>B: frame++；敌人沿路径移动<br/>干员索敌攻击；基地掉血判定
        B->>B: 周期性追加 BattleSnapshot（供断线恢复）
        B->>G: 2004 BattleStatePush(connID, frame, cost, base_hp, operators[], enemies[])
        G->>C: 按 connID 定向写回
    end

    Note over C,B: 战斗指令异步插入：部署/撤退/技能
    C->>G: 2002 DeployOperatorReq / 2006 Retreat / 2008 CastSkill
    G->>B: 转发
    B-->>C: 2003/2007/2009（code, remaining_cost[, damage, hit_count]）
```

### ④ 结算落库

```mermaid
sequenceDiagram
    participant C as 客户端
    participant G as gateway
    participant B as battle
    participant D as datasave:9997
    participant M as MySQL

    alt 全部波次歼灭 → 胜 / 基地血量归零 → 败
        B->>G: 2005 BattleSettlePush(connID, result, score, duration_sec, rewards[])
        G->>C: 定向推送
        B->>D: 3000 SaveBattleRecordReq(uid, result, score, duration, rewards)
        D->>M: INSERT battle_record
        D->>M: UPDATE player SET exp=exp+?, gold=gold+?
        D-->>B: 3100 code=0, exp_gain, gold_gain
        B->>B: 关闭房间 goroutine，释放房间
    end
```

### ⑤ 断线重连

```mermaid
sequenceDiagram
    participant C as 客户端
    participant G as gateway
    participant B as battle（房间）
    participant L as login

    Note over C: 网络中断，停止发心跳
    G->>G: 10s 无消息 → 心跳超时，关闭 Session
    G->>B: 通知离线（Session.roomID）
    B->>B: 标记 connID 离线，启动 30s 倒计时<br/>（房间 tick 继续，快照持续累积）

    C->>G: 新建 TCP 连接（新 connID）
    C->>G: 1001 重新登录
    G->>L: 校验 → 签发新 token
    C->>G: 2000 EnterRoomReq(room_id=旧房间号)
    G->>B: 转发
    alt 30s 内重连
        B->>B: 换绑新 connID，取消离线倒计时<br/>从最近快照帧恢复
        B-->>C: 2001 code=0（room_id, seed）
        B->>C: 2004 自恢复快照继续推送
    else 超时/房间已结束
        B-->>C: 2001 code=2（房间不存在或已结束）
    end
```

## 5. 目录结构

```
Abyssal-City-server/
├── api/                 # protobuf 定义与生成的 Go 代码
│   ├── base.proto       #   客户端协议（心跳/登录/战斗 0-2999）
│   └── config.proto     #   配置表结构（ConfigSet）
├── cmd/                 # 进程入口
│   ├── gateway/         #   网关
│   ├── battle/          #   战斗服务
│   ├── datasave/        #   数据落库服务
│   └── configtool/      #   配置表工具：xlsx → config.pb
├── config/              # operator.xlsx / level.xlsx / config.pb
├── db/schema.sql        # 建库建表（account / player / battle_record）
├── internal/
│   ├── common/          #   包编解码(packet)、日志(logger)、错误码(预留)
│   ├── config/          #   config.pb 加载查询
│   ├── gateway/         #   server.go(监听/路由/回包) + session.go(会话/限流)
│   ├── login/           #   account.go(注册/登录/token，含 main)
│   ├── battle/          #   server.go(协议分发) + room.go(战斗模拟) + level.go(关卡装配)
│   └── datasave/        #   server.go(3000+ 协议) + db.go + redis.go
├── tmpverify/           # 临时验证工具
│   ├── client/          #   登录链路验证
│   ├── battle/          #   战斗链路验证
│   └── loadtest/        #   压测客户端（见 docs/loadtest-report.md）
├── docs/                # 本文档 / protocol.md / deploy.md / loadtest-report.md
└── docker-compose.yml   # MySQL + Redis（daocloud 镜像源）
```

## 6. 数据流总结

- **读路径**：登录面板（login→datasave 3002→MySQL）。
- **写路径**：进房扣体力、结算写记录（battle→datasave 3001/3000→MySQL）；token（login→Redis）。
- **实时路径**：客户端⇄网关⇄battle 房间 goroutine，200ms 一帧推送；战斗状态全在内存（房间对象 + 周期快照），不落库，只有结算结果落库。
- **解耦点**：MySQL 只有 datasave 一个出口；Redis 只有 login 一个出口；网关对客户端是唯一入口，后端服务均不直接暴露。
