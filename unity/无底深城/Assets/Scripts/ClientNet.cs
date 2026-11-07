// 客户端网络层：连接网关、注册、登录、进房、战斗指令、断线重连。
// 只负责收发与状态发布，界面由 GameUI 驱动；勾选 autoTestOnStart 可回到早期"自动注册进房"演示模式。
// 依赖 Assets/Scripts/Proto（namespace Api）与 Assets/Plugins/Google.Protobuf.dll。
using System;
using System.Collections.Concurrent;
using System.IO;
using System.Net.Sockets;
using System.Threading;
using UnityEngine;
using Google.Protobuf;
using Api;

public class ClientNet : MonoBehaviour
{
    public static ClientNet Instance;          // 全局单例，BattleRenderer / GameUI 都通过它发请求

    [Header("调试开关：勾选后自动注册随机账号并直接进房（跳过 UI）")]
    public bool autoTestOnStart = false;

    public string ServerHost = "127.0.0.1";
    public int ServerPort = 8888;

    TcpClient client;
    NetworkStream stream;
    byte[] readBuf = new byte[8192];
    MemoryStream cache = new MemoryStream();
    readonly object sendLock = new object();               // 主线程心跳与读线程发包互斥
    readonly ConcurrentQueue<Action> mainQueue = new ConcurrentQueue<Action>(); // 子线程回调转主线程

    // ───────── 对外只读状态 ─────────
    public string Account { get; private set; } = "";
    public long Uid { get; private set; }
    public uint CurrentRoomId { get; private set; }
    public int Level { get; private set; }
    public long Stamina { get; private set; }
    public long Exp { get; private set; }
    public long Gold { get; private set; }
    public bool LoggedIn => Uid != 0;
    public bool InBattle => CurrentRoomId != 0;
    public bool Connected => client != null && client.Connected && stream != null;

    public static BattleStatePush LatestState;  // 最新一帧战斗状态，供 BattleRenderer 读取

    bool deployed;        // 自动模式下是否已自动部署
    bool reconnecting;    // 正在重连（登录后要带旧 room_id 进房）
    uint savedRoomId;
    bool autoRunning;
    bool quitting;

    // ───────── 事件（全部在主线程触发） ─────────
    public event Action<RegisterResp> OnRegisterResp;
    public event Action<LoginResp> OnLoginResp;
    public event Action<EnterRoomResp> OnEnterRoomResp;
    public event Action<BattleSettlePush> OnSettle;
    public event Action<string> OnError;
    public event Action OnDisconnected;

    void Start()
    {
        Instance = this;
        Connect();

        if (autoTestOnStart)
        {
            autoRunning = true;
            Register("unity_" + DateTime.Now.Ticks, "123456");
        }
    }

    // 把子线程收到的回调搬到主线程执行（Unity API 只能在主线程调）
    void Update()
    {
        Action a;
        while (mainQueue.TryDequeue(out a))
        {
            try { a(); } catch (Exception e) { Debug.LogError("[Net] 回调异常: " + e); }
        }
    }

    void Raise(Action a) { mainQueue.Enqueue(a); }

    // ───────── 连接 ─────────
    public bool Connect()
    {
        try
        {
            CloseSocket();
            client = new TcpClient();
            client.Connect(ServerHost, ServerPort);
            stream = client.GetStream();
            cache.SetLength(0);

            var t = new Thread(ReadLoop) { IsBackground = true };
            t.Start();

            CancelInvoke("SendPing");
            InvokeRepeating("SendPing", 5f, 5f); // 心跳保活：网关 10 秒无包会踢连接

            Debug.Log("[Net] 连接网关成功 " + ServerHost + ":" + ServerPort);
            return true;
        }
        catch (Exception e)
        {
            Debug.LogError("[Net] 连接失败: " + e.Message);
            Raise(() => OnError?.Invoke("连接服务器失败：" + e.Message));
            return false;
        }
    }

    // ───────── 账号 ─────────
    public void Register(string account, string password)
    {
        Account = account;
        Send(1003, new RegisterReq { Account = account, Password = password }.ToByteArray());
        Debug.Log("[登录] 已发送注册 account=" + account);
    }

    public void Login(string account, string password)
    {
        Account = account;
        Send(1001, new LoginReq { Account = account, Password = password }.ToByteArray());
        Debug.Log("[登录] 已发送登录 account=" + account);
    }

    // ───────── 进房 ─────────
    public void EnterRoom(uint levelId)
    {
        if (!LoggedIn) { Raise(() => OnError?.Invoke("请先登录")); return; }
        Send(2000, new EnterRoomReq {
            Uid = (ulong)Uid, LevelId = levelId, RoomId = reconnecting ? savedRoomId : 0
        }.ToByteArray());
        Debug.Log("[战斗] 已发送进房 uid=" + Uid + " level=" + levelId +
                  (reconnecting ? "（重连恢复）" : "（新建房间）"));
    }

