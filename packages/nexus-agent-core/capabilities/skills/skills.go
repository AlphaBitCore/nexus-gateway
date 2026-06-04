// Package skills loads the agent's skill playbooks: the embedded built-ins plus
// any operator-authored markdown skills in a local directory, and the
// download→review→install flow for fetching a skill over HTTP. It owns only skill
// loading; the ~/.config/nexus/{skills,memory,sessions} path helpers live in the
// parent capabilities package (paths.go) because the cli wires the session/memory
// dirs too, not just skills.
package skills

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-agent-core/agent"
)

//go:embed skills_builtin/*.md
var builtinSkillFS embed.FS

// parseSkill parses a markdown skill file with a YAML-ish frontmatter block:
//
//	---
//	name: <name>
//	description: <one line>
//	allowed-tools: a, b, c   (optional)
//	---
//	<body...>
//
// Only these three keys are recognized (a minimal parser, no YAML dep). A file
// without frontmatter or without a name is rejected.
func parseSkill(data []byte) (agent.Skill, error) {
	s := string(data)
	if !strings.HasPrefix(s, "---") {
		return agent.Skill{}, fmt.Errorf("skill file has no frontmatter")
	}
	rest := strings.TrimPrefix(s, "---")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return agent.Skill{}, fmt.Errorf("skill frontmatter is not terminated")
	}
	front := rest[:end]
	body := rest[end+len("\n---"):]
	sk := agent.Skill{Body: strings.TrimLeft(body, "\n")}
	for _, line := range strings.Split(front, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "name":
			sk.Name = val
		case "description":
			sk.Description = val
		case "allowed-tools":
			for _, t := range strings.Split(val, ",") {
				if t = strings.TrimSpace(t); t != "" {
					sk.AllowedTools = append(sk.AllowedTools, t)
				}
			}
		}
	}
	if sk.Name == "" {
		return agent.Skill{}, fmt.Errorf("skill is missing a name")
	}
	return sk, nil
}

// Load builds the SkillSet: the embedded built-ins first, then every *.md in
// localDir (if non-empty), which may add or override by name. A malformed local
// file is skipped (not fatal); a malformed built-in is a programming error and
// fails loudly.
func Load(localDir string) (*agent.SkillSet, error) {
	var skills []agent.Skill

	entries, err := builtinSkillFS.ReadDir("skills_builtin")
	if err != nil {
		return nil, fmt.Errorf("read built-in skills: %w", err)
	}
	for _, e := range entries {
		b, err := builtinSkillFS.ReadFile("skills_builtin/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read built-in %s: %w", e.Name(), err)
		}
		sk, err := parseSkill(b)
		if err != nil {
			return nil, fmt.Errorf("parse built-in %s: %w", e.Name(), err)
		}
		skills = append(skills, sk)
	}

	if localDir != "" {
		locals, err := os.ReadDir(localDir)
		if err == nil { // a missing local dir is fine (no custom skills yet)
			for _, e := range locals {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				b, err := os.ReadFile(filepath.Join(localDir, e.Name()))
				if err != nil {
					continue
				}
				if sk, err := parseSkill(b); err == nil {
					skills = append(skills, sk)
				}
			}
		}
	}
	return agent.NewSkillSet(skills...), nil
}
