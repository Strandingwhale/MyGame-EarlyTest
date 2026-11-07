# 踩坑复盘

按"现象 → 原因 → 解决"记录本项目实际踩到的问题。每条都能在代码或运行日志里对上。

---

## 1. Go 环境是 386 版，不能开 -race

- **现象**：`go test -race` 报不支持；并发问题只能靠日志判断。
- **原因**：本机 Go 是 `go1.27.3 windows/386`（32 位），race detector 不支持该平台。
- **解决**：把共享变量改成 `atomic.Pointer` / 加锁（如网关 `loginConn` 用 `atomic.Pointer[net.Conn]`），再靠端到端脚本验证并发行为。

## 2. 心跳 10 秒超时，客户端"莫名断线"

- **现象**：连通性测试脚本发一次 Ping、收到 Pong，10 秒后连接被关。
- **原因**：网关 `heartbeatTimeout = 10s`，超过 10 秒没收到任何包就断开。
- **解决**：客户端 `InvokeRepeating("SendPing", 5f, 5f)`，每 5 秒一次心跳；战斗 16 秒全程不断。

## 3. 自己拼帧头，网关只处理 1 帧

- **现象**：临时限流脚本连发 70 条，网关只当 1 条。
- **原因**：帧头长度字段写成 `10 + payload`，而协议是 `4B 长度 + 2B msgId + 8B connID`，长度应为 payload 长度。
- **解决**：测试脚本改用服务端 `common.Encode` 生成帧；复测 = 50 条转发 + 20 条限流丢弃，与设计一致。

## 4. 进房不带 uid，体力扣错人、结算落错账

- **现象**：早期 `EnterRoomReq` 没有 uid，扣体力与写 `battle_record` 用的都是 0。
- **原因**：battle 需要玩家身份来扣体力、结算落库。
- **解决**：`EnterRoomReq` 增加 `uid`；登录回包 `LoginResp` 带 `uid/level/stamina/exp/gold`；battle 校验 `uid != 0` 后调 DataSave 3001 扣体力，结算用真 uid。

## 5. 注册与登录同时发，登录查不到账号

- **现象**：客户端 Start 里连续发 1003（注册）+ 1001（登录），偶发登录失败。
- **原因**：login 对请求是并发处理（每请求一个 goroutine），登录可能先于建号执行。
- **解决**：客户端改成"收到 1004 注册回包后再发 1001"，把两步串行化。

## 6. 断线重连：不带旧 room_id 就是新开一局

- **现象**：重连后帧号从 0 开始，说明被当成新房间。
- **原因**：`room_id=0` 一律新建房间；重连必须带回原房间号，且 uid 必须与房间归属一致。
- **解决**：客户端断线前保存 `savedRoomId`，重连登录成功后发 `EnterRoomReq{room_id=旧, uid=同一账号}`；battle 走 `resume()`，更新 connID 并从当前帧继续推 2004。验收标准：**帧号连续、不归零**。

## 7. bcrypt 串行排队把认证拖到 14 秒

- **现象**：压测基线里战斗 p95 只有 6.8ms，但认证 p95 高达 14s。
- **原因**：pprof 显示 96.7% 时间在 `blowfish.encryptBlock`——bcrypt 是 CPU 密集，请求串行排队。
- **解决**：login 加并发 + `-max-inflight 8` 限制同时在跑的数量。第二轮认证 p95 降到 1.7s，战斗 p95 7.8ms，login CPU 24.9%。详见 `docs/loadtest-report.md`。

## 8. C# 客户端报 "ToByteArray 未定义"

- **现象**：Unity 编译报 5 处 `CS1061: "RegisterReq" 未包含 "ToByteArray" 的定义`。
- **原因**：`ToByteArray()` 是 `Google.Protobuf` 命名空间的扩展方法，脚本只 `using Api;` 没 `using Google.Protobuf;`。
- **解决**：`ClientNet.cs` 顶部补 `using Google.Protobuf;`。

## 9. Google.Protobuf 的依赖链，Unity 要一起放

- **现象**：Unity 报 `Assembly 'Assets/Plugins/Google.Protobuf.dll' will not be loaded`、
  `Unable to resolve reference 'System.Runtime.CompilerServices.Unsafe'`，连带 `Assembly-CSharp` 编译失败、脚本"丢失"。
- **原因**：`Google.Protobuf 3.28.3 (netstandard2.0)` 依赖 `System.Memory`，后者又依赖 `System.Buffers`、`System.Numerics.Vectors`、`System.Runtime.CompilerServices.Unsafe`；Unity 工程里都没有。
- **解决**：把 4 个 dll 一起放进 `Assets/Plugins/`：
  `System.Memory.dll`、`System.Buffers.dll`、`System.Numerics.Vectors.dll`、`System.Runtime.CompilerServices.Unsafe.dll`。

## 10. 团结引擎和 Unity 的几处差异

- 场景文件扩展名是 **`.scene`**（不是 `.unity`），查挂载脚本要搜 `Assets/Scenes/*.scene`。
- `.meta` 里的 guid 是 **base64 44 字符**，不是 Unity 标准的 32 位 hex，肉眼对不上属正常。
- `ProjectSettings` 里 `apiCompatibilityLevel: 6`（.NET Standard）、`activeInputHandler: 0`（旧 Input Manager），所以 `Input.GetMouseButtonDown` 可直接用。
- 版本为 `2022.3.62t17` / Tuanjie 1.11.0，编辑器在 `D:\tool soft\tuanjie\2022.3.62t17`。

## 11. 不启动编辑器也能验证 C# 能不能编译

- **做法**：新建 `dotnet` classlib（`netstandard2.1`、`LangVersion 9`），引用团结引擎 `Editor\Data\Managed` 下的 `UnityEngine*.dll` 与 `Google.Protobuf.dll`，把 `Base.cs/Config.cs/ClientNet.cs/BattleRenderer.cs` 一起 build。
- **收益**：秒级拿到真实报错（如 `CS1061`、`CS0029`），不用等编辑器重编译、不用猜。

## 12. 心跳与业务发包抢同一个 socket

- **现象**：战斗中偶发发送异常。
- **原因**：心跳在 Unity 主线程（`InvokeRepeating`），业务发包在读线程（收到回包后发下一条），共用 `NetworkStream` 可能交错。
- **解决**：`Send()` 里用 `lock (sendLock)` 包住 `stream.Write`。

## 13. 敌人位置是连续浮点，不是格子整数

- **现象**：如果按整数画，敌人会一格一格跳。
- **原因**：`EnemyState.pos_x/pos_y` 是路径上的**连续坐标**（0→8 格），速度单位是"格/秒"。
- **解决**：渲染直接 `(pos_x + 0.5) * CELL` 映射世界坐标，得到平滑移动。

## 14. 自动部署和手动交互打架

- **现象**：客户端首次收到状态时自动在 (4,0) 部署一个干员，和手动点格子部署同时存在。
- **原因**：早期为看效果加的演示逻辑，交互上线后没关。
- **解决**：保留（保证开局有干员可看），但要演示"纯手动"时先点它撤回；或后续把 `deployed` 自动部署开关去掉。

## 15. protoc 生成的 .cs 不能手改

- **规则**：`Assets/Scripts/Proto/Base.cs`、`Config.cs` 由 protoc 生成，文件头明确 `DO NOT EDIT`。
- **正确做法**：改 `api/base.proto` / `api/config.proto`，然后重跑：
  ```
  protoc -I api --csharp_out="D:\无底深城\unity\无底深城\Assets\Scripts\Proto" api\base.proto api\config.proto
  ```
  这样服务端与客户端协议永久同步。
