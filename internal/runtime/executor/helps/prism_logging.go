package helps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// PrismRequestLog records transport diagnostics without credentials or business bodies.
type PrismRequestLog struct {
	ctx     context.Context
	cfg     *config.Config
	entry   *log.Entry
	started time.Time
}

func NewPrismRequestLog(ctx context.Context, cfg *config.Config, req *http.Request, payload []byte, authID string) *PrismRequestLog {
	fields := log.Fields{
		"route": "prism", "adapter_url": req.URL.String(), "auth_id": authID,
		"model":            gjson.GetBytes(payload, "model").String(),
		"reasoning_effort": gjson.GetBytes(payload, "reasoning.effort").String(),
		"stream":           gjson.GetBytes(payload, "stream").Bool(), "request_bytes": len(payload),
	}
	summary, _ := json.Marshal(map[string]any{
		"payload_omitted": true, "model": fields["model"], "reasoning_effort": fields["reasoning_effort"],
		"stream": fields["stream"], "request_bytes": len(payload),
	})
	// A header allowlist also excludes private identity headers and future credentials.
	RecordAPIRequest(ctx, cfg, UpstreamRequestLog{
		URL: req.URL.String(), Method: req.Method, Provider: "prism", AuthID: authID,
		Headers: http.Header{"Content-Type": []string{req.Header.Get("Content-Type")}}, Body: summary,
	})
	diagnostic := &PrismRequestLog{ctx: ctx, cfg: cfg, entry: LogWithRequestID(ctx).WithFields(fields), started: time.Now()}
	diagnostic.entry.WithField("event", "prism_request_start").Info("Prism adapter request started")
	return diagnostic
}

func (l *PrismRequestLog) Failure(stage string, err error) {
	detail := prismTransportError(err)
	if stage == "response_size" {
		detail = "response exceeded size limit"
	}
	RecordAPIResponseError(l.ctx, l.cfg, errors.New(stage+": "+detail))
	l.entry.WithFields(log.Fields{
		"event": "prism_request_failed", "stage": stage, "error": detail,
		"elapsed_ms": time.Since(l.started).Milliseconds(),
	}).Warn("Prism adapter request failed")
}

func (l *PrismRequestLog) ResponseHeaders(status int, headers http.Header) {
	l.entry = l.entry.WithField("status", status)
	RecordAPIResponseMetadata(l.ctx, l.cfg, status, http.Header{"Content-Type": []string{headers.Get("Content-Type")}})
}

func (l *PrismRequestLog) Response(status int, body, payload []byte) {
	fields := log.Fields{
		"event": "prism_response_received", "status": status, "response_bytes": len(body),
		"elapsed_ms": time.Since(l.started).Milliseconds(), "payload_omitted": true,
	}
	if status != http.StatusOK {
		code := gjson.GetBytes(body, "error.type").String()
		if len(code) > 64 || strings.IndexFunc(code, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_')
		}) >= 0 {
			code = "unknown"
		}
		fields["error_type"] = code
		if code == "invalid_tools" && gjson.GetBytes(body, "error.message").String() == "Tool description is invalid" {
			for key, value := range prismInvalidToolDescription(payload) {
				fields[key] = value
			}
		}
		l.entry.WithFields(fields).Warn("Prism adapter rejected the request")
	} else {
		l.entry.WithFields(fields).Info("Prism adapter response received")
	}
	summary, _ := json.Marshal(fields)
	AppendAPIResponseChunk(l.ctx, l.cfg, summary)
}

// Transport errors can contain a reflected response or invalid header value.
// Preserve known causes, never arbitrary error text from the peer or credentials.
func prismTransportError(err error) string {
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF, io.EOF, net.ErrClosed} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "network timeout"
	}
	if strings.Contains(err.Error(), "invalid header") {
		return "invalid HTTP header"
	}
	var requestError *url.Error
	if errors.As(err, &requestError) {
		err = requestError.Err
	}
	return fmt.Sprintf("transport error (%T)", err)
}

// This mirrors the adapter's description diagnostic, without validating or
// modifying dispatch. Python len(str) counts Unicode code points, not bytes.
func prismInvalidToolDescription(payload []byte) log.Fields {
	const descriptionLimit = 32000
	var visit func(gjson.Result, string, string, int) log.Fields
	visit = func(tools gjson.Result, path, namespace string, depth int) log.Fields {
		if !tools.IsArray() || depth > 8 {
			return nil
		}
		for i, tool := range tools.Array() {
			itemPath := fmt.Sprintf("%s.%d", path, i)
			name := tool.Get("name").String()
			if namespace != "" {
				name = namespace + "." + name
			}
			kind := tool.Get("type").String()
			if kind == "namespace" {
				if fields := visit(tool.Get("tools"), itemPath+".tools", name, depth+1); fields != nil {
					return fields
				}
				continue
			}
			if kind != "function" && kind != "custom" {
				continue
			}
			description := tool.Get("description")
			if !description.Exists() {
				continue
			}
			chars := utf8.RuneCountInString(description.String())
			if description.Type == gjson.String && chars <= descriptionLimit {
				continue
			}
			fields := log.Fields{
				"tool_name": name, "tool_type": kind, "tool_path": itemPath + ".description",
				"description_type":        strings.ToLower(description.Type.String()),
				"description_limit_chars": descriptionLimit,
			}
			if description.Type == gjson.String {
				fields["description_chars"] = chars
				fields["description_issue"] = "too_long"
			} else {
				fields["description_issue"] = "not_string"
				if description.IsArray() {
					fields["description_type"] = "array"
				} else if description.IsObject() {
					fields["description_type"] = "object"
				} else if description.Type == gjson.True || description.Type == gjson.False {
					fields["description_type"] = "boolean"
				}
			}
			return fields
		}
		return nil
	}
	root := gjson.ParseBytes(payload)
	for _, field := range []string{"tools", "additional_tools"} {
		if fields := visit(root.Get(field), field, "", 0); fields != nil {
			return fields
		}
	}
	for i, item := range root.Get("input").Array() {
		if item.Get("type").String() == "additional_tools" {
			if fields := visit(item.Get("tools"), fmt.Sprintf("input.%d.tools", i), "", 0); fields != nil {
				return fields
			}
		}
	}
	return nil
}
