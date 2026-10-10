package trust

import (
	"sort"
)

// Rule 3 (parameter version 6, C152): endorsements are stakes, settled by
// independent confirmation. It replaces the propagation; seeds are as in
// rule 2.
//
// Every judgement act commits stake_ppm × its weight (edge_weights) × its
// decay of its author's standing, at most stake_budget_ppm of it in all
// (an account's acts are scaled down together above the budget):
//
//   - A vouch, an accepted work item and a verified witness MOVE that stake
//     to their target. It is the endorser's own standing: nothing comes from
//     a pool, so endorsing has a price and selling endorsements gains
//     nothing.
//   - A vote moves nothing; it ranks the post (v(s), unchanged). With a
//     witness verdict (verified or failed) on a claim, and the vouch or
//     accept itself on its target, it is a POSITION, settled below.
//   - A down vote is neither: it ranks.
//
// Settlement prices each object by its expected independent endorsement:
//
//   - a post opens at its author's median reception (the stake mass on the
//     author's posts that drew any, once prior_min_posts of them did; until
//     then the median over all such posts), and its price is the larger of
//     that and the stake already on it; realized R is the stake on it from
//     accounts independent of the position's author;
//   - a claim opens at nothing; R is the independent stake on the
//     position's side less the independent stake against it; a claim nobody
//     independent has checked settles nothing;
//   - an agent (vouch, accept) is priced by its trajectory, the stake moved
//     to it before the act scaled to the rest of the window (an agent with
//     none: the median over agents); R is the stake independent accounts
//     moved to it after the act. A penalised agent scores −1, and its
//     vouchers lose half their stake times the penalty to the seeds.
//
// Independent means another root and no transfer either way within
// funded_days. A position scores r = (R − P) / (R + P) in [−1, 1]. Losers
// pay winners within one pot (an author's posts, one claim, one agent): a
// position with r < 0 pays up to |r|/2 of its stake, a position with r > 0
// earns up to r/2 of it, both scaled so the pot balances. Nobody pays for
// endorsing late what others endorsed early, and nothing is minted: what a
// floor leaves over returns to the seeds, so total standing never exceeds
// the seed mass.
//
// Standing is the fixed point of seed + moved in − moved out + settled,
// reached in `iterations` steps from the seed (stakes read the previous
// step's standing). Penalties scale an account's standing each step; what
// they remove returns to the seeds by seed.

type stakeAct struct {
	src, dst, kind, item string
	at                   int64
	against              bool  // a failed witness verdict
	load                 int64 // weight × decay (ppm)
}

type stakeResult struct {
	c, raw, judged []int64 // standing, before the penalty, settled (last step)
	positions      int64
}

const (
	posNone = iota
	posPost
	posClaim
	posAgent
)

