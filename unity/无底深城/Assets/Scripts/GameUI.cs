// 客户端界面流程：登录 → 主界面 → 选择关卡 → 战斗；另有注册页、设置层、结算页与顶部提示。
// 全部界面用 uGUI 代码生成：场景里只需要新建一个空物体挂上本脚本，不用搭 Canvas / 预制体。
// 依赖 ClientNet（连接与协议）、BattleRenderer（战斗渲染，由本脚本开关）。
using System.Collections;
using UnityEngine;
using UnityEngine.UI;
using UnityEngine.EventSystems;
using UnityEngine.Events;
using Api;

public class GameUI : MonoBehaviour
{
    enum Page { Login, Register, Main, LevelSelect, Battle, Settle }

    Canvas canvas;
    Font uiFont;

    GameObject pageLogin, pageRegister, pageMain, pageLevel, pageBattle, pageSettle, settingsMask, toastGo;
    Text toastText, loginHint, mainInfo, settleTitle, settleDetail, settingsInfo;

    InputField loginAcc, loginPwd, regAcc, regPwd, regPwd2;

    ClientNet net;
    BattleRenderer views;
    bool bound;
    Coroutine toastCo;

    // ───────────── 生命周期 ─────────────
    void Awake()
    {
        BuildCanvas();
        BuildLoginPage();
        BuildRegisterPage();
        BuildMainPage();
        BuildLevelPage();
        BuildBattleBar();
        BuildSettlePage();
        BuildSettings();
        BuildToast();
        SetPage(Page.Login);
    }

    void Start()
    {
        // 战斗渲染默认关闭，进房成功后再打开
        views = FindObjectOfType<BattleRenderer>(true);
        // 只关渲染开关，不关物体：BattleRenderer 可能与 Main Camera 同物体，SetActive(false) 会连相机一起关掉
        if (views != null) views.BattleActive = false;
        StartCoroutine(BindNetNextFrame());
    }

    IEnumerator BindNetNextFrame()
    {
        yield return null; // 等 ClientNet.Start 跑完再取单例
        net = ClientNet.Instance != null ? ClientNet.Instance : FindObjectOfType<ClientNet>();
        if (net == null)
        {
            ShowToast("场景里缺少 ClientNet，无法连接服务器");
            yield break;
        }
        net.OnRegisterResp += HandleRegister;
        net.OnLoginResp += HandleLogin;
        net.OnEnterRoomResp += HandleEnterRoom;
        net.OnSettle += HandleSettle;
        net.OnError += ShowToast;
        net.OnDisconnected += HandleDisconnected;
        bound = true;
        Debug.Log("[UI] 已绑定 ClientNet");
    }

    // ───────────── 事件回调 ─────────────
    void HandleRegister(RegisterResp r)
    {
        if (r.Code == 0) { ShowToast("注册成功，正在登录…"); net.Login(regAcc.text.Trim(), regPwd.text); }
        else if (r.Code == 2) { ShowToast("账号已存在，直接登录…"); net.Login(regAcc.text.Trim(), regPwd.text); }
        else ShowToast("注册失败：" + r.Msg + "（code=" + r.Code + "）");
    }

    void HandleLogin(LoginResp r)
    {
        if (r.Code != 0)
        {
            if (loginHint != null) loginHint.text = "";
            ShowToast("登录失败：" + r.Msg + "（code=" + r.Code + "）");
            return;
        }
        if (loginHint != null) loginHint.text = "";
        if (mainInfo != null)
            mainInfo.text = "账号 " + net.Account + "　UID " + net.Uid +
                            "\n等级 " + net.Level + "　体力 " + net.Stamina +
                            "\n经验 " + net.Exp + "　金币 " + net.Gold;
        SetPage(Page.Main);
        ShowToast("登录成功");
    }

    void HandleEnterRoom(EnterRoomResp r)
    {
        if (r.Code != 0)
        {
            ShowToast("进入关卡失败：" + r.Msg + "（code=" + r.Code + "）");
            SetPage(Page.LevelSelect);
            return;
        }
        if (views != null) views.BattleActive = true;
        SetPage(Page.Battle);
        ShowToast("对局开始，房间号 " + r.RoomId);
    }

