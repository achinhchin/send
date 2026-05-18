package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"golang.org/x/term"
)

const CHUNK_SIZE = 64 * 1024

type Config struct {
	Host       *string `json:"host"`
	Port       *uint16 `json:"port"`
	Session    *string `json:"session"`
	OutputPath *string `json:"output_path"`
}

func configPath() string {
	// When using 'go run', os.Executable() returns a temporary folder in /var/folders/
	// To fix this, we'll just save it in the current working directory.
	return "send.json"
}

func loadConfig() Config {
	var c Config
	b, err := os.ReadFile(configPath())
	if err == nil {
		json.Unmarshal(b, &c)
	}
	return c
}

func saveConfig(c Config) {
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(configPath(), b, 0644)
	fmt.Fprintf(os.Stderr, "Config saved to: %s\n", configPath())
}

func prompt(label string) string {
	fmt.Printf("%s: ", label)
	var s string
	fmt.Scanln(&s)
	return strings.TrimSpace(s)
}

func getPrivateIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "unknown"
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return "unknown"
}

func sameLan(ip1, ip2 string) bool {
	p1 := strings.Split(ip1, ".")
	p2 := strings.Split(ip2, ".")
	if len(p1) >= 3 && len(p2) >= 3 && p1[0] == p2[0] && p1[1] == p2[1] && p1[2] == p2[2] {
		return true
	}
	return false
}

type DeviceEntry struct {
	ID         string `json:"id"`
	IP         string `json:"ip"`
	PrivateIP  string `json:"private_ip"`
	DeviceType string `json:"device_type"`
	JoinedAt   int64  `json:"joined_at"`
}

type Mode int
const (
	ModeDeviceList Mode = iota
	ModeFileInput
	ModeSending
	ModeReceiving
)

type App struct {
	Mode         Mode
	Devices      []DeviceEntry
	Selected     int
	MyID         string
	MyPrivateIP  string
	Status       string
	OutputPath   string
	
	// Input buf
	InputBuf string
	
	// Transfer state
	FileName    string
	FileTotal   int
	FileDone    int
	FilePercent int
}

func newApp(out string, privIP string) *App {
	return &App{
		Mode:        ModeDeviceList,
		MyPrivateIP: privIP,
		OutputPath:  out,
	}
}

func (a *App) targets() []DeviceEntry {
	var ts []DeviceEntry
	for _, d := range a.Devices {
		if d.ID != a.MyID {
			ts = append(ts, d)
		}
	}
	return ts
}

