package queue

import (
	"testing"
	"time"

	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/task"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/gofrs/uuid"
	"github.com/stretchr/testify/assert"
)

const testKnownType = "test_known_type"
const testUnknownType = "definitely_not_a_real_type"

func init() {
	// Register a dummy factory for the known-type tests.
	RegisterResumableTaskFactory(testKnownType, func(model *ent.Task) Task {
		return &DBTask{Task: model}
	})
}

func newTestModel(typ string) *ent.Task {
	return &ent.Task{
		ID:        42,
		Type:      typ,
		Status:    task.StatusQueued,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		PublicState: &types.TaskPublicState{
			Error:        "some error",
			ErrorHistory: []string{"err1", "err2"},
			RetryCount:   3,
		},
		PrivateState:  `{"key":"value"}`,
		CorrelationID: uuid.Nil,
	}
}

func TestNewTaskFromModel_KnownType(t *testing.T) {
	a := assert.New(t)

	model := newTestModel(testKnownType)
	got, err := NewTaskFromModel(model)
	a.NoError(err)
	a.NotNil(got)
	a.Equal(testKnownType, got.Type())
}

func TestNewTaskFromModel_UnknownType(t *testing.T) {
	a := assert.New(t)

	model := newTestModel(testUnknownType)
	got, err := NewTaskFromModel(model)
	a.Error(err)
	a.Nil(got)
	a.Contains(err.Error(), "unknown Task type")
}

func TestNewTaskFromModelOrFallback_KnownType(t *testing.T) {
	a := assert.New(t)

	model := newTestModel(testKnownType)
	got := NewTaskFromModelOrFallback(model)
	a.NotNil(got)
	a.Equal(testKnownType, got.Type())
	a.Equal(42, got.ID())
}

func TestNewTaskFromModelOrFallback_UnknownType(t *testing.T) {
	a := assert.New(t)

	model := newTestModel(testUnknownType)
	got := NewTaskFromModelOrFallback(model)
	a.NotNil(got, "fallback must never return nil")

	// The returned task must satisfy the full Task interface.
	a.Equal(testUnknownType, got.Type())
	a.Equal(42, got.ID())
	a.Equal(task.StatusQueued, got.Status())
	a.Equal(`{"key":"value"}`, got.State())
	a.True(got.Persisted())
	a.Equal(3, got.Retried())
	a.EqualError(got.Error(), "some error")
	a.Len(got.ErrorHistory(), 2)

	// Summarize must return a non-nil value (base DBTask returns empty Summary).
	summary := got.Summarize(nil)
	a.NotNil(summary)
	a.Equal(0, summary.NodeID)
	a.Empty(summary.Phase)

	// Do should return an error for the fallback (not resumable).
	_, err := got.Do(nil)
	a.Error(err)
}

func TestNewTaskFromModelOrFallback_MultipleUnknownTypes(t *testing.T) {
	// Simulates the scenario where the DB has several unknown types in a
	// batch — none of them should cause a nil task or a panic.
	a := assert.New(t)

	types := []string{"relocate", "future_type_v2", "legacy_migration"}
	for _, typ := range types {
		model := newTestModel(typ)
		got := NewTaskFromModelOrFallback(model)
		a.NotNil(got, "type %q must produce a non-nil fallback task", typ)
		a.Equal(typ, got.Type())
	}
}
