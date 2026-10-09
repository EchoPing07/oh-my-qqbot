// Package config implements the MaiBot dual-file configuration system:
// bot_config.toml (aligned with 参考MaiBot/src/config/official_configs.py,
// trimmed to the REWRITE_PLAN §5 surface) and model_config.toml (aligned with
// model_configs.py + default_model_config.py).
//
// Field names, defaults and the `mid_term_memory_lenth` spelling are kept
// identical to the Python version so user configs migrate verbatim.
package config

// ---------------------------------------------------------------------------
// BotConfig — aligned with official_configs.BotConfig
// ---------------------------------------------------------------------------

// BotConfig 对齐 official_configs.BotConfig（机器人配置类）。
type BotConfig struct {
	Platform   string   `toml:"platform"`   // 适配器没有上报身份时使用的备用主平台，例如 qq
	QQAccount  string   `toml:"qq_account"` // 备用 QQ 账号
	Platforms  []string `toml:"platforms"`  // 其他备用平台，格式为 platform:账号
	Nickname   string   `toml:"nickname"`   // 麦麦显示和自称时使用的名字（默认 麦麦）
	AliasNames []string `toml:"alias_names"`
}

func defaultBotConfig() BotConfig {
	return BotConfig{
		Platform:   "",
		QQAccount:  "",
		Platforms:  []string{},
		Nickname:   "麦麦",
		AliasNames: []string{},
	}
}

// ---------------------------------------------------------------------------
// PersonalityConfig — aligned with official_configs.PersonalityConfig
// ---------------------------------------------------------------------------

// PersonalityConfig 对齐 official_configs.PersonalityConfig（人格配置类）。
type PersonalityConfig struct {
	Personality         string   `toml:"personality"`
	BehaviorStyle       string   `toml:"behavior_style"`
	ReplyStyle          string   `toml:"reply_style"`
	MultipleReplyStyle  []string `toml:"multiple_reply_style"`
	MultipleProbability float64  `toml:"multiple_probability"`
}

// PersonalityEmotionSuffixes 对齐 PERSONALITY_EMOTION_SUFFIXES。
var PersonalityEmotionSuffixes = map[string]string{
	"rational_calm": "你整体理性冷静，回应时更偏向客观、克制和清晰判断，少用强烈情绪表达。",
	"neutral":       "",
	"sentimental":   "你更敏感细腻，容易被聊天氛围触动，会自然表现出一点惆怅、共情或情绪波动，但不要过度煽情。",
}

// BuildPersonalityEmotionSuffix 对齐 build_personality_emotion_suffix。
func BuildPersonalityEmotionSuffix(emotionTrait string) string {
	if s, ok := PersonalityEmotionSuffixes[emotionTrait]; ok {
		return s
	}
	return PersonalityEmotionSuffixes["neutral"]
}

func defaultPersonalityConfig() PersonalityConfig {
	return PersonalityConfig{
		Personality:   "是一个大二女大学生，现在正在上网和群友聊天。善于用人类的角度思考问题，聊天偏日常。",
		BehaviorStyle: "是大二女大学生，现在正在上网和群友聊天。善于用人类的角度思考问题，聊天偏日常。不会没话题硬找话题，",
		ReplyStyle:    "你的风格平淡简短，可以参考贴吧的回复风格。不滥用比喻或者生硬句子。视情况省略主语或者进行倒装，风格较为随意。",
		MultipleReplyStyle: []string{
			"你的风格平淡但不失讽刺，很简短,很白话。可以参考贴吧，微博的回复风格。",
			"用1-2个字进行回复",
			"用1-2个符号进行回复",
			"言辭凝練古雅，穿插《論語》經句卻不晦澀，以文言短句為基，輔以淺白語意，持長者溫和風範，全用繁體字表達，具先秦儒者談吐韻致。",
			"带点翻译腔，但不要太长",
		},
		MultipleProbability: 0,
	}
}

// ---------------------------------------------------------------------------
// talk_value rules / shared items — aligned with official_configs.TalkRulesItem
// ---------------------------------------------------------------------------

// TalkRulesItem 对齐 official_configs.TalkRulesItem（动态发言频率规则项）。
type TalkRulesItem struct {
	Platform string  `toml:"platform"` // 留空表示不限定平台，* 表示任意平台
	ItemID   string  `toml:"item_id"`  // 群号或用户 ID；留空表示不限定聊天，* 表示任意聊天
	RuleType string  `toml:"rule_type"`
	Time     string  `toml:"time"` // 留空为兜底，* 为全天，也可填 23:00-02:00
	Value    float64 `toml:"value"`
}

func defaultTalkRules() []TalkRulesItem {
	return []TalkRulesItem{
		{Platform: "", ItemID: "", RuleType: "group", Time: "00:00-08:59", Value: 0.8},
		{Platform: "", ItemID: "", RuleType: "group", Time: "09:00-18:59", Value: 1.0},
	}
}

// ExtraPromptItem 对齐 official_configs.ExtraPromptItem。
type ExtraPromptItem struct {
	Platform string `toml:"platform"`
	ItemID   string `toml:"item_id"`
	RuleType string `toml:"rule_type"`
	Prompt   string `toml:"prompt"`
}

// TargetItem 对齐 official_configs.TargetItem。
type TargetItem struct {
	Platform string `toml:"platform"`
	ItemID   string `toml:"item_id"`
	RuleType string `toml:"rule_type"`
}

// ChatStreamGroup 对齐 official_configs.ChatStreamGroup（聊天流共享组）。
type ChatStreamGroup struct {
	Targets []TargetItem `toml:"targets"`
}

// LearningItem 对齐 official_configs.LearningItem（表达/黑话/行为学习规则项）。
type LearningItem struct {
	Platform string `toml:"platform"`
	ItemID   string `toml:"item_id"`
	Type     string `toml:"type"`
	Use      bool   `toml:"use"`
	Learn    bool   `toml:"learn"`
}

func defaultLearningList() []LearningItem {
	return []LearningItem{{Platform: "", ItemID: "", Type: "group", Use: true, Learn: true}}
}

// ---------------------------------------------------------------------------
// ChatReplyTimingConfig — aligned with official_configs.ChatReplyTimingConfig
// ---------------------------------------------------------------------------

// ChatReplyTimingConfig 对齐 official_configs.ChatReplyTimingConfig（回复时机与频率）。
type ChatReplyTimingConfig struct {
	TalkValue                          float64         `toml:"talk_value"`
	PrivateTalkValue                   float64         `toml:"private_talk_value"`
	MentionedBotReply                  bool            `toml:"mentioned_bot_reply"`
	InevitableAtReply                  bool            `toml:"inevitable_at_reply"`
	ReplyTriggerMode                   string          `toml:"reply_trigger_mode"` // frequency | dynamic
	PlannerInterruptMaxConsecutiveCount int            `toml:"planner_interrupt_max_consecutive_count"`
	MaxConsecutiveWaitCount            int             `toml:"max_consecutive_wait_count"`
	NoActionBackoffBaseSeconds         float64         `toml:"no_action_backoff_base_seconds"`
	NoActionBackoffCapSeconds          float64         `toml:"no_action_backoff_cap_seconds"`
	NoActionBackoffStartCount          int             `toml:"no_action_backoff_start_count"`
	NoActionBackoffBypassPendingCount  int             `toml:"no_action_backoff_bypass_pending_count"`
	EnableTalkValueRules               bool            `toml:"enable_talk_value_rules"`
	TalkValueRules                     []TalkRulesItem `toml:"talk_value_rules"`
}

