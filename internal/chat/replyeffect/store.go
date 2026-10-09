package replyeffect

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// This file mirrors 参考/MaiBot/src/maisaka/reply_effect/storage.py 的持久化语义，
// 收敛为两个实现：
//
//   - MemoryStore：进程内存储（测试 + 单测状态机用）；
//   - DatabaseStore（database.go）：写 mai_reply_effects 表（internal/database）。
//
// Python 版的 JSON 镜像（logs/maisaka_reply_effect/*.json.gz）在 Go 版由
// TrackerConfig.MirrorDir 的可选诊断镜像承担，不作为权威存储。

// Store 是回复效果记录的持久化边界。
type Store interface {
	// Save 写入或覆盖一条记录（effect_id 为主键，状态变更时反复写入）。
	Save(ctx context.Context, record *Record) error
	// GetByID 按 effect_id 读取（不存在返回 nil）。
	GetByID(ctx context.Context, effectID string) (*Record, error)
	// LoadUnfinished 返回该会话中还未结算的记录
	// （pending / evaluating / evaluation_failed，按创建时间升序）。
	LoadUnfinished(ctx context.Context, sessionID string) ([]*Record, error)
	// LoadByIDs 批量读取（恢复评审所需的候选记录）。
	LoadByIDs(ctx context.Context, effectIDs []string) ([]*Record, error)
}

// MemoryStore 是内存实现（记录以副本存储，调用方修改不影响已保存内容）。
type MemoryStore struct {
	mu      sync.Mutex
	records map[string]*Record
}

// NewMemoryStore 构造内存 store。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]*Record{}}
}

// Save implements Store.
func (s *MemoryStore) Save(_ context.Context, record *Record) error {
	if record == nil {
		return fmt.Errorf("回复效果记录为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[record.EffectID] = record.Clone()
	return nil
}

// GetByID implements Store.
func (s *MemoryStore) GetByID(_ context.Context, effectID string) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[effectID]
	if !ok {
		return nil, nil
	}
	return record.Clone(), nil
}

// LoadUnfinished implements Store.
func (s *MemoryStore) LoadUnfinished(_ context.Context, sessionID string) ([]*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]*Record, 0)
	for _, record := range s.records {
		if sessionID != "" && record.Session.SessionID != sessionID {
			continue
		}
		switch record.Status {
		case StatusPending, StatusEvaluating, StatusEvaluationFailed:
			records = append(records, record.Clone())
		}
	}
	sortRecordsByCreatedAt(records)
	return records, nil
}

// LoadByIDs implements Store.
func (s *MemoryStore) LoadByIDs(_ context.Context, effectIDs []string) ([]*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]*Record, 0, len(effectIDs))
	for _, effectID := range effectIDs {
		if record, ok := s.records[effectID]; ok {
			records = append(records, record.Clone())
		}
	}
	return records, nil
}

// All 返回全部记录（时间升序；测试与内存侧聚合用）。
func (s *MemoryStore) All() []*Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	records := make([]*Record, 0, len(s.records))
	for _, record := range s.records {
		records = append(records, record.Clone())
	}
	sortRecordsByCreatedAt(records)
	return records
}

func sortRecordsByCreatedAt(records []*Record) {
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt == records[j].CreatedAt {
			return records[i].EffectID < records[j].EffectID
		}
		return records[i].CreatedAt < records[j].CreatedAt
	})
}

// encodeRecordJSON 序列化一条记录（镜像与测试用，字段名与 Python to_json_dict 一致）。
func encodeRecordJSON(record *Record) ([]byte, error) {
	return json.Marshal(record)
}

// DecodeRecordJSON 复原一条记录（导入路径与测试共用）。
func DecodeRecordJSON(payload []byte) (*Record, error) {
	var record Record
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, err
	}
	if record.EffectID == "" {
		return nil, fmt.Errorf("回复效果记录缺少 effect_id")
	}
	if record.FollowupMessages == nil {
		record.FollowupMessages = []FollowupSnapshot{}
	}
	if record.ContextSnapshot == nil {
		record.ContextSnapshot = []ContextItem{}
	}
	if record.EvaluationVersion == 0 {
		record.EvaluationVersion = EvaluationVersion
	}
	if record.SchemaVersion == 0 {
		record.SchemaVersion = SchemaVersion
	}
	return &record, nil
}
