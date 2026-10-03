package graphmodel

import "math"

// circle is one disc to pack: its radius in, its centre out.
type circle struct{ x, y, r float64 }

type chainNode struct {
	c          *circle
	next, prev *chainNode
}

// packSiblings places circles (sorted or not) without overlap around the
// origin with the front-chain algorithm (Wang et al., as in d3-hierarchy),
// recentres them on their enclosing circle and returns its radius.
func packSiblings(cs []*circle) float64 {
	n := len(cs)
	if n == 0 {
		return 0
	}
	a := cs[0]
	a.x, a.y = 0, 0
	if n == 1 {
		return a.r
	}
	b := cs[1]
	a.x, b.x, b.y = -b.r, a.r, 0
	if n == 2 {
		return a.r + b.r
	}
	c := cs[2]
	place(b, a, c)
	na, nb, nc := &chainNode{c: a}, &chainNode{c: b}, &chainNode{c: c}
	na.next, nc.prev = nb, nb
	nb.next, na.prev = nc, nc
	nc.next, nb.prev = na, na

pack:
	for i := 3; i < n; i++ {
		c := cs[i]
		place(na.c, nb.c, c)
		cn := &chainNode{c: c}
		j, k := nb.next, na.prev
		sj, sk := nb.c.r, na.c.r
		for steps := 0; ; steps++ {
			if sj <= sk {
				if intersects(j.c, cn.c) {
					nb = j
					na.next, nb.prev = nb, na
					i--
					continue pack
				}
				sj += j.c.r
				j = j.next
			} else {
				if intersects(k.c, cn.c) {
					na = k
					na.next, nb.prev = nb, na
					i--
					continue pack
				}
				sk += k.c.r
				k = k.prev
			}
			if j == k.next || steps > 4*n {
				break
			}
		}
		cn.prev, cn.next = na, nb
		na.next, nb.prev = cn, cn
		nb = cn
		// Pick the pair closest to the centre to grow from.
		aa := score(na)
		for x := cn.next; x != nb; x = x.next {
			if s := score(x); s < aa {
				na, aa = x, s
			}
		}
		nb = na.next
	}
	ex, ey, er := enclose(cs)
	for _, c := range cs {
		c.x -= ex
		c.y -= ey
	}
	return er
}

func place(b, a, c *circle) {
	dx, dy := b.x-a.x, b.y-a.y
	d2 := dx*dx + dy*dy
	if d2 > 0 {
		a2 := (a.r + c.r) * (a.r + c.r)
		b2 := (b.r + c.r) * (b.r + c.r)
		if a2 > b2 {
			x := (d2 + b2 - a2) / (2 * d2)
			y := math.Sqrt(math.Max(0, b2/d2-x*x))
			c.x = b.x - x*dx - y*dy
			c.y = b.y - x*dy + y*dx
		} else {
			x := (d2 + a2 - b2) / (2 * d2)
			y := math.Sqrt(math.Max(0, a2/d2-x*x))
			c.x = a.x + x*dx - y*dy
			c.y = a.y + x*dy + y*dx
		}
	} else {
		c.x = a.x + c.r
		c.y = a.y
	}
}

func intersects(a, b *circle) bool {
	dr := a.r + b.r - 1e-6
	dx, dy := b.x-a.x, b.y-a.y
	return dr > 0 && dr*dr > dx*dx+dy*dy
}

func score(n *chainNode) float64 {
	a, b := n.c, n.next.c
	ab := a.r + b.r
	dx := (a.x*b.r + b.x*a.r) / ab
	dy := (a.y*b.r + b.y*a.r) / ab
	return dx*dx + dy*dy
}

// enclose returns a circle containing every circle: centred on the
// area-weighted centroid, a few percent looser than the minimum at most.
func enclose(cs []*circle) (x, y, r float64) {
	var sw float64
	for _, c := range cs {
		w := c.r*c.r + 1e-9
		x += c.x * w
		y += c.y * w
		sw += w
	}
	x /= sw
	y /= sw
	for _, c := range cs {
		if d := math.Hypot(c.x-x, c.y-y) + c.r; d > r {
			r = d
		}
	}
	return x, y, r
}
