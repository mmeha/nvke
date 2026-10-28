// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/nodes"
	"github.com/dsx-ai-factory/nvke/controller/internal/testutil"
)

// rebalanceCluster builds a Nico cluster wired for rebalance: a control-plane
// config pinned to configID carrying the ordered preference, a VIP so
// replacement is not blocked, and the two flags. rebalanceEnabled maps to the
// inverted ControlPlaneConfig.NodeRebalanceDisabled toggle; scaleDownDisabled sets the automatic
// scale-down opt-out, which rebalance is deliberately independent of.
func rebalanceCluster(clusterID, configID uuid.UUID, preference []uuid.UUID, rebalanceEnabled, scaleDownDisabled bool) db.Cluster {
	var spec db.ControlPlaneConfigSpec
	spec.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: preference})
	return db.Cluster{
		ID:              clusterID,
		Name:            "test-cluster",
		ProviderType:    db.ProviderTypeNico,
		ControlPlaneVIP: "10.0.0.1",
		ControlPlaneConfig: db.ControlPlaneConfig{
			ConfigurationID:        configID,
			DesiredCount:           3,
			AutoScaleDownDisabled:  scaleDownDisabled,
			NodeRebalanceDisabled:  !rebalanceEnabled,
			ControlPlaneConfigSpec: spec,
		},
	}
}

// cpNodeOnType is a healthy, joined control-plane node placed on instanceType
// and pinned to configID.
func cpNodeOnType(clusterID, configID uuid.UUID, name string, instanceType uuid.UUID, createdAt time.Time) db.Node {
	node := healthyCPNode(clusterID, name, createdAt)
	node.ControlPlaneConfigurationID = configID
	node.SetNico(db.NicoNodeProvider{InstanceTypeID: instanceType})
	return node
}

func newRebalanceReconciler(tx *testutil.MockTx) *reconciler {
	return &reconciler{db: tx, logger: slog.Default()}
}

func newNodeProviderFor(t *testing.T, cluster db.Cluster) clients.NewNodeProvider {
	t.Helper()
	p, err := clients.GetNewNodeProvider(cluster)
	require.NoError(t, err)
	return p
}

// TestReconcileControlPlaneRebalance_SurgesMorePreferredReplacement pins the
// happy path: a joined node on a less-preferred type triggers a surge that is
// marked, pinned to a preference-capped snapshot, and created one at a time.
// Automatic scale-down is disabled here to pin that rebalance surges regardless
// of that toggle -- rebalance drives both the surge and the later shed itself.
func TestReconcileControlPlaneRebalance_SurgesMorePreferredReplacement(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour)),
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("ListControlPlaneConfigurationsByCluster", mock.Anything, clusterID).
		Return([]db.ControlPlaneConfiguration{}, nil)
	// The capped snapshot keeps only the primary type (strictly better than the
	// worst node's rank 1).
	mockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.MatchedBy(func(row *db.ControlPlaneConfiguration) bool {
		nico, err := row.Config.NicoProvider()
		return err == nil &&
			len(nico.InstanceTypePreference) == 1 &&
			nico.InstanceTypePreference[0] == primaryType &&
			row.ChangeReason == rebalanceSnapshotReason
	})).Return(db.ControlPlaneConfiguration{ID: cappedID, ClusterID: clusterID}, nil)
	mockTx.On("ListClusterNodeNames", mock.Anything, clusterID).
		Return([]string{"cp-a", "cp-b", "cp-old"}, nil)

	var created db.Node
	mockTx.On("CreateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		created = *node
		return isRebalanceReplacement(*node) && node.ControlPlaneConfigurationID == cappedID
	})).Return(db.Node{}, nil)
	// Starting a surge stamps the cooldown on the cluster.
	mockTx.On("GetCluster", mock.Anything, clusterID).Return(cluster, nil)
	var recorded db.Cluster
	mockTx.On("UpdateCluster", mock.Anything, mock.MatchedBy(func(c *db.Cluster) bool {
		recorded = *c
		return true
	})).Return(db.Cluster{}, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, newNodeProviderFor(t, cluster))
	require.NoError(t, err)

	mockTx.AssertNumberOfCalls(t, "CreateNode", 1)
	assert.True(t, isRebalanceReplacement(created), "the surge node carries the rebalance marker")
	assert.Equal(t, cappedID, created.ControlPlaneConfigurationID, "the surge is pinned to the capped snapshot")
	assert.Equal(t, db.ConditionStatusTrue, recorded.Conditions.ControlPlaneRebalanceAttempted.Status, "the surge is recorded to start the cooldown")
	assert.False(t, recorded.Conditions.ControlPlaneRebalanceAttempted.LastTransitionTime.IsZero(), "the attempt carries a timestamp")
}

