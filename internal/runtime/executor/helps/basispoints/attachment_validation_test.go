package basispoints

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAttachmentSupportedFormatsAndFilenames(t *testing.T) {
	var jpg, gifData bytes.Buffer
	if err := jpeg.Encode(&jpg, image.NewRGBA(image.Rect(0, 0, 1, 1)), nil); err != nil {
		t.Fatal(err)
	}
	if err := gif.Encode(&gifData, image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black}), nil); err != nil {
		t.Fatal(err)
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		declared, canonical, extension string
		data                           []byte
	}{
		{"image/png", "image/png", ".png", attachmentTestPNG(t)},
		{"image/x-png", "image/png", ".png", attachmentTestPNG(t)},
		{"image/jpeg", "image/jpeg", ".jpg", jpg.Bytes()},
		{"image/jpg", "image/jpeg", ".jpg", jpg.Bytes()},
		{"image/pjpeg", "image/jpeg", ".jpg", jpg.Bytes()},
		{"image/gif", "image/gif", ".gif", gifData.Bytes()},
		{"image/webp", "image/webp", ".webp", webp},
	} {
		t.Run(tc.declared, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: attachmentTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				reader, err := r.MultipartReader()
				if err != nil {
					t.Fatal(err)
				}
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(part)
				if err != nil || !bytes.Equal(data, tc.data) || part.FileName() != "image"+tc.extension || part.Header.Get("Content-Type") != tc.canonical {
					t.Fatalf("invalid upload: file=%s type=%s err=%v", part.FileName(), part.Header.Get("Content-Type"), err)
				}
				result, _ := json.Marshal(object{"openai_file_id": "uploaded-image"})
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(result))}, nil
			})}
			dataURL := "data:" + tc.declared + ";base64," + base64.StdEncoding.EncodeToString(tc.data)
			for _, toolResult := range []bool{false, true} {
				part := object{"type": "input_image", "image_url": dataURL, "detail": "high"}
				item := object{"role": "user", "content": []any{part}}
				path := "input.0.content.0"
				if toolResult {
					item = object{"type": "function_call_output", "call_id": "tool", "output": []any{part}}
					path = "input.0.output.0"
				}
				raw, _ := json.Marshal(object{"input": []any{item}})
				wire, err := UploadInputImages(t.Context(), client, raw, make(http.Header), "", nil)
				if err != nil {
					t.Fatal(err)
				}
				result := gjson.GetBytes(wire, path)
				if toolResult {
					want := "data:" + tc.canonical + ";base64," + base64.StdEncoding.EncodeToString(tc.data)
					if result.Get("image_url").String() != want || result.Get("file_id").Exists() {
						t.Fatal("tool image representation changed")
					}
				} else if result.Get("file_id").String() != "uploaded-image" || result.Get("image_url").Exists() {
					t.Fatal("user image was not uploaded")
				}
				if result.Get("detail").String() != "high" {
					t.Fatal("image detail changed")
				}
			}
			if calls != 1 {
				t.Fatalf("uploads=%d", calls)
			}
		})
	}
}

func TestInvalidImagesFailBeforeAnyUpload(t *testing.T) {
	valid := "data:image/png;base64," + base64.StdEncoding.EncodeToString(attachmentTestPNG(t))
	for _, invalid := range []string{
		"data:image/png;base64,eA==",
		"data:image/unknown;base64,eA==",
		"data:image/svg+xml,%3Csvg%3E%3C/svg%3E",
		"data:image/bmp;base64,Qk0=",
		strings.Replace(valid, "image/png", "image/jpeg", 1),
	} {
		for _, toolResult := range []bool{false, true} {
			calls := 0
			client := &http.Client{Transport: attachmentTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("upload must not run") })}
			bad := object{"type": "input_image", "image_url": invalid}
			input := []any{object{"role": "user", "content": []any{object{"type": "input_image", "image_url": valid}}}}
			if toolResult {
				input = append(input, object{"type": "function_call_output", "call_id": "tool", "output": []any{bad}})
			} else {
				input = append(input, object{"role": "user", "content": []any{bad}})
			}
			raw, _ := json.Marshal(object{"input": input})
			_, err := UploadInputImages(t.Context(), client, raw, make(http.Header), "", nil)
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != 400 || calls != 0 {
				t.Fatalf("invalid image reached upstream: err=%v calls=%d", err, calls)
			}
		}
	}
}
