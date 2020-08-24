// Copyright 2018 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package schedulers

import (
	log "github.com/pingcap/log"
	"github.com/tikv/pd/server/core"
	"github.com/tikv/pd/server/schedule"
	"go.uber.org/zap"
)

func init() {
	schedule.RegisterScheduler("label", func(opController *schedule.OperatorController, args []string) (schedule.Scheduler, error) {
		return newLabelScheduler(opController), nil
	})
}

const labelSchedulerName = "label-scheduler"

type labelScheduler struct {
	name string
	*baseScheduler
	selector *schedule.BalanceSelector
}

// LabelScheduler is mainly based on the store's label information for scheduling.
// Now only used for reject leader schedule, that will move the leader out of
// the store with the specific label.
func newLabelScheduler(opController *schedule.OperatorController) schedule.Scheduler {
	filters := []schedule.Filter{
		schedule.StoreStateFilter{ActionScope: labelSchedulerName, TransferLeader: true},
	}
	kind := core.NewScheduleKind(core.LeaderKind, core.BySize)
	return &labelScheduler{
		name:          labelSchedulerName,
		baseScheduler: newBaseScheduler(opController),
		selector:      schedule.NewBalanceSelector(kind, filters),
	}
}

func (s *labelScheduler) GetName() string {
	return s.name
}

func (s *labelScheduler) GetType() string {
	return "label"
}

func (s *labelScheduler) IsScheduleAllowed(cluster schedule.Cluster) bool {
	return s.opController.OperatorCount(schedule.OpLeader) < cluster.GetLeaderScheduleLimit()
}

func (s *labelScheduler) Schedule(cluster schedule.Cluster) []*schedule.Operator {
	schedulerCounter.WithLabelValues(s.GetName(), "schedule").Inc()
	stores := cluster.GetStores()
	rejectLeaderStores := make(map[uint64]struct{})
	for _, s := range stores {
		if cluster.CheckLabelProperty(schedule.RejectLeader, s.GetLabels()) {
			rejectLeaderStores[s.GetID()] = struct{}{}
		}
	}
	if len(rejectLeaderStores) == 0 {
		schedulerCounter.WithLabelValues(s.GetName(), "skip").Inc()
		return nil
	}
	log.Debug("label scheduler reject leader store list", zap.Reflect("stores", rejectLeaderStores))
	for id := range rejectLeaderStores {
		if region := cluster.RandLeaderRegion(id); region != nil {
			log.Debug("label scheduler selects region to transfer leader", zap.Uint64("region-id", region.GetID()))
			excludeStores := make(map[uint64]struct{})
			for _, p := range region.GetDownPeers() {
				excludeStores[p.GetPeer().GetStoreId()] = struct{}{}
			}
			for _, p := range region.GetPendingPeers() {
				excludeStores[p.GetStoreId()] = struct{}{}
			}
			f := schedule.NewExcludedFilter(s.GetName(), nil, excludeStores)
			target := s.selector.SelectTarget(cluster, cluster.GetFollowerStores(region), f)
			if target == nil {
				log.Debug("label scheduler no target found for region", zap.Uint64("region-id", region.GetID()))
				schedulerCounter.WithLabelValues(s.GetName(), "no_target").Inc()
				continue
			}

			schedulerCounter.WithLabelValues(s.GetName(), "new_operator").Inc()
			op := schedule.CreateTransferLeaderOperator("label-reject-leader", region, id, target.GetID(), schedule.OpLeader)
			return []*schedule.Operator{op}
		}
	}
	schedulerCounter.WithLabelValues(s.GetName(), "no_region").Inc()
	return nil
}
