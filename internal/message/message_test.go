package message

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCalculateSessionIDMatchesPython pins the md5 digests produced by
// `python -c "import hashlib;print(hashlib.md5(...).hexdigest())"` for the
// exact component joins of SessionUtils.calculate_session_id.
func TestCalculateSessionIDMatchesPython(t *testing.T) {
	tests := []struct {
		name     string
		platform string
		options  SessionIDOptions
		expect   string
		join     string
	}{
		{
			name:     "group only",
			platform: "qq",
			options:  SessionIDOptions{GroupID: "123456"},
			expect:   "180bb5b597902c53e35c9e8b1c2fd6f1",
			join:     "qq_123456",
		},
		{
			name:     "private only",
			platform: "qq",
			options:  SessionIDOptions{UserID: "123456"},
			expect:   "c3274d16e5efdbc47295676061f27666",
			join:     "qq_123456_private",
		},
		{
			name:     "account component before group",
			platform: "qq",
			options:  SessionIDOptions{GroupID: "123456", AccountID: "bot1"},
			expect:   "6af6fc487a4c15536bd8fc762665c9fa",
			join:     "qq_account:bot1_123456",
		},
		{
			name:     "scope component",
			platform: "qq",
			options:  SessionIDOptions{GroupID: "123456", Scope: "routeA"},
			expect:   "08f985f10a33dc298e41a5e05f46322d",
			join:     "qq_scope:routeA_123456",
		},
		{
			name:     "private with account and scope",
			platform: "webui",
			options:  SessionIDOptions{UserID: "10001", AccountID: "self-42", Scope: "ws-7"},
			expect:   "fdd3d26fae90056e5c4a129151a14efe",
			join:     "webui_account:self-42_scope:ws-7_10001_private",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if components := SessionIDComponents(test.platform, test.options.UserID, test.options.GroupID, test.options.AccountID, test.options.Scope); strings.Join(components, "_") != test.join {
				t.Fatalf("component join = %q, want %q", strings.Join(components, "_"), test.join)
			}
			got, err := CalculateSessionID(test.platform, test.options)
			if err != nil {
				t.Fatalf("CalculateSessionID error: %v", err)
			}
			if got != test.expect {
				t.Fatalf("session id = %s, want %s", got, test.expect)
			}
		})
	}
}

