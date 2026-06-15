package resource

import "testing"

// TestDistillExposesEveryOperation asserts distill no longer drops non-CRUD ops:
// every catalog operation for a kind appears, each with an operationId + label +
// method + path, and required body fields sort first.
func TestDistillExposesEveryOperation(t *testing.T) {
	for _, rk := range resCatalog.Kinds {
		raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + rk.File)
		if err != nil {
			t.Fatalf("%s: %v", rk.Kind, err)
		}
		d, err := distillKind(rk, raw)
		if err != nil {
			t.Fatalf("distill %s: %v", rk.Kind, err)
		}
		if len(d.Operations) != len(rk.Operations) {
			t.Fatalf("%s distilled %d ops but the catalog has %d — non-CRUD ops are being dropped", rk.Kind, len(d.Operations), len(rk.Operations))
		}
		for _, op := range d.Operations {
			if op.OperationID == "" || op.Label == "" || op.Method == "" || op.Path == "" {
				t.Fatalf("%s op missing operationId/label/method/path: %+v", rk.Kind, op)
			}
			seenOptional := false
			for _, f := range op.Body {
				if !f.Required {
					seenOptional = true
				} else if seenOptional {
					t.Fatalf("%s %s body must list required fields first: %+v", rk.Kind, op.Label, op.Body)
				}
			}
		}
	}
}

// TestSemanticCacheNotReadOnly is the regression for the old "(read-only)" mislabel:
// semantic-cache has write operations (PUT config, POST prewarm) and they must be
// present + flagged as writes, not hidden.
func TestSemanticCacheNotReadOnly(t *testing.T) {
	rk := resIdx["semantic-cache"]
	raw, _ := resourceSpecFS.ReadFile(resourceSpecDir + "/" + rk.File)
	d, _ := distillKind(rk, raw)
	var hasPut, hasPrewarm bool
	for _, op := range d.Operations {
		if op.OperationID == "putConfig" && op.Method == "PUT" {
			hasPut = true
		}
		if op.OperationID == "prewarmCache" && op.Method == "POST" {
			hasPrewarm = true
		}
	}
	if !hasPut || !hasPrewarm {
		t.Fatalf("semantic-cache must expose its write ops (putConfig, prewarmCache); got %+v", d.Operations)
	}
}

func TestDistillKindRoutingRulesBody(t *testing.T) {
	rk := resIdx["routing-rules"]
	raw, err := resourceSpecFS.ReadFile(resourceSpecDir + "/" + rk.File)
	if err != nil {
		t.Fatal(err)
	}
	d, err := distillKind(rk, raw)
	if err != nil {
		t.Fatal(err)
	}
	var create *DistilledOp
	for i := range d.Operations {
		if d.Operations[i].Verb == "create" {
			create = &d.Operations[i]
		}
	}
	if create == nil || len(create.Body) == 0 {
		t.Fatalf("routing-rules create op must distill a body, got %+v", d.Operations)
	}
	want := map[string]bool{"name": true, "strategyType": true, "config": true}
	for _, f := range create.Body {
		if want[f.Name] && !f.Required {
			t.Fatalf("routing-rules create field %q must be marked required: %+v", f.Name, create.Body)
		}
		delete(want, f.Name)
	}
	if len(want) != 0 {
		t.Fatalf("routing-rules create body missing fields: %v", want)
	}
}

// TestDistillKind_SummaryAndParamDesc asserts the distilled schema carries the
// operation summary and each parameter's description, so the model learns what an
// op does and what each filter means.
func TestDistillKind_SummaryAndParamDesc(t *testing.T) {
	rk := resourceKind{Kind: "x", Operations: []resourceOp{{Method: "GET", Path: "/api/admin/x", OperationID: "listX"}}}
	raw := []byte("paths:\n" +
		"  /api/admin/x:\n" +
		"    get:\n" +
		"      summary: List the X records\n" +
		"      parameters:\n" +
		"        - name: status\n" +
		"          in: query\n" +
		"          description: filter by record status\n" +
		"          schema:\n" +
		"            type: string\n")
	d, err := distillKind(rk, raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Operations) != 1 {
		t.Fatalf("want 1 op, got %d", len(d.Operations))
	}
	op := d.Operations[0]
	if op.Summary != "List the X records" {
		t.Fatalf("operation summary not carried: %q", op.Summary)
	}
	if len(op.Params) != 1 || op.Params[0].Desc != "filter by record status" {
		t.Fatalf("query-param description not carried: %+v", op.Params)
	}
}

// TestDistill exercises the public by-name wrapper: a known kind distills its full
// operation set; an unknown kind reports ok=false (the resource_describe tool's
// self-correction path).
func TestDistill(t *testing.T) {
	d, ok := Distill("virtual-keys")
	if !ok {
		t.Fatal("Distill(virtual-keys) must succeed")
	}
	if d.Kind != "virtual-keys" || len(d.Operations) == 0 {
		t.Fatalf("Distill(virtual-keys) returned an empty schema: %+v", d)
	}
	var sawList bool
	for _, op := range d.Operations {
		if op.OperationID == "listVirtualKeys" {
			sawList = true
		}
	}
	if !sawList {
		t.Fatalf("Distill(virtual-keys) must expose listVirtualKeys, got %+v", d.Operations)
	}
	if _, ok := Distill("no-such-kind"); ok {
		t.Fatal("Distill on an unknown kind must report ok=false")
	}
}

// The body distillation carries the nullability bit (3.1 unions AND 3.0
// nullable:true) so the model knows which request fields a contract allows null.
func TestDistillBodyNullability(t *testing.T) {
	spec := []byte(`
paths:
  /api/admin/traffic:
    post:
      operationId: createTrafficNote
      requestBody:
        content:
          application/json:
            schema:
              type: object
              required: [id]
              properties:
                id: { type: string }
                estimatedCostUsd: { type: [number, "null"] }
                legacyNote: { type: string, nullable: true }
`)
	d, err := distillKind(resourceKind{
		Kind: "traffic",
		Operations: []resourceOp{
			{Method: "POST", Path: "/api/admin/traffic", OperationID: "createTrafficNote", Tier: "write"},
		},
	}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Operations) != 1 {
		t.Fatalf("ops = %+v", d.Operations)
	}
	byName := map[string]DistilledField{}
	for _, f := range d.Operations[0].Body {
		byName[f.Name] = f
	}
	if f := byName["id"]; f.Nullable || !f.Required || f.Type != "string" {
		t.Fatalf("id = %+v", f)
	}
	if f := byName["estimatedCostUsd"]; !f.Nullable || f.Type != "number" {
		t.Fatalf("3.1 union nullability must survive distillation: %+v", f)
	}
	if f := byName["legacyNote"]; !f.Nullable || f.Type != "string" {
		t.Fatalf("3.0 nullable:true must survive distillation: %+v", f)
	}
}
