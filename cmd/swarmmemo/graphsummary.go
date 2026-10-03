package main

// The /graph AI summaries' configuration. See deploy/RUNBOOK.md#graph-summaries.

import (
	"bufio"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"swarmmemo/internal/httpapi"
)

// graphSummaryConfig reads the summary provider from the environment: the
// OpenRouter key from OPENROUTER_API_KEY, or from OPENROUTER_KEY_FILE (a
// file holding the key alone or an OPENROUTER_API_KEY=... line). Without a
// key summaries are off. GRAPH_SUMMARY_MODEL, GRAPH_SUMMARY_PRICES ("IN,OUT"
// USD per million tokens), GRAPH_SUMMARY_DAILY_USD and GRAPH_SUMMARY_UNTIL
// (YYYY-MM-DD, the last UTC day) override the defaults.
func graphSummaryConfig(publicURL string) *httpapi.GraphSummaryConfig {
	key := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if key == "" {
		if path := os.Getenv("OPENROUTER_KEY_FILE"); path != "" {
			key = readKeyFile(path)
			if key == "" {
				slog.Warn("OPENROUTER_KEY_FILE has no key; graph summaries are off")
			}
		}
	}
	if key == "" {
		return nil
	}
	cfg := &httpapi.GraphSummaryConfig{Provider: &httpapi.OpenRouter{Key: key, Referer: publicURL}, Model: strings.TrimSpace(os.Getenv("GRAPH_SUMMARY_MODEL"))}
	if v := os.Getenv("GRAPH_SUMMARY_PRICES"); v != "" {
		in, out, ok := strings.Cut(v, ",")
		a, e1 := strconv.ParseFloat(strings.TrimSpace(in), 64)
		b, e2 := strconv.ParseFloat(strings.TrimSpace(out), 64)
		if !ok || e1 != nil || e2 != nil || a < 0 || b < 0 {
			slog.Warn("GRAPH_SUMMARY_PRICES is IN,OUT in USD per million tokens; graph summaries are off")
			return nil
		}
		cfg.InPerM, cfg.OutPerM = a, b
	} else if cfg.Model != "" && cfg.Model != httpapi.GraphSummaryDefaultModel {
		slog.Warn("GRAPH_SUMMARY_MODEL needs GRAPH_SUMMARY_PRICES so the daily cap can be kept; graph summaries are off")
		return nil
	}
	if v := os.Getenv("GRAPH_SUMMARY_DAILY_USD"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 100 {
			slog.Warn("GRAPH_SUMMARY_DAILY_USD is a number of USD above 0 and at most 100; graph summaries are off")
			return nil
		}
		cfg.DailyUSD = n
	}
	if v := os.Getenv("GRAPH_SUMMARY_UNTIL"); v != "" {
		day, err := time.Parse("2006-01-02", v)
		if err != nil {
			slog.Warn("GRAPH_SUMMARY_UNTIL is YYYY-MM-DD; graph summaries are off")
			return nil
		}
		cfg.Until = day.AddDate(0, 0, 1)
	}
	return cfg
}

func readKeyFile(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok {
			if strings.TrimSpace(strings.TrimPrefix(name, "export ")) == "OPENROUTER_API_KEY" {
				return strings.Trim(strings.TrimSpace(value), `"'`)
			}
			continue
		}
		return line
	}
	return ""
}
