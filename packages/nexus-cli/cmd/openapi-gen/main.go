// Command openapi-gen generates OpenAPI 3.1 specifications for the Nexus
// Control Plane admin API from the control-plane Go source. It is a maintainer
// tool run inside the monorepo (it type-checks the control-plane packages), not
// part of the shipped operator binary — which is why it lives in its own
// command rather than as a `nexus` subcommand, keeping go/packages and x/tools
// out of the distributed `nexus` binary.
//
// Usage:
//
//	go run ./cmd/openapi-gen \
//	  --src ../control-plane \
//	  --out ../../docs/users/api/openapi/control-plane \
//	  --version dev
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/openapigen"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run parses args, runs the generator, and writes a human summary. It returns
// the process exit code (0 ok, 1 generate error, 2 bad flags) and takes its
// streams as parameters so it is exercised by tests without a subprocess.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("openapi-gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	src := fs.String("src", "../control-plane", "control-plane package directory to analyse")
	out := fs.String("out", "../../docs/users/api/openapi/control-plane", "output directory for generated OpenAPI files")
	version := fs.String("version", "dev", "value stamped into info.version of every document")
	base := fs.String("base", "/api/admin", "base path prefix for the root route group")
	title := fs.String("title", "Nexus Control Plane Admin API", "info.title prefix")
	roots := fs.String("roots", "RegisterAdminRoutes,RegisterAssistantRoutes",
		"comma-separated registrar function names to start the route walk from. "+
			"RegisterAssistantRoutes is included by default because the assistant is "+
			"wired in cmd/control-plane/wiring, outside RegisterAdminRoutes.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var rootFuncs []string
	for _, r := range strings.Split(*roots, ",") {
		if r = strings.TrimSpace(r); r != "" {
			rootFuncs = append(rootFuncs, r)
		}
	}

	rep, err := openapigen.Generate(openapigen.Options{
		SrcDir:     *src,
		OutDir:     *out,
		Version:    *version,
		BasePrefix: *base,
		Title:      *title,
		RootFuncs:  rootFuncs,
	})
	if err != nil {
		fmt.Fprintf(stderr, "openapi-gen: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "openapi-gen: %d routes across %d kinds -> %s\n", rep.Routes, len(rep.Kinds), *out)
	for _, k := range rep.Kinds {
		fmt.Fprintf(stdout, "  - %s\n", k)
	}
	if len(rep.Unresolved) > 0 {
		fmt.Fprintf(stdout, "\n%d unresolved (need openapi-review attention):\n", len(rep.Unresolved))
		for _, u := range rep.Unresolved {
			fmt.Fprintf(stdout, "  ! %s\n", u)
		}
	}
	return 0
}
