// 战斗链路验证客户端：
//
//	-mode=basic     进房 → 部署 → 收状态推送 → 收结算
//	-mode=reconnect 进房 → 部署 → 中途模拟断线 → 用 roomId 重连 → 状态继续 → 收结算
//	-mode=skills    进房 → 部署 → 技能（含冷却校验）→ 撤回（返还费用）→ 再部署 → 收结算
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
)

// 战斗消息号
const (
	msgEnterRoom  uint16 = 2000
	msgEnterRoomR uint16 = 2001
	msgDeploy     uint16 = 2002
	msgDeployR    uint16 = 2003
	msgState      uint16 = 2004
	msgSettle     uint16 = 2005
	msgRetreat    uint16 = 2006
	msgRetreatR   uint16 = 2007
	msgSkill      uint16 = 2008
	msgSkillR     uint16 = 2009
)

var (
	gwAddr      = flag.String("gw", "127.0.0.1:8888", "网关地址")
	mode        = flag.String("mode", "basic", "验证场景：basic | reconnect | skills")
	doDeploy    = flag.Bool("deploy", true, "是否部署干员")
	cutoffFrame = flag.Uint("cutoff", 12, "reconnect 场景收到该帧后模拟断线（应大于快照间隔帧）")
	uid         = flag.Uint64("uid", 1, "进房携带的玩家 uid（需在 player 表存在）")
)

func main() {
	flag.Parse()

	var err error
	switch *mode {
	case "basic":
		err = runBasic()
	case "reconnect":
		err = runReconnect()
	case "skills":
		err = runSkills()
	default:
		fmt.Println("未知 -mode:", *mode)
		os.Exit(1)
	}
	if err != nil {
		fmt.Println("失败:", err)
		os.Exit(1)
	}
}

