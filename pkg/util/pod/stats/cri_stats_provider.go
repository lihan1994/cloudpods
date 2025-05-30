// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package stats

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	cadvisorfs "github.com/google/cadvisor/fs"
	cadvisorapiv1 "github.com/google/cadvisor/info/v1"
	cadvisorapiv2 "github.com/google/cadvisor/info/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"

	"yunion.io/x/pkg/errors"

	"yunion.io/x/onecloud/pkg/util/pod/cadvisor"
)

var (
	// defaultCachePeriod is the default cache period for each cpuUsage.
	defaultCachePeriod = 10 * time.Minute
)

type cpuUsageRecord struct {
	stats          *runtimeapi.CpuUsage
	usageNanoCores *uint64
}

// criStatsProvider implements the ContainerStatsProvider interface by getting
// the container stats from CRI.
type criStatsProvider struct {
	// cadvisor is used to get the node root filesystem's stats (such as the
	// capacity/available bytes/inodes) that will be populated in per container
	// filesystem stats.
	cadvisor cadvisor.Interface
	// runtimeService is used to get the status and stats of the pods and its
	// managed containers.
	runtimeService runtimeapi.RuntimeServiceClient
	// imageService is used to get the stats of the image filesystem.
	imageService runtimeapi.ImageServiceClient

	// cpuUsageCache caches the cpu usage for containers.
	cpuUsageCache map[string]*cpuUsageRecord
	mutex         sync.RWMutex
}

func NewCRIContainerStatsProvider(
	cadvisor cadvisor.Interface,
	runtimeService runtimeapi.RuntimeServiceClient,
	imageService runtimeapi.ImageServiceClient,
) ContainerStatsProvider {
	return newCRIStatsProvider(cadvisor, runtimeService, imageService)
}

// newCRIStatsProvider returns a ContainerStatsProvider implementation that
// provides container stats using CRI.
func newCRIStatsProvider(
	cadvisor cadvisor.Interface,
	runtimeService runtimeapi.RuntimeServiceClient,
	imageService runtimeapi.ImageServiceClient,
) ContainerStatsProvider {
	return &criStatsProvider{
		cadvisor:       cadvisor,
		runtimeService: runtimeService,
		imageService:   imageService,
		cpuUsageCache:  make(map[string]*cpuUsageRecord),
	}
}

func (p *criStatsProvider) ListPodStats() ([]PodStats, error) {
	// Don't update CPU nano core usage.
	return p.listPodStats(false)
}

// ListPodStatsAndUpdateCPUNanoCoreUsage updates the cpu nano core usage for
// the containers and returns the stats for all the pod-managed containers.
// This is a workaround because CRI runtimes do not supply nano core usages,
// so this function calculate the difference between the current and the last
// (cached) cpu stats to calculate this metrics. The implementation assumes a
// single caller to periodically invoke this function to update the metrics. If
// there exist multiple callers, the period used to compute the cpu usage may
// vary and the usage could be incoherent (e.g., spiky). If no caller calls
// this function, the cpu usage will stay nil. Right now, eviction manager is
// the only caller, and it calls this function every 10s.
func (p *criStatsProvider) ListPodStatsAndUpdateCPUNanoCoreUsage() ([]PodStats, error) {
	// Update CPU nano core usage.
	return p.listPodStats(true)
}

