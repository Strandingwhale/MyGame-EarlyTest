// DataSave 服务入口：连 MySQL，监听 9997 处理内部写库请求
package main

import (
	"net"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/common"
	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/datasave"
)

var logger = common.NewLogger("datasave")

func main() {
	db, err := datasave.InitDB("root:fox114514@tcp(127.0.0.1:3306)/abyssal_city?parseTime=true")
	if err != nil {
		logger.Fatal("数据库连接失败：", err)
	}
	logger.Println("数据库连接成功")

	listener, err := net.Listen("tcp", datasave.ListenAddr)
	if err != nil {
		logger.Fatal("datasave服务监听失败:", err)
	}
	logger.Printf("datasave服务启动，监听：%s", datasave.ListenAddr)

	datasave.Serve(db, listener)
}
