package basispoints

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const AttachmentsURL = "https://bps.openai.com/basispoints/api/attachments"
const maxAttachmentEntries = 512

// AttachmentCache stores only digests and upstream file IDs. It never selects
// credentials: a newly selected account uploads its own copy of the image.
type AttachmentCache struct {
	mu      sync.Mutex
	items   map[string]*list.Element
	order   list.List
	pending map[string]*attachmentUpload
}

type attachmentEntry struct{ key, fileID string }
type attachmentUpload struct {
	done   chan struct{}
	fileID string
	err    error
}

var SharedAttachments AttachmentCache

func (c *AttachmentCache) getOrUpload(ctx context.Context, key string, upload func() (string, error)) (string, error) {
	c.mu.Lock()
	if entry := c.items[key]; entry != nil {
		c.order.MoveToFront(entry)
		fileID := entry.Value.(attachmentEntry).fileID
		c.mu.Unlock()
		return fileID, nil
	}
	if pending := c.pending[key]; pending != nil {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-pending.done:
			return pending.fileID, pending.err
		}
	}
	if c.pending == nil {
		c.pending = make(map[string]*attachmentUpload)
	}
	pending := &attachmentUpload{done: make(chan struct{})}
	c.pending[key] = pending
	c.mu.Unlock()
	fileID, err := upload()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, key)
	if err == nil {
		if c.items == nil {
			c.items = make(map[string]*list.Element)
		}
		c.items[key] = c.order.PushFront(attachmentEntry{key: key, fileID: fileID})
		if c.order.Len() > maxAttachmentEntries {
			oldest := c.order.Back()
			delete(c.items, oldest.Value.(attachmentEntry).key)
			c.order.Remove(oldest)
		}
	}
	pending.fileID, pending.err = fileID, err
	close(pending.done)
	return fileID, err
}

// UploadInputImages replaces inline images in user messages after Prepare has
// computed task/turn metadata. Tool-result images remain in their native inline
// form; text, HTTPS images, existing file IDs and detail survive.
func UploadInputImages(ctx context.Context, client *http.Client, raw []byte, headers http.Header, scope string, cache *AttachmentCache) ([]byte, error) {
	body, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if cache == nil {
		cache = &AttachmentCache{}
	}
	type inlineUpload struct {
		part      object
		mediaType string
		data      []byte
	}
	var uploads []inlineUpload
	changed := false
	items, _ := body["input"].([]any)
	for _, value := range items {
		item, _ := value.(object)
		userMessage := item["role"] == "user" && (item["type"] == nil || item["type"] == "" || item["type"] == "message")
		parts, _ := item["content"].([]any)
		if item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output" {
			parts, _ = item["output"].([]any)
		}
		for _, value := range parts {
			part, _ := value.(object)
			imageURL := stringValue(part["image_url"])
			if part["type"] != "input_image" || len(imageURL) < 5 || !strings.EqualFold(imageURL[:5], "data:") {
				continue
			}
			if stringValue(part["file_id"]) != "" {
				return nil, failure(400, "invalid_image", "input_image cannot contain both inline image_url and file_id")
			}
			mediaType, data, errDecode := decodeInlineImage(imageURL)
			if errDecode != nil {
				return nil, errDecode
			}
			if userMessage {
				uploads = append(uploads, inlineUpload{part: part, mediaType: mediaType, data: data})
			} else if end := 5 + strings.IndexAny(imageURL[5:], ";,"); imageURL[5:end] != mediaType {
				part["image_url"] = "data:" + mediaType + imageURL[end:]
				changed = true
			}
		}
	}
	if len(uploads) == 0 && !changed {
		return raw, nil
	}
	// Validate the whole request before uploading anything. Tool-result images
	// are checked too, but keep their existing inline representation.
	for _, image := range uploads {
		imageHash := sha256.Sum256(image.data)
		// Cache identity never changes credential selection or account rotation.
		key := Scope(scope, headers.Get("Chatgpt-Account-Id"), headers.Get("Authorization"), image.mediaType, fmt.Sprintf("%x", imageHash))
		fileID, errUpload := cache.getOrUpload(ctx, key, func() (string, error) {
			return uploadImage(ctx, client, headers, image.mediaType, image.data)
		})
		if errUpload != nil {
			return nil, errUpload
		}
		delete(image.part, "image_url")
		image.part["file_id"] = fileID
		if _, exists := image.part["detail"]; !exists {
			image.part["detail"] = "auto"
		}
	}
	return json.Marshal(body)
}

