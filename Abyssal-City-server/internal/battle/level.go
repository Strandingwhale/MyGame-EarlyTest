// 极简关卡配置：一条直线路径 + 一只怪
package battle

import "math"

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

// 关卡数值（硬编码，暂不读配置表）
const (
	enemyConfigID = 1    // 敌人配置ID
	enemyMaxHP    = 100  // 敌人血量
	enemySpeed    = 1.2  // 敌人速度（格/秒）
	baseMaxHP     = 1    // 基地生命，漏一只怪即失败
	operatorMaxHP = 100  // 干员血量（当前不会被攻击，仅展示）
	attackRange   = 2.5  // 干员攻击半径（格）
	attackDamage  = 12   // 干员每帧伤害
	deployCost    = 4    // 部署干员消耗
	initCost      = 10   // 开局费用
	maxOperators  = 1    // 场上干员上限
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

// distanceAtPoint 求路径上距 p 最近点对应的已行进距离（pointAtDistance 的逆，用于从快照坐标恢复 Dist）
func distanceAtPoint(p Vec2) float64 {
	best := 0.0
	bestD := math.Inf(1)
	acc := 0.0
	for i := 1; i < len(enemyPath); i++ {
		a, b := enemyPath[i-1], enemyPath[i]
		dx, dy := b.X-a.X, b.Y-a.Y
		seg := math.Hypot(dx, dy)
		t := 0.0
		if seg > 0 {
			t = ((p.X-a.X)*dx + (p.Y-a.Y)*dy) / (seg * seg)
			t = math.Max(0, math.Min(1, t))
		}
		qx, qy := a.X+t*dx, a.Y+t*dy
		if d := math.Hypot(p.X-qx, p.Y-qy); d < bestD {
			bestD = d
			best = acc + t*seg
		}
		acc += seg
	}
	return best
}
