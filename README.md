# Send

Direct file transfer between devices. Server only handles auth and message relay.

## Requirements
- [Rust](https://rustup.rs/) (1.75+)

## Quick Start

```bash
git clone <repo-url> && cd Send
```

### 1. Configure (optional)
Edit `settings.json`:
```json
{
    "host": "0.0.0.0",
    "port": 3000
}
```
`host` = IP to bind/connect. Use `0.0.0.0` to accept all, or a specific LAN IP.
`port` = port number. Default `3000`.

### 2. Start Server
```bash
cd Server
cargo run --release
```
Server creates `database/users.db` automatically. Web UI is served at `http://<host>:<port>`.

### 3. Use Web Client
Open `http://<host>:<port>` in a browser.
- Sign up → Login → See active devices
- Click a device to select it → Drop/select files to send
- Incoming files auto-download

### 4. Use CLI Client
```bash
cd Client/CLI
cargo build --release
```
The binary is at `target/release/CLI`. Copy it + `settings.json` anywhere.

**First time:**
```bash
./CLI signup myuser mypassword
./CLI login myuser mypassword
```

**Reconnect (session saved in `session.txt`):**
```bash
./CLI
```

**Interactive commands (after connected):**
```
list                    - refresh device list
send <id> <filepath>    - send file to device by ID number
quit                    - disconnect
```

**Example session:**
```
$ ./CLI login alice secret123
Logged in. Connecting...
Connected! Commands: list | send <id> <filepath> | quit

--- Devices ---
[1] Dev-A3Bc9f (You) [cli]
[2] Dev-X7Yz2w [web]
---------------

> send 2 ~/photo.jpg
Sending 'photo.jpg': 100%
✓ Sent 'photo.jpg'

> quit
Bye.
```

## How It Works
- Server stores users + sessions in SQLite, tracks active devices in memory
- All clients connect via WebSocket after auth
- Files are chunked (64KB), base64-encoded, and relayed through the server's WebSocket
- Devices join/leave updates are broadcast in real-time

## Project Structure
```
Send/
├── settings.json        # Shared config (host, port)
├── Server/              # Rust Axum server
│   └── src/main.rs
├── Web/                 # Static HTML/CSS/JS (served by Server)
│   ├── index.html
│   ├── style.css
│   └── script.js
└── Client/CLI/          # Rust CLI client
    └── src/main.rs
```
