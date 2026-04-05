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

type model struct {
	baseURL     string
	currentView view
	width       int
	height      int

	// Dashboard
	observers   []ObserverSummary
	lastRefresh time.Time
	fetchErr    error

	// Live feed
	packets    []Packet
	wsStatus   string
	packetsMu  sync.Mutex
	packetChan chan Packet
	wsDone     chan struct{}
}

func initialModel(baseURL string) model {
	return model{
		baseURL:    strings.TrimRight(baseURL, "/"),
		packets:    make([]Packet, 0, 500),
		wsStatus:   "disconnected",
		packetChan: make(chan Packet, 100),
		wsDone:     make(chan struct{}),
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
		body, err := io.ReadAll(resp.Body)
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

func listenForPackets(ch <-chan Packet) tea.Cmd {
	return func() tea.Msg {
		p := <-ch
		return packetMsg(p)
	}
}

// --- WebSocket goroutine ---

func connectWS(baseURL string, packetChan chan<- Packet, statusChan chan<- string, done <-chan struct{}) {
	u, err := url.Parse(baseURL)
	if err != nil {
		statusChan <- "invalid url"
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

		statusChan <- "connecting..."
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			statusChan <- fmt.Sprintf("error: %v", err)
			select {
			case <-done:
				return
			case <-time.After(backoff):
			}
			backoff = time.Duration(math.Min(float64(backoff)*2, float64(maxBackoff)))
			continue
		}

		statusChan <- "connected"
		backoff = time.Second

		for {
			select {
			case <-done:
				conn.Close()
				return
			default:
			}

			_, message, err := conn.ReadMessage()
			if err != nil {
				statusChan <- "disconnected"
				conn.Close()
				break
			}

			pkt := parseWSMessage(message)
			if pkt != nil {
				select {
				case packetChan <- *pkt:
				default: // drop if buffer full
				}
			}
		}
	}
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
				} else {
					pkt.Timestamp = ts[:8]
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
			pkt.ObserverName = name[:8]
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

// --- Init / Update / View ---

func (m model) Init() tea.Cmd {
	// Start WS in background
	statusChan := make(chan string, 10)
	go connectWS(m.baseURL, m.packetChan, statusChan, m.wsDone)
	go func() {
		for s := range statusChan {
			m.packetChan <- Packet{Type: "__status__", ObserverName: s}
		}
	}()

	return tea.Batch(
		fetchSummary(m.baseURL),
		tickEvery(5*time.Second),
		listenForPackets(m.packetChan),
	)
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			close(m.wsDone)
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
		)

	case packetMsg:
		p := Packet(msg)
		if p.Type == "__status__" {
			m.wsStatus = p.ObserverName
			return m, listenForPackets(m.packetChan)
		}
		m.packets = append(m.packets, p)
		if len(m.packets) > 500 {
			m.packets = m.packets[len(m.packets)-500:]
		}
		return m, listenForPackets(m.packetChan)
	}

	return m, nil
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
		name := o.ObserverID[:8]
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

	b.WriteString(fmt.Sprintf("Packets: %d/500 │ WS: %s\n\n", len(m.packets), m.wsStatus))

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
	start := 0
	if len(m.packets) > maxLines {
		start = len(m.packets) - maxLines
	}

	for _, p := range m.packets[start:] {
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
