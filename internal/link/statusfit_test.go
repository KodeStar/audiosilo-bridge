package link

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func statusWith(n int) Status {
	st := Status{Version: "0.9.0", Kinds: []string{"sidecars.generate"}, Workers: []Worker{}}
	for i := 1; i <= n; i++ {
		st.Libraries = append(st.Libraries, LibraryStatus{LibraryID: int64(i),
			MountPath: fmt.Sprintf("/libraries/%04d-%s", i, strings.Repeat("x", 60)),
			Error:     "the folder is missing"})
	}
	return st
}

func bodySize(t *testing.T, st Status) int {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func TestFitLibrariesLeavesAFittingListAlone(t *testing.T) {
	st := statusWith(10)
	got, cut := FitLibraries(st, LegacyStatusBytes, LegacyStatusLibraries)
	if cut != 0 || len(got.Libraries) != 10 {
		t.Fatalf("cut %d, kept %d, want the whole list", cut, len(got.Libraries))
	}
}

// A list over the byte bound is cut to a body that fits, the writable and
// mapped libraries first, the kept ones still in list order.
func TestFitLibrariesCutsToTheBytesKeepingWhatJobsNeed(t *testing.T) {
	st := statusWith(600)
	st.Libraries[599].Mapped, st.Libraries[599].Writable = true, true
	st.Libraries[450].Mapped = true
	if bodySize(t, st) <= LegacyStatusBytes {
		t.Fatal("the fixture fits already")
	}
	got, cut := FitLibraries(st, LegacyStatusBytes, LegacyStatusLibraries)
	if size := bodySize(t, got); size > LegacyStatusBytes {
		t.Fatalf("the cut body is %d bytes, over %d", size, LegacyStatusBytes)
	}
	if cut == 0 || cut+len(got.Libraries) != 600 {
		t.Fatalf("cut %d, kept %d of 600", cut, len(got.Libraries))
	}
	ids := map[int64]bool{}
	for i, l := range got.Libraries {
		ids[l.LibraryID] = true
		if i > 0 && got.Libraries[i-1].LibraryID >= l.LibraryID {
			t.Fatalf("kept out of list order at %d", i)
		}
	}
	if !ids[600] || !ids[451] {
		t.Fatal("the writable or the mapped library was cut")
	}
	if len(st.Libraries) != 600 {
		t.Fatal("the caller's list was changed")
	}
}

func TestFitLibrariesCutsToTheCount(t *testing.T) {
	st := statusWith(600)
	got, cut := FitLibraries(st, 1<<30, LegacyStatusLibraries)
	if len(got.Libraries) != LegacyStatusLibraries || cut != 600-LegacyStatusLibraries {
		t.Fatalf("kept %d, cut %d", len(got.Libraries), cut)
	}
}

// A body over the bound without its list can't be cut to fit: the list is left
// whole rather than sent empty (a laxer server taking it would unmap every
// library).
func TestFitLibrariesLeavesAListNoCutCanFit(t *testing.T) {
	st := statusWith(600)
	st.UIURL = "http://" + strings.Repeat("h", LegacyStatusBytes)
	got, cut := FitLibraries(st, LegacyStatusBytes, LegacyStatusLibraries)
	if cut != 0 || len(got.Libraries) != 600 {
		t.Fatalf("cut %d, kept %d, want the list left whole", cut, len(got.Libraries))
	}
}
