// Package iam is a stand-in for the shared IAM catalog used by the fixture,
// reproducing the ResourceDef.Action(Verb) expression shape the generator
// renders into the x-nexus-iam-action extension and the verb identifiers it
// keys tier derivation on.
package iam

// Verb is an action verb.
type Verb string

const (
	VerbRead   Verb = "read"
	VerbList   Verb = "list"
	VerbCreate Verb = "create"
	VerbUpdate Verb = "update"
	VerbDelete Verb = "delete"
)

// ResourceDef names a protected resource.
type ResourceDef struct{ name string }

// Action renders the canonical "<resource>.<verb>" action string.
func (r ResourceDef) Action(v Verb) string { return r.name + "." + string(v) }

// ResourceWidget is the fixture's protected resource.
var ResourceWidget = ResourceDef{name: "widget"}
