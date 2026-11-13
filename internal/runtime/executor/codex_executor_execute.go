package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
)

type codexCompactionSummaryContextKey struct{}

type codexCompactionSummaryState struct {
	baseModel string
}

func withCodexCompactionSummary(ctx context.Context, baseModel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, codexCompactionSummaryContextKey{}, &codexCompactionSummaryState{baseModel: baseModel})
}

func codexCompactionSummaryStateFromContext(ctx context.Context) *codexCompactionSummaryState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(codexCompactionSummaryContextKey{}).(*codexCompactionSummaryState)
	return state
}

func codexCompactionSummaryFromContext(ctx context.Context) bool {
	return codexCompactionSummaryStateFromContext(ctx) != nil
}

func rejectSyntheticCompactionPreviousResponseID(ctx context.Context, payload []byte) error {
	prevID := strings.TrimSpace(ojson.GetBytesNoCopy(payload, "previous_response_id").String())
	if !helps.RecognizedResponsesCompactionResponseID(prevID) {
		return nil
	}
	if cliproxyexecutor.DownstreamWebsocket(ctx) {
		return &cliproxyexecutor.UpstreamWebsocketTurnError{
			FullContextRequired: true,
			EventID:             strings.Clone(strings.TrimSpace(ojson.GetBytesNoCopy(payload, "event_id").String())),
		}
	}
	return statusErr{
		code: http.StatusBadRequest,
		msg:  "previous_response_id references a local compaction response; resend full conversation input without previous_response_id",
	}
}

func validateCodexLocalCompactionSource(ctx context.Context, payload []byte) error {
	prevID := strings.TrimSpace(ojson.GetBytesNoCopy(payload, "previous_response_id").String())
	reqType := strings.TrimSpace(ojson.GetBytesNoCopy(payload, "type").String())
	if prevID != "" || reqType == "response.append" {
		if cliproxyexecutor.DownstreamWebsocket(ctx) {
			return &cliproxyexecutor.UpstreamWebsocketTurnError{
				FullContextRequired: true,
				EventID:             strings.Clone(strings.TrimSpace(ojson.GetBytesNoCopy(payload, "event_id").String())),
			}
		}
		return statusErr{
			code: http.StatusBadRequest,
			msg:  "local summary compaction requires full conversation input without previous_response_id",
		}
	}
	if !helps.HasResponsesCompactionConversationInput(payload) {
		return statusErr{
			code: http.StatusBadRequest,
			msg:  "local summary compaction requires non-empty conversation input",
		}
	}
	return nil
}

func (e *CodexExecutor) expandCodexResponsesCompaction(auth *cliproxyauth.Auth, req *cliproxyexecutor.Request, opts *cliproxyexecutor.Options, baseModel string) (bool, error) {
	if req == nil || opts == nil {
		return false, nil
	}
	isCompat := e.resolveCodexModelIsCompat(auth, *req, baseModel)
	if e.runtime == nil && isCompat && opts.SourceFormat == sdktranslator.FormatOpenAIResponse && opts.Alt != "responses/compact" {
		return true, helps.ExpandResponsesCompactionCapsules(req, opts, e.Identifier())
	}
	return isCompat, helps.ExpandRecognizedResponsesCompactionCapsules(req, opts)
}

func (e *CodexExecutor) shouldCompactCompatResponsesTrigger(isCompat bool, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	return e.runtime == nil && isCompat && opts.SourceFormat == sdktranslator.FormatOpenAIResponse && opts.Alt != "responses/compact" && helps.HasResponsesCompactionTriggerRequest(req, opts)
}

