package helps

import (
	"bytes"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

func TestCodexFastModeOverrides(t *testing.T) {
	for _, mode := range []string{"", "auto", "default", "fast", "ultrafast", " FAST ", "unknown"} {
		for _, input := range []string{"", "default", "fast", "priority", "ultrafast"} {
			t.Run(mode+"/"+input, func(t *testing.T) {
				body := []byte("{ \"model\":\"gpt-6-astra\", \"input\":[] }")
				if input != "" {
					body = []byte("{ \"model\":\"gpt-6-astra\", \"input\":[], \"service_tier\":\"" + input + "\" }")
				}
				original := bytes.Clone(body)
				cfg := &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{FastMode: mode}}
				got := ApplyCodexFastMode(body, cfg)
				want := input
				switch mode {
				case "default":
					want = ""
				case "fast", " FAST ":
					want = "priority"
				case "ultrafast":
					want = "ultrafast"
				default:
					if !bytes.Equal(got, body) {
						t.Fatal("auto must preserve the body byte for byte")
					}
				}
				tier := gjson.GetBytes(got, "service_tier")
				if tier.String() != want || (want == "" && tier.Exists()) {
					t.Fatalf("tier=%s want=%q", tier.Raw, want)
				}
				if !bytes.Equal(body, original) {
					t.Fatal("source payload changed")
				}
				if gjson.GetBytes(got, "model").String() != "gpt-6-astra" || !gjson.GetBytes(got, "input").IsArray() {
					t.Fatal("unrelated fields changed")
				}
			})
		}
	}
	body := []byte("{\"service_tier\":\"priority\"}")
	if !bytes.Equal(ApplyCodexFastMode(body, nil), body) {
		t.Fatal("nil config changed payload")
	}
}

func TestCodexFastModeUsageRecord(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{{"auto", "auto"}, {"default", "default"}, {"fast", "priority"}, {"ultrafast", "ultrafast"}} {
		t.Run(tc.mode, func(t *testing.T) {
			reporter := &UsageReporter{model: "gpt-6-astra", serviceTier: "auto"}
			reporter.SetCodexFastMode(&config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{FastMode: tc.mode}})
			record := reporter.buildRecordForModel("gpt-6-astra", usage.Detail{ResponseServiceTier: "default"}, false, usage.Failure{})
			if record.ServiceTier != tc.want {
				t.Fatalf("request tier=%q want=%q", record.ServiceTier, tc.want)
			}
			if record.ResponseServiceTier != "default" {
				t.Fatal("upstream reported tier changed")
			}
		})
	}
	reporter := &UsageReporter{serviceTier: "fast"}
	reporter.SetCodexFastMode(&config.Config{})
	if reporter.serviceTier != "fast" {
		t.Fatal("auto changed the client-requested tier")
	}
}