func defaultChatReplyTimingConfig() ChatReplyTimingConfig {
	return ChatReplyTimingConfig{
		TalkValue:                           1,
		PrivateTalkValue:                    1,
		MentionedBotReply:                   false,
		InevitableAtReply:                   true,
		ReplyTriggerMode:                    "frequency",
		PlannerInterruptMaxConsecutiveCount: 0,
		MaxConsecutiveWaitCount:             3,
		NoActionBackoffBaseSeconds:          15,
		NoActionBackoffCapSeconds:           300,
		NoActionBackoffStartCount:           2,
		NoActionBackoffBypassPendingCount:   6,
		EnableTalkValueRules:                false,
		TalkValueRules:                      defaultTalkRules(),
	}
}

// ---------------------------------------------------------------------------
// ChatReplyStyleConfig / ChatConfig — aligned with official_configs
// ---------------------------------------------------------------------------

// ChatReplyStyleConfig 对齐 official_configs.ChatReplyStyleConfig（回复方式）。
type ChatReplyStyleConfig struct {
	EnableReplyQuote  bool             `toml:"enable_reply_quote"`
	GroupChatPrompt   string           `toml:"group_chat_prompt"`
	PrivateChatPrompt string           `toml:"private_chat_prompts"` // Python 字段名 private_chat_prompts
	ChatPrompts       []ExtraPromptItem `toml:"chat_prompts"`
}

// DefaultGroupChatPrompt / DefaultPrivateChatPrompt 照抄 Python 默认文案（供 replyer 直接引用）。
const (
	DefaultGroupChatPrompt = "你正在qq群里聊天，下面是群里正在聊的内容，聊天中包含文字，图片和表情包等消息。\n" +
		"回复尽量简短一些。最好一次对一个话题进行回复，但必须考虑不同群友发言之间的交互，免得啰嗦或者回复内容太乱。请注意把握聊天内容。\n" +
		"不要总是提及自己的身份背景，根据聊天内容自由发挥，但是要日常不浮夸，不要刻意找话题。\n" +
		"不用刻意回复其他人发送的表情包，只要关注表情包表达的含义。你可以适当发送表情包表达情绪。控制回复的频率，不要每个人的消息都回复，优先回复你感兴趣的或者主动提及你的，适当回复其他话题。\n"
	DefaultPrivateChatPrompt = "你正在聊天，下面是正在聊的内容，其中包含聊天记录和聊天中的图片。\n" +
		"回复尽量简短一些。请注意把握聊天内容。\n" +
		"请考虑对方的发言频率，想法，思考自己何时回复以及回复内容。\n"
)

func defaultChatReplyStyleConfig() ChatReplyStyleConfig {
	return ChatReplyStyleConfig{
		EnableReplyQuote:  true,
		GroupChatPrompt:   DefaultGroupChatPrompt,
		PrivateChatPrompt: DefaultPrivateChatPrompt,
		ChatPrompts:       []ExtraPromptItem{},
	}
}

// ChatConfig 对齐 official_configs.ChatConfig（聊天配置类）。
// 注意 mid_term_memory_lenth 保留 MaiBot 原字段拼写。
type ChatConfig struct {
	EnableReplyAt             bool                 `toml:"enable_reply_at"`
	MaxContextSize            int                  `toml:"max_context_size"`
	MaxPrivateContextSize     int                  `toml:"max_private_context_size"`
	EnableContextOptimization bool                 `toml:"enable_context_optimization"`
	MidTermMemory             bool                 `toml:"mid_term_memory"`
	MidTermMemoryLenth        int                  `toml:"mid_term_memory_lenth"`
	ReplyTiming               ChatReplyTimingConfig `toml:"reply_timing"`
	ReplyStyle                ChatReplyStyleConfig  `toml:"reply_style"`
}

func defaultChatConfig() ChatConfig {
	return ChatConfig{
		EnableReplyAt:             true,
		MaxContextSize:            40,
		MaxPrivateContextSize:     60,
		EnableContextOptimization: true,
		MidTermMemory:             true,
		MidTermMemoryLenth:        10,
		ReplyTiming:               defaultChatReplyTimingConfig(),
		ReplyStyle:                defaultChatReplyStyleConfig(),
	}
}

// ---------------------------------------------------------------------------
// ExperimentalConfig — aligned with official_configs.ExperimentalConfig
// ---------------------------------------------------------------------------

// AttentionDriftConfig 对齐 official_configs.AttentionDriftConfig（注意力漂移）。
type AttentionDriftConfig struct {
	Enabled       bool   `toml:"enabled"`
	DriftLevel    string `toml:"drift_level"`    // subtle|active|scattered|wild
	AnchorPolicy  string `toml:"anchor_policy"`  // strict|balanced|loose
	ReactionStyle string `toml:"reaction_style"` // reserved|natural|lively
}

func defaultAttentionDriftConfig() AttentionDriftConfig {
	return AttentionDriftConfig{
		Enabled:       false,
		DriftLevel:    "scattered",
		AnchorPolicy:  "balanced",
		ReactionStyle: "lively",
	}
}

// ExperimentalConfig 对齐 official_configs.ExperimentalConfig（实验性功能，默认全关）。
type ExperimentalConfig struct {
	EnableBehaviorLearning bool                  `toml:"enable_behavior_learning"`
	ReplyerRetroPrompt     bool                  `toml:"replyer_retro_prompt"`
	EmotionTrait           string                `toml:"emotion_trait"` // rational_calm|neutral|sentimental
	AttentionDrift         AttentionDriftConfig  `toml:"attention_drift"`
	BehaviorLearningList   []LearningItem        `toml:"behavior_learning_list"`
	BehaviorGroups         []ChatStreamGroup     `toml:"behavior_groups"`
	FocusMode              bool                  `toml:"focus_mode"`
	FocusOnPrivate         bool                  `toml:"focus_on_private"`
	FocusChatWhitelist     []TargetItem          `toml:"focus_chat_whitelist"`
	FocusGroups            []ChatStreamGroup     `toml:"focus_groups"`
	FocusCoolTime          int                   `toml:"focus_cool_time"`
}

func defaultExperimentalConfig() ExperimentalConfig {
	return ExperimentalConfig{
		EnableBehaviorLearning: false,
		ReplyerRetroPrompt:     false,
		EmotionTrait:           "neutral",
		AttentionDrift:         defaultAttentionDriftConfig(),
		BehaviorLearningList:   defaultLearningList(),
		BehaviorGroups:         []ChatStreamGroup{},
		FocusMode:              false,
		FocusOnPrivate:         false,
		FocusChatWhitelist:     []TargetItem{},
		FocusGroups:            []ChatStreamGroup{},
		FocusCoolTime:          120,
	}
}

// ---------------------------------------------------------------------------
// VisualConfig — aligned with official_configs.VisualConfig + ImageCacheCleanupConfig
// ---------------------------------------------------------------------------

// ImageCacheCleanupConfig 对齐 official_configs.ImageCacheCleanupConfig。
type ImageCacheCleanupConfig struct {
	Enabled                 bool    `toml:"enabled"`
	CheckIntervalHours      float64 `toml:"check_interval_hours"`
	ImageFileRetentionDays  int     `toml:"image_file_retention_days"`
	NoFileResultRetentionDays int   `toml:"no_file_result_retention_days"`
}

func defaultImageCacheCleanupConfig() ImageCacheCleanupConfig {
	return ImageCacheCleanupConfig{
		Enabled:                   true,
		CheckIntervalHours:        6.0,
		ImageFileRetentionDays:    14,
		NoFileResultRetentionDays: 30,
	}
}

