package link

import "encoding/json"

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
// were left out (0: st fits as it is, or the rest of its body is over
// maxBytes alone so no cut can fit it, and is returned unchanged). What a job
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
		return st, 0
	}
	room := maxBytes - len(b) // the list's "[]" is already counted
	if room < 0 {
		// The rest of the body is over the bound alone: no cut makes it fit,
		// and an empty list taken by a laxer server would unmap every library.
		return st, 0
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
	keep, kept := make([]bool, len(st.Libraries)), 0
	for r := 0; r < 3 && kept < maxLibraries; r++ {
		for i, l := range st.Libraries {
			if kept == maxLibraries {
				break
			}
			if rank(l) != r {
				continue
			}
			lb, err := json.Marshal(l)
			if err != nil {
				continue
			}
			size := len(lb)
			if kept > 0 {
				size++ // the comma
			}
			if size > room {
				continue // a smaller mapping further on may still fit
			}
			room -= size
			keep[i], kept = true, kept+1
		}
	}
	out := make([]LibraryStatus, 0, kept)
	for i, l := range st.Libraries {
		if keep[i] {
			out = append(out, l)
		}
	}
	base.Libraries = out
	return base, len(st.Libraries) - len(out)
}
