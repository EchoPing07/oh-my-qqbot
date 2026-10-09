package monitor

import (
	"encoding/json"
	"testing"
	"time"

	contracts "oh-my-qqbot/internal"
)

// 零订阅开销：Emit 不 panic、不分配 channel，事件直接蒸发。
func TestBusZeroSubscribers(t *testing.T) {
	bus := NewBus()
	// 快速路径：无订阅者时 Emit 应为零开销且无副作用。
	start := time.Now()
	for i := 0; i < 10000; i++ {
		bus.Emit(contracts.Event{Kind: contracts.EventMessageSent, SessionID: "s1"})
	}
	elapsed := time.Since(start)
	if bus.SubscriberCount() != 0 {
		t.Fatalf("订阅者数量应为 0, got %d", bus.SubscriberCount())
	}
	if bus.Dropped() != 0 {
		t.Fatalf("无订阅不应有丢弃: %d", bus.Dropped())
	}
	// 10000 次零开销调用应在极短时间内完成（保守上限 1s；加锁路径会明显更慢）。
	if elapsed > time.Second {
		t.Fatalf("零订阅 Emit 过慢: %v", elapsed)
	}
}

func TestBusSubscribeReceive(t *testing.T) {
	bus := NewBus()
	ch, cancel := bus.Subscribe(8)
	defer cancel()

	bus.Emit(contracts.Event{Kind: contracts.EventSessionStart, SessionID: "s1", Payload: map[string]any{"k": "v"}})
	select {
	case ev := <-ch:
		if ev.Kind != contracts.EventSessionStart || ev.SessionID != "s1" {
			t.Fatalf("收到错误事件: %+v", ev)
		}
		if ev.Payload["k"] != "v" {
			t.Fatalf("载荷丢失: %+v", ev.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("未收到事件")
	}

	if bus.SubscriberCount() != 1 {
		t.Fatalf("订阅者数量应为 1, got %d", bus.SubscriberCount())
	}
	cancel()
	if bus.SubscriberCount() != 0 {
		t.Fatalf("取消后订阅者应为 0, got %d", bus.SubscriberCount())
	}
	// 取消后 Emit 不应再派发（也不能 panic 向已关闭 channel 发送）。
	bus.Emit(contracts.Event{Kind: contracts.EventMessageSent})
}

// 背压：慢消费者 channel 满时事件被丢弃，Emit 不阻塞。
func TestBusBackpressureDropsAndNeverBlocks(t *testing.T) {
	bus := NewBus()
	// 容量 1 的慢消费者，先填满且不读取。
	ch, cancel := bus.Subscribe(1)
	defer cancel()

	bus.Emit(contracts.Event{Kind: contracts.EventMessageIngested}) // 占满缓冲
	bus.Emit(contracts.Event{Kind: contracts.EventMessageSent})     // 应被丢弃

	if bus.Dropped() != 1 {
		t.Fatalf("应丢弃 1 条事件, got %d", bus.Dropped())
	}
	select {
	case ev := <-ch:
		if ev.Kind != contracts.EventMessageIngested {
			t.Fatalf("缓冲内事件应为最早一条: %+v", ev.Kind)
		}
	default:
		t.Fatal("缓冲内应保留第一条事件")
	}
}

func TestBusFanOut(t *testing.T) {
	bus := NewBus()
	ch1, cancel1 := bus.Subscribe(4)
	ch2, cancel2 := bus.Subscribe(4)
	defer cancel1()
	defer cancel2()

	bus.Emit(contracts.Event{Kind: contracts.EventLLMRetry, SessionID: "s2"})
	for _, ch := range []<-chan contracts.Event{ch1, ch2} {
		select {
		case ev := <-ch:
			if ev.Kind != contracts.EventLLMRetry {
				t.Fatalf("fan-out 事件错误: %+v", ev.Kind)
			}
		case <-time.After(time.Second):
			t.Fatal("订阅者未收到事件")
		}
	}
}

// emit 辅助函数：载荷字段名对齐 events.py。
func TestEmitHelpersPayload(t *testing.T) {
	bus := NewBus()
	ch, cancel := bus.Subscribe(16)
	defer cancel()

	EmitSessionStart(bus, "s1", "测试群", true, "g1", "", "qq")
	EmitLLMRetry(bus, LLMRetryParams{SessionID: "s1", TaskName: "planner", Attempt: 2, MaxAttempts: 3, Reason: "timeout", RetryInterval: 1.5})
	EmitLLMError(bus, LLMErrorParams{SessionID: "s1", TaskName: "replyer", Message: "boom"})
	EmitMessageIngested(bus, MessageParams{SessionID: "s1", SpeakerName: "小明", Content: "你好", MessageID: "m1", Timestamp: 1700000000})
	EmitMessageSent(bus, MessageParams{SessionID: "s1", SpeakerName: "麦麦", Content: "嗨", MessageID: "m2", SourceKind: "guided_reply", Timestamp: 1700000001})
	EmitStageStatus(bus, StageStatusParams{SessionID: "s1", SessionName: "测试群", Stage: "planner", Detail: "第 2 轮", Timestamp: 1700000002})
	EmitPlannerProgress(bus, PlannerProgressParams{SessionID: "s1", CycleID: 1, RunID: "r1", Content: "ok"})

	received := map[contracts.EventKind]contracts.Event{}
	for i := 0; i < 7; i++ {
		select {
		case ev := <-ch:
			received[ev.Kind] = ev
		case <-time.After(time.Second):
			t.Fatalf("第 %d 个事件未送达", i+1)
		}
	}

	ss := received[contracts.EventSessionStart]
	if ss.Payload["session_name"] != "测试群" || ss.Payload["is_group_chat"] != true || ss.Payload["platform"] != "qq" {
		t.Fatalf("session.start 载荷不对齐: %+v", ss.Payload)
	}
	retry := received[contracts.EventLLMRetry]
	if retry.Payload["task_name"] != "planner" || retry.Payload["attempt"] != 2 || retry.Payload["retry_interval"] != 1.5 {
		t.Fatalf("llm.retry 载荷不对齐: %+v", retry.Payload)
	}
	lerr := received[contracts.EventLLMError]
	if lerr.Payload["message"] != "boom" {
		t.Fatalf("llm.error 载荷不对齐: %+v", lerr.Payload)
	}
	ing := received[contracts.EventMessageIngested]
	if ing.Payload["speaker_name"] != "小明" || ing.Payload["content"] != "你好" {
		t.Fatalf("message.ingested 载荷不对齐: %+v", ing.Payload)
	}
	if _, hasSource := ing.Payload["source_kind"]; hasSource {
		t.Fatal("message.ingested 不应带 source_kind")
	}
	sent := received[contracts.EventMessageSent]
	if sent.Payload["source_kind"] != "guided_reply" {
		t.Fatalf("message.sent 载荷不对齐: %+v", sent.Payload)
	}
	stage := received[contracts.EventStageStatus]
	if stage.Payload["stage"] != "planner" || stage.Payload["detail"] != "第 2 轮" {
		t.Fatalf("stage.status 载荷不对齐: %+v", stage.Payload)
	}
	prog := received[contracts.EventPlannerProgress]
	if prog.Payload["cycle_id"] != 1 || prog.Payload["run_id"] != "r1" {
		t.Fatalf("planner.progress 载荷不对齐: %+v", prog.Payload)
	}
	planner, ok := prog.Payload["planner"].(map[string]any)
	if !ok || planner["content"] != "ok" {
		t.Fatalf("planner 区块不对齐: %+v", prog.Payload["planner"])
	}

	// 事件可 JSON 序列化（WebUI WS 广播用）。
	for _, ev := range received {
		if _, err := json.Marshal(ev); err != nil {
			t.Fatalf("事件 JSON 序列化失败: %v", err)
		}
	}
}

// 编译期对齐 internal.EventBus 契约。
var _ contracts.EventBus = (*Bus)(nil)
