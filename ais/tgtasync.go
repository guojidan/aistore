// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"fmt"
	"net/http"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/xaction"
)

// List objects returns a list of objects in a bucket (with optional prefix)
// Special case:
// If URL contains cachedonly=true then the function returns the list of
// locally cached objects. Paging is used to return a long list of objects
func (t *targetrunner) listObjects(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, actionMsg *aisMsg) (ok bool) {
	query := r.URL.Query()
	if glog.FastV(4, glog.SmoduleAIS) {
		pid := query.Get(cmn.URLParamProxyID)
		glog.Infof("%s %s <= (%s)", r.Method, bck, pid)
	}

	var msg cmn.SelectMsg
	if err := cmn.TryUnmarshal(actionMsg.Value, &msg); err != nil {
		err := fmt.Errorf("unable to unmarshal 'value' in request to a cmn.SelectMsg: %v", actionMsg.Value)
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	ok = t.doAsync(w, r, actionMsg.Action, bck, &msg)
	return
}

func (t *targetrunner) bucketSummary(w http.ResponseWriter, r *http.Request, bck *cluster.Bck, actionMsg *aisMsg) (ok bool) {
	query := r.URL.Query()
	if glog.FastV(4, glog.SmoduleAIS) {
		pid := query.Get(cmn.URLParamProxyID)
		glog.Infof("%s %s <= (%s)", r.Method, bck, pid)
	}

	var msg cmn.SelectMsg
	if err := cmn.TryUnmarshal(actionMsg.Value, &msg); err != nil {
		err := fmt.Errorf("unable to unmarshal 'value' in request to a cmn.SelectMsg: %v", actionMsg.Value)
		t.invalmsghdlr(w, r, err.Error())
		return
	}
	ok = t.doAsync(w, r, actionMsg.Action, bck, &msg)
	return
}

// asynchronous bucket request
// - creates a new task that runs in background
// - returns status of a running task by its ID
// - returns the result of a task by its ID
func (t *targetrunner) doAsync(w http.ResponseWriter, r *http.Request, action string, bck *cluster.Bck, msg *cmn.SelectMsg) bool {
	var (
		query      = r.URL.Query()
		taskAction = query.Get(cmn.URLParamTaskAction)
		silent, _  = cmn.ParseBool(query.Get(cmn.URLParamSilent))
		ctx        = t.contextWithAuth(r.Header)
		// create task call
	)
	if taskAction == cmn.TaskStart {
		var (
			err error
		)

		switch action {
		case cmn.ActListObjects:
			_, err = xaction.Registry.RenewBckListXact(ctx, t, bck, msg)
		case cmn.ActSummaryBucket:
			_, err = xaction.Registry.RenewBckSummaryXact(ctx, t, bck, msg)
		default:
			t.invalmsghdlr(w, r, fmt.Sprintf("invalid action: %s", action))
			return false
		}

		if err != nil {
			t.invalmsghdlr(w, r, err.Error(), http.StatusInternalServerError)
			return false
		}

		w.WriteHeader(http.StatusAccepted)
		return true
	}

	xactStats, ok := xaction.Registry.GetXact(msg.TaskID)
	// task never started
	if !ok {
		s := fmt.Sprintf("Task %s not found", msg.TaskID)
		if silent {
			t.invalmsghdlrsilent(w, r, s, http.StatusNotFound)
		} else {
			t.invalmsghdlr(w, r, s, http.StatusNotFound)
		}
		return false
	}
	// task still running
	if !xactStats.Get().Finished() {
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	// task has finished
	result, err := xactStats.Get().Result()
	if err != nil {
		if cmn.IsErrBucketNought(err) {
			t.invalmsghdlr(w, r, err.Error(), http.StatusGone)
		} else {
			t.invalmsghdlr(w, r, err.Error())
		}
		return false
	}

	switch action {
	case cmn.ActListObjects:
		if !msg.Fast {
			break
		}
		if bckList, ok := result.(*cmn.BucketList); ok && bckList != nil {
			const minLoaded = 10 // check that many randomly-selected
			if len(bckList.Entries) > minLoaded {
				go func(bckEntries []*cmn.BucketEntry) {
					var (
						l      = len(bckEntries)
						m      = l / minLoaded
						loaded int
					)
					if l < minLoaded {
						return
					}
					for i := 0; i < l; i += m {
						lom := &cluster.LOM{T: t, ObjName: bckEntries[i].Name}
						err := lom.Init(bck.Bck)
						if err == nil && lom.IsLoaded() { // loaded?
							loaded++
						}
					}
					renew := loaded < minLoaded/2
					if glog.FastV(4, glog.SmoduleAIS) {
						glog.Errorf("%s: loaded %d/%d, renew=%t", t.si, loaded, minLoaded, renew)
					}
					if renew {
						xaction.Registry.RenewBckLoadLomCache(t, bck)
					}
				}(bckList.Entries)
			}
		}
	default:
		break
	}

	if taskAction == cmn.TaskResult {
		// return the final result only if it is requested explicitly
		body := cmn.MustMarshal(result)
		return t.writeJSON(w, r, body, "")
	}

	return true
}