func (p *criStatsProvider) listPodStats(updateCPUNanoCoreUsage bool) ([]PodStats, error) {
	// Gets node root filesystem information, which will be used to populate
	// the available and capacity bytes/inodes in container stats.
	rootFsInfo, err := p.cadvisor.RootFsInfo()
	if err != nil {
		return nil, fmt.Errorf("failed to get rootFs info: %v", err)
	}

	csResp, err := p.runtimeService.ListContainers(context.Background(), &runtimeapi.ListContainersRequest{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list all containers")
	}
	containers := csResp.Containers

	// Creates pod sandbox map.
	podSandboxMap := make(map[string]*runtimeapi.PodSandbox)
	resp, err := p.runtimeService.ListPodSandbox(context.Background(), &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		return nil, errors.Wrap(err, "failed to list all pod sandboxes")
	}
	podSandboxes := removeTerminatedPods(resp.Items)
	for _, s := range podSandboxes {
		podSandboxMap[s.Id] = s
	}
	// fsIDtoInfo is a map from filesystem id to its stats. This will be used
	// as a cache to avoid querying cAdvisor for the filesystem stats with the
	// same filesystem id many times.
	fsIDtoInfo := make(map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo)

	// sandboxIDToPodStats is a temporary map from sandbox ID to its pod stats.
	sandboxIDToPodStats := make(map[string]*PodStats)

	cstsResp, err := p.runtimeService.ListContainerStats(context.Background(), &runtimeapi.ListContainerStatsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all container stats: %v", err)
	}

	containers = removeTerminatedContainers(containers)
	// Creates container map.
	containerMap := make(map[string]*runtimeapi.Container)
	for _, c := range containers {
		containerMap[c.Id] = c
	}

	allInfos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch cadvisor stats: %v", err)
	}
	caInfos := getCRICadvisorStats(allInfos)

	// get network stats for containers.
	// This is only used on Windows. For other platforms, (nil, nil) should be returned.
	containerNetworkStats, err := p.listContainerNetworkStats()
	if err != nil {
		return nil, fmt.Errorf("failed to list container network stats: %v", err)
	}

	for _, stats := range cstsResp.Stats {
		containerID := stats.Attributes.Id
		container, found := containerMap[containerID]
		if !found {
			continue
		}

		podSandboxID := container.PodSandboxId
		podSandbox, found := podSandboxMap[podSandboxID]
		if !found {
			continue
		}

		// Creates the stats of the pod (if not created yet) which the
		// container belongs to.
		ps, found := sandboxIDToPodStats[podSandboxID]
		if !found {
			ps = buildPodStats(podSandbox)
			sandboxIDToPodStats[podSandboxID] = ps
		}

		// Fill available stats for full set of required pod stats
		cs := p.makeContainerStats(stats, container, &rootFsInfo, fsIDtoInfo, podSandbox.GetMetadata(), updateCPUNanoCoreUsage, allInfos)
		p.addPodNetworkStats(ps, podSandboxID, caInfos, cs, containerNetworkStats[podSandboxID])
		p.addPodCPUMemoryStats(ps, types.UID(podSandbox.Metadata.Uid), allInfos, cs)
		p.addDiskIoStats(ps, types.UID(podSandboxID), allInfos, cs)
		p.addProcessStats(ps, types.UID(podSandboxID), allInfos, cs)

		// If cadvisor stats is available for the container, use it to populate
		// container stats
		caStats, caFound := caInfos[containerID]
		if !caFound {
			klog.V(5).Infof("Unable to find cadvisor stats for %q", containerID)
		} else {
			p.addCadvisorContainerStats(cs, &caStats)
		}
		ps.Containers = append(ps.Containers, *cs)
	}
	// cleanup outdated caches.
	p.cleanupOutdatedCaches()

	result := make([]PodStats, 0, len(sandboxIDToPodStats))
	for _, s := range sandboxIDToPodStats {
		//p.makePodStorageStats(s, &rootFsInfo)
		result = append(result, *s)
	}
	return result, nil
}

