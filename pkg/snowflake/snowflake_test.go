package snowflake

import (
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withVirtualClock 注入虚拟时钟并返回恢复函数。
// 注意:unixNow 是包级变量,用虚拟时钟的测试不能 t.Parallel()。
func withVirtualClock(start int64) (*atomic.Int64, func()) {
	var vclock atomic.Int64
	vclock.Store(start)
	orig := unixNow
	unixNow = func() int64 { return vclock.Load() }
	return &vclock, func() { unixNow = orig }
}

func TestMinIDAt(t *testing.T) {
	if got := MinIDAt(int64(Epoch) - 1); got != 0 {
		t.Fatalf("MinIDAt(before epoch) = %d, want 0", got)
	}
	// t 秒生成的 ID 必落在 [MinIDAt(t), MinIDAt(t+1)) 区间
	vclock, restore := withVirtualClock(int64(Epoch) + 1000)
	defer restore()
	n := NewNode(3)
	id := n.Generate()
	lo, hi := MinIDAt(vclock.Load()), MinIDAt(vclock.Load()+1)
	if id < lo || id >= hi {
		t.Fatalf("id %d outside [%d, %d)", id, lo, hi)
	}
}

func TestGenerate_Unique(t *testing.T) {
	n := NewNode(0)
	const count = 10000
	seen := make(map[uint64]bool, count)
	for i := 0; i < count; i++ {
		id := n.Generate()
		if seen[id] {
			t.Fatalf("duplicate ID %d at iteration %d", id, i)
		}
		seen[id] = true
	}
}

func TestGenerate_Monotonic(t *testing.T) {
	n := NewNode(0)
	prev := uint64(0)
	for i := 0; i < 1000; i++ {
		id := n.Generate()
		if id <= prev {
			t.Fatalf("ID not monotonic: prev=%d, got=%d at iteration %d", prev, id, i)
		}
		prev = id
	}
}

func TestGenerate_EmbeddedNodeID(t *testing.T) {
	nodeID := uint64(42)
	n := NewNode(nodeID)
	id := n.Generate()

	extracted := (id >> nodeShift) & NodeMask
	if extracted != nodeID {
		t.Fatalf("expected nodeID %d in ID, got %d", nodeID, extracted)
	}
}

func TestGenerate_DifferentNodes_NoDuplicates(t *testing.T) {
	n1 := NewNode(1)
	n2 := NewNode(2)
	seen := make(map[uint64]bool)
	for i := 0; i < 1000; i++ {
		id1 := n1.Generate()
		id2 := n2.Generate()
		if seen[id1] {
			t.Fatalf("duplicate from node1: %d", id1)
		}
		if seen[id2] {
			t.Fatalf("duplicate from node2: %d", id2)
		}
		seen[id1] = true
		seen[id2] = true
	}
}

func TestGenerate_ConcurrentSafety(t *testing.T) {
	n := NewNode(0)
	const goroutines = 8
	const perGoroutine = 5000

	var mu sync.Mutex
	seen := make(map[uint64]bool, goroutines*perGoroutine)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			ids := make([]uint64, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				ids[i] = n.Generate()
			}
			mu.Lock()
			defer mu.Unlock()
			for _, id := range ids {
				if seen[id] {
					t.Errorf("duplicate ID %d", id)
				}
				seen[id] = true
			}
		}()
	}
	wg.Wait()
}

func TestNewNode_PanicsOnOverflow(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for oversized nodeID")
		}
	}()
	NewNode(NodeMask + 1)
}

