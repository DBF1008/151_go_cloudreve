package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/task"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/inventory/types"
	"github.com/cloudreve/Cloudreve/v4/pkg/auth"
	"github.com/cloudreve/Cloudreve/v4/pkg/cluster"
	"github.com/cloudreve/Cloudreve/v4/pkg/downloader"
	"github.com/cloudreve/Cloudreve/v4/pkg/filemanager/fs"
	"github.com/cloudreve/Cloudreve/v4/pkg/logging"
	"github.com/cloudreve/Cloudreve/v4/pkg/queue"
	"github.com/cloudreve/Cloudreve/v4/pkg/request"
	"github.com/cloudreve/Cloudreve/v4/pkg/setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// ---------------------------------------------------------------------------
// Mock helpers
// ---------------------------------------------------------------------------

// mockNode implements cluster.Node for testing.
type mockNode struct {
	mock.Mock
}

func (n *mockNode) ID() int        { return n.Called().Int(0) }
func (n *mockNode) Name() string   { return n.Called().String(0) }
func (n *mockNode) IsMaster() bool { return n.Called().Bool(0) }
func (n *mockNode) CreateTask(ctx context.Context, taskType string, state string) (int, error) {
	args := n.Called(ctx, taskType, state)
	return args.Int(0), args.Error(1)
}
func (n *mockNode) GetTask(ctx context.Context, id int, clearOnComplete bool) (*cluster.SlaveTaskSummary, error) {
	args := n.Called(ctx, id, clearOnComplete)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*cluster.SlaveTaskSummary), args.Error(1)
}
func (n *mockNode) CleanupFolders(ctx context.Context, folders ...string) error {
	args := n.Called(ctx, folders)
	return args.Error(0)
}
func (n *mockNode) AuthInstance() auth.Auth { return nil }
func (n *mockNode) CreateDownloader(ctx context.Context, c request.Client, s setting.Provider) (downloader.Downloader, error) {
	return nil, nil
}
func (n *mockNode) Settings(ctx context.Context) *types.NodeSetting {
	args := n.Called(ctx)
	if args.Get(0) == nil {
		return nil
	}
	return args.Get(0).(*types.NodeSetting)
}
func (n *mockNode) PrepareUpload(ctx context.Context, args *fs.StatelessPrepareUploadService) (*fs.StatelessPrepareUploadResponse, error) {
	return nil, nil
}
func (n *mockNode) CompleteUpload(ctx context.Context, args *fs.StatelessCompleteUploadService) error {
	return nil
}
func (n *mockNode) OnUploadFailed(ctx context.Context, args *fs.StatelessOnUploadFailedService) error {
	return nil
}
func (n *mockNode) CreateFile(ctx context.Context, args *fs.StatelessCreateFileService) error {
	return nil
}

// mockSettingProvider implements setting.Provider – only MaxParallelTransfer
// is used by slaveTransfer; all other methods panic to surface unexpected calls.
type mockSettingProvider struct {
	setting.Provider // embed interface – unimplemented methods will panic on nil
	maxParallel      int
}

func (m *mockSettingProvider) MaxParallelTransfer(ctx context.Context) int {
	return m.maxParallel
}

// mockDep implements dependency.Dep with only the methods slaveTransfer needs.
type mockDep struct {
	dependency.Dep // embed interface – unimplemented methods will panic on nil
	logger         logging.Logger
	settingProvider *mockSettingProvider
}

func (d *mockDep) Logger() logging.Logger             { return d.logger }
func (d *mockDep) SettingProvider() setting.Provider   { return d.settingProvider }

// ---------------------------------------------------------------------------
// Helper to build a RemoteDownloadTask pre-configured for slaveTransfer tests.
// ---------------------------------------------------------------------------

func newSlaveTransferTestTask(state *RemoteDownloadTaskState) *RemoteDownloadTask {
	stateBytes, _ := json.Marshal(state)
	return &RemoteDownloadTask{
		DBTask: &queue.DBTask{
			Task: &ent.Task{
				ID:           1,
				PrivateState: string(stateBytes),
				PublicState:  &types.TaskPublicState{},
			},
		},
		l:     logging.NewConsoleLogger(logging.LevelDebug),
		state: state,
	}
}