    void HandleSettle(BattleSettlePush s)
    {
        if (views != null) views.BattleActive = false;
        if (settleTitle != null)
            settleTitle.text = s.Result == 0 ? "对局胜利" : (s.Result == 1 ? "对局失败" : "已撤退");
        if (settleDetail != null)
        {
            string reward = s.Rewards.Count > 0 ? "奖励：" + string.Join("、", s.Rewards) : "奖励：无";
            settleDetail.text = "得分 " + s.Score + "　用时 " + s.DurationSec + " 秒\n" + reward;
        }
        SetPage(Page.Settle);
    }

    void HandleDisconnected()
    {
        ShowToast("与服务器断开连接，请重新登录");
        if (views != null) views.BattleActive = false;
        if (bound) SetPage(Page.Login);
    }

    // ───────────── 页面切换 ─────────────
    void SetPage(Page p)
    {
        if (pageLogin != null) pageLogin.SetActive(p == Page.Login);
        if (pageRegister != null) pageRegister.SetActive(p == Page.Register);
        if (pageMain != null) pageMain.SetActive(p == Page.Main);
        if (pageLevel != null) pageLevel.SetActive(p == Page.LevelSelect);
        if (pageBattle != null) pageBattle.SetActive(p == Page.Battle);
        if (pageSettle != null) pageSettle.SetActive(p == Page.Settle);
        if (settingsMask != null) settingsMask.SetActive(false);

        if (p != Page.Battle && views != null) views.BattleActive = false;
    }

    // ───────────── 按钮行为 ─────────────
    void OnClickLogin()
    {
        if (!bound) { ShowToast("网络层还没准备好"); return; }
        string acc = loginAcc.text.Trim();
        string pwd = loginPwd.text;
        if (acc.Length == 0) { ShowToast("请输入账号"); return; }
        if (pwd.Length == 0) pwd = "123456";
        if (!net.Connected && !net.Connect()) { ShowToast("连接服务器失败，请确认服务端已启动"); return; }
        loginHint.text = "正在登录…";
        net.Login(acc, pwd);
    }

    void OnClickRegister()
    {
        if (!bound) { ShowToast("网络层还没准备好"); return; }
        string a = regAcc.text.Trim();
        string p1 = regPwd.text;
        string p2 = regPwd2.text;
        if (a.Length == 0) { ShowToast("请输入账号"); return; }
        if (p1.Length == 0) { ShowToast("请输入密码"); return; }
        if (p1 != p2) { ShowToast("两次输入的密码不一致"); return; }
        if (!net.Connected && !net.Connect()) { ShowToast("连接服务器失败，请确认服务端已启动"); return; }
        net.Register(a, p1);
    }

    void StartLevel(uint levelId)
    {
        if (!bound) { ShowToast("网络层还没准备好"); return; }
        if (!net.LoggedIn) { ShowToast("请先登录"); SetPage(Page.Login); return; }
        ShowToast("正在进入关卡 " + levelId + "…");
        net.EnterRoom(levelId);
    }

    void OnClickLeaveBattle()
    {
        if (views != null) views.BattleActive = false;
        ShowToast("已离开对局界面（服务端该局仍会跑完，演示用）");
        SetPage(Page.Main);
    }

    void OnClickQuit()
    {
#if UNITY_EDITOR
        UnityEditor.EditorApplication.isPlaying = false;
#else
        Application.Quit();
#endif
    }

    // ───────────── 界面搭建 ─────────────
    void BuildCanvas()
    {
        uiFont = LoadFont();

        if (FindObjectOfType<EventSystem>() == null)
        {
            var es = new GameObject("EventSystem");
            es.AddComponent<EventSystem>();
            es.AddComponent<StandaloneInputModule>();
        }

        var go = new GameObject("GameUI_Canvas");
        go.transform.SetParent(transform, false);
        canvas = go.AddComponent<Canvas>();
        canvas.renderMode = RenderMode.ScreenSpaceOverlay;
        canvas.sortingOrder = 20;
        var scaler = go.AddComponent<CanvasScaler>();
        scaler.uiScaleMode = CanvasScaler.ScaleMode.ScaleWithScreenSize;
        scaler.referenceResolution = new Vector2(1280f, 720f);
        scaler.matchWidthOrHeight = 0.5f;
        go.AddComponent<GraphicRaycaster>();
    }