func main() {
	args := os.Args
	cmd := ""
	if len(args) > 1 {
		cmd = args[1]
	}
	config := loadConfig()
	exeName := "cli"
	if len(args) > 0 {
		exeName = filepath.Base(args[0])
	}

	switch cmd {
	case "host":
		if len(args) < 4 {
			fmt.Printf("Usage: %s host <ip> <port>\n", exeName)
			return
		}
		h := args[2]
		var p uint16
		fmt.Sscanf(args[3], "%d", &p)
		config.Host = &h
		config.Port = &p
		saveConfig(config)
		fmt.Printf("✓ Host set to %s:%d\n", h, p)
	case "signup":
		if config.Host == nil || config.Port == nil {
			fmt.Printf("Set host first:\n  %s host <ip> <port>\n", exeName)
			return
		}
		user := ""
		if len(args) > 2 {
			user = args[2]
		} else {
			user = prompt("Username")
		}
		fmt.Print("Password: ")
		passBytes, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		pass := string(passBytes)
		
		url := fmt.Sprintf("http://%s:%d/api/signup", *config.Host, *config.Port)
		b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(b))
		if err == nil && resp.StatusCode == 200 {
			fmt.Printf("✓ Account created! Now run:\n  %s login %s\n", exeName, user)
		} else {
			fmt.Println("✗ Signup failed (username may already exist)")
		}
	case "login":
		if config.Host == nil || config.Port == nil {
			fmt.Printf("Set host first:\n  %s host <ip> <port>\n", exeName)
			return
		}
		user := ""
		if len(args) > 2 {
			user = args[2]
		} else {
			user = prompt("Username")
		}
		fmt.Print("Password: ")
		passBytes, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		pass := string(passBytes)

		url := fmt.Sprintf("http://%s:%d/api/login", *config.Host, *config.Port)
		b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(b))
		if err == nil {
			var data map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&data)
			if data["status"] == "ok" {
				sess := data["session"].(string)
				config.Session = &sess
				saveConfig(config)
				fmt.Printf("✓ Logged in as %s\n", user)
			} else {
				fmt.Println("✗ Login failed. Wrong username or password.")
			}
		} else {
			fmt.Println("✗ Cannot connect to server")
		}
	case "output":
		if len(args) < 3 {
			fmt.Printf("Usage: %s output <path>\n", exeName)
			return
		}
		p := args[2]
		os.MkdirAll(p, 0755)
		config.OutputPath = &p
		saveConfig(config)
		fmt.Printf("✓ Output path set to: %s\n", p)
	case "help":
		fmt.Printf("Send CLI\n\nSetup:\n  %s host <ip> <port>\n  %s signup <user>\n  %s login <user>\n  %s output <path>\n\nConnect:\n  %s\n", exeName, exeName, exeName, exeName, exeName)
	default:
		// ── Check config completeness ───────────────────────────────
		if config.Host == nil || config.Port == nil {
			fmt.Printf("No server configured.\n\n")
			fmt.Printf("  %s host <ip_or_hostname> <port>\n", exeName)
			fmt.Printf("  Example: %s host 192.168.1.100 3000\n", exeName)
			return
		}

		if config.Session != nil {
			url := fmt.Sprintf("http://%s:%d/api/check-session", *config.Host, *config.Port)
			b, _ := json.Marshal(map[string]string{"session": *config.Session})
			resp, err := http.Post(url, "application/json", bytes.NewBuffer(b))
			if err != nil {
				fmt.Printf("✗ Cannot reach server at %s:%d (%s)\n", *config.Host, *config.Port, err)
				return
			}
			var data map[string]string
			json.NewDecoder(resp.Body).Decode(&data)
			if data["status"] != "ok" {
				fmt.Printf("Session expired.\n\n")
				fmt.Printf("  %s login <username>\n", exeName)
				config.Session = nil
				saveConfig(config)
				return
			}
		}

		if config.Session == nil {
			fmt.Printf("Not logged in.\n\n")
			fmt.Printf("  %s signup <username>   Create account\n", exeName)
			fmt.Printf("  %s login <username>    Login\n", exeName)
			return
		}

		if config.OutputPath == nil {
			fmt.Printf("No output path set.\n\n")
			fmt.Printf("  %s output <directory_path>\n", exeName)
			fmt.Printf("  Example: %s output ~/Downloads\n", exeName)
			return
		}

		runTUI(*config.Host, *config.Port, *config.Session, *config.OutputPath)
	}
}

// Global TUI channels and state
var (
	appState  *App
	appMu     sync.Mutex
	renderCh  = make(chan struct{}, 10)
	inputCh   = make(chan []byte, 100)
	wsConn    *websocket.Conn
	
	// Transfer state
	incomingChunks map[string][]string // from -> chunks
	incomingTotal  map[string]int
	incomingName   map[string]string
)

func runTUI(host string, port uint16, session string, output string) {
	wsURL := fmt.Sprintf("ws://%s:%d/ws", host, port)
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		fmt.Println("✗ WebSocket connection failed:", err)
		return
	}
	wsConn = c
	defer c.Close()

	privIP := getPrivateIP()
	authMsg := map[string]string{
		"type":        "auth",
		"session":     session,
		"device_type": "cli",
		"private_ip":  privIP,
	}
	c.WriteJSON(authMsg)

	appState = newApp(output, privIP)
	incomingChunks = make(map[string][]string)
	incomingTotal = make(map[string]int)
	incomingName = make(map[string]string)

	// Enter raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print("\033[?1049h\033[?25l") // alternate screen, hide cursor
	defer fmt.Print("\033[?1049l\033[?25h") // restore screen, show cursor

	go func() {
		b := make([]byte, 1)
		for {
			n, err := os.Stdin.Read(b)
			if err != nil || n == 0 {
				break
			}
			inputCh <- []byte{b[0]}
		}
	}()

	go func() {
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var v map[string]interface{}
			if err := json.Unmarshal(msg, &v); err == nil {
				appMu.Lock()
				handleWSMessage(v)
				appMu.Unlock()
				renderCh <- struct{}{}
			}
		}
	}()

	// Initial render
	renderCh <- struct{}{}

	for {
		select {
		case <-renderCh:
			appMu.Lock()
			render(appState)
			appMu.Unlock()
		case key := <-inputCh:
			appMu.Lock()
			exit := handleKey(key)
			appMu.Unlock()
			if exit {
				return
			}
			renderCh <- struct{}{}
		}
	}
}

