package scheduler

// Rooms: multi-agent group conversation over a single shared contextId. A room
// has two floor-control modes (RoomMode):
//
//   - relay = "多 Agent 协同" (multi-agent collaboration): @-mention driven
//     hub-and-spoke. Documented below; the shipped default.
//   - roundtable: the real round-table — consensus rounds (Texas Hold'em). Each
//     round is one full pass over the participants in a fresh random order; a
//     dedicated moderator agent judges consensus per round (under its own
//     contextId) and writes the final summary. See driveRoundtable.
//
// --- relay (multi-agent collaboration) ---
//
// A room hosts several registered agents in one conversation. Turn-taking is
// @-mention driven: any message — from the operator OR from an agent's reply —
// may address `@<name>`, which makes that agent the next speaker. An agent can
// thus hand a question to a peer (`@teacher what do you think?`), producing an
// organic question-and-answer chain. A message with no mention returns control
// to the operator (the room goes `waiting`).
//
// How agents "hear" each other (incremental relay): every room turn appends to
// a shared transcript. When it is an agent's turn, it is sent only the turns it
// has not seen since it last spoke, attributed per line. So each agent runs the
// LLM only on its own turn (bounded cost, no broadcast storm), while staying in
// sync with the discussion. This builds entirely on the existing shared-context
// + speaker-attribution machinery (executor tags each message [speaker: <name>])
// — no wrapper changes.
//
// Runaway guard: an autonomous agent→agent chain (two agents @-mentioning each
// other) is bounded by MaxTurns turns *between operator interventions*. Hitting
// the bound parks the room in `waiting` so a human decides whether to continue.
// Each operator message resets the budget.

