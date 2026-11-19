package link

import (
	"cmp"
	"encoding/json"
	"slices"
)

// The heartbeat bounds of a server before v2.7.0: it refuses a body over
// LegacyStatusBytes and a list of more than LegacyStatusLibraries mappings (a
// 400). A newer server takes 2 MiB and cuts a longer list to 4096 rather than
// refusing it, but a proxy in front of either can refuse less (a 413). A
// heartbeat refused for its size is sent again with a list that fits these
// (FitLibraries), so its reply still lists the server's libraries
// (server#445).
const (
	LegacyStatusBytes     = 64 << 10
	LegacyStatusLibraries = 512
)

// FitLibraries returns st with its library list cut so the whole body is at
// most maxBytes and the list at most maxLibraries long, and how many mappings
// were left out (0: st fits as it is, and is returned unchanged). What a job
// needs is kept first: the writable libraries, then the mapped ones, then the
// rest, each in list order; those kept stay in their list order. A server
// without a library's mapping counts it as not mapped, so a cut library only
// waits for its jobs, as it would unmapped.
func FitLibraries(st Status, maxBytes, maxLibraries int) (Status, int) {
	if len(st.Libraries) == 0 {
		return st, 0
	}
	if len(st.Libraries) <= maxLibraries {
		if b, err := json.Marshal(st); err == nil && len(b) <= maxBytes {
			return st, 0
		}
	}
	base := st
	base.Libraries = []LibraryStatus{}
	b, err := json.Marshal(base)
	if err != nil {
		return base, len(st.Libraries)
	}
	room := maxBytes - len(b) // the list's "[]" is already counted
	order := make([]int, len(st.Libraries))
	for i := range order {
		order[i] = i
	}
	rank := func(l LibraryStatus) int {
		switch {
		case l.Writable:
			return 0
		case l.Mapped:
			return 1
		}
		return 2
	}
	slices.SortStableFunc(order, func(a, c int) int {
		return cmp.Compare(rank(st.Libraries[a]), rank(st.Libraries[c]))
	})
	kept := make([]int, 0, min(len(order), maxLibraries))
	for _, i := range order {
		if len(kept) == maxLibraries {
			break
		}
		lb, err := json.Marshal(st.Libraries[i])
		if err != nil {
			continue
		}
		size := len(lb)
		if len(kept) > 0 {
			size++ // the comma
		}
		if size > room {
			continue // a smaller mapping further on may still fit
		}
		room -= size
		kept = append(kept, i)
	}
	slices.Sort(kept)
	out := make([]LibraryStatus, 0, len(kept))
	for _, i := range kept {
		out = append(out, st.Libraries[i])
	}
	base.Libraries = out
	return base, len(st.Libraries) - len(out)
}
