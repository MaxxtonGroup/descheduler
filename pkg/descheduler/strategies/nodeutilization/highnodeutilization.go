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

package nodeutilization

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/descheduler/pkg/api"
	"sigs.k8s.io/descheduler/pkg/descheduler/evictions"
	nodeutil "sigs.k8s.io/descheduler/pkg/descheduler/node"
	podutil "sigs.k8s.io/descheduler/pkg/descheduler/pod"
	"sigs.k8s.io/descheduler/pkg/utils"
)

// HighNodeUtilization evicts pods from underutilized nodes so that scheduler can schedule according to its strategy.
// Note that CPU/Memory requests are used to calculate nodes' utilization and not the actual resource usage.
func HighNodeUtilization(ctx context.Context, client clientset.Interface, strategy api.DeschedulerStrategy, nodes []*v1.Node, podEvictor *evictions.PodEvictor, getPodsAssignedToNode podutil.GetPodsAssignedToNodeFunc) {

	// check if HighNodeUtilization is set correctly 
	if err := validateNodeUtilizationParams(strategy.Params); err != nil {
		klog.ErrorS(err, "Invalid HighNodeUtilization parameters")
		return
	}

	nodeFit := false
	if strategy.Params != nil {
		nodeFit = strategy.Params.NodeFit
	}

	// get threshold priority
	thresholdPriority, err := utils.GetPriorityFromStrategyParams(ctx, client, strategy.Params)
	if err != nil {
		klog.ErrorS(err, "Failed to get threshold priority from strategy's params")
		return
	}

	// validate if the strategy config is valid based on threshold and target treshold set
	thresholds := strategy.Params.NodeResourceUtilizationThresholds.Thresholds
	targetThresholds := strategy.Params.NodeResourceUtilizationThresholds.TargetThresholds
	if err := validateHighUtilizationStrategyConfig(thresholds, targetThresholds); err != nil {
		klog.ErrorS(err, "HighNodeUtilization config is not valid")
		return
	}
	targetThresholds = make(api.ResourceThresholds)

	setDefaultForThresholds(thresholds, targetThresholds)
	resourceNames := getResourceNames(targetThresholds)
	totalNodes := len(nodes)
	for i := 0; i < totalNodes; i++ {
		currentNode := nodes[i]

		// classifies the nodes into sourceNodes and highNode groups
		sourceNodes, highNodes := classifyNodes(
			getNodeUsage(nodes, resourceNames, getPodsAssignedToNode),
			getNodeThresholds(nodes, thresholds, targetThresholds, resourceNames, getPodsAssignedToNode, false),
			func(node *v1.Node, usage NodeUsage, threshold NodeThresholds) bool {
				return isNodeWithLowUtilization(usage, threshold.lowResourceThreshold)
			},
			func(node *v1.Node, usage NodeUsage, threshold NodeThresholds) bool {
				if nodeutil.IsNodeUnschedulable(node) {
					klog.V(2).InfoS("Node is unschedulable", "node", klog.KObj(node))
					return false
				}
				return !isNodeWithLowUtilization(usage, threshold.lowResourceThreshold)
			})


		// log message in one line
		keysAndValues := []interface{}{
			"CPU", thresholds[v1.ResourceCPU],
			"Mem", thresholds[v1.ResourceMemory],
			"Pods", thresholds[v1.ResourcePods],
		}
		for name := range thresholds {
			if !isBasicResource(name) {
				keysAndValues = append(keysAndValues, string(name), int64(thresholds[name]))
			}
		}

		klog.V(1).InfoS("Criteria for a node below target utilization", keysAndValues...)
		klog.V(1).InfoS("Number of underutilized nodes", "totalNumber", len(sourceNodes))

		// checks to determinate if descheduling should be conducted
		// 1. checking if one or more node is underutilized
		if len(sourceNodes) == 0 {
			klog.V(1).InfoS("No node is underutilized, nothing to do here, you might tune your thresholds further")
			return
		}
		// 2. check if number of underutilized nodes is less or equal to the minimum underutilized nodes set thrugh the NumberOfNodes paramter in the configmap
		if len(sourceNodes) <= strategy.Params.NodeResourceUtilizationThresholds.NumberOfNodes {
			klog.V(1).InfoS("Number of nodes underutilized is less or equal than NumberOfNodes, nothing to do here", "underutilizedNodes", len(sourceNodes), "numberOfNodes", strategy.Params.NodeResourceUtilizationThresholds.NumberOfNodes)
			return
		}
		// 3. check if all nodes are underutilized
		if len(sourceNodes) == len(nodes) {
			klog.V(1).InfoS("All nodes are underutilized, nothing to do here")
			return
		}
		// 4. check if overutilized nodes are equal to null 
		if len(highNodes) == 0 {
			klog.V(1).InfoS("No node is available to schedule the pods, nothing to do here")
			return
		}
		
		// check if pods are evictable and can also fit the nodes
		evictable := podEvictor.Evictable(evictions.WithPriorityThreshold(thresholdPriority), evictions.WithNodeFit(nodeFit))

		// stop if the total available usage has dropped to zero, then no more pods can be scheduled
		continueEvictionCond := func(nodeInfo NodeInfo, totalAvailableUsage map[v1.ResourceName]*resource.Quantity) bool {
			for name := range totalAvailableUsage {
				if totalAvailableUsage[name].CmpInt64(0) < 1 {
					return false
				}
			}

			return true
		}

		// sort the nodes by the usage in ascending order
		sortNodesByUsage(sourceNodes, true) 

		// check if the currentNode we are on in the range is present in the sourceNode list
		sliceResult, err := slice(sourceNodes, currentNode)
		if (err == nil) {
			
			// cordon nodes
			if strategy.Params != nil && strategy.Params.Cordon {
				// for _, node := range sourceNodes {
				err = cordonNode(ctx, client, currentNode)

				if err != nil {
					klog.ErrorS(err, "Failed to cordon node", "node", klog.KObj(currentNode))
				}
				// }
			}

			// start eviction of pods fom the nodes  
			evictPodsFromSourceNodes(
				ctx,
				[]NodeInfo{sliceResult},
				highNodes,
				podEvictor,
				evictable.IsEvictable,
				resourceNames,
				"HighNodeUtilization",
				continueEvictionCond)

			time.Sleep(120 * time.Second)
			klog.V(1).InfoS("Sleeping for two minutes before continuing on the next node")
		}
	}
}

