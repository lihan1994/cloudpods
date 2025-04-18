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

package cgroupv1

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCgroupSet(t *testing.T) {
	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Enter pid: ")
	pid, _ := reader.ReadString('\n')
	pid = strings.TrimSpace(pid)
	t.Logf("Start %s cgroup set", pid)
	Init("/sys/fs/cgroup", "")
	manager.NewCGroupCPUTask(pid, "", 1).SetTask()
	manager.CgroupCleanAll("")
}