func handleWSMessage(v map[string]interface{}) {
	typ, _ := v["type"].(string)
	switch typ {
	case "auth_error":
		appState.Status = "Session expired."
	case "state":
		if you, ok := v["you"].(string); ok {
			appState.MyID = you
		}
		if devs, ok := v["devices"].(map[string]interface{}); ok {
			var list []DeviceEntry
			for id, d := range devs {
				info := d.(map[string]interface{})
				list = append(list, DeviceEntry{
					ID:         id,
					IP:         info["ip"].(string),
					PrivateIP:  info["private_ip"].(string),
					DeviceType: info["device_type"].(string),
					JoinedAt:   int64(info["joined_at"].(float64)),
				})
			}
			appState.Devices = list
		}
	// WebRTC Fallback Relay Handlers
	case "file_offer":
		from, _ := v["from"].(string)
		name, _ := v["filename"].(string)
		sizeFloat, _ := v["size"].(float64)
		size := int(sizeFloat)
		total := (size + CHUNK_SIZE - 1) / CHUNK_SIZE
		incomingChunks[from] = make([]string, total)
		incomingTotal[from] = total
		incomingName[from] = name
		appState.Mode = ModeReceiving
		appState.FileName = name
		appState.FilePercent = 0
		
		route := "Server Relay"
		for _, d := range appState.Devices {
			if d.ID == from {
				if sameLan(appState.MyPrivateIP, d.PrivateIP) {
					route = fmt.Sprintf("LAN Relay (%s → Server → %s)", d.PrivateIP, appState.MyPrivateIP)
				} else {
					route = fmt.Sprintf("WAN Relay (%s → Server → %s)", d.IP, appState.MyPrivateIP)
				}
				break
			}
		}
		appState.Status = fmt.Sprintf("Incoming via %s: %s (%d bytes)", route, name, size)
	case "file_chunk":
		from, _ := v["from"].(string)
		idxFloat, _ := v["index"].(float64)
		idx := int(idxFloat)
		data, _ := v["data"].(string)
		if chunks, ok := incomingChunks[from]; ok {
			if idx < len(chunks) {
				chunks[idx] = data
			}
			done := 0
			for _, c := range chunks {
				if c != "" {
					done++
				}
			}
			pct := 0
			if incomingTotal[from] > 0 {
				pct = done * 100 / incomingTotal[from]
			}
			appState.Mode = ModeReceiving
			appState.FileName = incomingName[from]
			appState.FilePercent = pct
		}
	case "file_done":
		from, _ := v["from"].(string)
		if chunks, ok := incomingChunks[from]; ok {
			var b []byte
			for _, chunk := range chunks {
				dec, _ := base64.StdEncoding.DecodeString(chunk)
				b = append(b, dec...)
			}
			path := filepath.Join(appState.OutputPath, incomingName[from])
			os.WriteFile(path, b, 0644)
			appState.Status = fmt.Sprintf("✓ Saved '%s'", path)
			appState.Mode = ModeDeviceList
			delete(incomingChunks, from)
			delete(incomingTotal, from)
			delete(incomingName, from)
		}
	// --- WebRTC Signaling Messages ---
	case "webrtc_offer":
		from, _ := v["from"].(string)
		sdp, _ := v["sdp"].(string)
		name, _ := v["filename"].(string)
		size, _ := v["size"].(float64)
		appState.Status = fmt.Sprintf("WebRTC offer from %s for %s (%.0f bytes)", from, name, size)
		go handleIncomingWebRTC(from, sdp, name, int(size))
	case "webrtc_answer":
		from, _ := v["from"].(string)
		sdp, _ := v["sdp"].(string)
		if pc, ok := peerConnections[from]; ok {
			pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp})
		}
	case "webrtc_ice":
		from, _ := v["from"].(string)
		candidate, _ := v["candidate"].(string)
		if pc, ok := peerConnections[from]; ok {
			pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate})
		}
	}
}

