package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"

	contracts "oh-my-qqbot/internal"
	"oh-my-qqbot/internal/message"
)

// This file mirrors 参考/MaiBot/src/maisaka/memory/person_profile.py
// (behaviour is authoritative there): 人物画像自动注入 + person facts 写回。
// 结构化画像文本协议对齐 src/A_memorix/core/utils/profile_text.py。

const (
	// ProfileQueryLimit mirrors PROFILE_QUERY_LIMIT.
	ProfileQueryLimit = 4
	// ProfileTextMaxChars mirrors PROFILE_TEXT_MAX_CHARS.
	ProfileTextMaxChars = 900
	// ProfileReferenceMarker mirrors `_format_profile_reference_block` 的首行。
	ProfileReferenceMarker = "【人物画像-内部参考】"
	// ProfileReferenceHeader mirrors 第二行。
	ProfileReferenceHeader = "以下内容仅供内部推理，不要向用户逐字复述。"
	// ProfileReferenceFooter mirrors 末尾提示。
	ProfileReferenceFooter = "使用时把它当作对当前人物的背景理解；若与当前对话冲突，以当前对话为准。"
	// ProfileRequiredPrefix mirrors PROFILE_REQUIRED_PREFIX.
	ProfileRequiredPrefix = "# 人物画像"
	// ProfileEmptyItem mirrors PROFILE_EMPTY_ITEM.
	ProfileEmptyItem = "- 暂无"
	// ProfileMaintenanceNote mirrors PROFILE_MAINTENANCE_NOTE.
	ProfileMaintenanceNote = "- 自动画像仅供内部参考；若与当前对话冲突，以当前对话为准。"
)

// ProfileSectionTitles mirrors PROFILE_SECTION_TITLES.
var ProfileSectionTitles = []string{
	"身份设定", "关系设定", "稳定了解", "相处偏好", "近期互动", "不确定信息", "维护备注",
}

// ProfileInjectionSectionTitles mirrors PROFILE_INJECTION_SECTION_TITLES.
var ProfileInjectionSectionTitles = []string{
	"身份设定", "关系设定", "稳定了解", "相处偏好", "近期互动",
}

// PersonProfileCandidate mirrors PersonProfileCandidate.
type PersonProfileCandidate struct {
	PersonID   string
	PersonName string
	UserID     string
	Source     string
}

// PersonProfileInjector mirrors the module-level functions of person_profile.py.
type PersonProfileInjector struct {
	// Enabled mirrors a_memorix.integration.enable_person_profile_injection（默认开）。
	Enabled bool
	// MaxProfiles mirrors person_profile_injection_max_profiles（默认 3）。
	MaxProfiles int
	// Kernel 读取画像（对齐 memory_service.profile_admin(action="query")）。
	Kernel ProfileReader
	// ResolvePersonID mirrors resolve_person_id_for_memory。
	ResolvePersonID ResolvePersonIDByNameFunc
	// IsBotSelf mirrors is_bot_self。
	IsBotSelf IsBotSelfFunc
	// Now / Logf 观测用。
	Logf func(format string, args ...any)
}

// NewPersonProfileInjector 构造注入器（默认开启，最多 3 个画像）。
func NewPersonProfileInjector() *PersonProfileInjector {
	return &PersonProfileInjector{Enabled: true, MaxProfiles: 3}
}

func (p *PersonProfileInjector) log(format string, args ...any) {
	if p.Logf != nil {
		p.Logf(format, args...)
	}
}

func (p *PersonProfileInjector) maxProfiles() int {
	if p.MaxProfiles > 0 {
		return p.MaxProfiles
	}
	return 3
}

