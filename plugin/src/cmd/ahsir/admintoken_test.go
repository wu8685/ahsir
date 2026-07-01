package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAdminTokenEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ahsir.yaml")
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AHSIR_ADMIN_TOKEN", "env-tok")

	if got := resolveAdminToken(cfgPath); got != "env-tok" {
		t.Fatalf("resolveAdminToken = %q, want env-tok", got)
	}
}

func TestResolveAdminTokenReadsFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "ahsir.yaml")
	if err := os.WriteFile(filepath.Join(dir, "admin-token"), []byte("file-tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AHSIR_ADMIN_TOKEN", "")

	if got := resolveAdminToken(cfgPath); got != "file-tok" {
		t.Fatalf("resolveAdminToken = %q, want file-tok", got)
	}
}

func TestResolveAdminTokenMissingBothEmpty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AHSIR_ADMIN_TOKEN", "")
	if got := resolveAdminToken(filepath.Join(dir, "ahsir.yaml")); got != "" {
		t.Fatalf("resolveAdminToken = %q, want empty", got)
	}
}

func TestRequestStartAttachesAdminToken(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Ahsir-Admin-Token")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"name":"x","port":9901}`))
	}))
	defer srv.Close()

	if _, err := requestStart(srv.URL, "x", "/tmp/x", "", "my-admin-tok"); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "my-admin-tok" {
		t.Fatalf("admin token header = %q, want my-admin-tok", gotHeader)
	}
}

func TestRequestStopAttachesAdminToken(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Ahsir-Admin-Token")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := requestStop(srv.URL, "x", "my-admin-tok"); err != nil {
		t.Fatal(err)
	}
	if gotHeader != "my-admin-tok" {
		t.Fatalf("admin token header = %q, want my-admin-tok", gotHeader)
	}
}
