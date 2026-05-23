package sync

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// withEnv sets env vars for the duration of one test and restores prior
// values. Required because LoadConfig reads env directly.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	prior := map[string]*string{}
	for k, v := range kv {
		if old, ok := os.LookupEnv(k); ok {
			s := old
			prior[k] = &s
		} else {
			prior[k] = nil
		}
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	t.Cleanup(func() {
		for k, v := range prior {
			if v == nil {
				_ = os.Unsetenv(k)
			} else {
				_ = os.Setenv(k, *v)
			}
		}
	})
}

func TestConfig_DefaultsToPublicSeedOnly(t *testing.T) {
	withEnv(t, map[string]string{
		"STRUCTS_RPC_SEED":    "",
		"STRUCTS_RPC_PRIMARY": "",
		"STRUCTS_RPC_URL":     "",
	})
	cfg := LoadConfig(nil)
	if cfg.RPCSeed != DefaultRPCSeed {
		t.Fatalf("RPCSeed default = %q, want %q", cfg.RPCSeed, DefaultRPCSeed)
	}
	if cfg.RPCPrimary != "" {
		t.Fatalf("RPCPrimary should default empty, got %q", cfg.RPCPrimary)
	}
	urls := cfg.RPCURLs()
	if !reflect.DeepEqual(urls, []string{DefaultRPCSeed}) {
		t.Fatalf("RPCURLs() = %v, want [seed only]", urls)
	}
	if cfg.RPCDeprecationNotice != "" {
		t.Fatalf("no deprecation notice expected with clean env, got %q", cfg.RPCDeprecationNotice)
	}
}

func TestConfig_PrimaryThenSeedOrder(t *testing.T) {
	withEnv(t, map[string]string{
		"STRUCTS_RPC_PRIMARY": "http://structsd:26657",
		"STRUCTS_RPC_SEED":    "https://public.testnet.structs.network:26657",
		"STRUCTS_RPC_URL":     "",
	})
	cfg := LoadConfig(nil)
	urls := cfg.RPCURLs()
	want := []string{"http://structsd:26657", "https://public.testnet.structs.network:26657"}
	if !reflect.DeepEqual(urls, want) {
		t.Fatalf("RPCURLs() = %v, want %v", urls, want)
	}
}

func TestConfig_LegacyRPCBecomesPrimary_EmitsDeprecation(t *testing.T) {
	withEnv(t, map[string]string{
		"STRUCTS_RPC_URL":     "http://reactor.oh.energy:26657",
		"STRUCTS_RPC_PRIMARY": "",
		"STRUCTS_RPC_SEED":    "",
	})
	cfg := LoadConfig(nil)
	if cfg.RPCPrimary != "http://reactor.oh.energy:26657" {
		t.Fatalf("legacy STRUCTS_RPC_URL should become primary, got %q", cfg.RPCPrimary)
	}
	if cfg.RPCDeprecationNotice == "" || !strings.Contains(cfg.RPCDeprecationNotice, "deprecated") {
		t.Fatalf("expected deprecation notice, got %q", cfg.RPCDeprecationNotice)
	}
	urls := cfg.RPCURLs()
	want := []string{"http://reactor.oh.energy:26657", DefaultRPCSeed}
	if !reflect.DeepEqual(urls, want) {
		t.Fatalf("RPCURLs() = %v, want %v", urls, want)
	}
}

func TestConfig_ExplicitPrimaryOverridesLegacy(t *testing.T) {
	withEnv(t, map[string]string{
		"STRUCTS_RPC_URL":     "http://legacy.example:26657",
		"STRUCTS_RPC_PRIMARY": "http://structsd:26657",
		"STRUCTS_RPC_SEED":    "",
	})
	cfg := LoadConfig(nil)
	if cfg.RPCPrimary != "http://structsd:26657" {
		t.Fatalf("explicit -rpc-primary should win, got %q", cfg.RPCPrimary)
	}
	// Deprecation notice still fires because the legacy var is set even
	// though we're not using it as primary; informs the operator to
	// clean it up.
	if cfg.RPCDeprecationNotice == "" {
		t.Fatalf("expected deprecation notice when legacy var is set")
	}
}

func TestConfig_StatementTimeout_DefaultsTo60s(t *testing.T) {
	withEnv(t, map[string]string{"SYNC_STATE_STATEMENT_TIMEOUT": ""})
	cfg := LoadConfig(nil)
	if cfg.StatementTimeout != 60*time.Second {
		t.Fatalf("StatementTimeout default = %s, want 1m0s", cfg.StatementTimeout)
	}
}

func TestConfig_StatementTimeout_RespectsEnv(t *testing.T) {
	withEnv(t, map[string]string{"SYNC_STATE_STATEMENT_TIMEOUT": "5s"})
	cfg := LoadConfig(nil)
	if cfg.StatementTimeout != 5*time.Second {
		t.Fatalf("StatementTimeout = %s, want 5s", cfg.StatementTimeout)
	}
}

func TestConfig_StatementTimeout_DisabledZero(t *testing.T) {
	withEnv(t, map[string]string{"SYNC_STATE_STATEMENT_TIMEOUT": "0"})
	cfg := LoadConfig(nil)
	if cfg.StatementTimeout != 0 {
		t.Fatalf("StatementTimeout=0 should pass through, got %s", cfg.StatementTimeout)
	}
}

func TestConfig_PrimaryEqualSeed_Deduplicated(t *testing.T) {
	withEnv(t, map[string]string{
		"STRUCTS_RPC_PRIMARY": DefaultRPCSeed,
		"STRUCTS_RPC_SEED":    DefaultRPCSeed,
		"STRUCTS_RPC_URL":     "",
	})
	cfg := LoadConfig(nil)
	urls := cfg.RPCURLs()
	if len(urls) != 1 || urls[0] != DefaultRPCSeed {
		t.Fatalf("RPCURLs() with primary==seed should yield 1 entry, got %v", urls)
	}
}
