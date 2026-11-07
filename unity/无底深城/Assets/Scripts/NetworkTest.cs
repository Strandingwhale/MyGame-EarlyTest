using UnityEngine;
using System.Net.Sockets;
using System.Threading;

public class NetworkTest : MonoBehaviour
{
    TcpClient client;
    NetworkStream stream;

    void Start()
    {
        client = new TcpClient();
        client.Connect("127.0.0.1", 8888);
        Debug.Log("[Net] 连接网关成功 127.0.0.1:8888");
        stream = client.GetStream();

        var t = new Thread(ReadLoop);
        t.IsBackground = true;
        t.Start();

        byte[] frame = MakeFrame((ushort)0, 0UL, new byte[0]);
        stream.Write(frame, 0, frame.Length);
        Debug.Log("[Net] 已发送 Ping");
    }

    byte[] MakeFrame(ushort msgId, ulong connId, byte[] payload)
    {
        int len = payload.Length;
        byte[] buf = new byte[4 + 2 + 8 + len];
        buf[0] = (byte)(len >> 24); buf[1] = (byte)(len >> 16);
        buf[2] = (byte)(len >> 8);  buf[3] = (byte)(len);
        buf[4] = (byte)(msgId >> 8); buf[5] = (byte)(msgId);
        for (int i = 0; i < 8; i++)
            buf[6 + i] = (byte)(connId >> (8 * (7 - i)));
        System.Array.Copy(payload, 0, buf, 14, len);
        return buf;
    }

    void ReadLoop()
    {
        try
        {
            byte[] buf = new byte[4096];
            while (true)
            {
                int n = stream.Read(buf, 0, buf.Length);
                if (n <= 0) { Debug.Log("[Net] 连接关闭"); break; }
                if (n >= 6)
                {
                    ushort msgId = (ushort)((buf[4] << 8) | buf[5]);
                    Debug.Log("[Net] 收到回包 msgId=" + msgId + " 字节数=" + n);
                }
            }
        }
        catch (System.Exception e) { Debug.Log("[Net] 读取异常: " + e.Message); }
    }
}
