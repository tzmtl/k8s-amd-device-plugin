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

// Kubernetes (k8s) device plugin to enable registration of AMD GPU to a container cluster
package plugin

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/ROCm/k8s-device-plugin/internal/pkg/allocator"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/amdgpu"
	"github.com/ROCm/k8s-device-plugin/internal/pkg/exporter"
	"github.com/golang/glog"
	"github.com/kubevirt/device-plugin-manager/pkg/dpm"
	"golang.org/x/net/context"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

// Plugin is identical to DevicePluginServer interface of device plugin API.
type AMDGPUPlugin struct {
	AMDGPUs            map[string]map[string]interface{}
	physicalAMDGPUs    map[string]map[string]interface{}
	Heartbeat          chan bool
	signal             chan os.Signal
	Resource           string
	devAllocator       allocator.Policy
	allocatorInitError bool
	Replica            int
}

type AMDGPUPluginOption func(*AMDGPUPlugin)

func NewAMDGPUPlugin(options ...AMDGPUPluginOption) *AMDGPUPlugin {
	amdGpuPlugin := &AMDGPUPlugin{}
	for _, option := range options {
		option(amdGpuPlugin)
	}
	return amdGpuPlugin
}

func WithAllocator(a allocator.Policy) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.devAllocator = a
	}
}

func WithHeartbeat(ch chan bool) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.Heartbeat = ch
	}
}

func WithResource(res string) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		p.Resource = res
	}
}

func WithReplica(replica int) AMDGPUPluginOption {
	return func(p *AMDGPUPlugin) {
		if replica < 1 {
			replica = 1
		}
		p.Replica = replica
	}
}

// Start is an optional interface that could be implemented by plugin.
// If case Start is implemented, it will be executed by Manager after
// plugin instantiation and before its registration to kubelet. This
// method could be used to prepare resources before they are offered
// to Kubernetes.
func (p *AMDGPUPlugin) Start() error {
	p.signal = make(chan os.Signal, 1)
	signal.Notify(p.signal, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	p.refreshAMDGPUs()
	err := p.devAllocator.Init(getDevicesFromAMDGPUs(p.physicalAMDGPUs), "")
	if err != nil {
		glog.Errorf("allocator init failed. Falling back to kubelet default allocation. Error %v", err)
		p.allocatorInitError = true
	}
	return nil
}

const (
	physicalIDKey   = "physicalID"
	replicaIndexKey = "replicaIndex"
)

func buildLogicalAMDGPUsFromPhysical(physical map[string]map[string]interface{}, replica int) map[string]map[string]interface{} {
	if replica < 1 {
		replica = 1
	}
	logical := make(map[string]map[string]interface{})
	for id, deviceData := range physical {
		for i := 0; i < replica; i++ {
			logicalID := id
			if replica > 1 {
				logicalID = fmt.Sprintf("%s-%d", id, i)
			}
			logicalDeviceData := cloneDeviceData(deviceData)
			logicalDeviceData[physicalIDKey] = id
			logicalDeviceData[replicaIndexKey] = i
			logical[logicalID] = logicalDeviceData
		}
	}
	return logical
}

func cloneDeviceData(deviceData map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(deviceData)+2)
	for k, v := range deviceData {
		cloned[k] = v
	}
	return cloned
}

func (p *AMDGPUPlugin) refreshAMDGPUs() {
	p.physicalAMDGPUs = amdgpu.GetAMDGPUs()
	p.AMDGPUs = buildLogicalAMDGPUsFromPhysical(p.physicalAMDGPUs, p.Replica)
}

func getDevicesFromAMDGPUs(devices map[string]map[string]interface{}) []*allocator.Device {
	var deviceList []*allocator.Device

	for id, deviceData := range devices {
		device := &allocator.Device{
			Id:                   id,
			Card:                 deviceData["card"].(int),
			RenderD:              deviceData["renderD"].(int),
			DevId:                deviceData["devID"].(string),
			ComputePartitionType: deviceData["computePartitionType"].(string),
			MemoryPartitionType:  deviceData["memoryPartitionType"].(string),
			NodeId:               deviceData["nodeId"].(int),
			NumaNode:             deviceData["numaNode"].(int),
		}
		deviceList = append(deviceList, device)
	}
	return deviceList
}

// Stop is an optional interface that could be implemented by plugin.
// If case Stop is implemented, it will be executed by Manager after the
// plugin is unregistered from kubelet. This method could be used to tear
// down resources.
func (p *AMDGPUPlugin) Stop() error {
	return nil
}

var topoSIMDre = regexp.MustCompile(`simd_count\s(\d+)`)

