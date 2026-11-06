// 极简关卡配置：一条直线路径 + 按波次出怪
package battle

import (
	"math"

	"github.com/Strandingwhale/Abyssal-City/Abyssal-City-server/internal/config"
)

// Vec2 世界坐标，1 单位 = 1 格
type Vec2 struct {
	X, Y float64
}

const (
	mapWidth  = 9   // 地图宽（格）
	mapHeight = 2   // 地图高（格）
	pathY     = 1.0 // 敌人路径所在的行（第 2 行），干员部署在第 1 行
)

// enemyPath 敌人行进路线：第 2 行从左到右的一条直线
var enemyPath = []Vec2{{0, pathY}, {mapWidth - 1, pathY}}

// Wave 一波敌人的配置
type Wave struct {
	Count       int     // 本波敌人数量
	HP          int32   // 每只敌人血量
	Speed       float64 // 移动速度（格/秒）
	IntervalSec float64 // 同波内相邻两只的出场间隔（秒）
}

// waves 默认波次表：配置表缺失或关卡不存在时的兜底
var waves = []Wave{
	{Count: 3, HP: 100, Speed: 1.2, IntervalSec: 2.0},
	{Count: 5, HP: 120, Speed: 1.4, IntervalSec: 1.5},
}

// wavesForLevel 按关卡配置生成波次表；配置缺失时回落默认 waves
func wavesForLevel(levelID uint32) []Wave {
	lv, ok := config.GetLevel(levelID)
	if !ok || lv.Waves <= 0 {
		return waves
	}
	out := make([]Wave, 0, lv.Waves)
	for i := int32(0); i < lv.Waves; i++ {
		out = append(out, Wave{
			Count:       int(lv.EnemyCount),
			HP:          lv.EnemyHp,
			Speed:       float64(lv.EnemySpeed),
			IntervalSec: float64(lv.WaveInterval),
		})
	}
	return out
}

// 关卡数值（硬编码默认值，配置表缺失时兜底）
const (
	enemyConfigID = 1   // 敌人配置ID（下发 EnemyId 用）
	baseMaxHP     = 1   // 基地生命，漏一只怪即失败
	operatorMaxHP = 100 // 干员血量（当前不会被攻击，仅展示）
	attackRange   = 2.5 // 干员攻击半径（格）
	attackDamage  = 12  // 干员每帧伤害
	deployCost    = 4   // 部署干员消耗
	initCost      = 10  // 开局费用
	maxOperators  = 1   // 场上干员上限

	retreatRefund       = deployCost / 2 // 撤回干员返还的费用
	skillCost           = 3              // 释放技能消耗的费用
	skillCooldownFrames = 15             // 技能冷却（帧）
	skillDamage         = 60             // 技能对射程内敌人造成的一次性伤害
)

// pathLength 路径总长
func pathLength() float64 {
	total := 0.0
	for i := 1; i < len(enemyPath); i++ {
		a, b := enemyPath[i-1], enemyPath[i]
		total += math.Hypot(b.X-a.X, b.Y-a.Y)
	}
	return total
}

// pointAtDistance 按已行进距离求路径上的坐标；超出总长时返回终点
func pointAtDistance(d float64) Vec2 {
	for i := 1; i < len(enemyPath); i++ {
		a, b := enemyPath[i-1], enemyPath[i]
		seg := math.Hypot(b.X-a.X, b.Y-a.Y)
		if seg == 0 {
			return a
		}
		if d <= seg {
			t := d / seg
			return Vec2{X: a.X + (b.X-a.X)*t, Y: a.Y + (b.Y-a.Y)*t}
		}
		d -= seg
	}
	return enemyPath[len(enemyPath)-1]
}
