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
https://github.com/kubernetes/autoscaler/blob/cluster-autoscaler-release-1.1/cluster-autoscaler/cloudprovider/aws/aws_manager.go

Modifications Copyright (c) 2017 SAP SE or an SAP affiliate company. All rights reserved.
*/

package mcm

import (
	"fmt"
	"os"
	"strings"
	"time"

	machineapi "github.com/gardener/machine-controller-manager/pkg/client/clientset/versioned/typed/machine/v1alpha1"
	machineinformers "github.com/gardener/machine-controller-manager/pkg/client/informers/externalversions"
	machinelisters "github.com/gardener/machine-controller-manager/pkg/client/listers/machine/v1alpha1"
	corecontroller "github.com/gardener/machine-controller-manager/pkg/controller"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	"k8s.io/autoscaler/cluster-autoscaler/config/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/golang/glog"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	operationWaitTimeout    = 5 * time.Second
	operationPollInterval   = 100 * time.Millisecond
	maxRecordsReturnedByAPI = 100
)

//McmManager manages the client communication for MachineDeployments.
type McmManager struct {
	namespace               string
	interrupt               chan struct{}
	discoveryOpts           cloudprovider.NodeGroupDiscoveryOptions
	machineclient           machineapi.MachineV1alpha1Interface
	machineDeploymentLister machinelisters.MachineDeploymentLister
	machinelisters          machinelisters.MachineLister
}

func createMCMManagerInternal(discoveryOpts cloudprovider.NodeGroupDiscoveryOptions) (*McmManager, error) {

	//kubeconfig for the cluster for which machine-controller-manager will create machines.
	kubeconfig, err := clientcmd.BuildConfigFromFlags("", "")
	if err != nil {
		kubeconfigPath := os.Getenv("CONTROL_KUBECONFIG")
		kubeconfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			return nil, err
		}
	}

	clientBuilder := corecontroller.SimpleClientBuilder{
		ClientConfig: kubeconfig,
	}

	machineClient := clientBuilder.ClientOrDie("machine-controller-manager-client").MachineV1alpha1()

	namespace := os.Getenv("CONTROL_NAMESPACE")
	machineInformerFactory := machineinformers.NewFilteredSharedInformerFactory(
		clientBuilder.ClientOrDie("machine-shared-informers"),
		12*time.Hour,
		namespace,
		nil,
	)

	machineSharedInformers := machineInformerFactory.Machine().V1alpha1()

	manager := &McmManager{
		namespace:               namespace,
		interrupt:               make(chan struct{}),
		machineclient:           machineClient,
		machineDeploymentLister: machineSharedInformers.MachineDeployments().Lister(),
		machinelisters:          machineSharedInformers.Machines().Lister(),
		discoveryOpts:           discoveryOpts,
	}

	return manager, nil
}

//CreateMcmManager creates the McmManager
func CreateMcmManager(discoveryOpts cloudprovider.NodeGroupDiscoveryOptions) (*McmManager, error) {
	return createMCMManagerInternal(discoveryOpts)
}

//GetMachineDeploymentForMachine returns the MachineDeployment for the Machine object.
func (m *McmManager) GetMachineDeploymentForMachine(machine *Ref) (*MachineDeployment, error) {
	if machine.Name == "" {
		//Considering the possibility when Machine has been deleted but due to cached Node object it appears here.
		return nil, fmt.Errorf("Node does not Exists")
	}
	machStr := strings.Split(machine.Name, "-")
	machName := strings.Join(machStr[:len(machStr)-2], "-")
	mcmRef := Ref{
		Name:      machName,
		Namespace: machine.Namespace,
	}

	discoveryOpts := m.discoveryOpts
	specs := discoveryOpts.NodeGroupSpecs
	var min, max int
	for _, spec := range specs {
		s, err := dynamic.SpecFromString(spec, true)
		if err != nil {
			fmt.Errorf("Error occured while parsing the spec")
		}

		str := strings.Split(s.Name, ".")
		_, Name := str[0], str[1]

		if Name == machName {
			min = s.MinSize
			max = s.MaxSize
			break
		}
	}

	return &MachineDeployment{
		mcmRef,
		m,
		min,
		max,
	}, nil
}

//Cleanup does nothing at the moment.
//TODO: Enable cleanup menthod for graceful shutdown.
func (m *McmManager) Cleanup() {
	return
}