func countGPUDevFromTopology(topoRootParam ...string) int {
	topoRoot := "/sys/class/kfd/kfd"
	if len(topoRootParam) == 1 {
		topoRoot = topoRootParam[0]
	}

	count := 0
	var nodeFiles []string
	var err error
	if nodeFiles, err = filepath.Glob(topoRoot + "/topology/nodes/*/properties"); err != nil {
		glog.Fatalf("glob error: %s", err)
		return count
	}

	for _, nodeFile := range nodeFiles {
		glog.Info("Parsing " + nodeFile)
		f, e := os.Open(nodeFile)
		if e != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			m := topoSIMDre.FindStringSubmatch(scanner.Text())
			if m == nil {
				continue
			}

			if v, _ := strconv.Atoi(m[1]); v > 0 {
				count++
				break
			}
		}
		f.Close()
	}
	return count
}

func simpleHealthCheck() bool {
	entries, err := filepath.Glob("/sys/class/kfd/kfd/topology/nodes/*/properties")
	if err != nil {
		glog.Errorf("Error finding properties files: %v", err)
		return false
	}

	for _, propFile := range entries {
		f, err := os.Open(propFile)
		if err != nil {
			glog.Errorf("Error opening %s: %v", propFile, err)
			continue
		}
		defer f.Close()

		var cpuCores, gfxVersion int
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "cpu_cores_count") {
				parts := strings.Fields(line)
				if len(parts) == 2 {
					cpuCores, _ = strconv.Atoi(parts[1])
				}
			} else if strings.HasPrefix(line, "gfx_target_version") {
				parts := strings.Fields(line)
				if len(parts) == 2 {
					gfxVersion, _ = strconv.Atoi(parts[1])
				}
			}
		}

		if err := scanner.Err(); err != nil {
			glog.Warningf("Error scanning %s: %v", propFile, err)
			continue
		}

		if cpuCores == 0 && gfxVersion > 0 {
			// Found a GPU
			return true
		}
	}

	glog.Warning("No GPU nodes found via properties")
	return false
}

// GetDevicePluginOptions returns options to be communicated with Device
// Manager
func (p *AMDGPUPlugin) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	if p.allocatorInitError && p.Replica <= 1 {
		return &pluginapi.DevicePluginOptions{}, nil
	}
	return &pluginapi.DevicePluginOptions{
		GetPreferredAllocationAvailable: true,
	}, nil
}

