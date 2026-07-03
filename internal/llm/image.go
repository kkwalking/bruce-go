package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

const (
	APIImageMaxBase64Size = 5 * 1024 * 1024
	MaxImageDimension     = 2000
)

type PreparedInput struct {
	Text    string
	Message Message
}

type ClipboardReader func(ctx context.Context) ([]byte, string, error)

func ParseImageReferences(ctx context.Context, input, workspaceRoot string, clipboard ClipboardReader) (PreparedInput, error) {
	if !strings.Contains(input, "@image:") && !strings.Contains(input, "@clipboard") {
		return PreparedInput{Text: input, Message: User(input)}, nil
	}
	var text strings.Builder
	var parts []ContentPart
	for cursor := 0; cursor < len(input); {
		imageIndex := strings.Index(input[cursor:], "@image:")
		clipIndex := findClipboard(input, cursor)
		if imageIndex >= 0 {
			imageIndex += cursor
		}
		next := earliest(imageIndex, clipIndex)
		if next < 0 {
			text.WriteString(input[cursor:])
			break
		}
		text.WriteString(input[cursor:next])
		if next == imageIndex {
			ref, end, err := readImageRef(input, next)
			if err != nil {
				return PreparedInput{}, err
			}
			path, err := resolveImagePath(ref, workspaceRoot)
			if err != nil {
				return PreparedInput{}, err
			}
			part, err := ProcessImageFile(path)
			if err != nil {
				return PreparedInput{}, err
			}
			parts = append(parts, part)
			cursor = end
			continue
		}
		if clipboard == nil {
			clipboard = ReadClipboardPNG
		}
		data, mimeType, err := clipboard(ctx)
		if err != nil {
			return PreparedInput{}, err
		}
		part, err := ProcessImageBytes(data, mimeType, "clipboard")
		if err != nil {
			return PreparedInput{}, err
		}
		parts = append(parts, part)
		cursor = next + len("@clipboard")
	}
	prompt := strings.Join(strings.Fields(text.String()), " ")
	if prompt != "" {
		parts = append([]ContentPart{TextPart(prompt)}, parts...)
	}
	return PreparedInput{Text: prompt, Message: UserParts(parts)}, nil
}

func ProcessImageFile(path string) (ContentPart, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ContentPart{}, err
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mimeType == "" {
		mimeType = "image/png"
	}
	return ProcessImageBytes(data, mimeType, path)
}