func (p *criStatsProvider) ListPodCPUAndMemoryStats() ([]PodStats, error) {
	ctx := context.Background()
	containersResp, err := p.runtimeService.ListContainers(ctx, &runtimeapi.ListContainersRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all containers: %v", err)
	}
	containers := containersResp.Containers

	// Creates pod sandbox map.
	podSandboxMap := make(map[string]*runtimeapi.PodSandbox)
	resp, err := p.runtimeService.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all pod sandboxes: %v", err)
	}
	podSandboxes := resp.Items
	podSandboxes = removeTerminatedPods(podSandboxes)
	for _, s := range podSandboxes {
		podSandboxMap[s.Id] = s
	}

	// sandboxIDToPodStats is a temporary map from sandbox ID to its pod stats.
	sandboxIDToPodStats := make(map[string]*PodStats)

	containerStatResp, err := p.runtimeService.ListContainerStats(ctx, &runtimeapi.ListContainerStatsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list all container stats: %v", err)
	}

	containers = removeTerminatedContainers(containers)
	// Creates container map.
	containerMap := make(map[string]*runtimeapi.Container)
	for _, c := range containers {
		containerMap[c.Id] = c
	}

	allInfos, err := getCadvisorContainerInfo(p.cadvisor)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch cadvisor stats: %v", err)
	}
	caInfos := getCRICadvisorStats(allInfos)

	for _, stats := range containerStatResp.Stats {
		containerID := stats.Attributes.Id
		container, found := containerMap[containerID]
		if !found {
			continue
		}

		podSandboxID := container.PodSandboxId
		podSandbox, found := podSandboxMap[podSandboxID]
		if !found {
			continue
		}

		// Creates the stats of the pod (if not created yet) which the
		// container belongs to.
		ps, found := sandboxIDToPodStats[podSandboxID]
		if !found {
			ps = buildPodStats(podSandbox)
			sandboxIDToPodStats[podSandboxID] = ps
		}

		// Fill available CPU and memory stats for full set of required pod stats
		cs := p.makeContainerCPUAndMemoryStats(stats, container, allInfos)
		p.addPodCPUMemoryStats(ps, types.UID(podSandbox.Metadata.Uid), allInfos, cs)
		p.addDiskIoStats(ps, types.UID(podSandboxID), allInfos, cs)
		p.addProcessStats(ps, types.UID(podSandboxID), allInfos, cs)

		// If cadvisor stats is available for the container, use it to populate
		// container stats
		caStats, caFound := caInfos[containerID]
		if !caFound {
			klog.V(4).Infof("Unable to find cadvisor stats for %q", containerID)
		} else {
			p.addCadvisorContainerStats(cs, &caStats)
		}
		ps.Containers = append(ps.Containers, *cs)
	}
	// cleanup outdated caches.
	p.cleanupOutdatedCaches()

	result := make([]PodStats, 0, len(sandboxIDToPodStats))
	for _, s := range sandboxIDToPodStats {
		result = append(result, *s)
	}
	return result, nil
}

func (p *criStatsProvider) ImageFsStats() (FsStats, error) {
	//TODO implement me
	panic("implement me")
}

func (p *criStatsProvider) ImageFsDevice() (string, error) {
	//TODO implement me
	panic("implement me")
}

// buildPodStats returns a PodStats that identifies the Pod managing cinfo
func buildPodStats(podSandbox *runtimeapi.PodSandbox) *PodStats {
	return &PodStats{
		PodRef: PodReference{
			Name:      podSandbox.Metadata.Name,
			UID:       podSandbox.Metadata.Uid,
			Namespace: podSandbox.Metadata.Namespace,
		},
		// The StartTime in the summary API is the pod creation time.
		StartTime: metav1.NewTime(time.Unix(0, podSandbox.CreatedAt)),
	}
}

/*func (p *criStatsProvider) makePodStorageStats(s *PodStats, rootFsInfo *cadvisorapiv2.FsInfo) {
	podNs := s.PodRef.Namespace
	podName := s.PodRef.Name
	podUID := types.UID(s.PodRef.UID)
	vstats, found := p.resourceAnalyzer.GetPodVolumeStats(podUID)
	if !found {
		return
	}
	podLogDir := kuberuntime.BuildPodLogsDirectory(podNs, podName, podUID)
	logStats, err := p.getPodLogStats(podLogDir, rootFsInfo)
	if err != nil {
		klog.Errorf("Unable to fetch pod log stats for path %s: %v ", podLogDir, err)
		// If people do in-place upgrade, there might be pods still using
		// the old log path. For those pods, no pod log stats is returned.
		// We should continue generating other stats in that case.
		// calcEphemeralStorage tolerants logStats == nil.
	}
	ephemeralStats := make([]statsapi.VolumeStats, len(vstats.EphemeralVolumes))
	copy(ephemeralStats, vstats.EphemeralVolumes)
	s.VolumeStats = append(append([]statsapi.VolumeStats{}, vstats.EphemeralVolumes...), vstats.PersistentVolumes...)
	s.EphemeralStorage = calcEphemeralStorage(s.Containers, ephemeralStats, rootFsInfo, logStats, true)
}*/

func (p *criStatsProvider) addPodNetworkStats(
	ps *PodStats,
	podSandboxID string,
	caInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *ContainerStats,
	netStats *NetworkStats,
) {
	caPodSandbox, found := caInfos[podSandboxID]
	// try get network stats from cadvisor first.
	if found {
		networkStats := cadvisorInfoToNetworkStats(&caPodSandbox)
		if networkStats != nil {
			ps.Network = networkStats
			return
		}
	}

	// Not found from cadvisor, get from netStats.
	if netStats != nil {
		ps.Network = netStats
		return
	}

	// TODO: sum Pod network stats from container stats.
	klog.V(4).Infof("Unable to find network stats for sandbox %q", podSandboxID)
}

