// 战斗画面渲染：把 ClientNet 收到的 BattleStatePush 画成方块/圆点。
// 地图 9 格宽 × 2 格高；干员在第 1 行(grid_y=0)，敌人沿第 2 行从左向右推进，基地在最右端。
// 依赖：ClientNet.LatestState（由 ClientNet 每帧写入）。
using System.Collections.Generic;
using UnityEngine;
using UnityEngine.EventSystems;
using Api;

public class BattleRenderer : MonoBehaviour
{
    [Header("由 GameUI 控制：只有战斗中才渲染与响应点击")]
    public bool BattleActive = false;

    const float CELL = 1f;   // 一格 = 1 世界单位
    const int MAP_W = 9;
    const int MAP_H = 2;

    Sprite whiteSprite;      // 1×1 白色方块
    Sprite circleSprite;     // 圆形

    readonly Dictionary<ulong, Transform> enemyViews = new Dictionary<ulong, Transform>();
    readonly Dictionary<uint, Transform> opViews = new Dictionary<uint, Transform>();

    GUIStyle hudStyle;

    void Start()
    {
        whiteSprite = MakeWhiteSprite();
        circleSprite = MakeCircleSprite();
        SetupCamera();
        BuildBoard();
        Debug.Log("[View] 画面初始化完成（9×2 网格）");
    }

    // ───────── 相机 ─────────
    void SetupCamera()
    {
        Camera cam = Camera.main;
        if (cam == null)
        {
            Debug.LogWarning("[View] 没有 Main Camera，画面可能看不到");
            return;
        }
        cam.orthographic = true;
        cam.orthographicSize = CalcSize();
        cam.transform.position = new Vector3(MAP_W * CELL * 0.5f, -MAP_H * CELL * 0.5f, -10f);
        cam.clearFlags = CameraClearFlags.SolidColor;
        cam.backgroundColor = new Color(0.94f, 0.96f, 0.98f);
    }

    float CalcSize()
    {
        float aspect = (float)Screen.width / Mathf.Max(1, Screen.height);
        float halfW = MAP_W * CELL * 0.5f + 0.7f;  // 留边框
        float halfH = MAP_H * CELL * 0.5f + 0.7f;
        return Mathf.Max(halfH, halfW / aspect);
    }

    // ───────── 静态底图 ─────────
    void BuildBoard()
    {
        // 整块底板
        MakeSprite(whiteSprite, new Color(0.90f, 0.93f, 0.96f),
            new Vector3(MAP_W * CELL * 0.5f, -MAP_H * CELL * 0.5f, 0f),
            new Vector3(MAP_W * CELL, MAP_H * CELL, 1f), 0, "Board");

        // 敌人路径行（第 2 行整条）
        MakeSprite(whiteSprite, new Color(0.96f, 0.86f, 0.86f),
            new Vector3(MAP_W * CELL * 0.5f, -(1 + 0.5f) * CELL, 0f),
            new Vector3(MAP_W * CELL, CELL, 1f), 1, "EnemyPath");

        // 基地（最右端竖条）
        MakeSprite(whiteSprite, new Color(0.62f, 0.67f, 0.92f),
            new Vector3(MAP_W * CELL + 0.4f, -MAP_H * CELL * 0.5f, 0f),
            new Vector3(0.45f, MAP_H * CELL, 1f), 2, "Base");

        // 竖网格线
        for (int x = 1; x < MAP_W; x++)
        {
            MakeSprite(whiteSprite, new Color(1f, 1f, 1f, 0.5f),
                new Vector3(x * CELL, -MAP_H * CELL * 0.5f, 0f),
                new Vector3(0.02f, MAP_H * CELL, 1f), 3, "VLine" + x);
        }
        // 横网格线（两行之间）
        MakeSprite(whiteSprite, new Color(1f, 1f, 1f, 0.5f),
            new Vector3(MAP_W * CELL * 0.5f, -CELL, 0f),
            new Vector3(MAP_W * CELL, 0.02f, 1f), 3, "HLine");
    }

    // ───────── 每帧刷新 ─────────
    void Update()
    {
        if (!BattleActive) return;   // 非战斗页面（登录/主界面/选关）不渲染、不响应输入
        BattleStatePush st = ClientNet.LatestState;
        if (st == null) return;
        UpdateOperators(st);
        UpdateEnemies(st);
        HandleInput(st);
    }