func ProcessImageBytes(data []byte, mimeType, source string) (ContentPart, error) {
	if len(data) == 0 {
		return ContentPart{}, errors.New("图片内容为空: " + source)
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err == nil && mimeType == "" {
		mimeType = "image/" + format
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	if err == nil && estimateBase64(len(data)) <= APIImageMaxBase64Size && !hasAlpha(img) {
		return ImagePart("data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(data), mimeType, source), nil
	}
	if err != nil {
		return ContentPart{}, err
	}
	rgb := toRGB(img)
	rgb = fitImage(rgb, MaxImageDimension)
	var out bytes.Buffer
	if err := png.Encode(&out, rgb); err == nil && estimateBase64(out.Len()) <= APIImageMaxBase64Size {
		return ImagePart("data:image/png;base64,"+base64.StdEncoding.EncodeToString(out.Bytes()), "image/png", source), nil
	}
	for _, quality := range []int{85, 70, 55, 40, 25} {
		out.Reset()
		if err := jpeg.Encode(&out, rgb, &jpeg.Options{Quality: quality}); err == nil && estimateBase64(out.Len()) <= APIImageMaxBase64Size {
			return ImagePart("data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(out.Bytes()), "image/jpeg", source), nil
		}
	}
	return ContentPart{}, errors.New("图片压缩后仍超过限制: " + source)
}

func ReadClipboardPNG(ctx context.Context) ([]byte, string, error) {
	tmp, err := os.CreateTemp("", "bruce-clipboard-*.png")
	if err != nil {
		return nil, "", err
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)
	script := `on run argv
set outputPath to item 1 of argv
set pngData to (the clipboard as «class PNGf»)
set fh to open for access (POSIX file outputPath as string) with write permission
set eof of fh to 0
write pngData to fh
close access fh
end run`
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-", path)
	cmd.Stdin = strings.NewReader(script)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, "", errors.New("剪贴板图片读取失败: " + out.String())
	}
	data, err := os.ReadFile(path)
	return data, "image/png", err
}

func readImageRef(input string, start int) (string, int, error) {
	pos := start + len("@image:")
	if pos >= len(input) {
		return "", 0, errors.New("@image: 后缺少图片路径")
	}
	if input[pos] == '<' {
		end := strings.IndexByte(input[pos+1:], '>')
		if end < 0 {
			return "", 0, errors.New("@image:<...> 缺少结束的 >")
		}
		ref := strings.TrimSpace(input[pos+1 : pos+1+end])
		if ref == "" {
			return "", 0, errors.New("@image:<...> 中的图片路径不能为空")
		}
		return ref, pos + 1 + end + 1, nil
	}
	end := pos
	for end < len(input) && !isSpace(input[end]) {
		end++
	}
	ref := strings.TrimSpace(input[pos:end])
	if ref == "" {
		return "", 0, errors.New("@image: 后缺少图片路径")
	}
	return ref, end, nil
}

func resolveImagePath(ref, root string) (string, error) {
	var path string
	if strings.HasPrefix(ref, "file://") {
		u, err := url.Parse(ref)
		if err != nil {
			return "", errors.New("非法 file:// 图片路径: " + ref)
		}
		path = u.Path
	} else {
		path = ref
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", errors.New("图片文件不存在或不是普通文件: " + path)
	}
	return path, nil
}

func findClipboard(input string, from int) int {
	idx := strings.Index(input[from:], "@clipboard")
	for idx >= 0 {
		idx += from
		end := idx + len("@clipboard")
		if end >= len(input) || isBoundary(input[end]) {
			return idx
		}
		next := strings.Index(input[end:], "@clipboard")
		if next < 0 {
			return -1
		}
		from = end
		idx = next
	}
	return -1
}

func earliest(a, b int) int {
	if a < 0 {
		return b
	}
	if b < 0 || a < b {
		return a
	}
	return b
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }
func isBoundary(b byte) bool {
	return isSpace(b) || strings.ContainsRune(".,;:!?)]}", rune(b))
}

func hasAlpha(img image.Image) bool {
	if img == nil {
		return false
	}
	_, ok := img.(*image.NRGBA)
	if ok {
		return true
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, a := img.At(x, y).RGBA()
			if a != 0xffff {
				return true
			}
		}
	}
	return false
}

func toRGB(src image.Image) *image.RGBA {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

func fitImage(src *image.RGBA, max int) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w <= max && h <= max {
		return src
	}
	scale := float64(max) / float64(w)
	if float64(max)/float64(h) < scale {
		scale = float64(max) / float64(h)
	}
	nw, nh := int(float64(w)*scale), int(float64(h)*scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Over, nil)
	return dst
}

func estimateBase64(n int) int {
	return ((n + 2) / 3) * 4
}

func FromBase64(payload, mimeType, source string) (ContentPart, error) {
	value := strings.TrimSpace(payload)
	if strings.HasPrefix(value, "data:") {
		if comma := strings.Index(value, ","); comma >= 0 {
			if mimeType == "" {
				prefix := value[len("data:"):comma]
				if semi := strings.Index(prefix, ";"); semi >= 0 {
					mimeType = prefix[:semi]
				}
			}
			value = value[comma+1:]
		}
	}
	data, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)))
	if err != nil {
		return ContentPart{}, err
	}
	return ProcessImageBytes(data, mimeType, source)
}
