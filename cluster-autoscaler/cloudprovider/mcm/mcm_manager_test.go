/*
Copyright 2017 The Kubernetes Authors.

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

package mcm

import (
	"errors"
	"github.com/gardener/machine-controller-manager/pkg/apis/machine/v1alpha1"
	"testing"

	"github.com/stretchr/testify/assert"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/autoscaler/cluster-autoscaler/cloudprovider"
	kubeletapis "k8s.io/kubelet/pkg/apis"
)

func TestBuildGenericLabels(t *testing.T) {
	var (
		instanceTypeC4Large = "c4.large"
		regionUSEast1       = "us-east-1"
		zoneUSEast1a        = "us-east-1a"
		testHostName        = "test-hostname"
	)

	labels := buildGenericLabels(&nodeTemplate{
		InstanceType: &instanceType{
			InstanceType:     instanceTypeC4Large,
			VCPU:             resource.MustParse("2"),
			Memory:           resource.MustParse("3840Mi"),
			EphemeralStorage: resource.MustParse("50378260Ki"),
		},
		Region: regionUSEast1,
		Zone:   zoneUSEast1a,
	}, testHostName)

	assert.Equal(t, cloudprovider.DefaultArch, labels[kubeletapis.LabelArch])
	assert.Equal(t, cloudprovider.DefaultArch, labels[apiv1.LabelArchStable])

	assert.Equal(t, cloudprovider.DefaultOS, labels[kubeletapis.LabelOS])
	assert.Equal(t, cloudprovider.DefaultOS, labels[apiv1.LabelOSStable])

	assert.Equal(t, instanceTypeC4Large, labels[apiv1.LabelInstanceType])
	assert.Equal(t, instanceTypeC4Large, labels[apiv1.LabelInstanceTypeStable])

	assert.Equal(t, regionUSEast1, labels[apiv1.LabelZoneRegion])
	assert.Equal(t, regionUSEast1, labels[apiv1.LabelZoneRegionStable])

	assert.Equal(t, zoneUSEast1a, labels[apiv1.LabelZoneFailureDomain])
	assert.Equal(t, zoneUSEast1a, labels[apiv1.LabelZoneFailureDomainStable])

	assert.Equal(t, testHostName, labels[apiv1.LabelHostname])
}

func TestGenerationOfCorrectZoneValueFromMCLabel(t *testing.T) {
	var (
		resultingZone string
		zoneA         = "zone-a"
		zoneB         = "zone-b"
		randomKey     = "random-key"
		randomValue   = "random-value"
	)

	// Basic test to get zone value
	resultingZone = getZoneValueFromMCLabels(map[string]string{
		apiv1.LabelZoneFailureDomainStable: zoneA,
	})
	assert.Equal(t, resultingZone, zoneA)

	// Prefer LabelZoneFailureDomainStable label over LabelZoneFailureDomain
	resultingZone = getZoneValueFromMCLabels(map[string]string{
		apiv1.LabelZoneFailureDomainStable: zoneA,
		apiv1.LabelZoneFailureDomain:       zoneB,
	})
	assert.Equal(t, resultingZone, zoneA)

	// Fallback to LabelZoneFailureDomain when LabelZoneFailureDomainStable is not found
	resultingZone = getZoneValueFromMCLabels(map[string]string{
		randomKey:                    randomValue,
		apiv1.LabelZoneFailureDomain: zoneB,
	})
	assert.Equal(t, resultingZone, zoneB)

	// When neither of the labels are found
	resultingZone = getZoneValueFromMCLabels(map[string]string{
		randomKey: randomValue,
	})
	assert.Equal(t, resultingZone, "")
}

func TestFilterNodes(t *testing.T) {
	var (
		node1 = &apiv1.Node{
			Status: apiv1.NodeStatus{
				Capacity: apiv1.ResourceList{
					"cpu":    resource.MustParse("2"),
					"memory": resource.MustParse("64Gi"),
				},
			},
		}
		node2 = &apiv1.Node{
			ObjectMeta: v1.ObjectMeta{
				Labels: map[string]string{
					apiv1.LabelInstanceTypeStable: "test-instance-type",
				},
			},
			Status: apiv1.NodeStatus{
				Capacity: apiv1.ResourceList{
					"cpu":    resource.MustParse("2"),
					"memory": resource.MustParse("64Gi"),
				},
			},
		}
		emptyNode = &apiv1.Node{}
	)
	filteredNodes := filterOutNodes([]*apiv1.Node{
		node1,
		node2,
		emptyNode,
	}, "test-instance-type")

	assert.EqualValues(t, len(filteredNodes), 1)
	assert.Equal(t, filteredNodes, []*apiv1.Node{node2})
}

func TestValidateNodeTemplate(t *testing.T) {
	awsM5Large := AWSInstanceTypes["m5.large"]
	m5Large := &instanceType{
		InstanceType: awsM5Large.InstanceType,
		VCPU:         awsM5Large.VCPU,
		Memory:       awsM5Large.Memory,
		GPU:          awsM5Large.GPU,
	}
	nt := v1alpha1.NodeTemplate{
		InstanceType: m5Large.InstanceType,
		Capacity:     make(apiv1.ResourceList),
	}
	err := validateNodeTemplate(&nt)
	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, ErrInvalidNodeTemplate))

	nt.Region = "europe-west1"
	nt.Zone = nt.Region + "-b"

	err = validateNodeTemplate(&nt)
	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, ErrInvalidNodeTemplate))

	if err != nil {
		t.Logf("error %s", err)
	}

	nt.Capacity[apiv1.ResourceEphemeralStorage] = resource.MustParse("20Gi")
	err = validateNodeTemplate(&nt)
	assert.Nil(t, err)
}
