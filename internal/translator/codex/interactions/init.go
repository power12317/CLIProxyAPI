package interactions

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
		Interactions,
		Codex,
		ConvertInteractionsRequestToCodex,
		interfaces.TranslateResponse{
			Stream:    ConvertCodexResponseToInteractions,
			NonStream: ConvertCodexResponseToInteractionsNonStream,
		},
	)
	sdktranslator.RegisterRequestEnvelope(sdktranslator.FormatInteractions, sdktranslator.FormatCodex, func(ctx context.Context, req sdktranslator.RequestEnvelope) sdktranslator.RequestEnvelope {
		req.Body = convertInteractionsRequestToCodex(req.Model, req.Body, req.Stream, translatorcommon.IsModelInput(ctx))
		return req
	})
}