    // ───────── 战斗指令 ─────────
    public void SendDeploy(uint operatorId, uint gridX, uint gridY, int direction)
    {
        if (CurrentRoomId == 0) { Debug.Log("[战斗] 还没进房，无法部署"); return; }
        Send(2002, new DeployOperatorReq {
            RoomId = CurrentRoomId, OperatorId = operatorId,
            GridX = gridX, GridY = gridY, Direction = direction
        }.ToByteArray());
        Debug.Log("[战斗] 已发送部署 operator=" + operatorId +
                  " 格(" + gridX + "," + gridY + ") 方向=" + direction);
    }

    public void SendRetreat(uint operatorId)
    {
        if (CurrentRoomId == 0) return;
        Send(2006, new RetreatOperatorReq { RoomId = CurrentRoomId, OperatorId = operatorId }.ToByteArray());
        Debug.Log("[战斗] 已发送撤回 operator=" + operatorId);
    }

    public void SendSkill(uint operatorId)
    {
        if (CurrentRoomId == 0) return;
        Send(2008, new CastSkillReq { RoomId = CurrentRoomId, OperatorId = operatorId }.ToByteArray());
        Debug.Log("[战斗] 已发送技能 operator=" + operatorId);
    }

    // ───────── 断线重连演示：断 socket → 等 1.5 秒 → 重连 → 同账号登录 → 带旧 room_id 进房 ─────────
    public void SimulateReconnect()
    {
        if (CurrentRoomId == 0) { Debug.Log("[Net] 还没进房，无法演示重连"); return; }
        savedRoomId = CurrentRoomId;
        reconnecting = true;
        Debug.Log("[Net] 模拟断线：关闭 socket（网关会给 battle 发 2010 断线通知）");
        CloseSocket();
        StartCoroutine(ReconnectRoutine());
    }

    System.Collections.IEnumerator ReconnectRoutine()
    {
        yield return new WaitForSeconds(1.5f);
        if (!Connect()) { reconnecting = false; yield break; }
        Debug.Log("[Net] 已重新连接网关，发送登录（同账号）");
        Login(Account, "123456");
    }

    // ───────── 发送 / 收包 ─────────
    // 帧结构：[4B BodyLen][2B msgId][8B connID][payload]，大端
    void Send(ushort msgId, byte[] payload)
    {
        if (stream == null) { Debug.LogWarning("[Net] 尚未连接，丢弃消息 " + msgId); return; }
        int len = payload.Length;
        byte[] buf = new byte[4 + 2 + 8 + len];
        buf[0] = (byte)(len >> 24); buf[1] = (byte)(len >> 16);
        buf[2] = (byte)(len >> 8);  buf[3] = (byte)(len);
        buf[4] = (byte)(msgId >> 8); buf[5] = (byte)(msgId);
        Array.Copy(payload, 0, buf, 14, len);
        lock (sendLock)
        {
            try { stream.Write(buf, 0, buf.Length); }
            catch (Exception e) { Debug.LogWarning("[Net] 发送失败（可能已断线）: " + e.Message); }
        }
    }

    void SendPing()
    {
        try { Send(0, new PingReq { Timestamp = 0 }.ToByteArray()); } catch { }
    }

    void ReadLoop()
    {
        try
        {
            while (true)
            {
                int n = stream.Read(readBuf, 0, readBuf.Length);
                if (n <= 0)
                {
                    Debug.Log("[Net] 连接关闭");
                    if (!reconnecting && !quitting) Raise(() => OnDisconnected?.Invoke());
                    break;
                }
                cache.Write(readBuf, 0, n);
                ParseFrames();
            }
        }
        catch (Exception e)
        {
            if (!reconnecting && !quitting)
            {
                Debug.Log("[Net] 读取异常: " + e.Message);
                Raise(() => OnDisconnected?.Invoke());
            }
        }
    }

    // 处理半包/粘包：缓冲区不够一个完整包就等下次
    void ParseFrames()
    {
        while (true)
        {
            byte[] data = cache.ToArray();
            if (data.Length < 14) return;

            int bodyLen = (data[0] << 24) | (data[1] << 16) | (data[2] << 8) | data[3];
            int total = 14 + bodyLen;
            if (data.Length < total) return;

            ushort msgId = (ushort)((data[4] << 8) | data[5]);
            byte[] payload = new byte[bodyLen];
            Array.Copy(data, 14, payload, 0, bodyLen);

            cache.SetLength(0);
            if (data.Length > total) cache.Write(data, total, data.Length - total);

            HandleMsg(msgId, payload);
        }
    }

