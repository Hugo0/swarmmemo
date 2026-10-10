package web

import (
	"fmt"
	"html"
	"html/template"
	"strconv"
	"strings"
)

// The /stats charts are server-rendered SVG, so they draw without
// JavaScript. A chart is drawn in a 1000 by 100 viewBox stretched to the
// plot (preserveAspectRatio none, strokes kept at their width by
// vector-effect). Each drawn series carries its formatted values
// (data-values) and positions (data-ys, percent from the top), and a
// readout-only series its values alone; assets/stats.js turns these into
// a crosshair with every series' value at the hovered or focused x.
// Without it, the legend's totals, the aria-label and the tables under the
// charts carry the numbers; no per-point markup is repeated for it.
//
// Every SVG carries its own width, height, fill and stroke as attributes
// (currentColor, with opacity for the lighter marks), so a chart still
// draws as thin lines and light bands if the stylesheet fails to load; the
// stylesheet's class rules refine them.

const chartWidth = 1000

// chartSeries is one series of a chart.
type chartSeries struct {
	Class, Label string
	// Term is the glossary key explaining Label in the legend, if any.
	Term   string
	Values []int64
	Format func(int64) string
	// Alt draws the series against the second scale, labelled on the left.
	Alt bool
	// Readout series are not drawn; their values appear in the readout,
	// and with Legend also in the legend, with their totals.
	Readout, Legend bool
	// NoTotal leaves the series' total out of the legend, for a count
	// whose days do not add up (distinct agents per day).
	NoTotal bool
	// Bytes rounds the scale in whole KB or MB.
	Bytes bool
}

// lineChart is one figure of /stats.
type lineChart struct {
	Title, Note, Summary string
	Legend               []chartKey
	Max, Half            string
	AltMax, AltHalf      string
	// Xs are the x labels joined by "|", for the readout.
	Xs   string
	SVG  template.HTML
	Axis []axisLabel
}

// chartKey is a legend entry: a drawn series with its total over the range.
type chartKey struct {
	Class, Label, Term, Total, Share string
	// Alt is on the second scale; Area a band; Split a readout series
	// listed with its total but not drawn.
	Alt, Area, Split bool
	// Key is the legend's mark, drawn like the series.
	Key template.HTML
}

// keySVG is the legend mark of a line or band of class cls.
func keySVG(cls string, area bool) template.HTML {
	mark := `<line class="line ` + cls + `" x1="0" x2="16" y1="5" y2="5"` + lineAttrs(cls) + `/>`
	if area {
		mark = `<rect class="band ` + cls + `" x="0" y="0" width="16" height="10" fill="currentColor" fill-opacity="` + bandOpacity(cls) + `"/>`
	}
	return template.HTML(`<svg class="key" width="16" height="10" viewBox="0 0 16 10" aria-hidden="true" focusable="false">` + mark + `</svg>`)
}

// lineAttrs are the presentation attributes of a line of class cls: the
// fallback the stylesheet refines.
func lineAttrs(cls string) string {
	out := ` fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"`
	switch cls {
	case "s-2":
		out += ` stroke-dasharray="6 4"`
	case "s-3":
		out += ` stroke-dasharray="2 3"`
	case "s-alt":
		out += ` stroke-dasharray=".5 5"`
	}
	return out
}

// bandOpacity is the fallback fill opacity of band class b-N.
func bandOpacity(cls string) string {
	switch cls {
	case "b-1":
		return ".9"
	case "b-2":
		return ".65"
	case "b-3":
		return ".45"
	case "b-4":
		return ".3"
	case "b-5":
		return ".18"
	}
	return ".08"
}

// chartOpen starts a plot's SVG; gridLines draws its grid at the top and
// middle, chartClose its baseline.
const (
	chartOpen  = `<svg class="chart-svg" width="100%" height="150" viewBox="0 0 1000 100" preserveAspectRatio="none" aria-hidden="true" focusable="false">`
	gridLines  = `<line class="grid" x1="0" x2="1000" y1="0" y2="0" stroke="currentColor" stroke-opacity=".15" vector-effect="non-scaling-stroke"/><line class="grid" x1="0" x2="1000" y1="50" y2="50" stroke="currentColor" stroke-opacity=".15" vector-effect="non-scaling-stroke"/>`
	chartClose = `<line class="baseline" x1="0" x2="1000" y1="100" y2="100" stroke="currentColor" stroke-opacity=".4" vector-effect="non-scaling-stroke"/></svg>`
)

