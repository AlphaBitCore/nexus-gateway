package capability

import (
	"sync"
	"testing"

	"github.com/AlphaBitCore/nexus-gateway/packages/ai-gateway/internal/platform/store"
)

func makeModel(id, lifecycle string, capJSON []byte) store.Model {
	return store.Model{
		ID:               id,
		Code:             id,
		InputModalities:  []string{"text"},
		OutputModalities: []string{"embedding"},
		Lifecycle:        lifecycle,
		CapabilityJson:   capJSON,
	}
}

var embeddingCapJSON = []byte(`{
	"embeddings": {
		"max_input_tokens": 8192,
		"supported_dimensions": [256, 512, 1024, 1536],
		"default_dimension": 1536,
		"max_batch_size": 100
	}
}`)

func TestNewSnapshot_Empty(t *testing.T) {
	snap := NewSnapshot(nil)
	if snap == nil {
		t.Fatal("NewSnapshot(nil) should return non-nil")
	}
	if snap.Get("any-id") != nil {
		t.Error("empty snapshot should return nil for any id")
	}
}

func TestNewSnapshot_SingleModel(t *testing.T) {
	models := []store.Model{
		makeModel("m1", "ga", embeddingCapJSON),
	}
	snap := NewSnapshot(models)
	cap := snap.Get("m1")
	if cap == nil {
		t.Fatal("Get(m1) returned nil")
	}
	if cap.Embeddings == nil {
		t.Fatal("Embeddings should be non-nil")
	}
	if cap.Embeddings.MaxBatchSize != 100 {
		t.Errorf("MaxBatchSize = %d, want 100", cap.Embeddings.MaxBatchSize)
	}
}

func TestNewSnapshot_MultipleModels(t *testing.T) {
	models := []store.Model{
		makeModel("m1", "ga", embeddingCapJSON),
		makeModel("m2", "preview", nil), // no capability data
		makeModel("m3", "ga", []byte(`{"embeddings":{"max_batch_size":50}}`)),
	}
	snap := NewSnapshot(models)

	if snap.Get("m1") == nil {
		t.Error("m1 should be present")
	}
	if snap.Get("m2") == nil {
		t.Error("m2 should be present (even with nil CapabilityJson)")
	}
	if snap.Get("m2").Embeddings != nil {
		t.Error("m2.Embeddings should be nil")
	}
	if snap.Get("m3") == nil {
		t.Error("m3 should be present")
	}
	if snap.Get("m3").Embeddings == nil {
		t.Error("m3.Embeddings should be non-nil")
	}
	if snap.Get("m3").Embeddings.MaxBatchSize != 50 {
		t.Errorf("m3 MaxBatchSize = %d, want 50", snap.Get("m3").Embeddings.MaxBatchSize)
	}
}

func TestSnapshot_GetMissing(t *testing.T) {
	snap := NewSnapshot([]store.Model{makeModel("m1", "ga", nil)})
	if snap.Get("nonexistent") != nil {
		t.Error("Get for nonexistent model should return nil")
	}
}

func TestSnapshot_NilReceiver(t *testing.T) {
	var s *Snapshot
	if s.Get("any") != nil {
		t.Error("nil Snapshot.Get should return nil")
	}
}

func TestCache_NewAndLoad(t *testing.T) {
	c := NewCache()
	s := c.Load()
	if s == nil {
		t.Fatal("Load on new cache should return non-nil snapshot")
	}
	// Empty snapshot — no models.
	if s.Get("anything") != nil {
		t.Error("fresh cache should have empty snapshot")
	}
}

func TestCache_Replace(t *testing.T) {
	c := NewCache()

	models := []store.Model{makeModel("m1", "ga", embeddingCapJSON)}
	snap := NewSnapshot(models)
	c.Replace(snap)

	s := c.Load()
	if s.Get("m1") == nil {
		t.Error("after Replace, m1 should be visible")
	}
}

func TestCache_ReplaceNil(t *testing.T) {
	c := NewCache()
	c.Replace(nil) // should not panic; loads an empty snapshot
	s := c.Load()
	if s == nil {
		t.Fatal("Load after Replace(nil) should return non-nil")
	}
}

// TestCache_AtomicSwap — multiple goroutines calling Load + Replace concurrently
// should not race (go test -race catches data races).
func TestCache_AtomicSwap(t *testing.T) {
	c := NewCache()
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				c.Replace(NewSnapshot([]store.Model{makeModel("m1", "ga", nil)}))
			} else {
				_ = c.Load()
			}
		}(i)
	}
	wg.Wait()
}
