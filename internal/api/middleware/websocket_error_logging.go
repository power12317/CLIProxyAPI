package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

// Snapshot a failed generation without finalizing the still-live connection or
// consuming its file-backed sources. The existing request ID stays downloadable.
func (w *ResponseWriterWrapper) snapshotWebsocketError(c *gin.Context, err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	sections := []struct {
		body   []byte
		source *logging.FileBodySource
	}{
		{w.extractAPIRequest(c), w.extractAPIRequestSource(c)},
		{w.extractAPIResponse(c), w.extractAPIResponseSource(c)},
		{w.extractAPIWebsocketTimeline(c), w.extractAPIWebsocketTimelineSource(c)},
	}
	for i := range sections {
		section := &sections[i]
		section.body = bytes.Clone(section.body)
		if section.source == nil {
			continue
		}
		payload, errRead := section.source.Bytes()
		if errRead != nil {
			return errRead
		}
		section.body = append(section.body, payload...)
	}
	apiError := &interfaces.ErrorMessage{StatusCode: clienterror.HTTPStatusFromErrorOr(err, http.StatusBadGateway), Error: err}
	return w.logRequest(w.extractRequestBody(c), http.StatusSwitchingProtocols, nil, []byte(err.Error()), nil, nil,
		sections[0].body, nil, sections[1].body, nil, sections[2].body, nil,
		w.extractAPIResponseTimestamp(c), []*interfaces.ErrorMessage{apiError}, true)
}
