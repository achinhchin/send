use crossterm::{cursor, event::{Event, EventStream, KeyCode, KeyModifiers}, execute, queue, style, terminal::{self, ClearType}};
use futures_util::{SinkExt, StreamExt};
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::{env, fs, io::{self, Write}, path::Path};
use tokio_tungstenite::{connect_async, tungstenite::protocol::Message};

const CHUNK_SIZE: usize = 64 * 1024;

// ── Config ──────────────────────────────────────────────────────────────────

#[derive(Serialize, Deserialize, Default, Clone)]
struct Config {
    host: Option<String>,
    port: Option<u16>,
    session: Option<String>,
    output_path: Option<String>,
}

fn config_path() -> std::path::PathBuf {
    env::current_exe().ok()
        .and_then(|p| p.parent().map(|d| d.join("send.json")))
        .unwrap_or_else(|| "send.json".into())
}

fn load_config() -> Config {
    fs::read_to_string(config_path()).ok()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default()
}

fn save_config(cfg: &Config) {
    fs::write(config_path(), serde_json::to_string_pretty(cfg).unwrap()).unwrap();
    eprintln!("Config saved to: {}", config_path().display());
}

fn prompt(label: &str) -> String {
    print!("{}: ", label);
    io::stdout().flush().unwrap();
    let mut s = String::new();
    io::stdin().read_line(&mut s).unwrap();
    s.trim().to_string()
}

fn get_private_ip() -> String {
    local_ip_address::local_ip().map(|ip| ip.to_string()).unwrap_or_else(|_| "unknown".into())
}

/// Check if two IPs share the same /24 subnet (simple LAN heuristic)
fn same_lan(ip1: &str, ip2: &str) -> bool {
    let p1: Vec<&str> = ip1.split('.').collect();
    let p2: Vec<&str> = ip2.split('.').collect();
    p1.len() >= 3 && p2.len() >= 3 && p1[..3] == p2[..3]
}

// ── Device from server ──────────────────────────────────────────────────────

#[derive(Clone, Debug)]
struct DeviceEntry {
    id: String,
    ip: String,
    private_ip: String,
    device_type: String,
    joined_at: i64,
}

// ── TUI state ───────────────────────────────────────────────────────────────

#[derive(Clone)]
enum Mode {
    DeviceList,
    FileInput(String),              // typing file path
    Sending(String, usize, usize),  // filename, done_chunks, total_chunks
    Receiving(String, usize),       // filename, percent
}

struct App {
    mode: Mode,
    devices: Vec<DeviceEntry>,
    selected: usize,
    my_id: String,
    my_private_ip: String,
    status: String,
    output_path: String,
}

impl App {
    fn new(output_path: String, private_ip: String) -> Self {
        Self { mode: Mode::DeviceList, devices: vec![], selected: 0, my_id: String::new(), my_private_ip: private_ip, status: String::new(), output_path }
    }

    fn targets(&self) -> Vec<&DeviceEntry> {
        self.devices.iter().filter(|d| d.id != self.my_id).collect()
    }

    fn selected_target(&self) -> Option<&DeviceEntry> {
        self.targets().get(self.selected).copied()
    }

    fn target_count(&self) -> usize {
        self.targets().len()
    }
}

// ── Rendering ───────────────────────────────────────────────────────────────

fn time_ago(ts: i64) -> String {
    let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).unwrap().as_secs() as i64;
    let diff = now - ts;
    if diff < 60 { "just now".into() }
    else if diff < 3600 { format!("{}m ago", diff / 60) }
    else if diff < 86400 { format!("{}h ago", diff / 3600) }
    else { format!("{}d ago", diff / 86400) }
}