// CollectCandidates mirrors collect_person_profile_candidates（当前对象优先）。
func (p *PersonProfileInjector) CollectCandidates(
	anchor *message.SessionMessage,
	pending []*message.SessionMessage,
) []PersonProfileCandidate {
	if anchor == nil {
		return nil
	}
	limit := p.maxProfiles()
	if anchor.MessageInfo.GroupInfo == nil {
		candidate := p.senderCandidate(anchor, "private_current_user")
		if candidate == nil {
			return nil
		}
		return []PersonProfileCandidate{*candidate}
	}

	candidates := make([]PersonProfileCandidate, 0, limit)
	seen := map[string]bool{}
	add := func(candidate *PersonProfileCandidate) bool {
		if candidate == nil || seen[candidate.PersonID] {
			return len(candidates) >= limit
		}
		seen[candidate.PersonID] = true
		candidates = append(candidates, *candidate)
		return len(candidates) >= limit
	}

	for _, msg := range MessagesCurrentFirst(anchor, pending) {
		if add(p.senderCandidate(msg, "recent_speaker")) {
			break
		}
		if msg.RawMessage == nil {
			continue
		}
		stop := false
		for _, component := range msg.RawMessage.Components {
			switch typed := component.(type) {
			case *message.AtComponent:
				if add(p.atCandidate(msg, typed)) {
					stop = true
				}
			case *message.ReplyComponent:
				if typed.TargetMessageSenderID == nil || strings.TrimSpace(*typed.TargetMessageSenderID) == "" {
					continue
				}
				if add(p.replyCandidate(msg, typed)) {
					stop = true
				}
			}
			if stop {
				break
			}
		}
		if len(candidates) >= limit {
			break
		}
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}

// MessagesCurrentFirst mirrors `_messages_current_first`.
func MessagesCurrentFirst(
	anchor *message.SessionMessage,
	pending []*message.SessionMessage,
) []*message.SessionMessage {
	out := []*message.SessionMessage{anchor}
	seen := map[string]bool{messageKey(anchor): true}
	for index := len(pending) - 1; index >= 0; index-- {
		msg := pending[index]
		if msg == nil {
			continue
		}
		key := messageKey(msg)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, msg)
	}
	return out
}

func messageKey(msg *message.SessionMessage) string {
	if msg == nil {
		return "object:nil"
	}
	if id := strings.TrimSpace(msg.MessageID); id != "" {
		return "message:" + id
	}
	return fmt.Sprintf("object:%p", msg)
}

func (p *PersonProfileInjector) resolveCandidate(
	platform string,
	userID string,
	personName string,
	source string,
) *PersonProfileCandidate {
	cleanPlatform := strings.TrimSpace(platform)
	cleanUserID := strings.TrimSpace(userID)
	cleanName := strings.TrimSpace(personName)
	if p.IsBotSelf != nil && p.IsBotSelf(cleanPlatform, cleanUserID) {
		return nil
	}
	personID := ""
	if p.ResolvePersonID != nil {
		switch {
		case cleanUserID != "":
			personID = strings.TrimSpace(p.ResolvePersonID(cleanPlatform, cleanUserID, ""))
		case cleanName != "":
			personID = strings.TrimSpace(p.ResolvePersonID("", "", cleanName))
		}
	}
	if personID == "" {
		return nil
	}
	return &PersonProfileCandidate{
		PersonID:   personID,
		PersonName: cleanName,
		UserID:     cleanUserID,
		Source:     source,
	}
}

func (p *PersonProfileInjector) senderCandidate(
	msg *message.SessionMessage,
	source string,
) *PersonProfileCandidate {
	if msg == nil {
		return nil
	}
	userInfo := msg.MessageInfo.UserInfo
	return p.resolveCandidate(msg.Platform, userInfo.UserID,
		candidateName(stringPtrValue(userInfo.UserCardname), userInfo.UserNickname, userInfo.UserID), source)
}

func (p *PersonProfileInjector) atCandidate(
	msg *message.SessionMessage,
	component *message.AtComponent,
) *PersonProfileCandidate {
	return p.resolveCandidate(msg.Platform, component.TargetUserID,
		candidateName(stringPtrValue(component.TargetUserCardname), stringPtrValue(component.TargetUserNickname),
			component.TargetUserID), "at_user")
}

func (p *PersonProfileInjector) replyCandidate(
	msg *message.SessionMessage,
	component *message.ReplyComponent,
) *PersonProfileCandidate {
	targetID := ""
	if component.TargetMessageSenderID != nil {
		targetID = strings.TrimSpace(*component.TargetMessageSenderID)
	}
	return p.resolveCandidate(msg.Platform, targetID,
		candidateName(stringPtrValue(component.TargetMessageSenderCardname),
			stringPtrValue(component.TargetMessageSenderNickname), targetID),
		"reply_sender")
}

