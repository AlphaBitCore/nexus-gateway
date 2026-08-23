package modelstore

import (
	"slices"
	"testing"
)

// Each case names the row that a flat ["text"]/["text"] default described
// wrongly. The assertion is the property the catalog must hold — a model never
// declares modalities that contradict the endpoint or the features on its own
// row — not merely that the function returns something.
func TestDefaultModalities_DerivesFromTheRowRatherThanAssumingText(t *testing.T) {
	cases := []struct {
		name      string
		modelType string
		features  []string
		wantIn    []string
		wantOut   []string
	}{
		{
			name:      "stt takes audio and emits text",
			modelType: "stt",
			wantIn:    []string{"audio"},
			wantOut:   []string{"text"},
		},
		{
			name:      "tts takes text and emits audio",
			modelType: "tts",
			wantIn:    []string{"text"},
			wantOut:   []string{"audio"},
		},
		{
			name:      "an image model emits images, not text",
			modelType: "image",
			wantIn:    []string{"text"},
			wantOut:   []string{"image"},
		},
		{
			name:      "a video model emits video",
			modelType: "video",
			wantIn:    []string{"text"},
			wantOut:   []string{"video"},
		},
		{
			name:      "an embedding model emits vectors",
			modelType: "embedding",
			wantIn:    []string{"text"},
			wantOut:   []string{"embedding"},
		},
		{
			name:      "realtime is bidirectional audio",
			modelType: "realtime",
			wantIn:    []string{"text", "audio"},
			wantOut:   []string{"text", "audio"},
		},
		{
			name:      "plain chat is text in, text out",
			modelType: "chat",
			wantIn:    []string{"text"},
			wantOut:   []string{"text"},
		},
		{
			name:      "rerank is text in, text out",
			modelType: "rerank",
			wantIn:    []string{"text"},
			wantOut:   []string{"text"},
		},
		{
			// The row that reached production: features said vision, the
			// modality default said text-only, and the two could not both be
			// true. command-a-vision-07-2025.
			name:      "a vision chat model accepts images as well as text",
			modelType: "chat",
			features:  []string{"streaming", "function_calling", "vision"},
			wantIn:    []string{"text", "image"},
			wantOut:   []string{"text"},
		},
		{
			// Extending, not replacing. A repair that replaced the array once
			// left 94 rows declaring image-only input, which would refuse text
			// on every vision model — the inverse failure.
			name:      "vision on an unknown type still keeps text input",
			modelType: "something-new",
			features:  []string{"vision"},
			wantIn:    []string{"text", "image"},
			wantOut:   []string{"text"},
		},
		{
			name:      "an unknown type falls back to text without inventing modalities",
			modelType: "something-new",
			wantIn:    []string{"text"},
			wantOut:   []string{"text"},
		},
		{
			// vision on an stt row is contradictory input, but the audio input
			// the endpoint requires must survive it.
			name:      "vision never displaces the modality the endpoint requires",
			modelType: "stt",
			features:  []string{"vision"},
			wantIn:    []string{"audio", "image"},
			wantOut:   []string{"text"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The composition CreateModel actually runs: derive from the type,
			// then fold the caller's legacy vision feature into the array that
			// owns the fact. Testing the halves separately would leave the
			// order between them — which is what makes vision survive a nil
			// modality list — unasserted.
			in, out := defaultModalities(tc.modelType)
			feats, in := FoldVision(tc.features, in)
			if slices.Contains(feats, "vision") {
				t.Errorf("features = %v, still carries vision after folding", feats)
			}
			if !slices.Equal(in, tc.wantIn) {
				t.Errorf("input modalities = %v, want %v", in, tc.wantIn)
			}
			if !slices.Equal(out, tc.wantOut) {
				t.Errorf("output modalities = %v, want %v", out, tc.wantOut)
			}
		})
	}
}

// The catalog-wide invariant, asserted directly: whatever the default produces,
// a row that advertises vision must accept image input. This is the check that
// would have caught command-a-vision-07-2025 at creation time.
func TestDefaultModalities_VisionAlwaysImpliesImageInput(t *testing.T) {
	for _, mt := range []string{"chat", "stt", "tts", "image", "video", "embedding", "realtime", "rerank", ""} {
		in, _ := defaultModalities(mt)
		_, in = FoldVision([]string{"vision"}, in)
		if !slices.Contains(in, "image") {
			t.Errorf("type %q with vision produced input %v, which does not accept images", mt, in)
		}
	}
}

// A model that declares vision twice must not accumulate duplicate entries —
// the arrays are served verbatim on GET /v1/models.
func TestDefaultModalities_RepeatedVisionFeatureDoesNotDuplicateImage(t *testing.T) {
	in, _ := defaultModalities("chat")
	_, in = FoldVision([]string{"vision", "vision"}, in)
	if !slices.Equal(in, []string{"text", "image"}) {
		t.Errorf("input modalities = %v, want [text image]", in)
	}
}

