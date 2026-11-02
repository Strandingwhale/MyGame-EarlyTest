// 房间与战斗推进：所有位移、伤害、胜负判定都在服务端完成，客户端只收状态推送。
package battle

import (
	"bytes"
	"encoding/json"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/datasave"
)

const tickInterval = 200 * time.Millisecond // 5 帧/秒

// dataSaveAddr DataSave 内部服务地址，结算落库走这里
const dataSaveAddr = "127.0.0.1:9997"

// snapshotIntervalFrames 每 N 帧保存一次内存快照（10 帧 = 2 秒）
const snapshotIntervalFrames = 10

// 结算结果，对应 BattleSettlePush.result
const (
	resultWin  = 0 // 胜利
	resultLose = 1 // 失败
)

var (
	roomMu  sync.RWMutex
	rooms   = map[uint32]*Room{}
	roomSeq atomic.Uint32
)

// Operator 场上干员
type Operator struct {
	ID        uint32
	X, Y      uint32
	Direction int32
	// SkillReadyFrame 技能可再次释放的帧号（0 表示随时可放）
	SkillReadyFrame uint32
}

// Enemy 场上敌人
type Enemy struct {
	InstanceID uint64
	HP         int32
	Speed      float64 // 移动速度（格/秒），由波次配置决定
	// Dist 已沿路径行进的距离
	Dist float64
}

// BattleSnapshot 一帧完整状态的序列化快照，仅存内存，供重连恢复/后续落盘使用。
// 存的是服务端内部状态（含敌人 Dist、速度与波次进度），不是对客户端的协议消息。
type BattleSnapshot struct {
	Frame uint32
	Data  []byte // snapshotState 的 JSON
}

// snapshotState 快照内容：Room 中由 mu 保护的全部战斗状态
type snapshotState struct {
	Frame       uint32
	Cost        int32
	BaseHP      int32
	Operators   []snapshotOperator
	Enemies     []snapshotEnemy
	WaveIndex   int     // 当前波次下标
	Spawned     int     // 当前波已生成数量
	SpawnTimer  float64 // 距下一只生成的剩余秒数
	InstanceSeq uint64  // 敌人实例 ID 自增序号
}

type snapshotOperator struct {
	ID              uint32
	X, Y            uint32
	Direction       int32
	SkillReadyFrame uint32
}

type snapshotEnemy struct {
	InstanceID uint64
	HP         int32
	Speed      float64
	Dist       float64
}

// Room 一场战斗。connID 与 send 在重连恢复时重新绑定（持 mu 进行），其余状态由 mu 保护。
type Room struct {
	ID     uint32
	Seed   uint64
	connID uint64
	send   func(msgID uint16, body []byte) // 推送给房间所属客户端

	mu        sync.Mutex
	frame     uint32
	cost      int32
	baseHP    int32
	operators []*Operator
	enemies   []*Enemy
	over      bool
	startedAt time.Time
	snapshots []*BattleSnapshot // 定时保存的内存快照，最新在最后

	// 波次进度：当前波次下标、本波已生成数量、距下一只的倒计时、敌人实例 ID 序号
	waveIndex   int
	spawned     int
	spawnTimer  float64
	instanceSeq uint64

	done   chan struct{} // 关闭以停止当前 tick 循环
	exited chan struct{} // run() 完全退出后关闭；与 done 配对，重连时一并重建
}

// createRoom 创建房间并立即开始 tick 循环
func createRoom(connID uint64, send func(msgID uint16, body []byte)) *Room {
	id := roomSeq.Add(1)
	r := &Room{
		ID:        id,
		Seed:      uint64(time.Now().UnixNano()),
		connID:    connID,
		send:      send,
		cost:      initCost,
		baseHP:    baseMaxHP,
		startedAt: time.Now(),
		done:      make(chan struct{}),
		exited:    make(chan struct{}),
	}
	// 敌人不在建房间时生成，由 step 按波次表逐个产出

	roomMu.Lock()
	rooms[id] = r
	roomMu.Unlock()

	go r.run()
	return r
}

func getRoom(id uint32) *Room {
	roomMu.RLock()
	defer roomMu.RUnlock()
	return rooms[id]
}

func removeRoom(id uint32) {
	roomMu.Lock()
	delete(rooms, id)
	roomMu.Unlock()
}

// run 服务端 tick 循环
func (r *Room) run() {
	defer close(r.exited) // r.exited 在循环启动前绑定，重连重建新通道不影响本次关闭
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	dt := tickInterval.Seconds()
	for {
		select {
		case <-r.done:
			return
		case <-ticker.C:
			if r.step(dt) {
				return
			}
		}
	}
}

