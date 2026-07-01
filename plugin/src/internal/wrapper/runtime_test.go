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

func TestResolveProviderEnv_CodexAPIKey(t *testing.T) {
	got, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "codex",
		APIKey:   "sk-test",
		Model:    "gpt-5.4",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got["CODEX_API_KEY"] != "sk-test" {
		t.Errorf("CODEX_API_KEY not set: %v", got)
	}
	if _, ok := got["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Errorf("codex provider should not set Anthropic env: %v", got)
	}
}

func TestResolveProviderEnv_CodexRejectsBaseURL(t *testing.T) {
	_, err := ResolveProviderEnv(RuntimeConfig{
		Provider: "codex",
		BaseURL:  "https://example.com",
		APIKey:   "sk-test",
	})
	if err == nil || !strings.Contains(err.Error(), "provider=codex") {
		t.Fatalf("expected codex baseURL error, got %v", err)
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