// VisualConfig 对齐 official_configs.VisualConfig（视觉配置类）。
type VisualConfig struct {
	MaxImageNum                int                    `toml:"max_image_num"`
	WaitImageRecognizeMaxTime  float64                `toml:"wait_image_recognize_max_time"`
	HandleOversizedImages      bool                   `toml:"handle_oversized_images"`
	MaxImageSizeMB             float64                `toml:"max_image_size_mb"`
	OversizedImageHandleMethod string                 `toml:"oversized_image_handle_method"` // compress|discard
	ImageCacheCleanup          ImageCacheCleanupConfig `toml:"image_cache_cleanup"`
}

func defaultVisualConfig() VisualConfig {
	return VisualConfig{
		MaxImageNum:                64,
		WaitImageRecognizeMaxTime:  32,
		HandleOversizedImages:      true,
		MaxImageSizeMB:             16.0,
		OversizedImageHandleMethod: "compress",
		ImageCacheCleanup:          defaultImageCacheCleanupConfig(),
	}
}

// ---------------------------------------------------------------------------
// ExpressionConfig / JargonConfig — aligned with official_configs
// ---------------------------------------------------------------------------

// ExpressionConfig 对齐 official_configs.ExpressionConfig（表达配置类）。
type ExpressionConfig struct {
	ExpressionCheckedOnly           bool              `toml:"expression_checked_only"`
	ExpressionSelfReflect           bool              `toml:"expression_self_reflect"`
	UseVectorExpression             bool              `toml:"use_vector_expression"`
	ExpressionVectorIndexPath       string            `toml:"expression_vector_index_path"`
	ExpressionVectorCandidatePoolSize int             `toml:"expression_vector_candidate_pool_size"`
	MaxExpressionLearner            int               `toml:"max_expression_learner"`
	LearningList                    []LearningItem    `toml:"learning_list"`
	ExpressionGroups                []ChatStreamGroup `toml:"expression_groups"`
}

func defaultExpressionConfig() ExpressionConfig {
	return ExpressionConfig{
		ExpressionCheckedOnly:             false,
		ExpressionSelfReflect:             true,
		UseVectorExpression:               true,
		ExpressionVectorIndexPath:         "data/expression_selection/expression_vector_index.json",
		ExpressionVectorCandidatePoolSize: 50,
		MaxExpressionLearner:              3,
		LearningList:                      defaultLearningList(),
		ExpressionGroups:                  []ChatStreamGroup{},
	}
}

// JargonConfig 对齐 official_configs.JargonConfig（黑话配置类）。
type JargonConfig struct {
	LearningList []LearningItem    `toml:"learning_list"`
	JargonGroups []ChatStreamGroup `toml:"jargon_groups"`
}

func defaultJargonConfig() JargonConfig {
	return JargonConfig{
		LearningList: defaultLearningList(),
		JargonGroups: []ChatStreamGroup{},
	}
}

// ---------------------------------------------------------------------------
// MessageReceiveConfig — aligned with official_configs.MessageReceiveConfig
// ---------------------------------------------------------------------------

// MessageReceiveConfig 对齐 official_configs.MessageReceiveConfig（消息接收）。
type MessageReceiveConfig struct {
	ImageParseThreshold int      `toml:"image_parse_threshold"`
	BanWords            []string `toml:"ban_words"`      // Python set[str]
	BanMsgsRegex        []string `toml:"ban_msgs_regex"` // Python set[str]
}

func defaultMessageReceiveConfig() MessageReceiveConfig {
	return MessageReceiveConfig{
		ImageParseThreshold: 5,
		BanWords:            []string{},
		BanMsgsRegex:        []string{},
	}
}

// ---------------------------------------------------------------------------
// EmojiConfig — aligned with official_configs.EmojiConfig + EmojiCacheCleanupConfig
// ---------------------------------------------------------------------------

// EmojiCacheCleanupConfig 对齐 official_configs.EmojiCacheCleanupConfig。
type EmojiCacheCleanupConfig struct {
	Enabled                   bool    `toml:"enabled"`
	CheckIntervalHours        float64 `toml:"check_interval_hours"`
	EmojiFileRetentionDays    int     `toml:"emoji_file_retention_days"`
	NoFileRecordRetentionDays int     `toml:"no_file_record_retention_days"`
}

func defaultEmojiCacheCleanupConfig() EmojiCacheCleanupConfig {
	return EmojiCacheCleanupConfig{
		Enabled:                   true,
		CheckIntervalHours:        6.0,
		EmojiFileRetentionDays:    30,
		NoFileRecordRetentionDays: 30,
	}
}

// EmojiConfig 对齐 official_configs.EmojiConfig（表情包配置类）。
type EmojiConfig struct {
	UseNewSendLogic  bool                    `toml:"use_new_send_logic"`
	EmojiSendNum     int                     `toml:"emoji_send_num"`
	MaxRegNum        int                     `toml:"max_reg_num"`
	DoReplace        bool                    `toml:"do_replace"`
	CheckInterval    int                     `toml:"check_interval"`
	StealEmoji       bool                    `toml:"steal_emoji"`
	MaxEmojiSizeMB   float64                 `toml:"max_emoji_size_mb"`
	ContentFiltration bool                   `toml:"content_filtration"`
	CacheCleanup     EmojiCacheCleanupConfig `toml:"cache_cleanup"`
}

func defaultEmojiConfig() EmojiConfig {
	return EmojiConfig{
		UseNewSendLogic:   false,
		EmojiSendNum:      25,
		MaxRegNum:         64,
		DoReplace:         true,
		CheckInterval:     10,
		StealEmoji:        true,
		MaxEmojiSizeMB:    5.0,
		ContentFiltration: false,
		CacheCleanup:      defaultEmojiCacheCleanupConfig(),
	}
}

// ---------------------------------------------------------------------------
// KeywordReaction — aligned with official_configs.KeywordRuleConfig/KeywordReactionConfig
// ---------------------------------------------------------------------------

// KeywordRuleConfig 对齐 official_configs.KeywordRuleConfig。
type KeywordRuleConfig struct {
	Keywords []string `toml:"keywords"`
	Regex    []string `toml:"regex"`
	Reaction string   `toml:"reaction"`
}

// KeywordReactionConfig 对齐 official_configs.KeywordReactionConfig。
type KeywordReactionConfig struct {
	KeywordRules []KeywordRuleConfig `toml:"keyword_rules"`
	RegexRules   []KeywordRuleConfig `toml:"regex_rules"`
}

func defaultKeywordReactionConfig() KeywordReactionConfig {
	return KeywordReactionConfig{
		KeywordRules: []KeywordRuleConfig{},
		RegexRules:   []KeywordRuleConfig{},
	}
}

// ---------------------------------------------------------------------------
// ResponsePostProcess / ChineseTypo / ResponseSplitter — aligned with official_configs
// ---------------------------------------------------------------------------

// ResponsePostProcessConfig 对齐 official_configs.ResponsePostProcessConfig。
type ResponsePostProcessConfig struct {
	EnableResponsePostProcess bool    `toml:"enable_response_post_process"`
	TypingSpeed               float64 `toml:"typing_speed"`
}

func defaultResponsePostProcessConfig() ResponsePostProcessConfig {
	return ResponsePostProcessConfig{EnableResponsePostProcess: true, TypingSpeed: 1.0}
}

// ChineseTypoConfig 对齐 official_configs.ChineseTypoConfig。
type ChineseTypoConfig struct {
	Enable                   bool    `toml:"enable"`
	EnableCorrectionQuote    bool    `toml:"enable_correction_quote"`
	CorrectionQuoteProbability float64 `toml:"correction_quote_probability"`
	ErrorRate                float64 `toml:"error_rate"`
	MinFreq                  int     `toml:"min_freq"`
	ToneErrorRate            float64 `toml:"tone_error_rate"`
	WordReplaceRate          float64 `toml:"word_replace_rate"`
}

