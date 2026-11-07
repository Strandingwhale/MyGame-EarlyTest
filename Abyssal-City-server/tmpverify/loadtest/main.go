// 压测客户端：模拟 N 个真实客户端同时在线（连接 → 注册 → 登录 → 心跳保活），
// 其中 games 个客户端完整打一局（进房 → 收首帧 → 部署 → 收状态推送 → 收结算）。
//
// 采集指标：
//   - 连接成功率、各请求-回包对的 RTT（p50/p95/p99/max）、消息吞吐、错误率
//   - 本进程与被 watch 的服务器进程（-watch name=pid,...）的内存/CPU 峰值
//
// 用法：
//
//	go run ./tmpverify/loadtest -conns 200 -games 30 \
//	  -watch "battle=1234,gateway=5678,login=9012,datasave=3456" -out result.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
)

// 消息号（与 api/base.proto 对应）
const (
	msgPing       uint16 = 0
	msgPingR      uint16 = 1
	msgLogin      uint16 = 1001
	msgLoginR     uint16 = 1002
	msgRegister   uint16 = 1003
	msgRegisterR  uint16 = 1004
	msgEnterRoom  uint16 = 2000
	msgEnterRoomR uint16 = 2001
	msgDeploy     uint16 = 2002
	msgDeployR    uint16 = 2003
	msgState      uint16 = 2004
	msgSettle     uint16 = 2005
)

var (
	gwAddr      = flag.String("gw", "127.0.0.1:8888", "网关地址")
	numConns    = flag.Int("conns", 200, "并发在线连接数")
	numGames    = flag.Int("games", 30, "同时开打的局数（取前 games 个登录成功的连接）")
	levelID     = flag.Uint("level", 1, "关卡 ID")
	gameTimeout = flag.Duration("game-timeout", 120*time.Second, "单局等待结算的最长时间")
	opTimeout   = flag.Duration("op-timeout", 90*time.Second, "注册/登录等单请求的最长等待时间")
	stagger     = flag.Duration("stagger", 5*time.Millisecond, "连接启动间隔（缓启动，避免瞬时 SYN 风暴）")
	watch       = flag.String("watch", "", "采样的服务器进程，格式 name=pid,name=pid（Windows）")
	outPath     = flag.String("out", "", "结果 JSON 输出路径（可选）")
)

func main() {
	flag.Parse()
	if *numGames > *numConns {
		fmt.Println("-games 不能大于 -conns")
		os.Exit(1)
	}

	m := newMetrics()
	t0 := time.Now()

	// 进程资源采样：自身 + -watch 指定的服务器进程
	sampler := newProcSampler(*watch)
	sampler.start()
	defer sampler.stop()

	fmt.Printf("压测开始：网关=%s 连接=%d 局数=%d\n", *gwAddr, *numConns, *numGames)

	endCh := make(chan struct{})      // 关闭以通知不打局的连接退出
	var wg sync.WaitGroup
	var gamerWG sync.WaitGroup

	for i := 0; i < *numConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			time.Sleep(time.Duration(id) * *stagger) // 缓启动
			c := newClient(id, m)
			if err := c.run(id < *numGames, &gamerWG, endCh); err != nil {
				m.errf("client[%d] %v", id, err)
			}
		}(i)
	}

	// 等所有打局的连接打完（或超时），再放行挂机连接退出
	gamerWG.Wait()
	m.gamePhaseEnd = time.Now()
	close(endCh)
	wg.Wait()
	m.end = time.Now()

	sampler.stop()
	printReport(m, sampler, t0)
	if *outPath != "" {
		if err := writeJSON(m, sampler, t0, *outPath); err != nil {
			fmt.Println("写 JSON 失败:", err)
		} else {
			fmt.Println("结果已写入:", *outPath)
		}
	}
}

// ---------------- 客户端 ----------------

