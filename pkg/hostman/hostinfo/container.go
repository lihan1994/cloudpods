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

package hostinfo

import (
	"context"
	"path"
	"time"

	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/utils"

	apis "yunion.io/x/onecloud/pkg/apis/compute"
	hostapi "yunion.io/x/onecloud/pkg/apis/host"
	"yunion.io/x/onecloud/pkg/hostman/isolated_device"
	"yunion.io/x/onecloud/pkg/hostman/options"
	"yunion.io/x/onecloud/pkg/util/pod"
	"yunion.io/x/onecloud/pkg/util/pod/cadvisor"
	"yunion.io/x/onecloud/pkg/util/pod/stats"
)

func (h *SHostInfo) initCRI() error {
	cri, err := pod.NewCRI(h.GetContainerRuntimeEndpoint(), 3*time.Second)
	if err != nil {
		return errors.Wrapf(err, "New CRI by endpoint %q", h.GetContainerRuntimeEndpoint())
	}
	ver, err := cri.Version(context.Background())
	if err != nil {
		return errors.Wrap(err, "get runtime version")
	}
	log.Infof("Init container runtime: %s", ver)
	h.cri = cri
	return nil
}

func (h *SHostInfo) initContainerCPUMap(topo *hostapi.HostTopology) error {
	statefile := path.Join(options.HostOptions.ServersPath, "container_cpu_map")
	cm, err := pod.NewHostContainerCPUMap(topo, statefile)
	if err != nil {
		return errors.Wrap(err, "NewHostContainerCPUMap")
	}
	h.containerCPUMap = cm
	return nil
}

func (h *SHostInfo) startContainerStatsProvider(cri pod.CRI) error {
	ca, err := cadvisor.New(nil, "/opt/cloud/workspace", []string{"cloudpods"})
	if err != nil {
		return errors.Wrap(err, "new cadvisor")
	}
	if err := ca.Start(); err != nil {
		return errors.Wrap(err, "start cadvisor")
	}
	h.containerStatsProvider = stats.NewCRIContainerStatsProvider(ca, cri.GetRuntimeClient(), cri.GetImageClient())
	return nil
}

func (h *SHostInfo) GetCRI() pod.CRI {
	return h.cri
}

func (h *SHostInfo) GetContainerCPUMap() *pod.HostContainerCPUMap {
	return h.containerCPUMap
}

func (h *SHostInfo) GetContainerStatsProvider() stats.ContainerStatsProvider {
	return h.containerStatsProvider
}

type INvidiaGpuIndexMemoryInterface interface {
	GetNvidiaDevMemSize() int
	GetNvidiaDevIndex() string
}

func (h *SHostInfo) GetNvidiaGpuIndexMemoryMap() map[string]int {
	res := map[string]int{}
	for i := range h.containerNvidiaGpus {
		iDev, ok := h.containerNvidiaGpus[i].(INvidiaGpuIndexMemoryInterface)
		if !ok {
			continue
		}
		index := iDev.GetNvidiaDevIndex()
		memSize := iDev.GetNvidiaDevMemSize()
		res[index] = memSize
	}
	return res
}

func (h *SHostInfo) HasContainerVastaitechGpu() bool {
	if h.hasVastaitechGpus != nil {
		return *h.hasVastaitechGpus
	}
	hasVastaitechGpus := false
	devs := h.IsolatedDeviceMan.GetDevices()
	for i := range devs {
		if devs[i].GetDeviceType() == apis.CONTAINER_DEV_VASTAITECH_GPU {
			hasVastaitechGpus = true
		}
	}
	h.hasVastaitechGpus = &hasVastaitechGpus
	return *h.hasVastaitechGpus
}

func (h *SHostInfo) HasContainerCphAmdGpu() bool {
	if h.hasCphAmdGpus != nil {
		return *h.hasCphAmdGpus
	}
	hasCphAmdGpus := false
	devs := h.IsolatedDeviceMan.GetDevices()
	for i := range devs {
		if devs[i].GetDeviceType() == apis.CONTAINER_DEV_CPH_AMD_GPU {
			hasCphAmdGpus = true
		}
	}
	h.hasCphAmdGpus = &hasCphAmdGpus
	return *h.hasCphAmdGpus
}

func (h *SHostInfo) HasContainerNvidiaGpu() bool {
	if h.hasNvidiaGpus != nil {
		return *h.hasNvidiaGpus
	}
	hasNvidiaGpus := false
	nvDevs := make([]isolated_device.IDevice, 0)
	devs := h.IsolatedDeviceMan.GetDevices()
	for i := range devs {
		if utils.IsInStringArray(devs[i].GetDeviceType(), apis.NVIDIA_GPU_TYPES) {
			hasNvidiaGpus = true
			nvDevs = append(nvDevs, devs[i])
		}
	}
	h.hasNvidiaGpus = &hasNvidiaGpus
	h.containerNvidiaGpus = nvDevs
	return *h.hasNvidiaGpus
}
