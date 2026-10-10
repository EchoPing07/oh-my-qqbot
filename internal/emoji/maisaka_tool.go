package emoji

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
)

// This file mirrors the outbound paths of
// 参考/MaiBot/src/emoji_system/maisaka_tool.py (send_emoji_for_maisaka) plus
// builtin_tool/send_emoji.py (_select_emoji_with_sub_agent) and
// builtin_tool/show_emoji_list.py (handle_tool) — 与 internal/chat/tools 留下的
// EmojiSender / EmojiLister 桩对接（适配器写在本包内，不改 tools 包）。

// EmojiSelectorFunc 是出站表情选择的函数形态（对齐 maisaka_tool.EmojiSelector）。
type EmojiSelectorFunc func(ctx context.Context, requestedEmotion, reasoning string, contextTexts []string, sampleSize int) (*Emoji, string, error)

// OutboundSender 是表情出站的最小发送边界（由装配层适配
// internal/chat/sender 或 qq.Channel）。
type OutboundSender interface {
	// SendEmoji 发送一个表情（emojiBase64 为 base64 数据）。
	SendEmoji(ctx context.Context, sessionID string, emojiBase64 string) (sentMessageID string, err error)
}

// SendResult mirrors MaisakaEmojiSendResult（裁剪到 Go 管线实际消费的面）。
type SendResult struct {
	Success          bool
	Message          string
	EmojiBase64      string
	Description      string
	Emotions         []string
	RequestedEmotion string
	MatchedEmotion   string
	SentMessageID    string
}

// emojiSendNum / candidate 上限照抄 send_emoji.py。
const (
	// EmojiSendNumDefault mirrors _get_emoji_candidate_count 的配置默认 25。
	EmojiSendNumDefault = 25
	// EmojiMaxCandidateCount mirrors _EMOJI_MAX_CANDIDATE_COUNT = 64。
	EmojiMaxCandidateCount = 64
	// emojiSubAgentContextLimit mirrors _EMOJI_SUB_AGENT_CONTEXT_LIMIT = 12。
	emojiSubAgentContextLimit = 12
)

// EmojiSuccessMessage / EmojiVLMNotConfiguredMessage mirrors send_emoji.py 常量。
const (
	EmojiSuccessMessage          = "表情包发送成功"
	EmojiVLMNotConfiguredMessage = "错误，没有配置视觉模型，无法使用表情包功能"
)

// CandidateCount 对齐 _get_emoji_candidate_count:
// max(1, min(emoji_send_num, 64))。
func CandidateCount(configured int) int {
	if configured <= 0 {
		configured = EmojiSendNumDefault
	}
	if configured > EmojiMaxCandidateCount {
		configured = EmojiMaxCandidateCount
	}
	if configured < 1 {
		configured = 1
	}
	return configured
}

// GetEmojiCandidateCount returns the effective candidate count for the pool.
func (m *Manager) GetEmojiCandidateCount(configured int) int { return CandidateCount(configured) }

// ---------------------------------------------------------------------------
// 旧逻辑：按情绪标签相似度 Top10 随机选 → 发送
// ---------------------------------------------------------------------------

// SendEmojiByEmotion implements the tools.EmojiSender contract:
// SendEmojiByEmotion(ctx, sessionID, requestedEmotion, reasoning) → (description, matchedEmotion, error)。
// 旧逻辑对齐 get_emoji_for_emotion 的 Top10 随机选择；发送走注入的 OutboundSender；
// 发送成功后 update_emoji_usage_by_hash 记录使用。
func (m *Manager) SendEmojiByEmotion(ctx context.Context, sessionID string, requestedEmotion string, reasoning string) (string, string, error) {
	result := m.sendEmojiForMaisaka(ctx, sessionID, requestedEmotion, reasoning, nil, func(selCtx context.Context, emotion, _reason string, texts []string, sampleSize int) (*Emoji, string, error) {
		emoji := m.GetEmojiForEmotion(emotion)
		if emoji == nil {
			return nil, "", fmt.Errorf("当前表情包库中没有可用表情")
		}
		return emoji, "", nil
	})
	if !result.Success {
		if result.Message == "当前表情包库中没有可用表情。" {
			return result.Description, result.MatchedEmotion, fmt.Errorf("当前表情包库中没有可用表情")
		}
		return result.Description, result.MatchedEmotion, fmt.Errorf("%s", result.Message)
	}
	return result.Description, result.MatchedEmotion, nil
}

