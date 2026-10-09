// Package internal holds the shared cross-module contracts of oh-my-qqbot.
//
// This file defines the interfaces that bind the chat pipeline together so
// that parallel work packages can be built against a single contract.
// Nothing here imports other internal packages: dependencies point inward.
package internal

import (
	"context"
	"time"
)

// ---------------------------------------------------------------------------
// Message representation passed between Channel and ChatManager
// ---------------------------------------------------------------------------

// Role mirrors OpenAI chat roles.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is one function call requested by the model.
type ToolCall struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	ArgsJSON string         `json:"arguments"`
	Extra    map[string]any `json:"extra,omitempty"`
}

// ChatRequest is a normalized chat-completions request.
type ChatRequest struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ToolSpec
	MaxTokens   int
	Temperature *float64
	ExtraParams map[string]any
}

// ChatMessage is one message of a ChatRequest/ChatResponse conversation.
type ChatMessage struct {
	Role       Role
	Content    string
	ToolCallID string
	ToolCalls  []ToolCall
	Images     []ImagePart
	Name       string
}

// ImagePart carries a base64 data URL or remote URL for vision models.
type ImagePart struct {
	URL    string
Base64  string
	Detail string
}

// ToolSpec declares a callable tool to the model.
type ToolSpec struct {
	Name        string
	Description string
	// ParametersJSON is a JSON schema object (as raw JSON string).
	ParametersJSON string
}

// Usage reports token accounting for one LLM call.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheEnabled     bool
	CacheHitTokens   int
	CacheMissTokens  int
}

// ChatResponse is the normalized completion result.
type ChatResponse struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	Model        string
}

// ---------------------------------------------------------------------------
// LLM orchestration contract (internal/llm)
// ---------------------------------------------------------------------------

// LLMLayer routes task-based requests to configured models with retries,
// timeouts and usage accounting. Implemented by internal/llm.
type LLMLayer interface {
	// Chat executes one chat-completions request for a logical task
	// (e.g. "planner", "replyer", "utils", "vlm", "embedding").
	Chat(ctx context.Context, task string, req ChatRequest) (*ChatResponse, error)
	// Embed computes embeddings for the given texts via the embedding task.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// ---------------------------------------------------------------------------
// Memory kernel contract (internal/memorix) — A_memorix rewrite
// ---------------------------------------------------------------------------

// MemoryHit is one retrieved memory item.
type MemoryHit struct {
	Kind       string  // "paragraph" | "relation" | "episode" | "profile"
	ID         string  // storage id / hash
	Text       string  // rendered content
	Score      float64 // fused score
	ChatID     string
	Time       time.Time
	Similarity float64
}

// PersonProfile is the profile view of one person.
type PersonProfile struct {
	PersonID  string            `json:"person_id"`
	Name      string            `json:"name"`
	Facts     []ProfileFact     `json:"facts"`
	Relations []ProfileRelation `json:"relations,omitempty"`
	Summary   string            `json:"summary,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// ProfileFact is one verified fact about a person.
type ProfileFact struct {
	ID         string    `json:"id"`
	Category   string    `json:"category"`
	Content    string    `json:"content"`
	Confidence float64   `json:"confidence"`
	Source     string    `json:"source,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// ProfileRelation links two persons.
type ProfileRelation struct {
	TargetPersonID string  `json:"target_person_id"`
	Kind           string  `json:"kind"`
	Confidence     float64 `json:"confidence"`
}

// MemoryKernel is the SDK-facing surface of the A_memorix kernel (§6.14).
type MemoryKernel interface {
	// IngestSummary ingests a chat summary paragraph (chat recall path).
	IngestSummary(ctx context.Context, chatID string, summary string, ts time.Time) error
	// IngestText ingests raw text, extracting entities/relations/paragraphs.
	IngestText(ctx context.Context, chatID string, text string, ts time.Time) error
	// SearchMemory runs the dual-path retrieval; mode is
	// "search"|"time"|"hybrid"|"episode"|"aggregate".
	SearchMemory(ctx context.Context, mode string, query string, chatID string, topK int) ([]MemoryHit, error)
	// GetPersonProfile returns the aggregated profile of one person.
	GetPersonProfile(ctx context.Context, personID string) (*PersonProfile, error)
	// UpsertPersonFact writes one fact into the profile store.
	UpsertPersonFact(ctx context.Context, personID string, name string, fact ProfileFact) error
	// IngestImageMemory stores an image description as memory.
	IngestImageMemory(ctx context.Context, chatID string, description string, hash string, ts time.Time) error
	// Close flushes and closes the kernel storage.
	Close() error
}

// ---------------------------------------------------------------------------
// Chat sender contract (internal/chat/sender)
// ---------------------------------------------------------------------------

// SendRequest describes one outbound message component already post-processed.
type SendRequest struct {
	SessionID   string
	ChatType    string // "group" | "private"
	GroupID     string
	GroupPlatform string
	TargetUserID string
	TargetUserName string
	ReplyTo     string // message id to quote (first segment only)
	Contents    []OutboundSegment
}

// OutboundSegment is one physical message to deliver.
type OutboundSegment struct {
	Type  string // "text" | "emoji" | "image"
	Text  string
	EmojiHash string
	ImagePath string
}

// SendResult reports delivery outcome.
type SendResult struct {
	MessageID  string // platform message id of the sent message
	Passive    bool   // true when delivered as a passive reply
	Err        error
}

// Sender delivers processed outbound messages through the channel layer and
// persists them. Implemented by internal/chat/sender.
type Sender interface {
	Send(ctx context.Context, req SendRequest) ([]SendResult, error)
}

// ---------------------------------------------------------------------------
// Monitor event bus contract (internal/monitor)
// ---------------------------------------------------------------------------

// EventKind enumerates monitor event types (aligned with maisaka/monitor/events.py).
type EventKind string

const (
	EventPlannerProgress  EventKind = "planner.progress"
	EventSessionStart     EventKind = "session.start"
	EventStageStatus      EventKind = "stage.status"
	EventMessageIngested  EventKind = "message.ingested"
	EventMessageSent      EventKind = "message.sent"
	EventLLMRetry         EventKind = "llm.retry"
	EventLLMError         EventKind = "llm.error"
)

// Event is one monitor event.
type Event struct {
	Kind      EventKind      `json:"kind"`
	SessionID string         `json:"session_id"`
	Time      time.Time      `json:"time"`
	Payload   map[string]any `json:"payload"`
}

// EventBus is the process-wide observation bus; zero subscribers = no cost.
type EventBus interface {
	Emit(ev Event)
	Subscribe(buffer int) (ch <-chan Event, cancel func())
}