func (p *criStatsProvider) addPodCPUMemoryStats(
	ps *PodStats,
	podUID types.UID,
	allInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *ContainerStats,
) {
	// try get cpu and memory stats from cadvisor first.
	podCgroupInfo := getCadvisorPodInfoFromPodUID(podUID, allInfos)
	if podCgroupInfo != nil {
		cpu, memory := cadvisorInfoToCPUandMemoryStats(podCgroupInfo)
		ps.CPU = cpu
		ps.Memory = memory
		return
	}

	// Sum Pod cpu and memory stats from containers stats.
	if cs.CPU != nil {
		if ps.CPU == nil {
			ps.CPU = &CPUStats{}
		}

		ps.CPU.Time = cs.CPU.Time
		usageCoreNanoSeconds := getUint64Value(cs.CPU.UsageCoreNanoSeconds) + getUint64Value(ps.CPU.UsageCoreNanoSeconds)
		usageNanoCores := getUint64Value(cs.CPU.UsageNanoCores) + getUint64Value(ps.CPU.UsageNanoCores)
		ps.CPU.UsageCoreNanoSeconds = &usageCoreNanoSeconds
		ps.CPU.UsageNanoCores = &usageNanoCores
	}

	if cs.Memory != nil {
		if ps.Memory == nil {
			ps.Memory = &MemoryStats{}
		}

		ps.Memory.Time = cs.Memory.Time
		availableBytes := getUint64Value(cs.Memory.AvailableBytes) + getUint64Value(ps.Memory.AvailableBytes)
		usageBytes := getUint64Value(cs.Memory.UsageBytes) + getUint64Value(ps.Memory.UsageBytes)
		workingSetBytes := getUint64Value(cs.Memory.WorkingSetBytes) + getUint64Value(ps.Memory.WorkingSetBytes)
		rSSBytes := getUint64Value(cs.Memory.RSSBytes) + getUint64Value(ps.Memory.RSSBytes)
		pageFaults := getUint64Value(cs.Memory.PageFaults) + getUint64Value(ps.Memory.PageFaults)
		majorPageFaults := getUint64Value(cs.Memory.MajorPageFaults) + getUint64Value(ps.Memory.MajorPageFaults)
		ps.Memory.AvailableBytes = &availableBytes
		ps.Memory.UsageBytes = &usageBytes
		ps.Memory.WorkingSetBytes = &workingSetBytes
		ps.Memory.RSSBytes = &rSSBytes
		ps.Memory.PageFaults = &pageFaults
		ps.Memory.MajorPageFaults = &majorPageFaults
	}
}

func (p *criStatsProvider) addDiskIoStats(
	ps *PodStats,
	podUID types.UID,
	allInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *ContainerStats) {
	info := getCadvisorPodInfoFromPodUID(podUID, allInfos)
	if info != nil {
		ps.DiskIo = cadvisorInfoToDiskIoStats(info)
	}
	if ps.DiskIo == nil {
		ps.DiskIo = make(map[string]*DiskIoStat)
	}
	ps.DiskIo.Add(cs.DiskIo)
}

func (p *criStatsProvider) addProcessStats(
	ps *PodStats,
	podUID types.UID,
	allInfos map[string]cadvisorapiv2.ContainerInfo,
	cs *ContainerStats,
) {
	// try get process stats from cadvisor only.
	info := getCadvisorPodInfoFromPodUID(podUID, allInfos)
	if info != nil {
		ps.ProcessStats = cadvisorInfoToProcessStats(info)
	}
	if cs.ProcessStats != nil {
		if ps.ProcessStats == nil {
			ps.ProcessStats = &ProcessStats{}
		}
		ps.ProcessStats.ProcessCount += cs.ProcessStats.ProcessCount
		ps.ProcessStats.FdCount += cs.ProcessStats.FdCount
		ps.ProcessStats.SocketCount += cs.ProcessStats.SocketCount
		ps.ProcessStats.ThreadsCurrent += cs.ProcessStats.ThreadsCurrent
		ps.ProcessStats.ThreadsMax += cs.ProcessStats.ThreadsMax
	}
}

