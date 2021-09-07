// Package downloader implements functionality to download resources into AIS cluster from external source.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/fs"
	"golang.org/x/sync/errgroup"
)

// Dispatcher serves as middle layer between receiving download requests
// and serving them to joggers which actually download objects from a remote location.

type (
	dispatcher struct {
		parent *Downloader

		startupSema startupSema        // Semaphore which synchronizes goroutines at dispatcher startup.
		joggers     map[string]*jogger // mpath -> jogger

		mtx      sync.RWMutex           // Protects map defined below.
		abortJob map[string]*cos.StopCh // jobID -> abort job chan

		downloadCh chan DlJob

		stopCh *cos.StopCh
	}

	startupSema struct {
		started atomic.Bool
	}
)

func newDispatcher(parent *Downloader) *dispatcher {
	initInfoStore(parent.t.DB()) // it will be initialized only once

	return &dispatcher{
		parent: parent,

		startupSema: startupSema{},
		joggers:     make(map[string]*jogger, 8),

		downloadCh: make(chan DlJob),

		stopCh:   cos.NewStopCh(),
		abortJob: make(map[string]*cos.StopCh, 100),
	}
}

func (d *dispatcher) run() (err error) {
	var (
		// Number of concurrent job dispatches - it basically limits the number
		// of goroutines so they won't go out of hand.
		sema       = cos.NewSemaphore(5 * fs.NumAvail())
		group, ctx = errgroup.WithContext(context.Background())
	)

	availablePaths, _ := fs.Get()
	for mpath := range availablePaths {
		d.addJogger(mpath)
	}

	// Release semaphore and allow other goroutines to work.
	d.startupSema.markStarted()

Loop:
	for {
		select {
		case <-d.parent.IdleTimer():
			glog.Infof("%s has timed out. Exiting...", d.parent.Name())
			break Loop
		case <-d.parent.ChanAbort():
			glog.Infof("%s has been aborted. Exiting...", d.parent.Name())
			break Loop
		case <-ctx.Done():
			break Loop
		case job := <-d.downloadCh:
			// Start dispatching each job in new goroutine to make sure that
			// all joggers are busy downloading the tasks (jobs with limits
			// may not saturate the full downloader throughput).
			d.mtx.Lock()
			d.abortJob[job.ID()] = cos.NewStopCh()
			d.mtx.Unlock()

			select {
			case <-d.parent.IdleTimer():
				glog.Infof("%s has timed out. Exiting...", d.parent.Name())
				break Loop
			case <-d.parent.ChanAbort():
				glog.Infof("%s has been aborted. Exiting...", d.parent.Name())
				break Loop
			case <-ctx.Done():
				break Loop
			case <-sema.TryAcquire():
				group.Go(func() error {
					defer sema.Release()
					if !d.dispatchDownload(job) {
						return cmn.NewErrAborted(job.String(), "download", nil)
					}
					return nil
				})
			}
		}
	}

	d.stop()
	return group.Wait()
}

// stop running joggers
// no need to cleanup maps, dispatcher should not be used after stop()
func (d *dispatcher) stop() {
	d.stopCh.Close()
	for _, jogger := range d.joggers {
		jogger.stop()
	}
}

func (d *dispatcher) addJogger(mpath string) {
	if _, ok := d.joggers[mpath]; ok {
		glog.Warningf("Attempted to add an already existing mountpath %q", mpath)
		return
	}
	mpathInfo, _ := fs.Path2MpathInfo(mpath)
	if mpathInfo == nil {
		glog.Errorf("Attempted to add a mountpath %q with no corresponding filesystem", mpath)
		return
	}
	j := newJogger(d, mpath)
	go j.jog()
	d.joggers[mpath] = j
}

func (d *dispatcher) cleanupJob(jobID string) {
	d.mtx.Lock()
	if ch, exists := d.abortJob[jobID]; exists {
		ch.Close()
		delete(d.abortJob, jobID)
	}
	d.mtx.Unlock()
}

