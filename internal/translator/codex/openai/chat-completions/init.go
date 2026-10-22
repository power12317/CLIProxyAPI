package chat_completions

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
		OpenAI,
		Codex,
		ConvertOpenAIRequestToCodex,
		interfaces.TranslateResponse{
			Stream:    ConvertCodexResponseToOpenAI,
			NonStream: ConvertCodexResponseToOpenAINonStream,
		},
	)
	sdktranslator.RegisterRequestEnvelope(sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, func(ctx context.Context, req sdktranslator.RequestEnvelope) sdktranslator.RequestEnvelope {
		req.Body, req.Err = convertOpenAIRequestToCodex(req.Model, req.Body, req.Stream, translatorcommon.IsModelInput(ctx))
		return req
	})
}
