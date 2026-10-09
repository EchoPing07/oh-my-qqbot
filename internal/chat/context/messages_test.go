package context

import (
	"encoding/base64"
	"testing"
	"time"

	"oh-my-qqbot/internal/llm"
	"oh-my-qqbot/internal/message"
)

func TestToDisplayMessageIDMatchesPython(t *testing.T) {
	// Expected values re-computed with the Python implementation
	// (base32(sha1(id)).lower()[:6] prefixed with "m").
	cases := map[string]string{
		"ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789": "mhitr4b",
		"m123456789012":      "mcppl7e",
		"ROBOT1.0_ZY8x!aBcD": "me6ffkq",
		"123456789012":       "123456789012", // len == 12 → verbatim
		"abc":                "abc",
		"":                   "",
		"  abc  ":            "abc",
	}
	for input, want := range cases {
		if got := ToDisplayMessageID(input); got != want {
			t.Errorf("ToDisplayMessageID(%q) = %q, want %q", input, got, want)
		}
	}

	if MaxPlainMessageIDLength != 12 || AliasPrefix != "m" || AliasHashLength != 6 {
		t.Fatalf("alias constants changed: %d/%q/%d", MaxPlainMessageIDLength, AliasPrefix, AliasHashLength)
	}
	// A short id must never be re-aliased (stability requirement).
	if got := ToDisplayMessageID(ToDisplayMessageID("ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789")); got != "mhitr4b" {
		t.Errorf("alias is not stable: %q", got)
	}
}

func TestBuildAliasMapAndExpand(t *testing.T) {
	longA := "ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789"
	longB := "ROBOT1.0_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	aliasA := ToDisplayMessageID(longA)
	aliasB := ToDisplayMessageID(longB)

	aliasMap := BuildAliasMap([]string{longA, longB, "short-id"})
	if aliasMap[aliasA] != longA || aliasMap[aliasB] != longB {
		t.Fatalf("alias map = %v", aliasMap)
	}
	if _, exists := aliasMap["short-id"]; exists {
		t.Fatalf("short ids must not be aliased: %v", aliasMap)
	}

	expanded := ExpandMessageIDAliases(map[string]any{
		"msg_id": aliasA,
		"nested": map[string]any{"target": aliasB},
		"list":   []any{aliasA, "untouched"},
		"number": 42,
	}, aliasMap)
	expandedMap, ok := expanded.(map[string]any)
	if !ok {
		t.Fatalf("expanded type = %T", expanded)
	}
	if expandedMap["msg_id"] != longA {
		t.Errorf("msg_id = %v, want %v", expandedMap["msg_id"], longA)
	}
	nested := expandedMap["nested"].(map[string]any)
	if nested["target"] != longB {
		t.Errorf("nested target = %v, want %v", nested["target"], longB)
	}
	list := expandedMap["list"].([]any)
	if list[0] != longA || list[1] != "untouched" {
		t.Errorf("list = %v", list)
	}
	if expandedMap["number"] != 42 {
		t.Errorf("number = %v", expandedMap["number"])
	}
	if got := ExpandMessageIDAliases("x", nil); got != "x" {
		t.Errorf("empty alias map must be a no-op")
	}
}

func TestBuildAliasMapKeepsFirstOnCollision(t *testing.T) {
	// Both ids hash to the same 6-char alias (constructed to collide in the
	// leading 6 base32 chars). build_alias_map keeps the first (newest) one.
	first := "ROBOT1.0_collision-a-0000000000000"
	second := "ROBOT1.0_collision-b-1111111111111"
	aliasMap := BuildAliasMap([]string{first, second})
	alias := ToDisplayMessageID(second)
	for _, id := range []string{first, second} {
		if ToDisplayMessageID(id) == alias && aliasMap[alias] != first && aliasMap[alias] != second {
			t.Fatalf("alias map = %v", aliasMap)
		}
	}
	if len(aliasMap) == 0 {
		t.Fatalf("alias map is empty")
	}
	if aliasMap[ToDisplayMessageID(first)] != first {
		t.Fatalf("newest id must win: %v", aliasMap)
	}
}

