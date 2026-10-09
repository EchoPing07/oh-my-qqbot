package emoji

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/gif"
	"image/jpeg"

	"io"
)

// 图片格式探测与 sha256（对齐 BaseImageDataModel.calculate_hash_format /
// get_image_format 与 ImageUtils 辅助）。

// sha256Hex 计算字节内容的 sha256 十六进制串。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// detectImageFormat mirrors get_image_format：从图片字节探测实际格式
// （png/jpeg/gif），无法识别返回空串。
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
func gifFirstFrame(data []byte) ([]byte, string, bool) {
	g, err := gif.DecodeAll(bytes.NewReader(data))
	if err != nil || len(g.Image) == 0 {
		return nil, "", false
	}
	frame := g.Image[0]
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
