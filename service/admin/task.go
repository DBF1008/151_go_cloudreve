package admin

import (
	"context"
	"strconv"
	"time"

	"github.com/cloudreve/Cloudreve/v4/application/dependency"
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/ent/task"
	"github.com/cloudreve/Cloudreve/v4/inventory"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/logging"
	"github.com/cloudreve/Cloudreve/v4/pkg/queue"
	"github.com/cloudreve/Cloudreve/v4/pkg/serializer"
	"github.com/cloudreve/Cloudreve/v4/pkg/setting"
	"github.com/gin-gonic/gin"
	"github.com/gofrs/uuid"
	"github.com/samber/lo"
)

func GetQueueMetrics(c *gin.Context) ([]QueueMetric, error) {
	res := []QueueMetric{}
	dep := dependency.FromContext(c)

	mediaMeta := dep.MediaMetaQueue(c)
	entityRecycle := dep.EntityRecycleQueue(c)
	ioIntense := dep.IoIntenseQueue(c)
	remoteDownload := dep.RemoteDownloadQueue(c)
	thumb := dep.ThumbQueue(c)

	res = append(res, QueueMetric{
		Name:            setting.QueueTypeMediaMeta,
		BusyWorkers:     mediaMeta.BusyWorkers(),
		SuccessTasks:    mediaMeta.SuccessTasks(),
		FailureTasks:    mediaMeta.FailureTasks(),
		SubmittedTasks:  mediaMeta.SubmittedTasks(),
		SuspendingTasks: mediaMeta.SuspendingTasks(),
	})
	res = append(res, QueueMetric{
		Name:            setting.QueueTypeEntityRecycle,
		BusyWorkers:     entityRecycle.BusyWorkers(),
		SuccessTasks:    entityRecycle.SuccessTasks(),
		FailureTasks:    entityRecycle.FailureTasks(),
		SubmittedTasks:  entityRecycle.SubmittedTasks(),
		SuspendingTasks: entityRecycle.SuspendingTasks(),
	})
	res = append(res, QueueMetric{
		Name:            setting.QueueTypeIOIntense,
		BusyWorkers:     ioIntense.BusyWorkers(),
		SuccessTasks:    ioIntense.SuccessTasks(),
		FailureTasks:    ioIntense.FailureTasks(),
		SubmittedTasks:  ioIntense.SubmittedTasks(),
		SuspendingTasks: ioIntense.SuspendingTasks(),
	})
	res = append(res, QueueMetric{
		Name:            setting.QueueTypeRemoteDownload,
		BusyWorkers:     remoteDownload.BusyWorkers(),
		SuccessTasks:    remoteDownload.SuccessTasks(),
		FailureTasks:    remoteDownload.FailureTasks(),
		SubmittedTasks:  remoteDownload.SubmittedTasks(),
		SuspendingTasks: remoteDownload.SuspendingTasks(),
	})
	res = append(res, QueueMetric{
		Name:            setting.QueueTypeThumb,
		BusyWorkers:     thumb.BusyWorkers(),
		SuccessTasks:    thumb.SuccessTasks(),
		FailureTasks:    thumb.FailureTasks(),
		SubmittedTasks:  thumb.SubmittedTasks(),
		SuspendingTasks: thumb.SuspendingTasks(),
	})

	return res, nil
}

const (
	taskTypeCondition          = "task_type"
	taskStatusCondition        = "task_status"
	taskCorrelationIDCondition = "task_correlation_id"
	taskUserIDCondition        = "task_user_id"
)

// summarizeTaskModel parses a persisted task model and returns its display
// summary. Unknown task types degrade safely to a nil summary instead of
// returning an error, so a single unrecognized task can't break the whole list
// or detail endpoint; callers can still surface basic task information for it.
func summarizeTaskModel(model *ent.Task, hasher hashid.Encoder, l logging.Logger) *queue.Summary {
	t, err := queue.NewTaskFromModel(model)
	if err != nil {
		if l != nil {
			l.Warning("Failed to parse task %d of type %q for display, falling back to basic info: %s", model.ID, model.Type, err)
		}
		return nil
	}

	return t.Summarize(hasher)
}

// newTaskResponse assembles a GetTaskResponse from a task model and its
// (possibly nil) summary, resolving the related node from the provided lookup.
// Identification fields are always populated; Summary and Node are only set when
// a summary is available, so unknown task types still return basic information.
func newTaskResponse(model *ent.Task, summary *queue.Summary, hasher hashid.Encoder, nodes map[int]*ent.Node) GetTaskResponse {
	resp := GetTaskResponse{
		Task:       model,
		TaskHashID: hashid.EncodeTaskID(hasher, model.ID),
	}

	if model.Edges.User != nil {
		resp.UserHashID = hashid.EncodeUserID(hasher, model.Edges.User.ID)
	}

	if summary != nil {
		resp.Summary = summary
		if summary.NodeID > 0 {
			resp.Node = nodes[summary.NodeID]
		}
	}

	return resp
}

