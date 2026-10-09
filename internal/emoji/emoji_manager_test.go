package emoji

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTestPNG 生成指定尺寸的纯色 PNG 字节（拼图合成测试用）。
func buildTestPNG(t *testing.T, w, h int, clr color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; h > y; y++ {
		for x := 0; w > x; x++ {
			img.SetRGBA(x, y, clr)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodePNGSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码 PNG 失败: %v", err)
	}
	bounds := img.Bounds()
	return bounds.Dx(), bounds.Dy()
}

// 拼图合成：生成 PNG 断言尺寸 256×n（含 gap）。
func TestMergeEmojiTilesGridShape(t *testing.T) {
	// 5 张候选：_calculate_grid_shape(5) → columns=2, rows=3（aspect_gap=1、
	// empty_slots=1；先到先得，与 Python 评分序一致）。
	candidates := make([][]byte, 5)
	for i := range candidates {
		candidates[i] = buildTestPNG(t, 100, 100, color.RGBA{255, 0, 0, 255})
	}
	rows, columns := CalculateGridShape(5)
	if rows != 3 || columns != 2 {
		t.Fatalf("5 张候选应为 3x2 布局, got %dx%d", rows, columns)
	}

	pngBytes, err := MergeEmojiTiles(candidates, 1)
	if err != nil {
		t.Fatal(err)
	}
	width, height := decodePNGSize(t, pngBytes)
	wantWidth := CollageTileSize*columns + collageGap*(columns-1)
	wantHeight := CollageTileSize*rows + collageGap*(rows-1)
	if width != wantWidth || height != wantHeight {
		t.Fatalf("拼图尺寸应为 %dx%d, got %dx%d", wantWidth, wantHeight, width, height)
	}

	// 单张 → 1x1 → 256x256。
	single, err := MergeEmojiTiles(candidates[:1], 1)
	if err != nil {
		t.Fatal(err)
	}
	width, height = decodePNGSize(t, single)
	if width != CollageTileSize || height != CollageTileSize {
		t.Fatalf("单张拼图应为 256x256, got %dx%d", width, height)
	}
}

// 拼图起始序号影响角标（BuildCollage 用进程级递增序号）。
func TestMergeEmojiTilesStartIndex(t *testing.T) {
	candidates := [][]byte{buildTestPNG(t, 50, 50, color.RGBA{0, 255, 0, 255})}
	if _, err := MergeEmojiTiles(candidates, 7); err != nil {
		t.Fatal(err)
	}
	if rows, columns := CalculateGridShape(1); rows != 1 || columns != 1 {
		t.Fatalf("1 张候选应为 1x1, got %dx%d", rows, columns)
	}
	if rows, columns := CalculateGridShape(0); rows != 1 || columns != 1 {
		t.Fatalf("0 张候选应退化为 1x1, got %dx%d", rows, columns)
	}
}

func TestNormalizeEmojiTagText(t *testing.T) {
	tags := NormalizeEmojiTagText("开心, 开心、无语;尴尬 \n 得意，兴 奋")
	joined := strings.Join(tags, "|")
	if joined != "开心|无语|尴尬|得意|兴 奋" {
		t.Fatalf("标签规范化结果错误: %q", joined)
	}
	if got := NormalizeEmojiTagText(""); len(got) != 0 {
		t.Fatalf("空串应返回空列表, got %v", got)
	}
}

// 假 LLM 与假存储。
type fakeVLM struct {
	configured  bool
	description string
	response    string
	fail        bool
}

func (f *fakeVLM) GenerateResponseForImage(ctx context.Context, req VLMRequest) (string, error) {
	if f.fail {
		return "", context.DeadlineExceeded
	}
	if req.Prompt != "" && strings.Contains(req.Prompt, "审核") {
		return f.response, nil
	}
	return f.description, nil
}

func (f *fakeVLM) Configured() bool { return f.configured }

type fakeTextLLM struct {
	configured bool
	decision   string
}

func (f *fakeTextLLM) GenerateResponse(ctx context.Context, prompt string, sessionID string) (string, error) {
	return f.decision, nil
}

func (f *fakeTextLLM) EmojiTaskConfigured() bool { return f.configured }

type memStore struct {
	records map[string]*EmojiRecord
	deleted []string
}

func newMemStore() *memStore { return &memStore{records: map[string]*EmojiRecord{}} }

func (s *memStore) GetByHash(ctx context.Context, hash string) (*EmojiRecord, error) {
	record := s.records[hash]
	if record == nil {
		return nil, nil
	}
	cloned := *record
	return &cloned, nil
}

func (s *memStore) Upsert(ctx context.Context, record *EmojiRecord) error {
	cloned := *record
	s.records[record.ImageHash] = &cloned
	return nil
}

func (s *memStore) Delete(ctx context.Context, hash string) error {
	delete(s.records, hash)
	s.deleted = append(s.deleted, hash)
	return nil
}

func (s *memStore) ListAll(ctx context.Context) ([]*EmojiRecord, error) {
	out := make([]*EmojiRecord, 0, len(s.records))
	for _, record := range s.records {
		cloned := *record
		out = append(out, &cloned)
	}
	return out, nil
}

func (s *memStore) CountRegistered(ctx context.Context) (int, error) {
	n := 0
	for _, record := range s.records {
		if record.IsRegistered {
			n++
		}
	}
	return n, nil
}

type passthroughResolver struct{ root string }

func (r passthroughResolver) Resolve(storedPath string) (string, error) {
	if strings.HasPrefix(storedPath, r.root) {
		return storedPath, nil
	}
	return filepath.Join(r.root, storedPath), nil
}

func (r passthroughResolver) Serialize(absolutePath string) (string, error) {
	return absolutePath, nil
}

func newTestManager(t *testing.T, vlm VLMClient, textLLM TextLLM) (*Manager, *memStore, string) {
	t.Helper()
	root := t.TempDir()
	emojiDir := filepath.Join(root, "data", "emoji")
	if err := os.MkdirAll(emojiDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	manager := NewManager(Options{
		Store:          store,
		Paths:          passthroughResolver{root: root},
		VLM:            vlm,
		TextLLM:        textLLM,
		EmojiDir:       emojiDir,
		ProjectRoot:    root,
		MaxRegNum:      4,
		MaxEmojiSizeMB: 5,
	})
	return manager, store, root
}

func saveEmojiPNG(t *testing.T, manager *Manager, clr color.RGBA) *Emoji {
	t.Helper()
	raw := buildTestPNG(t, 64, 64, clr)
	emoji, err := manager.EnsureEmojiSaved(context.Background(), raw, "")
	if err != nil {
		t.Fatal(err)
	}
	return emoji
}

// 注册：EnsureEmojiSaved 用 sha256 命名文件，RegisterEmojiToDB 登记。
func TestEnsureSavedAndRegister(t *testing.T) {
	manager, store, root := newTestManager(t, &fakeVLM{configured: true, description: "开心,高兴"}, nil)
	emoji := saveEmojiPNG(t, manager, color.RGBA{10, 20, 30, 255})

	if emoji.FileHash == "" || len(emoji.FileHash) != 64 {
		t.Fatalf("应计算 sha256: %q", emoji.FileHash)
	}
	if !strings.HasPrefix(filepath.Base(emoji.FullPath), emoji.FileHash) {
		t.Fatalf("文件应以 hash 命名: %s", emoji.FullPath)
	}
	if _, err := os.Stat(emoji.FullPath); err != nil {
		t.Fatalf("文件应存在: %v", err)
	}
	if strings.HasSuffix(emoji.ImageFormat, "tmp") || emoji.ImageFormat != "png" {
		t.Fatalf("扩展名应修正为 png: %q", emoji.ImageFormat)
	}

	status := manager.RegisterEmojiToDB(context.Background(), emoji)
	if status != RegisterStatusRegistered {
		t.Fatalf("注册应成功: %s", status)
	}
	record, _ := store.GetByHash(context.Background(), emoji.FileHash)
	if record == nil || !record.IsRegistered {
		t.Fatalf("数据库应有已注册记录: %+v", record)
	}
	// 重复注册 → skipped
	if again := manager.RegisterEmojiToDB(context.Background(), emoji); again != RegisterStatusSkipped {
		t.Fatalf("重复注册应跳过: %s", again)
	}
	_ = root
}

// VLM 描述 + 情绪标签：注册流程产出描述并写回。
func TestBuildDescriptionAndRegisterByFilename(t *testing.T) {
	vlm := &fakeVLM{configured: true, description: "开心, 开心、大笑; 猥琐"}
	manager, store, root := newTestManager(t, vlm, nil)

	// 直接往 data/emoji 放一个文件，走 register_emoji_by_filename 路径。
	raw := buildTestPNG(t, 80, 80, color.RGBA{1, 2, 3, 255})
	filename := filepath.Join(manager.emojiDir, "test_emoji.png")
	if err := os.WriteFile(filename, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	status := manager.RegisterEmojiByFilename(context.Background(), filename, false, false)
	if status != RegisterStatusRegistered {
		t.Fatalf("注册应成功: %s", status)
	}
	emoji := manager.Emojis()
	if len(emoji) != 1 {
		t.Fatalf("内存池应有 1 个表情: %d", len(emoji))
	}
	if emoji[0].Description != "开心,大笑,猥琐" {
		t.Fatalf("描述应为规范化去重标签逗号串: %q", emoji[0].Description)
	}
	record, _ := store.GetByHash(context.Background(), emoji[0].FileHash)
	if record == nil || record.Description == "" || record.IsRegistered != true {
		t.Fatalf("记录应有 VLM 描述且已注册: %+v", record)
	}
	_ = root
}

// 达 max_reg_num 后按 do_replace 用 LLM 决策替换（假 LLM 返回「取消注册编号1」）。
func TestReplaceDecisionWithFakeLLM(t *testing.T) {
	vlm := &fakeVLM{configured: true, description: "开心"}
	textLLM := &fakeTextLLM{decision: "取消注册编号1"}
	manager, _, _ := newTestManager(t, vlm, textLLM)

	// 填满 max_reg_num=4。
	for i := 0; i < 4; i++ {
		raw := buildTestPNG(t, 32, 32, color.RGBA{uint8(i), 0, 0, 255})
		emoji, err := manager.EnsureEmojiSaved(context.Background(), raw, "")
		if err != nil {
			t.Fatal(err)
		}
		if status := manager.RegisterEmojiToDB(context.Background(), emoji); status != RegisterStatusRegistered {
			t.Fatalf("填充注册失败: %s", status)
		}
		manager.emojis = append(manager.emojis, emoji)
		manager.emojiNum = len(manager.emojis)
	}

	// 新表情走替换决策。
	newEmoji := saveEmojiPNG(t, manager, color.RGBA{9, 9, 9, 255})
	if !manager.ReplaceAnEmojiByLLM(context.Background(), newEmoji, "") {
		t.Fatal("替换决策应成功")
	}
	if manager.EmojiNum() != 4 {
		t.Fatalf("替换后数量应保持 max_reg_num: %d", manager.EmojiNum())
	}
	hashes := map[string]bool{}
	for _, emoji := range manager.Emojis() {
		hashes[emoji.FileHash] = true
	}
	if !hashes[newEmoji.FileHash] {
		t.Fatal("新表情应在池中")
	}
}

// 假 LLM 返回「不取消注册」→ 不替换。
func TestReplaceDecisionSkip(t *testing.T) {
	vlm := &fakeVLM{configured: true, description: "开心"}
	textLLM := &fakeTextLLM{decision: "不取消注册"}
	manager, _, _ := newTestManager(t, vlm, textLLM)

	raw := buildTestPNG(t, 32, 32, color.RGBA{1, 1, 1, 255})
	emoji, _ := manager.EnsureEmojiSaved(context.Background(), raw, "")
	manager.emojis = append(manager.emojis, emoji)
	manager.emojiNum = 1

	if manager.ReplaceAnEmojiByLLM(context.Background(), saveEmojiPNG(t, manager, color.RGBA{2, 2, 2, 255}), "") {
		t.Fatal("不取消注册时替换应失败")
	}
}

// 情绪标签选择（旧逻辑）：相似度 Top10 随机。
func TestGetEmojiForEmotion(t *testing.T) {
	manager, _, _ := newTestManager(t, nil, nil)
	now := time.Now()
	happy := &Emoji{FileHash: "h1", FullPath: "h1.png", Description: "开心,高兴", RegisterTime: &now}
	noTag := &Emoji{FileHash: "h3", FullPath: "h3.png", Description: "", RegisterTime: &now}
	manager.emojis = []*Emoji{happy, noTag}
	manager.emojiNum = 2

	// 只有 h1 有情绪标签，确定性命中。
	if selected := manager.GetEmojiForEmotion("开心"); selected == nil || selected.FileHash != "h1" {
		t.Fatalf("开心应命中 h1: %+v", selected)
	}
	if selected := manager.GetEmojiForEmotion(""); selected != nil {
		t.Fatalf("空情绪不应选择: %+v", selected)
	}

	// 多候选时在相似度 Top 内随机（与 Python heapq.nlargest(10) + random.choice 语义一致）。
	angry := &Emoji{FileHash: "h2", FullPath: "h2.png", Description: "愤怒,生气", RegisterTime: &now}
	manager.emojis = []*Emoji{happy, angry}
	manager.emojiNum = 2
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		selected := manager.GetEmojiForEmotion("开心")
		if selected == nil {
			t.Fatal("应命中表情")
		}
		seen[selected.FileHash] = true
	}
	if !seen["h1"] || !seen["h2"] {
		t.Fatalf("Top10 随机应可能命中两个候选: %v", seen)
	}
}

// 出站适配：SendEmojiByEmotion 实现工具桩接口 + 发送成功记录使用次数。
type fakeOutbound struct {
	sent    []string
	succeed bool
	lastB64 string
}

func (f *fakeOutbound) SendEmoji(ctx context.Context, sessionID string, emojiBase64 string) (string, error) {
	if !f.succeed {
		return "", context.Canceled
	}
	f.sent = append(f.sent, sessionID)
	f.lastB64 = emojiBase64
	return "ROBOT_sent_1", nil
}

func TestSendEmojiByEmotionPath(t *testing.T) {
	manager, _, _ := newTestManager(t, nil, nil)
	now := time.Now()
	happy := saveEmojiPNG(t, manager, color.RGBA{5, 5, 5, 255})
	manager.emojis = []*Emoji{{FileHash: happy.FileHash, FullPath: happy.FullPath, Description: "开心", Emotions: []string{"开心"}, RegisterTime: &now}}
	manager.emojiNum = 1

	outbound := &fakeOutbound{succeed: true}
	manager.SetOutbound(outbound)
	manager.SetCandidateCount(25)

	description, matched, err := manager.SendEmojiByEmotion(context.Background(), "s1", "开心", "因为开心")
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if description != "开心" {
		t.Fatalf("描述不符: %q", description)
	}
	_ = matched
	if len(outbound.sent) != 1 || outbound.sent[0] != "s1" {
		t.Fatalf("应发送到会话 s1: %v", outbound.sent)
	}
	if outbound.lastB64 == "" {
		t.Fatal("应携带 base64 图片")
	}
	if manager.Emojis()[0].QueryCount != 1 {
		t.Fatalf("发送后应记录使用次数: %d", manager.Emojis()[0].QueryCount)
	}
	// 编译期对齐 tools.EmojiSender 方法集（同名同签名）。
	var _ interface {
		SendEmojiByEmotion(ctx context.Context, sessionID string, requestedEmotion string, reasoning string) (string, string, error)
	} = manager
}

// 子代理选择：拼图 + JSON {emoji_index, reason}。
func TestSelectEmojiWithSubAgentJSON(t *testing.T) {
	manager, _, _ := newTestManager(t, nil, nil)
	var pool []*Emoji
	for i := 0; i < 3; i++ {
		emoji := saveEmojiPNG(t, manager, color.RGBA{uint8(i + 1), 0, 0, 255})
		pool = append(pool, emoji)
	}
	manager.emojis = pool
	manager.emojiNum = len(pool)

	// 用带选择能力的假 VLM：返回固定 JSON。
	vlm := &fakeSelectionVLM{response: `{"emoji_index":2,"reason":"更适合当前语气"}`}
	manager.vlm = vlm

	emoji, reason, err := manager.SelectEmojiWithSubAgent(context.Background(), "s1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "更适合当前语气" {
		t.Fatalf("理由不符: %q", reason)
	}
	if emoji.FileHash != pool[1].FileHash {
		t.Fatalf("应选中序号 2: %s", emoji.FileHash)
	}
	// 越界序号回退第 1 张。
	vlm.response = `{"emoji_index":9,"reason":"x"}`
	emoji, _, err = manager.SelectEmojiWithSubAgent(context.Background(), "s1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if emoji.FileHash != pool[0].FileHash {
		t.Fatalf("越界应回退第 1 张: %s", emoji.FileHash)
	}
	// 解析失败回退首项。
	vlm.response = `这不是 JSON`
	emoji, _, err = manager.SelectEmojiWithSubAgent(context.Background(), "s1", 25)
	if err != nil {
		t.Fatal(err)
	}
	if emoji.FileHash != pool[0].FileHash {
		t.Fatalf("解析失败应回退首项: %s", emoji.FileHash)
	}
}

type fakeSelectionVLM struct{ response string }

func (f *fakeSelectionVLM) GenerateResponseForImage(ctx context.Context, req VLMRequest) (string, error) {
	return f.response, nil
}

func (f *fakeSelectionVLM) Configured() bool { return true }

// BuildCollage 桩：返回拼图 + 序号映射。
func TestBuildCollageForLister(t *testing.T) {
	manager, _, _ := newTestManager(t, nil, nil)
	var pool []*Emoji
	for i := 0; i < 6; i++ {
		pool = append(pool, saveEmojiPNG(t, manager, color.RGBA{uint8(i), 128, 128, 255}))
	}
	manager.emojis = pool
	manager.emojiNum = len(pool)
	manager.SetCandidateCount(4)

	pngBytes, indexToHash, startIndex, err := manager.BuildCollage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	width, height := decodePNGSize(t, pngBytes)
	if width != CollageTileSize*2+collageGap || height != CollageTileSize*2+collageGap {
		t.Fatalf("4 张候选应为 2x2 拼图, got %dx%d", width, height)
	}
	if len(indexToHash) != 4 || startIndex != 1 {
		t.Fatalf("序号映射应为 4 项、起始 1: %d %d", len(indexToHash), startIndex)
	}
	for index := range indexToHash {
		if index < 1 || index > 4 {
			t.Fatalf("序号越界: %d", index)
		}
	}
}

// 内容审核：VLM 回复「否」时拒绝注册。
func TestContentFiltrationReject(t *testing.T) {
	vlm := &fakeVLM{configured: true, description: "开心", response: "否"}
	manager, _, _ := newTestManager(t, vlm, nil)
	emoji := saveEmojiPNG(t, manager, color.RGBA{7, 7, 7, 255})
	if manager.ReviewEmojiForRegistration(context.Background(), emoji, "") {
		t.Fatal("审核回复「否」应拒绝")
	}
	vlm.response = "是"
	if !manager.ReviewEmojiForRegistration(context.Background(), emoji, "") {
		t.Fatal("审核回复「是」应通过")
	}
}

func TestParseSelectionJSONToleratesFences(t *testing.T) {
	selection, err := parseSelectionJSON("```json\n{\"emoji_index\": 3, \"reason\": \"test\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if selection.EmojiIndex != 3 || selection.Reason != "test" {
		t.Fatalf("解析结果错误: %+v", selection)
	}
	if _, err := parseSelectionJSON("no json here"); err == nil {
		t.Fatal("无 JSON 应报错")
	}
}

func TestCalculateEmotionSimilarity(t *testing.T) {
	if sim := CalculateEmotionSimilarity("开心", "开心"); sim != 1 {
		t.Fatalf("相同标签相似度应为 1, got %v", sim)
	}
	// Levenshtein("开心","开心呀")=1, max=3 → 2/3（对齐 rapidfuzz 归一化）。
	if sim := CalculateEmotionSimilarity("开心", "开心呀"); sim < 0.6 || sim >= 1 {
		t.Fatalf("相似度应在 [0.6,1): %v", sim)
	}
	if sim := CalculateEmotionSimilarity("开心", "高兴"); sim != 0 {
		t.Fatalf("完全不同汉字应为 0, got %v", sim)
	}
	if sim := CalculateEmotionSimilarity("", "开心"); sim != 0 {
		t.Fatalf("空串相似度应为 0, got %v", sim)
	}
}
