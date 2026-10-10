// 消息 ban 过滤 — 接线 bot_config 的 message_receive.ban_words / ban_msgs_regex
//（文档化配置，此前只读不生效）。语义对齐
// 参考/MaiBot/src/common/utils/utils_message.py 的 check_ban_words /
// check_ban_regex 与 bot.py receive_message：在消息登记（入库/会话登记）之前
// 对纯文本做过滤，命中即丢弃该消息；群聊与私聊都生效。
package assemble

import (
	"regexp"
	"strings"

	"oh-my-qqbot/internal/message"
)

// messagePlainText 返回用于 ban 检查的消息纯文本（对齐 Python 侧
// `processed_plain_text or ""`）。只检查纯文本：图片/表情等富媒体组件
// 不参与匹配，也不会因此被丢弃逻辑波及。
func messagePlainText(msg *message.SessionMessage) string {
	if msg == nil {
		return ""
	}
	if msg.ProcessedPlainText != nil {
		return *msg.ProcessedPlainText
	}
	if msg.RawMessage != nil {
		return msg.RawMessage.ToPlainText()
	}
	return ""
}

// compileBanRegex 编译并缓存一条过滤正则（按模式串去重；坏正则缓存为 nil
// 并 Warn 一次，避免每条消息重复编译报错；配置热重载更换模式串后自然命中
// 新键重新编译）。
func (a *App) compileBanRegex(pattern string) *regexp.Regexp {
	if cached, ok := a.banRegexCache.Load(pattern); ok {
		re, _ := cached.(*regexp.Regexp)
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		a.log.Warn("ban_msgs_regex 正则编译失败，已跳过该模式", "pattern", pattern, "err", err)
		re = nil
	}
	a.banRegexCache.Store(pattern, re)
	return re
}

// messageBanned 对消息纯文本做 ban 检查：ban_words 子串包含 / ban_msgs_regex
// 正则匹配（对齐 check_ban_words / check_ban_regex）。返回命中的词或模式；
// "" 表示放行。空文本不过滤（对齐 Python 的 `if not text` 分支）。
func (a *App) messageBanned(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	global := a.Cfg.GetGlobalConfig()
	for _, word := range global.MessageReceive.BanWords {
		// 跳过空词条（Python 侧空串包含匹配会误杀全部消息，属配置错误）。
		if word == "" {
			continue
		}
		if strings.Contains(text, word) {
			return word
		}
	}
	for _, pattern := range global.MessageReceive.BanMsgsRegex {
		if pattern == "" {
			continue
		}
		re := a.compileBanRegex(pattern)
		if re != nil && re.MatchString(text) {
			return pattern
		}
	}
	return ""
}