fn render(app: &App) -> io::Result<()> {
    let mut out = io::stdout();
    queue!(out, terminal::Clear(ClearType::All), cursor::MoveTo(0, 0))?;

    // Header
    queue!(out, style::Print("╔════════════════════════════════════════════════════════════════════════════════╗\r\n"))?;
    queue!(out, style::Print(format!("║  Send  │  You: {:<65}║\r\n", if app.my_id.is_empty() { "connecting..." } else { &app.my_id })))?;
    queue!(out, style::Print("╠════════════════════════════════════════════════════════════════════════════════╣\r\n"))?;

    // Column header
    queue!(out, style::Print("║  #  │ Device          │ Type │ Public IP       │ Private IP      │ Joined    ║\r\n"))?;
    queue!(out, style::Print("╠═════╪═════════════════╪══════╪═════════════════╪═════════════════╪═══════════╣\r\n"))?;

    // Devices
    let mut target_idx = 0usize;
    if app.devices.is_empty() {
        queue!(out, style::Print("║                            No devices connected                            ║\r\n"))?;
    } else {
        for dev in &app.devices {
            let is_you = dev.id == app.my_id;
            let joined = time_ago(dev.joined_at);
            if is_you {
                let label = format!("{} (You)", dev.id);
                queue!(out, style::Print(format!(
                    "║     │ {:<15} │ {:<4} │ {:<15} │ {:<15} │ {:<9} ║\r\n",
                    truncate(&label, 15), truncate(&dev.device_type, 4),
                    truncate(&dev.ip, 15), truncate(&dev.private_ip, 15), truncate(&joined, 9)
                )))?;
            } else {
                let marker = if target_idx == app.selected { "►" } else { " " };
                queue!(out, style::Print(format!(
                    "║ {}{:<3} │ {:<15} │ {:<4} │ {:<15} │ {:<15} │ {:<9} ║\r\n",
                    marker, target_idx + 1,
                    truncate(&dev.id, 15), truncate(&dev.device_type, 4),
                    truncate(&dev.ip, 15), truncate(&dev.private_ip, 15), truncate(&joined, 9)
                )))?;
                target_idx += 1;
            }
        }
    }

    queue!(out, style::Print("╠════════════════════════════════════════════════════════════════════════════════╣\r\n"))?;

    // Mode-specific footer
    match &app.mode {
        Mode::DeviceList => {
            queue!(out, style::Print("║  ↑↓ Select  │  Enter: Send File  │  q: Quit                                  ║\r\n"))?;
        }
        Mode::FileInput(buf) => {
            let display = if buf.len() > 60 { &buf[buf.len()-60..] } else { buf.as_str() };
            queue!(out, style::Print(format!("║  File path: {:<64}║\r\n", display)))?;
            queue!(out, style::Print("║  Enter: Send  │  Esc: Cancel                                                ║\r\n"))?;
        }
        Mode::Sending(name, done, total) => {
            let pct = if *total > 0 { *done * 100 / *total } else { 0 };
            let bar_w = 40;
            let filled = bar_w * pct / 100;
            let bar: String = format!("{}{}", "█".repeat(filled), "░".repeat(bar_w - filled));
            queue!(out, style::Print(format!("║  Sending: {:<20} [{}] {:>3}%                  ║\r\n", truncate(name, 20), bar, pct)))?;
        }
        Mode::Receiving(name, pct) => {
            let bar_w = 40;
            let filled = bar_w * pct / 100;
            let bar: String = format!("{}{}", "█".repeat(filled), "░".repeat(bar_w - filled));
            queue!(out, style::Print(format!("║  Receiving: {:<18} [{}] {:>3}%                  ║\r\n", truncate(name, 18), bar, pct)))?;
        }
    }

    // Status line
    if !app.status.is_empty() {
        let s = truncate(&app.status, 76);
        queue!(out, style::Print(format!("║  {:<76}║\r\n", s)))?;
    }

    queue!(out, style::Print("╚════════════════════════════════════════════════════════════════════════════════╝\r\n"))?;
    out.flush()?;
    Ok(())
}

fn truncate(s: &str, max: usize) -> &str {
    if s.len() > max { &s[..max] } else { s }
}

