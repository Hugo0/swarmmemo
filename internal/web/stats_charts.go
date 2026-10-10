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
	// Readout series are not drawn; their values appear in the readout.
	Readout bool
	// Bytes rounds the scale in whole KB or MB.
	Bytes bool
}

// lineChart is one figure of /stats.
type lineChart struct {
	Title, Note, Summary string
	Big, BigNote         string
	Legend               []chartKey
	Max, Half            string
	AltMax, AltHalf      string
	// Xs are the x labels joined by "|", for the readout.
	Xs    string
	SVG   template.HTML
	Axis  []axisLabel
	Small bool
}

// chartKey is a legend entry: a drawn series with its total over the range.
type chartKey struct {
	Class, Label, Term, Total, Share string
	Alt, Area                        bool
}

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
	b.WriteString(`<svg class="chart-svg" viewBox="0 0 1000 100" preserveAspectRatio="none" aria-hidden="true" focusable="false"><line class="grid" x1="0" x2="1000" y1="0" y2="0" vector-effect="non-scaling-stroke"/><line class="grid" x1="0" x2="1000" y1="50" y2="50" vector-effect="non-scaling-stroke"/>`)
	legend := []string{}
	for _, s := range series {
		var total int64
		for _, v := range s.Values {
			total += v
		}
		if s.Readout {
			fmt.Fprintf(&b, `<g class="readout-only" data-label="%s" data-values="%s"></g>`, html.EscapeString(s.Label), html.EscapeString(joinFormatted(s.Values, s.Format)))
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
			fmt.Fprintf(&b, `<polygon class="area %s" points="0,100 %s 1000,100"/>`, s.Class, strings.Join(points, " "))
		}
		fmt.Fprintf(&b, `<polyline class="line %s" points="%s" vector-effect="non-scaling-stroke" data-label="%s" data-values="%s" data-ys="%s"/>`, s.Class, strings.Join(points, " "), html.EscapeString(s.Label), html.EscapeString(joinFormatted(s.Values, s.Format)), strings.Join(ys, "|"))
		c.Legend = append(c.Legend, chartKey{Class: s.Class, Label: s.Label, Term: s.Term, Total: s.Format(total), Alt: s.Alt})
		legend = append(legend, s.Label+" "+s.Format(total))
	}
	b.WriteString(`<line class="baseline" x1="0" x2="1000" y1="100" y2="100" vector-effect="non-scaling-stroke"/></svg>`)
	c.SVG = template.HTML(b.String())
	if drawn == 1 {
		c.Legend = nil
	}
	c.Summary = title + ". " + note + " Totals over the range: " + strings.Join(legend, ", ") + ". The tables below list each day."
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
	b.WriteString(`<svg class="chart-svg" viewBox="0 0 1000 100" preserveAspectRatio="none" aria-hidden="true" focusable="false">`)
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
		fmt.Fprintf(&b, `<polygon class="band %s" points="%s %s" vector-effect="non-scaling-stroke" data-label="%s" data-values="%s"/>`, s.Class, strings.Join(upper, " "), strings.Join(lower, " "), html.EscapeString(s.Label), html.EscapeString(strings.Join(values, "|")))
		c.Legend = append(c.Legend, chartKey{Class: s.Class, Label: s.Label, Term: s.Term, Total: count(total), Share: share(total, grand), Area: true})
		legend = append(legend, s.Label+" "+count(total)+" ("+share(total, grand)+")")
	}
	b.WriteString(`<line class="baseline" x1="0" x2="1000" y1="100" y2="100" vector-effect="non-scaling-stroke"/></svg>`)
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
	return template.HTML(`<svg class="spark" viewBox="0 0 100 24" preserveAspectRatio="none" aria-hidden="true" focusable="false"><polygon class="spark-area" points="0,24 ` + line + ` 100,24"/><polyline class="spark-line" points="` + line + `" vector-effect="non-scaling-stroke"/></svg>`)
}
