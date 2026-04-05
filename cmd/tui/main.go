package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorilla/websocket"
)

// --- Data types ---

type ObserverSummary struct {
	ObserverID   string   `json:"observer_id"`
	ObserverName *string  `json:"observer_name"`
	CurrentNF    *float64 `json:"current_noise_floor"`
	AvgNF        *float64 `json:"avg_noise_floor_24h"`
	MaxNF        *float64 `json:"max_noise_floor_24h"`
	BatteryMv    *int     `json:"battery_mv"`
	SampleCount  int      `json:"sample_count"`
}

type Packet struct {
	Timestamp    string
	Type         string
	ObserverName string
	Hops         string
	RSSI         string
	SNR          string
	ChannelText  string
}

// --- Messages ---

type summaryMsg []ObserverSummary
type summaryErrMsg struct{ err error }
type packetMsg Packet
type wsStatusMsg string
type tickMsg time.Time

// --- Model ---

type view int

const (
	viewDashboard view = iota
	viewLiveFeed
)

// ringBufferMax is the maximum number of packets kept in the live feed.
const ringBufferMax = 500

type model struct {
	baseURL     string
	currentView view
	width       int
	height      int

	// Dashboard
	observers   []ObserverSummary
	lastRefresh time.Time
	fetchErr    error

	// Live feed — ring buffer with head/tail indices, no allocations in steady state.
	ringBuf  [ringBufferMax]Packet
	ringHead int // index of oldest element
	ringLen  int // number of elements in the buffer
	// wsMsgChan multiplexes packets and status updates from the WS goroutine
	// into the bubbletea event loop.
	wsMsgChan  chan tea.Msg
	wsStatus   string
	wsDone     chan struct{}
	wsCloseOnce sync.Once
}

func initialModel(baseURL string) model {
	return model{
		baseURL:   strings.TrimRight(baseURL, "/"),
		wsStatus:  "disconnected",
		wsMsgChan: make(chan tea.Msg, 100),
		wsDone:    make(chan struct{}),
	}
}

// --- Styles ---

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69"))
	greenStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	yellowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("226"))
	redStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	statusStyle = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252")).Padding(0, 1)
	tabActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69")).Underline(true)
	tabInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
)

// --- Commands ---

func fetchSummary(baseURL string) tea.Cmd {
	return func() tea.Msg {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Get(baseURL + "/api/observers/metrics/summary?window=24h")
		if err != nil {
			return summaryErrMsg{err}
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return summaryErrMsg{err}
		}
		var result []ObserverSummary
		if err := json.Unmarshal(body, &result); err != nil {
			return summaryErrMsg{fmt.Errorf("json: %w (body: %.100s)", err, string(body))}
		}
		return summaryMsg(result)
	}
}

func tickEvery(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// listenForWSMsg waits for the next message from the WebSocket goroutine and
// delivers it into the bubbletea event loop. Returns nil when the channel is
// closed (program shutting down).
func listenForWSMsg(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// --- WebSocket goroutine ---

// connectWS manages the WebSocket connection with exponential backoff reconnect.
// It sends packetMsg and wsStatusMsg on msgChan. It returns when done is closed.
func connectWS(baseURL string, msgChan chan<- tea.Msg, done <-chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			select {
			case msgChan <- wsStatusMsg(fmt.Sprintf("panic: %v", r)):
			default:
			}
		}
	}()

	u, err := url.Parse(baseURL)
	if err != nil {
		select {
		case msgChan <- wsStatusMsg("invalid url"):
		case <-done:
		}
		return
	}
	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	wsURL := scheme + "://" + u.Host + "/ws"

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-done:
			return
		default:
		}

		sendStatus(msgChan, done, "connecting...")
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			sendStatus(msgChan, done, fmt.Sprintf("error: %v", err))
			select {
			case <-done:
				return
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}

		sendStatus(msgChan, done, "connected")
		backoff = time.Second

		// readLoop reads messages until error or done. We use a read deadline
		// so that ReadMessage unblocks periodically, letting us check done.
		func() {
			defer conn.Close()
			for {
				select {
				case <-done:
					// Send a graceful close frame before returning.
					_ = conn.WriteMessage(
						websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
					)
					return
				default:
				}

				// Set a deadline so ReadMessage doesn't block forever, allowing
				// us to re-check the done channel.
				conn.SetReadDeadline(time.Now().Add(2 * time.Second))
				_, message, err := conn.ReadMessage()
				if err != nil {
					if websocket.IsCloseError(err, websocket.CloseNormalClosure) {
						sendStatus(msgChan, done, "disconnected")
						return
					}
					// Timeout is expected — just loop back to check done.
					if netErr, ok := err.(*websocket.CloseError); ok {
						sendStatus(msgChan, done, fmt.Sprintf("closed: %d", netErr.Code))
						return
					}
					if isTimeoutError(err) {
						continue
					}
					sendStatus(msgChan, done, "disconnected")
					return
				}

				pkt := parseWSMessage(message)
				if pkt != nil {
					select {
					case msgChan <- packetMsg(*pkt):
					case <-done:
						return
					}
				}
			}
		}()
	}
}