//GetMachineDeploymentSize returns the replicas field of the MachineDeployment
func (m *McmManager) GetMachineDeploymentSize(machinedeployment *MachineDeployment) (int64, error) {
	md, err := m.machineclient.MachineDeployments(machinedeployment.Namespace).Get(machinedeployment.Name, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("Unable to fetch MachineDeployment object %s %+v", machinedeployment.Name, err)
	}
	return int64(md.Spec.Replicas), nil
}

//SetMachineDeploymentSize sets the desired size for the Machinedeployment.
func (m *McmManager) SetMachineDeploymentSize(machinedeployment *MachineDeployment, size int64) error {

	md, err := m.machineclient.MachineDeployments(machinedeployment.Namespace).Get(machinedeployment.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Unable to fetch MachineDeployment object %s Error: %+v", machinedeployment.Name, err)
	}

	clone := md.DeepCopy()
	clone.Spec.Replicas = int32(size)

	_, err = m.machineclient.MachineDeployments(machinedeployment.Namespace).Update(clone)
	if err != nil {
		return fmt.Errorf("Unable to update MachineDeployment object %s Error: %+v", machinedeployment.Name, err)
	}

	return nil
}

//DeleteMachines deletes the Machines and also reduces the desired replicas of the Machinedeplyoment in parallel.
func (m *McmManager) DeleteMachines(machines []*Ref) error {
	if len(machines) == 0 {
		return nil
	}
	commonMachineDeployment, err := m.GetMachineDeploymentForMachine(machines[0])
	if err != nil {
		return err
	}

	for _, machine := range machines {
		machinedeployment, err := m.GetMachineDeploymentForMachine(machine)
		if err != nil {
			return err
		}
		if machinedeployment.Name != commonMachineDeployment.Name {
			return fmt.Errorf("Cannot delete machines which don't belong to the same MachineDeployment")
		}
	}

	for _, machine := range machines {

		mach, err := m.machineclient.Machines(machine.Namespace).Get(machine.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("Unable to fetch Machine object %s", machine.Name)
		}

		mclone := mach.DeepCopy()

		if mclone.Annotations != nil {
			mclone.Annotations["machinepriority.machine.sapcloud.io"] = "1" //TODO: avoid hardcoded string
		} else {
			mclone.Annotations = make(map[string]string)
			mclone.Annotations["machinepriority.machine.sapcloud.io"] = "1"
		}

		_, err = m.machineclient.Machines(machine.Namespace).Update(mclone)
		if err != nil {
			return fmt.Errorf("Unable to update Machine object %s", machine.Name)
		}
	}
	md, err := m.machineclient.MachineDeployments(commonMachineDeployment.Namespace).Get(commonMachineDeployment.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("Unable to fetch MachineDeployment object %s", commonMachineDeployment.Name)
	}

	mdclone := md.DeepCopy()

	if (int(mdclone.Spec.Replicas) - len(machines)) <= 0 {
		return fmt.Errorf("Unable to delete machine in MachineDeployment object %s , machine replicas are <= 0 ", commonMachineDeployment.Name)
	}
	mdclone.Spec.Replicas = mdclone.Spec.Replicas - int32(len(machines))

	_, err = m.machineclient.MachineDeployments(mdclone.Namespace).Update(mdclone)
	if err != nil {
		return fmt.Errorf("Unable to update MachineDeployment object %s", md.Name)
	}

	glog.V(2).Infof("MachineDeployment %s size decreased to %q", commonMachineDeployment.Name, mdclone.Spec.Replicas)

	return nil
}

//GetMachineDeploymentNodes returns the set of Nodes which belongs to the MachineDeployment.
func (m *McmManager) GetMachineDeploymentNodes(machinedeployment *MachineDeployment) ([]string, error) {
	md, err := m.machineclient.MachineDeployments(m.namespace).Get(machinedeployment.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch MachineDeployment object %s, Error: %v", machinedeployment.Name, err)
	}
	//machinelist, err := m.machinelisters.Machines(m.namespace).List(labels.Everything())
	machinelist, err := m.machineclient.Machines(m.namespace).List(metav1.ListOptions{})

	if err != nil {
		return nil, fmt.Errorf("Unable to fetch list of Machine objects %v", err)
	}

	var nodes []string
	for _, machine := range machinelist.Items {
		if strings.Contains(machine.Name, md.Name) {
			nodes = append(nodes, machine.Labels["node"])
		}
	}

	return nodes, nil
}
