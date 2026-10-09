package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 本文件覆盖 models.py 的内容块转换语义（json → 统一 ToolContentItem）以及
// 连接层的工具结果拼装。

func TestBuildToolContentItemTypes(t *testing.T) {
	cases := []struct {
		name        string
		content     mcp.Content
		wantType    ToolContentType
		wantHistory string
	}{
		{
			name:        "文本",
			content:     &mcp.TextContent{Text: "  hello  "},
			wantType:    ContentText,
			wantHistory: "hello",
		},
		{
			name:        "图片",
			content:     &mcp.ImageContent{MIMEType: "image/png", Data: []byte("QUJD")},
			wantType:    ContentImage,
			wantHistory: "[图片内容 image/png]",
		},
		{
			name:        "音频",
			content:     &mcp.AudioContent{MIMEType: "audio/wav", Data: []byte("QUJD")},
			wantType:    ContentAudio,
			wantHistory: "[音频内容 audio/wav]",
		},
		{
			name:        "资源链接",
			content:     &mcp.ResourceLink{URI: "mem://x", Name: "名字", MIMEType: "text/plain"},
			wantType:    ContentResourceLink,
			wantHistory: "[资源链接] 名字",
		},
		{
			name: "嵌入资源(文本)",
			content: &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI: "mem://y", MIMEType: "text/plain", Text: "资源正文",
			}},
			wantType:    ContentResource,
			wantHistory: "资源正文",
		},
		{
			name: "嵌入资源(二进制)",
			content: &mcp.EmbeddedResource{Resource: &mcp.ResourceContents{
				URI: "mem://z", MIMEType: "application/octet-stream", Blob: []byte("QUJD"),
			}},
			wantType:    ContentResource,
			wantHistory: "[嵌入资源] mem://z",
		},
		{
			name:        "工具调用块",
			content:     &mcp.ToolUseContent{ID: "c1", Name: "calc", Input: map[string]any{"x": 1}},
			wantType:    ContentText,
			wantHistory: "[工具调用] calc {\"x\":1}",
		},
		{
			name:        "未知类型",
			content:     &mcp.TextContent{},
			wantType:    ContentText,
			wantHistory: "",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			item := BuildToolContentItem(testCase.content)
			if item.ContentType != testCase.wantType {
				t.Fatalf("类型 = %s, want %s", item.ContentType, testCase.wantType)
			}
			if got := item.BuildHistoryText(); got != testCase.wantHistory {
				t.Fatalf("历史文本 = %q, want %q", got, testCase.wantHistory)
			}
		})
	}
}

func TestBuildToolContentItemKeepsDataAndMetadata(t *testing.T) {
	item := BuildToolContentItem(&mcp.ImageContent{
		MIMEType: "image/png",
		Data:     []byte("QUJD"),
		Meta:     mcp.Meta{"k": "v"},
	})
	if item.MIMEType != "image/png" || item.Data != "QUJD" {
		t.Fatalf("图片数据丢失: %+v", item)
	}
	if item.Metadata["k"] != "v" {
		t.Fatalf("元数据丢失: %+v", item.Metadata)
	}

	// 内容块注解 → ToolAnnotation（audience/priority）。
	annotated := BuildToolContentItem(&mcp.TextContent{
		Text:        "x",
		Annotations: &mcp.Annotations{Audience: []mcp.Role{"user"}, Priority: 0.5},
	})
	if annotated.Annotation == nil || len(annotated.Annotation.Audience) != 1 ||
		annotated.Annotation.Priority == nil || *annotated.Annotation.Priority != 0.5 {
		t.Fatalf("注解转换异常: %+v", annotated.Annotation)
	}
	// 空注解 → nil（对齐 build_tool_annotation 的「无有效内容时返回 None」）。
	if empty := BuildToolContentItem(&mcp.TextContent{Text: "x"}); empty.Annotation != nil {
		t.Fatalf("空注解应为 nil: %+v", empty.Annotation)
	}

	// 嵌入资源元数据与注解合并。
	embedded := BuildToolContentItem(&mcp.EmbeddedResource{
		Meta:        mcp.Meta{"outer": "1"},
		Annotations: &mcp.Annotations{Audience: []mcp.Role{"assistant"}},
		Resource: &mcp.ResourceContents{
			URI: "mem://m", MIMEType: "text/plain", Text: "t",
			Meta: mcp.Meta{"inner": "2"},
		},
	})
	if embedded.Metadata["outer"] != "1" || embedded.Metadata["inner"] != "2" {
		t.Fatalf("资源元数据未合并: %+v", embedded.Metadata)
	}
	if embedded.URI != "mem://m" || embedded.Text != "t" {
		t.Fatalf("资源字段异常: %+v", embedded)
	}
}

