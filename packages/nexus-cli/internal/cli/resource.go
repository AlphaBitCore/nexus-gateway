package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/capabilities/resource"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/restable"
)

// newResourceCmd builds `nexus resource …`, the CLI half of the generic,
// OpenAPI-driven admin surface the TUI cascade and the agent's resource_* tools
// share. It is operation-driven and search-first: discover an operation
// (search/describe), then read (GET) or invoke (write) it by operationId. Every
// documented Control Plane admin endpoint is reachable here — the same engine,
// same row/column rendering (internal/restable) as the TUI.
func newResourceCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resource",
		Short: "Browse and operate any Control Plane admin resource (OpenAPI-driven)",
		Long: "Browse and operate any Control Plane admin resource.\n\n" +
			"The catalog is generated from the Control Plane's OpenAPI specs, so every\n" +
			"documented admin endpoint is reachable — list/get/create/update/delete, plus\n" +
			"reports, singleton config, RPCs, and nested sub-resources at any depth.\n\n" +
			"Work search-first:\n" +
			"  nexus resource search \"node override\"        # find the operationId\n" +
			"  nexus resource describe nodes                 # its params + body schema\n" +
			"  nexus resource read nodes getNode --param id=<id>\n" +
			"  nexus resource invoke nodes setNodeOverride --param id=<id> --param configKey=<k> --body '{...}'",
	}
	cmd.AddCommand(
		newResourceKindsCmd(a),
		newResourceSearchCmd(a),
		newResourceDescribeCmd(a),
		newResourceReadCmd(a),
		newResourceInvokeCmd(a),
	)
	return cmd
}

func newResourceKindsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "kinds",
		Short:       "List every admin resource kind and how many operations it exposes",
		Example:     "  nexus resource kinds\n  nexus resource kinds -o json",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{"skipLoad": "true"}, // catalog is embedded; no env/auth needed
		RunE: func(cmd *cobra.Command, _ []string) error {
			kinds := resource.Kinds()
			if a.isJSON() {
				return a.renderJSON(kinds)
			}
			cells := make([][]string, 0, len(kinds))
			for _, k := range kinds {
				cells = append(cells, []string{k.Kind, fmt.Sprintf("%d", k.OpCount), strings.Join(k.Verbs, " ")})
			}
			return a.table([]string{"KIND", "OPS", "CANONICAL VERBS"}, cells)
		},
	}
}

func newResourceSearchCmd(a *App) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:         "search <query>",
		Short:       "Find admin operations matching a query (ranked, code-level match)",
		Long:        "Find candidate operations by a free-text query matched against kind, operationId, path, and label. Returns a short ranked list — use the operationId with `resource read` / `resource invoke`.",
		Example:     "  nexus resource search \"cache stats\"\n  nexus resource search node --limit 5 -o json",
		Args:        cobra.MinimumNArgs(1),
		Annotations: map[string]string{"skipLoad": "true"}, // catalog is embedded; no env/auth needed
		RunE: func(cmd *cobra.Command, args []string) error {
			ops := resource.SearchInfos(strings.Join(args, " "), limit)
			if a.isJSON() {
				return a.renderJSON(ops)
			}
			if len(ops) == 0 {
				a.printf("no operations match %q\n", strings.Join(args, " "))
				return nil
			}
			cells := make([][]string, 0, len(ops))
			for _, op := range ops {
				cells = append(cells, []string{op.Kind, op.OperationID, op.Method, op.Label, op.Path})
			}
			return a.table([]string{"KIND", "OPERATION", "METHOD", "LABEL", "PATH"}, cells)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum candidates to return")
	return cmd
}

func newResourceDescribeCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:         "describe <kind>",
		Short:       "Show every operation of a kind with its params and body fields",
		Example:     "  nexus resource describe virtual-keys\n  nexus resource describe nodes -o json",
		Args:        cobra.ExactArgs(1),
		Annotations: map[string]string{"skipLoad": "true"}, // catalog is embedded; no env/auth needed
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := args[0]
			ops := resource.Operations(kind)
			if ops == nil {
				return fmt.Errorf("%w: unknown kind %q — run `nexus resource kinds`", errUsage, kind)
			}
			descs := make([]resource.OperationSchema, 0, len(ops))
			for _, op := range ops {
				if s, ok := resource.DescribeOperation(kind, op.OperationID); ok {
					descs = append(descs, s)
				}
			}
			if a.isJSON() {
				return a.renderJSON(map[string]any{"kind": kind, "operations": descs})
			}
			cells := make([][]string, 0, len(ops))
			for _, op := range ops {
				params := strings.Join(op.Params, ",")
				body := ""
				if s, ok := resource.DescribeOperation(kind, op.OperationID); ok {
					body = fieldNames(s.Body)
					if q := queryNames(s.Params); q != "" {
						if params != "" {
							params += " "
						}
						params += "?" + q
					}
				}
				cells = append(cells, []string{op.OperationID, op.Method, op.Label, dashEmpty(params), dashEmpty(body)})
			}
			return a.table([]string{"OPERATION", "METHOD", "LABEL", "PARAMS", "BODY"}, cells)
		},
	}
}

func newResourceReadCmd(a *App) *cobra.Command {
	var params, query map[string]string
	cmd := &cobra.Command{
		Use:     "read <kind> <operationId>",
		Short:   "Execute a read (GET) operation by operationId",
		Long:    "Execute a read (GET) operation. Fill path placeholders with --param name=value and add filters/paging with --query name=value (see `resource describe`).",
		Example: "  nexus resource read cache cacheStats\n  nexus resource read jobs listJobRuns --param id=<jobId>\n  nexus resource read virtual-keys listVirtualKeys --query limit=20",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, opID := args[0], args[1]
			method, path, mutating, err := resource.ResolveOperation(kind, opID, params)
			if err != nil {
				return fmt.Errorf("%w: %v", errUsage, err)
			}
			if mutating {
				return fmt.Errorf("%w: %s is a %s write — use `nexus resource invoke`", errUsage, opID, method)
			}
			raw, _, err := a.client().AdminRequest(cmd.Context(), method, path, toValues(query), nil)
			if err != nil {
				return err
			}
			return a.renderResourceBody(raw)
		},
	}
	cmd.Flags().StringToStringVar(&params, "param", nil, "path placeholder name=value (repeatable)")
	cmd.Flags().StringToStringVar(&query, "query", nil, "query parameter name=value (repeatable)")
	return cmd
}

func newResourceInvokeCmd(a *App) *cobra.Command {
	var params, query map[string]string
	var body, bodyFile string
	var yes bool
	cmd := &cobra.Command{
		Use:     "invoke <kind> <operationId>",
		Short:   "Execute a write (POST/PUT/PATCH/DELETE) operation by operationId",
		Long:    "Execute a write operation. Fill path placeholders with --param name=value and supply the payload with --body '<json>' or --body-file <path> (see `resource describe`). Writes require confirmation: pass --yes, or confirm at the prompt on a terminal.",
		Example: "  nexus resource invoke virtual-keys createVirtualKey --body '{\"name\":\"ci\"}' --yes\n  nexus resource invoke nodes setNodeOverride --param id=<id> --param configKey=<k> --body '{\"value\":true}'",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind, opID := args[0], args[1]
			method, path, mutating, err := resource.ResolveOperation(kind, opID, params)
			if err != nil {
				return fmt.Errorf("%w: %v", errUsage, err)
			}
			if !mutating {
				return fmt.Errorf("%w: %s is a GET read — use `nexus resource read`", errUsage, opID)
			}
			payload, err := resolveBody(body, bodyFile, cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("%w: %v", errUsage, err)
			}
			if !yes {
				ok, err := a.confirmWrite(cmd, method, path)
				if err != nil {
					return err
				}
				if !ok {
					a.printf("aborted.\n")
					return nil
				}
			}
			raw, _, err := a.client().AdminRequest(cmd.Context(), method, path, toValues(query), payload)
			if err != nil {
				return err
			}
			return a.renderResourceBody(raw)
		},
	}
	cmd.Flags().StringToStringVar(&params, "param", nil, "path placeholder name=value (repeatable)")
	cmd.Flags().StringToStringVar(&query, "query", nil, "query parameter name=value (repeatable)")
	cmd.Flags().StringVar(&body, "body", "", "request body as a JSON string")
	cmd.Flags().StringVar(&bodyFile, "body-file", "", "read the request body JSON from a file (- for stdin)")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the confirmation prompt (required for non-interactive writes)")
	return cmd
}

