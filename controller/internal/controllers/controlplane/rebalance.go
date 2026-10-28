// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	// rebalanceSnapshotReason tags the transient, preference-capped
	// control-plane-config snapshots the rebalance step pins surge nodes to, so
	// they are distinguishable from operator-driven configuration changes in the
	// snapshot history.
	rebalanceSnapshotReason = "internal: rebalance preference cap"

	// defaultRebalanceSurgeTimeout bounds how long a not-yet-joined rebalance
	// surge may wait for capacity before it is abandoned. The cap guarantees the
	// surge never lands on a type no better than the node it replaces, so a surge
	// that cannot come up means the whole capped preference is out of capacity;
	// abandoning it frees the slot and lets the step re-probe later, when either
	// capacity returns or the least-preferred node has changed.
	defaultRebalanceSurgeTimeout = 45 * time.Minute

	// rebalanceShedReason is the delete reason recorded when rebalance sheds the
	// old, less-preferred node its surge replaced.
	rebalanceShedReason = "Control-plane rebalance: shedding a less-preferred node after its replacement joined"

	// rebalanceAttemptInterval rate-limits how often the step starts a new surge.
	// A surge that cannot find capacity is abandoned after
	// defaultRebalanceSurgeTimeout; without a cooldown the step would re-surge on
	// the very next reconcile and churn a node roughly every surge timeout. The
	// step records each surge on the cluster and skips starting another until this
	// interval elapses, so a cluster whose preferred instance type is out of
	// capacity re-probes on a slow, fixed cadence (roughly once every three hours)
	// rather than continuously.
	rebalanceAttemptInterval = 3 * time.Hour
)

// reconcileControlPlaneRebalance moves the control plane onto more-preferred
// instance types at steady state: when a joined member runs on a less-preferred
// type, it surges one more-preferred replacement, then sheds the old node itself
// -- through the shared control-plane delete path, with the quorum floor applied
// -- once the replacement joins. It drives both halves of the surge-then-shed, so
// it is independent of the automatic scale-down toggle. It is opportunistic and
// the lowest-priority control-plane step -- it only acts when nothing else is in
// flight, and it yields to upgrades and rollouts by abandoning an in-flight
// surge rather than blocking them. Surge starts are rate-limited to one per
// rebalanceAttemptInterval, so a cluster that cannot get its preferred instance
// type re-probes slowly instead of churning a surge every surge timeout.
//
// The step is provider-neutral: it decides whether to surge through the
// ControlPlaneScaleDownRanker seam, and bounds the surge's instance-type
// fallback through the ControlPlaneRebalanceCapper seam, both resolved from the
// cluster's control-plane config. A provider that expresses no ordered
// placement preference implements neither, so ScaleDownRanker returns nil and
// this step is a no-op for it. See ControlPlaneRebalanceCapper for how a new
// provider opts in.
func (r *reconciler) reconcileControlPlaneRebalance(
	ctx context.Context,
	cluster db.Cluster,
	cp *controlPlane,
	allocator domainAllocator,
	newNodeProvider clients.NewNodeProvider,
) error {
	if cluster.ControlPlaneConfig.NodeRebalanceDisabled {
		return nil
	}
	ranker := cluster.ControlPlaneConfig.ScaleDownRanker()
	if ranker == nil {
		return nil
	}

	logger := r.logger.With(
		"clusterID", cluster.ID,
		"clusterName", cluster.Name,
	)

	// An in-flight surge takes precedence: resolve it (clear + re-pin on join) or
	// abandon it (yield / timeout) before considering a new one.
	if surge, ok := cp.rebalanceSurge(); ok {
		return r.reconcileRebalanceSurge(ctx, cluster, cp, surge, logger)
	}

	// Only start a surge at steady state, and never while a higher-priority
	// operation owns the same slots. upgradeInProgress covers k8s upgrades and
	// recycling; the explicit rollout check also covers drift with recycling
	// disabled, which is a pending operator decision a surge must not muddy.
	if cp.upgradeInProgress(cluster) {
		return nil
	}
	if cp.countNodesNeedingRollout() > 0 {
		return nil
	}
	if cp.size() != cluster.ControlPlaneConfig.DesiredCount {
		return nil
	}
	if _, inFlight := cp.nodeWithDeleteInFlight(); inFlight {
		return nil
	}

	eligible := cp.scaleDownEligible()
	if !controlPlaneHasNodeBelowPrimaryRank(eligible, ranker) {
		return nil
	}
	// Rate-limit surge starts: a prior surge within the interval means we re-probe
	// later rather than churning a new one every surge timeout while the preferred
	// type stays out of capacity.
	if rebalanceCoolingDown(cluster) {
		logger.Debug("Control-plane rebalance cooling down since the last surge; skipping",
			"lastAttempt", cluster.Conditions.ControlPlaneRebalanceAttempted.LastTransitionTime,
			"interval", rebalanceAttemptInterval,
		)
		return nil
	}
	if r.controlPlaneReplacementBlocked(cluster) {
		logger.Warn("Skipping control-plane rebalance surge because controlPlaneVIP is not configured")
		return nil
	}

	// Cap the surge's fallback to types strictly better than the worst eligible
	// node: anything at or below that rank would be no improvement, so it is
	// excluded. The cap rides a transient, preference-capped snapshot the surge is
	// pinned to; the provider's own instance-type walk reads the shorter list.
	capRank := worstControlPlaneRank(eligible, ranker)
	cappedSpec, ok := cluster.ControlPlaneConfig.CappedControlPlaneConfig(capRank)
	if !ok {
		// Provider ranks but cannot express a capped preference; do not surge
		// unbounded.
		return nil
	}
	snapshotID, err := db.FindOrCreateControlPlaneConfiguration(ctx, r.db, cluster.ID, cappedSpec, rebalanceSnapshotReason)
	if err != nil {
		return fmt.Errorf("resolving capped control-plane config snapshot: %w", err)
	}

	created, err := r.scaleUp(ctx, cluster, 1, "control-plane rebalance surge", allocator, newNodeProvider, withRebalanceReplacement(snapshotID))
	if err != nil {
		return fmt.Errorf("surging control-plane rebalance replacement: %w", err)
	}
	if created == 1 {
		logger.Info("Surged control-plane rebalance replacement; scale-down sheds the old node once it joins",
			"preferenceCap", capRank,
			"configurationID", snapshotID,
		)
		// Start the cooldown so the next surge waits rebalanceAttemptInterval. A
		// record failure rolls back the surge in the same transaction, so a retry
		// re-surges cleanly rather than leaving an unrecorded one.
		if err := r.recordRebalanceAttempt(ctx, cluster.ID, logger); err != nil {
			return err
		}
	}
	return nil
}