// sendEmojiForMaisaka mirrors send_emoji_for_maisaka（无 Hook 面；选择函数注入）。
func (m *Manager) sendEmojiForMaisaka(
	ctx context.Context,
	streamID string,
	requestedEmotion string,
	reasoning string,
	contextTexts []string,
	selector EmojiSelectorFunc,
) SendResult {
	normalizedEmotion := trimSpace(requestedEmotion)
	result := SendResult{RequestedEmotion: normalizedEmotion, MatchedEmotion: ""}

	if selector == nil {
		result.Message = "当前表情包库中没有可用表情。"
		return result
	}
	selected, matchedEmotion, selectErr := selector(ctx, normalizedEmotion, reasoning, contextTexts, 20)
	result.MatchedEmotion = matchedEmotion
	if selectErr != nil {
		result.Message = fmt.Sprintf("发送表情包时发生异常：%v", selectErr)
		return result
	}
	if selected == nil {
		result.Message = "当前表情包库中没有可用表情。"
		return result
	}

	result.Description = trimSpace(selected.Description)
	result.Emotions = NormalizeEmojiTagText(selected.Description)

	emojiBytes, err := os.ReadFile(selected.FullPath)
	if err != nil {
		result.Message = fmt.Sprintf("发送表情包失败：%v", err)
		return result
	}
	emojiBase64 := base64.StdEncoding.EncodeToString(emojiBytes)
	result.EmojiBase64 = emojiBase64

	if m.outbound == nil {
		result.Message = "发送表情包失败：未配置发送通道"
		return result
	}
	sentMessageID, sendErr := m.outbound.SendEmoji(ctx, streamID, emojiBase64)
	if sendErr != nil || sentMessageID == "" {
		if sendErr == nil {
			sendErr = fmt.Errorf("发送表情包失败")
		}
		result.Message = fmt.Sprintf("发送表情包时发生异常：%v", sendErr)
		return result
	}
	result.SentMessageID = sentMessageID

	// 发送成功后记录使用次数（照抄 record_usage 分支）。
	if !m.UpdateEmojiUsageByHash(ctx, selected.FileHash) {
		// 记录失败不阻断成功回执（Python 版 update_emoji_usage 失败只打日志）。
	}
	result.Success = true
	result.Message = EmojiSuccessMessage
	return result
}

// outbound 是出站发送通道（SetOutbound 注入）。
// outbound 字段承载在 Manager 上（见下方扩展）。

// SetOutbound 注入出站发送通道。
func (m *Manager) SetOutbound(sender OutboundSender) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outbound = sender
}

// ---------------------------------------------------------------------------
// 新逻辑：拼图候选 + 子代理 JSON 选择（emoji_selection.prompt）
// ---------------------------------------------------------------------------

// BuildCollage implements the tools.EmojiLister contract:
// BuildCollage(ctx) → (pngBytes, indexToHash, startIndex, error)。
// 对齐 show_emoji_list.handle_tool：随机抽取候选、一次性分配连续编号、
// 合成 256px 格 + 64px 序号角标的拼图 PNG。
func (m *Manager) BuildCollage(ctx context.Context) ([]byte, map[int]string, int, error) {
	available := m.Emojis()
	if len(available) == 0 {
		return nil, nil, 0, fmt.Errorf("当前表情包库中没有可用表情")
	}
	return m.buildCollageFromPool(available, m.candidateCount, 1)
}

// buildCollageFromPool 从可用池随机抽取候选并合成拼图。
func (m *Manager) buildCollageFromPool(available []*Emoji, candidateCount int, startIndex int) ([]byte, map[int]string, int, error) {
	if len(available) == 0 {
		return nil, nil, 0, fmt.Errorf("当前表情包库中没有可用表情")
	}
	total := candidateCount
	if total > len(available) {
		total = len(available)
	}
	sampled := m.sampleEmojis(available, total)

	images := make([][]byte, 0, len(sampled))
	indexToHash := make(map[int]string, len(sampled))
	for i, emoji := range sampled {
		raw, err := os.ReadFile(emoji.FullPath)
		if err != nil {
			// 读取失败用空字节：tile 合成会画占位图（对齐 _build_placeholder_tile）。
			raw = nil
		}
		images = append(images, raw)
		indexToHash[startIndex+i] = emoji.FileHash
	}
	pngBytes, err := MergeEmojiTiles(images, startIndex)
	if err != nil {
		return nil, nil, 0, err
	}
	return pngBytes, indexToHash, startIndex, nil
}

// sampleEmojis mirrors random.sample（无放回采样）。S7：经 rngPerm 线程安全取随机排列。
func (m *Manager) sampleEmojis(pool []*Emoji, n int) []*Emoji {
	if n >= len(pool) {
		return append([]*Emoji{}, pool...)
	}
	indexes := m.rngPerm(len(pool))[:n]
	sampled := make([]*Emoji, 0, n)
	for _, idx := range indexes {
		sampled = append(sampled, pool[idx])
	}
	return sampled
}

// SubAgentLLM 是 send_emoji 子代理的视觉模型边界（需要看拼图）。
// 复用 VLMClient。

