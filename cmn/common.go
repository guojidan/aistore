// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"bytes"
	"fmt"
	"hash"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/OneOfOne/xxhash"
	jsoniter "github.com/json-iterator/go"
)

const (
	KiB = 1024
	MiB = 1024 * KiB
	GiB = 1024 * MiB
	TiB = 1024 * GiB

	DefaultBufSize = 32 * KiB
)

var toBiBytes = map[string]int64{
	"K":   KiB,
	"KB":  KiB,
	"KIB": KiB,
	"M":   MiB,
	"MB":  MiB,
	"MIB": MiB,
	"G":   GiB,
	"GB":  GiB,
	"GIB": GiB,
	"T":   TiB,
	"TB":  TiB,
	"TIB": TiB,
}

const (
	DoesNotExist = "does not exist"
	NotSupported = "not supported yet"
	NoMountpaths = "no mountpaths"

	GCS_URL     = "http://storage.googleapis.com"
	GCS_URL_ALT = "http://www.googleapis.com"
)

type (
	StringSet map[string]struct{}
	SimpleKVs map[string]string
	PairF32   struct {
		Min float32
		Max float32
	}
	PairU32 struct {
		Min uint32
		Max uint32
	}
)

//
// PairU32 & PairF32
//
func (src *PairU32) CopyTo(dst *PairU32) {
	atomic.StoreUint32(&dst.Min, atomic.LoadUint32(&src.Min))
	atomic.StoreUint32(&dst.Max, atomic.LoadUint32(&src.Max))
}

func (upair *PairU32) Init(f float32) {
	u := math.Float32bits(f)
	atomic.StoreUint32(&upair.Max, u)
	atomic.StoreUint32(&upair.Min, u)
}

func (upair *PairU32) U2F() (fpair PairF32) {
	min := atomic.LoadUint32(&upair.Min)
	fpair.Min = math.Float32frombits(min)
	max := atomic.LoadUint32(&upair.Max)
	fpair.Max = math.Float32frombits(max)
	return
}

func (fpair PairF32) String() string {
	if fpair.Min == 0 && fpair.Max == 0 {
		return "()"
	}
	return fmt.Sprintf("(%.2f, %.2f)", fpair.Min, fpair.Max)
}

//
// common utils
//

func S2B(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.ToUpper(s)
	for k, v := range toBiBytes {
		if ns := strings.TrimSuffix(s, k); ns != s {
			f, err := strconv.ParseFloat(strings.TrimSpace(ns), 64)
			return int64(float64(v) * f), err
		}
	}
	ns := strings.TrimSuffix(s, "B")
	f, err := strconv.ParseFloat(strings.TrimSpace(ns), 64)
	return int64(f), err
}

func B2S(b int64, digits int) string {
	if b >= TiB {
		return fmt.Sprintf("%.*f%s", digits, float32(b)/float32(TiB), "TiB")
	}
	if b >= GiB {
		return fmt.Sprintf("%.*f%s", digits, float32(b)/float32(GiB), "GiB")
	}
	if b >= MiB {
		return fmt.Sprintf("%.*f%s", digits, float32(b)/float32(MiB), "MiB")
	}
	if b >= KiB {
		return fmt.Sprintf("%.*f%s", digits, float32(b)/float32(KiB), "KiB")
	}
	return fmt.Sprintf("%dB", b)
}