func (e *CodexExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	auth = e.requestAuth(auth)
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	ctx = helps.WithCodexCommentaryPolicy(ctx, e.cfg)
	if opts.Alt == "responses/compact" {
		return e.executeCompact(ctx, auth, req, opts)
	}
	if isCodexOpenAIImageRequest(opts) {
		if errExpand := helps.ExpandRecognizedResponsesCompactionCapsules(&req, &opts); errExpand != nil {
			return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
		}
		return e.executeOpenAIImage(ctx, auth, req, opts)
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	isCompat, errExpand := e.expandCodexResponsesCompaction(auth, &req, &opts, baseModel)
	if errExpand != nil {
		return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
	}
	if !codexCompactionSummaryFromContext(ctx) && e.shouldCompactCompatResponsesTrigger(isCompat, req, opts) {
		return e.executeCodexResponsesCompaction(ctx, auth, req, opts)
	}
	if errPrev := rejectSyntheticCompactionPreviousResponseID(ctx, req.Payload); errPrev != nil {
		return resp, errPrev
	}

	apiKey, baseURL := codexCredsForRequest(ctx, auth, opts.Headers)
	if errSIWC := ensureCodexSIWCCredentialsForRequest(ctx, auth, apiKey, opts.Headers); errSIWC != nil {
		return resp, errSIWC
	}
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
	targetModel := resolveResponsesSignatureTargetModel(baseModel, req, opts)
	originalTranslated, body, updatesChanged, err := e.translateRequestPair(from, to, targetModel, originalPayload, req.Payload, false, opts.Headers, isCompat, ctx)
	if err != nil {
		return resp, err
	}
	originalTranslated = helps.SetStringIfDifferent(originalTranslated, "model", baseModel)
	doc := ojson.NewDoc(body)
	normalizeCodexOutputBudgetDoc(doc)
	helps.SetStringIfDifferentDoc(doc, "model", baseModel)

	if err = helps.ApplyRequestThinkingDoc(doc, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged); err != nil {
		return resp, err
	}

	body = doc.Bytes()
	requestPath := helps.PayloadRequestPath(opts)
	body = e.normalizeServiceTier(body)
	body = helps.ApplyCodexFastMode(body, e.cfg)
	ctx = helps.WithPayloadFinalizer(ctx, helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslated, req, opts, auth))
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body = helps.SetBoolIfDifferent(body, "stream", true)
	if e.runtime == nil {
		body, _ = ojson.DeleteBytes(body, "previous_response_id")
		body, _ = ojson.DeleteBytes(body, "generate")
		body, _ = ojson.DeleteBytes(body, "prompt_cache_retention")
		body, _ = ojson.DeleteBytes(body, "safety_identifier")
		body, _ = ojson.DeleteBytes(body, "stream_options")
	}
	useSIWC := codexUseSIWCForRequest(ctx, auth, opts.Headers)
	if !useSIWC {
		body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))
	}
	toolHeaders := codexToolPolicyHeaders(auth, opts.Headers, baseModel)
	if e.runtime != nil {
		body = helps.ApplyCodexRuntimeImagePolicy(body, codexShouldStripImageGeneration(e.cfg, requestPath))
	} else if !useSIWC {
		if !codexCompactionSummaryFromContext(ctx) {
			body = ensureWebSearchToolForHistory(body, opts.Headers)
		}
		if ojson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").Exists() {
			body = ensureCodexResponsesLiteMirror(body, toolHeaders)
		}
		if !codexCompactionSummaryFromContext(ctx) {
			body, err = applyCodexImageGenerationPolicy(body, baseModel, auth, e.cfg, toolHeaders, requestPath, officialCodexRequest, originalPayloadSource)
		}
		if err != nil {
			return resp, err
		}
		body = normalizeCodexParallelToolCalls(body, toolHeaders, officialCodexRequest)
		body = helps.NormalizeCodexToolSchemas(body)
	} else {
		if !codexCompactionSummaryFromContext(ctx) {
			body = ensureWebSearchToolForHistory(body, opts.Headers)
		}
		hasExplicitInstructions := ojson.GetBytes(req.Payload, "instructions").Exists() && strings.TrimSpace(ojson.GetBytes(req.Payload, "instructions").String()) != ""
		body = applyCodexSIWCRequestBodyRules(body, hasExplicitInstructions, cliproxyexecutor.Options{KeepTopLevelEffort: thinking.HasThinkingSuffix(req.Model)})
		body = normalizeCodexParallelToolCalls(body, toolHeaders, officialCodexRequest)
		body = helps.NormalizeCodexToolSchemas(body)
	}
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, isCompat)
	body, replayScope, errReplay := applyCodexReasoningReplayCacheRequired(ctx, from, req, opts, body)
	if errReplay != nil {
		return resp, errReplay
	}
	if e.runtime == nil {
		body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx, "codex executor", body, isCompat, targetModel, baseURL)
	}
	var oauthIdentity helps.CodexOAuthIdentity
	officialOAuthRequest := false
	var fixedInstallationID string
	if !useSIWC && (helps.CodexAuthUsesOAuthCookieJar(auth) || e.runtime != nil) && helps.IsOfficialCodexRequest(body) {
		body, oauthIdentity, officialOAuthRequest = helps.ApplyCodexOAuthFidelity(body, codexInstallationAccountID(auth), codexInstallationCredentialSystem(auth), codexDeviceConvergenceEnabled(e.cfg))
	}
	reporter.SetCodexFastMode(e.cfg)

	url := strings.TrimSuffix(baseURL, "/") + "/responses"
	httpReq, upstreamBody, err := e.prepareCachedRequest(ctx, from, url, req, body, opts.Headers)
	if err != nil {
		return resp, err
	}
	upstreamBody = helps.ApplyCodexTurnStateTicketBody(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, upstreamBody)
	var turnStateBody []byte
	if !officialOAuthRequest {
		if !useSIWC {
			upstreamBody, fixedInstallationID, _, _ = helps.ApplyCodexInstallationIdentity(upstreamBody, codexInstallationAccountID(auth), codexInstallationCredentialSystem(auth), codexDeviceConvergenceEnabled(e.cfg))
		}
		upstreamBody = helps.FinalizePayload(ctx, upstreamBody)
		turnStateBody = upstreamBody
		replaceCodexRequestBody(httpReq, e.codexWireBody(ctx, url, upstreamBody, auth, opts.Headers))
		alignCodexContentEncoding(httpReq)
	} else {
		upstreamBody = helps.FinalizePayload(ctx, upstreamBody)
		turnStateBody = upstreamBody
		encodedBody, errEncode := helps.EncodeCodexOAuthBody(upstreamBody)
		if errEncode != nil {
			return resp, fmt.Errorf("codex oauth: encode zstd body: %w", errEncode)
		}
		replaceCodexRequestBody(httpReq, encodedBody)
		httpReq.Header.Set("Content-Encoding", "zstd")
	}
	upstreamModel := strings.TrimSpace(ojson.GetBytes(turnStateBody, "model").String())
	if upstreamModel != "" && upstreamModel != baseModel {
		reporter.SetUpstreamModel(upstreamModel)
	}
	reporter.SetTranslatedReasoningEffort(turnStateBody, to.String())
	applyCodexHeaders(httpReq, auth, apiKey, true, e.cfg, opts.Headers)
	helps.RestoreCodexMetadataHeaders(httpReq.Header, turnStateBody)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, upstreamModel, turnStateBody, opts.Headers)
	applyModelHeaderOverrides(httpReq.Header, upstreamModel)
	if officialOAuthRequest {
		configuredUA, configuredBeta := codexHeaderDefaults(e.cfg, auth)
		helps.ApplyCodexOAuthHeaders(httpReq.Header, oauthIdentity, true, configuredUA, configuredBeta)
		applyCodexConfiguredHeaderOverrides(httpReq, auth, opts.Headers)
		applyModelHeaderOverrides(httpReq.Header, upstreamModel)
		httpReq.Header.Del("Cookie")
		httpReq.Header.Set("Authorization", "Bearer "+helps.CodexOAuthAccessToken(auth))
		httpReq.Header.Set("Chatgpt-Account-Id", helps.CodexOAuthAccountID(auth))
	} else {
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
	if errValidate := helps.ValidateCodexHostedImageGenerationDoc(ojson.NewDoc(turnStateBody), httpReq.Header, e.cfg, !useSIWC && !codexAuthUsesAPIKey(auth)); errValidate != nil {
		return resp, errValidate
	}
	if e.runtime != nil {
		return e.runtime.executeViaMaster(ctx, auth, req, opts, reporter, turnStateBody, optimizeMultiAgentV2, replayScope)
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
	prepareHeaders := func(request *http.Request) {
		request.Header = httpReq.Header.Clone()
		alignCodexContentEncoding(request)
	}
	httpResp, err := doCodexRequest(httpClient, httpReq, upstreamBody, prepareHeaders, nil)
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
		b, errRead := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		}
		if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, httpResp.StatusCode, b); errClearReplay != nil {
			return resp, errClearReplay
		}
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErrWithHeadersAndCooling(httpResp.StatusCode, httpResp.Header, b, e.modelLevelCooling())
		return resp, err
	}
	data, errRead := io.ReadAll(httpResp.Body)
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	events := helps.NewCodexSSEScanner(ctx, bytes.NewReader(data), len(data)+1)
	outputItemsByIndex := make(map[int64][]byte)
	var outputItemsFallback [][]byte
	sawOutputDelta := false
	for events.Scan() {
		line := events.Bytes()
		if !bytes.HasPrefix(line, dataTag) {
			continue
		}

		eventData := bytes.TrimSpace(line[5:])
		eventData = helps.RestoreCodexMultiAgentV2Response(eventData, optimizeMultiAgentV2)
		reporter.ObserveResponseModel(eventData)
		eventType := ojson.GetBytesNoCopy(eventData, "type").String()

		if helps.HasMeaningfulCodexOutputDelta(eventData) {
			sawOutputDelta = true
		}

		if streamErr, terminalBody, ok := codexTerminalFailureErrWithCooling(eventData, e.modelLevelCooling()); ok {
			if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
				if detail, hasDetail := helps.ParseCodexUsageWithOutputItems(eventData, outputItemsByIndex, outputItemsFallback); hasDetail {
					reporter.PublishFailureWithDetail(ctx, detail, errClearReplay)
				}
				return resp, errClearReplay
			}
			err = streamErr
			if detail, hasDetail := helps.ParseCodexUsageWithOutputItems(eventData, outputItemsByIndex, outputItemsFallback); hasDetail {
				reporter.PublishFailureWithDetail(ctx, detail, err)
			}
			return resp, err
		}

		if eventType == "response.output_item.done" {
			itemResult := ojson.GetBytesNoCopy(eventData, "item")
			if !itemResult.Exists() || itemResult.Type != ojson.JSON {
				continue
			}
			outputIndexResult := ojson.GetBytesNoCopy(eventData, "output_index")
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
			if detail, hasDetail := helps.ParseCodexUsageWithOutputItems(eventData, outputItemsByIndex, outputItemsFallback); hasDetail {
				reporter.PublishFailureWithDetail(ctx, detail, err)
			}
			return resp, err
		}

		completedData := patchCodexCompletedOutput(eventData, outputItemsByIndex, outputItemsFallback)
		if eventType == "response.completed" {
			cacheCodexReasoningReplayFromCompleted(replayScope, completedData)
		}

		var param any
		out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, body, completedData, &param)
		if helps.ApplyPatchTranslationError(param) != nil || len(out) == 0 {
			return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
		}
		detail, hasDetail := helps.ParseCodexUsageWithOutputItems(completedData, outputItemsByIndex, outputItemsFallback)
		if responseFormat == sdktranslator.FormatOpenAIResponse {
			out = helps.EnsureResponsesUsageDetails(out)
		}
		if summaryState := codexCompactionSummaryStateFromContext(ctx); summaryState != nil {
			if eventType == "response.incomplete" {
				err = statusErr{code: http.StatusBadGateway, msg: "compaction summary response status is \"incomplete\", want \"completed\""}
				if hasDetail {
					reporter.PublishFailureWithDetail(ctx, detail, err)
				} else {
					reporter.PublishFailure(ctx, err)
				}
				return cliproxyexecutor.Response{}, err
			}
			compactedPayload, errBuild := helps.BuildResponsesCompactionResponseFromSummary(summaryState.baseModel, out)
			if errBuild != nil {
				err = statusErr{code: http.StatusBadGateway, msg: errBuild.Error()}
				if hasDetail {
					reporter.PublishFailureWithDetail(ctx, detail, err)
				} else {
					reporter.PublishFailure(ctx, err)
				}
				return cliproxyexecutor.Response{}, err
			}
			if hasDetail {
				reporter.Publish(ctx, detail)
			} else {
				reporter.EnsurePublished(ctx)
			}
			publishCodexImageToolUsageWithOutputItems(ctx, reporter, body, completedData, outputItemsByIndex, outputItemsFallback)
			resp = cliproxyexecutor.Response{Payload: compactedPayload, Headers: helps.StripCodexInternalResponseHeaders(httpResp.Header)}
			return resp, nil
		}
		if hasDetail {
			reporter.Publish(ctx, detail)
		}
		publishCodexImageToolUsageWithOutputItems(ctx, reporter, body, completedData, outputItemsByIndex, outputItemsFallback)
		resp = cliproxyexecutor.Response{Payload: out, Incomplete: eventType == "response.incomplete", Headers: helps.StripCodexInternalResponseHeaders(httpResp.Header)}
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