// runBasic 进房 → 部署 → 收状态推送 → 收结算
func runBasic() error {
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.close()

	resp, err := enter(c, 0)
	if err != nil {
		return err
	}
	fmt.Printf("[进房] code=%d roomId=%d seed=%d\n", resp.Code, resp.RoomId, resp.Seed)
	if resp.Code != 0 {
		return fmt.Errorf("进房失败 code=%d msg=%s", resp.Code, resp.Msg)
	}

	deadline := time.Now().Add(20 * time.Second)
	deployed := false
	for time.Now().Before(deadline) {
		state, settle, err := nextBattle(c, deadline)
		if err != nil {
			return err
		}
		if settle != nil {
			printSettle(settle)
			return nil
		}
		printState(state)

		// 收到第一帧后再部署，确保房间已创建
		if *doDeploy && !deployed {
			deployed = true
			if err := deployOperator(c, resp.RoomId); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("超时，未收到结算")
}

// runReconnect 进房 → 部署 → 中途断线 → 用 roomId 重连 → 验证恢复后续跑 → 收结算
func runReconnect() error {
	// ---- 阶段一：首次进房，部署后推进到 cutoff 帧 ----
	c1, err := dial()
	if err != nil {
		return err
	}
	resp1, err := enter(c1, 0)
	if err != nil {
		c1.close()
		return err
	}
	if resp1.Code != 0 {
		c1.close()
		return fmt.Errorf("进房失败 code=%d msg=%s", resp1.Code, resp1.Msg)
	}
	fmt.Printf("[1] 进房 code=%d roomId=%d seed=%d\n", resp1.Code, resp1.RoomId, resp1.Seed)

	deadline := time.Now().Add(30 * time.Second)
	var last *api.BattleStatePush
	for last == nil {
		state, settle, err := nextBattle(c1, deadline)
		if err != nil {
			c1.close()
			return err
		}
		if settle != nil {
			c1.close()
			return fmt.Errorf("尚未断线战斗就已结算 result=%d", settle.Result)
		}
		printState(state)
		last = state
	}
	if *doDeploy {
		if err := deployOperator(c1, resp1.RoomId); err != nil {
			c1.close()
			return err
		}
	}

	for uint32(*cutoffFrame) > last.Frame {
		state, settle, err := nextBattle(c1, deadline)
		if err != nil {
			c1.close()
			return err
		}
		if settle != nil {
			c1.close()
			return fmt.Errorf("断线前战斗就已结算 result=%d", settle.Result)
		}
		printState(state)
		last = state
	}
	fmt.Printf("[2] 断线前最后帧 frame=%d cost=%d baseHP=%d 干员=%d 敌人=%d\n",
		last.Frame, last.Cost, last.BaseHp, len(last.Operators), len(last.Enemies))

	c1.close() // 模拟客户端断线，服务端房间与 tick 继续
	fmt.Println("--- 关闭连接，模拟断线 ---")

	// ---- 阶段二：用 roomId 重连，检查是否恢复而不是重开 ----
	c2, err := dial()
	if err != nil {
		return err
	}
	defer c2.close()
	resp2, err := enter(c2, resp1.RoomId)
	if err != nil {
		return err
	}
	if resp2.Code != 0 {
		return fmt.Errorf("重连失败 code=%d msg=%s（断线过久房间可能已结束）", resp2.Code, resp2.Msg)
	}
	fmt.Printf("[3] 重连 code=%d roomId=%d seed=%d\n", resp2.Code, resp2.RoomId, resp2.Seed)
	if resp2.Seed != resp1.Seed {
		return fmt.Errorf("重连返回 seed=%d 与首次进房 seed=%d 不一致，疑似换了房间", resp2.Seed, resp1.Seed)
	}

	state2, settle2, err := nextBattle(c2, time.Now().Add(20*time.Second))
	if err != nil {
		return err
	}
	if settle2 != nil {
		return fmt.Errorf("重连后直接结算 result=%d，未看到恢复状态", settle2.Result)
	}
	printState(state2)
	if state2.Frame <= 1 {
		return fmt.Errorf("恢复后帧号仍为 %d，疑似重开而非续跑", state2.Frame)
	}
	if len(state2.Operators) != len(last.Operators) {
		return fmt.Errorf("恢复后干员数 %d 与断线前 %d 不一致，状态未恢复", len(state2.Operators), len(last.Operators))
	}
	fmt.Printf("[4] 恢复校验通过：帧 %d→%d（快照回退在预期内），沿用干员 %d 个\n",
		last.Frame, state2.Frame, len(state2.Operators))

	// ---- 阶段三：从恢复点继续推进到结算 ----
	deadline2 := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline2) {
		state, settle, err := nextBattle(c2, deadline2)
		if err != nil {
			return err
		}
		if settle != nil {
			printSettle(settle)
			if settle.Result == 0 {
				fmt.Println("验证通过：断线重连后战斗从原进度继续，并正常结算（胜利）")
				return nil
			}
			return fmt.Errorf("断线重连后结算结果非胜利 result=%d", settle.Result)
		}
		printState(state)
	}
	return fmt.Errorf("重连后等待结算超时")
}

// runSkills 进房 → 部署 → 等敌人进入射程 → 放技能 → 冷却校验 → 撤回 → 再部署 → 收结算
func runSkills() error {
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.close()

	resp, err := enter(c, 0)
	if err != nil {
		return err
	}
	fmt.Printf("[1] 进房 code=%d roomId=%d\n", resp.Code, resp.RoomId)
	if resp.Code != 0 {
		return fmt.Errorf("进房失败 code=%d msg=%s", resp.Code, resp.Msg)
	}

	deadline := time.Now().Add(30 * time.Second)
	var last *api.BattleStatePush
	for last == nil || last.Frame < 8 {
		state, settle, err := nextBattle(c, deadline)
		if err != nil {
			return err
		}
		if settle != nil {
			return fmt.Errorf("操作尚未完成战斗就已结算 result=%d", settle.Result)
		}
		printState(state)
		last = state
		if len(state.Operators) == 0 {
			if err := deployOperator(c, resp.RoomId); err != nil {
				return err
			}
		}
	}

	// ---- 技能：敌人已进入射程，应扣费并造成伤害 ----
	sk, err := castSkill(c, resp.RoomId, 1)
	if err != nil {
		return err
	}
	if sk.Code != 0 {
		return fmt.Errorf("技能释放失败 code=%d msg=%s", sk.Code, sk.Msg)
	}
	if sk.Damage <= 0 || sk.HitCount <= 0 {
		return fmt.Errorf("技能未命中：damage=%d hit=%d", sk.Damage, sk.HitCount)
	}

	// ---- 冷却：立刻再放应被拒绝 ----
	sk2, err := castSkill(c, resp.RoomId, 1)
	if err != nil {
		return err
	}
	if sk2.Code == 0 {
		return fmt.Errorf("冷却未生效：连续两次释放均成功")
	}
	fmt.Printf("[冷却] 连续释放被拒绝 code=%d msg=%s\n", sk2.Code, sk2.Msg)

	// ---- 撤回：应返还部分费用 ----
	rt, err := retreat(c, resp.RoomId, 1)
	if err != nil {
		return err
	}
	if rt.Code != 0 {
		return fmt.Errorf("撤回失败 code=%d msg=%s", rt.Code, rt.Msg)
	}
	if rt.RemainingCost <= sk.RemainingCost {
		return fmt.Errorf("撤回未返还费用：技能后 %d → 撤回后 %d", sk.RemainingCost, rt.RemainingCost)
	}
	fmt.Printf("[费用] 技能后 %d → 撤回后 %d（返还生效）\n", sk.RemainingCost, rt.RemainingCost)

	// 撤回后场上没人，重新部署避免漏怪
	if err := deployOperator(c, resp.RoomId); err != nil {
		return err
	}

	// ---- 继续推进到结算 ----
	for time.Now().Before(deadline) {
		state, settle, err := nextBattle(c, deadline)
		if err != nil {
			return err
		}
		if settle != nil {
			printSettle(settle)
			if settle.Result == 0 {
				fmt.Println("验证通过：技能扣费并命中、冷却拦截连发、撤回返还费用后仍正常通关")
				return nil
			}
			return fmt.Errorf("结算结果非胜利 result=%d", settle.Result)
		}
		printState(state)
	}
	return fmt.Errorf("等待结算超时")
}

// ---- 协议收发 ----

type client struct {
	conn     net.Conn
	buf      bytes.Buffer
	lastPing time.Time
}

func dial() (*client, error) {
	conn, err := net.DialTimeout("tcp", *gwAddr, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("连接网关失败: %w", err)
	}
	return &client{conn: conn}, nil
}

func (c *client) close() { _ = c.conn.Close() }

func (c *client) send(msgID uint16, body []byte) error {
	pkt, err := common.Encode(msgID, 0, body)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(pkt)
	return err
}

// next 读下一个完整包；单次读超时返回 nil, nil，由调用方继续轮询
func (c *client) next(readTimeout time.Duration) (*common.Packet, error) {
	c.keepAlive()
	if p, err := common.Decode(&c.buf); err != nil || p != nil {
		return p, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(readTimeout))
	tmp := make([]byte, 4096)
	n, err := c.conn.Read(tmp)
	if err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			return nil, nil
		}
		return nil, err
	}
	c.buf.Write(tmp[:n])
	return common.Decode(&c.buf)
}

