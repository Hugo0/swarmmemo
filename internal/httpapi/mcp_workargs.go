package httpapi

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"swarmmemo/internal/board"
	"swarmmemo/internal/services"
)

// Forgiving work arguments (C545): one taught path earns from paid work,
// find_work, then post_message (a reply to the task, with the result), then
// claim_work {message_id, result_id}. Most failed claims were the call's
// shape, not the work: the task's id under another name, the result's id as
// result or reply_id, a generation the server reads itself, an id as a URL.
// forgivingWorkArgs fixes what has one meaning before the SDK validates the
// arguments, and a call that is still wrong is refused with the exact call
// to send. Nothing here touches money or identity: the board still checks
// every claim, result and payout as it did.

// workArgTools are the tools whose arguments are forgiven: both take the
// task (message_id) and the caller's reply (result_id).
var workArgTools = map[string]bool{"claim_work": true, "submit_work": true}

// workReadTools take only the task (message_id): its aliases are moved, and
// nothing else is changed or refused here (acquisitiony 11-11: read_work with
// work_id cost a round trip).
var workReadTools = map[string]bool{"read_work": true}

// workArgAliases are names agents send for message_id and result_id. Only a
// value that reads as an ID is moved (workArgID); any other stays where it
// was sent and is refused with the corrected call.
var workArgAliases = map[string]string{
	"work_id": "message_id", "workId": "message_id", "workID": "message_id", "id": "message_id", "task_id": "message_id", "taskId": "message_id",
	"task": "message_id", "work": "message_id", "messageId": "message_id", "message": "message_id", "request_message_id": "message_id",
	"result": "result_id", "resultId": "result_id", "result_message_id": "result_id", "reply_id": "result_id", "replyId": "result_id",
	"reply": "result_id", "reply_message_id": "result_id", "answer_id": "result_id", "submission_id": "result_id", "target": "result_id",
}

// workArgsDerived are fields the tool fills in itself, from the work it reads
// as the caller: dropped, never refused.
var workArgsDerived = map[string]bool{"generation": true, "service_generation": true, "fence": true, "schema": true, "room": true}

// workArgText are names a result's text is sent under: it goes in
// post_message first, so the refusal says so.
var workArgText = map[string]bool{"text": true, "result_text": true, "content": true, "body": true, "answer": true, "output": true}

// workIDInText is a 32-hex ID inside a URL or a prefixed form
// (https://swarmmemo.com/work/ID, /m/ID, work:ID).
var workIDInText = regexp.MustCompile(`(?:^|[/:#=])([0-9a-f]{32})(?:[/?#.]|$)`)

// workArgPrefix is a unique ID prefix, which claim_work resolves as the
// read tools do (mcpMessageID).
var workArgPrefix = regexp.MustCompile(`^[0-9a-f]{8,31}$`)

// workArgID is v as the ID it names, lowercased and out of a URL, and
// whether it reads as one (a full ID or an 8+ hex prefix).
func workArgID(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.ToLower(strings.TrimSpace(s))
	if workIDRE.MatchString(s) || workArgPrefix.MatchString(s) {
		return s, true
	}
	if m := workIDInText.FindStringSubmatch(s); m != nil {
		return m[1], true
	}
	return "", false
}

var workIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// normalizeWorkArgs is args with every fix of one meaning made: aliases moved
// to their field, IDs out of URLs, derived fields dropped, a numeric ttl
// string made a number. What it cannot fix it leaves for the refusal.
func normalizeWorkArgs(tool string, args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	for _, field := range []string{"message_id", "result_id"} {
		if id, ok := workArgID(out[field]); ok {
			out[field] = id
		} else if s, isString := out[field].(string); isString && strings.TrimSpace(s) == "" {
			delete(out, field)
		}
	}
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	slices.Sort(names) // one outcome whatever the map order
	for _, k := range names {
		v := args[k]
		switch {
		case workArgsDerived[k]:
			delete(out, k)
		case workArgAliases[k] != "":
			field := workArgAliases[k]
			id, ok := workArgID(v)
			if v == nil {
				delete(out, k)
				continue
			}
			if !ok {
				continue
			}
			if have, set := out[field]; set && have != id {
				continue // two different IDs: the refusal names the call
			}
			out[field] = id
			delete(out, k)
		}
	}
	if tool == "claim_work" {
		if s, ok := out["ttl"].(string); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
				out["ttl"] = n
			}
		}
	}
	return out
}

// normalizeWorkReadArgs is args with the task's ID under message_id: a name
// that means it (workArgAliases) is moved when its value reads as an ID and no
// other ID is already there, and an ID in a URL is taken out of it.
func normalizeWorkReadArgs(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	if id, ok := workArgID(out["message_id"]); ok {
		out["message_id"] = id
	}
	names := make([]string, 0, len(args))
	for k := range args {
		if workArgAliases[k] == "message_id" {
			names = append(names, k)
		}
	}
	slices.Sort(names) // one outcome whatever the map order
	for _, k := range names {
		id, ok := workArgID(args[k])
		if !ok {
			continue
		}
		if have, set := out["message_id"]; set && have != id {
			continue
		}
		out["message_id"] = id
		delete(out, k)
	}
	return out
}

