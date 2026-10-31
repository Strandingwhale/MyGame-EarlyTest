// 房间与战斗推进：所有位移、伤害、胜负判定都在服务端完成，客户端只收状态推送。
package battle

import (
	"math"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
)

const tickInterval = 200 * time.Millisecond // 5 帧/秒

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
}

// Enemy 场上敌人
type Enemy struct {
	InstanceID uint64
	HP         int32
	// Dist 已沿路径行进的距离
	Dist float64
}

// BattleSnapshot 一帧完整状态的序列化快照（frame、cost、baseHP、干员、敌人位置/血量），
// 仅存内存，供后续回放/恢复/落盘使用。
type BattleSnapshot struct {
	Frame uint32
	Data  []byte // proto.Marshal(BattleStatePush) 的结果
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
	// 极简关卡：开打即生成一只怪
	r.enemies = []*Enemy{{InstanceID: 1, HP: enemyMaxHP}}

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

// step 推进一帧；返回 true 表示战斗已结束
func (r *Room) step(dt float64) bool {
	r.mu.Lock()
	if r.over {
		r.mu.Unlock()
		return true
	}
	r.frame++

	total := pathLength()

	// 1. 敌人沿路径前进
	for _, e := range r.enemies {
		e.Dist += enemySpeed * dt
	}

	// 2. 射程内的干员对敌人造成伤害（每帧一次）
	for _, op := range r.operators {
		for _, e := range r.enemies {
			if e.HP <= 0 {
				continue
			}
			p := pointAtDistance(e.Dist) //pointAtDistance 按已行进距离求路径上的坐标；超出总长时返回终点
			if math.Hypot(p.X-float64(op.X), p.Y-float64(op.Y)) <= attackRange {
				e.HP -= attackDamage
			}
		}
	}

	// 3. 移除阵亡敌人；走到终点的敌人扣基地生命
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

	// 4. 胜负判定
	result := int32(-1)
	switch {
	case r.baseHP <= 0:
		result = resultLose
	case len(r.enemies) == 0:
		result = resultWin
	}
	state := r.snapshotLocked()
	// 每 N 帧保存一次内存快照
	if r.frame%snapshotIntervalFrames == 0 {
		if data, err := proto.Marshal(state); err == nil {
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
		var state api.BattleStatePush
		if err := proto.Unmarshal(r.snapshots[n-1].Data, &state); err != nil {
			logger.Printf("房间%d 快照反序列化失败，从当前状态继续: %v", r.ID, err)
		} else {
			r.frame = state.Frame
			r.cost = state.Cost
			r.baseHP = state.BaseHp
			r.operators = make([]*Operator, 0, len(state.Operators))
			for _, op := range state.Operators {
				r.operators = append(r.operators, &Operator{
					ID: op.OperatorId, X: op.GridX, Y: op.GridY, Direction: op.Direction,
				})
			}
			r.enemies = make([]*Enemy, 0, len(state.Enemies))
			for _, e := range state.Enemies {
				r.enemies = append(r.enemies, &Enemy{
					InstanceID: e.InstanceId,
					HP:         e.Hp,
					Dist:       distanceAtPoint(Vec2{X: float64(e.PosX), Y: float64(e.PosY)}),
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
	removeRoom(r.ID)
}
