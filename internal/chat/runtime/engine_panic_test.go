package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// panickingOncePlanner 首次 ExecuteToolCalls 时 panic（模拟工具层崩溃），
// 之后透传内嵌的 stubPlanner。
type panickingOncePlanner struct {
	stubPlanner

	mu         sync.Mutex
	panics     int
	firstCalls []PlannerToolCall
}

func (p *panickingOncePlanner) ExecuteToolCalls(ctx context.Context, calls []PlannerToolCall, planContent string) ([]PlannerToolResult, error) {
	// 记账必须在 panic 前解锁（否则守护层与断言都会卡死在锁上）。
	p.mu.Lock()
	p.panics++
	first := p.panics == 1
	if first {
		p.firstCalls = append([]PlannerToolCall(nil), calls...)
	}
	p.mu.Unlock()
	if first {
		panic("boom: 工具执行崩溃")
	}
	return p.stubPlanner.ExecuteToolCalls(ctx, calls, planContent)
}

// TestEngineLoopRecoversFromToolPanic 回归加固 4：引擎循环内工具执行 panic
// 必须被守护 goroutine 拦截并自愈（此前 panic 直接击穿 goroutine 使进程退出），
// 且下一条消息应能触发新一轮正常完成。同时断言修复 5：引擎为工具调用回填了
// 非空 LogicalTurnID。
func TestEngineLoopRecoversFromToolPanic(t *testing.T) {
	planner := &panickingOncePlanner{stubPlanner: *newStubPlanner(
		functionCallResponse("reply", "call-1"),
		&PlannerResponse{Content: "结束", OutputItems: assistantItems("结束")},
	)}
	runtime := newTestRuntime(Options{IsGroupSession: true, MaxInternalRounds: 5})
	runtime.SetPlanner(planner)
	runtime.Start(nil)
	defer runtime.Stop()

	// 第一条消息：ExecuteToolCalls panic（守护层拦截 + 清理排队触发后重建循环）。
	runtime.RegisterMessage(runtimeMessage("m1", time.Now()))
	waitFor(t, "first tool panic", func() bool {
		planner.mu.Lock()
		defer planner.mu.Unlock()
		return planner.panics >= 1
	})

	// 修复 5 断言：引擎回填了非空 LogicalTurnID（wait 依赖它进入等待态）。
	planner.mu.Lock()
	firstCalls := planner.firstCalls
	planner.mu.Unlock()
	if len(firstCalls) != 1 || strings.TrimSpace(firstCalls[0].LogicalTurnID) == "" {
		t.Fatalf("引擎应为工具调用回填非空 LogicalTurnID: %+v", firstCalls)
	}

	// 第二条消息：循环已自愈，应正常跑完一轮（no-tool 结束）。
	runtime.RegisterMessage(runtimeMessage("m2", time.Now()))
	waitFor(t, "second round", func() bool { return planner.requestCount() >= 2 })
	waitFor(t, "cycle end", func() bool { return len(planner.cycleEnds()) >= 1 })
	ends := planner.cycleEnds()
	if ends[len(ends)-1].EndReason != CycleEndPlannerNoToolEnd {
		t.Fatalf("end reason = %q, want %q", ends[len(ends)-1].EndReason, CycleEndPlannerNoToolEnd)
	}
}
