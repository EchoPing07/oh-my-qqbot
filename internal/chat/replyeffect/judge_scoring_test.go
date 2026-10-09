package replyeffect

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 假 runner 的枚举/概率校验与 prompt 组装（对齐 judge.py）。
func TestJudgeReplyEffectParsesValidPayload(t *testing.T) {
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{
			{MessageID: "F1", UserID: "U1", CandidateEffectIDs: []string{"EFF-1"}},
			{MessageID: "F2", UserID: "U2", CandidateEffectIDs: []string{"EFF-1"}},
		},
	}
	runner := &fixedJudgeRunner{responses: []string{lifecycleJudgeResponse()}}
	judgment, err := JudgeReplyEffect(t.Context(), record, []*Record{{EffectID: "EFF-1"}},
		BotIdentity{Nickname: "麦麦"}, runner.run)
	if err != nil {
		t.Fatalf("合法响应应被接受: %v", err)
	}
	if judgment.Primary != "answer" || len(judgment.Secondary) != 1 {
		t.Fatalf("策略解析不符: %+v", judgment)
	}
	if len(judgment.Associations) != 2 {
		t.Fatalf("应覆盖两条后续消息, got %v", judgment.Associations)
	}
	if len(judgment.Associations["F2"]) != 1 ||
		judgment.Associations["F2"][0].AttributionType != AttributionSemantic {
		t.Fatalf("语义关联解析不符: %+v", judgment.Associations["F2"])
	}
}

func TestJudgeRejectsInvalidPayloads(t *testing.T) {
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{
			{MessageID: "F1", UserID: "U1", CandidateEffectIDs: []string{"EFF-1"}},
		},
	}
	candidates := []*Record{{EffectID: "EFF-1"}}
	cases := map[string]string{
		"非 JSON":           "这不是 JSON",
		"数组根节点":            `[1,2,3]`,
		"未知策略":             `{"strategy":{"primary":"unknown","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":[]}]}`,
		"概率越界":             `{"strategy":{"primary":"answer","secondary":[],"confidence":1.5},"messages":[{"message_id":"F1","associations":[]}]}`,
		"未知消息":             `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F9","associations":[]}]}`,
		"漏覆盖":              `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[]}`,
		"未知候选":             `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":[{"candidate_id":"c9","attribution_confidence":0.5,"stance_target":"bot_content","stance":"neutral","contribution":"advance","confidence":0.5}]}]}`,
		"未知 stance":        `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":[{"candidate_id":"c1","attribution_confidence":0.5,"stance_target":"bot_content","stance":"unrelated","contribution":"advance","confidence":0.5}]}]}`,
		"未知 contribution":  `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":[{"candidate_id":"c1","attribution_confidence":0.5,"stance_target":"bot_content","stance":"neutral","contribution":"unrelated","confidence":0.5}]}]}`,
		"associations 非数组": `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":{}}]}`,
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fixedJudgeRunner{responses: []string{response, response}}
			if _, err := JudgeReplyEffect(t.Context(), record, candidates, BotIdentity{}, runner.run); err == nil {
				t.Fatal("非法响应应返回错误")
			} else if !strings.Contains(err.Error(), "连续两次校验失败") {
				t.Fatalf("错误文案不符: %v", err)
			}
			if len(runner.prompts) != 2 {
				t.Fatalf("应重试一次, got %d 次调用", len(runner.prompts))
			}
		})
	}
}

func TestJudgeRequiresExplicitQuoteAssociation(t *testing.T) {
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{{
			MessageID:          "F1",
			UserID:             "U1",
			CandidateEffectIDs: []string{"EFF-1"},
			Associations: []ReplyAssociation{{
				EffectID:        "EFF-1",
				AttributionType: AttributionExplicitQuote,
			}},
		}},
	}
	// 评审输出未包含被锁定的显式引用关联 → 校验失败。
	response := `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":[]}]}`
	runner := &fixedJudgeRunner{responses: []string{response, response}}
	_, err := JudgeReplyEffect(t.Context(), record, []*Record{{EffectID: "EFF-1"}}, BotIdentity{}, runner.run)
	if err == nil || !strings.Contains(err.Error(), "显式引用关联被遗漏") {
		t.Fatalf("应拒绝遗漏显式引用关联的输出: %v", err)
	}
}