func slaveStateJSON(files []SlaveUploadEntity, transferred map[int]interface{}, errMsg string) string {
	b, _ := json.Marshal(&SlaveUploadTaskState{
		Files:                files,
		Transferred:          transferred,
		MaxParallel:          2,
		UserID:               1,
		First5TransferErrors: errMsg,
	})
	return string(b)
}

func ctxWithDepAndUser(ctx context.Context, dep dependency.Dep, user *ent.User) context.Context {
	ctx = context.WithValue(ctx, dependency.DepCtx{}, dep)
	ctx = context.WithValue(ctx, inventory.UserCtx{}, user)
	return ctx
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestSlaveTransfer_PartialSuccess_ReturnsSuspending(t *testing.T) {
	// Scenario: slave completed but only transferred 2 of 3 files.
	// Expected: StatusSuspending, no error, SlaveUploadTaskID reset,
	// Transferred map updated with successful file indices.

	files := []SlaveUploadEntity{
		{Src: "/tmp/a.mkv", Index: 10},
		{Src: "/tmp/b.mkv", Index: 20},
		{Src: "/tmp/c.mkv", Index: 30},
	}
	transferred := map[int]interface{}{0: nil, 1: nil} // slice indices 0 and 1 → file indices 10 and 20

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusCompleted,
		PrivateState: slaveStateJSON(files, transferred, "upload timeout"),
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 10, Name: "a.mkv", Selected: true},
				{Index: 20, Name: "b.mkv", Selected: true},
				{Index: 30, Name: "c.mkv", Selected: true},
			},
		},
		Transferred: make(map[int]interface{}),
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.NoError(t, err, "partial success must NOT return an error")
	assert.Equal(t, task.StatusSuspending, next, "partial success must return StatusSuspending for immediate retry")
	assert.Equal(t, 0, m.state.SlaveUploadTaskID, "SlaveUploadTaskID must be reset to 0")

	// Check that transferred map now contains file indices 10 and 20
	assert.Contains(t, m.state.Transferred, 10, "file index 10 should be in Transferred")
	assert.Contains(t, m.state.Transferred, 20, "file index 20 should be in Transferred")
	assert.NotContains(t, m.state.Transferred, 30, "file index 30 should NOT be in Transferred (it failed)")

	// Phase should still be transfer (not advanced to seeding)
	assert.Equal(t, RemoteDownloadTaskPhaseTransfer, m.state.Phase)
}

func TestSlaveTransfer_AllFailed_ReturnsError(t *testing.T) {
	// Scenario: slave completed but transferred 0 files.
	// Expected: StatusError (real error), so queue handles retry/backoff.

	files := []SlaveUploadEntity{
		{Src: "/tmp/a.mkv", Index: 10},
		{Src: "/tmp/b.mkv", Index: 20},
	}
	transferred := map[int]interface{}{} // nothing transferred

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusError,
		PrivateState: slaveStateJSON(files, transferred, "connection refused"),
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 10, Name: "a.mkv", Selected: true},
				{Index: 20, Name: "b.mkv", Selected: true},
			},
		},
		Transferred: make(map[int]interface{}),
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.Error(t, err, "total failure must return an error")
	assert.Equal(t, task.StatusError, next, "total failure must return StatusError")
	assert.Contains(t, err.Error(), "all 2 files")
}

func TestSlaveTransfer_FullSuccess_AdvancesToSeeding(t *testing.T) {
	// Scenario: slave transferred all 2 files.
	// Expected: phase advances to AwaitSeeding, StatusSuspending.

	files := []SlaveUploadEntity{
		{Src: "/tmp/a.mkv", Index: 10},
		{Src: "/tmp/b.mkv", Index: 20},
	}
	transferred := map[int]interface{}{0: nil, 1: nil}

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusCompleted,
		PrivateState: slaveStateJSON(files, transferred, ""),
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 10, Name: "a.mkv", Selected: true},
				{Index: 20, Name: "b.mkv", Selected: true},
			},
		},
		Transferred: make(map[int]interface{}),
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.NoError(t, err)
	assert.Equal(t, task.StatusSuspending, next)
	assert.Equal(t, RemoteDownloadTaskPhaseAwaitSeeding, m.state.Phase, "phase must advance to AwaitSeeding")
}

