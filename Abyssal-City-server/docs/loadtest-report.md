# Abyssal-City 服务端压测报告

日期：2026-11-07 ｜ 工具：`tmpverify/loadtest`（自研压测客户端）｜ 参数：`-conns 200 -games 30`

## 1. 测试方案

### 1.1 场景

模拟 200 个真实客户端同时在线，每个连接走完整链路：

```
TCP 连接 → 注册 → 登录 →（心跳保活）
                        └─ 其中 30 个连接：进房 → 收首帧 → 部署干员 → 持续收状态推送 → 收结算
```

- **并发在线连接**：200（全部保持在线并每 5s 发心跳，直到打局结束）
- **同时开打局数**：30（取前 30 个登录成功的连接，各打 1 局关卡 1）
- 每个请求-回包配对打时间戳（发送时刻记录在途、回包到达算差值），统计 RTT

### 1.2 环境

| 项 | 值 |
|---|---|
| 机器 | Windows 10/11 开发机，32 逻辑核，本地回环（loopback） |
| Go | 1.27.3 |
| 服务 | gateway / login / battle / datasave 四进程 + MySQL 8 / Redis 7 |
| 运行方式 | 本地 exe 进程（非 Docker），battle 加载 `config/config.pb` |

> 注：loopback 下网络 RTT 接近 0，测得的延迟主要反映服务端处理与排队耗时，对容量结论偏乐观。

### 1.3 采集方式

- 压测客户端逐包计数：收/发消息数、字节数、状态推送数、结算数
- RTT：register / login / enter / deploy / ping 分类采样，算 p50 / p95 / p99 / max
- 进程资源：压测工具内嵌 Windows 采样器（`OpenProcess` + `GetProcessTimes` + `GetProcessMemoryInfo`），每 500ms 采样各服务进程的 CPU / 工作集内存，取峰值
- 热点定位：`net/http/pprof`（login 增加 `-pprof` 参数开启）+ `go tool pprof`

## 2. 基线结果（优化前）

连接成功率 100%（200/200），结算 30/30 全胜，请求错误率 0%。

| 操作 | 样本 | p50 | p95 | p99 | max |
|---|---:|---:|---:|---:|---:|
| register | 200 | 7128.1 | 13494.1 | 14085.9 | 14151.8 |
| login | 200 | 13561.7 | 14156.3 | 14163.4 | 14164.7 |
| enter | 30 | 4.6 | 21.8 | 21.8 | 21.8 |
| deploy | 30 | 0.6 | 4.8 | 4.8 | 4.8 |
| ping | 1068 | 0.0 | 0.0 | 0.0 | 0.6 |
| **战斗路径（enter+deploy）** | 60 | 4.0 | **6.8** | 21.8 | 21.8 |
| **认证（register+login）** | 400 | 12952.3 | **14059.7** | 14161.0 | 14164.7 |

（单位：毫秒）

| 吞吐与资源 | 值 |
|---|---|
| 总耗时 / 战斗阶段 | 30.5s / 28.7s |
| 收消息 / 发消息 | 3688 条（121 条/秒）/ 1528 条（50 条/秒） |
| 战斗阶段推送吞吐 | 74 条/秒（状态推送共 2130 条） |
| login 进程 | 内存峰值 17.6 MB，CPU 峰值 **3.2%**（≈1 个核） |
| battle / gateway / datasave | 内存均 < 21 MB，CPU 峰值均 < 1% |

**判定**：战斗路径 p95=6.8ms < 50ms ✅ 达标；但**认证链路 p95≈14s 严重不达标**，成为最大瓶颈点。

## 3. 瓶颈定位（pprof）

在压测认证风暴期间对 login 抓 20s CPU profile：

```
Duration: 20s, Total samples = 1856
      flat  flat%       cum   cum%
      1795 96.71%       1796 96.77%  golang.org/x/crypto/blowfish.encryptBlock
         0     0%       1274 68.64%  golang.org/x/crypto/bcrypt.CompareHashAndPassword   ← 登录
         0     0%        544 29.31%  golang.org/x/crypto/bcrypt.GenerateFromPassword     ← 注册
         0     0%       1829 98.55%  main.handleGatewayConn
```

**根因（两层）**：

1. **算法成本**：bcrypt(cost=10) 单次约 60~70ms，是刻意的安全设计（防口令暴破），不可去除。
2. **串行放大**：网关到 login 只有**一条**后端 TCP 连接，`handleGatewayConn` 在读取循环里**同步串行**处理所有请求。400 个认证请求 × ~70ms 在同一条连接上排队 → 队尾请求等足 14s。login CPU 峰值仅 3.2%（1 核）证实：机器其余 31 个核完全闲置，纯排队浪费。

## 4. 优化与重测

### 4.1 优化 v1：login 请求并发处理

改动 [account.go](../internal/login/account.go)：每个请求独立 goroutine 处理（回包经 `connWriter` 加锁写，防包交错；单客户端一次一个在途请求，回包顺序不受影响）。

| 指标 | 优化前 | v1（并发 64） | 变化 |
|---|---:|---:|---|
| 认证 p95 | 14059.7ms | **645.9ms** | **↓ 95.4%（22 倍）** |
| 总耗时 | 30.5s | 15.2s | ↓ 50% |
| login CPU 峰值 | 3.2% | **84.7%**（≈27 核） | 吃满机器 |
| **战斗路径 p95** | 6.8ms | **208.1ms ❌** | **恶化 30 倍** |