func TestJudgeRejectsUnknownCandidateForFollowup(t *testing.T) {
	// F1 只允许 EFF-1 候选（EFF-2 在消息发生时还不存在）。
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{{
			MessageID:          "F1",
			UserID:             "U1",
			CandidateEffectIDs: []string{"EFF-1"},
		}},
	}
	candidates := []*Record{{EffectID: "EFF-1"}, {EffectID: "EFF-2"}}
	response := `{"strategy":{"primary":"answer","secondary":[],"confidence":0.5},"messages":[{"message_id":"F1","associations":[{"candidate_id":"c2","attribution_confidence":0.5,"stance_target":"bot_content","stance":"neutral","contribution":"advance","confidence":0.5}]}]}`
	runner := &fixedJudgeRunner{responses: []string{response, response}}
	_, err := JudgeReplyEffect(t.Context(), record, candidates, BotIdentity{}, runner.run)
	if err == nil || !strings.Contains(err.Error(), "当时尚不存在或已结束观察") {
		t.Fatalf("应拒绝越界候选: %v", err)
	}
}

func TestBuildJudgePromptSectionsAndBudget(t *testing.T) {
	record := &Record{
		EffectID:  "EFF-1",
		CreatedAt: FormatTimestamp(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)),
		Reply: ReplySnapshot{
			TargetMessageID: "M2",
			ReplyText:       "这是 Bot 的回复正文",
		},
		ContextSnapshot: []ContextItem{
			{MessageID: "M1", Source: "user", Role: "user", Timestamp: "2026-03-01T11:58:00+08:00", Text: "前情消息",
				Sender: &ContextSender{DisplayName: "小明"}},
			{MessageID: "M2", Source: "user", Role: "user", Timestamp: "2026-03-01T11:59:00+08:00", Text: "触发消息",
				Sender: &ContextSender{DisplayName: "小红"}},
			{MessageID: "M3", Source: "tool_result", Role: "tool", Timestamp: "2026-03-01T11:59:30+08:00", Text: "工具结果不应出现"},
		},
		FollowupMessages: []FollowupSnapshot{{
			MessageID:          "F1",
			Timestamp:          "2026-03-01T12:01:00+08:00",
			Cardname:           "小刚",
			VisibleText:        "那按你说的试试",
			CandidateEffectIDs: []string{"EFF-1"},
		}},
	}
	candidates := []*Record{{
		EffectID:  "EFF-1",
		CreatedAt: FormatTimestamp(time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)),
		Reply:     ReplySnapshot{ReplyText: "这是 Bot 的回复正文"},
	}}
	prompt := BuildJudgePrompt(record, candidates, BotIdentity{Nickname: "麦麦", AliasNames: []string{"麦穗", "麦麦"}})

	for _, want := range []string{
		"回复效果评估标准版本：v6。",
		"Bot 身份名称：麦麦、麦穗。",
		"Bot 回复前的聊天上下文（按发送顺序排列）：",
		"待评估的 Bot 回复：",
		"- candidate_id=c1 time=",
		"Bot: 这是 Bot 的回复正文",
		"Bot 回复后的用户消息（按发送顺序排列）：",
		"message_id=F1 小刚",
		"允许候选（不代表相关）=[c1]",
		"策略 primary 只能是 answer/opinion",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt 缺少片段 %q", want)
		}
	}
	for _, unwanted := range []string{"工具结果不应出现", "M3"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt 不应包含 %q", unwanted)
		}
	}
	if !strings.Contains(prompt, "（触发当前 Bot 回复）") {
		t.Fatal("目标消息应带触发标记")
	}
}

func TestBuildJudgePromptTruncatesLongContent(t *testing.T) {
	record := &Record{
		EffectID:         "EFF-1",
		ContextSnapshot:  []ContextItem{},
		FollowupMessages: []FollowupSnapshot{},
		Reply:            ReplySnapshot{ReplyText: strings.Repeat("很长的回复内容", 500)},
	}
	candidates := []*Record{{EffectID: "EFF-1", Reply: ReplySnapshot{ReplyText: strings.Repeat("很长的回复内容", 500)}}}
	prompt := BuildJudgePrompt(record, candidates, BotIdentity{Nickname: "麦麦"})
	if len([]rune(prompt)) > MaxPromptChars+400 {
		t.Fatalf("prompt 长度应受 MAX_PROMPT_CHARS 约束, got %d", len([]rune(prompt)))
	}
	if !strings.Contains(prompt, "…") {
		t.Fatal("超长内容应被截断并带省略号")
	}
}