// deploy 部署干员，返回错误码、提示与剩余费用
func (r *Room) deploy(opID, x, y uint32, dir int32) (int32, string, int32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.over {
		return codeBadParam, "战斗已结束", r.cost
	}
	if x >= mapWidth || y >= mapHeight {
		return codeBadParam, "坐标超出地图", r.cost
	}
	if len(r.operators) >= maxOperators {
		return codeBadParam, "干员数量已达上限", r.cost
	}
	if r.cost < deployCost {
		return codeBadParam, "费用不足", r.cost
	}

	r.cost -= deployCost
	r.operators = append(r.operators, &Operator{ID: opID, X: x, Y: y, Direction: dir})
	logger.Printf("房间%d 部署干员%d 于(%d,%d)，剩余费用%d", r.ID, opID, x, y, r.cost)
	return codeOK, "ok", r.cost
}

// retreat
func (r *Room) retreat(opID uint32) (int32, string, int32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.over {
		return codeBadParam, "战斗已结束", r.cost
	}
	for i, op := range r.operators {
		if op.ID != opID {
			continue
		}
		r.operators = append(r.operators[:i], r.operators[i+1:]...)
		r.cost += retreatRefund
		logger.Printf("房间%d 撤回干员%d，返还费用%d，剩余费用%d", r.ID, opID, retreatRefund, r.cost)
		return codeOK, "ok", r.cost
	}
	return codeBadParam, "干员不在场上", r.cost
}

// castSkill 释放干员技能：校验在场、冷却与费用，对射程内敌人造成一次性伤害。
// 返回错误码、提示、剩余费用、总伤害与命中数。
func (r *Room) castSkill(opID uint32) (int32, string, int32, int32, int32) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.over {
		return codeBadParam, "战斗已结束", r.cost, 0, 0
	}

	var op *Operator
	for _, o := range r.operators {
		if o.ID == opID {
			op = o
			break
		}
	}
	if op == nil {
		return codeBadParam, "干员不在场上", r.cost, 0, 0
	}
	if r.frame < op.SkillReadyFrame {
		return codeBadParam, "技能冷却中", r.cost, 0, 0
	}
	if r.cost < skillCost {
		return codeBadParam, "费用不足", r.cost, 0, 0
	}

	r.cost -= skillCost
	op.SkillReadyFrame = r.frame + skillCooldownFrames

	var damage, hits int32
	for _, e := range r.enemies {
		if e.HP <= 0 {
			continue
		}
		p := pointAtDistance(e.Dist)
		if math.Hypot(p.X-float64(op.X), p.Y-float64(op.Y)) <= attackRange {
			e.HP -= skillDamage
			damage += skillDamage
			hits++
		}
	}
	logger.Printf("房间%d 干员%d 释放技能：命中%d 伤害%d，剩余费用%d", r.ID, opID, hits, damage, r.cost)
	return codeOK, "ok", r.cost, damage, hits
}

// step 推进一帧；返回 true 表示战斗已结束
func (r *Room) step(dt float64) bool {
	r.mu.Lock()
	if r.over {
		r.mu.Unlock()
		return true
	}
	r.frame++

	// 1. 按波次表生成敌人（每帧最多一只）
	r.spawnLocked(dt)

	total := pathLength()

	// 2. 敌人沿路径前进，速度由各自波次配置决定
	for _, e := range r.enemies {
		e.Dist += e.Speed * dt
	}

	// 3. 射程内的干员对敌人造成伤害（每帧一次）
	for _, op := range r.operators {
		for _, e := range r.enemies {
			if e.HP <= 0 {
				continue
			}
			p := pointAtDistance(e.Dist) // 按已行进距离求路径上的坐标
			if math.Hypot(p.X-float64(op.X), p.Y-float64(op.Y)) <= attackRange {
				e.HP -= attackDamage
			}
		}
	}

	// 4. 移除阵亡敌人；走到终点的敌人扣基地生命
	alive := make([]*Enemy, 0, len(r.enemies))
	for _, e := range r.enemies {
		if e.HP <= 0 {
			continue
		}
		if e.Dist >= total {
			r.baseHP--
			logger.Printf("房间%d 敌人到达终点，基地生命%d", r.ID, r.baseHP)
			continue
		}
		alive = append(alive, e)
	}
	r.enemies = alive

	// 5. 胜负判定：基地被击穿为失败；所有波次出完且场上清空为胜利
	result := int32(-1)
	switch {
	case r.baseHP <= 0:
		result = resultLose
	case r.waveIndex >= len(waves) && len(r.enemies) == 0:
		result = resultWin
	}
	state := r.snapshotLocked()
	// 每 N 帧保存一次内存快照（存内部状态，含敌人 Dist 与波次进度）
	if r.frame%snapshotIntervalFrames == 0 {
		if data, err := json.Marshal(r.snapshotDataLocked()); err == nil {
			r.snapshots = append(r.snapshots, &BattleSnapshot{Frame: r.frame, Data: data})
		} else {
			logger.Println("序列化战斗快照失败:", err)
		}
	}
	if result >= 0 {
		r.over = true
	}
	r.mu.Unlock()

	// 先解锁再推送，避免慢客户端阻塞 tick
	r.pushState(state)
	if result >= 0 {
		r.pushSettle(result)
		return true
	}
	return false
}

