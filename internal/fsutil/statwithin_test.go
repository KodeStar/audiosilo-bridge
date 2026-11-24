package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// StatWithin answers as os.Stat does for a file system that answers.
func TestStatWithinAnswers(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "a.m4b")
	if err := os.WriteFile(p, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := StatWithin(p, 5*time.Second)
	if err != nil || info.Size() != 5 {
		t.Fatalf("StatWithin = %v, %v", info, err)
	}
	if _, err := StatWithin(filepath.Join(t.TempDir(), "gone"), 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file = %v, want ErrNotExist", err)
	}
}

// A stat that doesn't answer in time returns ErrStatTimeout, and a second ask
// of the same path waits on the stat already out instead of starting another;
// once it answers, the next ask starts afresh.
func TestStatWithinTimesOutAndSharesTheStatOut(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/book.m4b"
	c := &lookCall[fs.FileInfo]{done: make(chan struct{}), started: time.Now()}
	stats.mu.Lock()
	stats.calls[path] = c // a stat stuck on a hung mount
	stats.mu.Unlock()
	for range 3 {
		if _, err := StatWithin(path, 10*time.Millisecond); !errors.Is(err, ErrStatTimeout) {
			t.Fatalf("a hung stat = %v, want ErrStatTimeout", err)
		}
	}
	// A stat out longer than d already: a later ask doesn't wait d again.
	c.started = time.Now().Add(-time.Minute)
	began := time.Now()
	if _, err := StatWithin(path, 5*time.Second); !errors.Is(err, ErrStatTimeout) || time.Since(began) > time.Second {
		t.Fatalf("joining a stat out past d = %v after %v, want ErrStatTimeout at once", err, time.Since(began))
	}
	stats.mu.Lock()
	if stats.calls[path] != c {
		stats.mu.Unlock()
		t.Fatal("a second stat of the hung path was started")
	}
	// The mount answers.
	c.err = fs.ErrNotExist
	delete(stats.calls, path)
	stats.mu.Unlock()
	close(c.done)
	if _, err := StatWithin(path, 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("after the mount answered = %v, want a fresh stat's ErrNotExist", err)
	}
}

// ReadDirWithin answers as os.ReadDir does for a file system that answers, and
// a folder read a hung mount holds is ErrStatTimeout, a second ask waiting on
// the read already out (bridge#65).
func TestReadDirWithin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.mp3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadDirWithin(dir, 5*time.Second)
	if err != nil || len(entries) != 1 || entries[0].Name() != "a.mp3" {
		t.Fatalf("ReadDirWithin = %v, %v", entries, err)
	}
	if _, err := ReadDirWithin(filepath.Join(dir, "gone"), 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing folder = %v, want ErrNotExist", err)
	}

	const hung = "/hung/mount/Author/Book"
	c := &lookCall[[]fs.DirEntry]{done: make(chan struct{}), started: time.Now()}
	readDirs.mu.Lock()
	readDirs.calls[hung] = c // a folder read stuck on a hung mount
	readDirs.mu.Unlock()
	for range 2 {
		var pe *fs.PathError
		if _, err := ReadDirWithin(hung, 10*time.Millisecond); !errors.Is(err, ErrStatTimeout) || !errors.As(err, &pe) || pe.Op != "readdir" {
			t.Fatalf("a hung folder read = %v, want a readdir ErrStatTimeout", err)
		}
	}
	readDirs.mu.Lock()
	same := readDirs.calls[hung] == c
	delete(readDirs.calls, hung)
	readDirs.mu.Unlock()
	close(c.done)
	if !same {
		t.Fatal("a second read of the hung folder was started")
	}
}

// LstatWithin answers as os.Lstat does (a link is the link, not its target),
// and a look a hung mount holds is ErrStatTimeout.
func TestLstatWithin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Fatal(err)
	}
	info, err := LstatWithin(link, 5*time.Second)
	if err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("LstatWithin = %v, %v, want the link itself", info, err)
	}
	if _, err := LstatWithin(filepath.Join(dir, "gone"), 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file = %v, want ErrNotExist", err)
	}
	const path = "/hung/mount/auth.json"
	c := &lookCall[fs.FileInfo]{done: make(chan struct{}), started: time.Now()}
	lstats.mu.Lock()
	lstats.calls[path] = c
	lstats.mu.Unlock()
	if _, err := LstatWithin(path, 10*time.Millisecond); !errors.Is(err, ErrStatTimeout) {
		t.Fatalf("a hung lstat = %v, want ErrStatTimeout", err)
	}
	lstats.mu.Lock()
	delete(lstats.calls, path)
	lstats.mu.Unlock()
	close(c.done)
}

