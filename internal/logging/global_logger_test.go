package logging

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

func TestLogFormatterTicketProbeUsesExactCompactFormat(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 9, 20, 18, 2, 0, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Caller = &runtime.Frame{File: "/src/codex_turn_state_ticket_log.go", Line: 42}
	entry.Message = `200 | 1.420s | gpt-5.6-astra/- | 0/332 | POST "/backend-api/codex/responses"`
	entry.Data = log.Fields{
		CodexTicketProbeLogField: true,
		"request_id":             "a1b2c3d6", "auth_file": "codex-user-team-windows.json",
		"session_id": "7a120003", "turn_id": "8b230003",
	}
	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	want := "[2026-09-20 18:02:00] [a1b2c3d6] [codex-user-team-windows.json] [7a120003] [8b230003] [info ] [TICKET-PROBE] 200 | 1.420s | gpt-5.6-astra/- | 0/332 | POST \"/backend-api/codex/responses\"\n"
	if string(formatted) != want {
		t.Fatalf("probe log = %q, want %q", formatted, want)
	}
	delete(entry.Data, CodexTicketProbeLogField)
	formatted, errFormat = (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatal(errFormat)
	}
	if !strings.Contains(string(formatted), "[codex_turn_state_ticket_log.go:42]") {
		t.Fatalf("ordinary log lost source location: %s", formatted)
	}
}

func setMigrationConsoleOutputForTest(w io.Writer) func() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	prev := consoleWriter
	consoleWriter = w
	return func() {
		consoleMu.Lock()
		defer consoleMu.Unlock()
		consoleWriter = prev
	}
}

func TestLogFormatterPrintsVersionField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 9, 11, 10, 2, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "fetched latest antigravity version"
	entry.Data["version"] = "2.1.0"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("formatted line %q missing version field", line)
	}
}

func TestLogFormatterPrintsMediaForwardingFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 7, 25, 7, 36, 4, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "codex live remote media forwarding started"
	entry.Data["credential"] = "Voice credential\nsecondary"
	entry.Data["connection"] = "via socks5 proxy"
	entry.Data["proxy_scheme"] = "socks5"
	entry.Data["remote_transport"] = "tcp"
	entry.Data["media_session_id"] = "media-session-id"
	entry.Data["call_id"] = "call-id"
	entry.Data["peer"] = "remote"
	entry.Data["state"] = "connected"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		`credential="Voice credential\nsecondary"`,
		`connection="via socks5 proxy"`,
		`proxy_scheme="socks5"`,
		`remote_transport="tcp"`,
		`media_session_id="media-session-id"`,
		`call_id="call-id"`,
		`peer="remote"`,
		`state="connected"`,
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %s", line, want)
		}
	}
	if strings.Count(line, "\n") != 1 {
		t.Fatalf("formatted line contains an unescaped newline: %q", line)
	}
}

func TestLogFormatterPrintsPluginFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 25, 20, 10, 0, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "pluginhost: plugin loaded"
	entry.Data["plugin_id"] = "sample-provider"
	entry.Data["plugin_name"] = "Sample Provider"
	entry.Data["version"] = "0.2.0"
	entry.Data["active_version"] = "0.1.0"
	entry.Data["retired_version"] = "0.2.0"
	entry.Data["path"] = "plugins/windows/amd64/sample-provider-v0.2.0.dll"
	entry.Data["active_path"] = "plugins/windows/amd64/sample-provider-v0.1.0.dll"
	entry.Data["retired_path"] = "plugins/windows/amd64/sample-provider-v0.2.0.dll"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"plugin_id=sample-provider",
		"plugin_name=Sample Provider",
		"version=0.2.0",
		"active_version=0.1.0",
		"retired_version=0.2.0",
		"path=plugins/windows/amd64/sample-provider-v0.2.0.dll",
		"active_path=plugins/windows/amd64/sample-provider-v0.1.0.dll",
		"retired_path=plugins/windows/amd64/sample-provider-v0.2.0.dll",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %s", line, want)
		}
	}
}

