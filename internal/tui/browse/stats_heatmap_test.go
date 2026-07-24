package browse

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHeatLevel(t *testing.T) {
	assert.Equal(t, 0, heatLevel(0, 10))
	assert.Equal(t, 0, heatLevel(5, 0))
	assert.Equal(t, 1, heatLevel(2, 10)) // 2*4 <= 10
	assert.Equal(t, 2, heatLevel(5, 10)) // 5*2 <= 10
	assert.Equal(t, 3, heatLevel(7, 10)) // 7*4 <= 30
	assert.Equal(t, 4, heatLevel(10, 10))
}

func TestFoldHeat(t *testing.T) {
	// A fixed "now": Wednesday, 2024-01-17 12:00 local.
	now := time.Date(2024, 1, 17, 12, 0, 0, 0, time.Local)
	nowMs := now.UnixMilli()

	var heat [7][maxHeatWeeks]int

	// Today (Wednesday) lands in the current week (last column), Wed row (3).
	foldHeat(&heat, nowMs, nowMs)
	assert.Equal(t, 1, heat[int(time.Wednesday)][maxHeatWeeks-1])

	// Seven days ago: same weekday, one week left.
	weekAgo := now.AddDate(0, 0, -7).UnixMilli()
	foldHeat(&heat, nowMs, weekAgo)
	assert.Equal(t, 1, heat[int(time.Wednesday)][maxHeatWeeks-2])

	// Yesterday (Tuesday) is still the current week.
	yest := now.AddDate(0, 0, -1).UnixMilli()
	foldHeat(&heat, nowMs, yest)
	assert.Equal(t, 1, heat[int(time.Tuesday)][maxHeatWeeks-1])

	// Far outside the window is ignored (no panic, no placement).
	old := now.AddDate(0, 0, -maxHeatWeeks*7-14).UnixMilli()
	before := heat
	foldHeat(&heat, nowMs, old)
	assert.Equal(t, before, heat, "out-of-window dates are dropped")
}