**新问题（资源竞争）**：bcrypt 并发开满后把整机 CPU 拉到 84.7%，同机的 battle / gateway / datasave 被抢占，战斗路径 p95 从 6.8ms 恶化到 208ms（心跳 p99 也到 45ms）。认证提速反而拖垮了延迟敏感的战斗链路 —— **先测再改**才暴露了这个干扰，盲目把并发拉满是错误的。

### 4.2 优化 v2（最终）：并发上限削峰

给 login 加 `-max-inflight` 并发上限（默认 **8**）：bcrypt 只用约 1/4 的机器核数，给战斗链路留出 CPU。这是"削峰 + 资源隔离"的权衡。

## 5. 最终结果（v2，同参数 `-conns 200 -games 30`）

连接成功率 100%（200/200），结算 30/30 全胜，请求错误率 0%（0/460）。

| 操作 | 样本 | p50 | p95 | p99 | max |
|---|---:|---:|---:|---:|---:|
| register | 200 | 922.8 | 1691.0 | 1773.8 | 1775.2 |
| login | 200 | 1441.0 | 1763.9 | 1780.9 | 1786.5 |
| enter | 30 | 6.6 | 8.8 | 8.8 | 8.8 |
| deploy | 30 | 1.0 | 7.8 | 7.8 | 7.8 |
| ping | 469 | 0.0 | 0.0 | 0.5 | 0.7 |
| **战斗路径（enter+deploy）** | 60 | 5.1 | **7.8** | 8.8 | 8.8 |
| **认证（register+login）** | 400 | 1282.5 | **1729.6** | 1779.7 | 1786.5 |

（单位：毫秒）

| 吞吐与资源 | 值 |
|---|---|
| 总耗时 / 战斗阶段 | 15.5s / 15.2s |
| 收消息 / 发消息 | 3089 条（200 条/秒）/ 929 条（60 条/秒） |
| 战斗阶段推送吞吐 | 140 条/秒（状态推送 2130 条 ≈ 30 房间 × 5 帧/秒 × 14.2s） |
| login | 内存峰值 20.0 MB，CPU 峰值 24.9%（≈8 核，符合上限设计） |
| battle | 内存峰值 17.1 MB，CPU 峰值 0.4% |
| gateway | 内存峰值 27.3 MB，CPU 峰值 0.5% |
| datasave | 内存峰值 14.4 MB，CPU 峰值 0.2% |
| loadtest 自身 | goroutine 峰值 602，Go 堆峰值 3.2 MB |

### 三轮对比汇总

| 指标 | 基线（串行） | v1（并发 64） | v2（并发 8） |
|---|---:|---:|---:|
| 战斗路径 p95 | 6.8ms ✅ | 208.1ms ❌ | **7.8ms ✅** |
| 认证 p95 | 14059.7ms | 645.9ms | 1729.6ms |
| 总耗时 | 30.5s | 15.2s | 15.5s |
| login CPU 峰值 | 3.2% | 84.7% | 24.9% |
| 连接成功率 | 100% | 100% | 100% |
| 结算完成 | 30/30 | 30/30 | 30/30 |
| 请求错误率 | 0% | 0% | 0% |

## 6. 结论

1. **p95 < 50ms 判定：达标**。200 并发在线 + 30 局并行下，战斗消息（enter/deploy）p95 = 7.8ms，心跳 p99 = 0.5ms，全部请求错误率 0%。
2. 瓶颈不在战斗链路：30 房间 tick + 每帧广播对 battle（CPU 0.4%）和 gateway（0.5%）毫无压力，room 级锁与广播开销在本量级下远未饱和。
3. 唯一真实瓶颈是**认证链路的 bcrypt 串行排队**，已通过"并发处理 + 限流削峰"把 p95 从 14s 压到 1.7s（8 倍），同时保证战斗链路不被 CPU 风暴拖垮。
4. 认证延迟的下界由 bcrypt 算法成本决定（单次 ~70ms，安全设计不可降），突发注册登录场景下 p95 不可能 < 50ms；这是每个会话一次性开销，不影响对局体验。

## 7. 后续建议

- **认证服务独立部署**或独立线程池，与战斗服务物理隔离，彻底消除 CPU 竞争；
- 注册/登录入口加**排队削峰**（如令牌桶限流入队），把突发压成匀速，避免队列里请求超时；
- 客户端侧：登录失败/超时自动重试 + 退避；
- 生产环境用 Docker Compose 在 Linux 上复测一次（本报告为 Windows loopback 数据，网络偏乐观）；
- 若后续单战斗服房间数上一个量级（百级房间），需重新评估：每帧 `snapshotLocked` 全量序列化广播的成本、以及 `rooms` 全局 RWMutex 的竞争。

## 附：复现命令

```bash
# 启动依赖与服务（MySQL/Redis 先行）
go build -o bin/datasave.exe ./cmd/datasave   && ./bin/datasave.exe
go build -o bin/login.exe    ./internal/login && ./bin/login.exe -pprof 127.0.0.1:6062
go build -o bin/battle.exe   ./cmd/battle     && ./bin/battle.exe
go build -o bin/gateway.exe  ./cmd/gateway    && ./bin/gateway.exe

# 压测（-watch 填各服务 PID 采集资源；结果同时输出 JSON）
go build -o bin/loadtest.exe ./tmpverify/loadtest
./bin/loadtest.exe -conns 200 -games 30 \
  -watch "datasave=<pid>,login=<pid>,battle=<pid>,gateway=<pid>" -out result.json

# 抓 login CPU profile（压测进行中）
curl -o login.pprof "http://127.0.0.1:6062/debug/pprof/profile?seconds=20"
go tool pprof -text -nodecount=15 login.pprof
```
