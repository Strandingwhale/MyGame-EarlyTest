// 配置表工具：读取 config/*.xlsx，把每行转成 protobuf 消息并输出 config.pb，供服务启动时加载。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"
	"google.golang.org/protobuf/proto"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/api"
)

var (
	dir = flag.String("dir", "config", "Excel 配置目录")
	out = flag.String("out", "config/config.pb", "输出的 .pb 文件路径")
)

func main() {
	flag.Parse()

	operators, err := readOperators(filepath.Join(*dir, "operator.xlsx"))
	if err != nil {
		fmt.Println("解析干员表失败:", err)
		os.Exit(1)
	}
	levels, err := readLevels(filepath.Join(*dir, "level.xlsx"))
	if err != nil {
		fmt.Println("解析关卡表失败:", err)
		os.Exit(1)
	}

	data, err := proto.Marshal(&api.ConfigSet{Operators: operators, Levels: levels})
	if err != nil {
		fmt.Println("序列化配置失败:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Println("写入配置文件失败:", err)
		os.Exit(1)
	}
	fmt.Printf("已生成 %s：干员 %d 条，关卡 %d 条\n", *out, len(operators), len(levels))
}

// readOperators 解析 operator.xlsx：id | name | cost | attack | range | hp
func readOperators(path string) ([]*api.OperatorCfg, error) {
	rows, header, err := readSheet(path)
	if err != nil {
		return nil, err
	}

	var list []*api.OperatorCfg
	for i := 1; i < len(rows); i++ {
		if isEmptyRow(rows[i]) {
			continue
		}
		id, err := intCell(rows[i], header, "id")
		if err != nil {
			return nil, rowErr(path, i, err)
		}
		if id <= 0 {
			return nil, rowErr(path, i, fmt.Errorf("id 必须大于 0"))
		}
		op := &api.OperatorCfg{Id: uint32(id), Name: strCell(rows[i], header, "name")}
		if op.Cost, err = int32Cell(rows[i], header, "cost"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if op.Attack, err = int32Cell(rows[i], header, "attack"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if op.Range, err = floatCell(rows[i], header, "range"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if op.Hp, err = int32Cell(rows[i], header, "hp"); err != nil {
			return nil, rowErr(path, i, err)
		}
		list = append(list, op)
	}
	return list, nil
}

// readLevels 解析 level.xlsx：id | waves | enemy_hp | enemy_speed | enemy_count | wave_interval
func readLevels(path string) ([]*api.LevelCfg, error) {
	rows, header, err := readSheet(path)
	if err != nil {
		return nil, err
	}

	var list []*api.LevelCfg
	for i := 1; i < len(rows); i++ {
		if isEmptyRow(rows[i]) {
			continue
		}
		id, err := intCell(rows[i], header, "id")
		if err != nil {
			return nil, rowErr(path, i, err)
		}
		if id <= 0 {
			return nil, rowErr(path, i, fmt.Errorf("id 必须大于 0"))
		}
		lv := &api.LevelCfg{Id: uint32(id)}
		if lv.Waves, err = int32Cell(rows[i], header, "waves"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if lv.EnemyHp, err = int32Cell(rows[i], header, "enemy_hp"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if lv.EnemySpeed, err = floatCell(rows[i], header, "enemy_speed"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if lv.EnemyCount, err = int32Cell(rows[i], header, "enemy_count"); err != nil {
			return nil, rowErr(path, i, err)
		}
		if lv.WaveInterval, err = floatCell(rows[i], header, "wave_interval"); err != nil {
			return nil, rowErr(path, i, err)
		}
		list = append(list, lv)
	}
	return list, nil
}

// readSheet 读取第一个工作表的全部行，并返回第一行表头（字段名小写）到列下标的映射
func readSheet(path string) ([][]string, map[string]int, error) {
	f, err := excelize.OpenFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	rows, err := f.GetRows(f.GetSheetName(0))
	if err != nil {
		return nil, nil, fmt.Errorf("读取 %s 失败: %w", path, err)
	}
	if len(rows) < 2 {
		return nil, nil, fmt.Errorf("%s 没有数据行", path)
	}

	header := make(map[string]int, len(rows[0]))
	for i, name := range rows[0] {
		header[strings.ToLower(strings.TrimSpace(name))] = i
	}
	return rows, header, nil
}

func isEmptyRow(row []string) bool {
	for _, v := range row {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

func rowErr(path string, row int, err error) error {
	return fmt.Errorf("%s 第 %d 行: %w", path, row+1, err)
}

// cell 取指定列的值；列不存在时返回空串
func cell(row []string, header map[string]int, name string) string {
	i, ok := header[name]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func strCell(row []string, header map[string]int, name string) string {
	return cell(row, header, name)
}

func intCell(row []string, header map[string]int, name string) (int64, error) {
	s := cell(row, header, name)
	if s == "" {
		return 0, fmt.Errorf("缺少字段 %s", name)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("字段 %s=%q 不是整数", name, s)
	}
	return v, nil
}

func int32Cell(row []string, header map[string]int, name string) (int32, error) {
	v, err := intCell(row, header, name)
	return int32(v), err
}

func floatCell(row []string, header map[string]int, name string) (float32, error) {
	s := cell(row, header, name)
	if s == "" {
		return 0, fmt.Errorf("缺少字段 %s", name)
	}
	v, err := strconv.ParseFloat(s, 32)
	if err != nil {
		return 0, fmt.Errorf("字段 %s=%q 不是数字", name, s)
	}
	return float32(v), nil
}
