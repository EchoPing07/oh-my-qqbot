// Package person 实现人物系统（对照 参考MaiBot/src/person_info/person_info.py，
// 行为以该文件为权威）：PersonInfo — person_id(md5 计算)、群名片、know_times、
// 数据库读写。依赖接口注入（不 import internal/qq、internal/config）。
package person

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// GetPersonID mirrors get_person_id: 平台名含 "-" 时取后半段（如
// "qq_official-qq" → "qq"），person_id = md5(platform + "_" + user_id)。
func GetPersonID(platform string, userID string) string {
	if strings.Contains(platform, "-") {
		platform = strings.Split(platform, "-")[1]
	}
	key := platform + "_" + userID
	sum := md5.Sum([]byte(key))
	return hex.EncodeToString(sum[:])
}

// GroupCardnameInfo 对齐 GroupCardnameInfo dataclass。
type GroupCardnameInfo struct {
	GroupID       string `json:"group_id"`
	GroupCardname string `json:"group_cardname"`
}

// dumpGroupCardnameRecords 对齐 dump_group_cardname_records：序列化为 JSON
// 文本（空列表返回空串）。
func dumpGroupCardnameRecords(records []GroupCardnameInfo) string {
	if len(records) == 0 {
		return ""
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// parseGroupCardnameJSON 对齐 parse_group_cardname_json：解析库内 JSON；
// 非法输入返回空列表。
func parseGroupCardnameJSON(raw string) []GroupCardnameInfo {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var records []GroupCardnameInfo
	if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
		return nil
	}
	valid := make([]GroupCardnameInfo, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.GroupID) == "" || strings.TrimSpace(record.GroupCardname) == "" {
			continue
		}
		valid = append(valid, GroupCardnameInfo{
			GroupID:       strings.TrimSpace(record.GroupID),
			GroupCardname: strings.TrimSpace(record.GroupCardname),
		})
	}
	return valid
}

// ---------------------------------------------------------------------------
// PersonStore 持久化边界
// ---------------------------------------------------------------------------

// PersonRecord 是 person_info 表的行（对齐 database_model.PersonInfo）。
type PersonRecord struct {
	ID             int64
	IsKnown        bool
	PersonID       string
	PersonName     *string
	NameReason     *string
	Platform       string
	UserID         string
	UserNickname   string
	GroupCardname  *string // JSON: [{"group_id":..., "group_cardname":...}]
	MemoryPoints   *string // JSON
	KnowCounts     int
	FirstKnownTime *time.Time
	LastKnownTime  *time.Time
}

// PersonStore 是人物持久化边界（由装配层适配 internal/database）。
type PersonStore interface {
	// GetByID 按 person_id 查询（不存在返回 nil, nil）。
	GetByID(ctx context.Context, personID string) (*PersonRecord, error)
	// GetIDByName 按 person_name 查询 person_id（不存在返回空串）。
	GetIDByName(ctx context.Context, personName string) (string, error)
	// Upsert 插入或更新记录（按 person_id）。
	Upsert(ctx context.Context, record *PersonRecord) error
}

// ---------------------------------------------------------------------------
// Person — 对齐 person_info.Person
// ---------------------------------------------------------------------------

// Person 对齐 Person 类：人物内存对象 + 数据库读写。
type Person struct {
	// IsKnown mirrors is_known。
	IsKnown bool
	// PersonID mirrors person_id（md5 派生）。
	PersonID string
	// Platform / UserID mirrors platform / user_id。
	Platform string
	UserID   string
	// Nickname mirrors user_nickname。
	Nickname string
	// PersonName mirrors person_name。
	PersonName string
	// NameReason mirrors name_reason。
	NameReason string
	// KnowTimes mirrors know_times（know_counts 列）。
	KnowTimes int
	// KnowSince / LastKnow mirrors know_since / last_know（unix 秒）。
	KnowSince *float64
	LastKnow  *float64
	// MemoryPoints mirrors memory_points（"category:text:weight" 字符串列表）。
	MemoryPoints []string
	// GroupCardnameList mirrors group_cardname_list。
	GroupCardnameList []GroupCardnameInfo

	store Store
}

// Store 是 Person 持久化边界的别名（注入用）。
type Store = PersonStore