// getFsInfo returns the information of the filesystem with the specified
// fsID. If any error occurs, this function logs the error and returns
// nil.
func (p *criStatsProvider) getFsInfo(fsID *runtimeapi.FilesystemIdentifier) *cadvisorapiv2.FsInfo {
	if fsID == nil {
		klog.V(2).Infof("Failed to get filesystem info: fsID is nil.")
		return nil
	}
	mountpoint := fsID.GetMountpoint()
	fsInfo, err := p.cadvisor.GetDirFsInfo(mountpoint)
	if err != nil {
		msg := fmt.Sprintf("Failed to get the info of the filesystem with mountpoint %q: %v.", mountpoint, err)
		if err == cadvisorfs.ErrNoSuchDevice {
			klog.V(2).Info(msg)
		} else {
			klog.Error(msg)
		}
		return nil
	}
	return &fsInfo
}

func (p *criStatsProvider) makeContainerStats(stats *runtimeapi.ContainerStats, container *runtimeapi.Container, rootFsInfo *cadvisorapiv2.FsInfo, fsIDtoInfo map[runtimeapi.FilesystemIdentifier]*cadvisorapiv2.FsInfo, meta *runtimeapi.PodSandboxMetadata, updateCPUNanoCoreUsage bool, infos map[string]cadvisorapiv2.ContainerInfo) *ContainerStats {
	result := &ContainerStats{
		Name: stats.Attributes.Metadata.Name,
		// The StartTime in the summary API is the container creation time.
		StartTime: metav1.NewTime(time.Unix(0, container.CreatedAt)),
		CPU:       &CPUStats{},
		Memory:    &MemoryStats{},
		Rootfs:    &FsStats{},
		// UserDefinedMetrics is not supported by CRI.
		ProcessStats: &ProcessStats{},
	}
	// process stats
	cStats := getLatestContainerStatsById(stats.Attributes.GetId(), infos)
	if cStats != nil {
		result.ProcessStats = convertToProcessStats(cStats.Processes)
		result.DiskIo = convertToDiskIoStats(cStats.DiskIo)
	}
	if stats.Cpu != nil {
		result.CPU.Time = metav1.NewTime(time.Unix(0, stats.Cpu.Timestamp))
		if stats.Cpu.UsageCoreNanoSeconds != nil {
			result.CPU.UsageCoreNanoSeconds = &stats.Cpu.UsageCoreNanoSeconds.Value
		}
		var usageNanoCores *uint64
		if updateCPUNanoCoreUsage {
			usageNanoCores = p.getAndUpdateContainerUsageNanoCores(stats)
		} else {
			usageNanoCores = p.getContainerUsageNanoCores(stats)
		}
		if usageNanoCores != nil {
			result.CPU.UsageNanoCores = usageNanoCores
		}
	} else {
		result.CPU.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.CPU.UsageCoreNanoSeconds = uint64Ptr(0)
		result.CPU.UsageNanoCores = uint64Ptr(0)
	}
	if stats.Memory != nil {
		result.Memory.Time = metav1.NewTime(time.Unix(0, stats.Memory.Timestamp))
		if stats.Memory.WorkingSetBytes != nil {
			result.Memory.WorkingSetBytes = &stats.Memory.WorkingSetBytes.Value
		}
		if cStats != nil && cStats.Memory != nil {
			result.Memory.UsageBytes = &cStats.Memory.Usage
			result.Memory.RSSBytes = &cStats.Memory.RSS
		}
	} else {
		result.Memory.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.Memory.WorkingSetBytes = uint64Ptr(0)
	}
	if stats.WritableLayer != nil {
		result.Rootfs.Time = metav1.NewTime(time.Unix(0, stats.WritableLayer.Timestamp))
		if stats.WritableLayer.UsedBytes != nil {
			result.Rootfs.UsedBytes = &stats.WritableLayer.UsedBytes.Value
		}
		if stats.WritableLayer.InodesUsed != nil {
			result.Rootfs.InodesUsed = &stats.WritableLayer.InodesUsed.Value
		}
	}
	fsID := stats.GetWritableLayer().GetFsId()
	if fsID != nil {
		imageFsInfo, found := fsIDtoInfo[*fsID]
		if !found {
			imageFsInfo = p.getFsInfo(fsID)
			fsIDtoInfo[*fsID] = imageFsInfo
		}
		if imageFsInfo != nil {
			// The image filesystem id is unknown to the local node or there's
			// an error on retrieving the stats. In these cases, we omit those stats
			// and return the best-effort partial result. See
			// https://github.com/kubernetes/heapster/issues/1793.
			result.Rootfs.AvailableBytes = &imageFsInfo.Available
			result.Rootfs.CapacityBytes = &imageFsInfo.Capacity
			result.Rootfs.InodesFree = imageFsInfo.InodesFree
			result.Rootfs.Inodes = imageFsInfo.Inodes
		}
	}
	// NOTE: This doesn't support the old pod log path, `/var/log/pods/UID`. For containers
	// using old log path, empty log stats are returned. This is fine, because we don't
	// officially support in-place upgrade anyway.
	/*var (
		containerLogPath = kuberuntime.BuildContainerLogsDirectory(meta.GetNamespace(),
			meta.GetName(), types.UID(meta.GetUid()), container.GetMetadata().GetName())
		err error
	)
	result.Logs, err = p.getPathFsStats(containerLogPath, rootFsInfo)
	if err != nil {
		klog.Errorf("Unable to fetch container log stats for path %s: %v ", containerLogPath, err)
	}*/
	return result
}