    // ───────── 交互：点格子部署 / 点干员撤回 / Q 放技能 ─────────
    void HandleInput(BattleStatePush st)
    {
        Camera cam = Camera.main;
        if (cam == null) return;

        // 鼠标压在 UI 按钮上时不当作战场点击，避免点"退出战斗"误部署
        if (EventSystem.current != null && EventSystem.current.IsPointerOverGameObject()) return;

        if (Input.GetMouseButtonDown(0))
        {
            Vector3 w = cam.ScreenToWorldPoint(Input.mousePosition);
            int gx = Mathf.FloorToInt(w.x);
            int gy = Mathf.FloorToInt(-w.y);
            if (gx < 0 || gx >= MAP_W || gy < 0 || gy >= MAP_H) return;

            // 该格是否已有干员
            uint opId = 0;
            foreach (OperatorState op in st.Operators)
            {
                if ((int)op.GridX == gx && (int)op.GridY == gy) { opId = op.OperatorId; break; }
            }

            if (opId != 0)
            {
                if (ClientNet.Instance != null) ClientNet.Instance.SendRetreat(opId);
                Debug.Log("[View] 点击干员 " + opId + " → 撤回");
                return;
            }

            if (gy == 0)
            {
                // 点在格子左半边朝左(3)，右半边朝右(1)
                float localX = w.x - gx;
                int dir = localX > 0.5f ? 1 : 3;
                if (ClientNet.Instance != null) ClientNet.Instance.SendDeploy(1, (uint)gx, (uint)gy, dir);
                Debug.Log("[View] 点击空格 (" + gx + "," + gy + ") → 部署，方向 " + dir);
            }
            else
            {
                Debug.Log("[View] 第 2 行是敌人路径，不能部署");
            }
        }

        if (Input.GetKeyDown(KeyCode.Q))
        {
            foreach (OperatorState op in st.Operators)
            {
                if (ClientNet.Instance != null) ClientNet.Instance.SendSkill(op.OperatorId);
                Debug.Log("[View] Q → 对干员 " + op.OperatorId + " 放技能");
                break;
            }
        }

        // R：断线重连演示（断开 1.5 秒后重连，服务端从原帧继续推）
        if (Input.GetKeyDown(KeyCode.R))
        {
            if (ClientNet.Instance != null) ClientNet.Instance.SimulateReconnect();
        }
    }

    void UpdateOperators(BattleStatePush st)
    {
        HashSet<uint> alive = new HashSet<uint>();
        foreach (OperatorState op in st.Operators)
        {
            alive.Add(op.OperatorId);
            if (!opViews.ContainsKey(op.OperatorId))
            {
                var sr = MakeSprite(whiteSprite, new Color(0.55f, 0.78f, 0.92f),
                    Vector3.zero, new Vector3(0.78f, 0.78f, 1f), 4, "Op" + op.OperatorId);
                AddFacingGun(sr.transform, op.Direction);
                opViews[op.OperatorId] = sr.transform;
            }
            Transform view = opViews[op.OperatorId];
            view.position = GridToWorld(op.GridX, op.GridY);

            // 干员血量：越少越暗（展示用，不加血条）
            float hpRatio = Mathf.Clamp01((float)op.Hp / 100f);
            var r = view.GetComponent<SpriteRenderer>();
            if (r != null) r.color = new Color(0.55f, 0.78f, 0.92f) * (0.55f + 0.45f * hpRatio);
        }

        // 移除已经不存在的干员
        List<uint> gone = null;
        foreach (var kv in opViews)
            if (!alive.Contains(kv.Key)) { if (gone == null) gone = new List<uint>(); gone.Add(kv.Key); }
        if (gone != null)
            foreach (uint k in gone) { Destroy(opViews[k].gameObject); opViews.Remove(k); }
    }

    void UpdateEnemies(BattleStatePush st)
    {
        HashSet<ulong> alive = new HashSet<ulong>();
        foreach (EnemyState e in st.Enemies)
        {
            alive.Add(e.InstanceId);
            if (!enemyViews.ContainsKey(e.InstanceId))
            {
                var sr = MakeSprite(circleSprite, new Color(0.92f, 0.40f, 0.41f),
                    Vector3.zero, new Vector3(0.62f, 0.62f, 1f), 6, "Enemy" + e.InstanceId);
                enemyViews[e.InstanceId] = sr.transform;
            }
            Transform view = enemyViews[e.InstanceId];
            // pos_x 是连续浮点（0→8），直接映射成世界坐标，移动平滑
            view.position = PosToWorld(e.PosX, e.PosY);
        }

        List<ulong> gone = null;
        foreach (var kv in enemyViews)
            if (!alive.Contains(kv.Key)) { if (gone == null) gone = new List<ulong>(); gone.Add(kv.Key); }
        if (gone != null)
            foreach (ulong k in gone) { Destroy(enemyViews[k].gameObject); enemyViews.Remove(k); }
    }

