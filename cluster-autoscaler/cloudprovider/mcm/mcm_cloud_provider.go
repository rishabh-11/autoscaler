/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

This file was copied and modified from the kubernetes/autoscaler project
https://github.com/kubernetes/autoscaler/blob/cluster-autoscaler-release-1.1/cluster-autoscaler/cloudprovider/aws/aws_cloud_provider.go

Modifications Copyright (c) 2017 SAP SE or an SAP affiliate company. All rights reserved.
*/

package mcm

import (
	"context"
	"fmt"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config"
	"k8s.io/autoscaler/cluster-autoscaler/utils/errors"
	"k8s.io/klog/v2"
	schedulerframework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// ProviderName is the cloud provider name for MCM
	ProviderName = "mcm"

	// GPULabel is the label added to nodes with GPU resource.
	// TODO: Align on a GPU Label for Gardener.
	GPULabel = "gardener.cloud/accelerator"

	// ScaleDownUtilizationThresholdAnnotation is the annotation key for the value of NodeGroupAutoscalingOptions.ScaleDownUtilizationThreshold
	ScaleDownUtilizationThresholdAnnotation = "autoscaler.gardener.cloud/scale-down-utilization-threshold"

	// ScaleDownGpuUtilizationThresholdAnnotation is the annotation key for the value of NodeGroupAutoscalingOptions.ScaleDownGpuUtilizationThreshold
	ScaleDownGpuUtilizationThresholdAnnotation = "autoscaler.gardener.cloud/scale-down-gpu-utilization-threshold"

	// ScaleDownUnneededTimeAnnotation is the annotation key for the value of NodeGroupAutoscalingOptions.ScaleDownUnneededTime
	ScaleDownUnneededTimeAnnotation = "autoscaler.gardener.cloud/scale-down-unneeded-time"

	// ScaleDownUnreadyTimeAnnotation is the annotation key for the value of NodeGroupAutoscalingOptions.ScaleDownUnreadyTime
	ScaleDownUnreadyTimeAnnotation = "autoscaler.gardener.cloud/scale-down-unready-time"

	// MaxNodeProvisionTimeAnnotation is the annotation key for the value of NodeGroupAutoscalingOptions.MaxNodeProvisionTime
	MaxNodeProvisionTimeAnnotation = "autoscaler.gardener.cloud/max-node-provision-time"
)

// MCMCloudProvider implements the cloud provider interface for machine-controller-manager
// Reference: https://github.com/gardener/machine-controller-manager
type mcmCloudProvider struct {
	mcmManager      *McmManager
	resourceLimiter *cloudprovider.ResourceLimiter
}

// BuildMcmCloudProvider builds CloudProvider implementation for machine-controller-manager.
func BuildMcmCloudProvider(mcmManager *McmManager, resourceLimiter *cloudprovider.ResourceLimiter) (cloudprovider.CloudProvider, error) {
	if mcmManager.discoveryOpts.StaticDiscoverySpecified() {
		return buildStaticallyDiscoveringProvider(mcmManager, resourceLimiter)
	}
	return nil, fmt.Errorf("Failed to build an mcm cloud provider: Either node group specs or node group auto discovery spec must be specified")
}

// BuildMCM builds the MCM provider and MCMmanager.
func BuildMCM(opts config.AutoscalingOptions, do cloudprovider.NodeGroupDiscoveryOptions, rl *cloudprovider.ResourceLimiter) cloudprovider.CloudProvider {
	var mcmManager *McmManager
	var err error
	mcmManager, err = CreateMcmManager(do)

	if err != nil {
		klog.Fatalf("Failed to create MCM Manager: %v", err)
	}
	provider, err := BuildMcmCloudProvider(mcmManager, rl)
	if err != nil {
		klog.Fatalf("Failed to create MCM cloud provider: %v", err)
	}
	return provider
}

func buildStaticallyDiscoveringProvider(mcmManager *McmManager, resourceLimiter *cloudprovider.ResourceLimiter) (*mcmCloudProvider, error) {
	mcm := &mcmCloudProvider{
		mcmManager:      mcmManager,
		resourceLimiter: resourceLimiter,
	}
	return mcm, nil
}