func TestLogFormatterOmitsGenericPathField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 25, 20, 20, 0, 0, time.Local)
	entry.Level = log.WarnLevel
	entry.Message = "failed to roll back token"
	entry.Data["path"] = "auths/private-token.json"
	entry.Data["active_path"] = "plugins/windows/amd64/sample-provider-v0.1.0.dll"
	entry.Data["retired_path"] = "plugins/windows/amd64/sample-provider-v0.2.0.dll"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, forbidden := range []string{"path=", "active_path=", "retired_path="} {
		if strings.Contains(line, forbidden) {
			t.Fatalf("formatted line %q contains generic %s field", line, forbidden)
		}
	}
}

func TestLogFormatterPrintsCodexCredentialAndTurnIdentityPrefix(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 9, 19, 20, 58, 48, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = `200 |       19.892s | { 42 } |      172.20.0.1 | POST    "/v1/responses"`
	entry.Data["request_id"] = "2edd80ee"
	entry.Data["auth_file"] = "codex-user-windows.json"
	entry.Data["session_id"] = "session-1"
	entry.Data["turn_id"] = "turn-1"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}
	got := string(formatted)
	wantPrefix := "[2edd80ee] [codex-user-windows.json] [session-1] [turn-1] [info ]"
	if !strings.Contains(got, wantPrefix) {
		t.Fatalf("formatted line %q missing prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "{ 42 }") {
		t.Fatalf("formatted line %q missing selected turn-state length", got)
	}
}

func TestLogFormatterPrintsCodexTopicConnectionDiagnostics(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Level = log.InfoLevel
	entry.Message = "codex websockets: upstream disconnected"
	entry.Data = log.Fields{
		"scope": "topic", "topic": "codex-topic/hash", "connection": "physical-1",
		"auth": "fixture.json", "url": "wss://chatgpt.com/backend-api/codex/responses",
		"reason": "upstream_disconnected", "last_event": "error", "is_terminal": true,
		"Authorization": "secret-token", "Cookie": "private-cookie",
	}
	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	line := string(formatted)
	for _, want := range []string{`scope="topic"`, `topic="codex-topic/hash"`, `connection="physical-1"`, `auth="fixture.json"`, `url="wss://chatgpt.com/backend-api/codex/responses"`, `reason="upstream_disconnected"`, `last_event="error"`, `is_terminal=true`} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %s in %s", want, line)
		}
	}
	if strings.Contains(line, "secret-token") || strings.Contains(line, "private-cookie") {
		t.Fatal("sensitive fields were printed")
	}
	entry.Data["auth"] = "fixture\nsecondary"
	formatted, err = (&LogFormatter{}).Format(entry)
	if err != nil || strings.Count(string(formatted), "\n") != 1 {
		t.Fatalf("unsafe multiline diagnostic: %q, %v", formatted, err)
	}
	entry.Message = "unrelated request"
	formatted, err = (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"auth=", "url=", "topic="} {
		if strings.Contains(string(formatted), field) {
			t.Fatalf("generic %s field became visible", field)
		}
	}
}

func TestConfigureLogOutput_V8MigrationMirror(t *testing.T) {
	tempDir := t.TempDir()
	cfg := &config.Config{
		LoggingToFile: true,
		AuthDir:       tempDir,
	}

	origOut := log.StandardLogger().Out
	t.Cleanup(func() {
		closeLogOutputs()
		log.SetOutput(origOut)
	})

	if err := ConfigureLogOutput(cfg); err != nil {
		t.Fatalf("ConfigureLogOutput(LoggingToFile=true) error = %v", err)
	}

	var consoleBuf bytes.Buffer
	restore := setMigrationConsoleOutputForTest(&consoleBuf)
	t.Cleanup(restore)

	raw := []byte("some-unknown-section:\n  foo: bar\n")
	if _, _, err := config.NormalizeConfigLayout(raw, true); err != nil {
		t.Fatalf("NormalizeConfigLayout() error = %v", err)
	}

	if !strings.Contains(consoleBuf.String(), "some-unknown-section") {
		t.Fatalf("expected console mirror to capture migration warning under file logging, got: %s", consoleBuf.String())
	}

	// Switch back to stdout logging
	cfg.LoggingToFile = false
	consoleBuf.Reset()
	if err := ConfigureLogOutput(cfg); err != nil {
		t.Fatalf("ConfigureLogOutput(LoggingToFile=false) error = %v", err)
	}

	if _, _, err := config.NormalizeConfigLayout(raw, true); err != nil {
		t.Fatalf("NormalizeConfigLayout() error = %v", err)
	}

	// Under stdout logging, console mirror is false (it goes to stdout directly)
	if consoleBuf.Len() != 0 {
		t.Fatalf("expected console mirror to be disabled under stdout logging, got: %s", consoleBuf.String())
	}
}

