package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDefaultBotConfigDefaults 校验默认值与 Python 版严格一致（对齐 official_configs.py Field 默认值）。
func TestDefaultBotConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"bot.nickname", cfg.Bot.Nickname, "麦麦"},
		{"personality.multiple_probability", cfg.Personality.MultipleProbability, float64(0)},
		{"chat.enable_reply_at", cfg.Chat.EnableReplyAt, true},
		{"chat.max_context_size", cfg.Chat.MaxContextSize, 40},
		{"chat.max_private_context_size", cfg.Chat.MaxPrivateContextSize, 60},
		{"chat.enable_context_optimization", cfg.Chat.EnableContextOptimization, true},
		{"chat.mid_term_memory", cfg.Chat.MidTermMemory, true},
		{"chat.mid_term_memory_lenth", cfg.Chat.MidTermMemoryLenth, 10},
		{"reply_timing.talk_value", cfg.Chat.ReplyTiming.TalkValue, float64(1)},
		{"reply_timing.private_talk_value", cfg.Chat.ReplyTiming.PrivateTalkValue, float64(1)},
		{"reply_timing.mentioned_bot_reply", cfg.Chat.ReplyTiming.MentionedBotReply, false},
		{"reply_timing.inevitable_at_reply", cfg.Chat.ReplyTiming.InevitableAtReply, true},
		{"reply_timing.reply_trigger_mode", cfg.Chat.ReplyTiming.ReplyTriggerMode, "frequency"},
		{"reply_timing.planner_interrupt", cfg.Chat.ReplyTiming.PlannerInterruptMaxConsecutiveCount, 0},
		{"reply_timing.max_consecutive_wait_count", cfg.Chat.ReplyTiming.MaxConsecutiveWaitCount, 3},
		{"reply_timing.backoff_base", cfg.Chat.ReplyTiming.NoActionBackoffBaseSeconds, float64(15)},
		{"reply_timing.backoff_cap", cfg.Chat.ReplyTiming.NoActionBackoffCapSeconds, float64(300)},
		{"reply_timing.backoff_start", cfg.Chat.ReplyTiming.NoActionBackoffStartCount, 2},
		{"reply_timing.backoff_bypass", cfg.Chat.ReplyTiming.NoActionBackoffBypassPendingCount, 6},
		{"reply_timing.enable_talk_value_rules", cfg.Chat.ReplyTiming.EnableTalkValueRules, false},
		{"reply_style.enable_reply_quote", cfg.Chat.ReplyStyle.EnableReplyQuote, true},
		{"visual.max_image_num", cfg.Visual.MaxImageNum, 64},
		{"visual.wait_image_recognize_max_time", cfg.Visual.WaitImageRecognizeMaxTime, float64(32)},
		{"visual.handle_oversized_images", cfg.Visual.HandleOversizedImages, true},
		{"visual.max_image_size_mb", cfg.Visual.MaxImageSizeMB, 16.0},
		{"visual.oversized_method", cfg.Visual.OversizedImageHandleMethod, "compress"},
		{"visual.cleanup.check_interval_hours", cfg.Visual.ImageCacheCleanup.CheckIntervalHours, 6.0},
		{"experimental.enable_behavior_learning", cfg.Experimental.EnableBehaviorLearning, false},
		{"experimental.replyer_retro_prompt", cfg.Experimental.ReplyerRetroPrompt, false},
		{"experimental.emotion_trait", cfg.Experimental.EmotionTrait, "neutral"},
		{"experimental.attention_drift.level", cfg.Experimental.AttentionDrift.DriftLevel, "scattered"},
		{"experimental.attention_drift.anchor", cfg.Experimental.AttentionDrift.AnchorPolicy, "balanced"},
		{"experimental.attention_drift.reaction", cfg.Experimental.AttentionDrift.ReactionStyle, "lively"},
		{"experimental.focus_cool_time", cfg.Experimental.FocusCoolTime, 120},
		{"expression.expression_self_reflect", cfg.Expression.ExpressionSelfReflect, true},
		{"expression.use_vector_expression", cfg.Expression.UseVectorExpression, true},
		{"expression.vector_index_path", cfg.Expression.ExpressionVectorIndexPath, "data/expression_selection/expression_vector_index.json"},
		{"expression.pool_size", cfg.Expression.ExpressionVectorCandidatePoolSize, 50},
		{"expression.max_learner", cfg.Expression.MaxExpressionLearner, 3},
		{"message_receive.image_parse_threshold", cfg.MessageReceive.ImageParseThreshold, 5},
		{"emoji.use_new_send_logic", cfg.Emoji.UseNewSendLogic, false},
		{"emoji.emoji_send_num", cfg.Emoji.EmojiSendNum, 25},
		{"emoji.max_reg_num", cfg.Emoji.MaxRegNum, 64},
		{"emoji.do_replace", cfg.Emoji.DoReplace, true},
		{"emoji.check_interval", cfg.Emoji.CheckInterval, 10},
		{"emoji.steal_emoji", cfg.Emoji.StealEmoji, true},
		{"emoji.max_emoji_size_mb", cfg.Emoji.MaxEmojiSizeMB, 5.0},
		{"emoji.content_filtration", cfg.Emoji.ContentFiltration, false},
		{"rpp.enable", cfg.ResponsePostProcess.EnableResponsePostProcess, true},
		{"rpp.typing_speed", cfg.ResponsePostProcess.TypingSpeed, 1.0},
		{"typo.enable", cfg.ChineseTypo.Enable, true},
		{"typo.enable_correction_quote", cfg.ChineseTypo.EnableCorrectionQuote, true},
		{"typo.correction_quote_probability", cfg.ChineseTypo.CorrectionQuoteProbability, 1.0},
		{"typo.error_rate", cfg.ChineseTypo.ErrorRate, 0.01},
		{"typo.min_freq", cfg.ChineseTypo.MinFreq, 9},
		{"typo.tone_error_rate", cfg.ChineseTypo.ToneErrorRate, 0.1},
		{"typo.word_replace_rate", cfg.ChineseTypo.WordReplaceRate, 0.006},
		{"splitter.mode", cfg.ResponseSplitter.Mode, "rule"},
		{"splitter.enable", cfg.ResponseSplitter.Enable, true},
		{"splitter.max_length", cfg.ResponseSplitter.MaxLength, 512},
		{"splitter.max_sentence_num", cfg.ResponseSplitter.MaxSentenceNum, 8},
		{"splitter.max_split_num", cfg.ResponseSplitter.MaxSplitNum, 3},
		{"splitter.kaomoji", cfg.ResponseSplitter.EnableKaomojiProtection, false},
		{"splitter.overflow", cfg.ResponseSplitter.EnableOverflowReturnAll, false},
		{"a_memorix.plugin.enabled", cfg.AMemorix.Plugin.Enabled, false},
		{"a_memorix.storage.data_dir", cfg.AMemorix.Storage.DataDir, "data/a-memorix"},
		{"a_memorix.embedding.model_name", cfg.AMemorix.Embedding.ModelName, "auto"},
		{"a_memorix.embedding.dimension", cfg.AMemorix.Embedding.Dimension, 1024},
		{"a_memorix.embedding.batch_size", cfg.AMemorix.Embedding.BatchSize, 32},
		{"a_memorix.embedding.quantization", cfg.AMemorix.Embedding.QuantizationType, "int8"},
		{"a_memorix.retrieval.top_k_paragraphs", cfg.AMemorix.Retrieval.TopKParagraphs, 20},
		{"a_memorix.retrieval.top_k_relations", cfg.AMemorix.Retrieval.TopKRelations, 10},
		{"a_memorix.retrieval.top_k_final", cfg.AMemorix.Retrieval.TopKFinal, 10},
		{"a_memorix.retrieval.alpha", cfg.AMemorix.Retrieval.Alpha, 0.5},
		{"a_memorix.retrieval.enable_ppr", cfg.AMemorix.Retrieval.EnablePPR, true},
		{"a_memorix.retrieval.ppr_alpha", cfg.AMemorix.Retrieval.PPRAlpha, 0.85},
		{"a_memorix.retrieval.ppr_timeout", cfg.AMemorix.Retrieval.PPRTimeoutSeconds, 1.5},
		{"a_memorix.retrieval.fusion.method", cfg.AMemorix.Retrieval.Fusion.Method, "weighted_rrf"},
		{"a_memorix.retrieval.fusion.rrf_k", cfg.AMemorix.Retrieval.Fusion.RRFK, 60},
		{"a_memorix.retrieval.fusion.vector_weight", cfg.AMemorix.Retrieval.Fusion.VectorWeight, 0.7},
		{"a_memorix.retrieval.fusion.bm25_weight", cfg.AMemorix.Retrieval.Fusion.BM25Weight, 0.3},
		{"a_memorix.retrieval.sparse.candidate_k", cfg.AMemorix.Retrieval.Sparse.CandidateK, 80},
		{"a_memorix.threshold.min", cfg.AMemorix.Threshold.MinThreshold, 0.29},
		{"a_memorix.threshold.max", cfg.AMemorix.Threshold.MaxThreshold, 0.95},
		{"a_memorix.threshold.percentile", cfg.AMemorix.Threshold.Percentile, 75},
		{"a_memorix.threshold.min_results", cfg.AMemorix.Threshold.MinResults, 4},
		{"a_memorix.integration.memory_query_limit", cfg.AMemorix.Integration.MemoryQueryDefaultLimit, 5},
		{"a_memorix.integration.summary_threshold", cfg.AMemorix.Integration.ChatSummaryWritebackMessageThreshold, 36},
		{"a_memorix.episode.max_chars_per_call", cfg.AMemorix.Episode.MaxCharsPerCall, 6000},
		{"a_memorix.person_profile.top_k_evidence", cfg.AMemorix.PersonProfile.TopKEvidence, 12},
		{"a_memorix.memory.half_life_hours", cfg.AMemorix.Memory.HalfLifeHours, 24.0},
		{"mcp.enable", cfg.MCP.Enable, true},
		{"mcp.client.client_name", cfg.MCP.Client.ClientName, "MaiBot"},
		{"mcp.client.sampling.task_name", cfg.MCP.Client.Sampling.TaskName, "planner"},
		{"webui.enabled", cfg.WebUI.Enabled, true},
		{"webui.port", cfg.WebUI.Port, 8001},
		{"webui.mode", cfg.WebUI.Mode, "production"},
		{"webui.webui_style", cfg.WebUI.WebuiStyle, 1},
		{"log.date_style", cfg.Log.DateStyle, "m-d H:i:s"},
		{"log.log_level", cfg.Log.LogLevel, "INFO"},
		{"log.file_log_level", cfg.Log.FileLogLevel, "DEBUG"},
		{"log.log_file_max_bytes", cfg.Log.LogFileMaxBytes, 5 * 1024 * 1024},
		{"log.max_log_files", cfg.Log.MaxLogFiles, 30},
		{"log.snapshot_limit", cfg.Log.LLMRequestSnapshotLimit, 128},
		{"debug.show_maisaka_thinking", cfg.Debug.ShowMaisakaThinking, true},
		{"debug.enable_console_input", cfg.Debug.EnableConsoleInput, true},
		{"database.save_binary_data", cfg.Database.SaveBinaryData, false},
	}
	for _, tt := range tests {
		if !equalAny(tt.got, tt.want) {
			t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	// talk_value_rules 默认两条照抄。
	if len(cfg.Chat.ReplyTiming.TalkValueRules) != 2 {
		t.Fatalf("talk_value_rules 默认应为 2 条，实际 %d", len(cfg.Chat.ReplyTiming.TalkValueRules))
	}
	if cfg.Chat.ReplyTiming.TalkValueRules[0].Time != "00:00-08:59" ||
		cfg.Chat.ReplyTiming.TalkValueRules[0].Value != 0.8 ||
		cfg.Chat.ReplyTiming.TalkValueRules[1].Time != "09:00-18:59" ||
		cfg.Chat.ReplyTiming.TalkValueRules[1].Value != 1.0 {
		t.Errorf("talk_value_rules 默认值与 Python 版不一致: %+v", cfg.Chat.ReplyTiming.TalkValueRules)
	}
	// suppress_libraries 照抄。
	if len(cfg.Log.SuppressLibraries) != 11 || cfg.Log.SuppressLibraries[0] != "faiss" {
		t.Errorf("suppress_libraries 默认值不一致: %v", cfg.Log.SuppressLibraries)
	}
}

func equalAny(a, b any) bool {
	switch av := a.(type) {
	case bool, int, int64, float64, string:
		return av == b
	}
	return a == b
}

// TestDefaultModelConfigTemplates 校验默认模板照抄 default_model_config.py。
func TestDefaultModelConfigTemplates(t *testing.T) {
	mc := DefaultModelConfig()
	if err := mc.Validate(); err != nil {
		t.Fatalf("默认模型配置校验失败: %v", err)
	}
	if len(mc.APIProviders) != 1 || mc.APIProviders[0].Name != "DeepSeek" {
		t.Fatalf("默认 provider 应为 DeepSeek: %+v", mc.APIProviders)
	}
	if mc.APIProviders[0].BaseURL != "https://api.deepseek.com" {
		t.Errorf("provider base_url 不一致: %s", mc.APIProviders[0].BaseURL)
	}
	if mc.APIProviders[0].APIKey != "your-api-key" {
		t.Errorf("provider api_key 不一致: %s", mc.APIProviders[0].APIKey)
	}
	// build_default_model_templates 只保留任务里用到的模型。
	names := map[string]bool{}
	for _, m := range mc.Models {
		names[m.Name] = true
	}
	if !names["deepseek-v4-pro-think"] || !names["deepseek-v4-pro-nonthink"] || !names["deepseek-v4-flash"] {
		t.Errorf("默认模型缺失: %v", names)
	}
	if len(mc.Models) != 3 {
		t.Errorf("默认模型应为 3 个，实际 %d", len(mc.Models))
	}
	// 任务默认值照抄 DEFAULT_TASK_CONFIG_TEMPLATES。
	if got := mc.ModelTaskConfig.Utils; got.MaxTokens != 8192 || got.Temperature != 0.5 ||
		got.SelectionStrategy != "random" || got.HardTimeout != 120.0 ||
		len(got.ModelList) != 1 || got.ModelList[0] != "deepseek-v4-flash" {
		t.Errorf("utils 任务默认值不一致: %+v", got)
	}
	if got := mc.ModelTaskConfig.Planner; got.MaxTokens != 16384 || got.HardTimeout != 180.0 {
		t.Errorf("planner 任务默认值不一致: %+v", got)
	}
	if got := mc.ModelTaskConfig.Replyer; got.Temperature != 1 || len(got.ModelList) != 2 {
		t.Errorf("replyer 任务默认值不一致: %+v", got)
	}
	if got := mc.ModelTaskConfig.Memory; got.Temperature != 0.3 || got.HardTimeout != 240.0 {
		t.Errorf("memory 任务默认值不一致: %+v", got)
	}
	if got := mc.ModelTaskConfig.Embedding; got.MaxTokens != 4096 || got.HardTimeout != 60.0 {
		t.Errorf("embedding 任务默认值不一致: %+v", got)
	}
	// TaskConfig 结构体默认值对齐 model_configs.TaskConfig。
	def := defaultTaskConfig()
	if def.MaxTokens != 4096 || def.Temperature != 0.7 || def.SelectionStrategy != "balance" || def.HardTimeout != 240.0 {
		t.Errorf("TaskConfig 默认值不一致: %+v", def)
	}
}

// TestTaskFallbackChain 校验任务回退链语义（对齐「留空时由调用方按需回退」）。
func TestTaskFallbackChain(t *testing.T) {
	mc := DefaultModelConfig()
	// mid_memory 留空 → fast_model 留空 → utils。
	got := mc.ModelTaskConfig.ResolveTask("mid_memory")
	if len(got) != 1 || got[0] != "deepseek-v4-flash" {
		t.Errorf("mid_memory 回退结果不一致: %v", got)
	}
	// learner 留空 → utils。
	got = mc.ModelTaskConfig.ResolveTask("learner")
	if len(got) != 1 || got[0] != "deepseek-v4-flash" {
		t.Errorf("learner 回退结果不一致: %v", got)
	}
	// replyer 有自己的列表。
	got = mc.ModelTaskConfig.ResolveTask("replyer")
	if len(got) != 2 || got[0] != "deepseek-v4-pro-think" {
		t.Errorf("replyer 解析不一致: %v", got)
	}
}

// TestModelConfigValidate 校验 model_post_init 语义。
func TestModelConfigValidate(t *testing.T) {
	base := DefaultModelConfig()
	// 重复模型名。
	dup := *base
	dup.Models = append([]ModelInfo{}, base.Models...)
	dup.Models = append(dup.Models, ModelInfo{ModelIdentifier: "x", Name: "deepseek-v4-flash", APIProvider: "DeepSeek"})
	if err := dup.Validate(); err == nil {
		t.Error("重复模型名应报错")
	}
	// 引用不存在的 provider。
	miss := DefaultModelConfig()
	miss.Models[0].APIProvider = "Nope"
	if err := miss.Validate(); err == nil {
		t.Error("引用不存在 provider 应报错")
	}
	// 空 models。
	empty := DefaultModelConfig()
	empty.Models = nil
	if err := empty.Validate(); err == nil {
		t.Error("空 models 应报错")
	}
	// 重复 provider。
	dupP := DefaultModelConfig()
	dupP.APIProviders = append(dupP.APIProviders, APIProvider{Name: "DeepSeek", BaseURL: "x"})
	if err := dupP.Validate(); err == nil {
		t.Error("重复 provider 名应报错")
	}
}

// TestVersionCompare 校验 compare_versions。
func TestVersionCompare(t *testing.T) {
	tests := []struct {
		old, new string
		want     bool
	}{
		{"8.14.61", "8.14.62", true},
		{"8.14.62", "8.14.62", false},
		{"8.14.63", "8.14.62", false},
		{"8.13.99", "8.14.1", true},
		{"1.17.14", "1.17.15", true},
	}
	for _, tt := range tests {
		if got := CompareVersions(tt.old, tt.new); got != tt.want {
			t.Errorf("CompareVersions(%q, %q) = %v, want %v", tt.old, tt.new, got, tt.want)
		}
	}
}

// TestLoadBotConfigSynthesizesDefaults 校验缺省字段合成与回写。
func TestLoadBotConfigSynthesizesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bot_config.toml")
	// 用户只写了一个字段（老版本 inner）。
	userTOML := `[inner]
version = "8.0.0"

[bot]
nickname = "自定义名字"
`
	if err := os.WriteFile(path, []byte(userTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("LoadBotConfig 失败: %v", err)
	}
	if cfg.Bot.Nickname != "自定义名字" {
		t.Errorf("用户字段应保留: %s", cfg.Bot.Nickname)
	}
	// 缺省字段自动合成。
	if cfg.Chat.MaxContextSize != 40 {
		t.Errorf("缺省字段未合成: %d", cfg.Chat.MaxContextSize)
	}
	if cfg.Chat.MidTermMemoryLenth != 10 {
		t.Errorf("mid_term_memory_lenth 未合成: %d", cfg.Chat.MidTermMemoryLenth)
	}
	if cfg.AMemorix.Storage.DataDir != "data/a-memorix" {
		t.Errorf("a_memorix 缺省未合成: %s", cfg.AMemorix.Storage.DataDir)
	}
	// 版本旧 → 回写升级。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	// go-toml v2 对无特殊字符字符串输出单引号字面量（合法 TOML），兼容单/双引号。
	var versionRe = regexp.MustCompile(`version = ['"]8\.14\.62['"]`)
	if !versionRe.MatchString(content) {
		t.Errorf("旧版本文件应回写新版本号，实际:\n%s", content)
	}
	if !strings.Contains(content, `max_context_size = 40`) {
		t.Errorf("回写应包含合成后的完整结构:\n%s", content)
	}
	// 再次加载应稳定（无回写循环）。
	cfg2, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if cfg2.Bot.Nickname != "自定义名字" {
		t.Errorf("二次加载用户字段应保留")
	}
}

// TestLoadBotConfigMissingGenerates 校验缺失文件生成默认模板。
func TestLoadBotConfigMissingGenerates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "bot_config.toml")
	cfg, err := LoadBotConfig(path)
	if err != nil {
		t.Fatalf("缺失文件应生成默认配置: %v", err)
	}
	if cfg.Bot.Nickname != "麦麦" {
		t.Errorf("默认 nickname 不一致: %s", cfg.Bot.Nickname)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[inner]") || !strings.Contains(string(raw), `[bot]`) {
		t.Errorf("生成文件结构不完整:\n%s", raw)
	}
}