// Cleanup stops the go routine that is handling the current view of the MachineDeployment in the form of a cache
func (mcm *mcmCloudProvider) Cleanup() error {
	mcm.mcmManager.Cleanup()
	return nil
}

func (mcm *mcmCloudProvider) Name() string {
	return "machine-controller-manager"
}

// NodeGroups returns all node groups configured for this cloud provider.
func (mcm *mcmCloudProvider) NodeGroups() []cloudprovider.NodeGroup {
	result := make([]cloudprovider.NodeGroup, 0, len(mcm.mcmManager.machineDeployments))
	for _, machinedeployment := range mcm.mcmManager.machineDeployments {
		if machinedeployment.maxSize == 0 {
			continue
		}
		result = append(result, machinedeployment)
	}
	return result
}

// NodeGroupForNode returns the node group for the given node.
func (mcm *mcmCloudProvider) NodeGroupForNode(node *apiv1.Node) (cloudprovider.NodeGroup, error) {
	if len(node.Spec.ProviderID) == 0 {
		klog.Warningf("Node %v has no providerId", node.Name)
		return nil, nil
	}

	ref, err := ReferenceFromProviderID(mcm.mcmManager, node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}

	if ref == nil {
		klog.V(4).Infof("Skipped node %v, it's either been removed or it's not managed by this controller", node.Spec.ProviderID)
		return nil, nil
	}

	md, err := mcm.mcmManager.GetMachineDeploymentForMachine(ref)
	if err != nil {
		return nil, err
	}

	key := types.NamespacedName{Namespace: md.Namespace, Name: md.Name}
	_, isManaged := mcm.mcmManager.machineDeployments[key]
	if !isManaged {
		klog.V(4).Infof("Skipped node %v, it's not managed by this controller", node.Spec.ProviderID)
		return nil, nil
	}

	return md, nil
}

// HasInstance returns whether a given node has a corresponding instance in this cloud provider
func (mcm *mcmCloudProvider) HasInstance(*apiv1.Node) (bool, error) {
	return true, cloudprovider.ErrNotImplemented
}

// Pricing returns pricing model for this cloud provider or error if not available.
func (mcm *mcmCloudProvider) Pricing() (cloudprovider.PricingModel, errors.AutoscalerError) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetAvailableMachineTypes get all machine types that can be requested from the cloud provider.
func (mcm *mcmCloudProvider) GetAvailableMachineTypes() ([]string, error) {
	return []string{}, nil
}

// NewNodeGroup builds a theoretical node group based on the node definition provided. The node group is not automatically
// created on the cloud provider side. The node group is not returned by NodeGroups() until it is created.
func (mcm *mcmCloudProvider) NewNodeGroup(machineType string, labels map[string]string, systemLabels map[string]string,
	taints []apiv1.Taint, extraResources map[string]resource.Quantity) (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrNotImplemented
}

// GetResourceLimiter returns struct containing limits (max, min) for resources (cores, memory etc.).
func (mcm *mcmCloudProvider) GetResourceLimiter() (*cloudprovider.ResourceLimiter, error) {
	return mcm.resourceLimiter, nil
}

func (mcm *mcmCloudProvider) checkMCMAvailableReplicas() error {
	namespace := mcm.mcmManager.namespace
	deployment, err := mcm.mcmManager.deploymentLister.Deployments(namespace).Get("machine-controller-manager")
	if err != nil {
		return fmt.Errorf("failed to get machine-controller-manager deployment: %v", err.Error())
	}

	if deployment.Status.AvailableReplicas == 0 {
		return fmt.Errorf("machine-controller-manager is offline. Cluster autoscaler operations would be suspended.")
	}

	return nil
}

// Refresh is called before every main loop and can be used to dynamically update cloud provider state.
// In particular the list of node groups returned by NodeGroups can change as a result of CloudProvider.Refresh().
func (mcm *mcmCloudProvider) Refresh() error {
	err := mcm.checkMCMAvailableReplicas()
	if err != nil {
		return err
	}
	return mcm.mcmManager.Refresh()
}