// axisLabel is one tick label; a Minor one is left out on narrow screens.
type axisLabel struct {
	Left  float64
	Text  string
	Minor bool
}

// chartX is where point i of n sits, in viewBox units.
func chartX(i, n int) float64 {
	if n <= 1 {
		return chartWidth / 2
	}
	return float64(i) * chartWidth / float64(n-1)
}

// scaleTop rounds a peak up to a readable top of scale.
func scaleTop(peak int64, bytes bool) int64 {
	if bytes {
		for _, u := range []int64{1 << 20, 1 << 10} {
			if peak >= u {
				return niceCeiling((peak+u-1)/u) * u
			}
		}
	}
	return niceCeiling(peak)
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }

// axisFor labels the x positions where label returns text; on a long range
// every other label is minor, so a phone shows half of them.
func axisFor(n int, label func(int) string) []axisLabel {
	var out []axisLabel
	for i := range n {
		if text := label(i); text != "" {
			left := 50.0
			if n > 1 {
				left = float64(i) * 100 / float64(n-1)
			}
			out = append(out, axisLabel{Left: left, Text: text, Minor: len(out)%2 == 1 && n > 60})
		}
	}
	return out
}

func joinFormatted(values []int64, format func(int64) string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = format(v)
	}
	return strings.Join(out, "|")
}

// buildLineChart draws series as lines over xs, a light area under the
// first when it is alone. A series marked Alt is drawn against its own
// scale. The last x is still filling.
func buildLineChart(title, note string, xs []string, axis []axisLabel, series []chartSeries) lineChart {
	c := lineChart{Title: title, Note: note, Xs: strings.Join(xs, "|"), Axis: axis}
	var peak, altPeak int64
	var bytes, altBytes bool
	format := count
	drawn := 0
	for _, s := range series {
		if s.Readout {
			continue
		}
		drawn++
		for _, v := range s.Values {
			if s.Alt {
				altPeak = max(altPeak, v)
			} else {
				peak = max(peak, v)
			}
		}
		if s.Alt {
			altBytes = s.Bytes
		} else {
			bytes, format = s.Bytes, s.Format
		}
	}
	top, altTop := scaleTop(peak, bytes), scaleTop(altPeak, altBytes)
	c.Max, c.Half = format(top), format(top/2)
	if top%2 != 0 {
		c.Half = ""
	}
	n := len(xs)
	var b strings.Builder
	b.WriteString(chartOpen + gridLines)
	legend := []string{}
	var split []chartKey
	for _, s := range series {
		var total int64
		for _, v := range s.Values {
			total += v
		}
		if s.Readout {
			fmt.Fprintf(&b, `<g class="readout-only" data-label="%s" data-values="%s"></g>`, html.EscapeString(s.Label), html.EscapeString(joinFormatted(s.Values, s.Format)))
			if s.Legend {
				split = append(split, chartKey{Label: s.Label, Term: s.Term, Total: s.Format(total), Split: true})
				legend = append(legend, s.Label+" "+s.Format(total))
			}
			continue
		}
		scale := top
		if s.Alt {
			scale = altTop
			c.AltMax, c.AltHalf = s.Format(altTop), s.Format(altTop/2)
			if altTop%2 != 0 {
				c.AltHalf = ""
			}
		}
		points := make([]string, n)
		ys := make([]string, n)
		for i, v := range s.Values {
			y := 100 - min(float64(v)*100/float64(scale), 100)
			points[i] = num(chartX(i, n)) + "," + num(y)
			ys[i] = num(y)
		}
		if drawn == 1 {
			fmt.Fprintf(&b, `<polygon class="area %s" points="0,100 %s 1000,100" fill="currentColor" fill-opacity=".06" stroke="none"/>`, s.Class, strings.Join(points, " "))
		}
		fmt.Fprintf(&b, `<polyline class="line %s" points="%s"%s vector-effect="non-scaling-stroke" data-label="%s" data-values="%s" data-ys="%s"/>`, s.Class, strings.Join(points, " "), lineAttrs(s.Class), html.EscapeString(s.Label), html.EscapeString(joinFormatted(s.Values, s.Format)), strings.Join(ys, "|"))
		key := chartKey{Class: s.Class, Label: s.Label, Term: s.Term, Total: s.Format(total), Alt: s.Alt, Key: keySVG(s.Class, false)}
		if s.NoTotal {
			key.Total = ""
		} else {
			legend = append(legend, s.Label+" "+s.Format(total))
		}
		c.Legend = append(c.Legend, key)
	}
	b.WriteString(chartClose)
	c.SVG = template.HTML(b.String())
	if drawn == 1 && len(split) == 0 {
		c.Legend = nil
	}
	c.Legend = append(c.Legend, split...)
	c.Summary = strings.TrimSpace(title+". "+note) + " Totals over the range: " + strings.Join(legend, ", ") + ". All numbers, at the bottom, lists each day."
	return c
}

