/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package noderesourcetopology

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"

	apiconfig "sigs.k8s.io/scheduler-plugins/apis/config"
	nrtcache "sigs.k8s.io/scheduler-plugins/pkg/noderesourcetopology/cache"

	topologyapi "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology"
	topologyv1alpha1 "github.com/k8stopologyawareschedwg/noderesourcetopology-api/pkg/apis/topology/v1alpha1"
)

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = "NodeResourceTopologyMatch"
)

type NUMANode struct {
	NUMAID    int
	Resources v1.ResourceList
}

type NUMANodeList []NUMANode

func subtractFromNUMAs(resources v1.ResourceList, numaNodes NUMANodeList, nodes ...int) {
	for resName, quantity := range resources {
		for _, node := range nodes {
			// quantity is zero no need to iterate through another NUMA node, go to another resource
			if quantity.IsZero() {
				break
			}

			nRes := numaNodes[node].Resources
			if available, ok := nRes[resName]; ok {
				switch quantity.Cmp(available) {
				case 0: // the same
					// basically zero container resources
					quantity.Sub(available)
					// zero NUMA quantity
					nRes[resName] = resource.Quantity{}
				case 1: // container wants more resources than available in this NUMA zone
					// substract NUMA resources from container request, to calculate how much is missing
					quantity.Sub(available)
					// zero NUMA quantity
					nRes[resName] = resource.Quantity{}
				case -1: // there are more resources available in this NUMA zone than container requests
					// substract container resources from resources available in this NUMA node
					available.Sub(quantity)
					// zero container quantity
					quantity = resource.Quantity{}
					nRes[resName] = available
				}
			}
		}
	}
}

type filterFn func(pod *v1.Pod, zones topologyv1alpha1.ZoneList, nodeInfo *framework.NodeInfo) *framework.Status
type scoringFn func(*v1.Pod, topologyv1alpha1.ZoneList) (int64, *framework.Status)

type filterHandlersMap map[topologyv1alpha1.TopologyManagerPolicy]filterFn
type scoreHandlersMap map[topologyv1alpha1.TopologyManagerPolicy]scoringFn

func leastNUMAscoreHandlers() scoreHandlersMap {
	return scoreHandlersMap{
		topologyv1alpha1.SingleNUMANodePodLevel:       leastNUMAPodScopeScore,
		topologyv1alpha1.SingleNUMANodeContainerLevel: leastNUMAContainerScopeScore,
		topologyv1alpha1.BestEffortPodLevel:           leastNUMAPodScopeScore,
		topologyv1alpha1.BestEffortContainerLevel:     leastNUMAContainerScopeScore,
		topologyv1alpha1.RestrictedPodLevel:           leastNUMAPodScopeScore,
		topologyv1alpha1.RestrictedContainerLevel:     leastNUMAContainerScopeScore,
	}
}

// TopologyMatch plugin which run simplified version of TopologyManager's admit handler
type TopologyMatch struct {
	filterHandlers      filterHandlersMap
	scoringHandlers     scoreHandlersMap
	resourceToWeightMap resourceToWeightMap
	nrtCache            nrtcache.Interface
}

var _ framework.FilterPlugin = &TopologyMatch{}
var _ framework.ReservePlugin = &TopologyMatch{}
var _ framework.ScorePlugin = &TopologyMatch{}
var _ framework.EnqueueExtensions = &TopologyMatch{}

// Name returns name of the plugin. It is used in logs, etc.
func (tm *TopologyMatch) Name() string {
	return Name
}

// New initializes a new plugin and returns it.
func New(args runtime.Object, handle framework.Handle) (framework.Plugin, error) {
	klog.V(5).InfoS("Creating new TopologyMatch plugin")
	tcfg, ok := args.(*apiconfig.NodeResourceTopologyMatchArgs)
	if !ok {
		return nil, fmt.Errorf("want args to be of type NodeResourceTopologyMatchArgs, got %T", args)
	}

	nrtCache, err := initNodeTopologyInformer(tcfg, handle)
	if err != nil {
		klog.ErrorS(err, "Cannot create clientset for NodeTopologyResource", "kubeConfig", handle.KubeConfig())
		return nil, err
	}

	resToWeightMap := make(resourceToWeightMap)
	for _, resource := range tcfg.ScoringStrategy.Resources {
		resToWeightMap[v1.ResourceName(resource.Name)] = resource.Weight
	}

	var scoringHandlers scoreHandlersMap

	if tcfg.ScoringStrategy.Type == apiconfig.LeastNUMANodes {
		scoringHandlers = leastNUMAscoreHandlers()
	} else {
		strategy, err := getScoringStrategyFunction(tcfg.ScoringStrategy.Type)
		if err != nil {
			return nil, err
		}

		scoringHandlers = newScoringHandlers(strategy, resToWeightMap)
	}

	topologyMatch := &TopologyMatch{
		filterHandlers:      newFilterHandlers(),
		scoringHandlers:     scoringHandlers,
		resourceToWeightMap: resToWeightMap,
		nrtCache:            nrtCache,
	}

	return topologyMatch, nil
}

// EventsToRegister returns the possible events that may make a Pod
// failed by this plugin schedulable.
// NOTE: if in-place-update (KEP 1287) gets implemented, then PodUpdate event
// should be registered for this plugin since a Pod update may free up resources
// that make other Pods schedulable.
func (tm *TopologyMatch) EventsToRegister() []framework.ClusterEvent {
	// To register a custom event, follow the naming convention at:
	// https://git.k8s.io/kubernetes/pkg/scheduler/eventhandlers.go#L403-L410
	nrtGVK := fmt.Sprintf("noderesourcetopologies.v1alpha1.%v", topologyapi.GroupName)
	return []framework.ClusterEvent{
		{Resource: framework.Pod, ActionType: framework.Delete},
		{Resource: framework.Node, ActionType: framework.Add | framework.UpdateNodeAllocatable},
		{Resource: framework.GVK(nrtGVK), ActionType: framework.Add | framework.Update},
	}
}