// GPULabel returns the label added to nodes with GPU resource.
func (mcm *mcmCloudProvider) GPULabel() string {
	return GPULabel
}

// GetAvailableGPUTypes return all available GPU types cloud provider supports
func (mcm *mcmCloudProvider) GetAvailableGPUTypes() map[string]struct{} {
	return nil
}

// GetNodeGpuConfig returns the label, type and resource name for the GPU added to node. If node doesn't have
// any GPUs, it returns nil.
func (mcm *mcmCloudProvider) GetNodeGpuConfig(*apiv1.Node) *cloudprovider.GpuConfig {
	return nil
}

// ReferenceFromProviderID extracts the Ref from providerId. It returns corresponding machine-name to providerid.
func ReferenceFromProviderID(m *McmManager, id string) (*types.NamespacedName, error) {
	machines, err := m.machineLister.Machines(m.namespace).List(labels.Everything())
	if err != nil {
		return nil, fmt.Errorf("Could not list machines due to error: %s", err)
	}

	var Name, Namespace string
	for _, machine := range machines {
		machineID := strings.Split(machine.Spec.ProviderID, "/")
		nodeID := strings.Split(id, "/")
		// If registered, the ID will match the cloudprovider instance ID.
		// If unregistered, the ID will match the machine name.
		if machineID[len(machineID)-1] == nodeID[len(nodeID)-1] ||
			nodeID[len(nodeID)-1] == machine.Name {

			Name = machine.Name
			Namespace = machine.Namespace
			break
		}
	}

	if Name == "" {
		// Could not find any machine corresponds to node %+v", id
		klog.V(4).Infof("No machine found for node ID %q", id)
		return nil, nil
	}
	return &types.NamespacedName{
		Name:      Name,
		Namespace: Namespace,
	}, nil
}

// MachineDeployment implements NodeGroup interface.
type MachineDeployment struct {
	types.NamespacedName

	mcmManager *McmManager

	scalingMutex sync.Mutex
	minSize      int
	maxSize      int
}

// MaxSize returns maximum size of the node group.
func (machineDeployment *MachineDeployment) MaxSize() int {
	return machineDeployment.maxSize
}

// MinSize returns minimum size of the node group.
func (machineDeployment *MachineDeployment) MinSize() int {
	return machineDeployment.minSize
}

// TargetSize returns the current TARGET size of the node group. It is possible that the
// number is different from the number of nodes registered in Kubernetes.
func (machineDeployment *MachineDeployment) TargetSize() (int, error) {
	size, err := machineDeployment.mcmManager.GetMachineDeploymentSize(machineDeployment)
	return int(size), err
}

// Exist checks if the node group really exists on the cloud provider side. Allows to tell the
// theoretical node group from the real one.
// TODO: Implement this to check if machine-deployment really exists.
func (machineDeployment *MachineDeployment) Exist() bool {
	return true
}

// Create creates the node group on the cloud provider side.
func (machineDeployment *MachineDeployment) Create() (cloudprovider.NodeGroup, error) {
	return nil, cloudprovider.ErrAlreadyExist
}

// Autoprovisioned returns true if the node group is autoprovisioned.
func (machineDeployment *MachineDeployment) Autoprovisioned() bool {
	return false
}

// Delete deletes the node group on the cloud provider side.
// This will be executed only for autoprovisioned node groups, once their size drops to 0.
func (machineDeployment *MachineDeployment) Delete() error {
	return cloudprovider.ErrNotImplemented
}

// IncreaseSize of the Machinedeployment.
func (machineDeployment *MachineDeployment) IncreaseSize(delta int) error {
	klog.V(0).Infof("Received request to increase size of machine deployment %s by %d", machineDeployment.Name, delta)
	if delta <= 0 {
		return fmt.Errorf("size increase must be positive")
	}
	machineDeployment.scalingMutex.Lock()
	defer machineDeployment.scalingMutex.Unlock()
	size, err := machineDeployment.mcmManager.GetMachineDeploymentSize(machineDeployment)
	if err != nil {
		return err
	}
	targetSize := int(size) + delta
	if targetSize > machineDeployment.MaxSize() {
		return fmt.Errorf("size increase too large - desired:%d max:%d", targetSize, machineDeployment.MaxSize())
	}
	return machineDeployment.mcmManager.retry(func(ctx context.Context) (bool, error) {
		return machineDeployment.mcmManager.SetMachineDeploymentSize(ctx, machineDeployment, int64(targetSize))
	}, "MachineDeployment", "update", machineDeployment.Name)
}

