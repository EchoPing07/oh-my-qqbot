package trigger

import (
	"fmt"
	"math"
	"sort"
	"sync"
)

// DynamicGate constants, copied verbatim from dynamic_gate.py.
const (
	// ReplyWindowSeconds mirrors REPLY_WINDOW_SECONDS.
	ReplyWindowSeconds = 3600.0
	// ForcedTurnExpectedReplies mirrors FORCED_TURN_EXPECTED_REPLIES: the
	// measured mean number of replies produced by one @ forced turn.
	ForcedTurnExpectedReplies = 0.91
	// MinWindowScoreCount mirrors MIN_WINDOW_SCORE_COUNT: below this many scored
	// batches the offline static threshold table is used instead.
	MinWindowScoreCount = 20
	// VirtualRoundSeconds mirrors VIRTUAL_ROUND_SECONDS.
	VirtualRoundSeconds = 40.0
	// MaxCountedMessageIDs mirrors MAX_COUNTED_MESSAGE_IDS.
	MaxCountedMessageIDs = 2000
)

// StaticKeepRatioThresholds mirrors _STATIC_KEEP_RATIO_THRESHOLDS: the offline
// probability thresholds for each kept reply ratio (11 rows).
var StaticKeepRatioThresholds = [][2]float64{
	{0.0, 1.0},
	{0.1, 0.605},
	{0.2, 0.482},
	{0.3, 0.430},
	{0.4, 0.376},
	{0.5, 0.336},
	{0.6, 0.313},
	{0.7, 0.290},
	{0.8, 0.250},
	{0.9, 0.206},
	{1.0, 0.0},
}

// StaticThreshold mirrors `_static_threshold`: linear interpolation over the
// offline table; ratios above 1.0 return 0.0 exactly like the Python fallthrough.
func StaticThreshold(keepRatio float64) float64 {
	for index := 0; index+1 < len(StaticKeepRatioThresholds); index++ {
		lowRatio := StaticKeepRatioThresholds[index][0]
		lowThreshold := StaticKeepRatioThresholds[index][1]
		highRatio := StaticKeepRatioThresholds[index+1][0]
		highThreshold := StaticKeepRatioThresholds[index+1][1]
		if keepRatio <= highRatio {
			progress := (keepRatio - lowRatio) / (highRatio - lowRatio)
			return lowThreshold + (highThreshold-lowThreshold)*progress
		}
	}
	return 0.0
}

// DynamicGateDecision mirrors DynamicGateDecision.
type DynamicGateDecision struct {
	ShouldTrigger   bool
	Probability     float64
	Threshold       float64
	ExpectedReplies float64
	TargetReplies   float64
	ActualReplies   int
	KeepRatio       float64
}

// Detail mirrors the `detail` property of DynamicGateDecision.
func (d DynamicGateDecision) Detail() string {
	return fmt.Sprintf(
		"概率=%.2f 阈值=%.2f 预计回复=%.1f 目标=%.1f 已回复=%d 保留比例=%.2f",
		d.Probability, d.Threshold, d.ExpectedReplies, d.TargetReplies, d.ActualReplies, d.KeepRatio,
	)
}

type timedValue struct {
	at    float64
	value float64
}

// DynamicReplyGate mirrors dynamic_gate.DynamicReplyGate: the per-stream
// dynamic reply gate state.
//
// T1 修复：Python 靠 asyncio 单线程事件循环天然串行；Go 侧 replyTimes 由引擎
// goroutine（RecordReply）追加、同时渠道读循环 goroutine 在 Evaluate→prune
// 里 reslice 同一切片，必须显式加锁。
type DynamicReplyGate struct {
	mu                  sync.Mutex
	windowSeconds       float64
	proactiveScores     []timedValue
	forcedExpectations  []timedValue
	replyTimes          []float64
	countedMessageIDs   map[string]bool
	countedMessageOrder []string
	openRoundStartedAt  *float64
}

// NewDynamicReplyGate mirrors DynamicReplyGate.__init__ (window_seconds
// defaults to REPLY_WINDOW_SECONDS).
func NewDynamicReplyGate(windowSeconds float64) *DynamicReplyGate {
	return &DynamicReplyGate{
		windowSeconds:       windowSeconds,
		countedMessageIDs:   map[string]bool{},
		countedMessageOrder: make([]string, 0),
	}
}

// TakeUncountedMessageIDs mirrors dynamic_gate.take_uncounted_message_ids:
// return the ids not yet counted towards the expected reply count and mark them.
func (g *DynamicReplyGate) TakeUncountedMessageIDs(messageIDs []string) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	newIDs := make([]string, 0, len(messageIDs))
	for _, messageID := range messageIDs {
		if messageID == "" || g.countedMessageIDs[messageID] {
			continue
		}
		newIDs = append(newIDs, messageID)
		g.countedMessageIDs[messageID] = true
		g.countedMessageOrder = append(g.countedMessageOrder, messageID)
	}
	for len(g.countedMessageOrder) > MaxCountedMessageIDs {
		oldest := g.countedMessageOrder[0]
		g.countedMessageOrder = g.countedMessageOrder[1:]
		delete(g.countedMessageIDs, oldest)
	}
	return newIDs
}

