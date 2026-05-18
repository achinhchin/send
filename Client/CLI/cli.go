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
const MAX_BUFFER = 1 * 1024 * 1024 // 1MB max in-flight buffer for WebRTC
const MAX_INFLIGHT_CHUNKS = 16     // Max chunks in-flight for WS relay

type Config struct {
	Host       *string `json:"host"`
	Port       *uint16 `json:"port"`
	Session    *string `json:"session"`
	OutputPath *string `json:"output_path"`
	DeviceName *string `json:"device_name"`
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
	Mode        Mode
	Devices     []DeviceEntry
	Selected    int
	MyID        string
	MyUsername  string
	MyPrivateIP string
	Status      string
	OutputPath  string

	// Input buf
	InputBuf string

	// Transfer state
	FileName      string
	FileTotal     int
	FileDone      int
	FilePercent   int
	TransferStart time.Time
}

func newApp(out string, privIP string, username string) *App {
	return &App{
		Mode:        ModeDeviceList,
		MyPrivateIP: privIP,
		MyUsername:  username,
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
	case "name":
		if len(args) < 3 {
			fmt.Printf("Usage: %s name <device_name>\n", exeName)
			return
		}
		n := args[2]
		config.DeviceName = &n
		saveConfig(config)
		fmt.Printf("✓ Device name set to: %s\n", n)
	case "delete-account":
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

		confirm := prompt("Are you sure you want to permanently delete this account? (y/n)")
		if strings.ToLower(confirm) != "y" {
			fmt.Println("Cancelled.")
			return
		}

		url := fmt.Sprintf("http://%s:%d/api/delete-account", *config.Host, *config.Port)
		b, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		resp, err := http.Post(url, "application/json", bytes.NewBuffer(b))
		if err == nil {
			if resp.StatusCode == 200 {
				fmt.Printf("✓ Account '%s' deleted successfully.\n", user)
				if config.Session != nil {
					// Clear local session just in case it was our account
					config.Session = nil
					saveConfig(config)
				}
			} else {
				fmt.Println("✗ Deletion failed. Wrong username or password.")
			}
		} else {
			fmt.Println("✗ Cannot connect to server")
		}
	case "help":
		fmt.Printf("Send CLI\n\nSetup:\n  %s host <ip> <port>\n  %s signup <user>\n  %s login <user>\n  %s name <dev_name>\n  %s output <path>\n  %s delete-account <user>\n\nConnect:\n  %s\n", exeName, exeName, exeName, exeName, exeName, exeName, exeName)
	default:
		// ── Check config completeness ───────────────────────────────
		if config.Host == nil || config.Port == nil {
			fmt.Printf("No server configured.\n\n")
			fmt.Printf("  %s host <ip_or_hostname> <port>\n", exeName)
			fmt.Printf("  Example: %s host 192.168.1.100 3000\n", exeName)
			return
		}

		var username string

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
			username = data["username"]
		}

		if config.Session == nil {
			fmt.Printf("Not logged in.\n\n")
			fmt.Printf("  %s signup <username>   Create account\n", exeName)
			fmt.Printf("  %s login <username>    Login\n", exeName)
			return
		}

		if config.DeviceName == nil {
			fmt.Printf("Device name not set.\n\n")
			fmt.Printf("  %s name <your_device_name>\n", exeName)
			fmt.Printf("  Example: %s name Mac-Office\n", exeName)
			return
		}

		if config.OutputPath == nil {
			fmt.Printf("No output path set.\n\n")
			fmt.Printf("  %s output <directory_path>\n", exeName)
			fmt.Printf("  Example: %s output ~/Downloads\n", exeName)
			return
		}

		runTUI(*config.Host, *config.Port, *config.Session, username, *config.DeviceName, *config.OutputPath)
	}
}

// Global TUI channels and state
var (
	appState *App
	appMu    sync.Mutex
	renderCh = make(chan struct{}, 10)
	inputCh  = make(chan []byte, 100)
	wsConn   *websocket.Conn

	// Transfer state
	incomingFiles map[string]*os.File
	incomingTotal map[string]int
	incomingDone  map[string]int
	incomingName  map[string]string

	// Relay send flow control
	relaySendAckCh chan struct{}
)

func queueRender() {
	select {
	case renderCh <- struct{}{}:
	default:
	}
}

