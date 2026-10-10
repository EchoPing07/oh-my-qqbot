package emoji

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log/slog"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// This file mirrors the collage helpers of
// 参考/MaiBot/src/maisaka/builtin_tool/send_emoji.py (behaviour is
// authoritative there):
//
//   - _EMOJI_CANDIDATE_TILE_SIZE = 256 / _EMOJI_INDEX_BADGE_SIZE = 64
//   - _EMOJI_INDEX_FONT_SIZE = 32（Go 版用 basicfont.Face7x13 放大绘制）
//   - _calculate_grid_shape / _build_labeled_tile / _build_placeholder_tile /
//     _merge_emoji_tiles
//
// 合成用 golang.org/x/image + image/png（REWRITE_PLAN §3 图像选型）。

// 常量照抄 send_emoji.py。
const (
	// CollageTileSize mirrors _EMOJI_CANDIDATE_TILE_SIZE（256px 格）。
	CollageTileSize = 256
	// IndexBadgeSize mirrors _EMOJI_INDEX_BADGE_SIZE（64px 序号角标）。
	IndexBadgeSize = 64
	// badgeMargin 对齐 _build_labeled_tile 的 badge_margin = 14。
	badgeMargin = 14
	// badgeRadius 对齐 rounded_rectangle 的 radius=8。
	badgeRadius = 8
	// gap 对齐 _merge_emoji_tiles 的 gap = 12。
	collageGap = 12
)

// CalculateGridShape mirrors _calculate_grid_shape: 计算尽量接近矩形的
// (rows, columns) 拼图行列数（aspect_gap 优先、empty_slots 次之）。
func CalculateGridShape(candidateCount int) (rows int, columns int) {
	if candidateCount <= 0 {
		return 1, 1
	}
	bestColumns := candidateCount
	bestRows := 1
	var bestScore [2]int
	found := false
	for columns := 1; columns <= candidateCount; columns++ {
		rows := int(math.Ceil(float64(candidateCount) / float64(columns)))
		emptySlots := rows*columns - candidateCount
		aspectGap := columns - rows
		if aspectGap < 0 {
			aspectGap = -aspectGap
		}
		score := [2]int{aspectGap, emptySlots}
		if !found || scoreLess(score, bestScore) {
			found = true
			bestScore = score
			bestColumns = columns
			bestRows = rows
		}
	}
	return bestRows, bestColumns
}

func scoreLess(a, b [2]int) bool {
	if a[0] != b[0] {
		return a[0] < b[0]
	}
	return a[1] < b[1]
}

// buildPlaceholderTile mirrors _build_placeholder_label: 图片读取失败时的
// 浅灰占位图（Python 用 PIL 默认字体居中画序号，Go 版用 basicfont）。
func buildPlaceholderTile(label string, tileSize int) *image.RGBA {
	tile := image.NewRGBA(image.Rect(0, 0, tileSize, tileSize))
	draw.Draw(tile, tile.Bounds(), &image.Uniform{color.RGBA{245, 245, 245, 255}}, image.Point{}, draw.Src)
	drawCenteredLabel(tile, label, color.RGBA{80, 80, 80, 255}, basicfont.Face7x13)
	return tile
}

// buildLabeledTile mirrors _build_labeled_tile: 缩放进 256px 白底格子并在
// 左上角画黑色半透明圆角序号角标。
// S6：全量解码前先校验声明尺寸（image.Decode 会按声明宽高分配画布，
// 超限声明可触发 fatal OOM）——超限/解析失败一律走占位图。
func buildLabeledTile(imageBytes []byte, index int, tileSize int) *image.RGBA {
	tile := image.NewRGBA(image.Rect(0, 0, tileSize, tileSize))
	draw.Draw(tile, tile.Bounds(), &image.Uniform{color.RGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)

	if err := checkImageSizeLimit(imageBytes); err != nil {
		slog.Warn("拼图候选图声明尺寸超限，使用占位图", "index", index, "err", err)
		return buildPlaceholderTile(fmt.Sprintf("%d", index), tileSize)
	}
	src, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return buildPlaceholderTile(fmt.Sprintf("%d", index), tileSize)
	}

	// image.thumbnail((tile_size, tile_size)) 等比缩小放进格子。
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW > tileSize || srcH > tileSize {
		scale := math.Min(float64(tileSize)/float64(srcW), float64(tileSize)/float64(srcH))
		dstW := int(float64(srcW) * scale)
		dstH := int(float64(srcH) * scale)
		if dstW < 1 {
			dstW = 1
		}
		if dstH < 1 {
			dstH = 1
		}
		src = resizeNearest(src, dstW, dstH)
		bounds = src.Bounds()
	}
	offsetX := (tileSize - bounds.Dx()) / 2
	offsetY := (tileSize - bounds.Dy()) / 2
	draw.Draw(tile, image.Rect(offsetX, offsetY, offsetX+bounds.Dx(), offsetY+bounds.Dy()),
		src, bounds.Min, draw.Src)

	// 左上角序号角标：黑色半透明圆角矩形 + 白色序号。
	label := fmt.Sprintf("%d", index)
	// basicfont.Face7x13 字符宽 7px；文字扩宽语义对齐 Python 的 while 循环：
	// 角标宽度随文字扩宽，超长编号缩小字体（basicfont 只有固定字号，改为
	// 宽度按字符数自适应，超长时保持角标内可容纳）。
	textWidth := len(label) * 7
	badgeWidth := IndexBadgeSize
	if badgeWidth < textWidth+12 {
		badgeWidth = textWidth + 12
	}
	drawRoundedRect(tile,
		image.Rect(badgeMargin, badgeMargin, badgeMargin+badgeWidth, badgeMargin+IndexBadgeSize),
		badgeRadius, color.RGBA{0, 0, 0, 180})
	drawLabel(tile, label,
		badgeMargin+(badgeWidth-textWidth)/2,
		badgeMargin+(IndexBadgeSize-13)/2,
		color.RGBA{255, 255, 255, 255})
	return tile
}

