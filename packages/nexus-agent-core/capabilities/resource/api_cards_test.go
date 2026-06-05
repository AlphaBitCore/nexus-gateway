package resource

import (
	"encoding/json"
	"strings"
	"testing"
)

// api_cards_test.go covers the two-segment SearchCards protocol (design doc
// §3.2): top-K full executable cards + thin tail, assembled from the
// init-time DistilledOp memo.

func TestSearchCardsTwoSegmentShape(t *testing.T) {
	res := SearchCards("virtual key", 5, 20)
	if len(res.Cards) == 0 || len(res.Cards) > 5 {
		t.Fatalf("want 1..5 cards, got %d", len(res.Cards))
	}
	if len(res.Cards)+len(res.More) > 20 {
		t.Fatalf("window overflow: %d cards + %d more", len(res.Cards), len(res.More))
	}
	// Cards carry the semantics the thin result hid: a real OpenAPI summary.
	top := res.Cards[0]
	if top.Summary == "" {
		t.Fatalf("top card %s has no summary — the blind-re-rank fix is the point", top.OperationID)
	}
	if top.Kind == "" || top.OperationID == "" || top.Method == "" || top.Path == "" {
		t.Fatalf("card missing structural identity: %+v", top)
	}
}

func TestSearchCardsWriteCardCarriesBodySkeleton(t *testing.T) {
	res := SearchCards("create virtual key", 5, 20)
	for _, c := range res.Cards {
		if c.OperationID != "createVirtualKey" {
			continue
		}
		if !c.Write {
			t.Fatal("createVirtualKey must be marked write")
		}
		if len(c.Body) == 0 {
			t.Fatal("write card must carry its body skeleton for one-step invoke")
		}
		return
	}
	t.Fatal("createVirtualKey not found in the card window for its own words")
}

// TestSearchCardsRefBodyCard pins the $ref integration end to end:
// setNodeOverride's body is a root $ref in nodes.yaml — before resolution its
// card would ship an empty skeleton.
func TestSearchCardsRefBodyCard(t *testing.T) {
	res := SearchCards("node override", 5, 20)
	for _, c := range res.Cards {
		if c.OperationID != "setNodeOverride" {
			continue
		}
		if len(c.Params) < 2 {
			t.Fatalf("setNodeOverride needs id+configKey params on the card, got %+v", c.Params)
		}
		if len(c.Body) == 0 {
			t.Fatal("setNodeOverride body must distill through its $ref")
		}
		return
	}
	t.Fatal("setNodeOverride not in card window for \"node override\"")
}

func TestSearchCardsDefaultsAndClamps(t *testing.T) {
	// cardK<=0 → default 5; cardK>8 → clamp 8; limit<cardK → lift to cardK.
	if res := SearchCards("list", 0, 20); len(res.Cards) > 5 {
		t.Fatalf("default card window must be 5, got %d", len(res.Cards))
	}
	if res := SearchCards("list", 99, 99); len(res.Cards) > 8 {
		t.Fatalf("card window must clamp at 8, got %d", len(res.Cards))
	}
	if res := SearchCards("list", 5, 1); len(res.Cards)+len(res.More) > 5 {
		t.Fatalf("limit below cardK must lift to cardK, got %d+%d", len(res.Cards), len(res.More))
	}
}

func TestSearchCardsNoMatchIsEmptyArrayNotNull(t *testing.T) {
	res := SearchCards("xqzvw", 5, 20)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"cards":[]`) {
		t.Fatalf("no-match must serialize cards as [], got %s", b)
	}
	if strings.Contains(string(b), `"more"`) {
		t.Fatalf("empty tail must be omitted, got %s", b)
	}
}

// TestSearchCardsThinTailStaysThin guards the token economics: tail entries
// must never grow card fields.
func TestSearchCardsThinTailStaysThin(t *testing.T) {
	res := SearchCards("list", 2, 20) // a broad query so the tail is populated
	if len(res.More) == 0 {
		t.Skip("query produced no tail; widen the query")
	}
	b, err := json.Marshal(res.More[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"summary", "params", "body", "write", "label"} {
		if strings.Contains(string(b), `"`+banned+`"`) {
			t.Fatalf("thin tail leaked %q: %s", banned, b)
		}
	}
}

// TestSearchCardsUsesMemoNotReparse asserts card assembly is wired to the
// init-time memo: a card's summary must equal the memoized DistilledOp's
// summary for the same operation (no divergent re-distillation path).
func TestSearchCardsUsesMemoNotReparse(t *testing.T) {
	res := SearchCards("routing rules", 5, 20)
	if len(res.Cards) == 0 {
		t.Fatal("no cards for routing rules")
	}
	c := res.Cards[0]
	dop, ok := distilledIdx[[2]string{c.Kind, c.OperationID}]
	if !ok {
		t.Fatalf("card op %s/%s missing from distilledIdx", c.Kind, c.OperationID)
	}
	if c.Summary != dop.Summary {
		t.Fatalf("card summary %q diverges from memo %q", c.Summary, dop.Summary)
	}
}
