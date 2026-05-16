# Send App
P2P file transfer via WebSocket relay. Rust Server (Axum), Web (HTML/JS/CSS), Rust CLI.

## Server (`/Server`, reads `../settings.json`)
- Serves `/Web` at `/`, API at `/api`, WebSocket at `/ws`
- DB: SQLite `database/users.db`
  - `Users(username PK, password bcrypt)`
  - `Sessions(username, sessions base64, timeout unix_ts)`
- In-memory: `HashMap<user, HashMap<dev_id, DeviceInfo{ip,device_type,joined_at}>>`
- WS relay: messages with `"to"` field get `"from"` injected and broadcast to target device only

## HTTP API
- `POST /api/signup` `{"username","password"}` → `{"status"}`
- `POST /api/login` `{"username","password"}` → `{"status","session"}`
- `POST /api/logout` `Auth: Bearer <s>` → `{"status"}`

## WebSocket Protocol
- Auth: `{"type":"auth","session":"<s>","device_type":"web|cli"}`
- State: `{"type":"state","you":"Dev-xxx","devices":{...}}`
- Reload: `{"type":"reload"}`
- File: `file_offer/file_chunk/file_done` with `"to"` and `"from"` fields, 64KB base64 chunks

## CLI (`/Client/CLI`, reads `../../settings.json` or `./settings.json`)
- `signup <u> <p>` | `login <u> <p>` | (no args = reconnect)
- Interactive: `list` | `send <id> <file>` | `quit`
- Session in `session.txt`, received files saved as `received_<filename>`