// DecreaseTargetSize decreases the target size of the node group. This function
// doesn't permit to delete any existing node and can be used only to reduce the
// request for new nodes that have not been yet fulfilled. Delta should be negative.
// It is assumed that cloud provider will not delete the existing nodes if the size
// when there is an option to just decrease the target.
func (machineDeployment *MachineDeployment) DecreaseTargetSize(delta int) error {
	klog.V(0).Infof("Received request to decrease target size of machine deployment %s by %d", machineDeployment.Name, delta)
	if delta >= 0 {
		return fmt.Errorf("size decrease size must be negative")
	}
	machineDeployment.scalingMutex.Lock()
	defer machineDeployment.scalingMutex.Unlock()
	size, err := machineDeployment.mcmManager.GetMachineDeploymentSize(machineDeployment)
	if err != nil {
		return err
	}
	decreaseAmount := int(size) + delta
	if decreaseAmount < machineDeployment.minSize {
		klog.Warningf("Cannot go below min size= %d for machineDeployment %s, requested target size= %d . Setting target size to min size", machineDeployment.minSize, machineDeployment.Name, size+int64(delta))
		decreaseAmount = machineDeployment.minSize
	}
	return machineDeployment.mcmManager.retry(func(ctx context.Context) (bool, error) {
		return machineDeployment.mcmManager.SetMachineDeploymentSize(ctx, machineDeployment, int64(decreaseAmount))
	}, "MachineDeployment", "update", machineDeployment.Name)
}

