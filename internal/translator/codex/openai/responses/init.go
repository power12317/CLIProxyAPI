package responses

import (
	"context"

	. "github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	translatorcommon "github.com/router-for-me/CLIProxyAPI/v8/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/translator/translator"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func init() {
	translator.Register(
		OpenaiResponse,
		Codex,
		ConvertOpenAIResponsesRequestToCodex,
		interfaces.TranslateResponse{
			Stream:    ConvertCodexResponseToOpenAIResponses,
			NonStream: ConvertCodexResponseToOpenAIResponsesNonStream,
		},
	)
	sdktranslator.RegisterRequestEnvelope(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatCodex, func(ctx context.Context, req sdktranslator.RequestEnvelope) sdktranslator.RequestEnvelope {
		if translatorcommon.IsModelInput(ctx) {
			req.Body = convertOpenAIResponsesModelInput(req.Body)
		} else {
			req.Body, req.Err = ConvertOpenAIResponsesRequestToCodex(req.Model, req.Body, req.Stream)
		}
		return req
	})
}
