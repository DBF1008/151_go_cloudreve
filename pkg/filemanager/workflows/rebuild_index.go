package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/task"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs/dbfs"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/manager"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/logging"
	"github.com/cloudreve/Cloudreve/v4/pkg/queue"
	"github.com/cloudreve/Cloudreve/v4/pkg/searcher"
)

type (
	RebuildIndexTask struct {
		*queue.DBTask

		l        logging.Logger
		state    *RebuildIndexTaskState
		progress queue.Progresses
	}
	RebuildIndexTaskPhase string
	RebuildIndexTaskState struct {
		Phase                 RebuildIndexTaskPhase `json:"phase"`
		Total                 int                   `json:"total"`
		Indexed               int                   `json:"indexed"`
		LastFileID            int                   `json:"last_file_id"`
		Failed                int                   `json:"failed"`
		Skipped               int                   `json:"skipped"`
		Excluded              int                   `json:"excluded"`
		FilteredStoragePolicy []int                 `json:"filtered_storage_policy"`
	}

	// indexResult represents the outcome of indexing a single file.
	indexResult int
)

const (
	indexResultIndexed indexResult = iota // Successfully indexed (with or without text).
	indexResultSkipped                    // Degraded / intentionally skipped (e.g. extraction failed, no entity).
	indexResultFailed                     // Hard failure (e.g. indexer call failed).
	indexResultExcluded                   // Excluded by storage policy filter.
)

const (
	RebuildIndexPhaseNuke  RebuildIndexTaskPhase = "nuke"
	RebuildIndexPhaseIndex RebuildIndexTaskPhase = "index"

	RebuildIndexBatchSize  = 1000
	RebuildIndexConcurrent = 4

	ProgressTypeRebuildIndex = "rebuild_index"
)

func init() {
	queue.RegisterResumableTaskFactory(queue.FullTextRebuildTaskType, NewRebuildIndexTaskFromModel)
}

func NewRebuildIndexTask(ctx context.Context, u *ent.User, filteredStoragePolicy []int) (queue.Task, error) {
	state := &RebuildIndexTaskState{
		Phase:                 RebuildIndexPhaseNuke,
		FilteredStoragePolicy: filteredStoragePolicy,
	}
	stateBytes, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal state: %w", err)
	}

	return &RebuildIndexTask{
		DBTask: &queue.DBTask{
			Task: &ent.Task{
				Type:          queue.FullTextRebuildTaskType,
				CorrelationID: logging.CorrelationID(ctx),
				PrivateState:  string(stateBytes),
				PublicState:   &types.TaskPublicState{},
			},
			DirectOwner: u,
		},
	}, nil
}

func NewRebuildIndexTaskFromModel(t *ent.Task) queue.Task {
	return &RebuildIndexTask{
		DBTask: &queue.DBTask{
			Task: t,
		},
	}
}

func (m *RebuildIndexTask) Do(ctx context.Context) (task.Status, error) {
	dep := dependency.FromContext(ctx)
	m.l = dep.Logger()

	m.Lock()
	if m.progress == nil {
		m.progress = make(queue.Progresses)
	}
	m.progress[ProgressTypeRebuildIndex] = &queue.Progress{}
	m.Unlock()

	state := &RebuildIndexTaskState{}
	if err := json.Unmarshal([]byte(m.State()), state); err != nil {
		return task.StatusError, fmt.Errorf("failed to unmarshal state: %s (%w)", err, queue.CriticalErr)
	}
	m.state = state

	var (
		next = task.StatusCompleted
		err  error
	)
	switch m.state.Phase {
	case RebuildIndexPhaseNuke, "":
		next, err = m.nuke(ctx, dep)
	case RebuildIndexPhaseIndex:
		next, err = m.index(ctx, dep)
	default:
		next, err = task.StatusError, fmt.Errorf("unknown phase %q: %w", m.state.Phase, queue.CriticalErr)
	}

	newStateStr, marshalErr := json.Marshal(m.state)
	if marshalErr != nil {
		return task.StatusError, fmt.Errorf("failed to marshal state: %w", marshalErr)
	}

	m.Lock()
	m.Task.PrivateState = string(newStateStr)
	m.Unlock()
	return next, err
}