// rebalanceCoolingDown reports whether a rebalance surge was started within
// rebalanceAttemptInterval, so the step should not start another yet. A cluster
// that has never recorded an attempt (zero timestamp) is never cooling down.
func rebalanceCoolingDown(cluster db.Cluster) bool {
	last := cluster.Conditions.ControlPlaneRebalanceAttempted.LastTransitionTime
	return !last.IsZero() && time.Since(last) < rebalanceAttemptInterval
}

// recordRebalanceAttempt stamps the cluster with the time of the surge just
// started, anchoring the rebalanceAttemptInterval cooldown. It re-reads the
// cluster before writing (the caller already holds the row lock) so it extends,
// rather than clobbers, conditions written earlier in the same reconcile.
func (r *reconciler) recordRebalanceAttempt(ctx context.Context, clusterID uuid.UUID, logger *slog.Logger) error {
	cluster, err := r.db.GetCluster(ctx, clusterID)
	if err != nil {
		return fmt.Errorf("loading cluster %s to record rebalance attempt: %w", clusterID, err)
	}
	cluster.Conditions.ControlPlaneRebalanceAttempted = db.NewCondition(true, "Surged a more-preferred control-plane replacement")
	if _, err := r.db.UpdateCluster(ctx, &cluster); err != nil {
		return fmt.Errorf("recording rebalance attempt for cluster %s: %w", clusterID, err)
	}
	logger.Debug("Recorded control-plane rebalance attempt; next surge waits for the cooldown", "interval", rebalanceAttemptInterval)
	return nil
}

// reconcileRebalanceSurge drives an in-flight rebalance surge to resolution: it
// finishes the surge once it joins, and otherwise abandons it on a yield signal
// or timeout.
func (r *reconciler) reconcileRebalanceSurge(ctx context.Context, cluster db.Cluster, cp *controlPlane, surge db.Node, logger *slog.Logger) error {
	nodeLogger := logger.With("nodeID", surge.ID, "nodeName", surge.Name)

	// Joined: the surge is a healthy member. Shed the old, less-preferred node it
	// replaced and clear the marker.
	if isCleanJoinedControlPlaneMember(surge) {
		return r.finishRebalanceSurge(ctx, cluster, cp, surge, nodeLogger)
	}

	// Not yet joined. Yield to any higher-priority operation by abandoning the
	// surge, so it never blocks an upgrade or rollout (its capped-config pin and
	// marker die with the node).
	if cp.upgradeInProgress(cluster) || cp.countNodesNeedingRollout() > 0 {
		return r.abandonRebalanceSurge(ctx, cp, surge, "yielding to a control-plane upgrade or rollout", nodeLogger)
	}

	// No capacity across the whole capped preference within the timeout: give up
	// and re-probe later.
	if rebalanceSurgeExpired(surge) {
		return r.abandonRebalanceSurge(ctx, cp, surge, "no capacity for a more-preferred instance type", nodeLogger)
	}

	nodeLogger.Debug("Control-plane rebalance surge coming up; waiting")
	return nil
}

