// 房间与战斗推进：所有位移、伤害、胜负判定都在服务端完成，客户端只收状态推送。
package battle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/config"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/datasave"
)

const tickInterval = 200 * time.Millisecond // 5 帧/秒

// dataSaveAddr DataSave 内部服务地址，结算落库走这里
const dataSaveAddr = "127.0.0.1:9997"

// snapshotIntervalFrames 每 N 帧保存一次内存快照（10 帧 = 2 秒）
const snapshotIntervalFrames = 10

// offlineTimeout 玩家断线后等待重连的时长；超时销毁房间，避免僵尸房间占用资源
const offlineTimeout = 30 * time.Second

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

	// 以下数值来自干员配置表，部署时写入，重连恢复时随快照回填
	Cost   int32   // 部署费用，撤回时按它返还
	Attack int32   // 每帧攻击伤害
	Range  float64 // 攻击半径（格）
	HP     int32   // 干员血量（仅展示）
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
	Cost            int32
	Attack          int32
	Range           float64
	HP              int32
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
	uid    uint64 // 房间归属玩家，进房扣体力与结算落库用
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

	// 断线等待重连：disconnected 为 true 时暂停推送，房间继续 tick
	disconnected bool
	offlineSeq   uint64 // 断线序号，重连/再次断线时递增，用于让旧的超时计时器失效

	// 波次进度：当前波次下标、本波已生成数量、距下一只的倒计时、敌人实例 ID 序号
	waveIndex   int
	spawned     int
	spawnTimer  float64
	instanceSeq uint64
	wavesCfg    []Wave // 本房间的波次表，来自关卡配置

	done   chan struct{} // 关闭以停止当前 tick 循环
	exited chan struct{} // run() 完全退出后关闭；与 done 配对，重连时一并重建
}

