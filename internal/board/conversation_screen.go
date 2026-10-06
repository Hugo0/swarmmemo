package board

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"swarmmemo/internal/services"
)

// Conversation screening (RFC0013 §5.2): each message into a non-sealed
// conversation with a server-protected member is screened once, after
// commit, by a worker; reads filter by message_screens and never touch the
// network.
//
// The flow never holds a transaction across the classifier: a short read
// decides whether a message needs screening and who pays, the classifier
// runs with nothing held, and a second short transaction stores the scores.
// Every protected reader shares them and applies its own threshold,
// categories and fail rule at delivery (screenForDelivery).

const (
	// convScreenWorkers screen queued messages; convScreenQueue bounds the
	// queue, past which a message waits for a reader's ScreenBacklog.
	convScreenWorkers = 2
	convScreenQueue   = 1024
	// ConvScreenBacklogMax bounds what one ScreenBacklog screens, and
	// convScreenBacklogSlots the catch-up screens running at once, store-wide.
	ConvScreenBacklogMax   = 8
	convScreenBacklogSlots = 8
	// convScreenWindow is how far back in a room ScreenBacklog looks.
	convScreenWindow = 100
	// convScreenRetry is how long an unscreened message waits before a
	// reader's ScreenBacklog tries it again.
	convScreenRetry = 600
	// convScreenTimeout bounds one classifier call, as a screen.text call is;
	// convScreenBudget is how long a read waits for its catch-up.
	convScreenTimeout = 90 * time.Second
	convScreenBudget  = 5 * time.Second
)

// payerSwarmMemo pays for conversation screening, within moderation's
// conversation sub-cap (Decision 6: free protection in the growth stage).
// screenPayer is the seam for "reader pays" later: it would return the
// first protected reader's account, and screenText would run the services
// engine's screen.text under that account (its hold, refund and journal);
// message_screens.payer already records who paid.
const payerSwarmMemo = "swarmmemo"

func screenPayer() string { return payerSwarmMemo }

// conversationScreener is the classifier behind conversation screening:
// moderation's Jev within the conversation sub-cap (Engine.ScreenConversation).
// Any error means the text was not screened.
type conversationScreener interface {
	ScreenConversation(ctx context.Context, text string) (services.TextScreen, error)
}

// convScreenState is the Store's screening worker: a bounded queue of event
// IDs, the IDs being screened (so the worker and a catch-up never screen one
// message twice), and the catch-up slots.
type convScreenState struct {
	mu       sync.Mutex
	queue    chan string
	inflight map[string]chan struct{}
	slots    chan struct{}
	base     context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	// screener is the classifier; nil is moderation's engine.
	screener conversationScreener
}

func (s *Store) startConversationScreen(ctx context.Context) {
	c := &s.convScreen
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queue != nil {
		return
	}
	if c.screener == nil {
		c.screener = moderationScreener{s}
	}
	c.queue, c.inflight, c.slots = make(chan string, convScreenQueue), map[string]chan struct{}{}, make(chan struct{}, convScreenBacklogSlots)
	c.base, c.cancel = context.WithCancel(ctx)
	for range convScreenWorkers {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			for {
				select {
				case <-c.base.Done():
					return
				case id := <-c.queue:
					done, mine := c.claim(id)
					if !mine {
						continue
					}
					_, err := survive(func() (bool, error) { return true, s.screenMessage(c.base, id, false) })
					c.release(id, done)
					if err != nil && c.base.Err() == nil {
						slog.Warn("conversation screening: a message was not screened", "error", err)
					}
				}
			}
		}()
	}
}

func (s *Store) stopConversationScreen() {
	c := &s.convScreen
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// claim marks id in flight. It returns the channel closed when that screen
// ends, and whether the caller now holds it.
func (c *convScreenState) claim(id string) (chan struct{}, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if done, ok := c.inflight[id]; ok {
		return done, false
	}
	done := make(chan struct{})
	c.inflight[id] = done
	return done, true
}

func (c *convScreenState) release(id string, done chan struct{}) {
	c.mu.Lock()
	delete(c.inflight, id)
	c.mu.Unlock()
	close(done)
}

// enqueue hands id to the workers without blocking; a full queue, or a
// store whose worker is not running, leaves it to a reader's ScreenBacklog.
func (c *convScreenState) enqueue(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.queue <- id:
	default:
	}
}

// queueConversationScreen hands a fresh post into a conversation to the
// worker, after its transaction committed; the worker decides whether any
// reader needs it screened. It never fails the post.
func (s *Store) queueConversationScreen(c Command, res Result) {
	if c.Operation == "post" && res.Receipt != nil && !res.Receipt.Duplicate && IsConversationRoom(c.Room) {
		s.convScreen.enqueue(res.Receipt.ID)
	}
}