type client struct {
	id   int
	conn net.Conn
	m    *metrics

	buf    bytes.Buffer
	sendMu sync.Mutex

	// 当前在途请求（每个客户端严格串行：发一条等一条回包）
	pendMu  sync.Mutex
	pendMsg uint16
	pendOp  string
	pendAt  time.Time
	respCh  chan *common.Packet

	pingMu sync.Mutex
	pingAt time.Time

	uid     uint64
	roomID  uint32
	gaming  atomic.Bool
	settled chan *api.BattleSettlePush

	firstPush     chan struct{}
	firstPushOnce sync.Once
	done          chan struct{}
	closeOnce     sync.Once
}

func newClient(id int, m *metrics) *client {
	return &client{
		id:        id,
		m:         m,
		respCh:    make(chan *common.Packet, 4),
		settled:   make(chan *api.BattleSettlePush, 1),
		firstPush: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

func (c *client) close() {
	c.closeOnce.Do(func() {
		if c.conn != nil {
			_ = c.conn.Close()
		}
		close(c.done)
	})
}

// run 单个连接的完整生命周期：连接 → 注册 → 登录 →（打局 | 挂机到结束）
func (c *client) run(gamer bool, gamerWG *sync.WaitGroup, endCh chan struct{}) (err error) {
	defer func() {
		if gamer {
			gamerWG.Done()
		}
		c.close()
	}()
	if gamer {
		gamerWG.Add(1)
	}

	// 1. TCP 连接
	if err := c.dial(); err != nil {
		c.m.connFail.Add(1)
		return err
	}
	c.m.connOK.Add(1)
	go c.readLoop()
	go c.pingLoop(endCh)

	// 2. 注册 + 登录（每个连接独立账号，完整走真实链路）
	account := fmt.Sprintf("lt_%d_%d", time.Now().UnixNano(), c.id)
	password := "LoadTest#1"
	if err := c.register(account, password); err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}
	if err := c.login(account, password); err != nil {
		return fmt.Errorf("登录失败: %w", err)
	}

	if !gamer {
		<-endCh // 挂机连接：保持在线心跳，直到打局全部结束
		return nil
	}

	// 3. 进房（记录对局开始，用于战斗阶段吞吐统计）
	c.m.markGameStart()
	roomID, err := c.enterRoom()
	if err != nil {
		return fmt.Errorf("进房失败: %w", err)
	}
	c.roomID = roomID
	c.gaming.Store(true)

	// 4. 等首帧状态推送（确认房间 tick 已起来），再部署干员
	select {
	case <-c.firstPush:
	case <-time.After(20 * time.Second):
		return fmt.Errorf("等首帧状态推送超时")
	}
	if err := c.deploy(); err != nil {
		return fmt.Errorf("部署失败: %w", err)
	}

	// 5. 收状态推送直到结算
	select {
	case s := <-c.settled:
		c.m.settleOK.Add(1)
		if s.Result == 0 {
			c.m.settleWin.Add(1)
		} else {
			c.m.settleLose.Add(1)
		}
		return nil
	case <-time.After(*gameTimeout):
		return fmt.Errorf("等结算超时")
	}
}

func (c *client) dial() error {
	conn, err := net.DialTimeout("tcp", *gwAddr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("连接网关失败: %w", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	c.conn = conn
	return nil
}

// send 编码并发送一个包，统计发出吞吐
func (c *client) send(msgID uint16, body []byte) error {
	pkt, err := common.Encode(msgID, 0, body)
	if err != nil {
		return err
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	n, err := c.conn.Write(pkt)
	c.m.msgOut.Add(1)
	c.m.bytesOut.Add(int64(n))
	return err
}

// request 记录一次请求的发送时刻，供回包到达时计算 RTT
func (c *client) request(expectResp uint16, op string, msgID uint16, body []byte) error {
	c.pendMu.Lock()
	c.pendMsg, c.pendOp, c.pendAt = expectResp, op, time.Now()
	c.pendMu.Unlock()
	c.m.opSent(op)
	return c.send(msgID, body)
}

// waitResp 等待当前在途请求的回包，到达时由 readLoop 已记录 RTT
func (c *client) waitResp(expect uint16, timeout time.Duration) (*common.Packet, error) {
	deadline := time.After(timeout)
	for {
		select {
		case p := <-c.respCh:
			if p.MessageID == expect {
				return p, nil
			}
		case <-c.done:
			return nil, fmt.Errorf("连接已关闭")
		case <-deadline:
			return nil, fmt.Errorf("等待 msgId=%d 回包超时", expect)
		}
	}
}

func (c *client) register(account, password string) error {
	body, _ := proto.Marshal(&api.RegisterReq{Account: account, Password: password})
	if err := c.request(msgRegisterR, "register", msgRegister, body); err != nil {
		c.m.opFail("register")
		return err
	}
	p, err := c.waitResp(msgRegisterR, *opTimeout)
	if err != nil {
		c.m.opFail("register")
		return err
	}
	var resp api.RegisterResp
	if err := proto.Unmarshal(p.Payload, &resp); err != nil {
		c.m.opFail("register")
		return err
	}
	if resp.Code != 0 {
		c.m.opFail("register")
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	c.m.opOK("register")
	return nil
}

func (c *client) login(account, password string) error {
	body, _ := proto.Marshal(&api.LoginReq{Account: account, Password: password})
	if err := c.request(msgLoginR, "login", msgLogin, body); err != nil {
		c.m.opFail("login")
		return err
	}
	p, err := c.waitResp(msgLoginR, *opTimeout)
	if err != nil {
		c.m.opFail("login")
		return err
	}
	var resp api.LoginResp
	if err := proto.Unmarshal(p.Payload, &resp); err != nil {
		c.m.opFail("login")
		return err
	}
	if resp.Code != 0 || resp.Uid == 0 {
		c.m.opFail("login")
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	c.uid = uint64(resp.Uid)
	c.m.opOK("login")
	return nil
}

func (c *client) enterRoom() (uint32, error) {
	body, _ := proto.Marshal(&api.EnterRoomReq{LevelId: uint32(*levelID), Uid: c.uid})
	if err := c.request(msgEnterRoomR, "enter", msgEnterRoom, body); err != nil {
		c.m.opFail("enter")
		return 0, err
	}
	p, err := c.waitResp(msgEnterRoomR, 20*time.Second)
	if err != nil {
		c.m.opFail("enter")
		return 0, err
	}
	var resp api.EnterRoomResp
	if err := proto.Unmarshal(p.Payload, &resp); err != nil {
		c.m.opFail("enter")
		return 0, err
	}
	if resp.Code != 0 {
		c.m.opFail("enter")
		return 0, fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	c.m.opOK("enter")
	return resp.RoomId, nil
}

func (c *client) deploy() error {
	body, _ := proto.Marshal(&api.DeployOperatorReq{
		RoomId: c.roomID, OperatorId: 1, GridX: 4, GridY: 0, Direction: 3,
	})
	if err := c.request(msgDeployR, "deploy", msgDeploy, body); err != nil {
		c.m.opFail("deploy")
		return err
	}
	p, err := c.waitResp(msgDeployR, 10*time.Second)
	if err != nil {
		c.m.opFail("deploy")
		return err
	}
	var resp api.DeployOperatorResp
	if err := proto.Unmarshal(p.Payload, &resp); err != nil {
		c.m.opFail("deploy")
		return err
	}
	if resp.Code != 0 {
		c.m.opFail("deploy")
		return fmt.Errorf("code=%d msg=%s", resp.Code, resp.Msg)
	}
	c.m.opOK("deploy")
	return nil
}

// readLoop 持续收包：回包计算 RTT 后投递，推送只计数
func (c *client) readLoop() {
	defer c.close()
	tmp := make([]byte, 8192)
	for {
		n, err := c.conn.Read(tmp)
		if err != nil {
			return
		}
		c.m.bytesIn.Add(int64(n))
		c.buf.Write(tmp[:n])
		for {
			p, err := common.Decode(&c.buf)
			if err != nil || p == nil {
				break
			}
			c.m.msgIn.Add(1)
			c.dispatch(p)
		}
		if err != nil {
			return
		}
	}
}

func (c *client) dispatch(p *common.Packet) {
	switch p.MessageID {
	case msgPingR: // 心跳回包
		c.pingMu.Lock()
		if !c.pingAt.IsZero() {
			c.m.lat.add("ping", time.Since(c.pingAt))
			c.pingAt = time.Time{}
		}
		c.pingMu.Unlock()

	case msgRegisterR, msgLoginR, msgEnterRoomR, msgDeployR: // 请求-回包配对，算 RTT
		c.pendMu.Lock()
		if p.MessageID == c.pendMsg && !c.pendAt.IsZero() {
			c.m.lat.add(c.pendOp, time.Since(c.pendAt))
			c.pendAt = time.Time{}
		}
		c.pendMu.Unlock()
		select {
		case c.respCh <- p:
		default:
		}

	case msgState: // 战斗状态推送：计数 + 标记首帧
		c.m.statePush.Add(1)
		c.firstPushOnce.Do(func() { close(c.firstPush) })

	case msgSettle: // 结算推送
		var s api.BattleSettlePush
		if err := proto.Unmarshal(p.Payload, &s); err == nil {
			select {
			case c.settled <- &s:
			default:
			}
		}
	}
}

// pingLoop 每 5 秒发一次心跳，防止网关 10 秒超时断连；挂机连接靠 endCh 退出
func (c *client) pingLoop(endCh chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-endCh:
			return
		case <-ticker.C:
			body, _ := proto.Marshal(&api.PingReq{Timestamp: time.Now().UnixNano()})
			c.pingMu.Lock()
			c.pingAt = time.Now()
			c.pingMu.Unlock()
			if err := c.send(msgPing, body); err != nil {
				return
			}
		}
	}
}

// ---------------- 指标 ----------------

type latencyBook struct {
	mu      sync.Mutex
	samples map[string][]float64 // 毫秒
}

func (l *latencyBook) add(op string, d time.Duration) {
	l.mu.Lock()
	l.samples[op] = append(l.samples[op], float64(d)/float64(time.Millisecond))
	l.mu.Unlock()
}

func (l *latencyBook) get(op string) []float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]float64, len(l.samples[op]))
	copy(out, l.samples[op])
	sort.Float64s(out)
	return out
}

// percentile 取排好序样本的分位数，q ∈ [0,1]
func percentile(s []float64, q float64) float64 {
	if len(s) == 0 {
		return 0
	}
	idx := int(q*float64(len(s)) + 0.5)
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

type metrics struct {
	connOK, connFail       atomic.Int64
	msgIn, msgOut          atomic.Int64
	bytesIn, bytesOut      atomic.Int64
	statePush              atomic.Int64
	settleOK               atomic.Int64
	settleWin, settleLose  atomic.Int64

	lat latencyBook

	opMu   sync.Mutex
	opCnt  map[string]*opCounter
	errCnt map[string]int64

	gameStartOnce sync.Once
	gameStart     time.Time
	gamePhaseEnd  time.Time
	end           time.Time
}

type opCounter struct{ sent, ok, fail int64 }

func newMetrics() *metrics {
	return &metrics{
		lat:    latencyBook{samples: map[string][]float64{}},
		opCnt:  map[string]*opCounter{},
		errCnt: map[string]int64{},
	}
}

func (m *metrics) counter(op string) *opCounter {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	c, ok := m.opCnt[op]
	if !ok {
		c = &opCounter{}
		m.opCnt[op] = c
	}
	return c
}

func (m *metrics) opSent(op string) { c := m.counter(op); atomic.AddInt64(&c.sent, 1) }
func (m *metrics) opOK(op string)   { c := m.counter(op); atomic.AddInt64(&c.ok, 1) }
func (m *metrics) opFail(op string) { c := m.counter(op); atomic.AddInt64(&c.fail, 1) }

func (m *metrics) errf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	m.opMu.Lock()
	if m.errCnt[msg] < 3 { // 每种错误只留前三条样本，刷屏的靠计数
		fmt.Println("[错误]", msg)
	}
	m.errCnt[msg]++
	m.opMu.Unlock()
}

func (m *metrics) markGameStart() {
	m.gameStartOnce.Do(func() { m.gameStart = time.Now() })
}

// percentileRow 一行分位数统计
type percentileRow struct {
	Op    string  `json:"op"`
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	P99   float64 `json:"p99_ms"`
	Max   float64 `json:"max_ms"`
}

func latencyRow(op string, s []float64) percentileRow {
	return percentileRow{
		Op: op, Count: len(s),
		P50: percentile(s, 0.50), P95: percentile(s, 0.95),
		P99: percentile(s, 0.99), Max: percentile(s, 1.0),
	}
}

func printReport(m *metrics, s *procSampler, t0 time.Time) {
	total := m.end.Sub(t0)
	gameDur := m.gamePhaseEnd.Sub(m.gameStart)

	fmt.Println()
	fmt.Println("================ 压测结果 ================")
	fmt.Printf("总耗时: %v | 战斗阶段: %v\n", total.Round(time.Millisecond), gameDur.Round(time.Millisecond))
	fmt.Printf("连接: 成功 %d / 失败 %d / 成功率 %.2f%%\n",
		m.connOK.Load(), m.connFail.Load(), pct(m.connOK.Load(), m.connOK.Load()+m.connFail.Load()))
	fmt.Printf("消息: 收 %d 条 (%.0f 条/秒) | 发 %d 条 (%.0f 条/秒)\n",
		m.msgIn.Load(), float64(m.msgIn.Load())/total.Seconds(),
		m.msgOut.Load(), float64(m.msgOut.Load())/total.Seconds())
	fmt.Printf("战斗阶段吞吐（推送为主）: %.0f 条/秒（状态推送 %d 条）\n",
		float64(m.statePush.Load())/gameDur.Seconds(), m.statePush.Load())
	fmt.Printf("结算: 完成 %d（胜 %d / 负 %d）\n", m.settleOK.Load(), m.settleWin.Load(), m.settleLose.Load())

	fmt.Println("--- RTT（毫秒）---")
	fmt.Printf("%-10s %6s %8s %8s %8s %8s\n", "操作", "样本", "p50", "p95", "p99", "max")
	var all []float64
	rows := map[string][]float64{}
	for _, op := range []string{"register", "login", "enter", "deploy", "ping"} {
		s2 := m.lat.get(op)
		rows[op] = s2
		r := latencyRow(op, s2)
		fmt.Printf("%-10s %6d %8.1f %8.1f %8.1f %8.1f\n", r.Op, r.Count, r.P50, r.P95, r.P99, r.Max)
	}
	// 汇总：注册登录为一次性认证开销，enter/deploy 为战斗交互路径
	battle := append(append([]float64{}, rows["enter"]...), rows["deploy"]...)
	sort.Float64s(battle)
	auth := append(append([]float64{}, rows["register"]...), rows["login"]...)
	sort.Float64s(auth)
	for _, s2 := range [][]float64{auth, battle} {
		all = append(all, s2...)
	}
	sort.Float64s(all)
	fmt.Println("--- 分组 ---")
	for _, g := range []struct {
		name string
		s    []float64
	}{{"战斗路径(enter+deploy)", battle}, {"认证(register+login)", auth}, {"全部请求", all}} {
		r := latencyRow(g.name, g.s)
		fmt.Printf("%-22s %6d %8.1f %8.1f %8.1f %8.1f\n", r.Op, r.Count, r.P50, r.P95, r.P99, r.Max)
	}

	// 错误率
	var sent, fail int64
	m.opMu.Lock()
	for _, c := range m.opCnt {
		sent += c.sent
		fail += c.fail
	}
	fmt.Println("--- 错误明细 ---")
	for e, n := range m.errCnt {
		fmt.Printf("  x%d %s\n", n, e)
	}
	m.opMu.Unlock()
	fmt.Printf("请求错误率: %.2f%% (%d/%d)\n", pct(fail, sent), fail, sent)

	// 进程资源
	fmt.Println("--- 进程资源（峰值）---")
	for _, w := range s.results() {
		fmt.Printf("%-10s pid=%-6d 内存峰值 %.1f MB | CPU 峰值 %.1f%%（整机）平均 %.1f%%\n",
			w.name, w.pid, float64(w.peakWS)/(1<<20), w.peakCPU, w.avgCPU())
	}
	fmt.Printf("loadtest 自身: goroutine 峰值 %d | Go 堆峰值 %.1f MB\n",
		s.peakGoroutine.Load(), float64(s.peakHeap.Load())/(1<<20))

	// 判定
	fmt.Println("--- 判定 ---")
	b95 := percentile(battle, 0.95)
	if b95 < 50 {
		fmt.Printf("战斗路径 p95=%.1fms < 50ms  ✅ 达标\n", b95)
	} else {
		fmt.Printf("战斗路径 p95=%.1fms ≥ 50ms  ❌ 未达标\n", b95)
	}
	fmt.Println("==========================================")
}

func pct(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total) * 100
}

// ---------------- JSON 输出 ----------------

type jsonReport struct {
	StartedAt        string             `json:"started_at"`
	TotalSec         float64            `json:"total_sec"`
	GamePhaseSec     float64            `json:"game_phase_sec"`
	Conns            int                `json:"conns"`
	Games            int                `json:"games"`
	ConnOK           int64              `json:"conn_ok"`
	ConnFail         int64              `json:"conn_fail"`
	MsgIn            int64              `json:"msg_in"`
	MsgOut           int64              `json:"msg_out"`
	MsgInPerSec      float64            `json:"msg_in_per_sec"`
	GamePushPerSec   float64            `json:"game_push_per_sec"`
	SettleOK         int64              `json:"settle_ok"`
	SettleWin        int64              `json:"settle_win"`
	SettleLose       int64              `json:"settle_lose"`
	Latency          []percentileRow    `json:"latency"`
	BattleP95Ms      float64            `json:"battle_p95_ms"`
	AllP95Ms         float64            `json:"all_p95_ms"`
	ErrRatePct       float64            `json:"err_rate_pct"`
	Errors           map[string]int64   `json:"errors"`
	Procs            []jsonProc         `json:"procs"`
	PeakGoroutine    int64              `json:"peak_goroutine"`
	PeakHeapMB       float64            `json:"peak_heap_mb"`
}

type jsonProc struct {
	Name       string  `json:"name"`
	PID        uint32  `json:"pid"`
	PeakMemMB  float64 `json:"peak_mem_mb"`
	PeakCPUPct float64 `json:"peak_cpu_pct"`
	AvgCPUPct  float64 `json:"avg_cpu_pct"`
}

func writeJSON(m *metrics, s *procSampler, t0 time.Time, path string) error {
	total := m.end.Sub(t0)
	gameDur := m.gamePhaseEnd.Sub(m.gameStart)
	r := jsonReport{
		StartedAt:      t0.Format(time.RFC3339),
		TotalSec:       total.Seconds(),
		GamePhaseSec:   gameDur.Seconds(),
		Conns:          *numConns,
		Games:          *numGames,
		ConnOK:         m.connOK.Load(),
		ConnFail:       m.connFail.Load(),
		MsgIn:          m.msgIn.Load(),
		MsgOut:         m.msgOut.Load(),
		MsgInPerSec:    float64(m.msgIn.Load()) / total.Seconds(),
		GamePushPerSec: float64(m.statePush.Load()) / gameDur.Seconds(),
		SettleOK:       m.settleOK.Load(),
		SettleWin:      m.settleWin.Load(),
		SettleLose:     m.settleLose.Load(),
		Errors:         m.errCnt,
		PeakGoroutine:  s.peakGoroutine.Load(),
		PeakHeapMB:     float64(s.peakHeap.Load()) / (1 << 20),
	}
	var battle, all []float64
	for _, op := range []string{"register", "login", "enter", "deploy", "ping"} {
		s2 := m.lat.get(op)
		r.Latency = append(r.Latency, latencyRow(op, s2))
		if op == "enter" || op == "deploy" {
			battle = append(battle, s2...)
		}
		all = append(all, s2...)
	}
	sort.Float64s(battle)
	sort.Float64s(all)
	r.BattleP95Ms = percentile(battle, 0.95)
	r.AllP95Ms = percentile(all, 0.95)

	var sent, fail int64
	m.opMu.Lock()
	for _, c := range m.opCnt {
		sent += c.sent
		fail += c.fail
	}
	m.opMu.Unlock()
	r.ErrRatePct = pct(fail, sent)

	for _, w := range s.results() {
		r.Procs = append(r.Procs, jsonProc{
			Name: w.name, PID: w.pid,
			PeakMemMB: float64(w.peakWS) / (1 << 20), PeakCPUPct: w.peakCPU, AvgCPUPct: w.avgCPU(),
		})
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ---------------- 进程资源采样（Windows）----------------

type procWatcher struct {
	name    string
	pid     uint32
	handle  uintptr
	lastCPU uint64 // 100ns 单位的累计 CPU 时间
	lastAt  time.Time
	peakWS  uint64  // 工作集内存峰值（字节）
	peakCPU float64 // 采样间隔内 CPU% 峰值（占整机）
	sumCPU  float64
	n       int
}

func (w *procWatcher) avgCPU() float64 {
	if w.n == 0 {
		return 0
	}
	return w.sumCPU / float64(w.n)
}

type procSampler struct {
	watchers      []*procWatcher
	stopCh        chan struct{}
	stopped       chan struct{}
	peakGoroutine atomic.Int64
	peakHeap      atomic.Int64
}

func newProcSampler(watchSpec string) *procSampler {
	s := &procSampler{stopCh: make(chan struct{}), stopped: make(chan struct{})}
	// 自身进程
	if h := openProc(uint32(os.Getpid())); h != 0 {
		s.watchers = append(s.watchers, &procWatcher{name: "loadtest", pid: uint32(os.Getpid()), handle: h})
	}
	if watchSpec == "" {
		return s
	}
	for _, item := range strings.Split(watchSpec, ",") {
		name, pidStr, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			continue
		}
		var pid uint32
		if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid == 0 {
			continue
		}
		h := openProc(pid)
		if h == 0 {
			fmt.Printf("watch: 打开进程 %s(pid=%d) 失败，跳过\n", name, pid)
			continue
		}
		s.watchers = append(s.watchers, &procWatcher{name: name, pid: pid, handle: h})
	}
	return s
}

func (s *procSampler) start() {
	go func() {
		defer close(s.stopped)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.sample()
			}
		}
	}()
}

