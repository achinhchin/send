use axum::{extract::{ws::{Message, WebSocket, WebSocketUpgrade}, ConnectInfo, State}, http::{HeaderMap, StatusCode}, response::IntoResponse, routing::{get, post}, Json, Router};
use base64::Engine;
use bcrypt::{hash, verify, DEFAULT_COST};
use rand::RngCore;
use rusqlite::{params, Connection};
use serde::{Deserialize, Serialize};
use std::{collections::HashMap, net::SocketAddr, sync::{Arc, Mutex}, time::{SystemTime, UNIX_EPOCH}};
use tokio::sync::broadcast;
use tower_http::{cors::CorsLayer, services::{ServeDir, ServeFile}};

#[derive(Clone, Serialize, Deserialize)]
struct DeviceInfo { ip: String, private_ip: String, device_type: String, joined_at: i64 }
#[derive(Deserialize)] struct Settings { port: Option<u16>, host: Option<String> }

struct AppState {
    db: Mutex<Connection>,
    users: Mutex<HashMap<String, HashMap<String, DeviceInfo>>>,
    tx: broadcast::Sender<(String, String)>,
}

#[derive(Deserialize)] struct AuthReq { username: String, password: Option<String> }
#[derive(Serialize)] struct AuthRes { status: String, session: Option<String> }

fn ts() -> i64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64 }

#[tokio::main]
async fn main() {
    std::fs::create_dir_all("database").unwrap();
    let db = Connection::open("database/users.db").unwrap();
    db.execute("CREATE TABLE IF NOT EXISTS Users (username VARCHAR(31) PRIMARY KEY, password TEXT NOT NULL)", []).unwrap();
    db.execute("CREATE TABLE IF NOT EXISTS Sessions (username VARCHAR(31), sessions TEXT, timeout INTEGER)", []).unwrap();

    let (tx, _) = broadcast::channel(16384);
    let state = Arc::new(AppState { db: Mutex::new(db), users: Mutex::new(HashMap::new()), tx });

    let api = Router::new()
        .route("/signup", post(signup))
        .route("/login", post(login))
        .route("/logout", post(logout))
        .route("/check-session", post(check_session));
    let app = Router::new()
        .nest_service("/", ServeDir::new("../Client/Web").fallback(ServeFile::new("../Client/Web/index.html")))
        .nest("/api", api)
        .route("/ws", get(|ws: WebSocketUpgrade, State(s), ConnectInfo(addr)| async move { ws.on_upgrade(move |socket| handle_ws(socket, s, addr)) }))
        .with_state(state).layer(CorsLayer::permissive());

    let settings = std::fs::read_to_string("../settings.json").ok()
        .and_then(|s| serde_json::from_str::<Settings>(&s).ok());
    let port = settings.as_ref().and_then(|s| s.port).unwrap_or(3000);
    let host = settings.as_ref().and_then(|s| s.host.clone()).unwrap_or_else(|| "0.0.0.0".to_string());
    let addr = format!("{}:{}", host, port);
    println!("Server running on {}", addr);
    axum::serve(tokio::net::TcpListener::bind(addr).await.unwrap(), app.into_make_service_with_connect_info::<SocketAddr>()).await.unwrap();
}

async fn signup(State(s): State<Arc<AppState>>, Json(p): Json<AuthReq>) -> impl IntoResponse {
    let pwd = p.password.unwrap_or_default();
    if p.username.is_empty() || p.username.len() > 31 || pwd.is_empty() { return (StatusCode::BAD_REQUEST, Json(AuthRes { status: "err".into(), session: None })); }
    
    match s.db.lock().unwrap().execute("INSERT INTO Users VALUES (?1, ?2)", params![p.username, hash(pwd, DEFAULT_COST).unwrap()]) {
        Ok(_) => (StatusCode::OK, Json(AuthRes { status: "ok".into(), session: None })),
        _ => (StatusCode::CONFLICT, Json(AuthRes { status: "err".into(), session: None })),
    }
}

async fn login(State(s): State<Arc<AppState>>, Json(p): Json<AuthReq>) -> impl IntoResponse {
    let db = s.db.lock().unwrap();
    let h = db.query_row("SELECT password FROM Users WHERE username = ?1", params![p.username], |r| r.get::<_, String>(0)).unwrap_or_default();
    
    if verify(p.password.unwrap_or_default(), &h).unwrap_or(false) {
        let mut b = [0u8; 24]; rand::thread_rng().fill_bytes(&mut b);
        let session = base64::engine::general_purpose::STANDARD.encode(b);
        db.execute("DELETE FROM Sessions WHERE timeout < ?1", params![ts()]).ok();
        db.execute("INSERT INTO Sessions VALUES (?1, ?2, ?3)", params![p.username, &session, ts() + 604800]).unwrap();
        (StatusCode::OK, Json(AuthRes { status: "ok".into(), session: Some(session) }))
    } else {
        (StatusCode::UNAUTHORIZED, Json(AuthRes { status: "err".into(), session: None }))
    }
}

