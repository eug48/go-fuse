package fuse

import (
	"bufio"
	"fmt"
	"github.com/hanwen/go-fuse/fuse"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

var CheckSuccess = fuse.CheckSuccess
var delay = 0 * time.Microsecond


type StatFs struct {
	fuse.DefaultFileSystem
	entries map[string]*fuse.Attr
	dirs    map[string][]fuse.DirEntry
	delay   time.Duration
}

func (me *StatFs) add(name string, a *fuse.Attr) {
	name = strings.TrimRight(name, "/")
	_, ok := me.entries[name]
	if ok {
		return
	}

	me.entries[name] = a
	if name == "/" || name == "" {
		return
	}

	dir, base := filepath.Split(name)
	dir = strings.TrimRight(dir, "/")
	me.dirs[dir] = append(me.dirs[dir], fuse.DirEntry{Name: base, Mode: a.Mode})
	me.add(dir, &fuse.Attr{Mode: fuse.S_IFDIR | 0755})
}

func (me *StatFs) GetAttr(name string, context *fuse.Context) (*fuse.Attr, fuse.Status) {
	e := me.entries[name]
	if e == nil {
		return nil, fuse.ENOENT
	}

	if me.delay > 0 {
		time.Sleep(me.delay)
	}
	return e, fuse.OK
}

func (me *StatFs) OpenDir(name string, context *fuse.Context) (stream []fuse.DirEntry, status fuse.Status) {
	entries := me.dirs[name]
	if entries == nil {
		return nil, fuse.ENOENT
	}
	return entries, fuse.OK
}

func NewStatFs() *StatFs {
	return &StatFs{
		entries: make(map[string]*fuse.Attr),
		dirs:    make(map[string][]fuse.DirEntry),
	}
}

func setupFs(fs fuse.FileSystem, opts *fuse.FileSystemOptions) (string, func()) {
	mountPoint, _ := ioutil.TempDir("", "stat_test")
	nfs := fuse.NewPathNodeFs(fs, nil)
	state, _, err := fuse.MountNodeFileSystem(mountPoint, nfs, opts)
	if err != nil {
		panic(fmt.Sprintf("cannot mount %v", err)) // ugh - benchmark has no error methods.
	}
	// state.Debug = true
	go state.Loop()

	return mountPoint, func() {
		err := state.Unmount()
		if err != nil {
			log.Println("error during unmount", err)
		} else {
			os.RemoveAll(mountPoint)
		}
	}
}

func TestNewStatFs(t *testing.T) {
	fs := NewStatFs()
	for _, n := range []string{
		"file.txt", "sub/dir/foo.txt",
		"sub/dir/bar.txt", "sub/marine.txt"} {
		fs.add(n, &fuse.Attr{Mode: fuse.S_IFREG | 0644})
	}

	wd, clean := setupFs(fs, nil)
	defer clean()

	names, err := ioutil.ReadDir(wd)
	CheckSuccess(err)
	if len(names) != 2 {
		t.Error("readdir /", names)
	}

	fi, err := os.Lstat(wd + "/sub")
	CheckSuccess(err)
	if !fi.IsDir() {
		t.Error("mode", fi)
	}
	names, err = ioutil.ReadDir(wd + "/sub")
	CheckSuccess(err)
	if len(names) != 2 {
		t.Error("readdir /sub", names)
	}
	names, err = ioutil.ReadDir(wd + "/sub/dir")
	CheckSuccess(err)
	if len(names) != 2 {
		t.Error("readdir /sub/dir", names)
	}

	fi, err = os.Lstat(wd + "/sub/marine.txt")
	CheckSuccess(err)
	if fi.Mode()&os.ModeType != 0 {
		t.Error("mode", fi)
	}
}

func GetTestLines() []string {
	wd, _ := os.Getwd()
	// Names from OpenJDK 1.6
	fn := wd + "/testpaths.txt"

	f, err := os.Open(fn)
	CheckSuccess(err)

	defer f.Close()
	r := bufio.NewReader(f)

	l := []string{}
	for {
		line, _, err := r.ReadLine()
		if line == nil || err != nil {
			break
		}

		fn := string(line)
		l = append(l, fn)
	}
	return l
}

func BenchmarkGoFuseThreadedStat(b *testing.B) {
	b.StopTimer()
	fs := NewStatFs()
	fs.delay = delay
	files := GetTestLines()
	for _, fn := range files {
		fs.add(fn, &fuse.Attr{Mode: fuse.S_IFREG | 0644})
	}
	if len(files) == 0 {
		log.Fatal("no files added")
	}

	log.Printf("Read %d file names", len(files))

	ttl := 100 * time.Millisecond
	opts := fuse.FileSystemOptions{
		EntryTimeout:    0.0,
		AttrTimeout:     0.0,
		NegativeTimeout: 0.0,
	}
	wd, clean := setupFs(fs, &opts)
	defer clean()

	for i, l := range files {
		files[i] = filepath.Join(wd, l)
	}

	threads := runtime.GOMAXPROCS(0)
	results := TestingBOnePass(b, threads, time.Duration((ttl*120)/100), files)
	AnalyzeBenchmarkRuns("Go-FUSE", results)
}

func TestingBOnePass(b *testing.B, threads int, sleepTime time.Duration, files []string) (results []float64) {
	runtime.GC()
	todo := b.N
	for todo > 0 {
		if len(files) > todo {
			files = files[:todo]
		}
		b.StartTimer()
		result := BulkStat(threads, files)
		todo -= len(files)
		b.StopTimer()
		results = append(results, result)
	}
	return results
}

func BenchmarkCFuseThreadedStat(b *testing.B) {
	b.StopTimer()

	lines := GetTestLines()
	unique := map[string]int{}
	for _, l := range lines {
		unique[l] = 1
		dir, _ := filepath.Split(l)
		for dir != "/" && dir != "" {
			unique[dir] = 1
			dir = filepath.Clean(dir)
			dir, _ = filepath.Split(dir)
		}
	}

	out := []string{}
	for k := range unique {
		out = append(out, k)
	}

	f, err := ioutil.TempFile("", "")
	CheckSuccess(err)
	sort.Strings(out)
	for _, k := range out {
		f.Write([]byte(fmt.Sprintf("/%s\n", k)))
	}
	f.Close()

	mountPoint, _ := ioutil.TempDir("", "stat_test")
	wd, _ := os.Getwd()
	cmd := exec.Command(wd+"/cstatfs",
		"-o",
		"entry_timeout=0.0,attr_timeout=0.0,ac_attr_timeout=0.0,negative_timeout=0.0",
		mountPoint)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("STATFS_INPUT=%s", f.Name()),
		fmt.Sprintf("STATFS_DELAY_USEC=%d", delay / time.Microsecond))
	cmd.Start()

	bin, err := exec.LookPath("fusermount")
	CheckSuccess(err)
	stop := exec.Command(bin, "-u", mountPoint)
	CheckSuccess(err)
	defer stop.Run()

	for i, l := range lines {
		lines[i] = filepath.Join(mountPoint, l)
	}

	// Wait for the daemon to mount.
	time.Sleep(200 * time.Millisecond)
	ttl := time.Millisecond * 100
	threads := runtime.GOMAXPROCS(0)
	results := TestingBOnePass(b, threads, time.Duration((ttl*12)/10), lines)
	AnalyzeBenchmarkRuns("CFuse", results)
}
