package main

import (
	"testing"

	"gogen/internal/config"
)

func TestProcessListenAddr(t *testing.T) {
	t.Run("config bind when the host env is unset", func(t *testing.T) {
		t.Setenv(config.WebListenEnv, "")
		if got := processListenAddr("127.0.0.1:8081"); got != "127.0.0.1:8081" {
			t.Fatalf("listen = %q, want the config bind", got)
		}
	})
	t.Run("host env wins and is not written back", func(t *testing.T) {
		t.Setenv(config.WebListenEnv, "  127.0.0.1:53244  ")
		cfgBind := "0.0.0.0:9090"
		if got := processListenAddr(cfgBind); got != "127.0.0.1:53244" {
			t.Fatalf("listen = %q, want the host port", got)
		}
		if cfgBind != "0.0.0.0:9090" {
			t.Fatalf("config bind changed to %q", cfgBind)
		}
	})
}
