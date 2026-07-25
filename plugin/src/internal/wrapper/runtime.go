package wrapper

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Provider identifiers recognised by ResolveProviderEnv.
const (
	ProviderAnthropic = "anthropic"
	ProviderZhipu     = "zhipu"
	ProviderDeepSeek  = "deepseek"
	ProviderCodex     = "codex"
	// ProviderEcho is a deterministic in-memory provider (tests + CMA-facade
	// e2e). It runs no CLI and needs no LLM env.
	ProviderEcho = "echo"
)

// zhipuDefaultBaseURL is the Anthropic-compatible endpoint published by
// Zhipu/智谱 for use with `claude -p` and other Anthropic SDK clients.
const zhipuDefaultBaseURL = "https://open.bigmodel.cn/api/anthropic"

// deepseekDefaultBaseURL is the Anthropic-compatible endpoint published by
// DeepSeek (parallel to zhipu — same ANTHROPIC_* env wiring).
const deepseekDefaultBaseURL = "https://api.deepseek.com/anthropic"

var (
	envKeyNameRE      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	legacyEnvAPIKeyRE = regexp.MustCompile(`^\$([A-Za-z_][A-Za-z0-9_]*)$`)
	legacyBraceKeyRE  = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
)

// CodexProviderConfig is the non-secret custom-provider configuration passed
// to Codex CLI overrides.
type CodexProviderConfig struct {
	BaseURL string
	WireAPI string
	EnvKey  string
}

// RuntimeProvider returns the canonical provider for a RuntimeConfig.
func RuntimeProvider(rt RuntimeConfig) string {
	provider := strings.ToLower(strings.TrimSpace(rt.Provider))
	if provider == "" {
		return ProviderAnthropic
	}
	return provider
}

// ResolveRuntimeModel expands runtime.model using the same strict env-var
// semantics as ResolveProviderEnv.
func ResolveRuntimeModel(rt RuntimeConfig) (string, error) {
	return expandStrict("runtime.model", rt.Model)
}

// ResolveRuntimeBaseURL expands runtime.baseURL using the same strict env-var
// semantics as ResolveProviderEnv. Codex uses the result to build a per-agent
// custom Responses API provider without modifying the operator's global
// config.toml.
func ResolveRuntimeBaseURL(rt RuntimeConfig) (string, error) {
	return expandStrict("runtime.baseURL", rt.BaseURL)
}

// ResolveCodexProvider validates the env-only credential contract without
// reading or returning the secret value.
func ResolveCodexProvider(rt RuntimeConfig) (CodexProviderConfig, error) {
	if rt.APIKey != "" {
		if name := legacyAPIKeyEnvName(rt.APIKey); name != "" {
			return CodexProviderConfig{}, fmt.Errorf("runtime.apiKey is no longer supported for provider=codex; use runtime.credential.envKey: %s", name)
		}
		return CodexProviderConfig{}, fmt.Errorf("runtime.apiKey must not contain a Codex provider secret; use runtime.credential.envKey")
	}

	baseURL, err := expandStrict("runtime.baseURL", rt.BaseURL)
	if err != nil {
		return CodexProviderConfig{}, err
	}
	envKey := strings.TrimSpace(rt.Credential.EnvKey)
	if envKey != "" && !envKeyNameRE.MatchString(envKey) {
		return CodexProviderConfig{}, fmt.Errorf("runtime.credential.envKey %q is not a valid environment variable name", envKey)
	}
	if baseURL != "" && envKey == "" {
		return CodexProviderConfig{}, fmt.Errorf("runtime.credential.envKey is required when runtime.baseURL is set (provider=codex)")
	}
	if envKey != "" {
		value, ok := os.LookupEnv(envKey)
		if !ok || value == "" {
			return CodexProviderConfig{}, fmt.Errorf("runtime.credential.envKey references missing or empty environment variable %s", envKey)
		}
	}
	wireAPI := strings.ToLower(strings.TrimSpace(rt.WireAPI))
	if wireAPI == "" {
		wireAPI = "responses"
	}
	if wireAPI != "responses" && wireAPI != "chat" {
		return CodexProviderConfig{}, fmt.Errorf("runtime.wireAPI %q is not supported for provider=codex; use responses or chat", rt.WireAPI)
	}
	return CodexProviderConfig{BaseURL: baseURL, WireAPI: wireAPI, EnvKey: envKey}, nil
}

func legacyAPIKeyEnvName(value string) string {
	for _, re := range []*regexp.Regexp{legacyEnvAPIKeyRE, legacyBraceKeyRE} {
		if match := re.FindStringSubmatch(value); len(match) == 2 {
			return match[1]
		}
	}
	return ""
}

