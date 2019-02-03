// Package fs provides mountpath and FQN abstractions and methods to resolve/map stored content
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package fs

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
)

/*
 * Besides objects we must to deal with additional files like: workfiles, dsort
 * intermediate files (used when spilling to disk) or EC slices. These files can
 * have different rules of rebalancing, evicting and other processing. Each
 * content type needs to implement ContentResolver to reflect the rules and
 * permission for different services. To see how the interface can be
 * implemented see: DefaultWorkfile implemention.
 *
 * When walking through the files we need to know if the file is an object or
 * other content. To do that we generate fqn with GenContentFQN. It adds short
 * prefix to the base name, which we believe is unique and will separate objects
 * from content files. We parse the file type to run ParseUniqueFQN (implemented
 * by this file type) on the rest of the base name.
 */

const (
	ObjectType   = "obj"
	WorkfileType = "work"
)

type (
	ContentResolver interface {
		// When set to true, services like rebalance have permission to move
		// content for example to another target because it is misplaced (HRW).
		PermToMove() bool
		// When set to true, services like LRU have permission to evict/delete content
		PermToEvict() bool
		// When set to true, content can be checksumed, shown or processed in other ways.
		PermToProcess() bool

		// Generates unique base name for original one. This function may add
		// additional information to the base name.
		// prefix - user-defined marker
		GenUniqueFQN(base, prefix string) (ufqn string)
		// Parses generated unique fqn to the original one.
		ParseUniqueFQN(base string) (orig string, old bool, ok bool)
	}

	ContentInfo struct {
		Dir  string // Original directory
		Base string // Original base name of the file
		Old  bool   // Determines if the file is old or not
		Type string // Type of the workfile
	}

	ContentSpecMgr struct {
		RegisteredContentTypes map[string]ContentResolver
	}
)

var (
	pid  int64 = 0xDEADBEEF   // pid of the current process
	spid       = "0xDEADBEEF" // string version of the pid

	CSM = &ContentSpecMgr{RegisteredContentTypes: make(map[string]ContentResolver, 8)}
)

func init() {
	pid = int64(os.Getpid())
	spid = strconv.FormatInt(pid, 16)
}

// RegisterFileType registers new object and workfile type with given content resolver.
//
// NOTE: fileType should not contain dot since it is separator for additional
// info and parsing can fail.
//
// NOTE: FIXME: All registration must happen at the startup, otherwise panic can
// be expected.
func (f *ContentSpecMgr) RegisterFileType(contentType string, spec ContentResolver) error {
	if strings.Contains(contentType, "/") {
		return fmt.Errorf("file type %s should not contain dot '.'", contentType)
	}

	if _, ok := f.RegisteredContentTypes[contentType]; ok {
		return fmt.Errorf("file type %s is already registered", contentType)
	}
	f.RegisteredContentTypes[contentType] = spec

	// Create type subfolders (ec, obj, work, dsort) for each mountpath
	available, _ := Mountpaths.Get()
	for mpath := range available {
		if err := cmn.CreateDir(filepath.Join(mpath, contentType)); err != nil {
			return err
		}
	}

	return nil
}

// GenContentFQN returns a new fqn generated from given fqn. Generated fqn will
// contain additional info which will then speed up subsequent parsing
func (f *ContentSpecMgr) GenContentFQN(fqn, contentType, prefix string) string {
	parsedFQN, err := Mountpaths.FQN2Info(fqn)
	if err != nil {
		cmn.Assert(false, err)
		return ""
	}
	return f.GenContentParsedFQN(parsedFQN, contentType, prefix)
}

func (f *ContentSpecMgr) GenContentParsedFQN(parsedFQN FQNparsed, contentType, prefix string) (fqn string) {
	spec, ok := f.RegisteredContentTypes[contentType]
	if !ok {
		cmn.Assert(false, fmt.Sprintf("Invalid content type '%s'", contentType))
	}
	fqn = f.FQN(
		parsedFQN.MpathInfo,
		contentType,
		parsedFQN.IsLocal,
		parsedFQN.Bucket,
		spec.GenUniqueFQN(parsedFQN.Objname, prefix))
	return
}

