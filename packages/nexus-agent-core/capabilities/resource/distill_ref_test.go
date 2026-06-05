package resource

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// distill_ref_test.go covers same-document $ref resolution (design doc §3.2):
// before resolution, 37 of the catalog's 116 JSON request bodies — every body
// declared as a root $ref — distilled to an empty skeleton, so a search card
// or describe row for those writes shipped no field list at all.

// refFixture builds a minimal spec with components/schemas for table cases.
const refFixtureSpec = `
paths:
  /api/admin/widgets:
    post:
      operationId: createWidget
      summary: Create a widget
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/WidgetCreate'
  /api/admin/widgets/chain:
    post:
      operationId: chainWidget
      summary: Three-hop ref chain
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Hop1'
  /api/admin/widgets/cycle:
    post:
      operationId: cycleWidget
      summary: Cyclic ref must terminate
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/CycleA'
  /api/admin/widgets/deep:
    post:
      operationId: deepWidget
      summary: Chain deeper than the cap resolves to nothing
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/Deep1'
  /api/admin/widgets/refprop:
    post:
      operationId: refPropWidget
      summary: A property that is itself a ref resolves to its type
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [mode]
              properties:
                mode:
                  $ref: '#/components/schemas/Mode'
  /api/admin/widgets/badref:
    post:
      operationId: badRefWidget
      summary: Unknown ref distills empty, not panics
      requestBody:
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/DoesNotExist'
components:
  schemas:
    WidgetCreate:
      type: object
      required: [name]
      properties:
        name: {type: string}
        size: {type: integer, enum: [1, 2, 3]}
    Hop1: {$ref: '#/components/schemas/Hop2'}
    Hop2: {$ref: '#/components/schemas/Hop3'}
    Hop3:
      type: object
      required: [leaf]
      properties:
        leaf: {type: string}
    CycleA: {$ref: '#/components/schemas/CycleB'}
    CycleB: {$ref: '#/components/schemas/CycleA'}
    Deep1: {$ref: '#/components/schemas/Deep2'}
    Deep2: {$ref: '#/components/schemas/Deep3'}
    Deep3: {$ref: '#/components/schemas/Deep4'}
    Deep4:
      type: object
      properties:
        tooDeep: {type: string}
    Mode:
      type: string
      enum: [fast, slow]
`

func refFixtureKind() resourceKind {
	return resourceKind{Kind: "widgets", File: "widgets.yaml", Operations: []resourceOp{
		{Method: "POST", Path: "/api/admin/widgets", OperationID: "createWidget"},
		{Method: "POST", Path: "/api/admin/widgets/chain", OperationID: "chainWidget"},
		{Method: "POST", Path: "/api/admin/widgets/cycle", OperationID: "cycleWidget"},
		{Method: "POST", Path: "/api/admin/widgets/deep", OperationID: "deepWidget"},
		{Method: "POST", Path: "/api/admin/widgets/refprop", OperationID: "refPropWidget"},
		{Method: "POST", Path: "/api/admin/widgets/badref", OperationID: "badRefWidget"},
	}}
}