// nuke deletes all existing index documents and ensures a fresh index exists,
// then counts total indexable files for progress tracking.
func (m *RebuildIndexTask) nuke(ctx context.Context, dep dependency.Dep) (task.Status, error) {
	indexer := dep.SearchIndexer(ctx)

	m.l.Info("Deleting all existing index documents...")
	if err := indexer.DeleteAll(ctx); err != nil {
		return task.StatusError, fmt.Errorf("failed to delete all index documents: %w", err)
	}

	if err := dep.FileClient().DeleteAllMetadataByName(ctx, dbfs.FullTextIndexKey); err != nil {
		return task.StatusError, fmt.Errorf("failed to delete all metadata by name: %w", err)
	}

	m.l.Info("Ensuring index exists with correct configuration...")
	if err := indexer.EnsureIndex(ctx); err != nil {
		return task.StatusError, fmt.Errorf("failed to ensure index: %w", err)
	}

	// Count total indexable files
	total, err := dep.FileClient().CountIndexableFiles(ctx)
	if err != nil {
		return task.StatusError, fmt.Errorf("failed to count indexable files: %w", err)
	}

	m.state.Total = total
	m.state.Phase = RebuildIndexPhaseIndex
	m.state.LastFileID = 0
	m.state.Indexed = 0

	m.l.Info("Found %d indexable files, starting rebuild...", total)
	m.ResumeAfter(0)
	return task.StatusSuspending, nil
}

// index processes a batch of files and suspends for the next batch.
func (m *RebuildIndexTask) index(ctx context.Context, dep dependency.Dep) (task.Status, error) {
	atomic.StoreInt64(&m.progress[ProgressTypeRebuildIndex].Total, int64(m.state.Total))
	atomic.StoreInt64(&m.progress[ProgressTypeRebuildIndex].Current, int64(m.state.Indexed))

	files, err := dep.FileClient().ListIndexableFiles(ctx, m.state.LastFileID, RebuildIndexBatchSize)
	if err != nil {
		return task.StatusError, fmt.Errorf("failed to list indexable files after ID %d: %w", m.state.LastFileID, err)
	}

	if len(files) == 0 {
		m.l.Info("Rebuild complete. %d files indexed, %d skipped, %d failed, %d excluded by storage policy filter.",
			m.state.Indexed-m.state.Failed-m.state.Skipped, m.state.Skipped, m.state.Failed,
			m.state.Total-m.state.Indexed)
		return task.StatusCompleted, nil
	}

	batchFailed, batchSkipped, batchExcluded := m.processBatch(ctx, dep, files)
	m.state.Failed += batchFailed
	m.state.Skipped += batchSkipped
	m.state.Excluded += batchExcluded
	m.state.Indexed += len(files)
	m.state.LastFileID = files[len(files)-1].ID

	atomic.StoreInt64(&m.progress[ProgressTypeRebuildIndex].Current, int64(m.state.Indexed))

	// Suspend and resume for next batch
	m.ResumeAfter(0)
	return task.StatusSuspending, nil
}

// processBatch indexes a batch of files concurrently, returning the number of
// hard failures and the number of skipped (degraded) files.
func (m *RebuildIndexTask) processBatch(ctx context.Context, dep dependency.Dep, files []*ent.File) (failed, skipped int) {
	user := inventory.UserFromContext(ctx)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)

	sem := make(chan struct{}, RebuildIndexConcurrent)
	indexer := dep.SearchIndexer(ctx)
	extractor := dep.TextExtractor(ctx)

	for _, f := range files {
		select {
		case <-ctx.Done():
			return failed, skipped
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(f *ent.File) {
			defer func() {
				<-sem
				wg.Done()
			}()

			result := m.indexSingleFile(ctx, dep, user, indexer, extractor, f)
			mu.Lock()
			switch result {
			case indexResultFailed:
				failed++
			case indexResultSkipped:
				skipped++
			case indexResultExcluded:
				// Excluded by storage policy filter; counted in Indexed for progress.
			}
			mu.Unlock()
		}(f)
	}

	wg.Wait()
	return failed, skipped
}