func convertToProcessStats(cStats *cadvisorapiv1.ProcessStats) *ProcessStats {
	if cStats == nil {
		return nil
	}
	return &ProcessStats{
		ProcessCount:   cStats.ProcessCount,
		FdCount:        cStats.FdCount,
		SocketCount:    cStats.SocketCount,
		ThreadsCurrent: cStats.ThreadsCurrent,
		ThreadsMax:     cStats.ThreadsMax,
	}
}

func (p *criStatsProvider) makeContainerCPUAndMemoryStats(
	stats *runtimeapi.ContainerStats,
	container *runtimeapi.Container,
	infos map[string]cadvisorapiv2.ContainerInfo,
) *ContainerStats {
	result := &ContainerStats{
		Name: stats.Attributes.Metadata.Name,
		// The StartTime in the summary API is the container creation time.
		StartTime: metav1.NewTime(time.Unix(0, container.CreatedAt)),
		CPU:       &CPUStats{},
		Memory:    &MemoryStats{},
		// UserDefinedMetrics is not supported by CRI.
		ProcessStats: &ProcessStats{},
	}
	// process stats
	cStats := getLatestContainerStatsById(stats.Attributes.GetId(), infos)
	if cStats != nil {
		result.ProcessStats = convertToProcessStats(cStats.Processes)
		result.DiskIo = convertToDiskIoStats(cStats.DiskIo)
	}
	if stats.Cpu != nil {
		result.CPU.Time = metav1.NewTime(time.Unix(0, stats.Cpu.Timestamp))
		if stats.Cpu.UsageCoreNanoSeconds != nil {
			result.CPU.UsageCoreNanoSeconds = &stats.Cpu.UsageCoreNanoSeconds.Value
		}

		usageNanoCores := p.getContainerUsageNanoCores(stats)
		if usageNanoCores != nil {
			result.CPU.UsageNanoCores = usageNanoCores
		}
	} else {
		result.CPU.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.CPU.UsageCoreNanoSeconds = uint64Ptr(0)
		result.CPU.UsageNanoCores = uint64Ptr(0)
	}
	if stats.Memory != nil {
		result.Memory.Time = metav1.NewTime(time.Unix(0, stats.Memory.Timestamp))
		if stats.Memory.WorkingSetBytes != nil {
			result.Memory.WorkingSetBytes = &stats.Memory.WorkingSetBytes.Value
		}
		if cStats != nil && cStats.Memory != nil {
			result.Memory.UsageBytes = &cStats.Memory.Usage
			result.Memory.RSSBytes = &cStats.Memory.RSS
		}
	} else {
		result.Memory.Time = metav1.NewTime(time.Unix(0, time.Now().UnixNano()))
		result.Memory.WorkingSetBytes = uint64Ptr(0)
	}

	return result
}

