package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/wu8685/ahsir/internal/cmagateway"
	cmaclient "github.com/wu8685/ahsir/internal/cmagateway/ahsir"
	cmastore "github.com/wu8685/ahsir/internal/cmagateway/store"
)

// startCMAFacade brings up the CMA-compatible HTTP API as a SECOND listener in
// the ahsir process (P1 of the cma-service→ahsir gateway migration). It speaks
// the CMA wire and drives THIS scheduler over loopback (schedulerURL) using the
// ported gateway client — no in-process coupling yet, that's a later refinement.
//
// Opt-in only: `ahsir start --cma-listen ADDR`. When the flag is empty the
// facade is not started, so the default `ahsir start` is byte-for-byte unchanged
// and the standalone cma-service keeps owning :18790 during the migration.
//
// The listener runs in a goroutine; the caller keeps blocking on the shutdown
// signal, so a facade bind error is fatal (nothing else would surface it).
func startCMAFacade(listen, schedulerURL, adminToken, statePath string) error {
	st, err := cmastore.New(statePath)
	if err != nil {
		return err
	}
	ac := cmaclient.New(schedulerURL, adminToken)
	cfg := cmagateway.Config{
		// APIKeys empty => allow all (local/zero-config). A shared deployment
		// would populate an x-api-key allowlist here.
		RuntimeProvider: cmaEnvOr("CMA_RUNTIME_PROVIDER", "anthropic"),
		RuntimeBaseURL:  os.Getenv("CMA_RUNTIME_BASE_URL"),
		RuntimeAPIKey:   os.Getenv("CMA_RUNTIME_API_KEY"),
		// Non-positive lets the handler pick its own default; a big value keeps
		// long agent turns from being cut short (matches the cma-service plist).
		TurnTimeout: cmaDurEnv("CMA_TURN_TIMEOUT", 720*time.Hour),
	}
	srv := cmagateway.New(cfg, st, ac)
	log.Printf("CMA facade listening on %s (scheduler=%s, state=%s)", listen, schedulerURL, statePath)
	go func() {
		if err := http.ListenAndServe(listen, srv.Handler()); err != nil {
			log.Fatalf("CMA facade listener on %s: %v", listen, err)
		}
	}()
	return nil
}

func cmaEnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func cmaDurEnv(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