func defaultChineseTypoConfig() ChineseTypoConfig {
	return ChineseTypoConfig{
		Enable:                     true,
		EnableCorrectionQuote:      true,
		CorrectionQuoteProbability: 1.0,
		ErrorRate:                  0.01,
		MinFreq:                    9,
		ToneErrorRate:              0.1,
		WordReplaceRate:            0.006,
	}
}

// ResponseSplitterConfig 对齐 official_configs.ResponseSplitterConfig。
type ResponseSplitterConfig struct {
	Mode                     string `toml:"mode"` // rule|llm
	Enable                   bool   `toml:"enable"`
	MaxLength                int    `toml:"max_length"`
	MaxSentenceNum           int    `toml:"max_sentence_num"`
	MaxSplitNum              int    `toml:"max_split_num"`
	EnableKaomojiProtection  bool   `toml:"enable_kaomoji_protection"`
	EnableOverflowReturnAll  bool   `toml:"enable_overflow_return_all"`
}

func defaultResponseSplitterConfig() ResponseSplitterConfig {
	return ResponseSplitterConfig{
		Mode:                    "rule",
		Enable:                  true,
		MaxLength:               512,
		MaxSentenceNum:          8,
		MaxSplitNum:             3,
		EnableKaomojiProtection: false,
		EnableOverflowReturnAll: false,
	}
}

// ---------------------------------------------------------------------------
// A_memorix — aligned with official_configs.AMemorixConfig（裁剪 web_import/bundle 面）
// ---------------------------------------------------------------------------

// AMemorixPluginConfig 对齐 AMemorixPluginConfig（总开关）。
type AMemorixPluginConfig struct {
	Enabled bool `toml:"enabled"`
}

// AMemorixStorageConfig 对齐 AMemorixStorageConfig。
type AMemorixStorageConfig struct {
	DataDir string `toml:"data_dir"`
}

// AMemorixImageMemoryConfig 对齐 AMemorixImageMemoryConfig（核心字段）。
type AMemorixImageMemoryConfig struct {
	Enabled              bool    `toml:"enabled"`
	TaskName             string  `toml:"task_name"`
	PreprocessVersion    string  `toml:"preprocess_version"`
	ProbeRetrySeconds    float64 `toml:"probe_retry_seconds"`
	MaxBytes             int     `toml:"max_bytes"`
	MaxPixels            int     `toml:"max_pixels"`
	CandidateLimit       int     `toml:"candidate_limit"`
	SimilarityThreshold  float64 `toml:"similarity_threshold"`
	JobPollIntervalSeconds float64 `toml:"job_poll_interval_seconds"`
	JobBatchSize         int     `toml:"job_batch_size"`
	JobLeaseSeconds      float64 `toml:"job_lease_seconds"`
	JobMaxRetries        int     `toml:"job_max_retries"`
	MinTrainThreshold    int     `toml:"min_train_threshold"`
}

// AMemorixEmbeddingFallbackConfig 对齐 AMemorixEmbeddingFallbackConfig。
type AMemorixEmbeddingFallbackConfig struct {
	Enabled               bool  `toml:"enabled"`
	ProbeIntervalSeconds  int   `toml:"probe_interval_seconds"`
	AllowMetadataOnlyWrite bool `toml:"allow_metadata_only_write"`
}

// AMemorixParagraphVectorBackfillConfig 对齐 AMemorixParagraphVectorBackfillConfig。
type AMemorixParagraphVectorBackfillConfig struct {
	Enabled bool `toml:"enabled"`
}

// AMemorixEmbeddingConfig 对齐 AMemorixEmbeddingConfig。
type AMemorixEmbeddingConfig struct {
	ModelName              string                                  `toml:"model_name"`
	Dimension              int                                     `toml:"dimension"`
	DimensionRequestMode   string                                  `toml:"dimension_request_mode"` // explicit|always|never
	BatchSize              int                                     `toml:"batch_size"`
	MaxConcurrent          int                                     `toml:"max_concurrent"`
	EnableCache            bool                                    `toml:"enable_cache"`
	RuntimeTrainThreshold  int                                     `toml:"runtime_train_threshold"`
	QuantizationType       string                                  `toml:"quantization_type"`
	Fallback               AMemorixEmbeddingFallbackConfig         `toml:"fallback"`
	ParagraphVectorBackfill AMemorixParagraphVectorBackfillConfig  `toml:"paragraph_vector_backfill"`
}

// AMemorixSparseRetrievalConfig 对齐 AMemorixSparseRetrievalConfig。
type AMemorixSparseRetrievalConfig struct {
	Enabled            bool   `toml:"enabled"`
	Backend            string `toml:"backend"` // fts5
	Mode               string `toml:"mode"`    // auto|fallback_only|hybrid
	TokenizerMode      string `toml:"tokenizer_mode"`
	CandidateK         int    `toml:"candidate_k"`
	RelationCandidateK int    `toml:"relation_candidate_k"`
}

// AMemorixSmartFallbackConfig 对齐 AMemorixSmartFallbackConfig。
type AMemorixSmartFallbackConfig struct {
	Enabled bool `toml:"enabled"`
}

// AMemorixRetrievalSearchConfig 对齐 AMemorixRetrievalSearchConfig。
type AMemorixRetrievalSearchConfig struct {
	SmartFallback AMemorixSmartFallbackConfig `toml:"smart_fallback"`
}

// AMemorixFusionRetrievalConfig 对齐 AMemorixFusionRetrievalConfig。
type AMemorixFusionRetrievalConfig struct {
	Method       string  `toml:"method"` // weighted_rrf|alpha_legacy
	RRFK         int     `toml:"rrf_k"`
	VectorWeight float64 `toml:"vector_weight"`
	BM25Weight   float64 `toml:"bm25_weight"`
}

// AMemorixRelationVectorizationConfig 对齐 AMemorixRelationVectorizationConfig。
type AMemorixRelationVectorizationConfig struct {
	Enabled         bool `toml:"enabled"`
	BackfillEnabled bool `toml:"backfill_enabled"`
	WriteOnImport   bool `toml:"write_on_import"`
}

// AMemorixRelationIntentVectorPoolConfig 对齐 AMemorixRelationIntentVectorPoolConfig。
type AMemorixRelationIntentVectorPoolConfig struct {
	GraphTopK           int     `toml:"graph_top_k"`
	SemanticWeight      float64 `toml:"semantic_weight"`
	SparseWeight        float64 `toml:"sparse_weight"`
	GraphWeight         float64 `toml:"graph_weight"`
	ReturnRelationItems bool    `toml:"return_relation_items"`
}

// AMemorixVectorPoolsConfig 对齐 AMemorixVectorPoolsConfig。
type AMemorixVectorPoolsConfig struct {
	Mode                   string                                 `toml:"mode"` // single|dual
	ParagraphTopK          int                                    `toml:"paragraph_top_k"`
	GraphTopK              int                                    `toml:"graph_top_k"`
	GraphExpandParagraphK  int                                    `toml:"graph_expand_paragraph_k"`
	RelationExpandPerHit   int                                    `toml:"relation_expand_per_hit"`
	EntityExpandPerHit     int                                    `toml:"entity_expand_per_hit"`
	RelationEvidenceWeight float64                                `toml:"relation_evidence_weight"`
	EntityEvidenceWeight   float64                                `toml:"entity_evidence_weight"`
	SemanticWeight         float64                                `toml:"semantic_weight"`
	SparseWeight           float64                                `toml:"sparse_weight"`
	GraphWeight            float64                                `toml:"graph_weight"`
	RelationIntent         AMemorixRelationIntentVectorPoolConfig `toml:"relation_intent"`
}