import (
	"fmt"
	"log"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// RoomStatus is the lifecycle state of a room.
type RoomStatus string

const (
	// RoomActive: a speaker is scheduled (Next set) and the drive loop is or
	// will be running.
	RoomActive RoomStatus = "active"
	// RoomWaiting: no agent is scheduled — the last turn addressed no one, or
	// the autonomous-chain budget was hit. Awaiting an operator message.
	RoomWaiting RoomStatus = "waiting"
	// RoomStopped: explicitly halted by the operator. Terminal.
	RoomStopped RoomStatus = "stopped"
)

// defaultMaxChain bounds an autonomous agent→agent chain between operator
// messages, so two agents @-mentioning each other can't loop forever.
const defaultMaxChain = 8

// RoomMode selects the floor-control policy.
//   - relay (default): @-pointer / organizer picks the next speaker. This is
//     "多 Agent 协同" (multi-agent collaboration) — the shipped hub-and-spoke flow.
//   - roundtable: the real round-table — consensus rounds (Texas Hold'em). Each
//     round is one full pass over the participants in a fresh random order; a
//     moderator agent judges consensus per round and writes the final summary.
type RoomMode string

const (
	RoomModeRelay      RoomMode = "relay"
	RoomModeRoundtable RoomMode = "roundtable"
)

// defaultRoundtableBudget bounds how many consensus rounds may run without
// agreement before the room parks for the operator (never force agreement).
const defaultRoundtableBudget = 12

// maxTopicRunes bounds a room topic — it's a header label, so an overlong one
// would wrap and break the layout. Measured in runes so multibyte (CJK) topics
// are counted by character, not byte.
const maxTopicRunes = 60

// truncateTopic caps topic at maxTopicRunes runes, appending an ellipsis when
// it had to cut. Rune-aware so it never splits a multibyte character.
func truncateTopic(topic string) string {
	r := []rune(topic)
	if len(r) <= maxTopicRunes {
		return topic
	}
	return string(r[:maxTopicRunes]) + "…"
}

// mentionRe matches an @-mention token. Names are matched case-insensitively
// against the room's participant list.
var mentionRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9_-]*)`)

// codeSpanRe matches fenced code blocks (```...```) and inline code spans
// (`...`). Mentions inside these are QUOTED tokens (an agent explaining the
// `@name` syntax), not addressing directives, so they are stripped before
// mention parsing.
var codeSpanRe = regexp.MustCompile("(?s)```.*?```|`[^`]*`")

// RoomTurn is one entry in a room's merged transcript.
type RoomTurn struct {
	Speaker  string    `json:"speaker"` // agent name, or "operator"
	Text     string    `json:"text"`
	Mentions []string  `json:"mentions,omitempty"`
	Round    int       `json:"round,omitempty"` // roundtable mode: 1-based round this turn belongs to
	TS       time.Time `json:"ts"`
	Error    string    `json:"error,omitempty"`
}

// Room is one group conversation. Its ID doubles as the shared A2A contextId,
// so every participant's session and transcript key on it.
type Room struct {
	ID           string
	Topic        string
	Participants []string
	// Organizer moderates the room: "operator" (the human) or a participant
	// agent's name. When a turn addresses no one, control returns here.
	Organizer string
	MaxChain  int

	// Roundtable mode (RoomModeRoundtable): a dedicated Moderator agent judges
	// per-round consensus and writes the summary; Budget caps rounds.
	Mode      RoomMode
	Moderator string
	Budget    int

	mu         sync.Mutex
	transcript []RoomTurn
	status     RoomStatus
	next       string         // relay: next speaker, "" => waiting for operator
	speaking   string         // agent whose turn is in flight ("" when idle)
	lastSeen   map[string]int // participant -> count of transcript entries consumed
	chain      int            // relay: autonomous turns since the last operator message
	running    bool           // a drive goroutine is active
	createdAt  time.Time

	// Roundtable round state (guarded by mu):
	order      []string // current round's speaking order (re-shuffled each round)
	cursor     int      // index into order of the next participant to speak
	round      int      // completed-rounds counter (vs Budget)
	roundStart int      // transcript index where the current round began
}

// RoomView is the JSON snapshot served to clients.
type RoomView struct {
	ID           string     `json:"id"`
	Topic        string     `json:"topic"`
	Participants []string   `json:"participants"`
	Organizer    string     `json:"organizer"`
	Mode         RoomMode   `json:"mode,omitempty"`
	Moderator    string     `json:"moderator,omitempty"`
	Status       RoomStatus `json:"status"`
	Next         string     `json:"next,omitempty"`
	Speaking     string     `json:"speaking,omitempty"` // agent with a turn in flight
	MaxChain     int        `json:"maxChain"`
	Transcript   []RoomTurn `json:"transcript"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// turnFunc executes one agent turn: deliver `message` (attributed to `speaker`)
// to `agent` under the room's contextId, returning the agent's reply. Injected
// so the manager is testable without real agents; production wires it to
// Scheduler.ChatWithAgentAs.
type turnFunc func(agent, contextID, speaker, message string) (string, error)

// RoomManager owns all rooms and the drive loop.
type RoomManager struct {
	mu    sync.Mutex
	rooms map[string]*Room

	run     turnFunc
	isAgent func(name string) bool // participant must be a registered agent
	store   *RoomStore             // nil-safe; persists rooms when configured
	shuffle func([]string)         // randomizes a roundtable round's order; injectable for tests
}

// NewRoomManager builds a manager. run executes agent turns; isAgent validates
// that a participant name is a registered agent.
func NewRoomManager(run turnFunc, isAgent func(string) bool) *RoomManager {
	return &RoomManager{
		rooms:   make(map[string]*Room),
		run:     run,
		isAgent: isAgent,
		shuffle: func(s []string) {
			rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		},
	}
}

// SetStore enables room persistence and seeds the manager with any rooms the
// store already holds (restored at scheduler startup). Called once before the
// manager serves traffic; a nil store leaves persistence off.
func (m *RoomManager) SetStore(store *RoomStore, restored map[string]*Room) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store = store
	for id, room := range restored {
		m.rooms[id] = room
	}
}

// persistTurn appends a turn to the room's log, best-effort: a persistence
// failure is logged but never breaks the discussion. No-op when store is nil.
func (m *RoomManager) persistTurn(roomID string, turn RoomTurn) {
	if err := m.store.AppendTurn(roomID, turn); err != nil {
		log.Printf("roundtable: persist turn room=%s: %v", roomID, err)
	}
}

// CreateRoom validates participants and registers a new room. An optional
// opening message (which may contain an @-mention to address the first speaker)
// is posted as the operator, kicking off the loop when it addresses someone.
func (m *RoomManager) CreateRoom(topic string, participants []string, organizer string, maxChain int, opening string) (*RoomView, error) {
	canonical, err := m.canonParticipants(participants)
	if err != nil {
		return nil, err
	}
	// Bound the topic so a long one can't break the header layout (it's a label,
	// not content). Truncate rather than reject — the room still works.
	topic = truncateTopic(strings.TrimSpace(topic))
	// Organizer is the human ("operator") or a participant agent; an unknown
	// agent name (not in participants) is rejected so a no-mention turn always
	// has a valid place to hand control back to.
	organizer = strings.TrimSpace(organizer)
	if organizer == "" {
		organizer = "operator"
	}
	if organizer != "operator" {
		ok := false
		for _, p := range canonical {
			if p == organizer {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("organizer %q must be a participant or \"operator\"", organizer)
		}
	}
	if maxChain <= 0 {
		maxChain = defaultMaxChain
	}

	now := time.Now()
	room := &Room{
		ID:           uuid.Must(uuid.NewV7()).String(),
		Topic:        topic,
		Participants: canonical,
		Organizer:    organizer,
		MaxChain:     maxChain,
		Mode:         RoomModeRelay,
		status:       RoomWaiting,
		lastSeen:     make(map[string]int),
		createdAt:    now,
	}
	m.mu.Lock()
	m.rooms[room.ID] = room
	m.mu.Unlock()

	// Persist the room's metadata as its log's first line before any turns.
	if err := m.store.WriteMeta(room); err != nil {
		log.Printf("roundtable: persist room meta room=%s: %v", room.ID, err)
	}

	if strings.TrimSpace(opening) != "" {
		if _, err := m.Say(room.ID, opening, "operator"); err != nil {
			return nil, err
		}
	}
	return m.snapshot(room.ID)
}

// canonParticipants trims, de-dupes (case-insensitively), and validates that
// every participant is a registered agent. Shared by both room constructors.
func (m *RoomManager) canonParticipants(participants []string) ([]string, error) {
	seen := map[string]bool{}
	canonical := make([]string, 0, len(participants))
	for _, p := range participants {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if m.isAgent != nil && !m.isAgent(p) {
			return nil, fmt.Errorf("participant %q is not a registered agent", p)
		}
		if !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			canonical = append(canonical, p)
		}
	}
	if len(canonical) == 0 {
		return nil, fmt.Errorf("a room needs at least one participant")
	}
	return canonical, nil
}

// CreateRoundtableRoom registers a room in roundtable (consensus-rounds) mode.
// `moderator` is a dedicated agent — NOT one of the participants — that judges
// per-round consensus and writes the final summary under its own contextId.
// `budget` caps rounds without consensus (0 -> default). An optional opening
// message (the operator's question) kicks off the first round.
func (m *RoomManager) CreateRoundtableRoom(topic string, participants []string, moderator string, budget int, opening string) (*RoomView, error) {
	canonical, err := m.canonParticipants(participants)
	if err != nil {
		return nil, err
	}
	moderator = strings.TrimSpace(moderator)
	if moderator == "" {
		return nil, fmt.Errorf("a roundtable needs a moderator agent")
	}
	if m.isAgent != nil && !m.isAgent(moderator) {
		return nil, fmt.Errorf("moderator %q is not a registered agent", moderator)
	}
	for _, p := range canonical {
		if strings.EqualFold(p, moderator) {
			return nil, fmt.Errorf("moderator %q must not also be a participant", moderator)
		}
	}
	if budget <= 0 {
		budget = defaultRoundtableBudget
	}
	topic = truncateTopic(strings.TrimSpace(topic))

	now := time.Now()
	room := &Room{
		ID:           uuid.Must(uuid.NewV7()).String(),
		Topic:        topic,
		Participants: canonical,
		Organizer:    "operator",
		Mode:         RoomModeRoundtable,
		Moderator:    moderator,
		Budget:       budget,
		status:       RoomWaiting,
		lastSeen:     make(map[string]int),
		createdAt:    now,
	}
	m.mu.Lock()
	m.rooms[room.ID] = room
	m.mu.Unlock()

	if err := m.store.WriteMeta(room); err != nil {
		log.Printf("roundtable: persist room meta room=%s: %v", room.ID, err)
	}

	if strings.TrimSpace(opening) != "" {
		if _, err := m.Say(room.ID, opening, "operator"); err != nil {
			return nil, err
		}
	}
	return m.snapshot(room.ID)
}

// Say posts a message into the room as `speaker` (empty -> "operator"). It is
// appended to the transcript, resets the autonomous-chain budget, and — when it
// @-mentions a participant — schedules that agent and starts the drive loop.
func (m *RoomManager) Say(roomID, text, speaker string) (*RoomView, error) {
	room := m.get(roomID)
	if room == nil {
		return nil, fmt.Errorf("room %q not found", roomID)
	}
	if speaker == "" {
		speaker = "operator"
	}

	room.mu.Lock()
	if room.status == RoomStopped {
		room.mu.Unlock()
		return nil, fmt.Errorf("room is stopped")
	}
	mentions := parseMentions(text, room.Participants, "")
	turn := RoomTurn{Speaker: speaker, Text: text, Mentions: mentions, TS: time.Now()}
	room.transcript = append(room.transcript, turn)

	// Roundtable: an operator message is the question that (re)opens a fresh
	// cycle — reset the round state and start a new round-robin pass.
	if room.Mode == RoomModeRoundtable {
		room.order = append([]string(nil), room.Participants...)
		m.shuffle(room.order)
		room.cursor = 0
		room.round = 0
		room.roundStart = len(room.transcript)
		room.status = RoomActive
		start := !room.running
		if start {
			room.running = true
		}
		room.mu.Unlock()
		m.persistTurn(roomID, turn)
		if start {
			go m.driveRoundtable(room)
		}
		return m.snapshot(roomID)
	}

	room.chain = 0 // relay: operator message resets the runaway budget
	room.scheduleNextLocked(speaker, mentions)
	start := false
	if room.next != "" && !room.running {
		room.running = true
		start = true
	}
	room.mu.Unlock()

	m.persistTurn(roomID, turn)

	if start {
		go m.drive(room)
	}
	return m.snapshot(roomID)
}

// Stop halts a room. Terminal; the underlying agent sessions are untouched.
func (m *RoomManager) Stop(roomID string) (*RoomView, error) {
	room := m.get(roomID)
	if room == nil {
		return nil, fmt.Errorf("room %q not found", roomID)
	}
	room.mu.Lock()
	room.status = RoomStopped
	room.next = ""
	room.mu.Unlock()
	if err := m.store.WriteStatus(roomID, RoomStopped); err != nil {
		log.Printf("roundtable: persist stop room=%s: %v", roomID, err)
	}
	return m.snapshot(roomID)
}

// drive runs scheduled agent turns until the room is no longer active (no next
// speaker, stopped, or the autonomous-chain budget is exhausted). The slow
// turnFunc call is made WITHOUT holding the room lock.
func (m *RoomManager) drive(room *Room) {
	for {
		room.mu.Lock()
		if room.status == RoomStopped || room.next == "" {
			room.running = false
			room.mu.Unlock()
			return
		}
		if room.chain >= room.MaxChain {
			// Park for the operator rather than loop forever.
			room.status = RoomWaiting
			room.next = ""
			room.running = false
			room.mu.Unlock()
			return
		}
		speaker := room.next
		room.next = ""           // consumed; the reply's mention (if any) sets the next one
		room.speaking = speaker  // mark in-flight so the UI can show "thinking"
		message := room.deltaLocked(speaker)
		addresser := "operator"
		if n := len(room.transcript); n > 0 {
			addresser = room.transcript[n-1].Speaker
		}
		contextID := room.ID
		room.mu.Unlock()

		reply, err := m.run(speaker, contextID, addresser, message)

		room.mu.Lock()
		room.speaking = "" // turn finished
		now := time.Now()
		if err != nil {
			// The turn failed (e.g. target agent unreachable mid-restart). Record
			// it but do NOT advance lastSeen[speaker]: on retry — by an operator
			// message or by post-restart compensation — the speaker must be re-fed
			// the messages it still owes a reply to.
			errTurn := RoomTurn{Speaker: speaker, Error: err.Error(), TS: now}
			room.transcript = append(room.transcript, errTurn)
			room.status = RoomWaiting
			room.running = false
			room.mu.Unlock()
			m.persistTurn(room.ID, errTurn)
			return
		}
		mentions := parseMentions(reply, room.Participants, speaker)
		replyTurn := RoomTurn{Speaker: speaker, Text: reply, Mentions: mentions, TS: now}
		room.transcript = append(room.transcript, replyTurn)
		room.lastSeen[speaker] = len(room.transcript)
		room.chain++
		room.scheduleNextLocked(speaker, mentions)
		room.mu.Unlock()
		m.persistTurn(room.ID, replyTurn)
	}
}

// driveRoundtable runs consensus rounds (Texas Hold'em). Each round is one full
// pass over room.order (re-shuffled per round); when the pass completes, the
// moderator judges consensus. Consensus -> the moderator writes a summary turn
// and the room parks. No consensus within Budget rounds -> park for the
// operator. The slow run() calls are made WITHOUT the room lock.
func (m *RoomManager) driveRoundtable(room *Room) {
	for {
		room.mu.Lock()
		if room.status == RoomStopped {
			room.running = false
			room.mu.Unlock()
			return
		}

		// Mid-round: the next participant in this round's order speaks.
		if room.cursor < len(room.order) {
			speaker := room.order[room.cursor]
			room.cursor++
			room.speaking = speaker
			message := room.deltaLocked(speaker)
			addresser := "operator"
			if n := len(room.transcript); n > 0 {
				addresser = room.transcript[n-1].Speaker
			}
			contextID := room.ID
			room.mu.Unlock()

			reply, err := m.run(speaker, contextID, addresser, message)

			room.mu.Lock()
			room.speaking = ""
			now := time.Now()
			if err != nil {
				errTurn := RoomTurn{Speaker: speaker, Error: err.Error(), TS: now}
				room.transcript = append(room.transcript, errTurn)
				room.status = RoomWaiting
				room.running = false
				room.mu.Unlock()
				m.persistTurn(room.ID, errTurn)
				return
			}
			replyTurn := RoomTurn{Speaker: speaker, Text: reply, Round: room.round + 1, TS: now}
			room.transcript = append(room.transcript, replyTurn)
			room.lastSeen[speaker] = len(room.transcript)
			room.mu.Unlock()
			m.persistTurn(room.ID, replyTurn)
			continue
		}

		// Round complete: the moderator consolidates — locks what's agreed,
		// lists what's still open, and signals convergence. The consolidation is
		// appended as a visible turn, so it anchors the NEXT round (participants
		// see it via the delta and are told to only push the open points).
		room.round++
		round := room.round
		roundStart := room.roundStart
		moderator := room.Moderator
		budget := room.Budget
		room.speaking = moderator
		room.mu.Unlock()

		text, consensus := m.roundtableConsolidate(room, roundStart, round, moderator)

		room.mu.Lock()
		room.speaking = ""
		if room.status == RoomStopped {
			room.running = false
			room.mu.Unlock()
			return
		}
		if text != "" {
			sumTurn := RoomTurn{Speaker: moderator, Text: text, Round: round, TS: time.Now()}
			room.transcript = append(room.transcript, sumTurn)
			room.mu.Unlock()
			m.persistTurn(room.ID, sumTurn)
			room.mu.Lock()
		}
		// Converged, or out of budget → park for the operator (the last
		// consolidation's 【已达成】 is the decision; 【待议】, if any, is what's left).
		if consensus || round >= budget {
			room.status = RoomWaiting
			room.running = false
			room.mu.Unlock()
			return
		}
		// Next round: fresh random order; the consolidation just appended is the
		// new anchor everyone sees.
		room.order = append([]string(nil), room.Participants...)
		m.shuffle(room.order)
		room.cursor = 0
		room.roundStart = len(room.transcript)
		room.mu.Unlock()
	}
}

// roundtableConsolidate runs the moderator's per-round consolidation: it reads
// this round's turns and emits an updated 小结 with two columns — 【已达成】
// (locked agreements, not to be relitigated) and 【待议】 (still open) — plus a
// CONSENSUS/CONTINUE verdict. It runs under a SEPARATE contextId (roomID+"#mod")
// so the moderator's session stays isolated, but its TEXT is returned to be
// appended as a visible turn that anchors the next round. Returns (text without
// the trailing verdict line, converged?).
func (m *RoomManager) roundtableConsolidate(room *Room, roundStart, round int, moderator string) (string, bool) {
	room.mu.Lock()
	var b strings.Builder
	if room.Topic != "" {
		fmt.Fprintf(&b, "议题：%s\n\n", room.Topic)
	}
	fmt.Fprintf(&b, "本轮（第 %d 轮）的发言：\n", round)
	for _, t := range room.transcript[roundStart:] {
		if t.Error != "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", t.Speaker, t.Text)
	}
	room.mu.Unlock()
	b.WriteString("\n请基于全部讨论，更新并输出本轮小结，分三栏：\n" +
		"【已达成】逐条列出已无争议、可锁定的结论。\n" +
		"【待议】逐条列出仍有分歧或未决的点。\n" +
		"【纠偏】如果某位参与者本轮的论述偏离了命题、过度发散、或钻进与决策无关的细节，" +
		"请点名（用 @他的名字）指出，并用一句话告诉他下一轮该如何收敛、回到命题；若无则写「无」。\n" +
		"规则：被各方接受的点放入【已达成】，下一轮不再重复讨论；" +
		"若本轮有人用新论据推翻了某条【已达成】所依赖的假设，把它移回【待议】并注明原因。\n" +
		"最后单独一行输出 CONSENSUS（【待议】已清空、全部达成一致）或 CONTINUE（仍有待议）。")

	reply, err := m.run(moderator, room.ID+"#mod", "system", b.String())
	if err != nil {
		return "", false // keep deliberating (bounded by Budget)
	}
	return stripVerdictLine(reply), parseConsensus(reply)
}

// parseConsensus reads the moderator's verdict token. CONTINUE wins if both
// appear; a reply with neither defaults to "not yet" (keep going).
func parseConsensus(reply string) bool {
	up := strings.ToUpper(reply)
	if strings.Contains(up, "CONTINUE") {
		return false
	}
	return strings.Contains(up, "CONSENSUS")
}

// stripVerdictLine drops a trailing bare CONSENSUS/CONTINUE line (and blank
// lines) from the consolidation so the visible turn shows only the 小结. If that
// would empty the text (e.g. a bare-token stub), the original is kept.
func stripVerdictLine(s string) string {
	lines := strings.Split(s, "\n")
	for len(lines) > 0 {
		last := strings.ToUpper(strings.TrimSpace(lines[len(lines)-1]))
		if last == "" || last == "CONSENSUS" || last == "CONTINUE" {
			lines = lines[:len(lines)-1]
			continue
		}
		break
	}
	out := strings.TrimSpace(strings.Join(lines, "\n"))
	if out == "" {
		return strings.TrimSpace(s)
	}
	return out
}

// scheduleNextLocked decides who speaks next after `speaker`'s turn produced
// `mentions`. A mention wins. Otherwise control returns to the organizer — but
// only if the organizer is an agent OTHER than the speaker (the self-organizer
// guard); else the room parks for the operator. A stopped room is never
// rescheduled. Caller holds room.mu.
func (room *Room) scheduleNextLocked(speaker string, mentions []string) {
	if room.status == RoomStopped {
		room.next = ""
		return
	}
	if len(mentions) > 0 {
		room.next = mentions[0]
		room.status = RoomActive
		return
	}
	if room.Organizer != "operator" && room.Organizer != speaker {
		room.next = room.Organizer
		room.status = RoomActive
		return
	}
	room.next = ""
	room.status = RoomWaiting
}

func (room *Room) isParticipant(name string) bool {
	for _, p := range room.Participants {
		if p == name {
			return true
		}
	}
	return false
}

// recoverScheduleLocked rebuilds turn-taking when a room is restored after a
// restart, so a turn interrupted mid-flight is retried (compensation) rather
// than left dangling. Caller holds room.mu (or the room is freshly loaded and
// not yet shared).
//
//   - lastSeen[P] is reset to each participant's last *successful* turn, so a
//     retried speaker is re-fed the messages it still owes a reply to (not an
//     empty "your turn").
//   - The next speaker is derived from the transcript tail: a trailing error
//     turn retries its speaker; otherwise the last good turn's @-mention drives
//     the next speaker, exactly as live scheduling would. This covers both a
//     recorded send failure and a restart so abrupt no error turn was written
//     (the last good turn still addresses someone who never answered).
func (room *Room) recoverScheduleLocked() {
	seen := map[string]int{}
	for i, t := range room.transcript {
		if t.Error == "" {
			seen[t.Speaker] = i + 1
		}
	}
	for _, p := range room.Participants {
		room.lastSeen[p] = seen[p] // 0 when the participant never spoke
	}
	room.speaking = ""
	room.running = false
	room.chain = 0 // a restart resets the runaway budget, like an operator turn

	// Roundtable rooms don't use mention/organizer scheduling. The transient
	// round state (order/cursor/round) isn't persisted, so a restored roundtable
	// parks for the operator; the next operator question re-opens a fresh cycle.
	if room.Mode == RoomModeRoundtable {
		room.order = nil
		room.cursor = 0
		room.round = 0
		if room.status != RoomStopped {
			room.status = RoomWaiting
		}
		room.next = ""
		return
	}

	if room.status == RoomStopped || len(room.transcript) == 0 {
		if room.status != RoomStopped {
			room.status = RoomWaiting
		}
		room.next = ""
		return
	}
	last := room.transcript[len(room.transcript)-1]
	if last.Error != "" {
		// Interrupted speaker never delivered — retry it. lastSeen already points
		// at its last success, so the retry re-feeds the unanswered messages.
		if room.isParticipant(last.Speaker) {
			room.next = last.Speaker
			room.status = RoomActive
		} else {
			room.next = ""
			room.status = RoomWaiting
		}
		return
	}
	// Last turn was a normal message — reschedule by its mentions, same as live.
	room.status = RoomWaiting
	room.scheduleNextLocked(last.Speaker, last.Mentions)
}

// deltaLocked builds the message handed to `speaker`: the transcript entries it
// has not yet consumed, attributed per line, plus a turn instruction. Caller
// holds room.mu.
func (room *Room) deltaLocked(speaker string) string {
	from := room.lastSeen[speaker]
	var b strings.Builder
	if room.Topic != "" {
		fmt.Fprintf(&b, "[圆桌 · 话题] %s\n\n", room.Topic)
	}
	if from < len(room.transcript) {
		b.WriteString("自你上次发言以来的新消息：\n")
		for _, t := range room.transcript[from:] {
			if t.Error != "" {
				continue
			}
			fmt.Fprintf(&b, "%s: %s\n", t.Speaker, t.Text)
		}
		b.WriteString("\n")
	}
	// Roundtable: the floor is round-robin (no @-handoff). Anchor each turn on
	// the moderator's latest 小结 so the discussion converges instead of sprawling:
	// only push the 待议 (open) points; don't relitigate 已达成 (locked) ones
	// unless a new argument breaks an assumption they rest on.
	if room.Mode == RoomModeRoundtable {
		fmt.Fprintf(&b, "轮到你（%s）发言。若上方有主持的最新小结，请只针对其中【待议】的点推进；"+
			"【已达成】的结论不要重复讨论，除非你有新论据推翻了它依赖的某个假设——那就明确指出是哪条、哪个假设被推翻。"+
			"若小结的【纠偏】里点到了你，请据此收敛、回到命题，别再发散。"+
			"若你对【待议】也无异议，请只回复『同意』；否则直接说出你的观点（不要加『同意』前缀）。", speaker)
		return b.String()
	}
	others := make([]string, 0, len(room.Participants))
	for _, p := range room.Participants {
		if p != speaker {
			others = append(others, p)
		}
	}
	fmt.Fprintf(&b, "轮到你（%s）发言。如需把问题交给某位参与者，请用 @他的名字。其他参与者：%s。",
		speaker, strings.Join(others, "、"))
	return b.String()
}

// snapshot returns a deep-ish copy safe to serialize.
func (m *RoomManager) snapshot(roomID string) (*RoomView, error) {
	room := m.get(roomID)
	if room == nil {
		return nil, fmt.Errorf("room %q not found", roomID)
	}
	room.mu.Lock()
	defer room.mu.Unlock()
	tr := make([]RoomTurn, len(room.transcript))
	copy(tr, room.transcript)
	return &RoomView{
		ID:           room.ID,
		Topic:        room.Topic,
		Participants: append([]string(nil), room.Participants...),
		Organizer:    room.Organizer,
		Mode:         room.Mode,
		Moderator:    room.Moderator,
		Status:       room.status,
		Next:         room.next,
		Speaking:     room.speaking,
		MaxChain:     room.MaxChain,
		Transcript:   tr,
		CreatedAt:    room.createdAt,
	}, nil
}

// ResumePending restarts the drive loop for any restored room left mid-turn
// (active with a scheduled next speaker), so a turn interrupted by a restart is
// actually retried. Call once after the scheduler has brought agents back up;
// each room waits for its next speaker to re-register before driving, so the
// compensating turn isn't sent before that agent has booted.
func (m *RoomManager) ResumePending() {
	m.mu.Lock()
	rooms := make([]*Room, 0, len(m.rooms))
	for _, r := range m.rooms {
		rooms = append(rooms, r)
	}
	m.mu.Unlock()

	for _, room := range rooms {
		room.mu.Lock()
		start := room.status == RoomActive && room.next != "" && !room.running
		if start {
			room.running = true
		}
		who, id := room.next, room.ID
		room.mu.Unlock()
		if !start {
			continue
		}
		go func(room *Room, who, id string) {
			// Wait for the target agent to (re)register before retrying, so the
			// compensating turn isn't delivered before the agent has booted.
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) {
				if m.isAgent == nil || m.isAgent(who) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			log.Printf("roundtable: resuming room %s — retrying interrupted turn for %q", id, who)
			m.drive(room)
		}(room, who, id)
	}
}

// List returns snapshots of all rooms, newest first.
func (m *RoomManager) List() []*RoomView {
	m.mu.Lock()
	ids := make([]string, 0, len(m.rooms))
	for id := range m.rooms {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	views := make([]*RoomView, 0, len(ids))
	for _, id := range ids {
		if v, err := m.snapshot(id); err == nil {
			views = append(views, v)
		}
	}
	// newest first
	for i := 0; i < len(views); i++ {
		for j := i + 1; j < len(views); j++ {
			if views[j].CreatedAt.After(views[i].CreatedAt) {
				views[i], views[j] = views[j], views[i]
			}
		}
	}
	return views
}

// Get returns a snapshot of one room.
func (m *RoomManager) Get(roomID string) (*RoomView, error) {
	return m.snapshot(roomID)
}

func (m *RoomManager) get(roomID string) *Room {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rooms[roomID]
}

// parseMentions extracts @-mentions that name a participant (case-insensitive),
// excluding `self`, preserving first-seen order and de-duplicating. The first
// element is the next speaker.
func parseMentions(text string, participants []string, self string) []string {
	canon := map[string]string{}
	for _, p := range participants {
		canon[strings.ToLower(p)] = p
	}
	// Drop code spans first so a quoted `@name` doesn't read as a directive.
	text = codeSpanRe.ReplaceAllString(text, " ")
	var out []string
	seen := map[string]bool{}
	for _, mm := range mentionRe.FindAllStringSubmatch(text, -1) {
		name := strings.ToLower(mm[1])
		p, ok := canon[name]
		if !ok || p == self || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, p)
	}
	return out
}
