# Abyssal-City 通信协议文档

来源：[api/base.proto](../api/base.proto)、[api/config.proto](../api/config.proto) 及各服务实现。

## 1. 传输与包格式

- 传输：**TCP 长连接**，protobuf（proto3）二进制负载
- 包格式（[internal/common/packet.go](../internal/common/packet.go)，大端序）：

```
[4B BodyLen][2B msgId][8B connID][payload]
```

| 字段 | 长度 | 说明 |
|---|---|---|
| BodyLen | 4 字节 | payload 长度（不含头） |
| msgId | 2 字节 | 消息号，路由依据 |
| connID | 8 字节 | 网关分配的连接标识，用于**定向回包**（见 §6） |
| payload | BodyLen | protobuf 序列化字节 |

解码器支持半包/粘包：缓冲区不足一个完整包时不消费，等下次读。

> 例外：DataSave（3000+）是**服务间内部协议**，payload 用 **JSON** 而非 protobuf（见 [internal/datasave/server.go](../internal/datasave/server.go)）。

## 2. 消息号分段约定

| 段 | 用途 | 网关路由 |
|---|---|---|
| 0–999 | 心跳等基础消息 | 网关本地处理 |
| 1000–1999 | 登录/注册 | 转发 login（127.0.0.1:9999） |
| 2000–2999 | 战斗 | 转发 battle（127.0.0.1:9998），**有令牌桶限流** |
| 3000–3999 | DataSave 内部（客户端不可达） | 不经网关 |

## 3. 消息表

### 3.1 心跳（网关本地处理，不转发）

| msgId | 方向 | 消息 | 用途 | 关键字段 |
|---:|---|---|---|---|
| 0 | C→S | PingReq | 心跳请求 | `timestamp`（回显用） |
| 1 | S→C | PingResp | 心跳回复 | `timestamp` |

网关在**任意**收到包时刷新 `lastRecvAt`；超过 **10 秒**无消息即判定心跳超时并断开连接（[gateway/server.go](../internal/gateway/server.go) `heartbeatTimeout`）。客户端应每 5 秒左右发一次心跳。

### 3.2 登录 / 注册（1001–1004，C→S 经网关 → login）

| msgId | 方向 | 消息 | 用途 | 关键字段 |
|---:|---|---|---|---|
| 1001 | C→S | LoginReq | 登录 | `account`、`password`（明文传输，服务端 bcrypt 校验） |
| 1002 | S→C | LoginResp | 登录回包 | `code`、`msg`、`uid`、`token`、`session_id`（=token）、`level`、`stamina`、`exp`、`gold` |
| 1003 | C→S | RegisterReq | 注册 | `account`、`password`（服务端 bcrypt 哈希后入库） |
| 1004 | S→C | RegisterResp | 注册回包 | `code`、`msg` |

token 机制：登录成功由 login 生成 32 位随机 hex token，以 `token:<token> → uid` 写入 Redis，**TTL 2 小时**（[login/account.go](../internal/login/account.go) `issueToken`）。

### 3.3 战斗（2000–2010，C→S 经网关 → battle，限流）

| msgId | 方向 | 消息 | 用途 | 关键字段 |
|---:|---|---|---|---|
| 2000 | C→S | EnterRoomReq | 进房/创建房间 | `room_id`（0=新建；**重连时填旧房间号**）、`level_id`、`uid` |
| 2001 | S→C | EnterRoomResp | 进房回包 | `code`、`room_id`、`seed`（双方生成相同随机序列） |
| 2002 | C→S | DeployOperatorReq | 部署干员 | `room_id`、`operator_id`、`grid_x/y`、`direction`（0上1右2下3左） |
| 2003 | S→C | DeployOperatorResp | 部署回包 | `code`、`remaining_cost` |
| 2004 | S→C | BattleStatePush | **帧状态推送**（单向，每 tick 200ms=5帧/秒） | `frame`、`cost`、`base_hp`、`operators[]`、`enemies[]` |
| 2005 | S→C | BattleSettlePush | **结算推送**（单向） | `result`（0胜/1败/2撤退）、`score`、`duration_sec`、`rewards[]` |
| 2006 | C→S | RetreatOperatorReq | 撤回干员（返还部分费用） | `room_id`、`operator_id` |
| 2007 | S→C | RetreatOperatorResp | 撤回回包 | `code`、`remaining_cost` |
| 2008 | C→S | CastSkillReq | 释放技能 | `room_id`、`operator_id` |
| 2009 | S→C | CastSkillResp | 技能回包 | `code`、`remaining_cost`、`damage`、`hit_count` |
| 2010 | gateway→battle | 断线通知（**内部消息，不下发客户端**） | 客户端断开时网关通知 battle，把该玩家房间标记为「等待重连」 | JSON payload：`uid`、`room_id` |

