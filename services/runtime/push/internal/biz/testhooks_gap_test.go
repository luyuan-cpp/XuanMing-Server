// testhooks_gap_test.go — 测试 fake 的 LostSince 实现(R4 P1-3 / R4 复审 P1-2)。
package biz

import "context"

// pullRepo:可注入丢失上界与检测错误。
func (r *pullRepo) LostSince(_ context.Context, _ uint64, _ int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lost, r.lostErr
}

// mockOffline:消费链路测试不关心 gap。
func (o *mockOffline) LostSince(_ context.Context, _ uint64, _ int64) (int64, error) {
	return 0, nil
}
