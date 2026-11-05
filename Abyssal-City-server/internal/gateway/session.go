// Session 结构体，保存每一个客户端连接信息：sessionId、玩家 ID、连接、最后心跳时间。
package gateway

import (
	"math"
	"net"
	"sync"
	"time"
)

// 战斗消息限流：令牌桶容量 50，每 5 秒回满，即长期速率 10 条/秒
const (
	battleMsgCapacity = 50
	battleMsgWindow   = 5 * time.Second
)

type Session struct {
	conn       net.Conn
	connID     uint64    // 网关内自增的连接标识，login 回包靠它定位
	remoteAddr string    //客户端地址
	readCache  []byte    //拆包缓存（处理粘包半包）
	lastRecvAt time.Time // 最后收到消息时间，用于心跳超时
	sessionID  string    // 登录成功后分配的会话id
	uid        uint64    // 用户id，登录成功后赋值
	roomID     uint32    // 当前所在房间，进房成功后赋值，断线时通知 battle
	closeOnce  sync.Once
	closeChan  chan struct{}
	mu         sync.Mutex

	// 战斗消息令牌桶
	battleTokens   float64
	battleRefillAt time.Time
}

// NewSession 创建新会话
func NewSession(conn net.Conn, connID uint64) *Session {
	return &Session{
		conn:           conn,
		connID:         connID,
		remoteAddr:     conn.RemoteAddr().String(),
		readCache:      make([]byte, 0, 4096),
		lastRecvAt:     time.Now(),
		closeChan:      make(chan struct{}),
		battleTokens:   battleMsgCapacity,
		battleRefillAt: time.Now(),
	}
}

// ConnID 返回网关分配的连接标识
func (s *Session) ConnID() uint64 {
	return s.connID
}

// SetLogin 登录成功后绑定用户身份
func (s *Session) SetLogin(uid uint64, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uid = uid
	s.sessionID = sessionID
}

// UID 返回登录绑定的用户 id（未登录为 0）
func (s *Session) UID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uid
}

// SetRoom 记录当前所在房间（0 表示不在房间）
func (s *Session) SetRoom(roomID uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roomID = roomID
}

// RoomID 返回当前所在房间（不在房间为 0）
func (s *Session) RoomID() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roomID
}

// AllowBattleMsg 战斗消息令牌桶限流：返回 false 表示超频，应丢弃该消息
func (s *Session) AllowBattleMsg(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if elapsed := now.Sub(s.battleRefillAt).Seconds(); elapsed > 0 {
		refill := elapsed * float64(battleMsgCapacity) / battleMsgWindow.Seconds()
		s.battleTokens = math.Min(float64(battleMsgCapacity), s.battleTokens+refill)
		s.battleRefillAt = now
	}
	if s.battleTokens < 1 {
		return false
	}
	s.battleTokens--
	return true
}

// UpdateRecvTime 更新最后接收时间（收到任何包的时候调用，重置心跳计时）
func (s *Session) UpdateRecvTime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastRecvAt = time.Now()
}

// IsTimeout 判断会话是否心跳超时
func (s *Session) IsTimeout(timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastRecvAt) > timeout
}

// WriteMsg 给客户端发协议包
func (s *Session) WriteMsg(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Write(data)
	return err
}

// Close 关闭会话
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		_ = s.conn.Close()
		close(s.closeChan)
	})
}

// Closed 返回关闭信号通道
func (s *Session) Closed() <-chan struct{} {
	return s.closeChan
}
