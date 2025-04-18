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

// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http//www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package models

import (
	"yunion.io/x/onecloud/pkg/cloudcommon/db"
)

type STopicResourceManager struct {
	db.SResourceBaseManager
}

var TopicResourceManager *STopicResourceManager

func init() {
	TopicResourceManager = &STopicResourceManager{
		SResourceBaseManager: db.NewResourceBaseManager(
			STopicResource{},
			"topic_resources_tbl",
			"topic_resource",
			"topic_resources",
		),
	}
	TopicResourceManager.SetVirtualObject(TopicResourceManager)
	TopicResourceManager.TableSpec().AddIndex(false, "topic_id", "resource_id", "deleted")
}

type STopicResource struct {
	db.SResourceBase

	ResourceId string `width:"64" nullable:"false" create:"required" update:"user" list:"user"`
	TopicId    string `width:"64" nullable:"false" create:"required" update:"user" list:"user"`
}