// TestGenerate_VirtualClockNoDupNoGap 是大规模并发唯一性测试。
//
// 为什么不直接"跑十亿次真实 Generate":秒级时间戳 + 15 bit step =
// 每节点每秒上限 32768 个 ID,真跑十亿要 ~8.5 小时墙钟,且 map 去重需 ~8GB
// 内存——不是可行的 CI 测试。
//
// 这里用虚拟时钟绕开容量墙:每个虚拟秒让所有 goroutine 把当秒的 32768 个
// step 抢光,然后断言这一秒产出的 ID 既无重复(每个 step 恰好出现一次)又
// 无遗漏(0..32767 全覆盖),再推进到下一秒。这样以 O(32768) 内存就能验证
// 任意大的总量:默认千万级,SNOWFLAKE_STRESS_ROUNDS 可放大到十亿级以上。
//
// 逐秒"恰好瓜分 step 池"是 CAS 争用最激烈的点:每秒第一个 ID 走"新秒"分支,
// 其余全靠 old+1 抢占,任何丢失/重复/错位都会被当场抓到。秒秒正确即整体正确。
func TestGenerate_VirtualClockNoDupNoGap(t *testing.T) {
	const perSecond = uint64(stepMask + 1) // 每虚拟秒 32768 个 step

	// rounds 个虚拟秒 × 32768 = 总验证量。默认 ~1000 万。
	rounds := 305
	if v := os.Getenv("SNOWFLAKE_STRESS_ROUNDS"); v != "" {
		if r, err := strconv.Atoi(v); err == nil && r > 0 {
			rounds = r // 十亿级:设 30518(30518 × 32768 ≈ 1e9)或更大
		}
	}
	if testing.Short() {
		rounds = 4
	}

	// 注入虚拟时钟,测试结束恢复。
	vclock, restore := withVirtualClock(int64(Epoch) + 1) // nowEpoch 起点 = 1(> 初始 lastTime=0)
	defer restore()

	const nodeID = uint64(7)
	n := NewNode(nodeID)

	goroutines := runtime.GOMAXPROCS(0)
	if goroutines < 2 {
		goroutines = 2
	}

	seen := make([]atomic.Bool, perSecond) // 复用:按 step 索引去重

	for r := 0; r < rounds; r++ {
		for i := range seen {
			seen[i].Store(false)
		}
		expectedTime := uint64(vclock.Load()) - Epoch
		var claimed atomic.Uint64
		var dup, badField atomic.Int64

		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func() {
				defer wg.Done()
				// 用 claimed 把当秒的 Generate 调用数精确限制为 perSecond,
				// 避免触发第 32769 次进入 waitNextTime 阻塞。
				for claimed.Add(1) <= perSecond {
					id := n.Generate()
					if id>>timeShift != expectedTime ||
						(id>>nodeShift)&NodeMask != nodeID {
						badField.Add(1)
						continue
					}
					step := id & stepMask
					if seen[step].Swap(true) {
						dup.Add(1)
					}
				}
			}()
		}
		wg.Wait()

		if d := dup.Load(); d != 0 {
			t.Fatalf("virtual second %d: %d duplicate IDs", r, d)
		}
		if b := badField.Load(); b != 0 {
			t.Fatalf("virtual second %d: %d IDs with wrong time/node bits", r, b)
		}
		for s := uint64(0); s < perSecond; s++ {
			if !seen[s].Load() {
				t.Fatalf("virtual second %d: step %d missing (gap)", r, s)
			}
		}

		vclock.Add(1) // 下一虚拟秒
	}

	t.Logf("verified %d unique IDs across %d virtual seconds: no duplicate, no gap",
		uint64(rounds)*perSecond, rounds)
}

func TestNewNode_MaxNodeID(t *testing.T) {
	n := NewNode(NodeMask)
	id := n.Generate()
	extracted := (id >> nodeShift) & NodeMask
	if extracted != NodeMask {
		t.Fatalf("expected nodeID %d, got %d", NodeMask, extracted)
	}
}

// 注意:秒级时间戳 + 15 bit step = 每节点每秒上限 32768 个 ID。
// 基准跑满一秒的 step 池后会退化为等待下一秒,故稳态 ~30.5µs/op 是容量上限,
// 不是临界区开销;临界区本身的开销看 N 较小时的首轮数字。
func BenchmarkGenerate(b *testing.B) {
	n := NewNode(0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		n.Generate()
	}
}