// FileSpec returns the specification/attributes and information about fqn. spec
// and info are only set when fqn was generated by GenContentFQN.
func (f *ContentSpecMgr) FileSpec(fqn string) (resolver ContentResolver, info *ContentInfo) {
	dir, base := filepath.Split(fqn)
	if !strings.HasSuffix(dir, "/") || base == "" {
		return
	}
	parsedFQN, err := Mountpaths.FQN2Info(fqn)
	if err != nil {
		return
	}
	spec, found := f.RegisteredContentTypes[parsedFQN.ContentType]
	if !found {
		// Quite weird, seemed like workfile but in the end it isn't
		glog.Warningf("fqn: %q has not registered file type %s", fqn, parsedFQN.ContentType)
		return
	}
	origBase, old, ok := spec.ParseUniqueFQN(base)
	if !ok {
		return
	}
	resolver = spec
	info = &ContentInfo{Dir: dir, Base: origBase, Old: old, Type: parsedFQN.ContentType}
	return
}

func (f *ContentSpecMgr) FQN(mi *MountpathInfo, contentType string, isLocal bool, bucket, objName string) (fqn string) {
	if _, ok := f.RegisteredContentTypes[contentType]; !ok {
		cmn.Assert(false, fmt.Sprintf("contentType %s was not registered", contentType))
	}
	return mi.MakePathBucketObject(contentType, bucket, objName, isLocal)
}

func (f *ContentSpecMgr) PermToEvict(fqn string) (ok, isOld bool) {
	spec, info := f.FileSpec(fqn)
	if spec == nil {
		return true, false
	}

	return spec.PermToEvict(), info.Old
}

func (f *ContentSpecMgr) PermToMove(fqn string) (ok bool) {
	spec, _ := f.FileSpec(fqn)
	if spec == nil {
		return false
	}

	return spec.PermToMove()
}

func (f *ContentSpecMgr) PermToProcess(fqn string) (ok bool) {
	spec, _ := f.FileSpec(fqn)
	if spec == nil {
		return false
	}

	return spec.PermToProcess()
}

// FIXME: This should be probably placed somewhere else \/

type (
	ObjectContentResolver   struct{}
	WorkfileContentResolver struct{}
)

func (wf *ObjectContentResolver) PermToMove() bool    { return true }
func (wf *ObjectContentResolver) PermToEvict() bool   { return true }
func (wf *ObjectContentResolver) PermToProcess() bool { return true }

func (wf *ObjectContentResolver) GenUniqueFQN(base, prefix string) string {
	return base
}

func (wf *ObjectContentResolver) ParseUniqueFQN(base string) (orig string, old bool, ok bool) {
	return base, false, true
}

func (wf *WorkfileContentResolver) PermToMove() bool    { return false }
func (wf *WorkfileContentResolver) PermToEvict() bool   { return true }
func (wf *WorkfileContentResolver) PermToProcess() bool { return false }

func (wf *WorkfileContentResolver) GenUniqueFQN(base, prefix string) string {
	// append prefix to mark what created the workfile
	dir, fname := filepath.Split(base)
	fname = prefix + "." + fname
	base = filepath.Join(dir, fname)

	tieBreaker := strconv.FormatInt(time.Now().UnixNano(), 16)
	return base + "." + tieBreaker[5:] + "." + spid
}

func (wf *WorkfileContentResolver) ParseUniqueFQN(base string) (orig string, old bool, ok bool) {
	// remove original content type
	cntIndex := strings.Index(base, ".")
	if cntIndex < 0 {
		return "", false, false
	}
	base = base[cntIndex+1:]

	pidIndex := strings.LastIndex(base, ".") // pid
	if pidIndex < 0 {
		return "", false, false
	}
	tieIndex := strings.LastIndex(base[:pidIndex], ".") // tie breaker
	if tieIndex < 0 {
		return "", false, false
	}
	filePID, err := strconv.ParseInt(base[pidIndex+1:], 16, 64)
	if err != nil {
		return "", false, false
	}

	return base[:tieIndex], filePID != pid, true
}