// createRoom 创建房间并立即开始 tick 循环；波次表按关卡配置生成，配置缺失时回落默认
func createRoom(uid, connID uint64, levelID uint32, send func(msgID uint16, body []byte)) *Room {
	id := roomSeq.Add(1)
	r := &Room{
		ID:        id,
		Seed:      uint64(time.Now().UnixNano()),
		uid:       uid,
		connID:    connID,
		send:      send,
		cost:      initCost,
		baseHP:    baseMaxHP,
		wavesCfg:  wavesForLevel(levelID),
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

	// 干员数值来自配置表；配置缺失时回落硬编码默认值
	cost, attack, rng, hp := int32(deployCost), int32(attackDamage), float64(attackRange), int32(operatorMaxHP)
	if cfg, ok := config.GetOperator(opID); ok {
		if cfg.Cost > 0 {
			cost = cfg.Cost
		}
		attack = cfg.Attack
		if cfg.Range > 0 {
			rng = float64(cfg.Range)
		}
		hp = cfg.Hp
	}

	if r.cost < cost {
		return codeBadParam, "费用不足", r.cost
	}

	r.cost -= cost
	r.operators = append(r.operators, &Operator{
		ID: opID, X: x, Y: y, Direction: dir,
		Cost: cost, Attack: attack, Range: rng, HP: hp,
	})
	logger.Printf("房间%d 部署干员%d 于(%d,%d)，费用%d 攻击%d 射程%.1f 剩余费用%d",
		r.ID, opID, x, y, cost, attack, rng, r.cost)
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
		// 返还按该干员实际部署费用的一半计算
		refund := op.Cost / 2
		if refund <= 0 {
			refund = retreatRefund
		}
		r.cost += refund
		logger.Printf("房间%d 撤回干员%d，返还费用%d，剩余费用%d", r.ID, opID, refund, r.cost)
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
		if math.Hypot(p.X-float64(op.X), p.Y-float64(op.Y)) <= op.Range {
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
			if math.Hypot(p.X-float64(op.X), p.Y-float64(op.Y)) <= op.Range {
				e.HP -= op.Attack
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
	case r.waveIndex >= len(r.wavesCfg) && len(r.enemies) == 0:
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
					Cost:            op.Cost,
					Attack:          op.Attack,
					Range:           op.Range,
					HP:              op.HP,
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
	r.disconnected = false
	r.offlineSeq++ // 让之前的断线超时计时器失效
	r.done = make(chan struct{})
	r.exited = make(chan struct{})
	go r.run()
	logger.Printf("房间%d 重连恢复：connID=%d，从帧%d继续", r.ID, connID, r.frame)
	return true
}

// markOffline 标记玩家断线：房间继续 tick 并暂停推送，等待重连；超时未重连则销毁
func (r *Room) markOffline(uid uint64) bool {
	r.mu.Lock()
	if r.over || r.uid != uid {
		r.mu.Unlock()
		return false
	}
	r.disconnected = true
	r.offlineSeq++
	seq := r.offlineSeq
	r.mu.Unlock()

	logger.Printf("房间%d 玩家 uid=%d 已断线，等待重连（超时%v）", r.ID, uid, offlineTimeout)
	go r.destroyIfStillOffline(seq)
	return true
}

// destroyIfStillOffline 断线超时仍未重连则销毁房间；seq 防止旧计时器误删已重连的房间
func (r *Room) destroyIfStillOffline(seq uint64) {
	timer := time.NewTimer(offlineTimeout)
	defer timer.Stop()
	<-timer.C

	r.mu.Lock()
	if r.over || !r.disconnected || r.offlineSeq != seq {
		r.mu.Unlock()
		return
	}
	r.over = true
	r.mu.Unlock()

	removeRoom(r.ID)
	logger.Printf("房间%d 断线超时未重连，房间已销毁", r.ID)
}

// spawnLocked 按波次表生成敌人，每帧最多生成一只；调用方需持有 r.mu
func (r *Room) spawnLocked(dt float64) {
	// 当前波已出完则推进到下一波（最后一波出完后下标越界，生成结束）
	for r.waveIndex < len(r.wavesCfg) && r.spawned >= r.wavesCfg[r.waveIndex].Count {
		r.waveIndex++
		r.spawned = 0
	}
	if r.waveIndex >= len(r.wavesCfg) {
		return
	}

	r.spawnTimer -= dt
	if r.spawnTimer > 0 {
		return
	}

	w := r.wavesCfg[r.waveIndex]
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
			Cost:            op.Cost,
			Attack:          op.Attack,
			Range:           op.Range,
			HP:              op.HP,
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
			Hp:         op.HP,
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

// pushState 下发帧状态（msgId 2004）；断线等待重连期间暂停推送
func (r *Room) pushState(state *api.BattleStatePush) {
	r.mu.Lock()
	if r.disconnected {
		r.mu.Unlock()
		return
	}
	send := r.send
	r.mu.Unlock()

	body, err := proto.Marshal(state)
	if err != nil {
		logger.Println("序列化 BattleStatePush 失败:", err)
		return
	}
	send(2004, body)
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

	r.mu.Lock()
	disconnected := r.disconnected
	send := r.send
	r.mu.Unlock()
	// 断线时客户端已不在，跳过推送；落库仍然执行
	if !disconnected {
		send(2005, body)
	}
	logger.Printf("房间%d 战斗结束：result=%d 用时%ds 分数%d", r.ID, result, duration, score)
	// 异步落库，避免写库阻塞 tick；uid 来自进房时登录态带入的玩家 ID
	go reportSettle(r.uid, result, score, duration, rewards)
	removeRoom(r.ID)
}

// callDataSave 作为客户端连接 DataSave，发送一条内部请求并解析回包到 resp
func callDataSave(msgID uint16, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("序列化datasave请求失败: %w", err)
	}
	pkt, err := common.Encode(msgID, 0, body)
	if err != nil {
		return fmt.Errorf("编码datasave请求失败: %w", err)
	}

	conn, err := net.DialTimeout("tcp", dataSaveAddr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("连接datasave服务失败: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("发送datasave请求失败: %w", err)
	}

	cache := &bytes.Buffer{}
	readBuf := make([]byte, 4096)
	for {
		n, err := conn.Read(readBuf)
		if err != nil {
			return fmt.Errorf("等待datasave回包失败: %w", err)
		}
		cache.Write(readBuf[:n])

		p, err := common.Decode(cache)
		if err != nil {
			return fmt.Errorf("datasave回包解码失败: %w", err)
		}
		if p == nil {
			continue
		}
		return json.Unmarshal(p.Payload, resp)
	}
}

// deductStamina 进房时扣 1 点体力；返回 DataSave 错误码与提示
func deductStamina(uid uint64) (int32, string) {
	var resp datasave.DeductStaminaResp
	if err := callDataSave(datasave.MsgDeductStamina, &datasave.DeductStaminaReq{
		UID:    uid,
		Amount: 1,
	}, &resp); err != nil {
		logger.Println("扣除体力失败:", err)
		return codeServerError, "服务器繁忙"
	}
	if resp.Code != 0 {
		logger.Printf("扣除体力失败：uid=%d code=%d msg=%s remaining=%d", uid, resp.Code, resp.Msg, resp.Remaining)
		return resp.Code, resp.Msg
	}
	logger.Printf("扣除体力成功：uid=%d 剩余%d", uid, resp.Remaining)
	return codeOK, "ok"
}

// reportSettle 作为客户端连接 DataSave，发送 3000 写战斗记录请求并读取回包
func reportSettle(uid uint64, result int32, score, duration int32, rewards []string) {
	var resp datasave.SaveBattleRecordResp
	err := callDataSave(datasave.MsgSaveBattleRecord, &datasave.SaveBattleRecordReq{
		UID:         uid,
		Result:      result,
		Score:       score,
		DurationSec: duration,
		Rewards:     rewards,
	}, &resp)
	if err != nil {
		logger.Printf("战斗记录落库失败：%v", err)
		return
	}
	if resp.Code != 0 {
		logger.Printf("战斗记录落库失败：code=%d msg=%s", resp.Code, resp.Msg)
		return
	}
	logger.Printf("战斗记录落库成功：uid=%d exp+%d gold+%d", uid, resp.ExpGain, resp.GoldGain)
}
