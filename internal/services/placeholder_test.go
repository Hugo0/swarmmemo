package services

import (
	"errors"
	"strings"
	"testing"

	"swarmmemo/internal/allowance"
)

func TestPlaceholder(t *testing.T) {
	for in, want := range map[string]string{
		"SOURCE_URL": "SOURCE_URL", "{URL}": "{URL}", "<PAGE_URL>": "<PAGE_URL>", " SHA256_OF_THE_PAGE_TEXT\n": "SHA256_OF_THE_PAGE_TEXT",
	} {
		if got := Placeholder(in); got != want {
			t.Errorf("Placeholder(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"", "https://example.com/", "http://EXAMPLE.COM/", "source_url", "hello", "OK", "AI", "{ok}",
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", "Source_URL", "SOURCE URL",
		strings.Repeat("A", PlaceholderBytes+1),
	} {
		if got := Placeholder(in); got != "" {
			t.Errorf("Placeholder(%q) = %q, want none", in, got)
		}
	}
}

// fetch_invalid_url names a placeholder in its own sentence (C837); any
// other invalid url carries none.
func TestFetchURLPlaceholderSentence(t *testing.T) {
	var e *allowance.Err
	if _, err := parseFetchURL(" {URL} "); !errors.As(err, &e) || e.Code != "fetch_invalid_url" || e.Message != FetchPlaceholderSentence("{URL}") {
		t.Fatalf("{URL}: %#v", err)
	}
	if _, err := parseFetchURL("example.com"); !errors.As(err, &e) || e.Code != "fetch_invalid_url" || e.Message != "" {
		t.Fatalf("example.com: %#v", err)
	}
}
