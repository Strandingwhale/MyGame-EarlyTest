package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/datasave"
)

var logger = common.NewLogger("login")

var (
	db  *sql.DB
	rdb *redis.Client
	ctx = context.Background()
)

// token 有效期：短期 2 小时
const tokenTTL = 2 * time.Hour

// 回包错误码
const (
	codeOK          = 0
	codeBadParam    = 1 // 参数缺失/格式错误
	codeAccountDup  = 2 // 账号已存在
	codeServerError = 3 // 服务器内部错误
)

func handleGatewayConn(conn net.Conn) {
	defer conn.Close()
	logger.Println("网关接入login服务")
	readBuf := make([]byte, 4096)
	cache := &bytes.Buffer{}

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
				logger.Println("login decode err:", err)
				return
			}
			if p == nil {
				break
			}

			// 按 msgId 分发到具体处理函数
			var respMsgId uint16
			var resp proto.Message
			switch p.MessageID {
			case 1001:
				respMsgId, resp = 1002, handleLogin(p.Payload)
			case 1003:
				respMsgId, resp = 1004, handleRegister(p.Payload)
			default:
				continue
			}

			respBody, err := proto.Marshal(resp)
			if err != nil {
				logger.Println("序列化回包失败:", err)
				continue
			}
			// 把网关发来的 connID 原样填回去，网关才能靠它定位客户端
			respPkt, _ := common.Encode(respMsgId, p.ConnId, respBody)
			_, _ = conn.Write(respPkt)
		}
	}
}

// handleLogin 查库校验密码（bcrypt 比对），成功则签发短期 token 存入 Redis
func handleLogin(payload []byte) *api.LoginResp {
	var req api.LoginReq
	if err := proto.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 LoginReq 失败:", err)
		return &api.LoginResp{Code: codeBadParam, Msg: "请求格式错误"}
	}
	logger.Printf("收到登录请求：account=%s", req.Account)

	var (
		uid  uint64
		hash string
	)
	err := db.QueryRow("SELECT id, password_hash FROM account WHERE account = ?", req.Account).
		Scan(&uid, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return &api.LoginResp{Code: codeBadParam, Msg: "账号密码错误"}
	}
	if err != nil {
		logger.Println("查询账号失败:", err)
		return &api.LoginResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	// bcrypt 比对：不解密哈希，而是用同样的算法验证是否匹配
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) != nil {
		return &api.LoginResp{Code: codeBadParam, Msg: "账号密码错误"}
	}

	token, err := issueToken(uid)
	if err != nil {
		logger.Println("签发 token 失败:", err)
		return &api.LoginResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	// 角色信息：注册时已建 player 行；老账号缺失时按零值返回，不阻断登录
	var role api.LoginResp
	roleErr := db.QueryRow(
		"SELECT level, stamina, exp, gold FROM player WHERE uid = ?", uid,
	).Scan(&role.Level, &role.Stamina, &role.Exp, &role.Gold)
	if roleErr != nil && !errors.Is(roleErr, sql.ErrNoRows) {
		logger.Println("查询角色失败:", roleErr)
		return &api.LoginResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	logger.Printf("登录成功：account=%s uid=%d", req.Account, uid)
	return &api.LoginResp{
		Code:      codeOK,
		Msg:       "ok",
		Uid:       int64(uid),
		Token:     token,
		SessionId: token,
		Level:     role.Level,
		Stamina:   role.Stamina,
		Exp:       role.Exp,
		Gold:      role.Gold,
	}
}

// issueToken 生成 32 位随机 token，以 token→uid 的形式存入 Redis，到期自动删除
func issueToken(uid uint64) (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	err := rdb.Set(ctx, "token:"+token, uid, tokenTTL).Err()
	return token, err
}

// handleRegister 注册：bcrypt 哈希密码后入库，绝不存明文
func handleRegister(payload []byte) *api.RegisterResp {
	var req api.RegisterReq
	if err := proto.Unmarshal(payload, &req); err != nil {
		logger.Println("解析 RegisterReq 失败:", err)
		return &api.RegisterResp{Code: codeBadParam, Msg: "请求格式错误"}
	}
	if req.Account == "" || req.Password == "" {
		return &api.RegisterResp{Code: codeBadParam, Msg: "账号或密码不能为空"}
	}

	// bcrypt 哈希：每次结果不同（自带随机盐），相同密码也无法从哈希看出
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		logger.Println("bcrypt 哈希失败:", err)
		return &api.RegisterResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	// 账号与角色行必须在同一事务里创建，避免只有账号没有角色
	tx, err := db.Begin()
	if err != nil {
		logger.Println("开启事务失败:", err)
		return &api.RegisterResp{Code: codeServerError, Msg: "服务器繁忙"}
	}
	defer tx.Rollback()

	res, err := tx.Exec("INSERT INTO account (account, password_hash) VALUES (?, ?)", req.Account, string(hash))
	if err != nil {
		// 唯一索引冲突 = 账号已存在
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return &api.RegisterResp{Code: codeAccountDup, Msg: "账号已存在"}
		}
		logger.Println("插入账号失败:", err)
		return &api.RegisterResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	uid, err := res.LastInsertId()
	if err != nil {
		logger.Println("读取新账号 uid 失败:", err)
		return &api.RegisterResp{Code: codeServerError, Msg: "服务器繁忙"}
	}
	if _, err := tx.Exec("INSERT INTO player (uid, name) VALUES (?, ?)", uid, req.Account); err != nil {
		logger.Println("创建角色失败:", err)
		return &api.RegisterResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	if err := tx.Commit(); err != nil {
		logger.Println("提交事务失败:", err)
		return &api.RegisterResp{Code: codeServerError, Msg: "服务器繁忙"}
	}

	logger.Printf("注册成功：account=%s uid=%d", req.Account, uid)
	return &api.RegisterResp{Code: codeOK, Msg: "ok"}
}

func main() {
	var err error
	db, err = datasave.InitDB("root:fox114514@tcp(127.0.0.1:3306)/abyssal_city?parseTime=true")
	if err != nil {
		logger.Fatal("数据库连接失败：", err)
	}
	logger.Println("数据库连接成功")

	rdb, err = datasave.InitRedis("127.0.0.1:6379")
	if err != nil {
		logger.Fatal("Redis 连接失败：", err)
	}
	logger.Println("Redis 连接成功")

	listener, err := net.Listen("tcp", ":9999")
	if err != nil {
		panic(err)
	}
	logger.Println("login服务启动，监听：9999")
	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleGatewayConn(conn)
	}
}
