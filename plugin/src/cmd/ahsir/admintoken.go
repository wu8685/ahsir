package main

import "github.com/wu8685/ahsir/internal/scheduler"

// resolveAdminToken finds the control-plane admin token the CLI should attach
// to privileged requests (agent new/delete). It mirrors the scheduler's
// read-only resolution (AHSIR_ADMIN_TOKEN env → the 0600 file beside
// ahsir.yaml) but never generates a token — the CLI must not create a secret
// the scheduler doesn't know about. Empty means "no token found"; the request
// then proceeds tokenless and surfaces the scheduler's 401 with a clear hint.
func resolveAdminToken(configPath string) string {
	token, _ := scheduler.ReadAdminToken(configPath)
	return token
}