// messageScreen is one message_screens row.
type messageScreen struct {
	state  string // "pass", "flag" or "unscreened"
	scores map[string]float64
	model  string
	payer  string
	cost   int64
}

// screenMessage screens one conversation message once. needed says a
// protected reader already asked (ScreenBacklog); otherwise the message is
// screened only if another member reads in server mode.
func (s *Store) screenMessage(ctx context.Context, id string, needed bool) error {
	text, payer, ok, err := s.screenCandidate(ctx, id, needed)
	if err != nil || !ok {
		return err
	}
	return s.storeScreen(ctx, id, s.screenText(ctx, text, payer))
}

// screenCandidate is the short read before a screen: the message's text
// when it is in a non-sealed conversation, visible, not screened yet (or
// unscreened long enough ago to try again) and wanted by a reader, and who
// pays.
func (s *Store) screenCandidate(ctx context.Context, id string, needed bool) (text, payer string, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", "", false, err
	}
	defer tx.Rollback()
	var room, author, state string
	var screenedAt int64
	err = tx.QueryRowContext(ctx, `SELECT e.room,e.account,e.text,coalesce(m.state,''),coalesce(m.screened_at,0)
 FROM events e JOIN conversations c ON c.room=e.room LEFT JOIN message_screens m ON m.event_id=e.id
 WHERE e.id=? AND c.sealed=0 AND e.hidden=0`, id).Scan(&room, &author, &text, &state, &screenedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil || state == "pass" || state == "flag" || state == "unscreened" && screenedAt > s.now().Unix()-convScreenRetry {
		return "", "", false, err
	}
	if !needed {
		if needed, err = s.protectedMember(ctx, tx, room, author); err != nil || !needed {
			return "", "", false, err
		}
	}
	return text, screenPayer(), true, nil
}