// PreStartContainer is expected to be called before each container start if indicated by plugin during registration phase.
// PreStartContainer allows kubelet to pass reinitialized devices to containers.
// PreStartContainer allows Device Plugin to run device specific operations on the Devices requested
func (p *AMDGPUPlugin) PreStartContainer(ctx context.Context, r *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// ListAndWatch returns a stream of List of Devices
// Whenever a Device state change or a Device disappears, ListAndWatch
// returns the new list
func (p *AMDGPUPlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {

	p.refreshAMDGPUs()

	glog.Infof("Found %d AMDGPUs", len(p.AMDGPUs))

	devs := make([]*pluginapi.Device, len(p.AMDGPUs))
	var isHomogeneous bool
	isHomogeneous = amdgpu.IsHomogeneous()
	// Initialize a map to store partitionType based device list
	resourceTypeDevs := make(map[string][]*pluginapi.Device)

	if isHomogeneous {
		// limit scope for hwloc
		func() {
			i := 0
			for id, device := range p.AMDGPUs {
				dev := &pluginapi.Device{
					ID:     id,
					Health: pluginapi.Healthy,
				}
				devs[i] = dev
				i++

				numas := []int64{int64(device["numaNode"].(int))}
				glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

				numaNodes := make([]*pluginapi.NUMANode, len(numas))
				for j, v := range numas {
					numaNodes[j] = &pluginapi.NUMANode{
						ID: int64(v),
					}
				}

				dev.Topology = &pluginapi.TopologyInfo{
					Nodes: numaNodes,
				}
			}
		}()
		s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	} else {
		func() {
			for id, device := range p.AMDGPUs {
				dev := &pluginapi.Device{
					ID:     id,
					Health: pluginapi.Healthy,
				}
				// Append a device belonging to a certain partition type to its respective list
				partitionType := device["computePartitionType"].(string) + "_" + device["memoryPartitionType"].(string)
				resourceTypeDevs[partitionType] = append(resourceTypeDevs[partitionType], dev)

				numas := []int64{int64(device["numaNode"].(int))}
				glog.Infof("Watching GPU with bus ID: %s NUMA Node: %+v", id, numas)

				numaNodes := make([]*pluginapi.NUMANode, len(numas))
				for j, v := range numas {
					numaNodes[j] = &pluginapi.NUMANode{
						ID: int64(v),
					}
				}

				dev.Topology = &pluginapi.TopologyInfo{
					Nodes: numaNodes,
				}
			}
		}()
		// Send the appropriate list of devices based on the partitionType
		if devList, exists := resourceTypeDevs[p.Resource]; exists {
			s.Send(&pluginapi.ListAndWatchResponse{Devices: devList})
		}
	}

loop:
	for {
		select {
		case <-p.Heartbeat:
			var health = pluginapi.Unhealthy

			if simpleHealthCheck() {
				health = pluginapi.Healthy
			}

			// update with per device GPU health status
			if isHomogeneous {
				exporter.PopulatePerGPUDHealth(devs, health)
				s.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
			} else {
				if devList, exists := resourceTypeDevs[p.Resource]; exists {
					exporter.PopulatePerGPUDHealth(devList, health)
					s.Send(&pluginapi.ListAndWatchResponse{Devices: devList})
				}
			}

		case <-p.signal:
			glog.Infof("Received signal, exiting")
			break loop
		}
	}
	// returning a value with this function will unregister the plugin from k8s

	return nil
}

// GetPreferredAllocation returns a preferred set of devices to allocate
// from a list of available ones. The resulting preferred allocation is not
// guaranteed to be the allocation ultimately performed by the
// devicemanager. It is only designed to help the devicemanager make a more
// informed allocation decision when possible.
func (p *AMDGPUPlugin) GetPreferredAllocation(ctx context.Context, req *pluginapi.PreferredAllocationRequest) (*pluginapi.PreferredAllocationResponse, error) {
	response := &pluginapi.PreferredAllocationResponse{}
	for _, req := range req.ContainerRequests {
		allocated_ids, err := p.getPreferredAllocation(req.AvailableDeviceIDs, req.MustIncludeDeviceIDs, int(req.AllocationSize))
		if err != nil {
			glog.Errorf("unable to get preferred allocation list. Error:%v", err)
			return nil, fmt.Errorf("unable to get preferred allocation list. Error:%v", err)
		}
		resp := &pluginapi.ContainerPreferredAllocationResponse{
			DeviceIDs: allocated_ids,
		}
		response.ContainerResponses = append(response.ContainerResponses, resp)
	}
	return response, nil
}

func (p *AMDGPUPlugin) getPreferredAllocation(availableIDs, requiredIDs []string, size int) ([]string, error) {
	if p.Replica <= 1 {
		return p.devAllocator.Allocate(availableIDs, requiredIDs, size)
	}
	return p.getReplicaPreferredAllocation(availableIDs, requiredIDs, size)
}

type replicaDeviceGroup struct {
	physicalID    string
	candidateIDs  []string
	selectedCount int
}

func (p *AMDGPUPlugin) getReplicaPreferredAllocation(availableIDs, requiredIDs []string, size int) ([]string, error) {
	outset := []string{}
	if size <= 0 {
		return outset, fmt.Errorf("allocation size should be positive integer")
	}
	if len(availableIDs) < size {
		return outset, fmt.Errorf("available devices count less than allocation size")
	}
	if len(requiredIDs) > size {
		return outset, fmt.Errorf("must_include devices size is more than allocation size")
	}
	if !stringSetContainsAll(availableIDs, requiredIDs) {
		return outset, fmt.Errorf("must_include devices must be part of available devices")
	}
	if len(availableIDs) == size {
		return copyStringSlice(availableIDs), nil
	}
	if len(requiredIDs) == size {
		return copyStringSlice(requiredIDs), nil
	}
	if p.AMDGPUs == nil {
		p.refreshAMDGPUs()
	}

	groups := make(map[string]*replicaDeviceGroup)
	requiredSet := makeStringSet(requiredIDs)
	outset = append(outset, requiredIDs...)
	for _, id := range availableIDs {
		deviceData, ok := p.AMDGPUs[id]
		if !ok {
			return nil, fmt.Errorf("unknown available device ID %q", id)
		}
		physicalID := physicalIDForDevice(id, deviceData)
		group, ok := groups[physicalID]
		if !ok {
			group = &replicaDeviceGroup{physicalID: physicalID}
			groups[physicalID] = group
		}
		if _, required := requiredSet[id]; required {
			group.selectedCount++
			continue
		}
		group.candidateIDs = append(group.candidateIDs, id)
	}
	for _, id := range requiredIDs {
		if _, ok := p.AMDGPUs[id]; !ok {
			return nil, fmt.Errorf("unknown required device ID %q", id)
		}
	}
	for _, group := range groups {
		sortDeviceIDsByPhysicalReplica(group.candidateIDs, p.AMDGPUs)
	}

	remaining := size - len(requiredIDs)
	requiredPhysicalIDs := getRequiredPhysicalIDs(groups)
	availablePhysicalIDs := getSortedPhysicalIDs(groups)
	targetPhysicalCount := len(requiredPhysicalIDs) + remaining
	if targetPhysicalCount > len(availablePhysicalIDs) {
		targetPhysicalCount = len(availablePhysicalIDs)
	}

	selectedPhysicalIDs := p.preferPhysicalIDs(availablePhysicalIDs, requiredPhysicalIDs, targetPhysicalCount)
	for _, physicalID := range selectedPhysicalIDs {
		if remaining == 0 {
			break
		}
		group := groups[physicalID]
		if group == nil || group.selectedCount > 0 || len(group.candidateIDs) == 0 {
			continue
		}
		outset = append(outset, popReplicaCandidate(group))
		remaining--
	}

	for remaining > 0 {
		fillOrder := getSortedPhysicalIDs(groups)
		sort.SliceStable(fillOrder, func(i, j int) bool {
			left := groups[fillOrder[i]]
			right := groups[fillOrder[j]]
			if left.selectedCount == right.selectedCount {
				return fillOrder[i] < fillOrder[j]
			}
			return left.selectedCount < right.selectedCount
		})
		progress := false
		for _, physicalID := range fillOrder {
			if remaining == 0 {
				break
			}
			group := groups[physicalID]
			if group == nil || len(group.candidateIDs) == 0 {
				continue
			}
			outset = append(outset, popReplicaCandidate(group))
			remaining--
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("unable to find enough replica candidates")
		}
	}

	return outset, nil
}

func (p *AMDGPUPlugin) preferPhysicalIDs(availablePhysicalIDs, requiredPhysicalIDs []string, size int) []string {
	if size <= len(requiredPhysicalIDs) {
		return copyStringSlice(requiredPhysicalIDs)
	}
	if !p.allocatorInitError && p.devAllocator != nil {
		allocatedIDs, err := p.devAllocator.Allocate(availablePhysicalIDs, requiredPhysicalIDs, size)
		if err == nil && len(allocatedIDs) == size && stringSetContainsAll(allocatedIDs, requiredPhysicalIDs) {
			return allocatedIDs
		}
		glog.Warningf("replica preferred allocation falling back to deterministic physical spread: %v", err)
	}

	selected := copyStringSlice(requiredPhysicalIDs)
	selectedSet := makeStringSet(selected)
	for _, id := range availablePhysicalIDs {
		if len(selected) == size {
			break
		}
		if _, exists := selectedSet[id]; exists {
			continue
		}
		selected = append(selected, id)
		selectedSet[id] = struct{}{}
	}
	return selected
}

func physicalIDForDevice(id string, deviceData map[string]interface{}) string {
	if physicalID, ok := deviceData[physicalIDKey].(string); ok && physicalID != "" {
		return physicalID
	}
	return id
}

func replicaIndexForDevice(deviceData map[string]interface{}) int {
	if idx, ok := deviceData[replicaIndexKey].(int); ok {
		return idx
	}
	return 0
}

func popReplicaCandidate(group *replicaDeviceGroup) string {
	id := group.candidateIDs[0]
	group.candidateIDs = group.candidateIDs[1:]
	group.selectedCount++
	return id
}

func getRequiredPhysicalIDs(groups map[string]*replicaDeviceGroup) []string {
	ids := make([]string, 0, len(groups))
	for physicalID, group := range groups {
		if group.selectedCount > 0 {
			ids = append(ids, physicalID)
		}
	}
	sort.Strings(ids)
	return ids
}

func getSortedPhysicalIDs(groups map[string]*replicaDeviceGroup) []string {
	ids := make([]string, 0, len(groups))
	for physicalID := range groups {
		ids = append(ids, physicalID)
	}
	sort.Strings(ids)
	return ids
}

func sortDeviceIDsByPhysicalReplica(ids []string, devices map[string]map[string]interface{}) {
	sort.Slice(ids, func(i, j int) bool {
		leftID := ids[i]
		rightID := ids[j]
		leftData := devices[leftID]
		rightData := devices[rightID]
		leftPhysicalID := physicalIDForDevice(leftID, leftData)
		rightPhysicalID := physicalIDForDevice(rightID, rightData)
		if leftPhysicalID != rightPhysicalID {
			return leftPhysicalID < rightPhysicalID
		}
		leftReplicaIndex := replicaIndexForDevice(leftData)
		rightReplicaIndex := replicaIndexForDevice(rightData)
		if leftReplicaIndex != rightReplicaIndex {
			return leftReplicaIndex < rightReplicaIndex
		}
		return leftID < rightID
	})
}

func stringSetContainsAll(set, subset []string) bool {
	setMap := makeStringSet(set)
	for _, id := range subset {
		if _, ok := setMap[id]; !ok {
			return false
		}
	}
	return true
}

func makeStringSet(ids []string) map[string]struct{} {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

func copyStringSlice(ids []string) []string {
	copied := make([]string, len(ids))
	copy(copied, ids)
	return copied
}

// Allocate is called during container creation so that the Device
// Plugin can run device specific operations and instruct Kubelet
// of the steps to make the Device available in the container
func (p *AMDGPUPlugin) Allocate(ctx context.Context, r *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	var response pluginapi.AllocateResponse
	var car pluginapi.ContainerAllocateResponse

	for _, req := range r.ContainerRequests {
		car = pluginapi.ContainerAllocateResponse{}
		seenDevicePaths := make(map[string]struct{})

		// Currently, there are only 1 /dev/kfd per nodes regardless of the # of GPU available
		// for compute/rocm/HSA use cases
		appendDeviceSpec(&car, seenDevicePaths, "/dev/kfd")

		for _, id := range req.DevicesIDs {
			glog.Infof("Allocating device ID: %s", id)

			deviceData, ok := p.AMDGPUs[id]
			if !ok {
				return nil, fmt.Errorf("unknown device ID %q in allocation request", id)
			}
			for _, k := range []string{"card", "renderD"} {
				v, ok := deviceData[k]
				if !ok {
					return nil, fmt.Errorf("device ID %q is missing %s data", id, k)
				}
				devpath := fmt.Sprintf("/dev/dri/%s%d", k, v)
				appendDeviceSpec(&car, seenDevicePaths, devpath)
			}
		}

		response.ContainerResponses = append(response.ContainerResponses, &car)
	}

	return &response, nil
}

func appendDeviceSpec(car *pluginapi.ContainerAllocateResponse, seenDevicePaths map[string]struct{}, devpath string) {
	if _, exists := seenDevicePaths[devpath]; exists {
		return
	}
	seenDevicePaths[devpath] = struct{}{}
	car.Devices = append(car.Devices, &pluginapi.DeviceSpec{
		HostPath:      devpath,
		ContainerPath: devpath,
		Permissions:   "rw",
	})
}

// Lister serves as an interface between imlementation and Manager machinery. User passes
// implementation of this interface to NewManager function. Manager will use it to obtain resource
// namespace, monitor available resources and instantate a new plugin for them.
type AMDGPULister struct {
	ResUpdateChan chan dpm.PluginNameList
	Heartbeat     chan bool
	Signal        chan os.Signal
	Replica       int
}

// GetResourceNamespace must return namespace (vendor ID) of implemented Lister. e.g. for
// resources in format "color.example.com/<color>" that would be "color.example.com".
func (l *AMDGPULister) GetResourceNamespace() string {
	return "amd.com"
}

// Discover notifies manager with a list of currently available resources in its namespace.
// e.g. if "color.example.com/red" and "color.example.com/blue" are available in the system,
// it would pass PluginNameList{"red", "blue"} to given channel. In case list of
// resources is static, it would use the channel only once and then return. In case the list is
// dynamic, it could block and pass a new list each times resources changed. If blocking is
// used, it should check whether the channel is closed, i.e. Discover should stop.
func (l *AMDGPULister) Discover(pluginListCh chan dpm.PluginNameList) {
	for {
		select {
		case newResourcesList := <-l.ResUpdateChan: // New resources found
			pluginListCh <- newResourcesList
		case <-pluginListCh: // Stop message received
			// Stop resourceUpdateCh
			return
		}
	}
}

// NewPlugin instantiates a plugin implementation. It is given the last name of the resource,
// e.g. for resource name "color.example.com/red" that would be "red". It must return valid
// implementation of a PluginInterface.
func (l *AMDGPULister) NewPlugin(resourceLastName string) dpm.PluginInterface {
	options := []AMDGPUPluginOption{
		WithHeartbeat(l.Heartbeat),
		WithResource(resourceLastName),
		WithAllocator(allocator.NewBestEffortPolicy()),
		WithReplica(l.Replica),
	}
	return NewAMDGPUPlugin(options...)
}