// TestReconcileControlPlaneRebalance_NoOps covers the gates that must surge
// nothing and write nothing.
func TestReconcileControlPlaneRebalance_NoOps(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()

	balanced := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
	}
	unbalanced := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
	}

	// A CAPI cluster expresses no ordered preference, so the ranker is nil.
	// NodeRebalanceDisabled left false (the default): rebalancing is on, and the
	// nil ranker is what makes this a no-op, not the flag.
	capiCluster := db.Cluster{
		ID: clusterID, Name: "test-cluster", ProviderType: db.ProviderTypeCAPI,
		ControlPlaneVIP:    "10.0.0.1",
		ControlPlaneConfig: db.ControlPlaneConfig{ConfigurationID: configID, DesiredCount: 3},
	}

	// A less-preferred node exists, but a surge was started within the cooldown
	// window, so the step waits rather than churning another.
	coolingDown := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	coolingDown.Conditions.ControlPlaneRebalanceAttempted = db.NewCondition(true, "recent surge")

	tests := []struct {
		name    string
		cluster db.Cluster
		nodes   []db.Node
	}{
		{name: "flag off", cluster: rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, false, false), nodes: unbalanced},
		{name: "provider has no ranker", cluster: capiCluster, nodes: unbalanced},
		{name: "already balanced on the primary type", cluster: rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false), nodes: balanced},
		{name: "cooling down after a recent surge", cluster: coolingDown, nodes: unbalanced},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mockTx := &testutil.MockTx{}
			r := newRebalanceReconciler(mockTx)
			cp := &controlPlane{nodes: tt.nodes, clusterControlPlaneConfig: tt.cluster.ControlPlaneConfig}

			err := r.reconcileControlPlaneRebalance(context.Background(), tt.cluster, cp, domainAllocator{}, nil)
			require.NoError(t, err)

			mockTx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
			mockTx.AssertNotCalled(t, "UpdateCluster", mock.Anything, mock.Anything)
		})
	}
}

// TestRebalanceCoolingDown pins the surge rate-limit: a surge started within
// rebalanceAttemptInterval blocks another, and the window reopens once it elapses.
func TestRebalanceCoolingDown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		last time.Time
		want bool
	}{
		{name: "never attempted", last: time.Time{}, want: false},
		{name: "just surged", last: time.Now(), want: true},
		{name: "within the interval", last: time.Now().Add(-rebalanceAttemptInterval + time.Minute), want: true},
		{name: "interval elapsed", last: time.Now().Add(-rebalanceAttemptInterval - time.Minute), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cluster db.Cluster
			if !tt.last.IsZero() {
				cluster.Conditions.ControlPlaneRebalanceAttempted = db.Condition{
					Status:             db.ConditionStatusTrue,
					LastTransitionTime: tt.last,
				}
			}

			assert.Equal(t, tt.want, rebalanceCoolingDown(cluster))
		})
	}
}