// keepAlive 定期发心跳（msgId=0），避免网关 10 秒无上行数据判定超时
func (c *client) keepAlive() {
	if time.Since(c.lastPing) < 5*time.Second {
		return
	}
	c.lastPing = time.Now()
	body, err := proto.Marshal(&api.PingReq{Timestamp: time.Now().UnixNano()})
	if err != nil {
		return
	}
	pkt, err := common.Encode(0, 0, body)
	if err != nil {
		return
	}
	_, _ = c.conn.Write(pkt)
}

// enter 发送进房请求（roomID=0 为新开，非 0 为断线重连），等待 2001 回包
func enter(c *client, roomID uint32) (*api.EnterRoomResp, error) {
	body, err := proto.Marshal(&api.EnterRoomReq{LevelId: 1, RoomId: roomID, Uid: *uid})
	if err != nil {
		return nil, err
	}
	if err := c.send(msgEnterRoom, body); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.next(time.Second)
		if err != nil {
			return nil, err
		}
		if p == nil || p.MessageID != msgEnterRoomR {
			continue // 跳过与本次请求无关的早期推送
		}
		var resp api.EnterRoomResp
		if err := proto.Unmarshal(p.Payload, &resp); err != nil {
			return nil, err
		}
		return &resp, nil
	}
	return nil, fmt.Errorf("等待进房回包超时")
}

