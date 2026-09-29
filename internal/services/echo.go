package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"swarmmemo/internal/allowance"
)

// EchoDelayMax bounds echo's simulated upstream delay.
const EchoDelayMax = 5000 // milliseconds

// echo returns its text. It exists to exercise every metering path: plain,
// it is a Local call; with simulate it stands in for an upstream, as a Remote
// call (after commit, under a deadline) or an Async one (settled later by the
// worker), with an optional delay, failure or crash. It never touches the
// network: the delay is a timer.
type echo struct{ simulate bool }

func newEcho(d Deps) Provider { return echo{simulate: d.EchoSimulate} }

func (echo) Describe() Descriptor {
	return Descriptor{
		ID:      "echo",
		Summary: `Returns its text. A test service: args.simulate {"mode":"remote"|"async","delay_ms":N,"fail":true,"crash":true} exercises the remote and async metering paths without any network, on test deployments only.`,
		Title:   "Echo",
		Line:    "A test service that returns its text, for trying a signed service call end to end.",
		Mode:    Local,
		Methods: []Method{{Name: "echo", Write: true, Signed: true, Resource: allowance.Credit, ArgsMax: 4 << 10, Price: Price{Base: 1, PerKiB: 1},
			Line:    "Return the text; simulate exercises the remote and async paths.",
			Args:    []Arg{{"text", "string", false, "any text"}, {"simulate", "object", false, `test deployments only (off in production): {"mode":"remote"|"async","delay_ms":N,"fail":true,"crash":true}`}},
			Example: json.RawMessage(`{"text":"hello"}`)}},
		MaxDuration: 15 * time.Second,
	}
}

type echoArgs struct {
	Text     string        `json:"text"`
	Simulate *echoSimulate `json:"simulate"`
}

type echoSimulate struct {
	Mode    string          `json:"mode"`
	DelayMS json.RawMessage `json:"delay_ms"`
	Fail    bool            `json:"fail"`
	Crash   bool            `json:"crash"`
}

type echoPlan struct {
	text        string
	mode        Mode
	delay       time.Duration
	fail, crash bool
}

// parse reads echo's args; simulate is refused unless the deployment turned
// it on (tests: Deps.EchoSimulate).
func (e echo) parse(raw json.RawMessage) (echoPlan, error) {
	return parseEcho(raw, e.simulate)
}

func parseEcho(raw json.RawMessage, simulate bool) (echoPlan, error) {
	var a echoArgs
	if err := StrictObject(raw, &a); err != nil {
		return echoPlan{}, err
	}
	p := echoPlan{text: a.Text}
	if a.Simulate != nil && !simulate {
		return p, refusal("invalid_service_data")
	}
	if s := a.Simulate; s != nil {
		switch s.Mode {
		case "", "local":
		case "remote":
			p.mode = Remote
		case "async":
			p.mode = Async
		default:
			return p, refusal("invalid_service_data")
		}
		if s.DelayMS != nil {
			ms, ok := Integer(s.DelayMS, EchoDelayMax)
			if !ok {
				return p, refusal("invalid_service_data")
			}
			p.delay = time.Duration(ms) * time.Millisecond
		}
		p.fail, p.crash = s.Fail, s.Crash
		// A Local call runs inside the command's transaction: it may fail,
		// but never waits or crashes there.
		if p.mode == Local && (p.delay > 0 || p.crash) {
			return p, refusal("invalid_service_data")
		}
	}
	return p, nil
}

func (e echo) ModeFor(c Call) Mode {
	p, err := e.parse(c.Args)
	if err != nil {
		return Local
	}
	return p.mode
}

func (e echo) Quote(c Call) (Quote, error) {
	if _, err := e.parse(c.Args); err != nil {
		return Quote{}, err
	}
	return Quote{Resource: allowance.Credit, Max: c.Price.For(int64(len(c.Args)))}, nil
}

func echoResult(text string, argsBytes int, used int64) Result {
	body, _ := json.Marshal(map[string]any{"echo": text, "args_bytes": argsBytes})
	public, _ := json.Marshal(map[string]any{"args_bytes": argsBytes})
	return Result{Body: body, Used: used, Public: public}
}

func (e echo) Run(ctx context.Context, tx *sql.Tx, c Call) (Result, error) {
	p, err := e.parse(c.Args)
	if err != nil {
		return Result{}, err
	}
	used := c.Price.For(int64(len(c.Args)))
	if p.mode == Async {
		data, _ := json.Marshal(echoJob{Text: p.text, Fail: p.fail, Crash: p.crash, Used: used, ArgsBytes: len(c.Args)})
		due := c.Now + int64((p.delay+time.Second-1)/time.Second)
		return Result{Job: &Job{DueAt: due, Data: data}}, nil
	}
	if p.delay > 0 {
		t := time.NewTimer(p.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return Result{}, ctx.Err()
		}
	}
	if p.crash {
		return Result{}, ErrCrash
	}
	if p.fail {
		return Result{}, refusal("upstream_failed")
	}
	return echoResult(p.text, len(c.Args), used), nil
}

type echoJob struct {
	Text      string `json:"text"`
	Fail      bool   `json:"fail"`
	Crash     bool   `json:"crash"`
	Used      int64  `json:"used"`
	ArgsBytes int    `json:"args_bytes"`
}

func (echo) Settle(ctx context.Context, job Job) (Result, error) {
	var j echoJob
	if err := json.Unmarshal(job.Data, &j); err != nil {
		return Result{}, refusal("upstream_failed")
	}
	if j.Crash {
		return Result{}, ErrCrash
	}
	if j.Fail {
		return Result{}, refusal("upstream_failed")
	}
	return echoResult(j.Text, j.ArgsBytes, j.Used), nil
}
