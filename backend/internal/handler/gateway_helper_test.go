package handler

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestWrapReleaseOnDone_NoGoroutineLeak 验证 wrapReleaseOnDone 修复后不会泄露 goroutine
func TestWrapReleaseOnDone_NoGoroutineLeak(t *testing.T) {
	// 记录测试开始时的 goroutine 数量
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	initialGoroutines := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 正常释放
	release()

	// 等待足够时间确保 goroutine 退出
	time.Sleep(200 * time.Millisecond)

	// 验证只释放一次
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}

	// 强制 GC，清理已退出的 goroutine
	runtime.GC()
	time.Sleep(100 * time.Millisecond)

	// 验证 goroutine 数量没有增加（允许±2的误差，考虑到测试框架本身可能创建的 goroutine）
	finalGoroutines := runtime.NumGoroutine()
	if finalGoroutines > initialGoroutines+2 {
		t.Errorf("goroutine leak detected: initial=%d, final=%d, leaked=%d",
			initialGoroutines, finalGoroutines, finalGoroutines-initialGoroutines)
	}
}

// TestWrapReleaseOnDone_ContextCancellation 验证 context 取消时也能正确释放
func TestWrapReleaseOnDone_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var releaseCount int32
	_ = wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 取消 context，应该触发释放
	cancel()

	// 等待释放完成
	time.Sleep(100 * time.Millisecond)

	// 验证释放被调用
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}
}

func TestWrapReleaseOnDone_AlreadyCancelledReleasesExactlyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})
	release()

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&releaseCount) == 1
	}, time.Second, time.Millisecond)
}

// TestWrapReleaseOnDone_MultipleCallsOnlyReleaseOnce 验证多次调用 release 只释放一次
func TestWrapReleaseOnDone_MultipleCallsOnlyReleaseOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 调用多次
	release()
	release()
	release()

	// 等待执行完成
	time.Sleep(100 * time.Millisecond)

	// 验证只释放一次
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}
}

// TestOnceRelease_NotBoundToContext 验证 onceRelease 不随客户端 ctx 取消而释放。
// 账号槽必须活到上游排空结束：客户端断连后上游调用是 detach 的
//（WithoutCancel），网关继续排空，槽若提前释放会造成本地并发计数低于上游真实
// 并发（2026-09-18 实测窗口 73~215s）。
func TestOnceRelease_NotBoundToContext(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	var releaseCount int32
	release := onceRelease(func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 客户端断连（ctx 取消）不得释放账号槽
	cancel()
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(0), atomic.LoadInt32(&releaseCount), "客户端 ctx 取消不应释放账号槽")

	// 转发/排空结束后显式释放，且幂等
	release()
	release()
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&releaseCount) == 1
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), atomic.LoadInt32(&releaseCount))
}

func TestOnceRelease_NilSafe(t *testing.T) {
	require.Nil(t, onceRelease(nil))
}

// TestWrapReleaseOnDone_NilReleaseFunc 验证 nil releaseFunc 不会 panic
func TestWrapReleaseOnDone_NilReleaseFunc(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := wrapReleaseOnDone(ctx, nil)

	if release != nil {
		t.Error("expected nil release function when releaseFunc is nil")
	}
}

// TestWrapReleaseOnDone_ConcurrentCalls 验证并发调用的安全性
func TestWrapReleaseOnDone_ConcurrentCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var releaseCount int32
	release := wrapReleaseOnDone(ctx, func() {
		atomic.AddInt32(&releaseCount, 1)
	})

	// 并发调用 release
	const numGoroutines = 10
	for i := 0; i < numGoroutines; i++ {
		go release()
	}

	// 等待所有 goroutine 完成
	time.Sleep(200 * time.Millisecond)

	// 验证只释放一次
	if count := atomic.LoadInt32(&releaseCount); count != 1 {
		t.Errorf("expected release count to be 1, got %d", count)
	}
}

// BenchmarkWrapReleaseOnDone 性能基准测试
func BenchmarkWrapReleaseOnDone(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		release := wrapReleaseOnDone(ctx, func() {})
		release()
	}
}