// IsRoundOpen mirrors dynamic_gate.is_round_open.
func (g *DynamicReplyGate) IsRoundOpen(now float64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.isRoundOpenLocked(now)
}

func (g *DynamicReplyGate) isRoundOpenLocked(now float64) bool {
	return g.openRoundStartedAt != nil && now-*g.openRoundStartedAt < VirtualRoundSeconds
}

// RecordProactiveDemand mirrors dynamic_gate.record_proactive_demand: inside an
// open virtual round the probability is overwritten (one score per round),
// otherwise a new round starts.
func (g *DynamicReplyGate) RecordProactiveDemand(probability float64, now float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.isRoundOpenLocked(now) && len(g.proactiveScores) > 0 {
		g.proactiveScores[len(g.proactiveScores)-1].value = probability
		return
	}
	startedAt := now
	g.openRoundStartedAt = &startedAt
	g.proactiveScores = append(g.proactiveScores, timedValue{at: now, value: probability})
}

// CloseRound mirrors dynamic_gate.close_round: the planner actually ran, so the
// current virtual round ends.
func (g *DynamicReplyGate) CloseRound() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.openRoundStartedAt = nil
}

// RecordForcedTurn mirrors dynamic_gate.record_forced_turn.
func (g *DynamicReplyGate) RecordForcedTurn(now float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.openRoundStartedAt = nil
	g.forcedExpectations = append(g.forcedExpectations, timedValue{at: now, value: ForcedTurnExpectedReplies})
}

// RecordReply mirrors dynamic_gate.record_reply.
func (g *DynamicReplyGate) RecordReply(now float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.replyTimes = append(g.replyTimes, now)
}

// pruneLocked 清理窗口外样本（调用方须持 g.mu）。
func (g *DynamicReplyGate) pruneLocked(now float64) {
	expireBefore := now - g.windowSeconds
	g.proactiveScores = pruneTimedValues(g.proactiveScores, expireBefore)
	g.forcedExpectations = pruneTimedValues(g.forcedExpectations, expireBefore)
	cut := 0
	for cut < len(g.replyTimes) && g.replyTimes[cut] < expireBefore {
		cut++
	}
	g.replyTimes = g.replyTimes[cut:]
}

func pruneTimedValues(values []timedValue, expireBefore float64) []timedValue {
	cut := 0
	for cut < len(values) && values[cut].at < expireBefore {
		cut++
	}
	if cut == 0 {
		return values
	}
	return values[cut:]
}

// ProactiveScores returns a copy of the scored probabilities inside the window
// (used by tests and diagnostics).
func (g *DynamicReplyGate) ProactiveScores() []float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.proactiveScoresLocked()
}

func (g *DynamicReplyGate) proactiveScoresLocked() []float64 {
	scores := make([]float64, 0, len(g.proactiveScores))
	for _, scored := range g.proactiveScores {
		scores = append(scores, scored.value)
	}
	return scores
}

// Evaluate mirrors dynamic_gate.evaluate.
func (g *DynamicReplyGate) Evaluate(probability float64, frequency float64, now float64) DynamicGateDecision {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(now)
	proactiveScores := g.proactiveScoresLocked()
	expectedProactive := 0.0
	for _, score := range proactiveScores {
		expectedProactive += score
	}
	expectedForced := 0.0
	for _, forced := range g.forcedExpectations {
		expectedForced += forced.value
	}
	expectedTotal := expectedProactive + expectedForced
	actualReplies := len(g.replyTimes)
	normalizedFrequency := math.Min(1.0, math.Max(0.0, frequency))
	targetReplies := normalizedFrequency * expectedTotal

	var keepRatio float64
	switch {
	case normalizedFrequency >= 1.0:
		keepRatio = 1.0
	case expectedProactive <= 0:
		keepRatio = normalizedFrequency
	default:
		feedforwardRatio := (targetReplies - expectedForced) / expectedProactive
		feedbackFactor := math.Min(1.0, math.Max(0.0,
			1.0+(targetReplies-float64(actualReplies))/math.Max(1.0, targetReplies)))
		keepRatio = math.Min(1.0, math.Max(0.0, feedforwardRatio*feedbackFactor))
	}

	threshold := g.resolveThreshold(proactiveScores, expectedProactive, keepRatio)
	return DynamicGateDecision{
		ShouldTrigger:   probability >= threshold,
		Probability:     probability,
		Threshold:       threshold,
		ExpectedReplies: expectedTotal,
		TargetReplies:   targetReplies,
		ActualReplies:   actualReplies,
		KeepRatio:       keepRatio,
	}
}

func (g *DynamicReplyGate) resolveThreshold(proactiveScores []float64, expectedProactive float64, keepRatio float64) float64 {
	if keepRatio >= 1.0 {
		return 0.0
	}
	if keepRatio <= 0.0 {
		return 1.0
	}
	if len(proactiveScores) < MinWindowScoreCount {
		return StaticThreshold(keepRatio)
	}

	// 按概率从高到低累计，累计到保留额度时的概率即阈值
	budget := keepRatio * expectedProactive
	accumulated := 0.0
	sorted := make([]float64, len(proactiveScores))
	copy(sorted, proactiveScores)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
	for _, score := range sorted {
		accumulated += score
		if accumulated >= budget {
			return score
		}
	}
	return 0.0
}