func BenchmarkGenerateParallel(b *testing.B) {
	n := NewNode(0)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n.Generate()
		}
	})
}

// ---------- 时钟回拨 ----------

// 回拨期间应继续消费 lastTime 秒剩余 step 池:不阻塞、不重复、严格递增。
func TestGenerate_ClockRollback_KeepsServing(t *testing.T) {
	vclock, restore := withVirtualClock(int64(Epoch) + 100)
	defer restore()

	n := NewNode(3)
	id1 := n.Generate() // lastTime=100, step=0

	vclock.Store(int64(Epoch) + 50) // 时钟回拨 50 秒

	prev := id1
	for i := 0; i < 1000; i++ {
		id := n.Generate()
		if id <= prev {
			t.Fatalf("rollback: ID not monotonic at %d: prev=%d got=%d", i, prev, id)
		}
		// 时间位必须钉在回拨前的 lastTime=100,绝不能用回拨后的 50
		if tm := id >> timeShift; tm != 100 {
			t.Fatalf("rollback: time bits = %d, want 100 (must not regress)", tm)
		}
		prev = id
	}
}

// 回拨 + step 耗尽叠加:必须阻塞到真实时钟追过 lastTime,期间一个 ID 都不能发。
func TestGenerate_RollbackThenExhausted_BlocksUntilClockCatchesUp(t *testing.T) {
	vclock, restore := withVirtualClock(int64(Epoch) + 200)
	defer restore()

	n := NewNode(0)
	// 把 lastTime=200 这一秒的 32768 个 step 全部耗尽
	for i := uint64(0); i <= stepMask; i++ {
		n.Generate()
	}
	vclock.Store(int64(Epoch) + 150) // 回拨到 150,step 又已耗尽 → 只能等

	done := make(chan uint64, 1)
	go func() { done <- n.Generate() }()

	select {
	case id := <-done:
		t.Fatalf("should block while clock(150) <= lastTime(200), but got ID %d", id)
	case <-time.After(50 * time.Millisecond):
		// 正确:在等待
	}

	vclock.Store(int64(Epoch) + 201) // 真实时钟追过 lastTime
	select {
	case id := <-done:
		if tm := id >> timeShift; tm != 201 {
			t.Fatalf("after catch-up: time bits = %d, want 201", tm)
		}
		if step := id & stepMask; step != 0 {
			t.Fatalf("after catch-up: step = %d, want 0", step)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("still blocked after clock caught up")
	}
}

// ---------- step 耗尽(正常时钟) ----------

// 同一秒发满 32768 个后必须等到下一秒,且新秒 step 从 0 重新计数。
func TestGenerate_StepExhausted_RollsToNextSecond(t *testing.T) {
	vclock, restore := withVirtualClock(int64(Epoch) + 10)
	defer restore()

	n := NewNode(5)
	for i := uint64(0); i <= stepMask; i++ {
		id := n.Generate()
		if step := id & stepMask; step != i {
			t.Fatalf("step sequence broken: want %d got %d", i, step)
		}
	}

	done := make(chan uint64, 1)
	go func() { done <- n.Generate() }()
	select {
	case id := <-done:
		t.Fatalf("should block when step pool exhausted, got %d", id)
	case <-time.After(50 * time.Millisecond):
	}

	vclock.Add(1)
	id := <-done
	if tm, step := id>>timeShift, id&stepMask; tm != 11 || step != 0 {
		t.Fatalf("next second: time=%d step=%d, want time=11 step=0", tm, step)
	}
}

// step=32767 时 old+1 绝不能进位污染 node 位(等待路径已挡住,这里验证位域完整性)。
func TestGenerate_StepNeverOverflowsIntoNode(t *testing.T) {
	vclock, restore := withVirtualClock(int64(Epoch) + 20)
	defer restore()

	const nodeID = uint64(NodeMask) // 全 1 的 node 段最容易暴露进位污染
	n := NewNode(nodeID)
	for i := uint64(0); i <= stepMask; i++ {
		id := n.Generate()
		if got := (id >> nodeShift) & NodeMask; got != nodeID {
			t.Fatalf("node bits corrupted at step %d: want %d got %d", i, nodeID, got)
		}
	}
	// 下一个 ID 必须来自新秒(而非进位),node 位仍完好
	vclock.Add(1)
	id := n.Generate()
	if got := (id >> nodeShift) & NodeMask; got != nodeID {
		t.Fatalf("node bits corrupted after second roll: want %d got %d", nodeID, got)
	}
	if tm := id >> timeShift; tm != 21 {
		t.Fatalf("time bits = %d, want 21", tm)
	}
}

// ---------- 时钟早于 Epoch ----------

// 系统时钟早于 Epoch 时必须 panic,绝不能下溢发出错 ID。
func TestGenerate_PanicsWhenClockBeforeEpoch(t *testing.T) {
	_, restore := withVirtualClock(int64(Epoch) - 1)
	defer restore()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when system clock is before epoch")
		}
	}()
	NewNode(0).Generate()
}

