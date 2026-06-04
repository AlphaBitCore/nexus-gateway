package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Skill is a progressive-disclosure playbook. Only Name + Description enter the
// system prompt (the catalog); Body is injected only when use_skill invokes it,
// and AllowedTools narrows the exposed toolset while the skill is active.
type Skill struct {
	Name         string
	Description  string
	Body         string
	AllowedTools []string
}

// SkillSet holds skills by name with stable ordering for the catalog.
type SkillSet struct {
	skills map[string]Skill
	order  []string
}

// NewSkillSet builds a skill set with the given skills, sorted by name.
func NewSkillSet(skills ...Skill) *SkillSet {
	s := &SkillSet{skills: map[string]Skill{}}
	for _, sk := range skills {
		if _, ok := s.skills[sk.Name]; !ok {
			s.order = append(s.order, sk.Name)
		}
		s.skills[sk.Name] = sk
	}
	sort.Strings(s.order)
	return s
}

// Get returns the full skill (incl. Body) by name.
func (s *SkillSet) Get(name string) (Skill, bool) { sk, ok := s.skills[name]; return sk, ok }

// Names returns skill names sorted.
func (s *SkillSet) Names() []string { return append([]string(nil), s.order...) }

// Catalog renders the name + one-line description list for the system prompt.
// It deliberately omits bodies (progressive disclosure).
func (s *SkillSet) Catalog() string {
	if len(s.order) == 0 {
		return "(no skills available)"
	}
	var b strings.Builder
	for _, n := range s.order {
		sk := s.skills[n]
		fmt.Fprintf(&b, "- %s: %s\n", sk.Name, sk.Description)
	}
	return strings.TrimRight(b.String(), "\n")
}

// activateFunc is called by use_skill when a skill is invoked, so the loop can
// narrow the exposed tools to the skill's allow-list for subsequent rounds.
type activateFunc func(name string, allowedTools []string)

// useSkillTool is the kernel-builtin tool that injects a skill body on demand.
// It is auto-tier (reading a playbook has no side effect). The loop also watches
// for its activation via the activateFunc seam.
type useSkillTool struct {
	set      *SkillSet
	activate activateFunc
}

func newUseSkillTool(set *SkillSet, activate activateFunc) *useSkillTool {
	return &useSkillTool{set: set, activate: activate}
}

func (u *useSkillTool) Name() string { return "use_skill" }

func (u *useSkillTool) Description() string {
	return "Load a skill playbook by name to follow a proven procedure. Use when the catalog lists a skill matching the task."
}

func (u *useSkillTool) Tier() Tier { return TierAuto }

func (u *useSkillTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"name":{"type":"string","description":"the skill name from the catalog"}},"required":["name"]}`)
}

func (u *useSkillTool) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &v); err != nil {
		return Result{Content: "invalid use_skill input: " + err.Error(), IsError: true}, nil
	}
	sk, ok := u.set.Get(v.Name)
	if !ok {
		return Result{Content: fmt.Sprintf("no skill named %q; available: %s", v.Name, strings.Join(u.set.Names(), ", ")), IsError: true}, nil
	}
	if u.activate != nil {
		u.activate(sk.Name, sk.AllowedTools)
	}
	return Result{Content: fmt.Sprintf("Skill %q loaded. Follow this playbook:\n\n%s", sk.Name, sk.Body)}, nil
}
