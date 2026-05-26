//go:build mintvk

// mint-test-vk — one-shot CLI that logs into the local CP with
// admin@nexus.ai/admin123 via OAuth+PKCE, creates a permanent virtual
// key under "test-vk-readiness-<unix>", and prints the raw key to
// stdout. Used by the readiness sign-off flow to refresh
// tests/.env.local's NEXUS_TEST_VK after schema/seed changes.
//
// Run with: cd tests/scenarios && go run -tags mintvk ../scripts/mint-test-vk.go
//
// Build-tagged so it doesn't fight the scenarios package's main_test.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	intg "github.com/AlphaBitCore/nexus-gateway/tests/integration-go/helpers"
	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

func main() {
	env, err := intg.LoadEnv()
	if err != nil {
		die("load env: %v", err)
	}
	helpers.MustBeLocalTarget(env)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	helpers.ResetTokenCache()
	token, err := helpers.CPLogin(ctx, env)
	if err != nil {
		die("login: %v", err)
	}

	name := fmt.Sprintf("test-vk-readiness-%d", time.Now().Unix())
	vk, err := helpers.CreateMyVK(ctx, env, token, name)
	if err != nil {
		die("create vk: %v", err)
	}
	fmt.Println(vk.RawKey)
}

func die(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	os.Exit(1)
}
