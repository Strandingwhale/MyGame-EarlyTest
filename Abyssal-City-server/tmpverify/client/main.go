// 端到端验证客户端：建库建表（-setup）或走网关完成 注册 → 登录 → 心跳 全链路校验
package main

import (
	"bytes"
	"context"
	"database/sql"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql" // 注册 database/sql 驱动
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
)

const (
	msgPing     uint16 = 0
	msgLoginReq uint16 = 1001
	msgLoginRsp uint16 = 1002
	msgRegReq   uint16 = 1003
	msgRegRsp   uint16 = 1004
)

var (
	gwAddr   = flag.String("gw", "127.0.0.1:8888", "网关地址")
	dbDSN    = flag.String("dsn", "root:fox114514@tcp(127.0.0.1:3306)/", "MySQL DSN（不含库名）")
	dbName   = flag.String("db", "abyssal_city", "库名")
	redisAdr = flag.String("redis", "127.0.0.1:6379", "Redis 地址")
	schema   = flag.String("schema", "db/schema.sql", "建表 SQL 路径")
	setup    = flag.Bool("setup", false, "只执行建库建表")
)

func main() {
	flag.Parse()
	if *setup {
		must(setupDB())
		fmt.Println("[setup] 建库建表完成")
		return
	}
	must(e2e())
}

// setupDB 建库并建表，供首次联调使用
func setupDB() error {
	root, err := sql.Open("mysql", *dbDSN+"?parseTime=true")
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Exec("CREATE DATABASE IF NOT EXISTS " + *dbName + " CHARACTER SET utf8mb4"); err != nil {
		return fmt.Errorf("create database: %w", err)
	}

	raw, err := os.ReadFile(*schema)
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", *dbDSN+*dbName+"?parseTime=true")
	if err != nil {
		return err
	}
	defer db.Close()
	for _, stmt := range strings.Split(string(raw), ";") {
		var lines []string
		for _, line := range strings.Split(stmt, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				lines = append(lines, line)
			}
		}
		stmt = strings.TrimSpace(strings.Join(lines, "\n"))
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("exec schema: %w", err)
		}
	}
	return nil
}