func slice(s []NodeInfo, n *v1.Node) (nodeInfo NodeInfo, err error) {
	for _, v := range s {
		if v.node.Name == n.Name {
			return v, nil
		}
	}
	return NodeInfo{}, fmt.Errorf("Node not found")
}

func cordonNode(ctx context.Context, client clientset.Interface, node *v1.Node) error {
	klog.InfoS("Cordon node", "node", klog.KObj(node))

	patch := struct {
		Spec struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
	}{}
	patch.Spec.Unschedulable = true

	patchJson, _ := json.Marshal(patch)
	options := metav1.PatchOptions{}
	_, error := client.CoreV1().Nodes().Patch(ctx, node.Name, types.StrategicMergePatchType, patchJson, options)
	return error
}

func validateHighUtilizationStrategyConfig(thresholds, targetThresholds api.ResourceThresholds) error {
	if targetThresholds != nil {
		return fmt.Errorf("targetThresholds is not applicable for HighNodeUtilization")
	}
	if err := validateThresholds(thresholds); err != nil {
		return fmt.Errorf("thresholds config is not valid: %v", err)
	}
	return nil
}

func setDefaultForThresholds(thresholds, targetThresholds api.ResourceThresholds) {
	// check if Pods/CPU/Mem are set, if not, set them to 100
	if _, ok := thresholds[v1.ResourcePods]; !ok {
		thresholds[v1.ResourcePods] = MaxResourcePercentage
	}
	if _, ok := thresholds[v1.ResourceCPU]; !ok {
		thresholds[v1.ResourceCPU] = MaxResourcePercentage
	}
	if _, ok := thresholds[v1.ResourceMemory]; !ok {
		thresholds[v1.ResourceMemory] = MaxResourcePercentage
	}

	// Default targetThreshold resource values to 100
	targetThresholds[v1.ResourcePods] = MaxResourcePercentage
	targetThresholds[v1.ResourceCPU] = MaxResourcePercentage
	targetThresholds[v1.ResourceMemory] = MaxResourcePercentage

	for name := range thresholds {
		if !isBasicResource(name) {
			targetThresholds[name] = MaxResourcePercentage
		}
	}
}