// 确定性评分（对照 scoring.py 的手算结果）。
func TestResponseScoreMatchesReferenceMath(t *testing.T) {
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{
			{
				MessageID: "F1", UserID: "U2",
				Associations: []ReplyAssociation{{
					EffectID: "EFF-1", AttributionType: AttributionExplicitQuote,
					AttributionConfidence: 1.0, EvaluatorConfidence: 0.6,
				}},
			},
			{
				MessageID: "F2", UserID: "U3",
				Associations: []ReplyAssociation{{
					EffectID: "EFF-1", AttributionType: AttributionSemantic,
					AttributionConfidence: 0.9, EvaluatorConfidence: 0.7,
				}},
			},
		},
	}
	// presence=1.0; per_user={U2:1.0, U3:0.9} → breadth=1.9/3; depth=1.9/5
	// score = 100*(0.45*1.0+0.25*(1.9/3)+0.20*(1.9/5))/0.9
	score, confidence := CalculateResponseScore(record)
	wantScore := roundTo(100.0*(0.45*1.0+0.25*(1.9/3.0)+0.20*(1.9/5.0))/0.90, 2)
	if score != wantScore {
		t.Fatalf("回应度计算不符: got %v want %v", score, wantScore)
	}
	perUserReliability := (1.0 + 0.9) / 2.0 * (0.75 + 0.25*(2.0/3.0))
	if confidence != roundTo(perUserReliability, 4) {
		t.Fatalf("回应度置信度不符: got %v want %v", confidence, roundTo(perUserReliability, 4))
	}
}

func TestResponseScoreZeroWithoutAssociations(t *testing.T) {
	record := &Record{EffectID: "EFF-1"}
	score, confidence := CalculateResponseScore(record)
	if score != 0 || confidence != 0 {
		t.Fatalf("无关联时应为 0 分: %v %v", score, confidence)
	}
}

func TestClassifyReceptionKeepsCategoryOrder(t *testing.T) {
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{
			{MessageID: "F1", UserID: "U1", Associations: []ReplyAssociation{{
				EffectID: "EFF-1", AttributionType: AttributionSemantic, AttributionConfidence: 0.9,
				StanceTarget: "bot_content", Stance: "rejection", EvaluatorConfidence: 0.8,
			}}},
			{MessageID: "F2", UserID: "U2", Associations: []ReplyAssociation{{
				EffectID: "EFF-1", AttributionType: AttributionSemantic, AttributionConfidence: 0.9,
				StanceTarget: "bot_persona", Stance: "appreciation", EvaluatorConfidence: 0.8,
			}}},
			// 话题/第三方情绪不计入 Bot 定向反馈。
			{MessageID: "F3", UserID: "U3", Associations: []ReplyAssociation{{
				EffectID: "EFF-1", AttributionType: AttributionSemantic, AttributionConfidence: 0.9,
				StanceTarget: "topic_or_third_party", Stance: "appreciation", EvaluatorConfidence: 0.8,
			}}},
		},
	}
	categories, counts, confidence := ClassifyReception(record)
	if len(categories) != 2 || categories[0] != "appreciation" || categories[1] != "rejection" {
		t.Fatalf("分类顺序应遵循 RECEPTION_CATEGORY_ORDER: %v", categories)
	}
	if counts["appreciation"] != 1 || counts["rejection"] != 1 {
		t.Fatalf("分类计数不符: %v", counts)
	}
	if confidence <= 0 {
		t.Fatalf("置信度应为正: %v", confidence)
	}
}

func TestClassifyReceptionExcludesZeroWeight(t *testing.T) {
	record := &Record{
		EffectID: "EFF-1",
		FollowupMessages: []FollowupSnapshot{{MessageID: "F1", UserID: "U1", Associations: []ReplyAssociation{{
			EffectID: "EFF-1", AttributionType: AttributionSemantic, AttributionConfidence: 0.9,
			StanceTarget: "bot_content", Stance: "appreciation", EvaluatorConfidence: 0,
		}}}},
	}
	categories, counts, confidence := ClassifyReception(record)
	if len(categories) != 0 || len(counts) != 0 || confidence != 0 {
		t.Fatalf("评估置信度为 0 的关联权重应为 0: %v %v %v", categories, counts, confidence)
	}
}

func TestConversationScorePenalizesWrongPush(t *testing.T) {
	base := &Record{
		EffectID:         "EFF-1",
		PreActivityCount: 2,
		FollowupMessages: []FollowupSnapshot{{
			MessageID: "F1", UserID: "U1", LatencySeconds: 60,
			Associations: []ReplyAssociation{{
				EffectID: "EFF-1", AttributionType: AttributionSemantic, AttributionConfidence: 0.9,
				Contribution: "advance", EvaluatorConfidence: 0.8,
			}},
		}},
	}
	good, _ := CalculateConversationScore(base)

	bad := base.Clone()
	bad.FollowupMessages[0].Associations[0].Contribution = "wrong_push"
	poor, _ := CalculateConversationScore(bad)
	if !(poor < good) {
		t.Fatalf("wrong_push 应显著拉低聊天推动度: good=%v poor=%v", good, poor)
	}
	if poor != 0 {
		t.Fatalf("纯 wrong_push 得分应为 0: %v", poor)
	}
}