// EmojiSelectionResult mirrors EmojiSelectionResult (pydantic model):
// {emoji_index, reason}。
type EmojiSelectionResult struct {
	EmojiIndex int    `json:"emoji_index"`
	Reason     string `json:"reason"`
}

// SelectEmojiWithSubAgent mirrors _select_emoji_with_sub_agent：
// 候选拼图 + emoji_selection.prompt（JSON {emoji_index, reason}）→ 子代理挑选。
// 返回选中的表情与理由；解析失败回退到候选首项（对齐 Python 回退分支）。
func (m *Manager) SelectEmojiWithSubAgent(ctx context.Context, sessionID string, configuredCount int) (*Emoji, string, error) {
	available := m.Emojis()
	if len(available) == 0 {
		return nil, "", fmt.Errorf("当前表情包库中没有可用表情")
	}
	if m.vlm == nil || !m.vlm.Configured() {
		return nil, "", fmt.Errorf(EmojiVLMNotConfiguredMessage)
	}
	total := CandidateCount(configuredCount)
	if total > len(available) {
		total = len(available)
	}
	sampled := m.sampleEmojis(available, total)
	startIndex := 1

	images := make([][]byte, 0, len(sampled))
	for _, emoji := range sampled {
		raw, err := os.ReadFile(emoji.FullPath)
		if err != nil {
			raw = nil
		}
		images = append(images, raw)
	}
	collage, err := MergeEmojiTiles(images, startIndex)
	if err != nil {
		return nil, "", err
	}
	gridRows, gridColumns := CalculateGridShape(len(sampled))

	systemPrompt := m.renderPromptOrDefault(ctx, "emoji_selection", map[string]string{
		"emoji_count":  fmt.Sprintf("%d", len(sampled)),
		"grid_rows":    fmt.Sprintf("%d", gridRows),
		"grid_columns": fmt.Sprintf("%d", gridColumns),
	}, "emoji_selection")

	content, err := m.vlm.GenerateResponseForImage(ctx, VLMRequest{
		Prompt: systemPrompt + "\n[选择任务]\n候选总数: " + fmt.Sprintf("%d", len(sampled)) +
			"\n拼图布局: " + fmt.Sprintf("%dx%d", gridRows, gridColumns) + "\n请只输出 JSON。",
		ImageBytes:  collage,
		ImageFormat: "png",
		SessionID:   sessionID,
	})
	if err != nil {
		return nil, "", err
	}

	selection, parseErr := parseSelectionJSON(content)
	if parseErr != nil {
		// 回退到候选首项（对齐 Python 解析失败分支）。
		return sampled[0], "", nil
	}
	emojiIndex := selection.EmojiIndex
	if emojiIndex < 1 || emojiIndex > len(sampled) {
		emojiIndex = 1
	}
	return sampled[emojiIndex-1], selection.Reason, nil
}

// SendEmojiWithSubAgent 是新逻辑关闭前的 send_emoji 子代理路径：
// 子代理从拼图中选一张并发送，返回 (description, reason, error)。
func (m *Manager) SendEmojiWithSubAgent(ctx context.Context, sessionID string, configuredCount int) (*SendResult, error) {
	result := &SendResult{RequestedEmotion: "", MatchedEmotion: ""}
	selected, subAgentReason, err := m.SelectEmojiWithSubAgent(ctx, sessionID, configuredCount)
	if err != nil {
		return nil, err
	}
	result.MatchedEmotion = subAgentReason
	result.Description = trimSpace(selected.Description)
	result.Emotions = NormalizeEmojiTagText(selected.Description)

	emojiBytes, err := os.ReadFile(selected.FullPath)
	if err != nil {
		result.Message = fmt.Sprintf("发送表情包失败：%v", err)
		return result, nil
	}
	result.EmojiBase64 = base64.StdEncoding.EncodeToString(emojiBytes)
	if m.outbound == nil {
		result.Message = "发送表情包失败：未配置发送通道"
		return result, nil
	}
	sentMessageID, sendErr := m.outbound.SendEmoji(ctx, sessionID, result.EmojiBase64)
	if sendErr != nil || sentMessageID == "" {
		if sendErr == nil {
			sendErr = fmt.Errorf("发送表情包失败")
		}
		result.Message = fmt.Sprintf("发送表情包时发生异常：%v", sendErr)
		return result, nil
	}
	result.SentMessageID = sentMessageID
	_ = m.UpdateEmojiUsageByHash(ctx, selected.FileHash)
	result.Success = true
	result.Message = EmojiSuccessMessage
	return result, nil
}

// candidateCount 默认候选数（SetCandidateCount 注入 emoji_send_num）。
// candidateCount 字段承载在 Manager 上。

// SetCandidateCount 注入 emoji.emoji_send_num（新逻辑候选数量）。
func (m *Manager) SetCandidateCount(count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.candidateCount = CandidateCount(count)
}