    Font LoadFont()
    {
        try
        {
            var f = Font.CreateDynamicFontFromOSFont(
                new string[] { "Microsoft YaHei", "微软雅黑", "SimHei", "Arial" }, 24);
            if (f != null) return f;
        }
        catch { }
        try { return Resources.GetBuiltinResource<Font>("LegacyRuntime.ttf"); } catch { }
        return null;
    }

    // ───────── 登录页 ─────────
    void BuildLoginPage()
    {
        pageLogin = NewPage("Page_Login", new Color(0.93f, 0.96f, 0.98f));

        NewText(pageLogin.transform, "无底深城", 44, new Color(0.13f, 0.20f, 0.26f),
            new Vector2(0f, 210f), new Vector2(600f, 62f));
        NewText(pageLogin.transform, "分布式塔防 · 演示客户端", 16, new Color(0.42f, 0.47f, 0.52f),
            new Vector2(0f, 162f), new Vector2(600f, 28f));

        loginAcc = NewInput(pageLogin.transform, "账号（例如 test001）", new Vector2(0f, 70f), new Vector2(360f, 50f), false);
        loginPwd = NewInput(pageLogin.transform, "密码（留空按 123456）", new Vector2(0f, 8f), new Vector2(360f, 50f), true);

        NewButton(pageLogin.transform, "登 录", new Vector2(-92f, -70f), new Vector2(172f, 52f),
            new Color(0.30f, 0.60f, 0.76f), OnClickLogin);
        NewButton(pageLogin.transform, "注 册", new Vector2(92f, -70f), new Vector2(172f, 52f),
            new Color(0.45f, 0.55f, 0.62f), () => SetPage(Page.Register));
        NewButton(pageLogin.transform, "退 出", new Vector2(0f, -145f), new Vector2(360f, 44f),
            new Color(0.72f, 0.42f, 0.42f), OnClickQuit);

        loginHint = NewText(pageLogin.transform, "", 14, new Color(0.42f, 0.47f, 0.52f),
            new Vector2(0f, -192f), new Vector2(700f, 24f));
    }

    // ───────── 注册页 ─────────
    void BuildRegisterPage()
    {
        pageRegister = NewPage("Page_Register", new Color(0.93f, 0.96f, 0.98f));

        NewText(pageRegister.transform, "注册账号", 36, new Color(0.13f, 0.20f, 0.26f),
            new Vector2(0f, 200f), new Vector2(600f, 56f));

        regAcc = NewInput(pageRegister.transform, "账号", new Vector2(0f, 90f), new Vector2(360f, 50f), false);
        regPwd = NewInput(pageRegister.transform, "密码", new Vector2(0f, 28f), new Vector2(360f, 50f), true);
        regPwd2 = NewInput(pageRegister.transform, "确认密码", new Vector2(0f, -34f), new Vector2(360f, 50f), true);

        NewButton(pageRegister.transform, "确认注册", new Vector2(-92f, -115f), new Vector2(172f, 52f),
            new Color(0.30f, 0.60f, 0.76f), OnClickRegister);
        NewButton(pageRegister.transform, "返 回", new Vector2(92f, -115f), new Vector2(172f, 52f),
            new Color(0.45f, 0.55f, 0.62f), () => SetPage(Page.Login));
    }

    // ───────── 主界面 ─────────
    void BuildMainPage()
    {
        pageMain = NewPage("Page_Main", new Color(0.93f, 0.96f, 0.98f));

        NewText(pageMain.transform, "无底深城", 40, new Color(0.13f, 0.20f, 0.26f),
            new Vector2(0f, 225f), new Vector2(600f, 56f));
        mainInfo = NewText(pageMain.transform, "未登录", 15, new Color(0.42f, 0.47f, 0.52f),
            new Vector2(0f, 150f), new Vector2(700f, 88f));

        NewButton(pageMain.transform, "开始游戏", new Vector2(0f, 25f), new Vector2(320f, 58f),
            new Color(0.30f, 0.60f, 0.76f), () => SetPage(Page.LevelSelect));
        NewButton(pageMain.transform, "设 置", new Vector2(0f, -52f), new Vector2(320f, 50f),
            new Color(0.45f, 0.55f, 0.62f), () => { settingsMask.SetActive(true); RefreshSettings(); });
        NewButton(pageMain.transform, "退出游戏", new Vector2(0f, -125f), new Vector2(320f, 44f),
            new Color(0.72f, 0.42f, 0.42f), OnClickQuit);
    }