// e2e 依次走 注册 → 登录 → 心跳，并回查 MySQL / Redis 落库结果
func e2e() error {
	account := fmt.Sprintf("verify_%d", time.Now().UnixNano())
	password := "Passw0rd!verify"

	conn, err := net.DialTimeout("tcp", *gwAddr, 3*time.Second)
	if err != nil {
		return fmt.Errorf("连接网关: %w", err)
	}
	defer conn.Close()
	c := &client{conn: conn, buf: &bytes.Buffer{}}
	fmt.Printf("[1] 已连接网关 %s\n", *gwAddr)

	// --- 注册 ---
	if err := c.send(msgRegReq, &api.RegisterReq{Account: account, Password: password}); err != nil {
		return err
	}
	p, err := c.recv(msgRegRsp)
	if err != nil {
		return err
	}
	var reg api.RegisterResp
	if err := proto.Unmarshal(p.Payload, &reg); err != nil {
		return err
	}
	if reg.Code != 0 {
		return fmt.Errorf("注册失败 code=%d msg=%s", reg.Code, reg.Msg)
	}
	fmt.Printf("[2] 注册成功 account=%s\n", account)

	// --- 登录 ---
	if err := c.send(msgLoginReq, &api.LoginReq{Account: account, Password: password}); err != nil {
		return err
	}
	p, err = c.recv(msgLoginRsp)
	if err != nil {
		return err
	}
	var login api.LoginResp
	if err := proto.Unmarshal(p.Payload, &login); err != nil {
		return err
	}
	if login.Code != 0 || login.Token == "" || login.Uid == 0 {
		return fmt.Errorf("登录失败 code=%d msg=%s", login.Code, login.Msg)
	}
	fmt.Printf("[3] 登录成功 uid=%d token=%s\n", login.Uid, login.Token)

	// --- 心跳 ---
	ts := time.Now().UnixMilli()
	if err := c.send(msgPing, &api.PingReq{Timestamp: ts}); err != nil {
		return err
	}
	p, err = c.recv(1)
	if err != nil {
		return err
	}
	var pong api.PingResp
	if err := proto.Unmarshal(p.Payload, &pong); err != nil {
		return err
	}
	if pong.Timestamp != ts {
		return fmt.Errorf("Pong 时间戳不符: 发出 %d 收到 %d", ts, pong.Timestamp)
	}
	fmt.Printf("[4] 心跳 Pong 正常 timestamp=%d\n", pong.Timestamp)

	// --- 回查 MySQL ---
	db, err := sql.Open("mysql", *dbDSN+*dbName+"?parseTime=true")
	if err != nil {
		return err
	}
	defer db.Close()
	var hash string
	if err := db.QueryRow("SELECT password_hash FROM account WHERE account = ?", account).Scan(&hash); err != nil {
		return fmt.Errorf("查询账号: %w", err)
	}
	if !strings.HasPrefix(hash, "$2") {
		return fmt.Errorf("密码未按 bcrypt 存储: %s", hash)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return fmt.Errorf("bcrypt 校验不通过")
	}
	var uidInDB uint64
	if err := db.QueryRow("SELECT id FROM account WHERE account = ?", account).Scan(&uidInDB); err != nil {
		return err
	}
	if uidInDB != uint64(login.Uid) {
		return fmt.Errorf("uid 不一致: 回包 %d 库内 %d", login.Uid, uidInDB)
	}
	var level, stamina, exp, gold int
	if err := db.QueryRow(
		"SELECT level, stamina, exp, gold FROM player WHERE uid = ?", uidInDB,
	).Scan(&level, &stamina, &exp, &gold); err != nil {
		return fmt.Errorf("查询角色: %w", err)
	}
	if int32(level) != login.Level || int32(stamina) != login.Stamina ||
		int32(exp) != login.Exp || int32(gold) != login.Gold {
		return fmt.Errorf("登录回包角色数据不符: 回包 level=%d stamina=%d exp=%d gold=%d，库内 %d/%d/%d/%d",
			login.Level, login.Stamina, login.Exp, login.Gold, level, stamina, exp, gold)
	}
	fmt.Printf("[5] MySQL 校验通过 hash=%s… uid=%d role level=%d stamina=%d exp=%d gold=%d\n",
		hash[:7], uidInDB, level, stamina, exp, gold)

	// --- 回查 Redis ---
	rdb := redis.NewClient(&redis.Options{Addr: *redisAdr})
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	val, err := rdb.Get(ctx, "token:"+login.Token).Result()
	if err != nil {
		return fmt.Errorf("Redis 读取 token: %w", err)
	}
	ttl, _ := rdb.TTL(ctx, "token:"+login.Token).Result()
	if val != fmt.Sprintf("%d", login.Uid) {
		return fmt.Errorf("token 值不符: %s != %d", val, login.Uid)
	}
	fmt.Printf("[6] Redis 校验通过 token→uid=%s ttl=%s\n", val, ttl.Round(time.Second))
	fmt.Println("全链路验证通过")
	return nil
}

type client struct {
	conn net.Conn
	buf  *bytes.Buffer
}

// 发送消息给端口
func (c *client) send(msgID uint16, msg proto.Message) error {
	body, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	pkt, err := common.Encode(msgID, 0, body)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(pkt)
	return err
}

// 接收消息
func (c *client) recv(wantMsgID uint16) (*common.Packet, error) {
	tmp := make([]byte, 4096)
	deadline := time.Now().Add(5 * time.Second) //防止超时
	for time.Now().Before(deadline) {
		p, err := common.Decode(c.buf)
		if err != nil {
			return nil, err
		}
		if p != nil {
			if p.MessageID != wantMsgID {
				return nil, fmt.Errorf("期望 msgId=%d，收到 %d", wantMsgID, p.MessageID)
			}
			return p, nil
		}
		_ = c.conn.SetReadDeadline(deadline)
		n, err := c.conn.Read(tmp)
		if err != nil {
			return nil, fmt.Errorf("等待 msgId=%d 超时/失败: %w", wantMsgID, err)
		}
		c.buf.Write(tmp[:n])
	}
	return nil, fmt.Errorf("等待 msgId=%d 超时", wantMsgID)
}

// 错误调用
func must(err error) {
	if err != nil {
		fmt.Println("失败:", err)
		os.Exit(1)
	}
}