// BuildInjectionMessages mirrors build_person_profile_injection_messages.
// 返回空切片表示本轮不注入。
func (p *PersonProfileInjector) BuildInjectionMessages(
	ctx context.Context,
	anchor *message.SessionMessage,
	pending []*message.SessionMessage,
) ([]string, error) {
	if !p.Enabled {
		return nil, nil
	}
	candidates := p.CollectCandidates(anchor, pending)
	if len(candidates) == 0 {
		return nil, nil
	}
	if p.Kernel == nil {
		return nil, nil
	}

	contextText := buildProfileContextText(anchor, pending)
	blocks := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		profile, err := p.Kernel.GetPersonProfile(ctx, candidate.PersonID)
		if err != nil {
			p.log("查询人物画像注入内容失败: person_id=%q err=%v", candidate.PersonID, err)
			continue
		}
		if profile == nil {
			continue
		}
		profileText := BuildProfileInjectionText(FormatStructuredProfileText(profile), nil, false)
		if strings.TrimSpace(profileText) == "" {
			p.log("人物画像注入跳过空画像: person_id=%q", candidate.PersonID)
			continue
		}
		_ = contextText
		displayName := profileDisplayName(candidate, profile)
		blocks = append(blocks, FormatProfilePersonBlock(displayName, profileText))
	}
	block := FormatProfileReferenceBlock(blocks)
	if block == "" {
		return nil, nil
	}
	return []string{block}, nil
}

func buildProfileContextText(
	anchor *message.SessionMessage,
	pending []*message.SessionMessage,
) string {
	parts := make([]string, 0, len(pending)+1)
	if anchor != nil {
		parts = append(parts, processedText(anchor))
	}
	for _, msg := range pending {
		parts = append(parts, processedText(msg))
	}
	joined := strings.Join(dedupeBlank(parts), "\n")
	if len([]rune(joined)) > 1000 {
		return string([]rune(joined)[:1000])
	}
	return joined
}

func profileDisplayName(candidate PersonProfileCandidate, profile *contracts.PersonProfile) string {
	if profile != nil && strings.TrimSpace(profile.Name) != "" {
		return strings.TrimSpace(profile.Name)
	}
	return candidateName(candidate.PersonName, candidate.UserID, candidate.PersonID)
}

// FormatProfileReferenceBlock mirrors `_format_profile_reference_block`.
func FormatProfileReferenceBlock(blocks []string) string {
	joined := strings.TrimSpace(strings.Join(dedupeBlank(blocks), "\n\n"))
	if joined == "" {
		return ""
	}
	return strings.Join([]string{
		ProfileReferenceMarker,
		ProfileReferenceHeader,
		"",
		joined,
		"",
		ProfileReferenceFooter,
	}, "\n")
}

// FormatProfilePersonBlock mirrors `_format_profile_person_block`.
func FormatProfilePersonBlock(displayName string, profileText string) string {
	return displayName + "：\n  " + TruncateProfileText(profileText)
}

// TruncateProfileText mirrors `_truncate_profile_text`.
func TruncateProfileText(profileText string) string {
	normalized := strings.TrimSpace(profileText)
	if len([]rune(normalized)) <= ProfileTextMaxChars {
		return normalized
	}
	return strings.TrimRight(string([]rune(normalized)[:ProfileTextMaxChars]), " ") + "..."
}

func candidateName(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// ---------------------------------------------------------------------------
// 结构化画像文本协议（对齐 A_memorix/core/utils/profile_text.py）
// ---------------------------------------------------------------------------

// ProfileSections 是从结构化画像文本解析出的段落集合。
type ProfileSections map[string][]string

// CleanProfileBullet mirrors clean_profile_bullet.
func CleanProfileBullet(value string) string {
	text := strings.TrimSpace(value)
	for strings.HasPrefix(text, "-") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "-"))
	}
	if text == "" {
		return ""
	}
	return "- " + text
}

// DedupeProfileBullets mirrors dedupe_profile_bullets.
func DedupeProfileBullets(values []string, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	bullets := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, value := range values {
		bullet := CleanProfileBullet(value)
		if bullet == "" || bullet == ProfileEmptyItem {
			continue
		}
		key := strings.ToLower(bullet)
		if seen[key] {
			continue
		}
		seen[key] = true
		bullets = append(bullets, bullet)
		if len(bullets) >= limit {
			break
		}
	}
	return bullets
}

// SectionIsEmpty mirrors section_is_empty.
func SectionIsEmpty(lines []string) bool {
	if len(lines) == 0 {
		return true
	}
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" && trimmed != ProfileEmptyItem {
			return false
		}
	}
	return true
}