    // ───────── 选关页 ─────────
    void BuildLevelPage()
    {
        pageLevel = NewPage("Page_Level", new Color(0.93f, 0.96f, 0.98f));

        NewText(pageLevel.transform, "选择关卡", 36, new Color(0.13f, 0.20f, 0.26f),
            new Vector2(0f, 205f), new Vector2(600f, 56f));

        NewButton(pageLevel.transform, "关卡 1 · 新手试炼", new Vector2(0f, 80f), new Vector2(440f, 64f),
            new Color(0.30f, 0.60f, 0.76f), () => StartLevel(1));
        NewText(pageLevel.transform, "9×2 地图　基础波次　进入对局会消耗体力", 13, new Color(0.45f, 0.50f, 0.55f),
            new Vector2(0f, 40f), new Vector2(440f, 20f));

        NewButton(pageLevel.transform, "关卡 2 · 深渊回响", new Vector2(0f, -20f), new Vector2(440f, 64f),
            new Color(0.36f, 0.50f, 0.70f), () => StartLevel(2));
        NewText(pageLevel.transform, "9×2 地图　多波次　进入对局会消耗体力", 13, new Color(0.45f, 0.50f, 0.55f),
            new Vector2(0f, -60f), new Vector2(440f, 20f));

        NewButton(pageLevel.transform, "退 出", new Vector2(0f, -145f), new Vector2(440f, 44f),
            new Color(0.55f, 0.58f, 0.62f), () => SetPage(Page.Main));
    }

    // ───────── 战斗页（只留顶部操作条，画面由 BattleRenderer 画） ─────────
    void BuildBattleBar()
    {
        pageBattle = new GameObject("Page_Battle", typeof(RectTransform));
        pageBattle.transform.SetParent(canvas.transform, false);
        var rt = pageBattle.GetComponent<RectTransform>();
        rt.anchorMin = Vector2.zero; rt.anchorMax = Vector2.one;
        rt.offsetMin = Vector2.zero; rt.offsetMax = Vector2.zero;

        var top = new GameObject("TopBar", typeof(RectTransform));
        top.transform.SetParent(pageBattle.transform, false);
        var trt = top.GetComponent<RectTransform>();
        trt.anchorMin = new Vector2(0f, 1f); trt.anchorMax = new Vector2(1f, 1f);
        trt.pivot = new Vector2(0.5f, 1f);
        trt.anchoredPosition = Vector2.zero;
        trt.sizeDelta = new Vector2(0f, 54f);
        var bg = top.AddComponent<Image>();
        bg.color = new Color(0.10f, 0.13f, 0.16f, 0.55f);
        bg.raycastTarget = false;

        var hint = NewText(top.transform, "点第 1 行空格部署　点干员撤回　Q 技能　R 断线重连", 14,
            new Color(0.90f, 0.93f, 0.96f), Vector2.zero, new Vector2(700f, 26f), TextAnchor.MiddleLeft);
        var hrt = hint.GetComponent<RectTransform>();
        hrt.anchorMin = new Vector2(0f, 0.5f); hrt.anchorMax = new Vector2(0f, 0.5f);
        hrt.pivot = new Vector2(0f, 0.5f);
        hrt.anchoredPosition = new Vector2(18f, 0f);

        var btn = NewButton(top.transform, "退出战斗", Vector2.zero, new Vector2(180f, 38f),
            new Color(0.72f, 0.42f, 0.42f), OnClickLeaveBattle);
        var brt = btn.GetComponent<RectTransform>();
        brt.anchorMin = new Vector2(1f, 0.5f); brt.anchorMax = new Vector2(1f, 0.5f);
        brt.pivot = new Vector2(1f, 0.5f);
        brt.anchoredPosition = new Vector2(-18f, 0f);
    }