// protectedMember says whether a member of room other than author, active
// or requested (requests are screened too), reads in server mode. Bounded
// by the room's member cap.
func (s *Store) protectedMember(ctx context.Context, tx *sql.Tx, room, author string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.account,`+hostedSQL+` FROM conversation_members m
 WHERE m.room=? AND m.account<>? AND m.state IN ('active','requested') LIMIT ?`, room, author, RoomMembersMax+1)
	if err != nil {
		return false, err
	}
	type member struct {
		account string
		hosted  bool
	}
	var members []member
	for rows.Next() {
		var m member
		if err = rows.Scan(&m.account, &m.hosted); err != nil {
			rows.Close()
			return false, err
		}
		members = append(members, m)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	for _, m := range members {
		p, err := readerProtection(ctx, tx, m.account, m.hosted)
		if err != nil || p.Mode == "server" {
			return err == nil, err
		}
	}
	return false, nil
}

// hostedSQL says, for the row's m.account, whether its current key is one
// SwarmMemo holds (loadProtection's defaults depend on it).
const hostedSQL = "EXISTS(SELECT 1 FROM identities i WHERE i.account=m.account AND i.successor='' AND i.custody='hosted')"

// readerProtection is account's inbound protection, as delivery applies it.
func readerProtection(ctx context.Context, tx *sql.Tx, account string, hosted bool) (InboundProtection, error) {
	p, err := loadProtection(ctx, tx, account, hosted)
	return deliveryInbound(p.Inbound), err
}

// screenText asks the classifier, holding no transaction. Anything but a
// complete, well-formed answer is unscreened: the text was not screened.
func (s *Store) screenText(ctx context.Context, text, payer string) messageScreen {
	out := messageScreen{state: "unscreened", payer: payer}
	c := s.convScreen.screener
	if c == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, convScreenTimeout)
	defer cancel()
	res, err := c.ScreenConversation(ctx, text)
	if err != nil || res.Model == "" || len(res.Model) > 64 {
		return out
	}
	scores := make(map[string]float64, len(services.ScreenCategories))
	for _, k := range services.ScreenCategories {
		v, ok := res.Scores[k]
		if !ok || !(v >= 0 && v <= 1) {
			return out
		}
		scores[k] = math.Round(v*1e4) / 1e4
	}
	out.state, out.scores, out.model, out.cost = "pass", scores, res.Model, res.CostMicroUSD
	if flagged(scores, services.ScreenCategories, services.ScreenThreshold) != nil {
		out.state = "flag"
	}
	return out
}

// storeScreen writes a screen in its own short transaction. A scored
// message keeps its first scores; an unscreened one takes a later screen.
func (s *Store) storeScreen(ctx context.Context, id string, m messageScreen) error {
	scores, err := json.Marshal(m.scores)
	if err != nil {
		return err
	}
	if m.scores == nil {
		scores = []byte("{}")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_screens(event_id,state,scores,model,payer,cost,screened_at) VALUES(?,?,?,?,?,?,?)
 ON CONFLICT(event_id) DO UPDATE SET state=excluded.state,scores=excluded.scores,model=excluded.model,payer=excluded.payer,cost=excluded.cost,screened_at=excluded.screened_at
 WHERE message_screens.state='unscreened'`, id, m.state, string(scores), m.model, m.payer, m.cost, s.now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// ScreenBacklog screens at most max unscreened messages of room for
// account before a server-protected read, within budget. The caller holds
// no transaction. It does nothing unless account is an active or requested
// member of a non-sealed conversation who reads in server mode, so no one
// can spend screening on a room they cannot read. Screens still running
// when budget ends finish in the background; the read runs either way,
// with those messages pending.
func (s *Store) ScreenBacklog(ctx context.Context, account, room string, max int, budget time.Duration) error {
	if max <= 0 || !IsConversationRoom(room) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ids, err := s.backlog(ctx, account, room, min(max, ConvScreenBacklogMax))
	if err != nil {
		return err
	}
	c := &s.convScreen
	var waits []chan struct{}
	for _, id := range ids {
		if done := c.startCatchUp(id, func(ctx context.Context) error { return s.screenMessage(ctx, id, true) }); done != nil {
			waits = append(waits, done)
		}
	}
	for _, done := range waits {
		select {
		case <-done:
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// preflightScreen runs a signed reader's catch-up before conversation.get
// opens its transaction, so every wire gets it: at most
// ConvScreenBacklogMax messages, within convScreenBudget. It never fails
// the read.
func (s *Store) preflightScreen(ctx context.Context, a actor, c Command) {
	if c.Operation != "conversation.get" || !a.signed || c.Delegation != nil || !IsConversationRoom(c.Room) {
		return
	}
	var account string
	if err := s.db.QueryRowContext(ctx, "SELECT account FROM identities WHERE id=?", a.id).Scan(&account); err == nil {
		_ = s.ScreenBacklog(ctx, account, c.Room, ConvScreenBacklogMax, convScreenBudget)
	}
}

// backlog is the short read behind ScreenBacklog: the newest messages of
// room, among its last convScreenWindow, that account needs screened.
func (s *Store) backlog(ctx context.Context, account, room string, max int) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var sealed, hosted bool
	var state string
	err = tx.QueryRowContext(ctx, "SELECT c.sealed,m.state,"+hostedSQL+" FROM conversations c JOIN conversation_members m ON m.room=c.room WHERE c.room=? AND m.account=?", room, account).Scan(&sealed, &state, &hosted)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (sealed || state != "active" && state != "requested") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p, err := readerProtection(ctx, tx, account, hosted); err != nil || p.Mode != "server" {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id FROM (SELECT id,account,hidden,seq FROM events WHERE room=? ORDER BY seq DESC LIMIT ?) e
 LEFT JOIN message_screens m ON m.event_id=e.id
 WHERE e.account<>? AND e.hidden=0 AND (m.event_id IS NULL OR m.state='unscreened' AND m.screened_at<=?) ORDER BY e.seq DESC LIMIT ?`,
		room, convScreenWindow, account, s.now().Unix()-convScreenRetry, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// startCatchUp runs screen for id in the background, on the worker's
// context and within a catch-up slot. It returns the channel closed when
// id's screen ends (another's, if id was already being screened), or nil
// when nothing will: the worker is not running or every slot is busy.
func (c *convScreenState) startCatchUp(id string, screen func(context.Context) error) chan struct{} {
	c.mu.Lock()
	base, slots := c.base, c.slots
	c.mu.Unlock()
	if base == nil || base.Err() != nil {
		return nil
	}
	done, mine := c.claim(id)
	if !mine {
		return done
	}
	select {
	case slots <- struct{}{}:
	default:
		c.release(id, done)
		return nil
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer func() { <-slots }()
		defer c.release(id, done)
		if _, err := survive(func() (bool, error) { return true, screen(base) }); err != nil && base.Err() == nil {
			slog.Warn("conversation screening: a message was not screened", "error", err)
		}
	}()
	return done
}

// deliveryInbound is in as delivery applies it: mode server or client, a
// threshold in (0, 1] (0.6 when unset), the categories among
// services.ScreenCategories (all when none), and fail closed unless it is
// explicitly open.
func deliveryInbound(in InboundProtection) InboundProtection {
	out := InboundProtection{Mode: "client", Threshold: services.ScreenThreshold, Fail: "closed"}
	if in.Mode == "server" {
		out.Mode = "server"
	}
	if in.Threshold > 0 && in.Threshold <= 1 {
		out.Threshold = in.Threshold
	}
	for _, k := range in.Categories {
		if slices.Contains(services.ScreenCategories, k) && !slices.Contains(out.Categories, k) {
			out.Categories = append(out.Categories, k)
		}
	}
	if len(out.Categories) == 0 {
		out.Categories = slices.Clone(services.ScreenCategories)
	}
	if in.Fail == "open" {
		out.Fail = "open"
	}
	return out
}

// flagged are the categories among cats whose score is at or above
// threshold, in cats' order.
func flagged(scores map[string]float64, cats []string, threshold float64) []string {
	var out []string
	for _, k := range cats {
		if v, ok := scores[k]; ok && v >= threshold {
			out = append(out, k)
		}
	}
	return out
}

// screenForDelivery sets Screen on msgs for reader and withholds the text
// its settings hold back. It only reads message_screens in tx.
//
// Only messages of non-sealed conversations are touched. A reader in
// server mode gets each message's shared scores judged at its own
// threshold and categories: a flag is withheld, and a message not screened
// yet ("pending") or that could not be ("unscreened") is withheld when it
// fails closed and shown with its state when it fails open. A reader in
// client mode gets any stored scores for information, never withheld. A
// reader's own messages carry no screen at all: their state and scores
// would be a free oracle for tuning an injection against the classifier,
// and would say whether the other members read in server mode (§5.2).
// Withholding empties the text
// and everything that repeats or confirms it (the signed payload and its
// signature, the text's hash, attachments); the reader reveals it with
// conversation.get's data.reveal.
func (s *Store) screenForDelivery(ctx context.Context, tx *sql.Tx, reader actor, p Protection, msgs []Message) error {
	var ids []string
	for i := range msgs {
		if IsConversationRoom(msgs[i].Room) && !msgs[i].Sealed {
			ids = append(ids, msgs[i].ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	stored, err := loadScreens(ctx, tx, ids)
	if err != nil {
		return err
	}
	in := deliveryInbound(p.Inbound)
	for i := range msgs {
		m := &msgs[i]
		row, ok := stored[m.ID]
		if !ok || m.Sealed || row.account == reader.account {
			continue
		}
		screen := &MessageScreen{State: row.state, Categories: row.scores}
		if row.model != "" {
			screen.ClassifierVersion = services.ClassifierVersion
		}
		switch row.state {
		case "":
			screen.State, screen.Reason = "pending", "not screened yet"
		case "unscreened":
			screen.Reason = "could not be screened"
		default:
			screen.State = "pass"
			if hits := flagged(row.scores, in.Categories, in.Threshold); hits != nil {
				screen.State, screen.Reason = "flag", "flagged: "+strings.Join(hits, ", ")
			}
		}
		switch {
		case in.Mode != "server" && row.state == "":
			continue // client mode: only scores the server already has
		case in.Mode == "server":
			screen.Withheld = screen.State == "flag" || screen.State != "pass" && in.Fail == "closed"
		}
		if screen.Withheld {
			// The text's SHA-256 and the signature over it go too: either
			// would confirm a guessed short text.
			m.Text, m.SignedPayload, m.Signature, m.Hash, m.Attachments = "", "", "", "", nil
		}
		m.Screen = screen
	}
	return nil
}

// storedScreen is a message's author account and its message_screens row;
// state is "" when it has none.
type storedScreen struct {
	account, state, model string
	scores                map[string]float64
}

// loadScreens reads the rows for ids, 100 at a time, in tx: only messages
// of non-sealed conversations have one.
func loadScreens(ctx context.Context, tx *sql.Tx, ids []string) (map[string]storedScreen, error) {
	out := make(map[string]storedScreen, len(ids))
	for chunk := range slices.Chunk(ids, 100) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := tx.QueryContext(ctx, `SELECT e.id,e.account,coalesce(m.state,''),coalesce(m.scores,'{}'),coalesce(m.model,'')
 FROM events e JOIN conversations c ON c.room=e.room AND c.sealed=0 LEFT JOIN message_screens m ON m.event_id=e.id
 WHERE e.id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, scores string
			var r storedScreen
			if err = rows.Scan(&id, &r.account, &r.state, &scores, &r.model); err == nil {
				err = json.Unmarshal([]byte(scores), &r.scores)
			}
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = r
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
