package llm

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseImageReferencesFileAndClipboard(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "shot.png")
	data := tinyPNG(t)
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	clipboard := func(context.Context) ([]byte, string, error) {
		return data, "image/png", nil
	}

	prepared, err := ParseImageReferences(context.Background(), "分析 @image:shot.png 和 @clipboard", dir, clipboard)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Text != "分析 和" {
		t.Fatalf("text = %q", prepared.Text)
	}
	if len(prepared.Message.ContentParts) != 3 {
		t.Fatalf("parts = %d", len(prepared.Message.ContentParts))
	}
	if prepared.Message.ContentParts[1].Type != ContentImageURL || !strings.HasPrefix(prepared.Message.ContentParts[1].ImageURL, "data:image/png;base64,") {
		t.Fatalf("first image part = %+v", prepared.Message.ContentParts[1])
	}
	if prepared.Message.ContentParts[2].Source != "clipboard" {
		t.Fatalf("clipboard source = %q", prepared.Message.ContentParts[2].Source)
	}
}

func TestFromBase64(t *testing.T) {
	part, err := FromBase64(strings.TrimPrefix(stringMustDataURL(tinyPNG(t)), "data:image/png;base64,"), "image/png", "inline")
	if err != nil {
		t.Fatal(err)
	}
	if part.Type != ContentImageURL || part.MIMEType != "image/png" {
		t.Fatalf("part = %+v", part)
	}
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func stringMustDataURL(data []byte) string {
	part, err := ProcessImageBytes(data, "image/png", "inline")
	if err != nil {
		panic(err)
	}
	return part.ImageURL
}
