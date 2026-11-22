package helps

import (
	"bytes"
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// BufferNativeToolDecision withholds one candidate response until its complete
// tool batch establishes whether the original request needs the native channel.
// It introduces no timer, retry, tool execution, or additional model request.
func BufferNativeToolDecision(ctx context.Context, result *core.StreamResult, preservePartialFailure bool) (*core.StreamResult, error) {
	var chunks []core.StreamChunk
	bufferedBytes := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok := <-result.Chunks:
			if !ok || chunk.Err != nil && preservePartialFailure && basispoints.RequestedNativeTool(chunk.Err) == "" {
				// Preserve the existing partial-error and retry boundary for client-only streams.
				if ok {
					chunks = append(chunks, chunk)
				}
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
			bufferedBytes += len(chunk.Payload)
			if bufferedBytes > 32<<20 || len(chunks) >= 65536 {
				return nil, &core.CodexReplayUnsafeError{Cause: fmt.Errorf("Basispoints candidate exceeds the relay limit")}
			}
			chunk.Payload = bytes.Clone(chunk.Payload)
			chunks = append(chunks, chunk)
		}
	}
}
