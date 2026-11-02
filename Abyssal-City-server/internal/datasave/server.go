// DataSave 服务：内部服务，接收其他服务（如 battle）的写库请求。
// 内部协议沿用 common.Encode/Decode 的包格式，负载用 JSON（不对客户端暴露，省去 proto 代码生成）。
package datasave

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
)

// ListenAddr DataSave 服务监听地址
const ListenAddr = ":9997"

// 内部协议号（3000+，与客户端协议错开）；回包号 = 请求号 + 100
const (
	MsgSaveBattleRecord uint16 = 3000 // 写战斗记录并累加 exp/gold
	MsgDeductStamina    uint16 = 3001 // 扣体力
	MsgGetRole          uint16 = 3002 // 查角色

	msgSaveBattleRecordResp uint16 = 3100
	msgDeductStaminaResp    uint16 = 3101
	msgGetRoleResp          uint16 = 3102
)

var logger = common.NewLogger("datasave")

// 回包错误码
const (
	codeOK               int32 = 0
	codeBadParam         int32 = 1 // 参数缺失/格式错误
	codeNotFound         int32 = 2 // 玩家不存在
	codeServerError      int32 = 3 // 服务器内部错误
	codeStaminaNotEnough int32 = 4 // 体力不足
)

// SaveBattleRecordReq 3000 写战斗记录请求
type SaveBattleRecordReq struct {
	UID         uint64   `json:"uid"`
	Result      int32    `json:"result"` // 0 胜 / 1 败
	Score       int32    `json:"score"`
	DurationSec int32    `json:"duration_sec"`
	Rewards     []string `json:"rewards"` // 如 "exp:100"、"gold:500"
}

// SaveBattleRecordResp 3100 写战斗记录回包
type SaveBattleRecordResp struct {
	Code     int32  `json:"code"`
	Msg      string `json:"msg"`
	ExpGain  int32  `json:"exp_gain"`
	GoldGain int32  `json:"gold_gain"`
}

// DeductStaminaReq 3001 扣体力请求
type DeductStaminaReq struct {
	UID    uint64 `json:"uid"`
	Amount int32  `json:"amount"`
}

// DeductStaminaResp 3101 扣体力回包
type DeductStaminaResp struct {
	Code      int32  `json:"code"`
	Msg       string `json:"msg"`
	Remaining int32  `json:"remaining"`
}

// GetRoleReq 3002 查角色请求
type GetRoleReq struct {
	UID uint64 `json:"uid"`
}

// GetRoleResp 3102 查角色回包
type GetRoleResp struct {
	Code    int32  `json:"code"`
	Msg     string `json:"msg"`
	UID     uint64 `json:"uid"`
	Name    string `json:"name"`
	Level   int32  `json:"level"`
	Stamina int32  `json:"stamina"`
	Exp     int32  `json:"exp"`
	Gold    int32  `json:"gold"`
}

type server struct {
	db *sql.DB
}

// Serve 接受连接并处理内部请求；阻塞运行
func Serve(db *sql.DB, listener net.Listener) {
	s := &server{db: db}
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go s.handleConn(conn)
	}
}

// handleConn 解码、按 msgId 分发、回包
func (s *server) handleConn(conn net.Conn) {
	defer conn.Close()
	logger.Println("服务接入datasave服务")
	readBuf := make([]byte, 4096)
	cache := &bytes.Buffer{}

	for {
		n, err := conn.Read(readBuf)
		if err != nil {
			logger.Println("连接断开：", err)
			return
		}
		cache.Write(readBuf[:n])

		for {
			p, err := common.Decode(cache)
			if err != nil {
				logger.Println("datasave decode err:", err)
				return
			}
			if p == nil {
				break
			}

			var respMsgID uint16
			var resp any
			switch p.MessageID {
			case MsgSaveBattleRecord:
				respMsgID, resp = msgSaveBattleRecordResp, s.handleSaveBattleRecord(p.Payload)
			case MsgDeductStamina:
				respMsgID, resp = msgDeductStaminaResp, s.handleDeductStamina(p.Payload)
			case MsgGetRole:
				respMsgID, resp = msgGetRoleResp, s.handleGetRole(p.Payload)
			default:
				logger.Printf("未知内部消息 msgId=%d，丢弃", p.MessageID)
				continue
			}

			respBody, err := json.Marshal(resp)
			if err != nil {
				logger.Println("序列化回包失败:", err)
				continue
			}
			// 把请求方的 connID 原样填回去，方便它匹配回包
			pkt, err := common.Encode(respMsgID, p.ConnId, respBody)
			if err != nil {
				logger.Println("编码回包失败:", err)
				continue
			}
			if _, err := conn.Write(pkt); err != nil {
				logger.Println("回包写入失败:", err)
				return
			}
		}
	}
}