// renderResourceBody prints an admin response. With -o json it emits the body
// verbatim; otherwise a list body renders as a table and any other body as an
// indented record — the SAME data-shape logic the TUI uses (internal/restable).
func (a *App) renderResourceBody(raw json.RawMessage) error {
	if a.isJSON() {
		return a.renderJSON(raw)
	}
	rows, ok := restable.ExtractRows(raw)
	if !ok {
		return a.renderJSON(raw) // a single record / scalar — indented JSON
	}
	if len(rows) == 0 {
		a.printf("(empty)\n")
		return nil
	}
	cols := restable.InferColumns(rows, 6)
	header := make([]string, len(cols))
	for i, c := range cols {
		header[i] = strings.ToUpper(c)
	}
	cells := make([][]string, 0, len(rows))
	for _, r := range rows {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = restable.CellString(r[c])
		}
		cells = append(cells, row)
	}
	if err := a.table(header, cells); err != nil {
		return err
	}
	a.printf("(%d rows)\n", len(rows))
	return nil
}

// confirmWrite asks the operator to authorize a write by prompting y/N on the
// command's input (default no). When no input is available — a pipe with nothing
// to read, a non-interactive run — it refuses with a usage error so a scripted
// write is never silently applied without --yes.
func (a *App) confirmWrite(cmd *cobra.Command, method, path string) (bool, error) {
	prefix := ""
	if a.Env.IsProd {
		prefix = fmt.Sprintf("PRODUCTION (%s) — ", a.Env.Name)
	}
	a.printf("%sApply %s %s? [y/N] ", prefix, method, path)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if line == "" && err != nil {
		return false, fmt.Errorf("%w: refusing a write without confirmation — pass --yes", errUsage)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// resolveBody reads the request payload from --body or --body-file (mutually
// exclusive; "-" reads stdin), validating it is JSON so a malformed body fails
// before the call.
func resolveBody(body, bodyFile string, stdin io.Reader) (json.RawMessage, error) {
	if body != "" && bodyFile != "" {
		return nil, fmt.Errorf("use --body or --body-file, not both")
	}
	var raw []byte
	switch {
	case body != "":
		raw = []byte(body)
	case bodyFile == "-":
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		raw = b
	case bodyFile != "":
		b, err := os.ReadFile(bodyFile)
		if err != nil {
			return nil, err
		}
		raw = b
	default:
		return nil, nil // no body
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("request body is not valid JSON")
	}
	return json.RawMessage(raw), nil
}

// toValues converts a name=value map into url.Values (nil for an empty map).
func toValues(m map[string]string) url.Values {
	if len(m) == 0 {
		return nil
	}
	v := url.Values{}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic encoding
	for _, k := range keys {
		v.Set(k, m[k])
	}
	return v
}

// fieldNames joins required-marked field names for a compact describe column.
func fieldNames(fs []resource.FieldInfo) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		n := f.Name
		if f.Required {
			n += "*"
		}
		parts = append(parts, n)
	}
	return strings.Join(parts, ",")
}

// queryNames joins the in=query parameter names of an operation.
func queryNames(fs []resource.FieldInfo) string {
	parts := make([]string, 0, len(fs))
	for _, f := range fs {
		if f.In == "query" {
			parts = append(parts, f.Name)
		}
	}
	return strings.Join(parts, ",")
}

func dashEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
