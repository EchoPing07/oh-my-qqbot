// S6 图片解码尺寸上限测试 — 构造声明超大尺寸的小 PNG/GIF 头，
// 验证统一入口在解码前拒绝（不触发巨型分配）。
package emoji

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

// patchPNGHeaderSize 把 PNG 的 IHDR 宽高改成声明的 w/h（重算 CRC，
// 保证 DecodeConfig 能正常读头）。
func patchPNGHeaderSize(t *testing.T, data []byte, w, h int) []byte {
	t.Helper()
	const ihdrTypeOff = 12 // 8 字节签名 + 4 字节长度
	if len(data) < ihdrTypeOff+4 || string(data[ihdrTypeOff:ihdrTypeOff+4]) != "IHDR" {
		t.Fatalf("输入不是含 IHDR 的 PNG")
	}
	out := append([]byte(nil), data...)
	binary.BigEndian.PutUint32(out[16:20], uint32(w))
	binary.BigEndian.PutUint32(out[20:24], uint32(h))
	// CRC 覆盖 chunk type + data（13 字节）。
	sum := crc32.ChecksumIEEE(out[ihdrTypeOff : ihdrTypeOff+4+13])
	binary.BigEndian.PutUint32(out[ihdrTypeOff+17:ihdrTypeOff+21], sum)
	return out
}

// patchGIFHeaderSize 把 GIF 逻辑屏幕描述符的宽高改成声明的 w/h（小端）。
func patchGIFHeaderSize(t *testing.T, data []byte, w, h int) []byte {
	t.Helper()
	if len(data) < 10 || string(data[:6]) != "GIF89a" {
		t.Fatalf("输入不是 GIF89a")
	}
	out := append([]byte(nil), data...)
	binary.LittleEndian.PutUint16(out[6:8], uint16(w))
	binary.LittleEndian.PutUint16(out[8:10], uint16(h))
	return out
}

// smallTestGIF 生成 2x2 两色 GIF。
func smallTestGIF(t *testing.T) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	img := image.NewPaletted(image.Rect(0, 0, 2, 2), pal)
	img.SetColorIndex(0, 0, 1)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestPatchHelpersSanity 校验补丁后头部确实声明了目标尺寸（DecodeConfig 读头）。
func TestPatchHelpersSanity(t *testing.T) {
	pngData := patchPNGHeaderSize(t, buildTestPNG(t, 4, 4, color.RGBA{1, 2, 3, 255}), 65535, 65535)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(pngData))
	if err != nil {
		t.Fatalf("补丁 PNG 头应可解析: %v", err)
	}
	if format != "png" || cfg.Width != 65535 || cfg.Height != 65535 {
		t.Fatalf("补丁 PNG 头不符: %s %dx%d", format, cfg.Width, cfg.Height)
	}

	gifData := patchGIFHeaderSize(t, smallTestGIF(t), 65535, 65535)
	cfg, format, err = image.DecodeConfig(bytes.NewReader(gifData))
	if err != nil {
		t.Fatalf("补丁 GIF 头应可解析: %v", err)
	}
	if format != "gif" || cfg.Width != 65535 || cfg.Height != 65535 {
		t.Fatalf("补丁 GIF 头不符: %s %dx%d", format, cfg.Width, cfg.Height)
	}
}

// TestCheckImageSizeLimit 超限声明被拒绝；正常图片放行。
func TestCheckImageSizeLimit(t *testing.T) {
	// 正常 PNG / GIF 通过。
	if err := checkImageSizeLimit(buildTestPNG(t, 64, 64, color.RGBA{0, 0, 0, 255})); err != nil {
		t.Fatalf("正常 PNG 不应被拒: %v", err)
	}
	if err := checkImageSizeLimit(smallTestGIF(t)); err != nil {
		t.Fatalf("正常 GIF 不应被拒: %v", err)
	}
	// 声明 65535x65535 的小 PNG（S6 攻击面）。
	if err := checkImageSizeLimit(patchPNGHeaderSize(t, buildTestPNG(t, 4, 4, color.RGBA{0, 0, 0, 255}), 65535, 65535)); err == nil {
		t.Fatalf("声明超大尺寸的 PNG 应被拒绝")
	}
	// 声明 65535x65535 的小 GIF。
	if err := checkImageSizeLimit(patchGIFHeaderSize(t, smallTestGIF(t), 65535, 65535)); err == nil {
		t.Fatalf("声明超大尺寸的 GIF 应被拒绝")
	}
	// 单边超限。
	if err := checkImageSizeLimit(patchPNGHeaderSize(t, buildTestPNG(t, 4, 4, color.RGBA{0, 0, 0, 255}), 9000, 10)); err == nil {
		t.Fatalf("单边超限的 PNG 应被拒绝")
	}
	// 像素总量超限（5000x9000 = 45M > 40M）。
	if err := checkImageSizeLimit(patchPNGHeaderSize(t, buildTestPNG(t, 4, 4, color.RGBA{0, 0, 0, 255}), 5000, 9000)); err == nil {
		t.Fatalf("像素总量超限的 PNG 应被拒绝")
	}
	// 垃圾字节报解析错误。
	if err := checkImageSizeLimit([]byte("not-an-image")); err == nil {
		t.Fatalf("无法识别的字节应报错")
	}
}

// TestGifFirstFrameRejectsOversized gifFirstFrame 对超限声明 GIF 返回 false。
func TestGifFirstFrameRejectsOversized(t *testing.T) {
	if _, _, ok := gifFirstFrame(patchGIFHeaderSize(t, smallTestGIF(t), 65535, 65535)); ok {
		t.Fatalf("超限 GIF 应拒绝解码")
	}
	// 正常 GIF 仍可用（行为回归）。
	jpegBytes, format, ok := gifFirstFrame(smallTestGIF(t))
	if !ok || format != "jpg" || len(jpegBytes) == 0 {
		t.Fatalf("正常 GIF 首帧抽取应成功")
	}
}

// TestBuildLabeledTileOversizedFallsBackToPlaceholder 拼图对超限声明图走占位图。
func TestBuildLabeledTileOversizedFallsBackToPlaceholder(t *testing.T) {
	oversized := patchPNGHeaderSize(t, buildTestPNG(t, 4, 4, color.RGBA{0, 255, 0, 255}), 65535, 65535)
	tile := buildLabeledTile(oversized, 3, CollageTileSize)
	if tile == nil || tile.Bounds().Dx() != CollageTileSize || tile.Bounds().Dy() != CollageTileSize {
		t.Fatalf("超限图应返回占位图格子")
	}
	// 占位图底色是浅灰：与正常图路径（白底）区分——这里只断言不 panic 且尺寸正确。
	if tile.At(0, 0) == nil {
		t.Fatalf("占位图应有像素")
	}
}
