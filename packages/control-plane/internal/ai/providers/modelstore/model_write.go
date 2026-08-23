package modelstore

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// The write half of the model store. Reads (the row shape, the column list,
// the scanners, and the list/get queries) stay in model.go; what a caller may
// set, what the store fills in for them, and the two statements that persist
// it live here. The normalizer in particular is the piece two different
// create paths used to each have their own copy of.

// CreateModelParams holds fields for creating a model. The DB
// primary key (id) is auto-generated UUID — callers don't supply it.
type CreateModelParams struct {
	Code                            string // customer-facing identifier; unique
	Name                            string
	Description                     *string
	ProviderID                      string
	ProviderModelID                 string
	Type                            string
	Features                        []string
	InputPricePerMillion            *float64
	OutputPricePerMillion           *float64
	CachedInputReadPricePerMillion  *float64
	CachedInputWritePricePerMillion *float64
	// Audio-token rates for realtime models; nil for every other type.
	AudioInputPricePerMillion           *float64
	AudioOutputPricePerMillion          *float64
	CachedAudioInputReadPricePerMillion *float64

	MaxContextTokens *int
	MaxOutputTokens  *int
	Aliases          []string
	InputModalities  []string // nil = derived from Type + Features
	// RequiredModalities: nil/empty = no constraint, which is the normal case.
	// Never derived — a model's floor cannot be inferred from its type.
	RequiredModalities []string
	OutputModalities   []string         // nil = derived from Type + Features
	Lifecycle          string           // defaults to "ga" when empty
	CapabilityJson     *json.RawMessage // nil = no capability data
	Enabled            bool
}

// CreateModel inserts a new model. The id column defaults to
// gen_random_uuid() server-side; callers receive the resolved row.
// NormalizeCreateParams fills the defaults and folds the legacy vision feature.
//
// Exported because a model is created down two paths — this package's
// CreateModel and providerstore's bulk provider-with-models insert — and they
// used to normalize separately. The bulk path defaulted every type to
// ["text"]/["text"], so a wizard-created stt model landed declaring it accepts
// text, and it never folded `vision` at all. Two normalizers for one write is
// the same shape of bug as two vocabularies for one fact.
func NormalizeCreateParams(p CreateModelParams) CreateModelParams {
	if p.Features == nil {
		p.Features = []string{}
	}
	if p.Aliases == nil {
		p.Aliases = []string{}
	}
	if p.InputModalities == nil || p.OutputModalities == nil {
		in, out := defaultModalities(p.Type)
		if p.InputModalities == nil {
			p.InputModalities = in
		}
		if p.OutputModalities == nil {
			p.OutputModalities = out
		}
	}
	// After the defaults, so a caller who sent `vision` and no modalities still
	// lands with image on the derived array rather than folding into a nil.
	p.Features, p.InputModalities = FoldVision(p.Features, p.InputModalities)
	if p.Lifecycle == "" {
		p.Lifecycle = "ga"
	}
	// requiredModalities is the one array column on Model that is NOT NULL, and
	// an explicit NULL beats a column DEFAULT — so leaving this nil sends NULL
	// and the insert fails the constraint. A caller who omits the field means
	// "no floor", which is the empty array, not the absence of a value. Adding
	// a model with no required modality is the ordinary case, so this fires on
	// nearly every create.
	if p.RequiredModalities == nil {
		p.RequiredModalities = []string{}
	}
	return p
}

func (store *Store) CreateModel(ctx context.Context, p CreateModelParams) (*Model, error) {
	p = NormalizeCreateParams(p)
	q := fmt.Sprintf(`
		INSERT INTO "Model" (id, code, name, description, "providerId", "providerModelId", type, features,
			"inputPricePerMillion", "outputPricePerMillion",
			"cachedInputReadPricePerMillion", "cachedInputWritePricePerMillion",
			"audioInputPricePerMillion", "audioOutputPricePerMillion", "cachedAudioInputReadPricePerMillion",
			"maxContextTokens", "maxOutputTokens",
			aliases, "inputModalities", "outputModalities", "requiredModalities", lifecycle, "capabilityJson",
			enabled, "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, NOW(), NOW())
		RETURNING %s
	`, ModelColumns)
	m, err := scanModel(store.pool.QueryRow(ctx, q,
		uuid.New().String(), p.Code, p.Name, p.Description, p.ProviderID, p.ProviderModelID, p.Type, p.Features,
		p.InputPricePerMillion, p.OutputPricePerMillion,
		p.CachedInputReadPricePerMillion, p.CachedInputWritePricePerMillion,
		p.AudioInputPricePerMillion, p.AudioOutputPricePerMillion, p.CachedAudioInputReadPricePerMillion,
		p.MaxContextTokens, p.MaxOutputTokens,
		p.Aliases, p.InputModalities, p.OutputModalities, p.RequiredModalities, p.Lifecycle, p.CapabilityJson,
		p.Enabled,
	))
	if err != nil {
		return nil, fmt.Errorf("create model: %w", err)
	}
	return m, nil
}