// NewPerson mirrors Person.__init__(person_id=...)/init：按 person_id 加载。
// 未认识的人 is_known=False（对齐 Python 语义：不认识不落库）。
func NewPerson(ctx context.Context, store Store, personID string) *Person {
	person := &Person{store: store, IsKnown: false}
	if strings.TrimSpace(personID) == "" {
		return person
	}
	person.PersonID = personID
	person.loadDefaults()
	person.loadFromDatabase(ctx)
	return person
}

// NewPersonByName mirrors Person.__init__(person_name=...)：按用户名解析。
func NewPersonByName(ctx context.Context, store Store, personName string) *Person {
	person := &Person{store: store, IsKnown: false}
	if strings.TrimSpace(personName) == "" {
		return person
	}
	personID, err := store.GetIDByName(ctx, strings.TrimSpace(personName))
	if err != nil || personID == "" {
		return person // is_known=False
	}
	person.PersonID = personID
	person.loadDefaults()
	person.loadFromDatabase(ctx)
	return person
}

// NewPersonByPlatformUser mirrors Person.__init__(platform=..., user_id=...)。
func NewPersonByPlatformUser(ctx context.Context, store Store, platform, userID string) *Person {
	return NewPerson(ctx, store, GetPersonID(platform, userID))
}

func (p *Person) loadDefaults() {
	p.Nickname = ""
	p.PersonName = ""
	p.NameReason = ""
	p.KnowTimes = 0
	p.KnowSince = nil
	p.LastKnow = nil
	p.MemoryPoints = []string{}
	p.GroupCardnameList = []GroupCardnameInfo{}
}

// RegisterPerson mirrors Person.register_person：注册新用户（已存在则更新群
// 名片并返回实例）。
func RegisterPerson(
	ctx context.Context,
	store Store,
	platform string,
	userID string,
	nickname string,
	groupID string,
	groupNickname string,
) *Person {
	if strings.TrimSpace(platform) == "" || strings.TrimSpace(userID) == "" || strings.TrimSpace(nickname) == "" {
		return nil
	}
	personID := GetPersonID(platform, userID)
	if IsPersonKnown(ctx, store, personID, "", "") {
		person := NewPerson(ctx, store, personID)
		if groupID != "" && groupNickname != "" {
			person.AddGroupNickName(ctx, groupID, groupNickname)
		}
		return person
	}

	now := float64(time.Now().UnixNano()) / 1e9
	person := &Person{
		store:             store,
		IsKnown:           true, // 注册后立即标记为已认识
		PersonID:          personID,
		Platform:          platform,
		UserID:            userID,
		Nickname:          nickname,
		PersonName:        nickname, // 使用 nickname 作为初始 person_name
		NameReason:        "用户注册时设置的昵称",
		KnowTimes:         1,
		KnowSince:         &now,
		LastKnow:          &now,
		MemoryPoints:      []string{},
		GroupCardnameList: []GroupCardnameInfo{},
	}
	if groupID != "" && groupNickname != "" {
		person.GroupCardnameList = append(person.GroupCardnameList,
			GroupCardnameInfo{GroupID: groupID, GroupCardname: groupNickname})
	}
	person.SyncToDatabase(ctx)
	return person
}

// IsPersonKnown mirrors is_person_known（三态入参，任一命中即可）。
func IsPersonKnown(ctx context.Context, store Store, personID, userID, platform string) bool {
	if store == nil {
		return false
	}
	queryID := personID
	if queryID == "" && userID != "" && platform != "" {
		queryID = GetPersonID(platform, userID)
	}
	if queryID != "" {
		record, err := store.GetByID(ctx, queryID)
		if err == nil && record != nil {
			return record.IsKnown
		}
		return false
	}
	return false
}

// IsPersonKnownByName mirrors is_person_known(person_name=...)。
func IsPersonKnownByName(ctx context.Context, store Store, personName string) bool {
	if store == nil || strings.TrimSpace(personName) == "" {
		return false
	}
	personID, err := store.GetIDByName(ctx, strings.TrimSpace(personName))
	if err != nil || personID == "" {
		return false
	}
	return IsPersonKnown(ctx, store, personID, "", "")
}