func (s *AdminListService) Tasks(c *gin.Context) (*ListTaskResponse, error) {
	dep := dependency.FromContext(c)
	taskClient := dep.TaskClient()
	hasher := dep.HashIDEncoder()
	l := dep.Logger()
	var (
		err           error
		userID        int
		correlationID *uuid.UUID
		status        []task.Status
		taskType      []string
	)

	if s.Conditions[taskTypeCondition] != "" {
		taskType = []string{s.Conditions[taskTypeCondition]}
	}

	if s.Conditions[taskStatusCondition] != "" {
		status = []task.Status{task.Status(s.Conditions[taskStatusCondition])}
	}

	if s.Conditions[taskCorrelationIDCondition] != "" {
		cid, err := uuid.FromString(s.Conditions[taskCorrelationIDCondition])
		if err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, "Invalid task correlation ID", err)
		}
		correlationID = &cid
	}

	if s.Conditions[taskUserIDCondition] != "" {
		userID, err = strconv.Atoi(s.Conditions[taskUserIDCondition])
		if err != nil {
			return nil, serializer.NewError(serializer.CodeParamErr, "Invalid task user ID", err)
		}
	}

	ctx := context.WithValue(c, inventory.LoadTaskUser{}, true)
	res, err := taskClient.List(ctx, &inventory.ListTaskArgs{
		PaginationArgs: &inventory.PaginationArgs{
			Page:     s.Page - 1,
			PageSize: s.PageSize,
			OrderBy:  s.OrderBy,
			Order:    inventory.OrderDirection(s.OrderDirection),
		},
		UserID:        userID,
		CorrelationID: correlationID,
		Types:         taskType,
		Status:        status,
	})

	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to list tasks", err)
	}

	// Summarize each task once, collecting the nodes referenced by the summaries.
	// Unknown task types degrade to a nil summary instead of failing the whole
	// listing, so a single unrecognized task can't break the endpoint.
	summaries := make([]*queue.Summary, len(res.Tasks))
	nodeMap := make(map[int]*ent.Node)
	for i, t := range res.Tasks {
		summary := summarizeTaskModel(t, hasher, l)
		summaries[i] = summary
		if summary != nil && summary.NodeID > 0 {
			nodeMap[summary.NodeID] = nil
		}
	}

	// Resolve all referenced nodes in a single query.
	nodes, err := dep.NodeClient().GetNodeByIds(c, lo.Keys(nodeMap))
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to query nodes", err)
	}
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}

	return &ListTaskResponse{
		Pagination: res.PaginationResults,
		Tasks: lo.Map(res.Tasks, func(t *ent.Task, i int) GetTaskResponse {
			return newTaskResponse(t, summaries[i], hasher, nodeMap)
		}),
	}, nil
}

type (
	SingleTaskService struct {
		ID int `uri:"id" json:"id" binding:"required"`
	}
	SingleTaskParamCtx struct{}
)

func (s *SingleTaskService) Get(c *gin.Context) (*GetTaskResponse, error) {
	dep := dependency.FromContext(c)
	taskClient := dep.TaskClient()
	hasher := dep.HashIDEncoder()
	l := dep.Logger()

	ctx := context.WithValue(c, inventory.LoadTaskUser{}, true)
	task, err := taskClient.GetTaskByID(ctx, s.ID)
	if err != nil {
		return nil, serializer.NewError(serializer.CodeDBError, "Failed to get task", err)
	}

	// Unknown task types degrade to a nil summary so an unrecognized task still
	// returns basic information instead of failing the detail endpoint.
	summary := summarizeTaskModel(task, hasher, l)
	nodeMap := make(map[int]*ent.Node)
	if summary != nil && summary.NodeID > 0 {
		if node, err := dep.NodeClient().GetNodeById(c, summary.NodeID); err == nil {
			nodeMap[summary.NodeID] = node
		}
	}

	resp := newTaskResponse(task, summary, hasher, nodeMap)
	return &resp, nil
}

type (
	BatchTaskService struct {
		IDs []int `json:"ids" binding:"required"`
	}
	BatchTaskParamCtx struct{}
)

func (s *BatchTaskService) Delete(c *gin.Context) error {
	dep := dependency.FromContext(c)
	taskClient := dep.TaskClient()

	err := taskClient.DeleteByIDs(c, s.IDs...)
	if err != nil {
		return serializer.NewError(serializer.CodeDBError, "Failed to delete tasks", err)
	}

	return nil
}

type (
	CleanupTaskService struct {
		NotAfter time.Time     `json:"not_after" binding:"required"`
		Types    []string      `json:"types"`
		Status   []task.Status `json:"status"`
	}
	CleanupTaskParameterCtx struct{}
)

func (s *CleanupTaskService) CleanupTask(c *gin.Context) error {
	dep := dependency.FromContext(c)
	taskClient := dep.TaskClient()

	if len(s.Status) == 0 {
		s.Status = []task.Status{task.StatusCanceled, task.StatusCompleted, task.StatusError}
	}

	if err := taskClient.DeleteBy(c, &inventory.DeleteTaskArgs{
		NotAfter: s.NotAfter,
		Types:    s.Types,
		Status:   s.Status,
	}); err != nil {
		return serializer.NewError(serializer.CodeDBError, "Failed to cleanup tasks", err)
	}

	return nil
}