func (e *CodexExecutor) executeCodexResponsesCompaction(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	var resp cliproxyexecutor.Response
	sourcePayload := helps.ResponsesCompactionSourcePayload(req, opts)
	if errValidate := validateCodexLocalCompactionSource(ctx, sourcePayload); errValidate != nil {
		return resp, errValidate
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	summaryPayload := helps.PrepareAntigravityCompactionSummaryPayload(sourcePayload, baseModel)
	summaryReq := req
	summaryReq.Payload = summaryPayload
	summaryOpts := opts
	summaryOpts.Alt = ""
	summaryOpts.Stream = false
	summaryOpts.OriginalRequest = nil
	summaryOpts.SourceFormat = sdktranslator.FormatOpenAIResponse
	summaryOpts.ResponseFormat = sdktranslator.FormatOpenAIResponse

	return e.Execute(withCodexCompactionSummary(ctx, baseModel), auth, summaryReq, summaryOpts)
}

func (e *CodexExecutor) executeCodexResponsesCompactionStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	cliproxyexecutor.MarkLocalWebsocketTurn(ctx)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	summaryResp, errSummary := e.executeCodexResponsesCompaction(ctx, auth, req, opts)
	if errSummary != nil {
		return nil, errSummary
	}
	chunks, errChunks := helps.BuildResponsesCompactionStreamChunksFromResponse(baseModel, summaryResp.Payload)
	if errChunks != nil {
		return nil, statusErr{code: http.StatusBadGateway, msg: errChunks.Error()}
	}
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)

	headers := summaryResp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func (e *CodexExecutor) executeCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if errExpand := helps.ExpandRecognizedResponsesCompactionCapsules(&req, &opts); errExpand != nil {
		return resp, statusErr{code: http.StatusBadRequest, msg: errExpand.Error()}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	apiKey, baseURL := codexCredsForRequest(ctx, auth, opts.Headers)
	if errSIWC := ensureCodexSIWCCredentialsForRequest(ctx, auth, apiKey, opts.Headers); errSIWC != nil {
		return resp, errSIWC
	}
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
	targetModel := resolveResponsesSignatureTargetModel(baseModel, req, opts)
	originalTranslated, body, updatesChanged, err := e.translateRequestPair(from, to, targetModel, originalPayload, req.Payload, false, opts.Headers, isCompat, ctx)
	if err != nil {
		return resp, err
	}
	originalTranslated = helps.SetStringIfDifferent(originalTranslated, "model", baseModel)

	doc := ojson.NewDoc(body)
	if err = helps.ApplyRequestThinkingDoc(doc, req, opts, from.String(), to.String(), e.Identifier(), updatesChanged); err != nil {
		return resp, err
	}

	body = doc.Bytes()
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.NormalizeCodexServiceTier(body)
	body = helps.ApplyCodexFastMode(body, e.cfg)
	ctx = helps.WithPayloadFinalizer(ctx, helps.NewPayloadFinalizer(e.cfg, e.Identifier(), baseModel, to.String(), "", originalTranslated, req, opts))
	body = helps.SetStringIfDifferent(body, "model", baseModel)
	body, _ = ojson.DeleteBytes(body, "stream")
	body = applyCodexImageGenerationPolicyWithoutInjection(body, e.cfg, requestPath)
	toolHeaders := codexToolPolicyHeaders(auth, opts.Headers, baseModel)
	if ojson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").Exists() {
		body = ensureCodexResponsesLiteMirror(body, toolHeaders)
	}
	if !codexUseSIWCForRequest(ctx, auth, opts.Headers) {
		body = normalizeCodexInstructions(body, helps.IsNativeCodexRequest(req.Payload, opts))
	} else {
		instructionsResult := ojson.GetBytesNoCopy(req.Payload, "instructions")
		hasExplicitInstructions := instructionsResult.Exists() && strings.TrimSpace(instructionsResult.String()) != ""
		body = applyCodexSIWCCompactBodyRules(body, hasExplicitInstructions, cliproxyexecutor.Options{KeepTopLevelEffort: thinking.HasThinkingSuffix(req.Model)})
	}
	body = ensureWebSearchToolForHistory(body, opts.Headers)
	body = sanitizeOpenAIResponsesReasoningEncryptedContentWithCompatModelAndBaseURL(ctx, "codex executor", body, isCompat, resolveResponsesSignatureTargetModel(baseModel, req, opts), baseURL)
	body = normalizeCodexParallelToolCalls(body, toolHeaders, officialCodexRequest)
	body = helps.NormalizeCodexToolSchemas(body)
	body, optimizeMultiAgentV2 := helps.OptimizeCodexMultiAgentV2RequestForAuth(ctx, opts.Headers, body, e.cfg, auth, isCompat)
	reporter.SetCodexFastMode(e.cfg)

	var compactIdentity helps.CodexOAuthIdentity
	compactOAuth := false
	if !codexUseSIWCForRequest(ctx, auth, opts.Headers) && (helps.CodexAuthUsesOAuthCookieJar(auth) || e.runtime != nil) && helps.IsOfficialCodexRequest(body) {
		body, compactIdentity, compactOAuth = helps.ApplyCodexOAuthFidelity(body, codexInstallationAccountID(auth), codexInstallationCredentialSystem(auth), codexDeviceConvergenceEnabled(e.cfg))
	}
	url := strings.TrimSuffix(baseURL, "/") + "/responses/compact"
	httpReq, upstreamBody, err := e.prepareCachedRequest(ctx, from, url, req, body, opts.Headers)
	if err != nil {
		return resp, err
	}
	turnStateBody := upstreamBody
	upstreamBody = helps.ApplyCodexTurnStateTicketBody(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, upstreamBody)
	upstreamBody = helps.FinalizePayload(ctx, upstreamBody)
	turnStateBody = upstreamBody
	replaceCodexRequestBody(httpReq, e.codexWireBody(ctx, url, upstreamBody, auth, opts.Headers))
	alignCodexContentEncoding(httpReq)
	reporter.SetTranslatedReasoningEffort(turnStateBody, to.String())
	applyCodexHeaders(httpReq, auth, apiKey, false, e.cfg, opts.Headers)
	helps.RestoreCodexMetadataHeaders(httpReq.Header, turnStateBody)
	applyCodexRoutingHint(ctx, httpReq.Header, auth, baseModel, turnStateBody, opts.Headers)
	if compactOAuth {
		ua, beta := codexHeaderDefaults(e.cfg, auth)
		helps.ApplyCodexOAuthHeaders(httpReq.Header, compactIdentity, false, ua, beta)
		applyCodexConfiguredHeaderOverrides(httpReq, auth, opts.Headers)
	}
	applyModelHeaderOverrides(httpReq.Header, baseModel)
	ensureCodexResponsesLiteHeader(httpReq.Header, upstreamBody)
	turnState := helps.NewCodexTurnState(ctx, auth, url, turnStateBody, httpReq.Header, baseModel, opts.Headers)
	turnState.ApplyHeaders(httpReq.Header)
	if errTicket := helps.ApplyCodexTurnStateTicket(auth, e.cfg.Codex.EffectiveTurnStateTicket(), baseModel, httpReq.Header); errTicket != nil {
		return resp, errTicket
	}
	if e.runtime != nil {
		return e.runtime.executeViaMaster(ctx, auth, req, opts, reporter, turnStateBody, optimizeMultiAgentV2, codexReasoningReplayScope{})
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
	prepareHeaders := func(request *http.Request) {
		request.Header = httpReq.Header.Clone()
		alignCodexContentEncoding(request)
	}
	httpResp, err := doCodexRequest(httpClient, httpReq, upstreamBody, prepareHeaders, nil)
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
		b, errRead := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		}
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = newCodexStatusErrWithHeadersAndCooling(httpResp.StatusCode, httpResp.Header, b, e.modelLevelCooling())
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	upstreamData := helps.RestoreCodexMultiAgentV2Response(data, optimizeMultiAgentV2)
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, originalPayload, body, upstreamData, &param)
	if helps.ApplyPatchTranslationError(param) != nil || len(out) == 0 {
		return cliproxyexecutor.Response{}, statusErr{code: http.StatusBadGateway, msg: helps.ApplyPatchUpstreamErrorMessage}
	}
	reporter.Publish(ctx, helps.ParseOpenAIUsage(upstreamData))
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: helps.StripCodexInternalResponseHeaders(httpResp.Header)}
	return resp, nil
}