// resume 重连恢复：停掉旧 tick 循环，从最近一次快照回填状态，重绑推送通道后重启 tick。
// 返回 false 表示战斗已结束，不可恢复。
func (r *Room) resume(connID uint64, send func(msgID uint16, body []byte)) bool {
	r.mu.Lock()
	if r.over {
		r.mu.Unlock()
		return false
	}
	close(r.done) // 通知旧循环退出
	r.mu.Unlock()

	<-r.exited // 等旧循环完全退出，避免恢复状态与 step 并发

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.over { // 等待期间战斗结束，pushSettle 已移除房间
		return false
	}

	// 从最近一次快照恢复 mu 保护的状态；无快照则保持当前状态继续
	if n := len(r.snapshots); n > 0 {
		var snap snapshotState
		if err := json.Unmarshal(r.snapshots[n-1].Data, &snap); err != nil {
			logger.Printf("房间%d 快照反序列化失败，从当前状态继续: %v", r.ID, err)
		} else {
			r.frame = snap.Frame
			r.cost = snap.Cost
			r.baseHP = snap.BaseHP
			r.waveIndex = snap.WaveIndex
			r.spawned = snap.Spawned
			r.spawnTimer = snap.SpawnTimer
			r.instanceSeq = snap.InstanceSeq
			r.operators = make([]*Operator, 0, len(snap.Operators))
			for _, op := range snap.Operators {
				r.operators = append(r.operators, &Operator{
					ID:              op.ID,
					X:               op.X,
					Y:               op.Y,
					Direction:       op.Direction,
					SkillReadyFrame: op.SkillReadyFrame,
				})
			}
			r.enemies = make([]*Enemy, 0, len(snap.Enemies))
			for _, e := range snap.Enemies {
				r.enemies = append(r.enemies, &Enemy{
					InstanceID: e.InstanceID,
					HP:         e.HP,
					Speed:      e.Speed,
					Dist:       e.Dist,
				})
			}
		}
	}

	r.connID = connID
	r.send = send
	r.done = make(chan struct{})
	r.exited = make(chan struct{})
	go r.run()
	logger.Printf("房间%d 重连恢复：connID=%d，从帧%d继续", r.ID, connID, r.frame)
	return true
}

// spawnLocked 按波次表生成敌人，每帧最多生成一只；调用方需持有 r.mu
func (r *Room) spawnLocked(dt float64) {
	// 当前波已出完则推进到下一波（最后一波出完后下标越界，生成结束）
	for r.waveIndex < len(waves) && r.spawned >= waves[r.waveIndex].Count {
		r.waveIndex++
		r.spawned = 0
	}
	if r.waveIndex >= len(waves) {
		return
	}

	r.spawnTimer -= dt
	if r.spawnTimer > 0 {
		return
	}

	w := waves[r.waveIndex]
	r.instanceSeq++
	r.enemies = append(r.enemies, &Enemy{
		InstanceID: r.instanceSeq,
		HP:         w.HP,
		Speed:      w.Speed,
	})
	r.spawned++
	r.spawnTimer = w.IntervalSec
	logger.Printf("房间%d 波次%d 生成敌人%d 血量%d 速度%.1f", r.ID, r.waveIndex+1, r.instanceSeq, w.HP, w.Speed)
}

// snapshotDataLocked 构造内部状态快照，调用方需持有 r.mu
func (r *Room) snapshotDataLocked() *snapshotState {
	s := &snapshotState{
		Frame:       r.frame,
		Cost:        r.cost,
		BaseHP:      r.baseHP,
		WaveIndex:   r.waveIndex,
		Spawned:     r.spawned,
		SpawnTimer:  r.spawnTimer,
		InstanceSeq: r.instanceSeq,
	}
	for _, op := range r.operators {
		s.Operators = append(s.Operators, snapshotOperator{
			ID:              op.ID,
			X:               op.X,
			Y:               op.Y,
			Direction:       op.Direction,
			SkillReadyFrame: op.SkillReadyFrame,
		})
	}
	for _, e := range r.enemies {
		s.Enemies = append(s.Enemies, snapshotEnemy{
			InstanceID: e.InstanceID,
			HP:         e.HP,
			Speed:      e.Speed,
			Dist:       e.Dist,
		})
	}
	return s
}

