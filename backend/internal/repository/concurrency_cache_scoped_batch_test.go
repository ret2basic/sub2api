package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// 分桶槽位的批量只读：清掉过期成员后按 账号 → 模型 计数，只返回大于 0 的项。
func TestGetAccountScopedConcurrencyBatch(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Now()
	mr.SetTime(now)
	cache := NewConcurrencyCache(rdb, 15, 0)
	reader, ok := cache.(service.ScopedAccountConcurrencyReader)
	require.True(t, ok, "concurrencyCache 必须实现分桶只读扩展")

	ctx := context.Background()
	fresh := float64(now.Unix())
	stale := float64(now.Add(-time.Hour).Unix())
	add := func(key string, score float64, member string) {
		require.NoError(t, rdb.ZAdd(ctx, key, redis.Z{Score: score, Member: member}).Err())
	}
	add(accountSlotKeyScoped(7, "glm-5.3"), fresh, "a")
	add(accountSlotKeyScoped(7, "glm-5.3"), fresh, "b")
	add(accountSlotKeyScoped(7, "glm-5.3"), stale, "old")
	add(accountSlotKeyScoped(7, "glm-5.3-flash"), fresh, "c")
	add(accountSlotKeyScoped(8, "glm-5.3-flash"), stale, "gone")
	add(accountSlotKey(7), fresh, "account-level-not-counted-here")

	got, err := reader.GetAccountScopedConcurrencyBatch(ctx, []int64{7, 8, 9}, []string{"glm-5.3", "glm-5.3-flash", ""})
	require.NoError(t, err)
	require.Equal(t, map[int64]map[string]int{7: {"glm-5.3": 2, "glm-5.3-flash": 1}}, got)
	left, err := rdb.ZCard(ctx, accountSlotKeyScoped(7, "glm-5.3")).Result()
	require.NoError(t, err)
	require.EqualValues(t, 2, left, "过期成员要被清掉："+strconv.FormatInt(left, 10))

	empty, err := reader.GetAccountScopedConcurrencyBatch(ctx, nil, []string{"glm-5.3"})
	require.NoError(t, err)
	require.Empty(t, empty)
}