// AMemorixRetrievalConfig 对齐 AMemorixRetrievalConfig。
type AMemorixRetrievalConfig struct {
	TopKParagraphs      int                                 `toml:"top_k_paragraphs"`
	TopKRelations       int                                 `toml:"top_k_relations"`
	TopKFinal           int                                 `toml:"top_k_final"`
	Alpha               float64                             `toml:"alpha"`
	EnablePPR           bool                                `toml:"enable_ppr"`
	PPRAlpha            float64                             `toml:"ppr_alpha"`
	PPRTimeoutSeconds   float64                             `toml:"ppr_timeout_seconds"`
	PPRConcurrencyLimit int                                 `toml:"ppr_concurrency_limit"`
	EnableParallel      bool                                `toml:"enable_parallel"`
	Search              AMemorixRetrievalSearchConfig       `toml:"search"`
	Fusion              AMemorixFusionRetrievalConfig       `toml:"fusion"`
	RelationVectorization AMemorixRelationVectorizationConfig `toml:"relation_vectorization"`
	VectorPools         AMemorixVectorPoolsConfig           `toml:"vector_pools"`
	Sparse              AMemorixSparseRetrievalConfig       `toml:"sparse"`
}

// AMemorixThresholdConfig 对齐 AMemorixThresholdConfig。
type AMemorixThresholdConfig struct {
	MinThreshold float64 `toml:"min_threshold"`
	MaxThreshold float64 `toml:"max_threshold"`
	Percentile   int     `toml:"percentile"`
	MinResults   int     `toml:"min_results"`
}

// AMemorixRetrievalSubtypeFilterConfig 对齐 AMemorixRetrievalSubtypeFilterConfig。
type AMemorixRetrievalSubtypeFilterConfig struct {
	Enabled bool     `toml:"enabled"`
	Mode    string   `toml:"mode"` // blacklist|whitelist
	Chats   []string `toml:"chats"`
}

// AMemorixRetrievalFilterConfig 对齐 AMemorixRetrievalFilterConfig。
type AMemorixRetrievalFilterConfig struct {
	ChatStream  AMemorixRetrievalSubtypeFilterConfig `toml:"chat_stream"`
	ChatSummary AMemorixRetrievalSubtypeFilterConfig `toml:"chat_summary"`
	Episode     AMemorixRetrievalSubtypeFilterConfig `toml:"episode"`
}

// AMemorixFilterConfig 对齐 AMemorixFilterConfig。
type AMemorixFilterConfig struct {
	Enabled   bool                         `toml:"enabled"`
	Mode      string                       `toml:"mode"` // blacklist|whitelist
	Chats     []string                     `toml:"chats"`
	Retrieval AMemorixRetrievalFilterConfig `toml:"retrieval"`
}

// AMemorixEpisodeConfig 对齐 AMemorixEpisodeConfig。
type AMemorixEpisodeConfig struct {
	Enabled                  bool     `toml:"enabled"`
	GenerationEnabled        bool     `toml:"generation_enabled"`
	SourcePollIntervalSeconds float64 `toml:"source_poll_interval_seconds"`
	SourceBatchSize          int      `toml:"source_batch_size"`
	SourceMaxRetry           int      `toml:"source_max_retry"`
	SourceLeaseSeconds       float64  `toml:"source_lease_seconds"`
	SourceMaxWaitSeconds     float64  `toml:"source_max_wait_seconds"`
	MaxParagraphsPerCall     int      `toml:"max_paragraphs_per_call"`
	MaxCharsPerCall          int      `toml:"max_chars_per_call"`
	SourceTimeWindowHours    float64  `toml:"source_time_window_hours"`
	SegmentationModel        string   `toml:"segmentation_model"`
	DisabledSourceTypes      []string `toml:"disabled_source_types"`
}

// AMemorixPersonProfileConfig 对齐 AMemorixPersonProfileConfig。
type AMemorixPersonProfileConfig struct {
	Enabled                    bool `toml:"enabled"`
	RefreshIntervalMinutes     int  `toml:"refresh_interval_minutes"`
	ActiveWindowHours          float64 `toml:"active_window_hours"`
	MaxRefreshPerCycle         int  `toml:"max_refresh_per_cycle"`
	RefreshDebounceSeconds     int  `toml:"refresh_debounce_seconds"`
	RefreshQueueIntervalSeconds int `toml:"refresh_queue_interval_seconds"`
	RefreshQueueBatchSize      int  `toml:"refresh_queue_batch_size"`
	RefreshRetryBackoffSeconds int  `toml:"refresh_retry_backoff_seconds"`
	MaxRetry                   int  `toml:"max_retry"`
	TopKEvidence               int  `toml:"top_k_evidence"`
	EvidenceClassificationMaxTokens int `toml:"evidence_classification_max_tokens"`
}

// AMemorixMemoryEvolutionConfig 对齐 AMemorixMemoryEvolutionConfig。
type AMemorixMemoryEvolutionConfig struct {
	Enabled                    bool    `toml:"enabled"`
	HalfLifeHours              float64 `toml:"half_life_hours"`
	PruneThreshold             float64 `toml:"prune_threshold"`
	FreezeDurationHours        float64 `toml:"freeze_duration_hours"`
	ReviveThreshold            float64 `toml:"revive_threshold"`
	AccessReinforcementAlpha   float64 `toml:"access_reinforcement_alpha"`
	AccessReinforcementCooldownMinutes float64 `toml:"access_reinforcement_cooldown_minutes"`
	ExplicitReinforcementAlpha float64 `toml:"explicit_reinforcement_alpha"`
	WeakenAlpha                float64 `toml:"weaken_alpha"`
	LifecycleBatchSize         int     `toml:"lifecycle_batch_size"`
}

// AMemorixAdvancedConfig 对齐 AMemorixAdvancedConfig。
type AMemorixAdvancedConfig struct {
	EnableAutoSave        bool `toml:"enable_auto_save"`
	AutoSaveIntervalMinutes int `toml:"auto_save_interval_minutes"`
	Debug                 bool `toml:"debug"`
}

// AMemorixConfig 对齐 official_configs.AMemorixConfig（核心面；web/bundle 次要面裁剪）。
type AMemorixConfig struct {
	Plugin     AMemorixPluginConfig          `toml:"plugin"`
	Integration AMemorixIntegrationConfig    `toml:"integration"`
	Storage    AMemorixStorageConfig         `toml:"storage"`
	ImageMemory AMemorixImageMemoryConfig    `toml:"image_memory"`
	Embedding  AMemorixEmbeddingConfig       `toml:"embedding"`
	Retrieval  AMemorixRetrievalConfig       `toml:"retrieval"`
	Threshold  AMemorixThresholdConfig       `toml:"threshold"`
	Filter     AMemorixFilterConfig          `toml:"filter"`

	GlobalMemorySharingEnabled bool              `toml:"global_memory_sharing_enabled"`
	SharedMemoryGroups         []ChatStreamGroup `toml:"shared_memory_groups"`

	Episode      AMemorixEpisodeConfig        `toml:"episode"`
	PersonProfile AMemorixPersonProfileConfig `toml:"person_profile"`
	Memory       AMemorixMemoryEvolutionConfig `toml:"memory"`
	Advanced     AMemorixAdvancedConfig       `toml:"advanced"`
}

