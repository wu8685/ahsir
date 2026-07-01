package scheduler

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func TestLoadOrCreateAdminTokenGeneratesFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ahsir.yaml")
	t.Setenv("AHSIR_ADMIN_TOKEN", "")

	tok, source, err := LoadOrCreateAdminToken(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hex64.MatchString(tok) {
		t.Fatalf("token = %q, want 64 hex chars", tok)
	}
	if source != adminTokenSourceFileGenerated {
		t.Fatalf("source = %q, want %q", source, adminTokenSourceFileGenerated)
	}

	tokenPath := filepath.Join(dir, "admin-token")
	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("token file not created: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %o, want 600", fi.Mode().Perm())
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	// The config dir already exists here (TempDir); the 0700 contract is
	// asserted in the MkdirAll path test below.
	_ = di
}

func TestLoadOrCreateAdminTokenReusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ahsir.yaml")
	t.Setenv("AHSIR_ADMIN_TOKEN", "")

	first, _, err := LoadOrCreateAdminToken(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	second, source, err := LoadOrCreateAdminToken(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("token changed across loads: %q vs %q", first, second)
	}
	if source != adminTokenSourceFile {
		t.Fatalf("second load source = %q, want %q", source, adminTokenSourceFile)
	}
}

func TestLoadOrCreateAdminTokenEnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ahsir.yaml")
	t.Setenv("AHSIR_ADMIN_TOKEN", "env-secret-123")

	tok, source, err := LoadOrCreateAdminToken(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "env-secret-123" {
		t.Fatalf("token = %q, want env value", tok)
	}
	if source != adminTokenSourceEnv {
		t.Fatalf("source = %q, want %q", source, adminTokenSourceEnv)
	}
	// Env override must not touch the filesystem.
	if _, err := os.Stat(filepath.Join(dir, "admin-token")); !os.IsNotExist(err) {
		t.Errorf("env override must not create a token file, stat err = %v", err)
	}
}

func TestLoadOrCreateAdminTokenRegeneratesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ahsir.yaml")
	tokenPath := filepath.Join(dir, "admin-token")
	t.Setenv("AHSIR_ADMIN_TOKEN", "")
	if err := os.WriteFile(tokenPath, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tok, source, err := LoadOrCreateAdminToken(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !hex64.MatchString(tok) {
		t.Fatalf("regenerated token = %q, want 64 hex chars", tok)
	}
	if source != adminTokenSourceFileGenerated {
		t.Fatalf("source = %q, want regenerated", source)
	}
}

func TestLoadOrCreateAdminTokenCreatesConfigDir0700(t *testing.T) {
	base := t.TempDir()
	cfgDir := filepath.Join(base, "nested", "ahsir")
	cfgPath := filepath.Join(cfgDir, "ahsir.yaml")
	t.Setenv("AHSIR_ADMIN_TOKEN", "")

	if _, _, err := LoadOrCreateAdminToken(cfgPath); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(cfgDir)
	if err != nil {
		t.Fatalf("config dir not created: %v", err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode = %o, want 700", fi.Mode().Perm())
	}
}

func TestSchedulerLoadAdminTokenFromConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AHSIR_ADMIN_TOKEN", "")
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.path = filepath.Join(dir, "ahsir.yaml")
	cfg.nextPort = cfg.PortRange.Start

	sch := New(cfg)
	if err := sch.loadAdminToken(); err != nil {
		t.Fatal(err)
	}
	first := sch.adminToken()
	if !hex64.MatchString(first) {
		t.Fatalf("adminToken = %q, want 64 hex", first)
	}

	// A second scheduler over the same config path reuses the token.
	cfg2 := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg2.path = cfg.path
	sch2 := New(cfg2)
	if err := sch2.loadAdminToken(); err != nil {
		t.Fatal(err)
	}
	if sch2.adminToken() != first {
		t.Fatalf("token changed across schedulers: %q vs %q", sch2.adminToken(), first)
	}
}

func TestSchedulerLoadAdminTokenNoPathNoEnvDisablesAuth(t *testing.T) {
	t.Setenv("AHSIR_ADMIN_TOKEN", "")
	cfg := &Config{
		Registry:  RegistryConfig{Host: "127.0.0.1", Port: freePort(t)},
		PortRange: PortRange{Start: 9801, End: 9900},
	}
	cfg.nextPort = cfg.PortRange.Start
	sch := New(cfg)
	if err := sch.loadAdminToken(); err != nil {
		t.Fatal(err)
	}
	if sch.adminToken() != "" {
		t.Fatalf("bare scheduler (no path, no env) must leave auth disabled, got %q", sch.adminToken())
	}
}