// MergeEmojiTiles mirrors _merge_emoji_tiles: 将候选表情图拼接成一张尽量
// 接近矩形的网格 PNG。startIndex 对应第一张图的序号（默认 1）。
func MergeEmojiTiles(imageBytesList [][]byte, startIndex int) ([]byte, error) {
	if startIndex <= 0 {
		startIndex = 1
	}
	tileSize := CollageTileSize
	candidateCount := len(imageBytesList)
	gridRows, gridColumns := CalculateGridShape(candidateCount)

	canvasWidth := tileSize*gridColumns + collageGap*(gridColumns-1)
	canvasHeight := tileSize*gridRows + collageGap*(gridRows-1)
	canvas := image.NewRGBA(image.Rect(0, 0, canvasWidth, canvasHeight))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{255, 255, 255, 255}}, image.Point{}, draw.Src)

	for offset, imageBytes := range imageBytesList {
		tile := buildLabeledTile(imageBytes, startIndex+offset, tileSize)
		row := offset / gridColumns
		column := offset % gridColumns
		offsetX := column * (tileSize + collageGap)
		offsetY := row * (tileSize + collageGap)
		draw.Draw(canvas, image.Rect(offsetX, offsetY, offsetX+tileSize, offsetY+tileSize),
			tile, image.Point{}, draw.Src)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return nil, fmt.Errorf("emoji: 编码拼图 PNG 失败: %w", err)
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// 绘图辅助
// ---------------------------------------------------------------------------

// resizeNearest 是等比缩放的最近邻实现（拼图场景无需高质量重采样）。
func resizeNearest(src image.Image, dstW, dstH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	for y := 0; y < dstH; y++ {
		srcY := bounds.Min.Y + y*srcH/dstH
		for x := 0; x < dstW; x++ {
			srcX := bounds.Min.X + x*srcW/dstW
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}
	return dst
}

// drawRoundedRect 画圆角矩形（radius <= 0 时退化为直角）。
func drawRoundedRect(dst *image.RGBA, rect image.Rectangle, radius int, fill color.Color) {
	if radius <= 0 {
		draw.Draw(dst, rect, &image.Uniform{fill}, image.Point{}, draw.Src)
		return
	}
	w, h := rect.Dx(), rect.Dy()
	if radius*2 > w {
		radius = w / 2
	}
	if radius*2 > h {
		radius = h / 2
	}
	r2 := radius * radius
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			// 四角做圆角判定，其余区域直接填充。
			cornerX, cornerY := -1, -1
			if x < rect.Min.X+radius && y < rect.Min.Y+radius {
				cornerX, cornerY = rect.Min.X+radius-x-1, rect.Min.Y+radius-y-1
			} else if x >= rect.Max.X-radius && y < rect.Min.Y+radius {
				cornerX, cornerY = x-(rect.Max.X-radius), rect.Min.Y+radius-y-1
			} else if x < rect.Min.X+radius && y >= rect.Max.Y-radius {
				cornerX, cornerY = rect.Min.X+radius-x-1, y-(rect.Max.Y-radius)
			} else if x >= rect.Max.X-radius && y >= rect.Max.Y-radius {
				cornerX, cornerY = x-(rect.Max.X-radius), y-(rect.Max.Y-radius)
			}
			if cornerX >= 0 && cornerY >= 0 && cornerX*cornerX+cornerY*cornerY > r2 {
				continue
			}
			dst.Set(x, y, fill)
		}
	}
}

// drawLabel 在指定基线位置绘制单行文字。
func drawLabel(dst *image.RGBA, label string, x, y int, clr color.Color) {
	drawer := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(clr),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y+13),
	}
	drawer.DrawString(label)
}

// drawCenteredLabel 在图块中心绘制文字（占位图用）。
func drawCenteredLabel(dst *image.RGBA, label string, clr color.Color, face font.Face) {
	textWidth := len(label) * 7
	textHeight := 13
	x := (dst.Bounds().Dx() - textWidth) / 2
	y := (dst.Bounds().Dy() - textHeight) / 2
	drawer := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot:  fixed.P(x, y+textHeight),
	}
	drawer.DrawString(label)
}
