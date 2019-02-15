// Copyright (c) 2017 Huawei Technologies Co., Ltd. All Rights Reserved.
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

/*
This module implements a entry into the OpenSDS northbound service.

*/

package api

import (
	"encoding/json"
	"fmt"

	log "github.com/golang/glog"
	"github.com/opensds/opensds/pkg/api/policy"
	c "github.com/opensds/opensds/pkg/context"
	"github.com/opensds/opensds/pkg/db"
	"github.com/opensds/opensds/pkg/model"
)

// DockPortal
type DockPortal struct {
	BasePortal
}

// ListDocks
func (d *DockPortal) ListDocks() {
	if !policy.Authorize(d.Ctx, "dock:list") {
		return
	}
	// Call db api module to handle list docks request.
	m, err := d.GetParameters()
	if err != nil {
		reason := fmt.Sprintf("List docks failed: %s", err.Error())
		d.Ctx.Output.SetStatus(model.ErrorBadRequest)
		d.Ctx.Output.Body(model.ErrorBadRequestStatus(reason))
		log.Error(reason)
		return
	}
	result, err := db.C.ListDocksWithFilter(c.GetContext(d.Ctx), m)
	if err != nil {
		reason := fmt.Sprintf("List docks failed: %s", err.Error())
		d.Ctx.Output.SetStatus(model.ErrorBadRequest)
		d.Ctx.Output.Body(model.ErrorBadRequestStatus(reason))
		log.Error(reason)
		return
	}

	// Marshal the result.
	body, err := json.Marshal(result)
	if err != nil {
		reason := fmt.Sprintf("Marshal docks failed: %s", err.Error())
		d.Ctx.Output.SetStatus(model.ErrorInternalServer)
		d.Ctx.Output.Body(model.ErrorInternalServerStatus(reason))
		log.Error(reason)
		return
	}

	d.Ctx.Output.SetStatus(StatusOK)
	d.Ctx.Output.Body(body)
	return
}

// GetDock
func (d *DockPortal) GetDock() {
	if !policy.Authorize(d.Ctx, "dock:get") {
		return
	}
	id := d.Ctx.Input.Param(":dockId")
	result, err := db.C.GetDock(c.GetContext(d.Ctx), id)
	if err != nil {
		reason := fmt.Sprintf("Get dock failed: %s", err.Error())
		d.Ctx.Output.SetStatus(model.ErrorBadRequest)
		d.Ctx.Output.Body(model.ErrorBadRequestStatus(reason))
		log.Error(reason)
		return
	}

	// Marshal the result.
	body, err := json.Marshal(result)
	if err != nil {
		reason := fmt.Sprintf("Marshal dock failed: %s", err.Error())
		d.Ctx.Output.SetStatus(model.ErrorInternalServer)
		d.Ctx.Output.Body(model.ErrorInternalServerStatus(reason))
		log.Error(reason)
		return
	}

	d.Ctx.Output.SetStatus(StatusOK)
	d.Ctx.Output.Body(body)
	return
}
