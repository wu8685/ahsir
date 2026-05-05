package wrapper

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// Provider identifiers recognised by ResolveProviderEnv.
const (
	ProviderAnthropic = "anthropic"
	ProviderZhipu     = "zhipu"
)

// zhipuDefaultBaseURL is the Anthropic-compatible endpoint published by
// Zhipu/智谱 for use with `claude -p` and other Anthropic SDK clients.
const zhipuDefaultBaseURL = "https://open.bigmodel.cn/api/anthropic"

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
	provider := strings.ToLower(strings.TrimSpace(rt.Provider))
	if provider == "" {
		provider = ProviderAnthropic
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
	case ProviderAnthropic, ProviderZhipu:
		// Zhipu uses the Anthropic-compatible endpoint, so the env var names
		// are identical — only the default URL differs.
		if provider == ProviderZhipu && baseURL == "" {
			baseURL = zhipuDefaultBaseURL
		}
		if baseURL != "" {
			out["ANTHROPIC_BASE_URL"] = baseURL
		}
		if apiKey != "" {
			// Anthropic-compat clients accept either ANTHROPIC_API_KEY or
			// ANTHROPIC_AUTH_TOKEN; AUTH_TOKEN is what Zhipu's docs use and
			// also works for upstream Anthropic.
			out["ANTHROPIC_AUTH_TOKEN"] = apiKey
		}
		if model != "" {
			out["ANTHROPIC_MODEL"] = model
		}
	default:
		// Unknown provider — refuse silently translating high-level fields,
		// but tolerate the case where the user only set Env explicitly.
		if baseURL != "" || apiKey != "" || model != "" {
			return nil, fmt.Errorf("unknown runtime.provider %q (high-level baseURL/apiKey/model fields can only be used with provider=anthropic|zhipu); use runtime.env for custom providers", rt.Provider)
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

	// For Anthropic-compat providers (anthropic/zhipu), if the user wrote a
	// non-default baseURL (i.e. talking to a third-party gateway), an empty
	// auth token will silently produce 401s downstream — fail fast instead.
	if provider == ProviderAnthropic || provider == ProviderZhipu {
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
