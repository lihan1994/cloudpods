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

package disk

import (
	"context"

	"yunion.io/x/jsonutils"

	"yunion.io/x/onecloud/pkg/cloudcommon/db/quotas"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/compute/models"
)

type SDiskBaseTask struct {
	taskman.STask
}

func (self *SDiskBaseTask) getDisk() *models.SDisk {
	obj := self.GetObject()
	return obj.(*models.SDisk)
}

func (self *SDiskBaseTask) SetStageFailed(ctx context.Context, reason jsonutils.JSONObject) {
	self.finalReleasePendingUsage(ctx)
	self.STask.SetStageFailed(ctx, reason)
}

func (self *SDiskBaseTask) finalReleasePendingUsage(ctx context.Context) {
	pendingUsage := models.SQuota{}
	err := self.GetPendingUsage(&pendingUsage, 0)
	if err == nil && !pendingUsage.IsEmpty() {
		quotas.CancelPendingUsage(ctx, self.UserCred, &pendingUsage, &pendingUsage, false)
	}
}

func (self *SDiskBaseTask) CleanHostSchedCache(disk *models.SDisk) {
	storage, _ := disk.GetStorage()
	if storage == nil {
		return
	}
	storage.ClearSchedDescCache()
	if len(disk.BackupStorageId) > 0 {
		bkStorage := models.StorageManager.FetchStorageById(disk.BackupStorageId)
		if bkStorage != nil {
			bkStorage.ClearSchedDescCache()
		}
	}
}