    // ───────── 结算页 ─────────
    void BuildSettlePage()
    {
        pageSettle = NewPage("Page_Settle", new Color(0.10f, 0.13f, 0.16f, 0.86f));

        settleTitle = NewText(pageSettle.transform, "对局结束", 44, Color.white,
            new Vector2(0f, 110f), new Vector2(600f, 70f));
        settleDetail = NewText(pageSettle.transform, "", 18, new Color(0.80f, 0.85f, 0.90f),
            new Vector2(0f, 15f), new Vector2(720f, 110f));
        NewButton(pageSettle.transform, "返回主界面", new Vector2(0f, -95f), new Vector2(300f, 56f),
            new Color(0.30f, 0.60f, 0.76f), () => SetPage(Page.Main));
    }

    // ───────── 设置层 ─────────
    void BuildSettings()
    {
        settingsMask = NewPage("SettingsMask", new Color(0.08f, 0.10f, 0.12f, 0.78f));

        var panel = new GameObject("Panel", typeof(RectTransform));
        panel.transform.SetParent(settingsMask.transform, false);
        var prt = panel.GetComponent<RectTransform>();
        prt.anchorMin = prt.anchorMax = new Vector2(0.5f, 0.5f);
        prt.sizeDelta = new Vector2(560f, 380f);
        var pbg = panel.AddComponent<Image>();
        pbg.color = new Color(0.97f, 0.98f, 1f);

        NewText(panel.transform, "设置", 28, new Color(0.13f, 0.20f, 0.26f),
            new Vector2(0f, 140f), new Vector2(400f, 40f));
        settingsInfo = NewText(panel.transform, "", 15, new Color(0.30f, 0.35f, 0.40f),
            new Vector2(0f, 25f), new Vector2(480f, 150f));
        NewButton(panel.transform, "关 闭", new Vector2(0f, -140f), new Vector2(200f, 48f),
            new Color(0.45f, 0.55f, 0.62f), () => settingsMask.SetActive(false));

        settingsMask.SetActive(false);
    }

    void RefreshSettings()
    {
        if (settingsInfo == null) return;
        settingsInfo.text =
            "服务器：" + (net != null ? net.ServerHost + ":" + net.ServerPort : "未绑定") +
            "\n连接状态：" + (net != null && net.Connected ? "已连接" : "未连接") +
            "\n当前账号：" + (net != null && net.Account.Length > 0 ? net.Account : "未登录") +
            "\n分辨率：" + Screen.width + "×" + Screen.height +
            "\n音画设置：演示版未接音频，预留";
    }

    // ───────── Toast ─────────
    void BuildToast()
    {
        toastGo = new GameObject("Toast", typeof(RectTransform));
        toastGo.transform.SetParent(canvas.transform, false);
        var rt = toastGo.GetComponent<RectTransform>();
        rt.anchorMin = new Vector2(0.5f, 1f); rt.anchorMax = new Vector2(0.5f, 1f);
        rt.pivot = new Vector2(0.5f, 1f);
        rt.anchoredPosition = new Vector2(0f, -78f);
        rt.sizeDelta = new Vector2(720f, 46f);

        var bg = toastGo.AddComponent<Image>();
        bg.color = new Color(0.10f, 0.13f, 0.16f, 0.90f);

        toastText = NewText(toastGo.transform, "", 16, Color.white, Vector2.zero, new Vector2(690f, 32f));
        toastGo.SetActive(false);
    }

    void ShowToast(string msg)
    {
        if (toastText == null) return;
        toastText.text = msg;
        toastGo.SetActive(true);
        if (toastCo != null) StopCoroutine(toastCo);
        toastCo = StartCoroutine(HideToast(2.4f));
    }

    IEnumerator HideToast(float sec)
    {
        yield return new WaitForSeconds(sec);
        if (toastGo != null) toastGo.SetActive(false);
    }

    // ───────── 控件工厂 ─────────
    GameObject NewPage(string name, Color bg)
    {
        var go = new GameObject(name, typeof(RectTransform));
        go.transform.SetParent(canvas.transform, false);
        var rt = go.GetComponent<RectTransform>();
        rt.anchorMin = Vector2.zero; rt.anchorMax = Vector2.one;
        rt.offsetMin = Vector2.zero; rt.offsetMax = Vector2.zero;
        var img = go.AddComponent<Image>();
        img.color = bg;
        return go;
    }

