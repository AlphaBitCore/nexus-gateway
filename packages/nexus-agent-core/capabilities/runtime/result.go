package runtime

import (
	"fmt"
	"github.com/goccy/go-json"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

// jsonResult renders v as indented JSON content for a tool_result.
func jsonResult(v any) (agent.Result, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return agent.Result{Content: "render error: " + err.Error(), IsError: true}, nil
	}
	return agent.Result{Content: string(b)}, nil
}

// errResult is a recoverable tool error the model can read and adapt to.
func errResult(format string, a ...any) agent.Result {
	return agent.Result{Content: fmt.Sprintf(format, a...), IsError: true}
}