// FormatStructuredProfileText 把内核画像对象渲染为固定格式的结构化画像文本
// （对齐 build_structured_profile_text，事实按类别映射到固定段落）。
func FormatStructuredProfileText(profile *contracts.PersonProfile) string {
	if profile == nil {
		return ""
	}
	identity := make([]string, 0, 2)
	relationships := make([]string, 0, 4)
	stable := make([]string, 0, 6)
	preferences := make([]string, 0, 5)
	recent := make([]string, 0, 3)
	uncertain := make([]string, 0, 3)
	for _, fact := range profile.Facts {
		content := strings.TrimSpace(fact.Content)
		if content == "" {
			continue
		}
		switch strings.TrimSpace(fact.Category) {
		case "identity":
			identity = append(identity, content)
		case "relationship":
			relationships = append(relationships, content)
		case "interaction_preference":
			preferences = append(preferences, content)
		case "recent_interaction":
			recent = append(recent, content)
		case "uncertain_notes":
			uncertain = append(uncertain, content)
		default:
			stable = append(stable, content)
		}
	}
	aliases := make([]string, 0, len(profile.Relations))
	for _, relation := range profile.Relations {
		if strings.TrimSpace(relation.TargetPersonID) != "" {
			aliases = append(aliases, relation.TargetPersonID)
		}
	}
	return BuildStructuredProfileText(StructuredProfileInput{
		PersonID:               profile.PersonID,
		PrimaryName:            profile.Name,
		Aliases:                aliases,
		IdentitySettings:       identity,
		RelationshipSettings:   relationships,
		StableFacts:            stable,
		InteractionPreferences: preferences,
		RecentInteractions:     recent,
		UncertainNotes:         uncertain,
	})
}

// StructuredProfileInput mirrors build_structured_profile_text 的关键字参数。
type StructuredProfileInput struct {
	PersonID               string
	PrimaryName            string
	Aliases                []string
	IdentitySettings       []string
	RelationshipSettings   []string
	StableFacts            []string
	InteractionPreferences []string
	RecentInteractions     []string
	UncertainNotes         []string
}

// BuildStructuredProfileText mirrors build_structured_profile_text.
func BuildStructuredProfileText(input StructuredProfileInput) string {
	aliasText := strings.Join(dedupeBlank(input.Aliases), "、")
	sections := map[string][]string{
		"身份设定":  DedupeProfileBullets(input.IdentitySettings, 4),
		"关系设定":  DedupeProfileBullets(input.RelationshipSettings, 4),
		"稳定了解":  DedupeProfileBullets(input.StableFacts, 6),
		"相处偏好":  DedupeProfileBullets(input.InteractionPreferences, 5),
		"近期互动":  DedupeProfileBullets(input.RecentInteractions, 3),
		"不确定信息": DedupeProfileBullets(input.UncertainNotes, 3),
		"维护备注":  {ProfileMaintenanceNote},
	}
	lines := []string{
		ProfileRequiredPrefix,
		"人物ID: " + strings.TrimSpace(input.PersonID),
		"主称呼: " + strings.TrimSpace(input.PrimaryName),
		"别名: " + aliasText,
	}
	for _, title := range ProfileSectionTitles {
		lines = append(lines, "", "## "+title)
		sectionLines := sections[title]
		if len(sectionLines) == 0 {
			lines = append(lines, ProfileEmptyItem)
			continue
		}
		lines = append(lines, sectionLines...)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ParseProfileSections mirrors parse_profile_sections.
func ParseProfileSections(profileText string) ProfileSections {
	sections := ProfileSections{}
	currentTitle := ""
	for _, rawLine := range strings.Split(profileText, "\n") {
		stripped := strings.TrimSpace(rawLine)
		if strings.HasPrefix(stripped, "## ") {
			title := strings.TrimSpace(strings.TrimPrefix(stripped, "## "))
			currentTitle = ""
			for _, known := range ProfileSectionTitles {
				if title == known {
					currentTitle = title
					break
				}
			}
			if currentTitle != "" {
				if _, ok := sections[currentTitle]; !ok {
					sections[currentTitle] = []string{}
				}
			}
			continue
		}
		if currentTitle != "" {
			sections[currentTitle] = append(sections[currentTitle], stripped)
		}
	}
	return sections
}

// IsStructuredProfileText mirrors is_structured_profile_text.
func IsStructuredProfileText(profileText string) bool {
	text := strings.TrimSpace(profileText)
	if !strings.HasPrefix(text, ProfileRequiredPrefix) {
		return false
	}
	sections := ParseProfileSections(text)
	for _, title := range ProfileSectionTitles {
		if _, ok := sections[title]; !ok {
			return false
		}
	}
	return true
}

// ReplaceProfileSection mirrors replace_profile_section.
func ReplaceProfileSection(profileText string, title string, newLines []string) (string, error) {
	cleanTitle := strings.TrimSpace(title)
	known := false
	for _, knownTitle := range ProfileSectionTitles {
		if cleanTitle == knownTitle {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("memory: 不支持的人物画像段落: %s", cleanTitle)
	}
	lines := strings.Split(profileText, "\n")
	out := make([]string, 0, len(lines)+len(newLines))
	replacement := DedupeProfileBullets(newLines, 20)
	if len(replacement) == 0 {
		replacement = []string{ProfileEmptyItem}
	}
	replaced := false
	for index := 0; index < len(lines); {
		line := lines[index]
		if strings.TrimSpace(line) == "## "+cleanTitle {
			out = append(out, line)
			out = append(out, replacement...)
			replaced = true
			index++
			for index < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[index]), "## ") {
				index++
			}
			continue
		}
		out = append(out, line)
		index++
	}
	if !replaced {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "## "+cleanTitle)
		out = append(out, replacement...)
	}
	return strings.TrimSpace(strings.Join(out, "\n")), nil
}