// forward request to designated jogger
func (d *dispatcher) dispatchDownload(job DlJob) (ok bool) {
	defer func() {
		debug.Infof("[downloader] Finished dispatching job %q, now waiting for it to finish and cleanup", job.ID())
		d.waitFor(job.ID())
		debug.Infof("[downloader] Job %q has finished waiting for all tasks to finish", job.ID())
		d.cleanupJob(job.ID())
		debug.Infof("[downloader] Cleaned up after job %q", job.ID())
		job.cleanup()
		debug.Infof("[downloader] Job %q has fully finished", job.ID())
	}()

	if aborted := d.checkAborted(); aborted || d.checkAbortedJob(job) {
		return !aborted
	}

	diffResolver := NewDiffResolver(nil)

	diffResolver.Start()

	// In case of `!job.Sync()` we don't want to traverse the whole bucket.
	// We just want to download requested objects so we know exactly which
	// objects must be checked (compared) and which not. Therefore, only traverse
	// bucket when we need to sync the objects.
	if job.Sync() {
		go func() {
			defer diffResolver.CloseSrc()

			err := fs.WalkBck(&fs.WalkBckOptions{
				Options: fs.Options{
					Bck: job.Bck(),
					CTs: []string{fs.ObjectType},
					Callback: func(fqn string, de fs.DirEntry) error {
						if diffResolver.Stopped() {
							return cmn.NewErrAborted(job.String(), "diff-resolver stopped", nil)
						}
						lom := &cluster.LOM{FQN: fqn}
						if err := lom.Init(job.Bck()); err != nil {
							return err
						}
						if !job.checkObj(lom.ObjName) {
							return nil
						}
						diffResolver.PushSrc(lom)
						return nil
					},
					Sorted: true,
				},
			})
			if err != nil && !cmn.IsErrAborted(err) {
				diffResolver.Abort(err)
			}
		}()
	}

	go func() {
		defer func() {
			diffResolver.CloseDst()
			if !job.Sync() {
				diffResolver.CloseSrc()
			}
		}()

		for {
			objs, ok, err := job.genNext()
			if err != nil {
				diffResolver.Abort(err)
				return
			}
			if !ok || diffResolver.Stopped() {
				return
			}

			for _, obj := range objs {
				if d.checkAborted() {
					err := cmn.NewErrAborted(job.String(), "", nil)
					diffResolver.Abort(err)
					return
				} else if d.checkAbortedJob(job) {
					diffResolver.Stop()
					return
				}

				if !job.Sync() {
					// When it is not a sync job, push LOM for a given object
					// because we need to check if it exists.
					lom := &cluster.LOM{ObjName: obj.objName}
					if err := lom.Init(job.Bck()); err != nil {
						diffResolver.Abort(err)
						return
					}
					diffResolver.PushSrc(lom)
				}

				if obj.link != "" {
					diffResolver.PushDst(&WebResource{
						ObjName: obj.objName,
						Link:    obj.link,
					})
				} else {
					diffResolver.PushDst(&BackendResource{
						ObjName: obj.objName,
					})
				}
			}
		}
	}()

	for {
		result, err := diffResolver.Next()
		if err != nil {
			return false
		}
		switch result.Action {
		case DiffResolverRecv, DiffResolverSkip, DiffResolverErr, DiffResolverDelete:
			var obj dlObj
			if dst := result.Dst; dst != nil {
				obj = dlObj{
					objName:    dst.ObjName,
					link:       dst.Link,
					fromRemote: dst.Link == "",
				}
			} else {
				src := result.Src
				cos.Assert(result.Action == DiffResolverDelete)
				obj = dlObj{
					objName:    src.ObjName,
					link:       "",
					fromRemote: true,
				}
			}

			dlStore.incScheduled(job.ID())

			if result.Action == DiffResolverSkip {
				dlStore.incSkipped(job.ID())
				continue
			}

			t := &singleObjectTask{
				parent: d.parent,
				obj:    obj,
				job:    job,
			}

			if result.Action == DiffResolverErr {
				t.markFailed(result.Err.Error())
				continue
			}

			if result.Action == DiffResolverDelete {
				cos.Assert(job.Sync())
				if _, err := d.parent.t.EvictObject(result.Src); err != nil {
					t.markFailed(err.Error())
				} else {
					dlStore.incFinished(job.ID())
				}
				continue
			}

			ok, err := d.blockingDispatchDownloadSingle(t)
			if err != nil {
				glog.Errorf("%s failed to download %s: %v", job, obj.objName, err)
				dlStore.setAborted(job.ID()) // TODO -- FIXME: pass (report, handle) error, here and elsewhere
				return ok
			}
			if !ok {
				dlStore.setAborted(job.ID())
				return false
			}
		case DiffResolverSend:
			cos.Assert(job.Sync())
		case DiffResolverEOF:
			dlStore.setAllDispatched(job.ID(), true)
			return true
		}
	}
}

func (d *dispatcher) jobAbortedCh(jobID string) *cos.StopCh {
	d.mtx.RLock()
	defer d.mtx.RUnlock()
	if abCh, ok := d.abortJob[jobID]; ok {
		return abCh
	}

	// Channel always sending something if entry in the map is missing.
	abCh := cos.NewStopCh()
	abCh.Close()
	return abCh
}

func (d *dispatcher) checkAbortedJob(job DlJob) bool {
	select {
	case <-d.jobAbortedCh(job.ID()).Listen():
		return true
	default:
		return false
	}
}

func (d *dispatcher) checkAborted() bool {
	select {
	case <-d.stopCh.Listen():
		return true
	default:
		return false
	}
}

// returns false if dispatcher encountered hard error, true otherwise
func (d *dispatcher) blockingDispatchDownloadSingle(task *singleObjectTask) (ok bool, err error) {
	bck := cluster.NewBckEmbed(task.job.Bck())
	if err := bck.Init(d.parent.t.Bowner()); err != nil {
		return true, err
	}

	mi, _, err := cluster.HrwMpath(bck.MakeUname(task.obj.objName))
	if err != nil {
		return false, err
	}
	jogger, ok := d.joggers[mi.Path]
	if !ok {
		err := fmt.Errorf("no jogger for mpath %s exists", mi.Path)
		return false, err
	}

	// NOTE: Throttle job before making jogger busy - we don't want to clog the
	//  jogger as other tasks from other jobs can be already ready to download.
	select {
	case <-task.job.throttler().tryAcquire():
		break
	case <-d.jobAbortedCh(task.job.ID()).Listen():
		return true, nil
	}

	// Secondly, try to push the new task into queue.
	select {
	// TODO -- FIXME: currently, dispatcher halts if any given jogger is "full" but others available
	case jogger.putCh(task) <- task:
		return true, nil
	case <-d.jobAbortedCh(task.job.ID()).Listen():
		task.job.throttler().release()
		return true, nil
	case <-d.stopCh.Listen():
		task.job.throttler().release()
		return false, nil
	}
}