// loadFromDatabase mirrors load_from_database。
func (p *Person) loadFromDatabase(ctx context.Context) {
	if p.store == nil {
		return
	}
	record, err := p.store.GetByID(ctx, p.PersonID)
	if err != nil {
		return
	}
	if record == nil {
		// 用户不存在：保持默认值（is_known=False，sync 不写库，对齐 Python 分支：
		// sync_to_database 在 is_known=False 时直接 return）。
		return
	}
	p.IsKnown = record.IsKnown
	p.UserID = record.UserID
	p.Platform = record.Platform
	p.Nickname = record.UserNickname
	if record.PersonName != nil {
		p.PersonName = *record.PersonName
	}
	if p.PersonName == "" {
		p.PersonName = p.Nickname
	}
	if record.NameReason != nil {
		p.NameReason = *record.NameReason
	}
	p.KnowTimes = record.KnowCounts
	if record.FirstKnownTime != nil {
		seconds := float64(record.FirstKnownTime.UnixNano()) / 1e9
		p.KnowSince = &seconds
	}
	if record.LastKnownTime != nil {
		seconds := float64(record.LastKnownTime.UnixNano()) / 1e9
		p.LastKnow = &seconds
	}
	p.MemoryPoints = parseMemoryPoints(record.MemoryPoints)
	p.GroupCardnameList = parseGroupCardnameJSON(orEmpty(record.GroupCardname))
}

func parseMemoryPoints(raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return []string{}
	}
	var points []string
	if err := json.Unmarshal([]byte(*raw), &points); err != nil {
		return []string{}
	}
	valid := make([]string, 0, len(points))
	for _, point := range points {
		if point != "" {
			valid = append(valid, point)
		}
	}
	return valid
}

func orEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// SyncToDatabase mirrors sync_to_database：is_known=False 时不写库。
func (p *Person) SyncToDatabase(ctx context.Context) {
	if !p.IsKnown || p.store == nil {
		return
	}
	record := &PersonRecord{
		IsKnown:      p.IsKnown,
		PersonID:     p.PersonID,
		Platform:     p.Platform,
		UserID:       p.UserID,
		UserNickname: p.Nickname,
		KnowCounts:   p.KnowTimes,
	}
	if p.PersonName != "" {
		name := p.PersonName
		record.PersonName = &name
	}
	if p.NameReason != "" {
		reason := p.NameReason
		record.NameReason = &reason
	}
	cardname := dumpGroupCardnameRecords(p.GroupCardnameList)
	if cardname != "" {
		record.GroupCardname = &cardname
	}
	memoryPoints := dumpMemoryPoints(p.MemoryPoints)
	if memoryPoints != "" {
		record.MemoryPoints = &memoryPoints
	}
	if p.KnowSince != nil {
		first := time.Unix(int64(*p.KnowSince), 0)
		record.FirstKnownTime = &first
	}
	if p.LastKnow != nil {
		last := time.Unix(int64(*p.LastKnow), 0)
		record.LastKnownTime = &last
	}
	if err := p.store.Upsert(ctx, record); err != nil {
		_ = err // 对齐 Python：同步失败只打日志，不上抛
	}
}

func dumpMemoryPoints(points []string) string {
	if len(points) == 0 {
		return "[]"
	}
	encoded, err := json.Marshal(points)
	if err != nil {
		return "[]"
	}
	return string(encoded)
}

// AddGroupNickName mirrors add_group_nick_name：添加或更新群名片。
func (p *Person) AddGroupNickName(ctx context.Context, groupID string, groupNickname string) {
	if strings.TrimSpace(groupID) == "" || strings.TrimSpace(groupNickname) == "" {
		return
	}
	for i := range p.GroupCardnameList {
		if p.GroupCardnameList[i].GroupID == groupID {
			p.GroupCardnameList[i].GroupCardname = groupNickname
			p.SyncToDatabase(ctx)
			return
		}
	}
	p.GroupCardnameList = append(p.GroupCardnameList,
		GroupCardnameInfo{GroupID: groupID, GroupCardname: groupNickname})
	p.SyncToDatabase(ctx)
}

// GetGroupCardname 返回指定群的群名片（无记录返回空串）。
func (p *Person) GetGroupCardname(groupID string) string {
	for _, record := range p.GroupCardnameList {
		if record.GroupID == groupID {
			return record.GroupCardname
		}
	}
	return ""
}

// DisplayName 返回展示名（群名片 > 昵称 > person_name）。
func (p *Person) DisplayName(groupID string) string {
	if cardname := p.GetGroupCardname(groupID); cardname != "" {
		return cardname
	}
	if p.Nickname != "" {
		return p.Nickname
	}
	return p.PersonName
}