func runStakes(st *StandingParams, asOf, ws int64, nodes []string, seed, pen []int64, acts []stakeAct, indep func(a, b string) bool) stakeResult {
	n := len(nodes)
	index := make(map[string]int, n)
	for i, a := range nodes {
		index[a] = i
	}
	kept := acts[:0:0]
	for _, a := range acts {
		_, okS := index[a.src]
		_, okD := index[a.dst]
		if okS && okD && a.load > 0 {
			kept = append(kept, a)
		}
	}
	acts = kept
	sort.Slice(acts, func(x, y int) bool {
		a, b := acts[x], acts[y]
		switch {
		case a.at != b.at:
			return a.at < b.at
		case a.src != b.src:
			return a.src < b.src
		case a.dst != b.dst:
			return a.dst < b.dst
		case a.kind != b.kind:
			return a.kind < b.kind
		case a.item != b.item:
			return a.item < b.item
		}
		return !a.against && b.against
	})
	m := len(acts)
	src, dst := make([]int, m), make([]int, m)
	transfer := make([]bool, m)
	class := make([]int, m)
	objOf := make([]string, m)
	load := make([]int64, n)
	objects := map[string][]int{}
	transfersIn := map[int][]int{}
	postAuthor := map[string]int{}
	var res stakeResult
	for x, a := range acts {
		src[x], dst[x] = index[a.src], index[a.dst]
		switch {
		case a.kind == "vouch" || a.kind == "work_accept":
			transfer[x], class[x], objOf[x] = true, posAgent, "g\x00"+a.dst
		case a.kind == "witness" && !a.against:
			transfer[x] = true
			if a.item != "" {
				class[x], objOf[x] = posClaim, "c\x00"+a.dst+"\x00"+a.item
			}
		case a.kind == "witness":
			if a.item != "" {
				class[x], objOf[x] = posClaim, "c\x00"+a.dst+"\x00"+a.item
			}
		case a.kind == "vote" && a.item != "":
			class[x], objOf[x] = posPost, "p\x00"+a.dst+"\x00"+a.item
			postAuthor[objOf[x]] = dst[x]
		}
		if transfer[x] {
			load[src[x]] += a.load
			transfersIn[dst[x]] = append(transfersIn[dst[x]], x)
		}
		if class[x] == posPost || class[x] == posClaim {
			load[src[x]] += a.load
		}
		if class[x] != posNone {
			objects[objOf[x]] = append(objects[objOf[x]], x)
			res.positions++
		}
	}
	scale := make([]int64, n)
	for i := range scale {
		scale[i] = 1e6
		if share := mulDiv(st.StakePPM, load[i], 1e6); share > st.StakeBudgetPPM {
			scale[i] = mulDiv(st.StakeBudgetPPM, 1e6, share)
		}
	}
	keys := make([]string, 0, len(objects))
	for k := range objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	agents := make([]int, 0, len(transfersIn))
	for d := range transfersIn {
		agents = append(agents, d)
	}
	sort.Ints(agents)
	// What returns to the seeds goes to them by seed, less any penalty: a
	// penalised account gets none of it back.
	eff := make([]int64, n)
	var S int64
	for i, s := range seed {
		eff[i] = mulDiv(s, 1e6-pen[i], 1e6)
		S += eff[i]
	}
	window := max(1, asOf-ws)

	c := append([]int64(nil), seed...)
	raw := make([]int64, n)
	judged := make([]int64, n)
	sig := make([]int64, m)
	r := make([]int64, m)
	on := make([]bool, m)
	for it := int64(0); it < st.Iterations; it++ {
		next := append([]int64(nil), seed...)
		for i := range judged {
			judged[i] = 0
		}
		for x, a := range acts {
			sig[x] = mulDiv(mulDiv(c[src[x]], st.StakePPM, 1e6), mulDiv(a.load, scale[src[x]], 1e6), 1e6)
			if transfer[x] {
				next[src[x]] -= sig[x]
				next[dst[x]] += sig[x]
			}
		}
		// Priors: post reception by author, agent trajectory.
		reception := map[int][]int64{}
		var all []int64
		postMass := map[string]int64{}
		for _, k := range keys {
			if k[0] != 'p' {
				continue
			}
			var mass int64
			for _, x := range objects[k] {
				mass += sig[x]
			}
			postMass[k] = mass
			if mass > 0 {
				reception[postAuthor[k]] = append(reception[postAuthor[k]], mass)
				all = append(all, mass)
			}
		}
		unknown := lowerMedian(all)
		var totals []int64
		for _, d := range agents {
			var t int64
			for _, y := range transfersIn[d] {
				t += sig[y]
			}
			if t > 0 {
				totals = append(totals, t)
			}
		}
		agentMedian := lowerMedian(totals)
		// Scores.
		for _, k := range keys {
			xs := objects[k]
			switch k[0] {
			case 'p':
				p0 := unknown
				if rec := reception[postAuthor[k]]; int64(len(rec)) >= st.PriorMinPosts {
					p0 = lowerMedian(rec)
				}
				for _, x := range xs {
					var R, B int64
					for _, y := range xs {
						if acts[y].at < acts[x].at {
							B += sig[y]
						}
						if y != x && indep(acts[x].src, acts[y].src) {
							R += sig[y]
						}
					}
					r[x], on[x] = score(R, max(p0, B, 1)), true
				}
			case 'c':
				for _, x := range xs {
					var same, other, B int64
					for _, y := range xs {
						if acts[y].against == acts[x].against && acts[y].at < acts[x].at {
							B += sig[y]
						}
						if y == x || !indep(acts[x].src, acts[y].src) {
							continue
						}
						if acts[y].against == acts[x].against {
							same += sig[y]
						} else {
							other += sig[y]
						}
					}
					on[x] = same+other > 0
					r[x] = score(max(0, same-other), max(B, 1))
				}
			case 'g':
				for _, x := range xs {
					var R, before int64
					for _, y := range transfersIn[dst[x]] {
						switch {
						case acts[y].at < acts[x].at:
							before += sig[y]
						case acts[y].at > acts[x].at && indep(acts[x].src, acts[y].src):
							R += sig[y]
						}
					}
					left := max(0, asOf-acts[x].at)
					var P int64
					if before > 0 {
						P = mulDiv(before, left, max(1, acts[x].at-ws))
					} else {
						P = mulDiv(agentMedian, left, window)
					}
					r[x], on[x] = score(R, max(P, 1)), true
					if pen[dst[x]] > 0 {
						r[x] = -1e6
					}
				}
			}
		}
		// Settlement: losers pay winners within a pot (an author's posts, a
		// claim, an agent).
		pots := map[string][]int{}
		var potKeys []string
		for _, k := range keys {
			pot := k
			if k[0] == 'p' {
				pot = "a\x00" + acts[objects[k][0]].dst
			}
			if _, ok := pots[pot]; !ok {
				potKeys = append(potKeys, pot)
			}
			pots[pot] = append(pots[pot], objects[k]...)
		}
		sort.Strings(potKeys)
		var back int64
		for _, pk := range potKeys {
			var L, G int64
			for _, x := range pots[pk] {
				switch {
				case !on[x]:
				case r[x] < 0:
					L += mulDiv(sig[x], -r[x], 2e6)
				case r[x] > 0:
					G += mulDiv(sig[x], r[x], 2e6)
				}
			}
			paid := min(L, G)
			var taken, given int64
			if paid > 0 {
				for _, x := range pots[pk] {
					if on[x] && r[x] < 0 {
						t := mulDiv(mulDiv(sig[x], -r[x], 2e6), paid, L)
						taken += t
						judged[src[x]] -= t
					}
				}
				for _, x := range pots[pk] {
					if on[x] && r[x] > 0 {
						g := mulDiv(mulDiv(sig[x], r[x], 2e6), taken, G)
						given += g
						judged[src[x]] += g
					}
				}
			}
			back += taken - given
			// Vouchers of a penalised agent lose half their stake times the
			// penalty, to the seeds.
			if pk[0] == 'g' {
				for _, x := range pots[pk] {
					if burn := mulDiv(sig[x], pen[dst[x]], 2e6); burn > 0 {
						judged[src[x]] -= burn
						back += burn
					}
				}
			}
		}
		var deficit int64
		removed := make([]int64, n)
		for i := range next {
			next[i] += judged[i]
			removed[i] = mulDiv(max(0, next[i]), pen[i], 1e6)
			next[i] -= removed[i]
			back += removed[i]
			if next[i] < 0 {
				deficit -= next[i]
				next[i] = 0
			}
		}
		back = max(0, back-deficit)
		for i := range next {
			if S > 0 && eff[i] > 0 {
				next[i] += mulDiv(back, eff[i], S)
			}
			raw[i] = next[i] + removed[i]
		}
		c = next
	}
	res.c, res.raw, res.judged = c, raw, judged
	return res
}

// score is (R − P) / (R + P) in ppm, P ≥ 1.
func score(R, P int64) int64 {
	if R >= P {
		return mulDiv(R-P, 1e6, R+P)
	}
	return -mulDiv(P-R, 1e6, R+P)
}

// lowerMedian is the lower median of xs (0 for none); xs is sorted in place.
func lowerMedian(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs[(len(xs)-1)/2]
}
