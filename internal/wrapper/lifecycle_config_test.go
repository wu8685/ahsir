package wrapper

import (
	"strings"
	"testing"
	"time"
)

func TestResolveLifecycleDisabledByDefault(t *testing.T) {
	r, err := ResolveLifecycle(LifecycleConfig{})
	if err != nil {
		t.Fatalf("empty lifecycle config should resolve cleanly: %v", err)
	}
	if r.Enabled() {
		t.Fatal("empty lifecycle config must stay disabled (legacy one-shot timeout behaviour)")
	}
	if r.Version == "" {
		t.Fatal("resolved lifecycle must always carry a version hash")
	}
}

func TestResolveLifecycleDefaults(t *testing.T) {
	r, err := ResolveLifecycle(LifecycleConfig{
		Initialize: LifecycleInitializeConfig{Enabled: true, Prompt: "init"},
		Finalize:   LifecycleFinalizeConfig{Enabled: true, WorkTimeout: "20m", Prompt: "wrap up"},
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if r.Initialize.Timeout != time.Minute {
		t.Fatalf("initialize.timeout default = %v, want 1m", r.Initialize.Timeout)
	}
	if r.Finalize.WorkTimeout != 20*time.Minute {
		t.Fatalf("finalize.work_timeout = %v, want 20m", r.Finalize.WorkTimeout)
	}
	if r.Finalize.GraceTimeout != 2*time.Minute {
		t.Fatalf("finalize.grace_timeout default = %v, want 2m", r.Finalize.GraceTimeout)
	}
	if r.Initialize.OnFailure != InitFailClosed {
		t.Fatalf("initialize.on_failure default = %q, want fail_closed", r.Initialize.OnFailure)
	}
	if !r.Enabled() {
		t.Fatal("enabled blocks must mark the resolved lifecycle enabled")
	}
}

func TestResolveLifecycleRejectsBadDurations(t *testing.T) {
	cases := []struct {
		name string
		cfg  LifecycleConfig
		want string
	}{
		{"init timeout unparsable", LifecycleConfig{Initialize: LifecycleInitializeConfig{Enabled: true, Timeout: "soon"}}, "lifecycle.initialize.timeout"},
		{"init timeout below floor", LifecycleConfig{Initialize: LifecycleInitializeConfig{Enabled: true, Timeout: "100ms"}}, "lifecycle.initialize.timeout"},
		{"init timeout above ceiling", LifecycleConfig{Initialize: LifecycleInitializeConfig{Enabled: true, Timeout: "2h"}}, "lifecycle.initialize.timeout"},
		{"work timeout unparsable", LifecycleConfig{Finalize: LifecycleFinalizeConfig{Enabled: true, WorkTimeout: "later"}}, "lifecycle.finalize.work_timeout"},
		{"grace unparsable", LifecycleConfig{Finalize: LifecycleFinalizeConfig{Enabled: true, WorkTimeout: "20m", GraceTimeout: "x"}}, "lifecycle.finalize.grace_timeout"},
		{"bad on_failure", LifecycleConfig{Initialize: LifecycleInitializeConfig{Enabled: true, OnFailure: "yolo"}}, "lifecycle.initialize.on_failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveLifecycle(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %s", err, tc.want)
			}
		})
	}
}

func TestResolveLifecycleFinalizeRequiresWorkTimeout(t *testing.T) {
	_, err := ResolveLifecycle(LifecycleConfig{Finalize: LifecycleFinalizeConfig{Enabled: true, GraceTimeout: "2m"}})
	if err == nil || !strings.Contains(err.Error(), "work_timeout") {
		t.Fatalf("finalize without work_timeout must fail: %v", err)
	}
}

func TestResolveLifecycleGraceMustNotExceedWork(t *testing.T) {
	_, err := ResolveLifecycle(LifecycleConfig{Finalize: LifecycleFinalizeConfig{Enabled: true, WorkTimeout: "1m", GraceTimeout: "5m"}})
	if err == nil || !strings.Contains(err.Error(), "grace_timeout") {
		t.Fatalf("grace_timeout > work_timeout must fail: %v", err)
	}
}

func TestLifecycleVersionChangesWithConfig(t *testing.T) {
	base := LifecycleConfig{Initialize: LifecycleInitializeConfig{Enabled: true, Prompt: "a"}}
	a, err := ResolveLifecycle(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Initialize.Prompt = "b"
	b, err := ResolveLifecycle(changed)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version == b.Version {
		t.Fatal("lifecycle version must change when the lifecycle config changes")
	}
	c, err := ResolveLifecycle(base)
	if err != nil {
		t.Fatal(err)
	}
	if a.Version != c.Version {
		t.Fatal("lifecycle version must be stable for identical config")
	}
}

func TestLifecycleConfigRoundTripsCardYAML(t *testing.T) {
	dir := t.TempDir()
	cfg := &AgentCardConfig{
		Name: "lc",
		Lifecycle: LifecycleConfig{
			Initialize: LifecycleInitializeConfig{Enabled: true, Timeout: "90s", Prompt: "read the docs"},
			Finalize:   LifecycleFinalizeConfig{Enabled: true, WorkTimeout: "20m", GraceTimeout: "2m", Prompt: "deliver now"},
		},
	}
	if err := WriteCard(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewAgentCardBuilder(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Lifecycle.Initialize.Enabled || loaded.Lifecycle.Initialize.Timeout != "90s" {
		t.Fatalf("lifecycle.initialize did not round-trip: %+v", loaded.Lifecycle.Initialize)
	}
	if loaded.Lifecycle.Finalize.WorkTimeout != "20m" || loaded.Lifecycle.Finalize.GraceTimeout != "2m" {
		t.Fatalf("lifecycle.finalize did not round-trip: %+v", loaded.Lifecycle.Finalize)
	}
}

func TestCardMatchesDetectsLifecycleChange(t *testing.T) {
	dir := t.TempDir()
	base := &AgentCardConfig{Name: "lc", Lifecycle: LifecycleConfig{
		Initialize: LifecycleInitializeConfig{Enabled: true, Prompt: "a"},
	}}
	if err := WriteCard(dir, base); err != nil {
		t.Fatal(err)
	}
	same, err := CardMatches(dir, base)
	if err != nil || !same {
		t.Fatalf("identical card should match: same=%v err=%v", same, err)
	}
	changed := &AgentCardConfig{Name: "lc", Lifecycle: LifecycleConfig{
		Initialize: LifecycleInitializeConfig{Enabled: true, Prompt: "b"},
	}}
	same, err = CardMatches(dir, changed)
	if err != nil {
		t.Fatal(err)
	}
	if same {
		t.Fatal("lifecycle change must participate in card versioning (CardMatches=false)")
	}
}

func TestLoadRejectsInvalidLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := &AgentCardConfig{Name: "lc", Lifecycle: LifecycleConfig{
		Finalize: LifecycleFinalizeConfig{Enabled: true, WorkTimeout: "not-a-duration"},
	}}
	if err := WriteCard(dir, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgentCardBuilder(dir).Load(); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatalf("Load must reject invalid lifecycle config: %v", err)
	}
}