func (ss StringSet) String() string {
	keys := make([]string, len(ss))
	idx := 0
	for key := range ss {
		keys[idx] = key
		idx++
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

const assertMsg = "assertion failed"

// NOTE: not to be used in the datapath - consider instead one of the 3 flavors below
func AssertFmt(cond bool, args ...interface{}) {
	if cond {
		return
	}
	var message = assertMsg
	if len(args) > 0 {
		message += ": "
		for i := 0; i < len(args); i++ {
			message += fmt.Sprintf("%#v ", args[i])
		}
	}
	panic(message)
}

// this and the other two asserts get inlined and optimized
func Assert(cond bool) {
	if !cond {
		panic(assertMsg)
	}
}

// NOTE: preferable usage is to have the 'if' in the calling code:
//       if (!cond) { AssertMsg(false, msg) }
// - otherwise the message (e.g. Sprintf) may get evaluated every time
func AssertMsg(cond bool, msg string) {
	if !cond {
		panic(assertMsg + ": " + msg)
	}
}
func AssertNoErr(err error) {
	if err != nil {
		panic(err)
	}
}

func StringInSlice(s string, arr []string) bool {
	for _, el := range arr {
		if el == s {
			return true
		}
	}
	return false
}

func CopyStruct(dst interface{}, src interface{}) {
	x := reflect.ValueOf(src)
	if x.Kind() == reflect.Ptr {
		starX := x.Elem()
		y := reflect.New(starX.Type())
		starY := y.Elem()
		starY.Set(starX)
		reflect.ValueOf(dst).Elem().Set(y.Elem())
	} else {
		dst = x.Interface()
	}
}

//
// files, IO, hash
//

var (
	_ ReadOpenCloser = &FileHandle{}
	_ ReadSizer      = &SizedReader{}
)

type (
	ReadOpenCloser interface {
		io.ReadCloser
		Open() (io.ReadCloser, error)
	}

	// ReadSizer is the interface that adds Size method to the basic reader.
	ReadSizer interface {
		io.Reader
		Size() int64
	}

	FileHandle struct {
		*os.File
		fqn string
	}

	// SizedReader is simple struct which implements ReadSizer interface.
	SizedReader struct {
		io.Reader
		size int64
	}
)

func NewFileHandle(fqn string) (*FileHandle, error) {
	file, err := os.Open(fqn)
	if err != nil {
		return nil, err
	}

	return &FileHandle{file, fqn}, nil
}

func (f *FileHandle) Open() (io.ReadCloser, error) {
	return os.Open(f.fqn)
}

func NewSizedReader(r io.Reader, size int64) *SizedReader {
	return &SizedReader{r, size}
}

func (f *SizedReader) Size() int64 {
	return f.size
}

// CreateDir creates directory if does not exists. Does not return error when
// directory already exists.
func CreateDir(dir string) error {
	return os.MkdirAll(dir, 0755)
}

// CreateFile creates file and ensures that the directories for the file will be
// created if they do not yet exist.
func CreateFile(fname string) (*os.File, error) {
	if err := CreateDir(filepath.Dir(fname)); err != nil {
		return nil, err
	}
	return os.Create(fname)
}

// MvFile renames file ensuring that the directory of dst exists. Creates
// destination directory when it does not exist.
func MvFile(src, dst string) error {
	if err := CreateDir(filepath.Dir(dst)); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

func CopyFile(src, dst string, buf []byte) (err error) {
	var (
		reader *os.File
		writer *os.File
	)
	if reader, err = os.Open(src); err != nil {
		glog.Errorf("Failed to open %s: %v", src, err)
		return
	}
	if writer, err = CreateFile(dst); err != nil {
		glog.Errorf("Failed to create %s: %v", dst, err)
		reader.Close()
		return
	}
	if _, err = io.CopyBuffer(writer, reader, buf); err != nil {
		glog.Errorf("Failed to copy %s -> %s: %v", src, dst, err)
	}
	writer.Close()
	reader.Close()
	return
}

func PathWalkErr(err error) string {
	if os.IsNotExist(err) {
		return ""
	}
	return fmt.Sprintf("filepath-walk invoked with err: %v", err)
}

// Saves the reader directly to a local file
// `size` is an optional argument, if it is set only first `size` bytes
// are saved to the file
func SaveReader(fqn string, reader io.Reader, buf []byte, size ...int64) error {
	file, err := CreateFile(fqn)
	if err != nil {
		return err
	}

	if len(size) != 0 {
		sz := size[0]
		_, err = io.CopyBuffer(file, io.LimitReader(reader, sz), buf)
	} else {
		_, err = io.CopyBuffer(file, reader, buf)
	}

	file.Close()
	if err != nil {
		return fmt.Errorf("failed to save to %q: %v", fqn, err)
	}

	return nil
}

// Saves the reader to a temporary file `tmpfqn`, and if everything is OK
// it moves the temporary file to a given `fqn`
// `size` is an optional argument, if it is set only first `size` bytes
// are saved to the file
func SaveReaderSafe(tmpfqn, fqn string, reader io.Reader, buf []byte, size ...int64) error {
	if fqn == "" {
		return nil
	}

	if err := SaveReader(tmpfqn, reader, buf, size...); err != nil {
		return err
	}

	if err := MvFile(tmpfqn, fqn); err != nil {
		return err
	}
	return nil
}

// WriteWithHash reads data from an io.Reader, writes data to an io.Writer and computes
// xxHash on the data.
func WriteWithHash(w io.Writer, r io.Reader, buf []byte) (int64, string, error) {
	h := xxhash.New64()
	mw := io.MultiWriter(h, w)
	total, err := io.CopyBuffer(mw, r, buf)
	return total, HashToStr(h), err
}

func ReceiveAndChecksum(w io.Writer, r io.Reader, buf []byte, hashes ...hash.Hash) (written int64, err error) {
	var writer io.Writer
	if len(hashes) == 0 {
		writer = w
	} else {
		writers := make([]io.Writer, len(hashes)+1)
		for i, h := range hashes {
			writers[i] = h
		}
		writers[len(hashes)] = w
		writer = io.MultiWriter(writers...)
	}
	written, err = io.CopyBuffer(writer, r, buf)
	return
}

func ComputeXXHash(reader io.Reader, buf []byte) (csum string, errstr string) {
	var err error
	var xx hash.Hash = xxhash.New64()
	_, err = io.CopyBuffer(xx.(io.Writer), reader, buf)
	if err != nil {
		return "", fmt.Sprintf("Failed to copy buffer, err: %v", err)
	}
	return HashToStr(xx), ""
}

func ParseIntRanged(str string, base, bits int, low, high int64) (int64, error) {
	Assert(low <= high)
	v, err := strconv.ParseInt(str, base, bits)
	if err != nil {
		return low, err
	}

	if v < low || v > high {
		if low == high {
			return low, fmt.Errorf("only %d is supported", low)
		}
		return low, fmt.Errorf("it must be between %d and %d", low, high)
	}

	return v, nil
}

//===========================================================================
//
// local (config) save and restore - NOTE: caller is responsible to serialize
//
//===========================================================================
func LocalSave(pathname string, v interface{}) error {
	tmp := pathname + ".tmp"
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	b, err := jsoniter.MarshalIndent(v, "", " ")
	if err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	r := bytes.NewReader(b)
	_, err = io.Copy(file, r)
	errclose := file.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if errclose != nil {
		_ = os.Remove(tmp)
		return err
	}
	err = os.Rename(tmp, pathname)
	return err
}

func LocalLoad(pathname string, v interface{}) (err error) {
	file, err := os.Open(pathname)
	if err != nil {
		return
	}
	err = jsoniter.NewDecoder(file).Decode(v)
	_ = file.Close()
	return
}

func Ratio(high, low, curr int64) float32 {
	Assert(high > low && high <= 100 && low < 100 && low > 0)
	if curr <= low {
		return 0
	}
	if curr >= high {
		return 1
	}
	return float32(curr-low) / float32(high-low)
}