// TestCalculateSessionIDGroupBeatsUser mirrors the Python branch order: the
// group branch wins whenever group_id is truthy.
func TestCalculateSessionIDGroupBeatsUser(t *testing.T) {
	withGroup, err := CalculateSessionIDStrings("qq", "u1", "g1", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := md5Hex("qq_g1")
	if withGroup != want {
		t.Fatalf("group branch = %s, want %s", withGroup, want)
	}
	withoutGroup, err := CalculateSessionIDStrings("qq", "u1", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if withoutGroup != md5Hex("qq_u1_private") {
		t.Fatalf("private branch = %s", withoutGroup)
	}
}

// TestCalculateSessionIDRequiresIdentity mirrors the Python ValueError.
func TestCalculateSessionIDRequiresIdentity(t *testing.T) {
	if _, err := CalculateSessionID("qq", SessionIDOptions{}); err != ErrMissingSessionIdentity {
		t.Fatalf("err = %v, want ErrMissingSessionIdentity", err)
	}
	if _, err := CalculateSessionIDStrings("qq", "  ", "", "", ""); err != ErrMissingSessionIdentity {
		t.Fatalf("blank user id err = %v, want ErrMissingSessionIdentity", err)
	}
}

// TestCalculateSessionIDEmptyRouteComponents mirrors Python truthiness: empty
// account_id/scope add no component at all (no stray underscores).
func TestCalculateSessionIDEmptyRouteComponents(t *testing.T) {
	got, err := CalculateSessionIDStrings("qq", "", "123456", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != md5Hex("qq_123456") {
		t.Fatalf("empty route components changed the join: %s", got)
	}
	if components := SessionIDComponents("qq", "", "123456", "", ""); len(components) != 2 {
		t.Fatalf("components = %v", components)
	}
}

func TestSessionRouteComponentsAliases(t *testing.T) {
	tests := []struct {
		name        string
		metadata    map[string]any
		wantAccount string
		wantScope   string
	}{
		{"canonical", map[string]any{"platform_io_account_id": "a1", "platform_io_scope": "s1"}, "a1", "s1"},
		{"fallback keys", map[string]any{"self_id": 42, "connection_id": "conn-9"}, "42", "conn-9"},
		{"priority wins", map[string]any{"platform_io_account_id": "a1", "account_id": "a2"}, "a1", ""},
		{"blank skipped", map[string]any{"platform_io_account_id": "  ", "account_id": "a2"}, "a2", ""},
		{"empty", nil, "", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account, scope := SessionRouteComponents(test.metadata)
			if account != test.wantAccount || scope != test.wantScope {
				t.Fatalf("got (%q,%q), want (%q,%q)", account, scope, test.wantAccount, test.wantScope)
			}
		})
	}
}

// TestSessionIDMatchesCallSiteArguments replays the exact call of
// 参考/MaiBot/src/chat/message_receive/bot.py:759 with route metadata extracted
// from additional_config, confirming the positional/keyword mapping.
func TestSessionIDMatchesCallSiteArguments(t *testing.T) {
	message := NewSessionMessage("m1", Now(), "qq")
	message.MessageInfo.UserInfo = NewUserInfo("user-9", "bob", "")
	message.MessageInfo.GroupInfo = &GroupInfo{GroupID: "group-9", GroupName: "g"}
	message.MessageInfo.AdditionalConfig = map[string]any{
		"platform_io_account_id": "acct-1",
		"platform_io_scope":      "scope-1",
	}
	accountID, scope := SessionRouteComponents(message.AdditionalConfigMap())
	got, err := CalculateSessionID(message.Platform, SessionIDOptions{
		UserID:    message.MessageInfo.UserInfo.UserID,
		GroupID:   message.MessageInfo.GroupInfo.GroupID,
		AccountID: accountID,
		Scope:     scope,
	})
	if err != nil {
		t.Fatalf("CalculateSessionID: %v", err)
	}
	want := md5Hex("qq_account:acct-1_scope:scope-1_group-9")
	if got != want {
		t.Fatalf("session id = %s, want %s", got, want)
	}
}

// TestSessionIDForMessageMirrorsRegisterMessage checks the ChatManager path:
// route components come from additional_config and group beats user.
func TestSessionIDForMessageMirrorsRegisterMessage(t *testing.T) {
	message := NewSessionMessage("m1", Now(), "qq")
	message.MessageInfo.UserInfo = UserInfo{UserID: "10001", UserNickname: "alice"}
	message.MessageInfo.GroupInfo = &GroupInfo{GroupID: "g-1", GroupName: ""}
	message.MessageInfo.AdditionalConfig = map[string]any{"self_id": "bot-1"}
	if err := message.EnsureSessionID(); err != nil {
		t.Fatalf("EnsureSessionID: %v", err)
	}
	want := md5Hex("qq_account:bot-1_g-1")
	if message.SessionID != want {
		t.Fatalf("session id = %s, want %s", message.SessionID, want)
	}
}

// TestMessageSequenceJSONRoundTrip mirrors MessageSequence.to_dict/from_dict
// (msgpack → JSON per §6.2) including placeholders and unknown-type fallback.
func TestMessageSequenceJSONRoundTrip(t *testing.T) {
	sequence := &MessageSequence{}
	sequence.Text("hello ")
	sequence.At("42")
	sequence.Image([]byte{0x89, 'P', 'N', 'G'}, " ")
	sequence.Emoji(nil, "")
	sequence.Voice(nil, "")
	sequence.Append(&FileComponent{Name: "a.txt", Size: "12", URL: "https://x/y"})
	sequence.Reply("m-9")
	wrapped := &DictComponent{Data: map[string]any{"type": "unknown_kind", "data": map[string]any{"k": "v"}}}
	sequence.Append(wrapped)

	encoded, err := sequence.EncodeJSON()
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}

	decoded, err := DecodeJSON(encoded)
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	if len(decoded.Components) != len(sequence.Components) {
		t.Fatalf("component count = %d, want %d", len(decoded.Components), len(sequence.Components))
	}

	// Wire shape assertions (Python `to_dict` layout).
	var items []map[string]any
	if err := json.Unmarshal([]byte(encoded), &items); err != nil {
		t.Fatalf("unmarshal items: %v", err)
	}
	if items[0]["type"] != "text" || items[0]["data"] != "hello " {
		t.Fatalf("text item = %#v", items[0])
	}
	atData := items[1]["data"].(map[string]any)
	if atData["target_user_id"] != "42" || atData["target_user_nickname"] != nil {
		t.Fatalf("at item = %#v", items[1])
	}
	if items[2]["data"] != "" || items[2]["hash"] == "" {
		t.Fatalf("image item = %#v", items[2])
	}
	if items[3]["data"] != EmojiPlaceholder {
		t.Fatalf("emoji placeholder = %#v", items[3])
	}
	if items[4]["data"] != VoicePlaceholder {
		t.Fatalf("voice placeholder = %#v", items[4])
	}
	if items[7]["type"] != "dict" {
		t.Fatalf("unknown type degraded to %#v", items[7])
	}

	// Round-tripped value equality.
	if text := decoded.Components[0].(*TextComponent).Text; text != "hello " {
		t.Fatalf("text = %q", text)
	}
	if at := decoded.Components[1].(*AtComponent); at.TargetUserID != "42" {
		t.Fatalf("at = %#v", at)
	}
	if image := decoded.Components[2].(*ImageComponent); image.BinaryHash != sequence.Components[2].(*ImageComponent).BinaryHash {
		t.Fatalf("image hash drift: %s", image.BinaryHash)
	}
	if file := decoded.Components[5].(*FileComponent); file.Name != "a.txt" || file.Size != "12" {
		t.Fatalf("file = %#v", file)
	}
	if reply := decoded.Components[6].(*ReplyComponent); reply.TargetMessageID != "m-9" {
		t.Fatalf("reply = %#v", reply)
	}
}

// TestDecodeJSONAcceptsLegacyWrapper tolerates {"components":[...]} payloads
// produced by earlier Python fixtures.
func TestDecodeJSONAcceptsLegacyWrapper(t *testing.T) {
	sequence, err := DecodeJSON(`{"components":[{"type":"text","data":"hi"}]}`)
	if err != nil {
		t.Fatalf("DecodeJSON wrapper: %v", err)
	}
	if len(sequence.Components) != 1 || sequence.Components[0].(*TextComponent).Text != "hi" {
		t.Fatalf("components = %#v", sequence.Components)
	}
}

// TestForwardNodeRoundTrip mirrors the forward component wire layout.
func TestForwardNodeRoundTrip(t *testing.T) {
	node := &ForwardComponent{
		UserNickname: "alice",
		UserID:       StringPtr("1"),
		MessageID:    "fm-1",
		Content:      []Component{NewTextComponent("node text")},
	}
	forward, err := NewForwardNodeComponent([]*ForwardComponent{node})
	if err != nil {
		t.Fatalf("NewForwardNodeComponent: %v", err)
	}
	sequence := NewMessageSequence(forward)
	encoded, err := sequence.EncodeJSON()
	if err != nil {
		t.Fatalf("EncodeJSON: %v", err)
	}
	if !strings.Contains(encoded, `"type":"forward"`) {
		t.Fatalf("forward wire tag missing: %s", encoded)
	}
	decoded, err := DecodeJSON(encoded)
	if err != nil {
		t.Fatalf("DecodeJSON: %v", err)
	}
	decodedForward, ok := decoded.Components[0].(*ForwardNodeComponent)
	if !ok {
		t.Fatalf("component = %T", decoded.Components[0])
	}
	if len(decodedForward.ForwardComponents) != 1 {
		t.Fatalf("nodes = %d", len(decodedForward.ForwardComponents))
	}
	if got := decodedForward.ForwardComponents[0].Content[0].(*TextComponent).Text; got != "node text" {
		t.Fatalf("nested text = %q", got)
	}
	if _, err := NewForwardNodeComponent(nil); err != ErrEmptyForwardComponents {
		t.Fatalf("empty forward err = %v", err)
	}
}

// TestToTextMirrorsProcess covers the component → plain text rendering rules.
func TestToTextMirrorsProcess(t *testing.T) {
	sequence := &MessageSequence{}
	sequence.Text("hi")
	sequence.At("42")
	sequence.Append(&AtComponent{TargetUserID: "7", TargetUserCardname: StringPtr("Bob")})
	sequence.Append(&FileComponent{Name: "a.txt", Size: "12", MIMEType: "text/plain", URL: "u", FileID: "f"})
	sequence.Append(NewReplyComponent("m-1"))
	sequence.Append(&DictComponent{Data: map[string]any{"type": "json", "data": map[string]any{"text": "payload"}}})

	got := sequence.ToText(TextOptions{EnableVoiceTranscription: true})
	// @42 has no nickname → user id; @7 prefers the group cardname.
	if !strings.Contains(got, "@42") || !strings.Contains(got, "@Bob") {
		t.Fatalf("at rendering = %q", got)
	}
	if !strings.Contains(got, "[文件] a.txt，大小: 12，类型: text/plain，链接: u，文件ID: f") {
		t.Fatalf("file rendering = %q", got)
	}
	if !strings.Contains(got, "[回复了一条消息，但原消息已无法访问]") {
		t.Fatalf("reply fallback = %q", got)
	}
	if !strings.Contains(got, "payload") {
		t.Fatalf("dict rendering = %q", got)
	}
}

// TestToTextResolvesReplyThroughHook checks the injected DB lookup semantics.
func TestToTextResolvesReplyThroughHook(t *testing.T) {
	sequence := NewMessageSequence(NewReplyComponent("m-1"))
	options := DefaultTextOptions()
	options.LookupReply = func(targetMessageID string) (string, string, bool) {
		if targetMessageID != "m-1" {
			return "", "", false
		}
		return "original text", "Carol", true
	}
	got := sequence.ToText(options)
	if got != "[回复了Carol的消息: original text]" {
		t.Fatalf("reply rendering = %q", got)
	}
	// Second render hits the cached content and does not call the hook again.
	options.LookupReply = func(string) (string, string, bool) { return "", "", false }
	if again := sequence.ToText(options); again != "original text" {
		t.Fatalf("cached reply rendering = %q", again)
	}
}

// TestVoicePlaceholderRemoval mirrors
// SessionMessage._remove_voice_placeholder_text_components.
func TestVoicePlaceholderRemoval(t *testing.T) {
	sequence := NewMessageSequence(NewTextComponent("[voice]"), NewVoiceComponent([]byte{1}, ""), NewTextComponent("hi"))
	sequence.RemoveVoicePlaceholderTextComponents()
	if len(sequence.Components) != 2 {
		t.Fatalf("components = %d", len(sequence.Components))
	}
	if _, ok := sequence.Components[0].(*VoiceComponent); !ok {
		t.Fatalf("first component = %T", sequence.Components[0])
	}
}

// TestForwardToTextMirrorsPython covers the recursive forward rendering.
func TestForwardToTextMirrorsPython(t *testing.T) {
	inner := &ForwardComponent{
		UserNickname: "dave",
		MessageID:    "n1",
		Content:      []Component{NewTextComponent("hello")},
	}
	node, _ := NewForwardNodeComponent([]*ForwardComponent{inner})
	got := NewMessageSequence(node).ToText(DefaultTextOptions())
	want := "【合并转发消息: \n 【dave】: hello\n】"
	if got != want {
		t.Fatalf("forward text = %q, want %q", got, want)
	}
}

func TestAssignMessageIDMirrorsPython(t *testing.T) {
	// Small batches use a single-digit suffix (1..9) and a 1-based index.
	if id := AssignMessageID(0, 3, 0); id != "m11" {
		t.Fatalf("id = %s", id)
	}
	if id := AssignMessageID(2, 3, 8); id != "m39" {
		t.Fatalf("id = %s", id)
	}
	// Large batches use a two-digit suffix (10..99).
	if id := AssignMessageID(0, 101, 0); id != "m110" {
		t.Fatalf("id = %s", id)
	}
	if id := AssignMessageID(100, 101, 89); id != "m10199" {
		t.Fatalf("id = %s", id)
	}
	ids := AssignMessageIDs(3, func(low, high int) int { return low })
	if ids[0] != "m11" || ids[1] != "m21" || ids[2] != "m31" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestMessageRecordRoundTrip mirrors to_db_instance / from_db_instance.
func TestMessageRecordRoundTrip(t *testing.T) {
	message := NewSessionMessage("m1", Now(), "qq")
	message.MessageInfo.UserInfo = NewUserInfo("10001", "alice", "AliceCard")
	message.MessageInfo.GroupInfo = &GroupInfo{GroupID: "g1", GroupName: ""}
	message.MessageInfo.AdditionalConfig = map[string]any{"self_id": "bot-1", "custom": 3}
	message.IsAt = true
	message.IsPicture = true
	message.SessionID = "sid"
	message.ReplyTo = StringPtr("m0")
	message.ReplyFreq = Float64Ptr(0.5)
	message.RawMessage = NewMessageSequence(NewTextComponent("hi"))
	message.ProcessedPlainText = StringPtr("hi")

	record, err := message.ToDatabaseRecord()
	if err != nil {
		t.Fatalf("ToDatabaseRecord: %v", err)
	}
	if record.GroupID == nil || *record.GroupID != "g1" || record.GroupName == nil || *record.GroupName != "" {
		t.Fatalf("group fields = %#v", record)
	}
	if record.AdditionalConfig == nil || !strings.Contains(*record.AdditionalConfig, "bot-1") {
		t.Fatalf("additional_config = %v", record.AdditionalConfig)
	}
	if !strings.Contains(record.RawContent, `"type":"text"`) {
		t.Fatalf("raw_content = %s", record.RawContent)
	}

	restored, err := FromDatabaseRecord(record)
	if err != nil {
		t.Fatalf("FromDatabaseRecord: %v", err)
	}
	if restored.ChatType() != "group" || restored.GroupID() != "g1" || restored.GroupName() != "" {
		t.Fatalf("restored group = %#v", restored.MessageInfo.GroupInfo)
	}
	if restored.MessageInfo.UserInfo.DisplayName() != "AliceCard" {
		t.Fatalf("display name = %q", restored.MessageInfo.UserInfo.DisplayName())
	}
	if restored.ChatType() != "group" {
		t.Fatalf("empty group name demoted the message to private")
	}
	if restored.ProcessedPlainText == nil || *restored.ProcessedPlainText != "hi" {
		t.Fatalf("plain text = %v", restored.ProcessedPlainText)
	}
}

// TestFromDatabaseRecordPrivateChat mirrors the private-chat branch (no group).
func TestFromDatabaseRecordPrivateChat(t *testing.T) {
	message := NewSessionMessage("m2", Now(), "qq")
	message.MessageInfo.UserInfo = NewUserInfo("10001", "alice", "")
	message.RawMessage = NewMessageSequence(NewTextComponent("hi"))
	record, err := message.ToDatabaseRecord()
	if err != nil {
		t.Fatalf("ToDatabaseRecord: %v", err)
	}
	if record.GroupID != nil {
		t.Fatalf("private chat carried a group id: %v", *record.GroupID)
	}
	restored, err := FromDatabaseRecord(record)
	if err != nil {
		t.Fatalf("FromDatabaseRecord: %v", err)
	}
	if restored.ChatType() != "private" {
		t.Fatalf("chat type = %s", restored.ChatType())
	}
	if record.AdditionalConfig != nil {
		t.Fatalf("empty additional_config should stay nil: %v", *record.AdditionalConfig)
	}
}

func TestProcessMirrorsPython(t *testing.T) {
	message := NewSessionMessage("m1", Now(), "qq")
	message.MessageInfo.UserInfo = NewUserInfo("1", "alice", "")
	message.RawMessage = NewMessageSequence(NewTextComponent("[voice]"), NewVoiceComponent([]byte{1}, ""), NewTextComponent("a"), NewTextComponent("b"))
	options := TextOptions{
		EnableHeavyMediaAnalysis: true,
		EnableVoiceTranscription: true,
		RenderVoice:              func(*VoiceComponent) string { return "[语音: transcript]" },
	}
	if got := message.Process(options); got != "[语音: transcript] a b" {
		t.Fatalf("processed text = %q", got)
	}
	if message.ProcessedPlainText == nil || *message.ProcessedPlainText != "[语音: transcript] a b" {
		t.Fatalf("plain text not stored: %v", message.ProcessedPlainText)
	}
	if strings.Contains(message.DebugString(), "Text(text='[voice]')") {
		t.Fatalf("voice placeholder text survived: %s", message.DebugString())
	}
	if !strings.Contains(message.DebugString(), "Voice(content=") {
		t.Fatalf("debug string = %s", message.DebugString())
	}
}

func TestToChatMessageCarriesImages(t *testing.T) {
	message := NewSessionMessage("m1", Now(), "qq")
	message.ProcessedPlainText = StringPtr("[图片]")
	message.RawMessage = NewMessageSequence(NewImageComponent([]byte{0xFF, 0xD8, 0xFF, 0x00}, ""))
	chat := message.ToChatMessage()
	if chat.Role != "user" || chat.Content != "[图片]" {
		t.Fatalf("chat message = %#v", chat)
	}
	if len(chat.Images) != 1 || !strings.HasPrefix(chat.Images[0].Base64, "data:image/jpeg;base64,") {
		t.Fatalf("images = %#v", chat.Images)
	}
}
