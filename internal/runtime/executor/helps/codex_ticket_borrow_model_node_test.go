package helps

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
)

func TestCodexBorrowProbeLogsActualResponseModelAndNode(t *testing.T) {
	created := "data: {\"type\":\"response.created\",\"response\":{\"model\":\"gpt-6-astra\"}}\n\n"
	final := "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-5.6-sol\"}}\n\n"
	for _, tc := range []struct {
		name, body, responseCookie, model, node  string
		httpStatus                               int
		readFailure, transportFailure, wantError bool
	}{
		{name: "terminal model and response node", body: created + final, responseCookie: "new", model: "gpt-5.6-sol", node: "unified-42"},
		{name: "request node fallback", body: final, model: "gpt-5.6-sol", node: "unified-86"},
		{name: "empty event before response", body: "data: {}\n\n" + final, model: "gpt-5.6-sol", node: "unified-86"},
		{name: "no model is not inferred", body: borrowTestComplete, model: "-", node: "unified-86"},
		{name: "failure keeps observed model", body: created + "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n\n", model: "gpt-6-astra", node: "unified-86", wantError: true},
		{name: "failed event model", body: "data: {\"type\":\"response.failed\",\"response\":{\"model\":\"gpt-5.6-sol\",\"error\":{\"code\":\"rate_limit_exceeded\"}}}\n\n", responseCookie: "new", model: "gpt-5.6-sol", node: "unified-42", wantError: true},
		{name: "read failure keeps metadata", body: created, readFailure: true, model: "gpt-6-astra", node: "unified-86", wantError: true},
		{name: "transport failure reports request node", transportFailure: true, model: "-", node: "unified-86", wantError: true},
		{name: "invalid response node does not fallback", body: final, responseCookie: "invalid", model: "gpt-5.6-sol", node: "-", wantError: true},
		{name: "deleted response node does not fallback", body: final, responseCookie: "deleted", model: "gpt-5.6-sol", node: "-", wantError: true},
		{name: "generated content is not model metadata", body: "data: {\"type\":\"response.output_text.delta\",\"response\":{\"model\":\"invented-model\"},\"delta\":\"model:invented-model\"}\n\n" + borrowTestComplete, model: "-", node: "unified-86"},
		{name: "HTTP error model", httpStatus: 429, body: `{"type":"error","model":"gpt-5.6-sol","code":"rate_limit_exceeded","message":"Try later"}`, model: "gpt-5.6-sol", node: "unified-86", wantError: true},
		{name: "CRLF multiline event", body: "\xef\xbb\xbfdata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"status\":\"completed\",\"model\":\"gpt-5.6-sol\"}}\r\n\r\n", model: "gpt-5.6-sol", node: "unified-86"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook := captureBorrowLogs(t)
			f := newBorrowFixture(t)
			requestCookie := oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-86.api.openai.com")
			responseCookie := oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-42.api.openai.com")
			f.seed.Route.OaiLB.Value = requestCookie
			ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
			logging.SetCodexTurnStateLogFields(ginCtx, logging.CodexTurnStateLogFields{RequestedModel: "business-model", ResponseModel: "business-response"})
			logging.SetCodexOaiLBNode(ginCtx, "business-node")
			f.ctx = context.WithValue(f.ctx, "gin", ginCtx)
			f.shot = func(req *http.Request, _ int32) (*http.Response, error) {
				if tc.transportFailure {
					return nil, context.DeadlineExceeded
				}
				header := http.Header{CodexTurnStateTicketHeader: {borrowTestState(time.Now(), 2)}}
				switch tc.responseCookie {
				case "new":
					header.Add("Set-Cookie", "__cflb=source-cf; Path=/")
					header.Add("Set-Cookie", "__oailb="+responseCookie+"; Path=/")
				case "invalid":
					header.Add("Set-Cookie", "__oailb=not-a-jwt; Path=/")
				case "deleted":
					header.Add("Set-Cookie", "__oailb=; Max-Age=0; Path=/")
				}
				var body io.Reader = strings.NewReader(tc.body)
				if tc.readFailure {
					body = io.MultiReader(body, borrowModelErrorReader{})
				}
				status := tc.httpStatus
				if status == 0 {
					status = http.StatusOK
				}
				return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(body), Request: req}, nil
			}
			_, _, err := probeCodexBorrow(f.ctx, f.cfg, f.auth, "gpt-6-astra", f.seed.State, *f.seed.Route, newCodexBorrowLog(f.ctx, f.auth, "gpt-6-astra"), "VERIFY")
			if (err != nil) != tc.wantError {
				t.Fatalf("err=%v wantError=%t", err, tc.wantError)
			}
			line := formattedBorrowLogs(t, hook)
			if !strings.Contains(line, " | "+tc.node+" | gpt-6-astra/"+tc.model+" | 780/") {
				t.Fatalf("missing model/node columns: %s", line)
			}
			for _, secret := range []string{requestCookie, responseCookie, "chat.gateway.", "__cflb=", "__oailb="} {
				if strings.Contains(line, secret) {
					t.Fatalf("unexpected cookie detail: %s", secret)
				}
			}
			fields, _ := logging.CodexTurnStateLogFieldsForContext(ginCtx)
			if fields.ResponseModel != "business-response" || logging.CodexOaiLBNode(ginCtx) != "business-node" {
				t.Fatal("probe contaminated business request log fields")
			}
		})
	}
}

type borrowModelErrorReader struct{}

func (borrowModelErrorReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCodexBorrowSourceAndReuseLogSnapshotNode(t *testing.T) {
	hook := captureBorrowLogs(t)
	f := newBorrowFixture(t)
	f.seed.Route.OaiLB.Value = oaiLBNodeTestJWT(time.Now().Add(time.Hour), "chat.gateway.unified-86.api.openai.com")
	ticket := f.acquire(t)
	if f.acquire(t) != ticket {
		t.Fatal("logging changed cache reuse")
	}
	logs := formattedBorrowLogs(t, hook)
	for _, want := range []string{
		" | unified-86 | gpt-6-astra/gpt-6-astra | 0/780 | POST",
		"READY | gpt-6-astra | node=unified-86 | ticket=780",
		"REUSE | gpt-6-astra | node=unified-86 | ticket=780",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q in %s", want, logs)
		}
	}
	if f.fetches.Load() != 1 || f.probes.Load() != 2 {
		t.Fatal("logging added network calls")
	}
}

func TestCodexBorrowModelFinalityAndRedaction(t *testing.T) {
	trace := &codexBorrowLog{}
	trace.observeResponseModel([]byte(`{"type":"response.completed","response":{"model":"gpt-6-astra"}}`), nil)
	trace.observeResponseModel([]byte(`{"type":"response.created","response":{"model":"late-model"}}`), nil)
	if trace.responseModel != "gpt-6-astra" {
		t.Fatal("late frame replaced terminal model")
	}
	trace = &codexBorrowLog{}
	trace.observeResponseModel([]byte(`{"type":"response.completed","response":{"model":"private-source.json"}}`), []string{"private-source.json"})
	if trace.responseModel != "[redacted]" {
		t.Fatal("response model exposed source identifier")
	}
}