// ── WebRTC Engine ───────────────────────────────────────────────────────────
var (
	peerConnections = make(map[string]*webrtc.PeerConnection)
	pcMu            sync.Mutex
)

// STUN servers for WAN Hole Punching
var webrtcConfig = webrtc.Configuration{
	ICEServers: []webrtc.ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: []string{"stun:stun1.l.google.com:19302"}},
	},
}

func sendFile(target DeviceEntry, filename string, b []byte) {

	// Mode and initial status are now set synchronously in handleKey

	pc, err := webrtc.NewPeerConnection(webrtcConfig)
	if err != nil {
		fallbackSendViaServer(target, filename, b)
		return
	}

	pcMu.Lock()
	peerConnections[target.ID] = pc
	pcMu.Unlock()

	dc, err := pc.CreateDataChannel("file", nil)
	if err != nil {
		fallbackSendViaServer(target, filename, b)
		return
	}

	dc.OnOpen(func() {
		appMu.Lock()
		route := "WAN"
		if sameLan(appState.MyPrivateIP, target.PrivateIP) {
			route = "LAN"
		}
		appState.Status = fmt.Sprintf("Direct %s: Sending %s to %s", route, filename, target.ID)
		appMu.Unlock()
		renderCh <- struct{}{}

		// Send file chunks over DataChannel
		for i := 0; i < len(b); i += CHUNK_SIZE {
			end := i + CHUNK_SIZE
			if end > len(b) {
				end = len(b)
			}
			dc.Send(b[i:end])
			appMu.Lock()
			appState.FileDone = end
			appMu.Unlock()
			renderCh <- struct{}{}
		}
		dc.SendText("DONE")
		time.Sleep(1 * time.Second) // wait for buffer to flush
		
		appMu.Lock()
		appState.Status = fmt.Sprintf("✓ Sent '%s' via Direct %s WebRTC", filename, route)
		appState.Mode = ModeDeviceList
		appMu.Unlock()
		renderCh <- struct{}{}
	})

	pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i != nil {
			wsConn.WriteJSON(map[string]interface{}{
				"type": "webrtc_ice", "to": target.ID, "candidate": i.ToJSON().Candidate,
			})
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		fallbackSendViaServer(target, filename, b)
		return
	}
	pc.SetLocalDescription(offer)

	wsConn.WriteJSON(map[string]interface{}{
		"type": "webrtc_offer", "to": target.ID, "sdp": offer.SDP,
		"filename": filename, "size": len(b),
	})

	// Fallback timeout
	go func() {
		time.Sleep(5 * time.Second)
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			appMu.Lock()
			appState.Status = "WebRTC Timeout, falling back to Server Relay..."
			appMu.Unlock()
			renderCh <- struct{}{}
			
			pc.Close()
			pcMu.Lock()
			delete(peerConnections, target.ID)
			pcMu.Unlock()
			fallbackSendViaServer(target, filename, b)
		}
	}()
}