// getContainerUsageNanoCores gets the cached usageNanoCores.
func (p *criStatsProvider) getContainerUsageNanoCores(stats *runtimeapi.ContainerStats) *uint64 {
	if stats == nil || stats.Attributes == nil {
		return nil
	}

	p.mutex.RLock()
	defer p.mutex.RUnlock()

	cached, ok := p.cpuUsageCache[stats.Attributes.Id]
	if !ok || cached.usageNanoCores == nil {
		return nil
	}
	// return a copy of the usage
	latestUsage := *cached.usageNanoCores
	return &latestUsage
}

// getContainerUsageNanoCores computes usageNanoCores based on the given and
// the cached usageCoreNanoSeconds, updates the cache with the computed
// usageNanoCores, and returns the usageNanoCores.
func (p *criStatsProvider) getAndUpdateContainerUsageNanoCores(stats *runtimeapi.ContainerStats) *uint64 {
	if stats == nil || stats.Attributes == nil || stats.Cpu == nil || stats.Cpu.UsageCoreNanoSeconds == nil {
		return nil
	}
	id := stats.Attributes.Id
	usage, err := func() (*uint64, error) {
		p.mutex.Lock()
		defer p.mutex.Unlock()

		cached, ok := p.cpuUsageCache[id]
		if !ok || cached.stats.UsageCoreNanoSeconds == nil || stats.Cpu.UsageCoreNanoSeconds.Value < cached.stats.UsageCoreNanoSeconds.Value {
			// Cannot compute the usage now, but update the cached stats anyway
			p.cpuUsageCache[id] = &cpuUsageRecord{stats: stats.Cpu, usageNanoCores: nil}
			return nil, nil
		}

		newStats := stats.Cpu
		cachedStats := cached.stats
		nanoSeconds := newStats.Timestamp - cachedStats.Timestamp
		if nanoSeconds <= 0 {
			return nil, fmt.Errorf("zero or negative interval (%v - %v)", newStats.Timestamp, cachedStats.Timestamp)
		}
		usageNanoCores := uint64(float64(newStats.UsageCoreNanoSeconds.Value-cachedStats.UsageCoreNanoSeconds.Value) /
			float64(nanoSeconds) * float64(time.Second/time.Nanosecond))

		// Update cache with new value.
		usageToUpdate := usageNanoCores
		p.cpuUsageCache[id] = &cpuUsageRecord{stats: newStats, usageNanoCores: &usageToUpdate}

		return &usageNanoCores, nil
	}()

	if err != nil {
		// This should not happen. Log now to raise visibility
		klog.Errorf("failed updating cpu usage nano core: %v", err)
	}
	return usage
}

func (p *criStatsProvider) cleanupOutdatedCaches() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for k, v := range p.cpuUsageCache {
		if v == nil {
			delete(p.cpuUsageCache, k)
			continue
		}

		if time.Since(time.Unix(0, v.stats.Timestamp)) > defaultCachePeriod {
			delete(p.cpuUsageCache, k)
		}
	}
}

// removeTerminatedPods returns pods with terminated ones removed.
// It only removes a terminated pod when there is a running instance
// of the pod with the same name and namespace.
// This is needed because:
// 1) PodSandbox may be recreated;
// 2) Pod may be recreated with the same name and namespace.
func removeTerminatedPods(pods []*runtimeapi.PodSandbox) []*runtimeapi.PodSandbox {
	podMap := make(map[PodReference][]*runtimeapi.PodSandbox)
	// Sort order by create time
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreatedAt < pods[j].CreatedAt
	})
	for _, pod := range pods {
		refID := PodReference{
			Name:      pod.GetMetadata().GetName(),
			Namespace: pod.GetMetadata().GetNamespace(),
			// UID is intentionally left empty.
		}
		podMap[refID] = append(podMap[refID], pod)
	}

	result := make([]*runtimeapi.PodSandbox, 0)
	for _, refs := range podMap {
		if len(refs) == 1 {
			result = append(result, refs[0])
			continue
		}
		found := false
		for i := 0; i < len(refs); i++ {
			if refs[i].State == runtimeapi.PodSandboxState_SANDBOX_READY {
				found = true
				result = append(result, refs[i])
			}
		}
		if !found {
			result = append(result, refs[len(refs)-1])
		}
	}
	return result
}