// ReadDirNWithin reads at most n entries (a large folder isn't read whole),
// answers an empty folder with none, and a missing one as os.Open does.
func TestReadDirNWithin(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if entries, err := ReadDirNWithin(dir, 3, 5*time.Second); err != nil || len(entries) != 0 {
		t.Fatalf("an empty folder = %v, %v", entries, err)
	}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if entries, err := ReadDirNWithin(dir, 3, 5*time.Second); err != nil || len(entries) != 3 {
		t.Fatalf("five entries read three at most = %d, %v", len(entries), err)
	}
	if entries, err := ReadDirNWithin(dir, 10, 5*time.Second); err != nil || len(entries) != 5 {
		t.Fatalf("five entries read ten at most = %d, %v", len(entries), err)
	}
	if _, err := ReadDirNWithin(filepath.Join(dir, "gone"), 3, 5*time.Second); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing folder = %v, want ErrNotExist", err)
	}
}

// Callers that wait on one folder read each get a slice of their own: one
// that sorts or trims what it got changes nothing another got (bridge#65).
func TestReadDirWithinAnswersEachCallerItsOwnSlice(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for kind, out := range map[string]*looksOut[[]fs.DirEntry]{"ReadDirWithin": readDirs, "ReadDirNWithin": readDirNs} {
		path := "/shared/" + kind
		// One read out, answered: every caller that joins it gets its answer.
		c := &lookCall[[]fs.DirEntry]{done: make(chan struct{}), started: time.Now(), val: read}
		close(c.done)
		out.mu.Lock()
		out.calls[path] = c
		out.mu.Unlock()
		ask := func() []fs.DirEntry {
			if kind == "ReadDirWithin" {
				got, err := ReadDirWithin(path, 5*time.Second)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			got, err := ReadDirNWithin(path, 5, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		first, second := ask(), ask()
		out.mu.Lock()
		delete(out.calls, path)
		out.mu.Unlock()
		if len(first) != 2 || len(second) != 2 {
			t.Fatalf("%s: %d and %d entries, want 2 each", kind, len(first), len(second))
		}
		first[0], first[1] = first[1], first[0]
		if second[0].Name() != "a" || read[0].Name() != "a" {
			t.Fatalf("%s: one caller's reorder changed another's slice", kind)
		}
	}
}

// LookWithin answers as its look does, and a look a hung mount holds is
// ErrStatTimeout naming the path: a second ask of the same op and path waits on
// the look already out, while another op at that path starts its own.
func TestLookWithin(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/library"
	if n, err := LookWithin("count", path, 5*time.Second, func(string) (int, error) { return 7, nil }); n != 7 || err != nil {
		t.Fatalf("an answered look = %v, %v", n, err)
	}
	release := make(chan struct{})
	defer close(release)
	var started atomic.Int32
	hung := func(string) (struct{}, error) {
		started.Add(1)
		<-release
		return struct{}{}, nil
	}
	for range 3 {
		_, err := LookWithin("probe", path, 10*time.Millisecond, hung)
		var pe *fs.PathError
		if !errors.Is(err, ErrStatTimeout) || !errors.As(err, &pe) || pe.Path != path || pe.Op != "probe" {
			t.Fatalf("a hung look = %v, want probe's ErrStatTimeout at %s", err, path)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d looks of the hung path started, want 1", n)
	}
	if _, err := LookWithin("other", path, 5*time.Second, func(string) (struct{}, error) { return struct{}{}, fs.ErrPermission }); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("another op at the hung path = %v, want its own answer", err)
	}
}

// ChangeWithin answers as its change does; one a hung mount holds past the
// bound is ErrStatTimeout, a second ask waiting on the change already out, and
// a change that finishes once every caller gave up is undone (nobody else
// will), while one a caller took is not.
func TestChangeWithin(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/podcasts"
	undone := make(chan int, 2)
	undo := func(n int) { undone <- n }
	if n, err := ChangeWithin("answers", path, 5*time.Second, func(string) (int, error) { return 7, nil }, Late[int]{Undo: undo}); n != 7 || err != nil {
		t.Fatalf("an answered change = %v, %v", n, err)
	}
	release := make(chan struct{})
	var started atomic.Int32
	hung := func(string) (int, error) {
		started.Add(1)
		<-release
		return 9, nil
	}
	for range 3 {
		_, err := ChangeWithin("marker", path, 10*time.Millisecond, hung, Late[int]{Undo: undo})
		var pe *fs.PathError
		if !errors.Is(err, ErrStatTimeout) || !errors.As(err, &pe) || pe.Path != path || pe.Op != "marker" {
			t.Fatalf("a hung change = %v, want marker's ErrStatTimeout at %s", err, path)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d changes of the hung path started, want 1", n)
	}
	select {
	case n := <-undone:
		t.Fatalf("undid %d before the change finished", n)
	default:
	}
	close(release)
	select {
	case n := <-undone:
		if n != 9 {
			t.Fatalf("undid %d, want the hung change's own answer", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a change nobody waited for any more was not undone")
	}
	// The answered change (taken by its caller) was never undone.
	select {
	case n := <-undone:
		t.Fatalf("undid a change a caller took (%d)", n)
	case <-time.After(50 * time.Millisecond):
	}
}

// A change nobody waits for any more stays out while its undo runs: a later
// call meanwhile waits on it and gets ErrStatTimeout, never starting a change
// of its own beside the undo (it would see what the undo is about to remove)
// nor taking the undone answer; once the undo is done the next call changes
// again.
func TestChangeWithinHoldsTheChangeWhileUndoing(t *testing.T) {
	t.Parallel()
	const path = "/hung/mount/undoing"
	release, undoing, undone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	undo := func(int) { close(undoing); <-undone }
	if _, err := ChangeWithin("marker", path, 10*time.Millisecond, func(string) (int, error) { <-release; return 1, nil }, Late[int]{Undo: undo}); !errors.Is(err, ErrStatTimeout) {
		t.Fatalf("a hung change = %v", err)
	}
	close(release)
	<-undoing
	var started atomic.Int32
	again := func(string) (int, error) { started.Add(1); return 2, nil }
	if n, err := ChangeWithin("marker", path, 10*time.Millisecond, again, Late[int]{}); !errors.Is(err, ErrStatTimeout) || n != 0 {
		t.Fatalf("a change asked for while the undo runs = %v, %v; want ErrStatTimeout", n, err)
	}
	if started.Load() != 0 {
		t.Fatal("a change started beside the undo")
	}
	close(undone)
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := ChangeWithin("marker", path, time.Second, again, Late[int]{})
		if err == nil {
			if n != 2 {
				t.Fatalf("the change after the undo = %d, want its own answer", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no change once the undo was done: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// A change that finishes once every caller gave up, and that Late.Keep takes,
// is kept rather than undone: the next call gets its answer at once, without a
// change of its own (a mount too slow for the bound gets one through), and it
// is that call's from then on (never undone). One Keep refuses is undone.
func TestChangeWithinKeepsALateAnswerForTheNextCall(t *testing.T) {
	t.Parallel()
	const path = "/slow/mount/kept"
	undone := make(chan int, 4)
	late := Late[int]{Undo: func(n int) { undone <- n }, Keep: func(n int, err error) (int, bool) { return n * 10, err == nil && n > 0 }, For: time.Minute}
	var started atomic.Int32
	slow := func(answer int) func(string) (int, error) {
		return func(string) (int, error) {
			started.Add(1)
			time.Sleep(60 * time.Millisecond)
			return answer, nil
		}
	}
	if _, err := ChangeWithin("marker", path, 10*time.Millisecond, slow(5), late); !errors.Is(err, ErrStatTimeout) {
		t.Fatalf("a slow change = %v, want ErrStatTimeout", err)
	}
	// The next call takes the late answer, however short its own bound.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := ChangeWithin("marker", path, time.Millisecond, slow(6), late)
		if err == nil {
			if n != 5 && n != 50 { // 5: it answered as this call joined it
				t.Fatalf("the next call = %d, want the late change's own answer, as kept", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the late answer was never handed over: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d changes started, want only the slow one", n)
	}
	select {
	case n := <-undone:
		t.Fatalf("undid %d, the answer a call took", n)
	default:
	}
	// A late answer Keep refuses is undone.
	if _, err := ChangeWithin("marker", path, 10*time.Millisecond, slow(-1), late); !errors.Is(err, ErrStatTimeout) {
		t.Fatalf("a slow change = %v, want ErrStatTimeout", err)
	}
	select {
	case n := <-undone:
		if n != -1 {
			t.Fatalf("undid %d, want only the answer Keep refused (the kept one was taken)", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a late answer Keep refused was not undone")
	}
}

// A kept answer no call comes for within Late.For is undone after all (nobody
// else will clean up after it), and the call after that changes again.
func TestChangeWithinUndoesAKeptAnswerNobodyTook(t *testing.T) {
	t.Parallel()
	const path = "/slow/mount/untaken"
	undone := make(chan int, 1)
	late := Late[int]{Undo: func(n int) { undone <- n }, Keep: func(n int, _ error) (int, bool) { return n, true }, For: 30 * time.Millisecond}
	release := make(chan struct{})
	if _, err := ChangeWithin("marker", path, 10*time.Millisecond, func(string) (int, error) { <-release; return 3, nil }, late); !errors.Is(err, ErrStatTimeout) {
		t.Fatalf("a hung change = %v, want ErrStatTimeout", err)
	}
	close(release)
	select {
	case n := <-undone:
		if n != 3 {
			t.Fatalf("undid %d, want the kept answer", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a kept answer nobody took was never undone")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := ChangeWithin("marker", path, time.Second, func(string) (int, error) { return 4, nil }, late)
		if err == nil {
			if n != 4 {
				t.Fatalf("the call after the undo = %d, want a change of its own", n)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no change once the kept answer was undone: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}