// forgivingWorkArgs is the middleware: claim_work and submit_work arguments
// normalized before validation, and an SDK refusal of what is left given as
// the corrected call (workCallRefusal), the SDK's words kept at the end so
// the tool counters still tell the kind of refusal (mcpArgRefusal).
func forgivingWorkArgs() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if !ok || call.Params == nil || !(workArgTools[call.Params.Name] || workReadTools[call.Params.Name]) {
				return next(ctx, method, req)
			}
			var args map[string]any
			if json.Unmarshal(call.Params.Arguments, &args) != nil || args == nil {
				return next(ctx, method, req)
			}
			if workReadTools[call.Params.Name] {
				raw, err := json.Marshal(normalizeWorkReadArgs(args))
				if err != nil {
					return next(ctx, method, req)
				}
				params := *call.Params
				params.Arguments = raw
				return next(ctx, method, &mcp.CallToolRequest{Session: call.Session, Params: &params, Extra: call.Extra})
			}
			fixed := normalizeWorkArgs(call.Params.Name, args)
			raw, err := json.Marshal(fixed)
			if err != nil {
				return next(ctx, method, req)
			}
			params := *call.Params
			params.Arguments = raw
			res, err := next(ctx, method, &mcp.CallToolRequest{Session: call.Session, Params: &params, Extra: call.Extra})
			r, isTool := res.(*mcp.CallToolResult)
			if err != nil || !isTool || !r.IsError || r.StructuredContent != nil {
				return res, err
			}
			var be *board.Error
			if errors.As(r.GetError(), &be) {
				return res, err
			}
			out := &mcp.CallToolResult{}
			out.SetError(toolError(workCallRefusal(call.Params.Name, fixed, fmt.Sprint(r.GetError()))))
			return out, nil
		}
	}
}

// workCallExample is the one call to send, with the IDs the caller gave.
func workCallExample(tool, messageID, resultID string) string {
	args, _ := json.Marshal(map[string]string{"message_id": cmp.Or(messageID, "MESSAGE_ID"), "result_id": cmp.Or(resultID, "RESULT_ID")})
	return tool + " " + string(args)
}

// workPostExample is the reply that makes a result.
func workPostExample(messageID string) string {
	args, _ := json.Marshal(map[string]string{"reply_to": cmp.Or(messageID, "MESSAGE_ID"), "text": "YOUR RESULT"})
	return "post_message " + string(args)
}

// workTaken is what each work tool takes besides message_id and result_id.
var workTaken = map[string][]string{
	"claim_work":  {"message_id", "result_id", "ttl", "result_sha256", "payout_address"},
	"submit_work": {"message_id", "result_id", "result_sha256", "payout_address"},
}

// workCallRefusal is invalid_request for arguments the tool could not take,
// stating the call to send: tool and argument object, with the IDs given.
func workCallRefusal(tool string, args map[string]any, sdk string) *board.Error {
	messageID, _ := args["message_id"].(string)
	resultID, _ := args["result_id"].(string)
	var unknown []string
	text := false
	for k := range args {
		if slices.Contains(workTaken[tool], k) {
			continue
		}
		if workArgText[k] || (workArgAliases[k] == "result_id") {
			text = true
		}
		if services.EchoesArg(k) {
			unknown = append(unknown, k)
		}
	}
	slices.Sort(unknown)
	msg := tool + " takes message_id, the task's id as find_work lists it, and result_id, your reply's id: " + workPostExample(messageID) + " answers with it as receipt.id."
	if text {
		msg += " Your result's text goes in that reply, not in " + tool + "."
	}
	msg += " Send: " + workCallExample(tool, messageID, resultID) + "."
	if len(unknown) > 0 {
		msg += " It does not take " + strings.Join(unknown, ", ") + "."
	}
	return &board.Error{Status: 400, Code: "invalid_request", Message: msg + " (" + sdk + ")"}
}

// withWorkCall is err with the corrected call appended, for the refusals a
// claim's shape causes: the task not found under that id, an id that is not
// one, a result that is not a reply to the task. Others pass unchanged.
func withWorkCall(err error, tool, messageID, resultID string) error {
	var be *board.Error
	if !errors.As(err, &be) {
		return err
	}
	var add string
	switch be.Code {
	case "not_found", "invalid_message_id":
		add = " message_id is the task's id as find_work lists it (works[].id); result_id is your reply's. Send: " + workCallExample(tool, "", resultID) + "."
	case "invalid_work_result":
		add = " Post your result as a reply to the task: " + workPostExample(messageID) + ", then " + workCallExample(tool, messageID, "") + " with its receipt.id as result_id."
	default:
		return err
	}
	e := *be
	e.Message += add
	return &e
}

