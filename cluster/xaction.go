// Package cluster provides local access to cluster-level metadata
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cluster

import (
	"sync"
	"time"

	"github.com/NVIDIA/aistore/cmn"
)

type QuiRes int

const (
	QuiInactiveCB = QuiRes(iota) // e.g., no pending requests (NOTE: used exclusively by `quicb` callbacks)
	QuiActive                    // active (e.g., receiving data)
	QuiActiveRet                 // active that immediately breaks waiting for quiecscence
	QuiDone                      // all done
	QuiAborted                   // aborted
	QuiTimeout                   // timeout
	Quiescent                    // idle => quiescent
)

type (
	QuiCB func(elapsed time.Duration) QuiRes // see enum below

	Xact interface {
		Run(*sync.WaitGroup)
		ID() string
		Kind() string
		Bck() *Bck
		StartTime() time.Time
		EndTime() time.Time
		ObjCount() int64
		BytesCount() int64
		Finished() bool
		Aborted() bool
		AbortedAfter(time.Duration) bool
		Quiesce(time.Duration, QuiCB) QuiRes
		ChanAbort() <-chan struct{}
		Result() (interface{}, error)
		Stats() XactStats

		// reporting: log, err
		String() string
		Name() string

		// modifiers
		Finish(err error)
		Abort()
		AddNotif(n Notif)

		BytesAdd(cnt int64) int64
		ObjectsInc() int64
		ObjectsAdd(cnt int64) int64
	}

	XactStats interface {
		ID() string
		Kind() string
		Bck() cmn.Bck
		StartTime() time.Time
		EndTime() time.Time
		ObjCount() int64
		BytesCount() int64
		Aborted() bool
		Running() bool
		Finished() bool
	}
)
