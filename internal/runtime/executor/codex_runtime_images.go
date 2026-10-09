package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *CodexRuntimeExecutor) executeImageViaMaster(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter, body []byte, imageAPI string, prepared codexOpenAIImagePreparedRequest) (resp coreexecutor.Response, err error) {
	ctx = helps.WithCodexOaiLBReporter(ctx, reporter)
	upstreamLog := helps.NewCodexRuntimeLog(ctx, e.cfg, auth, req.Model)
	defer upstreamLog.Flush()
	defer func() {
		if err != nil {
			upstreamLog.Error(err)
		}
	}()
	client, err := e.startOperation(ctx, auth, req, opts, body, upstreamLog, imageAPI)
	if err != nil {
		return resp, err
	}
	defer client.Close()
	reader := &bridge.BodyReader{Client: client}
	if !strings.Contains(client.Headers.Get("Content-Type"), "text/event-stream") {
		data, errRead := io.ReadAll(reader)
		if errRead != nil {
			return resp, errRead
		}
		data, err = e.translateImageJSON(ctx, reporter, body, data, imageAPI, prepared)
		if err != nil {
			return resp, err
		}
		reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
		reporter.EnsurePublished(ctx)
		return coreexecutor.Response{Payload: data, Headers: client.Headers}, nil
	}
	sse := bridge.NewSSEReader(reader)
	items := make(map[int64][]byte)
	var fallback [][]byte
	var terminal []byte
	var images []json.RawMessage
	imageResult := []byte(`{"created":0}`)
	for {
		frame, errNext := sse.Next()
		if errNext == io.EOF {
			break
		}
		if errNext != nil {
			return resp, errNext
		}
		event := frame.Data
		upstreamLog.Event(event)
		if failure, failureBody, ok := codexTerminalFailureErrWithCooling(event, e.cfg.Codex.ModelLevelCooling); ok {
			return resp, &bridge.Error{Status: failure.StatusCode(), Message: failure.Error(), Body: failureBody, ResponseHeaders: client.Headers.Clone()}
		}
		kind := gjson.GetBytes(event, "type").String()
		if imageAPI == "images" {
			if kind == "image_generation.completed" || kind == "image_edit.completed" {
				item, _ := sjson.DeleteBytes(event, "type")
				for _, key := range []string{"created", "created_at", "usage", "background", "size", "quality", "output_format"} {
					if value := gjson.GetBytes(event, key); value.Exists() {
						target := key
						if target == "created_at" {
							target = "created"
						}
						imageResult, _ = sjson.SetRawBytes(imageResult, target, []byte(value.Raw))
						item, _ = sjson.DeleteBytes(item, key)
					}
				}
				images = append(images, item)
			}
			continue
		}
		switch kind {
		case "response.output_item.done":
			collectCodexOutputItemDone(event, items, &fallback)
		case "response.completed":
			terminal = bytes.Clone(event)
		}
	}
	var output []byte
	if imageAPI == "images" {
		if len(images) == 0 {
			return resp, runtimeError(502, "Image stream ended without a completed image")
		}
		data, errMarshal := json.Marshal(images)
		if errMarshal != nil {
			return resp, errMarshal
		}
		output, err = sjson.SetRawBytes(imageResult, "data", data)
		reporter.Publish(ctx, helps.ParseOpenAIUsage(output))
	} else {
		if len(terminal) == 0 {
			return resp, runtimeError(502, "Image Responses stream ended without completion")
		}
		results, created, usage, meta, errExtract := codexExtractImageResults(terminal, items, fallback)
		if errExtract != nil {
			return resp, errExtract
		}
		if len(results) == 0 {
			return resp, runtimeError(502, "Upstream did not return image output")
		}
		output, err = codexBuildImagesAPIResponse(results, created, usage, meta, prepared.ResponseFormat)
		if detail, ok := helps.ParseCodexUsage(terminal); ok {
			reporter.Publish(ctx, detail)
		}
		publishCodexImageToolUsage(ctx, reporter, body, terminal)
	}
	if err != nil {
		return resp, err
	}
	reporter.EnsurePublished(ctx)
	client.Headers.Set("Content-Type", "application/json")
	return coreexecutor.Response{Payload: output, Headers: client.Headers}, nil
}

