package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// zhipuSessionCacheStub 是会话登记表的内存版：每个号一组会话，满了拒绝新会话。
type zhipuSessionCacheStub struct {
	SessionLimitCache

	sessions map[int64]map[string]bool
	idle     map[int64]time.Duration
	err      error
}

func newZhipuSessionCacheStub() *zhipuSessionCacheStub {
	return &zhipuSessionCacheStub{sessions: map[int64]map[string]bool{}, idle: map[int64]time.Duration{}}
}

func (c *zhipuSessionCacheStub) RegisterSession(_ context.Context, accountID int64, sessionUUID string, maxSessions int, idleTimeout time.Duration) (bool, error) {
	if c.err != nil {
		return false, c.err
	}
	c.idle[accountID] = idleTimeout
	set := c.sessions[accountID]
	if set == nil {
		set = map[string]bool{}
		c.sessions[accountID] = set
	}
	if set[sessionUUID] {
		return true, nil
	}
	if len(set) >= maxSessions {
		return false, nil
	}
	set[sessionUUID] = true
	return true, nil
}

func zhipuCapAccount(id int64, maxSessions int) *Account {
	return &Account{ID: id, Platform: PlatformZhipu, Type: AccountTypeAPIKey,
		Extra: map[string]any{"max_sessions": maxSessions, "session_idle_timeout_minutes": 15}}
}

// fakeSelector 按顺序给出第一个没被排除的号，并记录每次选号时的排除集和释放次数。
type fakeSelector struct {
	accounts []*Account
	released map[int64]int
	calls    []map[int64]struct{}
}

func (f *fakeSelector) pick(excluded map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	snapshot := map[int64]struct{}{}
	for id := range excluded {
		snapshot[id] = struct{}{}
	}
	f.calls = append(f.calls, snapshot)
	for _, acc := range f.accounts {
		if _, skip := excluded[acc.ID]; skip {
			continue
		}
		id := acc.ID
		return &AccountSelectionResult{Account: acc, Acquired: true, ReleaseFunc: func() { f.released[id]++ }}, OpenAIAccountScheduleDecision{}, nil
	}
	return nil, OpenAIAccountScheduleDecision{}, ErrNoAvailableAccounts
}

func TestZhipuSessionCapSkipsFullAccountAndKeepsCallerExclusions(t *testing.T) {
	cache := newZhipuSessionCacheStub()
	svc := &OpenAIGatewayService{}
	svc.SetSessionLimitCache(cache)
	a, b := zhipuCapAccount(1, 1), zhipuCapAccount(2, 1)
	sel := &fakeSelector{accounts: []*Account{a, b}, released: map[int64]int{}}
	ctx := context.Background()

	// 会话 s1 落到 1 号并登记。
	got, _, err := svc.admitZhipuSessionSelection(ctx, "s1", nil, sel.pick)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Account.ID)
	require.Equal(t, 15*time.Minute, cache.idle[1])

	// s1 再来还在 1 号（已登记的会话照常通过）。
	got, _, err = svc.admitZhipuSessionSelection(ctx, "s1", nil, sel.pick)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Account.ID)

	// 新会话 s2：1 号满了，释放槽位、换到 2 号；调用方的排除集不被改动。
	callerExcluded := map[int64]struct{}{99: {}}
	got, _, err = svc.admitZhipuSessionSelection(ctx, "s2", callerExcluded, sel.pick)
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Account.ID)
	require.Equal(t, 1, sel.released[1], "slot on the full account must be released")
	require.Equal(t, map[int64]struct{}{99: {}}, callerExcluded)
	last := sel.calls[len(sel.calls)-1]
	require.Contains(t, last, int64(1))
	require.Contains(t, last, int64(99))

	// 新会话 s3：两个号都满，返回会话上限错误（仍是 ErrNoAvailableAccounts）。
	_, _, err = svc.admitZhipuSessionSelection(ctx, "s3", nil, sel.pick)
	require.ErrorIs(t, err, errZhipuSessionCapReached)
	require.True(t, errors.Is(err, ErrNoAvailableAccounts))
	require.Equal(t, 2, sel.released[1])
	require.Equal(t, 1, sel.released[2])
}

func TestZhipuSessionCapLeavesOtherAccountsAlone(t *testing.T) {
	cache := newZhipuSessionCacheStub()
	svc := &OpenAIGatewayService{}
	svc.SetSessionLimitCache(cache)
	ctx := context.Background()

	grok := &Account{ID: 5, Platform: PlatformGrok, Extra: map[string]any{"max_sessions": 1}}
	noCap := &Account{ID: 6, Platform: PlatformZhipu}
	for _, acc := range []*Account{grok, noCap} {
		for _, sess := range []string{"a", "b", "c"} {
			require.True(t, svc.admitZhipuSession(ctx, acc, sess))
		}
	}
	require.Empty(t, cache.sessions, "accounts without a zhipu cap must not be registered")

	// 没有会话标识（如 python-httpx 直调）不受限，也不登记。
	capped := zhipuCapAccount(7, 1)
	require.True(t, svc.admitZhipuSession(ctx, capped, ""))
	require.Empty(t, cache.sessions)

	// 没注入登记表时放行。
	require.True(t, (&OpenAIGatewayService{}).admitZhipuSession(ctx, capped, "x"))

	// Redis 出错时放行（与 Anthropic 会话上限一致）。
	cache.err = errors.New("redis down")
	require.True(t, svc.admitZhipuSession(ctx, capped, "y"))
}

func TestZhipuSessionCapPassesThroughSelectionErrors(t *testing.T) {
	svc := &OpenAIGatewayService{}
	svc.SetSessionLimitCache(newZhipuSessionCacheStub())
	boom := errors.New("pricing restriction")
	_, _, err := svc.admitZhipuSessionSelection(context.Background(), "s", nil,
		func(map[int64]struct{}) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
			return nil, OpenAIAccountScheduleDecision{}, boom
		})
	require.ErrorIs(t, err, boom, "first-pass errors are returned unchanged")
}
