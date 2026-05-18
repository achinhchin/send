# Send App
P2P file transfer via WebSocket relay. Rust Server (Axum), Web (HTML/JS/CSS), Rust CLI.

## Server (`/Server`, reads `../settings.json`)
- Serves `/Client/Web` at `/`, API at `/api`, WebSocket at `/ws`
- DB: SQLite `database/users.db`
  - `Users(username PK, password bcrypt)`
  - `Sessions(username, sessions base64, timeout unix_ts)`
- In-memory: `HashMap<user, HashMap<dev_id, DeviceInfo{ip,private_ip,device_type,joined_at}>>`
- WS relay: messages with `"to"` field get `"from"` injected and broadcast to target device only
- Broadcast channel buffer: 16384 (handles large file transfers)

## HTTP API
- `POST /api/signup` `{"username","password"}` → `{"status"}`
- `POST /api/login` `{"username","password"}` → `{"status","session"}`
- `POST /api/logout` `Auth: Bearer <s>` → `{"status"}`
- `POST /api/check-session` `{"session"}` → `{"status","username"}` or `{"status":"expired"}`

## WebSocket Protocol
- Auth: `{"type":"auth","session":"<s>","device_type":"web|cli","private_ip":"<ip>"}`
- State: `{"type":"state","you":"Dev-xxx","devices":{"Dev-xxx":{ip,private_ip,device_type,joined_at}}}`
- Reload: `{"type":"reload"}`
- File: `file_offer/file_chunk/file_done` with `"to"` and `"from"` fields, 64KB base64 chunks

## CLI (`/Client/CLI`, config at `send.json` next to executable)
- Config file stores: host, port, session, output_path
- Subcommands: `host <ip> <port>` | `signup <user>` | `login <user>` | `output <path>` | `help`
- No args: validates session → enters TUI mode
- TUI: real-time device list (crossterm), ↑↓ to select, Enter to send file, q to quit
- Shows LAN/WAN route info (compares /24 subnets) before file transfer
- Received files saved to configured output_path
