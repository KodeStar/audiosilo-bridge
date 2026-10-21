package podcast

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Merge keeps assigned paths, file generations and first-seen timestamps. Every
// feed item first claims the kept episode of its own identity or id; only then
// do the items left over try the weaker matches, so a weak match never takes an
// episode whose own item is still in the feed. An item with a claimed episode's
// enclosure is that episode listed again (an alias, dropped); one that only
// shares a claimed episode's title and date is a new episode.
func Merge(f *Feed, p *Parsed, show string, now time.Time, occupied ...string) (int, error) {
	stamp := now.UTC().Format(time.RFC3339)
	old := f.Episodes
	identities, urls, titles := map[string]int{}, map[string]int{}, map[string]int{}
	ids := map[string]int{}
	used := map[string]bool{}
	for _, name := range occupied {
		used[foldName(show+"/"+name)] = true
	}
	for i, e := range old {
		ids[e.ID] = i
		identities[Identity(e)] = i
		if e.Enclosure.URL != "" {
			urls[EnclosureBase(e.Enclosure.URL)] = i
		}
		titles[e.Title+"|"+e.Published] = i
		used[foldName(e.Path)] = true
	}
	seen := map[string]bool{}
	items := make([]Episode, 0, len(p.Episodes))
	for _, e := range p.Episodes {
		if key := Identity(e); !seen[key] {
			seen[key] = true
			items = append(items, e)
		}
	}
	// claims[n] is the kept episode items[n] continues (-1: a new one); dup
	// marks an item whose own episode another item of the feed claimed first.
	claims := make([]int, len(items))
	dup := make([]bool, len(items))
	matched := map[int]bool{}
	for n, e := range items {
		claims[n] = -1
		i, ok := identities[Identity(e)]
		if !ok {
			i, ok = ids[EpisodeID(e)]
		}
		switch {
		case ok && matched[i]:
			dup[n] = true
		case ok:
			matched[i], claims[n] = true, i
		}
	}
	for n, e := range items {
		if claims[n] >= 0 || dup[n] {
			continue
		}
		// The same enclosure is the same episode: an item whose enclosure a
		// claimed episode has is that episode listed again (an alias).
		if i, ok := urls[EnclosureBase(e.Enclosure.URL)]; ok && !f.Private {
			if matched[i] {
				dup[n] = true
			} else {
				matched[i], claims[n] = true, i
			}
			continue
		}
		if i, ok := titles[e.Title+"|"+e.Published]; ok && !matched[i] {
			matched[i], claims[n] = true, i
		}
	}
	out := make([]Episode, 0, len(old)+len(items))
	added := 0
	for n, e := range items {
		if dup[n] {
			continue
		}
		if i := claims[n]; i >= 0 {
			kept := old[i]
			e.ID, e.Path, e.FirstSeen, e.Fetch = kept.ID, kept.Path, kept.FirstSeen, kept.Fetch
		} else {
			e.ID = EpisodeID(e)
			e.Path = EpisodePath(show, e, Extension(e.Enclosure.Type, e.Enclosure.URL), used)
			e.FirstSeen = stamp
			used[foldName(e.Path)] = true
			added++
		}
		e.RemovedAt = ""
		out = append(out, e)
	}
	for i, e := range old {
		if matched[i] {
			continue
		}
		if e.RemovedAt == "" {
			e.RemovedAt = stamp
		}
		out = append(out, e)
	}
	if len(out) > MaxEpisodes {
		out = dropRemoved(out, len(out)-MaxEpisodes)
	}
	if len(out) > MaxEpisodes {
		return 0, ErrTooLarge
	}
	slices.SortStableFunc(out, func(a, b Episode) int {
		if n := cmp.Compare(b.Published, a.Published); n != 0 {
			return n
		}
		return cmp.Compare(a.Path, b.Path)
	})
	image := f.Show.Image
	f.Show = p.Show
	f.Show.Image = image
	f.Episodes = out
	f.ScrubPrivate()
	return added, nil
}

// dropRemoved drops up to n removed episodes the Bridge never fetched, those
// gone longest first, so a feed that rotates its items through the episode cap
// keeps updating (one that comes back is named afresh).
func dropRemoved(eps []Episode, n int) []Episode {
	var gone []int
	for i, e := range eps {
		if e.RemovedAt != "" && e.Fetch == nil {
			gone = append(gone, i)
		}
	}
	slices.SortStableFunc(gone, func(a, b int) int { return cmp.Compare(eps[a].RemovedAt, eps[b].RemovedAt) })
	drop := map[int]bool{}
	for _, i := range gone[:min(n, len(gone))] {
		drop[i] = true
	}
	out := make([]Episode, 0, len(eps)-len(drop))
	for i, e := range eps {
		if !drop[i] {
			out = append(out, e)
		}
	}
	return out
}

func NextPoll(f *Feed, cacheControl string, now time.Time) int {
	next := 86400
	for _, e := range f.Episodes {
		if e.RemovedAt != "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.Published)
		if err != nil {
			continue
		}
		age := now.Sub(t)
		switch {
		case age < 2*24*time.Hour:
			next = min(next, 1800)
		case age < 14*24*time.Hour:
			next = min(next, 7200)
		case age < 60*24*time.Hour:
			next = min(next, 21600)
		}
	}
	for _, part := range strings.Split(cacheControl, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.EqualFold(key, "max-age") {
			if n, e := strconv.Atoi(strings.Trim(value, `"`)); e == nil {
				next = max(next, min(n, 86400))
			}
		}
	}
	return next
}
