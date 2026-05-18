# Send App (Go + WebRTC Migration)
P2P file transfer via direct WebRTC (LAN/WAN) with WebSocket fallback relay. 
The backend and CLI are written in Go. Web interface exists (HTML/JS/CSS).

## Server (`/Server`, Go)
- **Framework**: `net/http` and `gorilla/websocket`
- **DB**: SQLite `database/users.db`
  - `Users(username PK, password bcrypt)`
  - `Sessions(username, sessions base64, timeout unix_ts)`
- **In-memory state**: `map[string]map[string]DeviceInfo` (users -> user -> devID -> info)
- **Role**: 
  1. API server for auth.
  2. Signaling server for WebRTC (`webrtc_offer`, `webrtc_answer`, `webrtc_ice`).
  3. Fallback Relay server for data chunks (`file_offer`, `file_chunk`, `file_done`).

## HTTP API
- `POST /api/signup` `{"username","password"}` → `{"status"}`
- `POST /api/login` `{"username","password"}` → `{"status","session"}`
- `POST /api/logout` `Auth: Bearer <s>` → `{"status"}`
- `POST /api/check-session` `{"session"}` → `{"status","username"}` or `{"status":"expired"}`

## WebSocket Protocol
- **Auth**: `{"type":"auth","session":"<s>","device_type":"web|cli","private_ip":"<ip>"}`
- **State**: `{"type":"state","you":"Dev-xxx","devices":{"Dev-xxx":{ip,private_ip,device_type,joined_at}}}`
- **Signaling**: `webrtc_offer`, `webrtc_answer`, `webrtc_ice` with `sdp` or `candidate` fields.
- **Relay (Fallback)**: `file_offer/file_chunk/file_done` with `"to"` and `"from"` fields, 64KB base64 chunks.

## CLI (`/Client/CLI`, Go)
- **Config file**: `send.json` (saved in current working directory `.` to avoid Go's temporary `/var/folders/` issue with `go run`).
- **Dependencies**: `pion/webrtc` for true P2P, `golang.org/x/term` for TUI.
- **Subcommands**: `host <ip> <port>` | `signup <user>` | `login <user>` | `output <path>` | `help`
- **No args**: Checks config completeness (shows setup hints if missing) → validates session → enters TUI mode.
- **TUI**: Real-time device list using ANSI sequences. 
- **Networking Priority Logic**:
  1. Tries to negotiate a direct `DataChannel` via **WebRTC** (Hole punching using Google STUN).
  2. Checks `same_lan()` heuristic (comparing /24 subnets).
  3. If connection times out after 5 seconds, falls back to slicing the file into base64 chunks and relaying through the central **WebSocket Server**.
- **Display**: Shows exact IPs and dynamic route indicators (`Priority: Direct LAN WebRTC` or `WAN Relay`).