// sendStatus sends a wsStatusMsg, respecting cancellation.
func sendStatus(msgChan chan<- tea.Msg, done <-chan struct{}, status string) {
	select {
	case msgChan <- wsStatusMsg(status):
	case <-done:
	}
}

// isTimeoutError checks if an error is a network timeout (read deadline exceeded).
func isTimeoutError(err error) bool {
	// net.Error has a Timeout() method.
	type timeout interface {
		Timeout() bool
	}
	if t, ok := err.(timeout); ok {
		return t.Timeout()
	}
	return false
}

func parseWSMessage(data []byte) *Packet {
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil
	}

	pkt := &Packet{}

	// Timestamp
	if decoded, ok := msg["decoded"].(map[string]interface{}); ok {
		if ts, ok := decoded["timestamp"].(string); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				pkt.Timestamp = t.Format("15:04:05")
			} else {
				pkt.Timestamp = ts
			}
		}
	}
	if pkt.Timestamp == "" {
		if packet, ok := msg["packet"].(map[string]interface{}); ok {
			if ts, ok := packet["timestamp"].(string); ok {
				if t, err := time.Parse(time.RFC3339, ts); err == nil {
					pkt.Timestamp = t.Format("15:04:05")
				} else if len(ts) >= 8 {
					pkt.Timestamp = ts[:8]
				} else {
					pkt.Timestamp = ts
				}
			}
		}
	}
	if pkt.Timestamp == "" {
		pkt.Timestamp = time.Now().Format("15:04:05")
	}

	// Type
	if decoded, ok := msg["decoded"].(map[string]interface{}); ok {
		if t, ok := decoded["type"].(string); ok {
			pkt.Type = t
		}
	}
	if pkt.Type == "" {
		if packet, ok := msg["packet"].(map[string]interface{}); ok {
			if t, ok := packet["type"].(string); ok {
				pkt.Type = t
			}
		}
	}
	if pkt.Type == "" {
		pkt.Type = "UNKNOWN"
	}

	// Observer name
	if packet, ok := msg["packet"].(map[string]interface{}); ok {
		if name, ok := packet["observer_name"].(string); ok {
			pkt.ObserverName = name
		} else if name, ok := packet["observer_id"].(string); ok {
			pkt.ObserverName = safePrefix(name, 8)
		}
	}

	// Hops
	if decoded, ok := msg["decoded"].(map[string]interface{}); ok {
		if hops, ok := decoded["hops"].(float64); ok {
			pkt.Hops = fmt.Sprintf("%d", int(hops))
		}
	}

	// RSSI / SNR
	if packet, ok := msg["packet"].(map[string]interface{}); ok {
		if rssi, ok := packet["rssi"].(float64); ok {
			pkt.RSSI = fmt.Sprintf("%.0f", rssi)
		}
		if snr, ok := packet["snr"].(float64); ok {
			pkt.SNR = fmt.Sprintf("%.1f", snr)
		}
	}

	// Channel text
	if decoded, ok := msg["decoded"].(map[string]interface{}); ok {
		ch := ""
		if name, ok := decoded["channel_name"].(string); ok {
			ch = "#" + name
		}
		if text, ok := decoded["text"].(string); ok {
			if ch != "" {
				pkt.ChannelText = ch + " " + truncate(text, 40)
			} else {
				pkt.ChannelText = truncate(text, 40)
			}
		}
	}

	return pkt
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// safePrefix returns the first n bytes of s, or s itself if shorter.
func safePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// --- Init / Update / View ---

