package helps

import (
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

// CodexWebsocketRequestOverloaded identifies an explicit request capacity error,
// not an arbitrary transport failure that happens to use HTTP status 503.
func CodexWebsocketRequestOverloaded(payload []byte) bool {
	kind := ojson.GetBytes(payload, "type").String()
	if kind != "error" && kind != "response.failed" {
		return false
	}
	status := ojson.GetBytes(payload, "status").Int()
	if status == 0 {
		status = ojson.GetBytes(payload, "status_code").Int()
	}
	if status != 0 && (status < http.StatusInternalServerError || status >= 600) {
		return false
	}
	for _, path := range []string{"response.error", "body.error", "error"} {
		value := ojson.GetBytes(payload, path)
		if value.IsObject() {
			code := strings.ToLower(strings.TrimSpace(value.Get("code").String()))
			return code == "server_is_overloaded" || code == "" && value.Get("type").String() == "service_unavailable_error"
		}
	}
	return ojson.GetBytes(payload, "code").String() == "server_is_overloaded"
}