// ---------- 跨秒边界并发 ----------

// 时钟在并发 Generate 中途推进:「新秒」分支与「old+1」分支竞争也不能重不能漏。
// 每秒只消费半池就推进时钟,专打 now>lastTime 与同秒自增的交叉路径
// (大规模测试 TestGenerate_VirtualClockNoDupNoGap 是整池瓜分,二者互补)。
func TestGenerate_ConcurrentAcrossSecondBoundary(t *testing.T) {
	vclock, restore := withVirtualClock(int64(Epoch) + 1000)
	defer restore()

	n := NewNode(9)
	goroutines := runtime.GOMAXPROCS(0)
	if goroutines < 4 {
		goroutines = 4
	}
	const seconds = 200
	perSecond := (int(stepMask) + 1) / 2 // 半池,保证不触发等待

	var mu sync.Mutex
	seen := make(map[uint64]struct{}, seconds*perSecond)

	for s := 0; s < seconds; s++ {
		var claimed atomic.Int64
		var wg sync.WaitGroup
		wg.Add(goroutines)
		ids := make([][]uint64, goroutines)
		for g := 0; g < goroutines; g++ {
			go func(g int) {
				defer wg.Done()
				local := make([]uint64, 0, perSecond/goroutines+1)
				for claimed.Add(1) <= int64(perSecond) {
					local = append(local, n.Generate())
				}
				ids[g] = local
			}(g)
		}
		// 不等本秒收尾就推进时钟,制造秒切换与 old+1 的真实交叉
		vclock.Add(1)
		wg.Wait()

		mu.Lock()
		for _, batch := range ids {
			for _, id := range batch {
				if _, dup := seen[id]; dup {
					t.Fatalf("duplicate ID %d at virtual second %d", id, s)
				}
				seen[id] = struct{}{}
			}
		}
		mu.Unlock()
	}
}

// ---------- 并发单调 ----------

// 全局严格单调的推论:任何单个 goroutine 先后两次 Generate 的结果也必须递增。
func TestGenerate_ConcurrentPerCallerMonotonic(t *testing.T) {
	n := NewNode(0)
	goroutines := runtime.GOMAXPROCS(0)
	if goroutines < 4 {
		goroutines = 4
	}
	const perGoroutine = 3000 // 总量 < 32768,真实时钟下也不会撞容量墙

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			prev := uint64(0)
			for i := 0; i < perGoroutine; i++ {
				id := n.Generate()
				if id <= prev {
					t.Errorf("per-caller monotonicity broken: prev=%d got=%d", prev, id)
					return
				}
				prev = id
			}
		}()
	}
	wg.Wait()
}