// ResolveProviderEnv translates the high-level provider/baseURL/apiKey/model
// fields into the env-var shape the underlying CLI expects, then layers any
// user-supplied Env on top (so explicit Env wins over provider defaults).
//
// All input values run through expandStrict, which fails if a field
// references a shell variable that is not set. Empty/unset fields are fine —
// only fields the user explicitly wrote are validated. This catches the
// common footgun of forgetting `export ZHIPU_API_KEY=...` before launch.
//
// The returned map is *just the LLM-related additions* — caller is
// responsible for merging it with os.Environ() when building exec.Cmd.Env.
func ResolveProviderEnv(rt RuntimeConfig) (map[string]string, error) {
	provider := RuntimeProvider(rt)

	if provider == ProviderCodex {
		resolved, err := ResolveCodexProvider(rt)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for k, v := range rt.Env {
			if k == "OPENAI_API_KEY" || k == "CODEX_API_KEY" || k == resolved.EnvKey {
				return nil, fmt.Errorf("runtime.env.%s is reserved for Codex authentication; set credential.envKey and export the credential in the parent process environment", k)
			}
			expanded, err := expandStrict("runtime.env."+k, v)
			if err != nil {
				return nil, err
			}
			out[k] = expanded
		}
		return out, nil
	}

	baseURL, err := expandStrict("runtime.baseURL", rt.BaseURL)
	if err != nil {
		return nil, err
	}
	apiKey, err := expandStrict("runtime.apiKey", rt.APIKey)
	if err != nil {
		return nil, err
	}
	model, err := expandStrict("runtime.model", rt.Model)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}

	switch provider {
	case ProviderAnthropic, ProviderZhipu, ProviderDeepSeek:
		// Zhipu / DeepSeek both expose Anthropic-compatible endpoints, so the
		// env var names are identical to upstream Anthropic — only the default
		// URL differs per provider.
		switch {
		case provider == ProviderZhipu && baseURL == "":
			baseURL = zhipuDefaultBaseURL
		case provider == ProviderDeepSeek && baseURL == "":
			baseURL = deepseekDefaultBaseURL
		}
		if baseURL != "" {
			out["ANTHROPIC_BASE_URL"] = baseURL
		}
		if apiKey != "" {
			// Anthropic-compat clients accept either ANTHROPIC_API_KEY or
			// ANTHROPIC_AUTH_TOKEN; AUTH_TOKEN is what Zhipu's docs use and
			// also works for upstream Anthropic and DeepSeek.
			out["ANTHROPIC_AUTH_TOKEN"] = apiKey
		}
		if model != "" {
			out["ANTHROPIC_MODEL"] = model
		}
	case ProviderEcho:
		// Deterministic in-memory echo provider (tests + CMA-facade e2e). Runs
		// no CLI, so baseURL/apiKey/model are irrelevant and intentionally
		// ignored — leave out empty and skip the anthropic auth check below.
	default:
		// Unknown provider — refuse silently translating high-level fields,
		// but tolerate the case where the user only set Env explicitly.
		if baseURL != "" || apiKey != "" || model != "" {
			return nil, fmt.Errorf("unknown runtime.provider %q (high-level baseURL/apiKey/model fields can only be used with provider=anthropic|zhipu|deepseek|codex); use runtime.env for custom providers", rt.Provider)
		}
	}

	// Explicit Env entries override provider-derived defaults so users can
	// patch any single var without dropping the rest.
	for k, v := range rt.Env {
		expanded, err := expandStrict("runtime.env."+k, v)
		if err != nil {
			return nil, err
		}
		out[k] = expanded
	}

	// For Anthropic-compat providers (anthropic/zhipu/deepseek), if the user
	// wrote a non-default baseURL (i.e. talking to a third-party gateway), an
	// empty auth token will silently produce 401s downstream — fail fast
	// instead.
	if provider == ProviderAnthropic || provider == ProviderZhipu || provider == ProviderDeepSeek {
		if out["ANTHROPIC_BASE_URL"] != "" && out["ANTHROPIC_AUTH_TOKEN"] == "" {
			return nil, fmt.Errorf("runtime.apiKey is required when runtime.baseURL is set (provider=%s)", provider)
		}
	}

	return out, nil
}

// expandStrict expands ${VAR} / $VAR references in s using the parent
// environment. If a referenced variable is not set, it returns an error
// naming both the offending field and the missing variable, so configuration
// mistakes surface at agent startup instead of at first LLM call.
//
// Empty input is a no-op — only fields the user actually populated are
// validated.
func expandStrict(field, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	missingSet := map[string]struct{}{}
	expanded := os.Expand(s, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok {
			missingSet[name] = struct{}{}
		}
		return v
	})
	if len(missingSet) > 0 {
		missing := make([]string, 0, len(missingSet))
		for k := range missingSet {
			missing = append(missing, k)
		}
		sort.Strings(missing)
		return "", fmt.Errorf("%s references unset env vars: %s", field, strings.Join(missing, ", "))
	}
	return expanded, nil
}
