package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	contracts "oh-my-qqbot/internal"
)

// EmbeddingOptions carries per-call embedding overrides.
type EmbeddingOptions struct {
	// ExtraBody merges model extra_params into the request body (e.g. `task`
	// for multimodal embedding endpoints).
	ExtraBody map[string]any
	// ExtraHeaders merges provider-level headers.
	ExtraHeaders map[string]string
	// ExtraQuery merges provider-level query parameters.
	ExtraQuery map[string]any
	// Dimensions requests a specific vector size when > 0.
	Dimensions int
	// EncodingFormat mirrors the OpenAI `encoding_format` field (defaults to
	// "float" in the wire request).
	EncodingFormat string
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Model string          `json:"model"`
	Usage json.RawMessage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// EmbeddingResult is one embedding response together with the usage that must
// be accounted for.
type EmbeddingResult struct {
	Vectors [][]float32
	Model   string
	Usage   contracts.Usage
}

// Embeddings calls the OpenAI-compatible /embeddings endpoint for a batch of
// texts. Aligned with openai_client.get_embedding in
// 参考/MaiBot/src/llm_models/model_client/openai_client.py and
// 参考/Diana/model/llm/embeddings.go (one clean request, retries handled by the
// orchestrator).
func (c *Client) Embeddings(ctx context.Context, model ModelInfo, texts []string, options EmbeddingOptions) ([][]float32, error) {
	result, err := c.EmbeddingsWithUsage(ctx, model, texts, options)
	if err != nil {
		return nil, err
	}
	return result.Vectors, nil
}

// EmbeddingsWithUsage is Embeddings plus the provider-reported usage.
func (c *Client) EmbeddingsWithUsage(ctx context.Context, model ModelInfo, texts []string, options EmbeddingOptions) (*EmbeddingResult, error) {
	if len(texts) == 0 {
		return &EmbeddingResult{}, nil
	}
	identifier := model.ModelIdentifier
	if strings.TrimSpace(identifier) == "" {
		identifier = model.Name
	}
	if strings.TrimSpace(identifier) == "" {
		return nil, fmt.Errorf("llm: 模型标识符不能为空")
	}

	body := map[string]any{
		"model": identifier,
		"input": texts,
	}
	if options.Dimensions > 0 {
		body["dimensions"] = options.Dimensions
	}
	format := strings.TrimSpace(options.EncodingFormat)
	if format == "" {
		format = "float"
	}
	body["encoding_format"] = format

	for key, value := range options.ExtraBody {
		if key == "headers" || key == "query" {
			continue
		}
		if key == "body" {
			for nestedKey, nestedValue := range asAnyMap(value) {
				body[nestedKey] = nestedValue
			}
			continue
		}
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: encode embeddings request: %w", err)
	}

	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
	}
	for key, value := range options.ExtraHeaders {
		headers[key] = value
	}
	raw, err := c.do(ctx, "embeddings", payload, headers, options.ExtraQuery, "")
	if err != nil {
		return nil, err
	}

	var parsed embeddingsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseResponse, err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("llm: provider error: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("%w: 嵌入响应返回 %d 个向量，期望 %d 个", ErrParseResponse, len(parsed.Data), len(texts))
	}

	// The provider may return items out of order; index sorts them back
	// (mirrors Diana's sort on `data[].index`).
	items := append([]struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}(nil), parsed.Data...)
	sort.SliceStable(items, func(left, right int) bool { return items[left].Index < items[right].Index })

	vectors := make([][]float32, len(items))
	for position, item := range items {
		if len(item.Embedding) == 0 {
			return nil, fmt.Errorf("%w: 嵌入响应第 %d 项为空", ErrParseResponse, position)
		}
		if item.Index < 0 || item.Index >= len(vectors) {
			return nil, fmt.Errorf("%w: 嵌入响应索引越界: %d", ErrParseResponse, item.Index)
		}
		vectors[item.Index] = item.Embedding
	}
	for index, vector := range vectors {
		if len(vector) == 0 {
			return nil, fmt.Errorf("%w: 嵌入响应缺少索引 %d", ErrParseResponse, index)
		}
	}
	return &EmbeddingResult{
		Vectors: vectors,
		Model:   firstNonEmpty(parsed.Model, identifier),
		Usage:   parseUsage(parsed.Usage),
	}, nil
}

// EmbedText is a single-text convenience wrapper used by the memory kernel.
func (c *Client) EmbedText(ctx context.Context, model ModelInfo, text string, options EmbeddingOptions) ([]float32, error) {
	vectors, err := c.Embeddings(ctx, model, []string{text}, options)
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, fmt.Errorf("%w: 嵌入响应为空", ErrParseResponse)
	}
	return vectors[0], nil
}

// EmbedRemoteImage mirrors the image-embedding endpoint of MaiBot
// (ImageEmbeddingRequest + `_build_image_embedding_request`): the image travels
// as a base64 data URI inside the `input` array. It is intentionally kept out of
// internal.LLMLayer (P5 wires it optionally).
func (c *Client) EmbedRemoteImage(ctx context.Context, model ModelInfo, mimeType string, imageBase64 string, options EmbeddingOptions) (*EmbeddingResult, error) {
	identifier := firstNonEmpty(model.ModelIdentifier, model.Name)
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "image/jpeg"
	}
	body := map[string]any{
		"model": identifier,
		"input": []map[string]any{{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + mimeType + ";base64," + imageBase64},
		}},
	}
	for key, value := range options.ExtraBody {
		if key == "headers" || key == "query" {
			continue
		}
		if key == "body" {
			for nestedKey, nestedValue := range asAnyMap(value) {
				body[nestedKey] = nestedValue
			}
			continue
		}
		body[key] = value
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: encode image embedding request: %w", err)
	}
	raw, err := c.do(ctx, "embeddings", payload, map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json",
	}, options.ExtraQuery, "")
	if err != nil {
		return nil, err
	}
	var parsed embeddingsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParseResponse, err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("%w: 图片嵌入响应缺少 embeddings 数据", ErrParseResponse)
	}
	return &EmbeddingResult{
		Vectors: [][]float32{parsed.Data[0].Embedding},
		Model:   firstNonEmpty(parsed.Model, identifier),
		Usage:   parseUsage(parsed.Usage),
	}, nil
}