// BuildProfileInjectionText mirrors build_profile_injection_text.
func BuildProfileInjectionText(
	profileText string,
	uncertainCandidates []string,
	includeUncertainFallback bool,
) string {
	text := strings.TrimSpace(profileText)
	if !IsStructuredProfileText(text) {
		return text
	}
	sections := ParseProfileSections(text)
	selected := make([]string, 0, 16)
	meaningfulFound := false
	for _, title := range ProfileInjectionSectionTitles {
		lines := sections[title]
		if title == "近期互动" && len(lines) > 2 {
			lines = lines[:2]
		}
		if SectionIsEmpty(lines) {
			continue
		}
		meaningfulFound = true
		selected = append(selected, "## "+title)
		selected = append(selected, lines...)
		selected = append(selected, "")
	}

	selectedUncertain := DedupeProfileBullets(uncertainCandidates, 2)
	if !SectionIsEmpty(selectedUncertain) {
		selected = append(selected, "## 不确定信息（未确认，不可当作确定事实）")
		selected = append(selected, selectedUncertain...)
		selected = append(selected, "")
	} else if !meaningfulFound && includeUncertainFallback {
		uncertain := sections["不确定信息"]
		if len(uncertain) > 1 {
			uncertain = uncertain[:1]
		}
		if !SectionIsEmpty(uncertain) {
			selected = append(selected, "## 不确定信息（未确认）")
			selected = append(selected, uncertain...)
			selected = append(selected, "")
		}
	}
	return strings.TrimSpace(strings.Join(selected, "\n"))
}

// ---------------------------------------------------------------------------
// person facts 写回内核（对齐 _handle_message 的写回段）
// ---------------------------------------------------------------------------

// WritebackPersonFacts 把一批已验证人物事实写入内核 profile 存储。
//
// verified 为 true 时 confidence=1.0（server_verified），否则 0.5。
func WritebackPersonFacts(
	ctx context.Context,
	writer PersonFactWriter,
	personID string,
	personName string,
	facts []VerifiedPersonFact,
) (written int, err error) {
	if writer == nil {
		return 0, nil
	}
	for _, fact := range facts {
		content := strings.TrimSpace(fact.Fact)
		if content == "" {
			continue
		}
		profileFact := contracts.ProfileFact{
			Content:    content,
			Category:   "stable_facts",
			Confidence: 0.5,
			Source:     "summary_derived",
		}
		if fact.Verified {
			profileFact.Confidence = 1.0
			profileFact.Source = "user_supported:server_verified"
		}
		if err := writer.UpsertPersonFact(ctx, personID, personName, profileFact); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// VerifiedPersonFact 是一条带校验标记的人物事实。
type VerifiedPersonFact struct {
	Fact     string
	Verified bool
}

// SortProfileFactsByUpdatedAt 按更新时间倒序（画像注入顺序稳定）。
func SortProfileFactsByUpdatedAt(facts []contracts.ProfileFact) []contracts.ProfileFact {
	out := append([]contracts.ProfileFact{}, facts...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}