// UpdateModelParams holds optional fields for updating a model. nil = no change.
type UpdateModelParams struct {
	Code                            *string
	ProviderModelID                 *string
	Name                            *string
	Description                     *string
	Type                            *string
	InputPricePerMillion            *float64
	OutputPricePerMillion           *float64
	CachedInputReadPricePerMillion  *float64
	CachedInputWritePricePerMillion *float64
	// Audio-token rates for realtime models. nil = no change.
	AudioInputPricePerMillion           *float64
	AudioOutputPricePerMillion          *float64
	CachedAudioInputReadPricePerMillion *float64

	MaxContextTokens   *int
	MaxOutputTokens    *int
	Status             *string
	DeprecationDate    *time.Time
	ReplacedBy         *string
	Aliases            []string // nil = no change; empty = clear
	Enabled            *bool
	Features           []string         // nil = no change; empty = clear
	InputModalities    *[]string        // nil = no change
	RequiredModalities *[]string        // nil = no change
	OutputModalities   *[]string        // nil = no change
	Lifecycle          *string          // nil = no change
	CapabilityJson     *json.RawMessage // nil = no change; explicit null = clear
}

// UpdateModel updates a model using COALESCE — nil params preserve existing values.
// Arrays (Aliases, Features, InputModalities, OutputModalities) use CASE WHEN to conditionally update.
func (store *Store) UpdateModel(ctx context.Context, id string, p UpdateModelParams) (*Model, error) {
	// For arrays: pgx passes nil Go slice as SQL NULL, so we use
	// CASE WHEN $N IS NULL THEN col ELSE $N END to preserve the original.
	// For *[]string capability modalities: nil pointer → NULL → preserve existing.
	var inputMod, outputMod interface{}
	if p.InputModalities != nil {
		inputMod = *p.InputModalities
	}
	var outputModVal interface{}
	if p.OutputModalities != nil {
		outputModVal = *p.OutputModalities
	}
	outputMod = outputModVal
	var requiredMod interface{}
	if p.RequiredModalities != nil {
		requiredMod = *p.RequiredModalities
	}
	q := fmt.Sprintf(`UPDATE "Model" SET
		code = COALESCE($2, code),
		"providerModelId" = COALESCE($3, "providerModelId"),
		name = COALESCE($4, name),
		description = COALESCE($5, description),
		type = COALESCE($6, type),
		"inputPricePerMillion" = COALESCE($7, "inputPricePerMillion"),
		"outputPricePerMillion" = COALESCE($8, "outputPricePerMillion"),
		"cachedInputReadPricePerMillion" = COALESCE($9, "cachedInputReadPricePerMillion"),
		"cachedInputWritePricePerMillion" = COALESCE($10, "cachedInputWritePricePerMillion"),
		"audioInputPricePerMillion" = COALESCE($11, "audioInputPricePerMillion"),
		"audioOutputPricePerMillion" = COALESCE($12, "audioOutputPricePerMillion"),
		"cachedAudioInputReadPricePerMillion" = COALESCE($13, "cachedAudioInputReadPricePerMillion"),
		"maxContextTokens" = COALESCE($14, "maxContextTokens"),
		"maxOutputTokens" = COALESCE($15, "maxOutputTokens"),
		status = COALESCE($16, status),
		enabled = COALESCE($17, enabled),
		aliases = CASE WHEN $18::text[] IS NULL THEN aliases ELSE $18 END,
		features = CASE WHEN $19::text[] IS NULL THEN features ELSE $19 END,
		"deprecationDate" = COALESCE($20, "deprecationDate"),
		"replacedBy" = COALESCE($21, "replacedBy"),
		"inputModalities" = CASE WHEN $22::text[] IS NULL THEN "inputModalities" ELSE $22 END,
		"outputModalities" = CASE WHEN $23::text[] IS NULL THEN "outputModalities" ELSE $23 END,
		lifecycle = COALESCE($24, lifecycle),
		"capabilityJson" = CASE WHEN $25::jsonb IS NULL THEN "capabilityJson" ELSE $25 END,
		"requiredModalities" = CASE WHEN $26::text[] IS NULL THEN "requiredModalities" ELSE $26 END,
		"updatedAt" = NOW()
	WHERE id = $1 RETURNING %s`, ModelColumns)

	m, err := scanModel(store.pool.QueryRow(ctx, q,
		id, p.Code, p.ProviderModelID,
		p.Name, p.Description, p.Type,
		p.InputPricePerMillion, p.OutputPricePerMillion,
		p.CachedInputReadPricePerMillion, p.CachedInputWritePricePerMillion,
		p.AudioInputPricePerMillion, p.AudioOutputPricePerMillion, p.CachedAudioInputReadPricePerMillion,
		p.MaxContextTokens, p.MaxOutputTokens,
		p.Status, p.Enabled,
		p.Aliases, p.Features,
		p.DeprecationDate, p.ReplacedBy,
		inputMod, outputMod,
		p.Lifecycle, p.CapabilityJson,
		requiredMod,
	))
	if err != nil {
		return nil, fmt.Errorf("update model: %w", err)
	}
	return m, nil
}

// ProviderWithModels groups a provider summary with its models.