func (m model) Init() tea.Cmd {
	go connectWS(m.baseURL, m.wsMsgChan, m.wsDone)

	return tea.Batch(
		fetchSummary(m.baseURL),
		tickEvery(5*time.Second),
		listenForWSMsg(m.wsMsgChan),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.wsCloseOnce.Do(func() { close(m.wsDone) })
			return m, tea.Quit
		case "tab", "1":
			if m.currentView == viewDashboard {
				m.currentView = viewLiveFeed
			} else {
				m.currentView = viewDashboard
			}
		case "2":
			m.currentView = viewLiveFeed
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case summaryMsg:
		m.observers = []ObserverSummary(msg)
		m.lastRefresh = time.Now()
		m.fetchErr = nil

	case summaryErrMsg:
		m.fetchErr = msg.err

	case tickMsg:
		return m, tea.Batch(
			fetchSummary(m.baseURL),
			tickEvery(5*time.Second),
			listenForWSMsg(m.wsMsgChan),
		)

	case wsStatusMsg:
		m.wsStatus = string(msg)
		return m, listenForWSMsg(m.wsMsgChan)

	case packetMsg:
		p := Packet(msg)
		// Ring buffer: write at (head+len) % cap, no allocations.
		if m.ringLen < ringBufferMax {
			m.ringBuf[(m.ringHead+m.ringLen)%ringBufferMax] = p
			m.ringLen++
		} else {
			// Overwrite oldest, advance head.
			m.ringBuf[m.ringHead] = p
			m.ringHead = (m.ringHead + 1) % ringBufferMax
		}
		return m, listenForWSMsg(m.wsMsgChan)
	}

	// Always keep the WS listener running, even for unhandled messages.
	return m, listenForWSMsg(m.wsMsgChan)
}

func (m model) View() string {
	var b strings.Builder

	// Title
	b.WriteString(titleStyle.Render("🍄 CoreScope TUI"))
	b.WriteString("\n")

	// Tabs
	dash := tabInactive.Render("[1:Dashboard]")
	live := tabInactive.Render("[2:Live Feed]")
	if m.currentView == viewDashboard {
		dash = tabActive.Render("[1:Dashboard]")
	} else {
		live = tabActive.Render("[2:Live Feed]")
	}
	b.WriteString(dash + "  " + live + "\n\n")

	// Content
	switch m.currentView {
	case viewDashboard:
		b.WriteString(m.viewDashboard())
	case viewLiveFeed:
		b.WriteString(m.viewLiveFeed())
	}

	// Status bar
	b.WriteString("\n")
	wsIcon := "●"
	wsColor := redStyle
	if m.wsStatus == "connected" {
		wsColor = greenStyle
	} else if m.wsStatus == "connecting..." {
		wsColor = yellowStyle
	}
	status := fmt.Sprintf(" WS: %s %s │ View: %s │ %s │ q:quit Tab:switch",
		wsColor.Render(wsIcon), m.wsStatus,
		viewName(m.currentView),
		m.baseURL,
	)
	b.WriteString(statusStyle.Render(status))

	return b.String()
}

func viewName(v view) string {
	if v == viewDashboard {
		return "Dashboard"
	}
	return "Live Feed"
}

