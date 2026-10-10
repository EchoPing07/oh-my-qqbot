package emoji

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"log/slog"

	"io"
)

// 图片格式探测与 sha256（对齐 BaseImageDataModel.calculate_hash_format /
// get_image_format 与 ImageUtils 辅助）。

// 解码尺寸上限 — S6 修复：解码前从不校验声明的宽高，GIF/PNG 头可声明
// 65535×65535 → 按声明尺寸分配全画布（~17GB）→ fatal OOM（Go 运行时内存
// 分配失败不可 recover，直接崩进程）。Go 标准库无 PIL Image.MAX_IMAGE_PIXELS
// 式防护，统一入口在解码前用 image.DecodeConfig（只读头部，不分配像素）
// 校验声明尺寸。
const (
	// maxImageDimension 是单边像素上限。
	maxImageDimension = 8192
	// maxImagePixels 是像素总量上限（40M ≈ 8192×4883；参考 PIL 默认
	// MAX_IMAGE_PIXELS ≈ 1.78 亿的可拒绝阈值，此处更保守）。
	maxImagePixels = 40 << 20
)

// checkImageSizeLimit 校验图片声明尺寸（经 image.DecodeConfig 读头），
// 超限返回错误；无法识别格式返回其解析错误。
func checkImageSizeLimit(data []byte) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("解析图片头部失败: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("图片声明尺寸非法: %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.Width > maxImageDimension || cfg.Height > maxImageDimension {
		return fmt.Errorf("图片声明尺寸 %dx%d 超过单边上限 %d", cfg.Width, cfg.Height, maxImageDimension)
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return fmt.Errorf("图片声明像素量 %d 超过上限 %d", cfg.Width*cfg.Height, maxImagePixels)
	}
	return nil
}

// sha256Hex 计算字节内容的 sha256 十六进制串。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// detectImageFormat mirrors get_image_format：从图片字节探测实际格式
// （png/jpeg/gif），无法识别返回空串。DecodeConfig 只读头部，无巨型分配风险。
func detectImageFormat(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	switch format {
	case "png", "jpeg", "gif", "bmp", "webp":
		if format == "jpeg" {
			return "jpg"
		}
		return format
	}
	return ""
}

// gifFirstFrame 抽取 GIF 首帧为 JPEG 静态图（对齐
// ImageUtils.gif_2_static_image 在 emoji 描述路径的用途：GIF → 静态图请求）。
// 返回 (JPEG 字节, "jpg", true)；解析失败返回 false。
// S6：解码前校验声明尺寸；仅用首帧，用 gif.Decode（单帧）替代
// gif.DecodeAll（解码全部帧、逐帧分配），行为保持取首帧不变。
func gifFirstFrame(data []byte) ([]byte, string, bool) {
	if err := checkImageSizeLimit(data); err != nil {
		slog.Warn("GIF 声明尺寸超限，拒绝解码", "err", err)
		return nil, "", false
	}
	frame, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", false
	}
	// GIF 帧可能带调色板透明信息，先落到 RGBA 画布再编码 JPEG。
	bounds := frame.Bounds()
	canvas := image.NewRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			canvas.Set(x, y, frame.At(x, y))
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, "", false
	}
	return buf.Bytes(), "jpg", true
}

// ReadImageBytes 读取文件字节（供外部发送路径复用）。
func ReadImageBytes(path string) ([]byte, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