// ── Main ────────────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() {
    let args: Vec<String> = env::args().collect();
    let cmd = args.get(1).map(|s| s.as_str()).unwrap_or("");
    let mut config = load_config();
    let exe_name = args.first().and_then(|p| Path::new(p).file_name()).map(|n| n.to_string_lossy().to_string()).unwrap_or("cli".into());

    match cmd {
        "host" => {
            if args.len() < 4 {
                println!("Usage: {} host <ip_or_hostname> <port>", exe_name);
                println!("Example: {} host 192.168.1.100 3000", exe_name);
                return;
            }
            config.host = Some(args[2].clone());
            config.port = Some(args[3].parse().expect("Invalid port number"));
            save_config(&config);
            println!("✓ Host set to {}:{}", args[2], args[3]);
        }
        "signup" => {
            let (host, port) = match (config.host.as_ref(), config.port) {
                (Some(h), Some(p)) => (h.clone(), p),
                _ => { println!("Set host first:\n  {} host <ip> <port>", exe_name); return; }
            };
            let user = args.get(2).cloned().unwrap_or_else(|| prompt("Username"));
            let pass = rpassword::prompt_password("Password: ").unwrap();
            let url = format!("http://{}:{}/api/signup", host, port);
            match Client::new().post(&url).json(&serde_json::json!({"username": user, "password": pass})).send().await {
                Ok(res) => {
                    let data: serde_json::Value = res.json().await.unwrap_or_default();
                    if data["status"] == "ok" {
                        println!("✓ Account created! Now run:\n  {} login {}", exe_name, user);
                    } else {
                        println!("✗ Signup failed (username may already exist)");
                    }
                }
                Err(e) => eprintln!("✗ Cannot connect to server: {}", e),
            }
        }
        "login" => {
            let (host, port) = match (config.host.as_ref(), config.port) {
                (Some(h), Some(p)) => (h.clone(), p),
                _ => { println!("Set host first:\n  {} host <ip> <port>", exe_name); return; }
            };
            let user = args.get(2).cloned().unwrap_or_else(|| prompt("Username"));
            let pass = rpassword::prompt_password("Password: ").unwrap();
            let url = format!("http://{}:{}/api/login", host, port);
            match Client::new().post(&url).json(&serde_json::json!({"username": user, "password": pass})).send().await {
                Ok(res) => {
                    let data: serde_json::Value = res.json().await.unwrap_or_default();
                    if data["status"] == "ok" {
                        config.session = data["session"].as_str().map(|s| s.to_string());
                        save_config(&config);
                        println!("✓ Logged in as {}", user);
                    } else {
                        println!("✗ Login failed. Wrong username or password.");
                    }
                }
                Err(e) => eprintln!("✗ Cannot connect to server: {}", e),
            }
        }
        "output" => {
            if args.len() < 3 {
                println!("Usage: {} output <directory_path>", exe_name);
                println!("Example: {} output ~/Downloads", exe_name);
                return;
            }
            let path = args[2].clone();
            fs::create_dir_all(&path).ok();
            config.output_path = Some(path.clone());
            save_config(&config);
            println!("✓ Output path set to: {}", path);
        }
        "help" => {
            println!("Send CLI - P2P file transfer\n");
            println!("Setup:");
            println!("  {} host <ip> <port>       Set server address", exe_name);
            println!("  {} signup <username>       Create account", exe_name);
            println!("  {} login <username>        Login and save session", exe_name);
            println!("  {} output <path>           Set received files directory", exe_name);
            println!("\nConnect:");
            println!("  {}                         Connect to device room (TUI)", exe_name);
            println!("\nTUI Controls:");
            println!("  ↑/↓   Select device");
            println!("  Enter  Send file to selected device");
            println!("  q      Quit");
        }
        _ => {
            // ── Check config completeness ───────────────────────────────
            if config.host.is_none() || config.port.is_none() {
                println!("No server configured.\n");
                println!("  {} host <ip_or_hostname> <port>", exe_name);
                println!("  Example: {} host 192.168.1.100 3000", exe_name);
                save_config(&config);
                return;
            }
            let host = config.host.as_ref().unwrap().clone();
            let port = config.port.unwrap();

            if let Some(session) = &config.session {
                let url = format!("http://{}:{}/api/check-session", host, port);
                match Client::new().post(&url).json(&serde_json::json!({"session": session})).send().await {
                    Ok(res) => {
                        let data: serde_json::Value = res.json().await.unwrap_or_default();
                        if data["status"] != "ok" {
                            config.session = None;
                            save_config(&config);
                            println!("Session expired.\n");
                            println!("  {} login <username>", exe_name);
                            return;
                        }
                    }
                    Err(e) => {
                        eprintln!("✗ Cannot reach server at {}:{} ({})", host, port, e);
                        return;
                    }
                }
            }

            if config.session.is_none() {
                println!("Not logged in.\n");
                println!("  {} signup <username>   Create account", exe_name);
                println!("  {} login <username>    Login", exe_name);
                return;
            }

            if config.output_path.is_none() {
                println!("No output path set.\n");
                println!("  {} output <directory_path>", exe_name);
                println!("  Example: {} output ~/Downloads", exe_name);
                return;
            }

            let session = config.session.unwrap();
            let output = config.output_path.unwrap();
            run_tui(&host, port, &session, &output).await;
        }
    }
}

// ── TUI session ─────────────────────────────────────────────────────────────

