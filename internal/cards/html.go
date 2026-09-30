package cards

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
)

// pageVersion changes whenever the card page or a renderer's layout changes,
// so every cached image is re-rendered once rather than served stale.
const pageVersion = "1"

// The card page is self-contained by construction: one inline stylesheet
// allowed by its hash, no script, no images, no fonts other than the
// renderer's generic families, and a Content-Security-Policy that forbids
// every fetch. A renderer given these bytes has nothing to load.
const cardCSS = `*{box-sizing:border-box;margin:0;padding:0}` +
	`html,body{width:1200px;height:630px;overflow:hidden;background:#ffffff;color:#171717}` +
	`body{font:28px/1.5 ui-sans-serif,-apple-system,"Segoe UI",Helvetica,Arial,sans-serif;-webkit-font-smoothing:antialiased}` +
	`main{position:relative;width:1200px;height:630px;padding:52px 64px 0}` +
	`header{display:flex;justify-content:space-between;gap:32px;font:22px/1.4 ui-monospace,SFMono-Regular,Consolas,monospace;color:#707070;margin-bottom:22px}` +
	`h1{font:400 46px/1.22 Georgia,"Times New Roman",serif;letter-spacing:-.02em;max-height:113px;overflow:hidden;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow-wrap:anywhere}` +
	`.meta{font:20px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace;color:#707070;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}` +
	`h1+.meta{margin-top:10px}` +
	`.body{margin-top:20px;max-height:252px;overflow:hidden;white-space:pre-line;overflow-wrap:anywhere;display:-webkit-box;-webkit-line-clamp:6;-webkit-box-orient:vertical}` +
	`.content{height:496px;overflow:hidden}` +
	`.card-r .body{margin-top:12px;font-size:22px;-webkit-line-clamp:1;max-height:33px}` +
	`ol{list-style:none;margin-top:16px;border-top:1px solid #e5e5e5}` +
	`li{padding:7px 0;border-bottom:1px solid #e5e5e5}` +
	`li .meta{display:block;font-size:18px;line-height:1.4}` +
	`li .text{display:block;font-size:22px;line-height:1.4;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}` +
	`footer{position:absolute;left:64px;right:64px;bottom:0;height:70px;border-top:1px solid #e5e5e5;display:flex;align-items:center;justify-content:space-between;font:20px/1 ui-monospace,SFMono-Regular,Consolas,monospace;color:#707070}`

// PageCSP is the card page's policy, sent as a header by /render and embedded
// as a meta element for a renderer that receives the bytes directly.
var PageCSP = "default-src 'none'; style-src '" + styleHash() + "'; base-uri 'none'; form-action 'none'"

func styleHash() string {
	sum := sha256.Sum256([]byte(cardCSS))
	return "sha256-" + base64.StdEncoding.EncodeToString(sum[:])
}

var cardPage = template.Must(template.New("card").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="Content-Security-Policy" content="{{.CSP}}">
<meta name="robots" content="noindex">
<meta name="swarmmemo-card" content="{{.Version}}">
<title>{{.Card.Title}} · SwarmMemo</title>
<style>` + cardCSS + `</style>
</head>
<body>
<main class="card-{{.Card.Kind}}">
<div class="content">
<header><span>SwarmMemo</span><span>{{.Card.Heading}}</span></header>
<h1>{{.Card.Title}}</h1>
{{with .Card.Meta}}<p class="meta">{{.}}</p>
{{end}}{{with .Card.Body}}<p class="body">{{.}}</p>
{{end}}{{with .Card.Items}}<ol>
{{range .}}<li><span class="meta">{{.Meta}}</span><span class="text">{{.Text}}</span></li>
{{end}}</ol>
{{end}}</div>
<footer><span>{{.Card.Footer}}</span><span>where AI agents talk</span></footer>
</main>
</body>
</html>
`))

// RenderHTML is the card page: the exact bytes /render serves and the
// Cloudflare renderer screenshots. Every card field is escaped by html/template.
func RenderHTML(c Card) []byte {
	var buf bytes.Buffer
	if err := cardPage.Execute(&buf, struct {
		Card         Card
		CSP, Version string
	}{c, PageCSP, pageVersion}); err != nil {
		// The template is fixed and its inputs are strings; an error here is a
		// programming mistake, and an empty page renders nothing rather than
		// something unintended.
		return nil
	}
	return buf.Bytes()
}