func runTUI(host string, port uint16, session string, username string, devName string, output string) {
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
		"device_name": devName,
		"private_ip":  privIP,
	}
	c.WriteJSON(authMsg)

	appState = newApp(output, privIP, username)
	incomingFiles = make(map[string]*os.File)
	incomingTotal = make(map[string]int)
	incomingDone = make(map[string]int)
	incomingName = make(map[string]string)

	// Enter raw mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		panic(err)
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	fmt.Print("\033[?1049h\033[?25l")       // alternate screen, hide cursor
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
				queueRender()
			}
		}
	}()

	// Initial render
	queueRender()

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
			queueRender()
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
		path := filepath.Join(appState.OutputPath, name)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err == nil {
			incomingFiles[from] = f
		}
		incomingTotal[from] = total
		incomingDone[from] = 0
		incomingName[from] = name
		appState.Mode = ModeReceiving
		appState.FileName = name
		appState.FilePercent = 0
		appState.TransferStart = time.Now()

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
		data, _ := v["data"].(string)
		if f, ok := incomingFiles[from]; ok {
			dec, _ := base64.StdEncoding.DecodeString(data)
			f.Write(dec)
			incomingDone[from]++
			pct := 0
			if incomingTotal[from] > 0 {
				pct = incomingDone[from] * 100 / incomingTotal[from]
			}
			appState.Mode = ModeReceiving
			appState.FileName = incomingName[from]
			appState.FilePercent = pct
			// Send ACK back to sender with bytes received so far
			go wsConn.WriteJSON(map[string]interface{}{
				"type": "file_ack", "to": from, "received": incomingDone[from] * CHUNK_SIZE,
			})
		}
	case "file_done":
		from, _ := v["from"].(string)
		if f, ok := incomingFiles[from]; ok {
			f.Close()
			path := filepath.Join(appState.OutputPath, incomingName[from])
			appState.Status = fmt.Sprintf("✓ Saved '%s'", path)
			appState.Mode = ModeDeviceList
			delete(incomingFiles, from)
			delete(incomingTotal, from)
			delete(incomingDone, from)
			delete(incomingName, from)
		}
	case "file_ack":
		receivedFloat, _ := v["received"].(float64)
		received := int(receivedFloat)
		if appState.Mode == ModeSending {
			appState.FileDone = received
			if received >= appState.FileTotal {
				appState.FileDone = appState.FileTotal
			}
		}
		// Unblock the relay sender goroutine
		if relaySendAckCh != nil {
			select {
			case relaySendAckCh <- struct{}{}:
			default:
			}
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

func sendFile(target DeviceEntry, filename string, path string, size int) {

	pc, err := webrtc.NewPeerConnection(webrtcConfig)
	if err != nil {
		fallbackSendViaServer(target, filename, path, size)
		return
	}

	pcMu.Lock()
	peerConnections[target.ID] = pc
	pcMu.Unlock()

	dc, err := pc.CreateDataChannel("file", nil)
	if err != nil {
		fallbackSendViaServer(target, filename, path, size)
		return
	}

	ackCh := make(chan int, 100)

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		// Receive ACK messages from the receiver
		if msg.IsString {
			var received int
			if _, err := fmt.Sscanf(string(msg.Data), "ACK:%d", &received); err == nil {
				ackCh <- received
			}
		}
	})

	dc.OnOpen(func() {
		appMu.Lock()
		route := "WAN"
		if sameLan(appState.MyPrivateIP, target.PrivateIP) {
			route = "LAN"
		}
		appState.Status = fmt.Sprintf("Direct %s: Sending %s to %s", route, filename, target.ID)
		appMu.Unlock()
		queueRender()

		file, err := os.Open(path)
		if err != nil {
			appMu.Lock()
			appState.Status = fmt.Sprintf("✗ Cannot read file: %s", err.Error())
			appState.Mode = ModeDeviceList
			appMu.Unlock()
			queueRender()
			return
		}
		defer file.Close()

		// Listen for ACKs and update sender progress
		go func() {
			for received := range ackCh {
				appMu.Lock()
				appState.FileDone = received
				if received >= size {
					appState.FileDone = size
				}
				appMu.Unlock()
				queueRender()
			}
		}()

		// Send file chunks over DataChannel with backpressure
		const lowThreshold = 256 * 1024 // 256KB
		dc.SetBufferedAmountLowThreshold(lowThreshold)
		canSend := make(chan struct{}, 1)
		dc.OnBufferedAmountLow(func() {
			select {
			case canSend <- struct{}{}:
			default:
			}
		})

		buf := make([]byte, CHUNK_SIZE)
		for {
			n, err := file.Read(buf)
			if n > 0 {
				// Wait if buffer is too full
				for dc.BufferedAmount() > MAX_BUFFER {
					<-canSend
				}
				dc.Send(buf[:n])
			}
			if err != nil {
				break
			}
		}
		dc.SendText("DONE")

		// Wait for final ACK from receiver (up to 30s)
		timeout := time.After(30 * time.Second)
		for {
			appMu.Lock()
			done := appState.FileDone >= size
			appMu.Unlock()
			if done {
				break
			}
			select {
			case <-timeout:
				break
			case <-time.After(100 * time.Millisecond):
				continue
			}
			break
		}
		close(ackCh)

		appMu.Lock()
		appState.Status = fmt.Sprintf("✓ Sent '%s' via Direct %s WebRTC", filename, route)
		appState.Mode = ModeDeviceList
		appMu.Unlock()
		queueRender()
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
		fallbackSendViaServer(target, filename, path, size)
		return
	}
	pc.SetLocalDescription(offer)

	wsConn.WriteJSON(map[string]interface{}{
		"type": "webrtc_offer", "to": target.ID, "sdp": offer.SDP,
		"filename": filename, "size": size,
	})

	// Fallback timeout
	go func() {
		time.Sleep(5 * time.Second)
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			appMu.Lock()
			appState.Status = "WebRTC Timeout, falling back to Server Relay..."
			appMu.Unlock()
			queueRender()

			pc.Close()
			pcMu.Lock()
			delete(peerConnections, target.ID)
			pcMu.Unlock()
			fallbackSendViaServer(target, filename, path, size)
		}
	}()
}

