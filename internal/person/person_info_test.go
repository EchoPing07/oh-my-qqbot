package person

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oh-my-qqbot/internal/database"
)

type memStore struct {
	records map[string]*PersonRecord
}

func newMemStore() *memStore { return &memStore{records: map[string]*PersonRecord{}} }

func (s *memStore) GetByID(ctx context.Context, personID string) (*PersonRecord, error) {
	if record := s.records[personID]; record != nil {
		cloned := *record
		return &cloned, nil
	}
	return nil, nil
}

func (s *memStore) GetIDByName(ctx context.Context, personName string) (string, error) {
	for _, record := range s.records {
		if record.PersonName != nil && *record.PersonName == personName {
			return record.PersonID, nil
		}
	}
	return "", nil
}

func (s *memStore) Upsert(ctx context.Context, record *PersonRecord) error {
	cloned := *record
	s.records[record.PersonID] = &cloned
	return nil
}

// person_id：md5(platform_user)；platform 含 "-" 取后半段。
func TestGetPersonID(t *testing.T) {
	id := GetPersonID("qq", "123456")
	// md5("qq_123456") 的十六进制串。
	if len(id) != 32 {
		t.Fatalf("person_id 应为 md5 hex: %q", id)
	}
	// 平台含 "-" 时取后半段：get_person_id("qq_official-qq", "1") == get_person_id("qq", "1")
	if GetPersonID("qq_official-qq", "123456") != id {
		t.Fatalf("平台名应取 '-' 后半段")
	}
	if GetPersonID("qq", "1") == GetPersonID("qq", "2") {
		t.Fatal("不同 user_id 的 person_id 应不同")
	}
}

// 注册：know_times=1、is_known=True、person_name=nickname。
func TestRegisterPerson(t *testing.T) {
	store := newMemStore()
	person := RegisterPerson(context.Background(), store, "qq", "u1", "小明", "g1", "群内小卡")
	if person == nil {
		t.Fatal("注册应成功")
	}
	if person.PersonID != GetPersonID("qq", "u1") {
		t.Fatalf("person_id 应按 md5 计算: %s", person.PersonID)
	}
	if !person.IsKnown || person.KnowTimes != 1 {
		t.Fatalf("注册后应 known 且 know_times=1: %+v", person)
	}
	if person.PersonName != "小明" {
		t.Fatalf("person_name 应为 nickname: %q", person.PersonName)
	}
	if cardname := person.GetGroupCardname("g1"); cardname != "群内小卡" {
		t.Fatalf("群名片应记录: %q", cardname)
	}
	// 落库校验
	record := store.records[person.PersonID]
	if record == nil || !record.IsKnown || record.KnowCounts != 1 {
		t.Fatalf("记录应写入数据库: %+v", record)
	}

	// 重复注册返回已有实例且 know_times 不变（is_person_known 分支）。
	again := RegisterPerson(context.Background(), store, "qq", "u1", "小明", "g1", "群内小卡2")
	if again.KnowTimes != 1 {
		t.Fatalf("重复注册不应增加 know_times: %d", again.KnowTimes)
	}
	if again.GetGroupCardname("g1") != "群内小卡2" {
		t.Fatalf("重复注册应更新群名片: %q", again.GetGroupCardname("g1"))
	}
}

// 群名片 JSON 序列化/反序列化 + 更新语义。
func TestGroupCardname(t *testing.T) {
	store := newMemStore()
	person := RegisterPerson(context.Background(), store, "qq", "u2", "小红", "", "")
	person.AddGroupNickName(context.Background(), "g1", "卡1")
	person.AddGroupNickName(context.Background(), "g2", "卡2")
	// 更新已有群
	person.AddGroupNickName(context.Background(), "g1", "卡1新")

	// 重新加载验证持久化
	loaded := NewPerson(context.Background(), store, person.PersonID)
	if loaded.GetGroupCardname("g1") != "卡1新" {
		t.Fatalf("更新后的群名片应持久化: %q", loaded.GetGroupCardname("g1"))
	}
	if loaded.GetGroupCardname("g2") != "卡2" {
		t.Fatalf("第二张群名片应保留: %q", loaded.GetGroupCardname("g2"))
	}

	// dump/parse 往返
	raw := dumpGroupCardnameRecords(person.GroupCardnameList)
	parsed := parseGroupCardnameJSON(raw)
	if len(parsed) != 2 {
		t.Fatalf("群名片往返应保留 2 条: %d", len(parsed))
	}
	if parseGroupCardnameJSON("") != nil || parseGroupCardnameJSON("not json") != nil {
		t.Fatal("非法输入应返回空列表")
	}
}

// know_times 增长（Upsert 分支）。
func TestKnowTimesGrowth(t *testing.T) {
	store := newMemStore()
	person := RegisterPerson(context.Background(), store, "qq", "u3", "小刚", "", "")
	person.KnowTimes = 5
	person.SyncToDatabase(context.Background())

	loaded := NewPerson(context.Background(), store, person.PersonID)
	if loaded.KnowTimes != 5 {
		t.Fatalf("know_times 应持久化: %d", loaded.KnowTimes)
	}
	if loaded.KnowSince == nil || loaded.LastKnow == nil {
		t.Fatalf("认识时间应持久化: %+v", loaded)
	}
}

