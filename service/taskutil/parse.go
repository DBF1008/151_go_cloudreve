// Package taskutil provides shared helpers for parsing persisted task models
// into display-ready Task objects and for enriching them with node information.
//
// The key invariant is that all functions in this package tolerate unknown task
// types gracefully: an unrecognised type is wrapped in a bare DBTask so that
// list/detail endpoints never fail just because the factory registry does not
// contain a particular type string.
package taskutil

import (
	"github.com/cloudreve/Cloudreve/v4/ent"
	"github.com/cloudreve/Cloudreve/v4/pkg/hashid"
	"github.com/cloudreve/Cloudreve/v4/pkg/queue"
	"github.com/samber/lo"
)

// ParseTasks converts a slice of persisted ent.Task models into display-ready
// queue.Task values. Unknown task types are wrapped in a bare *queue.DBTask so
// that callers always receive a valid Task for every input row.
func ParseTasks(models []*ent.Task) []queue.Task {
	return lo.Map(models, func(m *ent.Task, _ int) queue.Task {
		return queue.NewTaskFromModelOrFallback(m)
	})
}

// CollectNodeIDs extracts the set of distinct node IDs referenced by the given
// tasks' summaries. The returned map contains each node ID mapped to nil; the
// caller is expected to fill in the actual *ent.Node values after a batch fetch.
func CollectNodeIDs(tasks []queue.Task, hasher hashid.Encoder) map[int]*ent.Node {
	nodeMap := make(map[int]*ent.Node)
	for _, t := range tasks {
		if s := t.Summarize(hasher); s != nil && s.NodeID > 0 {
			if _, ok := nodeMap[s.NodeID]; !ok {
				nodeMap[s.NodeID] = nil
			}
		}
	}
	return nodeMap
}

// FillNodes populates the nodeMap entries (keyed by node ID) with the
// corresponding *ent.Node values from the provided slice. Entries that have no
// match in nodes remain nil.
func FillNodes(nodeMap map[int]*ent.Node, nodes []*ent.Node) {
	for _, n := range nodes {
		nodeMap[n.ID] = n
	}
}

// LookupNode returns the *ent.Node associated with the given task's summary
// NodeID, or nil if the task has no summary, the summary has no node ID, or
// the node ID is not present in nodeMap.
func LookupNode(t queue.Task, hasher hashid.Encoder, nodeMap map[int]*ent.Node) *ent.Node {
	s := t.Summarize(hasher)
	if s == nil || s.NodeID <= 0 {
		return nil
	}
	return nodeMap[s.NodeID]
}