func handleIncomingWebRTC(from string, sdp string, filename string, size int) {
	pc, err := webrtc.NewPeerConnection(webrtcConfig)
	if err != nil { return }
	
	pcMu.Lock()
	peerConnections[from] = pc
	pcMu.Unlock()

	var fileBuf []byte

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			appMu.Lock()
			appState.Mode = ModeReceiving
			appState.FileName = filename
			appState.FilePercent = 0
			appState.Status = fmt.Sprintf("Receiving via Direct WebRTC from %s", from)
			appMu.Unlock()
			renderCh <- struct{}{}
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString && string(msg.Data) == "DONE" {
				path := filepath.Join(appState.OutputPath, filename)
				os.WriteFile(path, fileBuf, 0644)
				appMu.Lock()
				appState.Status = fmt.Sprintf("✓ Saved '%s' (Direct WebRTC)", path)
				appState.Mode = ModeDeviceList
				appMu.Unlock()
				renderCh <- struct{}{}
				return
			}
			fileBuf = append(fileBuf, msg.Data...)
			
			appMu.Lock()
			pct := 0
			if size > 0 { pct = len(fileBuf) * 100 / size }
			appState.FilePercent = pct
			appMu.Unlock()
			renderCh <- struct{}{}
		})
	})

	pc.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i != nil {
			wsConn.WriteJSON(map[string]interface{}{
				"type": "webrtc_ice", "to": from, "candidate": i.ToJSON().Candidate,
			})
		}
	})

	pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp})
	answer, _ := pc.CreateAnswer(nil)
	pc.SetLocalDescription(answer)

	wsConn.WriteJSON(map[string]interface{}{
		"type": "webrtc_answer", "to": from, "sdp": answer.SDP,
	})
}

func fallbackSendViaServer(target DeviceEntry, filename string, b []byte) {
	appMu.Lock()
	route := "WAN Relay"
	if sameLan(appState.MyPrivateIP, target.PrivateIP) {
		route = "LAN Relay"
	}
	appState.Mode = ModeSending
	appState.FileName = filename
	appState.FileTotal = len(b)
	appState.FileDone = 0
	appState.Status = fmt.Sprintf("Sending via %s to %s", route, target.ID)
	appMu.Unlock()
	renderCh <- struct{}{}

	total := (len(b) + CHUNK_SIZE - 1) / CHUNK_SIZE
	wsConn.WriteJSON(map[string]interface{}{
		"type": "file_offer", "to": target.ID, "filename": filename, "size": len(b),
	})

	for i := 0; i < total; i++ {
		start := i * CHUNK_SIZE
		end := start + CHUNK_SIZE
		if end > len(b) {
			end = len(b)
		}
		b64 := base64.StdEncoding.EncodeToString(b[start:end])
		wsConn.WriteJSON(map[string]interface{}{
			"type": "file_chunk", "to": target.ID, "index": i, "data": b64,
		})
		appMu.Lock()
		appState.FileDone = end
		appMu.Unlock()
		renderCh <- struct{}{}
	}
	wsConn.WriteJSON(map[string]interface{}{
		"type": "file_done", "to": target.ID, "filename": filename, "totalChunks": total,
	})
	
	appMu.Lock()
	appState.Status = fmt.Sprintf("✓ Sent '%s' via %s", filename, route)
	appState.Mode = ModeDeviceList
	appMu.Unlock()
	renderCh <- struct{}{}
}

// ── TUI ─────────────────────────────────────────────────────────────────────

func handleKey(key []byte) bool {
	if len(key) == 0 { return false }
	c := key[0]
	
	// Quit on q or Ctrl+C
	if c == 'q' || c == 3 {
		return true
	}

	switch appState.Mode {
	case ModeDeviceList:
		if c == '\033' && len(key) >= 3 && key[1] == '[' {
			if key[2] == 'A' { // Up
				if appState.Selected > 0 { appState.Selected-- }
			} else if key[2] == 'B' { // Down
				if appState.Selected < len(appState.targets())-1 { appState.Selected++ }
			}
		} else if c == '\r' || c == '\n' {
			ts := appState.targets()
			if appState.Selected < len(ts) {
				target := ts[appState.Selected]
				route := "WAN"
				if sameLan(appState.MyPrivateIP, target.PrivateIP) {
					route = "LAN"
				}
				
				appState.Mode = ModeFileInput
				appState.InputBuf = ""
				appState.Status = fmt.Sprintf("→ %s  │  Priority: Direct %s WebRTC", target.ID, route)
			}
		}
	case ModeFileInput:
		if c == '\033' { // Esc
			appState.Mode = ModeDeviceList
			appState.Status = ""
		} else if c == '\r' || c == '\n' {
			ts := appState.targets()
			if appState.Selected < len(ts) {
				target := ts[appState.Selected]
				path := strings.TrimSpace(appState.InputBuf)
				
				b, err := os.ReadFile(path)
				if err != nil {
					appState.Status = fmt.Sprintf("✗ File not found: %s", path)
					appState.Mode = ModeDeviceList
				} else {
					filename := filepath.Base(path)
					appState.Mode = ModeSending
					appState.FileName = filename
					appState.FileTotal = len(b)
					appState.FileDone = 0
					appState.Status = fmt.Sprintf("Negotiating WebRTC Direct Connection to %s...", target.ID)
					go sendFile(target, filename, b)
				}
			}
		} else if c == 127 || c == 8 { // Backspace
			if len(appState.InputBuf) > 0 {
				appState.InputBuf = appState.InputBuf[:len(appState.InputBuf)-1]
			}
		} else if c >= 32 && c <= 126 {
			appState.InputBuf += string(c)
		}
	}
	return false
}