// snapshotLocked 生成当前帧状态快照，调用方需持有 r.mu
func (r *Room) snapshotLocked() *api.BattleStatePush {
	s := &api.BattleStatePush{
		RoomId: r.ID,
		Frame:  r.frame,
		Cost:   r.cost,
		BaseHp: r.baseHP,
	}
	for _, op := range r.operators {
		s.Operators = append(s.Operators, &api.OperatorState{
			OperatorId: op.ID,
			GridX:      op.X,
			GridY:      op.Y,
			Hp:         operatorMaxHP,
			Direction:  op.Direction,
		})
	}
	for _, e := range r.enemies {
		p := pointAtDistance(e.Dist)
		s.Enemies = append(s.Enemies, &api.EnemyState{
			EnemyId:    enemyConfigID,
			InstanceId: e.InstanceID,
			Hp:         e.HP,
			PosX:       float32(p.X),
			PosY:       float32(p.Y),
		})
	}
	return s
}

// pushState 下发帧状态（msgId 2004）
func (r *Room) pushState(state *api.BattleStatePush) {
	body, err := proto.Marshal(state)
	if err != nil {
		logger.Println("序列化 BattleStatePush 失败:", err)
		return
	}
	r.send(2004, body)
}

// pushSettle 下发结算（msgId 2005），随后销毁房间
func (r *Room) pushSettle(result int32) {
	duration := int32(time.Since(r.startedAt).Seconds())
	score := int32(0)
	var rewards []string
	if result == resultWin {
		score = 1000
		rewards = []string{"exp:100", "gold:500"}
	}
	body, err := proto.Marshal(&api.BattleSettlePush{
		RoomId:      r.ID,
		Result:      result,
		Score:       score,
		DurationSec: duration,
		Rewards:     rewards,
	})
	if err != nil {
		logger.Println("序列化 BattleSettlePush 失败:", err)
		return
	}
	r.send(2005, body)
	logger.Printf("房间%d 战斗结束：result=%d 用时%ds 分数%d", r.ID, result, duration, score)
	// 异步落库，避免写库阻塞 tick；uid 暂用房间号占位（登录态→战斗的 uid 链路下一轮补齐）
	go reportSettle(uint64(r.ID), result, score, duration, rewards)
	removeRoom(r.ID)
}

// reportSettle 作为客户端连接 DataSave，发送 3000 写战斗记录请求并读取回包
func reportSettle(uid uint64, result int32, score, duration int32, rewards []string) {
	body, err := json.Marshal(&datasave.SaveBattleRecordReq{
		UID:         uid,
		Result:      result,
		Score:       score,
		DurationSec: duration,
		Rewards:     rewards,
	})
	if err != nil {
		logger.Println("序列化战斗记录失败:", err)
		return
	}
	pkt, err := common.Encode(datasave.MsgSaveBattleRecord, 0, body)
	if err != nil {
		logger.Println("编码战斗记录失败:", err)
		return
	}

	conn, err := net.DialTimeout("tcp", dataSaveAddr, 2*time.Second)
	if err != nil {
		logger.Printf("连接datasave服务失败，战斗记录未落库: %v", err)
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write(pkt); err != nil {
		logger.Println("发送战斗记录失败:", err)
		return
	}

	// 读回包确认落库结果
	cache := &bytes.Buffer{}
	readBuf := make([]byte, 4096)
	for {
		n, err := conn.Read(readBuf)
		if err != nil {
			logger.Println("等待datasave回包失败:", err)
			return
		}
		cache.Write(readBuf[:n])

		p, err := common.Decode(cache)
		if err != nil {
			logger.Println("datasave回包解码失败:", err)
			return
		}
		if p == nil {
			continue
		}

		var resp datasave.SaveBattleRecordResp
		if err := json.Unmarshal(p.Payload, &resp); err != nil {
			logger.Println("解析datasave回包失败:", err)
			return
		}
		if resp.Code != 0 {
			logger.Printf("战斗记录落库失败：code=%d msg=%s", resp.Code, resp.Msg)
			return
		}
		logger.Printf("战斗记录落库成功：uid=%d exp+%d gold+%d", uid, resp.ExpGain, resp.GoldGain)
		return
	}
}