// removeTerminatedContainers removes all terminated containers since they should
// not be used for usage calculations.
func removeTerminatedContainers(containers []*runtimeapi.Container) []*runtimeapi.Container {
	containerMap := make(map[containerID][]*runtimeapi.Container)
	// Sort order by create time
	sort.Slice(containers, func(i, j int) bool {
		return containers[i].CreatedAt < containers[j].CreatedAt
	})
	for _, container := range containers {
		refID := containerID{
			podRef:        buildPodRef(container.Labels),
			containerName: GetContainerName(container.Labels),
		}
		containerMap[refID] = append(containerMap[refID], container)
	}

	result := make([]*runtimeapi.Container, 0)
	for _, refs := range containerMap {
		for i := 0; i < len(refs); i++ {
			if refs[i].State == runtimeapi.ContainerState_CONTAINER_RUNNING {
				result = append(result, refs[i])
			}
		}
	}
	return result
}

func (p *criStatsProvider) addCadvisorContainerStats(
	cs *ContainerStats,
	caPodStats *cadvisorapiv2.ContainerInfo,
) {
	if caPodStats.Spec.HasCustomMetrics {
		cs.UserDefinedMetrics = cadvisorInfoToUserDefinedMetrics(caPodStats)
	}

	cpu, memory := cadvisorInfoToCPUandMemoryStats(caPodStats)
	if cpu != nil {
		cs.CPU = cpu
	}
	if memory != nil {
		cs.Memory = memory
	}
}

func getCRICadvisorStats(infos map[string]cadvisorapiv2.ContainerInfo) map[string]cadvisorapiv2.ContainerInfo {
	stats := make(map[string]cadvisorapiv2.ContainerInfo)
	infos = removeTerminatedContainerInfo(infos)
	for key, info := range infos {
		// On systemd using devicemapper each mount into the container has an
		// associated cgroup. We ignore them to ensure we do not get duplicate
		// entries in our summary. For details on .mount units:
		// http://man7.org/linux/man-pages/man5/systemd.mount.5.html
		if strings.HasSuffix(key, ".mount") {
			continue
		}
		// Build the Pod key if this container is managed by a Pod
		if !isPodManagedContainer(&info) {
			continue
		}
		stats[path.Base(key)] = info
	}
	return stats
}

/*func (p *criStatsProvider) getPathFsStats(path string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	m := p.logMetricsService.createLogMetricsProvider(path)
	logMetrics, err := m.GetMetrics()
	if err != nil {
		return nil, err
	}
	result := &statsapi.FsStats{
		Time:           metav1.NewTime(rootFsInfo.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
	}
	usedbytes := uint64(logMetrics.Used.Value())
	result.UsedBytes = &usedbytes
	inodesUsed := uint64(logMetrics.InodesUsed.Value())
	result.InodesUsed = &inodesUsed
	result.Time = maxUpdateTime(&result.Time, &logMetrics.Time)
	return result, nil
}*/

// getPodLogStats gets stats for logs under the pod log directory. Container logs usually exist
// under the container log directory. However, for some container runtimes, e.g. kata, gvisor,
// they may want to keep some pod level logs, in that case they can put those logs directly under
// the pod log directory. And kubelet will take those logs into account as part of pod ephemeral
// storage.
/*func (p *criStatsProvider) getPodLogStats(path string, rootFsInfo *cadvisorapiv2.FsInfo) (*statsapi.FsStats, error) {
	files, err := p.osInterface.ReadDir(path)
	if err != nil {
		return nil, err
	}
	result := &statsapi.FsStats{
		Time:           metav1.NewTime(rootFsInfo.Timestamp),
		AvailableBytes: &rootFsInfo.Available,
		CapacityBytes:  &rootFsInfo.Capacity,
		InodesFree:     rootFsInfo.InodesFree,
		Inodes:         rootFsInfo.Inodes,
	}
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		// Only include *files* under pod log directory.
		fpath := filepath.Join(path, f.Name())
		fstats, err := p.getPathFsStats(fpath, rootFsInfo)
		if err != nil {
			return nil, fmt.Errorf("failed to get fsstats for %q: %v", fpath, err)
		}
		result.UsedBytes = addUsage(result.UsedBytes, fstats.UsedBytes)
		result.InodesUsed = addUsage(result.InodesUsed, fstats.InodesUsed)
		result.Time = maxUpdateTime(&result.Time, &fstats.Time)
	}
	return result, nil
}*/