// TestLoadModelConfigSynthesis 校验 model_config 的缺省合成与校验。
func TestLoadModelConfigSynthesis(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_config.toml")
	userTOML := `[inner]
version = "1.0.0"

[[api_providers]]
name = "TestProvider"
base_url = "https://example.com"
api_key = "sk-test"

[[models]]
model_identifier = "test-model"
name = "test-model-main"
api_provider = "TestProvider"
price_in = 1.0
price_out = 2.0

[model_task_config.replyer]
model_list = ["test-model-main"]
`
	if err := os.WriteFile(path, []byte(userTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadModelConfig(path)
	if err != nil {
		t.Fatalf("LoadModelConfig 失败: %v", err)
	}
	// provider 缺省字段合成。
	p := cfg.APIProviders[0]
	if p.AuthType != "bearer" || p.MaxRetry != 3 || p.Timeout != 120 || p.RetryInterval != 4 {
		t.Errorf("provider 缺省字段未合成: %+v", p)
	}
	// 用户模型保留。
	if len(cfg.Models) != 1 || cfg.Models[0].Name != "test-model-main" {
		t.Fatalf("用户模型应保留: %+v", cfg.Models)
	}
	if !cfg.Models[0].SendTemperatureOrDefault() {
		t.Errorf("send_temperature 缺省应为 true")
	}
	// 任务缺省合成（temperature/max_tokens）。
	tc := cfg.ModelTaskConfig.Planner
	if tc.MaxTokens != 4096 || tc.Temperature != 0.7 || tc.SelectionStrategy != "balance" || tc.HardTimeout != 240.0 {
		t.Errorf("planner 任务缺省未合成: %+v", tc)
	}
	if tc := cfg.ModelTaskConfig.Replyer; len(tc.ModelList) != 1 || tc.ModelList[0] != "test-model-main" {
		t.Errorf("replyer 用户任务应保留: %+v", tc)
	}
	// 回写新版本（兼容 go-toml v2 的单引号字面量输出）。
	raw, _ := os.ReadFile(path)
	var modelVersionRe = regexp.MustCompile(`version = ['"]1\.17\.15['"]`)
	if !modelVersionRe.MatchString(string(raw)) {
		t.Errorf("model_config 应回写版本:\n%s", raw)
	}
}

// TestModelConfigInvalidRejects 校验非法配置被拒绝。
func TestModelConfigInvalidRejects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model_config.toml")
	badTOML := `[inner]
version = "1.17.15"

[[api_providers]]
name = "P"
base_url = "https://example.com"

[[models]]
model_identifier = "m"
name = "m1"
api_provider = "NotExist"
`
	if err := os.WriteFile(path, []byte(badTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadModelConfig(path); err == nil {
		t.Error("引用不存在 provider 的配置应被拒绝")
	}
}

// TestEnsureTemplateFiles 校验模板生成幂等。
func TestEnsureTemplateFiles(t *testing.T) {
	dir := t.TempDir()
	generated, err := EnsureTemplateFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("首次调用应生成模板")
	}
	generated, err = EnsureTemplateFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Error("二次调用不应重复生成")
	}
	if _, err := os.Stat(filepath.Join(dir, "bot_config.toml")); err != nil {
		t.Error("bot_config.toml 应存在")
	}
	if _, err := os.Stat(filepath.Join(dir, "model_config.toml")); err != nil {
		t.Error("model_config.toml 应存在")
	}
}

// TestSectionMatching 校验热重载回调的节前缀匹配（对齐 _section_matches）。
func TestSectionMatching(t *testing.T) {
	tests := []struct {
		declared, changed string
		want              bool
	}{
		{"chat", "chat", true},
		{"chat", "chat.reply_timing", true},
		{"chat.reply_timing", "chat", true},
		{"chat.reply_timing", "chat.reply_timing", true},
		{"webui", "chat", false},
		{"chatx", "chat", false},
	}
	for _, tt := range tests {
		if got := sectionMatches(tt.declared, tt.changed); got != tt.want {
			t.Errorf("sectionMatches(%q, %q) = %v, want %v", tt.declared, tt.changed, got, tt.want)
		}
	}
}

// TestBuildPersonalityEmotionSuffix 校验情绪后缀文案照抄。
func TestBuildPersonalityEmotionSuffix(t *testing.T) {
	if got := BuildPersonalityEmotionSuffix("neutral"); got != "" {
		t.Errorf("neutral 后缀应为空: %q", got)
	}
	if got := BuildPersonalityEmotionSuffix("rational_calm"); got != PersonalityEmotionSuffixes["rational_calm"] {
		t.Errorf("rational_calm 后缀不一致")
	}
	if got := BuildPersonalityEmotionSuffix("unknown"); got != "" {
		t.Errorf("未知档位应回退 neutral: %q", got)
	}
}
