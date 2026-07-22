package biz

import (
	"context"
	"sort"
	"testing"

	"github.com/luyuancpp/pandora/pkg/errcode"
	"github.com/luyuancpp/pandora/services/social/guild/internal/conf"
	"github.com/luyuancpp/pandora/services/social/guild/internal/data"
)

type boundaryGuildRepo struct {
	data.GuildRepo
	guild             *data.GuildRow
	members           map[uint64]*data.GuildMemberRow
	requests          map[uint64]*data.GuildJoinRequestRow
	canonicalRequest  uint64
	createRequestCall int
	lastMaxPending    int
	approveErr        error
	lastMaxMembers    int
	memberRows        []data.GuildMemberRow
	pendingRows       []data.GuildJoinRequestRow
	lastMemberCursor  uint64
	lastMemberLimit   int
	lastPendingCursor uint64
	lastPendingLimit  int
	pendingListCalls  int
}

func newBoundaryGuildRepo() *boundaryGuildRepo {
	return &boundaryGuildRepo{
		guild:    &data.GuildRow{GuildID: 100, Name: "G", LeaderID: 1, MemberCount: 1, MaxMembers: 3},
		members:  map[uint64]*data.GuildMemberRow{},
		requests: map[uint64]*data.GuildJoinRequestRow{},
	}
}

func (r *boundaryGuildRepo) GetMember(_ context.Context, playerID uint64) (*data.GuildMemberRow, bool, error) {
	m, ok := r.members[playerID]
	return m, ok, nil
}

func (r *boundaryGuildRepo) GetGuild(_ context.Context, guildID uint64) (*data.GuildRow, bool, error) {
	if r.guild == nil || r.guild.GuildID != guildID {
		return nil, false, nil
	}
	return r.guild, true, nil
}

func (r *boundaryGuildRepo) CreateJoinRequest(_ context.Context, newRequestID, guildID, playerID uint64, maxPending int) (uint64, bool, error) {
	r.createRequestCall++
	r.lastMaxPending = maxPending
	if r.canonicalRequest != 0 {
		return r.canonicalRequest, true, nil
	}
	r.canonicalRequest = newRequestID
	r.requests[newRequestID] = &data.GuildJoinRequestRow{
		RequestID: newRequestID, GuildID: guildID, PlayerID: playerID, Status: 1,
	}
	return newRequestID, false, nil
}

func (r *boundaryGuildRepo) GetRequest(_ context.Context, requestID uint64) (*data.GuildJoinRequestRow, bool, error) {
	rq, ok := r.requests[requestID]
	return rq, ok, nil
}

func (r *boundaryGuildRepo) ApproveJoin(_ context.Context, _, _ uint64, maxMembers int) (bool, error) {
	r.lastMaxMembers = maxMembers
	if r.approveErr != nil {
		return false, r.approveErr
	}
	return true, nil
}

