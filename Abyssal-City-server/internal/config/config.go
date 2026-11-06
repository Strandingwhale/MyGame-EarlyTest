// 配置表加载与查询：启动时读入 config.pb（由 cmd/configtool 从 Excel 生成），供战斗逻辑查询。
package config

import (
	"fmt"
	"os"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
)

var (
	mu        sync.RWMutex
	operators = map[uint32]*api.OperatorCfg{}
	levels    = map[uint32]*api.LevelCfg{}
)

// Load 读取并解析配置文件；失败时保留原有数据，调用方决定是否回落默认配置
func Load(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取配置文件失败: %w", err)
	}
	var set api.ConfigSet
	if err := proto.Unmarshal(data, &set); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	ops := make(map[uint32]*api.OperatorCfg, len(set.Operators))
	for _, op := range set.Operators {
		ops[op.Id] = op
	}
	lvs := make(map[uint32]*api.LevelCfg, len(set.Levels))
	for _, lv := range set.Levels {
		lvs[lv.Id] = lv
	}

	mu.Lock()
	operators = ops
	levels = lvs
	mu.Unlock()
	return nil
}

// GetOperator 按主键查询干员配置；ok=false 表示配置表中没有该 id
func GetOperator(id uint32) (*api.OperatorCfg, bool) {
	mu.RLock()
	defer mu.RUnlock()
	op, ok := operators[id]
	return op, ok
}

// GetLevel 按主键查询关卡配置；ok=false 表示配置表中没有该 id
func GetLevel(id uint32) (*api.LevelCfg, bool) {
	mu.RLock()
	defer mu.RUnlock()
	lv, ok := levels[id]
	return lv, ok
}

// Counts 返回已加载的干员数与关卡数，用于启动日志
func Counts() (int, int) {
	mu.RLock()
	defer mu.RUnlock()
	return len(operators), len(levels)
}