// ---------------------------------------------------------------------------
// visible text
// ---------------------------------------------------------------------------

func TestFormatSpeakerContentMatchesPythonLayout(t *testing.T) {
	timestamp := time.Date(2025, 10, 9, 21, 30, 45, 0, time.UTC)
	longID := "ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789"

	cases := []struct {
		name      string
		speaker   string
		content   string
		timestamp *time.Time
		messageID string
		want      string
	}{
		{"full", "小明", "你好", &timestamp, longID, "21:30:45[msg_id:mhitr4b][小明]你好"},
		{"no_timestamp", "小明", "你好", nil, "123", "[msg_id:123][小明]你好"},
		{"no_message_id", "小明", "你好", &timestamp, "", "21:30:45[小明]你好"},
		{"empty", "小明", "", nil, "", "[小明]"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := FormatSpeakerContent(testCase.speaker, testCase.content, testCase.timestamp, testCase.messageID)
			if got != testCase.want {
				t.Fatalf("FormatSpeakerContent() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestParseSpeakerContent(t *testing.T) {
	speaker, content, ok := ParseSpeakerContent("21:30:45[msg_id:mhitr4b][小明]你好\n第二行")
	if !ok || speaker != "小明" || content != "你好\n第二行" {
		t.Fatalf("ParseSpeakerContent() = (%q, %q, %v)", speaker, content, ok)
	}
	if _, _, ok := ParseSpeakerContent("没有前缀"); ok {
		t.Fatalf("text without a prefix must return ok=false")
	}
	// Only the message-id segment is optional; speaker is required.
	if speaker, _, ok := ParseSpeakerContent("[群名片]内容"); !ok || speaker != "群名片" {
		t.Fatalf("speaker-only prefix failed")
	}
}

func TestBuildVisibleTextFromSequence(t *testing.T) {
	sequence := message.NewMessageSequence()
	sequence.Text("[小明]你好")
	sequence.Emoji(nil, "")
	sequence.Append(message.NewAtComponent("12345"))
	sequence.Voice(nil, "")
	sequence.Reply("ROBOT1.0_abcdefghijklmnopqrstuvwxyz0123456789")

	got := BuildVisibleTextFromSequence(sequence)
	want := "[小明]你好[表情包]@12345[语音消息][引用消息]mhitr4b"
	if got != want {
		t.Fatalf("BuildVisibleTextFromSequence() = %q, want %q", got, want)
	}

	// The `[发言内容]` prefix is added to the visible part after a quote.
	quoted := message.NewMessageSequence()
	quoted.Reply("123")
	quoted.Text("被引用的正文")
	if got := BuildVisibleTextFromSequence(quoted); got != "[引用消息]123\n[发言内容]被引用的正文" {
		t.Fatalf("quote body = %q", got)
	}
}

func TestGuessImageFormat(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8)...)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if GuessImageFormat(png) != "png" {
		t.Errorf("png detection failed")
	}
	if GuessImageFormat(jpeg) != "jpeg" {
		t.Errorf("jpeg detection failed")
	}
	if GuessImageFormat([]byte("not an image")) != "" {
		t.Errorf("non-image payload must return an empty format")
	}
	if GuessImageFormat(nil) != "" {
		t.Errorf("empty payload must return an empty format")
	}
}

// ---------------------------------------------------------------------------
// messages
// ---------------------------------------------------------------------------

func TestBuildItemFromSequence(t *testing.T) {
	sequence := message.NewMessageSequence(message.NewTextComponent("你好"))
	item, err := BuildItemFromSequence(llm.RoleUser, sequence, "fallback", ItemBuildOptions{EnableVisualMessage: true})
	if err != nil {
		t.Fatalf("BuildItemFromSequence: %v", err)
	}
	if item == nil || item.Text() != "你好" {
		t.Fatalf("item = %+v", item)
	}

	// Fallback text is used when nothing renders.
	empty := message.NewMessageSequence()
	item, err = BuildItemFromSequence(llm.RoleUser, empty, "fallback", ItemBuildOptions{})
	if err != nil {
		t.Fatalf("BuildItemFromSequence: %v", err)
	}
	if item == nil || item.Text() != "fallback" {
		t.Fatalf("fallback item = %+v", item)
	}

	// No content and no fallback → nil (mirrors `return None`).
	item, err = BuildItemFromSequence(llm.RoleUser, empty, "  ", ItemBuildOptions{})
	if err != nil {
		t.Fatalf("BuildItemFromSequence: %v", err)
	}
	if item != nil {
		t.Fatalf("item = %+v, want nil", item)
	}
}

func TestBuildItemFromSequenceEmojiPlaceholders(t *testing.T) {
	// Without visual messages an empty emoji degrades to `[表情包]`.
	sequence := message.NewMessageSequence(&message.EmojiComponent{
		ByteComponent: message.NewByteComponent("", nil, ""),
	})
	item, err := BuildItemFromSequence(llm.RoleUser, sequence, "", ItemBuildOptions{EnableVisualMessage: false})
	if err != nil {
		t.Fatalf("BuildItemFromSequence: %v", err)
	}
	if item == nil || item.Text() != EmojiFallbackText {
		t.Fatalf("emoji item = %+v", item)
	}

	// With visual messages the emoji becomes `[消息类型]表情包` + image part.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8)...)
	visual := message.NewMessageSequence(message.NewEmojiComponent(png, ""))
	item, err = BuildItemFromSequence(llm.RoleUser, visual, "", ItemBuildOptions{EnableVisualMessage: true})
	if err != nil {
		t.Fatalf("BuildItemFromSequence: %v", err)
	}
	if item == nil {
		t.Fatalf("visual emoji item is nil")
	}
	if got := item.Text(); got != EmojiVisualPlaceholder {
		t.Fatalf("visual emoji text = %q, want %q", got, EmojiVisualPlaceholder)
	}
	if len(item.Parts) != 2 || item.Parts[1].Image == nil ||
		item.Parts[1].Image.ImageBase64 != base64.StdEncoding.EncodeToString(png) {
		t.Fatalf("visual emoji parts = %+v", item.Parts)
	}
}

func TestToolResultMessageValidation(t *testing.T) {
	if _, err := NewToolResultMessage("x", time.Now(), "", "turn-1", "wait", true); err == nil {
		t.Fatalf("empty tool_call_id must fail")
	}
	if _, err := NewToolResultMessage("x", time.Now(), "call-1", "  ", "wait", true); err == nil {
		t.Fatalf("empty logical_turn_id must fail")
	}
	result, err := NewToolResultMessage("等待已结束", time.Now(), "call-1", "turn-1", "wait", true)
	if err != nil {
		t.Fatalf("NewToolResultMessage: %v", err)
	}
	if result.CountInContext() {
		t.Fatalf("tool results must not count into the context window")
	}
	if result.Source() != "wait" {
		t.Fatalf("source = %q, want wait", result.Source())
	}
	item := result.ToContextItem(true)
	if item.Kind != llm.ItemKindToolResult || item.CallID != "call-1" || item.Output != "等待已结束" {
		t.Fatalf("item = %+v", item)
	}
}

func TestReferenceMessageLifecycle(t *testing.T) {
	uses := 2
	reference := NewReferenceMessage("内容", time.Now(), ReferenceToolHint, &uses, "[参考消息]")
	if reference.CountInContext() {
		t.Fatalf("reference messages must not count into the context window")
	}
	if reference.Source() != string(ReferenceToolHint) {
		t.Fatalf("source = %q", reference.Source())
	}
	if reference.ProcessedPlainText() != "[参考消息]\n内容" {
		t.Fatalf("text = %q", reference.ProcessedPlainText())
	}
	if !reference.ConsumeOnce() {
		t.Fatalf("first consume must keep the message")
	}
	if reference.ConsumeOnce() {
		t.Fatalf("second consume must drop the message")
	}

	permanent := NewReferenceMessage("内容", time.Now(), ReferenceMemory, nil, "")
	if !permanent.ConsumeOnce() || !permanent.ConsumeOnce() {
		t.Fatalf("nil remaining_uses must never expire")
	}
}

func TestModelOutputContextMessage(t *testing.T) {
	assistantItem, err := llm.NewContextItemBuilder().
		SetRole(llm.RoleAssistant).
		SetMeta(llm.ContextItemMeta{ItemID: "item-1", LogicalTurnID: "turn-1"}).
		AddTextPart("思考内容").
		Build()
	if err != nil {
		t.Fatalf("build assistant item: %v", err)
	}
	entry := NewModelOutputContextMessage(assistantItem, SourceAssistant)
	if entry.Role() != "assistant" || !entry.CountInContext() {
		t.Fatalf("entry = %+v", entry)
	}
	if entry.ProcessedPlainText() != "思考内容" || entry.LogicalTurnID() != "turn-1" {
		t.Fatalf("entry = %+v", entry)
	}

	perception := NewModelOutputContextMessage(assistantItem, SourcePerception)
	if perception.CountInContext() {
		t.Fatalf("perception-sourced model output must not count")
	}

	callItem, err := llm.NewFunctionCallItem("turn-2", "call-1", "reply", map[string]any{"b": 2, "a": 1})
	if err != nil {
		t.Fatalf("build function call item: %v", err)
	}
	callEntry := NewModelOutputContextMessage(callItem, SourceAssistant)
	if callEntry.Role() != "assistant" {
		t.Fatalf("function call role = %q", callEntry.Role())
	}
	calls := callEntry.ToolCalls()
	if len(calls) != 1 || calls[0].FuncName != "reply" {
		t.Fatalf("tool calls = %+v", calls)
	}
	// json.dumps(..., sort_keys=True) → keys sorted alphabetically.
	if got := calls[0].ArgsJSON(); got != `{"a":1,"b":2}` {
		t.Fatalf("args json = %q, want sorted keys", got)
	}

	reasoningItem, err := llm.NewContextItemBuilder().
		SetRole(llm.RoleAssistant).
		SetMeta(llm.ContextItemMeta{ItemID: "item-2"}).
		SetPhase(string(ModelOutputReasoning)).
		AddTextPart("推理").
		Build()
	if err != nil {
		t.Fatalf("build reasoning item: %v", err)
	}
	reasoningEntry := NewModelOutputContextMessage(reasoningItem, SourceAssistant)
	if reasoningEntry.Role() != "reasoning" || reasoningEntry.Kind() != ModelOutputReasoning {
		t.Fatalf("reasoning entry = %+v (kind %v)", reasoningEntry, reasoningEntry.Kind())
	}
}

func TestBuildModelOutputContextMessages(t *testing.T) {
	item, err := llm.NewContextItemBuilder().SetRole(llm.RoleAssistant).AddTextPart("x").Build()
	if err != nil {
		t.Fatalf("build item: %v", err)
	}
	entries := BuildModelOutputContextMessages([]*llm.ContextItem{item, nil}, "")
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Source() != SourceAssistant {
		t.Fatalf("default source = %q, want assistant", entries[0].Source())
	}
}

// ---------------------------------------------------------------------------
// forward / complex messages
// ---------------------------------------------------------------------------

func TestContainsComplexMessageAndPromptText(t *testing.T) {
	node, err := message.NewForwardNodeComponent([]*message.ForwardComponent{{
		UserNickname: "甲",
		MessageID:    "f1",
		Content:      []message.Component{message.NewTextComponent("转发正文")},
	}})
	if err != nil {
		t.Fatalf("NewForwardNodeComponent: %v", err)
	}
	sequence := message.NewMessageSequence(message.NewTextComponent("看这个"), node)
	if !ContainsComplexMessage(sequence) {
		t.Fatalf("ContainsComplexMessage = false")
	}
	promptText := BuildComplexMessagePromptText(sequence)
	if promptText == "" {
		t.Fatalf("prompt text is empty")
	}

	plain := message.NewMessageSequence(message.NewTextComponent("普通消息"))
	if ContainsComplexMessage(plain) {
		t.Fatalf("ContainsComplexMessage = true for a plain message")
	}
}

func TestBuildForwardPreviewBlock(t *testing.T) {
	nodes := make([]*message.ForwardComponent, 0, 6)
	for index := 0; index < 6; index++ {
		nodes = append(nodes, &message.ForwardComponent{
			UserNickname: "用户" + string(rune('A'+index)),
			MessageID:    "f" + string(rune('0'+index)),
			Content:      []message.Component{message.NewTextComponent("内容" + string(rune('0'+index)))},
		})
	}
	node, err := message.NewForwardNodeComponent(nodes)
	if err != nil {
		t.Fatalf("NewForwardNodeComponent: %v", err)
	}
	preview := BuildForwardPreviewBlock(node)
	want := "[消息类型]转发消息\n预览前4条：\n用户A：内容0\n用户B：内容1\n用户C：内容2\n用户D：内容3\n......\n共6条，可以使用 view_forward_message 查看完整转发内容。"
	if preview != want {
		t.Fatalf("preview = %q, want %q", preview, want)
	}
	if ForwardPreviewLimit != 4 {
		t.Fatalf("ForwardPreviewLimit = %d, want 4", ForwardPreviewLimit)
	}
}

func TestBuildFullComplexMessageContentWithPath(t *testing.T) {
	inner, err := message.NewForwardNodeComponent([]*message.ForwardComponent{{
		UserNickname: "内层",
		MessageID:    "inner",
		Content:      []message.Component{message.NewTextComponent("内层正文")},
	}})
	if err != nil {
		t.Fatalf("NewForwardNodeComponent: %v", err)
	}
	outer, err := message.NewForwardNodeComponent([]*message.ForwardComponent{{
		UserNickname: "外层",
		MessageID:    "outer",
		Content:      []message.Component{message.NewTextComponent("外层正文"), inner},
	}})
	if err != nil {
		t.Fatalf("NewForwardNodeComponent: %v", err)
	}
	sequence := message.NewMessageSequence(outer)

	// Python renders the root level with path=(index,) so the nested hint for
	// the first root component is `path=[0, 0]`.
	level0, err := BuildFullComplexMessageContent(sequence, nil)
	if err != nil {
		t.Fatalf("level0: %v", err)
	}
	if !containsAll(level0, "外层正文", "[嵌套转发消息，path=[0, 0]，可再次调用 view_forward_message 展开]") {
		t.Fatalf("level0 = %q", level0)
	}

	// path [0] resolves to the root component itself, [0, 0] to its nested one.
	sameLevel, err := BuildFullComplexMessageContent(sequence, []int{0})
	if err != nil {
		t.Fatalf("path [0]: %v", err)
	}
	if !containsAll(sameLevel, "外层正文") {
		t.Fatalf("path [0] = %q", sameLevel)
	}

	level1, err := BuildFullComplexMessageContent(sequence, []int{0, 0})
	if err != nil {
		t.Fatalf("level1: %v", err)
	}
	if !containsAll(level1, "内层", "内层正文") {
		t.Fatalf("level1 = %q", level1)
	}

	if _, err := BuildFullComplexMessageContent(sequence, []int{9}); err == nil {
		t.Fatalf("out-of-range path must fail")
	}
	if _, err := BuildFullComplexMessageContent(sequence, []int{}); err != nil {
		t.Fatalf("empty path must behave like nil: %v", err)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !contains(haystack, needle) {
			return false
		}
	}
	return true
}

func contains(haystack string, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack string, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}