func render(app *App) {
	out := "\033[2J\033[H" // Clear screen and move to top-left

	out += "╔════════════════════════════════════════════════════════════════════════════════╗\r\n"
	myID := app.MyID
	if myID == "" { myID = "connecting..." }
	out += fmt.Sprintf("║  Send (Go) │  You: %-60s║\r\n", myID)
	out += "╠════════════════════════════════════════════════════════════════════════════════╣\r\n"
	out += "║  #  │ Device          │ Type │ Public IP       │ Private IP      │ Joined    ║\r\n"
	out += "╠═════╪═════════════════╪══════╪═════════════════╪═════════════════╪═══════════╣\r\n"

	if len(app.Devices) == 0 {
		out += "║                            No devices connected                            ║\r\n"
	} else {
		targetIdx := 0
		for _, dev := range app.Devices {
			isYou := dev.ID == app.MyID
			joined := "just now" // simplified
			
			if isYou {
				label := dev.ID + " (You)"
				out += fmt.Sprintf("║     │ %-15s │ %-4s │ %-15s │ %-15s │ %-9s ║\r\n", trunc(label, 15), trunc(dev.DeviceType, 4), trunc(dev.IP, 15), trunc(dev.PrivateIP, 15), joined)
			} else {
				marker := " "
				if targetIdx == app.Selected { marker = "►" }
				out += fmt.Sprintf("║ %s%-3d │ %-15s │ %-4s │ %-15s │ %-15s │ %-9s ║\r\n", marker, targetIdx+1, trunc(dev.ID, 15), trunc(dev.DeviceType, 4), trunc(dev.IP, 15), trunc(dev.PrivateIP, 15), joined)
				targetIdx++
			}
		}
	}

	out += "╠════════════════════════════════════════════════════════════════════════════════╣\r\n"

	switch app.Mode {
	case ModeDeviceList:
		out += "║  ↑↓ Select  │  Enter: Send File  │  q: Quit                                  ║\r\n"
	case ModeFileInput:
		disp := app.InputBuf
		if len(disp) > 60 { disp = disp[len(disp)-60:] }
		out += fmt.Sprintf("║  File path: %-64s║\r\n", disp)
		out += "║  Enter: Send  │  Esc: Cancel                                                ║\r\n"
	case ModeSending:
		pct := 0
		if app.FileTotal > 0 { pct = app.FileDone * 100 / app.FileTotal }
		bar := makeBar(pct, 40)
		out += fmt.Sprintf("║  Sending: %-20s [%s] %3d%%                  ║\r\n", trunc(app.FileName, 20), bar, pct)
	case ModeReceiving:
		bar := makeBar(app.FilePercent, 40)
		out += fmt.Sprintf("║  Receiving: %-18s [%s] %3d%%                  ║\r\n", trunc(app.FileName, 18), bar, app.FilePercent)
	}

	if app.Status != "" {
		out += fmt.Sprintf("║  %-76s║\r\n", trunc(app.Status, 76))
	}

	out += "╚════════════════════════════════════════════════════════════════════════════════╝\r\n"
	fmt.Print(out)
}

func trunc(s string, l int) string {
	if len(s) > l { return s[:l] }
	return s
}

func makeBar(pct int, width int) string {
	filled := width * pct / 100
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}