// deployOperator 部署 1 号干员并等待 2003 回包
func deployOperator(c *client, roomID uint32) error {
	body, err := proto.Marshal(&api.DeployOperatorReq{
		RoomId: roomID, OperatorId: 1, GridX: 4, GridY: 0, Direction: 3,
	})
	if err != nil {
		return err
	}
	if err := c.send(msgDeploy, body); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.next(time.Second)
		if err != nil {
			return err
		}
		if p == nil || p.MessageID != msgDeployR {
			continue
		}
		var resp api.DeployOperatorResp
		if err := proto.Unmarshal(p.Payload, &resp); err != nil {
			return err
		}
		fmt.Printf("[部署] code=%d msg=%s remaining_cost=%d\n", resp.Code, resp.Msg, resp.RemainingCost)
		if resp.Code != 0 {
			return fmt.Errorf("部署失败: %s", resp.Msg)
		}
		return nil
	}
	return fmt.Errorf("等待部署回包超时")
}

// retreat 撤回干员并等待 2007 回包
func retreat(c *client, roomID, opID uint32) (*api.RetreatOperatorResp, error) {
	body, err := proto.Marshal(&api.RetreatOperatorReq{RoomId: roomID, OperatorId: opID})
	if err != nil {
		return nil, err
	}
	if err := c.send(msgRetreat, body); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.next(time.Second)
		if err != nil {
			return nil, err
		}
		if p == nil || p.MessageID != msgRetreatR {
			continue
		}
		var resp api.RetreatOperatorResp
		if err := proto.Unmarshal(p.Payload, &resp); err != nil {
			return nil, err
		}
		fmt.Printf("[撤回] code=%d msg=%s remaining_cost=%d\n", resp.Code, resp.Msg, resp.RemainingCost)
		return &resp, nil
	}
	return nil, fmt.Errorf("等待撤回回包超时")
}

// castSkill 释放技能并等待 2009 回包
func castSkill(c *client, roomID, opID uint32) (*api.CastSkillResp, error) {
	body, err := proto.Marshal(&api.CastSkillReq{RoomId: roomID, OperatorId: opID})
	if err != nil {
		return nil, err
	}
	if err := c.send(msgSkill, body); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.next(time.Second)
		if err != nil {
			return nil, err
		}
		if p == nil || p.MessageID != msgSkillR {
			continue
		}
		var resp api.CastSkillResp
		if err := proto.Unmarshal(p.Payload, &resp); err != nil {
			return nil, err
		}
		fmt.Printf("[技能] code=%d msg=%s remaining_cost=%d damage=%d hit=%d\n",
			resp.Code, resp.Msg, resp.RemainingCost, resp.Damage, resp.HitCount)
		return &resp, nil
	}
	return nil, fmt.Errorf("等待技能回包超时")
}

// nextBattle 读到 2004（状态）或 2005（结算）为止，二者互斥返回
func nextBattle(c *client, deadline time.Time) (*api.BattleStatePush, *api.BattleSettlePush, error) {
	for time.Now().Before(deadline) {
		p, err := c.next(time.Second)
		if err != nil {
			return nil, nil, err
		}
		if p == nil {
			continue
		}
		switch p.MessageID {
		case msgState:
			var s api.BattleStatePush
			if err := proto.Unmarshal(p.Payload, &s); err != nil {
				return nil, nil, err
			}
			return &s, nil, nil
		case msgSettle:
			var s api.BattleSettlePush
			if err := proto.Unmarshal(p.Payload, &s); err != nil {
				return nil, nil, err
			}
			return nil, &s, nil
		}
	}
	return nil, nil, fmt.Errorf("等待战斗推送超时")
}

func printState(s *api.BattleStatePush) {
	fmt.Printf("[状态] frame=%d cost=%d baseHP=%d 干员=%d 敌人=%d",
		s.Frame, s.Cost, s.BaseHp, len(s.Operators), len(s.Enemies))
	for _, e := range s.Enemies {
		fmt.Printf(" | enemy hp=%d pos=(%.2f,%.2f)", e.Hp, e.PosX, e.PosY)
	}
	fmt.Println()
}

func printSettle(s *api.BattleSettlePush) {
	fmt.Printf("[结算] result=%d score=%d duration=%ds rewards=%v\n",
		s.Result, s.Score, s.DurationSec, s.Rewards)
	if s.Result == 0 {
		fmt.Println("结论：胜利")
	} else {
		fmt.Println("结论：失败")
	}
}