func handleIncomingWebRTC(from string, sdp string, filename string, size int) {
	pc, err := webrtc.NewPeerConnection(webrtcConfig)
	if err != nil {
		return
	}

	pcMu.Lock()
	peerConnections[from] = pc
	pcMu.Unlock()

	var file *os.File
	var receivedSize int

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			appMu.Lock()
			appState.Mode = ModeReceiving
			appState.FileName = filename
			appState.FilePercent = 0
			appState.TransferStart = time.Now()
			appState.Status = fmt.Sprintf("Receiving via Direct WebRTC from %s", from)

			path := filepath.Join(appState.OutputPath, filename)
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err == nil {
				file = f
			}
			appMu.Unlock()
			queueRender()
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if msg.IsString && string(msg.Data) == "DONE" {
				if file != nil {
					file.Close()
				}
				// Send final ACK to sender
				dc.SendText(fmt.Sprintf("ACK:%d", receivedSize))
				path := filepath.Join(appState.OutputPath, filename)
				appMu.Lock()
				appState.Status = fmt.Sprintf("✓ Saved '%s' (Direct WebRTC)", path)
				appState.Mode = ModeDeviceList
				appMu.Unlock()
				queueRender()
				return
			}
			if file != nil {
				file.Write(msg.Data)
				receivedSize += len(msg.Data)
			}

			appMu.Lock()
			pct := 0
			if size > 0 {
				pct = receivedSize * 100 / size
			}
			appState.FilePercent = pct
			appMu.Unlock()
			queueRender()

			// Send ACK back to sender every chunk
			dc.SendText(fmt.Sprintf("ACK:%d", receivedSize))
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

func fallbackSendViaServer(target DeviceEntry, filename string, path string, size int) {
	file, err := os.Open(path)
	if err != nil {
		appMu.Lock()
		appState.Status = fmt.Sprintf("✗ Cannot read file: %s", err.Error())
		appState.Mode = ModeDeviceList
		appMu.Unlock()
		queueRender()
		return
	}
	defer file.Close()

	appMu.Lock()
	route := "WAN Relay"
	if sameLan(appState.MyPrivateIP, target.PrivateIP) {
		route = "LAN Relay"
	}
	appState.Mode = ModeSending
	appState.FileName = filename
	appState.FileTotal = size
	appState.FileDone = 0
	appState.TransferStart = time.Now()
	appState.Status = fmt.Sprintf("Sending via %s to %s", route, target.ID)
	appMu.Unlock()
	queueRender()

	total := (size + CHUNK_SIZE - 1) / CHUNK_SIZE
	wsConn.WriteJSON(map[string]interface{}{
		"type": "file_offer", "to": target.ID, "filename": filename, "size": size,
	})

	// Setup flow control: allow max N chunks in-flight
	relaySendAckCh = make(chan struct{}, MAX_INFLIGHT_CHUNKS)
	inflight := 0

	buf := make([]byte, CHUNK_SIZE)
	i := 0
	for {
		n, err := file.Read(buf)
		if n > 0 {
			b64 := base64.StdEncoding.EncodeToString(buf[:n])
			wsConn.WriteJSON(map[string]interface{}{
				"type": "file_chunk", "to": target.ID, "index": i, "data": b64,
			})
			i++
			inflight++
			// If too many chunks in-flight, wait for ACK
			for inflight >= MAX_INFLIGHT_CHUNKS {
				<-relaySendAckCh
				inflight--
			}
		}
		if err != nil {
			break
		}
	}
	wsConn.WriteJSON(map[string]interface{}{
		"type": "file_done", "to": target.ID, "filename": filename, "totalChunks": total,
	})

	// Wait for receiver ACK to reach 100% (up to 5 min for large files)
	timeout := time.After(5 * time.Minute)
	for {
		appMu.Lock()
		done := appState.FileDone >= appState.FileTotal
		appMu.Unlock()
		if done {
			break
		}
		select {
		case <-timeout:
			break
		case <-time.After(200 * time.Millisecond):
			continue
		}
		break
	}

	relaySendAckCh = nil

	appMu.Lock()
	appState.Status = fmt.Sprintf("✓ Sent '%s' via %s", filename, route)
	appState.Mode = ModeDeviceList
	appMu.Unlock()
	queueRender()
}

// ── TUI ─────────────────────────────────────────────────────────────────────

func handleKey(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	c := key[0]

	// Quit on q or Ctrl+C
	if c == 'q' || c == 3 {
		return true
	}

	switch appState.Mode {
	case ModeDeviceList:
		if c == '\033' && len(key) >= 3 && key[1] == '[' {
			if key[2] == 'A' { // Up
				if appState.Selected > 0 {
					appState.Selected--
				}
			} else if key[2] == 'B' { // Down
				if appState.Selected < len(appState.targets())-1 {
					appState.Selected++
				}
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
			if len(key) == 1 {
				appState.Mode = ModeDeviceList
				appState.Status = ""
			}
			// ignore ANSI escape sequences like arrows \033[D
		} else if c == '\r' || c == '\n' {
			ts := appState.targets()
			if appState.Selected < len(ts) {
				target := ts[appState.Selected]
				path := strings.TrimSpace(appState.InputBuf)

				fileInfo, err := os.Stat(path)
				if err != nil {
					appState.Status = fmt.Sprintf("✗ File not found: %s", path)
					appState.Mode = ModeDeviceList
				} else {
					filename := filepath.Base(path)
					appState.Mode = ModeSending
					appState.FileName = filename
					appState.FileTotal = int(fileInfo.Size())
					appState.FileDone = 0
					appState.TransferStart = time.Now()
					appState.Status = fmt.Sprintf("Negotiating WebRTC Direct Connection to %s...", target.ID)
					go sendFile(target, filename, path, int(fileInfo.Size()))
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
	if myID == "" {
		myID = "connecting..."
	}
	out += fmt.Sprintf("║  Send (Go) │  You: %-25s Username: %-23s ║\r\n", myID, trunc(app.MyUsername, 23))
	out += "╠════════════════════════════════════════════════════════════════════════════════╣\r\n"
	out += "║  #  │ Device           │ Typ │ Public IP       │ Private IP      │ Joined    ║\r\n"
	out += "╠═════╪══════════════════╪═════╪═════════════════╪═════════════════╪═══════════╣\r\n"

	if len(app.Devices) == 0 {
		out += "║                            No devices connected                            ║\r\n"
	} else {
		targetIdx := 0
		for _, dev := range app.Devices {
			isYou := dev.ID == app.MyID
			joined := timeAgo(dev.JoinedAt)

			if isYou {
				label := dev.ID
				if len(label) > 10 {
					label = label[:10]
				}
				label += " (You)"
				out += fmt.Sprintf("║     │ %-16s │ %-3s │ %-15s │ %-15s │ %-9s ║\r\n", trunc(label, 16), trunc(dev.DeviceType, 3), trunc(dev.IP, 15), trunc(dev.PrivateIP, 15), trunc(joined, 9))
			} else {
				marker := " "
				if targetIdx == app.Selected {
					marker = "►"
				}
				out += fmt.Sprintf("║ %s%-2d │ %-16s │ %-3s │ %-15s │ %-15s │ %-9s ║\r\n", marker, targetIdx+1, trunc(dev.ID, 16), trunc(dev.DeviceType, 3), trunc(dev.IP, 15), trunc(dev.PrivateIP, 15), trunc(joined, 9))
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
		if len(disp) > 60 {
			disp = disp[len(disp)-60:]
		}
		out += fmt.Sprintf("║  File path: %-64s║\r\n", disp)
		out += "║  Enter: Send  │  Esc: Cancel                                                ║\r\n"
	case ModeSending:
		pct := 0
		if app.FileTotal > 0 {
			pct = app.FileDone * 100 / app.FileTotal
		}
		bar := makeBar(pct, 30)
		speed, eta := transferStats(app.FileDone, app.FileTotal, app.TransferStart)
		out += fmt.Sprintf("║  ↑ %-14s [%s] %3d%% %s %s ║\r\n", trunc(app.FileName, 14), bar, pct, speed, eta)
		sizeStr := fmt.Sprintf("%s / %s", humanSize(float64(app.FileDone)), humanSize(float64(app.FileTotal)))
		out += fmt.Sprintf("║      %-70s║\r\n", sizeStr)
	case ModeReceiving:
		bar := makeBar(app.FilePercent, 30)
		// For receiving, compute done from percent
		recvDone := 0
		if app.FileTotal > 0 {
			recvDone = app.FilePercent * app.FileTotal / 100
		}
		speed, eta := transferStats(recvDone, app.FileTotal, app.TransferStart)
		out += fmt.Sprintf("║  ↓ %-14s [%s] %3d%% %s %s ║\r\n", trunc(app.FileName, 14), bar, app.FilePercent, speed, eta)
		sizeStr := fmt.Sprintf("%s / %s", humanSize(float64(recvDone)), humanSize(float64(app.FileTotal)))
		out += fmt.Sprintf("║      %-70s║\r\n", sizeStr)
	}

	if app.Status != "" {
		out += fmt.Sprintf("║  %-76s║\r\n", trunc(app.Status, 76))
	}

	out += "╚════════════════════════════════════════════════════════════════════════════════╝\r\n"
	fmt.Print(out)
}

func trunc(s string, l int) string {
	if len(s) > l {
		return s[:l]
	}
	return s
}

func timeAgo(ts int64) string {
	diff := time.Now().Unix() - ts
	if diff < 60 {
		return "just now"
	} else if diff < 3600 {
		return fmt.Sprintf("%dm ago", diff/60)
	} else if diff < 86400 {
		return fmt.Sprintf("%dh ago", diff/3600)
	}
	return fmt.Sprintf("%dd ago", diff/86400)
}

func makeBar(pct int, width int) string {
	filled := width * pct / 100
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func humanSize(b float64) string {
	if b < 1024 {
		return fmt.Sprintf("%.0f B", b)
	} else if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", b/1024)
	} else if b < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", b/(1024*1024))
	}
	return fmt.Sprintf("%.2f GB", b/(1024*1024*1024))
}

func transferStats(done int, total int, start time.Time) (string, string) {
	elapsed := time.Since(start).Seconds()
	if elapsed < 0.5 || done <= 0 {
		return fmt.Sprintf("%8s/s", "---"), fmt.Sprintf("ETA %5s", "--:--")
	}
	speed := float64(done) / elapsed
	speedStr := fmt.Sprintf("%8s/s", humanSize(speed))
	remaining := total - done
	if remaining <= 0 || speed <= 0 {
		return speedStr, fmt.Sprintf("ETA %5s", "00:00")
	}
	etaSec := int(float64(remaining) / speed)
	if etaSec > 3600 {
		return speedStr, fmt.Sprintf("ETA %dh%02dm", etaSec/3600, (etaSec%3600)/60)
	}
	return speedStr, fmt.Sprintf("ETA %02d:%02d", etaSec/60, etaSec%60)
}
