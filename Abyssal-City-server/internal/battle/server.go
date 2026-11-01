// 战斗服务：监听网关连接，处理 2000+ 战斗消息。
package battle

import (
	"bytes"
	"net"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
)

const listenAddr = ":9998"

var logger = common.NewLogger("battle")

// 回包错误码
const (
	codeOK       = 0
	codeBadParam = 1 // 参数缺失/格式错误
	codeNotFound = 2 // 房间不存在
)

// Run 启动战斗服务，监听网关连接
func Run() {
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		logger.Fatal("战斗服务监听失败:", err)
	}
	logger.Printf("battle服务启动，监听：%s", listenAddr)
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleGatewayConn(conn)
	}
}

// connWriter 串行化对网关连接的写入：tick 推送与请求回包来自不同 goroutine，加锁避免包交错
type connWriter struct {
	mu   sync.Mutex
	conn net.Conn
}

func (w *connWriter) write(msgID uint16, connID uint64, body []byte) error {
	pkt, err := common.Encode(msgID, connID, body)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.conn.Write(pkt)
	return err
}

// 处理来自网关的连接：解码、按 msgId 分发、回包
func handleGatewayConn(conn net.Conn) {
	defer conn.Close()
	logger.Println("网关接入battle服务")
	readBuf := make([]byte, 4096)
	cache := &bytes.Buffer{}
	cw := &connWriter{conn: conn}

	for {
		n, err := conn.Read(readBuf)
		if err != nil {
			logger.Println("网关连接断开：", err)
			return
		}
		cache.Write(readBuf[:n])

		for {
			p, err := common.Decode(cache)
			if err != nil {
				logger.Println("battle decode err:", err)
				return
			}
			if p == nil {
				break
			}

			// 按 msgId 分发到具体处理函数
			var respMsgID uint16
			var resp proto.Message
			switch p.MessageID {
			case 2000:
				respMsgID, resp = 2001, handleEnterRoom(p.Payload, p.ConnId, cw)
			case 2002:
				respMsgID, resp = 2003, handleDeployOperator(p.Payload)
			case 2006:
				respMsgID, resp = 2007, handleRetreatOperator(p.Payload)
			case 2008:
				respMsgID, resp = 2009, handleCastSkill(p.Payload)
			default:
				logger.Printf("未知战斗消息 msgId=%d，丢弃", p.MessageID)
				continue
			}

			respBody, err := proto.Marshal(resp)
			if err != nil {
				logger.Println("序列化回包失败:", err)
				continue
			}
			// 把网关发来的 connID 原样填回去，网关才能靠它定位客户端
			if err := cw.write(respMsgID, p.ConnId, respBody); err != nil {
				logger.Println("回包写入失败:", err)
				return
			}
		}
	}
}

// handleEnterRoom 创建房间；room_id 非 0 时按重连处理，从快照恢复继续战斗
func handleEnterRoom(payload []byte, connID uint64, cw *connWriter) *api.EnterRoomResp {
	var req api.EnterRoomReq
	if err := proto.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 EnterRoomReq 失败:", err)
		return &api.EnterRoomResp{Code: codeBadParam, Msg: "请求格式错误"}
	}

	// 重连恢复：从最近快照回填状态并重启 tick，战斗从断线时的帧继续（重连即接管，归属校验待接入 uid）
	if req.RoomId != 0 {
		room := getRoom(req.RoomId)
		if room == nil {
			return &api.EnterRoomResp{Code: codeNotFound, Msg: "房间不存在或已结束"}
		}
		if !room.resume(connID, func(msgID uint16, body []byte) {
			if err := cw.write(msgID, connID, body); err != nil {
				logger.Println("推送写入失败:", err)
			}
		}) {
			return &api.EnterRoomResp{Code: codeNotFound, Msg: "房间不存在或已结束"}
		}
		return &api.EnterRoomResp{Code: codeOK, Msg: "ok", RoomId: room.ID, Seed: room.Seed}
	}

	room := createRoom(connID, func(msgID uint16, body []byte) {
		if err := cw.write(msgID, connID, body); err != nil {
			logger.Println("推送写入失败:", err)
		}
	})
	logger.Printf("创建房间 roomId=%d (connID=%d) levelId=%d", room.ID, connID, req.LevelId)
	return &api.EnterRoomResp{Code: codeOK, Msg: "ok", RoomId: room.ID, Seed: room.Seed}
}

// handleDeployOperator 部署干员
func handleDeployOperator(payload []byte) *api.DeployOperatorResp {
	var req api.DeployOperatorReq
	if err := proto.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 DeployOperatorReq 失败:", err)
		return &api.DeployOperatorResp{Code: codeBadParam, Msg: "请求格式错误"}
	}

	room := getRoom(req.RoomId)
	if room == nil {
		return &api.DeployOperatorResp{Code: codeNotFound, Msg: "房间不存在或已结束"}
	}

	code, msg, remaining := room.deploy(req.OperatorId, req.GridX, req.GridY, req.Direction)
	return &api.DeployOperatorResp{
		Code:          code,
		Msg:           msg,
		OperatorId:    req.OperatorId,
		RemainingCost: remaining,
	}
}

// handleRetreatOperator 撤回干员
func handleRetreatOperator(payload []byte) *api.RetreatOperatorResp {
	var req api.RetreatOperatorReq
	if err := proto.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 RetreatOperatorReq 失败:", err)
		return &api.RetreatOperatorResp{Code: codeBadParam, Msg: "请求格式错误"}
	}

	room := getRoom(req.RoomId)
	if room == nil {
		return &api.RetreatOperatorResp{Code: codeNotFound, Msg: "房间不存在或已结束"}
	}

	code, msg, remaining := room.retreat(req.OperatorId)
	return &api.RetreatOperatorResp{
		Code:          code,
		Msg:           msg,
		OperatorId:    req.OperatorId,
		RemainingCost: remaining,
	}
}

// handleCastSkill 释放技能
func handleCastSkill(payload []byte) *api.CastSkillResp {
	var req api.CastSkillReq
	if err := proto.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 CastSkillReq 失败:", err)
		return &api.CastSkillResp{Code: codeBadParam, Msg: "请求格式错误"}
	}

	room := getRoom(req.RoomId)
	if room == nil {
		return &api.CastSkillResp{Code: codeNotFound, Msg: "房间不存在或已结束"}
	}

	code, msg, remaining, damage, hits := room.castSkill(req.OperatorId)
	return &api.CastSkillResp{
		Code:          code,
		Msg:           msg,
		OperatorId:    req.OperatorId,
		RemainingCost: remaining,
		Damage:        damage,
		HitCount:      hits,
	}
}