func decodeInlineImage(raw string) (string, []byte, error) {
	metadata, encoded, found := strings.Cut(raw[5:], ",")
	if !found {
		return "", nil, failure(400, "invalid_image", "input_image data URL is missing its data separator")
	}
	base64Encoded := strings.HasSuffix(strings.ToLower(metadata), ";base64")
	if base64Encoded {
		metadata = metadata[:len(metadata)-len(";base64")]
	}
	mediaType, _, err := mime.ParseMediaType(metadata)
	if err != nil || !strings.HasPrefix(mediaType, "image/") {
		return "", nil, failure(400, "invalid_image", "input_image data URL must declare an image media type")
	}
	switch mediaType {
	case "image/jpg", "image/pjpeg":
		mediaType = "image/jpeg"
	case "image/x-png":
		mediaType = "image/png"
	}
	if imageExtension(mediaType) == "" {
		return "", nil, failure(400, "invalid_image", "input_image supports only JPEG, PNG, GIF and WebP")
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		return "", nil, failure(400, "invalid_image", "input_image data URL has invalid percent encoding")
	}
	data := []byte(decoded)
	if base64Encoded {
		data, err = base64.StdEncoding.DecodeString(decoded)
	}
	if err != nil || len(data) == 0 {
		return "", nil, failure(400, "invalid_image", "input_image data URL contains empty or invalid image data")
	}
	if http.DetectContentType(data) != mediaType {
		return "", nil, failure(400, "invalid_image", "input_image bytes do not match the declared image format")
	}
	return mediaType, data, nil
}

// Do not use the host MIME database: it may prefer .jpe or lack an extension.
func imageExtension(mediaType string) string {
	switch mediaType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

func uploadImage(ctx context.Context, client *http.Client, headers http.Header, mediaType string, data []byte) (string, error) {
	extension := imageExtension(mediaType)
	if extension == "" {
		return "", failure(400, "invalid_image", "input_image supports only JPEG, PNG, GIF and WebP")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	filename := "image" + extension
	partHeaders := make(textproto.MIMEHeader)
	partHeaders.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": filename}))
	partHeaders.Set("Content-Type", mediaType)
	part, err := writer.CreatePart(partHeaders)
	if err != nil {
		return "", fmt.Errorf("encode Basispoints attachment: %w", err)
	}
	if _, err = part.Write(data); err != nil {
		return "", fmt.Errorf("write Basispoints attachment: %w", err)
	}
	if err = writer.Close(); err != nil {
		return "", fmt.Errorf("finish Basispoints attachment: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, AttachmentsURL, &body)
	if err != nil {
		return "", err
	}
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("Basispoints attachment upload: %w", err)
	}
	defer func() {
		if errClose := response.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("basispoints: close attachment response")
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxItemBytes))
	if err != nil {
		return "", fmt.Errorf("read Basispoints attachment response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := string(raw)
		for _, secret := range []string{strings.TrimPrefix(headers.Get("Authorization"), "Bearer "), headers.Get("Chatgpt-Account-Id"), base64.StdEncoding.EncodeToString(data), string(data)} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[REDACTED]")
			}
		}
		return "", failure(response.StatusCode, "attachment_upload_error", fmt.Sprintf("Basispoints attachment upload HTTP %d: %s", response.StatusCode, message))
	}
	var result struct {
		FileID string `json:"openai_file_id"`
	}
	if json.Unmarshal(raw, &result) != nil || strings.TrimSpace(result.FileID) == "" {
		return "", failure(502, "invalid_attachment_response", "Basispoints attachment upload returned no openai_file_id")
	}
	return strings.TrimSpace(result.FileID), nil
}