// 未认识的人：is_known=False（对齐 Python：记录不存在时保持默认值并创建），
// 且 sync 不写库。
func TestUnknownPerson(t *testing.T) {
	store := newMemStore()
	person := NewPerson(context.Background(), store, GetPersonID("qq", "nobody"))
	if person.IsKnown {
		t.Fatal("未注册的人不应 known")
	}
	if len(store.records) != 0 {
		t.Fatal("未认识的人不应写库")
	}
	if IsPersonKnown(context.Background(), store, "", "", "") {
		t.Fatal("空参应返回 false")
	}
}

// memory_points 删除（相似度阈值语义）。
func TestDelMemory(t *testing.T) {
	store := newMemStore()
	person := RegisterPerson(context.Background(), store, "qq", "u4", "小丽", "", "")
	person.MemoryPoints = []string{
		"occupation:程序员:0.8",
		"occupation:产品经理:0.6",
		"hobby:打游戏:0.9",
		"malformed",
	}
	deleted := person.DelMemory(context.Background(), "occupation", "程序员", 0.95)
	if deleted != 1 {
		t.Fatalf("应删除 1 条: %d", deleted)
	}
	if len(person.MemoryPoints) != 3 {
		t.Fatalf("应保留 3 条: %v", person.MemoryPoints)
	}
	// 相似但未达阈值不删（"程序员" vs "程序媛" 相似度 2/3 < 0.95）。
	person.MemoryPoints = []string{"occupation:程序媛:0.5"}
	if got := person.DelMemory(context.Background(), "occupation", "程序员", 0.95); got != 0 {
		t.Fatalf("相似度未达阈值不应删除: %d", got)
	}
	if got := person.DelMemory(context.Background(), "occupation", "程序媛", 0.95); got != 1 {
		t.Fatalf("相同内容应删除: %d", got)
	}
}

func TestCalculateStringSimilarity(t *testing.T) {
	if got := CalculateStringSimilarity("abc", "abc"); got != 1 {
		t.Fatalf("相同串相似度应为 1: %v", got)
	}
	if got := CalculateStringSimilarity("abc", "abd"); got <= 0.6 || got >= 1 {
		t.Fatalf("编辑距离 1 的相似度应为 2/3: %v", got)
	}
	if got := CalculateStringSimilarity("", "abc"); got != 0 {
		t.Fatalf("空串相似度应为 0: %v", got)
	}
}

// 按名字解析 person_id。
func TestResolvePersonIDForMemory(t *testing.T) {
	store := newMemStore()
	person := RegisterPerson(context.Background(), store, "qq", "u5", "小张", "", "")

	if got := ResolvePersonIDForMemory(context.Background(), store, "小张", "", "", false); got != person.PersonID {
		t.Fatalf("按名解析应命中: %q vs %q", got, person.PersonID)
	}
	if got := ResolvePersonIDForMemory(context.Background(), store, "", "qq", "u5", false); got != person.PersonID {
		t.Fatalf("按平台+用户解析应命中: %q", got)
	}
	if got := ResolvePersonIDForMemory(context.Background(), store, "", "qq", "nobody", true); got != "" {
		t.Fatalf("strictKnown 且未认识应返回空: %q", got)
	}
	if got := ResolvePersonIDForMemory(context.Background(), store, "", "qq", "u5", true); got != person.PersonID {
		t.Fatalf("strictKnown 且已认识应命中: %q", got)
	}
}

// DatabaseStore 集成：真实 SQLite 建表读写。
func TestDatabaseStoreRoundTrip(t *testing.T) {
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewDatabaseStore(db)

	personID := GetPersonID("qq", "u9")
	now := time.Now()
	err = store.Upsert(context.Background(), &PersonRecord{
		IsKnown:        true,
		PersonID:       personID,
		PersonName:     strPtr("小九"),
		Platform:       "qq",
		UserID:         "u9",
		UserNickname:   "九九",
		GroupCardname:  strPtr(`[{"group_id":"g1","group_cardname":"卡九"}]`),
		MemoryPoints:   strPtr(`["hobby:跑步:1"]`),
		KnowCounts:     2,
		FirstKnownTime: &now,
		LastKnownTime:  &now,
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := store.GetByID(context.Background(), personID)
	if err != nil || record == nil {
		t.Fatalf("应能读取记录: %v %v", record, err)
	}
	if record.KnowCounts != 2 || record.UserNickname != "九九" {
		t.Fatalf("记录字段不符: %+v", record)
	}
	if got, _ := store.GetIDByName(context.Background(), "小九"); got != personID {
		t.Fatalf("按名查询应命中: %q", got)
	}

	// 全量更新
	record.KnowCounts = 7
	record.UserNickname = "九九改"
	if err := store.Upsert(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	updated, _ := store.GetByID(context.Background(), personID)
	if updated.KnowCounts != 7 || updated.UserNickname != "九九改" {
		t.Fatalf("全量更新失败: %+v", updated)
	}
	_ = db.Read()
	_ = strings.TrimSpace("")
}

func strPtr(s string) *string { return &s }