func TestBuildToolContentItemsEmpty(t *testing.T) {
	if items := BuildToolContentItems(nil); items != nil {
		t.Fatalf("空输入应返回 nil: %+v", items)
	}
	items := BuildToolContentItems([]mcp.Content{
		&mcp.TextContent{Text: "a"},
		&mcp.TextContent{Text: "b"},
	})
	if len(items) != 2 {
		t.Fatalf("长度异常: %d", len(items))
	}
}

func TestSummarizeToolResultContent(t *testing.T) {
	// 成功:文本 + structuredContent 的 JSON。
	summary := SummarizeToolResultContent(&mcp.ToolResultContent{
		ToolUseID: "c1",
		Content: []mcp.Content{
			&mcp.TextContent{Text: "第一行"},
			&mcp.ImageContent{MIMEType: "image/png", Data: []byte("QUJD")},
		},
		StructuredContent: map[string]any{"ok": true},
	})
	if !strings.Contains(summary, "第一行") || !strings.Contains(summary, "[图片内容 image/png]") ||
		!strings.Contains(summary, `{"ok":true}`) {
		t.Fatalf("工具结果摘要异常: %q", summary)
	}

	// 失败:前缀文案照抄 _summarize_tool_result_content。
	failed := SummarizeToolResultContent(&mcp.ToolResultContent{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: "炸了"}},
	})
	if !strings.HasPrefix(failed, "工具执行失败：\n") || !strings.Contains(failed, "炸了") {
		t.Fatalf("失败摘要异常: %q", failed)
	}
	emptyFailed := SummarizeToolResultContent(&mcp.ToolResultContent{IsError: true})
	if emptyFailed != "工具执行失败。" {
		t.Fatalf("空失败摘要异常: %q", emptyFailed)
	}
	if got := SummarizeToolResultContent(nil); got != "" {
		t.Fatalf("nil 摘要应为空: %q", got)
	}
}

func TestJoinTextPartsAndHistoryText(t *testing.T) {
	items := []ToolContentItem{
		{ContentType: ContentText, Text: " a "},
		{ContentType: ContentImage, MIMEType: "image/png", Data: "x"},
		{ContentType: ContentText, Text: "b"},
		{ContentType: ContentText, Text: "   "},
	}
	if got := JoinTextParts(items); got != "a\nb" {
		t.Fatalf("文本拼接异常: %q", got)
	}
	// HistoryText 优先 content。
	if got := HistoryText(" direct ", items, nil, "err"); got != "direct" {
		t.Fatalf("content 优先失败: %q", got)
	}
	// 无 content → 内容块摘要。
	if got := HistoryText("", items, nil, "err"); !strings.Contains(got, "a") || !strings.Contains(got, "[图片内容 image/png]") {
		t.Fatalf("内容块摘要异常: %q", got)
	}
	// 无内容块 → structured_content。
	if got := HistoryText("", nil, map[string]any{"k": 1}, "err"); got != `{"k":1}` {
		t.Fatalf("structured 回落异常: %q", got)
	}
	// 字符串 structured_content 原样返回。
	if got := HistoryText("", nil, "raw", "err"); got != "raw" {
		t.Fatalf("字符串 structured 回落异常: %q", got)
	}
	// 最后回落到错误信息。
	if got := HistoryText("", nil, nil, " boom "); got != "boom" {
		t.Fatalf("错误信息回落异常: %q", got)
	}
}

