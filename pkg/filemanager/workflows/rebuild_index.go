package workflows

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
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
		Phase RebuildIndexTaskPhase `json:"phase"`
		Total int                   `json:"total"`
		// Indexed counts files indexed with extracted body content.
		Indexed int `json:"indexed"`
		// FilenameOnly counts files indexed with their file name only, because no body
		// content could be extracted (unsupported type, extraction failure, unreadable
		// source, or no primary entity). These are degraded successes, not failures.
		FilenameOnly int `json:"filename_only"`
		// Skipped counts files left out of scope by the storage policy filter.
		Skipped int `json:"skipped"`
		// Failed counts true indexing failures (the indexer rejected the document).
		Failed                int   `json:"failed"`
		LastFileID            int   `json:"last_file_id"`
		FilteredStoragePolicy []int `json:"filtered_storage_policy"`
	}
)

const (
	RebuildIndexPhaseNuke  RebuildIndexTaskPhase = "nuke"
	RebuildIndexPhaseIndex RebuildIndexTaskPhase = "index"

	RebuildIndexBatchSize  = 1000
	RebuildIndexConcurrent = 4

	ProgressTypeRebuildIndex = "rebuild_index"
)

const (
	summaryKeyIndexed      = "indexed"
	summaryKeyFilenameOnly = "filename_only"
	summaryKeySkipped      = "skipped"
)

// rebuildDecision is the action chosen for a file during a rebuild.
type rebuildDecision int

const (
	decisionSkip     rebuildDecision = iota // out of the storage policy filter scope
	decisionExtract                         // attempt body-text extraction, then index
	decisionNameOnly                        // index the file name only (no body content)
)

// rebuildOutcome is the result of attempting to index a single file.
type rebuildOutcome int

const (
	outcomeIndexed  rebuildOutcome = iota // indexed with extracted body content
	outcomeNameOnly                       // indexed with file name only (degraded)
	outcomeSkipped                        // skipped, out of storage policy scope
)

// batchStats accumulates per-file outcomes for a single batch.
type batchStats struct {
	indexed      int
	filenameOnly int
	skipped      int
	failed       int
}

// decideRebuildAction decides what to do with a file given whether it has a primary entity,
// that entity's storage policy ID, whether the file is eligible for text extraction, and the
// active storage policy filter (empty means no filtering).
//
// When a filter is active, only files whose primary entity belongs to one of the selected
// policies are processed; everything else (including files without a primary entity, which
// belong to no policy) is skipped, so out-of-scope files are never indexed.
func decideRebuildAction(hasEntity bool, policyID int, textExtractable bool, filter []int) rebuildDecision {
	if len(filter) > 0 && (!hasEntity || !slices.Contains(filter, policyID)) {
		return decisionSkip
	}
	if hasEntity && textExtractable {
		return decisionExtract
	}
	return decisionNameOnly
}

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
	m.state.FilenameOnly = 0
	m.state.Skipped = 0
	m.state.Failed = 0

	m.l.Info("Found %d indexable files, starting rebuild...", total)
	m.ResumeAfter(0)
	return task.StatusSuspending, nil
}

// index processes a batch of files and suspends for the next batch.
func (m *RebuildIndexTask) index(ctx context.Context, dep dependency.Dep) (task.Status, error) {
	atomic.StoreInt64(&m.progress[ProgressTypeRebuildIndex].Total, int64(m.state.Total))
	atomic.StoreInt64(&m.progress[ProgressTypeRebuildIndex].Current, int64(m.processed()))

	files, err := dep.FileClient().ListIndexableFiles(ctx, m.state.LastFileID, RebuildIndexBatchSize)
	if err != nil {
		return task.StatusError, fmt.Errorf("failed to list indexable files after ID %d: %w", m.state.LastFileID, err)
	}

	if len(files) == 0 {
		m.l.Info("Rebuild complete. %d indexed with content, %d file name only, %d skipped (out of storage policy), %d failed.",
			m.state.Indexed, m.state.FilenameOnly, m.state.Skipped, m.state.Failed)
		return task.StatusCompleted, nil
	}

	stats := m.processBatch(ctx, dep, files)
	m.state.Indexed += stats.indexed
	m.state.FilenameOnly += stats.filenameOnly
	m.state.Skipped += stats.skipped
	m.state.Failed += stats.failed
	m.state.LastFileID = files[len(files)-1].ID

	atomic.StoreInt64(&m.progress[ProgressTypeRebuildIndex].Current, int64(m.processed()))

	// Suspend and resume for next batch
	m.ResumeAfter(0)
	return task.StatusSuspending, nil
}

// processed returns the number of files handled so far (indexed, degraded, skipped, or
// failed). It is used as the progress current value and reaches Total once every file has
// been visited, even when many files are skipped by the storage policy filter.
func (m *RebuildIndexTask) processed() int {
	return m.state.Indexed + m.state.FilenameOnly + m.state.Skipped + m.state.Failed
}