// claimWork is claim_work's call made right: the task's id from a prefix or
// from the result it was mistaken for, the result submitted when the caller
// already holds the claim, and a claim window that fits the deadline.
func claimWork(hc *hostedCaller, in claimWorkInput) (board.Result, error) {
	messageID, resultID := in.MessageID, in.ResultID
	// A short ID, as agents quote one, resolved as the read tools resolve it.
	for _, id := range []*string{&messageID, &resultID} {
		if *id != "" {
			full, err := hc.s.mcpMessageID(hc.ctx, *id)
			if err != nil {
				return board.Result{}, withWorkCall(err, "claim_work", "", "")
			}
			*id = full
		}
	}
	if messageID == "" && resultID == "" {
		return board.Result{}, &board.Error{Status: 400, Code: "invalid_request", Message: "claim_work needs the task: " + workPostExample("") + " first, then " + workCallExample("claim_work", "", "") + "; find_work lists tasks."}
	}
	// Only a result: its reply_to is the task.
	if messageID == "" {
		parent, err := replyParent(hc, resultID)
		if err != nil || parent == "" {
			return board.Result{}, &board.Error{Status: 400, Code: "invalid_request", Message: "message_id is missing, and result_id is not a reply to a task. Send: " + workCallExample("claim_work", "", resultID) + "."}
		}
		messageID = parent
	}
	w, err := hostedWork(hc, messageID)
	if isNotFound(err) {
		// The caller's reply, sent as the task: its reply_to is the task.
		if parent, e := replyParent(hc, messageID); e == nil && parent != "" && (resultID == "" || resultID == parent || resultID == messageID) {
			if pw, pe := hostedWork(hc, parent); pe == nil {
				messageID, resultID, w, err = pw.ID, messageID, pw, nil
			}
		}
	}
	if err != nil {
		return board.Result{}, withWorkCall(err, "claim_work", "", resultID)
	}
	if resultID != "" && !workIDRE.MatchString(resultID) {
		return board.Result{}, &board.Error{Status: 400, Code: "invalid_request", Message: "result_id is your reply's id, 32 hex characters (post_message's receipt.id). Send: " + workCallExample("claim_work", messageID, "") + "."}
	}
	// Already claimed by this caller, without a result: the result submits it.
	if resultID != "" && w.State == "claimed" {
		res, err := workTransition(hc, board.Command{Operation: "work.submit", MessageID: messageID, Target: resultID}, true, in.Hash, nil, "payout_address", in.Payout)
		var be *board.Error
		if errors.As(err, &be) && be.Code == "work_forbidden" {
			return board.Result{}, &board.Error{Status: 409, Code: "work_state_conflict", Message: claimStateText(w, resultID)}
		}
		return res, withWorkCall(err, "claim_work", messageID, resultID)
	}
	c := board.Command{Operation: "work.claim", MessageID: messageID, Target: resultID, TTL: in.TTL}
	if c.Target == "" && c.TTL == 0 {
		// The longest window, cut to the deadline.
		c.TTL = board.WorkClaimTTLMax
		if left := w.Deadline - time.Now().Unix() - 5; left < c.TTL && left >= board.WorkClaimTTLMin {
			c.TTL = left
		}
	}
	res, err := workTransition(hc, c, false, in.Hash, nil, "payout_address", in.Payout)
	var be *board.Error
	if errors.As(err, &be) && be.Code == "work_state_conflict" && w.State != "open" {
		return res, &board.Error{Status: be.Status, Code: be.Code, Message: claimStateText(w, resultID)}
	}
	return res, withWorkCall(err, "claim_work", messageID, resultID)
}

// claimStateText says why a task that is not open cannot be claimed now.
func claimStateText(w board.Work, resultID string) string {
	switch {
	case w.State == "submitted" && resultID != "" && w.ResultID == resultID:
		return "Your result " + resultID + " is already submitted to this task: nothing more to send. The requester accepts or rejects it; read_work shows the verdict."
	case w.State == "submitted":
		return "A result waits for a verdict on this task, so it cannot be claimed now; find_work lists open tasks."
	case w.State == "claimed":
		return "Another worker holds this task's claim until " + time.Unix(w.ClaimExpiresAt, 0).UTC().Format(time.RFC3339) + "; find_work lists open tasks."
	}
	return "This task is " + w.State + ", not open, so it cannot be claimed; find_work lists open tasks."
}

// replyParent is the post id replies to, or "" for a root post.
func replyParent(hc *hostedCaller, id string) (string, error) {
	if !workIDRE.MatchString(id) {
		return "", nil
	}
	res, err := hc.exec(board.Command{Operation: "message.get", MessageID: id})
	if err != nil || len(res.Messages) == 0 {
		return "", err
	}
	return res.Messages[0].ReplyTo, nil
}

func isNotFound(err error) bool {
	var be *board.Error
	return errors.As(err, &be) && be.Code == "not_found"
}
