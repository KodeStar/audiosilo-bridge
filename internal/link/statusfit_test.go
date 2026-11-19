package link

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// Room left for none of the mappings is no cut either: the list is left whole
// rather than sent empty.
func TestFitLibrariesLeavesAListWithRoomForNone(t *testing.T) {
	st := statusWith(600)
	base := st
	base.Libraries = []LibraryStatus{}
	st.UIURL = "http://" + strings.Repeat("h", LegacyStatusBytes-bodySize(t, base)-len("http://")-20)
	base.UIURL = st.UIURL
	if room := LegacyStatusBytes - bodySize(t, base); room < 0 || room > 40 {
		t.Fatalf("the fixture leaves %d bytes, want a little room", room)
	}
	got, cut := FitLibraries(st, LegacyStatusBytes, LegacyStatusLibraries)
	if cut != 0 || len(got.Libraries) != 600 {
		t.Fatalf("cut %d, kept %d, want the list left whole", cut, len(got.Libraries))
	}
}

// A refusal is the size's only when its answer says so: a 413, the server's
// too_large code, an older server's "too many libraries", or "invalid status"
// (a body the server couldn't read) for a body over the smallest byte bound.
func TestSizeRefusal(t *testing.T) {
	big, small := statusWith(600), statusWith(3)
	if bodySize(t, big) <= LegacyStatusBytes || bodySize(t, small) > LegacyStatusBytes {
		t.Fatal("the fixtures are on the wrong side of the byte bound")
	}
	bad := func(code, msg string) error {
		return fmt.Errorf("beat: %w", &APIError{Status: http.StatusBadRequest, Code: code, Message: msg})
	}
	for _, c := range []struct {
		name string
		err  error
		st   Status
		want bool
	}{
		{"a 413", &APIError{Status: http.StatusRequestEntityTooLarge, Message: "Request Entity Too Large"}, small, true},
		{"the too_large code", bad("too_large", "the status is too large"), small, true},
		{"too many libraries", bad("", "invalid: too many libraries"), small, true},
		{"an unreadable body over the byte bound", bad("", "invalid status"), big, true},
		{"an unreadable body within it", bad("", "invalid status"), small, false},
		{"a bad ui_url on a long list", bad("", "invalid: ui_url must be an http(s) URL"), big, false},
		{"a bad setup block on a long list", bad("", "invalid: setup"), big, false},
		{"another code on a long list", bad("invalid_request", "invalid status"), big, false},
		{"a 503 on a long list", &APIError{Status: http.StatusServiceUnavailable, Message: "invalid status"}, big, false},
		{"no answer at all", errors.New("dial tcp: refused"), big, false},
		{"unauthorized", ErrUnauthorized, big, false},
	} {
		if got := SizeRefusal(c.err, c.st); got != c.want {
			t.Errorf("%s: SizeRefusal = %v, want %v", c.name, got, c.want)
		}
	}
}
