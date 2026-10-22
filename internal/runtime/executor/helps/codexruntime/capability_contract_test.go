package codexruntime

import (
	"io"
	"testing"

	"github.com/gorilla/websocket"
)

func TestClientConsumesAdvertisedBusinessAndCompletionSemantics(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "http-body-eof"
		if native {
			name = "native-terminal"
		}
		t.Run(name, func(t *testing.T) {
			caps := validCaps()
			caps.CompletionSemantics = name
			caps.BusinessNotification = "cpa/inference/body"
			caps.DiagnosticsCompleted = native
			caps.ResponsesWebsocket = native
			caps.TicketSnapshots = false
			caps.RuntimeVersion, caps.UpstreamRevision = "another-product-build", "another-source-revision"
			if native {
				caps.BusinessNotification = "cpa/inference/event"
			}
			releaseDiagnostics := make(chan struct{})
			logs := make(chan string, 2)
			endpoint := serveMaster(t, caps, func(conn *websocket.Conn, rpc message) {
				accept(conn, rpc)
				if !native {
					notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "request", Body: "original request"})
				}
				notify(conn, caps.BusinessNotification, map[string]any{"requestId": "req", "bodyBase64": []byte("original business bytes")})
				notify(conn, "cpa/inference/completed", map[string]any{"requestId": "req"})
				if native {
					<-releaseDiagnostics
					notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "request", Body: "original request"})
					notify(conn, "cpa/inference/diagnostics/completed", map[string]any{"requestId": "req"})
				}
			})
			client := dialMaster(t, endpoint)
			client.OnUpstream = func(entry UpstreamLog) {
				if entry.Kind == "request" {
					logs <- entry.Body
				}
			}
			if err := client.Start(request()); err != nil {
				t.Fatal(err)
			}
			frame, err := client.Next()
			if err != nil || string(frame.Body) != "original business bytes" {
				t.Fatalf("body=%q error=%v", frame.Body, err)
			}
			if _, err := client.Next(); err != io.EOF {
				t.Fatalf("completion=%v", err)
			}
			if client.NativeCompleted() != native {
				t.Fatal("HTTP EOF was confused with native model completion")
			}
			client.Close()
			close(releaseDiagnostics)
			client.WaitDiagnostics()
			if got := <-logs; got != "original request" {
				t.Fatalf("diagnostic=%q", got)
			}
		})
	}
}