// An explicit image modality plus the vision feature must not double up either.
func TestDefaultModalities_DoesNotRepeatAModalityTheTypeAlreadyImplies(t *testing.T) {
	// image type already yields text input; adding vision appends image once.
	in, out := defaultModalities("image")
	_, in = FoldVision([]string{"vision"}, in)
	if !slices.Equal(in, []string{"text", "image"}) {
		t.Errorf("input modalities = %v, want [text image]", in)
	}
	if !slices.Equal(out, []string{"image"}) {
		t.Errorf("output modalities = %v, want [image]", out)
	}
}

func TestContains(t *testing.T) {
	if !contains([]string{"text", "image"}, "image") {
		t.Error("contains reported a present value as absent")
	}
	if contains([]string{"text"}, "image") {
		t.Error("contains reported an absent value as present")
	}
	if contains(nil, "text") {
		t.Error("contains found a value in a nil slice")
	}
}

// The point of the fold: `vision` stops being stored. A row that keeps it
// alongside inputModalities ∋ image is one fact in two vocabularies, and the
// two drifted apart in production — command-a-vision-07-2025 reached the
// catalog advertising vision with inputModalities ["text"].
func TestFoldVision_RemovesTheFeatureAndKeepsTheRest(t *testing.T) {
	feats, in := FoldVision([]string{"function_calling", "vision", "streaming"}, []string{"text"})
	if !slices.Equal(feats, []string{"function_calling", "streaming"}) {
		t.Errorf("features = %v, want the non-modality capabilities only", feats)
	}
	if !slices.Equal(in, []string{"text", "image"}) {
		t.Errorf("input modalities = %v, want [text image]", in)
	}
}

// A caller that never mentioned vision must come out untouched — the fold is
// a translation for legacy callers, not a rewrite of everyone's row.
func TestFoldVision_LeavesAVisionlessRowAlone(t *testing.T) {
	feats, in := FoldVision([]string{"streaming"}, []string{"text"})
	if !slices.Equal(feats, []string{"streaming"}) || !slices.Equal(in, []string{"text"}) {
		t.Errorf("features = %v, input = %v; want both unchanged", feats, in)
	}
}

// The fold must not write through the caller's slice. UpdateModel passes the
// EXISTING row's modalities as the base and then compares lengths to decide
// whether anything changed; aliasing would make that comparison read the
// mutated array and report no change.
func TestFoldVision_DoesNotMutateTheCallersArray(t *testing.T) {
	base := make([]string, 1, 4) // spare capacity: append would write in place
	base[0] = "text"
	_, in := FoldVision([]string{"vision"}, base)
	if len(base) != 1 || base[0] != "text" {
		t.Errorf("caller's array was mutated to %v", base)
	}
	if !slices.Equal(in, []string{"text", "image"}) {
		t.Errorf("input modalities = %v, want [text image]", in)
	}
}

// The Model row's requiredModalities column is NOT NULL with a DEFAULT of the
// empty array, and an explicit NULL beats a column default — so a nil here
// reaches Postgres as NULL and the insert dies on the constraint. It fired on
// production the first time a model was added whose catalog entry declared no
// required modality, which is the ordinary case: adding Kimi K3 through the
// admin API answered 500 "null value in column requiredModalities violates
// not-null constraint".
//
// The other four array columns on Model are nullable, so this is the only one
// with the hazard, and NormalizeCreateParams is the one place that closes it.
func TestNormalizeCreateParams_RequiredModalitiesNeverReachesTheInsertAsNil(t *testing.T) {
	got := NormalizeCreateParams(CreateModelParams{Type: "chat"})
	if got.RequiredModalities == nil {
		t.Fatal("RequiredModalities is nil — it will be sent as SQL NULL into a NOT NULL column")
	}
	if len(got.RequiredModalities) != 0 {
		t.Errorf("RequiredModalities = %v, want empty — an omitted floor means no constraint, not a fabricated one", got.RequiredModalities)
	}
}

func TestNormalizeCreateParams_RequiredModalitiesPreservesACallersFloor(t *testing.T) {
	got := NormalizeCreateParams(CreateModelParams{Type: "chat", RequiredModalities: []string{"audio"}})
	if !slices.Equal(got.RequiredModalities, []string{"audio"}) {
		t.Errorf("RequiredModalities = %v, want [audio] — the default must not overwrite a stated floor", got.RequiredModalities)
	}
}

// Every other array field the insert writes must also be non-nil, for the same
// reason one level weaker: they are nullable today, and a row that stores NULL
// where the reader expects a list makes the empty case ambiguous.
func TestNormalizeCreateParams_NoArrayFieldReachesTheInsertAsNil(t *testing.T) {
	got := NormalizeCreateParams(CreateModelParams{Type: "chat"})
	for name, v := range map[string][]string{
		"Features":           got.Features,
		"Aliases":            got.Aliases,
		"InputModalities":    got.InputModalities,
		"OutputModalities":   got.OutputModalities,
		"RequiredModalities": got.RequiredModalities,
	} {
		if v == nil {
			t.Errorf("%s is nil after normalization", name)
		}
	}
}