    Text NewText(Transform parent, string text, int size, Color color,
                 Vector2 pos, Vector2 box, TextAnchor anchor = TextAnchor.MiddleCenter)
    {
        var go = new GameObject("Text", typeof(RectTransform));
        go.transform.SetParent(parent, false);
        var rt = go.GetComponent<RectTransform>();
        rt.anchorMin = rt.anchorMax = new Vector2(0.5f, 0.5f);
        rt.anchoredPosition = pos;
        rt.sizeDelta = box;

        var t = go.AddComponent<Text>();
        t.font = uiFont;
        t.fontSize = size;
        t.color = color;
        t.alignment = anchor;
        t.horizontalOverflow = HorizontalWrapMode.Wrap;
        t.verticalOverflow = VerticalWrapMode.Overflow;
        t.raycastTarget = false;
        t.text = text;
        return t;
    }

    Button NewButton(Transform parent, string label, Vector2 pos, Vector2 size, Color bg, UnityAction onClick)
    {
        var go = new GameObject("Btn_" + label, typeof(RectTransform));
        go.transform.SetParent(parent, false);
        var rt = go.GetComponent<RectTransform>();
        rt.anchorMin = rt.anchorMax = new Vector2(0.5f, 0.5f);
        rt.anchoredPosition = pos;
        rt.sizeDelta = size;

        var img = go.AddComponent<Image>();
        img.color = bg;

        var btn = go.AddComponent<Button>();
        btn.targetGraphic = img;
        var cb = btn.colors;
        cb.highlightedColor = new Color(1f, 1f, 1f, 1f);
        cb.pressedColor = new Color(0.88f, 0.88f, 0.88f, 1f);
        cb.selectedColor = new Color(1f, 1f, 1f, 1f);
        btn.colors = cb;
        if (onClick != null) btn.onClick.AddListener(onClick);

        NewText(go.transform, label, 20, Color.white, Vector2.zero, size, TextAnchor.MiddleCenter);
        return btn;
    }

    InputField NewInput(Transform parent, string placeholder, Vector2 pos, Vector2 size, bool password)
    {
        var go = new GameObject("Input_" + placeholder, typeof(RectTransform));
        go.transform.SetParent(parent, false);
        var rt = go.GetComponent<RectTransform>();
        rt.anchorMin = rt.anchorMax = new Vector2(0.5f, 0.5f);
        rt.anchoredPosition = pos;
        rt.sizeDelta = size;

        var img = go.AddComponent<Image>();
        img.color = Color.white;

        var tgo = new GameObject("Text", typeof(RectTransform));
        tgo.transform.SetParent(go.transform, false);
        var trt = tgo.GetComponent<RectTransform>();
        trt.anchorMin = Vector2.zero; trt.anchorMax = Vector2.one;
        trt.offsetMin = new Vector2(14f, 4f); trt.offsetMax = new Vector2(-14f, -4f);
        var txt = tgo.AddComponent<Text>();
        txt.font = uiFont; txt.fontSize = 18;
        txt.color = new Color(0.12f, 0.13f, 0.14f);
        txt.alignment = TextAnchor.MiddleLeft;
        txt.supportRichText = false;
        txt.raycastTarget = false;

        var pgo = new GameObject("Placeholder", typeof(RectTransform));
        pgo.transform.SetParent(go.transform, false);
        var prt = pgo.GetComponent<RectTransform>();
        prt.anchorMin = Vector2.zero; prt.anchorMax = Vector2.one;
        prt.offsetMin = new Vector2(14f, 4f); prt.offsetMax = new Vector2(-14f, -4f);
        var ph = pgo.AddComponent<Text>();
        ph.font = uiFont; ph.fontSize = 16;
        ph.color = new Color(0.55f, 0.58f, 0.62f);
        ph.alignment = TextAnchor.MiddleLeft;
        ph.text = placeholder;
        ph.raycastTarget = false;

        var input = go.AddComponent<InputField>();
        input.textComponent = txt;
        input.placeholder = ph;
        input.targetGraphic = img;
        input.lineType = InputField.LineType.SingleLine;
        if (password) input.contentType = InputField.ContentType.Password;
        return input;
    }
}
