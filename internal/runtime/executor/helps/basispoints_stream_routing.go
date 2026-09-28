package helps

import (
	"bytes"
	"context"

	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// BufferNativeToolDecision withholds one candidate response until its complete
// tool batch establishes whether the original request needs the native channel.
// It introduces no timer, retry, tool execution, or additional model request.
func BufferNativeToolDecision(ctx context.Context, result *core.StreamResult) (*core.StreamResult, error) {
	var chunks []core.StreamChunk
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok := <-result.Chunks:
			if !ok {
				out := make(chan core.StreamChunk, len(chunks))
				for _, saved := range chunks {
					out <- saved
				}
				close(out)
				return &core.StreamResult{Headers: result.Headers, Chunks: out}, nil
			}
			if chunk.Err != nil {
				return nil, chunk.Err
			}
			chunk.Payload = bytes.Clone(chunk.Payload)
			chunks = append(chunks, chunk)
		}
	}
}