async fn run_tui(host: &str, port: u16, session: &str, output_path: &str) {
    let ws_url = format!("ws://{}:{}/ws", host, port);
    let (ws, _) = match connect_async(&ws_url).await {
        Ok(c) => c,
        Err(e) => { eprintln!("✗ WebSocket connection failed: {}", e); return; }
    };
    let (mut sink, mut stream) = ws.split();

    let private_ip = get_private_ip();
    let auth_msg = serde_json::json!({"type":"auth","session":session,"device_type":"cli","private_ip":private_ip});
    sink.send(Message::Text(auth_msg.to_string().into())).await.unwrap();

    terminal::enable_raw_mode().unwrap();
    execute!(io::stdout(), terminal::EnterAlternateScreen, cursor::Hide).unwrap();

    let mut app = App::new(output_path.to_string(), private_ip);
    let mut event_stream = EventStream::new();
    let mut incoming: std::collections::HashMap<String, (String, usize, Vec<Option<String>>)> = std::collections::HashMap::new();

    render(&app).ok();

    loop {
        tokio::select! {
            Some(Ok(msg)) = stream.next() => {
                if let Ok(text) = msg.into_text() {
                    if let Ok(v) = serde_json::from_str::<serde_json::Value>(&text) {
                        match v["type"].as_str().unwrap_or("") {
                            "auth_error" => {
                                app.status = "Session expired. Please login again.".into();
                                render(&app).ok();
                                tokio::time::sleep(std::time::Duration::from_secs(2)).await;
                                break;
                            }
                            "state" => {
                                if let Some(y) = v["you"].as_str() { app.my_id = y.to_string(); }
                                // Parse device map: {"Dev-xxx": {ip, private_ip, device_type, joined_at}}
                                if let Some(obj) = v["devices"].as_object() {
                                    let mut devs: Vec<DeviceEntry> = obj.iter().map(|(id, info)| {
                                        DeviceEntry {
                                            id: id.clone(),
                                            ip: info["ip"].as_str().unwrap_or("").to_string(),
                                            private_ip: info["private_ip"].as_str().unwrap_or("").to_string(),
                                            device_type: info["device_type"].as_str().unwrap_or("").to_string(),
                                            joined_at: info["joined_at"].as_i64().unwrap_or(0),
                                        }
                                    }).collect();
                                    // Sort: newest first
                                    devs.sort_by(|a, b| b.joined_at.cmp(&a.joined_at));
                                    app.devices = devs;
                                }
                                let tc = app.target_count();
                                if tc > 0 && app.selected >= tc { app.selected = tc - 1; }
                                render(&app).ok();
                            }
                            "file_offer" => {
                                let from = v["from"].as_str().unwrap_or("?");
                                let name = v["filename"].as_str().unwrap_or("file");
                                let size = v["size"].as_u64().unwrap_or(0) as usize;
                                let total = (size + CHUNK_SIZE - 1) / CHUNK_SIZE;
                                incoming.insert(from.to_string(), (name.to_string(), total, vec![None; total]));
                                app.mode = Mode::Receiving(name.to_string(), 0);
                                app.status = format!("Incoming: {} ({} bytes) from {}", name, size, from);
                                render(&app).ok();
                            }
                            "file_chunk" => {
                                let from = v["from"].as_str().unwrap_or("?");
                                let idx = v["index"].as_u64().unwrap_or(0) as usize;
                                let data = v["data"].as_str().unwrap_or("");
                                if let Some(entry) = incoming.get_mut(from) {
                                    if idx < entry.2.len() { entry.2[idx] = Some(data.to_string()); }
                                    let done = entry.2.iter().filter(|x| x.is_some()).count();
                                    let pct = done * 100 / entry.1.max(1);
                                    app.mode = Mode::Receiving(entry.0.clone(), pct);
                                    render(&app).ok();
                                }
                            }
                            "file_done" => {
                                let from = v["from"].as_str().unwrap_or("?");
                                if let Some(entry) = incoming.remove(from) {
                                    use base64::Engine;
                                    let missing = entry.2.iter().filter(|x| x.is_none()).count();
                                    let mut data = Vec::new();
                                    for chunk in &entry.2 {
                                        if let Some(b64) = chunk {
                                            data.extend(base64::engine::general_purpose::STANDARD.decode(b64).unwrap_or_default());
                                        }
                                    }
                                    let out_dir = Path::new(&app.output_path);
                                    fs::create_dir_all(out_dir).ok();
                                    let filepath = out_dir.join(&entry.0);
                                    fs::write(&filepath, &data).unwrap();
                                    if missing > 0 {
                                        app.status = format!("⚠ Saved '{}' ({} chunks missing)", filepath.display(), missing);
                                    } else {
                                        app.status = format!("✓ Saved '{}'", filepath.display());
                                    }
                                    app.mode = Mode::DeviceList;
                                    render(&app).ok();
                                }
                            }
                            _ => {}
                        }
                    }
                }
            }
            Some(Ok(event)) = event_stream.next() => {
                if let Event::Key(key) = event {
                    match &app.mode {
                        Mode::DeviceList => {
                            match key.code {
                                KeyCode::Char('q') => break,
                                KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => break,
                                KeyCode::Up => {
                                    if app.selected > 0 { app.selected -= 1; }
                                    render(&app).ok();
                                }
                                KeyCode::Down => {
                                    if app.target_count() > 0 && app.selected < app.target_count() - 1 {
                                        app.selected += 1;
                                    }
                                    render(&app).ok();
                                }
                                KeyCode::Enter => {
                                    if let Some(target) = app.selected_target() {
                                        // Show route info
                                        let route = if same_lan(&app.my_private_ip, &target.private_ip) {
                                            format!("Route: LAN ({} ↔ {})", app.my_private_ip, target.private_ip)
                                        } else {
                                            format!("Route: WAN ({} ↔ {})", app.my_private_ip, target.ip)
                                        };
                                        app.status = format!("→ {}  │  {}", target.id, route);
                                        app.mode = Mode::FileInput(String::new());
                                        render(&app).ok();
                                    }
                                }
                                _ => {}
                            }
                        }
                        Mode::FileInput(buf) => {
                            let mut buf = buf.clone();
                            match key.code {
                                KeyCode::Esc => {
                                    app.mode = Mode::DeviceList;
                                    app.status.clear();
                                    render(&app).ok();
                                }
                                KeyCode::Char(c) => {
                                    buf.push(c);
                                    app.mode = Mode::FileInput(buf);
                                    render(&app).ok();
                                }
                                KeyCode::Backspace => {
                                    buf.pop();
                                    app.mode = Mode::FileInput(buf);
                                    render(&app).ok();
                                }
                                KeyCode::Enter => {
                                    let filepath = buf.trim().to_string();
                                    if let Some(target) = app.selected_target().cloned() {
                                        if let Ok(data) = fs::read(&filepath) {
                                            use base64::Engine;
                                            let filename = Path::new(&filepath).file_name().unwrap().to_str().unwrap().to_string();
                                            let size = data.len();
                                            let total = (size + CHUNK_SIZE - 1) / CHUNK_SIZE;

                                            // Show route before sending
                                            let route = if same_lan(&app.my_private_ip, &target.private_ip) {
                                                format!("LAN ({} ↔ {})", app.my_private_ip, target.private_ip)
                                            } else {
                                                format!("WAN ({} ↔ {})", app.my_private_ip, target.ip)
                                            };
                                            app.status = format!("Sending via {} to {}", route, target.id);
                                            app.mode = Mode::Sending(filename.clone(), 0, total);
                                            render(&app).ok();

                                            // Send offer
                                            sink.send(Message::Text(serde_json::json!({"type":"file_offer","to":target.id,"filename":filename,"size":size}).to_string().into())).await.ok();

                                            // Send chunks
                                            for i in 0..total {
                                                let start = i * CHUNK_SIZE;
                                                let end = std::cmp::min(start + CHUNK_SIZE, size);
                                                let b64 = base64::engine::general_purpose::STANDARD.encode(&data[start..end]);
                                                sink.send(Message::Text(serde_json::json!({"type":"file_chunk","to":target.id,"index":i,"data":b64}).to_string().into())).await.ok();
                                                app.mode = Mode::Sending(filename.clone(), i + 1, total);
                                                render(&app).ok();
                                            }

                                            // Send done
                                            sink.send(Message::Text(serde_json::json!({"type":"file_done","to":target.id,"filename":filename,"totalChunks":total}).to_string().into())).await.ok();
                                            app.status = format!("✓ Sent '{}' via {}", filename, route);
                                            app.mode = Mode::DeviceList;
                                            render(&app).ok();
                                        } else {
                                            app.status = format!("✗ File not found: {}", filepath);
                                            app.mode = Mode::DeviceList;
                                            render(&app).ok();
                                        }
                                    }
                                }
                                _ => {}
                            }
                        }
                        Mode::Sending(_, _, _) | Mode::Receiving(_, _) => {
                            // No input during transfer
                        }
                    }
                }
            }
            else => break,
        }
    }

    // Cleanup TUI
    terminal::disable_raw_mode().unwrap();
    execute!(io::stdout(), terminal::LeaveAlternateScreen, cursor::Show).unwrap();
    println!("Bye.");
}
