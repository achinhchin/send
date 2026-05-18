package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

type DeviceInfo struct {
	IP         string `json:"ip"`
	PrivateIP  string `json:"private_ip"`
	DeviceType string `json:"device_type"`
	JoinedAt   int64  `json:"joined_at"`
}

type AuthReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type AuthRes struct {
	Status  string  `json:"status"`
	Session *string `json:"session,omitempty"`
}

var (
	db       *sql.DB
	upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	// users[username][device_id] = DeviceInfo
	users   = make(map[string]map[string]DeviceInfo)
	usersMu sync.Mutex

	// broadcast channel
	broadcast = make(chan struct {
		User string
		Msg  string
	}, 16384)
)

func ts() int64 { return time.Now().Unix() }

func main() {
	var err error
	err = os.MkdirAll("database", 0755)
	db, err = sql.Open("sqlite3", "./database/users.db")
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS Users (username VARCHAR(31) PRIMARY KEY, password TEXT NOT NULL)")
	if err != nil {
		log.Fatal(err)
	}
	_, err = db.Exec("CREATE TABLE IF NOT EXISTS Sessions (username VARCHAR(31), sessions TEXT, timeout INTEGER)")
	if err != nil {
		log.Fatal(err)
	}

	http.Handle("/", http.FileServer(http.Dir("../../Client/Web")))
	http.HandleFunc("/api/signup", handleSignup)
	http.HandleFunc("/api/login", handleLogin)
	http.HandleFunc("/api/logout", handleLogout)
	http.HandleFunc("/api/check-session", handleCheckSession)
	http.HandleFunc("/ws", handleWS)

	fmt.Println("Server running on 0.0.0.0:3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

func handleSignup(w http.ResponseWriter, r *http.Request) {
	var req AuthReq
	json.NewDecoder(r.Body).Decode(&req)
	if req.Username == "" || len(req.Username) > 31 || req.Password == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(AuthRes{Status: "err"})
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	_, err := db.Exec("INSERT INTO Users VALUES (?, ?)", req.Username, hash)
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(AuthRes{Status: "err"})
		return
	}
	json.NewEncoder(w).Encode(AuthRes{Status: "ok"})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req AuthReq
	json.NewDecoder(r.Body).Decode(&req)
	var hash string
	err := db.QueryRow("SELECT password FROM Users WHERE username = ?", req.Username).Scan(&hash)
	if err == nil && bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)) == nil {
		b := make([]byte, 24)
		rand.Read(b)
		session := base64.StdEncoding.EncodeToString(b)
		db.Exec("DELETE FROM Sessions WHERE timeout < ?", ts())
		db.Exec("INSERT INTO Sessions VALUES (?, ?, ?)", req.Username, session, ts()+604800)
		json.NewEncoder(w).Encode(AuthRes{Status: "ok", Session: &session})
		return
	}
	w.WriteHeader(http.StatusUnauthorized)
	json.NewEncoder(w).Encode(AuthRes{Status: "err"})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	session := strings.Replace(auth, "Bearer ", "", 1)
	db.Exec("DELETE FROM Sessions WHERE sessions = ?", session)
	json.NewEncoder(w).Encode(AuthRes{Status: "ok"})
}

func handleCheckSession(w http.ResponseWriter, r *http.Request) {
	var req map[string]interface{}
	json.NewDecoder(r.Body).Decode(&req)
	session, _ := req["session"].(string)
	var username string
	err := db.QueryRow("SELECT username FROM Sessions WHERE sessions = ? AND timeout > ?", session, ts()).Scan(&username)
	if err == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "username": username})
	} else {
		json.NewEncoder(w).Encode(map[string]string{"status": "expired"})
	}
}

func buildStateJSON(username string) string {
	usersMu.Lock()
	defer usersMu.Unlock()
	devs := users[username]
	if devs == nil {
		devs = make(map[string]DeviceInfo)
	}
	data := map[string]interface{}{"type": "state", "devices": devs}
	b, _ := json.Marshal(data)
	return string(b)
}

var (
	// user_connections[username][dev_id] = conn
	conns   = make(map[string]map[string]*websocket.Conn)
	connsMu sync.Mutex
)

func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	var username, devID, dtype, privateIP string

	// Read first message for auth
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return
	}

	var authReq map[string]interface{}
	if err := json.Unmarshal(msg, &authReq); err == nil {
		if dt, ok := authReq["device_type"].(string); ok {
			dtype = dt
		} else {
			dtype = "web"
		}
		if pip, ok := authReq["private_ip"].(string); ok {
			privateIP = pip
		}
		if sess, ok := authReq["session"].(string); ok {
			err := db.QueryRow("SELECT username FROM Sessions WHERE sessions = ? AND timeout > ?", sess, ts()).Scan(&username)
			if err == nil {
				// simple dev ID generation
				alphanum := ""
				for _, c := range sess {
					if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
						alphanum += string(c)
						if len(alphanum) == 6 {
							break
						}
					}
				}
				devID = "Dev-" + alphanum
			}
		}
	}

	if username == "" {
		conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"auth_error"}`))
		return
	}

	// Register device
	ip := r.RemoteAddr
	if colon := strings.LastIndex(ip, ":"); colon != -1 {
		ip = ip[:colon]
	}

	usersMu.Lock()
	if users[username] == nil {
		users[username] = make(map[string]DeviceInfo)
	}
	users[username][devID] = DeviceInfo{
		IP:         ip,
		PrivateIP:  privateIP,
		DeviceType: dtype,
		JoinedAt:   ts(),
	}
	usersMu.Unlock()

	connsMu.Lock()
	if conns[username] == nil {
		conns[username] = make(map[string]*websocket.Conn)
	}
	conns[username][devID] = conn
	connsMu.Unlock()

	broadcastState(username)

	// Listen loop
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}
		
		var v map[string]interface{}
		if err := json.Unmarshal(msg, &v); err == nil {
			if strings.Contains(string(msg), `"reload"`) {
				sendStateTo(conn, username, devID)
			} else if to, ok := v["to"].(string); ok {
				// Relay direct messages (SDP, ICE, file chunks)
				v["from"] = devID
				b, _ := json.Marshal(v)
				connsMu.Lock()
				if targetConn, exists := conns[username][to]; exists {
					targetConn.WriteMessage(websocket.TextMessage, b)
				}
				connsMu.Unlock()
			}
		}
	}

	// Cleanup
	usersMu.Lock()
	if _, ok := users[username]; ok {
		delete(users[username], devID)
		if len(users[username]) == 0 {
			delete(users, username)
		}
	}
	usersMu.Unlock()

	connsMu.Lock()
	if _, ok := conns[username]; ok {
		delete(conns[username], devID)
		if len(conns[username]) == 0 {
			delete(conns, username)
		}
	}
	connsMu.Unlock()

	broadcastState(username)
}

func sendStateTo(conn *websocket.Conn, username, devID string) {
	stateMsg := buildStateJSON(username)
	var v map[string]interface{}
	json.Unmarshal([]byte(stateMsg), &v)
	v["you"] = devID
	b, _ := json.Marshal(v)
	conn.WriteMessage(websocket.TextMessage, b)
}

func broadcastState(username string) {
	stateMsg := buildStateJSON(username)
	connsMu.Lock()
	defer connsMu.Unlock()
	for targetDev, conn := range conns[username] {
		var v map[string]interface{}
		json.Unmarshal([]byte(stateMsg), &v)
		v["you"] = targetDev
		b, _ := json.Marshal(v)
		conn.WriteMessage(websocket.TextMessage, b)
	}
}
