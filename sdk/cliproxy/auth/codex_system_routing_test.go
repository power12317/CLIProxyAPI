package auth

import (
	"context"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func codexSystemRoutingTestAuth(id, accountID, email, system string) *Auth {
	return &Auth{
		ID:       id,
		Provider: "codex",
		Metadata: map[string]any{
			"access_token":        "access-" + id,
			"account_id":          accountID,
			"email":               email,
			"codex_client_system": system,
		},
	}
}

func codexSystemRoutingTestEligibility(t *testing.T, manager *Manager, payload string) authSelectionEligibility {
	t.Helper()
	opts := cliproxyexecutor.Options{}
	WithCodexClientSystemMetadata(&opts, []byte(payload))
	manager.withCodexSystemPairMetadata(&opts)
	return authSelectionEligibilityForRequest(context.Background(), opts)
}

func registerCodexSystemRoutingTestAuth(t *testing.T, manager *Manager, auth *Auth) *Auth {
	t.Helper()
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("Register() error = %v", errRegister)
	}
	return registered
}

func TestCodexSystemRoutingIgnoresSourceSystemForSingleCredentialAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	macAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("mac-only", "account-1", "one@example.com", "mac"))

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"windows_sandbox\"}"}}`)
	if !eligibility.allows(macAuth) {
		t.Fatal("Windows source request must be allowed to use the account's only macOS credential")
	}
}

func TestCodexSystemRoutingAllowsMacSourceToUseSingleWindowsCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	windowsAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("windows-only", "account-1", "one@example.com", "windows"))

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"workspace-write\"}"}}`)
	if !eligibility.allows(windowsAuth) {
		t.Fatal("macOS source request must be allowed to use the account's only Windows credential")
	}
}

func TestCodexSystemRoutingUsesMatchingCredentialForPairedAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	macAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("paired-mac", "account-1", "one@example.com", "mac"))
	windowsAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("paired-windows", "account-1", "one@example.com", "windows"))

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"windows_sandbox\"}"}}`)
	if eligibility.allows(macAuth) {
		t.Fatal("Windows source request must not use the paired account's macOS credential")
	}
	if !eligibility.allows(windowsAuth) {
		t.Fatal("Windows source request must use the paired account's Windows credential")
	}
}

func TestCodexSystemRoutingUsesMacCredentialForPairedAccount(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	macAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("paired-mac", "account-1", "one@example.com", "mac"))
	windowsAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("paired-windows", "account-1", "one@example.com", "windows"))

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"workspace-write\"}"}}`)
	if !eligibility.allows(macAuth) {
		t.Fatal("macOS source request must use the paired account's macOS credential")
	}
	if eligibility.allows(windowsAuth) {
		t.Fatal("macOS source request must not use the paired account's Windows credential")
	}
}

func TestCodexSystemRoutingKeepsDifferentSingleSystemAccountsEligible(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	macAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("account-one-mac", "account-1", "one@example.com", "mac"))
	windowsAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("account-two-windows", "account-2", "two@example.com", "windows"))

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"windows_sandbox\"}"}}`)
	if !eligibility.allows(macAuth) || !eligibility.allows(windowsAuth) {
		t.Fatal("single-system credentials belonging to different accounts must remain eligible")
	}
}

func TestCodexSystemRoutingDisabledCounterpartFallsBackToSingleCredential(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	macAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("active-mac", "account-1", "one@example.com", "mac"))
	disabledWindows := codexSystemRoutingTestAuth("disabled-windows", "account-1", "one@example.com", "windows")
	disabledWindows.Disabled = true
	registerCodexSystemRoutingTestAuth(t, manager, disabledWindows)

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"windows_sandbox\"}"}}`)
	if !eligibility.allows(macAuth) {
		t.Fatal("disabled Windows counterpart must not force system-scoped routing")
	}
}

func TestCodexSystemRoutingLeavesNonOfficialRequestsUnscoped(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	macAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("paired-mac", "account-1", "one@example.com", "mac"))
	windowsAuth := registerCodexSystemRoutingTestAuth(t, manager, codexSystemRoutingTestAuth("paired-windows", "account-1", "one@example.com", "windows"))

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"model":"gpt-5.6-terra"}`)
	if !eligibility.allows(macAuth) || !eligibility.allows(windowsAuth) {
		t.Fatal("request without Codex turn metadata must retain existing selection behavior")
	}
}

func TestCodexSystemRoutingKeepsAPIKeyCredentialsOutOfOfficialOAuthSelection(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	apiKeyAuth := &Auth{
		ID:       "codex-api-key",
		Provider: "codex",
		Attributes: map[string]string{
			AttributeAPIKey: "sk-test",
		},
	}

	eligibility := codexSystemRoutingTestEligibility(t, manager, `{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"windows_sandbox\"}"}}`)
	if eligibility.allows(apiKeyAuth) {
		t.Fatal("official Codex system routing must not select an API-key credential")
	}
}