func TestDistillRefResolution(t *testing.T) {
	d, err := distillKind(refFixtureKind(), []byte(refFixtureSpec))
	if err != nil {
		t.Fatalf("distill fixture: %v", err)
	}
	byID := map[string]DistilledOp{}
	for _, op := range d.Operations {
		byID[op.OperationID] = op
	}

	// Root $ref resolves to the real field list with required + enum intact.
	cw := byID["createWidget"]
	if len(cw.Body) != 2 {
		t.Fatalf("createWidget body = %+v, want name+size", cw.Body)
	}
	if cw.Body[0].Name != "name" || !cw.Body[0].Required {
		t.Fatalf("required field must sort first and keep its required flag: %+v", cw.Body)
	}
	if cw.Body[1].Name != "size" || len(cw.Body[1].Enum) != 3 {
		t.Fatalf("size must keep its enum through the ref: %+v", cw.Body[1])
	}

	// A 3-hop chain (the corpus's real maximum) resolves — the cap is inclusive.
	if ch := byID["chainWidget"]; len(ch.Body) != 1 || ch.Body[0].Name != "leaf" || !ch.Body[0].Required {
		t.Fatalf("3-hop chain must resolve to the leaf schema: %+v", byID["chainWidget"].Body)
	}

	// A cycle terminates with an empty body — never hangs, never panics.
	if cy := byID["cycleWidget"]; len(cy.Body) != 0 {
		t.Fatalf("cyclic ref must distill empty: %+v", cy.Body)
	}

	// A chain deeper than the cap degrades to empty (pre-resolution behavior).
	if dp := byID["deepWidget"]; len(dp.Body) != 0 {
		t.Fatalf("4-hop chain exceeds the inclusive cap of 3 and must distill empty: %+v", dp.Body)
	}

	// A property that is itself a $ref resolves to its type + enum.
	rp := byID["refPropWidget"]
	if len(rp.Body) != 1 || rp.Body[0].Name != "mode" || rp.Body[0].Type != "string" || len(rp.Body[0].Enum) != 2 {
		t.Fatalf("ref-valued property must resolve type+enum: %+v", rp.Body)
	}
	if !rp.Body[0].Required {
		t.Fatal("required must come from the PARENT schema, not the ref target")
	}

	// An unknown ref degrades to empty.
	if br := byID["badRefWidget"]; len(br.Body) != 0 {
		t.Fatalf("unknown ref must distill empty: %+v", br.Body)
	}
}

// TestDistillRefBodiesNonEmpty is the corpus-wide guarantee behind the
// "directly executable card" claim: every catalog operation whose spec
// declares a JSON request body with a root $ref or inline properties MUST
// distill to a non-empty body skeleton. Before resolution, all 37 root-$ref
// bodies failed this. (Bodies that are legitimately free-form — no $ref, no
// properties — and non-JSON media are out of scope by construction.)
func TestDistillRefBodiesNonEmpty(t *testing.T) {
	type rawOp struct {
		RequestBody *struct {
			Content map[string]struct {
				Schema struct {
					Ref        string         `yaml:"$ref"`
					Properties map[string]any `yaml:"properties"`
				} `yaml:"schema"`
			} `yaml:"content"`
		} `yaml:"requestBody"`
	}
	refBodies := 0
	for _, rk := range resCatalog.Kinds {
		raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + rk.File)
		if err != nil {
			t.Fatalf("read %s: %v", rk.File, err)
		}
		var doc struct {
			Paths map[string]map[string]rawOp `yaml:"paths"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", rk.File, err)
		}
		d, err := distillKind(rk, raw)
		if err != nil {
			t.Fatalf("distill %s: %v", rk.Kind, err)
		}
		byID := map[string]DistilledOp{}
		for _, op := range d.Operations {
			byID[op.OperationID] = op
		}
		for _, op := range rk.Operations {
			spec, ok := doc.Paths[op.Path][strings.ToLower(op.Method)]
			if !ok || spec.RequestBody == nil {
				continue
			}
			media, ok := spec.RequestBody.Content["application/json"]
			if !ok {
				continue
			}
			isRef := media.Schema.Ref != ""
			if !isRef && len(media.Schema.Properties) == 0 {
				continue // legitimately free-form
			}
			if isRef {
				refBodies++
			}
			if len(byID[op.OperationID].Body) == 0 {
				t.Errorf("%s %s (%s): JSON body declared but distilled empty", rk.Kind, op.OperationID, media.Schema.Ref)
			}
		}
	}
	// Guard the measurement the design rests on: the corpus really does carry
	// root-$ref bodies (37 at design time; the count may only grow).
	if refBodies < 37 {
		t.Errorf("expected >=37 root-$ref JSON bodies in the corpus, found %d — measurement drift", refBodies)
	}
}
