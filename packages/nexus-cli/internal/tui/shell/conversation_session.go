package shell

import (
	"fmt"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-cli/internal/tui/kit"
)

// conversation_session.go — the conversation's session lifecycle: resuming a
// past persisted session, tearing the built agent down for a rebuild, and the
// model switch that triggers one. Split from conversation.go along the
// session-management seam.

// resumeSession switches the conversation onto a past persisted session: cancel
// any in-flight turn, reset the agent (the next turn rebuilds bound to sess, so
// it appends to the SAME session id), and re-render the saved transcript —
// user/assistant text turns only; tool chips are not replayed. The context gauge
// resets to unknown until the next turn reports real usage.
func (c *conversation) resumeSession(sess *agent.Session) {
	if c.running {
		c.interrupted = true // suppress the cancelled turn's terminal noise
	}
	c.resetAgent()
	c.resume = sess
	c.lines = nil
	c.pending = nil
	c.streamFull = ""
	c.reveal = 0
	c.revealTag = ""
	c.scroll = 0
	c.ctxStats = agent.ContextStats{}
	c.appendLine("sys", fmt.Sprintf("⟲ resumed past conversation (%d saved messages) — it continues from here", len(sess.Messages)))
	for _, msg := range sess.Messages {
		text := strings.TrimSpace(msg.Text())
		if text == "" {
			continue // pure tool-result turns have no text to replay
		}
		switch msg.Role {
		case agent.RoleUser:
			c.appendLine("you", text)
		case agent.RoleAssistant:
			c.appendLine("asst", text)
		}
	}
	c.notice = "conversation resumed — your next message continues this session"
}

// resetAgent tears down the built agent so the next turn rebuilds it. Used by a
// model switch: the build seam reads the (now updated) model on the next turn.
func (c *conversation) resetAgent() {
	if c.bridge != nil {
		c.bridge.stop()
	}
	c.bridge = nil
	c.agentBuilt = false
	c.buildErr = nil
	c.running = false
}

// setModel switches the chat/agent model: cancel any in-flight turn, update the
// session, reset the agent (so the next turn rebuilds with the new model), and
// confirm in the transcript. Persisting the choice is the root's job (it owns the
// CLI seam); the conversation only changes what it builds against.
func (c *conversation) setModel(code string) {
	c.resetAgent()
	c.session.Model = code
	c.cf = kit.NewConfirm(c.session) // keep the confirm gate's env in sync
	c.appendLine("sys", "model → "+code)
	c.notice = ""
}