// TestReconcileControlPlaneRebalance_DefersWhileRolloutOwed pins that a pending
// rollout (drift) blocks a new surge even with recycling disabled, so rebalance
// never muddies an operator's pending config change.
func TestReconcileControlPlaneRebalance_DefersWhileRolloutOwed(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	cluster.ControlPlaneConfig.KubernetesVersion = "v1.34.2"

	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{
		nodes:                     nodesList,
		clusterControlPlaneConfig: cluster.ControlPlaneConfig,
		// Every node is pinned to an older version snapshot, so a rollout is owed.
		nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
			configID: {KubernetesVersion: "v1.33.6"},
		},
	}
	require.Positive(t, cp.countNodesNeedingRollout(), "the scenario must have a rollout owed")

	err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)

	mockTx.AssertNotCalled(t, "CreateControlPlaneConfiguration", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_WaitsWhileSurgeComingUp pins that an
// in-flight, not-yet-joined surge is left to come up: no new surge, no teardown.
func TestReconcileControlPlaneRebalance_WaitsWhileSurgeComingUp(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)

	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.NewCondition(true, "Pending")
	surge.Conditions.ControlPlaneRebalanceReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-old", worseType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)

	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "GetNodeForUpdate", mock.Anything, mock.Anything)
	mockTx.AssertNotCalled(t, "CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_FinishesJoinedSurge pins the completion
// step: once the surge joins, rebalance sheds the old, less-preferred node it
// replaced through the control-plane delete path, then clears the marker while
// leaving the surge pinned to its capped snapshot (no re-pin). Automatic
// scale-down is disabled here to pin that rebalance sheds the old node regardless
// of that toggle, rather than leaning on scale-down.
func TestReconcileControlPlaneRebalance_FinishesJoinedSurge(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, true)

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)
	surge.Conditions.ControlPlaneRebalanceReplacement = db.NewCondition(true, "Rebalance surge")
	old := cpNodeOnType(clusterID, configID, "cp-old", worseType, base.Add(-time.Hour))
	// Four nodes at desired 3 (the surge is the +1): shedding the old node leaves
	// three joined members, so the quorum floor permits the shed.
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		old,
		surge,
	}

	mockTx := &testutil.MockTx{}
	// Self-shed of the old node through the shared CP delete path.
	mockTx.On("GetNode", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	mockTx.On("GetNodeForUpdate", mock.Anything, old.ID).Return(old, nil)
	mockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil)
	mockTx.On("ListNodes", mock.Anything, mock.Anything).Return(nodesList, nil)
	// Clearing the marker locks and re-writes the surge node.
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)

	var writes []db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		writes = append(writes, *node)
		return true
	})).Return(surge, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, newNodeProviderFor(t, cluster))
	require.NoError(t, err)

	require.Len(t, writes, 2, "one write records the old node's delete, one clears the surge marker")

	var oldWrite, surgeWrite db.Node
	for _, w := range writes {
		switch w.ID {
		case old.ID:
			oldWrite = w
		case surge.ID:
			surgeWrite = w
		}
	}

	assert.True(t, nodes.IsDeleteRequested(oldWrite), "the old less-preferred node is shed through the delete path")
	assert.False(t, isRebalanceReplacement(surgeWrite), "the marker is cleared once the surge joins")
	assert.Equal(t, cappedID, surgeWrite.ControlPlaneConfigurationID, "the joined surge keeps its capped snapshot pin (no re-pin)")
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_AbandonsSurgeOnUpgrade pins the yield rule:
// a not-yet-joined surge is torn down when an upgrade starts, so it never blocks
// the upgrade.
func TestReconcileControlPlaneRebalance_AbandonsSurgeOnUpgrade(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	configID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()
	cluster := rebalanceCluster(clusterID, configID, []uuid.UUID{primaryType, worseType}, true, false)
	cluster.Conditions.ControlPlaneUpgrading = db.NewCondition(true, "Upgrading")

	surge := healthyCPNode(clusterID, "cp-surge", base)
	surge.ControlPlaneConfigurationID = cappedID
	surge.Conditions.Joined = db.NewCondition(false, "Not joined")
	surge.Conditions.Pending = db.NewCondition(true, "Pending")
	surge.Conditions.ControlPlaneRebalanceReplacement = db.NewCondition(true, "Rebalance surge")
	nodesList := []db.Node{
		cpNodeOnType(clusterID, configID, "cp-a", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-b", primaryType, base),
		cpNodeOnType(clusterID, configID, "cp-c", primaryType, base),
		surge,
	}

	mockTx := &testutil.MockTx{}
	mockTx.On("GetNodeForUpdate", mock.Anything, surge.ID).Return(surge, nil)
	mockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
	var written db.Node
	mockTx.On("UpdateNode", mock.Anything, mock.MatchedBy(func(node *db.Node) bool {
		written = *node
		return true
	})).Return(surge, nil)
	mockTx.On("CreateWorkflowOutboxEntry", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		Return(db.WorkflowOutbox{}, nil)

	r := newRebalanceReconciler(mockTx)
	cp := &controlPlane{nodes: nodesList, clusterControlPlaneConfig: cluster.ControlPlaneConfig}

	err := r.reconcileControlPlaneRebalance(context.Background(), cluster, cp, domainAllocator{}, nil)
	require.NoError(t, err)

	assert.True(t, nodes.IsDeleting(written), "the surge is torn down to yield to the upgrade")
	mockTx.AssertNumberOfCalls(t, "CreateWorkflowOutboxEntry", 1)
	mockTx.AssertNotCalled(t, "CreateNode", mock.Anything, mock.Anything)
}

// TestReconcileControlPlaneRebalance_CappedSurgeRolloutDrift pins the correctness
// the no-re-pin rule buys. A joined surge is left pinned to its capped snapshot;
// because we never re-pin it to the cluster's current config, the pin stays
// honest, so rollout drift is computed against what the surge was actually built
// on. The cap itself (a truncated preference) is a non-rollout field, so it never
// looks like drift on its own; but a config change in a rollout field while the
// surge was provisioning is still visible, so the stale node gets recycled rather
// than silently stranded.
func TestReconcileControlPlaneRebalance_CappedSurgeRolloutDrift(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	currentID := uuid.New()
	cappedID := uuid.New()
	base := time.Now()
	primaryType := uuid.New()
	worseType := uuid.New()

	cluster := rebalanceCluster(clusterID, currentID, []uuid.UUID{primaryType, worseType}, true, false)
	cluster.ControlPlaneConfig.KubernetesVersion = "v1.34.2"

	surge := cpNodeOnType(clusterID, cappedID, "cp-surge", primaryType, base)

	cappedSameVersion := db.ControlPlaneConfigSpec{KubernetesVersion: "v1.34.2"}
	cappedSameVersion.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{primaryType}})

	cappedStaleVersion := db.ControlPlaneConfigSpec{KubernetesVersion: "v1.33.6"}
	cappedStaleVersion.SetNico(db.NicoControlPlaneProvider{InstanceTypePreference: []uuid.UUID{primaryType}})

	tests := []struct {
		name       string
		cappedSpec db.ControlPlaneConfigSpec
		wantCount  int
	}{
		{
			name:       "capped preference alone is not rollout drift",
			cappedSpec: cappedSameVersion,
			wantCount:  0,
		},
		{
			name:       "stale capped snapshot after a config change is still drift",
			cappedSpec: cappedStaleVersion,
			wantCount:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cp := &controlPlane{
				nodes:                     []db.Node{surge},
				clusterControlPlaneConfig: cluster.ControlPlaneConfig,
				nodePinnedControlPlaneConfigs: map[uuid.UUID]db.ControlPlaneConfigSpec{
					cappedID: tt.cappedSpec,
				},
			}

			assert.Equal(t, tt.wantCount, cp.countNodesNeedingRollout())
		})
	}
}
