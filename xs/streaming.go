// Package xs contains eXtended actions (xactions) except storage services
// (mirror, ec) and extensions (downloader, lru).
/*
 * Copyright (c) 2021, NVIDIA CORPORATION. All rights reserved.
 */
package xs

import (
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/cos"
	"github.com/NVIDIA/aistore/cmn/debug"
	"github.com/NVIDIA/aistore/hk"
	"github.com/NVIDIA/aistore/transport"
	"github.com/NVIDIA/aistore/transport/bundle"
	"github.com/NVIDIA/aistore/xaction"
	"github.com/NVIDIA/aistore/xreg"
)

//
// multi-object on-demand (transactional) xactions - common logic
//

const (
	doneSendingOpcode = 31415

	waitRegRecv   = 4 * time.Second
	waitUnregRecv = 2 * waitRegRecv

	maxNumInParallel = 256
)

type (
	streamingF struct {
		xreg.RenewBase
		xact cluster.Xact
		kind string
		dm   *bundle.DataMover
	}
	streamingX struct {
		xaction.DemandBase
		p     *streamingF
		err   atomic.Value
		wiCnt atomic.Int32
	}
)

///////////////////////////
// (common factory part) //
///////////////////////////

func (p *streamingF) Kind() string      { return p.kind }
func (p *streamingF) Get() cluster.Xact { return p.xact }

func (p *streamingF) WhenPrevIsRunning(xprev xreg.Renewable) (xreg.WPR, error) {
	debug.Assertf(false, "%s vs %s", p.Str(p.Kind()), xprev) // xreg.usePrev() must've returned true
	return xreg.WprUse, nil
}

func (p *streamingF) newDM(prefix string, recv transport.ReceiveObj, sizePDU int32) (err error) {
	// NOTE:
	// transport endpoint `trname` must be identical across all participating targets
	bmd := p.Args.T.Bowner().Get()
	trname := fmt.Sprintf("%s-%s-%s-%d", prefix, p.Bck.Provider, p.Bck.Name, bmd.Version)

	dmExtra := bundle.Extra{Multiplier: 1, SizePDU: sizePDU}
	p.dm, err = bundle.NewDataMover(p.Args.T, trname, recv, cluster.RegularPut, dmExtra)
	if err != nil {
		return
	}
	if err = p.dm.RegRecv(); err != nil {
		var total time.Duration // retry for upto waitRegRecv
		glog.Error(err)
		sleep := cos.CalcProbeFreq(waitRegRecv)
		for err != nil && transport.IsErrDuplicateTrname(err) && total < waitRegRecv {
			time.Sleep(sleep)
			total += sleep
			err = p.dm.RegRecv()
		}
	}
	return
}

///////////////////////////
// (common xaction part) //
///////////////////////////

// limited pre-run abort
func (r *streamingX) TxnAbort() {
	err := cmn.NewErrAborted(r.Name(), "txn-abort", nil)
	if r.p.dm.IsOpen() {
		r.p.dm.Close(err)
	}
	r.p.dm.UnregRecv()
	r.XactBase.Finish(err)
}

func (r *streamingX) Stats() cluster.XactStats { return r.DemandBase.ExtStats() }

func (r *streamingX) raiseErr(err error, errCode int, contOnErr bool) {
	if cmn.IsErrAborted(err) {
		if verbose {
			glog.Warningf("%s[%s] aborted", r.p.T.Snode(), r)
		}
		return
	}
	errDetailed := fmt.Errorf("%s[%s]: %v(%d)", r.p.T.Snode(), r, err, errCode)
	if contOnErr {
		glog.Warningf("%v - ignoring...", errDetailed)
		return
	}
	glog.Errorf("%v - terminating...", errDetailed)
	r.err.Store(errDetailed)
}

// send EOI (end of iteration)
func (r *streamingX) eoi(uuid string, tsi *cluster.Snode) {
	o := transport.AllocSend()
	o.Hdr.Opcode = doneSendingOpcode
	o.Hdr.Opaque = []byte(uuid)
	if tsi != nil {
		r.p.dm.Send(o, nil, tsi) // to the responsible target
	} else {
		r.p.dm.Bcast(o) // to all
	}
}

func (r *streamingX) fin(err error) error {
	r.DemandBase.Stop()
	if err == nil {
		if errDetailed := r.err.Load(); errDetailed != nil {
			err = errDetailed.(error)
		} else if r.Aborted() {
			err = cmn.NewErrAborted(r.Name(), "", nil)
		}
	}
	r.p.dm.Close(err)
	hk.Reg(r.ID(), r.unreg, waitUnregRecv)
	r.Finish(err)
	return err
}

func (r *streamingX) unreg() (d time.Duration) {
	d = hk.UnregInterval
	if r.wiCnt.Load() > 0 {
		d = waitUnregRecv
	}
	return
}