func (e *CodexRuntimeExecutor) executeImageStreamViaMaster(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options, reporter *helps.UsageReporter, body []byte, imageAPI string, prepared codexOpenAIImagePreparedRequest) (*coreexecutor.StreamResult, error) {
	reporter.SetStream(true)
	ctx = helps.WithCodexOaiLBReporter(ctx, reporter)
	upstreamLog := helps.NewCodexRuntimeLog(ctx, e.cfg, auth, req.Model)
	client, err := e.startOperation(ctx, auth, req, opts, body, upstreamLog, imageAPI)
	if err != nil {
		upstreamLog.Error(err)
		return nil, err
	}
	chunks := make(chan coreexecutor.StreamChunk)
	go func() {
		defer close(chunks)
		defer client.Close()
		defer upstreamLog.Flush()
		send := func(data []byte, err error) bool {
			if err != nil {
				upstreamLog.Error(err)
				reporter.PublishFailure(ctx, err)
			}
			select {
			case chunks <- coreexecutor.StreamChunk{Payload: data, Err: err}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		if !strings.Contains(client.Headers.Get("Content-Type"), "text/event-stream") {
			data, errRead := io.ReadAll(&bridge.BodyReader{Client: client})
			if errRead != nil {
				send(nil, errRead)
				return
			}
			data, errRead = e.translateImageJSON(ctx, reporter, body, data, imageAPI, prepared)
			if errRead != nil {
				send(nil, errRead)
				return
			}
			if !json.Valid(data) || !gjson.GetBytes(data, "data").IsArray() {
				send(nil, runtimeError(502, "Invalid image JSON response from Codex runtime"))
				return
			}
			kind := "image_generation.completed"
			if strings.HasSuffix(helps.PayloadRequestPath(opts), codexImagesEditsPath) {
				kind = "image_edit.completed"
			}
			for _, item := range gjson.GetBytes(data, "data").Array() {
				event, _ := sjson.SetBytes([]byte(item.Raw), "type", kind)
				for _, key := range []string{"created", "usage", "background", "size", "quality", "output_format"} {
					if value := gjson.GetBytes(data, key); value.Exists() {
						target := key
						if target == "created" {
							target = "created_at"
						}
						event, _ = sjson.SetRawBytes(event, target, []byte(value.Raw))
					}
				}
				if !send(codexBuildSSEFrame(kind, event), nil) {
					return
				}
			}
			reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
			reporter.EnsurePublished(ctx)
			return
		}
		reader := bridge.NewSSEReader(&bridge.BodyReader{Client: client})
		items := make(map[int64][]byte)
		var fallback [][]byte
		terminal := false
		for {
			frame, errNext := reader.Next()
			if errNext == io.EOF {
				if !terminal {
					send(nil, runtimeError(502, "Image stream ended without completion"))
				} else {
					reporter.EnsurePublished(ctx)
				}
				return
			}
			if errNext != nil {
				send(nil, errNext)
				return
			}
			event := frame.Data
			upstreamLog.Event(event)
			if failure, failureBody, ok := codexTerminalFailureErrWithCooling(event, e.cfg.Codex.ModelLevelCooling); ok {
				send(nil, &bridge.Error{Status: failure.StatusCode(), Message: failure.Error(), Body: failureBody, ResponseHeaders: client.Headers.Clone()})
				return
			}
			kind := gjson.GetBytes(event, "type").String()
			if imageAPI == "images" {
				terminal = terminal || kind == "image_generation.completed" || kind == "image_edit.completed"
				if kind == "image_generation.completed" || kind == "image_edit.completed" {
					reporter.Publish(ctx, helps.ParseOpenAIUsage(event))
				}
				if !send(frame.Wire, nil) {
					return
				}
				continue
			}
			switch kind {
			case "response.output_item.done":
				collectCodexOutputItemDone(event, items, &fallback)
			case "response.image_generation_call.partial_image":
				if payload := codexBuildImagePartialFrame(event, prepared.ResponseFormat, prepared.StreamPrefix); len(payload) > 0 && !send(payload, nil) {
					return
				}
			case "response.completed":
				terminal = true
				results, _, usage, _, errExtract := codexExtractImageResults(event, items, fallback)
				if errExtract != nil {
					send(nil, errExtract)
					return
				}
				if len(results) == 0 {
					send(nil, runtimeError(502, "Upstream did not return image output"))
					return
				}
				if detail, ok := helps.ParseCodexUsage(event); ok {
					reporter.Publish(ctx, detail)
				}
				publishCodexImageToolUsage(ctx, reporter, body, event)
				for _, img := range results {
					if !send(codexBuildImageCompletedFrame(img, usage, prepared.ResponseFormat, prepared.StreamPrefix), nil) {
						return
					}
				}
			}
		}
	}()
	headers := client.Headers.Clone()
	headers.Set("Content-Type", "text/event-stream")
	return &coreexecutor.StreamResult{Headers: headers, Chunks: chunks}, nil
}

// translateImageJSON converts worker JSON into the public Images response format.
func (e *CodexRuntimeExecutor) translateImageJSON(ctx context.Context, reporter *helps.UsageReporter, body, data []byte, imageAPI string, prepared codexOpenAIImagePreparedRequest) ([]byte, error) {
	if !json.Valid(data) {
		return nil, runtimeError(502, "Invalid image JSON response from Codex runtime")
	}
	if imageAPI == "images" {
		return data, nil
	}
	terminal, err := json.Marshal(map[string]any{"type": "response.completed", "response": json.RawMessage(data)})
	if err != nil {
		return nil, err
	}
	if gjson.GetBytes(data, "status").String() == "failed" {
		terminal, _ = sjson.SetBytes(terminal, "type", "response.failed")
		if failure, failureBody, ok := codexTerminalFailureErrWithCooling(terminal, e.cfg.Codex.ModelLevelCooling); ok {
			return nil, &bridge.Error{Status: failure.StatusCode(), Message: failure.Error(), Body: failureBody}
		}
	}
	results, created, usage, meta, err := codexExtractImageResults(terminal, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, runtimeError(502, "Upstream did not return image output")
	}
	if detail, ok := helps.ParseCodexUsage(terminal); ok {
		reporter.Publish(ctx, detail)
	}
	publishCodexImageToolUsage(ctx, reporter, body, terminal)
	return codexBuildImagesAPIResponse(results, created, usage, meta, prepared.ResponseFormat)
}
