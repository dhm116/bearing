package match

import "strings"

// normalize lowercases a name and folds runs of '-', '_' and space into one
// space.
func normalize(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == '-' || r == '_' || r == ' '
	}), " ")
}

// jaroWinkler is the Jaro-Winkler similarity of two strings, 0 to 1, with the
// usual prefix scale of 0.1 over at most four runes.
func jaroWinkler(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	window := max(max(len(ra), len(rb))/2-1, 0)
	ma, mb := make([]bool, len(ra)), make([]bool, len(rb))
	matches := 0
	for i, x := range ra {
		for j := max(0, i-window); j < min(len(rb), i+window+1); j++ {
			if !mb[j] && rb[j] == x {
				ma[i], mb[j] = true, true
				matches++
				break
			}
		}
	}
	if matches == 0 {
		return 0
	}
	transpositions, j := 0, 0
	for i := range ra {
		if !ma[i] {
			continue
		}
		for !mb[j] {
			j++
		}
		if ra[i] != rb[j] {
			transpositions++
		}
		j++
	}
	m := float64(matches)
	jaro := (m/float64(len(ra)) + m/float64(len(rb)) + (m-float64(transpositions)/2)/m) / 3
	prefix := 0
	for prefix < min(4, len(ra), len(rb)) && ra[prefix] == rb[prefix] {
		prefix++
	}
	if jaro < 0.7 {
		return jaro
	}
	return jaro + float64(prefix)*0.1*(1-jaro)
}

// jaccard is |a ∩ b| / |a ∪ b|; two empty sets score 0, not 1, so having no
// members is not evidence.
func jaccard[K comparable](a, b map[K]bool) float64 {
	n := 0
	for k := range a {
		if b[k] {
			n++
		}
	}
	if u := len(a) + len(b) - n; u > 0 {
		return float64(n) / float64(u)
	}
	return 0
}
