use futures_util::{SinkExt, StreamExt};
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::{env, fs, io::{self, Write, BufRead}, path::Path};
use tokio_tungstenite::{connect_async, tungstenite::protocol::Message};

#[derive(Serialize)] struct AuthReq { username: String, password: Option<String> }
#[derive(Deserialize)] struct AuthRes { status: String, session: Option<String> }
#[derive(Deserialize)] struct Settings { port: Option<u16>, host: Option<String> }

const CHUNK_SIZE: usize = 64 * 1024;

fn load_settings() -> (String, u16) {
    let settings = fs::read_to_string("../../settings.json").ok()
        .or_else(|| fs::read_to_string("settings.json").ok())
        .and_then(|s| serde_json::from_str::<Settings>(&s).ok());
    let host = settings.as_ref().and_then(|s| s.host.clone()).unwrap_or_else(|| "127.0.0.1".into());
    let port = settings.as_ref().and_then(|s| s.port).unwrap_or(3000);
    (host, port)
}

fn prompt(label: &str) -> String {
    print!("{}: ", label);
    io::stdout().flush().unwrap();
    let mut s = String::new();
    io::stdin().read_line(&mut s).unwrap();
    s.trim().to_string()
}

fn auth_prompt(args: &[String]) -> (String, String) {
    let user = args.get(2).map(|s| s.clone()).unwrap_or_else(|| prompt("Username"));
    let pass = rpassword::prompt_password("Password: ").unwrap();
    (user, pass)
}

#[tokio::main]
async fn main() {
    let args: Vec<String> = env::args().collect();
    let cmd = args.get(1).map(|s| s.as_str()).unwrap_or("");
    let session_file = "session.txt";
    let (host, port) = load_settings();
    let api_url = format!("http://{}:{}", host, port);

    match cmd {
        "signup" => {
            let (user, pass) = auth_prompt(&args);
            let res = Client::new().post(format!("{}/api/signup", api_url))
                .json(&AuthReq { username: user, password: Some(pass) })
                .send().await.unwrap().json::<AuthRes>().await.unwrap();
            println!("{}", if res.status == "ok" { "Signed up. Now run: login [username]" } else { "Signup failed (user may exist)" });
        }
        "login" => {
            let (user, pass) = auth_prompt(&args);
            let res = Client::new().post(format!("{}/api/login", api_url))
                .json(&AuthReq { username: user, password: Some(pass) })
                .send().await.unwrap().json::<AuthRes>().await.unwrap();
            if res.status == "ok" {
                let sess = res.session.unwrap();
                fs::write(session_file, &sess).unwrap();
                println!("Logged in. Connecting...");
                run_session(&host, port, &sess).await;
            } else { println!("Login failed."); }
        }
        _ => {
            let sess = fs::read_to_string(session_file)
                .expect("Not logged in. Run: send-cli login [username]");
            println!("Reconnecting with saved session...");
            run_session(&host, port, &sess).await;
        }
    }
}