// AMemorixIntegrationConfig 对齐 AMemorixIntegrationConfig（聊天中使用记忆）。
type AMemorixIntegrationConfig struct {
	EnableMemoryQueryTool              bool `toml:"enable_memory_query_tool"`
	MemoryQueryDefaultLimit            int  `toml:"memory_query_default_limit"`
	EnablePersonProfileQueryTool       bool `toml:"enable_person_profile_query_tool"`
	EnablePersonProfileInjection       bool `toml:"enable_person_profile_injection"`
	PersonProfileInjectionMaxProfiles  int  `toml:"person_profile_injection_max_profiles"`
	HeuristicMemoryRecallEnabled       bool `toml:"heuristic_memory_recall_enabled"`
	HeuristicMemoryCrossChatEnabled    bool `toml:"heuristic_memory_cross_chat_enabled"`
	HeuristicMemoryRecallWindowSize    int  `toml:"heuristic_memory_recall_window_size"`
	HeuristicMemoryRecallLimit         int  `toml:"heuristic_memory_recall_limit"`
	HeuristicMemoryRecallMaxChars      int  `toml:"heuristic_memory_recall_max_chars"`
	HeuristicMemoryRecallMinIntervalSeconds int `toml:"heuristic_memory_recall_min_interval_seconds"`
	HeuristicMemoryRecallMinNewMessages     int `toml:"heuristic_memory_recall_min_new_messages"`
	HeuristicMemoryRecallCacheTTLSeconds    int `toml:"heuristic_memory_recall_cache_ttl_seconds"`
	HeuristicMemoryGroupToPrivateEnabled    bool `toml:"heuristic_memory_group_to_private_enabled"`
	HeuristicMemoryPrivateToGroupEnabled    bool `toml:"heuristic_memory_private_to_group_enabled"`
	PersonFactWritebackEnabled        bool `toml:"person_fact_writeback_enabled"`
	ChatSummaryWritebackEnabled       bool `toml:"chat_summary_writeback_enabled"`
	ChatSummaryWritebackMessageThreshold int `toml:"chat_summary_writeback_message_threshold"`
	ChatSummaryWritebackContextLength int  `toml:"chat_summary_writeback_context_length"`
	FuzzyModifyEnabled                bool `toml:"fuzzy_modify_enabled"`
	FuzzyModifyAutoExecuteEnabled     bool `toml:"fuzzy_modify_auto_execute_enabled"`
	FuzzyModifyConfirmThreshold       float64 `toml:"fuzzy_modify_confirm_threshold"`
	FuzzyModifyCandidateLimit         int  `toml:"fuzzy_modify_candidate_limit"`
	FuzzyModifyMaxTargets             int  `toml:"fuzzy_modify_max_targets"`
	FuzzyModifyAllowGlobalScope       bool `toml:"fuzzy_modify_allow_global_scope"`
	FeedbackCorrectionEnabled         bool `toml:"feedback_correction_enabled"`
	FeedbackCorrectionWindowHours     float64 `toml:"feedback_correction_window_hours"`
	FeedbackCorrectionCheckIntervalMinutes int `toml:"feedback_correction_check_interval_minutes"`
	FeedbackCorrectionBatchSize       int  `toml:"feedback_correction_batch_size"`
	FeedbackCorrectionAutoApplyThreshold float64 `toml:"feedback_correction_auto_apply_threshold"`
	FeedbackCorrectionMaxFeedbackMessages int  `toml:"feedback_correction_max_feedback_messages"`
	FeedbackCorrectionPrefilterEnabled bool `toml:"feedback_correction_prefilter_enabled"`
	FeedbackCorrectionParagraphMarkEnabled bool `toml:"feedback_correction_paragraph_mark_enabled"`
	FeedbackCorrectionParagraphHardFilterEnabled bool `toml:"feedback_correction_paragraph_hard_filter_enabled"`
	FeedbackCorrectionProfileRefreshEnabled bool `toml:"feedback_correction_profile_refresh_enabled"`
	FeedbackCorrectionProfileForceRefreshOnRead bool `toml:"feedback_correction_profile_force_refresh_on_read"`
	FeedbackCorrectionEpisodeRebuildEnabled bool `toml:"feedback_correction_episode_rebuild_enabled"`
}

func defaultAMemorixConfig() AMemorixConfig {
	return AMemorixConfig{
		Plugin:     AMemorixPluginConfig{Enabled: false},
		Integration: defaultAMemorixIntegrationConfig(),
		Storage:    AMemorixStorageConfig{DataDir: "data/a-memorix"},
		ImageMemory: AMemorixImageMemoryConfig{
			Enabled:                true,
			TaskName:               "image_embedding",
			PreprocessVersion:      "identity_v1",
			ProbeRetrySeconds:      60.0,
			MaxBytes:               10 * 1024 * 1024,
			MaxPixels:              40_000_000,
			CandidateLimit:         8,
			SimilarityThreshold:    0.72,
			JobPollIntervalSeconds: 2.0,
			JobBatchSize:           4,
			JobLeaseSeconds:        120.0,
			JobMaxRetries:          5,
			MinTrainThreshold:      40,
		},
		Embedding: AMemorixEmbeddingConfig{
			ModelName:             "auto",
			Dimension:             1024,
			DimensionRequestMode:  "explicit",
			BatchSize:             32,
			MaxConcurrent:         5,
			EnableCache:           false,
			RuntimeTrainThreshold: 256,
			QuantizationType:      "int8",
			Fallback: AMemorixEmbeddingFallbackConfig{
				Enabled:               true,
				ProbeIntervalSeconds:  180,
				AllowMetadataOnlyWrite: true,
			},
			ParagraphVectorBackfill: AMemorixParagraphVectorBackfillConfig{Enabled: true},
		},
		Retrieval: AMemorixRetrievalConfig{
			TopKParagraphs:      20,
			TopKRelations:       10,
			TopKFinal:           10,
			Alpha:               0.5,
			EnablePPR:           true,
			PPRAlpha:            0.85,
			PPRTimeoutSeconds:   1.5,
			PPRConcurrencyLimit: 4,
			EnableParallel:      true,
			Search:              AMemorixRetrievalSearchConfig{SmartFallback: AMemorixSmartFallbackConfig{Enabled: true}},
			Fusion: AMemorixFusionRetrievalConfig{
				Method:       "weighted_rrf",
				RRFK:         60,
				VectorWeight: 0.7,
				BM25Weight:   0.3,
			},
			RelationVectorization: AMemorixRelationVectorizationConfig{Enabled: false, BackfillEnabled: false, WriteOnImport: true},
			VectorPools: AMemorixVectorPoolsConfig{
				Mode:                   "dual",
				ParagraphTopK:          20,
				GraphTopK:              40,
				GraphExpandParagraphK:  80,
				RelationExpandPerHit:   5,
				EntityExpandPerHit:     8,
				RelationEvidenceWeight: 1.0,
				EntityEvidenceWeight:   0.55,
				SemanticWeight:         0.65,
				SparseWeight:           0.20,
				GraphWeight:            0.15,
				RelationIntent: AMemorixRelationIntentVectorPoolConfig{
					GraphTopK:           80,
					SemanticWeight:      0.45,
					SparseWeight:        0.15,
					GraphWeight:         0.40,
					ReturnRelationItems: false,
				},
			},
			Sparse: AMemorixSparseRetrievalConfig{
				Enabled:            true,
				Backend:            "fts5",
				Mode:               "auto",
				TokenizerMode:      "jieba",
				CandidateK:         80,
				RelationCandidateK: 60,
			},
		},
		Threshold: AMemorixThresholdConfig{MinThreshold: 0.29, MaxThreshold: 0.95, Percentile: 75, MinResults: 4},
		Filter: AMemorixFilterConfig{
			Enabled: true,
			Mode:    "blacklist",
			Chats:   []string{},
			Retrieval: AMemorixRetrievalFilterConfig{
				ChatStream:  AMemorixRetrievalSubtypeFilterConfig{Enabled: false, Mode: "blacklist", Chats: []string{}},
				ChatSummary: AMemorixRetrievalSubtypeFilterConfig{Enabled: false, Mode: "blacklist", Chats: []string{}},
				Episode:     AMemorixRetrievalSubtypeFilterConfig{Enabled: false, Mode: "blacklist", Chats: []string{}},
			},
		},
		GlobalMemorySharingEnabled: false,
		SharedMemoryGroups:         []ChatStreamGroup{},
		Episode: AMemorixEpisodeConfig{
			Enabled:                   true,
			GenerationEnabled:         true,
			SourcePollIntervalSeconds: 1.0,
			SourceBatchSize:           20,
			SourceMaxRetry:            3,
			SourceLeaseSeconds:        1800.0,
			SourceMaxWaitSeconds:      60.0,
			MaxParagraphsPerCall:      20,
			MaxCharsPerCall:           6000,
			SourceTimeWindowHours:     24.0,
			SegmentationModel:         "auto",
			DisabledSourceTypes:       []string{"person_fact"},
		},
		PersonProfile: AMemorixPersonProfileConfig{
			Enabled:                          true,
			RefreshIntervalMinutes:           30,
			ActiveWindowHours:                72.0,
			MaxRefreshPerCycle:               50,
			RefreshDebounceSeconds:           120,
			RefreshQueueIntervalSeconds:      60,
			RefreshQueueBatchSize:            10,
			RefreshRetryBackoffSeconds:       300,
			MaxRetry:                         3,
			TopKEvidence:                     12,
			EvidenceClassificationMaxTokens:  1200,
		},
		Memory: AMemorixMemoryEvolutionConfig{
			Enabled:                            true,
			HalfLifeHours:                      24.0,
			PruneThreshold:                     0.1,
			FreezeDurationHours:                24.0,
			ReviveThreshold:                    0.15,
			AccessReinforcementAlpha:           0.05,
			AccessReinforcementCooldownMinutes: 60.0,
			ExplicitReinforcementAlpha:         0.5,
			WeakenAlpha:                        0.5,
			LifecycleBatchSize:                 1000,
		},
		Advanced: AMemorixAdvancedConfig{EnableAutoSave: true, AutoSaveIntervalMinutes: 5, Debug: false},
	}
}