    Vector3 GridToWorld(uint gx, uint gy)
    {
        return new Vector3((gx + 0.5f) * CELL, -(gy + 0.5f) * CELL, 0f);
    }

    Vector3 PosToWorld(float px, float py)
    {
        return new Vector3((px + 0.5f) * CELL, -(py + 0.5f) * CELL, 0f);
    }

    // 给干员加一条朝向"枪线"
    void AddFacingGun(Transform parent, int direction)
    {
        GameObject gun = new GameObject("Gun");
        gun.transform.SetParent(parent, false);
        var sr = gun.AddComponent<SpriteRenderer>();
        sr.sprite = whiteSprite;
        sr.color = new Color(0.16f, 0.43f, 0.54f);
        sr.sortingOrder = 5;
        gun.transform.localScale = new Vector3(0.42f, 0.07f, 1f);

        // direction: 0上 1右 2下 3左
        float angle = 0f;
        Vector3 offset = new Vector3(0.25f, 0f, 0f);
        switch (direction)
        {
            case 0: angle = 90f;  offset = new Vector3(0f, 0.25f, 0f); break;
            case 1: angle = 0f;   offset = new Vector3(0.25f, 0f, 0f); break;
            case 2: angle = -90f; offset = new Vector3(0f, -0.25f, 0f); break;
            case 3: angle = 180f; offset = new Vector3(-0.25f, 0f, 0f); break;
        }
        gun.transform.localPosition = offset;
        gun.transform.localRotation = Quaternion.Euler(0f, 0f, angle);
    }

    // ───────── 工具 ─────────
    SpriteRenderer MakeSprite(Sprite s, Color c, Vector3 pos, Vector3 scale, int order, string name)
    {
        var go = new GameObject(name);
        go.transform.SetParent(transform, false);
        var sr = go.AddComponent<SpriteRenderer>();
        sr.sprite = s;
        sr.color = c;
        sr.sortingOrder = order;
        go.transform.position = pos;
        go.transform.localScale = scale;
        return sr;
    }

    Sprite MakeWhiteSprite()
    {
        int n = 4;
        var tex = new Texture2D(n, n, TextureFormat.RGBA32, false);
        var px = new Color[n * n];
        for (int i = 0; i < px.Length; i++) px[i] = Color.white;
        tex.SetPixels(px);
        tex.Apply();
        return Sprite.Create(tex, new Rect(0, 0, n, n), new Vector2(0.5f, 0.5f), n);
    }

    Sprite MakeCircleSprite()
    {
        int n = 64;
        var tex = new Texture2D(n, n, TextureFormat.RGBA32, false);
        var px = new Color[n * n];
        for (int y = 0; y < n; y++)
        {
            for (int x = 0; x < n; x++)
            {
                float dx = (x + 0.5f) / n * 2f - 1f;
                float dy = (y + 0.5f) / n * 2f - 1f;
                float d = Mathf.Sqrt(dx * dx + dy * dy);
                px[y * n + x] = (d <= 1f) ? Color.white : new Color(1f, 1f, 1f, 0f);
            }
        }
        tex.SetPixels(px);
        tex.Apply();
        return Sprite.Create(tex, new Rect(0, 0, n, n), new Vector2(0.5f, 0.5f), n);
    }

    // ───────── 顶部 HUD ─────────
    void OnGUI()
    {
        if (!BattleActive) return;   // 只在对局中显示 HUD
        if (hudStyle == null)
        {
            hudStyle = new GUIStyle(GUI.skin.label);
            hudStyle.fontSize = 16;
            hudStyle.normal.textColor = new Color(0.10f, 0.11f, 0.11f);
        }
        BattleStatePush st = ClientNet.LatestState;
        string text = st == null
            ? "等待战斗状态..."
            : "基地 HP " + st.BaseHp + "　费用 " + st.Cost + "　帧 " + st.Frame +
              "　敌人 " + st.Enemies.Count + "　干员 " + st.Operators.Count;
        GUI.Label(new Rect(14f, 12f, 760f, 26f), text, hudStyle);
    }
}