// ---------------------------------------------------------------------------
// memory_points 操作
// ---------------------------------------------------------------------------

// calculateStringSimilarity mirrors calculate_string_similarity（Levenshtein）。
func CalculateStringSimilarity(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	if s1 == "" || s2 == "" {
		return 0.0
	}
	distance := levenshteinDistance(s1, s2)
	maxLen := len([]rune(s1))
	if l2 := len([]rune(s2)); l2 > maxLen {
		maxLen = l2
	}
	if maxLen == 0 {
		return 0
	}
	return 1 - float64(distance)/float64(maxLen)
}

func levenshteinDistance(s1, s2 string) int {
	r1, r2 := []rune(s1), []rune(s2)
	if len(r1) < len(r2) {
		r1, r2 = r2, r1
	}
	if len(r2) == 0 {
		return len(r1)
	}
	previous := make([]int, len(r2)+1)
	current := make([]int, len(r2)+1)
	for j := 0; j <= len(r2); j++ {
		previous[j] = j
	}
	for i := 1; i <= len(r1); i++ {
		current[0] = i
		for j := 1; j <= len(r2); j++ {
			insertions := previous[j] + 1
			deletions := current[j-1] + 1
			substitutions := previous[j-1]
			if r1[i-1] != r2[j-1] {
				substitutions++
			}
			current[j] = min3(insertions, deletions, substitutions)
		}
		previous, current = current, previous
	}
	return previous[len(r2)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}

// DelMemory mirrors del_memory：删除指定分类且内容相似度达阈值的记忆点，
// 返回删除数量（删除后同步数据库）。
func (p *Person) DelMemory(ctx context.Context, category string, memoryContent string, similarityThreshold float64) int {
	if len(p.MemoryPoints) == 0 {
		return 0
	}
	if similarityThreshold <= 0 {
		similarityThreshold = 0.95
	}
	deleted := 0
	kept := make([]string, 0, len(p.MemoryPoints))
	for _, point := range p.MemoryPoints {
		parts := strings.SplitN(point, ":", 3)
		if len(parts) < 3 {
			kept = append(kept, point)
			continue
		}
		memoryCategory := strings.TrimSpace(parts[0])
		memoryText := strings.TrimSpace(parts[1])
		_ = strings.TrimSpace(parts[2]) // weight，不参与匹配
		if memoryCategory != category {
			kept = append(kept, point)
			continue
		}
		if CalculateStringSimilarity(memoryContent, memoryText) >= similarityThreshold {
			deleted++
			continue
		}
		kept = append(kept, point)
	}
	p.MemoryPoints = kept
	if deleted > 0 {
		p.SyncToDatabase(ctx)
	}
	return deleted
}

// AddMemoryPoint 添加一个记忆点（"category:text:weight"）。
func (p *Person) AddMemoryPoint(ctx context.Context, category, text, weight string) {
	if weight == "" {
		weight = "1"
	}
	if _, err := strconv.ParseFloat(weight, 64); err != nil {
		weight = "1"
	}
	p.MemoryPoints = append(p.MemoryPoints, fmt.Sprintf("%s:%s:%s", category, text, weight))
	p.SyncToDatabase(ctx)
}

// ---------------------------------------------------------------------------
// resolve_person_id_for_memory
// ---------------------------------------------------------------------------

// ResolvePersonIDForMemory mirrors resolve_person_id_for_memory：
// 1) 按 person_name 映射 2) 回退 platform+user_id 生成
// 3) strictKnown=True 时要求该 person_id 已被认识。
func ResolvePersonIDForMemory(ctx context.Context, store Store, personName, platform, userID string, strictKnown bool) string {
	cleanName := strings.TrimSpace(personName)
	if cleanName != "" && store != nil {
		if byName, err := store.GetIDByName(ctx, cleanName); err == nil && byName != "" {
			return byName
		}
	}
	cleanPlatform := strings.TrimSpace(platform)
	cleanUserID := strings.TrimSpace(userID)
	if cleanPlatform != "" && cleanUserID != "" {
		candidate := GetPersonID(cleanPlatform, cleanUserID)
		if strictKnown && !IsPersonKnown(ctx, store, candidate, "", "") {
			return ""
		}
		return candidate
	}
	return ""
}