func (m model) viewDashboard() string {
	var b strings.Builder

	if m.fetchErr != nil {
		b.WriteString(redStyle.Render(fmt.Sprintf("Error: %v", m.fetchErr)))
		b.WriteString("\n\n")
	}

	// Sort by worst noise floor (highest = worst)
	observers := make([]ObserverSummary, len(m.observers))
	copy(observers, m.observers)
	sort.Slice(observers, func(i, j int) bool {
		ni, nj := nfVal(observers[i].CurrentNF), nfVal(observers[j].CurrentNF)
		return ni > nj // worst first
	})

	refreshStr := ""
	if !m.lastRefresh.IsZero() {
		refreshStr = m.lastRefresh.Format("15:04:05")
	}
	b.WriteString(fmt.Sprintf("Observers: %d │ Last refresh: %s\n\n",
		len(observers), refreshStr))

	// Header
	b.WriteString(headerStyle.Render(fmt.Sprintf("%-24s %8s %8s %8s %10s %8s",
		"Observer", "NF(dBm)", "Avg NF", "Max NF", "Battery", "Samples")))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", 78)))
	b.WriteString("\n")

	for _, o := range observers {
		name := safePrefix(o.ObserverID, 8)
		if o.ObserverName != nil && *o.ObserverName != "" {
			name = truncate(*o.ObserverName, 24)
		}

		nf := fmtNF(o.CurrentNF)
		avg := fmtNF(o.AvgNF)
		maxnf := fmtNF(o.MaxNF)
		batt := "—"
		if o.BatteryMv != nil {
			batt = fmt.Sprintf("%dmV", *o.BatteryMv)
		}

		// Color code NF
		nfStyle := greenStyle
		if o.CurrentNF != nil {
			if *o.CurrentNF > -85 {
				nfStyle = redStyle
			} else if *o.CurrentNF > -100 {
				nfStyle = yellowStyle
			}
		}

		line := fmt.Sprintf("%-24s %8s %8s %8s %10s %8d",
			name, nfStyle.Render(nf), avg, maxnf, batt, o.SampleCount)
		b.WriteString(line + "\n")
	}

	return b.String()
}

func nfVal(nf *float64) float64 {
	if nf == nil {
		return -999
	}
	return *nf
}

func fmtNF(nf *float64) string {
	if nf == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *nf)
}

func (m model) viewLiveFeed() string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("Packets: %d/%d │ WS: %s\n\n", m.ringLen, ringBufferMax, m.wsStatus))

	b.WriteString(headerStyle.Render(fmt.Sprintf("%-10s %-10s %-20s %5s %6s %6s  %s",
		"Time", "Type", "Observer", "Hops", "RSSI", "SNR", "Channel/Text")))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(strings.Repeat("─", 85)))
	b.WriteString("\n")

	// Show last N packets that fit the screen
	maxLines := 20
	if m.height > 10 {
		maxLines = m.height - 10
	}
	// Calculate visible range from the ring buffer (most recent packets).
	visible := m.ringLen
	if visible > maxLines {
		visible = maxLines
	}
	startIdx := m.ringLen - visible // offset from oldest

	for i := 0; i < visible; i++ {
		p := m.ringBuf[(m.ringHead+startIdx+i)%ringBufferMax]
		typeStyle := dimStyle
		switch p.Type {
		case "ADVERT":
			typeStyle = greenStyle
		case "GRP_TXT", "TXT_MSG":
			typeStyle = yellowStyle
		case "REQ":
			typeStyle = redStyle
		}

		line := fmt.Sprintf("%-10s %s %-20s %5s %6s %6s  %s",
			dimStyle.Render(p.Timestamp),
			typeStyle.Render(fmt.Sprintf("%-10s", p.Type)),
			truncate(p.ObserverName, 20),
			p.Hops, p.RSSI, p.SNR,
			dimStyle.Render(p.ChannelText),
		)
		b.WriteString(line + "\n")
	}

	return b.String()
}

// --- Main ---

func main() {
	urlFlag := flag.String("url", "http://localhost:3000", "CoreScope server URL")
	flag.Parse()

	m := initialModel(*urlFlag)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