func TestSlaveTransfer_Canceled_ReturnsCriticalErr(t *testing.T) {
	// Scenario: slave task was canceled.
	// Expected: StatusError with CriticalErr (non-retryable).

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusCanceled,
		PrivateState: "{}",
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 10, Name: "a.mkv", Selected: true},
			},
		},
		Transferred: make(map[int]interface{}),
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.Error(t, err)
	assert.Equal(t, task.StatusError, next)
	assert.True(t, errors.Is(err, queue.CriticalErr), "canceled slave must be a critical (non-retryable) error")
}

func TestSlaveTransfer_StillRunning_ReturnsSuspending(t *testing.T) {
	// Scenario: slave task is still processing.
	// Expected: StatusSuspending, SlaveUploadTaskID unchanged.

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusProcessing,
		PrivateState: "{}",
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 10, Name: "a.mkv", Selected: true},
			},
		},
		Transferred: make(map[int]interface{}),
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.NoError(t, err)
	assert.Equal(t, task.StatusSuspending, next)
	assert.Equal(t, 42, m.state.SlaveUploadTaskID, "SlaveUploadTaskID must not be reset while still running")
}

func TestSlaveTransfer_PartialSuccess_PreservesExistingTransferred(t *testing.T) {
	// Scenario: a previous retry already transferred file index 5.
	// This batch transfers 3 more files, 2 succeed (indices 10, 20).
	// Expected: Transferred map contains 5, 10, and 20 after merge.

	files := []SlaveUploadEntity{
		{Src: "/tmp/a.mkv", Index: 10},
		{Src: "/tmp/b.mkv", Index: 20},
		{Src: "/tmp/c.mkv", Index: 30},
	}
	transferred := map[int]interface{}{0: nil, 1: nil} // indices 10 and 20

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusCompleted,
		PrivateState: slaveStateJSON(files, transferred, "timeout"),
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 5, Name: "prev.mkv", Selected: true},
				{Index: 10, Name: "a.mkv", Selected: true},
				{Index: 20, Name: "b.mkv", Selected: true},
				{Index: 30, Name: "c.mkv", Selected: true},
			},
		},
		Transferred: map[int]interface{}{5: nil}, // previously transferred
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.NoError(t, err)
	assert.Equal(t, task.StatusSuspending, next)

	assert.Contains(t, m.state.Transferred, 5, "previously transferred file must be preserved")
	assert.Contains(t, m.state.Transferred, 10)
	assert.Contains(t, m.state.Transferred, 20)
	assert.NotContains(t, m.state.Transferred, 30, "failed file must not be in Transferred")
}

func TestSlaveTransfer_PartialSuccess_SlaveStatusError(t *testing.T) {
	// Scenario: slave task ended with StatusError but some files transferred.
	// Expected: same as partial success with StatusCompleted – StatusSuspending.

	files := []SlaveUploadEntity{
		{Src: "/tmp/a.mkv", Index: 10},
		{Src: "/tmp/b.mkv", Index: 20},
	}
	transferred := map[int]interface{}{0: nil} // only first file

	node := new(mockNode)
	node.On("GetTask", mock.Anything, 42, true).Return(&cluster.SlaveTaskSummary{
		Status:       task.StatusError,
		PrivateState: slaveStateJSON(files, transferred, "disk full"),
	}, nil)

	state := &RemoteDownloadTaskState{
		Dst:               "cloudreve://dst/",
		SlaveUploadTaskID: 42,
		Status: &downloader.TaskStatus{
			Files: []downloader.TaskFile{
				{Index: 10, Name: "a.mkv", Selected: true},
				{Index: 20, Name: "b.mkv", Selected: true},
			},
		},
		Transferred: make(map[int]interface{}),
	}

	m := newSlaveTransferTestTask(state)
	m.node = node

	dep := &mockDep{
		logger:          logging.NewConsoleLogger(logging.LevelDebug),
		settingProvider: &mockSettingProvider{maxParallel: 2},
	}
	ctx := ctxWithDepAndUser(context.Background(), dep, &ent.User{ID: 1})

	next, err := m.slaveTransfer(ctx, dep)

	assert.NoError(t, err, "partial success with StatusError must return no error")
	assert.Equal(t, task.StatusSuspending, next)
	assert.Contains(t, m.state.Transferred, 10)
	assert.NotContains(t, m.state.Transferred, 20)
}
