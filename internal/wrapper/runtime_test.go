package wrapper

import (
	"os"
	"strings"
	"testing"
)

func TestResolveProviderEnv_DefaultAnthropic(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{
		BaseURL: "https://api.anthropic.com",
		APIKey:  "sk-test",
		Model:   "claude-opus-4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_BASE_URL"] != "https://api.anthropic.com" {
		t.Errorf("ANTHROPIC_BASE_URL not set: %v", got)
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "sk-test" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN not set: %v", got)
	}
	if got["ANTHROPIC_MODEL"] != "claude-opus-4" {
		t.Errorf("ANTHROPIC_MODEL not set: %v", got)
	}
}

func TestResolveProviderEnv_ZhipuDefaultsBaseURL(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "zhipu",
		APIKey:   "zp-fake",
		Model:    "glm-4.6",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_BASE_URL"] != zhipuDefaultBaseURL {
		t.Errorf("expected Zhipu default baseURL, got %q", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "zp-fake" {
		t.Errorf("auth token wrong: %v", got)
	}
	if got["ANTHROPIC_MODEL"] != "glm-4.6" {
		t.Errorf("model wrong: %v", got)
	}
}

func TestResolveProviderEnv_DeepSeekDefaultsBaseURL(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "deepseek",
		APIKey:   "ds-fake",
		Model:    "deepseek-v4-pro",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_BASE_URL"] != deepseekDefaultBaseURL {
		t.Errorf("expected DeepSeek default baseURL, got %q", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "ds-fake" {
		t.Errorf("auth token wrong: %v", got)
	}
	if got["ANTHROPIC_MODEL"] != "deepseek-v4-pro" {
		t.Errorf("model wrong: %v", got)
	}
}

func TestResolveCodexProviderAcceptsEnvCredential(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY_TESTONLY", "secret-must-not-escape")
	got, err := ResolveCodexProvider(RuntimeConfig{
		Provider: "codex",
		BaseURL:  "http://127.0.0.1:18793/v1",
		Model:    "k3",
		Credential: RuntimeCredentialConfig{
			EnvKey: "MOONSHOT_API_KEY_TESTONLY",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvKey != "MOONSHOT_API_KEY_TESTONLY" {
		t.Fatalf("EnvKey = %q", got.EnvKey)
	}
	if got.WireAPI != "responses" {
		t.Fatalf("WireAPI = %q, want responses", got.WireAPI)
	}
}

func TestResolveCodexProviderRejectsLiteralAPIKeyWithoutEcho(t *testing.T) {
	const secret = "sk-secret-must-not-appear"
	_, err := ResolveCodexProvider(RuntimeConfig{
		Provider: "codex",
		APIKey:   secret,
	})
	if err == nil {
		t.Fatal("expected literal apiKey rejection")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestResolveCodexProviderRejectsLegacyEnvReferenceWithHint(t *testing.T) {
	_, err := ResolveCodexProvider(RuntimeConfig{
		Provider: "codex",
		APIKey:   "${MOONSHOT_API_KEY}",
	})
	if err == nil || !strings.Contains(err.Error(), "credential.envKey: MOONSHOT_API_KEY") {
		t.Fatalf("expected migration hint, got %v", err)
	}
}

func TestResolveCodexProviderValidatesEnvCredentialAndWireAPI(t *testing.T) {
	os.Unsetenv("MISSING_CODEX_KEY_TESTONLY")
	t.Setenv("EMPTY_CODEX_KEY_TESTONLY", "")
	t.Setenv("PRESENT_CODEX_KEY_TESTONLY", "secret")

	tests := []struct {
		name string
		rt   RuntimeConfig
		want string
	}{
		{
			name: "malformed env name",
			rt: RuntimeConfig{
				Provider:   "codex",
				Credential: RuntimeCredentialConfig{EnvKey: "NOT-AN-ENV"},
			},
			want: "valid environment variable name",
		},
		{
			name: "missing env",
			rt: RuntimeConfig{
				Provider:   "codex",
				Credential: RuntimeCredentialConfig{EnvKey: "MISSING_CODEX_KEY_TESTONLY"},
			},
			want: "MISSING_CODEX_KEY_TESTONLY",
		},
		{
			name: "empty env",
			rt: RuntimeConfig{
				Provider:   "codex",
				Credential: RuntimeCredentialConfig{EnvKey: "EMPTY_CODEX_KEY_TESTONLY"},
			},
			want: "EMPTY_CODEX_KEY_TESTONLY",
		},
		{
			name: "base URL requires credential",
			rt: RuntimeConfig{
				Provider: "codex",
				BaseURL:  "https://example.test/v1",
			},
			want: "credential.envKey is required",
		},
		{
			name: "unsupported wire API",
			rt: RuntimeConfig{
				Provider:   "codex",
				WireAPI:    "completions",
				Credential: RuntimeCredentialConfig{EnvKey: "PRESENT_CODEX_KEY_TESTONLY"},
			},
			want: "use responses or chat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveCodexProvider(tt.rt)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}

	got, err := ResolveCodexProvider(RuntimeConfig{
		Provider:   "codex",
		WireAPI:    "chat",
		Credential: RuntimeCredentialConfig{EnvKey: "PRESENT_CODEX_KEY_TESTONLY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.WireAPI != "chat" {
		t.Fatalf("WireAPI = %q, want chat", got.WireAPI)
	}
}

func TestResolveProviderEnv_CodexDoesNotSynthesizeAPIKey(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY_TESTONLY", "secret-must-not-escape")
	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider:   "codex",
		Credential: RuntimeCredentialConfig{EnvKey: "MOONSHOT_API_KEY_TESTONLY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["CODEX_API_KEY"]; ok {
		t.Fatalf("CODEX_API_KEY must not be synthesized: %v", got)
	}
	if _, ok := got["OPENAI_API_KEY"]; ok {
		t.Fatalf("OPENAI_API_KEY must not be synthesized: %v", got)
	}
}

func TestResolveProviderEnv_CodexRejectsCredentialValuesInRuntimeEnv(t *testing.T) {
	t.Setenv("MOONSHOT_API_KEY_TESTONLY", "inherited-secret")

	for _, key := range []string{
		"OPENAI_API_KEY",
		"CODEX_API_KEY",
		"MOONSHOT_API_KEY_TESTONLY",
	} {
		t.Run(key, func(t *testing.T) {
			const literalSecret = "literal-secret-must-not-appear"
			_, err := ResolveProviderEnv(RuntimeConfig{
				Provider:   "codex",
				Credential: RuntimeCredentialConfig{EnvKey: "MOONSHOT_API_KEY_TESTONLY"},
				Env:        map[string]string{key: literalSecret},
			})
			if err == nil {
				t.Fatalf("expected runtime.env.%s to be rejected", key)
			}
			if !strings.Contains(err.Error(), "runtime.env."+key) {
				t.Fatalf("error must identify rejected field, got: %v", err)
			}
			if strings.Contains(err.Error(), literalSecret) {
				t.Fatalf("error leaked credential value: %v", err)
			}
		})
	}
}

func TestResolveRuntimeBaseURL_ExpandsEnv(t *testing.T) {
	t.Setenv("CODEX_PROXY_BASE", "http://127.0.0.1:18793/v1")
	got, err := ResolveRuntimeBaseURL(RuntimeConfig{BaseURL: "${CODEX_PROXY_BASE}"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:18793/v1" {
		t.Fatalf("baseURL = %q", got)
	}
}

func TestResolveProviderEnv_ZhipuExplicitBaseURLWins(t *testing.T) {
	got, _ := ResolveProviderEnv(RuntimeConfig{
		Provider: "Zhipu", // also tests case-insensitivity
		BaseURL:  "https://custom.example.com/anthropic",
		APIKey:   "k",
	})
	if got["ANTHROPIC_BASE_URL"] != "https://custom.example.com/anthropic" {
		t.Errorf("explicit baseURL should win, got %q", got["ANTHROPIC_BASE_URL"])
	}
}

func TestResolveProviderEnv_EnvVarExpansion(t *testing.T) {
	t.Setenv("MY_ZP_KEY", "expanded-secret")
	t.Setenv("MY_BASE", "https://h.example/anthropic")

	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "zhipu",
		BaseURL:  "${MY_BASE}",
		APIKey:   "$MY_ZP_KEY",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_BASE_URL"] != "https://h.example/anthropic" {
		t.Errorf("baseURL expansion failed: %q", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "expanded-secret" {
		t.Errorf("apiKey expansion failed: %q", got["ANTHROPIC_AUTH_TOKEN"])
	}
}

func TestResolveProviderEnv_ExplicitEnvOverridesProvider(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "zhipu",
		APIKey:   "from-apikey-field",
		Env: map[string]string{
			"ANTHROPIC_AUTH_TOKEN": "from-explicit-env",
			"EXTRA":                "yes",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["ANTHROPIC_AUTH_TOKEN"] != "from-explicit-env" {
		t.Errorf("explicit env should win, got %q", got["ANTHROPIC_AUTH_TOKEN"])
	}
	if got["EXTRA"] != "yes" {
		t.Errorf("extra env not propagated: %v", got)
	}
}

func TestResolveProviderEnv_UnknownProviderRejectsHighLevelFields(t *testing.T) {
	_, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "openai",
		BaseURL:  "https://api.openai.com",
		APIKey:   "x",
	})
	if err == nil {
		t.Fatal("expected error for unknown provider with high-level fields")
	}
}

func TestResolveProviderEnv_UnknownProviderEnvOnlyOK(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "openai",
		Env: map[string]string{
			"OPENAI_API_KEY": "k",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["OPENAI_API_KEY"] != "k" {
		t.Errorf("expected OPENAI_API_KEY passthrough, got %v", got)
	}
}

// TestResolveProviderEnv_FailsOnMissingEnvVarInAPIKey is the headline
// validation test: forgetting to `export ZHIPU_API_KEY=...` should fail
// agent startup with a message naming both the field and the variable.
func TestResolveProviderEnv_FailsOnMissingEnvVarInAPIKey(t *testing.T) {
	os.Unsetenv("ZHIPU_API_KEY_TESTONLY")
	_, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "zhipu",
		APIKey:   "${ZHIPU_API_KEY_TESTONLY}",
		Model:    "glm-5.1",
	})
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
	if !strings.Contains(err.Error(), "runtime.apiKey") {
		t.Errorf("error should name the field, got: %v", err)
	}
	if !strings.Contains(err.Error(), "ZHIPU_API_KEY_TESTONLY") {
		t.Errorf("error should name the missing var, got: %v", err)
	}
}

func TestResolveProviderEnv_FailsOnMissingEnvVarInBaseURL(t *testing.T) {
	os.Unsetenv("MY_GATEWAY_TESTONLY")
	_, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "anthropic",
		BaseURL:  "${MY_GATEWAY_TESTONLY}",
		APIKey:   "k",
	})
	if err == nil || !strings.Contains(err.Error(), "runtime.baseURL") {
		t.Fatalf("expected runtime.baseURL error, got: %v", err)
	}
}

func TestResolveProviderEnv_FailsOnMissingEnvVarInExplicitEnv(t *testing.T) {
	os.Unsetenv("MISSING_X_TESTONLY")
	_, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "openai",
		Env: map[string]string{
			"OPENAI_API_KEY": "${MISSING_X_TESTONLY}",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime.env.OPENAI_API_KEY") {
		t.Fatalf("expected runtime.env.OPENAI_API_KEY error, got: %v", err)
	}
}

// TestResolveProviderEnv_FailsWhenZhipuMissingAPIKey covers the case where
// the user forgot to set apiKey at all but Zhipu still auto-fills the
// baseURL — empty auth token would 401 silently, so we error early.
func TestResolveProviderEnv_FailsWhenZhipuMissingAPIKey(t *testing.T) {
	_, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "zhipu",
		Model:    "glm-5.1",
		// no apiKey
	})
	if err == nil || !strings.Contains(err.Error(), "runtime.apiKey is required") {
		t.Fatalf("expected required-apiKey error, got: %v", err)
	}
}

// TestResolveProviderEnv_AnthropicNoFieldsOK verifies that the
// "claude CLI uses its own OAuth login" path still works — no fields set,
// no error, no env injection.
func TestResolveProviderEnv_AnthropicNoFieldsOK(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty env, got %v", got)
	}
}