// finishRebalanceSurge completes a joined surge: it sheds the old, less-preferred
// node the surge replaced through the shared control-plane delete path, then
// clears the marker so the surge becomes an ordinary member. Shedding and
// clearing happen in the same pass; a teardown already in flight (a shed recorded
// by a prior pass whose marker clear did not land) short-circuits to just clearing
// the marker, so the surge replaces exactly one node and the step is safe to retry.
func (r *reconciler) finishRebalanceSurge(ctx context.Context, cluster db.Cluster, cp *controlPlane, surge db.Node, logger *slog.Logger) error {
	// One teardown at a time. If one is already in flight -- the shed this step
	// recorded on an earlier pass -- the surge has done its job; just clear the
	// marker rather than selecting a second victim.
	if _, inFlight := cp.nodeWithDeleteInFlight(); inFlight {
		return r.clearRebalanceMarker(ctx, surge, logger)
	}

	// Shed the old node the surge improves on: the least-preferred joined member
	// other than the surge, provided it ranks strictly worse. The quorum floor in
	// the delete path bounds the shrink; a floor refusal (desiredCount below the
	// floor) is benign and still falls through to clearing the marker, so the
	// surge stops being held out of scale-down.
	if victim, ok := leastPreferredControlPlaneNodeToShed(cp, surge, cluster.ControlPlaneConfig.ScaleDownRanker()); ok {
		recorded, err := r.recordControlPlaneManagedDelete(ctx, logger, victim, rebalanceShedReason)
		if err != nil {
			return err
		}
		if recorded {
			logger.Info("Rebalance surge joined; recorded delete of the less-preferred node it replaced",
				"victimID", victim.ID,
				"victimName", victim.Name,
			)
		}
	}

	return r.clearRebalanceMarker(ctx, surge, logger)
}

// clearRebalanceMarker clears the rebalance marker so scale-down stops holding the
// joined surge out. The surge keeps its pinned snapshot. The capped instance-type
// preference matters only while the machine is provisioning; once the node has
// joined, ranking uses the cluster's live config.
func (r *reconciler) clearRebalanceMarker(ctx context.Context, surge db.Node, logger *slog.Logger) error {
	locked, err := r.db.GetNodeForUpdate(ctx, surge.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("locking rebalance surge node %s: %w", surge.ID, err)
	}
	locked.Conditions.ControlPlaneRebalanceReplacement = db.NewCondition(false, "Rebalance surge joined")
	if _, err := r.db.UpdateNode(ctx, &locked); err != nil {
		return fmt.Errorf("finishing rebalance surge for node %s: %w", locked.ID, err)
	}
	logger.Info("Rebalance surge joined; cleared marker (surge keeps its capped snapshot)")
	return nil
}

// leastPreferredControlPlaneNodeToShed returns the control-plane node a joined rebalance surge replaces:
// the least-preferred clean joined member other than the surge, and only when it
// ranks strictly worse than the surge (so a shed is always a net improvement).
// A nil ranker, or nothing worse than the surge, reports no victim -- the signal
// the rebalance round is complete.
func leastPreferredControlPlaneNodeToShed(cp *controlPlane, surge db.Node, ranker db.ControlPlaneScaleDownRanker) (db.Node, bool) {
	if ranker == nil {
		return db.Node{}, false
	}
	var candidates []db.Node
	for _, node := range cp.scaleDownEligible() {
		if node.ID != surge.ID {
			candidates = append(candidates, node)
		}
	}
	if len(candidates) == 0 {
		return db.Node{}, false
	}
	victim := leastPreferredControlPlaneNode(candidates, ranker)
	if ranker.ScaleDownRank(victim) <= ranker.ScaleDownRank(surge) {
		return db.Node{}, false
	}
	return victim, true
}

// abandonRebalanceSurge tears the surge down through the serialized teardown
// path, the same one scale-down and rollout use.
func (r *reconciler) abandonRebalanceSurge(ctx context.Context, cp *controlPlane, surge db.Node, reason string, logger *slog.Logger) error {
	started, err := r.teardownOldNode(ctx, cp, surge, "Rebalance surge abandoned: "+reason, "rebalance-abandon", logger)
	if err != nil {
		return fmt.Errorf("abandoning rebalance surge for node %s: %w", surge.ID, err)
	}
	if started {
		logger.Info("Abandoned control-plane rebalance surge", "reason", reason)
	}
	return nil
}

// rebalanceSurgeExpired reports whether a not-yet-joined surge has been coming up
// longer than defaultRebalanceSurgeTimeout, anchored on when it entered Pending
// (falling back to its creation time).
func rebalanceSurgeExpired(surge db.Node) bool {
	anchor := surge.Conditions.Pending.LastTransitionTime
	if anchor.IsZero() {
		anchor = surge.CreatedAt
	}
	return !anchor.IsZero() && time.Since(anchor) > defaultRebalanceSurgeTimeout
}

// worstControlPlaneRank returns the highest ScaleDownRank among the eligible
// nodes -- the least-preferred placed member -- which is the exclusive bound for
// a rebalance surge: the surge may fall back only to types strictly better than
// this. Callers gate on controlPlaneHasNodeBelowPrimaryRank first, so the result
// is at least primaryControlPlaneRank+1.
func worstControlPlaneRank(eligible []db.Node, ranker db.ControlPlaneScaleDownRanker) int {
	worst := primaryControlPlaneRank
	for _, node := range eligible {
		if rank := ranker.ScaleDownRank(node); rank > worst {
			worst = rank
		}
	}
	return worst
}