func (s *procSampler) stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	<-s.stopped
}

func (s *procSampler) sample() {
	now := time.Now()
	numCPU := float64(runtime.NumCPU())
	for _, w := range s.watchers {
		kernel, user, ok := procTimes(w.handle)
		if !ok {
			continue
		}
		cpu := kernel + user
		if !w.lastAt.IsZero() && cpu >= w.lastCPU {
			wall := now.Sub(w.lastAt).Seconds()
			if wall > 0 {
				// 100ns → 秒；除以核数得到占整机的百分比
				p := (float64(cpu-w.lastCPU) / 1e7) / wall / numCPU * 100
				if p > w.peakCPU {
					w.peakCPU = p
				}
				w.sumCPU += p
				w.n++
			}
		}
		w.lastCPU, w.lastAt = cpu, now

		if ws, ok := procWorkingSet(w.handle); ok && ws > w.peakWS {
			w.peakWS = ws
		}
	}
	// 自身 Go 运行时指标
	if g := int64(runtime.NumGoroutine()); g > s.peakGoroutine.Load() {
		s.peakGoroutine.Store(g)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if h := int64(ms.HeapAlloc); h > s.peakHeap.Load() {
		s.peakHeap.Store(h)
	}
}

func (s *procSampler) results() []*procWatcher { return s.watchers }
