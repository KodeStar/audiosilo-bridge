package podcast

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Merge keeps assigned paths, file generations and first-seen timestamps.
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
	matched := map[int]bool{}
	out := make([]Episode, 0, len(old))
	added := 0
	for _, e := range p.Episodes {
		key := Identity(e)
		if seen[key] {
			continue
		}
		seen[key] = true
		i, ok := identities[key]
		if !ok {
			i, ok = ids[EpisodeID(e)]
		}
		if !ok && !f.Private {
			i, ok = urls[EnclosureBase(e.Enclosure.URL)]
		}
		if !ok {
			i, ok = titles[e.Title+"|"+e.Published]
		}
		if ok {
			if matched[i] {
				continue
			}
			matched[i] = true
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