**2010 说明**：只在网关与 battle 之间走内部 TCP（`connID=0`，JSON 编码，不是 protobuf），客户端永远收不到。battle 收到后进入等待重连状态，**30 秒内**没有同 uid 带原 `room_id` 重新进房（2000）则销毁房间（[battle/server.go](../internal/battle/server.go) `offlineTimeout`）。这也是客户端"按 R 断开 1.5 秒再连"能演示成功的依据。

子结构：

- `OperatorState`：`operator_id`、`grid_x/y`、`hp`、`direction`
- `EnemyState`：`enemy_id`（配置ID）、`instance_id`（场上实例ID）、`hp`、`pos_x/pos_y`（路径上的连续坐标）

### 3.4 DataSave 内部（3000+，服务间 TCP，JSON payload）

回包号 = 请求号 + 100。调用方：battle（进房扣体力、结算落库）、login（查角色）。

| msgId | 方向 | 消息 | 用途 | 关键字段 |
|---:|---|---|---|---|
| 3000 | battle→DS | SaveBattleRecordReq | 写战斗记录并累加 exp/gold | `uid`、`result`、`score`、`duration_sec`、`rewards[]` |
| 3100 | DS→battle | SaveBattleRecordResp | | `code`、`exp_gain`、`gold_gain` |
| 3001 | battle→DS | DeductStaminaReq | 扣体力 | `uid`、`amount` |
| 3101 | DS→battle | DeductStaminaResp | | `code`、`remaining`（**code=4 体力不足**） |
| 3002 | login→DS | GetRoleReq | 查角色 | `uid` |
| 3102 | DS→login | GetRoleResp | | `code`、`uid`、`name`、`level`、`stamina`、`exp`、`gold` |

### 3.5 配置表（不进网络包）

[api/config.proto](../api/config.proto) 定义 `OperatorCfg`（id/name/cost/attack/range/hp）、`LevelCfg`（id/waves/enemy_hp/enemy_speed/enemy_count/wave_interval）、`ConfigSet`。由 `cmd/configtool` 从 `config/*.xlsx` 生成 `config/config.pb`，battle 启动时本地加载（[internal/config](../internal/config/config.go)）。

## 4. connID 定向回包

服务间只有**一条** TCP 连接（网关↔后端），所有客户端消息复用其上，靠 connID 区分：

1. 网关为每个客户端连接分配自增 `connID`，转发时**覆盖**包头 connID 字段（`forwardToBackend`）。
2. 后端回包时把请求的 connID 原样填回。
3. 网关读后端连接，按 connID 查 Session 表，写给对应客户端（`handleLoginResp` / `handleBattleResp`）。
4. battle 的主动推送（2004/2005）同样带 connID —— 房间记录所属玩家的 connID，经同一通道回网关定向下发；断线重连后房间更新为新 connID。

## 5. 限流与超时规则（网关）

| 规则 | 值 | 位置 |
|---|---|---|
| 战斗消息（2000+）令牌桶 | 容量 50，每 5 秒回满 ⇒ **长期 10 条/秒/连接**，超频直接丢弃 | [session.go](../internal/gateway/session.go) `AllowBattleMsg` |
| 心跳超时 | 10 秒无任何包 ⇒ 断开 | [server.go](../internal/gateway/server.go) `heartbeatTimeout` |
| 心跳/登录消息 | 不限流 | — |
| login 并发上限 | `-max-inflight`（默认 8），防 bcrypt 占满 CPU（见 [压测报告](loadtest-report.md)） | [login/account.go](../internal/login/account.go) |

## 6. 错误码表

> 说明：[internal/common/error_code.go](../internal/common/error_code.go) 目前只有包声明（预留），各码值实际分散定义在各服务包内，汇总如下。`msg` 字段携带人读原因。

| code | login（1002/1004） | battle（2001/2003/2007/2009） | DataSave（3100/3101/3102） |
|---:|---|---|---|
| 0 | 成功 | 成功 | 成功 |
| 1 | 参数错误（含"账号密码错误""账号或密码不能为空"） | 参数错误（含"战斗已结束""坐标超出地图""干员数量已达上限""费用不足""干员不在场上""技能冷却中"） | 参数缺失/不合法 |
| 2 | 账号已存在（仅 1004） | 房间不存在或已结束 | 玩家不存在 |
| 3 | 服务器繁忙（DB/Redis 错误） | 内部服务（DataSave）调用失败 | 服务器内部错误 |
| 4 | — | —（由 3101 透传：体力不足，见 2001 回包 msg） | 体力不足（仅 3101） |