async fn logout(State(s): State<Arc<AppState>>, headers: HeaderMap) -> impl IntoResponse {
    if let Some(t) = headers.get("Authorization") {
        s.db.lock().unwrap().execute("DELETE FROM Sessions WHERE sessions = ?1", params![t.to_str().unwrap().replace("Bearer ", "")]).ok();
    }
    Json(AuthRes { status: "ok".into(), session: None })
}

async fn check_session(State(s): State<Arc<AppState>>, Json(p): Json<serde_json::Value>) -> impl IntoResponse {
    let session = p["session"].as_str().unwrap_or("");
    match s.db.lock().unwrap().query_row(
        "SELECT username FROM Sessions WHERE sessions = ?1 AND timeout > ?2",
        params![session, ts()], |r| r.get::<_, String>(0)
    ) {
        Ok(username) => Json(serde_json::json!({"status": "ok", "username": username})),
        Err(_) => Json(serde_json::json!({"status": "expired"})),
    }
}

/// Build state JSON with device map
fn build_state_json(users: &HashMap<String, HashMap<String, DeviceInfo>>, user: &str, dev: &str) -> String {
    let empty = HashMap::new();
    let devs = users.get(user).unwrap_or(&empty);
    serde_json::to_string(&serde_json::json!({"type": "state", "you": dev, "devices": devs})).unwrap()
}

async fn handle_ws(mut ws: WebSocket, s: Arc<AppState>, addr: SocketAddr) {
    let mut user = String::new();
    let mut dev = String::new();
    let mut dtype = "web".to_string();
    let mut private_ip = String::new();
    
    if let Some(Ok(Message::Text(t))) = ws.recv().await {
        if let Ok(v) = serde_json::from_str::<serde_json::Value>(&t) {
            if let Some(dt) = v.get("device_type").and_then(|x| x.as_str()) { dtype = dt.to_string(); }
            if let Some(pip) = v.get("private_ip").and_then(|x| x.as_str()) { private_ip = pip.to_string(); }
            if let Some(sess) = v.get("session").and_then(|x| x.as_str()) {
                if let Ok(u) = s.db.lock().unwrap().query_row("SELECT username FROM Sessions WHERE sessions = ?1 AND timeout > ?2", params![sess, ts()], |r| r.get::<_, String>(0)) {
                    user = u;
                    dev = format!("Dev-{}", &sess.chars().filter(|c| c.is_alphanumeric()).take(6).collect::<String>());
                }
            }
        }
    }

    if user.is_empty() { let _ = ws.send(Message::Text(r#"{"type":"auth_error"}"#.into())).await; return; }

    s.users.lock().unwrap().entry(user.clone()).or_default().insert(dev.clone(), DeviceInfo {
        ip: addr.ip().to_string(), private_ip, device_type: dtype, joined_at: ts()
    });
    
    let state_msg = || {
        let users = s.users.lock().unwrap();
        build_state_json(&users, &user, &dev)
    };
    let mut rx = s.tx.subscribe();
    
    let _ = ws.send(Message::Text(state_msg().into())).await;
    let _ = s.tx.send((user.clone(), state_msg()));

    loop {
        tokio::select! {
            Some(Ok(Message::Text(t))) = ws.recv() => {
                if t.contains("\"reload\"") {
                    let _ = ws.send(Message::Text(state_msg().into())).await;
                } else if t.contains("\"to\"") {
                    // Relay file messages (file_offer, file_chunk, file_done) to target
                    if let Ok(mut v) = serde_json::from_str::<serde_json::Value>(&t) {
                        v["from"] = serde_json::Value::String(dev.clone());
                        let _ = s.tx.send((user.clone(), serde_json::to_string(&v).unwrap()));
                    }
                }
            }
            msg = rx.recv() => {
                match msg {
                    Ok((u, m)) => {
                        if u == user {
                            if let Ok(v) = serde_json::from_str::<serde_json::Value>(&m) {
                                let msg_type = v["type"].as_str().unwrap_or("");
                                let to = v["to"].as_str().unwrap_or("");
                                let from = v["from"].as_str().unwrap_or("");
                                if msg_type == "state" || (to == dev && from != dev) {
                                    let _ = ws.send(Message::Text(m.into())).await;
                                }
                            }
                        }
                    }
                    Err(broadcast::error::RecvError::Lagged(n)) => {
                        eprintln!("Device {} lagged, {} messages skipped", dev, n);
                    }
                    Err(_) => break,
                }
            }
            else => break,
        }
    }
    // Cleanup: remove device entry, remove user if no devices left
    {
        let mut users = s.users.lock().unwrap();
        if let Some(devs) = users.get_mut(&user) {
            devs.remove(&dev);
            if devs.is_empty() { users.remove(&user); }
        }
    }
    let _ = s.tx.send((user.clone(), state_msg()));
}