    void HandleMsg(ushort msgId, byte[] payload)
    {
        switch (msgId)
        {
            case 1004: // 注册回包
                var reg = RegisterResp.Parser.ParseFrom(payload);
                Debug.Log("[登录] 注册回包 code=" + reg.Code + " msg=" + reg.Msg);
                if (autoRunning)
                    Send(1001, new LoginReq { Account = Account, Password = "123456" }.ToByteArray());
                Raise(() => OnRegisterResp?.Invoke(reg));
                break;

            case 1002: // 登录回包
                var login = LoginResp.Parser.ParseFrom(payload);
                Debug.Log("[登录] 登录回包 code=" + login.Code + " uid=" + login.Uid +
                          " 体力=" + login.Stamina + " exp=" + login.Exp + " gold=" + login.Gold +
                          " msg=" + login.Msg);
                if (login.Code == 0)
                {
                    Uid = login.Uid;
                    Level = (int)login.Level;
                    Stamina = login.Stamina;
                    Exp = login.Exp;
                    Gold = login.Gold;
                    if (autoRunning)
                        Send(2000, new EnterRoomReq { Uid = (ulong)Uid, LevelId = 1, RoomId = 0 }.ToByteArray());
                }
                Raise(() => OnLoginResp?.Invoke(login));
                break;

            case 2001: // 进房回包
                var enter = EnterRoomResp.Parser.ParseFrom(payload);
                Debug.Log("[战斗] 进房回包 code=" + enter.Code + " roomId=" + enter.RoomId +
                          " seed=" + enter.Seed + " msg=" + enter.Msg);
                if (enter.Code == 0)
                {
                    CurrentRoomId = enter.RoomId;
                    deployed = false;
                    if (reconnecting)
                    {
                        reconnecting = false;
                        Debug.Log("[战斗] 重连成功，从房间 " + CurrentRoomId + " 继续（状态推送即将恢复）");
                    }
                }
                Raise(() => OnEnterRoomResp?.Invoke(enter));
                break;

            case 2004: // 战斗状态推送（每 tick 一条）
                var state = BattleStatePush.Parser.ParseFrom(payload);
                LatestState = state;
                if (autoRunning && !deployed && CurrentRoomId != 0)
                {
                    deployed = true;
                    Send(2002, new DeployOperatorReq {
                        RoomId = CurrentRoomId, OperatorId = 1, GridX = 4, GridY = 0, Direction = 1
                    }.ToByteArray());
                    Debug.Log("[战斗] 已自动部署 1 号干员 于(4,0)");
                }
                if (state.Frame % 10 == 0)
                {
                    Debug.Log("[战斗] frame=" + state.Frame + " cost=" + state.Cost +
                              " baseHP=" + state.BaseHp +
                              " 干员=" + state.Operators.Count + " 敌人=" + state.Enemies.Count);
                }
                break;

            case 2003:
                var dep = DeployOperatorResp.Parser.ParseFrom(payload);
                Debug.Log("[战斗] 部署回包 code=" + dep.Code + " operator=" + dep.OperatorId +
                          " 剩余费用=" + dep.RemainingCost + " msg=" + dep.Msg);
                break;

            case 2007:
                var ret = RetreatOperatorResp.Parser.ParseFrom(payload);
                Debug.Log("[战斗] 撤回回包 code=" + ret.Code + " operator=" + ret.OperatorId +
                          " 剩余费用=" + ret.RemainingCost + " msg=" + ret.Msg);
                break;

            case 2009:
                var sk = CastSkillResp.Parser.ParseFrom(payload);
                Debug.Log("[战斗] 技能回包 code=" + sk.Code + " operator=" + sk.OperatorId +
                          " 伤害=" + sk.Damage + " 命中=" + sk.HitCount +
                          " 剩余费用=" + sk.RemainingCost + " msg=" + sk.Msg);
                break;

            case 2005: // 结算推送
                var settle = BattleSettlePush.Parser.ParseFrom(payload);
                Debug.Log("[战斗] 结算 result=" + settle.Result + " score=" + settle.Score +
                          " 时长=" + settle.DurationSec + "s rewards=" +
                          string.Join(",", settle.Rewards));
                CurrentRoomId = 0;
                deployed = false;
                Raise(() => OnSettle?.Invoke(settle));
                break;

            case 1:
                Debug.Log("[Net] Pong");
                break;

            default:
                Debug.Log("[Net] 收到 msgId=" + msgId + " len=" + payload.Length);
                break;
        }
    }

    void CloseSocket()
    {
        CancelInvoke("SendPing");
        try { stream?.Close(); } catch { }
        try { client?.Close(); } catch { }
        stream = null;
    }

    void OnDestroy()
    {
        quitting = true;
        Instance = null;
        CloseSocket();
    }
}