func (d *dispatcher) dispatchAdminReq(req *request) (resp interface{}, statusCode int, err error) {
	debug.Infof("[downloader] Dispatching admin request (id: %q, action: %q, onlyActive: %t)", req.id, req.action, req.onlyActive)
	defer debug.Infof("[downloader] Finished admin request (id: %q, action: %q, onlyActive: %t)", req.id, req.action, req.onlyActive)

	// Need to make sure that the dispatcher has fully initialized and started,
	// and it's ready for processing the requests.
	d.startupSema.waitForStartup()

	switch req.action {
	case actStatus:
		d.handleStatus(req)
	case actAbort:
		d.handleAbort(req)
	case actRemove:
		d.handleRemove(req)
	case actList:
		_handleList(req)
	default:
		cos.Assertf(false, "%v; %v", req, req.action)
	}
	r := req.response
	return r.value, r.statusCode, r.err
}

func (d *dispatcher) handleRemove(req *request) {
	jInfo, err := d.parent.checkJob(req)
	if err != nil {
		return
	}

	// There's a slight chance this doesn't happen if target rejoins after target checks for download not running
	dlInfo := jInfo.ToDlJobInfo()
	if dlInfo.JobRunning() {
		req.writeErrResp(fmt.Errorf("download job with id = %s is still running", jInfo.ID), http.StatusBadRequest)
		return
	}

	dlStore.delJob(req.id)
	req.writeResp(nil)
}

func (d *dispatcher) handleAbort(req *request) {
	_, err := d.parent.checkJob(req)
	if err != nil {
		return
	}

	d.jobAbortedCh(req.id).Close()

	for _, j := range d.joggers {
		j.abortJob(req.id)
	}

	dlStore.setAborted(req.id)
	req.writeResp(nil)
}

func (d *dispatcher) handleStatus(req *request) {
	var (
		finishedTasks []TaskDlInfo
		dlErrors      []TaskErrInfo
	)

	jInfo, err := d.parent.checkJob(req)
	if err != nil {
		return
	}

	currentTasks := d.activeTasks(req.id)
	if !req.onlyActive {
		finishedTasks, err = dlStore.getTasks(req.id)
		if err != nil {
			req.writeErrResp(err, http.StatusInternalServerError)
			return
		}

		dlErrors, err = dlStore.getErrors(req.id)
		if err != nil {
			req.writeErrResp(err, http.StatusInternalServerError)
			return
		}
		sort.Sort(TaskErrByName(dlErrors))
	}

	req.writeResp(&DlStatusResp{
		DlJobInfo:     jInfo.ToDlJobInfo(),
		CurrentTasks:  currentTasks,
		FinishedTasks: finishedTasks,
		Errs:          dlErrors,
	})
}

func _handleList(req *request) {
	records := dlStore.getList(req.regex)
	respMap := make(map[string]DlJobInfo)
	for _, r := range records {
		respMap[r.ID] = r.ToDlJobInfo()
	}

	req.writeResp(respMap)
}

func (d *dispatcher) activeTasks(reqID string) []TaskDlInfo {
	currentTasks := make([]TaskDlInfo, 0, len(d.joggers))
	for _, j := range d.joggers {
		task := j.getTask()
		if task != nil && task.jobID() == reqID {
			currentTasks = append(currentTasks, task.ToTaskDlInfo())
		}
	}

	sort.Sort(TaskInfoByName(currentTasks))
	return currentTasks
}

// pending returns `true` if any joggers has pending tasks for a given `reqID`,
// `false` otherwise.
func (d *dispatcher) pending(jobID string) bool {
	for _, j := range d.joggers {
		if j.pending(jobID) {
			return true
		}
	}
	return false
}

// PRECONDITION: All tasks should be dispatched.
func (d *dispatcher) waitFor(jobID string) {
	for ; ; time.Sleep(time.Second) {
		if !d.pending(jobID) {
			return
		}
	}
}

func (ss *startupSema) markStarted() {
	ss.started.Store(true)
}

func (ss *startupSema) waitForStartup() {
	if ss.started.Load() {
		return
	}

	const (
		sleep   = 500 * time.Millisecond
		timeout = 10 * time.Second
	)
	for slept := time.Duration(0); !ss.started.Load(); slept += sleep {
		time.Sleep(sleep)

		// If we are sleeping for more than `timeout` then there is something
		// wrong. This should never happen even on the slowest machines.
		cos.AssertMsg(slept < timeout, "dispatcher takes impossible time to start")
	}
}