// processBatch indexes a batch of files concurrently.
func (m *RebuildIndexTask) processBatch(ctx context.Context, dep dependency.Dep, files []*ent.File) batchStats {
	user := inventory.UserFromContext(ctx)

	// Resolve the storage policy of every primary entity up front in a single query, so the
	// storage policy filter can be applied to every file (including files we never open for
	// text extraction). This avoids opening storage sources just to read a policy ID.
	entityIDs := make([]int, 0, len(files))
	for _, f := range files {
		if f.PrimaryEntity != 0 {
			entityIDs = append(entityIDs, f.PrimaryEntity)
		}
	}
	policyByEntity, err := dep.FileClient().GetEntityPolicyIDs(ctx, entityIDs)
	if err != nil {
		// Without policy information the filter cannot be honored safely. Count the whole
		// batch as failed rather than risk indexing files outside the selected policies.
		m.l.Warning("Failed to load entity storage policies for batch: %s", err)
		return batchStats{failed: len(files)}
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		stats batchStats
	)

	sem := make(chan struct{}, RebuildIndexConcurrent)
	indexer := dep.SearchIndexer(ctx)
	extractor := dep.TextExtractor(ctx)

	for _, f := range files {
		select {
		case <-ctx.Done():
			return stats
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(f *ent.File) {
			defer func() {
				<-sem
				wg.Done()
			}()

			outcome, err := m.indexSingleFile(ctx, dep, user, indexer, extractor, f, policyByEntity[f.PrimaryEntity])

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				m.l.Warning("Failed to index file %d (%s): %s", f.ID, f.Name, err)
				stats.failed++
				return
			}
			switch outcome {
			case outcomeIndexed:
				stats.indexed++
			case outcomeNameOnly:
				stats.filenameOnly++
			case outcomeSkipped:
				stats.skipped++
			}
		}(f)
	}

	wg.Wait()
	return stats
}

// indexSingleFile indexes a single file. It always keeps at least a file-name index for
// in-scope files: when body text cannot be extracted (unsupported type, extraction failure,
// unreadable source) or the file has no primary entity, it degrades to a file-name-only
// index instead of dropping the file from the index. Files outside the active storage policy
// filter are skipped. policyID is the storage policy ID of the file's primary entity (0 when
// the file has no primary entity).
func (m *RebuildIndexTask) indexSingleFile(
	ctx context.Context,
	dep dependency.Dep,
	user *ent.User,
	indexer searcher.SearchIndexer,
	extractor searcher.TextExtractor,
	f *ent.File,
	policyID int,
) (rebuildOutcome, error) {
	entityID := f.PrimaryEntity
	hasEntity := entityID != 0
	extractable := hasEntity && manager.ShouldExtractText(extractor, f.Name, f.Size)

	decision := decideRebuildAction(hasEntity, policyID, extractable, m.state.FilteredStoragePolicy)
	if decision == decisionSkip {
		m.l.Debug("File %d is outside the storage policy filter scope, skipping.", f.ID)
		return outcomeSkipped, nil
	}

	// Best-effort body-text extraction. Any failure degrades to a file-name-only index so the
	// file stays searchable by name instead of disappearing from the index entirely.
	var text string
	if decision == decisionExtract {
		fm := manager.NewFileManager(dep, user)
		defer fm.Recycle()

		source, err := fm.GetEntitySource(ctx, entityID)
		if err != nil {
			m.l.Warning("Cannot get entity source for file %d: %s; indexing file name only.", f.ID, err)
		} else {
			defer source.Close()
			if extracted, err := extractor.Extract(ctx, source); err != nil {
				m.l.Warning("Failed to extract text for file %d: %s; indexing file name only.", f.ID, err)
			} else {
				text = extracted
			}
		}
	}

	if err := indexer.IndexFile(ctx, f.OwnerID, f.ID, entityID, f.Name, text); err != nil {
		return outcomeSkipped, fmt.Errorf("failed to index file %d: %w", f.ID, err)
	}

	// Record which entity has been indexed so later incremental updates can detect changes
	// and delete stale chunks. Files without a primary entity carry no such marker.
	if hasEntity {
		if err := dep.FileClient().UpsertMetadata(ctx, f, map[string]string{
			dbfs.FullTextIndexKey: hashid.EncodeEntityID(dep.HashIDEncoder(), entityID),
		}, nil); err != nil {
			m.l.Warning("Failed to upsert metadata for file %d: %s", f.ID, err)
		}
	}

	if strings.TrimSpace(text) == "" {
		return outcomeNameOnly, nil
	}
	return outcomeIndexed, nil
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
			SummaryKeyTotal:        m.state.Total,
			summaryKeyIndexed:      m.state.Indexed,
			summaryKeyFilenameOnly: m.state.FilenameOnly,
			summaryKeySkipped:      m.state.Skipped,
			SummaryKeyFailed:       m.state.Failed,
		},
	}
}