func TestConfigureLogOutput_ConcurrentMigration(t *testing.T) {
	tempDir := t.TempDir()
	cfgFile := &config.Config{
		LoggingToFile: true,
		AuthDir:       tempDir,
	}
	cfgStdout := &config.Config{
		LoggingToFile: false,
		AuthDir:       tempDir,
	}

	origOut := log.StandardLogger().Out
	t.Cleanup(func() {
		closeLogOutputs()
		log.SetOutput(origOut)
	})

	var consoleBuf bytes.Buffer
	restore := setMigrationConsoleOutputForTest(&consoleBuf)
	t.Cleanup(restore)

	raw := []byte("some-unknown-section:\n  foo: bar\n")
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr error
	recordErr := func(err error) {
		if err != nil {
			errOnce.Do(func() { firstErr = err })
		}
	}
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func(iteration int) {
			defer wg.Done()
			var err error
			if iteration%2 == 0 {
				err = ConfigureLogOutput(cfgFile)
			} else {
				err = ConfigureLogOutput(cfgStdout)
			}
			recordErr(err)
		}(i)
		go func() {
			defer wg.Done()
			var localBuf bytes.Buffer
			r := setMigrationConsoleOutputForTest(&localBuf)
			defer r()
			_, _, err := config.NormalizeConfigLayout(raw, true)
			recordErr(err)
		}()
		go func() {
			defer wg.Done()
			_, _, err := config.NormalizeConfigLayout(raw, true)
			recordErr(err)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("concurrent migration/log output switch encountered error: %v", firstErr)
	}
}

func TestLogFormatterFormatsShortRequestID(t *testing.T) {
	formatter := &LogFormatter{}

	// Test case 1: UUID v7 full string truncated to trailing 8 chars in console log
	entryUUID := log.NewEntry(log.New())
	entryUUID.Time = time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	entryUUID.Level = log.InfoLevel
	entryUUID.Message = "handling request"
	entryUUID.Data["request_id"] = "018f3a5b-1234-7abc-def0-12345678abcd"

	formattedUUID, errFormatUUID := formatter.Format(entryUUID)
	if errFormatUUID != nil {
		t.Fatalf("Format() error = %v", errFormatUUID)
	}
	lineUUID := string(formattedUUID)
	if !strings.Contains(lineUUID, "[5678abcd]") {
		t.Fatalf("formatted line %q does not contain expected short request ID [5678abcd]", lineUUID)
	}
	if strings.Contains(lineUUID, "018f3a5b-1234-7abc-def0-12345678abcd") {
		t.Fatalf("formatted line %q should not contain full UUID", lineUUID)
	}

	// Test case 2: legacy or short 8-char request ID preserved
	entryShort := log.NewEntry(log.New())
	entryShort.Time = time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	entryShort.Level = log.InfoLevel
	entryShort.Message = "handling short id request"
	entryShort.Data["request_id"] = "00000042"

	formattedShort, errFormatShort := formatter.Format(entryShort)
	if errFormatShort != nil {
		t.Fatalf("Format() error = %v", errFormatShort)
	}
	lineShort := string(formattedShort)
	if !strings.Contains(lineShort, "[00000042]") {
		t.Fatalf("formatted line %q does not contain expected request ID [00000042]", lineShort)
	}

	// Test case 3: omitted request ID formats placeholder
	entryEmpty := log.NewEntry(log.New())
	entryEmpty.Time = time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	entryEmpty.Level = log.InfoLevel
	entryEmpty.Message = "system event"

	formattedEmpty, errFormatEmpty := formatter.Format(entryEmpty)
	if errFormatEmpty != nil {
		t.Fatalf("Format() error = %v", errFormatEmpty)
	}
	lineEmpty := string(formattedEmpty)
	if !strings.Contains(lineEmpty, "[--------]") {
		t.Fatalf("formatted line %q does not contain expected placeholder [--------]", lineEmpty)
	}
}