func TestScoreRecordConfidenceNullWithoutAssociations(t *testing.T) {
	scores := ScoreRecord(&Record{EffectID: "EFF-1"})
	if scores.Confidence != nil {
		t.Fatalf("无关联时 confidence 应为 null: %v", *scores.Confidence)
	}
	if scores.ResponseScore != 0 || scores.ConversationScore != 0 {
		t.Fatalf("无关联时两项得分应为 0: %+v", scores)
	}
	// 置信度权重表：有 reception 时三项加权（0.40/0.30/0.30）。
	if scores.ReceptionEvidenceConfidence != 0 {
		t.Fatalf("无情绪分类时证据置信度应为 0: %v", scores.ReceptionEvidenceConfidence)
	}
}

func TestActivityBucketBoundaries(t *testing.T) {
	cases := map[int]string{0: "low", 1: "low", 2: "medium", 5: "medium", 6: "high"}
	for input, want := range cases {
		if got := ActivityBucket(input); got != want {
			t.Fatalf("ActivityBucket(%d)=%s want %s", input, got, want)
		}
	}
}

func TestNormalizeTextForPrompt(t *testing.T) {
	if got := NormalizeTextForPrompt("  a\t\n b  ", 100); got != "a b" {
		t.Fatalf("空白折叠不符: %q", got)
	}
	got := NormalizeTextForPrompt(strings.Repeat("字", 10), 5)
	if len([]rune(got)) != 5 || !strings.HasSuffix(got, "…") {
		t.Fatalf("截断不符: %q", got)
	}
}

func TestCountPreActivity(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	snapshot := []ContextItem{
		{MessageID: "M1", Role: "user", Timestamp: FormatTimestamp(now.Add(-3 * time.Minute))},
		{MessageID: "M2", Role: "user", Timestamp: FormatTimestamp(now.Add(-1 * time.Minute))},
		{MessageID: "M3", Role: "assistant", Timestamp: FormatTimestamp(now.Add(-10 * time.Second))},
		{MessageID: "M4", Role: "user", Timestamp: "非法时间"},
	}
	if got := countPreActivity(snapshot, now); got != 1 {
		t.Fatalf("2 分钟窗口内应计 1 条（排除 assistant 与非法时间）, got %d", got)
	}
}

// Record JSON 往返（导入导出与落库共用）。
func TestRecordJSONRoundTrip(t *testing.T) {
	record := &Record{
		EffectID:   "EFF-1",
		Status:     StatusFinalized,
		CreatedAt:  "2026-03-01T12:00:00+08:00",
		UpdatedAt:  "2026-03-01T12:30:00+08:00",
		Session:    SessionSnapshot{SessionID: "s1", ChatType: "group"},
		Reply:      ReplySnapshot{ReplyText: "hi", ReplySegments: []string{"hi"}},
		TargetUser: UserSnapshot{UserID: "U1", Nickname: "n"},
		FollowupMessages: []FollowupSnapshot{{
			MessageID: "F1",
			Associations: []ReplyAssociation{{
				EffectID: "EFF-1", AttributionType: AttributionSemantic, AttributionConfidence: 0.5,
			}},
			Attachments: []VisualAttachment{{Kind: "image", Index: 0, Hash: "abc"}},
		}},
		Scores:            &Scores{ResponseScore: 50, ReceptionCounts: map[string]int{"neutral": 1}},
		EvaluationVersion: EvaluationVersion,
		SchemaVersion:     SchemaVersion,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeRecordJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if restored.EffectID != record.EffectID || restored.Status != record.Status ||
		restored.Scores.ResponseScore != 50 || len(restored.FollowupMessages) != 1 ||
		restored.FollowupMessages[0].Associations[0].AttributionType != AttributionSemantic ||
		restored.FollowupMessages[0].Attachments[0].Hash != "abc" {
		t.Fatalf("往返结果不符: %+v", restored)
	}
	if _, err := DecodeRecordJSON([]byte(`{"status":"pending"}`)); err == nil {
		t.Fatal("缺少 effect_id 应报错")
	}
}