func defaultAMemorixIntegrationConfig() AMemorixIntegrationConfig {
	return AMemorixIntegrationConfig{
		EnableMemoryQueryTool:              true,
		MemoryQueryDefaultLimit:            5,
		EnablePersonProfileQueryTool:       true,
		EnablePersonProfileInjection:       true,
		PersonProfileInjectionMaxProfiles:  3,
		HeuristicMemoryRecallEnabled:       false,
		HeuristicMemoryCrossChatEnabled:    false,
		HeuristicMemoryRecallWindowSize:    20,
		HeuristicMemoryRecallLimit:         3,
		HeuristicMemoryRecallMaxChars:      900,
		HeuristicMemoryRecallMinIntervalSeconds: 180,
		HeuristicMemoryRecallMinNewMessages:     60,
		HeuristicMemoryRecallCacheTTLSeconds:    300,
		HeuristicMemoryGroupToPrivateEnabled:    false,
		HeuristicMemoryPrivateToGroupEnabled:    false,
		PersonFactWritebackEnabled:         true,
		ChatSummaryWritebackEnabled:        true,
		ChatSummaryWritebackMessageThreshold: 36,
		ChatSummaryWritebackContextLength:  36,
		FuzzyModifyEnabled:                 true,
		FuzzyModifyAutoExecuteEnabled:      false,
		FuzzyModifyConfirmThreshold:        0.85,
		FuzzyModifyCandidateLimit:          20,
		FuzzyModifyMaxTargets:              5,
		FuzzyModifyAllowGlobalScope:        false,
		FeedbackCorrectionEnabled:          false,
		FeedbackCorrectionWindowHours:      12.0,
		FeedbackCorrectionCheckIntervalMinutes: 30,
		FeedbackCorrectionBatchSize:        20,
		FeedbackCorrectionAutoApplyThreshold: 0.85,
		FeedbackCorrectionMaxFeedbackMessages: 30,
		FeedbackCorrectionPrefilterEnabled: true,
		FeedbackCorrectionParagraphMarkEnabled: true,
		FeedbackCorrectionParagraphHardFilterEnabled: true,
		FeedbackCorrectionProfileRefreshEnabled: true,
		FeedbackCorrectionProfileForceRefreshOnRead: true,
		FeedbackCorrectionEpisodeRebuildEnabled: true,
	}
}

// ---------------------------------------------------------------------------
// MCP — aligned with official_configs.MCPConfig 及其子配置
// ---------------------------------------------------------------------------

// MCPAuthorizationConfig 对齐 MCPAuthorizationConfig。
type MCPAuthorizationConfig struct {
	Mode        string `toml:"mode"` // none|bearer
	BearerToken string `toml:"bearer_token"`
}

// MCPRootItemConfig 对齐 MCPRootItemConfig。
type MCPRootItemConfig struct {
	Enabled bool   `toml:"enabled"`
	URI     string `toml:"uri"`
	Name    string `toml:"name"`
}

// MCPRootsConfig 对齐 MCPRootsConfig。
type MCPRootsConfig struct {
	Enable bool               `toml:"enable"`
	Items  []MCPRootItemConfig `toml:"items"`
}

// MCPSamplingConfig 对齐 MCPSamplingConfig。
type MCPSamplingConfig struct {
	Enable               bool `toml:"enable"`
	TaskName             string `toml:"task_name"`
	IncludeContextSupport bool `toml:"include_context_support"`
	ToolSupport          bool `toml:"tool_support"`
}

// MCPElicitationConfig 对齐 MCPElicitationConfig。
type MCPElicitationConfig struct {
	Enable   bool `toml:"enable"`
	AllowForm bool `toml:"allow_form"`
	AllowURL bool `toml:"allow_url"`
}

// MCPClientConfig 对齐 MCPClientConfig。
type MCPClientConfig struct {
	ClientName    string               `toml:"client_name"`
	ClientVersion string               `toml:"client_version"`
	Roots         MCPRootsConfig       `toml:"roots"`
	Sampling      MCPSamplingConfig    `toml:"sampling"`
	Elicitation   MCPElicitationConfig `toml:"elicitation"`
}

// MCPServerItemConfig 对齐 MCPServerItemConfig。
type MCPServerItemConfig struct {
	Name              string                 `toml:"name"`
	Enabled           bool                   `toml:"enabled"`
	Transport         string                 `toml:"transport"` // stdio|streamable_http|sse
	Command           string                 `toml:"command"`
	Args              []string               `toml:"args"`
	Env               map[string]string      `toml:"env"`
	URL               string                 `toml:"url"`
	Headers           map[string]string      `toml:"headers"`
	HTTPTimeoutSeconds float64               `toml:"http_timeout_seconds"`
	ReadTimeoutSeconds float64               `toml:"read_timeout_seconds"`
	Authorization     MCPAuthorizationConfig `toml:"authorization"`
}

// MCPConfig 对齐 MCPConfig。
type MCPConfig struct {
	Enable  bool                 `toml:"enable"`
	Client  MCPClientConfig      `toml:"client"`
	Servers []MCPServerItemConfig `toml:"servers"`
}

func defaultMCPConfig() MCPConfig {
	return MCPConfig{
		Enable: true,
		Client: MCPClientConfig{
			ClientName:    "MaiBot",
			ClientVersion: "1.0.0",
			Roots:         MCPRootsConfig{Enable: false, Items: []MCPRootItemConfig{}},
			Sampling:      MCPSamplingConfig{Enable: false, TaskName: "planner", IncludeContextSupport: false, ToolSupport: false},
			Elicitation:   MCPElicitationConfig{Enable: false, AllowForm: true, AllowURL: false},
		},
		Servers: []MCPServerItemConfig{},
	}
}