// buildShareChart draws series as a 100% stacked area over xs: each band
// is the series' share of the x's total, so a change in the mix shows even
// while the totals move. The readout gives each count and its share.
func buildShareChart(title, note string, xs []string, axis []axisLabel, series []chartSeries) lineChart {
	c := lineChart{Title: title, Note: note, Xs: strings.Join(xs, "|"), Axis: axis, Max: "100%", Half: "50%"}
	n := len(xs)
	totals := make([]int64, n)
	var grand int64
	for _, s := range series {
		for i, v := range s.Values {
			totals[i] += v
			grand += v
		}
	}
	share := func(v, total int64) string { return percent(v, total) }
	var b strings.Builder
	b.WriteString(chartOpen)
	below := make([]float64, n) // cumulative share below the band, in percent
	legend := []string{}
	for _, s := range series {
		upper := make([]string, n)
		lower := make([]string, n)
		values := make([]string, n)
		var total int64
		for i, v := range s.Values {
			total += v
			h := 0.0
			if totals[i] > 0 {
				h = float64(v) * 100 / float64(totals[i])
			}
			x := num(chartX(i, n))
			lower[n-1-i] = x + "," + num(100-below[i])
			below[i] += h
			upper[i] = x + "," + num(100-below[i])
			values[i] = count(v) + " · " + share(v, totals[i])
			if totals[i] == 0 {
				values[i] = "0"
			}
		}
		fmt.Fprintf(&b, `<polygon class="band %s" points="%s %s" fill="currentColor" fill-opacity="%s" stroke="none" vector-effect="non-scaling-stroke" data-label="%s" data-values="%s"/>`, s.Class, strings.Join(upper, " "), strings.Join(lower, " "), bandOpacity(s.Class), html.EscapeString(s.Label), html.EscapeString(strings.Join(values, "|")))
		c.Legend = append(c.Legend, chartKey{Class: s.Class, Label: s.Label, Term: s.Term, Total: count(total), Share: share(total, grand), Area: true, Key: keySVG(s.Class, true)})
		legend = append(legend, s.Label+" "+count(total)+" ("+share(total, grand)+")")
	}
	b.WriteString(chartClose)
	c.SVG = template.HTML(b.String())
	c.Summary = title + ". " + note + " Over the range: " + strings.Join(legend, ", ") + "."
	return c
}

// sparkline draws values as a small line with a light area, for a stat
// tile; the tile's words carry the number, so it is decoration.
func sparkline(values []int64) template.HTML {
	if len(values) == 0 {
		return ""
	}
	var peak int64
	for _, v := range values {
		peak = max(peak, v)
	}
	peak = max(peak, 1)
	points := make([]string, len(values))
	for i, v := range values {
		x := 50.0
		if len(values) > 1 {
			x = float64(i) * 100 / float64(len(values)-1)
		}
		points[i] = num(x) + "," + num(22-float64(v)*20/float64(peak))
	}
	line := strings.Join(points, " ")
	return template.HTML(`<svg class="spark" width="100%" height="24" viewBox="0 0 100 24" preserveAspectRatio="none" aria-hidden="true" focusable="false"><polygon class="spark-area" points="0,24 ` + line + ` 100,24" fill="currentColor" fill-opacity=".08" stroke="none"/><polyline class="spark-line" points="` + line + `" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" vector-effect="non-scaling-stroke"/></svg>`)
}

// statsBar is a table cell's bar, width percent of the cell, sized and
// filled by its own attributes.
func statsBar(width float64) template.HTML {
	return template.HTML(`<svg class="via-bar" width="100%" height="8" aria-hidden="true" focusable="false"><rect height="100%" width="` + strconv.FormatFloat(max(0, min(width, 100)), 'f', 1, 64) + `%" fill="currentColor"/></svg>`)
}