func TestBuildToolAnnotationAndIcons(t *testing.T) {
	destroy := true
	openWorld := false
	annotation := BuildToolAnnotation(&mcp.ToolAnnotations{
		Title:           " 标题 ",
		ReadOnlyHint:    true,
		DestructiveHint: &destroy,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
	})
	if annotation == nil {
		t.Fatalf("注解不应为空")
	}
	if annotation.Title != "标题" || annotation.ReadOnly == nil || !*annotation.ReadOnly {
		t.Fatalf("注解字段异常: %+v", annotation)
	}
	if annotation.Destructive == nil || !*annotation.Destructive {
		t.Fatalf("destructiveHint 未透传: %+v", annotation.Destructive)
	}
	if annotation.Idempotent == nil || !*annotation.Idempotent {
		t.Fatalf("idempotentHint 未透传: %+v", annotation.Idempotent)
	}
	if annotation.OpenWorld == nil || *annotation.OpenWorld {
		t.Fatalf("openWorldHint 未透传: %+v", annotation.OpenWorld)
	}
	if BuildToolAnnotation(nil) != nil {
		t.Fatalf("nil 注解应为 nil")
	}

	icons := BuildToolIcons([]mcp.Icon{{Source: "https://x/i.png", MIMEType: "image/png", Sizes: []string{"48x48"}}})
	if len(icons) != 1 || icons[0].Src != "https://x/i.png" || icons[0].Sizes[0] != "48x48" {
		t.Fatalf("图标转换异常: %+v", icons)
	}
	if BuildToolIcons(nil) != nil {
		t.Fatalf("空图标应为 nil")
	}
}

func TestCallToolReturnsImageContent(t *testing.T) {
	// echo 工具返回文本 + 图片:文本进 content，图片保留在 ContentItems。
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	result := service.CallTool(context.Background(), ToolInvocation{
		ToolName:  "echo",
		Arguments: map[string]any{"text": "带图"},
	}, SessionContext{})
	if !result.Success {
		t.Fatalf("调用失败: %+v", result)
	}
	if result.Content != "echo:带图" {
		t.Fatalf("文本内容异常: %q", result.Content)
	}
	if len(result.ContentItems) != 2 {
		t.Fatalf("内容块数量异常: %+v", result.ContentItems)
	}
	if result.ContentItems[1].ContentType != ContentImage || result.ContentItems[1].MIMEType != "image/png" {
		t.Fatalf("图片块未保留: %+v", result.ContentItems[1])
	}
	// HistoryContent 只含文本（图片以摘要形式补位）。
	if got := result.HistoryContent(); got != "echo:带图" {
		t.Fatalf("历史文本异常: %q", got)
	}
}

func TestCallToolOnDisconnectedServer(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	service.mu.Lock()
	connection := service.manager.connections["alpha"]
	service.mu.Unlock()

	// 关闭底层连接但保留路由表 → 调用失败并带 servers 级错误信息。
	if err := connection.closeSession(); err != nil {
		t.Fatalf("关闭会话失败: %v", err)
	}
	result := service.CallTool(context.Background(), ToolInvocation{ToolName: "echo"}, SessionContext{})
	if result.Success {
		t.Fatalf("断连后不应成功: %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "未连接") {
		t.Fatalf("错误文案异常: %q", result.ErrorMessage)
	}
}

func TestManagerUnknownToolAndPrompt(t *testing.T) {
	restore := installInMemoryTransport(t, testServerOptions{})
	defer restore()

	service := NewService()
	defer func() { _ = service.Close() }()
	if err := service.EnsureInitialized(context.Background(), stdioTestConfig("alpha")); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	result := service.CallTool(context.Background(), ToolInvocation{ToolName: "ghost"}, SessionContext{})
	if result.Success {
		t.Fatalf("未知工具不应成功")
	}
	if !strings.Contains(result.ErrorMessage, "未找到") {
		t.Fatalf("错误文案异常: %q", result.ErrorMessage)
	}
	if service.IsTool("ghost") || !service.IsTool("echo") {
		t.Fatalf("IsTool 判定异常")
	}
	if rule := service.PermissionFor("ghost"); !rule.IsUnrestricted() {
		t.Fatalf("未知工具的权限应为空规则")
	}
}
