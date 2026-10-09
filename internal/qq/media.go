package qq

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // 开放平台要求 MD5 校验值
	"crypto/sha1" //nolint:gosec // 开放平台要求 SHA1 校验值
	"encoding/hex"

	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 富媒体分片上传 (§6.12 media.go)
//
// POST /v2/groups/{openid}/files 之前先走:
//  1. POST {prefix}/upload_prepare  (file_type / file_size / file_name / md5 / sha1 / md5_10m)
//     → upload_id + parts[].presigned_url
//  2. PUT 每个分片到 presigned_url, 每片后 POST {prefix}/upload_part_finish
//  3. POST {prefix}/files (file_type / file_name / upload_id / srv_send_msg=false)
//     → file_info
//
// 拿到 file_info 后发 msg_type 7: {"msg_type":7,"media":{"file_info": "…"}}。
// ---------------------------------------------------------------------------

// 开放平台富媒体接口的 file_type。
const (
	// FileTypeImage 是图片。
	FileTypeImage = 1
	// FileTypeVideo 是视频。
	FileTypeVideo = 2
	// FileTypeAudio 是语音。
	FileTypeAudio = 3
	// FileTypeFile 是普通文件。
	FileTypeFile = 4
)

// 各类型的大小软上限。
const (
	// MaxImageBytes 是图片上限 (20MB)。
	MaxImageBytes = 20 << 20
	// MaxVideoBytes 是视频上限: 36MB 分片上传通过, 210MB 在预上传被拒
	// (500/850012), 取 100MB 留出余量。
	MaxVideoBytes = 100 << 20
	// MaxAudioBytes 是语音上限; 平台会转码, 过大的音频无实际意义。
	MaxAudioBytes = 20 << 20
	// MaxFileBytes 是普通文件上限。
	MaxFileBytes = 50 << 20
	// md510mBoundary 是 md5_10m 取文件头部的字节数 (平台约定)。
	md510mBoundary = 10002432
	// uploadRetries 是单个分片的上传重试次数。
	uploadRetries = 3
)

// MaxBytesFor 返回某个 file_type 的大小上限 (未知类型返回 MaxFileBytes)。
func MaxBytesFor(fileType int) int64 {
	switch fileType {
	case FileTypeImage:
		return MaxImageBytes
	case FileTypeVideo:
		return MaxVideoBytes
	case FileTypeAudio:
		return MaxAudioBytes
	default:
		return MaxFileBytes
	}
}

// uploadPrepareResponse 是预上传的响应。
type uploadPrepareResponse struct {
	UploadID  string `json:"upload_id"`
	BlockSize string `json:"block_size"`
	Parts     []struct {
		Index        int    `json:"index"`
		PresignedURL string `json:"presigned_url"`
		BlockSize    string `json:"block_size"`
	} `json:"parts"`
}

// uploadMergeResponse 是合并分片的响应。
type uploadMergeResponse struct {
	FileInfo string `json:"file_info"`
	TTL      int    `json:"ttl"`
}

// UploadMedia 把一个富媒体上传到会话, 返回可放进 media.file_info 的凭据。
//
// prefix 由 UploadPrefix 生成: /v2/groups/{openid} 或 /v2/users/{openid}。
func (a *APIClient) UploadMedia(ctx context.Context, prefix string, payload []byte, name string, fileType int) (string, error) {
	if len(payload) == 0 {
		return "", fmt.Errorf("qq: 媒体内容为空")
	}
	if limit := MaxBytesFor(fileType); int64(len(payload)) > limit {
		return "", fmt.Errorf("qq: 媒体 %d 字节超过上限 %d", len(payload), limit)
	}
	auth, err := a.AuthHeader(ctx)
	if err != nil {
		return "", err
	}
	headers := map[string]string{"Authorization": auth}
	call := func(path string, body any, out any) error {
		req, err := a.newRequest(ctx, http.MethodPost, prefix+path, headers, body)
		if err != nil {
			return err
		}
		raw, status, err := a.httpDo(ctx, req)
		if err != nil {
			return fmt.Errorf("qq: 请求 %s 失败: %w", path, err)
		}
		return decodeEnvelope(raw, status, out)
	}

	md5Sum := md5.Sum(payload)   //nolint:gosec
	sha1Sum := sha1.Sum(payload) //nolint:gosec
	head := payload
	if len(head) > md510mBoundary {
		head = head[:md510mBoundary]
	}
	headSum := md5.Sum(head) //nolint:gosec

	var prepared uploadPrepareResponse
	if err := call("/upload_prepare", map[string]any{
		"file_type": fileType,
		"file_size": strconv.Itoa(len(payload)),
		"file_name": mediaName(name, fileType),
		"md5":       hex.EncodeToString(md5Sum[:]),
		"sha1":      hex.EncodeToString(sha1Sum[:]),
		"md5_10m":   hex.EncodeToString(headSum[:]),
	}, &prepared); err != nil {
		return "", fmt.Errorf("qq: 预上传失败: %w", err)
	}
	if prepared.UploadID == "" || len(prepared.Parts) == 0 {
		return "", fmt.Errorf("qq: 预上传没有返回上传任务")
	}
	sort.Slice(prepared.Parts, func(i, j int) bool { return prepared.Parts[i].Index < prepared.Parts[j].Index })

	offset := 0
	for _, part := range prepared.Parts {
		sizeText := part.BlockSize
		if strings.TrimSpace(sizeText) == "" {
			sizeText = prepared.BlockSize
		}
		size, err := strconv.Atoi(strings.TrimSpace(sizeText))
		if err != nil || size <= 0 {
			return "", fmt.Errorf("qq: 分片 %d 的大小无效: %q", part.Index, sizeText)
		}
		if offset >= len(payload) {
			return "", fmt.Errorf("qq: 分片计划超出媒体长度")
		}
		end := min(offset+size, len(payload))
		chunk := payload[offset:end]
		offset = end
		chunkSum := md5.Sum(chunk) //nolint:gosec
		var lastErr error
		for attempt := 0; attempt < uploadRetries; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			if lastErr = a.putPart(ctx, part.PresignedURL, chunk); lastErr != nil {
				continue
			}
			lastErr = call("/upload_part_finish", map[string]any{
				"upload_id":  prepared.UploadID,
				"part_index": part.Index,
				"block_size": strconv.Itoa(len(chunk)),
				"md5":        hex.EncodeToString(chunkSum[:]),
			}, nil)
			if lastErr == nil {
				break
			}
		}
		if lastErr != nil {
			return "", fmt.Errorf("qq: 上传分片 %d 失败: %w", part.Index, lastErr)
		}
	}
	if offset != len(payload) {
		return "", fmt.Errorf("qq: 分片计划没有覆盖整个媒体")
	}

	var merged uploadMergeResponse
	if err := call("/files", map[string]any{
		"file_type":    fileType,
		"file_name":    mediaName(name, fileType),
		"upload_id":    prepared.UploadID,
		"srv_send_msg": false,
	}, &merged); err != nil {
		return "", fmt.Errorf("qq: 合并分片失败: %w", err)
	}
	if strings.TrimSpace(merged.FileInfo) == "" {
		return "", fmt.Errorf("qq: 合并结果缺少 file_info")
	}
	return strings.TrimSpace(merged.FileInfo), nil
}

