package canton

// Round-2 sweep of the loose index with the extra unescaping: escapes that
// survive net/http's decode (%25XX), at every level, combined with the
// previous round's case, dot, slash and ?/# spellings.

import (
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mainLooseKey is looseKey as it is on main b7873d07 (no extra unescaping).
func mainLooseKey(method, urlPath string) string {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	return method + " " + strings.ToLower(path.Clean("/"+urlPath))
}

// encodeAgain turns every "%" of s into "%25", n times.
func encodeAgain(s string, n int) string {
	for i := 0; i < n; i++ {
		s = strings.ReplaceAll(s, "%", "%25")
	}
	return s
}

func z2SweepSpellings(p string) []string {
	set := map[string]bool{}
	for _, s := range spellingsOf(p) {
		set[s] = true
		if strings.Contains(s, "%") && !strings.HasPrefix(s, "http://") {
			for n := 1; n <= 4; n++ {
				set[encodeAgain(s, n)] = true
			}
		}
	}
	// One character escaped and then escaped again, 1..4 extra levels, upper
	// and lower hex; and the whole path escaped twice.
	for i := 1; i < len(p); i++ {
		c := p[i]
		for n := 1; n <= 4; n++ {
			set[p[:i]+encodeAgain(fmt.Sprintf("%%%02X", c), n)+p[i+1:]] = true
			set[p[:i]+encodeAgain(fmt.Sprintf("%%%02x", c), n)+p[i+1:]] = true
			set[strings.ToUpper(p[:i])+encodeAgain(fmt.Sprintf("%%%02X", c), n)+strings.ToUpper(p[i+1:])] = true
		}
	}
	var full strings.Builder
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			full.WriteByte('/')
		} else {
			fmt.Fprintf(&full, "%%%02X", p[i])
		}
	}
	for n := 1; n <= 3; n++ {
		set[encodeAgain(full.String(), n)] = true
		set[encodeAgain(full.String()+"%3Fz", n)] = true
		set["/x"+encodeAgain("%2F..%2F", n)+strings.TrimPrefix(p, "/")] = true
		set[encodeAgain("%2E", n)+p] = true
		set[p+encodeAgain("%2F", n)] = true
		set[p+encodeAgain("%2F.", n)] = true
		set[p+encodeAgain("%3F", n)+"x"] = true
		set[p+encodeAgain("%23", n)+"x"] = true
		set[p+"%3F"+encodeAgain("%2F..%2Ffree", n)] = true
	}
	// An escape that is broken after one more decode stops the unescaping.
	set[p+"%25"] = true
	set[encodeAgain("/%2E", 1)+p+"%25zz"] = true
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestZ2SweepExtraUnescaping(t *testing.T) {
	routes := sweepRoutes()
	g := buildSweepGate(t, routes)
	mainIndex := map[string]bool{}
	keyPaths := map[string]bool{}
	for key := range routes {
		method, p, _ := strings.Cut(key, " ")
		keyPaths[p] = true
		mainIndex[mainLooseKey(method, p)] = true
	}
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "get", "Post"}
	var checked, parseFailed, gated, charged, refused int
	var falseNeg, newRefusals, chargeMismatch, newRefusalNoPercent []string
	depth := map[int]int{}
	seen := map[string]bool{}
	for p := range keyPaths {
		for _, target := range z2SweepSpellings(p) {
			if seen[target] {
				continue
			}
			seen[target] = true
			r0, err := parseRaw("GET", target)
			if err != nil {
				parseFailed++
				continue
			}
			probe := probeAnswer(t, g, r0)
			for _, m := range methods {
				r, err := parseRaw(m, target)
				if err != nil {
					continue
				}
				require.Equal(t, probe, probeAnswer(t, g, r), "probe depends on the method: %s %q", m, target)
				class := coreClass(g, r)
				checked++
				key := strings.ToUpper(m)
				if key == http.MethodHead {
					key = http.MethodGet
				}
				sdk := g.httpServer.RequiresPayment(g.requestContext(r))
				if (class == "charge") != sdk {
					chargeMismatch = append(chargeMismatch, fmt.Sprintf("%s %q class=%s sdk=%v", m, target, class, sdk))
				}
				switch class {
				case "charge":
					charged++
				case "refuse":
					refused++
				}
				if class == "charge" || class == "refuse" {
					gated++
					if !probe[key] {
						falseNeg = append(falseNeg, fmt.Sprintf("%s %q path=%q core=%s probe=%v", m, target, r.URL.Path, class, probe))
					}
				}
				if class == "refuse" && !mainIndex[mainLooseKey(m, r.URL.Path)] {
					newRefusals = append(newRefusals, fmt.Sprintf("%s %q path=%q", m, target, r.URL.Path))
					if !strings.Contains(r.URL.Path, "%") {
						newRefusalNoPercent = append(newRefusalNoPercent, target)
					}
					// how many extra decodes it took
					d, s := 0, r.URL.Path
					for d < 4 && !mainIndex[mainLooseKey(m, s)] {
						u, err := url.PathUnescape(s)
						if err != nil || u == s {
							break
						}
						s, d = u, d+1
					}
					depth[d]++
				}
				if strings.HasPrefix(class, "other") {
					t.Errorf("unexpected core answer %s for %s %q", class, m, target)
				}
			}
		}
	}
	t.Logf("spellings=%d parseFailed=%d requests=%d gated=%d (charged=%d refused=%d) falseNegatives=%d newRefusalsVsMain=%d chargeMismatch=%d",
		len(seen), parseFailed, checked, gated, charged, refused, len(falseNeg), len(newRefusals), len(chargeMismatch))
	sort.Strings(newRefusals)
	for i, line := range newRefusals {
		if i < 12 {
			t.Logf("new refusal (main passed it to next): %s", line)
		}
	}
	t.Logf("extra decodes needed by the new refusals: %v", depth)
	for _, line := range falseNeg {
		t.Errorf("FALSE NEGATIVE: %s", line)
	}
	for _, line := range chargeMismatch {
		t.Errorf("a charge that is not the SDK's: %s", line)
	}
	for _, target := range newRefusalNoPercent {
		t.Errorf("new refusal without %% in the decoded path: %q", target)
	}
}

// The extra unescaping on a path at net/http's size limit: bounded work.
func TestZ2LooseKeyCost(t *testing.T) {
	g := buildSweepGate(t, sweepRoutes())
	for _, unit := range []string{"%2525", "%25", "%252F..", "a"} {
		long := "/" + strings.Repeat(unit, (1<<20)/len(unit))
		r, err := http.NewRequest("GET", "http://h/x", nil)
		require.NoError(t, err)
		r.URL.Path = long
		start := time.Now()
		_ = g.routes.gatedMethods(long)
		probeTook := time.Since(start)
		start = time.Now()
		_ = g.routes.looksLikePaidRoute(r)
		coreTook := time.Since(start)
		t.Logf("unit %q, path %d bytes: gatedMethods %s, looksLikePaidRoute %s", unit, len(long), probeTook, coreTook)
		require.Less(t, probeTook, 2*time.Second)
	}
}