func (r *boundaryGuildRepo) ListMembers(_ context.Context, _ uint64, cursor uint64, limit int) ([]data.GuildMemberRow, error) {
	r.lastMemberCursor = cursor
	r.lastMemberLimit = limit
	rows := append([]data.GuildMemberRow(nil), r.memberRows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].PlayerID < rows[j].PlayerID })
	filtered := rows[:0]
	for _, row := range rows {
		if cursor == 0 || row.PlayerID > cursor {
			filtered = append(filtered, row)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (r *boundaryGuildRepo) ListPendingRequests(_ context.Context, _ uint64, cursor uint64, limit int) ([]data.GuildJoinRequestRow, error) {
	r.pendingListCalls++
	r.lastPendingCursor = cursor
	r.lastPendingLimit = limit
	rows := append([]data.GuildJoinRequestRow(nil), r.pendingRows...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].RequestID < rows[j].RequestID })
	filtered := rows[:0]
	for _, row := range rows {
		if cursor == 0 || row.RequestID > cursor {
			filtered = append(filtered, row)
		}
	}
	if len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func newBoundaryGuildUsecase(repo data.GuildRepo) *GuildUsecase {
	return NewGuildUsecase(repo, nil, nil, conf.GuildConf{
		MaxGuildMembers:            3,
		MaxPendingRequestsPerGuild: 2,
		MaxNameLen:                 24,
	})
}

func requireBoundaryGuildCode(t *testing.T, err error, want errcode.Code) {
	t.Helper()
	if got := errcode.As(err); got != want {
		t.Fatalf("错误码不符: got=%d want=%d err=%v", got, want, err)
	}
}

// TestApplyJoinDuplicateReturnsCanonicalRequest 覆盖响应丢失后的重复申请：新的
// request_id 不能泄露给客户端，必须始终回放数据库里的 canonical request_id。
func TestApplyJoinDuplicateReturnsCanonicalRequest(t *testing.T) {
	repo := newBoundaryGuildRepo()
	uc := newBoundaryGuildUsecase(repo)

	first, err := uc.ApplyJoin(context.Background(), 7, 100, 7001)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := uc.ApplyJoin(context.Background(), 7, 100, 7002)
	if err != nil {
		t.Fatalf("duplicate apply: %v", err)
	}
	if first != 7001 || second != first {
		t.Fatalf("重复申请必须回放 canonical id: first=%d second=%d", first, second)
	}
	if repo.createRequestCall != 2 || repo.lastMaxPending != 2 {
		t.Fatalf("申请上限参数或调用次数不符: calls=%d max_pending=%d",
			repo.createRequestCall, repo.lastMaxPending)
	}
}

// TestApproveJoinGuildFullPropagates 容量满必须保持明确业务错误，不能伪装成
// 申请无效或成功，客户端才能提示并继续等待名额。
func TestApproveJoinGuildFullPropagates(t *testing.T) {
	repo := newBoundaryGuildRepo()
	repo.requests[8001] = &data.GuildJoinRequestRow{RequestID: 8001, GuildID: 100, PlayerID: 8, Status: 1}
	repo.approveErr = errcode.New(errcode.ErrGuildFull, "guild full")

	err := newBoundaryGuildUsecase(repo).ApproveJoin(context.Background(), 1, 8001)
	requireBoundaryGuildCode(t, err, errcode.ErrGuildFull)
	if repo.lastMaxMembers != 3 {
		t.Fatalf("审批必须把成员容量下传数据层: got=%d want=3", repo.lastMaxMembers)
	}
}

// TestGuildPaginationAndPermissionBoundaries 覆盖成员/申请列表游标，以及普通成员
// 不能读取申请、官员可以读取的权限边界。
func TestGuildPaginationAndPermissionBoundaries(t *testing.T) {
	repo := newBoundaryGuildRepo()
	repo.memberRows = []data.GuildMemberRow{
		{PlayerID: 3, GuildID: 100, Role: data.GuildRoleMember},
		{PlayerID: 1, GuildID: 100, Role: data.GuildRoleLeader},
		{PlayerID: 2, GuildID: 100, Role: data.GuildRoleOfficer},
	}
	repo.pendingRows = []data.GuildJoinRequestRow{
		{RequestID: 20, GuildID: 100, PlayerID: 20},
		{RequestID: 10, GuildID: 100, PlayerID: 10},
	}
	uc := newBoundaryGuildUsecase(repo)

	first, next, err := uc.ListMembers(context.Background(), 100, 0, 2)
	if err != nil || len(first) != 2 || first[0].GetPlayerId() != 1 || first[1].GetPlayerId() != 2 || next != 2 {
		t.Fatalf("成员首页不符: rows=%+v next=%d err=%v", first, next, err)
	}
	second, next, err := uc.ListMembers(context.Background(), 100, 2, 2)
	if err != nil || len(second) != 1 || second[0].GetPlayerId() != 3 || next != 0 {
		t.Fatalf("成员次页不符: rows=%+v next=%d err=%v", second, next, err)
	}

	repo.members[7] = &data.GuildMemberRow{PlayerID: 7, GuildID: 100, Role: data.GuildRoleMember}
	_, _, err = uc.ListJoinRequests(context.Background(), 7, 0, 1)
	requireBoundaryGuildCode(t, err, errcode.ErrGuildNoPermission)
	if repo.pendingListCalls != 0 {
		t.Fatalf("普通成员无权时不得查询申请明细: calls=%d", repo.pendingListCalls)
	}

	repo.members[7].Role = data.GuildRoleOfficer
	requests, requestNext, err := uc.ListJoinRequests(context.Background(), 7, 0, 1)
	if err != nil || len(requests) != 1 || requests[0].GetRequestId() != 10 || requestNext != 10 {
		t.Fatalf("官员申请列表不符: rows=%+v next=%d err=%v", requests, requestNext, err)
	}
}