// UploadPrefix 返回会话的媒体前缀: /v2/groups/{openid} 或 /v2/users/{openid}。
func (a *APIClient) UploadPrefix(target OutboundTarget) (string, error) {
	id, isGroup := target.Target()
	if id == "" {
		return "", fmt.Errorf("qq: 缺少会话标识 (group_openid 或 user openid)")
	}
	if isGroup {
		return fmt.Sprintf("%s/v2/groups/%s", a.APIBase(), url.PathEscape(id)), nil
	}
	return fmt.Sprintf("%s/v2/users/%s", a.APIBase(), url.PathEscape(id)), nil
}

// SendMedia 上传一个富媒体并以 msg_type 7 发出。
func (a *APIClient) SendMedia(ctx context.Context, req SendRequest, payload []byte, name string, fileType int) (*SendResponse, error) {
	prefix, err := a.UploadPrefix(req.Target)
	if err != nil {
		return nil, err
	}
	fileInfo, err := a.UploadMedia(ctx, prefix, payload, name, fileType)
	if err != nil {
		return nil, err
	}
	mediaReq := req
	mediaReq.Media = fileInfo
	mediaReq.Text = ""
	return a.Send(ctx, mediaReq)
}

// putPart 把一个分片 PUT 到预签名地址; 这个地址自带签名, 不能再带鉴权头。
func (a *APIClient) putPart(ctx context.Context, presignedURL string, chunk []byte) error {
	if strings.TrimSpace(presignedURL) == "" {
		return fmt.Errorf("预签名地址为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, presignedURL, bytes.NewReader(chunk))
	if err != nil {
		return fmt.Errorf("预签名地址无效")
	}
	req.ContentLength = int64(len(chunk))
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 256))
	}
	return nil
}

// mediaName 取平台可识别的文件名; 空或无扩展名时按类型给默认名。
func mediaName(name string, fileType int) string {
	trimmed := strings.TrimSpace(name)
	if trimmed != "" {
		if base := filepath.Base(trimmed); base != "" && base != "." && base != "/" && filepath.Ext(base) != "" {
			return base
		}
	}
	switch fileType {
	case FileTypeImage:
		return "image.png"
	case FileTypeVideo:
		return "video.mp4"
	case FileTypeAudio:
		return "audio.mp3"
	default:
		return "file.bin"
	}
}

// ---------------------------------------------------------------------------
// 入站附件 → 本地字节 (供上层图片 / 表情 / 语音处理)
// ---------------------------------------------------------------------------

// FetchAttachment 下载一条入站附件的字节 (带大小上限)。
//
// 官方给的附件 URL 带一次性签名, 过期即失效: 上层应及时取走需要的内容。
func (a *APIClient) FetchAttachment(ctx context.Context, attachment Attachment, maxBytes int64) ([]byte, error) {
	target := strings.TrimSpace(attachment.URL)
	if target == "" {
		return nil, fmt.Errorf("qq: 附件缺少下载地址")
	}
	if maxBytes <= 0 {
		maxBytes = MaxBytesFor(FileTypeImage)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("qq: 附件地址无效: %w", err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("qq: 下载附件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("qq: 下载附件失败: http %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("qq: 读取附件失败: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("qq: 附件 %d 字节超过上限 %d", len(data), maxBytes)
	}
	return data, nil
}
