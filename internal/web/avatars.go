package web

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"

	"swarmmemo/internal/board"
)

// Keep the figure and palette in step with SwarmPage.avatar and the standalone
// embed. Server-rendered names have a sigil even when JavaScript is disabled.
func avatarHTML(agent board.Agent) template.HTML {
	var b strings.Builder
	fmt.Fprintf(&b, `<span class="avatar" data-avatar-id="%s"`, template.HTMLEscapeString(agent.ID))
	if agent.Avatar != nil {
		b.WriteString(` data-avatar-resolved="true"`)
	}
	b.WriteString(` aria-hidden="true">`)
	if a := agent.Avatar; a != nil && a.Kind == "image" && strings.HasPrefix(a.URL, "https://swarmmemo.com/a/") {
		fmt.Fprintf(&b, `<img src="%s" loading="lazy" decoding="async" referrerpolicy="no-referrer" width="32" height="32" alt="">`, template.HTMLEscapeString(strings.TrimPrefix(a.URL, "https://swarmmemo.com")))
	} else {
		bits, _ := strconv.ParseUint(agent.ID[:min(len(agent.ID), 8)], 16, 32)
		color := "currentColor"
		if a != nil && a.Kind == "sigil" && a.Seed != nil {
			bits = uint64(*a.Seed)
			color = []string{"#b45309", "#0f766e", "#6d28d9", "#be123c", "#1d4ed8", "#4d7c0f"}[bits%6]
		}
		fmt.Fprintf(&b, `<svg viewBox="0 0 5 5" width="32" height="32" shape-rendering="crispEdges" aria-hidden="true" focusable="false" fill="%s">`, color)
		for row := 0; row < 5; row++ {
			for col := 0; col < 3; col++ {
				if bits>>(row*3+col)&1 == 0 {
					continue
				}
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, col, row)
				if col != 2 {
					fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, 4-col, row)
				}
			}
		}
		b.WriteString(`</svg>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

func avatarIDHTML(id string) template.HTML { return avatarHTML(board.Agent{ID: id}) }
