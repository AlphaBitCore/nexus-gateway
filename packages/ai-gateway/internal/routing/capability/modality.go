package capability

import (
	normcore "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/normalize/core"
)

// CarriedModalities reads the normalized payload, where every ingress shape has
// already been reduced to one block vocabulary.
//
// It reports FACTS about the request and applies no policy. Text counts when a
// text block is non-empty — never inferred from the presence of messages, since
// an image-only turn still has messages and treating that as text would satisfy
// a floor it does not meet.
//
// Reporting text matters because the alternative is a predicate that special-
// cases it. When this returned media modalities only, every floor check had to
// skip text to avoid refusing all traffic against a text-requiring model — and
// the proxy's own builder, which did count text, then disagreed with the router
// about the same request. One builder reporting everything it can see leaves
// the predicates with nothing to compensate for.
func CarriedModalities(p *normcore.NormalizedPayload) []string {
	if p == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(modality string) {
		if modality == "" || seen[modality] {
			return
		}
		seen[modality] = true
		out = append(out, modality)
	}
	for _, m := range p.Messages {
		for _, b := range m.Content {
			if b.Type == normcore.ContentText {
				if b.Text != "" {
					add("text")
				}
				continue
			}
			if b.MediaRef != nil {
				add(b.MediaRef.Modality)
			}
		}
	}
	return out
}

// Accepts applies the ceiling and the floor together.
func (s *Snapshot) Accepts(modelID string, carried []string) bool {
	if s == nil {
		return true
	}
	cap := s.Get(modelID)
	if cap == nil {
		return true
	}
	for _, want := range carried {
		if !DeclarationAcceptsModality(cap.InputModalities, want) {
			return false
		}
	}
	return s.MeetsFloor(modelID, carried)
}

// AcceptsModality is the ceiling for ONE modality, so a caller holding a
// candidate list can apply the pool-emptying escape per dimension.
func (s *Snapshot) AcceptsModality(modelID, modality string) bool {
	if s == nil {
		return true
	}
	cap := s.Get(modelID)
	if cap == nil {
		return true
	}
	return DeclarationAcceptsModality(cap.InputModalities, modality)
}

// DeclarationAcceptsModality is the one definition of the ceiling, shared by the
// smart strategy and the resolver; the two answering differently is how a
// capability-filtered model ended up serving an audio request.
//
// An empty list is UNDECLARED, not text-only — reading it as a restriction hid
// 36 document-capable models from routing.
func DeclarationAcceptsModality(inputModalities []string, modality string) bool {
	if modality == "text" || len(inputModalities) == 0 {
		return true
	}
	return contains(inputModalities, modality)
}

// MissingFloor lists the modalities a model requires that the request does not
// carry. Empty means the floor is met.
//
// No modality is special here, text included: CarriedModalities reports what
// the request holds, and a requirement is missing when it is not in that set.
// The one place the answer is decided — callers needing yes/no take
// DeclarationMeetsFloor, callers needing to name the gap take this. Two
// implementations were free to disagree about text, and did.
func MissingFloor(requiredModalities, carried []string) []string {
	var missing []string
	for _, need := range requiredModalities {
		if !contains(carried, need) {
			missing = append(missing, need)
		}
	}
	return missing
}

// DeclarationMeetsFloor treats an unrecognised requirement as unsatisfiable
// rather than absent.
func DeclarationMeetsFloor(requiredModalities, carried []string) bool {
	return len(MissingFloor(requiredModalities, carried)) == 0
}

// MeetsFloor is NOT conditional on the request carrying media: gpt-audio-mini
// requires audio and must be dropped from a plain-text request.
func (s *Snapshot) MeetsFloor(modelID string, carried []string) bool {
	if s == nil {
		return true
	}
	cap := s.Get(modelID)
	if cap == nil {
		return true
	}
	return DeclarationMeetsFloor(cap.RequiredModalities, carried)
}

// contains compares exactly. Both sides are already lower-case, by different
// means: catalog declarations are normalised when the snapshot is built, and
// carried modalities come from the canonical's own constants. Folding case here
// instead measured ~14% of the filter's cost.
//
// The asymmetry is worth knowing — an ingress that ever wrote a raw modality
// string into a MediaRef rather than using a constant would slip past this
// silently, and the failure would look like a capability gap rather than a
// casing bug.
func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