// handleSaveBattleRecord 写一条战斗记录，并在同一事务里累加玩家 exp/gold
func (s *server) handleSaveBattleRecord(payload []byte) *SaveBattleRecordResp {
	var req SaveBattleRecordReq
	if err := json.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 SaveBattleRecordReq 失败:", err)
		return &SaveBattleRecordResp{Code: codeBadParam, Msg: "请求格式错误"}
	}
	if req.UID == 0 {
		return &SaveBattleRecordResp{Code: codeBadParam, Msg: "uid 不能为 0"}
	}

	expGain, goldGain := parseRewards(req.Rewards)

	tx, err := s.db.Begin()
	if err != nil {
		logger.Println("开启事务失败:", err)
		return &SaveBattleRecordResp{Code: codeServerError, Msg: "服务器繁忙"}
	}
	defer tx.Rollback()

	// 先确认玩家存在，避免外键失败时错误信息含糊
	var exist int
	if err := tx.QueryRow("SELECT 1 FROM player WHERE uid = ?", req.UID).Scan(&exist); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			logger.Printf("写战斗记录失败：玩家 uid=%d 不存在", req.UID)
			return &SaveBattleRecordResp{Code: codeNotFound, Msg: "玩家不存在"}
		}
		logger.Println("查询玩家失败:", err)
		return &SaveBattleRecordResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	rewards := strings.Join(req.Rewards, ",")
	if _, err := tx.Exec(
		"INSERT INTO battle_record (uid, result, score, duration_sec, rewards) VALUES (?, ?, ?, ?, ?)",
		req.UID, req.Result, req.Score, req.DurationSec, rewards,
	); err != nil {
		logger.Println("写入战斗记录失败:", err)
		return &SaveBattleRecordResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	if _, err := tx.Exec(
		"UPDATE player SET exp = exp + ?, gold = gold + ? WHERE uid = ?",
		expGain, goldGain, req.UID,
	); err != nil {
		logger.Println("累加玩家奖励失败:", err)
		return &SaveBattleRecordResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	if err := tx.Commit(); err != nil {
		logger.Println("提交事务失败:", err)
		return &SaveBattleRecordResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	logger.Printf("战斗记录落库：uid=%d result=%d score=%d duration=%ds exp+%d gold+%d",
		req.UID, req.Result, req.Score, req.DurationSec, expGain, goldGain)
	return &SaveBattleRecordResp{Code: codeOK, Msg: "ok", ExpGain: expGain, GoldGain: goldGain}
}

// handleDeductStamina 原子扣体力：stamina >= amount 才更新，不足则回包剩余体力
func (s *server) handleDeductStamina(payload []byte) *DeductStaminaResp {
	var req DeductStaminaReq
	if err := json.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 DeductStaminaReq 失败:", err)
		return &DeductStaminaResp{Code: codeBadParam, Msg: "请求格式错误"}
	}
	if req.UID == 0 || req.Amount <= 0 {
		return &DeductStaminaResp{Code: codeBadParam, Msg: "uid 或扣减数量不合法"}
	}

	res, err := s.db.Exec(
		"UPDATE player SET stamina = stamina - ? WHERE uid = ? AND stamina >= ?",
		req.Amount, req.UID, req.Amount,
	)
	if err != nil {
		logger.Println("扣除体力失败:", err)
		return &DeductStaminaResp{Code: codeServerError, Msg: "服务器繁忙"}
	}
	n, err := res.RowsAffected()
	if err != nil {
		logger.Println("读取受影响行数失败:", err)
		return &DeductStaminaResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	if n == 0 {
		var stamina int32
		err := s.db.QueryRow("SELECT stamina FROM player WHERE uid = ?", req.UID).Scan(&stamina)
		if errors.Is(err, sql.ErrNoRows) {
			return &DeductStaminaResp{Code: codeNotFound, Msg: "玩家不存在"}
		}
		if err != nil {
			logger.Println("查询体力失败:", err)
			return &DeductStaminaResp{Code: codeServerError, Msg: "服务器繁忙"}
		}
		return &DeductStaminaResp{Code: codeStaminaNotEnough, Msg: "体力不足", Remaining: stamina}
	}

	var stamina int32
	if err := s.db.QueryRow("SELECT stamina FROM player WHERE uid = ?", req.UID).Scan(&stamina); err != nil {
		logger.Println("查询剩余体力失败:", err)
		return &DeductStaminaResp{Code: codeServerError, Msg: "服务器繁忙"}
	}
	logger.Printf("扣除体力：uid=%d -%d，剩余%d", req.UID, req.Amount, stamina)
	return &DeductStaminaResp{Code: codeOK, Msg: "ok", Remaining: stamina}
}

// handleGetRole 查角色基础信息
func (s *server) handleGetRole(payload []byte) *GetRoleResp {
	var req GetRoleReq
	if err := json.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 GetRoleReq 失败:", err)
		return &GetRoleResp{Code: codeBadParam, Msg: "请求格式错误"}
	}
	if req.UID == 0 {
		return &GetRoleResp{Code: codeBadParam, Msg: "uid 不能为 0"}
	}

	var resp GetRoleResp
	err := s.db.QueryRow(
		"SELECT name, level, stamina, exp, gold FROM player WHERE uid = ?", req.UID,
	).Scan(&resp.Name, &resp.Level, &resp.Stamina, &resp.Exp, &resp.Gold)
	if errors.Is(err, sql.ErrNoRows) {
		return &GetRoleResp{Code: codeNotFound, Msg: "玩家不存在"}
	}
	if err != nil {
		logger.Println("查询角色失败:", err)
		return &GetRoleResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	resp.Code = codeOK
	resp.Msg = "ok"
	resp.UID = req.UID
	return &resp
}

// parseRewards 从 "exp:100"、"gold:500" 形式的奖励描述里解析累加值
func parseRewards(rewards []string) (exp, gold int32) {
	for _, r := range rewards {
		k, v, ok := strings.Cut(r, ":")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "exp":
			exp += int32(n)
		case "gold":
			gold += int32(n)
		}
	}
	return exp, gold
}
