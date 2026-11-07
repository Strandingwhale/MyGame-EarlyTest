# Abyssal-City 服务端部署手册

## 1. 前置依赖

| 依赖 | 版本 | 用途 |
|---|---|---|
| Go | **1.27.3**（见 [go.mod](../go.mod)） | 编译运行 4 个服务 |
| MySQL | 8.x（3306） | 账号/角色/战斗记录持久化 |
| Redis | 7.x（6379） | 登录 token（TTL 2h） |

数据库初始化：首次启动 MySQL 前执行 [db/schema.sql](../db/schema.sql)，建 3 张表：

- `account`（id 即 uid，account 唯一，password_hash 为 bcrypt）
- `player`（uid 外键 → account.id；默认 level=1、stamina=120、exp/gold=0）
- `battle_record`（每场结算一条，uid 外键 → player.uid）

## 2. 配置表生成

战斗数值来自 Excel，需先生成 protobuf 配置（**工作目录必须是仓库根**）：

```bash
go run ./cmd/configtool            # 读 config/operator.xlsx + config/level.xlsx
                                   # 输出 config/config.pb（-out 可改路径）
```

battle 启动时加载 `config/config.pb`；改表后需重跑 configtool 并重启 battle。

## 3. 启动顺序（重要）

后端互相有依赖，**必须按序启动**，每个等上一個就绪（看日志）再启下一个：

```bash
# 1. datasave（:9997）—— MySQL 连接池，最先
go run ./cmd/datasave        # 或 go build -o bin/datasave.exe ./cmd/datasave && ./bin/datasave.exe

# 2. login（:9999）—— 依赖 MySQL + Redis；可选 -pprof 127.0.0.1:6062 开性能端点
go run ./internal/login

# 3. battle（:9998）—— 依赖 config/config.pb（相对路径，cwd=仓库根）
go run ./cmd/battle

# 4. gateway（:8888，客户端入口）—— 最后，启动后自动连 login/battle（断线自动重连）
go run ./cmd/gateway
```

> 网关对后端有自动重连，后端重启不必重启网关；但冷启动仍建议按上序，避免首波请求失败。

## 4. 两种跑法

### 4.1 Docker Compose（推荐，仅起基础设施）

[docker-compose.yml](../docker-compose.yml) 已改 **daocloud 镜像源**（`docker.m.daocloud.io`），只含 MySQL + Redis，4 个 Go 服务仍在本机跑：

```bash
docker compose up -d        # 起 abyssal-mysql(3306) + abyssal-redis(6379)
# MySQL 首次启动自动建库 abyssal_city 并执行 db/schema.sql（挂载 /docker-entrypoint-initdb.d）
# 数据卷 mysql-data / redis-data 持久化
docker compose down         # 停止
```

**注意 DSN 不一致**：compose 里 MySQL root 密码是 `root123`，而 [cmd/datasave/main.go](../cmd/datasave/main.go) 与 [internal/login/account.go](../internal/login/account.go) 中 DSN **硬编码**为 `root:fox114514@tcp(127.0.0.1:3306)/abyssal_city?parseTime=true`。用 compose 的 MySQL 时，要么把两处 DSN 改成 `root123`，要么把 compose 的 `MYSQL_ROOT_PASSWORD` 改成 `fox114514`（改密码需清掉旧数据卷：`docker compose down -v`）。

### 4.2 本机直连

本机已装 MySQL/Redis 时不用 Docker：保证 3306/6379 可达、root 密码与代码 DSN 一致（`fox114514`），并手动建库导表：

```bash
mysql -uroot -p -e "CREATE DATABASE IF NOT EXISTS abyssal_city CHARSET utf8mb4"
mysql -uroot -p abyssal_city < db/schema.sql
```

## 5. 端口表

| 端口 | 进程 | 作用 | 暴露面 |
|---|---|---|---|
| 8888 | gateway | 客户端 TCP 入口 | **对外** |
| 9999 | login | 注册/登录（仅网关访问） | 内网 |
| 9998 | battle | 战斗房间（仅网关访问） | 内网 |
| 9997 | datasave | 数据落库（login/battle 访问） | 内网 |
| 3306 | MySQL | 数据库 | 内网 |
| 6379 | Redis | token | 内网 |
| 6062 | login（可选） | pprof（`-pprof` 开启） | 仅本机 |

## 6. 环境变量 / 配置项

当前**无环境变量**，连接参数均为代码内常量，部署到非本机环境需要改码：

| 配置 | 位置 | 当前值 |
|---|---|---|
| login → MySQL DSN | [internal/login/account.go](../internal/login/account.go) `main()` | `root:fox114514@tcp(127.0.0.1:3306)/abyssal_city?parseTime=true` |
| datasave → MySQL DSN | [cmd/datasave/main.go](../cmd/datasave/main.go) | 同上 |
| login → Redis | internal/login/account.go | `127.0.0.1:6379` |
| gateway → login / battle | [internal/gateway/server.go](../internal/gateway/server.go) | `127.0.0.1:9999` / `127.0.0.1:9998` |
| battle → datasave | internal/battle（`dataSaveAddr`） | `127.0.0.1:9997` |
| login 并发上限 | 启动参数 `-max-inflight` | 默认 8（削峰，防 bcrypt 占满 CPU，见[压测报告](loadtest-report.md)） |
| login pprof | 启动参数 `-pprof` | 默认关闭 |

## 7. 常见问题（FAQ）

1. **注册/登录突然变慢（秒级）**：bcrypt(cost=10) 单次 ~70ms 是刻意的安全开销；突发下瓶颈是 CPU 而非 DB。已修复为并发处理 + `-max-inflight` 削峰，细节与数据见 [docs/loadtest-report.md](loadtest-report.md)。生产建议认证服务独立部署。
2. **3306/6379 被占用**：本机已装 MySQL/Redis 时又起 compose 会端口冲突 —— 二选一：停本机服务用 Docker，或关掉 compose 用本机直连（§4.2）。
3. **battle 启动报配置加载失败**：`config/config.pb` 是相对路径，必须在**仓库根目录**启动 battle；且需先跑 configtool（§2）。
4. **登录失败"账号密码错误"但确实注册过**：检查是否连错库（compose 的 `abyssal_city` vs 本机库），或 DSN 密码不一致（§4.1 的坑）。
5. **Windows 下重新编译提示 exe 被占用**：先停掉正在运行的对应进程再 `go build`；服务在后台跑时可用 `Get-Process <名字> | Stop-Process`。
6. **token 失效**：Redis 重启且未持久化 / TTL 2h 到期，重新登录即可。
