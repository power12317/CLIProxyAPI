package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	if isCodexOpenAIImageRequest(opts) {
		return e.executeOpenAIImage(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("codex")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	officialCodexRequest := codexOfficialRequest(originalPayloadSource, req.Payload)
	isCompat := e.resolveCodexModelIsCompat(auth, req, baseModel)
	originalTranslated, body := translateCodexRequestPair(from, to, baseModel, originalPayload, req.Payload, false, opts.Headers, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.SetBoolIfDifferent(body, "stream", true)
	body, _ = sjson.DeleteBytes(body, "previous_response_id")
	body, _ = sjson.DeleteBytes(body, "generate")
	body, _ = sjson.DeleteBytes(body, "prompt_cache_retention")
	body, _ = sjson.DeleteBytes(body, "safety_identifier")
	body, _ = sjson.DeleteBytes(body, "stream_options")
	body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))
	toolHeaders := codexToolPolicyHeaders(auth, opts.Headers, baseModel)
	if gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").Exists() {
		body = ensureCodexResponsesLiteMirror(body, toolHeaders)
	}
	body, err = applyCodexImageGenerationPolicy(body, baseModel, auth, e.cfg, toolHeaders, requestPath, officialCodexRequest, originalPayloadSource)
	if err != nil {
		return resp, err
	}
	body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompat(ctx, "codex executor", body, isCompat)
	body = normalizeCodexParallelToolCalls(body, toolHeaders, officialCodexRequest)
	body = helps.NormalizeCodexToolSchemas(body)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, baseModel)
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, from, req, opts, body)
	if errReplay != nil {
		return resp, errReplay
	}
	var oauthIdentity helps.CodexOAuthIdentity
	officialOAuthRequest := false
	var fixedInstallationID string
	if helps.CodexAuthUsesOAuthCookieJar(auth) && helps.IsOfficialCodexRequest(body) {
		body, oauthIdentity, officialOAuthRequest = helps.ApplyCodexOAuthFidelity(body, codexInstallationAccountID(auth), codexInstallationCredentialSystem(auth), codexDeviceConvergenceEnabled(e.cfg))
	}
	reporter.SetTranslatedReasoningEffort(body, to.String())

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	var identityState codexIdentityConfuseState
	httpReq, upstreamBody, identityState, err := e.cacheHelper(ctx, from, url, auth, req, originalPayloadSource, body, opts.Headers)
	if err != nil {
		return resp, err
	}
	turnStateBody := upstreamBody
	upstreamBody = helps.ApplyCodexTurnStateTicketBody(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, upstreamBody)
	turnStateBody = upstreamBody
	if !officialOAuthRequest {
		upstreamBody, fixedInstallationID, _, _ = helps.ApplyCodexInstallationIdentity(upstreamBody, codexInstallationAccountID(auth), codexInstallationCredentialSystem(auth), codexDeviceConvergenceEnabled(e.cfg))
		replaceCodexRequestBody(httpReq, upstreamBody)
	} else {
		encodedBody, errEncode := helps.EncodeCodexOAuthBody(upstreamBody)
		if errEncode != nil {
			return resp, fmt.Errorf("codex oauth: encode zstd body: %w", errEncode)
		}
		upstreamBody = encodedBody
		replaceCodexRequestBody(httpReq, upstreamBody)
		httpReq.Header.Set("Content-Encoding", "zstd")
	}
	if helps.CodexAuthUsesOAuthCookieJar(auth) {
		helps.ApplyCodexOAuthRoutingHint(httpReq.Header, turnStateBody)
	}
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, opts.Headers)
	helps.RestoreCodexMetadataHeaders(httpReq.Header, turnStateBody)
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	if officialOAuthRequest {
		configuredUA, configuredBeta := codexHeaderDefaults(e.cfg, auth)
		helps.ApplyCodexOAuthHeaders(httpReq.Header, oauthIdentity, true, configuredUA, configuredBeta)
		applyCodexConfiguredHeaderOverrides(httpReq, auth, opts.Headers)
		applyModelHeaderOverrides(httpReq.Header, baseModel)
		httpReq.Header.Del("Cookie")
		httpReq.Header.Set("Authorization", "Bearer "+helps.CodexOAuthAccessToken(auth))
		httpReq.Header.Set("Chatgpt-Account-Id", helps.CodexOAuthAccountID(auth))
	} else {
		applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
		if fixedInstallationID != "" && httpReq.Header.Get("X-Codex-Turn-Metadata") != "" {
			httpReq.Header.Set("X-Codex-Turn-Metadata", helps.RewriteCodexTurnMetadataInstallation(httpReq.Header.Get("X-Codex-Turn-Metadata"), fixedInstallationID))
		}
	}
	ensureCodexResponsesLiteHeader(httpReq.Header, turnStateBody)
	turnState := helps.NewCodexTurnState(ctx, auth, url, turnStateBody, httpReq.Header, baseModel, opts.Headers)
	turnState.ApplyHeaders(httpReq.Header)
	if errTicket := helps.ApplyCodexTurnStateTicket(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, httpReq.Header); errTicket != nil {
		return resp, errTicket
	}
	turnState.ObserveRequest(httpReq.Header, nil)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      upstreamBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	turnState.ObserveResponse(httpResp)
	helps.RecordCodexTurnStateTicketOnResponse(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, httpResp)
	turnState.LogResponse(ctx, e.cfg, false)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		b = applyCodexIdentityConfuseResponsePayload(b, identityState)
		if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, httpResp.StatusCode, b); errClearReplay != nil {
			return resp, errClearReplay
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErrWithCooling(httpResp.StatusCode, b, e.modelLevelCooling())
		return resp, err
	}
	data, errRead := io.ReadAll(httpResp.Body)
	upstreamData := applyCodexIdentityConfuseResponsePayload(data, identityState)
	helps.AppendAPIResponseChunk(ctx, e.cfg, upstreamData)

	lines := bytes.Split(upstreamData, []byte("\n"))
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	sawOutputDelta := false
	for _, line := range lines {
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		eventData := bytes.TrimSpace(line[5:])
		eventData = helps.RestoreCodexMultiAgentV2Response(eventData, optimizeMultiAgentV2)
		reporter.ObserveCodexResponseModel(eventData)
		eventType := gjson.GetBytes(eventData, "type").String()

		if helps.HasMeaningfulCodexOutputDelta(eventData) {
			sawOutputDelta = true
		}

		if streamErr, terminalBody, ok := codexTerminalFailureErrWithCooling(eventData, e.modelLevelCooling()); ok {
			if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
				return resp, errClearReplay
			}
			err = streamErr
			return resp, err
		}

		if eventType == "response.output_item.done" {
			itemResult := gjson.GetBytes(eventData, "item")
			if !itemResult.Exists() || itemResult.Type != gjson.JSON {
				continue
			}
			outputIndexResult := gjson.GetBytes(eventData, "output_index")
			if outputIndexResult.Exists() {
				outputItemsByIndex[outputIndexResult.Int()] = []byte(itemResult.Raw)
			} else {
				outputItemsFallback = append(outputItemsFallback, []byte(itemResult.Raw))
			}
			continue
		}

		if eventType != "response.completed" && eventType != "response.incomplete" {
			continue
		}

		if helps.IsCodexTerminalEmptyIncomplete(eventData, len(outputItemsByIndex)+len(outputItemsFallback), sawOutputDelta) {
			err = newCodexEmptyIncompleteStreamError()
			return resp, err
		}

		if detail, ok := helps.ParseCodexUsage(eventData); ok {
			reporter.Publish(ctx, detail)
		}
		publishCodexImageToolUsage(ctx, reporter, body, eventData)

		completedData := patchCodexCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
		if eventType == "response.completed" {
			cacheCodexReasoningReplayFromCompleted(replayScope, completedData)
		}

		var param any
		clientCompletedData := applyCodexIdentityExposeResponsePayload(completedData, identityState)
		out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, body, clientCompletedData, &param)
		if responseFormat == sdktranslator.FormatOpenAIResponse {
			out = helps.EnsureResponsesUsageDetails(out)
		}
		resp = cliproxyexecutor.Response{Payload: out, Headers: helps.StripCodexInternalResponseHeaders(httpResp.Header)}
		return resp, nil
	}
	if errRead != nil {
		if errCtx := ctx.Err(); errCtx != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errCtx)
			err = errCtx
			return resp, err
		}
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
	}
	err = newCodexIncompleteStreamError()
	return resp, err
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}

	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai-response")
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalPayload := originalPayloadSource
	officialCodexRequest := codexOfficialRequest(originalPayloadSource, req.Payload)
	isCompat := e.resolveCodexModelIsCompat(auth, req, baseModel)
	originalTranslated, body := translateCodexRequestPair(from, to, baseModel, originalPayload, req.Payload, false, opts.Headers, isCompat)

	body, err = helps.ApplyRequestThinking(body, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return resp, err
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body, _ = sjson.DeleteBytes(body, "stream")
	body = applyCodexImageGenerationPolicyWithoutInjection(body, e.cfg, requestPath)
	toolHeaders := codexToolPolicyHeaders(auth, opts.Headers, baseModel)
	if gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").Exists() {
		body = ensureCodexResponsesLiteMirror(body, toolHeaders)
	}
	body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))
	body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompat(ctx, "codex executor", body, isCompat)
	body = normalizeCodexParallelToolCalls(body, toolHeaders, officialCodexRequest)
	body = helps.NormalizeCodexToolSchemas(body)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, baseModel)
	reporter.SetTranslatedReasoningEffort(body, to.String())

	var compactIdentity helps.CodexOAuthIdentity
	compactOAuth := false
	if helps.CodexAuthUsesOAuthCookieJar(auth) && helps.IsOfficialCodexRequest(body) {
		body, compactIdentity, compactOAuth = helps.ApplyCodexOAuthFidelity(body, codexInstallationAccountID(auth), codexInstallationCredentialSystem(auth), codexDeviceConvergenceEnabled(e.cfg))
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	var identityState codexIdentityConfuseState
	httpReq, upstreamBody, identityState, err := e.cacheHelper(ctx, from, url, auth, req, originalPayloadSource, body, opts.Headers)
	if err != nil {
		return resp, err
	}
	turnStateBody := upstreamBody
	upstreamBody = helps.ApplyCodexTurnStateTicketBody(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, upstreamBody)
	turnStateBody = upstreamBody
	replaceCodexRequestBody(httpReq, upstreamBody)
	if helps.CodexAuthUsesOAuthCookieJar(auth) {
		helps.ApplyCodexOAuthRoutingHint(httpReq.Header, turnStateBody)
	}
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg, opts.Headers)
	helps.RestoreCodexMetadataHeaders(httpReq.Header, turnStateBody)
	if compactOAuth {
		ua, beta := codexHeaderDefaults(e.cfg, auth)
		helps.ApplyCodexOAuthHeaders(httpReq.Header, compactIdentity, false, ua, beta)
		applyCodexConfiguredHeaderOverrides(httpReq, auth, opts.Headers)
	}
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	ensureCodexResponsesLiteHeader(httpReq.Header, upstreamBody)
	applyCodexIdentityConfuseHeaders(httpReq.Header, &identityState)
	turnState := helps.NewCodexTurnState(ctx, auth, url, turnStateBody, httpReq.Header, baseModel, opts.Headers)
	turnState.ApplyHeaders(httpReq.Header)
	if errTicket := helps.ApplyCodexTurnStateTicket(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, httpReq.Header); errTicket != nil {
		return resp, errTicket
	}
	turnState.ObserveRequest(httpReq.Header, nil)
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      upstreamBody,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	turnState.ObserveResponse(httpResp)
	helps.RecordCodexTurnStateTicketOnResponse(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, httpResp)
	turnState.LogResponse(ctx, e.cfg, false)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		b = applyCodexIdentityConfuseResponsePayload(b, identityState)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErrWithCooling(httpResp.StatusCode, b, e.modelLevelCooling())
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	upstreamData := applyCodexIdentityConfuseResponsePayload(data, identityState)
	helps.AppendAPIResponseChunk(ctx, e.cfg, upstreamData)
	upstreamData = helps.RestoreCodexMultiAgentV2Response(upstreamData, optimizeMultiAgentV2)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(upstreamData))
	reporter.EnsurePublished(ctx)
	var param any
	clientData := applyCodexIdentityExposeResponsePayload(upstreamData, identityState)
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, body, clientData, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: helps.StripCodexInternalResponseHeaders(httpResp.Header)}
	return resp, nil
}
