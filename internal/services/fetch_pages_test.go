package services

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// Dense official pages keep their whole body (C684). The fixtures are
// trimmed copies of public-domain Federal Reserve pages, saved as served:
// their head, mega-menu navigation, share list and footer with the
// statement or minutes between. A statement page is most of its bytes in
// markup and navigation, so its text is short, and all of the statement is
// in it.
func TestFetchKeepsTheBodyOfDenseOfficialPages(t *testing.T) {
	for _, tc := range []struct {
		file, title string
		keep, drop  []string
	}{
		{
			file:  "fomc_statement_20261028.html",
			title: "Federal Reserve Board - Federal Reserve issues FOMC statement",
			keep: []string{
				"### Federal Reserve issues FOMC statement",
				"The Federal Open Market Committee approved the following statement for release by a 10 – 2 vote:",
				"The Committee decided to maintain the target range for the federal funds rate at 3-3/4 to 4 percent, in support of the Federal Reserve's dual mandate.",
				"Economic activity is expanding at a strong pace.",
				"The Committee will deliver price stability.",
				"Voting against the monetary policy action were Lisa D. Cook and Beth M. Hammack, who preferred to raise the target range for the federal funds rate by 1/4 percentage point at this meeting.",
				"[Implementation Note issued October 28, 2026](https://www.federalreserve.gov/newsevents/pressreleases/monetary20261028a1.htm)",
			},
			drop: []string{"About<br />", "Monetary Policy](", "About the Fed", "visibility", "Please enable JavaScript", "gradient"},
		},
		{
			file:  "fomc_minutes_20250917.html",
			title: "The Fed - Monetary Policy:",
			keep: []string{
				"### Minutes of the Federal Open Market Committee",
				"A joint meeting of the Federal Open Market Committee and the Board of Governors of the Federal Reserve System was held in the offices of the Board of Governors on Tuesday, September 16, 2025",
				"Developments in Financial Markets and Open Market Operations",
				"Committee Policy Actions",
				"almost all members agreed to lower the target range for the federal funds rate 1/4 percentage point to 4 to 4-1/4 percent.",
				"They would be prepared to adjust the stance of monetary policy as appropriate",
			},
			drop: []string{"Monetary Policy](", "About the Fed", "visibility"},
		},
	} {
		raw, err := os.ReadFile("testdata/fetch/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		base, _ := url.Parse("https://www.federalreserve.gov/newsevents/pressreleases/x.htm")
		got := htmlToText(string(raw), base)
		flat := strings.Join(strings.Fields(got.Text), " ")
		if !strings.HasPrefix(got.Title, tc.title) {
			t.Errorf("%s: title %q", tc.file, got.Title)
		}
		for _, s := range tc.keep {
			if !strings.Contains(flat, strings.Join(strings.Fields(s), " ")) {
				t.Errorf("%s: the body lost %q:\n%s", tc.file, s, got.Text)
			}
		}
		for _, s := range tc.drop {
			if strings.Contains(got.Text, s) {
				t.Errorf("%s: %q leaked into the text", tc.file, s)
			}
		}
	}
}

// A menu left open (its </div> or </nav> missing) ends where the page's
// main content starts, instead of swallowing the page; scripts, templates
// and a menu holding one still run to their own closing tag, so nothing a
// reader is never shown comes back.
func TestFetchMenuLeftOpenKeepsTheMain(t *testing.T) {
	for doc, want := range map[string]string{
		`<body><div role="navigation"><ul><li><a href="/a">MENU</a></li></ul><main><p>STATEMENT text.</p></main></body>`:       "STATEMENT text.",
		`<body><nav><ul><li>MENU<div id="content" role="main"><p>STATEMENT text.</p></div></body>`:                             "STATEMENT text.",
		`<body><footer>FOOT<main>STATEMENT text.</main></body>`:                                                                "STATEMENT text.",
		`<body><nav>MENU</nav><main>STATEMENT text.</main></body>`:                                                             "STATEMENT text.",
		`<body><nav><template><main>HIDDEN</main></template></nav><p>after</p></body>`:                                         "after",
		`<body><nav><object><main>HIDDEN</main></object><main>HIDDEN too</main></nav><p>after</p></body>`:                      "after",
		`<body><template><main>HIDDEN</main></template><script>x="<main>HIDDEN</main>"</script><svg><main>HIDDEN</main></svg>`: "",
	} {
		got := htmlToText(doc, nil).Text
		if got != want {
			t.Errorf("%s\n got %q, want %q", doc, got, want)
		}
	}
}
