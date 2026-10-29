package readalong

// ratio is Python's difflib.SequenceMatcher(None, a, b).ratio() over runes, the
// spike's fuzzy word match. No junk and no "popular" pruning: that only applies to
// a b of 200 or more elements, and these are words.
func ratio(a, b []rune) float64 {
	if len(a)+len(b) == 0 {
		return 1
	}
	// difflib's j2len dicts as two rows indexed j+1 (0 = "no j-1"), reused by
	// every longestMatch: words are short, so clearing a row costs less than a map.
	rows := make([]int, 2*(len(b)+1))
	matches := 0
	queue := [][4]int{{0, len(a), 0, len(b)}}
	for len(queue) > 0 {
		q := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		alo, ahi, blo, bhi := q[0], q[1], q[2], q[3]
		i, j, k := longestMatch(a, b, rows, alo, ahi, blo, bhi)
		if k == 0 {
			continue
		}
		matches += k
		if alo < i && blo < j {
			queue = append(queue, [4]int{alo, i, blo, j})
		}
		if i+k < ahi && j+k < bhi {
			queue = append(queue, [4]int{i + k, ahi, j + k, bhi})
		}
	}
	return 2.0 * float64(matches) / float64(len(a)+len(b))
}

// longestMatch is find_longest_match: the longest block a[i:i+k] == b[j:j+k] in
// the bounds, the earliest in a (then b) among the longest.
func longestMatch(a, b []rune, rows []int, alo, ahi, blo, bhi int) (besti, bestj, bestsize int) {
	besti, bestj = alo, blo
	w := len(b) + 1
	j2len, next := rows[:w], rows[w:]
	clear(j2len)
	for i := alo; i < ahi; i++ {
		clear(next)
		for j := blo; j < bhi; j++ {
			if b[j] != a[i] {
				continue
			}
			k := j2len[j] + 1
			next[j+1] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len, next = next, j2len
	}
	for besti > alo && bestj > blo && a[besti-1] == b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && a[besti+bestsize] == b[bestj+bestsize] {
		bestsize++
	}
	return besti, bestj, bestsize
}
