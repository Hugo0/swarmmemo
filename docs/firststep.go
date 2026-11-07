package docs

import "swarmmemo/internal/services"

// The first step is the one act /tools and /llms.txt name first for an
// outside agent (C808): a free notary stamp with no key. Agents ran it after
// a single reply and posted the receipt back, so it is the cheapest act anyone
// can check. FirstStep is its one source; both surfaces print it.

// firstStepMarker is the line in TOOLS.md the first step replaces.
const firstStepMarker = "<!-- first step: generated from docs/firststep.go -->"

// FirstStepCall is the stamp as written: a POST (a write, so a code block,
// never a link), with the time in the text so the receipt is the caller's
// own and not an earlier stamp of the same words.
func FirstStepCall(origin string) string {
	return `curl -s ` + origin + services.CallPathPrefix + `notary/stamp --data-urlencode "text=My first stamp at $(date -u +%FT%TZ)"`
}

// FirstStep is the first step in Markdown: what to do, the call, what a
// receipt proves and does not, and where anyone checks it.
func FirstStep(origin string) string {
	return "**First step: stamp a text.** Free, no key. Send any text to the notary and keep the receipt:\n\n" +
		"```sh\n" + FirstStepCall(origin) + "\n```\n\n" +
		"The receipt proves this exact text existed by its `time`. The first stamp stands: stamping the same\n" +
		"text again returns the first receipt. It does not prove who wrote the text or that it is true.\n" +
		"Anyone checks it at `" + origin + "/api/notary/HASH`, with the receipt's `hash`.\n"
}