// Refresh resets the priority annotation for the machines that are not present in machines-marked-by-ca-for-deletion annotation on the machineDeployment
func (machineDeployment *MachineDeployment) Refresh() error {
	machineDeployment.scalingMutex.Lock()
	defer machineDeployment.scalingMutex.Unlock()
	mcd, err := machineDeployment.mcmManager.GetMachineDeploymentResource(machineDeployment.Name)
	if err != nil {
		return err
	}
	markedMachineNames := getMachineNamesMarkedByCAForDeletion(mcd)
	machines, err := machineDeployment.mcmManager.getMachinesForMachineDeployment(machineDeployment.Name)
	if err != nil {
		klog.Errorf("[Refresh] failed to get machines for machine deployment %s, hence skipping it. Err: %v", machineDeployment.Name, err.Error())
		return err
	}
	// update the machines-marked-by-ca-for-deletion annotation with the machines that are still marked for deletion by CA.
	// This is done to ensure that the machines that are no longer present are removed from the annotation.
	var updatedMarkedMachineNames []string
	for _, machineName := range markedMachineNames {
		if slices.ContainsFunc(machines, func(mc *v1alpha1.Machine) bool {
			return mc.Name == machineName
		}) {
			updatedMarkedMachineNames = append(updatedMarkedMachineNames, machineName)
		}
	}
	clone := mcd.DeepCopy()
	if clone.Annotations == nil {
		clone.Annotations = map[string]string{}
	}
	updatedMachinesMarkedByCAForDeletionAnnotationVal := createMachinesMarkedForDeletionAnnotationValue(updatedMarkedMachineNames)
	if clone.Annotations[machinesMarkedByCAForDeletion] != updatedMachinesMarkedByCAForDeletionAnnotationVal {
		clone.Annotations[machinesMarkedByCAForDeletion] = updatedMachinesMarkedByCAForDeletionAnnotationVal
		ctx, cancelFn := context.WithTimeout(context.Background(), machineDeployment.mcmManager.maxRetryTimeout)
		defer cancelFn()
		_, err = machineDeployment.mcmManager.machineClient.MachineDeployments(machineDeployment.Namespace).Update(ctx, clone, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}
	// reset the priority for the machines that are not present in machines-marked-by-ca-for-deletion annotation
	var incorrectlyMarkedMachines []string
	for _, machine := range machines {
		// no need to reset priority for machines already in termination or failed phase
		if isMachineFailedOrTerminating(machine) {
			continue
		}
		// check if the machine is marked for deletion by CA but not present in machines-marked-by-ca-for-deletion annotation. This means that CA was not able to reduce the replicas
		// corresponding to this machine and hence the machine should not be marked for deletion.
		if annotValue, ok := machine.Annotations[machinePriorityAnnotation]; ok && annotValue == priorityValueForDeletionCandidateMachines && !slices.Contains(markedMachineNames, machine.Name) {
			incorrectlyMarkedMachines = append(incorrectlyMarkedMachines, machine.Name)
		}
	}
	return machineDeployment.mcmManager.resetPriorityForMachines(incorrectlyMarkedMachines)
}

// Belongs returns true if the given node belongs to the NodeGroup.
// TODO: Implement this to iterate over machines under machinedeployment, and return true if node exists in list.
func (machineDeployment *MachineDeployment) Belongs(node *apiv1.Node) (bool, error) {
	ref, err := ReferenceFromProviderID(machineDeployment.mcmManager, node.Spec.ProviderID)
	if err != nil {
		return false, err
	}
	targetMd, err := machineDeployment.mcmManager.GetMachineDeploymentForMachine(ref)
	if err != nil {
		return false, err
	}
	if targetMd == nil {
		return false, fmt.Errorf("%s doesn't belong to a known MachinDeployment", node.Name)
	}
	if targetMd.Id() != machineDeployment.Id() {
		return false, nil
	}
	return true, nil
}

// DeleteNodes deletes the nodes from the group. It is expected that this method will not be called
// for nodes which are not part of ANY machine deployment.
func (machineDeployment *MachineDeployment) DeleteNodes(nodes []*apiv1.Node) error {
	nodeNames := getNodeNames(nodes)
	klog.V(0).Infof("Received request to delete nodes:- %v", nodeNames)
	size, err := machineDeployment.mcmManager.GetMachineDeploymentSize(machineDeployment)
	if err != nil {
		return err
	}
	if int(size) <= machineDeployment.MinSize() {
		return fmt.Errorf("min size reached, nodes will not be deleted")
	}
	machines := make([]*types.NamespacedName, 0, len(nodes))
	for _, node := range nodes {
		belongs, err := machineDeployment.Belongs(node)
		if err != nil {
			return err
		} else if !belongs {
			return fmt.Errorf("%s belongs to a different machinedeployment than %s", node.Name, machineDeployment.Id())
		}
		ref, err := ReferenceFromProviderID(machineDeployment.mcmManager, node.Spec.ProviderID)
		if err != nil {
			return fmt.Errorf("couldn't find the machine-name from provider-id %s", node.Spec.ProviderID)
		}
		machines = append(machines, ref)
	}
	return machineDeployment.mcmManager.DeleteMachines(machines)
}

func getNodeNames(nodes []*apiv1.Node) interface{} {
	nodeNames := make([]string, 0, len(nodes))
	for _, node := range nodes {
		nodeNames = append(nodeNames, node.Name)
	}
	return nodeNames
}

// Id returns machinedeployment id.
func (machineDeployment *MachineDeployment) Id() string {
	return machineDeployment.Name
}

// Debug returns a debug string for the Asg.
func (machineDeployment *MachineDeployment) Debug() string {
	return fmt.Sprintf("%s (%d:%d)", machineDeployment.Id(), machineDeployment.MinSize(), machineDeployment.MaxSize())
}

// Nodes returns a list of all nodes that belong to this node group.
func (machineDeployment *MachineDeployment) Nodes() ([]cloudprovider.Instance, error) {
	instances, err := machineDeployment.mcmManager.GetInstancesForMachineDeployment(machineDeployment)
	if err != nil {
		return nil, fmt.Errorf("failed to get the cloudprovider.Instance for machines backed by the machinedeployment %q, error: %v", machineDeployment.Name, err)
	}
	erroneousInstanceInfos := make([]string, 0, len(instances))
	for _, instance := range instances {
		if instance.Status != nil && instance.Status.ErrorInfo != nil {
			erroneousInstanceInfos = append(erroneousInstanceInfos, fmt.Sprintf("[Instance: %s, ErrorClass: %s, ErrorCode: %s]", instance.Id, instance.Status.ErrorInfo.ErrorClass.String(), instance.Status.ErrorInfo.ErrorCode))
		}
	}
	if len(erroneousInstanceInfos) != 0 {
		klog.V(0).Infof("Found erroneous instances:- %v", erroneousInstanceInfos)
	}
	return instances, nil
}

// GetOptions returns NodeGroupAutoscalingOptions that should be used for this particular
// NodeGroup. Returning a nil will result in using default options.
// Implementation optional.
func (machineDeployment *MachineDeployment) GetOptions(defaults config.NodeGroupAutoscalingOptions) (*config.NodeGroupAutoscalingOptions, error) {
	options := defaults
	mcdAnnotations, err := machineDeployment.mcmManager.GetMachineDeploymentAnnotations(machineDeployment.Name)
	if err != nil {
		return nil, err
	}

	if _, ok := mcdAnnotations[ScaleDownUtilizationThresholdAnnotation]; ok {
		if floatVal, err := strconv.ParseFloat(mcdAnnotations[ScaleDownUtilizationThresholdAnnotation], 64); err == nil {
			options.ScaleDownUtilizationThreshold = floatVal
		}
	}
	if _, ok := mcdAnnotations[ScaleDownGpuUtilizationThresholdAnnotation]; ok {
		if floatVal, err := strconv.ParseFloat(mcdAnnotations[ScaleDownGpuUtilizationThresholdAnnotation], 64); err == nil {
			options.ScaleDownGpuUtilizationThreshold = floatVal
		}
	}
	if _, ok := mcdAnnotations[ScaleDownUnneededTimeAnnotation]; ok {
		if durationVal, err := time.ParseDuration(mcdAnnotations[ScaleDownUnneededTimeAnnotation]); err == nil {
			options.ScaleDownUnneededTime = durationVal
		}
	}
	if _, ok := mcdAnnotations[ScaleDownUnreadyTimeAnnotation]; ok {
		if durationVal, err := time.ParseDuration(mcdAnnotations[ScaleDownUnreadyTimeAnnotation]); err == nil {
			options.ScaleDownUnreadyTime = durationVal
		}
	}
	if _, ok := mcdAnnotations[MaxNodeProvisionTimeAnnotation]; ok {
		if durationVal, err := time.ParseDuration(mcdAnnotations[MaxNodeProvisionTimeAnnotation]); err == nil {
			options.MaxNodeProvisionTime = durationVal
		}
	}
	return &options, nil
}

// TemplateNodeInfo returns a node template for this node group.
func (machineDeployment *MachineDeployment) TemplateNodeInfo() (*schedulerframework.NodeInfo, error) {

	nodeTemplate, err := machineDeployment.mcmManager.GetMachineDeploymentNodeTemplate(machineDeployment)
	if err != nil {
		return nil, err
	}

	node, err := machineDeployment.mcmManager.buildNodeFromTemplate(machineDeployment.Name, nodeTemplate)
	if err != nil {
		return nil, err
	}

	nodeInfo := schedulerframework.NewNodeInfo(cloudprovider.BuildKubeProxy(machineDeployment.Name))
	nodeInfo.SetNode(node)
	return nodeInfo, nil
}

// AtomicIncreaseSize is not implemented.
func (machineDeployment *MachineDeployment) AtomicIncreaseSize(delta int) error {
	return cloudprovider.ErrNotImplemented
}

// getMachineNamesMarkedByCAForDeletion returns the set of machine names marked by CA for deletion.
func getMachineNamesMarkedByCAForDeletion(mcd *v1alpha1.MachineDeployment) []string {
	if mcd.Annotations == nil || mcd.Annotations[machinesMarkedByCAForDeletion] == "" {
		return make([]string, 0)
	}
	return strings.Split(mcd.Annotations[machinesMarkedByCAForDeletion], ",")
}
