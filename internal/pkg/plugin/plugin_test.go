/**
 * Copyright 2018 Advanced Micro Devices, Inc.  All rights reserved.
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
**/

package plugin

import (
	"reflect"
	"testing"

	"github.com/ROCm/k8s-device-plugin/internal/pkg/allocator"
	"golang.org/x/net/context"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestCountGPUDevFromTopology(t *testing.T) {
	count := countGPUDevFromTopology("../../../testdata/topology-parsing")

	expCount := 2
	if count != expCount {
		t.Errorf("Count was incorrect, got: %d, want: %d.", count, expCount)
	}
}

func TestBuildLogicalAMDGPUsFromPhysical(t *testing.T) {
	logical := buildLogicalAMDGPUsFromPhysical(testPhysicalAMDGPUs(), 3)

	if len(logical) != 6 {
		t.Fatalf("expected 6 logical devices, got %d", len(logical))
	}

	for _, id := range []string{"gpu0-0", "gpu0-1", "gpu0-2", "gpu1-0", "gpu1-1", "gpu1-2"} {
		if _, ok := logical[id]; !ok {
			t.Fatalf("expected logical device %s", id)
		}
	}
	if physicalIDForDevice("gpu0-2", logical["gpu0-2"]) != "gpu0" {
		t.Fatalf("expected gpu0-2 to map back to gpu0")
	}
	if replicaIndexForDevice(logical["gpu1-2"]) != 2 {
		t.Fatalf("expected gpu1-2 to keep replica index 2")
	}
}

func TestReplicaPreferredAllocationSpreadsAcrossPhysicalGPUs(t *testing.T) {
	p := testReplicaPlugin(4)
	available := []string{"gpu0-0", "gpu0-1", "gpu0-2", "gpu0-3", "gpu1-0", "gpu1-1", "gpu1-2", "gpu1-3"}

	got, err := p.getReplicaPreferredAllocation(available, nil, 2)
	if err != nil {
		t.Fatalf("getReplicaPreferredAllocation failed: %v", err)
	}

	wantPhysicalIDs := []string{"gpu0", "gpu1"}
	physicalIDs := gotPhysicalIDs(got, p.AMDGPUs)
	if !reflect.DeepEqual(physicalIDs, wantPhysicalIDs) {
		t.Fatalf("expected allocation across %v, got IDs %v across %v", wantPhysicalIDs, got, physicalIDs)
	}
}

func TestReplicaPreferredAllocationHonorsRequiredAndSpreads(t *testing.T) {
	p := testReplicaPlugin(4)
	available := []string{"gpu0-0", "gpu0-1", "gpu0-2", "gpu0-3", "gpu1-0", "gpu1-1", "gpu1-2", "gpu1-3"}
	required := []string{"gpu0-2"}

	got, err := p.getReplicaPreferredAllocation(available, required, 2)
	if err != nil {
		t.Fatalf("getReplicaPreferredAllocation failed: %v", err)
	}

	if got[0] != "gpu0-2" {
		t.Fatalf("expected required device to be first, got %v", got)
	}
	if physicalIDForDevice(got[1], p.AMDGPUs[got[1]]) != "gpu1" {
		t.Fatalf("expected second device from gpu1, got %v", got)
	}
}

func TestReplicaPreferredAllocationCanOversubscribeAfterDistinctGPUs(t *testing.T) {
	p := testReplicaPlugin(2)
	available := []string{"gpu0-0", "gpu0-1", "gpu1-0", "gpu1-1"}

	got, err := p.getReplicaPreferredAllocation(available, nil, 3)
	if err != nil {
		t.Fatalf("getReplicaPreferredAllocation failed: %v", err)
	}

	counts := physicalCounts(got, p.AMDGPUs)
	if len(counts) != 2 {
		t.Fatalf("expected both physical GPUs to be used, got IDs %v counts %v", got, counts)
	}
	if counts["gpu0"] != 2 || counts["gpu1"] != 1 {
		t.Fatalf("expected balanced oversubscription after distinct GPUs, got IDs %v counts %v", got, counts)
	}
}

func TestReplicaPreferredAllocationUsesPhysicalAllocatorOrder(t *testing.T) {
	p := testReplicaPlugin(2)
	p.allocatorInitError = false
	p.devAllocator = &staticAllocator{allocated: []string{"gpu1", "gpu0"}}
	available := []string{"gpu0-0", "gpu0-1", "gpu1-0", "gpu1-1"}

	got, err := p.getReplicaPreferredAllocation(available, nil, 2)
	if err != nil {
		t.Fatalf("getReplicaPreferredAllocation failed: %v", err)
	}

	want := []string{"gpu1-0", "gpu0-0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestAllocateDedupesSharedPhysicalDeviceSpecs(t *testing.T) {
	p := testReplicaPlugin(2)

	resp, err := p.Allocate(context.Background(), &pluginapi.AllocateRequest{
		ContainerRequests: []*pluginapi.ContainerAllocateRequest{
			{DevicesIDs: []string{"gpu0-0", "gpu0-1"}},
		},
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	if len(resp.ContainerResponses) != 1 {
		t.Fatalf("expected 1 container response, got %d", len(resp.ContainerResponses))
	}

	got := deviceSpecPaths(resp.ContainerResponses[0].Devices)
	want := []string{"/dev/kfd", "/dev/dri/card0", "/dev/dri/renderD128"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected device specs %v, got %v", want, got)
	}
}

func testReplicaPlugin(replica int) *AMDGPUPlugin {
	return &AMDGPUPlugin{
		AMDGPUs:            buildLogicalAMDGPUsFromPhysical(testPhysicalAMDGPUs(), replica),
		physicalAMDGPUs:    testPhysicalAMDGPUs(),
		Replica:            replica,
		allocatorInitError: true,
	}
}

func testPhysicalAMDGPUs() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"gpu0": {
			"card":                 0,
			"renderD":              128,
			"devID":                "0000:01:00.0",
			"computePartitionType": "spx",
			"memoryPartitionType":  "nps1",
			"nodeId":               1,
			"numaNode":             0,
		},
		"gpu1": {
			"card":                 1,
			"renderD":              129,
			"devID":                "0000:02:00.0",
			"computePartitionType": "spx",
			"memoryPartitionType":  "nps1",
			"nodeId":               2,
			"numaNode":             0,
		},
	}
}

func gotPhysicalIDs(ids []string, devices map[string]map[string]interface{}) []string {
	physicalIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		physicalIDs = append(physicalIDs, physicalIDForDevice(id, devices[id]))
	}
	return physicalIDs
}

func physicalCounts(ids []string, devices map[string]map[string]interface{}) map[string]int {
	counts := make(map[string]int)
	for _, id := range ids {
		counts[physicalIDForDevice(id, devices[id])]++
	}
	return counts
}

func deviceSpecPaths(devs []*pluginapi.DeviceSpec) []string {
	paths := make([]string, 0, len(devs))
	for _, dev := range devs {
		paths = append(paths, dev.HostPath)
	}
	return paths
}

type staticAllocator struct {
	allocated []string
}

func (s *staticAllocator) Init(devs []*allocator.Device, topoDir string) error {
	return nil
}

func (s *staticAllocator) Allocate(available, required []string, size int) ([]string, error) {
	return s.allocated, nil
}