// ---------------------------------------------------------------------------
// WebUI / Log / Debug — aligned with official_configs.WebUIConfig/LogConfig/DebugConfig
// ---------------------------------------------------------------------------

// WebUIConfig 对齐 WebUIConfig。
type WebUIConfig struct {
	Enabled     bool     `toml:"enabled"`
	Host        []string `toml:"host"`
	Port        int      `toml:"port"`
	Mode        string   `toml:"mode"`         // development|production
	WebuiStyle  int      `toml:"webui_style"`  // 0|1|2
}

func defaultWebUIConfig() WebUIConfig {
	return WebUIConfig{
		Enabled:    true,
		Host:       []string{"127.0.0.1", "::1"},
		Port:       8001,
		Mode:       "production",
		WebuiStyle: 1,
	}
}

// LogConfig 对齐 LogConfig（裁剪 TelemetryConfig；suppress_libraries/library_log_levels 保留）。
type LogConfig struct {
	DateStyle                  string            `toml:"date_style"`
	LogLevelStyle              string            `toml:"log_level_style"` // lite|compact|full
	ColorText                  string            `toml:"color_text"`      // none|title|full
	LogLevel                   string            `toml:"log_level"`
	ConsoleLogLevel            string            `toml:"console_log_level"`
	FileLogLevel               string            `toml:"file_log_level"`
	LogFileMaxBytes            int               `toml:"log_file_max_bytes"`
	MaxLogFiles                int               `toml:"max_log_files"`
	LogCleanupDays             int               `toml:"log_cleanup_days"`
	LLMRequestSnapshotLimit    int               `toml:"llm_request_snapshot_limit"`
	MaisakaPromptPreviewLimit  int               `toml:"maisaka_prompt_preview_limit"`
	MaisakaReplyEffectLimit    int               `toml:"maisaka_reply_effect_limit"`
	EventLoopWatchdogEnabled   bool              `toml:"event_loop_watchdog_enabled"`
	EventLoopWatchdogWarnSeconds float64         `toml:"event_loop_watchdog_warn_seconds"`
	SuppressLibraries          []string          `toml:"suppress_libraries"`
	LibraryLogLevels           map[string]string `toml:"library_log_levels"`
}

func defaultLogConfig() LogConfig {
	return LogConfig{
		DateStyle:                    "m-d H:i:s",
		LogLevelStyle:                "lite",
		ColorText:                    "full",
		LogLevel:                     "INFO",
		ConsoleLogLevel:              "INFO",
		FileLogLevel:                 "DEBUG",
		LogFileMaxBytes:              5 * 1024 * 1024,
		MaxLogFiles:                  30,
		LogCleanupDays:               30,
		LLMRequestSnapshotLimit:      128,
		MaisakaPromptPreviewLimit:    256,
		MaisakaReplyEffectLimit:      256,
		EventLoopWatchdogEnabled:     false,
		EventLoopWatchdogWarnSeconds: 0.5,
		SuppressLibraries: []string{
			"faiss", "httpx", "urllib3", "asyncio", "websockets", "httpcore",
			"requests", "sqlalchemy", "openai", "uvicorn", "jieba",
		},
		LibraryLogLevels: map[string]string{"aiohttp": "WARNING", "PIL": "WARNING"},
	}
}

// DebugConfig 对齐 DebugConfig（force_plugin_compatibility 因插件系统裁剪不迁移）。
type DebugConfig struct {
	EnableConsoleInput          bool `toml:"enable_console_input"`
	ShowMaisakaThinking         bool `toml:"show_maisaka_thinking"`
	EnableReplyEffectTracking   bool `toml:"enable_reply_effect_tracking"`
	KeepPromptPreviewJSONBase64 bool `toml:"keep_prompt_preview_json_base64"`
	RecordToolStructuredContent bool `toml:"record_tool_structured_content"`
	EnableLLMCacheStats         bool `toml:"enable_llm_cache_stats"`
}

func defaultDebugConfig() DebugConfig {
	return DebugConfig{
		EnableConsoleInput:          true,
		ShowMaisakaThinking:         true,
		EnableReplyEffectTracking:   false,
		KeepPromptPreviewJSONBase64: false,
		RecordToolStructuredContent: false,
		EnableLLMCacheStats:         false,
	}
}

// ---------------------------------------------------------------------------
// Config 总类 — aligned with config.py class Config（裁剪面：去掉插件/平台适配/遥测/maim_message）
// ---------------------------------------------------------------------------

// Config 对齐 config.py class Config。保留节：bot/personality/chat/experimental/visual/
// expression/jargon/a_memorix/message_receive/emoji/keyword_reaction/
// response_post_process/chinese_typo/response_splitter/log/debug/mcp/webui/database。
type Config struct {
	Bot                BotConfig                `toml:"bot"`
	Personality        PersonalityConfig        `toml:"personality"`
	Chat               ChatConfig               `toml:"chat"`
	Experimental       ExperimentalConfig       `toml:"experimental"`
	Visual             VisualConfig             `toml:"visual"`
	Expression         ExpressionConfig         `toml:"expression"`
	Jargon             JargonConfig             `toml:"jargon"`
	AMemorix           AMemorixConfig           `toml:"a_memorix"`
	MessageReceive     MessageReceiveConfig     `toml:"message_receive"`
	Emoji              EmojiConfig              `toml:"emoji"`
	KeywordReaction    KeywordReactionConfig    `toml:"keyword_reaction"`
	ResponsePostProcess ResponsePostProcessConfig `toml:"response_post_process"`
	ChineseTypo        ChineseTypoConfig        `toml:"chinese_typo"`
	ResponseSplitter   ResponseSplitterConfig   `toml:"response_splitter"`
	Log                LogConfig                `toml:"log"`
	Debug              DebugConfig              `toml:"debug"`
	MCP                MCPConfig                `toml:"mcp"`
	WebUI              WebUIConfig              `toml:"webui"`
	Database           DatabaseConfig           `toml:"database"`
}

// DatabaseConfig 对齐 official_configs.DatabaseConfig。
type DatabaseConfig struct {
	SaveBinaryData bool `toml:"save_binary_data"`
}

func defaultDatabaseConfig() DatabaseConfig {
	return DatabaseConfig{SaveBinaryData: false}
}

// DefaultConfig 返回全新默认 Config（对齐 config_class() 的 default_factory 语义）。
func DefaultConfig() *Config {
	return &Config{
		Bot:                 defaultBotConfig(),
		Personality:         defaultPersonalityConfig(),
		Chat:                defaultChatConfig(),
		Experimental:        defaultExperimentalConfig(),
		Visual:              defaultVisualConfig(),
		Expression:          defaultExpressionConfig(),
		Jargon:              defaultJargonConfig(),
		AMemorix:            defaultAMemorixConfig(),
		MessageReceive:      defaultMessageReceiveConfig(),
		Emoji:               defaultEmojiConfig(),
		KeywordReaction:     defaultKeywordReactionConfig(),
		ResponsePostProcess: defaultResponsePostProcessConfig(),
		ChineseTypo:         defaultChineseTypoConfig(),
		ResponseSplitter:    defaultResponseSplitterConfig(),
		Log:                 defaultLogConfig(),
		Debug:               defaultDebugConfig(),
		MCP:                 defaultMCPConfig(),
		WebUI:               defaultWebUIConfig(),
		Database:            defaultDatabaseConfig(),
	}
}