async fn run_session(host: &str, port: u16, session: &str) {
    let (ws, _) = connect_async(format!("ws://{}:{}/ws", host, port)).await.expect("Connection failed");
    let (mut sink, mut stream) = ws.split();

    sink.send(Message::Text(serde_json::json!({"type":"auth","session":session,"device_type":"cli"}).to_string().into())).await.unwrap();

    let mut devices: serde_json::Map<String, serde_json::Value> = serde_json::Map::new();
    let mut my_id = String::new();
    let mut dev_map: Vec<(String, String)> = vec![];

    // Spawn stdin reader
    let (tx, mut rx) = tokio::sync::mpsc::channel::<String>(32);
    std::thread::spawn(move || {
        let stdin = io::stdin();
        for line in stdin.lock().lines().map_while(Result::ok) {
            if tx.blocking_send(line).is_err() { break; }
        }
    });

    // Receiving files state
    let mut incoming: std::collections::HashMap<String, (String, usize, Vec<Option<String>>)> = std::collections::HashMap::new();

    println!("Connected! Commands: list | send <id> <filepath> | quit");

    loop {
        tokio::select! {
            Some(Ok(msg)) = stream.next() => {
                if let Ok(text) = msg.into_text() {
                    if let Ok(v) = serde_json::from_str::<serde_json::Value>(&text) {
                        match v["type"].as_str().unwrap_or("") {
                            "state" => {
                                if let Some(y) = v["you"].as_str() { my_id = y.to_string(); }
                                if let Some(d) = v["devices"].as_object() {
                                    devices = d.clone();
                                    dev_map.clear();
                                    println!("\n--- Devices ---");
                                    let mut i = 1;
                                    for (name, info) in &devices {
                                        let you = if name == &my_id { " (You)" } else { "" };
                                        let dt = info["device_type"].as_str().unwrap_or("?");
                                        println!("[{}] {}{} [{}]", i, name, you, dt);
                                        dev_map.push((i.to_string(), name.clone()));
                                        i += 1;
                                    }
                                    println!("---------------");
                                }
                            }
                            "auth_error" => { println!("Session expired. Please login again."); return; }
                            "file_offer" => {
                                let from = v["from"].as_str().unwrap_or("?");
                                let name = v["filename"].as_str().unwrap_or("file");
                                let size = v["size"].as_u64().unwrap_or(0) as usize;
                                let total = (size + CHUNK_SIZE - 1) / CHUNK_SIZE;
                                println!("Receiving '{}' ({} bytes) from {}...", name, size, from);
                                incoming.insert(from.to_string(), (name.to_string(), total, vec![None; total]));
                            }
                            "file_chunk" => {
                                let from = v["from"].as_str().unwrap_or("?");
                                let idx = v["index"].as_u64().unwrap_or(0) as usize;
                                let data = v["data"].as_str().unwrap_or("");
                                if let Some(entry) = incoming.get_mut(from) {
                                    if idx < entry.2.len() { entry.2[idx] = Some(data.to_string()); }
                                    let done = entry.2.iter().filter(|x| x.is_some()).count();
                                    print!("\rReceiving '{}': {}%", entry.0, done * 100 / entry.1);
                                    io::stdout().flush().ok();
                                }
                            }
                            "file_done" => {
                                let from = v["from"].as_str().unwrap_or("?");
                                if let Some(entry) = incoming.remove(from) {
                                    use base64::Engine;
                                    let mut data = Vec::new();
                                    for chunk in &entry.2 {
                                        if let Some(b64) = chunk {
                                            data.extend(base64::engine::general_purpose::STANDARD.decode(b64).unwrap_or_default());
                                        }
                                    }
                                    let filename = format!("received_{}", entry.0);
                                    fs::write(&filename, &data).unwrap();
                                    println!("\n✓ Saved as '{}'", filename);
                                }
                            }
                            _ => {}
                        }
                    }
                }
            }
            Some(line) = rx.recv() => {
                let parts: Vec<&str> = line.trim().splitn(3, ' ').collect();
                match parts.first().map(|s| *s) {
                    Some("list") => {
                        sink.send(Message::Text(r#"{"type":"reload"}"#.into())).await.ok();
                    }
                    Some("send") if parts.len() >= 3 => {
                        let id = parts[1];
                        let filepath = parts[2];
                        let target = dev_map.iter().find(|(i, _)| i == id).map(|(_, n)| n.clone());
                        if let Some(target_dev) = target {
                            if let Ok(data) = fs::read(filepath) {
                                use base64::Engine;
                                let filename = Path::new(filepath).file_name().unwrap().to_str().unwrap();
                                let size = data.len();
                                let total = (size + CHUNK_SIZE - 1) / CHUNK_SIZE;

                                sink.send(Message::Text(serde_json::json!({"type":"file_offer","to":target_dev,"filename":filename,"size":size}).to_string().into())).await.ok();

                                for i in 0..total {
                                    let start = i * CHUNK_SIZE;
                                    let end = std::cmp::min(start + CHUNK_SIZE, size);
                                    let b64 = base64::engine::general_purpose::STANDARD.encode(&data[start..end]);
                                    sink.send(Message::Text(serde_json::json!({"type":"file_chunk","to":target_dev,"index":i,"data":b64}).to_string().into())).await.ok();
                                    print!("\rSending '{}': {}%", filename, (i + 1) * 100 / total);
                                    io::stdout().flush().ok();
                                }

                                sink.send(Message::Text(serde_json::json!({"type":"file_done","to":target_dev,"filename":filename,"totalChunks":total}).to_string().into())).await.ok();
                                println!("\n✓ Sent '{}'", filename);
                            } else {
                                println!("File not found: {}", filepath);
                            }
                        } else {
                            println!("Invalid device ID '{}'. Type 'list' to refresh.", id);
                        }
                    }
                    Some("quit") | Some("exit") => { println!("Bye."); return; }
                    _ => println!("Commands: list | send <id> <filepath> | quit"),
                }
            }
            else => break,
        }
    }
}