// indexSingleFile indexes a single file. It always attempts to create at least
// a filename-only index entry so that the file is searchable by name, even when
// text extraction is not possible or fails.
//
// Return values:
//   - indexResultIndexed:  file indexed successfully (with or without text).
//   - indexResultSkipped:  degraded scenario — file was indexed by name only
//     because extraction was not possible (no entity, unsupported type, etc.).
//   - indexResultFailed:   hard failure — indexer call itself failed.
//   - indexResultExcluded: excluded by the storage policy filter; not indexed.
func (m *RebuildIndexTask) indexSingleFile(
	ctx context.Context,
	dep dependency.Dep,
	user *ent.User,
	indexer searcher.SearchIndexer,
	extractor searcher.TextExtractor,
	f *ent.File,
) indexResult {
	entityID := f.PrimaryEntity

	// 1. Early storage policy filter check using the file's own policy ID.
	//    This avoids an expensive GetEntitySource call for files that will be
	//    excluded anyway.
	if len(m.state.FilteredStoragePolicy) > 0 {
		if !slices.Contains(m.state.FilteredStoragePolicy, f.StoragePolicyFiles) {
			m.l.Debug("File %d storage policy %d not in filtered policies, excluding.", f.ID, f.StoragePolicyFiles)
			return indexResultExcluded
		}
	}

	// 2. No primary entity — index with file name only (degraded).
	if entityID == 0 {
		m.l.Debug("No primary entity for file %d, indexing file name only.", f.ID)
		if err := indexer.IndexFile(ctx, f.OwnerID, f.ID, 0, f.Name, ""); err != nil {
			m.l.Warning("Failed to index file name for file %d: %s", f.ID, err)
			return indexResultFailed
		}
		return indexResultSkipped
	}

	// 3. Determine whether text extraction should be attempted.
	var text string
	if manager.ShouldExtractText(extractor, f.Name, f.Size) {
		fm := manager.NewFileManager(dep, user)
		defer fm.Recycle()

		source, err := fm.GetEntitySource(ctx, entityID)
		if err != nil {
			// Cannot get source — fall back to file-name-only index (degraded).
			m.l.Warning("Cannot get entity source for file %d: %s, indexing file name only.", f.ID, err)
		} else {
			defer source.Close()

			extracted, err := extractor.Extract(ctx, source)
			if err != nil {
				// Extraction failed — fall back to file-name-only index (degraded).
				m.l.Debug("Failed to extract text for file %d: %s, indexing file name only.", f.ID, err)
			} else {
				text = extracted
			}
		}
	}

	// 4. Index the file (with text if available, or file-name-only).
	if err := indexer.IndexFile(ctx, f.OwnerID, f.ID, entityID, f.Name, text); err != nil {
		m.l.Warning("Failed to index file %d (%s): %s", f.ID, f.Name, err)
		return indexResultFailed
	}

	// 5. Mark as indexed via metadata so incremental flows can detect it.
	if err := dep.FileClient().UpsertMetadata(ctx, f, map[string]string{
		dbfs.FullTextIndexKey: hashid.EncodeEntityID(dep.HashIDEncoder(), entityID),
	}, nil); err != nil {
		m.l.Warning("Failed to upsert metadata for file %d: %s", f.ID, err)
	}

	if text == "" {
		return indexResultSkipped
	}
	return indexResultIndexed
}

func (m *RebuildIndexTask) Progress(ctx context.Context) queue.Progresses {
	m.Lock()
	defer m.Unlock()
	return m.progress
}

func (m *RebuildIndexTask) Summarize(hasher hashid.Encoder) *queue.Summary {
	if m.state == nil {
		if err := json.Unmarshal([]byte(m.State()), &m.state); err != nil {
			return nil
		}
	}

	return &queue.Summary{
		Phase: string(m.state.Phase),
		Props: map[string]any{
			SummaryKeyFailed:  m.state.Failed,
			SummaryKeyTotal:   m.state.Total,
			SummaryKeySkipped: m.state.Skipped,
		},
	}
}
